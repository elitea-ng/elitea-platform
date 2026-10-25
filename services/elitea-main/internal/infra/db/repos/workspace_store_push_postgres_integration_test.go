package repos

// Store Phase 2 (ADR-0034 decision 2) against a REAL database: PUSH segments
// on a COMPACT, their prerequisites checked against the thread's refs, the
// compare-and-set race, an idempotent PUSH replay, the chain cap, the fetch
// plan's chains, compaction superseding the chain, and the sweeper keeping
// an object a grant still targets.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore/workspacestoretest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

const pushExecution = "exec-push-1"

func pushPublish(thread workspacestore.Thread, epoch int64, kind, sha string, bytes int64, prerequisites []string, old *string, cloud string) workspacestore.PublishRequest {
	return workspacestore.PublishRequest{
		Thread: thread, Caller: workspacestore.Caller{Writer: workspacestore.WriterCloudExecutor, ExecutionID: pushExecution},
		Epoch: epoch, Kind: kind, SHA256: sha, Bytes: bytes, Prerequisites: prerequisites,
		Refs:         []workspacestore.RefUpdate{{Name: workspacestore.RefCloud, Old: old, New: cloud}},
		ObjectFormat: workspacestore.ObjectFormatSHA1,
	}
}

// pushFixture is a thread whose desktop COMPACT (base, handoff/1) is
// published, now CLOUD_RUNNING under pushExecution at epoch 1.
type pushFixture struct {
	thread  workspacestore.Thread
	store   *workspacestoretest.Store
	service *workspacestore.Service
	audit   *recordingAudit
	handoff string
}

func newPushFixture(t *testing.T, owner int64, limits workspacestore.Limits) pushFixture {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	fixture := seedLocalWorkThread(t, pool, owner, "local_work")
	seedHandoffRow(t, pool, fixture.thread, owner, workspacestore.StateHandingOff, 1, false, "")
	store := workspacestoretest.New(true)
	service, recorder := newWorkspaceStoreService(t, pool, store, limits)
	snapshot := []byte("desktop compact " + strconv.FormatInt(owner, 10))
	sha := uploadSegment(t, store, fixture.thread, snapshot, time.Now())
	handoff := sha1Of("handoff" + strconv.FormatInt(owner, 10))
	if _, err := service.Publish(ctx, desktopPublish(fixture.thread, owner, 1, sha, int64(len(snapshot)),
		workspacestore.RefUpdate{Name: workspacestore.RefBase, New: sha1Of("base")},
		workspacestore.RefUpdate{Name: workspacestore.RefHandoff1, New: handoff})); err != nil {
		t.Fatal(err)
	}
	seedHandoffRow(t, pool, fixture.thread, owner, workspacestore.StateCloudRunning, 1, false, pushExecution)
	return pushFixture{thread: fixture.thread, store: store, service: service, audit: recorder, handoff: handoff}
}

func (f pushFixture) push(t *testing.T, content string, prerequisite string, old *string, cloud string) (workspacestore.PublishResult, error) {
	t.Helper()
	bytes := []byte(content)
	sha := uploadSegment(t, f.store, f.thread, bytes, time.Now())
	return f.service.Publish(context.Background(), pushPublish(f.thread, 1, workspacestore.KindPush, sha,
		int64(len(bytes)), []string{prerequisite}, old, cloud))
}

func planSeqs(plan workspacestore.FetchPlan) []string {
	out := []string{}
	for _, segment := range plan.Segments {
		out = append(out, segment.Kind+":"+strconv.FormatInt(segment.Seq, 10))
	}
	return out
}

func TestPostgresWorkspaceStorePushChain(t *testing.T) {
	ctx := context.Background()
	f := newPushFixture(t, 5301, defaultWorkspaceLimits())
	executor := workspacestore.Caller{Writer: workspacestore.WriterCloudExecutor, ExecutionID: pushExecution}
	c1, c2 := sha1Of("cloud-1"), sha1Of("cloud-2")

	first, err := f.push(t, "push one", f.handoff, nil, c1)
	if err != nil || first.Seq != 2 || len(first.Superseded) != 0 {
		t.Fatalf("first push = %+v, %v; want seq 2 superseding nothing", first, err)
	}
	second, err := f.push(t, "push two", c1, ptr(c1), c2)
	if err != nil || second.Seq != 3 || second.Refs[workspacestore.RefHandoff1] != f.handoff || second.Refs[workspacestore.RefCloud] != c2 {
		t.Fatalf("second push = %+v, %v; want seq 3 with the full ref map", second, err)
	}

	t.Run("an identical retried push answers the original result", func(t *testing.T) {
		bytes := []byte("push two")
		replay, err := f.service.Publish(ctx, pushPublish(f.thread, 1, workspacestore.KindPush, digestOf(bytes),
			int64(len(bytes)), []string{c1}, ptr(c1), c2))
		if err != nil || !replay.Replayed || replay.Seq != 3 {
			t.Fatalf("replay = %+v, %v; want the original seq 3, replayed", replay, err)
		}
	})
	t.Run("the fetch plan is the compact and every later push", func(t *testing.T) {
		plan, err := f.service.Plan(ctx, f.thread, executor, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(planSeqs(plan), " "); got != "COMPACT:1 PUSH:2 PUSH:3" || plan.HeadSeq != 3 {
			t.Fatalf("plan = %s (head %d), want COMPACT:1 PUSH:2 PUSH:3", got, plan.HeadSeq)
		}
		if push := plan.Segments[2]; len(push.Prerequisites) != 1 || push.Prerequisites[0] != c1 ||
			push.Refs[workspacestore.RefCloud] != c2 || push.Refs[workspacestore.RefBase] != sha1Of("base") || push.URL == "" {
			t.Fatalf("push item = %+v, want its prerequisite, the full refs and a URL", push)
		}
		since, err := f.service.Plan(ctx, f.thread, executor, 2)
		if err != nil || strings.Join(planSeqs(since), " ") != "PUSH:3" {
			t.Fatalf("plan since 2 = %v, %v; want PUSH:3 only", planSeqs(since), err)
		}
		head, err := f.service.Plan(ctx, f.thread, executor, 3)
		if err != nil || len(head.Segments) != 0 {
			t.Fatalf("plan at the head = %v, %v; want nothing", planSeqs(head), err)
		}
	})
	t.Run("two pushes on the same old value: one wins, one conflicts", func(t *testing.T) {
		if _, err := f.push(t, "racer a", c2, ptr(c2), sha1Of("racer-a")); err != nil {
			t.Fatal(err)
		}
		_, err := f.push(t, "racer b", c2, ptr(c2), sha1Of("racer-b"))
		var conflict *workspacestore.RefConflictError
		if !errors.As(err, &conflict) || conflict.Current[workspacestore.RefCloud] == nil ||
			*conflict.Current[workspacestore.RefCloud] != sha1Of("racer-a") {
			t.Fatalf("the losing push = %v, want a ref conflict naming the winner", err)
		}
	})
	t.Run("an unknown prerequisite is refused and nothing moves", func(t *testing.T) {
		tip := sha1Of("racer-a")
		_, err := f.push(t, "stranger", sha1Of("never-published"), ptr(tip), sha1Of("stranger"))
		if !errors.Is(err, workspacestore.ErrPrerequisiteUnknown) {
			t.Fatalf("err = %v, want ErrPrerequisiteUnknown", err)
		}
		plan, _ := f.service.Plan(ctx, f.thread, executor, 0)
		if plan.Refs[workspacestore.RefCloud] != tip || plan.HeadSeq != 4 {
			t.Fatalf("after the refusal: cloud %s head %d, want %s and 4", plan.Refs[workspacestore.RefCloud], plan.HeadSeq, tip)
		}
	})
	t.Run("a compact supersedes the chain", func(t *testing.T) {
		tip := sha1Of("racer-a")
		bytes := []byte("cloud compact")
		sha := uploadSegment(t, f.store, f.thread, bytes, time.Now())
		compact, err := f.service.Publish(ctx, pushPublish(f.thread, 1, workspacestore.KindCompact, sha,
			int64(len(bytes)), nil, ptr(tip), sha1Of("compacted")))
		if err != nil || len(compact.Superseded) != 4 {
			t.Fatalf("compact = %+v, %v; want the 4 earlier segments superseded", compact, err)
		}
		plan, err := f.service.Plan(ctx, f.thread, executor, 0)
		if err != nil || strings.Join(planSeqs(plan), " ") != "COMPACT:5" {
			t.Fatalf("plan after compaction = %v, %v; want COMPACT:5", planSeqs(plan), err)
		}
	})
	actions := strings.Join(f.audit.actions(), "\n")
	for _, want := range []string{"store.publish, writer cloud_executor, kind PUSH, prerequisites 1",
		"kind PUSH, prerequisites 1, epoch 1, segment", "reason prerequisite_unknown", "reason ref_conflict"} {
		if !strings.Contains(actions, want) {
			t.Errorf("audit lacks %q:\n%s", want, actions)
		}
	}
}

func TestPostgresWorkspaceStorePushChainCapAndQuota(t *testing.T) {
	ctx := context.Background()
	t.Run("the chain cap refuses a push and a compact resets it", func(t *testing.T) {
		f := newPushFixture(t, 5311, defaultWorkspaceLimits())
		f.service.WithMaxPushChain(2)
		c1, c2 := sha1Of("cap-1"), sha1Of("cap-2")
		if _, err := f.push(t, "cap one", f.handoff, nil, c1); err != nil {
			t.Fatal(err)
		}
		if _, err := f.push(t, "cap two", c1, ptr(c1), c2); err != nil {
			t.Fatal(err)
		}
		if _, err := f.push(t, "cap three", c2, ptr(c2), sha1Of("cap-3")); !errors.Is(err, workspacestore.ErrChainTooLong) {
			t.Fatalf("third push = %v, want ErrChainTooLong", err)
		}
		bytes := []byte("cap compact")
		sha := uploadSegment(t, f.store, f.thread, bytes, time.Now())
		if _, err := f.service.Publish(ctx, pushPublish(f.thread, 1, workspacestore.KindCompact, sha,
			int64(len(bytes)), nil, ptr(c2), sha1Of("cap-compact"))); err != nil {
			t.Fatal(err)
		}
		if _, err := f.push(t, "cap after", sha1Of("cap-compact"), ptr(sha1Of("cap-compact")), sha1Of("cap-4")); err != nil {
			t.Fatalf("a push after the compact = %v, want accepted", err)
		}
		if !strings.Contains(strings.Join(f.audit.actions(), "\n"), "reason segment_chain_too_long") {
			t.Fatal("the cap refusal is not audited")
		}
	})
	t.Run("a push counts the thread's own live bytes against the ceiling", func(t *testing.T) {
		limits := defaultWorkspaceLimits()
		// The desktop compact of newPushFixture is 21 bytes ("desktop compact 5321").
		limits.MaxProjectWorkspaceBytes = 30
		f := newPushFixture(t, 5321, limits)
		if _, err := f.push(t, "a twelve-byte", f.handoff, nil, sha1Of("q-1")); !errors.Is(err, workspacestore.ErrQuotaExceeded) {
			t.Fatalf("push = %v, want ErrQuotaExceeded (21 live + 13 > 30)", err)
		}
	})
}

func TestPostgresWorkspaceStoreSweeperKeepsAnObjectAGrantTargets(t *testing.T) {
	ctx := context.Background()
	f := newPushFixture(t, 5331, defaultWorkspaceLimits())
	c1 := sha1Of("gc-1")
	if _, err := f.push(t, "gc push", f.handoff, nil, c1); err != nil {
		t.Fatal(err)
	}
	bytes := []byte("gc compact")
	sha := uploadSegment(t, f.store, f.thread, bytes, time.Now())
	if _, err := f.service.Publish(ctx, pushPublish(f.thread, 1, workspacestore.KindCompact, sha,
		int64(len(bytes)), nil, ptr(c1), sha1Of("gc-2"))); err != nil {
		t.Fatal(err)
	}
	// A writer asks to upload the superseded push's bytes again: it gets a
	// grant (the digest is no longer live), and the sweeper must keep the
	// object that grant targets.
	pushed := digestOf([]byte("gc push"))
	executor := workspacestore.Caller{Writer: workspacestore.WriterCloudExecutor, ExecutionID: pushExecution}
	grant, err := f.service.RequestUpload(ctx, workspacestore.UploadRequest{Thread: f.thread, Caller: executor, Epoch: 1, SHA256: pushed, Bytes: 7})
	if err != nil || grant.Exists {
		t.Fatalf("grant = %+v, %v; want an upload grant", grant, err)
	}
	service := f.service.WithClock(func() time.Time { return time.Now().Add(workspacestore.SupersededGrace + time.Hour) })
	result, err := service.SweepObjects(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	object := func(digest string) bool {
		ref, err := storage.NewObjectRef(strconv.FormatInt(f.thread.ProjectID, 10), storage.WorkspaceBucketName,
			workspacestore.ObjectKey(f.thread.ConversationUUID, digest))
		if err != nil {
			t.Fatal(err)
		}
		return f.store.Has(ref)
	}
	desktop := digestOf([]byte("desktop compact 5331"))
	if result.SupersededDeleted != 1 || object(desktop) || !object(pushed) || !object(sha) {
		t.Fatalf("sweep = %+v; desktop compact kept %t, granted push kept %t, live compact kept %t; want only the desktop compact deleted",
			result, object(desktop), object(pushed), object(sha))
	}
}
