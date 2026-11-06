package browsersessions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/browsersession"
)

const (
	testExecution    = "0123456789abcdef0123456789abcdef"
	testConversation = "8f14e45f-ceea-467f-a8ee-2b1c3d4e5f60"
	ownerID          = "7"
	participantID    = "8"
	readerID         = "9"
	strangerID       = "10"
)

var testSessionID = browsersession.NameFor(testExecution)

func testSession(state browsersession.State) browsersession.Session {
	return browsersession.Session{
		SessionID: testSessionID, ExecutionID: testExecution, Generation: 3, TenantID: "1", ProjectID: 5,
		OwnerActorID: ownerID, ConversationUUID: testConversation, SupervisorAudience: "dns:elitea-sandbox-browser",
		Spec: browsersession.Spec{
			Revision: 1, Name: testSessionID, Engine: 1, CPUMillis: 1000, MemoryBytes: 1 << 30, WallSeconds: 600, MaxPages: 3,
		},
		State: state, LingerUntil: linger(state),
	}
}

func linger(state browsersession.State) time.Time {
	if state == browsersession.StateLingering {
		return time.Now().Add(5 * time.Minute)
	}
	return time.Time{}
}

// sessionStore is the one table every replica reads.
type sessionStore struct {
	mu   sync.Mutex
	rows map[string]browsersession.Session
	err  error
}

func (s *sessionStore) Lookup(_ context.Context, id string) (browsersession.Session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return browsersession.Session{}, false, s.err
	}
	row, ok := s.rows[id]
	return row, ok, nil
}

// conversations: the participant and the reader may read the conversation.
type conversations struct{ err error }

func (c conversations) MayReadConversation(_ context.Context, project int64, conversation string, principal auth.User) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	return project == 5 && conversation == testConversation && (principal.UserID == participantID || principal.UserID == readerID), nil
}

// permissions: everyone is a member; the participant and the owner may chat.
type permissions struct{}

func (permissions) ResolvePermissions(_ context.Context, principal auth.User, _ string, project string) (auth.PermissionResolution, error) {
	if project != "5" {
		return auth.PermissionResolution{}, auth.ErrPermissionDenied
	}
	switch principal.UserID {
	case ownerID, participantID:
		return auth.PermissionResolution{Permissions: []string{"models.chat.conversations.get", TakeOverPermission}}, nil
	default:
		return auth.PermissionResolution{Permissions: []string{"models.chat.conversations.get"}}, nil
	}
}

func user(id string) auth.User { return auth.User{ID: id, UserID: id} }

func newTestHandler(t *testing.T, store *sessionStore, dialer ViewDialer) *Handler {
	t.Helper()
	handler, err := NewHandler(store, conversations{}, permissions{}, dialer, nil)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestAuthorizeMatrix(t *testing.T) {
	handler := newTestHandler(t, &sessionStore{}, &fakeDialer{})
	session := testSession(browsersession.StateLive)
	noConversation := session
	noConversation.ConversationUUID = ""
	for _, test := range []struct {
		name      string
		session   browsersession.Session
		principal auth.User
		want      Access
	}{
		{"the owner watches and drives", session, user(ownerID), Access{Watch: true, Control: true}},
		{"a participant who may chat drives", session, user(participantID), Access{Watch: true, Control: true}},
		{"a reader without messages.create only watches", session, user(readerID), Access{Watch: true}},
		{"a member who cannot read the conversation sees nothing", session, user(strangerID), Access{}},
		{"no conversation: only the owner", noConversation, user(participantID), Access{}},
		{"no conversation: the owner still drives", noConversation, user(ownerID), Access{Watch: true, Control: true}},
		{"a token principal has no owning user", session, auth.User{ID: "token-1", TokenID: "token-1"}, Access{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := handler.Authorize(context.Background(), test.session, test.principal)
			if err != nil || got != test.want {
				t.Fatalf("access=%+v err=%v, want %+v", got, err, test.want)
			}
		})
	}
	other := session
	other.ProjectID = 6
	if got, _ := handler.Authorize(context.Background(), other, user(ownerID)); got != (Access{}) {
		t.Fatalf("the owner who is no longer a member of the project kept access: %+v", got)
	}
}

// fakeStream is the supervisor's side of one BrowserView stream.
type fakeStream struct {
	control bool
	out     chan *runtimev1.BrowserViewOutputV1
	mu      sync.Mutex
	in      []*runtimev1.BrowserViewInputV1
	closed  chan struct{}
	once    sync.Once
}

func newFakeStream(control bool) *fakeStream {
	return &fakeStream{control: control, out: make(chan *runtimev1.BrowserViewOutputV1, 8), closed: make(chan struct{})}
}

func (s *fakeStream) Send(frame *runtimev1.BrowserViewInputV1) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.in = append(s.in, frame)
	return nil
}

func (s *fakeStream) Recv() (*runtimev1.BrowserViewOutputV1, error) {
	select {
	case frame := <-s.out:
		return frame, nil
	case <-s.closed:
		return nil, io.EOF
	}
}

func (s *fakeStream) CloseSend() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *fakeStream) received() []*runtimev1.BrowserViewInputV1 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*runtimev1.BrowserViewInputV1(nil), s.in...)
}

type fakeDialer struct {
	mu      sync.Mutex
	streams []*fakeStream
	err     error
}

func (d *fakeDialer) OpenView(_ context.Context, _ browsersession.Session, control bool) (ViewStream, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	stream := newFakeStream(control)
	d.streams = append(d.streams, stream)
	return stream, nil
}

func (d *fakeDialer) last() *fakeStream {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.streams[len(d.streams)-1]
}

// serve mounts the handler as the /api/v2 group would, with the principal
// the auth middleware would have put on the context.
func serve(t *testing.T, handler *Handler, principal *auth.User) *httptest.Server {
	t.Helper()
	router := chi.NewRouter()
	router.Get(ViewPath, func(w http.ResponseWriter, r *http.Request) {
		if principal != nil {
			r = r.WithContext(auth.ContextWithAuthenticatedUser(r.Context(), *principal, auth.AuthenticationSourceSession))
		}
		handler.ServeHTTP(w, r)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

func viewURL(server *httptest.Server, id string) string {
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v2/browser_sessions/" + id + "/view"
}

func dial(t *testing.T, server *httptest.Server, id string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, viewURL(server, id), nil)
}

func readJSON(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, raw, err := conn.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("read: type=%v err=%v", typ, err)
	}
	var message map[string]any
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatal(err)
	}
	return message
}

func writeJSON(t *testing.T, conn *websocket.Conn, message string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(message)); err != nil {
		t.Fatal(err)
	}
}

func TestViewRefusalsBeforeTheUpgrade(t *testing.T) {
	ended := testSession(browsersession.StateEnded)
	expired := testSession(browsersession.StateLingering)
	expired.LingerUntil = time.Now().Add(-time.Minute)
	expiredID := browsersession.NameFor("fedcba9876543210fedcba9876543210")
	expired.SessionID, expired.Spec.Name = expiredID, expiredID
	endedID := browsersession.NameFor("00000000000000000000000000000001")
	ended.SessionID, ended.Spec.Name = endedID, endedID
	store := &sessionStore{rows: map[string]browsersession.Session{
		testSessionID: testSession(browsersession.StateLive), endedID: ended, expiredID: expired,
	}}
	owner, stranger := user(ownerID), user(strangerID)
	for _, test := range []struct {
		name      string
		store     *sessionStore
		dialer    *fakeDialer
		principal *auth.User
		id        string
		status    int
	}{
		{"no credential", store, &fakeDialer{}, nil, testSessionID, http.StatusUnauthorized},
		{"a malformed id", store, &fakeDialer{}, &owner, "elitea-br-nothex", http.StatusNotFound},
		{"an unknown session", store, &fakeDialer{}, &owner, browsersession.NameFor("unknown"), http.StatusNotFound},
		{"a stranger is not told the session exists", store, &fakeDialer{}, &stranger, testSessionID, http.StatusNotFound},
		{"an ended session", store, &fakeDialer{}, &owner, endedID, http.StatusGone},
		{"a linger that elapsed", store, &fakeDialer{}, &owner, expiredID, http.StatusGone},
		{"the table is unavailable", &sessionStore{err: errors.New("down")}, &fakeDialer{}, &owner, testSessionID, http.StatusServiceUnavailable},
		{"the supervisor is unavailable", store, &fakeDialer{err: errors.New("dial")}, &owner, testSessionID, http.StatusBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := serve(t, newTestHandler(t, test.store, test.dialer), test.principal)
			conn, response, err := dial(t, server, test.id)
			if err == nil {
				_ = conn.CloseNow()
				t.Fatal("the upgrade succeeded")
			}
			if response == nil || response.StatusCode != test.status {
				t.Fatalf("response=%v, want %d", response, test.status)
			}
			if test.status != http.StatusBadGateway && len(test.dialer.streams) != 0 {
				t.Fatal("a refused viewer reached the supervisor")
			}
		})
	}
}

func TestViewRefusesAForeignOrigin(t *testing.T) {
	store := &sessionStore{rows: map[string]browsersession.Session{testSessionID: testSession(browsersession.StateLive)}}
	owner := user(ownerID)
	dialer := &fakeDialer{}
	server := serve(t, newTestHandler(t, store, dialer), &owner)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	header := http.Header{"Origin": []string{"https://attacker.example"}}
	conn, response, err := websocket.Dial(ctx, viewURL(server, testSessionID), &websocket.DialOptions{HTTPHeader: header})
	if err == nil {
		_ = conn.CloseNow()
		t.Fatal("a cross-site page opened the view")
	}
	if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("response=%v", response)
	}
	if len(dialer.streams) != 0 {
		t.Fatal("a cross-site page reached the supervisor")
	}
}

func TestRelayADrivingViewer(t *testing.T) {
	store := &sessionStore{rows: map[string]browsersession.Session{testSessionID: testSession(browsersession.StateLive)}}
	owner := user(ownerID)
	dialer := &fakeDialer{}
	server := serve(t, newTestHandler(t, store, dialer), &owner)
	conn, _, err := dial(t, server, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	hello := readJSON(t, conn)
	if hello["type"] != "hello" || hello["session_id"] != testSessionID || hello["can_control"] != true || hello["lingering"] != false {
		t.Fatalf("hello=%v", hello)
	}
	stream := dialer.last()
	if !stream.control {
		t.Fatal("the owner's stream was opened watch-only")
	}
	stream.out <- &runtimev1.BrowserViewOutputV1{Frame: &runtimev1.BrowserViewOutputV1_Screen{Screen: &runtimev1.BrowserFrameV1{
		Seq: 4, Width: 1280, Height: 800, Jpeg: []byte{0xff, 0xd8, 0xff}, Url: "https://example.com/", Title: "Example",
		Control: runtimev1.BrowserControlV1_BROWSER_CONTROL_V1_AGENT,
	}}}
	header := readJSON(t, conn)
	if header["type"] != "frame" || header["seq"] != float64(4) || header["bytes"] != float64(3) || header["url"] != "https://example.com/" ||
		header["title"] != "Example" || header["control"] != "agent" || header["width"] != float64(1280) {
		t.Fatalf("frame=%v", header)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, jpeg, err := conn.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(jpeg) != "\xff\xd8\xff" {
		t.Fatalf("jpeg: type=%v err=%v bytes=%x", typ, err, jpeg)
	}
	writeJSON(t, conn, `{"type":"ack","seq":4}`)
	writeJSON(t, conn, `{"type":"take_over"}`)
	writeJSON(t, conn, `{"type":"pointer","kind":"down","x":10,"y":20,"button":1,"click_count":1}`)
	writeJSON(t, conn, `{"type":"key","kind":"char","text":"a"}`)
	writeJSON(t, conn, `{"type":"hand_back","note":"logged in"}`)
	stream.out <- &runtimev1.BrowserViewOutputV1{Frame: &runtimev1.BrowserViewOutputV1_Notice{Notice: &runtimev1.BrowserViewNoticeV1{Code: "session_lingering"}}}
	if notice := readJSON(t, conn); notice["type"] != "notice" || notice["code"] != "session_lingering" {
		t.Fatalf("notice=%v", notice)
	}
	waitFor(t, func() bool { return len(stream.received()) == 5 })
	got := stream.received()
	if got[0].GetAck() != 4 || !got[1].GetInput().GetTakeOver() || got[2].GetInput().GetPointer().GetX() != 10 ||
		got[3].GetInput().GetKey().GetText() != "a" || got[4].GetInput().GetHandBack().GetNote() != "logged in" {
		t.Fatalf("up=%v", got)
	}
	stream.out <- &runtimev1.BrowserViewOutputV1{Frame: &runtimev1.BrowserViewOutputV1_End{End: &runtimev1.BrowserViewEndV1{Code: "session_destroyed"}}}
	if end := readJSON(t, conn); end["type"] != "end" || end["code"] != "session_destroyed" {
		t.Fatalf("end=%v", end)
	}
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("close=%v", err)
	}
}

func TestRelayRefusesAWatchOnlyViewersInput(t *testing.T) {
	store := &sessionStore{rows: map[string]browsersession.Session{testSessionID: testSession(browsersession.StateLive)}}
	reader := user(readerID)
	dialer := &fakeDialer{}
	server := serve(t, newTestHandler(t, store, dialer), &reader)
	conn, _, err := dial(t, server, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	if hello := readJSON(t, conn); hello["can_control"] != false {
		t.Fatalf("hello=%v", hello)
	}
	stream := dialer.last()
	if stream.control {
		t.Fatal("a watch-only viewer's stream was opened to drive")
	}
	for _, message := range []string{`{"type":"take_over"}`, `{"type":"pointer","kind":"move","x":1,"y":1}`, `{"type":"key","kind":"down","key":"a"}`, `{"type":"hand_back"}`} {
		writeJSON(t, conn, message)
		if refused := readJSON(t, conn); refused["type"] != "refused" || refused["code"] != "input_refused" {
			t.Fatalf("%s: %v", message, refused)
		}
	}
	writeJSON(t, conn, `{"type":"ack","seq":1}`)
	writeJSON(t, conn, `{"type":"nonsense"}`)
	if refused := readJSON(t, conn); refused["code"] != "malformed" {
		t.Fatalf("malformed: %v", refused)
	}
	waitFor(t, func() bool { return len(stream.received()) == 1 })
	if got := stream.received(); got[0].GetAck() != 1 {
		t.Fatalf("up=%v", got)
	}
}

func TestALingeringSessionOpensWatchOnlyEvenForTheOwner(t *testing.T) {
	store := &sessionStore{rows: map[string]browsersession.Session{testSessionID: testSession(browsersession.StateLingering)}}
	owner := user(ownerID)
	dialer := &fakeDialer{}
	server := serve(t, newTestHandler(t, store, dialer), &owner)
	conn, _, err := dial(t, server, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	if hello := readJSON(t, conn); hello["can_control"] != false || hello["lingering"] != true {
		t.Fatalf("hello=%v", hello)
	}
	if dialer.last().control {
		t.Fatal("a lingering session was opened to drive")
	}
}

// Two replicas share nothing but the session table: a viewer on either one
// reaches the session, and each replica opens its own supervisor stream.
func TestAnyReplicaServesAnySession(t *testing.T) {
	store := &sessionStore{rows: map[string]browsersession.Session{}}
	owner, reader := user(ownerID), user(readerID)
	dialerA, dialerB := &fakeDialer{}, &fakeDialer{}
	replicaA := serve(t, newTestHandler(t, store, dialerA), &owner)
	replicaB := serve(t, newTestHandler(t, store, dialerB), &reader)
	if _, response, err := dial(t, replicaB, testSessionID); err == nil || response.StatusCode != http.StatusNotFound {
		t.Fatalf("a session not yet reported: response=%v", response)
	}
	// The worker's report lands (on whichever replica its control stream is).
	store.mu.Lock()
	store.rows[testSessionID] = testSession(browsersession.StateLive)
	store.mu.Unlock()
	connA, _, err := dial(t, replicaA, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connA.CloseNow() }()
	connB, _, err := dial(t, replicaB, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connB.CloseNow() }()
	if hello := readJSON(t, connA); hello["can_control"] != true {
		t.Fatalf("replica A: %v", hello)
	}
	if hello := readJSON(t, connB); hello["can_control"] != false {
		t.Fatalf("replica B: %v", hello)
	}
	if len(dialerA.streams) != 1 || len(dialerB.streams) != 1 {
		t.Fatalf("streams: A=%d B=%d", len(dialerA.streams), len(dialerB.streams))
	}
}

func TestViewLimits(t *testing.T) {
	store := &sessionStore{rows: map[string]browsersession.Session{testSessionID: testSession(browsersession.StateLive)}}
	owner := user(ownerID)
	handler := newTestHandler(t, store, &fakeDialer{}).WithLimits(Limits{MaxStreams: 8, MaxPerPrincipal: 1})
	server := serve(t, handler, &owner)
	conn, _, err := dial(t, server, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	readJSON(t, conn)
	if _, response, err := dial(t, server, testSessionID); err == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second stream: response=%v", response)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return handler.open == 0
	})
}

func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
