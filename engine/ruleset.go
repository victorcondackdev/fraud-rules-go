package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/victorcondackdev/fraud-rules-go/expr"
)

// Ruleset is the JSON document operators edit: a named, versioned list of
// rules plus the score thresholds that turn accumulated weight into an
// action.
type Ruleset struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	// TimeZone is the default zone for hour, weekday and isNight.
	// Empty means America/Sao_Paulo.
	TimeZone   string     `json:"time_zone,omitempty"`
	Thresholds Thresholds `json:"thresholds,omitempty"`
	// FailModes sets the default fail mode per action, e.g. BLOCK rules
	// fail closed while REVIEW rules fail open. Rules may override it.
	FailModes map[Action]FailMode `json:"fail_modes,omitempty"`
	Rules     []Rule              `json:"rules"`
}

// Thresholds escalate the decision when the summed score of matched
// enforced rules reaches them. Zero disables a threshold.
type Thresholds struct {
	Review int `json:"review,omitempty"`
	StepUp int `json:"step_up,omitempty"`
	Block  int `json:"block,omitempty"`
}

func (t Thresholds) action(score int) (Action, int) {
	switch {
	case t.Block > 0 && score >= t.Block:
		return Block, t.Block
	case t.StepUp > 0 && score >= t.StepUp:
		return StepUp, t.StepUp
	case t.Review > 0 && score >= t.Review:
		return Review, t.Review
	}
	return Allow, 0
}

// Rule is one condition and its consequence.
type Rule struct {
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
	// Expr is a bool expression in the rule language; see package expr.
	Expr   string `json:"expr"`
	Action Action `json:"action"`
	// Score is added to the decision score when the rule matches. It may
	// be negative to offset other rules (e.g. a long-trusted beneficiary).
	Score   int      `json:"score,omitempty"`
	Enabled *bool    `json:"enabled,omitempty"` // default true
	Mode    Mode     `json:"mode,omitempty"`
	HoldFor Duration `json:"hold_for,omitempty"` // required for HOLD
	OnError FailMode `json:"on_error,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

// IsEnabled reports whether the rule runs at all.
func (r *Rule) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// ParseRuleset decodes a ruleset, rejecting unknown fields so that typos
// such as "acton" fail loudly instead of silently defaulting.
func ParseRuleset(data []byte) (Ruleset, error) {
	var rs Ruleset
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rs); err != nil {
		return Ruleset{}, fmt.Errorf("engine: decoding ruleset: %w", err)
	}
	if dec.More() {
		return Ruleset{}, errors.New("engine: decoding ruleset: trailing data after JSON object")
	}
	return rs, nil
}

// Problem is one validation failure.
type Problem struct {
	RuleID string // empty for ruleset-level problems
	Index  int    // rule index, -1 for ruleset-level problems
	Field  string // JSON field at fault, e.g. "expr" or "hold_for"
	Err    error  // *expr.Error for expression problems
}

// Error returns "rule <id> (#i) expr: 1:5: message".
func (p Problem) Error() string {
	if p.Index < 0 {
		return fmt.Sprintf("ruleset %s: %v", p.Field, p.Err)
	}
	return fmt.Sprintf("rule %q (#%d) %s: %v", p.RuleID, p.Index, p.Field, p.Err)
}

// Snippet is like Error but, for expression problems, adds the source line
// with a caret under the offending span.
func (p Problem) Snippet() string {
	var ee *expr.Error
	if errors.As(p.Err, &ee) {
		return fmt.Sprintf("rule %q (#%d) %s: %s", p.RuleID, p.Index, p.Field, ee.Snippet())
	}
	return p.Error()
}

// ValidationError lists every problem found in a ruleset, so an operator
// fixes them in one pass instead of one per deploy.
type ValidationError struct{ Problems []Problem }

// ErrInvalidRuleset is matched by every [*ValidationError].
var ErrInvalidRuleset = errors.New("engine: invalid ruleset")

func (v *ValidationError) Error() string {
	msgs := make([]string, len(v.Problems))
	for i, p := range v.Problems {
		msgs[i] = p.Error()
	}
	return fmt.Sprintf("engine: invalid ruleset: %d problem(s): %s", len(v.Problems), strings.Join(msgs, "; "))
}

// Is makes errors.Is(err, ErrInvalidRuleset) true.
func (v *ValidationError) Is(target error) bool { return target == ErrInvalidRuleset }

// Compiled is a validated ruleset with every expression compiled. It is
// immutable and shared by concurrent evaluations.
type Compiled struct {
	Ruleset Ruleset
	// Revision is assigned by [Engine.Load]: 1 for the first load, +1 for
	// every load after. Zero until loaded.
	Revision int64
	LoadedAt time.Time
	// Digest is a short SHA-256 of the canonical JSON, to tell apart two
	// uploads that reuse a version string.
	Digest string
	rules  []compiledRule
}

type compiledRule struct {
	*Rule
	prog     *expr.Program
	failMode FailMode
	hasAggs  bool
}

// CompileOptions tunes [Compile].
type CompileOptions struct {
	// MaxWindow rejects aggregate windows the feature store cannot answer.
	// Zero disables the check.
	MaxWindow time.Duration
	// MaxCost is the per-expression cost budget; zero means
	// [expr.DefaultMaxCost].
	MaxCost int
}

// Compile validates rs and compiles every enabled and disabled rule (so a
// broken disabled rule cannot be switched on later). It returns a
// [*ValidationError] listing all problems.
func Compile(rs Ruleset, opts CompileOptions) (*Compiled, error) {
	var probs []Problem
	top := func(field string, format string, args ...any) {
		probs = append(probs, Problem{Index: -1, Field: field, Err: fmt.Errorf(format, args...)})
	}
	if strings.TrimSpace(rs.Name) == "" {
		top("name", "must not be empty")
	}
	if strings.TrimSpace(rs.Version) == "" {
		top("version", "must not be empty")
	}
	if len(rs.Rules) == 0 {
		top("rules", "must contain at least one rule")
	}
	th := rs.Thresholds
	if th.Review < 0 || th.StepUp < 0 || th.Block < 0 {
		top("thresholds", "must not be negative")
	}
	if (th.Review > 0 && th.StepUp > 0 && th.Review >= th.StepUp) ||
		(th.StepUp > 0 && th.Block > 0 && th.StepUp >= th.Block) ||
		(th.Review > 0 && th.Block > 0 && th.Review >= th.Block) {
		top("thresholds", "must increase: review < step_up < block")
	}
	for a, f := range rs.FailModes {
		if int(a) >= len(actionNames) || f == FailUnset {
			top("fail_modes", "invalid entry %v: %v", a, f)
		}
	}
	eo := expr.Options{MaxCost: opts.MaxCost, DefaultTimeZone: rs.TimeZone, MaxWindow: opts.MaxWindow}
	if rs.TimeZone != "" {
		if _, err := time.LoadLocation(rs.TimeZone); err != nil {
			top("time_zone", "unknown time zone %q", rs.TimeZone)
			eo.DefaultTimeZone = ""
		}
	}

	c := &Compiled{Ruleset: rs}
	// Rules point into a private copy so later edits to rs cannot change
	// a ruleset that is already serving traffic.
	c.Ruleset.Rules = append([]Rule(nil), rs.Rules...)
	seen := map[string]int{}
	for i := range c.Ruleset.Rules {
		r := &c.Ruleset.Rules[i]
		bad := func(field, format string, args ...any) {
			probs = append(probs, Problem{RuleID: r.ID, Index: i, Field: field, Err: fmt.Errorf(format, args...)})
		}
		switch {
		case strings.TrimSpace(r.ID) == "":
			bad("id", "must not be empty")
		case seen[r.ID] > 0:
			bad("id", "duplicate of rule #%d", seen[r.ID]-1)
		default:
			seen[r.ID] = i + 1
		}
		if int(r.Action) >= len(actionNames) {
			bad("action", "invalid action %d", r.Action)
		}
		if r.Action == Hold && r.HoldFor <= 0 {
			bad("hold_for", "HOLD rules need a positive hold_for, e.g. \"30m\"")
		}
		if r.Action != Hold && r.HoldFor != 0 {
			bad("hold_for", "only HOLD rules may set hold_for")
		}
		if r.Mode > Shadow {
			bad("mode", "invalid mode %d", r.Mode)
		}
		prog, err := expr.Compile(r.Expr, pixSchema, eo)
		if err != nil {
			probs = append(probs, Problem{RuleID: r.ID, Index: i, Field: "expr", Err: err})
			continue
		}
		if prog.Type() != expr.Bool {
			bad("expr", "expression is %s, want bool", prog.Type())
			continue
		}
		fm := r.OnError
		if fm == FailUnset {
			fm = rs.FailModes[r.Action]
		}
		if fm == FailUnset {
			fm = FailOpen
		}
		c.rules = append(c.rules, compiledRule{Rule: r, prog: prog, failMode: fm, hasAggs: len(prog.Aggregates()) > 0})
	}
	if len(probs) > 0 {
		return nil, &ValidationError{Problems: probs}
	}
	canon, _ := json.Marshal(c.Ruleset)
	sum := sha256.Sum256(canon)
	c.Digest = hex.EncodeToString(sum[:6])
	return c, nil
}

// Label returns "name@version".
func (c *Compiled) Label() string { return c.Ruleset.Name + "@" + c.Ruleset.Version }

// Program returns the compiled expression of a rule, for tooling.
func (c *Compiled) Program(id string) (*expr.Program, bool) {
	for _, r := range c.rules {
		if r.ID == id {
			return r.prog, true
		}
	}
	return nil, false
}

// Windows returns every distinct aggregate the ruleset reads, which tells
// capacity planning which velocity windows matter.
func (c *Compiled) Windows() []expr.Aggregate {
	var out []expr.Aggregate
	seen := map[expr.Aggregate]bool{}
	for _, r := range c.rules {
		for _, a := range r.prog.Aggregates() {
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	return out
}
