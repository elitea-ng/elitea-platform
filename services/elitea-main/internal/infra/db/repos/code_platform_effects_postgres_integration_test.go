package repos

// Real-PostgreSQL evidence for the Code platform journal "effects" question.
//
// Boundary crossed: service integration (real PostgreSQL, real migrations, real
// claim/fence/lease SQL in CodeIntentRepository.lockAccess, real journal SQL).
// The immutable visit/intent record decode (saved source, YAML policy and
// canonical visit digests) is NOT rebuilt here: the authorization-matrix
// consumer below keeps the production callback shape of
// WithRegisteredCodePlatformIntent (lockAccess first, the registered intent
// inside the transaction, lockAccess again after apply) but takes the intent
// from a fixture. The production consumer is exercised for its refusals.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	recoveryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/noderecovery"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	effectsWorkload = "spiffe://elitea.test/worker/native"
	effectsParent   = "0123456789abcdef0123456789abcdef"
	effectsForeign  = "fedcba9876543210fedcba9876543210"
	// Claim ids are 32 lowercase hex characters, like production claims.
	effectsClaim1 = "00000000000000000000000000000001"
	effectsClaim2 = "00000000000000000000000000000002"
	effectsClaim3 = "00000000000000000000000000000003"
	effectsClaim4 = "00000000000000000000000000000004"
	effectsClaim5 = "00000000000000000000000000000005"
)

func effectsHex(c string) string { return strings.Repeat(c, 64) }

// effectID is a distinct lowercase-hex effect identifier per call row.
func effectID(n int) string { return fmt.Sprintf("%064x", n) }

type effectsWorld struct {
	t        *testing.T
	pool     *pgxpool.Pool
	journal  *CodePlatformCallsRepository
	projects int64
}

func newEffectsWorld(t *testing.T) *effectsWorld {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	journal, err := NewCodePlatformCallsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	return &effectsWorld{t: t, pool: pool, journal: journal}
}

func (w *effectsWorld) exec(query string, args ...any) {
	w.t.Helper()
	if _, err := w.pool.Exec(w.t.Context(), query, args...); err != nil {
		w.t.Fatalf("%s: %v", strings.Fields(query)[0:3], err)
	}
}

func (w *effectsWorld) project() int64 {
	w.t.Helper()
	w.projects++
	id := 7000 + w.projects
	w.exec(`INSERT INTO centry.project (id, create_success, suspended) VALUES ($1::int, TRUE, FALSE)`, id)
	return id
}

// job inserts one execution_jobs row. Parents run the Code node; children are toolkit runs.
func (w *effectsWorld) job(execution string, project int64, state string) {
	w.t.Helper()
	bundle := "bundle-" + execution
	w.exec(`INSERT INTO elitea_runtime.input_bundles (input_bundle_id, immutable_version, media_type, resource_project_id, manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ($1, '1', 'application/x-protobuf', $2::int, sha256('m'::bytea), 1, 'm'::bytea, 'tests')`, bundle, project)
	w.exec(`INSERT INTO elitea_runtime.input_bundle_entries (input_bundle_id, entry_id, entry_version, semantic_role, media_type, content_digest, content_size, content_reference, classification, required_grant_audience, content_bytes)
VALUES ($1, 'request', '1', 'agent.execution_request', 'application/json', sha256('{}'::bytea), 2, 'inline://request', 'internal', 'worker', '{}'::bytea)`, bundle)
	w.exec(`INSERT INTO elitea_runtime.execution_jobs (execution_id, generation, command_id, tenant_id, resource_project_id, projection_project_id, actor_id, principal_ref, capability_id, capability_version, input_bundle_id, request_digest, idempotency_scope, idempotency_key, state, desired_state, invocation_state, admitted_at)
VALUES ($1, 1, 'command-'||$1, ($2::int)::text, $2::int, $2::int, '11', 'user:11', 'agent.execute.application.v1', '1', $3, sha256(convert_to($1::text,'UTF8')), 'effects-'||$1, $1, $4, 'RUNNING', 'MAY_HAVE_STARTED', clock_timestamp())`, execution, project, bundle, state)
}

// access makes the execution claimable by one worker and returns its claim.
func (w *effectsWorld) access(execution string, project int64, claimID string, attempt int, fence byte) storage.ContentClaim {
	w.t.Helper()
	w.exec(`INSERT INTO elitea_runtime.workload_sessions (workload_session_id, workload_identity, producer_id, expires_at) VALUES ('session-'||$1, $2, 'producer', clock_timestamp()+interval '10 minutes')`, claimID, effectsWorkload)
	fenceBytes := bytes.Repeat([]byte{fence}, 32)
	w.exec(`INSERT INTO elitea_runtime.execution_claims (claim_id, execution_id, generation, workload_session_id, workload_identity, producer_id, claim_attempt, lease_epoch, fence_token, claimed_at, lease_expires_at)
VALUES ($1, $2, 1, 'session-'||$1, $3, 'producer', $4, $4, $5, clock_timestamp(), clock_timestamp()+interval '5 minutes')`, claimID, execution, effectsWorkload, attempt, fenceBytes)
	identity, _ := url.Parse(effectsWorkload)
	return storage.ContentClaim{PeerCertificate: &x509.Certificate{URIs: []*url.URL{identity}}, ExecutionID: execution, Generation: 1, ClaimID: claimID, FenceToken: fenceBytes}
}

func (w *effectsWorld) admit(execution string) {
	w.t.Helper()
	w.exec(`INSERT INTO elitea_runtime.command_outbox (outbox_id, execution_id, generation, stream_name, resource_class, isolation_class, priority, deadline, limits_revision,
 prepared_signed_envelope_bytes, prepared_signed_envelope_digest, prepared_signature_profile, prepared_key_id, prepared_at, published_at, published_envelope_digest, authority_granted_at)
VALUES ('outbox-'||$1, $1, 1, 'commands', 'agent', 'project', 1, clock_timestamp()+interval '1 hour', 'limits', 'e'::bytea, sha256('e'::bytea), 1, 'key', clock_timestamp(), clock_timestamp(), sha256('e'::bytea), clock_timestamp())`, execution)
	w.exec(`INSERT INTO elitea_runtime.agent_execution_jobs (execution_id, generation, capability_id, input_bundle_id, request_entry_id, client_stream_id, client_message_id, client_execution_generation, sio_event)
VALUES ($1, 1, 'agent.execute.application.v1', 'bundle-'||$1, 'request', gen_random_uuid()::text, gen_random_uuid()::text, '1', 'chat_predict')`, execution)
}

// registration pins one broker job (and, for the matrix, the immutable visit and broker rows).
func (w *effectsWorld) registration(execution, activation, prepared string, project int64) {
	w.t.Helper()
	w.exec(`INSERT INTO elitea_runtime.code_platform_jobs (tenant_id, execution_id, original_generation, activation_sha256, prepared_sha256, policy_sha256, retained_runtime_id, project_id, actor_id, max_calls, max_total_bytes)
VALUES (($1::int)::text, $2, 1, decode($3,'hex'), decode($4,'hex'), decode($5,'hex'), 'runtime-1', $1::int, 11, 8, 65536)`, project, execution, activation, prepared, effectsHex("9"))
}

// call inserts one journal row. Prepared rows carry no dispatch authority.
func (w *effectsWorld) call(execution, activation string, sequence int, state, effect, claimID string) {
	w.t.Helper()
	columns := `execution_id, activation_sha256, sequence, operation, effect_id, call_sha256, resource_sha256, arguments_sha256, payload_sha256, frame_sha256, frame_bytes, intent_ref, state`
	values := `$1, decode($2,'hex'), $3, 'user_get', $4, sha256('c'::bytea), sha256('r'::bytea), sha256('a'::bytea), sha256('p'::bytea), sha256('f'::bytea), 64, $4||'.cp1', $5`
	args := []any{execution, activation, sequence, effect, state}
	if state != "prepared" {
		columns += `, dispatch_claim_id, dispatch_generation, dispatch_lease_epoch`
		values += `, $6, 1, 1`
		args = append(args, claimID)
	}
	if state == "committed" {
		columns += `, response_ref, response_sha256, response_bytes, resolved_at`
		values += `, $4||'.cp1', sha256('x'::bytea), 8, clock_timestamp()`
	}
	w.exec(`INSERT INTO elitea_runtime.code_platform_calls (`+columns+`) VALUES (`+values+`)`, args...)
}

func (w *effectsWorld) toolkitChild(effect, child, childState string, project int64) {
	w.t.Helper()
	w.job(child, project, childState)
	w.exec(`INSERT INTO elitea_runtime.code_platform_toolkit_children (effect_id, child_execution_id, child_generation, toolkit_id, toolkit_revision, arguments_sha256)
VALUES ($1, $2, 1, 5, $3, sha256('a'::bytea))`, effect, child, effectsHex("5"))
}

func (w *effectsWorld) read(execution string, generation uint64, dispatch, fingerprint string) (bool, error) {
	w.t.Helper()
	tx, err := w.pool.Begin(w.t.Context())
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	return w.journal.ReadOriginalCodePlatformUnresolvedEffects(w.t.Context(), tx, execution, generation, dispatch, fingerprint)
}

func TestCodePlatformUnresolvedEffectsJournalPostgres(t *testing.T) {
	w := newEffectsWorld(t)
	project := w.project()
	w.job(effectsParent, project, "RUNNING")
	prepared := effectsHex("7")
	// One activation per case: a job has at most one in-flight call.
	cases := []struct {
		name     string
		activate string
		calls    func(activation string)
		want     bool
	}{
		{"registered job without any call", "a1", func(string) {}, false},
		{"prepared only: Dispatch never granted", "a2", func(a string) { w.call(effectsParent, a, 1, "prepared", effectID(2), "") }, false},
		{"dispatching", "a3", func(a string) { w.call(effectsParent, a, 1, "dispatching", effectID(3), effectsClaim1) }, true},
		{"uncertain", "a4", func(a string) { w.call(effectsParent, a, 1, "uncertain", effectID(4), effectsClaim1) }, true},
		{"committed only", "a5", func(a string) { w.call(effectsParent, a, 1, "committed", effectID(5), effectsClaim1) }, false},
		{"committed then uncertain", "a6", func(a string) {
			w.call(effectsParent, a, 1, "committed", effectID(60), effectsClaim1)
			w.call(effectsParent, a, 2, "uncertain", effectID(61), effectsClaim1)
		}, true},
		{"committed then prepared", "a7", func(a string) {
			w.call(effectsParent, a, 1, "committed", effectID(70), effectsClaim1)
			w.call(effectsParent, a, 2, "prepared", effectID(71), "")
		}, false},
	}
	// Dispatch authority rows reference a real claim.
	w.access(effectsParent, project, effectsClaim1, 1, 7)
	for _, tc := range cases {
		activation := strings.Repeat(tc.activate, 32)
		w.registration(effectsParent, activation, prepared, project)
		tc.calls(activation)
		got, err := w.read(effectsParent, 1, activation, prepared)
		if err != nil || got != tc.want {
			t.Errorf("%s: possible=%v err=%v, want %v", tc.name, got, err, tc.want)
		}
	}

	t.Run("committed call with a non-terminal toolkit child", func(t *testing.T) {
		for i, state := range []string{"PENDING", "DISPATCHED", "CLAIMED", "RUNNING", "SETTLING", "SUCCEEDED", "FAILED", "CANCELLED"} {
			activation := strings.Repeat(fmt.Sprintf("b%d", i), 32)
			effect := effectID(100 + i)
			w.registration(effectsParent, activation, prepared, project)
			w.call(effectsParent, activation, 1, "committed", effect, effectsClaim1)
			w.toolkitChild(effect, fmt.Sprintf("toolkit-child-%d", i), state, project)
			want := state != "SUCCEEDED" && state != "FAILED" && state != "CANCELLED"
			got, err := w.read(effectsParent, 1, activation, prepared)
			if err != nil || got != want {
				t.Errorf("child %s: possible=%v err=%v, want %v", state, got, err, want)
			}
		}
	})

	t.Run("an unresolved call of this job is not visible to another selector", func(t *testing.T) {
		activation := strings.Repeat("a3", 32)
		for name, selector := range map[string]struct {
			execution   string
			generation  uint64
			dispatch    string
			fingerprint string
		}{
			"foreign execution":        {effectsForeign, 1, activation, prepared},
			"other generation":         {effectsParent, 2, activation, prepared},
			"unrelated activation":     {effectsParent, 1, effectsHex("f"), prepared},
			"other prepared":           {effectsParent, 1, activation, effectsHex("8")},
			"sibling activation clean": {effectsParent, 1, strings.Repeat("a1", 32), prepared},
		} {
			got, err := w.read(selector.execution, selector.generation, selector.dispatch, selector.fingerprint)
			if err != nil || got {
				t.Errorf("%s: possible=%v err=%v", name, got, err)
			}
		}
	})

	t.Run("invalid selectors are errors, never none", func(t *testing.T) {
		for name, selector := range map[string]struct {
			execution  string
			generation uint64
			dispatch   string
			prepared   string
		}{
			"empty execution": {"", 1, effectsHex("1"), prepared},
			"zero generation": {effectsParent, 0, effectsHex("1"), prepared},
			"zero dispatch":   {effectsParent, 1, effectsHex("0"), prepared},
			"short prepared":  {effectsParent, 1, effectsHex("1"), "abc"},
		} {
			if got, err := w.read(selector.execution, selector.generation, selector.dispatch, selector.prepared); err == nil || got || !errors.Is(err, domain.ErrConflict) {
				t.Errorf("%s: possible=%v err=%v", name, got, err)
			}
		}
		if _, err := (*CodePlatformCallsRepository)(nil).ReadOriginalCodePlatformUnresolvedEffects(t.Context(), nil, effectsParent, 1, effectsHex("1"), prepared); err == nil {
			t.Error("nil repository answered")
		}
	})

	t.Run("the read is plain: it writes nothing", func(t *testing.T) {
		snapshot := func() string {
			var s string
			if err := w.pool.QueryRow(t.Context(), `SELECT md5(coalesce((SELECT string_agg(c::text,'|' ORDER BY execution_id,activation_sha256,sequence) FROM elitea_runtime.code_platform_calls c),'')||
coalesce((SELECT string_agg(j::text,'|' ORDER BY execution_id,activation_sha256) FROM elitea_runtime.code_platform_jobs j),''))`).Scan(&s); err != nil {
				t.Fatal(err)
			}
			return s
		}
		before := snapshot()
		for i := 0; i < 3; i++ {
			if _, err := w.read(effectsParent, 1, strings.Repeat("a3", 32), prepared); err != nil {
				t.Fatal(err)
			}
		}
		if snapshot() != before {
			t.Fatal("effects read changed the journal")
		}
	})
}

// effectsConsumer keeps the production callback shape of
// CodeIntentRepository.WithRegisteredCodePlatformIntent: real lockAccess first,
// the registered intent in the same serializable transaction, real lockAccess
// again after apply. Only the immutable intent record comes from a fixture.
type effectsConsumer struct {
	repo   *CodeIntentRepository
	intent storage.RegisteredCodeIntent
}

func (c *effectsConsumer) WithRegisteredCodePlatformIntent(ctx context.Context, claim storage.ContentClaim, dispatch, fingerprint string, apply func(context.Context, storage.CodeTransaction, storage.RegisteredCodeIntent) error) error {
	if !code.NonzeroDigest(dispatch) || !code.NonzeroDigest(fingerprint) {
		return code.ErrRejected
	}
	return c.repo.shared.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		current, err := c.repo.lockAccess(ctx, tx, claim, "RUNNING")
		if err != nil {
			return err
		}
		intent := c.intent
		if claim.ExecutionID != intent.Original.ExecutionID || dispatch != intent.Binding.DispatchActivation || fingerprint != intent.PreparedFingerprint {
			return code.ErrRejected
		}
		intent.Access = storage.CodeIntentAccess{ClaimID: claim.ClaimID, ClaimAttempt: current.attempt, LeaseEpoch: current.epoch}
		if err = apply(ctx, codeIntentTransaction{tx}, intent); err != nil {
			return err
		}
		latest, err := c.repo.lockAccess(ctx, tx, claim, "RUNNING")
		if err != nil || latest.attempt != intent.Access.ClaimAttempt || latest.epoch != intent.Access.LeaseEpoch {
			return storage.ErrContentUnauthorized
		}
		return nil
	})
}

type countingEffects struct {
	storage.CodePlatformUnresolvedEffectReader
	calls int
}

func (c *countingEffects) ReadOriginalCodePlatformUnresolvedEffects(ctx context.Context, tx storage.CodeTransaction, execution string, generation uint64, dispatch, fingerprint string) (bool, error) {
	c.calls++
	return c.CodePlatformUnresolvedEffectReader.ReadOriginalCodePlatformUnresolvedEffects(ctx, tx, execution, generation, dispatch, fingerprint)
}

type noEffectsCallbacks struct{}

func (noEffectsCallbacks) ReadRetainedRuntime(context.Context, code.PlatformGrantClaims, code.SignedGrant) (code.RetainedRuntime, error) {
	return code.RetainedRuntime{}, domain.ErrUnavailable
}
func (noEffectsCallbacks) ReadPendingPlatformCall(context.Context, code.PlatformGrantClaims, code.SignedGrant) (code.RetainedRuntime, []byte, error) {
	return code.RetainedRuntime{}, nil, domain.ErrUnavailable
}
func (noEffectsCallbacks) PublishCommittedPlatformReply(context.Context, code.PlatformGrantClaims, code.SignedGrant, []byte) (code.RetainedRuntime, error) {
	return code.RetainedRuntime{}, domain.ErrUnavailable
}
func (noEffectsCallbacks) SignPlatform(code.PlatformGrantClaims) (code.SignedGrant, error) {
	return code.SignedGrant{}, domain.ErrUnavailable
}
func (noEffectsCallbacks) ForCodePlatformCall(storage.ContentClaim, domain.Admission) (storage.CodePlatformCallService, error) {
	return nil, domain.ErrUnavailable
}

func TestCodePlatformEffectsAuthorizationMatrixPostgres(t *testing.T) {
	w := newEffectsWorld(t)
	allow := true
	repo := &CodeIntentRepository{permissions: func(context.Context, sqlExecutor, recoveryapp.Selector, string) error {
		if !allow {
			return errors.New("permission lost")
		}
		return nil
	}}
	var err error
	if repo.shared, err = newPostgresSharedStore(w.pool); err != nil {
		t.Fatal(err)
	}
	project := w.project()
	foreignProject := w.project()

	dispatch, prepared := effectsHex("2"), effectsHex("7")
	for _, execution := range []struct {
		id      string
		project int64
		claim   string
		fence   byte
	}{{effectsParent, project, effectsClaim2, 3}, {effectsForeign, foreignProject, effectsClaim3, 4}} {
		w.job(execution.id, execution.project, "RUNNING")
		w.admit(execution.id)
		w.registration(execution.id, dispatch, prepared, execution.project)
	}
	claim := w.access(effectsParent, project, effectsClaim2, 1, 3)
	foreignClaim := w.access(effectsForeign, foreignProject, effectsClaim3, 1, 4)
	// Both journals hold a different truth: the parent is clean, the foreign one is unknown.
	w.call(effectsForeign, dispatch, 1, "uncertain", effectID(200), effectsClaim3)
	// Registered broker rows for the parent (immutable visit + broker binding).
	visit := effectsHex("a")
	w.exec(`INSERT INTO elitea_runtime.original_code_visits (execution_id, generation, visit_id, visit_digest, activation_id, attempt, record_json, registered_claim_id) VALUES ($1,1,$2,$3,$4,1,'{}'::bytea,'00000000000000000000000000000002')`, effectsParent, visit, effectsHex("c"), effectsHex("1"))
	brokerJSON, _ := json.Marshal(storage.CodePreparedPlatformClient{Revision: 1, PolicySHA256: effectsHex("9"), MaxCalls: 8, MaxTotalBytes: 65536})
	w.exec(`INSERT INTO elitea_runtime.original_code_broker_bindings (execution_id, original_generation, visit_id, prepared_sha256, prepared_fingerprint, policy_sha256, dependency_bundle_sha256, broker_json) VALUES ($1,1,$2,$3,$4,$5,'',$6)`, effectsParent, visit, effectsHex("5"), prepared, effectsHex("9"), brokerJSON)

	intent := storage.RegisteredCodeIntent{
		Binding:             code.Binding{Schema: "elitea.sandbox.whole-code-binding.v1", Purpose: "whole_code_execute", ExecutionID: effectsParent, OriginalGeneration: 1, ActivationID: effectsHex("1"), NodeID: "run", GraphThread: "root", Step: 1, Attempt: 1, DispatchActivation: dispatch, JobKey: code.JobKey(effectsParent, dispatch), RequestDigest: prepared, SupervisorAudience: "spiffe://elitea/supervisor/one", NodeDigest: effectsHex("4"), Language: "python", PreparedSHA256: effectsHex("5"), SourceSHA256: effectsHex("6"), InputSHA256: effectsHex("7")},
		PreparedFingerprint: prepared,
		Original:            storage.OriginalCodeVisit{TenantID: fmt.Sprint(project), ResourceProjectID: project, ExecutionID: effectsParent, OriginalGeneration: 1, Reference: code.OriginalVisitRef{VisitID: visit, Revision: 1, DigestSHA256: effectsHex("c")}},
	}
	intent.Original.Declaration.PlatformClient = true
	reader := &countingEffects{CodePlatformUnresolvedEffectReader: w.journal}
	noop := noEffectsCallbacks{}
	pump, err := storage.NewCodePlatformPump(&effectsConsumer{repo: repo, intent: intent}, noop, noop, noop, w.journal, "spiffe://elitea.test/main/one")
	if err != nil {
		t.Fatal(err)
	}
	pump.WithEffectJournal(reader)
	ask := func(c storage.ContentClaim, d, p string) (bool, error) {
		reader.calls = 0
		return pump.Effects(t.Context(), c, d, p)
	}
	mustRefuse := func(name string, c storage.ContentClaim, d, p string, want error) {
		t.Helper()
		possible, err := ask(c, d, p)
		if !errors.Is(err, want) || possible || reader.calls != 0 {
			t.Errorf("%s: possible=%v err=%v reads=%d, want %v", name, possible, err, reader.calls, want)
		}
	}

	// Positive control: the right claim reads its own, clean journal.
	if possible, err := ask(claim, dispatch, prepared); err != nil || possible {
		t.Fatalf("right claim: possible=%v err=%v", possible, err)
	}
	if reader.calls != 1 {
		t.Fatalf("reader calls %d", reader.calls)
	}
	// The same claim sees "possible" once its own journal holds an unknown outcome.
	w.call(effectsParent, dispatch, 1, "dispatching", effectID(201), effectsClaim2)
	if possible, err := ask(claim, dispatch, prepared); err != nil || !possible {
		t.Fatalf("own unresolved journal: possible=%v err=%v", possible, err)
	}

	mutate := func(f func(*storage.ContentClaim)) storage.ContentClaim {
		c := claim
		c.FenceToken = bytes.Clone(claim.FenceToken)
		f(&c)
		return c
	}
	wrongPeer, _ := url.Parse("spiffe://elitea.test/worker/other")
	for name, c := range map[string]storage.ContentClaim{
		"wrong fence":                  mutate(func(c *storage.ContentClaim) { c.FenceToken = bytes.Repeat([]byte{9}, 32) }),
		"foreign fence of other claim": mutate(func(c *storage.ContentClaim) { c.FenceToken = foreignClaim.FenceToken }),
		"foreign claim id":             mutate(func(c *storage.ContentClaim) { c.ClaimID = foreignClaim.ClaimID }),
		"unknown claim id":             mutate(func(c *storage.ContentClaim) { c.ClaimID = effectsClaim5 }),
		"wrong generation":             mutate(func(c *storage.ContentClaim) { c.Generation = 2 }),
		"other workload identity":      mutate(func(c *storage.ContentClaim) { c.PeerCertificate = &x509.Certificate{URIs: []*url.URL{wrongPeer}} }),
		"no peer certificate":          mutate(func(c *storage.ContentClaim) { c.PeerCertificate = nil }),
		"foreign execution":            foreignClaim,
	} {
		t.Run(name, func(t *testing.T) {
			// A claim of another execution or project must never read this journal. The
			// foreign execution has its own claim, so it is refused by the intent/scope check.
			want := storage.ErrContentUnauthorized
			if name == "foreign execution" {
				want = code.ErrRejected
			}
			if name == "no peer certificate" {
				want = domain.ErrUnauthorized
			}
			mustRefuse(name, c, dispatch, prepared, want)
		})
	}
	// The foreign execution's unknown journal never leaks into a claim for this execution.
	if possible, err := ask(foreignClaim, dispatch, prepared); possible || err == nil {
		t.Fatalf("foreign claim read the parent's job: possible=%v err=%v", possible, err)
	}

	t.Run("unrelated dispatch activation or prepared fingerprint", func(t *testing.T) {
		mustRefuse("dispatch", claim, effectsHex("f"), prepared, code.ErrRejected)
		mustRefuse("prepared", claim, dispatch, effectsHex("f"), code.ErrRejected)
	})

	t.Run("revoked authority", func(t *testing.T) {
		allow = false
		mustRefuse("actor lost permission", claim, dispatch, prepared, storage.ErrContentUnauthorized)
		allow = true
		w.exec(`UPDATE centry.project SET suspended = TRUE WHERE id = $1`, project)
		mustRefuse("suspended project", claim, dispatch, prepared, storage.ErrContentUnauthorized)
		w.exec(`UPDATE centry.project SET suspended = FALSE WHERE id = $1`, project)
		w.exec(`UPDATE elitea_runtime.workload_sessions SET revoked_at = clock_timestamp() WHERE workload_session_id = 'session-00000000000000000000000000000002'`)
		mustRefuse("revoked workload session", claim, dispatch, prepared, storage.ErrContentUnauthorized)
		w.exec(`UPDATE elitea_runtime.workload_sessions SET revoked_at = NULL WHERE workload_session_id = 'session-00000000000000000000000000000002'`)
		if possible, err := ask(claim, dispatch, prepared); err != nil || !possible {
			t.Fatalf("authority restored: possible=%v err=%v", possible, err)
		}
	})

	t.Run("stale lease and replaced claim", func(t *testing.T) {
		w.exec(`UPDATE elitea_runtime.execution_claims SET claimed_at = clock_timestamp()-interval '10 minutes', lease_expires_at = clock_timestamp()-interval '1 second' WHERE claim_id = '00000000000000000000000000000002'`)
		mustRefuse("expired lease", claim, dispatch, prepared, storage.ErrContentUnauthorized)
		// Takeover: the old claim is released and a higher attempt owns the execution.
		w.exec(`UPDATE elitea_runtime.execution_claims SET released_at = clock_timestamp(), release_reason = 'takeover' WHERE claim_id = '00000000000000000000000000000002'`)
		replacement := w.access(effectsParent, project, effectsClaim4, 2, 5)
		mustRefuse("released stale claim", claim, dispatch, prepared, storage.ErrContentUnauthorized)
		if possible, err := ask(replacement, dispatch, prepared); err != nil || !possible {
			t.Fatalf("replacement claim: possible=%v err=%v", possible, err)
		}
	})

	t.Run("execution no longer running", func(t *testing.T) {
		w.exec(`UPDATE elitea_runtime.execution_jobs SET desired_state = 'CANCELLED' WHERE execution_id = $1`, effectsParent)
		replacement := storage.ContentClaim{PeerCertificate: claim.PeerCertificate, ExecutionID: effectsParent, Generation: 1, ClaimID: effectsClaim4, FenceToken: bytes.Repeat([]byte{5}, 32)}
		mustRefuse("cancelled execution", replacement, dispatch, prepared, storage.ErrContentUnauthorized)
	})

	// The production consumer refuses everything above without a registered intent.
	t.Run("production consumer refuses before any read", func(t *testing.T) {
		production := &CodeIntentRepository{shared: repo.shared, permissions: repo.permissions}
		called := false
		for name, c := range map[string]storage.ContentClaim{
			"wrong fence":   mutate(func(c *storage.ContentClaim) { c.FenceToken = bytes.Repeat([]byte{9}, 32) }),
			"foreign claim": foreignClaim,
		} {
			err := production.WithRegisteredCodePlatformIntent(t.Context(), c, dispatch, prepared, func(context.Context, storage.CodeTransaction, storage.RegisteredCodeIntent) error {
				called = true
				return nil
			})
			if err == nil || called {
				t.Errorf("%s: err=%v called=%v", name, err, called)
			}
		}
	})
}
