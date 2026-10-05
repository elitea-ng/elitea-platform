package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"github.com/go-chi/chi/v5"
)

type NodeRecoveryResultStore interface {
	ReadNodeRecoveryResult(context.Context, ContentClaim, string, string) ([]byte, error)
}

func (s *ContentServer) WithNodeRecoveryResults(store NodeRecoveryResultStore) *ContentServer {
	if s != nil {
		s.nodeRecoveryResults = store
	}
	return s
}

func (s *ContentServer) PostNodeRecoveryResult(w http.ResponseWriter, r *http.Request) {
	contentID, version := chi.URLParam(r, "contentID"), chi.URLParam(r, "version")
	if s.nodeRecoveryResults == nil || r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.URL.RawQuery != "" || !domain.ValidID(contentID) || !domain.ValidID(version) {
		http.Error(w, "Invalid node recovery result request", http.StatusBadRequest)
		return
	}
	claim, err := parseExecutionClaim(r)
	if err != nil {
		http.Error(w, "Node recovery claim denied", http.StatusForbidden)
		return
	}
	if !s.takeNodeRecoverySlot(w) {
		return
	}
	defer func() { <-s.requests }()
	data, err := s.nodeRecoveryResults.ReadNodeRecoveryResult(r.Context(), claim, contentID, version)
	if err != nil {
		writeNodeRecoveryControlError(w, err)
		return
	}
	if len(data) == 0 || len(data) > 1024*1024 {
		writeNodeRecoveryControlError(w, ErrContentRejected)
		return
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != version {
		writeNodeRecoveryControlError(w, ErrContentRejected)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("X-Elitea-Content-SHA256", version)
	_, _ = w.Write(data)
}
