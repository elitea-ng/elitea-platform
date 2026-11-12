package localturn

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// forwardingStore is a decorator that forwards the start to its inner store
// without running Admit: it loses the routing and the recall.
type forwardingStore struct{ commitFakeStore }

func (s *forwardingStore) StartLocalTurn(_ context.Context, record StartRecord) (StartedTurn, error) {
	return StartedTurn{ExecutionID: record.ExecutionID, Created: true}, nil
}

// Issue 1940: the store runs Admit. A start whose store did not is refused
// with ErrUnavailable, never answered without its routing and recall.
func TestStartRefusesAStoreThatDidNotRunAdmit(t *testing.T) {
	recalls := 0
	service, err := NewService(&forwardingStore{}, commitFakePolicy{allowed: true},
		countingMemories{recalls: &recalls}, commitFakeAudit{},
		func() (string, error) { return strings.Repeat("d", 32), nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Start(t.Context(), grantStartRequest("", 10))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Start() error = %v, want ErrUnavailable", err)
	}
}

// admittingStore runs Admit and answers through Admitted unless forget is
// set; created false is the replay of an earlier start.
type admittingStore struct {
	commitFakeStore
	forget  bool
	created bool
}

func (s *admittingStore) StartLocalTurn(ctx context.Context, record StartRecord) (StartedTurn, error) {
	record, err := record.Admit(ctx, func() (ConversationFloor, error) { return ConversationFloor{}, nil })
	if err != nil {
		return StartedTurn{}, err
	}
	turn := StartedTurn{ExecutionID: record.ExecutionID, Created: s.created}
	if s.forget {
		return turn, nil
	}
	return record.Admitted(turn), nil
}

func startWith(t *testing.T, store Store) error {
	t.Helper()
	recalls := 0
	service, err := NewService(store, commitFakePolicy{allowed: true},
		countingMemories{recalls: &recalls}, commitFakeAudit{},
		func() (string, error) { return strings.Repeat("d", 32), nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Start(t.Context(), grantStartRequest("", 10))
	return err
}

// The evidence rides the answer, not a side effect of the call: a store that
// ran Admit but answers without it is refused, and a replay (Created false)
// that is admitted like the first start is not.
func TestStartChecksTheAdmissionEvidenceOnTheAnswer(t *testing.T) {
	if err := startWith(t, &admittingStore{forget: true, created: true}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("answer without evidence: error = %v, want ErrUnavailable", err)
	}
	if err := startWith(t, &admittingStore{created: true}); err != nil {
		t.Fatalf("admitted start: %v", err)
	}
	if err := startWith(t, &admittingStore{created: false}); err != nil {
		t.Fatalf("admitted replay refused: %v", err)
	}
}
