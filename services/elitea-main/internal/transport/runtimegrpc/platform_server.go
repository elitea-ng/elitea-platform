package runtimegrpc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/grpcauth"
)

// PlatformMaxMessageBytes caps one gRPC message the platform listener
// receives, and every answer but a unary model answer. Bulk uploads
// (artifact objects, image edit bodies) travel as chunked streams, so no
// single request message needs more.
const PlatformMaxMessageBytes = 16 << 20

// PlatformMaxSendMessageBytes caps one gRPC message the platform listener
// sends. A unary ModelService answer (ImageGenerate, ImageEdit, Embeddings)
// is the gateway's whole answer in one message: up to the gateway's 64 MiB
// unary cap plus its head, as the gateway listener sends it
// (elitea-llm-gateway internal/grpcapi, maxSendMessage). A b64 image answer
// passes 16 MiB easily. Clients of ModelService read up to this.
const PlatformMaxSendMessageBytes = 65 << 20

// DefaultPlatformGRPCPolicy is the platform listener's connection policy:
// the private listeners' keepalive rhythm, with a long connection-age grace
// because model completions stream on this port for minutes.
func DefaultPlatformGRPCPolicy() GRPCServerPolicy {
	return GRPCServerPolicy{
		MaxConcurrentStreams:  maxPrivateConcurrentStreams,
		MaxConnections:        256,
		KeepaliveTime:         time.Minute,
		KeepaliveTimeout:      10 * time.Second,
		MaxConnectionIdle:     5 * time.Minute,
		MaxConnectionAge:      30 * time.Minute,
		MaxConnectionAgeGrace: 15 * time.Minute,
	}
}

// PlatformService is one gRPC service served on the platform listener, with
// the workload identities allowed to call it. Name is the fully qualified
// service name, exactly as the generated *_ServiceDesc.ServiceName spells it;
// Register registers the implementation on the server it is handed.
type PlatformService struct {
	Name     string
	Clients  grpcauth.Allowlist
	Register func(grpc.ServiceRegistrar)
}

// PlatformServerConfig configures the platform listener.
type PlatformServerConfig struct {
	Address string
	// TLS must pass validateServerTLSConfig: TLS 1.3, ALPN h2,
	// RequireAndVerifyClientCert against the runtime CA.
	TLS    *tls.Config
	Policy GRPCServerPolicy
}

// PlatformServer is elitea-main's platform gRPC listener (#1359 D1,
// ELITEA_RUNTIME_PLATFORM_ADDRESS, :9446): the internal port the worker, the
// DeepWiki/Inventory engines and the sub-application host call. It serves
// gRPC only.
//
// Every call passes a grpcauth.Policy built from the registered services:
// the client certificate admits the connection at the handshake, and the
// policy admits the caller's workload identity to the one service it calls.
// grpc.health.v1.Health is always registered and open to any verified peer;
// server reflection is never registered.
type PlatformServer struct {
	address      string
	server       *grpc.Server
	health       *health.Server
	maxConns     int
	listen       func(network, address string) (net.Listener, error)
	shutdownOnce sync.Once
}

// NewPlatformServer builds the listener with the given services. Service
// names must be distinct and every service needs a non-empty allowlist.
func NewPlatformServer(config PlatformServerConfig, services ...PlatformService) (*PlatformServer, error) {
	return newPlatformServer(config, net.Listen, services...)
}

func newPlatformServer(config PlatformServerConfig, listen func(string, string) (net.Listener, error), services ...PlatformService) (*PlatformServer, error) {
	if config.Address == "" {
		return nil, errors.New("platform listener address is required")
	}
	if err := validateServerTLSConfig(config.TLS); err != nil {
		return nil, fmt.Errorf("platform listener TLS: %w", err)
	}
	if err := validateGRPCServerPolicy(config.Policy); err != nil {
		return nil, fmt.Errorf("platform listener gRPC policy: %w", err)
	}
	server, healthServer, err := newAuthorizedGRPCServer(config.TLS, config.Policy, services)
	if err != nil {
		return nil, err
	}
	return &PlatformServer{
		address:  config.Address,
		server:   server,
		health:   healthServer,
		maxConns: config.Policy.MaxConnections,
		listen:   listen,
	}, nil
}

// newAuthorizedGRPCServer is the shared construction of elitea-main's
// internal gRPC listeners that admit callers per service (the platform
// listener, :9446, and the content listener, :9445): TLS credentials from
// tlsConfig, PlatformMaxMessageBytes in and PlatformMaxSendMessageBytes out, the policy's
// stream and keepalive limits, grpc.health.v1 open to any verified peer, and
// a grpcauth.Policy built from services that refuses any registered service
// without an allowlist. tlsConfig and policy must already be validated.
func newAuthorizedGRPCServer(tlsConfig *tls.Config, policy GRPCServerPolicy, services []PlatformService) (*grpc.Server, *health.Server, error) {
	healthServer := health.NewServer()
	rules := map[string]grpcauth.Allowlist{
		healthv1.Health_ServiceDesc.ServiceName: grpcauth.AnyVerifiedPeer(),
	}
	for _, service := range services {
		if service.Name == "" || service.Register == nil {
			return nil, nil, errors.New("a gRPC service needs a name and a registration")
		}
		if _, duplicate := rules[service.Name]; duplicate {
			return nil, nil, fmt.Errorf("gRPC service %s is registered twice", service.Name)
		}
		if !service.Clients.Configured() {
			return nil, nil, fmt.Errorf("gRPC service %s: %w", service.Name, grpcauth.ErrNoClients)
		}
		rules[service.Name] = service.Clients
	}
	authorization, err := grpcauth.NewPolicy(rules)
	if err != nil {
		return nil, nil, err
	}
	options := append([]grpc.ServerOption{
		grpc.Creds(credentials.NewTLS(tlsConfig.Clone())),
		grpc.MaxRecvMsgSize(PlatformMaxMessageBytes),
		grpc.MaxSendMsgSize(PlatformMaxSendMessageBytes),
		grpc.MaxConcurrentStreams(policy.MaxConcurrentStreams),
		grpc.KeepaliveEnforcementPolicy(RuntimeKeepaliveEnforcementPolicy()),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     policy.MaxConnectionIdle,
			MaxConnectionAge:      policy.MaxConnectionAge,
			MaxConnectionAgeGrace: policy.MaxConnectionAgeGrace,
			Time:                  policy.KeepaliveTime,
			Timeout:               policy.KeepaliveTimeout,
		}),
	}, authorization.ServerOptions()...)
	server := grpc.NewServer(options...)
	healthv1.RegisterHealthServer(server, healthServer)
	for _, service := range services {
		service.Register(server)
		// A registration that does not register the named service would
		// leave a policy entry for nothing and the real service unreachable
		// (fail closed, but silently). Refuse it at boot.
		if _, ok := server.GetServiceInfo()[service.Name]; !ok {
			return nil, nil, fmt.Errorf("gRPC service %s was not registered by its registration", service.Name)
		}
		healthServer.SetServingStatus(service.Name, healthv1.HealthCheckResponse_SERVING)
	}
	for name := range server.GetServiceInfo() {
		if _, ok := rules[name]; !ok {
			return nil, nil, fmt.Errorf("gRPC service %s was registered without an allowlist", name)
		}
	}
	healthServer.SetServingStatus("", healthv1.HealthCheckResponse_SERVING)
	return server, healthServer, nil
}

// Addr is the configured listen address.
func (s *PlatformServer) Addr() string { return s.address }

// ListenAndServe binds the address and serves until Shutdown. After
// Shutdown it returns grpc.ErrServerStopped or nil.
func (s *PlatformServer) ListenAndServe() error {
	listener, err := s.listen("tcp", s.address)
	if err != nil {
		return fmt.Errorf("listen for the platform listener: %w", err)
	}
	return s.Serve(listener)
}

// Serve serves on an already bound listener (tests bind 127.0.0.1:0).
func (s *PlatformServer) Serve(listener net.Listener) error {
	bounded := newBoundedListener(listener, s.maxConns)
	defer func() { _ = bounded.Close() }()
	return s.server.Serve(bounded)
}

// Shutdown marks every service NOT_SERVING, then drains in-flight calls
// until ctx expires and stops the rest.
func (s *PlatformServer) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("platform listener shutdown context is required")
	}
	s.shutdownOnce.Do(s.health.Shutdown)
	gracefulStop(ctx, s.server)
	return nil
}
