package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/contextsettings"
)

// UserContextDefaultsRepo reads one user's context-management defaults off the
// author record — the two jsonb columns `PUT /social/author` writes.
//
// It is a separate repository from the conversations one because the row it
// reads is not a tenant row: centry.social_users is central, one per user,
// while everything ConversationsRepo touches is inside a project schema. The
// per-conversation strategy route needs both, and the resolution rule is the
// only thing that joins them.
type UserContextDefaultsRepo struct {
	pool *pgxpool.Pool
}

func NewUserContextDefaultsRepo(pool *pgxpool.Pool) *UserContextDefaultsRepo {
	return &UserContextDefaultsRepo{pool: pool}
}

// ContextDefaults answers with the user's defaults, or the zero value when the
// user has none.
//
// A user with no social_users row is not an error: it is the ordinary state of
// an account that has never opened Settings › Memory, and the resolution rule
// answers it with the contract's constants. Only a real query failure is
// reported, so a caller cannot mistake "the database is down" for "this user
// has no preferences".
func (r *UserContextDefaultsRepo) ContextDefaults(ctx context.Context, userID int64) (contextsettings.UserDefaults, error) {
	if r == nil || r.pool == nil || userID <= 0 {
		return contextsettings.UserDefaults{}, nil
	}

	var contextManagement, summarization []byte
	err := r.pool.QueryRow(ctx, `
		SELECT default_context_management, default_summarization
		FROM centry.social_users
		WHERE user_id = $1::integer
	`, userID).Scan(&contextManagement, &summarization)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contextsettings.UserDefaults{}, nil
		}
		return contextsettings.UserDefaults{}, fmt.Errorf("repos: read user context defaults: %w", err)
	}

	// A stored block that will not decode is treated as "no opinion" rather
	// than as a failure: this read sits on the chat path, and one malformed
	// settings blob must not take a conversation down. The write path
	// validates, so a blob can only get here from outside this service.
	decodedContext, _ := contextsettings.DecodeContextManagement(contextManagement)
	decodedSummary, _ := contextsettings.DecodeSummarization(summarization)

	return contextsettings.UserDefaults{
		ContextManagement: decodedContext,
		Summarization:     decodedSummary,
	}, nil
}

// Personalization reads only the authenticated actor's defaults for new chats.
func (r *UserContextDefaultsRepo) Personalization(ctx context.Context, userID int64) (map[string]any, error) {
	if r == nil || r.pool == nil || userID <= 0 {
		return nil, nil
	}
	var raw []byte
	err := r.pool.QueryRow(ctx, `SELECT personalization FROM centry.social_users WHERE user_id=$1`, userID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if len(raw) == 0 {
		return nil, nil
	}
	if err = json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}
