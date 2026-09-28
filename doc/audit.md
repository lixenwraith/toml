# Configuration integration audit

The audit fixes concrete failures in the original release:

- Numeric conversion truncated fractions, accepted float values outside integer
  range, rejected ordinary `uint` input, and could not decode full `uint64`.
  Checked conversions now operate in the source integer domain and reject loss.
- Parser integers depended on 64-bit `int`. They are now explicitly `int64`;
  narrowed destinations retain overflow checks on every architecture.
- Failed decoding could partially overwrite a destination; decoding now commits
  only after success. Named string map keys no longer panic, and tags with an
  empty name correctly fall back to the field name. Existing pointer defaults
  survive updates, and errors do not mutate the original pointee or containers.
- Fixed arrays could be encoded but not decoded. They now decode with exact
  length and element-range checks, including nested arrays and arrays of tables.
- Nested table headers failed to quote their component keys. Arrays of interface
  table values could encode empty tables. Mixed/nested arrays now encode using
  inline tables, and nested keys use the same escaping as scalar keys.
- Backspace/form-feed strings did not decode the escapes emitted by Marshal.
  Invalid UTF-8, raw string controls, missing array/inline-table commas and
  missing statement separators are rejected.
- Cyclic encoding could overflow the stack. Encoding/decoding and dotted paths
  now have depth limits. Numeric keys and unsupported uint64 literals fail
  explicitly instead of generating unreadable output. Nil array elements error.
- Numeric key checks now cover decimal keys of any length, including values
  beyond int64; the parser and encoder apply the same restriction.
- Formatting an invalid unsigned input could recurse through a cyclic map.
  Conversion errors now report the source type without formatting its contents.
- Map decoding and encoder entry collection preallocate from known sizes. All
  operation state stays local; no global cache or lock was added.

No datetime, custom marshal interface, literal-string or multiline-string
feature was added. The supported subset and remaining deviations are in README.

This branch consolidates the prior `audit/numeric-hardening-20260928` and
`audit/config-roundtrip-hardening` commits, retaining both in its ancestry.
Only this repository is changed. Consumer migration, watcher publication and
snapshot ownership remain work for the separate config audit.

Verification for this pass: unit and race suites and `go vet` pass on Go 1.27.1;
the 386 test binary cross-compiles. This host cannot execute 386 binaries, so CI
executes that suite. The first CI run caught a test fixture that cast an int64
through machine-sized int before Decode; that fixture now retains its int64.
Regression tests for fixed arrays, pointer defaults and oversized numeric keys
were also run against recovered commit `6384895` and reproduced the failures.

The 15-second round-trip fuzz run passed 71,221 executions. Its property now
fails on unexpected Marshal errors instead of silently skipping them; only the
documented whole-tree depth bound is excluded. Integer fuzzing covers full
uint64 preservation, signed boundaries and uint8 narrowing. CI runs these bounded
fuzz checks on every push and pull request. Refer to the PR checks for final CI
results.

Integration limits: parsed integers are int64; raw assertions must migrate.
Direct Decode can retain the full uint64 range, but file encoding rejects values
above MaxInt64. Interface destinations retain source references, so config must
own its snapshots. Comment text can be collected with the existing lexer and
written as a preamble; Marshal does not preserve source positions. README
documents these contracts and the intentionally limited TOML syntax.
