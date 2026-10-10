package spi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// App is a sub-application as the host sees it: a name, a descriptor, an
// admission table and a runner. Everything else on the wire is the host's.
type App struct {
	// Name is the provider's name, reported as `plugin` by /health.
	Name string
	// Version is reported as providerVersion.
	Version string
	// Descriptor returns the provider's self-description for the service
	// location the host is configured with. The document is the
	// application's; the host injects nothing but the URL it is given.
	Descriptor func(serviceLocationURL string) any
	Toolkits   Toolkits
	Runner     Runner
}

// Validate refuses an application the host could not serve honestly.
func (a App) Validate() error {
	if a.Name == "" || a.Version == "" || a.Descriptor == nil {
		return fmt.Errorf("%w: an application needs a name, a version and a descriptor", ErrConfig)
	}
	if a.Runner == nil {
		return fmt.Errorf("%w: application %q has no runner (use UnavailableRunner to serve without one)", ErrConfig, a.Name)
	}
	return a.Toolkits.Validate()
}

// Server serves the SPI for one application.
type Server struct {
	settings  Settings
	app       App
	manager   *Manager
	logger    *slog.Logger
	startedAt time.Time
	ready     func(context.Context) map[string]bool
	handler   http.Handler
}

// Option adjusts a Server.
type Option func(*Server)

// WithStore selects the invocation store (default: in memory).
func WithStore(store Store) Option {
	return func(s *Server) {
		s.manager = NewManager(store, time.Duration(s.settings.InvocationRetentionSeconds)*time.Second, s.logger)
	}
}

// WithReadiness adds checks /ready reports; every check must pass for READY.
func WithReadiness(check func(context.Context) map[string]bool) Option {
	return func(s *Server) { s.ready = check }
}

// NewServer composes the host.
func NewServer(settings Settings, app App, logger *slog.Logger, options ...Option) (*Server, error) {
	if err := app.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{settings: settings, app: app, logger: logger, startedAt: time.Now()}
	s.manager = NewManager(nil, time.Duration(settings.InvocationRetentionSeconds)*time.Second, logger)
	for _, option := range options {
		option(s)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /descriptor", s.descriptor)
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /ready", s.readiness)
	mux.HandleFunc("GET /slots", s.slots)
	mux.HandleFunc("POST "+DeleteProjectPath, s.deleteProject)
	mux.HandleFunc("POST /tools/{toolkit}/{tool}/invoke", s.invoke)
	mux.HandleFunc("GET /tools/{toolkit}/{tool}/invocations/{invocation}", s.poll)
	mux.HandleFunc("DELETE /tools/{toolkit}/{tool}/invocations/{invocation}", s.cancel)
	s.handler = s.identityGate(s.mtlsGate(mux))
	return s, nil
}

// Manager exposes the registry (for tests and for an embedding main).
func (s *Server) Manager() *Manager { return s.manager }

// Start begins housekeeping; Stop ends every in-flight invocation.
func (s *Server) Start(ctx context.Context) { s.manager.Start(ctx) }
func (s *Server) Stop()                     { s.manager.Stop() }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// unauthenticatedPaths are reachable without mTLS or an identity: the probes.
// Requiring a client certificate on them would empty a rotation the moment
// mutual TLS went on.
var unauthenticatedPaths = map[string]bool{"/health": true, "/ready": true}

// mtlsGate refuses a hop that is not mutually authenticated when a client CA
// is configured. When THIS process terminates TLS with a client CA, the
// listener already required and verified a client certificate at the
// handshake, so a request that reached the handler over TLS is the proof;
// the peer certificates are checked all the same, because Go exposes them
// and a check that costs nothing is worth having. A cleartext hop is 421; a
// TLS hop with no verified client certificate is 496 — the legacy shell's
// two refusals, which the facade already parses.
func (s *Server) mtlsGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.settings.MTLSRequired() || unauthenticatedPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		if r.TLS == nil {
			s.logger.Warn("refusing a cleartext hop", "method", r.Method, "path", r.URL.Path)
			writeJSON(w, http.StatusMisdirectedRequest, TransportError(http.StatusMisdirectedRequest, "Misdirected Request"))
			return
		}
		if len(r.TLS.VerifiedChains) == 0 && !s.settings.TerminatesMTLS() {
			s.logger.Warn("refusing a hop with no verified client certificate", "method", r.Method, "path", r.URL.Path)
			writeJSON(w, 496, TransportError(496, "No Client Certificate"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// identityGate strips every identity header a caller presented, then puts
// back only what a valid signature vouches for. With a secret configured
// and mTLS required, a request off the probes with no valid signature is
// refused; without mTLS the headers are merely dropped, so a dev stack
// still serves. A dropped identity is EMPTY, never the body's: a runner
// whose tools address tenant data must refuse a call with no verified
// project (inventory's run.Runner.RequireVerifiedProject) rather than read
// the project from the body the caller writes.
func (s *Server) identityGate(next http.Handler) http.Handler {
	secret := []byte(s.settings.IdentitySecret)
	required := s.settings.MTLSRequired()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity := Identity{}
		if strings.HasPrefix(r.URL.Path, internalPrefix) {
			// The platform routes are authorised by the client certificate
			// alone (platform.go): no identity is read, and none is required.
			StripIdentityHeaders(r.Header)
			next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), identity)))
			return
		}
		if len(secret) > 0 {
			switch {
			case VerifySignature(r.Header, secret):
				identity = IdentityFromHeaders(r.Header)
			case required && !unauthenticatedPaths[r.URL.Path]:
				s.logger.Warn("refusing a hop with a missing or invalid identity signature", "method", r.Method, "path", r.URL.Path)
				writeJSON(w, http.StatusUnauthorized, TransportError(http.StatusUnauthorized, "Unauthorized"))
				return
			default:
				s.logger.Warn("dropping unverified identity headers", "method", r.Method, "path", r.URL.Path)
			}
		}
		StripIdentityHeaders(r.Header)
		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), identity)))
	})
}

type identityKey struct{}

func withIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, identity)
}

// IdentityFromContext is the verified identity of the caller, empty when the
// hop carried none.
func IdentityFromContext(ctx context.Context) Identity {
	identity, _ := ctx.Value(identityKey{}).(Identity)
	return identity
}

func (s *Server) descriptor(w http.ResponseWriter, _ *http.Request) {
	// The descriptor is written WITHOUT Go's HTML escaping, unlike every
	// other body: it is a document the application supplies verbatim, and
	// the golden fixtures were recorded from Python's json.dumps, which
	// leaves <, > and & alone. Escaping them is JSON-equivalent but not
	// byte-equal, and the descriptor is pinned byte for byte — Inventory's
	// recorded text says "Jira ticket -> GitHub PR". Only this route is
	// affected; nothing else the host serves changes shape.
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(s.app.Descriptor(s.settings.ServiceLocationURL)); err != nil {
		writeJSON(w, http.StatusOK, s.app.Descriptor(s.settings.ServiceLocationURL))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bytes.TrimRight(buffer.Bytes(), "\n"))
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	hostname := os.Getenv("HOSTNAME")
	if hostname == "" {
		hostname = os.Getenv("POD_NAME")
	}
	if hostname == "" {
		hostname = "unknown"
	}
	podIP := os.Getenv("POD_IP")
	if podIP == "" {
		podIP = "unknown"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "UP",
		"providerVersion": s.app.Version,
		"uptime":          int(time.Since(s.startedAt).Seconds()),
		"timestamp":       time.Now().UTC().Format("2006-01-02T15:04:05+00:00"),
		"plugin":          s.app.Name,
		"configuration": map[string]any{
			"scratch_path":         s.settings.ScratchPath,
			"service_location_url": s.settings.ServiceLocationURL,
		},
		"extra_info": map[string]any{
			"hostname":            hostname,
			"pod_ip":              podIP,
			"durable_invocations": s.manager.Store().Durable(),
			"runner":              s.app.Runner.Name(),
			"mtls_required":       s.settings.MTLSRequired(),
			"identity_verified":   s.settings.IdentitySecret != "",
			"git_egress":          ParseEgressPolicy(s.settings.GitAllowlist).Describe(),
		},
	})
}

func (s *Server) readiness(w http.ResponseWriter, r *http.Request) {
	checks := map[string]bool{"invocations": true}
	if s.ready != nil {
		for name, ok := range s.ready(r.Context()) {
			checks[name] = ok
		}
	}
	status, code := "READY", http.StatusOK
	for _, ok := range checks {
		if !ok {
			status, code = "NOT_READY", http.StatusServiceUnavailable
		}
	}
	writeJSON(w, code, map[string]any{"status": status, "checks": checks})
}

func (s *Server) slots(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Slots(s.settings, s.manager.InFlight()))
}

// maxInvokeBytes bounds a request body: large enough for an expanded
// toolkit configuration, small enough that a client cannot hold a goroutine
// on a stream that never ends.
const maxInvokeBytes = 4 << 20

func (s *Server) invoke(w http.ResponseWriter, r *http.Request) {
	toolkit, tool := r.PathValue("toolkit"), r.PathValue("tool")
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxInvokeBytes+1))
	if err != nil || len(raw) > maxInvokeBytes {
		writeJSON(w, http.StatusBadRequest, TransportError(http.StatusBadRequest, "Bad Request"))
		return
	}
	var request map[string]any
	if err := json.Unmarshal(raw, &request); err != nil || request == nil {
		writeJSON(w, http.StatusBadRequest, TransportError(http.StatusBadRequest, "Bad Request"))
		return
	}
	// Admission runs INSIDE the invocation: an unknown toolkit or tool is
	// accepted with an id and terminates as resource_not_found on the first
	// poll, which is what the recording shows and what the facade's poll loop
	// expects. Refusing at the door would be tidier and would break both.
	identity := IdentityFromContext(r.Context())
	call := func(ctx context.Context, tc *Context) (map[string]any, error) {
		family, err := s.app.Toolkits.Resolve(toolkit)
		if err != nil {
			return nil, err
		}
		if err := s.app.Toolkits.Admit(family, tool); err != nil {
			return nil, err
		}
		return s.app.Runner.Invoke(ctx, Invoke{Family: family, Toolkit: toolkit, Tool: tool, Request: request, Identity: identity}, tc)
	}
	invocation, err := s.manager.Submit(r.Context(), toolkit, tool, call)
	if err != nil {
		s.logger.Error("failed to accept an invocation", "toolkit", toolkit, "tool", tool, "error", err)
		writeJSON(w, http.StatusInternalServerError, TransportError(http.StatusInternalServerError, "Internal Server Error"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invocation_id": invocation.ID, "status": wireStarted})
}

func (s *Server) poll(w http.ResponseWriter, r *http.Request) {
	body, err := s.manager.Poll(r.Context(), r.PathValue("toolkit"), r.PathValue("tool"), r.PathValue("invocation"))
	if err != nil {
		s.logger.Error("poll failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, TransportError(http.StatusInternalServerError, "Internal Server Error"))
		return
	}
	if body == nil {
		writeJSON(w, http.StatusNotFound, TransportError(http.StatusNotFound, "Resource Not Found"))
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	known, err := s.manager.Cancel(r.Context(), r.PathValue("toolkit"), r.PathValue("tool"), r.PathValue("invocation"))
	if err != nil {
		s.logger.Error("cancel failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, TransportError(http.StatusInternalServerError, "Internal Server Error"))
		return
	}
	if !known {
		writeJSON(w, http.StatusNotFound, TransportError(http.StatusNotFound, "Resource Not Found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		encoded = []byte(`{"errorCode":"500","message":"Internal Server Error","details":[]}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}
