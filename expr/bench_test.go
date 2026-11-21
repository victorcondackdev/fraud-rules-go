package expr

import (
	"testing"
)

const benchRule = `tx.amount > 1000_00 && hour(tx.time, "America/Sao_Paulo") between 20 and 5 && !payer.device.trusted || count(payer.transfers, 10m) >= 5 || sum(payer.transfers, 1h) > 5000_00`

func BenchmarkCompile(b *testing.B) {
	s := testSchema()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Compile(benchRule, s, Options{}); err != nil {
			b.Fatal(err)
		}
	}
}

// fastEnv is a struct-backed Env like the engine's, without map lookups.
type fastEnv struct{ amount, at, now int64 }

func (e *fastEnv) Int(id int) int64 {
	if id == 0 {
		return e.amount
	}
	return e.at
}
func (e *fastEnv) Bool(int) bool                      { return false }
func (e *fastEnv) String(int) string                  { return "" }
func (e *fastEnv) List(int) []string                  { return nil }
func (e *fastEnv) Now() int64                         { return e.now }
func (e *fastEnv) Aggregate(Aggregate) (int64, error) { return 1, nil }

func BenchmarkEval(b *testing.B) {
	s := testSchema()
	p := MustCompile(benchRule, s, Options{})
	env := &fastEnv{amount: 1500_00, at: evalAt.UnixNano(), now: evalAt.UnixNano()}
	f := NewFrame(env)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Run(f); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEvalSimple(b *testing.B) {
	p := MustCompile(`tx.amount > 1000_00 && !payer.device.trusted`, testSchema(), Options{})
	f := NewFrame(&fastEnv{amount: 1500_00})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = p.Run(f)
	}
}
