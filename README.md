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
are sorted, but a table holding `macro`, a macro invocation, opens with it and
then with `each`, as one is written. Scalar fields precede nested tables. Table path segments are quoted
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
change it. Errors quote keys, cut a key or a literal past 32 bytes, and never
repeat a string value. `Marshal` writes a table inline once its header path would pass 256
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
own := doc.Comments(toml.Key{"pattern"})   // the comments above each [[pattern]]
err = doc.Set(toml.Key{"play", "sfx"}, false)
added, err := doc.Fill(shipped)             // keys a newer template sets, with their comments
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
  comments, or with no table above the file's closing comments. A key under an
  array of tables, inside an inline table or array, or naming a table fails. The
  value is encoded as `Marshal` writes one.
- `Delete` removes the key and every key under it: each statement setting one and
  each header naming one, in every table of an array of tables, each with its
  comments. A run of statements going whole takes the comments that belong to
  it and one blank run beside it. It reports whether it removed anything; a key
  inside an inline value fails.
- `Comments` gives what `Delete` takes from above each `[[key]]` header of an
  array of tables, one element a header in the document's order, so element i
  is table i of the array in `Tree`: the comment lines that are that table's
  own, as written, each run of blank lines among or under them cut to one. A
  table with none gives nil, as does a key that is not an array of tables
  written with headers. A caller that deletes the array and writes its tables
  anew puts each element under one blank line and above its table's header:
  the comments are that table's again, a tie going below, and doing it twice
  changes no byte. A comment inside a table (above a key or a sub-table's
  header, under its last key, at a line's end) is not returned and is lost; one
  that ends what is left of the document, a blank line above it, becomes the
  first table's.
- `Fill` adds each key another document sets that this one does not, as that
  one writes it with the comments that are its own there, and names them: a
  newer template's keys reach a file a person edited, every line of the file
  kept. A key goes after the one the template sets before it in its table when
  the file sets that, else after its table's header, else where `Set` puts it;
  a table the file lacks comes as its header and that header's comments, where
  `Set` puts a new table. A value the file holds at a key or over it (its own,
  an inline table or an array of tables) stays the file's; the template's arrays
  of tables are left out. The edit is one splice and one parse, linear in both.
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
- `base` and `with`, in place of `body`: the macro is written over another
  (below).

Inside a body, `${name}` names a parameter, in a string or in a key. A string
that is exactly one reference becomes the argument itself, of any type;
elsewhere the argument is spliced into the text and must be a string, an integer
or a boolean. `$$` is a literal `$`; any other `$` in a body is an error. Only
bodies are templates: an argument's own text is never scanned.

Any table with a `macro` key is an invocation; its other keys are arguments,
checked against the parameters' types. Bodies may invoke other macros and
arguments may be invocations: arguments are expanded where they are written,
then bound, then the body is substituted and expanded in turn. Where an
invocation stands decides what it becomes:

- a value: the expansion;
- an array element: an array expansion spliced into the array, any other one
  element;
- an array element holding `each`: an expansion for each element of the arrays
  in `each`, every one spliced or appended so. `each` anywhere else is an error.

`each` is a table of arguments, at least one, read once they are expanded:

- An array is zipped: an element to an expansion, each checked against the
  parameter's type. The arrays are all of one length.
- Any other value is shared: every expansion takes it, exactly as it takes an
  argument written beside `each`, and it is checked against the parameter's
  type itself.
- With no array in it, `each` expands once.
- A parameter named in `each` and also given beside it is an error.
- An array in `each` is always zipped, whatever its parameter's type: one that
  every expansion takes whole, for an `array` or `any` parameter, is written
  beside `each`.

```toml
[macros.Note]
doc = "One note: pos, deg, dur in steps, vel"
params = { pos = "int", deg = "int", dur = "int", vel = "float" }
defaults = { dur = 1, vel = 0.5 }
body = { pos = "${pos}", deg = "${deg}", dur = "${dur}", vel = "${vel}" }

[[track]]
instr = "lead"
event = [{ macro = "Note", each = { pos = [0, 4, 8], deg = [0, 2, 4], vel = 0.8 } }]
```

A default is checked as written and expands where it is used, so one that
invokes is a `table` or `any` parameter's. A document built in Go may hold any
integer or float type where the parser gives `int64` and `float64`.

A macro that is one invocation of another is written over it: `base` names the
other, `with` holds the arguments it fixes.

```toml
[macros.Line]
doc = "A line on instr: a note per pos and deg, at one vel or at one each"
params = { instr = "string", pos = "array", deg = "array", vel = "any", follow = "bool" }
defaults = { vel = 0.5, follow = true }
body = { instr = "${instr}", follow_chord = "${follow}", event = [
  { macro = "Note", each = { pos = "${pos}", deg = "${deg}", vel = "${vel}" } },
] }

[macros.Lead]
doc = "A lead line in absolute degrees of the key"
base = "Line"
defaults = { instr = "lead" }
with = { follow = false }

[macros.Stab]
doc = "One note of the key on a beat"
base = "Lead"
params = { beat = "int", deg = "int" }
with = { pos = ["${beat}"], deg = ["${deg}"] }
```

- A definition has `body` or `base`, never both, and `with` only beside `base`.
- `base` is a macro's name as written, never a template: one of the same
  document's macros, whatever the order of their names, or one the set already
  holds. A base a later `Define` brings is unknown, and a macro that is its own
  base through any chain is an error.
- `with` holds arguments of the base and nothing else: each key is a parameter
  of the base, so neither `macro` nor `each`.
- The macro's parameters are the base's that `with` does not set, left open
  with the base's type and default, and those its own `params` declares: new
  names, or names `with` sets, where the base's parameter is fixed and the
  macro's own stands under the name, of whatever type it declares (`deg` above,
  an array to `Lead` and an integer to `Stab`). Declaring a name the base leaves
  open is an error.
- `defaults` gives or replaces the default of any of those parameters, checked
  against that parameter's type.
- `with` is a template as a body is: `${name}` names any of the macro's
  parameters. A value in it written without `$` that invokes nothing is checked
  against the base's type when the definition is read; any other is checked at
  expansion, as an argument is.
- The macro expands exactly as the body `{ macro = <base>, <with>, p = "${p}"
  for each open parameter p }` would: at that body's cost, one invocation deeper
  than its base, and refused where that body is, the error naming `with` for
  `body`. An open parameter left out is so passed its default, and one that
  invokes expands there.

Definitions are checked when read: an unknown key, an unknown type, a default of
the wrong type, a `${name}` naming no parameter and a `base` or `with` breaking
a rule above are errors before any use. A body is read once, into the template
every invocation instantiates, so an expansion costs what it reads and writes:
the document, and the values it writes. Expansion is a pure function of the
documents given, so equal bytes expand to equal trees.

`Expand` does not modify its document, and its result shares no table or array
with the document or the set: writing over a result reaches neither. Arrays
come back as `[]any`, whatever form the parser gave them. `Expand` refuses a
document holding `[macros]` that `Define` has not taken, so a file meant only
to invoke cannot define. The zero `Set` is an empty one; a set is not for
concurrent use.

A document may come from a peer, so every expansion is bounded, and passing a
bound is an error:

- a macro reached again through its own expansion, and invocations nested
  deeper than 32, a base being one;
- `Set.MaxValues` values written through one set, `DefaultMaxValues` (2^18)
  when zero: each value a body writes, a copied argument's or default's among
  them, summed over every `Expand`, so a set's documents share one budget;
- a result nested deeper than 1000 levels, which is what `Decode` and `Marshal`
  take, and a document nested so before it expands;
- a macro of more than 64 parameters, those a base leaves open among them;
- more than 2^16 parameters left open by bases through one set: each costs its
  macro what a declared one does, and its document nothing.

An error names the invocation path through every macro involved,
`states.A: macro "Tower": body.on_enter[0]: unknown macro "Emit"`, each key as a
document writes it, and `with` for `body` in a macro over a base. What it quotes
of a document is cut to 32 bytes and a path to 64 steps, so an error stays small
whatever the document.

`Fold` is the inverse of `Expand`: given an expanded value, a whole document or
a part of one, it returns the value with every part that an invocation of one of
the set's macros expands to replaced by that invocation, a table holding `macro`
and its arguments. What no macro expands to stays as it is, which is never an
error.

```go
value, err := set.Expand(doc)
folded, err := set.Fold(value) // expands to value again
```

- **Where it looks.** At every level: each table, and each run of an array's
  elements. A run that a macro whose body is an array expands to becomes one
  element; an array is never an invocation as a whole.
- **Through bodies.** A body is matched as what it expands to, through the
  macros it invokes and any base. `each` reads backwards: an argument comes
  back as one value where every expansion agrees and the parameter takes one,
  and as an array where they differ or the parameter is an `array`.
- **Which.** The fewest values written, a table, an array and a scalar each
  one: a macro over a base before its base, and a table left a table where its
  own folded parts write fewer than an invocation of the whole. An invocation
  that writes as many as the table it replaces stands. Then the fewest
  arguments, then the first name, so one value always folds one way.
- **Defaults.** An argument equal to its parameter's default is left out.
- **The same value.** A fold's expansion is its value again under one rule,
  made for what `Marshal` writes of a Go struct, where a field at its zero
  value is absent and a float field writes `1.0` where a body wrote `1`:
  - the expansion may hold a key the value leaves out, where it holds a zero
    value: `0`, `0.0`, `false`, `""`, an empty array, or a table of nothing
    else;
  - an integer and the float of exactly its value are one number.

  It is never read the other way: a fold drops no key its value holds. In
  counting, a key holding a zero costs nothing in the value, there or not, and
  what it writes in a body, so a struct's shape folds to the invocations its
  expansion does. A zero left out of an `any` parameter is written `0`, unless
  a value that is there says which zero it is.
- **Checked.** What a fold writes, the expander itself expands, and the result
  is compared with the value under that rule before `Fold` returns. An
  invocation that does not come back is dropped and its part left as it was.
- **A fixed point.** Folding a fold's expansion gives the fold again.
- **Left alone.** A body that invokes by a name spelled from arguments
  (`"${theme}Fg"`); text around more than one reference still unknown, whose
  split nothing settles; an `each` written as one reference; a table holding
  nothing but zero values; and macros the set does not hold. A run of single
  elements is not gathered into one invocation with `each`: only a body's own
  `each` is read back.

`Fold` does not modify its value, and its result shares no table or array with
the value or the set; arrays come back as `[]any`. It spends none of the set's
budget, and what it writes expands within `Set.MaxValues` through a set that
has spent nothing. It refuses what `Expand` refuses, a value holding `[macros]`
or nested deeper than 1000 levels, and it is bounded: a fold takes at most 64
steps, a value read, compared or written, for each value of its input times
each value the set's macros hold, and passing that is an error.

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
