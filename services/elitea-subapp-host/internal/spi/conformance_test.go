package spi_test

// The SPI conformance suite — the Python shell's tests/conformance/test_spi.py,
// ported test for test against the same frozen fixtures, and run against
// BOTH sub-applications the host ships: DeepWiki's table (which the fixtures
// were recorded from) and the echo table. A behaviour that holds for only
// one of them is not the host's.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/echo"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

const (
	toolkit = "Wikis"
	tool    = "generate_wiki"
)

func invokeURL() string              { return "/tools/" + toolkit + "/" + tool + "/invoke" }
func invocationURL(id string) string { return "/tools/" + toolkit + "/" + tool + "/invocations/" + id }
func rawFixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	path := filepath.Join(append([]string{"..", "..", "..", "..", "conformance", "provider", "fixtures", "deepwiki"}, parts...)...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// settings pins what the Python fixtures were recorded with.
func settings() spi.Settings {
	s, _ := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", env(map[string]string{"ELITEA_DEEPWIKI_MAX_PARALLEL_WORKERS": "3"}))
	return s
}

type engine struct {
	name   string
	invoke func(ctx context.Context, call spi.Invoke, tc *spi.Context) (map[string]any, error)
}

func (e engine) Name() string { return e.name }
func (e engine) Invoke(ctx context.Context, call spi.Invoke, tc *spi.Context) (map[string]any, error) {
	return e.invoke(ctx, call, tc)
}

func host(t *testing.T, runner spi.Runner, options ...spi.Option) *spi.Server {
	t.Helper()
	server, err := spi.NewServer(settings(), deepwiki.App(runner), nil, options...)
	if err != nil {
		t.Fatal(err)
	}
	server.Start(context.Background())
	t.Cleanup(server.Stop)
	return server
}

func do(h http.Handler, method, path string, body []byte) (*httptest.ResponseRecorder, map[string]any) {
	var request *http.Request
	if body == nil {
		request = httptest.NewRequest(method, path, nil)
	} else {
		request = httptest.NewRequest(method, path, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder, decoded
}

func start(t *testing.T, h http.Handler, body []byte) string {
	t.Helper()
	if body == nil {
		body = []byte(`{}`)
	}
	recorder, decoded := do(h, http.MethodPost, invokeURL(), body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("invoke: %d %s", recorder.Code, recorder.Body.String())
	}
	return decoded["invocation_id"].(string)
}

func pollUntilTerminal(t *testing.T, h http.Handler, path string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, body := do(h, http.MethodGet, path, nil)
		if status, _ := body["status"].(string); status != "Started" && status != "InProgress" {
			return body
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never reached a terminal state", path)
	return nil
}

// DeepWikiDescriptorRevision is the revision the host serves for DeepWiki.
// It moved from legacy-v0 to legacy-v1 when the wiki chat gained reader
// attachments: `ask` and `deep_research` declare `context_paths` and
// `context_wiki_version_id`, and the `chat_history` the legacy plugin read
// on every turn and never declared. legacy-v0 stays in the fixtures as the
// record of what the legacy plugin ACTUALLY declared — the test below reads
// both, so deleting either fails rather than passing quietly.
const DeepWikiDescriptorRevision = "legacy-v1"

func TestDescriptorIsByteIdenticalToTheGoldenFixture(t *testing.T) {
	recorder, _ := do(host(t, spi.UnavailableRunner{}), http.MethodGet, "/descriptor", nil)
	golden := rawFixture(t, "descriptor", DeepWikiDescriptorRevision, "provider_descriptor.json")
	if recorder.Code != 200 {
		t.Fatalf("%d", recorder.Code)
	}
	// Byte-identical after canonical whitespace: the fixture is indented,
	// the wire is compact, and the key ORDER — which map-based encoders lose
	// — must survive. The fixture's location is the settings default.
	var compact bytes.Buffer
	if err := json.Compact(&compact, golden); err != nil {
		t.Fatal(err)
	}
	if recorder.Body.String() != compact.String() {
		t.Fatalf("descriptor differs from the golden fixture\n got: %.200s\nwant: %.200s", recorder.Body.String(), compact.String())
	}
}

func TestHealthMatchesTheRecordedShape(t *testing.T) {
	recorded := fixture(t, "spi", "health.get.json")["success"].(map[string]any)["body"].(map[string]any)
	recorder, body := do(host(t, spi.UnavailableRunner{}), http.MethodGet, "/health", nil)
	if recorder.Code != 200 {
		t.Fatal(recorder.Code)
	}
	for key := range recorded {
		if _, ok := body[key]; !ok {
			t.Errorf("health lacks %q", key)
		}
	}
	if body["status"] != "UP" || body["providerVersion"] != recorded["providerVersion"] {
		t.Fatalf("%v", body)
	}
	if _, ok := body["uptime"].(float64); !ok {
		t.Fatal("uptime is not an integer")
	}
	ts := body["timestamp"].(string)
	if len(ts) != len("2026-01-01T00:00:00+00:00") || ts[len(ts)-6:] != "+00:00" {
		t.Fatalf("timestamp %q", ts)
	}
	extra := body["extra_info"].(map[string]any)
	for _, key := range []string{"hostname", "pod_ip"} {
		if _, ok := extra[key]; !ok {
			t.Errorf("extra_info lacks %q", key)
		}
	}
	if extra["durable_invocations"] != false || extra["runner"] != "unavailable" {
		t.Fatalf("extra_info %v", extra)
	}
}

func TestSlotsMatchTheRecordedBody(t *testing.T) {
	recorded := fixture(t, "spi", "slots.get.json")["cases"].(map[string]any)["subprocess_without_worker_pool_module"].(map[string]any)["recorded"].(map[string]any)
	recorder, body := do(host(t, spi.UnavailableRunner{}), http.MethodGet, "/slots", nil)
	if float64(recorder.Code) != recorded["status_code"] {
		t.Fatal(recorder.Code)
	}
	if body["mode"] != "subprocess" || body["total"] != 3.0 || body["active"] != 0.0 || body["available"] != 3.0 || body["can_start"] != true || body["canStart"] != true {
		t.Fatalf("%v", body)
	}
}

func TestSlotsCountAnInFlightInvocation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	slow := engine{name: "slow", invoke: func(ctx context.Context, _ spi.Invoke, tc *spi.Context) (map[string]any, error) {
		close(started)
		<-release
		return spi.Completed(tc.InvocationID()), nil
	}}
	s, _ := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", env(map[string]string{"ELITEA_DEEPWIKI_MAX_PARALLEL_WORKERS": "1"}))
	server, err := spi.NewServer(s, deepwiki.App(slow), nil)
	if err != nil {
		t.Fatal(err)
	}
	server.Start(context.Background())
	defer server.Stop()
	if _, body := do(server, http.MethodGet, "/slots", nil); body["can_start"] != true {
		t.Fatal("idle host cannot start")
	}
	start(t, server, nil)
	<-started
	_, busy := do(server, http.MethodGet, "/slots", nil)
	if busy["active"] != 1.0 || busy["available"] != 0.0 || busy["can_start"] != false || busy["canStart"] != false {
		t.Fatalf("busy %v", busy)
	}
	close(release)
}

func TestJobsModeRefusesRatherThanReportingSubprocessCapacity(t *testing.T) {
	s, _ := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", env(map[string]string{"ELITEA_DEEPWIKI_SLOTS_MODE": "true", "ELITEA_DEEPWIKI_MAX_CONCURRENT_JOBS": "5"}))
	server, err := spi.NewServer(s, deepwiki.App(spi.UnavailableRunner{}), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, body := do(server, http.MethodGet, "/slots", nil)
	if body["mode"] != "jobs" || body["can_start"] != false || body["canStart"] != false || body["available"] != 0.0 || body["total"] != 5.0 || body["error"] == "" {
		t.Fatalf("%v", body)
	}
}

func TestInvokeReturnsStartedWithAnInvocationID(t *testing.T) {
	fx := fixture(t, "spi", "invoke.post.json")
	example, _ := json.Marshal(fx["request"].(map[string]any)["example"])
	recorder, body := do(host(t, spi.UnavailableRunner{}), http.MethodPost, invokeURL(), example)
	accepted := fx["accepted"].(map[string]any)
	if float64(recorder.Code) != accepted["status_code"] {
		t.Fatal(recorder.Code)
	}
	if len(body) != len(accepted["body"].(map[string]any)) || body["status"] != "Started" || body["invocation_id"].(string)[:11] != "invocation_" {
		t.Fatalf("%v", body)
	}
}

func TestInvokeRejectsAMalformedBody(t *testing.T) {
	fx := fixture(t, "spi", "invoke.post.json")["malformed_json"].(map[string]any)
	recorder, body := do(host(t, spi.UnavailableRunner{}), http.MethodPost, invokeURL(), []byte("{not json"))
	if float64(recorder.Code) != fx["status_code"] {
		t.Fatal(recorder.Code)
	}
	want, _ := json.Marshal(fx["body"])
	got, _ := json.Marshal(body)
	if string(got) != string(want) {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestInvokeIsAsyncEvenForAToolThatAdvertisesSync(t *testing.T) {
	var descriptor map[string]any
	_ = json.Unmarshal(rawFixture(t, "descriptor", "legacy-v0", "provider_descriptor.json"), &descriptor)
	for _, tk := range descriptor["provided_toolkits"].([]any) {
		for _, tl := range tk.(map[string]any)["provided_tools"].([]any) {
			if tl.(map[string]any)["sync_invocation_supported"] != true {
				t.Fatal("the fixture no longer advertises sync")
			}
		}
	}
	_, body := do(host(t, spi.UnavailableRunner{}), http.MethodPost, invokeURL(), []byte(`{}`))
	if body["status"] != "Started" {
		t.Fatalf("%v", body)
	}
}

func TestPollOfAnUnknownInvocationIs404(t *testing.T) {
	fx := fixture(t, "spi", "invocations.get.json")["get"].(map[string]any)["unknown_invocation"].(map[string]any)
	h := host(t, spi.UnavailableRunner{})
	for _, path := range []string{invocationURL("invocation_does_not_exist"), "/tools/NotAToolkit/" + tool + "/invocations/invocation_whatever", "/tools/" + toolkit + "/not_a_tool/invocations/invocation_whatever"} {
		recorder, body := do(h, http.MethodGet, path, nil)
		if float64(recorder.Code) != fx["status_code"] || body["errorCode"] != "404" {
			t.Fatalf("%s: %d %v", path, recorder.Code, body)
		}
	}
}

func TestPollProjectsInFlightStatusAndThenTheTerminalResult(t *testing.T) {
	fx := fixture(t, "spi", "invocations.get.json")
	running := make(chan struct{})
	release := make(chan struct{})
	slow := engine{name: "slow", invoke: func(ctx context.Context, _ spi.Invoke, tc *spi.Context) (map[string]any, error) {
		close(running)
		<-release
		return spi.Completed(tc.InvocationID(), spi.Message("Wiki generation completed successfully")), nil
	}}
	h := host(t, slow)
	id := start(t, h, nil)
	<-running
	_, inFlight := do(h, http.MethodGet, invocationURL(id), nil)
	if len(inFlight) != 2 || inFlight["invocation_id"] != id || inFlight["status"] != "InProgress" {
		t.Fatalf("in flight %v", inFlight)
	}
	close(release)
	terminal := pollUntilTerminal(t, h, invocationURL(id))
	recorded := fx["get"].(map[string]any)["completed"].(map[string]any)["body"].(map[string]any)
	for key := range recorded {
		if _, ok := terminal[key]; !ok {
			t.Errorf("terminal lacks %q", key)
		}
	}
	if terminal["status"] != "Completed" || terminal["result_type"] != "String" {
		t.Fatalf("%v", terminal)
	}
	var got, want []any
	_ = json.Unmarshal([]byte(terminal["result"].(string)), &got)
	_ = json.Unmarshal([]byte(recorded["result"].(string)), &want)
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("result %s want %s", g, w)
	}
}

func TestTerminalResultIsReturnedOnEveryPoll(t *testing.T) {
	done := engine{name: "done", invoke: func(_ context.Context, _ spi.Invoke, tc *spi.Context) (map[string]any, error) {
		return spi.Completed(tc.InvocationID()), nil
	}}
	h := host(t, done)
	id := start(t, h, nil)
	first := pollUntilTerminal(t, h, invocationURL(id))
	_, second := do(h, http.MethodGet, invocationURL(id), nil)
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatalf("%s vs %s", a, b)
	}
}

func TestCustomEventsAccumulateAndDrainOnRead(t *testing.T) {
	fx := fixture(t, "spi", "invocations.get.json")["get"].(map[string]any)
	emitted := make(chan struct{})
	release := make(chan struct{})
	chatty := engine{name: "chatty", invoke: func(ctx context.Context, _ spi.Invoke, tc *spi.Context) (map[string]any, error) {
		_ = tc.Thinking(ctx, "Cloning repository")
		_ = tc.Thinking(ctx, "Indexing 128 files")
		close(emitted)
		<-release
		return spi.Completed(tc.InvocationID()), nil
	}}
	h := host(t, chatty)
	id := start(t, h, nil)
	<-emitted
	_, first := do(h, http.MethodGet, invocationURL(id), nil)
	_, second := do(h, http.MethodGet, invocationURL(id), nil)
	close(release)
	wantEvents, _ := json.Marshal(fx["running_with_events"].(map[string]any)["body"].(map[string]any)["custom_events"])
	gotEvents, _ := json.Marshal(first["custom_events"])
	if string(gotEvents) != string(wantEvents) {
		t.Fatalf("events %s want %s", gotEvents, wantEvents)
	}
	if _, present := second["custom_events"]; present {
		t.Fatal("events were not drained by the first read")
	}
	if second["status"] != "InProgress" || second["invocation_id"] != id || len(second) != 2 {
		t.Fatalf("after drain %v", second)
	}
}

func TestCancelReturns204AndAnUnknownID404s(t *testing.T) {
	fx := fixture(t, "spi", "invocations.delete.json")
	finished := make(chan struct{})
	cancellable := engine{name: "cancellable", invoke: func(ctx context.Context, _ spi.Invoke, tc *spi.Context) (map[string]any, error) {
		for i := 0; i < 400; i++ {
			if err := tc.Checkpoint(); err != nil {
				return nil, err
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(finished)
		return spi.Completed(tc.InvocationID()), nil
	}}
	h := host(t, cancellable)
	id := start(t, h, nil)
	recorder, _ := do(h, http.MethodDelete, invocationURL(id), nil)
	if float64(recorder.Code) != fx["known_invocation"].(map[string]any)["status_code"] || recorder.Body.Len() != 0 {
		t.Fatalf("cancel: %d %q", recorder.Code, recorder.Body.String())
	}
	terminal := pollUntilTerminal(t, h, invocationURL(id))
	if terminal["status"] != "Error" {
		t.Fatalf("cancelled invocation ended %v", terminal)
	}
	select {
	case <-finished:
		t.Fatal("the engine ran to completion despite the cancel")
	default:
	}
	unknown, body := do(h, http.MethodDelete, invocationURL("invocation_nope"), nil)
	if float64(unknown.Code) != fx["unknown_invocation"].(map[string]any)["status_code"] || body["errorCode"] != "404" {
		t.Fatalf("unknown cancel: %d %v", unknown.Code, body)
	}
}

func TestAFailingToolIsHTTP200WithStatusError(t *testing.T) {
	broken := engine{name: "broken", invoke: func(context.Context, spi.Invoke, *spi.Context) (map[string]any, error) {
		return nil, spi.Failf(spi.KindRuntime, "worker exited with code 1")
	}}
	h := host(t, broken)
	id := start(t, h, nil)
	body := pollUntilTerminal(t, h, invocationURL(id))
	if body["status"] != "Error" || body["error_category"] != "runtime_error" || body["error_type"] != "RuntimeError" || body["result_type"] != "String" {
		t.Fatalf("%v", body)
	}
	var objects []map[string]any
	_ = json.Unmarshal([]byte(body["result"].(string)), &objects)
	if objects[0]["object_type"] != "message" || objects[0]["result_target"] != "response" {
		t.Fatalf("%v", objects)
	}
}

func TestAnUnknownToolkitTerminatesTheInvocationAsResourceNotFound(t *testing.T) {
	h := host(t, spi.UnavailableRunner{})
	recorder, accepted := do(h, http.MethodPost, "/tools/NotAToolkit/generate_wiki/invoke", []byte(`{}`))
	if recorder.Code != 200 {
		t.Fatal(recorder.Code)
	}
	body := pollUntilTerminal(t, h, "/tools/NotAToolkit/generate_wiki/invocations/"+accepted["invocation_id"].(string))
	if body["status"] != "Error" || body["error_category"] != "resource_not_found" || body["error_type"] != "FileNotFoundError" {
		t.Fatalf("%v", body)
	}
}

func TestEveryAdvertisedToolkitNameIsAccepted(t *testing.T) {
	aliases := fixture(t, "spi", "toolkit_aliases.json")
	accepted := map[string]bool{}
	for _, names := range aliases["accepted_toolkit_names"].(map[string]any) {
		for _, name := range names.([]any) {
			accepted[name.(string)] = true
		}
	}
	all := map[string]bool{}
	for _, name := range deepwiki.Toolkits.AllNames() {
		all[name] = true
	}
	if len(all) != len(accepted) {
		t.Fatalf("the table names %d toolkits, the fixture %d", len(all), len(accepted))
	}
	for name := range accepted {
		if !all[name] {
			t.Errorf("%q is accepted by the fixture and not the table", name)
		}
	}
	for _, name := range aliases["declared_toolkit_names"].([]any) {
		if _, err := deepwiki.Toolkits.Resolve(name.(string)); err != nil {
			t.Errorf("declared %q: %v", name, err)
		}
	}
}

func TestToolAdmissionPerFamilyMatchesTheFixture(t *testing.T) {
	per := fixture(t, "spi", "toolkit_aliases.json")["tools_per_family"].(map[string]any)
	for familyName, alias := range map[string]string{"main": "Wikis", "query": "wikis_query", "wiki_query": "wiki_query"} {
		family, err := deepwiki.Toolkits.Resolve(alias)
		if err != nil {
			t.Fatal(err)
		}
		for _, tl := range per[familyName].([]any) {
			if err := deepwiki.Toolkits.Admit(family, tl.(string)); err != nil {
				t.Errorf("%s/%s refused: %v", familyName, tl, err)
			}
		}
	}
	main, _ := deepwiki.Toolkits.Resolve("Wikis")
	if err := deepwiki.Toolkits.Admit(main, "list_wikis"); spi.KindOf(err) != spi.KindNotFound {
		t.Errorf("main/list_wikis: %v", err)
	}
	query, _ := deepwiki.Toolkits.Resolve("wikis_query")
	if err := deepwiki.Toolkits.Admit(query, "generate_wiki"); spi.KindOf(err) != spi.KindValue {
		t.Errorf("query/generate_wiki: %v", err)
	}
	wq, _ := deepwiki.Toolkits.Resolve("wiki_query")
	if err := deepwiki.Toolkits.Admit(wq, "ask"); spi.KindOf(err) != spi.KindValue {
		t.Errorf("wiki_query/ask: %v", err)
	}
}

// The host is generic: the same path holds for the echo application, whose
// table the fixtures were NOT recorded from.
func TestTheEchoApplicationWalksTheSamePath(t *testing.T) {
	s, _ := spi.SettingsFromEnv("ELITEA_ECHO_", env(map[string]string{}))
	server, err := spi.NewServer(s, echo.App(0), nil)
	if err != nil {
		t.Fatal(err)
	}
	server.Start(context.Background())
	defer server.Stop()
	recorder, accepted := do(server, http.MethodPost, "/tools/Echo/echo/invoke", []byte(`{"parameters":{"query":"hello"}}`))
	if recorder.Code != 200 || accepted["status"] != "Started" {
		t.Fatalf("%d %v", recorder.Code, accepted)
	}
	id := accepted["invocation_id"].(string)
	body := pollUntilTerminal(t, server, "/tools/Echo/echo/invocations/"+id)
	if body["status"] != "Completed" {
		t.Fatalf("%v", body)
	}
	var objects []map[string]any
	_ = json.Unmarshal([]byte(body["result"].(string)), &objects)
	if len(objects) != 1 || objects[0]["data"] != `Echo/echo echoed {"query":"hello"}` {
		t.Fatalf("%v", objects)
	}
	if _, health := do(server, http.MethodGet, "/health", nil); health["plugin"] != echo.Name || health["extra_info"].(map[string]any)["runner"] != "echo" {
		t.Fatalf("%v", health)
	}
	recorder, _ = do(server, http.MethodGet, "/tools/Echo/nope/invocations/x", nil)
	if recorder.Code != 404 {
		t.Fatal(recorder.Code)
	}
}

// TestDeepWikiLegacyV1AddsThreeArgumentsAndChangesNothingElse is the review a
// 17 KB generated document cannot get by being read. The revision's whole
// claim is "three arguments on two tools"; anything else that moved — a
// renamed tool, a dropped toolkit, an altered result_composition — would ship
// under cover of the same diff.
func TestDeepWikiLegacyV1AddsThreeArgumentsAndChangesNothingElse(t *testing.T) {
	added := []string{"chat_history", "context_paths", "context_wiki_version_id"}
	carrying := map[string]bool{"ask": true, "deep_research": true}

	read := func(revision string) map[string]any {
		var document map[string]any
		if err := json.Unmarshal(rawFixture(t, "descriptor", revision, "provider_descriptor.json"), &document); err != nil {
			t.Fatal(err)
		}
		return document
	}
	v0, v1 := read("legacy-v0"), read(DeepWikiDescriptorRevision)

	// Strip the added keys out of v1 and the two documents must be equal.
	// Comparing the WHOLE document, rather than walking the tools, is what
	// makes "and changes nothing else" an assertion rather than a comment.
	toolkits, _ := v1["provided_toolkits"].([]any)
	if len(toolkits) != len(v0["provided_toolkits"].([]any)) {
		t.Fatalf("legacy-v1 has %d toolkits, legacy-v0 has %d", len(toolkits), len(v0["provided_toolkits"].([]any)))
	}
	seen := 0
	for _, raw := range toolkits {
		tools, _ := raw.(map[string]any)["provided_tools"].([]any)
		for _, rawTool := range tools {
			tool := rawTool.(map[string]any)
			schema, _ := tool["args_schema"].(map[string]any)
			for _, name := range added {
				_, present := schema[name]
				if present != carrying[tool["name"].(string)] {
					t.Fatalf("%s: %s present=%v, want %v", tool["name"], name, present, carrying[tool["name"].(string)])
				}
				if present {
					seen++
					delete(schema, name)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("legacy-v1 declares none of the new arguments")
	}
	stripped, _ := json.Marshal(v1)
	original, _ := json.Marshal(v0)
	if string(stripped) != string(original) {
		t.Fatalf("legacy-v1 differs from legacy-v0 by more than the added arguments\n got: %.400s\nwant: %.400s", stripped, original)
	}
}
