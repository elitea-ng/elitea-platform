package run_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// The inventory_admin family (issue #1244): the platform's calls, never
// advertised, reaching the engine only with the project of a VERIFIED
// identity, and the project-wide one only with no user in it.

func adminHarness(t *testing.T) *harness {
	t.Helper()
	result := map[string]any{"success": true, "result": "Deleted."}
	h := newHarness(t, map[string]run.Tool{
		"delete_graph":          answer(result),
		"delete_project_graphs": answer(result),
	})
	h.runner.RequireVerifiedProject = true
	return h
}

func TestTheAdminFamilyIsAdmittedAndNeverAdvertised(t *testing.T) {
	family, err := inventory.Toolkits.Resolve("inventory_admin")
	if err != nil {
		t.Fatal(err)
	}
	if family.Name != run.AdminFamily {
		t.Fatalf("family = %q, want %q", family.Name, run.AdminFamily)
	}
	for _, name := range inventory.Toolkits.Advertised {
		if name == "inventory_admin" {
			t.Fatal("inventory_admin is advertised: a user could be offered a graph delete")
		}
	}
	if err := inventory.Toolkits.Validate(); err != nil {
		t.Fatal(err)
	}
	served := map[string]bool{}
	for _, tool := range run.EngineTools() {
		served[tool] = true
	}
	for _, tool := range []string{run.DeleteGraphTool, run.DeleteProjectGraphsTool} {
		if !served[tool] {
			t.Errorf("the engine runner does not serve %s", tool)
		}
	}
	// The descriptor the host serves lists neither.
	descriptor := fmt.Sprint(inventory.Descriptor("https://host"))
	for _, tool := range []string{run.DeleteGraphTool, run.DeleteProjectGraphsTool} {
		if strings.Contains(descriptor, tool) {
			t.Errorf("the descriptor advertises %s", tool)
		}
	}
}

func TestDeletingAToolkitsGraphNeedsTheVerifiedProjectAndTheToolkit(t *testing.T) {
	h := adminHarness(t)
	request := map[string]any{"configuration": map[string]any{"application_id": 70}, "parameters": map[string]any{}}

	// elitea-main's toolkit deletion: the project signed, a user in it.
	h.identity = spi.Identity{ProjectID: "7", UserID: "42"}
	if _, err := h.invoke("inventory_admin", "inventory_admin", "delete_graph", request); err != nil {
		t.Fatal(err)
	}
	if h.lastArgs["project_id"] != "7" || fmt.Sprint(h.lastArgs["application_id"]) != "70" || h.lastArgs["family"] != "inventory_admin" {
		t.Errorf("the engine got %v, want project 7 (the verified one), toolkit 70, family inventory_admin", h.lastArgs)
	}

	// A body naming another project does not reach it.
	h.lastArgs = nil
	other := map[string]any{"project_id": 8, "configuration": map[string]any{"application_id": 70, "project_id": 8},
		"parameters": map[string]any{"project_id": 8}}
	if _, err := h.invoke("inventory_admin", "inventory_admin", "delete_graph", other); err != nil {
		t.Fatal(err)
	}
	if h.lastArgs["project_id"] != "7" {
		t.Errorf("project_id = %v, want the verified 7", h.lastArgs["project_id"])
	}

	// No verified project: refused, the engine never called.
	h.lastArgs = nil
	h.identity = spi.Identity{}
	if body, err := h.invoke("inventory_admin", "inventory_admin", "delete_graph", request); err == nil || h.lastArgs != nil {
		t.Fatalf("an unsigned call deleted a graph: %v %v", body, h.lastArgs)
	}

	// No toolkit named: the host will not guess which graph.
	h.identity = spi.Identity{ProjectID: "7"}
	body, err := h.invoke("inventory_admin", "inventory_admin", "delete_graph", map[string]any{"parameters": map[string]any{}})
	if err == nil || h.lastArgs != nil {
		t.Fatalf("a delete that names no toolkit reached the engine: %v %v", body, h.lastArgs)
	}
	if !strings.Contains(fmt.Sprint(body), "application_id") {
		t.Errorf("the refusal does not name the missing toolkit: %v", body)
	}
}

func TestDeletingAProjectsGraphsIsAPlatformCallWithNoUser(t *testing.T) {
	h := adminHarness(t)
	request := map[string]any{"parameters": map[string]any{}}

	// A user session is refused whatever project it names.
	h.identity = spi.Identity{ProjectID: "7", UserID: "42"}
	body, err := h.invoke("inventory_admin", "inventory_admin", "delete_project_graphs", request)
	if err == nil || h.lastArgs != nil {
		t.Fatalf("a user session deleted a project's graphs: %v %v", body, h.lastArgs)
	}
	if !strings.Contains(fmt.Sprint(body), "user session") {
		t.Errorf("the refusal does not say why: %v", body)
	}
	if category(body) != spi.CategoryInvalidInput {
		t.Errorf("category = %s, want %s", category(body), spi.CategoryInvalidInput)
	}

	// An unverified hop (the gate emptied the identity) and a stray user
	// with no project are refused too.
	for _, identity := range []spi.Identity{{}, {UserID: "42"}} {
		h.identity, h.lastArgs = identity, nil
		if body, err := h.invoke("inventory_admin", "inventory_admin", "delete_project_graphs", request); err == nil || h.lastArgs != nil {
			t.Fatalf("identity %+v deleted a project's graphs: %v %v", identity, body, h.lastArgs)
		}
	}

	// elitea-main's project deprovisioning: the project signed, no user.
	h.identity, h.lastArgs = spi.Identity{ProjectID: "7"}, nil
	if _, err := h.invoke("inventory_admin", "inventory_admin", "delete_project_graphs", request); err != nil {
		t.Fatal(err)
	}
	if h.lastArgs["project_id"] != "7" {
		t.Errorf("the engine got %v, want the verified project 7", h.lastArgs)
	}
}

func TestAnUnknownAdminToolIsRefusedAtTheDoor(t *testing.T) {
	h := adminHarness(t)
	h.identity = spi.Identity{ProjectID: "7"}
	if _, err := h.invoke("inventory_admin", "inventory_admin", "search_graph", map[string]any{}); err == nil || h.lastArgs != nil {
		t.Fatal("the admin family served a read tool")
	}
}
