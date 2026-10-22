package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/config"
)

// grpcListenerTestEnv is the environment a gateway subprocess needs to start
// since #1359: the GatewayService listener refuses to run without mutual TLS,
// and main() stops when it cannot build it. It writes a throwaway CA and a
// server certificate and binds the listener to a free loopback port.
func grpcListenerTestEnv(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	caPub, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "gateway test CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caPub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		DNSNames:     []string{"elitea-llm-gateway"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, pub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, kind string, der []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return []string{
		"GATEWAY_TLS_CA_FILE=" + write("ca.crt", "CERTIFICATE", caDER),
		"GATEWAY_TLS_CERT_FILE=" + write("tls.crt", "CERTIFICATE", der),
		"GATEWAY_TLS_KEY_FILE=" + write("tls.key", "PRIVATE KEY", pkcs8),
		"GATEWAY_GRPC_ADDR=127.0.0.1:0",
	}
}

func TestNewGatewayGRPCRefusesWithoutMutualTLS(t *testing.T) {
	cfg := config.Config{GRPCAddr: "127.0.0.1:0", GRPCClients: config.DefaultGRPCClients}
	_, err := newGatewayGRPC(cfg, http.NotFoundHandler(), http.NotFoundHandler(), testLogger())
	if err == nil || !strings.Contains(err.Error(), "GATEWAY_TLS_CA_FILE") {
		t.Fatalf("newGatewayGRPC without TLS material = %v; the listener must never serve without mutual TLS", err)
	}
}

func TestNewGatewayGRPCRefusesAnEmptyAllowlist(t *testing.T) {
	cfg := config.Config{GRPCAddr: "127.0.0.1:0", GRPCClients: " , "}
	for _, kv := range grpcListenerTestEnv(t) {
		name, value, _ := strings.Cut(kv, "=")
		switch name {
		case "GATEWAY_TLS_CA_FILE":
			cfg.TLSCAFile = value
		case "GATEWAY_TLS_CERT_FILE":
			cfg.TLSCertFile = value
		case "GATEWAY_TLS_KEY_FILE":
			cfg.TLSKeyFile = value
		}
	}
	if _, err := newGatewayGRPC(cfg, http.NotFoundHandler(), http.NotFoundHandler(), testLogger()); err == nil || !strings.Contains(err.Error(), "GATEWAY_GRPC_CLIENTS") {
		t.Fatalf("newGatewayGRPC with no client identity = %v, want a GATEWAY_GRPC_CLIENTS error", err)
	}
}

type spyShutdown struct{ order *[]string }

func (s spyShutdown) ShutdownHTTP(context.Context) error {
	*s.order = append(*s.order, "http")
	return nil
}
func (s spyShutdown) Close() { *s.order = append(*s.order, "nats") }

type spyGRPC struct {
	order *[]string
	err   error
}

func (s spyGRPC) Drain(context.Context) error {
	*s.order = append(*s.order, "grpc-drain")
	return s.err
}
func (s spyGRPC) Stop() { *s.order = append(*s.order, "grpc-stop") }

// TestGatewayServersOrder pins the order the composite runs in: the gRPC
// calls drain before bifrost is released inside ShutdownHTTP, and the last
// gRPC call is stopped before NATS closes.
func TestGatewayServersOrder(t *testing.T) {
	var order []string
	servers := newGatewayServers(spyShutdown{&order}, spyGRPC{order: &order, err: errors.New("deadline")}, time.Second)
	if err := servers.ShutdownHTTP(context.Background()); err == nil {
		t.Fatal("a gRPC drain that timed out must be reported")
	}
	servers.Close()
	if got := strings.Join(order, ","); got != "grpc-drain,http,grpc-stop,nats" {
		t.Fatalf("shutdown order = %s, want grpc-drain,http,grpc-stop,nats", got)
	}
}
