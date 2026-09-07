package supportassistant

// THE SUPPORT TURN, AGAINST A REAL DATABASE AND THE REAL TURN RESOLVER.
//
// The two defects this file exists for could not be seen by a mock. Both were
// missing ROWS, and the thing that objected to them was one SQL statement in
// another package:
//
//	ResolveCurrentApplicationTurn — internal/db/queries/agent_chat.sql
//
// It joins `chat_participants` on `entity_name = 'user'` for the caller, and
// `application_versions` on `entity_settings ->> 'version_id'`. This package
// wrote neither, so the join produced no row, the repository answered the bare
// `ErrUnsupportedCurrentAgentStart` sentinel, and every support question
// answered 502 — on a stack where the package's own 12 unit tests were green
// and the coverage was 18%.
//
// So the tests below drive the ROUTES, against a migrated PostgreSQL database,
// and then run the REAL `CurrentAgentStartRepository.ResolveCurrentApplication`
// over the rows the routes wrote. Nothing here restates the query or asserts on
// a fixture of what the rows "should" look like: the proof is that the shipped
// resolver accepts them.
//
// Requires a PostgreSQL service. Set ELITEA_TEST_DATABASE_URL.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const (
	supportDatabaseURLVariable = "ELITEA_TEST_DATABASE_URL"
	// supportTenant is the hidden support project every test in this file
	// resolves. It is not 1: a project id that is also the default hides a
	// handler that forgot to read the configured one.
	supportTenant = 7
	// supportCallerID and supportOtherCallerID are two ordinary platform users.
	// The second one exists to prove the author predicate, which is the only
	// thing keeping one user's support transcript out of another's history.
	supportCallerID      = 11
	supportOtherCallerID = 12
	// supportAgentID names the application the operator points the assistant
	// at, and supportAgentVersionID its published version.
	supportAgentID        = 31
	supportAgentVersionID = 41
)

var supportTemplate string

func TestMain(m *testing.M) {
	databaseURL := os.Getenv(supportDatabaseURLVariable)
	if databaseURL == "" {
		// Every PostgreSQL test here skips itself. Run the rest.
		os.Exit(m.Run())
	}
	ctx, cancel := dbtest.BuildContext(context.Background())
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", supportDatabaseURLVariable, err)
		cancel()
		os.Exit(1)
	}
	name, err := dbtest.EnsureTemplate(ctx, adminPool, dbtest.Spec{
		Files:   platformmigrations.Files,
		Seed:    supportSeedSQL,
		Tenants: []int64{supportTenant},
	})
	adminPool.Close()
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build the support integration template: %v\n", err)
		os.Exit(1)
	}
	supportTemplate = name
	os.Exit(m.Run())
}

// supportSeedSQL is the LEGACY-OWNED schema, the part the ledgered corpus does
// not create because pylon created it first. It is the same arrangement every
// other integration suite here uses: the seed provides what a real deployment
// inherited, the corpus then runs on top of it, and the test proves the
// corpus's tables and the inherited ones work together.
const supportSeedSQL = `
CREATE SCHEMA centry;
CREATE TABLE centry.project (
    id INTEGER PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    create_success BOOLEAN NOT NULL DEFAULT TRUE,
    suspended BOOLEAN NOT NULL DEFAULT FALSE
);
INSERT INTO centry.project (id, name) VALUES (7, 'Support Assistant');
CREATE TABLE centry.platform_config (
    section VARCHAR(64) NOT NULL,
    key VARCHAR(128) NOT NULL,
    value JSONB NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_by VARCHAR(255),
    PRIMARY KEY (section, key)
);
CREATE TABLE centry.social_users (
    user_id INTEGER PRIMARY KEY,
    avatar VARCHAR
);
CREATE TABLE auth_core__user (
    id SERIAL PRIMARY KEY,
    email VARCHAR(255) NOT NULL UNIQUE,
    name VARCHAR(255)
);
INSERT INTO auth_core__user (id, email, name) VALUES
    (11, 'caller@example.com', 'Caller'),
    (12, 'other@example.com', 'Other');
SELECT setval(pg_get_serial_sequence('auth_core__user', 'id'), 100);
CREATE TABLE auth_core__project_role (
    id SERIAL PRIMARY KEY,
    project_id INTEGER NOT NULL,
    name VARCHAR(64) NOT NULL,
    CONSTRAINT _project_role_uc UNIQUE (project_id, name)
);
INSERT INTO auth_core__project_role (id, project_id, name) VALUES (1, 7, 'viewer');
CREATE TABLE auth_core__project_user_role (
    project_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    role_id INTEGER NOT NULL,
    PRIMARY KEY (project_id, user_id, role_id)
);
CREATE SCHEMA p_7;
CREATE TABLE p_7.applications (
    id SERIAL PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    owner_id INTEGER NOT NULL DEFAULT 11,
    meta JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE TABLE p_7.application_versions (
    id SERIAL PRIMARY KEY,
    application_id INTEGER NOT NULL REFERENCES p_7.applications(id) ON DELETE CASCADE,
    name VARCHAR(128) NOT NULL,
    status VARCHAR NOT NULL DEFAULT 'draft',
    author_id INTEGER NOT NULL DEFAULT 11,
    uuid UUID NOT NULL UNIQUE DEFAULT gen_random_uuid(),
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    llm_settings JSONB NOT NULL DEFAULT '{}'::jsonb,
    instructions VARCHAR,
    conversation_starters JSON NOT NULL DEFAULT '[]'::json,
    welcome_message VARCHAR NOT NULL DEFAULT '',
    agent_type VARCHAR NOT NULL DEFAULT 'openai',
    meta JSONB NOT NULL DEFAULT '{}'::jsonb,
    pipeline_settings JSONB NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT _application_version_name_uc UNIQUE (application_id, name)
);
CREATE TABLE p_7.application_variables (
    id SERIAL PRIMARY KEY,
    application_version_id INTEGER NOT NULL REFERENCES p_7.application_versions(id) ON DELETE CASCADE,
    name VARCHAR NOT NULL,
    value VARCHAR,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_at TIMESTAMP,
    CONSTRAINT _application_version_variable_name_uc UNIQUE (application_version_id, name)
);
CREATE TABLE p_7.skills (
    id SERIAL PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    description VARCHAR(2304) NOT NULL,
    owner_id INTEGER NOT NULL DEFAULT 11,
    author_id INTEGER NOT NULL DEFAULT 11,
    meta JSONB DEFAULT '{}'::jsonb
);
CREATE TABLE p_7.skill_versions (
    id SERIAL PRIMARY KEY,
    skill_id INTEGER NOT NULL REFERENCES p_7.skills(id) ON DELETE CASCADE,
    name VARCHAR(128) NOT NULL DEFAULT 'base',
    instructions TEXT NOT NULL,
    author_id INTEGER NOT NULL DEFAULT 11,
    meta JSONB DEFAULT '{}'::jsonb
);
CREATE TABLE p_7.elitea_tools (
    id SERIAL PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    type VARCHAR(64) NOT NULL,
    description TEXT,
    owner_id INTEGER NOT NULL DEFAULT 11,
    author_id INTEGER NOT NULL DEFAULT 11,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    uuid UUID UNIQUE DEFAULT gen_random_uuid(),
    meta JSONB DEFAULT '{}'::jsonb,
    settings JSONB DEFAULT '{}'::jsonb,
    env_vars JSONB DEFAULT '{}'::jsonb
);
CREATE TABLE p_7.entity_tool_mapping (
    id SERIAL PRIMARY KEY,
    entity_version_id INTEGER NOT NULL,
    entity_type VARCHAR(50) NOT NULL,
    tool_id INTEGER NOT NULL REFERENCES p_7.elitea_tools(id) ON DELETE CASCADE,
    selected_tools JSONB DEFAULT '[]'::jsonb,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_at TIMESTAMP,
    CONSTRAINT _entity_tool_unique UNIQUE (entity_version_id, tool_id, entity_type)
);
CREATE TABLE p_7.configuration (
    id SERIAL PRIMARY KEY,
    uuid UUID NOT NULL UNIQUE,
    project_id INTEGER NOT NULL,
    label VARCHAR,
    elitea_title VARCHAR NOT NULL UNIQUE,
    type VARCHAR NOT NULL,
    section VARCHAR NOT NULL,
    data JSONB NOT NULL,
    meta JSONB NOT NULL,
    shared BOOLEAN NOT NULL,
    status_ok BOOLEAN NOT NULL,
    status_logs TEXT,
    source VARCHAR NOT NULL,
    author_id INTEGER,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_at TIMESTAMP
);
CREATE TABLE p_7.entity_skill_mapping (
    id SERIAL PRIMARY KEY,
    entity_version_id INTEGER NOT NULL,
    entity_type VARCHAR(50) NOT NULL,
    skill_id INTEGER NOT NULL REFERENCES p_7.skills(id),
    skill_version_id INTEGER REFERENCES p_7.skill_versions(id)
);
INSERT INTO p_7.applications (id, name) VALUES (31, 'Support Agent');
INSERT INTO p_7.application_versions (id, application_id, name, agent_type, instructions)
VALUES (41, 31, 'latest', 'openai', 'Answer support questions');
SELECT setval(pg_get_serial_sequence('p_7.applications', 'id'), 100);
SELECT setval(pg_get_serial_sequence('p_7.application_versions', 'id'), 100);
`

/* ── the harness ───────────────────────────────────────────────────────── */

// newSupportPool copies the template into a private database.
func newSupportPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(supportDatabaseURLVariable)
	if databaseURL == "" {
		t.Skipf("set %s to run the support-assistant PostgreSQL integration test", supportDatabaseURLVariable)
	}
	if supportTemplate == "" {
		t.Fatal("TestMain did not build the support integration template")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", supportDatabaseURLVariable, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open the admin pool: %v", err)
	}
	name := fmt.Sprintf("elitea_support_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := dbtest.CreateFromTemplate(ctx, adminPool, supportTemplate, name); err != nil {
		adminPool.Close()
		t.Fatalf("copy the template: %v", err)
	}
	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = name
	// Small on purpose. This suite runs beside every other integration package
	// in the workspace against ONE PostgreSQL service, and a generous per-test
	// pool exhausts `max_connections` for everybody (SQLSTATE 53300). Four is
	// more than any test here uses at once.
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		adminPool.Close()
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		quoted := pgx.Identifier{name}.Sanitize()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
		adminPool.Close()
	})
	return pool
}

// enableAssistant writes the operator's configuration, exactly as the admin
// Features page does — one row per key in `centry.platform_config`.
func enableAssistant(t *testing.T, pool *pgxpool.Pool, values map[string]any) {
	t.Helper()
	for key, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `
INSERT INTO centry.platform_config (section, key, value)
VALUES ('support_assistant', $1, $2::jsonb)
ON CONFLICT (section, key) DO UPDATE SET value = EXCLUDED.value`, key, string(encoded)); err != nil {
			t.Fatalf("write %s: %v", key, err)
		}
	}
}

// enableConfiguredAssistant is the ordinary, correct operator configuration:
// the switch on, the support project bootstrapped, an agent chosen in it.
func enableConfiguredAssistant(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	enableAssistant(t, pool, map[string]any{
		"support_assistant_enabled": true,
		"support_project_id":        supportTenant,
		"support_agent_id":          supportAgentID,
		"support_agent_project_id":  supportTenant,
		"support_assistant_name":    "Client Support",
	})
}

// grantingResolver admits the caller. The permission model is not what these
// tests are about — reusing `models.chat.*` is settled and covered by the unit
// tests — so the gate is satisfied rather than re-proved here.
type grantingResolver struct{ userID int64 }

func (g grantingResolver) ResolvePermissions(
	_ context.Context, _ auth.User, _ string, _ string,
) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{
		UserID: g.userID,
		Permissions: []string{
			PermissionConversationsList, PermissionConversationsCreate,
			PermissionConversationRead, PermissionMessagesCreate,
		},
	}, nil
}

// resolvingStart is the StartUseCase, and it is the point of this file.
//
// It does not pretend to run an agent. It runs the REAL turn resolver —
// `CurrentAgentStartRepository.ResolveCurrentApplication`, the same call the
// production use case makes first — over the rows the facade has just written.
// A test that stubbed this would have passed on the broken code, because the
// broken code's request was perfectly well formed; only the database rows
// behind it were not.
type resolvingStart struct {
	repository *repos.CurrentAgentStartRepository
	requests   []agentexecutionapp.CurrentApplicationStartRequest
	target     agentexecutionapp.CurrentApplicationTarget
}

func (s *resolvingStart) StartCurrentApplication(
	ctx context.Context, request agentexecutionapp.CurrentApplicationStartRequest,
) (agentexecutionapp.CurrentApplicationStartOutcome, error) {
	s.requests = append(s.requests, request)
	target, err := s.repository.ResolveCurrentApplication(ctx, request)
	if err != nil {
		return agentexecutionapp.CurrentApplicationStartOutcome{}, err
	}
	s.target = target
	return agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "execution-1", CommandID: "command-1",
		ResponseMessageID: "response-1", Created: true,
	}, nil
}

// supportHarness is the wired facade: real store, real chat repository, real
// turn resolver.
type supportHarness struct {
	pool    *pgxpool.Pool
	routes  http.Handler
	start   *resolvingStart
	handler *Handler
}

func newSupportHarness(t *testing.T) *supportHarness {
	t.Helper()
	pool := newSupportPool(t)

	chat := repos.NewConversationsRepo(pool)
	startRepository, err := repos.NewCurrentAgentStartRepository(pool)
	if err != nil {
		t.Fatalf("build the current-agent start repository: %v", err)
	}
	start := &resolvingStart{repository: startRepository}
	handler := NewHandler(pool,
		WithChatStore(chat),
		WithStartUseCase(start),
		WithPermissionResolver(grantingResolver{userID: supportCallerID}),
	)
	return &supportHarness{pool: pool, routes: handler.Routes(), start: start, handler: handler}
}

// call drives one request through the whole route chain, as the browser does.
func (h *supportHarness) call(
	t *testing.T, userID int64, method, path, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	request := httptest.NewRequest(method, path, reader)
	identity := strconv.FormatInt(userID, 10)
	request = withUser(request, auth.User{ID: identity, UserID: identity, Name: "Caller"})
	recorder := httptest.NewRecorder()
	h.routes.ServeHTTP(recorder, request)
	return recorder
}

// createConversation opens one support conversation through the route and
// returns its UUID.
func (h *supportHarness) createConversation(t *testing.T, userID int64) string {
	t.Helper()
	recorder := h.call(t, userID, http.MethodPost, "/conversations/", `{}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create conversation status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var created Conversation
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode the created conversation: %v", err)
	}
	if created.UUID == "" || created.ID == 0 {
		t.Fatalf("the created conversation has no identity: %+v", created)
	}
	return created.UUID
}

const supportQuestionID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

func predictBody(content, questionID string) string {
	encoded, _ := json.Marshal(PredictRequest{
		Content: content, QuestionID: questionID,
		Context: &AssistantContext{CurrentPage: "/agents", ProjectName: "Acme"},
	})
	return string(encoded)
}

/* ── the headline: a turn the real resolver accepts ────────────────────── */

// A CONVERSATION THIS PACKAGE CREATES CAN ANSWER A QUESTION.
//
// Before the fix this test failed at the predict call with 502, because
// `CreateConversation` wrote no `user` participant and `ensureAgentParticipant`
// wrote `entity_settings = {}`. Both rows are asserted through the resolver
// rather than by reading them back: the resolver is the thing that objected.
func TestSupportTurnResolvesAgainstTheRowsTheFacadeWrites(t *testing.T) {
	harness := newSupportHarness(t)
	enableConfiguredAssistant(t, harness.pool)

	conversationUUID := harness.createConversation(t, supportCallerID)

	recorder := harness.call(t, supportCallerID, http.MethodPost,
		"/predict/"+conversationUUID, predictBody("why is my agent slow?", supportQuestionID))
	if recorder.Code != http.StatusOK {
		t.Fatalf("predict status = %d, body %s", recorder.Code, recorder.Body.String())
	}

	var response PredictResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode the predict response: %v", err)
	}
	if response.ExecutionID == "" || response.EventsURL == "" {
		t.Fatalf("the predict response carries no stream to subscribe to: %+v", response)
	}
	wantEvents := "/api/v2/executions/" + strconv.Itoa(supportTenant) + "/execution-1/events"
	if response.EventsURL != wantEvents {
		t.Fatalf("events_url = %q, want %q", response.EventsURL, wantEvents)
	}

	// The resolver accepted the turn, and accepted it against the VERSION the
	// operator's agent actually has.
	if harness.start.target.ApplicationID != supportAgentID ||
		harness.start.target.ApplicationVersionID != supportAgentVersionID {
		t.Fatalf("the turn resolved to application %d version %d, want %d/%d",
			harness.start.target.ApplicationID, harness.start.target.ApplicationVersionID,
			supportAgentID, supportAgentVersionID)
	}

	// The page context reached the agent as text on the message.
	if len(harness.start.requests) != 1 {
		t.Fatalf("start calls = %d, want 1", len(harness.start.requests))
	}
	if !strings.Contains(harness.start.requests[0].UserInput, "<support_assistant_context>") {
		t.Fatalf("the page context did not reach the run: %q", harness.start.requests[0].UserInput)
	}
}

// THE ROWS THE RESOLVER NEEDS ARE THE ROWS THAT LAND.
//
// The test above proves the turn resolves. This one names WHY, so a future
// change that removes one of the two rows fails with a sentence about that row
// rather than with an opaque 502.
func TestBothParticipantRowsLandWithTheSettingsTheResolverReads(t *testing.T) {
	harness := newSupportHarness(t)
	enableConfiguredAssistant(t, harness.pool)
	conversationUUID := harness.createConversation(t, supportCallerID)

	// The author participant is written by CREATE, before any turn runs. A
	// conversation the user opens and abandons is already resolvable.
	participants := harness.participants(t, conversationUUID)
	if !hasUserParticipant(participants, supportCallerID) {
		t.Fatalf("create wrote no entity_name='user' participant for the caller: %+v", participants)
	}

	if recorder := harness.call(t, supportCallerID, http.MethodPost,
		"/predict/"+conversationUUID, predictBody("hello", supportQuestionID)); recorder.Code != http.StatusOK {
		t.Fatalf("predict status = %d, body %s", recorder.Code, recorder.Body.String())
	}

	agent, found := findApplicationParticipant(harness.participants(t, conversationUUID), supportAgentID)
	if !found {
		t.Fatal("no application participant was attached")
	}
	versionID, ok := metaIntPresent(agent.EntitySettings, "version_id")
	if !ok || versionID != supportAgentVersionID {
		t.Fatalf("entity_settings.version_id = %v, want %d (settings %+v)",
			agent.EntitySettings["version_id"], supportAgentVersionID, agent.EntitySettings)
	}
	if agent.EntitySettings["agent_type"] != "openai" {
		t.Fatalf("entity_settings.agent_type = %v, want openai", agent.EntitySettings["agent_type"])
	}
	if _, present := agent.EntitySettings["variables"]; !present {
		t.Fatalf("entity_settings carries no variables key: %+v", agent.EntitySettings)
	}
}

/* ── the repair path ───────────────────────────────────────────────────── */

// A CONVERSATION BROKEN BY THE OLD CODE HEALS ON ITS NEXT TURN.
//
// The mapping insert is ON CONFLICT DO NOTHING, so a conversation that already
// holds the `{}` mapping cannot be corrected by attaching the agent again. Every
// deployment that tried the assistant before this change has such rows. Without
// a repair path those users would keep getting 502 forever, and would have no
// way to tell that opening a NEW conversation was the workaround.
func TestAConversationWrittenByTheBrokenCodeIsRepaired(t *testing.T) {
	harness := newSupportHarness(t)
	enableConfiguredAssistant(t, harness.pool)

	// Exactly what the shipped code left behind: a support conversation, an
	// application participant with an empty entity_settings document, and NO
	// user participant.
	conversationUUID := harness.seedBrokenConversation(t, supportCallerID)

	before := harness.participants(t, conversationUUID)
	if hasUserParticipant(before, supportCallerID) {
		t.Fatal("the broken fixture already has an author participant; it proves nothing")
	}
	agent, found := findApplicationParticipant(before, supportAgentID)
	if !found || len(agent.EntitySettings) != 0 {
		t.Fatalf("the broken fixture does not carry the empty mapping: %+v", before)
	}

	if recorder := harness.call(t, supportCallerID, http.MethodPost,
		"/predict/"+conversationUUID, predictBody("still broken?", supportQuestionID)); recorder.Code != http.StatusOK {
		t.Fatalf("predict status = %d, body %s", recorder.Code, recorder.Body.String())
	}

	after := harness.participants(t, conversationUUID)
	if !hasUserParticipant(after, supportCallerID) {
		t.Fatalf("the missing author participant was not added: %+v", after)
	}
	repaired, _ := findApplicationParticipant(after, supportAgentID)
	if versionID, ok := metaIntPresent(repaired.EntitySettings, "version_id"); !ok || versionID != supportAgentVersionID {
		t.Fatalf("the empty entity_settings was not repaired: %+v", repaired.EntitySettings)
	}
	if harness.start.target.ApplicationVersionID != supportAgentVersionID {
		t.Fatalf("the repaired conversation still does not resolve: %+v", harness.start.target)
	}
}

// A REPAIR DOES NOT DISCARD KEYS THIS PACKAGE DOES NOT OWN.
//
// `UpdateEntitySettings` replaces the whole document. A conversation may carry
// an operator's own `llm_settings` or a chat history template, and a repair
// that dropped them would be a silent data loss on a path taken by every turn.
func TestARepairKeepsSettingsThisPackageDoesNotOwn(t *testing.T) {
	harness := newSupportHarness(t)
	enableConfiguredAssistant(t, harness.pool)
	conversationUUID := harness.seedBrokenConversationWithSettings(t, supportCallerID,
		`{"chat_history_template":"all","llm_settings":{"temperature":0.1}}`)

	if recorder := harness.call(t, supportCallerID, http.MethodPost,
		"/predict/"+conversationUUID, predictBody("keep my settings", supportQuestionID)); recorder.Code != http.StatusOK {
		t.Fatalf("predict status = %d, body %s", recorder.Code, recorder.Body.String())
	}

	agent, _ := findApplicationParticipant(harness.participants(t, conversationUUID), supportAgentID)
	if agent.EntitySettings["chat_history_template"] != "all" {
		t.Fatalf("the repair dropped chat_history_template: %+v", agent.EntitySettings)
	}
	if _, present := agent.EntitySettings["llm_settings"]; !present {
		t.Fatalf("the repair dropped llm_settings: %+v", agent.EntitySettings)
	}
}

// A SECOND TURN WRITES NOTHING. The repair must be idempotent: a turn that
// finds correct rows must not UPDATE them, or every message rewrites the
// mapping row for nothing.
func TestASecondTurnDoesNotRewriteTheMapping(t *testing.T) {
	harness := newSupportHarness(t)
	enableConfiguredAssistant(t, harness.pool)
	conversationUUID := harness.createConversation(t, supportCallerID)

	for _, questionID := range []string{
		supportQuestionID, "4f2504e0-4f89-41d3-9a0c-0305e82c3302",
	} {
		if recorder := harness.call(t, supportCallerID, http.MethodPost,
			"/predict/"+conversationUUID, predictBody("again", questionID)); recorder.Code != http.StatusOK {
			t.Fatalf("predict status = %d, body %s", recorder.Code, recorder.Body.String())
		}
	}

	participants := harness.participants(t, conversationUUID)
	applications := 0
	users := 0
	for _, participant := range participants {
		switch participant.EntityName {
		case applicationEntityName:
			applications++
		case userEntityName:
			users++
		}
	}
	if applications != 1 || users != 1 {
		t.Fatalf("two turns produced %d application and %d user participants, want 1 and 1",
			applications, users)
	}
}

/* ── the configuration the facade cannot serve ─────────────────────────── */

// AN AGENT IN ANOTHER PROJECT IS REFUSED, AND REFUSED BEFORE THE RUN.
//
// A support turn runs in the support project, and the resolver reads
// `application_versions` from that project's tenant schema and requires the
// participant's `entity_meta.project_id` to be that project. So an agent kept
// elsewhere cannot answer whatever this package writes. Refusing at the attach
// step keeps the cause in one log line instead of turning it into a 502 from a
// resolver three packages away.
func TestAnAgentOutsideTheSupportProjectIsRefused(t *testing.T) {
	harness := newSupportHarness(t)
	enableAssistant(t, harness.pool, map[string]any{
		"support_assistant_enabled": true,
		"support_project_id":        supportTenant,
		"support_agent_id":          supportAgentID,
		"support_agent_project_id":  supportTenant + 1,
	})
	conversationUUID := harness.createConversation(t, supportCallerID)

	recorder := harness.call(t, supportCallerID, http.MethodPost,
		"/predict/"+conversationUUID, predictBody("who answers?", supportQuestionID))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(harness.start.requests) != 0 {
		t.Fatal("a run was started against an agent the resolver cannot reach")
	}
}

// AN AGENT ID THAT NAMES NOTHING IS REFUSED THE SAME WAY. The operator types
// raw integers on the Features page, so a typo is the ordinary case.
func TestAnAgentIDThatNamesNothingIsRefused(t *testing.T) {
	harness := newSupportHarness(t)
	enableAssistant(t, harness.pool, map[string]any{
		"support_assistant_enabled": true,
		"support_project_id":        supportTenant,
		"support_agent_id":          9999,
		"support_agent_project_id":  supportTenant,
	})
	conversationUUID := harness.createConversation(t, supportCallerID)
	recorder := harness.call(t, supportCallerID, http.MethodPost,
		"/predict/"+conversationUUID, predictBody("anyone?", supportQuestionID))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", recorder.Code, recorder.Body.String())
	}
}

/* ── which version answers ─────────────────────────────────────────────── */

// THE PINNED DEFAULT VERSION WINS, then `latest`, then the newest row.
//
// The operator configures an AGENT. Which of its versions answers is this
// package's decision, and it must be the platform's own: `SetDefaultVersion`
// writes `applications.meta.default_version_id`, and an operator who pins a
// version expects the assistant to use it.
func TestTheDefaultVersionAnswersWhenTheOperatorPinsOne(t *testing.T) {
	harness := newSupportHarness(t)
	enableConfiguredAssistant(t, harness.pool)

	// A second, newer version, and the agent pinned to the OLD one.
	if _, err := harness.pool.Exec(context.Background(), `
INSERT INTO p_7.application_versions (id, application_id, name, agent_type, created_at)
VALUES (42, 31, 'v2', 'openai', now() + interval '1 hour');
UPDATE p_7.applications SET meta = '{"default_version_id":"41"}'::jsonb WHERE id = 31;`); err != nil {
		t.Fatalf("seed the second version: %v", err)
	}

	version, err := harness.handler.store.agentVersionOf(context.Background(), supportTenant, supportAgentID)
	if err != nil {
		t.Fatalf("resolve the agent version: %v", err)
	}
	if version.ID != supportAgentVersionID {
		t.Fatalf("version = %d, want the pinned default %d", version.ID, supportAgentVersionID)
	}

	// With no pin, `latest` wins over the newer row.
	if _, err := harness.pool.Exec(context.Background(),
		`UPDATE p_7.applications SET meta = '{}'::jsonb WHERE id = 31`); err != nil {
		t.Fatal(err)
	}
	version, err = harness.handler.store.agentVersionOf(context.Background(), supportTenant, supportAgentID)
	if err != nil {
		t.Fatalf("resolve the agent version: %v", err)
	}
	if version.ID != supportAgentVersionID {
		t.Fatalf("version = %d, want the `latest` row %d", version.ID, supportAgentVersionID)
	}

	// With no `latest` either, the newest row answers rather than nothing.
	if _, err := harness.pool.Exec(context.Background(),
		`UPDATE p_7.application_versions SET name = 'v1' WHERE id = 41`); err != nil {
		t.Fatal(err)
	}
	version, err = harness.handler.store.agentVersionOf(context.Background(), supportTenant, supportAgentID)
	if err != nil {
		t.Fatalf("resolve the agent version: %v", err)
	}
	if version.ID != 42 {
		t.Fatalf("version = %d, want the newest row 42", version.ID)
	}
}

// A REPUBLISHED AGENT ANSWERS ON THE NEXT MESSAGE. The version is resolved per
// turn, so an operator who publishes a new version does not have to tell users
// to start a new conversation.
func TestANewDefaultVersionTakesEffectOnTheNextTurn(t *testing.T) {
	harness := newSupportHarness(t)
	enableConfiguredAssistant(t, harness.pool)
	conversationUUID := harness.createConversation(t, supportCallerID)

	if recorder := harness.call(t, supportCallerID, http.MethodPost,
		"/predict/"+conversationUUID, predictBody("first", supportQuestionID)); recorder.Code != http.StatusOK {
		t.Fatalf("predict status = %d, body %s", recorder.Code, recorder.Body.String())
	}

	if _, err := harness.pool.Exec(context.Background(), `
INSERT INTO p_7.application_versions (id, application_id, name, agent_type)
VALUES (42, 31, 'v2', 'openai');
UPDATE p_7.applications SET meta = '{"default_version_id":"42"}'::jsonb WHERE id = 31;`); err != nil {
		t.Fatalf("publish a new version: %v", err)
	}

	if recorder := harness.call(t, supportCallerID, http.MethodPost,
		"/predict/"+conversationUUID,
		predictBody("second", "4f2504e0-4f89-41d3-9a0c-0305e82c3302")); recorder.Code != http.StatusOK {
		t.Fatalf("predict status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	if harness.start.target.ApplicationVersionID != 42 {
		t.Fatalf("the second turn ran version %d, want the republished 42",
			harness.start.target.ApplicationVersionID)
	}
}

/* ── the shared project's author predicate ─────────────────────────────── */

// THE SUPPORT PROJECT IS SHARED BY EVERY USER, so the listing and the details
// route must show a caller their OWN transcripts and nothing else. "Not yours"
// and "does not exist" answer the same 404 on purpose.
func TestSupportHistoryIsScopedToItsAuthor(t *testing.T) {
	harness := newSupportHarness(t)
	enableConfiguredAssistant(t, harness.pool)

	mine := harness.createConversation(t, supportCallerID)

	// The other caller needs their own granting resolver, because the gate
	// rewrites the identity from the resolution.
	other := NewHandler(harness.pool,
		WithChatStore(mustConversationsRepo(t, harness.pool)),
		WithStartUseCase(harness.start),
		WithPermissionResolver(grantingResolver{userID: supportOtherCallerID}),
	).Routes()
	otherHarness := &supportHarness{pool: harness.pool, routes: other, start: harness.start}
	theirs := otherHarness.createConversation(t, supportOtherCallerID)

	listing := harness.call(t, supportCallerID, http.MethodGet, "/conversations/", "")
	if listing.Code != http.StatusOK {
		t.Fatalf("list status = %d, body %s", listing.Code, listing.Body.String())
	}
	var page ListResponse
	if err := json.Unmarshal(listing.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode the listing: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].UUID != mine {
		t.Fatalf("the listing is not scoped to its author: %+v", page)
	}
	if page.HasMore {
		t.Fatalf("has_more is set on a complete page: %+v", page)
	}

	if details := harness.call(t, supportCallerID, http.MethodGet, "/conversation/"+theirs, ""); details.Code != http.StatusNotFound {
		t.Fatalf("reading another user's conversation = %d, want 404", details.Code)
	}
	if predict := harness.call(t, supportCallerID, http.MethodPost,
		"/predict/"+theirs, predictBody("read this", supportQuestionID)); predict.Code != http.StatusNotFound {
		t.Fatalf("predicting into another user's conversation = %d, want 404", predict.Code)
	}
}

// THE DETAILS ROUTE RETURNS THE TRANSCRIPT AND THE PARTICIPANTS, which is what
// the widget re-renders a reopened conversation from.
func TestConversationDetailsCarryTheTranscriptAndParticipants(t *testing.T) {
	harness := newSupportHarness(t)
	enableConfiguredAssistant(t, harness.pool)
	conversationUUID := harness.createConversation(t, supportCallerID)

	recorder := harness.call(t, supportCallerID, http.MethodGet, "/conversation/"+conversationUUID, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var details ConversationDetails
	if err := json.Unmarshal(recorder.Body.Bytes(), &details); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if details.UUID != conversationUUID {
		t.Fatalf("uuid = %q, want %q", details.UUID, conversationUUID)
	}
	if details.MessageGroups == nil {
		t.Fatal("message_groups is null; the widget iterates it")
	}
	if !hasUserParticipant(details.Participants, supportCallerID) {
		t.Fatalf("the details response does not carry the author: %+v", details.Participants)
	}
}

// A CONVERSATION SEARCH MATCHES ON THE NAME the user gave it.
func TestConversationSearchMatchesTheStoredName(t *testing.T) {
	harness := newSupportHarness(t)
	enableConfiguredAssistant(t, harness.pool)

	if recorder := harness.call(t, supportCallerID, http.MethodPost, "/conversations/",
		`{"name":"billing 50% question"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	harness.createConversation(t, supportCallerID)

	// `%` is the user's character, not LIKE's.
	recorder := harness.call(t, supportCallerID, http.MethodGet, "/conversations/?q=50%25+question", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var page ListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 {
		t.Fatalf("search matched %d conversations, want 1: %+v", page.Total, page.Items)
	}
}

/* ── enrolment and configuration ───────────────────────────────────────── */

// A FIRST-TIME CALLER IS ENROLLED AS VIEWER, exactly once. The support project
// is shared, so nobody holds a role in it until they ask a question.
func TestAFirstTimeCallerIsEnrolledAsViewer(t *testing.T) {
	harness := newSupportHarness(t)
	enableConfiguredAssistant(t, harness.pool)

	harness.createConversation(t, supportCallerID)
	harness.createConversation(t, supportCallerID)

	var roles int
	if err := harness.pool.QueryRow(context.Background(), `
SELECT count(*) FROM auth_core__project_user_role
WHERE project_id = $1 AND user_id = $2`, supportTenant, supportCallerID).Scan(&roles); err != nil {
		t.Fatal(err)
	}
	if roles != 1 {
		t.Fatalf("the caller holds %d roles in the support project, want exactly 1 viewer", roles)
	}
}

// THE CONFIG ROUTE REPORTS THE OPERATOR'S STRINGS AND THE CALLER'S AVATAR.
func TestConfigReportsTheOperatorStringsAndTheCallersIdentity(t *testing.T) {
	harness := newSupportHarness(t)
	enableConfiguredAssistant(t, harness.pool)
	if _, err := harness.pool.Exec(context.Background(),
		`INSERT INTO centry.social_users (user_id, avatar) VALUES ($1, 'https://example.test/a.png')`,
		supportCallerID); err != nil {
		t.Fatal(err)
	}

	recorder := harness.call(t, supportCallerID, http.MethodGet, "/config/", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var body ConfigResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Enabled || body.Title != "Client Support" || body.SupportProjectID != supportTenant {
		t.Fatalf("config = %+v", body)
	}
	if body.User.ID != supportCallerID || body.User.Avatar == "" {
		t.Fatalf("the config carries no caller identity: %+v", body.User)
	}
	// The defaults answer for the strings the operator left alone.
	if body.WelcomeMessage == "" || body.Placeholder == "" {
		t.Fatalf("the defaulted strings are empty: %+v", body)
	}
}

// AN ENABLED DEPLOYMENT WITH NO AGENT IS NOT READY, and reports itself off
// rather than rendering a widget whose first message fails.
func TestAnEnabledDeploymentWithNoAgentReportsItselfOff(t *testing.T) {
	harness := newSupportHarness(t)
	enableAssistant(t, harness.pool, map[string]any{
		"support_assistant_enabled": true,
		"support_project_id":        supportTenant,
	})
	recorder := harness.call(t, supportCallerID, http.MethodGet, "/config/", "")
	var body ConfigResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Enabled {
		t.Fatalf("an assistant with no agent reported itself enabled: %+v", body)
	}
	if recorder := harness.call(t, supportCallerID, http.MethodPost, "/conversations/", `{}`); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("create status = %d, want 503", recorder.Code)
	}
}

/* ── the hidden project bootstrap ──────────────────────────────────────── */

// AN EXISTING "Support Assistant" PROJECT IS ADOPTED, NOT DUPLICATED.
//
// An operator who clears the id on the Features page must not get a second
// project. The seed's project 7 carries that name, so the bootstrap must find
// it and record its id.
func TestTheBootstrapAdoptsTheExistingSupportProject(t *testing.T) {
	harness := newSupportHarness(t)
	enableAssistant(t, harness.pool, map[string]any{
		"support_assistant_enabled": true,
		"support_agent_id":          supportAgentID,
	})
	harness.handler.store.provisioner = refusingProvisioner{}

	settings, err := harness.handler.store.settings(context.Background())
	if err != nil {
		t.Fatalf("resolve settings: %v", err)
	}
	if settings.ProjectID != supportTenant {
		t.Fatalf("project id = %d, want the adopted %d", settings.ProjectID, supportTenant)
	}

	var stored int64
	if err := harness.pool.QueryRow(context.Background(),
		`SELECT (value #>> '{}')::bigint FROM centry.platform_config
		 WHERE section = 'support_assistant' AND key = 'support_project_id'`).Scan(&stored); err != nil {
		t.Fatalf("the adopted id was not recorded: %v", err)
	}
	if stored != supportTenant {
		t.Fatalf("the recorded id is %d, want %d", stored, supportTenant)
	}
}

// A DEPLOYMENT WITH NO PROVISIONER REPORTS THE ASSISTANT OFF rather than
// erroring on every page load.
func TestNoProvisionerAndNoProjectReportsTheAssistantOff(t *testing.T) {
	harness := newSupportHarness(t)
	enableAssistant(t, harness.pool, map[string]any{
		"support_assistant_enabled": true,
		"support_agent_id":          supportAgentID,
	})
	// Rename the seeded project so the adopt-by-name branch cannot answer.
	if _, err := harness.pool.Exec(context.Background(),
		`UPDATE centry.project SET name = 'Something Else' WHERE id = $1`, supportTenant); err != nil {
		t.Fatal(err)
	}
	settings, err := harness.handler.store.settings(context.Background())
	if err != nil {
		t.Fatalf("resolve settings: %v", err)
	}
	if settings.Ready() {
		t.Fatalf("an unbootstrapped deployment reported itself ready: %+v", settings)
	}
}

// THE PROVISIONER MINTS THE PROJECT ONCE and its id is recorded, so the next
// replica reads the same row instead of provisioning a second one.
func TestTheProvisionerMintsTheProjectOnceAndTheIDIsRecorded(t *testing.T) {
	harness := newSupportHarness(t)
	enableAssistant(t, harness.pool, map[string]any{
		"support_assistant_enabled": true,
		"support_agent_id":          supportAgentID,
	})
	if _, err := harness.pool.Exec(context.Background(),
		`UPDATE centry.project SET name = 'Something Else' WHERE id = $1`, supportTenant); err != nil {
		t.Fatal(err)
	}
	provisioner := &countingProvisioner{pool: harness.pool, projectID: 900}
	harness.handler.store.provisioner = provisioner

	for range 2 {
		settings, err := harness.handler.store.settings(context.Background())
		if err != nil {
			t.Fatalf("resolve settings: %v", err)
		}
		if settings.ProjectID != 900 {
			t.Fatalf("project id = %d, want the minted 900", settings.ProjectID)
		}
	}
	if provisioner.calls != 1 {
		t.Fatalf("the project was provisioned %d times, want 1", provisioner.calls)
	}
	// The system user is created by the bootstrap, and is the project's owner.
	var owner int64
	if err := harness.pool.QueryRow(context.Background(),
		`SELECT id FROM auth_core__user WHERE email = $1`, SystemUserEmail).Scan(&owner); err != nil {
		t.Fatalf("the system user was not created: %v", err)
	}
	if provisioner.ownerID != owner {
		t.Fatalf("the project was provisioned for owner %d, want the system user %d",
			provisioner.ownerID, owner)
	}
}

// A FAILING PROVISIONER IS NOT RETRIED ON EVERY REQUEST. `GET /config` is
// ungated and the widget calls it on every page load, so an unbounded retry
// would queue every navigation behind a failing multi-second pipeline.
func TestAFailingBootstrapIsAttemptedOncePerCooldown(t *testing.T) {
	harness := newSupportHarness(t)
	enableAssistant(t, harness.pool, map[string]any{
		"support_assistant_enabled": true,
		"support_agent_id":          supportAgentID,
	})
	if _, err := harness.pool.Exec(context.Background(),
		`UPDATE centry.project SET name = 'Something Else' WHERE id = $1`, supportTenant); err != nil {
		t.Fatal(err)
	}
	provisioner := &failingProvisioner{}
	harness.handler.store.provisioner = provisioner

	for range 3 {
		if _, err := harness.handler.store.settings(context.Background()); err != nil {
			t.Fatalf("resolve settings: %v", err)
		}
	}
	if provisioner.calls != 1 {
		t.Fatalf("a failing bootstrap ran %d times, want 1 inside the cooldown", provisioner.calls)
	}
}

/* ── helpers ───────────────────────────────────────────────────────────── */

func mustConversationsRepo(t *testing.T, pool *pgxpool.Pool) *repos.ConversationsRepo {
	t.Helper()
	return repos.NewConversationsRepo(pool)
}

func (h *supportHarness) participants(t *testing.T, conversationUUID string) []conversations.Participant {
	t.Helper()
	var conversationID int64
	if err := h.pool.QueryRow(context.Background(),
		`SELECT id FROM p_7.chat_conversations WHERE uuid = $1::uuid`, conversationUUID).Scan(&conversationID); err != nil {
		t.Fatalf("resolve the conversation row: %v", err)
	}
	repository := mustConversationsRepo(t, h.pool)
	participants, err := repository.ListParticipants(context.Background(),
		strconv.Itoa(supportTenant), strconv.FormatInt(conversationID, 10))
	if err != nil {
		t.Fatalf("read the participants: %v", err)
	}
	return participants
}

// seedBrokenConversation writes the exact rows the shipped code left behind:
// a support conversation, an application participant, an EMPTY entity_settings
// mapping, and no author participant.
func (h *supportHarness) seedBrokenConversation(t *testing.T, userID int64) string {
	t.Helper()
	return h.seedBrokenConversationWithSettings(t, userID, `{}`)
}

func (h *supportHarness) seedBrokenConversationWithSettings(
	t *testing.T, userID int64, entitySettings string,
) string {
	t.Helper()
	var conversationUUID string
	if err := h.pool.QueryRow(context.Background(), `
WITH conversation AS (
    INSERT INTO p_7.chat_conversations (uuid, name, is_private, author_id, meta, source)
    VALUES (gen_random_uuid(), 'broken', TRUE, $1,
            '{"is_hidden":true,"conversation_type":"support","internal_tools":["internal_mcp"]}'::jsonb,
            'support')
    RETURNING id, uuid
), participant AS (
    INSERT INTO p_7.chat_participants (uuid, entity_name, entity_meta, meta)
    VALUES (gen_random_uuid(), 'application',
            jsonb_build_object('id', $2::int, 'project_id', $3::int), '{}'::json)
    RETURNING id
), mapping AS (
    INSERT INTO p_7.chat_participant_mapping (conversation_id, participant_id, entity_settings)
    SELECT conversation.id, participant.id, $4::jsonb FROM conversation, participant
    RETURNING conversation_id
)
SELECT conversation.uuid::text FROM conversation, mapping LIMIT 1`,
		userID, supportAgentID, supportTenant, entitySettings).Scan(&conversationUUID); err != nil {
		t.Fatalf("seed the broken conversation: %v", err)
	}
	return conversationUUID
}

func hasUserParticipant(participants []conversations.Participant, userID int64) bool {
	for _, participant := range participants {
		if participant.EntityName == userEntityName && metaInt(participant.EntityMeta, "id") == userID {
			return true
		}
	}
	return false
}

func findApplicationParticipant(
	participants []conversations.Participant, agentID int64,
) (conversations.Participant, bool) {
	for _, participant := range participants {
		if participant.EntityName == applicationEntityName && metaInt(participant.EntityMeta, "id") == agentID {
			return participant, true
		}
	}
	return conversations.Participant{}, false
}

type refusingProvisioner struct{}

func (refusingProvisioner) Provision(context.Context, ProvisionRequest) (int64, error) {
	return 0, fmt.Errorf("the provisioner must not be called when a project can be adopted")
}

type countingProvisioner struct {
	pool      *pgxpool.Pool
	projectID int64
	calls     int
	ownerID   int64
}

func (p *countingProvisioner) Provision(
	ctx context.Context, request ProvisionRequest,
) (int64, error) {
	p.calls++
	p.ownerID = request.OwnerID
	if _, err := p.pool.Exec(ctx,
		`INSERT INTO centry.project (id, name) VALUES ($1, $2)`, p.projectID, request.Name); err != nil {
		return 0, err
	}
	return p.projectID, nil
}

type failingProvisioner struct{ calls int }

func (p *failingProvisioner) Provision(context.Context, ProvisionRequest) (int64, error) {
	p.calls++
	return 0, fmt.Errorf("provisioning is unavailable")
}
