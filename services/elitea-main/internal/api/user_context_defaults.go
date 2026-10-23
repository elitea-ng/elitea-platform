package api

import (
	"github.com/jackc/pgx/v5/pgxpool"

	v2convs "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// newUserContextDefaults wires the conversation routes' middle resolution tier
// — the caller's Settings › Memory defaults — to the repository that reads
// them off centry.social_users.
//
// It returns nil when there is no pool, the same "no database configured,
// degrade gracefully" convention newAttachmentStore follows: the context
// routes then resolve a conversation with no stored strategy straight to the
// contract's constants, which is a truthful answer rather than an error at
// router construction time.
//
// A typed nil would NOT do here — `WithUserContextDefaults((*T)(nil))` stores
// a non-nil interface holding a nil pointer, and the handler's `== nil` guard
// would not fire. Hence the explicit branch (the typed-nil trap this codebase
// has hit before).
func newUserContextDefaults(pool *pgxpool.Pool) v2convs.UserContextDefaults {
	if pool == nil {
		return nil
	}
	return dbrepos.NewUserContextDefaultsRepo(pool)
}
