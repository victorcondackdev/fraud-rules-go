package engine

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/victorcondackdev/fraud-rules-go/velocity"
)

// benchEngine loads the default ruleset over a store warmed with a week of
// traffic for 1,000 payers, so aggregates read real buckets.
func benchEngine(b *testing.B, opts Options) (*Engine, []*Transaction) {
	data, err := os.ReadFile("../rulesets/pix-default.json")
	if err != nil {
		b.Fatal(err)
	}
	store, _ := velocity.New(velocity.Config{})
	opts.Logger = quiet
	e := New(store, opts)
	if _, err := e.LoadJSON(data); err != nil {
		b.Fatal(err)
	}
	start := night.AddDate(0, 0, -7)
	for i := 0; i < 20_000; i++ {
		tx := baseTx()
		tx.Time = start.Add(time.Duration(i) * 30 * time.Second)
		tx.Payer.ID = fmt.Sprint("acc-", i%1000)
		tx.Payer.Device.ID = fmt.Sprint("dev-", i%1000)
		tx.Beneficiary.ID = fmt.Sprint("ben-", i%37)
		tx.Amount = int64(1000 + i%50_000)
		e.Record(tx)
	}
	txs := make([]*Transaction, 1000)
	for i := range txs {
		tx := baseTx()
		tx.Time = start.Add(20_000 * 30 * time.Second)
		tx.Payer.ID = fmt.Sprint("acc-", i)
		tx.Payer.Device.ID = fmt.Sprint("dev-", i)
		tx.Beneficiary.ID = fmt.Sprint("ben-", i%41)
		tx.Amount = int64(5000 + 97*i)
		txs[i] = tx
	}
	return e, txs
}

// BenchmarkEvaluateDefaultRuleset measures one full decision (31 enabled
// rules, velocity lookups, explanation of matched rules) per op.
func BenchmarkEvaluateDefaultRuleset(b *testing.B) {
	e, txs := benchEngine(b, Options{})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.Evaluate(ctx, txs[i%len(txs)]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEvaluateDefaultRulesetNoExplain(b *testing.B) {
	e, txs := benchEngine(b, Options{NoExplain: true})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.Evaluate(ctx, txs[i%len(txs)]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEvaluateDefaultRulesetParallel(b *testing.B) {
	e, txs := benchEngine(b, Options{})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i++
			if _, err := e.Evaluate(ctx, txs[i%len(txs)]); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkEvaluateNoStore isolates the rule evaluation itself: every
// aggregate reads zero without touching a store.
func BenchmarkEvaluateNoStore(b *testing.B) {
	data, _ := os.ReadFile("../rulesets/pix-default.json")
	e := New(nil, Options{Logger: quiet})
	if _, err := e.LoadJSON(data); err != nil {
		b.Fatal(err)
	}
	tx := baseTx()
	tx.Time = night.Add(-10 * time.Hour)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = e.Evaluate(ctx, tx)
	}
}
