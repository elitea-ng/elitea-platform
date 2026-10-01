package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// This live case consumes native preparer output. It needs an S3 emulator and
// a real private bundle fixture. It does not prove supervisor or UI recovery.
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
	for _, name := range bundle.FileNames() {
		file, err := os.Open(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		err = producer.PutFile(ctx, scope, bundle, name, file)
		closed := file.Close()
		if err != nil {
			t.Fatal(err)
		}
		if closed != nil {
			t.Fatal(closed)
		}
	}
	if err := producer.Publish(ctx, scope, bundle); err != nil {
		t.Fatal(err)
	}
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
	loaded, err := replacement.OpenBundle(ctx, scope, root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Digest() != root {
		t.Fatal("replacement returned a different root")
	}
	for _, name := range loaded.FileNames() {
		var content bytes.Buffer
		if err := replacement.FetchFile(ctx, scope, loaded, name, &content); err != nil {
			t.Fatal(err)
		}
		original, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(original, content.Bytes()) {
			t.Fatal("shared package bytes differ")
		}
		if output := os.Getenv("SANDBOX_BUNDLE_OUTPUT"); output != "" {
			if err := os.WriteFile(filepath.Join(output, name), content.Bytes(), 0o600); err != nil {
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
	t.Logf("Verified %d native bundle files after replacement", len(loaded.FileNames()))
}

type bundleRecordingStore struct {
	storage.ObjectStore
	written []storage.ObjectRef
}

func (store *bundleRecordingStore) Put(ctx context.Context, ref storage.ObjectRef, body io.Reader, options storage.PutOptions) (storage.ObjectInfo, error) {
	store.written = append(store.written, ref)
	return store.ObjectStore.Put(ctx, ref, body, options)
}
