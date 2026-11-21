package expr

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Sentinel errors. Compile errors are always an [*Error] that matches one
// of the first four with [errors.Is]; evaluation errors wrap the others.
var (
	// ErrSyntax reports malformed input: a bad token, an unexpected token or
	// an unbalanced bracket.
	ErrSyntax = errors.New("expr: syntax error")
	// ErrType reports a well-formed expression whose operand types do not
	// fit, e.g. adding a duration to a string.
	ErrType = errors.New("expr: type error")
	// ErrUndefined reports a reference to a field or function that does not
	// exist in the schema.
	ErrUndefined = errors.New("expr: undefined name")
	// ErrTooComplex reports an expression over the source size, nesting
	// depth or cost budget.
	ErrTooComplex = errors.New("expr: expression too complex")
	// ErrDivideByZero is returned at evaluation time for x / 0 or x % 0.
	ErrDivideByZero = errors.New("expr: division by zero")
)

// Pos is a position in the source text. Line and Col are 1-based; Col
// counts Unicode code points, so a caret lines up under the character.
type Pos struct {
	Offset int // byte offset
	Line   int
	Col    int
}

// String returns "line:col".
func (p Pos) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Col) }

// position converts a byte offset in src to a Pos.
func position(src string, off int) Pos {
	off = max(0, min(off, len(src)))
	line, col := 1, 1
	for _, r := range src[:off] {
		if r == '\n' {
			line, col = line+1, 1
			continue
		}
		col++
	}
	return Pos{Offset: off, Line: line, Col: col}
}

// Error is a compile error with the source span it refers to.
type Error struct {
	Kind error  // ErrSyntax, ErrType, ErrUndefined or ErrTooComplex
	Pos  Pos    // start of the offending span
	End  int    // byte offset just past the span (>= Pos.Offset)
	Msg  string // human-readable message without position
	Src  string // the full source, for [Error.Snippet]
}

// Error returns "line:col: message".
func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Pos, e.Msg) }

// Unwrap returns the error kind, so errors.Is(err, ErrType) works.
func (e *Error) Unwrap() error { return e.Kind }

// Snippet renders the offending source line with a caret marker under the
// span, the way compilers do:
//
//	1:14: undefined field "tx.amout" (did you mean "tx.amount"?)
//	  tx.amount > tx.amout
//	              ^~~~~~~~
func (e *Error) Snippet() string {
	var b strings.Builder
	b.WriteString(e.Error())
	lineStart := strings.LastIndexByte(e.Src[:e.Pos.Offset], '\n') + 1
	lineEnd := strings.IndexByte(e.Src[lineStart:], '\n')
	if lineEnd < 0 {
		lineEnd = len(e.Src)
	} else {
		lineEnd += lineStart
	}
	line := e.Src[lineStart:lineEnd]
	b.WriteString("\n  ")
	b.WriteString(strings.ReplaceAll(line, "\t", " "))
	b.WriteString("\n  ")
	b.WriteString(strings.Repeat(" ", e.Pos.Col-1))
	b.WriteByte('^')
	end := min(e.End, lineEnd)
	if n := utf8.RuneCountInString(e.Src[e.Pos.Offset:max(end, e.Pos.Offset)]); n > 1 {
		b.WriteString(strings.Repeat("~", n-1))
	}
	return b.String()
}

func newError(kind error, src string, start, end int, format string, args ...any) *Error {
	start = max(0, min(start, len(src)))
	end = max(start, min(end, len(src)))
	return &Error{
		Kind: kind,
		Pos:  position(src, start),
		End:  end,
		Msg:  fmt.Sprintf(format, args...),
		Src:  src,
	}
}
