package repos

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

func TestArtifactRetentionNotificationRepositoryResolvesProjectOwner(t *testing.T) {
	queries := &artifactRetentionNotificationQueriesStub{ownerID: 42}
	repository, err := newArtifactRetentionNotificationRepository(queries)
	if err != nil {
		t.Fatal(err)
	}
	ownerID, err := repository.ProjectOwnerUserID(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if ownerID != 42 {
		t.Fatalf("ownerID = %d, want 42", ownerID)
	}
	if queries.getOwnerCalls != 1 || queries.getOwnerArg != 7 {
		t.Fatalf("calls=%d arg=%d", queries.getOwnerCalls, queries.getOwnerArg)
	}
}

func TestArtifactRetentionNotificationRepositoryWrapsOwnerLookupFailure(t *testing.T) {
	queries := &artifactRetentionNotificationQueriesStub{err: errors.New("no such project")}
	repository, err := newArtifactRetentionNotificationRepository(queries)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ProjectOwnerUserID(context.Background(), 7); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestArtifactRetentionNotificationRepositoryInsertsExpectedShape(t *testing.T) {
	queries := &artifactRetentionNotificationQueriesStub{rows: 1}
	repository, err := newArtifactRetentionNotificationRepository(queries)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	if err := repository.NotifyBucketExpiring(context.Background(), 7, 42, 99, expiresAt); err != nil {
		t.Fatal(err)
	}
	if queries.insertCalls != 1 || queries.insertArg.ProjectID != 7 || queries.insertArg.UserID != 42 {
		t.Fatalf("calls=%d arg=%+v", queries.insertCalls, queries.insertArg)
	}
	var metadata map[string]any
	if err := json.Unmarshal(queries.insertArg.Meta, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["bucket_id"] != float64(99) || metadata["expires_at"] != "2026-08-05T00:00:00Z" {
		t.Fatalf("metadata = %#v", metadata)
	}
}

// TestArtifactRetentionNotificationUUIDStableWithinCycleDistinctAcrossCycles
// proves the fix for the deterministic-UUID collision the naive
// bucketID-only derivation would have: the same (bucketID, expiresAt) pair
// (a retried tick within one notification cycle) must hash to the same
// UUID, letting ON CONFLICT (uuid) DO NOTHING absorb the retry — but a
// bucket renotified under a NEW expires_at (after
// UpdateArtifactBucketRetention resets notified_at) must get a different
// UUID, or that second, real notification would silently collide with and
// be dropped by the first.
func TestArtifactRetentionNotificationUUIDStableWithinCycleDistinctAcrossCycles(t *testing.T) {
	first := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	second := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)

	retry := artifactBucketExpiryNotificationUUID(99, first)
	sameCycle := artifactBucketExpiryNotificationUUID(99, first)
	if retry != sameCycle {
		t.Fatalf("same (bucketID, expiresAt) produced different UUIDs: %q vs %q", retry, sameCycle)
	}

	nextCycle := artifactBucketExpiryNotificationUUID(99, second)
	if nextCycle == retry {
		t.Fatalf("a bucket renotified under a new expires_at must get a distinct UUID, got the same: %q", nextCycle)
	}

	differentBucket := artifactBucketExpiryNotificationUUID(100, first)
	if differentBucket == retry {
		t.Fatalf("a different bucket must get a distinct UUID, got the same: %q", differentBucket)
	}

	if len(retry) != 36 || retry[14] != '4' {
		t.Fatalf("notification UUID is not UUIDv4-shaped: %q", retry)
	}
	switch retry[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Fatalf("notification UUID has an invalid RFC 4122 variant: %q", retry)
	}
}

type artifactRetentionNotificationQueriesStub struct {
	getOwnerCalls int
	getOwnerArg   int32
	ownerID       int32

	insertCalls int
	insertArg   sqlcgen.InsertArtifactBucketExpiryNotificationParams
	rows        int64

	err error
}

func (stub *artifactRetentionNotificationQueriesStub) GetArtifactBucketOwningProjectUserID(
	_ context.Context,
	id int32,
) (int32, error) {
	stub.getOwnerCalls++
	stub.getOwnerArg = id
	return stub.ownerID, stub.err
}

func (stub *artifactRetentionNotificationQueriesStub) InsertArtifactBucketExpiryNotification(
	_ context.Context,
	arg sqlcgen.InsertArtifactBucketExpiryNotificationParams,
) (int64, error) {
	stub.insertCalls++
	stub.insertArg = arg
	return stub.rows, stub.err
}
