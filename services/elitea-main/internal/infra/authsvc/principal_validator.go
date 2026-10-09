package authsvc

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

var ErrPrincipalInactive = auth.ErrPrincipalInactive

// ErrPrincipalUnavailable marks a refusal the principal STORE caused, rather
// than one the principal caused (#537). Every caller must answer 5xx for it.
var ErrPrincipalUnavailable = auth.ErrPrincipalUnavailable

type principalQueries interface {
	GetActivePATPrincipalByID(
		context.Context,
		int32,
	) (sqlcgen.GetActivePATPrincipalByIDRow, error)
	GetActiveUserPrincipalByID(
		context.Context,
		int32,
	) (sqlcgen.GetActiveUserPrincipalByIDRow, error)
}

// PrincipalValidator rechecks mutable account state after a credential or
// session has been validated. This keeps suspension authoritative even when
// the credential result came from the short-lived authentication cache.
type PrincipalValidator struct {
	queries principalQueries
}

func NewPrincipalValidator(pool *pgxpool.Pool) *PrincipalValidator {
	if pool == nil {
		return &PrincipalValidator{}
	}
	return &PrincipalValidator{queries: sqlcgen.New(pool)}
}

func (v *PrincipalValidator) ValidatePrincipal(ctx context.Context, principal auth.User) (auth.User, error) {
	if v == nil || v.queries == nil {
		return auth.User{}, ErrPrincipalInactive
	}

	isToken := principal.TokenID != "" || strings.EqualFold(principal.AuthType, "token")
	if isToken {
		// ID was historically ambiguous: depending on the producer it could be
		// either auth_core__token.id or auth_core__user.id. New trusted producers
		// must provide both typed IDs, so stale or partial cache entries fail
		// closed instead of being resolved through a colliding numeric row.
		if principal.TokenID == "" || principal.UserID == "" {
			return auth.User{}, ErrPrincipalInactive
		}
		id, ok := principalDatabaseID(principal.TokenID)
		if !ok {
			return auth.User{}, ErrPrincipalInactive
		}
		claimedUserID, ok := principalDatabaseID(principal.UserID)
		if !ok {
			return auth.User{}, ErrPrincipalInactive
		}
		row, err := v.queries.GetActivePATPrincipalByID(ctx, id)
		if err != nil {
			return auth.User{}, principalValidationError("token", err)
		}
		if row.TokenID <= 0 || row.UserID <= 0 || claimedUserID != row.UserID {
			return auth.User{}, ErrPrincipalInactive
		}
		principal.TokenID = strconv.FormatInt(int64(row.TokenID), 10)
		principal.UserID = strconv.FormatInt(int64(row.UserID), 10)
		// Compatibility handlers still use User.ID as an owning-user foreign
		// key. Never allow the forwarded token row ID to reach those paths.
		principal.ID = principal.UserID
		// EdgeAuth intentionally emits the current-baseline token reference
		// "-". Resolve mutable identity attributes from PostgreSQL instead of
		// trusting that transport field or a stale session/cache value.
		principal.Email = row.Email
		// The binding is storage-only (auth.User.TokenProjectID), so the row
		// replaces whatever the incoming principal carried. A token that
		// arrives as a forwarded projection is thereby bound exactly as the
		// same token presented as a bearer.
		principal.TokenProjectID = tokenProjectID(row.ProjectID)
		principal.TokenProjectActive = tokenProjectActive(row.ProjectID, row.BoundProjectActive)
		return principal, nil
	}

	userIDValue := principal.UserID
	if userIDValue == "" {
		userIDValue = principal.ID
	}
	userID, ok := principalDatabaseID(userIDValue)
	if !ok {
		return auth.User{}, ErrPrincipalInactive
	}
	row, err := v.queries.GetActiveUserPrincipalByID(ctx, userID)
	if err != nil {
		return auth.User{}, principalValidationError("user", err)
	}
	if row.UserID <= 0 || row.UserID != userID {
		return auth.User{}, ErrPrincipalInactive
	}
	principal.UserID = strconv.FormatInt(int64(row.UserID), 10)
	principal.ID = principal.UserID
	principal.Email = row.Email
	return principal, nil
}

func principalDatabaseID(value string) (int32, bool) {
	id, err := strconv.ParseInt(value, 10, 32)
	if err != nil || id <= 0 {
		return 0, false
	}
	return int32(id), true
}

// principalValidationError separates the two answers PostgreSQL gives.
//
// pgx.ErrNoRows is the store answering: no active row of this kind, so the
// principal may not act. Every other error is the store NOT answering — a pool
// timeout, a cancelled context, a dropped connection — and the principal was
// never read. The second case carries ErrPrincipalUnavailable so the caller
// answers 5xx and not 401 (#537).
//
// The cause is kept in the chain for the log line. It must not reach the
// client: the message names the store, and the store's message can name a
// host, a database, or a query.
func principalValidationError(kind string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPrincipalInactive
	}
	return fmt.Errorf("authsvc: validate active %s: %w: %w", kind, ErrPrincipalUnavailable, err)
}
