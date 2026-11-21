// Package expr implements the rule language: a small, typed, side-effect
// free expression language compiled once into Go closures and evaluated in
// tens of nanoseconds.
//
//	tx.amount > 1000_00 && hour(tx.time, "America/Sao_Paulo") between 20 and 5 && !payer.device.trusted
//	count(payer.transfers, 10m) >= 5 || sum(payer.transfers, 1h) > 5000_00
//
// # Pipeline
//
// A hand-written lexer feeds a Pratt parser that builds an AST. The checker
// assigns a static type to every node, resolves field paths against a
// [Schema], loads time zones and validates aggregate windows, and computes
// a static cost. The code generator then turns the typed tree into a tree
// of closures specialized by type (func(*Frame) int64, func(*Frame) bool,
// ...), with constant operands folded into the closures. Evaluation does
// not allocate.
//
// # Types
//
//	bool          true, false
//	int           1500, 1_000_00 (amounts are integer cents; no floats)
//	string        "PF" (escapes: \" \\ \n \t)
//	duration      500ms, 30s, 10m, 1h30m, 7d
//	time          fields only; nanoseconds since the Unix epoch
//	list<string>  ["PF", "PJ"] or a list field
//	list<int>     [0, 6], literals only
//	stream        velocity stream fields, usable only in count/sum/distinct
//
// # Operators
//
// From lowest to highest precedence; all binary operators are
// left-associative:
//
//	||                                   bool
//	&&                                   bool
//	== != < <= > >=                      == and != on any scalar; ordering on int, duration, time
//	x between lo and hi                  inclusive; wraps around when lo > hi
//	x in list, x not in list             string in list<string>, int in list<int>
//	a startsWith b, endsWith, contains   strings
//	+ -                                  int±int, duration±duration, time±duration, time-time
//	* / %                                int; duration*int, duration/int
//	! -                                  unary not, negation
//
// && and || short-circuit. Integer overflow wraps, as in Go. Division or
// remainder by zero is an evaluation error ([ErrDivideByZero]), never a
// panic.
//
// "between" is designed for clock arithmetic: hour(t) between 20 and 5
// is true from 20:00 through 05:59.
//
// # Builtins
//
//	hour(t [, "Zone"])      int 0-23 in the zone (default America/Sao_Paulo)
//	weekday(t [, "Zone"])   int 0-6, Sunday = 0
//	isNight(t [, "Zone"])   bool, hour >= 20 || hour < 6 by default
//	age(t)                  duration since t, relative to the evaluation instant
//	len(list)               int
//	count(stream, window)   events in the trailing window
//	sum(stream, window)     sum of amounts (cents) in the window
//	distinct(stream, window) distinct counterparties in the window
//
// Zone names and windows must be literals so they can be validated at
// compile time and so the engine knows every velocity window a ruleset
// will read before it serves traffic.
//
// # Guarantees
//
//   - No loops, recursion or user-defined functions: evaluation time is
//     linear in the size of the expression.
//   - Bounded cost: sources over 8 KiB, nesting over 64 levels and trees
//     whose static cost exceeds [Options.MaxCost] are rejected at compile
//     time with [ErrTooComplex]. Aggregates are weighted heavily so a rule
//     cannot hide hundreds of feature-store lookups.
//   - No panics: the lexer, parser, checker and evaluator are fuzzed.
//   - Determinism: evaluation reads only the [Env]; "now" is the
//     transaction time, not the wall clock, and time zones come from the
//     embedded tzdata, so replays and backtests are exact.
//   - Positional errors: every compile error is an [*Error] with line and
//     column, and [Error.Snippet] renders a caret under the offending span.
package expr
