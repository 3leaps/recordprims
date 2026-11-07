package wire

import "strconv"

// Encode writes n as compact JSON text: members in document order, numbers as
// their raw tokens, and strings with only the escapes JSON requires. Parsing
// the result gives back an equal tree, so a digest over Encode(n) can be
// computed by canonicalising it.
func Encode(n *Node) []byte {
	return appendNode(nil, n)
}

func appendNode(b []byte, n *Node) []byte {
	switch n.Kind {
	case Null:
		return append(b, "null"...)
	case Bool:
		return strconv.AppendBool(b, n.Bool)
	case Number:
		return append(b, n.Token...)
	case String:
		return appendString(b, n.Str)
	case Array:
		b = append(b, '[')
		for i, e := range n.Elems {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendNode(b, e)
		}
		return append(b, ']')
	default:
		b = append(b, '{')
		for i, m := range n.Fields {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendString(b, m.Name)
			b = append(b, ':')
			b = appendNode(b, m.Value)
		}
		return append(b, '}')
	}
}

const hexDigits = "0123456789abcdef"

func appendString(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b = append(b, '\\', c)
		case c < 0x20:
			b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		default:
			b = append(b, c)
		}
	}
	return append(b, '"')
}
