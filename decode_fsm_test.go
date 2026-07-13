package toml

import (
	"testing"
)

// TestDecode_MapPointerValues tests map[string]*Struct decoding
// This is the exact pattern used by RootConfig.States
func TestDecode_MapPointerValues(t *testing.T) {
	data := map[string]any{
		"items": map[string]any{
			"first": map[string]any{
				"name": "alpha",
			},
			"second": map[string]any{
				"name": "beta",
			},
		},
	}

	type Item struct {
		Name string `toml:"name"`
	}
	type Config struct {
		Items map[string]*Item `toml:"items"`
	}

	var cfg Config
	if err := Decode(data, &cfg); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if cfg.Items == nil {
		t.Fatal("Items map is nil")
	}
	if len(cfg.Items) != 2 {
		t.Fatalf("Expected 2 items, got %d", len(cfg.Items))
	}
	if cfg.Items["first"] == nil || cfg.Items["first"].Name != "alpha" {
		t.Errorf("first item mismatch: %+v", cfg.Items["first"])
	}
	if cfg.Items["second"] == nil || cfg.Items["second"].Name != "beta" {
		t.Errorf("second item mismatch: %+v", cfg.Items["second"])
	}
}

// TestUnmarshal_DottedTableToMapPointer tests [parent.child] -> map[string]*Struct
func TestUnmarshal_DottedTableToMapPointer(t *testing.T) {
	input := []byte(`
[states.Gameplay]
parent = "Root"

[states.TrySpawn]
parent = "Gameplay"
`)

	type StateConfig struct {
		Parent string `toml:"parent"`
	}
	type Config struct {
		States map[string]*StateConfig `toml:"states"`
	}

	var cfg Config
	if err := Unmarshal(input, &cfg); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if cfg.States == nil {
		t.Fatal("States map is nil")
	}
	if len(cfg.States) != 2 {
		t.Fatalf("Expected 2 states, got %d", len(cfg.States))
	}
	if cfg.States["Gameplay"] == nil {
		t.Fatal("Gameplay state is nil")
	}
	if cfg.States["Gameplay"].Parent != "Root" {
		t.Errorf("Gameplay.Parent mismatch: %q", cfg.States["Gameplay"].Parent)
	}
}

// TestUnmarshal_InlineTableArray tests arrays of inline tables
func TestUnmarshal_InlineTableArray(t *testing.T) {
	input := []byte(`
[state]
transitions = [
	{ trigger = "EventA", target = "StateB" },
	{ trigger = "EventB", target = "StateC", guard = "CheckX" }
]
`)

	type Transition struct {
		Trigger string `toml:"trigger"`
		Target  string `toml:"target"`
		Guard   string `toml:"guard,omitempty"`
	}
	type State struct {
		Transitions []Transition `toml:"transitions"`
	}
	type Config struct {
		State State `toml:"state"`
	}

	var cfg Config
	if err := Unmarshal(input, &cfg); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if len(cfg.State.Transitions) != 2 {
		t.Fatalf("Expected 2 transitions, got %d", len(cfg.State.Transitions))
	}
	if cfg.State.Transitions[0].Trigger != "EventA" {
		t.Errorf("Transition[0].Trigger mismatch: %q", cfg.State.Transitions[0].Trigger)
	}
	if cfg.State.Transitions[1].Guard != "CheckX" {
		t.Errorf("Transition[1].Guard mismatch: %q", cfg.State.Transitions[1].Guard)
	}
}

func TestUnmarshal_MultilineInlineTable(t *testing.T) {
	input := []byte(`
[state]
config = {
	name = "test",
	nested = { a = 1, b = 2 },
	array = [
		{ x = 10 },
		{ x = 20 }
	]
}
`)

	type Inner struct {
		X int `toml:"x"`
	}
	type Config struct {
		Name   string         `toml:"name"`
		Nested map[string]int `toml:"nested"`
		Array  []Inner        `toml:"array"`
	}
	type State struct {
		Config Config `toml:"config"`
	}
	type Root struct {
		State State `toml:"state"`
	}

	var cfg Root
	if err := Unmarshal(input, &cfg); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if cfg.State.Config.Name != "test" {
		t.Errorf("Name = %q", cfg.State.Config.Name)
	}
	if cfg.State.Config.Nested["a"] != 1 {
		t.Errorf("Nested.a = %d", cfg.State.Config.Nested["a"])
	}
	if len(cfg.State.Config.Array) != 2 || cfg.State.Config.Array[1].X != 20 {
		t.Errorf("Array = %+v", cfg.State.Config.Array)
	}
}

func TestUnmarshal_DeeplyNestedMultiline(t *testing.T) {
	input := []byte(`
transition = { trigger = "Tick", target = "Active", guard = "Or", guard_args = { guards = [
	{ name = "Compare", args = { key = "val", op = "gt", value = 0 } },
	{ name = "Check", args = { flag = true } }
]} }
`)

	type Args struct {
		Key   string `toml:"key"`
		Op    string `toml:"op"`
		Value int    `toml:"value"`
		Flag  bool   `toml:"flag"`
	}
	type Guard struct {
		Name string `toml:"name"`
		Args Args   `toml:"args"`
	}
	type GuardArgs struct {
		Guards []Guard `toml:"guards"`
	}
	type Transition struct {
		Trigger   string    `toml:"trigger"`
		Target    string    `toml:"target"`
		Guard     string    `toml:"guard"`
		GuardArgs GuardArgs `toml:"guard_args"`
	}
	type Root struct {
		Transition Transition `toml:"transition"`
	}

	var cfg Root
	if err := Unmarshal(input, &cfg); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if cfg.Transition.Guard != "Or" {
		t.Errorf("Guard = %q", cfg.Transition.Guard)
	}
	if len(cfg.Transition.GuardArgs.Guards) != 2 {
		t.Fatalf("Guards count = %d", len(cfg.Transition.GuardArgs.Guards))
	}
	if cfg.Transition.GuardArgs.Guards[0].Args.Op != "gt" {
		t.Errorf("Guards[0].Args.Op = %q", cfg.Transition.GuardArgs.Guards[0].Args.Op)
	}
}

// TestUnmarshal_FSMConfigExact tests the exact FSM config structure
func TestUnmarshal_FSMConfigExact(t *testing.T) {
	input := []byte(`
initial = "TrySpawnGold"

[states.Gameplay]
parent = "Root"

[states.TrySpawnGold]
parent = "Gameplay"
on_enter = [
	{ action = "EmitEvent", event = "EventGoldSpawnRequest" }
]
transitions = [
	{ trigger = "EventGoldSpawned", target = "GoldActive" },
	{ trigger = "EventGoldSpawnFailed", target = "GoldRetryWait" }
]

[states.GoldRetryWait]
parent = "Gameplay"
transitions = [
	{ trigger = "Tick", target = "TrySpawnGold", guard = "StateTimeExceeds", guard_args = { ms = 2000 } }
]

[states.GoldActive]
parent = "Gameplay"
transitions = [
	{ trigger = "EventGoldCollected", target = "TrySpawnGold" }
]
`)

	type ActionConfig struct {
		Action string `toml:"action"`
		Event  string `toml:"event,omitempty"`
	}
	type TransitionConfig struct {
		Trigger   string         `toml:"trigger"`
		Target    string         `toml:"target"`
		Guard     string         `toml:"guard,omitempty"`
		GuardArgs map[string]any `toml:"guard_args,omitempty"`
	}
	type StateConfig struct {
		Parent      string             `toml:"parent,omitempty"`
		OnEnter     []ActionConfig     `toml:"on_enter,omitempty"`
		Transitions []TransitionConfig `toml:"transitions,omitempty"`
	}
	type RootConfig struct {
		InitialState string                  `toml:"initial"`
		States       map[string]*StateConfig `toml:"states"`
	}

	var cfg RootConfig
	if err := Unmarshal(input, &cfg); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Check initial
	if cfg.InitialState != "TrySpawnGold" {
		t.Errorf("InitialState mismatch: %q", cfg.InitialState)
	}

	// Check states map
	if cfg.States == nil {
		t.Fatal("States map is nil")
	}
	if len(cfg.States) != 4 {
		t.Errorf("Expected 4 states, got %d", len(cfg.States))
		for k := range cfg.States {
			t.Logf("  Found state: %q", k)
		}
	}

	// Check TrySpawnGold
	tsg := cfg.States["TrySpawnGold"]
	if tsg == nil {
		t.Fatal("TrySpawnGold state is nil")
	}
	if tsg.Parent != "Gameplay" {
		t.Errorf("TrySpawnGold.Parent mismatch: %q", tsg.Parent)
	}
	if len(tsg.OnEnter) != 1 {
		t.Errorf("TrySpawnGold.OnEnter count mismatch: %d", len(tsg.OnEnter))
	}
	if len(tsg.Transitions) != 2 {
		t.Errorf("TrySpawnGold.Transitions count mismatch: %d", len(tsg.Transitions))
	}

	// Check GoldRetryWait guard_args
	grw := cfg.States["GoldRetryWait"]
	if grw == nil {
		t.Fatal("GoldRetryWait state is nil")
	}
	if len(grw.Transitions) != 1 {
		t.Fatalf("GoldRetryWait.Transitions count mismatch: %d", len(grw.Transitions))
	}
	if grw.Transitions[0].GuardArgs == nil {
		t.Error("GuardArgs is nil")
	} else if ms, ok := grw.Transitions[0].GuardArgs["ms"]; !ok {
		t.Error("GuardArgs missing 'ms' key")
	} else if msInt, ok := ms.(int); !ok || msInt != 2000 {
		t.Errorf("GuardArgs.ms mismatch: %T %v", ms, ms)
	}
}

// TestParser_DottedTableStructure verifies parser output for dotted tables
func TestParser_DottedTableStructure(t *testing.T) {
	input := []byte(`
[states.Alpha]
name = "first"

[states.Beta]
name = "second"
`)

	p := NewParser(input)
	result, err := p.Parse()
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// Check raw parser output structure
	states, ok := result["states"]
	if !ok {
		t.Fatal("'states' key missing from parser output")
	}

	statesMap, ok := states.(map[string]any)
	if !ok {
		t.Fatalf("'states' is not map[string]any, got %T", states)
	}

	if len(statesMap) != 2 {
		t.Errorf("Expected 2 states in parser output, got %d", len(statesMap))
	}

	alpha, ok := statesMap["Alpha"]
	if !ok {
		t.Error("'Alpha' key missing")
	}
	alphaMap, ok := alpha.(map[string]any)
	if !ok {
		t.Fatalf("'Alpha' is not map[string]any, got %T", alpha)
	}
	if alphaMap["name"] != "first" {
		t.Errorf("Alpha.name mismatch: %v", alphaMap["name"])
	}
}

// TestDecode_MapNilInitialization verifies map initialization during decode
func TestDecode_MapNilInitialization(t *testing.T) {
	data := map[string]any{
		"items": map[string]any{
			"a": map[string]any{"val": 1},
		},
	}

	type Item struct {
		Val int `toml:"val"`
	}
	type Config struct {
		Items map[string]*Item `toml:"items"` // nil initially
	}

	var cfg Config
	// cfg.Items is nil here

	if err := Decode(data, &cfg); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if cfg.Items == nil {
		t.Fatal("Decode did not initialize nil map")
	}
}

func TestUnmarshal_ExtremeComplexity(t *testing.T) {
	input := []byte(`
# Root level mixed types
version = "2.0.0-beta"
debug = true
tick_rate = 144
delta_time = 0.00694

# Deep dotted header (5 levels)
[engine.renderer.pipeline.stage.config]
name = "deferred"
priority = 1
enabled = true
scale_factor = 1.5e-2
tags = ["lighting", "shadows", "post-fx"]

# Nested inline table inside dotted section
[engine.renderer.pipeline.stage.config.viewport]
width = 1920
height = 1080
settings = { vsync = true, hdr = false, gamma = 2.2 }

# Hyphenated keys at multiple levels
[engine.audio-system.spatial-audio]
enabled = true
max-sources = 64
falloff-curve = "exponential"
rolloff-factor = 1.0e+0

# Map with pointer values using dotted headers
[game.entities.player]
health = 100
position.x = 0.0
position.y = -9.81e-1
position.z = 0.0
tags = ["controllable", "damageable"]
inventory = { slots = 20, weight_limit = 150.5 }

[game.entities.enemy-boss]
health = 5000
position.x = 100.0
position.y = 0.0
position.z = -50.0
tags = ["hostile", "boss", "damageable"]
ai = { aggression = 0.9, patrol_radius = 25 }

[game.entities."فارسی-test"]
health = 1
position.x = 1.0
position.y = 1.0
position.z = 1.0
tags = []

# Nested map of maps
[game.levels.level-01.zones.spawn-area]
bounds.min.x = -10
bounds.min.y = 0
bounds.min.z = -10
bounds.max.x = 10
bounds.max.y = 5
bounds.max.z = 10
enemy_count = 0
is_safe = true

[game.levels.level-01.zones.combat-zone]
bounds.min.x = 50
bounds.min.y = 0
bounds.min.z = 50
bounds.max.x = 150
bounds.max.y = 20
bounds.max.z = 150
enemy_count = 25
is_safe = false

# Array of tables with nested complexity
[[game.waves]]
id = 1
delay_ms = 0
spawns = [
	{ entity = "enemy-grunt", count = 5, position = { x = 10.0, y = 0.0, z = 10.0 } },
	{ entity = "enemy-scout", count = 3, position = { x = -10.0, y = 0.0, z = 10.0 } }
]

[[game.waves]]
id = 2
delay_ms = 30000
spawns = [
	{ entity = "enemy-boss", count = 1, position = { x = 0.0, y = 0.0, z = 50.0 } }
]

# Deeply nested with mixed inline and standard tables
[physics.collision.layers.player-projectiles]
mask = 0b1010
priority = 10
callbacks.on_enter = "HandleProjectileHit"
callbacks.on_exit = "CleanupProjectile"

[physics.collision.layers.environment]
mask = 0b1111
priority = 1
callbacks.on_enter = "HandleCollision"
callbacks.on_exit = ""

# Scientific notation stress test
[constants]
planck = 6.62607015e-34
c = 2.998e+8
epsilon_0 = 8.854e-12
very_small = 1e-100
very_large = 1e+100
negative_exp = -5.5e-10

# Empty and edge cases mixed in
[edge.cases]
empty_string = ""
empty_array = []
empty_inline = {}
zero_int = 0
zero_float = 0.0
negative_int = -42
negative_float = -273.15
unicode_value = "日本語テスト 🎮 Ελληνικά"
hex_val = 0xDEAD
octal_val = 0o755
binary_val = 0b1010
`)

	type Vec3 struct {
		X float64 `toml:"x"`
		Y float64 `toml:"y"`
		Z float64 `toml:"z"`
	}

	type Bounds struct {
		Min Vec3 `toml:"min"`
		Max Vec3 `toml:"max"`
	}

	type ViewportSettings struct {
		Vsync bool    `toml:"vsync"`
		HDR   bool    `toml:"hdr"`
		Gamma float64 `toml:"gamma"`
	}

	type Viewport struct {
		Width    int              `toml:"width"`
		Height   int              `toml:"height"`
		Settings ViewportSettings `toml:"settings"`
	}

	type StageConfig struct {
		Name        string   `toml:"name"`
		Priority    int      `toml:"priority"`
		Enabled     bool     `toml:"enabled"`
		ScaleFactor float64  `toml:"scale_factor"`
		Tags        []string `toml:"tags"`
		Viewport    Viewport `toml:"viewport"`
	}

	type Stage struct {
		Config StageConfig `toml:"config"`
	}

	type Pipeline struct {
		Stage Stage `toml:"stage"`
	}

	type Renderer struct {
		Pipeline Pipeline `toml:"pipeline"`
	}

	type SpatialAudio struct {
		Enabled       bool    `toml:"enabled"`
		MaxSources    int     `toml:"max-sources"`
		FalloffCurve  string  `toml:"falloff-curve"`
		RolloffFactor float64 `toml:"rolloff-factor"`
	}

	type AudioSystem struct {
		SpatialAudio SpatialAudio `toml:"spatial-audio"`
	}

	type Engine struct {
		Renderer    Renderer    `toml:"renderer"`
		AudioSystem AudioSystem `toml:"audio-system"`
	}

	type EntityConfig struct {
		Health    int            `toml:"health"`
		Position  Vec3           `toml:"position"`
		Tags      []string       `toml:"tags"`
		Inventory map[string]any `toml:"inventory,omitempty"`
		AI        map[string]any `toml:"ai,omitempty"`
	}

	type Zone struct {
		Bounds     Bounds `toml:"bounds"`
		EnemyCount int    `toml:"enemy_count"`
		IsSafe     bool   `toml:"is_safe"`
	}

	type Level struct {
		Zones map[string]*Zone `toml:"zones"`
	}

	type SpawnPoint struct {
		Entity   string         `toml:"entity"`
		Count    int            `toml:"count"`
		Position map[string]any `toml:"position"`
	}

	type Wave struct {
		ID      int          `toml:"id"`
		DelayMs int          `toml:"delay_ms"`
		Spawns  []SpawnPoint `toml:"spawns"`
	}

	type Game struct {
		Entities map[string]*EntityConfig `toml:"entities"`
		Levels   map[string]*Level        `toml:"levels"`
		Waves    []*Wave                  `toml:"waves"`
	}

	type Callbacks struct {
		OnEnter string `toml:"on_enter"`
		OnExit  string `toml:"on_exit"`
	}

	type CollisionLayer struct {
		Mask      int       `toml:"mask"`
		Priority  int       `toml:"priority"`
		Callbacks Callbacks `toml:"callbacks"`
	}

	type Collision struct {
		Layers map[string]*CollisionLayer `toml:"layers"`
	}

	type Physics struct {
		Collision Collision `toml:"collision"`
	}

	type Constants struct {
		Planck      float64 `toml:"planck"`
		C           float64 `toml:"c"`
		Epsilon0    float64 `toml:"epsilon_0"`
		VerySmall   float64 `toml:"very_small"`
		VeryLarge   float64 `toml:"very_large"`
		NegativeExp float64 `toml:"negative_exp"`
	}

	type EdgeCases struct {
		EmptyString   string         `toml:"empty_string"`
		EmptyArray    []any          `toml:"empty_array"`
		EmptyInline   map[string]any `toml:"empty_inline"`
		ZeroInt       int            `toml:"zero_int"`
		ZeroFloat     float64        `toml:"zero_float"`
		NegativeInt   int            `toml:"negative_int"`
		NegativeFloat float64        `toml:"negative_float"`
		UnicodeValue  string         `toml:"unicode_value"`
		HexVal        int            `toml:"hex_val"`
		OctalVal      int            `toml:"octal_val"`
		BinaryVal     int            `toml:"binary_val"`
	}

	type Edge struct {
		Cases EdgeCases `toml:"cases"`
	}

	type Config struct {
		Version   string    `toml:"version"`
		Debug     bool      `toml:"debug"`
		TickRate  int       `toml:"tick_rate"`
		DeltaTime float64   `toml:"delta_time"`
		Engine    Engine    `toml:"engine"`
		Game      Game      `toml:"game"`
		Physics   Physics   `toml:"physics"`
		Constants Constants `toml:"constants"`
		Edge      Edge      `toml:"edge"`
	}

	var cfg Config
	if err := Unmarshal(input, &cfg); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Root level
	if cfg.Version != "2.0.0-beta" {
		t.Errorf("Version = %q", cfg.Version)
	}
	if !cfg.Debug {
		t.Error("Debug should be true")
	}
	if cfg.TickRate != 144 {
		t.Errorf("TickRate = %d", cfg.TickRate)
	}

	// 5-level deep dotted header
	sc := cfg.Engine.Renderer.Pipeline.Stage.Config
	if sc.Name != "deferred" {
		t.Errorf("Stage.Config.Name = %q", sc.Name)
	}
	if sc.ScaleFactor != 1.5e-2 {
		t.Errorf("ScaleFactor = %e", sc.ScaleFactor)
	}
	if len(sc.Tags) != 3 || sc.Tags[1] != "shadows" {
		t.Errorf("Stage tags = %v", sc.Tags)
	}
	if sc.Viewport.Width != 1920 {
		t.Errorf("Viewport.Width = %d", sc.Viewport.Width)
	}
	if sc.Viewport.Settings.Gamma != 2.2 {
		t.Errorf("Viewport.Settings.Gamma = %f", sc.Viewport.Settings.Gamma)
	}

	// Hyphenated keys
	sa := cfg.Engine.AudioSystem.SpatialAudio
	if sa.MaxSources != 64 {
		t.Errorf("MaxSources = %d", sa.MaxSources)
	}
	if sa.FalloffCurve != "exponential" {
		t.Errorf("FalloffCurve = %q", sa.FalloffCurve)
	}

	// Map pointer values with dotted keys inside
	player := cfg.Game.Entities["player"]
	if player == nil {
		t.Fatal("player entity nil")
	}
	if player.Health != 100 {
		t.Errorf("player.Health = %d", player.Health)
	}
	if player.Position.Y != -9.81e-1 {
		t.Errorf("player.Positions.Y = %e", player.Position.Y)
	}
	if len(player.Tags) != 2 {
		t.Errorf("player.Tags = %v", player.Tags)
	}

	boss := cfg.Game.Entities["enemy-boss"]
	if boss == nil {
		t.Fatal("enemy-boss entity nil")
	}
	if boss.Health != 5000 {
		t.Errorf("boss.Health = %d", boss.Health)
	}

	// Unicode key (edge case)
	unicode := cfg.Game.Entities["فارسی-test"]
	if unicode == nil {
		t.Fatal("unicode entity nil")
	}
	if unicode.Health != 1 {
		t.Errorf("unicode.Health = %d", unicode.Health)
	}

	// Deeply nested map of maps
	lvl := cfg.Game.Levels["level-01"]
	if lvl == nil {
		t.Fatal("level-01 nil")
	}
	spawn := lvl.Zones["spawn-area"]
	if spawn == nil {
		t.Fatal("spawn-area nil")
	}
	if spawn.Bounds.Min.X != -10 {
		t.Errorf("spawn.Bounds.Min.X = %f", spawn.Bounds.Min.X)
	}
	if spawn.Bounds.Max.Y != 5 {
		t.Errorf("spawn.Bounds.Max.Y = %f", spawn.Bounds.Max.Y)
	}
	if !spawn.IsSafe {
		t.Error("spawn.IsSafe should be true")
	}

	combat := lvl.Zones["combat-zone"]
	if combat == nil {
		t.Fatal("combat-zone nil")
	}
	if combat.EnemyCount != 25 {
		t.Errorf("combat.EnemyCount = %d", combat.EnemyCount)
	}

	// Array of tables with pointer slice
	if len(cfg.Game.Waves) != 2 {
		t.Fatalf("Waves count = %d", len(cfg.Game.Waves))
	}
	w1 := cfg.Game.Waves[0]
	if w1.ID != 1 || w1.DelayMs != 0 {
		t.Errorf("Wave[0] = %+v", w1)
	}
	if len(w1.Spawns) != 2 {
		t.Errorf("Wave[0].Spawns count = %d", len(w1.Spawns))
	}
	if w1.Spawns[0].Entity != "enemy-grunt" || w1.Spawns[0].Count != 5 {
		t.Errorf("Wave[0].Spawns[0] = %+v", w1.Spawns[0])
	}

	w2 := cfg.Game.Waves[1]
	if w2.DelayMs != 30000 {
		t.Errorf("Wave[1].DelayMs = %d", w2.DelayMs)
	}

	// Collision layers map
	projLayer := cfg.Physics.Collision.Layers["player-projectiles"]
	if projLayer == nil {
		t.Fatal("player-projectiles layer nil")
	}
	if projLayer.Mask != 0b1010 {
		t.Errorf("projLayer.Mask = %d", projLayer.Mask)
	}
	if projLayer.Callbacks.OnEnter != "HandleProjectileHit" {
		t.Errorf("projLayer.Callbacks.OnEnter = %q", projLayer.Callbacks.OnEnter)
	}

	// Scientific notation
	if cfg.Constants.Planck != 6.62607015e-34 {
		t.Errorf("Planck = %e", cfg.Constants.Planck)
	}
	if cfg.Constants.C != 2.998e+8 {
		t.Errorf("C = %e", cfg.Constants.C)
	}
	if cfg.Constants.VerySmall != 1e-100 {
		t.Errorf("VerySmall = %e", cfg.Constants.VerySmall)
	}
	if cfg.Constants.NegativeExp != -5.5e-10 {
		t.Errorf("NegativeExp = %e", cfg.Constants.NegativeExp)
	}

	// Edge cases
	if cfg.Edge.Cases.EmptyString != "" {
		t.Errorf("EmptyString = %q", cfg.Edge.Cases.EmptyString)
	}
	if len(cfg.Edge.Cases.EmptyArray) != 0 {
		t.Errorf("EmptyArray = %v", cfg.Edge.Cases.EmptyArray)
	}
	if len(cfg.Edge.Cases.EmptyInline) != 0 {
		t.Errorf("EmptyInline = %v", cfg.Edge.Cases.EmptyInline)
	}
	if cfg.Edge.Cases.NegativeInt != -42 {
		t.Errorf("NegativeInt = %d", cfg.Edge.Cases.NegativeInt)
	}
	if cfg.Edge.Cases.NegativeFloat != -273.15 {
		t.Errorf("NegativeFloat = %f", cfg.Edge.Cases.NegativeFloat)
	}
	if cfg.Edge.Cases.UnicodeValue != "日本語テスト 🎮 Ελληνικά" {
		t.Errorf("UnicodeValue = %q", cfg.Edge.Cases.UnicodeValue)
	}
	if cfg.Edge.Cases.HexVal != 0xDEAD {
		t.Errorf("HexVal = %d, want %d", cfg.Edge.Cases.HexVal, 0xDEAD)
	}
	if cfg.Edge.Cases.OctalVal != 0o755 {
		t.Errorf("OctalVal = %d, want %d", cfg.Edge.Cases.OctalVal, 0o755)
	}
	if cfg.Edge.Cases.BinaryVal != 0b1010 {
		t.Errorf("BinaryVal = %d, want %d", cfg.Edge.Cases.BinaryVal, 0b1010)
	}
}