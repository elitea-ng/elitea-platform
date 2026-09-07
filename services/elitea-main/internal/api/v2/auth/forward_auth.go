package auth

import (
	"errors"
	"net/http"
	"strings"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/forwardauth"
	identity "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

var traefikForwardHeaders = [...]string{
	"X-Forwarded-Method",
	"X-Forwarded-Proto",
	"X-Forwarded-Host",
	"X-Forwarded-Uri",
	"X-Forwarded-For",
}

// ForwardAuthCredentialHeader maps an additional request header to one of the
// credential handlers supported by the current baseline: "bearer" or "basic".
// Order is significant when a request contains more than one configured
// header, so callers must preserve configuration order.
type ForwardAuthCredentialHeader struct {
	Name           string
	CredentialType string
}

type ForwardAuthOption func(*ForwardAuthHandler)

// WithForwardAuthCredentialHeaders configures current-baseline
// other_auth_headers behavior. No additional credential header is trusted by
// default.
func WithForwardAuthCredentialHeaders(headers ...ForwardAuthCredentialHeader) ForwardAuthOption {
	configured := append([]ForwardAuthCredentialHeader(nil), headers...)
	return func(handler *ForwardAuthHandler) {
		handler.credentialHeaders = configured
	}
}

// ForwardAuthHandler implements Traefik's forward-auth protocol.
// Traefik sends the original request headers; this handler validates
// credentials and responds 200 on success or 403 on credential failure.
type ForwardAuthHandler struct {
	credentials       *forwardapp.TokenCredentialAuthenticator
	credentialHeaders []ForwardAuthCredentialHeader
}

// NewForwardAuthHandler takes exactly one token validator. It used to take a
// pylon Redis-RPC client as a fallback for a nil validator; #383 deleted that
// client, so a nil validator now means the handler authenticates nothing and
// refuses every credential.
func NewForwardAuthHandler(
	validator apimw.TokenValidator,
	opts ...ForwardAuthOption,
) *ForwardAuthHandler {
	var tokenValidator forwardapp.TokenValidator
	if validator != nil {
		tokenValidator = validator
	}
	credentials, _ := forwardapp.NewTokenCredentialAuthenticator(tokenValidator)
	handler := &ForwardAuthHandler{credentials: credentials}
	for _, opt := range opts {
		opt(handler)
	}
	return handler
}

func (h *ForwardAuthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")

	// The current baseline checks header presence to establish that the request
	// came through Traefik. It intentionally does not require non-empty values.
	for _, name := range traefikForwardHeaders {
		if _, ok := requestHeader(r.Header, name); !ok {
			writeAccessDenied(w)
			return
		}
	}

	// Authorization has precedence over every configured additional credential
	// header, including when its value is empty or malformed.
	if authorization, ok := requestHeader(r.Header, "Authorization"); ok {
		credentialType, credentialData, ok := parseAuthorization(authorization)
		if !ok {
			writeAccessDenied(w)
			return
		}
		h.authenticate(w, r, credentialType, credentialData)
		return
	}

	for _, configured := range h.credentialHeaders {
		credentialData, ok := requestHeader(r.Header, configured.Name)
		if !ok {
			continue
		}
		h.authenticate(w, r, configured.CredentialType, credentialData)
		return
	}

	writeAccessDenied(w)
}

func (h *ForwardAuthHandler) authenticate(
	w http.ResponseWriter,
	r *http.Request,
	credentialType string,
	credentialData string,
) {
	if h.credentials == nil {
		writeAccessDenied(w)
		return
	}
	result, err := h.credentials.AuthenticateCredential(r.Context(), forwardapp.Source{}, forwardapp.CredentialInput{
		Present: true,
		Type:    credentialType,
		Data:    credentialData,
	})
	if err != nil || result.Resolution != forwardapp.CredentialAccepted {
		writeAccessDenied(w)
		return
	}
	if err := writeSuccess(w, r, result.Principal); err != nil {
		writeAccessDenied(w)
	}
}

func writeSuccess(w http.ResponseWriter, r *http.Request, user identity.User) error {
	// The current baseline identifies a token by auth_core__token.id. The
	// additive owner header lets downstream code cross-check it without changing
	// the existing X-Auth-ID contract.
	if user.TokenID == "" || user.UserID == "" {
		return errors.New("validated token identity is incomplete")
	}

	targetValues, targetProvided := r.URL.Query()["target"]
	if !targetProvided {
		writeOK(w)
		return nil
	}
	target := ""
	if len(targetValues) > 0 {
		target = targetValues[0]
	}
	if target != "rpc" {
		return errors.New("forward-auth success target is not registered")
	}

	w.Header().Set("X-Auth-Type", "token")
	w.Header().Set("X-Auth-ID", user.TokenID)
	w.Header().Set("X-Auth-User-ID", user.UserID)
	w.Header().Set("X-Auth-Reference", "-")
	writeOK(w)
	return nil
}

func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func writeAccessDenied(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte("Access Denied"))
}

func parseAuthorization(value string) (string, string, bool) {
	separator := strings.IndexByte(value, ' ')
	if separator < 0 {
		return "", "", false
	}
	return strings.ToLower(value[:separator]), value[separator+1:], true
}

func requestHeader(headers http.Header, name string) (string, bool) {
	canonicalName := http.CanonicalHeaderKey(name)
	if values, ok := headers[canonicalName]; ok {
		if len(values) == 0 {
			return "", true
		}
		return values[0], true
	}
	// Incoming net/http requests use canonical keys. The fallback preserves
	// HTTP's case-insensitive semantics for directly constructed request maps.
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		if len(values) == 0 {
			return "", true
		}
		return values[0], true
	}
	return "", false
}
