package identityrepo

// What the FORM provisioning plane must leave behind, against a real
// PostgreSQL.
//
// # Why this needed a database, and why it needed a plane
//
// #615 gave the OIDC and SAML plane an actor personal access token at first
// sign-in. The Form plane — the one internal/authcomposition.NewFormGraph
// composes, and the only one a deployment without single sign-on has — was
// left out, so which login route an operator chose decided whether a fresh
// install could complete a chat turn. The install that could not was cured
// only by hand-written SQL in deploy/scripts/standalone-stack.sh.
//
// The defect is a ROW THAT WAS NEVER WRITTEN. Every scripted provisioning test
// went on passing while the row was absent, because absence is exactly what a
// scripted transaction cannot see. So these read the row back through
// GetActivePATForUser — the SAME query authsvc.LocalIssuer reads at runtime
// stage `actor_pat_issuance`, not a lookalike SELECT.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/identity"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

func TestFormProvisioningIssuesTheActorPAT(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := newActorPATTestDatabase(t, ctx)
	service := newProvisionService(t, pool, identity.ProvisioningPolicy{})

	// ── the row a fresh Form login must leave ─────────────────────────────
	t.Run("a first form login leaves a visible revocable actor PAT", func(t *testing.T) {
		result := provisionForm(t, ctx, service, "first-login")

		tokens := actorPATRows(t, ctx, pool, result.UserID)
		if len(tokens) != 1 {
			t.Fatalf("tokens = %d, want 1: a form-provisioned user was left with none", len(tokens))
		}
		if tokens[0].name != ActorPATName {
			t.Fatalf("token name = %q, want %q: the auto-issued key must be "+
				"distinguishable from one the user made", tokens[0].name, ActorPATName)
		}
		if tokens[0].expires {
			t.Fatal("the auto-issued token must not expire; expiry breaks every " +
				"later chat turn with the same database-stage error")
		}
		if tokens[0].uuid == "" {
			t.Fatal("the bearer is signed from the uuid, so an empty one is unusable")
		}

		// Read it back through the query the runtime itself reads. A row that
		// this query misses is the same failure as no row at all.
		active, err := sqlcgen.New(pool).GetActivePATForUser(ctx, int32(result.UserID))
		if err != nil {
			t.Fatalf("GetActivePATForUser: %v (stage actor_pat_issuance would fail)", err)
		}
		if active.Uuid == nil || *active.Uuid != tokens[0].uuid {
			t.Fatalf("active PAT uuid = %v, want %q", active.Uuid, tokens[0].uuid)
		}
	})

	// ── the three ways it must NOT write ──────────────────────────────────
	t.Run("repeated logins issue only one", func(t *testing.T) {
		first := provisionForm(t, ctx, service, "repeat-login")
		second := provisionForm(t, ctx, service, "repeat-login")
		third := provisionForm(t, ctx, service, "repeat-login")
		if second.UserID != first.UserID || third.UserID != first.UserID {
			t.Fatalf("user ids = %d, %d, %d", first.UserID, second.UserID, third.UserID)
		}
		if tokens := actorPATRows(t, ctx, pool, first.UserID); len(tokens) != 1 {
			t.Fatalf("tokens = %d, want 1: a key accumulated per login", len(tokens))
		}
	})

	t.Run("an account that already has its own key gets nothing extra", func(t *testing.T) {
		var userID int64
		if err := pool.QueryRow(ctx, `
INSERT INTO public.auth_core__user (email, name) VALUES ('own-key@centry.user', 'Own Key')
RETURNING id`).Scan(&userID); err != nil {
			t.Fatal(err)
		}
		mustExec(t, ctx, pool, `
INSERT INTO public.auth_core__token (uuid, user_id, name) VALUES ('own-key-uuid', $1, 'my laptop')`,
			userID)

		result := provisionForm(t, ctx, service, "own-key")
		if result.UserID != userID {
			t.Fatalf("user id = %d, want %d", result.UserID, userID)
		}
		tokens := actorPATRows(t, ctx, pool, result.UserID)
		if len(tokens) != 1 || tokens[0].name != "my laptop" {
			t.Fatalf("tokens = %+v, want the user's own key alone", tokens)
		}
	})

	t.Run("a suspended account is provisioned nothing", func(t *testing.T) {
		mustExec(t, ctx, pool, `
INSERT INTO public.auth_core__user (email, name, suspended)
VALUES ('suspended@centry.user', 'Suspended', true)`)

		_, err := service.Provision(ctx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
			Provider:          "form",
			ProviderReference: "suspended",
		}})
		if !errors.Is(err, identity.ErrIdentitySuspended) {
			t.Fatalf("error = %v, want %v", err, identity.ErrIdentitySuspended)
		}
		assertCount(t, ctx, pool, 0, `
SELECT count(*) FROM public.auth_core__token AS t
JOIN public.auth_core__user AS u ON u.id = t.user_id
WHERE u.email = 'suspended@centry.user'`)
	})

	// ── the way it must heal ──────────────────────────────────────────────
	t.Run("revoking every key is healed by the next login", func(t *testing.T) {
		result := provisionForm(t, ctx, service, "self-heal")
		mustExec(t, ctx, pool, `DELETE FROM public.auth_core__token WHERE user_id = $1`, result.UserID)

		repeated := provisionForm(t, ctx, service, "self-heal")
		if repeated.UserID != result.UserID {
			t.Fatalf("user id = %d, want %d", repeated.UserID, result.UserID)
		}
		tokens := actorPATRows(t, ctx, pool, result.UserID)
		if len(tokens) != 1 || tokens[0].name != ActorPATName {
			t.Fatalf("tokens = %+v: deleting every key left the account permanently unable to chat", tokens)
		}
	})

	// An EXPIRED key is not an active one. GetActivePATForUser is what decides
	// that, and the issuance predicate reads the same query, so a person whose
	// only key has lapsed is re-armed rather than left broken.
	t.Run("an expired key does not count as active", func(t *testing.T) {
		result := provisionForm(t, ctx, service, "expired-key")
		mustExec(t, ctx, pool, `
UPDATE public.auth_core__token SET expires = TIMESTAMP '2000-01-01 00:00:00' WHERE user_id = $1`,
			result.UserID)

		provisionForm(t, ctx, service, "expired-key")
		tokens := actorPATRows(t, ctx, pool, result.UserID)
		if len(tokens) != 2 {
			t.Fatalf("tokens = %d, want 2: the lapsed key was treated as active", len(tokens))
		}
	})
}

// The rollback contract still holds with the token write inside it: a
// provisioning that cannot finish leaves no credential behind either.
func TestFormProvisioningRollbackLeavesNoActorPAT(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := newIdentityTestDatabase(t, ctx)
	// No root group: the membership insert fails after the user row is written.
	service := newProvisionService(t, pool, identity.ProvisioningPolicy{})

	_, err := service.Provision(ctx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
		Provider:          "form",
		ProviderReference: "rollback-form",
	}})
	if !errors.Is(err, identity.ErrProvisioningFailed) {
		t.Fatalf("error = %v, want %v", err, identity.ErrProvisioningFailed)
	}
	assertCount(t, ctx, pool, 0, `SELECT count(*) FROM public.auth_core__token`)
}

func provisionForm(
	t *testing.T,
	ctx context.Context,
	service *identity.ProvisionService,
	reference string,
) identity.ProvisionResult {
	t.Helper()
	result, err := service.Provision(ctx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
		Provider:          "form",
		ProviderReference: reference,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.UserID <= 0 {
		t.Fatalf("result = %+v", result)
	}
	return result
}

type actorPATRow struct {
	name    string
	uuid    string
	expires bool
}

func actorPATRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID int64) []actorPATRow {
	t.Helper()
	rows, err := pool.Query(ctx, `
SELECT COALESCE(name, ''), COALESCE(uuid, ''), expires IS NOT NULL
FROM public.auth_core__token WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var tokens []actorPATRow
	for rows.Next() {
		var token actorPATRow
		if err := rows.Scan(&token.name, &token.uuid, &token.expires); err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return tokens
}

// newActorPATTestDatabase is the provisioning fixture plus the root group a
// newly created account is added to. No role rows: this file is about the
// credential, and the grant has its own coverage in the HTTP-level test.
func newActorPATTestDatabase(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	pool := newIdentityTestDatabase(t, ctx)
	mustExec(t, ctx, pool, `INSERT INTO public.auth_core__group (id, name) VALUES (1, 'Root')`)
	return pool
}
