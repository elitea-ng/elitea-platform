package eliteacore_test

// The avatar of an automatic hire (issue 2091). The policy's hire left the
// avatar files uncopied, so the agent it created had no avatar, while the same
// hire made by a person through the wizard had one. It now copies the files as
// the person the policy acts for.

import (
	"context"
	"encoding/json"
	"image/color"
	"net/http"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

func TestAutoHireCopiesTheListingsAvatarAsThePersonThePolicyActsFor(t *testing.T) {
	j := newWorkforceJourney(t)
	ctx := context.Background()

	media := newMediaStore()
	still := putMedia(t, media, "9", color.White)
	thinking := putMedia(t, media, "9", color.Black)
	packBytes, err := json.Marshal(avatarPack(still, thinking))
	if err != nil {
		t.Fatal(err)
	}
	packText := string(packBytes)
	if _, err := j.pool.Exec(ctx, `
UPDATE elitea_marketplace.listing_versions
   SET definition = jsonb_set(definition, '{version,avatar_pack}', $2::jsonb)
 WHERE version_id = $1`, mustID(j.v2), packText); err != nil {
		t.Fatal(err)
	}
	// The files are readable only as the person the policy acts for (user 11),
	// which is what the wizard's hire does with the caller.
	readable := func(ctx context.Context, owner string) bool {
		user, ok := auth.UserFromContext(ctx)
		return ok && user.ID == "11" && owner == "9"
	}
	j.store.WithListingHirer(eliteacore.NewHandler(j.pool,
		eliteacore.WithPublishGovernor(j.lifecycle),
		eliteacore.WithWorkforceManagerCheck(managesAgents),
		eliteacore.WithObjectStore(media),
		eliteacore.WithMediaSourceReadable(readable)))

	for i, user := range []int{11, 12, 13} {
		j.hire(4+i%2, 700+int64(i), j.v2)
		status, body := j.post(user, 4+i%2, "marketplace_feedback", map[string]any{"rating": []int{5, 4, 4}[i], "version_id": j.v2, "body": "Fine."})
		if status != http.StatusCreated {
			t.Fatalf("review by %d = %d %v", user, status, body)
		}
	}
	j.setPolicy(j.person, func(p *agentworkforce.Policy) { p.AutoHireEnabled = true })
	proposed := j.call(j.run, "propose_hire", map[string]any{
		"listing_id": j.listingN, "version": j.v2, "mode": marketplace.ModeInstall, "rationale": "It fits the project."})
	id := proposalID(t, proposed)
	if summary, err := j.store.WorkforcePolicyPass(ctx); err != nil || summary.Hired != 1 {
		t.Fatalf("the policy pass hired %v (%v)", summary, err)
	}
	_, _, _, applied := j.proposalRow(id)
	app, _ := applied["application_id"].(float64)

	var pack string
	if err := j.pool.QueryRow(ctx, `SELECT COALESCE(avatar_pack::text, '') FROM p_3.application_versions WHERE application_id = $1`, int64(app)).Scan(&pack); err != nil {
		t.Fatal(err)
	}
	if pack == "" {
		t.Fatal("the automatic hire has no avatar pack")
	}
	if strings.Contains(pack, "/9/") || !media.has("3", fileOf(still)) || !media.has("3", fileOf(thinking)) {
		t.Fatalf("the files were not copied into project 3: %s", pack)
	}
}
