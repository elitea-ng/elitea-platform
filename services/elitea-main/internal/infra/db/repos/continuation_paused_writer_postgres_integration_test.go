package repos

// FU-265: the paused writer a continuation names is the response's paused
// execution and the generation its paused terminal settled under, read in the
// deciding project's transaction. Requires ELITEA_TEST_DATABASE_URL; it skips
// without it.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
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

func TestPostgresPausedWriterIsTheResponsesSettledPausedExecution(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	queries := sqlcgen.New(tx)
	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	paused := "0123456789abcdef0123456789abcdef"
	responseID := insertPostgresCurrentApplicationTurn(
		t, queries, conversationID, "20000000-0000-4000-8000-000000000265",
		"30000000-0000-4000-8000-000000000265", "40000000-0000-4000-8000-000000000265",
		"approve the call", paused,
	)
	executor := pgxExecutor{queryer: tx}
	refused := func(name string, lock bool) {
		t.Helper()
		if _, _, err := readPausedWriter(t.Context(), executor, responseID, lock); !errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
			t.Fatalf("%s: %v, want a refusal", name, err)
		}
	}
	refused("no settled terminal", false)

	// A run reclaimed before it paused settles under a later generation; a
	// failed attempt of another generation is not the paused terminal.
	settlePausedFixture(t, tx, paused, 1, "FAILED")
	settlePausedFixture(t, tx, paused, 3, "SUCCEEDED")
	for _, lock := range []bool{false, true} {
		execution, generation, err := readPausedWriter(t.Context(), executor, responseID, lock)
		if err != nil || execution != paused || generation != 3 {
			t.Fatalf("lock=%v: %s %d %v", lock, execution, generation, err)
		}
	}

	// Admission re-reads it under the response lock: only the named writer
	// is admitted.
	turn := agentexecutionapp.CurrentContinueTurn{
		ContinuationKind:  agentexecutionapp.CurrentContinuationHITL,
		ResponseMessageID: "40000000-0000-4000-8000-000000000265",
		PausedExecutionID: paused, PausedGeneration: 3,
	}
	if err := verifyPausedWriter(t.Context(), tx, turn); err != nil {
		t.Fatalf("named writer: %v", err)
	}
	for name, mutate := range map[string]func(*agentexecutionapp.CurrentContinueTurn){
		"another generation": func(turn *agentexecutionapp.CurrentContinueTurn) { turn.PausedGeneration = 2 },
		"another execution": func(turn *agentexecutionapp.CurrentContinueTurn) {
			turn.PausedExecutionID = "fedcba9876543210fedcba9876543210"
		},
		"none named": func(turn *agentexecutionapp.CurrentContinueTurn) { turn.PausedExecutionID = "" },
	} {
		moved := turn
		mutate(&moved)
		if err := verifyPausedWriter(t.Context(), tx, moved); !errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
			t.Fatalf("%s: %v, want a refusal", name, err)
		}
	}
	// An output-limit continuation names no paused writer.
	if err := verifyPausedWriter(t.Context(), tx, agentexecutionapp.CurrentContinueTurn{
		ContinuationKind: agentexecutionapp.CurrentContinuationOutputLimit,
	}); err != nil {
		t.Fatalf("output limit: %v", err)
	}

	// Two settled terminals: never pick one.
	settlePausedFixture(t, tx, paused, 4, "SUCCEEDED")
	refused("two settled terminals", true)
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
	paused := "0123456789abcdef0123456789abcdef"
	responseID := insertPostgresCurrentApplicationTurn(
		t, sqlcgen.New(tx), mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031"),
		"20000000-0000-4000-8000-000000000266", "30000000-0000-4000-8000-000000000266",
		"40000000-0000-4000-8000-000000000266", "approve the call", paused,
	)
	settlePausedFixture(t, tx, paused, 1, "SUCCEEDED")
	if _, _, err := readPausedWriter(t.Context(), pgxExecutor{queryer: tx}, responseID, false); err != nil {
		t.Fatalf("own project: %v", err)
	}
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 2}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPausedWriter(t.Context(), pgxExecutor{queryer: tx}, responseID, false); !errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
		t.Fatalf("foreign project: %v, want a refusal", err)
	}
}
