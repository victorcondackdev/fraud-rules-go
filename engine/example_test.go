package engine_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/victorcondackdev/fraud-rules-go/engine"
	"github.com/victorcondackdev/fraud-rules-go/velocity"
)

const exampleRuleset = `{
  "name": "example", "version": "1",
  "thresholds": {"review": 30, "step_up": 60, "block": 100},
  "rules": [
    {"id": "night.limit", "action": "BLOCK", "score": 100,
     "expr": "isNight(tx.time) && tx.amount > payer.night_limit"},
    {"id": "new.device", "action": "STEP_UP", "score": 30,
     "expr": "!payer.device.trusted && age(payer.device.first_seen) < 24h"},
    {"id": "burst", "action": "HOLD", "hold_for": "30m", "score": 20,
     "expr": "count(payer.transfers, 10m) >= 2"},
    {"id": "fanout.candidate", "action": "BLOCK", "score": 50, "mode": "shadow",
     "expr": "distinct(payer.transfers, 1h) >= 2"}
  ]
}`

func Example() {
	store, _ := velocity.New(velocity.Config{})
	eng := engine.New(store, engine.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if _, err := eng.LoadJSON([]byte(exampleRuleset)); err != nil {
		panic(err)
	}
	brt := time.FixedZone("-03", -3*3600)
	now := time.Date(2026, 11, 21, 23, 40, 0, 0, brt)
	tx := engine.Transaction{
		ID: "E2E-1", Time: now, Amount: 900_00,
		Payer: engine.Payer{ID: "acc-1", Type: "PF", NightLimit: 1000_00,
			Device: engine.Device{ID: "dev-x", FirstSeen: now.Add(-15 * time.Minute)}},
		Beneficiary: engine.Beneficiary{ID: "ben-1"},
	}
	// Two settled transfers in the last few minutes, to two beneficiaries.
	for i, ben := range []string{"ben-1", "ben-2"} {
		prior := tx
		prior.Time, prior.Beneficiary.ID = now.Add(-time.Duration(i+2)*time.Minute), ben
		eng.Record(&prior)
	}

	d, _ := eng.Evaluate(context.Background(), &tx)
	fmt.Printf("%s for %s, score %d\n", d.Action, d.HoldFor, d.Score)
	for _, r := range d.Reasons {
		fmt.Println("reason:", r)
	}
	for _, h := range d.Hits {
		fmt.Printf("%s (%s %+d)\n", h.RuleID, h.Action, h.Score)
		for _, f := range h.Features {
			fmt.Printf("    %s = %s\n", f.Name, f.Value)
		}
	}
	for _, h := range d.ShadowHits {
		fmt.Printf("shadow %s would make it %s\n", h.RuleID, d.Shadow.Action)
	}
	// Output:
	// HOLD for 30m, score 50
	// reason: rule burst → HOLD
	// new.device (STEP_UP +30)
	//     payer.device.trusted = false
	//     payer.device.first_seen = 2026-11-22T02:25:00Z
	//     age(payer.device.first_seen) = 15m
	// burst (HOLD +20)
	//     count(payer.transfers, 10m) = 2
	// shadow fanout.candidate would make it BLOCK
}

func ExampleCompile_validation() {
	rs, _ := engine.ParseRuleset([]byte(`{"name": "bad", "version": "1", "rules": [
	  {"id": "limit", "action": "BLOCK", "expr": "tx.amount > payer.nigth_limit"}]}`))
	_, err := engine.Compile(rs, engine.CompileOptions{})
	if ve, ok := err.(*engine.ValidationError); ok {
		for _, p := range ve.Problems {
			fmt.Println(p.Snippet())
		}
	}
	// Output:
	// rule "limit" (#0) expr: 1:13: undefined field "payer.nigth_limit" (did you mean "payer.night_limit"?)
	//   tx.amount > payer.nigth_limit
	//               ^~~~~~~~~~~~~~~~~
}
