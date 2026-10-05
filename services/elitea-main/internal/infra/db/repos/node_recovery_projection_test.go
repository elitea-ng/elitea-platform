package repos

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type recoveryProjectionExecutor struct {
	*scriptedExecutor
	binding sqlcgen.GetCurrentAgentTraceBindingRow
}

func (e *recoveryProjectionExecutor) GetCurrentAgentTraceBinding(context.Context, sqlcgen.GetCurrentAgentTraceBindingParams) (sqlcgen.GetCurrentAgentTraceBindingRow, error) {
	return e.binding, nil
}

type recoveryProjectionStore struct {
	*recoveryProjectionExecutor
	committed bool
}

func (s *recoveryProjectionStore) WithinTx(ctx context.Context, _ pgx.TxOptions, fn func(sqlExecutor) error) error {
	err := fn(s.recoveryProjectionExecutor)
	s.committed = err == nil
	return err
}

func TestNodeRecoveryProjectionSharesAcceptedEventAndSuspensionTransaction(t *testing.T) {
	_, receipt, _ := recoveryFixture(t)
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "suspension fails"}[fail], func(t *testing.T) {
			frame := testNodeEventFrame()
			frame.ResourceProjectID = "7"
			frame.ProjectionProjectID = "7"
			frame.TenantID = "7"
			frame.BrowserData, _ = json.Marshal(map[string]any{"type": "agent_node_recovery_required", "stream_id": "stream-1", "message_id": "message-1", "execution_generation": "logical-1", "sio_event": "chat_predict", "response_metadata": map[string]any{"node_recovery_required_v1": json.RawMessage(receipt)}})
			e := &recoveryProjectionExecutor{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{{err: pgx.ErrNoRows}, {values: []any{"claim-node-1"}}, {values: []any{int64(0), "", []byte{}, []byte{}, int64(0), int64(0), int64(0), int64(0)}}, {values: []any{int64(17), "claim-node-1", "RUNNING", executiondomain.AgentApplicationCapability, false, false}}, {values: []any{int64(41)}}}, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1"), pgconn.NewCommandTag("INSERT 0 1"), pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("UPDATE 1")}}, binding: sqlcgen.GetCurrentAgentTraceBindingRow{ClientStreamID: "stream-1", ClientMessageID: "message-1", ClientExecutionGeneration: "logical-1", SioEvent: "chat_predict"}}
			if fail {
				e.execErrors = []error{nil, nil, errors.New("suspension write failure")}
			}
			s := &recoveryProjectionStore{recoveryProjectionExecutor: e}
			r, err := newNodeEventsRepository(s)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := r.ProjectNodeEvent(t.Context(), frame)
			if fail {
				if err == nil || s.committed || outcome.Inserted {
					t.Fatal(outcome, err, s.committed)
				}
				return
			}
			if err != nil || !s.committed || !outcome.Inserted || outcome.CommittedSequence != 1 {
				t.Fatal(outcome, err, s.committed)
			}
			if len(e.execCalls) != 4 || !strings.Contains(e.execCalls[1].sql, "node_recovery_visits") || !strings.Contains(e.execCalls[2].sql, "desired_state='SUSPENDED'") {
				t.Fatal(e.execCalls)
			}
			for _, call := range e.execCalls {
				if strings.Contains(call.sql, "settlement_receipts") || strings.Contains(call.sql, "output_inbox") {
					t.Fatal("pause created terminal output")
				}
			}
		})
	}
}
