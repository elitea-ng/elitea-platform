package canvaspresence_test

// #622 — canvas presence over the project SSE plane.
//
// The test that MATTERS is TestPresenceOfOneProjectNeverReachesAnother. Canvas
// ids are per-tenant-schema integers, so "canvas 7" exists in nearly every
// project; the deleted socket prototype's room was `"canvas:" + canvasID` with
// no project component, which made canvas 7 in project 1 and canvas 7 in
// project 7 THE SAME ROOM. Every other test here would have passed against that
// prototype.
//
// RED BEFORE GREEN, recorded rather than claimed:
//   - TestPresenceOfOneProjectNeverReachesAnother fails if the channel is built
//     from anything but the resolved mount segment; replacing
//     events.ProjectChannel(projectID) with a constant makes both projects
//     publish to one channel and the subscriber count assertion fails.
//   - TestHeartbeatRefusesACanvasOutsideTheProject fails if the resolver call is
//     dropped: the handler then answers 200 and publishes for an id that means
//     nothing in this schema.
//   - TestHeartbeatFailsClosedWithoutThePermission fails if the route is
//     registered without projectPermission: it answers 200.
//   - TestExpiredEditorLeavesTheRoster fails if the store expires the KEY rather
//     than each entry (the reference's design): the stale editor stays.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	v2canvaspresence "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/canvaspresence"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/events"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// errRosterUnavailable stands in for a Redis that is down.
var errRosterUnavailable = errors.New("roster store unavailable")

// ---------------------------------------------------------------------------
// doubles
// ---------------------------------------------------------------------------

// schemaResolver answers only for canvases that exist in the named project's
// schema, which is exactly what repos.ResolveCanvas does with
// `FROM "p_<projectID>".chat_message_items`. The map is keyed
// project -> canvas id, so the same id can exist in two projects and mean two
// different canvases — the condition the cross-project test needs.
type schemaResolver struct {
	canvases map[string]map[string]string // project -> canvasID -> canvas uuid
	asked    []string
}

func (r *schemaResolver) ResolveCanvas(_ context.Context, projectID, canvasID string) (string, string, error) {
	r.asked = append(r.asked, projectID+"/"+canvasID)
	inProject, ok := r.canvases[projectID]
	if !ok {
		return "", "", apierr.NotFound("canvas not found")
	}
	canvasUUID, ok := inProject[canvasID]
	if !ok {
		return "", "", apierr.NotFound("canvas not found")
	}
	return canvasUUID, "group-" + canvasUUID, nil
}

// recordingBus is an events.Bus that keeps every publish with its CHANNEL, so a
// test can assert which channel a payload landed on rather than only that a
// publish happened.
type recordingBus struct {
	published []publishedEvent
}

type publishedEvent struct {
	channel   string
	eventType string
	payload   any
}

func (b *recordingBus) Publish(_ context.Context, channel, eventType string, payload interface{}) error {
	b.published = append(b.published, publishedEvent{channel: channel, eventType: eventType, payload: payload})
	return nil
}

// onChannel returns the events this bus saw on one channel.
func (b *recordingBus) onChannel(channel string) []publishedEvent {
	var matched []publishedEvent
	for _, event := range b.published {
		if event.channel == channel {
			matched = append(matched, event)
		}
	}
	return matched
}

// principal is the authenticated caller. RuntimePrincipalFromContext demands a
// server-derived provenance marker, so a test that only calls ContextWithUser
// would get a 401 and prove nothing.
func withPrincipal(r *http.Request, id, name string) *http.Request {
	ctx := auth.ContextWithAuthenticatedUser(
		r.Context(),
		auth.User{ID: id, Name: name, Email: name},
		auth.AuthenticationSourceSession,
	)
	return r.WithContext(ctx)
}

// route mounts the handler on the real path shape so chi supplies {projectID}
// and {canvasID} exactly as router.go does.
func route(handler *v2canvaspresence.Handler) chi.Router {
	r := chi.NewRouter()
	r.Post("/canvas/prompt_lib/{projectID}/{canvasID}/presence", handler.Heartbeat)
	return r
}

func post(t *testing.T, router chi.Router, path, body, userID, userName string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if userID != "" {
		request = withPrincipal(request, userID, userName)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeResponse(t *testing.T, recorder *httptest.ResponseRecorder) v2canvaspresence.Response {
	t.Helper()
	var response v2canvaspresence.Response
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v (body %q)", err, recorder.Body.String())
	}
	return response
}

func twoProjectResolver() *schemaResolver {
	return &schemaResolver{canvases: map[string]map[string]string{
		"7": {"1": "aaaaaaaa-0000-4000-8000-000000000001"},
		"8": {"1": "bbbbbbbb-0000-4000-8000-000000000002"},
	}}
}

// ---------------------------------------------------------------------------
// join
// ---------------------------------------------------------------------------

func TestHeartbeatPublishesTheRosterOnTheProjectChannel(t *testing.T) {
	bus := &recordingBus{}
	handler := v2canvaspresence.NewHandler(
		twoProjectResolver(),
		v2canvaspresence.WithEmitter(events.NewPublisher(bus)),
	)

	recorder := post(t, route(handler), "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`, "42", "ada@example.com")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", recorder.Code, recorder.Body.String())
	}

	response := decodeResponse(t, recorder)
	if len(response.Editors) != 1 {
		t.Fatalf("editors = %#v, want exactly the caller", response.Editors)
	}
	editor := response.Editors[0]
	if editor.UserID != "42" || editor.UserName != "ada@example.com" || editor.State != v2canvaspresence.StateEditing {
		t.Fatalf("editor = %#v, want the server-derived principal in the editing state", editor)
	}
	if response.CanvasUUID != "aaaaaaaa-0000-4000-8000-000000000001" {
		t.Fatalf("canvas_uuid = %q, want the uuid the resolver read out of the project's schema", response.CanvasUUID)
	}
	if response.MessageGroupUUID != "group-aaaaaaaa-0000-4000-8000-000000000001" {
		t.Fatalf("message_group_uuid = %q, want the resolver's", response.MessageGroupUUID)
	}
	if response.TTLSeconds != int(v2canvaspresence.TTL/time.Second) {
		t.Fatalf("ttl_seconds = %d, want %d", response.TTLSeconds, int(v2canvaspresence.TTL/time.Second))
	}

	channel := events.ProjectChannel("7")
	published := bus.onChannel(channel)
	if len(published) != 1 {
		t.Fatalf("published on %s = %d events, want 1 (all: %#v)", channel, len(published), bus.published)
	}
	if published[0].eventType != v2canvaspresence.EventType {
		t.Fatalf("event type = %q, want %q", published[0].eventType, v2canvaspresence.EventType)
	}
	payload, ok := published[0].payload.(v2canvaspresence.Response)
	if !ok {
		t.Fatalf("payload type = %T, want the same Response shape the POST answers with", published[0].payload)
	}
	if payload.EntityType != "canvas" || payload.Action != "editors" || payload.ProjectID != "7" {
		t.Fatalf("payload envelope = %#v, want the canvas/editors DomainEvent fields for project 7", payload)
	}
	if len(payload.Editors) != 1 || payload.Editors[0].UserName != "ada@example.com" {
		t.Fatalf("payload editors = %#v, want the roster", payload.Editors)
	}
}

func TestTwoCallersSeeEachOtherOnOneCanvas(t *testing.T) {
	handler := v2canvaspresence.NewHandler(twoProjectResolver())
	router := route(handler)

	post(t, router, "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`, "1", "ada@example.com")
	second := post(t, router, "/canvas/prompt_lib/7/1/presence", `{"state":"viewing"}`, "2", "grace@example.com")

	response := decodeResponse(t, second)
	if len(response.Editors) != 2 {
		t.Fatalf("editors = %#v, want both callers", response.Editors)
	}
	if response.Editors[0].UserID != "1" || response.Editors[1].UserID != "2" {
		t.Fatalf("editors = %#v, want a stable order by user id", response.Editors)
	}
}

// The sentinel accounts the reference filters in three places in the SPA. The
// filter is server-side here as well, because the roster is also a REST answer.
func TestServiceAccountsAreNotEditors(t *testing.T) {
	handler := v2canvaspresence.NewHandler(twoProjectResolver())
	router := route(handler)

	post(t, router, "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`, "1", "system@centry.user")
	recorder := post(t, router, "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`, "2", "admin@centry.user")

	response := decodeResponse(t, recorder)
	if len(response.Editors) != 0 {
		t.Fatalf("editors = %#v, want the two service principals suppressed", response.Editors)
	}
	if !strings.Contains(recorder.Body.String(), `"editors":[]`) {
		t.Fatalf("body = %s; an empty roster must serialise as [] so a client can tell it from an unanswered field", recorder.Body.String())
	}
}

// ---------------------------------------------------------------------------
// leave and expiry
// ---------------------------------------------------------------------------

func TestLeaveRemovesTheEditorImmediately(t *testing.T) {
	handler := v2canvaspresence.NewHandler(twoProjectResolver())
	router := route(handler)

	post(t, router, "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`, "1", "ada@example.com")
	post(t, router, "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`, "2", "grace@example.com")

	// The reference could not do this: its leave path only deletes the roster
	// when the leaver is the LAST editor, so with two people editing the first
	// to close a tab stays present until the whole key expires.
	left := post(t, router, "/canvas/prompt_lib/7/1/presence", `{"state":"left"}`, "1", "ada@example.com")
	response := decodeResponse(t, left)
	if len(response.Editors) != 1 || response.Editors[0].UserID != "2" {
		t.Fatalf("editors after leave = %#v, want only the caller who stayed", response.Editors)
	}
}

func TestExpiredEditorLeavesTheRoster(t *testing.T) {
	store := v2canvaspresence.NewMemoryStore()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store.SetClock(clock)

	handler := v2canvaspresence.NewHandler(
		twoProjectResolver(),
		v2canvaspresence.WithStore(store),
		v2canvaspresence.WithClock(clock),
	)
	router := route(handler)

	post(t, router, "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`, "1", "ada@example.com")

	// One TTL and one second later, the tab has died without ever sending its
	// leave. Nothing sweeps; the next read is what drops it.
	now = now.Add(v2canvaspresence.TTL + time.Second)

	recorder := post(t, router, "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`, "2", "grace@example.com")
	response := decodeResponse(t, recorder)
	if len(response.Editors) != 1 || response.Editors[0].UserID != "2" {
		t.Fatalf("editors = %#v, want the expired tab gone and the live one kept", response.Editors)
	}
}

// ---------------------------------------------------------------------------
// the cross-project negative
// ---------------------------------------------------------------------------

func TestPresenceOfOneProjectNeverReachesAnother(t *testing.T) {
	bus := &recordingBus{}
	handler := v2canvaspresence.NewHandler(
		twoProjectResolver(),
		v2canvaspresence.WithEmitter(events.NewPublisher(bus)),
	)
	router := route(handler)

	// The SAME canvas id in two projects. Both resolve, because the id is a
	// per-schema integer and both schemas have one.
	post(t, router, "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`, "1", "ada@example.com")
	post(t, router, "/canvas/prompt_lib/8/1/presence", `{"state":"editing"}`, "2", "mallory@example.com")

	sevens := bus.onChannel(events.ProjectChannel("7"))
	eights := bus.onChannel(events.ProjectChannel("8"))
	if len(sevens) != 1 || len(eights) != 1 {
		t.Fatalf("channels 7/8 saw %d/%d events, want 1 each — one channel per project (all: %#v)", len(sevens), len(eights), bus.published)
	}

	// A subscriber of project 7 must never learn about project 8's editor.
	sevenPayload := sevens[0].payload.(v2canvaspresence.Response)
	for _, editor := range sevenPayload.Editors {
		if editor.UserName == "mallory@example.com" {
			t.Fatalf("project 7's channel carried project 8's editor: %#v", sevenPayload.Editors)
		}
	}
	if sevenPayload.CanvasUUID == eights[0].payload.(v2canvaspresence.Response).CanvasUUID {
		t.Fatal("both projects published the same canvas uuid; the id was not resolved inside the project's own schema")
	}

	// And the rosters themselves are separate keys, not one shared room.
	back := post(t, router, "/canvas/prompt_lib/7/1/presence", `{"state":"viewing"}`, "1", "ada@example.com")
	response := decodeResponse(t, back)
	if len(response.Editors) != 1 {
		t.Fatalf("project 7's roster = %#v, want only project 7's editor", response.Editors)
	}
}

func TestHeartbeatRefusesACanvasOutsideTheProject(t *testing.T) {
	resolver := twoProjectResolver()
	bus := &recordingBus{}
	handler := v2canvaspresence.NewHandler(resolver, v2canvaspresence.WithEmitter(events.NewPublisher(bus)))

	// Canvas 99 exists in neither project.
	recorder := post(t, route(handler), "/canvas/prompt_lib/7/99/presence", `{"state":"editing"}`, "1", "ada@example.com")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a canvas that does not resolve inside the project's schema", recorder.Code)
	}
	if len(bus.published) != 0 {
		t.Fatalf("published %#v; nothing may be published for an unresolved canvas", bus.published)
	}
	if len(resolver.asked) != 1 || resolver.asked[0] != "7/99" {
		t.Fatalf("resolver asked %#v, want exactly the project and canvas from the mount pattern", resolver.asked)
	}
}

// ---------------------------------------------------------------------------
// the gate
// ---------------------------------------------------------------------------

// TestHeartbeatFailsClosedWithoutThePermission mounts the route through the
// SAME middleware router.go composes it with — apimw.RequireResolvedPermissions
// on the shared project resolver — with a nil resolver, which is the fail-closed
// state that middleware is built around.
func TestHeartbeatFailsClosedWithoutThePermission(t *testing.T) {
	bus := &recordingBus{}
	handler := v2canvaspresence.NewHandler(twoProjectResolver(), v2canvaspresence.WithEmitter(events.NewPublisher(bus)))

	router := chi.NewRouter()
	router.With(apimw.RequireResolvedPermissions(nil, auth.PermissionModeDefault, v2canvaspresence.Permission)).
		Post("/canvas/prompt_lib/{projectID}/{canvasID}/presence", handler.Heartbeat)

	recorder := post(t, router, "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`, "1", "ada@example.com")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 from a nil permission resolver", recorder.Code)
	}
	if len(bus.published) != 0 {
		t.Fatalf("published %#v; a refused heartbeat must publish nothing", bus.published)
	}
}

func TestHeartbeatRefusesAnUnauthenticatedCaller(t *testing.T) {
	handler := v2canvaspresence.NewHandler(twoProjectResolver())
	recorder := post(t, route(handler), "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`, "", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when no server-derived principal is in the context", recorder.Code)
	}
}

// ---------------------------------------------------------------------------
// the body
// ---------------------------------------------------------------------------

func TestTheBodyCannotNameTheProjectOrTheCanvas(t *testing.T) {
	bus := &recordingBus{}
	handler := v2canvaspresence.NewHandler(twoProjectResolver(), v2canvaspresence.WithEmitter(events.NewPublisher(bus)))

	// Every field the prototype trusted, sent at once.
	body := `{"state":"editing","project_id":"8","canvas_uuid":"bbbbbbbb-0000-4000-8000-000000000002",` +
		`"user_id":"999","user_name":"mallory@example.com","message_group_uuid":"forged"}`
	recorder := post(t, route(handler), "/canvas/prompt_lib/7/1/presence", body, "1", "ada@example.com")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", recorder.Code, recorder.Body.String())
	}

	response := decodeResponse(t, recorder)
	if response.ProjectID != "7" {
		t.Fatalf("project_id = %q; the body must not be able to move the event to another project", response.ProjectID)
	}
	if response.CanvasUUID != "aaaaaaaa-0000-4000-8000-000000000001" {
		t.Fatalf("canvas_uuid = %q; the body must not be able to name the canvas", response.CanvasUUID)
	}
	if response.MessageGroupUUID == "forged" {
		t.Fatal("message_group_uuid was echoed from the body; it must come from the resolver")
	}
	if response.Editors[0].UserID != "1" || response.Editors[0].UserName != "ada@example.com" {
		t.Fatalf("editor = %#v; identity must come from the authenticated principal", response.Editors[0])
	}
	if bus.published[0].channel != events.ProjectChannel("7") {
		t.Fatalf("channel = %q, want project 7's", bus.published[0].channel)
	}
}

func TestAnEmptyBodyIsAViewingBeat(t *testing.T) {
	handler := v2canvaspresence.NewHandler(twoProjectResolver())
	recorder := post(t, route(handler), "/canvas/prompt_lib/7/1/presence", "", "1", "ada@example.com")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	response := decodeResponse(t, recorder)
	if len(response.Editors) != 1 || response.Editors[0].State != v2canvaspresence.StateViewing {
		t.Fatalf("editors = %#v, want one viewing beat", response.Editors)
	}
}

func TestAnUnknownStateIsRefused(t *testing.T) {
	handler := v2canvaspresence.NewHandler(twoProjectResolver())
	recorder := post(t, route(handler), "/canvas/prompt_lib/7/1/presence", `{"state":"locking"}`, "1", "ada@example.com")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a state outside the vocabulary", recorder.Code)
	}
}

func TestAnOversizedBodyIsRefused(t *testing.T) {
	handler := v2canvaspresence.NewHandler(twoProjectResolver())
	body := `{"state":"editing","message_group_uuid":"` + strings.Repeat("x", 8<<10) + `"}`
	recorder := post(t, route(handler), "/canvas/prompt_lib/7/1/presence", body, "1", "ada@example.com")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an oversized body", recorder.Code)
	}
}

// A handler built with no resolver must refuse rather than publish presence for
// an id nothing has checked.
func TestAHandlerWithNoResolverRefuses(t *testing.T) {
	bus := &recordingBus{}
	handler := v2canvaspresence.NewHandler(nil, v2canvaspresence.WithEmitter(events.NewPublisher(bus)))
	recorder := post(t, route(handler), "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`, "1", "ada@example.com")
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when no canvas resolver is composed", recorder.Code)
	}
	if len(bus.published) != 0 {
		t.Fatalf("published %#v; nothing may be published without a resolver", bus.published)
	}
}

// ---------------------------------------------------------------------------
// identity fallbacks and store failures
// ---------------------------------------------------------------------------

// postAs is post's sibling for a principal whose display name and e-mail differ,
// which is what the fallback ladder in principalName is about.
func postAs(t *testing.T, router chi.Router, path, body string, user auth.User) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request = request.WithContext(auth.ContextWithAuthenticatedUser(request.Context(), user, auth.AuthenticationSourceSession))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestTheEditorNameFallsBackToEmailThenToTheID(t *testing.T) {
	router := route(v2canvaspresence.NewHandler(twoProjectResolver()))

	named := postAs(t, router, "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`,
		auth.User{ID: "1", Name: "  ", Email: "ada@example.com"})
	if got := decodeResponse(t, named).Editors[0].UserName; got != "ada@example.com" {
		t.Fatalf("user_name = %q, want the e-mail when the display name is blank", got)
	}

	anonymous := postAs(t, router, "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`,
		auth.User{ID: "9"})
	roster := decodeResponse(t, anonymous).Editors
	var nameless string
	for _, editor := range roster {
		if editor.UserID == "9" {
			nameless = editor.UserName
		}
	}
	if nameless != "9" {
		t.Fatalf("user_name = %q, want the principal id so no entry is nameless", nameless)
	}
}

func TestAMalformedBodyIsRefused(t *testing.T) {
	handler := v2canvaspresence.NewHandler(twoProjectResolver())
	recorder := post(t, route(handler), "/canvas/prompt_lib/7/1/presence", `{"state":`, "1", "ada@example.com")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a body that is not a JSON object", recorder.Code)
	}
}

// failingStore is the roster backend refusing every call. A presence route that
// answered 200 over a store that stored nothing would be the #130/#180 class:
// a handler that answers and writes nothing.
type failingStore struct{ err error }

func (s failingStore) Touch(context.Context, string, v2canvaspresence.Editor, time.Duration) error {
	return s.err
}
func (s failingStore) Remove(context.Context, string, string) error { return s.err }
func (s failingStore) List(context.Context, string) ([]v2canvaspresence.Editor, error) {
	return nil, s.err
}

// listOnlyFailingStore accepts writes and refuses the read, so the read arm's
// own refusal is exercised rather than being shadowed by the write's.
type listOnlyFailingStore struct{ err error }

func (s listOnlyFailingStore) Touch(context.Context, string, v2canvaspresence.Editor, time.Duration) error {
	return nil
}
func (s listOnlyFailingStore) Remove(context.Context, string, string) error { return nil }
func (s listOnlyFailingStore) List(context.Context, string) ([]v2canvaspresence.Editor, error) {
	return nil, s.err
}

func TestAStoreFailureIsRefusedRatherThanAnsweredEmpty(t *testing.T) {
	failure := errRosterUnavailable
	bus := &recordingBus{}

	cases := []struct {
		name  string
		store v2canvaspresence.Store
		body  string
	}{
		{"touch", failingStore{err: failure}, `{"state":"editing"}`},
		{"remove", failingStore{err: failure}, `{"state":"left"}`},
		{"list", listOnlyFailingStore{err: failure}, `{"state":"editing"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handler := v2canvaspresence.NewHandler(
				twoProjectResolver(),
				v2canvaspresence.WithStore(testCase.store),
				v2canvaspresence.WithEmitter(events.NewPublisher(bus)),
			)
			recorder := post(t, route(handler), "/canvas/prompt_lib/7/1/presence", testCase.body, "1", "ada@example.com")
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 when the roster store refuses", recorder.Code)
			}
		})
	}
	if len(bus.published) != 0 {
		t.Fatalf("published %#v; a refused heartbeat must publish nothing", bus.published)
	}
}

// The nil-tolerant options are called unconditionally by router.go, so their
// no-op arms are part of the contract rather than defensive filler.
func TestNilOptionsLeaveTheDefaultsInPlace(t *testing.T) {
	handler := v2canvaspresence.NewHandler(
		twoProjectResolver(),
		v2canvaspresence.WithStore(nil),
		v2canvaspresence.WithEmitter(nil),
		v2canvaspresence.WithClock(nil),
	)
	recorder := post(t, route(handler), "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`, "1", "ada@example.com")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the in-process defaults", recorder.Code)
	}
}
