package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
	"github.com/elitea-ng/elitea-platform/libs/go/faults/logx"
	code "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codesandbox"
)

// B3: a registration still contended after its bounded retries is answered
// retryable with the registry code in its status, and writes one WARN fault
// line; a refusal stays a refusal.
func TestAContendedCodeRegistrationIsRetryableAndLoggedOnce(t *testing.T) {
	var out bytes.Buffer
	server := &ContentServer{logger: slog.New(logx.NewHandler(&out, logx.Options{}))}
	claim := ContentClaim{ExecutionID: "0123456789abcdef0123456789abcdef", Generation: 1}
	contended := fmt.Errorf("%w after 3 attempts: %w", code.ErrRegistrationContended, errors.New("SQLSTATE 40001"))
	status, _ := grpcstatus.FromError(server.originalCodeStatus(t.Context(), claim, "visit", contended))
	if status.Code() != codes.Unavailable || !strings.Contains(status.Message(), "("+faults.CodeRegistrationContended.String()+")") {
		t.Fatalf("status = %v", status)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want one fault line, got %q", out.String())
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
		t.Fatal(err)
	}
	if line["level"] != "WARN" || line["code"] != faults.CodeRegistrationContended.String() || line["msg"] != faults.CodeRegistrationContended.Sentence() || line["execution_id"] != claim.ExecutionID || line["registration"] != "visit" {
		t.Fatalf("fault line = %v", line)
	}
	out.Reset()
	refused, _ := grpcstatus.FromError(server.originalCodeStatus(t.Context(), claim, "intent", code.ErrRejected))
	if refused.Code() != codes.Aborted || out.Len() != 0 {
		t.Fatalf("a refusal changed: %v %q", refused, out.String())
	}
}
