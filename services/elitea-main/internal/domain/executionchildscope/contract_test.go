package executionchildscope

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func ownerWireFixture(t *testing.T) (Wire, []byte) {
	t.Helper()
	definition := []byte(`{"agent_type":"pipeline","instructions":"nodes: [{id: send, type: http}]","body_number":184467440737095516170000000000000001}`)
	full := FrozenDefinitionDigest(7, 31, 41, definition)
	source, _ := NewSourceWire(7, 9, 31, 41, definition)
	sourceRef := source.Reference()
	wire := Wire{SchemaVersion: Schema, Revision: 1, ExecutionID: "0123456789abcdef0123456789abcdef", Generation: 3, TenantID: "tenant-a", ResourceProjectID: 7, ProjectionProjectID: 8, ActorID: 9, RootInputSHA256: Digest([]byte("original input")),
		Family:            OriginalFamily{Kind: "agent_native", ParentThreadID: "original/root", ChildThreadID: "original/root/static-v1:family", NodeID: "elitea_agent_31_v_41", OriginalInvocationID: "original-invocation", OriginalCallID: "call-a", OriginalBatchSHA256: Digest([]byte("batch-a")), OriginalLineageWireB64: "YQ==", Ordinal: 1, InputSHA256: Digest([]byte("arguments")), CatalogSHA256: Digest([]byte("catalog")), AdmittedThreads: []string{"original/root/static-v1:family"}},
		Selected:          SavedSelector{ApplicationID: 31, VersionID: 41, FrozenDefinitionSHA256: full},
		Members:           []Member{{ThreadID: "original/root/static-v1:family", ApplicationID: 31, VersionID: 41, FrozenDefinitionSHA256: full, YAMLSHA256: Digest([]byte("nodes: [{id: send, type: http}]")), AllowedNodeIDs: []string{"send"}, SourceReference: &sourceRef}},
		RequestedPurposes: []Purpose{CodeDebug, NestedHTTP}, Purposes: []Purpose{NestedHTTP}}
	wire.Canonicalize()
	return wire, definition
}

func TestOriginalOccurrenceSlotAndImmutableIntentAreSeparate(t *testing.T) {
	wire, _ := ownerWireFixture(t)
	for _, mutate := range []func(*Wire){
		func(w *Wire) { w.Family.InputSHA256 = Digest([]byte("changed input")) },
		func(w *Wire) { w.Family.CatalogSHA256 = Digest([]byte("changed catalog")) },
		func(w *Wire) { w.Selected.VersionID++ },
		func(w *Wire) { w.Selected.FrozenDefinitionSHA256 = Digest([]byte("changed child")) },
		func(w *Wire) { w.Family.ChildThreadID += "/sibling" },
	} {
		changed := wire
		mutate(&changed)
		if changed.OccurrenceID() != wire.OccurrenceID() || bytes.Equal(changed.CanonicalBytes(), wire.CanonicalBytes()) {
			t.Fatal("altered intent did not stay in the conflicting immutable original slot")
		}
	}
	for _, mutate := range []func(*Wire){
		func(w *Wire) { w.Family.OriginalCallID = "call-b" },
		func(w *Wire) { w.Family.Ordinal++ },
		func(w *Wire) { w.Generation++ },
		func(w *Wire) { w.Family.ParentThreadID = "sibling/root" },
	} {
		changed := wire
		mutate(&changed)
		if changed.OccurrenceID() == wire.OccurrenceID() {
			t.Fatal("different original occurrence collided")
		}
	}
}

func TestCanonicalScopeWireRejectsTamperingAndExtraAuthority(t *testing.T) {
	wire, _ := ownerWireFixture(t)
	if _, err := DecodeWire(wire.CanonicalBytes()); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*Wire){
		"wrong original execution":  func(w *Wire) { w.ExecutionID = "foreign" },
		"caller revision":           func(w *Wire) { w.Revision = 2 },
		"foreign root input":        func(w *Wire) { w.RootInputSHA256 = "invalid" },
		"sibling member":            func(w *Wire) { w.Members[0].ThreadID += "/sibling" },
		"changed version":           func(w *Wire) { w.Members[0].VersionID++ },
		"scope replay":              func(w *Wire) { w.ScopeID = strings.Repeat("f", 64) },
		"undeclared purpose":        func(w *Wire) { w.Purposes = []Purpose{PlatformBroker} },
		"duplicate purpose":         func(w *Wire) { w.RequestedPurposes = []Purpose{NestedHTTP, NestedHTTP} },
		"path not compiler member":  func(w *Wire) { w.Members[0].MemberPath = "a//b" },
		"duplicate node membership": func(w *Wire) { w.Members[0].AllowedNodeIDs = []string{"send", "send"} },
		"non graph node":            func(w *Wire) { w.Members[0].AllowedNodeIDs = []string{"send/sibling"} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var changed Wire
			if json.Unmarshal(wire.CanonicalBytes(), &changed) != nil {
				t.Fatal("fixture")
			}
			mutate(&changed)
			if _, err := DecodeWire(changed.CanonicalBytes()); err == nil {
				t.Fatal("tampered scope accepted")
			}
		})
	}
	for _, raw := range [][]byte{append(wire.CanonicalBytes(), '\n'), append(wire.CanonicalBytes(), []byte(`{}`)...), bytes.Replace(wire.CanonicalBytes(), []byte(`"revision":1`), []byte(`"revision":1,"revision":1`), 1)} {
		if _, err := DecodeWire(raw); err == nil {
			t.Fatal("noncanonical or duplicate wire accepted")
		}
	}
}

func TestOwningDefinitionPreservesExactBytesAndSeparatesSameNodeNames(t *testing.T) {
	wire, definition := ownerWireFixture(t)
	raw := wire.CanonicalBytes()
	owner, err := wire.OwningDefinition(raw, definition, wire.Family.ChildThreadID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(owner.PreRedemptionVersion, definition) || !owner.AllowsNode("send") || owner.AllowsNode("sibling/send") {
		t.Fatal("exact owning declaration was not retained")
	}
	if _, err := wire.OwningDefinition(raw, definition, wire.Family.ChildThreadID+"/sibling", nil); err == nil {
		t.Fatal("same named sibling acquired owner")
	}
	for _, replacement := range []string{"1", "1.0", "184467440737095516170000000000000002"} {
		changed := bytes.Replace(definition, []byte("184467440737095516170000000000000001"), []byte(replacement), 1)
		if _, err := wire.OwningDefinition(raw, changed, wire.Family.ChildThreadID, nil); err == nil {
			t.Fatal("changed exact numeric token acquired original owner")
		}
	}
	definition[0] = ' '
	raw[0] = ' '
	if owner.PreRedemptionVersion[0] != '{' || owner.CanonicalWire[0] != '{' {
		t.Fatal("caller bytes alias immutable owner")
	}
}

func TestDefinitionFrameNeverEquatesNumbersOrKeyOrder(t *testing.T) {
	seen := map[string]bool{}
	for _, raw := range []string{`{"n":1,"a":2}`, `{"n":1.0,"a":2}`, `{"a":2,"n":1}`, `{"n":184467440737095516170000000000000001}`} {
		digest := FrozenDefinitionDigest(7, 31, 41, []byte(raw))
		if seen[digest] {
			t.Fatal("exact definition images equated")
		}
		seen[digest] = true
	}
	if FrozenDefinitionDigest(7, 31, 41, []byte(`{"n":1}`)) == FrozenDefinitionDigest(8, 31, 41, []byte(`{"n":1}`)) {
		t.Fatal("resource project frame dropped")
	}
}

func TestRequestedPurposesNeverConferMissingAdmission(t *testing.T) {
	wire, _ := ownerWireFixture(t)
	if wire.Allows(CodeDebug) || !wire.Allows(NestedHTTP) {
		t.Fatal("requested purpose became grant")
	}
	wire.Purposes = nil
	if _, err := DecodeWire(wire.CanonicalBytes()); err != nil {
		t.Fatal(err)
	}
	for _, purpose := range []Purpose{NestedHTTP, CodeRecovery, CodeDebug, CodeWorkspace, PlatformBroker} {
		if wire.Allows(purpose) {
			t.Fatal("empty operator admission granted consumer")
		}
	}
}
