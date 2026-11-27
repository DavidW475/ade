package factory

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Expr ist eine übersetzte Bedingung (when). Sprache:
//
//	review.verdict == 'changes' || test.failed > 0
//	!(len(review.comments) == 0) && review.score >= 7
//
// Pfade beginnen mit dem Namen eines früheren Schritts und lesen Felder aus seinem
// strukturierten Artefakt; fehlende Felder sind null. Literale: 'text' oder "text", Zahlen,
// true, false, null. Operatoren: == != < <= > >= && || ! und Klammern; len(x) liefert die
// Länge einer Liste, eines Objekts oder einer Zeichenkette. < und > vergleichen nur Zahlen
// mit Zahlen und Text mit Text (sonst falsch). Wahr sind alle Werte außer false, null, 0,
// "" und leeren Listen/Objekten.
type Expr struct {
	src  string
	root node
}

// Lookup liefert das strukturierte Artefakt eines Schritts (ok=false: keins vorhanden).
type Lookup func(step string) (v any, ok bool)

// maxExprLen begrenzt eine Bedingung.
const maxExprLen = 2000

// ParseExpr übersetzt eine Bedingung.
func ParseExpr(src string) (*Expr, error) {
	if len(src) > maxExprLen {
		return nil, fmt.Errorf("Bedingung ist zu lang (höchstens %d Zeichen)", maxExprLen)
	}
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	n, err := p.or(0)
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tEOF {
		return nil, fmt.Errorf("unerwartet %q an Stelle %d", t.text, t.pos+1)
	}
	return &Expr{src: src, root: n}, nil
}

// String liefert den Quelltext.
func (e *Expr) String() string { return e.src }

// Paths liefert alle Pfade der Bedingung (z. B. [review verdict]).
func (e *Expr) Paths() [][]string {
	var out [][]string
	var walk func(n node)
	walk = func(n node) {
		switch x := n.(type) {
		case pathNode:
			out = append(out, x)
		case unaryNode:
			walk(x.x)
		case binaryNode:
			walk(x.l)
			walk(x.r)
		case lenNode:
			walk(x.x)
		}
	}
	walk(e.root)
	return out
}

// Eval wertet die Bedingung aus.
func (e *Expr) Eval(lookup Lookup) bool {
	return truthy(e.root.eval(lookup))
}

// --- Lexer -----------------------------------------------------------------------------

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tNum
	tStr
	tOp
)

type token struct {
	kind tokKind
	text string
	pos  int
	num  float64
}

func lex(src string) ([]token, error) {
	var out []token
	rs := []rune(src)
	for i := 0; i < len(rs); {
		c := rs[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '\'' || c == '"':
			start := i
			i++
			var b strings.Builder
			for ; i < len(rs) && rs[i] != c; i++ {
				if rs[i] == '\\' && i+1 < len(rs) {
					i++
				}
				b.WriteRune(rs[i])
			}
			if i >= len(rs) {
				return nil, fmt.Errorf("Zeichenkette ab Stelle %d ist nicht geschlossen", start+1)
			}
			i++
			out = append(out, token{kind: tStr, text: b.String(), pos: start})
		case c >= '0' && c <= '9':
			start := i
			for i < len(rs) && (rs[i] >= '0' && rs[i] <= '9' || rs[i] == '.') {
				i++
			}
			f, err := strconv.ParseFloat(string(rs[start:i]), 64)
			if err != nil {
				return nil, fmt.Errorf("ungültige Zahl %q", string(rs[start:i]))
			}
			out = append(out, token{kind: tNum, text: string(rs[start:i]), pos: start, num: f})
		case c == '_' || unicode.IsLetter(c):
			start := i
			for i < len(rs) && (rs[i] == '_' || rs[i] == '-' || unicode.IsLetter(rs[i]) || unicode.IsDigit(rs[i])) {
				i++
			}
			out = append(out, token{kind: tIdent, text: string(rs[start:i]), pos: start})
		default:
			two := ""
			if i+1 < len(rs) {
				two = string(rs[i : i+2])
			}
			switch two {
			case "==", "!=", "<=", ">=", "&&", "||":
				out = append(out, token{kind: tOp, text: two, pos: i})
				i += 2
				continue
			}
			switch c {
			case '<', '>', '!', '(', ')', '.':
				out = append(out, token{kind: tOp, text: string(c), pos: i})
				i++
			case '=':
				return nil, fmt.Errorf("an Stelle %d: Vergleich mit == schreiben", i+1)
			default:
				return nil, fmt.Errorf("unerwartetes Zeichen %q an Stelle %d", c, i+1)
			}
		}
	}
	return append(out, token{kind: tEOF, pos: len(rs)}), nil
}

// --- Parser ----------------------------------------------------------------------------

type parser struct {
	toks []token
	i    int
}

// maxExprDepth begrenzt die Verschachtelung.
const maxExprDepth = 64

func (p *parser) peek() token { return p.toks[p.i] }

func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tEOF {
		p.i++
	}
	return t
}

func (p *parser) isOp(s string) bool {
	t := p.peek()
	return t.kind == tOp && t.text == s
}

func (p *parser) binary(depth int, ops []string, sub func(int) (node, error)) (node, error) {
	l, err := sub(depth)
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tOp || !slices.Contains(ops, t.text) {
			return l, nil
		}
		p.next()
		r, err := sub(depth)
		if err != nil {
			return nil, err
		}
		l = binaryNode{op: t.text, l: l, r: r}
	}
}

func (p *parser) or(depth int) (node, error) {
	return p.binary(depth, []string{"||"}, p.and)
}

func (p *parser) and(depth int) (node, error) {
	return p.binary(depth, []string{"&&"}, p.cmp)
}

func (p *parser) cmp(depth int) (node, error) {
	l, err := p.unary(depth)
	if err != nil {
		return nil, err
	}
	t := p.peek()
	if t.kind == tOp && slices.Contains([]string{"==", "!=", "<", "<=", ">", ">="}, t.text) {
		p.next()
		r, err := p.unary(depth)
		if err != nil {
			return nil, err
		}
		if n := p.peek(); n.kind == tOp && slices.Contains([]string{"==", "!=", "<", "<=", ">", ">="}, n.text) {
			return nil, fmt.Errorf("an Stelle %d: Vergleiche nicht verketten, Klammern oder && verwenden", n.pos+1)
		}
		return binaryNode{op: t.text, l: l, r: r}, nil
	}
	return l, nil
}

func (p *parser) unary(depth int) (node, error) {
	if depth > maxExprDepth {
		return nil, fmt.Errorf("Bedingung ist zu tief verschachtelt")
	}
	if p.isOp("!") {
		p.next()
		x, err := p.unary(depth + 1)
		if err != nil {
			return nil, err
		}
		return unaryNode{x: x}, nil
	}
	return p.primary(depth)
}

func (p *parser) primary(depth int) (node, error) {
	t := p.next()
	switch t.kind {
	case tNum:
		return litNode{v: t.num}, nil
	case tStr:
		return litNode{v: t.text}, nil
	case tOp:
		if t.text == "(" {
			n, err := p.or(depth + 1)
			if err != nil {
				return nil, err
			}
			if !p.isOp(")") {
				return nil, fmt.Errorf("an Stelle %d: ) fehlt", p.peek().pos+1)
			}
			p.next()
			return n, nil
		}
	case tIdent:
		switch t.text {
		case "true":
			return litNode{v: true}, nil
		case "false":
			return litNode{v: false}, nil
		case "null":
			return litNode{v: nil}, nil
		case "len":
			if p.isOp("(") {
				p.next()
				x, err := p.or(depth + 1)
				if err != nil {
					return nil, err
				}
				if !p.isOp(")") {
					return nil, fmt.Errorf("an Stelle %d: ) fehlt", p.peek().pos+1)
				}
				p.next()
				return lenNode{x: x}, nil
			}
		}
		path := pathNode{t.text}
		for p.isOp(".") {
			p.next()
			f := p.next()
			if f.kind != tIdent {
				return nil, fmt.Errorf("an Stelle %d: Feldname nach . fehlt", f.pos+1)
			}
			path = append(path, f.text)
		}
		if len(path) < 2 {
			return nil, fmt.Errorf("an Stelle %d: %s ist kein Pfad (<schritt>.<feld>)", t.pos+1, t.text)
		}
		return path, nil
	case tEOF:
		return nil, fmt.Errorf("Bedingung endet unerwartet")
	}
	return nil, fmt.Errorf("unerwartet %q an Stelle %d", t.text, t.pos+1)
}

// --- Auswertung ------------------------------------------------------------------------

type node interface{ eval(Lookup) any }

type litNode struct{ v any }

func (n litNode) eval(Lookup) any { return n.v }

type pathNode []string

func (n pathNode) eval(lookup Lookup) any {
	v, ok := lookup(n[0])
	if !ok {
		return nil
	}
	for _, f := range n[1:] {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[f]
	}
	return v
}

type unaryNode struct{ x node }

func (n unaryNode) eval(l Lookup) any { return !truthy(n.x.eval(l)) }

type lenNode struct{ x node }

func (n lenNode) eval(l Lookup) any {
	switch v := n.x.eval(l).(type) {
	case []any:
		return float64(len(v))
	case map[string]any:
		return float64(len(v))
	case string:
		return float64(len([]rune(v)))
	}
	return float64(0)
}

type binaryNode struct {
	op   string
	l, r node
}

func (n binaryNode) eval(lookup Lookup) any {
	switch n.op {
	case "&&":
		return truthy(n.l.eval(lookup)) && truthy(n.r.eval(lookup))
	case "||":
		return truthy(n.l.eval(lookup)) || truthy(n.r.eval(lookup))
	}
	a, b := n.l.eval(lookup), n.r.eval(lookup)
	switch n.op {
	case "==":
		return jsonEqual(a, b)
	case "!=":
		return !jsonEqual(a, b)
	}
	var c int
	switch x := a.(type) {
	case float64:
		y, ok := b.(float64)
		if !ok || math.IsNaN(x) || math.IsNaN(y) {
			return false
		}
		c = cmp.Compare(x, y)
	case string:
		y, ok := b.(string)
		if !ok {
			return false
		}
		c = strings.Compare(x, y)
	default:
		return false
	}
	switch n.op {
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case ">=":
		return c >= 0
	}
	return false
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}
