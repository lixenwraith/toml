package toml

import (
	"fmt"
	"unicode/utf8"
)

type Lexer struct {
	input []byte
	pos   int
	line  int
	col   int
	width int
}

func NewLexer(input []byte) *Lexer {
	return &Lexer{
		input: input,
		line:  1,
		col:   0,
	}
}

func (l *Lexer) NextToken() Token {
	l.skipWhitespace()

	if l.pos >= len(l.input) {
		return l.newToken(TokenEOF, "")
	}

	ch := l.peek()

	if ch == '\n' {
		l.advance()
		return l.newToken(TokenNewline, "\n")
	}

	if ch == '#' {
		return l.readComment()
	}

	switch ch {
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
	if isDigit(ch) {
		return l.readNumber()
	}
	if ch == '+' {
		if isDigit(l.peekAt(1)) {
			return l.readNumber()
		}
		// Lone + is invalid TOML
		l.advance()
		return l.newToken(TokenError, "unexpected character: +")
	}
	if ch == '-' {
		if isDigit(l.peekAt(1)) {
			return l.readNumber()
		}
		// Bare key can start with hyphen
		return l.readIdent()
	}

	// Identifier: alpha or underscore
	if isAlpha(ch) || ch == '_' {
		return l.readIdent()
	}

	l.advance()
	return l.newToken(TokenError, fmt.Sprintf("unexpected character: %c", ch))
}

func (l *Lexer) readNumber() Token {
	start := l.pos
	startCol := l.col

	// Optional sign
	if l.peek() == '+' || l.peek() == '-' {
		l.advance()
	}

	// Check radix prefix
	if l.peek() == '0' {
		next := l.peekAt(1)
		switch next {
		case 'x', 'X':
			l.advance() // '0'
			l.advance() // 'x'
			return l.readHex(start, startCol)
		case 'o', 'O':
			l.advance()
			l.advance()
			return l.readOctal(start, startCol)
		case 'b', 'B':
			l.advance()
			l.advance()
			return l.readBinary(start, startCol)
		}
	}

	// Integer part
	for isDigit(l.peek()) {
		l.advance()
	}

	isFloat := false

	// Fractional part: '.' followed by digit
	if l.peek() == '.' && isDigit(l.peekAt(1)) {
		isFloat = true
		l.advance() // consume '.'

		for isDigit(l.peek()) {
			l.advance()
		}

		// Multi-dot: another '.' followed by digit is fatal
		if l.peek() == '.' && isDigit(l.peekAt(1)) {
			return Token{Type: TokenError, Literal: "invalid number: multiple decimal points", Line: l.line, Col: startCol}
		}
	}

	// Exponent: e/E followed by optional sign and digits
	if l.peek() == 'e' || l.peek() == 'E' {
		next := l.peekAt(1)
		validExp := isDigit(next) || ((next == '+' || next == '-') && isDigit(l.peekAt(2)))
		if validExp {
			isFloat = true
			l.advance() // 'e'
			if l.peek() == '+' || l.peek() == '-' {
				l.advance()
			}
			for isDigit(l.peek()) {
				l.advance()
			}
		}
	}

	lit := string(l.input[start:l.pos])

	// Validate float format: 1.e2 is invalid (no digit after dot before exponent)
	if isFloat {
		if err := validateFloat(lit); err != nil {
			return Token{Type: TokenError, Literal: err.Error(), Line: l.line, Col: startCol}
		}
		return Token{Type: TokenFloat, Literal: lit, Line: l.line, Col: startCol}
	}
	return Token{Type: TokenInteger, Literal: lit, Line: l.line, Col: startCol}
}

func validateFloat(lit string) error {
	// Find dot position (if any)
	dotIdx := -1
	expIdx := -1
	for i := 0; i < len(lit); i++ {
		if lit[i] == '.' {
			dotIdx = i
		}
		if lit[i] == 'e' || lit[i] == 'E' {
			expIdx = i
			break
		}
	}

	if dotIdx >= 0 {
		// Check digit before dot (ignoring sign)
		start := 0
		if lit[0] == '+' || lit[0] == '-' {
			start = 1
		}
		if dotIdx == start {
			return fmt.Errorf("invalid float %q: no digit before decimal point", lit)
		}

		// Check digit after dot
		afterDot := dotIdx + 1
		endFrac := len(lit)
		if expIdx > 0 {
			endFrac = expIdx
		}
		if afterDot >= endFrac {
			return fmt.Errorf("invalid float %q: no digit after decimal point", lit)
		}
	}
	return nil
}

func (l *Lexer) readHex(start, startCol int) Token {
	if !isHexDigit(l.peek()) {
		return Token{Type: TokenError, Literal: "invalid hex: no digits after prefix", Line: l.line, Col: startCol}
	}
	for isHexDigit(l.peek()) {
		l.advance()
	}
	return Token{Type: TokenInteger, Literal: string(l.input[start:l.pos]), Line: l.line, Col: startCol}
}

func (l *Lexer) readOctal(start, startCol int) Token {
	if !isOctalDigit(l.peek()) {
		return Token{Type: TokenError, Literal: "invalid octal: no digits after prefix", Line: l.line, Col: startCol}
	}
	for isOctalDigit(l.peek()) {
		l.advance()
	}
	return Token{Type: TokenInteger, Literal: string(l.input[start:l.pos]), Line: l.line, Col: startCol}
}

func (l *Lexer) readBinary(start, startCol int) Token {
	if !isBinaryDigit(l.peek()) {
		return Token{Type: TokenError, Literal: "invalid binary: no digits after prefix", Line: l.line, Col: startCol}
	}
	for isBinaryDigit(l.peek()) {
		l.advance()
	}
	return Token{Type: TokenInteger, Literal: string(l.input[start:l.pos]), Line: l.line, Col: startCol}
}

func (l *Lexer) readIdent() Token {
	start := l.pos
	for l.pos < len(l.input) {
		ch := l.peek()
		if isAlpha(ch) || isDigit(ch) || ch == '_' || ch == '-' {
			l.advance()
		} else {
			break
		}
	}
	lit := string(l.input[start:l.pos])
	if lit == "true" || lit == "false" {
		return l.newToken(TokenBool, lit)
	}
	return l.newToken(TokenIdent, lit)
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
		l.width = 0
		return 0
	}
	r, w := utf8.DecodeRune(l.input[l.pos:])
	l.width = w
	l.pos += w
	if r == '\n' {
		l.line++
		l.col = 0
	} else {
		l.col++
	}
	return r
}

func (l *Lexer) peek() rune {
	if l.pos >= len(l.input) {
		return 0
	}
	r, _ := utf8.DecodeRune(l.input[l.pos:])
	return r
}

func (l *Lexer) peekAt(n int) rune {
	pos := l.pos
	for i := 0; i < n && pos < len(l.input); i++ {
		_, w := utf8.DecodeRune(l.input[pos:])
		pos += w
	}
	if pos >= len(l.input) {
		return 0
	}
	r, _ := utf8.DecodeRune(l.input[pos:])
	return r
}

func (l *Lexer) skipWhitespace() {
	for l.pos < len(l.input) {
		ch := l.peek()
		if ch == ' ' || ch == '\t' || ch == '\r' {
			l.advance()
		} else {
			break
		}
	}
}

func (l *Lexer) readComment() Token {
	l.advance() // '#'
	start := l.pos
	for l.pos < len(l.input) && l.peek() != '\n' {
		l.advance()
	}
	return l.newToken(TokenComment, string(l.input[start:l.pos]))
}

func (l *Lexer) readString() Token {
	l.advance() // opening quote
	var result []byte
	escaped := false

	for l.pos < len(l.input) {
		ch := l.peek()
		if ch < 0x20 && ch != '\t' || ch == 0x7f {
			return l.newToken(TokenError, "unterminated string: newline in basic string")
		}
		if ch == '"' && !escaped {
			l.advance()
			return l.newToken(TokenString, string(result))
		}
		if ch == '\\' && !escaped {
			escaped = true
			l.advance()
			continue
		}
		if escaped {
			switch ch {
			case '"':
				result = append(result, '"')
			case '\\':
				result = append(result, '\\')
			case 'n':
				result = append(result, '\n')
			case 't':
				result = append(result, '\t')
			case 'r':
				result = append(result, '\r')
			case 'b':
				result = append(result, '\b')
			case 'f':
				result = append(result, '\f')
			case 'u', 'U':
				n := 4
				if ch == 'U' {
					n = 8
				}
				l.advance() // consume 'u'/'U'
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
				var buf [utf8.UTFMax]byte
				w := utf8.EncodeRune(buf[:], code)
				result = append(result, buf[:w]...)
				escaped = false
				continue // hex digits already consumed; skip trailing l.advance()
			default:
				// Unknown escape: preserve backslash and full rune
				result = append(result, '\\')
				var buf [utf8.UTFMax]byte
				n := utf8.EncodeRune(buf[:], ch)
				result = append(result, buf[:n]...)
			}
			escaped = false
		} else {
			// Get actual width of current character, don't use stale l.width
			_, w := utf8.DecodeRune(l.input[l.pos:])
			result = append(result, l.input[l.pos:l.pos+w]...)
		}
		l.advance()
	}
	return l.newToken(TokenError, "unterminated string")
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
