// Package authlib provides shared authentication primitives for Elitea services.
package authlib

import (
	"crypto/subtle"
	"errors"
)

// ErrInvalidToken is returned when a token cannot be verified.
var ErrInvalidToken = errors.New("authlib: invalid token")

// Principal represents an authenticated identity (user or service account).
type Principal struct {
	ID       string
	Email    string
	Roles    []string
	TenantID string
}

// VerifyAPIKey performs a constant-time comparison of the provided key against
// the expected value to prevent timing attacks.
// TODO: replace with full JWT verification against the auth service's JWKS.
func VerifyAPIKey(provided, expected string) error {
	if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		return ErrInvalidToken
	}
	return nil
}
