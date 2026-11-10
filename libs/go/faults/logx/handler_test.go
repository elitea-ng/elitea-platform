package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
)

func oneLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("line %q: %v", buf.String(), err)
	}
	return entry
}

func TestErrKeepsItsStringAndGainsCodeCauseAndSupportRef(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Options{})
	ctx := faults.WithSupportRef(context.Background(), "sr-0123456789abcdef")
	fault := faults.New(faults.ToolkitSettingsUnresolved, fmt.Errorf("resolve: %w", errors.New("toolkit settings resolution is unavailable")), slog.Int("toolkit_id", 104))
	faults.Log(ctx, logger, slog.LevelError, fault)
	line := oneLine(t, &buf)
	if line["msg"] != faults.ToolkitSettingsUnresolved.Sentence() || line["code"] != "toolkit.settings_unresolved" {
		t.Fatalf("line = %v", line)
	}
	if line["err"] != fault.Error() {
		t.Fatalf("err = %v, want today's string %q", line["err"], fault.Error())
	}
	if line["support_ref"] != "sr-0123456789abcdef" || line["toolkit_id"] != float64(104) || line["action"] == "" {
		t.Fatalf("line = %v", line)
	}
	cause, _ := line["cause"].([]any)
	if len(cause) != 3 {
		t.Fatalf("cause = %v", line["cause"])
	}
}

func TestAPlainErrorLineIsUnchangedExceptForItsChain(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Options{})
	logger.Error("index registry maintenance failed", "err", errors.New("flat"))
	line := oneLine(t, &buf)
	if line["err"] != "flat" || line["code"] != nil || line["cause"] != nil || line["support_ref"] != nil {
		t.Fatalf("a flat error line changed: %v", line)
	}
}

func TestLevelFromEnvKeepsEachServicesVariable(t *testing.T) {
	env := map[string]string{"GATEWAY_LOG_LEVEL": "debug"}
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }
	if got := LevelFromEnv(lookup, "GATEWAY_LOG_LEVEL", "ELITEA_LOG_LEVEL"); got != slog.LevelDebug {
		t.Fatalf("gateway level = %v", got)
	}
	if got := LevelFromEnv(lookup, "ELITEA_LOG_LEVEL"); got != slog.LevelInfo {
		t.Fatalf("unset level = %v, want Info (unchanged)", got)
	}
	env["ELITEA_LOG_LEVEL"] = "WARN"
	if got := LevelFromEnv(lookup, "ELITEA_LOG_LEVEL"); got != slog.LevelWarn {
		t.Fatalf("level = %v", got)
	}
	if !strings.Contains(slog.LevelInfo.String(), "INFO") {
		t.Fatal("unreachable")
	}
}
