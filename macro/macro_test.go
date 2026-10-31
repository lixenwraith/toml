package macro

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/lixenwraith/toml"
)

func parse(t testing.TB, src string) map[string]any {
	t.Helper()
	doc, err := toml.NewParser([]byte(src)).Parse()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}

func expand(t *testing.T, src string) map[string]any {
	t.Helper()
	out, err := ExpandDocument(parse(t, src))
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	return out
}

func equal(t *testing.T, got map[string]any, want string) {
	t.Helper()
	if w := parse(t, want); !reflect.DeepEqual(normalize(got), normalize(w)) {
		t.Fatalf("expansion\n got %v\nwant %v", got, w)
	}
}

// normalize folds the parser's two array forms into one so trees compare by value.
func normalize(v any) any {
	if list, ok := elements(v); ok {
		out := make([]any, len(list))
		for i, el := range list {
			out[i] = normalize(el)
		}
		return out
	}
	if t, ok := v.(map[string]any); ok {
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = normalize(x)
		}
		return out
	}
	return v
}

// A whole-string reference keeps the argument's type; a reference inside a string
// or a key is spliced as text; an omitted argument takes its default.
func TestInvocationTakesTheBodyWithArgumentsSubstituted(t *testing.T) {
	got := expand(t, `
[macros.Spawn]
params = { id = "string", x = "int", hp = "float", at = "table" }
defaults = { hp = 2.5 }
body = { name = "Td${id}", x = "${x}", hp = "${hp}", pos = "${at}", "${id}_anchor" = "$$${x}" }

[states]
A = { macro = "Spawn", id = "Q1", x = 125, at = { y = 62 } }
`)
	equal(t, got, `
[states.A]
name = "TdQ1"
x = 125
hp = 2.5
pos = { y = 62 }
Q1_anchor = "$125"
`)
}

// An expansion that is an array splices into the array holding the invocation;
// each expands once per zipped element, beside the arguments shared by all.
func TestArrayInvocationsSplice(t *testing.T) {
	got := expand(t, `
[macros.Announce]
params = { message = "string" }
body = [{ strobe = true }, { message = "${message}" }]

[macros.Zero]
params = { name = "string", value = "int" }
defaults = { value = 0 }
body = { set = "${name}", value = "${value}" }

[states.A]
on_enter = [
    { first = true },
    { macro = "Announce", message = "GO" },
    { macro = "Zero", each = { name = ["a", "b"], value = [1, 2] } },
    { macro = "Zero", name = "c" },
]
`)
	equal(t, got, `
[states.A]
on_enter = [
    { first = true },
    { strobe = true }, { message = "GO" },
    { set = "a", value = 1 }, { set = "b", value = 2 },
    { set = "c", value = 0 },
]
`)
}

// Bodies invoke other macros, arguments are expanded where they are written, and
// argument text is never read as a template, so a $ an author passes stays one.
func TestNestedInvocationsExpandAndArgumentsStayInert(t *testing.T) {
	got := expand(t, `
[macros.Emit]
params = { event = "string", payload = "table" }
defaults = { payload = {} }
body = { action = "EmitEvent", event = "${event}", payload = "${payload}" }

[macros.Say]
params = { text = "string" }
body = { macro = "Emit", event = "Message", payload = { message = "${text}" } }

[macros.Both]
params = { first = "array", text = "string" }
body = { list = "${first}", say = { macro = "Say", text = "${text}" } }

[states]
A = { macro = "Both", first = [{ macro = "Emit", event = "Clear" }], text = "costs ${x} $" }
`)
	equal(t, got, `
[states.A]
list = [{ action = "EmitEvent", event = "Clear", payload = {} }]
say = { action = "EmitEvent", event = "Message", payload = { message = "costs ${x} $" } }
`)
}

func TestMalformedMacrosAreRejected(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"unknown macro", `a = { macro = "Nope" }`, `unknown macro "Nope"`},
		{"unknown argument", `
[macros.M]
body = 1
[x]
a = { macro = "M", y = 1 }`, `unknown argument "y"`},
		{"missing argument", `
[macros.M]
params = { y = "int" }
body = "${y}"
[x]
a = { macro = "M" }`, `missing argument "y"`},
		{"argument type", `
[macros.M]
params = { y = "int" }
body = "${y}"
[x]
a = { macro = "M", y = "one" }`, `argument "y": want int, got string`},
		{"undeclared reference", `
[macros.M]
body = { a = "${y}" }`, `body.a: undeclared parameter "y"`},
		{"lone dollar", `
[macros.M]
body = "5 $"`, `a literal $ is written $$`},
		{"recursion", `
[macros.A]
body = { macro = "B" }
[macros.B]
body = [{ macro = "A" }]
[x]
a = { macro = "A" }`, `macro "A" invokes itself through A → B`},
		{"each outside an array", `
[macros.M]
params = { y = "int" }
body = "${y}"
[x]
a = { macro = "M", each = { y = [1, 2] } }`, `each expands only inside an array`},
		{"each lengths", `
[macros.M]
params = { y = "int", z = "int" }
body = "${y}"
[x]
a = [{ macro = "M", each = { y = [1, 2], z = [1] } }]`, `has 1 elements, the others 2`},
		{"table spliced into text", `
[macros.M]
params = { y = "table" }
body = "at ${y}"
[x]
a = { macro = "M", y = {} }`, `table argument "y" cannot be part of a string`},
		{"keys collide", `
[macros.M]
params = { y = "string" }
body = { "${y}" = 1, b = 2 }
[x]
a = { macro = "M", y = "b" }`, `key "b" appears twice`},
		{"default type", `
[macros.M]
params = { y = "int" }
defaults = { y = "one" }
body = 1`, `default for "y": want int, got string`},
		{"reserved parameter", `
[macros.M]
params = { each = "int" }
body = 1`, `parameter "each": not a usable name`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ExpandDocument(parse(t, c.src))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// A document invoking from another document's set may not define its own, and a
// name defined in both is refused rather than shadowed.
func TestDefinitionsBelongToTheSet(t *testing.T) {
	s := NewSet()
	if err := s.Define(parse(t, "[macros.M]\nbody = 1\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.Define(parse(t, "[macros.M]\nbody = 2\n")); err == nil {
		t.Fatal("a second definition of M was accepted")
	}
	if _, err := s.Expand(parse(t, "[macros.N]\nbody = 1\n")); err == nil {
		t.Fatal("a document that only invokes defined a macro")
	}
	out, err := s.Expand(parse(t, "a = { macro = \"M\" }\n"))
	if err != nil || out["a"] != int64(1) {
		t.Fatalf("got %v, %v", out, err)
	}
}

// Doubling bodies grow exponentially in the invocation depth; the set's budget
// stops them long before memory does, and nesting has its own ceiling.
func TestExpansionIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("[macros.M0]\nbody = [1, 2, 3, 4, 5, 6, 7, 8]\n")
	for i := 1; i <= 24; i++ {
		b.WriteString("[macros.M" + strconv.Itoa(i) + "]\nbody = [{ macro = \"M" + strconv.Itoa(i-1) + "\" }, { macro = \"M" + strconv.Itoa(i-1) + "\" }]\n")
	}
	b.WriteString("[x]\na = { macro = \"M24\" }\n")
	if _, err := ExpandDocument(parse(t, b.String())); err == nil || !strings.Contains(err.Error(), "expansion exceeds") {
		t.Fatalf("doubling to 2^24 copies: %v", err)
	}

	b.Reset()
	for i := range maxDepth + 1 {
		b.WriteString("[macros.N" + strconv.Itoa(i) + "]\nbody = { macro = \"N" + strconv.Itoa(i+1) + "\" }\n")
	}
	b.WriteString("[macros.N" + strconv.Itoa(maxDepth+1) + "]\nbody = 1\n[x]\na = { macro = \"N0\" }\n")
	if _, err := ExpandDocument(parse(t, b.String())); err == nil || !strings.Contains(err.Error(), "nest deeper") {
		t.Fatalf("a chain of %d macros: %v", maxDepth+2, err)
	}
}

// Scenarios arrive from peers: any parsed document either expands or is refused.
func FuzzExpandDocument(f *testing.F) {
	f.Add("[macros.M]\nparams = { y = \"int\" }\nbody = [\"${y}\"]\n[x]\na = [{ macro = \"M\", each = { y = [1, 2] } }]\n")
	f.Add("[macros.M]\nbody = { \"${y}\" = 1 }\n")
	f.Add("a = { macro = \"M\" }\n")
	f.Fuzz(func(t *testing.T, src string) {
		doc, err := toml.NewParser([]byte(src)).Parse()
		if err != nil {
			return
		}
		_, _ = ExpandDocument(doc)
	})
}
