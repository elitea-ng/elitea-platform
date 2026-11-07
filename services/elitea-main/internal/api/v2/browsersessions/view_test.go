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
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
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

// fakeStream is the supervisor's side of one BrowserView stream. As in
// production, a refusal arrives on the first Recv (the dial itself
// succeeds), and an admitted view's first message is the admission notice.
type fakeStream struct {
	control bool
	out     chan *runtimev1.BrowserViewOutputV1
	mu      sync.Mutex
	in      []*runtimev1.BrowserViewInputV1
	closed  chan struct{}
	once    sync.Once
	refusal error
}

func newFakeStream(control bool, admission string, refusal error) *fakeStream {
	s := &fakeStream{control: control, out: make(chan *runtimev1.BrowserViewOutputV1, 8), closed: make(chan struct{}), refusal: refusal}
	if admission != "" {
		s.out <- notice(admission)
	}
	return s
}

func notice(code string) *runtimev1.BrowserViewOutputV1 {
	return &runtimev1.BrowserViewOutputV1{Frame: &runtimev1.BrowserViewOutputV1_Notice{Notice: &runtimev1.BrowserViewNoticeV1{Code: code}}}
}

func (s *fakeStream) Send(frame *runtimev1.BrowserViewInputV1) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.in = append(s.in, frame)
	return nil
}

func (s *fakeStream) Recv() (*runtimev1.BrowserViewOutputV1, error) {
	if s.refusal != nil {
		return nil, s.refusal
	}
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
	// refusal is the supervisor's answer on the first Recv.
	refusal error
	// admission is the supervisor's first notice; "" sends none (a
	// supervisor that sends its first frame first), "-" sends none at all.
	admission string
}

func (d *fakeDialer) OpenView(_ context.Context, _ browsersession.Session, control bool) (ViewStream, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	admission := d.admission
	switch admission {
	case "":
		admission = noticeSessionLive
	case "-":
		admission = ""
	}
	stream := newFakeStream(control, admission, d.refusal)
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
		// The supervisor's refusals come on the first Recv, as in production.
		{"the supervisor no longer holds the session", store, &fakeDialer{refusal: browsersession.ErrViewSessionGone}, &owner, testSessionID, http.StatusGone},
		{"the supervisor is unreachable", store, &fakeDialer{refusal: browsersession.ErrViewUnavailable}, &owner, testSessionID, http.StatusBadGateway},
		{"the supervisor refuses Main's grant", store, &fakeDialer{refusal: browsersession.ErrViewRefused}, &owner, testSessionID, http.StatusBadGateway},
		{"the supervisor is at its limit", store, &fakeDialer{refusal: browsersession.ErrViewBusy}, &owner, testSessionID, http.StatusTooManyRequests},
		{"the supervisor ends the stream at once", store, &fakeDialer{refusal: io.EOF}, &owner, testSessionID, http.StatusBadGateway},
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
			if test.dialer.refusal == nil && len(test.dialer.streams) != 0 {
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
	// The view's button numbering is the proto's (1 left): the relay passes it through unchanged.
	if got[0].GetAck() != 4 || !got[1].GetInput().GetTakeOver() || got[2].GetInput().GetPointer().GetX() != 10 ||
		got[2].GetInput().GetPointer().GetButton() != 1 ||
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

// endRecorder records the rows the relay marks ended.
type endRecorder struct {
	mu    sync.Mutex
	ended []string
}

func (e *endRecorder) MarkEnded(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ended = append(e.ended, id)
	return nil
}

// A supervisor that no longer holds the session ends its row: the next
// viewer reads 410 from the row, without a dial.
func TestASupervisorThatLostTheSessionEndsItsRow(t *testing.T) {
	store := &sessionStore{rows: map[string]browsersession.Session{testSessionID: testSession(browsersession.StateLive)}}
	owner := user(ownerID)
	ender := &endRecorder{}
	handler := newTestHandler(t, store, &fakeDialer{refusal: browsersession.ErrViewSessionGone}).WithSessionEnder(ender)
	server := serve(t, handler, &owner)
	if _, response, err := dial(t, server, testSessionID); err == nil || response.StatusCode != http.StatusGone {
		t.Fatalf("response=%v", response)
	}
	if len(ender.ended) != 1 || ender.ended[0] != testSessionID {
		t.Fatalf("ended=%v", ender.ended)
	}
	// A supervisor that announces the session ended in its first message
	// is the same answer.
	end := &runtimev1.BrowserViewOutputV1{Frame: &runtimev1.BrowserViewOutputV1_End{End: &runtimev1.BrowserViewEndV1{Code: "wall"}}}
	dialer := &fakeDialer{admission: "-"}
	server = serve(t, newTestHandler(t, store, endFirst{dialer, end}).WithSessionEnder(ender), &owner)
	if _, response, err := dial(t, server, testSessionID); err == nil || response.StatusCode != http.StatusGone {
		t.Fatalf("end first: response=%v", response)
	}
	if len(ender.ended) != 2 {
		t.Fatalf("ended=%v", ender.ended)
	}
}

// endFirst queues an end as the supervisor's first message.
type endFirst struct {
	dialer *fakeDialer
	end    *runtimev1.BrowserViewOutputV1
}

func (e endFirst) OpenView(ctx context.Context, session browsersession.Session, control bool) (ViewStream, error) {
	stream, err := e.dialer.OpenView(ctx, session, control)
	if err == nil {
		stream.(*fakeStream).out <- e.end
	}
	return stream, err
}

// A row the worker never ended (it crashed, or lost its lease) stops being
// watchable at its start plus its wall plus the linger.
// A plain GET (the web client's diagnosis after a refused upgrade) learns
// 426 for a viewable session, and never takes a view slot or dials the
// supervisor; its refusals are the upgrade's.
func TestAPlainGetIsAnsweredWithoutTheSupervisor(t *testing.T) {
	store := &sessionStore{rows: map[string]browsersession.Session{testSessionID: testSession(browsersession.StateLive)}}
	owner, stranger := user(ownerID), user(strangerID)
	get := func(t *testing.T, principal *auth.User, header http.Header) (*fakeDialer, int) {
		t.Helper()
		dialer := &fakeDialer{}
		// One view per principal: a GET that took the slot would leave none.
		handler := newTestHandler(t, store, dialer).WithLimits(Limits{MaxStreams: 1, MaxPerPrincipal: 1})
		server := serve(t, handler, principal)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v2/browser_sessions/"+testSessionID+"/view", nil)
		if err != nil {
			t.Fatal(err)
		}
		for name, values := range header {
			request.Header[name] = values
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		return dialer, response.StatusCode
	}
	for _, test := range []struct {
		name      string
		principal *auth.User
		header    http.Header
		status    int
	}{
		{"a plain GET", &owner, nil, http.StatusUpgradeRequired},
		{"an Upgrade without Connection: upgrade", &owner, http.Header{"Upgrade": {"websocket"}}, http.StatusUpgradeRequired},
		{"a Connection: upgrade to something else", &owner, http.Header{"Connection": {"Upgrade"}, "Upgrade": {"h2c"}}, http.StatusUpgradeRequired},
		{"a stranger still reads not found", &stranger, nil, http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			dialer, status := get(t, test.principal, test.header)
			if status != test.status {
				t.Fatalf("status=%d, want %d", status, test.status)
			}
			if len(dialer.streams) != 0 {
				t.Fatal("a plain GET dialled the supervisor")
			}
		})
	}
}

func TestIsWebSocketUpgradeIsCaseInsensitive(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Connection", "keep-alive, UPGRADE")
	request.Header.Set("Upgrade", "WebSocket")
	if !isWebSocketUpgrade(request) {
		t.Fatal("a mixed-case upgrade was not recognised")
	}
	request.Header.Set("Connection", "keep-alive")
	if isWebSocketUpgrade(request) {
		t.Fatal("a request without Connection: upgrade was taken as an upgrade")
	}
}

func TestALiveRowPastItsBoundIsGone(t *testing.T) {
	stale := testSession(browsersession.StateLive)
	stale.StartedAt = time.Now().Add(-time.Hour)
	store := &sessionStore{rows: map[string]browsersession.Session{testSessionID: stale}}
	owner := user(ownerID)
	dialer := &fakeDialer{}
	server := serve(t, newTestHandler(t, store, dialer), &owner)
	if _, response, err := dial(t, server, testSessionID); err == nil || response.StatusCode != http.StatusGone {
		t.Fatalf("response=%v", response)
	}
	if len(dialer.streams) != 0 {
		t.Fatal("a stale row reached the supervisor")
	}
}

// A supervisor that admits the view as lingering (the run ended after the
// row was read) opens it watch-only, whatever the row said.
func TestTheSupervisorsLingeringAdmissionOpensWatchOnly(t *testing.T) {
	store := &sessionStore{rows: map[string]browsersession.Session{testSessionID: testSession(browsersession.StateLive)}}
	owner := user(ownerID)
	dialer := &fakeDialer{admission: noticeSessionLingering}
	server := serve(t, newTestHandler(t, store, dialer), &owner)
	conn, _, err := dial(t, server, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	if hello := readJSON(t, conn); hello["can_control"] != false || hello["lingering"] != true {
		t.Fatalf("hello=%v", hello)
	}
	writeJSON(t, conn, `{"type":"take_over"}`)
	if refused := readJSON(t, conn); refused["type"] != "refused" || refused["code"] != "input_refused" {
		t.Fatalf("refused=%v", refused)
	}
}

// failingPermissions: the resolver did not answer.
type failingPermissions struct{}

func (failingPermissions) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{}, errors.New("database down")
}

// A resolver outage is 503 (retry), not 404 (not yours): only a refusal
// means no access.
func TestAResolverOutageIsNotANotFound(t *testing.T) {
	store := &sessionStore{rows: map[string]browsersession.Session{testSessionID: testSession(browsersession.StateLive)}}
	handler, err := NewHandler(store, conversations{}, failingPermissions{}, &fakeDialer{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := user(ownerID)
	server := serve(t, handler, &owner)
	if _, response, err := dial(t, server, testSessionID); err == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response=%v", response)
	}
	// A refusal is still no access.
	if got, err := newTestHandler(t, store, &fakeDialer{}).Authorize(context.Background(), testSession(browsersession.StateLive), user(strangerID)); err != nil || got != (Access{}) {
		t.Fatalf("a stranger: %+v %v", got, err)
	}
}

// switchablePermissions: who may chat, changeable while a view is open.
type switchablePermissions struct {
	mu      sync.Mutex
	chat    map[string]bool
	members map[string]bool
}

func (p *switchablePermissions) set(id string, member, chat bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.members[id], p.chat[id] = member, chat
}

func (p *switchablePermissions) ResolvePermissions(_ context.Context, principal auth.User, _ string, _ string) (auth.PermissionResolution, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.members[principal.UserID] {
		return auth.PermissionResolution{}, auth.ErrPermissionDenied
	}
	if p.chat[principal.UserID] {
		return auth.PermissionResolution{Permissions: []string{TakeOverPermission}}, nil
	}
	return auth.PermissionResolution{Permissions: []string{}}, nil
}

// revalidator: the viewer's credential, revocable while a view is open.
type revalidator struct {
	mu  sync.Mutex
	err error
}

func (r *revalidator) set(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *revalidator) check(req *http.Request) (auth.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return auth.User{}, r.err
	}
	principal, _ := auth.RuntimePrincipalFromContext(req.Context())
	return principal, nil
}

func reauthHandler(t *testing.T, store *sessionStore, dialer ViewDialer, perms *switchablePermissions, creds *revalidator) *Handler {
	t.Helper()
	handler, err := NewHandler(store, conversations{}, perms, dialer, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler.reauthInterval = 20 * time.Millisecond
	return handler.WithRevalidator(creds.check)
}

func readUntil(t *testing.T, conn *websocket.Conn, want func(map[string]any) bool) map[string]any {
	t.Helper()
	for range 50 {
		if message := readJSON(t, conn); want(message) {
			return message
		}
	}
	t.Fatal("the message never came")
	return nil
}

// A driver who loses messages.create mid-stream is downgraded on the same
// stream: a notice says so, the control it held goes back to the agent,
// and its input is refused from then on (never relayed).
func TestADriverWhoLosesControlIsDowngraded(t *testing.T) {
	store := &sessionStore{rows: map[string]browsersession.Session{testSessionID: testSession(browsersession.StateLive)}}
	perms := &switchablePermissions{chat: map[string]bool{}, members: map[string]bool{}}
	perms.set(participantID, true, true)
	creds := &revalidator{}
	dialer := &fakeDialer{}
	participant := user(participantID)
	server := serve(t, reauthHandler(t, store, dialer, perms, creds), &participant)
	conn, _, err := dial(t, server, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	if hello := readJSON(t, conn); hello["can_control"] != true {
		t.Fatalf("hello=%v", hello)
	}
	stream := dialer.last()
	writeJSON(t, conn, `{"type":"take_over"}`)
	waitFor(t, func() bool { return len(stream.received()) == 1 })

	perms.set(participantID, true, false)
	readUntil(t, conn, func(m map[string]any) bool { return m["type"] == "notice" && m["code"] == noticeControlRevoked })
	waitFor(t, func() bool { return len(stream.received()) == 2 })
	if got := stream.received(); got[1].GetInput().GetHandBack() == nil {
		t.Fatalf("the held control was not handed back: %v", got)
	}
	writeJSON(t, conn, `{"type":"pointer","kind":"down","x":1,"y":1,"button":1,"click_count":1}`)
	if refused := readJSON(t, conn); refused["type"] != "refused" || refused["code"] != "input_refused" {
		t.Fatalf("refused=%v", refused)
	}
	writeJSON(t, conn, `{"type":"ack","seq":1}`)
	waitFor(t, func() bool { return len(stream.received()) == 3 })
	if got := stream.received(); got[2].GetAck() != 1 {
		t.Fatalf("the downgraded viewer's input reached the supervisor: %v", got)
	}
}

// A viewer who may no longer watch, or whose credential no longer
// authenticates (signed out, revoked, suspended), is closed with 4403.
func TestAViewerWhoLosesAccessIsClosed(t *testing.T) {
	for name, revoke := range map[string]func(*switchablePermissions, *revalidator){
		"no longer a member": func(p *switchablePermissions, _ *revalidator) { p.set(ownerID, false, false) },
		"signed out":         func(_ *switchablePermissions, r *revalidator) { r.set(auth.ErrPrincipalInactive) },
	} {
		t.Run(name, func(t *testing.T) {
			store := &sessionStore{rows: map[string]browsersession.Session{testSessionID: testSession(browsersession.StateLive)}}
			perms := &switchablePermissions{chat: map[string]bool{}, members: map[string]bool{}}
			perms.set(ownerID, true, true)
			creds := &revalidator{}
			dialer := &fakeDialer{}
			owner := user(ownerID)
			server := serve(t, reauthHandler(t, store, dialer, perms, creds), &owner)
			conn, _, err := dial(t, server, testSessionID)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.CloseNow() }()
			readJSON(t, conn)
			revoke(perms, creds)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for {
				if _, _, err := conn.Read(ctx); err != nil {
					if websocket.CloseStatus(err) != CloseAccessRevoked {
						t.Fatalf("close=%v", err)
					}
					break
				}
			}
			waitFor(t, func() bool {
				select {
				case <-dialer.last().closed:
					return true
				default:
					return false
				}
			})
		})
	}
}

// A re-check that cannot be answered does not close the view at once; three
// in a row do, with "try again later".
func TestAnUnanswerableReauthorizationClosesTryAgain(t *testing.T) {
	store := &sessionStore{rows: map[string]browsersession.Session{testSessionID: testSession(browsersession.StateLive)}}
	perms := &switchablePermissions{chat: map[string]bool{}, members: map[string]bool{}}
	perms.set(ownerID, true, true)
	creds := &revalidator{}
	owner := user(ownerID)
	server := serve(t, reauthHandler(t, store, &fakeDialer{}, perms, creds), &owner)
	conn, _, err := dial(t, server, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	readJSON(t, conn)
	creds.set(auth.ErrPrincipalUnavailable)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != CloseAuthorizationUnavailable {
		t.Fatalf("close=%v", err)
	}
}

// AuthRevalidator reads the /api/v2 authentication's answer: a principal,
// a refusal (inactive) or an outage (unavailable).
func TestAuthRevalidator(t *testing.T) {
	owner := user(ownerID)
	for _, test := range []struct {
		name   string
		status int
		want   error
	}{
		{"authenticated", 0, nil},
		{"refused", http.StatusUnauthorized, auth.ErrPrincipalInactive},
		{"store down", http.StatusServiceUnavailable, auth.ErrPrincipalUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			authenticate := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if test.status != 0 {
						http.Error(w, "no", test.status)
						return
					}
					next.ServeHTTP(w, r.WithContext(auth.ContextWithAuthenticatedUser(r.Context(), owner, auth.AuthenticationSourceSession)))
				})
			}
			got, err := AuthRevalidator(authenticate)(httptest.NewRequest(http.MethodGet, "/", nil))
			if !errors.Is(err, test.want) || (test.want == nil && got.ID != ownerID) {
				t.Fatalf("got=%+v err=%v", got, err)
			}
		})
	}
}

// A frame's header and its JPEG are one unit: no other message (a refusal
// written by the reader, a notice) comes between them.
func TestAFrameHeaderIsAlwaysFollowedByItsJPEG(t *testing.T) {
	store := &sessionStore{rows: map[string]browsersession.Session{testSessionID: testSession(browsersession.StateLive)}}
	reader := user(readerID)
	dialer := &fakeDialer{}
	server := serve(t, newTestHandler(t, store, dialer), &reader)
	conn, _, err := dial(t, server, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	readJSON(t, conn)
	stream := dialer.last()
	const frames = 300
	go func() {
		for i := range frames {
			stream.out <- &runtimev1.BrowserViewOutputV1{Frame: &runtimev1.BrowserViewOutputV1_Screen{Screen: &runtimev1.BrowserFrameV1{
				Seq: uint64(i + 1), Jpeg: []byte{0xff, 0xd8, byte(i)}, Width: 1, Height: 1,
			}}}
		}
	}()
	// The watch-only reader's input: each draws a `refused` written by the
	// up side while the down side writes frames.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for range frames {
			if conn.Write(ctx, websocket.MessageText, []byte(`{"type":"take_over"}`)) != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	seen, refused := 0, 0
	for seen < frames || refused < frames {
		typ, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read after %d frames, %d refusals: %v", seen, refused, err)
		}
		if typ != websocket.MessageText {
			t.Fatalf("a JPEG with no header before it (after %d frames)", seen)
		}
		var message map[string]any
		if err := json.Unmarshal(raw, &message); err != nil {
			t.Fatal(err)
		}
		switch message["type"] {
		case "refused":
			refused++
			continue
		case "frame":
		default:
			t.Fatalf("message=%v", message)
		}
		typ, jpeg, err := conn.Read(ctx)
		if err != nil || typ != websocket.MessageBinary || len(jpeg) != 3 || jpeg[2] != byte(seen) {
			t.Fatalf("frame %d: the next message is type=%v bytes=%q err=%v, not its JPEG", seen, typ, jpeg, err)
		}
		seen++
	}
}

// recordedAudit keeps the events.
type recordedAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recordedAudit) Record(_ context.Context, event audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordedAudit) actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, event := range r.events {
		out = append(out, event.Action)
	}
	return out
}

// Opening a driving view and each take over are in the audit trail; a
// watch-only view is not.
func TestDrivingAndTakingOverAreAudited(t *testing.T) {
	store := &sessionStore{rows: map[string]browsersession.Session{testSessionID: testSession(browsersession.StateLive)}}
	recorder := &recordedAudit{}
	owner, reader := user(ownerID), user(readerID)
	dialer := &fakeDialer{}
	watcher := serve(t, newTestHandler(t, store, dialer).WithAudit(recorder), &reader)
	conn, _, err := dial(t, watcher, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	readJSON(t, conn)
	_ = conn.CloseNow()
	if got := recorder.actions(); len(got) != 0 {
		t.Fatalf("a watch-only view was audited: %v", got)
	}
	driver := serve(t, newTestHandler(t, store, dialer).WithAudit(recorder), &owner)
	conn, _, err = dial(t, driver, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	readJSON(t, conn)
	writeJSON(t, conn, `{"type":"take_over"}`)
	waitFor(t, func() bool { return len(recorder.actions()) == 2 })
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	first, second := recorder.events[0], recorder.events[1]
	if !strings.HasPrefix(first.Action, "Open a driving browser view; session "+testSessionID) ||
		!strings.HasPrefix(second.Action, "Take over the browser; session "+testSessionID) ||
		first.EventType != "agent" || first.HTTPRoute != ViewPath || *first.ProjectID != 5 || *first.UserID != 7 ||
		first.EntityType != "browser_session" || first.EntityName != testSessionID {
		t.Fatalf("events=%+v", recorder.events)
	}
}

// The deployment's public origin and the desktop app's bundled page are
// admitted; another site, or the public host on another scheme, is not.
func TestOriginPatternsAdmitThePublicOriginAndTheDesktop(t *testing.T) {
	store := &sessionStore{rows: map[string]browsersession.Session{testSessionID: testSession(browsersession.StateLive)}}
	owner := user(ownerID)
	handler := newTestHandler(t, store, &fakeDialer{}).WithOriginPatterns(OriginPatterns("https://elitea.example/some/path"))
	server := serve(t, handler, &owner)
	for origin, admitted := range map[string]bool{
		"https://elitea.example":                  true,
		"tauri://localhost":                       true,
		"http://tauri.localhost":                  true,
		"https://tauri.localhost":                 true,
		"http://elitea.example":                   false,
		"https://attacker.example":                false,
		"https://elitea.example.attacker.example": false,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, response, err := websocket.Dial(ctx, viewURL(server, testSessionID), &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{origin}}})
		cancel()
		if admitted != (err == nil) {
			t.Errorf("%s: admitted=%v response=%v", origin, err == nil, response)
		}
		if conn != nil {
			_ = conn.CloseNow()
		}
	}
	if got := OriginPatterns(""); len(got) != len(DesktopOrigins) {
		t.Fatalf("no public origin: %v", got)
	}
}
