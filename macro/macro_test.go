package macro

import (
	"fmt"
	"math"
	"reflect"
	"runtime"
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
	out, _ := clone(v)
	return out
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
// stops them long before memory does. Invocation nesting, a result's nesting
// and a macro's parameters have ceilings of their own, and MaxValues moves the
// budget.
func TestExpansionIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("[macros.M0]\nbody = [1, 2, 3, 4, 5, 6, 7, 8]\n")
	for i := 1; i <= 24; i++ {
		b.WriteString("[macros.M" + strconv.Itoa(i) + "]\nbody = [{ macro = \"M" + strconv.Itoa(i-1) + "\" }, { macro = \"M" + strconv.Itoa(i-1) + "\" }]\n")
	}
	b.WriteString("[x]\na = { macro = \"M24\" }\n")
	doubling := b.String()

	b.Reset()
	for i := range maxDepth + 1 {
		b.WriteString("[macros.N" + strconv.Itoa(i) + "]\nbody = { macro = \"N" + strconv.Itoa(i+1) + "\" }\n")
	}
	b.WriteString("[macros.N" + strconv.Itoa(maxDepth+1) + "]\nbody = 1\n[x]\na = { macro = \"N0\" }\n")
	chain := b.String()

	wrapped := "[macros.W]\nparams = { x = \"any\" }\nbody = [[\"${x}\"]]\n[x]\na = " +
		strings.Repeat("{ macro = \"W\", x = ", 600) + "1" + strings.Repeat(" }", 600) + "\n"

	b.Reset()
	b.WriteString("[macros.P]\nbody = 1\n[macros.P.params]\n")
	for i := range maxParams + 1 {
		b.WriteString("p" + strconv.Itoa(i) + " = \"int\"\n")
	}
	for _, c := range []struct{ name, src, want string }{
		{"doubling to 2^24 copies", doubling, "expansion exceeds"},
		{"a chain of macros", chain, "nest deeper"},
		{"a result of 1200 levels", wrapped, "nests deeper than 1000"},
		{"65 parameters", b.String(), "more than 64 parameters"},
	} {
		if _, err := ExpandDocument(parse(t, c.src)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}

	s := &Set{MaxValues: 8}
	if err := s.Define(parse(t, "[macros.M]\nbody = [1, 2, 3]\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Expand(parse(t, "a = { macro = \"M\" }\nb = { macro = \"M\" }\n")); err != nil {
		t.Fatalf("eight values within a budget of eight: %v", err)
	}
	if _, err := s.Expand(parse(t, "a = { macro = \"M\" }\n")); err == nil || !strings.Contains(err.Error(), "expansion exceeds 8 values") {
		t.Fatalf("a ninth value: %v", err)
	}
}

// A default is written once and expands where it is used: as a value there, and
// as an array's element spliced in like an invocation written there. Its type
// is checked as written, so one that invokes is a table.
func TestADefaultThatInvokesExpandsWhereItIsUsed(t *testing.T) {
	got := expand(t, `
[macros.White]
body = [255, 255, 255]

[macros.Flash]
params = { rgb = "any" }
defaults = { rgb = { macro = "White" } }
body = { color = "${rgb}", spread = [0, "${rgb}"] }

[states]
A = { macro = "Flash" }
`)
	equal(t, got, `
[states.A]
color = [255, 255, 255]
spread = [0, 255, 255, 255]
`)
}

// A document built in Go may hold any integer type where the parser gives int64
func TestAnIntegerOfAnyGoTypeIsAnInt(t *testing.T) {
	doc := parse(t, "[macros.M]\nparams = { n = \"int\" }\nbody = { at = \"cell_${n}\", n = \"${n}\" }\n")
	doc["a"] = map[string]any{"macro": "M", "n": 7}
	out, err := ExpandDocument(doc)
	if want := map[string]any{"at": "cell_7", "n": 7}; err != nil || !reflect.DeepEqual(out["a"], want) {
		t.Fatalf("got %v, %v", out, err)
	}
}

// An error names the path through every macro involved, each key as a document
// would write it, and stays small whatever the document's keys and depth.
func TestErrorsNameThePathAndStayBounded(t *testing.T) {
	long := strings.Repeat("k", 1<<16)
	deep := strings.Repeat("[", 900) + "{ macro = \"Nope\" }" + strings.Repeat("]", 900)
	for _, c := range []struct{ name, src, want string }{
		{"through two macros", `
[macros.Inner]
params = { n = "int" }
body = "${n}"
[macros.Outer]
params = { n = "any" }
body = { list = [0, { macro = "Inner", n = "${n}" }] }
[states]
A = { macro = "Outer", n = "one" }`, `states.A: macro "Outer": body.list[1]: macro "Inner": argument "n": want int, got string`},
		{"a key no bare key spells", "[x]\n\"a.b\\n\" = [{ macro = \"Nope\" }]\n", `x."a.b\n"[0]: unknown macro "Nope"`},
		{"the root", `macro = "Nope"`, `document: unknown macro "Nope"`},
		{"a long key", "[x]\n\"" + long + "\" = { macro = \"" + long + "\" }\n", `(65536 bytes): unknown macro "kkkk`},
		{"a deep path", "a = " + deep + "\n", ` ... `},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := ExpandDocument(parse(t, c.src))
			if err == nil || !strings.Contains(err.Error(), c.want) || len(err.Error()) > 1024 {
				t.Fatalf("got %.300v (%d bytes), want an error containing %q", err, len(fmt.Sprint(err)), c.want)
			}
		})
	}
}

// What an expansion costs follows what it reads and writes: a long key is not
// copied once per value under it, as a path built on the way down would
func TestExpansionCostFollowsItsSize(t *testing.T) {
	src := "\"" + strings.Repeat("k", 1<<16) + "\" = [" + strings.Repeat("1, ", 1<<12) + "]\n"
	doc := parse(t, src)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := ExpandDocument(doc); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 8*uint64(len(src)) {
		t.Fatalf("%d bytes allocated to expand a document of %d", grown, len(src))
	}
}

// A result is its reader's own: writing over it reaches neither the document
// nor the set, whether a body copied, moved or lent what it holds, and the
// result holds no table or array twice.
func TestAResultSharesNothing(t *testing.T) {
	const src = `
[macros.Leaf]
params = { at = "table", tags = "array" }
defaults = { tags = ["d", { deep = [1] }] }
body = { at = "${at}", again = "${at}", tags = "${tags}", fixed = { list = [1, [2]] } }

[macros.Branch]
params = { at = "table", cells = "array", tags = "array" }
defaults = { tags = [{ t = 1 }] }
body = { one = { macro = "Leaf", at = "${at}", tags = "${tags}" }, each = [{ macro = "Leaf", each = { at = "${cells}" } }] }

[states]
plain = { kept = [{ k = [1, 2] }] }
A = { macro = "Branch", at = { x = [1] }, cells = [{ y = [2] }, { y = [3] }] }
B = { macro = "Branch", at = { x = [4] }, cells = [{ y = { macro = "Leaf", at = {} } }], tags = [[5]] }
`
	s := NewSet()
	doc := parse(t, src)
	if err := s.Define(doc); err != nil {
		t.Fatal(err)
	}
	kept, _ := clone(doc)
	first, err := s.Expand(doc)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := clone(first)
	held := map[uintptr]bool{}
	hold(t, held, first)
	hold(t, held, doc)
	scribble(first)
	if !reflect.DeepEqual(any(doc), kept) {
		t.Fatal("writing over the result changed the document")
	}
	if second, err := s.Expand(doc); err != nil || !reflect.DeepEqual(any(second), want) {
		t.Fatalf("writing over the result changed the next expansion: %v", err)
	}
}

// hold records every table and array under v, failing on one already held
func hold(t *testing.T, held map[uintptr]bool, v any) {
	t.Helper()
	switch v := v.(type) {
	case map[string]any:
		if p := reflect.ValueOf(v).Pointer(); held[p] {
			t.Fatal("a table is held twice")
		} else {
			held[p] = true
		}
		for _, x := range v {
			hold(t, held, x)
		}
	case []any:
		if p := reflect.ValueOf(v).Pointer(); len(v) > 0 && held[p] {
			t.Fatal("an array is held twice")
		} else {
			held[p] = true
		}
		for _, x := range v {
			hold(t, held, x)
		}
	case []map[string]any:
		for _, x := range v {
			hold(t, held, x)
		}
	}
}

// scribble writes over every table and array under v
func scribble(v any) {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			scribble(x)
			v[k] = "scribbled"
		}
		v["scribble"] = true
	case []any:
		for i, x := range v {
			scribble(x)
			v[i] = "scribbled"
		}
	}
}

// Scenarios arrive from peers: any parsed document either expands or is refused,
// the same way each time, into a result that holds no invocation and shares
// nothing with the document.
func FuzzExpandDocument(f *testing.F) {
	f.Add("[macros.M]\nparams = { y = \"int\" }\nbody = [\"${y}\"]\n[x]\na = [{ macro = \"M\", each = { y = [1, 2] } }]\n")
	f.Add("[macros.M]\nbody = { \"${y}\" = 1 }\n")
	f.Add("a = { macro = \"M\" }\n")
	f.Add("[macros.M]\nparams = { t = \"table\", l = \"array\" }\ndefaults = { l = [{ macro = \"N\" }] }\nbody = { a = \"${t}\", b = \"${t}\", \"k${l}\" = [\"${l}\"] }\n[macros.N]\nbody = [[1]]\n[[x]]\nmacro = \"M\"\nt = { u = [{}] }\n")
	f.Fuzz(func(t *testing.T, src string) {
		doc, err := toml.NewParser([]byte(src)).Parse()
		if err != nil {
			return
		}
		again, _ := toml.NewParser([]byte(src)).Parse()
		out, err := ExpandDocument(doc)
		twice, errTwice := ExpandDocument(again)
		if err != nil {
			if errTwice == nil || err.Error() != errTwice.Error() {
				t.Fatalf("refused as %q, then as %v", err, errTwice)
			}
			return
		}
		if !reflect.DeepEqual(out, twice) {
			t.Fatal("two expansions of one document differ")
		}
		held := map[uintptr]bool{}
		hold(t, held, out)
		hold(t, held, doc)
		if strings.Contains(fmt.Sprint(out), "map[macro:") && !strings.Contains(src, "map[macro:") {
			t.Fatalf("an invocation is left in %v", out)
		}
	})
}

// bank writes a music bank as its authors do: lanes and lines as macros over
// one another, and n patterns of four tracks invoking them
func bank(n int) string {
	var b strings.Builder
	b.WriteString(`
[macros.Hit]
params = { pos = "int", vel = "float", prob = "float" }
defaults = { prob = 0 }
body = { pos = "${pos}", vel = "${vel}", prob = "${prob}" }

[macros.Drum]
params = { instr = "string", pos = "array", vel = "float", humanize = "float" }
defaults = { humanize = 0.4 }
body = { instr = "${instr}", humanize = "${humanize}", event = [{ macro = "Hit", each = { pos = "${pos}" }, vel = "${vel}" }] }

[macros.Kick4]
params = { vel = "float" }
defaults = { vel = 1.0 }
body = { macro = "Drum", instr = "kick", pos = [0, 4, 8, 12], vel = "${vel}", humanize = 0 }

[macros.Note]
params = { pos = "int", deg = "int", dur = "int", vel = "float", oct = "int" }
defaults = { deg = 0, dur = 1, vel = 0.5, oct = 2 }
body = { pos = "${pos}", deg = "${deg}", dur = "${dur}", vel = "${vel}", oct = "${oct}" }

[macros.LineV]
params = { instr = "string", pos = "array", deg = "array", dur = "array", vel = "array", oct = "int" }
defaults = { oct = 2 }
body = { instr = "${instr}", follow_chord = true, event = [{ macro = "Note", each = { pos = "${pos}", deg = "${deg}", dur = "${dur}", vel = "${vel}" }, oct = "${oct}" }] }

[macros.Arp8]
params = { deg = "array", vel = "float" }
defaults = { vel = 0.5 }
body = { macro = "LineV", instr = "pluck", pos = [0, 2, 4, 6, 8, 10, 12, 14], deg = "${deg}", dur = [2, 2, 2, 2, 2, 2, 2, 2], vel = ["${vel}", 0.42, 0.48, 0.42, "${vel}", 0.42, 0.48, 0.44] }
`)
	for i := range n {
		fmt.Fprintf(&b, `
[[pattern]]
name = "pattern_%d"
steps = 16
groups = ["rolling"]
tiers = ["calm", "normal"]
[[pattern.track]]
macro = "Kick4"
vel = 0.9
[[pattern.track]]
macro = "Drum"
instr = "hihat"
pos = [2, 6, 10, 14]
vel = 0.4
[[pattern.track]]
macro = "LineV"
instr = "bass"
oct = 0
pos = [1, 2, 3, 5, 6, 7, 9, 10, 11, 13, 14, 15]
deg = [0, 0, 7, 0, 4, 0, 0, 0, 7, 4, 2, 0]
dur = [1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1]
vel = [0.85, 0.65, 0.65, 0.85, 0.65, 0.65, 0.85, 0.65, 0.65, 0.85, 0.65, 0.65]
[[pattern.track]]
macro = "Arp8"
deg = [0, 2, 4, 7, 4, 2, 0, 2]
`, i)
	}
	return b.String()
}

// BenchmarkExpand is a bank of 84 patterns, some 15000 values expanded, from
// its parsed tree to its expansion: the set reads its macros once
func BenchmarkExpand(b *testing.B) {
	src := bank(84)
	doc := parse(b, src)
	s := &Set{MaxValues: math.MaxInt}
	if err := s.Define(doc); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.Expand(doc); err != nil {
			b.Fatal(err)
		}
	}
}
