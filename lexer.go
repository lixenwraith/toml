package toml

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

type Lexer struct {
	input []byte
	pos   int
	line  int
	col   int
	// lean is a parser's lexer: a comment or number carries no Literal, the parser
	// reading what it needs from Pos and End, so neither allocates
	lean bool
}

func NewLexer(input []byte) *Lexer {
	return &Lexer{input: input, line: 1}
}

func (l *Lexer) NextToken() Token {
	l.skipWhitespace()
	start := l.pos
	t := l.next()
	t.Pos, t.End = start, l.pos
	return t
}

func (l *Lexer) next() Token {
	if l.pos >= len(l.input) {
		return l.newToken(TokenEOF, "")
	}

	ch := l.peek()
	switch ch {
	case '\n':
		l.advance()
		return l.newToken(TokenNewline, "\n")
	case '#':
		return l.readComment()
	case '=':
		l.advance()
		return l.newToken(TokenEqual, "=")
	case '.':
		l.advance()
		return l.newToken(TokenDot, ".")
	case ',':
		l.advance()
		return l.newToken(TokenComma, ",")
	case '[':
		l.advance()
		return l.newToken(TokenLBracket, "[")
	case ']':
		l.advance()
		return l.newToken(TokenRBracket, "]")
	case '{':
		l.advance()
		return l.newToken(TokenLBrace, "{")
	case '}':
		l.advance()
		return l.newToken(TokenRBrace, "}")
	case '"':
		return l.readString()
	}

	// Number: digit or sign+digit
	if isDigit(ch) || (ch == '+' || ch == '-') && isDigit(rune(l.byteAt(l.pos+1))) {
		return l.readNumber()
	}
	if ch == '+' {
		// Lone + is invalid TOML
		l.advance()
		return l.newToken(TokenError, "unexpected character: +")
	}
	// A bare key may start with a hyphen, not a digit
	if isAlpha(ch) || ch == '_' || ch == '-' {
		return l.readIdent()
	}

	l.advance()
	return l.newToken(TokenError, fmt.Sprintf("unexpected character: %q", ch))
}

func (l *Lexer) readNumber() Token {
	start, startCol := l.pos, l.col
	i := start
	if c := l.input[i]; c == '+' || c == '-' {
		i++
	}

	// Radix prefix
	if l.byteAt(i) == '0' {
		var digit func(rune) bool
		var none string
		switch l.byteAt(i + 1) {
		case 'x', 'X':
			digit, none = isHexDigit, "invalid hex: no digits after prefix"
		case 'o', 'O':
			digit, none = isOctalDigit, "invalid octal: no digits after prefix"
		case 'b', 'B':
			digit, none = isBinaryDigit, "invalid binary: no digits after prefix"
		}
		if digit != nil {
			i += 2
			j := i
			for j < len(l.input) && digit(rune(l.input[j])) {
				j++
			}
			l.skipTo(j)
			if j == i {
				return Token{Type: TokenError, Literal: none, Line: l.line, Col: startCol}
			}
			return l.number(TokenInteger, start, startCol)
		}
	}

	typ := TokenInteger
	i = l.digits(i)
	// Fractional part: '.' followed by digit
	if l.byteAt(i) == '.' && isDigit(rune(l.byteAt(i+1))) {
		typ, i = TokenFloat, l.digits(i+1)
		if l.byteAt(i) == '.' && isDigit(rune(l.byteAt(i+1))) {
			l.skipTo(i)
			return Token{Type: TokenError, Literal: "invalid number: multiple decimal points", Line: l.line, Col: startCol}
		}
	}
	// Exponent: e/E followed by optional sign and digits
	if c := l.byteAt(i); c == 'e' || c == 'E' {
		j := i + 1
		if c := l.byteAt(j); c == '+' || c == '-' {
			j++
		}
		if isDigit(rune(l.byteAt(j))) {
			typ, i = TokenFloat, l.digits(j)
		}
	}
	l.skipTo(i)
	return l.number(typ, start, startCol)
}

func (l *Lexer) number(typ TokenType, start, col int) Token {
	t := Token{Type: typ, Line: l.line, Col: col}
	if !l.lean {
		t.Literal = string(l.input[start:l.pos])
	}
	return t
}

// digits is the offset past the decimal digits at i
func (l *Lexer) digits(i int) int {
	for i < len(l.input) && isDigit(rune(l.input[i])) {
		i++
	}
	return i
}

func (l *Lexer) readIdent() Token {
	start, i := l.pos, l.pos
	for i < len(l.input) {
		if c := rune(l.input[i]); isAlpha(c) || isDigit(c) || c == '_' || c == '-' {
			i++
		} else {
			break
		}
	}
	l.skipTo(i)
	switch lit := l.input[start:i]; string(lit) {
	case "true":
		return l.newToken(TokenBool, "true")
	case "false":
		return l.newToken(TokenBool, "false")
	default:
		return l.newToken(TokenIdent, string(lit))
	}
}

func (l *Lexer) newToken(typ TokenType, literal string) Token {
	col := l.col - len(literal)
	if col < 0 {
		col = 0
	}
	return Token{Type: typ, Literal: literal, Line: l.line, Col: col}
}

func (l *Lexer) advance() rune {
	if l.pos >= len(l.input) {
		return 0
	}
	r, w := rune(l.input[l.pos]), 1
	if r >= utf8.RuneSelf {
		r, w = utf8.DecodeRune(l.input[l.pos:])
	}
	l.pos += w
	if r == '\n' {
		l.line++
		l.col = 0
	} else {
		l.col++
	}
	return r
}

// skipTo advances to i over single-byte runes on one line
func (l *Lexer) skipTo(i int) {
	l.col += i - l.pos
	l.pos = i
}

func (l *Lexer) peek() rune {
	if l.pos >= len(l.input) {
		return 0
	}
	if c := l.input[l.pos]; c < utf8.RuneSelf {
		return rune(c)
	}
	r, _ := utf8.DecodeRune(l.input[l.pos:])
	return r
}

// byteAt is the input byte at i, 0 past the end
func (l *Lexer) byteAt(i int) byte {
	if i < len(l.input) {
		return l.input[i]
	}
	return 0
}

// skipWhitespace takes a CR only as part of CRLF: a lone one would let a
// terminal hide what precedes it on the line, so it lexes as an error.
func (l *Lexer) skipWhitespace() {
	i := l.pos
	for i < len(l.input) {
		if c := l.input[i]; c == ' ' || c == '\t' || c == '\r' && l.byteAt(i+1) == '\n' {
			i++
		} else {
			break
		}
	}
	l.skipTo(i)
}

// readComment refuses control characters but tab, as TOML does: an ESC or a
// lone CR in a comment can hide the key after it from a reader.
func (l *Lexer) readComment() Token {
	l.advance() // '#'
	start := l.pos
	for l.pos < len(l.input) {
		c := l.input[l.pos]
		if c == '\n' || c == '\r' && l.byteAt(l.pos+1) == '\n' {
			break
		}
		if c < 0x20 && c != '\t' || c == 0x7f {
			return l.newToken(TokenError, fmt.Sprintf("control character %q in comment", rune(c)))
		}
		w := 1
		if c >= utf8.RuneSelf {
			_, w = utf8.DecodeRune(l.input[l.pos:])
		}
		l.pos += w
		l.col++
	}
	if l.lean {
		return l.newToken(TokenComment, "")
	}
	return l.newToken(TokenComment, string(l.input[start:l.pos]))
}

// readString takes a string without escapes in one allocation, the string itself
func (l *Lexer) readString() Token {
	l.advance() // opening quote
	start := l.pos
	for i := start; i < len(l.input); i++ {
		c := l.input[i]
		if c == '"' {
			l.col += utf8.RuneCount(l.input[start:i]) + 1
			l.pos = i + 1
			return l.newToken(TokenString, string(l.input[start:i]))
		}
		if c == '\\' || c < 0x20 && c != '\t' || c == 0x7f {
			break
		}
	}
	return l.readEscaped()
}

// readEscaped decodes a string's escapes into one buffer sized to its source,
// which an escape only shortens
func (l *Lexer) readEscaped() Token {
	var result strings.Builder
	result.Grow(l.stringBound())
	escaped := false

	for l.pos < len(l.input) {
		ch := l.peek()
		if ch < 0x20 && ch != '\t' || ch == 0x7f {
			return l.newToken(TokenError, "unterminated string: newline in basic string")
		}
		if ch == '"' && !escaped {
			l.advance()
			return l.newToken(TokenString, result.String())
		}
		if ch == '\\' && !escaped {
			escaped = true
			l.advance()
			continue
		}
		if escaped {
			switch ch {
			case '"':
				result.WriteByte('"')
			case '\\':
				result.WriteByte('\\')
			case 'n':
				result.WriteByte('\n')
			case 't':
				result.WriteByte('\t')
			case 'r':
				result.WriteByte('\r')
			case 'b':
				result.WriteByte('\b')
			case 'f':
				result.WriteByte('\f')
			case 'e':
				result.WriteByte(0x1b)
			case 'x', 'u', 'U':
				n := 2
				switch ch {
				case 'u':
					n = 4
				case 'U':
					n = 8
				}
				l.advance() // consume 'x'/'u'/'U'
				var code rune
				for j := 0; j < n; j++ {
					d, ok := hexDigitVal(l.peek())
					if !ok {
						return l.newToken(TokenError, "invalid unicode escape: expected hex digit")
					}
					code = code<<4 | rune(d)
					l.advance()
				}
				// ValidRune rejects surrogates and > U+10FFFF (spec: scalar values only).
				// \UFFFFFFFF overflows rune's sign bit -> negative -> rejected here too.
				if !utf8.ValidRune(code) {
					return l.newToken(TokenError, fmt.Sprintf("invalid unicode escape: U+%X is not a scalar value", code))
				}
				result.WriteRune(code)
				escaped = false
				continue // hex digits already consumed; skip trailing l.advance()
			default:
				// Unknown escape, invalid in every TOML version: preserve backslash and full rune
				result.WriteByte('\\')
				result.WriteRune(ch)
			}
			escaped = false
		} else {
			_, w := utf8.DecodeRune(l.input[l.pos:])
			result.Write(l.input[l.pos : l.pos+w])
		}
		l.advance()
	}
	return l.newToken(TokenError, "unterminated string")
}

// stringBound is the source length of the string at pos, through its closing
// quote, a line's end or the input's
func (l *Lexer) stringBound() int {
	for i := l.pos; i < len(l.input); i++ {
		switch c := l.input[i]; {
		case c == '\\':
			i++
		case c == '"', c < 0x20 && c != '\t', c == 0x7f:
			return i - l.pos
		}
	}
	return len(l.input) - l.pos
}

func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}

func isAlpha(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isHexDigit(r rune) bool {
	return isDigit(r) || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

func hexDigitVal(r rune) (int, bool) {
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0'), true
	case r >= 'a' && r <= 'f':
		return int(r-'a') + 10, true
	case r >= 'A' && r <= 'F':
		return int(r-'A') + 10, true
	}
	return 0, false
}

func isOctalDigit(r rune) bool {
	return r >= '0' && r <= '7'
}

func isBinaryDigit(r rune) bool {
	return r == '0' || r == '1'
}
