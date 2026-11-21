package expr

// Grammar (lowest to highest precedence):
//
//	expr       = or
//	or         = and { "||" and }
//	and        = cmp { "&&" cmp }
//	cmp        = sum [ cmpop sum
//	                 | "between" sum "and" sum
//	                 | ["not"] "in" sum
//	                 | ("startsWith" | "endsWith" | "contains") sum ]
//	sum        = product { ("+" | "-") product }
//	product    = unary { ("*" | "/" | "%") unary }
//	unary      = ("!" | "-") unary | primary
//	primary    = literal | path | call | "(" expr ")" | list
//	path       = ident { "." ident }
//	call       = ident "(" [ expr { "," expr } ] ")"
//	list       = "[" [ expr { "," expr } [","] ] "]"
//
// The parser is a Pratt (top-down operator precedence) parser; the grammar
// above is what it accepts.

const (
	precOr = 1 + iota
	precAnd
	precCmp
	precSum
	precProduct
	precUnary
)

func infixPrec(k tokKind) int {
	switch k {
	case tOrOr:
		return precOr
	case tAndAnd:
		return precAnd
	case tEq, tNe, tLt, tLe, tGt, tGe, tIn, tNot, tBetween, tStartsWith, tEndsWith, tContains:
		return precCmp
	case tPlus, tMinus:
		return precSum
	case tStar, tSlash, tPercent:
		return precProduct
	}
	return 0
}

// Hard limits that protect the parser itself; the cost budget in
// [Options] is checked later, on the full tree.
const (
	maxSourceLen = 8 << 10
	maxDepth     = 64
)

type parser struct {
	src   string
	toks  []token
	i     int
	depth int
	nodes int
}

func parse(src string) (node, int, error) {
	if len(src) > maxSourceLen {
		return nil, 0, newError(ErrTooComplex, src, maxSourceLen, len(src), "expression is %d bytes, limit is %d", len(src), maxSourceLen)
	}
	toks, err := lex(src)
	if err != nil {
		return nil, 0, err
	}
	p := &parser{src: src, toks: toks}
	if p.peek().kind == tEOF {
		return nil, 0, newError(ErrSyntax, src, 0, len(src), "empty expression")
	}
	n, err := p.expr(0)
	if err != nil {
		return nil, 0, err
	}
	if t := p.peek(); t.kind != tEOF {
		return nil, 0, p.unexpected(t, "end of expression")
	}
	return n, p.nodes, nil
}

func (p *parser) peek() token { return p.toks[p.i] }

func (p *parser) advance() token {
	t := p.toks[p.i]
	if t.kind != tEOF {
		p.i++
	}
	return t
}

func (p *parser) unexpected(t token, want string) error {
	if t.kind == tEOF {
		return newError(ErrSyntax, p.src, t.pos, t.end, "unexpected end of input, expected %s", want)
	}
	what := t.kind.String()
	if t.kind == tIdent {
		what = "identifier " + quote(t.text)
	} else if t.kind == tInt || t.kind == tDuration || t.kind == tString {
		what = t.kind.String() + " " + p.src[t.pos:t.end]
	} else {
		what = quote(what)
	}
	return newError(ErrSyntax, p.src, t.pos, t.end, "unexpected %s, expected %s", what, want)
}

func (p *parser) expect(k tokKind, want string) (token, error) {
	t := p.peek()
	if t.kind != k {
		return t, p.unexpected(t, want)
	}
	return p.advance(), nil
}

func (p *parser) enter(t token) error {
	p.depth++
	if p.depth > maxDepth {
		return newError(ErrTooComplex, p.src, t.pos, t.end, "expression nested deeper than %d levels", maxDepth)
	}
	return nil
}

// expr parses an expression whose operators all bind tighter than minPrec.
func (p *parser) expr(minPrec int) (node, error) {
	if err := p.enter(p.peek()); err != nil {
		return nil, err
	}
	defer func() { p.depth-- }()
	left, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		prec := infixPrec(t.kind)
		if prec == 0 || prec <= minPrec {
			return left, nil
		}
		p.advance()
		p.nodes++
		start, _ := left.span()
		switch t.kind {
		case tBetween:
			lo, err := p.expr(precCmp)
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(tAnd, `"and" in "x between lo and hi"`); err != nil {
				return nil, err
			}
			hi, err := p.expr(precCmp)
			if err != nil {
				return nil, err
			}
			_, end := hi.span()
			left = &betweenNode{base: base{start: start, end: end}, x: left, lo: lo, hi: hi}
		case tIn, tNot:
			not := t.kind == tNot
			if not {
				if _, err := p.expect(tIn, `"in" after "not"`); err != nil {
					return nil, err
				}
			}
			list, err := p.expr(precCmp)
			if err != nil {
				return nil, err
			}
			_, end := list.span()
			left = &inNode{base: base{start: start, end: end}, not: not, x: left, list: list}
		default:
			// All binary operators are left-associative: the right operand
			// may only contain operators that bind tighter.
			right, err := p.expr(prec)
			if err != nil {
				return nil, err
			}
			_, end := right.span()
			left = &binaryNode{base: base{start: start, end: end}, op: t.kind, opPos: t.pos, l: left, r: right}
		}
	}
}

func (p *parser) unary() (node, error) {
	t := p.peek()
	if t.kind == tBang || t.kind == tMinus {
		if err := p.enter(t); err != nil {
			return nil, err
		}
		defer func() { p.depth-- }()
		p.advance()
		p.nodes++
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		_, end := x.span()
		return &unaryNode{base: base{start: t.pos, end: end}, op: t.kind, x: x}, nil
	}
	return p.primary()
}

func (p *parser) primary() (node, error) {
	t := p.advance()
	p.nodes++
	switch t.kind {
	case tTrue, tFalse, tInt, tDuration, tString:
		return &litNode{base: base{start: t.pos, end: t.end}, kind: t.kind, ival: t.ival, sval: t.text}, nil
	case tLParen:
		n, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tRParen, `")"`); err != nil {
			return nil, err
		}
		return n, nil
	case tLBrack:
		ln := &listNode{base: base{start: t.pos}}
		for p.peek().kind != tRBrack {
			e, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			ln.elems = append(ln.elems, e)
			if p.peek().kind != tComma {
				break
			}
			p.advance()
		}
		end, err := p.expect(tRBrack, `"," or "]"`)
		if err != nil {
			return nil, err
		}
		ln.end = end.end
		return ln, nil
	case tIdent:
		if p.peek().kind == tLParen {
			return p.call(t)
		}
		path, end := t.text, t.end
		for p.peek().kind == tDot {
			p.advance()
			part, err := p.expect(tIdent, "field name after \".\"")
			if err != nil {
				return nil, err
			}
			path += "." + part.text
			end = part.end
		}
		return &fieldNode{base: base{start: t.pos, end: end}, path: path}, nil
	}
	return nil, p.unexpected(t, "an operand")
}

func (p *parser) call(name token) (node, error) {
	p.advance() // (
	c := &callNode{base: base{start: name.pos}, name: name.text, nameEnd: name.end}
	for p.peek().kind != tRParen {
		a, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		c.args = append(c.args, a)
		if p.peek().kind != tComma {
			break
		}
		p.advance()
	}
	end, err := p.expect(tRParen, `"," or ")"`)
	if err != nil {
		return nil, err
	}
	c.end = end.end
	return c, nil
}
