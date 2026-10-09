package main

// Inventory on the host, composed the way a deployment composes it — through
// the registry, from the environment.
//
// With no runner named, a deployment gets the whole SPI and NO engine:
// /descriptor and /health answer, and every tool the descriptor advertises
// terminates with a readable refusal rather than an empty success something
// downstream could be built against. That is still the default after
// ADR-0023 stage I3 gave the application a `sidecar` runner (`legacy` before ADR-0027), and deliberately:
// enabling the engine is naming ELITEA_INVENTORY_RUNNER=sidecar AND giving it a
// socket to reach, both explicit. An image built without the engine extra must
// not look like it has one.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

func TestInventoryComposesAndRefusesEveryToolWithNoRunnerNamed(t *testing.T) {
	app, settings, err := compose(lookup(map[string]string{"ELITEA_SUBAPP": "inventory"}))
	if err != nil {
		t.Fatal(err)
	}
	if settings.Prefix != inventory.EnvPrefix {
		t.Fatalf("settings prefix %q", settings.Prefix)
	}
	server, err := spi.NewServer(settings, app, nil)
	if err != nil {
		t.Fatal(err)
	}
	server.Start(context.Background())
	defer server.Stop()

	call := func(method, path string, body io.Reader) (*httptest.ResponseRecorder, map[string]any) {
		request := httptest.NewRequest(method, path, body)
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		var decoded map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
		return recorder, decoded
	}

	recorder, descriptor := call(http.MethodGet, "/descriptor", nil)
	if recorder.Code != http.StatusOK || descriptor["name"] != "inventory" {
		t.Fatalf("/descriptor: %d %v", recorder.Code, descriptor["name"])
	}
	if toolkits, _ := descriptor["provided_toolkits"].([]any); len(toolkits) != 2 {
		t.Fatalf("/descriptor advertises %d toolkits", len(toolkits))
	}
	recorder, health := call(http.MethodGet, "/health", nil)
	if recorder.Code != http.StatusOK || health["status"] != "UP" || health["plugin"] != inventory.Name ||
		health["extra_info"].(map[string]any)["runner"] != "unavailable" {
		t.Fatalf("/health: %d %v", recorder.Code, health)
	}

	for _, family := range inventory.Toolkits.Families {
		for _, tool := range family.Tools {
			base := "/tools/" + family.Aliases[0] + "/" + tool
			recorder, accepted := call(http.MethodPost, base+"/invoke", strings.NewReader(`{}`))
			if recorder.Code != http.StatusOK || accepted["status"] != "Started" {
				t.Fatalf("%s: %d %v", base, recorder.Code, accepted)
			}
			path := base + "/invocations/" + accepted["invocation_id"].(string)
			deadline := time.Now().Add(5 * time.Second)
			var terminal map[string]any
			for time.Now().Before(deadline) {
				_, terminal = call(http.MethodGet, path, nil)
				if status, _ := terminal["status"].(string); status != "Started" && status != "InProgress" {
					break
				}
				time.Sleep(2 * time.Millisecond)
			}
			if terminal["status"] != "Error" || terminal["error_category"] != "resource_not_found" {
				t.Fatalf("%s ended %v", base, terminal)
			}
			if result, _ := terminal["result"].(string); !strings.Contains(result, "No tool runner is configured") {
				t.Fatalf("%s refusal does not say why: %v", base, terminal["result"])
			}
		}
	}
}
