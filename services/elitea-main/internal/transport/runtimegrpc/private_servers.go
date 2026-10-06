package runtimegrpc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

const (
	maxPrivateConcurrentStreams = 256
	maxPrivateConnections       = 1024
)

type GRPCServerPolicy struct {
	MaxConcurrentStreams  uint32
	MaxConnections        int
	MinClientPingInterval time.Duration
	KeepaliveTime         time.Duration
	KeepaliveTimeout      time.Duration
	MaxConnectionIdle     time.Duration
	MaxConnectionAge      time.Duration
	MaxConnectionAgeGrace time.Duration
}

type PrivateServerConfig struct {
	ControlAddress         string
	OutputAddress          string
	ContentAddress         string
	ControlTLS             *tls.Config
	OutputTLS              *tls.Config
	ContentTLS             *tls.Config
	ControlMaxRequestBytes int
	// Zero keeps compiled snapshot authority disabled at the transport boundary.
	CompiledSnapshotMaxRequestBytes int
	ControlMaxResponseBytes         int
	OutputMaxRequestBytes           int
	OutputMaxResponseBytes          int
	ControlGRPC                     GRPCServerPolicy
	OutputGRPC                      GRPCServerPolicy
	ContentMaxConnections           int
	ContentMaxStreams               int
	ContentReadTimeout              time.Duration
	ContentWriteTimeout             time.Duration
	ContentIdleTimeout              time.Duration
	ContentMaxHeaderBytes           int
	ShutdownTimeout                 time.Duration
}

type PrivateServices struct {
	Control runtimev1.RuntimeControlServiceServer
	Output  runtimev1.ExecutionOutputServiceServer
	Content http.Handler
}

// PrivateServerSet owns the three deliberately separate authenticated data
// paths. It does not expose any of them on the public elitea-main HTTP listener:
// control gRPC, output gRPC and claim-bound HTTPS each have their own mTLS
// listener, codec/body bound and lifecycle.
type PrivateServerSet struct {
	config        PrivateServerConfig
	controlServer *grpc.Server
	outputServer  *grpc.Server
	contentServer *http.Server
	listen        func(network, address string) (net.Listener, error)
	shutdownOnce  sync.Once
	shutdownDone  chan struct{}
	shutdownErr   error
	shuttingDown  atomic.Bool
}

func NewPrivateServerSet(config PrivateServerConfig, services PrivateServices) (*PrivateServerSet, error) {
	return newPrivateServerSet(config, services, net.Listen)
}

func newPrivateServerSet(config PrivateServerConfig, services PrivateServices, listen func(string, string) (net.Listener, error)) (*PrivateServerSet, error) {
	if services.Control == nil || services.Output == nil || services.Content == nil || listen == nil {
		return nil, errors.New("runtime control, output and content services are required")
	}
	if config.ControlAddress == "" || config.OutputAddress == "" || config.ContentAddress == "" || config.ControlAddress == config.OutputAddress || config.ControlAddress == config.ContentAddress || config.OutputAddress == config.ContentAddress {
		return nil, errors.New("three distinct private runtime listener addresses are required")
	}
	if err := validateServerTLSConfig(config.ControlTLS); err != nil {
		return nil, fmt.Errorf("control TLS: %w", err)
	}
	if err := validateServerTLSConfig(config.OutputTLS); err != nil {
		return nil, fmt.Errorf("output TLS: %w", err)
	}
	if err := validateServerTLSConfig(config.ContentTLS); err != nil {
		return nil, fmt.Errorf("content TLS: %w", err)
	}
	if config.ControlMaxRequestBytes <= 0 || config.ControlMaxResponseBytes <= 0 || config.OutputMaxRequestBytes <= 0 || config.OutputMaxResponseBytes <= 0 || config.ContentReadTimeout <= 0 || config.ContentWriteTimeout <= 0 || config.ContentIdleTimeout <= 0 || config.ContentMaxHeaderBytes <= 0 || config.ShutdownTimeout <= 0 {
		return nil, errors.New("runtime listener message and lifecycle limits must be positive")
	}
	if err := validateGRPCServerPolicy(config.ControlGRPC); err != nil {
		return nil, fmt.Errorf("control gRPC policy: %w", err)
	}
	if err := validateGRPCServerPolicy(config.OutputGRPC); err != nil {
		return nil, fmt.Errorf("output gRPC policy: %w", err)
	}
	if config.ContentMaxConnections <= 0 || config.ContentMaxConnections > maxPrivateConnections || config.ContentMaxStreams <= 0 || config.ContentMaxStreams > maxPrivateConcurrentStreams {
		return nil, errors.New("content connection limit is invalid")
	}
	controlCodec, err := NewDirectionalStrictProtoCodec(config.ControlMaxRequestBytes, config.ControlMaxResponseBytes)
	if err != nil {
		return nil, err
	}
	if config.CompiledSnapshotMaxRequestBytes != 0 {
		if err = controlCodec.WithCompiledSnapshotRequestLimit(config.CompiledSnapshotMaxRequestBytes); err != nil {
			return nil, err
		}
	}
	outputCodec, err := NewDirectionalStrictProtoCodec(config.OutputMaxRequestBytes, config.OutputMaxResponseBytes)
	if err != nil {
		return nil, err
	}
	controlOptions := privateGRPCServerOptions(controlCodec, config.ControlTLS, config.ControlGRPC)
	outputOptions := privateGRPCServerOptions(outputCodec, config.OutputTLS, config.OutputGRPC)
	controlServer := grpc.NewServer(controlOptions...)
	outputServer := grpc.NewServer(outputOptions...)
	runtimev1.RegisterRuntimeControlServiceServer(controlServer, services.Control)
	runtimev1.RegisterExecutionOutputServiceServer(outputServer, services.Output)
	contentServer := &http.Server{
		Handler:           services.Content,
		TLSConfig:         config.ContentTLS.Clone(),
		ReadHeaderTimeout: config.ContentReadTimeout,
		ReadTimeout:       config.ContentReadTimeout,
		WriteTimeout:      config.ContentWriteTimeout,
		IdleTimeout:       config.ContentIdleTimeout,
		MaxHeaderBytes:    config.ContentMaxHeaderBytes,
	}
	if err := http2.ConfigureServer(contentServer, &http2.Server{
		MaxConcurrentStreams: uint32(config.ContentMaxStreams),
	}); err != nil {
		return nil, fmt.Errorf("configure content HTTP/2 limits: %w", err)
	}
	return &PrivateServerSet{
		config:        config,
		controlServer: controlServer,
		outputServer:  outputServer,
		contentServer: contentServer,
		listen:        listen,
		shutdownDone:  make(chan struct{}),
	}, nil
}

func validateGRPCServerPolicy(policy GRPCServerPolicy) error {
	if policy.MaxConcurrentStreams == 0 || policy.MaxConcurrentStreams > maxPrivateConcurrentStreams || policy.MaxConnections <= 0 || policy.MaxConnections > maxPrivateConnections {
		return errors.New("stream or connection limit is invalid")
	}
	if policy.MinClientPingInterval < time.Second || policy.KeepaliveTime < time.Second || policy.KeepaliveTimeout < time.Second || policy.MaxConnectionIdle < time.Second || policy.MaxConnectionAge < time.Second || policy.MaxConnectionAgeGrace < time.Second {
		return errors.New("keepalive durations must be at least one second")
	}
	if policy.KeepaliveTimeout > policy.KeepaliveTime || policy.MaxConnectionIdle > policy.MaxConnectionAge || policy.MaxConnectionAgeGrace > policy.MaxConnectionAge {
		return errors.New("keepalive duration ordering is invalid")
	}
	return nil
}

func privateGRPCServerOptions(codec *StrictProtoCodec, serverTLS *tls.Config, policy GRPCServerPolicy) []grpc.ServerOption {
	options := StrictServerOptions(codec)
	return append(options,
		grpc.Creds(credentials.NewTLS(serverTLS.Clone())),
		grpc.MaxConcurrentStreams(policy.MaxConcurrentStreams),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             policy.MinClientPingInterval,
			PermitWithoutStream: false,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     policy.MaxConnectionIdle,
			MaxConnectionAge:      policy.MaxConnectionAge,
			MaxConnectionAgeGrace: policy.MaxConnectionAgeGrace,
			Time:                  policy.KeepaliveTime,
			Timeout:               policy.KeepaliveTimeout,
		}),
	)
}

// Serve binds all three listeners before starting any server. A bind failure
// closes every listener already acquired. Context cancellation or one server
// failure drains all siblings through one bounded shutdown owner.
func (s *PrivateServerSet) Serve(ctx context.Context) error {
	if ctx == nil {
		return errors.New("runtime server context is required")
	}
	listeners, err := s.openListeners()
	if err != nil {
		return err
	}
	defer listeners.close()

	type serveResult struct {
		name string
		err  error
	}
	results := make(chan serveResult, 3)
	go func() { results <- serveResult{name: "control", err: s.controlServer.Serve(listeners.control)} }()
	go func() { results <- serveResult{name: "output", err: s.outputServer.Serve(listeners.output)} }()
	go func() {
		// ServeTLS configures HTTP/2 when the server TLS configuration advertises
		// h2. The certificates are already present, so file arguments stay empty.
		results <- serveResult{name: "content", err: s.contentServer.ServeTLS(listeners.content, "", "")}
	}()

	var cause error
	select {
	case <-ctx.Done():
		cause = ctx.Err()
	case result := <-results:
		if s.shuttingDown.Load() && (result.err == nil || errors.Is(result.err, grpc.ErrServerStopped) || errors.Is(result.err, http.ErrServerClosed)) {
			cause = context.Canceled
		} else if result.err != nil && !errors.Is(result.err, grpc.ErrServerStopped) && !errors.Is(result.err, http.ErrServerClosed) {
			cause = fmt.Errorf("runtime %s listener failed: %w", result.name, result.err)
		} else {
			cause = fmt.Errorf("runtime %s listener stopped unexpectedly", result.name)
		}
	}

	shutdownCtx, cancel := privateShutdownContext(ctx, s.config.ShutdownTimeout)
	defer cancel()
	shutdownErr := s.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		return errors.Join(cause, shutdownErr)
	}
	return cause
}

// Shutdown drains all private listeners exactly once. The first caller owns
// the supplied deadline; subsequent callers wait for that same drain. This
// lets elitea-main pass its one application-wide drain context instead of
// racing an independent listener timeout against database and Redis closure.
func (s *PrivateServerSet) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("runtime listener shutdown context is required")
	}
	s.shuttingDown.Store(true)
	s.shutdownOnce.Do(func() {
		go func() {
			s.shutdownErr = s.shutdownServers(ctx)
			close(s.shutdownDone)
		}()
	})

	select {
	case <-s.shutdownDone:
		return s.shutdownErr
	case <-ctx.Done():
		// shutdownServers uses this same context and hard-stops both gRPC
		// servers when it expires. Wait for ownership to be released before
		// returning so callers may safely close dependent pools.
		<-s.shutdownDone
		return errors.Join(s.shutdownErr, ctx.Err())
	}
}

func privateShutdownContext(ctx context.Context, fallback time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok && ctx.Err() == nil {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(context.Background(), fallback)
}

type privateListeners struct {
	control net.Listener
	output  net.Listener
	content net.Listener
}

func (s *PrivateServerSet) openListeners() (privateListeners, error) {
	var listeners privateListeners
	var err error
	listeners.control, err = s.openBoundedListener(s.config.ControlAddress, s.config.ControlGRPC.MaxConnections)
	if err != nil {
		return privateListeners{}, fmt.Errorf("listen for runtime control: %w", err)
	}
	listeners.output, err = s.openBoundedListener(s.config.OutputAddress, s.config.OutputGRPC.MaxConnections)
	if err != nil {
		_ = listeners.control.Close()
		return privateListeners{}, fmt.Errorf("listen for runtime output: %w", err)
	}
	listeners.content, err = s.openBoundedListener(s.config.ContentAddress, s.config.ContentMaxConnections)
	if err != nil {
		_ = listeners.output.Close()
		_ = listeners.control.Close()
		return privateListeners{}, fmt.Errorf("listen for runtime content: %w", err)
	}
	return listeners, nil
}

func (s *PrivateServerSet) openBoundedListener(address string, maxConnections int) (net.Listener, error) {
	listener, err := s.listen("tcp", address)
	if err != nil {
		return nil, err
	}
	return newBoundedListener(listener, maxConnections), nil
}

func (l privateListeners) close() {
	if l.control != nil {
		_ = l.control.Close()
	}
	if l.output != nil {
		_ = l.output.Close()
	}
	if l.content != nil {
		_ = l.content.Close()
	}
}

func (s *PrivateServerSet) shutdownServers(ctx context.Context) error {
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		gracefulStop(ctx, s.controlServer)
	}()
	go func() {
		defer wait.Done()
		gracefulStop(ctx, s.outputServer)
	}()
	httpErr := s.contentServer.Shutdown(ctx)
	wait.Wait()
	if httpErr != nil && !errors.Is(httpErr, context.Canceled) && !errors.Is(httpErr, context.DeadlineExceeded) {
		return fmt.Errorf("shutdown runtime content listener: %w", httpErr)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("runtime listener shutdown: %w", err)
	}
	return nil
}

func gracefulStop(ctx context.Context, server *grpc.Server) {
	done := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		server.Stop()
		<-done
	}
}

type boundedListener struct {
	net.Listener
	slots     chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func newBoundedListener(listener net.Listener, maxConnections int) *boundedListener {
	return &boundedListener{
		Listener: listener,
		slots:    make(chan struct{}, maxConnections),
		closed:   make(chan struct{}),
	}
}

func (l *boundedListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.closed:
		return nil, net.ErrClosed
	}
	connection, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &boundedConnection{Conn: connection, release: func() { <-l.slots }}, nil
}

func (l *boundedListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

type boundedConnection struct {
	net.Conn
	release     func()
	releaseOnce sync.Once
}

func (c *boundedConnection) Close() error {
	err := c.Conn.Close()
	c.releaseOnce.Do(c.release)
	return err
}
