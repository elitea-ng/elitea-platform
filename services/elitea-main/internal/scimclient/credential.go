// Package scimclient owns the dedicated SCIM client credentials (shared
// migration 0135).
//
// # Why SCIM has its own credential
//
// The SCIM tree used to accept a personal access token of an account that held
// `admin.auth.users`. That token carried the full rights of a person, every
// identity provider change was attributed to that person, it stopped working
// when the person left, and it could not be scoped or rotated per identity
// provider. A SCIM client is a credential that belongs to the INTEGRATION. It
// authenticates the SCIM tree and nothing else, and the audit trail records it
// as `scim:<client name>`.
//
// # Two methods
//
//   - bearer: the secret is the bearer token. Microsoft Entra ID calls this
//     "Bearer authentication" and takes it as the "Secret token".
//   - client_credentials: the identity provider posts `client_id` and
//     `client_secret` to the token endpoint (RFC 6749 §4.4) and receives an
//     access token that lives for AccessTokenTTL. Entra ID calls this "OAuth2
//     client credentials grant".
//
// # Formats
//
// Every secret and access token is 32 random bytes in base64url (no padding)
// behind a prefix a secret scanner can match:
//
//	scim_<43>    bearer secret
//	scimc_<22>   client identifier (public)
//	scimcs_<43>  client secret
//	scimat_<43>  access token
//
// The store keeps the hex SHA-256 of the whole string. A slow password hash is
// not needed: the input has 256 bits of entropy, so the hash cannot be reversed
// by search.
package scimclient

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The two authentication methods. They are the values of
// `elitea_auth.scim_clients.auth_method`.
const (
	MethodBearer            = "bearer"
	MethodClientCredentials = "client_credentials"
)

// The credential prefixes. "scim_" is not a prefix of the other three: the
// fifth character of each of them is a letter, not an underscore.
const (
	PrefixBearerSecret = "scim_"
	PrefixClientID     = "scimc_"
	PrefixClientSecret = "scimcs_"
	PrefixAccessToken  = "scimat_"
)

// The access token lifetime and its bounds.
const (
	// AccessTokenTTLEnv names the environment variable that sets the access
	// token lifetime. It is a Go duration, for example "1h".
	AccessTokenTTLEnv     = "ELITEA_SCIM_ACCESS_TOKEN_TTL"
	DefaultAccessTokenTTL = time.Hour
	MinAccessTokenTTL     = 5 * time.Minute
	MaxAccessTokenTTL     = 24 * time.Hour
)

const (
	secretBytes   = 32
	clientIDBytes = 16
	hintLength    = 4
	// MaxNameLength is the longest client name the store accepts. The
	// migration's CHECK states the same bound.
	MaxNameLength = 100
)

// MaxClientLifetime is the furthest expiry a client may be created with. An
// operator who wants a longer-lived credential rotates it instead; a credential
// with no end date is still possible by leaving the expiry empty.
const MaxClientLifetime = 2 * 365 * 24 * time.Hour

// The errors the store returns. Callers map them to HTTP answers; none of them
// carries a database cause across a trust boundary.
var (
	// ErrRejected means the presented credential does not authenticate. It is
	// one error for "unknown", "wrong secret", "revoked" and "expired", so a
	// caller cannot learn which one applied.
	ErrRejected = errors.New("scim client credential rejected")
	// ErrNotFound means no client has that id.
	ErrNotFound = errors.New("scim client not found")
	// ErrDuplicateName means another client already has that name.
	ErrDuplicateName = errors.New("a SCIM client with that name already exists")
	// ErrInvalidName and ErrInvalidMethod refuse a bad create request.
	ErrInvalidName   = errors.New("the name must be between 1 and 100 characters")
	ErrInvalidMethod = errors.New("the authentication method must be bearer or client_credentials")
	// ErrRevoked means the operation needs an active client.
	ErrRevoked = errors.New("the SCIM client is revoked")
	// ErrInvalidExpiry refuses an expiry in the past or too far ahead.
	ErrInvalidExpiry = errors.New("the expiry must be in the future and at most two years away")
)

// AccessTokenTTLFromEnv reads AccessTokenTTLEnv. An empty value gives
// DefaultAccessTokenTTL. A value that is not a duration, or that is outside
// [MinAccessTokenTTL, MaxAccessTokenTTL], is an error that names the variable,
// so the boot stops instead of running with a lifetime nobody chose.
func AccessTokenTTLFromEnv(getenv func(string) string) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(AccessTokenTTLEnv))
	if raw == "" {
		return DefaultAccessTokenTTL, nil
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration, for example 1h: %q", AccessTokenTTLEnv, raw)
	}
	if ttl < MinAccessTokenTTL || ttl > MaxAccessTokenTTL {
		return 0, fmt.Errorf("%s must be between %s and %s: %q",
			AccessTokenTTLEnv, MinAccessTokenTTL, MaxAccessTokenTTL, raw)
	}
	return ttl, nil
}

// randomToken returns prefix + base64url(n random bytes).
func randomToken(prefix string, n int) (string, error) {
	buffer := make([]byte, n)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buffer), nil
}

// HashSecret returns the hex SHA-256 of the full credential string.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// hashesEqual compares two hex hashes in constant time.
func hashesEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// secretHint returns the last four characters, which the admin screen shows so
// an operator can match a secret to the row it belongs to.
func secretHint(secret string) string {
	if len(secret) <= hintLength {
		return secret
	}
	return secret[len(secret)-hintLength:]
}

// wellFormed reports whether value is prefix + base64url of the expected length.
// It refuses a malformed value before any database read.
func wellFormed(value, prefix string, n int) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	body := value[len(prefix):]
	if len(body) != base64.RawURLEncoding.EncodedLen(n) {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(body)
	return err == nil
}

// normalizeName trims the name and checks its length in characters.
func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	length := len([]rune(name))
	if length < 1 || length > MaxNameLength {
		return "", ErrInvalidName
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", ErrInvalidName
		}
	}
	return name, nil
}

// Principal is the identity a SCIM request runs as. It is a CLIENT, not a user,
// and it carries no user id on purpose.
type Principal struct {
	ID     int64
	Name   string
	Method string
}

// ActorLabel is what the audit trail records as the actor.
func (p Principal) ActorLabel() string { return "scim:" + p.Name }
