package expr

import (
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

type tokKind uint8

const (
	tEOF tokKind = iota
	tIdent
	tInt
	tDuration
	tString
	tLParen
	tRParen
	tLBrack
	tRBrack
	tComma
	tDot
	tPlus
	tMinus
	tStar
	tSlash
	tPercent
	tEq
	tNe
	tLt
	tLe
	tGt
	tGe
	tAndAnd
	tOrOr
	tBang
	// keywords
	tTrue
	tFalse
	tIn
	tNot
	tBetween
	tAnd
	tStartsWith
	tEndsWith
	tContains
)

var tokNames = [...]string{
	tEOF: "end of input", tIdent: "identifier", tInt: "integer",
	tDuration: "duration", tString: "string", tLParen: "(", tRParen: ")",
	tLBrack: "[", tRBrack: "]", tComma: ",", tDot: ".", tPlus: "+",
	tMinus: "-", tStar: "*", tSlash: "/", tPercent: "%", tEq: "==",
	tNe: "!=", tLt: "<", tLe: "<=", tGt: ">", tGe: ">=", tAndAnd: "&&",
	tOrOr: "||", tBang: "!", tTrue: "true", tFalse: "false", tIn: "in",
	tNot: "not", tBetween: "between", tAnd: "and", tStartsWith: "startsWith",
	tEndsWith: "endsWith", tContains: "contains",
}

func (k tokKind) String() string {
	if int(k) < len(tokNames) {
		return tokNames[k]
	}
	return "?"
}

var keywords = map[string]tokKind{
	"true": tTrue, "false": tFalse, "in": tIn, "not": tNot,
	"between": tBetween, "and": tAnd, "startsWith": tStartsWith,
	"endsWith": tEndsWith, "contains": tContains,
}

var twoCharTokens = map[string]tokKind{
	"==": tEq, "!=": tNe, "<=": tLe, ">=": tGe, "&&": tAndAnd, "||": tOrOr,
}

var oneCharTokens = map[byte]tokKind{
	'(': tLParen, ')': tRParen, '[': tLBrack, ']': tRBrack, ',': tComma,
	'.': tDot, '+': tPlus, '-': tMinus, '*': tStar, '/': tSlash,
	'%': tPercent, '<': tLt, '>': tGt, '!': tBang,
}

type token struct {
	kind tokKind
	pos  int    // byte offset of the first character
	end  int    // byte offset just past the token
	text string // identifier name or decoded string literal
	ival int64  // integer value, or duration in nanoseconds
}

// lex splits src into tokens, always ending with tEOF.
func lex(src string) ([]token, error) {
	l := lexer{src: src}
	toks := make([]token, 0, len(src)/3+1)
	for {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, t)
		if t.kind == tEOF {
			return toks, nil
		}
	}
}

type lexer struct {
	src string
	i   int
}

func (l *lexer) errf(start, end int, format string, args ...any) error {
	return newError(ErrSyntax, l.src, start, end, format, args...)
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' }
func isDigit(c byte) bool  { return c >= '0' && c <= '9' }

func (l *lexer) next() (token, error) {
	src := l.src
	for l.i < len(src) {
		c := src[l.i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			l.i++
			continue
		}
		if c == '#' { // comment to end of line
			for l.i < len(src) && src[l.i] != '\n' {
				l.i++
			}
			continue
		}
		break
	}
	start := l.i
	if l.i >= len(src) {
		return token{kind: tEOF, pos: start, end: start}, nil
	}
	c := src[l.i]
	switch {
	case isLetter(c):
		for l.i < len(src) && (isLetter(src[l.i]) || isDigit(src[l.i])) {
			l.i++
		}
		word := src[start:l.i]
		if k, ok := keywords[word]; ok {
			return token{kind: k, pos: start, end: l.i, text: word}, nil
		}
		return token{kind: tIdent, pos: start, end: l.i, text: word}, nil
	case isDigit(c):
		return l.number()
	case c == '"':
		return l.str()
	}
	two := ""
	if l.i+1 < len(src) {
		two = src[l.i : l.i+2]
	}
	if k, ok := twoCharTokens[two]; ok {
		l.i += 2
		return token{kind: k, pos: start, end: l.i}, nil
	}
	if k, ok := oneCharTokens[c]; ok {
		l.i++
		return token{kind: k, pos: start, end: l.i}, nil
	}
	switch c {
	case '&':
		return token{}, l.errf(start, start+1, `unexpected "&" (did you mean "&&"?)`)
	case '|':
		return token{}, l.errf(start, start+1, `unexpected "|" (did you mean "||"?)`)
	case '=':
		return token{}, l.errf(start, start+1, `unexpected "=" (did you mean "=="?)`)
	case '\'':
		return token{}, l.errf(start, start+1, `strings use double quotes`)
	}
	r, size := utf8.DecodeRuneInString(src[l.i:])
	if r == utf8.RuneError && size <= 1 {
		return token{}, l.errf(start, start+1, "invalid UTF-8 byte 0x%02x", c)
	}
	return token{}, l.errf(start, start+size, "unexpected character %q", r)
}

// digits scans [0-9_]+ starting at l.i and returns the value. Underscores
// are separators and must sit between two digits.
func (l *lexer) digits() (int64, error) {
	start := l.i
	var v int64
	overflow := false
	prevUnderscore := false
	for l.i < len(l.src) {
		c := l.src[l.i]
		if c == '_' {
			if prevUnderscore || l.i == start {
				return 0, l.errf(l.i, l.i+1, "misplaced \"_\" in number")
			}
			prevUnderscore = true
			l.i++
			continue
		}
		if !isDigit(c) {
			break
		}
		prevUnderscore = false
		d := int64(c - '0')
		if v > (math.MaxInt64-d)/10 {
			overflow = true
		} else {
			v = v*10 + d
		}
		l.i++
	}
	if prevUnderscore {
		return 0, l.errf(l.i-1, l.i, "misplaced \"_\" in number")
	}
	if overflow {
		return 0, l.errf(start, l.i, "integer %s overflows int64", l.src[start:l.i])
	}
	return v, nil
}

var durationUnits = []struct {
	name string
	d    time.Duration
}{
	// "ms" must be tried before "m".
	{"ms", time.Millisecond}, {"s", time.Second}, {"m", time.Minute},
	{"h", time.Hour}, {"d", 24 * time.Hour},
}

func (l *lexer) number() (token, error) {
	start := l.i
	v, err := l.digits()
	if err != nil {
		return token{}, err
	}
	if l.i < len(l.src) && l.src[l.i] == '.' && l.i+1 < len(l.src) && isDigit(l.src[l.i+1]) {
		return token{}, l.errf(start, l.i+2, "fractional numbers are not supported; use integer cents")
	}
	if l.i >= len(l.src) || !isLetter(l.src[l.i]) {
		return token{kind: tInt, pos: start, end: l.i, ival: v}, nil
	}
	// Duration literal: one or more <digits><unit> groups, e.g. 1h30m.
	var total int64
	for {
		unitStart := l.i
		var unit time.Duration
		for _, u := range durationUnits {
			if strings.HasPrefix(l.src[l.i:], u.name) {
				unit = u.d
				l.i += len(u.name)
				break
			}
		}
		if unit == 0 || (l.i < len(l.src) && isLetter(l.src[l.i])) {
			end := l.i
			for end < len(l.src) && (isLetter(l.src[end]) || isDigit(l.src[end])) {
				end++
			}
			return token{}, l.errf(unitStart, end, "invalid duration unit %q (want ms, s, m, h or d)", l.src[unitStart:end])
		}
		if v > math.MaxInt64/int64(unit) || total > math.MaxInt64-v*int64(unit) {
			return token{}, l.errf(start, l.i, "duration %s overflows", l.src[start:l.i])
		}
		total += v * int64(unit)
		if l.i >= len(l.src) || !isDigit(l.src[l.i]) {
			break
		}
		if v, err = l.digits(); err != nil {
			return token{}, err
		}
		if l.i >= len(l.src) || !isLetter(l.src[l.i]) {
			return token{}, l.errf(start, l.i, "duration %s is missing a unit", l.src[start:l.i])
		}
	}
	return token{kind: tDuration, pos: start, end: l.i, ival: total}, nil
}

func (l *lexer) str() (token, error) {
	start := l.i
	l.i++ // opening quote
	var b strings.Builder
	for l.i < len(l.src) {
		c := l.src[l.i]
		switch c {
		case '"':
			l.i++
			return token{kind: tString, pos: start, end: l.i, text: b.String()}, nil
		case '\n':
			return token{}, l.errf(start, l.i, "unterminated string")
		case '\\':
			if l.i+1 >= len(l.src) {
				return token{}, l.errf(start, l.i+1, "unterminated string")
			}
			esc := l.src[l.i+1]
			switch esc {
			case '"', '\\':
				b.WriteByte(esc)
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				return token{}, l.errf(l.i, l.i+2, "unknown escape \\%c", esc)
			}
			l.i += 2
		default:
			b.WriteByte(c)
			l.i++
		}
	}
	return token{}, l.errf(start, l.i, "unterminated string")
}

// ParseDuration parses a duration in the literal syntax of the language:
// one or more <integer><unit> groups with units ms, s, m, h and d, such as
// "500ms", "10m", "1h30m" or "7d". A leading "-" is accepted.
func ParseDuration(s string) (time.Duration, error) {
	neg := strings.HasPrefix(s, "-")
	body := strings.TrimPrefix(s, "-")
	if body == "" || !isDigit(body[0]) {
		return 0, newError(ErrSyntax, s, 0, len(s), "invalid duration %q", s)
	}
	l := lexer{src: body}
	t, err := l.number()
	if err != nil {
		return 0, err
	}
	if l.i != len(body) {
		return 0, newError(ErrSyntax, s, 0, len(s), "invalid duration %q", s)
	}
	if t.kind == tInt {
		if t.ival != 0 {
			return 0, newError(ErrSyntax, s, 0, len(s), "duration %q is missing a unit", s)
		}
	}
	if neg {
		return -time.Duration(t.ival), nil
	}
	return time.Duration(t.ival), nil
}
