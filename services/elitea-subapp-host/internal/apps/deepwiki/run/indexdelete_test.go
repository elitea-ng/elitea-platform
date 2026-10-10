package run_test

// Deleting a wiki's search index (issue #1243, ADR-0031 phase D0).
//
// delete_wiki used to remove the artifact objects only; the engine's index
// of the wiki (nodes, edges, embeddings, BM25 rows) stayed for ever. It now
// calls the engine's delete_wiki_index after the objects are gone and reports
// both halves. delete_project_wikis is the entry point for project
// deprovisioning: a platform call, signed with the project and no user.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// engineIndex stands in for the engine's delete_wiki_index.
type engineIndex struct {
	mu     sync.Mutex
	wikis  []string
	result map[string]any
	err    error
}

func (e *engineIndex) tool(_ context.Context, arguments map[string]any, _ *spi.Context) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.wikis = append(e.wikis, arguments["wiki_id"].(string))
	if e.err != nil {
		return nil, e.err
	}
	return e.result, nil
}

func (e *engineIndex) calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.wikis...)
}

func indexedWiki() *fakeStore {
	return newStore(map[string]string{
		"acme--gone--main/wiki_manifest_1.json": "{}",
		"acme--gone--main/wiki_pages/a.md":      "# a",
	})
}

func removed(rows float64) map[string]any {
	return map[string]any{"success": true, "deleted": true, "rows": map[string]any{"nodes": rows, "edges": 0.0}}
}

func TestDeleteWikiReportsTheArtifactsAndTheIndex(t *testing.T) {
	engine := &engineIndex{result: removed(40)}
	store := indexedWiki()
	runner := wikiRunner(store, run.WikiQueryDeps{DeleteIndex: engine.tool})
	_, got := answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--gone--main"})
	want := "Wiki 'acme--gone--main' successfully deleted.\n- Objects removed: 2\n- Registry updated: No\n- Search index removed: Yes (41 rows)"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	if calls := engine.calls(); len(calls) != 1 || calls[0] != "acme--gone--main" {
		t.Fatalf("the engine was asked to delete %v", calls)
	}
	// The index is deleted AFTER the artifacts: by then the bucket is empty.
	if len(store.objects) != 0 {
		t.Fatalf("objects survived: %v", store.objects)
	}
}

func TestDeleteWikiSaysWhenThereWasNoIndex(t *testing.T) {
	engine := &engineIndex{result: map[string]any{"success": true, "deleted": false, "rows": map[string]any{}}}
	runner := wikiRunner(indexedWiki(), run.WikiQueryDeps{DeleteIndex: engine.tool})
	_, got := answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--gone--main"})
	if !strings.HasSuffix(got, "- Search index removed: No (the wiki had no search index)") {
		t.Fatalf("%q", got)
	}
}

// A failed index delete leaves the half-deleted state the tool exists to
// avoid, so it is named, with the way out; and the retry, which finds no
// objects, finishes the job.
func TestDeleteWikiNamesAFailedIndexDeleteAndTheRetryFinishesIt(t *testing.T) {
	engine := &engineIndex{err: errors.New("the index database refused it")}
	store := indexedWiki()
	runner := wikiRunner(store, run.WikiQueryDeps{DeleteIndex: engine.tool})
	_, got := answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--gone--main"})
	for _, part := range []string{"deletion completed with errors", "Objects removed: 2", "Search index: removing it failed: the index database refused it", "Retry the delete"} {
		if !strings.Contains(got, part) {
			t.Errorf("missing %q in %q", part, got)
		}
	}
	if len(store.objects) != 0 {
		t.Fatal("the artifacts are gone, as reported")
	}

	engine.mu.Lock()
	engine.err, engine.result = nil, removed(40)
	engine.mu.Unlock()
	_, got = answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--gone--main"})
	if !strings.Contains(got, "has no objects in the bucket") || !strings.Contains(got, "Search index removed: Yes (41 rows)") {
		t.Fatalf("the retry did not remove the index: %q", got)
	}
}

// With objects left over, the wiki is still listed and answerable; the
// index goes with the last of them.
func TestDeleteWikiKeepsTheIndexWhileObjectsRemain(t *testing.T) {
	engine := &engineIndex{result: removed(40)}
	store := indexedWiki()
	store.undeletable["acme--gone--main/wiki_pages/a.md"] = true
	runner := wikiRunner(store, run.WikiQueryDeps{DeleteIndex: engine.tool})
	_, got := answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--gone--main"})
	if !strings.Contains(got, "the keys above remain") || !strings.Contains(got, "The search index was kept") {
		t.Fatalf("%q", got)
	}
	if calls := engine.calls(); len(calls) != 0 {
		t.Fatalf("the index was deleted while objects remain: %v", calls)
	}
}

// A wiki with no objects and no index is "not found", the legacy text.
func TestDeleteWikiWithNeitherObjectsNorIndexIsNotFound(t *testing.T) {
	engine := &engineIndex{result: map[string]any{"success": true, "deleted": false, "rows": map[string]any{}}}
	runner := wikiRunner(newStore(map[string]string{}), run.WikiQueryDeps{DeleteIndex: engine.tool})
	_, got := answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--nope--main"})
	if got != "Wiki 'acme--nope--main' not found in registry." {
		t.Fatalf("%q", got)
	}
	if calls := engine.calls(); len(calls) != 1 {
		t.Fatalf("the index of a wiki with no objects must still be looked for: %v", calls)
	}
}

// ---------------------------------------------------------------------------
// delete_project_wikis, through the SPI server and the engine socket
// ---------------------------------------------------------------------------

func platformServer(t *testing.T, sidecar *fakeSidecar) *spi.Server {
	t.Helper()
	settings, err := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", func(key string) (string, bool) {
		switch key {
		case "ELITEA_DEEPWIKI_ENGINE_SOCKET":
			return sidecar.socket, true
		case "ELITEA_DEEPWIKI_IDENTITY_SECRET":
			return identitySecret, true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := spi.NewServer(settings, deepwiki.App(run.NewEngineRunner(settings)), nil)
	if err != nil {
		t.Fatal(err)
	}
	server.Start(t.Context())
	t.Cleanup(server.Stop)
	return server
}

func invokeSigned(t *testing.T, server *spi.Server, identity spi.Identity, tool string) map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/tools/wiki_query/"+tool+"/invoke",
		bytes.NewReader([]byte(`{"parameters": {}}`)))
	spi.SignHeaders(request.Header, identity, []byte(identitySecret))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	var accepted map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &accepted)
	id, _ := accepted["invocation_id"].(string)
	if id == "" {
		t.Fatalf("not accepted: %d %s", recorder.Code, recorder.Body.String())
	}
	return pollUntilTerminal(t, server, "/tools/wiki_query/"+tool+"/invocations/"+id)
}

func TestProjectDeprovisioningDeletesTheProjectsWikiIndexes(t *testing.T) {
	sidecar := newFakeSidecar(t, []string{
		`{"result": {"success": true, "project_id": 17, "wikis": ["acme--a--main", "acme--b--main"], "builds": 0}}`,
	}, 0)
	server := platformServer(t, sidecar)

	// The platform's call: the project is signed, no user is.
	body := invokeSigned(t, server, spi.Identity{ProjectID: "17"}, "delete_project_wikis")
	if body["status"] != "Completed" {
		t.Fatalf("%v", body)
	}
	if !strings.Contains(body["result"].(string), "Project 17: removed the search index of 2 wiki(s).") {
		t.Fatalf("%v", body["result"])
	}
	sidecar.mu.Lock()
	defer sidecar.mu.Unlock()
	if len(sidecar.requests) != 1 {
		t.Fatalf("the engine got %d requests", len(sidecar.requests))
	}
	call := sidecar.requests[0]
	arguments, _ := call["arguments"].(map[string]any)
	if call["tool"] != "delete_project_wikis" || arguments[run.ProjectArgument] != "17" {
		t.Fatalf("the engine was called as %v", call)
	}
}

// A user session carries a user id; a project-wide delete is not theirs to
// run, and the engine is never called.
func TestAUserSessionCannotDeleteAProjectsWikiIndexes(t *testing.T) {
	sidecar := newFakeSidecar(t, []string{`{"result": {"success": true, "wikis": []}}`}, 0)
	server := platformServer(t, sidecar)
	body := invokeSigned(t, server, spi.Identity{ProjectID: "17", UserID: "5"}, "delete_project_wikis")
	if body["status"] != "Error" || body["error_category"] != "invalid_input" {
		t.Fatalf("a user session ran a platform operation: %v", body)
	}
	sidecar.mu.Lock()
	defer sidecar.mu.Unlock()
	if len(sidecar.requests) != 0 {
		t.Fatalf("the engine was called %d time(s)", len(sidecar.requests))
	}
}

// Without an identity secret the project is only llm_settings.organization,
// which a project-wide delete must not trust.
func TestAnUnverifiedHostRefusesAProjectWideDelete(t *testing.T) {
	sidecar := newFakeSidecar(t, []string{`{"result": {"success": true, "wikis": []}}`}, 0)
	runner := engineRunner(sidecar, &fakeArtifactClient{})
	if runner.VerifiedIdentity {
		t.Fatal("no identity secret is configured")
	}
	_, err := invokeFamily(t, runner, wikiQueryFamily(), "delete_project_wikis", queryRequest(map[string]any{}))
	if spi.KindOf(err) != spi.KindValue || !strings.Contains(err.Error(), "verified platform identity") {
		t.Fatalf("%v", err)
	}
	sidecar.mu.Lock()
	defer sidecar.mu.Unlock()
	if len(sidecar.requests) != 0 {
		t.Fatal("the engine was called")
	}
}

// The fixture runner has no engine: it refuses rather than claiming it
// deleted an index.
func TestTheFixtureRunnerHasNoIndexToDelete(t *testing.T) {
	runner := run.NewFixtureRunner(spi.Settings{GitAllowlist: "*"}, 0)
	runner.VerifiedIdentity = true
	_, err := invokeFamily(t, runner, wikiQueryFamily(), "delete_project_wikis", queryRequest(map[string]any{}))
	if err == nil {
		t.Fatal("a fixture runner claimed to delete an index")
	}
}
