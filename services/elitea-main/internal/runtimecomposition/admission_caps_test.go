package runtimecomposition

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

func TestReportAdmissionCapsStatesConfiguredAndPersistedPerCapability(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&out, nil))
	reportAdmissionCaps(logger, 2048, []repos.AdmissionPolicy{
		{CapabilityID: "agent.execute.v1", MaxOutstanding: 2048},
		{CapabilityID: "index.ingest.v1", MaxOutstanding: 1024},
	})
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one line per capability, got %q", out.String())
	}
	if !strings.Contains(lines[0], "level=INFO") || !strings.Contains(lines[0], "capability=agent.execute.v1 configured=2048 persisted=2048") {
		t.Fatalf("matching cap line: %s", lines[0])
	}
	for _, want := range []string{"level=ERROR", "MISMATCH", "capability=index.ingest.v1 configured=2048 persisted=1024", "elitea-admission-ctl set -max"} {
		if !strings.Contains(lines[1], want) {
			t.Fatalf("mismatch line lacks %q: %s", want, lines[1])
		}
	}

	out.Reset()
	reportAdmissionCaps(logger, 64, nil)
	if !strings.Contains(out.String(), "configured=64") || !strings.Contains(out.String(), "no capability has a persisted cap") {
		t.Fatalf("empty policy table line: %s", out.String())
	}
}
