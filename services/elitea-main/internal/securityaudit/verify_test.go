package securityaudit

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func envLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestRunVerifyRefusesInvalidUsage(t *testing.T) {
	withURL := envLookup(map[string]string{"DATABASE_URL": "postgres://u:p@h/db"})
	cases := map[string]struct {
		args   []string
		lookup func(string) (string, bool)
	}{
		"no tenant":     {nil, withURL},
		"bad from":      {[]string{"--tenant", "1", "--from", "0"}, withURL},
		"bad source":    {[]string{"--tenant", "1", "--source", "x"}, withURL},
		"extra arg":     {[]string{"--tenant", "1", "x"}, withURL},
		"no url":        {[]string{"--tenant", "1"}, envLookup(nil)},
		"unknown flag":  {[]string{"--tenant", "1", "--fix"}, withURL},
		"negative":      {[]string{"--tenant", "-2"}, withURL},
		"nil lookup":    {[]string{"--tenant", "1"}, nil},
		"tenant not id": {[]string{"--tenant", "abc"}, withURL},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			connect := func(context.Context, string) (VerifyDB, error) {
				t.Fatal("connected on invalid usage")
				return nil, nil
			}
			var stdout, stderr bytes.Buffer
			if code := runVerify(context.Background(), tc.args, tc.lookup, &stdout, &stderr, connect); code != VerifyExitInvalidUsage {
				t.Fatalf("exit %d, want %d; stderr %s", code, VerifyExitInvalidUsage, stderr.String())
			}
		})
	}
}

func TestRunVerifyDoesNotEchoTheDSN(t *testing.T) {
	connect := func(context.Context, string) (VerifyDB, error) {
		return nil, errors.New("dial postgres://u:hunter2@h/db failed")
	}
	var stdout, stderr bytes.Buffer
	code := runVerify(context.Background(), []string{"--tenant", "1"},
		envLookup(map[string]string{"DATABASE_URL": "postgres://u:hunter2@h/db"}), &stdout, &stderr, connect)
	if code != VerifyExitFailure {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(stderr.String()+stdout.String(), "hunter2") {
		t.Fatalf("output echoes the DSN: %s", stderr.String())
	}
}

func TestNewRelayRefusesAMissingThreshold(t *testing.T) {
	if _, err := NewRelay(nil, RelayConfig{BacklogAlertThreshold: 1}); err == nil {
		t.Error("nil database accepted")
	}
	if _, err := NewRelay(struct{ TxStarter }{}, RelayConfig{}); err == nil {
		t.Error("zero threshold accepted")
	}
	if _, err := NewRelay(struct{ TxStarter }{}, RelayConfig{BacklogAlertThreshold: 1, BatchSize: maxRelayBatchSize + 1}); err == nil {
		t.Error("oversized batch accepted")
	}
	relay, err := NewRelay(struct{ TxStarter }{}, RelayConfig{BacklogAlertThreshold: 10})
	if err != nil {
		t.Fatal(err)
	}
	if relay.batch != DefaultRelayBatchSize || relay.interval != DefaultRelayInterval {
		t.Fatalf("defaults not applied: %+v", relay)
	}
	relay.report(context.Background(), 11)
	if !relay.Alerting() {
		t.Error("backlog above the threshold does not alert")
	}
	relay.report(context.Background(), 10)
	if relay.Alerting() {
		t.Error("backlog at the threshold still alerts")
	}
}

func TestStageLockKeysDiffer(t *testing.T) {
	if stageLockKey("store") == stageLockKey("export") {
		t.Fatal("the two relay stages share one advisory lock")
	}
}
