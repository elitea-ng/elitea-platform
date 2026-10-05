package storage

import (
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"strconv"
)

// A nil dependency leaves the route absent during a rolling deployment.
func (s *ContentServer) WithCodeWorkspaces(workspaces *CodeWorkspaceService) *ContentServer {
	if s == nil {
		return nil
	}
	s.codeWorkspaces = workspaces
	return s
}

func (s *ContentServer) PostCodeWorkspace(w http.ResponseWriter, r *http.Request) {
	setPrivateNoCacheHeaders(w.Header())
	if !s.acquire(w) {
		return
	}
	defer s.release()
	if s.codeWorkspaces == nil || r.Header.Get("Content-Type") != "application/json" ||
		r.ContentLength <= 0 || r.ContentLength > 2<<20 || len(r.TransferEncoding) != 0 {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	claim, err := parseExecutionClaim(r)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
	if err != nil || int64(len(body)) != r.ContentLength {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	request, err := ParseCodeWorkspaceResolveRequest(body, s.codeWorkspaces.policy)
	if err != nil {
		codeWorkspaceHTTPError(w, err)
		return
	}
	manifest, err := s.codeWorkspaces.Resolve(r.Context(), claim, request)
	if err != nil {
		codeWorkspaceHTTPError(w, err)
		return
	}
	native := manifest.Bytes()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(native)))
	w.Header().Set("Content-Digest", formatSHA256Digest(sha256.Sum256(native)))
	w.Header().Set("X-Elitea-Workspace-Root", manifest.Digest())
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(native); err != nil {
		s.logger.WarnContext(r.Context(), "Code workspace response write failed")
	}
}

func codeWorkspaceHTTPError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, ErrContentUnauthorized):
		status = http.StatusForbidden
	case errors.Is(err, ErrCodeWorkspaceInvalid), errors.Is(err, ErrCodeWorkspaceCapability), errors.Is(err, ErrCodeWorkspaceWritableUnavailable):
		status = http.StatusUnprocessableEntity
	}
	var bounds *CodeWorkspaceBoundsError
	if errors.As(err, &bounds) {
		// Only typed constant bound names and operator numbers reach this response.
		http.Error(w, bounds.Error(), http.StatusUnprocessableEntity)
		return
	}
	http.Error(w, http.StatusText(status), status)
}
