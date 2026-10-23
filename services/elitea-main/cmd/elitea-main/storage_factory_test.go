package main

import (
	"context"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage/azure"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage/gcs"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage/s3"
)

// unwrapObjectStore undoes storage.Instrument's wrapping (S18) so a test can
// assert something about the concrete backend newObjectStore actually
// dispatched to, through the instrumentation layer every returned store now
// carries.
func unwrapObjectStore(t *testing.T, store storage.ObjectStore) storage.ObjectStore {
	t.Helper()
	u, ok := store.(interface{ Unwrap() storage.ObjectStore })
	if !ok {
		t.Fatalf("newObjectStore returned %T, want something implementing Unwrap() (storage.Instrument-wrapped, S18)", store)
	}
	return u.Unwrap()
}

func TestArtifactNewObjectStoreDispatchesPerBackend(t *testing.T) {
	ctx := context.Background()

	t.Run("s3", func(t *testing.T) {
		store, err := newObjectStore(ctx, storage.Config{
			Backend: "s3",
			S3:      storage.S3Config{Region: "us-east-1", Bucket: "b"},
		})
		if err != nil {
			t.Fatalf("newObjectStore(s3) returned %v, want nil", err)
		}
		if _, ok := unwrapObjectStore(t, store).(*s3.Backend); !ok {
			t.Fatalf("newObjectStore(s3) unwrapped to %T, want *s3.Backend", store)
		}
	})

	t.Run("azure", func(t *testing.T) {
		// New() takes the shared-key path (Key set), so this needs no real
		// network reachability — it never dials out at construction time.
		store, err := newObjectStore(ctx, storage.Config{
			Backend: "azure",
			Azure:   storage.AzureConfig{Account: "devstoreaccount1", Key: "a2V5", ContainerName: "b"},
		})
		if err != nil {
			t.Fatalf("newObjectStore(azure) returned %v, want nil", err)
		}
		if _, ok := unwrapObjectStore(t, store).(*azure.Backend); !ok {
			t.Fatalf("newObjectStore(azure) unwrapped to %T, want *azure.Backend", store)
		}
	})

	t.Run("gcs", func(t *testing.T) {
		// gcs.New dials for live GCP default credentials at construction
		// time unless Endpoint/WithoutAuthentication is set — S2's support
		// for that landed before S3, which this stage depends on, so this
		// asserts a concrete type rather than tolerating a credential error.
		store, err := newObjectStore(ctx, storage.Config{
			Backend: "gcs",
			GCS:     storage.GCSConfig{Endpoint: "http://127.0.0.1:4443/storage/v1/", Bucket: "b"},
		})
		if err != nil {
			t.Fatalf("newObjectStore(gcs) returned %v, want nil", err)
		}
		if _, ok := unwrapObjectStore(t, store).(*gcs.Backend); !ok {
			t.Fatalf("newObjectStore(gcs) unwrapped to %T, want *gcs.Backend", store)
		}
	})

	t.Run("unrecognised backend", func(t *testing.T) {
		if _, err := newObjectStore(ctx, storage.Config{Backend: "filesystem"}); err == nil {
			t.Fatal("newObjectStore(filesystem) returned nil error, want an error")
		}
	})
}

// TestArtifactNewObjectStoreIsInstrumented is a regression test for S18:
// every store this factory returns must carry storage.Instrument's
// wrapping, not just the s3-specific case unwrapObjectStore's helper
// happens to exercise above — this is what actually makes "instrument every
// ObjectStore method" true for the one and only production construction
// path (cmd/elitea-main/main.go), not just true for the storage package's
// own direct unit tests.
func TestArtifactNewObjectStoreIsInstrumented(t *testing.T) {
	store, err := newObjectStore(context.Background(), storage.Config{
		Backend: "s3",
		S3:      storage.S3Config{Region: "us-east-1", Bucket: "b"},
	})
	if err != nil {
		t.Fatalf("newObjectStore: %v", err)
	}
	if _, ok := store.(interface{ Unwrap() storage.ObjectStore }); !ok {
		t.Fatalf("newObjectStore returned %T, want an storage.Instrument-wrapped store (S18)", store)
	}
}
