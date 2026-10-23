package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/config"
)

// expectedReplicasEnv names the replica count that the degraded-mode budget
// allowance is divided by (failmode.PerReplicaShare).
const expectedReplicasEnv = "LLM_BUDGET_EXPECTED_REPLICAS"

// errExpectedReplicas marks a set LLM_BUDGET_EXPECTED_REPLICAS that is not a
// positive integer.
var errExpectedReplicas = errors.New(expectedReplicasEnv + " must be a positive integer")

// expectedReplicas reads LLM_BUDGET_EXPECTED_REPLICAS STRICTLY. The gateway
// refuses to start on an error (human decision, DECISIONS.md 2026-10-22).
//
// Unset or empty gives config.DefaultNATSReplicas (1). A set value must be an
// integer of 1 or more. config.FromEnv reads 0, a negative number or text as
// the default 1. That silent fallback is not safe here: each replica would
// then spend the WHOLE degraded allowance, and a fleet of N replicas would
// overspend by N times. So main refuses such a value before it builds the
// budget engine.
func expectedReplicas(lookup func(string) (string, bool)) (int, error) {
	raw, ok := lookup(expectedReplicasEnv)
	raw = strings.TrimSpace(raw)
	if !ok || raw == "" {
		return config.DefaultNATSReplicas, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: got %q. Set it to the most gateway replicas that can run "+
			"(the autoscaler's maximum, else the replica count)", errExpectedReplicas, raw)
	}
	if n < 1 {
		return 0, fmt.Errorf("%w: got %d. The degraded-mode budget cap is divided by it, "+
			"so set it to the most gateway replicas that can run", errExpectedReplicas, n)
	}
	return n, nil
}
