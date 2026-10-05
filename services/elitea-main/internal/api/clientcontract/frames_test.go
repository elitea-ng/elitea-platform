package clientcontract_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/clientcontract"
)

const framesFixture = `openapi: "3.0.3"
info:
  title: frames
  version: "1"
  x-elitea-client-contract: "1.1"
x-elitea-client-frames:
  agent_tool_start: '#/components/schemas/ToolStart'
paths: {}
components:
  schemas:
    ToolStart:
      type: object
      required: [type]
      properties:
        type: { type: string }
        tool_name: { type: string }
`

// TestFrameCatalogueIsComparedLikeAResponse: a frame a client renders is a
// promise like a response body — a frame type or one of its properties may not
// disappear, and a new optional property is additive.
func TestFrameCatalogueIsComparedLikeAResponse(t *testing.T) {
	base := normalizeYAML(t, framesFixture)
	if base.Frames["agent_tool_start"] == nil || base.Frames["agent_tool_start"].Properties["tool_name"] == nil {
		t.Fatalf("the catalogue was not normalized: %+v", base.Frames)
	}
	dropped := normalizeYAML(t, strings.Replace(framesFixture, "        tool_name: { type: string }\n", "", 1))
	if problems := clientcontract.Compare(base, dropped); !strings.Contains(strings.Join(problems, "\n"), "frame agent_tool_start data.tool_name: property removed") {
		t.Fatalf("a removed frame property passed: %v", problems)
	}
	gone := normalizeYAML(t, strings.Replace(framesFixture, "  agent_tool_start: '#/components/schemas/ToolStart'\n", "  other: '#/components/schemas/ToolStart'\n", 1))
	if problems := clientcontract.Compare(base, gone); !strings.Contains(strings.Join(problems, "\n"), "frame agent_tool_start: removed") {
		t.Fatalf("a removed frame type passed: %v", problems)
	}
	grown := normalizeYAML(t, strings.Replace(framesFixture, "        tool_name: { type: string }\n", "        tool_name: { type: string }\n        tool_run_id: { type: string }\n", 1))
	if problems := clientcontract.Compare(base, grown); len(problems) > 0 {
		t.Fatalf("an additive frame was refused: %v", problems)
	}
	loader := openapi3.NewLoader()
	broken, err := loader.LoadFromData([]byte(strings.Replace(framesFixture, "schemas/ToolStart'", "schemas/Missing'", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientcontract.Normalize(broken); err == nil {
		t.Fatal("a frame naming a schema the document lacks was accepted")
	}
}

// framesDir holds frames captured from BOTH workers by their own unit tests
// (services/elitea-worker-python/tests/unit/test_client_frames.py and
// services/elitea-worker-rust/src/agents/client_frames_tests.rs). Each worker
// test fails when what it emits drifts from its file; this test fails when a
// file no longer satisfies the catalogue. Together: a worker cannot change a
// frame a client renders without the catalogue (and so the lock) noticing.
const framesDir = "../../../../../testdata/client-frames"

// TestClientFrameCatalogueAcceptsWorkerFrames validates every captured frame
// against the schema x-elitea-client-frames names for its type.
func TestClientFrameCatalogueAcceptsWorkerFrames(t *testing.T) {
	doc := loadSpec(t)
	refs, _ := doc.Extensions[clientcontract.FramesExtension].(map[string]any)
	if len(refs) == 0 {
		t.Fatalf("v2.yaml has no %s", clientcontract.FramesExtension)
	}
	// The validator is not vacuous: a tool frame whose call key is a number,
	// or that has no response_metadata, is refused.
	startSchema := doc.Components.Schemas["ClientFrameAgentToolStart"].Value
	for _, bad := range []map[string]any{
		{"type": "agent_tool_start", "response_metadata": map[string]any{"tool_name": "x", "tool_run_id": float64(7)}},
		{"type": "agent_tool_start"},
	} {
		if err := startSchema.VisitJSON(bad); err == nil {
			t.Fatalf("the catalogue accepted a malformed frame %v", bad)
		}
	}
	covered := map[string]map[string]bool{}
	for _, worker := range []string{"python", "rust"} {
		covered[worker] = map[string]bool{}
		data, err := os.ReadFile(filepath.Join(framesDir, worker+".json"))
		if err != nil {
			t.Fatalf("%s frames: %v", worker, err)
		}
		var frames map[string]json.RawMessage
		if err := json.Unmarshal(data, &frames); err != nil {
			t.Fatalf("%s frames: %v", worker, err)
		}
		names := make([]string, 0, len(frames))
		for name := range frames {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			var frame map[string]any
			if err := json.Unmarshal(frames[name], &frame); err != nil {
				t.Fatalf("%s %s: %v", worker, name, err)
			}
			frameType, _ := frame["type"].(string)
			ref, ok := refs[frameType].(string)
			if !ok {
				t.Errorf("%s %s: frame type %q is not in the catalogue", worker, name, frameType)
				continue
			}
			schema := doc.Components.Schemas[strings.TrimPrefix(ref, "#/components/schemas/")]
			if err := schema.Value.VisitJSON(frame, openapi3.MultiErrors()); err != nil {
				t.Errorf("%s %s (%s) does not satisfy %s:\n%v", worker, name, frameType, ref, err)
			}
			covered[worker][frameType] = true
		}
	}
	// The floor: each worker's file must exercise the frames it can emit, so
	// a file emptied by a broken capture does not pass vacuously.
	for worker, want := range map[string][]string{
		"python": {"agent_tool_start", "agent_tool_end", "agent_tool_error", "agent_tool_paused", "agent_tool_output_chunk", "agent_hitl_interrupt", "mcp_authorization_required"},
		"rust":   {"agent_tool_start", "agent_tool_end", "agent_hitl_interrupt", "mcp_authorization_required", "agent_requires_confirmation"},
	} {
		for _, frameType := range want {
			if !covered[worker][frameType] {
				t.Errorf("testdata/client-frames/%s.json has no %s frame", worker, frameType)
			}
		}
	}
}
