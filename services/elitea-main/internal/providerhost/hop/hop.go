// Package hop carries the two things every provider hop in this service does
// identically, and one of them was already being done in only one of them.
//
// WHAT IS NOT HERE, and why this package is small. The mTLS transport
// (llmproxy.NewMTLSTransport) and the identity signer
// (llmproxy.SignIdentityHeaders) look like the obvious contents and are
// deliberately absent: they already have THREE callers — the /llm proxy, the
// DeepWiki facade, and the provider connection check — so they are already
// extracted, into llmproxy. Moving them here would rename an import across
// three packages and change nothing. ADR-0012's plan called this package's
// subject "the ReverseProxy assembly, not the crypto", and this is what
// survives once the crypto is excluded.
//
// PROMOTION TRIGGER: a second Go MODULE needing these. Today every caller is
// inside elitea-main, so this stays internal.
package hop

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrInvalidTarget reports a target URL a hop cannot be built from.
var ErrInvalidTarget = errors.New("invalid provider target URL")

// TargetOptions describes what a particular hop requires of its peer's URL.
type TargetOptions struct {
	// EnvName is the setting an operator would edit. It is in the message
	// because "invalid target url" tells an operator nothing about which of
	// the twenty-odd variables to look at.
	EnvName string

	// RequireTLS refuses a non-https target.
	//
	// The two hops differ here ON PURPOSE and the difference is not an
	// oversight to be unified away. The DeepWiki provider REFUSES non-mTLS
	// traffic, so a plain-HTTP base URL there produces a facade that 502s on
	// every call and is worth catching at startup. The /llm gateway can be
	// reached over plain HTTP in a development stack, and refusing that would
	// break a supported configuration.
	RequireTLS bool
}

// ParseTarget validates a hop's peer URL.
//
// Both hops did this inline and neither said which setting was wrong. The
// message names it.
func ParseTarget(raw string, opts TargetOptions) (*url.URL, error) {
	setting := opts.EnvName
	if setting == "" {
		setting = "the provider target URL"
	}

	target, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s could not be parsed: %w", ErrInvalidTarget, setting, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("%w: %s must be an absolute URL with a scheme and host, got %q",
			ErrInvalidTarget, setting, raw)
	}
	if opts.RequireTLS && target.Scheme != "https" {
		return nil, fmt.Errorf("%w: %s must be https, got %q", ErrInvalidTarget, setting, raw)
	}
	return target, nil
}

// ParseGRPCTarget validates a provider's gRPC target: a host:port with both
// halves, which is what the provider service listens on (#1359). A URL is
// refused with the setting named — it is what the retired BASE_URL held,
// and the likeliest thing an operator carries over.
func ParseGRPCTarget(raw, envName string) error {
	setting := envName
	if setting == "" {
		setting = "the provider target"
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil || host == "" || port == "" || strings.Contains(raw, "/") {
		return fmt.Errorf("%w: %s must be a host:port (the provider's gRPC listener), got %q",
			ErrInvalidTarget, setting, raw)
	}
	return nil
}

// ClearWriteDeadline removes the per-connection write deadline before a hop
// streams a response.
//
// WHY EVERY HOP NEEDS IT. http.Server's WriteTimeout, when set, kills a
// long-lived response mid-stream. A generation's result and an SSE stream are
// both long-lived, and the failure is a truncated body with no error anywhere:
// the client sees a stream that simply stops.
//
// TODAY THIS IS LATENT, NOT LIVE, and it is called anyway. cmd/elitea-main's
// server sets ReadHeaderTimeout and IdleTimeout and NO WriteTimeout, so
// nothing currently kills a long response. The /llm hop has cleared the
// deadline since it was written; the DeepWiki hop never did. That difference
// is invisible until someone adds a WriteTimeout — a change that would look
// entirely reasonable, and would break one hop and not the other. Calling this
// from both is what makes them fail or survive together.
//
// A ResponseWriter that does not support deadlines (httptest.ResponseRecorder
// does not) is not an error: the deadline it cannot set is one it does not
// have.
func ClearWriteDeadline(w http.ResponseWriter, logger *slog.Logger) {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Time{}); err != nil &&
		!errors.Is(err, http.ErrNotSupported) {
		if logger != nil {
			logger.Warn("provider hop: clear write deadline", "error", err)
		}
	}
}
