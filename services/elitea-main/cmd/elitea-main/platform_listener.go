package main

import (
	"context"
	"errors"
	"net/http"

	"google.golang.org/grpc"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
)

// The platform listener (#1359 D1) is elitea-main's internal gRPC port
// (ELITEA_RUNTIME_PLATFORM_ADDRESS, :9446): the port the worker, the
// DeepWiki/Inventory engines and the sub-application host call. It serves
// gRPC only — TLS 1.3, ALPN h2, a runtime-CA client certificate required at
// the handshake — and every call passes grpcauth's per-service allowlist
// (runtimegrpc.PlatformServer).
//
// It no longer serves the public HTTP handler. Until the units that move the
// worker's HTTP calls onto gRPC services land (#1359 U2 ModelService, U3
// ArtifactObjectService, U5 InternalToolService), only grpc.health.v1 answers
// here, and those worker calls fail.

// platformServices are the gRPC services the platform listener serves beside
// grpc.health.v1. Each names its fully qualified service, the workload
// identities allowed to call it, and how it registers.
//
// TODO(#1359 U2/U3/U5): ModelService, ArtifactObjectService and
// InternalToolService register here.
func platformServices() []runtimegrpc.PlatformService {
	return nil
}

// newPlatformServer builds the platform listener, or returns nil when the
// configuration opens none (the runtime is off, or no address is named).
func newPlatformServer(config runtimecomposition.Config, services []runtimegrpc.PlatformService) (*runtimegrpc.PlatformServer, error) {
	if !config.Enabled || config.PlatformAddress == "" {
		return nil, nil
	}
	tlsConfig, err := runtimegrpc.LoadPlatformServerTLSConfig(config.PlatformTLS)
	if err != nil {
		return nil, err
	}
	return runtimegrpc.NewPlatformServer(runtimegrpc.PlatformServerConfig{
		Address: config.PlatformAddress,
		TLS:     tlsConfig,
		Policy:  runtimegrpc.DefaultPlatformGRPCPolicy(),
	}, services...)
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
