package supportassistant

// The participant bookkeeping, and the request bounds around it, WITHOUT a
// database.
//
// The integration suite beside this file proves the rows resolve. These tests
// prove the decisions that pick those rows: which participant is the agent,
// when a stored mapping counts as wrong, and which requests never reach the
// database at all. They run everywhere, including on a machine with no
// PostgreSQL, which is why the cheap half of the package's behaviour lives
// here rather than in the integration file.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

/* ── a chat store that records what it was asked to write ──────────────── */

type fakeChatStore struct {
	participants []conversations.Participant
	added        []map[string]any
	updated      []map[string]any
	updatedIDs   []string
	listErr      error
	addErr       error
	updateErr    error
	// nextID numbers the participants the fake creates, so an add is visible
	// to the read that follows it.
	nextID int
}

func (f *fakeChatStore) ListMessageGroups(
	context.Context, string, string, int, string,
) ([]map[string]any, error) {
	return nil, nil
}

func (f *fakeChatStore) ListParticipants(
	context.Context, string, string,
) ([]conversations.Participant, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.participants, nil
}

func (f *fakeChatStore) AddParticipant(
	_ context.Context, _, _ string, body map[string]any,
) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, body)
	f.nextID++
	entityMeta, _ := body["entity_meta"].(map[string]any)
	entitySettings, _ := body["entity_settings"].(map[string]any)
	name, _ := body["entity_name"].(string)
	f.participants = append(f.participants, conversations.Participant{
		ID: f.nextID, EntityName: name,
		EntityMeta: entityMeta, EntitySettings: entitySettings,
	})
	return nil
}

func (f *fakeChatStore) UpdateEntitySettings(
	_ context.Context, _, _, participantID string, settings map[string]any,
) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updatedIDs = append(f.updatedIDs, participantID)
	f.updated = append(f.updated, settings)
	return nil
}

/* ── finding the agent among the participants ──────────────────────────── */

// THE AGENT IS FOUND BY ENTITY IDENTITY, not by position or by name.
//
// A support conversation legitimately holds the user, the agent, and whatever
// an operator's repointing left behind. Picking the wrong participant sends the
// question to a different agent, with the platform's credentials.
func TestTheAgentParticipantIsMatchedOnIdentity(t *testing.T) {
	handler := NewHandler(nil)
	store := &fakeChatStore{participants: []conversations.Participant{
		{ID: 1, EntityName: userEntityName, EntityMeta: map[string]any{"id": float64(11)}},
		{ID: 2, EntityName: applicationEntityName, EntityMeta: map[string]any{"id": float64(99), "project_id": float64(7)}},
		{ID: 3, EntityName: applicationEntityName, EntityMeta: map[string]any{"id": float64(31), "project_id": float64(8)}},
		{ID: 4, EntityName: applicationEntityName, EntityMeta: map[string]any{"id": float64(31), "project_id": float64(7)},
			EntitySettings: map[string]any{"version_id": "41"}},
	}}
	handler.chat = store

	id, settings, found, err := handler.findAgentParticipant(context.Background(), "7", "1", 31, 7)
	if err != nil || !found {
		t.Fatalf("found = %v, err = %v", found, err)
	}
	if id != 4 {
		t.Fatalf("participant = %d, want 4: a different agent or a different project answered", id)
	}
	if settings["version_id"] != "41" {
		t.Fatalf("the stored settings did not come back: %+v", settings)
	}

	// A participant written before an operator set a project id still names the
	// same agent, so an ABSENT project matches.
	store.participants = []conversations.Participant{
		{ID: 9, EntityName: applicationEntityName, EntityMeta: map[string]any{"id": float64(31)}},
	}
	if id, _, found, _ = handler.findAgentParticipant(context.Background(), "7", "1", 31, 7); !found || id != 9 {
		t.Fatalf("a participant with no project id was not matched: id=%d found=%v", id, found)
	}

	// A read failure is a failure, not "no agent" — answering "not found" would
	// attach a SECOND participant on every transient error.
	store.listErr = errors.New("read failed")
	if _, _, _, err := handler.findAgentParticipant(context.Background(), "7", "1", 31, 7); err == nil {
		t.Fatal("a failed participants read reported no agent instead of an error")
	}
}

/* ── the author participant ────────────────────────────────────────────── */

// THE AUTHOR IS ATTACHED ONCE. The predict route runs this on every turn, so a
// second attach per message would grow one participant row per question.
func TestTheAuthorParticipantIsAttachedOnce(t *testing.T) {
	handler := NewHandler(nil)
	store := &fakeChatStore{}
	handler.chat = store

	for range 3 {
		if err := handler.ensureUserParticipant(context.Background(), "7", "1", 7, 11); err != nil {
			t.Fatalf("attach: %v", err)
		}
	}
	if len(store.added) != 1 {
		t.Fatalf("the author was attached %d times, want 1", len(store.added))
	}
	body := store.added[0]
	if body["entity_name"] != userEntityName {
		t.Fatalf("entity_name = %v, want %q", body["entity_name"], userEntityName)
	}
	meta, _ := body["entity_meta"].(map[string]any)
	if meta["id"] != int64(11) || meta["project_id"] != int64(7) {
		t.Fatalf("entity_meta = %+v, want the caller and the support project", meta)
	}
}

// A HANDLER WITH NO CHAT STORE REFUSES rather than dereferencing nil. The
// routes answer 503 before reaching here; this is the second line.
func TestParticipantWritesRefuseWithoutAChatStore(t *testing.T) {
	handler := NewHandler(nil)
	if err := handler.ensureUserParticipant(context.Background(), "7", "1", 7, 11); !errors.Is(err, errChatStoreUnavailable) {
		t.Fatalf("err = %v, want errChatStoreUnavailable", err)
	}
	_, err := handler.resolveAnsweringVersion(context.Background(),
		platformconfig.SupportAssistant{ProjectID: 7, AgentID: 31})
	if !errors.Is(err, errChatStoreUnavailable) {
		t.Fatalf("err = %v, want errChatStoreUnavailable", err)
	}
	if _, err := handler.attachAgentParticipant(context.Background(),
		platformconfig.SupportAssistant{ProjectID: 7, AgentID: 31}, "1",
		agentVersion{ID: 41}); !errors.Is(err, errChatStoreUnavailable) {
		t.Fatalf("err = %v, want errChatStoreUnavailable", err)
	}
}

// AN AGENT IN ANOTHER PROJECT IS REFUSED BEFORE ANY WRITE. The turn reads
// `application_versions` from the support project's schema, so a participant
// pointing anywhere else resolves to nothing.
func TestAnAgentProjectThatIsNotTheSupportProjectIsRefused(t *testing.T) {
	handler := NewHandler(nil)
	store := &fakeChatStore{}
	handler.chat = store

	_, err := handler.resolveAnsweringVersion(context.Background(),
		platformconfig.SupportAssistant{ProjectID: 7, AgentProjectID: 8, AgentID: 31})
	if !errors.Is(err, errAgentProjectNotSupportProject) {
		t.Fatalf("err = %v, want errAgentProjectNotSupportProject", err)
	}
	if len(store.added) != 0 {
		t.Fatal("a participant was written for an agent the resolver cannot reach")
	}
}

/* ── deciding whether a stored mapping is wrong ────────────────────────── */

// THE MERGE FORCES ONLY WHAT THE RESOLVER READS, and defaults the rest.
func TestTheEntitySettingsMergeCorrectsOnlyWhatItOwns(t *testing.T) {
	wanted := agentEntitySettings(agentVersion{ID: 41, AgentType: "openai"})

	for name, testCase := range map[string]struct {
		current    map[string]any
		wantChange bool
		check      func(*testing.T, map[string]any)
	}{
		"an empty document is filled": {
			current: map[string]any{}, wantChange: true,
			check: func(t *testing.T, merged map[string]any) {
				if merged["version_id"] != "41" || merged["agent_type"] != "openai" {
					t.Fatalf("merged = %+v", merged)
				}
			},
		},
		"a stale version is corrected": {
			current:    map[string]any{"version_id": "40", "agent_type": "openai", "variables": []any{}, "icon_meta": map[string]any{}},
			wantChange: true,
			check: func(t *testing.T, merged map[string]any) {
				if merged["version_id"] != "41" {
					t.Fatalf("version_id = %v, want 41", merged["version_id"])
				}
			},
		},
		"a numeric version id is the same value": {
			current:    map[string]any{"version_id": float64(41), "agent_type": "openai", "variables": []any{}, "icon_meta": map[string]any{}},
			wantChange: false,
		},
		"keys this package does not own survive": {
			current: map[string]any{
				"version_id": "41", "agent_type": "openai",
				"variables": []any{}, "icon_meta": map[string]any{},
				"llm_settings": map[string]any{"temperature": 0.1},
			},
			wantChange: false,
		},
		"a conversation's own variables are not reset": {
			current: map[string]any{
				"version_id": "41", "agent_type": "openai",
				"variables": []any{map[string]any{"name": "tone", "value": "brief"}},
				"icon_meta": map[string]any{},
			},
			wantChange: false,
			check: func(t *testing.T, merged map[string]any) {
				list, _ := merged["variables"].([]any)
				if len(list) != 1 {
					t.Fatalf("the conversation's variables were reset: %+v", merged["variables"])
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			merged, changed := mergeAgentEntitySettings(testCase.current, wanted)
			if changed != testCase.wantChange {
				t.Fatalf("changed = %v, want %v (merged %+v)", changed, testCase.wantChange, merged)
			}
			if testCase.check != nil {
				testCase.check(t, merged)
			}
			// Nothing the caller stored is ever dropped.
			for key := range testCase.current {
				if _, present := merged[key]; !present {
					t.Fatalf("the merge dropped %q: %+v", key, merged)
				}
			}
		})
	}
}

// AN EMPTY RESOLVED VALUE IS NOT A CORRECTION. `agent_type` is COALESCEd out of
// a nullable column, and writing "" over a stored "openai" would lose what the
// read simply did not have.
func TestAnEmptyResolvedValueDoesNotOverwriteAStoredOne(t *testing.T) {
	merged, changed := mergeAgentEntitySettings(
		map[string]any{"version_id": "41", "agent_type": "openai", "variables": []any{}, "icon_meta": map[string]any{}},
		agentEntitySettings(agentVersion{ID: 41, AgentType: ""}))
	if changed {
		t.Fatalf("an empty agent_type rewrote the mapping: %+v", merged)
	}
	if merged["agent_type"] != "openai" {
		t.Fatalf("agent_type = %v, want the stored openai", merged["agent_type"])
	}
}

// `1`, `"1"` AND `1.0` ARE ONE VERSION ID. entity_settings is jsonb read back
// through encoding/json, so treating the spellings as different would make
// every turn write an UPDATE that changes nothing.
func TestJSONScalarsCompareByValue(t *testing.T) {
	for name, testCase := range map[string]struct {
		stored, wanted any
		same           bool
	}{
		"string and number":  {"41", float64(41), true},
		"number and string":  {float64(41), "41", true},
		"different numbers":  {float64(40), "41", false},
		"text and text":      {"openai", "openai", true},
		"text and other":     {"openai", "agent", false},
		"absent and present": {nil, "41", false},
		"absent and absent":  {nil, nil, true},
		"number and text":    {float64(41), "openai", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sameJSONValue(testCase.stored, testCase.wanted); got != testCase.same {
				t.Fatalf("sameJSONValue(%v, %v) = %v, want %v",
					testCase.stored, testCase.wanted, got, testCase.same)
			}
		})
	}
}

// ENTITY META NUMBERS ARRIVE IN FOUR SPELLINGS, because the document travels
// through jsonb, through encoding/json and through a Go literal in this
// package's own writes.
func TestEntityMetaNumbersAreReadInEverySpelling(t *testing.T) {
	for name, raw := range map[string]any{
		"float64": float64(31),
		"int64":   int64(31),
		"int":     31,
		"string":  "31",
	} {
		t.Run(name, func(t *testing.T) {
			value, ok := metaIntPresent(map[string]any{"id": raw}, "id")
			if !ok || value != 31 {
				t.Fatalf("metaIntPresent = (%d, %v), want (31, true)", value, ok)
			}
		})
	}
	for name, meta := range map[string]map[string]any{
		"absent":       {},
		"null":         {"id": nil},
		"not a number": {"id": "thirty-one"},
		"a document":   {"id": map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			if value, ok := metaIntPresent(meta, "id"); ok || value != 0 {
				t.Fatalf("metaIntPresent(%+v) = (%d, %v), want (0, false)", meta, value, ok)
			}
			if metaInt(meta, "id") != 0 {
				t.Fatalf("metaInt(%+v) is not zero", meta)
			}
		})
	}
}

/* ── the request bounds, before any database ───────────────────────────── */

// resolvedRequest builds a request that has already passed `resolve`, so a
// handler can be called directly and reach its own validation.
func resolvedRequest(method, path, body string, settings platformconfig.SupportAssistant) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request = request.WithContext(withSettings(request.Context(), settings))
	return withUser(request, auth.User{ID: "11", UserID: "11", Name: "Caller"})
}

// EVERY REQUEST THE ROUTE CAN REFUSE ON ITS OWN IS REFUSED AS THE CLIENT'S
// PROBLEM.
//
// The use case behind this route answers one undifferentiated error for nine
// distinct causes, which the route can only render as a 502 — "the support
// agent is broken". Each case below is one the user can act on.
func TestPredictRefusesABadRequestBeforeItReachesTheRun(t *testing.T) {
	settings := platformconfig.SupportAssistant{ProjectID: 7, AgentID: 31}
	oversized := strings.Repeat("a", maxAgentUserInput+1)

	for name, testCase := range map[string]struct {
		body string
		want int
	}{
		"not JSON":            {`{`, http.StatusBadRequest},
		"no content":          {`{"content":"","question_id":"` + sampleUUID + `"}`, http.StatusBadRequest},
		"no question id":      {`{"content":"hi"}`, http.StatusBadRequest},
		"upper-case question": {`{"content":"hi","question_id":"3F2504E0-4F89-41D3-9A0C-0305E82C3301"}`, http.StatusBadRequest},
		"a message too long":  {`{"content":"` + oversized + `","question_id":"` + sampleUUID + `"}`, http.StatusRequestEntityTooLarge},
		"a NUL in the text":   {"{\"content\":\"a\\u0000b\",\"question_id\":\"" + sampleUUID + "\"}", http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			handler := NewHandler(nil, WithChatStore(&fakeChatStore{}), WithStartUseCase(refusingStart{}))
			recorder := httptest.NewRecorder()
			handler.Predict(recorder, resolvedRequest(http.MethodPost, "/predict/"+sampleUUID, testCase.body, settings))
			if recorder.Code != testCase.want {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, testCase.want, recorder.Body.String())
			}
		})
	}
}

// A BODY OVER THE CEILING IS "TOO LARGE", not "invalid".
//
// A bare LimitReader truncates silently, so an oversized body arrives as
// malformed JSON and the client is told it sent nonsense when it sent too much.
func TestAnOversizedBodyIsReportedAsTooLarge(t *testing.T) {
	handler := NewHandler(nil, WithChatStore(&fakeChatStore{}), WithStartUseCase(refusingStart{}))
	body := `{"content":"` + strings.Repeat("b", int(maxPredictBody)+16) + `"}`
	recorder := httptest.NewRecorder()
	handler.Predict(recorder, resolvedRequest(http.MethodPost, "/predict/"+sampleUUID, body,
		platformconfig.SupportAssistant{ProjectID: 7, AgentID: 31}))
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %s)", recorder.Code, recorder.Body.String())
	}
}

// PREDICT REFUSES WITHOUT THE DEPENDENCIES IT NEEDS, with 503 rather than a nil
// dereference.
func TestPredictRefusesWithoutItsDependencies(t *testing.T) {
	settings := platformconfig.SupportAssistant{ProjectID: 7, AgentID: 31}
	recorder := httptest.NewRecorder()
	NewHandler(nil).Predict(recorder,
		resolvedRequest(http.MethodPost, "/predict/"+sampleUUID, `{}`, settings))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}

	// No settings on the context is a middleware-order fault, not a request
	// fault, so it answers 500.
	recorder = httptest.NewRecorder()
	NewHandler(nil).Predict(recorder,
		withUser(httptest.NewRequest(http.MethodPost, "/predict/"+sampleUUID, strings.NewReader(`{}`)),
			auth.User{ID: "11", UserID: "11"}))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
}

// AN UNAUTHENTICATED CALLER IS 401 ON EVERY GATED HANDLER, and a caller whose
// principal owns no user is the same answer.
func TestGatedHandlersRefuseAnUnusableCaller(t *testing.T) {
	settings := platformconfig.SupportAssistant{ProjectID: 7, AgentID: 31}
	for name, request := range map[string]*http.Request{
		"no user": httptest.NewRequest(http.MethodGet, "/conversations/", nil),
		"no owning user": withUser(httptest.NewRequest(http.MethodGet, "/conversations/", nil),
			auth.User{ID: "", UserID: ""}),
	} {
		t.Run(name, func(t *testing.T) {
			request = request.WithContext(withSettings(request.Context(), settings))
			recorder := httptest.NewRecorder()
			NewHandler(nil).ListConversations(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", recorder.Code)
			}
		})
	}
}

// CREATE REFUSES A BODY IT CANNOT READ AND A NAME IT CANNOT STORE, before it
// writes anything.
func TestCreateRefusesAnUnusableName(t *testing.T) {
	settings := platformconfig.SupportAssistant{ProjectID: 7, AgentID: 31}
	for name, testCase := range map[string]struct {
		body string
		want int
	}{
		"not JSON": {`{`, http.StatusBadRequest},
		"too long": {`{"name":"` + strings.Repeat("n", maxConversationNameLength+1) + `"}`, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			NewHandler(nil).CreateConversation(recorder,
				resolvedRequest(http.MethodPost, "/conversations/", testCase.body, settings))
			if recorder.Code != testCase.want {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, testCase.want, recorder.Body.String())
			}
		})
	}
}

// THE LISTING'S PAGINATION HINTS ARE CLAMPED, NOT REFUSED. They are hints from
// a widget, not instructions, and the reference passes no ceiling at all — a
// client asking for `limit=100000` made the server build that page.
func TestPaginationHintsAreClamped(t *testing.T) {
	for query, want := range map[string]int{
		"":            defaultListLimit,
		"?limit=":     defaultListLimit,
		"?limit=abc":  defaultListLimit,
		"?limit=0":    1,
		"?limit=-5":   1,
		"?limit=5":    5,
		"?limit=1000": maxListLimit,
	} {
		request := httptest.NewRequest(http.MethodGet, "/conversations/"+query, nil)
		if got := boundedQueryInt(request, "limit", defaultListLimit, 1, maxListLimit); got != want {
			t.Fatalf("limit%q = %d, want %d", query, got, want)
		}
	}
}

// A PROJECT ID THAT IS NOT A PROJECT ID GIVES A SCHEMA THAT CANNOT EXIST, so
// the statement fails closed instead of reading another tenant.
func TestAnUnusableProjectIDGivesAnImpossibleSchema(t *testing.T) {
	if got := tenantSchema(7); got != `"p_7"` {
		t.Fatalf("tenantSchema(7) = %s", got)
	}
	if got := tenantSchema(-1); !strings.Contains(got, "invalid") {
		t.Fatalf("tenantSchema(-1) = %s, want an impossible schema", got)
	}
}

// AN AVATAR IS DECORATION. A missing row, a NULL and a failed query are all the
// empty string; no failure to find one costs the caller their widget.
func TestAMissingAvatarIsTheEmptyString(t *testing.T) {
	store := &store{}
	if got := store.avatar(context.Background(), 11); got != "" {
		t.Fatalf("avatar = %q, want empty", got)
	}
}

// THE OPTIONS THAT CARRY DEPENDENCIES ARE APPLIED. WithLogger ignores nil so a
// caller cannot silence the package by passing one.
func TestConstructionOptionsAreApplied(t *testing.T) {
	provisioner := refusingProvisioner{}
	handler := NewHandler(nil, WithProvisioner(provisioner), WithLogger(nil))
	if handler.store.provisioner == nil {
		t.Fatal("WithProvisioner did not reach the store")
	}
	if handler.logger == nil || handler.store.logger == nil {
		t.Fatal("a nil logger replaced the default")
	}
}

// A STORE ERROR IS A 500 AND A MISSING CONVERSATION IS A 404. "Not yours" and
// "does not exist" are ONE answer: a distinct 403 would confirm that a UUID
// names a real support conversation belonging to a real other user.
func TestConversationErrorsMapToTheirStatus(t *testing.T) {
	for name, testCase := range map[string]struct {
		err  error
		want int
	}{
		"not found": {errConversationNotFound, http.StatusNotFound},
		"broken":    {errors.New("the database is unreachable"), http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			NewHandler(nil).writeConversationError(recorder, testCase.err, "read conversation")
			if recorder.Code != testCase.want {
				t.Fatalf("status = %d, want %d", recorder.Code, testCase.want)
			}
		})
	}
}

type refusingStart struct{}

func (refusingStart) StartCurrentApplication(
	context.Context, agentexecutionapp.CurrentApplicationStartRequest,
) (agentexecutionapp.CurrentApplicationStartOutcome, error) {
	return agentexecutionapp.CurrentApplicationStartOutcome{},
		errors.New("the run must not be reached")
}
