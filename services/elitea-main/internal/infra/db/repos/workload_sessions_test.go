package repos

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/workloadauth"
)

func TestWorkloadSessionsRepositoryVerifiesExactActiveBindingWithDatabaseClock(t *testing.T) {
	store := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{true}}}}
	repository, err := newWorkloadSessionsRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	binding := workloadauth.SessionBinding{
		WorkloadIdentity:  "spiffe://elitea.example/runtime/python-worker",
		WorkloadSessionID: "session-1",
		ProducerID:        "producer-1",
	}
	if err := repository.VerifyActiveSession(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	if len(store.rowCalls) != 1 {
		t.Fatalf("query calls = %d, want 1", len(store.rowCalls))
	}
	call := store.rowCalls[0]
	if !strings.Contains(call.sql, "issued_at <= clock_timestamp()") || !strings.Contains(call.sql, "expires_at > clock_timestamp()") || !strings.Contains(call.sql, "revoked_at IS NULL") {
		t.Fatalf("query does not use authoritative active-session time: %s", call.sql)
	}
	wantArgs := []any{binding.WorkloadSessionID, binding.WorkloadIdentity, binding.ProducerID}
	if len(call.args) != len(wantArgs) {
		t.Fatalf("query args = %v", call.args)
	}
	for index := range wantArgs {
		if call.args[index] != wantArgs[index] {
			t.Fatalf("query arg %d = %v, want %v", index, call.args[index], wantArgs[index])
		}
	}
}

func TestWorkloadSessionsRepositoryFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		binding workloadauth.SessionBinding
		row     scriptedRow
		want    error
		queries int
	}{
		{
			name: "invalid binding",
			binding: workloadauth.SessionBinding{
				WorkloadIdentity:  "spiffe://elitea.example/runtime/python-worker",
				WorkloadSessionID: "session\nattacker",
				ProducerID:        "producer-1",
			},
			want: workloadauth.ErrWorkloadUnauthorized,
		},
		{
			name: "inactive",
			binding: workloadauth.SessionBinding{
				WorkloadIdentity:  "spiffe://elitea.example/runtime/python-worker",
				WorkloadSessionID: "session-1",
				ProducerID:        "producer-1",
			},
			row:     scriptedRow{values: []any{false}},
			want:    workloadauth.ErrWorkloadUnauthorized,
			queries: 1,
		},
		{
			name: "cancellation",
			binding: workloadauth.SessionBinding{
				WorkloadIdentity:  "spiffe://elitea.example/runtime/python-worker",
				WorkloadSessionID: "session-1",
				ProducerID:        "producer-1",
			},
			row:     scriptedRow{err: context.Canceled},
			want:    context.Canceled,
			queries: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &scriptedExecutor{}
			if test.queries != 0 {
				store.rowResults = []scriptedRow{test.row}
			}
			repository, err := newWorkloadSessionsRepository(store)
			if err != nil {
				t.Fatal(err)
			}
			err = repository.VerifyActiveSession(context.Background(), test.binding)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if len(store.rowCalls) != test.queries {
				t.Fatalf("query calls = %d, want %d", len(store.rowCalls), test.queries)
			}
		})
	}
}
