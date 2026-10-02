package repos

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type currentAgentTerminalWriterStub struct {
	scriptedExecutor
	mixed          sqlcgen.FinalizeCurrentAgentMixedPauseParams
	existingSkills string
	hitl           sqlcgen.FinalizeCurrentAgentHITLPauseParams
	full           sqlcgen.FinalizeCurrentAgentFullMessageParams
	attachments    []sqlcgen.UpdateCurrentAgentAttachmentContentParams
	attachmentRows int64
	attachmentErr  error
	deletedText    sqlcgen.DeleteCurrentAgentProvisionalTextParams
}

func (s *currentAgentTerminalWriterStub) LockCurrentAgentResponseForTerminal(
	context.Context,
	sqlcgen.LockCurrentAgentResponseForTerminalParams,
) (int32, error) {
	return 17, nil
}

func (s *currentAgentTerminalWriterStub) InsertCurrentAgentTextItem(context.Context, int64) (int32, error) {
	return 23, nil
}

func (s *currentAgentTerminalWriterStub) InsertCurrentAgentTextContent(
	context.Context,
	sqlcgen.InsertCurrentAgentTextContentParams,
) error {
	return nil
}

func (s *currentAgentTerminalWriterStub) DeleteCurrentAgentProvisionalText(
	_ context.Context,
	arg sqlcgen.DeleteCurrentAgentProvisionalTextParams,
) error {
	s.deletedText = arg
	return nil
}

func (s *currentAgentTerminalWriterStub) UpdateCurrentAgentAttachmentContent(
	_ context.Context,
	arg sqlcgen.UpdateCurrentAgentAttachmentContentParams,
) (int64, error) {
	s.attachments = append(s.attachments, arg)
	if s.attachmentErr != nil {
		return 0, s.attachmentErr
	}
	return s.attachmentRows, nil
}

func (s *currentAgentTerminalWriterStub) FinalizeCurrentAgentFullMessage(
	_ context.Context,
	arg sqlcgen.FinalizeCurrentAgentFullMessageParams,
) (int64, error) {
	s.full = arg
	return 1, nil
}

func (s *currentAgentTerminalWriterStub) FinalizeCurrentAgentHITLPause(
	_ context.Context,
	arg sqlcgen.FinalizeCurrentAgentHITLPauseParams,
) (int64, error) {
	s.hitl = arg
	return 1, nil
}

func (s *currentAgentTerminalWriterStub) FinalizeCurrentAgentAuthorizationPause(
	context.Context,
	sqlcgen.FinalizeCurrentAgentAuthorizationPauseParams,
) (int64, error) {
	return 1, nil
}

func (s *currentAgentTerminalWriterStub) GetCurrentAgentInvokedSkills(context.Context, int64) (string, error) {
	return s.existingSkills, nil
}

func TestPersistCurrentAgentTerminalPreservesSkillsAcrossPauseReloadAndContinuation(t *testing.T) {
	existing := `[{"skill_id":1,"name":"Existing","icon_meta":null}]`
	pauseWriter := &currentAgentTerminalWriterStub{existingSkills: existing}
	err := persistCurrentAgentTerminal(t.Context(), pauseWriter, outputapp.ExpectedAgentExecution{}, currentAgentTerminal{
		HITLPause: &currentAgentHITLPause{
			ThreadID:      "thread-1",
			Interrupt:     json.RawMessage(`{"interrupt_id":"interrupt-1"}`),
			Interrupts:    json.RawMessage(`[{"interrupt_id":"interrupt-1"}]`),
			InvokedSkills: json.RawMessage(`[{"skill_id":2,"name":"Loaded","icon_meta":{"name":"book"}}]`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPaused := `[{"skill_id":2,"name":"Loaded","icon_meta":{"name":"book"}},{"skill_id":1,"name":"Existing","icon_meta":null}]`
	if string(pauseWriter.hitl.InvokedSkills) != wantPaused {
		t.Fatalf("paused invoked skills = %s", pauseWriter.hitl.InvokedSkills)
	}

	continuationWriter := &currentAgentTerminalWriterStub{existingSkills: string(pauseWriter.hitl.InvokedSkills)}
	err = persistCurrentAgentTerminal(t.Context(), continuationWriter, outputapp.ExpectedAgentExecution{}, currentAgentTerminal{
		FullMessage: &currentAgentFullMessage{
			Content:       "done",
			ThreadID:      "thread-1",
			References:    json.RawMessage(`[]`),
			InvokedSkills: json.RawMessage(`[{"skill_id":3,"name":"LOADED","icon_meta":{"name":"updated"}},{"skill_id":4,"name":"Final","icon_meta":null}]`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantFinal := `[{"skill_id":3,"name":"LOADED","icon_meta":{"name":"updated"}},{"skill_id":4,"name":"Final","icon_meta":null},{"skill_id":1,"name":"Existing","icon_meta":null}]`
	if string(continuationWriter.full.InvokedSkills) != wantFinal {
		t.Fatalf("continued invoked skills = %s", continuationWriter.full.InvokedSkills)
	}
}

func TestPersistCurrentAgentTerminalReplacesOnlyItsProvisionalText(t *testing.T) {
	writer := &currentAgentTerminalWriterStub{existingSkills: `[]`}
	expected := outputapp.ExpectedAgentExecution{
		ExecutionID: "execution-9",
		Generation:  3,
	}
	err := persistCurrentAgentTerminal(t.Context(), writer, expected, currentAgentTerminal{
		FullMessage: &currentAgentFullMessage{
			Content:       "complete answer",
			ThreadID:      "thread-1",
			References:    json.RawMessage(`[]`),
			InvokedSkills: json.RawMessage(`[]`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if writer.deletedText.MessageGroupID != 17 ||
		writer.deletedText.ExecutionID != expected.ExecutionID ||
		writer.deletedText.Generation != int64(expected.Generation) {
		t.Fatalf("deleted provisional text binding = %+v", writer.deletedText)
	}
}

func TestDecodeCurrentAgentHITLPauseAcceptsOneSequentialNestedInterrupt(t *testing.T) {
	interrupt := map[string]any{
		"type":                 "hitl",
		"interrupt_id":         "interrupt-nested-1",
		"available_actions":    []any{"approve", "reject", "block_with_comment"},
		"parent_agent_call_id": "call-pipeline-1",
		"parent_agent_path": []any{
			map[string]any{"name": "artifact_test", "call_id": "call-pipeline-1"},
		},
	}
	pause := decodeAgentHITLPauseForTest(t, interrupt)
	if pause.ThreadID != "thread-parent-1" {
		t.Fatalf("thread ID = %q", pause.ThreadID)
	}
	var persisted map[string]any
	if err := json.Unmarshal(pause.Interrupt, &persisted); err != nil ||
		persisted["parent_agent_call_id"] != "call-pipeline-1" {
		t.Fatalf("persisted interrupt = %#v, error = %v", persisted, err)
	}
}

func TestDecodeCurrentAgentHITLPauseAcceptsBoundedParallelInProcessInterrupts(t *testing.T) {
	interrupts := []map[string]any{
		{
			"type": "hitl", "interrupt_id": "interrupt-parallel-1",
			"available_actions":    []any{"approve", "reject"},
			"parent_agent_call_id": "call-orchestrator-1",
			"parent_agent_path":    []any{map[string]any{"name": "orchestrator", "call_id": "call-orchestrator-1"}},
		},
		{
			"type": "hitl", "interrupt_id": "interrupt-parallel-2",
			"available_actions":    []any{"approve", "block_with_comment"},
			"parent_agent_call_id": "call-orchestrator-2",
			"parent_agent_path":    []any{map[string]any{"name": "orchestrator", "call_id": "call-orchestrator-2"}},
		},
	}
	pause, err := decodeCurrentAgentHITLPause(
		json.RawMessage(`"Approve both?"`),
		hitlMetadataForInterruptsForTest(t, interrupts),
	)
	if err != nil {
		t.Fatal(err)
	}
	var persisted []map[string]any
	if err := json.Unmarshal(pause.Interrupts, &persisted); err != nil || len(persisted) != 2 ||
		persisted[0]["interrupt_id"] != "interrupt-parallel-1" ||
		persisted[1]["interrupt_id"] != "interrupt-parallel-2" {
		t.Fatalf("persisted interrupts = %#v, error = %v", persisted, err)
	}
}

func TestDecodeCurrentAgentHITLPauseRejectsParallelChildRouting(t *testing.T) {
	for _, routed := range []map[string]any{
		{"child_thread_id": "child-thread-1"},
		{"via_call_id": "call-1"},
		{"_via_call_id": "call-1"},
	} {
		interrupt := map[string]any{
			"interrupt_id":      "interrupt-child-1",
			"available_actions": []any{"approve"},
		}
		for key, value := range routed {
			interrupt[key] = value
		}
		_, err := decodeCurrentAgentHITLPause(
			json.RawMessage(`"Approve?"`),
			hitlMetadataForTest(t, interrupt),
		)
		if !errors.Is(err, outputapp.ErrAgentExecutionResultMismatch) {
			t.Fatalf("routed interrupt %#v error = %v", routed, err)
		}
	}
}

func TestDecodeCurrentAgentHITLPauseRejectsUnboundNestedPath(t *testing.T) {
	interrupt := map[string]any{
		"interrupt_id":         "interrupt-nested-1",
		"available_actions":    []any{"approve"},
		"parent_agent_call_id": "call-pipeline-1",
		"parent_agent_path": []any{
			map[string]any{"name": "artifact_test", "call_id": "different-call"},
		},
	}
	_, err := decodeCurrentAgentHITLPause(
		json.RawMessage(`"Approve?"`),
		hitlMetadataForTest(t, interrupt),
	)
	if !errors.Is(err, outputapp.ErrAgentExecutionResultMismatch) {
		t.Fatalf("unbound path error = %v", err)
	}
}

func TestDecodeCurrentAgentHITLPauseRejectsIncompleteNestedIdentity(t *testing.T) {
	for _, interrupt := range []map[string]any{
		{
			"interrupt_id":         "interrupt-call-only",
			"available_actions":    []any{"approve"},
			"parent_agent_call_id": "call-pipeline-1",
		},
		{
			"interrupt_id":      "interrupt-path-only",
			"available_actions": []any{"approve"},
			"parent_agent_path": []any{
				map[string]any{"name": "artifact_test", "call_id": "call-pipeline-1"},
			},
		},
	} {
		_, err := decodeCurrentAgentHITLPause(
			json.RawMessage(`"Approve?"`),
			hitlMetadataForTest(t, interrupt),
		)
		if !errors.Is(err, outputapp.ErrAgentExecutionResultMismatch) {
			t.Fatalf("incomplete nested identity %#v error = %v", interrupt, err)
		}
	}
}

func TestDecodeCurrentAgentAuthorizationPausePreservesExactInvocationHierarchy(t *testing.T) {
	requests := []map[string]any{
		{
			"interrupt_id": "mcp_auth_sharepoint_1",
			"tool_run_id":  "legacy-tool-run-sharepoint-1",
			"tool_call_id": "call-sharepoint-search-1",
			"server_url":   "https://sharepoint.example.test",
			"tool_name":    "list_sites",
			"toolkit_name": "SharePoint",
			"toolkit_type": "sharepoint",
			"metadata": map[string]any{
				"parent_agent_name":    "researcher",
				"parent_agent_call_id": "agent-call-1",
				"parent_agent_path": []any{
					map[string]any{"name": "researcher", "call_id": "agent-call-1"},
				},
			},
		},
	}
	metadata, err := json.Marshal(map[string]any{
		"thread_id":              "thread-parent-1",
		"interrupt_id":           "mcp_auth_sharepoint_1",
		"tool_run_id":            "legacy-tool-run-sharepoint-1",
		"authorization_requests": requests,
		"invoked_skills": []map[string]any{{
			"skill_id": 41, "name": "SharePoint operations", "icon_meta": nil,
			"instructions": "must not persist",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pause, err := decodeCurrentAgentAuthorizationPause(
		json.RawMessage(`"SharePoint authorization is required."`),
		metadata,
	)
	if err != nil || pause.ThreadID != "thread-parent-1" {
		t.Fatalf("pause=%+v error=%v", pause, err)
	}
	var persisted []map[string]any
	if err := json.Unmarshal(pause.Requests, &persisted); err != nil || len(persisted) != 1 ||
		persisted[0]["interrupt_id"] != "mcp_auth_sharepoint_1" ||
		persisted[0]["tool_call_id"] != "call-sharepoint-search-1" {
		t.Fatalf("persisted=%#v error=%v", persisted, err)
	}
	if string(pause.InvokedSkills) != `[{"skill_id":41,"name":"SharePoint operations","icon_meta":null}]` {
		t.Fatalf("compact invoked skills = %s", pause.InvokedSkills)
	}
}

func TestDecodeCurrentAgentAuthorizationPauseRejectsAmbiguousIdentity(t *testing.T) {
	for name, test := range map[string]struct {
		requests   []map[string]any
		terminalID string
	}{
		"duplicate": {
			requests: []map[string]any{
				{"tool_run_id": "tool-run-1", "server_url": "https://one.example.test"},
				{"tool_run_id": "tool-run-1", "server_url": "https://two.example.test"},
			},
			terminalID: "tool-run-1",
		},
		"terminal mismatch": {
			requests: []map[string]any{
				{"tool_run_id": "tool-run-1", "server_url": "https://one.example.test"},
			},
			terminalID: "tool-run-other",
		},
	} {
		t.Run(name, func(t *testing.T) {
			metadata, err := json.Marshal(map[string]any{
				"thread_id":              "thread-parent-1",
				"tool_run_id":            test.terminalID,
				"authorization_requests": test.requests,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = decodeCurrentAgentAuthorizationPause(
				json.RawMessage(`"Authorization required."`),
				metadata,
			)
			if !errors.Is(err, outputapp.ErrAgentExecutionResultMismatch) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func decodeAgentHITLPauseForTest(
	t *testing.T,
	interrupt map[string]any,
) currentAgentHITLPause {
	t.Helper()
	pause, err := decodeCurrentAgentHITLPause(
		json.RawMessage(`"Approve?"`),
		hitlMetadataForTest(t, interrupt),
	)
	if err != nil {
		t.Fatal(err)
	}
	return pause
}

func hitlMetadataForTest(t *testing.T, interrupt map[string]any) json.RawMessage {
	t.Helper()
	return hitlMetadataForInterruptsForTest(t, []map[string]any{interrupt})
}

func hitlMetadataForInterruptsForTest(t *testing.T, interrupts []map[string]any) json.RawMessage {
	t.Helper()
	if len(interrupts) == 0 {
		t.Fatal("interrupts are required")
	}
	encoded, err := json.Marshal(map[string]any{
		"thread_id":       "thread-parent-1",
		"hitl_interrupt":  interrupts[0],
		"hitl_interrupts": interrupts,
		"invoked_skills": []map[string]any{{
			"skill_id": 31, "name": "Safe changes", "icon_meta": map[string]any{"name": "shield"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// #607: the terminal transaction writes back the text the worker extracted, on
// the response group it just locked, alongside the assistant's own text.
func TestPersistCurrentAgentTerminalWritesBackAttachmentContentOnEveryTerminalState(t *testing.T) {
	content := json.RawMessage(
		`[{"type":"text","text":"Bucket: chat-attachments","elitea_attachment":{"item_id":"50000000-0000-4000-8000-000000000001"}},` +
			`{"type":"text","text":"EXTRACTED TEXT"}]`,
	)
	contents := []outputapp.AgentExecutionAttachmentContent{
		{ItemID: "50000000-0000-4000-8000-000000000001", Content: content},
	}
	// A completed answer and a HITL pause both get the write-back: a paused
	// turn has ALREADY read its attachments, and it is the turn most likely to
	// be continued with a follow-up question about the file.
	for name, terminal := range map[string]currentAgentTerminal{
		"completed": {
			AttachmentContents: contents,
			FullMessage: &currentAgentFullMessage{
				Content: "done", ThreadID: "thread-1",
				References: json.RawMessage(`[]`), InvokedSkills: json.RawMessage(`[]`),
			},
		},
		"paused on HITL": {
			AttachmentContents: contents,
			HITLPause: &currentAgentHITLPause{
				ThreadID:      "thread-1",
				Interrupt:     json.RawMessage(`{"interrupt_id":"interrupt-1"}`),
				Interrupts:    json.RawMessage(`[{"interrupt_id":"interrupt-1"}]`),
				InvokedSkills: json.RawMessage(`[]`),
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			writer := &currentAgentTerminalWriterStub{existingSkills: `[]`, attachmentRows: 1}
			if err := persistCurrentAgentTerminal(
				t.Context(), writer, outputapp.ExpectedAgentExecution{}, terminal,
			); err != nil {
				t.Fatal(err)
			}
			if len(writer.attachments) != 1 {
				t.Fatalf("attachment writes = %+v", writer.attachments)
			}
			written := writer.attachments[0]
			// The response group id the lock returned, so the SQL scope has
			// the turn to check the item against.
			if written.ResponseMessageGroupID != 17 {
				t.Fatalf("response message group = %d", written.ResponseMessageGroupID)
			}
			if written.ItemID != mustCurrentPGUUID(t, "50000000-0000-4000-8000-000000000001") {
				t.Fatalf("item id = %+v", written.ItemID)
			}
			// Byte-for-byte: the `elitea_attachment` marker in the header
			// chunk is what stops a later turn re-reading the file.
			if string(written.Content) != string(content) {
				t.Fatalf("content = %s", written.Content)
			}
		})
	}
}

func TestPersistCurrentAgentTerminalWithoutAttachmentContentWritesNothingBack(t *testing.T) {
	writer := &currentAgentTerminalWriterStub{existingSkills: `[]`, attachmentRows: 1}
	if err := persistCurrentAgentTerminal(
		t.Context(), writer, outputapp.ExpectedAgentExecution{}, currentAgentTerminal{
			FullMessage: &currentAgentFullMessage{
				Content: "done", ThreadID: "thread-1",
				References: json.RawMessage(`[]`), InvokedSkills: json.RawMessage(`[]`),
			},
		},
	); err != nil {
		t.Fatal(err)
	}
	// An ordinary turn carries no write-back at all; it must not cost a
	// statement inside the terminal transaction.
	if len(writer.attachments) != 0 {
		t.Fatalf("attachment writes = %+v", writer.attachments)
	}
	if writer.full.ThreadID != "thread-1" {
		t.Fatalf("the answer was not finalized: %+v", writer.full)
	}
}

// A refused write-back must not cost the user the answer. 0 rows is the SQL
// scope declining an item that is not on this turn's question group -- the
// protection working, after it has already worked.
func TestPersistCurrentAgentTerminalTreatsARefusedAttachmentWriteAsANoOp(t *testing.T) {
	writer := &currentAgentTerminalWriterStub{existingSkills: `[]`, attachmentRows: 0}
	err := persistCurrentAgentTerminal(
		t.Context(), writer, outputapp.ExpectedAgentExecution{}, currentAgentTerminal{
			AttachmentContents: []outputapp.AgentExecutionAttachmentContent{{
				ItemID:  "50000000-0000-4000-8000-000000000001",
				Content: json.RawMessage(`[{"type":"text","text":"A"}]`),
			}},
			FullMessage: &currentAgentFullMessage{
				Content: "done", ThreadID: "thread-1",
				References: json.RawMessage(`[]`), InvokedSkills: json.RawMessage(`[]`),
			},
		},
	)
	if err != nil {
		t.Fatalf("a refused write-back failed the terminal projection: %v", err)
	}
	if len(writer.attachments) != 1 || writer.full.ThreadID != "thread-1" {
		t.Fatalf("writes=%+v full=%+v", writer.attachments, writer.full)
	}
}

// A DATABASE error is different: the transaction cannot proceed, so it must not
// be swallowed into a half-written terminal.
func TestPersistCurrentAgentTerminalFailsOnAnAttachmentWriteDatabaseError(t *testing.T) {
	writer := &currentAgentTerminalWriterStub{
		existingSkills: `[]`, attachmentErr: errors.New("connection reset"),
	}
	err := persistCurrentAgentTerminal(
		t.Context(), writer, outputapp.ExpectedAgentExecution{}, currentAgentTerminal{
			AttachmentContents: []outputapp.AgentExecutionAttachmentContent{{
				ItemID:  "50000000-0000-4000-8000-000000000001",
				Content: json.RawMessage(`[{"type":"text","text":"A"}]`),
			}},
			FullMessage: &currentAgentFullMessage{
				Content: "done", ThreadID: "thread-1",
				References: json.RawMessage(`[]`), InvokedSkills: json.RawMessage(`[]`),
			},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("error = %v", err)
	}
	if writer.full.ThreadID != "" {
		t.Fatalf("the terminal was finalized after a failed write-back: %+v", writer.full)
	}
}

type terminalNodeEventStub struct {
	scriptedExecutor
	row sqlcgen.GetAgentExecutionTerminalNodeEventRow
}

func (s *terminalNodeEventStub) GetAgentExecutionTerminalNodeEvent(
	context.Context,
	sqlcgen.GetAgentExecutionTerminalNodeEventParams,
) (sqlcgen.GetAgentExecutionTerminalNodeEventRow, error) {
	return s.row, nil
}

// #607: the write-back list is the ONE part of the terminal that comes from the
// frame rather than from the durable node event, so this pins the wiring that
// carries it. The node event deliberately cannot carry it -- it is the
// browser-facing `full_message`, and riding it would ship every attached file's
// full text to the UI on every turn -- so if this assignment is dropped, every
// other test in the chain still passes and nothing is ever written.
func TestLoadCurrentAgentTerminalCarriesTheFramesAttachmentContents(t *testing.T) {
	event := []byte(`{"type":"full_message","stream_id":"stream-1","message_id":"message-1",` +
		`"content":"done","references":[],"sio_event":"sio-1","execution_generation":"generation-1",` +
		`"response_metadata":{"thread_id":"thread-1"}}`)
	digest := runtimedomain.SHA256(event)
	cursor := int64(7)
	contents := []outputapp.AgentExecutionAttachmentContent{{
		ItemID:  "50000000-0000-4000-8000-000000000001",
		Content: json.RawMessage(`[{"type":"text","text":"A"},{"type":"text","text":"EXTRACTED TEXT"}]`),
	}}
	projection := outputapp.AgentExecutionProjection{
		Expected: outputapp.ExpectedAgentExecution{
			ClientStreamID: "stream-1", ClientMessageID: "message-1",
			SIOEvent: "sio-1", ClientExecutionGeneration: "generation-1",
		},
		Frame: outputapp.AgentExecutionFrame{
			Sequence: 5,
			Fence:    runtimedomain.Fence{ExecutionID: "execution-1", Generation: 1},
			Result: outputapp.AgentExecutionResult{
				TerminalState: outputapp.AgentExecutionTerminalCompleted,
				ResultArtifact: outputapp.AgentExecutionArtifactReference{
					ArtifactID:       "node-event:execution-1:full-message",
					ImmutableVersion: "sha256:" + hex.EncodeToString(digest[:]),
					ByteLength:       uint64(len(event)),
					Digest:           digest,
				},
				AttachmentContents: contents,
			},
		},
	}
	stub := &terminalNodeEventStub{row: sqlcgen.GetAgentExecutionTerminalNodeEventRow{
		LastNodeCursor:      &cursor,
		LastNodeSequence:    4,
		LastNodeEventBytes:  event,
		LastNodeEventDigest: digest[:],
	}}

	terminal, err := loadCurrentAgentTerminal(t.Context(), stub, 1, projection)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.FullMessage == nil || terminal.FullMessage.Content != "done" {
		t.Fatalf("terminal=%+v", terminal)
	}
	if len(terminal.AttachmentContents) != 1 ||
		terminal.AttachmentContents[0].ItemID != contents[0].ItemID ||
		string(terminal.AttachmentContents[0].Content) != string(contents[0].Content) {
		t.Fatalf("attachment contents=%+v", terminal.AttachmentContents)
	}
}

func TestPipelineTerminalReplacesProvisionalHistoryOnlyForPipelineSnapshots(t *testing.T) {
	for _, kind := range []string{"pipeline", "openai", ""} {
		t.Run(kind, func(t *testing.T) {
			metadata, err := json.Marshal(map[string]any{
				"thread_id":           "thread-1",
				"application_details": map[string]any{"version_details": map[string]any{"agent_type": kind}},
			})
			if err != nil {
				t.Fatal(err)
			}
			message, err := decodeCurrentAgentFullMessage(json.RawMessage(`"final answer"`), nil, metadata)
			if err != nil {
				t.Fatal(err)
			}
			writer := &currentAgentTerminalWriterStub{existingSkills: `[]`}
			err = persistCurrentAgentTerminal(t.Context(), writer, outputapp.ExpectedAgentExecution{ExecutionID: "resume", Generation: 1}, currentAgentTerminal{FullMessage: &message})
			if err != nil {
				t.Fatal(err)
			}
			if writer.deletedText.ReplacePipelineProvisional != (kind == "pipeline") {
				t.Fatalf("cleanup scope for %q: %+v", kind, writer.deletedText)
			}
		})
	}
}

func (s *currentAgentTerminalWriterStub) FinalizeCurrentAgentMixedPause(_ context.Context, arg sqlcgen.FinalizeCurrentAgentMixedPauseParams) (int64, error) {
	s.mixed = arg
	return 1, nil
}

func TestLoadAndPersistCurrentAgentMixedPausePreservesBothGuards(t *testing.T) {
	event := []byte(`{"type":"agent_hitl_interrupt","stream_id":"stream-1","message_id":"message-1","content":"Approval required","sio_event":"sio-1","execution_generation":"generation-1","response_metadata":{"thread_id":"thread-1","interrupt_id":"auth-1","hitl_interrupt":{"type":"hitl","interrupt_id":"sensitive-1","available_actions":["approve","reject"]},"hitl_interrupts":[{"type":"hitl","interrupt_id":"sensitive-1","available_actions":["approve","reject"]}],"authorization_requests":[{"interrupt_id":"auth-1","server_url":"https://example.test/mcp"}]}}`)
	digest := runtimedomain.SHA256(event)
	cursor := int64(7)
	contents := []outputapp.AgentExecutionAttachmentContent{{
		ItemID:  "50000000-0000-4000-8000-000000000001",
		Content: json.RawMessage(`[{"type":"text","text":"A"},{"type":"text","text":"EXTRACTED TEXT"}]`),
	}}
	projection := outputapp.AgentExecutionProjection{
		Expected: outputapp.ExpectedAgentExecution{
			ClientStreamID: "stream-1", ClientMessageID: "message-1",
			SIOEvent: "sio-1", ClientExecutionGeneration: "generation-1",
		},
		Frame: outputapp.AgentExecutionFrame{
			Sequence: 5,
			Fence:    runtimedomain.Fence{ExecutionID: "execution-1", Generation: 1},
			Result: outputapp.AgentExecutionResult{
				TerminalState: outputapp.AgentExecutionTerminalPausedHITL,
				ResultArtifact: outputapp.AgentExecutionArtifactReference{
					ArtifactID:       "node-event:execution-1:hitl-interrupt",
					ImmutableVersion: "sha256:" + hex.EncodeToString(digest[:]),
					ByteLength:       uint64(len(event)),
					Digest:           digest,
				},
				AttachmentContents: contents,
			},
		},
	}
	stub := &terminalNodeEventStub{row: sqlcgen.GetAgentExecutionTerminalNodeEventRow{
		LastNodeCursor:      &cursor,
		LastNodeSequence:    4,
		LastNodeEventBytes:  event,
		LastNodeEventDigest: digest[:],
	}}

	terminal, err := loadCurrentAgentTerminal(t.Context(), stub, 1, projection)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.HITLPause == nil || terminal.AuthorizationPause == nil {
		t.Fatal("mixed pause lost a guard set")
	}
	writer := &currentAgentTerminalWriterStub{existingSkills: `[]`, attachmentRows: 1}
	if err := persistCurrentAgentTerminal(t.Context(), writer, projection.Expected, terminal); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(writer.mixed.HitlInterrupts), "sensitive-1") || !strings.Contains(string(writer.mixed.AuthorizationRequests), "auth-1") {
		t.Fatal("mixed projection lost identities")
	}
	if writer.hitl.ThreadID != "" {
		t.Fatal("mixed pause used the single-kind writer")
	}
}
