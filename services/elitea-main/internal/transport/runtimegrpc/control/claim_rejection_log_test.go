package control

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// A refused claim must say why in Main's log: the worker only sees a safe code.
func TestClaimRejectionLogsTheReasonWithoutPayloads(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	command := &runtimev1.WorkerCommandV1{ExecutionId: "execution-1", Generation: 2, CommandId: "command-1"}
	logClaimRejection(context.Background(), "claim", command, errors.Join(runtimedomain.ErrStaleFence))
	line := out.String()
	for _, want := range []string{"runtime claim rejected", "stage=claim", "STALE_FENCE", "execution_id=execution-1", "generation=2", "command_id=command-1"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line lacks %q: %s", want, line)
		}
	}
}
