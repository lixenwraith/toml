package toml

import (
	"cmp"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"unicode/utf8"
)

// Parser parses TOML tokens into a map[string]any
type Parser struct {
	root    map[string]any
	lexer   Lexer
	current map[string]any // the table keys are assigned to
	// Inline tables a key's value made, immutable per TOML spec: a few in place,
	// a set past them. One in an array no key can reach, so it is not held.
	frozen    [4]uintptr
	nfrozen   int
	frozenSet map[uintptr]bool
	curToken  Token
	peekToken Token
	// Value nesting depth (arrays / inline tables)
	depth int
	// Tables (maps) a document may create; 0 means DefaultMaxTables
	MaxTables int
	tables    int
	// Elements of the arrays being parsed, an inner array's above its parent's,
	// so each array is one allocation of its final length
	values []any
}

// Recursion bound for parseValue -> parseArray/parseInlineTable
const maxValueDepth = 1000

// DefaultMaxTables bounds the maps one document creates. Every dotted-key
// segment and header part is a map of about 350 bytes, so without a bound a
// 10 MiB file of dotted keys holds near 2 GB.
const DefaultMaxTables = 1 << 16

func NewParser(input []byte) *Parser {
	p := &Parser{lexer: Lexer{input: input, line: 1, lean: true}, root: make(map[string]any)}
	p.current = p.root
	p.nextToken()
	p.nextToken()
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
		return fmt.Errorf("unexpected token line %d: %s", p.curToken.Line, p.show(p.curToken))
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
	var buf [4]string
	keys, err := p.parseKeyParts(buf[:0])
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
						if p.isFrozen(m) {
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
					if p.isFrozen(m) {
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

func (p *Parser) parseKeyValuePair(scope map[string]any) error {
	// Parse Key (dotted allowed: a.b.c = 1)
	var buf [4]string
	keys, err := p.parseKeyParts(buf[:0])
	if err != nil {
		return err
	}

	if p.curToken.Type != TokenEqual {
		return fmt.Errorf("expected '=' after key at line %d, got %s", p.curToken.Line, p.show(p.curToken))
	}
	p.nextToken() // consume =

	val, err := p.parseValue()
	if err != nil {
		return err
	}
	return p.assignValue(scope, keys, val)
}

// assignValue sets keys under scope to val, freezing val if it is an inline table
func (p *Parser) assignValue(scope map[string]any, keys []string, val any) error {
	currentMap := scope
	for i, key := range keys {
		if i == len(keys)-1 {
			// Final key, assign value
			if _, exists := currentMap[key]; exists {
				return fmt.Errorf("duplicate key %q at line %d", key, p.curToken.Line)
			}
			currentMap[key] = val
			if m, ok := val.(map[string]any); ok {
				p.freeze(m)
			}
		} else {
			// Intermediate, ensure map
			if existing, exists := currentMap[key]; exists {
				if m, ok := existing.(map[string]any); ok {
					if p.isFrozen(m) {
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

// freeze marks an inline table no later key may extend
func (p *Parser) freeze(m map[string]any) {
	k := reflect.ValueOf(m).Pointer()
	if p.frozenSet == nil && p.nfrozen < len(p.frozen) {
		p.frozen[p.nfrozen] = k
		p.nfrozen++
		return
	}
	if p.frozenSet == nil {
		p.frozenSet = make(map[uintptr]bool)
		for _, f := range p.frozen {
			p.frozenSet[f] = true
		}
	}
	p.frozenSet[k] = true
}

func (p *Parser) isFrozen(m map[string]any) bool {
	k := reflect.ValueOf(m).Pointer()
	if p.frozenSet != nil {
		return p.frozenSet[k]
	}
	return slices.Contains(p.frozen[:p.nfrozen], k)
}

// parseKeyParts appends a dotted key's parts to keys, which callers keep on
// their stack
func (p *Parser) parseKeyParts(keys []string) ([]string, error) {
	for {
		// Rule: Tokens identified as Numbers are forbidden as keys
		if p.curToken.Type == TokenInteger || p.curToken.Type == TokenFloat {
			return nil, fmt.Errorf("numeric keys are forbidden: %s", brief(string(p.lexer.input[p.curToken.Pos:p.curToken.End])))
		}

		if p.curToken.Type == TokenString {
			// Rule: Even quoted strings shouldn't be pure numbers per instruction
			if numericKey(p.curToken.Literal) {
				return nil, fmt.Errorf("numeric string keys are forbidden: %s", brief(p.curToken.Literal))
			}
		}

		if p.curToken.Type != TokenIdent && p.curToken.Type != TokenString {
			return nil, fmt.Errorf("expected key, got %s", p.show(p.curToken))
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
		lit := p.lexer.input[p.curToken.Pos:p.curToken.End]
		val, err := parseInteger(lit)
		if err != nil {
			return nil, fmt.Errorf("invalid integer %s at line %d: %w", brief(string(lit)), p.curToken.Line, numCause(err))
		}
		p.nextToken()
		return val, nil
	case TokenFloat:
		lit := p.lexer.input[p.curToken.Pos:p.curToken.End]
		val, err := strconv.ParseFloat(string(lit), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid float %s at line %d: %w", brief(string(lit)), p.curToken.Line, numCause(err))
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
	return nil, fmt.Errorf("unexpected value token %s at line %d", p.show(p.curToken), p.curToken.Line)
}

// parseInteger reads a lexed integer: a prefixed one as strconv's base 0 does, the
// rest in base 10, so a leading zero is not octal. Neither keeps lit.
func parseInteger(lit []byte) (int64, error) {
	digits := lit
	if len(digits) > 0 && (digits[0] == '+' || digits[0] == '-') {
		digits = digits[1:]
	}
	if len(digits) > 2 && digits[0] == '0' && !isDigit(rune(digits[1])) {
		return strconv.ParseInt(string(lit), 0, 64)
	}
	return strconv.ParseInt(string(lit), 10, 64)
}

func (p *Parser) parseArray() ([]any, error) {
	p.nextToken() // consume [
	base := len(p.values)
	if p.values == nil {
		p.values = make([]any, 0, 8)
	}

	for p.curToken.Type != TokenRBracket {
		if p.curToken.Type == TokenNewline {
			p.nextToken()
			continue
		}

		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		p.values = append(p.values, val)

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
	arr := make([]any, len(p.values)-base)
	copy(arr, p.values[base:])
	clear(p.values[base:])
	p.values = p.values[:base]
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
		var buf [4]string
		keys, err := p.parseKeyParts(buf[:0])
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
	// The key this is the value of freezes it (assignValue), after the dotted
	// keys inside it ({a.b = 1}) built it
	return m, nil
}

// show is t as an error names it; a number's text, which the lexer leaves out,
// read back from the input
func (p *Parser) show(t Token) string {
	if t.Type == TokenInteger || t.Type == TokenFloat {
		t.Literal = string(p.lexer.input[t.Pos:t.End])
	}
	return t.String()
}

// adjacent reports b starting where a ends
func adjacent(a, b Token) bool { return b.Pos == a.End }

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
