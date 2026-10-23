package mtlsclient_test

// The reloading credentials against a real TLS listener: the material is
// loaded from files, a handshake verifies the server and presents the client
// leaf, and when the files are SWAPPED MID-RUN (the server moved to a new CA
// and the Secret was rewritten) the next handshake uses the new material
// without a new Credentials. A half-written rotation keeps the last good
// material and is retried.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/mtlsclient"
)

type pki struct {
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey
	caPEM []byte
	pool  *x509.CertPool
}

func newPKI(t *testing.T, name string) *pki {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &pki{ca: ca, caKey: key, pool: pool, caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (p *pki) leaf(t *testing.T, name string, usage x509.ExtKeyUsage) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, DNSNames: []string{name}}
	if usage == x509.ExtKeyUsageServerAuth {
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// writeClient writes the client leaf and the CA of p to the fixed paths in
// dir: the shape of a Secret mount, whose file names never change.
func writeClient(t *testing.T, dir string, p *pki) mtlsclient.Material {
	t.Helper()
	_, certPEM, keyPEM := p.leaf(t, "elitea-main", x509.ExtKeyUsageClientAuth)
	for name, body := range map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM, "ca.crt": p.caPEM} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return mtlsclient.Material{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key"),
		CAFile: filepath.Join(dir, "ca.crt"), ServerName: "server"}
}

// server is a TLS listener whose PKI the test switches: it serves the leaf of
// the current PKI and requires a client certificate from the same CA.
type server struct {
	addr    string
	current atomic.Pointer[tls.Config]
}

func serve(t *testing.T, p *pki) *server {
	t.Helper()
	s := &server{}
	s.use(t, p)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return s.current.Load(), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.addr = listener.Addr().String()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				// Drive the handshake so the client sees the verdict, then close.
				if tlsConn, ok := conn.(*tls.Conn); ok {
					_ = tlsConn.Handshake()
				}
				_ = conn.Close()
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return s
}

func (s *server) use(t *testing.T, p *pki) {
	t.Helper()
	leaf, _, _ := p.leaf(t, "server", x509.ExtKeyUsageServerAuth)
	// h2 is negotiated as a gRPC server would: grpc-go's credentials require
	// the ALPN result.
	s.current.Store(&tls.Config{Certificates: []tls.Certificate{leaf}, ClientCAs: p.pool,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}})
}

// handshake dials the server and runs the credentials' client handshake.
func handshake(t *testing.T, creds *mtlsclient.Credentials, addr string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	conn, _, err := creds.ClientHandshake(ctx, addr, raw)
	if err == nil {
		_ = conn.Close()
	}
	return err
}

// touch makes the file's stamp differ even when its new content has the same
// size and the write landed in the same timestamp tick.
func touch(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}

func TestMaterialSwappedMidRunIsUsedByTheNextHandshake(t *testing.T) {
	t.Parallel()
	old, fresh := newPKI(t, "runtime-ca-1"), newPKI(t, "runtime-ca-2")
	dir := t.TempDir()
	material := writeClient(t, dir, old)
	var reloadErrs []error
	creds, err := mtlsclient.NewCredentials(material, func(err error) { reloadErrs = append(reloadErrs, err) })
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, old)
	if err := handshake(t, creds, s.addr); err != nil {
		t.Fatalf("the first handshake with the loaded material failed: %v", err)
	}

	// The server rotates to a new CA; the files on disk are still the old
	// ones, so the handshake fails: the server's leaf chains to a CA this
	// process does not trust yet.
	s.use(t, fresh)
	if err := handshake(t, creds, s.addr); err == nil {
		t.Fatal("a handshake against a rotated server succeeded with the old roots")
	}

	// The Secret is rewritten with the new CA's material: the next handshake
	// reloads and succeeds, with the same Credentials.
	writeClient(t, dir, fresh)
	for _, name := range []string{"tls.crt", "tls.key", "ca.crt"} {
		touch(t, filepath.Join(dir, name))
	}
	if err := handshake(t, creds, s.addr); err != nil {
		t.Fatalf("the handshake after the rotation failed: %v", err)
	}
	if len(reloadErrs) != 0 {
		t.Fatalf("a clean rotation reported reload errors: %v", reloadErrs)
	}

	// Rotated back: the server trusts the old CA again, the files follow.
	s.use(t, old)
	writeClient(t, dir, old)
	for _, name := range []string{"tls.crt", "tls.key", "ca.crt"} {
		touch(t, filepath.Join(dir, name))
	}
	if err := handshake(t, creds, s.addr); err != nil {
		t.Fatalf("the handshake after rotating back failed: %v", err)
	}
}

func TestAHalfWrittenRotationKeepsTheLastGoodMaterial(t *testing.T) {
	t.Parallel()
	p := newPKI(t, "runtime-ca")
	dir := t.TempDir()
	material := writeClient(t, dir, p)
	var reloadErrs []error
	creds, err := mtlsclient.NewCredentials(material, func(err error) { reloadErrs = append(reloadErrs, err) })
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, p)
	if err := handshake(t, creds, s.addr); err != nil {
		t.Fatal(err)
	}
	// The key is truncated mid-rewrite: the reload fails, the handshake still
	// runs on the last good leaf, and the failure is reported.
	if err := os.WriteFile(material.KeyFile, []byte("-----BEGIN EC PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	touch(t, material.KeyFile)
	if err := handshake(t, creds, s.addr); err != nil {
		t.Fatalf("the handshake with a half-written rotation failed instead of using the last good material: %v", err)
	}
	if len(reloadErrs) == 0 {
		t.Fatal("a failed reload was not reported")
	}
	// The rewrite completes: the next handshake picks the new key up, because
	// the failed reload did not advance the stamps.
	writeClient(t, dir, p)
	for _, name := range []string{"tls.crt", "tls.key", "ca.crt"} {
		touch(t, filepath.Join(dir, name))
	}
	before := len(reloadErrs)
	if err := handshake(t, creds, s.addr); err != nil {
		t.Fatal(err)
	}
	if len(reloadErrs) != before {
		t.Fatalf("a completed rotation reported a reload error: %v", reloadErrs[before:])
	}
}

func TestConstructionFailsOnUnusableMaterial(t *testing.T) {
	t.Parallel()
	p := newPKI(t, "runtime-ca")
	dir := t.TempDir()
	good := writeClient(t, dir, p)
	if _, err := mtlsclient.NewCredentials(good, nil); err != nil {
		t.Fatalf("complete material failed: %v", err)
	}
	// The server name derives from the target's host.
	derived := good
	derived.ServerName, derived.Target = "", "vector.example:9470"
	if _, err := mtlsclient.NewCredentials(derived, nil); err != nil {
		t.Fatalf("a target without a server name failed: %v", err)
	}
	for name, mutate := range map[string]func(*mtlsclient.Material){
		"no cert":         func(m *mtlsclient.Material) { m.CertFile = "" },
		"no key":          func(m *mtlsclient.Material) { m.KeyFile = "" },
		"no ca":           func(m *mtlsclient.Material) { m.CAFile = "" },
		"no name":         func(m *mtlsclient.Material) { m.ServerName, m.Target = "", "" },
		"missing cert":    func(m *mtlsclient.Material) { m.CertFile = filepath.Join(dir, "absent.crt") },
		"ca is not a pem": func(m *mtlsclient.Material) { m.CAFile = m.KeyFile },
	} {
		m := good
		mutate(&m)
		if _, err := mtlsclient.NewCredentials(m, nil); err == nil {
			t.Errorf("%s: construction succeeded", name)
		}
	}
	// Client credentials cannot serve.
	creds, _ := mtlsclient.NewCredentials(good, nil)
	if _, _, err := creds.ServerHandshake(nil); err == nil {
		t.Error("ServerHandshake succeeded")
	}
	if info := creds.Info(); info.SecurityProtocol != "tls" {
		t.Errorf("Info = %+v", info)
	}
	if clone := creds.Clone(); clone == nil || clone == creds {
		t.Error("Clone must return a distinct credentials value")
	}
}
