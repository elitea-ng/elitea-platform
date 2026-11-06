package chatactivity

import (
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/responsepause"
)

// FU-284: "needs you" counts every wait for a person, a parked fan-out
// included. Load's predicate is built from responsepause; Paused reads one
// arm per partial index, each in its index's exact text.
func TestPausedPredicateNamesEveryWaitingKey(t *testing.T) {
	predicate := PausedPredicate()
	if !strings.Contains(predicate, responsepause.SQL(responsepause.Group)) {
		t.Errorf("the paused predicate is not built from responsepause: %s", predicate)
	}
	query := pausedQuery(`"p_1"`)
	for _, key := range responsepause.WaitingKeys() {
		if !strings.Contains(predicate, "'"+key+"'") || !strings.Contains(query, "g.meta ? '"+key+"'") {
			t.Errorf("%s is missing from the paused predicate or query", key)
		}
	}
	for _, arm := range []string{pausedIndexedArm, parkedIndexedArm} {
		if !strings.Contains(query, arm) {
			t.Errorf("Paused does not carry the index text %s", arm)
		}
	}
	if !strings.Contains(query, "UNION") {
		t.Error("Paused does not read the two arms separately")
	}
}
