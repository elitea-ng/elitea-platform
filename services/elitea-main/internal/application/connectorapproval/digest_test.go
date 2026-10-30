package connectorapproval

import (
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
