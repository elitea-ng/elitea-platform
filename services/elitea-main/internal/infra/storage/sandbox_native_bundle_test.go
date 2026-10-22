package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
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

const nativeBundle = runtimev1.SandboxBundleKindV1_SANDBOX_BUNDLE_KIND_V1_NATIVE

func nativeRPCFixture(t *testing.T) (*sandboxRPCFixture, *ContentServer, context.Context, *NativeSandboxBundle) {
	t.Helper()
	store, backend, scope, _, _ := sandboxTestStore(t)
	_, bundle := nativeFixture(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	now := time.Unix(1000, 0)
	service, err := NewSandboxBundleContentService(store, sandboxBundleTestKeys(public), []string{"dns:sandbox.test"}, func() time.Time { return now })
	require.NoError(t, err)
	root, err := hex.DecodeString(bundle.Digest())
	require.NoError(t, err)
	request, err := hex.DecodeString(bundle.record.Preparation)
	require.NoError(t, err)
	fixture := &sandboxRPCFixture{store: store, backend: backend, scope: scope, signer: private, service: service, claims: &runtimev1.SandboxJobGrantClaimsV1{Revision: 3, TenantId: "tenant-a", ProjectId: 2, ExecutionId: "exec", ActivationId: "node", RequestDigest: request, SubmitterWorkloadIdentity: "dns:worker.test", Audience: "dns:sandbox.test", Generation: 1, IssuedAtUnixMillis: now.UnixMilli(), ExpiresAtUnixMillis: now.Add(30 * time.Second).UnixMilli(), DependencyBundleSha256: root}}
	return fixture, (&ContentServer{}).WithSandboxBundles(service), contentPeerContext(context.Background(), sandboxPeer(t, "sandbox.test")), bundle
}

func TestNativeWriteGrantAndReadBindMTLSRootAndRequest(t *testing.T) {
	f, content, ctx, b := nativeRPCFixture(t)
	publish := func(grant []byte) error {
		_, err := content.PublishSandboxBundle(ctx, &runtimev1.PublishSandboxBundleRequestV1{Kind: nativeBundle, Root: b.Digest(), SignedGrant: grant, BundleJson: b.native})
		return err
	}
	get := func(grant []byte) ([]byte, error) {
		response, err := content.GetSandboxBundle(ctx, &runtimev1.GetSandboxBundleRequestV1{Kind: nativeBundle, Root: b.Digest(), SignedGrant: grant})
		return response.GetBundleJson(), err
	}
	require.Error(t, publish(f.grant(t, nil)))
	for _, file := range b.files {
		var data []byte
		if file.Bytes != 0 {
			data = []byte("{\"version\":\"5\"}\n")
		}
		require.NoError(t, f.store.PutFile(context.Background(), f.scope, b, file.Name, bytes.NewReader(data)))
	}
	require.NoError(t, publish(f.grant(t, nil)))
	requireContentCode(t, publish(f.grant(t, func(c *runtimev1.SandboxJobGrantClaimsV1) { c.RequestDigest = bytes.Repeat([]byte{7}, 32) })), codes.PermissionDenied)
	read, err := get(f.grant(t, func(c *runtimev1.SandboxJobGrantClaimsV1) { c.RequestDigest = bytes.Repeat([]byte{7}, 32) }))
	require.NoError(t, err)
	require.Equal(t, b.native, read)
	_, err = get(f.grant(t, func(c *runtimev1.SandboxJobGrantClaimsV1) { c.ExpiresAtUnixMillis = 1 }))
	requireContentCode(t, err, codes.PermissionDenied)
	_, err = get(f.grant(t, func(c *runtimev1.SandboxJobGrantClaimsV1) { c.DependencyBundleSha256 = bytes.Repeat([]byte{7}, 32) }))
	requireContentCode(t, err, codes.PermissionDenied)
	// The Python kind never reads a native root.
	_, err = content.GetSandboxBundle(ctx, &runtimev1.GetSandboxBundleRequestV1{Kind: pythonBundle, Root: b.Digest(), SignedGrant: f.grant(t, nil)})
	require.Error(t, err)
}

func TestNativeUploadRejectsWrongPreparationBeforeStorage(t *testing.T) {
	f, content, ctx, bundle := nativeRPCFixture(t)
	file := bundle.files[0]
	require.Zero(t, file.Bytes)
	call := func(grant []byte) error {
		stream := newScriptedClientStream[runtimev1.UploadSandboxBundleFileRequestV1, runtimev1.UploadSandboxBundleFileResponseV1](ctx,
			&runtimev1.UploadSandboxBundleFileRequestV1{Part: &runtimev1.UploadSandboxBundleFileRequestV1_Header{Header: &runtimev1.UploadSandboxBundleFileHeaderV1{Kind: nativeBundle, Root: bundle.Digest(), Name: file.Name, SignedGrant: grant, BundleJson: bundle.native}}})
		return content.UploadSandboxBundleFile(stream)
	}
	denied := call(f.grant(t, func(claims *runtimev1.SandboxJobGrantClaimsV1) {
		claims.RequestDigest = bytes.Repeat([]byte{7}, 32)
	}))
	requireContentCode(t, denied, codes.PermissionDenied)
	require.Contains(t, denied.Error(), "grant is invalid")
	require.NotContains(t, denied.Error(), "Retry")
	require.Empty(t, f.backend.objects)

	require.NoError(t, call(f.grant(t, nil)))
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

// A bundle another toolchain prepared is served, so the worker can see it is
// stale and prepare again; only a malformed release or a broken digest is
// rejected as tampered content.
func TestNativeCargoBundleFromAnotherToolchainIsStaleNotRejected(t *testing.T) {
	raw, err := os.ReadFile("testdata/native-cargo-v2.json")
	require.NoError(t, err)
	withRust := func(t *testing.T, rust string, rehash bool) ([]byte, string) {
		t.Helper()
		var v map[string]any
		require.NoError(t, json.Unmarshal(raw, &v))
		v["payload"].(map[string]any)["profile"].(map[string]any)["rust_revision"] = rust
		if !rehash {
			changed, err := sandboxJSON(v)
			require.NoError(t, err)
			return changed, v["digest"].(string)
		}
		changed := nativeRehash(t, v)
		return changed, v["digest"].(string)
	}
	for _, rust := range []string{"1.97.1", "1.100.0", "2.0.0"} {
		t.Run(rust, func(t *testing.T) {
			changed, root := withRust(t, rust, true)
			_, err := ParseNativeSandboxBundle(changed, root)
			require.NoError(t, err)
			store, backend, scope, _, _ := sandboxTestStore(t)
			ref, err := NewPlatformObjectRef(sandboxBundleBucket, scope.key+"/native-v2/"+root+"/elitea-native-bundle-v2.json")
			require.NoError(t, err)
			backend.objects[ref] = changed
			opened, err := store.OpenNativeBundle(context.Background(), scope, root)
			require.NoError(t, err)
			require.Equal(t, changed, opened.native)
		})
	}
	for _, rust := range []string{"", "1.99", "1.99.1-nightly", "01.99.1", "1..1", "latest"} {
		t.Run("malformed "+rust, func(t *testing.T) {
			changed, root := withRust(t, rust, true)
			_, err := ParseNativeSandboxBundle(changed, root)
			require.ErrorIs(t, err, ErrContentRejected)
		})
	}
	changed, root := withRust(t, "1.97.1", false)
	_, err = ParseNativeSandboxBundle(changed, root)
	require.ErrorIs(t, err, ErrContentRejected, "the digest still covers rust_revision")
}
