package storage

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
	"io"
	"log"
	"math/big"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type sandboxBundleTestKeys ed25519.PublicKey

func (keys sandboxBundleTestKeys) ResolveEd25519PublicKey(_ context.Context, id string) (ed25519.PublicKey, error) {
	if id != "key-1" {
		return nil, ErrContentUnauthorized
	}
	return ed25519.PublicKey(keys), nil
}

type sandboxHTTPFixture struct {
	store   *SandboxBundleStore
	backend *sandboxObjectFake
	scope   SandboxBundleScope
	bundle  *PythonSandboxBundle
	files   map[string][]byte
	service *SandboxBundleContentService
	server  *httptest.Server
	client  *http.Client
	wrong   *http.Client
	signer  ed25519.PrivateKey
	claims  *runtimev1.SandboxJobGrantClaimsV1
}

func sandboxTestCertificate(t *testing.T, name string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true}
	native, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	require.NoError(t, err)
	parsed, err := x509.ParseCertificate(native)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{native}, PrivateKey: private}, parsed
}

func sandboxNewHTTPFixture(t *testing.T) *sandboxHTTPFixture {
	t.Helper()
	store, backend, scope, bundle, files := sandboxTestStore(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	now := time.Unix(1000, 0)
	service, err := NewSandboxBundleContentService(store, sandboxBundleTestKeys(public), []string{"dns:sandbox.test", "dns:other.test"}, func() time.Time { return now })
	require.NoError(t, err)
	content, err := NewContentServer(contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
		t.Error("bundle request reached execution input authorizer")
		return ContentAuthorization{}, ErrContentUnauthorized
	}), contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
		t.Error("bundle request reached execution input storage")
		return nil, ErrContentUnauthorized
	}), 1024)
	require.NoError(t, err)
	absent := httptest.NewRecorder()
	content.Routes().ServeHTTP(absent, httptest.NewRequest(http.MethodGet, "/sandbox-bundles/"+bundle.Digest(), nil))
	require.Equal(t, http.StatusNotFound, absent.Code)
	content.WithSandboxBundles(service)
	cert, leaf := sandboxTestCertificate(t, "sandbox.test")
	wrongCert, wrongLeaf := sandboxTestCertificate(t, "other.test")
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	pool.AddCert(wrongLeaf)
	server := httptest.NewUnstartedServer(content.Routes())
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	server.StartTLS()
	t.Cleanup(server.Close)
	client := func(cert tls.Certificate) *http.Client {
		transport := server.Client().Transport.(*http.Transport).Clone()
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		transport.TLSClientConfig.Certificates = []tls.Certificate{cert}
		t.Cleanup(transport.CloseIdleConnections)
		return &http.Client{Transport: transport, Timeout: 5 * time.Second}
	}
	root, err := hex.DecodeString(bundle.Digest())
	require.NoError(t, err)
	return &sandboxHTTPFixture{store: store, backend: backend, scope: scope, bundle: bundle, files: files, service: service,
		server: server, client: client(cert), wrong: client(wrongCert), signer: private,
		claims: &runtimev1.SandboxJobGrantClaimsV1{Revision: 3, TenantId: "tenant-a", ProjectId: 2,
			ExecutionId: "execution-1", ActivationId: "node/activation-1", RequestDigest: bytes.Repeat([]byte{3}, 32),
			SubmitterWorkloadIdentity: "dns:worker.test", Audience: "dns:sandbox.test", Generation: 1,
			IssuedAtUnixMillis: now.UnixMilli(), ExpiresAtUnixMillis: now.Add(30 * time.Second).UnixMilli(), DependencyBundleSha256: root}}
}

func (fixture *sandboxHTTPFixture) grant(t *testing.T, change func(*runtimev1.SandboxJobGrantClaimsV1)) string {
	t.Helper()
	claims := proto.Clone(fixture.claims).(*runtimev1.SandboxJobGrantClaimsV1)
	if change != nil {
		change(claims)
	}
	native, err := proto.MarshalOptions{Deterministic: true}.Marshal(claims)
	require.NoError(t, err)
	input := append([]byte(sandboxBundleGrantDomain), make([]byte, 8)...)
	binary.BigEndian.PutUint64(input[len(sandboxBundleGrantDomain):], uint64(len(native)))
	input = append(input, native...)
	envelope, err := proto.Marshal(&runtimev1.SignedSandboxJobGrantV1{KeyId: "key-1", ClaimsBytes: native, Signature: ed25519.Sign(fixture.signer, input)})
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(envelope)
}

func (fixture *sandboxHTTPFixture) request(t *testing.T, client *http.Client, method, suffix, mediaType, grant string, body []byte) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, fixture.server.URL+"/sandbox-bundles/"+fixture.bundle.Digest()+suffix, bytes.NewReader(body))
	require.NoError(t, err)
	if mediaType != "" {
		req.Header.Set("Content-Type", mediaType)
	}
	req.Header.Set(SandboxBundleGrantHeader, grant)
	// Caller headers cannot select a project or worker identity.
	req.Header.Set("X-Project-Id", "999")
	req.Header.Set("X-Workload-Identity", "dns:sandbox.test")
	res, err := client.Do(req)
	require.NoError(t, err)
	native, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	return res.StatusCode, res.Header, native
}

func sandboxMultipart(t *testing.T, metadata []byte, name string, data []byte, extra bool) (string, []byte) {
	t.Helper()
	var output bytes.Buffer
	writer := multipart.NewWriter(&output)
	part, err := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="bundle"`}, "Content-Type": {"application/json"}})
	require.NoError(t, err)
	_, err = part.Write(metadata)
	require.NoError(t, err)
	part, err = writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="file"; filename="` + name + `"`}, "Content-Type": {"application/octet-stream"}})
	require.NoError(t, err)
	_, err = part.Write(data)
	require.NoError(t, err)
	if extra {
		_, err = writer.CreateFormField("extra")
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return writer.FormDataContentType(), output.Bytes()
}

func TestSandboxBundleHTTPVerifiedMTLSTransfer(t *testing.T) {
	f := sandboxNewHTTPFixture(t)
	grant := f.grant(t, nil)
	status, _, _ := f.request(t, f.client, http.MethodPost, "", "application/json", grant, f.bundle.native)
	require.Equal(t, http.StatusNotFound, status)
	for _, name := range f.bundle.FileNames() {
		media, native := sandboxMultipart(t, f.bundle.native, name, f.files[name], false)
		status, _, _ := f.request(t, f.client, http.MethodPut, "/files/"+name, media, grant, native)
		require.Equal(t, http.StatusNoContent, status)
	}
	status, _, _ = f.request(t, f.client, http.MethodPost, "", "application/json", grant, f.bundle.native)
	require.Equal(t, http.StatusNoContent, status)
	status, headers, native := f.request(t, f.client, http.MethodGet, "", "", grant, nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, f.bundle.native, native)
	require.Contains(t, headers.Get("Cache-Control"), "no-store")
	for _, name := range f.bundle.FileNames() {
		status, headers, native = f.request(t, f.client, http.MethodGet, "/files/"+name, "", grant, nil)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, f.files[name], native)
		require.NotEmpty(t, headers.Get("Content-Digest"))
	}
	other := f.grant(t, func(c *runtimev1.SandboxJobGrantClaimsV1) { c.ProjectId = 3 })
	status, _, _ = f.request(t, f.client, http.MethodGet, "", "", other, nil)
	require.Equal(t, http.StatusNotFound, status)
	for ref, content := range f.backend.objects {
		if strings.HasSuffix(ref.Key(), "z-test.whl") {
			f.backend.objects[ref] = bytes.Repeat([]byte{'x'}, len(content))
		}
	}
	status, _, native = f.request(t, f.client, http.MethodGet, "/files/z-test.whl", "", grant, nil)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.NotContains(t, string(native), "xxxx")
}

func TestSandboxBundleHTTPRejectsWrongAuthorityBeforeStorage(t *testing.T) {
	f := sandboxNewHTTPFixture(t)
	for name, change := range map[string]func(*runtimev1.SandboxJobGrantClaimsV1){
		"execute":   func(c *runtimev1.SandboxJobGrantClaimsV1) { c.Revision = 1 },
		"cancel":    func(c *runtimev1.SandboxJobGrantClaimsV1) { c.Revision = 2; c.CancelOnly = true },
		"expired":   func(c *runtimev1.SandboxJobGrantClaimsV1) { c.ExpiresAtUnixMillis = 1000000 },
		"future":    func(c *runtimev1.SandboxJobGrantClaimsV1) { c.IssuedAtUnixMillis++ },
		"long_life": func(c *runtimev1.SandboxJobGrantClaimsV1) { c.ExpiresAtUnixMillis++ },
		"root":      func(c *runtimev1.SandboxJobGrantClaimsV1) { c.DependencyBundleSha256[0] ^= 1 },
		"recipient": func(c *runtimev1.SandboxJobGrantClaimsV1) { c.Audience = "dns:other.test" },
		"project":   func(c *runtimev1.SandboxJobGrantClaimsV1) { c.ProjectId = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			status, _, _ := f.request(t, f.client, http.MethodGet, "", "", f.grant(t, change), nil)
			require.Equal(t, http.StatusForbidden, status)
		})
	}
	grant := f.grant(t, nil)
	status, _, _ := f.request(t, f.wrong, http.MethodGet, "", "", grant, nil)
	require.Equal(t, http.StatusForbidden, status)
	native, err := base64.StdEncoding.DecodeString(grant)
	require.NoError(t, err)
	native[len(native)-1] ^= 1
	status, _, _ = f.request(t, f.client, http.MethodGet, "", "", base64.StdEncoding.EncodeToString(native), nil)
	require.Equal(t, http.StatusForbidden, status)
	req, err := http.NewRequest(http.MethodGet, f.server.URL+"/sandbox-bundles/"+f.bundle.Digest(), nil)
	require.NoError(t, err)
	req.Header.Set(SandboxBundleGrantHeader, grant)
	res, err := f.server.Client().Do(req) // Valid server trust, no client certificate.
	if res != nil {
		_ = res.Body.Close()
	}
	require.Error(t, err)
	require.Empty(t, f.backend.puts)
}

func TestSandboxBundleHTTPMalformedTransferCannotPublish(t *testing.T) {
	f := sandboxNewHTTPFixture(t)
	grant := f.grant(t, nil)
	for _, name := range []string{"wrong_bytes", "wrong_metadata", "extra_part"} {
		t.Run(name, func(t *testing.T) {
			metadata, content := f.bundle.native, f.files["z-test.whl"]
			if name == "wrong_bytes" {
				content = []byte("bad")
			}
			if name == "wrong_metadata" {
				metadata = []byte(`{}`)
			}
			media, native := sandboxMultipart(t, metadata, "z-test.whl", content, name == "extra_part")
			status, _, _ := f.request(t, f.client, http.MethodPut, "/files/z-test.whl", media, grant, native)
			require.Equal(t, http.StatusUnprocessableEntity, status)
			status, _, _ = f.request(t, f.client, http.MethodGet, "", "", grant, nil)
			require.Equal(t, http.StatusNotFound, status)
		})
	}
	// Capacity is independent of body size and returns before any object access.
	for i := 0; i < cap(f.service.requests); i++ {
		f.service.requests <- struct{}{}
	}
	status, _, _ := f.request(t, f.client, http.MethodGet, "", "", grant, nil)
	require.Equal(t, http.StatusServiceUnavailable, status)
	for i := 0; i < cap(f.service.requests); i++ {
		<-f.service.requests
	}
}
