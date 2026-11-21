package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/victorcondackdev/fraud-rules-go/expr"
)

// Errors returned by the engine.
var (
	// ErrNoRuleset is returned by [Engine.Evaluate] before any ruleset has
	// been loaded.
	ErrNoRuleset = errors.New("engine: no ruleset loaded")
	// ErrNoPrevious is returned by [Engine.Rollback] when there is no older
	// revision to return to.
	ErrNoPrevious = errors.New("engine: no previous ruleset revision")
	// ErrDeadline is reported in [RuleError] for rules skipped because the
	// latency budget ran out.
	ErrDeadline = errors.New("engine: latency budget exceeded")
)

// Options configures an [Engine].
type Options struct {
	// Budget caps the time spent evaluating one transaction, on top of any
	// context deadline. Rules not reached in time are resolved by their fail
	// mode. Zero means no budget.
	Budget time.Duration
	// History is how many revisions are kept for rollback, including the
	// active one. Default 10.
	History int
	// Logger receives ruleset lifecycle events. Default: slog.Default().
	Logger *slog.Logger
	// Clock is used for load timestamps and the latency budget. Default
	// time.Now.
	Clock func() time.Time
	// NoExplain skips collecting feature values for matched rules. Rule
	// IDs, scores and reasons are still reported. Explanations account for
	// roughly a third of evaluation time when many rules match; keep them
	// on unless profiling says otherwise.
	NoExplain bool
}

// Engine evaluates transactions against the active ruleset. Rulesets are
// swapped atomically: an evaluation in flight finishes on the revision it
// started with. All methods are safe for concurrent use.
type Engine struct {
	store FeatureStore
	opts  Options
	cur   atomic.Pointer[Compiled]

	mu      sync.Mutex
	history []*Compiled // oldest first; last is active
	nextRev int64

	envs sync.Pool
}

// New returns an engine reading velocity features from store. A nil store
// makes every aggregate zero, which is handy for unit-testing rules.
func New(store FeatureStore, opts Options) *Engine {
	if opts.History <= 0 {
		opts.History = 10
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	e := &Engine{store: store, opts: opts}
	e.envs.New = func() any { return &evalState{} }
	return e
}

// CompileOptions returns the options [Compile] should use for this engine's
// feature store.
func (e *Engine) CompileOptions() CompileOptions {
	if e.store == nil {
		return CompileOptions{}
	}
	return CompileOptions{MaxWindow: e.store.MaxWindow()}
}

// LoadJSON parses, validates, compiles and activates a ruleset.
func (e *Engine) LoadJSON(data []byte) (*Compiled, error) {
	rs, err := ParseRuleset(data)
	if err != nil {
		return nil, err
	}
	c, err := Compile(rs, e.CompileOptions())
	if err != nil {
		return nil, err
	}
	return e.Load(c), nil
}

// Load activates c, assigning it the next revision number, and returns it.
// The previous revision stays in the history for [Engine.Rollback].
func (e *Engine) Load(c *Compiled) *Compiled {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nextRev++
	cc := *c // the caller's value stays unassigned and reusable
	cc.Revision = e.nextRev
	cc.LoadedAt = e.opts.Clock()
	e.history = append(e.history, &cc)
	if len(e.history) > e.opts.History {
		e.history = append([]*Compiled(nil), e.history[len(e.history)-e.opts.History:]...)
	}
	e.cur.Store(&cc)
	e.opts.Logger.Info("ruleset loaded", "ruleset", cc.Label(), "revision", cc.Revision,
		"digest", cc.Digest, "rules", len(cc.rules))
	return &cc
}

// Rollback reactivates the revision loaded before the active one and drops
// the active one from the history.
func (e *Engine) Rollback() (*Compiled, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.history) < 2 {
		return nil, ErrNoPrevious
	}
	bad := e.history[len(e.history)-1]
	e.history = e.history[:len(e.history)-1]
	prev := e.history[len(e.history)-1]
	e.cur.Store(prev)
	e.opts.Logger.Warn("ruleset rolled back", "from_revision", bad.Revision,
		"to", prev.Label(), "revision", prev.Revision)
	return prev, nil
}

// Active returns the ruleset currently serving, or nil.
func (e *Engine) Active() *Compiled { return e.cur.Load() }

// History returns the kept revisions, oldest first; the last is active.
func (e *Engine) History() []*Compiled {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*Compiled(nil), e.history...)
}

// Record adds a settled transaction to the feature store. Call it when
// the payment actually settles, not when it is evaluated, so blocked
// attempts do not inflate the payer's velocity.
func (e *Engine) Record(tx *Transaction) {
	if e.store != nil {
		record(e.store, tx)
	}
}

// Feature is one input of a matched rule with its value, the core of the
// explanation attached to every decision.
type Feature struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	// Display is a human rendering when it differs from Value, such as
	// "R$ 1.500,00" for an amount in cents.
	Display string `json:"display,omitempty"`
}

// Hit is a rule that matched (or failed closed).
type Hit struct {
	RuleID      string    `json:"rule_id"`
	Description string    `json:"description,omitempty"`
	Action      Action    `json:"action"`
	Score       int       `json:"score"`
	Mode        Mode      `json:"mode"`
	HoldFor     Duration  `json:"hold_for,omitempty"`
	Features    []Feature `json:"features"`
	// FailedClosed is set when the rule could not be evaluated and its
	// fail mode counted it as matched.
	FailedClosed bool `json:"failed_closed,omitempty"`
}

// RuleError reports a rule that could not be evaluated.
type RuleError struct {
	RuleID   string   `json:"rule_id"`
	Error    string   `json:"error"`
	FailMode FailMode `json:"fail_mode"`
}

// Outcome is an aggregated action and score.
type Outcome struct {
	Action  Action   `json:"action"`
	Score   int      `json:"score"`
	HoldFor Duration `json:"hold_for,omitempty"`
}

// Decision is the result of evaluating one transaction.
type Decision struct {
	TransactionID string `json:"transaction_id"`
	Outcome
	// Reasons explain the action in one line each: which rule or
	// threshold produced it.
	Reasons []string `json:"reasons"`
	// Hits are the enforced rules that matched.
	Hits []Hit `json:"hits"`
	// ShadowHits are shadow rules that matched; Shadow is the outcome the
	// decision would have had with them enforced.
	ShadowHits []Hit       `json:"shadow_hits,omitempty"`
	Shadow     *Outcome    `json:"shadow,omitempty"`
	Errors     []RuleError `json:"errors,omitempty"`
	// Degraded is true when any rule was skipped or failed.
	Degraded  bool   `json:"degraded,omitempty"`
	Ruleset   string `json:"ruleset"`
	Revision  int64  `json:"revision"`
	ElapsedNs int64  `json:"elapsed_ns"`
}

// Trace is the per-rule result of [Engine.Trace], matched or not.
type Trace struct {
	RuleID   string    `json:"rule_id"`
	Mode     Mode      `json:"mode"`
	Enabled  bool      `json:"enabled"`
	Matched  bool      `json:"matched"`
	Error    string    `json:"error,omitempty"`
	Features []Feature `json:"features"`
}

type evalState struct {
	env   txEnv
	frame expr.Frame
}

// Evaluate runs every enabled rule of the active ruleset against tx and
// aggregates the result:
//
//  1. the action is the most severe among matched enforced rules
//     (BLOCK > HOLD > STEP_UP > REVIEW > ALLOW);
//  2. scores of matched enforced rules are summed and compared with the
//     thresholds, which may escalate the action further;
//  3. HOLD carries the longest hold_for among matched HOLD rules.
//
// Shadow rules are evaluated identically but only reported. Evaluation
// stops early when ctx is done or the budget runs out; remaining rules are
// resolved by their fail mode and the decision is marked Degraded.
func (e *Engine) Evaluate(ctx context.Context, tx *Transaction) (Decision, error) {
	c := e.cur.Load()
	if c == nil {
		return Decision{TransactionID: tx.ID, Reasons: []string{"no ruleset loaded"}}, ErrNoRuleset
	}
	return e.evaluate(ctx, c, tx, nil), nil
}

// EvaluateWith is like [Engine.Evaluate] with an explicit compiled ruleset
// instead of the active one, e.g. to compare a candidate before loading it.
func (e *Engine) EvaluateWith(ctx context.Context, c *Compiled, tx *Transaction) Decision {
	return e.evaluate(ctx, c, tx, nil)
}

// Trace evaluates like Evaluate and also returns, for every rule, whether
// it matched and the value of each feature it reads. It is meant for
// debugging rules (riskctl explain), not for the hot path.
func (e *Engine) Trace(ctx context.Context, tx *Transaction) (Decision, []Trace, error) {
	c := e.cur.Load()
	if c == nil {
		return Decision{}, nil, ErrNoRuleset
	}
	var tr []Trace
	d := e.evaluate(ctx, c, tx, &tr)
	return d, tr, nil
}

func (e *Engine) evaluate(ctx context.Context, c *Compiled, tx *Transaction, trace *[]Trace) Decision {
	start := e.opts.Clock()
	var deadline time.Time
	if e.opts.Budget > 0 {
		deadline = start.Add(e.opts.Budget)
	}
	if d, ok := ctx.Deadline(); ok && (deadline.IsZero() || d.Before(deadline)) {
		deadline = d
	}
	done := ctx.Done()

	st := e.envs.Get().(*evalState)
	st.env.reset(tx, e.store)
	st.frame.Reset(&st.env)

	d := Decision{TransactionID: tx.ID, Ruleset: c.Label(), Revision: c.Revision}
	var enf, sh Outcome
	expired := false
	for i := range c.rules {
		r := &c.rules[i]
		enabled := r.IsEnabled()
		if !enabled {
			if trace != nil {
				*trace = append(*trace, Trace{RuleID: r.ID, Mode: r.Mode})
			}
			continue
		}
		// Checking the clock costs tens of nanoseconds, so do it only before
		// rules that may hit the feature store, and every 8th rule.
		if !expired && (r.hasAggs || i%8 == 0) {
			if done != nil {
				select {
				case <-done:
					expired = true
				default:
				}
			}
			if !expired && !deadline.IsZero() && !e.opts.Clock().Before(deadline) {
				expired = true
			}
		}
		var matched bool
		var err error
		if expired {
			err = ErrDeadline
			if ctxErr := ctx.Err(); ctxErr != nil {
				err = fmt.Errorf("%w: %v", ErrDeadline, ctxErr)
			}
		} else {
			matched, err = r.prog.Run(&st.frame)
		}
		failedClosed := false
		if err != nil {
			d.Degraded = true
			d.Errors = append(d.Errors, RuleError{RuleID: r.ID, Error: err.Error(), FailMode: r.failMode})
			matched = r.failMode == FailClosed
			failedClosed = matched
		}
		var feats []Feature
		if (matched && !e.opts.NoExplain) || trace != nil {
			if !expired {
				feats = explain(r.prog, &st.env)
			}
		}
		if trace != nil {
			t := Trace{RuleID: r.ID, Mode: r.Mode, Enabled: true, Matched: matched, Features: feats}
			if err != nil {
				t.Error = err.Error()
			}
			*trace = append(*trace, t)
		}
		if !matched {
			continue
		}
		h := Hit{RuleID: r.ID, Description: r.Description, Action: r.Action, Score: r.Score,
			Mode: r.Mode, HoldFor: r.HoldFor, Features: feats, FailedClosed: failedClosed}
		sh.add(h)
		if r.Mode == Shadow {
			d.ShadowHits = append(d.ShadowHits, h)
			continue
		}
		enf.add(h)
		d.Hits = append(d.Hits, h)
	}
	e.envs.Put(st)

	d.Outcome, d.Reasons = finish(enf, c.Ruleset.Thresholds, d.Hits)
	if len(d.ShadowHits) > 0 {
		o, _ := finish(sh, c.Ruleset.Thresholds, nil)
		d.Shadow = &o
	}
	d.ElapsedNs = int64(e.opts.Clock().Sub(start))
	return d
}

func (o *Outcome) add(h Hit) {
	o.Score += h.Score
	if h.Action > o.Action {
		o.Action = h.Action
	}
	if h.Action == Hold && h.HoldFor > o.HoldFor {
		o.HoldFor = h.HoldFor
	}
}

// finish applies the score thresholds and writes the reasons.
func finish(o Outcome, th Thresholds, hits []Hit) (Outcome, []string) {
	var reasons []string
	for _, h := range hits {
		if h.Action == o.Action && h.Action != Allow {
			reasons = append(reasons, fmt.Sprintf("rule %s → %s", h.RuleID, h.Action))
		}
	}
	if ta, limit := th.action(o.Score); ta > o.Action {
		o.Action = ta
		reasons = append(reasons[:0], fmt.Sprintf("score %d ≥ %s threshold %d", o.Score, strings.ToLower(ta.String()), limit))
	}
	if o.Action != Hold {
		o.HoldFor = 0
	} else if o.HoldFor == 0 {
		o.HoldFor = Duration(30 * time.Minute) // HOLD reached via threshold has no rule duration
	}
	if len(reasons) == 0 {
		reasons = []string{"no enforced rule matched"}
		if o.Score != 0 {
			reasons = []string{fmt.Sprintf("score %d below thresholds", o.Score)}
		}
	}
	return o, reasons
}

func explain(p *expr.Program, env expr.Env) []Feature {
	vals := p.Explain(env)
	out := make([]Feature, len(vals))
	for i, v := range vals {
		out[i] = Feature{Name: v.Name, Value: v.Value.String(), Display: display(v)}
	}
	return out
}

var saoPaulo = func() *time.Location {
	l, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		return time.UTC
	}
	return l
}()

// display renders money in reais and times in Brasília time.
func display(v expr.FeatureValue) string {
	switch v.Value.Type {
	case expr.Int:
		if moneyFields[v.Name] || strings.HasPrefix(v.Name, "sum(") {
			return FormatBRL(v.Value.Int)
		}
	case expr.Time:
		if v.Value.Int == 0 {
			return "unset"
		}
		return time.Unix(0, v.Value.Int).In(saoPaulo).Format("2006-01-02 15:04:05 -07:00")
	}
	return ""
}

// FormatBRL formats cents as Brazilian reais: 150000 → "R$ 1.500,00".
func FormatBRL(cents int64) string {
	neg := cents < 0
	u := uint64(cents)
	if neg {
		u = -u
	}
	whole := strconv.FormatUint(u/100, 10)
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString("R$ ")
	for i, ch := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(ch)
	}
	frac := u % 100
	b.WriteByte(',')
	b.WriteByte(byte('0' + frac/10))
	b.WriteByte(byte('0' + frac%10))
	return b.String()
}
