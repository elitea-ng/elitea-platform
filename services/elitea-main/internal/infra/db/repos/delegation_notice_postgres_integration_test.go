package repos

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
)

// FU-335: the delegation-refused notice is the one node event shown beside a
// FAILED terminal. Every case runs the real replay query on real PostgreSQL.

const (
	delegationNoticeEvent = `{"type":"agent_delegation_refused","content":null,"response_metadata":{"delegation_refused":{"code":"DELEGATION_GRANT_MISSING","node":"agent","toolkit":"batch"}}}`
	ordinaryFinalMessage  = `{"type":"full_message","content":"a partial answer","response_metadata":{}}`
)

// replayWithTerminal appends node events, then the execution's terminal
// projected as payloadType, and returns the replay seen before and after it.
func replayWithTerminal(t *testing.T, pool *pgxpool.Pool, suffix, payloadType string, events ...string) (before, after []string) {
	t.Helper()
	progress, terminal := preparePostgresNodeEventConcurrencyFixture(t, pool, suffix)
	nodes, err := NewNodeEventsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for index, data := range events {
		frame := progress
		frame.Sequence = uint64(index + 1)
		frame.EventID = fmt.Sprintf("%s:%d", frame.Fence.CommandID, frame.Sequence)
		frame.LogicalOutputID = outputapp.NodeEventLogicalOutputID(frame.Fence.ExecutionID, frame.Sequence)
		frame.EncodedEvent = []byte(fmt.Sprintf("delegation-notice-wire-%s-%d", suffix, frame.Sequence))
		frame.PayloadDigest = runtimedomain.SHA256(frame.EncodedEvent)
		frame.BrowserData = []byte(data)
		if _, err := nodes.ProjectNodeEvent(ctx, frame); err != nil {
			t.Fatalf("project node event %d: %v", frame.Sequence, err)
		}
	}
	replayTypes := func() []string {
		replay, err := NewReplayEventsRepository(pool)
		if err != nil {
			t.Fatal(err)
		}
		served, err := replay.Replay(ctx, terminal.ProjectionProjectID, terminal.Fence.ExecutionID, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		types := []string{}
		for _, event := range served {
			if event.Type != replayEventNodeEvent {
				types = append(types, event.Type)
				continue
			}
			var kind string
			for _, candidate := range []string{"agent_delegation_refused", "full_message"} {
				if strings.Contains(string(event.Data), `"type":"`+candidate+`"`) {
					kind = candidate
				}
			}
			types = append(types, kind)
		}
		return types
	}
	before = replayTypes()

	sequence := uint64(len(events) + 1)
	terminal.Sequence = sequence
	terminal.EventID = fmt.Sprintf("%s:%d", terminal.Fence.CommandID, sequence)
	terminal.Settlement.TerminalSequence = sequence
	terminal.Settlement.TerminalEventID = terminal.EventID
	terminal.EncodedSettlement, err = proto.MarshalOptions{Deterministic: true}.Marshal(&runtimev1.SettlementProposalV1{
		ProposalId:              terminal.Settlement.ProposalID,
		RequestedOutcome:        runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED,
		TerminalLogicalOutputId: terminal.Settlement.TerminalLogicalOutputID,
		TerminalEventId:         terminal.Settlement.TerminalEventID,
		TerminalSequence:        terminal.Settlement.TerminalSequence,
		TerminalPayloadDigest:   postgresDigestV1(terminal.Settlement.TerminalPayloadDigest),
		PrepareIdempotencyKey:   terminal.Settlement.IdempotencyKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	terminal.Settlement.ProposalDigest = runtimedomain.SHA256(terminal.EncodedSettlement)
	results, err := NewIndexIngestResultsRepository(pool, IndexIngestOutputPolicy{
		LimitsRevision: "index-limits-v1", ArtifactMediaType: "application/json", MaxArtifactBytes: 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newPostgresIndexOutputService(t, pool, results).IngestIndex(ctx, terminal); err != nil {
		t.Fatalf("project terminal: %v", err)
	}
	// The fixture's terminal is an index result; the replay barrier reads
	// only the projected terminal's payload type, which this case sets.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.output_inbox SET payload_type = $2
WHERE execution_id = $1 AND sequence = $3`, terminal.Fence.ExecutionID, payloadType, int64(sequence)); err != nil {
		t.Fatalf("set terminal payload type: %v", err)
	}
	return before, replayTypes()
}

func TestPostgresDelegationNoticeIsShownBesideAFailedTerminal(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	before, after := replayWithTerminal(t, pool, "notice-failed", "RUNTIME_FAILURE", delegationNoticeEvent)
	if len(before) != 0 {
		t.Fatalf("the notice was shown before its failed terminal: %v", before)
	}
	if len(after) < 1 || after[0] != "agent_delegation_refused" {
		t.Fatalf("the notice is not shown beside the failed terminal: %v", after)
	}
}

func TestPostgresAnOrdinaryFinalMessageStaysHiddenOnAFailedRun(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, after := replayWithTerminal(t, pool, "final-failed", "RUNTIME_FAILURE", ordinaryFinalMessage)
	if len(after) != 0 {
		t.Fatalf("a partial answer surfaced on a failed run: %v", after)
	}
}

func TestPostgresASucceededRunIsUnchangedAndHidesTheNotice(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, after := replayWithTerminal(t, pool, "final-succeeded", "AGENT_EXECUTION_RESULT", ordinaryFinalMessage, delegationNoticeEvent)
	if len(after) != 1 || after[0] != "full_message" {
		t.Fatalf("a succeeded run must show its final message and no notice: %v", after)
	}
}

// A reload reads the failed turn from its message group: the notice's node
// kind and bounded toolkit name are persisted there with the failure, and a
// name past the bound, or a failure that is not this refusal, persists none.
func TestPostgresAFailedTurnPersistsItsDelegationNoticeForAReload(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	const conversationID = "10000000-0000-4000-8000-000000000031"
	cases := []struct {
		name, toolkit, code string
		want                string
	}{
		{"at the bound", strings.Repeat("x", MaxDelegationNoticeToolkitChars), "AUTHORIZATION_FAILED", strings.Repeat("x", MaxDelegationNoticeToolkitChars)},
		{"past the bound", strings.Repeat("y", MaxDelegationNoticeToolkitChars+1), "AUTHORIZATION_FAILED", ""},
		{"another failure", "batch", "INTERNAL", ""},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			suffix := fmt.Sprintf("%012d", 335000+index)
			executionID := "execution-delegation-notice-" + suffix
			tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
				t.Fatal(err)
			}
			responseID := insertPostgresCurrentApplicationTurn(
				t, sqlcgen.New(tx), mustCurrentPGUUID(t, conversationID),
				"20000000-0000-4000-8000-"+suffix, "40000000-0000-4000-8000-"+suffix,
				"30000000-0000-4000-8000-"+suffix, "run the refused agent", executionID,
			)
			insertPostgresCurrentAgentCancelBinding(
				t, tx, responseID, "20000000-0000-4000-8000-"+suffix, executionID,
				"agent.execute.application.v1", "RUNNING", "RUNNING",
			)
			if _, err := tx.Exec(t.Context(), `
UPDATE elitea_runtime.agent_execution_jobs SET client_stream_id = $1 WHERE execution_id = $2`,
				conversationID, executionID); err != nil {
				t.Fatal(err)
			}
			notice := fmt.Sprintf(`{"type":"agent_delegation_refused","content":null,"response_metadata":{"delegation_refused":{"code":"DELEGATION_GRANT_MISSING","node":"agent","toolkit":%q}}}`, tc.toolkit)
			digest := runtimedomain.SHA256([]byte(notice))
			if _, err := tx.Exec(t.Context(), `
INSERT INTO elitea_runtime.execution_replay_events (
    event_id, execution_id, generation, projection_project_id, event_type, event_bytes, event_digest
) VALUES ($1, $2, 1, 1, 'execution.node_event', $3, $4)`,
				"command-"+suffix+":1", executionID, []byte(notice), digest[:]); err != nil {
				t.Fatal(err)
			}
			if err := persistCurrentAgentRuntimeTerminal(
				t.Context(), pgxExecutor{queryer: tx}, 1, executiondomain.AgentApplicationCapability,
				outputRecord{ExecutionID: executionID, Generation: 1}, tc.code, "The run was refused.",
			); err != nil {
				t.Fatal(err)
			}
			var node, toolkit, isError *string
			if err := tx.QueryRow(t.Context(), `
SELECT meta -> 'delegation_refused' ->> 'node', meta -> 'delegation_refused' ->> 'toolkit', meta ->> 'is_error'
FROM chat_message_group WHERE uuid = $1`, responseID).Scan(&node, &toolkit, &isError); err != nil {
				t.Fatal(err)
			}
			if isError == nil || *isError != "true" {
				t.Fatalf("the failure was not persisted: is_error=%v", isError)
			}
			if tc.want == "" {
				if node != nil || toolkit != nil {
					t.Fatalf("a notice was persisted: node=%v toolkit=%v", node, toolkit)
				}
				return
			}
			if node == nil || *node != "agent" || toolkit == nil || *toolkit != tc.want {
				t.Fatalf("notice not persisted: node=%v toolkit=%v", node, toolkit)
			}
		})
	}
}
