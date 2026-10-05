// Package browserflow defines provider-neutral browser authentication values.
package browserflow

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	CurrentTransactionSchemaVersion = 1
	TransactionIDRandomBytes        = 32
	MaxProviderBytes                = 64
	MaxProviderReferenceBytes       = 768
	MaxEmailBytes                   = 1024
	MaxNameClaimBytes               = 2048
	MaxOpaqueIDBytes                = 512
	MaxReturnTargetBytes            = 4096
	MaxCorrelationValueBytes        = 2048
	MaxTransactionLifetime          = 15 * time.Minute
	MinPKCEVerifierBytes            = 43
	MaxPKCEVerifierBytes            = 128
)

var (
	ErrInvalidValue        = errors.New("invalid browser authentication value")
	ErrTransactionRejected = errors.New("browser authentication transaction rejected")
)

// ProtocolCorrelation contains response-bound protocol values. TransactionID
// itself is the OAuth state, SAML RelayState, or form CSRF value. Provider
// adapters return only a verified nonce or request ID here; server-only token-
// exchange material is held separately in ProviderState.
type ProtocolCorrelation struct {
	Nonce     string `json:"nonce,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// ProviderState is server-only verification material. It is returned only to
// the trusted protocol verifier after one-time transaction consumption and is
// never copied into VerifiedAssertion or the browser session. Raw tokens, SAML
// assertions, passwords, and provider credentials do not belong here.
type ProviderState struct {
	PKCEVerifier string `json:"pkce_verifier,omitempty"`
}

// VerificationContext is the bounded, server-only input passed to a trusted
// Form, OIDC, or SAML assertion verifier after the transaction has been
// consumed. OriginatingSessionID is copied from that consumed transaction; the
// context is neither serialized nor persisted as provider state.
type VerificationContext struct {
	Provider             string              `json:"-"`
	OriginatingSessionID string              `json:"-"`
	Correlation          ProtocolCorrelation `json:"-"`
	ProviderState        ProviderState       `json:"-"`
}

// Transaction is stored server-side and consumed exactly once. The store must
// atomically bind consumption to Provider and OriginatingSessionID.
type Transaction struct {
	SchemaVersion        int                 `json:"schema_version"`
	Provider             string              `json:"provider"`
	OriginatingSessionID string              `json:"originating_session_id"`
	ReturnTarget         string              `json:"return_target"`
	CreatedAt            time.Time           `json:"created_at"`
	ExpiresAt            time.Time           `json:"expires_at"`
	Correlation          ProtocolCorrelation `json:"correlation"`
	ProviderState        ProviderState       `json:"provider_state"`
}

// VerifiedAssertion is the provider-neutral output of a successful Form,
// OIDC, or SAML protocol adapter. ProviderAttributes must be a bounded JSON
// object; the browser session schema performs that structural validation. It
// must contain only the minimal claims needed after login, such as a SAML
// session index or an explicitly required bounded OIDC logout hint. Raw SAML
// assertions, PKCE verifiers, passwords, access/refresh tokens, and provider
// credentials never belong here.
type VerifiedAssertion struct {
	Provider            string              `json:"provider"`
	ProviderReference   string              `json:"provider_reference"`
	Email               string              `json:"email,omitempty"`
	GivenName           string              `json:"given_name,omitempty"`
	FamilyName          string              `json:"family_name,omitempty"`
	Name                string              `json:"name,omitempty"`
	ProviderAttributes  json.RawMessage     `json:"provider_attributes"`
	Expiration          *time.Time          `json:"expiration,omitempty"`
	ProtocolCorrelation ProtocolCorrelation `json:"protocol_correlation"`
}

func ValidateProvider(provider string) error {
	if !validRequiredText(provider, MaxProviderBytes) || strings.ContainsFunc(provider, unicode.IsSpace) {
		return ErrInvalidValue
	}
	return nil
}

func ValidateOpaqueID(id string) error {
	if !validRequiredText(id, MaxOpaqueIDBytes) || strings.ContainsFunc(id, unicode.IsSpace) {
		return ErrInvalidValue
	}
	return nil
}

// NewTransactionID returns a canonical unpadded base64url encoding of 256
// random bits. Transaction stores use this contract when creating keys.
func NewTransactionID() (string, error) {
	random := make([]byte, TransactionIDRandomBytes)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}

func ValidateTransactionID(id string) error {
	decoded, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || len(decoded) != TransactionIDRandomBytes ||
		base64.RawURLEncoding.EncodeToString(decoded) != id {
		return ErrInvalidValue
	}
	return nil
}

// CanonicalReturnTarget accepts only a same-origin absolute path and returns
// the parsed, canonical value that is safe to pass to an HTTP redirect.
func CanonicalReturnTarget(target string) (string, error) {
	if len(target) == 0 || len(target) > MaxReturnTargetBytes || !utf8.ValidString(target) ||
		strings.ContainsFunc(target, unicode.IsControl) || target[0] != '/' ||
		(len(target) > 1 && (target[1] == '/' || target[1] == '\\')) ||
		strings.ContainsRune(target, '\\') {
		return "", ErrInvalidValue
	}
	decoded, err := url.PathUnescape(target)
	if err != nil || len(decoded) == 0 || !utf8.ValidString(decoded) ||
		strings.ContainsFunc(decoded, unicode.IsControl) ||
		decoded[0] != '/' || (len(decoded) > 1 && (decoded[1] == '/' || decoded[1] == '\\')) ||
		strings.ContainsRune(decoded, '\\') {
		return "", ErrInvalidValue
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.IsAbs() || parsed.Hostname() != "" || parsed.Host != "" ||
		parsed.User != nil || parsed.Opaque != "" {
		return "", ErrInvalidValue
	}
	if _, err := url.ParseRequestURI(target); err != nil {
		return "", ErrInvalidValue
	}
	return parsed.String(), nil
}

// ValidateReturnTarget accepts only a same-origin absolute path. Redirect
// scheme, host, and cookie policy remain responsibilities of the HTTP adapter.
func ValidateReturnTarget(target string) error {
	_, err := CanonicalReturnTarget(target)
	return err
}

func (c ProtocolCorrelation) Validate() error {
	if !validOptionalCorrelation(c.Nonce) || !validOptionalCorrelation(c.RequestID) {
		return ErrInvalidValue
	}
	return nil
}

func (c ProtocolCorrelation) Equal(other ProtocolCorrelation) bool {
	return c.Nonce == other.Nonce && c.RequestID == other.RequestID
}

func (s ProviderState) Validate() error {
	if s.PKCEVerifier == "" {
		return nil
	}
	return ValidatePKCEVerifier(s.PKCEVerifier)
}

// ValidatePKCEVerifier accepts an RFC 7636 §4.1 code verifier: 43 to 128
// characters from the unreserved set [A-Za-z0-9-._~]. The native token
// endpoint (ADR-0025) checks a client's verifier with the same rule this
// package applies to the verifier it sends an identity provider.
func ValidatePKCEVerifier(verifier string) error {
	if len(verifier) < MinPKCEVerifierBytes || len(verifier) > MaxPKCEVerifierBytes {
		return ErrInvalidValue
	}
	for index := range len(verifier) {
		if !isPKCEVerifierCharacter(verifier[index]) {
			return ErrInvalidValue
		}
	}
	return nil
}

func (t Transaction) Validate() error {
	if t.SchemaVersion != CurrentTransactionSchemaVersion ||
		ValidateProvider(t.Provider) != nil || ValidateOpaqueID(t.OriginatingSessionID) != nil ||
		ValidateReturnTarget(t.ReturnTarget) != nil || t.Correlation.Validate() != nil ||
		t.ProviderState.Validate() != nil ||
		t.CreatedAt.IsZero() || t.ExpiresAt.IsZero() || !t.ExpiresAt.After(t.CreatedAt) ||
		t.CreatedAt.Location() != time.UTC || t.ExpiresAt.Location() != time.UTC ||
		t.ExpiresAt.Sub(t.CreatedAt) > MaxTransactionLifetime {
		return ErrInvalidValue
	}
	return nil
}

func (t Transaction) ActiveAt(now time.Time) bool {
	return !now.IsZero() && !t.CreatedAt.After(now) && now.Before(t.ExpiresAt)
}

func (a VerifiedAssertion) Validate() error {
	if ValidateProvider(a.Provider) != nil ||
		!validRequiredText(a.ProviderReference, MaxProviderReferenceBytes) ||
		!validOptionalText(a.Email, MaxEmailBytes) || strings.ContainsFunc(a.Email, unicode.IsSpace) ||
		!validOptionalText(a.GivenName, MaxNameClaimBytes) ||
		!validOptionalText(a.FamilyName, MaxNameClaimBytes) ||
		!validOptionalText(a.Name, MaxNameClaimBytes) ||
		a.ProtocolCorrelation.Validate() != nil ||
		(a.Expiration != nil && a.Expiration.IsZero()) {
		return ErrInvalidValue
	}
	return nil
}

func validOptionalCorrelation(value string) bool {
	return value == "" || (validRequiredText(value, MaxCorrelationValueBytes) &&
		!strings.ContainsFunc(value, unicode.IsSpace))
}

func validOptionalText(value string, maxBytes int) bool {
	return value == "" || validRequiredText(value, maxBytes)
}

func validRequiredText(value string, maxBytes int) bool {
	return len(value) <= maxBytes && utf8.ValidString(value) &&
		strings.TrimSpace(value) != "" && !strings.ContainsFunc(value, unicode.IsControl)
}

func isPKCEVerifierCharacter(character byte) bool {
	return character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9' ||
		character == '-' || character == '.' || character == '_' || character == '~'
}
