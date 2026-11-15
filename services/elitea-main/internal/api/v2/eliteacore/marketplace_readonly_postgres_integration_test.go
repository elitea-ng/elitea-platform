package eliteacore_test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

// installedAgent hires a plain agent and answers its application and version.
func (h *marketplaceHarness) installedAgent(name string) (appID, versionRow int64) {
	h.t.Helper()
	h.publish(h.seedAgent(2, name, nil), "v1", "")
	id := h.listingID(name)
	status, body := h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2", hireBody(id, h.currentVersionID(id), "install"))
	if status != http.StatusCreated {
		h.t.Fatalf("hire = %d %v", status, body)
	}
	appID = mustInt(h.t, body["application_id"].(string))
	if err := h.pool.QueryRow(context.Background(), `SELECT id FROM p_2.application_versions WHERE application_id = $1`, appID).Scan(&versionRow); err != nil {
		h.t.Fatal(err)
	}
	return appID, versionRow
}

// TestEveryMutationOfAnInstalledDefinitionIsRefused walks the write paths that
// change an agent's definition outside the version save: sub-agent relations
// (attach and detach) and skill attach and detach through the skills
// repository. Each answers the read-only refusal and writes nothing. The
// version save, a new version and the version delete are covered in the
// applications handler test, the toolkit relation in the toolkits handler test.
func TestEveryMutationOfAnInstalledDefinitionIsRefused(t *testing.T) {
	h := newMarketplaceHarness(t)
	appID, versionRow := h.installedAgent("Plain Helper")
	ctx := context.Background()
	other := h.seedAgent(2, "Other", nil)

	router := chi.NewRouter()
	router.Patch("/application_relation/prompt_lib/{projectID}/{appID}/{versionID}", h.handler.UpdateApplicationRelation)
	for _, attach := range []bool{true, false} {
		status, body := h.do(router, http.MethodPatch,
			fmt.Sprintf("/application_relation/prompt_lib/2/%d/%d", other.appID, other.versionID),
			map[string]any{"application_id": appID, "version_id": versionRow, "has_relation": attach})
		if status != http.StatusConflict || body["error"] != marketplace.ReadOnlyCode {
			t.Errorf("sub-agent relation (attach=%v) = %d %v, want 409 %s", attach, status, body, marketplace.ReadOnlyCode)
		}
	}

	skills := repos.NewSkillsRepo(h.pool)
	relation := v2skills.SkillRelation{EntityVersionID: strconv.FormatInt(versionRow, 10), EntityType: "agent", SkillVersionID: "1"}
	if _, err := skills.AttachSkill(ctx, "2", "1", relation); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("skill attach = %v, want the read-only refusal", err)
	}
	if err := skills.DetachSkill(ctx, "2", "1", relation); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("skill detach = %v, want the read-only refusal", err)
	}
}

// TestBindingsStayEditableWhileTheDefinitionIsReadOnly is the re-binding rule: a
// hired agent's toolkit binding can change, with the definition untouched, and
// the same checks as the hire apply.
func TestBindingsStayEditableWhileTheDefinitionIsReadOnly(t *testing.T) {
	h := newMarketplaceHarness(t)
	h.publish(h.seedAgent(2, "Issue Reader", map[string][]string{"github": {"list_issues"}}), "v1", "")
	id := h.listingID("Issue Reader")
	first := h.projectToolkit(2, "github")
	second := h.projectToolkit(2, "github")
	var key string
	for _, item := range h.detail(id)["versions"].([]any)[0].(map[string]any)["requirements"].(map[string]any)["items"].([]any) {
		key = item.(map[string]any)["key"].(string)
	}
	status, body := h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2",
		hireBody(id, h.currentVersionID(id), "install", map[string]any{"requirement_key": key, "toolkit_id": strconv.Itoa(first)}))
	if status != http.StatusCreated {
		t.Fatalf("hire = %d %v", status, body)
	}
	appID := body["application_id"].(string)
	target := "/elitea_core/marketplace_bindings/prompt_lib/2/" + appID
	router := chi.NewRouter()
	router.Get("/elitea_core/marketplace_bindings/prompt_lib/{projectID}/{applicationID}", h.handler.MarketplaceBindings)
	router.Put("/elitea_core/marketplace_bindings/prompt_lib/{projectID}/{applicationID}", h.handler.MarketplaceBindings)

	status, state := h.do(router, http.MethodGet, target, nil)
	if status != http.StatusOK || state["tools_enabled"] != true || len(state["bindings"].([]any)) != 1 {
		t.Fatalf("read bindings = %d %v", status, state)
	}
	if status, state := h.do(router, http.MethodPut, target, map[string]any{"bindings": []any{}}); status != http.StatusConflict || state["error"] != "binding_missing" {
		t.Errorf("an unbound required toolkit = %d %v, want 409 binding_missing", status, state)
	}
	status, state = h.do(router, http.MethodPut, target, map[string]any{"bindings": []any{map[string]any{"requirement_key": key, "toolkit_id": strconv.Itoa(second)}}})
	if status != http.StatusOK {
		t.Fatalf("re-bind = %d %v", status, state)
	}
	ctx := context.Background()
	count := func(toolkit int) int {
		var n int
		if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM p_2.entity_tool_mapping m JOIN p_2.application_versions v ON v.id = m.entity_version_id WHERE v.application_id = $1 AND m.tool_id = $2`, appID, toolkit).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(first) != 0 || count(second) != 1 {
		t.Errorf("mappings after the re-bind: first %d, second %d, want 0 and 1", count(first), count(second))
	}
	var instructions string
	if err := h.pool.QueryRow(ctx, `SELECT instructions FROM p_2.application_versions WHERE application_id = $1`, appID).Scan(&instructions); err != nil || instructions != "summarise the release" {
		t.Errorf("the definition changed by a re-binding: %q (%v)", instructions, err)
	}
	wrong := h.projectToolkit(2, "jira")
	if status, state := h.do(router, http.MethodPut, target, map[string]any{"bindings": []any{map[string]any{"requirement_key": key, "toolkit_id": strconv.Itoa(wrong)}}}); status != http.StatusBadRequest {
		t.Errorf("a toolkit of the wrong type = %d %v, want 400", status, state)
	}
}
