package workspacestore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore/workspacestoretest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

const (
	flightProject = int64(7)
	flightThread  = "0b6f9a52-1c2d-4e5f-8a9b-0c1d2e3f4a5b"
	flightOwner   = int64(11)
)

// flightRepo is the publish half of the repository. The other methods are
// not called by Publish; the embedded nil interface panics if one is.
type flightRepo struct {
	workspacestore.Repository
	mu        sync.Mutex
	prechecks int
	// gate, when set, holds the FIRST precheck until it is closed, and
	// entered receives once when it starts waiting.
	gate    chan struct{}
	entered chan struct{}
	// panicFirst makes the first precheck panic (after the gate).
	panicFirst bool
}

func (r *flightRepo) WorkspaceBucket(context.Context, int64) (workspacestore.WorkspaceBucket, bool, error) {
	return workspacestore.WorkspaceBucket{ID: 1, Name: storage.WorkspaceBucketName}, true, nil
}

func (r *flightRepo) PublishPrecheck(context.Context, workspacestore.Thread, string) (workspacestore.PublishPrecheck, error) {
	r.mu.Lock()
	r.prechecks++
	first := r.prechecks == 1
	r.mu.Unlock()
	if first && r.gate != nil {
		r.entered <- struct{}{}
		<-r.gate
		if r.panicFirst {
			panic("repository fault")
		}
	}
	if r.panicFirst && !first {
		// The retry after a panicked leader: the thread is gone.
		return workspacestore.PublishPrecheck{}, workspacestore.ErrNotFound
	}
	return workspacestore.PublishPrecheck{Thread: workspacestore.ThreadRow{
		State: workspacestore.StateHandingOff, Epoch: 1, OwnerUserID: flightOwner,
	}}, nil
}

func (r *flightRepo) Publish(context.Context, workspacestore.PublishRecord) (workspacestore.PublishResult, error) {
	return workspacestore.PublishResult{
		Seq: 1, Refs: map[string]string{workspacestore.RefHandoff1: "a"}, Superseded: []int64{},
	}, nil
}

type flightLimits struct{}

func (flightLimits) Limits(context.Context, int64) (workspacestore.Limits, error) {
	return workspacestore.Limits{MaxSegmentBytes: 1 << 20, MaxProjectWorkspaceBytes: 1 << 30}, nil
}

type flightAudit struct{}

func (flightAudit) StoreEvent(context.Context, workspacestore.AuditEvent) {}

func flightService(t *testing.T, repo *flightRepo) (*workspacestore.Service, workspacestore.PublishRequest) {
	t.Helper()
	store := workspacestoretest.New(true)
	content := []byte("segment")
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	ref, err := storage.NewObjectRef(fmt.Sprint(flightProject), storage.WorkspaceBucketName, workspacestore.ObjectKey(flightThread, sha))
	if err != nil {
		t.Fatal(err)
	}
	store.Seed(ref, content, time.Now())
	service, err := workspacestore.NewService(repo,
		workspacestore.Buckets{Default: workspacestore.PhysicalStore{Name: "physical", Store: store}},
		flightLimits{}, flightAudit{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service, workspacestore.PublishRequest{
		Thread: workspacestore.Thread{ProjectID: flightProject, ConversationUUID: flightThread},
		Caller: workspacestore.Caller{Writer: workspacestore.WriterDesktop, UserID: flightOwner},
		Epoch:  1, Kind: workspacestore.KindCompact, SHA256: sha, Bytes: int64(len(content)),
		Prerequisites: []string{}, ObjectFormat: workspacestore.ObjectFormatSHA1,
		Refs: []workspacestore.RefUpdate{{Name: workspacestore.RefHandoff1, New: "0123456789abcdef0123456789abcdef01234567"}},
	}
}

// A leader that panics must not hand its waiter a zero result as success:
// the waiter runs on its own and answers its own outcome.
func TestIdenticalRetryAfterAPanickedLeaderRunsOnItsOwn(t *testing.T) {
	repo := &flightRepo{gate: make(chan struct{}), entered: make(chan struct{}, 1), panicFirst: true}
	service, request := flightService(t, repo)
	leader := make(chan any, 1)
	go func() {
		defer func() { leader <- recover() }()
		_, _ = service.Publish(context.Background(), request)
	}()
	<-repo.entered
	waiter := make(chan error, 1)
	var waiterResult workspacestore.PublishResult
	go func() {
		var err error
		waiterResult, err = service.Publish(context.Background(), request)
		waiter <- err
	}()
	time.Sleep(20 * time.Millisecond) // the waiter is parked on the leader
	close(repo.gate)
	if recovered := <-leader; recovered == nil {
		t.Fatal("the leader did not panic")
	}
	err := <-waiter
	if err == nil || waiterResult.Seq != 0 {
		t.Fatalf("waiter after a panicked leader = %+v, %v; want its own failure, not a shared zero success", waiterResult, err)
	}
	if !errors.Is(err, workspacestore.ErrNotFound) {
		t.Fatalf("waiter error = %v, want its own run's answer (not found)", err)
	}
}

// The waiter gets a COPY of the leader's result: writing one answer's Refs
// races with nothing that reads the other (run with -race).
func TestIdenticalRetryGetsItsOwnCopyOfTheResult(t *testing.T) {
	repo := &flightRepo{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	service, request := flightService(t, repo)
	type outcome struct {
		result workspacestore.PublishResult
		err    error
	}
	first, second := make(chan outcome, 1), make(chan outcome, 1)
	go func() {
		result, err := service.Publish(context.Background(), request)
		first <- outcome{result, err}
	}()
	<-repo.entered
	go func() {
		result, err := service.Publish(context.Background(), request)
		second <- outcome{result, err}
	}()
	time.Sleep(20 * time.Millisecond)
	close(repo.gate)
	a, b := <-first, <-second
	if a.err != nil || b.err != nil || a.result.Seq != b.result.Seq || !b.result.Replayed {
		t.Fatalf("leader %+v, waiter %+v", a, b)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 1000 {
			a.result.Refs[fmt.Sprint("x", i)] = "y"
		}
	}()
	go func() {
		defer wg.Done()
		for range 1000 {
			_ = b.result.Refs[workspacestore.RefHandoff1]
		}
	}()
	wg.Wait()
	if _, shared := b.result.Refs["x0"]; shared {
		t.Fatal("the waiter's Refs is the leader's map")
	}
	if repo.prechecks != 1 {
		t.Fatalf("prechecks = %d, want 1 (the retry shared the leader's run)", repo.prechecks)
	}
}
