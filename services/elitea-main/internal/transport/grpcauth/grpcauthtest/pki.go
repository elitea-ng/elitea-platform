// Package grpcauthtest is a throwaway runtime CA for real-TLS tests of
// internal gRPC services: a server certificate for one DNS name and client
// certificates for chosen workload identities, all issued per test.
//
//	pki := grpcauthtest.NewPKI(t)
//	serverTLS := pki.ServerTLS(t)                      // TLS 1.3, h2, RequireAndVerifyClientCert
//	worker := pki.ClientTLS(t, "elitea-worker")        // presents dns:elitea-worker
//	stranger := grpcauthtest.NewPKI(t).ClientTLS(t, "elitea-worker") // another CA
package grpcauthtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// ServerName is the DNS SAN of the server certificate ServerTLS issues.
const ServerName = "elitea-main"

// PKI is one test certificate authority.
type PKI struct {
	ca      *x509.Certificate
	caKey   ed25519.PrivateKey
	roots   *x509.CertPool
	serials atomic.Int64
}

// NewPKI creates a fresh CA.
func NewPKI(t testing.TB) *PKI {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ELITEA gRPC test runtime CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	pki := &PKI{ca: ca, caKey: private, roots: roots}
	pki.serials.Store(1)
	return pki
}

// Roots is a pool holding only this CA.
func (p *PKI) Roots() *x509.CertPool { return p.roots.Clone() }

func (p *PKI) issue(t testing.TB, dnsName string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(p.serials.Add(1)),
		// The common name is deliberately not the identity: workloadidentity
		// never reads it.
		Subject:     pkix.Name{CommonName: "ignored-" + dnsName},
		DNSNames:    []string{dnsName},
		NotBefore:   now.Add(-time.Minute),
		NotAfter:    now.Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, p.ca, public, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, p.ca.Raw}, PrivateKey: private}
}

// ServerTLS is the private listeners' server profile with a certificate for
// ServerName: TLS 1.3, ALPN h2, a client certificate from this CA required.
func (p *PKI) ServerTLS(t testing.TB) *tls.Config {
	t.Helper()
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{p.issue(t, ServerName, x509.ExtKeyUsageServerAuth)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    p.Roots(),
		NextProtos:   []string{"h2"},
	}
}

// ClientTLS presents a client certificate whose only SAN is dnsName, so the
// workload identity the server sees is "dns:"+dnsName. It trusts this CA for
// the server and expects ServerName.
func (p *PKI) ClientTLS(t testing.TB, dnsName string) *tls.Config {
	t.Helper()
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{p.issue(t, dnsName, x509.ExtKeyUsageClientAuth)},
		RootCAs:      p.Roots(),
		ServerName:   ServerName,
	}
}

// WriteFiles writes this CA and a client certificate for dnsName as PEM files
// under dir, for tests of tools that take file paths.
func (p *PKI) WriteFiles(t testing.TB, dir, dnsName string) (caPath, certPath, keyPath string) {
	t.Helper()
	client := p.issue(t, dnsName, x509.ExtKeyUsageClientAuth)
	key, err := x509.MarshalPKCS8PrivateKey(client.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	caPath = filepath.Join(dir, "runtime-ca.crt")
	certPath = filepath.Join(dir, dnsName+".crt")
	keyPath = filepath.Join(dir, dnsName+".key")
	write := func(path, kind string, der []byte) {
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(caPath, "CERTIFICATE", p.ca.Raw)
	write(certPath, "CERTIFICATE", client.Certificate[0])
	write(keyPath, "PRIVATE KEY", key)
	return caPath, certPath, keyPath
}

// AnonymousTLS trusts this CA for the server but presents no certificate.
func (p *PKI) AnonymousTLS() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: p.Roots(), ServerName: ServerName}
}
