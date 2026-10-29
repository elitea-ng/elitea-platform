package repos

// The run step records (ADR-0042 decisions 1, 2 and 4, issue #1499) against
// a REAL migrated database: a run's steps chained in arrival order, a retried
// report's step_ids skipped, a toolkit's evidence_fields applied, a rewritten
// row named by the verifier, the anchor written with the governed profile
// off, and the retention sweeps deleting content and runs past their maxima
// while a legal-held run survives. It SKIPS without ELITEA_TEST_DATABASE_URL;
// each case asserts rows a skip could not have produced.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runevidence"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// memEvidence is an in-memory runevidence.ContentStore.
type memEvidence struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (m *memEvidence) Put(_ context.Context, key string, body []byte, _ string, _ []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = append([]byte(nil), body...)
	return nil
}

func (m *memEvidence) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.objects[key], nil
}

func (m *memEvidence) Delete(_ context.Context, keys []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, key := range keys {
		delete(m.objects, key)
	}
	return nil
}

func (m *memEvidence) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok
}

const (
	stepToolA     = "0192f0c1-7b6e-7c3a-8f00-0000000000a1"
	stepToolB     = "0192f0c1-7b6e-7c3a-8f00-0000000000a2"
	stepRetrieval = "0192f0c1-7b6e-7c3a-8f00-0000000000a3"
	stepOutput    = "0192f0c1-7b6e-7c3a-8f00-0000000000a4"
)

func reportedStep(id string, kind runevidence.Kind, offset time.Duration) runevidence.StepInput {
	at := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC).Add(offset)
	return runevidence.StepInput{StepID: id, Kind: kind, StartedAt: at, EndedAt: at.Add(250 * time.Millisecond)}
}

// twoToolsAndARetrieval is the reported run: two tool calls of toolkit 17
// (whose evidence_fields keeps "id") and one retrieval.
func twoToolsAndARetrieval() []runevidence.StepInput {
	a := reportedStep(stepToolA, runevidence.KindToolCall, 0)
	a.Name, a.ToolkitID, a.SummaryJSON = "search", "17", `{"status":"ok","latency_ms":1.5e2}`
	a.Inputs = &runevidence.ContentInput{Bytes: []byte(`{"query":"q"}`), MediaType: runevidence.MediaJSON}
	a.Outputs = &runevidence.ContentInput{Bytes: []byte(`{"id":7,"ssn":"123-45-6789"}`), MediaType: runevidence.MediaJSON}
	b := reportedStep(stepToolB, runevidence.KindToolCall, time.Second)
	b.Name, b.ToolkitID = "fetch", "17"
	b.Outputs = &runevidence.ContentInput{Bytes: []byte("plain text result"), MediaType: runevidence.MediaText}
	r := reportedStep(stepRetrieval, runevidence.KindRetrieval, 2*time.Second)
	r.Retrieval = &runevidence.Retrieval{IndexName: "docs", Query: "what", TopK: 2, FiltersJSON: `{"lang":"en"}`,
		Documents: []runevidence.Document{{DocumentID: "d1", Source: "s3://x", Score: 0.91, Rank: 1}, {DocumentID: "d2", Score: 0.5, Rank: 2}}}
	return []runevidence.StepInput{a, b, r}
}

func setUpRunSteps(t *testing.T) (*pgxpool.Pool, *RunSteps, *runevidence.Service, *memEvidence) {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	store, err := ComposeRunSteps(pool)
	if err != nil {
		t.Fatal(err)
	}
	content := &memEvidence{objects: map[string][]byte{}}
	service, err := runevidence.NewService(store, content, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE SCHEMA IF NOT EXISTS p_3`,
		`CREATE TABLE IF NOT EXISTS p_3.elitea_tools (id integer PRIMARY KEY, settings jsonb)`,
		`INSERT INTO p_3.elitea_tools (id, settings) VALUES (17, '{"evidence_fields":["id"]}'), (18, '{"evidence_fields":"id"}')
		     ON CONFLICT (id) DO UPDATE SET settings = EXCLUDED.settings`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed toolkits: %v", err)
		}
	}
	return pool, store, service, content
}

// asReplica runs statement with triggers off: how a tamperer with table
// access, or a test backdating a row, bypasses the append-only trigger.
func asReplica(t *testing.T, pool *pgxpool.Pool, statement string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, `SET session_replication_role = DEFAULT`) }()
	if _, err := conn.Exec(ctx, statement, args...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

func verifyRun(t *testing.T, store *RunSteps, projectID int64, runID string) *runevidence.Break {
	t.Helper()
	ctx := context.Background()
	head, err := store.Head(ctx, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.Steps(ctx, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	return runevidence.VerifyChain(records, head.LastSeq, head.LastHash)
}

func TestRunStepsPersistChainAndVerify(t *testing.T) {
	pool, store, service, content := setUpRunSteps(t)
	ctx := context.Background()
	seedEgressExecution(t, pool, "exec-rs-1", 3, 31)

	n, err := service.Report(ctx, "exec-rs-1", 1, twoToolsAndARetrieval())
	if err != nil || n != 3 {
		t.Fatalf("report: n=%d err=%v", n, err)
	}
	records, err := store.Steps(ctx, 3, "exec-rs-1")
	if err != nil || len(records) != 3 {
		t.Fatalf("steps: %d %v", len(records), err)
	}
	wantKinds := []runevidence.Kind{runevidence.KindToolCall, runevidence.KindToolCall, runevidence.KindRetrieval}
	for i, r := range records {
		if r.Seq != int64(i+1) || r.Kind != wantKinds[i] || r.ProjectID != 3 || r.ApplicationID == nil || *r.ApplicationID != 31 ||
			r.Actor != "user:actor-1" || len(r.Hash) != 32 {
			t.Fatalf("record %d = %+v", i, r)
		}
	}
	if !bytes.Equal(records[0].PrevHash, runevidence.ZeroHash) || !bytes.Equal(records[1].PrevHash, records[0].Hash) {
		t.Fatal("the steps are not chained in arrival order")
	}

	// evidence_fields: toolkit 17 keeps "id" of a JSON result; its text
	// result cannot be filtered field by field and is not stored.
	first, second := records[0], records[1]
	kept := content.objects[first.Outputs.ObjectKey]
	if !first.Outputs.Filtered || string(kept) != `{"id":7}` || first.Outputs.OriginalSize != 28 {
		t.Fatalf("filtered outputs: %+v %s", first.Outputs, kept)
	}
	if digest := sha256.Sum256(kept); !bytes.Equal(first.Outputs.Digest, digest[:]) {
		t.Fatal("the digest is not the stored bytes'")
	}
	if !second.Outputs.Filtered || second.Outputs.Digest != nil || second.Outputs.ObjectKey != "" {
		t.Fatalf("an unfilterable result was stored: %+v", second.Outputs)
	}
	if first.Inputs.Filtered || string(content.objects[first.Inputs.ObjectKey]) != `{"query":"q"}` {
		t.Fatalf("inputs: %+v", first.Inputs)
	}
	if first.Inputs.ObjectKey != "3/runs/exec-rs-1/"+stepToolA+"/inputs" {
		t.Fatalf("object key %q", first.Inputs.ObjectKey)
	}
	if records[2].Retrieval == nil || !bytes.Contains(records[2].Retrieval, []byte(`"d2"`)) {
		t.Fatalf("retrieval = %s", records[2].Retrieval)
	}
	if b := verifyRun(t, store, 3, "exec-rs-1"); b != nil {
		t.Fatalf("a fresh run does not verify: %+v", b)
	}

	// A retried report: its stored step_ids are skipped, the new step lands
	// at seq 4 on the same chain.
	retry := append(twoToolsAndARetrieval(), reportedStep(stepOutput, runevidence.KindOutput, 3*time.Second))
	retry[3].CitedStepIDs, retry[3].CitedDocumentIDs = []string{stepRetrieval}, []string{"d1"}
	if n, err := service.Report(ctx, "exec-rs-1", 1, retry); err != nil || n != 1 {
		t.Fatalf("retry: n=%d err=%v", n, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.run_steps WHERE run_id = 'exec-rs-1'`).Scan(&rows); err != nil || rows != 4 {
		t.Fatalf("rows after retry: %d %v", rows, err)
	}
	if b := verifyRun(t, store, 3, "exec-rs-1"); b != nil {
		t.Fatalf("the run does not verify after a retry: %+v", b)
	}

	// The append-only trigger refuses a rewrite of a chained column.
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.run_steps SET name = 'other' WHERE run_id = 'exec-rs-1' AND seq = 2`); err == nil {
		t.Fatal("a chained column was rewritten")
	}

	// Anchoring with the governed profile off (no admission profile row):
	// the head lands in the tenant's security audit chain.
	if err := store.Anchor(ctx, "exec-rs-1"); err != nil {
		t.Fatal(err)
	}
	var (
		eventType, partition string
		chainHead            string
		chainLength          int64
	)
	if err := pool.QueryRow(ctx, `
SELECT event_type, partition_key, event->'data'->>'chain_head', (event->'data'->>'chain_length')::bigint
  FROM elitea_runtime.security_audit_outbox ORDER BY seq DESC LIMIT 1`).Scan(&eventType, &partition, &chainHead, &chainLength); err != nil {
		t.Fatalf("anchor row: %v", err)
	}
	head, _ := store.Head(ctx, 3, "exec-rs-1")
	if eventType != string(securityaudit.TypeRunStepsAnchored) || partition != "3" || chainLength != 4 || head.AnchoredSeq != 4 {
		t.Fatalf("anchor: %s %s %d anchored=%d", eventType, partition, chainLength, head.AnchoredSeq)
	}
	if len(chainHead) != 64 {
		t.Fatalf("chain head %q", chainHead)
	}
	// Anchoring an unchanged head writes nothing more.
	if err := store.Anchor(ctx, "exec-rs-1"); err != nil {
		t.Fatal(err)
	}
	var anchors int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.security_audit_outbox WHERE event_type = $1`, string(securityaudit.TypeRunStepsAnchored)).Scan(&anchors)
	if anchors != 1 {
		t.Fatalf("anchors = %d", anchors)
	}

	// A tamperer who bypasses the trigger is named by the verifier.
	asReplica(t, pool, `UPDATE elitea_runtime.run_steps SET name = 'rewritten' WHERE run_id = 'exec-rs-1' AND seq = 2`)
	if b := verifyRun(t, store, 3, "exec-rs-1"); b == nil || b.Seq != 2 {
		t.Fatalf("tampered seq 2: %+v", b)
	}
}

func TestRunStepsRefuseAStaleGeneration(t *testing.T) {
	pool, _, service, _ := setUpRunSteps(t)
	seedEgressExecution(t, pool, "exec-rs-stale", 3, 31)
	if _, err := service.Report(context.Background(), "exec-rs-stale", 2, twoToolsAndARetrieval()); err == nil {
		t.Fatal("a report for a generation the kernel does not hold was stored")
	}
}

func TestRunStepsRetentionRespectsLegalHolds(t *testing.T) {
	pool, store, service, content := setUpRunSteps(t)
	ctx := context.Background()
	seedEgressExecution(t, pool, "exec-rs-old", 3, 31)
	seedEgressExecution(t, pool, "exec-rs-held", 3, 32)
	for _, run := range []string{"exec-rs-old", "exec-rs-held"} {
		if _, err := service.Report(ctx, run, 1, twoToolsAndARetrieval()); err != nil {
			t.Fatal(err)
		}
		if err := store.Anchor(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	// Both runs are ten days old; agent 32 is under a legal hold. Of each
	// run only the first tool call has stored content (rows counted below).
	old := time.Now().Add(-10 * 24 * time.Hour)
	asReplica(t, pool, `UPDATE elitea_runtime.run_steps SET recorded_at = $1`, old)
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.run_step_heads SET updated_at = $1`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.legal_holds (scope, project_id, agent_id, reason, created_by) VALUES ('agent', 3, 32, 'litigation', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.retention_policy SET max_age_days = $2 WHERE scope = 'platform' AND data_class = $1`, "step_content", 1); err != nil {
		t.Fatal(err)
	}
	gate := func(context.Context) (bool, error) { return false, nil }
	sweeper, err := runevidence.NewSweeper(NewRetentionPolicyRepo(pool), store, content, gate, runevidence.RetentionConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := sweeper.SweepContent(ctx)
	if err != nil || stats.Expired != 1 || stats.Purged != 1 || stats.SkippedHold != 1 {
		t.Fatalf("content sweep: %+v %v", stats, err)
	}
	expired, err := store.Step(ctx, 3, "exec-rs-old", stepToolA)
	if err != nil {
		t.Fatal(err)
	}
	if expired.Status(runevidence.SideInputs) != runevidence.ContentExpired || expired.ContentExpiredPolicy != "retention.step_content:1d" ||
		content.has("3/runs/exec-rs-old/"+stepToolA+"/inputs") {
		t.Fatalf("expired step: %+v", expired)
	}
	held, _ := store.Step(ctx, 3, "exec-rs-held", stepToolA)
	if held.Status(runevidence.SideInputs) != runevidence.ContentStored || !content.has(held.Inputs.ObjectKey) {
		t.Fatalf("a held run's content was expired: %+v", held)
	}
	// Expired content leaves a chain that still verifies.
	if b := verifyRun(t, store, 3, "exec-rs-old"); b != nil {
		t.Fatalf("expired content broke the chain: %+v", b)
	}

	// step_record: whole runs past the maximum go, the held one stays.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.retention_policy SET max_age_days = $2 WHERE scope = 'platform' AND data_class = $1`, "step_record", 7); err != nil {
		t.Fatal(err)
	}
	records, err := sweeper.SweepRecords(ctx)
	if err != nil || records.DeletedRuns != 1 {
		t.Fatalf("record sweep: %+v %v", records, err)
	}
	if _, err := store.Head(ctx, 3, "exec-rs-old"); err != ErrRunNotFound {
		t.Fatalf("the expired run survived: %v", err)
	}
	if b := verifyRun(t, store, 3, "exec-rs-held"); b != nil {
		t.Fatalf("the held run does not verify: %+v", b)
	}
}

func TestConversationDeleteExpiresItsStepContent(t *testing.T) {
	pool, store, service, _ := setUpRunSteps(t)
	ctx := context.Background()
	const conversation = "0192f0c1-7b6e-7c3a-8f00-00000000c0c0"
	seedEgressExecution(t, pool, "exec-rs-conv", 3, 31)
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.agent_execution_jobs SET client_stream_id = $1 WHERE execution_id = 'exec-rs-conv'`, conversation); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Report(ctx, "exec-rs-conv", 1, twoToolsAndARetrieval()); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ConversationRuns(ctx, 3, conversation)
	if err != nil || len(runs) != 1 || runs[0].RunID != "exec-rs-conv" || runs[0].Steps != 3 {
		t.Fatalf("conversation runs: %+v %v", runs, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := expireConversationStepContent(ctx, tx, "3", conversation); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	step, _ := store.Step(ctx, 3, "exec-rs-conv", stepToolA)
	if step.Status(runevidence.SideInputs) != runevidence.ContentExpired || step.ContentExpiredPolicy != runevidence.PolicyConversationDeleted {
		t.Fatalf("step after the conversation delete: %+v", step)
	}
}
