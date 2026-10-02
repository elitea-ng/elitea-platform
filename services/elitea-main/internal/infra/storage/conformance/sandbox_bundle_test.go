package conformance

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"google.golang.org/protobuf/proto"
)

// This live case transfers native preparer output over verified mTLS to S3.
// It does not prove active database claims, supervisor dispatch, or UI recovery.
func TestPythonSandboxBundleSharedStore(t *testing.T) {
	directory, root := os.Getenv("SANDBOX_BUNDLE_FIXTURE"), os.Getenv("SANDBOX_BUNDLE_ROOT")
	if directory == "" || root == "" {
		t.Skip("SANDBOX_BUNDLE_FIXTURE and SANDBOX_BUNDLE_ROOT are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	backend := &bundleRecordingStore{ObjectStore: setupS3(t, ctx)}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, ref := range backend.written {
			if err := backend.Delete(cleanup, ref); err != nil {
				t.Errorf("delete test bundle object: %v", err)
			}
		}
	})
	native, err := os.ReadFile(filepath.Join(directory, "elitea-python-bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := storage.ParsePythonSandboxBundle(native, root)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	tenant := fmt.Sprintf("sandbox-conformance-%x", nonce)
	scope, err := storage.NewSandboxBundleScope(tenant, 999000001)
	if err != nil {
		t.Fatal(err)
	}
	spool := t.TempDir()
	if err := os.Chmod(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	producer, err := storage.NewSandboxBundleStore(backend, spool, 2)
	if err != nil {
		t.Fatal(err)
	}
	transfer := newBundleTransfer(t, producer, tenant, root)
	for _, name := range bundle.FileNames() {
		file, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="bundle"`}, "Content-Type": {"application/json"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(native); err != nil {
			t.Fatal(err)
		}
		part, err = writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="file"; filename="` + name + `"`}, "Content-Type": {"application/octet-stream"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(file); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		transfer.request(t, http.MethodPut, "/files/"+name, writer.FormDataContentType(), body.Bytes(), http.StatusNoContent)
	}
	transfer.request(t, http.MethodPost, "", "application/json", native, http.StatusNoContent)
	transfer.server.Close()
	if err := os.Remove(spool); err != nil {
		t.Fatal(err)
	}
	// Replacement has a new staging path and uses only shared object storage.
	newSpool := t.TempDir()
	if err := os.Chmod(newSpool, 0o700); err != nil {
		t.Fatal(err)
	}
	replacement, err := storage.NewSandboxBundleStore(backend, newSpool, 2)
	if err != nil {
		t.Fatal(err)
	}
	transfer = newBundleTransfer(t, replacement, tenant, root)
	metadata := transfer.request(t, http.MethodGet, "", "", nil, http.StatusOK)
	if !bytes.Equal(metadata, native) {
		t.Fatal("shared metadata differs")
	}
	loaded, err := replacement.OpenBundle(ctx, scope, root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Digest() != root {
		t.Fatal("replacement returned a different root")
	}
	for _, name := range loaded.FileNames() {
		content := transfer.request(t, http.MethodGet, "/files/"+name, "", nil, http.StatusOK)
		original, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(original, content) {
			t.Fatal("shared package bytes differ")
		}
		if output := os.Getenv("SANDBOX_BUNDLE_OUTPUT"); output != "" {
			if err := os.WriteFile(filepath.Join(output, name), content, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if output := os.Getenv("SANDBOX_BUNDLE_OUTPUT"); output != "" {
		if err := os.WriteFile(filepath.Join(output, "elitea-python-bundle.json"), native, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	other, err := storage.NewSandboxBundleScope(tenant, 999000002)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.OpenBundle(ctx, other, root); err == nil {
		t.Fatal("another project read the bundle")
	}
	if binary := os.Getenv("SANDBOX_BUNDLE_RUST_TEST_BINARY"); binary != "" {
		transfer.runRustClient(t, ctx, binary, directory)
	}
	t.Logf("Verified %d native bundle files over mTLS after replacement", len(loaded.FileNames()))
}

type bundleTransferKeys ed25519.PublicKey

func (key bundleTransferKeys) ResolveEd25519PublicKey(_ context.Context, id string) (ed25519.PublicKey, error) {
	if id != "fixture-key" {
		return nil, storage.ErrContentUnauthorized
	}
	return ed25519.PublicKey(key), nil
}

type bundleTransfer struct {
	server *httptest.Server
	client *http.Client
	grant  string
	root   string
	ca     []byte
}

func newBundleTransfer(t *testing.T, store *storage.SandboxBundleStore, tenant, root string) *bundleTransfer {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	service, err := storage.NewSandboxBundleContentService(store, bundleTransferKeys(public), []string{"dns:sandbox.test"}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true,
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true}
	ca, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, public, private)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(ca)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	clientPublic, clientPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"sandbox.test"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true}
	clientCert, err := x509.CreateCertificate(rand.Reader, clientTemplate, caCert, clientPublic, private)
	if err != nil {
		t.Fatal(err)
	}
	serverPublic, serverPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{SerialNumber: big.NewInt(3), DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		NotBefore:   now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	serverCert, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, serverPublic, private)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(service.Routes())
	mux := http.NewServeMux()
	mux.Handle("/sandbox-bundles/", http.StripPrefix("/sandbox-bundles", service.Routes()))
	server.Config.Handler = mux
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool,
		Certificates: []tls.Certificate{{Certificate: [][]byte{serverCert}, PrivateKey: serverPrivate}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	client := server.Client()
	transport := client.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.RootCAs = pool
	transport.TLSClientConfig.Certificates = []tls.Certificate{{Certificate: [][]byte{clientCert}, PrivateKey: clientPrivate}}
	client = &http.Client{Transport: transport, Timeout: 15 * time.Second}
	t.Cleanup(transport.CloseIdleConnections)
	digest, err := hex.DecodeString(root)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := proto.Marshal(&runtimev1.SandboxJobGrantClaimsV1{Revision: 3, TenantId: tenant, ProjectId: 999000001,
		ExecutionId: "fixture-execution", ActivationId: "fixture-preparation", RequestDigest: bytes.Repeat([]byte{1}, 32),
		SubmitterWorkloadIdentity: "dns:fixture-worker", Audience: "dns:sandbox.test", Generation: 1,
		IssuedAtUnixMillis: now.UnixMilli(), ExpiresAtUnixMillis: now.Add(30 * time.Second).UnixMilli(), DependencyBundleSha256: digest})
	if err != nil {
		t.Fatal(err)
	}
	const domain = "elitea.sandbox.job-grant.ed25519.v1\x00"
	input := append([]byte(domain), make([]byte, 8)...)
	binary.BigEndian.PutUint64(input[len(domain):], uint64(len(claims)))
	input = append(input, claims...)
	envelope, err := proto.Marshal(&runtimev1.SignedSandboxJobGrantV1{KeyId: "fixture-key", ClaimsBytes: claims, Signature: ed25519.Sign(private, input)})
	if err != nil {
		t.Fatal(err)
	}
	return &bundleTransfer{server: server, client: client, grant: base64.StdEncoding.EncodeToString(envelope), root: root, ca: ca}
}

func (transfer *bundleTransfer) runRustClient(t *testing.T, ctx context.Context, binary, source string) {
	t.Helper()
	material := t.TempDir()
	if err := os.Chmod(material, 0o700); err != nil {
		t.Fatal(err)
	}
	transport := transfer.client.Transport.(*http.Transport)
	client := transport.TLSClientConfig.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(client.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	combined := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: client.Certificate[0]}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
	envelope, err := base64.StdEncoding.DecodeString(transfer.grant)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"client-combined.pem": combined,
		"ca.pem":              pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: transfer.ca}),
		"grant.pb":            envelope,
	} {
		if err := os.WriteFile(filepath.Join(material, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.CommandContext(ctx, binary, "sandbox::dependency_content::tests::live_native_mtls_download_and_publication", "--exact", "--ignored", "--nocapture")
	command.Env = append(os.Environ(), "ELITEA_TEST_BUNDLE_ORIGIN="+transfer.server.URL,
		"ELITEA_TEST_BUNDLE_TLS="+material, "ELITEA_TEST_BUNDLE_SOURCE="+source)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Rust package client failed: %v\n%s", err, output)
	}
	t.Logf("Rust client verifies native download, publication, authority rejection, and staging cleanup: %s", output)
}

func (transfer *bundleTransfer) request(t *testing.T, method, suffix, mediaType string, body []byte, expected int) []byte {
	t.Helper()
	request, err := http.NewRequest(method, transfer.server.URL+"/sandbox-bundles/"+transfer.root+suffix, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(storage.SandboxBundleGrantHeader, transfer.grant)
	if mediaType != "" {
		request.Header.Set("Content-Type", mediaType)
	}
	response, err := transfer.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	native, err := io.ReadAll(response.Body)
	closed := response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closed != nil {
		t.Fatal(closed)
	}
	if response.StatusCode != expected {
		t.Fatalf("bundle transfer status %d; expected %d", response.StatusCode, expected)
	}
	return native
}

type bundleRecordingStore struct {
	storage.ObjectStore
	written []storage.ObjectRef
}

func (store *bundleRecordingStore) Put(ctx context.Context, ref storage.ObjectRef, body io.Reader, options storage.PutOptions) (storage.ObjectInfo, error) {
	store.written = append(store.written, ref)
	return store.ObjectStore.Put(ctx, ref, body, options)
}
