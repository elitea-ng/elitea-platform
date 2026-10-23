package run_test

// tests/engine/test_legacy_runner.py, ported test for test against the same
// P0 fixture: composed_result.json holds the legacy composer's OUTPUT, and
// workerResult rebuilds the input that produced it from the recorded
// objects. Round-tripping that through the ported composer is the binding.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi"
)

var fixtures = filepath.Join("..", "..", "..", "..", "..", "..", "conformance", "provider", "fixtures", "deepwiki")

func composed(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtures, "generation", "composed_result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func recordedObjects(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, o := range composed(t)["result_objects"].([]any) {
		out = append(out, o.(map[string]any))
	}
	return out
}

// workerResult rebuilds the engine result the fixture's composition was
// recorded from.
func workerResult(t *testing.T) map[string]any {
	t.Helper()
	objects := recordedObjects(t)
	var artifacts []any
	var context map[string]any
	for _, obj := range objects {
		switch obj["object_type"] {
		case "wiki_page":
			artifacts = append(artifacts, map[string]any{"name": obj["name"], "type": "text/markdown", "data": obj["data"]})
		case "wiki_structure", "wiki_manifest":
			artifacts = append(artifacts, map[string]any{"name": obj["name"], "type": "application/json", "data": obj["data"]})
		case "repository_context":
			context = obj
		}
	}
	wikiID := strings.SplitN(context["name"].(string), "/", 2)[0]
	return map[string]any{
		"success":            true,
		"result":             objects[0]["data"],
		"artifacts":          artifacts,
		"repository_context": context["data"],
		"wiki_id":            wikiID,
	}
}

// asMaps round-trips composed objects through JSON, which is how the wire
// carries them and how the fixture stores them.
func asMaps(t *testing.T, objects []run.Object) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(objects)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGenerateWikiCompositionMatchesTheRecordedObjects(t *testing.T) {
	got := asMaps(t, run.ComposeResultObjects("generate_wiki", workerResult(t)))
	want := recordedObjects(t)
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("composition differs from the fixture\n got: %.1500s\nwant: %.1500s", g, w)
	}
	if got[0]["object_type"] != "message" || got[0]["result_target"] != "response" {
		t.Fatal("the message object is not first")
	}
}

// `wiki-artifacts` at invoke time, `wiki` in the descriptor: the legacy
// disagreement, pinned so a tidy-up has to be deliberate.
func TestEveryArtifactCarriesTheInvokeTimeBucket(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(fixtures, "descriptor", "legacy-v0", "provider_descriptor.json"))
	if err != nil {
		t.Fatal(err)
	}
	var descriptor map[string]any
	_ = json.Unmarshal(raw, &descriptor)
	declared := map[string]bool{}
	for _, toolkit := range descriptor["provided_toolkits"].([]any) {
		for _, tool := range toolkit.(map[string]any)["provided_tools"].([]any) {
			meta, _ := tool.(map[string]any)["tool_metadata"].(map[string]any)
			objects, _ := meta["result_objects"].([]any)
			for _, obj := range objects {
				if bucket, _ := obj.(map[string]any)["result_bucket"].(string); bucket != "" {
					declared[bucket] = true
				}
			}
		}
	}
	if !reflect.DeepEqual(declared, map[string]bool{"wiki": true}) || run.DefaultBucket != "wiki-artifacts" {
		t.Fatalf("declared %v, default %q", declared, run.DefaultBucket)
	}
	for _, obj := range run.ComposeResultObjects("generate_wiki", workerResult(t))[1:] {
		if obj.ResultBucket != run.DefaultBucket {
			t.Fatalf("%s carries bucket %q", obj.NameString(), obj.ResultBucket)
		}
	}
}

func TestPartialFailuresAreReportedInBand(t *testing.T) {
	result := workerResult(t)
	result["failed_pages"] = []any{map[string]any{"page_id": "1#2", "title": "Search Ranking", "status": "failed"}, "a bare string entry"}
	result["errors"] = []any{"llm timeout", "rate limited"}
	var messages []string
	for _, obj := range run.ComposeResultObjects("generate_wiki", result) {
		if obj.ObjectType == "message" {
			messages = append(messages, obj.Data)
		}
	}
	if !strings.HasPrefix(messages[1], "⚠️ Partial issues detected:") || !strings.Contains(messages[1], "Failed pages: 2") || !strings.Contains(messages[1], "Errors: 2") {
		t.Fatalf("summary %q", messages[1])
	}
	if messages[2] != "Failed pages:\n- 1#2 Search Ranking (failed)\n- a bare string entry" {
		t.Fatalf("failed pages %q", messages[2])
	}
	if messages[3] != "Errors:\n- llm timeout\n- rate limited" {
		t.Fatalf("errors %q", messages[3])
	}
}

func TestANamelessManifestIsRecognisedByItsBody(t *testing.T) {
	body := `{"wiki_version_id": "20260101T000000Z-abcdef12", "pages": []}`
	objects := run.ComposeResultObjects("generate_wiki", map[string]any{"success": true, "result": "ok",
		"artifacts": []any{map[string]any{"name": nil, "type": "application/json", "data": body}}})
	if objects[1].ObjectType != "wiki_manifest" || objects[1].NameString() != "wiki_manifest_20260101T000000Z-abcdef12.json" {
		t.Fatalf("%+v", objects[1])
	}
	objects = run.ComposeResultObjects("generate_wiki", map[string]any{"success": true, "result": "ok",
		"artifacts": []any{map[string]any{"name": "", "type": "application/json", "data": `{"wiki_title": "x"}`}}})
	if objects[1].ObjectType != "wiki_structure" || objects[1].NameString() != "wiki_structure.json" {
		t.Fatalf("%+v", objects[1])
	}
}

func TestArtifactsTheWorkerAlreadyUploadedAreSkipped(t *testing.T) {
	result := workerResult(t)
	for _, a := range result["artifacts"].([]any) {
		a.(map[string]any)["_uploaded_directly"] = true
	}
	var kinds []string
	for _, obj := range run.ComposeResultObjects("generate_wiki", result) {
		kinds = append(kinds, obj.ObjectType)
	}
	if !reflect.DeepEqual(kinds, []string{"message", "repository_context"}) {
		t.Fatal(kinds)
	}
}

func TestAskDeepResearchAndRepositoryContextRules(t *testing.T) {
	var sources []any
	for i := 0; i < 8; i++ {
		sources = append(sources, map[string]any{"source": "file" + string(rune('0'+i)) + ".py"})
	}
	ask := run.ComposeResultObjects("ask", map[string]any{"success": true, "answer": "Notes are stored in SQLite.", "sources": sources})
	if len(ask) != 2 || ask[0].Data != "Notes are stored in SQLite." || strings.Count(ask[1].Data, "- file") != 5 {
		t.Fatalf("ask %+v", ask)
	}
	if run.ComposeResultObjects("deep_research", map[string]any{"success": true, "report": "R", "answer": "A"})[0].Data != "R" ||
		run.ComposeResultObjects("deep_research", map[string]any{"success": true, "answer": "A"})[0].Data != "A" {
		t.Fatal("deep_research does not prefer report then answer")
	}
	// Only generate_wiki emitted the repository context, even when present.
	if objects := run.ComposeResultObjects("ask", map[string]any{"success": true, "answer": "x", "repository_context": "ctx"}); len(objects) != 1 {
		t.Fatalf("ask emitted %d objects", len(objects))
	}
}

func TestTheParameterMergeKeepsTheLegacyAsymmetry(t *testing.T) {
	merged := run.MergeParameters(map[string]any{"configuration": map[string]any{"parameters": map[string]any{"a": 1.0}}, "parameters": map[string]any{"b": 2.0}})
	if !reflect.DeepEqual(merged, run.Params{"a": 1.0, "b": 2.0}) {
		t.Fatal(merged)
	}
	// exclude_tests=false explicitly does NOT override a configured true.
	merged = run.MergeParameters(map[string]any{"configuration": map[string]any{"parameters": map[string]any{"exclude_tests": true}}, "parameters": map[string]any{"exclude_tests": false}})
	if merged["exclude_tests"] != true {
		t.Fatal("a falsy tool parameter overrode the configuration")
	}
	merged = run.MergeParameters(map[string]any{"configuration": map[string]any{"parameters": map[string]any{"exclude_tests": false}}, "parameters": map[string]any{"exclude_tests": true}})
	if merged["exclude_tests"] != true {
		t.Fatal("a truthy tool parameter did not override")
	}
}

func githubRequest(query string, extra map[string]any) map[string]any {
	parameters := map[string]any{
		"code_toolkit": map[string]any{"github_configuration": map[string]any{
			"url": "https://github.com", "repository": "acme/notes-service", "active_branch": "main"}},
	}
	for k, v := range extra {
		parameters[k] = v
	}
	return map[string]any{"configuration": map[string]any{"parameters": parameters}, "parameters": map[string]any{"query": query}}
}

func TestRepoConfigReproducesTheRecordedEngineCall(t *testing.T) {
	recorded := composed(t)["engine_call"].(map[string]any)["repo_config"]
	got := run.ExtractRepoConfig(run.MergeParameters(githubRequest("", nil))).Map()
	if !reflect.DeepEqual(got, recorded) {
		t.Fatalf("\n got %v\nwant %v", got, recorded)
	}
	// The GitHub precedence chain: a top-level repository beats the
	// github_repository spelling, and active_branch beats base_branch.
	top := run.ExtractRepoConfig(map[string]any{"code_toolkit": map[string]any{"github_configuration": map[string]any{},
		"repository": "acme/top", "github_repository": "acme/other", "base_branch": "release", "active_branch": "feature"}})
	if top.RepositoryString() != "acme/top" || top.BranchString() != "feature" {
		t.Fatalf("%+v", top)
	}
	for key, provider := range map[string]string{
		"github_configuration": "github", "gitlab_configuration": "gitlab",
		"bitbucket_configuration": "bitbucket", "ado_configuration": "ado_repos",
	} {
		config := run.ExtractRepoConfig(map[string]any{"code_toolkit": map[string]any{key: map[string]any{"url": "https://example.test"}}})
		if config.ProviderType != provider || !reflect.DeepEqual(config.ProviderConfig, map[string]any{"url": "https://example.test"}) {
			t.Errorf("%s: %+v", key, config)
		}
	}
}

func TestDestinationHostFollowsTheLegacyOrder(t *testing.T) {
	if h := run.DestinationHost(run.RepoConfig{ProviderType: "github", ProviderConfig: map[string]any{"url": "https://ghe.example/x"}}); h != "ghe.example" {
		t.Fatal(h)
	}
	if h := run.DestinationHost(run.RepoConfig{ProviderType: "gitlab", ProviderConfig: map[string]any{}, Repository: "https://gitlab.example/a/b.git"}); h != "gitlab.example" {
		t.Fatal(h)
	}
	if h := run.DestinationHost(run.RepoConfig{ProviderType: "ado_repos", ProviderConfig: map[string]any{}}); h != "dev.azure.com" {
		t.Fatal(h)
	}
	policy := spi.ParseEgressPolicy("github.com")
	if _, err := run.CheckEgress(policy, run.MergeParameters(githubRequest("", nil))); err != nil {
		t.Fatal(err)
	}
	if _, err := run.CheckEgress(policy, map[string]any{"code_toolkit": map[string]any{"gitlab_configuration": map[string]any{"url": "https://gitlab.com"}}}); spi.Classify(err) != spi.CategoryInvalidInput {
		t.Fatalf("a forbidden host was not refused as invalid_input: %v", err)
	}
	if _, err := run.CheckEgress(spi.ParseEgressPolicy(""), map[string]any{"question": "?"}); err != nil {
		t.Fatal("a request naming no repository was checked")
	}
}

// A runner over a stubbed generate_wiki, composing against the fixture and
// recording the exact keyword set the legacy handler passed.
func TestTheRunnerDispatchesAndComposes(t *testing.T) {
	var calls []map[string]any
	runner := &run.Runner{
		Tools: map[string]run.Tool{"generate_wiki": func(_ context.Context, arguments map[string]any, _ *spi.Context) (map[string]any, error) {
			calls = append(calls, arguments)
			return workerResult(t), nil
		}},
		Egress: spi.ParseEgressPolicy("github.com"),
		// No transport factory: this test pins COMPOSITION; the upload half
		// is fixture_test.go's with a fake client.
	}
	engineCall := composed(t)["engine_call"].(map[string]any)
	body, _ := invoke(t, runner, "generate_wiki", githubRequest("Document the notes service", map[string]any{
		"llm_settings": engineCall["llm_settings"], "embedding_model": engineCall["embedding_model"]}))
	if body["status"] != "Completed" || body["result_type"] != "String" {
		t.Fatalf("%v", body)
	}
	var objects []map[string]any
	_ = json.Unmarshal([]byte(body["result"].(string)), &objects)
	if !reflect.DeepEqual(objects, recordedObjects(t)) {
		t.Fatal("the composed result differs from the fixture")
	}
	if len(calls) != 1 {
		t.Fatal(len(calls))
	}
	gotKeys, wantKeys := map[string]bool{}, map[string]bool{}
	for k := range calls[0] {
		gotKeys[k] = true
	}
	for k := range engineCall {
		wantKeys[k] = true
	}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("keyword set %v, recorded %v", gotKeys, wantKeys)
	}
	if !reflect.DeepEqual(calls[0]["repo_config"], engineCall["repo_config"]) || calls[0]["run_in_subprocess"] != true {
		t.Fatalf("arguments %v", calls[0])
	}
}

// A generate_wiki result whose manifest lists no pages: measured on PR #725,
// where the engine's 404'd analysis call still produced "success" and an
// empty manifest, and the host stored it as a finished wiki.
func manifestResult(t *testing.T, manifestBody string) map[string]any {
	t.Helper()
	return map[string]any{
		"success": true,
		"result":  "Wiki generation completed successfully",
		"wiki_id": "acme--notes-service--main",
		"artifacts": []any{
			map[string]any{"name": "acme--notes-service--main/wiki_manifest_20260902T000000Z-abcdef12.json",
				"type": "application/json", "data": manifestBody},
			map[string]any{"name": "acme--notes-service--main/wiki_pages/overview.md", "type": "text/markdown", "data": "# Overview"},
		},
	}
}

func TestAWikiWithNoPagesIsRefusedBeforeAnythingIsUploaded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest string
		refused  bool
	}{
		{"an empty pages list", `{"wiki_version_id": "v1", "pages": []}`, true},
		{"no pages key at all", `{"wiki_version_id": "v1", "wiki_title": "Notes"}`, true},
		{"pages the worker filled in", `{"wiki_version_id": "v1", "pages": ["acme--notes-service--main/wiki_pages/overview.md"]}`, false},
		{"a manifest body that is not JSON", "not json at all", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeArtifactClient{}
			runner := &run.Runner{
				Tools: map[string]run.Tool{"generate_wiki": func(context.Context, map[string]any, *spi.Context) (map[string]any, error) {
					return manifestResult(t, tc.manifest), nil
				}},
				Egress:    spi.ParseEgressPolicy("github.com"),
				Artifacts: func(map[string]any) (run.ArtifactClient, error) { return client, nil },
			}
			body, err := invoke(t, runner, "generate_wiki", githubRequest("Document the notes service",
				map[string]any{"llm_settings": transport}))
			if !tc.refused {
				if err != nil || body["status"] != "Completed" {
					t.Fatalf("a wiki that has pages was refused: %v / %v", err, body)
				}
				if names := client.names(); len(names) != 2 {
					t.Fatalf("uploaded %v", names)
				}
				return
			}
			if err == nil || body["status"] != "Error" {
				t.Fatalf("a wiki with no pages completed: %v", body)
			}
			if body["error_category"] != spi.CategoryInferenceFailed || body["error_type"] != string(spi.KindRuntime) {
				t.Fatalf("category %v, type %v", body["error_category"], body["error_type"])
			}
			if !strings.Contains(str(body["result"]), "produced no pages") {
				t.Fatalf("message %v", body["result"])
			}
			// Nothing may land: a manifest in the bucket is a wiki the
			// browser cannot tell from a real one.
			if names := client.names(); len(names) != 0 {
				t.Fatalf("a refused run uploaded %v", names)
			}
		})
	}
}

func TestTheNoPagesRefusalCarriesTheEnginesLastWorkerLog(t *testing.T) {
	empty := `{"wiki_version_id": "v1", "pages": []}`
	for _, tc := range []struct {
		name   string
		extra  map[string]any
		expect string
	}{
		{"appended to an error the engine still carried",
			map[string]any{"error": "analysis step failed\nLast worker log: [worker] LLM call returned HTTP 404"},
			"Last worker log: [worker] LLM call returned HTTP 404"},
		{"carried in the errors list",
			map[string]any{"errors": []any{"page 1 skipped", "Last worker log: [worker] Done"}},
			"Last worker log: [worker] Done"},
		{"absent", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := manifestResult(t, empty)
			for k, v := range tc.extra {
				result[k] = v
			}
			objects := run.ComposeResultObjects("generate_wiki", result)
			err := run.CheckWikiHasPages("generate_wiki", objects, result)
			if err == nil {
				t.Fatal("an empty manifest was accepted")
			}
			if tc.expect == "" {
				if strings.Contains(err.Error(), "Last worker log:") {
					t.Fatalf("a worker log was invented: %v", err)
				}
				return
			}
			if !strings.HasSuffix(err.Error(), "\n"+tc.expect) {
				t.Fatalf("%q does not end in the worker log line %q", err, tc.expect)
			}
		})
	}
	// Only generate_wiki produces a wiki, so only it is guarded.
	if err := run.CheckWikiHasPages("ask", run.ComposeResultObjects("generate_wiki", manifestResult(t, empty)), nil); err != nil {
		t.Fatalf("ask was guarded: %v", err)
	}
}

func TestAnUnsuccessfulEngineResultRaisesTheRightCategory(t *testing.T) {
	for _, tc := range []struct {
		result   map[string]any
		kind     spi.Kind
		category string
	}{
		{map[string]any{"success": false, "error": "boom"}, spi.KindRuntime, "runtime_error"},
		{map[string]any{"success": false, "error": "bad query", "error_category": "invalid_input"}, spi.KindValue, "invalid_input"},
		{map[string]any{"success": false, "error": "[SERVICE_BUSY] too many jobs"}, spi.KindRuntime, "runtime_error"},
		{map[string]any{"success": false, "error": "[SERVICE_BUSY]"}, spi.KindRuntime, "service_busy"},
	} {
		runner := &run.Runner{Tools: map[string]run.Tool{"ask": func(context.Context, map[string]any, *spi.Context) (map[string]any, error) { return tc.result, nil }}}
		body, err := invoke(t, runner, "ask", map[string]any{"parameters": map[string]any{"question": "?"}})
		if err == nil || body["error_type"] != string(tc.kind) || body["error_category"] != tc.category {
			t.Errorf("%v: got %v / %v (%v)", tc.result, body["error_type"], body["error_category"], err)
		}
	}
	// The legacy defect, named: the marker is stripped before the classifier
	// looks for it, so only a BARE marker classifies as service_busy.
	explained := run.EngineError(map[string]any{"success": false, "error": "[SERVICE_BUSY] too many jobs"})
	bare := run.EngineError(map[string]any{"success": false, "error": "[SERVICE_BUSY]"})
	if explained.Error() != "too many jobs" || bare.Error() != "DeepWiki service is busy. Please try again later." {
		t.Fatalf("%q / %q", explained, bare)
	}
}

func TestAnUnknownToolIsRefusedBeforeTheEngineIsTouched(t *testing.T) {
	runner := &run.Runner{Tools: map[string]run.Tool{}}
	if _, err := invoke(t, runner, "nope", map[string]any{}); spi.KindOf(err) != spi.KindNotFound {
		t.Fatalf("%v", err)
	}
}

func TestTheQueryRewriteMergesOnlyAnExpandedReference(t *testing.T) {
	transformed, err := run.TransformQueryRequest(map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{
			"wikis_toolkit": map[string]any{"code_toolkit": map[string]any{"github_configuration": map[string]any{}}},
			"llm_settings":  map[string]any{"model_name": "gpt-4o"},
		}},
		"parameters": map[string]any{"question": "?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	merged := transformed["configuration"].(map[string]any)["parameters"].(map[string]any)
	if _, ok := merged["code_toolkit"]; !ok || !reflect.DeepEqual(merged["llm_settings"], map[string]any{"model_name": "gpt-4o"}) ||
		!reflect.DeepEqual(transformed["parameters"], map[string]any{"question": "?"}) {
		t.Fatalf("%v", transformed)
	}
	transformed, err = run.TransformQueryRequest(map[string]any{"configuration": map[string]any{"parameters": map[string]any{"deepwiki_toolkit": map[string]any{"code_toolkit": map[string]any{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := transformed["configuration"].(map[string]any)["parameters"].(map[string]any)["code_toolkit"]; !ok {
		t.Fatal("the legacy deepwiki_toolkit key was not accepted")
	}
	_, err = run.TransformQueryRequest(map[string]any{"configuration": map[string]any{"parameters": map[string]any{"wikis_toolkit": 42.0}}})
	if err == nil || !strings.Contains(err.Error(), "must arrive expanded") || spi.KindOf(err) != spi.KindValue {
		t.Fatalf("a bare id: %v", err)
	}
	_, err = run.TransformQueryRequest(map[string]any{"configuration": map[string]any{"parameters": map[string]any{}}})
	if err == nil || !strings.Contains(err.Error(), "wikis_toolkit parameter is required") {
		t.Fatalf("a missing reference: %v", err)
	}
	// Through the runner, the query family gets the rewrite and the main
	// family does not.
	seen := map[string]any{}
	runner := &run.Runner{Egress: spi.ParseEgressPolicy("*"), Tools: map[string]run.Tool{"ask": func(_ context.Context, arguments map[string]any, _ *spi.Context) (map[string]any, error) {
		seen = arguments
		return map[string]any{"success": true, "answer": "x"}, nil
	}}}
	_, err = invokeFamily(t, runner, spi.Family{Name: "query"}, "ask", map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{"wikis_toolkit": map[string]any{"code_toolkit": map[string]any{"github_configuration": map[string]any{"repository": "acme/x"}}}}},
		"parameters":    map[string]any{"question": "?"}})
	if err != nil || seen["repo_config"].(map[string]any)["provider_config"].(map[string]any)["repository"] != "acme/x" {
		t.Fatalf("query family: %v %v", err, seen)
	}
	if _, err = invokeFamily(t, runner, spi.Family{Name: "query"}, "ask", map[string]any{"parameters": map[string]any{"question": "?"}}); spi.KindOf(err) != spi.KindValue {
		t.Fatalf("a query without a reference was served: %v", err)
	}
}

// invoke runs one tool through a live invocation manager, the way the host
// does, and returns the terminal body and the thinking events.
func invoke(t *testing.T, runner spi.Runner, tool string, request map[string]any) (map[string]any, error) {
	t.Helper()
	return invokeFamily(t, runner, spi.Family{Name: "main"}, tool, request)
}

func invokeFamily(t *testing.T, runner spi.Runner, family spi.Family, tool string, request map[string]any) (map[string]any, error) {
	t.Helper()
	body, _, err := invokeWithEvents(t, runner, family, tool, request, "")
	return body, err
}
