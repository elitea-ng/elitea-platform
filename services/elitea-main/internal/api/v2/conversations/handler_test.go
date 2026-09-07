package conversations_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/contextsettings"
)

// mockRepo implements conversations.Repository for testing.
type mockRepo struct {
	listFn                    func(ctx context.Context, projectID string, page, pageSize int) (conversations.ListResponse, error)
	getFn                     func(ctx context.Context, projectID, conversationID string) (conversations.Conversation, error)
	createFn                  func(ctx context.Context, projectID string, conv conversations.Conversation) (conversations.Conversation, error)
	updateFn                  func(ctx context.Context, projectID, conversationID string, conv conversations.Conversation) (conversations.Conversation, error)
	deleteFn                  func(ctx context.Context, projectID, conversationID string) error
	listMessagesFn            func(ctx context.Context, projectID, conversationID string, query conversations.MessagesQuery) (conversations.MessagesListResponse, error)
	addParticipantFn          func(ctx context.Context, projectID, conversationID string, body map[string]any) error
	removeParticipantFn       func(ctx context.Context, projectID, conversationID, participantID string) error
	updateEntitySettingsFn    func(ctx context.Context, projectID, conversationID, participantID string, settings map[string]any) error
	batchUpdateEntitySettings func(ctx context.Context, projectID, conversationID string, settings []map[string]any) error
	selectConversationFn      func(ctx context.Context, projectID, conversationID, userID string) error
	deselectConversationFn    func(ctx context.Context, projectID, userID string) error
	createCanvasFn            func(ctx context.Context, projectID string, body map[string]any) (map[string]any, error)
	getCanvasFn               func(ctx context.Context, projectID, canvasID string) (map[string]any, error)
	updateCanvasFn            func(ctx context.Context, projectID, canvasID string, body map[string]any) error
	updateAttachmentStorageFn func(ctx context.Context, projectID, conversationID string, body map[string]any) error
	addAttachmentsFn          func(ctx context.Context, projectID, conversationID string, body map[string]any) error
	deleteAttachmentsFn       func(ctx context.Context, projectID, conversationID string) error
	getContextStateFn         func(ctx context.Context, projectID, conversationID string) (contextsettings.ConversationState, error)
	updateContextStrategyFn   func(ctx context.Context, projectID, conversationID string, strategy contextsettings.Strategy) error
	getMessageByUUIDFn        func(ctx context.Context, projectID, messageUUID string) (map[string]any, error)
	deleteMessagesFn          func(ctx context.Context, projectID, conversationID string) error
	deleteMessageFn           func(ctx context.Context, projectID, groupUID string) error
	// deleteMessageUserID records the identity the handler resolved from the
	// request context and forwarded. Deleting a message is authorised against
	// the caller, so a handler that dropped the identity would leave the
	// repository unable to apply its rules — and would look exactly like a
	// working handler to a test that only asserted the status code.
	deleteMessageUserID string
	// deleteMessageResult is what the repository reports it removed — one id
	// for a lone group, two when the reply's paired question went with it.
	deleteMessageResult []string
	// deleteMessageAttachments is what the repository reports the deleted
	// groups had stored, which the handler only acts on when the request asks
	// for it with `delete_attachment`.
	deleteMessageAttachments []conversations.AttachmentRef
	// deleteConversationAttachments is what a conversation delete reports it
	// removed rows for. The handler acts on it unconditionally — deleting the
	// conversation leaves nothing that could ever name those files again.
	deleteConversationAttachments []conversations.AttachmentRef
	listMessageGroupsFn           func(ctx context.Context, projectID, conversationID string, limit int, sortOrder string) ([]map[string]any, error)
	listParticipantsFn            func(ctx context.Context, projectID, conversationID string) ([]conversations.Participant, error)
}

func (m *mockRepo) List(ctx context.Context, projectID string, page, pageSize int) (conversations.ListResponse, error) {
	return m.listFn(ctx, projectID, page, pageSize)
}

func (m *mockRepo) Get(ctx context.Context, projectID, conversationID string) (conversations.Conversation, error) {
	return m.getFn(ctx, projectID, conversationID)
}

func (m *mockRepo) Create(ctx context.Context, projectID string, conv conversations.Conversation) (conversations.Conversation, error) {
	return m.createFn(ctx, projectID, conv)
}

func (m *mockRepo) Update(ctx context.Context, projectID, conversationID string, conv conversations.Conversation) (conversations.Conversation, error) {
	return m.updateFn(ctx, projectID, conversationID, conv)
}

// Delete reports the attachments the deleted conversation carried, the way the
// real repository does. `deleteFn` stays error-only because every existing
// caller only ever wanted to choose between success and a refusal; a nil
// `deleteFn` is a plain success, so a test that cares only about the
// attachments does not have to state one.
func (m *mockRepo) Delete(ctx context.Context, projectID, conversationID string) ([]conversations.AttachmentRef, error) {
	if m.deleteFn != nil {
		if err := m.deleteFn(ctx, projectID, conversationID); err != nil {
			// Nothing to clean up: the row delete never became a fact.
			return nil, err
		}
	}
	return m.deleteConversationAttachments, nil
}

func (m *mockRepo) ListMessages(ctx context.Context, projectID, conversationID string, query conversations.MessagesQuery) (conversations.MessagesListResponse, error) {
	return m.listMessagesFn(ctx, projectID, conversationID, query)
}

func (m *mockRepo) AddParticipant(ctx context.Context, projectID, conversationID string, body map[string]any) error {
	return m.addParticipantFn(ctx, projectID, conversationID, body)
}

func (m *mockRepo) RemoveParticipant(ctx context.Context, projectID, conversationID, participantID string) error {
	return m.removeParticipantFn(ctx, projectID, conversationID, participantID)
}

func (m *mockRepo) UpdateEntitySettings(ctx context.Context, projectID, conversationID, participantID string, settings map[string]any) error {
	return m.updateEntitySettingsFn(ctx, projectID, conversationID, participantID, settings)
}

func (m *mockRepo) BatchUpdateEntitySettings(ctx context.Context, projectID, conversationID string, settings []map[string]any) error {
	return m.batchUpdateEntitySettings(ctx, projectID, conversationID, settings)
}

func (m *mockRepo) SelectConversation(ctx context.Context, projectID, conversationID, userID string) error {
	return m.selectConversationFn(ctx, projectID, conversationID, userID)
}

func (m *mockRepo) DeselectConversation(ctx context.Context, projectID, userID string) error {
	return m.deselectConversationFn(ctx, projectID, userID)
}

func (m *mockRepo) CreateCanvas(ctx context.Context, projectID string, body map[string]any) (map[string]any, error) {
	return m.createCanvasFn(ctx, projectID, body)
}

func (m *mockRepo) GetCanvas(ctx context.Context, projectID, canvasID string) (map[string]any, error) {
	return m.getCanvasFn(ctx, projectID, canvasID)
}

// ResolveCanvas satisfies the Repository interface. The presence route
// (internal/api/v2/canvaspresence) is what calls it; no handler in this package
// does, so the double answers a fixed pair rather than taking a hook.
func (m *mockRepo) ResolveCanvas(_ context.Context, _, canvasID string) (string, string, error) {
	return canvasID, "message-group-" + canvasID, nil
}

func (m *mockRepo) UpdateCanvas(ctx context.Context, projectID, canvasID string, body map[string]any) error {
	return m.updateCanvasFn(ctx, projectID, canvasID, body)
}

func (m *mockRepo) UpdateAttachmentStorage(ctx context.Context, projectID, conversationID string, body map[string]any) error {
	return m.updateAttachmentStorageFn(ctx, projectID, conversationID, body)
}

func (m *mockRepo) AddAttachments(ctx context.Context, projectID, conversationID string, body map[string]any) error {
	return m.addAttachmentsFn(ctx, projectID, conversationID, body)
}

func (m *mockRepo) DeleteAttachments(ctx context.Context, projectID, conversationID string) error {
	return m.deleteAttachmentsFn(ctx, projectID, conversationID)
}

func (m *mockRepo) GetContextState(ctx context.Context, projectID, conversationID string) (contextsettings.ConversationState, error) {
	if m.getContextStateFn == nil {
		return contextsettings.ConversationState{}, nil
	}
	return m.getContextStateFn(ctx, projectID, conversationID)
}

func (m *mockRepo) UpdateContextStrategy(ctx context.Context, projectID, conversationID string, strategy contextsettings.Strategy) error {
	return m.updateContextStrategyFn(ctx, projectID, conversationID, strategy)
}

func (m *mockRepo) ListMessageGroups(ctx context.Context, projectID, conversationID string, limit int, sortOrder string) ([]map[string]any, error) {
	if m.listMessageGroupsFn != nil {
		return m.listMessageGroupsFn(ctx, projectID, conversationID, limit, sortOrder)
	}
	return nil, nil
}

func (m *mockRepo) ListParticipants(ctx context.Context, projectID, conversationID string) ([]conversations.Participant, error) {
	if m.listParticipantsFn != nil {
		return m.listParticipantsFn(ctx, projectID, conversationID)
	}
	return nil, nil
}

func (m *mockRepo) GetMessageByUUID(ctx context.Context, projectID, messageUUID string) (map[string]any, error) {
	if m.getMessageByUUIDFn != nil {
		return m.getMessageByUUIDFn(ctx, projectID, messageUUID)
	}
	return map[string]any{}, nil
}

func (m *mockRepo) DeleteMessages(ctx context.Context, projectID, conversationID string) error {
	if m.deleteMessagesFn != nil {
		return m.deleteMessagesFn(ctx, projectID, conversationID)
	}
	return nil
}

func (m *mockRepo) DeleteMessage(ctx context.Context, projectID, groupUID, userID string) (conversations.DeleteMessageResult, error) {
	m.deleteMessageUserID = userID
	result := conversations.DeleteMessageResult{
		Deleted:     m.deleteMessageResult,
		Attachments: m.deleteMessageAttachments,
	}
	if result.Deleted == nil {
		result.Deleted = []string{groupUID}
	}
	if m.deleteMessageFn != nil {
		return result, m.deleteMessageFn(ctx, projectID, groupUID)
	}
	return result, nil
}

// newRouter mounts the handler under /projects/{projectID}/conversations to
// give chi URL params their values.
func newRouter(h *conversations.Handler) chi.Router {
	r := chi.NewRouter()
	r.Route("/projects/{projectID}/conversations", func(r chi.Router) {
		r.Get("/", h.List)
		r.Post("/", h.Create)
		r.Get("/{conversationID}", h.Get)
		r.Put("/{conversationID}", h.Update)
		r.Delete("/{conversationID}", h.Delete)
		r.Get("/{conversationID}/messages", h.ListMessages)
		r.Delete("/{conversationID}/messages", h.DeleteMessages)
		r.Delete("/{conversationID}/messages/{messageID}", h.DeleteMessage)
		r.Post("/{conversationID}/participants", h.AddParticipant)
		r.Delete("/{conversationID}/participants/{participantID}", h.RemoveParticipant)
		r.Put("/{conversationID}/participants/{participantID}/settings", h.UpdateEntitySettings)
		r.Put("/{conversationID}/participants/settings/batch", h.BatchUpdateEntitySettings)
		r.Post("/{conversationID}/select", h.SelectConversation)
		r.Post("/deselect", h.DeselectConversation)
		r.Post("/{conversationID}/regenerate", h.Regenerate)
		r.Post("/canvas", h.CreateCanvas)
		r.Get("/canvas/{canvasID}", h.GetCanvas)
		r.Put("/canvas/{canvasID}", h.UpdateCanvas)
		r.Put("/{conversationID}/attachment-storage", h.UpdateAttachmentStorage)
		r.Post("/{conversationID}/attachments", h.AddAttachments)
		r.Delete("/{conversationID}/attachments", h.DeleteAttachments)
		r.Get("/{conversationID}/context-analytics", h.GetContextStatus)
		r.Get("/{conversationID}/context-strategy", h.GetContextStrategy)
		r.Put("/{conversationID}/context-strategy", h.UpdateContextStrategy)
	})
	return r
}

var errRepo = errors.New("repo error")

func fixedTime() time.Time {
	return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

// The List handler bypasses the Repository and queries the DB pool directly.
// In tests the pool is nil so the handler returns the graceful nil-pool response:
// HTTP 200 with {"total":0,"rows":[]}.

func TestList_Success(t *testing.T) {
	// Without a real DB pool the handler returns an empty result set gracefully.
	h := conversations.NewHandler(&mockRepo{})
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/conversations/?limit=10&offset=0", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	// Nil-pool graceful response always has total=0 and an empty rows slice.
	if total, ok := resp["total"].(float64); !ok || total != 0 {
		t.Errorf("expected total=0, got %v", resp["total"])
	}
	rows, ok := resp["rows"].([]any)
	if !ok || len(rows) != 0 {
		t.Errorf("expected rows=[], got %v", resp["rows"])
	}
}

func TestList_Error(t *testing.T) {
	// Without a real DB pool the handler always returns 200 with empty rows,
	// regardless of any repository error (the repository is never called).
	h := conversations.NewHandler(&mockRepo{})
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/conversations/", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Pool is nil → graceful 200 with empty rows, not 500.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (nil-pool graceful path), got %d", w.Code)
	}
}

func TestList_DefaultPagination(t *testing.T) {
	// The List handler uses limit/offset query params (defaults: limit=10, offset=0)
	// and queries the DB pool directly — the repository is never called.
	// With a nil pool the observable behavior is: 200 with {"total":0,"rows":[]}.
	h := conversations.NewHandler(&mockRepo{})
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/conversations/", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	// Verify the graceful default response shape (pool nil path).
	if _, ok := resp["total"]; !ok {
		t.Errorf("response missing 'total' key: %v", resp)
	}
	if _, ok := resp["rows"]; !ok {
		t.Errorf("response missing 'rows' key: %v", resp)
	}
}

// ---------------------------------------------------------------------------
// Get
// ---------------------------------------------------------------------------

func TestGet_Success(t *testing.T) {
	repo := &mockRepo{
		getFn: func(_ context.Context, projectID, conversationID string) (conversations.Conversation, error) {
			return conversations.Conversation{ID: conversationID, ProjectID: projectID, Name: "test"}, nil
		},
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/conversations/conv-1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var conv conversations.Conversation
	if err := json.NewDecoder(w.Body).Decode(&conv); err != nil {
		t.Fatal(err)
	}
	if conv.ID != "conv-1" {
		t.Errorf("expected conv-1, got %s", conv.ID)
	}
}

func TestGet_NotFound(t *testing.T) {
	repo := &mockRepo{
		getFn: func(_ context.Context, _, _ string) (conversations.Conversation, error) {
			return conversations.Conversation{}, errRepo
		},
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/conversations/conv-x", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

func TestCreate_Success(t *testing.T) {
	repo := &mockRepo{
		createFn: func(_ context.Context, projectID string, conv conversations.Conversation) (conversations.Conversation, error) {
			conv.ID = "new-conv"
			conv.ProjectID = projectID
			conv.CreatedAt = fixedTime()
			conv.UpdatedAt = fixedTime()
			return conv, nil
		},
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	body, _ := json.Marshal(conversations.Conversation{Name: "My Conv"})
	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}
	var conv conversations.Conversation
	if err := json.NewDecoder(w.Body).Decode(&conv); err != nil {
		t.Fatal(err)
	}
	if conv.ID != "new-conv" {
		t.Errorf("expected new-conv, got %s", conv.ID)
	}
}

func TestCreate_InvalidBody(t *testing.T) {
	repo := &mockRepo{}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreate_RepoError(t *testing.T) {
	repo := &mockRepo{
		createFn: func(_ context.Context, _ string, _ conversations.Conversation) (conversations.Conversation, error) {
			return conversations.Conversation{}, errRepo
		},
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	body, _ := json.Marshal(conversations.Conversation{Name: "conv"})
	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

func TestUpdate_Success(t *testing.T) {
	repo := &mockRepo{
		updateFn: func(_ context.Context, projectID, conversationID string, conv conversations.Conversation) (conversations.Conversation, error) {
			conv.ID = conversationID
			conv.ProjectID = projectID
			return conv, nil
		},
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	body, _ := json.Marshal(conversations.Conversation{Name: "Updated"})
	req := httptest.NewRequest(http.MethodPut, "/projects/proj-1/conversations/conv-1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var conv conversations.Conversation
	if err := json.NewDecoder(w.Body).Decode(&conv); err != nil {
		t.Fatal(err)
	}
	if conv.Name != "Updated" {
		t.Errorf("expected Updated, got %s", conv.Name)
	}
}

func TestUpdate_InvalidBody(t *testing.T) {
	repo := &mockRepo{}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPut, "/projects/proj-1/conversations/conv-1", bytes.NewBufferString("{bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpdate_RepoError(t *testing.T) {
	repo := &mockRepo{
		updateFn: func(_ context.Context, _, _ string, _ conversations.Conversation) (conversations.Conversation, error) {
			return conversations.Conversation{}, errRepo
		},
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	body, _ := json.Marshal(conversations.Conversation{Name: "conv"})
	req := httptest.NewRequest(http.MethodPut, "/projects/proj-1/conversations/conv-1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

func TestDelete_Success(t *testing.T) {
	repo := &mockRepo{
		deleteFn: func(_ context.Context, _, _ string) error { return nil },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/projects/proj-1/conversations/conv-1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
}

func TestDelete_Error(t *testing.T) {
	repo := &mockRepo{
		deleteFn: func(_ context.Context, _, _ string) error { return errRepo },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/projects/proj-1/conversations/conv-1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// ListMessages
// ---------------------------------------------------------------------------

func TestListMessages_Success(t *testing.T) {
	repo := &mockRepo{
		listMessagesFn: func(_ context.Context, projectID, conversationID string, query conversations.MessagesQuery) (conversations.MessagesListResponse, error) {
			return conversations.MessagesListResponse{
				Items:    []conversations.Message{{ID: "m1", ConversationID: conversationID}},
				Total:    1,
				Page:     query.Offset/query.Limit + 1,
				PageSize: query.Limit,
			}, nil
		},
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/conversations/conv-1/messages", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp conversations.MessagesListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 || resp.Items[0].ID != "m1" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

// listMessagesQuery drives one request through the router and reports the
// MessagesQuery the handler resolved from it.
//
// Every test below asserts on that value rather than on the response body,
// because the parameters are exactly what #603 lost: the old handler read
// `page`/`page_size`, the clients have always sent `limit`/`offset`, and every
// response still looked structurally fine. No server test named a wire
// parameter, so the two sides stayed internally consistent and neither could
// see the other.
func listMessagesQuery(t *testing.T, rawQuery string) conversations.MessagesQuery {
	t.Helper()
	var got conversations.MessagesQuery
	repo := &mockRepo{
		listMessagesFn: func(_ context.Context, _, _ string, query conversations.MessagesQuery) (conversations.MessagesListResponse, error) {
			got = query
			return conversations.MessagesListResponse{}, nil
		},
	}
	router := newRouter(conversations.NewHandler(repo))

	target := "/projects/proj-1/conversations/conv-1/messages"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s answered %d, want 200", target, w.Code)
	}
	return got
}

// A request that names nothing gets pylon's defaults (messages.py:73-77), not
// this server's invented page-1-of-50. The 50 was never reachable by any
// caller, so nothing depends on it.
func TestListMessages_DefaultPagination(t *testing.T) {
	got := listMessagesQuery(t, "")
	want := conversations.MessagesQuery{Limit: 10, Offset: 0, SortBy: "created_at", SortOrder: "desc"}
	if got != want {
		t.Errorf("no query string resolved to %+v, want %+v", got, want)
	}
}

// The parameter names the clients actually put on the wire
// (apps/elitea-web/src/entities/conversation/api/messageApi.ts:59-63). This is
// the assertion whose absence made #603 invisible.
func TestListMessages_ReadsLimitOffsetSortFromTheWire(t *testing.T) {
	got := listMessagesQuery(t, "limit=25&offset=75&sort_by=updated_at&sort_order=asc")
	want := conversations.MessagesQuery{Limit: 25, Offset: 75, SortBy: "updated_at", SortOrder: "asc"}
	if got != want {
		t.Errorf("limit/offset/sort request resolved to %+v, want %+v", got, want)
	}
}

// The scroll-back orchestrator (useLoadMoreMessages.ts:96) walks the offset in
// page-size steps. Each step has to reach the repository unchanged; the old
// handler dropped all of them, so every step re-served the first window.
func TestListMessages_OffsetAdvancesAcrossPages(t *testing.T) {
	for _, offset := range []int{10, 20, 30} {
		got := listMessagesQuery(t, fmt.Sprintf("limit=10&offset=%d", offset))
		if got.Offset != offset {
			t.Errorf("offset=%d resolved to Offset %d", offset, got.Offset)
		}
	}
}

// page/page_size is kept as a fallback so a caller that does send it — none
// today, but this server advertised the pair — is not broken by the fix.
func TestListMessages_PageAndPageSizeStillWorkAsAFallback(t *testing.T) {
	got := listMessagesQuery(t, "page=3&page_size=25")
	if got.Limit != 25 || got.Offset != 50 {
		t.Errorf("page=3&page_size=25 resolved to limit %d offset %d, want 25 and 50", got.Limit, got.Offset)
	}
}

// Precedence is explicit: limit/offset are the pair pylon defined and the pair
// every client sends, so they win over page/page_size when both appear.
func TestListMessages_LimitOffsetWinOverPageAndPageSize(t *testing.T) {
	got := listMessagesQuery(t, "page=3&page_size=25&limit=5&offset=7")
	if got.Limit != 5 || got.Offset != 7 {
		t.Errorf("mixed request resolved to limit %d offset %d, want 5 and 7", got.Limit, got.Offset)
	}
}

// The cap is the one the page_size branch already enforced. 100 is what the
// largest real caller asks for (usePlaybackConversation.ts:66), so it binds
// nothing that exists and bounds the per-group string_agg subquery for
// everything else.
func TestListMessages_LimitIsCapped(t *testing.T) {
	if got := listMessagesQuery(t, "limit=100"); got.Limit != 100 {
		t.Errorf("limit=100 resolved to %d, want 100 served intact", got.Limit)
	}
	if got := listMessagesQuery(t, "limit=5000"); got.Limit != 100 {
		t.Errorf("limit=5000 resolved to %d, want the 100 cap", got.Limit)
	}
}

// Junk must not silently reverse the transcript or zero the window: an
// unparseable or non-positive value leaves the documented default standing.
// (Pylon's `desc if sort_order == 'desc' else asc` did reverse it.)
func TestListMessages_RejectsUnusableParameters(t *testing.T) {
	got := listMessagesQuery(t, "limit=abc&offset=-5&sort_order=descending")
	want := conversations.MessagesQuery{Limit: 10, Offset: 0, SortBy: "created_at", SortOrder: "desc"}
	if got != want {
		t.Errorf("unusable parameters resolved to %+v, want the defaults %+v", got, want)
	}
}

func TestListMessages_Error(t *testing.T) {
	repo := &mockRepo{
		listMessagesFn: func(_ context.Context, _, _ string, _ conversations.MessagesQuery) (conversations.MessagesListResponse, error) {
			return conversations.MessagesListResponse{}, errRepo
		},
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/conversations/conv-1/messages", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// DeleteMessages (stub - no repo call)
// ---------------------------------------------------------------------------

func TestDeleteMessages_Success(t *testing.T) {
	h := conversations.NewHandler(&mockRepo{})
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/projects/proj-1/conversations/conv-1/messages", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// DeleteMessage (stub - no repo call)
// ---------------------------------------------------------------------------

// 200 with a body, not 204. One request can remove TWO groups — the reply named
// in the URL and the question it answers — and the response is the only channel
// telling the client which ones really went, so a body is the point of the
// status change rather than an incidental consequence of it.
func TestDeleteMessage_Success(t *testing.T) {
	h := conversations.NewHandler(&mockRepo{})
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/projects/proj-1/conversations/conv-1/messages/msg-1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body struct {
		Deleted []string `json:"deleted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, w.Body.String())
	}
	if len(body.Deleted) != 1 || body.Deleted[0] != "msg-1" {
		t.Fatalf("deleted is %v, want [msg-1]", body.Deleted)
	}
}

// When the repository removed the pair, both ids reach the client. A handler
// that answered a bare 204, or that echoed only the requested id, would leave
// the paired question on screen until a reload — the exact outcome pairing
// exists to prevent.
func TestDeleteMessage_ReportsThePairedGroupItRemoved(t *testing.T) {
	repo := &mockRepo{deleteMessageResult: []string{"answer-uid", "question-uid"}}
	router := newRouter(conversations.NewHandler(repo))

	req := httptest.NewRequest(http.MethodDelete, "/projects/proj-1/conversations/conv-1/messages/answer-uid", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body struct {
		Deleted []string `json:"deleted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if len(body.Deleted) != 2 || body.Deleted[0] != "answer-uid" || body.Deleted[1] != "question-uid" {
		t.Fatalf("deleted is %v, want [answer-uid question-uid]", body.Deleted)
	}
}

// ---------------------------------------------------------------------------
// AddParticipant
// ---------------------------------------------------------------------------

func TestAddParticipant_Success(t *testing.T) {
	repo := &mockRepo{
		addParticipantFn: func(_ context.Context, _, _ string, _ map[string]any) error { return nil },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	// Handler expects a JSON array of participant objects.
	body, _ := json.Marshal([]map[string]any{{"user_id": "u-1"}})
	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/conv-1/participants", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestAddParticipant_Error(t *testing.T) {
	repo := &mockRepo{
		addParticipantFn: func(_ context.Context, _, _ string, _ map[string]any) error { return errRepo },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	// Handler expects a JSON array; a single object returns 400 (decode error).
	// Send a proper array so the handler reaches the repository and returns 500.
	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/conv-1/participants", bytes.NewBufferString(`[{}]`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// RemoveParticipant
// ---------------------------------------------------------------------------

func TestRemoveParticipant_Success(t *testing.T) {
	var gotParticipantID string
	repo := &mockRepo{
		removeParticipantFn: func(_ context.Context, _, _, participantID string) error {
			gotParticipantID = participantID
			return nil
		},
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/projects/proj-1/conversations/conv-1/participants/part-1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Handler calls w.WriteHeader(http.StatusNoContent) on success.
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if gotParticipantID != "part-1" {
		t.Errorf("expected part-1, got %s", gotParticipantID)
	}
}

func TestRemoveParticipant_Error(t *testing.T) {
	repo := &mockRepo{
		removeParticipantFn: func(_ context.Context, _, _, _ string) error { return errRepo },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/projects/proj-1/conversations/conv-1/participants/part-1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// UpdateEntitySettings
// ---------------------------------------------------------------------------

func TestUpdateEntitySettings_Success(t *testing.T) {
	repo := &mockRepo{
		updateEntitySettingsFn: func(_ context.Context, _, _, _ string, _ map[string]any) error { return nil },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	body, _ := json.Marshal(map[string]any{"key": "val"})
	req := httptest.NewRequest(http.MethodPut, "/projects/proj-1/conversations/conv-1/participants/part-1/settings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestUpdateEntitySettings_Error(t *testing.T) {
	repo := &mockRepo{
		updateEntitySettingsFn: func(_ context.Context, _, _, _ string, _ map[string]any) error { return errRepo },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPut, "/projects/proj-1/conversations/conv-1/participants/part-1/settings", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// BatchUpdateEntitySettings
// ---------------------------------------------------------------------------

func TestBatchUpdateEntitySettings_Success(t *testing.T) {
	repo := &mockRepo{
		batchUpdateEntitySettings: func(_ context.Context, _, _ string, _ []map[string]any) error { return nil },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	body, _ := json.Marshal([]map[string]any{{"id": "p1", "key": "val"}})
	req := httptest.NewRequest(http.MethodPut, "/projects/proj-1/conversations/conv-1/participants/settings/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestBatchUpdateEntitySettings_Error(t *testing.T) {
	repo := &mockRepo{
		batchUpdateEntitySettings: func(_ context.Context, _, _ string, _ []map[string]any) error { return errRepo },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPut, "/projects/proj-1/conversations/conv-1/participants/settings/batch", bytes.NewBufferString("[]"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// SelectConversation
// ---------------------------------------------------------------------------

func TestSelectConversation_Success(t *testing.T) {
	var gotUserID string
	repo := &mockRepo{
		selectConversationFn: func(_ context.Context, _, _, userID string) error {
			gotUserID = userID
			return nil
		},
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/conv-1/select", nil)
	ctx := auth.ContextWithUser(req.Context(), auth.User{ID: "user-1", Email: "test@test.com"})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if gotUserID != "user-1" {
		t.Errorf("expected user-1, got %s", gotUserID)
	}
}

func TestSelectConversation_Error(t *testing.T) {
	repo := &mockRepo{
		selectConversationFn: func(_ context.Context, _, _, _ string) error { return errRepo },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/conv-1/select", nil)
	ctx := auth.ContextWithUser(req.Context(), auth.User{ID: "user-1", Email: "test@test.com"})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// DeselectConversation
// ---------------------------------------------------------------------------

func TestDeselectConversation_Success(t *testing.T) {
	var gotUserID string
	repo := &mockRepo{
		deselectConversationFn: func(_ context.Context, _, userID string) error {
			gotUserID = userID
			return nil
		},
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/deselect", nil)
	ctx := auth.ContextWithUser(req.Context(), auth.User{ID: "user-1", Email: "test@test.com"})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if gotUserID != "user-1" {
		t.Errorf("expected user-1, got %s", gotUserID)
	}
}

func TestDeselectConversation_Error(t *testing.T) {
	repo := &mockRepo{
		deselectConversationFn: func(_ context.Context, _, _ string) error { return errRepo },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/deselect", nil)
	ctx := auth.ContextWithUser(req.Context(), auth.User{ID: "user-1", Email: "test@test.com"})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Regenerate (stub - no repo call)
// ---------------------------------------------------------------------------

func TestRegenerate_Success(t *testing.T) {
	h := conversations.NewHandler(&mockRepo{})
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/conv-1/regenerate", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// CreateCanvas
// ---------------------------------------------------------------------------

func TestCreateCanvas_Success(t *testing.T) {
	repo := &mockRepo{
		createCanvasFn: func(_ context.Context, _ string, body map[string]any) (map[string]any, error) {
			body["id"] = "canvas-1"
			return body, nil
		},
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	body, _ := json.Marshal(map[string]any{"title": "My Canvas"})
	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/canvas", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Handler uses writeJSON(w, http.StatusOK, canvas) — returns 200, not 201.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result map[string]any
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["id"] != "canvas-1" {
		t.Errorf("expected canvas-1, got %v", result["id"])
	}
}

func TestCreateCanvas_Error(t *testing.T) {
	repo := &mockRepo{
		createCanvasFn: func(_ context.Context, _ string, _ map[string]any) (map[string]any, error) {
			return nil, errRepo
		},
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/canvas", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// GetCanvas
// ---------------------------------------------------------------------------

func TestGetCanvas_Success(t *testing.T) {
	repo := &mockRepo{
		getCanvasFn: func(_ context.Context, _, canvasID string) (map[string]any, error) {
			return map[string]any{"id": canvasID, "title": "Canvas"}, nil
		},
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/conversations/canvas/canvas-1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result map[string]any
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["id"] != "canvas-1" {
		t.Errorf("expected canvas-1, got %v", result["id"])
	}
}

func TestGetCanvas_Error(t *testing.T) {
	repo := &mockRepo{
		getCanvasFn: func(_ context.Context, _, _ string) (map[string]any, error) {
			return nil, errRepo
		},
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/conversations/canvas/canvas-x", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// UpdateCanvas
// ---------------------------------------------------------------------------

func TestUpdateCanvas_Success(t *testing.T) {
	repo := &mockRepo{
		updateCanvasFn: func(_ context.Context, _, _ string, _ map[string]any) error { return nil },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	body, _ := json.Marshal(map[string]any{"title": "Updated"})
	req := httptest.NewRequest(http.MethodPut, "/projects/proj-1/conversations/canvas/canvas-1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestUpdateCanvas_Error(t *testing.T) {
	repo := &mockRepo{
		updateCanvasFn: func(_ context.Context, _, _ string, _ map[string]any) error { return errRepo },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPut, "/projects/proj-1/conversations/canvas/canvas-1", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// UpdateAttachmentStorage
// ---------------------------------------------------------------------------

func TestUpdateAttachmentStorage_Success(t *testing.T) {
	repo := &mockRepo{
		updateAttachmentStorageFn: func(_ context.Context, _, _ string, _ map[string]any) error { return nil },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	body, _ := json.Marshal(map[string]any{"bucket": "s3://test"})
	req := httptest.NewRequest(http.MethodPut, "/projects/proj-1/conversations/conv-1/attachment-storage", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestUpdateAttachmentStorage_Error(t *testing.T) {
	repo := &mockRepo{
		updateAttachmentStorageFn: func(_ context.Context, _, _ string, _ map[string]any) error { return errRepo },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPut, "/projects/proj-1/conversations/conv-1/attachment-storage", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// AddAttachments
// ---------------------------------------------------------------------------

func TestAddAttachments_Success(t *testing.T) {
	repo := &mockRepo{
		addAttachmentsFn: func(_ context.Context, _, _ string, _ map[string]any) error { return nil },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	body, _ := json.Marshal(map[string]any{"files": []string{"file1.txt"}})
	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/conv-1/attachments", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestAddAttachments_Error(t *testing.T) {
	repo := &mockRepo{
		addAttachmentsFn: func(_ context.Context, _, _ string, _ map[string]any) error { return errRepo },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/conv-1/attachments", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// DeleteAttachments
// ---------------------------------------------------------------------------

func TestDeleteAttachments_Success(t *testing.T) {
	repo := &mockRepo{
		deleteAttachmentsFn: func(_ context.Context, _, _ string) error { return nil },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/projects/proj-1/conversations/conv-1/attachments", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDeleteAttachments_Error(t *testing.T) {
	repo := &mockRepo{
		deleteAttachmentsFn: func(_ context.Context, _, _ string) error { return errRepo },
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/projects/proj-1/conversations/conv-1/attachments", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Context status and context strategy
// ---------------------------------------------------------------------------

// storedStateRepo answers GetContextState with one fixed state.
func storedStateRepo(state contextsettings.ConversationState) *mockRepo {
	return &mockRepo{
		getContextStateFn: func(_ context.Context, _, _ string) (contextsettings.ConversationState, error) {
			return state, nil
		},
		updateContextStrategyFn: func(_ context.Context, _, _ string, _ contextsettings.Strategy) error { return nil },
	}
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return result
}

// A conversation with no runtime record must not report a token count. The
// previous implementation answered `message_group_count * 500`, so every
// conversation showed a fabricated budget usage.
func TestGetContextStatus_RefusesTheTokenCountItCannotCompute(t *testing.T) {
	h := conversations.NewHandler(storedStateRepo(contextsettings.ConversationState{MessageGroupsTotal: 7}))
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/conversations/conv-1/context-analytics", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	result := decodeBody(t, w)
	if result["context_analytics_available"] != false {
		t.Errorf("context_analytics_available = %v, want false", result["context_analytics_available"])
	}
	if result["unavailable_reason"] == "" || result["unavailable_reason"] == nil {
		t.Error("an unavailable status has to name its reason")
	}
	if result["current_tokens"].(float64) != 0 {
		t.Errorf("current_tokens = %v, want 0 — nothing was measured", result["current_tokens"])
	}
	// The transcript size IS knowable, and is not the same field.
	if result["message_groups_total"].(float64) != 7 {
		t.Errorf("message_groups_total = %v, want 7", result["message_groups_total"])
	}
	if result["message_groups_in_context"].(float64) != 0 {
		t.Errorf("message_groups_in_context = %v, want 0", result["message_groups_in_context"])
	}
	// The budget comes from the resolved strategy, not from a constant.
	if result["max_tokens"].(float64) != contextsettings.DefaultMaxContextTokens {
		t.Errorf("max_tokens = %v, want %d", result["max_tokens"], contextsettings.DefaultMaxContextTokens)
	}
}

// With a runtime record present the counters are served, and utilization is
// pylon's 0..1 ratio.
func TestGetContextStatus_ServesTheRecordedCounters(t *testing.T) {
	h := conversations.NewHandler(storedStateRepo(contextsettings.ConversationState{
		Strategy:  []byte(`{"max_context_tokens": 10000}`),
		Analytics: []byte(`{"current_context_tokens": 2500, "messages_in_context": 4, "summaries_generated": 2}`),
	}))
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/conversations/conv-1/context-analytics", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	result := decodeBody(t, w)
	if result["context_analytics_available"] != true {
		t.Fatalf("context_analytics_available = %v, want true", result["context_analytics_available"])
	}
	if result["current_tokens"].(float64) != 2500 {
		t.Errorf("current_tokens = %v, want 2500", result["current_tokens"])
	}
	if result["utilization"].(float64) != 0.25 {
		t.Errorf("utilization = %v, want 0.25 (a ratio, not a percentage)", result["utilization"])
	}
	if result["summary_count"].(float64) != 2 {
		t.Errorf("summary_count = %v, want 2", result["summary_count"])
	}
}

// The strategy read resolves: stored keys win, absent keys fall to the
// contract's constants rather than to zero values.
func TestGetContextStrategy_ResolvesAbsentKeysToTheContract(t *testing.T) {
	h := conversations.NewHandler(storedStateRepo(contextsettings.ConversationState{
		Strategy: []byte(`{"max_context_tokens": 8000}`),
	}))
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/conversations/conv-1/context-strategy", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	result := decodeBody(t, w)
	if result["max_context_tokens"].(float64) != 8000 {
		t.Errorf("max_context_tokens = %v, want the stored 8000", result["max_context_tokens"])
	}
	if result["preserve_recent_messages"].(float64) != contextsettings.DefaultPreserveRecentMessages {
		t.Errorf("preserve_recent_messages = %v, want the default %d",
			result["preserve_recent_messages"], contextsettings.DefaultPreserveRecentMessages)
	}
	if result["enabled"] != true {
		t.Errorf("enabled = %v, want the default true", result["enabled"])
	}
}

// The PUT is a partial update over the resolved strategy. Writing the body
// straight into the column — what this route used to do — dropped every key
// the form did not mention.
func TestUpdateContextStrategy_MergesOntoTheStoredStrategy(t *testing.T) {
	var written contextsettings.Strategy
	repo := storedStateRepo(contextsettings.ConversationState{
		Strategy: []byte(`{"max_context_tokens": 9000, "summary_llm_settings": {"model_name": "gpt-4o-mini", "max_tokens": 500}}`),
	})
	repo.updateContextStrategyFn = func(_ context.Context, _, _ string, strategy contextsettings.Strategy) error {
		written = strategy
		return nil
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPut, "/projects/proj-1/conversations/conv-1/context-strategy",
		bytes.NewBufferString(`{"preserve_recent_messages": 9}`))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if written.PreserveRecentMessages != 9 {
		t.Errorf("preserve_recent_messages stored as %d, want the submitted 9", written.PreserveRecentMessages)
	}
	if written.MaxContextTokens != 9000 {
		t.Errorf("max_context_tokens stored as %d, want the untouched 9000", written.MaxContextTokens)
	}
	if written.SummaryLLMSettings["model_name"] != "gpt-4o-mini" {
		t.Errorf("summary_llm_settings was dropped by a request that never mentioned it: %v", written.SummaryLLMSettings)
	}
	result := decodeBody(t, w)
	if result["message"] != "Strategy updated successfully" {
		t.Errorf("message = %v, want pylon's confirmation string", result["message"])
	}
	if _, ok := result["updated_strategy"]; !ok {
		t.Error("the response has to carry updated_strategy")
	}
	if _, ok := result["max_tokens"]; !ok {
		t.Error("the response has to carry the context status too")
	}
}

// Out-of-range values are refused by field name, in this API's validation
// shape.
func TestUpdateContextStrategy_RefusesOutOfRangeByField(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"below the token floor", `{"max_context_tokens": 999}`, "max_context_tokens"},
		{"zero preserved messages", `{"preserve_recent_messages": 0}`, "preserve_recent_messages"},
		{"more than 99 preserved messages", `{"preserve_recent_messages": 100}`, "preserve_recent_messages"},
		{"summary budget at least as large as the context",
			`{"max_context_tokens": 4000, "summary_llm_settings": {"max_tokens": 4000}}`,
			"summary_llm_settings.max_tokens"},
		{"summary budget under the floor",
			`{"summary_llm_settings": {"max_tokens": 10}}`, "summary_llm_settings.max_tokens"},
		{"wrong type", `{"max_context_tokens": "lots"}`, "max_context_tokens"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := conversations.NewHandler(storedStateRepo(contextsettings.ConversationState{}))
			router := newRouter(h)

			req := httptest.NewRequest(http.MethodPut,
				"/projects/proj-1/conversations/conv-1/context-strategy", bytes.NewBufferString(testCase.body))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			result := decodeBody(t, w)
			if result["field"] != testCase.field {
				t.Errorf("field = %v, want %q", result["field"], testCase.field)
			}
			if result["error"] == "" || result["error"] == nil {
				t.Error("a refusal has to say what was wrong")
			}
		})
	}
}

// An accepted edge value proves the ranges are inclusive where the contract
// says they are.
func TestUpdateContextStrategy_AcceptsTheRangeBoundaries(t *testing.T) {
	h := conversations.NewHandler(storedStateRepo(contextsettings.ConversationState{}))
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPut, "/projects/proj-1/conversations/conv-1/context-strategy",
		bytes.NewBufferString(`{"max_context_tokens": 1000, "preserve_recent_messages": 99}`))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 at the inclusive boundaries, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateContextStrategy_Error(t *testing.T) {
	repo := storedStateRepo(contextsettings.ConversationState{})
	repo.updateContextStrategyFn = func(_ context.Context, _, _ string, _ contextsettings.Strategy) error { return errRepo }
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPut, "/projects/proj-1/conversations/conv-1/context-strategy", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// The user-defaults tier: a conversation with no stored strategy resolves to
// the CALLER's Settings > Memory defaults before it falls back to constants.
func TestContextStrategy_FallsBackToTheCallersUserDefaults(t *testing.T) {
	maxTokens := 12000
	h := conversations.NewHandler(storedStateRepo(contextsettings.ConversationState{})).
		WithUserContextDefaults(stubUserDefaults{defaults: contextsettings.UserDefaults{
			ContextManagement: &contextsettings.ContextManagement{MaxContextTokens: &maxTokens},
		}})
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/conversations/conv-1/context-strategy", nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "7", UserID: "7"}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	result := decodeBody(t, w)
	if result["max_context_tokens"].(float64) != 12000 {
		t.Errorf("max_context_tokens = %v, want the user default 12000", result["max_context_tokens"])
	}
}

// A stored per-conversation value beats the user default — the outer tier of
// the resolution rule.
func TestContextStrategy_ConversationBeatsUserDefaults(t *testing.T) {
	maxTokens := 12000
	h := conversations.NewHandler(storedStateRepo(contextsettings.ConversationState{
		Strategy: []byte(`{"max_context_tokens": 3000}`),
	})).WithUserContextDefaults(stubUserDefaults{defaults: contextsettings.UserDefaults{
		ContextManagement: &contextsettings.ContextManagement{MaxContextTokens: &maxTokens},
	}})
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/conversations/conv-1/context-strategy", nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "7", UserID: "7"}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	result := decodeBody(t, w)
	if result["max_context_tokens"].(float64) != 3000 {
		t.Errorf("max_context_tokens = %v, want the conversation's own 3000", result["max_context_tokens"])
	}
}

// The wire, not just the struct: a conversation with no summary model must
// serve `"summary_llm_settings":null`. The Rust runtime refuses a non-null
// value (it names a second model whose credential the claim does not carry),
// and `{}` is a non-null value.
func TestContextStrategy_ServesANullSummaryModelNotAnEmptyObject(t *testing.T) {
	for name, stored := range map[string][]byte{
		"nothing stored":     nil,
		"an empty object":    []byte(`{"summary_llm_settings": {}}`),
		"cleared by a write": []byte(`{"summary_llm_settings": null}`),
	} {
		t.Run(name, func(t *testing.T) {
			h := conversations.NewHandler(storedStateRepo(contextsettings.ConversationState{Strategy: stored}))
			router := newRouter(h)

			req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/conversations/conv-1/context-strategy", nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			body := w.Body.String()
			if !strings.Contains(body, `"summary_llm_settings":null`) {
				t.Fatalf("response does not carry a null summary_llm_settings: %s", body)
			}
			if strings.Contains(body, `"summary_llm_settings":{}`) {
				t.Fatalf("response carries an EMPTY OBJECT summary_llm_settings, which the runtime "+
					"refuses with UnsupportedCapability: %s", body)
			}
		})
	}
}

// And the same on the write path's echo, which is what a client re-submits.
func TestUpdateContextStrategy_StoresANullSummaryModelNotAnEmptyObject(t *testing.T) {
	var written contextsettings.Strategy
	repo := storedStateRepo(contextsettings.ConversationState{})
	repo.updateContextStrategyFn = func(_ context.Context, _, _ string, strategy contextsettings.Strategy) error {
		written = strategy
		return nil
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	req := httptest.NewRequest(http.MethodPut, "/projects/proj-1/conversations/conv-1/context-strategy",
		bytes.NewBufferString(`{"summary_llm_settings": {}}`))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if written.SummaryLLMSettings != nil {
		t.Fatalf("stored SummaryLLMSettings is %#v, want nil", written.SummaryLLMSettings)
	}
	if strings.Contains(w.Body.String(), `"summary_llm_settings":{}`) {
		t.Fatalf("the echoed strategy carries an empty object: %s", w.Body.String())
	}
}

type stubUserDefaults struct {
	defaults contextsettings.UserDefaults
}

func (s stubUserDefaults) ContextDefaults(_ context.Context, _ int64) (contextsettings.UserDefaults, error) {
	return s.defaults, nil
}

// ---------------------------------------------------------------------------
// The `query` wire parameter, and the identity DeleteMessage forwards
// ---------------------------------------------------------------------------

// The free-text search term has to survive the handler. It reaches the
// repository raw — escaping it here would bake one storage layer's pattern
// syntax into the HTTP boundary.
func TestListMessages_ReadsQueryFromTheWire(t *testing.T) {
	got := listMessagesQuery(t, "query=50%25+off")
	if got.Query != "50% off" {
		t.Fatalf("Query is %q, want %q — the term was dropped or mangled in transit", got.Query, "50% off")
	}
}

// An explicit `query=` is the same as sending nothing, which is what pylon's
// truthiness check did. The distinction matters: as a pattern the empty string
// matches every group, so a cleared search box would look like a working filter
// returning everything rather than a filter that is off.
func TestListMessages_EmptyQueryParameterIsNoFilter(t *testing.T) {
	for _, rawQuery := range []string{"", "query="} {
		if got := listMessagesQuery(t, rawQuery); got.Query != "" {
			t.Errorf("%q resolved to Query %q, want empty", rawQuery, got.Query)
		}
	}
}

// DeleteMessage is authorised against the caller, so the handler must resolve
// the identity and hand it to the repository. A handler that dropped it would
// still answer 204 here — which is exactly why this asserts the forwarded
// value rather than the status code.
func TestDeleteMessage_ForwardsTheCallerIdentity(t *testing.T) {
	repo := &mockRepo{}
	router := newRouter(conversations.NewHandler(repo))

	req := httptest.NewRequest(http.MethodDelete, "/projects/proj-1/conversations/conv-1/messages/msg-1", nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "user-1", Email: "test@test.com"}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if repo.deleteMessageUserID != "user-1" {
		t.Fatalf("the repository was given caller %q, want %q", repo.deleteMessageUserID, "user-1")
	}
}

// With no user on the context the handler forwards an empty id rather than
// inventing one. The repository refuses that, so the route fails closed.
func TestDeleteMessage_ForwardsAnEmptyIdentityWhenUnauthenticated(t *testing.T) {
	repo := &mockRepo{}
	router := newRouter(conversations.NewHandler(repo))

	req := httptest.NewRequest(http.MethodDelete, "/projects/proj-1/conversations/conv-1/messages/msg-1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if repo.deleteMessageUserID != "" {
		t.Fatalf("an unauthenticated request forwarded caller %q, want empty", repo.deleteMessageUserID)
	}
}
