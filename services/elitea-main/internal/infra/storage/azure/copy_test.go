package azure

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

type fakeToken struct {
	mu     sync.Mutex
	scopes []string
}

func (f *fakeToken) GetToken(_ context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopes = options.Scopes
	return azcore.AccessToken{Token: "entra-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// Under workload identity the copy is Copy Blob From URL with the source
// authorized by a Bearer token for the storage scope: the bare URL of a
// private blob is refused (403 CannotVerifyCopySource).
func TestCopyAuthorizesTheSourceUnderWorkloadIdentity(t *testing.T) {
	var mu sync.Mutex
	var copySource, sourceAuthorization, method string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", "7")
			w.WriteHeader(http.StatusOK)
		case http.MethodPut:
			mu.Lock()
			method = r.Method
			copySource = r.Header.Get("x-ms-copy-source")
			sourceAuthorization = r.Header.Get("x-ms-copy-source-authorization")
			mu.Unlock()
			w.Header().Set("x-ms-copy-status", "success")
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	token := &fakeToken{}
	client, err := azblob.NewClient(server.URL+"/", token, &azblob.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: server.Client()},
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &Backend{client: client, containerName: "c", token: token}
	src, _ := storage.NewObjectRef("7", "elitea-workspaces", "t/seg/a.seg.upload-1")
	dst, _ := storage.NewObjectRef("7", "elitea-workspaces", "t/seg/a.seg")
	if _, err := storage.CopyObject(context.Background(), storage.Instrument(backend, "azure"), src, dst); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	decoded, _ := url.PathUnescape(copySource)
	if method != http.MethodPut || !strings.HasSuffix(decoded, "/c/"+src.StorageKey("")) {
		t.Fatalf("copy request = %s, source %q", method, copySource)
	}
	if sourceAuthorization != "Bearer entra-token" {
		t.Fatalf("x-ms-copy-source-authorization = %q, want the Bearer token", sourceAuthorization)
	}
	token.mu.Lock()
	defer token.mu.Unlock()
	if len(token.scopes) == 0 || token.scopes[0] != storageScope {
		t.Fatalf("token scopes = %v", token.scopes)
	}
}

// A refused copy source degrades to the caller's read-back fallback
// (ErrNotSupported) instead of failing the promote outright.
func TestCopyErrorMapsARefusedSourceToNotSupported(t *testing.T) {
	refused := &azcore.ResponseError{StatusCode: http.StatusForbidden, ErrorCode: "CannotVerifyCopySource"}
	if err := copyError(refused); !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("CannotVerifyCopySource = %v, want ErrNotSupported", err)
	}
	forbidden := &azcore.ResponseError{StatusCode: http.StatusForbidden, ErrorCode: "SomethingElse"}
	if err := copyError(forbidden); !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("403 = %v, want ErrNotSupported", err)
	}
	other := &azcore.ResponseError{StatusCode: http.StatusInternalServerError, ErrorCode: "InternalError"}
	if err := copyError(other); errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("a 500 = %v, want an error, not the fallback", err)
	}
}
