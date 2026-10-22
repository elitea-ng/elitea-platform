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
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/artifactgrpc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/internaltoolgrpc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/modelgrpc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
)

// The platform listener (#1359 D1) is elitea-main's internal gRPC port
// (ELITEA_RUNTIME_PLATFORM_ADDRESS, :9446): the port the worker, the
// DeepWiki/Inventory engines and the sub-application hosts call. It serves
// gRPC only — TLS 1.3, ALPN h2, a runtime-CA client certificate required at
// the handshake — and every call passes grpcauth's per-service allowlist
// (runtimegrpc.PlatformServer). It serves no HTTP: health and metrics stay on
// their own plain-HTTP routes.
//
// Its services, each running the public handler's own route in process so
// that authentication and authorization are one implementation:
//
//   - elitea.llm.v1.ModelService (#1359 U2): the worker's and the engines'
//     model calls through the /llm pipeline (internal/transport/modelgrpc);
//   - elitea.runtime.v1.ArtifactObjectService (#1359 U5): the worker's Office
//     host and indexing source, the engines' artifact-folder ingest and the
//     sub-application hosts' uploads through /api/v2/artifacts
//     (internal/transport/artifactgrpc);
//   - elitea.runtime.v1.InternalToolService (#1359 U5): the worker's internal
//     MCP builders through /app/{project}/mcp and the Inventory engine's
//     source-tool calls through the toolkit test_tool route
//     (internal/transport/internaltoolgrpc).
//
// and, behind ELITEA_VECTOR_INTROSPECTION_CLIENTS, elitea-vector's
// elitea.vector.v1.TokenIntrospectionService (ADR-0031) — not a public route
// but the same server the runtime control listener serves, or, with the
// runtime off, one that verifies provider callback tokens only
// (runtimecomposition.NewPlatformVectorIntrospection).

// ModelServiceClientsEnv lists the workload identities admitted to
// elitea.llm.v1.ModelService: the agent worker's and the sub-application
// callback identity the DeepWiki/Inventory engines present, as "dns:<name>"
// or spiffe:// URIs, comma-separated. Required when the platform listener is
// open: a model plane nobody may call is a configuration error.
const ModelServiceClientsEnv = "ELITEA_MODEL_SERVICE_CLIENTS"

// ArtifactServiceClientsEnv lists the identities admitted to
// elitea.runtime.v1.ArtifactObjectService: the worker's and the
// sub-application callback identity (the DeepWiki/Inventory hosts and
// engines). Empty serves no artifact service: every artifact call is refused.
const ArtifactServiceClientsEnv = "ELITEA_ARTIFACT_SERVICE_CLIENTS"

// InternalToolServiceClientsEnv lists the identities admitted to
// elitea.runtime.v1.InternalToolService: the worker's, and the
// sub-application callback identity when the Inventory engine makes source
// calls. Empty serves no internal-tool service.
const InternalToolServiceClientsEnv = "ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS"

// platformClients is the raw allowlist of each platform service.
type platformClients struct {
	Model        string
	Artifact     string
	InternalTool string
}

// platformClientsFromEnv reads the three allowlists.
func platformClientsFromEnv(getenv func(string) string) platformClients {
	return platformClients{
		Model:        getenv(ModelServiceClientsEnv),
		Artifact:     getenv(ArtifactServiceClientsEnv),
		InternalTool: getenv(InternalToolServiceClientsEnv),
	}
}

// platformServices are the gRPC services the platform listener serves beside
// grpc.health.v1. Each names its fully qualified service, the workload
// identities allowed to call it, and how it registers. publicHandler is the
// public HTTP handler whose routes the services run. ModelService is
// required; the artifact and internal-tool services register only when their
// list names someone (an entry that is not an identity is refused either way).
func platformServices(publicHandler http.Handler, clients platformClients, logger *slog.Logger) ([]runtimegrpc.PlatformService, error) {
	models, err := grpcauth.NewAllowlist(strings.Split(clients.Model, ","))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ModelServiceClientsEnv, err)
	}
	modelServer, err := modelgrpc.NewServer(publicHandler, logger)
	if err != nil {
		return nil, err
	}
	services := []runtimegrpc.PlatformService{modelServer.PlatformService(models)}

	artifacts, ok, err := optionalAllowlist(ArtifactServiceClientsEnv, clients.Artifact)
	if err != nil {
		return nil, err
	}
	if ok {
		server, err := artifactgrpc.NewServer(publicHandler, logger)
		if err != nil {
			return nil, err
		}
		services = append(services, server.PlatformService(artifacts))
	}
	tools, ok, err := optionalAllowlist(InternalToolServiceClientsEnv, clients.InternalTool)
	if err != nil {
		return nil, err
	}
	if ok {
		server, err := internaltoolgrpc.NewServer(publicHandler, logger)
		if err != nil {
			return nil, err
		}
		services = append(services, server.PlatformService(tools))
	}
	return services, nil
}

// withVectorIntrospection appends elitea-vector's token introspection to the
// platform services when ELITEA_VECTOR_INTROSPECTION_CLIENTS names someone;
// otherwise services are returned unchanged.
func withVectorIntrospection(
	services []runtimegrpc.PlatformService,
	config runtimecomposition.Config,
	runtime *runtimecomposition.Runtime,
	dependencies runtimecomposition.PlatformVectorIntrospection,
) ([]runtimegrpc.PlatformService, error) {
	service, ok, err := runtimecomposition.NewPlatformVectorIntrospection(config, runtime, dependencies)
	if err != nil {
		return nil, fmt.Errorf("compose vector token introspection: %w", err)
	}
	if ok {
		services = append(services, service)
	}
	return services, nil
}

// optionalAllowlist parses a list that may be empty: (_, false, nil) when it
// names nobody.
func optionalAllowlist(env, raw string) (grpcauth.Allowlist, bool, error) {
	allowlist, err := grpcauth.NewAllowlist(strings.Split(raw, ","))
	if errors.Is(err, grpcauth.ErrNoClients) {
		return grpcauth.Allowlist{}, false, nil
	}
	if err != nil {
		return grpcauth.Allowlist{}, false, fmt.Errorf("%s: %w", env, err)
	}
	return allowlist, true, nil
}

// newPlatformServer builds the platform listener, or returns nil when the
// configuration names no address. The runtime plane is not required: every
// service runs the public handler's routes in process.
func newPlatformServer(config runtimecomposition.Config, services func() ([]runtimegrpc.PlatformService, error)) (*runtimegrpc.PlatformServer, error) {
	if config.PlatformAddress == "" {
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
