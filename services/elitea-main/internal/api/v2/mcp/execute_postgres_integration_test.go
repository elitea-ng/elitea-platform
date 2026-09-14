package mcp_test

// Acceptance for `tools/call` RUNNING an agent, against a real PostgreSQL.
//
// What a unit test cannot reach and this does:
//
//   - the descriptor's target really is the row's application and version, so
//     the turn the use case is handed names the agent the caller listed;
//   - the conversation this creates is one the RUNTIME RESOLVER can use — the
//     five rows are asserted through the same joins
//     ResolveCurrentApplicationTurn performs (internal/db/queries/agent_chat.sql),
//     which is the difference between "rows were written" and "a turn can be
//     admitted against them";
//   - the answer handed back is the one in the chat projection, so the
//     async→sync bridge reads the right row and returns real text.
//
// The start use case is faked. Admitting a turn for real would need the whole
// runtime plane — outbox, Redis stream, a Python or Rust worker — which is not
// what this file is about: the seam under test is everything on THIS side of
// `StartCurrentApplication`, plus the settle poll on the other side of it.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/mcp"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

/* ── fake start ────────────────────────────────────────────────────────── */

// fakeStart records the admission request and answers with a canned outcome.
//
// It also lets a test act as the RUNTIME would: `onStart` runs while the
// handler is inside StartCurrentApplication, which is exactly where the real
// admission writes the question and response message groups, so a test can put
// the projected answer in place and let the settle poll find it.
type fakeStart struct {
	requests []agentexecutionapp.CurrentApplicationStartRequest
	outcome  agentexecutionapp.CurrentApplicationStartOutcome
	err      error
	onStart  func(agentexecutionapp.CurrentApplicationStartRequest)
}

func (s *fakeStart) StartCurrentApplication(
	_ context.Context, request agentexecutionapp.CurrentApplicationStartRequest,
) (agentexecutionapp.CurrentApplicationStartOutcome, error) {
	s.requests = append(s.requests, request)
	if s.onStart != nil {
		s.onStart(request)
	}
	return s.outcome, s.err
}

/* ── seeding the projected answer ──────────────────────────────────────── */

// projectAnswer writes what the runtime's terminal projection writes: a
// response message group, settled, carrying the assistant's text.
//
// The columns are the ones the real finalizers set (agent_chat.sql:
// InsertCurrentApplicationTurn for the group, InsertCurrentAgentTextItem and
// InsertCurrentAgentTextContent for the text, FinalizeCurrentAgentFullMessage
// for `is_streaming = FALSE` and the meta). Writing them here rather than
// running the runtime is what keeps this test about the READ.
func projectAnswer(
	t *testing.T, pool *pgxpool.Pool, schema, conversationUUID, responseMessageID, meta string, chunks ...string,
) {
	t.Helper()
	ctx := context.Background()
	var conversationID int64
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT id FROM %q.chat_conversations WHERE uuid = $1::uuid`, schema),
		conversationUUID).Scan(&conversationID); err != nil {
		t.Fatalf("resolve conversation %s: %v", conversationUUID, err)
	}
	var groupID int64
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %q.chat_message_group (uuid, conversation_id, meta, is_streaming)
		VALUES ($1::uuid, $2, $3::jsonb, FALSE) RETURNING id`, schema),
		responseMessageID, conversationID, meta).Scan(&groupID); err != nil {
		t.Fatalf("seed response group: %v", err)
	}
	for index, chunk := range chunks {
		var itemID int64
		if err := pool.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %q.chat_message_items (uuid, item_type, order_index, meta, message_group_id)
			VALUES (gen_random_uuid(), 'text_message', $1, '{}'::jsonb, $2) RETURNING id`, schema),
			index, groupID).Scan(&itemID); err != nil {
			t.Fatalf("seed response item: %v", err)
		}
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %q.chat_messages_text (id, content) VALUES ($1, $2)`, schema),
			itemID, chunk); err != nil {
			t.Fatalf("seed response text: %v", err)
		}
	}
}

const completedMeta = `{"is_error": false, "error": "", "thread_id": "t-1"}`

// allowRuns is the permission resolver a project member who may start a chat
// turn gets.
//
// The RESOLVER is faked here and the rows are real, which is the right way
// round for this file: what it is asserting is the execution bridge, and the
// permission gate's own behaviour — including that it refuses a member without
// the permission — is asserted in execute_test.go against every resolver
// answer. Seeding a full role graph here would test legacyrbac twice.
type allowRuns struct{ userID int64 }

func (a allowRuns) ResolvePermissions(
	_ context.Context, _ auth.User, _ string, _ string,
) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{
		UserID:      a.userID,
		Permissions: []string{"models.chat.messages.create"},
	}, nil
}

func callTool(t *testing.T, pool *pgxpool.Pool, start mcp.AgentStartUseCase, target, name, task string) map[string]any {
	t.Helper()
	router := newRouter(
		mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), start, allowRuns{userID: callerUserID}),
		callerUserID)
	body := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{"task":%q}}}`, name, task)
	recorder := do(t, router, http.MethodPost, target, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("tools/call: status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	decoded := decode(t, recorder)
	if errorMember, present := decoded["error"]; present {
		t.Fatalf("tools/call: JSON-RPC error %v", errorMember)
	}
	result, ok := decoded["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call: result missing in %v", decoded)
	}
	return result
}

func resultText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content = %v, want one block", result["content"])
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	return text
}

/* ── the run ───────────────────────────────────────────────────────────── */

// The whole bridge, end to end on this side of the runtime: a listed agent is
// called, a conversation is created for it, the turn names that agent's own
// application and version, and the answer that lands in the projection is what
// the client is handed.
func TestCallOfAnAgentToolRunsItAndReturnsTheAnswer(t *testing.T) {
	pool := newMCPPool(t)
	seedUser(t, pool, callerUserID)
	versionID := seedAgent(t, pool, homeSchema, "Release Notes", "writes release notes", "mcp")

	const responseMessageID = "3f1d1d3a-0d0a-4a0e-9a0e-1c2d3e4f5a6b"
	start := &fakeStart{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID:       "exec-1",
		CommandID:         "cmd-1",
		ResponseMessageID: responseMessageID,
		Created:           true,
	}}
	start.onStart = func(request agentexecutionapp.CurrentApplicationStartRequest) {
		projectAnswer(t, pool, homeSchema, request.ConversationUUID, responseMessageID, completedMeta,
			"Release ", "notes.")
	}

	result := callTool(t, pool, start, "/app/"+homeProject+"/mcp", "Release_Notes", "summarise the sprint")

	if _, present := result["isError"]; present {
		t.Fatalf("a successful run reported isError: %v", result)
	}
	// The chunks are concatenated IN ITEM ORDER, which is how the transcript
	// assembles them; a test with one chunk could not see a reversal.
	if got := resultText(t, result); got != "Release notes." {
		t.Fatalf("text = %q, want %q", got, "Release notes.")
	}

	if len(start.requests) != 1 {
		t.Fatalf("start called %d times, want once", len(start.requests))
	}
	request := start.requests[0]
	if request.UserInput != "summarise the sprint" {
		t.Fatalf("UserInput = %q, want the caller's task", request.UserInput)
	}
	if request.ActorUserID != callerUserID {
		t.Fatalf("ActorUserID = %d, want %d", request.ActorUserID, callerUserID)
	}
	if request.ProjectID != 1 {
		t.Fatalf("ProjectID = %d, want 1", request.ProjectID)
	}

	// THE DISCRIMINATING ASSERTION. The participant the turn is addressed to
	// must resolve, through the joins ResolveCurrentApplicationTurn performs,
	// to the application version that backs the tool that was called. Asserting
	// only that rows exist would pass against a conversation pointing at some
	// other agent.
	var resolvedApplicationID, resolvedVersionID int64
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(`
		SELECT (target_participant.entity_meta ->> 'id')::bigint,
		       (target_mapping.entity_settings ->> 'version_id')::bigint
		FROM %[1]q.chat_conversations AS conversation
		JOIN %[1]q.chat_participant_mapping AS author_mapping
		  ON author_mapping.conversation_id = conversation.id
		JOIN %[1]q.chat_participants AS author_participant
		  ON author_participant.id = author_mapping.participant_id
		 AND author_participant.entity_name = 'user'
		 AND (author_participant.entity_meta ->> 'id')::bigint = $2
		JOIN %[1]q.chat_participant_mapping AS target_mapping
		  ON target_mapping.conversation_id = conversation.id
		 AND target_mapping.participant_id = $3
		JOIN %[1]q.chat_participants AS target_participant
		  ON target_participant.id = target_mapping.participant_id
		 AND target_participant.entity_name = 'application'
		JOIN %[1]q.application_versions AS application_version
		  ON application_version.id = (target_mapping.entity_settings ->> 'version_id')::integer
		 AND application_version.application_id = (target_participant.entity_meta ->> 'id')::integer
		WHERE conversation.uuid = $1::uuid`, homeSchema),
		request.ConversationUUID, callerUserID, request.TargetParticipantID,
	).Scan(&resolvedApplicationID, &resolvedVersionID); err != nil {
		t.Fatalf("the created conversation does not resolve as a runtime turn: %v", err)
	}
	if resolvedVersionID != versionID {
		t.Fatalf("resolved version = %d, want the tagged agent's version %d", resolvedVersionID, versionID)
	}

	// And the conversation is visible rather than hidden — the whole reason
	// this creates one (see execute.go's decision note).
	var source string
	var meta map[string]any
	var rawMeta []byte
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(`
		SELECT source, meta FROM %q.chat_conversations WHERE uuid = $1::uuid`, homeSchema),
		request.ConversationUUID).Scan(&source, &rawMeta); err != nil {
		t.Fatalf("read created conversation: %v", err)
	}
	if err := json.Unmarshal(rawMeta, &meta); err != nil {
		t.Fatalf("decode conversation meta: %v", err)
	}
	if source != "mcp" {
		t.Fatalf("conversation source = %q, want mcp", source)
	}
	if _, hidden := meta["is_hidden"]; hidden {
		t.Fatalf("conversation is hidden, so an operator cannot see what was run: %v", meta)
	}
	if meta["mcp_tool"] != "Release_Notes" {
		t.Fatalf("conversation meta does not name the tool: %v", meta)
	}
}

// A run that the projection marks failed must be reported as failed, with the
// runtime's own reason — not waited out until the deadline.
func TestCallOfAnAgentToolReportsAProjectedFailure(t *testing.T) {
	pool := newMCPPool(t)
	seedUser(t, pool, callerUserID)
	seedAgent(t, pool, homeSchema, "Release Notes", "writes release notes", "mcp")

	const responseMessageID = "4f1d1d3a-0d0a-4a0e-9a0e-1c2d3e4f5a6c"
	start := &fakeStart{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "exec-2", ResponseMessageID: responseMessageID,
	}}
	start.onStart = func(request agentexecutionapp.CurrentApplicationStartRequest) {
		projectAnswer(t, pool, homeSchema, request.ConversationUUID, responseMessageID,
			`{"is_error": true, "error": "the model provider refused"}`)
	}

	result := callTool(t, pool, start, "/app/"+homeProject+"/mcp", "Release_Notes", "go")

	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	text := resultText(t, result)
	if !strings.Contains(text, "the model provider refused") {
		t.Fatalf("text = %q, want the runtime's own reason", text)
	}
	if !strings.Contains(text, "exec-2") {
		t.Fatalf("text = %q, want it to name the execution id", text)
	}
}

// A run the projection PAUSED is terminal for MCP: there is no way to answer a
// human-approval prompt from a tool result, so it is reported rather than
// waited on.
func TestCallOfAnAgentToolReportsAPauseRatherThanWaiting(t *testing.T) {
	pool := newMCPPool(t)
	seedUser(t, pool, callerUserID)
	seedAgent(t, pool, homeSchema, "Release Notes", "writes release notes", "mcp")

	const responseMessageID = "5f1d1d3a-0d0a-4a0e-9a0e-1c2d3e4f5a6d"
	start := &fakeStart{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "exec-3", ResponseMessageID: responseMessageID,
	}}
	start.onStart = func(request agentexecutionapp.CurrentApplicationStartRequest) {
		projectAnswer(t, pool, homeSchema, request.ConversationUUID, responseMessageID,
			`{"is_error": false, "error": "", "hitl_interrupt": {"question": "may I?"}}`)
	}

	result := callTool(t, pool, start, "/app/"+homeProject+"/mcp", "Release_Notes", "go")

	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	if text := resultText(t, result); !strings.Contains(text, "human approval") ||
		!strings.Contains(text, "exec-3") {
		t.Fatalf("text = %q, want it to name the pause and the execution", text)
	}
}

// A turn the runtime REFUSES TO ADMIT leaves no empty transcript behind. A
// client retrying against a misconfigured agent would otherwise fill the chat
// list with conversations that never held anything.
func TestARefusedAdmissionLeavesNoEmptyConversation(t *testing.T) {
	pool := newMCPPool(t)
	seedUser(t, pool, callerUserID)
	seedAgent(t, pool, homeSchema, "Release Notes", "writes release notes", "mcp")

	start := &fakeStart{err: agentexecutionapp.ErrUnsupportedCurrentAgentStart}
	result := callTool(t, pool, start, "/app/"+homeProject+"/mcp", "Release_Notes", "go")

	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	if text := resultText(t, result); !strings.Contains(text, "Release_Notes") {
		t.Fatalf("text = %q, want it to name the tool", text)
	}
	if len(start.requests) != 1 {
		t.Fatalf("start called %d times, want once — the conversation must be built before the admission",
			len(start.requests))
	}
	var conversations int
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT count(*) FROM %q.chat_conversations`, homeSchema)).Scan(&conversations); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if conversations != 0 {
		t.Fatalf("a refused admission left %d conversations behind, want 0", conversations)
	}
}

// A TOOLKIT tool is refused even with a runtime present, and refused with the
// sentence that names only the missing half — and it must not create a
// conversation on its way to refusing.
func TestCallOfAToolkitToolStillRefusesAndWritesNothing(t *testing.T) {
	pool := newMCPPool(t)
	seedUser(t, pool, callerUserID)
	seedToolkit(t, pool, homeSchema, "github", "github", availableByMCP, "get_issue")

	start := &fakeStart{}
	result := callTool(t, pool, start, "/app/"+homeProject+"/mcp", "github_get_issue", "go")

	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	if text := resultText(t, result); text != mcp.ToolkitExecutionUnavailableReason {
		t.Fatalf("text = %q,\nwant %q", text, mcp.ToolkitExecutionUnavailableReason)
	}
	if len(start.requests) != 0 {
		t.Fatalf("start called %d times for a toolkit tool, want 0", len(start.requests))
	}
	var conversations int
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT count(*) FROM %q.chat_conversations`, homeSchema)).Scan(&conversations); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if conversations != 0 {
		t.Fatalf("a refused toolkit call wrote %d conversations, want 0", conversations)
	}
}

// A deployment with no runtime keeps answering exactly what it always did, and
// writes nothing — asserted against a real database so a stray conversation
// insert on the refusal path would be caught.
func TestCallWithoutARuntimeRefusesVerbatimAndWritesNothing(t *testing.T) {
	pool := newMCPPool(t)
	seedUser(t, pool, callerUserID)
	seedAgent(t, pool, homeSchema, "Release Notes", "writes release notes", "mcp")

	result := callTool(t, pool, nil, "/app/"+homeProject+"/mcp", "Release_Notes", "go")

	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	if text := resultText(t, result); text != mcp.ToolExecutionUnavailableReason {
		t.Fatalf("text = %q,\nwant the original refusal %q", text, mcp.ToolExecutionUnavailableReason)
	}
	var conversations int
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT count(*) FROM %q.chat_conversations`, homeSchema)).Scan(&conversations); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if conversations != 0 {
		t.Fatalf("a runtime-less refusal wrote %d conversations, want 0", conversations)
	}
}

// The resource-scoped URL runs the version the CALLER named, not a
// project-wide pick — and it does so for an agent carrying no `mcp` tag, which
// is the scope's documented behaviour on the listing side too.
func TestResourceScopedCallRunsTheNamedVersion(t *testing.T) {
	pool := newMCPPool(t)
	seedUser(t, pool, callerUserID)
	versionID := seedAgent(t, pool, homeSchema, "Direct Agent", "addressed by id")

	const responseMessageID = "6f1d1d3a-0d0a-4a0e-9a0e-1c2d3e4f5a6e"
	start := &fakeStart{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "exec-4", ResponseMessageID: responseMessageID,
	}}
	start.onStart = func(request agentexecutionapp.CurrentApplicationStartRequest) {
		projectAnswer(t, pool, homeSchema, request.ConversationUUID, responseMessageID, completedMeta, "done")
	}

	target := fmt.Sprintf("/app/%s/mcp/agent/%d", homeProject, versionID)
	result := callTool(t, pool, start, target, "Direct_Agent", "go")

	if _, present := result["isError"]; present {
		t.Fatalf("run reported isError: %v", result)
	}
	if len(start.requests) != 1 {
		t.Fatalf("start called %d times, want once", len(start.requests))
	}
	var resolvedVersionID int64
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(`
		SELECT (mapping.entity_settings ->> 'version_id')::bigint
		FROM %[1]q.chat_participant_mapping AS mapping
		WHERE mapping.participant_id = $1`, homeSchema),
		start.requests[0].TargetParticipantID).Scan(&resolvedVersionID); err != nil {
		t.Fatalf("read participant mapping: %v", err)
	}
	if resolvedVersionID != versionID {
		t.Fatalf("version = %d, want the version named in the URL %d", resolvedVersionID, versionID)
	}
}

// Pipelines use the same application execution bridge, but their persisted
// discriminator makes the worker compile a graph instead of an agent loop.
// Keep a separate acceptance case so agent-only fixtures cannot make the
// external pipeline claim appear covered.
func TestResourceScopedPipelineCallRunsTheNamedPipelineVersion(t *testing.T) {
	pool := newMCPPool(t)
	seedUser(t, pool, callerUserID)
	versionID := seedPipeline(t, pool, homeSchema, "Release Pipeline", "runs the release graph")

	const responseMessageID = "7f1d1d3a-0d0a-4a0e-9a0e-1c2d3e4f5a6f"
	start := &fakeStart{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "exec-pipeline-1", ResponseMessageID: responseMessageID,
	}}
	start.onStart = func(request agentexecutionapp.CurrentApplicationStartRequest) {
		projectAnswer(t, pool, homeSchema, request.ConversationUUID, responseMessageID, completedMeta, "pipeline done")
	}

	target := fmt.Sprintf("/app/%s/mcp/pipeline/%d", homeProject, versionID)
	result := callTool(t, pool, start, target, "Release_Pipeline", "prepare the release")

	if got := resultText(t, result); got != "pipeline done" {
		t.Fatalf("text = %q, want the pipeline result", got)
	}
	if len(start.requests) != 1 {
		t.Fatalf("start called %d times, want once", len(start.requests))
	}
	var resolvedVersionID int64
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(`
		SELECT (mapping.entity_settings ->> 'version_id')::bigint
		FROM %[1]q.chat_participant_mapping AS mapping
		WHERE mapping.participant_id = $1`, homeSchema),
		start.requests[0].TargetParticipantID).Scan(&resolvedVersionID); err != nil {
		t.Fatalf("read pipeline participant mapping: %v", err)
	}
	if resolvedVersionID != versionID {
		t.Fatalf("version = %d, want the pipeline version named in the URL %d", resolvedVersionID, versionID)
	}
}
