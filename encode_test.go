package toml

import (
	"strings"
	"testing"
)

func TestMarshal_Primitives(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]any
		expected string // partial match or exact
	}{
		{
			name:  "Scalars",
			input: map[string]any{"str": "hello", "int": 42, "bool": true, "float": 3.14},
			expected: `bool = true
float = 3.14
int = 42
str = "hello"`,
		},
		{
			name:  "Quoted Keys",
			input: map[string]any{"123a": 1, "key.dot": 2, "true": 3},
			expected: `"123a" = 1
"key.dot" = 2
"true" = 3`,
		},
		{
			name:     "Inline Arrays",
			input:    map[string]any{"arr": []int{1, 2, 3}},
			expected: `arr = [1, 2, 3]`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Marshal(tc.input)
			if err != nil {
				t.Fatalf("Marshal failed: %v", err)
			}
			out := strings.TrimSpace(string(b))
			if out != tc.expected {
				t.Errorf("Mismatch:\nGot:\n%s\nWant:\n%s", out, tc.expected)
			}
		})
	}
}

func TestMarshal_StructsAndNesting(t *testing.T) {
	type Server struct {
		IP   string `toml:"ip"`
		Port int    `toml:"port"`
	}
	type Config struct {
		Name    string            `toml:"name"`
		Tags    []string          `toml:"tags"`
		Servers []Server          `toml:"servers"` // Array of tables
		Meta    map[string]string `toml:"meta"`    // Table
	}

	input := Config{
		Name: "Production",
		Tags: []string{"web", "api"},
		Servers: []Server{
			{IP: "10.0.0.1", Port: 80},
			{IP: "10.0.0.2", Port: 8080},
		},
		Meta: map[string]string{
			"env": "prod",
			"dc":  "us-east",
		},
	}

	b, err := Marshal(input)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	out := string(b)

	// Verify key order (determinism) and structure
	// Scalars first: name, tags
	if !strings.Contains(out, `name = "Production"`) {
		t.Error("Missing name field")
	}
	if !strings.Contains(out, `tags = ["web", "api"]`) {
		t.Error("Missing tags field")
	}

	// Tables later: meta
	if !strings.Contains(out, `[meta]`) {
		t.Error("Missing [meta] table")
	}
	if !strings.Contains(out, `env = "prod"`) {
		t.Error("Missing env inside meta")
	}

	// Array of tables: servers
	if !strings.Contains(out, `[[servers]]`) {
		t.Error("Missing [[servers]] header")
	}
	if !strings.Contains(out, `ip = "10.0.0.1"`) {
		t.Error("Missing server IP")
	}
}

func TestMarshal_RoundTrip(t *testing.T) {
	// Complex input covering most features
	input := map[string]any{
		"title": "Symmetry Test",
		"owner": map[string]any{
			"name": "Tom",
			"dob":  "1979-05-27T07:32:00Z", // String because date support is limited
		},
		"database": map[string]any{
			"server":         "192.168.1.1",
			"ports":          []any{8001, 8001, 8002},
			"connection_max": 5000,
			"enabled":        true,
		},
		"servers": []map[string]any{
			{"ip": "10.0.0.1", "role": "frontend"},
			{"ip": "10.0.0.2", "role": "backend"},
		},
		"quoted-keys": map[string]any{
			// "1234": "val" would fail because Parser explicitly forbids keys that validly Atoi()
			"1234a": "alphanumeric starting with digit", // Should be quoted in output, accepted by Parser
			"a-b":   "bare key",                         // Should not be quoted
			"true":  "bool key",                         // Should be quoted
		},
	}

	// 1. Marshal
	data, err := Marshal(input)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// 2. Unmarshal back
	var output map[string]any
	if err := Unmarshal(data, &output); err != nil {
		t.Fatalf("Unmarshal failed on generated output: %v\nOutput:\n%s", err, string(data))
	}

	// 3. Compare
	if input["title"] != output["title"] {
		t.Errorf("Title mismatch: %v != %v", input["title"], output["title"])
	}

	dbIn := input["database"].(map[string]any)
	dbOut := output["database"].(map[string]any)
	if dbIn["server"] != dbOut["server"] {
		t.Error("Database server mismatch")
	}

	// Check quoted keys
	qk := output["quoted-keys"].(map[string]any)
	if qk["1234a"] != "alphanumeric starting with digit" {
		t.Error("Failed to round-trip numeric-like key '1234a'")
	}
	if qk["true"] != "bool key" {
		t.Error("Failed to round-trip boolean key 'true'")
	}
}

func TestMarshal_Omitempty(t *testing.T) {
	type Config struct {
		Visible string `toml:"visible"`
		Hidden  string `toml:"hidden,omitempty"`
		Zero    int    `toml:"zero,omitempty"`
	}

	cfg := Config{Visible: "here"}
	b, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	out := string(b)

	if !strings.Contains(out, `visible = "here"`) {
		t.Error("Visible field missing")
	}
	if strings.Contains(out, "hidden") {
		t.Error("Hidden field present but should be omitted")
	}
	if strings.Contains(out, "zero") {
		t.Error("Zero field present but should be omitted")
	}
}

func TestMarshal_SkipNil(t *testing.T) {
	type Config struct {
		Ptr *int `toml:"ptr"`
	}
	cfg := Config{Ptr: nil}
	b, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if len(b) > 0 {
		t.Errorf("Expected empty output for nil pointer, got: %s", string(b))
	}
}