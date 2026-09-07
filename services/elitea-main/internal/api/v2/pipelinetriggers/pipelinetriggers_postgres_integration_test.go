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
	// tenantMigration is applied as the SHIPPED FILE, not as a copy of its DDL,
	// so a change to the migration that these tests do not expect fails here
	// rather than passing against a stale duplicate.
	tenantMigration = "tenant/0133_pipeline_triggers_and_schedules.sql"
)

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
		recorder, nil,
	)
	router := chi.NewRouter()
	// The SETTINGS routes are mounted WITHOUT the project permission gate the
	// production router puts above them: those gates are pinned in
	// internal/api/router_elitea_core_project_scope_test.go, and repeating them
	// here would test the middleware twice and the handler not at all.
	//
	// The authenticated caller is injected the way the Auth middleware does it,
	// because `created_by` and `author_id` come from the CONTEXT and never from
	// a body, and a handler that read them from a body would pass a test that
	// supplied one.
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
	return &harness{pool: pool, start: start, vault: vault, recorder: recorder, handler: handler, router: router}
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
		h.recorder, nil,
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
		h.recorder, nil,
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
	migration, err := platformmigrations.Files.ReadFile(tenantMigration)
	if err != nil {
		t.Fatalf("read %s: %v", tenantMigration, err)
	}
	for _, schema := range []string{homeSchema, otherSchema} {
		if _, err := pool.Exec(ctx,
			fmt.Sprintf("SET search_path TO %s; %s", pgx.Identifier{schema}.Sanitize(), string(migration)),
		); err != nil {
			t.Fatalf("apply %s to %s: %v", tenantMigration, schema, err)
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
