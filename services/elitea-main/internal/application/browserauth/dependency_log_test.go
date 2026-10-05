package browserauth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
)

// captureDefaultLog swaps the process-wide slog logger for one that writes to
// a buffer. Callers must not be parallel: the default logger is global.
func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buffer
}

// A dependency failure answers the browser with a generic sentinel, and that
// sentinel used to be the only thing left of it: the cause was dropped and
// nothing was logged, so a first-login 503 gave an operator no clue at all.
// The response must stay generic; the server log must carry the cause.
func TestDependencyFailureCauseIsLoggedNotReturned(t *testing.T) {
	logs := captureDefaultLog(t)

	const cause = `relation "public.example_missing" does not exist (SQLSTATE 42P01)`
	service, _, _, provisioner, _, clock := newTestService(t)
	provisioner.err = errors.New(cause)
	correlation := browserflow.ProtocolCorrelation{Nonce: "nonce-1"}
	begin := beginFlow(t, service, "oidc", correlation)
	_, err := service.Complete(context.Background(), CompleteRequest{
		SessionID:     begin.SessionID,
		TransactionID: begin.TransactionID,
		Provider:      "oidc",
	}, &assertionVerifierStub{assertion: validAssertion(clock.Now(), "oidc", correlation)})
	if !errors.Is(err, ErrDependencyUnavailable) {
		t.Fatalf("error = %v, want %v", err, ErrDependencyUnavailable)
	}
	if strings.Contains(err.Error(), "SQLSTATE") || strings.Contains(err.Error(), "example_missing") {
		t.Fatalf("returned error leaked the cause: %v", err)
	}
	logged := logs.String()
	if !strings.Contains(logged, "example_missing") || !strings.Contains(logged, "provision authenticated identity") {
		t.Fatalf("server log does not carry the operation and cause:\n%s", logged)
	}
}

// A rejected credential is an expected outcome, not a server fault: it is not
// logged as an error, so a stream of wrong passwords cannot flood the log.
func TestRejectedCredentialIsNotLoggedAsDependencyFailure(t *testing.T) {
	logs := captureDefaultLog(t)

	service, _, _, _, _, _ := newTestService(t)
	correlation := browserflow.ProtocolCorrelation{Nonce: "nonce-1"}
	begin := beginFlow(t, service, "oidc", correlation)
	_, err := service.Complete(context.Background(), CompleteRequest{
		SessionID:     begin.SessionID,
		TransactionID: begin.TransactionID,
		Provider:      "oidc",
	}, &assertionVerifierStub{err: errors.New("wrong password")})
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("error = %v, want %v", err, ErrUnauthenticated)
	}
	if strings.Contains(logs.String(), "wrong password") {
		t.Fatalf("rejected credential was logged as a failure:\n%s", logs.String())
	}
}
