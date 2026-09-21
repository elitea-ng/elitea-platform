package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
)

// The `admin/system_info` surface (#219), asserted on the ROUTER.
//
// A handler test alone cannot close this issue. This repository has a recurring
// class of handlers that answer well and are reached from nowhere, so the answer
// only matters once the route is shown to carry a request to it. These tests
// therefore drive the same router a deployment builds.
func newSystemInfoTestRouter(t *testing.T) chi.Router {
	t.Helper()
	return NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	})
}

// TestSystemInfoRoutesStayRegistered pins both routes. `prompt_lib` is a STATIC
// segment that chi matches ahead of the `{mode}` parameter, and it is the
// Help Center's ungated read. Losing either registration turns an explicit
// refusal into a 404, which reads as a broken deployment.
func TestSystemInfoRoutesStayRegistered(t *testing.T) {
	got := walkRoutes(t, newSystemInfoTestRouter(t))

	for _, required := range []string{
		"GET /api/v2/admin/system_info/prompt_lib",
		"GET /api/v2/admin/system_info/{mode}",
	} {
		if _, ok := got[required]; !ok {
			t.Errorf("%s is not registered", required)
		}
	}
}

// TestSystemInfoReportsNoFabricatedPluginsOverTheRouter is the regression guard
// for the defect itself, taken through the route an operator's browser uses.
//
// The caller is authenticated and holds no permissions, so the gated `{mode}`
// route answers 403 and the ungated `prompt_lib` route reaches the handler.
//
// #892 gave this route a real 200 — this binary's own build version, always,
// plus a `migrations` entry when a database answered (there is none in this
// router-level harness, so only `elitea-main` is expected here; the DB-backed
// half is covered by internal/api/v2/admin's postgres-integration test). The
// assertion stays on the BODY, not the status: #219's defect was a 200 whose
// body invented a plugin list, and the fix must not reintroduce that under a
// different field name. `components` must carry no plugin, worker or gateway
// name — only the two this service can honestly report about itself.
func TestSystemInfoReportsNoFabricatedPluginsOverTheRouter(t *testing.T) {
	recorder := serveResponse(t, newSystemInfoTestRouter(t), http.MethodGet, "/api/v2/admin/system_info/prompt_lib")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("the response is not a JSON object: %v (%q)", err, recorder.Body.String())
	}
	if _, present := body["plugins"]; present {
		t.Errorf("the route still reports a plugin inventory under the old key: %v", body["plugins"])
	}
	components, ok := body["components"].([]any)
	if !ok {
		t.Fatalf("the response carries no components array: %v", body)
	}
	for _, entry := range components {
		row, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a components entry is not an object: %v", entry)
		}
		name, _ := row["name"].(string)
		switch name {
		case "elitea_core", "admin", "notifications", "configurations", "sdk_plugin", "indexer_worker":
			t.Errorf("the route reports a fabricated plugin fleet component %q (#219)", name)
		case "elitea-main", "migrations":
			// This service's own real, local facts — the only names it may report.
		default:
			t.Errorf("the route reports an unexpected component %q with no known source", name)
		}
	}
}

// TestSystemInfoAdministrationModeKeepsItsGate — `/system_info/{mode}` declares
// `["runtime.plugins"]` in pylon and the router gates it on that permission.
// Answering 501 from the handler must not become a reason to drop the gate: the
// refusal is a statement about this platform's architecture, and an unprivileged
// caller has no business learning it from an administration-mode route.
func TestSystemInfoAdministrationModeKeepsItsGate(t *testing.T) {
	status := serveStatus(t, newSystemInfoTestRouter(t), http.MethodGet, "/api/v2/admin/system_info/administration")

	switch status {
	case http.StatusForbidden:
		// The gate ran and refused, which is correct for a caller with no grants.
	case http.StatusNotImplemented:
		t.Error("GET /api/v2/admin/system_info/administration reached the handler for a caller with " +
			"no permissions; the route lost its runtime.plugins gate")
	case http.StatusNotFound:
		t.Error("GET /api/v2/admin/system_info/administration is not routed at all")
	default:
		t.Errorf("GET /api/v2/admin/system_info/administration status = %d, want 403", status)
	}
}
