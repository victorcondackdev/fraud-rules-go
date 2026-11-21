package expr

import (
	"strings"
	"testing"
	"time"
)

var fuzzSeeds = []string{
	`tx.amount > 1000_00 && hour(tx.time, "America/Sao_Paulo") between 20 and 6 && !payer.device.trusted`,
	`count(payer.transfers, 10m) >= 5 || sum(payer.transfers, 1h) > 5000_00`,
	`distinct(payer.transfers, 1h) >= 3 && count(pair.transfers, 90d) == 0`,
	`"senior" in payer.tags && tx.description contains "pix"`,
	`payer.type not in ["PF", "PJ"] || tx.channel startsWith "a"`,
	`age(payer.device.first_seen) < 24h && isNight(tx.time)`,
	`tx.amount / (payer.night_limit - 100000) % 7 == -1`,
	`weekday(tx.time) in [0, 6] && len(payer.tags) > 1`,
	`tx.time - 1h30m > payer.device.first_seen + 3 * 10m`,
	`((((1))))`, `"a\"b\\"`, `[1, 2,]`, `1__2`, `10x`, `"unterminated`, `a = b`, "\xff\xfe",
}

func FuzzLex(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		toks, err := lex(src)
		if err != nil {
			if _, ok := err.(*Error); !ok {
				t.Fatalf("lex error %T is not *Error", err)
			}
			return
		}
		if len(toks) == 0 || toks[len(toks)-1].kind != tEOF {
			t.Fatal("token stream must end with EOF")
		}
		prev := 0
		for _, tk := range toks {
			if tk.pos < prev || tk.end < tk.pos || tk.end > len(src) {
				t.Fatalf("bad span %d..%d after %d (len %d)", tk.pos, tk.end, prev, len(src))
			}
			prev = tk.end
		}
	})
}

func FuzzParse(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		n, _, err := parse(src)
		if err != nil {
			e, ok := err.(*Error)
			if !ok {
				t.Fatalf("parse error %T is not *Error", err)
			}
			if e.Pos.Offset < 0 || e.Pos.Offset > len(src) || e.End < e.Pos.Offset || e.End > len(src) {
				t.Fatalf("error span %d..%d out of range for %q", e.Pos.Offset, e.End, src)
			}
			_ = e.Snippet()
			return
		}
		// The canonical form must parse back to itself.
		out := format(n)
		n2, _, err := parse(out)
		if err != nil {
			// Parenthesizing can only push depth or size past the limits.
			if e, ok := err.(*Error); ok && e.Kind == ErrTooComplex {
				return
			}
			t.Fatalf("canonical form %q of %q does not parse: %v", out, src, err)
		}
		if out2 := format(n2); out2 != out {
			t.Fatalf("not a fixed point: %q -> %q", out, out2)
		}
	})
}

func FuzzCompile(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	s := testSchema()
	f.Fuzz(func(t *testing.T, src string) {
		p, err := Compile(src, s, Options{})
		if err != nil {
			e, ok := err.(*Error)
			if !ok {
				t.Fatalf("compile error %T is not *Error", err)
			}
			if !strings.HasPrefix(e.Error(), e.Pos.String()+": ") {
				t.Fatalf("error %q lacks position", e)
			}
			_ = e.Snippet()
			return
		}
		if p.Cost() > DefaultMaxCost {
			t.Fatalf("cost %d over budget accepted", p.Cost())
		}
		if p.Type() == Invalid || p.Type() == Stream {
			t.Fatalf("compiled to type %s", p.Type())
		}
	})
}

// FuzzEval compiles the input and evaluates it against field values and
// aggregates derived from the fuzzer's numbers. Evaluation must not panic
// and must be deterministic.
func FuzzEval(f *testing.F) {
	for i, s := range fuzzSeeds {
		f.Add(s, int64(i*7919), int64(1000_00), "PF", true)
	}
	s := testSchema()
	f.Fuzz(func(t *testing.T, src string, n, limit int64, str string, flag bool) {
		p, err := Compile(src, s, Options{})
		if err != nil {
			return
		}
		at := time.Unix(0, n).UTC()
		env := &MapEnv{
			Schema: s, At: at,
			Vars: map[string]any{
				"tx.amount": n, "tx.time": at, "tx.channel": str, "tx.description": str,
				"payer.type": str, "payer.night_limit": limit, "payer.device.trusted": flag,
				"payer.device.first_seen": time.Unix(0, limit).UTC(), "payer.tags": []string{str},
				"payer.wait": time.Duration(limit),
			},
		}
		v1, err1 := p.Eval(env)
		v2, err2 := p.Eval(env)
		if (err1 == nil) != (err2 == nil) || v1.String() != v2.String() {
			t.Fatalf("non-deterministic: %v/%v vs %v/%v", v1, err1, v2, err2)
		}
		if p.Type() == Bool {
			b, err := p.EvalBool(env)
			if err == nil && err1 == nil && b != v1.Bool {
				t.Fatalf("EvalBool %v != Eval %v", b, v1.Bool)
			}
		}
		_ = p.Explain(env)
	})
}
