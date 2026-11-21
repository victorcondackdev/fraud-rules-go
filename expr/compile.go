package expr

import (
	"strings"
	"time"
)

// Options tunes compilation. The zero value is usable.
type Options struct {
	// MaxCost is the static cost budget. Each node costs 1; string
	// matching costs 4, time-zone builtins 5, velocity aggregates 26 or
	// 41. Zero means [DefaultMaxCost].
	MaxCost int
	// DefaultTimeZone is used by hour, weekday and isNight when no zone is
	// given. Empty means "America/Sao_Paulo".
	DefaultTimeZone string
	// NightStart and NightEnd delimit the local hours isNight reports as
	// night: hour >= NightStart || hour < NightEnd. Both zero means 20 and 6.
	NightStart, NightEnd int
	// MaxWindow, when positive, rejects aggregate windows longer than the
	// feature store can answer.
	MaxWindow time.Duration
}

// DefaultMaxCost is the cost budget used when [Options.MaxCost] is zero. It
// comfortably fits rules with a dozen aggregates while rejecting
// pathological machine-generated input.
const DefaultMaxCost = 500

func (o Options) withDefaults() Options {
	if o.MaxCost <= 0 {
		o.MaxCost = DefaultMaxCost
	}
	if o.DefaultTimeZone == "" {
		o.DefaultTimeZone = "America/Sao_Paulo"
	}
	if o.NightStart == 0 && o.NightEnd == 0 {
		o.NightStart, o.NightEnd = 20, 6
	}
	return o
}

// Env supplies field values and velocity aggregates during evaluation.
// Field IDs come from the [Schema] the program was compiled against.
// Implementations must be safe to call with any ID of the right type and
// should be cheap: they sit on the hot path.
type Env interface {
	Int(id int) int64 // Int, Duration (ns) and Time (Unix ns) fields
	Bool(id int) bool
	String(id int) string
	List(id int) []string
	// Now is the evaluation instant in Unix nanoseconds, used by age().
	// Using the transaction time rather than the wall clock keeps
	// evaluation deterministic and makes replays exact.
	Now() int64
	// Aggregate answers count/sum/distinct over a stream within a
	// trailing window ending at Now.
	Aggregate(a Aggregate) (int64, error)
}

// Frame carries per-evaluation state. Reusing one Frame across the rules
// of a transaction avoids an allocation per rule; a Frame must not be used
// by two goroutines at once.
type Frame struct {
	env Env
	err error
}

// NewFrame returns a frame bound to env.
func NewFrame(env Env) *Frame { return &Frame{env: env} }

// Reset rebinds the frame to env and clears any error.
func (f *Frame) Reset(env Env) { f.env, f.err = env, nil }

func (f *Frame) fail(err error) {
	if f.err == nil {
		f.err = err
	}
}

// code is a compiled node: exactly one function is set, chosen by type.
type code struct {
	t       Type
	i       func(*Frame) int64 // Int, Duration, Time
	b       func(*Frame) bool
	s       func(*Frame) string
	l       func(*Frame) []string
	isConst bool  // i is a constant
	cval    int64 // value when isConst
	ints    []int64
	strs    []string
}

// Feature is an input an expression reads: a field or a builtin call. The
// engine evaluates features of matched rules to explain decisions.
type Feature struct {
	Name string // canonical source, e.g. "tx.amount" or "count(payer.transfers, 10m)"
	Type Type
	eval code
}

// FeatureValue is a feature together with its value in one evaluation.
type FeatureValue struct {
	Name  string
	Value Value
}

// Program is a compiled, type-checked expression. It is immutable and safe
// for concurrent use.
type Program struct {
	src      string
	typ      Type
	cost     int
	root     code
	features []Feature
	aggs     []Aggregate
}

// Compile parses, type-checks and compiles src against schema. Errors are
// always an [*Error] carrying the position of the problem.
func Compile(src string, schema *Schema, opts Options) (*Program, error) {
	opts = opts.withDefaults()
	root, _, err := parse(src)
	if err != nil {
		return nil, err
	}
	c := &checker{src: src, schema: schema, opts: &opts,
		locs: map[*callNode]*time.Location{}, aggs: map[*callNode]Aggregate{}}
	t, err := c.check(root)
	if err != nil {
		return nil, err
	}
	if t == Stream {
		return nil, c.errf(ErrType, root, "a stream can only be used inside count, sum or distinct")
	}
	if c.cost > opts.MaxCost {
		return nil, c.errf(ErrTooComplex, root, "expression cost %d exceeds budget %d", c.cost, opts.MaxCost)
	}
	g := &gen{checker: c, seen: map[string]bool{}}
	p := &Program{src: src, typ: t, cost: c.cost}
	p.root = g.gen(root)
	p.features = g.features
	p.aggs = g.aggList
	return p, nil
}

// MustCompile is like [Compile] but panics on error. It is meant for
// expressions fixed at build time, such as tests and examples.
func MustCompile(src string, schema *Schema, opts Options) *Program {
	p, err := Compile(src, schema, opts)
	if err != nil {
		panic(err)
	}
	return p
}

// Source returns the expression text.
func (p *Program) Source() string { return p.src }

// Type returns the result type.
func (p *Program) Type() Type { return p.typ }

// Cost returns the static cost computed by the checker.
func (p *Program) Cost() int { return p.cost }

// Features returns the fields and builtin calls the program reads, in
// source order, without duplicates.
func (p *Program) Features() []Feature { return append([]Feature(nil), p.features...) }

// Aggregates returns the velocity lookups the program may perform.
func (p *Program) Aggregates() []Aggregate { return append([]Aggregate(nil), p.aggs...) }

// Run evaluates a bool program in f. Non-bool programs report ErrType.
func (p *Program) Run(f *Frame) (bool, error) {
	if p.typ != Bool {
		return false, ErrType
	}
	f.err = nil
	v := p.root.b(f)
	if f.err != nil {
		return false, f.err
	}
	return v, nil
}

// EvalBool evaluates a bool program against env.
func (p *Program) EvalBool(env Env) (bool, error) { return p.Run(&Frame{env: env}) }

// Eval evaluates a program of any type against env.
func (p *Program) Eval(env Env) (Value, error) {
	f := &Frame{env: env}
	v := p.root.value(f)
	if f.err != nil {
		return Value{}, f.err
	}
	return v, nil
}

// Explain evaluates every feature of the program against env. Errors in
// individual features (such as an unavailable feature store) are reported
// as the zero value; explanations must never fail a decision.
func (p *Program) Explain(env Env) []FeatureValue {
	out := make([]FeatureValue, len(p.features))
	f := &Frame{env: env}
	for i, ft := range p.features {
		f.err = nil
		out[i] = FeatureValue{Name: ft.Name, Value: ft.eval.value(f)}
	}
	return out
}

func (c code) value(f *Frame) Value {
	switch c.t {
	case Bool:
		return Value{Type: Bool, Bool: c.b(f)}
	case Int, Duration, Time:
		return Value{Type: c.t, Int: c.i(f)}
	case String:
		return Value{Type: String, Str: c.s(f)}
	case StringList:
		return Value{Type: StringList, List: c.l(f)}
	case IntList:
		return Value{Type: IntList, Ints: c.ints}
	}
	return Value{}
}

type gen struct {
	*checker
	seen     map[string]bool
	features []Feature
	aggList  []Aggregate
}

func (g *gen) feature(n node, c code) {
	name := format(n)
	if g.seen[name] {
		return
	}
	g.seen[name] = true
	g.features = append(g.features, Feature{Name: name, Type: c.t, eval: c})
}

func constCode(t Type, v int64) code {
	return code{t: t, i: func(*Frame) int64 { return v }, isConst: true, cval: v}
}

func (g *gen) gen(n node) code {
	switch n := n.(type) {
	case *litNode:
		switch n.kind {
		case tTrue:
			return code{t: Bool, b: func(*Frame) bool { return true }}
		case tFalse:
			return code{t: Bool, b: func(*Frame) bool { return false }}
		case tInt:
			return constCode(Int, n.ival)
		case tDuration:
			return constCode(Duration, n.ival)
		default:
			s := n.sval
			return code{t: String, s: func(*Frame) string { return s }}
		}
	case *fieldNode:
		c := fieldCode(n.field)
		if n.field.Type != Stream {
			g.feature(n, c)
		}
		return c
	case *unaryNode:
		x := g.gen(n.x)
		if n.op == tBang {
			xb := x.b
			return code{t: Bool, b: func(f *Frame) bool { return !xb(f) }}
		}
		if x.isConst {
			return constCode(x.t, -x.cval)
		}
		xi := x.i
		return code{t: x.t, i: func(f *Frame) int64 { return -xi(f) }}
	case *binaryNode:
		return g.binary(n)
	case *betweenNode:
		return g.between(n)
	case *inNode:
		return g.in(n)
	case *listNode:
		c := code{t: n.typ()}
		for _, e := range n.elems {
			if v, ok := constInt(e); ok {
				c.ints = append(c.ints, v)
			} else {
				c.strs = append(c.strs, e.(*litNode).sval)
			}
		}
		strs := c.strs
		c.l = func(*Frame) []string { return strs }
		return c
	case *callNode:
		c := g.call(n)
		g.feature(n, c)
		return c
	}
	panic("expr: unreachable node in gen")
}

func fieldCode(fl Field) code {
	id := fl.ID
	switch fl.Type {
	case Bool:
		return code{t: Bool, b: func(f *Frame) bool { return f.env.Bool(id) }}
	case Int, Duration, Time:
		return code{t: fl.Type, i: func(f *Frame) int64 { return f.env.Int(id) }}
	case String:
		return code{t: String, s: func(f *Frame) string { return f.env.String(id) }}
	case StringList:
		return code{t: StringList, l: func(f *Frame) []string { return f.env.List(id) }}
	}
	return code{t: Stream} // only consumed by aggregates, which read the ID
}

func (g *gen) binary(n *binaryNode) code {
	l, r := g.gen(n.l), g.gen(n.r)
	switch n.op {
	case tAndAnd:
		lb, rb := l.b, r.b
		return code{t: Bool, b: func(f *Frame) bool { return lb(f) && rb(f) }}
	case tOrOr:
		lb, rb := l.b, r.b
		return code{t: Bool, b: func(f *Frame) bool { return lb(f) || rb(f) }}
	case tStartsWith, tEndsWith, tContains:
		ls, rs := l.s, r.s
		var fn func(string, string) bool
		switch n.op {
		case tStartsWith:
			fn = strings.HasPrefix
		case tEndsWith:
			fn = strings.HasSuffix
		default:
			fn = strings.Contains
		}
		return code{t: Bool, b: func(f *Frame) bool { return fn(ls(f), rs(f)) }}
	case tEq, tNe:
		eq := equality(l, r)
		if n.op == tNe {
			return code{t: Bool, b: func(f *Frame) bool { return !eq(f) }}
		}
		return code{t: Bool, b: eq}
	case tLt, tLe, tGt, tGe:
		return code{t: Bool, b: compare(n.op, l, r)}
	}
	return arith(n.op, n.typ(), l, r)
}

func equality(l, r code) func(*Frame) bool {
	switch l.t {
	case Bool:
		lb, rb := l.b, r.b
		return func(f *Frame) bool { return lb(f) == rb(f) }
	case String:
		ls, rs := l.s, r.s
		return func(f *Frame) bool { return ls(f) == rs(f) }
	}
	li, ri := l.i, r.i
	if r.isConst {
		v := r.cval
		return func(f *Frame) bool { return li(f) == v }
	}
	return func(f *Frame) bool { return li(f) == ri(f) }
}

func compare(op tokKind, l, r code) func(*Frame) bool {
	li, ri := l.i, r.i
	if r.isConst {
		v := r.cval
		switch op {
		case tLt:
			return func(f *Frame) bool { return li(f) < v }
		case tLe:
			return func(f *Frame) bool { return li(f) <= v }
		case tGt:
			return func(f *Frame) bool { return li(f) > v }
		default:
			return func(f *Frame) bool { return li(f) >= v }
		}
	}
	switch op {
	case tLt:
		return func(f *Frame) bool { return li(f) < ri(f) }
	case tLe:
		return func(f *Frame) bool { return li(f) <= ri(f) }
	case tGt:
		return func(f *Frame) bool { return li(f) > ri(f) }
	default:
		return func(f *Frame) bool { return li(f) >= ri(f) }
	}
}

// arith compiles + - * / %. Integer overflow wraps (two's complement), as
// in Go; it is deterministic and amounts in cents stay far from the limits.
func arith(op tokKind, t Type, l, r code) code {
	li, ri := l.i, r.i
	if l.t == Duration && r.t == Time { // duration + time: swap to time + duration
		li, ri = ri, li
	}
	var fn func(*Frame) int64
	switch op {
	case tPlus:
		fn = func(f *Frame) int64 { return li(f) + ri(f) }
	case tMinus:
		fn = func(f *Frame) int64 { return li(f) - ri(f) }
	case tStar:
		fn = func(f *Frame) int64 { return li(f) * ri(f) }
	case tSlash:
		fn = func(f *Frame) int64 {
			a, b := li(f), ri(f)
			if b == 0 {
				f.fail(ErrDivideByZero)
				return 0
			}
			return a / b
		}
	default:
		fn = func(f *Frame) int64 {
			a, b := li(f), ri(f)
			if b == 0 {
				f.fail(ErrDivideByZero)
				return 0
			}
			return a % b
		}
	}
	return code{t: t, i: fn}
}

// between is inclusive on both ends. When lo > hi the range wraps around,
// so "hour(t) between 20 and 5" means 20:00 through 05:59.
func (g *gen) between(n *betweenNode) code {
	x, lo, hi := g.gen(n.x), g.gen(n.lo), g.gen(n.hi)
	xi := x.i
	if lo.isConst && hi.isConst {
		a, b := lo.cval, hi.cval
		if a <= b {
			return code{t: Bool, b: func(f *Frame) bool { v := xi(f); return v >= a && v <= b }}
		}
		return code{t: Bool, b: func(f *Frame) bool { v := xi(f); return v >= a || v <= b }}
	}
	loi, hii := lo.i, hi.i
	return code{t: Bool, b: func(f *Frame) bool {
		v, a, b := xi(f), loi(f), hii(f)
		if a <= b {
			return v >= a && v <= b
		}
		return v >= a || v <= b
	}}
}

func (g *gen) in(n *inNode) code {
	x, list := g.gen(n.x), g.gen(n.list)
	not := n.not
	var fn func(*Frame) bool
	switch {
	case x.t == Int:
		xi, set := x.i, list.ints
		fn = func(f *Frame) bool {
			v := xi(f)
			for _, s := range set {
				if s == v {
					return true
				}
			}
			return false
		}
	case len(list.strs) > 8:
		xs := x.s
		set := make(map[string]struct{}, len(list.strs))
		for _, s := range list.strs {
			set[s] = struct{}{}
		}
		fn = func(f *Frame) bool { _, ok := set[xs(f)]; return ok }
	default:
		xs, ls := x.s, list.l
		fn = func(f *Frame) bool {
			v := xs(f)
			for _, s := range ls(f) {
				if s == v {
					return true
				}
			}
			return false
		}
	}
	if not {
		return code{t: Bool, b: func(f *Frame) bool { return !fn(f) }}
	}
	return code{t: Bool, b: fn}
}

func (g *gen) call(n *callNode) code {
	switch n.name {
	case "hour", "weekday", "isNight":
		ti := g.gen(n.args[0]).i
		loc := g.locs[n]
		switch n.name {
		case "hour":
			return code{t: Int, i: func(f *Frame) int64 { return int64(time.Unix(0, ti(f)).In(loc).Hour()) }}
		case "weekday":
			return code{t: Int, i: func(f *Frame) int64 { return int64(time.Unix(0, ti(f)).In(loc).Weekday()) }}
		}
		start, end := g.opts.NightStart, g.opts.NightEnd
		return code{t: Bool, b: func(f *Frame) bool {
			h := time.Unix(0, ti(f)).In(loc).Hour()
			if start <= end {
				return h >= start && h < end
			}
			return h >= start || h < end
		}}
	case "age":
		ti := g.gen(n.args[0]).i
		return code{t: Duration, i: func(f *Frame) int64 { return f.env.Now() - ti(f) }}
	case "len":
		l := g.gen(n.args[0]).l
		return code{t: Int, i: func(f *Frame) int64 { return int64(len(l(f))) }}
	}
	a := g.aggs[n]
	dup := false
	for _, b := range g.aggList {
		dup = dup || b == a
	}
	if !dup {
		g.aggList = append(g.aggList, a)
	}
	return code{t: Int, i: func(f *Frame) int64 {
		v, err := f.env.Aggregate(a)
		if err != nil {
			f.fail(err)
			return 0
		}
		return v
	}}
}
