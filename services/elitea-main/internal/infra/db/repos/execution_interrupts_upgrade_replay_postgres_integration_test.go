package repos

// FU-268: a pipeline sensitive-tool fan-out card raised by a Worker from
// before the approval-rule release (#1540) is raised again by the upgraded
// Worker on a restore or a wake. Requires ELITEA_TEST_DATABASE_URL; it skips
// without it.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
)

// The cards one Worker test pause renders as (`interrupt_card_v1`, exactly
// as each Worker serialised it): `pre-1540` by the Worker before #1540,
// `1540` by #1540's Worker, `fu268` by the fixed Worker.
func upgradeCard(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "fu268", "tool-guard-card-"+name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestRaiseReplaysAToolGuardCardRaisedBeforeTheApprovalRuleRelease(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	first, err := h.repo.Raise(t.Context(), h.raiseInput(response, upgradeCard(t, "pre-1540")))
	if err != nil || first.Replay {
		t.Fatalf("raise by the earlier Worker = %+v, %v", first, err)
	}
	// #1540's projection changed the card's payload digest: Main refuses it,
	// and a parked run spanning the upgrade would stop.
	if _, err := h.repo.Raise(t.Context(), h.raiseInput(response, upgradeCard(t, "1540"))); !errors.Is(err, domain.ErrRaiseConflict) {
		t.Fatalf("raise by #1540's Worker: %v, want ErrRaiseConflict", err)
	}
	// The fixed Worker raises it again compatibly (only the tolerated
	// display argument digest is added): a replay, nothing new.
	again, err := h.repo.Raise(t.Context(), h.raiseInput(response, upgradeCard(t, "fu268")))
	if err != nil || !again.Replay || again.InterruptKey != first.InterruptKey {
		t.Fatalf("raise by the fixed Worker = %+v, %v", again, err)
	}
	if got := h.row(response, first.InterruptKey); got.state != "PENDING" || got.revision != 1 {
		t.Fatalf("row after the re-raise = %+v", got)
	}
	// The run resumes: the card is decided once.
	decided, err := h.decide(interruptOwner, response, first.InterruptKey, "tab-1", "approve", "")
	if err != nil || decided.State != domain.StateDecided || decided.Revision != 2 {
		t.Fatalf("decide after the upgrade = %+v, %v", decided, err)
	}
}
