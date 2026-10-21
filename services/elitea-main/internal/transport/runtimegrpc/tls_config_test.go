package runtimegrpc

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

func TestServerTLSConfigRequiresTLS13VerifiedClientCertificates(t *testing.T) {
	certificate, roots := runtimeServerCertificate(t)
	config, err := NewServerTLSConfig(certificate, roots)
	if err != nil {
		t.Fatal(err)
	}
	if config.MinVersion != tls.VersionTLS13 || config.ClientAuth != tls.RequireAndVerifyClientCert || config.ClientCAs == nil {
		t.Fatalf("unexpected runtime TLS policy: min=%d client_auth=%d", config.MinVersion, config.ClientAuth)
	}
	if len(config.NextProtos) != 1 || config.NextProtos[0] != "h2" {
		t.Fatalf("runtime TLS ALPN = %v, want h2 only", config.NextProtos)
	}
	if err := validateServerTLSConfig(config); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateServerSetRejectsWeakOrSharedListenerComposition(t *testing.T) {
	certificate, roots := runtimeServerCertificate(t)
	secure, err := NewServerTLSConfig(certificate, roots)
	if err != nil {
		t.Fatal(err)
	}
	services := PrivateServices{
		Control: runtimev1.UnimplementedRuntimeControlServiceServer{},
		Output:  runtimev1.UnimplementedExecutionOutputServiceServer{},
		Content: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	}
	valid := PrivateServerConfig{
		ControlAddress: ":9443", OutputAddress: ":9444", ContentAddress: ":9445",
		ControlTLS: secure, OutputTLS: secure.Clone(), ContentTLS: secure.Clone(),
		ControlMaxRequestBytes: 64 * 1024, ControlMaxResponseBytes: 80 * 1024,
		OutputMaxRequestBytes: 64 * 1024, OutputMaxResponseBytes: 80 * 1024,
		ControlGRPC: testGRPCServerPolicy(), OutputGRPC: testGRPCServerPolicy(),
		ContentMaxConnections: 16, ContentMaxStreams: 8,
		ContentReadTimeout: 5 * time.Second, ContentWriteTimeout: 30 * time.Second,
		ContentIdleTimeout: 30 * time.Second, ContentMaxHeaderBytes: 8 * 1024,
		ShutdownTimeout: 10 * time.Second,
	}
	if _, err := NewPrivateServerSet(valid, services); err != nil {
		t.Fatal(err)
	}

	shared := valid
	shared.OutputAddress = shared.ControlAddress
	if _, err := NewPrivateServerSet(shared, services); err == nil {
		t.Fatal("shared control/output listener was accepted")
	}

	weak := valid
	weak.ControlTLS = secure.Clone()
	weak.ControlTLS.MinVersion = tls.VersionTLS12
	if _, err := NewPrivateServerSet(weak, services); err == nil {
		t.Fatal("TLS 1.2 control listener was accepted")
	}

	noClientVerification := valid
	noClientVerification.ContentTLS = secure.Clone()
	noClientVerification.ContentTLS.ClientAuth = tls.NoClientCert
	if _, err := NewPrivateServerSet(noClientVerification, services); err == nil {
		t.Fatal("content listener without client-certificate verification was accepted")
	}

	replacedPolicy := valid
	replacedPolicy.OutputTLS = secure.Clone()
	replacedPolicy.OutputTLS.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return &tls.Config{MinVersion: tls.VersionTLS12}, nil
	}
	if _, err := NewPrivateServerSet(replacedPolicy, services); err == nil {
		t.Fatal("per-client TLS policy replacement was accepted")
	}

	unboundedStreams := valid
	unboundedStreams.OutputGRPC.MaxConcurrentStreams = 0
	if _, err := NewPrivateServerSet(unboundedStreams, services); err == nil {
		t.Fatal("unbounded output streams were accepted")
	}

	unboundedConnections := valid
	unboundedConnections.ControlGRPC.MaxConnections = 0
	if _, err := NewPrivateServerSet(unboundedConnections, services); err == nil {
		t.Fatal("unbounded control connections were accepted")
	}

	unboundedContentStreams := valid
	unboundedContentStreams.ContentMaxStreams = 0
	if _, err := NewPrivateServerSet(unboundedContentStreams, services); err == nil {
		t.Fatal("unbounded content HTTP/2 streams were accepted")
	}

	unsafeKeepalive := valid
	unsafeKeepalive.OutputGRPC.MaxConnectionAge = 10 * time.Second
	unsafeKeepalive.OutputGRPC.MaxConnectionIdle = 20 * time.Second
	if _, err := NewPrivateServerSet(unsafeKeepalive, services); err == nil {
		t.Fatal("invalid output keepalive ordering was accepted")
	}
}

func testGRPCServerPolicy() GRPCServerPolicy {
	return GRPCServerPolicy{
		MaxConcurrentStreams:  16,
		MaxConnections:        32,
		KeepaliveTime:         time.Minute,
		KeepaliveTimeout:      10 * time.Second,
		MaxConnectionIdle:     5 * time.Minute,
		MaxConnectionAge:      30 * time.Minute,
		MaxConnectionAgeGrace: 30 * time.Second,
	}
}

func runtimeServerCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	caPublic, caPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ELITEA runtime test CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caPublic, caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	_, serverPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "ignored-common-name"},
		DNSNames:     []string{"runtime.internal.test"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, ca, serverPrivate.Public(), caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return tls.Certificate{Certificate: [][]byte{serverDER, caDER}, PrivateKey: serverPrivate}, roots
}

func TestPlatformServerTLSConfigAdmitsHTTP1AndKeepsMutualTLS(t *testing.T) {
	certificate, roots := runtimeServerCertificate(t)
	config, err := NewServerTLSConfig(certificate, roots)
	if err != nil {
		t.Fatal(err)
	}
	config.NextProtos = []string{"h2", "http/1.1"}
	if err := ValidatePlatformServerTLSConfig(config); err != nil {
		t.Fatalf("the platform profile was refused: %v", err)
	}
	// The gRPC listeners' validator still refuses http/1.1.
	if err := validateServerTLSConfig(config); err == nil {
		t.Fatal("the gRPC listener validator admitted http/1.1")
	}
	for name, weaken := range map[string]func(*tls.Config){
		"optional client certificates": func(c *tls.Config) { c.ClientAuth = tls.VerifyClientCertIfGiven },
		"TLS 1.2":                      func(c *tls.Config) { c.MinVersion = tls.VersionTLS12 },
		"no client CA":                 func(c *tls.Config) { c.ClientCAs = nil },
		"h2 only":                      func(c *tls.Config) { c.NextProtos = []string{"h2"} },
		"per-client policy": func(c *tls.Config) {
			c.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) { return nil, nil }
		},
	} {
		weakened := config.Clone()
		weaken(weakened)
		if err := ValidatePlatformServerTLSConfig(weakened); err == nil {
			t.Errorf("%s: the platform profile admitted a weakened policy", name)
		}
	}
}
