package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The names are the contract between deploy/scripts/gen-runtime-certs.sh, the
// Helm chart and internal/runtimecomposition. They are the keys that the
// Kubernetes Secret must carry.
const (
	caName          = "runtime-ca.crt"
	keyringName     = "command-signing-keyring.json"
	signingKeyName  = "command-signing-key.pem"
	signingKeyID    = "runtime-test-v1"
	sourceDirectory = "source"
	targetDirectory = "material"
)

type authority struct {
	certificate *x509.Certificate
	der         []byte
	privateKey  ed25519.PrivateKey
}

// materialFixture is one complete set of runtime material, plus the client
// identity that a worker presents to the three listeners.
type materialFixture struct {
	keys         map[string][]byte
	ca           authority
	clientChain  []byte
	clientKeyPEM []byte
}

func newMaterialFixture(t *testing.T) materialFixture {
	t.Helper()
	ca := newAuthority(t, "elitea runtime test CA")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.der})

	keys := map[string][]byte{
		caName: caPEM,
	}
	for _, listener := range []string{"control", "output", "content"} {
		certificatePEM, privateKeyPEM := issueCertificate(t, ca, listener+".runtime.test", x509.ExtKeyUsageServerAuth)
		keys[listener+"-server.crt"] = certificatePEM
		keys[listener+"-server.key"] = privateKeyPEM
	}

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keys[signingKeyName] = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	keyring, err := json.Marshal(map[string]any{
		"schema_version": "elitea.runtime-ed25519-keyring.v1",
		"keys": []map[string]string{{
			"key_id":            signingKeyID,
			"public_key_base64": base64.StdEncoding.EncodeToString(publicKey),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	keys[keyringName] = keyring

	clientCertificate, clientKey := issueCertificate(t, ca, "agent-worker.runtime.test", x509.ExtKeyUsageClientAuth)
	return materialFixture{keys: keys, ca: ca, clientChain: clientCertificate, clientKeyPEM: clientKey}
}

// writeSecretVolume reproduces the layout that the kubelet writes for a Secret
// volume mounted whole: one timestamped directory that holds the real files, a
// "..data" symlink to it, and one relative symlink for each key.
//
// This is the layout that internal/security/securefile refuses, and it is the
// reason this command exists.
func (fixture materialFixture) writeSecretVolume(t *testing.T, root string, mode os.FileMode) {
	t.Helper()
	stamp := "..2026_08_15_09_41_07.4184283913"
	data := filepath.Join(root, stamp)
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, contents := range fixture.keys {
		if err := os.WriteFile(filepath.Join(data, name), contents, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(data, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(stamp, filepath.Join(root, "..data")); err != nil {
		t.Fatal(err)
	}
	for name := range fixture.keys {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
}

// deployment is one temporary stand-in for the pod: a Secret volume, an empty
// directory that both containers mount, and the environment block that the
// chart renders into the ConfigMap.
type deployment struct {
	source      string
	destination string
	environment map[string]string
}

func newDeployment(t *testing.T, fixture materialFixture, secretMode os.FileMode) deployment {
	t.Helper()
	// EvalSymlinks first: on macOS the temporary directory sits under a
	// symlinked /var, and securefile refuses a path that is not canonical.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, sourceDirectory)
	destination := filepath.Join(root, targetDirectory)
	for _, directory := range []string{source, destination} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fixture.writeSecretVolume(t, source, secretMode)
	return deployment{source: source, destination: destination, environment: runtimeEnvironment(destination)}
}

func (d deployment) lookup(name string) (string, bool) {
	value, found := d.environment[name]
	return value, found
}

func (d deployment) path(name string) string { return filepath.Join(d.destination, name) }

// runtimeEnvironment mirrors elitea-main.runtimeEnv in the Helm chart: every
// file path resolves under one directory, and the names are the ones that
// deploy/scripts/gen-runtime-certs.sh writes.
func runtimeEnvironment(directory string) map[string]string {
	values := map[string]string{
		"ELITEA_RUNTIME_ENABLED":         "true",
		"ELITEA_RUNTIME_COMMAND_STREAM":  "ELITEA_RT_V1_VALIDATE",
		"ELITEA_RUNTIME_MAX_OUTSTANDING": "64",
		// The runtime NATS identity is its own cert-manager Secret, not
		// runtime material; plaintext here keeps this test about the
		// material directory.
		"ELITEA_RUNTIME_NATS_URL":                  "nats://elitea-nats:4222",
		"ELITEA_RUNTIME_SIGNING_KEY_ID":            signingKeyID,
		"ELITEA_RUNTIME_SIGNING_KEY_FILE":          filepath.Join(directory, signingKeyName),
		"ELITEA_RUNTIME_VERIFICATION_KEYRING_FILE": filepath.Join(directory, keyringName),
		"ELITEA_RUNTIME_CONTROL_ADDRESS":           "127.0.0.1:9443",
		"ELITEA_RUNTIME_OUTPUT_ADDRESS":            "127.0.0.1:9444",
		"ELITEA_RUNTIME_CONTENT_ADDRESS":           "127.0.0.1:9445",
	}
	for _, listener := range []string{"control", "output", "content"} {
		prefix := "ELITEA_RUNTIME_" + upper(listener) + "_TLS_"
		values[prefix+"CERT_FILE"] = filepath.Join(directory, listener+"-server.crt")
		values[prefix+"KEY_FILE"] = filepath.Join(directory, listener+"-server.key")
		values[prefix+"CLIENT_CA_FILE"] = filepath.Join(directory, caName)
	}
	return values
}

func upper(text string) string {
	raw := []byte(text)
	for index := range raw {
		if raw[index] >= 'a' && raw[index] <= 'z' {
			raw[index] -= 'a' - 'A'
		}
	}
	return string(raw)
}

func newAuthority(t *testing.T, commonName string) authority {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return authority{certificate: certificate, der: der, privateKey: privateKey}
}

var serialCounter int64 = 100

func issueCertificate(t *testing.T, issuer authority, dnsName string, usage x509.ExtKeyUsage) ([]byte, []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serialCounter++
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serialCounter),
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     []string{dnsName},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer.certificate, publicKey, issuer.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
}

// clientTLSFor is the worker side of mutual TLS: the client certificate that
// the runtime CA signed, and that same CA as the trust root for the listener.
func (fixture materialFixture) clientTLSFor(t *testing.T, serverName string) *tls.Config {
	t.Helper()
	certificate, err := tls.X509KeyPair(fixture.clientChain, fixture.clientKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		RootCAs:      fixture.rootPool(),
		ServerName:   serverName,
		NextProtos:   []string{"h2"},
	}
}

func (fixture materialFixture) rootPool() *x509.CertPool {
	roots := x509.NewCertPool()
	roots.AddCert(fixture.ca.certificate)
	return roots
}

func freeLoopbackAddresses(t *testing.T, count int) []string {
	t.Helper()
	listeners := make([]net.Listener, 0, count)
	addresses := make([]string, 0, count)
	for range count {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, listener)
		addresses = append(addresses, listener.Addr().String())
	}
	for _, listener := range listeners {
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return addresses
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(fmt.Errorf("stat %s: %w", path, err))
	}
	return info
}
