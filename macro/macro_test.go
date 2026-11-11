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
// nor the set, whether a body copied what it holds or lent it on, and the
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

// refuse fails unless src is refused with an error holding want; with no
// invocation in src, the refusal is one made when a definition is read
func refuse(t *testing.T, src, want string) {
	t.Helper()
	if _, err := ExpandDocument(parse(t, src)); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want an error containing %q", err, want)
	}
}

// through expands src through a set of its own, and gives what that spent
func through(t *testing.T, src string) (map[string]any, int, error) {
	t.Helper()
	var s Set
	doc := parse(t, src)
	if err := s.Define(doc); err != nil {
		return nil, 0, err
	}
	out, err := s.Expand(doc)
	return out, s.spent, err
}

// await is the base the tests below write macros over: a parameter of each
// kind a macro over it fixes, renames or leaves open
const await = `
[macros.Emit]
params = { event = "string", to = "string" }
body = { action = "EmitEvent", event = "${event}", to = "${to}_anchor" }

[macros.Await]
params = { request = "table", capture = "string", next = "string", fallback = "string", actions = "array", timeout_ms = "int" }
defaults = { actions = [], timeout_ms = 500 }
body = { on_enter = ["${request}"], capture = "${capture}", transitions = [
    { target = "${next}" },
    { target = "${fallback}", actions = "${actions}", ms = "${timeout_ms}" },
] }
`

func TestADefinitionHasBodyOrBaseNeverBoth(t *testing.T) {
	refuse(t, await+"[macros.M]\nbase = \"Await\"\nbody = 1\n", `macro "M": body and base are both given`)
	refuse(t, await+"[macros.M]\nbody = 1\nwith = { capture = \"c\" }\n", `macro "M": with stands only beside base`)
	refuse(t, await+"[macros.M]\nwith = { capture = \"c\" }\n", `macro "M": with stands only beside base`)
	refuse(t, await+"[macros.M]\ndoc = \"neither\"\n", `macro "M": body is missing`)
}

// A base is checked with no invocation in the document, and is the name as
// written where a body would write a$$b
func TestABaseIsALiteralNameCheckedWhenRead(t *testing.T) {
	refuse(t, await+"[macros.M]\nbase = \"Awaits\"\n", `macro "M": unknown base "Awaits"`)
	refuse(t, await+"[macros.M]\nbase = [\"Await\"]\n", `macro "M": base must be a macro's name`)
	refuse(t, "[macros.M]\nbase = \"${x}\"\nparams = { x = \"string\" }\n", `macro "M": unknown base "${x}"`)
	got := expand(t, `
[macros."a$b"]
params = { n = "int" }
body = { n = "${n}" }

[macros.M]
base = "a$b"

[states]
A = { macro = "M", n = 1 }
`)
	equal(t, got, "[states]\nA = { n = 1 }\n")
}

func TestWithHoldsOnlyArgumentsOfTheBase(t *testing.T) {
	for key, value := range map[string]string{"zz": "1", "macro": `"Emit"`, "each": "{ capture = [] }"} {
		refuse(t, await+"[macros.M]\nbase = \"Await\"\nwith = { "+key+" = "+value+" }\n",
			`macro "M": with: macro "Await": unknown argument "`+key+`"`)
	}
	refuse(t, await+"[macros.M]\nbase = \"Await\"\nwith = [\"c\"]\n", `macro "M": with must be a table of arguments`)
}

// What with does not set stays open, with the base's type and default; the
// macro's own are new names, or stand under names with sets
func TestAMacroOverABaseTakesItsOpenParametersAndItsOwn(t *testing.T) {
	const pylon = await + `
[macros.Pylon]
base = "Await"
params = { x = "int", attach = "string", next = "int" }
[macros.Pylon.with]
request = { spawn = "pylon", x = "${x}" }
capture = "gw"
next = "${attach}"
fallback = "Wave${next}"
`
	got := expand(t, pylon+`
[states]
A = { macro = "Pylon", x = 20, attach = "GwTL", next = 2 }
B = { macro = "Pylon", x = 30, attach = "GwTR", next = 3, timeout_ms = 50, actions = [{ macro = "Emit", event = "Skip", to = "t1" }] }
`)
	equal(t, got, `
[states.A]
on_enter = [{ spawn = "pylon", x = 20 }]
capture = "gw"
transitions = [{ target = "GwTL" }, { target = "Wave2", actions = [], ms = 500 }]

[states.B]
on_enter = [{ spawn = "pylon", x = 30 }]
capture = "gw"
transitions = [{ target = "GwTR" }, { target = "Wave3", actions = [{ action = "EmitEvent", event = "Skip", to = "t1_anchor" }], ms = 50 }]
`)
	refuse(t, pylon+"[states]\nA = { macro = \"Pylon\", x = 1, attach = \"G\", next = 2, timeout_ms = \"soon\" }\n",
		`states.A: macro "Pylon": argument "timeout_ms": want int, got string`)
	refuse(t, pylon+"[states]\nA = { macro = \"Pylon\", x = 1, attach = \"G\", next = 2, capture = \"c\" }\n",
		`states.A: macro "Pylon": unknown argument "capture"`)
	refuse(t, pylon+"[states]\nA = { macro = \"Pylon\", attach = \"G\", next = 2 }\n", `states.A: macro "Pylon": missing argument "x"`)
	refuse(t, await+"[macros.M]\nbase = \"Await\"\nwith = { capture = \"c\" }\n[states]\nA = { macro = \"M\", request = {}, next = \"N\" }\n",
		`states.A: macro "M": missing argument "fallback"`)
	refuse(t, await+"[macros.M]\nbase = \"Await\"\nparams = { timeout_ms = \"int\" }\n",
		`macro "M": parameter "timeout_ms": the base leaves it open`)
}

func TestDefaultsGiveOrReplaceADefaultOfAMacroOverABase(t *testing.T) {
	const kick = await + `
[macros.Kick]
base = "Await"
params = { beat = "int" }
defaults = { next = "Idle", timeout_ms = 100, beat = 4 }
with = { request = { beat = "${beat}" }, capture = "c" }
`
	got := expand(t, kick+"[states]\nA = { macro = \"Kick\", fallback = \"F\" }\n")
	equal(t, got, `
[states.A]
on_enter = [{ beat = 4 }]
capture = "c"
transitions = [{ target = "Idle" }, { target = "F", actions = [], ms = 100 }]
`)
	for _, c := range []struct{ defaults, want string }{
		{`{ timeout_ms = "soon" }`, `default for "timeout_ms": want int, got string`},
		{`{ beat = "four" }`, `default for "beat": want int, got string`},
		{`{ capture = "c" }`, `default for undeclared parameter "capture"`},
	} {
		refuse(t, await+"[macros.M]\nbase = \"Await\"\nparams = { beat = \"int\" }\nwith = { capture = \"c\" }\ndefaults = "+c.defaults+"\n", c.want)
	}
}

// with names the macro's parameters, its own and the open ones. Written without
// $ a value is checked when read; holding a reference, when it has its argument.
func TestWithIsATemplateCheckedWhenReadOrAtExpansion(t *testing.T) {
	const flank = await + `
[macros.Flank]
base = "Await"
params = { side = "string", ms = "any" }
with = { capture = "${side}_p2n", fallback = "${next}", timeout_ms = "${ms}", actions = [{ macro = "Emit", event = "Lost", to = "${side}" }] }
`
	got := expand(t, flank+"[states]\nA = { macro = \"Flank\", request = {}, side = \"l\", next = \"N\", ms = 7 }\n")
	equal(t, got, `
[states.A]
on_enter = [{}]
capture = "l_p2n"
transitions = [{ target = "N" }, { target = "N", actions = [{ action = "EmitEvent", event = "Lost", to = "l_anchor" }], ms = 7 }]
`)
	refuse(t, flank+"[states]\nA = { macro = \"Flank\", request = {}, side = \"l\", next = \"N\", ms = \"soon\" }\n",
		`states.A: macro "Flank": with: macro "Await": argument "timeout_ms": want int, got string`)
	refuse(t, await+"[macros.M]\nbase = \"Await\"\nwith = { timeout_ms = \"soon\" }\n",
		`macro "M": with: macro "Await": argument "timeout_ms": want int, got string`)
	refuse(t, await+"[macros.M]\nbase = \"Await\"\nwith = { capture = \"${side}\" }\n", `macro "M": with.capture: undeclared parameter "side"`)

	// Any other value is checked with its arguments, though its type shows
	for _, c := range []struct{ with, want string }{
		{`{ actions = { macro = "Emit", event = "Lost", to = "t1" } }`, `argument "actions": want array, got table`},
		{`{ timeout_ms = ["${next}"] }`, `argument "timeout_ms": want int, got array`},
		{`{ timeout_ms = "5 $$" }`, `argument "timeout_ms": want int, got string`},
	} {
		late := await + "[macros.M]\nbase = \"Await\"\nwith = " + c.with + "\n"
		expand(t, late)
		refuse(t, late+"[states]\nA = { macro = \"M\", request = {}, capture = \"c\", next = \"N\", fallback = \"F\" }\n",
			`states.A: macro "M": with: macro "Await": `+c.want)
	}
}

// The shapes the macros written as one invocation of another take, over their
// base and as the body they stand for: the same tree at the same cost, and the
// same refusal but for the name of the part
func TestAMacroOverABaseExpandsAsItsBodyWould(t *testing.T) {
	const over = `
[macros.Rank]
base = "Await"
with = { capture = "rank", timeout_ms = 900, actions = [{ macro = "Emit", event = "Late", to = "t1" }] }

[macros.Kick]
base = "Await"
defaults = { next = "Idle", timeout_ms = 100 }

[macros.Arp]
base = "Await"
params = { actions = "string" }
with = { actions = [{ macro = "Emit", event = "${actions}", to = "t1" }, { macro = "Emit", event = "Done", to = "t2" }] }

[macros.Pylon]
base = "Await"
params = { attach = "string", next = "string" }
with = { next = "${attach}", fallback = "${next}", capture = "gw" }

[macros.Flank]
base = "Pylon"
params = { side = "string", ms = "any" }
with = { attach = "${side}_p2n", timeout_ms = "${ms}", actions = [{ macro = "Emit", event = "Lost", to = "${side}" }] }
`
	const body = `
[macros.Rank]
params = { request = "table", next = "string", fallback = "string" }
body = { macro = "Await", request = "${request}", next = "${next}", fallback = "${fallback}", capture = "rank", timeout_ms = 900, actions = [{ macro = "Emit", event = "Late", to = "t1" }] }

[macros.Kick]
params = { request = "table", capture = "string", next = "string", fallback = "string", actions = "array", timeout_ms = "int" }
defaults = { actions = [], next = "Idle", timeout_ms = 100 }
body = { macro = "Await", request = "${request}", capture = "${capture}", next = "${next}", fallback = "${fallback}", actions = "${actions}", timeout_ms = "${timeout_ms}" }

[macros.Arp]
params = { request = "table", capture = "string", next = "string", fallback = "string", actions = "string", timeout_ms = "int" }
defaults = { timeout_ms = 500 }
body = { macro = "Await", request = "${request}", capture = "${capture}", next = "${next}", fallback = "${fallback}", timeout_ms = "${timeout_ms}", actions = [{ macro = "Emit", event = "${actions}", to = "t1" }, { macro = "Emit", event = "Done", to = "t2" }] }

[macros.Pylon]
params = { request = "table", attach = "string", next = "string", actions = "array", timeout_ms = "int" }
defaults = { actions = [], timeout_ms = 500 }
body = { macro = "Await", request = "${request}", next = "${attach}", fallback = "${next}", capture = "gw", actions = "${actions}", timeout_ms = "${timeout_ms}" }

[macros.Flank]
params = { request = "table", next = "string", side = "string", ms = "any" }
body = { macro = "Pylon", request = "${request}", next = "${next}", attach = "${side}_p2n", timeout_ms = "${ms}", actions = [{ macro = "Emit", event = "Lost", to = "${side}" }] }
`
	const states = `
[states]
A = { macro = "Rank", request = { r = [1] }, next = "B", fallback = "F" }
B = { macro = "Kick", request = {}, capture = "c", fallback = "F" }
C = { macro = "Arp", request = {}, capture = "c", next = "B", fallback = "F", actions = "Go" }
D = { macro = "Pylon", request = {}, attach = "At", next = "Nx", timeout_ms = 50 }
E = { macro = "Flank", request = {}, side = "l", next = "Nx", ms = 7 }
list = [{ macro = "Kick", each = { capture = ["a", "b"], request = [{}, { r = 2 }] }, fallback = "F" }]
`
	want, wantSpent, err := through(t, await+body+states)
	if err != nil {
		t.Fatal(err)
	}
	got, spent, err := through(t, await+over+states)
	if err != nil || !reflect.DeepEqual(got, want) || spent != wantSpent {
		t.Fatalf("over a base: %v, spent %d\n got %v\nwant %v, spent %d", err, spent, got, want, wantSpent)
	}
	const late = "F = { macro = \"Flank\", request = {}, side = \"l\", next = \"Nx\", ms = \"soon\" }\n"
	_, _, wantErr := through(t, await+body+states+late)
	_, _, err = through(t, await+over+states+late)
	if err == nil || wantErr == nil || strings.ReplaceAll(err.Error(), ": with: ", ": body: ") != wantErr.Error() {
		t.Fatalf("over a base refused as %v, the body as %v", err, wantErr)
	}
}

// A document's macros are read in the order of their names, which is not the
// order of their bases. Across a set a base is one already defined: the last
// document defined is the one that invokes.
func TestABaseResolvesWhateverTheOrderOfNames(t *testing.T) {
	got := expand(t, `
[macros.A]
base = "B"
with = { b = 2 }
[macros.B]
base = "D"
with = { c = 3 }
[macros.C]
base = "A"
with = { a = 1 }
[macros.D]
params = { a = "int", b = "int", c = "int" }
body = [["${a}", "${b}", "${c}"]]
[states]
list = [{ macro = "C" }, { macro = "A", a = 4 }, { macro = "B", a = 5, b = 6 }]
`)
	equal(t, got, "[states]\nlist = [[1, 2, 3], [4, 2, 3], [5, 6, 3]]\n")

	const library, entry = "[macros.Raider]\nparams = { hp = \"int\" }\nbody = { hp = \"${hp}\" }\n",
		"[macros.Rank1]\nbase = \"Raider\"\nwith = { hp = 8 }\n"
	s := NewSet()
	if err := s.Define(parse(t, library)); err != nil {
		t.Fatal(err)
	}
	if err := s.Define(parse(t, entry)); err != nil {
		t.Fatalf("a base of an earlier document: %v", err)
	}
	if out, err := s.Expand(parse(t, "a = { macro = \"Rank1\" }\n")); err != nil || !reflect.DeepEqual(out["a"], map[string]any{"hp": int64(8)}) {
		t.Fatalf("got %v, %v", out, err)
	}
	s = NewSet()
	if err := s.Define(parse(t, entry)); err == nil || !strings.Contains(err.Error(), `macro "Rank1": unknown base "Raider"`) {
		t.Fatalf("a base of a later document: %v", err)
	}
}

func TestAMacroIsNotItsOwnBase(t *testing.T) {
	var b strings.Builder
	for i := range 5000 {
		fmt.Fprintf(&b, "[macros.M%d]\nbase = \"M%d\"\n", i, (i+1)%5000)
	}
	for _, c := range []struct{ name, src, want string }{
		{"itself", "[macros.A]\nbase = \"A\"\n", `macro "A" is its own base through "A"`},
		{"through another", "[macros.A]\nbase = \"B\"\n[macros.B]\nbase = \"A\"\n", `macro "A" is its own base through "B"`},
		{"under a macro over it", "[macros.A]\nbase = \"C\"\n[macros.B]\nbase = \"C\"\n[macros.C]\nbase = \"B\"\n", `macro "C" is its own base through "B"`},
		{"through 5000", b.String(), `macro "M0" is its own base through "M4999"`},
	} {
		if _, err := ExpandDocument(parse(t, c.src)); err == nil || !strings.Contains(err.Error(), c.want) || len(err.Error()) > 1024 {
			t.Errorf("%s: %.300v", c.name, err)
		}
	}
}

func TestTheParameterBoundCountsAllOfAMacroOverABase(t *testing.T) {
	var b strings.Builder
	b.WriteString("[macros.B]\nbody = 1\n[macros.B.params]\n")
	for i := range maxParams {
		b.WriteString("p" + strconv.Itoa(i) + " = \"int\"\n")
	}
	base := b.String()
	if _, err := ExpandDocument(parse(t, base+"[macros.M]\nbase = \"B\"\nparams = { p0 = \"string\" }\nwith = { p0 = 1 }\n")); err != nil {
		t.Fatalf("64 parameters, one its own under a name with sets: %v", err)
	}
	refuse(t, base+"[macros.M]\nbase = \"B\"\nparams = { own = \"int\" }\n", `macro "M": more than 64 parameters`)
}

// An open parameter costs its macro what a declared one does and its document
// nothing: 40000 definitions of two lines over a base of 64 would hold 850 MB
func TestTheParametersBasesLeaveOpenAreBounded(t *testing.T) {
	var b, fixed strings.Builder
	b.WriteString("[macros.B]\nbody = 1\n[macros.B.params]\n")
	fixed.WriteString("[macros.Fixed]\nbase = \"M0\"\n[macros.Fixed.with]\n")
	for i := range maxParams {
		b.WriteString("p" + strconv.Itoa(i) + " = \"int\"\n")
		fixed.WriteString("p" + strconv.Itoa(i) + " = 1\n")
	}
	for i := range maxOpen / maxParams {
		fmt.Fprintf(&b, "[macros.M%d]\nbase = \"B\"\n", i)
	}
	s := NewSet()
	if err := s.Define(parse(t, b.String())); err != nil {
		t.Fatalf("%d open parameters: %v", maxOpen, err)
	}
	err := s.Define(parse(t, "[macros.Over]\nbase = \"B\"\nwith = { p0 = 1 }\n"))
	if err == nil || !strings.Contains(err.Error(), `macro "Over": bases leave more than 65536 parameters open`) {
		t.Fatalf("63 more through the same set: %v", err)
	}
	if err := s.Define(parse(t, fixed.String())); err != nil {
		t.Fatalf("a macro that leaves none open, after one refused: %v", err)
	}
}

// A base is an invocation deeper, as the body it stands for is; a chain no
// invocation can reach the end of is still read, and read without recursion
func TestAChainOfBasesCountsTowardTheNestingBound(t *testing.T) {
	var b strings.Builder
	b.WriteString("[macros.N0]\nbody = 1\n")
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&b, "[macros.N%d]\nbase = \"N%d\"\n", i, i-1)
	}
	chain := b.String()
	got := expand(t, chain+"[x]\na = { macro = \"N"+strconv.Itoa(maxDepth-1)+"\" }\n")
	equal(t, got, "[x]\na = 1\n")
	refuse(t, chain+"[x]\na = { macro = \"N"+strconv.Itoa(maxDepth)+"\" }\n", `with: macros nest deeper than 32`)
}

// A path through with reads as one through a body does, a key no bare key
// spells quoted and a long one cut
func TestErrorsNameThePathThroughWith(t *testing.T) {
	long := strings.Repeat("k", 1<<16)
	for _, c := range []struct{ name, with, want string }{
		{"into a value", `actions = [0, { macro = "Emit", event = "${capture}" }]`,
			`states.A: macro "M": with.actions[1]: macro "Emit": missing argument "to"`},
		{"a key no bare key spells", `actions = [{ "a.b\n" = { macro = "Nope" } }]`, `states.A: macro "M": with.actions[0]."a.b\n": unknown macro "Nope"`},
		{"a long key", `actions = [{ "` + long + `" = { macro = "Nope" } }]`, `(65536 bytes): unknown macro "Nope"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := await + "[macros.M]\nbase = \"Await\"\nwith = { " + c.with + " }\n" +
				"[states]\nA = { macro = \"M\", request = {}, capture = \"c\", next = \"N\", fallback = \"F\" }\n"
			_, err := ExpandDocument(parse(t, src))
			if err == nil || !strings.Contains(err.Error(), c.want) || len(err.Error()) > 1024 {
				t.Fatalf("got %.300v (%d bytes), want an error containing %q", err, len(fmt.Sprint(err)), c.want)
			}
		})
	}
}

// hit is the lane the tests of each write
const hit = `
[macros.Hit]
params = { pos = "int", vel = "float", tags = "any" }
defaults = { tags = 0 }
body = { pos = "${pos}", vel = "${vel}", tags = "${tags}" }
`

// A value in each that is no array is every expansion's, as the same argument
// beside each is: the same tree at the same cost, each expansion its own copy
func TestEachSharesAValueThatIsNotAnArray(t *testing.T) {
	got, spent, err := through(t, hit+"[x]\nevent = [{ macro = \"Hit\", each = { pos = [0, 4], vel = 0.8, tags = { on = [1] } } }]\n")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, got, "[x]\nevent = [{ pos = 0, vel = 0.8, tags = { on = [1] } }, { pos = 4, vel = 0.8, tags = { on = [1] } }]\n")
	hold(t, map[uintptr]bool{}, got)
	want, wantSpent, err := through(t, hit+"[x]\nevent = [{ macro = \"Hit\", each = { pos = [0, 4] }, vel = 0.8, tags = { on = [1] } }]\n")
	if err != nil || !reflect.DeepEqual(got, want) || spent != wantSpent {
		t.Fatalf("shared in each spent %d, beside each %d: %v\n got %v\nwant %v", spent, wantSpent, err, got, want)
	}
	refuse(t, hit+"[x]\nevent = [{ macro = \"Hit\", each = { pos = [0, 4], tags = [1], vel = 0.8 } }]\n",
		`each: argument "tags" has 1 elements, the others 2`)

	// A value nested near the bound and a shallow one leave each other's nesting
	// alone, whichever of them is shared: pos is placed twelve levels under tags
	const deep = "[macros.Deep]\nparams = { pos = \"int\", tags = \"any\" }\nbody = { tags = \"${tags}\", at = [[[[[[[[[[[[\"${pos}\"]]]]]]]]]]]] }\n"
	nested := strings.Repeat("[", 990) + strings.Repeat("]", 990)
	for _, c := range [][2]string{
		{"each = { pos = [0, 4], tags = { k = " + nested + " } }", "each = { pos = [0, 4] }, tags = { k = " + nested + " }"},
		{"each = { pos = 4, tags = [" + nested + ", " + nested + "] }", "each = { tags = [" + nested + ", " + nested + "] }, pos = 4"},
	} {
		got, spent, err := through(t, deep+"[x]\nevent = [{ macro = \"Deep\", "+c[0]+" }]\n")
		want, wantSpent, wantErr := through(t, deep+"[x]\nevent = [{ macro = \"Deep\", "+c[1]+" }]\n")
		if err != nil || wantErr != nil || !reflect.DeepEqual(got, want) || spent != wantSpent {
			t.Fatalf("shared in each: %.200v, spent %d; beside each: %.200v, spent %d", err, spent, wantErr, wantSpent)
		}
	}
}

// An array's elements are each an argument; a shared value is one whole,
// whether or not an expansion comes to use it
func TestASharedValueIsCheckedAgainstItsParametersType(t *testing.T) {
	refuse(t, hit+"[x]\nevent = [{ macro = \"Hit\", each = { pos = [0, \"four\"], vel = 0.8 } }]\n",
		`x.event[0]: macro "Hit": each: argument "pos"[1]: want int, got string`)
	refuse(t, hit+"[x]\nevent = [{ macro = \"Hit\", each = { pos = [0, 4], vel = \"loud\" } }]\n",
		`x.event[0]: macro "Hit": each: argument "vel": want float, got string`)
	refuse(t, hit+"[x]\nevent = [{ macro = \"Hit\", each = { pos = [], vel = \"loud\" } }]\n",
		`x.event[0]: macro "Hit": each: argument "vel": want float, got string`)
}

func TestAnEachWithNoArrayExpandsOnce(t *testing.T) {
	got := expand(t, hit+"[x]\nevent = [{ first = true }, { macro = \"Hit\", each = { pos = 4, vel = 0.8 } }]\n")
	equal(t, got, "[x]\nevent = [{ first = true }, { pos = 4, vel = 0.8, tags = 0 }]\n")
	refuse(t, hit+"[x]\nevent = { macro = \"Hit\", each = { pos = 4, vel = 0.8 } }\n", `x.event: each expands only inside an array`)
	refuse(t, hit+"[x]\nevent = [{ macro = \"Hit\", pos = 4, vel = 0.8, each = {} }]\n", `each must be a table of arguments`)
}

func TestAParameterInEachIsNotAlsoGivenDirectly(t *testing.T) {
	for _, each := range []string{"{ pos = [0, 4], vel = 0.8 }", "{ pos = [0, 4], vel = [0.8, 0.6] }"} {
		refuse(t, hit+"[x]\nevent = [{ macro = \"Hit\", vel = 0.5, each = "+each+" }]\n",
			`x.event[0]: macro "Hit": each: argument "vel" is also given directly`)
	}
}

// each is read in a document, in a body that writes it as a table, a field a
// parameter, and in a body that takes the whole table as one argument: one
// body serves a velocity that is a number and one that is a list
func TestSharingHoldsOnEveryPathThatReadsEach(t *testing.T) {
	got := expand(t, hit+`
[macros.Drum]
params = { instr = "string", pos = "array", vel = "any" }
body = { instr = "${instr}", event = [{ macro = "Hit", each = { pos = "${pos}", vel = "${vel}", tags = "x" } }] }

[macros.Lane]
params = { lists = "table" }
body = [{ macro = "Hit", each = "${lists}" }]

[[track]]
macro = "Drum"
instr = "kick"
pos = [0, 4]
vel = 0.8
[[track]]
macro = "Drum"
instr = "snare"
pos = [4, 12]
vel = [1.0, 0.9]

[x]
event = [{ macro = "Lane", lists = { pos = [2, 6], vel = 0.5 } }, { macro = "Hit", each = { pos = [1], vel = 0.25 } }]
`)
	equal(t, got, `
[[track]]
instr = "kick"
event = [{ pos = 0, vel = 0.8, tags = "x" }, { pos = 4, vel = 0.8, tags = "x" }]
[[track]]
instr = "snare"
event = [{ pos = 4, vel = 1.0, tags = "x" }, { pos = 12, vel = 0.9, tags = "x" }]

[x]
event = [{ pos = 2, vel = 0.5, tags = 0 }, { pos = 6, vel = 0.5, tags = 0 }, { pos = 1, vel = 0.25, tags = 0 }]
`)
}

// Where a parameter takes an array, an array in each is still an element per
// expansion: one for every expansion to take whole is written beside each
func TestAnArrayInEachIsAlwaysZipped(t *testing.T) {
	got := expand(t, hit+`
[x]
zipped = [{ macro = "Hit", each = { pos = [0, 4], tags = ["a", ["b"]] }, vel = 0.8 }]
beside = [{ macro = "Hit", each = { pos = [0, 4] }, tags = ["a", ["b"]], vel = 0.8 }]
`)
	equal(t, got, `
[x]
zipped = [{ pos = 0, vel = 0.8, tags = "a" }, { pos = 4, vel = 0.8, tags = ["b"] }]
beside = [{ pos = 0, vel = 0.8, tags = ["a", ["b"]] }, { pos = 4, vel = 0.8, tags = ["a", ["b"]] }]
`)
	refuse(t, hit+"[x]\nevent = [{ macro = \"Hit\", each = { pos = [0, 4], tags = [\"a\"] }, vel = 0.8 }]\n",
		`each: argument "tags" has 1 elements, the others 2`)
}

// Scenarios arrive from peers: any parsed document either expands or is refused,
// the same way each time, into a result that holds no invocation and shares
// nothing with the document.
func FuzzExpandDocument(f *testing.F) {
	f.Add("[macros.M]\nparams = { y = \"int\" }\nbody = [\"${y}\"]\n[x]\na = [{ macro = \"M\", each = { y = [1, 2] } }]\n")
	f.Add("[macros.M]\nbody = { \"${y}\" = 1 }\n")
	f.Add("a = { macro = \"M\" }\n")
	f.Add("[macros.M]\nparams = { t = \"table\", l = \"array\" }\ndefaults = { l = [{ macro = \"N\" }] }\nbody = { a = \"${t}\", b = \"${t}\", \"k${l}\" = [\"${l}\"] }\n[macros.N]\nbody = [[1]]\n[[x]]\nmacro = \"M\"\nt = { u = [{}] }\n")
	f.Add("[macros.A]\nbase = \"B\"\nparams = { b = \"int\" }\ndefaults = { a = 1 }\nwith = { b = [\"${b}\", { macro = \"C\" }] }\n[macros.B]\nparams = { a = \"int\", b = \"any\", c = \"table\" }\ndefaults = { c = { macro = \"C\" } }\nbody = { a = \"${a}\", b = \"${b}\", c = [\"${c}\"] }\n[macros.C]\nbase = \"B\"\nwith = { a = 2, b = {}, c = {} }\n[[x]]\nmacro = \"A\"\nb = 3\n")
	f.Add("[macros.Hit]\nparams = { pos = \"int\", vel = \"any\" }\nbody = { pos = \"${pos}\", vel = \"${vel}\" }\n[macros.Drum]\nparams = { pos = \"array\", vel = \"any\", lists = \"table\" }\ndefaults = { lists = { pos = [9], vel = { k = [1] } } }\nbody = [{ macro = \"Hit\", each = { pos = \"${pos}\", vel = \"${vel}\" } }, { macro = \"Hit\", each = \"${lists}\" }]\n[x]\na = [{ macro = \"Drum\", each = { pos = [[0, 4]], vel = 0.5 } }, { macro = \"Drum\", pos = [2], vel = [0.25] }]\n")
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

// asStruct is v as a Go struct marshals it, written apart from the fold: no key
// holding a zero value, and a whole float as its integer
func asStruct(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range v {
			if x = asStruct(x); !zeroValue(x) {
				out[k] = x
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = asStruct(x)
		}
		return out
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			return int64(v)
		}
	}
	return v
}

func zeroValue(v any) bool {
	switch v := v.(type) {
	case map[string]any:
		return len(asStruct(v).(map[string]any)) == 0
	case []any:
		return len(v) == 0
	}
	return v == int64(0) || v == 0.0 || v == "" || v == false
}

// covers is the fold's rule, written apart from it: back holds every key of v
// with the same value, a key v leaves out holds a zero value, and an integer is
// the float of its value
func covers(back, v any) bool {
	switch v := v.(type) {
	case map[string]any:
		b, ok := back.(map[string]any)
		for k, x := range v {
			if y, held := b[k]; !held || !covers(y, x) {
				return false
			}
		}
		for k, y := range b {
			if _, held := v[k]; !held && !zeroValue(y) {
				return false
			}
		}
		return ok
	case []any:
		b, ok := back.([]any)
		if !ok || len(b) != len(v) {
			return false
		}
		for i := range v {
			if !covers(b[i], v[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(asStruct(back), asStruct(v))
}

// folds defines src's macros, folds what the rest of it expands to and wants
// the document want; as of every fold, it must expand to its value again and
// be the fold of that expansion
func folds(t *testing.T, src, want string) {
	t.Helper()
	doc := parse(t, src)
	var s Set
	if err := s.Define(doc); err != nil {
		t.Fatal(err)
	}
	value, err := s.Expand(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Fold(value)
	if w := normalize(parse(t, want)); err != nil || !reflect.DeepEqual(got, w) {
		t.Fatalf("fold\n got %v (%v)\nwant %v", got, err, w)
	}
	s.spent = 0
	back, err := s.Expand(got.(map[string]any))
	if err != nil || !covers(back, value) {
		t.Fatalf("the fold expands to %v (%v), not %v", back, err, value)
	}
	if again, err := s.Fold(back); err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("the fold of the fold's expansion is %v (%v), not %v", again, err, got)
	}
}

// A table a macro expands to becomes its invocation, at every level, and one
// none expands to stays as it is.
func TestATableAMacroExpandsToFoldsToItsInvocation(t *testing.T) {
	folds(t, `
[macros.Hit]
params = { pos = "int", vel = "float" }
body = { kind = "hit", loud = true, pos = "${pos}", vel = "${vel}" }

[song]
name = "a"
intro = { kind = "hit", loud = true, pos = 8, vel = 0.5 }
[[song.track]]
event = [{ kind = "hit", loud = true, pos = 4, vel = 1.0 }, { kind = "miss", loud = true, pos = 4, vel = 1.0 }]
`, `
[song]
name = "a"
intro = { macro = "Hit", pos = 8, vel = 0.5 }
[[song.track]]
event = [{ macro = "Hit", pos = 4, vel = 1.0 }, { kind = "miss", loud = true, pos = 4, vel = 1.0 }]
`)
}

const chords = `
[macros.Note]
params = { pos = "int", deg = "int", vel = "float" }
defaults = { vel = 0.5 }
body = { pos = "${pos}", deg = "${deg}", vel = "${vel}" }

[macros.Chord]
params = { pos = "int", deg = "array", vel = "float" }
defaults = { vel = 0.5 }
body = [{ macro = "Note", each = { deg = "${deg}" }, pos = "${pos}", vel = "${vel}" }]

[macros.Line]
params = { instr = "string", pos = "array", deg = "any", vel = "any" }
defaults = { vel = 0.5 }
body = { instr = "${instr}", event = [{ macro = "Note", each = { pos = "${pos}", deg = "${deg}", vel = "${vel}" } }] }
`

// A run of an array's elements that one invocation expands to, its body an
// array, becomes that one element; the elements around it stay.
func TestARunOfElementsFoldsToOneInvocation(t *testing.T) {
	folds(t, chords+`
[doc]
event = ["rest", { pos = 2, deg = 1, vel = 0.9 }, { pos = 2, deg = 3, vel = 0.9 }, { pos = 2, deg = 5, vel = 0.9 }, { pos = 6, deg = 1, vel = 0.9 }]
`, `
[doc]
event = ["rest", { macro = "Chord", pos = 2, deg = [1, 3, 5], vel = 0.9 }, { pos = 6, deg = 1, vel = 0.9 }]
`)
}

// A body that invokes with each is matched as what it expands to: an argument
// comes back as one value where every element agrees and the parameter takes
// one, and as an array where they differ or the parameter is an array.
func TestEachIsReadBackwards(t *testing.T) {
	folds(t, chords+`
[[track]]
instr = "bass"
event = [{ pos = 0, deg = 7, vel = 0.9 }, { pos = 4, deg = 7, vel = 0.6 }]
[[track]]
instr = "lead"
event = [{ pos = 2, deg = 1, vel = 0.5 }, { pos = 2, deg = 3, vel = 0.5 }]
`, `
[[track]]
macro = "Line"
instr = "bass"
pos = [0, 4]
deg = 7
vel = [0.9, 0.6]
[[track]]
macro = "Line"
instr = "lead"
pos = [2, 2]
deg = [1, 3]
`)
}

// A table stays a table where its folded parts write fewer values than the
// invocation of a macro that expands to the whole of it.
func TestATableStaysWhereItsFoldedPartsWriteFewer(t *testing.T) {
	folds(t, chords+`
[[track]]
instr = "piano"
event = [
  { pos = 2, deg = 1, vel = 0.9 }, { pos = 2, deg = 3, vel = 0.9 }, { pos = 2, deg = 5, vel = 0.9 },
  { pos = 6, deg = 1, vel = 0.7 }, { pos = 6, deg = 3, vel = 0.7 }, { pos = 6, deg = 5, vel = 0.7 },
]
`, `
[[track]]
instr = "piano"
event = [{ macro = "Chord", pos = 2, deg = [1, 3, 5], vel = 0.9 }, { macro = "Chord", pos = 6, deg = [1, 3, 5], vel = 0.7 }]
`)
}

// Among the invocations that expand to a table, the one writing the fewest
// values stands, so a macro over a base before its base; then the fewest
// arguments; then the first name.
func TestTheFewestValuesThenArgumentsThenTheNameDecide(t *testing.T) {
	folds(t, `
[macros.Drum]
params = { instr = "string", vel = "float" }
body = { instr = "${instr}", vel = "${vel}" }

[macros.Kick]
base = "Drum"
with = { instr = "kick" }

[macros.Thud]
base = "Drum"
with = { instr = "kick" }

[macros.Pair]
params = { k = "int", j = "int", spare = "int" }
body = { kind = "x", n = 1, inner = { k = "${k}", j = "${j}" } }

[macros.Wrap]
params = { t = "table" }
body = { kind = "x", n = 1, inner = "${t}" }

[doc]
values = { instr = "kick", vel = 0.8 }
arguments = { kind = "x", n = 1, inner = { k = 1, j = 2 } }
`, `
[doc]
values = { macro = "Kick", vel = 0.8 }
arguments = { macro = "Wrap", t = { k = 1, j = 2 } }
`)
}

// An argument equal to its parameter's default is left out.
func TestAnArgumentEqualToItsDefaultIsLeftOut(t *testing.T) {
	folds(t, `
[macros.Pad]
params = { pos = "array", vel = "float", mode = "table" }
defaults = { vel = 1, mode = { soft = true } }
body = { instr = "pad", pos = "${pos}", vel = "${vel}", mode = "${mode}" }

[doc]
a = { instr = "pad", pos = [0, 16], vel = 1.0, mode = { soft = true } }
b = { instr = "pad", pos = [0], vel = 0.3, mode = { soft = true } }
`, `
[doc]
a = { macro = "Pad", pos = [0, 16] }
b = { macro = "Pad", pos = [0], vel = 0.3 }
`)
}

// A value is the same where the expansion holds a key it leaves out, holding a
// zero value, or an integer for the float of its value: what a Go struct
// marshals to folds as its expansion does. A key the value holds is never
// dropped, zero or not.
func TestAValueFoldsFromAStructsShape(t *testing.T) {
	folds(t, `
[macros.Hit]
params = { pos = "int", vel = "float", prob = "float" }
defaults = { vel = 1, prob = 0 }
body = { kind = "hit", mute = false, pos = "${pos}", vel = "${vel}", prob = "${prob}", fx = { wet = 0, on = [] } }

[doc]
event = [{ kind = "hit", vel = 1.0 }, { kind = "hit", pos = 4, vel = 0.5 }, { kind = "hit", pos = 4, vel = 0.5, pan = 0 }]
`, `
[doc]
event = [{ macro = "Hit", pos = 0 }, { macro = "Hit", pos = 4, vel = 0.5 }, { kind = "hit", pos = 4, vel = 0.5, pan = 0 }]
`)
}

// A key a struct left out says only that an any parameter is some zero: a value
// that is there says which, and with none it is written 0.
func TestAZeroLeftOutOfAnAnyParameterIsSettledByAValueThatIsThere(t *testing.T) {
	folds(t, `
[macros.Flag]
params = { u = "any", k = "int" }
body = { kind = "flag", k = "${k}", c = "${u}", list = ["${u}"] }

[doc]
there = { kind = "flag", k = 3, list = [false] }
none = [{ kind = "flag", k = 3, list = [0] }, { kind = "flag", k = 3, c = "", list = [""] }]
`, `
[doc]
there = { macro = "Flag", k = 3, u = false }
none = [{ macro = "Flag", k = 3, u = 0 }, { macro = "Flag", k = 3, u = "" }]
`)
}

// A fold is checked before it stands: an invocation the expander does not
// expand to what it replaces is dropped and the part left as it was.
func TestAnInvocationThatDoesNotExpandBackIsDropped(t *testing.T) {
	folds(t, `
[macros.Inner]
params = { n = "int" }
body = { kind = "x", n = "${n}" }

[macros.Outer]
doc = "matches a table holding n = \"seven\", and can never expand: n wants an int"
body = { macro = "Inner", n = "seven" }

[doc]
v = { kind = "x", n = "seven" }
w = { kind = "x", n = 7 }
`, `
[doc]
v = { kind = "x", n = "seven" }
w = { macro = "Inner", n = 7 }
`)
}

// Left alone: a body that invokes by a name spelled from arguments, text around
// more than one reference, and an each written as one reference.
func TestWhatAFoldLeavesAlone(t *testing.T) {
	folds(t, `
[macros.RedFg]
body = { color = 1, bold = true }

[macros.Theme]
params = { theme = "string" }
body = { on = true, fg = { macro = "${theme}Fg" } }

[macros.Tag]
params = { a = "string", b = "string" }
body = { kind = "tag", id = "${a}-${b}" }

[macros.Cell]
params = { x = "int" }
body = { kind = "cell", x = "${x}" }

[macros.Row]
params = { lists = "table" }
body = { kind = "row", cells = [{ macro = "Cell", each = "${lists}" }] }

[doc]
name = { on = true, fg = { color = 1, bold = true } }
text = { kind = "tag", id = "x-y" }
each = { kind = "row", cells = [{ kind = "cell", x = 1 }, { kind = "cell", x = 2 }] }
`, `
[doc]
name = { on = true, fg = { macro = "RedFg" } }
text = { kind = "tag", id = "x-y" }
each = { kind = "row", cells = [{ macro = "Cell", x = 1 }, { macro = "Cell", x = 2 }] }
`)
}

// What its author wrote comes back: a bank's expansion, and that expansion in
// a struct's shape, fold to the document's own invocations.
func TestAFoldWritesWhatItsAuthorWrote(t *testing.T) {
	doc := parse(t, bank(8))
	var s Set
	if err := s.Define(doc); err != nil {
		t.Fatal(err)
	}
	value, err := s.Expand(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []any{value, asStruct(value)} {
		if got, err := s.Fold(in); err != nil || !reflect.DeepEqual(got, normalize(doc)) {
			t.Fatalf("fold\n got %v (%v)\nwant %v", got, err, doc)
		}
	}
}

// A fold leaves its value alone, and its result is its reader's own: writing
// over it reaches neither the value nor the set.
func TestAFoldSharesNothing(t *testing.T) {
	doc := parse(t, bank(2))
	var s Set
	if err := s.Define(doc); err != nil {
		t.Fatal(err)
	}
	value, _ := s.Expand(doc)
	kept := normalize(value)
	got, err := s.Fold(value)
	if err != nil {
		t.Fatal(err)
	}
	held := map[uintptr]bool{}
	hold(t, held, got)
	hold(t, held, value)
	scribble(got)
	s.spent = 0
	if again, err := s.Expand(doc); err != nil || !reflect.DeepEqual(value, kept) || !reflect.DeepEqual(again, kept) {
		t.Fatalf("writing over a fold reached its value or the set (%v)", err)
	}
}

// A fold is bounded by its value and the set: one that would cost more steps
// than both allow is refused, and so is what Expand refuses, a value holding
// [macros] or nested deeper than a result may be.
func TestAFoldIsBounded(t *testing.T) {
	var b strings.Builder
	for i := range 16 {
		fmt.Fprintf(&b, "[macros.A%d]\nbody = { x = { macro = \"A%d\" }, y = { macro = \"A%d\" } }\n", i, i+1, i+1)
	}
	b.WriteString("[macros.A16]\nbody = { k = 0 }\n[macros.Top]\nparams = { z = \"int\" }\nbody = { z = \"${z}\", t = { macro = \"A0\" } }\n")
	doc := parse(t, b.String())
	var s Set
	if err := s.Define(doc); err != nil {
		t.Fatal(err)
	}
	deep := any(int64(1))
	for range maxNesting + 1 {
		deep = []any{deep}
	}
	for want, v := range map[string]any{
		"fold exceeds": map[string]any{"z": int64(1)},
		"not allowed":  map[string]any{"macros": map[string]any{}},
		"nests deeper": deep,
	} {
		if _, err := s.Fold(v); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("got %v, want an error containing %q", err, want)
		}
	}
}

// A fold never changes what a value means: whatever a document expands to, the
// fold of it, and of it in a struct's shape, expands to it again, leaves it
// alone, shares nothing with it and folds to itself.
func FuzzFold(f *testing.F) {
	f.Add("[macros.M]\nparams = { y = \"int\" }\nbody = [\"${y}\"]\n[x]\na = [{ macro = \"M\", each = { y = [1, 2] } }]\n")
	f.Add("[macros.M]\nparams = { k = \"string\", v = \"any\" }\ndefaults = { v = 0 }\nbody = { \"p_${k}\" = \"${v}\", name = \"${k}Fg\", n = 0 }\n[[x]]\nmacro = \"M\"\nk = \"a\"\nv = [{ macro = \"M\", k = \"b\" }]\n")
	f.Add("[macros.A]\nbase = \"B\"\nparams = { b = \"int\" }\ndefaults = { a = 1 }\nwith = { b = [\"${b}\", { macro = \"C\" }] }\n[macros.B]\nparams = { a = \"int\", b = \"any\", c = \"table\" }\ndefaults = { c = { macro = \"C\" } }\nbody = { a = \"${a}\", b = \"${b}\", c = [\"${c}\"] }\n[macros.C]\nbase = \"B\"\nwith = { a = 2, b = {}, c = {} }\n[[x]]\nmacro = \"A\"\nb = 3\n")
	f.Add("[macros.Hit]\nparams = { pos = \"int\", vel = \"any\" }\nbody = { pos = \"${pos}\", vel = \"${vel}\" }\n[macros.Drum]\nparams = { pos = \"array\", vel = \"any\", lists = \"table\" }\ndefaults = { lists = { pos = [9], vel = { k = [1] } } }\nbody = [{ macro = \"Hit\", each = { pos = \"${pos}\", vel = \"${vel}\" } }, { macro = \"Hit\", each = \"${lists}\" }]\n[x]\na = [{ macro = \"Drum\", each = { pos = [[0, 4]], vel = 0.5 } }, { macro = \"Drum\", pos = [2], vel = [0.25] }]\n")
	f.Add("[macros.Z]\nparams = { a = \"int\", v = \"float\", p = \"any\" }\ndefaults = { p = false }\nbody = { A = \"${v}\", B = \"${p}\", C = [\"${p}\"] }\n[x]\ny = [{}, { C = [false] }]\n")
	f.Add(bank(2))
	f.Fuzz(func(t *testing.T, src string) {
		doc, err := toml.NewParser([]byte(src)).Parse()
		if err != nil {
			return
		}
		var s Set
		if s.Define(doc) != nil {
			return
		}
		out, err := s.Expand(doc)
		if err != nil {
			return
		}
		kept := normalize(out)
		for _, in := range []any{out, asStruct(out)} {
			folded, err := s.Fold(in)
			if err != nil {
				if !strings.Contains(err.Error(), "fold exceeds") {
					t.Fatalf("fold: %v", err)
				}
				return
			}
			if !reflect.DeepEqual(out, kept) {
				t.Fatal("the fold changed its value")
			}
			held := map[uintptr]bool{}
			hold(t, held, folded)
			hold(t, held, in)
			s.spent = 0
			back, err := s.Expand(folded.(map[string]any))
			if err != nil {
				t.Fatalf("the fold %v does not expand: %v", folded, err)
			}
			if !covers(back, in) {
				t.Fatalf("the fold %v\nexpands to %v\n     not %v", folded, back, in)
			}
			if again, err := s.Fold(back); err != nil || !reflect.DeepEqual(again, folded) {
				t.Fatalf("the fold %v\nof its expansion is %v (%v)", folded, again, err)
			}
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

// BenchmarkFold is the expansion of BenchmarkExpand's bank folded back to its
// 336 invocations: read, matched, and checked by expanding it once
func BenchmarkFold(b *testing.B) {
	src := bank(84)
	doc := parse(b, src)
	s := &Set{MaxValues: math.MaxInt}
	if err := s.Define(doc); err != nil {
		b.Fatal(err)
	}
	value, err := s.Expand(doc)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.Fold(value); err != nil {
			b.Fatal(err)
		}
	}
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
