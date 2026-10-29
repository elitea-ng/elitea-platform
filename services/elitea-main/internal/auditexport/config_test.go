package auditexport

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	otellog "go.opentelemetry.io/otel/log"

	"github.com/elitea-ng/elitea-platform/libs/go/observability"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

func env(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { v, ok := values[name]; return v, ok }
}

type fakeLogs struct {
	entries []observability.LogEntry
	err     error
}

func (f *fakeLogs) Export(_ context.Context, entries []observability.LogEntry) error {
	if f.err != nil {
		return f.err
	}
	f.entries = append(f.entries, entries...)
	return nil
}

func TestOTLPSinkSendsEnvelopeAsBodyAndFlatAttributes(t *testing.T) {
	logs := &fakeLogs{}
	sink, err := NewOTLPSink(logs)
	if err != nil {
		t.Fatal(err)
	}
	deny, err := FromSecurityRecord(securityRecord(t, securityaudit.TypeDecision, func(e *securityaudit.SecurityEvent) { e.Effect = securityaudit.EffectDeny }))
	if err != nil {
		t.Fatal(err)
	}
	api := FromAuditRow(AuditRow{ID: 3, Timestamp: time.Now(), UserID: ptr(int64(1)), EventType: "api", Action: "GET /x"})
	if err := sink.Send(context.Background(), []Event{deny, api}); err != nil {
		t.Fatal(err)
	}
	if len(logs.entries) != 2 {
		t.Fatalf("entries = %d", len(logs.entries))
	}
	first := logs.entries[0]
	var body Event
	if err := json.Unmarshal([]byte(first.Body), &body); err != nil || body.EventID != deny.EventID || body.Schema != SchemaVersion {
		t.Fatalf("the whole envelope is the body: %v %q", err, first.Body)
	}
	if first.Severity != otellog.SeverityWarn || logs.entries[1].Severity != otellog.SeverityInfo {
		t.Fatalf("a denied decision is a warning: %v / %v", first.Severity, logs.entries[1].Severity)
	}
	for key, want := range map[string]string{
		"elitea.audit.event_id": deny.EventID, "elitea.audit.source": "security_audit", "elitea.audit.outcome": "deny",
		"elitea.audit.tenant.project_id": "42", "elitea.audit.ocsf.class_uid": "3003",
	} {
		if first.Attributes[key] != want {
			t.Fatalf("attribute %s = %q, want %q", key, first.Attributes[key], want)
		}
	}
	if first.EventName != EventName || !first.Time.Equal(deny.parsedTime(t)) {
		t.Fatalf("event name / time: %q %v", first.EventName, first.Time)
	}

	logs.err = errors.New("collector down")
	if err := sink.Send(context.Background(), []Event{api}); err == nil {
		t.Fatal("a collector failure must reach the relay")
	}
	var permanent *PermanentError
	if errors.As(sink.Send(context.Background(), []Event{api}), &permanent) {
		t.Fatal("OTLP failures are never permanent")
	}
}

func (e Event) parsedTime(t *testing.T) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, e.Time)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestParseKeyFile(t *testing.T) {
	good := `{"keys":[{"id":"k1","secret":"` + strings.Repeat("a", 32) + `","created":"2026-01-01T00:00:00Z"},
	                  {"id":"k2","secret":"` + strings.Repeat("b", 40) + `","created":"2026-06-01T00:00:00Z"}]}`
	keys, err := ParseKeyFile([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if newest, _ := NewestKey(keys); newest.ID != "k2" {
		t.Fatalf("newest = %s", newest.ID)
	}
	secret := strings.Repeat("s", 32)
	bad := map[string]string{
		"empty":        `{"keys":[]}`,
		"three":        `{"keys":[{"id":"a","secret":"` + secret + `","created":"2026-01-01T00:00:00Z"},{"id":"b","secret":"` + secret + `","created":"2026-01-02T00:00:00Z"},{"id":"c","secret":"` + secret + `","created":"2026-01-03T00:00:00Z"}]}`,
		"short secret": `{"keys":[{"id":"a","secret":"short","created":"2026-01-01T00:00:00Z"}]}`,
		"bad id":       `{"keys":[{"id":"a b","secret":"` + secret + `","created":"2026-01-01T00:00:00Z"}]}`,
		"no created":   `{"keys":[{"id":"a","secret":"` + secret + `"}]}`,
		"duplicate":    `{"keys":[{"id":"a","secret":"` + secret + `","created":"2026-01-01T00:00:00Z"},{"id":"a","secret":"` + secret + `","created":"2026-01-02T00:00:00Z"}]}`,
		"same time":    `{"keys":[{"id":"a","secret":"` + secret + `","created":"2026-01-01T00:00:00Z"},{"id":"b","secret":"` + secret + `","created":"2026-01-01T00:00:00Z"}]}`,
		"unknown":      `{"keys":[],"extra":1}`,
	}
	for name, raw := range bad {
		if _, err := ParseKeyFile([]byte(raw)); err == nil {
			t.Fatalf("%s: accepted", name)
		} else if strings.Contains(err.Error(), secret) {
			t.Fatalf("%s: error leaks the secret", name)
		}
	}
}

func TestFileKeySourceReloadsAndSurvivesAReadError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	write := func(id string) {
		raw := `{"keys":[{"id":"` + id + `","secret":"` + strings.Repeat("x", 32) + `","created":"2026-01-01T00:00:00Z"}]}`
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("one")
	clock := time.Now()
	source, err := FileKeySource(path, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := source()
	if keys[0].ID != "one" {
		t.Fatal("initial key")
	}
	write("two")
	if keys, _ = source(); keys[0].ID != "one" {
		t.Fatal("reloaded before the interval")
	}
	clock = clock.Add(keyReloadInterval + time.Second)
	if keys, _ = source(); keys[0].ID != "two" {
		t.Fatal("rotated key not picked up without a restart")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(keyReloadInterval + time.Second)
	if keys, err = source(); err != nil || keys[0].ID != "two" {
		t.Fatal("a vanished file must keep the last good keys")
	}
	if _, err := FileKeySource(filepath.Join(dir, "missing.json"), nil); err == nil {
		t.Fatal("a missing key file must refuse boot")
	}
}

func TestConfigFromEnv(t *testing.T) {
	off, err := ConfigFromEnv(env(map[string]string{EnvBatchSize: "garbage"}))
	if err != nil || off.Enabled() {
		t.Fatalf("with no endpoint nothing else is read: %v %+v", err, off)
	}

	cfg, err := ConfigFromEnv(env(map[string]string{
		EnvOTLPEndpoint: "http://collector:4318", EnvWebhookURL: "https://siem.example.com/hook",
		EnvWebhookKeysFile: "/etc/elitea/audit-export/webhook-keys.json",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled() || cfg.OTLPProtocol != observability.LogsProtocolHTTP || !cfg.AuditEvents || cfg.AuditEventsFromBeginning ||
		cfg.BatchSize != DefaultBatchSize || cfg.Interval != DefaultInterval || cfg.MaxPermanentAttempts != DefaultMaxPermanentAttempts {
		t.Fatalf("defaults: %+v", cfg)
	}

	bad := map[string]map[string]string{
		"webhook without keys":  {EnvWebhookURL: "https://siem.example.com/hook"},
		"keys without webhook":  {EnvOTLPEndpoint: "http://c:4318", EnvWebhookKeysFile: "/k.json"},
		"relative keys path":    {EnvWebhookURL: "https://s/h", EnvWebhookKeysFile: "k.json"},
		"http without allow":    {EnvWebhookURL: "http://siem/h", EnvWebhookKeysFile: "/k.json"},
		"url credentials":       {EnvWebhookURL: "https://u:p@siem/h", EnvWebhookKeysFile: "/k.json"},
		"protocol w/o endpoint": {EnvWebhookURL: "https://s/h", EnvWebhookKeysFile: "/k.json", EnvOTLPProtocol: "grpc"},
		"bad protocol":          {EnvOTLPEndpoint: "http://c", EnvOTLPProtocol: "udp"},
		"http endpoint no url":  {EnvOTLPEndpoint: "collector:4318"},
		"bad from":              {EnvOTLPEndpoint: "http://c", EnvAuditEventsFrom: "yesterday"},
		"bad batch":             {EnvOTLPEndpoint: "http://c", EnvBatchSize: "0"},
		"bad interval":          {EnvOTLPEndpoint: "http://c", EnvInterval: "1ms"},
		"bad attempts":          {EnvOTLPEndpoint: "http://c", EnvMaxPermanent: "0"},
	}
	for name, values := range bad {
		if _, err := ConfigFromEnv(env(values)); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if _, err := ConfigFromEnv(env(map[string]string{
		EnvWebhookURL: "http://10.0.0.5:8080/h", EnvWebhookKeysFile: "/k.json", EnvEgressAllowlist: "10.0.0.5",
	})); err != nil {
		t.Fatalf("http to an allowlisted receiver: %v", err)
	}
	grpc, err := ConfigFromEnv(env(map[string]string{EnvOTLPEndpoint: "collector:4317", EnvOTLPProtocol: "grpc", EnvAuditEvents: "false", EnvAuditEventsFrom: "beginning"}))
	if err != nil || grpc.OTLPProtocol != observability.LogsProtocolGRPC || grpc.AuditEvents || !grpc.AuditEventsFromBeginning {
		t.Fatalf("grpc: %v %+v", err, grpc)
	}
}

// Step record retention waits only for the sinks exporting run_steps now.
func TestRunStepsSinks(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  Config
		want []string
	}{
		"export off":   {Config{RunSteps: true}, []string{}},
		"stream off":   {Config{OTLPEndpoint: "collector:4317", WebhookURL: "https://siem", RunSteps: false}, []string{}},
		"both sinks":   {Config{OTLPEndpoint: "collector:4317", WebhookURL: "https://siem", RunSteps: true}, []string{"otlp", "webhook"}},
		"webhook only": {Config{WebhookURL: "https://siem", RunSteps: true}, []string{"webhook"}},
	} {
		if got := tc.cfg.RunStepsSinks(); strings.Join(got, ",") != strings.Join(tc.want, ",") || got == nil {
			t.Fatalf("%s: %v", name, got)
		}
	}
}
