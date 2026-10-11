package run

// The wiki_query family, ported from the legacy plugin's
// `_handle_wiki_query_tool` and the four handlers under it
// (deepwiki_plugin/methods/invoke.py:600-1238).
//
// THE REGRESSION THIS CLOSES. The host DECLARED this family in its
// admission table (deepwiki.go) and served none of it: EngineTools listed
// three tools, so `list_wikis` was admitted at the door and then refused as
// "Unknown tool" by the runner. An agent configured with a wiki_query
// toolkit got past every gate and failed at the last one.
//
// WHERE EACH TOOL RUNS, and why it is not all in the sidecar. Three of the
// four are ARTIFACT operations: a wiki is a set of keys under `{wiki_id}/`
// in the bucket, and listing wikis, reading their manifests and deleting
// one are reads and writes against the platform's artifact API. ADR-0023
// put that transport in the host — services/elitea-deepwiki's own README
// records it ("the host uploads, through the transport the facade hands
// over; no surface of this service sends X-SECRET or disables TLS
// verification"), and the legacy MiniArtifactClient is exactly the code
// that carried both of those defects. So the bucket half lives here, on
// the host's artifact client, and only the half that needs a MODEL — which
// wiki a free-text question is about — crosses to the engine, as the
// `resolve_wiki` tool.
//
// One legacy behaviour is deliberately NOT reproduced. `delete_wiki` called
// WikiRegistryManager.delete_wiki_with_artifacts, which called
// `client.list_artifacts` — a method MiniArtifactClient does not define. It
// raised AttributeError into the manager's own `except Exception`, so every
// legacy delete answered "deletion completed with errors", deleted nothing,
// and left the wiki listed. This port deletes for real, in ONE batch, and
// names the keys that survived, which is DeleteWikiButton's semantics
// (apps/elitea-web/src/widgets/deepwiki/ui/DeleteWikiButton.tsx).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/artifacts"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/engine"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// WikiQueryTools are the four tools of the wiki_query family, in the order
// the legacy handler listed them.
var WikiQueryToolNames = []string{"list_wikis", "resolve_and_ask", "resolve_and_deep_research", "delete_wiki"}

// ResolveWikiTool is the engine tool that decides WHICH wiki a question is
// about. It is the one part of this family that needs a model.
const ResolveWikiTool = "resolve_wiki"

// RegistryPath is where the legacy registry lived in the bucket.
const RegistryPath = "_registry/wikis.json"

// manifestKey matches a wiki manifest object key, the same expression the
// wiki browser uses (entities/wiki/api/wikiArtifactsApi.ts).
var manifestKey = regexp.MustCompile(`(^|/)wiki_manifest[^/]*\.json$`)

// WikiQueryDeps are the model-backed halves this family delegates: which
// wiki a question is about, and the two retrieval tools it then calls.
// They are Tools like any other, so the fixture table and the sidecar table
// supply the same three and the composed behaviour is identical.
type WikiQueryDeps struct {
	Resolve      Tool
	Ask          Tool
	DeepResearch Tool
	// DeleteIndex deletes one wiki's search index in the engine
	// (delete_wiki_index). Nil when the runner has no engine (the fixture
	// table): delete_wiki then deletes the artifacts only and says nothing
	// of an index. Deleting a whole project's indexes is NOT a toolkit tool;
	// it is the platform route (Runner.DeleteProject, spi/platform.go).
	DeleteIndex Tool
	// StopGenerations stops the running generate_wiki invocations of the
	// wiki in the invocation's project and waits a bounded time for them to
	// end; it fails with spi.ErrGenerationRunning when one is still running.
	// delete_wiki calls it BEFORE it deletes anything: a generation that
	// outlives the deletion publishes the wiki back. Nil: nothing to stop
	// (no engine, so no generation here).
	StopGenerations func(ctx context.Context, wikiID string) error
	// IndexDeletionServed reports whether the engine lists delete_wiki_index
	// among the tools it serves (its health document). known is false when it
	// does not say; the call is then tried and a refusal recognised.
	IndexDeletionServed func(ctx context.Context) (served, known bool)
}

// WikiQueryTools builds the family over an artifact transport and those
// three. The transport is a FACTORY, not a client: it is derived per
// invocation from the llm_settings the facade minted, and there is no
// deployment-wide credential to build one from.
func WikiQueryTools(transport ArtifactClientFactory, deps WikiQueryDeps) map[string]Tool {
	family := &wikiQuery{transport: transport, deps: deps}
	return map[string]Tool{
		"list_wikis":                family.listWikis,
		"resolve_and_ask":           family.resolveAndAsk,
		"resolve_and_deep_research": family.resolveAndDeepResearch,
		"delete_wiki":               family.deleteWiki,
	}
}

type wikiQuery struct {
	transport ArtifactClientFactory
	deps      WikiQueryDeps
}

// response is a terminal result carrying one response object of objectType.
// The legacy handlers wrote the object list themselves; here they answer a
// result dict and ComposeResultObjects renders it, so the whole family goes
// through the one composer and the one upload path.
func response(objectType, data string) map[string]any {
	return map[string]any{"success": true, "object_type": objectType, "data": data}
}

// store derives the artifact transport for one invocation, or reports why
// there is none. The two refusal texts are the legacy ones because they
// reach the caller verbatim.
func (q *wikiQuery) store(arguments map[string]any) (artifacts.Store, string) {
	llmSettings := object(arguments["llm_settings"])
	if len(llmSettings) == 0 {
		return nil, "artifacts settings not configured"
	}
	if q.transport == nil {
		return nil, "artifacts base_url not configured"
	}
	client, err := q.transport(llmSettings)
	if err != nil {
		return nil, fmt.Sprintf("artifacts client error: %v", err)
	}
	if client == nil {
		// ArtifactClientFrom answers nil without BOTH halves of the
		// transport, and the half a caller actually omits is the base URL.
		return nil, "artifacts base_url not configured"
	}
	store, ok := client.(artifacts.Store)
	if !ok {
		return nil, "the artifact transport cannot read or delete objects"
	}
	return store, ""
}

// listWikis is the legacy `_list_wikis`: the registry rendered as text, in
// one of two formats. Every "cannot reach the bucket" branch answers a
// COMPLETED result carrying the reason, not an error — the legacy handler
// did, and an agent that asks "what wikis are there" is better served by
// "none, and here is why" than by a failed invocation.
func (q *wikiQuery) listWikis(ctx context.Context, arguments map[string]any, tc *spi.Context) (map[string]any, error) {
	store, unavailable := q.store(arguments)
	if store == nil {
		return response("wiki_list", "No wikis found ("+unavailable+")"), nil
	}
	wikis, err := LoadWikiRegistry(ctx, store, DefaultBucket)
	if err != nil {
		return response("wiki_list", fmt.Sprintf("No wikis found (artifacts client error: %v)", err)), nil
	}
	if err := tc.Checkpoint(); err != nil {
		return nil, err
	}
	return response("wiki_list", RenderWikiList(wikis, Truthy(arguments["include_metadata"]))), nil
}

// RenderWikiList is the legacy text, both formats, byte for byte.
func RenderWikiList(wikis []map[string]any, includeMetadata bool) string {
	if len(wikis) == 0 {
		return "No wikis have been generated yet. Use the Deepwiki toolkit to generate wikis first."
	}
	if includeMetadata {
		lines := []string{"# Available Wikis\n"}
		for _, wiki := range wikis {
			lines = append(lines,
				"## "+stringOr(wiki, "id", "unknown"),
				"- **Repository**: "+stringOr(wiki, "repo", "N/A"),
				"- **Branch**: "+stringOr(wiki, "branch", "N/A"),
				"- **Title**: "+stringOr(wiki, "display_name", "N/A"),
				"- **Description**: "+stringOr(wiki, "description", "N/A"),
				"- **Created**: "+stringOr(wiki, "created_at", "N/A"),
				"")
		}
		return strings.Join(lines, "\n")
	}
	lines := []string{"Available wikis:"}
	for _, wiki := range wikis {
		wikiID := stringOr(wiki, "id", "unknown")
		title := str(wiki["display_name"])
		// The legacy slice is `[:100]` on a Python str, i.e. RUNES; slicing
		// bytes would split a multi-byte character in a description.
		description := ""
		if runes := []rune(str(wiki["description"])); len(runes) > 0 {
			if len(runes) > 100 {
				runes = runes[:100]
			}
			description = " - " + string(runes) + "..."
		}
		lines = append(lines, "- "+wikiID+": "+title+description)
	}
	return strings.Join(lines, "\n")
}

// deleteWiki removes a wiki and every object under it.
func (q *wikiQuery) deleteWiki(ctx context.Context, arguments map[string]any, tc *spi.Context) (map[string]any, error) {
	wikiID := strings.TrimSpace(str(arguments["wiki_id"]))
	if wikiID == "" {
		return nil, spi.Failf(spi.KindValue, "wiki_id is required")
	}
	if len(object(arguments["llm_settings"])) == 0 {
		return nil, spi.Failf(spi.KindValue, "llm_settings is required for artifact access")
	}
	store, unavailable := q.store(arguments)
	if store == nil {
		return nil, spi.Failf(spi.KindRuntime, "Could not access wiki registry: %s", unavailable)
	}
	// Stop the wiki's running generations FIRST, and delete nothing while one
	// is still running: a generation that outlives the deletion publishes the
	// wiki's index back, for artifacts that are gone.
	if q.deps.StopGenerations != nil {
		if err := q.deps.StopGenerations(ctx, wikiID); err != nil {
			return nil, err
		}
	}

	// The key set is READ AT DELETE TIME, not taken from a manifest: a
	// manifest lists pages, and a wiki also holds analysis files, older
	// manifests and a repository context that no manifest names.
	prefix := wikiID + "/"
	objects, err := store.List(ctx, DefaultBucket, prefix)
	if err != nil {
		return nil, spi.Failf(spi.KindRuntime, "Could not list the wiki's objects: %v", err)
	}
	keys := make([]string, 0, len(objects))
	for _, o := range objects {
		if strings.HasPrefix(o.Key, prefix) {
			keys = append(keys, o.Key)
		}
	}
	if len(keys) == 0 {
		// No objects, but the search index can still exist: a delete that
		// failed between its two halves, or a wiki whose objects were removed
		// by hand. The index is cleaned up here, so a retry of a half-failed
		// delete finishes the job.
		index := q.deleteIndex(ctx, wikiID, tc)
		switch {
		case index.unavailable:
			// Nothing to clean here and the engine cannot say whether an
			// index exists: the wiki is as absent as the bucket shows. No
			// index is known to have existed, so this is not a failure.
			return response("message", fmt.Sprintf("Wiki '%s' not found in registry.\n- Search index: %s", wikiID, indexUnavailable)), nil
		case index.err != nil:
			// The wiki's index may remain and the caller must know: an error,
			// not a message a program reads as success.
			return nil, spi.Failf(spi.KindRuntime,
				"Wiki '%s' has no objects in the bucket, but removing its search index failed: %v\nRetry the delete to remove it.",
				wikiID, index.err)
		case index.removed:
			return response("message", fmt.Sprintf(
				"Wiki '%s' has no objects in the bucket.\n- Search index removed: Yes (%d rows)", wikiID, index.rows)), nil
		}
		return response("message", fmt.Sprintf("Wiki '%s' not found in registry.", wikiID)), nil
	}
	if err := tc.Checkpoint(); err != nil {
		return nil, err
	}
	if err := tc.Thinking(ctx, fmt.Sprintf("Deleting %d wiki objects", len(keys))); err != nil {
		return nil, err
	}

	deleted, failed, err := store.DeleteBatch(ctx, DefaultBucket, keys)
	if err != nil {
		return nil, spi.Failf(spi.KindRuntime, "Deleting wiki '%s' failed: %v", wikiID, err)
	}
	// The registry, when the bucket carries one. It lives at `_registry/…`,
	// not under `{wiki_id}/`, so the batch above never touches it — and a
	// wiki whose objects are gone but whose registry row is not still lists,
	// which is the half-deleted state this whole tool exists to avoid.
	unregistered := Unregister(ctx, store, DefaultBucket, wikiID)
	if len(failed) > 0 {
		// BY NAME, not as a count. A partial delete leaves a half-removed
		// wiki that still lists, and "3 of 5 deleted" tells the operator
		// nothing they can act on.
		lines := make([]string, 0, len(failed))
		for _, f := range failed {
			lines = append(lines, fmt.Sprintf("- %s: %s", f.Key, strings.TrimSpace(f.Code+" "+f.Message)))
		}
		message := fmt.Sprintf(
			"Wiki '%s' deletion completed with errors:\n%s\n\nDeleted %d of %d objects; the keys above remain.",
			wikiID, strings.Join(lines, "\n"), len(deleted), len(keys))
		if q.deps.DeleteIndex != nil {
			// The index is kept while the wiki still has objects: answering
			// questions from a half-deleted wiki is better than from none,
			// and the retry removes both.
			message += "\nThe search index was kept; delete the wiki again once the keys above are gone."
		}
		return response("message", message), nil
	}
	message := fmt.Sprintf(
		"Wiki '%s' successfully deleted.\n- Objects removed: %d\n- Registry updated: %s",
		wikiID, len(deleted), yesNo(unregistered))
	if q.deps.DeleteIndex == nil {
		return response("message", message), nil
	}
	index := q.deleteIndex(ctx, wikiID, tc)
	if index.err != nil {
		// The artifacts are gone and the index is not: the half-deleted
		// state this tool exists to avoid. It is an ERROR, so a caller that
		// reads the status and not the text (an agent, a script) sees the
		// failure; the text keeps what was done and the way out.
		return nil, spi.Failf(spi.KindRuntime,
			"Wiki '%s' deletion completed with errors:\n- Objects removed: %d\n- Registry updated: %s\n- Search index: removing it failed: %v\n\nRetry the delete to remove the search index.",
			wikiID, len(deleted), yesNo(unregistered), index.err)
	}
	if index.unavailable {
		// The wiki had objects, so an index very likely exists, and this
		// engine cannot delete it (an older release during a rolling
		// deploy). Reporting success would leave it there unseen.
		return nil, spi.Failf(spi.KindRuntime,
			"Wiki '%s' deletion completed with errors:\n- Objects removed: %d\n- Registry updated: %s\n- Search index: %s; the wiki's index may remain until the engine is upgraded\n\nRetry the delete once the engine is upgraded.",
			wikiID, len(deleted), yesNo(unregistered), indexUnavailable)
	}
	if index.removed {
		message += fmt.Sprintf("\n- Search index removed: Yes (%d rows)", index.rows)
	} else {
		message += "\n- Search index removed: No (the wiki had no search index)"
	}
	return response("message", message), nil
}

// indexOutcome is what the engine said about deleting a wiki's search index.
type indexOutcome struct {
	removed bool // any index row was removed (a wikis row or stray statistics)
	rows    int  // rows removed, as the engine counted them
	// unavailable is set when the engine does not serve index deletion (an
	// older release, during a rolling deploy). Whether that is a failure
	// depends on whether an index can exist: for a wiki that had objects it
	// is (the delete reports an error), for a wiki that is nowhere it is not.
	unavailable bool
	err         error
}

// indexUnavailable is the text a caller reads when the engine cannot delete
// an index.
const indexUnavailable = "search index cleanup unavailable on this engine version"

// deleteIndex deletes one wiki's search index in the engine. A host without
// an engine (DeleteIndex nil) reports nothing removed and no error.
func (q *wikiQuery) deleteIndex(ctx context.Context, wikiID string, tc *spi.Context) indexOutcome {
	if q.deps.DeleteIndex == nil {
		return indexOutcome{}
	}
	// The engine says what it serves (its health document): decide from that.
	// Only an engine that does not list its tools is asked by trying the call.
	if q.deps.IndexDeletionServed != nil {
		if served, known := q.deps.IndexDeletionServed(ctx); known && !served {
			slog.Warn("the engine does not list delete_wiki_index among its tools; its search index cannot be deleted by this host until the engine is upgraded", "wiki_id", wikiID)
			return indexOutcome{unavailable: true}
		}
	}
	if err := tc.Thinking(ctx, "Deleting the wiki's search index"); err != nil {
		return indexOutcome{err: err}
	}
	result, err := q.deps.DeleteIndex(ctx, map[string]any{"wiki_id": wikiID}, tc)
	// The last resort, for an engine that does not list its tools: the
	// refusal of a tool it does not serve.
	if errors.Is(err, engine.ErrUnknownTool) {
		slog.Warn("the engine does not serve delete_wiki_index; its search index cannot be deleted by this host until the engine is upgraded", "wiki_id", wikiID)
		return indexOutcome{unavailable: true}
	}
	if err != nil {
		return indexOutcome{err: err}
	}
	if !Truthy(result["success"]) {
		return indexOutcome{err: EngineError(result)}
	}
	// `deleted` is the engine's word for "any row was removed", also when
	// there was no wikis row; the counts are what it removed.
	rows := countRows(result["rows"])
	return indexOutcome{removed: Truthy(result["deleted"]) || rows > 0, rows: rows}
}

// countRows adds up the engine's per-table row counts (the wikis row is one
// of them, as "wikis").
func countRows(value any) int {
	total := 0
	for _, count := range object(value) {
		if n, ok := count.(float64); ok {
			total += int(n)
		}
	}
	return total
}

func yesNo(value bool) string {
	if value {
		return "Yes"
	}
	return "No"
}

// Unregister removes one wiki from `_registry/wikis.json` and reports
// whether it was there. A bucket with no registry — every bucket this
// platform writes, since the port dropped the registry write — answers
// false and nothing is created: writing a registry here would create a
// second source of truth for wikis that the generation path never updates.
func Unregister(ctx context.Context, store artifacts.Store, bucket, wikiID string) bool {
	body, err := store.Download(ctx, bucket, RegistryPath)
	if err != nil {
		return false
	}
	var registry map[string]any
	if json.Unmarshal(body, &registry) != nil {
		return false
	}
	entries, _ := registry["wikis"].([]any)
	kept := make([]any, 0, len(entries))
	removed := false
	for _, entry := range entries {
		if str(object(entry)["id"]) == wikiID {
			removed = true
			continue
		}
		kept = append(kept, entry)
	}
	if !removed {
		return false
	}
	registry["wikis"] = kept
	encoded, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return false
	}
	return store.Upload(ctx, bucket, RegistryPath, encoded) == nil
}

// resolveAndAsk and resolveAndDeepResearch are one flow with three
// differences the legacy handlers carried: the object type, the retrieval
// tool, and deep research's hardcoded max_tokens.
func (q *wikiQuery) resolveAndAsk(ctx context.Context, arguments map[string]any, tc *spi.Context) (map[string]any, error) {
	return q.resolveAndQuery(ctx, arguments, tc, "answer", "resolve_and_ask")
}

func (q *wikiQuery) resolveAndDeepResearch(ctx context.Context, arguments map[string]any, tc *spi.Context) (map[string]any, error) {
	return q.resolveAndQuery(ctx, arguments, tc, "report", "resolve_and_deep_research")
}

func (q *wikiQuery) resolveAndQuery(
	ctx context.Context, arguments map[string]any, tc *spi.Context, objectType, tool string,
) (map[string]any, error) {
	question := str(arguments["question"])
	if question == "" {
		return nil, spi.Failf(spi.KindValue, "Question is required")
	}
	llmSettings := object(arguments["llm_settings"])
	if len(llmSettings) == 0 {
		return nil, spi.Failf(spi.KindValue, "llm_settings is required")
	}
	store, unavailable := q.store(arguments)
	if store == nil {
		return nil, spi.Failf(spi.KindRuntime, "Could not access wiki registry: %s", unavailable)
	}
	wikis, err := LoadWikiRegistry(ctx, store, DefaultBucket)
	if err != nil {
		return nil, spi.Failf(spi.KindRuntime, "Could not access wiki registry: %v", err)
	}

	wikiID := strings.TrimSpace(str(arguments["wiki_id_hint"]))
	if wikiID == "" {
		if len(wikis) == 0 {
			return response(objectType,
				"No wikis available. Please generate wikis using the Deepwiki toolkit first."), nil
		}
		if err := tc.Thinking(ctx, "Resolving which wiki to query"); err != nil {
			return nil, err
		}
		resolved, err := q.resolve(ctx, question, wikis, llmSettings, tc)
		if err != nil {
			return nil, err
		}
		if resolved == "" {
			// The `ask` message names the candidates and the deep-research
			// one does not; that asymmetry is legacy's and is preserved.
			if objectType == "answer" {
				return response(objectType, fmt.Sprintf(
					"Could not determine which wiki to query for: '%s'. Available wikis: %s",
					question, strings.Join(WikiIDs(wikis), ", "))), nil
			}
			return response(objectType, fmt.Sprintf(
				"Could not determine which wiki to query for: '%s'", question)), nil
		}
		wikiID = resolved
	}

	entry := WikiEntry(wikis, wikiID)
	if entry == nil {
		return response(objectType, fmt.Sprintf("Wiki '%s' not found in registry.", wikiID)), nil
	}
	repoConfig, identifier := RepoConfigFor(entry, wikiID)

	call := map[string]any{
		"question":                 question,
		"llm_settings":             llmSettings,
		"embedding_model":          arguments["embedding_model"],
		"repo_config":              repoConfig,
		"chat_history":             get(arguments, "chat_history", []any{}),
		"k":                        get(arguments, "k", 15),
		"repo_identifier_override": identifier,
		"analysis_key_override":    entry["analysis_key"],
	}
	retrieve := q.deps.Ask
	if tool == "resolve_and_deep_research" {
		call["research_type"] = get(arguments, "research_type", "general")
		call["enable_subagents"] = get(arguments, "enable_subagents", true)
		// wiki_query carries no toolkit configuration, so the legacy handler
		// hardcoded a generous budget for a comprehensive report. Only when
		// the facade did not already set one.
		if _, present := llmSettings["max_tokens"]; !present {
			widened := map[string]any{}
			for k, v := range llmSettings {
				widened[k] = v
			}
			widened["max_tokens"] = 16384
			call["llm_settings"] = widened
		}
		retrieve = q.deps.DeepResearch
	}
	if retrieve == nil {
		return nil, spi.Failf(spi.KindNotFound, "Unknown tool: %s", tool)
	}
	if err := tc.Checkpoint(); err != nil {
		return nil, err
	}
	result, err := retrieve(ctx, call, tc)
	if err != nil {
		return nil, err
	}
	if result == nil || !Truthy(result["success"]) {
		message := "Unknown error"
		if result != nil && str(result["error"]) != "" {
			message = str(result["error"])
		}
		// A failed retrieval on a resolved wiki is reported IN BAND, as
		// legacy did: the invocation succeeded in deciding which wiki, and
		// the caller is told what went wrong inside it.
		if objectType == "answer" {
			return response(objectType, fmt.Sprintf("Failed to query wiki %s: %s", wikiID, message)), nil
		}
		return response(objectType, fmt.Sprintf("Failed deep research on wiki %s: %s", wikiID, message)), nil
	}
	if objectType == "answer" {
		return response(objectType, fmt.Sprintf("*Querying wiki: %s*\n\n%s",
			wikiID, stringOr(result, "answer", "Question answered successfully"))), nil
	}
	return response(objectType, fmt.Sprintf("*Deep research on wiki: %s*\n\n%s",
		wikiID, stringOr(result, "report", stringOr(result, "answer", "Research completed")))), nil
}

// resolve asks the engine which wiki the question is about, and validates
// the answer against the candidates — the legacy validation, fuzzy fallback
// included. An id the engine invented is NOT accepted: it would send the
// retrieval at a wiki that does not exist.
func (q *wikiQuery) resolve(
	ctx context.Context, question string, wikis []map[string]any, llmSettings map[string]any, tc *spi.Context,
) (string, error) {
	if q.deps.Resolve == nil {
		return "", spi.Failf(spi.KindNotFound, "Unknown tool: %s", ResolveWikiTool)
	}
	candidates := make([]any, 0, len(wikis))
	for _, wiki := range wikis {
		candidates = append(candidates, map[string]any{
			"wiki_id":     stringOr(wiki, "id", ""),
			"wiki_title":  str(wiki["display_name"]),
			"description": str(wiki["description"]),
		})
	}
	result, err := q.deps.Resolve(ctx, map[string]any{
		"question":     question,
		"wikis":        candidates,
		"llm_settings": llmSettings,
	}, tc)
	if err != nil {
		return "", err
	}
	if result == nil || !Truthy(result["success"]) {
		// Legacy swallowed a resolution failure and returned None, which
		// became "Could not determine which wiki". Kept: a model that would
		// not answer is not a reason to fail an invocation the caller wrote
		// correctly.
		return "", nil
	}
	answer := strings.Trim(strings.TrimSpace(str(result["wiki_id"])), `"'`)
	if answer == "" || answer == "NONE" {
		return "", nil
	}
	for _, id := range WikiIDs(wikis) {
		if id == answer {
			return id, nil
		}
	}
	lowered := strings.ToLower(answer)
	for _, id := range WikiIDs(wikis) {
		candidate := strings.ToLower(id)
		if strings.Contains(candidate, lowered) || strings.Contains(lowered, candidate) {
			return id, nil
		}
	}
	return "", nil
}

// WikiIDs lists the registry's ids, in order.
func WikiIDs(wikis []map[string]any) []string {
	ids := make([]string, 0, len(wikis))
	for _, wiki := range wikis {
		ids = append(ids, stringOr(wiki, "id", ""))
	}
	return ids
}

// WikiEntry finds one registry entry by id.
func WikiEntry(wikis []map[string]any, wikiID string) map[string]any {
	for _, wiki := range wikis {
		if stringOr(wiki, "id", "") == wikiID {
			return wiki
		}
	}
	return nil
}

// RepoConfigFor builds the repo_config the retrieval tools read, plus the
// canonical identifier override, from one registry entry. Both legacy
// quirks are kept: a `repo` carrying an embedded branch and commit
// ("owner/repo:branch:sha") is cut back to owner/repo, and a missing branch
// is parsed out of the wiki id.
func RepoConfigFor(entry map[string]any, wikiID string) (map[string]any, string) {
	repo := str(entry["repo"])
	if index := strings.Index(repo, ":"); index >= 0 {
		repo = repo[:index]
	}
	branch := str(entry["branch"])
	if branch == "" {
		branch = BranchFromWikiID(wikiID)
	}
	identifier := str(entry["canonical_repo_identifier"])
	if identifier == "" {
		identifier = repo + ":" + branch
	}
	return map[string]any{
		"provider_type": firstTruthy(entry["provider"], "github"),
		// Empty on purpose: the retrieval path reads the CACHED artifacts of
		// an already-generated wiki, so it needs no clone credential — and a
		// wiki_query toolkit has none to give.
		"provider_config": map[string]any{},
		"repository":      repo,
		"branch":          branch,
	}, identifier
}

// BranchFromWikiID is the legacy parse_wiki_id: `owner--repo--branch`, with
// the repo half allowed to carry its own `--` separators.
func BranchFromWikiID(wikiID string) string {
	parts := strings.Split(wikiID, "--")
	if len(parts) >= 3 {
		return parts[len(parts)-1]
	}
	return "main"
}

// LoadWikiRegistry answers what wikis the bucket holds.
//
// TWO SOURCES, and the second is not a fallback for robustness — it is the
// only one that answers on this platform. The legacy service kept
// `_registry/wikis.json` up to date from its own generation path;
// ADR-0022's port dropped that write (services/elitea-deepwiki's
// legacy_runner.py: "The registry write. Its successor is the `wikis` table
// from migration 0001"). A port that read only the registry would answer
// "no wikis" on every deployment, for every wiki it had just generated. So
// the registry is read when it is there — a bucket carried over from the
// legacy service still has one — and the manifests the generation path
// DOES write are read when it is not. That is the same set the wiki
// browser lists from.
func LoadWikiRegistry(ctx context.Context, store artifacts.Store, bucket string) ([]map[string]any, error) {
	if wikis, err := registryFile(ctx, store, bucket); err == nil && len(wikis) > 0 {
		return wikis, nil
	}
	return manifestRegistry(ctx, store, bucket)
}

func registryFile(ctx context.Context, store artifacts.Store, bucket string) ([]map[string]any, error) {
	body, err := store.Download(ctx, bucket, RegistryPath)
	if err != nil {
		return nil, err
	}
	var registry struct {
		Wikis []map[string]any `json:"wikis"`
	}
	if err := json.Unmarshal(body, &registry); err != nil {
		return nil, err
	}
	return registry.Wikis, nil
}

// manifestRegistry derives the registry from the manifests in the bucket.
func manifestRegistry(ctx context.Context, store artifacts.Store, bucket string) ([]map[string]any, error) {
	objects, err := store.List(ctx, bucket, "")
	if err != nil {
		return nil, err
	}
	// Sorted by key so a wiki with several manifests resolves the same way
	// on every call, and the LAST one wins — manifest names carry the
	// version, so the highest is the newest.
	keys := make([]string, 0, len(objects))
	for _, o := range objects {
		if manifestKey.MatchString(o.Key) {
			keys = append(keys, o.Key)
		}
	}
	sort.Strings(keys)

	entries := map[string]map[string]any{}
	var order []string
	for _, key := range keys {
		body, err := store.Download(ctx, bucket, key)
		if err != nil {
			// One unreadable manifest is not an unreadable bucket. Skipped,
			// so the wikis that DO read still list.
			continue
		}
		var manifest map[string]any
		if json.Unmarshal(body, &manifest) != nil {
			continue
		}
		id := str(manifest["wiki_id"])
		if id == "" {
			// The key's first segment is the wiki id by construction
			// ({wiki_id}/wiki_manifest_….json).
			if index := strings.Index(key, "/"); index > 0 {
				id = key[:index]
			}
		}
		if id == "" {
			continue
		}
		if _, seen := entries[id]; !seen {
			order = append(order, id)
		}
		entries[id] = map[string]any{
			"id":                        id,
			"repo":                      firstTruthy(manifest["repository"], manifest["canonical_repo_identifier"], ""),
			"branch":                    firstTruthy(manifest["branch"], BranchFromWikiID(id)),
			"provider":                  firstTruthy(manifest["provider_type"], "github"),
			"display_name":              firstTruthy(manifest["wiki_title"], id),
			"description":               firstTruthy(manifest["description"], ""),
			"created_at":                firstTruthy(manifest["created_at"], ""),
			"canonical_repo_identifier": manifest["canonical_repo_identifier"],
			"analysis_key":              manifest["analysis_key"],
		}
	}
	wikis := make([]map[string]any, 0, len(order))
	for _, id := range order {
		wikis = append(wikis, entries[id])
	}
	return wikis, nil
}
