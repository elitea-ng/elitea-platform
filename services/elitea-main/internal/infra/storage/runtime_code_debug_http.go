package storage

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/go-chi/chi/v5"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Mount under the existing execution/generation mTLS content route.
// No source or binary payload passes through Redis or a public control route.
func (s *RuntimeCodeDebugArtifactService) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(s.boundedHTTP)
	r.Post("/admit", s.admitHTTP)
	r.Post("/commit/{visit_id}", s.commitHTTP)
	return r
}
func (s *RuntimeCodeDebugArtifactService) admitHTTP(w http.ResponseWriter, r *http.Request) {
	setPrivateNoCacheHeaders(w.Header())
	claim, err := parseExecutionClaim(r)
	if err != nil {
		codeDebugHTTPError(w, err)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxCodeDebugAdmissionBytes+1))
	if err != nil || len(raw) > MaxCodeDebugAdmissionBytes {
		codeDebugHTTPError(w, ErrContentRejected)
		return
	}
	var a CodeDebugAdmission
	if strictCodeDebugJSON(raw, &a) != nil {
		codeDebugHTTPError(w, ErrContentRejected)
		return
	}
	if err = s.Admit(r.Context(), claim, a); err != nil {
		codeDebugHTTPError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *RuntimeCodeDebugArtifactService) commitHTTP(w http.ResponseWriter, r *http.Request) {
	setPrivateNoCacheHeaders(w.Header())
	claim, err := parseExecutionClaim(r)
	if err != nil {
		codeDebugHTTPError(w, err)
		return
	}
	if r.Header.Get("Content-Type") != "application/json" || r.ContentLength < 1 || r.ContentLength > MaxCodeDebugSnapshotBytes {
		codeDebugHTTPError(w, ErrContentRejected)
		return
	}
	upload, err := s.prepareCommit(r.Context(), claim, chi.URLParam(r, "visit_id"))
	if err != nil {
		codeDebugHTTPError(w, err)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxCodeDebugSnapshotBytes+1))
	if err != nil || int64(len(raw)) != r.ContentLength {
		codeDebugHTTPError(w, ErrContentRejected)
		return
	}
	ref, err := s.commitAuthorized(r.Context(), claim, upload, raw)
	if err != nil {
		codeDebugHTTPError(w, err)
		return
	}
	body, err := json.Marshal(ref)
	if err != nil || len(body) > 4096 {
		codeDebugHTTPError(w, ErrContentUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
func codeDebugHTTPError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	if errors.Is(err, ErrContentUnauthorized) {
		status = http.StatusForbidden
	} else if errors.Is(err, ErrContentNotFound) {
		status = http.StatusNotFound
	} else if errors.Is(err, ErrContentRejected) {
		status = http.StatusUnprocessableEntity
	}
	http.Error(w, "The Code debug artifact is unavailable.", status)
}

// Four binary requests share a fixed memory budget. No request waits in a queue.
func (s *RuntimeCodeDebugArtifactService) boundedHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setPrivateNoCacheHeaders(w.Header())
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		default:
			http.Error(w, "The Code debug artifact is unavailable.", http.StatusTooManyRequests)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Second))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
