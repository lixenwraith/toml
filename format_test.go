package toml

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func formats(t *testing.T, f Formatter, in, want string) {
	t.Helper()
	got, err := f.Format([]byte(in))
	if err != nil {
		t.Fatalf("format %q: %v", in, err)
	}
	if string(got) != want {
		t.Fatalf("format of:\n%s\ngot:\n%s\nwant:\n%s", in, got, want)
	}
}

// TestFormatSpacesAsMarshal: one space around = and after a comma, none inside
// an array's brackets, one inside an inline table's; a value's lines a tab deeper
// than the line it opens on, a trailing comma exactly when the closing bracket
// has its own line; a blank line run kept as one; LF line ends, no trailing blank.
func TestFormatSpacesAsMarshal(t *testing.T) {
	formats(t, Formatter{}, "  a=[1,2,   3]\r\nb = {x=1,y = {z=[ ]}, w = { }}\r\n\r\n\r\n"+
		"c = [\n    1, # one\n  2\n]\nd = [ [1,\n2], 3, ]   # note   \n[ t . \"u v\" ]\n[[ arr ]]\nk = {\na = 1 }\n\n",
		"a = [1, 2, 3]\nb = { x = 1, y = { z = [] }, w = {} }\n\n"+
			"c = [\n\t1, # one\n\t2,\n]\nd = [[1,\n\t2], 3] # note\n[t.\"u v\"]\n[[arr]]\nk = {\n\ta = 1 }\n")
}

// TestFormatKeepsCommentsWithTheirOwners: a comment stays with what fewer blank
// lines part it from as a Document reads it, and the output's blank lines say so
// again: two follow a group belonging to what is above it, after a blank line.
// Comments inside a value keep their place among its elements.
func TestFormatKeepsCommentsWithTheirOwners(t *testing.T) {
	// The request's example: a dotted key under [section2] is section2's
	formats(t, Formatter{}, "# comment s1\n[section1]\npart1=1 # comment s1p1\n\n#comment s2\n[section2]\n"+
		"# comment s2p2\npart1=1\n\n# comment s1p2\nsection1.part2 = 5\n",
		"# comment s1\n[section1]\npart1 = 1 # comment s1p1\n\n#comment s2\n[section2]\n"+
			"# comment s2p2\npart1 = 1\n\n# comment s1p2\nsection1.part2 = 5\n")
	formats(t, Formatter{}, "# file\n\n# also file\n\n\n[a]\nx = 1\n\n# about a\n\n\n# about b\n\n[b]\n"+
		"y = [ # open\n\n  # first\n  1,\n\n  # second\n  2 # two\n  , 3,\n  # last\n\n]\n\n# end\n",
		"# file\n\n# also file\n\n\n[a]\nx = 1\n\n# about a\n\n\n# about b\n\n[b]\n"+
			"y = [ # open\n\t# first\n\t1,\n\n\t# second\n\t2, # two\n\t3,\n\t# last\n]\n\n# end\n")
}

// TestFormatWritesATableOnce: a header opening a table again gives its keys to
// the first header, a blank line apart, its comments above them; a dotted key
// reaching a table that has a header moves there, spelled from it, its own
// comments along. Arrays of tables, and tables under them, stay as written.
func TestFormatWritesATableOnce(t *testing.T) {
	formats(t, Formatter{}, "# comment s1\n[section1]\npart1=1 # comment s1p1\n\n#comment s2\n[section2]\n"+
		"# comment s2p2\npart1=1\n\n# comment s1p2\n[section1]\npart2 = 5\n",
		"# comment s1\n[section1]\npart1 = 1 # comment s1p1\n\n# comment s1p2\npart2 = 5\n\n"+
			"#comment s2\n[section2]\n# comment s2p2\npart1 = 1\n")
	formats(t, Formatter{}, "# about part2\nsection1.part2 = 5\na.b.c = 1\n\n[a.b]\nd = 2\n[section1]\npart1 = 1\n[a]\nb.e = 3\n",
		"[a.b]\nd = 2\nc = 1\ne = 3\n[section1]\npart1 = 1\n# about part2\npart2 = 5\n[a]\n")
	formats(t, Formatter{}, "[[s]]\nx = 1\n[s.t]\ny = 1\n[[s]]\nx = 2\n[s.t]\ny = 2\n",
		"[[s]]\nx = 1\n[s.t]\ny = 1\n[[s]]\nx = 2\n[s.t]\ny = 2\n")
}

// TestFormatterOptions: Indent sets a level's characters, IndentTables indents a
// table a level per table it is under whose header comes first, Align lines up
// the = and the comments of keys on consecutive lines, a comment or blank line
// ending the run.
func TestFormatterOptions(t *testing.T) {
	formats(t, Formatter{Indent: "  ", IndentTables: true},
		"[servers]\na = 1\n[servers.alpha]\nports = [\n80,\n]\n[servers.alpha.deep]\nx = 1\n[other.deep]\ny = 2\n",
		"[servers]\na = 1\n  [servers.alpha]\n  ports = [\n    80,\n  ]\n    [servers.alpha.deep]\n    x = 1\n[other.deep]\ny = 2\n")
	formats(t, Formatter{Align: true}, "[play]\nsfx = true # on\nmusic = true\nsfx_volume = 0.5 # half\n# lead\nmouse = true # m\n\nhud = false\nwait = true # w\n",
		"[play]\nsfx        = true # on\nmusic      = true\nsfx_volume = 0.5  # half\n# lead\nmouse = true # m\n\nhud  = false\nwait = true  # w\n")
}

// TestFormatGrowsBoundedly: past 16 levels a line indents no deeper, and a key or
// line too long to align stands alone, so however deep or long its source, the
// result stays within a fixed multiple of it: a two-byte line such as "]" takes
// 16 tabs and a comma at most
func TestFormatGrowsBoundedly(t *testing.T) {
	deep := "a = " + strings.Repeat("[\n", 999) + "1" + strings.Repeat("\n]", 999) + "\n"
	var long strings.Builder
	long.WriteString("\"" + strings.Repeat("k", 4096) + "\" = 1 # c\n")
	for i := range 1000 {
		fmt.Fprintf(&long, "k%d = %d # c\n", i, i)
	}
	for _, doc := range []string{deep, long.String()} {
		out, err := Formatter{Align: true}.Format([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		if len(out) > 10*len(doc) {
			t.Errorf("%d bytes grew to %d", len(doc), len(out))
		}
		if bytes.Contains(out, []byte(strings.Repeat("\t", 17))) {
			t.Error("a line indents past 16 levels")
		}
	}
}

// TestFormatRefusesWhatItCannotKeep: a document Parse refuses, a comment holding
// a rune that can hide the text after it, and an indent that is not up to 8
// blanks are refused; such a rune in a string or quoted key is written escaped.
func TestFormatRefusesWhatItCannotKeep(t *testing.T) {
	for _, c := range []struct {
		f   Formatter
		doc string
	}{
		{Formatter{}, "a = \n"},
		{Formatter{}, "a = 1 # \u202e reversed\n"},
		{Formatter{}, "# \u2066isolate\na = 1\n"},
		{Formatter{}, "# next\u2028line\na = 1\n"},
		{Formatter{Indent: "# "}, "a = [\n1,\n]\n"},
		{Formatter{Indent: strings.Repeat(" ", 9)}, "a = 1\n"},
	} {
		if out, err := c.f.Format([]byte(c.doc)); err == nil {
			t.Errorf("format of %q taken:\n%s", c.doc, out)
		}
	}
	formats(t, Formatter{}, "s = \"a\u202eb\"\n\"k\u2066\" = \"\\\u202e\"\n", "s = \"a\\u202Eb\"\n\"k\\u2066\" = \"\\\\\\u202E\"\n")
}

// FuzzFormat: whatever Parse takes Format writes, unless a comment holds a rune
// that can hide text, with every option: the result reads as the document does,
// formats to itself, keeps every comment once and has no CR or trailing blank.
func FuzzFormat(f *testing.F) {
	for _, s := range []string{
		settingsDoc,
		"# a\n\n# b\n\n\n[t]\nk = 1\n# t's\n\n\n# u\n[u]\nv = [ # o\n\n1, # one\n# c\n2\n,3]\n\n# end\n",
		"x.y = 1\n[x]\nz = 2\n[w]\n[x] # again\nq = { a.b = 1, c = [ {d = 1} ] }\n",
		"[[s]]\n[s.t]\na = 1\n[[s]]\n[s.t]\nb = 2\n",
		"k = \"\\u00e9 \\\\ \\\" \\t\"\r\n\"q k\" = \"\u202e\"\r\n",
		"a = [[[[1]]],\n[[2,\n3]]]\nb = {\nc = {\nd = 1,\n},\n}\n",
		"\n\n# only\n\n# comments\n\n",
		"a=1",
		"[a]\n[a.b]\n[a]\nc = 1\n",
		"[A]\n\n#\n\n[A]",
		"[a]\n[a.A]\n[a]#",
		"[a]\n[a.A]\n[a.A]#",
		"[\"a b\".c]\nx = 1\n[\"a b\"]\nc.y = 2\n",
		"[A]\n\n#\n\n\n[A0]\n[A]#",
		"# p\n\n[a]\nx = 1\n# t\n\n# u\n\n\n[b]\n# lb\n[a] # n\n# la\ny = 2\n\n# v\n\n\n[a.c]\n[a]\n\n# w\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		tree, err := NewParser(data).Parse()
		if err != nil || hiddenInComments(data) != nil {
			return
		}
		for _, fm := range []Formatter{{}, {Indent: "  ", IndentTables: true, Align: true}} {
			out, err := fm.Format(data)
			if err != nil {
				t.Fatalf("%+v refused %q: %v", fm, data, err)
			}
			if got, _ := NewParser(out).Parse(); !reflect.DeepEqual(got, tree) {
				t.Fatalf("%+v changed the values:\n%s", fm, out)
			}
			if again, err := fm.Format(out); err != nil || !bytes.Equal(again, out) {
				t.Fatalf("%+v does not format to itself (%v):\n%s\nthen:\n%s", fm, err, out, again)
			}
			if a, b := comments(data), comments(out); !slices.Equal(a, b) {
				t.Fatalf("%+v comments %q became %q:\n%s", fm, a, b, out)
			}
			if bytes.Contains(out, []byte("\r")) || bytes.Contains(out, []byte(" \n")) || bytes.Contains(out, []byte("\t\n")) ||
				bytes.Contains(out, []byte("\n\n\n\n")) || len(out) > 0 && out[len(out)-1] != '\n' {
				t.Fatalf("%+v wrote a stray blank:\n%q", fm, out)
			}
			if len(out) > 48*len(data)+64 {
				t.Fatalf("%+v grew %d bytes to %d", fm, len(data), len(out))
			}
		}
	})
}

// comments lists a document's comments, trailing blanks dropped, sorted
func comments(data []byte) []string {
	var out []string
	lx := NewLexer(data)
	for t := lx.NextToken(); t.Type != TokenEOF; t = lx.NextToken() {
		if t.Type == TokenComment {
			out = append(out, strings.TrimRight(t.Literal, " \t"))
		}
	}
	slices.Sort(out)
	return out
}
