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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/engine"
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
	return map[string]any{"success": true, "deleted": true, "rows": map[string]any{"nodes": rows, "edges": 0.0, "wikis": 1.0}}
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
	// A partial failure is an invocation ERROR, not a success with a message:
	// a caller that reads the status and not the text must see it.
	_, err := invokeFamily(t, runner, wikiQueryFamily(), "delete_wiki",
		queryRequest(map[string]any{"wiki_id": "acme--gone--main"}))
	if err == nil {
		t.Fatal("artifacts deleted and the index not, yet the invocation succeeded")
	}
	got := err.Error()
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

// A rolling deploy: the host is newer than the engine, which answers
// "Unknown tool" for delete_wiki_index. The artifacts ARE deleted, but a wiki
// that had objects had an index too, and this engine cannot delete it: that is
// an ERROR (a caller reading only the status must not take it for a clean
// delete), with the way out in its text. A wiki that exists nowhere is still
// just "not found".
func TestDeleteWikiFailsWhenAnOldEngineCannotDeleteTheIndexOfAWikiThatHadObjects(t *testing.T) {
	old := func(context.Context, map[string]any, *spi.Context) (map[string]any, error) {
		return nil, spi.NewFailure(spi.KindRuntime, fmt.Errorf("The DeepWiki engine refused the invocation: HTTP 400 Unknown tool: delete_wiki_index: %w", engine.ErrUnknownTool))
	}
	store := indexedWiki()
	runner := wikiRunner(store, run.WikiQueryDeps{DeleteIndex: old})
	_, err := invokeFamily(t, runner, wikiQueryFamily(), "delete_wiki",
		queryRequest(map[string]any{"wiki_id": "acme--gone--main"}))
	if err == nil {
		t.Fatal("objects deleted, index left, yet the invocation succeeded")
	}
	for _, part := range []string{"deletion completed with errors", "Objects removed: 2",
		"search index cleanup unavailable on this engine version", "may remain", "upgraded"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("missing %q in %q", part, err)
		}
	}
	if len(store.objects) != 0 {
		t.Fatal("the artifacts are gone, as reported")
	}
	// No objects and no index anywhere: not found, and not a failure.
	runner = wikiRunner(newStore(map[string]string{}), run.WikiQueryDeps{DeleteIndex: old})
	_, got := answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--nope--main"})
	if !strings.Contains(got, "not found in registry") || !strings.Contains(got, "search index cleanup unavailable on this engine version") ||
		strings.Contains(got, "failed") {
		t.Fatalf("%q", got)
	}
}

// A no-objects delete whose index removal FAILS is an error too: the index may
// remain.
func TestDeleteWikiWithNoObjectsFailsWhenTheIndexRemovalFails(t *testing.T) {
	broken := &engineIndex{err: errors.New("the index database refused it")}
	runner := wikiRunner(newStore(map[string]string{}), run.WikiQueryDeps{DeleteIndex: broken.tool})
	_, err := invokeFamily(t, runner, wikiQueryFamily(), "delete_wiki",
		queryRequest(map[string]any{"wiki_id": "acme--gone--main"}))
	if err == nil || !strings.Contains(err.Error(), "removing its search index failed: the index database refused it") ||
		!strings.Contains(err.Error(), "Retry the delete") {
		t.Fatalf("%v", err)
	}
}

// The engine says what it serves (its health document) and the host decides
// from that: an engine that lists its tools without delete_wiki_index is not
// asked, whatever its refusal would have said; one that lists it is; one that
// lists nothing is asked and its refusal is the fallback.
func TestDeleteWikiDecidesFromTheEnginesToolList(t *testing.T) {
	deps := func(eng *engineIndex, served, known bool) run.WikiQueryDeps {
		return run.WikiQueryDeps{
			DeleteIndex:         eng.tool,
			IndexDeletionServed: func(context.Context) (bool, bool) { return served, known },
		}
	}
	remove := func(runner *run.Runner) (string, error) {
		body, err := invokeFamily(t, runner, wikiQueryFamily(), "delete_wiki",
			queryRequest(map[string]any{"wiki_id": "acme--gone--main"}))
		if err != nil {
			return "", err
		}
		var objects []map[string]any
		_ = json.Unmarshal([]byte(body["result"].(string)), &objects)
		return str(objects[0]["data"]), nil
	}

	// Listed without the tool: unavailable WITHOUT a call.
	absent := &engineIndex{result: removed(40)}
	_, err := remove(wikiRunner(indexedWiki(), deps(absent, false, true)))
	if err == nil || !strings.Contains(err.Error(), "unavailable on this engine version") {
		t.Fatalf("%v", err)
	}
	if calls := absent.calls(); len(calls) != 0 {
		t.Fatalf("an engine that does not list the tool was asked for it: %v", calls)
	}
	// Listed with it: called.
	present := &engineIndex{result: removed(40)}
	got, err := remove(wikiRunner(indexedWiki(), deps(present, true, true)))
	if err != nil || !strings.Contains(got, "Search index removed: Yes (41 rows)") {
		t.Fatalf("%q %v", got, err)
	}
	if calls := present.calls(); len(calls) != 1 {
		t.Fatalf("%v", calls)
	}
	// No list (an older release): tried, and the refusal is the fallback.
	unknown := &engineIndex{err: spi.NewFailure(spi.KindRuntime, fmt.Errorf("Unknown tool: %w", engine.ErrUnknownTool))}
	_, err = remove(wikiRunner(indexedWiki(), deps(unknown, false, false)))
	if err == nil || !strings.Contains(err.Error(), "unavailable on this engine version") {
		t.Fatalf("%v", err)
	}
	if calls := unknown.calls(); len(calls) != 1 {
		t.Fatalf("an engine that lists nothing must be asked: %v", calls)
	}
}

// `deleted` is true when ANY row was removed, even with no wikis row: the
// host reports the engine's counts, not a guessed extra row.
func TestDeleteWikiReportsTheEnginesActualCounts(t *testing.T) {
	stray := &engineIndex{result: map[string]any{"success": true, "deleted": true,
		"rows": map[string]any{"nodes": 0.0, "edges": 0.0, "embeddings": 0.0, "statistics": 3.0, "wikis": 0.0}}}
	runner := wikiRunner(newStore(map[string]string{}), run.WikiQueryDeps{DeleteIndex: stray.tool})
	_, got := answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--stray--main"})
	if !strings.Contains(got, "Search index removed: Yes (3 rows)") {
		t.Fatalf("%q", got)
	}
}

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

// ---------------------------------------------------------------------------
// The toolkit no longer carries the project-wide delete
// ---------------------------------------------------------------------------

func TestTheWikiQueryToolkitDoesNotListProjectDeletion(t *testing.T) {
	for _, list := range [][]string{run.WikiQueryToolNames, run.EngineTools, deepwiki.Toolkits.Families[2].Tools} {
		for _, name := range list {
			if name == "delete_project_wikis" {
				t.Fatalf("delete_project_wikis is still in %v", list)
			}
		}
	}
	// And the runner does not serve it as a tool.
	sidecar := newFakeSidecar(t, []string{`{"result": {"success": true, "wikis": []}}`}, 0)
	server := platformServer(t, sidecar)
	body := invokeSigned(t, server, spi.Identity{ProjectID: "17"}, "delete_project_wikis")
	if body["status"] != "Error" {
		t.Fatalf("a toolkit call ran the project deletion: %v", body)
	}
	sidecar.mu.Lock()
	defer sidecar.mu.Unlock()
	if len(sidecar.requests) != 0 {
		t.Fatalf("the engine was called %d time(s)", len(sidecar.requests))
	}
}

// ---------------------------------------------------------------------------
// delete_wiki stops the wiki's running generations before it deletes
// ---------------------------------------------------------------------------

// generationTool is a generate_wiki that runs until it is stopped (honouring
// the checkpoint), or, with stubborn, until released (ignoring it).
func generationTool(stubborn bool, release <-chan struct{}) run.Tool {
	return func(ctx context.Context, _ map[string]any, tc *spi.Context) (map[string]any, error) {
		for {
			if stubborn {
				select {
				case <-release:
					return map[string]any{"success": true, "result": "done"}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			if err := tc.Checkpoint(); err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// deleteRig is a runner with a generate_wiki tool and the real delete_wiki,
// wired the way NewEngineRunner wires it (StopGenerations from the runner).
type deleteRig struct {
	runner  *run.Runner
	manager *spi.Manager
	store   *fakeStore
	engine  *engineIndex
	order   *orderLog
}

type orderLog struct {
	mu     sync.Mutex
	events []string
}

func (o *orderLog) add(event string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, event)
}

func (o *orderLog) all() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

func newDeleteRig(t *testing.T, stubborn bool, release <-chan struct{}) *deleteRig {
	t.Helper()
	rig := &deleteRig{store: newStore(map[string]string{
		"acme--e2e-service--main/wiki_manifest_1.json": "{}",
		"acme--e2e-service--main/wiki_pages/a.md":      "# a",
	}), engine: &engineIndex{result: removed(40)}, order: &orderLog{}}
	rig.runner = &run.Runner{Egress: spi.ParseEgressPolicy("*"), StopWait: 200 * time.Millisecond}
	generate := generationTool(stubborn, release)
	rig.runner.Tools = run.WikiQueryTools(storeFactory(rig.store), run.WikiQueryDeps{
		DeleteIndex: func(ctx context.Context, arguments map[string]any, tc *spi.Context) (map[string]any, error) {
			rig.order.add("index deleted")
			return rig.engine.tool(ctx, arguments, tc)
		},
		StopGenerations: rig.runner.StopGenerations,
	})
	rig.runner.Tools["generate_wiki"] = func(ctx context.Context, arguments map[string]any, tc *spi.Context) (map[string]any, error) {
		result, err := generate(ctx, arguments, tc)
		rig.order.add("generation ended")
		return result, err
	}
	rig.manager = spi.NewManager(nil, time.Hour, nil)
	rig.manager.Start(t.Context())
	t.Cleanup(rig.manager.Stop)
	rig.runner.AttachManager(rig.manager)
	return rig
}

// generate starts a generation of acme/e2e-service on main for the project
// the organization names, and waits until it can be found by its labels.
func (r *deleteRig) generate(t *testing.T, organization string) *spi.Invocation {
	t.Helper()
	settings := map[string]any{"api_base": "http://elitea-main:8080/llm/v1", "api_key": "minted", "organization": organization}
	request := fixtureRequest("GO", settings)
	invocation, err := r.manager.Submit(t.Context(), "Wikis", "generate_wiki", func(ctx context.Context, tc *spi.Context) (map[string]any, error) {
		return r.runner.Invoke(ctx, spi.Invoke{Family: spi.Family{Name: "main"}, Toolkit: "Wikis", Tool: "generate_wiki", Request: request}, tc)
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		found := false
		r.manager.StopMatching(t.Context(), func(running spi.RunningInvocation) bool {
			found = found || (running.ID == invocation.ID && running.Labels["project"] == organization && running.Labels["wiki"] != "")
			return false
		}, 0)
		if found {
			return invocation
		}
		if time.Now().After(deadline) {
			t.Fatal("the generation never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *deleteRig) deleteWiki(t *testing.T, wiki string) (string, error) {
	t.Helper()
	body, err := invokeThrough(t, r.runner, r.manager, "delete_wiki", queryRequest(map[string]any{"wiki_id": wiki}))
	if err != nil {
		return "", err
	}
	var objects []map[string]any
	_ = json.Unmarshal([]byte(body["result"].(string)), &objects)
	return str(objects[0]["data"]), nil
}

// invokeThrough runs a wiki_query tool on a given runner and manager.
func invokeThrough(t *testing.T, runner *run.Runner, manager *spi.Manager, tool string, request map[string]any) (map[string]any, error) {
	t.Helper()
	invocation, err := manager.Submit(t.Context(), "Wikis", tool, func(ctx context.Context, tc *spi.Context) (map[string]any, error) {
		return runner.Invoke(ctx, spi.Invoke{Family: wikiQueryFamily(), Toolkit: "Wikis", Tool: tool, Request: request}, tc)
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		body, err := manager.Poll(t.Context(), "Wikis", tool, invocation.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch body["status"] {
		case "Completed":
			return body, nil
		case "Error":
			return body, fmt.Errorf("%s", body["result"])
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the invocation never settled")
	return nil, nil
}

func TestDeleteWikiStopsThatWikisGenerationBeforeDeletingAnything(t *testing.T) {
	rig := newDeleteRig(t, false, nil)
	// Project 90200 is the organization queryRequest and fixtureRequest share.
	mine := rig.generate(t, "90200")
	otherProject := rig.generate(t, "555")

	got, err := rig.deleteWiki(t, "acme--e2e-service--main")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "successfully deleted") {
		t.Fatalf("%q", got)
	}
	// The generation ended BEFORE the engine was asked to delete the index.
	order := rig.order.all()
	if len(order) < 2 || order[0] != "generation ended" || order[len(order)-1] != "index deleted" {
		t.Fatalf("order %v", order)
	}
	if body, _ := rig.manager.Poll(t.Context(), "Wikis", "generate_wiki", mine.ID); body["status"] != "Error" {
		t.Fatalf("the wiki's generation was not stopped: %v", body)
	}
	// Another PROJECT's generation of the same repository is not touched.
	if body, _ := rig.manager.Poll(t.Context(), "Wikis", "generate_wiki", otherProject.ID); body["status"] == "Error" || body["status"] == "Completed" {
		t.Fatalf("another project's generation was stopped: %v", body)
	}
	if rig.manager.InFlight() != 1 {
		t.Fatalf("%d running, want only the other project's", rig.manager.InFlight())
	}
}

func TestDeleteWikiLeavesAnotherWikisGenerationAlone(t *testing.T) {
	rig := newDeleteRig(t, false, nil)
	mine := rig.generate(t, "90200")
	if _, err := rig.deleteWiki(t, "acme--some-other-wiki--main"); err != nil {
		t.Fatal(err)
	}
	if body, _ := rig.manager.Poll(t.Context(), "Wikis", "generate_wiki", mine.ID); body["status"] == "Error" {
		t.Fatalf("a generation of ANOTHER wiki was stopped: %v", body)
	}
}

func TestDeleteWikiIsRefusedAndDeletesNothingWhileAGenerationWillNotStop(t *testing.T) {
	release := make(chan struct{})
	rig := newDeleteRig(t, true, release)
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			close(release)
		}
	}
	// Cleanups run last-in first-out: release the tool before the manager
	// waits for it.
	t.Cleanup(releaseOnce)
	rig.generate(t, "90200")

	_, err := rig.deleteWiki(t, "acme--e2e-service--main")
	if err == nil || !strings.Contains(err.Error(), "a generation is still running; retry") {
		t.Fatalf("%v", err)
	}
	if len(rig.store.objects) != 2 {
		t.Fatalf("objects were deleted while a generation runs: %v", rig.store.objects)
	}
	if calls := rig.engine.calls(); len(calls) != 0 {
		t.Fatalf("the index was deleted while a generation runs: %v", calls)
	}
	// Once the generation has ended, the same call goes through.
	releaseOnce()
	deadline := time.Now().Add(5 * time.Second)
	for rig.manager.InFlight() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got, err := rig.deleteWiki(t, "acme--e2e-service--main")
	if err != nil || !strings.Contains(got, "successfully deleted") {
		t.Fatalf("%q %v", got, err)
	}
}
