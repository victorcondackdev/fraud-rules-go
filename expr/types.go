package expr

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Type is the static type of an expression or of a schema field.
type Type uint8

// Types understood by the checker. Money is represented as [Int] holding
// cents; there are no floating point values anywhere in the language.
const (
	Invalid    Type = iota
	Bool            // true / false
	Int             // signed 64-bit integer (amounts are cents)
	String          // UTF-8 string
	Duration        // signed 64-bit nanoseconds, literal 10m, 1h30m, 7d
	Time            // instant, nanoseconds since the Unix epoch
	StringList      // list of strings, from a field or a literal
	IntList         // list of integers, literal only
	Stream          // velocity stream handle, usable only in count/sum/distinct
)

var typeNames = [...]string{
	Invalid:    "invalid",
	Bool:       "bool",
	Int:        "int",
	String:     "string",
	Duration:   "duration",
	Time:       "time",
	StringList: "list<string>",
	IntList:    "list<int>",
	Stream:     "stream",
}

// String returns the type name used in error messages, e.g. "duration".
func (t Type) String() string {
	if int(t) >= len(typeNames) {
		return typeNames[Invalid]
	}
	return typeNames[t]
}

// ordered reports whether values of t support <, <=, >, >= and between.
func (t Type) ordered() bool { return t == Int || t == Duration || t == Time }

// scalar reports whether values of t support == and !=.
func (t Type) scalar() bool {
	return t == Bool || t == Int || t == String || t == Duration || t == Time
}

// Field describes a variable an expression may reference.
type Field struct {
	// Name is the dotted path, e.g. "payer.device.trusted".
	Name string
	// Type is the static type; [Stream] fields are velocity streams.
	Type Type
	// ID is the dense index passed to [Env] accessors.
	ID int
}

// Schema is the set of fields an expression may reference. It is built once
// and shared read-only by every compiled [Program]; adding fields after
// compiling programs against it is not safe for concurrent use.
type Schema struct {
	byName map[string]Field
	fields []Field
}

// NewSchema returns an empty schema.
func NewSchema() *Schema { return &Schema{byName: make(map[string]Field)} }

// Define adds a field and returns its ID. Defining an existing name with
// the same type returns the existing ID; with a different type it panics,
// because schemas are static program configuration.
func (s *Schema) Define(name string, t Type) int {
	if f, ok := s.byName[name]; ok {
		if f.Type != t {
			panic(fmt.Sprintf("expr: field %q redefined as %s (was %s)", name, t, f.Type))
		}
		return f.ID
	}
	if t == Invalid || t == IntList || int(t) >= len(typeNames) {
		panic(fmt.Sprintf("expr: field %q has unsupported type %s", name, t))
	}
	f := Field{Name: name, Type: t, ID: len(s.fields)}
	s.byName[name] = f
	s.fields = append(s.fields, f)
	return f.ID
}

// Lookup returns the field with the given dotted name.
func (s *Schema) Lookup(name string) (Field, bool) {
	f, ok := s.byName[name]
	return f, ok
}

// Fields returns every field in definition order.
func (s *Schema) Fields() []Field { return append([]Field(nil), s.fields...) }

// Name returns the name of the field with the given ID, or "" if unknown.
func (s *Schema) Name(id int) string {
	if id < 0 || id >= len(s.fields) {
		return ""
	}
	return s.fields[id].Name
}

// suggest returns the defined name closest to name, for "did you mean"
// hints, or "" when nothing is reasonably close.
func (s *Schema) suggest(name string) string {
	best, bestD := "", 4
	names := make([]string, 0, len(s.fields))
	for _, f := range s.fields {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	for _, n := range names {
		if d := editDistance(name, n, bestD); d < bestD {
			best, bestD = n, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance between a and b, or limit if it
// is at least limit.
func editDistance(a, b string, limit int) int {
	if d := len(a) - len(b); d >= limit || -d >= limit {
		return limit
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			rowMin = min(rowMin, cur[j])
		}
		if rowMin >= limit {
			return limit
		}
		prev, cur = cur, prev
	}
	return min(prev[len(b)], limit)
}

// Value is a dynamically typed result, used by [Program.Eval] and by
// explanations. Only the member matching Type is meaningful.
type Value struct {
	Type Type
	Int  int64 // Int, Duration (ns) and Time (Unix ns)
	Str  string
	Bool bool
	List []string // StringList
	Ints []int64  // IntList
}

// String renders the value the way it would be written in an expression:
// 150000, "abc", true, 10m, 2026-11-21T23:10:00Z, ["a", "b"].
func (v Value) String() string {
	switch v.Type {
	case Bool:
		return strconv.FormatBool(v.Bool)
	case Int:
		return strconv.FormatInt(v.Int, 10)
	case String:
		return strconv.Quote(v.Str)
	case Duration:
		return FormatDuration(time.Duration(v.Int))
	case Time:
		return time.Unix(0, v.Int).UTC().Format(time.RFC3339)
	case StringList:
		q := make([]string, len(v.List))
		for i, s := range v.List {
			q[i] = strconv.Quote(s)
		}
		return "[" + strings.Join(q, ", ") + "]"
	case IntList:
		q := make([]string, len(v.Ints))
		for i, n := range v.Ints {
			q[i] = strconv.FormatInt(n, 10)
		}
		return "[" + strings.Join(q, ", ") + "]"
	}
	return "<invalid>"
}

// FormatDuration writes d in the literal syntax accepted by the language,
// using the largest exact units: 10m, 1h30m, 7d, 1500ms, 0s.
func FormatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	var b strings.Builder
	u := uint64(d)
	if d < 0 {
		b.WriteByte('-')
		u = -u
	}
	units := [...]struct {
		n    uint64
		name string
	}{
		{uint64(24 * time.Hour), "d"},
		{uint64(time.Hour), "h"},
		{uint64(time.Minute), "m"},
		{uint64(time.Second), "s"},
		{uint64(time.Millisecond), "ms"},
	}
	for _, un := range units {
		if u >= un.n {
			b.WriteString(strconv.FormatUint(u/un.n, 10))
			b.WriteString(un.name)
			u %= un.n
		}
	}
	if u != 0 {
		// Sub-millisecond remainder: the literal syntax cannot express it,
		// so fall back to the standard library spelling.
		b.WriteString("+")
		b.WriteString(time.Duration(u).String())
	}
	return b.String()
}
