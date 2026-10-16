package repos

// The workspace store (ADR-0034 decision 2) against a REAL database
// (ELITEA_TEST_DATABASE_URL): the upload grant, the publish transaction's
// fences (state, epoch, writer allowlist, compare-and-set, idempotent replay),
// the object check before the transaction, the fetch plan, concurrency and the
// object sweeper. The object store is in memory; the rules live in PostgreSQL.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore/workspacestoretest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

func ptr(value string) *string { return &value }

func desktopPublish(thread workspacestore.Thread, owner, epoch int64, sha string, bytes int64, refs ...workspacestore.RefUpdate) workspacestore.PublishRequest {
	return workspacestore.PublishRequest{
		Thread: thread, Caller: workspacestore.Caller{Writer: workspacestore.WriterDesktop, UserID: owner},
		Epoch: epoch, Kind: workspacestore.KindCompact, SHA256: sha, Bytes: bytes,
		Prerequisites: []string{}, Refs: refs, ObjectFormat: workspacestore.ObjectFormatSHA1,
	}
}

func TestPostgresWorkspaceStoreUploadGrant(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5101
	fixture := seedLocalWorkThread(t, pool, owner, "local_work")
	seedHandoffRow(t, pool, fixture.thread, owner, workspacestore.StateHandingOff, 1, false, "")
	desktop := workspacestore.Caller{Writer: workspacestore.WriterDesktop, UserID: owner}
	sha := digestOf([]byte("bundle"))

	t.Run("presigned PUT for an exact key", func(t *testing.T) {
		service, _ := newWorkspaceStoreService(t, pool, workspacestoretest.New(true), defaultWorkspaceLimits())
		grant, err := service.RequestUpload(ctx, workspacestore.UploadRequest{Thread: fixture.thread, Caller: desktop, Epoch: 1, SHA256: sha, Bytes: 6})
		if err != nil {
			t.Fatal(err)
		}
		wantKey := "1/" + storage.WorkspaceBucketName + "/t/" + fixture.thread.ConversationUUID + "/seg/" + sha + ".seg"
		if grant.Exists || !strings.HasSuffix(grant.PutURL, wantKey) || grant.Multipart != nil ||
			grant.Headers["Content-Type"] != workspacestore.MediaType || grant.ExpiresAt.After(time.Now().Add(15*time.Minute+time.Second)) {
			t.Fatalf("grant = %+v, want a presigned PUT for %s valid at most 15 minutes", grant, wantKey)
		}
		var rows int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_storage.transfer_grants g JOIN elitea_storage.buckets b ON b.id = g.bucket_id
WHERE b.bucket_type = 'workspace' AND b.name = $1 AND g.key = $2 AND g.max_bytes = 6 AND g.method = 'PUT'`,
			storage.WorkspaceBucketName, workspacestore.ObjectKey(fixture.thread.ConversationUUID, sha)).Scan(&rows); err != nil || rows != 1 {
			t.Fatalf("grant rows = %d, %v; want one transfer grant on the workspace bucket", rows, err)
		}
	})
	t.Run("streaming facade without presign", func(t *testing.T) {
		service, _ := newWorkspaceStoreService(t, pool, workspacestoretest.New(false), defaultWorkspaceLimits())
		grant, err := service.RequestUpload(ctx, workspacestore.UploadRequest{Thread: fixture.thread, Caller: desktop, Epoch: 1, SHA256: sha, Bytes: 6})
		if err != nil {
			t.Fatal(err)
		}
		want := workspacestore.ObjectRoutePrefix + "1/" + fixture.thread.ConversationUUID + "/" + sha
		if grant.PutURL != want {
			t.Fatalf("facade URL = %q, want %q", grant.PutURL, want)
		}
	})
	t.Run("multipart above 64 MiB", func(t *testing.T) {
		store := workspacestoretest.New(true)
		store.Multipart = true
		service, _ := newWorkspaceStoreService(t, pool, store, defaultWorkspaceLimits())
		grant, err := service.RequestUpload(ctx, workspacestore.UploadRequest{Thread: fixture.thread, Caller: desktop, Epoch: 1, SHA256: digestOf([]byte("big")), Bytes: 130 << 20})
		if err != nil {
			t.Fatal(err)
		}
		if grant.Multipart == nil || len(grant.Multipart.PartURLs) != 3 || grant.PutURL != "" ||
			!strings.HasPrefix(grant.Multipart.CompleteURL, workspacestore.UploadCompleteRoutePrefix+"1/") {
			t.Fatalf("grant = %+v, want 3 parts and a completion URL", grant)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		limits := defaultWorkspaceLimits()
		limits.MaxSegmentBytes = 100
		limits.MaxProjectWorkspaceBytes = 50
		service, _ := newWorkspaceStoreService(t, pool, workspacestoretest.New(true), limits)
		for _, tc := range []struct {
			name    string
			request workspacestore.UploadRequest
			want    error
		}{
			{"stale epoch", workspacestore.UploadRequest{Thread: fixture.thread, Caller: desktop, Epoch: 2, SHA256: sha, Bytes: 6}, workspacestore.ErrStaleEpoch},
			{"too large", workspacestore.UploadRequest{Thread: fixture.thread, Caller: desktop, Epoch: 1, SHA256: sha, Bytes: 101}, workspacestore.ErrSegmentTooLarge},
			{"quota", workspacestore.UploadRequest{Thread: fixture.thread, Caller: desktop, Epoch: 1, SHA256: sha, Bytes: 60}, workspacestore.ErrQuotaExceeded},
			{"not the owner", workspacestore.UploadRequest{Thread: fixture.thread, Caller: workspacestore.Caller{Writer: workspacestore.WriterDesktop, UserID: owner + 1}, Epoch: 1, SHA256: sha, Bytes: 6}, workspacestore.ErrNotFound},
			{"the executor outside CLOUD_RUNNING", workspacestore.UploadRequest{Thread: fixture.thread, Caller: workspacestore.Caller{Writer: workspacestore.WriterCloudExecutor, ExecutionID: "e"}, Epoch: 1, SHA256: sha, Bytes: 6}, workspacestore.ErrStaleEpoch},
		} {
			if _, err := service.RequestUpload(ctx, tc.request); !errors.Is(err, tc.want) {
				t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
			}
		}
	})
}

func TestPostgresWorkspaceStorePublishFencesAndReplay(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5111
	fixture := seedLocalWorkThread(t, pool, owner, "local_work")
	thread := fixture.thread
	seedHandoffRow(t, pool, thread, owner, workspacestore.StateHandingOff, 1, false, "")
	store := workspacestoretest.New(true)
	service, recorder := newWorkspaceStoreService(t, pool, store, defaultWorkspaceLimits())

	content := []byte("the first snapshot bundle")
	sha := digestOf(content)
	base, snapshot := sha1Of("base"), sha1Of("snapshot")
	refs := []workspacestore.RefUpdate{
		{Name: workspacestore.RefBase, New: base}, {Name: workspacestore.RefHandoff1, New: snapshot},
	}

	// The object check runs BEFORE the transaction: nothing stored yet.
	if _, err := service.Publish(ctx, desktopPublish(thread, owner, 1, sha, int64(len(content)), refs...)); !errors.Is(err, workspacestore.ErrSegmentMissing) {
		t.Fatalf("publish before upload = %v, want ErrSegmentMissing", err)
	}
	// Wrong bytes at the key: checksum mismatch, and the object is deleted.
	forged := uploadSegment(t, store, thread, []byte("other bytes"), time.Now())
	ref, _ := storage.NewObjectRef("1", storage.WorkspaceBucketName, workspacestore.ObjectKey(thread.ConversationUUID, sha))
	store.Seed(ref, []byte("tampered bytes not the declared ones!!"), time.Now())
	if _, err := service.Publish(ctx, desktopPublish(thread, owner, 1, sha, int64(len("tampered bytes not the declared ones!!")), refs...)); !errors.Is(err, workspacestore.ErrChecksumMismatch) {
		t.Fatalf("tampered publish = %v, want ErrChecksumMismatch", err)
	}
	if store.Has(ref) {
		t.Fatalf("a segment whose bytes do not match its address stayed stored")
	}
	_ = forged
	uploadSegment(t, store, thread, content, time.Now())
	if _, err := service.Publish(ctx, desktopPublish(thread, owner, 1, sha, int64(len(content))+1, refs...)); !errors.Is(err, workspacestore.ErrChecksumMismatch) {
		t.Fatalf("a wrong declared size = %v, want ErrChecksumMismatch", err)
	}

	sha256Format := desktopPublish(thread, owner, 1, sha, int64(len(content)), refs...)
	sha256Format.ObjectFormat = "sha256"
	for _, tc := range []struct {
		name    string
		request workspacestore.PublishRequest
		want    error
	}{
		{"sha256 repository", sha256Format, workspacestore.ErrUnsupportedObjectFormat},
		{"stale epoch", desktopPublish(thread, owner, 2, sha, int64(len(content)), refs...), workspacestore.ErrStaleEpoch},
		{"the cloud ref from the desktop", desktopPublish(thread, owner, 1, sha, int64(len(content)),
			workspacestore.RefUpdate{Name: workspacestore.RefCloud, New: base}), workspacestore.ErrRefNotAllowed},
		{"upstream without an upstream", desktopPublish(thread, owner, 1, sha, int64(len(content)),
			workspacestore.RefUpdate{Name: workspacestore.RefUpstream, New: base}), workspacestore.ErrRefNotAllowed},
		{"another user's thread", desktopPublish(thread, owner+1, 1, sha, int64(len(content)), refs...), workspacestore.ErrNotFound},
		{"an expected old value on an absent ref", desktopPublish(thread, owner, 1, sha, int64(len(content)),
			workspacestore.RefUpdate{Name: workspacestore.RefBase, Old: ptr(base), New: base},
			workspacestore.RefUpdate{Name: workspacestore.RefHandoff1, New: snapshot}), workspacestore.ErrRefConflict},
	} {
		if _, err := service.Publish(ctx, tc.request); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	// The failed CAS moved NOTHING (all or nothing).
	var refCount int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.workspace_refs WHERE conversation_uuid = $1::uuid`, thread.ConversationUUID).Scan(&refCount)
	if refCount != 0 {
		t.Fatalf("a refused publish left %d refs", refCount)
	}

	first, err := service.Publish(ctx, desktopPublish(thread, owner, 1, sha, int64(len(content)), refs...))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if first.Seq != 1 || first.Replayed || first.Refs[workspacestore.RefBase] != base || first.Refs[workspacestore.RefHandoff1] != snapshot {
		t.Fatalf("publish = %+v", first)
	}
	// An identical retry answers the original result, not a conflict.
	replay, err := service.Publish(ctx, desktopPublish(thread, owner, 1, sha, int64(len(content)), refs...))
	if err != nil || !replay.Replayed || replay.Seq != 1 {
		t.Fatalf("replay = %+v, %v; want seq 1 replayed", replay, err)
	}
	// The same refs with a stale expectation conflict and name the current values.
	second := []byte("the second snapshot bundle")
	secondSHA := uploadSegment(t, store, thread, second, time.Now())
	_, err = service.Publish(ctx, desktopPublish(thread, owner, 1, secondSHA, int64(len(second)),
		workspacestore.RefUpdate{Name: workspacestore.RefHandoff1, New: sha1Of("second")}))
	var conflict *workspacestore.RefConflictError
	if !errors.As(err, &conflict) || conflict.Current[workspacestore.RefHandoff1] == nil ||
		*conflict.Current[workspacestore.RefHandoff1] != snapshot {
		t.Fatalf("stale CAS = %v (%+v), want a conflict naming %s", err, conflict, snapshot)
	}
	advanced, err := service.Publish(ctx, desktopPublish(thread, owner, 1, secondSHA, int64(len(second)),
		workspacestore.RefUpdate{Name: workspacestore.RefHandoff1, Old: ptr(snapshot), New: sha1Of("second")}))
	if err != nil || advanced.Seq != 2 || len(advanced.Superseded) != 1 || advanced.Superseded[0] != 1 ||
		advanced.Refs[workspacestore.RefBase] != base {
		t.Fatalf("second publish = %+v, %v; want seq 2 superseding 1 and keeping base", advanced, err)
	}
	var cachedHandoff, consumed string
	_ = pool.QueryRow(ctx, `SELECT handoff_sha FROM elitea_runtime.workspace_handoffs WHERE conversation_uuid = $1::uuid`,
		thread.ConversationUUID).Scan(&cachedHandoff)
	if cachedHandoff != sha1Of("second") {
		t.Fatalf("handoff row sha cache = %s", cachedHandoff)
	}
	_ = consumed

	// The fetch plan: the newest COMPACT only.
	plan, err := service.Plan(ctx, thread, workspacestore.Caller{Writer: workspacestore.WriterDesktop, UserID: owner}, 0)
	if err != nil || plan.HeadSeq != 2 || len(plan.Segments) != 1 || plan.Segments[0].Seq != 2 ||
		plan.Segments[0].SHA256 != secondSHA || !strings.HasPrefix(plan.Segments[0].URL, "https://store.test/get/") {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if plan, err := service.Plan(ctx, thread, workspacestore.Caller{Writer: workspacestore.WriterDesktop, UserID: owner}, 3); err != nil || len(plan.Segments) != 0 {
		t.Fatalf("plan past the head = %+v, %v; want no segment", plan, err)
	}

	// The executor writes only the cloud ref, only in CLOUD_RUNNING, only as
	// the running turn.
	seedHandoffRow(t, pool, thread, owner, workspacestore.StateCloudRunning, 2, false, "exec-1")
	cloudContent := []byte("the first cloud turn")
	cloudSHA := uploadSegment(t, store, thread, cloudContent, time.Now())
	executor := func(execution string, refs ...workspacestore.RefUpdate) workspacestore.PublishRequest {
		request := desktopPublish(thread, owner, 2, cloudSHA, int64(len(cloudContent)), refs...)
		request.Caller = workspacestore.Caller{Writer: workspacestore.WriterCloudExecutor, ExecutionID: execution}
		return request
	}
	if _, err := service.Publish(ctx, executor("exec-2", workspacestore.RefUpdate{Name: workspacestore.RefCloud, New: sha1Of("c1")})); !errors.Is(err, workspacestore.ErrStaleEpoch) {
		t.Fatalf("another turn's publish = %v, want ErrStaleEpoch", err)
	}
	if _, err := service.Publish(ctx, executor("exec-1", workspacestore.RefUpdate{Name: workspacestore.RefHandoff1, Old: ptr(sha1Of("second")), New: sha1Of("c1")})); !errors.Is(err, workspacestore.ErrRefNotAllowed) {
		t.Fatalf("the executor moving the handoff ref = %v, want ErrRefNotAllowed", err)
	}
	cloud, err := service.Publish(ctx, executor("exec-1", workspacestore.RefUpdate{Name: workspacestore.RefCloud, New: sha1Of("c1")}))
	if err != nil || cloud.Seq != 3 || cloud.Refs[workspacestore.RefCloud] != sha1Of("c1") {
		t.Fatalf("cloud publish = %+v, %v", cloud, err)
	}
	// The desktop cannot write in CLOUD_RUNNING.
	if _, err := service.Publish(ctx, desktopPublish(thread, owner, 2, cloudSHA, int64(len(cloudContent)),
		workspacestore.RefUpdate{Name: workspacestore.RefBase, Old: ptr(base), New: base})); !errors.Is(err, workspacestore.ErrStaleEpoch) {
		t.Fatalf("a desktop publish while the cloud runs = %v, want ErrStaleEpoch", err)
	}

	actions := strings.Join(recorder.actions(), "\n")
	for _, want := range []string{"store.publish, writer desktop", "store.publish_refused", "store.integrity_failed", "store.publish, writer cloud_executor"} {
		if !strings.Contains(actions, want) {
			t.Fatalf("audit actions lack %q:\n%s", want, actions)
		}
	}
	for _, event := range recorder.events {
		if event.EntityType != workspacestore.AuditEntityType || event.EntityName != thread.ConversationUUID {
			t.Fatalf("audit event %+v is not a workspace_handoff event of the thread", event)
		}
	}
}

// Concurrent publishes on ONE thread: exactly one wins the compare-and-set;
// every other answers a ref conflict. Run with -race.
func TestPostgresWorkspaceStoreConcurrentPublishOneWinner(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5121
	fixture := seedLocalWorkThread(t, pool, owner, "local_work")
	seedHandoffRow(t, pool, fixture.thread, owner, workspacestore.StateHandingOff, 1, false, "")
	store := workspacestoretest.New(true)
	service, _ := newWorkspaceStoreService(t, pool, store, defaultWorkspaceLimits())

	const writers = 8
	requests := make([]workspacestore.PublishRequest, writers)
	for i := range requests {
		content := []byte(fmt.Sprintf("snapshot %d", i))
		sha := uploadSegment(t, store, fixture.thread, content, time.Now())
		requests[i] = desktopPublish(fixture.thread, owner, 1, sha, int64(len(content)),
			workspacestore.RefUpdate{Name: workspacestore.RefBase, New: sha1Of("base")},
			workspacestore.RefUpdate{Name: workspacestore.RefHandoff1, New: sha1Of(fmt.Sprintf("snap %d", i))})
	}
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		wins      int
		conflicts int
		others    []error
	)
	start := make(chan struct{})
	for i := range requests {
		wg.Add(1)
		go func(request workspacestore.PublishRequest) {
			defer wg.Done()
			<-start
			_, err := service.Publish(ctx, request)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, workspacestore.ErrRefConflict):
				conflicts++
			default:
				others = append(others, err)
			}
		}(requests[i])
	}
	close(start)
	wg.Wait()
	if wins != 1 || conflicts != writers-1 || len(others) != 0 {
		t.Fatalf("wins %d, conflicts %d, others %v; want exactly one winner", wins, conflicts, others)
	}
}

// Publishes on DIFFERENT threads never wait on each other: thread A's handoff
// row is held locked by an open transaction while thread B publishes.
func TestPostgresWorkspaceStorePublishesOnDifferentThreadsDoNotBlock(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5131
	store := workspacestoretest.New(true)
	service, _ := newWorkspaceStoreService(t, pool, store, defaultWorkspaceLimits())
	threads := make([]workspacestore.Thread, 6)
	for i := range threads {
		threads[i] = seedLocalWorkThread(t, pool, owner, "local_work").thread
		seedHandoffRow(t, pool, threads[i], owner, workspacestore.StateHandingOff, 1, false, "")
	}
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM elitea_runtime.workspace_handoffs
WHERE conversation_uuid = $1::uuid FOR UPDATE`, threads[0].ConversationUUID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(threads))
	for i := 1; i < len(threads); i++ {
		content := []byte("thread " + strconv.Itoa(i))
		sha := uploadSegment(t, store, threads[i], content, time.Now())
		wg.Add(1)
		go func(i int, sha string, size int64) {
			defer wg.Done()
			bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_, errs[i] = service.Publish(bounded, desktopPublish(threads[i], owner, 1, sha, size,
				workspacestore.RefUpdate{Name: workspacestore.RefBase, New: sha1Of("b")},
				workspacestore.RefUpdate{Name: workspacestore.RefHandoff1, New: sha1Of("h")}))
		}(i, sha, int64(len(content)))
	}
	wg.Wait()
	for i := 1; i < len(threads); i++ {
		if errs[i] != nil {
			t.Fatalf("thread %d publish = %v while another thread's row was locked", i, errs[i])
		}
	}
	// The locked thread itself does wait (the lock is real).
	content := []byte("thread 0")
	sha := uploadSegment(t, store, threads[0], content, time.Now())
	bounded, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := service.Publish(bounded, desktopPublish(threads[0], owner, 1, sha, int64(len(content)),
		workspacestore.RefUpdate{Name: workspacestore.RefBase, New: sha1Of("b")})); err == nil {
		t.Fatalf("a publish on the locked thread did not wait for the lock")
	}
}

func TestPostgresWorkspaceStoreSweeper(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5141
	fixture := seedLocalWorkThread(t, pool, owner, "local_work")
	thread := fixture.thread
	seedHandoffRow(t, pool, thread, owner, workspacestore.StateHandingOff, 1, false, "")
	store := workspacestoretest.New(true)
	service, recorder := newWorkspaceStoreService(t, pool, store, defaultWorkspaceLimits())
	// The first upload grant creates the project's workspace bucket, which
	// the orphan sweep walks.
	if _, err := service.RequestUpload(ctx, workspacestore.UploadRequest{
		Thread: thread, Caller: workspacestore.Caller{Writer: workspacestore.WriterDesktop, UserID: owner},
		Epoch: 1, SHA256: digestOf([]byte("x")), Bytes: 1,
	}); err != nil {
		t.Fatal(err)
	}

	old, fresh := []byte("old snapshot"), []byte("new snapshot")
	oldSHA := uploadSegment(t, store, thread, old, time.Now())
	if _, err := service.Publish(ctx, desktopPublish(thread, owner, 1, oldSHA, int64(len(old)),
		workspacestore.RefUpdate{Name: workspacestore.RefBase, New: sha1Of("b")})); err != nil {
		t.Fatal(err)
	}
	freshSHA := uploadSegment(t, store, thread, fresh, time.Now())
	if _, err := service.Publish(ctx, desktopPublish(thread, owner, 1, freshSHA, int64(len(fresh)),
		workspacestore.RefUpdate{Name: workspacestore.RefBase, Old: ptr(sha1Of("b")), New: sha1Of("b2")})); err != nil {
		t.Fatal(err)
	}
	// An orphan: an object with no segment row, older than 24 hours; and a
	// young one (an upload in flight), which stays.
	orphan := uploadSegment(t, store, thread, []byte("never published"), time.Now().Add(-25*time.Hour))
	young := uploadSegment(t, store, thread, []byte("upload in flight"), time.Now())

	// Within the grace nothing is deleted.
	if result, err := service.SweepObjects(ctx, 10); err != nil || result.SupersededDeleted != 0 || result.OrphansDeleted != 1 {
		t.Fatalf("first sweep = %+v, %v; want only the old orphan", result, err)
	}
	keyRef := func(sha string) storage.ObjectRef {
		ref, _ := storage.NewObjectRef("1", storage.WorkspaceBucketName, workspacestore.ObjectKey(thread.ConversationUUID, sha))
		return ref
	}
	if store.Has(keyRef(orphan)) || !store.Has(keyRef(young)) || !store.Has(keyRef(oldSHA)) {
		t.Fatalf("after the first sweep: orphan %t, young %t, superseded %t", store.Has(keyRef(orphan)), store.Has(keyRef(young)), store.Has(keyRef(oldSHA)))
	}
	// 25 hours later the superseded segment's object goes; the row stays as a
	// tombstone; the live segment is untouched.
	service.WithClock(func() time.Time { return time.Now().Add(25 * time.Hour) })
	result, err := service.SweepObjects(ctx, 10)
	if err != nil || result.SupersededDeleted != 1 {
		t.Fatalf("second sweep = %+v, %v; want the superseded object", result, err)
	}
	if store.Has(keyRef(oldSHA)) || !store.Has(keyRef(freshSHA)) {
		t.Fatalf("superseded kept %t, live kept %t", store.Has(keyRef(oldSHA)), store.Has(keyRef(freshSHA)))
	}
	var tombstones int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.workspace_segments
WHERE conversation_uuid = $1::uuid AND object_deleted_at IS NOT NULL`, thread.ConversationUUID).Scan(&tombstones)
	if tombstones != 1 {
		t.Fatalf("tombstones = %d, want 1", tombstones)
	}
	if !strings.Contains(strings.Join(recorder.actions(), "\n"), "store.gc") {
		t.Fatalf("no store.gc audit event: %v", recorder.actions())
	}

	// A failed delete is released for the next sweep.
	another := []byte("third snapshot")
	anotherSHA := uploadSegment(t, store, thread, another, time.Now())
	service.WithClock(time.Now)
	if _, err := service.Publish(ctx, desktopPublish(thread, owner, 1, anotherSHA, int64(len(another)),
		workspacestore.RefUpdate{Name: workspacestore.RefBase, Old: ptr(sha1Of("b2")), New: sha1Of("b3")})); err != nil {
		t.Fatal(err)
	}
	store.FailDelete = true
	service.WithClock(func() time.Time { return time.Now().Add(25 * time.Hour) })
	if result, _ := service.SweepObjects(ctx, 10); result.SupersededDeleted != 0 {
		t.Fatalf("a failing store counted a delete")
	}
	store.FailDelete = false
	if result, err := service.SweepObjects(ctx, 10); err != nil || result.SupersededDeleted != 1 {
		t.Fatalf("retry sweep = %+v, %v; want the released segment deleted", result, err)
	}

	// The thread erasure: every object goes, then the listing is empty.
	empty, err := service.EraseThreadObjects(ctx, thread)
	if err != nil || !empty {
		t.Fatalf("erase = %t, %v", empty, err)
	}
	if store.Has(keyRef(anotherSHA)) || store.Has(keyRef(young)) {
		t.Fatalf("erase left objects behind")
	}
	var live int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.workspace_segments
WHERE conversation_uuid = $1::uuid AND object_deleted_at IS NULL`, thread.ConversationUUID).Scan(&live)
	if live != 0 {
		t.Fatalf("erase left %d live segment rows", live)
	}
}

// Publish hashing (ADR-0034 decision 2): on a backend that verifies a SHA-256
// checksum on PUT, the grant binds the digest and publish reads it back with
// HEAD, so no bytes stream through Main. Without one, publish hashes the bytes.
func TestPostgresWorkspaceStoreChecksumBoundPublish(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	refsFor := func(seed string) []workspacestore.RefUpdate {
		return []workspacestore.RefUpdate{
			{Name: workspacestore.RefBase, New: sha1Of(seed + "-base")}, {Name: workspacestore.RefHandoff1, New: sha1Of(seed)},
		}
	}
	thread := func(owner int64) workspacestore.Thread {
		fixture := seedLocalWorkThread(t, pool, owner, "local_work")
		seedHandoffRow(t, pool, fixture.thread, owner, workspacestore.StateHandingOff, 1, false, "")
		return fixture.thread
	}
	objectRef := func(thread workspacestore.Thread, sha string) storage.ObjectRef {
		ref, err := storage.NewObjectRef(strconv.FormatInt(thread.ProjectID, 10), storage.WorkspaceBucketName,
			workspacestore.ObjectKey(thread.ConversationUUID, sha))
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}

	t.Run("the grant binds the digest", func(t *testing.T) {
		const owner int64 = 5161
		th := thread(owner)
		store := workspacestoretest.New(true)
		store.Checksum, store.Multipart = true, true
		service, _ := newWorkspaceStoreService(t, pool, store, defaultWorkspaceLimits())
		content := []byte("checksum-bound bundle")
		sha := digestOf(content)
		raw, _ := hex.DecodeString(sha)
		grant, err := service.RequestUpload(ctx, workspacestore.UploadRequest{
			Thread: th, Caller: workspacestore.Caller{Writer: workspacestore.WriterDesktop, UserID: owner},
			Epoch: 1, SHA256: sha, Bytes: int64(len(content)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if grant.Headers[storage.ChecksumSHA256Header] != base64.StdEncoding.EncodeToString(raw) {
			t.Fatalf("grant headers = %v, want the base64 digest in %s", grant.Headers, storage.ChecksumSHA256Header)
		}
		if !bytes.Equal(store.PresignedChecksum(objectRef(th, sha)), raw) {
			t.Fatalf("the presigned PUT is not bound to the digest")
		}
		// Above the multipart threshold a checksum backend still gets ONE
		// checksum-bound PUT: a multipart upload has no full-object digest.
		big := digestOf([]byte("big"))
		grant, err = service.RequestUpload(ctx, workspacestore.UploadRequest{
			Thread: th, Caller: workspacestore.Caller{Writer: workspacestore.WriterDesktop, UserID: owner},
			Epoch: 1, SHA256: big, Bytes: 130 << 20,
		})
		if err != nil {
			t.Fatal(err)
		}
		if grant.Multipart != nil || grant.PutURL == "" || grant.Headers[storage.ChecksumSHA256Header] == "" {
			t.Fatalf("grant = %+v, want one checksum-bound PUT", grant)
		}
	})

	t.Run("publish reads the checksum, not the bytes", func(t *testing.T) {
		const owner int64 = 5162
		th := thread(owner)
		store := workspacestoretest.New(true)
		store.Checksum = true
		service, _ := newWorkspaceStoreService(t, pool, store, defaultWorkspaceLimits())
		content := []byte("a bundle the store verified on PUT")
		sha := digestOf(content)
		store.SeedWithChecksum(objectRef(th, sha), content)
		if _, err := service.Publish(ctx, desktopPublish(th, owner, 1, sha, int64(len(content)), refsFor("a")...)); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if gets := store.GetCount(); gets != 0 {
			t.Fatalf("publish read the object %d times, want 0 (HEAD only)", gets)
		}
	})

	t.Run("a reported digest that is not the address is refused and deleted", func(t *testing.T) {
		const owner int64 = 5163
		th := thread(owner)
		store := workspacestoretest.New(true)
		store.Checksum = true
		service, recorder := newWorkspaceStoreService(t, pool, store, defaultWorkspaceLimits())
		content := []byte("declared bundle bytes")
		sha := digestOf(content)
		other := sha256.Sum256([]byte("other"))
		ref := objectRef(th, sha)
		store.SeedWithDigest(ref, content, other[:])
		if _, err := service.Publish(ctx, desktopPublish(th, owner, 1, sha, int64(len(content)), refsFor("b")...)); !errors.Is(err, workspacestore.ErrChecksumMismatch) {
			t.Fatalf("publish = %v, want ErrChecksumMismatch", err)
		}
		if store.Has(ref) || store.GetCount() != 0 {
			t.Fatalf("has = %v, gets = %d; want the object deleted without a read", store.Has(ref), store.GetCount())
		}
		if !strings.Contains(strings.Join(recorder.actions(), ","), "integrity_failed") {
			t.Fatalf("audit = %v, want an integrity failure", recorder.actions())
		}
	})

	t.Run("no full-object checksum falls back to hashing the bytes", func(t *testing.T) {
		const owner int64 = 5164
		th := thread(owner)
		store := workspacestoretest.New(true)
		store.Checksum = true
		service, _ := newWorkspaceStoreService(t, pool, store, defaultWorkspaceLimits())
		content := []byte("a multipart bundle without a full-object digest")
		sha := digestOf(content)
		store.Seed(objectRef(th, sha), content, time.Now())
		if _, err := service.Publish(ctx, desktopPublish(th, owner, 1, sha, int64(len(content)), refsFor("c")...)); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if gets := store.GetCount(); gets != 1 {
			t.Fatalf("gets = %d, want 1 (streaming verify)", gets)
		}
	})

	t.Run("a backend without checksums hashes the bytes", func(t *testing.T) {
		const owner int64 = 5165
		th := thread(owner)
		store := workspacestoretest.New(true)
		service, _ := newWorkspaceStoreService(t, pool, store, defaultWorkspaceLimits())
		content := []byte("a bundle on a backend without checksums")
		sha := digestOf(content)
		grant, err := service.RequestUpload(ctx, workspacestore.UploadRequest{
			Thread: th, Caller: workspacestore.Caller{Writer: workspacestore.WriterDesktop, UserID: owner},
			Epoch: 1, SHA256: sha, Bytes: int64(len(content)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := grant.Headers[storage.ChecksumSHA256Header]; ok {
			t.Fatalf("grant headers = %v, want no checksum header", grant.Headers)
		}
		uploadSegment(t, store, th, content, time.Now())
		if _, err := service.Publish(ctx, desktopPublish(th, owner, 1, sha, int64(len(content)), refsFor("d")...)); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if gets := store.GetCount(); gets != 1 {
			t.Fatalf("gets = %d, want 1", gets)
		}
	})
}

// The project ceiling holds under concurrent publishes of two threads: both
// passed the check before the transaction (it is a fast path only), and the
// re-check inside the transaction, under the project's lock, lets exactly
// one through.
func TestPostgresWorkspaceStoreQuotaHoldsAcrossConcurrentThreads(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	repo, err := NewWorkspaceStoreRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	const owner int64 = 5171
	threads := []workspacestore.Thread{}
	for range 2 {
		fixture := seedLocalWorkThread(t, pool, owner, "local_work")
		seedHandoffRow(t, pool, fixture.thread, owner, workspacestore.StateHandingOff, 1, false, "")
		threads = append(threads, fixture.thread)
	}
	for round := 0; round < 20; round++ {
		if _, err := pool.Exec(ctx, `DELETE FROM elitea_runtime.workspace_segments; DELETE FROM elitea_runtime.workspace_refs;
UPDATE elitea_runtime.workspace_handoffs SET last_seq = 0`); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errs := make([]error, len(threads))
		var wg sync.WaitGroup
		for index, thread := range threads {
			wg.Add(1)
			go func(index int, thread workspacestore.Thread) {
				defer wg.Done()
				sha := digestOf([]byte(fmt.Sprintf("quota %d %d", round, index)))
				<-start
				_, errs[index] = repo.Publish(ctx, workspacestore.PublishRecord{
					Thread: thread, Caller: workspacestore.Caller{Writer: workspacestore.WriterDesktop, UserID: owner},
					Epoch: 1, Kind: workspacestore.KindCompact, SHA256: sha, Bytes: 60, Prerequisites: []string{},
					Refs:      []workspacestore.RefUpdate{{Name: workspacestore.RefBase, New: sha1Of(sha)}, {Name: workspacestore.RefHandoff1, New: sha1Of(sha + "h")}},
					ObjectKey: workspacestore.ObjectKey(thread.ConversationUUID, sha), PhysicalBucket: "elitea-test",
					MaxProjectBytes: 100,
				})
			}(index, thread)
		}
		close(start)
		wg.Wait()
		ok, refused := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, workspacestore.ErrQuotaExceeded):
				refused++
			default:
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if ok != 1 || refused != 1 {
			t.Fatalf("round %d: %d published, %d refused; want exactly one of each (60+60 > 100)", round, ok, refused)
		}
		var used int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(byte_length),0) FROM elitea_runtime.workspace_segments WHERE superseded_at IS NULL`).Scan(&used); err != nil || used != 60 {
			t.Fatalf("round %d: usage %d, %v", round, used, err)
		}
	}
}

// The streaming facade (no presign) writes to a temporary key and promotes
// only verified bytes. A retry after a lost acknowledgement, even a short
// one, never overwrites or deletes the segment that is in place.
func TestPostgresWorkspaceStoreFacadeKeepsAValidSegment(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5181
	fixture := seedLocalWorkThread(t, pool, owner, "local_work")
	seedHandoffRow(t, pool, fixture.thread, owner, workspacestore.StateHandingOff, 1, false, "")
	store := workspacestoretest.New(false)
	service, _ := newWorkspaceStoreService(t, pool, store, defaultWorkspaceLimits())
	desktop := workspacestore.Caller{Writer: workspacestore.WriterDesktop, UserID: owner}
	content := []byte("a facade segment of some length")
	sha := digestOf(content)
	if _, err := service.RequestUpload(ctx, workspacestore.UploadRequest{
		Thread: fixture.thread, Caller: desktop, Epoch: 1, SHA256: sha, Bytes: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	final, _ := storage.NewObjectRef("1", storage.WorkspaceBucketName, workspacestore.ObjectKey(fixture.thread.ConversationUUID, sha))
	stored := func() []byte {
		t.Helper()
		body, _, err := store.Get(ctx, final, nil)
		if err != nil {
			return nil
		}
		defer func() { _ = body.Close() }()
		raw, _ := io.ReadAll(body)
		return raw
	}

	// A short first upload: refused, nothing at the segment key, no
	// temporary object left.
	if err := service.PutObject(ctx, fixture.thread, desktop, sha, bytes.NewReader(content[:5])); !errors.Is(err, workspacestore.ErrChecksumMismatch) {
		t.Fatalf("short upload = %v", err)
	}
	if store.Has(final) || store.Count() != 0 {
		t.Fatalf("a refused upload left objects: has=%t count=%d", store.Has(final), store.Count())
	}
	// The real upload; its acknowledgement is lost.
	if err := service.PutObject(ctx, fixture.thread, desktop, sha, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored(), content) || store.Count() != 1 {
		t.Fatalf("after upload: %q, count %d", stored(), store.Count())
	}
	deletes := store.Deletes
	// The retry is cut short (a client that gave up mid-body): the segment
	// in place is neither overwritten nor deleted.
	if err := service.PutObject(ctx, fixture.thread, desktop, sha, bytes.NewReader(content[:3])); err != nil {
		t.Fatalf("short retry over a valid segment = %v, want success (the segment is in place)", err)
	}
	// A full retry and a retry with other bytes of the same length: the same.
	if err := service.PutObject(ctx, fixture.thread, desktop, sha, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if err := service.PutObject(ctx, fixture.thread, desktop, sha, bytes.NewReader(bytes.Repeat([]byte("x"), len(content)))); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored(), content) || store.Count() != 1 || store.Deletes != deletes {
		t.Fatalf("retries changed the segment: %q, count %d, deletes %d->%d", stored(), store.Count(), deletes, store.Deletes)
	}
	// Wrong bytes already at the key (size differs): a verified upload
	// replaces them; the temporary object is removed.
	store.Seed(final, []byte("bad"), time.Now())
	if err := service.PutObject(ctx, fixture.thread, desktop, sha, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored(), content) || store.Count() != 1 {
		t.Fatalf("a verified upload did not replace bad bytes: %q, count %d", stored(), store.Count())
	}
	if _, err := service.Publish(ctx, desktopPublish(fixture.thread, owner, 1, sha, int64(len(content)),
		workspacestore.RefUpdate{Name: workspacestore.RefBase, New: sha1Of("f-base")},
		workspacestore.RefUpdate{Name: workspacestore.RefHandoff1, New: sha1Of("f")})); err != nil {
		t.Fatalf("publish after the facade upload: %v", err)
	}
}

// The fetch plan reads the head, the refs and the newest segment in one
// snapshot: a publish that commits between the reads changes none of them.
func TestPostgresWorkspaceStoreFetchRowsIsOneSnapshot(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5191
	fixture := seedLocalWorkThread(t, pool, owner, "local_work")
	seedHandoffRow(t, pool, fixture.thread, owner, workspacestore.StateHandingOff, 1, false, "")
	store := workspacestoretest.New(true)
	service, _ := newWorkspaceStoreService(t, pool, store, defaultWorkspaceLimits())
	repo, err := NewWorkspaceStoreRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	first := []byte("first snapshot")
	firstSHA := uploadSegment(t, store, fixture.thread, first, time.Now())
	if _, err := service.Publish(ctx, desktopPublish(fixture.thread, owner, 1, firstSHA, int64(len(first)),
		workspacestore.RefUpdate{Name: workspacestore.RefBase, New: sha1Of("base")},
		workspacestore.RefUpdate{Name: workspacestore.RefHandoff1, New: sha1Of("one")})); err != nil {
		t.Fatal(err)
	}
	second := []byte("second snapshot")
	secondSHA := uploadSegment(t, store, fixture.thread, second, time.Now())
	fetchRowsBetweenReads = func() {
		fetchRowsBetweenReads = nil
		if _, err := service.Publish(ctx, desktopPublish(fixture.thread, owner, 1, secondSHA, int64(len(second)),
			workspacestore.RefUpdate{Name: workspacestore.RefHandoff1, Old: ptr(sha1Of("one")), New: sha1Of("two")})); err != nil {
			t.Errorf("interleaved publish: %v", err)
		}
	}
	t.Cleanup(func() { fetchRowsBetweenReads = nil })
	rows, err := repo.FetchRows(ctx, fixture.thread, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rows.HeadSeq != 1 || rows.Refs[workspacestore.RefHandoff1] != sha1Of("one") || rows.Newest == nil ||
		rows.Newest.Seq != 1 || rows.Newest.SHA256 != firstSHA {
		t.Fatalf("fetch rows mixed two publishes: head %d, handoff/1 %s, newest %+v", rows.HeadSeq,
			rows.Refs[workspacestore.RefHandoff1], rows.Newest)
	}
	after, err := repo.FetchRows(ctx, fixture.thread, 0)
	if err != nil || after.HeadSeq != 2 || after.Refs[workspacestore.RefHandoff1] != sha1Of("two") || after.Newest.SHA256 != secondSHA {
		t.Fatalf("the next fetch = %+v, %v", after, err)
	}
}
