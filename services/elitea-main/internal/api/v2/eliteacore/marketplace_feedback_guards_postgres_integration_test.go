package eliteacore_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

// TestReviewGuardsAndRestrictedFlags covers the review findings: a member of
// the publishing project cannot review its own listing, a publisher cannot
// flag what they may not see or clear an administrator's flag, and a dismissed
// hire does not keep a withdrawn snapshot alive.
func TestReviewGuardsAndRestrictedFlags(t *testing.T) {
	f := newFeedbackFixture(t)
	ctx := context.Background()
	f.hire(2, 900, f.v1) // the publishing project hires its own listing
	if status, body := f.post(14, 2, "marketplace_feedback", map[string]any{"rating": 5}); status != http.StatusForbidden || body["code"] != "own_listing" {
		t.Errorf("a member of the publishing project = %d %v, want 403 own_listing", status, body)
	}

	f.hire(3, 501, f.v1)
	status, body := f.post(11, 3, "marketplace_feedback", map[string]any{"rating": 2})
	if status != http.StatusCreated {
		t.Fatalf("review = %d %v", status, body)
	}
	id := body["id"].(string)
	act := func(user, project int, action string) int {
		s, _ := f.do(f.router(user), http.MethodPost,
			fmt.Sprintf("/elitea_core/marketplace_feedback_action/prompt_lib/%d/%s/%s", project, f.listing, id), map[string]any{"action": action})
		return s
	}
	// An administrator flags; the publisher cannot clear that flag.
	if s := act(moderatorUser, 4, "flag"); s != http.StatusOK {
		t.Fatalf("administrator flag = %d", s)
	}
	if s := act(7, 2, "unflag"); s != http.StatusOK {
		t.Errorf("publisher unflag of another's flag = %d, want 200 and no change", s)
	}
	var flagged bool
	if err := f.pool.QueryRow(ctx, `SELECT flagged_at IS NOT NULL FROM elitea_marketplace.listing_feedback WHERE id = $1`, id).Scan(&flagged); err != nil || !flagged {
		t.Errorf("the administrator's flag was cleared by the publisher (%v)", err)
	}
	// A hidden item looks missing to the publisher.
	if s := act(moderatorUser, 4, "hide"); s != http.StatusOK {
		t.Fatalf("hide = %d", s)
	}
	if s := act(7, 2, "flag"); s != http.StatusNotFound {
		t.Errorf("publisher flag of a hidden item = %d, want 404", s)
	}
	if s := act(moderatorUser, 4, "unhide"); s != http.StatusOK {
		t.Fatalf("unhide = %d", s)
	}

	// A withdrawn snapshot whose every hire was dismissed is purged, with its feedback.
	if _, err := f.pool.Exec(ctx, `UPDATE elitea_marketplace.installs SET dismissed_at = clock_timestamp() WHERE listing_id = $1 AND version_id = $2`, f.listingN, f.v1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE elitea_marketplace.listing_versions SET withdrawn_at = clock_timestamp() - interval '60 days' WHERE listing_id = $1 AND version_id = $2`, f.listingN, f.v1); err != nil {
		t.Fatal(err)
	}
	removed, err := marketplace.PurgeUnheld(ctx, f.pool, marketplace.SnapshotRetentionDays)
	if err != nil || removed != 1 {
		t.Errorf("purge = %d (%v), want the one dismissed snapshot", removed, err)
	}
	if n := f.countRows(`SELECT count(*) FROM elitea_marketplace.listing_versions WHERE listing_id = $1`, f.listingN); n != 1 {
		t.Errorf("versions left = %d, want only v2", n)
	}
}
