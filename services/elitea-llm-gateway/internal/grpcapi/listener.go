package grpcapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	gatewayv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/gateway/v1"
)

const (
	// maxRecvMessage admits a JSON request body up to the handler's own
	// 32 MiB cap (llmproxy.maxRequestBody) plus its head. A larger body is
	// cut by elitea-main at the cap plus one byte, so the handler answers
	// the same 400 it answered over HTTP.
	maxRecvMessage = 40 << 20
	// maxSendMessage admits a unary response up to maxUnaryResponse plus
	// its head.
	maxSendMessage = maxUnaryResponse + 1<<20

	// maxConnectionAge makes elitea-main reconnect, and so re-resolve the
	// headless Service, at this interval: a replica that scaled out then
	// receives a share of the calls. A call in flight is not cut: the
	// grace is unbounded, so a long stream or realtime session finishes on
	// its connection.
	maxConnectionAge = 5 * time.Minute

	// keepaliveMinTime is the shortest client ping spacing tolerated.
	keepaliveMinTime = 10 * time.Second
)

// TLSFiles names the gateway's mTLS material: the server certificate and key,
// and the CA bundle that client certificates must chain to.
type TLSFiles struct {
	CertFile string
	KeyFile  string
	CAFile   string
}

// ServerTLS is the listener's TLS profile: TLS 1.3, ALPN h2, a client
// certificate from CAFile required and verified.
func ServerTLS(files TLSFiles) (*tls.Config, error) {
	if files.CertFile == "" || files.KeyFile == "" || files.CAFile == "" {
		return nil, errors.New("grpcapi: the gRPC listener needs GATEWAY_TLS_CERT_FILE, GATEWAY_TLS_KEY_FILE and GATEWAY_TLS_CA_FILE; it never serves without mutual TLS")
	}
	cert, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("grpcapi: load the server certificate: %w", err)
	}
	caPEM, err := os.ReadFile(files.CAFile)
	if err != nil {
		return nil, fmt.Errorf("grpcapi: read the CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("grpcapi: no certificates in the CA bundle %q", files.CAFile)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		NextProtos:   []string{"h2"},
	}, nil
}

// Server is the gRPC listener of GatewayService.
type Server struct {
	service *Service
	grpc    *grpc.Server
	addr    string

	stopOnce sync.Once
}

// NewServer builds the listener. tlsConfig must require client certificates
// (ServerTLS); allow names the admitted callers.
func NewServer(addr string, tlsConfig *tls.Config, allow Allowlist, service *Service) (*Server, error) {
	if addr == "" {
		return nil, errors.New("grpcapi: the listen address is required")
	}
	if tlsConfig == nil || tlsConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		return nil, errors.New("grpcapi: the listener requires mutual TLS")
	}
	if len(allow.identities) == 0 {
		return nil, errors.New("grpcapi: the client allowlist is empty")
	}
	if service == nil {
		return nil, errors.New("grpcapi: the service is required")
	}
	server := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		grpc.ChainUnaryInterceptor(allow.UnaryInterceptor()),
		grpc.ChainStreamInterceptor(allow.StreamInterceptor()),
		grpc.MaxRecvMsgSize(maxRecvMessage),
		grpc.MaxSendMsgSize(maxSendMessage),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             keepaliveMinTime,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge: maxConnectionAge,
			Time:             30 * time.Second,
			Timeout:          10 * time.Second,
		}),
	)
	gatewayv1.RegisterGatewayServiceServer(server, service)
	return &Server{service: service, grpc: server, addr: addr}, nil
}

// Serve accepts calls on lis until Stop. It returns nil after Stop.
func (s *Server) Serve(lis net.Listener) error {
	if err := s.grpc.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

// ListenAndServe listens on the configured address and serves.
func (s *Server) ListenAndServe() error {
	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("grpcapi: listen on %s: %w", s.addr, err)
	}
	return s.Serve(lis)
}

// Drain is the gRPC half of the shutdown sequence's first phase: no new
// call is accepted, the listener stops, and the calls in flight that are not
// realtime sessions finish, bounded by ctx. Realtime sessions keep running;
// the handler's CloseRealtimeSessions ends them, and Stop releases what is left.
func (s *Server) Drain(ctx context.Context) error {
	// GracefulStop closes the listener and sends GOAWAY at once, then waits
	// for every call, sessions included; it runs in the background and Stop
	// ends that wait.
	go s.grpc.GracefulStop()
	return s.service.Drain(ctx)
}

// Stop ends every call still open.
func (s *Server) Stop() {
	s.stopOnce.Do(s.grpc.Stop)
}
