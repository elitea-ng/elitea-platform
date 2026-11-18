package runtimeplane

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// ConversationSource marks the conversations a runtime_plane case creates.
const ConversationSource = repos.EvalConversationSource

// Turn is what one read of the case's response message sees.
type Turn = repos.EvalCaseTurn

// PostgresStore is the Store over the project's tenant schema; its SQL and its
// integration tests are in the repos package.
type PostgresStore = repos.EvalRuntimeStore

// NewPostgresStore builds the store; a nil pool is refused.
func NewPostgresStore(pool *pgxpool.Pool, logger *slog.Logger) (*PostgresStore, error) {
	return repos.NewEvalRuntimeStore(pool, logger)
}
