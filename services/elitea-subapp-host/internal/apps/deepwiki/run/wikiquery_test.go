package run_test

// The wiki_query family, against the P0 recording in
// conformance/provider/fixtures/deepwiki/wiki_query/, which was produced by
// running the legacy handlers themselves (tools/record_wiki_query.py).
//
// The three answers that are NOT compared to the recording are delete_wiki's,
// and the fixture says why: the legacy delete never worked in production —
// MiniArtifactClient had no list_artifacts, so every delete raised
// AttributeError into the manager's own except and answered "0 artifacts".
// The recording carries that under `legacy_defect`, and the port's own
// behaviour is asserted here against DeleteWikiButton's semantics instead.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/artifacts"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// deepwikiToolkits is the admission table the host actually serves.
func deepwikiToolkits() spi.Toolkits { return deepwiki.Toolkits }

// ---------------------------------------------------------------------------
// A bucket
// ---------------------------------------------------------------------------

type fakeStore struct {
	mu          sync.Mutex
	objects     map[string]string
	undeletable map[string]bool
	listErr     error
	deleted     []string
	uploaded    []string
}

func newStore(objects map[string]string) *fakeStore {
	return &fakeStore{objects: objects, undeletable: map[string]bool{}}
}

func (s *fakeStore) Upload(_ context.Context, _, name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[name] = string(data)
	s.uploaded = append(s.uploaded, name)
	return nil
}

func (s *fakeStore) List(_ context.Context, _, prefix string) ([]artifacts.Object, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := make([]artifacts.Object, 0, len(keys))
	for _, key := range keys {
		out = append(out, artifacts.Object{Key: key})
	}
	return out, nil
}

func (s *fakeStore) Download(_ context.Context, bucket, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("artifact not found: %s/%s", bucket, key)
	}
	return []byte(body), nil
}

func (s *fakeStore) DeleteBatch(_ context.Context, _ string, keys []string) ([]string, []artifacts.Failure, error) {
	if len(keys) == 0 {
		return nil, nil, errors.New("no keys were named for deletion")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var deleted []string
	var failed []artifacts.Failure
	for _, key := range keys {
		if s.undeletable[key] {
			failed = append(failed, artifacts.Failure{Key: key, Code: "AccessDenied", Message: "not authorized"})
			continue
		}
		delete(s.objects, key)
		deleted = append(deleted, key)
		s.deleted = append(s.deleted, key)
	}
	return deleted, failed, nil
}

func storeFactory(store artifacts.Store) run.ArtifactClientFactory {
	return func(llmSettings map[string]any) (artifacts.Client, error) {
		if !run.Truthy(llmSettings["api_base"]) {
			return nil, nil
		}
		return store, nil
	}
}

// ---------------------------------------------------------------------------
// The recording
// ---------------------------------------------------------------------------

func wikiQueryFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtures, "wiki_query", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// recordedCase returns one recorded case's single response object.
func recordedCase(t *testing.T, fixture map[string]any, name string) (objectType, data string) {
	t.Helper()
	cases := fixture["cases"].(map[string]any)
	entry, ok := cases[name].(map[string]any)
	if !ok {
		t.Fatalf("the fixture has no case %q", name)
	}
	objects := entry["result_objects"].([]any)
	if len(objects) != 1 {
		t.Fatalf("case %q recorded %d objects, not one", name, len(objects))
	}
	object := objects[0].(map[string]any)
	for _, pinned := range []struct{ key, want string }{
		{"result_target", "response"}, {"result_encoding", "plain"},
	} {
		if object[pinned.key] != pinned.want {
			t.Fatalf("case %q: %s is %v", name, pinned.key, object[pinned.key])
		}
	}
	return object["object_type"].(string), object["data"].(string)
}

// registryBucket is the fixture's own registry, in a bucket.
func registryBucket(t *testing.T, fixture map[string]any) *fakeStore {
	t.Helper()
	encoded, err := json.Marshal(fixture["registry"])
	if err != nil {
		t.Fatal(err)
	}
	return newStore(map[string]string{run.RegistryPath: string(encoded)})
}

// llmSettings is a transport the factory above accepts. It is the fixture's
// own block, field for field, so a recorded engine call can be compared
// whole rather than key by key.
var llmSettings = map[string]any{
	"model_name": "gpt-4o", "api_base": "http://elitea/llm/v1",
	"api_key": "<redacted>", "organization": "90200",
}

// wikiRunner is the family over one bucket and canned model-backed halves.
func wikiRunner(store artifacts.Store, deps run.WikiQueryDeps) *run.Runner {
	return &run.Runner{
		Egress: spi.ParseEgressPolicy("*"),
		Tools:  run.WikiQueryTools(storeFactory(store), deps),
	}
}

// queryRequest is a wiki_query invoke: the toolkit configuration carries the
// llm_settings the facade minted, the tool carries its own arguments.
func queryRequest(parameters map[string]any) map[string]any {
	return map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{"llm_settings": llmSettings}},
		"parameters":    parameters,
	}
}

func wikiQueryFamily() spi.Family {
	return spi.Family{Name: "wiki_query", Aliases: []string{"wiki_query"},
		Tools: run.WikiQueryToolNames, UnknownToolIsInvalidInput: true, Label: "wiki_query"}
}

// answer runs one tool and returns its single composed object.
func answer(t *testing.T, runner *run.Runner, tool string, parameters map[string]any) (objectType, data string) {
	t.Helper()
	body, err := invokeFamily(t, runner, wikiQueryFamily(), tool, queryRequest(parameters))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var objects []map[string]any
	if err := json.Unmarshal([]byte(body["result"].(string)), &objects); err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 {
		t.Fatalf("%s composed %d objects, not one: %v", tool, len(objects), objects)
	}
	if objects[0]["result_target"] != "response" || objects[0]["result_encoding"] != "plain" {
		t.Fatalf("%s: %v", tool, objects[0])
	}
	if _, named := objects[0]["name"]; named {
		t.Fatalf("%s composed a NAMED object; a response object carries no key: %v", tool, objects[0])
	}
	return objects[0]["object_type"].(string), objects[0]["data"].(string)
}

// ---------------------------------------------------------------------------
// The regression itself
// ---------------------------------------------------------------------------

// TestTheAdmissionTableAndTheRunnerAgree is THE regression test. The host's
// toolkit table declared seven tools while EngineTools listed three, so four
// were admitted at the door and refused at the last gate.
func TestEveryAdmittedToolIsServed(t *testing.T) {
	served := map[string]bool{}
	for _, name := range run.EngineTools {
		served[name] = true
	}
	for _, family := range deepwikiToolkits().Families {
		for _, tool := range family.Tools {
			if !served[tool] {
				t.Errorf("toolkit family %q admits %q and no runner serves it", family.Name, tool)
			}
		}
	}
	// And the other way: a tool nothing admits would be dead code with a
	// name, reachable by no caller.
	admitted := map[string]bool{}
	for _, family := range deepwikiToolkits().Families {
		for _, tool := range family.Tools {
			admitted[tool] = true
		}
	}
	for _, name := range run.EngineTools {
		if !admitted[name] {
			t.Errorf("the runner serves %q and no toolkit family admits it", name)
		}
	}
	// resolve_wiki is an internal step, so it is on the sidecar's list and
	// NOT on the served list: a caller must not be able to invoke it.
	if served[run.ResolveWikiTool] {
		t.Error("resolve_wiki is invocable as a tool; it is an internal resolution step")
	}
	sidecar := map[string]bool{}
	for _, name := range run.SidecarTools {
		sidecar[name] = true
	}
	if !sidecar[run.ResolveWikiTool] {
		t.Error("the sidecar does not serve resolve_wiki, so resolution cannot reach a model")
	}
}

// TestTheFixtureRunnerServesTheWholeTable pins the same property for the
// runner an E2E stack actually runs.
func TestTheFixtureRunnerServesTheWholeTable(t *testing.T) {
	runner := run.NewFixtureRunner(spi.Settings{GitAllowlist: "*"}, 0)
	for _, tool := range run.EngineTools {
		if _, err := invokeFamily(t, runner, wikiQueryFamily(), tool, queryRequest(map[string]any{})); err != nil {
			if spi.KindOf(err) == spi.KindNotFound && strings.Contains(err.Error(), "Unknown tool") {
				t.Errorf("the fixture runner does not serve %q", tool)
			}
		}
	}
	if _, ok := runner.Tools[run.ResolveWikiTool]; ok {
		t.Error("the fixture runner exposes resolve_wiki as a tool")
	}
}

// ---------------------------------------------------------------------------
// list_wikis
// ---------------------------------------------------------------------------

func TestListWikisRendersTheRecordedText(t *testing.T) {
	fixture := wikiQueryFixture(t, "list_wikis")
	runner := wikiRunner(registryBucket(t, fixture), run.WikiQueryDeps{})

	for _, tc := range []struct {
		name       string
		parameters map[string]any
	}{
		{"compact", map[string]any{}},
		{"include_metadata", map[string]any{"include_metadata": true}},
	} {
		wantType, want := recordedCase(t, fixture, tc.name)
		gotType, got := answer(t, runner, "list_wikis", tc.parameters)
		if gotType != wantType || got != want {
			t.Errorf("%s:\n got %q (%s)\nwant %q (%s)", tc.name, got, gotType, want, wantType)
		}
	}

	empty := wikiRunner(newStore(map[string]string{
		run.RegistryPath: `{"schema_version":1,"wikis":[]}`}), run.WikiQueryDeps{})
	wantType, want := recordedCase(t, fixture, "empty_registry")
	if gotType, got := answer(t, empty, "list_wikis", map[string]any{}); got != want || gotType != wantType {
		t.Errorf("empty registry: %q (%s)", got, gotType)
	}
}

// TestListWikisFallsBackToTheManifests is the port's own behaviour, and the
// only reason the tool answers anything on this platform: ADR-0022 dropped
// the registry WRITE, so `_registry/wikis.json` does not exist in a bucket
// this platform filled. A port that read only the registry would answer "no
// wikis" for every wiki it had just generated.
func TestListWikisFallsBackToTheManifests(t *testing.T) {
	manifest := func(id, repository, title string) string {
		encoded, _ := json.Marshal(map[string]any{
			"wiki_id": id, "repository": repository, "branch": "main",
			"wiki_title": title, "description": "", "created_at": "2026-01-01T00:00:00Z",
			"provider_type": "github", "pages": []string{"wiki_pages/a.md"},
		})
		return string(encoded)
	}
	store := newStore(map[string]string{
		"acme--e2e-service--main/wiki_manifest_1.json":  manifest("acme--e2e-service--main", "acme/e2e-service", "E2E Service Wiki"),
		"acme--e2e-service--main/wiki_pages/a.md":       "# a",
		"acme--other--main/wiki_manifest_2026.json":     manifest("acme--other--main", "acme/other", "Other"),
		"acme--e2e-service--main/analysis/summary.json": `{"not":"a manifest"}`,
	})
	runner := wikiRunner(store, run.WikiQueryDeps{})
	_, got := answer(t, runner, "list_wikis", map[string]any{})
	want := "Available wikis:\n- acme--e2e-service--main: E2E Service Wiki\n- acme--other--main: Other"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	// An analysis file is not a manifest, and a manifest that will not parse
	// is skipped rather than failing the listing.
	store.objects["acme--broken--main/wiki_manifest_x.json"] = "{not json"
	if _, got := answer(t, runner, "list_wikis", map[string]any{}); got != want {
		t.Fatalf("an unreadable manifest changed the listing: %q", got)
	}
}

// TestTheCompactListingCutsADescriptionAtAHundredRunes pins the boundary the
// recorded registry does not reach: every description in the fixture is
// shorter than the cut, so a wrong limit survives it. The cut is a RUNE
// slice because the legacy `description[:100]` is Python's, and slicing
// bytes would split a multi-byte character in half.
func TestTheCompactListingCutsADescriptionAtAHundredRunes(t *testing.T) {
	long := strings.Repeat("é", 130)
	got := run.RenderWikiList([]map[string]any{
		{"id": "acme--long--main", "display_name": "long", "description": long},
	}, false)
	want := "Available wikis:\n- acme--long--main: long - " + strings.Repeat("é", 100) + "..."
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	// A description SHORTER than the cut is not padded, and an empty one adds
	// no separator at all — the second fixture wiki's shape.
	if got := run.RenderWikiList([]map[string]any{
		{"id": "acme--short--main", "display_name": "short", "description": ""},
	}, false); got != "Available wikis:\n- acme--short--main: short" {
		t.Fatalf("%q", got)
	}
}

func TestListWikisSaysWhyItCannotReachTheBucket(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runner  *run.Runner
		want    string
		request map[string]any
	}{
		{
			name:    "no transport at all",
			runner:  &run.Runner{Egress: spi.ParseEgressPolicy("*"), Tools: run.WikiQueryTools(nil, run.WikiQueryDeps{})},
			want:    "No wikis found (artifacts settings not configured)",
			request: map[string]any{"configuration": map[string]any{"parameters": map[string]any{}}},
		},
		{
			name:    "llm_settings with no api_base",
			runner:  wikiRunner(newStore(map[string]string{}), run.WikiQueryDeps{}),
			want:    "No wikis found (artifacts base_url not configured)",
			request: map[string]any{"configuration": map[string]any{"parameters": map[string]any{"llm_settings": map[string]any{"api_key": "k"}}}},
		},
	} {
		body, err := invokeFamily(t, tc.runner, wikiQueryFamily(), "list_wikis", tc.request)
		if err != nil {
			t.Fatalf("%s: a listing that cannot reach the bucket must COMPLETE, not fail: %v", tc.name, err)
		}
		var objects []map[string]any
		_ = json.Unmarshal([]byte(body["result"].(string)), &objects)
		if len(objects) != 1 || objects[0]["data"] != tc.want || objects[0]["object_type"] != "wiki_list" {
			t.Errorf("%s: %v", tc.name, objects)
		}
	}
}

// ---------------------------------------------------------------------------
// resolve_and_ask / resolve_and_deep_research
// ---------------------------------------------------------------------------

// resolver answers a fixed id and records what it was asked.
func resolver(id string, seen *map[string]any) run.Tool {
	return func(_ context.Context, arguments map[string]any, _ *spi.Context) (map[string]any, error) {
		if seen != nil {
			*seen = arguments
		}
		return map[string]any{"success": true, "wiki_id": id}, nil
	}
}

func canned(result map[string]any, seen *map[string]any) run.Tool {
	return func(_ context.Context, arguments map[string]any, _ *spi.Context) (map[string]any, error) {
		if seen != nil {
			*seen = arguments
		}
		return result, nil
	}
}

func TestResolveAndAskReproducesTheRecordedAnswerAndEngineCall(t *testing.T) {
	fixture := wikiQueryFixture(t, "resolve_and_ask")
	var askArguments map[string]any
	runner := wikiRunner(registryBucket(t, wikiQueryFixture(t, "list_wikis")), run.WikiQueryDeps{
		Resolve: resolver("acme--notes-service--main", nil),
		Ask:     canned(map[string]any{"success": true, "answer": "The notes service stores notes in Postgres."}, &askArguments),
	})
	wantType, want := recordedCase(t, fixture, "resolved")
	gotType, got := answer(t, runner, "resolve_and_ask",
		map[string]any{"question": "How does notes-service store notes?"})
	if gotType != wantType || got != want {
		t.Fatalf("\n got %q (%s)\nwant %q (%s)", got, gotType, want, wantType)
	}
	// The keyword set the legacy handler passed to `ask`, field for field.
	recorded := fixture["cases"].(map[string]any)["resolved"].(map[string]any)["ask_arguments"].(map[string]any)
	for _, key := range []string{"question", "repo_config", "chat_history", "k",
		"repo_identifier_override", "analysis_key_override", "embedding_model"} {
		if !jsonEqual(askArguments[key], recorded[key]) {
			t.Errorf("ask argument %s:\n got %#v\nwant %#v", key, askArguments[key], recorded[key])
		}
	}
}

// TestResolveAndAskCarriesTheLegacyEntryQuirks pins the two shapes an old
// registry row has: a `repo` with the branch and commit glued on, and no
// `branch` at all — the branch then comes out of the wiki id.
func TestResolveAndAskCarriesTheLegacyEntryQuirks(t *testing.T) {
	fixture := wikiQueryFixture(t, "resolve_and_ask")
	var askArguments map[string]any
	runner := wikiRunner(registryBucket(t, wikiQueryFixture(t, "list_wikis")), run.WikiQueryDeps{
		Ask: canned(map[string]any{"success": true, "answer": "The notes service stores notes in Postgres."}, &askArguments),
	})
	_, got := answer(t, runner, "resolve_and_ask",
		map[string]any{"question": "What is release-2?", "wiki_id_hint": "acme--billing--release-2"})
	_, want := recordedCase(t, fixture, "wiki_id_hint")
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	recorded := fixture["cases"].(map[string]any)["wiki_id_hint"].(map[string]any)["ask_arguments"].(map[string]any)
	for _, key := range []string{"repo_config", "repo_identifier_override"} {
		if !jsonEqual(askArguments[key], recorded[key]) {
			t.Errorf("%s:\n got %#v\nwant %#v", key, askArguments[key], recorded[key])
		}
	}
}

func TestResolveAndAskAnswersEveryRefusalInBand(t *testing.T) {
	fixture := wikiQueryFixture(t, "resolve_and_ask")
	registry := wikiQueryFixture(t, "list_wikis")
	for _, tc := range []struct {
		name   string
		deps   run.WikiQueryDeps
		store  *fakeStore
		record string
	}{
		{
			name:   "unresolved",
			deps:   run.WikiQueryDeps{Resolve: resolver("NONE", nil)},
			record: "unresolved",
		},
		{
			name:   "wiki_not_in_registry",
			deps:   run.WikiQueryDeps{Resolve: resolver("acme--nope--main", nil)},
			record: "unresolved",
		},
		{
			name:   "no_wikis",
			deps:   run.WikiQueryDeps{Resolve: resolver("x", nil)},
			store:  newStore(map[string]string{run.RegistryPath: `{"schema_version":1,"wikis":[]}`}),
			record: "no_wikis",
		},
		{
			name: "retrieval_failed",
			deps: run.WikiQueryDeps{
				Resolve: resolver("acme--notes-service--main", nil),
				Ask:     canned(map[string]any{"success": false, "error": "index unavailable"}, nil),
			},
			record: "retrieval_failed",
		},
	} {
		store := tc.store
		if store == nil {
			store = registryBucket(t, registry)
		}
		gotType, got := answer(t, wikiRunner(store, tc.deps), "resolve_and_ask",
			map[string]any{"question": questionFor(tc.name)})
		if gotType != "answer" {
			t.Errorf("%s: object_type %q", tc.name, gotType)
		}
		if tc.name == "wiki_not_in_registry" {
			// An id the resolver invented is NOT trusted: it falls back to
			// "could not determine", never to a query against a wiki that
			// does not exist.
			if !strings.HasPrefix(got, "Could not determine which wiki to query for:") {
				t.Errorf("%s: %q", tc.name, got)
			}
			continue
		}
		_, want := recordedCase(t, fixture, tc.record)
		if got != want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, want)
		}
	}
}

func questionFor(name string) string {
	switch name {
	case "unresolved":
		return "How does anything work?"
	default:
		return "?"
	}
}

func TestResolveAndDeepResearchWidensTheTokenBudget(t *testing.T) {
	fixture := wikiQueryFixture(t, "resolve_and_deep_research")
	var arguments map[string]any
	runner := wikiRunner(registryBucket(t, wikiQueryFixture(t, "list_wikis")), run.WikiQueryDeps{
		Resolve:      resolver("acme--notes-service--main", nil),
		DeepResearch: canned(map[string]any{"success": true, "report": "# Architecture\n\nThe notes service is three processes."}, &arguments),
	})
	wantType, want := recordedCase(t, fixture, "resolved")
	gotType, got := answer(t, runner, "resolve_and_deep_research", map[string]any{
		"question": "How is notes-service put together?", "research_type": "architecture"})
	if gotType != wantType || got != want {
		t.Fatalf("\n got %q (%s)\nwant %q (%s)", got, gotType, want, wantType)
	}
	recorded := fixture["cases"].(map[string]any)["resolved"].(map[string]any)["deep_research_arguments"].(map[string]any)
	for _, key := range []string{"llm_settings", "research_type", "enable_subagents", "k", "repo_config"} {
		if !jsonEqual(arguments[key], recorded[key]) {
			t.Errorf("deep_research argument %s:\n got %#v\nwant %#v", key, arguments[key], recorded[key])
		}
	}
	// The widening is a COPY: a caller's own settings must not be mutated.
	if _, widened := llmSettings["max_tokens"]; widened {
		t.Fatal("the request's llm_settings was mutated in place")
	}
	// A budget the facade already set is left alone.
	settings := map[string]any{"model_name": "m", "api_base": "http://e/llm/v1", "api_key": "k", "max_tokens": 512.0}
	body, err := invokeFamily(t, runner, wikiQueryFamily(), "resolve_and_deep_research", map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{"llm_settings": settings}},
		"parameters":    map[string]any{"question": "?"}})
	if err != nil {
		t.Fatal(err, body)
	}
	if arguments["llm_settings"].(map[string]any)["max_tokens"] != 512.0 {
		t.Fatalf("a configured max_tokens was overridden: %v", arguments["llm_settings"])
	}
}

func TestResolveRefusesAQuestionlessCall(t *testing.T) {
	runner := wikiRunner(registryBucket(t, wikiQueryFixture(t, "list_wikis")), run.WikiQueryDeps{})
	for _, tool := range []string{"resolve_and_ask", "resolve_and_deep_research"} {
		_, err := invokeFamily(t, runner, wikiQueryFamily(), tool, queryRequest(map[string]any{}))
		if spi.KindOf(err) != spi.KindValue || !strings.Contains(err.Error(), "Question is required") {
			t.Errorf("%s: %v", tool, err)
		}
	}
	// And one with no credentials at all.
	bare := &run.Runner{Egress: spi.ParseEgressPolicy("*"), Tools: run.WikiQueryTools(nil, run.WikiQueryDeps{})}
	_, err := invokeFamily(t, bare, wikiQueryFamily(), "resolve_and_ask", map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{}},
		"parameters":    map[string]any{"question": "?"}})
	if spi.KindOf(err) != spi.KindValue || !strings.Contains(err.Error(), "llm_settings is required") {
		t.Fatalf("%v", err)
	}
}

// ---------------------------------------------------------------------------
// delete_wiki
// ---------------------------------------------------------------------------

func TestDeleteWikiRemovesEveryKeyInOneBatch(t *testing.T) {
	registry := map[string]any{"schema_version": 1, "wikis": []any{
		map[string]any{"id": "acme--gone--main", "repo": "acme/gone", "branch": "main"},
		map[string]any{"id": "acme--stays--main", "repo": "acme/stays", "branch": "main"},
	}}
	encoded, _ := json.Marshal(registry)
	store := newStore(map[string]string{
		run.RegistryPath:                         string(encoded),
		"acme--gone--main/wiki_manifest_1.json":  "{}",
		"acme--gone--main/wiki_pages/a.md":       "# a",
		"acme--gone--main/analysis/summary.json": "{}",
		"acme--stays--main/wiki_manifest_1.json": "{}",
	})
	runner := wikiRunner(store, run.WikiQueryDeps{})
	gotType, got := answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--gone--main"})
	if gotType != "message" {
		t.Fatalf("object_type %q", gotType)
	}
	want := "Wiki 'acme--gone--main' successfully deleted.\n- Objects removed: 3\n- Registry updated: Yes"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	// The keys are READ AT DELETE TIME, so the analysis file no manifest
	// names goes too — and the neighbouring wiki does not.
	if _, alive := store.objects["acme--gone--main/analysis/summary.json"]; alive {
		t.Error("an object no manifest names survived")
	}
	if _, alive := store.objects["acme--stays--main/wiki_manifest_1.json"]; !alive {
		t.Error("a NEIGHBOURING wiki's object was deleted")
	}
	// And the registry row is gone, so the wiki stops listing.
	if strings.Contains(store.objects[run.RegistryPath], "acme--gone--main") {
		t.Error("the deleted wiki is still in the registry, so it still lists")
	}
}

// TestDeleteWikiNamesTheKeysThatSurvived is DeleteWikiButton's semantics and
// the divergence from legacy this port makes on purpose: legacy counted
// successes into a sentence, so a partial failure left a half-deleted wiki
// that still listed and named nothing the operator could act on.
func TestDeleteWikiNamesTheKeysThatSurvived(t *testing.T) {
	store := newStore(map[string]string{
		"acme--gone--main/wiki_manifest_1.json":  "{}",
		"acme--gone--main/analysis/summary.json": "{}",
	})
	store.undeletable["acme--gone--main/analysis/summary.json"] = true
	_, got := answer(t, wikiRunner(store, run.WikiQueryDeps{}), "delete_wiki",
		map[string]any{"wiki_id": "acme--gone--main"})
	if !strings.Contains(got, "acme--gone--main/analysis/summary.json") ||
		!strings.Contains(got, "the keys above remain") {
		t.Fatalf("a partial delete must name the survivors: %q", got)
	}
	// The recorded legacy answer named a COUNT and no key. Pinned so the
	// divergence stays a decision.
	defect := wikiQueryFixture(t, "delete_wiki")["legacy_defect"].(map[string]any)
	legacy := defect["result_objects"].([]any)[0].(map[string]any)["data"].(string)
	if !strings.Contains(legacy, "Deleted 0 artifacts") {
		t.Fatalf("the recorded legacy defect changed shape: %q", legacy)
	}
}

func TestDeleteWikiRefusesWhatItCannotDo(t *testing.T) {
	fixture := wikiQueryFixture(t, "delete_wiki")
	runner := wikiRunner(newStore(map[string]string{}), run.WikiQueryDeps{})

	if _, err := invokeFamily(t, runner, wikiQueryFamily(), "delete_wiki", queryRequest(map[string]any{})); spi.KindOf(err) != spi.KindValue ||
		!strings.Contains(err.Error(), "wiki_id is required") {
		t.Errorf("a nameless delete: %v", err)
	}
	bare := &run.Runner{Egress: spi.ParseEgressPolicy("*"), Tools: run.WikiQueryTools(nil, run.WikiQueryDeps{})}
	_, err := invokeFamily(t, bare, wikiQueryFamily(), "delete_wiki", map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{}},
		"parameters":    map[string]any{"wiki_id": "acme--gone--main"}})
	if spi.KindOf(err) != spi.KindValue || !strings.Contains(err.Error(), "llm_settings is required for artifact access") {
		t.Errorf("a delete with no transport: %v", err)
	}
	// A wiki with no objects is "not found", the legacy text.
	_, want := recordedCase(t, fixture, "unknown_wiki")
	_, got := answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--nope--main"})
	if got != strings.Replace(want, "acme--nope--main", "acme--nope--main", 1) {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	// A listing that fails is an ERROR, not an empty wiki: answering "not
	// found" for a bucket that could not be read would report a wiki as
	// already gone and let the caller stop looking.
	broken := newStore(map[string]string{})
	broken.listErr = errors.New("bucket unreachable")
	if _, err := invokeFamily(t, wikiRunner(broken, run.WikiQueryDeps{}), wikiQueryFamily(),
		"delete_wiki", queryRequest(map[string]any{"wiki_id": "acme--gone--main"})); err == nil {
		t.Error("an unreadable bucket answered a successful delete")
	}
}

// ---------------------------------------------------------------------------
// The keyword sets
// ---------------------------------------------------------------------------

func TestArgumentsForTheWikiQueryFamily(t *testing.T) {
	for _, tc := range []struct {
		tool   string
		params run.Params
		want   map[string]any
	}{
		{"list_wikis", run.Params{}, map[string]any{"include_metadata": false}},
		{"list_wikis", run.Params{"include_metadata": true}, map[string]any{"include_metadata": true}},
		{"resolve_and_ask", run.Params{"question": "?"}, map[string]any{
			"question": "?", "wiki_id_hint": nil, "chat_history": []any{}, "k": 15}},
		{"resolve_and_deep_research", run.Params{"question": "?"}, map[string]any{
			"question": "?", "wiki_id_hint": nil, "chat_history": []any{}, "k": 15,
			"research_type": "general", "enable_subagents": true}},
		{"delete_wiki", run.Params{}, map[string]any{"wiki_id": ""}},
	} {
		got := run.ArgumentsFor(tc.tool, tc.params)
		for key, want := range tc.want {
			if !jsonEqual(got[key], want) {
				t.Errorf("%s[%s]: got %#v, want %#v", tc.tool, key, got[key], want)
			}
		}
		// Every tool in this family reads the transport out of llm_settings,
		// so the keyword set must always carry it — an absent one is what
		// makes the tool answer "artifacts settings not configured".
		if _, ok := got["llm_settings"]; !ok {
			t.Errorf("%s: no llm_settings in the keyword set", tc.tool)
		}
	}
}

func jsonEqual(a, b any) bool {
	left, err1 := json.Marshal(a)
	right, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(left) == string(right)
}
