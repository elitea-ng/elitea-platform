package connectorapproval

import (
	"encoding/json"
	"testing"
)

// digestVectorSchema is the input schema of the vector the Rust worker
// asserts too (libs/rust/agent-runtime/src/toolkits/mcp_pin.rs): escaping,
// key order and non-ASCII text all reach the digest.
const digestVectorSchema = `{
  "type": "object",
  "properties": {"query": {"type": "string", "description": "a \"quoted\"\n<tag> & ünïcode"}},
  "required": ["query"],
  "additionalProperties": false
}`

const digestVector = "551f3dee2f607af84054d833c4fdd55ffda6091ef0a6a8106da2a0fa018ada3b"

func TestToolDigestMatchesTheWorkersVector(t *testing.T) {
	schema, err := DecodeJSONValue([]byte(digestVectorSchema))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := ToolDigest("search", "Search the docs.\tTabbed.", schema)
	if err != nil {
		t.Fatal(err)
	}
	if digest != digestVector {
		t.Fatalf("digest %s, want %s", digest, digestVector)
	}
	canonical, err := CanonicalJSON(map[string]any{"description": "Search the docs.\tTabbed.", "inputSchema": schema, "name": "search"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"description":"Search the docs.\tTabbed.","inputSchema":{"additionalProperties":false,"properties":{"query":{"description":"a \"quoted\"\n<tag> & ünïcode","type":"string"}},"required":["query"],"type":"object"},"name":"search"}`
	if string(canonical) != want {
		t.Fatalf("canonical\n%s\nwant\n%s", canonical, want)
	}
}

func TestToolDigestChangesWithTheDescriptionOrTheSchema(t *testing.T) {
	base, _ := ToolDigest("search", "a", map[string]any{"type": "object"})
	description, _ := ToolDigest("search", "b", map[string]any{"type": "object"})
	schema, _ := ToolDigest("search", "a", map[string]any{"type": "object", "required": []any{"q"}})
	if base == description || base == schema || description == schema {
		t.Fatal("a changed tool kept its digest")
	}
	numbers, _ := ToolDigest("n", "", map[string]any{"maximum": float64(3), "minimum": float64(1.5)})
	written, _ := DecodeJSONValue([]byte(`{"maximum": 3, "minimum": 1.5}`))
	asWritten, _ := ToolDigest("n", "", written)
	if numbers != asWritten {
		t.Fatal("a number decoded as float64 digests differently from the number as written")
	}
}

// numberVectors are the vectors the worker asserts too (mcp_pin.rs
// numbers_canonicalise_to_one_form): every spelling of one double is one
// canonical number.
var numberVectors = [][2]string{
	{"0.0", "0"}, {"-0", "0"}, {"-0.0", "0"}, {"1.0", "1"},
	{"1e-07", "1e-7"}, {"1E-7", "1e-7"}, {"1E+3", "1000"}, {"0.000001", "0.000001"},
	{"1.5", "1.5"}, {"-2.5e-8", "-2.5e-8"}, {"1e21", "1e+21"},
	{"1.5E300", "1.5e+300"},
	{"9007199254740993", "9007199254740992"},
	{"12345678901234567890", "12345678901234567000"},
	{"0.1", "0.1"}, {"100", "100"},
}

const numberVectorDigest = "b5d7b208c63bd18571ec21160061b959317057c9f5f38786c594ccd262cc7724"

func TestToolDigestNumbersHaveOneCanonicalForm(t *testing.T) {
	for _, vector := range numberVectors {
		value, err := DecodeJSONValue([]byte(vector[0]))
		if err != nil {
			t.Fatal(err)
		}
		canonical, err := CanonicalJSON(value)
		if err != nil || string(canonical) != vector[1] {
			t.Fatalf("%s canonicalises to %s (%v), want %s", vector[0], canonical, err, vector[1])
		}
	}
	if _, err := CanonicalJSON(json.Number("1e400")); err == nil {
		t.Fatal("a number outside the double range canonicalised")
	}
	written, _ := DecodeJSONValue([]byte(`{"type":"object","properties":{"n":{"type":"number","minimum":0.0,"maximum":1E+3,"multipleOf":1e-07,"default":-0}}}`))
	// The same schema through a float64 decode (Main's json.Unmarshal, a
	// JavaScript form) digests the same.
	var asFloat any
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"n":{"type":"number","minimum":0.0,"maximum":1E+3,"multipleOf":1e-07,"default":-0}}}`), &asFloat); err != nil {
		t.Fatal(err)
	}
	a, err := ToolDigest("bounded", "Numbers.", written)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ToolDigest("bounded", "Numbers.", asFloat)
	if a != b {
		t.Fatalf("number text and float64 digest differently: %s %s", a, b)
	}
	if a != numberVectorDigest {
		t.Fatalf("digest %s, want %s", a, numberVectorDigest)
	}
}
