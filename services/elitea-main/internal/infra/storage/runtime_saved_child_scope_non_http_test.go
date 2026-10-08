package storage

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestFreezeChildWithoutHTTPNodesPreservesSavedBytes(t *testing.T) {
	for name, instructions := range map[string]string{
		"100KiB":      "entry_point: tick\nnodes:\n  - id: tick\n    type: state_modifier\n    template: '" + strings.Repeat("x", 100*1024) + "'\n    transition: END\n",
		"numeric id":  "entry_point: 1\nnodes:\n  - id: 1\n    type: state_modifier\n    template: '{{ count }}'\n    transition: END\n",
		"anchored id": "entry_point: tick\nnodes:\n  - id: &tick tick\n    type: state_modifier\n    template: '{{ count }}'\n    transition: END\ninterrupt_before: [*tick]\n",
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"agent_type": "pipeline", "instructions": instructions})
			frozen, err := FreezeHTTPChildVersion(31, 41, raw)
			if err != nil {
				t.Fatalf("saved non-HTTP child pipeline denied: %v", err)
			}
			if !bytes.Equal(frozen, raw) {
				t.Fatal("saved non-HTTP child bytes changed")
			}
		})
	}
}
