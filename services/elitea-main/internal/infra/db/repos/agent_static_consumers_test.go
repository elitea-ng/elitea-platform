package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Exercise the generated queries and their production adapter with the shared
// row fixture. PostgreSQL locking and durable replay need the integration suite.
type staticConsumerQueryer struct{ *scriptedExecutor }

func (q staticConsumerQueryer) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	return q.scriptedExecutor.QueryRow(ctx, query, args...)
}

func (q staticConsumerQueryer) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected static consumer Query")
}

func staticConsumerTurn(t *testing.T, tools bool) agentexecutionapp.CurrentContinueTurn {
	t.Helper()
	var metadata struct {
		Proof agentexecutionapp.CurrentPipelineStaticProof `json:"pipeline_static_v1"`
	}
	if err := json.Unmarshal([]byte(staticMessageMetadataFixture()), &metadata); err != nil {
		t.Fatal(err)
	}
	turn := agentexecutionapp.CurrentContinueTurn{
		ProjectID: 7, ActorUserID: 11, TargetParticipantID: 21,
		ConversationUUID:    "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		QuestionID:          "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
		ResponseMessageID:   "30e0913e-10d4-43db-b8d0-c7b79480935a",
		ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
		ThreadID:            "thread-current-1", Kind: agentexecutionapp.CurrentRegenerationApplication,
		ApplicationID: 31, ApplicationVersionID: 41, ContinuationKind: agentexecutionapp.CurrentContinuationStatic,
	}
	if tools {
		turn.PipelineStaticTools = &agentexecutionapp.CurrentPipelineStaticTools{
			RootKind:      agentexecutionapp.CurrentStaticInventoryAgentRoot,
			ApplicationID: 31, ApplicationVersionID: 41, InputDigest: bytes.Repeat([]byte{7}, 32),
			Inventory: agentexecutionapp.CurrentStaticToolInventory{Revision: 1, Pauses: []agentexecutionapp.CurrentStaticToolPause{{
				ToolCallID: "original-call", ChildThreadID: "original-child", OriginalBatchEventID: "original-batch",
				OriginalOrdinal: 16, Proof: metadata.Proof,
			}}},
			Selected: []agentexecutionapp.CurrentStaticLeafDecision{{PauseID: metadata.Proof.PauseID,
				ToolCallID: "original-call", ChildThreadID: "original-child", Action: "continue", Value: "continue original"}},
		}
	} else {
		turn.PipelineStaticPause = &agentexecutionapp.CurrentPipelineStaticPause{
			Proof: metadata.Proof, ApplicationID: 31, ApplicationVersionID: 41, InputDigest: bytes.Repeat([]byte{8}, 32),
		}
	}
	if err := turn.Validate(); err != nil {
		t.Fatal(err)
	}
	return turn
}

func TestStaticContinuationConsumerBindsOriginalInputAndResponse(t *testing.T) {
	for _, tools := range []bool{false, true} {
		name := "root"
		if tools {
			name = "tools"
		}
		t.Run(name, func(t *testing.T) {
			turn := staticConsumerTurn(t, tools)
			response, _ := currentPGUUID(turn.ResponseMessageID)
			fixture := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{int32(17), response}}}}
			queries := sqlcgen.New(staticConsumerQueryer{fixture})
			var err error
			if tools {
				err = resumeCurrentAgentStaticTools(t.Context(), queries, "next-execution", turn)
			} else {
				err = resumeCurrentAgentStatic(t.Context(), queries, "next-execution", turn)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(fixture.rowCalls) != 1 || len(fixture.execCalls) != 0 {
				t.Fatal("unexpected mutation outside guarded resume")
			}
			args := fixture.rowCalls[0].args
			conversation, _ := currentPGUUID(turn.ConversationUUID)
			question, _ := currentPGUUID(turn.QuestionID)
			want := []any{int32(21), int64(11), int32(41), int32(31), int32(7), turn.ExecutionGeneration}
			for i, expected := range want {
				if args[i] != expected {
					t.Fatalf("identity argument %d = %v", i, args[i])
				}
			}
			if args[7] != conversation || args[8] != response || args[9] != question {
				t.Fatal("original response identity changed")
			}
			var proof []byte
			var digest []byte
			if tools {
				proof, _ = json.Marshal(turn.PipelineStaticTools.Inventory)
				digest = turn.PipelineStaticTools.InputDigest
				var ids []string
				if json.Unmarshal(args[11].([]byte), &ids) != nil || len(ids) != 1 || ids[0] != turn.PipelineStaticTools.Selected[0].PauseID {
					t.Fatal("selected original pause changed")
				}
			} else {
				proof, _ = json.Marshal(turn.PipelineStaticPause.Proof)
				digest = turn.PipelineStaticPause.InputDigest
			}
			if !bytes.Equal(args[6].([]byte), digest) || !bytes.Equal(args[10].([]byte), proof) || args[len(args)-2] != turn.ThreadID || args[len(args)-1] != "next-execution" {
				t.Fatal("original proof, digest, thread or next execution changed")
			}
		})
	}
}

func TestStaticContinuationConsumerRefusesMismatchBeforeWriting(t *testing.T) {
	for _, tools := range []bool{false, true} {
		name := "root"
		if tools {
			name = "tools"
		}
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name   string
				change func(*agentexecutionapp.CurrentContinueTurn)
			}{
				{"other application", func(turn *agentexecutionapp.CurrentContinueTurn) { turn.ApplicationID++ }},
				{"unrepresentable project", func(turn *agentexecutionapp.CurrentContinueTurn) { turn.ProjectID = math.MaxInt32 + 1 }},
				{"mixed HITL", func(turn *agentexecutionapp.CurrentContinueTurn) {
					turn.HITLDecisions = []byte(`[{"interrupt_id":"other","action":"approve"}]`)
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					turn := staticConsumerTurn(t, tools)
					tc.change(&turn)
					fixture := &scriptedExecutor{}
					queries := sqlcgen.New(staticConsumerQueryer{fixture})
					var err error
					if tools {
						err = resumeCurrentAgentStaticTools(t.Context(), queries, "next-execution", turn)
					} else {
						err = resumeCurrentAgentStatic(t.Context(), queries, "next-execution", turn)
					}
					if !errors.Is(err, executionapp.ErrInvalidAdmission) || len(fixture.rowCalls) != 0 || len(fixture.execCalls) != 0 {
						t.Fatal("invalid continuation reached the database", err)
					}
				})
			}
			for _, tc := range []struct {
				name     string
				row      scriptedRow
				expected error
			}{
				{"already consumed", scriptedRow{err: pgx.ErrNoRows}, agentexecutionapp.ErrUnsupportedCurrentAgentStart},
				{"wrong response", scriptedRow{values: []any{int32(17), pgtype.UUID{}}}, executionapp.ErrInvalidAdmission},
			} {
				t.Run(tc.name, func(t *testing.T) {
					turn := staticConsumerTurn(t, tools)
					fixture := &scriptedExecutor{rowResults: []scriptedRow{tc.row}}
					queries := sqlcgen.New(staticConsumerQueryer{fixture})
					var err error
					if tools {
						err = resumeCurrentAgentStaticTools(t.Context(), queries, "next-execution", turn)
					} else {
						err = resumeCurrentAgentStatic(t.Context(), queries, "next-execution", turn)
					}
					if !errors.Is(err, tc.expected) || len(fixture.rowCalls) != 1 || len(fixture.execCalls) != 0 {
						t.Fatal("resume refusal lost", err)
					}
				})
			}
		})
	}
}

func TestStaticFullMessagePersistenceAndOrdinaryCompletionClearing(t *testing.T) {
	for _, tc := range []struct {
		name, metadata string
		static         bool
	}{
		{"paused", staticMessageMetadataFixture(), true},
		{"ordinary", `{"thread_id":"thread-current-1","invoked_skills":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message, err := decodeCurrentAgentFullMessage([]byte(`"result"`), []byte(`[]`), []byte(tc.metadata))
			if err != nil {
				t.Fatal(err)
			}
			writer := &currentAgentTerminalWriterStub{existingSkills: `[]`}
			if err := persistCurrentAgentTerminal(t.Context(), writer, outputapp.ExpectedAgentExecution{}, currentAgentTerminal{FullMessage: &message}); err != nil {
				t.Fatal(err)
			}
			if tc.static {
				if !bytes.Equal(writer.full.PipelineStaticProof, message.PipelineStaticProof) {
					t.Fatal("validated proof lost")
				}
			} else if string(writer.full.PipelineStaticProof) != "null" {
				t.Fatal("ordinary completion retained stale proof")
			}
			if string(writer.full.PipelineStaticTools) != "null" {
				t.Fatal("ordinary/root terminal retained tool inventory")
			}
		})
	}
}

func TestStaticFullMessageRejectsOtherPauseAuthority(t *testing.T) {
	for _, field := range []string{`"hitl_interrupt":{"interrupt_id":"dynamic"}`, `"hitl_interrupts":[{}]`, `"authorization_requests":[{}]`, `"pipeline_static_tools_v1":{}`, `"hitl_interrupt":[]`, `"authorization_requests":{}`} {
		t.Run(field, func(t *testing.T) {
			metadata := strings.TrimSuffix(staticMessageMetadataFixture(), "}") + "," + field + "}"
			if _, err := decodeCurrentAgentFullMessage([]byte(`"paused"`), []byte(`[]`), []byte(metadata)); !errors.Is(err, outputapp.ErrAgentExecutionResultMismatch) {
				t.Fatal("mixed pause metadata accepted", err)
			}
		})
	}
}

func TestStaticFullMessageAcceptsNeutralOtherPauseFields(t *testing.T) {
	for _, field := range []string{`"hitl_interrupt":null`, `"hitl_interrupt":{}`, `"hitl_interrupts":null`, `"hitl_interrupts":[]`, `"authorization_requests":null`, `"authorization_requests":[]`} {
		t.Run(field, func(t *testing.T) {
			metadata := strings.TrimSuffix(staticMessageMetadataFixture(), "}") + "," + field + "}"
			if _, err := decodeCurrentAgentFullMessage([]byte(`"paused"`), []byte(`[]`), []byte(metadata)); err != nil {
				t.Fatal("neutral pause metadata rejected", err)
			}
		})
	}
}
