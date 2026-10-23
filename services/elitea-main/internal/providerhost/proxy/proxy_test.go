package proxy_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/hop"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/proxy"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/proxy/providertest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/spi"
)

// What the shared hop must do for BOTH facades, proved against a real gRPC
// server with mutual TLS (#1359): answer the facade's caller with the bytes
// the HTTP SPI answered, report the provider's status back through the
// Outcome, and carry nothing across but the signed identity.

func TestForwardSignsTheIdentityAndCarriesNothingElse(t *testing.T) {
	var seen http.Header
	cfg := mtlsProvider(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"slots":1}`))
	})
	hop := build(t, cfg)

	outcome := &proxy.Outcome{CaptureLimit: 1 << 10}
	request := httptest.NewRequest(http.MethodGet, "/api/v2/anything/slots/1", nil)
	request.Header.Set("X-Secret", "legacy-shared-secret")
	request.Header.Set("Authorization", "Bearer browser")
	request.Header.Set("X-Elitea-Project-Id", "999") // a caller's forgery; the signer overwrites it
	recorder := httptest.NewRecorder()
	hop.Forward(recorder, request.WithContext(proxy.WithOutcome(request.Context(), outcome)), spi.SlotsPath, "1", "7")

	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"slots":1}` {
		t.Fatalf("caller saw %d %q", recorder.Code, recorder.Body.String())
	}
	if outcome.Status != http.StatusOK || string(outcome.Body) != `{"slots":1}` || outcome.Truncated {
		t.Fatalf("Outcome %+v", outcome)
	}
	for _, header := range []string{"X-Secret", "Authorization"} {
		if got := seen.Get(header); got != "" {
			t.Fatalf("%s reached the provider as %q", header, got)
		}
	}
	if got := seen.Get("X-Elitea-Project-Id"); got != "1" {
		t.Fatalf("the signed project id was %q, want the facade's 1", got)
	}
	if got := seen.Get("X-Elitea-User-Id"); got != "7" {
		t.Fatalf("the signed user id was %q, want 7", got)
	}
	if seen.Get("X-Elitea-Identity-Signature") == "" {
		t.Fatal("the hop did not sign the identity")
	}
}

// Every SPI route keeps its HTTP answer: the invoke body reaches the
// provider and its acceptance comes back, a poll returns the body, a cancel
// is a 204, and a refusal is the transport-error envelope byte for byte.
func TestForwardKeepsTheSPIAnswers(t *testing.T) {
	var invoked string
	cfg := mtlsProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/tools/Wiki s/ask/invoke":
			raw, _ := io.ReadAll(r.Body)
			invoked = string(raw)
			_, _ = w.Write([]byte(`{"invocation_id":"invocation_1","status":"Started"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/tools/Wiki s/ask/invocations/invocation_1":
			_, _ = w.Write([]byte(`{"invocation_id":"invocation_1","status":"Completed","result":"done"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/tools/Wiki s/ask/invocations/invocation_1":
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/tools/Wiki s/ask/invocations/gone":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"details":[],"errorCode":"404","message":"Resource Not Found"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	hop := build(t, cfg)
	call := func(method, path, body string) (*httptest.ResponseRecorder, *proxy.Outcome) {
		outcome := &proxy.Outcome{CaptureLimit: 16}
		request := httptest.NewRequest(method, "/facade", strings.NewReader(body))
		recorder := httptest.NewRecorder()
		hop.Forward(recorder, request.WithContext(proxy.WithOutcome(request.Context(), outcome)), path, "1", "7")
		return recorder, outcome
	}

	accepted, outcome := call(http.MethodPost, spi.InvokePath("Wiki s", "ask"), `{"question":"why"}`)
	if accepted.Code != http.StatusOK || accepted.Body.String() != `{"invocation_id":"invocation_1","status":"Started"}` {
		t.Fatalf("invoke answered %d %q", accepted.Code, accepted.Body.String())
	}
	if invoked != `{"question":"why"}` {
		t.Fatalf("the provider received %q", invoked)
	}
	// The capture is a prefix and says so; the caller still has it all.
	if outcome.Status != http.StatusOK || !outcome.Truncated || string(outcome.Body) != `{"invocation_id"` {
		t.Fatalf("Outcome %+v", outcome)
	}
	if polled, _ := call(http.MethodGet, spi.InvocationPath("Wiki s", "ask", "invocation_1"), ""); polled.Code != http.StatusOK ||
		polled.Body.String() != `{"invocation_id":"invocation_1","status":"Completed","result":"done"}` {
		t.Fatalf("poll answered %d %q", polled.Code, polled.Body.String())
	}
	if cancelled, outcome := call(http.MethodDelete, spi.InvocationPath("Wiki s", "ask", "invocation_1"), ""); cancelled.Code != http.StatusNoContent ||
		cancelled.Body.Len() != 0 || outcome.Status != http.StatusNoContent {
		t.Fatalf("cancel answered %d %q", cancelled.Code, cancelled.Body.String())
	}
	for _, path := range []string{spi.InvocationPath("Wiki s", "ask", "gone")} {
		gone, outcome := call(http.MethodGet, path, "")
		if gone.Code != http.StatusNotFound || gone.Body.String() != `{"details":[],"errorCode":"404","message":"Resource Not Found"}` || outcome.Status != http.StatusNotFound {
			t.Fatalf("an unknown invocation answered %d %q", gone.Code, gone.Body.String())
		}
	}
	if bad, _ := call(http.MethodPost, spi.InvokePath("Wiki s", "unknown"), `{}`); bad.Code != http.StatusBadRequest ||
		bad.Body.String() != `{"details":[],"errorCode":"400","message":"Bad Request"}` {
		t.Fatalf("a refused invoke answered %d %q", bad.Code, bad.Body.String())
	}
	// A path the SPI does not have never reaches the provider.
	if odd, _ := call(http.MethodPut, "/descriptor", ""); odd.Code != http.StatusNotFound {
		t.Fatalf("a non-SPI path answered %d", odd.Code)
	}
	// An invoke body over the provider's limit is refused here.
	if huge, _ := call(http.MethodPost, spi.InvokePath("Wiki s", "ask"), strings.Repeat("x", proxy.MaxInvokeBytes+1)); huge.Code != http.StatusBadRequest {
		t.Fatalf("an oversized invoke answered %d", huge.Code)
	}
}

func TestOutcomeStaysZeroWhenTheProviderIsUnreachable(t *testing.T) {
	cfg := mtlsProvider(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	cfg.Target = "127.0.0.1:1" // nothing listens
	cfg.Timeout = 2 * time.Second
	hop := build(t, cfg)
	outcome := &proxy.Outcome{}
	request := httptest.NewRequest(http.MethodGet, "/x", nil)
	recorder := httptest.NewRecorder()
	hop.Forward(recorder, request.WithContext(proxy.WithOutcome(context.Background(), outcome)), spi.SlotsPath, "1", "7")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("an unreachable provider answered %d, want 503", recorder.Code)
	}
	if outcome.Status != 0 {
		t.Fatalf("Outcome for an unreached provider was %d, want 0", outcome.Status)
	}
}

// A client certificate the provider's CA did not sign is refused at the
// handshake: the facade answers 503 and the provider's handler never runs.
func TestAForeignClientCertificateIsRefused(t *testing.T) {
	ran := false
	cfg := mtlsProvider(t, func(w http.ResponseWriter, _ *http.Request) { ran = true })
	ca, caKey := authority(t, "another-ca")
	foreign := issue(t, ca, caKey, "elitea-main", x509.ExtKeyUsageClientAuth)
	writePEM(t, cfg.ClientCertFile, "CERTIFICATE", foreign.Certificate[0])
	key, _ := x509.MarshalECPrivateKey(foreign.PrivateKey.(*ecdsa.PrivateKey))
	writePEM(t, cfg.ClientKeyFile, "EC PRIVATE KEY", key)
	cfg.Timeout = 2 * time.Second
	hop := build(t, cfg)
	recorder := httptest.NewRecorder()
	hop.Forward(recorder, httptest.NewRequest(http.MethodGet, "/x", nil), spi.SlotsPath, "1", "7")
	if recorder.Code != http.StatusServiceUnavailable || ran {
		t.Fatalf("a foreign certificate answered %d (handler ran: %v)", recorder.Code, ran)
	}
}

func TestNewRefusesAURLTarget(t *testing.T) {
	cfg := mtlsProvider(t, func(http.ResponseWriter, *http.Request) {})
	for _, target := range []string{"https://elitea-deepwiki:8080", "", "elitea-deepwiki", ":9443"} {
		cfg.Target = target
		if _, err := proxy.New(cfg, "ELITEA_TEST_PLATFORM_GRPC_TARGET", nil); !errors.Is(err, hop.ErrInvalidTarget) ||
			!strings.Contains(err.Error(), "ELITEA_TEST_PLATFORM_GRPC_TARGET") {
			t.Errorf("%q: %v", target, err)
		}
	}
}

func build(t *testing.T, cfg facade.Config) *proxy.Proxy {
	t.Helper()
	hop, err := proxy.New(cfg, "ELITEA_TEST_PLATFORM_GRPC_TARGET", nil)
	if err != nil {
		t.Fatalf("building the hop: %v", err)
	}
	t.Cleanup(func() { _ = hop.Close() })
	return hop
}

// mtlsProvider serves handler as a provider behind mutual TLS and returns a
// facade.Config whose client certificate the server trusts.
func mtlsProvider(t *testing.T, handler http.HandlerFunc) facade.Config {
	t.Helper()
	ca, caKey := authority(t, "providerhost-test-ca")
	serverCert := issue(t, ca, caKey, "provider.internal", x509.ExtKeyUsageServerAuth)
	clientCert := issue(t, ca, caKey, "elitea-main", x509.ExtKeyUsageClientAuth)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	target := providertest.Start(t, handler, &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	})
	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")
	caFile := filepath.Join(dir, "ca.crt")
	writePEM(t, certFile, "CERTIFICATE", clientCert.Certificate[0])
	key, err := x509.MarshalECPrivateKey(clientCert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, keyFile, "EC PRIVATE KEY", key)
	writePEM(t, caFile, "CERTIFICATE", ca.Raw)
	return facade.Config{
		Enabled:        true,
		Target:         target,
		ClientCertFile: certFile,
		ClientKeyFile:  keyFile,
		CAFile:         caFile,
		ServerName:     "provider.internal",
		IdentitySecret: "shared-with-the-provider",
		Timeout:        10 * time.Second,
	}
}

func authority(t *testing.T, name string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
}

func issue(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	encoded := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}
