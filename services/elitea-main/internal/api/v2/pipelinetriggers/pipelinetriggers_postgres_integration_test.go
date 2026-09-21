package pipelinetriggers_test

// Acceptance for both unattended entry points, against a real PostgreSQL.
//
// What a unit test cannot reach and this does:
//
//   - a credential really is scoped to ONE pipeline in ONE project. The
//     refusals below are the ones issue 192 says are the whole risk, and each
//     is driven through the HTTP route with a real row behind it rather than
//     asserted about a helper;
//   - the run this admits is the run the chat composer would have admitted. The
//     dispatch request is compared FIELD BY FIELD against a chat-started one,
//     and the rows it wrote are read back through the joins
//     ResolveCurrentApplicationTurn performs — which is the difference between
//     "rows were written" and "a turn can be admitted against them";
//   - the schedule tick's four outcomes each leave the row in the state the
//     settings tab reads, and only a DISPATCH moves `last_run`.
//
// The start use case is faked. Admitting a turn for real needs the whole
// runtime plane — outbox, Redis stream, a worker — which is not what this file
// is about: the seam under test is everything on THIS side of
// `StartCurrentApplication`.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/pipelinetriggers"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const (
	homeProject  = "1"
	homeSchema   = "p_1"
	otherProject = "2"
	otherSchema  = "p_2"

	ownerUserID = int64(41)
)

// tenantMigrations are applied as the SHIPPED FILES, not as a copy of their
// DDL, so a change to one that these tests do not expect fails here rather
// than passing against a stale duplicate. In ledger order, which is the order
// a real deployment applies them in: 0138 ALTERs the table 0133 creates.
var tenantMigrations = []string{
	"tenant/0133_pipeline_triggers_and_schedules.sql",
	"tenant/0138_pipeline_trigger_auth_mode.sql",
}

/* ── doubles ───────────────────────────────────────────────────────────── */

// fakeStart records the admission request and answers with a canned outcome.
type fakeStart struct {
	mu       sync.Mutex
	requests []agentexecutionapp.CurrentApplicationStartRequest
	outcome  agentexecutionapp.CurrentApplicationStartOutcome
	err      error
}

func (s *fakeStart) StartCurrentApplication(
	_ context.Context, request agentexecutionapp.CurrentApplicationStartRequest,
) (agentexecutionapp.CurrentApplicationStartOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	return s.outcome, s.err
}

func (s *fakeStart) last() (agentexecutionapp.CurrentApplicationStartRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return agentexecutionapp.CurrentApplicationStartRequest{}, false
	}
	return s.requests[len(s.requests)-1], true
}

func (s *fakeStart) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// memoryVault is the hidden bucket, in a map.
type memoryVault struct {
	mu      sync.Mutex
	entries map[string]string
	fail    bool
}

func newMemoryVault() *memoryVault { return &memoryVault{entries: map[string]string{}} }

func (v *memoryVault) StoreAdminHiddenSecret(_ context.Context, name, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.fail {
		return fmt.Errorf("vault is closed")
	}
	v.entries[name] = value
	return nil
}

func (v *memoryVault) LookupAdminHiddenSecret(_ context.Context, name string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	value, ok := v.entries[name]
	if !ok {
		return "", fmt.Errorf("no such secret")
	}
	return value, nil
}

func (v *memoryVault) DeleteAdminHiddenSecret(_ context.Context, name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.entries, name)
	return nil
}

func (v *memoryVault) size() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.entries)
}

// fixedPermissions grants one permission to one user in one project.
type fixedPermissions struct {
	userID      int64
	projectID   string
	permissions []string
}

func (p fixedPermissions) ResolvePermissions(
	_ context.Context, principal auth.User, _ string, projectID string,
) (auth.PermissionResolution, error) {
	if principal.UserID != strconv.FormatInt(p.userID, 10) || projectID != p.projectID {
		return auth.PermissionResolution{}, nil
	}
	id, _ := strconv.ParseInt(principal.UserID, 10, 64)
	return auth.PermissionResolution{UserID: id, Permissions: p.permissions}, nil
}

// recordingRecorder keeps every audit event.
type recordingRecorder struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recordingRecorder) Record(_ context.Context, event audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordingRecorder) all() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]audit.Event(nil), r.events...)
}

/* ── harness ───────────────────────────────────────────────────────────── */

type harness struct {
	pool     *pgxpool.Pool
	start    *fakeStart
	vault    *memoryVault
	recorder *recordingRecorder
	handler  *pipelinetriggers.Handler
	router   chi.Router
}

// newHarness builds the handler with the run permission granted to ownerUserID
// in the HOME project only. That single fact is what most of the refusals
// below turn on.
func newHarness(t *testing.T) *harness {
	t.Helper()
	pool := newPool(t)
	start := &fakeStart{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID:       "execution-1",
		CommandID:         "command-1",
		ResponseMessageID: "11111111-1111-4111-8111-111111111111",
		Created:           true,
	}}
	vault := newMemoryVault()
	recorder := &recordingRecorder{}
	handler := pipelinetriggers.NewPlatformHandler(
		pool, start, vault,
		fixedPermissions{
			userID:      ownerUserID,
			projectID:   homeProject,
			permissions: []string{pipelinetriggers.RunPermission},
		},
		recorder, nil, nil, nil,
	)
	return &harness{pool: pool, start: start, vault: vault, recorder: recorder, handler: handler, router: mountRoutes(handler)}
}

// mountRoutes mounts every route this package serves.
//
// The SETTINGS routes are mounted WITHOUT the project permission gate the
// production router puts above them: those gates are pinned in
// internal/api/router_elitea_core_project_scope_test.go, and repeating them
// here would test the middleware twice and the handler not at all.
//
// The authenticated caller is injected the way the Auth middleware does it,
// because `created_by` and `author_id` come from the CONTEXT and never from a
// body, and a handler that read them from a body would pass a test that
// supplied one.
func mountRoutes(handler *pipelinetriggers.Handler) *chi.Mux {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := auth.User{
				ID:     strconv.FormatInt(ownerUserID, 10),
				UserID: strconv.FormatInt(ownerUserID, 10),
				Email:  "owner@example.com",
			}
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), user)))
		})
	})
	router.Get("/api/v2/pipeline_triggers/prompt_lib/{projectID}/{versionID}", handler.GetTrigger)
	router.Get("/api/v2/pipeline_triggers/secret/prompt_lib/{projectID}/{versionID}", handler.RevealTrigger)
	router.Post("/api/v2/pipeline_triggers/prompt_lib/{projectID}/{versionID}", handler.CreateOrRotateTrigger)
	router.Delete("/api/v2/pipeline_triggers/prompt_lib/{projectID}/{versionID}", handler.RevokeTrigger)
	router.Get("/api/v2/pipeline_schedules/prompt_lib/{projectID}/{versionID}", handler.GetSchedule)
	router.Put("/api/v2/pipeline_schedules/prompt_lib/{projectID}/{versionID}", handler.SaveSchedule)
	router.Delete("/api/v2/pipeline_schedules/prompt_lib/{projectID}/{versionID}", handler.DeleteSchedule)
	router.Post(pipelinetriggers.InboundPath, handler.Trigger)
	// Both inbound registrations, exactly as internal/api/router.go makes
	// them: the provider-suffixed url is the ONE a preset hands a sender, so a
	// harness without it would test a url nobody is given (#970).
	router.Post(pipelinetriggers.InboundProviderPath, handler.Trigger)
	return router
}

// newHarnessWithoutRunner is the composition a deployment with
// `runtime.enabled` off gets: every dependency except the execution use case,
// which arrives as an untyped nil (see TestSettingsWorkWithoutTheExecutionRuntime).
func newHarnessWithoutRunner(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.handler = pipelinetriggers.NewPlatformHandler(
		h.pool, nil, h.vault,
		fixedPermissions{
			userID:      ownerUserID,
			projectID:   homeProject,
			permissions: []string{pipelinetriggers.RunPermission},
		},
		h.recorder, nil, nil, nil,
	)
	h.router = mountRoutes(h.handler)
	return h
}

func (h *harness) do(t *testing.T, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)
	return recorder
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return body
}

// mintTrigger creates a pipeline version and its trigger, and returns the
// version id and the secret the caller was shown once.
func (h *harness) mintTrigger(t *testing.T, project, schema, name string, owner int64) (int64, string, string) {
	t.Helper()
	versionID := seedPipeline(t, h.pool, schema, name, owner)
	response := h.do(t, http.MethodPost,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", project, versionID), "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("mint trigger: status = %d, body = %s", response.Code, response.Body.String())
	}
	body := decode(t, response)
	secret, _ := body["secret"].(string)
	tokenID, _ := body["token_id"].(string)
	if secret == "" || tokenID == "" {
		t.Fatalf("mint trigger returned no credential: %v", body)
	}
	return versionID, tokenID, secret
}

func inboundTarget(project, tokenID string) string {
	return "/api/v2/pipeline_trigger/" + project + "/" + tokenID
}

/* ── the inbound trigger ───────────────────────────────────────────────── */

// TestInboundTriggerAdmitsTheSameRunAChatStartWouldHave is the claim the whole
// package rests on: an unattended run is not a second execution path.
func TestInboundTriggerAdmitsTheSameRunAChatStartWouldHave(t *testing.T) {
	h := newHarness(t)
	versionID, tokenID, secret := h.mintTrigger(t, homeProject, homeSchema, "Nightly report", ownerUserID)

	response := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID),
		`{"input":"run it"}`, map[string]string{"Authorization": "Bearer " + secret})
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", response.Code, response.Body.String())
	}
	body := decode(t, response)
	if body["execution_id"] != "execution-1" {
		t.Fatalf("execution_id = %v, want the admitted execution", body["execution_id"])
	}
	if body["events_url"] != "/api/v2/executions/1/execution-1/events" {
		t.Fatalf("events_url = %v — issue 192 asks the answer to name the stream the caller follows",
			body["events_url"])
	}

	request, ok := h.start.last()
	if !ok {
		t.Fatal("nothing was dispatched")
	}
	// FIELD BY FIELD against what the chat start route builds
	// (internal/api/v2/agentexecution/route.go): same project, same actor, a
	// conversation uuid, a participant id, a question uuid, and the caller's
	// text. Nothing extra and nothing missing.
	if request.ProjectID != 1 {
		t.Fatalf("ProjectID = %d, want 1", request.ProjectID)
	}
	if request.ActorUserID != ownerUserID {
		t.Fatalf("ActorUserID = %d, want the trigger's creator %d — the run identity must come "+
			"from the stored row", request.ActorUserID, ownerUserID)
	}
	if request.UserInput != "run it" {
		t.Fatalf("UserInput = %q, want the body's input", request.UserInput)
	}
	if request.ConversationUUID == "" || request.QuestionID == "" || request.TargetParticipantID <= 0 {
		t.Fatalf("incomplete dispatch: %+v", request)
	}
	if len(request.Attachments) != 0 {
		t.Fatalf("an unattended run carries no attachments, got %d", len(request.Attachments))
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("the dispatch the use case would refuse: %v", err)
	}

	// The rows are ones the RUNTIME RESOLVER can use: the agent participant is
	// mapped to this conversation and its mapping carries the version. This is
	// the join ResolveCurrentApplicationTurn performs.
	var mappedVersion int64
	err := h.pool.QueryRow(context.Background(), fmt.Sprintf(`
SELECT (mapping.entity_settings ->> 'version_id')::bigint
  FROM %[1]s.chat_participant_mapping AS mapping
  JOIN %[1]s.chat_conversations AS conversation ON conversation.id = mapping.conversation_id
 WHERE conversation.uuid = $1::uuid AND mapping.participant_id = $2`, homeSchema),
		request.ConversationUUID, request.TargetParticipantID).Scan(&mappedVersion)
	if err != nil {
		t.Fatalf("the admitted turn's participant mapping is not resolvable: %v", err)
	}
	if mappedVersion != versionID {
		t.Fatalf("mapped version = %d, want %d — the run must be pinned to the trigger's version",
			mappedVersion, versionID)
	}

	// The transcript is findable: source marks it, and the conversation is not
	// hidden. A run nobody watched must still be openable by the pipeline's
	// owner.
	var source string
	if err := h.pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT source FROM %s.chat_conversations WHERE uuid = $1::uuid`, homeSchema),
		request.ConversationUUID).Scan(&source); err != nil {
		t.Fatalf("read the run conversation: %v", err)
	}
	if source != pipelinetriggers.TriggerConversationSource {
		t.Fatalf("conversation source = %q, want %q", source, pipelinetriggers.TriggerConversationSource)
	}
}

// TestInboundTriggerRefusesEveryUnusableCredential is issue 192's own risk
// list, and the #11 class it names. Every row must be 401 with the SAME body,
// and none may dispatch.
func TestInboundTriggerRefusesEveryUnusableCredential(t *testing.T) {
	h := newHarness(t)
	versionID, tokenID, secret := h.mintTrigger(t, homeProject, homeSchema, "Nightly report", ownerUserID)
	_ = versionID
	// A SECOND pipeline in the same project, and one in another project. Both
	// exist so a credential can be tried against the wrong target rather than
	// against nothing.
	_, otherPipelineToken, otherPipelineSecret := h.mintTrigger(
		t, homeProject, homeSchema, "Weekly report", ownerUserID)
	otherProjectVersion := seedPipeline(t, h.pool, otherSchema, "Foreign pipeline", ownerUserID)
	_ = otherProjectVersion

	baseline := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), "",
		map[string]string{"Authorization": "Bearer " + secret})
	if baseline.Code != http.StatusAccepted {
		t.Fatalf("the control row must succeed first: status = %d, body = %s",
			baseline.Code, baseline.Body.String())
	}
	dispatchedBefore := h.start.count()

	// Revoke the FIRST trigger so the revoked row can be exercised.
	revoke := h.do(t, http.MethodDelete,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID), "", nil)
	if revoke.Code != http.StatusOK {
		t.Fatalf("revoke: status = %d, body = %s", revoke.Code, revoke.Body.String())
	}

	var refusalBody string
	for _, test := range []struct {
		name    string
		project string
		token   string
		secret  string
		header  string
	}{
		{name: "revoked trigger", project: homeProject, token: tokenID, secret: secret},
		{name: "wrong secret", project: homeProject, token: otherPipelineToken, secret: secret},
		{name: "no credential at all", project: homeProject, token: otherPipelineToken, secret: ""},
		{
			// The token of ANOTHER PIPELINE presented against this one's id.
			// The id selects the row and the secret must match THAT row.
			name: "another pipeline's secret", project: homeProject,
			token: tokenID, secret: otherPipelineSecret,
		},
		{
			// Rule 1: the project id says WHERE TO LOOK and nothing more. A
			// live credential aimed at another project finds no row.
			name: "a live credential aimed at another project", project: otherProject,
			token: otherPipelineToken, secret: otherPipelineSecret,
		},
		{name: "unknown token id", project: homeProject, token: "deadbeef", secret: secret},
		{name: "malformed project id", project: "not-a-project", token: otherPipelineToken, secret: otherPipelineSecret},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := map[string]string{}
			if test.secret != "" {
				headers["Authorization"] = "Bearer " + test.secret
			}
			response := h.do(t, http.MethodPost, inboundTarget(test.project, test.token), "", headers)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", response.Code, response.Body.String())
			}
			if refusalBody == "" {
				refusalBody = response.Body.String()
			} else if response.Body.String() != refusalBody {
				t.Fatalf("this refusal differs from the others:\n got %s\nwant %s\n"+
					"A refusal that names its cause is an oracle for enumerating a deployment's pipelines.",
					response.Body.String(), refusalBody)
			}
		})
	}
	if h.start.count() != dispatchedBefore {
		t.Fatalf("a refused call dispatched a run: %d dispatches after the control row's %d",
			h.start.count(), dispatchedBefore)
	}
}

// TestInboundTriggerRefusesWhenTheCreatorLostThePermission is rule 4: a
// credential must not outlive its holder's access.
func TestInboundTriggerRefusesWhenTheCreatorLostThePermission(t *testing.T) {
	h := newHarness(t)
	_, tokenID, secret := h.mintTrigger(t, homeProject, homeSchema, "Nightly report", ownerUserID)

	// A second handler over the same rows, whose resolver grants the creator
	// nothing. Nothing about the STORED trigger changes: it is still there, it
	// is not revoked, and the secret is still correct.
	stripped := pipelinetriggers.NewPlatformHandler(
		h.pool, h.start, h.vault,
		fixedPermissions{userID: ownerUserID, projectID: homeProject, permissions: nil},
		h.recorder, nil, nil, nil,
	)
	router := chi.NewRouter()
	router.Post(pipelinetriggers.InboundPath, stripped.Trigger)

	before := h.start.count()
	request := httptest.NewRequest(http.MethodPost, inboundTarget(homeProject, tokenID), strings.NewReader(""))
	request.Header.Set("Authorization", "Bearer "+secret)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — a token must not outlive its creator's access; body = %s",
			response.Code, response.Body.String())
	}
	if h.start.count() != before {
		t.Fatal("a run was dispatched for a creator who holds no run permission")
	}
}

// TestInboundTriggerAcceptsTheThreeCredentialCarriers pins the wire contract a
// webhook sender depends on.
func TestInboundTriggerAcceptsTheThreeCredentialCarriers(t *testing.T) {
	h := newHarness(t)
	_, tokenID, secret := h.mintTrigger(t, homeProject, homeSchema, "Nightly report", ownerUserID)

	for _, test := range []struct {
		name   string
		target string
		header map[string]string
	}{
		{
			name:   "Authorization bearer",
			target: inboundTarget(homeProject, tokenID),
			header: map[string]string{"Authorization": "Bearer " + secret},
		},
		{
			name:   "the dedicated header",
			target: inboundTarget(homeProject, tokenID),
			header: map[string]string{pipelinetriggers.TriggerTokenHeader: secret},
		},
		{
			name: "the query parameter, for senders that cannot set a header",
			target: inboundTarget(homeProject, tokenID) + "?" +
				pipelinetriggers.TriggerTokenQueryParam + "=" + secret,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := h.do(t, http.MethodPost, test.target, "", test.header)
			if response.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202; body = %s", response.Code, response.Body.String())
			}
		})
	}
}

// TestInboundTriggerWritesAnAuditRowForEveryOutcome. The audit middleware
// cannot see this route — it is mounted above the Auth group — so the row this
// package writes itself is the ONLY record that an inbound call happened.
func TestInboundTriggerWritesAnAuditRowForEveryOutcome(t *testing.T) {
	h := newHarness(t)
	_, tokenID, secret := h.mintTrigger(t, homeProject, homeSchema, "Nightly report", ownerUserID)

	h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), "",
		map[string]string{"Authorization": "Bearer " + secret})
	h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), "",
		map[string]string{"Authorization": "Bearer wrong"})

	var accepted, refused int
	for _, event := range h.recorder.all() {
		if event.HTTPRoute != pipelinetriggers.InboundPath {
			continue
		}
		if event.StatusCode == nil {
			t.Fatal("an audit row with no status code cannot be filtered by outcome")
		}
		if event.DurationMS == nil {
			t.Fatal("an audit row with no duration belongs to no band on the trail's heatmap")
		}
		// The route PATTERN, never the raw target: the target may carry the
		// secret in its query string, and a credential written into a table the
		// admin page renders would outlive every rotation.
		if strings.Contains(event.Action, secret) || strings.Contains(event.HTTPRoute, secret) {
			t.Fatal("the audit row carries the trigger secret")
		}
		switch *event.StatusCode {
		case int32(http.StatusAccepted):
			accepted++
			if event.IsError {
				t.Fatal("an admitted run is recorded as an error")
			}
		case int32(http.StatusUnauthorized):
			refused++
			if !event.IsError {
				t.Fatal("a refusal is not recorded as an error")
			}
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("audit rows: accepted = %d, refused = %d, want 1 and 1 — a trail that holds only "+
			"the successful calls cannot answer 'is someone trying my webhook URLs?'", accepted, refused)
	}
}

/* ── the settings routes ───────────────────────────────────────────────── */

// TestRotationInvalidatesThePreviousSecretImmediately.
func TestRotationInvalidatesThePreviousSecretImmediately(t *testing.T) {
	h := newHarness(t)
	versionID, tokenID, secret := h.mintTrigger(t, homeProject, homeSchema, "Nightly report", ownerUserID)

	rotate := h.do(t, http.MethodPost,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID), "", nil)
	if rotate.Code != http.StatusOK {
		t.Fatalf("rotate: status = %d, body = %s", rotate.Code, rotate.Body.String())
	}
	rotated := decode(t, rotate)
	if rotated["secret"] == secret {
		t.Fatal("rotation reissued the same secret")
	}
	if rotated["token_id"] == tokenID {
		t.Fatal("rotation kept the same token id, so the old URL still names the new credential")
	}
	if rotated["rotated_at"] == nil {
		t.Fatal("rotation did not stamp rotated_at")
	}

	stale := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), "",
		map[string]string{"Authorization": "Bearer " + secret})
	if stale.Code != http.StatusUnauthorized {
		t.Fatalf("the superseded credential still works: status = %d", stale.Code)
	}
	fresh := h.do(t, http.MethodPost, inboundTarget(homeProject, rotated["token_id"].(string)), "",
		map[string]string{"Authorization": "Bearer " + rotated["secret"].(string)})
	if fresh.Code != http.StatusAccepted {
		t.Fatalf("the rotated credential does not work: status = %d, body = %s",
			fresh.Code, fresh.Body.String())
	}
	// Exactly one entry survives: the superseded one is removed, and it is
	// removed AFTER the new row commits, never before.
	if h.vault.size() != 1 {
		t.Fatalf("vault holds %d entries after one rotation, want 1", h.vault.size())
	}
}

// TestTheReadDoesNotCarryTheSecretAndTheRevealDoes is the separation the two
// permissions exist for.
func TestTheReadDoesNotCarryTheSecretAndTheRevealDoes(t *testing.T) {
	h := newHarness(t)
	versionID, _, secret := h.mintTrigger(t, homeProject, homeSchema, "Nightly report", ownerUserID)

	read := decode(t, h.do(t, http.MethodGet,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID), "", nil))
	if _, present := read["secret"]; present {
		t.Fatal("the plain read carries the credential; a view-only member could copy a working URL")
	}
	if _, present := read["secret_url"]; present {
		t.Fatal("the plain read carries the credential-bearing URL")
	}
	if read["url"] == nil || strings.Contains(read["url"].(string), secret) {
		t.Fatalf("url = %v, want the inbound path without the secret", read["url"])
	}

	revealed := decode(t, h.do(t, http.MethodGet,
		fmt.Sprintf("/api/v2/pipeline_triggers/secret/prompt_lib/%s/%d", homeProject, versionID), "", nil))
	if revealed["secret"] != secret {
		t.Fatalf("reveal returned %v, want the stored credential", revealed["secret"])
	}
	if !strings.Contains(revealed["secret_url"].(string), secret) {
		t.Fatal("reveal returned a secret_url with no secret in it")
	}
}

// TestAPipelineWithNoTriggerAnswers200 — "no trigger yet" is the normal state
// of almost every pipeline and must not render as an error.
func TestAPipelineWithNoTriggerAnswers200(t *testing.T) {
	h := newHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Untriggered", ownerUserID)

	trigger := h.do(t, http.MethodGet,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID), "", nil)
	if trigger.Code != http.StatusOK || decode(t, trigger)["configured"] != false {
		t.Fatalf("trigger read: status = %d, body = %s", trigger.Code, trigger.Body.String())
	}
	schedule := h.do(t, http.MethodGet,
		fmt.Sprintf("/api/v2/pipeline_schedules/prompt_lib/%s/%d", homeProject, versionID), "", nil)
	if schedule.Code != http.StatusOK || decode(t, schedule)["configured"] != false {
		t.Fatalf("schedule read: status = %d, body = %s", schedule.Code, schedule.Body.String())
	}
}

// TestSettingsWorkWithoutTheExecutionRuntime is #899's Go half.
//
// With `runtime.enabled` off the composition root used to build no handler at
// all, so `/pipeline_triggers` and `/pipeline_schedules` were never mounted and
// the editor's whole Schedule/Webhook surface 404ed — on the E2E stack among
// others. CONFIGURING an entry point is a table and a credential; only STARTING
// a run needs the runner. The handler is therefore built either way, with a nil
// AgentStartUseCase, and this pins both halves of that: the settings routes
// work, and the inbound POST degrades honestly with a 503 rather than a 202 it
// cannot keep or a nil dereference.
func TestSettingsWorkWithoutTheExecutionRuntime(t *testing.T) {
	h := newHarnessWithoutRunner(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Runtime-less", ownerUserID)

	created := h.do(t, http.MethodPost,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID), "", nil)
	if created.Code != http.StatusOK {
		t.Fatalf("create trigger: status = %d, body = %s", created.Code, created.Body.String())
	}
	body := decode(t, created)
	secret, _ := body["secret"].(string)
	tokenID, _ := body["token_id"].(string)
	if secret == "" || tokenID == "" {
		t.Fatalf("create trigger answered without a credential: %s", created.Body.String())
	}

	saved := h.do(t, http.MethodPut,
		fmt.Sprintf("/api/v2/pipeline_schedules/prompt_lib/%s/%d", homeProject, versionID),
		`{"cron":"0 9 * * 1","active":true}`, nil)
	if saved.Code != http.StatusOK || decode(t, saved)["configured"] != true {
		t.Fatalf("save schedule: status = %d, body = %s", saved.Code, saved.Body.String())
	}

	// The one thing that genuinely cannot work says so, and says it with the
	// status a caller can act on.
	fired := h.do(t, http.MethodPost,
		fmt.Sprintf("/api/v2/pipeline_trigger/%s/%s", homeProject, tokenID), "",
		map[string]string{"Authorization": "Bearer " + secret})
	if fired.Code != http.StatusServiceUnavailable {
		t.Fatalf("inbound trigger without a runner: status = %d, want 503; body = %s", fired.Code, fired.Body.String())
	}
}

// TestScheduleWriteRefusesAnExpressionTheRunnerCannotParse. An accepted
// unparseable cron would not error at run time — it would silently never fire.
func TestScheduleWriteRefusesAnExpressionTheRunnerCannotParse(t *testing.T) {
	h := newHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Nightly report", ownerUserID)
	target := fmt.Sprintf("/api/v2/pipeline_schedules/prompt_lib/%s/%d", homeProject, versionID)

	for _, expression := range []string{"", "@daily", "* * *", "not a cron", "99 * * * *"} {
		body, _ := json.Marshal(map[string]any{"cron": expression, "active": true})
		response := h.do(t, http.MethodPut, target, string(body), nil)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("cron %q: status = %d, want 400; body = %s",
				expression, response.Code, response.Body.String())
		}
	}

	body, _ := json.Marshal(map[string]any{"cron": "0 3 * * *", "active": true, "input": "go"})
	response := h.do(t, http.MethodPut, target, string(body), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("a five-field expression: status = %d, body = %s", response.Code, response.Body.String())
	}
	saved := decode(t, response)
	if saved["next_run"] == nil {
		t.Fatal("no next_run preview — a cron expression does not tell a person when it fires")
	}
	if saved["author_id"] != float64(ownerUserID) {
		t.Fatalf("author_id = %v, want the authenticated caller %d", saved["author_id"], ownerUserID)
	}
}

/* ── the schedule tick ─────────────────────────────────────────────────── */

func (h *harness) saveSchedule(t *testing.T, versionID int64, cron string, active bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"cron": cron, "active": active, "input": "scheduled"})
	response := h.do(t, http.MethodPut,
		fmt.Sprintf("/api/v2/pipeline_schedules/prompt_lib/%s/%d", homeProject, versionID),
		string(body), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("save schedule: status = %d, body = %s", response.Code, response.Body.String())
	}
}

func (h *harness) readSchedule(t *testing.T, versionID int64) map[string]any {
	t.Helper()
	return decode(t, h.do(t, http.MethodGet,
		fmt.Sprintf("/api/v2/pipeline_schedules/prompt_lib/%s/%d", homeProject, versionID), "", nil))
}

// TestScheduleTickDispatchesAndStampsOnlyOnDispatch covers three of the four
// outcomes in one pass, because they are the same claim from three sides: only
// a real dispatch may move `last_run`.
func TestScheduleTickDispatchesAndStampsOnlyOnDispatch(t *testing.T) {
	h := newHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Nightly report", ownerUserID)
	h.saveSchedule(t, versionID, "* * * * *", true)

	// FIVE SECONDS PAST A MINUTE BOUNDARY, not `time.Now()`.
	//
	// The "not due" step below asks one second later, and `next(last_run)` for
	// a per-minute cron is the NEXT minute boundary. A run started at
	// HH:MM:59.5 would therefore be due again one second later, and this test
	// would fail about once every sixty runs — on the clock, not on the code.
	now := time.Now().UTC().Truncate(time.Minute).Add(5 * time.Second)
	result, err := h.handler.RunDueSchedules(context.Background(), now)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if result.Dispatched != 1 {
		t.Fatalf("dispatched = %d, want 1 (%+v)", result.Dispatched, result)
	}
	request, ok := h.start.last()
	if !ok {
		t.Fatal("the tick dispatched nothing")
	}
	if request.ActorUserID != ownerUserID {
		t.Fatalf("ActorUserID = %d, want the schedule's author %d", request.ActorUserID, ownerUserID)
	}
	if request.UserInput != "scheduled" {
		t.Fatalf("UserInput = %q, want the schedule's stored input", request.UserInput)
	}
	after := h.readSchedule(t, versionID)
	if after["last_result"] != "dispatched" {
		t.Fatalf("last_result = %v, want dispatched", after["last_result"])
	}
	if after["last_run"] == nil {
		t.Fatal("a dispatch did not stamp last_run")
	}
	if after["last_execution_id"] != "execution-1" {
		t.Fatalf("last_execution_id = %v — a tenant must be able to find the run", after["last_execution_id"])
	}
	stamped := after["last_run"]

	// NOT DUE. `next(last_run) <= now` is false one second later, so nothing
	// happens and nothing is written.
	second, err := h.handler.RunDueSchedules(context.Background(), now.Add(time.Second))
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if second.Dispatched != 0 || second.Considered != 0 {
		t.Fatalf("a schedule that is not due was considered: %+v", second)
	}
	if h.readSchedule(t, versionID)["last_run"] != stamped {
		t.Fatal("a tick that dispatched nothing moved last_run")
	}

	// NO CATCH-UP STORM. Six hours later the row is due exactly ONCE, not once
	// per missed minute.
	third, err := h.handler.RunDueSchedules(context.Background(), now.Add(6*time.Hour))
	if err != nil {
		t.Fatalf("third tick: %v", err)
	}
	if third.Dispatched != 1 {
		t.Fatalf("dispatched = %d after a six-hour gap, want exactly 1 — a catch-up storm would land "+
			"the whole backlog on a platform that has just come back", third.Dispatched)
	}
}

// TestMaintenanceSuppressesDispatchAndStampsNothing. This is the rule that
// makes a maintenance window recoverable.
func TestMaintenanceSuppressesDispatchAndStampsNothing(t *testing.T) {
	h := newHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Nightly report", ownerUserID)
	h.saveSchedule(t, versionID, "* * * * *", true)
	setMaintenance(t, h.pool, true)

	now := time.Now().UTC().Truncate(time.Minute).Add(5 * time.Second)
	result, err := h.handler.RunDueSchedules(context.Background(), now)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !result.Suppressed || result.Dispatched != 0 {
		t.Fatalf("maintenance did not suppress the pass: %+v", result)
	}
	if h.start.count() != 0 {
		t.Fatal("a run started while the platform was closed")
	}
	during := h.readSchedule(t, versionID)
	if during["last_run"] != nil {
		t.Fatal("a suppressed tick stamped last_run — the schedule's slot was silently consumed")
	}
	if during["last_result"] != nil {
		t.Fatalf("a suppressed tick wrote last_result = %v; the row's own state did not change",
			during["last_result"])
	}

	// The window closes and the schedule runs ONCE, promptly.
	setMaintenance(t, h.pool, false)
	after, err := h.handler.RunDueSchedules(context.Background(), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("tick after the window: %v", err)
	}
	if after.Dispatched != 1 {
		t.Fatalf("dispatched = %d after the window closed, want 1", after.Dispatched)
	}
}

// TestOverlapIsSkippedAndReported. A pipeline slower than its own cron must not
// build a backlog behind a schedule nobody is watching.
func TestOverlapIsSkippedAndReported(t *testing.T) {
	h := newHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Nightly report", ownerUserID)
	h.saveSchedule(t, versionID, "* * * * *", true)

	now := time.Now().UTC().Truncate(time.Minute).Add(5 * time.Second)
	if _, err := h.handler.RunDueSchedules(context.Background(), now); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	request, _ := h.start.last()
	// The runtime's own projection: a response group that is still streaming is
	// what "the previous run has not finished" means.
	seedStreamingResponse(t, h.pool, homeSchema, request.ConversationUUID)
	before := h.start.count()
	firstRun := h.readSchedule(t, versionID)["last_run"]

	result, err := h.handler.RunDueSchedules(context.Background(), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if result.Dispatched != 0 || result.Skipped != 1 {
		t.Fatalf("an overlapping run was started: %+v", result)
	}
	if h.start.count() != before {
		t.Fatal("a second run was dispatched while the first was still streaming")
	}
	after := h.readSchedule(t, versionID)
	if after["last_result"] != "skipped_overlap" {
		t.Fatalf("last_result = %v, want skipped_overlap — a tenant must be able to see why "+
			"nothing ran", after["last_result"])
	}
	if after["last_run"] != firstRun {
		t.Fatal("a skipped tick moved last_run")
	}
	if after["last_result_detail"] == nil {
		t.Fatal("no reason was recorded for the skip")
	}
}

// TestAnAuthorWhoLostAccessStopsTheSchedule is the schedule half of rule 4, and
// the answer to "who does a scheduled run execute as".
func TestAnAuthorWhoLostAccessStopsTheSchedule(t *testing.T) {
	h := newHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Nightly report", ownerUserID)
	h.saveSchedule(t, versionID, "* * * * *", true)

	stripped := pipelinetriggers.NewPlatformHandler(
		h.pool, h.start, h.vault,
		fixedPermissions{userID: ownerUserID, projectID: homeProject, permissions: nil},
		h.recorder, nil, nil, nil,
	)
	result, err := stripped.RunDueSchedules(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if result.Dispatched != 0 || result.Skipped != 1 {
		t.Fatalf("a schedule fired for an author with no run permission: %+v", result)
	}
	if h.start.count() != 0 {
		t.Fatal("a run was dispatched for an author with no run permission")
	}
	after := h.readSchedule(t, versionID)
	if after["last_result"] != "skipped_unauthorized" {
		t.Fatalf("last_result = %v, want skipped_unauthorized", after["last_result"])
	}
	if after["last_run"] != nil {
		t.Fatal("a skipped tick stamped last_run")
	}
	// The operator-facing half. `schedule` is the event type the admin Audit
	// Trail files on its SYSTEM tab, which is where an unattended run belongs.
	var found bool
	for _, event := range h.recorder.all() {
		if event.EventType == "schedule" && strings.Contains(event.Action, "skipped_unauthorized") {
			found = true
		}
	}
	if !found {
		t.Fatal("no audit event for a schedule that stopped firing")
	}
}

// TestAnInactiveScheduleNeverFires.
func TestAnInactiveScheduleNeverFires(t *testing.T) {
	h := newHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Nightly report", ownerUserID)
	h.saveSchedule(t, versionID, "* * * * *", false)

	result, err := h.handler.RunDueSchedules(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if result.Considered != 0 || result.Dispatched != 0 {
		t.Fatalf("a disabled schedule was considered: %+v", result)
	}
}

/* ── database bootstrap ────────────────────────────────────────────────── */

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_pt_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	initial, err := os.ReadFile(filepath.Join("..", "..", "..", "infra", "db", "migrations", "001_initial.sql"))
	if err != nil {
		t.Fatalf("read 001_initial.sql: %v", err)
	}
	if _, err := pool.Exec(ctx, string(initial)); err != nil {
		t.Fatalf("apply 001_initial.sql: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT create_tenant_schema($1)`, otherSchema); err != nil {
		t.Fatalf("create second project schema: %v", err)
	}

	// The SHIPPED migration file, applied with the search_path the real runner
	// pins (internal/infra/db/migrate/runner.go), because its table names are
	// unqualified.
	for _, name := range tenantMigrations {
		migration, err := platformmigrations.Files.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, schema := range []string{homeSchema, otherSchema} {
			if _, err := pool.Exec(ctx,
				fmt.Sprintf("SET search_path TO %s; %s", pgx.Identifier{schema}.Sanitize(), string(migration)),
			); err != nil {
				t.Fatalf("apply %s to %s: %v", name, schema, err)
			}
		}
	}
	return pool
}

// seedPipeline writes one application and one PIPELINE version, and returns the
// version id. The `agent_type` matters: both entry points refuse a version that
// is not a pipeline.
func seedPipeline(t *testing.T, pool *pgxpool.Pool, schema, name string, owner int64) int64 {
	t.Helper()
	ctx := context.Background()
	var applicationID int64
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`INSERT INTO %s.applications (name, description, owner_id) VALUES ($1, '', $2) RETURNING id`, schema),
		name, owner).Scan(&applicationID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	var versionID int64
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.application_versions (application_id, name, status, author_id, agent_type, llm_settings)
VALUES ($1, 'latest', 'published', $2, 'pipeline', '{}'::jsonb)
RETURNING id`, schema), applicationID, owner).Scan(&versionID); err != nil {
		t.Fatalf("seed pipeline version: %v", err)
	}
	return versionID
}

// seedStreamingResponse writes what an UNFINISHED run leaves in the projection.
func seedStreamingResponse(t *testing.T, pool *pgxpool.Pool, schema, conversationUUID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(`
INSERT INTO %[1]s.chat_message_group (uuid, conversation_id, author_participant_id, is_streaming, meta)
SELECT gen_random_uuid(), conversation.id, mapping.participant_id, TRUE, '{}'::jsonb
  FROM %[1]s.chat_conversations AS conversation
  JOIN %[1]s.chat_participant_mapping AS mapping ON mapping.conversation_id = conversation.id
 WHERE conversation.uuid = $1::uuid
 LIMIT 1`, schema), conversationUUID); err != nil {
		t.Fatalf("seed a streaming response group: %v", err)
	}
}

// setMaintenance writes the switch the admin Configuration page writes.
func setMaintenance(t *testing.T, pool *pgxpool.Pool, enabled bool) {
	t.Helper()
	value := "false"
	if enabled {
		value = "true"
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO centry.platform_config (section, key, value)
VALUES ('maintenance', 'maintenance_enabled', $1::jsonb)
ON CONFLICT (section, key) DO UPDATE SET value = EXCLUDED.value`, value); err != nil {
		t.Fatalf("write the maintenance switch: %v", err)
	}
}

/* ── #970: the provider signature mode ─────────────────────────────────── */

// mintSignedTrigger creates a pipeline and a GitHub-preset trigger, and
// returns the version id, the token id, the secret shown once, and the url the
// settings route handed out — which is the ONE url a sender is given.
func (h *harness) mintSignedTrigger(
	t *testing.T, project, schema, name string, owner int64, body string,
) (int64, string, string, string) {
	t.Helper()
	versionID := seedPipeline(t, h.pool, schema, name, owner)
	response := h.do(t, http.MethodPost,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", project, versionID), body, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("mint signed trigger: status = %d, body = %s", response.Code, response.Body.String())
	}
	decoded := decode(t, response)
	secret, _ := decoded["secret"].(string)
	tokenID, _ := decoded["token_id"].(string)
	url, _ := decoded["url"].(string)
	if secret == "" || tokenID == "" || url == "" {
		t.Fatalf("mint signed trigger returned no usable trigger: %v", decoded)
	}
	return versionID, tokenID, secret, url
}

// githubSignature is what a GitHub sender puts in `X-Hub-Signature-256`.
func githubSignature(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestGitHubTriggerAcceptsItsOwnSignature is #970's acceptance: the call a
// GitHub repository webhook really makes — no `Authorization` header, a signed
// raw body, and the `/github` url this service handed out — starts the run.
func TestGitHubTriggerAcceptsItsOwnSignature(t *testing.T) {
	h := newHarness(t)
	versionID, _, secret, url := h.mintSignedTrigger(
		t, homeProject, homeSchema, "Repository webhook", ownerUserID, `{"type":"github"}`)

	if !strings.HasSuffix(url, "/github") {
		t.Fatalf("url = %q, want the /github suffix a GitHub webhook form is configured with", url)
	}

	// A real push payload: valid JSON with no `input` key at all. The run must
	// be admitted on an EMPTY input rather than refused for the shape of a
	// sender's own payload.
	body := `{"ref":"refs/heads/main","commits":[],"repository":{"full_name":"acme/widgets"}}`
	response := h.do(t, http.MethodPost, url, body,
		map[string]string{pipelinetriggers.GitHubSignatureHeader: githubSignature(secret, body)})
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", response.Code, response.Body.String())
	}
	if h.start.count() != 1 {
		t.Fatalf("dispatches = %d, want 1", h.start.count())
	}
	request, _ := h.start.last()
	if request.UserInput != "" {
		t.Fatalf("UserInput = %q, want empty — a push payload carries no `input`", request.UserInput)
	}

	// The stored row says what the settings tab renders, and the plain read
	// carries it: which header the sender must be configured with.
	read := h.do(t, http.MethodGet,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID), "", nil)
	stored := decode(t, read)
	if stored["auth_mode"] != pipelinetriggers.AuthModeHMACSHA256 {
		t.Fatalf("auth_mode = %v, want %q", stored["auth_mode"], pipelinetriggers.AuthModeHMACSHA256)
	}
	if stored["signature_header"] != pipelinetriggers.GitHubSignatureHeader {
		t.Fatalf("signature_header = %v, want %q", stored["signature_header"], pipelinetriggers.GitHubSignatureHeader)
	}
	if stored["provider"] != pipelinetriggers.ProviderGitHub {
		t.Fatalf("provider = %v, want %q", stored["provider"], pipelinetriggers.ProviderGitHub)
	}
	// A signing trigger gets no `secret_url`: the query carrier is a way to
	// present the BEARER secret, which this trigger does not accept.
	if _, present := stored["secret_url"]; present {
		t.Fatalf("a signing trigger answered a secret_url: %v", stored)
	}
}

// TestGitHubTriggerRefusesEverySignatureThatIsNotItsOwn is the other half, and
// the one that decides whether the mode is worth having. Each row must be 401
// with the SAME body, and none may dispatch.
func TestGitHubTriggerRefusesEverySignatureThatIsNotItsOwn(t *testing.T) {
	h := newHarness(t)
	_, tokenID, secret, url := h.mintSignedTrigger(
		t, homeProject, homeSchema, "Repository webhook", ownerUserID, `{"type":"github"}`)
	body := `{"ref":"refs/heads/main"}`
	valid := githubSignature(secret, body)

	for _, test := range []struct {
		name   string
		target string
		body   string
		header map[string]string
	}{
		{
			name:   "no signature header at all",
			target: url,
			body:   body,
			header: nil,
		},
		{
			name:   "a signature computed under another secret",
			target: url,
			body:   body,
			header: map[string]string{pipelinetriggers.GitHubSignatureHeader: githubSignature("not-the-secret", body)},
		},
		{
			// The signature is the trigger's own and the BODY is not the one
			// it was computed over. This is the case a digest comparison
			// cannot catch and the reason the raw bytes are kept.
			name:   "the right signature over a tampered body",
			target: url,
			body:   `{"ref":"refs/heads/main","tampered":true}`,
			header: map[string]string{pipelinetriggers.GitHubSignatureHeader: valid},
		},
		{
			name:   "a well-formed signature in the wrong header",
			target: url,
			body:   body,
			header: map[string]string{"X-Gitlab-Token": valid},
		},
		{
			name:   "the malformed value a misconfigured sender sends",
			target: url,
			body:   body,
			header: map[string]string{pipelinetriggers.GitHubSignatureHeader: "sha256=not-hex"},
		},
		{
			// The mode is not decorative: the BEARER secret, which is the same
			// string, must not be accepted by a signing trigger. Otherwise a
			// url in a proxy log plus the secret in the provider's form would
			// still start runs.
			name:   "the bearer secret this trigger no longer accepts",
			target: url,
			body:   body,
			header: map[string]string{"Authorization": "Bearer " + secret},
		},
		{
			// The suffix is checked against the stored provider, so a shape
			// this service never minted does not quietly work.
			name:   "a provider suffix the row does not carry",
			target: inboundTarget(homeProject, tokenID) + "/gitlab",
			body:   body,
			header: map[string]string{pipelinetriggers.GitHubSignatureHeader: valid},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := h.do(t, http.MethodPost, test.target, test.body, test.header)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "this trigger cannot be used") {
				t.Fatalf("body = %s, want the ONE refusal every credential failure shares", response.Body.String())
			}
		})
	}
	if h.start.count() != 0 {
		t.Fatalf("dispatches = %d, want 0 — no refusal may start a run", h.start.count())
	}
}

// TestBearerTriggerIsUnchangedByTheSignatureMode. Every existing trigger is a
// `token` one, and the mode must not have moved the ground under it: the three
// carriers still work, the /github url does NOT, and a signature header is
// simply not read.
func TestBearerTriggerIsUnchangedByTheSignatureMode(t *testing.T) {
	h := newHarness(t)
	versionID, tokenID, secret := h.mintTrigger(t, homeProject, homeSchema, "Nightly report", ownerUserID)

	read := h.do(t, http.MethodGet,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID), "", nil)
	stored := decode(t, read)
	if stored["auth_mode"] != pipelinetriggers.AuthModeToken {
		t.Fatalf("auth_mode = %v, want %q for a create with no body", stored["auth_mode"], pipelinetriggers.AuthModeToken)
	}
	if url, _ := stored["url"].(string); strings.HasSuffix(url, "/github") {
		t.Fatalf("url = %q — a bearer trigger must keep the bare inbound url", url)
	}
	if _, present := stored["signature_header"]; present {
		t.Fatalf("a bearer trigger answered a signature_header: %v", stored)
	}

	body := `{"input":"run it"}`
	accepted := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), body,
		map[string]string{"Authorization": "Bearer " + secret})
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", accepted.Code, accepted.Body.String())
	}

	// A signature against a bearer trigger is not a second way in.
	signed := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), body,
		map[string]string{pipelinetriggers.GitHubSignatureHeader: githubSignature(secret, body)})
	if signed.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a signature against a bearer trigger; body = %s",
			signed.Code, signed.Body.String())
	}

	// And the /github url is refused for it, because the row says `custom`.
	suffixed := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID)+"/github", body,
		map[string]string{"Authorization": "Bearer " + secret})
	if suffixed.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a provider url on a bearer trigger; body = %s",
			suffixed.Code, suffixed.Body.String())
	}
}

// TestRotatingATriggerRewritesItsMode. Rotation is the only way the settings
// dialog changes the mode, so a rotation that kept the old one would leave a
// trigger verifying signatures the user has just switched off — and the
// reverse, which is worse.
func TestRotatingATriggerRewritesItsMode(t *testing.T) {
	h := newHarness(t)
	versionID, _, secret, url := h.mintSignedTrigger(
		t, homeProject, homeSchema, "Repository webhook", ownerUserID, `{"type":"github"}`)
	body := `{"ref":"refs/heads/main"}`
	if response := h.do(t, http.MethodPost, url, body,
		map[string]string{pipelinetriggers.GitHubSignatureHeader: githubSignature(secret, body)},
	); response.Code != http.StatusAccepted {
		t.Fatalf("the signed trigger did not work before the rotation: %d %s", response.Code, response.Body.String())
	}

	rotated := h.do(t, http.MethodPost,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID), `{"type":"custom"}`, nil)
	if rotated.Code != http.StatusOK {
		t.Fatalf("rotate: status = %d, body = %s", rotated.Code, rotated.Body.String())
	}
	after := decode(t, rotated)
	if after["auth_mode"] != pipelinetriggers.AuthModeToken {
		t.Fatalf("auth_mode after the rotation = %v, want %q", after["auth_mode"], pipelinetriggers.AuthModeToken)
	}
	newSecret, _ := after["secret"].(string)
	newURL, _ := after["url"].(string)
	if strings.HasSuffix(newURL, "/github") {
		t.Fatalf("url after the rotation = %q, want the bare inbound url", newURL)
	}
	if response := h.do(t, http.MethodPost, newURL, body,
		map[string]string{"Authorization": "Bearer " + newSecret},
	); response.Code != http.StatusAccepted {
		t.Fatalf("the rotated bearer trigger was refused: %d %s", response.Code, response.Body.String())
	}
}

// TestSignatureModeRefusesAModeThisServiceDoesNotImplement. The settings route
// says what is wrong, unlike the inbound one: its caller is an authenticated
// person configuring their own pipeline, and a silent fallback to the weaker
// mode is the failure being avoided.
func TestSignatureModeRefusesAModeThisServiceDoesNotImplement(t *testing.T) {
	h := newHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Repository webhook", ownerUserID)
	target := fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID)

	for _, body := range []string{
		`{"type":"gitlab"}`,
		`{"auth_mode":"hmac_sha512","signature_header":"X-Sig"}`,
		`{"auth_mode":"hmac_sha256"}`,
		`{"type":"github","auth_mode":"token"}`,
		`{"auth_mode":"hmac_sha256","signature_header":"X Sig"}`,
	} {
		t.Run(body, func(t *testing.T) {
			response := h.do(t, http.MethodPost, target, body, nil)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", response.Code, response.Body.String())
			}
		})
	}
	// Nothing was stored by any of them: a refused create leaves no trigger.
	read := h.do(t, http.MethodGet, target, "", nil)
	if decode(t, read)["configured"] == true {
		t.Fatal("a refused create left a trigger behind")
	}
	if h.vault.size() != 0 {
		t.Fatalf("vault entries = %d, want 0 — a refused create must mint nothing", h.vault.size())
	}
}

// TestSignedTriggerReportsAnUnreadableVaultAsAnOutage. An HMAC cannot be
// verified from a digest, so this is the one inbound path that opens the
// vault. A vault that will not open is a deployment fault: reporting it as the
// credential refusal would tell a sender to change a secret that is correct.
func TestSignedTriggerReportsAnUnreadableVaultAsAnOutage(t *testing.T) {
	h := newHarness(t)
	_, _, secret, url := h.mintSignedTrigger(
		t, homeProject, homeSchema, "Repository webhook", ownerUserID, `{"type":"github"}`)
	body := `{"ref":"refs/heads/main"}`
	signature := githubSignature(secret, body)

	// The row stays and the plaintext goes, which is exactly what an unopenable
	// Fernet vault looks like to this path.
	for name := range h.vault.entries {
		if err := h.vault.DeleteAdminHiddenSecret(context.Background(), name); err != nil {
			t.Fatalf("empty the vault: %v", err)
		}
	}

	response := h.do(t, http.MethodPost, url, body,
		map[string]string{pipelinetriggers.GitHubSignatureHeader: signature})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for an unreadable credential store; body = %s",
			response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "this trigger cannot be used") {
		t.Fatalf("an outage was reported as a credential refusal: %s", response.Body.String())
	}
	if h.start.count() != 0 {
		t.Fatalf("dispatches = %d, want 0", h.start.count())
	}
}

// #984: A BODYLESS ROTATE KEEPS THE TRIGGER IT ROTATES.
//
// Create and rotate are one route, and the rotate button on the EditPipeline
// card sends NO BODY (`usePipelineTriggerSettings.ts`). `parseAuthMode` mapped
// an absent body to the CREATE default (bearer/custom) and `upsertTrigger`
// writes all three 0138 columns on the conflict arm, so rotating a GitHub
// trigger silently turned it into a token one: the url lost its `/github`
// suffix, the row stopped verifying signatures, and GitHub's next delivery was
// refused — with nothing in the product saying anything but the secret had
// changed.
//
// `TestRotatingATriggerRewritesItsMode` is the other half and must keep
// passing: a body that NAMES a mode still replaces it.
func TestABodylessRotateKeepsTheStoredSignatureMode(t *testing.T) {
	h := newHarness(t)
	versionID, _, secret, url := h.mintSignedTrigger(
		t, homeProject, homeSchema, "Repository webhook", ownerUserID, `{"type":"github"}`)
	body := `{"ref":"refs/heads/main","commits":[]}`
	if response := h.do(t, http.MethodPost, url, body,
		map[string]string{pipelinetriggers.GitHubSignatureHeader: githubSignature(secret, body)},
	); response.Code != http.StatusAccepted {
		t.Fatalf("the signed trigger did not work before the rotation: %d %s",
			response.Code, response.Body.String())
	}

	// THE ROTATION THE PRODUCT ACTUALLY SENDS: no body at all.
	rotated := h.do(t, http.MethodPost,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID), "", nil)
	if rotated.Code != http.StatusOK {
		t.Fatalf("rotate: status = %d, body = %s", rotated.Code, rotated.Body.String())
	}
	after := decode(t, rotated)
	if after["auth_mode"] != pipelinetriggers.AuthModeHMACSHA256 {
		t.Fatalf("auth_mode after a bodyless rotate = %v, want the stored %q",
			after["auth_mode"], pipelinetriggers.AuthModeHMACSHA256)
	}
	if after["signature_header"] != pipelinetriggers.GitHubSignatureHeader {
		t.Fatalf("signature_header after a bodyless rotate = %v, want %q",
			after["signature_header"], pipelinetriggers.GitHubSignatureHeader)
	}
	if after["provider"] != pipelinetriggers.ProviderGitHub {
		t.Fatalf("provider after a bodyless rotate = %v, want %q",
			after["provider"], pipelinetriggers.ProviderGitHub)
	}
	newURL, _ := after["url"].(string)
	if !strings.HasSuffix(newURL, "/github") {
		t.Fatalf("url after a bodyless rotate = %q, want the /github suffix kept", newURL)
	}

	// And the thing that matters to the person who pressed the button: the new
	// secret verifies the next signed delivery, on the same webhook form.
	newSecret, _ := after["secret"].(string)
	if newSecret == "" || newSecret == secret {
		t.Fatalf("the rotation did not mint a new secret: %v", after)
	}
	if response := h.do(t, http.MethodPost, newURL, body,
		map[string]string{pipelinetriggers.GitHubSignatureHeader: githubSignature(newSecret, body)},
	); response.Code != http.StatusAccepted {
		t.Fatalf("the rotated signed trigger was refused: %d %s",
			response.Code, response.Body.String())
	}
}

// #984: A SIGNED DELIVERY LARGER THAN 64 KiB MUST BE ABLE TO START A RUN.
//
// `readInboundRaw` capped EVERY inbound body at 64 KiB and answered 413 before
// the trigger row was looked up. A signing sender's body is not ours to shape
// — GitHub documents 25 MB as its maximum, and an ordinary push with many
// commits or a pull_request with a long description passes 64 KiB without
// being unusual — so those deliveries could never run: GitHub marked them
// failed and the pipeline never fired.
//
// The bearer cap is unchanged and now applied AFTER the row says which mode it
// is, which the second half of this test pins: the larger read is for signing
// triggers, not a relaxation for everybody.
func TestASignedDeliveryLargerThanTheBearerCapStartsARun(t *testing.T) {
	h := newHarness(t)
	_, _, secret, url := h.mintSignedTrigger(
		t, homeProject, homeSchema, "Repository webhook", ownerUserID, `{"type":"github"}`)

	// A push payload just over 64 KiB, built the way a real one grows: many
	// commit messages, not one enormous string.
	commits := make([]string, 0, 700)
	for index := range 700 {
		commits = append(commits, fmt.Sprintf(
			`{"id":"%040d","message":"AUTOTESTMED commit %d — %s"}`,
			index, index, strings.Repeat("detail ", 12)))
	}
	body := `{"ref":"refs/heads/main","commits":[` + strings.Join(commits, ",") + `]}`
	if len(body) <= 64*1024 {
		t.Fatalf("this case needs a body over the bearer cap; got %d bytes", len(body))
	}

	response := h.do(t, http.MethodPost, url, body,
		map[string]string{pipelinetriggers.GitHubSignatureHeader: githubSignature(secret, body)})
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 for a %d-byte signed delivery; body = %s",
			response.Code, len(body), response.Body.String())
	}
	if h.start.count() != 1 {
		t.Fatalf("dispatches = %d, want 1", h.start.count())
	}

	// The signature is still verified over the WHOLE body: a large payload is
	// not a way past the check.
	tampered := body[:len(body)-1] + " }"
	refused := h.do(t, http.MethodPost, url, tampered,
		map[string]string{pipelinetriggers.GitHubSignatureHeader: githubSignature(secret, body)})
	if refused.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a large body whose signature does not cover it",
			refused.Code)
	}
}

// The other side of the same pair: a BEARER trigger keeps the 64 KiB cap, and
// the refusal is still a 413.
func TestABearerDeliveryOverTheCapIsStillRefused(t *testing.T) {
	h := newHarness(t)
	_, tokenID, secret := h.mintTrigger(t, homeProject, homeSchema, "Nightly report", ownerUserID)

	body := `{"input":"` + strings.Repeat("A", 70*1024) + `"}`
	response := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), body,
		map[string]string{"Authorization": "Bearer " + secret})
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 for a %d-byte bearer body; body = %s",
			response.Code, len(body), response.Body.String())
	}
	if h.start.count() != 0 {
		t.Fatalf("dispatches = %d, want 0", h.start.count())
	}
}
