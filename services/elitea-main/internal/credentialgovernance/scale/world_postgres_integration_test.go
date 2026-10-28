package scale

// The wiring of the governed scale measurements: a migrated database per
// run, and "replicas" that each hold their own pool and the production
// credential broker composition over it. Only the edges are fakes: the
// secret backend and the agent identity provider (the "fake provider"), and
// the two pre-existing platform gates (RBAC, entity access), which read the
// legacy project tables this fixture does not seed.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/fakeprovider"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/authzcompose"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/cache"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	scaleURLEnv    = "ELITEA_TEST_DATABASE_URL"
	scaleBootstrap = "../../infra/db/migrations/001_initial.sql"
	// replicaPoolSize is the main pool size of one Main replica (PR #998).
	replicaPoolSize = 16
	workloadID      = "spiffe://elitea.internal/runtime/scale-worker"
)

// newScaleDatabase creates a database from the migrated template and
// answers its connection config. The database is dropped at cleanup.
func newScaleDatabase(t *testing.T) *pgxpool.Config {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(scaleURLEnv))
	if databaseURL == "" {
		t.Fatalf("%s is required for the governed scale run", scaleURLEnv)
	}
	bootstrap, err := os.ReadFile(scaleBootstrap)
	require.NoError(t, err)
	ctx, cancel := dbtest.BuildContext(context.Background())
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	require.NoError(t, err)
	defer admin.Close()
	template, err := dbtest.EnsureTemplate(ctx, admin, dbtest.Spec{Files: platformmigrations.Files, Seed: string(bootstrap)})
	require.NoError(t, err)
	name := fmt.Sprintf("elitea_governed_scale_%d_%d", os.Getpid(), time.Now().UnixNano())
	require.NoError(t, dbtest.CreateFromTemplate(ctx, admin, template, name))
	config, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	config.ConnConfig.Database = name
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	return config
}

func openPool(t *testing.T, config *pgxpool.Config, maxConns int32) *pgxpool.Pool {
	t.Helper()
	c := config.Copy()
	c.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(context.Background(), c)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// allowPermissions and allowEntities stand for the RBAC and entity access
// gates: both read the legacy project, role and folder tables, which this
// fixture does not seed. Every other gate reads PostgreSQL.
type allowPermissions struct{}

func (allowPermissions) ResolvePermissions(_ context.Context, user auth.User, _ string, _ string) (auth.PermissionResolution, error) {
	id, _ := strconv.ParseInt(user.UserID, 10, 64)
	return auth.PermissionResolution{UserID: id, Permissions: credentialgrant.Permissions()}, nil
}

type allowEntities struct{}

func (allowEntities) AgentAccessible(context.Context, int64, int64, int64) (bool, error) {
	return true, nil
}

// fakeSecrets is the secret backend: an in-memory static secret, no
// network. It counts reads.
type fakeSecrets struct{ reads atomic.Int64 }

var scaleSecret = []byte(`{"token":"scale-token-never-logged","username":"scale@example.test"}`)

func (f *fakeSecrets) Read(_ context.Context, sc secretstore.StoreContext, _ secretstore.SecretRef, version int64) (secretstore.SecretVersion, error) {
	if err := sc.Validate(secretstore.OpRead); err != nil {
		return secretstore.SecretVersion{}, err
	}
	f.reads.Add(1)
	return secretstore.SecretVersion{Version: version, Value: secretstore.NewRuntimeSecret(append([]byte(nil), scaleSecret...))}, nil
}

func (f *fakeSecrets) Write(context.Context, secretstore.StoreContext, secretstore.SecretRef, secretstore.RuntimeSecret, int64) (int64, error) {
	panic("the broker never writes a secret")
}

func (f *fakeSecrets) Metadata(context.Context, secretstore.StoreContext, secretstore.SecretRef) (secretstore.Metadata, error) {
	panic("the broker never reads metadata")
}

func (f *fakeSecrets) Retire(context.Context, secretstore.StoreContext, secretstore.SecretRef, int64) error {
	panic("the broker never retires a secret")
}

// replica is one Main process: its own pool, invalidation hub, credential
// cache and broker.
type replica struct {
	name   string
	pool   *pgxpool.Pool
	hub    *epoch.Hub
	broker *credentialbroker.Broker
	audit  securityaudit.Appender
	// The writers an operator's revocation goes through on this replica.
	grants     *repos.CredentialGrants
	bindings   *repos.AgentIdentityBindings
	mandates   *repos.Mandates
	identities *repos.ExternalAgentIdentities
	custody    *delegationstore.Store
}

type replicaOptions struct {
	tokens credentialbroker.ProviderTokenSource
}

var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func newReplica(t *testing.T, config *pgxpool.Config, f *Fixture, name string, secrets *fakeSecrets, opts replicaOptions) *replica {
	t.Helper()
	pool := openPool(t, config, replicaPoolSize)
	audit, err := securityaudit.NewPostgresAppender("elitea-main/" + name)
	require.NoError(t, err)
	hub, err := epoch.NewHub(epoch.HubOptions{Logger: quietLogger})
	require.NoError(t, err)
	r := &replica{
		name: name, pool: pool, hub: hub, audit: audit,
		grants:     repos.NewCredentialGrants(pool, audit).WithNotifier(hub),
		bindings:   repos.NewAgentIdentityBindings(pool, audit).WithNotifier(hub),
		mandates:   repos.NewMandates(pool, audit).WithNotifier(hub),
		identities: repos.NewExternalAgentIdentities(pool, audit).WithNotifier(hub),
	}
	r.custody, err = delegationstore.New(pool, MasterKey(), audit)
	require.NoError(t, err)
	r.custody.WithNotifier(hub)
	governance, err := repos.NewCredentialGovernanceStore(pool)
	require.NoError(t, err)
	catalog, err := credentialadapters.NewBuiltinRegistry()
	require.NoError(t, err)

	decider, err := authzcompose.NewGovernedDecider(authzcompose.Dependencies{
		AuthSources: authzcompose.DefaultAuthSources{}, Permissions: allowPermissions{}, EntityAccess: allowEntities{},
		Bindings: r.bindings, Toolkits: r.grants, Grants: r.grants, References: r.grants, Operations: catalog,
		Mandates: r.mandates, Delegations: r.custody, Pins: governance,
	})
	require.NoError(t, err)
	registry, err := agentidentity.NewRegistry(fakeprovider.New(IdentityProvider))
	require.NoError(t, err)
	sessions, err := agentidentity.NewBroker(agentidentity.BrokerConfig{
		Registry: registry, Policy: agentidentity.DenyAssertedUser{ModePolicy: f.Revision.Document},
		Bindings: r.bindings, Identities: r.identities, Sessions: repos.NewAgentSessions(pool, audit),
	})
	require.NoError(t, err)
	store, err := credentialbroker.NewPostgresStore(pool, audit)
	require.NoError(t, err)
	epochs, err := epoch.NewPostgresStore(pool)
	require.NoError(t, err)
	settings := f.Revision.Document.CacheSettings()
	credentialCache, err := cache.New(cache.Limits{MaxEntries: settings.MaxEntries, MaxBytes: settings.MaxBytes, Skew: settings.Skew}, nil)
	require.NoError(t, err)
	cfg := credentialbroker.Config{
		Decider: decider, References: r.grants, Targets: r.grants, Toolkits: r.grants, Bindings: r.bindings,
		Mandates: r.mandates, Delegations: r.custody,
		SecretStores: map[string]secretstore.SecretStore{secretstore.BackendPGVault: secrets},
		Store:        store, Pins: governance, Sessions: sessions, Catalog: catalog,
		Epochs: epochs, Cache: credentialCache, Invalidations: hub, Revisions: governance,
		ProviderTokens: opts.tokens, Logger: quietLogger,
	}
	r.broker, err = credentialbroker.New(cfg)
	require.NoError(t, err)
	return r
}

// run is one admitted governed run the broker can redeem for.
type run struct {
	agent  *Agent
	claim  credentialbroker.ClaimedRun
	digest [32]byte
	// toolkits are the toolkit ids its context froze.
	toolkits []int64
	seq      atomic.Int64
}

// seedRuns writes, for each agent, the governed pin and the security
// context admission would have written: the agent's binding, mandate and
// delegation at their current epochs, the calling application, the
// version, and every toolkit's reference frozen. It uses the same encoder
// admission does (securitycontext.Context.Marshal).
func seedRuns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f *Fixture, agents []*Agent, label string) []*run {
	t.Helper()
	runs := make([]*run, 0, len(agents))
	var pins, contexts [][]any
	now := time.Now().UTC()
	for _, a := range agents {
		executionID := fmt.Sprintf("scale-%s-%d", label, a.Index)
		subject := strconv.FormatInt(a.SubjectUserID, 10)
		c := securitycontext.Context{
			PolicyRevisionID: f.Revision.ID, PolicyDigest: runtimedomain.Digest(f.Revision.Digest),
			TenantProjectID: a.TenantProjectID, SubjectPrincipalRef: subject,
			AgentBindingID: a.BindingID, MandateID: a.MandateID, DelegationID: a.DelegationID,
			Caller:           securitycontext.Caller{AuthenticationSource: "api_key", CallingApplicationID: a.CallingApplicationID},
			DefinitionDigest: runtimedomain.Digest(a.DefinitionDigest),
			ActorChain:       []securitycontext.ActorLink{{Kind: "user", Ref: subject}},
			IssuedAt:         now, ApplicationID: a.ApplicationID, ApplicationVersionID: a.ApplicationVersionID,
		}
		r := &run{agent: a}
		for _, tk := range a.Toolkits {
			c.CredentialReferences = append(c.CredentialReferences, securitycontext.CredentialReference{
				ToolkitID: tk.ID, CredentialReferenceID: tk.CredentialReferenceID, TargetBindingID: tk.TargetBindingID,
				Generation: tk.Generation, ToolkitDigest: runtimedomain.Digest(tk.Digest),
			})
			r.toolkits = append(r.toolkits, tk.ID)
		}
		encoded, ref, err := c.Marshal(executionID)
		require.NoError(t, err)
		r.digest = ref.Digest
		r.claim = credentialbroker.ClaimedRun{
			ExecutionID: executionID, Generation: 1, ClaimID: "claim-" + executionID, WorkloadID: workloadID,
			TenantProjectID: a.TenantProjectID, ActorID: subject, ContextBytes: encoded, ContextDigest: ref.Digest,
		}
		pins = append(pins, []any{executionID, "elitea.credential-profile.governed.v1", f.Revision.ID, f.Revision.Digest[:]})
		contexts = append(contexts, []any{executionID, ref.ContentID, ref.SchemaRevision, ref.Digest[:], encoded})
		runs = append(runs, r)
	}
	_, err := pool.CopyFrom(ctx, pgx.Identifier{"elitea_runtime", "execution_admission_profiles"},
		[]string{"execution_id", "profile", "policy_revision_id", "policy_digest"}, pgx.CopyFromRows(pins))
	require.NoError(t, err)
	_, err = pool.CopyFrom(ctx, pgx.Identifier{"elitea_runtime", "execution_security_contexts"},
		[]string{"execution_id", "content_id", "schema_revision", "digest", "context_bytes"}, pgx.CopyFromRows(contexts))
	require.NoError(t, err)
	return runs
}

// request is the next bound tool invocation of the run, on the toolkit
// picked round-robin. The worker names no reference: the broker takes the
// frozen one.
func (r *run) request() credentialbroker.RedeemRequest {
	n := r.seq.Add(1)
	toolkit := r.toolkits[int(n-1)%len(r.toolkits)]
	args := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", r.claim.ExecutionID, n)))
	return credentialbroker.RedeemRequest{
		Run: r.claim,
		Invocation: credentialbroker.BoundToolInvocation{
			InvocationID: uuid.Must(uuid.NewV7()).String(), ToolCallID: fmt.Sprintf("call-%d", n), ToolkitID: toolkit,
			OperationID: Operation, ResourceRef: "jira:issue/SCALE-" + strconv.FormatInt(n, 10), ArgumentsDigest: args,
		},
		SecurityContextDigest: r.digest,
	}
}

// outcome is one timed redemption.
type outcome struct {
	start, end time.Time
	permitted  bool
	code       string
	reason     string
	err        error
}

func redeem(ctx context.Context, b *credentialbroker.Broker, req credentialbroker.RedeemRequest) outcome {
	o := outcome{start: time.Now()}
	result, err := b.Redeem(ctx, req)
	o.end = time.Now()
	switch {
	case err != nil:
		o.err = err
	case result.Envelope != nil:
		o.permitted = true
		result.Envelope.Destroy()
	case result.Denial != nil:
		o.code, o.reason = string(result.Denial.Code), result.Denial.Reason()
	}
	return o
}

// sample picks n agents spread across every team: a stride through the
// fixture, skipping the agents another measurement owns.
func sample(f *Fixture, n int, taken map[int]bool) []*Agent {
	out := make([]*Agent, 0, n)
	stride := max(1, len(f.Agents)/max(n, 1))
	for offset := 0; len(out) < n && offset < stride; offset++ {
		for i := offset; i < len(f.Agents) && len(out) < n; i += stride {
			if !taken[i] {
				taken[i] = true
				out = append(out, &f.Agents[i])
			}
		}
	}
	return out
}

// teamsOf counts the distinct teams of a set of agents.
func teamsOf(agents []*Agent) int {
	seen := map[int64]bool{}
	for _, a := range agents {
		seen[a.TenantProjectID] = true
	}
	return len(seen)
}

// collector gathers samples from many goroutines.
type collector struct {
	mu      sync.Mutex
	samples []time.Duration
}

func (c *collector) add(d time.Duration) {
	c.mu.Lock()
	c.samples = append(c.samples, d)
	c.mu.Unlock()
}
