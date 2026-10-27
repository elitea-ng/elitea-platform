package control

// Issue #1121 over a real database: the one-statement epoch read across
// every source table, a mandate revocation cancelling a running governed
// execution at its next RenewLease (CANCELLED, durably), and a revoked
// binding refusing the governed claim. CI-only: skipped without the
// integration database.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	epochsURLEnv    = "ELITEA_TEST_DATABASE_URL"
	epochsBootstrap = "../../../infra/db/migrations/001_initial.sql"
	epochsRevision  = "rev-1121"
	epochsTenant    = int64(7)
	epochsExternal  = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1121"
)

var (
	epochsTemplateOnce sync.Once
	epochsTemplate     string
	epochsTemplateErr  error
)

func newEpochsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(epochsURLEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the governed epoch integration test", epochsURLEnv)
	}
	epochsTemplateOnce.Do(func() {
		bootstrap, err := os.ReadFile(epochsBootstrap)
		if err != nil {
			epochsTemplateErr = err
			return
		}
		ctx, cancel := dbtest.BuildContext(context.Background())
		defer cancel()
		admin, err := pgxpool.New(ctx, databaseURL)
		if err != nil {
			epochsTemplateErr = err
			return
		}
		defer admin.Close()
		epochsTemplate, epochsTemplateErr = dbtest.EnsureTemplate(ctx, admin, dbtest.Spec{
			Files: platformmigrations.Files, Seed: string(bootstrap),
		})
	})
	require.NoError(t, epochsTemplateErr)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	require.NoError(t, err)
	defer admin.Close()
	name := fmt.Sprintf("elitea_epochs_%d_%d", os.Getpid(), time.Now().UnixNano())
	require.NoError(t, dbtest.CreateFromTemplate(ctx, admin, epochsTemplate, name))
	config, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	return pool
}

type epochsAllowModes struct{}

func (epochsAllowModes) AllowsIdentityMode(agentidentity.IdentityMode) bool { return true }

type epochsTTL struct{}

func (epochsTTL) MaxMandateTTL() time.Duration { return 24 * time.Hour }

// epochsWorld is an active binding with an active autonomous mandate.
type epochsWorld struct {
	pool     *pgxpool.Pool
	bindings *repos.AgentIdentityBindings
	mandates *repos.Mandates
	hub      *epoch.Hub
	binding  agentidentity.AgentIdentityBinding
	mandate  mandate.Mandate
	changes  chan epoch.Change
}

func newEpochsWorld(t *testing.T) *epochsWorld {
	t.Helper()
	ctx := context.Background()
	pool := newEpochsPool(t)
	canonical := []byte(`{"schema":"elitea.credential-policy.v1"}`)
	digest := sha256.Sum256(canonical)
	_, err := pool.Exec(ctx, `
INSERT INTO elitea_identity.credential_policy_revisions (id, digest, canonical, document, created_by)
VALUES ($1, $2, $3, $4::jsonb, 'integration-test')`, epochsRevision, digest[:], canonical, string(canonical))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO elitea_identity.external_agent_identities (
    id, tenant_project_id, application_id, mode, provider, provider_tenant, kind, external_id, state,
    created_by, last_transition_by)
VALUES ($1::uuid, $2, 3, 'dedicated_agent', 'fake', 't1', 'fake.agent', 'obj-1121', 'active', '100', '100')`,
		epochsExternal, epochsTenant)
	require.NoError(t, err)

	audit, err := securityaudit.NewPostgresAppender("elitea-main/integration-test")
	require.NoError(t, err)
	hub, err := epoch.NewHub(epoch.HubOptions{})
	require.NoError(t, err)
	w := &epochsWorld{pool: pool, hub: hub, changes: make(chan epoch.Change, 16)}
	hub.Listen(func(c epoch.Change) { w.changes <- c })
	w.bindings = repos.NewAgentIdentityBindings(pool, audit).WithNotifier(hub)
	w.mandates = repos.NewMandates(pool, audit).WithNotifier(hub)

	now := time.Now().UTC().Truncate(time.Microsecond)
	proposed, err := agentidentity.NewBinding("0192f5e2-7c3a-7b1e-8a9d-000000001121", agentidentity.Proposal{
		TenantProjectID: epochsTenant, ApplicationID: 3, ApplicationVersionID: 11,
		DefinitionDigest: [32]byte{11}, Mode: agentidentity.IdentityModeDedicatedAgent,
		ExternalAgentIdentityID: epochsExternal,
	}, epochsAllowModes{}, epochsRevision, "100", now)
	require.NoError(t, err)
	_, err = w.bindings.CreateBinding(ctx, proposed)
	require.NoError(t, err)
	w.binding, err = w.bindings.TransitionBinding(ctx, proposed.ID, func(b agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
		return b.Transition(agentidentity.ActionApprove, "200", epochsRevision, time.Now().UTC())
	})
	require.NoError(t, err)
	<-w.changes // the approval's own signal

	m, err := mandate.New("0192f5e2-7c3a-7b1e-8a9d-000000002121", mandate.Request{
		TenantProjectID: epochsTenant, Kind: mandate.KindAutonomous, AgentBindingID: w.binding.ID,
		OperationScopes: []string{"github.issues.read"}, ExpiresAt: now.Add(time.Hour),
	}, epochsTTL{}, epochsRevision, "100", now)
	require.NoError(t, err)
	created, err := w.mandates.CreateMandate(ctx, m, func(agentidentity.AgentIdentityBinding, *mandate.Delegation) error { return nil })
	require.NoError(t, err)
	w.mandate, err = w.mandates.TransitionMandate(ctx, created.ID, w.mandateBy(mandate.ActionApprove))
	require.NoError(t, err)
	<-w.changes
	return w
}

func (w *epochsWorld) mandateBy(action mandate.Action) func(mandate.Mandate, agentidentity.AgentIdentityBinding) (mandate.Mandate, error) {
	return func(current mandate.Mandate, _ agentidentity.AgentIdentityBinding) (mandate.Mandate, error) {
		return current.Transition(action, "200", epochsRevision, time.Now().UTC())
	}
}

// storeContext stores the run's security context at the current epochs and
// its job, RUNNING.
func (w *epochsWorld) storeContext(t *testing.T, executionID string) {
	t.Helper()
	ctx := context.Background()
	encoded, ref, err := securitycontext.Context{
		PolicyRevisionID: epochsRevision, PolicyDigest: runtimedomain.SHA256([]byte("policy")),
		TenantProjectID: epochsTenant, SubjectPrincipalRef: "",
		AgentBindingID: w.binding.ID, AgentBindingEpoch: w.binding.RevocationEpoch,
		MandateID: w.mandate.ID, MandateEpoch: w.mandate.RevocationEpoch,
		DefinitionDigest: runtimedomain.Digest{11}, IssuedAt: time.Now().UTC().Truncate(time.Millisecond),
	}.Marshal(executionID)
	require.NoError(t, err)
	_, err = w.pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_security_contexts (execution_id, content_id, schema_revision, digest, context_bytes)
VALUES ($1, $2, $3, $4, $5)`, executionID, ref.ContentID, ref.SchemaRevision, ref.Digest[:], encoded)
	require.NoError(t, err)
	_, err = w.pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles (
    input_bundle_id, immutable_version, media_type, resource_project_id,
    manifest_digest, manifest_size, manifest_bytes, created_by
) VALUES ($1, '1', 'application/x-protobuf', 1, sha256('m'::bytea), 1, '\x00'::bytea, 'test')`, "bundle-"+executionID)
	require.NoError(t, err)
	_, err = w.pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state, admitted_at
) VALUES ($1, 1, 'cmd-' || $1, '1', 1, 1, '42', 'user:42', 'index.ingest.v1',
          '1', $2, sha256('r'::bytea), 'scope', 'key-' || $1, 'RUNNING', 'RUNNING', now())`,
		executionID, "bundle-"+executionID)
	require.NoError(t, err)
}

func (w *epochsWorld) server(t *testing.T, calls *[]string) *Server {
	t.Helper()
	store, err := epoch.NewPostgresStore(w.pool)
	require.NoError(t, err)
	contexts, err := epoch.NewPostgresContextLoader(w.pool)
	require.NoError(t, err)
	runs, err := epoch.NewRunChecker(contexts, store)
	require.NoError(t, err)
	revoker, err := epoch.NewPostgresAuthorityRevoker(w.pool)
	require.NoError(t, err)
	return withEpochs(t, governedTestServer(t, calls, nil), runs, revoker)
}

func (w *epochsWorld) desiredState(t *testing.T, executionID string) string {
	t.Helper()
	var desired string
	require.NoError(t, w.pool.QueryRow(context.Background(),
		`SELECT desired_state FROM elitea_runtime.execution_jobs WHERE execution_id = $1 AND generation = 1`, executionID).Scan(&desired))
	return desired
}

func TestEpochReadCoversEverySourceTableInOneStatement(t *testing.T) {
	w := newEpochsWorld(t)
	store, err := epoch.NewPostgresStore(w.pool)
	require.NoError(t, err)
	keys := []epoch.SecurityEpoch{
		{Kind: epoch.KindBinding, ID: w.binding.ID, Value: w.binding.RevocationEpoch},
		{Kind: epoch.KindMandate, ID: w.mandate.ID, Value: w.mandate.RevocationEpoch},
		{Kind: epoch.KindExternalIdentity, ID: epochsExternal, Value: epoch.Unpinned},
		{Kind: epoch.KindDelegation, ID: "0192f5e2-7c3a-7b1e-8a9d-00000000dead", Value: 0},
		{Kind: epoch.KindGrant, ID: "0192f5e2-7c3a-7b1e-8a9d-00000000beef", Value: 0},
		{Kind: epoch.KindCredentialRef, ID: "0192f5e2-7c3a-7b1e-8a9d-00000000cafe", Value: 0},
	}
	current, err := store.Read(context.Background(), keys)
	require.NoError(t, err)
	require.Equal(t, []int64{w.binding.RevocationEpoch, w.mandate.RevocationEpoch, 0, epoch.Revoked, epoch.Revoked, epoch.Revoked},
		[]int64{current[0].Value, current[1].Value, current[2].Value, current[3].Value, current[4].Value, current[5].Value})
	stale, err := store.Current(context.Background(), keys)
	require.NoError(t, err)
	// Missing rows are stale whatever their epoch.
	require.Equal(t, keys[3:], stale)

	// A suspended binding is stale at its old epoch and at its new one.
	suspended, err := w.bindings.TransitionBinding(context.Background(), w.binding.ID, func(b agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
		return b.Transition(agentidentity.ActionSuspend, "200", epochsRevision, time.Now().UTC())
	})
	require.NoError(t, err)
	for _, value := range []int64{w.binding.RevocationEpoch, suspended.RevocationEpoch} {
		stale, err = store.Current(context.Background(), []epoch.SecurityEpoch{{Kind: epoch.KindBinding, ID: w.binding.ID, Value: value}})
		require.NoError(t, err)
		require.Len(t, stale, 1)
	}
}

// Revoking the mandate cancels the running governed execution at its next
// RenewLease: desired CANCELLED on the wire and in the job row. The change
// reached the hub after commit, by reference.
func TestMandateRevocationCancelsAtTheNextRenewLease(t *testing.T) {
	w := newEpochsWorld(t)
	lease := validLease()
	w.storeContext(t, lease.Fence.ExecutionID)
	calls := []string{}
	server := w.server(t, &calls)

	response, err := server.RenewLease(context.Background(), renewRequestFor(t))
	require.NoError(t, err)
	require.Nil(t, response.GetRejection())
	require.Equal(t, runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_RUNNING, response.GetDesiredState())

	revokedAt := time.Now()
	revoked, err := w.mandates.TransitionMandate(context.Background(), w.mandate.ID, w.mandateBy(mandate.ActionRevoke))
	require.NoError(t, err)
	change := <-w.changes
	require.Equal(t, epoch.SecurityEpoch{Kind: epoch.KindMandate, ID: w.mandate.ID, Value: revoked.RevocationEpoch}, change.Epoch)
	require.False(t, change.ChangedAt.Before(revokedAt.Add(-time.Second)))

	response, err = server.RenewLease(context.Background(), renewRequestFor(t))
	require.NoError(t, err)
	require.Nil(t, response.GetRejection())
	require.Equal(t, runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_CANCELLED, response.GetDesiredState())
	require.Equal(t, "CANCELLED", w.desiredState(t, lease.Fence.ExecutionID))
}

// A governed claim whose binding was revoked after admission is refused.
func TestGovernedClaimWithAStaleBindingEpochIsRejectedOverPostgres(t *testing.T) {
	w := newEpochsWorld(t)
	request := governedClaimRequest(t, nil, true)
	command := &runtimev1.WorkerCommandV1{}
	require.NoError(t, proto.Unmarshal(request.GetSignedCommand().GetWorkerCommandBytes(), command))
	w.storeContext(t, command.GetExecutionId())
	_, err := w.bindings.TransitionBinding(context.Background(), w.binding.ID, func(b agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
		return b.Transition(agentidentity.ActionRevoke, "200", epochsRevision, time.Now().UTC())
	})
	require.NoError(t, err)
	// The binding and its cascaded mandate both signalled.
	kinds := map[epoch.Kind]bool{}
	for range 2 {
		kinds[(<-w.changes).Epoch.Kind] = true
	}
	require.True(t, kinds[epoch.KindBinding] && kinds[epoch.KindMandate], "%v", kinds)

	calls := []string{}
	response, err := w.server(t, &calls).ClaimCommand(context.Background(), request)
	require.Equal(t, runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED, rejectionCode(t, response, err))
	require.False(t, claimed(calls), "%v", calls)
	require.Equal(t, "CANCELLED", w.desiredState(t, command.GetExecutionId()))
}
