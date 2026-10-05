package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

type compiledHTTPIndex struct {
	candidate                           domain.SnapshotCandidate
	reserved, ready, completed, cleaned bool
	calls                               int
}

func (*compiledHTTPIndex) OriginalExecution(context.Context, domain.SnapshotScope, string, string, string) (domain.SnapshotExecution, bool, error) {
	return domain.SnapshotExecution{}, false, nil
}
func (i *compiledHTTPIndex) Candidate(_ context.Context, s domain.SnapshotScope, key, job string, complete bool) (domain.SnapshotCandidate, error) {
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
func (i *compiledHTTPIndex) Ready(_ context.Context, s domain.SnapshotScope, key string) (domain.SnapshotCandidate, error) {
	i.calls++
	if !i.ready || s != i.candidate.Scope || key != i.candidate.Key {
		return domain.SnapshotCandidate{}, domain.ErrSnapshotUnavailable
	}
	return i.candidate, nil
}
func (i *compiledHTTPIndex) Reserve(_ context.Context, c domain.SnapshotCandidate) error {
	if !c.SameArtifact(i.candidate) {
		return domain.ErrSnapshotConflict
	}
	i.reserved = true
	return nil
}
func (i *compiledHTTPIndex) WithPublishing(_ context.Context, c domain.SnapshotCandidate, fn func() error) error {
	if !i.reserved || i.ready || !c.SameArtifact(i.candidate) {
		return domain.ErrSnapshotUnavailable
	}
	return fn()
}
func (i *compiledHTTPIndex) WithReady(ctx context.Context, s domain.SnapshotScope, key, root string, fn func(domain.SnapshotCandidate) error) error {
	c, err := i.Ready(ctx, s, key)
	if err != nil {
		return err
	}
	if c.Root != root {
		return domain.ErrSnapshotConflict
	}
	return fn(c)
}
func (i *compiledHTTPIndex) CommitReady(_ context.Context, c domain.SnapshotCandidate, fn func() error) error {
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

type compiledHTTPFixture struct {
	store   *SandboxBundleStore
	backend *sandboxObjectFake
	index   *compiledHTTPIndex
	service *CompiledSnapshotContentService
	key     ed25519.PrivateKey
	tls     *tls.ConnectionState
	body    []byte
	now     time.Time
}

func compiledNewHTTPFixture(t *testing.T) *compiledHTTPFixture {
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
	index := &compiledHTTPIndex{candidate: candidate}
	signer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{11}, 32))
	now := time.Now().UTC()
	service, err := NewCompiledSnapshotContentService(store, index, sandboxBundleTestKeys(signer.Public().(ed25519.PublicKey)), []string{"dns:sandbox.test"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	_, leaf := sandboxTestCertificate(t, "sandbox.test")
	return &compiledHTTPFixture{store, backend, index, service, signer, &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf}}, PeerCertificates: []*x509.Certificate{leaf}}, body, now}
}
func (f *compiledHTTPFixture) claims(p runtimev1.RustCompiledSnapshotPurposeV1) *runtimev1.RustCompiledSnapshotGrantClaimsV1 {
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
func (f *compiledHTTPFixture) request(t *testing.T, method, suffix, media string, body []byte, c *runtimev1.RustCompiledSnapshotGrantClaimsV1) *httptest.ResponseRecorder {
	t.Helper()
	exact, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	input := append([]byte(sandboxBundleGrantDomain), make([]byte, 8)...)
	binary.BigEndian.PutUint64(input[len(sandboxBundleGrantDomain):], uint64(len(exact)))
	input = append(input, exact...)
	grant, _ := proto.Marshal(&runtimev1.SignedSandboxJobGrantV1{KeyId: "key-1", ClaimsBytes: exact, Signature: ed25519.Sign(f.key, input)})
	r := httptest.NewRequest(method, "/"+f.index.candidate.Root+suffix, bytes.NewReader(body))
	r.TLS = f.tls
	r.Header.Set(SandboxCompiledGrantHeader, base64.StdEncoding.EncodeToString(grant))
	if media != "" {
		r.Header.Set("Content-Type", media)
	}
	w := httptest.NewRecorder()
	f.service.Routes().ServeHTTP(w, r)
	return w
}
func compiledMultipart(t *testing.T, descriptor, body []byte) (string, []byte) {
	t.Helper()
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="descriptor"`)
	header.Set("Content-Type", "application/json")
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(descriptor)
	header = textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="elitea-code-job"`)
	header.Set("Content-Type", "application/octet-stream")
	part, err = writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(body)
	_ = writer.Close()
	return writer.FormDataContentType(), out.Bytes()
}
func TestCompiledSnapshotHTTPStagingIsInertUntilSuccessfulReceiptAndCleanup(t *testing.T) {
	f := compiledNewHTTPFixture(t)
	publish := f.claims(runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH)
	read := f.claims(runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_READ)
	media, body := compiledMultipart(t, f.index.candidate.DescriptorJSON, f.body)
	w := f.request(t, http.MethodPut, "/files/elitea-code-job", media, body, publish)
	if w.Code != 204 || !f.index.reserved || f.index.ready {
		t.Fatal(w.Code, w.Body.String())
	}
	w = f.request(t, http.MethodGet, "", "", nil, read)
	if w.Code != 409 {
		t.Fatal("staged metadata readable", w.Code)
	}
	w = f.request(t, http.MethodGet, "/files/elitea-code-job", "", nil, read)
	if w.Code != 409 {
		t.Fatal("staged executable readable", w.Code)
	}
	for _, cleanup := range []bool{false, true} {
		f.index.cleaned = cleanup
		w = f.request(t, http.MethodPost, "", "application/json", f.index.candidate.DescriptorJSON, publish)
		if w.Code != 409 {
			t.Fatal("missing successful terminal receipt accepted")
		}
	}
	f.index.completed = true
	f.index.cleaned = false
	w = f.request(t, http.MethodPost, "", "application/json", f.index.candidate.DescriptorJSON, publish)
	if w.Code != 409 {
		t.Fatal("missing cleanup accepted")
	}
	f.index.cleaned = true
	w = f.request(t, http.MethodPost, "", "application/json", f.index.candidate.DescriptorJSON, publish)
	if w.Code != 204 || !f.index.ready {
		t.Fatal(w.Code, w.Body.String())
	}
	w = f.request(t, http.MethodGet, "/files/elitea-code-job", "", nil, read)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), f.body) {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestCompiledSnapshotHTTPCorruptContentCannotBecomeReady(t *testing.T) {
	f := compiledNewHTTPFixture(t)
	publish := f.claims(runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH)
	media, body := compiledMultipart(t, f.index.candidate.DescriptorJSON, f.body)
	if w := f.request(t, http.MethodPut, "/files/elitea-code-job", media, body, publish); w.Code != 204 {
		t.Fatal(w.Code)
	}
	for ref, raw := range f.backend.objects {
		if bytes.Equal(raw, f.body) {
			f.backend.objects[ref] = []byte("corrupt")
		}
	}
	f.index.completed = true
	f.index.cleaned = true
	w := f.request(t, http.MethodPost, "", "application/json", f.index.candidate.DescriptorJSON, publish)
	if w.Code != 422 || f.index.ready {
		t.Fatal("corrupt publication", w.Code)
	}
	scope, _ := NewSandboxBundleScope(f.index.candidate.Scope.TenantID, f.index.candidate.Scope.ProjectID)
	if _, err := f.store.OpenCompiledSnapshot(context.Background(), scope, f.index.candidate.Root); err == nil {
		t.Fatal("metadata published before verified content")
	}
}
func TestCompiledSnapshotHTTPExactRoleAndFreshAuthority(t *testing.T) {
	for _, name := range []string{"compile", "execute", "publish-read", "read-publish", "expired", "audience", "lease", "root", "plaintext"} {
		t.Run(name, func(t *testing.T) {
			f := compiledNewHTTPFixture(t)
			claims := f.claims(runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_READ)
			method := http.MethodGet
			switch name {
			case "compile":
				claims.Purpose = runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_COMPILE
			case "execute":
				claims.Purpose = runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_EXECUTE
			case "publish-read":
				claims = f.claims(runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH)
			case "read-publish":
				method = http.MethodPost
			case "expired":
				claims.ExpiresAtUnixMillis = 1000000
			case "audience":
				claims.Audience = "dns:other.test"
			case "lease":
				claims = f.claims(runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH)
				claims.CompilationLeaseEpoch = 0
				method = http.MethodPost
			case "root":
				claims.DescriptorSha256 = bytes.Repeat([]byte{1}, 32)
			case "plaintext":
				f.tls = nil
			}
			w := f.request(t, method, "", "", nil, claims)
			if w.Code != 403 || f.index.calls != 0 || len(f.backend.puts) != 0 {
				t.Fatal("wrong authority reached storage", w.Code)
			}
		})
	}
}
func TestCompiledSnapshotHTTPRejectsCorruptUploadBeforeBackendWrite(t *testing.T) {
	f := compiledNewHTTPFixture(t)
	claims := f.claims(runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH)
	media, body := compiledMultipart(t, f.index.candidate.DescriptorJSON, []byte("substitution"))
	w := f.request(t, http.MethodPut, "/files/elitea-code-job", media, body, claims)
	if w.Code != 422 || len(f.backend.puts) != 0 || f.index.ready {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := f.request(t, http.MethodPut, "/files/../anything", media, body, claims); w.Code == 204 {
		t.Fatal("arbitrary path accepted")
	}
}
func TestCompiledSnapshotObjectStoreFixedPathsAndBounds(t *testing.T) {
	f := compiledNewHTTPFixture(t)
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

func TestCompiledSnapshotHTTPStagingRequiresReclaimedLivePublisherLease(t *testing.T) {
	f := compiledNewHTTPFixture(t)
	f.index.candidate.CurrentLeaseLive = false
	publish := f.claims(runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH)
	media, body := compiledMultipart(t, f.index.candidate.DescriptorJSON, f.body)
	if w := f.request(t, http.MethodPut, "/files/elitea-code-job", media, body, publish); w.Code != 409 || len(f.backend.puts) != 0 || f.index.reserved {
		t.Fatal("released lease authorized staging", w.Code)
	}
	f.index.candidate.CurrentLeaseLive = true
	if w := f.request(t, http.MethodPut, "/files/elitea-code-job", media, body, publish); w.Code != 204 {
		t.Fatal("fresh publisher lease rejected", w.Code)
	}
}
