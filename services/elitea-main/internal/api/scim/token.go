package scim

// The OAuth 2.0 token endpoint for SCIM clients (RFC 6749 §4.4, client
// credentials grant). Microsoft Entra ID "OAuth2 client credentials grant"
// posts to it and then calls the SCIM tree with the access token.
//
//	POST /api/v2/scim/oauth/token
//	Content-Type: application/x-www-form-urlencoded
//
//	grant_type=client_credentials&client_id=scimc_...&client_secret=scimcs_...
//
// Client authentication is `client_secret_post` (the two values in the body)
// or `client_secret_basic` (HTTP Basic, RFC 6749 §2.3.1). A request that uses
// both is refused, as §2.3 requires. `scope` is ignored: a SCIM client has one
// scope, the SCIM tree, and refusing a value an identity provider adds on its
// own would break the integration for no gain.
//
// The route is PUBLIC: it is how a client without a session gets a
// credential. It is listed in internal/api/main_public_rules.go for that
// reason.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/scimclient"
)

// TokenPath is the absolute path of the token endpoint.
const TokenPath = "/api/v2/scim/oauth/token"

// maxTokenRequestBytes bounds the form body. A valid request is about 150
// bytes.
const maxTokenRequestBytes = 8 << 10

// The failure limit at the token endpoint: tokenFailureLimit failed client
// authentications per key per tokenFailureWindow.
const (
	tokenFailureLimit  = 20
	tokenFailureWindow = time.Minute
)

// TokenIssuer is the store seam for the token endpoint.
type TokenIssuer interface {
	IssueAccessToken(ctx context.Context, clientID, clientSecret string) (scimclient.AccessToken, scimclient.Principal, error)
}

// ClientKeyResolver resolves the caller's address through the configured
// trusted proxies. browserauth.TrustedProxyResolver implements it; it is the
// same interpretation the sign-in attempt limiter uses, so X-Forwarded-For
// from an untrusted peer is never believed.
type ClientKeyResolver interface {
	ResolveClientKey(*http.Request) (string, error)
}

// TokenHandler serves the token endpoint.
type TokenHandler struct {
	issuer   TokenIssuer
	resolver ClientKeyResolver
	limiter  *scimclient.FailureLimiter
}

// NewTokenHandler builds the handler with the default failure limiter.
// resolver may be nil: the socket peer is then the caller's address.
func NewTokenHandler(issuer TokenIssuer, resolver ClientKeyResolver) *TokenHandler {
	return &TokenHandler{
		issuer:   issuer,
		resolver: resolver,
		limiter:  scimclient.NewFailureLimiter(tokenFailureLimit, tokenFailureWindow),
	}
}

// ServeHTTP answers one token request.
func (h *TokenHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.issuer == nil {
		writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable",
			"SCIM provisioning is not available on this deployment", false)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request",
			"the request body must be application/x-www-form-urlencoded", false)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenRequestBytes)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the request body could not be read", false)
		return
	}
	form := r.PostForm

	// RFC 6749 §3.2: a parameter sent more than once is invalid.
	for _, name := range []string{"grant_type", "client_id", "client_secret", "scope"} {
		if len(form[name]) > 1 {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", name+" is repeated", false)
			return
		}
	}
	grantType := form.Get("grant_type")
	if grantType == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "grant_type is required", false)
		return
	}
	if grantType != "client_credentials" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"only the client_credentials grant is supported", false)
		return
	}

	clientID, clientSecret, usedBasic, ok := clientCredentials(r, form)
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request",
			"use exactly one client authentication method", false)
		return
	}
	if clientID == "" || clientSecret == "" {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed", usedBasic)
		return
	}

	// The failure limit is keyed per (client id, caller address) and is
	// checked BEFORE the secret is verified, so a blocked caller costs no
	// database read. The caller address is part of the key, so an attacker
	// who knows the public client id can block only their own address: the
	// identity provider, calling from another address, is never blocked by
	// failures it did not make. A refusal writes no log line and no audit row.
	key := h.limiterKey(r, clientID)
	if blocked, retry := h.limiter.Blocked(key); blocked {
		writeTooManyFailures(w, retry)
		return
	}
	token, principal, err := h.issuer.IssueAccessToken(r.Context(), clientID, clientSecret)
	if errors.Is(err, scimclient.ErrRejected) {
		h.limiter.Fail(key)
		if blocked, retry := h.limiter.Blocked(key); blocked {
			writeTooManyFailures(w, retry)
			return
		}
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed", usedBasic)
		return
	}
	if err != nil {
		slog.Error("SCIM: access token issue failed", "err", err)
		writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable",
			"the token could not be issued; retry later", false)
		return
	}
	audit.Annotate(r.Context(), audit.Annotation{Actor: principal.ActorLabel()})

	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": token.Token,
		"token_type":   "Bearer",
		"expires_in":   int(token.ExpiresIn.Seconds()),
	})
}

// clientCredentials returns the client id and secret from exactly one of the
// two supported methods. ok is false when both methods are present.
func clientCredentials(r *http.Request, form url.Values) (clientID, clientSecret string, usedBasic, ok bool) {
	bodyID, bodySecret := form.Get("client_id"), form.Get("client_secret")
	header := r.Header.Get("Authorization")
	if header == "" {
		return bodyID, bodySecret, false, true
	}
	scheme, _, _ := strings.Cut(header, " ")
	if !strings.EqualFold(scheme, "Basic") {
		// Another scheme carries no client credential this endpoint reads.
		// It is not refused as "two methods": treat the body as the method.
		return bodyID, bodySecret, false, true
	}
	if bodySecret != "" {
		return "", "", true, false
	}
	user, password, parsed := r.BasicAuth()
	if !parsed {
		return "", "", true, true
	}
	// RFC 6749 §2.3.1: both values are form-urlencoded before Basic encoding.
	decodedUser, errUser := url.QueryUnescape(user)
	decodedPassword, errPassword := url.QueryUnescape(password)
	if errUser != nil || errPassword != nil {
		return "", "", true, true
	}
	if bodyID != "" && bodyID != decodedUser {
		return "", "", true, false
	}
	return decodedUser, decodedPassword, true, true
}

// limiterKey keys the failure counter by client id AND caller address. The
// address comes from the trusted-proxy resolver when one is configured and the
// socket peer is a trusted proxy; otherwise the socket peer IS the caller.
// An oversized client id is replaced by a fixed marker so it cannot grow the
// key space.
func (h *TokenHandler) limiterKey(r *http.Request, clientID string) string {
	address := ""
	if h.resolver != nil {
		if resolved, err := h.resolver.ResolveClientKey(r); err == nil {
			address = resolved
		}
	}
	if address == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		address = host
	}
	if len(clientID) > 128 {
		clientID = "<oversized>"
	}
	return "client:" + clientID + "|addr:" + address
}

// writeTooManyFailures answers a caller whose failure window is full.
func writeTooManyFailures(w http.ResponseWriter, retry time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
	writeOAuthError(w, http.StatusTooManyRequests, "invalid_request",
		"too many failed client authentications; retry later", false)
}

// writeOAuthError renders RFC 6749 §5.2. A failed Basic authentication carries
// WWW-Authenticate, as the section requires.
func writeOAuthError(w http.ResponseWriter, status int, code, description string, basic bool) {
	if basic && status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="SCIM", charset="UTF-8"`)
	}
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             code,
		"error_description": description,
	})
}
