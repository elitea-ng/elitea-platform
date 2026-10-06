package runtimegrpc

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Requires root-owned generated postimages from the authoritative compiled_code
// source. These tests are authored but have not run against old generated files.
func TestCodeWorkspaceOptionalVisitKeepsLegacyRequestAndGrantBytes(t *testing.T) {
	request := &runtimev1.AuthorizeRustCompiledSnapshotRequestV1{ActivationId: "compile", Purpose: runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_COMPILE}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil || hex.EncodeToString(raw) != "2207636f6d70696c653001" {
		t.Fatal("absent selector changed legacy request", hex.EncodeToString(raw), err)
	}
	claims := &runtimev1.RustCompiledSnapshotGrantClaimsV1{Revision: 4, Purpose: runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_COMPILE}
	raw, err = proto.MarshalOptions{Deterministic: true}.Marshal(claims)
	if err != nil || hex.EncodeToString(raw) != "0804900201" {
		t.Fatal("absent access changed legacy signed claims", hex.EncodeToString(raw), err)
	}
}
func TestCodeWorkspaceVisitRequestAndSignedAccessUseOwningTypedWire(t *testing.T) {
	ref := &runtimev1.OriginalCodeVisitRefV1{VisitId: strings.Repeat("1", 64), Revision: 1, DigestSha256: strings.Repeat("2", 64)}
	request := &runtimev1.AuthorizeRustCompiledSnapshotRequestV1{Purpose: runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_COMPILE, OriginalCodeVisit: ref}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	codec, _ := NewDirectionalStrictProtoCodec(64*1024, 64*1024)
	decoded := new(runtimev1.AuthorizeRustCompiledSnapshotRequestV1)
	if err = codec.Unmarshal(wire, decoded); err != nil || !proto.Equal(decoded.OriginalCodeVisit, ref) {
		t.Fatal("typed field11 lost", err)
	}
	access := &runtimev1.OriginalCodeVisitAccessV1{OriginalVisit: ref, ClaimId: "23456789abcdef0123456789abcdef01", ClaimAttempt: 2, LeaseEpoch: 3, FenceSha256: bytes.Repeat([]byte{7}, 32)}
	claims := &runtimev1.RustCompiledSnapshotGrantClaimsV1{Revision: 4, Purpose: runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_COMPILE, OriginalCodeVisitAccess: access}
	wire, err = proto.MarshalOptions{Deterministic: true}.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	result := new(runtimev1.RustCompiledSnapshotGrantClaimsV1)
	if ScanStrictMessage(wire, result.ProtoReflect().Descriptor()) != nil || proto.Unmarshal(wire, result) != nil || !proto.Equal(result.OriginalCodeVisitAccess, access) {
		t.Fatal("typed signed field42 lost")
	}
	if ScanStrictMessage(wire, new(runtimev1.SandboxJobGrantClaimsV1).ProtoReflect().Descriptor()) == nil {
		t.Fatal("legacy job grant accepted Compile visit access")
	}
}
func TestCodeWorkspaceWireRejectsDuplicateAndNestedReservedSelectors(t *testing.T) {
	ref := &runtimev1.OriginalCodeVisitRefV1{VisitId: strings.Repeat("1", 64), Revision: 1, DigestSha256: strings.Repeat("2", 64)}
	refWire, _ := proto.Marshal(ref)
	single := protowire.AppendTag(nil, 11, protowire.BytesType)
	single = protowire.AppendBytes(single, refWire)
	duplicate := append(bytes.Clone(single), single...)
	unknownRef := protowire.AppendTag(bytes.Clone(refWire), 4, protowire.VarintType)
	unknownRef = protowire.AppendVarint(unknownRef, 1)
	nestedUnknown := protowire.AppendTag(nil, 11, protowire.BytesType)
	nestedUnknown = protowire.AppendBytes(nestedUnknown, unknownRef)
	reserved := protowire.AppendTag(bytes.Clone(single), 12, protowire.VarintType)
	reserved = protowire.AppendVarint(reserved, 1)
	for _, raw := range [][]byte{duplicate, nestedUnknown, reserved} {
		if ScanStrictMessage(raw, new(runtimev1.AuthorizeRustCompiledSnapshotRequestV1).ProtoReflect().Descriptor()) == nil {
			t.Fatal("duplicate/unknown selector admitted")
		}
	}
	accessWire, _ := proto.Marshal(&runtimev1.OriginalCodeVisitAccessV1{OriginalVisit: ref})
	accessWire = protowire.AppendTag(accessWire, 6, protowire.VarintType)
	accessWire = protowire.AppendVarint(accessWire, 1)
	claimsWire := protowire.AppendTag(nil, 42, protowire.BytesType)
	claimsWire = protowire.AppendBytes(claimsWire, accessWire)
	if ScanStrictMessage(claimsWire, new(runtimev1.RustCompiledSnapshotGrantClaimsV1).ProtoReflect().Descriptor()) == nil {
		t.Fatal("nested signed access unknown field admitted")
	}
}
