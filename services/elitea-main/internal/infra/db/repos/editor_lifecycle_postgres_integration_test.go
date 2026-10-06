package repos

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	agentapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/ownership"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

type editorLifecycleFixture struct {
	pool          *pgxpool.Pool
	repo          *ConversationsRepo
	app           applications.Application
	first, second applications.Version
	prefix        string
}

func editorLifecycleContext(ctx context.Context, actor string) context.Context {
	return auth.ContextWithUser(ctx, auth.User{ID: actor, UserID: actor})
}

func newEditorLifecycleFixture(t *testing.T, pool *pgxpool.Pool) editorLifecycleFixture {
	t.Helper()
	prefix := "editor-lifecycle-" + uuid.NewString()
	appRepo := NewApplicationsRepo(pool)
	ctx := editorLifecycleContext(t.Context(), "1")
	app, err := appRepo.Create(ctx, applications.CreateRequest{
		ProjectID: "1", Name: prefix, Type: "pipeline", AuthorID: ownership.UserID(1),
		InitialVersion: &applications.Version{Name: "saved-first", AuthorID: 1, AgentType: "pipeline", Instructions: "version one", LLMSettings: map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(app.Versions) != 1 {
		t.Fatalf("initial saved versions=%d", len(app.Versions))
	}
	second, err := appRepo.CreateVersion(ctx, "1", app.ID, applications.Version{Name: "saved-second", AuthorID: 1, AgentType: "pipeline", Instructions: "version two", LLMSettings: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	return editorLifecycleFixture{pool: pool, repo: NewConversationsRepo(pool), app: app, first: app.Versions[0], second: second, prefix: prefix}
}

func (f editorLifecycleFixture) candidate(version string) conversations.Conversation {
	return conversations.Conversation{Name: f.prefix, Source: conversations.EditorTestSource, CreatedBy: "999", Participants: []conversations.Participant{{EntityName: "application", EntityMeta: map[string]any{"id": f.app.ID, "project_id": "1"}, EntitySettings: map[string]any{"version_id": version}}}}
}

func (f editorLifecycleFixture) create(t *testing.T, version string) conversations.Conversation {
	t.Helper()
	c, err := f.repo.Create(editorLifecycleContext(t.Context(), "1"), "1", f.candidate(version))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func editorLifecycleSnapshot(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var snapshot string
	err := pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
      'conversations',(SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY c.id),'[]') FROM p_1.chat_conversations c),
      'participants',(SELECT COALESCE(jsonb_agg(to_jsonb(p) ORDER BY p.id),'[]') FROM p_1.chat_participants p),
      'mapping',(SELECT COALESCE(jsonb_agg(to_jsonb(m) ORDER BY m.id),'[]') FROM p_1.chat_participant_mapping m),
      'groups',(SELECT COALESCE(jsonb_agg(to_jsonb(m) ORDER BY m.id),'[]') FROM p_1.chat_message_group m),
      'items',(SELECT COALESCE(jsonb_agg(to_jsonb(m) ORDER BY m.id),'[]') FROM p_1.chat_message_items m),
      'text',(SELECT COALESCE(jsonb_agg(to_jsonb(m) ORDER BY m.id),'[]') FROM p_1.chat_messages_text m),
      'jobs',(SELECT COALESCE(jsonb_agg(to_jsonb(j) ORDER BY j.execution_id,j.generation),'[]') FROM elitea_runtime.execution_jobs j),
      'outbox',(SELECT COALESCE(jsonb_agg(to_jsonb(o) ORDER BY o.outbox_id),'[]') FROM elitea_runtime.command_outbox o),
      'bindings',(SELECT COALESCE(jsonb_agg(to_jsonb(b) ORDER BY b.execution_id,b.generation),'[]') FROM elitea_runtime.agent_execution_jobs b),
      'bundles',(SELECT COALESCE(jsonb_agg(to_jsonb(b) ORDER BY b.input_bundle_id),'[]') FROM elitea_runtime.input_bundles b),
      'entries',(SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY e.input_bundle_id,e.entry_id),'[]') FROM elitea_runtime.input_bundle_entries e),
      'claims',(SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY c.claim_id),'[]') FROM elitea_runtime.execution_claims c),
      'reservations',(SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.capability_id,r.idempotency_key),'[]') FROM elitea_runtime.agent_admission_reservations r)
    )::text`).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func editorLifecycleHTTP(t *testing.T, f editorLifecycleFixture, actor, url, conversationID string, get bool) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/editor-fixture"+url, nil)
	routing := chi.NewRouteContext()
	routing.URLParams.Add("projectID", "1")
	if conversationID != "" {
		routing.URLParams.Add("conversationID", conversationID)
	}
	ctx := context.WithValue(editorLifecycleContext(t.Context(), actor), chi.RouteCtxKey, routing)
	request = request.WithContext(ctx)
	response := httptest.NewRecorder()
	handler := conversations.NewHandler(f.repo).WithPool(f.pool)
	if get {
		handler.Get(response, request)
	} else {
		handler.List(response, request)
	}
	return response
}

// The fixture uses the production atomic admission writer, never INSERTs result rows.
func (f editorLifecycleFixture) submitRequest(t *testing.T, c conversations.Conversation) agentapp.SubmitRequest {
	t.Helper()
	var target int64
	for _, p := range c.Participants {
		if p.EntityName == "application" {
			target = int64(p.ID)
		}
	}
	appID, _ := strconv.ParseInt(f.app.ID, 10, 64)
	metadata, _ := c.Meta["editor_test"].(map[string]any)
	versionID, _ := strconv.ParseInt(metadata["application_version_id"].(string), 10, 64)
	question, response, item := uuid.NewString(), uuid.NewString(), uuid.NewString()
	input := agentCapacityInput(question)
	input.Application = []byte(fmt.Sprintf(`{"id":%d,"version_id":%d}`, appID, versionID))
	input.UserInput = []byte(`"fixture request"`)
	input.ExecutionGeneration = proto.String(question)
	return agentapp.SubmitRequest{
		Identity:       executionapp.AdmissionIdentity{TenantID: "1", ResourceProjectID: "1", ProjectionProjectID: "1", ActorID: "1"},
		IdempotencyKey: question, CapabilityID: executiondomain.AgentApplicationCapability,
		ClientStreamID: c.UUID, ClientMessageID: response, SIOEvent: "chat_predict", Input: input,
		CurrentTurn: &agentapp.CurrentApplicationTurn{ProjectID: 1, ActorUserID: 1, ConversationUUID: c.UUID, TargetParticipantID: target, ApplicationID: appID, ApplicationVersionID: versionID, QuestionID: question, QuestionItemID: item, ResponseMessageID: response, QuestionMeta: []byte(`{}`), UserInput: "fixture request"},
	}
}

func editorLifecycleAdmissions(t *testing.T, pool *pgxpool.Pool) *agentapp.AdmissionService {
	t.Helper()
	policy := testAgentDispatchPolicy()
	policy.MaxOutstanding = 1024
	repo, err := NewAgentExecutionJobsRepository(pool, policy, 1)
	if err != nil {
		t.Fatal(err)
	}
	newID := func() (string, error) { return uuid.NewString(), nil }
	factory, err := agentapp.NewInputBundleFactory(agentapp.InputProfile{Classification: "tenant-confidential", RequiredGrantAudience: "elitea.runtime.input.read.v1"}, newID)
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentapp.NewAdmissionService(repo, factory, time.Now, newID)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func (f editorLifecycleFixture) persistTerminal(t *testing.T, request agentapp.SubmitRequest, outcome executionapp.AdmissionOutcome, paused bool) {
	t.Helper()
	projects, err := newPostgresProjectStore(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	terminal := currentAgentTerminal{FullMessage: &currentAgentFullMessage{Content: "fixture response", ThreadID: request.ClientStreamID, References: []byte(`[]`), InvokedSkills: []byte(`[]`)}}
	if paused {
		terminal = currentAgentTerminal{HITLPause: &currentAgentHITLPause{ThreadID: request.ClientStreamID, Interrupt: []byte(`{"interrupt_id":"original-decision"}`), Interrupts: []byte(`[{"interrupt_id":"original-decision"}]`), InvokedSkills: []byte(`[]`)}}
	}
	expected := outputapp.ExpectedAgentExecution{ClientStreamID: request.ClientStreamID, ClientMessageID: request.ClientMessageID, ExecutionID: outcome.ExecutionID, Generation: 1, ClientExecutionGeneration: request.CurrentTurn.QuestionID}
	if err := projects.WithinProjectTx(t.Context(), 1, pgx.TxOptions{}, func(tx sqlExecutor) error { return persistCurrentAgentTerminal(t.Context(), tx, expected, terminal) }); err != nil {
		t.Fatal(err)
	}
}

func TestEditorLifecyclePostgres(t *testing.T) {
	for _, key := range []string{"ELITEA_AI_PROJECT_ID", "AI_PROJECT_ID", "PUBLIC_PROJECT_ID", "SHARED_PROJECT_ID"} {
		t.Setenv(key, "99")
	}
	pool := editorLifecyclePostgresPool(t)
	t.Run("atomic_scope_immutable_identity_and_saved_version", func(t *testing.T) {
		f := newEditorLifecycleFixture(t, pool)
		c := f.create(t, f.first.ID)
		if c.CreatedBy != "1" || c.Source != "editor_test" || c.IsPrivate == nil || !*c.IsPrivate || c.Meta["is_hidden"] != true || len(c.Participants) != 3 {
			t.Fatalf("invalid atomic scope: %+v", c)
		}
		before := editorLifecycleSnapshot(t, pool)
		bads := []conversations.Conversation{f.candidate("999999999"), f.candidate(f.first.ID), f.candidate(f.first.ID), f.candidate(f.first.ID)}
		bads[1].Participants[0].EntityMeta["project_id"] = "2"
		bads[2].Meta = map[string]any{"editor_test": map[string]any{"actor_id": "999"}}
		bads[3].Participants = append(bads[3].Participants, conversations.Participant{EntityName: "user", EntityMeta: map[string]any{"id": "999"}})
		for i, bad := range bads {
			if _, err := f.repo.Create(editorLifecycleContext(t.Context(), "1"), "1", bad); err == nil {
				t.Errorf("invalid scope %d admitted", i)
			}
			if after := editorLifecycleSnapshot(t, pool); after != before {
				t.Fatalf("invalid scope %d changed durable rows", i)
			}
		}
		if _, err := f.repo.Update(editorLifecycleContext(t.Context(), "1"), "1", c.ID, conversations.Conversation{Meta: map[string]any{"steps_limit": 7}}); err != nil {
			t.Fatal(err)
		}
		stored, err := f.repo.Get(t.Context(), "1", c.ID)
		if err != nil || !reflect.DeepEqual(stored.Meta["editor_test"], c.Meta["editor_test"]) || stored.Meta["is_hidden"] != true {
			t.Fatal("generic update replaced immutable scope")
		}
		target := strconv.Itoa(c.Participants[0].ID)
		if err := f.repo.UpdateEntitySettings(t.Context(), "1", c.ID, target, map[string]any{"version_id": f.second.ID}); err == nil {
			t.Fatal("saved version changed")
		}
		if err := f.repo.RemoveParticipant(t.Context(), "1", c.ID, target); err == nil {
			t.Fatal("immutable participant removed")
		}
		savedSecond := f.create(t, f.second.ID)
		resolver, err := NewCurrentAgentStartRepository(pool, 99)
		if err != nil {
			t.Fatal(err)
		}
		for _, scope := range []conversations.Conversation{c, savedSecond} {
			request := f.submitRequest(t, scope)
			target, err := resolver.ResolveCurrentApplication(t.Context(), agentapp.CurrentApplicationStartRequest{ProjectID: 1, ActorUserID: 1, ConversationUUID: scope.UUID, TargetParticipantID: request.CurrentTurn.TargetParticipantID, QuestionID: request.CurrentTurn.QuestionID, UserInput: "fixture request"})
			if err != nil || target.ApplicationVersionID != request.CurrentTurn.ApplicationVersionID {
				t.Fatalf("saved version isolation target=%+v err=%v", target, err)
			}
		}
	})

	t.Run("participant_failure_rolls_back_atomic_initial_scope", func(t *testing.T) {
		f := newEditorLifecycleFixture(t, pool)
		if _, err := pool.Exec(t.Context(), `CREATE FUNCTION public.editor_lifecycle_reject_mapping() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF EXISTS (SELECT 1 FROM p_1.chat_conversations WHERE id=NEW.conversation_id AND meta->>'editor_test_fault'='participant-mapping') THEN RAISE EXCEPTION 'private fixture participant fault'; END IF; RETURN NEW; END $$; CREATE TRIGGER editor_lifecycle_reject_mapping BEFORE INSERT ON p_1.chat_participant_mapping FOR EACH ROW EXECUTE FUNCTION public.editor_lifecycle_reject_mapping()`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := pool.Exec(context.WithoutCancel(t.Context()), `DROP TRIGGER editor_lifecycle_reject_mapping ON p_1.chat_participant_mapping; DROP FUNCTION public.editor_lifecycle_reject_mapping()`); err != nil {
				t.Errorf("remove exact fixture fault: %v", err)
			}
		})
		before := editorLifecycleSnapshot(t, pool)
		c := f.candidate(f.first.ID)
		c.Meta = map[string]any{"editor_test_fault": "participant-mapping"}
		if _, err := f.repo.Create(editorLifecycleContext(t.Context(), "1"), "1", c); err == nil {
			t.Fatal("fixture fault did not fail initial participant write")
		}
		if after := editorLifecycleSnapshot(t, pool); after != before {
			t.Fatal("failed initial participant write left conversation or participant rows")
		}
	})
	t.Run("normal_list_excludes_persisted_test_scope", func(t *testing.T) {
		f := newEditorLifecycleFixture(t, pool)
		c := f.create(t, f.first.ID)
		ordinary, err := f.repo.Create(editorLifecycleContext(t.Context(), "1"), "1", conversations.Conversation{Name: f.prefix + "-ordinary"})
		if err != nil {
			t.Fatal(err)
		}
		before := editorLifecycleSnapshot(t, pool)
		for _, query := range []string{"?limit=100", "?limit=100&mine=true", "?limit=100&source=editor_test"} {
			response := editorLifecycleHTTP(t, f, "1", query, "", false)
			if response.Code != 200 {
				t.Fatalf("list %s: %d %s", query, response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), c.Name+`"`) {
				t.Fatalf("ordinary list leaked editor Test scope: %s", query)
			}
			if query == "?limit=100" && !strings.Contains(response.Body.String(), ordinary.Name) {
				t.Fatal("ordinary list lost normal conversation")
			}
		}
		response := editorLifecycleHTTP(t, f, "1", "?limit=100&hidden=only&source=editor_test&entity_name=application&entity_meta_id="+f.app.ID, "", false)
		if response.Code != 200 || !strings.Contains(response.Body.String(), c.Name) {
			t.Fatalf("Test-specific History missing: %d %s", response.Code, response.Body.String())
		}
		if after := editorLifecycleSnapshot(t, pool); after != before {
			t.Fatal("list creates or mutates durable work")
		}
	})
	t.Run("competing_exact_admission_is_one_atomic_effect", func(t *testing.T) {
		f := newEditorLifecycleFixture(t, pool)
		c := f.create(t, f.first.ID)
		request := f.submitRequest(t, c)
		services := []*agentapp.AdmissionService{editorLifecycleAdmissions(t, pool), editorLifecycleAdmissions(t, pool)}
		const attempts = 8
		type result struct {
			outcome executionapp.AdmissionOutcome
			err     error
		}
		start := make(chan struct{})
		results := make(chan result, attempts)
		var wg sync.WaitGroup
		for i := 0; i < attempts; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				o, e := services[i%2].Submit(t.Context(), request)
				results <- result{o, e}
			}(i)
		}
		close(start)
		wg.Wait()
		close(results)
		created := 0
		var winner executionapp.AdmissionOutcome
		all := make([]executionapp.AdmissionOutcome, 0, attempts)
		for result := range results {
			all = append(all, result.outcome)
			if result.err != nil {
				t.Fatal(result.err)
			}
			if result.outcome.Created {
				created++
				winner = result.outcome
			}
		}
		if created != 1 {
			t.Fatalf("competing exact admission winners=%d", created)
		}
		for _, outcome := range all {
			if outcome.ExecutionID != winner.ExecutionID || outcome.CommandID != winner.CommandID || !outcome.AdmittedAt.Equal(winner.AdmittedAt) || !outcome.Deadline.Equal(winner.Deadline) {
				t.Fatal("competing replay changed durable identity or original clocks")
			}
		}
		var jobs, outbox, bundles, groups int
		if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM elitea_runtime.execution_jobs WHERE idempotency_key=$1),(SELECT count(*) FROM elitea_runtime.command_outbox WHERE execution_id=$2),(SELECT count(*) FROM elitea_runtime.input_bundles b JOIN elitea_runtime.execution_jobs j USING(input_bundle_id) WHERE j.execution_id=$2),(SELECT count(*) FROM p_1.chat_message_group WHERE conversation_id=$3::integer)`, request.IdempotencyKey, winner.ExecutionID, c.ID).Scan(&jobs, &outbox, &bundles, &groups); err != nil {
			t.Fatal(err)
		}
		if jobs != 1 || outbox != 1 || bundles != 1 || groups != 2 {
			t.Fatalf("atomic effects jobs=%d outbox=%d bundles=%d groups=%d", jobs, outbox, bundles, groups)
		}
		before := editorLifecycleSnapshot(t, pool)
		changed := request
		changed.Input = proto.Clone(request.Input).(*runtimev1.AgentExecutionInputV1)
		changed.Input.UserInput = []byte(`"different request"`)
		if _, err := services[0].Submit(t.Context(), changed); !errors.Is(err, executionapp.ErrIdempotencyConflict) {
			t.Fatalf("changed exact replay: %v", err)
		}
		if after := editorLifecycleSnapshot(t, pool); after != before {
			t.Fatal("conflicting replay changed original durable effects")
		}
	})
	t.Run("wrong_saved_version_admission_rolls_back_all_effects", func(t *testing.T) {
		f := newEditorLifecycleFixture(t, pool)
		c := f.create(t, f.first.ID)
		request := f.submitRequest(t, c)
		id, _ := strconv.ParseInt(f.second.ID, 10, 64)
		request.CurrentTurn.ApplicationVersionID = id
		request.Input.Application = []byte(fmt.Sprintf(`{"id":%s,"version_id":%s}`, f.app.ID, f.second.ID))
		before := editorLifecycleSnapshot(t, pool)
		_, err := editorLifecycleAdmissions(t, pool).Submit(t.Context(), request)
		if !errors.Is(err, agentapp.ErrUnsupportedCurrentAgentStart) {
			t.Fatalf("wrong saved version error=%v", err)
		}
		if after := editorLifecycleSnapshot(t, pool); after != before {
			t.Fatal("failed turn left admission/message effects")
		}
	})
	t.Run("durable_history_pages_57_real_admissions_without_read_effects", func(t *testing.T) {
		f := newEditorLifecycleFixture(t, pool)
		c := f.create(t, f.first.ID)
		service := editorLifecycleAdmissions(t, pool)
		const count = 57
		expected := make([]string, 0, count)
		for i := 0; i < count; i++ {
			request := f.submitRequest(t, c)
			outcome, err := service.Submit(t.Context(), request)
			if err != nil || !outcome.Created {
				t.Fatalf("admit %d: %+v %v", i, outcome, err)
			}
			expected = append(expected, outcome.ExecutionID)
			f.persistTerminal(t, request, outcome, false)
		}
		before := editorLifecycleSnapshot(t, pool)
		first, err := f.repo.EditorTestRuns(editorLifecycleContext(t.Context(), "1"), "1", c.ID, 50, 0)
		if err != nil {
			t.Fatalf("read actual 50-row history: %v", err)
		}
		second, err := NewConversationsRepo(pool).EditorTestRuns(editorLifecycleContext(t.Context(), "1"), "1", c.UUID, 50, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(first.Rows) != 50 || !first.HasMore || len(second.Rows) != 7 || second.HasMore {
			t.Fatalf("bounded history pages=%d/%t %d/%t", len(first.Rows), first.HasMore, len(second.Rows), second.HasMore)
		}
		rows := append(first.Rows, second.Rows...)
		for i, run := range rows {
			if run.ExecutionID != expected[count-1-i] || run.QuestionID != run.ExecutionGeneration || run.InputReference.BundleID == "" || run.InputReference.EntryID == "" || run.InputReference.ImmutableVersion == "" || len(run.InputReference.ContentDigest) != 64 {
				t.Fatalf("incorrect durable row %d: %+v", i, run)
			}
			var bytes []byte
			if err := pool.QueryRow(t.Context(), `SELECT content_digest FROM elitea_runtime.input_bundle_entries WHERE input_bundle_id=$1 AND entry_id=$2`, run.InputReference.BundleID, run.InputReference.EntryID).Scan(&bytes); err != nil || hex.EncodeToString(bytes) != run.InputReference.ContentDigest {
				t.Fatal("restore changed original input reference")
			}
		}
		again, err := f.repo.EditorTestRuns(editorLifecycleContext(t.Context(), "1"), "1", c.ID, 50, 0)
		if err != nil || !reflect.DeepEqual(first, again) {
			t.Fatal("unchanged durable projection is nondeterministic")
		}
		empty, err := f.repo.EditorTestRuns(editorLifecycleContext(t.Context(), "1"), "1", c.ID, 50, count)
		if err != nil || len(empty.Rows) != 0 || empty.HasMore {
			t.Fatal("offset past history wrapped or produced a row")
		}
		for _, bounds := range [][2]int{{0, 0}, {51, 0}, {50, -1}, {50, 10001}} {
			if _, err := f.repo.EditorTestRuns(editorLifecycleContext(t.Context(), "1"), "1", c.ID, bounds[0], bounds[1]); apiStatus(err) != 400 {
				t.Fatalf("invalid bounds accepted: %v", bounds)
			}
		}
		// Discarding the browser's ephemeral transcript is a local action. A
		// new Main reader must still see every durable question and response.
		transcript, err := NewConversationsRepo(pool).ListMessages(t.Context(), "1", c.UUID, conversations.MessagesQuery{Limit: 200, SortOrder: "asc", SortBy: "created_at"})
		if err != nil || transcript.Total != count*2 {
			t.Fatalf("reload destroyed durable transcript/history total=%d err=%v", transcript.Total, err)
		}
		if after := editorLifecycleSnapshot(t, pool); after != before {
			t.Fatal("history/reload read changed durable effects")
		}
	})
	t.Run("code_recovery_pause_retains_original_identity_and_no_events", func(t *testing.T) {
		f := newEditorLifecycleFixture(t, pool)
		c := f.create(t, f.first.ID)
		request := f.submitRequest(t, c)
		outcome, err := editorLifecycleAdmissions(t, pool).Submit(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		read := func(t *testing.T) conversations.EditorTestRun {
			t.Helper()
			before := editorLifecycleSnapshot(t, pool)
			response := editorLifecycleHTTP(t, f, "1", "?editor_test_runs=true", c.UUID, true)
			if response.Code != http.StatusOK {
				t.Fatalf("restore read: %d %s", response.Code, response.Body.String())
			}
			var payload struct {
				Runs conversations.EditorTestRunsPage `json:"editor_test_runs"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload.Runs.Rows) != 1 {
				t.Fatalf("restore rows=%d", len(payload.Runs.Rows))
			}
			if after := editorLifecycleSnapshot(t, pool); after != before {
				t.Fatal("recovery restore read changed durable effects")
			}
			return payload.Runs.Rows[0]
		}
		original := read(t)
		for _, test := range []struct {
			name, executionID, generation, phase string
			streaming, canControl, hasEvents     bool
		}{
			{"streaming", outcome.ExecutionID, request.CurrentTurn.QuestionID, "RUNNING", true, true, true},
			{"paused", outcome.ExecutionID, request.CurrentTurn.QuestionID, "PAUSED", false, true, false},
			{"foreign_execution", uuid.NewString(), request.CurrentTurn.QuestionID, "TERMINAL", false, false, false},
			{"foreign_generation", outcome.ExecutionID, uuid.NewString(), "TERMINAL", false, false, false},
		} {
			t.Run(test.name, func(t *testing.T) {
				// The read projection uses the metadata key; it does not authorize this fixture receipt.
				tag, err := pool.Exec(t.Context(), `UPDATE p_1.chat_message_group SET is_streaming=$2,task_id=$3,
meta=meta || jsonb_build_object('execution_generation',$4::text,'node_recovery_required_v1',jsonb_build_object('fixture','code-recovery-pause'))
WHERE uuid=$1::uuid`, request.ClientMessageID, test.streaming, test.executionID, test.generation)
				if err != nil || tag.RowsAffected() != 1 {
					t.Fatalf("set exact response fixture: rows=%d err=%v", tag.RowsAffected(), err)
				}
				run := read(t)
				if run.Phase != test.phase || run.CanControl != test.canControl || (run.EventsURL != "") != test.hasEvents {
					t.Fatalf("incorrect recovery projection: %+v", run)
				}
				if run.ResponseMessageID != request.ClientMessageID || run.QuestionID != request.CurrentTurn.QuestionID || run.ExecutionID != outcome.ExecutionID || run.ExecutionGeneration != request.CurrentTurn.QuestionID || run.InputReference != original.InputReference {
					t.Fatalf("restore changed original admission identity: %+v", run)
				}
			})
		}
	})
	t.Run("paused_restore_retains_original_control_and_denies_foreign_actor", func(t *testing.T) {
		f := newEditorLifecycleFixture(t, pool)
		c := f.create(t, f.first.ID)
		request := f.submitRequest(t, c)
		service := editorLifecycleAdmissions(t, pool)
		outcome, err := service.Submit(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		f.persistTerminal(t, request, outcome, true)
		before := editorLifecycleSnapshot(t, pool)
		response := editorLifecycleHTTP(t, f, "1", "?editor_test_runs=true&runs_limit=50&messages_limit=50", c.UUID, true)
		if response.Code != 200 {
			t.Fatalf("restore read: %d %s", response.Code, response.Body.String())
		}
		var payload struct {
			Runs   conversations.EditorTestRunsPage `json:"editor_test_runs"`
			Groups []map[string]any                 `json:"message_groups"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Runs.Rows) != 1 {
			t.Fatalf("restore rows=%d", len(payload.Runs.Rows))
		}
		run := payload.Runs.Rows[0]
		if run.Phase != "PAUSED" || !run.CanControl || run.ResponseMessageID != request.ClientMessageID || run.QuestionID != request.CurrentTurn.QuestionID || run.ExecutionID != outcome.ExecutionID || run.EventsURL != "" || !strings.Contains(response.Body.String(), "original-decision") {
			t.Fatalf("restore changed original paused identities: %+v", run)
		}
		denied := editorLifecycleHTTP(t, f, "2", "?editor_test_runs=true", c.ID, true)
		if denied.Code != 404 && denied.Code != 403 {
			t.Fatalf("foreign actor read private scope: %d", denied.Code)
		}
		if after := editorLifecycleSnapshot(t, pool); after != before {
			t.Fatal("restoring/observing paused history creates effects")
		}
		cancelRepo, err := NewCurrentAgentCancelRepository(pool)
		if err != nil {
			t.Fatal(err)
		}
		_, err = cancelRepo.CancelCurrentAgent(t.Context(), agentapp.CurrentAgentCancelRequest{ProjectID: 1, ActorUserID: 2, ResponseMessageID: run.ResponseMessageID})
		if !errors.Is(err, agentapp.ErrCurrentAgentCancelNotAllowed) {
			t.Fatalf("foreign Stop allowed: %v", err)
		}
		if after := editorLifecycleSnapshot(t, pool); after != before {
			t.Fatal("foreign Stop changed durable work")
		}
	})
}
