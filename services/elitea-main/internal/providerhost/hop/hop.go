// Package hop carries what every hop from elitea-main to a provider host does
// identically: validate the provider's gRPC target the same way (naming the
// setting an operator must fix), and clear the per-connection write deadline
// before a long-lived answer is written.
//
// WHAT IS NOT HERE. The mTLS client credentials (internal/infra/mtlsclient)
// and the identity signer (llmproxy.SignIdentityHeaders, carried as gRPC
// metadata by providerhost/proxy.SignedContext) have their own homes. The
// HTTP reverse-proxy assembly this package once served is gone: every
// provider hop is elitea.subapp.v1.ProviderService over gRPC (#1359).
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
	"strings"
	"time"
)

// ErrInvalidTarget reports a provider target a hop cannot be built from.
var ErrInvalidTarget = errors.New("invalid provider target")

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
// nothing currently kills a long response. Adding one would look entirely
// reasonable and would break whichever hop forgot to clear it; the /llm
// surface and the provider facades all call this so they fail or survive
// together.
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
