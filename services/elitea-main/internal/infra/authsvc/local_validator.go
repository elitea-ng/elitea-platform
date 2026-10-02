package authsvc

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

var (
	// ErrTokenRejected identifies a syntactically invalid, expired, revoked, or
	// otherwise unknown caller credential. It is safe to map to an ordinary
	// authentication denial without exposing the underlying reason.
	ErrTokenRejected = auth.ErrCredentialRejected
	// ErrTokenValidationUnavailable identifies configuration, storage, or data
	// integrity failures. EdgeAuth must fail closed without misreporting an
	// infrastructure outage as an ordinary bad credential.
	ErrTokenValidationUnavailable = auth.ErrCredentialValidationUnavailable
)

type activePATQueries interface {
	GetActivePATPrincipalByUUID(context.Context, string) (sqlcgen.GetActivePATPrincipalByUUIDRow, error)
}

type LocalValidator struct {
	queries   activePATQueries
	secretKey []byte
}

func NewLocalValidator(pool *pgxpool.Pool, secretKey string) *LocalValidator {
	return NewLocalValidatorBytes(pool, []byte(secretKey))
}

// NewLocalValidatorBytes snapshots the exact HS512 key bytes without forcing
// production composition to create an additional immutable secret string.
// Existing Python-issued PAT compatibility depends on preserving every byte.
func NewLocalValidatorBytes(pool *pgxpool.Pool, secretKey []byte) *LocalValidator {
	validator := &LocalValidator{secretKey: append([]byte(nil), secretKey...)}
	if pool != nil {
		validator.queries = sqlcgen.New(pool)
	}
	return validator
}

type tokenClaims struct {
	UUID    string  `json:"uuid"`
	Expires *string `json:"expires"`
	jwt.RegisteredClaims
}

func (v *LocalValidator) ValidateToken(ctx context.Context, tokenStr string) (auth.User, error) {
	if v == nil || len(v.secretKey) == 0 {
		return auth.User{}, fmt.Errorf("%w: token signing key is not configured", ErrTokenValidationUnavailable)
	}
	token, err := jwt.ParseWithClaims(tokenStr, &tokenClaims{}, func(t *jwt.Token) (interface{}, error) {
		if t.Method.Alg() != jwt.SigningMethodHS512.Alg() {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return v.secretKey, nil
	})
	if err != nil {
		return auth.User{}, fmt.Errorf("%w: invalid token: %v", ErrTokenRejected, err)
	}

	claims, ok := token.Claims.(*tokenClaims)
	if !ok || claims.UUID == "" {
		return auth.User{}, fmt.Errorf("%w: invalid token claims", ErrTokenRejected)
	}
	if v.queries == nil {
		return auth.User{}, fmt.Errorf("%w: token repository is not configured", ErrTokenValidationUnavailable)
	}

	// Expiry and active ownership are validated in one generated query. Roles
	// and permissions remain authoritative in the RBAC resolver and are not
	// cached into the credential identity.
	principal, err := v.queries.GetActivePATPrincipalByUUID(ctx, claims.UUID)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return auth.User{}, contextErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return auth.User{}, fmt.Errorf("%w: token not found", ErrTokenRejected)
		}
		return auth.User{}, fmt.Errorf("%w: db lookup failed: %w", ErrTokenValidationUnavailable, err)
	}
	if principal.TokenID <= 0 || principal.UserID <= 0 {
		return auth.User{}, fmt.Errorf("%w: token principal contains invalid identity data", ErrTokenValidationUnavailable)
	}

	return auth.User{
		ID:             strconv.FormatInt(int64(principal.UserID), 10),
		UserID:         strconv.FormatInt(int64(principal.UserID), 10),
		TokenID:        strconv.FormatInt(int64(principal.TokenID), 10),
		Email:          principal.Email,
		AuthType:       "token",
		TokenProjectID: tokenProjectID(principal.ProjectID),
		TokenProjectActive: tokenProjectActive(
			principal.ProjectID,
			principal.BoundProjectActive,
		),
	}, nil
}

// tokenProjectActive carries the bound project's lifecycle state, which the
// same row already answers, so it costs no additional round trip.
//
// It returns nil for an unbound token: there is no project to report on, and
// nil means "not determined" to the edge.
//
// DEFECT this closes: an operator suspends a project with one UPDATE on
// centry.project, and no binding is revoked. A token bound to that project
// kept resolving to it at the /llm edge. The gateway then kept decrypting the
// project's provider credentials and charging its budget. An UNBOUND caller
// naming the same project was already refused, because that path runs
// IsCurrentUserProjectMember, which requires suspended IS FALSE.
func tokenProjectActive(stored *int32, active bool) *bool {
	if stored == nil || *stored <= 0 {
		return nil
	}
	value := active
	return &value
}

// tokenProjectID converts the stored binding into the identity field. The
// binding comes from elitea_identity.token_project_binding, which the same
// GetActivePATPrincipalByUUID row already carries, so it costs no additional
// round trip (spec-llm-project-scope §3.2). Storage is the only source: no
// header, no token name and no principal name may reach this value.
//
// A non-positive stored value cannot name a project, so it reads as unbound
// rather than as project 0. That fails closed: the caller falls back to the
// personal project instead of binding to an identifier that names nothing.
func tokenProjectID(stored *int32) *int64 {
	if stored == nil || *stored <= 0 {
		return nil
	}
	value := int64(*stored)
	return &value
}
