package repos

// FU-269: the paused writer a continuation re-enters is the response's paused
// execution and the generation its paused terminal settled under. The
// admitting transaction reads it under the response lock, before the resume
// consumes the pause, stores it on the job, and the claim serves it back.
// Requires ELITEA_TEST_DATABASE_URL; it skips without it.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
)

const (
	pausedWriterFixture     = "0123456789abcdef0123456789abcdef"
	continuingWriterFixture = "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b"
)

// settlePausedFixture records a settled terminal of execution/generation. The
// settlement's job and claim rows are not under test here, so this fixture
// inserts it with the transaction's foreign-key triggers off.
func settlePausedFixture(t *testing.T, tx pgx.Tx, execution string, generation int64, disposition string) {
	t.Helper()
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", execution, generation)))
	if _, err := tx.Exec(t.Context(), `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
INSERT INTO elitea_runtime.execution_settlements (
    execution_id, generation, claim_id, fence_token, workload_identity, workload_session_id, producer_id,
    claim_attempt, lease_epoch, settlement_receipt_id, proposal_id, proposal_bytes, proposal_digest,
    idempotency_key, disposition, final_logical_output_id, terminal_event_id, terminal_sequence,
    terminal_payload_digest)
VALUES ($1, $2, 'claim-' || $1 || '-' || $5, $3, 'worker', 'session', 'producer', 1, 1,
        'receipt-' || $1 || '-' || $5, 'proposal', '\x01', $3, 'key-' || $1 || '-' || $5, $4,
        'output', 'event-' || $1 || '-' || $5, 1, $3)`,
		execution, generation, digest[:], disposition, fmt.Sprint(generation)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `SET LOCAL session_replication_role = origin`); err != nil {
		t.Fatal(err)
	}
}

// admittedJobFixture inserts the continuation's agent job row, as the
// admission's InsertAgentExecutionBinding does, with its parents' foreign keys
// off (they are not under test here). They stay off for the rest of the
// transaction: PostgreSQL re-checks them when a row this transaction inserted
// is updated. Check constraints still hold. The production-path test below
// runs with every key on.
func admittedJobFixture(t *testing.T, tx pgx.Tx, execution string) {
	t.Helper()
	if _, err := tx.Exec(t.Context(), `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
INSERT INTO elitea_runtime.agent_execution_jobs (
    execution_id, generation, capability_id, input_bundle_id, request_entry_id,
    client_stream_id, client_message_id, client_execution_generation, sio_event)
VALUES ($1, 1, 'agent.execute.application.v1', 'bundle-' || $1, 'request', 'stream', 'message', 'generation',
        'chat_continue_predict')`, execution); err != nil {
		t.Fatal(err)
	}
}

func storedPausedWriter(t *testing.T, tx pgx.Tx, execution string) (*string, *int64) {
	t.Helper()
	var pausedExecution *string
	var pausedGeneration *int64
	if err := tx.QueryRow(t.Context(), `
SELECT paused_execution_id, paused_generation FROM elitea_runtime.agent_execution_jobs
WHERE execution_id = $1 AND generation = 1`, execution).Scan(&pausedExecution, &pausedGeneration); err != nil {
		t.Fatal(err)
	}
	return pausedExecution, pausedGeneration
}

// pausedWriterTx opens a project-1 transaction holding one paused response of
// the given paused execution.
func pausedWriterTx(t *testing.T, suffix, pausedExecution string) (pgx.Tx, pgtype.UUID, string) {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	response := "40000000-0000-4000-8000-000000000" + suffix
	responseID := insertPostgresCurrentApplicationTurn(
		t, sqlcgen.New(tx), mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031"),
		"20000000-0000-4000-8000-000000000"+suffix, "30000000-0000-4000-8000-000000000"+suffix,
		response, "approve the call", pausedExecution,
	)
	return tx, responseID, response
}

func TestPostgresPausedWriterIsTheResponsesSettledPausedExecution(t *testing.T) {
	tx, responseID, _ := pausedWriterTx(t, "265", pausedWriterFixture)
	executor := pgxExecutor{queryer: tx}
	none := func(name string, lock bool) {
		t.Helper()
		if _, _, err := readPausedWriter(t.Context(), executor, responseID, lock); !errors.Is(err, errNoPausedWriter) {
			t.Fatalf("%s: %v, want none", name, err)
		}
	}
	none("no settled terminal", false)

	// Only a SUCCEEDED settlement is the paused terminal. An agent execution
	// settles at generation 1 today (it is admitted at 1 and a reclaim keeps
	// it, agentexecution/admission.go and repos/claims.go claim attempts); the
	// reader takes the settled generation as stored rather than assuming 1,
	// which generation 3 here proves.
	settlePausedFixture(t, tx, pausedWriterFixture, 1, "FAILED")
	settlePausedFixture(t, tx, pausedWriterFixture, 3, "SUCCEEDED")
	for _, lock := range []bool{false, true} {
		execution, generation, err := readPausedWriter(t.Context(), executor, responseID, lock)
		if err != nil || execution != pausedWriterFixture || generation != 3 {
			t.Fatalf("lock=%v: %s %d %v", lock, execution, generation, err)
		}
	}

	// Two settled terminals break the one-settlement invariant: never pick
	// one, and warn with ids and generations only.
	var captured lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&captured, nil)))
	defer slog.SetDefault(previous)
	settlePausedFixture(t, tx, pausedWriterFixture, 4, "SUCCEEDED")
	none("two settled terminals", true)
	logged := captured.String()
	if !strings.Contains(logged, "more than one settled terminal") ||
		!strings.Contains(logged, pausedWriterFixture) || !strings.Contains(logged, "generations=") {
		t.Fatalf("the invariant break is not logged with its ids: %q", logged)
	}
	for _, private := range []string{"approve the call", "40000000-0000-4000-8000-000000000265"} {
		if strings.Contains(logged, private) {
			t.Fatalf("the warning logs %q: %q", private, logged)
		}
	}
}

// C5: the read runs in the deciding project's transaction, so a decision in
// another project cannot name this project's paused execution.
func TestPostgresPausedWriterIsReadOnlyInTheDecidingProject(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	if _, err := pool.Exec(t.Context(), `
INSERT INTO centry.project (id, create_success, suspended) VALUES (2, TRUE, FALSE) ON CONFLICT (id) DO NOTHING;
CREATE SCHEMA IF NOT EXISTS p_2;
CREATE TABLE IF NOT EXISTS p_2.chat_message_group (LIKE p_1.chat_message_group INCLUDING DEFAULTS)`); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	responseID := insertPostgresCurrentApplicationTurn(
		t, sqlcgen.New(tx), mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031"),
		"20000000-0000-4000-8000-000000000266", "30000000-0000-4000-8000-000000000266",
		"40000000-0000-4000-8000-000000000266", "approve the call", pausedWriterFixture,
	)
	settlePausedFixture(t, tx, pausedWriterFixture, 1, "SUCCEEDED")
	if _, _, err := readPausedWriter(t.Context(), pgxExecutor{queryer: tx}, responseID, false); err != nil {
		t.Fatalf("own project: %v", err)
	}
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 2}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPausedWriter(t.Context(), pgxExecutor{queryer: tx}, responseID, false); !errors.Is(err, errNoPausedWriter) {
		t.Fatalf("foreign project: %v, want none", err)
	}
}

// The admitting transaction stores the writer it read before the resume
// moves the response to the continuation; later changes to the response do
// not move it, and nothing writes it a second time.
func TestPostgresContinuationAdmissionStoresThePausedWriterOnItsJob(t *testing.T) {
	tx, _, response := pausedWriterTx(t, "269", pausedWriterFixture)
	settlePausedFixture(t, tx, pausedWriterFixture, 1, "FAILED")
	settlePausedFixture(t, tx, pausedWriterFixture, 3, "SUCCEEDED")
	admittedJobFixture(t, tx, continuingWriterFixture)
	turn := agentexecutionapp.CurrentContinueTurn{
		ContinuationKind: agentexecutionapp.CurrentContinuationHITL, ResponseMessageID: response,
	}
	if err := recordPausedWriter(t.Context(), tx, continuingWriterFixture, 1, turn); err != nil {
		t.Fatal(err)
	}
	// The resume consumes the pause: the response now names the continuation.
	if _, err := tx.Exec(t.Context(), `UPDATE chat_message_group SET task_id = $1 WHERE uuid = $2::uuid`,
		continuingWriterFixture, response); err != nil {
		t.Fatal(err)
	}
	execution, generation := storedPausedWriter(t, tx, continuingWriterFixture)
	if execution == nil || *execution != pausedWriterFixture || generation == nil || *generation != 3 {
		t.Fatalf("stored writer %v %v", execution, generation)
	}
	// Even a response that later names another settled execution never
	// rewrites the stored writer.
	const later = "0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c"
	settlePausedFixture(t, tx, later, 1, "SUCCEEDED")
	if _, err := tx.Exec(t.Context(), `UPDATE chat_message_group SET task_id = $1 WHERE uuid = $2::uuid`,
		later, response); err != nil {
		t.Fatal(err)
	}
	if err := recordPausedWriter(t.Context(), tx, continuingWriterFixture, 1, turn); err == nil {
		t.Fatal("a second write of the paused writer was accepted")
	}
	if execution, generation := storedPausedWriter(t, tx, continuingWriterFixture); *execution != pausedWriterFixture || *generation != 3 {
		t.Fatalf("the stored writer moved to %v %v", *execution, *generation)
	}

	// The column checks hold whatever writes the row.
	for name, update := range map[string]string{
		"its own execution": `UPDATE elitea_runtime.agent_execution_jobs SET paused_execution_id = execution_id WHERE execution_id = $1`,
		"generation 0":      `UPDATE elitea_runtime.agent_execution_jobs SET paused_generation = 0 WHERE execution_id = $1`,
		"half a writer":     `UPDATE elitea_runtime.agent_execution_jobs SET paused_generation = NULL WHERE execution_id = $1`,
		"not an id":         `UPDATE elitea_runtime.agent_execution_jobs SET paused_execution_id = 'execution-paused' WHERE execution_id = $1`,
	} {
		if _, err := tx.Exec(t.Context(), `SAVEPOINT paused_writer_check`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), update, continuingWriterFixture); err == nil {
			t.Fatalf("%s: the check accepted it", name)
		}
		if _, err := tx.Exec(t.Context(), `ROLLBACK TO SAVEPOINT paused_writer_check`); err != nil {
			t.Fatal(err)
		}
	}
}

// Every case where the continuation cannot name exactly one writer stores
// none: the continuation runs as before.
func TestPostgresContinuationAdmissionNamesNoWriterWithoutOneSettledPause(t *testing.T) {
	for _, tc := range []struct {
		name, suffix, paused string
		kind                 agentexecutionapp.CurrentContinuationKind
		settle               func(t *testing.T, tx pgx.Tx)
	}{
		{name: "no settled terminal", suffix: "270", paused: pausedWriterFixture,
			kind: agentexecutionapp.CurrentContinuationHITL},
		{name: "two settled terminals", suffix: "271", paused: pausedWriterFixture,
			kind: agentexecutionapp.CurrentContinuationStatic, settle: func(t *testing.T, tx pgx.Tx) {
				settlePausedFixture(t, tx, pausedWriterFixture, 1, "SUCCEEDED")
				settlePausedFixture(t, tx, pausedWriterFixture, 2, "SUCCEEDED")
			}},
		{name: "an output limit", suffix: "272", paused: pausedWriterFixture,
			kind: agentexecutionapp.CurrentContinuationOutputLimit, settle: func(t *testing.T, tx pgx.Tx) {
				settlePausedFixture(t, tx, pausedWriterFixture, 1, "SUCCEEDED")
			}},
		{name: "not a runtime execution id", suffix: "273", paused: "execution-paused",
			kind: agentexecutionapp.CurrentContinuationAuthorization, settle: func(t *testing.T, tx pgx.Tx) {
				settlePausedFixture(t, tx, "execution-paused", 1, "SUCCEEDED")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, _, response := pausedWriterTx(t, tc.suffix, tc.paused)
			if tc.settle != nil {
				tc.settle(t, tx)
			}
			admittedJobFixture(t, tx, continuingWriterFixture)
			if err := recordPausedWriter(t.Context(), tx, continuingWriterFixture, 1, agentexecutionapp.CurrentContinueTurn{
				ContinuationKind: tc.kind, ResponseMessageID: response,
			}); err != nil {
				t.Fatal(err)
			}
			if execution, generation := storedPausedWriter(t, tx, continuingWriterFixture); execution != nil || generation != nil {
				t.Fatalf("stored writer %v %v", execution, generation)
			}
		})
	}
}

// The claim serves the stored writer only on an executable or recovering
// agent claim of that job.
func TestPostgresClaimServesThePausedWriterOnlyOnAgentWork(t *testing.T) {
	tx, _, _ := pausedWriterTx(t, "274", pausedWriterFixture)
	admittedJobFixture(t, tx, continuingWriterFixture)
	if _, err := tx.Exec(t.Context(), `
UPDATE elitea_runtime.agent_execution_jobs SET paused_execution_id = $2, paused_generation = 3
WHERE execution_id = $1`, continuingWriterFixture, pausedWriterFixture); err != nil {
		t.Fatal(err)
	}
	executor := pgxExecutor{queryer: tx}
	claim := func(capability string, generation uint64, disposition executionapp.ClaimDisposition) *executionapp.PausedWriter {
		t.Helper()
		decision := executionapp.ClaimDecision{Disposition: disposition}
		if err := selectClaimPausedWriter(t.Context(), executor, executionapp.ClaimRequest{
			ExecutionID: continuingWriterFixture, Generation: generation, CapabilityID: capability,
		}, &decision); err != nil {
			t.Fatal(err)
		}
		return decision.PausedWriter
	}
	for _, disposition := range []executionapp.ClaimDisposition{
		executionapp.ClaimAccepted, executionapp.ClaimRecoverAgentModelCheckpoint, executionapp.ClaimRecoverNodeVisit,
	} {
		writer := claim(executiondomain.AgentApplicationCapability, 1, disposition)
		if writer == nil || writer.ExecutionID != pausedWriterFixture || writer.Generation != 3 {
			t.Fatalf("%s: %+v", disposition, writer)
		}
	}
	for _, disposition := range []executionapp.ClaimDisposition{
		executionapp.ClaimActiveLeaseNoACK, executionapp.ClaimRecoverTerminalACK, executionapp.ClaimRecoverRunningNoACK,
		executionapp.ClaimRecoverAmbiguousInvocationNoACK, executionapp.ClaimRetryLaterNoACK,
	} {
		if writer := claim(executiondomain.AgentApplicationCapability, 1, disposition); writer != nil {
			t.Fatalf("%s served %+v", disposition, writer)
		}
	}
	if writer := claim(executiondomain.ToolkitCallToolCapability, 1, executionapp.ClaimAccepted); writer != nil {
		t.Fatalf("another capability served %+v", writer)
	}
	if writer := claim(executiondomain.AgentApplicationCapability, 2, executionapp.ClaimAccepted); writer != nil {
		t.Fatalf("another generation served %+v", writer)
	}
}

// End to end through the admission service and the production claim path: a
// fresh run's claim carries no writer, and a job holding one has it served on
// its accepted claim and passing the claim service's validation.
func TestPostgresProductionClaimPathServesTheJobsPausedWriter(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, fresh := admitAndClaimAgentRun(t, pool, "fu269-fresh", executiondomain.AgentApplicationCapability,
		agentCapacityInput("gen-fu269"), nil)
	if fresh.PausedWriter != nil {
		t.Fatalf("fresh run served %+v", fresh.PausedWriter)
	}
	_, continued := admitAndClaimAgentRun(t, pool, "fu269-continued", executiondomain.AgentApplicationCapability,
		agentCapacityInput("gen-fu269"), func(execution string) {
			if _, err := pool.Exec(t.Context(), `
UPDATE elitea_runtime.agent_execution_jobs SET paused_execution_id = $2, paused_generation = 3
WHERE execution_id = $1`, execution, pausedWriterFixture); err != nil {
				t.Fatal(err)
			}
		})
	if writer := continued.PausedWriter; writer == nil || writer.ExecutionID != pausedWriterFixture || writer.Generation != 3 {
		t.Fatalf("continued run served %+v", writer)
	}
}
