# Configuration integration audit

The audit fixes concrete failures in the original release:

- Numeric conversion truncated fractions, accepted float values outside integer
  range, rejected ordinary `uint` input, and could not decode full `uint64`.
  Checked conversions now operate in the source integer domain and reject loss.
- Parser integers depended on 64-bit `int`. They are now explicitly `int64`;
  narrowed destinations retain overflow checks on every architecture.
- Failed decoding could partially overwrite a destination; decoding now commits
  only after success. Named string map keys no longer panic, and tags with an
  empty name correctly fall back to the field name.
- Nested table headers failed to quote their component keys. Arrays of interface
  table values could encode empty tables. Mixed/nested arrays now encode using
  inline tables, and nested keys use the same escaping as scalar keys.
- Backspace/form-feed strings did not decode the escapes emitted by Marshal.
  Invalid UTF-8, raw string controls, missing array/inline-table commas and
  missing statement separators are rejected.
- Cyclic encoding could overflow the stack. Encoding/decoding and dotted paths
  now have depth limits. Numeric keys and unsupported uint64 literals fail
  explicitly instead of generating unreadable output. Nil array elements error.

No datetime, custom marshal interface, literal-string or multiline-string
feature was added. The supported subset and remaining deviations are in README.

Verification: complete unit and race suites, `go vet`, 386 test-binary cross
compilation, 30 seconds of round-trip fuzzing (449,617 executions) and 15 seconds
of integer-conversion fuzzing (1,625,781 executions). The local host cannot execute
386 binaries; CI includes the 386 execution gate. Consumer checks are recorded in
config's audit document.
