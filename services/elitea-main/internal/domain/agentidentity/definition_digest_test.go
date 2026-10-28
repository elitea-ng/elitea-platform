package agentidentity

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"

	scope "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
)

// The canonical root digest is the nested-child framing, spelled out here
// independently, and equals a captured root source's
// source_definition_sha256 for the same bytes.
func TestVersionDefinitionDigestIsTheFrozenDefinitionFraming(t *testing.T) {
	definition := []byte(`{"id":41,"application_id":31,"instructions":"Authored source","tools":[]}`)
	got, err := VersionDefinitionDigest(7, 31, 41, definition)
	if err != nil {
		t.Fatal(err)
	}
	var frame [32]byte
	binary.BigEndian.PutUint64(frame[0:8], 7)
	binary.BigEndian.PutUint64(frame[8:16], 31)
	binary.BigEndian.PutUint64(frame[16:24], 41)
	binary.BigEndian.PutUint64(frame[24:32], uint64(len(definition)))
	hash := sha256.New()
	hash.Write([]byte("elitea.runtime.application-definition.v1\x00"))
	hash.Write(frame[:])
	hash.Write(definition)
	if want := hash.Sum(nil); hex.EncodeToString(got[:]) != hex.EncodeToString(want) {
		t.Fatalf("digest %x, want %x", got, want)
	}
	source, err := scope.NewSourceWire(7, 5, 31, 41, definition)
	if err != nil {
		t.Fatal(err)
	}
	if source.SourceDefinitionSHA256 != hex.EncodeToString(got[:]) {
		t.Fatalf("source_definition_sha256 %s, digest %x", source.SourceDefinitionSHA256, got)
	}
}

// Every input is bound: another project, version or byte is another digest.
func TestVersionDefinitionDigestBindsEveryInput(t *testing.T) {
	definition := []byte(`{"instructions":"a"}`)
	base, _ := VersionDefinitionDigest(7, 31, 41, definition)
	for name, other := range map[string]func() ([32]byte, error){
		"project":    func() ([32]byte, error) { return VersionDefinitionDigest(8, 31, 41, definition) },
		"app":        func() ([32]byte, error) { return VersionDefinitionDigest(7, 32, 41, definition) },
		"version":    func() ([32]byte, error) { return VersionDefinitionDigest(7, 31, 42, definition) },
		"definition": func() ([32]byte, error) { return VersionDefinitionDigest(7, 31, 41, []byte(`{"instructions":"b"}`)) },
	} {
		got, err := other()
		if err != nil || got == base {
			t.Fatalf("%s: %x, %v", name, got, err)
		}
	}
	for _, bad := range [][]byte{nil, {}} {
		if _, err := VersionDefinitionDigest(7, 31, 41, bad); err == nil {
			t.Fatal("an empty definition was digested")
		}
	}
	if _, err := VersionDefinitionDigest(0, 31, 41, definition); err == nil {
		t.Fatal("project 0 was digested")
	}
}
