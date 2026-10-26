package agentexecution

import (
	"context"
	"errors"
	"strings"
	"testing"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type steeringStoreStub struct {
	effects []SteeringEffect
	err     error
	calls   int
}

func (s *steeringStoreStub) QueueSteering(context.Context, SteeringQueueRequest) (SteeringMessage, []SteeringEffect, error) {
	s.calls++
	return SteeringMessage{ID: "queued"}, s.effects, s.err
}

func (s *steeringStoreStub) ListSteering(context.Context, SteeringTarget) ([]SteeringMessage, error) {
	s.calls++
	return nil, s.err
}

func (s *steeringStoreStub) EditSteering(context.Context, SteeringEditRequest) (SteeringMessage, []SteeringEffect, error) {
	s.calls++
	return SteeringMessage{}, s.effects, s.err
}

func (s *steeringStoreStub) RemoveSteering(context.Context, SteeringRemoveRequest) ([]SteeringEffect, error) {
	s.calls++
	return s.effects, s.err
}

func (s *steeringStoreStub) HandOffSteering(context.Context, SteeringHandOffRequest) ([]SteeringMessage, error) {
	s.calls++
	return nil, s.err
}

func (s *steeringStoreStub) ClaimSteering(context.Context, SteeringClaimRequest) ([]ClaimedSteeringMessage, []SteeringEffect, error) {
	s.calls++
	return nil, s.effects, s.err
}

type steeringWakerStub struct{ wakes []string }

func (s *steeringWakerStub) Notify(projectID, executionID string, cursor uint64) {
	s.wakes = append(s.wakes, projectID+"/"+executionID)
	_ = cursor
}

var steeringTestTarget = SteeringTarget{ProjectID: 7, ActorUserID: 11, ResponseMessageID: "10000000-0000-4000-8000-000000000061"}

func TestSteeringServiceValidatesBeforeTheStore(t *testing.T) {
	for _, test := range []struct {
		name    string
		request SteeringQueueRequest
		want    error
	}{
		{name: "no key", request: SteeringQueueRequest{SteeringTarget: steeringTestTarget, Content: "x"}, want: ErrSteeringIdempotencyRequired},
		{name: "bad key", request: SteeringQueueRequest{SteeringTarget: steeringTestTarget, IdempotencyKey: "a b", Content: "x"}, want: ErrSteeringIdempotencyRequired},
		{name: "blank", request: SteeringQueueRequest{SteeringTarget: steeringTestTarget, IdempotencyKey: "k", Content: " \n\t"}, want: ErrInvalidSteering},
		{name: "too large", request: SteeringQueueRequest{SteeringTarget: steeringTestTarget, IdempotencyKey: "k", Content: strings.Repeat("a", MaxSteeringContentBytes+1)}, want: ErrInvalidSteering},
		{name: "invalid utf-8", request: SteeringQueueRequest{SteeringTarget: steeringTestTarget, IdempotencyKey: "k", Content: "\xff"}, want: ErrInvalidSteering},
		{name: "upper-case response id", request: SteeringQueueRequest{SteeringTarget: SteeringTarget{ProjectID: 7, ActorUserID: 11, ResponseMessageID: "1000000A-0000-4000-8000-000000000061"}, IdempotencyKey: "k", Content: "x"}, want: ErrInvalidSteering},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &steeringStoreStub{}
			service, err := NewSteeringService(store, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Queue(context.Background(), test.request); !errors.Is(err, test.want) {
				t.Fatalf("err=%v want=%v", err, test.want)
			}
			if store.calls != 0 {
				t.Fatal("an invalid request reached the store")
			}
		})
	}
}

func TestSteeringServiceWakesTheStreamAndHidesRawErrors(t *testing.T) {
	store := &steeringStoreStub{effects: []SteeringEffect{
		{ProjectID: 7, ExecutionID: "execution-1", Cursor: 41},
		{ProjectID: 7, ExecutionID: "execution-1"}, // a replayed event: no cursor, no wake
	}}
	waker := &steeringWakerStub{}
	service, err := NewSteeringService(store, waker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Queue(context.Background(), SteeringQueueRequest{SteeringTarget: steeringTestTarget, IdempotencyKey: "k-1", Content: "go"}); err != nil {
		t.Fatal(err)
	}
	if len(waker.wakes) != 1 || waker.wakes[0] != "7/execution-1" {
		t.Fatalf("wakes=%v", waker.wakes)
	}

	store.err = errors.New("pq: relation does not exist")
	if _, err := service.List(context.Background(), steeringTestTarget); !errors.Is(err, ErrSteeringFailed) || strings.Contains(err.Error(), "pq") {
		t.Fatalf("err=%v", err)
	}
	store.err = ErrSteeringQueueFull
	if _, err := service.Queue(context.Background(), SteeringQueueRequest{SteeringTarget: steeringTestTarget, IdempotencyKey: "k-2", Content: "go"}); !errors.Is(err, ErrSteeringQueueFull) {
		t.Fatalf("err=%v", err)
	}
	store.err = runtimedomain.ErrStaleFence
	fence := runtimedomain.Fence{CommandID: "c", ExecutionID: "e", Generation: 1, WorkloadIdentity: "w", WorkloadSessionID: "s", ProducerID: "p", ClaimAttempt: 1, LeaseEpoch: 1}
	fence.Token[0] = 1
	if _, err := service.Claim(context.Background(), SteeringClaimRequest{Fence: fence}); !errors.Is(err, runtimedomain.ErrStaleFence) {
		t.Fatalf("claim err=%v", err)
	}
	calls := store.calls
	if _, err := service.Claim(context.Background(), SteeringClaimRequest{Fence: fence, Acknowledged: []string{"not-a-uuid"}}); !errors.Is(err, ErrInvalidSteering) {
		t.Fatalf("bad ack err=%v", err)
	}
	if store.calls != calls {
		t.Fatal("a malformed acknowledgement reached the store")
	}
}

func TestSteeringServiceHandOffNeedsAClientKey(t *testing.T) {
	for _, key := range []string{"", "a b", "server_10000000-0000-4000-8000-000000000001"} {
		store := &steeringStoreStub{}
		service, err := NewSteeringService(store, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.HandOff(context.Background(), SteeringHandOffRequest{SteeringTarget: steeringTestTarget, IdempotencyKey: key}); err != ErrSteeringIdempotencyRequired || store.calls != 0 {
			t.Fatalf("key=%q err=%v calls=%d", key, err, store.calls)
		}
	}
	store := &steeringStoreStub{}
	service, _ := NewSteeringService(store, nil)
	if _, err := service.HandOff(context.Background(), SteeringHandOffRequest{SteeringTarget: steeringTestTarget, IdempotencyKey: "k-1", QuestionID: "not-a-uuid"}); err != ErrInvalidSteering || store.calls != 0 {
		t.Fatalf("bad question id err=%v calls=%d", err, store.calls)
	}
	messages, err := service.HandOff(context.Background(), SteeringHandOffRequest{SteeringTarget: steeringTestTarget, IdempotencyKey: "k-1", QuestionID: "10000000-0000-4000-8000-000000000099"})
	if err != nil || messages == nil || len(messages) != 0 || store.calls != 1 {
		t.Fatalf("messages=%v err=%v calls=%d", messages, err, store.calls)
	}
}
