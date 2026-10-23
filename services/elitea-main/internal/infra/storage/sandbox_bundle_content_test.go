package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

type sandboxBundleTestKeys ed25519.PublicKey

func (keys sandboxBundleTestKeys) ResolveEd25519PublicKey(_ context.Context, id string) (ed25519.PublicKey, error) {
	if id != "key-1" {
		return nil, ErrContentUnauthorized
	}
	return ed25519.PublicKey(keys), nil
}

type sandboxRPCFixture struct {
	store     *SandboxBundleStore
	backend   *sandboxObjectFake
	scope     SandboxBundleScope
	bundle    *PythonSandboxBundle
	files     map[string][]byte
	service   *SandboxBundleContentService
	client    runtimev1.RuntimeContentServiceClient
	wrong     runtimev1.RuntimeContentServiceClient
	anonymous runtimev1.RuntimeContentServiceClient
	signer    ed25519.PrivateKey
	claims    *runtimev1.SandboxJobGrantClaimsV1
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

const pythonBundle = runtimev1.SandboxBundleKindV1_SANDBOX_BUNDLE_KIND_V1_PYTHON

func sandboxNewRPCFixture(t *testing.T) *sandboxRPCFixture {
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
	_, err = content.GetSandboxBundle(context.Background(), &runtimev1.GetSandboxBundleRequestV1{Kind: pythonBundle, Root: bundle.Digest()})
	requireContentCode(t, err, codes.Unimplemented)
	content.WithSandboxBundles(service)
	pki, dial := startContentGRPC(t, content)
	root, err := hex.DecodeString(bundle.Digest())
	require.NoError(t, err)
	return &sandboxRPCFixture{store: store, backend: backend, scope: scope, bundle: bundle, files: files, service: service,
		client: dial(pki.ClientTLS(t, "sandbox.test")), wrong: dial(pki.ClientTLS(t, "other.test")), anonymous: dial(pki.AnonymousTLS()),
		signer: private,
		claims: &runtimev1.SandboxJobGrantClaimsV1{Revision: 3, TenantId: "tenant-a", ProjectId: 2,
			ExecutionId: "execution-1", ActivationId: "node/activation-1", RequestDigest: bytes.Repeat([]byte{3}, 32),
			SubmitterWorkloadIdentity: "dns:worker.test", Audience: "dns:sandbox.test", Generation: 1,
			IssuedAtUnixMillis: now.UnixMilli(), ExpiresAtUnixMillis: now.Add(30 * time.Second).UnixMilli(), DependencyBundleSha256: root}}
}

func (fixture *sandboxRPCFixture) grant(t *testing.T, change func(*runtimev1.SandboxJobGrantClaimsV1)) []byte {
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
	return envelope
}

func sandboxGet(client runtimev1.RuntimeContentServiceClient, kind runtimev1.SandboxBundleKindV1, root string, grant []byte) ([]byte, error) {
	response, err := client.GetSandboxBundle(context.Background(), &runtimev1.GetSandboxBundleRequestV1{Kind: kind, Root: root, SignedGrant: grant})
	return response.GetBundleJson(), err
}

func sandboxPublish(client runtimev1.RuntimeContentServiceClient, kind runtimev1.SandboxBundleKindV1, root string, grant, metadata []byte) error {
	_, err := client.PublishSandboxBundle(context.Background(), &runtimev1.PublishSandboxBundleRequestV1{Kind: kind, Root: root, SignedGrant: grant, BundleJson: metadata})
	return err
}

// sandboxUpload streams one file; trailing appends data after it.
func sandboxUpload(t *testing.T, client runtimev1.RuntimeContentServiceClient, kind runtimev1.SandboxBundleKindV1, root string, grant, metadata []byte, name string, data, trailing []byte) error {
	t.Helper()
	stream, err := client.UploadSandboxBundleFile(context.Background())
	if err != nil {
		return err
	}
	if err := stream.Send(&runtimev1.UploadSandboxBundleFileRequestV1{Part: &runtimev1.UploadSandboxBundleFileRequestV1_Header{Header: &runtimev1.UploadSandboxBundleFileHeaderV1{Kind: kind, Root: root, Name: name, SignedGrant: grant, BundleJson: metadata}}}); err != nil {
		_, err = stream.CloseAndRecv()
		return err
	}
	for _, chunk := range splitContentChunks(append(append([]byte(nil), data...), trailing...)) {
		if err := stream.Send(&runtimev1.UploadSandboxBundleFileRequestV1{Part: &runtimev1.UploadSandboxBundleFileRequestV1_Chunk{Chunk: chunk}}); err != nil {
			break
		}
	}
	_, err = stream.CloseAndRecv()
	return err
}

func sandboxFetch(t *testing.T, client runtimev1.RuntimeContentServiceClient, kind runtimev1.SandboxBundleKindV1, root string, grant []byte, name string) (*runtimev1.SandboxContentFileHeaderV1, []byte, error) {
	t.Helper()
	stream, err := client.FetchSandboxBundleFile(context.Background(), &runtimev1.FetchSandboxBundleFileRequestV1{Kind: kind, Root: root, Name: name, SignedGrant: grant})
	require.NoError(t, err)
	var header *runtimev1.SandboxContentFileHeaderV1
	var data []byte
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return header, data, nil
		}
		if err != nil {
			return header, data, err
		}
		if message.GetHeader() != nil {
			require.Nil(t, header, "second header")
			header = message.GetHeader()
			continue
		}
		require.NotNil(t, header, "chunk before header")
		data = append(data, message.GetChunk()...)
	}
}

func TestSandboxBundleRPCVerifiedMTLSTransfer(t *testing.T) {
	f := sandboxNewRPCFixture(t)
	grant := f.grant(t, nil)
	root := f.bundle.Digest()
	requireContentCode(t, sandboxPublish(f.client, pythonBundle, root, grant, f.bundle.native), codes.NotFound)
	for _, name := range f.bundle.FileNames() {
		require.NoError(t, sandboxUpload(t, f.client, pythonBundle, root, grant, f.bundle.native, name, f.files[name], nil))
	}
	require.NoError(t, sandboxPublish(f.client, pythonBundle, root, grant, f.bundle.native))
	native, err := sandboxGet(f.client, pythonBundle, root, grant)
	require.NoError(t, err)
	require.Equal(t, f.bundle.native, native)
	for _, name := range f.bundle.FileNames() {
		header, data, err := sandboxFetch(t, f.client, pythonBundle, root, grant, name)
		require.NoError(t, err)
		require.Equal(t, f.files[name], data)
		digest := sha256.Sum256(f.files[name])
		require.Equal(t, digest[:], header.GetSha256())
		require.EqualValues(t, len(f.files[name]), header.GetLength())
	}
	// The native kind is a separate namespace: the Python root is not there.
	_, err = sandboxGet(f.client, runtimev1.SandboxBundleKindV1_SANDBOX_BUNDLE_KIND_V1_NATIVE, root, grant)
	require.Error(t, err)
	_, err = sandboxGet(f.client, runtimev1.SandboxBundleKindV1_SANDBOX_BUNDLE_KIND_V1_UNSPECIFIED, root, grant)
	requireContentCode(t, err, codes.FailedPrecondition)
	other := f.grant(t, func(c *runtimev1.SandboxJobGrantClaimsV1) { c.ProjectId = 3 })
	_, err = sandboxGet(f.client, pythonBundle, root, other)
	requireContentCode(t, err, codes.NotFound)
	for ref, content := range f.backend.objects {
		if strings.HasSuffix(ref.Key(), "z-test.whl") {
			f.backend.objects[ref] = bytes.Repeat([]byte{'x'}, len(content))
		}
	}
	header, data, err := sandboxFetch(t, f.client, pythonBundle, root, grant, "z-test.whl")
	requireContentCode(t, err, codes.FailedPrecondition)
	require.Nil(t, header, "corrupt storage must not yield a header")
	require.Empty(t, data)
	require.NotContains(t, err.Error(), "xxxx")
}

func TestSandboxBundleRPCRejectsWrongAuthorityBeforeStorage(t *testing.T) {
	f := sandboxNewRPCFixture(t)
	root := f.bundle.Digest()
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
			_, err := sandboxGet(f.client, pythonBundle, root, f.grant(t, change))
			requireContentCode(t, err, codes.PermissionDenied)
		})
	}
	grant := f.grant(t, nil)
	// A verified peer that is not the grant's recipient.
	_, err := sandboxGet(f.wrong, pythonBundle, root, grant)
	requireContentCode(t, err, codes.PermissionDenied)
	tampered := append([]byte(nil), grant...)
	tampered[len(tampered)-1] ^= 1
	_, err = sandboxGet(f.client, pythonBundle, root, tampered)
	requireContentCode(t, err, codes.PermissionDenied)
	_, err = sandboxGet(f.client, pythonBundle, root, nil)
	requireContentCode(t, err, codes.PermissionDenied)
	// Valid server trust, no client certificate: the handshake fails.
	_, err = sandboxGet(f.anonymous, pythonBundle, root, grant)
	requireContentCode(t, err, codes.Unavailable)
	require.Error(t, sandboxUpload(t, f.anonymous, pythonBundle, root, grant, f.bundle.native, "z-test.whl", f.files["z-test.whl"], nil))
	require.Empty(t, f.backend.puts)
}

func TestSandboxBundleRPCMalformedTransferCannotPublish(t *testing.T) {
	f := sandboxNewRPCFixture(t)
	grant := f.grant(t, nil)
	root := f.bundle.Digest()
	for _, name := range []string{"wrong_bytes", "wrong_metadata", "trailing_data"} {
		t.Run(name, func(t *testing.T) {
			metadata, content := f.bundle.native, f.files["z-test.whl"]
			var trailing []byte
			switch name {
			case "wrong_bytes":
				content = []byte("bad")
			case "wrong_metadata":
				metadata = []byte(`{}`)
			case "trailing_data":
				trailing = []byte("x")
			}
			requireContentCode(t, sandboxUpload(t, f.client, pythonBundle, root, grant, metadata, "z-test.whl", content, trailing), codes.FailedPrecondition)
			_, err := sandboxGet(f.client, pythonBundle, root, grant)
			requireContentCode(t, err, codes.NotFound)
		})
	}
	// A chunk before the header is refused.
	stream, err := f.client.UploadSandboxBundleFile(context.Background())
	require.NoError(t, err)
	require.NoError(t, stream.Send(&runtimev1.UploadSandboxBundleFileRequestV1{Part: &runtimev1.UploadSandboxBundleFileRequestV1_Chunk{Chunk: []byte("x")}}))
	_, err = stream.CloseAndRecv()
	requireContentCode(t, err, codes.FailedPrecondition)
	// Capacity is independent of body size and returns before any object access.
	for i := 0; i < cap(f.service.requests); i++ {
		f.service.requests <- struct{}{}
	}
	_, err = sandboxGet(f.client, pythonBundle, root, grant)
	requireContentCode(t, err, codes.Unavailable)
	for i := 0; i < cap(f.service.requests); i++ {
		<-f.service.requests
	}
}

func TestSandboxBundleRPCFetchStopsWhenTheCallerCancels(t *testing.T) {
	f := sandboxNewRPCFixture(t)
	grant := f.grant(t, nil)
	root := f.bundle.Digest()
	for _, name := range f.bundle.FileNames() {
		require.NoError(t, sandboxUpload(t, f.client, pythonBundle, root, grant, f.bundle.native, name, f.files[name], nil))
	}
	require.NoError(t, sandboxPublish(f.client, pythonBundle, root, grant, f.bundle.native))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	content := (&ContentServer{}).WithSandboxBundles(f.service)
	stream := newRecordingServerStream[runtimev1.FetchSandboxBundleFileResponseV1](contentPeerContext(ctx, sandboxPeer(t, "sandbox.test")))
	err := content.FetchSandboxBundleFile(&runtimev1.FetchSandboxBundleFileRequestV1{Kind: pythonBundle, Root: root, Name: "z-test.whl", SignedGrant: grant}, stream)
	requireContentCode(t, err, codes.DeadlineExceeded)
	require.Empty(t, stream.sent)
	require.Len(t, f.service.requests, 0, "a cancelled transfer must release its slot")
}

func sandboxPeer(t *testing.T, name string) *x509.Certificate {
	t.Helper()
	_, leaf := sandboxTestCertificate(t, name)
	return leaf
}
