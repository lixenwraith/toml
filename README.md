# toml

A deterministic TOML subset encoder/decoder using Go standard library.
Requires Go 1.27.1.

```go
type Settings struct {
    Name string `toml:"name"`
    Port uint16 `toml:"port"`
}
var settings Settings
err := toml.Unmarshal([]byte("name = \"service\"\nport = 8080\n"), &settings)
data, err := toml.Marshal(settings)
```

`NewParser(data).Parse()` returns `map[string]any`. `Decode(data, &target)`
converts an already parsed value. Errors leave the destination unchanged.
Missing struct fields retain their existing values; present maps and slices
replace old containers. Existing pointer defaults are retained without writing
through the original pointer. Fixed arrays require an exact input length.
Tags use `toml:"name,omitempty"`; an empty tag name
falls back to the Go field name. Matching is case sensitive. Unknown input keys
and unexported fields are ignored.

## Values and conversions

| TOML value | Parsed Go value | Typed destinations |
| --- | --- | --- |
| Integer | `int64`, on every architecture | Checked integer and float types |
| Float | `float64` | Float types; integers only when integral and in range |
| String | `string` | `string`, `any` |
| Boolean | `bool` | `bool`, `any` |
| Array | `[]any` | Slices, fixed arrays, `any` |
| Table | `map[string]any` | Structs, string-keyed maps, `any` |
| Array of tables | `[]map[string]any` | Slices and fixed arrays, including pointer elements |

Direct `Decode` supports all signed/unsigned numeric widths and named numeric
types, including the full `uint64` range. Conversions reject overflow, negative
unsigned inputs, fractional-to-integer truncation, non-finite floats, and
integer-to-float precision loss. Float64-to-float32 rounding is allowed when in
range. Strings are not coerced to numbers by this package.

`Decode` is transactional, not a deep-copy or synchronization API. Interface
destinations retain input values (including map and slice references); unchanged
fields may retain existing references. A nil input is a no-op. Callers own
snapshot isolation and synchronization when values are shared between goroutines.

TOML integer literals have a signed 64-bit range. Consequently `Marshal` rejects
unsigned values above `MaxInt64`; callers may explicitly encode decimal strings
and interpret those in their application. Integer parsing and narrow destination
checks also work on 32-bit systems.

## Encoding and round trips

`Marshal` accepts a struct or string-keyed map, including pointers to them. Keys
are sorted. Scalar fields precede nested tables. Table path segments are quoted
individually, preserving dots, spaces, quotes and Unicode in keys. Mixed arrays
and nested table values use inline tables when needed. Struct `omitempty` is
honored; nil pointer/interface fields are omitted. Nil pointer/interface array
elements error, because TOML has no null value. Nil maps and slices encode as
empty containers. Duplicate struct tag names error.

Basic-string escapes emitted by the encoder (`\b`, `\f`, `\n`, `\r`, `\t`,
quotes, backslashes, Unicode escapes) decode symmetrically; the TOML 1.1 `\e`
and `\xHH` decode too. Invalid UTF-8 and unescaped control characters in
strings are rejected, as are control characters other than tab in comments and
a carriage return outside CRLF. Encoding and typed decoding
have recursion bounds (1000 levels), so cyclic traversals fail instead of
overflowing the stack. Parsing bounds individual value nesting and dotted paths;
combining paths can still produce trees that exceed the encoder's bound. A
document may create at most `DefaultMaxTables` (65536) tables, counting every
dotted-key segment and header part; set `Parser.MaxTables` before `Parse` to
change it. Errors quote keys and cut long literals, and never repeat a string
value. `Marshal` writes a table inline once its header path would pass 256
bytes, so output stays linear in depth.

Comments and original formatting are not preserved by `Parse`/`Marshal`. A
configuration editor can scan the original bytes with `NewLexer` and retain
`TokenComment.Literal` values, then prefix each with `#` and write them as a
preamble before the marshaled data. This preserves comment text (including
inline comments), but not its original location. Hashes inside strings are not
comment tokens. Stop scanning on `TokenEOF` or `TokenError`.

## Deliberate subset and deviations

- Datetime syntax is unsupported. Use quoted strings and parse them in the
  application; this package does not handle `time.Time` or custom marshal hooks.
- Literal strings and multiline strings, digit separators, and `inf`/`nan` are
  unsupported.
- Decimal integer keys (including quoted keys, optional signs and values beyond
  int64) are forbidden. This restriction also applies to the encoder.
- String escapes invalid in every TOML version are preserved verbatim as a
  documented relaxation.
- Explicit table headers may reopen existing tables, and dotted keys may extend
  a table a header defined. Inline tables cannot be extended later.
- Inline tables allow newlines and trailing commas. Arrays require commas,
  including between lines. Top-level statements require newline separation.
- Signed radix integers and uppercase radix prefixes are accepted extensions.
- Leading zeros are accepted in decimal numbers; `0123` means decimal 123,
  not octal. Use `0o123` for octal.
- Embedded struct fields are not promoted. Custom marshal interfaces are not
  supported. Decode input containers are the parser's generic maps/slices.

## License

BSD-3-Clause
