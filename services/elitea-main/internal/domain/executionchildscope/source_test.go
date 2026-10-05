package executionchildscope

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestOriginalSourceReferenceBindsExactMainBytesFrameAndActor(t *testing.T) {
	raw := []byte(`{"agent_type":"pipeline","instructions":"nodes:\n  fetch:\n    type: http\n","large":184467440737095516170000000000000001,"value":1.0,"meta":{"debug":false}}`)
	wire, err := NewSourceWire(7, 11, 31, 41, raw)
	if err != nil {
		t.Fatal(err)
	}
	ref := wire.Reference()
	if ref.Validate() != nil || ref.SourceDefinitionSHA256 != FrozenDefinitionDigest(7, 31, 41, raw) {
		t.Fatal("source did not reuse Main exact bytes/frame")
	}
	source, err := DecodeSourceWire(wire.CanonicalBytes(), raw, ref, 7, 11)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source.PreRedemptionVersion, raw) || source.Reference != ref || source.Instructions != "nodes:\n  fetch:\n    type: http\n" {
		t.Fatal("source bytes changed")
	}
	for _, spec := range []struct {
		name           string
		project, actor int64
		raw            []byte
		ref            SourceReference
	}{{"foreign actor", 7, 12, raw, ref}, {"foreign project", 8, 11, raw, ref}, {"numeric token", 7, 11, bytes.Replace(raw, []byte(`"value":1.0`), []byte(`"value":1`), 1), ref}, {"key order", 7, 11, []byte(`{"instructions":"nodes:\n  fetch:\n    type: http\n","agent_type":"pipeline","large":184467440737095516170000000000000001,"value":1.0,"meta":{"debug":false}}`), ref}, {"wrong source id", 7, 11, raw, func() SourceReference { r := ref; r.SourceID = Digest([]byte("other")); return r }()}} {
		t.Run(spec.name, func(t *testing.T) {
			if _, err := DecodeSourceWire(wire.CanonicalBytes(), spec.raw, spec.ref, spec.project, spec.actor); err == nil {
				t.Fatal("altered source accepted")
			}
		})
	}
	source.PreRedemptionVersion[0] = '!'
	source.CanonicalWire[0] = '!'
	if raw[0] != '{' || wire.CanonicalBytes()[0] != '{' {
		t.Fatal("caller mutation changed stored source")
	}
}
func TestOriginalSourceReferenceSavedAndEphemeralSelectorsStayDistinct(t *testing.T) {
	raw := []byte(`{"agent_type":"agent","instructions":"Unsaved declaration","tools":[]}`)
	saved, _ := NewSourceWire(7, 11, 31, 41, raw)
	ephemeral, err := NewSourceWire(7, 11, 0, 0, raw)
	if err != nil {
		t.Fatal(err)
	}
	if ephemeral.Kind != "ephemeral_definition" || ephemeral.Reference().Validate() != nil || ephemeral.SourceDefinitionSHA256 == saved.SourceDefinitionSHA256 || ephemeral.SourceID == saved.SourceID {
		t.Fatal("ephemeral and saved selectors collided")
	}
	for _, ids := range [][2]uint64{{0, 41}, {31, 0}} {
		if _, err := NewSourceWire(7, 11, ids[0], ids[1], raw); err == nil {
			t.Fatal("partial saved selector accepted")
		}
	}
	// Reference is deliberately distinct from the Rust compiler definition digest.
	if ephemeral.SourceDefinitionSHA256 == Digest([]byte("compiler-bound-definition")) {
		t.Fatal("unrelated compiler digest unexpectedly matched")
	}
}
func TestOriginalSourceWireStrictCanonicalBoundedAndNoEditableIdentity(t *testing.T) {
	raw := []byte(`{"instructions":"source","source_definition":{"source_id":"editable"},"meta":{"frozen_definition_sha256":"editable"}}`)
	wire, err := NewSourceWire(7, 11, 31, 41, raw)
	if err != nil {
		t.Fatal(err)
	}
	if wire.SourceDefinitionSHA256 == "editable" || wire.SourceID == "editable" {
		t.Fatal("editable metadata selected source")
	}
	for _, candidate := range [][]byte{append(wire.CanonicalBytes(), []byte(` {}`)...), append([]byte(" "), wire.CanonicalBytes()...), bytes.Replace(wire.CanonicalBytes(), []byte(`"revision":1`), []byte(`"revision":1,"unexpected":true`), 1), bytes.Replace(wire.CanonicalBytes(), []byte(`"revision":1`), []byte(`"revision":1,"revision":1`), 1)} {
		if _, err := DecodeSourceWire(candidate, raw, wire.Reference(), 7, 11); err == nil {
			t.Fatal("noncanonical source wire accepted")
		}
	}
	oversized, _ := json.Marshal(map[string]string{"instructions": string(bytes.Repeat([]byte{'x'}, MaxDefinitionBytes))})
	if _, err := NewSourceWire(7, 11, 31, 41, oversized); err == nil {
		t.Fatal("oversize source accepted")
	}
}
