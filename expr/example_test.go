package expr_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/victorcondackdev/fraud-rules-go/expr"
)

func ExampleCompile() {
	s := expr.NewSchema()
	s.Define("tx.amount", expr.Int)
	s.Define("tx.time", expr.Time)
	s.Define("payer.device.trusted", expr.Bool)
	s.Define("payer.transfers", expr.Stream)

	p, err := expr.Compile(
		`tx.amount > 1000_00 && hour(tx.time, "America/Sao_Paulo") between 20 and 5 && !payer.device.trusted
		 || count(payer.transfers, 10m) >= 5`, s, expr.Options{})
	if err != nil {
		panic(err)
	}
	at := time.Date(2026, 11, 21, 23, 40, 0, 0, time.FixedZone("-03", -3*3600))
	env := &expr.MapEnv{
		Schema:     s,
		At:         at,
		Vars:       map[string]any{"tx.amount": 1500_00, "tx.time": at, "payer.device.trusted": false},
		Aggregates: map[string]int64{"count(payer.transfers, 10m)": 1},
	}
	matched, err := p.EvalBool(env)
	fmt.Println(matched, err)
	for _, f := range p.Explain(env) {
		fmt.Printf("%s = %s\n", f.Name, f.Value)
	}
	// Output:
	// true <nil>
	// tx.amount = 150000
	// tx.time = 2026-11-22T02:40:00Z
	// hour(tx.time, "America/Sao_Paulo") = 23
	// payer.device.trusted = false
	// count(payer.transfers, 10m) = 1
}

func ExampleError_Snippet() {
	s := expr.NewSchema()
	s.Define("tx.amount", expr.Int)
	s.Define("tx.time", expr.Time)

	_, err := expr.Compile(`tx.amount > 500_00 && age(tx.time) > "1h"`, s, expr.Options{})
	var e *expr.Error
	if errors.As(err, &e) {
		fmt.Println(e.Snippet())
		fmt.Println(errors.Is(err, expr.ErrType))
	}
	// Output:
	// 1:36: operator ">" not defined for duration and string
	//   tx.amount > 500_00 && age(tx.time) > "1h"
	//                                      ^
	// true
}

func ExampleParseDuration() {
	d, _ := expr.ParseDuration("1h30m")
	fmt.Println(d, expr.FormatDuration(36*time.Hour))
	// Output: 1h30m0s 1d12h
}
