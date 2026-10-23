// Package proxy is elitea-egress-proxy's data plane: an HTTP forward proxy
// that admits a sandbox's connection only to the hosts its run token allows
// (ADR-0034 decision 9, "Host allowlist: the egress proxy").
//
// Per request it:
//
//  1. reads the run token from Proxy-Authorization (Basic, user "run");
//  2. verifies the token with the public key set (libs/go/egresslib/runtoken);
//  3. matches host and port against the token's allowlist;
//  4. resolves the host itself and refuses the request when ANY resolved
//     address is not public (libs/go/egresslib/addrcheck) or falls in an
//     operator deny range (the cluster's pod and service ranges);
//  5. for a CONNECT, answers 200, then peeks the client's TLS ClientHello and
//     refuses the tunnel when its server_name is not the CONNECT host
//     (clienthello.go);
//  6. dials the checked IP literal, so DNS rebinding cannot swap the address;
//  7. relays bytes: a CONNECT tunnel, or one absolute-form http:// request.
//
// It never terminates TLS: the sandbox verifies the destination's certificate
// end to end. It never follows a redirect: a forwarded response goes back as
// it is. It never logs, echoes or forwards the token.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib/addrcheck"
	"github.com/elitea-ng/elitea-platform/libs/go/egresslib/runtoken"
)

// RefusalHeader carries the reason of a refusal, from the fixed set in
// metrics.go. It never carries request or token text.
const RefusalHeader = "X-Elitea-Egress-Refused"

// TokenUser is the Basic user name that carries a run token as the password,
// as HTTPS_PROXY=http://run:<token>@host:port produces.
const TokenUser = "run"

// copyBufferBytes is the relay buffer of one direction.
const copyBufferBytes = 32 << 10

// The bounds of the ClientHello peek of a CONNECT tunnel. A client that has
// not sent a whole ClientHello within ClientHelloTimeout of the 200, or whose
// ClientHello (record headers included) needs more than ClientHelloMaxBytes,
// is refused. A real ClientHello, post-quantum key shares included, is under
// 2 KiB.
const (
	ClientHelloTimeout  = 10 * time.Second
	ClientHelloMaxBytes = 16 << 10
)

// alertAccessDenied is a plaintext fatal TLS alert, access_denied (49). The
// proxy sends it when it refuses a ClientHello, so the client reports
// "access denied" rather than an unexplained EOF.
var alertAccessDenied = []byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 49}

// Options configures a Server.
type Options struct {
	Keys      KeySource
	Resolver  addrcheck.Resolver
	Dialer    addrcheck.ContextDialer
	DenyCIDRs []*net.IPNet

	MaxConnections        int
	MaxHeaderBytes        int
	ReadHeaderTimeout     time.Duration
	DialTimeout           time.Duration
	RunMaxConnections     int
	RunMaxBytes           int64
	IdleTimeout           time.Duration
	MaxConnectionLifetime time.Duration

	Logger  *slog.Logger
	Metrics *Metrics
	// Now replaces time.Now, for tests.
	Now func() time.Time

	// The ClientHello peek bounds; zero means ClientHelloTimeout and
	// ClientHelloMaxBytes. Tests lower them.
	clientHelloTimeout  time.Duration
	clientHelloMaxBytes int
}

// Server is the proxy. Build it with New, run it with Serve, stop it with
// Shutdown.
type Server struct {
	opts    Options
	runs    *Runs
	httpSrv *http.Server

	draining atomic.Bool

	mu       sync.Mutex
	sessions map[*session]struct{}
}

// New builds a server.
func New(opts Options) (*Server, error) {
	if opts.Keys == nil {
		return nil, errors.New("proxy: no key source")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Metrics == nil {
		opts.Metrics = NewMetrics()
	}
	if opts.Resolver == nil {
		opts.Resolver = net.DefaultResolver
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = addrcheck.DialTimeout
	}
	if opts.Dialer == nil {
		opts.Dialer = &net.Dialer{Timeout: opts.DialTimeout}
	}
	if opts.clientHelloTimeout <= 0 {
		opts.clientHelloTimeout = ClientHelloTimeout
	}
	if opts.clientHelloMaxBytes <= 0 {
		opts.clientHelloMaxBytes = ClientHelloMaxBytes
	}
	switch {
	case opts.MaxConnections <= 0, opts.MaxHeaderBytes <= 0, opts.RunMaxConnections <= 0,
		opts.RunMaxBytes <= 0, opts.IdleTimeout <= 0, opts.MaxConnectionLifetime <= 0, opts.ReadHeaderTimeout <= 0:
		return nil, errors.New("proxy: every limit must be positive")
	}
	s := &Server{
		opts:     opts,
		runs:     NewRuns(opts.RunMaxConnections, opts.RunMaxBytes, opts.Logger),
		sessions: map[*session]struct{}{},
	}
	s.httpSrv = &http.Server{
		Handler:           s,
		MaxHeaderBytes:    opts.MaxHeaderBytes,
		ReadHeaderTimeout: opts.ReadHeaderTimeout,
		IdleTimeout:       opts.IdleTimeout,
		// net/http's own messages (malformed requests, header overflows)
		// carry no request header values.
		ErrorLog: slog.NewLogLogger(opts.Logger.Handler(), slog.LevelDebug),
	}
	opts.Metrics.Gauge("egress_proxy_runs", "Runs this replica holds state for.", s.runs.Active)
	return s, nil
}

// Runs is the run registry, for the summary loop.
func (s *Server) Runs() *Runs { return s.runs }

// Serve accepts proxy connections on ln until Shutdown.
func (s *Server) Serve(ln net.Listener) error {
	err := s.httpSrv.Serve(newLimitListener(ln, s.opts.MaxConnections, s.opts.Metrics))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Draining reports whether Shutdown has begun.
func (s *Server) Draining() bool { return s.draining.Load() }

// SetDraining marks the server as draining before Shutdown, so readiness
// fails while the listener still accepts.
func (s *Server) SetDraining() { s.draining.Store(true) }

// Shutdown stops accepting, waits for open tunnels and forwarded requests,
// and closes the rest when ctx ends.
func (s *Server) Shutdown(ctx context.Context) error {
	s.draining.Store(true)
	err := s.httpSrv.Shutdown(ctx)
	if s.waitSessions(ctx.Done()) {
		return err
	}
	s.mu.Lock()
	for sess := range s.sessions {
		sess.stop(OutcomeShutdown)
	}
	s.mu.Unlock()
	_ = s.httpSrv.Close()
	// The closed connections end their relays at once; this bound only stops
	// a stuck handler from holding the process.
	forced, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.waitSessions(forced.Done())
	if err == nil {
		err = ctx.Err()
	}
	return err
}

// waitSessions polls until no session is open, or until stop closes. It
// polls rather than waits on a WaitGroup, because a handler that passed its
// checks before Shutdown may still open a session while Shutdown waits.
func (s *Server) waitSessions(stop <-chan struct{}) bool {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		s.mu.Lock()
		n := len(s.sessions)
		s.mu.Unlock()
		if n == 0 {
			return true
		}
		select {
		case <-stop:
			return false
		case <-t.C:
		}
	}
}

// record is one connection's log line.
type record struct {
	mode        string
	executionID string
	projectID   int64
	host        string
	port        int
	outcome     string
	reason      string
	start       time.Time
	up, down    int64
}

func (s *Server) log(rec *record) {
	attrs := []slog.Attr{
		slog.String("mode", rec.mode),
		slog.String("outcome", rec.outcome),
		slog.String("host", rec.host),
		slog.Int("port", rec.port),
		slog.Int64("bytes_up", rec.up),
		slog.Int64("bytes_down", rec.down),
		slog.Int64("duration_ms", s.opts.Now().Sub(rec.start).Milliseconds()),
	}
	if rec.executionID != "" {
		attrs = append(attrs, slog.String("execution_id", rec.executionID), slog.Int64("project_id", rec.projectID))
	}
	if rec.reason != "" {
		attrs = append(attrs, slog.String("reason", rec.reason))
	}
	s.opts.Logger.LogAttrs(context.Background(), slog.LevelInfo, ConnectionEvent, attrs...)
}

// refuse answers a refusal. msg is fixed text chosen here, never input.
func (s *Server) refuse(w http.ResponseWriter, rec *record, status int, reason, msg string) {
	rec.outcome = OutcomeRefused
	rec.reason = reason
	s.opts.Metrics.refusal(reason)
	h := w.Header()
	h.Set(RefusalHeader, reason)
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	if status == http.StatusProxyAuthRequired {
		h.Set("Proxy-Authenticate", `Basic realm="elitea-egress"`)
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "elitea-egress-proxy: "+msg+"\n")
}

// ServeHTTP handles one proxy request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &record{start: s.opts.Now(), mode: r.Method}
	defer s.log(rec)

	if s.draining.Load() {
		w.Header().Set("Connection", "close")
		s.refuse(w, rec, http.StatusServiceUnavailable, ReasonDraining, "this replica is shutting down; retry")
		return
	}

	// The shape of the request comes first: it decides where host and port
	// are, and a non-proxy request needs no token to be refused.
	var hostText, portText string
	connect := r.Method == http.MethodConnect
	switch {
	case connect:
		h, p, err := net.SplitHostPort(r.Host)
		if err != nil {
			s.refuse(w, rec, http.StatusBadRequest, ReasonBadTarget, "CONNECT needs host:port")
			return
		}
		hostText, portText = h, p
	case r.URL.IsAbs() && strings.EqualFold(r.URL.Scheme, "http"):
		hostText, portText = r.URL.Hostname(), r.URL.Port()
		if portText == "" {
			portText = "80"
		}
	case r.URL.IsAbs():
		s.refuse(w, rec, http.StatusForbidden, ReasonUnsupported,
			"only CONNECT (for TLS) and absolute-form http:// requests are proxied; use CONNECT for https")
		return
	default:
		s.refuse(w, rec, http.StatusBadRequest, ReasonUnsupported,
			"this is an egress proxy: send CONNECT host:port or an absolute-form http:// request")
		return
	}
	rec.host = truncate(hostText, 255)

	// The token.
	token, present := proxyToken(r)
	if token == "" {
		reason, msg := ReasonTokenMissing, "no run token: set HTTPS_PROXY=http://run:<token>@host:port"
		if present {
			reason, msg = ReasonTokenInvalid, "the proxy credential is not Basic with user \"run\""
		}
		s.refuse(w, rec, http.StatusProxyAuthRequired, reason, msg)
		return
	}
	verified, err := s.opts.Keys.Current().Verify(token, s.opts.Now())
	if err != nil {
		if errors.Is(err, runtoken.ErrExpired) {
			s.refuse(w, rec, http.StatusProxyAuthRequired, ReasonTokenExpired, "the run token has expired")
		} else {
			s.refuse(w, rec, http.StatusProxyAuthRequired, ReasonTokenInvalid, "the run token is not valid")
		}
		return
	}
	claims := verified.Claims
	rec.executionID, rec.projectID = claims.ExecutionID, claims.ProjectID

	// The target.
	port, err := runtoken.ParsePort(portText)
	if err != nil {
		s.refuse(w, rec, http.StatusBadRequest, ReasonBadTarget, "the port is not a number from 1 to 65535")
		return
	}
	rec.port = port
	target, err := runtoken.ParseTarget(hostText, port)
	if err != nil {
		s.refuse(w, rec, http.StatusBadRequest, ReasonBadTarget, "the host is not a valid host name or IP literal")
		return
	}
	rec.host = target.Host
	hp := hostPort{host: target.Host, port: port}
	refuseRun := func(status int, reason, msg string) {
		s.runs.refused(claims, hp, reason)
		s.refuse(w, rec, status, reason, msg)
	}

	if !verified.Allowlist.Allows(target) {
		switch {
		case target.IP != nil:
			refuseRun(http.StatusForbidden, ReasonIPLiteralNotAllowed, "an IP literal is allowed only when the project's allowlist names it")
		case verified.Allowlist.AllowsHost(target):
			msg := "the port is not allowed for this host; without a port in the allowlist entry only 443 and 80 are"
			if port == 22 {
				msg += "; git over SSH is not available in allowlist mode, use https"
			}
			refuseRun(http.StatusForbidden, ReasonPortNotAllowed, msg)
		default:
			msg := "the host is not in the project's egress allowlist"
			if port == 22 {
				msg += "; git over SSH is not available in allowlist mode, use https"
			}
			refuseRun(http.StatusForbidden, ReasonHostNotAllowed, msg)
		}
		return
	}

	// The run's limits.
	rn, reason := s.runs.acquire(claims)
	if rn == nil {
		if reason == ReasonRunConnectionLimit {
			refuseRun(http.StatusTooManyRequests, reason, "the run has too many open connections")
		} else {
			refuseRun(http.StatusForbidden, reason, "the run has used its byte budget")
		}
		return
	}
	reached := false
	defer func() { s.runs.release(rn, hp, rec.up, rec.down, reached) }()

	// The addresses. Every one must be public; one that is not refuses the
	// whole request, because a name that answers both is a rebinding setup.
	ips, err := addrcheck.Resolve(r.Context(), s.opts.Resolver, target.Host)
	if err != nil {
		refuseRun(http.StatusBadGateway, ReasonResolveFailed, "the host did not resolve")
		return
	}
	for _, ip := range ips {
		if addrcheck.Classify(ip) != addrcheck.Public || addrcheck.InAny(s.opts.DenyCIDRs, addrcheck.Canonical(ip)) {
			refuseRun(http.StatusForbidden, ReasonAddressNotPublic,
				"the host resolves to a private, loopback, link-local, metadata or cluster address")
			return
		}
	}
	if len(ips) == 0 {
		refuseRun(http.StatusBadGateway, ReasonResolveFailed, "the host did not resolve")
		return
	}

	deadline := rec.start.Add(s.opts.MaxConnectionLifetime)
	lifetimeOutcome := OutcomeLifetime
	if claims.ExpiresAt.Before(deadline) {
		deadline, lifetimeOutcome = claims.ExpiresAt, OutcomeTokenExpired
	}
	dial := func() (net.Conn, error) {
		dialCtx, cancel := context.WithTimeout(r.Context(), s.opts.DialTimeout)
		defer cancel()
		return addrcheck.DialFirst(dialCtx, s.opts.Dialer, "tcp", ips, strconv.Itoa(port))
	}

	if connect {
		// The session opens before the dial: the ClientHello peek runs inside
		// it, so the idle timeout, the lifetime and Shutdown cover a client
		// that never sends one. The dial comes only after the peek passed.
		sess := s.newSession(rn, nil, deadline, lifetimeOutcome)
		defer s.endSession(sess, rec)
		reached = s.tunnel(w, sess, rec, target, dial, func(reason string) {
			s.runs.refused(claims, hp, reason)
			rec.outcome, rec.reason = OutcomeRefused, reason
			s.opts.Metrics.refusal(reason)
		})
		return
	}

	upstream, err := dial()
	if err != nil {
		rec.outcome, rec.reason = OutcomeUpstreamError, ReasonDialFailed
		s.opts.Metrics.outcome(OutcomeUpstreamError)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "elitea-egress-proxy: could not connect to the destination\n")
		return
	}
	reached = true
	sess := s.newSession(rn, upstream, deadline, lifetimeOutcome)
	defer s.endSession(sess, rec)
	s.forward(w, r, sess)
}

// ReasonDialFailed is the log reason of an upstream-error outcome when the
// checked address did not accept the connection. It is not a refusal.
const ReasonDialFailed = "dial-failed"

// proxyToken reads the token from the one Proxy-Authorization header. present
// reports that a header was sent, so a malformed credential is told apart
// from a missing one.
func proxyToken(r *http.Request) (token string, present bool) {
	values := r.Header.Values("Proxy-Authorization")
	if len(values) == 0 || (len(values) == 1 && strings.TrimSpace(values[0]) == "") {
		return "", false
	}
	if len(values) != 1 {
		return "", true
	}
	scheme, cred, ok := strings.Cut(strings.TrimSpace(values[0]), " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return "", true
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cred))
	if err != nil {
		return "", true
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok || user != TokenUser || pass == "" {
		return "", true
	}
	return pass, true
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// session is one upstream connection and the client side it serves. A
// tunnel's session opens before its upstream exists (the ClientHello peek
// runs first); attach sets the upstream.
type session struct {
	srv      *Server
	run      *run
	deadline time.Time
	onLife   string

	mu       sync.Mutex
	upstream net.Conn
	client   net.Conn // set for a tunnel
	outcome  string
	closed   bool

	lastActivity atomic.Int64 // UnixNano
	up, down     atomic.Int64
	done         chan struct{}
}

func (s *Server) newSession(rn *run, upstream net.Conn, deadline time.Time, onLife string) *session {
	sess := &session{srv: s, run: rn, upstream: upstream, deadline: deadline, onLife: onLife, done: make(chan struct{})}
	sess.touch()
	s.mu.Lock()
	s.sessions[sess] = struct{}{}
	s.mu.Unlock()
	s.opts.Metrics.activeTunnels.Add(1)
	go sess.watch()
	return sess
}

func (s *Server) endSession(sess *session, rec *record) {
	sess.stop("")
	close(sess.done)
	s.mu.Lock()
	delete(s.sessions, sess)
	s.mu.Unlock()
	s.opts.Metrics.activeTunnels.Add(-1)

	rec.up, rec.down = sess.up.Load(), sess.down.Load()
	s.opts.Metrics.bytesUp.Add(rec.up)
	s.opts.Metrics.bytesDown.Add(rec.down)
	sess.mu.Lock()
	outcome := sess.outcome
	sess.mu.Unlock()
	if outcome == "" {
		outcome = OutcomeOK
	}
	// An outcome the handler already set (a peek refusal, a failed dial) was
	// counted where it was set.
	if rec.outcome == "" {
		rec.outcome = outcome
		s.opts.Metrics.outcome(rec.outcome)
	}
}

func (sess *session) touch() { sess.lastActivity.Store(sess.srv.opts.Now().UnixNano()) }

// stop closes both sides once. The first non-empty outcome wins.
func (sess *session) stop(outcome string) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.outcome == "" && outcome != "" {
		sess.outcome = outcome
	}
	if sess.closed {
		return
	}
	sess.closed = true
	if sess.upstream != nil {
		_ = sess.upstream.Close()
	}
	if sess.client != nil {
		_ = sess.client.Close()
	}
}

// attach sets the upstream of a tunnel. It reports false, and closes c, when
// the session was stopped meanwhile.
func (sess *session) attach(c net.Conn) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed {
		_ = c.Close()
		return false
	}
	sess.upstream = c
	return true
}

func (sess *session) isClosed() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.closed
}

// watch enforces the idle timeout and the lifetime.
func (sess *session) watch() {
	opts := sess.srv.opts
	tick := opts.IdleTimeout / 10
	if tick > time.Second {
		tick = time.Second
	}
	if tick < 5*time.Millisecond {
		tick = 5 * time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-sess.done:
			return
		case <-t.C:
			now := opts.Now()
			if !now.Before(sess.deadline) {
				sess.stop(sess.onLife)
				return
			}
			if now.Sub(time.Unix(0, sess.lastActivity.Load())) >= opts.IdleTimeout {
				sess.stop(OutcomeIdleTimeout)
				return
			}
		}
	}
}

// relay copies src to dst, counting against the run's budget. up is the
// sandbox-to-destination direction.
func (sess *session) relay(dst io.Writer, src io.Reader, up bool, flush func()) error {
	buf := make([]byte, copyBufferBytes)
	counter := &sess.down
	if up {
		counter = &sess.up
	}
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			sess.touch()
			if !sess.run.spend(n, sess.srv.opts.RunMaxBytes) {
				sess.stop(OutcomeByteLimit)
				return errors.New("run byte limit")
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			counter.Add(int64(n))
			if flush != nil {
				flush()
			}
			sess.touch()
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}

// tunnel answers 200 to a CONNECT, peeks the ClientHello, dials, and
// splices the two connections. It reports whether the destination was
// dialled.
//
// The order is 200, peek, check, dial. A client sends its ClientHello only
// after the 200, so the peek cannot come first; dialling only after the check
// means a refused tunnel never opens an upstream connection, and the
// destination never sees a byte of it. The cost: a failed dial can no longer
// answer 502, so the tunnel closes after the 200 (logged as upstream-error,
// dial-failed).
func (s *Server) tunnel(w http.ResponseWriter, sess *session, rec *record, target runtoken.Target,
	dial func() (net.Conn, error), refuse func(reason string)) bool {
	hj, ok := w.(http.Hijacker)
	if !ok {
		sess.stop(OutcomeUpstreamError)
		w.WriteHeader(http.StatusInternalServerError)
		return false
	}
	client, brw, err := hj.Hijack()
	if err != nil {
		sess.stop(OutcomeUpstreamError)
		return false
	}
	// The server may have set deadlines on the connection; the peek and then
	// the watchdog own the timeouts from here.
	_ = client.SetDeadline(time.Time{})
	sess.mu.Lock()
	if sess.closed {
		sess.mu.Unlock()
		_ = client.Close()
		return false
	}
	sess.client = client
	sess.mu.Unlock()

	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		sess.stop(OutcomeOK)
		return false
	}

	// brw.Reader holds any bytes the client sent after its CONNECT headers.
	prefix, reason, proceed := s.peekClientHello(sess, client, brw.Reader, target)
	if !proceed {
		if reason != "" {
			refuse(reason)
		}
		sess.stop("")
		return false
	}

	upstream, err := dial()
	if err != nil {
		rec.outcome, rec.reason = OutcomeUpstreamError, ReasonDialFailed
		s.opts.Metrics.outcome(OutcomeUpstreamError)
		sess.stop("")
		return false
	}
	if !sess.attach(upstream) {
		return true
	}

	var wg sync.WaitGroup
	wg.Add(2)
	// Every peeked byte goes to the destination first, unchanged, and counts
	// against the run's budget like any other byte.
	up := io.MultiReader(bytes.NewReader(prefix), brw.Reader)
	go func() {
		defer wg.Done()
		if err := sess.relay(upstream, up, true, nil); err == nil {
			closeWrite(upstream)
		} else {
			sess.stop("")
		}
	}()
	go func() {
		defer wg.Done()
		if err := sess.relay(client, upstream, false, nil); err == nil {
			closeWrite(client)
		} else {
			sess.stop("")
		}
	}()
	wg.Wait()
	return true
}

// peekClientHello reads the client's first bytes, at most clientHelloMaxBytes
// within clientHelloTimeout, and decides on them:
//
//   - a ClientHello whose server_name is the CONNECT host (ASCII
//     case-insensitive, one trailing dot ignored) passes;
//   - a ClientHello without server_name passes only when the CONNECT host is
//     an IP literal (sni-missing otherwise); a server_name on an IP-literal
//     CONNECT must be that literal (sni-mismatch otherwise);
//   - with ECH the server_name checked is the outer one, the ECH public name
//     (see clienthello.go);
//   - bytes that do not start with a TLS handshake record are not-tls on port
//     443, and pass unchanged on every other port;
//   - a malformed or oversize ClientHello, or none in time, is refused.
//
// It returns the bytes read, to be replayed to the destination, and whether
// the tunnel proceeds. A refusal has a reason; a client that closed, or a
// session the watchdog stopped, has none. A refused ClientHello gets a TLS
// access_denied alert before the close.
func (s *Server) peekClientHello(sess *session, client net.Conn, br *bufio.Reader,
	target runtoken.Target) (prefix []byte, reason string, proceed bool) {
	limit := s.opts.clientHelloMaxBytes
	_ = client.SetReadDeadline(time.Now().Add(s.opts.clientHelloTimeout))
	defer func() { _ = client.SetReadDeadline(time.Time{}) }()

	refuseTLS := func(reason string) ([]byte, string, bool) {
		_ = client.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = client.Write(alertAccessDenied)
		_ = client.SetWriteDeadline(time.Time{})
		return nil, reason, false
	}
	buf := make([]byte, 0, 2048)
	chunk := make([]byte, 4096)
	for {
		room := limit - len(buf)
		if room <= 0 {
			return refuseTLS(ReasonClientHelloTooLarge)
		}
		n, err := br.Read(chunk[:min(room, len(chunk))])
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			hello, complete, perr := scanClientHello(buf, limit)
			switch {
			case errors.Is(perr, errNotTLS):
				if target.Port == 443 {
					return nil, ReasonNotTLS, false
				}
				return buf, "", true
			case errors.Is(perr, errHelloTooLarge):
				return refuseTLS(ReasonClientHelloTooLarge)
			case perr != nil:
				return refuseTLS(ReasonClientHelloMalformed)
			case complete && !hello.hasServerName && target.IP == nil:
				return refuseTLS(ReasonSNIMissing)
			case complete && hello.hasServerName && !sameHost(hello.serverName, target.Host):
				return refuseTLS(ReasonSNIMismatch)
			case complete:
				return buf, "", true
			}
		}
		if err != nil {
			if sess.isClosed() {
				return nil, "", false
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return refuseTLS(ReasonSNITimeout)
			}
			// The client went away before its ClientHello was complete.
			return nil, "", false
		}
	}
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// hopHeaders are connection-scoped and never forwarded (RFC 9110 7.6.1), in
// either direction. Proxy-Authorization is among them, so the token never
// leaves the proxy.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHopHeaders(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}

// headerCapReader bounds the bytes read while the response headers are read,
// and keeps them: net/http's ReadResponse deletes a Connection header that
// says "close", and with it the other hop-by-hop names that header listed.
type headerCapReader struct {
	r         io.Reader
	remaining int64
	capped    bool
	captured  []byte
}

func (h *headerCapReader) Read(p []byte) (int, error) {
	if !h.capped {
		return h.r.Read(p)
	}
	if h.remaining <= 0 {
		return 0, errors.New("upstream response headers are too large")
	}
	if int64(len(p)) > h.remaining {
		p = p[:h.remaining]
	}
	n, err := h.r.Read(p)
	h.remaining -= int64(n)
	h.captured = append(h.captured, p[:n]...)
	return n, err
}

// connectionTokens returns the names every Connection header line in raw
// lists. raw is the header block exactly as the destination sent it.
func connectionTokens(raw []byte) []string {
	var names []string
	for _, line := range strings.Split(string(raw), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Connection") {
			continue
		}
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				names = append(names, token)
			}
		}
	}
	return names
}

// countingReader counts and budgets the request body, up.
type countingReader struct {
	sess *session
	r    io.Reader
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.sess.touch()
		if !c.sess.run.spend(n, c.sess.srv.opts.RunMaxBytes) {
			c.sess.stop(OutcomeByteLimit)
			return 0, errors.New("run byte limit")
		}
		c.sess.up.Add(int64(n))
	}
	return n, err
}

// forward sends one absolute-form http:// request to the checked upstream
// and relays the response as it is. A redirect is relayed, never followed.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, sess *session) {
	out := *r.URL
	out.User = nil
	var body io.Reader
	if r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0 {
		body = &countingReader{sess: sess, r: r.Body}
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, out.String(), body)
	if err != nil {
		sess.stop(OutcomeUpstreamError)
		s.badGateway(w)
		return
	}
	req.Header = r.Header.Clone()
	stripHopHeaders(req.Header)
	// The proxy relays one final response; it does not take part in 100-continue.
	req.Header.Del("Expect")
	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header["User-Agent"] = []string{""} // write no default User-Agent
	}
	req.Host = r.Host
	req.ContentLength = r.ContentLength
	if body == nil {
		req.ContentLength = 0
	}
	req.Close = true

	if err := req.Write(sess.upstream); err != nil {
		if !sess.isClosed() {
			sess.stop(OutcomeUpstreamError)
		}
		s.badGateway(w)
		return
	}
	capReader := &headerCapReader{r: sess.upstream, remaining: int64(s.opts.MaxHeaderBytes), capped: true}
	br := bufio.NewReader(capReader)
	var resp *http.Response
	for {
		resp, err = http.ReadResponse(br, req)
		if err != nil {
			sess.stop(OutcomeUpstreamError)
			s.badGateway(w)
			return
		}
		// 1xx interim responses are skipped; 101 is never asked for, because
		// Upgrade is a hop-by-hop header the proxy removed.
		if resp.StatusCode >= 200 || resp.StatusCode == http.StatusSwitchingProtocols {
			break
		}
		_ = resp.Body.Close()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		sess.stop(OutcomeUpstreamError)
		s.badGateway(w)
		return
	}
	// The bytes consumed so far are exactly the header blocks; what bufio
	// holds beyond them is body.
	consumed := capReader.captured[:len(capReader.captured)-br.Buffered()]
	capReader.capped, capReader.captured = false, nil
	sess.touch()

	h := w.Header()
	for k, v := range resp.Header {
		h[k] = v
	}
	for _, name := range connectionTokens(consumed) {
		h.Del(name)
	}
	stripHopHeaders(h)
	w.WriteHeader(resp.StatusCode)
	var flush func()
	if f, ok := w.(http.Flusher); ok {
		flush = f.Flush
	}
	if err := sess.relay(w, resp.Body, false, flush); err != nil {
		sess.stop("")
	}
}

func (s *Server) badGateway(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = io.WriteString(w, "elitea-egress-proxy: the destination did not answer with a valid HTTP response\n")
}

// limitListener bounds client connections. A connection over the limit gets
// a 503 and is closed at once; it never waits in the accept queue.
type limitListener struct {
	net.Listener
	sem     chan struct{}
	metrics *Metrics
}

func newLimitListener(ln net.Listener, max int, m *Metrics) net.Listener {
	return &limitListener{Listener: ln, sem: make(chan struct{}, max), metrics: m}
}

const overLimitResponse = "HTTP/1.1 503 Service Unavailable\r\n" + RefusalHeader + ": " + ReasonProxyConnectionLimit +
	"\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\nContent-Length: 48\r\n\r\n" +
	"elitea-egress-proxy: too many connections; retry"

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.sem <- struct{}{}:
			l.metrics.activeConnections.Add(1)
			return &trackedConn{Conn: c, release: func() {
				<-l.sem
				l.metrics.activeConnections.Add(-1)
			}}, nil
		default:
			l.metrics.refusal(ReasonProxyConnectionLimit)
			go func() {
				_ = c.SetDeadline(time.Now().Add(time.Second))
				_, _ = io.WriteString(c, overLimitResponse)
				_ = c.Close()
			}()
		}
	}
}

type trackedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

func (c *trackedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return fmt.Errorf("proxy: connection has no half-close")
}
