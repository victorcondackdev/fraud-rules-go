package expr

import (
	"strconv"
	"strings"
	"time"
)

// node is an AST node. Every node records its source span so the checker
// can point at it, and carries the type the checker assigned to it.
type node interface {
	span() (start, end int)
	typ() Type
	setType(Type)
	write(b *strings.Builder)
}

type base struct {
	start, end int
	t          Type
}

func (b *base) span() (int, int) { return b.start, b.end }
func (b *base) typ() Type        { return b.t }
func (b *base) setType(t Type)   { b.t = t }

// litNode is a bool, int, duration or string literal.
type litNode struct {
	base
	kind tokKind // tTrue, tFalse, tInt, tDuration, tString
	ival int64
	sval string
}

// fieldNode is a dotted path such as payer.device.trusted.
type fieldNode struct {
	base
	path  string
	field Field // resolved by the checker
}

type unaryNode struct {
	base
	op tokKind // tBang or tMinus
	x  node
}

type binaryNode struct {
	base
	op    tokKind
	opPos int
	l, r  node
}

type betweenNode struct {
	base
	x, lo, hi node
}

type inNode struct {
	base
	not  bool
	x    node
	list node
}

type listNode struct {
	base
	elems []node
}

type callNode struct {
	base
	name    string
	nameEnd int
	args    []node
}

func (n *litNode) write(b *strings.Builder) {
	switch n.kind {
	case tTrue:
		b.WriteString("true")
	case tFalse:
		b.WriteString("false")
	case tInt:
		b.WriteString(strconv.FormatInt(n.ival, 10))
	case tDuration:
		b.WriteString(FormatDuration(time.Duration(n.ival)))
	case tString:
		b.WriteString(quote(n.sval))
	}
}

func (n *fieldNode) write(b *strings.Builder) { b.WriteString(n.path) }

func (n *unaryNode) write(b *strings.Builder) {
	b.WriteString(n.op.String())
	n.x.write(b)
}

func (n *binaryNode) write(b *strings.Builder) {
	b.WriteByte('(')
	n.l.write(b)
	b.WriteByte(' ')
	b.WriteString(n.op.String())
	b.WriteByte(' ')
	n.r.write(b)
	b.WriteByte(')')
}

func (n *betweenNode) write(b *strings.Builder) {
	b.WriteByte('(')
	n.x.write(b)
	b.WriteString(" between ")
	n.lo.write(b)
	b.WriteString(" and ")
	n.hi.write(b)
	b.WriteByte(')')
}

func (n *inNode) write(b *strings.Builder) {
	b.WriteByte('(')
	n.x.write(b)
	if n.not {
		b.WriteString(" not")
	}
	b.WriteString(" in ")
	n.list.write(b)
	b.WriteByte(')')
}

func (n *listNode) write(b *strings.Builder) {
	b.WriteByte('[')
	for i, e := range n.elems {
		if i > 0 {
			b.WriteString(", ")
		}
		e.write(b)
	}
	b.WriteByte(']')
}

func (n *callNode) write(b *strings.Builder) {
	b.WriteString(n.name)
	b.WriteByte('(')
	for i, a := range n.args {
		if i > 0 {
			b.WriteString(", ")
		}
		a.write(b)
	}
	b.WriteByte(')')
}

// format prints n in canonical, fully parenthesized form. Parsing the
// output yields an equivalent tree, which the fuzz tests rely on.
func format(n node) string {
	var b strings.Builder
	n.write(&b)
	return b.String()
}

// quote writes s as a string literal using only the escapes the lexer
// understands.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
