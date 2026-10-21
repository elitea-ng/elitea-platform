package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"google.golang.org/grpc"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/modelgrpc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
)

// The platform listener (#1359 D1) is elitea-main's internal gRPC port
// (ELITEA_RUNTIME_PLATFORM_ADDRESS, :9446): the port the worker, the
// DeepWiki/Inventory engines and the sub-application host call. It serves
// gRPC only — TLS 1.3, ALPN h2, a runtime-CA client certificate required at
// the handshake — and every call passes grpcauth's per-service allowlist
// (runtimegrpc.PlatformServer).
//
// It serves no HTTP. The worker's and the engines' model calls are
// elitea.llm.v1.ModelService (#1359 U2), which runs each call through the
// public handler's /llm pipeline in process (internal/transport/modelgrpc).
// Until the artifact and internal-MCP units land (#1359 U3/U5:
// ArtifactObjectService, InternalToolService), those worker calls still fail
// here.

// ModelServiceClientsEnv lists the workload identities admitted to
// elitea.llm.v1.ModelService: the agent worker's and the sub-application
// callback identity the DeepWiki/Inventory engines present, as "dns:<name>"
// or spiffe:// URIs, comma-separated. Required when the platform listener is
// open: a model plane nobody may call is a configuration error.
const ModelServiceClientsEnv = "ELITEA_MODEL_SERVICE_CLIENTS"

// platformServices are the gRPC services the platform listener serves beside
// grpc.health.v1. Each names its fully qualified service, the workload
// identities allowed to call it, and how it registers. publicHandler is the
// public HTTP handler, whose /llm pipeline ModelService runs; modelClients is
// the raw ELITEA_MODEL_SERVICE_CLIENTS value.
//
// TODO(#1359 U3/U5): ArtifactObjectService and InternalToolService register
// here.
func platformServices(publicHandler http.Handler, modelClients string, logger *slog.Logger) ([]runtimegrpc.PlatformService, error) {
	clients, err := grpcauth.NewAllowlist(strings.Split(modelClients, ","))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ModelServiceClientsEnv, err)
	}
	models, err := modelgrpc.NewServer(publicHandler, logger)
	if err != nil {
		return nil, err
	}
	return []runtimegrpc.PlatformService{models.PlatformService(clients)}, nil
}

// newPlatformServer builds the platform listener, or returns nil when the
// configuration opens none (the runtime is off, or no address is named).
func newPlatformServer(config runtimecomposition.Config, services func() ([]runtimegrpc.PlatformService, error)) (*runtimegrpc.PlatformServer, error) {
	if !config.Enabled || config.PlatformAddress == "" {
		return nil, nil
	}
	platform, err := services()
	if err != nil {
		return nil, err
	}
	tlsConfig, err := runtimegrpc.LoadPlatformServerTLSConfig(config.PlatformTLS)
	if err != nil {
		return nil, err
	}
	return runtimegrpc.NewPlatformServer(runtimegrpc.PlatformServerConfig{
		Address: config.PlatformAddress,
		TLS:     tlsConfig,
		Policy:  runtimegrpc.DefaultPlatformGRPCPolicy(),
	}, platform...)
}

// platformLifecycle is the part of *runtimegrpc.PlatformServer the
// application lifecycle drives.
type platformLifecycle interface {
	ListenAndServe() error
	Shutdown(context.Context) error
}

// applicationServers runs the public server and, when present, the platform
// listener as ONE lifecycle component: serveApplication drains them together
// within one deadline, and either one failing stops both.
type applicationServers struct {
	public   *http.Server
	platform platformLifecycle
}

func (servers applicationServers) ListenAndServe() error {
	if servers.platform == nil {
		return servers.public.ListenAndServe()
	}
	results := make(chan error, 2)
	go func() { results <- servers.public.ListenAndServe() }()
	go func() { results <- platformServeResult(servers.platform.ListenAndServe()) }()
	// The first to stop decides; serveApplication then shuts both down. The
	// second result lands in the buffered channel and is dropped.
	return <-results
}

// platformServeResult speaks the gRPC server's "stopped" in the public
// server's terms. grpc.Server.Serve returns nil (or ErrServerStopped) after
// a Stop: that is the drain, not a failure, and serveApplication recognises
// a drain by http.ErrServerClosed.
func platformServeResult(err error) error {
	if err == nil || errors.Is(err, grpc.ErrServerStopped) {
		return http.ErrServerClosed
	}
	return err
}

func (servers applicationServers) Shutdown(ctx context.Context) error {
	if servers.platform == nil {
		return servers.public.Shutdown(ctx)
	}
	errs := make(chan error, 2)
	go func() { errs <- servers.public.Shutdown(ctx) }()
	go func() { errs <- servers.platform.Shutdown(ctx) }()
	return errors.Join(<-errs, <-errs)
}
