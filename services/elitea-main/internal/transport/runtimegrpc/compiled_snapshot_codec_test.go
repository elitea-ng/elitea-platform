package runtimegrpc

import (
	"bytes"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"
)

func TestCompiledSnapshotIsolatedFrameBound(t *testing.T) {
	codec, _ := NewDirectionalStrictProtoCodec(64*1024, 64*1024)
	request := &runtimev1.AuthorizeRustCompiledSnapshotRequestV1{PreparedJobJson: bytes.Repeat([]byte("x"), 80*1024)}
	raw, _ := proto.Marshal(request)
	if codec.Unmarshal(raw, new(runtimev1.AuthorizeRustCompiledSnapshotRequestV1)) == nil {
		t.Fatal("disabled larger frame accepted")
	}
	if err := codec.WithCompiledSnapshotRequestLimit(1024*1024 + 80*1024); err != nil {
		t.Fatal(err)
	}
	if err := codec.Unmarshal(raw, new(runtimev1.AuthorizeRustCompiledSnapshotRequestV1)); err != nil {
		t.Fatal(err)
	}
	ordinary, _ := proto.Marshal(&runtimev1.AuthorizeSandboxJobRequestV1{ActivationId: string(bytes.Repeat([]byte("x"), 80*1024))})
	if codec.Unmarshal(ordinary, new(runtimev1.AuthorizeSandboxJobRequestV1)) == nil {
		t.Fatal("ordinary control bound expanded")
	}
	request.PreparedJobJson = bytes.Repeat([]byte("x"), 1024*1024+80*1024)
	raw, _ = proto.Marshal(request)
	if codec.Unmarshal(raw, new(runtimev1.AuthorizeRustCompiledSnapshotRequestV1)) == nil {
		t.Fatal("oversized isolated request accepted")
	}
	if codec.WithCompiledSnapshotRequestLimit(2*1024*1024) == nil {
		t.Fatal("unbounded limit accepted")
	}
}
func TestCompiledSnapshotOldGrantDecoderRejectsNewRole(t *testing.T) {
	claim := &runtimev1.RustCompiledSnapshotGrantClaimsV1{Revision: 4, Purpose: runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH}
	raw, _ := proto.Marshal(claim)
	if ScanStrictMessage(raw, new(runtimev1.SandboxJobGrantClaimsV1).ProtoReflect().Descriptor()) == nil {
		t.Fatal("old grants accepted compiled authority")
	}
}
