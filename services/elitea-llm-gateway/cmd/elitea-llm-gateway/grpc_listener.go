package main

// grpc_listener.go — the GatewayService gRPC listener (#1359, decision D4).
//
// elitea-main reaches the gateway over gRPC only. The /llm router and the
// governance status handler are served through grpcapi in process; the HTTP
// listener keeps /healthz, /readyz and /metrics for the kubelet and the
// scraper, and nothing else.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/config"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/grpcapi"
)

// newGatewayGRPC builds the GatewayService listener. It refuses to build one
// without mutual TLS or without a client allowlist: the listener is the only
// way to the /llm surface, so either gap is a misconfiguration main() stops on.
func newGatewayGRPC(cfg config.Config, llm, status http.Handler, logger *slog.Logger) (*grpcapi.Server, error) {
	service, err := grpcapi.NewService(grpcapi.Config{LLM: llm, Status: status, Logger: logger})
	if err != nil {
		return nil, err
	}
	tlsConfig, err := grpcapi.ServerTLS(grpcapi.TLSFiles{
		CertFile: cfg.TLSCertFile,
		KeyFile:  cfg.TLSKeyFile,
		CAFile:   cfg.TLSCAFile,
	})
	if err != nil {
		return nil, err
	}
	allow, err := grpcapi.ParseAllowlist(cfg.GRPCClients)
	if err != nil {
		return nil, fmt.Errorf("GATEWAY_GRPC_CLIENTS: %w", err)
	}
	server, err := grpcapi.NewServer(cfg.GRPCAddr, tlsConfig, allow, service)
	if err != nil {
		return nil, err
	}
	logger.Info("GatewayService gRPC listener configured",
		"addr", cfg.GRPCAddr, "clients", cfg.GRPCClients)
	return server, nil
}

// gatewayServers puts the two listeners behind the one httpShutdowner seam of
// shutdownSequence, so the ordering that loses no spend stays in one place.
type gatewayServers struct {
	http    httpShutdowner
	grpc    grpcDrainer
	timeout time.Duration
}

// newGatewayServers is the one place the two listeners are paired.
func newGatewayServers(http httpShutdowner, grpc grpcDrainer, timeout time.Duration) gatewayServers {
	return gatewayServers{http: http, grpc: grpc, timeout: timeout}
}

type grpcDrainer interface {
	Drain(context.Context) error
	Stop()
}

// ShutdownHTTP is phase 1: gRPC calls in flight finish (streams settle with
// billing open), then the HTTP listener stops and bifrost is released. The
// gRPC drain runs FIRST because bifrost's shutdown inside ShutdownHTTP would
// cut the streams it serves. Realtime sessions keep running into phase 3.
func (s gatewayServers) ShutdownHTTP(ctx context.Context) error {
	drainCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	grpcErr := s.grpc.Drain(drainCtx)
	if grpcErr != nil {
		grpcErr = fmt.Errorf("gRPC drain: %w", grpcErr)
	}
	srv := s.http
	return errors.Join(grpcErr, srv.ShutdownHTTP(ctx))
}

// Close ends whatever gRPC call is still open, then closes NATS.
func (s gatewayServers) Close() {
	s.grpc.Stop()
	srv := s.http
	srv.Close()
}
