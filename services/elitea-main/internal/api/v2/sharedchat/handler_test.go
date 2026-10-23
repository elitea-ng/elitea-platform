package sharedchat_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/sharedchat"
)

// fakeStore is an in-memory Store. It applies the SAME revocation and expiry
// filter the SQL does, in the same place — inside the resolve — because the
// contract under test is "these three cases are one answer", and a double that
// distinguished them would let the handler pass while production leaked.
type fakeStore struct {
	links    map[string]*storedLink
	now      func() time.Time
	created  []sharedchat.CreateInput
	revoked  []int64
	accesses int
	failNext bool
}

type storedLink struct {
	id           int64
	projectID    string
	conversation int64
	scope        string
	groupIDs     []int64
	passwordHash []byte
	passwordSalt []byte
	expiresAt    time.Time
	revoked      bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{links: map[string]*storedLink{}, now: time.Now}
}

func (f *fakeStore) key(hash []byte) string { return string(hash) }

func (f *fakeStore) Create(_ context.Context, in sharedchat.CreateInput) (sharedchat.Link, error) {
	f.created = append(f.created, in)
	id := int64(len(f.created))
	f.links[f.key(in.TokenHash)] = &storedLink{
		id: id, projectID: in.ProjectID, conversation: in.ConversationID,
		scope: in.Scope, groupIDs: in.MessageGroupIDs,
		passwordHash: in.PasswordHash, passwordSalt: in.PasswordSalt,
		expiresAt: in.ExpiresAt,
	}
	return sharedchat.Link{
		ID: id, Scope: in.Scope, HasPassword: len(in.PasswordHash) > 0,
		ExpiresAt: in.ExpiresAt, CreatedAt: f.now(), Active: true,
	}, nil
}

func (f *fakeStore) ListByConversation(context.Context, string, int64) ([]sharedchat.Link, error) {
	out := []sharedchat.Link{}
	for _, l := range f.links {
		out = append(out, sharedchat.Link{ID: l.id, Scope: l.scope, ExpiresAt: l.expiresAt})
	}
	return out, nil
}

func (f *fakeStore) Revoke(_ context.Context, projectID string, conversationID, linkID int64) error {
	for _, l := range f.links {
		if l.id == linkID && l.projectID == projectID && l.conversation == conversationID && !l.revoked {
			l.revoked = true
			f.revoked = append(f.revoked, linkID)
			return nil
		}
	}
	return sharedchat.ErrNoLink
}

func (f *fakeStore) ResolveByTokenHash(_ context.Context, hash []byte) (sharedchat.Resolved, error) {
	l, ok := f.links[f.key(hash)]
	if !ok || l.revoked || !l.expiresAt.After(f.now()) {
		return sharedchat.Resolved{}, sharedchat.ErrNoLink
	}
	return sharedchat.Resolved{
		ID: l.id, ProjectID: l.projectID, ConversationID: l.conversation,
		Scope: l.scope, MessageGroupIDs: l.groupIDs,
		PasswordHash: l.passwordHash, PasswordSalt: l.passwordSalt,
		ExpiresAt: l.expiresAt,
	}, nil
}

func (f *fakeStore) RecordAccess(context.Context, int64) error {
	f.accesses++
	if f.failNext {
		f.failNext = false
		return context.DeadlineExceeded
	}
	return nil
}

type fakeTranscript struct {
	name       string
	messages   []sharedchat.SharedMessage
	sawGroups  []int64
	sawProject string
}

func (f *fakeTranscript) SharedTranscript(_ context.Context, projectID string, _ int64, groupIDs []int64) (string, []sharedchat.SharedMessage, error) {
	f.sawGroups = groupIDs
	f.sawProject = projectID
	return f.name, f.messages, nil
}

func newRouter(h *sharedchat.Handler) chi.Router {
	r := chi.NewRouter()
	r.Get("/api/v2/elitea_core/shared_chat_links/prompt_lib/{projectID}/{conversationID}", h.List)
	r.Post("/api/v2/elitea_core/shared_chat_links/prompt_lib/{projectID}/{conversationID}", h.Create)
	r.Delete("/api/v2/elitea_core/shared_chat_link/prompt_lib/{projectID}/{conversationID}/{linkID}", h.Revoke)
	r.Get("/api/v2/elitea_core/shared_chat_view/prompt_lib/{token}", h.View)
	r.Post("/api/v2/elitea_core/shared_chat_view_unlock/prompt_lib/{token}/unlock", h.Unlock)
	return r
}

func createLink(t *testing.T, router chi.Router, body string) (token string, id int64) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/elitea_core/shared_chat_links/prompt_lib/7/42", strings.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Token string `json:"token"`
		ID    int64  `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Token, out.ID
}

func view(router chi.Router, token string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/shared_chat_view/prompt_lib/"+token, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func unlock(router chi.Router, token, password string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"password": password})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v2/elitea_core/shared_chat_view_unlock/prompt_lib/"+token+"/unlock", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// -------------------------------------------------------------- happy path

func TestCreateThenViewReturnsTheTranscript(t *testing.T) {
	store := newFakeStore()
	transcript := &fakeTranscript{
		name: "Quarterly plan",
		messages: []sharedchat.SharedMessage{
			{AuthorType: "user", AuthorName: "Ada", Items: []sharedchat.SharedMessageIt{{Type: "text_message", Content: "hello"}}},
			{AuthorType: "assistant", AuthorName: "Planner", Items: []sharedchat.SharedMessageIt{{Type: "text_message", Content: "hi"}}},
		},
	}
	router := newRouter(sharedchat.NewHandler(store, transcript, []byte("secret")))

	token, _ := createLink(t, router, `{"expiry":"7d","scope":"all"}`)
	if token == "" {
		t.Fatal("create returned no token")
	}

	rec := view(router, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("view: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ConversationName string `json:"conversation_name"`
		Messages         []struct {
			ID    int `json:"id"`
			Items []struct {
				Content string `json:"content"`
			} `json:"items"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ConversationName != "Quarterly plan" {
		t.Fatalf("conversation_name = %q", out.ConversationName)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("messages = %d", len(out.Messages))
	}
	// Ordinals, not database ids.
	if out.Messages[0].ID != 0 || out.Messages[1].ID != 1 {
		t.Fatalf("ids = %d,%d — expected response ordinals", out.Messages[0].ID, out.Messages[1].ID)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q — an anonymous transcript must not be cached by a proxy", rec.Header().Get("Cache-Control"))
	}
	if transcript.sawProject != "7" {
		t.Fatalf("transcript read project %q — the project must come from the stored row", transcript.sawProject)
	}
}

// TestViewNeverEmitsInternalIdentifiers reads the raw JSON rather than a typed
// struct: a typed decode can only assert what it knows to look for, and the
// risk here is a field nobody thought to name.
func TestViewNeverEmitsInternalIdentifiers(t *testing.T) {
	store := newFakeStore()
	transcript := &fakeTranscript{
		name: "Secrets",
		messages: []sharedchat.SharedMessage{{
			AuthorType: "user", AuthorName: "Ada",
			Items: []sharedchat.SharedMessageIt{
				{Type: "text_message", Content: "body"},
				{Type: "attachment_message", Attachment: &sharedchat.SharedAttachment{Name: "report.pdf", Type: "file"}},
			},
		}},
	}
	router := newRouter(sharedchat.NewHandler(store, transcript, []byte("secret")))
	token, _ := createLink(t, router, `{"expiry":"1d","scope":"all"}`)

	body := view(router, token, nil).Body.String()
	for _, forbidden := range []string{
		"project_id", "conversation_id", "\"uuid\"", "bucket", "task_id",
		"email", "message_group_id", "participant_id", "meta", "token",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("anonymous view leaked %q; body = %s", forbidden, body)
		}
	}
	if !strings.Contains(body, "report.pdf") {
		t.Fatalf("attachment name missing from body = %s", body)
	}
}

// ------------------------------------------------------- negative: refusals

func TestViewRefusesAWrongToken(t *testing.T) {
	store := newFakeStore()
	router := newRouter(sharedchat.NewHandler(store, &fakeTranscript{}, []byte("secret")))
	createLink(t, router, `{"expiry":"7d"}`)

	rec := view(router, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("wrong token: status = %d, want 404", rec.Code)
	}
}

func TestViewRefusesARevokedLink(t *testing.T) {
	store := newFakeStore()
	router := newRouter(sharedchat.NewHandler(store, &fakeTranscript{name: "x"}, []byte("secret")))
	token, id := createLink(t, router, `{"expiry":"7d"}`)

	if got := view(router, token, nil).Code; got != http.StatusOK {
		t.Fatalf("precondition: live link status = %d", got)
	}

	req := httptest.NewRequest(http.MethodDelete,
		"/api/v2/elitea_core/shared_chat_link/prompt_lib/7/42/"+itoa(id), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	after := view(router, token, nil)
	if after.Code != http.StatusNotFound {
		t.Fatalf("revoked link: status = %d, want 404", after.Code)
	}
}

func TestViewRefusesAnExpiredLink(t *testing.T) {
	store := newFakeStore()
	clock := time.Now()
	store.now = func() time.Time { return clock }
	h := sharedchat.NewHandler(store, &fakeTranscript{name: "x"}, []byte("secret")).
		WithClock(func() time.Time { return clock })
	router := newRouter(h)

	token, _ := createLink(t, router, `{"expiry":"1h"}`)
	if got := view(router, token, nil).Code; got != http.StatusOK {
		t.Fatalf("precondition: live link status = %d", got)
	}

	clock = clock.Add(2 * time.Hour)
	after := view(router, token, nil)
	if after.Code != http.StatusNotFound {
		t.Fatalf("expired link: status = %d, want 404", after.Code)
	}
}

// TestRefusalsAreIndistinguishable is the enumeration-oracle test. A wrong, a
// revoked and an expired token must produce the SAME status and the SAME body:
// any difference confirms to a guesser that a token was once real.
func TestRefusalsAreIndistinguishable(t *testing.T) {
	store := newFakeStore()
	clock := time.Now()
	store.now = func() time.Time { return clock }
	h := sharedchat.NewHandler(store, &fakeTranscript{name: "x"}, []byte("secret")).
		WithClock(func() time.Time { return clock })
	router := newRouter(h)

	revokedToken, revokedID := createLink(t, router, `{"expiry":"7d"}`)
	expiredToken, _ := createLink(t, router, `{"expiry":"1h"}`)

	req := httptest.NewRequest(http.MethodDelete,
		"/api/v2/elitea_core/shared_chat_link/prompt_lib/7/42/"+itoa(revokedID), nil)
	router.ServeHTTP(httptest.NewRecorder(), req)
	clock = clock.Add(2 * time.Hour)

	unknown := view(router, "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", nil)
	malformed := view(router, "not-a-token-because-of-these-!!", nil)
	revoked := view(router, revokedToken, nil)
	expired := view(router, expiredToken, nil)

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"malformed": malformed, "revoked": revoked, "expired": expired,
	} {
		if rec.Code != unknown.Code {
			t.Fatalf("%s status = %d, unknown-token status = %d — the two must not be distinguishable",
				name, rec.Code, unknown.Code)
		}
		if rec.Body.String() != unknown.Body.String() {
			t.Fatalf("%s body = %q, unknown-token body = %q — the two must not be distinguishable",
				name, rec.Body.String(), unknown.Body.String())
		}
	}
}

// ------------------------------------------------------ negative: passwords

func TestPasswordProtectedLinkRefusesUntilUnlocked(t *testing.T) {
	store := newFakeStore()
	router := newRouter(sharedchat.NewHandler(store, &fakeTranscript{name: "locked"}, []byte("secret")))
	token, _ := createLink(t, router, `{"expiry":"7d","password":"correct horse"}`)

	rec := view(router, token, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("locked view: status = %d, want 401", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "password_required") {
		t.Fatalf("locked body = %s", body)
	}
	// A locked link must disclose NOTHING about the conversation.
	if strings.Contains(body, "locked") || strings.Contains(body, "messages") {
		t.Fatalf("locked view leaked conversation detail: %s", body)
	}
}

func TestUnlockRefusesAWrongPassword(t *testing.T) {
	store := newFakeStore()
	router := newRouter(sharedchat.NewHandler(store, &fakeTranscript{name: "locked"}, []byte("secret")))
	token, _ := createLink(t, router, `{"expiry":"7d","password":"correct horse"}`)

	rec := unlock(router, token, "wrong horse")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong password: status = %d, want 403", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("wrong password issued a grant cookie")
	}
}

// TestUnlockDoesNotRevealWhetherTheLinkExists pins the second half of the
// oracle: a wrong password on a REAL link and any password on a link that does
// not exist must answer identically.
func TestUnlockDoesNotRevealWhetherTheLinkExists(t *testing.T) {
	store := newFakeStore()
	router := newRouter(sharedchat.NewHandler(store, &fakeTranscript{name: "locked"}, []byte("secret")))
	token, _ := createLink(t, router, `{"expiry":"7d","password":"correct horse"}`)

	real := unlock(router, token, "wrong horse")
	fake := unlock(router, "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC", "wrong horse")
	unpassworded, _ := createLink(t, router, `{"expiry":"7d"}`)
	open := unlock(router, unpassworded, "anything")

	if real.Code != fake.Code || real.Body.String() != fake.Body.String() {
		t.Fatalf("wrong-password (%d %q) and no-such-link (%d %q) are distinguishable",
			real.Code, real.Body.String(), fake.Code, fake.Body.String())
	}
	if real.Code != open.Code || real.Body.String() != open.Body.String() {
		t.Fatalf("wrong-password (%d %q) and link-without-password (%d %q) are distinguishable",
			real.Code, real.Body.String(), open.Code, open.Body.String())
	}
}

func TestUnlockGrantsAccessAndIsBoundToOneLink(t *testing.T) {
	store := newFakeStore()
	router := newRouter(sharedchat.NewHandler(store, &fakeTranscript{name: "locked"}, []byte("secret")))
	tokenA, _ := createLink(t, router, `{"expiry":"7d","password":"correct horse"}`)
	tokenB, _ := createLink(t, router, `{"expiry":"7d","password":"correct horse"}`)

	rec := unlock(router, tokenA, "correct horse")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unlock: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("unlock set %d cookies", len(cookies))
	}
	grant := cookies[0]
	if !grant.HttpOnly {
		t.Fatal("grant cookie is not HttpOnly")
	}
	if grant.SameSite != http.SameSiteLaxMode {
		t.Fatalf("grant cookie SameSite = %v", grant.SameSite)
	}
	if !strings.HasPrefix(grant.Path, "/api/v2/elitea_core/shared_chat_view") {
		t.Fatalf("grant cookie Path = %q — it must not ride authenticated requests", grant.Path)
	}

	if got := view(router, tokenA, grant).Code; got != http.StatusOK {
		t.Fatalf("unlocked view: status = %d", got)
	}
	// The SAME cookie must not open a different link.
	if got := view(router, tokenB, grant).Code; got != http.StatusUnauthorized {
		t.Fatalf("grant for one link opened another: status = %d", got)
	}
}

func TestUnlockRefusesWithNoSessionSecret(t *testing.T) {
	store := newFakeStore()
	router := newRouter(sharedchat.NewHandler(store, &fakeTranscript{name: "locked"}, nil))
	token, _ := createLink(t, router, `{"expiry":"7d","password":"correct horse"}`)

	rec := unlock(router, token, "correct horse")
	if rec.Code == http.StatusNoContent {
		t.Fatal("unlock issued a grant with no keying material")
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("unlock issued a cookie with no keying material")
	}
}

// --------------------------------------------------------- create contract

func TestCreateStoresOnlyTheTokenHash(t *testing.T) {
	store := newFakeStore()
	router := newRouter(sharedchat.NewHandler(store, &fakeTranscript{}, []byte("secret")))
	token, _ := createLink(t, router, `{"expiry":"7d"}`)

	if len(store.created) != 1 {
		t.Fatalf("created = %d", len(store.created))
	}
	want := sha256.Sum256([]byte(token))
	if string(store.created[0].TokenHash) != string(want[:]) {
		t.Fatal("stored hash is not SHA-256 of the issued token")
	}
	if strings.Contains(string(store.created[0].TokenHash), token) {
		t.Fatal("the plaintext token reached the store")
	}
}

func TestCreateTokensAreUnpredictable(t *testing.T) {
	store := newFakeStore()
	router := newRouter(sharedchat.NewHandler(store, &fakeTranscript{}, []byte("secret")))
	seen := map[string]bool{}
	for i := 0; i < 25; i++ {
		token, _ := createLink(t, router, `{"expiry":"7d"}`)
		if len(token) < 40 {
			t.Fatalf("token %q is too short to be 256 bits of entropy", token)
		}
		if seen[token] {
			t.Fatalf("token %q issued twice", token)
		}
		seen[token] = true
	}
}

func TestCreateRefusesAnUnknownExpiryRatherThanDefaulting(t *testing.T) {
	store := newFakeStore()
	router := newRouter(sharedchat.NewHandler(store, &fakeTranscript{}, []byte("secret")))
	req := httptest.NewRequest(http.MethodPost,
		"/api/v2/elitea_core/shared_chat_links/prompt_lib/7/42", strings.NewReader(`{"expiry":"never"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expiry=never: status = %d, want 400 — a link with no end of life must be unrepresentable", rec.Code)
	}
}

func TestCreateCapsTheLifetime(t *testing.T) {
	store := newFakeStore()
	clock := time.Now()
	h := sharedchat.NewHandler(store, &fakeTranscript{}, []byte("secret")).
		WithClock(func() time.Time { return clock })
	router := newRouter(h)
	createLink(t, router, `{"expiry":"30d"}`)
	got := store.created[0].ExpiresAt.Sub(clock)
	if got > 30*24*time.Hour {
		t.Fatalf("lifetime = %s, exceeds the 30d cap", got)
	}
}

func TestCreateRefusesAShortPassword(t *testing.T) {
	store := newFakeStore()
	router := newRouter(sharedchat.NewHandler(store, &fakeTranscript{}, []byte("secret")))
	req := httptest.NewRequest(http.MethodPost,
		"/api/v2/elitea_core/shared_chat_links/prompt_lib/7/42", strings.NewReader(`{"password":"short"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("short password: status = %d, want 400", rec.Code)
	}
}

func TestCreatePartialShareRequiresGroupsAndKeepsThem(t *testing.T) {
	store := newFakeStore()
	transcript := &fakeTranscript{name: "partial"}
	router := newRouter(sharedchat.NewHandler(store, transcript, []byte("secret")))

	req := httptest.NewRequest(http.MethodPost,
		"/api/v2/elitea_core/shared_chat_links/prompt_lib/7/42", strings.NewReader(`{"scope":"partial"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty partial share: status = %d, want 400", rec.Code)
	}

	token, _ := createLink(t, router, `{"scope":"partial","message_group_ids":[11,12]}`)
	if got := view(router, token, nil).Code; got != http.StatusOK {
		t.Fatalf("partial view: status = %d", got)
	}
	if len(transcript.sawGroups) != 2 || transcript.sawGroups[0] != 11 {
		t.Fatalf("transcript scope = %v, want the link's own group list", transcript.sawGroups)
	}
}

// TestCreateDropsGroupIDsOnAFullShare pins that an 'all' link cannot carry a
// narrower list that a later reader might honour instead.
func TestCreateDropsGroupIDsOnAFullShare(t *testing.T) {
	store := newFakeStore()
	router := newRouter(sharedchat.NewHandler(store, &fakeTranscript{}, []byte("secret")))
	createLink(t, router, `{"scope":"all","message_group_ids":[11]}`)
	if len(store.created[0].MessageGroupIDs) != 0 {
		t.Fatalf("full share stored group ids %v", store.created[0].MessageGroupIDs)
	}
}

func TestRevokeIsScopedToItsConversation(t *testing.T) {
	store := newFakeStore()
	router := newRouter(sharedchat.NewHandler(store, &fakeTranscript{name: "x"}, []byte("secret")))
	_, id := createLink(t, router, `{"expiry":"7d"}`)

	req := httptest.NewRequest(http.MethodDelete,
		"/api/v2/elitea_core/shared_chat_link/prompt_lib/7/99/"+itoa(id), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-conversation revoke: status = %d, want 404", rec.Code)
	}
	if len(store.revoked) != 0 {
		t.Fatalf("cross-conversation revoke changed rows: %v", store.revoked)
	}
}

// TestViewSurvivesAnAccessCountFailure: the counter is owner-facing
// accounting, never part of the authorisation decision.
func TestViewSurvivesAnAccessCountFailure(t *testing.T) {
	store := newFakeStore()
	router := newRouter(sharedchat.NewHandler(store, &fakeTranscript{name: "x"}, []byte("secret")))
	token, _ := createLink(t, router, `{"expiry":"7d"}`)
	store.failNext = true
	if got := view(router, token, nil).Code; got != http.StatusOK {
		t.Fatalf("view with a failing access counter: status = %d", got)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
