package nativeauth

import (
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
)

// formSingle parses an RFC 6749 form body: urlencoded only, bounded, and no
// parameter repeated (§3.1). ok is false when an error was written.
func formSingle(w http.ResponseWriter, r *http.Request, names []string) (url.Values, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request",
			"the request body must be application/x-www-form-urlencoded")
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return nil, false
	}
	for _, name := range names {
		if len(r.PostForm[name]) > 1 {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", name+" is repeated")
			return nil, false
		}
	}
	return r.PostForm, true
}

var tokenParameters = []string{
	"grant_type", "code", "redirect_uri", "client_id", "code_verifier", "refresh_token", "client_version",
}

// token serves both grants. Public clients: no client authentication, and an
// Authorization header is refused rather than ignored.
func (h *Handler) token(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request",
			"a native client is public: send no Authorization header")
		return
	}
	form, ok := formSingle(w, r, tokenParameters)
	if !ok {
		return
	}
	grantType := form.Get("grant_type")
	if grantType == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "grant_type is required")
		return
	}
	if grantType != "authorization_code" && grantType != "refresh_token" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"only authorization_code and refresh_token are supported")
		return
	}
	clientID := form.Get("client_id")
	if clientID == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_id is required")
		return
	}

	address, known := h.address(r)
	keys := []string{clientAddressKey(clientID, address, known), "token|" + addressKey(address, known)}
	if known {
		if blocked, retry := h.tokenBlocked(keys); blocked {
			writeTooManyFailures(w, retry)
			return
		}
	}
	fail := func(status int, code, description string) {
		h.tokenFailures.Fail(keys[0])
		h.tokenAddressFailures.Fail(keys[1])
		if blocked, retry := h.tokenBlocked(keys); blocked {
			writeTooManyFailures(w, retry)
			return
		}
		writeOAuthError(w, status, code, description)
	}

	if grantType == "authorization_code" {
		h.exchangeCode(w, r, form, clientID, fail)
		return
	}
	h.refresh(w, r, form, clientID, fail)
}

func (h *Handler) tokenBlocked(keys []string) (bool, time.Duration) {
	if blocked, retry := h.tokenFailures.Blocked(keys[0]); blocked {
		return true, retry
	}
	return h.tokenAddressFailures.Blocked(keys[1])
}

func (h *Handler) exchangeCode(
	w http.ResponseWriter, r *http.Request, form url.Values, clientID string,
	fail func(int, string, string),
) {
	code, redirectURI, verifier := form.Get("code"), form.Get("redirect_uri"), form.Get("code_verifier")
	if code == "" || redirectURI == "" || verifier == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code, redirect_uri and code_verifier are required")
		return
	}
	if browserflow.ValidatePKCEVerifier(verifier) != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request",
			"code_verifier must be 43 to 128 unreserved characters")
		return
	}
	if _, active, err := h.cfg.Registry.Active(r.Context(), clientID); err != nil {
		h.unavailable(w, r, err)
		return
	} else if !active {
		fail(http.StatusUnauthorized, "invalid_client", "the client is not registered or is disabled")
		return
	}
	grant, replay, err := h.cfg.Store.ExchangeCode(r.Context(), domain.ExchangeRequest{
		Code: code, ClientID: clientID, RedirectURI: redirectURI, Verifier: verifier,
	})
	switch {
	case errors.Is(err, domain.ErrCodeReplay):
		if replay != nil {
			h.record(r.Context(), audit.Event{
				UserID: int64Ptr(replay.UserID), Action: "Native device revoked: authorization code replayed",
				HTTPRoute: TokenPath, StatusCode: int32Ptr(http.StatusBadRequest), IsError: true,
				EntityName: replay.DeviceName,
			})
		}
		fail(http.StatusBadRequest, "invalid_grant", "the authorization code is invalid or was already used")
		return
	case errors.Is(err, domain.ErrInvalidGrant):
		fail(http.StatusBadRequest, "invalid_grant", "the authorization code is invalid, expired or already used")
		return
	case err != nil:
		h.unavailable(w, r, err)
		return
	}
	h.record(r.Context(), audit.Event{
		UserID: int64Ptr(grant.UserID), UserEmail: grant.UserEmail,
		Action:    "Native device signed in: " + grant.ClientID + " on " + grant.DeviceName,
		HTTPRoute: TokenPath, StatusCode: int32Ptr(http.StatusOK), EntityName: grant.DeviceName,
	})
	h.writeGrant(w, r, grant)
}

func (h *Handler) refresh(
	w http.ResponseWriter, r *http.Request, form url.Values, clientID string,
	fail func(int, string, string),
) {
	refreshToken := form.Get("refresh_token")
	if refreshToken == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	clientVersion := form.Get("client_version")
	if !clientVersionPattern.MatchString(clientVersion) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_version is malformed")
		return
	}
	_, active, err := h.cfg.Registry.Active(r.Context(), clientID)
	if err != nil {
		h.unavailable(w, r, err)
		return
	}
	grant, outcome, err := h.cfg.Store.Refresh(r.Context(), domain.RefreshRequest{
		RefreshToken: refreshToken, ClientID: clientID, ClientVersion: clientVersion, ClientActive: active,
	})
	switch {
	case errors.Is(err, domain.ErrFamilyRevoked):
		if errors.Is(err, domain.ErrRefreshReuse) {
			h.record(r.Context(), audit.Event{
				UserID: int64Ptr(outcome.UserID), UserEmail: outcome.UserEmail,
				Action:    "Native device revoked: refresh token reused",
				HTTPRoute: TokenPath, StatusCode: int32Ptr(http.StatusUnauthorized), IsError: true,
				EntityName: outcome.DeviceName,
			})
		}
		// A revoked, expired, deactivated or reused family: the client wipes
		// (ADR-0025 decision 4). The same body the API answers.
		apimw.WriteDeviceRevoked(w)
		return
	case errors.Is(err, domain.ErrInvalidGrant):
		fail(http.StatusBadRequest, "invalid_grant", "the refresh token is invalid")
		return
	case err != nil:
		h.unavailable(w, r, err)
		return
	}
	h.writeGrant(w, r, grant)
}

func (h *Handler) writeGrant(w http.ResponseWriter, r *http.Request, grant domain.Grant) {
	body := map[string]any{
		"access_token":             grant.AccessToken,
		"token_type":               "Bearer",
		"expires_in":               int64(grant.AccessExpiresIn / time.Second),
		"refresh_token":            grant.RefreshToken,
		"refresh_token_expires_in": int64(grant.RefreshExpiresIn / time.Second),
		"device_id":                grant.DeviceID,
	}
	if h.cfg.Decorate != nil {
		if err := h.cfg.Decorate(r.Context(), grant.ClientID, body); err != nil {
			slog.ErrorContext(r.Context(), "native token: response decoration failed", "err", err)
		}
	}
	writeJSON(w, http.StatusOK, body)
}

func (h *Handler) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "native token endpoint: store did not answer", "err", err)
	w.Header().Set("Retry-After", "5")
	writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "retry later")
}

func writeTooManyFailures(w http.ResponseWriter, retry time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
	writeOAuthError(w, http.StatusTooManyRequests, "invalid_request", "too many failed requests; retry later")
}

// writeOAuthError renders RFC 6749 §5.2.
func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}

/* ── revoke (RFC 7009) ──────────────────────────────────────────────────── */

func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request",
			"a native client is public: send no Authorization header")
		return
	}
	address, known := h.address(r)
	key := "revoke|" + addressKey(address, known)
	if known {
		if blocked, retry := h.revocations.Blocked(key); blocked {
			writeTooManyFailures(w, retry)
			return
		}
	}
	h.revocations.Fail(key)
	form, ok := formSingle(w, r, []string{"token", "token_type_hint", "client_id"})
	if !ok {
		return
	}
	token, clientID := form.Get("token"), form.Get("client_id")
	if token == "" || clientID == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "token and client_id are required")
		return
	}
	if _, registered, err := h.cfg.Registry.Lookup(r.Context(), clientID); err != nil {
		h.unavailable(w, r, err)
		return
	} else if !registered {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "the client is not registered")
		return
	}
	revoked, err := h.cfg.Store.RevokeByToken(r.Context(), token, clientID)
	if err != nil {
		h.unavailable(w, r, err)
		return
	}
	if revoked != nil {
		h.record(r.Context(), audit.Event{
			UserID: int64Ptr(revoked.UserID), Action: "Native device signed out: " + revoked.ClientID +
				" on " + revoked.DeviceName,
			HTTPRoute: RevokePath, StatusCode: int32Ptr(http.StatusOK), EntityName: revoked.DeviceName,
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
}
