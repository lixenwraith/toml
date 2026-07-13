# toml

Zero-dependency TOML encoder/decoder for Go.
Implements a practical subset of TOML v1.0 with documented deviations.
Standard library only.

## Install

    go get github.com/lixenwraith/toml

## Usage

### Unmarshal

```go
input := []byte(`
initial = "Idle"

[states.Idle]
parent = "Root"
transitions = [
    { trigger = "Start", target = "Active" }
]
`)

type Transition struct {
    Trigger string `toml:"trigger"`
    Target  string `toml:"target"`
    Guard   string `toml:"guard,omitempty"`
}
type State struct {
    Parent      string       `toml:"parent"`
    Transitions []Transition `toml:"transitions"`
}
type Config struct {
    Initial string            `toml:"initial"`
    States  map[string]*State `toml:"states"`
}

var cfg Config
err := toml.Unmarshal(input, &cfg)
```

### Marshal

```go
out, err := toml.Marshal(cfg) // deterministic: keys sorted alphabetically
```

### Decode

`Decode(data any, v any)` maps an already-parsed `map[string]any` onto a
target. `Unmarshal` = `Parse` + `Decode`.

## Architecture

    Unmarshal: []byte → Lexer → tokens → Parser → map[string]any → Decode (reflection) → target
    Marshal:   value → encoder (reflection, two-pass) → []byte

### Lexer (`lexer.go`)

- Single-pass, UTF-8 aware byte scanner with rune lookahead (`peekAt`).
- Emits: ident, string, integer, float, bool, punctuation, newline, comment.
- Number/key disambiguation happens at the token level: `1.5` → Float,
  `a.b` → Ident Dot Ident, `1.2.3` → Error (multi-dot). Hex/octal/binary
  prefixes and exponent forms are validated during scanning.
- Basic strings only. Escapes: `\"` `\\` `\n` `\t` `\r`. Unknown escape
  sequences are preserved verbatim (spec requires an error — see Deviations).

### Parser (`parser.go`)

- Recursive descent with a two-token window (`curToken`/`peekToken`).
  Comments are skipped during token advance.
- Output model: tables → `map[string]any`, arrays → `[]any`,
  arrays of tables → `[]map[string]any`.
- Table headers (`[a.b]`) always resolve from the root; a `current` cursor
  tracks the active table scope for subsequent key/value pairs.
- Dotted keys create intermediate maps. Traversal through an existing
  `[[array]]` descends into its last element (TOML semantics).
- Conflict detection at assignment: duplicate keys, scalar-vs-table
  redefinition, table-vs-array-of-tables shadowing. Numeric keys are
  rejected in all positions (bare, quoted, dotted segments).
- Inline tables are immutable (later dotted keys or `[headers]` targeting them error); value nesting (arrays/inline tables) capped at 1000 levels.

### Decoder (`decode.go`)

- Kind-switch reflection. Pointers are auto-allocated at any depth
  (`******int` works). Nil maps and slices are materialized.
- Struct fields match by `toml` tag first, then exact (case-sensitive)
  field name. `toml:"-"` skips. Unexported fields are skipped.
- Numeric coercion: any parser-produced int/uint/float64 converts to the
  target numeric kind. Unknown keys in input are ignored.
- Numeric decode errors on target overflow (`300` → `int8` errors); non-empty interface targets error unless assignable; unsupported target kinds (chan, func, complex, fixed arrays) error; `uint`/`uint64` sources above `MaxInt64` are rejected.

### Encoder (`encode.go`)

- Two-pass emission per table: scalars and inline arrays first, then
  `[tables]` and `[[arrays of tables]]`. Guarantees keys are defined
  before sub-tables, i.e. output is always valid TOML.
- Keys sorted alphabetically → deterministic output for diffing/VCS.
- Bare-key validation mirrors the lexer's rules: keys that would lex as
  numbers or booleans (`true`, `123a`, `-1x`) are quoted, so output always
  re-parses (round-trip safe).
- `omitempty` honored; nil pointers and `toml:"-"` fields skipped;
  root must be a struct or map.
- Floats use shortest `'g'` form at the source bit width (large magnitudes emit exponent notation); `NaN`/`±Inf` return an error; arrays mixing tables and scalars return an error (parser accepts them — encode-side limitation); struct keys sort by resolved tag name.

## Type Mapping

| TOML            | Parser output (`map[string]any`) | Decode targets                    |
|-----------------|----------------------------------|-----------------------------------|
| string          | `string`                         | `string`, `any`                   |
| integer         | `int`                            | any int/uint kind, float kinds    |
| float           | `float64`                        | `float32`, `float64`              |
| boolean         | `bool`                           | `bool`                            |
| array           | `[]any`                          | `[]T`, `any`                      |
| table           | `map[string]any`                 | struct, `map[string]T`, `any`     |
| array of tables | `[]map[string]any`               | `[]T`, `[]*T`                     |

## Deviations from TOML v1.0

Intentional restrictions:
- Numeric keys are forbidden everywhere: `123 = 1`, `[456]`, `"789"`,
  `a.1.b` all error. Stricter than spec.

Extensions (accepted, though spec forbids):
- Newlines inside inline tables (multi-line inline tables parse).
- Trailing commas in inline tables.

Relaxations:
- Unknown string escapes are preserved instead of erroring.
- Explicit table redefinition (`[a]` … `[a]`) is not rejected; the second
  header reopens the table. Value/table conflicts are still caught.

## Limitations

Not supported:
- Date/time types (all four TOML forms). Workaround: store RFC 3339
  strings and convert with `time.Parse` at the call site; strings
  round-trip cleanly.
- Literal strings `'...'` and multi-line strings `"""..."""`, `'''...'''`.
- Underscore digit separators (`1_000`).
- Arrays mixing tables and scalar values are not encodable.
- `inf` / `nan` float literals.
- Custom marshaling interfaces (`Marshaler`/`Unmarshaler`,
  `encoding.TextMarshaler`).
- Embedded struct field promotion in decode.

Platform note: TOML integers parse to Go `int`; 64-bit platforms assumed (amd64/arm64) and enforced at compile time (32-bit build fails).
Overflow of int64 during parse errors; 32-bit truncation of `int(int64)` is not guarded.

## License

BSD-3-Clause (see LICENSE).
