package repos

// What a provider callback token was minted for
// (elitea_identity.callback_token_grant, shared/0159).
//
// A callback token is an ordinary auth_core__token row, and a user can create
// a PAT with the same name, project binding and expiry. This record is the
// only thing that tells the two apart, and only the minting facade writes it.
//
// TWO KINDS OF RECORD share the one row a token may have (token_id is the
// key), told apart by owner_toolkit_id:
//
//   - an INVOCATION grant (RecordInvocation): written for every bearer a
//     DeepWiki or Inventory facade mints, before the hop. owner_toolkit_id is
//     0 (no toolkit row has that id) and source_toolkit_ids is empty, so it
//     admits no source tool; what it carries is the provider, which decides
//     the vector source the token may use in elitea-vector (ADR-0031,
//     vectorintrospection.ProviderSource);
//   - a SOURCE-TOOL grant (Record): Inventory `investigate` with a named
//     invoking toolkit. It upgrades the invocation grant the mint already
//     wrote for the same token, provider and project, adding the invoking
//     toolkit and its source toolkits (test_tool's SourceToolGate).
//
// Reusing shared/0159's columns needs no new migration: 0 never names a
// toolkit, and AdmitsSourceTool requires the toolkit among the sources, which
// an invocation grant has none of.

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
//
// A token that already carries a grant — the invocation grant its mint wrote
// (RecordInvocation) — is UPGRADED in place when that grant is for the same
// provider and project: the tool, the invoking toolkit and the sources are
// replaced. A grant for another provider or project is never rewritten, and
// the call then records nothing.
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
  AND b.project_id = $3
ON CONFLICT (token_id) DO UPDATE
SET tool               = EXCLUDED.tool,
    owner_toolkit_id   = EXCLUDED.owner_toolkit_id,
    source_toolkit_ids = EXCLUDED.source_toolkit_ids
WHERE callback_token_grant.provider = EXCLUDED.provider
  AND callback_token_grant.project_id = EXCLUDED.project_id`,
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

// CallbackInvocationGrant is the record every minted DeepWiki or Inventory
// callback bearer gets: the provider and tool of the invocation it was minted
// for, and the project it is bound to. It names no toolkit.
type CallbackInvocationGrant struct {
	// TokenUUID is the minted token's uuid (material.Grant.UUID).
	TokenUUID string
	// OwnerID owns the token; ProjectID is the project it is bound to.
	OwnerID   int64
	ProjectID int64
	// Provider is the facade ("deepwiki", "inventory"); Tool the invoked tool.
	Provider string
	Tool     string
}

// RecordInvocation writes the invocation grant of a freshly minted token
// (owner_toolkit_id 0, no sources). The token must exist, belong to OwnerID
// and be bound to ProjectID, and carry no grant yet; anything else records
// nothing and returns ErrCallbackGrantNotRecorded.
func (g *CallbackTokenGrants) RecordInvocation(ctx context.Context, grant CallbackInvocationGrant) error {
	if g == nil || g.pool == nil {
		return fmt.Errorf("%w: no database", ErrCallbackGrantNotRecorded)
	}
	owner, ownerOK := narrowPositive(grant.OwnerID)
	project, projectOK := narrowPositive(grant.ProjectID)
	if !ownerOK || !projectOK || grant.TokenUUID == "" || len(grant.TokenUUID) > 36 ||
		grant.Provider == "" || grant.Tool == "" {
		return fmt.Errorf("%w: incomplete grant", ErrCallbackGrantNotRecorded)
	}
	tag, err := g.pool.Exec(ctx, `
INSERT INTO elitea_identity.callback_token_grant
    (token_id, provider, tool, project_id, owner_toolkit_id, source_toolkit_ids)
SELECT t.id, $4, $5, $3, 0, '{}'::integer[]
FROM public.auth_core__token AS t
JOIN elitea_identity.token_project_binding AS b ON b.token_id = t.id
WHERE t.uuid = $1
  AND t.user_id = $2
  AND b.project_id = $3
ON CONFLICT (token_id) DO NOTHING`,
		grant.TokenUUID, owner, project, grant.Provider, grant.Tool)
	if err != nil {
		return fmt.Errorf("record callback invocation grant: %w", err)
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
