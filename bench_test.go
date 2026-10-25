package toml

import (
	"fmt"
	"strings"
	"testing"
)

// benchSettings is a settings file as people write one: comment blocks over
// tables of scalars
const benchSettings = `# settings: what a flag would otherwise say on every run. A flag given on the
# command line wins; a key left out keeps the default written here.

[paths]
# A configuration root searched before the user root.
root = ""
# Where logs, journals and music go; empty is the platform default.
log = ""
journal = ""
music = ""

[audio]
# Mixer period and output latency, in milliseconds.
buffer_ms = 50

[presentation]
window = false
maximized = false
cell = 0

# What the configuration menu changes in a run, written back so the next starts
# as this one was left.
[play]
sfx = true
music = true
sfx_volume = 0.5
auto_fire = "on"
mouse = true
hud = false
`

// benchPayload is an event payload a command or script carries
const benchPayload = `_p = { entity = 7, x = 3, y = -2, pointer = true, name = "cursor" }`

// scenarioDoc writes n states as a scenario file does: headers, arrays of inline
// tables across lines, comments between elements
func scenarioDoc(n int) []byte {
	var b strings.Builder
	b.WriteString("# A generated region: states and their transitions\n\n")
	for i := range n {
		fmt.Fprintf(&b, `# --- state %d ---

[states.S%d]
parent = "S%d"
on_enter = [
    { action = "EmitEvent", event = "EventSpawn%d" },
]
transitions = [
    # the usual way on
    { trigger = "EventDone", target = "S%d", guard = "StateTimeExceeds", guard_args = { ms = %d } },
    { trigger = "Tick", target = "S%d", weight = 0.25 },
]

`, i, i, i/2, i, i+1, 100+i, i)
	}
	return []byte(b.String())
}

type benchTransition struct {
	Trigger   string         `toml:"trigger"`
	Target    string         `toml:"target"`
	Guard     string         `toml:"guard,omitempty"`
	GuardArgs map[string]any `toml:"guard_args,omitempty"`
	Weight    float64        `toml:"weight,omitempty"`
}

type benchState struct {
	Parent      string            `toml:"parent"`
	OnEnter     []map[string]any  `toml:"on_enter,omitempty"`
	Transitions []benchTransition `toml:"transitions"`
}

type benchScenario struct {
	States map[string]benchState `toml:"states"`
}

type benchEvent struct {
	Entity  int64  `toml:"entity"`
	X       int    `toml:"x"`
	Y       int    `toml:"y"`
	Pointer bool   `toml:"pointer"`
	Name    string `toml:"name"`
}

func BenchmarkParse(b *testing.B) {
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"payload", []byte(benchPayload)},
		{"settings", []byte(benchSettings)},
		{"scenario", scenarioDoc(64)},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(c.data)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := NewParser(c.data).Parse(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkPayload is a command's payload from text to its typed struct
func BenchmarkPayload(b *testing.B) {
	data := []byte(benchPayload)
	b.ReportAllocs()
	for b.Loop() {
		tree, err := NewParser(data).Parse()
		if err != nil {
			b.Fatal(err)
		}
		var ev benchEvent
		if err := Decode(tree["_p"], &ev); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecode(b *testing.B) {
	tree, err := NewParser(scenarioDoc(64)).Parse()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		var s benchScenario
		if err := Decode(tree, &s); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMarshal(b *testing.B) {
	var s benchScenario
	if err := Unmarshal(scenarioDoc(64), &s); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Marshal(s); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseDocument(b *testing.B) {
	data := []byte(benchSettings)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseDocument(data); err != nil {
			b.Fatal(err)
		}
	}
}
