package toml

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

const settingsDoc = `# mine
play.hud = true # root dotted

[paths]
log = "l" # beside the file

[play]
# kept
mouse = false # keys
volume = 0.3

# about later
[later] # a newer build's
key = 1
[later.sub]
deep = [
  1, # one
  2,
]

[audio]
buffer_ms = 20
`

// TestDocumentEditsOnlyWhatItNames: a set value keeps the rest of its line, a
// missing key goes after its table's last key in that key's spelling, a missing
// table is appended, a delete takes a key's lines or a table's header with every
// key under it and the comments that are theirs, and every other byte stays.
func TestDocumentEditsOnlyWhatItNames(t *testing.T) {
	d, err := ParseDocument([]byte(settingsDoc))
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func() error{
		func() error { return d.Set(Key{"paths", "log"}, "logs") },
		func() error { return d.Set(Key{"play", "sfx"}, false) },
		func() error { return d.Set(Key{"presentation", "cell"}, 14.0) },
		func() error { return d.Set(Key{"audio", "buffer ms"}, 30) },
		func() error { return del(d, Key{"play", "volume"}) },
		func() error { return del(d, Key{"later"}) },
	} {
		if err := edit(); err != nil {
			t.Fatal(err)
		}
	}
	want := `# mine
play.hud = true # root dotted

[paths]
log = "logs" # beside the file

[play]
# kept
mouse = false # keys
sfx = false

[audio]
buffer_ms = 20
"buffer ms" = 30

[presentation]
cell = 14.0
`
	if got := string(d.Bytes()); got != want {
		t.Fatalf("edited:\n%s\nwant:\n%s", got, want)
	}
	var s struct {
		Play struct {
			HUD   bool `toml:"hud"`
			Mouse bool `toml:"mouse"`
			Sfx   bool `toml:"sfx"`
		} `toml:"play"`
	}
	s.Play.Sfx = true
	if err := d.Decode(&s); err != nil || !s.Play.HUD || s.Play.Mouse || s.Play.Sfx {
		t.Errorf("decoded %+v, %v", s, err)
	}
}

func del(d *Document, k Key) error {
	ok, err := d.Delete(k)
	if err == nil && !ok {
		err = fmt.Errorf("%s: nothing deleted", k)
	}
	return err
}

// TestSetSpellsAKeyAsItsTableDoes: with no header, a key joins the dotted keys that
// made its table, spelled as they are.
func TestSetSpellsAKeyAsItsTableDoes(t *testing.T) {
	d, err := ParseDocument([]byte("play.hud = true\n\n[audio]\nbuffer_ms = 20\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Set(Key{"play", "mouse"}, false); err != nil {
		t.Fatal(err)
	}
	if got, want := string(d.Bytes()), "play.hud = true\nplay.mouse = false\n\n[audio]\nbuffer_ms = 20\n"; got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestCommentsGoWithTheNearerSide: a comment group belongs to what fewer blank
// lines part it from, below on a tie; the file's start and end count as sides, so
// its opening and closing comments stay. A key takes its own comments; a run of
// statements going whole takes its comments and one blank run beside it.
func TestCommentsGoWithTheNearerSide(t *testing.T) {
	for _, c := range []struct {
		name, doc string
		key       Key
		want      string
	}{
		{"right above", "a = 1\n# about b\nb = 2\nc = 3\n", Key{"b"}, "a = 1\nc = 3\n"},
		{"right below, run stays", "a = 1\nb = 2\n# after\n\n[t]\nk = 1\n", Key{"b"}, "a = 1\n# after\n\n[t]\nk = 1\n"},
		{"right below, run goes", "[a]\nx = 1\n# end of a\n\n[b]\ny = 2\n", Key{"a"}, "[b]\ny = 2\n"},
		{"tie goes below", "[a]\nx = 1\n\n# about b\n\n[b]\ny = 2\n\n[c]\nz = 3\n", Key{"b"}, "[a]\nx = 1\n\n[c]\nz = 3\n"},
		{"nearer above", "[a]\nx = 1\n\n# about a\n\n\n[b]\ny = 2\n", Key{"b"}, "[a]\nx = 1\n\n# about a\n"},
		{"widest gap splits", "[a]\nx = 1\n# a1\n\n# a2\n\n\n# b1\n\n# b2\n[b]\ny = 2\n", Key{"b"}, "[a]\nx = 1\n# a1\n\n# a2\n"},
		{"file's opening", "# file\n\n[a]\nx = 1\n", Key{"a"}, "# file\n"},
		{"file's closing", "[a]\nx = 1\n\n[b]\ny = 2\n\n# end\n", Key{"b"}, "[a]\nx = 1\n\n# end\n"},
		{"touching the end", "[a]\nx = 1\n\n[b]\ny = 2\n# y's\n", Key{"b"}, "[a]\nx = 1\n"},
		{"one blank run left", "a = 1\n\n[b]\nx = 1\n\n\n[c]\ny = 1\n", Key{"b"}, "a = 1\n\n[c]\ny = 1\n"},
	} {
		d, err := ParseDocument([]byte(c.doc))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if err := del(d, c.key); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := string(d.Bytes()); got != c.want {
			t.Errorf("%s:\n%s\nwant:\n%s", c.name, got, c.want)
		}
	}
}

// TestSetLeavesCommentsTheirOwners: a root key added above the first table keeps
// a blank line from that table's comments, and a new table goes above the file's
// closing comments.
func TestSetLeavesCommentsTheirOwners(t *testing.T) {
	for _, c := range []struct {
		doc  string
		key  Key
		want string
	}{
		{"# file\n\n# about a\n[a]\nx = 1\n", Key{"top"}, "# file\n\ntop = 1\n\n# about a\n[a]\nx = 1\n"},
		{"[a]\nx = 1\n\n# end\n", Key{"b", "y"}, "[a]\nx = 1\n\n[b]\ny = 1\n\n# end\n"},
	} {
		d, err := ParseDocument([]byte(c.doc))
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Set(c.key, 1); err != nil {
			t.Fatal(err)
		}
		if got := string(d.Bytes()); got != c.want {
			t.Errorf("set %s:\n%s\nwant:\n%s", c.key, got, c.want)
		}
	}
}

// TestDocumentRefusesAnEditItCannotPlace: under an array of tables, inside an
// inline value, or over a table, an edit fails and the document stays as it was.
func TestDocumentRefusesAnEditItCannotPlace(t *testing.T) {
	doc := "x = 1\nm = { a = 1 }\n[t]\nk = 1\n[[arr]]\nn = 1\n"
	d, err := ParseDocument([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func() error{
		"array of tables": func() error { return d.Set(Key{"arr", "n"}, 2) },
		"inline set":      func() error { return d.Set(Key{"m", "b"}, 2) },
		"inline delete":   func() error { _, err := d.Delete(Key{"m", "a"}); return err },
		"table":           func() error { return d.Set(Key{"t"}, 2) },
		"under a value":   func() error { return d.Set(Key{"x", "y"}, 2) },
	} {
		if err := edit(); err == nil {
			t.Errorf("%s: the edit was taken", name)
		}
	}
	if string(d.Bytes()) != doc {
		t.Fatalf("a refused edit changed the document:\n%s", d.Bytes())
	}
}

// TestUnmappedNamesWhatDecodeIgnores: keys no field takes, by tag or field name,
// unexported and "-" fields included, are named once each, under arrays of tables
// without their index; a map or interface destination takes every key under it.
// A parsed tree is read as its document is, and UnknownKeys quotes each.
func TestUnmappedNamesWhatDecodeIgnores(t *testing.T) {
	doc := `top = 1
skip = 2
hidden = 3
Named = 4
[sub]
ok = 1
bad = 2
[sub.deeper]
x = 1
[free]
anything = 1
[anyv]
v = { w = 1 }
[[items]]
id = 1
odd = 1
[[items]]
id = 2
odd = 2
[byname.one]
ok = 1
nope = 1
`
	type item struct {
		ID int `toml:"id"`
	}
	type sub struct {
		OK int `toml:"ok,omitempty"`
	}
	var v struct {
		Top    int `toml:"top"`
		Skip   int `toml:"-"`
		hidden int
		Named  int            `toml:",omitempty"`
		Sub    *sub           `toml:"sub"`
		Free   map[string]any `toml:"free"`
		Anyv   any            `toml:"anyv"`
		Items  []item         `toml:"items"`
		ByName map[string]sub `toml:"byname"`
	}
	d, err := ParseDocument([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, k := range d.Unmapped(&v) {
		got = append(got, k.String())
	}
	want := []string{"byname.one.nope", "hidden", "items.odd", "skip", "sub.bad", "sub.deeper"}
	if !slices.Equal(got, want) {
		t.Fatalf("unmapped %q, want %q", got, want)
	}
	if tree := Unmapped(d.Tree(), &v); !slices.EqualFunc(tree, d.Unmapped(&v), slices.Equal) {
		t.Fatalf("the tree's unmapped %q differ from the document's", tree)
	}
	if err := UnknownKeys(d.Unmapped(&v)[:2]).Error(); err != `unknown keys "byname.one.nope", "hidden"` {
		t.Fatalf("error %s", err)
	}
}

// TestKeyStringQuotesWhatABareKeyCannotHold
func TestKeyStringQuotesWhatABareKeyCannotHold(t *testing.T) {
	if got := (Key{"a", "b.c", "d e", "7"}).String(); got != `a."b.c"."d e"."7"` {
		t.Fatalf("got %s", got)
	}
}

// FuzzDocument: a document that parses keeps its bytes; deleting any key it sets
// leaves a shorter document that parses without that key, of whole lines it had
// in their order, and setting one or a key beside it, where the edit is taken,
// leaves it holding the value set, a new key keeping every line it had.
func FuzzDocument(f *testing.F) {
	for _, s := range []string{
		settingsDoc,
		"a = 1\n[t]\nk = [1,\n2] # c\n[[r]]\nx = 1\n[[r]]\nx = 2\n",
		"t.a.b = { c = 1 }\n# end",
		"\"q.k\" = 1\r\n[\"x y\".z]\r\nv = \"#\"\r\n",
		"# file\n\n# a\n[a]\nx = 1 # x\n# end a\n\n\n# b\n\n[b]\ny = [\n# in\n1,\n]\n\n# foot\n",
		"A={}",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := ParseDocument(data)
		if err != nil {
			return
		}
		if string(d.Bytes()) != string(data) {
			t.Fatal("a parsed document changed its bytes")
		}
		for _, s := range d.stmts {
			c, _ := ParseDocument(data)
			ok, err := c.Delete(s.path())
			if err != nil {
				continue
			}
			if !ok {
				t.Fatalf("delete of %s, which the document sets, removed nothing", s.path())
			}
			if has(c.Tree(), s.path()) {
				t.Fatalf("%s still set after its delete:\n%s", s.path(), c.Bytes())
			}
			if len(c.Bytes()) >= len(data) {
				t.Fatalf("delete of %s did not shorten the document", s.path())
			}
			if !keptLines(c.Bytes(), data) {
				t.Fatalf("delete of %s left lines the document did not have:\n%s", s.path(), c.Bytes())
			}
		}
		for _, s := range d.stmts {
			for _, k := range []Key{s.path(), append(s.path()[:len(s.path())-1:len(s.path())-1], "added")} {
				c, _ := ParseDocument(data)
				added := value(c.Tree(), k) == nil
				err := c.Set(k, int64(7))
				if err == nil && value(c.Tree(), k) != int64(7) {
					t.Fatalf("set %s took but reads %v:\n%s", k, value(c.Tree(), k), c.Bytes())
				}
				if err == nil && added && !keptLines(data, c.Bytes()) {
					t.Fatalf("set of new %s lost a line:\n%s", k, c.Bytes())
				}
			}
		}
	})
}

func has(tree map[string]any, k Key) bool { return value(tree, k) != nil }

// keptLines reports every line of part is a line of whole, in whole's order; a
// last line may gain its newline
func keptLines(part, whole []byte) bool {
	lines := strings.Split(string(whole), "\n")
	for _, l := range strings.Split(string(part), "\n") {
		for len(lines) > 0 && lines[0] != l {
			lines = lines[1:]
		}
		if len(lines) == 0 {
			return l == ""
		}
		lines = lines[1:]
	}
	return true
}

// value is what k holds in tree, in the last table of an array of tables
func value(tree map[string]any, k Key) any {
	var cur any = tree
	for _, s := range k {
		m, ok := cur.(map[string]any)
		if !ok {
			if a, ok := cur.([]map[string]any); ok && len(a) > 0 {
				m = a[len(a)-1]
			} else {
				return nil
			}
		}
		if cur, ok = m[s]; !ok {
			return nil
		}
	}
	return cur
}
