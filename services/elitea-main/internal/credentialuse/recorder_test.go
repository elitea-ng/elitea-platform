package credentialuse

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeStore struct {
	batches [][]Use
	err     error
}

func (f *fakeStore) WriteLastUse(_ context.Context, uses []Use) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.batches = append(f.batches, uses)
	return int64(len(uses)), nil
}

func TestTwoUsesInAnHourAreOneWrite(t *testing.T) {
	store := &fakeStore{}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	r := New(store, nil).WithClock(func() time.Time { return now })
	r.TouchID(KindPAT, 7, 0)
	now = now.Add(10 * time.Minute)
	r.TouchID(KindPAT, 7, 0)
	if _, err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(40 * time.Minute) // 50 minutes after the first use
	r.TouchID(KindPAT, 7, 0)
	if _, err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.batches) != 1 || len(store.batches[0]) != 1 {
		t.Fatalf("batches = %+v, want one write", store.batches)
	}
	// An hour after the first use, the next use is written again.
	now = now.Add(11 * time.Minute)
	r.TouchID(KindPAT, 7, 0)
	if _, err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.batches) != 2 {
		t.Fatalf("batches = %d, want a second write after the hour", len(store.batches))
	}
}

func TestDistinctCredentialsShareOneFlush(t *testing.T) {
	store := &fakeStore{}
	r := New(store, nil)
	r.TouchID(KindPAT, 1, 0)
	r.Touch(KindToolkitSecret, "3:token", 3)
	r.Touch(KindCredentialGrant, "b/1/r", 3)
	if _, err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.batches) != 1 || len(store.batches[0]) != 3 {
		t.Fatalf("batches = %+v", store.batches)
	}
}

func TestAFailedFlushLetsTheNextUseQueueAgain(t *testing.T) {
	store := &fakeStore{err: errors.New("down")}
	r := New(store, nil)
	r.TouchID(KindPAT, 1, 0)
	if _, err := r.Flush(context.Background()); err == nil {
		t.Fatal("want the store error")
	}
	store.err = nil
	r.TouchID(KindPAT, 1, 0)
	if _, err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.batches) != 1 {
		t.Fatalf("batches = %d, want the use re-queued after the failure", len(store.batches))
	}
}

func TestNilRecorderIgnoresEverything(t *testing.T) {
	var r *Recorder
	r.TouchID(KindPAT, 1, 0)
	if _, err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	Install(nil)
	Default().Touch(KindPAT, "1", 0)
}
