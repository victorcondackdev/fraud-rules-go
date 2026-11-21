package expr

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func testSchema() *Schema {
	s := NewSchema()
	s.Define("tx.amount", Int)
	s.Define("tx.time", Time)
	s.Define("tx.channel", String)
	s.Define("tx.description", String)
	s.Define("payer.type", String)
	s.Define("payer.night_limit", Int)
	s.Define("payer.device.trusted", Bool)
	s.Define("payer.device.first_seen", Time)
	s.Define("payer.tags", StringList)
	s.Define("payer.wait", Duration)
	s.Define("payer.transfers", Stream)
	s.Define("pair.transfers", Stream)
	return s
}

// 2026-11-21 23:30 in São Paulo (UTC-3) is 02:30 UTC on the 22nd.
var evalAt = time.Date(2026, 11, 22, 2, 30, 0, 0, time.UTC)

func testEnv(s *Schema) *MapEnv {
	return &MapEnv{
		Schema: s,
		At:     evalAt,
		Vars: map[string]any{
			"tx.amount":               int64(150000),
			"tx.time":                 evalAt,
			"tx.channel":              "app",
			"tx.description":          "aluguel novembro",
			"payer.type":              "PF",
			"payer.night_limit":       100000,
			"payer.device.trusted":    false,
			"payer.device.first_seen": evalAt.Add(-30 * time.Minute),
			"payer.tags":              []string{"senior", "vip"},
			"payer.wait":              90 * time.Second,
		},
		Aggregates: map[string]int64{
			"count(payer.transfers, 10m)":   5,
			"sum(payer.transfers, 1h)":      600000,
			"distinct(payer.transfers, 1h)": 4,
			"count(pair.transfers, 90d)":    0,
		},
	}
}

func TestEval(t *testing.T) {
	s := testSchema()
	tests := []struct {
		src  string
		want string
	}{
		// literals and arithmetic
		{`1 + 2 * 3`, `7`},
		{`(1 + 2) * 3`, `9`},
		{`10 - 4 - 3`, `3`},
		{`17 / 5`, `3`},
		{`17 % 5`, `2`},
		{`-5 + 2`, `-3`},
		{`--5`, `5`},
		{`1_000_00`, `100000`},
		{`"a\"b\\c\n\t"`, `"a\"b\\c\n\t"`},
		{`true`, `true`},
		{`!true`, `false`},
		{`10m`, `10m`},
		{`1h30m`, `1h30m`},
		{`7d`, `7d`},
		{`1500ms`, `1s500ms`},
		{`1h + 30m`, `1h30m`},
		{`1h - 2h`, `-1h`},
		{`10m * 3`, `30m`},
		{`3 * 10m`, `30m`},
		{`1h / 4`, `15m`},
		{`-10m`, `-10m`},
		{`["a", "b"]`, `["a", "b"]`},
		{`[1, -2, 3]`, `[1, -2, 3]`},
		// fields
		{`tx.amount`, `150000`},
		{`payer.night_limit`, `100000`},
		{`payer.tags`, `["senior", "vip"]`},
		{`payer.wait`, `1m30s`},
		{`tx.time`, `2026-11-22T02:30:00Z`},
		{`tx.time - 1h`, `2026-11-22T01:30:00Z`},
		{`tx.time + 1h`, `2026-11-22T03:30:00Z`},
		{`1h + tx.time`, `2026-11-22T03:30:00Z`},
		{`tx.time - payer.device.first_seen`, `30m`},
		// comparison
		{`tx.amount > 1000_00`, `true`},
		{`tx.amount >= 150000`, `true`},
		{`tx.amount < 150000`, `false`},
		{`tx.amount <= 149999`, `false`},
		{`tx.amount == 150000`, `true`},
		{`tx.amount != 150000`, `false`},
		{`tx.amount > payer.night_limit`, `true`},
		{`tx.amount < payer.night_limit`, `false`},
		{`tx.amount <= payer.night_limit`, `false`},
		{`tx.amount >= payer.night_limit`, `true`},
		{`tx.amount == payer.night_limit`, `false`},
		{`payer.type == "PF"`, `true`},
		{`payer.type != "PJ"`, `true`},
		{`payer.device.trusted == false`, `true`},
		{`payer.device.trusted != true`, `true`},
		{`payer.wait < 2m`, `true`},
		{`tx.time > payer.device.first_seen`, `true`},
		// logic and short-circuit
		{`true && false`, `false`},
		{`true || false`, `true`},
		{`false && 1 / 0 == 0`, `false`},
		{`true || 1 / 0 == 0`, `true`},
		{`!payer.device.trusted && tx.amount > 0`, `true`},
		// between
		{`5 between 1 and 10`, `true`},
		{`11 between 1 and 10`, `false`},
		{`23 between 20 and 5`, `true`},
		{`3 between 20 and 5`, `true`},
		{`12 between 20 and 5`, `false`},
		{`tx.amount between payer.night_limit and payer.night_limit * 2`, `true`},
		// wrap-around with runtime bounds: lo > hi means v >= lo || v <= hi
		{`tx.amount between payer.night_limit * 2 and payer.night_limit`, `false`},
		{`tx.amount between payer.night_limit * 3 and payer.night_limit * 2`, `true`},
		{`payer.wait between 1m and 2m`, `true`},
		// in
		{`payer.type in ["PF", "PJ"]`, `true`},
		{`payer.type not in ["PF", "PJ"]`, `false`},
		{`payer.type in []`, `false`},
		{`tx.amount in []`, `false`},
		{`"senior" in payer.tags`, `true`},
		{`"minor" in payer.tags`, `false`},
		{`"minor" not in payer.tags`, `true`},
		{`3 in [1, 2, 3]`, `true`},
		{`4 not in [1, 2, 3]`, `true`},
		{`tx.channel in ["a","b","c","d","e","f","g","h","i","app"]`, `true`},
		{`tx.channel in ["a","b","c","d","e","f","g","h","i","j"]`, `false`},
		// strings
		{`tx.description startsWith "aluguel"`, `true`},
		{`tx.description endsWith "novembro"`, `true`},
		{`tx.description contains "guel"`, `true`},
		{`tx.description contains "pix"`, `false`},
		// builtins
		{`hour(tx.time)`, `23`},
		{`hour(tx.time, "UTC")`, `2`},
		{`hour(tx.time, "America/Sao_Paulo") between 20 and 6`, `true`},
		{`weekday(tx.time)`, `6`},
		{`weekday(tx.time, "UTC")`, `0`},
		{`isNight(tx.time)`, `true`},
		{`isNight(tx.time - 12h)`, `false`},
		{`isNight(tx.time, "Asia/Tokyo")`, `false`},
		{`age(payer.device.first_seen)`, `30m`},
		{`age(payer.device.first_seen) < 24h`, `true`},
		{`len(payer.tags)`, `2`},
		{`count(payer.transfers, 10m)`, `5`},
		{`count(payer.transfers, 10m) >= 5 || sum(payer.transfers, 1h) > 5000_00`, `true`},
		{`sum(payer.transfers, 1h)`, `600000`},
		{`distinct(payer.transfers, 1h)`, `4`},
		{`count(pair.transfers, 90d) == 0`, `true`},
		{`count(payer.transfers, 10m) + count(payer.transfers, 10m)`, `10`},
		// comments and whitespace
		{"tx.amount > 0 # nonzero\n  && true", `true`},
		{"\n\ttx.amount\r\n", `150000`},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			p, err := Compile(tt.src, s, Options{})
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			v, err := p.Eval(testEnv(s))
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if got := v.String(); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
			if p.Source() != tt.src {
				t.Errorf("Source() = %q", p.Source())
			}
			if v.Type == Bool {
				b, err := p.EvalBool(testEnv(s))
				if err != nil || b != v.Bool {
					t.Errorf("EvalBool = %v, %v", b, err)
				}
			}
		})
	}
}

func TestCompileErrors(t *testing.T) {
	s := testSchema()
	tests := []struct {
		src     string
		kind    error
		pos     string
		msgPart string
	}{
		{``, ErrSyntax, "1:1", "empty expression"},
		{`   `, ErrSyntax, "1:1", "empty expression"},
		{`1 +`, ErrSyntax, "1:4", "unexpected end of input"},
		{`(1 + 2`, ErrSyntax, "1:7", `expected ")"`},
		{`1 2`, ErrSyntax, "1:3", "expected end of expression"},
		{`tx.`, ErrSyntax, "1:4", "field name"},
		{`tx.amount > )`, ErrSyntax, "1:13", "an operand"},
		{`a = 1`, ErrSyntax, "1:3", `did you mean "=="`},
		{`a & b`, ErrSyntax, "1:3", `did you mean "&&"`},
		{`a | b`, ErrSyntax, "1:3", `did you mean "||"`},
		{`'x'`, ErrSyntax, "1:1", "double quotes"},
		{`"abc`, ErrSyntax, "1:1", "unterminated"},
		{"\"ab\nc\"", ErrSyntax, "1:1", "unterminated"},
		{`"a\`, ErrSyntax, "1:1", "unterminated"},
		{`"\q"`, ErrSyntax, "1:2", "unknown escape"},
		{`1__0`, ErrSyntax, "1:3", "misplaced"},
		{`10_`, ErrSyntax, "1:3", "misplaced"},
		{`99999999999999999999`, ErrSyntax, "1:1", "overflows"},
		{`10x`, ErrSyntax, "1:3", "invalid duration unit"},
		{`10min`, ErrSyntax, "1:3", "invalid duration unit"},
		{`1h30`, ErrSyntax, "1:1", "missing a unit"},
		{`999999999999d`, ErrSyntax, "1:1", "overflows"},
		{`1.5`, ErrSyntax, "1:1", "fractional"},
		{`@`, ErrSyntax, "1:1", "unexpected character"},
		{"\xff", ErrSyntax, "1:1", "invalid UTF-8"},
		{`é`, ErrSyntax, "1:1", "unexpected character"},
		{`x between 1 or 2`, ErrSyntax, "1:13", `"and"`},
		{`x not 1`, ErrSyntax, "1:7", `"in" after "not"`},
		{`f(1,`, ErrSyntax, "1:5", "an operand"},
		{`f(1 2)`, ErrSyntax, "1:5", `"," or ")"`},
		{`[1 2]`, ErrSyntax, "1:4", `"," or "]"`},
		{`not x`, ErrSyntax, "1:1", "an operand"},
		{`tx.amout > 1`, ErrUndefined, "1:1", `did you mean "tx.amount"`},
		{`zzz.qqq`, ErrUndefined, "1:1", `undefined field "zzz.qqq"`},
		{`nope(1)`, ErrUndefined, "1:1", `undefined function "nope"`},
		{`tx.amount + "x"`, ErrType, "1:11", `operator "+" not defined for int and string`},
		{`tx.amount && true`, ErrType, "1:1", "want bool"},
		{`true || 1`, ErrType, "1:9", "want bool"},
		{`!1`, ErrType, "1:1", `"!" needs bool`},
		{`-"a"`, ErrType, "1:1", `"-" needs int or duration`},
		{`"a" < "b"`, ErrType, "1:5", `"<"`},
		{`payer.tags == payer.tags`, ErrType, "1:12", `"=="`},
		{`1 == "1"`, ErrType, "1:3", `"=="`},
		{`1 contains "a"`, ErrType, "1:3", `"contains"`},
		{`tx.time + tx.time`, ErrType, "1:9", `"+"`},
		{`1 - 1h`, ErrType, "1:3", `"-"`},
		{`1h * 1h`, ErrType, "1:4", `"*"`},
		{`1 / 1h`, ErrType, "1:3", `"/"`},
		{`1h % 2`, ErrType, "1:4", `"%"`},
		{`"a" between 1 and 2`, ErrType, "1:1", `"between" needs`},
		{`1 between 1h and 2`, ErrType, "1:11", "lower bound"},
		{`1 between 1 and 2h`, ErrType, "1:17", "upper bound"},
		{`1 in ["a"]`, ErrType, "1:1", "cannot look up int"},
		{`"a" in [1]`, ErrType, "1:1", "cannot look up string"},
		{`true in []`, ErrType, "1:1", `"in" needs`},
		{`"a" in "abc"`, ErrType, "1:8", "want a list"},
		{`[1, "a"]`, ErrType, "1:5", "mixes"},
		{`[tx.amount]`, ErrType, "1:2", "literals"},
		{`hour()`, ErrType, "1:1", "takes 1 or 2 arguments"},
		{`age(1, 2)`, ErrType, "1:1", "takes 1 arguments"},
		{`hour(1)`, ErrType, "1:6", "argument 1 of hour() is int"},
		{`hour(tx.time, tx.channel)`, ErrType, "1:15", "string literal"},
		{`hour(tx.time, "Mars/Olympus")`, ErrType, "1:15", "unknown time zone"},
		{`count(tx.amount, 1h)`, ErrType, "1:7", "want stream"},
		{`count(payer.transfers, payer.wait)`, ErrType, "1:24", "duration literal"},
		{`count(payer.transfers, -1h)`, ErrType, "1:24", "duration literal"},
		{`count(payer.transfers, 0s)`, ErrType, "1:24", "positive"},
		{`payer.transfers`, ErrType, "1:1", "only be used inside"},
		{`payer.transfers == payer.transfers`, ErrType, "1:17", `"=="`},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			_, err := Compile(tt.src, s, Options{})
			if err == nil {
				t.Fatal("Compile succeeded, want error")
			}
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("error %T is not *Error", err)
			}
			if !errors.Is(err, tt.kind) {
				t.Errorf("kind = %v, want %v", e.Kind, tt.kind)
			}
			if e.Pos.String() != tt.pos {
				t.Errorf("pos = %s, want %s (%v)", e.Pos, tt.pos, err)
			}
			if !strings.Contains(err.Error(), tt.msgPart) {
				t.Errorf("error %q does not contain %q", err, tt.msgPart)
			}
			_ = e.Snippet() // must not panic for any position
		})
	}
}

func TestBudget(t *testing.T) {
	s := testSchema()
	src := strings.Repeat("count(payer.transfers, 1h) + ", 30) + "0 > 1"
	_, err := Compile(src, s, Options{})
	if !errors.Is(err, ErrTooComplex) {
		t.Fatalf("err = %v, want ErrTooComplex", err)
	}
	p, err := Compile(src, s, Options{MaxCost: 5000})
	if err != nil {
		t.Fatalf("with larger budget: %v", err)
	}
	if p.Cost() <= DefaultMaxCost {
		t.Errorf("cost %d should exceed default budget", p.Cost())
	}
	if n := len(p.Aggregates()); n != 1 {
		t.Errorf("aggregates deduplicated to %d, want 1", n)
	}

	deep := strings.Repeat("(", 100) + "1" + strings.Repeat(")", 100)
	if _, err := Compile(deep, s, Options{}); !errors.Is(err, ErrTooComplex) {
		t.Errorf("deep nesting: err = %v", err)
	}
	unary := strings.Repeat("!", 100) + "true"
	if _, err := Compile(unary, s, Options{}); !errors.Is(err, ErrTooComplex) {
		t.Errorf("deep unary: err = %v", err)
	}
	long := strings.Repeat(" ", maxSourceLen) + "1"
	if _, err := Compile(long, s, Options{}); !errors.Is(err, ErrTooComplex) {
		t.Errorf("long source: err = %v", err)
	}
}

func TestMaxWindow(t *testing.T) {
	s := testSchema()
	_, err := Compile(`count(payer.transfers, 400d) > 0`, s, Options{MaxWindow: 180 * 24 * time.Hour})
	if !errors.Is(err, ErrType) || !strings.Contains(err.Error(), "exceeds the feature store horizon 180d") {
		t.Fatalf("err = %v", err)
	}
}

func TestRuntimeErrors(t *testing.T) {
	s := testSchema()
	for _, src := range []string{`tx.amount / (payer.night_limit - 100000) > 0`, `tx.amount % (payer.night_limit - 100000) > 0`, `1h / (tx.amount - tx.amount) > 0s`} {
		p := MustCompile(src, s, Options{})
		if _, err := p.EvalBool(testEnv(s)); !errors.Is(err, ErrDivideByZero) {
			t.Errorf("%s: err = %v", src, err)
		}
		if _, err := p.Eval(testEnv(s)); !errors.Is(err, ErrDivideByZero) {
			t.Errorf("%s: Eval err = %v", src, err)
		}
	}
	// Missing aggregate in a non-nil map surfaces as an evaluation error.
	p := MustCompile(`count(pair.transfers, 1h) > 0`, s, Options{})
	if _, err := p.EvalBool(testEnv(s)); err == nil {
		t.Error("missing aggregate: want error")
	}
	// A nil aggregate map reads as zero.
	env := testEnv(s)
	env.Aggregates = nil
	if ok, err := p.EvalBool(env); err != nil || ok {
		t.Errorf("nil aggregates = %v, %v", ok, err)
	}
	// Run on a non-bool program is a type error.
	if _, err := MustCompile(`1`, s, Options{}).Run(NewFrame(env)); !errors.Is(err, ErrType) {
		t.Errorf("Run non-bool: %v", err)
	}
}

func TestFrameReuse(t *testing.T) {
	s := testSchema()
	p := MustCompile(`tx.amount > 1000_00`, s, Options{})
	bad := MustCompile(`1 / (tx.amount - tx.amount) == 0`, s, Options{})
	f := NewFrame(testEnv(s))
	if _, err := bad.Run(f); err == nil {
		t.Fatal("want division error")
	}
	if ok, err := p.Run(f); err != nil || !ok {
		t.Fatalf("frame not reset between runs: %v, %v", ok, err)
	}
	env := testEnv(s)
	env.Vars["tx.amount"] = int64(5)
	f.Reset(env)
	if ok, _ := p.Run(f); ok {
		t.Error("Reset did not rebind env")
	}
}

func TestNightOptions(t *testing.T) {
	s := testSchema()
	p := MustCompile(`isNight(tx.time)`, s, Options{NightStart: 22, NightEnd: 23})
	if ok, _ := p.EvalBool(testEnv(s)); !ok { // 23:30 local is not in [22,23)
		// expected: 23:30 is outside [22, 23)
	} else {
		t.Error("23:30 should not be in [22, 23)")
	}
	p = MustCompile(`isNight(tx.time)`, s, Options{NightStart: 23, NightEnd: 24})
	if ok, _ := p.EvalBool(testEnv(s)); !ok {
		t.Error("23:30 should be in [23, 24)")
	}
}

func TestFeaturesAndExplain(t *testing.T) {
	s := testSchema()
	p := MustCompile(`tx.amount > payer.night_limit && isNight(tx.time) && count(payer.transfers, 10m) >= 5 && tx.amount > 0`, s, Options{})
	var names []string
	for _, f := range p.Features() {
		names = append(names, f.Name)
	}
	want := "tx.amount|payer.night_limit|tx.time|isNight(tx.time)|count(payer.transfers, 10m)"
	if got := strings.Join(names, "|"); got != want {
		t.Errorf("features = %s\nwant       %s", got, want)
	}
	vals := p.Explain(testEnv(s))
	var got []string
	for _, v := range vals {
		got = append(got, v.Name+"="+v.Value.String())
	}
	if g := strings.Join(got, " "); g != `tx.amount=150000 payer.night_limit=100000 tx.time=2026-11-22T02:30:00Z isNight(tx.time)=true count(payer.transfers, 10m)=5` {
		t.Errorf("explain = %s", g)
	}
	if a := p.Aggregates(); len(a) != 1 || a[0].String() != "count(payer.transfers, 10m)" || a[0].Op != AggCount {
		t.Errorf("aggregates = %v", a)
	}
}

func TestMustCompilePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustCompile did not panic")
		}
	}()
	MustCompile(`1 +`, testSchema(), Options{})
}

func TestSchema(t *testing.T) {
	s := NewSchema()
	id := s.Define("a", Int)
	if s.Define("a", Int) != id {
		t.Error("redefining with the same type must return the same ID")
	}
	if f, ok := s.Lookup("a"); !ok || f.Type != Int || f.ID != id {
		t.Errorf("Lookup = %v, %v", f, ok)
	}
	if s.Name(id) != "a" || s.Name(-1) != "" || s.Name(99) != "" {
		t.Error("Name out of range")
	}
	if len(s.Fields()) != 1 {
		t.Error("Fields")
	}
	for _, bad := range []func(){
		func() { s.Define("a", String) },
		func() { s.Define("b", Invalid) },
		func() { s.Define("c", IntList) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("Define did not panic")
				}
			}()
			bad()
		}()
	}
}

func TestTypeAndValueStrings(t *testing.T) {
	if Type(200).String() != "invalid" || StringList.String() != "list<string>" {
		t.Error("Type.String")
	}
	if (Value{}).String() != "<invalid>" {
		t.Error("zero Value")
	}
	if Agg(0).String() != "agg?" || AggSum.String() != "sum" || AggDistinct.String() != "distinct" {
		t.Error("Agg.String")
	}
	for d, want := range map[time.Duration]string{
		0: "0s", time.Millisecond: "1ms", 90 * time.Minute: "1h30m", 49 * time.Hour: "2d1h",
		-time.Second: "-1s", time.Microsecond: "+1µs",
	} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"10m": 10 * time.Minute, "1h30m": 90 * time.Minute, "7d": 7 * 24 * time.Hour,
		"-5s": -5 * time.Second, "0": 0, "250ms": 250 * time.Millisecond,
	} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-", "m", "10", "10q", "1h ", "1.5h", "1h30"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) succeeded", in)
		}
	}
}

func TestMapEnvTypes(t *testing.T) {
	s := testSchema()
	env := &MapEnv{Schema: s, Vars: map[string]any{
		"tx.amount": "not an int", "tx.time": time.Time{}, "payer.wait": 3 * time.Second,
	}}
	id := func(n string) int { f, _ := s.Lookup(n); return f.ID }
	if env.Int(id("tx.amount")) != 0 || env.Int(id("tx.time")) != 0 || env.Int(id("payer.wait")) != int64(3*time.Second) {
		t.Error("MapEnv.Int conversions")
	}
	if env.Now() != 0 || UnixNano(time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)) != 0 {
		t.Error("out-of-range times must map to 0")
	}
	if env.Bool(id("payer.device.trusted")) || env.String(id("tx.channel")) != "" || env.List(id("payer.tags")) != nil {
		t.Error("missing values must be zero")
	}
}

func TestErrorSnippetMultiline(t *testing.T) {
	s := testSchema()
	src := "tx.amount > 0 &&\n\tpayer.typo == \"PF\""
	_, err := Compile(src, s, Options{})
	var e *Error
	if !errors.As(err, &e) {
		t.Fatal(err)
	}
	want := "2:2: undefined field \"payer.typo\" (did you mean \"payer.type\"?)\n   payer.typo == \"PF\"\n   ^~~~~~~~~~"
	if got := e.Snippet(); got != want {
		t.Errorf("snippet:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatRoundTrip(t *testing.T) {
	s := testSchema()
	for _, src := range []string{
		`a + b * c - d / e % f`, `!x && -y || z`, `x between 1 and 2 && y not in ["a", "b\n"]`,
		`f(1h30m, "s") contains "x"`, `[1, 2,]`, `(a startsWith b) endsWith c`,
	} {
		n, _, err := parse(src)
		if err != nil {
			t.Fatalf("parse(%q): %v", src, err)
		}
		out := format(n)
		n2, _, err := parse(out)
		if err != nil {
			t.Fatalf("reparse(%q): %v", out, err)
		}
		if format(n2) != out {
			t.Errorf("round trip of %q: %q != %q", src, format(n2), out)
		}
	}
	_ = s
}
