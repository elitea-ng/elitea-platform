package main

// `identity.initial_global_admins` is applied at a first login that can happen
// days after this boot, and a wrong entry produces silence: the person signs in
// and simply is not an administrator. The start-up line is the only place the
// operator can see the difference between a wrong VALUE and a wrong SHAPE, so
// what it reports is asserted here.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// captureLog returns one decoded record per line the helper emitted.
func captureLog(t *testing.T, admins []string) []map[string]any {
	t.Helper()
	var buffer bytes.Buffer
	logInitialGlobalAdminShapes(
		slog.New(slog.NewJSONHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelInfo})),
		admins)

	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buffer.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func TestLogInitialGlobalAdminShapesCountsBothShapes(t *testing.T) {
	records := captureLog(t, []string{
		"oidc:alice-sub",
		"saml:bob@corp.com",
		"email:carol@corp.com",
	})
	if len(records) != 1 {
		t.Fatalf("emitted %d records, want 1: %v", len(records), records)
	}
	if level := records[0]["level"]; level != "INFO" {
		t.Fatalf("level = %v, want INFO", level)
	}
	for key, want := range map[string]float64{
		"total": 3, "provider_references": 2, "verified_emails": 1,
	} {
		if got := records[0][key]; got != want {
			t.Fatalf("%s = %v, want %v", key, got, want)
		}
	}
}

// A malformed entry WARNS. It must not stop the boot: this value only changes
// what a first login receives, and refusing to start would turn one bad list
// entry into an outage of a running deployment.
func TestLogInitialGlobalAdminShapesWarnsOnAnEntryThatMatchesNothing(t *testing.T) {
	records := captureLog(t, []string{"oidc:alice-sub", "email:not-an-address"})
	if len(records) != 2 {
		t.Fatalf("emitted %d records, want an info line and a warning: %v", len(records), records)
	}
	if level := records[1]["level"]; level != "WARN" {
		t.Fatalf("level = %v, want WARN", level)
	}
	entries, ok := records[1]["entries"].([]any)
	if !ok || len(entries) != 1 || entries[0] != "email:not-an-address" {
		t.Fatalf("entries = %v, want the malformed entry named", records[1]["entries"])
	}
}

// An unconfigured deployment is the common case and is not worth a line.
func TestLogInitialGlobalAdminShapesSaysNothingWhenUnconfigured(t *testing.T) {
	if records := captureLog(t, nil); len(records) != 0 {
		t.Fatalf("emitted %v for an empty list", records)
	}
}
