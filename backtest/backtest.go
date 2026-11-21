// Package backtest replays labeled historical transactions through a
// ruleset and measures how well each rule, and the ruleset as a whole,
// separates fraud from legitimate traffic.
//
// Replay is exact: the feature store runs on event time and every
// expression uses the transaction time as "now", so the velocity features
// a rule sees in a backtest are the ones it would have seen in production.
package backtest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/victorcondackdev/fraud-rules-go/engine"
	"github.com/victorcondackdev/fraud-rules-go/velocity"
)

// Labels accepted in [engine.Transaction.Label].
const (
	LabelFraud = "fraud"
	LabelLegit = "legit"
)

// ErrLabel reports a history line without a valid label.
var ErrLabel = errors.New("backtest: label must be \"fraud\" or \"legit\"")

// Options tunes a run.
type Options struct {
	// Warmup is a leading period, measured from the first transaction,
	// whose transactions are recorded into the feature store but not
	// scored. It gives "first transfer to this beneficiary" and similar
	// rules the history they would have in production.
	Warmup time.Duration
	// RecordBlocked also records transactions the ruleset blocked. The
	// default (false) mirrors production, where a blocked Pix never
	// settles and so never reaches the velocity store.
	RecordBlocked bool
	// PromoteShadow scores shadow rules as if they were enforced, to see
	// the impact of promoting them.
	PromoteShadow bool
	// Store configures the feature store; zero means velocity defaults.
	Store velocity.Config
}

// Confusion counts outcomes of a binary classifier against the labels,
// by number of transactions and by amount in cents.
type Confusion struct {
	TP, FP, FN, TN     int
	TPAmount, FPAmount int64
	FNAmount, TNAmount int64
}

// Precision is TP / (TP + FP), or 0 with no positives.
func (c Confusion) Precision() float64 { return ratio(c.TP, c.TP+c.FP) }

// Recall is TP / (TP + FN), or 0 with no fraud.
func (c Confusion) Recall() float64 { return ratio(c.TP, c.TP+c.FN) }

// AmountRecall is the share of fraud value caught.
func (c Confusion) AmountRecall() float64 {
	if c.TPAmount+c.FNAmount == 0 {
		return 0
	}
	return float64(c.TPAmount) / float64(c.TPAmount+c.FNAmount)
}

// FalsePositiveRate is FP / (FP + TN): the share of legitimate customers
// who felt friction.
func (c Confusion) FalsePositiveRate() float64 { return ratio(c.FP, c.FP+c.TN) }

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func (c *Confusion) add(predicted, fraud bool, amount int64) {
	switch {
	case predicted && fraud:
		c.TP++
		c.TPAmount += amount
	case predicted:
		c.FP++
		c.FPAmount += amount
	case fraud:
		c.FN++
		c.FNAmount += amount
	default:
		c.TN++
		c.TNAmount += amount
	}
}

// RuleStats measures one rule over the scored transactions.
type RuleStats struct {
	ID     string        `json:"id"`
	Action engine.Action `json:"action"`
	Mode   engine.Mode   `json:"mode"`
	Hits   int           `json:"hits"`
	// Unique counts fraud transactions this rule was the only one to
	// catch: a rule with zero unique catches is redundant with the rest.
	Unique    int       `json:"unique"`
	Confusion Confusion `json:"confusion"`
}

// PatternStats is recall per injected fraud pattern.
type PatternStats struct {
	Pattern     string `json:"pattern"`
	Total       int    `json:"total"`
	Flagged     int    `json:"flagged"`
	Interdicted int    `json:"interdicted"`
}

// Report is the result of a run.
type Report struct {
	Ruleset      string `json:"ruleset"`
	Transactions int    `json:"transactions"` // scored, warm-up excluded
	Warmup       int    `json:"warmup"`
	Fraud        int    `json:"fraud"`
	Legit        int    `json:"legit"`
	FraudAmount  int64  `json:"fraud_amount"`
	LegitAmount  int64  `json:"legit_amount"`
	// Flagged treats any action other than ALLOW as a positive.
	Flagged Confusion `json:"flagged"`
	// Interdicted treats BLOCK and HOLD as positives: money that did not
	// leave (immediately).
	Interdicted Confusion             `json:"interdicted"`
	Actions     map[engine.Action]int `json:"actions"`
	Rules       []RuleStats           `json:"rules"`
	Patterns    []PatternStats        `json:"patterns"`
	MeanEvalNs  int64                 `json:"mean_eval_ns"`
}

// ReadHistory decodes JSONL transactions (one JSON object per line, blank
// lines ignored) and returns them sorted by time.
func ReadHistory(r io.Reader) ([]engine.Transaction, error) {
	var txs []engine.Transaction
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(trimSpace(b)) == 0 {
			continue
		}
		var tx engine.Transaction
		if err := json.Unmarshal(b, &tx); err != nil {
			return nil, fmt.Errorf("backtest: line %d: %w", line, err)
		}
		txs = append(txs, tx)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("backtest: reading history: %w", err)
	}
	sort.SliceStable(txs, func(i, j int) bool { return txs[i].Time.Before(txs[j].Time) })
	return txs, nil
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// Run replays txs (sorted by time) through rs with a fresh feature store.
func Run(ctx context.Context, rs engine.Ruleset, txs []engine.Transaction, opts Options) (*Report, error) {
	if opts.PromoteShadow {
		rs.Rules = append([]engine.Rule(nil), rs.Rules...)
		for i := range rs.Rules {
			rs.Rules[i].Mode = engine.Enforce
		}
	}
	store, err := velocity.New(opts.Store)
	if err != nil {
		return nil, err
	}
	eng := engine.New(store, engine.Options{Logger: discardLogger})
	c, err := engine.Compile(rs, eng.CompileOptions())
	if err != nil {
		return nil, err
	}
	c = eng.Load(c)

	rep := &Report{Ruleset: c.Label(), Actions: map[engine.Action]int{}}
	idx := map[string]int{}
	for _, r := range rs.Rules {
		if !r.IsEnabled() {
			continue
		}
		idx[r.ID] = len(rep.Rules)
		rep.Rules = append(rep.Rules, RuleStats{ID: r.ID, Action: r.Action, Mode: r.Mode})
	}
	patterns := map[string]*PatternStats{}
	var warmEnd time.Time
	if len(txs) > 0 {
		warmEnd = txs[0].Time.Add(opts.Warmup)
	}
	var totalNs int64
	hit := make([]bool, len(rep.Rules))
	for i := range txs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tx := &txs[i]
		if tx.Label != LabelFraud && tx.Label != LabelLegit {
			return nil, fmt.Errorf("%w: transaction %q has %q", ErrLabel, tx.ID, tx.Label)
		}
		if tx.Time.Before(warmEnd) {
			rep.Warmup++
			eng.Record(tx)
			continue
		}
		d, err := eng.Evaluate(ctx, tx)
		if err != nil {
			return nil, err
		}
		totalNs += d.ElapsedNs
		fraud := tx.Label == LabelFraud
		rep.Transactions++
		if fraud {
			rep.Fraud++
			rep.FraudAmount += tx.Amount
		} else {
			rep.Legit++
			rep.LegitAmount += tx.Amount
		}
		rep.Actions[d.Action]++
		rep.Flagged.add(d.Action != engine.Allow, fraud, tx.Amount)
		interdicted := d.Action == engine.Block || d.Action == engine.Hold
		rep.Interdicted.add(interdicted, fraud, tx.Amount)

		clear(hit)
		nhit := 0
		for _, hs := range [][]engine.Hit{d.Hits, d.ShadowHits} {
			for _, h := range hs {
				if j, ok := idx[h.RuleID]; ok && !hit[j] {
					hit[j] = true
					if rep.Rules[j].Mode == engine.Enforce && h.Action != engine.Allow {
						nhit++
					}
				}
			}
		}
		for j := range rep.Rules {
			rs := &rep.Rules[j]
			rs.Confusion.add(hit[j], fraud, tx.Amount)
			if hit[j] {
				rs.Hits++
				if fraud && nhit == 1 && rs.Mode == engine.Enforce && rs.Action != engine.Allow {
					rs.Unique++
				}
			}
		}
		if fraud {
			name := tx.Pattern
			if name == "" {
				name = "unlabeled"
			}
			ps := patterns[name]
			if ps == nil {
				ps = &PatternStats{Pattern: name}
				patterns[name] = ps
			}
			ps.Total++
			if d.Action != engine.Allow {
				ps.Flagged++
			}
			if interdicted {
				ps.Interdicted++
			}
		}
		if d.Action != engine.Block || opts.RecordBlocked {
			eng.Record(tx)
		}
	}
	if rep.Transactions > 0 {
		rep.MeanEvalNs = totalNs / int64(rep.Transactions)
	}
	for _, ps := range patterns {
		rep.Patterns = append(rep.Patterns, *ps)
	}
	sort.Slice(rep.Patterns, func(i, j int) bool { return rep.Patterns[i].Pattern < rep.Patterns[j].Pattern })
	return rep, nil
}
