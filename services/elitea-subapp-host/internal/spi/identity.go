package spi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

// The identity headers the facade signs on every hop (ADR-0022 decision 5),
// and the canonical string both sides HMAC. The Go signer in elitea-main
// (internal/llmproxy) and the gateway's verifier spell this the same way; a
// fourth spelling here is the risk ADR-0023 names, so the shape is pinned by
// a test against a vector the Python shell produced.
const (
	HeaderProjectID   = "X-Elitea-Project-Id"
	HeaderUserID      = "X-Elitea-User-Id"
	HeaderTenantID    = "X-Elitea-Tenant-Id"
	HeaderExecutionID = "X-Elitea-Execution-Id"
	HeaderSignature   = "X-Elitea-Identity-Signature"

	signatureV1 = "v1"
	signatureV2 = "v2"
)

// IdentityHeaders is the closed list a request's caller may not supply: the
// gate strips every one before the handler runs, and re-derives the identity
// from the signature.
var IdentityHeaders = []string{HeaderProjectID, HeaderUserID, HeaderTenantID, HeaderExecutionID, HeaderSignature}

// Identity is who the facade says is calling.
type Identity struct {
	ProjectID   string
	UserID      string
	TenantID    string
	ExecutionID string
}

// IsEmpty reports an identity that names nobody.
func (i Identity) IsEmpty() bool { return i.ProjectID == "" && i.UserID == "" && i.TenantID == "" }

func (i Identity) canonical(version string) string {
	base := version + "\n" + i.ProjectID + "\n" + i.UserID + "\n" + i.TenantID
	if version == signatureV2 {
		return base + "\n" + i.ExecutionID
	}
	return base
}

func (i Identity) version() string {
	if i.ExecutionID != "" {
		return signatureV2
	}
	return signatureV1
}

// Sign returns the "sha256=<hex>" signature over the canonical string.
func (i Identity) Sign(secret []byte) string { return i.sign(secret, i.version()) }

func (i Identity) sign(secret []byte, version string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(i.canonical(version)))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// IdentityFromHeaders reads the four identity headers, unverified.
func IdentityFromHeaders(h http.Header) Identity {
	return Identity{
		ProjectID:   h.Get(HeaderProjectID),
		UserID:      h.Get(HeaderUserID),
		TenantID:    h.Get(HeaderTenantID),
		ExecutionID: h.Get(HeaderExecutionID),
	}
}

// VerifySignature reports whether the headers carry a signature the secret
// produced. It fails closed: with no secret there is nothing to verify
// against, so no header set verifies (#2959). v2 (with an execution id) is
// tried first; v1 is accepted only for a request that carries no execution
// id, so an execution id can never be appended to a v1 signature after the
// fact.
func VerifySignature(h http.Header, secret []byte) bool {
	if len(secret) == 0 {
		return false
	}
	got := h.Get(HeaderSignature)
	if got == "" {
		return false
	}
	identity := IdentityFromHeaders(h)
	if hmac.Equal([]byte(got), []byte(identity.sign(secret, signatureV2))) {
		return true
	}
	if identity.ExecutionID != "" {
		return false
	}
	return hmac.Equal([]byte(got), []byte(identity.sign(secret, signatureV1)))
}

// SignHeaders sets the identity headers and the signature on h — what a
// facade does; here for tests and for a host that fronts another provider.
func SignHeaders(h http.Header, identity Identity, secret []byte) {
	set := func(name, value string) {
		if value != "" {
			h.Set(name, value)
		}
	}
	set(HeaderProjectID, identity.ProjectID)
	set(HeaderUserID, identity.UserID)
	set(HeaderTenantID, identity.TenantID)
	set(HeaderExecutionID, identity.ExecutionID)
	if len(secret) > 0 {
		h.Set(HeaderSignature, identity.Sign(secret))
	}
}

// StripIdentityHeaders removes every identity header a caller presented.
func StripIdentityHeaders(h http.Header) {
	for _, name := range IdentityHeaders {
		h.Del(name)
	}
}
