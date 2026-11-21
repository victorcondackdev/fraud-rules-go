package expr

import (
	"fmt"
	"time"
)

// MapEnv is an [Env] backed by maps, convenient for tests, examples and
// tooling. Production callers implement Env over their own structs, which
// avoids the map lookups.
//
// Vars holds field values by dotted name: int64 or int for Int, bool,
// string, time.Duration, time.Time and []string. Missing or mistyped
// values read as the zero value. Aggregates holds velocity results keyed by
// [Aggregate.String], e.g. "count(payer.transfers, 10m)"; missing keys read
// as 0.
type MapEnv struct {
	Schema     *Schema
	Vars       map[string]any
	Aggregates map[string]int64
	At         time.Time // evaluation instant for age()
}

var _ Env = (*MapEnv)(nil)

func (m *MapEnv) get(id int) any { return m.Vars[m.Schema.Name(id)] }

// Int implements [Env].
func (m *MapEnv) Int(id int) int64 {
	switch v := m.get(id).(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case time.Duration:
		return int64(v)
	case time.Time:
		return unixNano(v)
	}
	return 0
}

// Bool implements [Env].
func (m *MapEnv) Bool(id int) bool { v, _ := m.get(id).(bool); return v }

// String implements [Env].
func (m *MapEnv) String(id int) string { v, _ := m.get(id).(string); return v }

// List implements [Env].
func (m *MapEnv) List(id int) []string { v, _ := m.get(id).([]string); return v }

// Now implements [Env].
func (m *MapEnv) Now() int64 { return unixNano(m.At) }

// Aggregate implements [Env].
func (m *MapEnv) Aggregate(a Aggregate) (int64, error) {
	v, ok := m.Aggregates[a.String()]
	if !ok && m.Aggregates == nil {
		return 0, nil
	}
	if !ok {
		return 0, fmt.Errorf("expr: no value for %s", a)
	}
	return v, nil
}

// unixNano converts t to Unix nanoseconds, mapping the zero time (and any
// instant outside the int64 range) to 0 so missing timestamps are
// harmless.
func unixNano(t time.Time) int64 {
	if t.IsZero() || t.Year() < 1678 || t.Year() > 2261 {
		return 0
	}
	return t.UnixNano()
}

// UnixNano converts t the way [Env] implementations should: Unix
// nanoseconds, with the zero time mapped to 0.
func UnixNano(t time.Time) int64 { return unixNano(t) }
