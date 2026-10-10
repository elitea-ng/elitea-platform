package spi_test

// The SPI conformance suite, run a third time — against Inventory's table.
//
// The suite in conformance_test.go is the Python shell's tests/conformance/
// test_spi.py against DeepWiki's table (the one the fixtures were recorded
// from), with one pass over echo. Inventory is the second REAL application,
// and it is the falsification test for ADR-0023's runner generalisation: it
// contributes a descriptor and an admission table and nothing else, so
// every behaviour below holds here for exactly the reason it holds for
// DeepWiki — the host, not the application.
//
// Where a case in the DeepWiki suite reads a recorded body, this one reads
// the same recording: the shapes are the protocol's, and no fixture of
// Inventory's own records them (fixtures/inventory/ holds its descriptor).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

const (
	inventoryToolkit = "inventory"
	inventoryTool    = "run_ingestion"
	searchToolkit    = "inventory_search"
)

func inventoryInvokeURL() string {
	return "/tools/" + inventoryToolkit + "/" + inventoryTool + "/invoke"
}
func inventoryInvocationURL(id string) string {
	return "/tools/" + inventoryToolkit + "/" + inventoryTool + "/invocations/" + id
}

// inventorySettings pins the same host settings the DeepWiki suite runs
// with, under Inventory's namespace.
func inventorySettings(t *testing.T, pairs map[string]string) spi.Settings {
	t.Helper()
	if pairs == nil {
		pairs = map[string]string{"ELITEA_INVENTORY_MAX_PARALLEL_WORKERS": "3"}
	}
	settings, err := spi.SettingsFromEnv("ELITEA_INVENTORY_", env(pairs))
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func inventoryHost(t *testing.T, runner spi.Runner, pairs map[string]string) *spi.Server {
	t.Helper()
	server, err := spi.NewServer(inventorySettings(t, pairs), inventory.App(runner), nil)
	if err != nil {
		t.Fatal(err)
	}
	server.Start(context.Background())
	t.Cleanup(server.Stop)
	return server
}

func inventoryFixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	path := filepath.Join(append([]string{"..", "..", "..", "..", "conformance", "provider", "fixtures", "inventory"}, parts...)...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// DescriptorRevision is the revision the host serves, and the one the byte pin
// below reads. It moved from legacy-v0 to legacy-v1 in ADR-0023 H4c stage I3,
// which added four tools the legacy plugin implemented, routed and never
// declared — see internal/apps/inventory/inventory.go and the fixture
// directory's README for which four and why. It moved to legacy-v2 when the
// native engine's graph import and export became tools.
//
// legacy-v0 stays in the fixtures. It is the record of what the legacy plugin
// ACTUALLY declared, which is what a parity question is asked against; the test
// below reads it as well, so deleting it fails rather than passing quietly.
const DescriptorRevision = "legacy-v2"

func TestTheInventoryApplicationWalksTheWholeSuite(t *testing.T) {
	// The served descriptor is the frozen legacy-v2 document, byte for byte
	// after canonical whitespace — key ORDER included, which a map-based
	// encoder would lose. The fixture's location is the settings default.
	t.Run("the descriptor is byte-identical to the golden fixture", func(t *testing.T) {
		recorder, _ := do(inventoryHost(t, spi.UnavailableRunner{}, nil), http.MethodGet, "/descriptor", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%d", recorder.Code)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, inventoryFixture(t, "descriptor", DescriptorRevision, "provider_descriptor.json")); err != nil {
			t.Fatal(err)
		}
		if recorder.Body.String() != compact.String() {
			t.Fatalf("descriptor differs from the golden fixture\n got: %.200s\nwant: %.200s", recorder.Body.String(), compact.String())
		}
	})

	// legacy-v1 differs from legacy-v0 by exactly four added tools and
	// nothing else. Checked here rather than trusted, because the change was
	// made to a 37 KB document by a generator: a generator that also dropped a
	// tool, reordered a family or edited an args_schema would produce a
	// descriptor this suite's other cases still accept.
	t.Run("legacy-v1 adds four tools to legacy-v0 and changes nothing else", func(t *testing.T) {
		type tool struct {
			Name string `json:"name"`
		}
		type toolkit struct {
			Name          string          `json:"name"`
			ProvidedTools []tool          `json:"provided_tools"`
			Config        json.RawMessage `json:"toolkit_config"`
		}
		read := func(revision string) []toolkit {
			var document struct {
				ProvidedToolkits []toolkit `json:"provided_toolkits"`
			}
			if err := json.Unmarshal(inventoryFixture(t, "descriptor", revision, "provider_descriptor.json"), &document); err != nil {
				t.Fatal(err)
			}
			return document.ProvidedToolkits
		}
		v0, v1 := read("legacy-v0"), read("legacy-v1")
		if len(v0) != len(v1) {
			t.Fatalf("legacy-v1 has %d toolkits, legacy-v0 has %d", len(v1), len(v0))
		}
		added := map[string]bool{
			"get_ingestion_status": true, "get_entities_by_ids": true,
			"get_entity_neighbors": true, "smart_normalize_types": true,
		}
		for i := range v0 {
			if v0[i].Name != v1[i].Name {
				t.Fatalf("toolkit %d renamed: %q -> %q", i, v0[i].Name, v1[i].Name)
			}
			if !bytes.Equal(v0[i].Config, v1[i].Config) {
				t.Fatalf("%s: toolkit_config changed; legacy-v1 adds tools only", v0[i].Name)
			}
			var kept []string
			for _, tl := range v1[i].ProvidedTools {
				if !added[tl.Name] {
					kept = append(kept, tl.Name)
				}
			}
			var before []string
			for _, tl := range v0[i].ProvidedTools {
				before = append(before, tl.Name)
			}
			if strings.Join(kept, ",") != strings.Join(before, ",") {
				t.Fatalf("%s: legacy-v0's tools are not legacy-v1 minus the four added\n got: %v\nwant: %v",
					v0[i].Name, kept, before)
			}
		}
	})

	// legacy-v2 differs from legacy-v1 by two added tools and one corrected
	// description, and nothing else. The same reason as above: a generator
	// edited a large document, and only a diff shows what it did not change.
	t.Run("legacy-v2 adds two tools to legacy-v1 and corrects one description", func(t *testing.T) {
		type toolkit struct {
			Name          string            `json:"name"`
			ProvidedTools []json.RawMessage `json:"provided_tools"`
			Config        json.RawMessage   `json:"toolkit_config"`
		}
		read := func(revision string) []toolkit {
			var document struct {
				ProvidedToolkits []toolkit `json:"provided_toolkits"`
			}
			if err := json.Unmarshal(inventoryFixture(t, "descriptor", revision, "provider_descriptor.json"), &document); err != nil {
				t.Fatal(err)
			}
			return document.ProvidedToolkits
		}
		nameOf := func(raw json.RawMessage) string {
			var tool struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(raw, &tool)
			return tool.Name
		}
		withoutDescription := func(raw json.RawMessage) string {
			var tool map[string]any
			_ = json.Unmarshal(raw, &tool)
			delete(tool, "description")
			encoded, _ := json.Marshal(tool)
			return string(encoded)
		}
		// legacy-v2 adds one optional toolkit parameter, reasoning_effort;
		// everything else in toolkit_config must be what legacy-v1 has.
		configWithoutReasoningEffort := func(raw json.RawMessage) string {
			var config map[string]any
			_ = json.Unmarshal(raw, &config)
			if parameters, ok := config["parameters"].(map[string]any); ok {
				delete(parameters, "reasoning_effort")
			}
			if order, ok := config["fields_order"].([]any); ok {
				kept := order[:0]
				for _, field := range order {
					if field != "reasoning_effort" {
						kept = append(kept, field)
					}
				}
				config["fields_order"] = kept
			}
			encoded, _ := json.Marshal(config)
			return string(encoded)
		}
		v1, v2 := read("legacy-v1"), read(DescriptorRevision)
		if len(v1) != len(v2) {
			t.Fatalf("legacy-v2 has %d toolkits, legacy-v1 has %d", len(v2), len(v1))
		}
		added := map[string]bool{"import_graph": true, "export_graph": true}
		seen := 0
		for i := range v1 {
			if v1[i].Name != v2[i].Name || configWithoutReasoningEffort(v1[i].Config) != configWithoutReasoningEffort(v2[i].Config) {
				t.Fatalf("toolkit %d: name or toolkit_config changed beyond the reasoning_effort parameter", i)
			}
			var kept []json.RawMessage
			for _, raw := range v2[i].ProvidedTools {
				if v2[i].Name == "inventory" && added[nameOf(raw)] {
					seen++
					continue
				}
				kept = append(kept, raw)
			}
			if len(kept) != len(v1[i].ProvidedTools) {
				t.Fatalf("%s: %d tools kept, legacy-v1 has %d", v1[i].Name, len(kept), len(v1[i].ProvidedTools))
			}
			for j := range kept {
				if nameOf(kept[j]) == "smart_normalize_types" {
					if withoutDescription(kept[j]) != withoutDescription(v1[i].ProvidedTools[j]) {
						t.Fatalf("smart_normalize_types changed beyond its description")
					}
					if strings.Contains(string(kept[j]), "automatically") {
						t.Fatalf("smart_normalize_types still claims to run automatically")
					}
					continue
				}
				var before, after any
				_ = json.Unmarshal(v1[i].ProvidedTools[j], &before)
				_ = json.Unmarshal(kept[j], &after)
				b, _ := json.Marshal(before)
				a, _ := json.Marshal(after)
				if !bytes.Equal(a, b) {
					t.Fatalf("%s/%s changed between legacy-v1 and legacy-v2", v1[i].Name, nameOf(kept[j]))
				}
			}
		}
		if seen != len(added) {
			t.Fatalf("legacy-v2 adds %d of the %d tools", seen, len(added))
		}
	})

	// The admission table and the advertised document are the same list.
	// Either half drifting is how a tool becomes callable and undocumented,
	// or documented and refused.
	t.Run("the table admits exactly what the descriptor advertises", func(t *testing.T) {
		var document struct {
			ProvidedToolkits []struct {
				Name          string `json:"name"`
				ProvidedTools []struct {
					Name string `json:"name"`
				} `json:"provided_tools"`
			} `json:"provided_toolkits"`
		}
		if err := json.Unmarshal(inventoryFixture(t, "descriptor", DescriptorRevision, "provider_descriptor.json"), &document); err != nil {
			t.Fatal(err)
		}
		// The table also holds the platform's own family (issue #1244), which
		// the descriptor does not advertise: every OTHER family is advertised,
		// and the one that is not is the admin family, not in Advertised.
		advertisedFamilies := map[string]bool{}
		for _, toolkit := range document.ProvidedToolkits {
			advertisedFamilies[toolkit.Name] = true
		}
		var unadvertised []string
		for _, family := range inventory.Toolkits.Families {
			if !advertisedFamilies[family.Name] {
				unadvertised = append(unadvertised, family.Name)
			}
		}
		if len(unadvertised) != 1 || unadvertised[0] != "inventory_admin" {
			t.Fatalf("the table has families the descriptor does not advertise: %v, want only inventory_admin", unadvertised)
		}
		for _, name := range inventory.Toolkits.Advertised {
			if name == "inventory_admin" {
				t.Fatal("inventory_admin is advertised")
			}
		}
		for _, toolkit := range document.ProvidedToolkits {
			family, err := inventory.Toolkits.Resolve(toolkit.Name)
			if err != nil {
				t.Fatalf("advertised toolkit %q is not admitted: %v", toolkit.Name, err)
			}
			advertised := map[string]bool{}
			for _, tool := range toolkit.ProvidedTools {
				advertised[tool.Name] = true
				if err := inventory.Toolkits.Admit(family, tool.Name); err != nil {
					t.Errorf("%s/%s is advertised and refused: %v", toolkit.Name, tool.Name, err)
				}
			}
			for _, tool := range family.Tools {
				if !advertised[tool] {
					t.Errorf("%s/%s is admitted and not advertised", toolkit.Name, tool)
				}
			}
		}
	})

	t.Run("health matches the recorded shape", func(t *testing.T) {
		recorded := fixture(t, "spi", "health.get.json")["success"].(map[string]any)["body"].(map[string]any)
		recorder, body := do(inventoryHost(t, spi.UnavailableRunner{}, nil), http.MethodGet, "/health", nil)
		if recorder.Code != http.StatusOK {
			t.Fatal(recorder.Code)
		}
		for key := range recorded {
			if _, ok := body[key]; !ok {
				t.Errorf("health lacks %q", key)
			}
		}
		if body["status"] != "UP" || body["plugin"] != inventory.Name || body["providerVersion"] != inventory.Version {
			t.Fatalf("%v", body)
		}
		extra := body["extra_info"].(map[string]any)
		if extra["durable_invocations"] != false || extra["runner"] != "unavailable" {
			t.Fatalf("extra_info %v", extra)
		}
	})

	t.Run("slots report the subprocess capacity", func(t *testing.T) {
		recorder, body := do(inventoryHost(t, spi.UnavailableRunner{}, nil), http.MethodGet, "/slots", nil)
		if recorder.Code != http.StatusOK {
			t.Fatal(recorder.Code)
		}
		if body["mode"] != "subprocess" || body["total"] != 3.0 || body["active"] != 0.0 ||
			body["available"] != 3.0 || body["can_start"] != true || body["canStart"] != true {
			t.Fatalf("%v", body)
		}
	})

	t.Run("slots count an in-flight invocation", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		slow := engine{name: "slow", invoke: func(_ context.Context, _ spi.Invoke, tc *spi.Context) (map[string]any, error) {
			close(started)
			<-release
			return spi.Completed(tc.InvocationID()), nil
		}}
		h := inventoryHost(t, slow, map[string]string{"ELITEA_INVENTORY_MAX_PARALLEL_WORKERS": "1"})
		defer close(release)
		if _, body := do(h, http.MethodGet, "/slots", nil); body["can_start"] != true {
			t.Fatal("idle host cannot start")
		}
		inventoryStart(t, h, nil)
		<-started
		_, busy := do(h, http.MethodGet, "/slots", nil)
		if busy["active"] != 1.0 || busy["available"] != 0.0 || busy["can_start"] != false || busy["canStart"] != false {
			t.Fatalf("busy %v", busy)
		}
	})

	t.Run("jobs mode refuses rather than reporting subprocess capacity", func(t *testing.T) {
		h := inventoryHost(t, spi.UnavailableRunner{}, map[string]string{
			"ELITEA_INVENTORY_SLOTS_MODE": "true", "ELITEA_INVENTORY_MAX_CONCURRENT_JOBS": "5"})
		_, body := do(h, http.MethodGet, "/slots", nil)
		if body["mode"] != "jobs" || body["can_start"] != false || body["canStart"] != false ||
			body["available"] != 0.0 || body["total"] != 5.0 || body["error"] == "" {
			t.Fatalf("%v", body)
		}
	})

	t.Run("invoke returns Started with an invocation id", func(t *testing.T) {
		accepted := fixture(t, "spi", "invoke.post.json")["accepted"].(map[string]any)
		recorder, body := do(inventoryHost(t, spi.UnavailableRunner{}, nil), http.MethodPost, inventoryInvokeURL(), []byte(`{}`))
		if float64(recorder.Code) != accepted["status_code"] {
			t.Fatal(recorder.Code)
		}
		if len(body) != len(accepted["body"].(map[string]any)) || body["status"] != "Started" ||
			body["invocation_id"].(string)[:11] != "invocation_" {
			t.Fatalf("%v", body)
		}
	})

	t.Run("invoke rejects a malformed body", func(t *testing.T) {
		fx := fixture(t, "spi", "invoke.post.json")["malformed_json"].(map[string]any)
		recorder, body := do(inventoryHost(t, spi.UnavailableRunner{}, nil), http.MethodPost, inventoryInvokeURL(), []byte("{not json"))
		if float64(recorder.Code) != fx["status_code"] {
			t.Fatal(recorder.Code)
		}
		want, _ := json.Marshal(fx["body"])
		got, _ := json.Marshal(body)
		if string(got) != string(want) {
			t.Fatalf("got %s want %s", got, want)
		}
	})

	// The descriptor advertises sync invocation for every tool, and the
	// host is async for all of them anyway — the Python shell's quirk, the
	// same one for every application.
	t.Run("invoke is async even for a tool that advertises sync", func(t *testing.T) {
		var descriptor map[string]any
		_ = json.Unmarshal(inventoryFixture(t, "descriptor", DescriptorRevision, "provider_descriptor.json"), &descriptor)
		sync := false
		for _, tk := range descriptor["provided_toolkits"].([]any) {
			for _, tl := range tk.(map[string]any)["provided_tools"].([]any) {
				if tl.(map[string]any)["sync_invocation_supported"] == true {
					sync = true
				}
			}
		}
		if !sync {
			t.Fatal("the fixture no longer advertises sync anywhere")
		}
		_, body := do(inventoryHost(t, spi.UnavailableRunner{}, nil), http.MethodPost, inventoryInvokeURL(), []byte(`{}`))
		if body["status"] != "Started" {
			t.Fatalf("%v", body)
		}
	})

	t.Run("a poll of an unknown invocation is 404", func(t *testing.T) {
		fx := fixture(t, "spi", "invocations.get.json")["get"].(map[string]any)["unknown_invocation"].(map[string]any)
		h := inventoryHost(t, spi.UnavailableRunner{}, nil)
		for _, path := range []string{
			inventoryInvocationURL("invocation_does_not_exist"),
			"/tools/NotAToolkit/" + inventoryTool + "/invocations/invocation_whatever",
			"/tools/" + inventoryToolkit + "/not_a_tool/invocations/invocation_whatever",
		} {
			recorder, body := do(h, http.MethodGet, path, nil)
			if float64(recorder.Code) != fx["status_code"] || body["errorCode"] != "404" {
				t.Fatalf("%s: %d %v", path, recorder.Code, body)
			}
		}
	})

	t.Run("poll projects in-flight status and then the terminal result", func(t *testing.T) {
		running := make(chan struct{})
		release := make(chan struct{})
		slow := engine{name: "slow", invoke: func(_ context.Context, _ spi.Invoke, tc *spi.Context) (map[string]any, error) {
			close(running)
			<-release
			return spi.Completed(tc.InvocationID(), spi.Message("Ingestion completed")), nil
		}}
		h := inventoryHost(t, slow, nil)
		id := inventoryStart(t, h, nil)
		<-running
		_, inFlight := do(h, http.MethodGet, inventoryInvocationURL(id), nil)
		if len(inFlight) != 2 || inFlight["invocation_id"] != id || inFlight["status"] != "InProgress" {
			t.Fatalf("in flight %v", inFlight)
		}
		close(release)
		terminal := pollUntilTerminal(t, h, inventoryInvocationURL(id))
		recorded := fixture(t, "spi", "invocations.get.json")["get"].(map[string]any)["completed"].(map[string]any)["body"].(map[string]any)
		for key := range recorded {
			if _, ok := terminal[key]; !ok {
				t.Errorf("terminal lacks %q", key)
			}
		}
		if terminal["status"] != "Completed" || terminal["result_type"] != "String" {
			t.Fatalf("%v", terminal)
		}
	})

	t.Run("the terminal result is returned on every poll", func(t *testing.T) {
		done := engine{name: "done", invoke: func(_ context.Context, _ spi.Invoke, tc *spi.Context) (map[string]any, error) {
			return spi.Completed(tc.InvocationID()), nil
		}}
		h := inventoryHost(t, done, nil)
		id := inventoryStart(t, h, nil)
		first := pollUntilTerminal(t, h, inventoryInvocationURL(id))
		_, second := do(h, http.MethodGet, inventoryInvocationURL(id), nil)
		a, _ := json.Marshal(first)
		b, _ := json.Marshal(second)
		if string(a) != string(b) {
			t.Fatalf("%s vs %s", a, b)
		}
	})

	t.Run("custom events accumulate and drain on read", func(t *testing.T) {
		emitted := make(chan struct{})
		release := make(chan struct{})
		chatty := engine{name: "chatty", invoke: func(ctx context.Context, _ spi.Invoke, tc *spi.Context) (map[string]any, error) {
			_ = tc.Thinking(ctx, "Reading sources")
			_ = tc.Thinking(ctx, "Building the graph")
			close(emitted)
			<-release
			return spi.Completed(tc.InvocationID()), nil
		}}
		h := inventoryHost(t, chatty, nil)
		id := inventoryStart(t, h, nil)
		<-emitted
		_, first := do(h, http.MethodGet, inventoryInvocationURL(id), nil)
		_, second := do(h, http.MethodGet, inventoryInvocationURL(id), nil)
		close(release)
		events, ok := first["custom_events"].([]any)
		if !ok || len(events) != 2 {
			t.Fatalf("events %v", first["custom_events"])
		}
		if _, present := second["custom_events"]; present {
			t.Fatal("events were not drained by the first read")
		}
		if second["status"] != "InProgress" || second["invocation_id"] != id || len(second) != 2 {
			t.Fatalf("after drain %v", second)
		}
	})

	t.Run("cancel returns 204 and an unknown id 404s", func(t *testing.T) {
		fx := fixture(t, "spi", "invocations.delete.json")
		finished := make(chan struct{})
		cancellable := engine{name: "cancellable", invoke: func(_ context.Context, _ spi.Invoke, tc *spi.Context) (map[string]any, error) {
			for i := 0; i < 400; i++ {
				if err := tc.Checkpoint(); err != nil {
					return nil, err
				}
				time.Sleep(5 * time.Millisecond)
			}
			close(finished)
			return spi.Completed(tc.InvocationID()), nil
		}}
		h := inventoryHost(t, cancellable, nil)
		id := inventoryStart(t, h, nil)
		recorder, _ := do(h, http.MethodDelete, inventoryInvocationURL(id), nil)
		if float64(recorder.Code) != fx["known_invocation"].(map[string]any)["status_code"] || recorder.Body.Len() != 0 {
			t.Fatalf("cancel: %d %q", recorder.Code, recorder.Body.String())
		}
		if terminal := pollUntilTerminal(t, h, inventoryInvocationURL(id)); terminal["status"] != "Error" {
			t.Fatalf("cancelled invocation ended %v", terminal)
		}
		select {
		case <-finished:
			t.Fatal("the engine ran to completion despite the cancel")
		default:
		}
		unknown, body := do(h, http.MethodDelete, inventoryInvocationURL("invocation_nope"), nil)
		if float64(unknown.Code) != fx["unknown_invocation"].(map[string]any)["status_code"] || body["errorCode"] != "404" {
			t.Fatalf("unknown cancel: %d %v", unknown.Code, body)
		}
	})

	t.Run("a failing tool is HTTP 200 with status Error", func(t *testing.T) {
		broken := engine{name: "broken", invoke: func(context.Context, spi.Invoke, *spi.Context) (map[string]any, error) {
			return nil, spi.Failf(spi.KindRuntime, "the graph store is unreachable")
		}}
		h := inventoryHost(t, broken, nil)
		id := inventoryStart(t, h, nil)
		body := pollUntilTerminal(t, h, inventoryInvocationURL(id))
		if body["status"] != "Error" || body["error_category"] != "runtime_error" ||
			body["error_type"] != "RuntimeError" || body["result_type"] != "String" {
			t.Fatalf("%v", body)
		}
		var objects []map[string]any
		_ = json.Unmarshal([]byte(body["result"].(string)), &objects)
		if objects[0]["object_type"] != "message" || objects[0]["result_target"] != "response" {
			t.Fatalf("%v", objects)
		}
	})

	t.Run("an unknown toolkit terminates the invocation as resource_not_found", func(t *testing.T) {
		h := inventoryHost(t, spi.UnavailableRunner{}, nil)
		recorder, accepted := do(h, http.MethodPost, "/tools/NotAToolkit/"+inventoryTool+"/invoke", []byte(`{}`))
		if recorder.Code != http.StatusOK {
			t.Fatal(recorder.Code)
		}
		body := pollUntilTerminal(t, h, "/tools/NotAToolkit/"+inventoryTool+"/invocations/"+accepted["invocation_id"].(string))
		if body["status"] != "Error" || body["error_category"] != "resource_not_found" || body["error_type"] != "FileNotFoundError" {
			t.Fatalf("%v", body)
		}
	})

	// Both families refuse an unknown tool as invalid input, naming the
	// toolkit and what it does serve — the legacy plugin raised ValueError
	// for both, and the two families do not share a tool set.
	t.Run("an unknown tool is invalid input in both families", func(t *testing.T) {
		for toolkit, foreign := range map[string]string{
			inventoryToolkit: "search_knowledge_graph", // inventory_search's
			searchToolkit:    "run_ingestion",          // inventory's
		} {
			family, err := inventory.Toolkits.Resolve(toolkit)
			if err != nil {
				t.Fatal(err)
			}
			err = inventory.Toolkits.Admit(family, foreign)
			if spi.KindOf(err) != spi.KindValue {
				t.Fatalf("%s/%s: %v", toolkit, foreign, err)
			}
			h := inventoryHost(t, spi.UnavailableRunner{}, nil)
			base := "/tools/" + toolkit + "/" + foreign
			recorder, accepted := do(h, http.MethodPost, base+"/invoke", []byte(`{}`))
			if recorder.Code != http.StatusOK {
				t.Fatal(recorder.Code)
			}
			body := pollUntilTerminal(t, h, base+"/invocations/"+accepted["invocation_id"].(string))
			if body["status"] != "Error" || body["error_category"] != "invalid_input" || body["error_type"] != "ValueError" {
				t.Fatalf("%s/%s: %v", toolkit, foreign, body)
			}
		}
	})

	// Every advertised name resolves, and no name outside the table does.
	t.Run("every advertised toolkit name is accepted and nothing else is", func(t *testing.T) {
		for _, name := range inventory.Toolkits.Advertised {
			if _, err := inventory.Toolkits.Resolve(name); err != nil {
				t.Errorf("advertised %q: %v", name, err)
			}
		}
		for _, name := range []string{"Inventory", "inventory-search", "InventorySearch", "Wikis", ""} {
			if _, err := inventory.Toolkits.Resolve(name); spi.KindOf(err) != spi.KindNotFound {
				t.Errorf("%q resolved: %v", name, err)
			}
		}
	})

	// The unavailable runner is what a deployment gets until stage I3: every
	// tool of every family terminates with a readable refusal.
	t.Run("the unavailable runner refuses every advertised tool", func(t *testing.T) {
		h := inventoryHost(t, spi.UnavailableRunner{}, nil)
		for _, family := range inventory.Toolkits.Families {
			for _, tool := range family.Tools {
				base := "/tools/" + family.Aliases[0] + "/" + tool
				recorder, accepted := do(h, http.MethodPost, base+"/invoke", []byte(`{}`))
				if recorder.Code != http.StatusOK || accepted["status"] != "Started" {
					t.Fatalf("%s: %d %v", base, recorder.Code, accepted)
				}
				body := pollUntilTerminal(t, h, base+"/invocations/"+accepted["invocation_id"].(string))
				if body["status"] != "Error" || body["error_category"] != "resource_not_found" {
					t.Fatalf("%s ended %v", base, body)
				}
			}
		}
	})
}

func inventoryStart(t *testing.T, h http.Handler, body []byte) string {
	t.Helper()
	if body == nil {
		body = []byte(`{}`)
	}
	recorder, decoded := do(h, http.MethodPost, inventoryInvokeURL(), body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("invoke: %d %s", recorder.Code, recorder.Body.String())
	}
	return decoded["invocation_id"].(string)
}
