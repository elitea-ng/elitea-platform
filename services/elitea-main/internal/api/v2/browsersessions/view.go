// Package browsersessions serves the cloud browser view relay (ADR-0049
// decision 6, #1597): GET /api/v2/browser_sessions/{session_id}/view, a
// WebSocket that elitea-main relays to the supervisor's BrowserView gRPC
// stream over mTLS (ADR-0036).
//
// Any replica serves any session. The handler keeps no session state: it
// reads the session row (elitea_runtime.browser_sessions, written by the
// worker's ReportBrowserSession), decides who may watch and who may drive,
// signs a view grant for the session's own run, and dials the supervisor
// the row names. Two viewers on two replicas are two independent streams;
// the supervisor counts them, and it screencasts only while one is attached.
//
// WHO MAY DO WHAT.
//   - Watch: the run's owner, or a principal who may read the run's
//     conversation (the chat detail visibility rule; the same rule the
//     execution event stream applies).
//   - Drive (take_over, pointer, wheel, key, hand_back): the run's owner, or
//     a viewer who also holds `models.chat.messages.create` in the project.
//   - Everything else is refused before the WebSocket opens: 401 with no
//     credential, 404 for a session the caller may not watch (a forbidden
//     session is not told apart from an absent one), 410 for an ended one,
//     503 when the session or the caller's access could not be read. The
//     supervisor's own refusal is also an HTTP status: the relay waits for
//     the supervisor's first message before it upgrades (410 gone, 429 at a
//     limit, 502 unreachable or refused).
//
// A watch-only viewer's input is refused here, with a `refused` message,
// and never reaches the supervisor. The supervisor refuses it again: the
// grant's digest binds `control`.
//
// AN OPEN VIEW IS RE-AUTHORIZED. Every 30 seconds the relay re-reads the
// caller's credential (a signed-out session, a revoked token or a suspended
// account ends the view), the session row and the caller's access. A
// viewer who may no longer watch is closed with 4403 `access_revoked`. A
// viewer who may still watch but no longer drive is downgraded on the same
// stream: Main refuses its input from then on (a `control_revoked` notice
// says so) and, if the viewer held control, hands it back to the agent.
// Main is the gate of every input the stream carries, so a watch-only
// stream is not reopened: that would drop the viewer's frame and detach it
// from the supervisor for nothing.
package browsersessions

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/browsersession"
)

// ViewPath is the route.
const ViewPath = "/api/v2/browser_sessions/{session_id}/view"

// pingInterval keeps an idle view open through proxies with idle timeouts.
const pingInterval = 30 * time.Second

// reauthInterval is how often an open view re-checks its viewer.
const reauthInterval = 30 * time.Second

// maxReauthFailures re-checks that could not be answered (a database or
// principal store outage) close the view; a refusal closes it at once.
const maxReauthFailures = 3

// firstMessageTimeout bounds the wait for the supervisor's first message,
// before the upgrade.
const firstMessageTimeout = 10 * time.Second

// The close codes of a view the relay ends itself.
const (
	// CloseAccessRevoked: the viewer may no longer watch (signed out,
	// suspended, no longer a reader of the conversation).
	CloseAccessRevoked websocket.StatusCode = 4403
	// CloseAuthorizationUnavailable: the viewer's access could not be
	// re-checked maxReauthFailures times in a row; reconnect.
	CloseAuthorizationUnavailable = websocket.StatusTryAgainLater
)

// TakeOverPermission lets a viewer who is not the run's owner drive.
const TakeOverPermission = "models.chat.messages.create"

// The supervisor's first notices (browser_session_service.rs): which state
// the session admitted the view in. Neither is relayed: hello says it.
const (
	noticeSessionLive      = "session_live"
	noticeSessionLingering = "session_lingering"
	noticeControlRevoked   = "control_revoked"
)

// SessionReader reads a session row on any replica (repos.BrowserSessions).
type SessionReader interface {
	Lookup(ctx context.Context, sessionID string) (browsersession.Session, bool, error)
}

// SessionEnder marks a session row ended (repos.BrowserSessions), for a
// supervisor that answers a view with "not started" or "ended".
type SessionEnder interface {
	MarkEnded(ctx context.Context, sessionID string) error
}

// ConversationReader answers whether principal may read the conversation
// under the chat detail visibility rule.
type ConversationReader interface {
	MayReadConversation(ctx context.Context, projectID int64, conversationUUID string, principal auth.User) (bool, error)
}

// ViewStream is one open BrowserView stream to a supervisor.
type ViewStream interface {
	Send(*runtimev1.BrowserViewInputV1) error
	// Recv returns the supervisor's next message. A refusal is one of
	// browsersession's ErrView* kinds.
	Recv() (*runtimev1.BrowserViewOutputV1, error)
	// CloseSend ends the up direction; the supervisor then detaches the
	// viewer.
	CloseSend() error
}

// ViewDialer signs the view grant and opens the stream for one session
// (the composition's supervisor client: gRPC over mTLS to the audience the
// row names). It sends the open frame itself.
type ViewDialer interface {
	OpenView(ctx context.Context, session browsersession.Session, control bool) (ViewStream, error)
}

// Revalidator re-reads the credential of the request that opened a view
// and answers its principal now: auth.ErrPrincipalInactive when the
// credential no longer authenticates (signed out, revoked, suspended), any
// other error when the stores did not answer.
type Revalidator func(r *http.Request) (auth.User, error)

// Limits bound the viewers one replica relays.
type Limits struct {
	MaxStreams      int
	MaxPerPrincipal int
}

// DefaultLimits is 256 streams and 4 per person on one replica.
func DefaultLimits() Limits { return Limits{MaxStreams: 256, MaxPerPrincipal: 4} }

// DesktopOrigins are the Origins the desktop app's bundled page sends:
// `tauri://localhost` on macOS and Linux, `http(s)://tauri.localhost` on
// Windows (apps/elitea-desktop/src-tauri/src/window.rs).
var DesktopOrigins = []string{"tauri://localhost", "http://tauri.localhost", "https://tauri.localhost"}

// OriginPatterns is what a deployment admits besides the request's own
// host: its public origin (DEPLOYMENT_URL, "" when unset) and the desktop
// app's origins. Each pattern names a scheme, so a public origin of
// https://elitea.example admits that origin only, not http on the same host.
func OriginPatterns(publicOrigin string) []string {
	patterns := make([]string, 0, len(DesktopOrigins)+1)
	if u, err := url.Parse(publicOrigin); err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http") {
		patterns = append(patterns, strings.ToLower(u.Scheme+"://"+u.Host))
	}
	return append(patterns, DesktopOrigins...)
}

// Handler serves ViewPath.
type Handler struct {
	sessions      SessionReader
	ender         SessionEnder
	conversations ConversationReader
	permissions   auth.PermissionResolver
	dialer        ViewDialer
	audit         audit.Recorder
	revalidate    Revalidator
	now           func() time.Time
	logger        *slog.Logger
	// originPatterns: the origins besides the request's own host that a
	// browser Origin may name (coder/websocket's OriginPatterns).
	originPatterns []string

	reauthInterval      time.Duration
	firstMessageTimeout time.Duration

	mu          sync.Mutex
	open        int
	byPrincipal map[string]int
	limits      Limits
}

// NewHandler builds the handler. Every dependency is required.
func NewHandler(sessions SessionReader, conversations ConversationReader, permissions auth.PermissionResolver, dialer ViewDialer, logger *slog.Logger) (*Handler, error) {
	if sessions == nil || conversations == nil || permissions == nil || dialer == nil {
		return nil, errors.New("browser view relay: sessions, conversations, permissions and the dialer are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		sessions: sessions, conversations: conversations, permissions: permissions, dialer: dialer,
		now: time.Now, logger: logger, byPrincipal: map[string]int{}, limits: DefaultLimits(),
		reauthInterval: reauthInterval, firstMessageTimeout: firstMessageTimeout,
	}, nil
}

// WithOriginPatterns admits a browser Origin matching these patterns
// besides the request's own host (OriginPatterns builds the deployment's).
func (h *Handler) WithOriginPatterns(patterns []string) *Handler {
	h.originPatterns = append([]string(nil), patterns...)
	return h
}

// WithSessionEnder marks a row ended when the supervisor says the session
// is gone. Nil leaves the rows to the worker's reports and their bound.
func (h *Handler) WithSessionEnder(ender SessionEnder) *Handler {
	h.ender = ender
	return h
}

// WithAudit records each driving view and each take over in the audit
// trail. Nil records nothing.
func (h *Handler) WithAudit(recorder audit.Recorder) *Handler {
	h.audit = recorder
	return h
}

// WithRevalidator re-reads the viewer's credential on every re-check.
// Route sets the /api/v2 group's; nil re-checks the access only.
func (h *Handler) WithRevalidator(revalidate Revalidator) *Handler {
	h.revalidate = revalidate
	return h
}

// WithLimits replaces the per-replica stream limits; zero fields keep the
// defaults.
func (h *Handler) WithLimits(limits Limits) *Handler {
	if limits.MaxStreams > 0 {
		h.limits.MaxStreams = limits.MaxStreams
	}
	if limits.MaxPerPrincipal > 0 {
		h.limits.MaxPerPrincipal = limits.MaxPerPrincipal
	}
	return h
}

// Route wraps the handler in the /api/v2 group's authentication (cookie or
// bearer) and a chi router for ViewPath. The same authentication re-reads
// the viewer's credential on every re-check of an open view.
func (h *Handler) Route(authConfig apimw.AuthConfig) http.Handler {
	authenticate := apimw.Auth(authConfig)
	h.WithRevalidator(AuthRevalidator(authenticate))
	router := chi.NewRouter()
	router.Method(http.MethodGet, ViewPath, authenticate(h))
	return router
}

// AuthRevalidator runs authenticate (an apimw.Auth) over a copy of the
// opening request: the same four credential sources, the same session,
// token and suspension checks as the upgrade itself.
func AuthRevalidator(authenticate func(http.Handler) http.Handler) Revalidator {
	return func(r *http.Request) (auth.User, error) {
		var (
			user  auth.User
			found bool
		)
		writer := &statusWriter{header: http.Header{}}
		authenticate(http.HandlerFunc(func(_ http.ResponseWriter, inner *http.Request) {
			user, found = auth.RuntimePrincipalFromContext(inner.Context())
		})).ServeHTTP(writer, r)
		switch {
		case found:
			return user, nil
		case writer.status >= http.StatusInternalServerError:
			return auth.User{}, auth.ErrPrincipalUnavailable
		default:
			return auth.User{}, auth.ErrPrincipalInactive
		}
	}
}

// statusWriter keeps only the status of a refusal nobody reads.
type statusWriter struct {
	header http.Header
	status int
}

func (w *statusWriter) Header() http.Header { return w.header }
func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return len(p), nil
}
func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

// Access is what a principal may do with one session.
type Access struct {
	Watch   bool
	Control bool
}

// Authorize decides the principal's access to session. The owner may watch
// and drive. Anyone else must be able to read the run's conversation to
// watch, and must also hold TakeOverPermission in the project to drive. It
// resolves permissions on every call, so it also revalidates the principal
// against the project. A refusal (no membership, an inactive principal or
// project) is no access; any other resolver or reader failure is an error,
// which the relay answers 503: an outage is not a statement about access.
func (h *Handler) Authorize(ctx context.Context, session browsersession.Session, principal auth.User) (Access, error) {
	actor, ok := principal.OwningUserID()
	if !ok || actor <= 0 {
		return Access{}, nil
	}
	project := strconv.FormatInt(session.ProjectID, 10)
	resolution, err := h.permissions.ResolvePermissions(ctx, principal, auth.PermissionModeDefault, project)
	if err != nil {
		if errors.Is(err, auth.ErrPermissionDenied) || errors.Is(err, auth.ErrPrincipalInactive) {
			return Access{}, nil
		}
		return Access{}, err
	}
	if session.OwnerActorID == strconv.FormatInt(actor, 10) {
		return Access{Watch: true, Control: true}, nil
	}
	if session.ConversationUUID == "" {
		return Access{}, nil
	}
	readable, err := h.conversations.MayReadConversation(ctx, session.ProjectID, session.ConversationUUID, principal)
	if err != nil {
		return Access{}, err
	}
	if !readable {
		return Access{}, nil
	}
	control := false
	for _, permission := range resolution.Permissions {
		if permission == TakeOverPermission {
			control = true
			break
		}
	}
	return Access{Watch: true, Control: control}, nil
}

func (h *Handler) admit(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open >= h.limits.MaxStreams || h.byPrincipal[key] >= h.limits.MaxPerPrincipal {
		return false
	}
	h.open++
	h.byPrincipal[key]++
	return true
}

func (h *Handler) release(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.open--
	if h.byPrincipal[key]--; h.byPrincipal[key] <= 0 {
		delete(h.byPrincipal, key)
	}
}

// ServeHTTP authorizes, opens the supervisor stream, waits for the
// supervisor's first message, then upgrades and relays. Every refusal is
// an HTTP status before the upgrade, so a client can tell "not yours" from
// "gone" from "try again".
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A cross-site page must not reach the supervisor at all: the Origin
	// is checked before anything is read or dialled (Accept checks it
	// again). A native client sends no Origin.
	if !h.originAllowed(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	principal, ok := auth.RuntimePrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	sessionID := chi.URLParam(r, "session_id")
	if !browsersession.ValidName(sessionID) {
		http.Error(w, "browser session not found", http.StatusNotFound)
		return
	}
	session, found, err := h.sessions.Lookup(r.Context(), sessionID)
	if err != nil {
		h.logger.WarnContext(r.Context(), "browser view: session lookup failed", "session_id", sessionID)
		http.Error(w, "browser session lookup unavailable", http.StatusServiceUnavailable)
		return
	}
	if !found {
		http.Error(w, "browser session not found", http.StatusNotFound)
		return
	}
	access, err := h.Authorize(r.Context(), session, principal)
	if err != nil {
		h.logger.WarnContext(r.Context(), "browser view: authorization failed", "session_id", sessionID)
		w.Header().Set("Retry-After", "5")
		http.Error(w, "browser session authorization unavailable", http.StatusServiceUnavailable)
		return
	}
	if !access.Watch {
		http.Error(w, "browser session not found", http.StatusNotFound)
		return
	}
	if !session.Watchable(h.now()) {
		http.Error(w, "browser session ended", http.StatusGone)
		return
	}
	lingering := session.State == browsersession.StateLingering
	// A lingering session refuses input: it is opened watch-only.
	control := access.Control && !lingering
	key := principal.ID
	if !h.admit(key) {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "too many browser views", http.StatusTooManyRequests)
		return
	}
	defer h.release(key)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stream, err := h.dialer.OpenView(ctx, session, control)
	if err != nil {
		h.refuse(w, r, session, err)
		return
	}
	first, err := h.firstMessage(stream, cancel)
	if err != nil {
		_ = stream.CloseSend()
		h.refuse(w, r, session, err)
		return
	}
	var pending *runtimev1.BrowserViewOutputV1
	switch frame := first.GetFrame().(type) {
	case *runtimev1.BrowserViewOutputV1_End:
		_ = stream.CloseSend()
		h.refuse(w, r, session, browsersession.ErrViewSessionGone)
		return
	case *runtimev1.BrowserViewOutputV1_Notice:
		switch frame.Notice.GetCode() {
		case noticeSessionLive:
		case noticeSessionLingering:
			// The run ended since the row was read: the supervisor
			// refuses input, and so does the relay.
			lingering, control = true, false
		default:
			pending = first
		}
	default:
		// A supervisor that announces nothing: its first frame is the
		// admission.
		pending = first
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.originPatterns})
	if err != nil {
		// Accept wrote the refusal (a bad handshake or a foreign Origin).
		_ = stream.CloseSend()
		return
	}
	conn.SetReadLimit(maxUpMessageBytes)
	if control {
		h.auditControl(r.Context(), principal, session, "Open a driving browser view")
	}
	v := &view{
		h: h, conn: conn, stream: stream, session: session, principal: principal,
		ctx: ctx, cancel: cancel, opening: r.Clone(context.WithoutCancel(r.Context())),
		closeCode: websocket.StatusNormalClosure,
	}
	v.control.Store(control)
	v.relay(helloMessage{Type: typeHello, SessionID: sessionID, CanControl: control, Lingering: lingering}, pending)
}

// firstMessage waits up to firstMessageTimeout for the supervisor's first
// message: an admission notice, a frame, an end, or a refusal.
func (h *Handler) firstMessage(stream ViewStream, cancel context.CancelFunc) (*runtimev1.BrowserViewOutputV1, error) {
	type result struct {
		out *runtimev1.BrowserViewOutputV1
		err error
	}
	got := make(chan result, 1)
	go func() {
		out, err := stream.Recv()
		got <- result{out, err}
	}()
	timer := time.NewTimer(h.firstMessageTimeout)
	defer timer.Stop()
	select {
	case res := <-got:
		return res.out, res.err
	case <-timer.C:
		// The stream's context ends the Recv.
		cancel()
		return nil, browsersession.ErrViewUnavailable
	}
}

// refuse answers a supervisor refusal before the upgrade: 410 for a session
// the supervisor no longer holds (and its row is marked ended), 429 for a
// supervisor at a limit, 502 for one that is unreachable or refused Main's
// grant (logged: Main signed something its supervisor does not accept).
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, session browsersession.Session, err error) {
	switch {
	case errors.Is(err, browsersession.ErrViewSessionGone):
		if h.ender != nil {
			if endErr := h.ender.MarkEnded(context.WithoutCancel(r.Context()), session.SessionID); endErr != nil {
				h.logger.WarnContext(r.Context(), "browser view: the ended session row was not updated", "session_id", session.SessionID)
			}
		}
		http.Error(w, "browser session ended", http.StatusGone)
	case errors.Is(err, browsersession.ErrViewBusy):
		w.Header().Set("Retry-After", "5")
		http.Error(w, "browser supervisor is busy", http.StatusTooManyRequests)
	case errors.Is(err, browsersession.ErrViewRefused):
		h.logger.ErrorContext(r.Context(), "browser view: the supervisor refused Main's view grant",
			"session_id", session.SessionID, "audience", session.SupervisorAudience)
		http.Error(w, "browser session unavailable", http.StatusBadGateway)
	default:
		h.logger.WarnContext(r.Context(), "browser view: the supervisor stream did not open", "session_id", session.SessionID)
		http.Error(w, "browser session unavailable", http.StatusBadGateway)
	}
}

// auditControl records a person taking the browser (centry.audit_events,
// type `agent`, the trail's user tab): opening a driving view, and each
// take over.
func (h *Handler) auditControl(ctx context.Context, principal auth.User, session browsersession.Session, what string) {
	if h.audit == nil {
		return
	}
	var user *int64
	if actor, ok := principal.OwningUserID(); ok {
		user = audit.ID(actor)
	}
	status := int32(http.StatusSwitchingProtocols)
	duration := 0.0
	h.audit.Record(context.WithoutCancel(ctx), audit.Event{
		Timestamp: h.now().UTC(), UserID: user, UserEmail: principal.Email, ProjectID: audit.ID(session.ProjectID),
		EventType: "agent", Action: what + "; session " + session.SessionID + "; execution " + session.ExecutionID,
		HTTPMethod: http.MethodGet, HTTPRoute: ViewPath, StatusCode: &status, DurationMS: &duration,
		EntityType: "browser_session", EntityName: session.SessionID,
	})
}

// originAllowed is coder/websocket's rule: no Origin (a native client), the
// request's own host, or a configured pattern.
func (h *Handler) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(r.Host, u.Host) {
		return true
	}
	for _, pattern := range h.originPatterns {
		target := u.Host
		if strings.Contains(pattern, "://") {
			target = u.Scheme + "://" + u.Host
		}
		if matched, err := path.Match(strings.ToLower(pattern), strings.ToLower(target)); err == nil && matched {
			return true
		}
	}
	return false
}

// view is one relayed viewer.
type view struct {
	h         *Handler
	conn      *websocket.Conn
	stream    ViewStream
	session   browsersession.Session
	principal auth.User
	ctx       context.Context
	cancel    context.CancelFunc
	// opening is the request that opened the view, for the re-checks.
	opening *http.Request

	// writeMu orders the WebSocket writes: a frame's header and its JPEG
	// are written under one hold, so nothing comes between them.
	writeMu sync.Mutex
	// sendMu orders the stream's Sends, and makes the control check and
	// the Send of one input one step: no input passes after a downgrade.
	sendMu sync.Mutex
	// control: the viewer may drive now. holding: it took over and has not
	// handed back.
	control atomic.Bool
	holding bool

	closeOnce   sync.Once
	closeCode   websocket.StatusCode // StatusNormalClosure until finish
	closeReason string
}

func (v *view) write(typ websocket.MessageType, payloads ...[]byte) error {
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	for _, payload := range payloads {
		writeCtx, done := context.WithTimeout(v.ctx, 10*time.Second)
		err := v.conn.Write(writeCtx, typ, payload)
		done()
		if err != nil {
			return err
		}
		typ = websocket.MessageBinary
	}
	return nil
}

func (v *view) writeJSON(message any) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return v.write(websocket.MessageText, payload)
}

// writeFrame writes a frame's header and then its JPEG, as one unit.
func (v *view) writeFrame(screen *runtimev1.BrowserFrameV1) error {
	header, err := json.Marshal(frameMessage{
		Type: typeFrame, Seq: screen.GetSeq(), Width: screen.GetWidth(), Height: screen.GetHeight(),
		Bytes: len(screen.GetJpeg()), URL: screen.GetUrl(), Title: screen.GetTitle(), Control: controlName(screen.GetControl()),
	})
	if err != nil {
		return err
	}
	return v.write(websocket.MessageText, header, screen.GetJpeg())
}

func (v *view) finish(code websocket.StatusCode, reason string) {
	v.closeOnce.Do(func() { v.closeCode, v.closeReason = code, reason })
	v.cancel()
}

// down relays one supervisor message; false ends the view.
func (v *view) down(out *runtimev1.BrowserViewOutputV1) bool {
	switch frame := out.GetFrame().(type) {
	case *runtimev1.BrowserViewOutputV1_Screen:
		if v.writeFrame(frame.Screen) != nil {
			v.finish(websocket.StatusGoingAway, "")
			return false
		}
	case *runtimev1.BrowserViewOutputV1_Notice:
		if v.writeJSON(codeMessage{Type: typeNotice, Code: frame.Notice.GetCode()}) != nil {
			v.finish(websocket.StatusGoingAway, "")
			return false
		}
	case *runtimev1.BrowserViewOutputV1_End:
		_ = v.writeJSON(codeMessage{Type: typeEnd, Code: frame.End.GetCode()})
		v.finish(websocket.StatusNormalClosure, frame.End.GetCode())
		return false
	}
	return true
}

// send relays one viewer message. Input passes only while the viewer may
// drive, checked under the same hold as the Send.
func (v *view) send(frame *runtimev1.BrowserViewInputV1, isInput bool) (refused bool, err error) {
	v.sendMu.Lock()
	defer v.sendMu.Unlock()
	if isInput && !v.control.Load() {
		return true, nil
	}
	if err := v.stream.Send(frame); err != nil {
		return false, err
	}
	switch {
	case frame.GetInput().GetTakeOver():
		v.holding = true
		v.h.auditControl(v.ctx, v.principal, v.session, "Take over the browser")
	case frame.GetInput().GetHandBack() != nil:
		v.holding = false
	}
	return false, nil
}

// downgrade makes a driving viewer watch-only: its input is refused from
// now on, and a control it held goes back to the agent.
func (v *view) downgrade() {
	v.sendMu.Lock()
	wasDriving := v.control.Swap(false)
	held := v.holding
	v.holding = false
	if held {
		_ = v.stream.Send(&runtimev1.BrowserViewInputV1{Frame: &runtimev1.BrowserViewInputV1_Input{Input: &runtimev1.BrowserInputEventV1{
			Event: &runtimev1.BrowserInputEventV1_HandBack{HandBack: &runtimev1.BrowserHandBackV1{}},
		}}})
	}
	v.sendMu.Unlock()
	if wasDriving {
		_ = v.writeJSON(codeMessage{Type: typeNotice, Code: noticeControlRevoked})
	}
}

// errAccessRevoked ends a view whose viewer may no longer watch.
var errAccessRevoked = errors.New("browser view access revoked")

// reauthorize re-reads the viewer's credential, the session row and the
// viewer's access.
func (v *view) reauthorize() (Access, error) {
	ctx, done := context.WithTimeout(v.ctx, 10*time.Second)
	defer done()
	principal := v.principal
	if v.h.revalidate != nil {
		current, err := v.h.revalidate(v.opening.WithContext(ctx))
		if errors.Is(err, auth.ErrPrincipalInactive) {
			return Access{}, errAccessRevoked
		}
		if err != nil {
			return Access{}, err
		}
		if current.ID != principal.ID {
			return Access{}, errAccessRevoked
		}
		principal = current
	}
	session, found, err := v.h.sessions.Lookup(ctx, v.session.SessionID)
	if err != nil {
		return Access{}, err
	}
	if !found {
		return Access{}, errAccessRevoked
	}
	access, err := v.h.Authorize(ctx, session, principal)
	if err != nil {
		return Access{}, err
	}
	if !access.Watch {
		return Access{}, errAccessRevoked
	}
	if session.State != browsersession.StateLive {
		access.Control = false
	}
	return access, nil
}

// relay pumps one viewer until either side ends. Down: each supervisor
// frame becomes a JSON header and one binary JPEG. Up: acks always, input
// only while the viewer may drive. A re-check every reauthInterval ends or
// downgrades the view as the viewer's access changes.
func (v *view) relay(hello helloMessage, pending *runtimev1.BrowserViewOutputV1) {
	if v.writeJSON(hello) != nil || (pending != nil && !v.down(pending)) {
		_ = v.stream.CloseSend()
		_ = v.conn.CloseNow()
		return
	}
	// The reader's context is not the relay's: cancelling a read context
	// makes coder/websocket drop the connection without the close
	// handshake. conn.Close below ends the read instead, after the close
	// frame, so the viewer reads the end code and a clean close.
	readCtx, readCancel := context.WithCancel(context.WithoutCancel(v.ctx))
	defer readCancel()
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { // keepalive: a still page sends no frame for minutes
		defer wg.Done()
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-v.ctx.Done():
				return
			case <-ticker.C:
				pingCtx, done := context.WithTimeout(v.ctx, 10*time.Second)
				err := v.conn.Ping(pingCtx)
				done()
				if err != nil && v.ctx.Err() == nil {
					v.finish(websocket.StatusGoingAway, "")
					return
				}
			}
		}
	}()
	go func() { // re-authorization
		defer wg.Done()
		ticker := time.NewTicker(v.h.reauthInterval)
		defer ticker.Stop()
		failures := 0
		for {
			select {
			case <-v.ctx.Done():
				return
			case <-ticker.C:
			}
			access, err := v.reauthorize()
			switch {
			case v.ctx.Err() != nil:
				return
			case errors.Is(err, errAccessRevoked):
				v.finish(CloseAccessRevoked, "access_revoked")
				return
			case err != nil:
				failures++
				v.h.logger.WarnContext(v.ctx, "browser view: the viewer's access could not be re-checked",
					"session_id", v.session.SessionID, "failures", failures)
				if failures >= maxReauthFailures {
					v.finish(CloseAuthorizationUnavailable, "authorization_unavailable")
					return
				}
				continue
			}
			failures = 0
			if !access.Control && v.control.Load() {
				v.downgrade()
			}
		}
	}()
	go func() { // down
		defer wg.Done()
		for {
			out, err := v.stream.Recv()
			if err != nil {
				v.finish(websocket.StatusGoingAway, "browser_session_unavailable")
				return
			}
			if !v.down(out) {
				return
			}
		}
	}()
	go func() { // up
		defer wg.Done()
		defer func() {
			v.sendMu.Lock()
			_ = v.stream.CloseSend()
			v.sendMu.Unlock()
		}()
		for {
			typ, raw, err := v.conn.Read(readCtx)
			if err != nil {
				v.finish(websocket.StatusNormalClosure, "")
				return
			}
			if typ != websocket.MessageText {
				v.finish(websocket.StatusUnsupportedData, "text messages only")
				return
			}
			frame, isInput, err := decodeUp(raw)
			if isInput && !v.control.Load() {
				if v.writeJSON(codeMessage{Type: typeRefused, Code: "input_refused"}) != nil {
					v.finish(websocket.StatusGoingAway, "")
					return
				}
				continue
			}
			if err != nil {
				if v.writeJSON(codeMessage{Type: typeRefused, Code: "malformed"}) != nil {
					v.finish(websocket.StatusGoingAway, "")
					return
				}
				continue
			}
			refused, err := v.send(frame, isInput)
			if err != nil {
				v.finish(websocket.StatusGoingAway, "browser_session_unavailable")
				return
			}
			if refused && v.writeJSON(codeMessage{Type: typeRefused, Code: "input_refused"}) != nil {
				v.finish(websocket.StatusGoingAway, "")
				return
			}
		}
	}()
	<-v.ctx.Done()
	// A context the request ended (not finish) still settles the code:
	// the empty Do orders this read after any finish.
	v.closeOnce.Do(func() {})
	// Close waits (bounded by the library) for the viewer's close frame;
	// the reader then returns. readCancel is the backstop.
	_ = v.conn.Close(v.closeCode, v.closeReason)
	readCancel()
	wg.Wait()
}
