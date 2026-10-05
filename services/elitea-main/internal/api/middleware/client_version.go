package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/clientversion"
)

// The minimum client version gate (ADR-0025 decision 5, WP4).
//
// WHO IS GATED. Only a caller authenticated by a NATIVE access token, which
// carries a registered client id (coordinator decision 13). A personal access
// token, an API key or a browser cookie is exempt whatever it sends: the web
// app ships with the server and is never "too old", and a script holding a PAT
// is not a native client this policy describes.
//
// WHAT IS COMPARED. The `X-Client-Version` header against the effective
// minimum for the caller's client — the higher of the `native_client_policy`
// section's `min_client_version` and the client's own registered minimum
// (internal/application/nativepolicy). A native request WITHOUT the header is
// passed: the header is how a client states its version, and the gate does
// not guess one.
//
// THE ANSWER. Below the minimum: `426 Upgrade Required` with
// `{"error":"client_upgrade_required","min_client_version":"x.y.z"}` and
// `X-Min-Client-Version`. RFC 9110 §15.5.22 says a 426 MUST carry `Upgrade:`
// naming a protocol; there is no protocol to name here — the upgrade is of the
// app, not the transport — so it is omitted, and v2.yaml says so. A header that
// does not parse is `400 invalid_client_version`.
//
// WHERE. Directly after the Maintenance gate in the API group (router.go): a
// 503 says "come back later", a 426 says "upgrade", and during a window the
// first is the true one. The native token endpoint is outside the group and
// applies the same rule inline (internal/api/nativeauth/token.go), so an
// outdated client cannot obtain fresh tokens either. `/revoke` is never gated:
// an outdated client must still be able to sign out and wipe.

// ClientVersionHeader is the header a native client states its version in.
const ClientVersionHeader = "X-Client-Version"

// MinClientVersionHeader carries the minimum on a 426.
const MinClientVersionHeader = "X-Min-Client-Version"

// ClientUpgradeRequiredError is the `error` of the 426 body.
const ClientUpgradeRequiredError = "client_upgrade_required"

// clientVersionExempt are path prefixes never gated, matched on segment
// boundaries. The revoke route is root-mounted outside the API group today;
// it is listed so a future move into the group cannot start refusing it.
var clientVersionExempt = []string{
	"/api/v2/auth/native/revoke",
}

// ClientVersionConfig wires the gate.
type ClientVersionConfig struct {
	// MinimumFor returns the effective minimum version for a registered
	// client id; "" means none. Nil disables the gate.
	MinimumFor func(ctx context.Context, clientID string) (string, error)
	// NativeClientForToken resolves the client of a bearer token when the
	// principal carries none — the principal was established from the edge's
	// forwarded identity, which has no client id, while the request still
	// presents the native token. ok is false for anything that is not a native
	// access token. Nil skips the fallback.
	NativeClientForToken func(ctx context.Context, token string) (clientID string, ok bool, err error)
}

// ClientVersionVerdict is the outcome of comparing one stated version.
type ClientVersionVerdict int

// The verdicts.
const (
	ClientVersionAccepted ClientVersionVerdict = iota
	ClientVersionMalformed
	ClientVersionTooOld
)

// EvaluateClientVersion compares a stated version with a minimum. An empty
// minimum accepts anything that parses; a minimum that does not parse (it was
// validated on save, so only a hand-written row) accepts everything rather than
// lock every client out.
func EvaluateClientVersion(version, minimum string) ClientVersionVerdict {
	if !clientversion.Valid(version) {
		return ClientVersionMalformed
	}
	below, err := clientversion.Below(version, minimum)
	if err != nil || !below {
		return ClientVersionAccepted
	}
	return ClientVersionTooOld
}

// ClientVersion returns the gate. A config without MinimumFor is a
// pass-through.
func ClientVersion(cfg ClientVersionConfig) func(http.Handler) http.Handler {
	if cfg.MinimumFor == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			version := r.Header.Get(ClientVersionHeader)
			if version == "" || pathHasSegmentPrefix(r.URL.Path, clientVersionExempt) {
				next.ServeHTTP(w, r)
				return
			}
			clientID := nativeClientOf(r, cfg)
			if clientID == "" {
				next.ServeHTTP(w, r)
				return
			}
			minimum, err := cfg.MinimumFor(r.Context(), clientID)
			if err != nil {
				// Fail OPEN: the gate is a compatibility promise, not an
				// access control, and a cold policy cache over a failing
				// database must not refuse every native request.
				slog.WarnContext(r.Context(), "client version gate: policy unreadable; request passed", "err", err)
				next.ServeHTTP(w, r)
				return
			}
			switch EvaluateClientVersion(version, minimum) {
			case ClientVersionMalformed:
				WriteInvalidClientVersion(w)
			case ClientVersionTooOld:
				WriteClientUpgradeRequired(w, minimum)
			default:
				next.ServeHTTP(w, r)
			}
		})
	}
}

// nativeClientOf is the caller's registered native client, "" for every other
// credential.
func nativeClientOf(r *http.Request, cfg ClientVersionConfig) string {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return ""
	}
	if user.NativeClientID != "" {
		return user.NativeClientID
	}
	if cfg.NativeClientForToken == nil {
		return ""
	}
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	token = strings.TrimSpace(token)
	if !found || token == "" {
		return ""
	}
	clientID, ok, err := cfg.NativeClientForToken(r.Context(), token)
	if err != nil {
		slog.WarnContext(r.Context(), "client version gate: native client lookup failed; request passed", "err", err)
		return ""
	}
	if !ok {
		return ""
	}
	return clientID
}

func pathHasSegmentPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// WriteClientUpgradeRequired answers 426. The native token endpoint writes the
// same answer.
func WriteClientUpgradeRequired(w http.ResponseWriter, minimum string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(MinClientVersionHeader, minimum)
	w.WriteHeader(http.StatusUpgradeRequired)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":              ClientUpgradeRequiredError,
		"error_description":  "This version of the app is no longer supported by this deployment. Update the app.",
		"min_client_version": minimum,
	})
}

// WriteInvalidClientVersion answers 400 for an X-Client-Version that does not
// parse.
func WriteInvalidClientVersion(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             "invalid_client_version",
		"error_description": ClientVersionHeader + " must be MAJOR.MINOR.PATCH with an optional -prerelease",
	})
}
