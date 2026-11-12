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
