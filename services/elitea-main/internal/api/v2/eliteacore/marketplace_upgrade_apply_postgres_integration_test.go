package eliteacore_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// TestApprovalRevalidatesTheBindings proves the bindings are checked again when
// the request is approved: a toolkit deleted meanwhile refuses the approval as
// stale and leaves the agent on its version.
func TestApprovalRevalidatesTheBindings(t *testing.T) {
	h := newMarketplaceHarness(t)
	ctx := context.Background()
	agent := h.seedAgent(2, "Issue Reader", map[string][]string{"github": {"list_issues"}})
	h.publish(agent, "v1", "")
	id := h.listingID("Issue Reader")
	appID, _, toolkit := h.installed(id)
	if _, err := h.pool.Exec(ctx, `
UPDATE p_2.entity_tool_mapping SET selected_tools = '["list_issues", "create_issue"]'::jsonb WHERE entity_version_id = $1`, agent.versionID); err != nil {
		t.Fatal(err)
	}
	h.publish(agent, "v2", "Can open issues.")
	v2 := h.versionIDByName(id, "v2")
	v1 := h.pinnedVersion(appID)
	status, body := h.do(h.newRouter(11), http.MethodPost, upgradeURL(appID), map[string]any{"version_id": v2})
	if status != http.StatusOK || body["awaiting_approval"] != true {
		t.Fatalf("upgrade = %d %v, want a change request awaiting approval", status, body)
	}
	changeID := mustInt(t, body["change_request_id"].(string))
	if _, err := h.pool.Exec(ctx, `DELETE FROM p_2.elitea_tools WHERE id = $1`, toolkit); err != nil {
		t.Fatalf("delete the toolkit: %v", err)
	}
	_, err := h.lifecycle.ApproveChangeRequest(ctx, repos.Approval{ProjectID: 2, ApplicationID: appID, ChangeID: changeID, ApproverID: 12, Governance: true})
	if err == nil || !strings.Contains(err.Error(), "deleted or changed") {
		t.Fatalf("approval after the toolkit was deleted = %v, want a stale refusal that says so", err)
	}
	if h.pinnedVersion(appID) != v1 {
		t.Errorf("the agent moved to %s although the approval was refused", h.pinnedVersion(appID))
	}
}

// TestUpgradeUpdatesTheAvatarAndDropsRemovedMetaKeys proves that an upgrade
// takes the new version's avatar, drops a meta key the new version removed, and
// keeps a key the project set itself.
func TestUpgradeUpdatesTheAvatarAndDropsRemovedMetaKeys(t *testing.T) {
	h := newMarketplaceHarness(t)
	ctx := context.Background()
	agent := h.seedAgent(2, "Plain Helper", nil)
	if _, err := h.pool.Exec(ctx, `UPDATE p_2.application_versions SET meta = '{"category":"Development","feature_flag":"on"}'::jsonb WHERE id = $1`, agent.versionID); err != nil {
		t.Fatal(err)
	}
	h.publish(agent, "v1", "")
	id := h.listingID("Plain Helper")
	status, body := h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2", hireBody(id, h.currentVersionID(id), "install"))
	if status != http.StatusCreated {
		t.Fatalf("hire = %d %v", status, body)
	}
	appID := mustInt(t, body["application_id"].(string))
	if _, err := h.pool.Exec(ctx, `
UPDATE p_2.application_versions SET meta = meta || '{"project_note":"ours"}'::jsonb, avatar_pack = '{"stale":true}'::jsonb WHERE application_id = $1`, appID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `UPDATE p_2.application_versions SET meta = '{"category":"Development"}'::jsonb WHERE id = $1`, agent.versionID); err != nil {
		t.Fatal(err)
	}
	h.publish(agent, "v2", "")
	v2 := h.versionIDByName(id, "v2")
	if status, body = h.do(h.router, http.MethodPost, upgradeURL(appID), map[string]any{"version_id": v2}); status != http.StatusOK || body["applied"] != true {
		t.Fatalf("upgrade = %d %v", status, body)
	}
	var meta string
	var hasAvatar bool
	if err := h.pool.QueryRow(ctx, `SELECT meta::text, avatar_pack IS NOT NULL FROM p_2.application_versions WHERE application_id = $1`, appID).Scan(&meta, &hasAvatar); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(meta, "feature_flag") || !strings.Contains(meta, "project_note") || !strings.Contains(meta, `"origin_version_id": "`+v2+`"`) {
		t.Errorf("meta after the upgrade = %s, want the removed key gone, the project's key kept and the new origin", meta)
	}
	if hasAvatar {
		t.Errorf("the installed avatar is still the old one; v2 has none")
	}
}

// TestPublishSettlesEveryAutoInstall hires one listing into many auto installs
// and publishes a text-only version: every install is upgraded, and the time it
// took is logged (the measured bound for settling on the publish path).
func TestPublishSettlesEveryAutoInstall(t *testing.T) {
	h := newMarketplaceHarness(t)
	agent := h.seedAgent(2, "Plain Helper", nil)
	h.publish(agent, "v1", "")
	id := h.listingID("Plain Helper")
	const installs = 25
	var apps []int64
	for i := 0; i < installs; i++ {
		status, body := h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2", hireBody(id, h.currentVersionID(id), "install"))
		if status != http.StatusCreated {
			t.Fatalf("hire %d = %d %v", i, status, body)
		}
		app := mustInt(t, body["application_id"].(string))
		apps = append(apps, app)
		url := "/elitea_core/marketplace_upgrade_policy/prompt_lib/2/" + strconv.FormatInt(app, 10)
		if status, _ := h.do(h.router, http.MethodPut, url, map[string]any{"policy": "auto"}); status != http.StatusOK {
			t.Fatalf("policy %d = %d", i, status)
		}
	}
	h.setInstructions(agent, "a newer text")
	start := time.Now()
	h.publish(agent, "v2", "")
	t.Logf("settled %d auto installs in %s on the publish path", installs, time.Since(start))
	v2 := h.versionIDByName(id, "v2")
	for _, app := range apps {
		if h.pinnedVersion(app) != v2 {
			t.Fatalf("agent %d was not settled onto v2", app)
		}
	}
}
