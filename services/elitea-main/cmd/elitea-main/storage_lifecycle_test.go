package main

import (
	"context"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

type lifecycleBackend struct {
	storage.ObjectStore
	calls int
}

func (b *lifecycleBackend) ConfigureRetentionLifecycle(context.Context) error {
	b.calls++
	return nil
}

// The startup step installs the backend's lifecycle rule through the
// instrumented wrapper newObjectStore always returns.
func TestConfigureObjectStoreRetentionLifecycleReachesTheWrappedBackend(t *testing.T) {
	backend := &lifecycleBackend{}
	if err := configureObjectStoreRetentionLifecycle(context.Background(), storage.Instrument(backend, "s3")); err != nil {
		t.Fatal(err)
	}
	if backend.calls != 1 {
		t.Fatalf("the lifecycle rule was configured %d times through the wrapper, want 1", backend.calls)
	}
}
