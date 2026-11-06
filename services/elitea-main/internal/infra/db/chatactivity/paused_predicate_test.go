package chatactivity

import (
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/responsepause"
)

// FU-284: "needs you" counts every wait for a person, a parked fan-out
// included, and keeps the partial index's text as its first arm.
func TestPausedPredicateNamesEveryWaitingKey(t *testing.T) {
	predicate := PausedPredicate()
	for _, key := range responsepause.WaitingKeys() {
		if !strings.Contains(predicate, "g.meta ? '"+key+"'") {
			t.Errorf("the paused predicate misses %s", key)
		}
	}
	const indexed = `(NOT g.is_streaming AND (g.meta ? 'hitl_interrupt' OR g.meta ? 'hitl_interrupts'
		OR g.meta ? 'authorization_requests' OR g.meta->'output_limit_reached' = 'true'::jsonb
		OR g.meta ? 'pipeline_static_v1' OR g.meta ? 'node_recovery_required_v1'))`
	if !strings.Contains(predicate, indexed) {
		t.Error("the paused predicate no longer carries chat_message_group_paused_idx's text")
	}
}
