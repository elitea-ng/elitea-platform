package repos

// Real-PostgreSQL evidence on Main's production registration path (B6).
//
// Boundary crossed: the production CodeIntentRepository configuration
// (NewCodeIntentRepository's shared store and checkRecoveryPermission, the real
// legacy RBAC resolver over seeded auth tables), the real claim/fence/lease
// SQL, the real captured root source and fan-out child owner resolution
// (CodeDefinitionSourceAdapter), and the real visit and intent statements.
// Nothing on that path is stubbed; only the Worker side is played by the test
// (it sends the visit and intent requests the Worker sends for 4 Map items).
// Requires ELITEA_TEST_DATABASE_URL.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	httpapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/httpaction"
	code "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	scope "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"google.golang.org/protobuf/proto"
)

const (
	realPathActor     = 11
	realPathItems     = 4
	realPathAudience  = "spiffe://elitea/supervisor/one"
	realPathWorkerCfg = `{"id":"shout","type":"code","language":"python","code":{"type":"fixed","value":"1"},"input":["entity","item_index"],"output":["out"],"structured_output":false,"debug":false,"transition":null}`
)

// realPathWorld is one running execution whose Map "each" owns the Code
// worker "shout" (fanoutInstructions), claimed by one Worker.
type realPathWorld struct {
	w            *effectsWorld
	repo         *CodeIntentRepository
	claim        storage.ContentClaim
	root         string
	yamlSHA      string
	instructions string
}

func newRealPathWorld(t *testing.T, execution, claimID string, fence byte) *realPathWorld {
	t.Helper()
	return newRealPathWorldWith(t, execution, claimID, fence, fanoutInstructions)
}

func newRealPathWorldWith(t *testing.T, execution, claimID string, fence byte, fanoutInstructions string) *realPathWorld {
	t.Helper()
	return newRealPathWorldWithVersion(t, execution, claimID, fence, fanoutInstructions, nil)
}

// newRealPathWorldWithVersion also captures the root version's tools (the
// saved applications its Agent nodes select).
func newRealPathWorldWithVersion(t *testing.T, execution, claimID string, fence byte, fanoutInstructions string, tools []map[string]any) *realPathWorld {
	t.Helper()
	w := newEffectsWorld(t)
	project := w.project()
	// RBAC: the actor holds the permission the visit and intent check.
	seedAuthCoreTables(t, w.pool)
	w.exec(`CREATE TABLE IF NOT EXISTS public.auth_core__role (id serial PRIMARY KEY, name varchar(64) NOT NULL, mode varchar(64) NOT NULL, UNIQUE (name, mode));
CREATE TABLE IF NOT EXISTS public.auth_core__role_permission (id serial PRIMARY KEY, role_id integer NOT NULL REFERENCES public.auth_core__role (id) ON DELETE CASCADE, permission varchar(64), UNIQUE (role_id, permission));
CREATE TABLE IF NOT EXISTS public.auth_core__project_role_permission (id serial PRIMARY KEY, project_id integer NOT NULL, role_id integer REFERENCES public.auth_core__project_role (id) ON UPDATE CASCADE ON DELETE CASCADE, permission text NOT NULL, UNIQUE (project_id, role_id, permission))`)
	w.exec(`INSERT INTO public.auth_core__user (id, email, name, suspended) VALUES ($1, 'runner@example.com', 'Runner', false) ON CONFLICT (id) DO NOTHING`, realPathActor)
	w.exec(`INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES (9201, $1, 'code_runner')`, project)
	w.exec(`INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission) VALUES ($1, 9201, 'models.chat.messages.create')`, project)
	w.exec(`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES ($1, $2, 9201)`, project, realPathActor)

	// The captured root source the run was admitted with.
	root := map[string]any{"instructions": fanoutInstructions, "agent_type": "pipeline"}
	if tools != nil {
		root["tools"] = tools
	}
	version, _ := json.Marshal(root)
	wire, err := scope.NewSourceWire(project, realPathActor, 17, 19, version)
	if err != nil {
		t.Fatal(err)
	}
	ref := wire.Reference()
	w.exec(`INSERT INTO elitea_runtime.execution_captured_definitions(resource_project_id,application_id,version_id,definition_sha256,definition_bytes,byte_length) VALUES($1,17,19,$2,$3,$4)`, project, ref.SourceDefinitionSHA256, version, len(version))
	w.exec(`INSERT INTO elitea_runtime.execution_definition_source_refs(source_id,revision,digest_sha256,resource_project_id,actor_id,application_id,version_id,definition_sha256,canonical_wire) VALUES($1,$2,$3,$4,$5,17,19,$6,$7)`, ref.SourceID, int64(ref.Revision), ref.DigestSHA256, project, realPathActor, ref.SourceDefinitionSHA256, wire.CanonicalBytes())

	// The execution, its admitted request (the protobuf input naming that
	// source), the command and a live claim.
	application, _ := json.Marshal(map[string]any{"id": 17, "version_id": 19, "source_definition": ref})
	input, err := proto.Marshal(&runtimev1.AgentExecutionInputV1{ThreadId: proto.String("map-thread"), Application: application})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(input)
	bundle := "bundle-" + execution
	w.exec(`INSERT INTO elitea_runtime.input_bundles (input_bundle_id, immutable_version, media_type, resource_project_id, manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ($1, '1', 'application/x-protobuf', $2::int, sha256('m'::bytea), 1, 'm'::bytea, 'tests')`, bundle, project)
	w.exec(`INSERT INTO elitea_runtime.input_bundle_entries (input_bundle_id, entry_id, entry_version, semantic_role, media_type, content_digest, content_size, content_reference, classification, required_grant_audience, content_bytes)
VALUES ($1, 'request', '1', 'agent.execution_request', 'application/vnd.elitea.agent-execution-input.v1+protobuf', $2, $3, 'inline://request', 'internal', 'worker', $4)`, bundle, digest[:], len(input), input)
	w.exec(`INSERT INTO elitea_runtime.execution_jobs (execution_id, generation, command_id, tenant_id, resource_project_id, projection_project_id, actor_id, principal_ref, capability_id, capability_version, input_bundle_id, request_digest, idempotency_scope, idempotency_key, state, desired_state, invocation_state, admitted_at)
VALUES ($1, 1, 'command-'||$1, ($2::int)::text, $2::int, $2::int, $3, 'user:'||$3, 'agent.execute.application.v1', '1', $4, sha256(convert_to($1::text,'UTF8')), 'real-'||$1, $1, 'RUNNING', 'RUNNING', 'MAY_HAVE_STARTED', clock_timestamp())`, execution, project, fmt.Sprint(realPathActor), bundle)
	w.admit(execution)
	claim := w.access(execution, project, claimID, 1, fence)

	signer, err := storage.NewCodeOwnerGrantSigner("key-1", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32)), "spiffe://elitea/main", []string{realPathAudience})
	if err != nil {
		t.Fatal(err)
	}
	sources, err := NewCodeDefinitionSourceAdapter(&ExecutionChildScopesRepository{})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := newPostgresSharedStore(w.pool)
	if err != nil {
		t.Fatal(err)
	}
	// NewCodeIntentRepository's configuration, without the Supervisor owner
	// client (the visit and intent never call it).
	repo := &CodeIntentRepository{shared: shared, permissions: checkRecoveryPermission, signer: signer, sources: sources}
	return &realPathWorld{w: w, repo: repo, claim: claim,
		root:         httpapp.RootGraphThread(fmt.Sprint(project), project, project, "map-thread"),
		yamlSHA:      code.Digest([]byte(fanoutInstructions)),
		instructions: fanoutInstructions}
}

// visit is the visit request the Worker sends for Map item `item` of `count`.
func (r *realPathWorld) visit(t *testing.T, item, count int) (code.VisitRequest, []byte) {
	t.Helper()
	policy, err := storage.OriginalSavedCodePolicy(r.instructions, r.yamlSHA, "shout")
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := storage.MatchOriginalSavedCodeConfiguration(policy, realPathWorkerCfg)
	if err != nil {
		t.Fatal(err)
	}
	prepared := []byte(fmt.Sprintf(`{"revision":1,"language":"python","source":"1","input":{"entity":"item-%d","item_index":%d},"image_digest":"sha256:%s","policy_revision":"code-v1","timeout_seconds":10}`, item, item, strings.Repeat("9", 64)))
	thread := fmt.Sprintf("m1:item-%d", item)
	childScope, _ := json.Marshal(map[string]any{"schema": storage.FanoutCodeChildScopeSchema, "kind": "map_item", "node_id": "each", "owned_node_id": "shout", "ordinal": item, "item_count": count, "parent_thread_id": r.root, "child_thread_id": thread})
	return code.VisitRequest{Schema: "elitea.sandbox.original-code-visit-request.v1", ActivationID: fmt.Sprintf("%064x", 0xc000+item), NodeID: "shout", GraphThread: thread, Step: 1, Attempt: 1,
		NodeDigest: hex.EncodeToString(declaration.ConfigurationDigest[:]), OwningYAMLSHA256: r.yamlSHA,
		ConfigurationBase64URL: base64.RawURLEncoding.EncodeToString([]byte(realPathWorkerCfg)), PreWorkspacePreparedBase64URL: base64.RawURLEncoding.EncodeToString(prepared), SavedChildScope: childScope}, prepared
}

// register runs one item through RegisterOriginalCodeVisit then
// RegisterOriginalCodeIntent, as the Worker does before submission.
func (r *realPathWorld) register(ctx context.Context, t *testing.T, item, count int) error {
	request, prepared := r.visit(t, item, count)
	visit, err := r.repo.RegisterOriginalCodeVisit(ctx, r.claim, request)
	if err != nil {
		return fmt.Errorf("visit: %w", err)
	}
	policy, _ := storage.OriginalSavedCodePolicy(r.instructions, r.yamlSHA, "shout")
	declaration, _ := storage.MatchOriginalSavedCodeConfiguration(policy, realPathWorkerCfg)
	metadata, err := storage.MatchCodePrepared(declaration, []byte(realPathWorkerCfg), prepared)
	if err != nil {
		return err
	}
	intent := code.IntentRequest{Schema: "elitea.sandbox.original-code-intent-request.v1", OriginalVisit: visit.OriginalVisit, DispatchActivation: fmt.Sprintf("%064x", 0xd000+item),
		RequestDigest: metadata.Fingerprint, SupervisorAudience: realPathAudience, PreparedBase64URL: base64.RawURLEncoding.EncodeToString(prepared)}
	if _, err := r.repo.RegisterOriginalCodeIntent(ctx, r.claim, intent); err != nil {
		return fmt.Errorf("intent: %w", err)
	}
	return nil
}

func (r *realPathWorld) count(t *testing.T, table string) int {
	t.Helper()
	var rows int
	if err := r.w.pool.QueryRow(t.Context(), `SELECT count(*) FROM elitea_runtime.`+table+` WHERE execution_id=$1`, r.claim.ExecutionID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// B6: 4 Map items of one run register their visit and intent at once, on
// Main's production path. Every one commits; none is refused.
func TestFourConcurrentMapItemsRegisterOnTheProductionPathPostgres(t *testing.T) {
	r := newRealPathWorld(t, effectsParent, effectsClaim2, 3)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	errs := make([]error, realPathItems)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for item := range realPathItems {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[item] = r.register(ctx, t, item, realPathItems)
		}()
	}
	close(start)
	wg.Wait()
	for item, err := range errs {
		if err != nil {
			t.Errorf("item %d: %v (refused=%v)", item, err, errors.Is(err, storage.ErrContentUnauthorized) || errors.Is(err, code.ErrRejected))
		}
	}
	if visits, intents := r.count(t, "original_code_visits"), r.count(t, "original_code_intents"); visits != realPathItems || intents != realPathItems {
		t.Fatalf("visits=%d intents=%d, want %d each", visits, intents, realPathItems)
	}
	// A replay of the same registrations is idempotent.
	for item := range realPathItems {
		if err := r.register(ctx, t, item, realPathItems); err != nil {
			t.Fatalf("replay of item %d: %v", item, err)
		}
	}
	waitNoAdvisoryLocks(t, r.w)
}

// B5, Main's half on the production path: a Worker registers 4 of 8 Map
// items, one more is in flight holding the run's claim lock when the Worker
// is killed (its ctx cancelled), and three more wait. A replacement Worker
// takes the run over with a new claim and registers all 8 items at once: the
// 4 registered items re-attach to the same visits and intents (one record
// each, never a second), the others register fresh, nothing is refused and
// nothing stays locked.
func TestATakeoverReRegistersEveryMapItemOnTheProductionPathPostgres(t *testing.T) {
	const items = 8
	r := newRealPathWorld(t, effectsParent, effectsClaim2, 3)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for item := range 4 {
		if err := r.register(ctx, t, item, items); err != nil {
			t.Fatalf("first Worker, item %d: %v", item, err)
		}
	}
	firstVisits := map[int]string{}
	for item := range 4 {
		request, _ := r.visit(t, item, items)
		visit, err := r.repo.RegisterOriginalCodeVisit(ctx, r.claim, request)
		if err != nil {
			t.Fatal(err)
		}
		firstVisits[item] = visit.OriginalVisit.VisitID
	}
	// The kill: item 4 holds the claim lock inside its visit transaction,
	// items 5-7 wait on it, then every in-flight call is cancelled.
	killed, kill := context.WithCancel(ctx)
	holding := make(chan struct{})
	inFlight := make(chan error, 4)
	go func() {
		inFlight <- withinCodeRegistrationTx(killed, r.repo.shared, func(tx sqlExecutor) error {
			if _, err := r.repo.lockAccess(killed, tx, r.claim, "RUNNING"); err != nil {
				return err
			}
			close(holding)
			<-killed.Done()
			return killed.Err()
		})
	}()
	<-holding
	for item := 5; item < items; item++ {
		go func() { inFlight <- r.register(killed, t, item, items) }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, waiting, _ := heldLocks(t, r.w); waiting >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the in-flight registrations never waited on the claim lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	kill()
	for range 4 {
		if err := <-inFlight; err == nil {
			t.Fatal("a registration of the killed Worker committed")
		}
	}
	waitNoAdvisoryLocks(t, r.w)

	// The takeover: the old claim is released and a new claim of the same
	// generation (the next attempt and lease epoch) owns the run.
	r.w.exec(`UPDATE elitea_runtime.execution_claims SET released_at=clock_timestamp() WHERE claim_id=$1`, effectsClaim2)
	var project int64
	if err := r.w.pool.QueryRow(t.Context(), `SELECT resource_project_id FROM elitea_runtime.execution_jobs WHERE execution_id=$1`, effectsParent).Scan(&project); err != nil {
		t.Fatal(err)
	}
	r.claim = r.w.access(effectsParent, project, effectsClaim3, 2, 4)

	errs := make([]error, items)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for item := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[item] = r.register(ctx, t, item, items)
		}()
	}
	close(start)
	wg.Wait()
	for item, err := range errs {
		if err != nil {
			t.Errorf("replacement Worker, item %d: %v", item, err)
		}
	}
	for item, visitID := range firstVisits {
		request, _ := r.visit(t, item, items)
		visit, err := r.repo.RegisterOriginalCodeVisit(ctx, r.claim, request)
		if err != nil || visit.OriginalVisit.VisitID != visitID {
			t.Fatalf("item %d did not re-attach to its visit: %v", item, err)
		}
	}
	if visits, intents := r.count(t, "original_code_visits"), r.count(t, "original_code_intents"); visits != items || intents != items {
		t.Fatalf("visits=%d intents=%d, want %d each (one record per item)", visits, intents, items)
	}
	waitNoAdvisoryLocks(t, r.w)
}

// R2: a transient database failure inside a registration (here the visit
// table is held by another session until the call's deadline, as a
// failover or a statement timeout would fail it) is never answered as a
// refusal: the error is not code.ErrRejected or ErrContentUnauthorized, so
// Main answers it retryable (code.registration_failed). Once the database is
// back the same registration succeeds.
func TestATransientDatabaseFailureIsRetryableNeverARefusalPostgres(t *testing.T) {
	r := newRealPathWorld(t, effectsParent, effectsClaim2, 3)
	refused := func(err error) bool {
		return errors.Is(err, code.ErrRejected) || errors.Is(err, storage.ErrContentUnauthorized)
	}
	block := func() func() {
		conn, err := r.w.pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		tx, err := conn.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), `LOCK TABLE elitea_runtime.original_code_visits IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		return func() {
			_ = tx.Rollback(context.Background())
			conn.Release()
		}
	}
	// The visit: its per-run visit count cannot be read in time.
	request, _ := r.visit(t, 0, 2)
	release := block()
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	_, err := r.repo.RegisterOriginalCodeVisit(ctx, r.claim, request)
	cancel()
	release()
	if err == nil || refused(err) {
		t.Fatalf("a transient failure of the visit was %v, want a retryable error", err)
	}
	visit, err := r.repo.RegisterOriginalCodeVisit(t.Context(), r.claim, request)
	if err != nil {
		t.Fatalf("the visit did not continue once the database was back: %v", err)
	}
	// The intent: its visit cannot be read in time.
	_, prepared := r.visit(t, 0, 2)
	policy, _ := storage.OriginalSavedCodePolicy(r.instructions, r.yamlSHA, "shout")
	declaration, _ := storage.MatchOriginalSavedCodeConfiguration(policy, realPathWorkerCfg)
	metadata, err := storage.MatchCodePrepared(declaration, []byte(realPathWorkerCfg), prepared)
	if err != nil {
		t.Fatal(err)
	}
	intent := code.IntentRequest{Schema: "elitea.sandbox.original-code-intent-request.v1", OriginalVisit: visit.OriginalVisit, DispatchActivation: fmt.Sprintf("%064x", 0xd000),
		RequestDigest: metadata.Fingerprint, SupervisorAudience: realPathAudience, PreparedBase64URL: base64.RawURLEncoding.EncodeToString(prepared)}
	release = block()
	ctx, cancel = context.WithTimeout(t.Context(), 500*time.Millisecond)
	_, err = r.repo.RegisterOriginalCodeIntent(ctx, r.claim, intent)
	cancel()
	release()
	if err == nil || refused(err) {
		t.Fatalf("a transient failure of the intent was %v, want a retryable error", err)
	}
	if _, err := r.repo.RegisterOriginalCodeIntent(t.Context(), r.claim, intent); err != nil {
		t.Fatalf("the intent did not continue once the database was back: %v", err)
	}
	waitNoAdvisoryLocks(t, r.w)
}

// R8: per-visit registration latency of a 64-item Map on the production
// path (visit then intent, one item at a time, so it measures the work under
// the run's row lock rather than the wait). Logged for the source mapping.
func TestPerVisitRegistrationLatencyOfA64ItemMapPostgres(t *testing.T) {
	const items = 64
	instructions := strings.Replace(fanoutInstructions, "max_items: 8", "max_items: 64", 1)
	r := newRealPathWorldWith(t, effectsParent, effectsClaim2, 3, instructions)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	visits, intents := make([]time.Duration, 0, items), make([]time.Duration, 0, items)
	intentParses := 0
	for item := range items {
		request, prepared := r.visit(t, item, items)
		started := time.Now()
		visit, err := r.repo.RegisterOriginalCodeVisit(ctx, r.claim, request)
		if err != nil {
			t.Fatalf("item %d: %v", item, err)
		}
		visits = append(visits, time.Since(started))
		policy, _ := storage.OriginalSavedCodePolicy(r.instructions, r.yamlSHA, "shout")
		declaration, _ := storage.MatchOriginalSavedCodeConfiguration(policy, realPathWorkerCfg)
		metadata, err := storage.MatchCodePrepared(declaration, []byte(realPathWorkerCfg), prepared)
		if err != nil {
			t.Fatal(err)
		}
		intent := code.IntentRequest{Schema: "elitea.sandbox.original-code-intent-request.v1", OriginalVisit: visit.OriginalVisit, DispatchActivation: fmt.Sprintf("%064x", 0xd000+item),
			RequestDigest: metadata.Fingerprint, SupervisorAudience: realPathAudience, PreparedBase64URL: base64.RawURLEncoding.EncodeToString(prepared)}
		parses := storage.SavedCodeParsesForTests()
		started = time.Now()
		if _, err := r.repo.RegisterOriginalCodeIntent(ctx, r.claim, intent); err != nil {
			t.Fatalf("item %d intent: %v", item, err)
		}
		intents = append(intents, time.Since(started))
		intentParses = int(storage.SavedCodeParsesForTests() - parses)
	}
	quantile := func(samples []time.Duration, q float64) time.Duration {
		sorted := append([]time.Duration(nil), samples...)
		slices.Sort(sorted)
		return sorted[int(q*float64(len(sorted)-1))]
	}
	t.Logf("R8-REGISTRATION-LATENCY items=%d visit_p50=%v visit_p99=%v intent_p50=%v intent_p99=%v yaml_parses_per_visit=%d yaml_parses_per_intent=%d",
		items, quantile(visits, 0.5), quantile(visits, 0.99), quantile(intents, 0.5), quantile(intents, 0.99), parsesPerVisit(t, r), intentParses)
}

// parsesPerVisit counts the YAML parses one more visit registration makes.
func parsesPerVisit(t *testing.T, r *realPathWorld) int {
	t.Helper()
	request, _ := r.visit(t, 0, 64)
	before := storage.SavedCodeParsesForTests()
	if _, err := r.repo.RegisterOriginalCodeVisit(t.Context(), r.claim, request); err != nil {
		t.Fatal(err)
	}
	return int(storage.SavedCodeParsesForTests() - before)
}

// R5: a permission revoked while an intent registration is in flight (the
// intent passed its first claim and permission check, then waits on its
// visit row, held here by another session) is seen by the intent's final
// claim and permission re-check before signing: the registration fails
// typed (refused at claim_recheck), no intent row is written and no grant is
// signed. Under SERIALIZABLE that re-check would read the transaction's
// first snapshot and sign against the revoked state.
func TestAPermissionRevokedDuringIntentRegistrationIsNeverSignedPostgres(t *testing.T) {
	r := newRealPathWorld(t, effectsParent, effectsClaim2, 3)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	request, prepared := r.visit(t, 0, 2)
	visit, err := r.repo.RegisterOriginalCodeVisit(ctx, r.claim, request)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := storage.OriginalSavedCodePolicy(r.instructions, r.yamlSHA, "shout")
	declaration, _ := storage.MatchOriginalSavedCodeConfiguration(policy, realPathWorkerCfg)
	metadata, err := storage.MatchCodePrepared(declaration, []byte(realPathWorkerCfg), prepared)
	if err != nil {
		t.Fatal(err)
	}
	intent := code.IntentRequest{Schema: "elitea.sandbox.original-code-intent-request.v1", OriginalVisit: visit.OriginalVisit, DispatchActivation: fmt.Sprintf("%064x", 0xd000),
		RequestDigest: metadata.Fingerprint, SupervisorAudience: realPathAudience, PreparedBase64URL: base64.RawURLEncoding.EncodeToString(prepared)}

	// Another session holds the visit row: the intent passes its first
	// lockAccess, then waits on readVisit's FOR UPDATE.
	holder, err := r.w.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	hold, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `SELECT 1 FROM elitea_runtime.original_code_visits WHERE visit_id=$1 FOR UPDATE`, visit.OriginalVisit.VisitID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.repo.RegisterOriginalCodeIntent(ctx, r.claim, intent)
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, waiting, _ := heldLocks(t, r.w); waiting >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the intent never waited on its visit row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The revocation commits while the intent is in flight.
	r.w.exec(`DELETE FROM public.auth_core__project_user_role WHERE user_id=$1`, realPathActor)
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done
	if !errors.Is(err, storage.ErrContentUnauthorized) {
		t.Fatalf("an intent was answered %v after the permission was revoked, want a typed refusal", err)
	}
	var stager interface{ Stage() string }
	if !errors.As(err, &stager) || stager.Stage() != "claim_recheck" {
		t.Fatalf("refused at %v, want claim_recheck", err)
	}
	if intents := r.count(t, "original_code_intents"); intents != 0 {
		t.Fatalf("intents = %d, want none written", intents)
	}
	waitNoAdvisoryLocks(t, r.w)
}

// 5b-1 PR B real-PG Main dispatch identity test:
// settings.retry_on_fail accepts ordinal tries 1..=max_tries (+ MaxRuntimeLostRetries
// for non-platform-client), refuses +1, refuses legacy identity on try 1, and
// refuses forged/foreign execution claims.
func TestOriginalCodeSettingsRetryDispatchIdentityPostgres(t *testing.T) {
	instructions := `entry_point: run
state:
  input: str
  answer: int
nodes:
  - id: run
    type: code
    language: python
    code: '1'
    input: [input]
    output: [answer]
    settings: {retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 100, timeout_ms: 5000}
    transition: END
`
	cfg := `{"id":"run","type":"code","language":"python","code":{"type":"fixed","value":"1"},"input":["input"],"output":["answer"],"structured_output":false,"debug":false,"transition":"END"}`
	r := newRealPathWorldWith(t, effectsParent, effectsClaim2, 3, instructions)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	policy, err := storage.OriginalSavedCodePolicy(r.instructions, r.yamlSHA, "run")
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := storage.MatchOriginalSavedCodeConfiguration(policy, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !declaration.SettingsRetryOnFail || declaration.SettingsMaxTries != 3 {
		t.Fatalf("unexpected declaration settings: retry=%v maxTries=%d", declaration.SettingsRetryOnFail, declaration.SettingsMaxTries)
	}
	prepared := []byte(`{"revision":1,"language":"python","source":"1","input":{"input":"hi"},"image_digest":"sha256:` + strings.Repeat("9", 64) + `","policy_revision":"code-v1","timeout_seconds":5}`)
	metadata, err := storage.MatchCodePrepared(declaration, []byte(cfg), prepared)
	if err != nil {
		t.Fatal(err)
	}
	activation := strings.Repeat("4", 64)
	legacyDispatch := strings.Repeat("a", 64)

	registerAttempt := func(claim storage.ContentClaim, attempt uint16, dispatch string) error {
		visitReq := code.VisitRequest{
			Schema:                        "elitea.sandbox.original-code-visit-request.v1",
			ActivationID:                  activation,
			NodeID:                        "run",
			GraphThread:                   r.root,
			Step:                          1,
			Attempt:                       attempt,
			NodeDigest:                    hex.EncodeToString(declaration.ConfigurationDigest[:]),
			OwningYAMLSHA256:              r.yamlSHA,
			ConfigurationBase64URL:        base64.RawURLEncoding.EncodeToString([]byte(cfg)),
			PreWorkspacePreparedBase64URL: base64.RawURLEncoding.EncodeToString(prepared),
			SavedChildScope:               json.RawMessage("null"),
		}
		visit, err := r.repo.RegisterOriginalCodeVisit(ctx, claim, visitReq)
		if err != nil {
			return err
		}
		intentReq := code.IntentRequest{
			Schema:             "elitea.sandbox.original-code-intent-request.v1",
			OriginalVisit:      visit.OriginalVisit,
			DispatchActivation: dispatch,
			RequestDigest:      metadata.Fingerprint,
			SupervisorAudience: realPathAudience,
			PreparedBase64URL:  base64.RawURLEncoding.EncodeToString(prepared),
		}
		_, err = r.repo.RegisterOriginalCodeIntent(ctx, claim, intentReq)
		return err
	}

	// Legacy non-ordinal dispatch on try 1 must be refused when settings.retry_on_fail is true.
	if err := registerAttempt(r.claim, 1, legacyDispatch); err == nil {
		t.Fatal("legacy non-ordinal dispatch was admitted on try 1")
	}

	// Ordinal tries 1..=max_tries + MaxRuntimeLostRetries (5) all succeed on real PG.
	for attempt := uint16(1); attempt <= 3+MaxRuntimeLostRetries; attempt++ {
		if err := registerAttempt(r.claim, attempt, code.DispatchActivation(activation, attempt)); err != nil {
			t.Fatalf("ordinal attempt %d refused: %v", attempt, err)
		}
	}

	// One past the allowance (attempt 6) must be refused closed.
	over := uint16(3 + MaxRuntimeLostRetries + 1)
	if err := registerAttempt(r.claim, over, code.DispatchActivation(activation, over)); err == nil {
		t.Fatalf("attempt %d past max_tries + MaxRuntimeLostRetries was admitted", over)
	}

	// Forged / foreign execution claim must be refused closed.
	foreignClaim := r.claim
	foreignClaim.ExecutionID = effectsForeign
	if err := registerAttempt(foreignClaim, 1, code.DispatchActivation(activation, 1)); err == nil {
		t.Fatal("foreign execution claim was admitted")
	}

	if intents := r.count(t, "original_code_intents"); intents != int(3+MaxRuntimeLostRetries) {
		t.Fatalf("intents = %d, want %d", intents, 3+MaxRuntimeLostRetries)
	}
	waitNoAdvisoryLocks(t, r.w)
}

func TestOriginalCodeSettingsNodeErrorsVisitPostgres(t *testing.T) {
	instructions := `entry_point: broken_code
state:
  out: str
  verdict: str
nodes:
  - id: broken_code
    type: code
    language: python
    code: '1'
    output: [out]
    settings:
      on_error: error_output
      error_transition: handle_err
    transition: END
  - id: handle_err
    type: code
    language: python
    code: '2'
    input: [node_errors]
    output: [verdict]
    transition: END
`
	brokenCfg := `{"id":"broken_code","type":"code","language":"python","code":{"type":"fixed","value":"1"},"input":[],"output":["out"],"structured_output":false,"debug":false,"transition":"END"}`
	handlerCfg := `{"id":"handle_err","type":"code","language":"python","code":{"type":"fixed","value":"2"},"input":["node_errors"],"output":["verdict"],"structured_output":false,"debug":false,"transition":"END"}`
	r := newRealPathWorldWith(t, effectsParent, effectsClaim2, 4, instructions)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	registerNode := func(nodeID, cfgJSON, preparedInputJSON, activation string, step uint64) error {
		policy, err := storage.OriginalSavedCodePolicy(r.instructions, r.yamlSHA, nodeID)
		if err != nil {
			return err
		}
		declaration, err := storage.MatchOriginalSavedCodeConfiguration(policy, cfgJSON)
		if err != nil {
			return err
		}
		source := "1"
		if nodeID == "handle_err" {
			source = "2"
		}
		prepared := []byte(`{"revision":1,"language":"python","source":"` + source + `","input":` + preparedInputJSON + `,"image_digest":"sha256:` + strings.Repeat("9", 64) + `","policy_revision":"code-v1","timeout_seconds":5}`)
		visitReq := code.VisitRequest{
			Schema:                        "elitea.sandbox.original-code-visit-request.v1",
			ActivationID:                  activation,
			NodeID:                        nodeID,
			GraphThread:                   r.root,
			Step:                          step,
			Attempt:                       1,
			NodeDigest:                    hex.EncodeToString(declaration.ConfigurationDigest[:]),
			OwningYAMLSHA256:              r.yamlSHA,
			ConfigurationBase64URL:        base64.RawURLEncoding.EncodeToString([]byte(cfgJSON)),
			PreWorkspacePreparedBase64URL: base64.RawURLEncoding.EncodeToString(prepared),
			SavedChildScope:               json.RawMessage("null"),
		}
		_, err = r.repo.RegisterOriginalCodeVisit(ctx, r.claim, visitReq)
		return err
	}

	// broken_code (input: []) includes synthesized node_errors: [] alongside input: "hi".
	if err := registerNode("broken_code", brokenCfg, `{"input":"hi","node_errors":[]}`, strings.Repeat("5", 64), 1); err != nil {
		t.Fatalf("broken_code visit with synthesized node_errors refused: %v", err)
	}
	// handle_err explicitly selects synthesized node_errors.
	if err := registerNode("handle_err", handlerCfg, `{"node_errors":[{"node":"broken_code"}]}`, strings.Repeat("6", 64), 2); err != nil {
		t.Fatalf("handle_err visit selecting node_errors refused: %v", err)
	}
	// Non-list node_errors must fail closed at stage "input".
	if err := registerNode("broken_code", brokenCfg, `{"node_errors":"not-a-list"}`, strings.Repeat("7", 64), 1); err == nil {
		t.Fatal("non-list node_errors was admitted")
	}
	if visits := r.count(t, "original_code_visits"); visits != 2 {
		t.Fatalf("visits = %d, want 2", visits)
	}
	waitNoAdvisoryLocks(t, r.w)
}
