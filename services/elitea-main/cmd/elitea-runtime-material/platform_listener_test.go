package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
)

// TestPlatformListenerAdmitsOnlyRuntimeCAClients holds the platform listener
// (#1359, ELITEA_RUNTIME_PLATFORM_ADDRESS) to its material: installed from the
// same Secret-volume material and loaded through the same config path the
// composition root uses, the gRPC listener
//
//   - SERVES grpc.health.v1 to a client that presents a runtime-CA client
//     certificate;
//   - REFUSES at the handshake a client with no certificate at all;
//   - REFUSES a client whose certificate another authority signed.
func TestPlatformListenerAdmitsOnlyRuntimeCAClients(t *testing.T) {
	fixture := newMaterialFixture(t)
	pod := newDeployment(t, fixture, secretVolumeMode)
	address := freeLoopbackAddresses(t, 1)[0]
	pod.environment["ELITEA_RUNTIME_PLATFORM_ADDRESS"] = address
	// The chart's choice: the platform listener reuses the content listener's
	// keypair (its SAN is the elitea-main Service name) and the runtime CA.
	pod.environment["ELITEA_RUNTIME_PLATFORM_TLS_CERT_FILE"] = pod.path("content-server.crt")
	pod.environment["ELITEA_RUNTIME_PLATFORM_TLS_KEY_FILE"] = pod.path("content-server.key")
	pod.environment["ELITEA_RUNTIME_PLATFORM_TLS_CLIENT_CA_FILE"] = pod.path(caName)

	if _, err := install(pod.source, pod.lookup); err != nil {
		t.Fatalf("the init container could not install the material: %v", err)
	}
	config, err := runtimecomposition.ConfigFromEnv(pod.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if config.PlatformAddress != address {
		t.Fatalf("PlatformAddress = %q, want %q", config.PlatformAddress, address)
	}
	serverTLS, err := runtimegrpc.LoadPlatformServerTLSConfig(config.PlatformTLS)
	if err != nil {
		t.Fatalf("load platform listener TLS: %v", err)
	}
	server, err := runtimegrpc.NewPlatformServer(runtimegrpc.PlatformServerConfig{
		Address: config.PlatformAddress,
		TLS:     serverTLS,
		Policy:  runtimegrpc.DefaultPlatformGRPCPolicy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.ListenAndServe() }()
	awaitPrivateListeners(t, address)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		if err := <-served; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("platform listener: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	t.Run("a runtime-CA client with a workload identity", func(t *testing.T) {
		response, err := platformHealth(ctx, address, workloadClientTLS(t, fixture, "elitea-worker"))
		if err != nil || response != healthv1.HealthCheckResponse_SERVING {
			t.Fatalf("the platform listener did not serve a runtime-CA client: %v, %v", response, err)
		}
	})

	// The fixture's generic client certificate also carries an IP SAN, so it
	// names no single workload: the handshake succeeds, the call does not.
	t.Run("a runtime-CA client without one workload identity", func(t *testing.T) {
		if _, err := platformHealth(ctx, address, fixture.clientTLSFor(t, "content.runtime.test")); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("an ambiguous identity = %v, want Unauthenticated", err)
		}
	})

	t.Run("a client with no certificate", func(t *testing.T) {
		anonymous := &tls.Config{
			MinVersion: tls.VersionTLS13,
			RootCAs:    fixture.rootPool(),
			ServerName: "content.runtime.test",
		}
		if _, err := platformHealth(ctx, address, anonymous); status.Code(err) != codes.Unavailable {
			t.Fatalf("a client without a certificate = %v, want a handshake failure", err)
		}
	})

	t.Run("a client another authority signed", func(t *testing.T) {
		stranger := newMaterialFixture(t).clientTLSFor(t, "content.runtime.test")
		stranger.RootCAs = fixture.rootPool()
		if _, err := platformHealth(ctx, address, stranger); status.Code(err) != codes.Unavailable {
			t.Fatalf("a client the runtime CA did not sign = %v, want a handshake failure", err)
		}
	})
}

// workloadClientTLS is a client certificate from the fixture's CA whose only
// SAN is one DNS name, as gen-runtime-certs.sh issues them.
func workloadClientTLS(t *testing.T, fixture materialFixture, dnsName string) *tls.Config {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serialCounter++
	now := time.Now().UTC()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(serialCounter),
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{dnsName},
	}, fixture.ca.certificate, publicKey, fixture.ca.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: privateKey}},
		RootCAs:      fixture.rootPool(),
		ServerName:   "content.runtime.test",
	}
}

func platformHealth(ctx context.Context, address string, clientTLS *tls.Config) (healthv1.HealthCheckResponse_ServingStatus, error) {
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		return 0, err
	}
	defer func() { _ = connection.Close() }()
	response, err := healthv1.NewHealthClient(connection).Check(ctx, &healthv1.HealthCheckRequest{})
	return response.GetStatus(), err
}
