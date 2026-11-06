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
//     session is not told apart from an absent one), 410 for an ended one.
//
// A watch-only viewer's input is refused here, with a `refused` message,
// and never reaches the supervisor. The supervisor refuses it again: the
// grant's digest binds `control`.
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
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/browsersession"
)

// ViewPath is the route.
const ViewPath = "/api/v2/browser_sessions/{session_id}/view"

// pingInterval keeps an idle view open through proxies with idle timeouts.
const pingInterval = 30 * time.Second

// TakeOverPermission lets a viewer who is not the run's owner drive.
const TakeOverPermission = "models.chat.messages.create"

// SessionReader reads a session row on any replica (repos.BrowserSessions).
type SessionReader interface {
	Lookup(ctx context.Context, sessionID string) (browsersession.Session, bool, error)
}

// ConversationReader answers whether principal may read the conversation
// under the chat detail visibility rule.
type ConversationReader interface {
	MayReadConversation(ctx context.Context, projectID int64, conversationUUID string, principal auth.User) (bool, error)
}

// ViewStream is one open BrowserView stream to a supervisor.
type ViewStream interface {
	Send(*runtimev1.BrowserViewInputV1) error
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

// ErrSupervisorUnavailable is a dial that did not reach the supervisor, or
// a supervisor this replica is not configured for.
var ErrSupervisorUnavailable = errors.New("browser supervisor unavailable")

// Limits bound the viewers one replica relays.
type Limits struct {
	MaxStreams      int
	MaxPerPrincipal int
}

// DefaultLimits is 256 streams and 4 per person on one replica.
func DefaultLimits() Limits { return Limits{MaxStreams: 256, MaxPerPrincipal: 4} }

// Handler serves ViewPath.
type Handler struct {
	sessions      SessionReader
	conversations ConversationReader
	permissions   auth.PermissionResolver
	dialer        ViewDialer
	now           func() time.Time
	logger        *slog.Logger
	// originPatterns: the hosts besides the request's own that a browser
	// Origin may name (coder/websocket's OriginPatterns).
	originPatterns []string

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
	}, nil
}

// WithOriginPatterns admits a browser Origin on these hosts besides the
// request's own (a deployment that serves the SPA on another host).
func (h *Handler) WithOriginPatterns(patterns []string) *Handler {
	h.originPatterns = append([]string(nil), patterns...)
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
// bearer) and a chi router for ViewPath.
func (h *Handler) Route(authConfig apimw.AuthConfig) http.Handler {
	router := chi.NewRouter()
	router.Method(http.MethodGet, ViewPath, apimw.Auth(authConfig)(http.HandlerFunc(h.ServeHTTP)))
	return router
}

// Access is what a principal may do with one session.
type Access struct {
	Watch   bool
	Control bool
}

// Authorize decides the principal's access to session. The owner may watch
// and drive. Anyone else must be able to read the run's conversation to
// watch, and must also hold TakeOverPermission in the project to drive. It
// resolves permissions on every call, so it also revalidates the principal.
func (h *Handler) Authorize(ctx context.Context, session browsersession.Session, principal auth.User) (Access, error) {
	actor, ok := principal.OwningUserID()
	if !ok || actor <= 0 {
		return Access{}, nil
	}
	project := strconv.FormatInt(session.ProjectID, 10)
	resolution, err := h.permissions.ResolvePermissions(ctx, principal, auth.PermissionModeDefault, project)
	if err != nil {
		// An inactive principal, a revoked token, no membership.
		return Access{}, nil
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

// ServeHTTP authorizes, opens the supervisor stream, then upgrades and
// relays. Every refusal is an HTTP status before the upgrade, so a client
// can tell "not yours" from "gone" from "try again".
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
		h.logger.WarnContext(r.Context(), "browser view: the supervisor stream did not open", "session_id", sessionID)
		http.Error(w, "browser session unavailable", http.StatusBadGateway)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.originPatterns})
	if err != nil {
		// Accept wrote the refusal (a bad handshake or a foreign Origin).
		_ = stream.CloseSend()
		return
	}
	conn.SetReadLimit(maxUpMessageBytes)
	relay(ctx, cancel, conn, stream, helloMessage{Type: typeHello, SessionID: sessionID, CanControl: control, Lingering: lingering})
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

// relay pumps one viewer until either side ends. Down: each supervisor
// frame becomes a JSON header and one binary JPEG. Up: acks always, input
// only when the stream may drive.
func relay(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, stream ViewStream, hello helloMessage) {
	var writeMu sync.Mutex
	write := func(typ websocket.MessageType, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		writeCtx, done := context.WithTimeout(ctx, 10*time.Second)
		defer done()
		return conn.Write(writeCtx, typ, payload)
	}
	writeJSON := func(v any) error {
		payload, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return write(websocket.MessageText, payload)
	}
	if writeJSON(hello) != nil {
		_ = stream.CloseSend()
		_ = conn.CloseNow()
		return
	}
	closeCode, closeReason := websocket.StatusNormalClosure, ""
	var closeOnce sync.Once
	finish := func(code websocket.StatusCode, reason string) {
		closeOnce.Do(func() { closeCode, closeReason = code, reason })
		cancel()
	}
	// The reader's context is not the relay's: cancelling a read context
	// makes coder/websocket drop the connection without the close
	// handshake. conn.Close below ends the read instead, after the close
	// frame, so the viewer reads the end code and a clean close.
	readCtx, readCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer readCancel()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // keepalive: a still page sends no frame for minutes
		defer wg.Done()
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pingCtx, done := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Ping(pingCtx)
				done()
				if err != nil && ctx.Err() == nil {
					finish(websocket.StatusGoingAway, "")
					return
				}
			}
		}
	}()
	go func() { // down
		defer wg.Done()
		for {
			out, err := stream.Recv()
			if err != nil {
				finish(websocket.StatusGoingAway, "browser_session_unavailable")
				return
			}
			switch frame := out.GetFrame().(type) {
			case *runtimev1.BrowserViewOutputV1_Screen:
				screen := frame.Screen
				if writeJSON(frameMessage{
					Type: typeFrame, Seq: screen.GetSeq(), Width: screen.GetWidth(), Height: screen.GetHeight(),
					Bytes: len(screen.GetJpeg()), URL: screen.GetUrl(), Title: screen.GetTitle(), Control: controlName(screen.GetControl()),
				}) != nil || write(websocket.MessageBinary, screen.GetJpeg()) != nil {
					finish(websocket.StatusGoingAway, "")
					return
				}
			case *runtimev1.BrowserViewOutputV1_Notice:
				if writeJSON(codeMessage{Type: typeNotice, Code: frame.Notice.GetCode()}) != nil {
					finish(websocket.StatusGoingAway, "")
					return
				}
			case *runtimev1.BrowserViewOutputV1_End:
				_ = writeJSON(codeMessage{Type: typeEnd, Code: frame.End.GetCode()})
				finish(websocket.StatusNormalClosure, frame.End.GetCode())
				return
			}
		}
	}()
	go func() { // up
		defer wg.Done()
		defer func() { _ = stream.CloseSend() }()
		for {
			typ, raw, err := conn.Read(readCtx)
			if err != nil {
				finish(websocket.StatusNormalClosure, "")
				return
			}
			if typ != websocket.MessageText {
				finish(websocket.StatusUnsupportedData, "text messages only")
				return
			}
			frame, isInput, err := decodeUp(raw)
			switch {
			case isInput && !hello.CanControl:
				if writeJSON(codeMessage{Type: typeRefused, Code: "input_refused"}) != nil {
					finish(websocket.StatusGoingAway, "")
					return
				}
				continue
			case err != nil:
				if writeJSON(codeMessage{Type: typeRefused, Code: "malformed"}) != nil {
					finish(websocket.StatusGoingAway, "")
					return
				}
				continue
			}
			if stream.Send(frame) != nil {
				finish(websocket.StatusGoingAway, "browser_session_unavailable")
				return
			}
		}
	}()
	<-ctx.Done()
	// Close waits (bounded by the library) for the viewer's close frame;
	// the reader then returns. readCancel is the backstop.
	_ = conn.Close(closeCode, closeReason)
	readCancel()
	wg.Wait()
}
