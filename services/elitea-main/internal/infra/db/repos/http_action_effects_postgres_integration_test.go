package repos

// Real-PostgreSQL evidence for HTTP action effects (T-5D 5d-1; design §13).
//
// Boundary crossed: service integration — real PostgreSQL 18, the full
// ledgered migrations (incl. 0170), the real claim/lease SQL, the real legacy
// RBAC resolver on the Begin transaction, and the real configuration row.
// Requires ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	httpWorldExecution = "0123456789abcdef0123456789abcdef"
	httpWorldClaim     = "00000000000000000000000000000011"
	httpWorldClaim2    = "00000000000000000000000000000012"
	httpWorldConfig    = int32(77)
	httpWorldActor     = int64(11)
)

type httpWorld struct {
	*effectsWorld
	repo     *HTTPActionRepository
	ruleID   int64
	template app.NodeTemplate
}

func allowAllTools() app.ToolPolicy { return httpToolsAllowed{} }

type httpToolsAllowed struct{}

func (httpToolsAllowed) Permits(string, string) bool { return true }

func newHTTPWorld(t *testing.T, method string) *httpWorld {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	w := &httpWorld{effectsWorld: &effectsWorld{t: t, pool: pool}}
	seedAuthCoreTables(t, pool)
	w.exec(`
CREATE TABLE IF NOT EXISTS public.auth_core__role (
    id serial PRIMARY KEY, name varchar(64) NOT NULL, mode varchar(64) NOT NULL, UNIQUE (name, mode));
CREATE TABLE IF NOT EXISTS public.auth_core__role_permission (
    id serial PRIMARY KEY, role_id integer NOT NULL REFERENCES public.auth_core__role (id) ON DELETE CASCADE,
    permission varchar(64), UNIQUE (role_id, permission));
CREATE TABLE IF NOT EXISTS public.auth_core__project_role_permission (
    id serial PRIMARY KEY, project_id integer NOT NULL,
    role_id integer REFERENCES public.auth_core__project_role (id) ON UPDATE CASCADE ON DELETE CASCADE,
    permission text NOT NULL, UNIQUE (project_id, role_id, permission))`)
	w.exec(`INSERT INTO public.auth_core__user (id, email, name, suspended) VALUES (11, 'actor@example.com', 'Actor', false), (12, 'other@example.com', 'Other', false)`)
	w.exec(`INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES (9201, 1, 'editor')`)
	w.exec(`INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission) VALUES (1, 9201, 'configurations.configuration.details')`)
	w.exec(`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES (1, 11, 9201)`)
	w.exec(`INSERT INTO p_1.configuration (id,uuid,project_id,elitea_title,type,section,data,meta,shared,status_ok,source,author_id,created_at)
VALUES ($1,'60000000-0000-4000-8000-000000000077',1,'http_creds','openapi','credentials','{"api_key":"{{secret.k}}","auth_type":"Bearer"}'::jsonb,'{}'::jsonb,false,true,'user',11, now() AT TIME ZONE 'UTC')`, httpWorldConfig)
	if err := pool.QueryRow(t.Context(), `INSERT INTO elitea_runtime.http_action_rules (project_id,name,origin,path_template,method,query_names,configuration_id,toolkit,tool,output_bucket,replay,enabled,created_by,updated_by)
VALUES (1,'tracker.item','https://api.example.com','/items/{item}',$1,'{}',$2,'tracker','item','http-results','never',TRUE,11,11) RETURNING id`, method, httpWorldConfig).Scan(&w.ruleID); err != nil {
		t.Fatal(err)
	}
	w.job(httpWorldExecution, 1, "RUNNING")
	w.access(httpWorldExecution, 1, httpWorldClaim, 1, 0x11)
	w.admit(httpWorldExecution)
	repo, err := NewHTTPActionRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	w.repo = repo
	node, err := app.ParseNode([]byte(`{"id":"fetch","type":"http","revision":2,"operation":"tracker.item","request":{"path":{"item":{"from":"k"}},"idempotency_key":{"from":"r","optional":true}},"response":{"mode":"json"},"output":["out"]}`))
	if err != nil {
		t.Fatal(err)
	}
	w.template = node.Template
	return w
}

func (w *httpWorld) admission(claim string, epoch uint64) app.Admission {
	return app.Admission{ExecutionID: httpWorldExecution, Generation: 1, ProjectID: 1, ActorID: httpWorldActor, ClaimID: claim, LeaseEpoch: epoch, Deadline: time.Now().Add(time.Hour), Template: w.template, Tools: allowAllTools()}
}

func (w *httpWorld) invocation(attempt uint32, item string) app.Invocation {
	w.t.Helper()
	inputs := []byte(`{"path":{"item":"` + item + `"}}`)
	binding := strings.Repeat("d", 64)
	inv := app.Invocation{SchemaVersion: app.Schema, NodeID: "fetch", ThreadID: "graph", Step: 1, Attempt: attempt, BindingDigest: binding, InputsB64: base64.StdEncoding.EncodeToString(inputs), InputsDigest: app.Digest(inputs)}
	inv.ActivationID = app.VisitID(inv.ThreadID, inv.NodeID, inv.Step, app.BindingBytes(binding))
	parsed, err := app.ParseInvocation(inv)
	if err != nil {
		w.t.Fatal(err)
	}
	return parsed
}

func (w *httpWorld) credentialRevision() string {
	w.t.Helper()
	var revision string
	if err := w.repo.store.WithinTx(w.t.Context(), pgx.TxOptions{}, func(tx sqlExecutor) error {
		access, err := httpConfigurationAccess(w.t.Context(), tx, 1, httpWorldActor, httpWorldConfig, false)
		revision = access.revision
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return revision
}

func (w *httpWorld) intent(admitted app.Admission, inv app.Invocation) app.Intent {
	w.t.Helper()
	rule, err := w.repo.Rule(w.t.Context(), 1, "tracker.item")
	if err != nil {
		w.t.Fatal(err)
	}
	rendered, err := app.Render(rule, w.template, inv.Inputs)
	if err != nil {
		w.t.Fatal(err)
	}
	return app.Intent{EffectID: app.EffectID(admitted.ExecutionID, admitted.Generation, inv), Rule: rule, RequestWire: rendered.Wire, RequestDigest: rendered.Digest, CredentialRevision: w.credentialRevision(), Retryable: rule.Retryable(rendered.Request), DispatchDeadline: time.Now().Add(time.Minute)}
}

func (w *httpWorld) effectRows() int {
	w.t.Helper()
	var count int
	if err := w.pool.QueryRow(w.t.Context(), `SELECT count(*) FROM elitea_runtime.execution_http_effects WHERE execution_id=$1`, httpWorldExecution).Scan(&count); err != nil {
		w.t.Fatal(err)
	}
	return count
}

func completedReceipt(begun app.Receipt) app.Receipt {
	begun.State = "completed"
	begun.Result = &app.Projection{Status: 200, Data: app.Data{Kind: "empty"}}
	return begun
}

// Design §13 case 5: 32 concurrent Begin calls on one activation dispatch once.
func TestHTTPEffectBegin32ConcurrentOneDispatchPostgres(t *testing.T) {
	w := newHTTPWorld(t, "POST")
	admitted := w.admission(httpWorldClaim, 1)
	inv := w.invocation(0, "i")
	intent := w.intent(admitted, inv)
	var dispatched, inFlight atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, dispatch, err := w.repo.Begin(context.Background(), admitted, inv, intent)
			switch {
			case err == nil && dispatch:
				dispatched.Add(1)
			case errors.Is(err, app.ErrDispatchInFlight):
				inFlight.Add(1)
			default:
				t.Errorf("unexpected Begin outcome: %v %v", dispatch, err)
			}
		}()
	}
	wg.Wait()
	if dispatched.Load() != 1 || inFlight.Load() != 31 || w.effectRows() != 1 {
		t.Fatalf("dispatched=%d inflight=%d rows=%d", dispatched.Load(), inFlight.Load(), w.effectRows())
	}
}

// Design §13 cases 3, 4 and 10: a dispatching row is never dispatched again;
// a live in-flight row answers 503, a lost lease fences Commit, and a new
// claim reads it as uncertain/reconciliation_required.
func TestHTTPEffectCrashWindowsPostgres(t *testing.T) {
	w := newHTTPWorld(t, "POST")
	admitted := w.admission(httpWorldClaim, 1)
	inv := w.invocation(0, "i")
	begun, dispatch, err := w.repo.Begin(t.Context(), admitted, inv, w.intent(admitted, inv))
	if err != nil || !dispatch {
		t.Fatal(err)
	}
	// Case 4: the same live claim asks again while the call is in flight.
	if _, _, err := w.repo.Lookup(t.Context(), admitted, inv); !errors.Is(err, app.ErrDispatchInFlight) {
		t.Fatalf("in-flight lookup: %v", err)
	}
	// Case 10 / 3: Main dies, the lease is lost, a new claim takes over.
	w.exec(`UPDATE elitea_runtime.execution_claims SET released_at=clock_timestamp(), release_reason='takeover' WHERE claim_id=$1`, httpWorldClaim)
	if err := w.repo.Commit(t.Context(), admitted, completedReceipt(begun)); !errors.Is(err, ErrHTTPEffectFenced) {
		t.Fatalf("commit after lease loss: %v", err)
	}
	w.access(httpWorldExecution, 1, httpWorldClaim2, 2, 0x12)
	takeover := w.admission(httpWorldClaim2, 2)
	receipt, found, err := w.repo.Lookup(t.Context(), takeover, inv)
	if err != nil || !found || receipt.State != "uncertain" || *receipt.FailureCode != "reconciliation_required" {
		t.Fatalf("takeover lookup: %+v %v %v", receipt, found, err)
	}
	// Begin under the new claim returns the same uncertain receipt and never dispatches.
	again, dispatch, err := w.repo.Begin(t.Context(), takeover, inv, w.intent(takeover, inv))
	if err != nil || dispatch || again.State != "uncertain" || w.effectRows() != 1 {
		t.Fatalf("re-dispatch after crash: %+v %v %v rows=%d", again, dispatch, err, w.effectRows())
	}
	// The fenced original commit still cannot overwrite the settled row.
	w.exec(`UPDATE elitea_runtime.execution_claims SET released_at=clock_timestamp() WHERE claim_id=$1`, httpWorldClaim2)
	w.exec(`UPDATE elitea_runtime.execution_claims SET released_at=NULL WHERE claim_id=$1`, httpWorldClaim)
	if err := w.repo.Commit(t.Context(), admitted, completedReceipt(begun)); !errors.Is(err, ErrHTTPEffectFenced) {
		t.Fatalf("late commit overwrote the uncertain receipt: %v", err)
	}
}

// A dispatching row past its dispatch deadline is uncertain even for the
// same claim (Main died and the Worker kept its lease).
func TestHTTPEffectDispatchDeadlinePostgres(t *testing.T) {
	w := newHTTPWorld(t, "GET")
	admitted := w.admission(httpWorldClaim, 1)
	inv := w.invocation(0, "i")
	intent := w.intent(admitted, inv)
	intent.DispatchDeadline = time.Now().Add(-time.Second)
	if _, dispatch, err := w.repo.Begin(t.Context(), admitted, inv, intent); err != nil || !dispatch {
		t.Fatal(err)
	}
	receipt, found, err := w.repo.Lookup(t.Context(), admitted, inv)
	if err != nil || !found || receipt.State != "uncertain" {
		t.Fatalf("%+v %v %v", receipt, found, err)
	}
}

// Design §13 cases 1 and 8: a committed receipt is returned to any later
// claim without a second dispatch; other inputs for the same activation are a
// typed 409 and never dispatch.
func TestHTTPEffectCommittedReceiptAndReplayDifferentInputsPostgres(t *testing.T) {
	w := newHTTPWorld(t, "POST")
	admitted := w.admission(httpWorldClaim, 1)
	inv := w.invocation(0, "i")
	begun, _, err := w.repo.Begin(t.Context(), admitted, inv, w.intent(admitted, inv))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.repo.Commit(t.Context(), admitted, completedReceipt(begun)); err != nil {
		t.Fatal(err)
	}
	w.exec(`UPDATE elitea_runtime.execution_claims SET released_at=clock_timestamp() WHERE claim_id=$1`, httpWorldClaim)
	w.access(httpWorldExecution, 1, httpWorldClaim2, 2, 0x12)
	replacement := w.admission(httpWorldClaim2, 2)
	receipt, found, err := w.repo.Lookup(t.Context(), replacement, inv)
	if err != nil || !found || receipt.State != "completed" || receipt.EffectID != begun.EffectID {
		t.Fatalf("committed receipt not returned: %+v %v", receipt, err)
	}
	other := w.invocation(0, "j")
	if _, _, err := w.repo.Lookup(t.Context(), replacement, other); app.FailureCode(err) != app.CodeNonDetermined {
		t.Fatalf("different inputs: %v", err)
	}
	if _, dispatch, err := w.repo.Begin(t.Context(), replacement, other, w.intent(replacement, other)); dispatch || app.FailureCode(err) != app.CodeNonDetermined || w.effectRows() != 1 {
		t.Fatalf("different inputs began: %v %v", dispatch, err)
	}
}

// Authority runs inside Begin: a disabled or edited rule, a lost permission, a
// changed or foreign configuration, or a fenced claim refuse with no row.
func TestHTTPEffectBeginRechecksAuthorityPostgres(t *testing.T) {
	for name, change := range map[string]func(*httpWorld){
		"rule disabled": func(w *httpWorld) {
			w.exec(`UPDATE elitea_runtime.http_action_rules SET enabled=FALSE, revision=revision+1 WHERE id=$1`, w.ruleID)
		},
		"rule edited": func(w *httpWorld) {
			w.exec(`UPDATE elitea_runtime.http_action_rules SET origin='https://other.example.com', revision=revision+1 WHERE id=$1`, w.ruleID)
		},
		"permission removed": func(w *httpWorld) { w.exec(`DELETE FROM public.auth_core__project_user_role WHERE user_id=11`) },
		"configuration edited": func(w *httpWorld) {
			w.exec(`UPDATE p_1.configuration SET data='{"api_key":"{{secret.k2}}"}'::jsonb, updated_at=now() AT TIME ZONE 'UTC' WHERE id=$1`, httpWorldConfig)
		},
		"configuration private": func(w *httpWorld) { w.exec(`UPDATE p_1.configuration SET author_id=12 WHERE id=$1`, httpWorldConfig) },
		"configuration retyped": func(w *httpWorld) { w.exec(`UPDATE p_1.configuration SET type='github' WHERE id=$1`, httpWorldConfig) },
		"claim released": func(w *httpWorld) {
			w.exec(`UPDATE elitea_runtime.execution_claims SET released_at=clock_timestamp() WHERE claim_id=$1`, httpWorldClaim)
		},
		"job cancelled": func(w *httpWorld) {
			w.exec(`UPDATE elitea_runtime.execution_jobs SET desired_state='CANCELLED' WHERE execution_id=$1`, httpWorldExecution)
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newHTTPWorld(t, "POST")
			admitted := w.admission(httpWorldClaim, 1)
			inv := w.invocation(0, "i")
			intent := w.intent(admitted, inv)
			change(w)
			_, dispatch, err := w.repo.Begin(t.Context(), admitted, inv, intent)
			if dispatch || !errors.Is(err, app.ErrUnauthorized) || w.effectRows() != 0 {
				t.Fatalf("dispatch=%v err=%v rows=%d", dispatch, err, w.effectRows())
			}
		})
	}
	t.Run("foreign project rule", func(t *testing.T) {
		w := newHTTPWorld(t, "POST")
		w.exec(`INSERT INTO centry.project (id, create_success, suspended) VALUES (2, TRUE, FALSE)`)
		w.exec(`UPDATE elitea_runtime.http_action_rules SET project_id=2 WHERE id=$1`, w.ruleID)
		if _, err := w.repo.Rule(t.Context(), 1, "tracker.item"); app.FailureCode(err) != app.CodePolicyDenied {
			t.Fatalf("foreign project rule visible: %v", err)
		}
	})
}

// A rule disable that commits while Begin waits is ordered before it: Begin
// sees the disabled rule and writes nothing (never both).
func TestHTTPEffectConcurrentDisableIsOrderedPostgres(t *testing.T) {
	w := newHTTPWorld(t, "POST")
	admitted := w.admission(httpWorldClaim, 1)
	inv := w.invocation(0, "i")
	intent := w.intent(admitted, inv)
	tx, err := w.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE elitea_runtime.http_action_rules SET enabled=FALSE, revision=revision+1 WHERE id=$1`, w.ruleID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, _, err := w.repo.Begin(context.Background(), admitted, inv, intent)
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("Begin did not wait for the concurrent disable: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, app.ErrUnauthorized) || w.effectRows() != 0 {
		t.Fatalf("Begin after disable: %v rows=%d", err, w.effectRows())
	}
}

// D2 classes at the ledger: attempt N needs attempt N-1 ended uncertain or
// failed, and a retryable rule.
func TestHTTPEffectAttemptsPostgres(t *testing.T) {
	w := newHTTPWorld(t, "GET")
	admitted := w.admission(httpWorldClaim, 1)
	first := w.invocation(0, "i")
	second := w.invocation(1, "i")
	if _, dispatch, err := w.repo.Begin(t.Context(), admitted, second, w.intent(admitted, second)); dispatch || !errors.Is(err, app.ErrUnauthorized) {
		t.Fatalf("attempt 1 without attempt 0: %v", err)
	}
	begun, _, err := w.repo.Begin(t.Context(), admitted, first, w.intent(admitted, first))
	if err != nil {
		t.Fatal(err)
	}
	completed := completedReceipt(begun)
	if err := w.repo.Commit(t.Context(), admitted, completed); err != nil {
		t.Fatal(err)
	}
	if _, dispatch, err := w.repo.Begin(t.Context(), admitted, second, w.intent(admitted, second)); dispatch || !errors.Is(err, app.ErrUnauthorized) {
		t.Fatalf("attempt after a completed receipt: %v", err)
	}
	w.exec(`UPDATE elitea_runtime.execution_http_effects SET state='uncertain' WHERE effect_id=$1`, begun.EffectID)
	if _, dispatch, err := w.repo.Begin(t.Context(), admitted, second, w.intent(admitted, second)); !dispatch || err != nil {
		t.Fatalf("GET retry after uncertain refused: %v", err)
	}
	post := newHTTPWorld(t, "POST")
	postAdmitted := post.admission(httpWorldClaim, 1)
	postFirst := post.invocation(0, "i")
	postBegun, _, _ := post.repo.Begin(t.Context(), postAdmitted, postFirst, post.intent(postAdmitted, postFirst))
	uncertain := postBegun
	uncertain.State = "uncertain"
	failure := "reconciliation_required"
	uncertain.FailureCode = &failure
	if err := post.repo.Commit(t.Context(), postAdmitted, uncertain); err != nil {
		t.Fatal(err)
	}
	postSecond := post.invocation(1, "i")
	if _, dispatch, err := post.repo.Begin(t.Context(), postAdmitted, postSecond, post.intent(postAdmitted, postSecond)); dispatch || !errors.Is(err, app.ErrUnauthorized) {
		t.Fatalf("POST replay=never retried after uncertain: %v", err)
	}
}

type httpWriteCounter struct {
	mu      sync.Mutex
	inserts int
	updates int
}

func (c *httpWriteCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := strings.TrimSpace(data.SQL)
	if strings.Contains(sql, "elitea_runtime.execution_http_effects") {
		c.mu.Lock()
		switch {
		case strings.HasPrefix(sql, "INSERT"):
			c.inserts++
		case strings.HasPrefix(sql, "UPDATE"):
			c.updates++
		}
		c.mu.Unlock()
	}
	return ctx
}

func (*httpWriteCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Budget: exactly one INSERT and one UPDATE on the effect table per action.
func TestHTTPEffectWriteCountPostgres(t *testing.T) {
	w := newHTTPWorld(t, "POST")
	config := w.pool.Config().Copy()
	counter := &httpWriteCounter{}
	config.ConnConfig.Tracer = counter
	traced, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	repo, _ := NewHTTPActionRepository(traced)
	admitted := w.admission(httpWorldClaim, 1)
	inv := w.invocation(0, "i")
	if _, found, err := repo.Lookup(t.Context(), admitted, inv); err != nil || found {
		t.Fatal(err)
	}
	begun, _, err := repo.Begin(t.Context(), admitted, inv, w.intent(admitted, inv))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Commit(t.Context(), admitted, completedReceipt(begun)); err != nil {
		t.Fatal(err)
	}
	if counter.inserts != 1 || counter.updates != 1 {
		t.Fatalf("writes: %d INSERT, %d UPDATE", counter.inserts, counter.updates)
	}
}
