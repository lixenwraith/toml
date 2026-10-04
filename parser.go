package toml

import (
	"cmp"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"unicode/utf8"
)

// Parser parses TOML tokens into a map[string]any
type Parser struct {
	root    map[string]any
	lexer   *Lexer
	current any // Pointer to the current map or slice of maps being populated (scope)
	// Identity set of inline-table maps (immutable per TOML spec)
	frozen    map[uintptr]bool
	curToken  Token
	peekToken Token
	// Value nesting depth (arrays / inline tables)
	depth int
	// Tables (maps) a document may create; 0 means DefaultMaxTables
	MaxTables int
	tables    int
}

// Recursion bound for parseValue -> parseArray/parseInlineTable
const maxValueDepth = 1000

// DefaultMaxTables bounds the maps one document creates. Every dotted-key
// segment and header part is a map of about 350 bytes, so without a bound a
// 10 MiB file of dotted keys holds near 2 GB.
const DefaultMaxTables = 1 << 16

func NewParser(input []byte) *Parser {
	l := NewLexer(input)
	p := &Parser{
		lexer:  l,
		root:   make(map[string]any),
		frozen: make(map[uintptr]bool),
	}
	p.nextToken()
	p.nextToken()
	p.current = p.root
	return p
}

// newTable makes a map, counted against the document's table budget
func (p *Parser) newTable() (map[string]any, error) {
	p.tables++
	if limit := cmp.Or(p.MaxTables, DefaultMaxTables); p.tables > limit {
		return nil, fmt.Errorf("document exceeds %d tables at line %d", limit, p.curToken.Line)
	}
	return make(map[string]any), nil
}

func (p *Parser) nextToken() {
	p.curToken = p.peekToken
	p.peekToken = p.lexer.NextToken()

	// Skip comments automatically
	for p.peekToken.Type == TokenComment {
		p.peekToken = p.lexer.NextToken()
	}
}

func (p *Parser) Parse() (map[string]any, error) {
	if !utf8.Valid(p.lexer.input) {
		return nil, fmt.Errorf("invalid UTF-8")
	}
	for p.curToken.Type != TokenEOF {
		if p.curToken.Type == TokenNewline {
			p.nextToken()
			continue
		}

		if err := p.parseStatement(); err != nil {
			return nil, err
		}
		if p.curToken.Type != TokenNewline && p.curToken.Type != TokenEOF {
			return nil, fmt.Errorf("expected newline at line %d", p.curToken.Line)
		}
	}
	return p.root, nil
}

func (p *Parser) parseStatement() error {
	switch p.curToken.Type {
	case TokenLBracket:
		// Table Definition: [table] or [[array.table]]
		return p.parseTableDeclaration()
	case TokenIdent, TokenString:
		// Key-Value Pair: key = value
		return p.parseKeyValuePair(p.current)
	case TokenError:
		return fmt.Errorf("lexing error line %d: %s", p.curToken.Line, p.curToken.Literal)
	default:
		return fmt.Errorf("unexpected token line %d: %s", p.curToken.Line, p.curToken.String())
	}
}

// parseTableDeclaration handles [key] and [[key]]; the doubled brackets of
// an array of tables are one delimiter, so "[ [a] ]" is no header
func (p *Parser) parseTableDeclaration() error {
	isArray := false
	if p.peekToken.Type == TokenLBracket && adjacent(p.curToken, p.peekToken) {
		// It is [[ ...
		p.nextToken() // consume first [
		isArray = true
	}
	p.nextToken() // consume [

	// Parse Key (dotted)
	keys, err := p.parseKeyParts()
	if err != nil {
		return err
	}

	if isArray {
		if p.curToken.Type != TokenRBracket || p.peekToken.Type != TokenRBracket || !adjacent(p.curToken, p.peekToken) {
			return fmt.Errorf("expected ]] closing array table at line %d", p.curToken.Line)
		}
		p.nextToken() // consume first ]
	}

	if p.curToken.Type != TokenRBracket {
		return fmt.Errorf("expected closing bracket for table at line %d", p.curToken.Line)
	}
	p.nextToken() // consume final ]

	// Define scope
	return p.setTableScope(keys, isArray)
}

// setTableScope navigates/creates the map structure and sets p.current
func (p *Parser) setTableScope(keys []string, isArrayOfTables bool) error {
	// Table declarations always start from root
	var ptr any = p.root

	for i, key := range keys {
		isLast := i == len(keys)-1
		currentMap, ok := ptr.(map[string]any)
		if !ok {
			return fmt.Errorf("key path conflict: %q is not a map", key)
		}

		if isLast {
			if isArrayOfTables {
				// [[a.b]] -> Ensure 'b' is a slice of maps, append new map, set cursor to it
				var slice []map[string]any
				if val, exists := currentMap[key]; exists {
					if s, ok := val.([]map[string]any); ok {
						slice = s
					} else {
						return fmt.Errorf("key conflict: %q is not an array of tables", key)
					}
				} else {
					slice = make([]map[string]any, 0)
				}

				newMap, err := p.newTable()
				if err != nil {
					return err
				}
				slice = append(slice, newMap)
				currentMap[key] = slice
				p.current = newMap
			} else {
				// [a.b] -> Ensure 'b' is a map, set cursor to it
				var targetMap map[string]any
				if val, exists := currentMap[key]; exists {
					if m, ok := val.(map[string]any); ok {
						// Inline tables cannot be reopened
						if p.frozen[reflect.ValueOf(m).Pointer()] {
							return fmt.Errorf("cannot extend inline table %q at line %d", key, p.curToken.Line)
						}
						targetMap = m
					} else {
						return fmt.Errorf("key conflict: %q is not a table", key)
					}
				} else {
					var err error
					if targetMap, err = p.newTable(); err != nil {
						return err
					}
					currentMap[key] = targetMap
				}
				p.current = targetMap
			}
		} else {
			// Intermediate key -> ensure map exists and traverse.
			// Traversal through an existing [[array]] descends into its last element.
			if val, exists := currentMap[key]; exists {
				if m, ok := val.(map[string]any); ok {
					// Inline tables cannot be extended via sub-tables
					if p.frozen[reflect.ValueOf(m).Pointer()] {
						return fmt.Errorf("cannot extend inline table %q at line %d", key, p.curToken.Line)
					}
					ptr = m
				} else if slice, ok := val.([]map[string]any); ok {
					if len(slice) == 0 {
						return fmt.Errorf("cannot traverse empty array table %q", key)
					}
					ptr = slice[len(slice)-1]
				} else {
					return fmt.Errorf("intermediate key %q is not a map", key)
				}
			} else {
				newMap, err := p.newTable()
				if err != nil {
					return err
				}
				currentMap[key] = newMap
				ptr = newMap
			}
		}
	}
	return nil
}

func (p *Parser) parseKeyValuePair(scope any) error {
	// Parse Key (dotted allowed: a.b.c = 1)
	keys, err := p.parseKeyParts()
	if err != nil {
		return err
	}

	if p.curToken.Type != TokenEqual {
		return fmt.Errorf("expected '=' after key at line %d, got %s", p.curToken.Line, p.curToken.String())
	}
	p.nextToken() // consume =

	val, err := p.parseValue()
	if err != nil {
		return err
	}

	// Assign value to scope
	return p.assignValue(scope, keys, val)
}

func (p *Parser) assignValue(scope any, keys []string, val any) error {
	ptr := scope

	// If scope is map, easy. If scope is not map, error.
	currentMap, ok := ptr.(map[string]any)
	if !ok {
		return fmt.Errorf("scope is not a map")
	}

	for i, key := range keys {
		if i == len(keys)-1 {
			// Final key, assign value
			if _, exists := currentMap[key]; exists {
				return fmt.Errorf("duplicate key %q at line %d", key, p.curToken.Line)
			}
			currentMap[key] = val
		} else {
			// Intermediate, ensure map
			if existing, exists := currentMap[key]; exists {
				if m, ok := existing.(map[string]any); ok {
					if p.frozen[reflect.ValueOf(m).Pointer()] {
						return fmt.Errorf("cannot extend inline table %q at line %d", key, p.curToken.Line)
					}
					currentMap = m
				} else {
					return fmt.Errorf("intermediate key %q is not a map", key)
				}
			} else {
				newMap, err := p.newTable()
				if err != nil {
					return err
				}
				currentMap[key] = newMap
				currentMap = newMap
			}
		}
	}
	return nil
}

func (p *Parser) parseKeyParts() ([]string, error) {
	var keys []string
	for {
		// Rule: Tokens identified as Numbers are forbidden as keys
		if p.curToken.Type == TokenInteger || p.curToken.Type == TokenFloat {
			return nil, fmt.Errorf("numeric keys are forbidden: %s", brief(p.curToken.Literal))
		}

		if p.curToken.Type == TokenString {
			// Rule: Even quoted strings shouldn't be pure numbers per instruction
			if numericKey(p.curToken.Literal) {
				return nil, fmt.Errorf("numeric string keys are forbidden: %s", brief(p.curToken.Literal))
			}
		}

		if p.curToken.Type != TokenIdent && p.curToken.Type != TokenString {
			return nil, fmt.Errorf("expected key, got %s", p.curToken.String())
		}

		if len(keys) >= maxValueDepth {
			return nil, fmt.Errorf("key nesting exceeds %d", maxValueDepth)
		}
		keys = append(keys, p.curToken.Literal)
		p.nextToken()

		if p.curToken.Type == TokenDot {
			p.nextToken()
			continue
		}
		break
	}
	return keys, nil
}

func (p *Parser) parseValue() (any, error) {
	// Guard unbounded recursion on nested
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > maxValueDepth {
		return nil, fmt.Errorf("value nesting exceeds %d at line %d", maxValueDepth, p.curToken.Line)
	}

	switch p.curToken.Type {
	case TokenString:
		val := p.curToken.Literal
		p.nextToken()
		return val, nil
	case TokenInteger:
		val, err := p.parseInteger(p.curToken.Literal)
		if err != nil {
			return nil, fmt.Errorf("invalid integer %s at line %d: %w", brief(p.curToken.Literal), p.curToken.Line, numCause(err))
		}
		p.nextToken()
		return val, nil
	case TokenFloat:
		val, err := strconv.ParseFloat(p.curToken.Literal, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid float %s at line %d: %w", brief(p.curToken.Literal), p.curToken.Line, numCause(err))
		}
		p.nextToken()
		return val, nil
	case TokenBool:
		val := p.curToken.Literal == "true"
		p.nextToken()
		return val, nil
	case TokenLBracket:
		return p.parseArray()
	case TokenLBrace:
		return p.parseInlineTable()
	}
	if p.curToken.Type == TokenIdent {
		// An unquoted value may be a secret: say what it is, not what it says
		return nil, fmt.Errorf("unquoted value at line %d: strings need quotes", p.curToken.Line)
	}
	return nil, fmt.Errorf("unexpected value token %s at line %d", p.curToken.String(), p.curToken.Line)
}

func (p *Parser) parseInteger(lit string) (int64, error) {
	// Handle optional leading sign
	negative := false
	numLit := lit
	if len(numLit) > 0 && (numLit[0] == '+' || numLit[0] == '-') {
		negative = numLit[0] == '-'
		numLit = numLit[1:]
	}

	var val int64
	var err error

	if len(numLit) > 2 && numLit[0] == '0' {
		switch numLit[1] {
		case 'x', 'X':
			val, err = strconv.ParseInt(signPrefix(negative)+numLit[2:], 16, 64)
		case 'o', 'O':
			val, err = strconv.ParseInt(signPrefix(negative)+numLit[2:], 8, 64)
		case 'b', 'B':
			val, err = strconv.ParseInt(signPrefix(negative)+numLit[2:], 2, 64)
		default:
			val, err = strconv.ParseInt(lit, 10, 64)
			return val, err
		}
		if err != nil {
			return 0, err
		}
		return val, nil
	}

	val, err = strconv.ParseInt(lit, 10, 64)
	return val, err
}

func (p *Parser) parseArray() ([]any, error) {
	p.nextToken() // consume [
	arr := make([]any, 0)

	for p.curToken.Type != TokenRBracket {
		if p.curToken.Type == TokenNewline {
			p.nextToken()
			continue
		}

		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)

		for p.curToken.Type == TokenNewline {
			p.nextToken()
		}
		if p.curToken.Type == TokenComma {
			p.nextToken()
		} else if p.curToken.Type != TokenRBracket {
			return nil, fmt.Errorf("expected comma or closing bracket in array at line %d", p.curToken.Line)
		}
	}
	p.nextToken() // consume ]
	return arr, nil
}

func (p *Parser) parseInlineTable() (map[string]any, error) {
	p.nextToken() // consume {
	m, err := p.newTable()
	if err != nil {
		return nil, err
	}

	for p.curToken.Type != TokenRBrace {
		if p.curToken.Type == TokenNewline {
			p.nextToken()
			continue
		}

		// Parse key = value
		keys, err := p.parseKeyParts()
		if err != nil {
			return nil, err
		}

		if p.curToken.Type != TokenEqual {
			return nil, fmt.Errorf("expected '=' in inline table at line %d", p.curToken.Line)
		}
		p.nextToken()

		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}

		// Inline tables can have dotted keys too: { a.b = 1 }
		if err := p.assignValue(m, keys, val); err != nil {
			return nil, err
		}

		for p.curToken.Type == TokenNewline {
			p.nextToken()
		}
		if p.curToken.Type == TokenComma {
			p.nextToken()
		} else if p.curToken.Type != TokenRBrace {
			return nil, fmt.Errorf("expected comma or closing brace in inline table at line %d", p.curToken.Line)
		}
	}
	p.nextToken() // consume }

	// Mark inline table immutable. Value.Pointer for maps is documented
	// stable for identity comparison. Nested inline tables self-mark on return;
	// same-table dotted assignments happen before the mark, so intra-table
	// dotted keys ({a.b = 1}) remain unaffected.
	p.frozen[reflect.ValueOf(m).Pointer()] = true
	return m, nil
}

// adjacent reports b starting right after the one-byte token a
func adjacent(a, b Token) bool { return a.Line == b.Line && b.Col == a.Col+1 }

// brief quotes s for an error, cut to 32 bytes: a literal can be megabytes
func brief(s string) string {
	if len(s) > 32 {
		return fmt.Sprintf("%q... (%d bytes)", s[:32], len(s))
	}
	return fmt.Sprintf("%q", s)
}

// numCause drops strconv's copy of the literal, which brief already shows
func numCause(err error) error {
	if ne, ok := errors.AsType[*strconv.NumError](err); ok {
		return ne.Err
	}
	return err
}

func signPrefix(negative bool) string {
	if negative {
		return "-"
	}
	return ""
}

// Keep the package's decimal-key restriction independent of numeric range.
func numericKey(s string) bool {
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for _, c := range s {
		if !isDigit(c) {
			return false
		}
	}
	return true
}
