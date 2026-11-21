package backtest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/victorcondackdev/fraud-rules-go/engine"
	"github.com/victorcondackdev/fraud-rules-go/velocity"
)

var day = time.Date(2026, 10, 1, 14, 0, 0, 0, time.UTC)

func tx(id string, at time.Time, amount int64, payer, ben, label string) engine.Transaction {
	return engine.Transaction{ID: id, Time: at, Amount: amount, Label: label,
		Payer:       engine.Payer{ID: payer, Type: "PF", Device: engine.Device{ID: "d-" + payer, Trusted: true}},
		Beneficiary: engine.Beneficiary{ID: ben}}
}

func testRuleset() engine.Ruleset {
	return engine.Ruleset{Name: "bt", Version: "1", Rules: []engine.Rule{
		{ID: "big", Expr: `tx.amount >= 1000_00`, Action: engine.Block},
		{ID: "burst", Expr: `count(payer.transfers, 10m) >= 2`, Action: engine.Review},
		{ID: "shadow.medium", Expr: `tx.amount >= 500_00`, Action: engine.Hold, HoldFor: engine.Duration(time.Hour), Mode: engine.Shadow},
		{ID: "off", Expr: `true`, Action: engine.Block, Enabled: new(bool)},
	}}
}

func history() []engine.Transaction {
	return []engine.Transaction{
		tx("w1", day.Add(-48*time.Hour), 10_00, "a", "x", LabelLegit), // warm-up
		tx("1", day, 2000_00, "a", "x", LabelFraud),                   // big: TP
		tx("2", day.Add(time.Minute), 1500_00, "b", "y", LabelLegit),  // big: FP
		tx("3", day.Add(2*time.Minute), 600_00, "c", "z", LabelFraud), // missed by enforce, shadow TP
		tx("4", day.Add(3*time.Minute), 10_00, "c", "z", LabelFraud),  // burst? only 1 prior: no
		tx("5", day.Add(4*time.Minute), 10_00, "c", "z", LabelFraud),  // burst: TP (2 prior)
		tx("6", day.Add(time.Hour), 10_00, "d", "x", LabelLegit),      // TN
	}
}

func TestRun(t *testing.T) {
	rep, err := Run(context.Background(), testRuleset(), history(), Options{Warmup: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Warmup != 1 || rep.Transactions != 6 || rep.Fraud != 4 || rep.Legit != 2 {
		t.Fatalf("totals = %+v", rep)
	}
	if rep.FraudAmount != 2620_00 || rep.LegitAmount != 1510_00 {
		t.Errorf("amounts = %d %d", rep.FraudAmount, rep.LegitAmount)
	}
	f := rep.Flagged
	if f.TP != 2 || f.FP != 1 || f.FN != 2 || f.TN != 1 {
		t.Errorf("flagged = %+v", f)
	}
	if f.Precision() != 2.0/3 || f.Recall() != 0.5 || f.FalsePositiveRate() != 0.5 {
		t.Errorf("flagged ratios %v %v %v", f.Precision(), f.Recall(), f.FalsePositiveRate())
	}
	if math.Abs(f.AmountRecall()-2010_00.0/2620_00) > 1e-9 {
		t.Errorf("amount recall %v", f.AmountRecall())
	}
	if i := rep.Interdicted; i.TP != 1 || i.FP != 1 {
		t.Errorf("interdicted = %+v", i)
	}
	if rep.Actions[engine.Block] != 2 || rep.Actions[engine.Review] != 1 || rep.Actions[engine.Allow] != 3 {
		t.Errorf("actions = %v", rep.Actions)
	}
	byID := map[string]RuleStats{}
	for _, r := range rep.Rules {
		byID[r.ID] = r
	}
	if _, ok := byID["off"]; ok {
		t.Error("disabled rule reported")
	}
	if b := byID["big"]; b.Hits != 2 || b.Confusion.TP != 1 || b.Unique != 1 {
		t.Errorf("big = %+v", b)
	}
	if b := byID["burst"]; b.Hits != 1 || b.Confusion.TP != 1 || b.Unique != 1 {
		t.Errorf("burst = %+v", b)
	}
	if s := byID["shadow.medium"]; s.Hits != 3 || s.Confusion.TP != 2 || s.Unique != 0 || s.Mode != engine.Shadow {
		t.Errorf("shadow = %+v", s)
	}
	if len(rep.Patterns) != 1 || rep.Patterns[0].Pattern != "unlabeled" || rep.Patterns[0].Flagged != 2 {
		t.Errorf("patterns = %+v", rep.Patterns)
	}
	var b strings.Builder
	if err := rep.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"6 scored (1 warm-up)", "flagged (≠ALLOW)", "66.7%", "shadow.medium", "unlabeled"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("report missing %q:\n%s", want, b.String())
		}
	}
}

func TestPromoteShadow(t *testing.T) {
	rep, err := Run(context.Background(), testRuleset(), history(), Options{Warmup: 24 * time.Hour, PromoteShadow: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Interdicted.TP != 2 || rep.Actions[engine.Hold] != 1 {
		t.Errorf("promoted: interdicted %+v actions %v", rep.Interdicted, rep.Actions)
	}
	if rs := testRuleset(); rs.Rules[2].Mode != engine.Shadow {
		t.Error("promotion must not modify the caller's ruleset")
	}
}

// Blocked transfers never settle, so by default they do not feed velocity.
func TestRecordBlocked(t *testing.T) {
	h := []engine.Transaction{
		tx("1", day, 2000_00, "a", "x", LabelFraud),
		tx("2", day.Add(time.Minute), 2000_00, "a", "x", LabelFraud),
		tx("3", day.Add(2*time.Minute), 10_00, "a", "x", LabelFraud),
	}
	count := func(opts Options) int {
		rep, err := Run(context.Background(), testRuleset(), h, opts)
		if err != nil {
			t.Fatal(err)
		}
		return rep.Actions[engine.Review]
	}
	if n := count(Options{}); n != 0 {
		t.Errorf("default: %d reviews", n)
	}
	if n := count(Options{RecordBlocked: true}); n != 1 {
		t.Errorf("record blocked: %d reviews", n)
	}
}

func TestRunErrors(t *testing.T) {
	ctx := context.Background()
	bad := []engine.Transaction{tx("1", day, 1, "a", "b", "maybe")}
	if _, err := Run(ctx, testRuleset(), bad, Options{}); !errors.Is(err, ErrLabel) {
		t.Errorf("label: %v", err)
	}
	rs := testRuleset()
	rs.Rules[0].Expr = "tx.amount >"
	if _, err := Run(ctx, rs, history(), Options{}); !errors.Is(err, engine.ErrInvalidRuleset) {
		t.Errorf("invalid ruleset: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Run(cctx, testRuleset(), history(), Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
	if _, err := Run(ctx, testRuleset(), nil, Options{Store: configWithBadPrecision()}); err == nil {
		t.Error("bad store config accepted")
	}
	rep, err := Run(ctx, testRuleset(), nil, Options{})
	if err != nil || rep.Transactions != 0 || rep.MeanEvalNs != 0 {
		t.Errorf("empty history: %+v, %v", rep, err)
	}
	var z Confusion
	if z.Precision() != 0 || z.Recall() != 0 || z.AmountRecall() != 0 {
		t.Error("zero confusion ratios")
	}
}

func TestReadHistory(t *testing.T) {
	in := `{"id":"2","time":"2026-10-01T10:00:00Z","amount":1,"payer":{"id":"a"},"beneficiary":{"id":"b"},"label":"legit"}

  ` + "\t" + `
{"id":"1","time":"2026-10-01T09:00:00Z","amount":1,"payer":{"id":"a"},"beneficiary":{"id":"b"},"label":"fraud"}
`
	txs, err := ReadHistory(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 2 || txs[0].ID != "1" {
		t.Errorf("not sorted by time: %+v", txs)
	}
	_, err = ReadHistory(strings.NewReader("{}\n{oops}\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("bad line: %v", err)
	}
	_, err = ReadHistory(strings.NewReader(strings.Repeat("x", 5<<20)))
	if err == nil {
		t.Error("over-long line accepted")
	}
}

// TestSampleHistory pins the numbers quoted in the README: if the sample,
// the generator or the default ruleset changes, update both.
func TestSampleHistory(t *testing.T) {
	f, err := os.Open("../testdata/history-sample.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	txs, err := ReadHistory(f)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../rulesets/pix-default.json")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := engine.ParseRuleset(data)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), rs, txs, Options{Warmup: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprintf("scored=%d fraud=%d flagged=%.1f/%.1f interdicted=%.1f/%.1f",
		rep.Transactions, rep.Fraud,
		100*rep.Flagged.Precision(), 100*rep.Flagged.Recall(),
		100*rep.Interdicted.Precision(), 100*rep.Interdicted.Recall())
	const want = "scored=1544 fraud=174 flagged=64.4/98.9 interdicted=88.3/56.3"
	if got != want {
		t.Errorf("sample backtest:\n got %s\nwant %s", got, want)
	}
}

func configWithBadPrecision() velocity.Config { return velocity.Config{HLLPrecision: 99} }
