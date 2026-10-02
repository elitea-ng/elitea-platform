package storage

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
	"github.com/go-chi/chi/v5"
	"google.golang.org/protobuf/proto"
)

const SandboxBundleGrantHeader = "X-Elitea-Sandbox-Bundle-Grant"
const sandboxBundleGrantDomain = "elitea.sandbox.job-grant.ed25519.v1\x00"

// SandboxBundleKeys is the existing runtime keyring contract. The service never
// holds a signing key or accepts a key supplied by a request.
type SandboxBundleKeys interface {
	ResolveEd25519PublicKey(context.Context, string) (ed25519.PublicKey, error)
}

// SandboxBundleContentService serves only content, under fresh Main authority.
// Compose behind verified client-certificate TLS. Metadata is published last.
type SandboxBundleContentService struct {
	store     *SandboxBundleStore
	keys      SandboxBundleKeys
	audiences map[string]struct{}
	now       func() time.Time
	requests  chan struct{}
}

func NewSandboxBundleContentService(store *SandboxBundleStore, keys SandboxBundleKeys, audiences []string, now func() time.Time) (*SandboxBundleContentService, error) {
	if store == nil || keys == nil || now == nil || len(audiences) == 0 || len(audiences) > 16 {
		return nil, errors.New("sandbox content requires storage, trusted keys, exact recipients, and a clock")
	}
	allowed := make(map[string]struct{}, len(audiences))
	for _, audience := range audiences {
		if !sandboxClaimIdentity(audience) {
			return nil, errors.New("sandbox content recipient is invalid")
		}
		allowed[audience] = struct{}{}
	}
	return &SandboxBundleContentService{store: store, keys: keys, audiences: allowed, now: now, requests: make(chan struct{}, 4)}, nil
}

func sandboxClaimIdentity(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00")
}

func (service *SandboxBundleContentService) authorize(r *http.Request, root string) (SandboxBundleScope, error) {
	if !sandboxDigest(root) || r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return SandboxBundleScope{}, ErrContentUnauthorized
	}
	peer, err := workloadIdentity(r.TLS.PeerCertificates[0])
	if err != nil {
		return SandboxBundleScope{}, ErrContentUnauthorized
	}
	if _, ok := service.audiences[peer]; !ok {
		return SandboxBundleScope{}, ErrContentUnauthorized
	}
	header, ok := singleHeader(r.Header, SandboxBundleGrantHeader)
	if !ok || len(header) == 0 || len(header) > 8192 {
		return SandboxBundleScope{}, ErrContentUnauthorized
	}
	native, err := base64.StdEncoding.Strict().DecodeString(header)
	if err != nil || base64.StdEncoding.EncodeToString(native) != header {
		return SandboxBundleScope{}, ErrContentUnauthorized
	}
	grant := &runtimev1.SignedSandboxJobGrantV1{}
	if runtimegrpc.ScanStrictMessage(native, grant.ProtoReflect().Descriptor()) != nil || proto.Unmarshal(native, grant) != nil || !sandboxClaimIdentity(grant.KeyId) || len(grant.Signature) != ed25519.SignatureSize || len(grant.ClaimsBytes) == 0 || len(grant.ClaimsBytes) > 4096 {
		return SandboxBundleScope{}, ErrContentUnauthorized
	}
	key, err := service.keys.ResolveEd25519PublicKey(r.Context(), grant.KeyId)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return SandboxBundleScope{}, ErrContentUnauthorized
	}
	input := make([]byte, len(sandboxBundleGrantDomain)+8+len(grant.ClaimsBytes))
	offset := copy(input, sandboxBundleGrantDomain)
	binary.BigEndian.PutUint64(input[offset:offset+8], uint64(len(grant.ClaimsBytes)))
	copy(input[offset+8:], grant.ClaimsBytes)
	if !ed25519.Verify(key, input, grant.Signature) {
		return SandboxBundleScope{}, ErrContentUnauthorized
	}
	claims := &runtimev1.SandboxJobGrantClaimsV1{}
	if runtimegrpc.ScanStrictMessage(grant.ClaimsBytes, claims.ProtoReflect().Descriptor()) != nil || proto.Unmarshal(grant.ClaimsBytes, claims) != nil {
		return SandboxBundleScope{}, ErrContentUnauthorized
	}
	now := service.now().UnixMilli()
	if claims.Revision != 3 || claims.CancelOnly || claims.Audience != peer || claims.Generation == 0 || !sandboxClaimIdentity(claims.TenantId) || !sandboxClaimIdentity(claims.ExecutionId) || !sandboxClaimIdentity(claims.ActivationId) || !sandboxClaimIdentity(claims.SubmitterWorkloadIdentity) || len(claims.RequestDigest) != 32 || len(claims.DependencyBundleSha256) != 32 || hex.EncodeToString(claims.DependencyBundleSha256) != root || claims.IssuedAtUnixMillis > now || claims.ExpiresAtUnixMillis <= now || claims.IssuedAtUnixMillis < 0 || claims.ExpiresAtUnixMillis-claims.IssuedAtUnixMillis > 30000 {
		return SandboxBundleScope{}, ErrContentUnauthorized
	}
	return NewSandboxBundleScope(claims.TenantId, claims.ProjectId)
}

func (service *SandboxBundleContentService) Routes() http.Handler {
	router := chi.NewRouter()
	router.Get("/{root}", service.metadata)
	router.Post("/{root}", service.publish)
	router.Put("/{root}/files/{name}", service.upload)
	router.Get("/{root}/files/{name}", service.download)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setPrivateNoCacheHeaders(w.Header())
		select {
		case service.requests <- struct{}{}:
			defer func() { <-service.requests }()
		default:
			sandboxBundleHTTPError(w, r, ErrContentUnavailable)
			return
		}
		router.ServeHTTP(w, r)
	})
}

func (service *SandboxBundleContentService) scope(w http.ResponseWriter, r *http.Request) (SandboxBundleScope, bool) {
	scope, err := service.authorize(r, chi.URLParam(r, "root"))
	if err != nil {
		sandboxBundleHTTPError(w, r, err)
		return SandboxBundleScope{}, false
	}
	return scope, true
}

func sandboxBundleJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Header.Get("Content-Type") != "application/json" || len(r.TransferEncoding) != 0 || r.ContentLength <= 0 || r.ContentLength > sandboxBundleMetadataLimit {
		return nil, ErrContentRejected
	}
	r.Body = http.MaxBytesReader(w, r.Body, sandboxBundleMetadataLimit)
	native, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if int64(len(native)) != r.ContentLength {
		return nil, ErrContentRejected
	}
	return native, nil
}

func (service *SandboxBundleContentService) publish(w http.ResponseWriter, r *http.Request) {
	scope, ok := service.scope(w, r)
	if !ok {
		return
	}
	native, err := sandboxBundleJSONBody(w, r)
	if err == nil {
		var bundle *PythonSandboxBundle
		bundle, err = ParsePythonSandboxBundle(native, chi.URLParam(r, "root"))
		if err == nil {
			err = service.store.Publish(r.Context(), scope, bundle)
		}
	}
	if err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (service *SandboxBundleContentService) upload(w http.ResponseWriter, r *http.Request) {
	scope, ok := service.scope(w, r)
	if !ok {
		return
	}
	const maximum = sandboxBundleMetadataLimit + sandboxBundleFileLimit + 4096
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || len(params) != 1 || params["boundary"] == "" || len(params["boundary"]) > 70 || len(r.TransferEncoding) != 0 || r.ContentLength <= 0 || r.ContentLength > maximum {
		sandboxBundleHTTPError(w, r, ErrContentRejected)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maximum)
	parts := multipart.NewReader(r.Body, params["boundary"])
	metadata, err := parts.NextRawPart()
	if err != nil || metadata.FormName() != "bundle" || metadata.FileName() != "" || metadata.Header.Get("Content-Type") != "application/json" || metadata.Header.Get("Content-Transfer-Encoding") != "" {
		sandboxBundleHTTPError(w, r, ErrContentRejected)
		return
	}
	native, err := io.ReadAll(io.LimitReader(metadata, sandboxBundleMetadataLimit+1))
	if err != nil || len(native) > sandboxBundleMetadataLimit {
		sandboxBundleHTTPError(w, r, ErrContentRejected)
		return
	}
	bundle, err := ParsePythonSandboxBundle(native, chi.URLParam(r, "root"))
	if err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	name := chi.URLParam(r, "name")
	if _, err := bundle.file(name); err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	file, err := parts.NextRawPart()
	if err != nil || file.FormName() != "file" || file.FileName() != name || file.Header.Get("Content-Type") != "application/octet-stream" || file.Header.Get("Content-Transfer-Encoding") != "" {
		sandboxBundleHTTPError(w, r, ErrContentRejected)
		return
	}
	if err := service.store.PutFile(r.Context(), scope, bundle, name, file); err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	// Any already-written bytes are verified content under this exact root.
	// Reject an extra part; do not publish completion for malformed requests.
	if _, err := parts.NextRawPart(); err != io.EOF {
		sandboxBundleHTTPError(w, r, ErrContentRejected)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (service *SandboxBundleContentService) metadata(w http.ResponseWriter, r *http.Request) {
	scope, ok := service.scope(w, r)
	if !ok {
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		sandboxBundleHTTPError(w, r, ErrContentRejected)
		return
	}
	bundle, err := service.store.OpenBundle(r.Context(), scope, chi.URLParam(r, "root"))
	if err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(bundle.native)))
	if _, err := w.Write(bundle.native); err != nil {
		slog.ErrorContext(r.Context(), "sandbox bundle metadata delivery failed")
	}
}

func (service *SandboxBundleContentService) download(w http.ResponseWriter, r *http.Request) {
	scope, ok := service.scope(w, r)
	if !ok {
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		sandboxBundleHTTPError(w, r, ErrContentRejected)
		return
	}
	bundle, err := service.store.OpenBundle(r.Context(), scope, chi.URLParam(r, "root"))
	if err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	file, err := bundle.file(chi.URLParam(r, "name"))
	if err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	staged, err := os.CreateTemp(service.store.spoolDir, "delivery-*")
	if err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	defer func() {
		if err := errors.Join(staged.Close(), os.Remove(staged.Name())); err != nil {
			slog.ErrorContext(r.Context(), "sandbox bundle delivery staging cleanup failed")
		}
	}()
	// Validate before sending headers. Corrupt storage cannot yield HTTP success.
	if err := service.store.FetchFile(r.Context(), scope, bundle, file.Name, staged); err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(file.Bytes, 10))
	hash, _ := hex.DecodeString(file.SHA256)
	w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(hash)+":")
	if _, err := io.Copy(w, sandboxContextReader{r.Context(), staged}); err != nil {
		slog.ErrorContext(r.Context(), "sandbox bundle file delivery failed")
	}
}

func sandboxBundleHTTPError(w http.ResponseWriter, r *http.Request, err error) {
	status, message := http.StatusBadGateway, "Shared package storage failed. Retry the same preparation identity."
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, ErrContentUnauthorized), errors.Is(err, ErrAccessDenied):
		status, message = http.StatusForbidden, "The package storage grant is invalid, expired, or belongs to another workload."
	case errors.Is(err, ErrContentRejected):
		status, message = http.StatusUnprocessableEntity, "Package metadata or content does not match the admitted bundle."
	case errors.Is(err, ErrNotFound):
		status, message = http.StatusNotFound, "The prepared package bundle is incomplete or unavailable."
	case errors.Is(err, ErrContentUnavailable):
		status, message = http.StatusServiceUnavailable, "Package transfer capacity is full. Retry the same preparation identity."
	case errors.As(err, &tooLarge):
		status, message = http.StatusRequestEntityTooLarge, "Package content exceeds its transfer limit."
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status, message = http.StatusRequestTimeout, "Package transfer was interrupted. Retry the same preparation identity."
	}
	if status >= 500 {
		slog.ErrorContext(r.Context(), "sandbox bundle transfer failed", "status", status, "reason", message)
	}
	http.Error(w, message, status)
}

// WithSandboxBundles must be called before Routes. Nil leaves routes absent.
func (server *ContentServer) WithSandboxBundles(service *SandboxBundleContentService) *ContentServer {
	server.sandboxBundles = service
	return server
}
