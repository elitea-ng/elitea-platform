package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type compiledRPCIndex struct {
	candidate                           domain.SnapshotCandidate
	reserved, ready, completed, cleaned bool
	calls                               int
}

func (*compiledRPCIndex) OriginalExecution(context.Context, domain.SnapshotScope, string, string, string) (domain.SnapshotExecution, bool, error) {
	return domain.SnapshotExecution{}, false, nil
}
func (i *compiledRPCIndex) Candidate(_ context.Context, s domain.SnapshotScope, key, job string, complete bool) (domain.SnapshotCandidate, error) {
	i.calls++
	if s != i.candidate.Scope || key != i.candidate.Key || job != i.candidate.CompilationJobKey || complete && (!i.completed || !i.cleaned) {
		return domain.SnapshotCandidate{}, domain.ErrSnapshotUnavailable
	}
	c := i.candidate
	if i.completed && i.cleaned {
		c.ReceiptSHA256 = domain.SnapshotContentSHA256([]byte("immutable receipt"))
	}
	return c, nil
}
func (i *compiledRPCIndex) Ready(_ context.Context, s domain.SnapshotScope, key string) (domain.SnapshotCandidate, error) {
	i.calls++
	if !i.ready || s != i.candidate.Scope || key != i.candidate.Key {
		return domain.SnapshotCandidate{}, domain.ErrSnapshotUnavailable
	}
	return i.candidate, nil
}
func (i *compiledRPCIndex) Reserve(_ context.Context, c domain.SnapshotCandidate) error {
	if !c.SameArtifact(i.candidate) {
		return domain.ErrSnapshotConflict
	}
	i.reserved = true
	return nil
}
func (i *compiledRPCIndex) WithPublishing(_ context.Context, c domain.SnapshotCandidate, fn func() error) error {
	if !i.reserved || i.ready || !c.SameArtifact(i.candidate) {
		return domain.ErrSnapshotUnavailable
	}
	return fn()
}
func (i *compiledRPCIndex) WithReady(ctx context.Context, s domain.SnapshotScope, key, root string, fn func(domain.SnapshotCandidate) error) error {
	c, err := i.Ready(ctx, s, key)
	if err != nil {
		return err
	}
	if c.Root != root {
		return domain.ErrSnapshotConflict
	}
	return fn(c)
}
func (i *compiledRPCIndex) CommitReady(_ context.Context, c domain.SnapshotCandidate, fn func() error) error {
	if !i.reserved || !i.completed || !i.cleaned || !c.SameArtifact(i.candidate) || !domain.SnapshotDigest(c.ReceiptSHA256) {
		return domain.ErrSnapshotUnavailable
	}
	if err := fn(); err != nil {
		return err
	}
	i.ready = true
	i.candidate = c
	return nil
}

type compiledRPCFixture struct {
	store   *SandboxBundleStore
	backend *sandboxObjectFake
	index   *compiledRPCIndex
	service *CompiledSnapshotContentService
	key     ed25519.PrivateKey
	ctx     context.Context
	body    []byte
	now     time.Time
}

func compiledNewRPCFixture(t *testing.T) *compiledRPCFixture {
	t.Helper()
	store, backend, _, _, _ := sandboxTestStore(t)
	raw, err := os.ReadFile("testdata/compiled-snapshot-v1/binding.json")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := domain.ParseRustSnapshotBinding(raw)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := binding.Key()
	body := []byte("component executable bytes")
	descriptor, _ := domain.SnapshotJSON(domain.RustSnapshotDescriptor{Revision: 1, Binding: binding, SnapshotKeySHA256: key, ExecutableSHA256: domain.SnapshotContentSHA256(body), ExecutableBytes: int64(len(body))})
	digest, _ := domain.SnapshotJobDigest("compile", binding, "")
	candidate := domain.SnapshotCandidate{Scope: domain.SnapshotScope{TenantID: binding.TenantID, ProjectID: binding.ProjectID}, Key: key, Root: domain.SnapshotContentSHA256(descriptor), DescriptorJSON: descriptor, CompilationJobKey: domain.SnapshotActivationKey("execution-1", "compile-1"), CompilationRequestDigest: digest, CompilationRuntimeID: "original-runtime", CompilationLeaseEpoch: 3, CurrentLeaseLive: true}
	index := &compiledRPCIndex{candidate: candidate}
	signer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{11}, 32))
	now := time.Now().UTC()
	service, err := NewCompiledSnapshotContentService(store, index, sandboxBundleTestKeys(signer.Public().(ed25519.PublicKey)), []string{"dns:sandbox.test"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	_, leaf := sandboxTestCertificate(t, "sandbox.test")
	return &compiledRPCFixture{store, backend, index, service, signer, contentPeerContext(context.Background(), leaf), body, now}
}
func (f *compiledRPCFixture) claims(p runtimev1.RustCompiledSnapshotPurposeV1) *runtimev1.RustCompiledSnapshotGrantClaimsV1 {
	candidate := f.index.candidate
	d, _ := candidate.Descriptor()
	digest, _ := domain.SnapshotJobDigest("execute", d.Binding, candidate.Root)
	c := &runtimev1.RustCompiledSnapshotGrantClaimsV1{Revision: 4, TenantId: candidate.Scope.TenantID, ProjectId: candidate.Scope.ProjectID, ExecutionId: "execution-1", ActivationId: "compile-1", RequestDigest: decodeCompiledHex(digest), SubmitterWorkloadIdentity: "dns:worker.test", Audience: "dns:sandbox.test", IssuedAtUnixMillis: f.now.UnixMilli(), ExpiresAtUnixMillis: f.now.Add(30 * time.Second).UnixMilli(), Generation: 1, Purpose: p, BasePreparedRequestSha256: decodeCompiledHex(d.Binding.BasePreparedRequestSHA256), SnapshotKeySha256: decodeCompiledHex(candidate.Key), DescriptorSha256: decodeCompiledHex(candidate.Root)}
	if p == runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH {
		c.RequestDigest = decodeCompiledHex(candidate.CompilationRequestDigest)
		c.CompilationJobKey = decodeCompiledHex(candidate.CompilationJobKey)
		c.CompilationRuntimeId = candidate.CompilationRuntimeID
		c.CompilationRequestDigest = decodeCompiledHex(candidate.CompilationRequestDigest)
		c.CompilationLeaseEpoch = candidate.CompilationLeaseEpoch
	}
	return c
}
func decodeCompiledHex(v string) []byte { raw, _ := hex.DecodeString(v); return raw }
func (f *compiledRPCFixture) grant(c *runtimev1.RustCompiledSnapshotGrantClaimsV1) []byte {
	exact, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	input := append([]byte(sandboxBundleGrantDomain), make([]byte, 8)...)
	binary.BigEndian.PutUint64(input[len(sandboxBundleGrantDomain):], uint64(len(exact)))
	input = append(input, exact...)
	grant, _ := proto.Marshal(&runtimev1.SignedSandboxJobGrantV1{KeyId: "key-1", ClaimsBytes: exact, Signature: ed25519.Sign(f.key, input)})
	return grant
}

// call runs one RPC: "get", "publish", "fetch" or "upload".
func (f *compiledRPCFixture) call(t *testing.T, op, name string, body []byte, c *runtimev1.RustCompiledSnapshotGrantClaimsV1) (codes.Code, []byte) {
	t.Helper()
	content := (&ContentServer{}).WithCompiledSnapshots(f.service)
	root, grant := f.index.candidate.Root, f.grant(c)
	var data []byte
	var err error
	switch op {
	case "get":
		var response *runtimev1.GetCompiledSnapshotResponseV1
		response, err = content.GetCompiledSnapshot(f.ctx, &runtimev1.GetCompiledSnapshotRequestV1{Root: root, SignedGrant: grant})
		data = response.GetDescriptorJson()
	case "publish":
		_, err = content.PublishCompiledSnapshot(f.ctx, &runtimev1.PublishCompiledSnapshotRequestV1{Root: root, SignedGrant: grant, DescriptorJson: body})
	case "fetch":
		stream := newRecordingServerStream[runtimev1.FetchCompiledSnapshotFileResponseV1](f.ctx)
		err = content.FetchCompiledSnapshotFile(&runtimev1.FetchCompiledSnapshotFileRequestV1{Root: root, Name: name, SignedGrant: grant}, stream)
		for i, message := range stream.sent {
			if (i == 0) != (message.GetHeader() != nil) {
				t.Fatal("header is not exactly the first message")
			}
			data = append(data, message.GetChunk()...)
		}
	case "upload":
		messages := []*runtimev1.UploadCompiledSnapshotFileRequestV1{{Part: &runtimev1.UploadCompiledSnapshotFileRequestV1_Header{Header: &runtimev1.UploadCompiledSnapshotFileHeaderV1{Root: root, Name: name, SignedGrant: grant, DescriptorJson: f.index.candidate.DescriptorJSON}}}}
		for _, chunk := range splitContentChunks(body) {
			messages = append(messages, &runtimev1.UploadCompiledSnapshotFileRequestV1{Part: &runtimev1.UploadCompiledSnapshotFileRequestV1_Chunk{Chunk: chunk}})
		}
		err = content.UploadCompiledSnapshotFile(newScriptedClientStream[runtimev1.UploadCompiledSnapshotFileRequestV1, runtimev1.UploadCompiledSnapshotFileResponseV1](f.ctx, messages...))
	default:
		t.Fatal(op)
	}
	return grpcstatus.Code(err), data
}

func TestCompiledSnapshotRPCStagingIsInertUntilSuccessfulReceiptAndCleanup(t *testing.T) {
	f := compiledNewRPCFixture(t)
	publish := f.claims(runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH)
	read := f.claims(runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_READ)
	if code, _ := f.call(t, "upload", SnapshotExecutableName, f.body, publish); code != codes.OK || !f.index.reserved || f.index.ready {
		t.Fatal(code)
	}
	if code, _ := f.call(t, "get", "", nil, read); code != codes.Aborted {
		t.Fatal("staged metadata readable", code)
	}
	if code, _ := f.call(t, "fetch", SnapshotExecutableName, nil, read); code != codes.Aborted {
		t.Fatal("staged executable readable", code)
	}
	for _, cleanup := range []bool{false, true} {
		f.index.cleaned = cleanup
		if code, _ := f.call(t, "publish", "", f.index.candidate.DescriptorJSON, publish); code != codes.Aborted {
			t.Fatal("missing successful terminal receipt accepted")
		}
	}
	f.index.completed = true
	f.index.cleaned = false
	if code, _ := f.call(t, "publish", "", f.index.candidate.DescriptorJSON, publish); code != codes.Aborted {
		t.Fatal("missing cleanup accepted")
	}
	f.index.cleaned = true
	if code, _ := f.call(t, "publish", "", f.index.candidate.DescriptorJSON, publish); code != codes.OK || !f.index.ready {
		t.Fatal(code)
	}
	if code, descriptor := f.call(t, "get", "", nil, read); code != codes.OK || !bytes.Equal(descriptor, f.index.candidate.DescriptorJSON) {
		t.Fatal(code)
	}
	if code, data := f.call(t, "fetch", SnapshotExecutableName, nil, read); code != codes.OK || !bytes.Equal(data, f.body) {
		t.Fatal(code, string(data))
	}
}
func TestCompiledSnapshotRPCCorruptContentCannotBecomeReady(t *testing.T) {
	f := compiledNewRPCFixture(t)
	publish := f.claims(runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH)
	if code, _ := f.call(t, "upload", SnapshotExecutableName, f.body, publish); code != codes.OK {
		t.Fatal(code)
	}
	for ref, raw := range f.backend.objects {
		if bytes.Equal(raw, f.body) {
			f.backend.objects[ref] = []byte("corrupt")
		}
	}
	f.index.completed = true
	f.index.cleaned = true
	if code, _ := f.call(t, "publish", "", f.index.candidate.DescriptorJSON, publish); code != codes.FailedPrecondition || f.index.ready {
		t.Fatal("corrupt publication", code)
	}
	scope, _ := NewSandboxBundleScope(f.index.candidate.Scope.TenantID, f.index.candidate.Scope.ProjectID)
	if _, err := f.store.OpenCompiledSnapshot(context.Background(), scope, f.index.candidate.Root); err == nil {
		t.Fatal("metadata published before verified content")
	}
}
func TestCompiledSnapshotRPCExactRoleAndFreshAuthority(t *testing.T) {
	for _, name := range []string{"compile", "execute", "publish-read", "read-publish", "expired", "audience", "lease", "root", "plaintext"} {
		t.Run(name, func(t *testing.T) {
			f := compiledNewRPCFixture(t)
			claims := f.claims(runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_READ)
			op := "get"
			switch name {
			case "compile":
				claims.Purpose = runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_COMPILE
			case "execute":
				claims.Purpose = runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_EXECUTE
			case "publish-read":
				claims = f.claims(runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH)
			case "read-publish":
				op = "publish"
			case "expired":
				claims.ExpiresAtUnixMillis = 1000000
			case "audience":
				claims.Audience = "dns:other.test"
			case "lease":
				claims = f.claims(runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH)
				claims.CompilationLeaseEpoch = 0
				op = "publish"
			case "root":
				claims.DescriptorSha256 = bytes.Repeat([]byte{1}, 32)
			case "plaintext":
				f.ctx = context.Background()
			}
			if code, _ := f.call(t, op, "", f.index.candidate.DescriptorJSON, claims); code != codes.PermissionDenied || f.index.calls != 0 || len(f.backend.puts) != 0 {
				t.Fatal("wrong authority reached storage", code)
			}
		})
	}
}
func TestCompiledSnapshotRPCRejectsCorruptUploadBeforeBackendWrite(t *testing.T) {
	f := compiledNewRPCFixture(t)
	claims := f.claims(runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH)
	if code, _ := f.call(t, "upload", SnapshotExecutableName, []byte("substitution"), claims); code != codes.FailedPrecondition || len(f.backend.puts) != 0 || f.index.ready {
		t.Fatal(code)
	}
	if code, _ := f.call(t, "upload", "../anything", f.body, claims); code == codes.OK {
		t.Fatal("arbitrary path accepted")
	}
	// Trailing bytes after the executable refuse the upload.
	if code, _ := f.call(t, "upload", SnapshotExecutableName, append(append([]byte(nil), f.body...), 'x'), claims); code != codes.FailedPrecondition {
		t.Fatal("trailing data accepted", code)
	}
}
func TestCompiledSnapshotObjectStoreFixedPathsAndBounds(t *testing.T) {
	f := compiledNewRPCFixture(t)
	bundle, err := ParseRustCompiledSnapshot(f.index.candidate.DescriptorJSON, f.index.candidate.Root)
	if err != nil {
		t.Fatal(err)
	}
	scope, _ := NewSandboxBundleScope(f.index.candidate.Scope.TenantID, f.index.candidate.Scope.ProjectID)
	if err = f.store.PutFile(context.Background(), scope, bundle, "../x", bytes.NewReader(f.body)); err == nil {
		t.Fatal("arbitrary name")
	}
	if err = f.store.Publish(context.Background(), scope, bundle); err == nil {
		t.Fatal("missing executable")
	}
	if err = f.store.PutFile(context.Background(), scope, bundle, SnapshotExecutableName, bytes.NewReader(f.body)); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Publish(context.Background(), scope, bundle); err != nil {
		t.Fatal(err)
	}
	opened, err := f.store.OpenCompiledSnapshot(context.Background(), scope, bundle.Digest())
	if err != nil || !bytes.Equal(opened.native, bundle.native) {
		t.Fatal(err)
	}
	if err = f.store.FetchFile(context.Background(), scope, bundle, SnapshotExecutableName, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestCompiledSnapshotRPCStagingRequiresReclaimedLivePublisherLease(t *testing.T) {
	f := compiledNewRPCFixture(t)
	f.index.candidate.CurrentLeaseLive = false
	publish := f.claims(runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH)
	if code, _ := f.call(t, "upload", SnapshotExecutableName, f.body, publish); code != codes.Aborted || len(f.backend.puts) != 0 || f.index.reserved {
		t.Fatal("released lease authorized staging", code)
	}
	f.index.candidate.CurrentLeaseLive = true
	if code, _ := f.call(t, "upload", SnapshotExecutableName, f.body, publish); code != codes.OK {
		t.Fatal("fresh publisher lease rejected", code)
	}
}
