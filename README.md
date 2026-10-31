# toml

A deterministic TOML subset encoder, decoder and formatter using Go standard library,
with a macro expander for parsed documents (`macro`, below). Requires Go 1.27.1.

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
and unexported fields are ignored; `Unmapped(data, &target)` names the keys a
parsed value holds that no field takes, and a caller refusing them returns
`UnknownKeys(keys)`, which quotes each. A decode error names where it failed by
the key path the document spells: `rooms[1].width: cannot convert string to int`.

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
and `\xHH` decode too. The encoder escapes the runes that can hide text, as
`Format` does (below). Invalid UTF-8 and unescaped control characters in
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

`Marshal` writes an inline table as `{ a = 1 }` and a blank line above each
header but one opening the document; its output is what `Format` writes for it.
Comments and original formatting are not preserved by `Parse`/`Marshal`; a
`Document` edits a file in place, and `Format` rewrites one with its comments
kept.

## Editing a document in place

```go
doc, err := toml.ParseDocument(data)
err = doc.Decode(&settings)
unknown := doc.Unmapped(&settings)         // []toml.Key no field takes
removed, err := doc.Delete(toml.Key{"play", "effects"})
err = doc.Set(toml.Key{"play", "sfx"}, false)
data = doc.Bytes()
```

`ParseDocument` parses as `Parse` does and keeps the bytes. An edit rewrites only
the statements it names, with the comments that are theirs, so blank lines,
order, every other comment and the spelling of every other value stay as
written; the result is parsed again, and an edit it would not parse after is
refused with the document unchanged.

A group of comment lines belongs to whichever side fewer blank lines part it
from, the side below on a tie: the statement right under it, or the run of
statements (lines no blank line parts) above or below it. The file's start and
end count as sides, so a comment at the top with a blank line under it, or at
the end with one above it, belongs to the file and stays, while one touching
the last statement is that statement's run's. Between two runs, several groups
split at the widest gap.

- `Set` replaces the value of a key the document sets, keeping the rest of its
  line and its comment. A key it lacks goes after the last key of its table,
  spelled as that key is (`play.mouse` beside `play.hud`), else after the table's
  header, else in a `[table]` appended at the end, above the file's closing
  comments. A root key with no other goes above the first table and its
  comments. A key under an array of tables, inside an inline table or array, or
  naming a table fails. The value is encoded as `Marshal` writes one.
- `Delete` removes the key and every key under it: each statement setting one and
  each header naming one, in every table of an array of tables, each with its
  comments. A run of statements going whole takes the comments that belong to
  it and one blank run beside it. It reports whether it removed anything; a key
  inside an inline value fails.
- `Unmapped` names the keys no field of the destination takes, matched as `Decode`
  matches them (tags, then field names; unexported and `-` fields take nothing),
  each once and sorted. A map or interface destination takes every key under it;
  a key under an array of tables is named without its index. The function of the
  same name reads any value `Parse` produced, a table inside one included.
- `Key` is a dotted path, one segment per part; `Key.String` spells it as a
  document would, quoting segments that need it.

A token's `Pos` and `End` are its byte offsets in the input.

## Formatting

```go
out, err := toml.Format(data)
out, err = toml.Formatter{Indent: "  ", IndentTables: true, Align: true}.Format(data)
```

`Format` writes a document spaced as `Marshal` writes one: one space around `=`
and after a comma, `[1, 2]`, `{ a = 1 }`, `[a.b]`, LF line ends. A value's lines
go a level deeper than the line its opening bracket is on, and an array or
inline table has a trailing comma exactly when its closing bracket has a line of
its own. Keys, scalars and comments keep their spelling; a run of blank lines
becomes one.

Every comment stays with what it belongs to, as a `Document` reads it, and the
result's blank lines say so again: two blank lines follow a group of comments
that belongs to what is above it, after a blank line. Each table is written
once, at its first header. The keys of a header opening a table again go there,
a blank line apart, the header's comments above them; a dotted key reaching a
table that has a header moves into it, spelled from it, with its own comments.
Tables of an array of tables stay as written.

- `Indent` is one level, up to 8 tabs or spaces; a tab when empty.
- `IndentTables` puts a table a level deeper for each table it is under whose
  header comes before it: `[a.b]` after `[a]`.
- `Align` lines up the `=` and the comments of keys on consecutive lines, with
  spaces. A comment or blank line, a value of several lines, a key over 32 runes
  or a line over 80 ends the run.

A line indents no deeper than 16 levels, so a result stays within a fixed
multiple of its source. `Format` refuses what `Parse` refuses, and a comment
holding a bidirectional embedding, override or isolate (U+202A to U+202E, U+2066
to U+2069) or a line break other than LF (U+0085, U+2028, U+2029): such a rune
can show a reader text other than what parses, and a comment has no escape for
it. In a string or quoted key it is written as its `\u` escape. A result that
would not parse to the same values is refused.

`Parse`, `Unmarshal` and `Decode` never format: they skip a comment without
reading its text.

## Macros

```go
import "github.com/lixenwraith/toml/macro"

doc, err := toml.NewParser(data).Parse()
doc, err = macro.ExpandDocument(doc) // a document that defines its own macros
err = toml.Decode(doc, &target)

set := macro.NewSet()       // one set shared by several documents
err = set.Define(library)   // takes library's [macros]; a name defined twice is an error
doc, err = set.Expand(doc)  // doc defines none; its invocations use the set
```

A macro is data in the document, expanded on the parsed tree between `Parse`
and `Decode`, so nothing downstream of decoding sees anything but plain tables.
A macro is a `[macros.<Name>]` table:

- `params`: parameter name to type: `int`, `float` (an integer is accepted),
  `bool`, `string`, `table`, `array` or `any`; `macro` and `each` are reserved.
- `defaults`: values for optional parameters; every other parameter is required.
- `doc`: one line for authors.
- `body`: any value: a table, an array, a string or a number.

Inside a body, `${name}` names a parameter, in a string or in a key. A string
that is exactly one reference becomes the argument itself, of any type;
elsewhere the argument is spliced into the text and must be a string, an integer
or a boolean. `$$` is a literal `$`; any other `$` in a body is an error. Only
bodies are templates: an argument's own text is never scanned.

Any table with a `macro` key is an invocation; its other keys are arguments,
checked against the parameters' types. As a value it is replaced by the
expansion. As an array element, an array expansion is spliced into the array and
any other is one element; with `each`, a table of parameter = array, all of one
length, it expands once per zipped element. Bodies may invoke other macros and
arguments may be invocations: arguments are expanded where they are written,
then bound, then the body is substituted and expanded in turn.

```toml
[macros.Note]
doc = "One note: pos, deg, dur in steps, vel"
params = { pos = "int", deg = "int", dur = "int", vel = "float" }
defaults = { dur = 1, vel = 0.5 }
body = { pos = "${pos}", deg = "${deg}", dur = "${dur}", vel = "${vel}" }

[[track]]
instr = "lead"
event = [{ macro = "Note", each = { pos = [0, 4, 8], deg = [0, 2, 4] } }]
```

Definitions are checked when read: an unknown key, an unknown type, a default of
the wrong type and a `${name}` naming no parameter are errors before any use.
Expansion is a pure function of the documents given, so equal bytes expand to
equal trees. A macro reached again through its own expansion, nesting deeper
than 32, and more than 2^18 values produced through one set are errors, and an
error names the invocation path through every macro involved. `Expand` refuses
a document holding `[macros]` that `Define` has not taken, so a file meant only
to invoke cannot define. Arrays come back as `[]any`, whatever form the parser
gave them.

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
