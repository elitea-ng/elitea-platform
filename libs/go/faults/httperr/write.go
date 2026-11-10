// Package httperr is the one HTTP error writer of the convention (FU-299):
// it logs the fault line, then answers {"error", "code", "support_ref"}.
// The cause, a panic value and a stack never reach the client (C2).
package httperr

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
)

// SupportRefHeader carries the server-generated support reference.
const SupportRefHeader = "X-Elitea-Support-Ref"

// Body is the JSON error body.
type Body struct {
	// OK is set only for a route whose existing contract answers "ok": false
	// (KeepOKFalse); it is never sensitive.
	OK         *bool  `json:"ok,omitempty"`
	Error      string `json:"error"`
	Code       string `json:"code"`
	SupportRef string `json:"support_ref,omitempty"`
}

// Option adjusts one answer.
type Option func(*Body)

// KeepOKFalse keeps a route's existing "ok": false field.
func KeepOKFalse() Option {
	return func(body *Body) {
		ok := false
		body.OK = &ok
	}
}

// Write logs err once (ERROR for 5xx, WARN otherwise) and answers status
// with the code's sentence, the code and the support reference. r may be
// nil, for a caller with no request; the answer then has no support
// reference.
func Write(w http.ResponseWriter, r *http.Request, logger *slog.Logger, status int, err *faults.Error, options ...Option) {
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	level := slog.LevelWarn
	if status >= 500 {
		level = slog.LevelError
	}
	ref := faults.SupportRef(ctx)
	faults.Log(ctx, logger, level, err)
	header := w.Header()
	if ref != "" {
		header.Set(SupportRefHeader, ref)
	}
	header.Set("Content-Type", "application/json")
	header.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	body := Body{Error: err.Code().Sentence(), Code: err.Code().String(), SupportRef: ref}
	for _, option := range options {
		option(&body)
	}
	_ = json.NewEncoder(w).Encode(body)
}
