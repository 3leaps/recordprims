// Package wire is recordprims' strict reader for durable-record wire text:
// the contract's first two evaluation stages, ingest and numeric_domain, run on
// raw bytes before any value is converted to a Go number.
//
// encoding/json is not used. It keeps the last of duplicate member names,
// replaces invalid UTF-8 and lone-surrogate escapes with U+FFFD, and converts
// numbers to float64, so it silently changes documents the contract requires a
// reader to refuse.
package wire

import (
	"fmt"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// Reasons this package returns. ReasonIngest and ReasonNumericDomain are
// contract refusal codes. ReasonLocalLimit is recordprims' own: the document
// may be valid, but it exceeds a resource bound this implementation imposes
// (nesting depth), so it must not be reported as malformed.
const (
	ReasonIngest        = "ingest"
	ReasonNumericDomain = "numeric_domain"
	ReasonLocalLimit    = "local_limit"
)

// Reject is a refusal with a contract reason code.
type Reject struct {
	Reason string
	Detail string
}

func (r *Reject) Error() string { return r.Reason + ": " + r.Detail }

func ingestf(format string, a ...any) error {
	return &Reject{Reason: ReasonIngest, Detail: fmt.Sprintf(format, a...)}
}

// Kind is the JSON type of a Node.
type Kind uint8

// Node kinds.
const (
	Null Kind = iota
	Bool
	Number
	String
	Array
	Object
)

// Node is one parsed JSON value. Numbers keep their raw token; Int is set only
// on protocol-integer positions that passed numeric_domain.
type Node struct {
	Kind   Kind
	Bool   bool
	Str    string   // String
	Token  string   // Number: the raw token, as written
	Int    int64    // Number at a protocol-integer position, after numeric_domain
	IsInt  bool     // Int is valid
	Elems  []*Node  // Array
	Fields []Member // Object, in document order
}

// Member is one object member.
type Member struct {
	Name  string
	Value *Node
}

// Get returns the value of the named member of an object, or nil.
func (n *Node) Get(name string) *Node {
	if n == nil || n.Kind != Object {
		return nil
	}
	for _, m := range n.Fields {
		if m.Name == name {
			return m.Value
		}
	}
	return nil
}

// maxDepth bounds nesting so hostile input cannot exhaust the stack. The
// contract defines no bound and record payloads are arbitrary JSON, so a
// deeper document is refused as ReasonLocalLimit, not as malformed. The
// contract's own envelopes nest a handful of levels.
const maxDepth = 512

// ParseJSON is stage 1's syntax pass: strict RFC 8259 over UTF-8 text, with
// unique member names at every depth (compared after unescaping), no lone
// surrogates, no byte order mark and no trailing data. Numbers are kept as
// raw tokens.
func ParseJSON(raw []byte) (*Node, error) {
	if !utf8.Valid(raw) {
		return nil, ingestf("invalid UTF-8")
	}
	p := &parser{s: raw}
	p.ws()
	n, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.s) {
		return nil, ingestf("trailing data at offset %d", p.i)
	}
	return n, nil
}

type parser struct {
	s []byte
	i int
}

func (p *parser) ws() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) value(depth int) (*Node, error) {
	if depth > maxDepth {
		return nil, &Reject{Reason: ReasonLocalLimit, Detail: fmt.Sprintf("nesting deeper than %d", maxDepth)}
	}
	if p.i >= len(p.s) {
		return nil, ingestf("unexpected end of input")
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		s, err := p.str()
		if err != nil {
			return nil, err
		}
		return &Node{Kind: String, Str: s}, nil
	case c == 't':
		return p.literal("true", &Node{Kind: Bool, Bool: true})
	case c == 'f':
		return p.literal("false", &Node{Kind: Bool})
	case c == 'n':
		return p.literal("null", &Node{Kind: Null})
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	default:
		return nil, ingestf("unexpected character at offset %d", p.i)
	}
}

func (p *parser) literal(word string, n *Node) (*Node, error) {
	if len(p.s)-p.i < len(word) || string(p.s[p.i:p.i+len(word)]) != word {
		return nil, ingestf("invalid literal at offset %d", p.i)
	}
	p.i += len(word)
	return n, nil
}

func (p *parser) object(depth int) (*Node, error) {
	p.i++ // {
	n := &Node{Kind: Object}
	seen := map[string]bool{}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return n, nil
	}
	for {
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return nil, ingestf("expected member name at offset %d", p.i)
		}
		name, err := p.str()
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, ingestf("duplicate object member name")
		}
		seen[name] = true
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, ingestf("expected ':' at offset %d", p.i)
		}
		p.i++
		p.ws()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		n.Fields = append(n.Fields, Member{Name: name, Value: v})
		p.ws()
		if p.i >= len(p.s) {
			return nil, ingestf("unexpected end of input in object")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return n, nil
		default:
			return nil, ingestf("expected ',' or '}' at offset %d", p.i)
		}
	}
}

func (p *parser) array(depth int) (*Node, error) {
	p.i++ // [
	n := &Node{Kind: Array}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return n, nil
	}
	for {
		p.ws()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		n.Elems = append(n.Elems, v)
		p.ws()
		if p.i >= len(p.s) {
			return nil, ingestf("unexpected end of input in array")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return n, nil
		default:
			return nil, ingestf("expected ',' or ']' at offset %d", p.i)
		}
	}
}

// str reads a string starting at the opening quote. Control characters are
// refused, escapes are decoded, and a surrogate escape must be one half of a
// valid pair.
func (p *parser) str() (string, error) {
	p.i++ // "
	var out []byte
	for {
		if p.i >= len(p.s) {
			return "", ingestf("unterminated string")
		}
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return string(out), nil
		case c < 0x20:
			return "", ingestf("control character in string at offset %d", p.i)
		case c != '\\':
			out = append(out, c)
			p.i++
			continue
		}
		// Escape.
		p.i++
		if p.i >= len(p.s) {
			return "", ingestf("unterminated escape")
		}
		e := p.s[p.i]
		p.i++
		switch e {
		case '"', '\\', '/':
			out = append(out, e)
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, err := p.hex4()
			if err != nil {
				return "", err
			}
			if utf16.IsSurrogate(r) {
				if r >= 0xDC00 || p.i+6 > len(p.s) || p.s[p.i] != '\\' || p.s[p.i+1] != 'u' {
					return "", ingestf("lone Unicode surrogate")
				}
				p.i += 2
				r2, err := p.hex4()
				if err != nil {
					return "", err
				}
				r = utf16.DecodeRune(r, r2)
				if r == utf8.RuneError {
					return "", ingestf("lone Unicode surrogate")
				}
			}
			out = utf8.AppendRune(out, r)
		default:
			return "", ingestf("invalid escape at offset %d", p.i-1)
		}
	}
}

func (p *parser) hex4() (rune, error) {
	if p.i+4 > len(p.s) {
		return 0, ingestf("truncated \\u escape")
	}
	v, err := strconv.ParseUint(string(p.s[p.i:p.i+4]), 16, 32)
	if err != nil {
		return 0, ingestf("invalid \\u escape")
	}
	for _, c := range p.s[p.i : p.i+4] {
		if c == '+' || c == '-' || c == '_' {
			return 0, ingestf("invalid \\u escape")
		}
	}
	p.i += 4
	return rune(v), nil
}

// number reads one RFC 8259 number token without converting it.
func (p *parser) number() (*Node, error) {
	start := p.i
	if p.s[p.i] == '-' {
		p.i++
	}
	switch {
	case p.i < len(p.s) && p.s[p.i] == '0':
		p.i++
	case p.i < len(p.s) && p.s[p.i] >= '1' && p.s[p.i] <= '9':
		for p.i < len(p.s) && isDigit(p.s[p.i]) {
			p.i++
		}
	default:
		return nil, ingestf("invalid number at offset %d", start)
	}
	if p.i < len(p.s) && p.s[p.i] == '.' {
		p.i++
		d := p.i
		for p.i < len(p.s) && isDigit(p.s[p.i]) {
			p.i++
		}
		if p.i == d {
			return nil, ingestf("invalid number at offset %d", start)
		}
	}
	if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		p.i++
		if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
			p.i++
		}
		d := p.i
		for p.i < len(p.s) && isDigit(p.s[p.i]) {
			p.i++
		}
		if p.i == d {
			return nil, ingestf("invalid number at offset %d", start)
		}
	}
	return &Node{Kind: Number, Token: string(p.s[start:p.i])}, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
