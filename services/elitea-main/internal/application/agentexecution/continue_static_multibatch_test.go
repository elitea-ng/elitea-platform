package agentexecution

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStaticInventoryMultipleOriginalBatches(t *testing.T) {
	first := CurrentStaticToolPause{ToolCallID: "call", ChildThreadID: "child-1", OriginalBatchEventID: "batch-1", OriginalOrdinal: 16, Proof: staticProofFixture()}
	second := first
	second.ChildThreadID = "child-2"
	second.OriginalBatchEventID = "batch-2"
	second.Proof.PauseID = "pipeline-static:sha256:" + strings.Repeat("d", 64)
	inventory := CurrentStaticToolInventory{Revision: 1, Pauses: []CurrentStaticToolPause{first, second}}
	parse := func(in CurrentStaticToolInventory) error {
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ParseCurrentStaticToolInventory(raw)
		return err
	}
	if err := parse(inventory); err != nil {
		t.Fatal(err)
	}
	selected := CurrentStaticLeafDecision{PauseID: second.Proof.PauseID, ChildThreadID: second.ChildThreadID, ToolCallID: second.ToolCallID, Action: "continue", Value: "next"}
	if !staticLeafDecisionsMatch([]CurrentStaticLeafDecision{selected}, inventory) || len(inventory.Pauses) != 2 {
		t.Fatal("selected subset changed original inventory")
	}
	for _, tc := range []struct {
		name   string
		change func(*CurrentStaticToolPause)
	}{
		{"same batch ordinal", func(p *CurrentStaticToolPause) { p.OriginalBatchEventID = first.OriginalBatchEventID }},
		{"same child call", func(p *CurrentStaticToolPause) { p.ChildThreadID = first.ChildThreadID }},
		{"same pause", func(p *CurrentStaticToolPause) { p.Proof.PauseID = first.Proof.PauseID }},
		{"zero ordinal", func(p *CurrentStaticToolPause) { p.OriginalOrdinal = 0 }},
		{"high ordinal", func(p *CurrentStaticToolPause) { p.OriginalOrdinal = 17 }},
		{"empty batch", func(p *CurrentStaticToolPause) { p.OriginalBatchEventID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := inventory
			bad.Pauses = append([]CurrentStaticToolPause{}, inventory.Pauses...)
			tc.change(&bad.Pauses[1])
			if parse(bad) == nil {
				t.Fatal("invalid occurrence accepted")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*CurrentStaticLeafDecision)
	}{
		{"cross child", func(d *CurrentStaticLeafDecision) { d.ChildThreadID = first.ChildThreadID }},
		{"stale pause", func(d *CurrentStaticLeafDecision) { d.PauseID = "pipeline-static:sha256:" + strings.Repeat("e", 64) }},
		{"wrong call", func(d *CurrentStaticLeafDecision) { d.ToolCallID = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := selected
			tc.change(&bad)
			if staticLeafDecisionsMatch([]CurrentStaticLeafDecision{bad}, inventory) {
				t.Fatal("cross-scope selector accepted")
			}
		})
	}
}
