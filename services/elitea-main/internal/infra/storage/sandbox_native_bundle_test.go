package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func nativeFixture(t *testing.T) ([]byte, *NativeSandboxBundle) {
	t.Helper()
	raw, err := os.ReadFile("testdata/native-deno-v2.json")
	require.NoError(t, err)
	var r nativeRecord
	require.NoError(t, json.Unmarshal(raw, &r))
	b, err := ParseNativeSandboxBundle(raw, r.Digest)
	require.NoError(t, err)
	return raw, b
}
func nativeRehash(t *testing.T, v map[string]any) []byte {
	t.Helper()
	delete(v, "digest")
	content, err := sandboxJSON(v)
	require.NoError(t, err)
	hash := sha256.Sum256(content)
	v["digest"] = hex.EncodeToString(hash[:])
	raw, err := sandboxJSON(v)
	require.NoError(t, err)
	return raw
}
func TestNativeBundleConformanceAndPathBoundaries(t *testing.T) {
	raw, b := nativeFixture(t)
	require.Equal(t, raw, b.native)
	require.Equal(t, "native-v2/", b.storagePrefix())
	require.Len(t, b.files, 2)
	for _, f := range b.files {
		require.Equal(t, f.SHA256+".blob", f.Name)
	}
	for name, change := range map[string]func(map[string]any){"unknown": func(v map[string]any) { v["destination"] = "/workspace" }, "language": func(v map[string]any) { v["language"] = "python" }, "platform": func(v map[string]any) { v["platform"].(map[string]any)["os"] = "darwin" }, "path": func(v map[string]any) {
		v["payload"].(map[string]any)["files"].([]any)[0].(map[string]any)["name"] = "../escaped"
	}, "kind": func(v map[string]any) { v["kind"] = "cargo" }, "runtime": func(v map[string]any) { v["payload"].(map[string]any)["runtime"] = "deno-latest" }, "large": func(v map[string]any) {
		v["payload"].(map[string]any)["files"].([]any)[0].(map[string]any)["bytes"] = 33 * 1024 * 1024
	}} {
		t.Run(name, func(t *testing.T) {
			var v map[string]any
			require.NoError(t, json.Unmarshal(raw, &v))
			change(v)
			changed := nativeRehash(t, v)
			var r nativeRecord
			require.NoError(t, json.Unmarshal(changed, &r))
			_, err := ParseNativeSandboxBundle(changed, r.Digest)
			require.ErrorIs(t, err, ErrContentRejected)
		})
	}
	_, err := ParseNativeSandboxBundle(append([]byte(" "), raw...), b.Digest())
	require.ErrorIs(t, err, ErrContentRejected)
	duplicate := bytes.Replace(raw, []byte(`"revision":2`), []byte(`"revision":2,"revision":2`), 1)
	_, err = ParseNativeSandboxBundle(duplicate, b.Digest())
	require.ErrorIs(t, err, ErrContentRejected)
	_, err = ParsePythonSandboxBundle(raw, b.Digest())
	require.ErrorIs(t, err, ErrContentRejected)
}
func TestNativeBundlePublicationIsMetadataLastAndNamespaced(t *testing.T) {
	_, b := nativeFixture(t)
	store, _, scope, _, _ := sandboxTestStore(t)
	ctx := context.Background()
	require.Error(t, store.Publish(ctx, scope, b))
	for _, f := range b.files {
		var content []byte
		if f.Bytes != 0 {
			content = []byte("{\"version\":\"5\"}\n")
		}
		require.NoError(t, store.PutFile(ctx, scope, b, f.Name, bytes.NewReader(content)))
	}
	require.NoError(t, store.Publish(ctx, scope, b))
	opened, err := store.OpenNativeBundle(ctx, scope, b.Digest())
	require.NoError(t, err)
	require.Equal(t, b.native, opened.native)
	_, err = store.OpenBundle(ctx, scope, b.Digest())
	require.Error(t, err)
	require.Error(t, store.PutFile(ctx, scope, b, b.files[0].Name, strings.NewReader("wrong")))
	require.NoError(t, store.FetchFile(ctx, scope, b, b.files[0].Name, io.Discard))
}
func nativeHTTPFixture(t *testing.T) (*sandboxHTTPFixture, *x509.Certificate, *NativeSandboxBundle) {
	t.Helper()
	store, backend, scope, _, _ := sandboxTestStore(t)
	_, bundle := nativeFixture(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	now := time.Unix(1000, 0)
	service, err := NewSandboxBundleContentService(store, sandboxBundleTestKeys(public), []string{"dns:sandbox.test"}, func() time.Time { return now })
	require.NoError(t, err)
	_, leaf := sandboxTestCertificate(t, "sandbox.test")
	root, err := hex.DecodeString(bundle.Digest())
	require.NoError(t, err)
	request, err := hex.DecodeString(bundle.record.Preparation)
	require.NoError(t, err)
	return &sandboxHTTPFixture{store: store, backend: backend, scope: scope, signer: private, service: service, claims: &runtimev1.SandboxJobGrantClaimsV1{Revision: 3, TenantId: "tenant-a", ProjectId: 2, ExecutionId: "exec", ActivationId: "node", RequestDigest: request, SubmitterWorkloadIdentity: "dns:worker.test", Audience: "dns:sandbox.test", Generation: 1, IssuedAtUnixMillis: now.UnixMilli(), ExpiresAtUnixMillis: now.Add(30 * time.Second).UnixMilli(), DependencyBundleSha256: root}}, leaf, bundle
}
func TestNativeWriteGrantAndReadRouteBindMTLSRootAndRequest(t *testing.T) {
	f, leaf, b := nativeHTTPFixture(t)
	call := func(method string, grant string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/"+b.Digest(), bytes.NewReader(body))
		r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf}}, PeerCertificates: []*x509.Certificate{leaf}}
		r.Header.Set(SandboxBundleGrantHeader, grant)
		if body != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		f.service.NativeRoutes().ServeHTTP(w, r)
		return w
	}
	require.NotEqual(t, http.StatusNoContent, call("POST", f.grant(t, nil), b.native).Code)
	for _, file := range b.files {
		var content []byte
		if file.Bytes != 0 {
			content = []byte("{\"version\":\"5\"}\n")
		}
		require.NoError(t, f.store.PutFile(context.Background(), f.scope, b, file.Name, bytes.NewReader(content)))
	}
	require.Equal(t, http.StatusNoContent, call("POST", f.grant(t, nil), b.native).Code)
	require.NotEqual(t, http.StatusNoContent, call("POST", f.grant(t, func(c *runtimev1.SandboxJobGrantClaimsV1) { c.RequestDigest = bytes.Repeat([]byte{7}, 32) }), b.native).Code)
	read := call("GET", f.grant(t, func(c *runtimev1.SandboxJobGrantClaimsV1) { c.RequestDigest = bytes.Repeat([]byte{7}, 32) }), nil)
	require.Equal(t, http.StatusOK, read.Code)
	require.Equal(t, b.native, read.Body.Bytes())
	require.Equal(t, http.StatusForbidden, call("GET", f.grant(t, func(c *runtimev1.SandboxJobGrantClaimsV1) { c.ExpiresAtUnixMillis = 1 }), nil).Code)
	require.Equal(t, http.StatusForbidden, call("GET", f.grant(t, func(c *runtimev1.SandboxJobGrantClaimsV1) { c.DependencyBundleSha256 = bytes.Repeat([]byte{7}, 32) }), nil).Code)
}

func TestNativeUploadRejectsWrongPreparationBeforeStorage(t *testing.T) {
	f, leaf, bundle := nativeHTTPFixture(t)
	file := bundle.files[0]
	require.Zero(t, file.Bytes)
	mediaType, body := sandboxMultipart(t, bundle.native, file.Name, nil, false)
	call := func(grant string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPut, "/"+bundle.Digest()+"/files/"+file.Name, bytes.NewReader(body))
		request.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf}}, PeerCertificates: []*x509.Certificate{leaf}}
		request.Header.Set("Content-Type", mediaType)
		request.Header.Set(SandboxBundleGrantHeader, grant)
		response := httptest.NewRecorder()
		f.service.NativeRoutes().ServeHTTP(response, request)
		return response
	}
	denied := call(f.grant(t, func(claims *runtimev1.SandboxJobGrantClaimsV1) {
		claims.RequestDigest = bytes.Repeat([]byte{7}, 32)
	}))
	require.Equal(t, http.StatusForbidden, denied.Code)
	require.Contains(t, denied.Body.String(), "grant is invalid")
	require.NotContains(t, denied.Body.String(), "Retry")
	require.Empty(t, f.backend.objects)

	accepted := call(f.grant(t, nil))
	require.Equal(t, http.StatusNoContent, accepted.Code)
	require.Len(t, f.backend.objects, 1)
}

func TestNativeCargoFixtureKeepsRecordArchiveOrderAndBounds(t *testing.T) {
	raw, err := os.ReadFile("testdata/native-cargo-v2.json")
	require.NoError(t, err)
	var record nativeRecord
	require.NoError(t, json.Unmarshal(raw, &record))
	bundle, err := ParseNativeSandboxBundle(raw, record.Digest)
	require.NoError(t, err)
	require.Len(t, bundle.files, 2)
	var payload struct{ Objects []struct{ Name string } }
	require.NoError(t, json.Unmarshal(record.Payload, &payload))
	require.Equal(t, payload.Objects[0].Name, bundle.files[0].Name)
	require.Equal(t, payload.Objects[1].Name, bundle.files[1].Name)
	var value map[string]any
	require.NoError(t, json.Unmarshal(raw, &value))
	objects := value["payload"].(map[string]any)["objects"].([]any)
	objects[0].(map[string]any)["bytes"] = 8*1024*1024 + 1
	changed := nativeRehash(t, value)
	require.NoError(t, json.Unmarshal(changed, &record))
	_, err = ParseNativeSandboxBundle(changed, record.Digest)
	require.ErrorIs(t, err, ErrContentRejected)
}
