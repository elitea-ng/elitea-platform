package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"unicode/utf8"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"
)

const ToolkitDiscoveryArtifactMediaType = "application/vnd.elitea.toolkit-available-tools.v1+json"
const MaxToolkitDiscoveryArtifactBytes = 1024 * 1024

// ToolkitDiscoveryArtifactStore keeps immutable results outside command transport.
// Every operation validates the live claim and its admitted settings entry.
type ToolkitDiscoveryArtifactStore interface {
	PutToolkitDiscoveryArtifact(context.Context, ContentClaim, []byte) (*runtimev1.ToolkitAvailableToolsArtifactReferenceV1, error)
	GetToolkitDiscoveryArtifact(context.Context, ContentClaim) ([]byte, error)
}

// WithToolkitDiscoveryArtifacts adds a bounded result route before serving requests.
func (s *ContentServer) WithToolkitDiscoveryArtifacts(store ToolkitDiscoveryArtifactStore) *ContentServer {
	s.toolkitArtifacts = store
	return s
}

func (s *ContentServer) PutToolkitDiscoveryArtifact(w http.ResponseWriter, r *http.Request) {
	setPrivateNoCacheHeaders(w.Header())
	if !s.acquire(w) {
		return
	}
	defer s.release()
	claim, err := parseContentClaim(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	// Authorize before reading the bounded request. The write repeats this
	// check under database locks to fence cancellation and claim replacement.
	authorization, err := s.authorizer.AuthorizeContent(r.Context(), claim)
	if err != nil || authorization.CapabilityID != "toolkit.available_tools.v1" || authorization.SemanticRole != "toolkit.available_tools.settings" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if r.ContentLength <= 0 || r.ContentLength > MaxToolkitDiscoveryArtifactBytes {
		http.Error(w, "Request Entity Too Large", http.StatusRequestEntityTooLarge)
		return
	}
	if r.Header.Get("Content-Type") != ToolkitDiscoveryArtifactMediaType || len(r.TransferEncoding) != 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxToolkitDiscoveryArtifactBytes+1))
	defer clearContentBytes(body)
	if err != nil || int64(len(body)) != r.ContentLength || len(body) > MaxToolkitDiscoveryArtifactBytes || !utf8.Valid(body) || !json.Valid(body) {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	digest := sha256.Sum256(body)
	supplied, ok := singleHeader(r.Header, "Content-Digest")
	if !ok || supplied != formatSHA256Digest(digest) {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	ref, err := s.toolkitArtifacts.PutToolkitDiscoveryArtifact(r.Context(), claim, body)
	if err != nil {
		toolkitArtifactError(w, err)
		return
	}
	encoded, err := proto.Marshal(ref)
	if err != nil {
		toolkitArtifactError(w, ErrContentUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/protobuf")
	w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
	w.Header().Set("Content-Digest", formatSHA256Digest(sha256.Sum256(encoded)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func (s *ContentServer) GetToolkitDiscoveryArtifact(w http.ResponseWriter, r *http.Request) {
	setPrivateNoCacheHeaders(w.Header())
	if !s.acquire(w) {
		return
	}
	defer s.release()
	claim, err := parseContentClaim(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := s.toolkitArtifacts.GetToolkitDiscoveryArtifact(r.Context(), claim)
	if err != nil {
		toolkitArtifactError(w, err)
		return
	}
	defer clearContentBytes(body)
	w.Header().Set("Content-Type", ToolkitDiscoveryArtifactMediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Content-Digest", formatSHA256Digest(sha256.Sum256(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func toolkitArtifactError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	if errors.Is(err, ErrContentUnauthorized) {
		status = http.StatusForbidden
	}
	if errors.Is(err, ErrContentRejected) {
		status = http.StatusConflict
	}
	if errors.Is(err, ErrContentNotFound) {
		status = http.StatusNotFound
	}
	http.Error(w, http.StatusText(status), status)
}
