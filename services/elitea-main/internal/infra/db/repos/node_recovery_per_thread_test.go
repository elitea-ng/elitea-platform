package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	app "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/noderecovery"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func fanoutChildReceiptFixture(t *testing.T, threadID, activationHex string) (domain.Receipt, []byte, []byte) {
	t.Helper()
	r := domain.Receipt{
		Schema:          domain.Schema,
		ActivationID:    activationHex,
		JournalRevision: 3,
		NodeID:          "validate",
		GraphThread:     threadID,
		Step:            2,
		Attempt:         1,
		FailureClass:    "dependency_unavailable",
		StopReason:      "operator_approval_required",
		ReplaySafety:    domain.ReplaySafety{Kind: "no_external_effect"},
		AllowedActions:  []string{"retry"},
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = domain.CanonicalReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	d := sha256.Sum256(raw)
	return r, raw, d[:]
}

// FU-071 (5b-5): operator recovery pending visits, projection, poll, ack, and
// exact replay operate per graph_thread without sibling starvation or collision.
func TestNodeRecoveryPerThreadProjectionReadPollAckAndReplay(t *testing.T) {
	t.Parallel()
	threadA := "p1:Z8y5h_dA4WoUJhoL5KeWzytJ6KsEHvKFo9OBSMzxW6Q"
	threadB := "m1:KRe5343ZNGVAvfuZ-MJ-RAfTgyFfIxbwR_9OVB4NU24"
	actA := strings.Repeat("a", 64)
	receiptA, rawA, digestA := fanoutChildReceiptFixture(t, threadA, actA)

	t.Run("projection allows second child thread when job already suspended", func(t *testing.T) {
		t.Parallel()
		frame := testNodeEventFrame()
		frame.ResourceProjectID = "7"
		frame.ProjectionProjectID = "7"
		frame.TenantID = "7"
		frame.BrowserData, _ = json.Marshal(map[string]any{
			"type":                 "agent_node_recovery_required",
			"stream_id":            "stream-1",
			"message_id":           "message-1",
			"execution_generation": "logical-1",
			"sio_event":            "chat_predict",
			"response_metadata":    map[string]any{"node_recovery_required_v1": json.RawMessage(rawA)},
		})
		e := &recoveryProjectionExecutor{
			scriptedExecutor: &scriptedExecutor{
				rowResults: []scriptedRow{
					{err: pgx.ErrNoRows},
					{values: []any{"claim-node-1"}},
					{values: []any{int64(0), "", []byte{}, []byte{}, int64(0), int64(0), int64(0), int64(0)}},
					{values: []any{int64(17), "claim-node-1", "SUSPENDED", executiondomain.AgentApplicationCapability, false, false}},
					{values: []any{int64(41)}},
				},
				execTags: []pgconn.CommandTag{
					pgconn.NewCommandTag("INSERT 0 1"),
					pgconn.NewCommandTag("INSERT 0 1"),
					pgconn.NewCommandTag("UPDATE 1"),
					pgconn.NewCommandTag("UPDATE 1"),
				},
			},
			binding: sqlcgen.GetCurrentAgentTraceBindingRow{
				ClientStreamID:            "stream-1",
				ClientMessageID:           "message-1",
				ClientExecutionGeneration: "logical-1",
				SioEvent:                  "chat_predict",
			},
		}
		s := &recoveryProjectionStore{recoveryProjectionExecutor: e}
		r, err := newNodeEventsRepository(s)
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := r.ProjectNodeEvent(t.Context(), frame)
		if err != nil || !s.committed || !outcome.Inserted {
			t.Fatalf("outcome=%+v err=%v committed=%v", outcome, err, s.committed)
		}
		if !strings.Contains(e.execCalls[2].sql, "desired_state IN ('RUNNING','SUSPENDED')") {
			t.Fatalf("projection did not allow already-suspended sibling job: %s", e.execCalls[2].sql)
		}
	})

	t.Run("read filters by graph_thread and refuses mismatched receipt thread", func(t *testing.T) {
		t.Parallel()
		sel := recoverySubmission().Selector
		sel.GraphThread = threadA
		e := &scriptedExecutor{rowResults: []scriptedRow{
			{values: []any{recoverySubmission().Request.ExecutionID, uint64(1), "SUSPENDED"}},
			{values: []any{"SUSPENDED", rawA, digestA}},
		}}
		s := &recoveryTxStore{scriptedExecutor: e}
		state, err := recoveryRepo(t, s).Read(t.Context(), sel)
		if err != nil || !s.committed || state.Status != "suspended" || string(state.Receipt) != string(rawA) {
			t.Fatalf("state=%+v err=%v committed=%v", state, err, s.committed)
		}
		q := e.rowCalls[1]
		if !strings.Contains(q.sql, "AND graph_thread=$3") || len(q.args) != 3 || q.args[2] != threadA {
			t.Fatalf("read query not scoped by graph_thread: %+v", q)
		}

		// Tampered/mismatched thread inside stored receipt must fail closed.
		selOther := sel
		selOther.GraphThread = threadB
		eMismatch := &scriptedExecutor{rowResults: []scriptedRow{
			{values: []any{recoverySubmission().Request.ExecutionID, uint64(1), "SUSPENDED"}},
			{values: []any{"SUSPENDED", rawA, digestA}},
		}}
		sMismatch := &recoveryTxStore{scriptedExecutor: eMismatch}
		if _, err := recoveryRepo(t, sMismatch).Read(t.Context(), selOther); !errors.Is(err, app.ErrNotAllowed) || sMismatch.committed {
			t.Fatalf("mismatched thread accepted: err=%v committed=%v", err, sMismatch.committed)
		}
	})

	t.Run("poll prioritizes authorized child thread over newer suspended sibling", func(t *testing.T) {
		t.Parallel()
		sha := hex.EncodeToString(digestA)
		actionBytes, _ := json.Marshal(recoveryAction{
			RequestID:        strings.Repeat("2", 64),
			ActivationID:     actA,
			ExpectedRevision: 3,
			LastAttempt:      1,
			Action:           "retry",
			ReceiptSHA256:    sha,
		})
		rows := append(
			recoveryClaimRows(),
			scriptedRow{values: []any{rawA, digestA, actA, uint64(3)}},
			scriptedRow{values: []any{actionBytes}},
		)
		e := &scriptedExecutor{rowResults: rows}
		s := &recoveryTxStore{scriptedExecutor: e}
		control, err := recoveryRepo(t, s).PollNodeRecovery(t.Context(), recoveryClaim())
		if err != nil || !s.committed || string(control.Receipt) != string(rawA) || string(control.Action) != string(actionBytes) {
			t.Fatalf("control=%+v err=%v committed=%v", control, err, s.committed)
		}
		if !strings.Contains(e.rowCalls[2].sql, "WHEN 'AUTHORIZED' THEN 0") {
			t.Fatalf("poll did not prioritize AUTHORIZED visits: %s", e.rowCalls[2].sql)
		}
	})

	t.Run("ack locks latest per graph_thread and preserves suspended while sibling pending", func(t *testing.T) {
		t.Parallel()
		sha := hex.EncodeToString(digestA)
		reqID := strings.Repeat("2", 64)
		actionBytes, _ := json.Marshal(recoveryAction{
			RequestID:        reqID,
			ActivationID:     actA,
			ExpectedRevision: 3,
			LastAttempt:      1,
			Action:           "retry",
			ReceiptSHA256:    sha,
		})
		ack := storage.NodeRecoveryAck{
			RequestID:        reqID,
			ActivationID:     actA,
			ExpectedRevision: 3,
			ReceiptSHA256:    sha,
			AppliedRevision:  4,
		}
		rows := append(
			recoveryClaimRows(),
			scriptedRow{values: []any{receiptA.ActivationID, receiptA.JournalRevision}},
			scriptedRow{values: []any{rawA, digestA, "AUTHORIZED", actionBytes, "11", "", uint64(0), []byte(nil)}},
		)
		e := &scriptedExecutor{
			rowResults: rows,
			execTags: []pgconn.CommandTag{
				pgconn.NewCommandTag("UPDATE 1"),
				pgconn.NewCommandTag("UPDATE 1"),
				pgconn.NewCommandTag("UPDATE 1"),
				pgconn.NewCommandTag("INSERT 0 1"),
			},
		}
		s := &recoveryTxStore{scriptedExecutor: e}
		out, err := recoveryRepo(t, s).AcknowledgeNodeRecovery(t.Context(), recoveryClaim(), ack)
		if err != nil || !s.committed || !out.RecoveryResumeAuthorized {
			t.Fatalf("out=%+v err=%v committed=%v", out, err, s.committed)
		}
		if !strings.Contains(e.rowCalls[2].sql, "graph_thread=target.graph_thread") {
			t.Fatalf("ack current-visit lock not scoped per graph_thread: %s", e.rowCalls[2].sql)
		}
		if !strings.Contains(e.execCalls[2].sql, "CASE WHEN EXISTS(SELECT 1 FROM elitea_runtime.node_recovery_visits") {
			t.Fatalf("ack job desired_state update does not preserve SUSPENDED for pending siblings: %s", e.execCalls[2].sql)
		}
	})

	t.Run("exact replay of resumed child succeeds while sibling thread still pending and fails otherwise", func(t *testing.T) {
		t.Parallel()
		sha := hex.EncodeToString(digestA)
		reqID := strings.Repeat("2", 64)
		actionBytes, _ := json.Marshal(recoveryAction{
			RequestID:        reqID,
			ActivationID:     actA,
			ExpectedRevision: 3,
			LastAttempt:      1,
			Action:           "retry",
			ReceiptSHA256:    sha,
		})
		ack := storage.NodeRecoveryAck{
			RequestID:        reqID,
			ActivationID:     actA,
			ExpectedRevision: 3,
			ReceiptSHA256:    sha,
			AppliedRevision:  4,
		}
		savedAck, _ := canonicalRecoveryAck(ack)
		for _, siblingPending := range []bool{true, false} {
			rows := append(
				recoveryClaimRows(),
				scriptedRow{values: []any{receiptA.ActivationID, receiptA.JournalRevision}},
				scriptedRow{values: []any{rawA, digestA, "RESUMED", actionBytes, "11", "prior-claim", uint64(4), savedAck}},
				scriptedRow{values: []any{siblingPending}},
			)
			e := &scriptedExecutor{rowResults: rows}
			s := &recoveryTxStore{scriptedExecutor: e}
			out, err := recoveryRepo(t, s).AcknowledgeNodeRecovery(t.Context(), recoveryClaim(), ack)
			if siblingPending {
				if err != nil || !s.committed || !out.Replay || !out.RecoveryResumeAuthorized || len(e.execCalls) != 0 {
					t.Fatalf("valid multi-thread replay rejected: out=%+v err=%v committed=%v", out, err, s.committed)
				}
			} else {
				if !errors.Is(err, storage.ErrContentRejected) || s.committed || len(e.execCalls) != 0 {
					t.Fatalf("inconsistent suspended replay accepted without pending sibling: err=%v committed=%v", err, s.committed)
				}
			}
		}
	})
}

// T-5B §5 negative authz test: a foreign actor or foreign project cannot read
// or authorize a fan-out child node-recovery visit.
func TestNodeRecoveryFanoutChildVisitRefusesForeignActorAndForeignProject(t *testing.T) {
	t.Parallel()
	childThread := "p1:Z8y5h_dA4WoUJhoL5KeWzytJ6KsEHvKFo9OBSMzxW6Q"
	actID := strings.Repeat("c", 64)
	_, raw, digest := fanoutChildReceiptFixture(t, childThread, actID)

	for _, tc := range []struct {
		name      string
		actorID   int64
		projectID int64
		permDeny  bool
	}{
		{name: "foreign actor in same project", actorID: 99, projectID: 7},
		{name: "foreign project", actorID: 11, projectID: 88},
		{name: "revoked permission", actorID: 11, projectID: 7, permDeny: true},
	} {
		tc := tc
		t.Run("read/"+tc.name, func(t *testing.T) {
			t.Parallel()
			e := &scriptedExecutor{rowResults: []scriptedRow{{err: pgx.ErrNoRows}, {values: []any{"SUSPENDED", raw, digest}}}}
			s := &recoveryTxStore{scriptedExecutor: e}
			repo := &NodeRecoveryRepository{
				projects: s,
				shared:   s,
				permissions: func(_ context.Context, _ sqlExecutor, _ app.Selector, _ string) error {
					if tc.permDeny {
						return app.ErrNotAllowed
					}
					return nil
				},
			}
			sel := app.Selector{
				ProjectID:         tc.projectID,
				ActorUserID:       tc.actorID,
				ResponseMessageID: "10000000-0000-4000-8000-000000000043",
				GraphThread:       childThread,
			}
			_, err := repo.Read(t.Context(), sel)
			if !errors.Is(err, app.ErrNotAllowed) || s.committed || len(e.execCalls) != 0 {
				t.Fatalf("foreign read escaped: err=%v committed=%v writes=%d", err, s.committed, len(e.execCalls))
			}
		})

		t.Run("submit/"+tc.name, func(t *testing.T) {
			t.Parallel()
			e := &scriptedExecutor{rowResults: []scriptedRow{{err: pgx.ErrNoRows}}}
			s := &recoveryTxStore{scriptedExecutor: e}
			repo := &NodeRecoveryRepository{
				projects: s,
				shared:   s,
				permissions: func(_ context.Context, _ sqlExecutor, _ app.Selector, _ string) error {
					if tc.permDeny {
						return app.ErrNotAllowed
					}
					return nil
				},
			}
			sub := app.Submission{
				Selector: app.Selector{
					ProjectID:         tc.projectID,
					ActorUserID:       tc.actorID,
					ResponseMessageID: "10000000-0000-4000-8000-000000000043",
				},
				Request: domain.Request{
					RequestID:        strings.Repeat("2", 64),
					ExecutionID:      "0123456789abcdef0123456789abcdef",
					Generation:       1,
					ActivationID:     actID,
					ExpectedRevision: 3,
					Action:           "retry",
				},
			}
			_, err := repo.Submit(t.Context(), sub)
			if !errors.Is(err, app.ErrNotAllowed) || s.committed || len(e.execCalls) != 0 {
				t.Fatalf("foreign submit escaped: err=%v committed=%v writes=%d", err, s.committed, len(e.execCalls))
			}
		})
	}

	// Ensure unused-bytes import is avoided if refactored.
	_ = bytes.Equal
}
