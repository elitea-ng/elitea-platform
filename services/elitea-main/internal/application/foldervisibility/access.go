// Package foldervisibility resolves the optional folder authorization projection.
package foldervisibility

import (
	"context"
	"errors"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Access struct {
	ActorID            int64
	FolderRestrictions bool
}

var ErrIdentity = errors.New("folder visibility requires an owning user")
var ErrPartialProjection = errors.New("folder access projection is incomplete")

// Resolve requires actor identity, including when the optional projection is absent.
func Resolve(ctx context.Context, pool *pgxpool.Pool, schema string) (Access, error) {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return Access{}, ErrIdentity
	}
	actor, ok := user.OwningUserID()
	if !ok {
		return Access{}, ErrIdentity
	}
	if pool == nil {
		return Access{}, errors.New("folder visibility database is unavailable")
	}
	var state int32
	err := pool.QueryRow(ctx, `SELECT CASE
 WHEN to_regclass($1) IS NOT NULL AND to_regclass($2) IS NOT NULL AND to_regclass($3) IS NOT NULL THEN 1
 WHEN to_regclass($3) IS NULL THEN 0 ELSE -1 END::integer`,
		schema+".entity_folders", schema+".social_folder_items", schema+".folder_access_overrides").Scan(&state)
	if err != nil {
		return Access{}, err
	}
	if state < 0 {
		return Access{}, ErrPartialProjection
	}
	return Access{ActorID: actor, FolderRestrictions: state == 1}, nil
}
