package run_test

// tests/unit/test_fixture_runner.py, ported: the fixture runner, and the
// artifact upload it shares with the real one. The upload tests drive the
// shared Runner with a fake client, so what they pin is the wiring — which
// objects go where, under which key, and what a caller is told when that
// fails.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

var transport = map[string]any{"api_base": "http://elitea-main:8080/llm/v1", "api_key": "minted", "organization": "90200"}

type upload struct{ bucket, name, data string }

type fakeArtifactClient struct {
	mu      sync.Mutex
	uploads []upload
	fail    map[string]bool
}

func (c *fakeArtifactClient) Upload(_ context.Context, bucket, name string, data []byte) error {
	if c.fail[name] {
		return errors.New("bucket is read-only")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.uploads = append(c.uploads, upload{bucket, name, string(data)})
	return nil
}

func (c *fakeArtifactClient) names() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]bool{}
	for _, u := range c.uploads {
		out[u.name] = true
	}
	return out
}

func fixtureRequest(query string, llmSettings map[string]any) map[string]any {
	parameters := map[string]any{
		"code_toolkit": map[string]any{"github_configuration": map[string]any{
			"url": "https://github.com", "repository": "acme/e2e-service", "active_branch": "main"}},
	}
	if llmSettings != nil {
		parameters["llm_settings"] = llmSettings
	}
	return map[string]any{"configuration": map[string]any{"parameters": parameters}, "parameters": map[string]any{"query": query}}
}

func fixtureRunner(client run.ArtifactClient, step time.Duration) *run.Runner {
	settings, _ := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", func(key string) (string, bool) {
		if key == "ELITEA_DEEPWIKI_GIT_ALLOWLIST" {
			return "github.com,*.github.com", true
		}
		return "", false
	})
	runner := run.NewFixtureRunner(settings, step)
	runner.Artifacts = func(map[string]any) (run.ArtifactClient, error) {
		if client == nil {
			return nil, nil
		}
		return client, nil
	}
	return runner
}

// invokeWithEvents runs the tool through a live invocation manager — the
// runner is the invocation's Call, exactly as the host wires it — polls to
// the terminal body, and returns every thinking event the polls drained.
// cancelAfter, when set, requests a stop once a thinking event carrying that
// text has been seen — i.e. once the TOOL is running, so the stop is
// observed at the tool's own checkpoints and not at the runner's first one.
func invokeWithEvents(t *testing.T, runner spi.Runner, family spi.Family, tool string, request map[string]any, cancelAfter string) (map[string]any, []string, error) {
	t.Helper()
	manager := spi.NewManager(nil, time.Hour, nil)
	manager.Start(context.Background())
	defer manager.Stop()
	ctx := context.Background()
	invocation, err := manager.Submit(ctx, "Wikis", tool, func(ctx context.Context, tc *spi.Context) (map[string]any, error) {
		return runner.Invoke(ctx, spi.Invoke{Family: family, Toolkit: "Wikis", Tool: tool, Request: request}, tc)
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled := false
	var events []string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		body, err := manager.Poll(ctx, "Wikis", tool, invocation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if raw, ok := body["custom_events"].([]map[string]any); ok {
			for _, e := range raw {
				events = append(events, strings.TrimSpace(strings.Join(stringsOf(e), " ")))
			}
		}
		if cancelAfter != "" && !cancelled && strings.Contains(strings.Join(events, "\n"), cancelAfter) {
			cancelled = true
			if _, err := manager.Cancel(ctx, "Wikis", tool, invocation.ID); err != nil {
				t.Fatal(err)
			}
		}
		switch body["status"] {
		case "Completed":
			return body, events, nil
		case "Error":
			return body, events, spi.Failf(spi.Kind(str(body["error_type"])), "%s: %s", str(body["error_category"]), str(body["result"]))
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the invocation never settled")
	return nil, nil, nil
}

// stringsOf reads the thinking message out of a stored event, which the
// store shapes as {"data": {"message": …}} — the legacy invocation_thinking
// envelope the web app's thinking log reads.
func stringsOf(m map[string]any) []string {
	if data, ok := m["data"].(map[string]any); ok {
		return []string{str(data["message"])}
	}
	return nil
}

func str(v any) string { s, _ := v.(string); return s }

func objectsOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	if body["status"] != "Completed" {
		t.Fatalf("status %v: %v", body["status"], body)
	}
	var objects []map[string]any
	if err := json.Unmarshal([]byte(body["result"].(string)), &objects); err != nil {
		t.Fatal(err)
	}
	return objects
}

func TestTheWikiIDIsTheEngineCanonicalForm(t *testing.T) {
	if run.WikiIDFor(map[string]any{"repository": "acme/e2e-service"}, "main") != "acme--e2e-service--main" ||
		run.WikiIDFor(map[string]any{"provider_config": map[string]any{"repository": "octocat/hello"}}, "") != "octocat--hello--main" ||
		run.WikiIDFor(nil, "") != "fixture--repository--main" {
		t.Fatal("wiki id")
	}
}

func TestGenerationLandsEveryObjectUnderTheWikiID(t *testing.T) {
	client := &fakeArtifactClient{}
	body, events, err := invokeWithEvents(t, fixtureRunner(client, 0), spi.Family{Name: "main"}, "generate_wiki", fixtureRequest("GO", transport), "")
	if err != nil {
		t.Fatal(err)
	}
	objects := objectsOf(t, body)
	if objects[0]["data"] != "Wiki generated: 3 pages" || objects[0]["object_type"] != "message" {
		t.Fatalf("%v", objects[0])
	}
	expected := map[string]bool{}
	kinds := map[string]bool{}
	for _, obj := range objects[1:] {
		kinds[obj["object_type"].(string)] = true
		if obj["result_target"] == "artifact" {
			expected[obj["name"].(string)] = true
		}
	}
	uploaded := client.names()
	if len(uploaded) != 6 || len(expected) != 6 {
		t.Fatalf("uploaded %v expected %v", uploaded, expected)
	}
	for name := range expected {
		if !uploaded[name] || !strings.HasPrefix(name, "acme--e2e-service--main/") {
			t.Fatalf("%s", name)
		}
	}
	for _, u := range client.uploads {
		if u.bucket != "wiki-artifacts" {
			t.Fatal(u.bucket)
		}
		if strings.HasSuffix(u.name, "request-flow.md") && !strings.Contains(u.data, "```mermaid\ngraph TD\n  A[Client] -->\n```") {
			t.Fatal("the broken mermaid page is not broken")
		}
	}
	for _, kind := range []string{"wiki_structure", "wiki_page", "wiki_manifest", "repository_context"} {
		if !kinds[kind] {
			t.Fatalf("no %s object", kind)
		}
	}
	var manifest map[string]any
	for _, obj := range objects {
		if obj["object_type"] == "wiki_manifest" {
			_ = json.Unmarshal([]byte(obj["data"].(string)), &manifest)
		}
	}
	pages, _ := json.Marshal(manifest["pages"])
	if string(pages) != `["wiki_pages/overview/getting-started.md","wiki_pages/architecture/request-flow.md","wiki_pages/components/storage.md"]` {
		t.Fatal(string(pages))
	}
	text := strings.Join(events, "\n")
	if !strings.Contains(text, "Cloning the repository") || !strings.Contains(text, "Uploaded 6 wiki objects") {
		t.Fatalf("events %q", text)
	}
}

func TestAFailedUploadIsReportedInBandAndTheRunStillCompletes(t *testing.T) {
	client := &fakeArtifactClient{fail: map[string]bool{"acme--e2e-service--main/wiki_pages/components/storage.md": true}}
	body, events, err := invokeWithEvents(t, fixtureRunner(client, 0), spi.Family{Name: "main"}, "generate_wiki", fixtureRequest("GO", transport), "")
	if err != nil {
		t.Fatal(err)
	}
	objects := objectsOf(t, body)
	warning := objects[len(objects)-1]
	if warning["object_type"] != "message" || !strings.HasPrefix(warning["data"].(string), "⚠️ The wiki was generated but 1 of 6 objects could not be uploaded") ||
		!strings.Contains(warning["data"].(string), "components/storage.md: bucket is read-only") {
		t.Fatalf("%v", warning)
	}
	if len(client.uploads) != 5 || !strings.Contains(strings.Join(events, "\n"), "Uploading FAILED for 1 object(s)") {
		t.Fatalf("uploads %d, events %v", len(client.uploads), events)
	}
}

func TestWithoutATransportNothingIsUploadedAndTheResultIsUnchanged(t *testing.T) {
	var calls []map[string]any
	runner := fixtureRunner(nil, 0)
	runner.Artifacts = func(settings map[string]any) (run.ArtifactClient, error) {
		calls = append(calls, settings)
		return nil, nil
	}
	body, _, err := invokeWithEvents(t, runner, spi.Family{Name: "main"}, "generate_wiki", fixtureRequest("GO", nil), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || len(calls[0]) != 0 {
		t.Fatalf("factory calls %v", calls)
	}
	for _, obj := range objectsOf(t, body) {
		if strings.Contains(str(obj["data"]), "could not be uploaded") {
			t.Fatal("an in-band upload failure was reported with no transport")
		}
	}
}

func TestTheDefaultFactoryNeedsBothHalvesOfTheTransport(t *testing.T) {
	factory := run.ArtifactClientFrom("")
	for _, settings := range []map[string]any{{}, {"api_base": "http://x/llm/v1"}, {"api_key": "k"}} {
		if client, err := factory(settings); client != nil || err != nil {
			t.Fatalf("%v: %v %v", settings, client, err)
		}
	}
	client, err := factory(transport)
	if err != nil {
		t.Fatal(err)
	}
	http, ok := client.(*run.HTTPArtifactClient)
	if !ok || http.Settings.BaseURL != "http://elitea-main:8080" || http.Settings.ProjectID != "90200" || http.Settings.APIKey != "minted" {
		t.Fatalf("%+v", client)
	}
}

func TestAskAnswersWithSourcesAndUploadsNothing(t *testing.T) {
	client := &fakeArtifactClient{}
	request := fixtureRequest("", transport)
	request["parameters"] = map[string]any{"question": "Where do pages live?"}
	body, _, err := invokeWithEvents(t, fixtureRunner(client, 0), spi.Family{Name: "main"}, "ask", request, "")
	if err != nil {
		t.Fatal(err)
	}
	objects := objectsOf(t, body)
	if objects[0]["data"] != "Fixture answer to: Where do pages live?" || !strings.HasPrefix(objects[1]["data"].(string), "\n\nSources:\n- wiki_pages/overview/getting-started.md") {
		t.Fatalf("%v", objects)
	}
	if len(client.uploads) != 0 {
		t.Fatal("ask uploaded something")
	}
}

// Deliberately pinned as part of DWIKI-012b. The fixture's deep_research
// used to emit three lines of progress and a report, and no plan — so the
// browser's research panel, which renders only when a run HAS one, stayed
// empty on the fixture path and no journey could tell "no plan" from "the
// panel is not wired up". The run now publishes one, and this is the test
// that says what it publishes and in what envelope: a `todo_update`
// structured event carried on the same thinking channel as the progress
// lines, which is the only channel the SPI has.
func TestDeepResearchPublishesItsPlanBeforeTheReport(t *testing.T) {
	client := &fakeArtifactClient{}
	request := fixtureRequest("", transport)
	request["parameters"] = map[string]any{"question": "How does indexing work?", "research_type": "architecture"}
	body, events, err := invokeWithEvents(t, fixtureRunner(client, 0), spi.Family{Name: "main"}, "deep_research", request, "")
	if err != nil {
		t.Fatal(err)
	}
	if report := str(objectsOf(t, body)[0]["data"]); !strings.Contains(report, "# Research report (architecture)") ||
		!strings.Contains(report, "Question: How does indexing work?") {
		t.Fatalf("report %q", report)
	}
	if len(client.uploads) != 0 {
		t.Fatal("deep research uploaded something")
	}

	// The plan travels as ONE event, before the steps that work through it.
	var plan map[string]any
	planAt, readingAt := -1, -1
	for i, event := range events {
		switch {
		case strings.Contains(event, `"todo_update"`):
			if err := json.Unmarshal([]byte(event), &plan); err != nil {
				t.Fatalf("event %d is not an envelope: %v", i, err)
			}
			if planAt >= 0 {
				t.Fatalf("the plan was published twice: %v", events)
			}
			planAt = i
		case event == "Reading the relevant pages":
			readingAt = i
		}
	}
	// Before the work it plans, not after it: a plan that arrives with the
	// report is a summary, and says nothing while the run is going.
	if planAt < 0 || readingAt < 0 || planAt > readingAt {
		t.Fatalf("plan at %d, work at %d, of %v", planAt, readingAt, events)
	}
	if plan["event"] != "todo_update" {
		t.Fatalf("event name %v", plan["event"])
	}
	items, _ := plan["data"].(map[string]any)["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("items %v", items)
	}
	// `title` and `status`, not the engine's pre-normalisation `content`:
	// the browser reads `title` and renders "Untitled step" without it.
	first, _ := items[0].(map[string]any)
	if first["title"] != "Plan the research" || first["status"] != "completed" {
		t.Fatalf("first todo %v", first)
	}
	last, _ := items[2].(map[string]any)
	if last["title"] != "Write the report" || last["status"] != "pending" {
		t.Fatalf("last todo %v", last)
	}
}

func TestACancelledRunStopsBeforeTheToolAnswers(t *testing.T) {
	client := &fakeArtifactClient{}
	body, events, err := invokeWithEvents(t, fixtureRunner(client, 30*time.Millisecond), spi.Family{Name: "main"}, "generate_wiki", fixtureRequest("GO", transport), "Cloning the repository")
	if err == nil {
		t.Fatalf("a cancelled run completed: %v", body)
	}
	if len(client.uploads) != 0 {
		t.Fatal("a cancelled run uploaded")
	}
	// The stop is observed at the TOOL's checkpoints, not only at the
	// runner's upload loop: the fixture never reaches its last step.
	if text := strings.Join(events, "\n"); strings.Contains(text, "Assembling the manifest") || strings.Contains(text, "Uploading") {
		t.Fatalf("the tool ran to its end before stopping: %q", text)
	}
}

func TestAForbiddenCloneDestinationIsRefusedBeforeTheToolRuns(t *testing.T) {
	client := &fakeArtifactClient{}
	request := fixtureRequest("GO", transport)
	request["configuration"].(map[string]any)["parameters"].(map[string]any)["code_toolkit"] = map[string]any{
		"gitlab_configuration": map[string]any{"url": "https://gitlab.com", "repository": "acme/x"}}
	body, _, err := invokeWithEvents(t, fixtureRunner(client, 0), spi.Family{Name: "main"}, "generate_wiki", request, "")
	if err == nil || body["error_category"] != "invalid_input" || len(client.uploads) != 0 {
		t.Fatalf("%v %v", body, err)
	}
}
