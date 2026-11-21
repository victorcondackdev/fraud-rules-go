package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/victorcondackdev/fraud-rules-go/expr"
)

// Action is what a rule, or the final decision, asks the payment flow to
// do. Actions are ordered by severity; the decision takes the most severe
// action among the enforced rules that matched.
type Action uint8

// Actions from least to most severe.
const (
	Allow  Action = iota // release the payment
	Review               // release, and queue for analyst review
	StepUp               // require additional authentication before releasing
	Hold                 // accept but delay settlement (cautionary hold)
	Block                // reject
)

var actionNames = [...]string{Allow: "ALLOW", Review: "REVIEW", StepUp: "STEP_UP", Hold: "HOLD", Block: "BLOCK"}

// String returns the wire name: "ALLOW", "REVIEW", "STEP_UP", "HOLD" or
// "BLOCK".
func (a Action) String() string {
	if int(a) >= len(actionNames) {
		return fmt.Sprintf("Action(%d)", uint8(a))
	}
	return actionNames[a]
}

// ParseAction parses a wire name, case-insensitively.
func ParseAction(s string) (Action, error) {
	for i, n := range actionNames {
		if strings.EqualFold(s, n) {
			return Action(i), nil
		}
	}
	return 0, fmt.Errorf("engine: unknown action %q (want ALLOW, REVIEW, STEP_UP, HOLD or BLOCK)", s)
}

// MarshalText implements [encoding.TextMarshaler].
func (a Action) MarshalText() ([]byte, error) {
	if int(a) >= len(actionNames) {
		return nil, fmt.Errorf("engine: invalid action %d", uint8(a))
	}
	return []byte(a.String()), nil
}

// UnmarshalText implements [encoding.TextUnmarshaler].
func (a *Action) UnmarshalText(b []byte) error {
	v, err := ParseAction(string(b))
	if err == nil {
		*a = v
	}
	return err
}

// Mode says whether a rule affects decisions.
type Mode uint8

// Rule modes.
const (
	// Enforce rules contribute to the decision.
	Enforce Mode = iota
	// Shadow rules are evaluated and reported in [Decision.Shadow] but
	// never change the decision: the safe way to trial a rule on live
	// traffic before promoting it.
	Shadow
)

// String returns "enforce" or "shadow".
func (m Mode) String() string {
	if m == Shadow {
		return "shadow"
	}
	return "enforce"
}

// MarshalText implements [encoding.TextMarshaler].
func (m Mode) MarshalText() ([]byte, error) { return []byte(m.String()), nil }

// UnmarshalText implements [encoding.TextUnmarshaler]; empty means enforce.
func (m *Mode) UnmarshalText(b []byte) error {
	switch strings.ToLower(string(b)) {
	case "", "enforce":
		*m = Enforce
	case "shadow":
		*m = Shadow
	default:
		return fmt.Errorf("engine: unknown mode %q (want enforce or shadow)", b)
	}
	return nil
}

// FailMode says how a rule that could not be evaluated (deadline exceeded,
// feature store error, division by zero) is treated.
type FailMode uint8

// Fail modes.
const (
	// FailUnset defers to the ruleset default for the rule's action, then
	// to FailOpen.
	FailUnset FailMode = iota
	// FailOpen treats the rule as not matched: availability wins.
	FailOpen
	// FailClosed treats the rule as matched: safety wins.
	FailClosed
)

// String returns "open", "closed" or "".
func (f FailMode) String() string {
	switch f {
	case FailOpen:
		return "open"
	case FailClosed:
		return "closed"
	}
	return ""
}

// MarshalText implements [encoding.TextMarshaler].
func (f FailMode) MarshalText() ([]byte, error) { return []byte(f.String()), nil }

// UnmarshalText implements [encoding.TextUnmarshaler].
func (f *FailMode) UnmarshalText(b []byte) error {
	switch strings.ToLower(string(b)) {
	case "":
		*f = FailUnset
	case "open":
		*f = FailOpen
	case "closed":
		*f = FailClosed
	default:
		return fmt.Errorf("engine: unknown fail mode %q (want open or closed)", b)
	}
	return nil
}

// Duration is a [time.Duration] that encodes in JSON as a string in the
// rule language's literal syntax ("30m", "1h30m", "3d").
type Duration time.Duration

// MarshalJSON implements [json.Marshaler].
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(expr.FormatDuration(time.Duration(d)))
}

// UnmarshalJSON implements [json.Unmarshaler]. It accepts a string such as
// "30m" or a JSON number of nanoseconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n int64
		if err2 := json.Unmarshal(b, &n); err2 != nil {
			return fmt.Errorf("engine: duration must be a string like \"30m\": %w", err)
		}
		*d = Duration(n)
		return nil
	}
	v, err := expr.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("engine: invalid duration %q", s)
	}
	*d = Duration(v)
	return nil
}

// String formats d like the rule language does.
func (d Duration) String() string { return expr.FormatDuration(time.Duration(d)) }
