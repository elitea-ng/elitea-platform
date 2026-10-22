// elitea-subapp-host serves the provider SPI for one sub-application.
//
//	ELITEA_SUBAPP=deepwiki|echo|inventory  which application (default: deepwiki)
//	ELITEA_<APP>_RUNNER=unavailable|echo|…  the application's runner; fixture
//	                                and native are DeepWiki's own (legacy is
//	                                retired and refused at start); Inventory's
//	                                engine runner is sidecar (legacy = alias)
//	ELITEA_<APP>_ENGINE_SOCKET      the engine sidecar's Unix socket (native, sidecar)
//	ELITEA_<APP>_DATABASE_URL       the durable invocation store (else in memory)
//	ELITEA_<APP>_TLS_CERTFILE, _TLS_KEYFILE, _TLS_CA_FILE
//	                                the listener's mutual TLS (required)
//	ELITEA_<APP>_PLATFORM_CLIENTS   certificate identities allowed to call the
//	                                listener (required; elitea-main)
//	ELITEA_<APP>_PLATFORM_GRPC_ADDR where it listens (default :9443)
//	ELITEA_<APP>_ADMIN_ADDR         the admin port: /healthz and /readyz,
//	                                plain HTTP (default :9090)
//	ELITEA_<APP>_NATS_URL, _NATS_TLS_{CA,CERT,KEY}_FILE
//	                                the cancel fan-out across replicas
//	ELITEA_<APP>_*                  the host settings under the app's prefix
//
// One binary, one application per process; the prefix keeps each
// application's settings in its own namespace, exactly as the Python shell
// did for DeepWiki.
//
// ONE LISTENER, gRPC OVER MUTUAL TLS (#1359). The provider service (the SPI:
// descriptor, health, slots, invoke, poll, cancel) and the platform
// operations are served on it together, behind a TLS configuration that
// requires and verifies a client certificate at the handshake and an
// allowlist of the certificate identities that may call. The SPI's HTTP
// listener is gone: internal service-to-service traffic is gRPC over mTLS.
//
// The admin port is the one exception, and it serves nothing but health:
// /healthz and /readyz in plain HTTP, for the kubelet and the container
// probe, which present no client certificate. /readyz carries the readiness
// checks — a replica that lost its invocation-store owner claim answers 503
// and leaves the Service.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/cancelbus"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
	"google.golang.org/grpc"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		// The container probe. The image is distroless — no shell, no curl —
		// so the binary asks its own admin port's /readyz.
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
	// The listener's configuration is checked before anything is started:
	// a host that cannot serve must not reconcile rows or dial NATS first.
	tlsConfig, err := listenerTLS(settings)
	if err != nil {
		return err
	}
	if len(settings.PlatformClients) == 0 {
		return fmt.Errorf("%w: %sPLATFORM_CLIENTS must name the certificate identities allowed to call this host (elitea-main)", spi.ErrConfig, settings.Prefix)
	}
	var options []spi.Option
	if settings.DatabaseURL != "" {
		// The durable store (ADR-0023 H2b): the rows the engine's migrations
		// own, shared by every replica. Orphans of a process that is gone are
		// reconciled before the first call, so no poll answers InProgress for
		// work nobody is doing. A database that is configured and unreachable
		// is a boot failure, not a host that silently runs in memory.
		store, err := spi.NewPostgresStore(ctx, settings.DatabaseURL, "", logger, spi.WithMaxConns(settings.DatabaseMaxConns))
		if err != nil {
			return err
		}
		defer store.Close()
		if reconciled, err := store.Reconcile(ctx); err != nil {
			return fmt.Errorf("reconcile the invocation store: %w", err)
		} else if reconciled > 0 {
			logger.Warn("invocations orphaned by a process that is gone were terminated", "count", reconciled)
		}
		// A lost owner claim refuses new invocations (spi.ErrClaimLost);
		// readiness says so — gRPC Health and the admin port's /readyz, which
		// the kubelet reads — so the replica leaves the Service.
		options = append(options, spi.WithStore(store), spi.WithReadiness(func(context.Context) map[string]bool {
			return map[string]bool{"owner_claim": store.ClaimHeld()}
		}))
	}
	bus, closeBus, err := composeCancelBus(settings, os.LookupEnv, logger)
	if err != nil {
		return err
	}
	defer closeBus()
	if bus != nil {
		options = append(options, spi.WithCancelBus(bus))
	}
	server, err := spi.NewServer(settings, app, logger, options...)
	if err != nil {
		return err
	}
	if err := server.Start(ctx); err != nil {
		return err
	}
	defer server.Stop()

	grpcServer, listener, err := listen(server, settings, tlsConfig, logger)
	if err != nil {
		return err
	}
	adminListener, err := net.Listen("tcp", settings.AdminAddr)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("listen for the admin port on %s: %w", settings.AdminAddr, err)
	}
	admin := &http.Server{
		Handler:           spi.AdminHandler(server),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	errs := make(chan error, 2)
	go func() {
		if err := admin.Serve(adminListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("the admin port: %w", err)
		}
	}()
	go func() {
		logger.Info("elitea-subapp-host listening",
			"app", app.Name, "runner", app.Runner.Name(), "addr", listener.Addr().String(), "admin_addr", adminListener.Addr().String(),
			"clients", len(settings.PlatformClients), "identity_verified", settings.IdentitySecret != "",
			"cancel_fanout", bus != nil)
		errs <- grpcServer.Serve(listener)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		stopGRPC(grpcServer, shutdownCtx)
		// No new invocation arrives now. The ones running get the drain
		// before server.Stop (deferred) cancels what is left; a poll or a
		// cancel for them reaches a sibling, which reads the shared store
		// and fans the cancel back here. The admin port stays up, so the
		// in-flight gauge is scraped while it falls.
		drainInvocations(server.Manager(), time.Duration(settings.DrainTimeoutSeconds)*time.Second, logger)
		adminCtx, adminCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer adminCancel()
		_ = admin.Shutdown(adminCtx)
		return nil
	case err := <-errs:
		_ = admin.Close()
		if errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return err
	}
}

// drainInvocations waits up to timeout for manager's in-flight invocations
// to end. Zero waits for nothing.
func drainInvocations(manager *spi.Manager, timeout time.Duration, logger *slog.Logger) {
	running := manager.InFlight()
	if running == 0 || timeout <= 0 {
		return
	}
	logger.Info("draining in-flight invocations", "count", running, "timeout", timeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := manager.WaitIdle(ctx); err != nil {
		logger.Warn("the drain ended with invocations still running; they are cancelled", "count", manager.InFlight())
	}
}

// listen builds the host's gRPC listener (spi.NewGRPCServer) and opens its
// port.
func listen(server *spi.Server, settings spi.Settings, tlsConfig *tls.Config, logger *slog.Logger) (*grpc.Server, net.Listener, error) {
	grpcServer, err := spi.NewGRPCServer(server, settings.PlatformClients, tlsConfig, logger)
	if err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("tcp", settings.PlatformGRPCAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen for gRPC on %s: %w", settings.PlatformGRPCAddr, err)
	}
	return grpcServer, listener, nil
}

// composeCancelBus connects the cancel fan-out when <PREFIX>NATS_URL is set.
// Without it cancels stay on the replica they land on, which is correct for
// one replica and is logged, because with more a cancel can miss its run.
func composeCancelBus(settings spi.Settings, lookup spi.Lookup, logger *slog.Logger) (spi.CancelBus, func(), error) {
	if settings.NATSURL == "" {
		logger.Warn("the cancel fan-out is off (no NATS_URL): a cancel reaches only the replica it lands on; set it when running more than one replica",
			"set", settings.Prefix+"NATS_URL")
		return nil, func() {}, nil
	}
	app := appKey(settings.Prefix)
	env := func(name string) (string, bool) { return lookup(name) }
	var material natsconn.Material
	var err error
	// One literal prefix per application that has a NATS identity, so the
	// chart's render test (deploy/helm/tests/render-nats-security.sh) can read
	// which <PREFIX>_NATS_TLS_* names the code builds.
	switch app {
	case "deepwiki":
		material, err = natsconn.FromEnv(deepwikiNATSPrefix, env)
	case "inventory":
		material, err = natsconn.FromEnv(inventoryNATSPrefix, env)
	default:
		err = fmt.Errorf("%s has no NATS identity; unset %sNATS_URL", app, settings.Prefix)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", spi.ErrConfig, err)
	}
	conn, err := cancelbus.Connect(cancelbus.Config{URL: settings.NATSURL, Material: material, App: app}, logger)
	if err != nil {
		return nil, nil, err
	}
	bus, err := cancelbus.New(conn, app, logger)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return bus, func() { _ = conn.Drain() }, nil
}

// The NATS environment prefixes of the two applications with a NATS
// identity (natsconn.SubappIdentity).
const (
	deepwikiNATSPrefix  = "ELITEA_DEEPWIKI"
	inventoryNATSPrefix = "ELITEA_INVENTORY"
)

// appKey is the application's subject token, from its settings prefix:
// ELITEA_DEEPWIKI_ → deepwiki.
func appKey(prefix string) string {
	return strings.ToLower(strings.Trim(strings.TrimPrefix(prefix, "ELITEA_"), "_"))
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

// healthcheck asks the admin port's /readyz on loopback: healthy when the
// host serves and every readiness check passes (an owner claim held).
func healthcheck(lookup spi.Lookup) error {
	_, settings, err := compose(lookup)
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(settings.AdminAddr)
	if err != nil {
		return fmt.Errorf("admin address %q: %w", settings.AdminAddr, err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/readyz")
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz answered %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
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

// listenerTLS builds the listener's TLS: the server certificate and, from
// the client CA, RequireAndVerifyClientCert — the mutual-TLS terminus. All
// three files are required: the listener serves nothing in the clear.
func listenerTLS(settings spi.Settings) (*tls.Config, error) {
	if !settings.TerminatesMTLS() {
		return nil, fmt.Errorf("%w: %sTLS_CERTFILE, %sTLS_KEYFILE and %sTLS_CA_FILE are required: the host serves gRPC over mutual TLS only",
			spi.ErrConfig, settings.Prefix, settings.Prefix, settings.Prefix)
	}
	certificate, err := tls.LoadX509KeyPair(settings.TLSCertFile, settings.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load the server certificate: %w", err)
	}
	pem, err := os.ReadFile(settings.TLSCAFile)
	if err != nil {
		return nil, fmt.Errorf("read the client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%w: %s holds no certificate", spi.ErrConfig, settings.TLSCAFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
