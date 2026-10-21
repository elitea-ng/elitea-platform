package main

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
)

// TestPlatformListenerAdmitsOnlyRuntimeCAClients is the gate the retired
// platform-edge Traefik had no equivalent of (#1359): the platform listener —
// elitea-main's product API on ELITEA_RUNTIME_PLATFORM_ADDRESS — is installed
// from the same Secret-volume material, loaded through the same config path the
// composition root uses, and then
//
//   - SERVES a client that presents a runtime-CA client certificate, over
//     both HTTP/1.1 and h2 (it is REST + SSE, not gRPC);
//   - REFUSES at the handshake a client with no certificate at all;
//   - REFUSES a client whose certificate another authority signed.
//
// A refusal is checked by the absence of a response, and a success by a body
// only the handler can produce.
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

	server := &http.Server{
		Addr: config.PlatformAddress,
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			_, _ = io.WriteString(writer, "platform listener served "+request.Proto+" "+request.URL.Path)
		}),
		TLSConfig:         serverTLS,
		ReadHeaderTimeout: 5 * time.Second,
	}
	served := make(chan error, 1)
	go func() { served <- server.ListenAndServeTLS("", "") }()
	awaitPrivateListeners(t, address)
	defer func() {
		_ = server.Close()
		if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("platform listener: %v", err)
		}
	}()

	worker := fixture.clientTLSFor(t, "content.runtime.test")
	worker.NextProtos = nil
	for _, testCase := range []struct {
		name  string
		http2 bool
		proto string
	}{
		{name: "HTTP/1.1", http2: false, proto: "HTTP/1.1"},
		{name: "h2", http2: true, proto: "HTTP/2.0"},
	} {
		t.Run("a runtime-CA client over "+testCase.name, func(t *testing.T) {
			body, err := platformGet(address, worker.Clone(), testCase.http2)
			if err != nil {
				t.Fatalf("the platform listener did not serve a runtime-CA client: %v", err)
			}
			if want := "platform listener served " + testCase.proto + " /api/v2/probe"; body != want {
				t.Fatalf("body = %q, want %q", body, want)
			}
		})
	}

	t.Run("a client with no certificate", func(t *testing.T) {
		anonymous := &tls.Config{
			MinVersion: tls.VersionTLS13,
			RootCAs:    fixture.rootPool(),
			ServerName: "content.runtime.test",
		}
		if body, err := platformGet(address, anonymous, false); err == nil {
			t.Fatalf("the platform listener served a client without a certificate: %q", body)
		}
	})

	t.Run("a client another authority signed", func(t *testing.T) {
		stranger := newMaterialFixture(t).clientTLSFor(t, "content.runtime.test")
		stranger.RootCAs = fixture.rootPool()
		stranger.NextProtos = nil
		if body, err := platformGet(address, stranger, false); err == nil {
			t.Fatalf("the platform listener served a client the runtime CA did not sign: %q", body)
		}
	})
}

func platformGet(address string, clientTLS *tls.Config, http2 bool) (string, error) {
	transport := &http.Transport{
		TLSClientConfig: clientTLS,
		Protocols:       new(http.Protocols),
		DialContext:     (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	}
	if http2 {
		transport.Protocols.SetHTTP2(true)
	} else {
		transport.Protocols.SetHTTP1(true)
	}
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Get("https://" + address + "/api/v2/probe")
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return string(body), errors.New(response.Status)
	}
	return string(body), nil
}
