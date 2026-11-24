package llmproxy

import (
	"log/slog"
	"net/http"
)

// newTestHandler is NewHandler for this package's unit tests.
//
// Production refuses every identity-bearing request when the identity secret
// is empty (verifySignature fails closed). Most tests here exercise routing,
// billing, guardrails and streaming and build their requests without a
// signature, so a test that passes NO secret gets a handler whose identity
// check accepts the unsigned headers. A test that passes a secret gets the real
// verifier, unchanged. The refusal itself is pinned by
// TestNewHandler_EmptySecretRefusesEveryIdentity, which calls NewHandler
// directly.
func newTestHandler(router LLMRouter, logger *slog.Logger, identitySecret []byte, opts ...HandlerOption) *Handler {
	h := NewHandler(router, logger, identitySecret, opts...)
	if len(identitySecret) == 0 {
		h.identityCheck = func(http.Header) bool { return true }
	}
	return h
}
