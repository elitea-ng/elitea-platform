package repos

// What a provider callback token was minted for
// (elitea_identity.callback_token_grant, shared/0159).
//
// A callback token is an ordinary auth_core__token row, and a user can create
// a PAT with the same name, project binding and expiry. This record is the
// only thing that tells the two apart, and only the minting facade writes it.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrCallbackGrantNotRecorded reports a Record that matched no token row: the
// uuid names no token owned by that user and bound to that project.
var ErrCallbackGrantNotRecorded = errors.New("callback token grant not recorded")

// CallbackTokenGrant is one record.
type CallbackTokenGrant struct {
	// TokenUUID is the minted token's uuid (material.Grant.UUID).
	TokenUUID string
	// OwnerID owns the token; ProjectID is the project it is bound to.
	OwnerID   int64
	ProjectID int64
	// Provider and Tool name the invocation ("inventory", "investigate").
	Provider string
	Tool     string
	// OwnerToolkitID is the invoking toolkit; SourceToolkitIDs the toolkits
	// the invocation may call tools of.
	OwnerToolkitID   int32
	SourceToolkitIDs []int32
}

// CallbackTokenGrants reads and writes the records. A nil pool records
// nothing (Record fails) and admits nothing (AdmitsSourceTool answers false),
// which is the closed direction for both.
type CallbackTokenGrants struct {
	pool *pgxpool.Pool
}

// NewCallbackTokenGrants builds the store over pool.
func NewCallbackTokenGrants(pool *pgxpool.Pool) *CallbackTokenGrants {
	return &CallbackTokenGrants{pool: pool}
}

// Record writes grant for the token it names. The token must exist, belong
// to OwnerID and be bound to ProjectID; anything else records nothing and
// returns ErrCallbackGrantNotRecorded.
func (g *CallbackTokenGrants) Record(ctx context.Context, grant CallbackTokenGrant) error {
	if g == nil || g.pool == nil {
		return fmt.Errorf("%w: no database", ErrCallbackGrantNotRecorded)
	}
	owner, ownerOK := narrowPositive(grant.OwnerID)
	project, projectOK := narrowPositive(grant.ProjectID)
	if !ownerOK || !projectOK || grant.TokenUUID == "" || len(grant.TokenUUID) > 36 ||
		grant.Provider == "" || grant.Tool == "" || grant.OwnerToolkitID <= 0 {
		return fmt.Errorf("%w: incomplete grant", ErrCallbackGrantNotRecorded)
	}
	sources := grant.SourceToolkitIDs
	if sources == nil {
		sources = []int32{}
	}
	tag, err := g.pool.Exec(ctx, `
INSERT INTO elitea_identity.callback_token_grant
    (token_id, provider, tool, project_id, owner_toolkit_id, source_toolkit_ids)
SELECT t.id, $4, $5, $3, $6, $7::integer[]
FROM public.auth_core__token AS t
JOIN elitea_identity.token_project_binding AS b ON b.token_id = t.id
WHERE t.uuid = $1
  AND t.user_id = $2
  AND b.project_id = $3`,
		grant.TokenUUID, owner, project, grant.Provider, grant.Tool,
		grant.OwnerToolkitID, sources)
	if err != nil {
		return fmt.Errorf("record callback token grant: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: token %s", ErrCallbackGrantNotRecorded, grant.TokenUUID)
	}
	return nil
}

// AdmitsSourceTool answers whether tokenID — the token that AUTHENTICATED the
// request (auth.User.TokenID), never one the request names — is a callback
// token owned by userID, bound to projectID, unexpired (no slack: a token
// past its expiry no longer validates either), and minted for provider/tool
// with toolkitID among its source toolkits.
//
// A malformed id answers false, nil.
func (g *CallbackTokenGrants) AdmitsSourceTool(
	ctx context.Context,
	projectID, userID, tokenID, toolkitID int64,
	provider, tool string,
) (bool, error) {
	if g == nil || g.pool == nil {
		return false, nil
	}
	project, projectOK := narrowPositive(projectID)
	user, userOK := narrowPositive(userID)
	token, tokenOK := narrowPositive(tokenID)
	toolkit, toolkitOK := narrowPositive(toolkitID)
	if !projectOK || !userOK || !tokenOK || !toolkitOK || provider == "" || tool == "" {
		return false, nil
	}
	var ok bool
	if err := g.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM public.auth_core__token AS t
    JOIN elitea_identity.token_project_binding AS b ON b.token_id = t.id
    JOIN elitea_identity.callback_token_grant AS g ON g.token_id = t.id
    WHERE t.id = $1
      AND t.user_id = $2
      AND b.project_id = $3
      AND g.project_id = $3
      AND g.provider = $4
      AND g.tool = $5
      AND $6 = ANY (g.source_toolkit_ids)
      AND t.expires IS NOT NULL
      AND t.expires > (clock_timestamp() AT TIME ZONE 'UTC')
)`, token, user, project, provider, tool, toolkit).Scan(&ok); err != nil {
		return false, fmt.Errorf("check callback token grant: %w", err)
	}
	return ok, nil
}

// ErrCallbackTokenFactsNotFound reports a token that is not a live callback
// token: no such row, no project binding, no recorded grant, a grant for
// another project, no expiry, or an expiry in the past.
var ErrCallbackTokenFactsNotFound = errors.New("callback token facts not found")

// CallbackTokenFacts is what elitea-vector's token introspection
// (ADR-0031) needs about a callback token beyond its signature.
type CallbackTokenFacts struct {
	UserID    int64
	ProjectID int64
	ExpiresAt time.Time
	Provider  string
}

// Facts reads the facts of tokenID — the token that the signature check
// resolved, never one a request names. Only a live callback token answers:
// one with a project binding, a grant recorded for that same project, and
// an expiry in the future. A personal access token with the same name,
// binding and expiry has no grant, so it answers ErrCallbackTokenFactsNotFound.
func (g *CallbackTokenGrants) Facts(ctx context.Context, tokenID int64) (CallbackTokenFacts, error) {
	if g == nil || g.pool == nil {
		return CallbackTokenFacts{}, fmt.Errorf("%w: no database", ErrCallbackTokenFactsNotFound)
	}
	token, ok := narrowPositive(tokenID)
	if !ok {
		return CallbackTokenFacts{}, ErrCallbackTokenFactsNotFound
	}
	var (
		userID, projectID int32
		expires           time.Time
		provider          string
	)
	err := g.pool.QueryRow(ctx, `
SELECT t.user_id, b.project_id, t.expires, g.provider
FROM public.auth_core__token AS t
JOIN elitea_identity.token_project_binding AS b ON b.token_id = t.id
JOIN elitea_identity.callback_token_grant AS g ON g.token_id = t.id
WHERE t.id = $1
  AND g.project_id = b.project_id
  AND t.user_id IS NOT NULL
  AND t.expires IS NOT NULL
  AND t.expires > (clock_timestamp() AT TIME ZONE 'UTC')`, token).
		Scan(&userID, &projectID, &expires, &provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return CallbackTokenFacts{}, ErrCallbackTokenFactsNotFound
	}
	if err != nil {
		return CallbackTokenFacts{}, fmt.Errorf("read callback token facts: %w", err)
	}
	return CallbackTokenFacts{
		UserID:    int64(userID),
		ProjectID: int64(projectID),
		// auth_core__token.expires is a UTC wall clock without a zone.
		ExpiresAt: time.Date(expires.Year(), expires.Month(), expires.Day(),
			expires.Hour(), expires.Minute(), expires.Second(), expires.Nanosecond(), time.UTC),
		Provider: provider,
	}, nil
}

func narrowPositive(value int64) (int32, bool) {
	if value < 1 || value > 1<<31-1 {
		return 0, false
	}
	return int32(value), true
}
