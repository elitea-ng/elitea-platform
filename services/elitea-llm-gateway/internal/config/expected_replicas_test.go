package config

import (
	"strings"
	"testing"
)

// mustFromEnv is FromEnv for tests whose environment is valid.
func mustFromEnv(t *testing.T) Config {
	t.Helper()
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	return cfg
}

// TestExpectedReplicasFromEnv pins the strict parse (human decision,
// DECISIONS.md 2026-10-22). The accepted form is the chart's elitea.integer
// rule with min 1: digits only, no sign, no leading zero.
func TestExpectedReplicasFromEnv(t *testing.T) {
	env := func(v string, ok bool) func(string) (string, bool) {
		return func(k string) (string, bool) {
			if k != "LLM_BUDGET_EXPECTED_REPLICAS" {
				return "", false
			}
			return v, ok
		}
	}
	accepted := []struct {
		v    string
		ok   bool
		want int
	}{
		{"", false, 1},
		{"", true, 1},
		{"  ", true, 1},
		{"1", true, 1},
		{"2", true, 2},
		{"3", true, 3},
		{" 7 ", true, 7},
		{"40", true, 40},
		{"100", true, 100},
	}
	for _, tc := range accepted {
		got, err := ExpectedReplicasFromEnv(env(tc.v, tc.ok))
		if err != nil || got != tc.want {
			t.Errorf("ExpectedReplicasFromEnv(%q, set=%v) = %d, %v; want %d, nil", tc.v, tc.ok, got, err, tc.want)
		}
	}
	refused := []struct {
		v, msg string
	}{
		{"0", "at least 1"},
		{"-1", "at least 1"},
		{"-40", "at least 1"},
		{"+3", "whole number"},
		{"007", "whole number"},
		{"03", "whole number"},
		{"two", "whole number"},
		{"1.5", "whole number"},
		{"3x", "whole number"},
		{"0x2", "whole number"},
		{"1e3", "whole number"},
		{"99999999999999999999999", "out of range"},
	}
	for _, tc := range refused {
		got, err := ExpectedReplicasFromEnv(env(tc.v, true))
		if err == nil {
			t.Errorf("ExpectedReplicasFromEnv(%q) = %d, nil; want a refusal", tc.v, got)
			continue
		}
		if !strings.Contains(err.Error(), tc.msg) || !strings.Contains(err.Error(), "LLM_BUDGET_EXPECTED_REPLICAS") {
			t.Errorf("ExpectedReplicasFromEnv(%q) error %q; want it to name the variable and say %q", tc.v, err, tc.msg)
		}
	}
}

// TestFromEnvRefusesBadExpectedReplicas: FromEnv itself returns the error, so
// main cannot start with a value that would fall back to 1.
func TestFromEnvRefusesBadExpectedReplicas(t *testing.T) {
	for _, bad := range []string{"0", "-2", "+3", "007", "two"} {
		t.Setenv("LLM_BUDGET_EXPECTED_REPLICAS", bad)
		if _, err := FromEnv(); err == nil {
			t.Errorf("FromEnv with LLM_BUDGET_EXPECTED_REPLICAS=%q: want an error", bad)
		}
	}
	t.Setenv("LLM_BUDGET_EXPECTED_REPLICAS", "5")
	if cfg := mustFromEnv(t); cfg.ExpectedReplicas != 5 {
		t.Errorf("ExpectedReplicas = %d, want 5", cfg.ExpectedReplicas)
	}
}

// TestFromEnvDegradedReservation: the per-request reservation defaults to
// 0.10 USD and reads LLM_BUDGET_NATS_DEGRADED_RESERVATION_USD.
func TestFromEnvDegradedReservation(t *testing.T) {
	t.Setenv("LLM_BUDGET_NATS_DEGRADED_RESERVATION_USD", "")
	if got := mustFromEnv(t).NATSDegradedReservationUSD; got != DefaultNATSDegradedReservationUSD {
		t.Errorf("default reservation = %v, want %v", got, DefaultNATSDegradedReservationUSD)
	}
	t.Setenv("LLM_BUDGET_NATS_DEGRADED_RESERVATION_USD", "2.5")
	if got := mustFromEnv(t).NATSDegradedReservationUSD; got != 2.5 {
		t.Errorf("reservation = %v, want 2.5", got)
	}
}
