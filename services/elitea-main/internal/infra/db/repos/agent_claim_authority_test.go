package repos

import (
	"slices"
	"strings"
	"testing"
)

// TestAgentClaimAuthorityIsSharedAndModeBound pins FU-063: node recovery
// control and the interrupt ledger use one live-claim query that differs only
// by the claim's recovery mode and the accepted desired states.
func TestAgentClaimAuthorityIsSharedAndModeBound(t *testing.T) {
	for _, predicate := range []string{
		"c.recovery_mode = ANY($6::text[])", "j.desired_state = ANY($7::text[])", "c.released_at IS NULL",
		"c.lease_expires_at > clock_timestamp()", "ws.revoked_at IS NULL", "command.deadline > clock_timestamp()",
		"command.authority_granted_at IS NOT NULL", "NOT EXISTS (SELECT 1 FROM elitea_runtime.output_inbox terminal",
		"FOR UPDATE OF j, c FOR SHARE OF ws, command",
	} {
		if !strings.Contains(agentClaimAuthoritySQL, predicate) {
			t.Errorf("shared claim authority lost %q", predicate)
		}
	}
	// An ordinary claim, first or reclaimed with model-checkpoint recovery; never node recovery.
	if !slices.Equal(interruptClaimMode.recoveryModes, []string{"NONE", "AGENT_MODEL_CHECKPOINT"}) || !slices.Equal(interruptClaimMode.desiredStates, []string{"RUNNING"}) {
		t.Errorf("interrupt routes must accept only ordinary RUNNING claims: %+v", interruptClaimMode)
	}
	if !slices.Equal(nodeRecoveryClaimMode.recoveryModes, []string{"NODE_RECOVERY"}) || !slices.Equal(nodeRecoveryClaimMode.desiredStates, []string{"SUSPENDED", "RUNNING"}) {
		t.Errorf("node recovery control mode changed: %+v", nodeRecoveryClaimMode)
	}
}
