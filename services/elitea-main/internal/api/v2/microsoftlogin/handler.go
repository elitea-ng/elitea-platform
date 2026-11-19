// Package microsoftlogin serves the Microsoft sign-in routes of ADR-0057 decision 6
// (epic 2254, story S1c):
//
//	POST /api/v2/microsoft_login:start     the starter, authenticated
//	GET  /api/v2/microsoft_login:callback  Entra's redirect; the one https URI
//	POST /api/v2/microsoft_login:redeem    the starter again, authenticated
//
// Start and redeem take the credential's update permission in the body's
// project_id. The callback has no session by design: on desktop and mobile it runs
// in the system browser, which holds none, and on web in a popup that may belong to
// a victim. It therefore attaches nothing to anyone (internal/microsoftlogin); it
// hands the client a one-time handoff code, and only the starter can redeem it.
//
// The callback's page for a web popup posts {type, state, handoff | error} to the
// opener (targeted at the deployment origin) and on a same-origin BroadcastChannel
// (an Entra page that severs window.opener cannot lose the message). For desktop and
// mobile it redirects to the loopback listener or the app's custom scheme. A code,
// token or secret is never in a URL, a page or an error.
package microsoftlogin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftlogin"
)

// Paths, relative to /api/v2 (the callback is mounted at the root router with its
// full path: it is public).
const (
	StartPath    = "/microsoft_login:start"
	RedeemPath   = "/microsoft_login:redeem"
	CallbackPath = "/api/v2/microsoft_login:callback"
)

// MessageType is the `type` of the web popup's message and broadcast.
const MessageType = "elitea:microsoft-login"

// ChannelName is the BroadcastChannel the callback page also posts on.
const ChannelName = "elitea-microsoft-login"

const maxBody = 4 << 10

// Flow is the sign-in service (*microsoftlogin.Service).
type Flow interface {
	Start(ctx context.Context, r microsoftlogin.StartRequest) (microsoftlogin.StartResult, error)
	Callback(ctx context.Context, p microsoftlogin.CallbackParams) microsoftlogin.Outcome
	Redeem(ctx context.Context, r microsoftlogin.RedeemRequest) (microsoftlogin.Redeemed, error)
}

// Handler serves the routes. A nil flow answers 503.
type Handler struct {
	flow   Flow
	origin string
	logger *slog.Logger
}

// New builds the handler. origin is the deployment's public origin: the target of
// the popup's postMessage.
func New(flow Flow, origin string, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	if parsed, err := url.Parse(origin); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		origin = parsed.Scheme + "://" + parsed.Host
	}
	return &Handler{flow: flow, origin: origin, logger: logger}
}

type startBody struct {
	ProjectID       int64  `json:"project_id"`
	CredentialID    string `json:"credential_id"`
	ClientKind      string `json:"client_kind"`
	ClientChallenge string `json:"client_challenge"`
	LoopbackPort    int    `json:"loopback_port"`
	RedirectScheme  string `json:"redirect_scheme"`
}

type redeemBody struct {
	ProjectID      int64  `json:"project_id"`
	Handoff        string `json:"handoff"`
	State          string `json:"state"`
	ClientVerifier string `json:"client_verifier"`
}

type bodyKey struct{}

// DecodeStart and DecodeRedeem read the request body once, keep it in the context and
// let the permission gate read the project from it (BodyProject).
var (
	DecodeStart  = decode[startBody](func(b startBody) int64 { return b.ProjectID })
	DecodeRedeem = decode[redeemBody](func(b redeemBody) int64 { return b.ProjectID })
)

func decode[T any](project func(T) int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
			if err != nil || len(raw) > maxBody {
				writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
				return
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			var body T
			if err := decoder.Decode(&body); err != nil || project(body) <= 0 {
				writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), bodyKey{}, body)))
		})
	}
}

// BodyProject is the permission gate's project: the decoded body's project_id.
func BodyProject(r *http.Request) (string, bool) {
	switch body := r.Context().Value(bodyKey{}).(type) {
	case startBody:
		return strconv.FormatInt(body.ProjectID, 10), body.ProjectID > 0
	case redeemBody:
		return strconv.FormatInt(body.ProjectID, 10), body.ProjectID > 0
	}
	return "", false
}

func caller(w http.ResponseWriter, r *http.Request) (int64, bool) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return 0, false
	}
	id, err := strconv.ParseInt(user.UserID, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusForbidden, "forbidden", "the caller has no resolved user id")
		return 0, false
	}
	return id, true
}

type startView struct {
	AuthorizationURL string `json:"authorization_url"`
	State            string `json:"state"`
	ExpiresAt        string `json:"expires_at"`
	App              string `json:"app"`
	HandoffTTL       int    `json:"handoff_ttl_seconds"`
	ChannelName      string `json:"channel_name"`
}

// Start serves POST /microsoft_login:start, behind Decode and the permission gate.
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.flow == nil {
		writeError(w, http.StatusServiceUnavailable, "not_available", "Microsoft sign-in is not available on this deployment")
		return
	}
	userID, ok := caller(w, r)
	if !ok {
		return
	}
	body, _ := r.Context().Value(bodyKey{}).(startBody)
	result, err := h.flow.Start(r.Context(), microsoftlogin.StartRequest{
		UserID: userID, ProjectID: body.ProjectID, CredentialID: body.CredentialID, ClientKind: body.ClientKind,
		ClientChallenge: body.ClientChallenge, LoopbackPort: body.LoopbackPort, ReturnScheme: body.RedirectScheme,
	})
	if err != nil {
		h.startFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, startView{
		AuthorizationURL: result.AuthorizationURL, State: result.State,
		ExpiresAt: result.ExpiresAt.UTC().Format(time.RFC3339), App: result.App,
		HandoffTTL: int(microsoftlogin.HandoffTTL.Seconds()), ChannelName: ChannelName,
	})
}

func (h *Handler) startFailure(w http.ResponseWriter, r *http.Request, err error) {
	var refusal *microsoftlogin.Refusal
	if !errors.As(err, &refusal) {
		h.logger.ErrorContext(r.Context(), "microsoft login: start failed")
		writeError(w, http.StatusInternalServerError, "internal_error", "Microsoft sign-in could not be started")
		return
	}
	status := http.StatusConflict
	switch refusal.Code {
	case microsoftlogin.CodeCredentialNotFound:
		status = http.StatusNotFound
	case microsoftlogin.CodeTooManySignIns:
		status = http.StatusTooManyRequests
	case microsoftlogin.CodeClientKindInvalid, microsoftlogin.CodeClientChallenge,
		microsoftlogin.CodeClientReturnInvalid, microsoftlogin.CodeCredentialIDInvalid:
		status = http.StatusBadRequest
	}
	writeError(w, status, refusal.Code, refusal.Message)
}

type redeemView struct {
	AccessToken    string   `json:"access_token"`
	TokenType      string   `json:"token_type"`
	ExpiresAt      string   `json:"expires_at"`
	ExpiresIn      int      `json:"expires_in"`
	Scope          []string `json:"scope"`
	CredentialID   string   `json:"credential_id"`
	CredentialType string   `json:"credential_type"`
	App            string   `json:"app"`
	TenantID       string   `json:"tenant_id"`
	AccountName    string   `json:"account_name"`
}

// Redeem serves POST /microsoft_login:redeem, behind Decode and the permission gate.
// Every refusal is the same 400: a caller learns nothing about why.
func (h *Handler) Redeem(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.flow == nil {
		writeError(w, http.StatusServiceUnavailable, "not_available", "Microsoft sign-in is not available on this deployment")
		return
	}
	userID, ok := caller(w, r)
	if !ok {
		return
	}
	body, _ := r.Context().Value(bodyKey{}).(redeemBody)
	redeemed, err := h.flow.Redeem(r.Context(), microsoftlogin.RedeemRequest{
		UserID: userID, ProjectID: body.ProjectID, Handoff: body.Handoff, State: body.State,
		ClientVerifier: body.ClientVerifier,
	})
	if errors.Is(err, microsoftlogin.ErrHandoffInvalid) {
		writeError(w, http.StatusBadRequest, "handoff_invalid", "The sign-in is not valid, has expired or was already used. Sign in again.")
		return
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "microsoft login: redeem failed")
		writeError(w, http.StatusInternalServerError, "internal_error", "Microsoft sign-in could not be completed")
		return
	}
	scope := redeemed.Scope
	if scope == nil {
		scope = []string{}
	}
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, redeemView{
		AccessToken: redeemed.AccessToken, TokenType: "Bearer", ExpiresAt: redeemed.ExpiresAt.UTC().Format(time.RFC3339),
		ExpiresIn: max(0, int(time.Until(redeemed.ExpiresAt).Seconds())), Scope: scope,
		CredentialID: redeemed.CredentialID, CredentialType: redeemed.CredentialType, App: redeemed.App,
		TenantID: redeemed.TenantID, AccountName: redeemed.AccountName,
	})
}

// Callback serves GET /api/v2/microsoft_login:callback. It is public.
func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	setPageHeaders(w)
	if h == nil || h.flow == nil {
		writePage(w, http.StatusServiceUnavailable, "", pageUnavailable)
		return
	}
	query := r.URL.Query()
	outcome := h.flow.Callback(r.Context(), microsoftlogin.CallbackParams{
		State: query.Get("state"), Code: query.Get("code"), Error: query.Get("error"),
	})
	if !outcome.Known {
		writePage(w, http.StatusBadRequest, "", pageInvalid)
		return
	}
	switch outcome.ClientKind {
	case microsoftlogin.ClientDesktop, microsoftlogin.ClientMobile:
		w.Header().Set("Location", outcome.ReturnURL())
		w.WriteHeader(http.StatusFound)
		return
	}
	h.writeWebPage(w, outcome)
}

func setPageHeaders(w http.ResponseWriter) {
	header := w.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("Pragma", "no-cache")
	header.Set("Referrer-Policy", "no-referrer")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}

// pageScriptHash lets the page's one script run under a CSP that allows nothing else.
func scriptHash(script string) string {
	sum := sha256.Sum256([]byte(script))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}
