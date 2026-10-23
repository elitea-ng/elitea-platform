package authsvc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

var ErrCurrentUserUnavailable = errors.New("authsvc: current user is unavailable")

// CurrentAuthUser is the current-baseline auth_core user record. Profile,
// provider-session, personal-project, and social fields belong to their own
// composing use cases and are deliberately not added to the security principal.
type CurrentAuthUser struct {
	ID        int64
	Email     *string
	Name      *string
	LastLogin *time.Time
	Suspended bool
}

type currentUserQueries interface {
	GetCurrentActiveAuthUser(context.Context, int32) (sqlcgen.AuthCoreUser, error)
}

// CurrentUserResolver loads mutable user attributes only after authentication
// has resolved an unambiguous owning user ID. It does not trust email, name,
// roles, or permissions carried by a credential or cookie.
type CurrentUserResolver struct {
	queries currentUserQueries
}

func NewCurrentUserResolver(pool *pgxpool.Pool) *CurrentUserResolver {
	resolver := &CurrentUserResolver{}
	if pool != nil {
		resolver.queries = sqlcgen.New(pool)
	}
	return resolver
}

func (r *CurrentUserResolver) Resolve(
	ctx context.Context,
	principal auth.User,
) (CurrentAuthUser, error) {
	if r == nil || r.queries == nil {
		return CurrentAuthUser{}, ErrCurrentUserUnavailable
	}

	ownerID, ok := principal.OwningUserID()
	if !ok || ownerID > math.MaxInt32 {
		return CurrentAuthUser{}, ErrCurrentUserUnavailable
	}
	row, err := r.queries.GetCurrentActiveAuthUser(ctx, int32(ownerID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CurrentAuthUser{}, ErrCurrentUserUnavailable
		}
		return CurrentAuthUser{}, fmt.Errorf("authsvc: resolve current user: %w", err)
	}
	if row.ID <= 0 || int64(row.ID) != ownerID || row.Suspended {
		return CurrentAuthUser{}, ErrCurrentUserUnavailable
	}

	var lastLogin *time.Time
	if row.LastLogin.Valid {
		value := row.LastLogin.Time
		lastLogin = &value
	}
	return CurrentAuthUser{
		ID:        int64(row.ID),
		Email:     cloneOptionalString(row.Email),
		Name:      cloneOptionalString(row.Name),
		LastLogin: lastLogin,
		Suspended: row.Suspended,
	}, nil
}

func cloneOptionalString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
