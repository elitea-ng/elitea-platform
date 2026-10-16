package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

func captureClaimRejection(t *testing.T, stage string, command *runtimev1.WorkerCommandV1, err error) string {
	t.Helper()
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	logClaimRejection(context.Background(), stage, command, err)
	return out.String()
}

// A refused claim must say why in Main's log: the worker only sees a safe code.
func TestClaimRejectionLogsTheReasonWithoutPayloads(t *testing.T) {
	command := &runtimev1.WorkerCommandV1{ExecutionId: "execution-1", Generation: 2, CommandId: "command-1"}
	line := captureClaimRejection(t, "claim", command, errors.Join(runtimedomain.ErrStaleFence))
	for _, want := range []string{"runtime claim rejected", "stage=claim", "STALE_FENCE", "reason=stale_fence", "execution_id=execution-1", "generation=2", "command_id=command-1"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line lacks %q: %s", want, line)
		}
	}
	if strings.Contains(line, "cause=") {
		t.Fatalf("the raw error is still a log attribute: %s", line)
	}
}

// Raw error text can carry driver messages and payload fragments. Neither the
// text around a known sentinel nor the text of an unknown error may reach the log.
func TestClaimRejectionNeverLogsErrorText(t *testing.T) {
	const (
		driverText  = `pq: duplicate key value violates unique constraint "execution_claims_pkey" (token=abc123)`
		payloadText = `payload={"prompt":"my secret question","api_key":"sk-live-0000"}`
	)
	leaked := []string{"duplicate key", "execution_claims_pkey", "token=abc123", "abc123", "my secret question", "sk-live-0000", "payload=", "api_key", driverText}
	cases := []struct {
		name   string
		err    error
		reason string
		code   string
	}{
		{"known sentinel wrapped in driver text", fmt.Errorf("%s: %w: %s", driverText, runtimedomain.ErrStaleFence, payloadText), "reason=stale_fence", "STALE_FENCE"},
		{"known sentinel joined with payload", errors.Join(errors.New(payloadText), ErrCommandAuthentication), "reason=command_authentication", "AUTHENTICATION_FAILED"},
		{"unknown error with driver text", errors.New(driverText), "reason=unclassified", "INTERNAL"},
		{"unknown error with payload text", fmt.Errorf("claim failed: %s: %w", payloadText, errors.New(driverText)), "reason=unclassified", "INTERNAL"},
		{"nil-like empty error", errors.New(""), "reason=unclassified", "INTERNAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := captureClaimRejection(t, "verify_command", nil, tc.err)
			if !strings.Contains(line, tc.reason) || !strings.Contains(line, tc.code) || !strings.Contains(line, "stage=verify_command") {
				t.Fatalf("log line lacks the fixed reason %q or code %q: %s", tc.reason, tc.code, line)
			}
			for _, text := range leaked {
				if strings.Contains(line, text) {
					t.Fatalf("log line leaks %q: %s", text, line)
				}
			}
		})
	}
}

// The reason is a member of a closed set, whatever the error.
func TestClaimRejectionReasonIsAClosedSet(t *testing.T) {
	closed := map[string]bool{
		reasonStaleFence: true, reasonCommandAuthentication: true, reasonCommandIncompatible: true, reasonMalformedRequest: true,
		reasonSettlementConflict: true, reasonTerminalOutputNotReady: true, reasonClaimDependencyUnavailable: true,
		reasonInvalidSettlement: true, reasonCancelled: true, reasonDeadlineExceeded: true, reasonUnclassified: true,
	}
	for _, err := range []error{
		runtimedomain.ErrStaleFence, runtimedomain.ErrLeaseExpired, ErrCommandAuthentication, ErrCommandIncompatible,
		ErrMalformedWorkerCommand, runtimedomain.ErrInvalidFence, context.Canceled, context.DeadlineExceeded,
		errors.New("anything else (secret)"), nil,
	} {
		reason := claimRejectionReason(err)
		if !closed[reason] || strings.Contains(reason, "secret") {
			t.Fatalf("reason %q for %v is outside the closed set", reason, err)
		}
	}
	if claimRejectionReason(errors.New("x")) != "unclassified" || claimRejectionReason(nil) != "unclassified" {
		t.Fatal("unknown errors must be unclassified")
	}
}
