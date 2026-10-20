package conversationactivity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	snapshot Snapshot
	found    bool
	err      error
}

func (s fakeStore) ConversationByUUID(context.Context, int64, string) (Snapshot, bool, error) {
	return s.snapshot, s.found, s.err
}

func (s fakeStore) ConversationByExecution(context.Context, string) (Snapshot, bool, error) {
	return s.snapshot, s.found, s.err
}

func (s fakeStore) ConversationByMessage(context.Context, int64, string) (Snapshot, bool, error) {
	return s.snapshot, s.found, s.err
}

type fakeBus struct {
	mu     sync.Mutex
	frames []Frame
	done   chan struct{}
}

func (b *fakeBus) Publish(_ context.Context, _ int64, _ Audience, frame Frame) error {
	b.mu.Lock()
	b.frames = append(b.frames, frame)
	b.mu.Unlock()
	if b.done != nil {
		b.done <- struct{}{}
	}
	return nil
}

func TestNilNotifierPublishesNothing(t *testing.T) {
	if New(nil, &fakeBus{}, nil) != nil || New(fakeStore{}, nil, nil) != nil {
		t.Fatal("New without a store or a bus must answer nil")
	}
	var n *Notifier
	n.ConversationChanged(context.Background(), 1, "c")
	n.ExecutionChanged(context.Background(), "e")
	n.MessageChanged(context.Background(), 1, "m")
	n.Read(context.Background(), 1, 2, 3, "c", "t")
}

func TestNotifierHintsUnreadOnlyWhenTheTurnStops(t *testing.T) {
	for _, tc := range []struct {
		state string
		hint  bool
	}{{"running", false}, {"needs_you", true}, {"idle", true}} {
		bus := &fakeBus{}
		store := fakeStore{found: true, snapshot: Snapshot{ProjectID: 1, Audience: Audience{Project: true}, Frame: Frame{ConversationID: 3, State: tc.state}}}
		New(store, bus, nil, Synchronous()).ExecutionChanged(context.Background(), "e")
		if len(bus.frames) != 1 {
			t.Fatalf("%s: %d frames", tc.state, len(bus.frames))
		}
		got := bus.frames[0].UnreadHint
		if (got != nil) != tc.hint || (got != nil && !*got) {
			t.Fatalf("%s: unread_hint = %v, want %v", tc.state, got, tc.hint)
		}
	}
}

func TestNotifierSkipsHiddenMissingAndFailedReads(t *testing.T) {
	for name, store := range map[string]fakeStore{
		"hidden":  {found: true, snapshot: Snapshot{ProjectID: 1, Hidden: true, Frame: Frame{State: "idle"}}},
		"missing": {},
		"failed":  {err: errors.New("down")},
	} {
		bus := &fakeBus{}
		New(store, bus, nil, Synchronous()).ConversationChanged(context.Background(), 1, "c")
		if len(bus.frames) != 0 {
			t.Fatalf("%s: published %v", name, bus.frames)
		}
	}
}

func TestNotifierPublishesDetachedFromTheCaller(t *testing.T) {
	bus := &fakeBus{done: make(chan struct{}, 1)}
	store := fakeStore{found: true, snapshot: Snapshot{ProjectID: 1, Frame: Frame{ConversationID: 3, State: "running"}}}
	ctx, cancel := context.WithCancel(context.Background())
	New(store, bus, nil).ConversationChanged(ctx, 1, "c")
	cancel() // the caller's request ends; the frame still goes out
	select {
	case <-bus.done:
	case <-time.After(5 * time.Second):
		t.Fatal("no frame after the caller's context ended")
	}
}
