package identity

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// The repository error behind ErrProvisioningFailed used to be discarded with
// nothing logged, so a first sign-in that failed on a missing table surfaced
// as a bare 503 with no trace of the SQLSTATE anywhere. The returned error
// stays generic; the cause must reach the server log.
func TestProvisionLogsTheRepositoryCause(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	const cause = `relation "public.example_missing" does not exist (SQLSTATE 42P01)`
	service := mustProvisionService(t, &repositoryStub{err: errors.New(cause)})
	_, err := service.Provision(context.Background(), ProvisionRequest{Assertion: validAssertion(nil)})
	if !errors.Is(err, ErrProvisioningFailed) {
		t.Fatalf("error = %v, want %v", err, ErrProvisioningFailed)
	}
	if strings.Contains(err.Error(), "example_missing") {
		t.Fatalf("returned error leaked the cause: %v", err)
	}
	if !strings.Contains(logs.String(), "example_missing") {
		t.Fatalf("server log does not carry the repository cause:\n%s", logs.String())
	}
}
