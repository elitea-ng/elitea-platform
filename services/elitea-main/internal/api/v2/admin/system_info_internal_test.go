package admin

// Handler-level tests for `GET /admin/system_info/{mode}` and its ungated
// `/admin/system_info/prompt_lib` sibling (#219, #892).
//
// #219's defect does not look broken: the handler used to answer 200 with a
// plausible plugin list — `elitea_core` and `auth` at version "2.0.0" — from a
// service that loads no plugins. Nothing in a status code, a log line or a
// type check reports that. Only an assertion on the BODY does, so these tests
// assert on the body, both for the fabricated-plugin defect (still guarded
// below, `NewHandler(nil)` reports no plugin fleet) and for #892's real
// components (this binary's own build version, always present; the migration
// head, present only with a database).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/buildinfo"
)

// systemInfoBody serves the handler once and returns its status and decoded body.
func systemInfoBody(t *testing.T) (int, map[string]any) {
	t.Helper()

	recorder := httptest.NewRecorder()
	NewHandler(nil).SystemInfo(recorder, httptest.NewRequest(http.MethodGet, "/admin/system_info/prompt_lib", nil))

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("the response is not a JSON object: %v (%q)", err, recorder.Body.String())
	}
	return recorder.Code, body
}

// componentNames extracts the `name` field of every entry in `components`.
func componentNames(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["components"]
	if !ok {
		t.Fatalf("response carries no components field: %v", body)
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("components is not an array: %v", raw)
	}
	names := make([]string, 0, len(list))
	for _, entry := range list {
		row, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a components entry is not an object: %v", entry)
		}
		name, ok := row["name"].(string)
		if !ok {
			t.Fatalf("a components entry has no string name: %v", row)
		}
		names = append(names, name)
	}
	return names
}

// TestSystemInfoReportsNoPluginFleet is the #219 regression guard. This
// service loads no plugins and has no Arbiter bus to ask about other
// processes, so it must never report a fleet — not a fabricated one, and not
// under any of the six Pylon plugin names.
func TestSystemInfoReportsNoPluginFleet(t *testing.T) {
	_, body := systemInfoBody(t)

	for _, forbidden := range []string{"elitea_core", "admin", "notifications", "configurations", "sdk_plugin", "indexer_worker", "auth"} {
		for _, name := range componentNames(t, body) {
			if name == forbidden {
				t.Errorf("response reports a fleet component %q; this service has no plugin fleet to ask about (#219)", forbidden)
			}
		}
	}
	if _, present := body["plugins"]; present {
		t.Errorf("response still carries the old plugins field: %v", body["plugins"])
	}
}

// TestSystemInfoReportsThisBinarysOwnVersion is #892's acceptance guard: the
// one component this process can always report about itself is its own
// build version, with no database needed.
func TestSystemInfoReportsThisBinarysOwnVersion(t *testing.T) {
	status, body := systemInfoBody(t)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	names := componentNames(t, body)
	if len(names) != 1 || names[0] != "elitea-main" {
		t.Fatalf("components = %v, want exactly [\"elitea-main\"] with a nil pool", names)
	}

	raw := body["components"].([]any)
	row := raw[0].(map[string]any)
	if row["version"] != buildinfo.Version {
		t.Errorf("elitea-main version = %v, want the buildinfo.Version this test binary was built with (%q)", row["version"], buildinfo.Version)
	}
}

// TestSystemInfoOmitsMigrationsWithNoDatabase covers the fail-open path: a
// nil pool (every unit test in this package, and any handler wired without
// WithPool) must not turn an informational tooltip into a panic or a 500.
func TestSystemInfoOmitsMigrationsWithNoDatabase(t *testing.T) {
	_, body := systemInfoBody(t)

	for _, name := range componentNames(t, body) {
		if name == "migrations" {
			t.Errorf("a nil pool must not produce a migrations component: %v", body)
		}
	}
}
