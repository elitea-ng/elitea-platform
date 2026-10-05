package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"
)

// This fixture substitutes the sealed-read owner. Signature, role, and mTLS
// verification remain the owning signer's separate tests.
type workspaceTestReadConsumer struct {
	visit    OriginalCodeVisit
	job      CodePreparedRequest
	calls    int
	refuseAt int
	active   bool
}

func (c *workspaceTestReadConsumer) WithCodeWorkspaceRead(ctx context.Context, authority CodeWorkspaceReadAuthority, prepared []byte, apply func(context.Context, CodeTransaction, OriginalCodeVisit, CodePreparedRequest) error) error {
	c.calls++
	if _, ok := authority.View(); !ok || c.refuseAt > 0 && c.calls == c.refuseAt || len(prepared) == 0 {
		return ErrContentUnauthorized
	}
	if c.active {
		panic("nested read transaction")
	}
	c.active = true
	defer func() { c.active = false }()
	return apply(ctx, &workspaceTestTransaction{}, c.visit, c.job)
}
func workspaceReadHarness(t *testing.T) (workspaceTestHarness, *CodeWorkspaceManifest, *workspaceTestReadConsumer, *CodeWorkspaceReadServer, []byte, CodeWorkspaceReadAuthority) {
	t.Helper()
	h := newWorkspaceTestHarness(t)
	manifest, err := h.service.Resolve(context.Background(), h.claim, h.request)
	if err != nil {
		t.Fatal(err)
	}
	base, err := decodeCodeWorkspacePrepared(h.request.SelectedPreparedBase64URL)
	if err != nil {
		t.Fatal(err)
	}
	var wire codePreparedWire
	if err = codePreparedDecode(base, &wire); err != nil {
		t.Fatal(err)
	}
	policy, err := manifest.Policy().SHA256()
	if err != nil {
		t.Fatal(err)
	}
	wire.Revision = 4
	wire.Workspace, err = sandboxJSON(CodeWorkspaceBinding{1, manifest.Selection(), manifest.Digest(), policy})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := codePreparedBytes(wire)
	if err != nil {
		t.Fatal(err)
	}
	job, err := ParseCodePreparedRequest(prepared)
	if err != nil {
		t.Fatal(err)
	}
	c := &workspaceTestReadConsumer{visit: h.authorizer.visit, job: job}
	s := &CodeWorkspaceReadServer{visits: c, workspaces: h.service}
	// A test-only sealed token is never issued by a production handler.
	authority := CodeWorkspaceReadAuthority{valid: true}
	return h, manifest, c, s, prepared, authority
}
func TestCodeWorkspaceContentReadIsOutsideLocksAndRequiresFreshFence(t *testing.T) {
	for _, metadata := range []bool{true, false} {
		for _, takeover := range []bool{false, true} {
			h, manifest, c, server, prepared, authority := workspaceReadHarness(t)
			h.objects.onRead = func() {
				if c.active || h.authorizer.active {
					t.Fatal("binary read holds current-writer locks")
				}
				if takeover {
					c.refuseAt = 2
				}
			}
			payload, err := server.read(context.Background(), authority, prepared, manifest.Digest(), workspaceFilesFixture()[0].SHA256, metadata)
			if takeover {
				if !errors.Is(err, ErrContentUnauthorized) || payload != nil {
					t.Fatal("read published bytes after fence loss")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				expected := []byte("hello")
				if metadata {
					expected = manifest.Bytes()
				}
				if !bytes.Equal(payload, expected) {
					t.Fatal("content read changed immutable bytes")
				}
			}
			if c.calls != 2 {
				t.Fatal("read did not recheck the current fence")
			}
		}
	}
}
func TestCodeWorkspaceContentReadInvalidReceiptFailsBeforeObjectIO(t *testing.T) {
	for _, kind := range []string{"missing_receipt", "root", "policy", "selection", "unsealed", "first_fence"} {
		t.Run(kind, func(t *testing.T) {
			h, manifest, c, server, prepared, authority := workspaceReadHarness(t)
			root := manifest.Digest()
			switch kind {
			case "missing_receipt":
				h.receipts.root = ""
			case "root":
				root = strings.Repeat("e", 64)
			case "policy":
				c.job.Workspace.PolicySHA256 = strings.Repeat("f", 64)
			case "selection":
				c.job.Workspace.Selection.Commit = strings.Repeat("f", 40)
			case "unsealed":
				authority = CodeWorkspaceReadAuthority{}
			case "first_fence":
				c.refuseAt = 1
			}
			reads := h.objects.reads
			payload, err := server.read(context.Background(), authority, prepared, root, "", true)
			if err == nil || payload != nil || h.objects.reads != reads {
				t.Fatal("invalid proof crossed the object-read boundary")
			}
		})
	}
}
func TestCodeWorkspaceContentRequestPreservesExactSignedBytesAndRefusesAmbiguousBody(t *testing.T) {
	grant := &runtimev1.SignedSandboxJobGrantV1{KeyId: "fixture", ClaimsBytes: []byte("signed opaque bytes"), Signature: []byte("signature")}
	grantBytes, err := proto.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	intent := base64.RawURLEncoding.EncodeToString([]byte("exact signed intent"))
	for _, selectedIntent := range []*string{nil, &intent} {
		request := codeWorkspaceReadRequest{"elitea.sandbox.workspace-content-read.v1", base64.RawURLEncoding.EncodeToString(grantBytes), base64.RawURLEncoding.EncodeToString([]byte("exact prepared bytes")), selectedIntent}
		raw, err := sandboxJSON(request)
		if err != nil {
			t.Fatal(err)
		}
		got, prepared, gotIntent, err := parseCodeWorkspaceReadRequest(raw)
		if err != nil || !proto.Equal(got, grant) || !bytes.Equal(prepared, []byte("exact prepared bytes")) || (selectedIntent == nil) != (gotIntent == nil) {
			t.Fatal("transport changed signed input")
		}
		for _, changed := range [][]byte{
			append(append([]byte{}, raw...), []byte(" ")...),
			bytes.Replace(raw, []byte(`"schema":`), []byte(`"schema":"duplicate","schema":`), 1),
			bytes.Replace(raw, []byte(`"grant_base64url":`), []byte(`"unexpected":true,"grant_base64url":`), 1),
			bytes.Replace(raw, []byte(request.Grant), []byte(request.Grant+"="), 1),
		} {
			if _, _, _, err := parseCodeWorkspaceReadRequest(changed); err == nil {
				t.Fatal("accepted ambiguous or noncanonical read authority")
			}
		}
	}
}
