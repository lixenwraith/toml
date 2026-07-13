package toml

import (
	"fmt"
)

// TokenType represents the type of a lexical token
type TokenType int

const (
	TokenError TokenType = iota
	TokenEOF
	TokenComment

	// Literals
	TokenIdent   // bare key
	TokenString  // "quoted"
	TokenInteger // 123
	TokenFloat   // 123.45
	TokenBool    // true/false

	// Operators and Delimiters
	TokenEqual    // =
	TokenDot      // .
	TokenComma    // ,
	TokenLBracket // [
	TokenRBracket // ]
	TokenLBrace   // {
	TokenRBrace   // }
	TokenNewline  // \n
)

// Token represents a lexical token
type Token struct {
	Type    TokenType
	Literal string
	Line    int
	Col     int
}

func (t Token) String() string {
	switch t.Type {
	case TokenEOF:
		return "EOF"
	case TokenError:
		return fmt.Sprintf("Error(%s)", t.Literal)
	case TokenNewline:
		return "Newline"
	}
	if len(t.Literal) > 20 {
		return fmt.Sprintf("%q...", t.Literal[:20])
	}
	return fmt.Sprintf("%q", t.Literal)
}