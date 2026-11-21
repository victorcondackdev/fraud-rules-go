package expr

import (
	"sync"
	"time"
	_ "time/tzdata" // hour() and friends must not depend on the host's zoneinfo
)

// Agg is a velocity aggregate function.
type Agg uint8

// Aggregates over a velocity stream within a trailing window.
const (
	AggCount    Agg = iota + 1 // number of events
	AggSum                     // sum of event amounts (cents)
	AggDistinct                // number of distinct counterparties
)

var aggNames = [...]string{AggCount: "count", AggSum: "sum", AggDistinct: "distinct"}

// String returns the function name: "count", "sum" or "distinct".
func (a Agg) String() string {
	if a == 0 || int(a) >= len(aggNames) {
		return "agg?"
	}
	return aggNames[a]
}

// Aggregate is one velocity lookup an expression performs.
type Aggregate struct {
	Op       Agg
	Stream   string // stream field name, e.g. "payer.transfers"
	StreamID int
	Window   time.Duration
}

// String returns the call as written canonically, e.g.
// "count(payer.transfers, 10m)". It is also the key [MapEnv] looks up.
func (a Aggregate) String() string {
	return a.Op.String() + "(" + a.Stream + ", " + FormatDuration(a.Window) + ")"
}

// builtin describes a function's signature and static cost.
type builtin struct {
	name     string
	minArgs  int
	maxArgs  int
	cost     int
	argTypes []Type // per position; trailing optional args use the last entries
	result   Type
	agg      Agg
}

var builtins = map[string]builtin{
	"hour":     {name: "hour", minArgs: 1, maxArgs: 2, cost: 4, argTypes: []Type{Time, String}, result: Int},
	"weekday":  {name: "weekday", minArgs: 1, maxArgs: 2, cost: 4, argTypes: []Type{Time, String}, result: Int},
	"isNight":  {name: "isNight", minArgs: 1, maxArgs: 2, cost: 4, argTypes: []Type{Time, String}, result: Bool},
	"age":      {name: "age", minArgs: 1, maxArgs: 1, cost: 2, argTypes: []Type{Time}, result: Duration},
	"len":      {name: "len", minArgs: 1, maxArgs: 1, cost: 2, argTypes: []Type{StringList}, result: Int},
	"count":    {name: "count", minArgs: 2, maxArgs: 2, cost: 25, argTypes: []Type{Stream, Duration}, result: Int, agg: AggCount},
	"sum":      {name: "sum", minArgs: 2, maxArgs: 2, cost: 25, argTypes: []Type{Stream, Duration}, result: Int, agg: AggSum},
	"distinct": {name: "distinct", minArgs: 2, maxArgs: 2, cost: 40, argTypes: []Type{Stream, Duration}, result: Int, agg: AggDistinct},
}

var locCache sync.Map // string -> *time.Location

func loadLocation(name string) (*time.Location, error) {
	if l, ok := locCache.Load(name); ok {
		return l.(*time.Location), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	locCache.Store(name, loc)
	return loc, nil
}

// checker assigns a type to every node, resolves fields and time zones,
// and accumulates the static cost.
type checker struct {
	src    string
	schema *Schema
	opts   *Options
	cost   int
	locs   map[*callNode]*time.Location
	aggs   map[*callNode]Aggregate
}

func (c *checker) errf(kind error, n node, format string, args ...any) error {
	s, e := n.span()
	return newError(kind, c.src, s, e, format, args...)
}

func (c *checker) errAt(kind error, start, end int, format string, args ...any) error {
	return newError(kind, c.src, start, end, format, args...)
}

func (c *checker) check(n node) (Type, error) {
	t, err := c.visit(n)
	if err != nil {
		return Invalid, err
	}
	n.setType(t)
	return t, nil
}

func (c *checker) visit(n node) (Type, error) {
	c.cost++
	switch n := n.(type) {
	case *litNode:
		switch n.kind {
		case tTrue, tFalse:
			return Bool, nil
		case tInt:
			return Int, nil
		case tDuration:
			return Duration, nil
		default:
			return String, nil
		}
	case *fieldNode:
		f, ok := c.schema.Lookup(n.path)
		if !ok {
			if s := c.schema.suggest(n.path); s != "" {
				return Invalid, c.errf(ErrUndefined, n, "undefined field %q (did you mean %q?)", n.path, s)
			}
			return Invalid, c.errf(ErrUndefined, n, "undefined field %q", n.path)
		}
		n.field = f
		return f.Type, nil
	case *unaryNode:
		t, err := c.check(n.x)
		if err != nil {
			return Invalid, err
		}
		if n.op == tBang {
			if t != Bool {
				return Invalid, c.errf(ErrType, n, `operator "!" needs bool, got %s`, t)
			}
			return Bool, nil
		}
		if t != Int && t != Duration {
			return Invalid, c.errf(ErrType, n, `operator "-" needs int or duration, got %s`, t)
		}
		return t, nil
	case *binaryNode:
		return c.binary(n)
	case *betweenNode:
		tx, err := c.check(n.x)
		if err != nil {
			return Invalid, err
		}
		tl, err := c.check(n.lo)
		if err != nil {
			return Invalid, err
		}
		th, err := c.check(n.hi)
		if err != nil {
			return Invalid, err
		}
		if !tx.ordered() {
			return Invalid, c.errf(ErrType, n.x, `"between" needs int, duration or time, got %s`, tx)
		}
		if tl != tx {
			return Invalid, c.errf(ErrType, n.lo, "lower bound is %s, want %s", tl, tx)
		}
		if th != tx {
			return Invalid, c.errf(ErrType, n.hi, "upper bound is %s, want %s", th, tx)
		}
		return Bool, nil
	case *inNode:
		return c.in(n)
	case *listNode:
		return c.list(n)
	case *callNode:
		return c.call(n)
	}
	return Invalid, c.errf(ErrSyntax, n, "unsupported expression")
}

func (c *checker) binary(n *binaryNode) (Type, error) {
	lt, err := c.check(n.l)
	if err != nil {
		return Invalid, err
	}
	rt, err := c.check(n.r)
	if err != nil {
		return Invalid, err
	}
	mismatch := func() (Type, error) {
		return Invalid, c.errAt(ErrType, n.opPos, n.opPos+len(n.op.String()),
			"operator %q not defined for %s and %s", n.op.String(), lt, rt)
	}
	switch n.op {
	case tAndAnd, tOrOr:
		if lt != Bool {
			return Invalid, c.errf(ErrType, n.l, "left operand of %q is %s, want bool", n.op.String(), lt)
		}
		if rt != Bool {
			return Invalid, c.errf(ErrType, n.r, "right operand of %q is %s, want bool", n.op.String(), rt)
		}
		return Bool, nil
	case tEq, tNe:
		if lt != rt || !lt.scalar() {
			return mismatch()
		}
		return Bool, nil
	case tLt, tLe, tGt, tGe:
		if lt != rt || !lt.ordered() {
			return mismatch()
		}
		return Bool, nil
	case tStartsWith, tEndsWith, tContains:
		c.cost += 3
		if lt != String || rt != String {
			return mismatch()
		}
		return Bool, nil
	case tPlus:
		switch {
		case lt == Int && rt == Int:
			return Int, nil
		case lt == Duration && rt == Duration:
			return Duration, nil
		case lt == Time && rt == Duration, lt == Duration && rt == Time:
			return Time, nil
		}
	case tMinus:
		switch {
		case lt == Int && rt == Int:
			return Int, nil
		case lt == Duration && rt == Duration, lt == Time && rt == Time:
			return Duration, nil
		case lt == Time && rt == Duration:
			return Time, nil
		}
	case tStar:
		switch {
		case lt == Int && rt == Int:
			return Int, nil
		case lt == Duration && rt == Int, lt == Int && rt == Duration:
			return Duration, nil
		}
	case tSlash:
		switch {
		case lt == Int && rt == Int:
			return Int, nil
		case lt == Duration && rt == Int:
			return Duration, nil
		}
	case tPercent:
		if lt == Int && rt == Int {
			return Int, nil
		}
	}
	return mismatch()
}

func (c *checker) in(n *inNode) (Type, error) {
	xt, err := c.check(n.x)
	if err != nil {
		return Invalid, err
	}
	lt, err := c.check(n.list)
	if err != nil {
		return Invalid, err
	}
	if ln, ok := n.list.(*listNode); ok && len(ln.elems) == 0 {
		if xt != String && xt != Int {
			return Invalid, c.errf(ErrType, n.x, `"in" needs a string or int on the left, got %s`, xt)
		}
		lt = map[Type]Type{String: StringList, Int: IntList}[xt]
		n.list.setType(lt)
	}
	switch {
	case xt == String && lt == StringList, xt == Int && lt == IntList:
	case lt != StringList && lt != IntList:
		return Invalid, c.errf(ErrType, n.list, `right operand of "in" is %s, want a list`, lt)
	default:
		return Invalid, c.errf(ErrType, n.x, "cannot look up %s in %s", xt, lt)
	}
	if _, ok := n.list.(*fieldNode); ok {
		c.cost += 8 // linear scan of a runtime list
	}
	return Bool, nil
}

func (c *checker) list(n *listNode) (Type, error) {
	var elem Type
	for _, e := range n.elems {
		t, err := c.check(e)
		if err != nil {
			return Invalid, err
		}
		if _, ok := constInt(e); !ok {
			if l, ok := e.(*litNode); !ok || l.kind != tString {
				return Invalid, c.errf(ErrType, e, "list elements must be string or integer literals")
			}
		}
		if elem == Invalid {
			elem = t
		} else if t != elem {
			return Invalid, c.errf(ErrType, e, "list mixes %s and %s", elem, t)
		}
	}
	if elem == Int {
		return IntList, nil
	}
	return StringList, nil
}

// constInt reports the value of an integer literal, possibly negated.
func constInt(n node) (int64, bool) {
	switch n := n.(type) {
	case *litNode:
		if n.kind == tInt {
			return n.ival, true
		}
	case *unaryNode:
		if n.op == tMinus {
			if v, ok := constInt(n.x); ok {
				return -v, true
			}
		}
	}
	return 0, false
}

func (c *checker) call(n *callNode) (Type, error) {
	b, ok := builtins[n.name]
	if !ok {
		return Invalid, c.errAt(ErrUndefined, n.start, n.nameEnd, "undefined function %q", n.name)
	}
	c.cost += b.cost
	if len(n.args) < b.minArgs || len(n.args) > b.maxArgs {
		want := itoa(b.minArgs)
		if b.maxArgs != b.minArgs {
			want += " or " + itoa(b.maxArgs)
		}
		return Invalid, c.errf(ErrType, n, "%s() takes %s arguments, got %d", n.name, want, len(n.args))
	}
	for i, a := range n.args {
		t, err := c.check(a)
		if err != nil {
			return Invalid, err
		}
		if want := b.argTypes[i]; t != want {
			return Invalid, c.errf(ErrType, a, "argument %d of %s() is %s, want %s", i+1, n.name, t, want)
		}
	}
	switch n.name {
	case "hour", "weekday", "isNight":
		tz := c.opts.DefaultTimeZone
		if len(n.args) == 2 {
			lit, ok := n.args[1].(*litNode)
			if !ok {
				return Invalid, c.errf(ErrType, n.args[1], "time zone must be a string literal")
			}
			tz = lit.sval
		}
		loc, err := loadLocation(tz)
		if err != nil {
			at := node(n)
			if len(n.args) == 2 {
				at = n.args[1]
			}
			return Invalid, c.errf(ErrType, at, "unknown time zone %q", tz)
		}
		c.locs[n] = loc
	case "count", "sum", "distinct":
		lit, ok := n.args[1].(*litNode)
		if !ok {
			return Invalid, c.errf(ErrType, n.args[1], "window must be a duration literal such as 10m")
		}
		if lit.ival <= 0 {
			return Invalid, c.errf(ErrType, lit, "window must be positive")
		}
		if c.opts.MaxWindow > 0 && time.Duration(lit.ival) > c.opts.MaxWindow {
			return Invalid, c.errf(ErrType, lit, "window %s exceeds the feature store horizon %s",
				FormatDuration(time.Duration(lit.ival)), FormatDuration(c.opts.MaxWindow))
		}
		f := n.args[0].(*fieldNode) // only fields can have type Stream
		c.aggs[n] = Aggregate{Op: b.agg, Stream: f.path, StreamID: f.field.ID, Window: time.Duration(lit.ival)}
	}
	return b.result, nil
}

func itoa(n int) string {
	if n < 10 && n >= 0 {
		return string(rune('0' + n))
	}
	return "many"
}
