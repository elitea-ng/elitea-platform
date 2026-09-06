package run

// The fixture runner: the real merge, source check, composition and upload
// over a canned knowledge graph.
//
// RUNNER=fixture is for a stack that must exercise the whole ingest → land →
// read path — the facade's source expansion and callback minting, this host's
// admission, composition and artifact upload, the bucket, and whatever browser
// screen reads the graph back — without the engine's dependency closure, a
// repository to clone or a model to extract entities with. It is Inventory's
// half of what internal/apps/deepwiki/run/fixture.go does for DeepWiki, and it
// exists for the same reason: the E2E stack has no engine image.
//
// WHAT IS CANNED IS ONLY WHAT THE ENGINE WOULD HAVE COMPUTED. The graph below
// is a constant; everything around it is production code. `run_ingestion`
// still refuses a body with no expanded source (CheckSource), still composes
// through ComposeResultObjects, and still uploads through the request's own
// artifact transport, so a test can predict every key that lands:
// graph.json, sources_status.json and one .ingestion-checkpoint-<source>.json.
//
// DETERMINISM IS THE POINT. A browser journey asserts on entity names, counts
// and object keys, so nothing here reads a clock, a random source or the
// filesystem. What varies with the request is only what the request decides:
// the source label an ingestion reports, the bucket the objects land in, and
// whether a read answers markdown or JSON (`output_format`, the legacy
// switch every read tool carries).
//
// THERE IS A SECOND FIXTURE RUNNER, in Python
// (services/elitea-inventory/src/elitea_inventory/fixture_runner.py), reached
// over the engine socket when a stack runs the sidecar. The two are kept in
// step BY HAND — the same trap DeepWiki records. This one is the richer of the
// two on purpose: it is the one a browser journey reads.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// FixtureEntity is one node of the canned graph, in the shape the legacy
// handlers emit: the retrieval tools return `id`, `name`, `type`, `layer` and
// the citation's `source_toolkit`, and `get_entity_content` returns `content`.
type FixtureEntity struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Layer    string `json:"layer"`
	Source   string `json:"source_toolkit"`
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// FixtureRelation is one edge, `source` → `target` under a relation type.
type FixtureRelation struct {
	From string `json:"source"`
	To   string `json:"target"`
	Type string `json:"relation_type"`
}

// FixtureEntities is the canned graph's nodes.
//
// Two source toolkits, three types and two layers, because every read tool
// below groups by one of those three and a graph with one of each cannot tell
// a correct grouping from a constant. `service.checkout` is deliberately the
// only entity with more than one inbound edge — it is what impact_analysis and
// get_entity_neighbors have something to say about.
var FixtureEntities = []FixtureEntity{
	{
		ID: "code:checkout-service", Name: "CheckoutService", Type: "class",
		Layer: "application", Source: "code", FilePath: "src/checkout/service.py",
		Content: "class CheckoutService:\n    \"\"\"Places an order.\"\"\"\n",
	},
	{
		ID: "code:place-order", Name: "place_order", Type: "function",
		Layer: "application", Source: "code", FilePath: "src/checkout/service.py",
		Content: "def place_order(cart):\n    return Order(cart)\n",
	},
	{
		ID: "code:order-model", Name: "Order", Type: "class",
		Layer: "domain", Source: "code", FilePath: "src/checkout/models.py",
		Content: "class Order:\n    items: list\n",
	},
	{
		ID: "code:payment-client", Name: "PaymentClient", Type: "class",
		Layer: "infrastructure", Source: "code", FilePath: "src/payments/client.py",
		Content: "class PaymentClient:\n    def charge(self, order): ...\n",
	},
	{
		ID: "docs:checkout-guide", Name: "Checkout guide", Type: "document",
		Layer: "documentation", Source: "docs", FilePath: "docs/checkout.md",
		Content: "# Checkout guide\n\nThe checkout service places an order.\n",
	},
	{
		ID: "docs:payments-guide", Name: "Payments guide", Type: "document",
		Layer: "documentation", Source: "docs", FilePath: "docs/payments.md",
		Content: "# Payments guide\n\nPayment is charged after the order lands.\n",
	},
}

// FixtureRelations is the canned graph's edges. The last two cross a source
// boundary, which is the only thing get_cross_source_relations can report.
var FixtureRelations = []FixtureRelation{
	{From: "code:checkout-service", To: "code:place-order", Type: "defines"},
	{From: "code:place-order", To: "code:order-model", Type: "returns"},
	{From: "code:place-order", To: "code:payment-client", Type: "calls"},
	{From: "docs:checkout-guide", To: "code:checkout-service", Type: "documents"},
	{From: "docs:payments-guide", To: "code:payment-client", Type: "documents"},
}

// FixturePresets are the two named ingestion presets the preset tools report.
var FixturePresets = map[string]string{
	"code":          "Parsers for source files, one entity per class and function.",
	"documentation": "Markdown and text files, one entity per document.",
}

// FixtureSteps is what each fixture tool emits before it answers, down the one
// channel the SPI has. A tool with no entry uses fixtureReadSteps: a read is
// two steps everywhere, and listing thirty identical entries would only invite
// them to drift.
var FixtureSteps = map[string][]string{
	"run_ingestion": {
		"Building the source toolkit",
		"Reading the source files",
		"Extracting entities",
		"Linking relations",
		"Detecting communities",
		"Writing graph.json",
	},
	"remove_source_entities": {"Finding the source's entities", "Rewriting the graph"},
	"rebuild_indices":        {"Reading the graph", "Rebuilding the indices"},
	"normalize_types":        {"Reading the graph", "Normalising the entity types"},
	"smart_normalize_types":  {"Reading the graph", "Normalising the entity types"},
	"cleanup_cache":          {"Reading the cache", "Removing the stale entries"},
	"investigate":            {"Planning the investigation", "Reading the graph", "Writing the answer"},
}

var fixtureReadSteps = []string{"Loading the graph", "Reading the graph"}

// SourceLabelFor names the source an ingestion ran against, derived from the
// EXPANDED source object the facade put in the body — `{type}:{id}` — so the
// label a test predicts is the one the request asked for rather than a
// constant that would pass with the expansion missing.
//
// CheckSource has already refused a body with no source object by the time any
// of this runs, so the fallback is reachable only for a source that carries
// neither field.
func SourceLabelFor(params Params) string {
	source := object(params["source"])
	kind := strings.TrimSpace(str(source["type"]))
	id := strings.TrimSpace(fmt.Sprint(firstTruthy(source["id"], source["toolkit_id"], "")))
	switch {
	case kind != "" && id != "":
		return kind + ":" + id
	case kind != "":
		return kind
	case id != "":
		return id
	default:
		return "fixture-source"
	}
}

// FixtureGraph is the graph document `run_ingestion` writes, for one source
// label. The `_metadata` block is what the read tools would have loaded.
func FixtureGraph(sourceLabel string) map[string]any {
	nodes := make([]any, 0, len(FixtureEntities))
	for _, entity := range FixtureEntities {
		nodes = append(nodes, map[string]any{
			"id": entity.ID, "name": entity.Name, "type": entity.Type,
			"layer": entity.Layer, "file_path": entity.FilePath,
			"citations": []any{map[string]any{"source_toolkit": entity.Source, "file_path": entity.FilePath}},
		})
	}
	edges := make([]any, 0, len(FixtureRelations))
	for _, relation := range FixtureRelations {
		edges = append(edges, map[string]any{
			"source": relation.From, "target": relation.To, "relation_type": relation.Type,
		})
	}
	return map[string]any{
		"nodes": nodes,
		"edges": edges,
		"_metadata": map[string]any{
			"fixture":         true,
			"schema_version":  1,
			"source_toolkits": fixtureSourceNames(),
			"ingested_source": sourceLabel,
			"node_count":      len(FixtureEntities),
			"edge_count":      len(FixtureRelations),
		},
	}
}

// fixtureSourceNames are the citation sources, in a stable order.
func fixtureSourceNames() []string {
	seen := map[string]bool{}
	var names []string
	for _, entity := range FixtureEntities {
		if !seen[entity.Source] {
			seen[entity.Source] = true
			names = append(names, entity.Source)
		}
	}
	sort.Strings(names)
	return names
}

// FixtureTools is the canned tool table: one entry per name EngineTools()
// serves, so a fixture host advertises and serves exactly what an engine host
// does. A test compares the two sets — a tool the engine serves and this does
// not would make a browser journey pass against the engine and fail here, or
// worse, the other way round.
func FixtureTools(step time.Duration) map[string]Tool {
	handlers := fixtureHandlers()
	tools := map[string]Tool{}
	for _, name := range EngineTools() {
		handler, ok := handlers[name]
		if !ok {
			// Deliberately not a panic and not a silent omission: the tool is
			// admitted, so it must answer, and a fixture that has nothing to
			// say about it says exactly that.
			handler = fixtureUnwritten(name)
		}
		tools[name] = pacedFixture(name, step, handler)
	}
	return tools
}

// fixtureFunc answers one call from the merged parameters.
type fixtureFunc func(family, tool string, params Params) map[string]any

// pacedFixture wraps a handler with the progress the SPI streams and the stop
// checkpoint that makes a cancel land mid-run rather than after the answer.
func pacedFixture(name string, step time.Duration, handler fixtureFunc) Tool {
	return func(ctx context.Context, arguments map[string]any, tc *spi.Context) (map[string]any, error) {
		steps, ok := FixtureSteps[name]
		if !ok {
			steps = fixtureReadSteps
		}
		for _, progress := range steps {
			if err := tc.Checkpoint(); err != nil {
				return nil, err
			}
			if err := tc.Thinking(ctx, progress); err != nil {
				return nil, err
			}
			if step > 0 {
				select {
				case <-time.After(step):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}
		return handler(str(arguments["family"]), str(arguments["tool"]), object(arguments["params"])), nil
	}
}

func fixtureUnwritten(name string) fixtureFunc {
	return func(string, string, Params) map[string]any {
		return map[string]any{
			"success": false,
			"error": "the fixture runner has no canned answer for '" + name +
				"'; run this deployment against the engine",
			"error_category": "resource_not_found",
		}
	}
}

// ---------------------------------------------------------------------------
// the answers
// ---------------------------------------------------------------------------

// fixtureHandlers maps every served tool onto its canned answer. Written out
// name by name rather than defaulted, so a tool that gains a handler in the
// engine and none here is visible in this table instead of answering something
// generic that reads like a real result.
func fixtureHandlers() map[string]fixtureFunc {
	return map[string]fixtureFunc{
		// ── ingestion ──────────────────────────────────────────────────────
		"run_ingestion":          fixtureRunIngestion,
		"remove_source_entities": fixtureRemoveSourceEntities,

		// ── graph management ───────────────────────────────────────────────
		"list_ingested_sources": fixtureListIngestedSources,
		"list_graphs":           fixtureListGraphs,
		"load_graph":            fixtureLoadGraph,
		"get_graph_info":        fixtureGraphInfo,

		// ── retrieval ──────────────────────────────────────────────────────
		"search_graph":               fixtureSearch,
		"search_knowledge_graph":     fixtureSearch,
		"get_entity":                 fixtureEntity,
		"get_entity_details":         fixtureEntity,
		"get_entity_content":         fixtureEntityContent,
		"get_entities_by_ids":        fixtureEntitiesByIDs,
		"get_related_entities":       fixtureRelated,
		"get_entity_neighbors":       fixtureRelated,
		"impact_analysis":            fixtureImpact,
		"get_cross_source_relations": fixtureCrossSource,
		"get_stats":                  fixtureStats,
		"list_entities_by_type":      fixtureByType,
		"list_entities_by_layer":     fixtureByLayer,
		"list_entities_by_source":    fixtureBySource,
		"list_entity_types":          fixtureEntityTypes,
		"query_graph":                fixtureQueryGraph,
		"investigate":                fixtureInvestigate,

		// ── presets ────────────────────────────────────────────────────────
		"list_presets":    fixtureListPresets,
		"get_preset_info": fixturePresetInfo,

		// ── cache ──────────────────────────────────────────────────────────
		"get_cache_stats": fixtureCacheStats,
		"cleanup_cache":   fixtureCleanupCache,

		// ── status ─────────────────────────────────────────────────────────
		"get_ingestion_status": fixtureIngestionStatus,
		"get_sources_status":   fixtureSourcesStatus,

		// ── maintenance ────────────────────────────────────────────────────
		"normalize_types":       fixtureNormalizeTypes,
		"smart_normalize_types": fixtureNormalizeTypes,
		"rebuild_indices":       fixtureRebuildIndices,
	}
}

func fixtureRunIngestion(_, _ string, params Params) map[string]any {
	label := SourceLabelFor(params)
	graph, _ := json.MarshalIndent(FixtureGraph(label), "", "  ")
	status, _ := json.MarshalIndent(map[string]any{
		"sources": []any{map[string]any{
			"source":         label,
			"status":         "completed",
			"entity_count":   len(FixtureEntities),
			"relation_count": len(FixtureRelations),
		}},
	}, "", "  ")
	checkpoint, _ := json.MarshalIndent(map[string]any{
		"source": label, "stage": "completed", "files_processed": 12,
	}, "", "  ")
	return map[string]any{
		"success": true,
		"result": fmt.Sprintf(
			"Ingestion completed for %s: %d entities, %d relations from %d files.",
			label, len(FixtureEntities), len(FixtureRelations), 12),
		"artifacts": []any{
			map[string]any{"name": "graph.json", "type": "application/json", "data": string(graph)},
			map[string]any{"name": "sources_status.json", "type": "application/json", "data": string(status)},
			map[string]any{
				"name": ".ingestion-checkpoint-" + label + ".json",
				"type": "application/json", "data": string(checkpoint),
			},
		},
	}
}

func fixtureRemoveSourceEntities(_, _ string, params Params) map[string]any {
	label := strings.TrimSpace(str(firstTruthy(params["source_toolkit"], params["source"], "")))
	if label == "" {
		label = SourceLabelFor(params)
	}
	removed := 0
	for _, entity := range FixtureEntities {
		if entity.Source == label {
			removed++
		}
	}
	return fixtureAnswer(params, map[string]any{
		"source": label, "removed_entities": removed,
	}, fmt.Sprintf("Removed %d entities contributed by %s.", removed, label))
}

func fixtureListIngestedSources(_, _ string, params Params) map[string]any {
	var rows []any
	var text strings.Builder
	names := fixtureSourceNames()
	fmt.Fprintf(&text, "# Ingested Sources (%d)\n\n", len(names))
	for _, name := range names {
		entities, relations := 0, 0
		for _, entity := range FixtureEntities {
			if entity.Source == name {
				entities++
			}
		}
		for _, relation := range FixtureRelations {
			if fixtureSourceOf(relation.From) == name {
				relations++
			}
		}
		rows = append(rows, map[string]any{
			"source_toolkit": name, "entity_count": entities, "relation_count": relations,
		})
		fmt.Fprintf(&text, "- **%s**: %d entities, %d relations\n", name, entities, relations)
	}
	return fixtureAnswer(params,
		map[string]any{"sources": rows, "total_sources": len(names)}, text.String())
}

func fixtureListGraphs(_, _ string, params Params) map[string]any {
	name := fixtureGraphName(params)
	return fixtureAnswer(params, map[string]any{
		"graphs": []any{map[string]any{"name": name, "size": 4096}},
		"bucket": ResolveBucket(params),
	}, fmt.Sprintf("# Available Graphs in '%s'\n\n- **%s** (4.0 KB)\n", ResolveBucket(params), name))
}

func fixtureLoadGraph(_, _ string, params Params) map[string]any {
	name := fixtureGraphName(params)
	return fixtureAnswer(params, map[string]any{
		"graph_name": name, "node_count": len(FixtureEntities), "edge_count": len(FixtureRelations),
	}, fmt.Sprintf("Loaded graph: %s\nNodes: %d, Edges: %d",
		name, len(FixtureEntities), len(FixtureRelations)))
}

func fixtureGraphInfo(_, _ string, params Params) map[string]any {
	name := fixtureGraphName(params)
	return fixtureAnswer(params, map[string]any{
		"path":            ResolveBucket(params) + "/" + name + "/graph.json",
		"node_count":      len(FixtureEntities),
		"edge_count":      len(FixtureRelations),
		"source_toolkits": fixtureSourceNames(),
	}, fmt.Sprintf("# Graph %s\n\nNodes: %d\nEdges: %d\nSources: %s\n",
		name, len(FixtureEntities), len(FixtureRelations),
		strings.Join(fixtureSourceNames(), ", ")))
}

func fixtureSearch(_, _ string, params Params) map[string]any {
	query := strings.ToLower(strings.TrimSpace(
		str(firstTruthy(params["query"], params["search_query"], params["question"], ""))))
	matches := fixtureMatch(query)
	rows := make([]any, 0, len(matches))
	var text strings.Builder
	fmt.Fprintf(&text, "# Results for %q (%d)\n\n", query, len(matches))
	for _, entity := range matches {
		rows = append(rows, fixtureEntityRow(entity))
		fmt.Fprintf(&text, "- **%s** (%s, %s) — %s\n",
			entity.Name, entity.Type, entity.Layer, entity.FilePath)
	}
	return fixtureAnswer(params,
		map[string]any{"query": query, "results": rows, "total": len(matches)}, text.String())
}

func fixtureEntity(_, _ string, params Params) map[string]any {
	entity, found := fixtureLookup(str(firstTruthy(
		params["entity_id"], params["entity"], params["id"], "")))
	if !found {
		return fixtureNotFound(params, "entity")
	}
	return fixtureAnswer(params, fixtureEntityRow(entity),
		fmt.Sprintf("# %s\n\n- id: %s\n- type: %s\n- layer: %s\n- source: %s\n- file: %s\n",
			entity.Name, entity.ID, entity.Type, entity.Layer, entity.Source, entity.FilePath))
}

func fixtureEntityContent(_, _ string, params Params) map[string]any {
	entity, found := fixtureLookup(str(firstTruthy(
		params["entity_id"], params["entity"], params["id"], "")))
	if !found {
		return fixtureNotFound(params, "entity")
	}
	return fixtureAnswer(params,
		map[string]any{"id": entity.ID, "file_path": entity.FilePath, "content": entity.Content},
		fmt.Sprintf("```\n%s```\n", entity.Content))
}

func fixtureEntitiesByIDs(_, _ string, params Params) map[string]any {
	var rows []any
	var missing []any
	for _, raw := range fixtureList(params["entity_ids"], params["ids"]) {
		id := strings.TrimSpace(fmt.Sprint(raw))
		if entity, found := fixtureLookup(id); found {
			rows = append(rows, fixtureEntityRow(entity))
			continue
		}
		missing = append(missing, id)
	}
	return fixtureAnswer(params,
		map[string]any{"entities": rows, "missing": missing},
		fmt.Sprintf("Found %d of %d entities.", len(rows), len(rows)+len(missing)))
}

func fixtureRelated(_, _ string, params Params) map[string]any {
	id := str(firstTruthy(params["entity_id"], params["entity"], params["id"], ""))
	if _, found := fixtureLookup(id); !found {
		return fixtureNotFound(params, "entity")
	}
	var rows []any
	var text strings.Builder
	fmt.Fprintf(&text, "# Neighbours of %s\n\n", id)
	for _, relation := range FixtureRelations {
		switch id {
		case relation.From:
			rows = append(rows, map[string]any{
				"entity_id": relation.To, "relation_type": relation.Type, "direction": "outgoing"})
			fmt.Fprintf(&text, "- %s → %s (%s)\n", id, relation.To, relation.Type)
		case relation.To:
			rows = append(rows, map[string]any{
				"entity_id": relation.From, "relation_type": relation.Type, "direction": "incoming"})
			fmt.Fprintf(&text, "- %s ← %s (%s)\n", id, relation.From, relation.Type)
		}
	}
	return fixtureAnswer(params,
		map[string]any{"entity_id": id, "related": rows, "total": len(rows)}, text.String())
}

func fixtureImpact(_, _ string, params Params) map[string]any {
	id := str(firstTruthy(params["entity_id"], params["entity"], params["id"], ""))
	if _, found := fixtureLookup(id); !found {
		return fixtureNotFound(params, "entity")
	}
	// Everything that reaches the entity, transitively: what a change to it
	// would be felt by. One pass is enough for a graph this shape, but the
	// closure is computed rather than listed so a change to FixtureRelations
	// cannot leave a stale answer behind.
	affected := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, relation := range FixtureRelations {
			if relation.To != id && !affected[relation.To] {
				continue
			}
			if !affected[relation.From] {
				affected[relation.From] = true
				changed = true
			}
		}
	}
	ids := make([]string, 0, len(affected))
	for candidate := range affected {
		ids = append(ids, candidate)
	}
	sort.Strings(ids)
	rows := make([]any, 0, len(ids))
	for _, candidate := range ids {
		rows = append(rows, candidate)
	}
	return fixtureAnswer(params,
		map[string]any{"entity_id": id, "impacted": rows, "total": len(rows)},
		fmt.Sprintf("# Impact of %s\n\n%d entities depend on it: %s\n",
			id, len(ids), strings.Join(ids, ", ")))
}

func fixtureCrossSource(_, _ string, params Params) map[string]any {
	var rows []any
	var text strings.Builder
	text.WriteString("# Cross-source relations\n\n")
	for _, relation := range FixtureRelations {
		from, to := fixtureSourceOf(relation.From), fixtureSourceOf(relation.To)
		if from == to || from == "" || to == "" {
			continue
		}
		rows = append(rows, map[string]any{
			"source": relation.From, "target": relation.To,
			"relation_type": relation.Type, "from_source": from, "to_source": to,
		})
		fmt.Fprintf(&text, "- %s (%s) → %s (%s) [%s]\n",
			relation.From, from, relation.To, to, relation.Type)
	}
	return fixtureAnswer(params,
		map[string]any{"relations": rows, "total": len(rows)}, text.String())
}

func fixtureStats(_, _ string, params Params) map[string]any {
	byType := fixtureCount(func(e FixtureEntity) string { return e.Type })
	byLayer := fixtureCount(func(e FixtureEntity) string { return e.Layer })
	return fixtureAnswer(params, map[string]any{
		"node_count":        len(FixtureEntities),
		"edge_count":        len(FixtureRelations),
		"entities_by_type":  byType,
		"entities_by_layer": byLayer,
		"source_toolkits":   fixtureSourceNames(),
	}, fmt.Sprintf("# Graph statistics\n\nNodes: %d\nEdges: %d\nTypes: %d\nLayers: %d\n",
		len(FixtureEntities), len(FixtureRelations), len(byType), len(byLayer)))
}

func fixtureByType(_, _ string, params Params) map[string]any {
	return fixtureFiltered(params, "entity_type",
		func(e FixtureEntity) string { return e.Type })
}

func fixtureByLayer(_, _ string, params Params) map[string]any {
	return fixtureFiltered(params, "layer",
		func(e FixtureEntity) string { return e.Layer })
}

func fixtureBySource(_, _ string, params Params) map[string]any {
	return fixtureFiltered(params, "source_toolkit",
		func(e FixtureEntity) string { return e.Source })
}

func fixtureEntityTypes(_, _ string, params Params) map[string]any {
	counts := fixtureCount(func(e FixtureEntity) string { return e.Type })
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	var text strings.Builder
	text.WriteString("# Entity types\n\n")
	for _, name := range names {
		fmt.Fprintf(&text, "- **%s**: %d\n", name, counts[name])
	}
	return fixtureAnswer(params,
		map[string]any{"types": names, "counts": counts}, text.String())
}

func fixtureQueryGraph(_, _ string, params Params) map[string]any {
	query := strings.TrimSpace(str(firstTruthy(params["query"], params["question"], "")))
	matches := fixtureMatch(strings.ToLower(query))
	rows := make([]any, 0, len(matches))
	for _, entity := range matches {
		rows = append(rows, fixtureEntityRow(entity))
	}
	return fixtureAnswer(params,
		map[string]any{"query": query, "matches": rows, "total": len(matches)},
		fmt.Sprintf("# Query: %s\n\n%d matching entities.\n", query, len(matches)))
}

func fixtureInvestigate(_, _ string, params Params) map[string]any {
	question := strings.TrimSpace(str(firstTruthy(params["question"], params["query"], "")))
	matches := fixtureMatch(strings.ToLower(question))
	cited := make([]any, 0, len(matches))
	var lines strings.Builder
	for _, entity := range matches {
		cited = append(cited, entity.ID)
		fmt.Fprintf(&lines, "- %s (%s)\n", entity.Name, entity.FilePath)
	}
	answer := fmt.Sprintf(
		"[fixture] Answer to %q, from the canned graph:\n\n%s",
		question, lines.String())
	return fixtureAnswer(params,
		map[string]any{"question": question, "answer": answer, "entities": cited}, answer)
}

func fixtureListPresets(_, _ string, params Params) map[string]any {
	names := make([]string, 0, len(FixturePresets))
	for name := range FixturePresets {
		names = append(names, name)
	}
	sort.Strings(names)
	var text strings.Builder
	text.WriteString("# Presets\n\n")
	for _, name := range names {
		fmt.Fprintf(&text, "- **%s**: %s\n", name, FixturePresets[name])
	}
	return fixtureAnswer(params, map[string]any{"presets": names}, text.String())
}

func fixturePresetInfo(_, _ string, params Params) map[string]any {
	name := strings.TrimSpace(str(firstTruthy(params["preset"], params["preset_name"], "")))
	description, found := FixturePresets[name]
	if !found {
		return fixtureNotFound(params, "preset")
	}
	return fixtureAnswer(params,
		map[string]any{"preset": name, "description": description},
		fmt.Sprintf("# %s\n\n%s\n", name, description))
}

func fixtureCacheStats(_, _ string, params Params) map[string]any {
	return fixtureAnswer(params, map[string]any{
		"cached_graphs": 1, "cache_size_bytes": 4096, "hits": 0, "misses": 1,
	}, "# Cache\n\nGraphs: 1\nSize: 4.0 KB\n")
}

func fixtureCleanupCache(_, _ string, params Params) map[string]any {
	return fixtureAnswer(params,
		map[string]any{"removed_graphs": 1, "freed_bytes": 4096},
		"Removed 1 cached graph (4.0 KB).")
}

func fixtureIngestionStatus(_, _ string, params Params) map[string]any {
	return fixtureAnswer(params, map[string]any{
		"running": false, "last_status": "completed",
		"entity_count": len(FixtureEntities), "relation_count": len(FixtureRelations),
	}, "No ingestion is running. The last one completed.")
}

func fixtureSourcesStatus(_, _ string, params Params) map[string]any {
	rows := make([]any, 0, len(fixtureSourceNames()))
	var text strings.Builder
	text.WriteString("# Sources\n\n")
	for _, name := range fixtureSourceNames() {
		entities := 0
		for _, entity := range FixtureEntities {
			if entity.Source == name {
				entities++
			}
		}
		rows = append(rows, map[string]any{
			"source": name, "status": "completed", "entity_count": entities})
		fmt.Fprintf(&text, "- **%s**: completed, %d entities\n", name, entities)
	}
	return fixtureAnswer(params, map[string]any{"sources": rows}, text.String())
}

func fixtureNormalizeTypes(_, _ string, params Params) map[string]any {
	counts := fixtureCount(func(e FixtureEntity) string { return e.Type })
	return fixtureAnswer(params,
		map[string]any{"normalized": 0, "types": counts},
		fmt.Sprintf("Types are already normalised: %d distinct types.", len(counts)))
}

func fixtureRebuildIndices(_, _ string, params Params) map[string]any {
	return fixtureAnswer(params,
		map[string]any{"indexed_entities": len(FixtureEntities)},
		fmt.Sprintf("Rebuilt the indices over %d entities.", len(FixtureEntities)))
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// fixtureAnswer is the legacy `output_format` switch: every read tool returns
// markdown by default and a JSON document when the caller asks for one. The
// engine's handlers all carry it, so a fixture that answered only one shape
// would let a caller depend on a format the engine does not give it.
func fixtureAnswer(params Params, document map[string]any, text string) map[string]any {
	if strings.EqualFold(strings.TrimSpace(str(params["output_format"])), "json") {
		encoded, _ := json.Marshal(document)
		return map[string]any{"success": true, "result": string(encoded)}
	}
	return map[string]any{"success": true, "result": text}
}

// fixtureNotFound is the refusal the engine gives for an id the graph does not
// hold. `resource_not_found`, which EngineError maps to the SPI's not-found
// category — a fixture that answered "success with an empty body" would let a
// caller ship a screen that renders a missing entity as an empty one.
func fixtureNotFound(params Params, what string) map[string]any {
	id := strings.TrimSpace(str(firstTruthy(
		params["entity_id"], params["entity"], params["id"],
		params["preset"], params["preset_name"], "")))
	return map[string]any{
		"success":        false,
		"error":          fmt.Sprintf("No %s %q in this graph.", what, id),
		"error_category": "resource_not_found",
	}
}

func fixtureEntityRow(entity FixtureEntity) map[string]any {
	return map[string]any{
		"id": entity.ID, "name": entity.Name, "type": entity.Type,
		"layer": entity.Layer, "source_toolkit": entity.Source,
		"file_path": entity.FilePath,
	}
}

func fixtureLookup(id string) (FixtureEntity, bool) {
	id = strings.TrimSpace(id)
	for _, entity := range FixtureEntities {
		if entity.ID == id || strings.EqualFold(entity.Name, id) {
			return entity, true
		}
	}
	return FixtureEntity{}, false
}

// fixtureMatch is substring matching over the id, the name and the file path.
// It is what the model is being asked for and is decidable without one — the
// same rule DeepWiki's fixture resolver applies. An empty query matches
// everything, because that is what a browser's first render sends.
func fixtureMatch(query string) []FixtureEntity {
	var matches []FixtureEntity
	for _, entity := range FixtureEntities {
		haystack := strings.ToLower(entity.ID + " " + entity.Name + " " + entity.FilePath)
		if query == "" || strings.Contains(haystack, query) {
			matches = append(matches, entity)
		}
	}
	return matches
}

func fixtureFiltered(params Params, key string, of func(FixtureEntity) string) map[string]any {
	wanted := strings.TrimSpace(str(firstTruthy(params[key], params["type"], params["value"], "")))
	rows := make([]any, 0, len(FixtureEntities))
	var text strings.Builder
	fmt.Fprintf(&text, "# Entities where %s = %q\n\n", key, wanted)
	for _, entity := range FixtureEntities {
		if wanted != "" && !strings.EqualFold(of(entity), wanted) {
			continue
		}
		rows = append(rows, fixtureEntityRow(entity))
		fmt.Fprintf(&text, "- **%s** (%s) — %s\n", entity.Name, of(entity), entity.FilePath)
	}
	return fixtureAnswer(params,
		map[string]any{key: wanted, "entities": rows, "total": len(rows)}, text.String())
}

func fixtureCount(of func(FixtureEntity) string) map[string]any {
	counts := map[string]any{}
	for _, entity := range FixtureEntities {
		key := of(entity)
		current, _ := counts[key].(int)
		counts[key] = current + 1
	}
	return counts
}

func fixtureSourceOf(id string) string {
	if entity, found := fixtureLookup(id); found {
		return entity.Source
	}
	return ""
}

func fixtureList(values ...any) []any {
	for _, value := range values {
		if list, ok := value.([]any); ok && len(list) > 0 {
			return list
		}
	}
	return nil
}

func fixtureGraphName(params Params) string {
	if name := strings.TrimSpace(str(firstTruthy(
		params["graph_name"], params["toolkit_configuration_graph_name"], ""))); name != "" {
		return name
	}
	return "inventory"
}

// NewFixtureRunner is the fixture table under the shared runner, pacing its
// progress by step, with the callback CA from the host's settings.
//
// No egress policy, for the reason NewEngineRunner states: Inventory reads its
// source through the SDK's toolkit and there is no request-supplied clone URL
// for a host-side allowlist to check.
func NewFixtureRunner(settings spi.Settings, step time.Duration) *Runner {
	return &Runner{
		RunnerName: "fixture",
		Tools:      FixtureTools(step),
		Artifacts:  ArtifactClientFrom(settings.TLSCAFile),
	}
}
