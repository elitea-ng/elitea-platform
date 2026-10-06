package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
	"github.com/go-chi/chi/v5"
	"google.golang.org/protobuf/proto"
)

const SandboxCompiledGrantHeader = "X-Elitea-Sandbox-Compiled-Grant"

type CompiledSnapshotContentService struct {
	store     *SandboxBundleStore
	index     domain.RustSnapshotIndex
	keys      SandboxBundleKeys
	audiences map[string]struct{}
	now       func() time.Time
	requests  chan struct{}
}

func NewCompiledSnapshotContentService(store *SandboxBundleStore, index domain.RustSnapshotIndex, keys SandboxBundleKeys, audiences []string, now func() time.Time) (*CompiledSnapshotContentService, error) {
	if store == nil || index == nil || keys == nil || now == nil || len(audiences) == 0 || len(audiences) > 16 {
		return nil, domain.ErrSnapshotInvalid
	}
	allowed := map[string]struct{}{}
	for _, a := range audiences {
		if !sandboxClaimIdentity(a) {
			return nil, domain.ErrSnapshotInvalid
		}
		allowed[a] = struct{}{}
	}
	return &CompiledSnapshotContentService{store, index, keys, allowed, now, make(chan struct{}, 4)}, nil
}
func (s *CompiledSnapshotContentService) authorize(r *http.Request, root string) (*runtimev1.RustCompiledSnapshotGrantClaimsV1, error) {
	if !domain.SnapshotDigest(root) || r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return nil, ErrContentUnauthorized
	}
	peer, err := workloadIdentity(r.TLS.PeerCertificates[0])
	if err != nil {
		return nil, ErrContentUnauthorized
	}
	if _, ok := s.audiences[peer]; !ok {
		return nil, ErrContentUnauthorized
	}
	header, ok := singleHeader(r.Header, SandboxCompiledGrantHeader)
	if !ok || len(header) == 0 || len(header) > 8192 {
		return nil, ErrContentUnauthorized
	}
	native, err := base64.StdEncoding.Strict().DecodeString(header)
	if err != nil || base64.StdEncoding.EncodeToString(native) != header {
		return nil, ErrContentUnauthorized
	}
	grant := &runtimev1.SignedSandboxJobGrantV1{}
	if runtimegrpc.ScanStrictMessage(native, grant.ProtoReflect().Descriptor()) != nil || proto.Unmarshal(native, grant) != nil || !sandboxClaimIdentity(grant.KeyId) || len(grant.Signature) != ed25519.SignatureSize || len(grant.ClaimsBytes) == 0 || len(grant.ClaimsBytes) > 4096 {
		return nil, ErrContentUnauthorized
	}
	key, err := s.keys.ResolveEd25519PublicKey(r.Context(), grant.KeyId)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, ErrContentUnauthorized
	}
	input := make([]byte, len(sandboxBundleGrantDomain)+8+len(grant.ClaimsBytes))
	offset := copy(input, sandboxBundleGrantDomain)
	binary.BigEndian.PutUint64(input[offset:offset+8], uint64(len(grant.ClaimsBytes)))
	copy(input[offset+8:], grant.ClaimsBytes)
	if !ed25519.Verify(key, input, grant.Signature) {
		return nil, ErrContentUnauthorized
	}
	c := &runtimev1.RustCompiledSnapshotGrantClaimsV1{}
	if runtimegrpc.ScanStrictMessage(grant.ClaimsBytes, c.ProtoReflect().Descriptor()) != nil || proto.Unmarshal(grant.ClaimsBytes, c) != nil {
		return nil, ErrContentUnauthorized
	}
	now := s.now().UnixMilli()
	if c.Revision != 4 || c.Audience != peer || c.Generation == 0 || !sandboxClaimIdentity(c.TenantId) || !sandboxClaimIdentity(c.ExecutionId) || !sandboxClaimIdentity(c.ActivationId) || !sandboxClaimIdentity(c.SubmitterWorkloadIdentity) || c.ProjectId <= 0 || len(c.RequestDigest) != 32 || len(c.BasePreparedRequestSha256) != 32 || len(c.SnapshotKeySha256) != 32 || len(c.DescriptorSha256) != 32 || hex.EncodeToString(c.DescriptorSha256) != root || c.IssuedAtUnixMillis < 0 || c.IssuedAtUnixMillis > now || c.ExpiresAtUnixMillis <= now || c.ExpiresAtUnixMillis-c.IssuedAtUnixMillis > 30000 {
		return nil, ErrContentUnauthorized
	}
	read := runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_READ
	publish := runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH
	switch r.Method {
	case http.MethodGet:
		if c.Purpose != read || len(c.CompilationJobKey) != 0 || c.CompilationRuntimeId != "" || len(c.CompilationRequestDigest) != 0 || c.CompilationLeaseEpoch != 0 {
			return nil, ErrContentUnauthorized
		}

	case http.MethodPut, http.MethodPost:
		if c.Purpose != publish || len(c.CompilationJobKey) != 32 || c.CompilationRuntimeId == "" || len(c.CompilationRuntimeId) > 512 || len(c.CompilationRequestDigest) != 32 || c.CompilationLeaseEpoch == 0 || domain.SnapshotActivationKey(c.ExecutionId, c.ActivationId) != hex.EncodeToString(c.CompilationJobKey) {
			return nil, ErrContentUnauthorized
		}

	default:
		return nil, ErrContentUnauthorized

	}
	return c, nil
}
func (s *CompiledSnapshotContentService) Routes() http.Handler {
	router := chi.NewRouter()
	router.Get("/{root}", s.metadata)
	router.Post("/{root}", s.publish)
	router.Get("/{root}/files/{name}", s.download)
	router.Put("/{root}/files/{name}", s.upload)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setPrivateNoCacheHeaders(w.Header())
		select {
		case s.requests <- struct{}{}:
			defer func() { <-s.requests }()
		default:
			compiledHTTPError(w, ErrContentUnavailable)
			return
		}
		router.ServeHTTP(w, r)
	})
}
func (s *CompiledSnapshotContentService) admission(w http.ResponseWriter, r *http.Request) (*http.Request, *runtimev1.RustCompiledSnapshotGrantClaimsV1, context.CancelFunc, bool) {
	c, err := s.authorize(r, chi.URLParam(r, "root"))
	if err != nil {
		compiledHTTPError(w, err)
		return r, nil, func() {}, false
	}
	ctx, cancel := context.WithDeadline(r.Context(), time.UnixMilli(c.ExpiresAtUnixMillis))
	return r.WithContext(ctx), c, cancel, true
}
func compiledScope(c *runtimev1.RustCompiledSnapshotGrantClaimsV1) domain.SnapshotScope {
	return domain.SnapshotScope{TenantID: c.TenantId, ProjectID: c.ProjectId}
}
func compiledBindingClaims(candidate domain.SnapshotCandidate, c *runtimev1.RustCompiledSnapshotGrantClaimsV1) error {
	d, err := candidate.Descriptor()
	if err != nil || candidate.Root != hex.EncodeToString(c.DescriptorSha256) || candidate.Key != hex.EncodeToString(c.SnapshotKeySha256) || d.Binding.BasePreparedRequestSHA256 != hex.EncodeToString(c.BasePreparedRequestSha256) {
		return ErrContentUnauthorized
	}
	expected, err := domain.SnapshotJobDigest("execute", d.Binding, candidate.Root)
	if c.Purpose == runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH {
		expected = candidate.CompilationRequestDigest
		if candidate.CompilationJobKey != hex.EncodeToString(c.CompilationJobKey) || candidate.CompilationRequestDigest != hex.EncodeToString(c.CompilationRequestDigest) || candidate.CompilationRuntimeID != c.CompilationRuntimeId || candidate.CompilationLeaseEpoch != c.CompilationLeaseEpoch {
			return ErrContentUnauthorized
		}
	}
	if err != nil || expected != hex.EncodeToString(c.RequestDigest) {
		return ErrContentUnauthorized
	}
	return nil
}
func (s *CompiledSnapshotContentService) publicationCandidate(ctx context.Context, c *runtimev1.RustCompiledSnapshotGrantClaimsV1, complete bool) (domain.SnapshotCandidate, error) {
	candidate, err := s.index.Candidate(ctx, compiledScope(c), hex.EncodeToString(c.SnapshotKeySha256), hex.EncodeToString(c.CompilationJobKey), complete)
	if err != nil {
		return candidate, err
	}
	if !complete && candidate.ReceiptSHA256 == "" && !candidate.CurrentLeaseLive {
		return candidate, domain.ErrSnapshotUnavailable
	}
	return candidate, compiledBindingClaims(candidate, c)
}
func (s *CompiledSnapshotContentService) metadata(w http.ResponseWriter, r *http.Request) {
	r, c, cancel, ok := s.admission(w, r)
	if !ok {
		return
	}
	defer cancel()
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		compiledHTTPError(w, ErrContentRejected)
		return
	}
	var body []byte
	err := s.index.WithReady(r.Context(), compiledScope(c), hex.EncodeToString(c.SnapshotKeySha256), hex.EncodeToString(c.DescriptorSha256), func(candidate domain.SnapshotCandidate) error {
		if err := compiledBindingClaims(candidate, c); err != nil {
			return err
		}
		scope, _ := NewSandboxBundleScope(c.TenantId, c.ProjectId)
		bundle, err := s.store.OpenCompiledSnapshot(r.Context(), scope, candidate.Root)
		if err != nil {
			return err
		}
		if !bytes.Equal(bundle.native, candidate.DescriptorJSON) {
			return ErrContentRejected
		}
		body = bundle.native
		return nil
	})
	if err != nil {
		compiledHTTPError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}
func (s *CompiledSnapshotContentService) upload(w http.ResponseWriter, r *http.Request) {
	r, c, cancel, ok := s.admission(w, r)
	if !ok {
		return
	}
	defer cancel()
	if chi.URLParam(r, "name") != SnapshotExecutableName {
		compiledHTTPError(w, ErrContentRejected)
		return
	}
	candidate, err := s.publicationCandidate(r.Context(), c, false)
	if err != nil {
		compiledHTTPError(w, err)
		return
	}
	if err = s.index.Reserve(r.Context(), candidate); err != nil {
		compiledHTTPError(w, err)
		return
	}
	const maxBytes = domain.SnapshotDescriptorLimit + domain.SnapshotExecutableLimit + 4096
	media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "multipart/form-data" || len(params) != 1 || params["boundary"] == "" || len(params["boundary"]) > 70 || len(r.TransferEncoding) != 0 || r.ContentLength <= 0 || r.ContentLength > maxBytes {
		compiledHTTPError(w, ErrContentRejected)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	parts := multipart.NewReader(r.Body, params["boundary"])
	metadata, err := parts.NextRawPart()
	if err != nil || metadata.FormName() != "descriptor" || metadata.FileName() != "" || metadata.Header.Get("Content-Type") != "application/json" || metadata.Header.Get("Content-Transfer-Encoding") != "" {
		compiledHTTPError(w, ErrContentRejected)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(metadata, domain.SnapshotDescriptorLimit+1))
	if err != nil || !bytes.Equal(raw, candidate.DescriptorJSON) {
		compiledHTTPError(w, ErrContentRejected)
		return
	}
	bundle, err := ParseRustCompiledSnapshot(raw, candidate.Root)
	if err != nil {
		compiledHTTPError(w, err)
		return
	}
	file, err := parts.NextRawPart()
	if err != nil || file.FormName() != "file" || file.FileName() != SnapshotExecutableName || file.Header.Get("Content-Type") != "application/octet-stream" || file.Header.Get("Content-Transfer-Encoding") != "" {
		compiledHTTPError(w, ErrContentRejected)
		return
	}
	scope, _ := NewSandboxBundleScope(c.TenantId, c.ProjectId)
	err = s.index.WithPublishing(r.Context(), candidate, func() error {
		if err := s.store.PutFile(r.Context(), scope, bundle, SnapshotExecutableName, file); err != nil {
			return err
		}
		if _, err := parts.NextRawPart(); err != io.EOF {
			return ErrContentRejected
		}
		return nil
	})
	if err != nil {
		compiledHTTPError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *CompiledSnapshotContentService) publish(w http.ResponseWriter, r *http.Request) {
	r, c, cancel, ok := s.admission(w, r)
	if !ok {
		return
	}
	defer cancel()
	if r.Header.Get("Content-Type") != "application/json" || len(r.TransferEncoding) != 0 || r.ContentLength < 1 || r.ContentLength > domain.SnapshotDescriptorLimit {
		compiledHTTPError(w, ErrContentRejected)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, domain.SnapshotDescriptorLimit)
	raw, err := io.ReadAll(r.Body)
	if err != nil || int64(len(raw)) != r.ContentLength {
		compiledHTTPError(w, ErrContentRejected)
		return
	}
	candidate, err := s.publicationCandidate(r.Context(), c, true)
	if err != nil || !bytes.Equal(raw, candidate.DescriptorJSON) {
		if err == nil {
			err = ErrContentRejected
		}
		compiledHTTPError(w, err)
		return
	}
	bundle, err := ParseRustCompiledSnapshot(raw, candidate.Root)
	if err != nil {
		compiledHTTPError(w, err)
		return
	}
	scope, _ := NewSandboxBundleScope(c.TenantId, c.ProjectId)
	err = s.index.CommitReady(r.Context(), candidate, func() error { return s.store.Publish(r.Context(), scope, bundle) })
	if err != nil {
		compiledHTTPError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *CompiledSnapshotContentService) download(w http.ResponseWriter, r *http.Request) {
	r, c, cancel, ok := s.admission(w, r)
	if !ok {
		return
	}
	defer cancel()
	if chi.URLParam(r, "name") != SnapshotExecutableName || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		compiledHTTPError(w, ErrContentRejected)
		return
	}
	file, err := os.CreateTemp(s.store.spoolDir, "compiled-delivery-*")
	if err != nil {
		compiledHTTPError(w, err)
		return
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	var size int64
	var digest string
	err = s.index.WithReady(r.Context(), compiledScope(c), hex.EncodeToString(c.SnapshotKeySha256), hex.EncodeToString(c.DescriptorSha256), func(candidate domain.SnapshotCandidate) error {
		if err := compiledBindingClaims(candidate, c); err != nil {
			return err
		}
		scope, _ := NewSandboxBundleScope(c.TenantId, c.ProjectId)
		bundle, err := s.store.OpenCompiledSnapshot(r.Context(), scope, candidate.Root)
		if err != nil {
			return err
		}
		if !bytes.Equal(bundle.native, candidate.DescriptorJSON) {
			return ErrContentRejected
		}
		size = bundle.descriptor.ExecutableBytes
		digest = bundle.descriptor.ExecutableSHA256
		return s.store.FetchFile(r.Context(), scope, bundle, SnapshotExecutableName, file)
	})
	if err != nil {
		compiledHTTPError(w, err)
		return
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		compiledHTTPError(w, err)
		return
	}
	hash, _ := hex.DecodeString(digest)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(hash)+":")
	_, _ = io.Copy(w, sandboxContextReader{r.Context(), file})
}
func compiledHTTPError(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	message := "Compiled snapshot storage is unavailable."
	switch {
	case errors.Is(err, ErrContentUnauthorized), errors.Is(err, ErrAccessDenied):
		code = http.StatusForbidden
		message = "The compiled snapshot grant is not accepted."
	case errors.Is(err, ErrContentRejected), errors.Is(err, domain.ErrSnapshotInvalid):
		code = http.StatusUnprocessableEntity
		message = "Compiled snapshot content does not match its immutable descriptor."
	case errors.Is(err, domain.ErrSnapshotConflict), errors.Is(err, domain.ErrSnapshotUnavailable), errors.Is(err, domain.ErrSnapshotMiss):
		code = http.StatusConflict
		message = "The original compilation or selected snapshot is not ready."
	case errors.Is(err, domain.ErrSnapshotQuota), errors.Is(err, ErrContentUnavailable):
		code = http.StatusServiceUnavailable
		message = "Compiled snapshot capacity is full."
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = http.StatusRequestTimeout
		message = "Compiled snapshot transfer was interrupted."
	}
	http.Error(w, message, code)
}
func (s *ContentServer) WithCompiledSnapshots(service *CompiledSnapshotContentService) *ContentServer {
	s.compiledSnapshots = service
	return s
}
