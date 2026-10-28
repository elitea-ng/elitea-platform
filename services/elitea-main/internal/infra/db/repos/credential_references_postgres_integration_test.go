package repos

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Shared 0193 (issue #1124): staging is idempotent, activation is
// two-person (in code and in the CHECK), retire revokes at the epoch layer
// the broker checks, attach edits the toolkit's configuration reference and
// moves its digest, and a pin records the digest admission computes.
// CI-only: skipped without the integration database.

const refTestBinding = "0192f5e2-7c3a-7b1e-8a9d-000000001241"

func newRefTestID() string { return uuid.NewString() }

// seedRefTenant creates the tenant tables these tests read: the repos
// template has no p_7.elitea_tools.
func seedRefTenant(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
CREATE SCHEMA IF NOT EXISTS p_7;
CREATE TABLE IF NOT EXISTS p_7.elitea_tools (
    id serial PRIMARY KEY, type varchar NOT NULL, name varchar(128),
    settings jsonb NOT NULL DEFAULT '{}'::jsonb, meta jsonb NOT NULL DEFAULT '{}'::jsonb)`)
	require.NoError(t, err)
}

func refTestToolkit(t *testing.T, pool *pgxpool.Pool, toolkitType, settings string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO p_7.elitea_tools (type, name, settings) VALUES ($1, 't', $2::jsonb) RETURNING id`,
		toolkitType, settings).Scan(&id))
	return id
}

func stageRefTest(t *testing.T, store *CredentialReferences, source string) StageResult {
	t.Helper()
	result, err := store.StageReference(context.Background(), StagedReference{
		TenantProjectID: 7, ProviderType: "jira", TargetIdentity: "https://acme.atlassian.net",
		Class: credentialgrant.ClassStaticSecret, Backend: "pgvault", Locator: "0123456789abcdef0123456789abcdef",
		Generation: 1, MigratedFrom: source, StagedBy: "100",
	}, newRefTestID)
	require.NoError(t, err)
	return result
}

func TestCredentialReferenceStagingIsIdempotentAndActivationIsTwoPerson(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	store := NewCredentialReferences(pool, agentIdentityAudit(t))

	first := stageRefTest(t, store, "configuration:11111111-1111-4111-8111-111111111111:pgvault")
	require.True(t, first.Created)
	require.Equal(t, credentialgrant.ReferenceStaged, first.Record.State)
	require.Equal(t, "100", first.Record.StagedBy)
	again := stageRefTest(t, store, "configuration:11111111-1111-4111-8111-111111111111:pgvault")
	require.False(t, again.Created)
	require.Equal(t, first.Record.ID, again.Record.ID)
	other := stageRefTest(t, store, "configuration:22222222-2222-4222-8222-222222222222:pgvault")
	require.Equal(t, first.Record.TargetBindingID, other.Record.TargetBindingID, "one target per canonical identity")
	var references, targets int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_identity.credential_references WHERE tenant_project_id = 7`).Scan(&references))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_identity.target_bindings WHERE tenant_project_id = 7`).Scan(&targets))
	require.Equal(t, 2, references)
	require.Equal(t, 1, targets)

	// The stager cannot activate; the database refuses it as well.
	_, err := store.Transition(ctx, first.Record.ID, CredentialReferenceActivate, "100")
	require.ErrorIs(t, err, ErrCredentialReferenceSelfActive)
	_, err = pool.Exec(ctx, `UPDATE elitea_identity.credential_references SET activated_by = staged_by WHERE id = $1::uuid`, first.Record.ID)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr))
	require.Equal(t, "credential_references_activator_not_stager", pgErr.ConstraintName)

	active, err := store.Transition(ctx, first.Record.ID, CredentialReferenceActivate, "200")
	require.NoError(t, err)
	require.Equal(t, credentialgrant.ReferenceActive, active.State)
	require.Equal(t, "200", active.ActivatedBy)
	_, err = store.Transition(ctx, first.Record.ID, CredentialReferenceActivate, "300")
	require.ErrorIs(t, err, ErrCredentialReferenceTransition)

	// A hand-written row with no stager never activates.
	noStager := uuid.NewString()
	_, err = pool.Exec(ctx, `INSERT INTO elitea_identity.credential_references
    (id, tenant_project_id, class, backend, locator, provider_type, target_binding_id, generation)
VALUES ($1::uuid, 7, 'static_secret', 'pgvault', 'x', 'jira', $2::uuid, 1)`, noStager, first.Record.TargetBindingID)
	require.NoError(t, err)
	_, err = store.Transition(ctx, noStager, CredentialReferenceActivate, "200")
	require.ErrorIs(t, err, ErrCredentialReferenceNoStager)

	listed, err := store.List(ctx, CredentialReferenceFilter{TenantProjectID: 7, State: credentialgrant.ReferenceActive})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, "https://acme.atlassian.net", listed[0].TargetIdentity)
}

type recordingNotifier struct{ changes []epoch.Change }

func (n *recordingNotifier) Notify(_ context.Context, changes ...epoch.Change) {
	n.changes = append(n.changes, changes...)
}

// Retire revokes at the epoch layer: the broker's checkEpochs reads the
// reference's epoch with this reader and denies authority_revoked for a
// stale key, so the next RedeemCredential of a run that froze it is denied.
func TestCredentialReferenceRetireRevokesTheNextRedemption(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	notifier := &recordingNotifier{}
	store := NewCredentialReferences(pool, agentIdentityAudit(t)).WithNotifier(notifier)
	staged := stageRefTest(t, store, "configuration:33333333-3333-4333-8333-333333333333:pgvault")
	_, err := store.Transition(ctx, staged.Record.ID, CredentialReferenceActivate, "200")
	require.NoError(t, err)

	reader, err := epoch.NewPostgresStore(pool)
	require.NoError(t, err)
	frozen := []epoch.SecurityEpoch{{Kind: epoch.KindCredentialRef, ID: staged.Record.ID, Value: staged.Record.Generation}}
	current, err := reader.Read(ctx, frozen)
	require.NoError(t, err)
	require.Empty(t, epoch.Stale(frozen, current), "an active reference redeems")

	retired, err := store.Transition(ctx, staged.Record.ID, CredentialReferenceRetire, "200")
	require.NoError(t, err)
	require.Equal(t, credentialgrant.ReferenceRetired, retired.State)
	require.Equal(t, staged.Record.Generation, retired.Generation, "the secret version never moves")
	current, err = reader.Read(ctx, frozen)
	require.NoError(t, err)
	require.Equal(t, epoch.Revoked, current[0].Value)
	require.Len(t, epoch.Stale(frozen, current), 1, "the next redemption is denied authority_revoked")
	require.Len(t, notifier.changes, 1)
	require.Equal(t, epoch.KindCredentialRef, notifier.changes[0].Epoch.Kind)

	_, err = store.Transition(ctx, staged.Record.ID, CredentialReferenceRetire, "200")
	require.ErrorIs(t, err, ErrCredentialReferenceTransition)
	// The governed resolver refuses it too.
	grants := NewCredentialGrants(pool, nil)
	ref, err := grants.CredentialReference(ctx, 7, staged.Record.ID)
	require.NoError(t, err)
	require.False(t, ref.State.Usable())
}

func TestCredentialReferenceAttachMovesTheDigestAndPinFollowsIt(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedRefTenant(t, pool)
	seedAgentIdentityPolicyRevision(t, pool)
	ctx := context.Background()
	_, err := NewAgentIdentityBindings(pool, agentIdentityAudit(t)).CreateBinding(ctx, proposedTestBinding(t, refTestBinding, 1241, 0x41))
	require.NoError(t, err)
	store := NewCredentialReferences(pool, agentIdentityAudit(t))
	staged := stageRefTest(t, store, "configuration:44444444-4444-4444-8444-444444444444:pgvault")

	toolkit := refTestToolkit(t, pool, "jira",
		`{"jira_configuration": {"elitea_title": "jira-prod", "private": false}, "limit": 5, "project": "ACME"}`)
	target := ToolkitAttachment{ProjectID: 7, ToolkitID: toolkit}

	before, err := store.PinToolkit(ctx, refTestBinding, target, "100")
	require.NoError(t, err)

	attached, err := store.Attach(ctx, staged.Record.ID, target, "100")
	require.NoError(t, err)
	require.Equal(t, "jira_configuration", attached.Field)
	require.NotEqual(t, before.ToolkitDigest, attached.ToolkitDigest, "attaching edits the toolkit")
	settings := readRefTestSettings(t, pool, toolkit)
	refs := configurationapp.ToolkitCredentialRefs(settings)
	require.Len(t, refs, 1)
	require.Equal(t, staged.Record.ID, refs[0].CredentialReferenceID)
	digest, err := credentialgrant.ToolkitDigest("jira", settings)
	require.NoError(t, err)
	require.Equal(t, digest, attached.ToolkitDigest)

	// The pin follows the new digest; it equals what admission computes.
	after, err := store.PinToolkit(ctx, refTestBinding, target, "100")
	require.NoError(t, err)
	require.Equal(t, digest, after.ToolkitDigest)
	bound, err := NewCredentialGrants(pool, nil).AgentToolkitBinding(ctx, refTestBinding, toolkit)
	require.NoError(t, err)
	require.Equal(t, digest, bound.ToolkitDigest)

	// Attaching again is a no-op; another reference is refused.
	_, err = store.Attach(ctx, staged.Record.ID, target, "100")
	require.NoError(t, err)
	second := stageRefTest(t, store, "configuration:55555555-5555-4555-8555-555555555555:pgvault")
	_, err = store.Attach(ctx, second.Record.ID, target, "100")
	require.ErrorIs(t, err, ErrToolkitAttachedElsewhere)

	detached, err := store.Detach(ctx, staged.Record.ID, target, "100")
	require.NoError(t, err)
	require.Empty(t, detached.CredentialReferenceID)
	require.Equal(t, before.ToolkitDigest, detached.ToolkitDigest, "detach restores the stored settings")
	require.Empty(t, configurationapp.ToolkitCredentialRefs(readRefTestSettings(t, pool, toolkit))[0].CredentialReferenceID)

	// Refusals: another provider, a private reference, a toolkit with no
	// configuration reference (MCP), another tenant, a missing toolkit.
	confluence := refTestToolkit(t, pool, "confluence", `{"c": {"elitea_title": "x", "private": false}}`)
	_, err = store.Attach(ctx, staged.Record.ID, ToolkitAttachment{ProjectID: 7, ToolkitID: confluence}, "100")
	require.ErrorIs(t, err, ErrCredentialReferenceProvider)
	private := refTestToolkit(t, pool, "jira", `{"c": {"elitea_title": "x", "private": true}}`)
	_, err = store.Attach(ctx, staged.Record.ID, ToolkitAttachment{ProjectID: 7, ToolkitID: private}, "100")
	require.ErrorIs(t, err, ErrToolkitPrivateConfiguration)
	bare := refTestToolkit(t, pool, "jira", `{"url": "https://mcp.example.com/mcp"}`)
	_, err = store.Attach(ctx, staged.Record.ID, ToolkitAttachment{ProjectID: 7, ToolkitID: bare}, "100")
	require.ErrorIs(t, err, ErrToolkitNoConfigurationRef)
	_, err = store.Attach(ctx, staged.Record.ID, ToolkitAttachment{ProjectID: 8, ToolkitID: toolkit}, "100")
	require.ErrorIs(t, err, ErrCredentialReferenceTenant)
	_, err = store.Attach(ctx, staged.Record.ID, ToolkitAttachment{ProjectID: 7, ToolkitID: 999999}, "100")
	require.ErrorIs(t, err, ErrToolkitNotFound)

	require.NoError(t, store.UnpinToolkit(ctx, refTestBinding, target, "100"))
	require.ErrorIs(t, store.UnpinToolkit(ctx, refTestBinding, target, "100"), ErrToolkitNotFound)
	_, err = store.PinToolkit(ctx, refTestBinding, ToolkitAttachment{ProjectID: 8, ToolkitID: toolkit}, "100")
	require.ErrorIs(t, err, ErrAgentBindingNotUsable)
}

func TestCredentialReferenceWriteRollsBackWhenItsAuditAppendFails(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	staged := stageRefTest(t, NewCredentialReferences(pool, agentIdentityAudit(t)), "configuration:66666666-6666-4666-8666-666666666666:pgvault")
	failing := NewCredentialReferences(pool, securityaudit.FailingAppender{Err: errors.New("outbox down")})
	_, err := failing.Transition(ctx, staged.Record.ID, CredentialReferenceActivate, "200")
	require.Error(t, err)
	rec, err := NewCredentialReferences(pool, nil).Get(ctx, staged.Record.ID)
	require.NoError(t, err)
	require.Equal(t, credentialgrant.ReferenceStaged, rec.State)
}

func readRefTestSettings(t *testing.T, pool *pgxpool.Pool, toolkit int64) map[string]any {
	t.Helper()
	var raw []byte
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT settings::text FROM p_7.elitea_tools WHERE id = $1`, toolkit).Scan(&raw))
	decoded, err := decodeCurrentToolkitJSON(raw, "settings")
	require.NoError(t, err)
	return decoded.(map[string]any)
}

// A retired reference does not hold its migration source: the next staging
// of the same source is a NEW reference (the runbook's rotation: retire,
// then stage, activate, attach and pin a replacement), and it is in turn
// idempotent.
func TestCredentialReferenceRetiredSourceStagesAReplacement(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	store := NewCredentialReferences(pool, agentIdentityAudit(t))
	const source = "configuration:77777777-7777-4777-8777-777777777777:pgvault"
	first := stageRefTest(t, store, source)
	_, err := store.Transition(ctx, first.Record.ID, CredentialReferenceRetire, "200")
	require.NoError(t, err)

	_, found, err := store.FindMigrated(ctx, 7, source)
	require.NoError(t, err)
	require.False(t, found, "a retired reference is not the source's live reference")
	replacement := stageRefTest(t, store, source)
	require.True(t, replacement.Created)
	require.NotEqual(t, first.Record.ID, replacement.Record.ID)
	require.Equal(t, credentialgrant.ReferenceStaged, replacement.Record.State)
	again := stageRefTest(t, store, source)
	require.False(t, again.Created)
	require.Equal(t, replacement.Record.ID, again.Record.ID)
	live, found, err := store.FindMigrated(ctx, 7, source)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, replacement.Record.ID, live.ID)
	// The database still holds one live reference per source.
	_, err = pool.Exec(ctx, `UPDATE elitea_identity.credential_references SET state = 'staged' WHERE id = $1::uuid`, first.Record.ID)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr))
	require.Equal(t, "23505", pgErr.Code)
}

// The two-person rule compares user ids, not their spelling: "007" is user
// 7, recorded as 7, and user 7 cannot activate it under either spelling.
func TestCredentialReferenceTwoPersonRuleComparesCanonicalIDs(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	store := NewCredentialReferences(pool, agentIdentityAudit(t))
	staged, err := store.StageReference(ctx, StagedReference{
		TenantProjectID: 7, ProviderType: "jira", TargetIdentity: "https://acme.atlassian.net",
		Class: credentialgrant.ClassStaticSecret, Backend: "pgvault", Locator: "0123456789abcdef0123456789abcdef",
		Generation: 1, MigratedFrom: "configuration:88888888-8888-4888-8888-888888888888:pgvault", StagedBy: "007",
	}, newRefTestID)
	require.NoError(t, err)
	require.Equal(t, "7", staged.Record.StagedBy)
	for _, actor := range []string{"7", "007", " 7 "} {
		_, err = store.Transition(ctx, staged.Record.ID, CredentialReferenceActivate, actor)
		require.ErrorIs(t, err, ErrCredentialReferenceSelfActive, actor)
	}
	for _, actor := range []string{"alice", "-7", "0", "+8", "99999999999999999999"} {
		_, err = store.Transition(ctx, staged.Record.ID, CredentialReferenceActivate, actor)
		require.ErrorIs(t, err, ErrCredentialReferenceActor, actor)
	}
	_, err = store.StageReference(ctx, StagedReference{
		TenantProjectID: 7, ProviderType: "jira", TargetIdentity: "https://acme.atlassian.net",
		Class: credentialgrant.ClassStaticSecret, Backend: "pgvault", Locator: "x", Generation: 1,
		MigratedFrom: "mcp:1", StagedBy: "alice",
	}, newRefTestID)
	require.ErrorIs(t, err, ErrCredentialReferenceActor)
	active, err := store.Transition(ctx, staged.Record.ID, CredentialReferenceActivate, "008")
	require.NoError(t, err)
	require.Equal(t, "8", active.ActivatedBy)
	require.Equal(t, "8", active.LastTransitionBy)
}

// A --copy-to vault reference is staged at generation 0 and cannot be
// activated until its copy records the version it wrote.
func TestCredentialReferenceVaultCopyGenerationGatesActivation(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	store := NewCredentialReferences(pool, agentIdentityAudit(t))
	staged, err := store.StageReference(ctx, StagedReference{
		TenantProjectID: 7, ProviderType: "jira", TargetIdentity: "https://acme.atlassian.net",
		Class: credentialgrant.ClassStaticSecret, Backend: "vault", Locator: "elitea/projects/7/credentials/x",
		MigratedFrom: "configuration:99999999-9999-4999-8999-999999999999:vault", StagedBy: "100",
	}, newRefTestID)
	require.NoError(t, err)
	require.EqualValues(t, 0, staged.Record.Generation)
	_, err = store.Transition(ctx, staged.Record.ID, CredentialReferenceActivate, "200")
	require.ErrorIs(t, err, ErrCredentialReferenceIncomplete)

	copied, err := store.SetCopiedGeneration(ctx, staged.Record.ID, 3, "100")
	require.NoError(t, err)
	require.EqualValues(t, 3, copied.Generation)
	again, err := store.SetCopiedGeneration(ctx, staged.Record.ID, 3, "100")
	require.NoError(t, err, "a concurrent run that recorded the same version")
	require.EqualValues(t, 3, again.Generation)
	_, err = store.SetCopiedGeneration(ctx, staged.Record.ID, 4, "100")
	require.ErrorIs(t, err, ErrCredentialReferenceTransition, "a recorded version never moves")
	active, err := store.Transition(ctx, staged.Record.ID, CredentialReferenceActivate, "200")
	require.NoError(t, err)
	require.EqualValues(t, 3, active.Generation)
}

// A composite basic-auth key map in pgvault (#1481) is staged at generation
// 0 too, and takes version 1, the only version a pgvault secret has.
func TestCredentialReferencePGVaultKeyMapTakesVersionOne(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	store := NewCredentialReferences(pool, agentIdentityAudit(t))
	staged, err := store.StageReference(ctx, StagedReference{
		TenantProjectID: 7, ProviderType: "jira", TargetIdentity: "https://cloud.atlassian.net",
		Class: credentialgrant.ClassStaticSecret, Backend: "pgvault", Locator: "governed_99999999999949998999999999999999",
		MigratedFrom: "configuration:99999999-9999-4999-8999-999999999999:pgvault", StagedBy: "100",
	}, newRefTestID)
	require.NoError(t, err)
	_, err = store.SetCopiedGeneration(ctx, staged.Record.ID, 2, "100")
	require.ErrorIs(t, err, ErrCredentialReferenceTransition, "pgvault holds version 1 only")
	copied, err := store.SetCopiedGeneration(ctx, staged.Record.ID, 1, "100")
	require.NoError(t, err)
	require.EqualValues(t, 1, copied.Generation)
	require.NoError(t, store.RecordSecretCopy(ctx, 7, staged.Record.ID, newRefTestID(), "100", "pgvault"))
	require.Error(t, store.RecordSecretCopy(ctx, 7, staged.Record.ID, newRefTestID(), "100", "s3"))
}

// Concurrent stagings of one target identity all succeed and share one
// target binding. With `ON CONFLICT DO NOTHING ... UNION ALL SELECT` the
// loser's statement could see neither its own insert nor the winner's row.
func TestCredentialReferenceConcurrentStagingSharesOneTarget(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	store := NewCredentialReferences(pool, agentIdentityAudit(t))
	const workers, rounds = 8, 12
	for round := 0; round < rounds; round++ {
		identity := fmt.Sprintf("https://acme-%d.atlassian.net", round)
		var (
			start = make(chan struct{})
			wg    sync.WaitGroup
			errs  = make([]error, workers)
			ids   = make([]string, workers)
		)
		for worker := 0; worker < workers; worker++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				<-start
				result, err := store.StageReference(ctx, StagedReference{
					TenantProjectID: 7, ProviderType: "jira", TargetIdentity: identity,
					Class: credentialgrant.ClassOAuthDelegated, Backend: "pgvault", Locator: "oauth_grant",
					MigratedFrom: fmt.Sprintf("mcp:%d%02d", round+1, worker), StagedBy: "100",
				}, newRefTestID)
				errs[worker], ids[worker] = err, result.Record.TargetBindingID
			}(worker)
		}
		close(start)
		wg.Wait()
		for worker := 0; worker < workers; worker++ {
			require.NoError(t, errs[worker], "round %d worker %d", round, worker)
			require.Equal(t, ids[0], ids[worker], "round %d: one target binding per identity", round)
		}
		var targets int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_identity.target_bindings
WHERE tenant_project_id = 7 AND target_identity = $1`, identity).Scan(&targets))
		require.Equal(t, 1, targets)
	}
}
