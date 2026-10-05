// Package nativeauth is the native authorization server for registered public
// clients (ADR-0025 decisions 3 and 4): the client registry, the authorization
// requests between /authorize and code exchange, the device registry (one row
// per refresh-token family) and the opaque access and refresh tokens.
//
// # Formats
//
// Every credential is 32 random bytes in unpadded base64url behind a prefix a
// secret scanner can match:
//
//	elnat_<43>  access token    (15 minutes by default)
//	elnrt_<43>  refresh token   (rotated on every use)
//	elnac_<43>  authorization code (60 seconds, single use)
//
// No prefix is a prefix of another, none collides with a personal access token
// (a JWT, `eyJ…`) or with the SCIM prefixes (`scim_`, `scimc_`, `scimcs_`,
// `scimat_`). The store keeps the hex SHA-256 of the whole string; a plain hash
// is enough because the input has 256 bits of entropy.
//
// # The anchor
//
// Each device session owns one public.auth_core__token row with uuid NULL. The
// validator returns that row's id as the principal's TokenID, so a native
// principal is field-for-field a personal-access-token principal and every
// downstream check (principal re-check, legacy RBAC, the edge kernel, the
// forwarded X-Auth-* contract, the ADR-0018 binding) accepts it unchanged.
// Revoking a family deletes the anchor, which is what cuts off an
// edge-validated (forwarded) native principal.
package nativeauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// The credential prefixes.
const (
	PrefixAccessToken  = "elnat_"
	PrefixRefreshToken = "elnrt_"
	PrefixCode         = "elnac_"
)

const secretBytes = 32

// Fixed lifetimes the ADR states.
const (
	// AuthorizationTTL is how long an authorization request may wait for the
	// browser sign-in and the consent decision.
	AuthorizationTTL = 10 * time.Minute
	// CodeTTL is the authorization code's lifetime (ADR-0025 decision 3).
	CodeTTL = 60 * time.Second
)

// The configurable lifetimes and their environment variables.
const (
	AccessTokenTTLEnv     = "ELITEA_NATIVE_ACCESS_TOKEN_TTL"
	DefaultAccessTokenTTL = 15 * time.Minute
	MinAccessTokenTTL     = 5 * time.Minute
	MaxAccessTokenTTL     = time.Hour

	RefreshIdleTTLEnv     = "ELITEA_NATIVE_REFRESH_IDLE_TTL"
	DefaultRefreshIdleTTL = 720 * time.Hour
	MinRefreshIdleTTL     = 24 * time.Hour
	MaxRefreshIdleTTL     = 2160 * time.Hour

	// SessionMaxLifetimeEnv is the optional absolute cap on a device session.
	// Zero (the default) means none.
	SessionMaxLifetimeEnv = "ELITEA_NATIVE_SESSION_MAX_LIFETIME"
	MinSessionMaxLifetime = 24 * time.Hour
	MaxSessionMaxLifetime = 8760 * time.Hour

	// RedeliveryWindowEnv is the refresh re-delivery grace window
	// (coordinator decision 7). Zero is strict rotation: any reuse revokes.
	RedeliveryWindowEnv     = "ELITEA_NATIVE_REFRESH_REDELIVERY_WINDOW"
	DefaultRedeliveryWindow = 30 * time.Second
	MaxRedeliveryWindow     = 5 * time.Minute
)

// Config is the token lifetime policy, read once at boot.
type Config struct {
	AccessTokenTTL time.Duration
	RefreshIdleTTL time.Duration
	// SessionMaxLifetime is the absolute cap; zero means none.
	SessionMaxLifetime time.Duration
	// RedeliveryWindow is how long the immediately-previous refresh token
	// still returns the SAME successor pair; zero is strict.
	RedeliveryWindow time.Duration
}

// DefaultConfig is the policy with no environment set.
func DefaultConfig() Config {
	return Config{
		AccessTokenTTL:   DefaultAccessTokenTTL,
		RefreshIdleTTL:   DefaultRefreshIdleTTL,
		RedeliveryWindow: DefaultRedeliveryWindow,
	}
}

// ConfigFromEnv reads the four variables. An empty value takes the default; a
// value that is not a Go duration, or is outside its bounds, is an error that
// names the variable, so the boot stops instead of running with a lifetime
// nobody chose (the scimclient.AccessTokenTTLFromEnv rule).
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := DefaultConfig()
	var err error
	if cfg.AccessTokenTTL, err = durationFromEnv(getenv, AccessTokenTTLEnv,
		DefaultAccessTokenTTL, MinAccessTokenTTL, MaxAccessTokenTTL, false); err != nil {
		return Config{}, err
	}
	if cfg.RefreshIdleTTL, err = durationFromEnv(getenv, RefreshIdleTTLEnv,
		DefaultRefreshIdleTTL, MinRefreshIdleTTL, MaxRefreshIdleTTL, false); err != nil {
		return Config{}, err
	}
	if cfg.SessionMaxLifetime, err = durationFromEnv(getenv, SessionMaxLifetimeEnv,
		0, MinSessionMaxLifetime, MaxSessionMaxLifetime, true); err != nil {
		return Config{}, err
	}
	if cfg.RedeliveryWindow, err = durationFromEnv(getenv, RedeliveryWindowEnv,
		DefaultRedeliveryWindow, time.Second, MaxRedeliveryWindow, true); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func durationFromEnv(
	getenv func(string) string, name string, fallback, minimum, maximum time.Duration, zeroAllowed bool,
) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration, for example 15m: %q", name, raw)
	}
	if value == 0 && zeroAllowed {
		return 0, nil
	}
	if value < minimum || value > maximum {
		if zeroAllowed {
			return 0, fmt.Errorf("%s must be 0 or between %s and %s: %q", name, minimum, maximum, raw)
		}
		return 0, fmt.Errorf("%s must be between %s and %s: %q", name, minimum, maximum, raw)
	}
	return value, nil
}

// newSecret returns prefix + base64url(32 random bytes).
func newSecret(prefix string) (string, error) {
	buffer := make([]byte, secretBytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("nativeauth: read random bytes: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buffer), nil
}

// NewOpaque returns base64url(32 random bytes) with no prefix: the authorize
// handle and the browser binder.
func NewOpaque() (string, error) { return newSecret("") }

// HashSecret is the hex SHA-256 of the whole credential string.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// WellFormed reports whether value is prefix + base64url of 32 bytes. It
// refuses a malformed value before any database read.
func WellFormed(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	return wellFormedOpaque(value[len(prefix):])
}

func wellFormedOpaque(body string) bool {
	if len(body) != base64.RawURLEncoding.EncodedLen(secretBytes) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(body)
	return err == nil && base64.RawURLEncoding.EncodeToString(decoded) == body
}

// WellFormedOpaque reports whether value is base64url of 32 bytes (a handle or
// a binder).
func WellFormedOpaque(value string) bool { return wellFormedOpaque(value) }
