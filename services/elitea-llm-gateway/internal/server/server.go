// Package server wires bifrost/core into the gateway's HTTP server and owns
// the process lifecycle (Init, serve, graceful shutdown).
//
// The /llm chi handler, converter, and SSE loop are added in task BF0.3; this
// package provides the bifrost initialisation and the §9.5 server/shutdown
// settings that the rest of the gateway builds on.
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/sony/gobreaker/v2"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/bifrostlog"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/config"
	natsinfra "github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/infra/nats"
)

// NATSClient is the budget-path surface the gateway server owns and hands to
// downstream governance wiring (the GovernanceStore + tiered-hybrid FSM).
// *nats.Client satisfies it; defining it here as an interface is the seam that
// lets server tests inject a fake without a live NATS server.
//
// The interface includes all methods that governance.GovernanceStore and
// failmode.Reconciler need so main.go can pass srv.NATS() directly:
//   - IncrBudgetIdempotent: idempotent counter increment (governance + reconciler)
//   - OnBreakerStateChange: breaker-edge wiring (governance, reconciler)
//   - BreakerState: current breaker state (governance)
//   - BudgetSubject: subject formatter used by the reconciler (failmode.Counter)
type NATSClient interface {
	IncrBudget(ctx context.Context, subject string, deltaNano int64) (int64, error)
	IncrBudgetIdempotent(ctx context.Context, subject, eventID string, deltaNano int64) (total int64, applied bool, err error)
	ReadBudget(ctx context.Context, subject string) (int64, error)
	TryAlertCooldown(ctx context.Context, key string) (bool, error)
	PublishDelta(ctx context.Context, eventID string, payload []byte) error
	// PublishSoftAlertEvent emits budget.soft_alert onto gateway.events.*
	// (spec §8.3); satisfies llmproxy.AlertEventPublisher.
	PublishSoftAlertEvent(ctx context.Context, projectID string, event []byte) error
	// PublishOpsEvent emits operator-only events (budget.unbilled_stream,
	// issue #9) onto gateway.events.ops.*; satisfies llmproxy.OpsEventPublisher.
	// Separate from the soft-alert publisher because that one is tenant-facing.
	PublishOpsEvent(ctx context.Context, event []byte) error
	OnBreakerStateChange(fn func(from, to gobreaker.State))
	BreakerState() gobreaker.State
	// BudgetSubject builds the counter subject for a scope/period key. It must
	// match the formula used by the NATS client when writing counters.
	BudgetSubject(scope, scopeID string, periodStartUnix int64) string
	Close()
}

// natsConnector opens a hardened NATS client. It defaults to nats.Connect and
// is overridable in tests via WithNATSConnector so the server lifecycle
// (connect on start, close on shutdown) is verifiable offline.
type natsConnector func(ctx context.Context, cfg natsinfra.Config) (NATSClient, error)

// defaultNATSConnector adapts nats.Connect to the NATSClient interface.
func defaultNATSConnector(ctx context.Context, cfg natsinfra.Config) (NATSClient, error) {
	return natsinfra.Connect(ctx, cfg)
}

// Option customises Server construction. It exists for the injectable NATS
// connector seam; production code passes none and gets the real connector.
type Option func(*options)

type options struct {
	natsConnect natsConnector
}

// WithNATSConnector overrides the NATS connector (tests inject a fake).
func WithNATSConnector(fn natsConnector) Option {
	return func(o *options) { o.natsConnect = fn }
}

// ErrNATSNotConfigured is returned by RedialNATS when GATEWAY_NATS_URL is
// unset. A NATS-less deployment is a deliberate posture, not a fault, so a
// caller must not retry it.
var ErrNATSNotConfigured = errors.New("server: NATS is not configured")

// ErrServerClosed is returned by RedialNATS after Close. It stops a background
// re-dial from opening a connection that nothing will ever close.
var ErrServerClosed = errors.New("server: closed")

// Server owns the embedded bifrost/core client and the HTTP server that
// fronts the /llm surface.
type Server struct {
	cfg    config.Config
	core   *bifrost.Bifrost
	http   *http.Server
	logger *slog.Logger

	// natsConnect is the connector New used. It is kept so a re-dial goes
	// through the SAME seam a test injects (issue #315).
	natsConnect natsConnector

	// natsMu guards nats and closed. The client is dialled once in New, but a
	// gateway that started without one re-dials in the background, so the field
	// has a writer that runs while /readyz and the shutdown path read it.
	natsMu sync.Mutex
	nats   NATSClient // nil when GATEWAY_NATS_URL is unset (NATS disabled)
	closed bool
}

// New initialises bifrost/core with the injected slog/OTel logger and the
// §9.5 pool/concurrency settings, then constructs the HTTP server with the
// SSE-safe timeout profile.
//
// account is the schemas.Account bifrost uses to resolve provider
// credentials. Passing nil falls back to a bootstrap account (zero configured
// providers) so the server can start before the vault-backed Account
// (BF0.2-account) is wired.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger, level *slog.LevelVar, account schemas.Account, handler http.Handler, opts ...Option) (*Server, error) {
	if account == nil {
		account = newBootstrapAccount(cfg.ProviderConcurrency)
	}

	o := options{natsConnect: defaultNATSConnector}
	for _, opt := range opts {
		opt(&o)
	}

	// §6.1: a Logger MUST be injected or bifrost.Init overwrites the zerolog
	// global. §9.5: tune InitialPoolSize down to fit the memory limit.
	core, err := bifrost.Init(ctx, schemas.BifrostConfig{
		Account:         account,
		Logger:          bifrostlog.New(logger, level),
		InitialPoolSize: cfg.InitialPoolSize,
	})
	if err != nil {
		return nil, fmt.Errorf("bifrost.Init: %w", err)
	}

	// Connect the budget-path NATS client when configured. An unset URL
	// disables NATS wiring (dev/test). A configured-but-unreachable NATS at
	// startup is logged and left nil rather than aborting boot: the
	// tiered-hybrid fail-mode FSM (design §8.5, a later BF0.4 subtask) owns the
	// enforcement policy while NATS is unavailable, and a gateway that refuses
	// to start on a NATS blip cannot serve /llm at all. The bootstrap chart is
	// the authoritative owner of the KV/stream assets.
	var natsClient NATSClient
	if cfg.NATSURL != "" {
		nc, nerr := o.natsConnect(ctx, natsClientConfig(cfg))
		if nerr != nil {
			logger.Warn("NATS budget path unavailable at startup; continuing without enforcement wiring",
				"err", nerr, "url", redactURL(cfg.NATSURL), "nats_auth", natsAuthMode(cfg))
		} else {
			natsClient = nc
			logNATSPosture(logger, "NATS budget path connected", cfg)
		}
	} else {
		logger.Info("NATS budget path disabled (GATEWAY_NATS_URL unset)")
	}

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: handler,
		// A finite ReadHeaderTimeout is safe and bounds slow-header attacks;
		// the streaming request bodies themselves are not time-bounded here.
		ReadHeaderTimeout: 10 * time.Second,
		// §9.5: WriteTimeout MUST be 0 (disabled) for the /llm SSE path. A
		// finite per-connection write deadline hard-kills any active SSE
		// response in normal operation. Per-stream deadlines are cleared in
		// the handler via http.NewResponseController before the first Flush.
		WriteTimeout: 0,
	}

	return &Server{
		cfg:         cfg,
		core:        core,
		http:        srv,
		logger:      logger,
		natsConnect: o.natsConnect,
		nats:        natsClient,
	}, nil
}

// Core exposes the embedded bifrost client to handlers (request methods live
// in the llmproxy package added by BF0.3).
func (s *Server) Core() *bifrost.Bifrost { return s.core }

// NATS exposes the budget-path client to downstream governance wiring (the
// GovernanceStore + tiered-hybrid FSM added by later BF0.4 subtasks). It is
// nil when NATS is disabled or was unreachable at startup; callers MUST nil-check.
func (s *Server) NATS() NATSClient {
	s.natsMu.Lock()
	defer s.natsMu.Unlock()
	return s.nats
}

// RedialNATS opens the budget-path client again, and reports the live client
// when one is already connected.
//
// It exists because New dials EXACTLY ONCE, and nats.go only resurrects a
// connection that succeeded at least once: a failed initial dial leaves the
// client nil for the life of the process, so budget enforcement never comes
// back on its own (issues #304, #315). A caller polls this method, and installs
// enforcement on the running handler when it returns a client.
//
// It never opens a second connection: the whole method holds one mutex, and a
// caller that arrives after another succeeded gets the same client back.
func (s *Server) RedialNATS(ctx context.Context) (NATSClient, error) {
	if s.cfg.NATSURL == "" {
		return nil, ErrNATSNotConfigured
	}
	s.natsMu.Lock()
	defer s.natsMu.Unlock()
	if s.closed {
		return nil, ErrServerClosed
	}
	if s.nats != nil {
		return s.nats, nil
	}
	nc, err := s.natsConnect(ctx, natsClientConfig(s.cfg))
	if err != nil {
		return nil, fmt.Errorf("server: re-dial NATS: %w", err)
	}
	s.nats = nc
	logNATSPosture(s.logger, "NATS budget path reconnected", s.cfg)
	return nc, nil
}

// ListenAndServe starts the HTTP server. When TLSCertFile and TLSKeyFile are
// both configured it switches to TLS; if TLSCAFile is also set the server
// requires and verifies client certificates (mTLS). Falls back to plain HTTP
// when the cert/key paths are unset (local/dev).
//
// FIX #10: wire GATEWAY_TLS_CERT_FILE / GATEWAY_TLS_KEY_FILE / GATEWAY_TLS_CA_FILE.
func (s *Server) ListenAndServe() error {
	s.logger.Info("gateway listening", "addr", s.cfg.HTTPAddr)
	if s.cfg.TLSCertFile != "" && s.cfg.TLSKeyFile != "" {
		tlsCfg, err := buildTLSConfig(s.cfg)
		if err != nil {
			return fmt.Errorf("gateway tls config: %w", err)
		}
		s.http.TLSConfig = tlsCfg
		s.logger.Info("gateway TLS enabled", "cert", s.cfg.TLSCertFile, "mtls", s.cfg.TLSCAFile != "")
		if err := s.http.ListenAndServeTLS(s.cfg.TLSCertFile, s.cfg.TLSKeyFile); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}
	if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// redactURL removes the userinfo component (username:password@ or token@)
// from a URL before logging so credentials are never written to log streams
// (Fix round-3 #10). Returns the original string on any parse failure so the
// caller always gets a printable value.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// buildTLSConfig builds a *tls.Config for the server. When cfg.TLSCAFile is
// set the server requires and verifies client certificates (mTLS).
func buildTLSConfig(cfg config.Config) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Advertise http/1.1 ONLY. This is a correctness requirement of the
		// realtime WebSocket route (/llm/v1/realtime), not a tuning choice.
		//
		// ListenAndServeTLS adds "h2" to NextProtos when the field is empty, so
		// the listener negotiated HTTP/2 with any client that offered it. A
		// WebSocket upgrade needs the raw TCP connection, and net/http gets it
		// by hijacking the ResponseWriter — but an HTTP/2 ResponseWriter serves
		// ONE STREAM of a multiplexed connection, so it implements neither
		// http.Hijacker nor an Unwrap chain that reaches one. RFC 8441
		// (extended CONNECT) is the HTTP/2 way to carry WebSocket, and neither
		// net/http nor this gateway implements it.
		//
		// Without this line the failure is an opaque "not a hijacker" error
		// raised deep inside the accept, AFTER the caller believed the
		// handshake had started. With it, an h2-only client fails at ALPN with
		// a protocol-negotiation error naming the cause.
		//
		// Production survives today only because elitea-main's proxy transport
		// pins http/1.1; a direct in-cluster h2 client does not.
		NextProtos: []string{"http/1.1"},
	}
	if cfg.TLSCAFile != "" {
		caBytes, err := os.ReadFile(cfg.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, fmt.Errorf("no valid certificates found in CA file %q", cfg.TLSCAFile)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return tlsCfg, nil
}

// Shutdown drains in-flight streams and releases every resource, HTTP first
// and NATS last. It is the whole-lifecycle convenience form, kept for callers
// (and tests) that have no billing drain to interleave.
//
// The composition root does NOT use it: it needs to bill between the two
// halves, so it calls ShutdownHTTP, drains billing, then Close. See
// shutdownSequence in cmd/elitea-llm-gateway.
//
// §9.5: the shutdown context timeout MUST be ≥150s — a harder ceiling on
// stream drain than terminationGracePeriodSeconds — so provider LLM streams
// (up to ~120s) are not truncated on rolling deploys. The caller passes a
// parent ctx; this method applies cfg.ShutdownTimeout on top of it.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.ShutdownHTTP(ctx)
	s.Close()
	return err
}

// ShutdownHTTP quiesces the request-serving surface: it drains in-flight HTTP
// (including SSE) requests, then releases bifrost's worker goroutines and
// pooled resources. It deliberately leaves the NATS client OPEN.
//
// That split is load-bearing for billing. Streams settle DURING this call, and
// a stream settling here may have just recovered a provider usage trailer that
// still has to be billed — which needs a live NATS client. Closing NATS as part
// of one monolithic Shutdown forced the caller to choose between "bill after
// handlers quiesce" (increments hit a closed connection and divert to the
// outage-delta path) and "bill before" (billingClosing set while handlers are
// live, so recovered spend is refused). Both were shipped and both lost money;
// splitting the lifecycle is what removes the choice.
func (s *Server) ShutdownHTTP(ctx context.Context) error {
	shutCtx, cancel := context.WithTimeout(ctx, s.cfg.ShutdownTimeout)
	defer cancel()

	s.logger.Info("gateway shutting down", "grace", s.cfg.ShutdownTimeout.String())
	err := s.http.Shutdown(shutCtx)
	// Release bifrost worker goroutines and pooled resources after the HTTP
	// server has stopped accepting and drained in-flight requests.
	s.core.Shutdown()
	return err
}

// Close releases the NATS client. It MUST run after every billing increment has
// been issued — i.e. after the handler's billing drain and the governance
// store's persist drain — so no increment lands on a closed connection.
// Idempotent enough for the shutdown path (Close is only called once).
func (s *Server) Close() {
	s.natsMu.Lock()
	defer s.natsMu.Unlock()
	// closed stops a background re-dial from opening a connection after this
	// point: nothing would ever close it.
	s.closed = true
	if s.nats != nil {
		s.nats.Close()
	}
}

// natsClientConfig is the one place the gateway's NATS settings become the
// client's: the startup dial and the lazy re-dial must not disagree.
func natsClientConfig(cfg config.Config) natsinfra.Config {
	return natsinfra.Config{
		URL:                cfg.NATSURL,
		Name:               cfg.ServiceName,
		CBFailureThreshold: cfg.CBFailureThreshold,
		CBOpenDuration:     cfg.CBOpenDuration,
		TLSCAFile:          cfg.NATSTLSCAFile,
		TLSCertFile:        cfg.NATSTLSCertFile,
		TLSKeyFile:         cfg.NATSTLSKeyFile,
	}
}

// natsAuthMode is "mtls" when the gateway presents a NATS client certificate
// and "none" otherwise.
func natsAuthMode(cfg config.Config) string {
	if cfg.NATSTLSCertFile != "" {
		return "mtls"
	}
	return "none"
}

// logNATSPosture states the connection's security posture once per dial. The
// plaintext, identity-less posture is compose's; in a cluster the NATS server
// refuses it, so seeing it logged at WARN anywhere else is the signal (#1076).
func logNATSPosture(logger *slog.Logger, msg string, cfg config.Config) {
	mode := natsAuthMode(cfg)
	attrs := []any{"url", redactURL(cfg.NATSURL), "nats_auth", mode, "tls", mode == "mtls"}
	if mode == "none" {
		logger.Warn(msg+" without TLS or a client identity (compose posture only)", attrs...)
		return
	}
	logger.Info(msg, attrs...)
}
