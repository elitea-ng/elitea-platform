// elitea-subapp-host serves the provider SPI for one sub-application.
//
//	ELITEA_SUBAPP=deepwiki|echo|inventory  which application (default: deepwiki)
//	ELITEA_<APP>_RUNNER=unavailable|echo|…  the application's runner; fixture
//	                                and native are DeepWiki's own (legacy is
//	                                retired and refused at start); Inventory's
//	                                engine runner is sidecar (legacy = alias)
//	ELITEA_<APP>_ENGINE_SOCKET      the engine sidecar's Unix socket (native, sidecar)
//	ELITEA_<APP>_DATABASE_URL       the durable invocation store (else in memory)
//	ELITEA_<APP>_PLATFORM_CLIENTS   certificate identities allowed to call the
//	                                platform gRPC service (empty: service off)
//	ELITEA_<APP>_PLATFORM_GRPC_ADDR where that service listens (default :9443)
//	ELITEA_<APP>_*                  the host settings under the app's prefix
//
// One binary, one application per process; the prefix keeps each
// application's settings in its own namespace, exactly as the Python shell
// did for DeepWiki. Mutual TLS is on whenever a client CA is configured:
// the listener then requires and verifies a client certificate at the
// handshake, and the host trusts its own handshake (ADR-0023 H1 — the
// Python shell looked for an ASGI extension uvicorn never populated, and
// refused every authenticated hop until the standalone stack found it).
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
	"google.golang.org/grpc"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		// The container probe. The image is distroless — no shell, no curl —
		// and the listener requires a client certificate at the handshake, so
		// an HTTP probe from inside the container would need a certificate
		// it does not have. A TCP connect is what the Python shell's probe
		// did, and it is what "the listener is up" means.
		if err := healthcheck(os.LookupEnv); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(logger); err != nil {
		logger.Error("elitea-subapp-host stopped with an error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	app, settings, err := compose(os.LookupEnv)
	if err != nil {
		return err
	}
	var options []spi.Option
	if settings.DatabaseURL != "" {
		// The durable store (ADR-0023 H2b): the rows the Python migrations
		// own. Orphans of a previous process are reconciled before the first
		// request, so no poll answers InProgress for work nobody is doing. A
		// database that is configured and unreachable is a boot failure, not
		// a host that silently runs in memory.
		store, err := spi.NewPostgresStore(ctx, settings.DatabaseURL, "", logger)
		if err != nil {
			return err
		}
		defer store.Close()
		if reconciled, err := store.Reconcile(ctx); err != nil {
			return fmt.Errorf("reconcile the invocation store: %w", err)
		} else if reconciled > 0 {
			logger.Warn("invocations orphaned by a previous process were terminated", "count", reconciled)
		}
		options = append(options, spi.WithStore(store))
	}
	server, err := spi.NewServer(settings, app, logger, options...)
	if err != nil {
		return err
	}
	server.Start(ctx)
	defer server.Stop()

	httpServer := &http.Server{
		Addr:              settings.ListenAddr,
		Handler:           server,
		ReadHeaderTimeout: 10 * time.Second,
		// net/http reports every aborted handshake at error level, and the
		// container probe aborts one every few seconds by design (see
		// healthcheck). Those lines are dropped; everything else the server
		// reports still reaches the log.
		ErrorLog: log.New(probeNoiseFilter{logger: logger}, "", 0),
		// No WriteTimeout: a poll is short, but an invoke may hold a long
		// body, and a deadline here would truncate it with nothing logged.
	}
	if settings.TLSCertFile != "" {
		tlsConfig, err := listenerTLS(settings)
		if err != nil {
			return err
		}
		httpServer.TLSConfig = tlsConfig
	}
	errs := make(chan error, 2)
	platform, err := startPlatformGRPC(server, settings, httpServer.TLSConfig, logger, errs)
	if err != nil {
		return err
	}
	go func() {
		logger.Info("elitea-subapp-host listening",
			"app", app.Name, "runner", app.Runner.Name(), "addr", settings.ListenAddr,
			"tls", settings.TLSCertFile != "", "mtls", settings.MTLSRequired(), "identity_verified", settings.IdentitySecret != "")
		if httpServer.TLSConfig != nil {
			errs <- httpServer.ListenAndServeTLS("", "")
		} else {
			errs <- httpServer.ListenAndServe()
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if platform != nil {
			stopGRPC(platform, shutdownCtx)
		}
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errs:
		if platform != nil {
			platform.Stop()
		}
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// startPlatformGRPC serves the platform operations (spi/platform.go) on a
// listener of its own, behind the SPI listener's TLS configuration, when the
// runner has any AND <PREFIX>PLATFORM_CLIENTS names who may call them. With
// no clients it does nothing: the service is off, no port is opened.
//
// Clients configured without mutual TLS is a boot failure, not a service that
// silently refuses everyone or, worse, accepts anyone.
func startPlatformGRPC(server *spi.Server, settings spi.Settings, tlsConfig *tls.Config, logger *slog.Logger, errs chan<- error) (*grpc.Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	addr := settings.PlatformGRPCAddr
	if len(settings.PlatformClients) == 0 {
		if addr != "" {
			logger.Warn("the platform gRPC service is off: no platform clients are allowed (set PLATFORM_CLIENTS)", "addr", addr)
		}
		return nil, nil
	}
	ops, ok := server.PlatformOps()
	if !ok {
		logger.Warn("platform clients are configured but this application has no platform operations; the service is off")
		return nil, nil
	}
	if addr == "" {
		addr = spi.DefaultPlatformGRPCAddr
	}
	platform, err := spi.NewPlatformGRPCServer(ops, settings.PlatformClients, tlsConfig, logger)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen for the platform gRPC service on %s: %w", addr, err)
	}
	go func() {
		logger.Info("platform gRPC service listening", "addr", listener.Addr().String(), "clients", len(settings.PlatformClients))
		errs <- platform.Serve(listener)
	}()
	return platform, nil
}

// stopGRPC drains a gRPC server, then stops it at the deadline.
func stopGRPC(server *grpc.Server, ctx context.Context) {
	done := make(chan struct{})
	go func() { server.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		server.Stop()
	}
}

// probeNoiseFilter forwards net/http's server errors to the structured log,
// minus the handshake abort the TCP probe causes.
type probeNoiseFilter struct{ logger *slog.Logger }

func (f probeNoiseFilter) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	if !isProbeNoise(line) {
		f.logger.Warn("http server", "message", line)
	}
	return len(p), nil
}

// isProbeNoise is the exact shape of a connection opened and closed without
// a handshake: a TLS handshake error from loopback ending in EOF.
func isProbeNoise(line string) bool {
	return strings.HasPrefix(line, "http: TLS handshake error from 127.0.0.1:") && strings.HasSuffix(line, ": EOF")
}

// healthcheck dials the configured listen port on loopback.
func healthcheck(lookup spi.Lookup) error {
	_, settings, err := compose(lookup)
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(settings.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen address %q: %w", settings.ListenAddr, err)
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 2*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

// compose picks the application and its runner from the environment. Both
// come out of the registry (internal/apps): the application by its
// ELITEA_SUBAPP key, the runner by name from the set that application
// serves. Nothing here knows which applications exist.
func compose(lookup spi.Lookup) (spi.App, spi.Settings, error) {
	key, _ := lookup("ELITEA_SUBAPP")
	entry, err := apps.Lookup(key)
	if err != nil {
		return spi.App{}, spi.Settings{}, err
	}
	settings, err := spi.SettingsFromEnv(entry.EnvPrefix, lookup)
	if err != nil {
		return spi.App{}, spi.Settings{}, err
	}
	runnerName, _ := lookup(entry.EnvPrefix + "RUNNER")
	runnerName = strings.TrimSpace(runnerName)
	if runnerName == "" {
		runnerName = "unavailable"
	}
	stepRaw, _ := lookup(entry.EnvPrefix + "FIXTURE_STEP_SECONDS")
	step := time.Second
	if stepRaw != "" {
		parsed, err := time.ParseDuration(strings.TrimSpace(stepRaw) + "s")
		if err != nil || parsed < 0 {
			return spi.App{}, spi.Settings{}, fmt.Errorf("%w: %sFIXTURE_STEP_SECONDS must be a number of seconds, got %q", spi.ErrConfig, entry.EnvPrefix, stepRaw)
		}
		step = parsed
	}
	runner, err := entry.Runner(runnerName, settings, step)
	if err != nil {
		return spi.App{}, spi.Settings{}, err
	}
	return entry.Compose(runner), settings, nil
}

// listenerTLS builds the listener's TLS: the server certificate, and — with
// a client CA — RequireAndVerifyClientCert, the mutual-TLS terminus.
func listenerTLS(settings spi.Settings) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(settings.TLSCertFile, settings.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load the server certificate: %w", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	if settings.TLSCAFile != "" {
		pem, err := os.ReadFile(settings.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read the client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%w: %s holds no certificate", spi.ErrConfig, settings.TLSCAFile)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}
