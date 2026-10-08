//! The `inventory` family's read tools: the `_tool_*` handlers of
//! `tool_operations.py` and the `InventoryRetrievalApiWrapper` text methods
//! (`engine/inventory/retrieval.py`) they delegate to, over a
//! [`GraphView`]. Every answer is the string the Python handler returned —
//! markdown, or `json.dumps(...)` (no indent, `ensure_ascii`) when
//! `output_format` is exactly `"json"` — wrapped by [`answer`].
//!
//! The goldens (`tests/retrieval_tools.rs`) are what the REAL Python
//! handlers answered over the same `graph.json`
//! (`tests/fixtures/retrieval/generate.py`).
//!
//! # Deviations from Python, each asserted by a test
//!
//! * **D1 — rows carry `id`.** See [`super::query`]: Python's `dict(data)`
//!   rows of a reloaded graph had no `id` (`list_entities_by_layer`,
//!   `list_entities_by_source`, `query_graph`), so neither the UI nor the
//!   edge lookup could address them. Every row has `id` appended.
//! * **D2 — the JSON listings answer.** `_get_edges_for_entities` returns
//!   `(edges, connected_ids)` and the JSON forms of `impact_analysis` and
//!   `list_entities_by_{type,layer,source}` dumped that tuple, whose `set`
//!   `json.dumps` cannot serialise: they always raised `TypeError`. `edges`
//!   is the edge list here.
//! * **D3 — the location is read from `citations`.** `get_entity_content`,
//!   the `impact_analysis` text and the `list_entities_by_type` text read the
//!   legacy single `citation` key, which no v1 graph has, so they answered
//!   "has no file citation" / `unknown`. The legacy key is still read first;
//!   without it the first of `citations` is. The engine has no source access
//!   (v1 configured no `base_directory` and no source toolkits), so
//!   `get_entity_content` answers Python's "Could not retrieve content"
//!   message with the cited location — what v1 answered for an entity it
//!   could locate.
//! * **D4 — an id is accepted where a name is expected.** The web client
//!   sends entity ids; Python resolved names only (`find_all_entities_by_name`)
//!   and answered "not found". Where no entity has the given name and a node
//!   has it as id, that node is the entity, and the text answers describe it
//!   directly (Python's text methods would re-resolve the name).
//! * **D5 — no raw vectors.** JSON entity rows omit `embedding` (Python sent
//!   every matched entity's vector, and `search_graph` defaults to sending
//!   every edge of the graph too, which the graph UI draws and is kept).
//! * **D6 — `list_graphs` answers.** Its handler referenced an undefined
//!   `bucket` and raised `NameError` on every call. It answers the one graph
//!   this toolkit has (`bucket` / `graph_name` parameters, defaulting to
//!   `inventory`), sized as its `graph.json`, or none for an empty graph.
//! * **D7 — the graph `path`.** `get_graph_info` reported the pod's cache
//!   path; there is none, so it reports `{bucket}/{graph_name}/graph.json`
//!   as the fixture runner does.
//! * **Embedding-space check.** v1's `_get_or_create_wrapper` refused EVERY
//!   tool when the graph's embedding model differed from the toolkit's; the
//!   lexical tools here do not (only semantic search can be wrong then).
//! * **Parameters.** A `null` string or number parameter reads as absent
//!   (Python raised on most); a numeric parameter given as a numeric string
//!   is read as the number (Python raised `TypeError`).
//! * **No graph.** Python answered `get_stats` "No graph data yet" when the
//!   graph file was missing; here that is the empty default view (revision 0)
//!   the runner substitutes for a graph the store does not have.

// The answers are built line by line, as the Python handlers built them.
#![allow(clippy::format_push_string)]

use super::query::{
    self, AdvancedFilters, Entity, SearchFilters, available_layers, citations_of, cited_by,
    description_of, edges_touching, entity, entity_id_first, find_bridging_nodes,
    find_entity_by_reference, get_display, get_display_or, head_len, ids_by_layer, ids_by_type,
    ids_named, impact_analysis as impact, new_neighbours, py_display, relations, search, stats,
    strip_embedding, text, type_layer, wrapper_lookup,
};
use super::view::GraphView;
use super::{Call, Handled, answer};
use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::pyjson::dumps;
use elitea_engine_core::pystr::{self, prefix_chars};
use elitea_engine_core::pyvalue::py_truthy;
use indexmap::{IndexMap, IndexSet};
use serde_json::{Map, Value, json};

/// See [`super::dispatch`].
#[must_use]
pub fn handle(call: &Call<'_>) -> Handled {
    if call.family != "inventory" {
        return None;
    }
    let (view, params) = (call.view, call.params);
    let result = match call.tool {
        "search_graph" => search_graph(view, params),
        "get_entity" => Ok(get_entity(view, params)),
        "get_entity_content" => Ok(get_entity_content(view, params)),
        "impact_analysis" => impact_analysis(view, params),
        "get_related_entities" => Ok(get_related_entities(view, params)),
        "get_cross_source_relations" => Ok(get_cross_source_relations(view, params)),
        "get_stats" => Ok(get_stats(view, params)),
        "get_graph_info" => Ok(get_graph_info(view, params)),
        "list_ingested_sources" => Ok(list_ingested_sources(view, params)),
        "list_graphs" => Ok(list_graphs(view, params)),
        "load_graph" => Ok(load_graph(view, params)),
        "list_entities_by_type" => list_entities_by_type(view, params),
        "list_entities_by_layer" => list_entities_by_layer(view, params),
        "list_entities_by_source" => list_entities_by_source(view, params),
        "get_entities_by_ids" => get_entities_by_ids(view, params),
        "get_entity_neighbors" => get_entity_neighbors(view, params),
        _ => return None,
    };
    Some(result.map(answer))
}

// ------------------------------------------------------------ parameters

/// `params.get(key, default)` for a string parameter (`null` = absent; a
/// non-string is its Python `str()`).
pub(super) fn str_param(params: &Map<String, Value>, key: &str, default: &str) -> String {
    match params.get(key) {
        None | Some(Value::Null) => default.to_owned(),
        Some(value) => py_display(value),
    }
}

/// `params.get(key)` when truthy, as text.
pub(super) fn truthy_str(params: &Map<String, Value>, key: &str) -> Option<String> {
    params.get(key).filter(|v| py_truthy(v)).map(py_display)
}

/// `params.get(key, default)` for a flag: absent is `default`, anything else
/// its Python truthiness.
pub(super) fn flag(params: &Map<String, Value>, key: &str, default: bool) -> bool {
    params.get(key).map_or(default, py_truthy)
}

/// Python `int(value)` (a bool is 0/1, a float truncates, a string parses).
pub(super) fn py_int(value: &Value) -> Option<i64> {
    match value {
        Value::Bool(flag) => Some(i64::from(*flag)),
        Value::Number(number) => number.as_i64().or_else(|| {
            number
                .as_f64()
                .filter(|f| f.is_finite())
                // Truncation toward zero is Python's int(float).
                .map(|f| {
                    #[allow(clippy::cast_possible_truncation)]
                    let truncated = f.trunc() as i64;
                    truncated
                })
        }),
        Value::String(text) => pystr::strip(text).replace('_', "").parse().ok(),
        _ => None,
    }
}

/// `params.get(key, default)` for a number.
///
/// # Errors
///
/// A `ValueError` for a value that is not a number.
pub(super) fn int_param(
    params: &Map<String, Value>,
    key: &str,
    default: i64,
) -> Result<i64, EngineError> {
    match params.get(key) {
        None | Some(Value::Null) => Ok(default),
        Some(value) => py_int(value).ok_or_else(|| {
            EngineError::new(
                ErrorType::Value,
                format!("invalid literal for int(): {}", py_display(value)),
            )
        }),
    }
}

/// `params.get("output_format", default) == "json"`.
pub(super) fn wants_json(params: &Map<String, Value>, default_json: bool) -> bool {
    match params.get("output_format") {
        None => default_json,
        Some(value) => value.as_str() == Some("json"),
    }
}

/// The raw parameter, `null` when absent (`params.get(key)` echoed in JSON).
pub(super) fn raw(params: &Map<String, Value>, key: &str) -> Value {
    params.get(key).cloned().unwrap_or(Value::Null)
}

/// An entity row for a JSON answer (D5: no `embedding`).
pub(super) fn public_row(view: &GraphView, id: &str) -> Value {
    let mut row = entity(view, id).unwrap_or_default();
    strip_embedding(&mut row);
    Value::Object(row)
}

fn public(mut row: Entity) -> Value {
    strip_embedding(&mut row);
    Value::Object(row)
}

/// `json.dumps({"error": message})` or the message.
pub(super) fn error_answer_for(json: bool, message: String) -> String {
    if json {
        dumps(&json!({"error": message}))
    } else {
        message
    }
}

/// The citation a location is read from: the legacy `citation` when truthy,
/// else the first of `citations` (D3).
fn location_citation(node: &Map<String, Value>) -> Option<&Map<String, Value>> {
    if let Some(legacy) = node.get("citation").filter(|c| py_truthy(c)) {
        return legacy.as_object();
    }
    match node.get("citations") {
        Some(Value::Array(items)) => items.first().filter(|c| py_truthy(c))?.as_object(),
        _ => None,
    }
}

/// `citation.get('file_path', 'unknown') if citation else 'unknown'`.
fn location_of(node: &Map<String, Value>) -> String {
    location_citation(node).map_or_else(
        || "unknown".to_owned(),
        |citation| get_display_or(citation, "file_path", "unknown"),
    )
}

// ---------------------------------------------------------- search_graph

/// `_tool_search_graph` (also `search_knowledge_graph`).
///
/// # Errors
///
/// A non-numeric `top_k` or `max_depth`.
pub(super) fn search_graph(
    view: &GraphView,
    params: &Map<String, Value>,
) -> Result<String, EngineError> {
    let query_text = str_param(params, "query", "");
    let entity_type = truthy_str(params, "entity_type");
    let layer = truthy_str(params, "layer");
    let source_toolkit = truthy_str(params, "source_toolkit");
    let file_pattern = truthy_str(params, "file_pattern");
    let top_k = int_param(params, "top_k", 10)?;
    let max_depth = int_param(params, "max_depth", 0)?;
    let show_all_edges = flag(params, "show_all_edges", true);
    let filters = SearchFilters {
        entity_type: entity_type.as_deref(),
        layer: layer.as_deref(),
        file_pattern: file_pattern.as_deref(),
    };
    let from_source = |hits: Vec<query::Hit>| -> Vec<query::Hit> {
        match &source_toolkit {
            Some(source) => hits
                .into_iter()
                .filter(|hit| {
                    view.node(&hit.id)
                        .is_some_and(|node| cited_by(node, source))
                })
                .collect(),
            None => hits,
        }
    };

    if wants_json(params, false) {
        let hits = from_source(search(view, &query_text, top_k, filters));
        let mut results: Vec<Value> = hits
            .iter()
            .map(|hit| {
                json!({"entity": public_row(view, &hit.id), "score": hit.score, "match_field": hit.match_field})
            })
            .collect();
        let initial: IndexSet<String> = hits.iter().map(|hit| hit.id.clone()).collect();
        let entity_ids = if max_depth > 0 && !initial.is_empty() {
            let (all, expanded) = expand_by_depth(view, &initial, max_depth);
            results.extend(expanded);
            all
        } else {
            initial
        };
        let edges: Vec<Value> = if show_all_edges {
            view.graph
                .edges()
                .map(|(source, target, attributes)| query::edge_row(source, target, attributes))
                .collect()
        } else {
            let (edges, connected) = edges_touching(view, &entity_ids);
            for id in &connected {
                if view.node(id).is_some() {
                    results.push(json!({"entity": public_row(view, id), "score": 0.0, "match_field": "connected"}));
                }
            }
            edges
        };
        let document = json!({
            "results": results,
            "edges": edges,
            "query": query_text,
            "filters": {
                "entity_type": raw(params, "entity_type"),
                "layer": raw(params, "layer"),
                "source_toolkit": raw(params, "source_toolkit"),
                "file_pattern": raw(params, "file_pattern"),
                "max_depth": params.get("max_depth").cloned().unwrap_or_else(|| json!(0)),
            },
            "total_results": results.len(),
            "total_edges": edges.len(),
            "initial_matches": hits.len(),
        });
        return Ok(dumps(&document));
    }

    let result = wrapper_search_graph(view, &query_text, filters, top_k);
    let Some(source) = source_toolkit
        .clone()
        .filter(|_| !result.contains("No entities found"))
    else {
        return Ok(result);
    };
    let filtered = from_source(search(view, &query_text, top_k.saturating_mul(2), filters));
    if filtered.is_empty() {
        return Ok(format!(
            "No entities found matching '{query_text}' from source '{source}'"
        ));
    }
    let shown = &filtered[..head_len(filtered.len(), top_k)];
    let mut output = format!(
        "Found {} entities matching '{query_text}' from '{source}':\n\n",
        shown.len()
    );
    for (i, hit) in shown.iter().enumerate() {
        let node = entity(view, &hit.id).unwrap_or_default();
        output += &format!(
            "{:>2}. **{}** ({})\n",
            i + 1,
            get_display(&node, "name"),
            get_display_or(&node, "type", "unknown")
        );
    }
    Ok(output)
}

/// `_expand_entities_by_depth`: every id within `max_depth` hops, and the
/// rows of the ones added (`{'id': …, **data}`, score 0.5).
fn expand_by_depth(
    view: &GraphView,
    initial: &IndexSet<String>,
    max_depth: i64,
) -> (IndexSet<String>, Vec<Value>) {
    let mut current = initial.clone();
    let mut all = initial.clone();
    let mut rows = Vec::new();
    for _ in 0..max_depth {
        let mut next = IndexSet::new();
        for id in &current {
            if view.node(id).is_some() {
                new_neighbours(view, id, &all, &mut next);
            }
        }
        if next.is_empty() {
            break;
        }
        for id in &next {
            if view.node(id).is_some_and(|node| !node.is_empty())
                && let Some(row) = entity_id_first(view, id)
            {
                rows.push(json!({"entity": public(row), "score": 0.5, "match_field": "expanded"}));
            }
        }
        all.extend(next.iter().cloned());
        current = next;
    }
    (all, rows)
}

/// `InventoryRetrievalApiWrapper.search_graph`.
fn wrapper_search_graph(
    view: &GraphView,
    query_text: &str,
    filters: SearchFilters<'_>,
    top_k: i64,
) -> String {
    let hits = search(view, query_text, top_k, filters);
    if hits.is_empty() {
        let mut named = Vec::new();
        if let Some(kind) = filters.entity_type {
            named.push(format!("type={kind}"));
        }
        if let Some(layer) = filters.layer {
            named.push(format!("layer={layer}"));
        }
        if let Some(pattern) = filters.file_pattern {
            named.push(format!("file={pattern}"));
        }
        let suffix = if named.is_empty() {
            String::new()
        } else {
            format!(" (filters: {})", named.join(", "))
        };
        return format!("No entities found matching '{query_text}'{suffix}");
    }
    let mut output = format!("Found {} entities matching '{query_text}':\n\n", hits.len());
    for (i, hit) in hits.iter().enumerate() {
        let node = entity(view, &hit.id).unwrap_or_default();
        let mut kind = get_display_or(&node, "type", "unknown");
        let mut layer = text(&node, "layer").to_owned();
        if layer.is_empty() {
            type_layer(&kind.to_lowercase()).clone_into(&mut layer);
        }
        if !layer.is_empty() {
            kind = format!("{layer}/{kind}");
        }
        output += &format!(
            "{:>2}. **{}** ({kind})\n",
            i + 1,
            get_display(&node, "name")
        );
        let citations = citations_of(&node);
        if let Some(first) = citations.first() {
            let empty = Map::new();
            let citation = first.as_object().unwrap_or(&empty);
            let mut line = String::new();
            if let Some(start) = citation.get("line_start").filter(|v| py_truthy(v)) {
                line = match citation.get("line_end").filter(|v| py_truthy(v)) {
                    Some(end) => format!(":{}-{}", py_display(start), py_display(end)),
                    None => format!(":{}", py_display(start)),
                };
            }
            output += &format!(
                "    📍 `{}{line}`\n",
                get_display_or(citation, "file_path", "unknown")
            );
        } else if let Some(path) = node.get("file_path").filter(|v| py_truthy(v)) {
            output += &format!("    📍 `{}`\n", py_display(path));
        }
        let description = description_of(&node);
        if !description.is_empty() {
            let mut shown = prefix_chars(description, 120).to_owned();
            if description.chars().count() > 120 {
                shown += "...";
            }
            output += &format!("    {shown}\n");
        }
        output += "\n";
    }
    output
}

// ------------------------------------------------------------ get_entity

/// `_tool_get_entity` (also `get_entity_details`).
pub(super) fn get_entity(view: &GraphView, params: &Map<String, Value>) -> String {
    let json = wants_json(params, false);
    let name = str_param(params, "entity_name", "");
    let include_relations = flag(params, "include_relations", true);
    let resolved = match find_entity_by_reference(view, &name) {
        Ok(resolved) => resolved,
        Err(message) => return error_answer_for(json, message),
    };
    if json {
        let mut result = Map::new();
        result.insert("entity".to_owned(), public_row(view, &resolved.id));
        if include_relations {
            let (mut incoming, mut outgoing) = (Vec::new(), Vec::new());
            for relation in relations(view, &resolved.id, "both") {
                if relation.source == resolved.id {
                    outgoing.push(relation.to_value());
                } else {
                    incoming.push(relation.to_value());
                }
            }
            result.insert("incoming".to_owned(), Value::Array(incoming));
            result.insert("outgoing".to_owned(), Value::Array(outgoing));
        }
        return dumps(&Value::Object(result));
    }
    if resolved.by_id {
        return entity_details(view, &[resolved.id], &name, include_relations);
    }
    wrapper_get_entity(view, &name, include_relations)
}

/// `InventoryRetrievalApiWrapper.get_entity`.
fn wrapper_get_entity(view: &GraphView, name: &str, include_relations: bool) -> String {
    let mut ids: Vec<String> = ids_named(view, name)
        .into_iter()
        .map(str::to_owned)
        .collect();
    if ids.is_empty() {
        ids = search(view, name, 5, SearchFilters::default())
            .into_iter()
            .map(|hit| hit.id)
            .collect();
    }
    if ids.is_empty() {
        return format!("Entity '{name}' not found");
    }
    entity_details(view, &ids, name, include_relations)
}

fn layer_and_type(node: &Map<String, Value>) -> (String, String) {
    let kind = get_display_or(node, "type", "unknown");
    let mut layer = text(node, "layer").to_owned();
    if layer.is_empty() {
        type_layer(&kind.to_lowercase()).clone_into(&mut layer);
    }
    (layer, kind)
}

/// The keys the detail page does not list as properties.
const SKIP: [&str; 10] = [
    "id",
    "name",
    "type",
    "layer",
    "citation",
    "citations",
    "description",
    "file_path",
    "source_toolkit",
    "properties",
];

/// The detail page of the wrapper's `get_entity` for `ids` (disambiguation
/// first when there are several).
fn entity_details(view: &GraphView, ids: &[String], name: &str, include_relations: bool) -> String {
    let entities: Vec<Entity> = ids.iter().filter_map(|id| entity(view, id)).collect();
    let Some(primary) = entities.first() else {
        return format!("Entity '{name}' not found");
    };
    let mut output = String::new();
    if entities.len() > 1 {
        output += &format!("# Found {} entities named '{name}'\n\n", entities.len());
        for (i, candidate) in entities.iter().enumerate() {
            let (layer, kind) = layer_and_type(candidate);
            let type_text = if layer.is_empty() {
                kind
            } else {
                format!("{layer}/{kind}")
            };
            output += &format!(
                "{}. **{}** ({type_text})",
                i + 1,
                get_display(candidate, "name")
            );
            if let Some(path) = candidate.get("file_path").filter(|v| py_truthy(v)) {
                output += &format!(" - `{}`", py_display(path));
            }
            output += &format!("\n   ID: `{}`\n\n", get_display(candidate, "id"));
        }
        output += "\n---\n\n";
        output += "Showing details for the first match:\n\n";
    }
    output += &format!("# {}\n\n", get_display(primary, "name"));
    let (layer, kind) = layer_and_type(primary);
    output += &format!("**Type:** {kind}\n");
    if !layer.is_empty() {
        output += &format!("**Layer:** {layer}\n");
    }
    output += &format!("**ID:** `{}`\n", get_display(primary, "id"));
    let citations = citations_of(primary);
    if !citations.is_empty() {
        output += &format!("\n**Locations ({}):**\n", citations.len());
        for citation in citations.iter().take(5).filter_map(|c| c.as_object()) {
            let mut line = String::new();
            if let Some(start) = citation.get("line_start").filter(|v| py_truthy(v)) {
                line = format!(":{}", py_display(start));
                if let Some(end) = citation.get("line_end").filter(|v| py_truthy(v)) {
                    line += &format!("-{}", py_display(end));
                }
            }
            output += &format!(
                "- `{}{line}` ({})\n",
                get_display_or(citation, "file_path", "unknown"),
                get_display_or(citation, "source_toolkit", "filesystem")
            );
        }
        if citations.len() > 5 {
            output += &format!("- ... and {} more citations\n", citations.len() - 5);
        }
    } else if let Some(path) = primary.get("file_path").filter(|v| py_truthy(v)) {
        output += &format!("\n**Location:** `{}`\n", py_display(path));
    }
    let description = description_of(primary);
    if !description.is_empty() {
        output += &format!("\n**Description:**\n{description}\n");
    }
    output += &properties_section(primary);
    if include_relations {
        output += &relations_section(view, primary);
    }
    output
}

/// The `**Properties:**` section of the detail page.
fn properties_section(primary: &Entity) -> String {
    let mut output = String::new();
    let mut props: Map<String, Value> = primary
        .iter()
        .filter(|(key, _)| !SKIP.contains(&key.as_str()))
        .map(|(key, value)| (key.clone(), value.clone()))
        .collect();
    if let Some(Value::Object(nested)) = primary.get("properties") {
        for (key, value) in nested {
            if !SKIP.contains(&key.as_str()) {
                props.insert(key.clone(), value.clone());
            }
        }
    }
    if !props.is_empty() {
        output += "\n**Properties:**\n";
        for (key, value) in &props {
            match value {
                Value::Array(items) => output += &format!("- {key}: {} items\n", items.len()),
                Value::Object(fields) => output += &format!("- {key}: {} items\n", fields.len()),
                Value::String(long) if long.chars().count() > 100 => {
                    output += &format!("- {key}: {}...\n", prefix_chars(long, 100));
                }
                other => output += &format!("- {key}: {}\n", py_display(other)),
            }
        }
    }
    output
}

/// The `**Relations (n):**` section of the detail page.
fn relations_section(view: &GraphView, primary: &Entity) -> String {
    let mut output = String::new();
    let primary_id = text(primary, "id");
    if !primary_id.is_empty() {
        let found = relations(view, primary_id, "both");
        if !found.is_empty() {
            output += &format!("\n**Relations ({}):**\n", found.len());
            let (mut outgoing, mut incoming) = (Vec::new(), Vec::new());
            let name_of = |id: &str| {
                view.node(id)
                    .map_or_else(|| id.to_owned(), |node| get_display_or(node, "name", id))
            };
            for relation in &found {
                let kind = py_display(&relation.relation_type());
                if relation.source == primary_id {
                    outgoing.push(format!("→ {kind} → **{}**", name_of(relation.target)));
                } else {
                    incoming.push(format!("← {kind} ← **{}**", name_of(relation.source)));
                }
            }
            for line in outgoing.iter().take(5) {
                output += &format!("- {line}\n");
            }
            if outgoing.len() > 5 {
                output += &format!("- ... and {} more outgoing\n", outgoing.len() - 5);
            }
            for line in incoming.iter().take(5) {
                output += &format!("- {line}\n");
            }
            if incoming.len() > 5 {
                output += &format!("- ... and {} more incoming\n", incoming.len() - 5);
            }
        }
    }
    output
}

// ---------------------------------------------------- get_entity_content

/// `_tool_get_entity_content` → `InventoryRetrievalApiWrapper.get_entity_content`
/// (D3, D4).
fn get_entity_content(view: &GraphView, params: &Map<String, Value>) -> String {
    let name = str_param(params, "entity_name", "");
    let id = ids_named(view, &name)
        .first()
        .map(|id| (*id).to_owned())
        .or_else(|| {
            let candidate = pystr::strip(&name);
            view.node(candidate).map(|_| candidate.to_owned())
        })
        .or_else(|| {
            search(view, &name, 1, SearchFilters::default())
                .into_iter()
                .next()
                .map(|hit| hit.id)
        });
    let Some(node) = id.and_then(|id| entity(view, &id)) else {
        return format!("Entity '{name}' not found");
    };
    let Some(citation) =
        location_citation(&node).filter(|c| c.get("file_path").is_some_and(py_truthy))
    else {
        return format!("Entity '{name}' has no file citation");
    };
    let mut location = get_display(citation, "file_path");
    if let Some(start) = citation.get("line_start").filter(|v| py_truthy(v)) {
        location += &format!(":{}", py_display(start));
        if let Some(end) = citation.get("line_end").filter(|v| py_truthy(v)) {
            location += &format!("-{}", py_display(end));
        }
    }
    format!(
        "Could not retrieve content for '{name}'\nLocation: {location}\nSource: {}\n\nThe file may not be accessible locally. Ensure base_directory is set or the source toolkit is available.",
        get_display_or(citation, "source_toolkit", "filesystem")
    )
}

// ------------------------------------------------------- impact_analysis

/// `_tool_impact_analysis` (D2 for JSON, D3/D4 for text).
fn impact_analysis(view: &GraphView, params: &Map<String, Value>) -> Result<String, EngineError> {
    let json = wants_json(params, false);
    let name = str_param(params, "entity_name", "");
    let direction = str_param(params, "direction", "downstream");
    let max_depth = int_param(params, "max_depth", 3)?;
    let resolved = match find_entity_by_reference(view, &name) {
        Ok(resolved) => resolved,
        Err(message) => return Ok(error_answer_for(json, message)),
    };
    if json {
        let impacted = impact(view, &resolved.id, &direction, max_depth);
        let mut ids: IndexSet<String> = IndexSet::from([resolved.id.clone()]);
        ids.extend(impacted.iter().map(|item| item.id.clone()));
        let (edges, _) = edges_touching(view, &ids);
        let items: Vec<Value> = impacted
            .iter()
            .map(|item| json!({"entity": public_row(view, &item.id), "depth": item.depth, "path": item.path}))
            .collect();
        return Ok(dumps(&json!({
            "entity_name": name,
            "direction": direction,
            "impacted": items,
            "edges": edges,
        })));
    }
    let id = if resolved.by_id {
        Some(resolved.id)
    } else {
        wrapper_lookup(view, &name)
    };
    let Some(id) = id else {
        return Ok(format!("Entity '{name}' not found"));
    };
    let impacted = impact(view, &id, &direction, max_depth);
    if impacted.is_empty() {
        return Ok(format!("No {direction} dependencies found for '{name}'"));
    }
    let mut output = format!("# Impact Analysis: {name}\n\n");
    output += &format!("**Direction:** {direction}\n");
    output += &format!("**Total impacted:** {} entities\n\n", impacted.len());
    let mut by_depth: IndexMap<i64, Vec<&query::Impacted>> = IndexMap::new();
    for item in &impacted {
        by_depth.entry(item.depth).or_default().push(item);
    }
    by_depth.sort_keys();
    for (depth, items) in &by_depth {
        output += &format!("## Level {depth} ({} entities)\n\n", items.len());
        for item in items.iter().take(15) {
            let node = entity(view, &item.id).unwrap_or_default();
            output += &format!(
                "- **{}** ({}) - `{}`\n",
                get_display(&node, "name"),
                get_display(&node, "type"),
                location_of(&node)
            );
        }
        if items.len() > 15 {
            output += &format!("- ... and {} more\n", items.len() - 15);
        }
        output += "\n";
    }
    Ok(output)
}

// -------------------------------------------------- get_related_entities

/// `_tool_get_related_entities` (both families).
pub(super) fn get_related_entities(view: &GraphView, params: &Map<String, Value>) -> String {
    let json = wants_json(params, false);
    let name = str_param(params, "entity_name", "");
    let relation_type = truthy_str(params, "relation_type");
    let direction = str_param(params, "direction", "both");
    let resolved = match find_entity_by_reference(view, &name) {
        Ok(resolved) => resolved,
        Err(message) => return error_answer_for(json, message),
    };
    let of_type = |relation: &query::Relation<'_>| {
        relation_type
            .as_deref()
            .is_none_or(|wanted| relation.relation_type().as_str() == Some(wanted))
    };
    if json {
        let found: Vec<Value> = relations(view, &resolved.id, &direction)
            .iter()
            .filter(|relation| of_type(relation))
            .map(query::Relation::to_value)
            .collect();
        return dumps(&json!({"entity_name": name, "relations": found}));
    }
    let id = if resolved.by_id {
        Some(resolved.id)
    } else {
        wrapper_lookup(view, &name)
    };
    let Some(id) = id else {
        return format!("Entity '{name}' not found");
    };
    let found: Vec<query::Relation<'_>> = relations(view, &id, &direction)
        .into_iter()
        .filter(|relation| of_type(relation))
        .collect();
    if found.is_empty() {
        let filter = relation_type
            .map(|kind| format!(" of type '{kind}'"))
            .unwrap_or_default();
        return format!("No relations{filter} found for '{name}'");
    }
    let mut output = format!("# Related to: {name}\n\n");
    let mut by_type: IndexMap<String, (Vec<Entity>, Vec<Entity>)> = IndexMap::new();
    let row = |other: &str| {
        entity(view, other).unwrap_or_else(|| {
            let mut stub = Map::new();
            stub.insert("name".to_owned(), json!(other));
            stub
        })
    };
    for relation in &found {
        let group = by_type
            .entry(py_display(&relation.relation_type()))
            .or_default();
        if relation.source == id {
            group.0.push(row(relation.target));
        } else {
            group.1.push(row(relation.source));
        }
    }
    for (kind, (outgoing, incoming)) in &by_type {
        output += &format!("## {kind}\n\n");
        for (rows, label, arrow) in [(outgoing, "Outgoing", "→"), (incoming, "Incoming", "←")] {
            if rows.is_empty() {
                continue;
            }
            output += &format!("**{label} ({}):**\n", rows.len());
            for other in rows.iter().take(10) {
                output += &format!(
                    "- {arrow} **{}** ({})\n",
                    get_display(other, "name"),
                    get_display_or(other, "type", "unknown")
                );
            }
            if rows.len() > 10 {
                output += &format!("- ... and {} more\n", rows.len() - 10);
            }
        }
        output += "\n";
    }
    output
}

// ---------------------------------------------------- cross-source, stats

fn get_cross_source_relations(view: &GraphView, params: &Map<String, Value>) -> String {
    let cross = query::cross_source_relations(view);
    if wants_json(params, false) {
        return dumps(&json!({"relations": cross, "total": cross.len()}));
    }
    if cross.is_empty() {
        return "No cross-source relations found. These appear when entities from different toolkits are related.".to_owned();
    }
    let mut output = format!("# Cross-Source Relations ({})\n\n", cross.len());
    let joined = |value: &Value| {
        value
            .as_array()
            .map(|items| items.iter().map(py_display).collect::<Vec<_>>().join(", "))
            .unwrap_or_default()
    };
    for relation in cross.iter().take(50) {
        output += &format!(
            "- **{}** ({}) → {} → **{}** ({})\n",
            py_display(&relation["source"]),
            joined(&relation["source_toolkits"]),
            py_display(&relation["relation_type"]),
            py_display(&relation["target"]),
            joined(&relation["target_toolkits"]),
        );
    }
    if cross.len() > 50 {
        output += &format!("\n... and {} more\n", cross.len() - 50);
    }
    output
}

/// Whether the runner substituted an empty view for a graph the store does
/// not have.
fn no_graph(view: &GraphView) -> bool {
    view.revision == 0 && view.graph.node_count() == 0 && view.graph.edge_count() == 0
}

fn get_stats(view: &GraphView, params: &Map<String, Value>) -> String {
    let json = wants_json(params, false);
    if no_graph(view) {
        if json {
            return dumps(&json!({
                "node_count": 0, "edge_count": 0, "entity_types": {}, "relation_types": {},
                "sources": [], "edge_types": [], "source_toolkits": [],
                "message": "No graph data yet. Run ingestion to populate the knowledge graph.",
            }));
        }
        return "No graph data yet. Run ingestion to populate the knowledge graph.".to_owned();
    }
    if json {
        return dumps(&Value::Object(stats(view)));
    }
    wrapper_stats(view)
}

/// `InventoryRetrievalApiWrapper.get_stats`.
fn wrapper_stats(view: &GraphView) -> String {
    let stats = stats(view);
    let mut output = "# Knowledge Graph Statistics\n\n".to_owned();
    output += &format!("**Entities:** {}\n", py_display(&stats["node_count"]));
    output += &format!("**Relations:** {}\n", py_display(&stats["edge_count"]));
    let by_count = |key: &str| -> Vec<(String, i64)> {
        let mut rows: Vec<(String, i64)> = stats[key]
            .as_object()
            .map(|counts| {
                counts
                    .iter()
                    .map(|(name, count)| (name.clone(), count.as_i64().unwrap_or(0)))
                    .collect()
            })
            .unwrap_or_default();
        rows.sort_by_key(|row| std::cmp::Reverse(row.1));
        rows
    };
    for (key, title) in [
        ("entity_types", "Entity Types"),
        ("relation_types", "Relation Types"),
    ] {
        let rows = by_count(key);
        if !rows.is_empty() {
            output += &format!("\n## {title}\n");
            for (name, count) in rows {
                output += &format!("- {name}: {count}\n");
            }
        }
    }
    if let Some(sources) = stats["source_toolkits"]
        .as_array()
        .filter(|s| !s.is_empty())
    {
        output += "\n## Sources\n";
        for source in sources {
            output += &format!("- {}\n", py_display(source));
        }
    }
    if py_truthy(&stats["last_saved"]) {
        output += &format!("\n**Last updated:** {}\n", py_display(&stats["last_saved"]));
    }
    output
}

/// `{bucket}/{graph_name}/graph.json` (D7) and its two parts.
fn graph_location(params: &Map<String, Value>) -> (String, String) {
    let bucket = truthy_str(params, "bucket").unwrap_or_else(|| "inventory".to_owned());
    let name = truthy_str(params, "graph_name")
        .or_else(|| truthy_str(params, "toolkit_configuration_graph_name"))
        .map(|name| pystr::strip(&name).to_owned())
        .filter(|name| !name.is_empty())
        .unwrap_or_else(|| "inventory".to_owned());
    (bucket, name)
}

fn get_graph_info(view: &GraphView, params: &Map<String, Value>) -> String {
    if wants_json(params, false) {
        let (bucket, name) = graph_location(params);
        let mut document = Map::new();
        document.insert(
            "path".to_owned(),
            json!(format!("{bucket}/{name}/graph.json")),
        );
        document.extend(stats(view));
        return dumps(&Value::Object(document));
    }
    wrapper_stats(view)
}

fn list_ingested_sources(view: &GraphView, params: &Map<String, Value>) -> String {
    let stats = stats(view);
    let sources: Vec<String> = stats["source_toolkits"]
        .as_array()
        .map(|items| items.iter().map(py_display).collect())
        .unwrap_or_default();
    let mut entities_by_source: IndexMap<String, i64> = IndexMap::new();
    for (_, node) in view.graph.nodes() {
        for citation in citations_of(node) {
            if let Some(citation) = citation.as_object() {
                *entities_by_source
                    .entry(get_display_or(citation, "source_toolkit", "unknown"))
                    .or_insert(0) += 1;
            }
        }
    }
    let relations_of = |source: &str| {
        stats["relations_by_source"]
            .get(source)
            .and_then(Value::as_i64)
            .unwrap_or(0)
    };
    let entities_of = |source: &str| entities_by_source.get(source).copied().unwrap_or(0);
    if wants_json(params, false) {
        let rows: Vec<Value> = sources
            .iter()
            .map(|source| {
                json!({
                    "source_toolkit": source,
                    "entity_count": entities_of(source),
                    "relation_count": relations_of(source),
                })
            })
            .collect();
        return dumps(&json!({"sources": rows, "total_sources": sources.len()}));
    }
    if sources.is_empty() {
        return "No sources have been ingested yet. Use run_ingestion to add data from a toolkit."
            .to_owned();
    }
    let mut output = format!("# Ingested Sources ({})\n\n", sources.len());
    for source in &sources {
        output += &format!(
            "- **{source}**: {} entities, {} relations\n",
            entities_of(source),
            relations_of(source)
        );
    }
    output
}

/// D6: the one graph of this toolkit.
fn list_graphs(view: &GraphView, params: &Map<String, Value>) -> String {
    let (bucket, name) = graph_location(params);
    let graphs: Vec<Value> = if no_graph(view) {
        Vec::new()
    } else {
        let saved = view
            .graph
            .metadata
            .get("last_saved")
            .and_then(Value::as_str)
            .unwrap_or("");
        vec![json!({"name": name, "size": view.graph.to_json_text(saved).len()})]
    };
    if wants_json(params, false) {
        return dumps(&json!({"graphs": graphs, "bucket": bucket}));
    }
    if graphs.is_empty() {
        return format!("No graphs found in bucket '{bucket}'");
    }
    let mut output = format!("# Available Graphs in '{bucket}'\n\n");
    for graph in &graphs {
        #[allow(clippy::cast_precision_loss)]
        let kilobytes = graph["size"].as_u64().unwrap_or(0) as f64 / 1024.0;
        output += &format!("- **{}** ({kilobytes:.1} KB)\n", py_display(&graph["name"]));
    }
    output
}

fn load_graph(view: &GraphView, params: &Map<String, Value>) -> String {
    let Some(name) = params.get("graph_name").filter(|v| py_truthy(v)) else {
        return "Error: graph_name is required".to_owned();
    };
    format!(
        "Loaded graph: {}\nNodes: {}, Edges: {}",
        py_display(name),
        view.graph.node_count(),
        view.graph.edge_count()
    )
}

// ------------------------------------------------------------- listings

fn listing_document(view: &GraphView, head: [(&str, Value); 2], ids: &[String]) -> String {
    let set: IndexSet<String> = ids.iter().cloned().collect();
    let (edges, _) = edges_touching(view, &set);
    let mut document = Map::new();
    for (key, value) in head {
        document.insert(key.to_owned(), value);
    }
    document.insert(
        "entities".to_owned(),
        Value::Array(ids.iter().map(|id| public_row(view, id)).collect()),
    );
    document.insert("edges".to_owned(), Value::Array(edges));
    document.insert("total".to_owned(), json!(ids.len()));
    dumps(&Value::Object(document))
}

fn source_filtered(
    view: &GraphView,
    ids: Vec<String>,
    source: Option<&str>,
    limit: i64,
) -> Vec<String> {
    let Some(source) = source else {
        return ids;
    };
    let mut kept: Vec<String> = ids
        .into_iter()
        .filter(|id| view.node(id).is_some_and(|node| cited_by(node, source)))
        .collect();
    kept.truncate(head_len(kept.len(), limit));
    kept
}

fn from_source_suffix(source: Option<&str>) -> String {
    source
        .map(|source| format!(" from source '{source}'"))
        .unwrap_or_default()
}

fn list_entities_by_type(
    view: &GraphView,
    params: &Map<String, Value>,
) -> Result<String, EngineError> {
    let entity_type = str_param(params, "entity_type", "");
    let source = truthy_str(params, "source_toolkit");
    let limit = int_param(params, "limit", 50)?;
    let wide = if source.is_some() {
        limit.saturating_mul(2)
    } else {
        limit
    };
    let ids = source_filtered(
        view,
        ids_by_type(view, &entity_type, wide),
        source.as_deref(),
        limit,
    );
    if wants_json(params, false) {
        return Ok(listing_document(
            view,
            [
                ("entity_type", json!(entity_type)),
                ("source_toolkit", raw(params, "source_toolkit")),
            ],
            &ids,
        ));
    }
    if ids.is_empty() {
        return Ok(format!(
            "No entities of type '{entity_type}' found{}",
            from_source_suffix(source.as_deref())
        ));
    }
    // The wrapper lists again, WITHOUT the source filter, as Python did.
    let listed = ids_by_type(view, &entity_type, limit);
    if listed.is_empty() {
        return Ok(format!("No entities of type '{entity_type}' found"));
    }
    let mut output = format!("# Entities of type '{entity_type}' ({})\n\n", listed.len());
    for id in &listed {
        let node = entity(view, id).unwrap_or_default();
        output += &format!(
            "- **{}** - `{}`\n",
            get_display(&node, "name"),
            location_of(&node)
        );
    }
    if i64::try_from(listed.len()).is_ok_and(|n| n == limit) {
        output += &format!("\n*Limited to {limit} results*\n");
    }
    Ok(output)
}

fn list_entities_by_layer(
    view: &GraphView,
    params: &Map<String, Value>,
) -> Result<String, EngineError> {
    let layer = str_param(params, "layer", "");
    let source = truthy_str(params, "source_toolkit");
    let limit = int_param(params, "limit", 50)?;
    let wide = if source.is_some() {
        limit.saturating_mul(2)
    } else {
        limit
    };
    let ids = source_filtered(
        view,
        ids_by_layer(view, &layer, wide),
        source.as_deref(),
        limit,
    );
    if wants_json(params, false) {
        return Ok(listing_document(
            view,
            [
                ("layer", json!(layer)),
                ("source_toolkit", raw(params, "source_toolkit")),
            ],
            &ids,
        ));
    }
    if ids.is_empty() {
        return Ok(format!(
            "No entities in layer '{layer}' found{}",
            from_source_suffix(source.as_deref())
        ));
    }
    let listed = ids_by_layer(view, &layer, limit);
    if listed.is_empty() {
        return Ok(format!(
            "No entities in layer '{layer}' found. Available layers: {}",
            available_layers()
        ));
    }
    let mut output = format!("# Entities in layer '{layer}' ({})\n\n", listed.len());
    let mut by_type: IndexMap<String, Vec<Entity>> = IndexMap::new();
    for id in &listed {
        let node = entity(view, id).unwrap_or_default();
        by_type
            .entry(get_display_or(&node, "type", "unknown"))
            .or_default()
            .push(node);
    }
    let mut groups: Vec<(&String, &Vec<Entity>)> = by_type.iter().collect();
    groups.sort_by_key(|group| std::cmp::Reverse(group.1.len()));
    for (kind, rows) in groups {
        output += &format!("## {kind} ({})\n", rows.len());
        for row in rows.iter().take(10) {
            match row.get("file_path").filter(|v| py_truthy(v)) {
                Some(path) => {
                    output += &format!(
                        "- **{}** - `{}`\n",
                        get_display(row, "name"),
                        py_display(path)
                    );
                }
                None => output += &format!("- **{}**\n", get_display(row, "name")),
            }
        }
        if rows.len() > 10 {
            output += &format!("- ... and {} more\n", rows.len() - 10);
        }
        output += "\n";
    }
    Ok(output)
}

fn list_entities_by_source(
    view: &GraphView,
    params: &Map<String, Value>,
) -> Result<String, EngineError> {
    let source = str_param(params, "source_toolkit", "");
    let entity_type = truthy_str(params, "entity_type");
    let limit = int_param(params, "limit", 50)?;
    if source.is_empty() {
        return Ok("Error: source_toolkit is required".to_owned());
    }
    let mut ids: Vec<String> = Vec::new();
    for (id, node) in view.graph.nodes() {
        if let Some(kind) = &entity_type
            && text(node, "type").to_lowercase() != kind.to_lowercase()
        {
            continue;
        }
        if cited_by(node, &source) {
            ids.push(id.to_owned());
        }
        if i64::try_from(ids.len()).is_ok_and(|n| n >= limit) {
            break;
        }
    }
    if wants_json(params, false) {
        return Ok(listing_document(
            view,
            [
                ("source_toolkit", json!(source)),
                ("entity_type", raw(params, "entity_type")),
            ],
            &ids,
        ));
    }
    if ids.is_empty() {
        let filter = entity_type
            .map(|kind| format!(" of type '{kind}'"))
            .unwrap_or_default();
        return Ok(format!("No entities{filter} found from source '{source}'"));
    }
    let mut output = format!("# Entities from '{source}' ({})\n\n", ids.len());
    for id in &ids {
        let node = entity(view, id).unwrap_or_default();
        output += &format!(
            "- **{}** ({})\n",
            get_display(&node, "name"),
            get_display_or(&node, "type", "unknown")
        );
    }
    if i64::try_from(ids.len()).is_ok_and(|n| n == limit) {
        output += &format!("\n*Limited to {limit} results*\n");
    }
    Ok(output)
}

// ------------------------------------------------- ids and neighbourhoods

/// The ids of a `entity_ids` parameter: an array's strings (anything else
/// is no ids).
fn id_list(params: &Map<String, Value>) -> Vec<String> {
    match params.get("entity_ids") {
        Some(Value::Array(items)) => items.iter().map(py_display).collect(),
        _ => Vec::new(),
    }
}

fn no_results(json: bool, error: &str) -> String {
    if json {
        dumps(&json!({"results": [], "edges": [], "error": error}))
    } else {
        error.to_owned()
    }
}

/// `{"source", "target", "type"}` of every edge among `ids`, by source in
/// `ids` order.
fn edges_within(view: &GraphView, ids: &IndexSet<String>) -> Vec<Value> {
    let mut edges = Vec::new();
    for source in ids {
        for (target, attributes) in view.out_edges(source) {
            if ids.contains(target) {
                edges.push(json!({
                    "source": source,
                    "target": target,
                    "type": attributes.get("relation_type").cloned().unwrap_or_else(|| json!("RELATED")),
                }));
            }
        }
    }
    edges
}

fn get_entities_by_ids(
    view: &GraphView,
    params: &Map<String, Value>,
) -> Result<String, EngineError> {
    let json = wants_json(params, true);
    let entity_ids = id_list(params);
    let include_edges = flag(params, "include_edges", true);
    let include_bridging = flag(params, "include_bridging", true);
    let max_bridge_length = int_param(params, "max_bridge_length", 4)?;
    if entity_ids.is_empty() {
        return Ok(no_results(json, "No entity_ids provided"));
    }
    let mut results: Vec<(Entity, f64)> = Vec::new();
    let mut id_set: IndexSet<String> = entity_ids.iter().cloned().collect();
    for id in &entity_ids {
        if let Some(row) = entity(view, id) {
            results.push((row, 1.0));
        }
    }
    let mut bridging = query::Bridging {
        clusters: 1,
        ..query::Bridging::default()
    };
    if include_bridging && results.len() > 1 {
        bridging = find_bridging_nodes(view, &entity_ids, max_bridge_length, 20);
        for bridge in &bridging.nodes {
            if !id_set.contains(bridge)
                && let Some(mut row) = entity(view, bridge)
            {
                row.insert("is_bridging".to_owned(), json!(true));
                results.push((row, 0.5));
                id_set.insert(bridge.clone());
            }
        }
    }
    let mut edges: Vec<Value> = if include_edges && !results.is_empty() {
        edges_within(view, &id_set)
    } else {
        Vec::new()
    };
    if include_bridging {
        let key = |edge: &Value| {
            format!(
                "{}--{}-->{}",
                py_display(&edge["source"]),
                py_display(&edge["type"]),
                py_display(&edge["target"])
            )
        };
        let mut existing: IndexSet<String> = edges.iter().map(key).collect();
        for edge in &bridging.edges {
            if existing.insert(key(edge)) {
                edges.push(edge.clone());
            }
        }
    }
    if json {
        let rows: Vec<Value> = results
            .into_iter()
            .map(|(row, score)| json!({"entity": public(row), "score": score}))
            .collect();
        return Ok(dumps(&json!({
            "results": rows,
            "edges": edges,
            "total_entities": rows.len(),
            "total_edges": edges.len(),
            "clusters_found": bridging.clusters,
            "bridging_nodes_added": bridging.nodes.len(),
        })));
    }
    Ok(ids_text(&results, &edges))
}

/// The text form of `get_entities_by_ids`.
fn ids_text(results: &[(Entity, f64)], edges: &[Value]) -> String {
    if results.is_empty() {
        return "No entities found for the provided IDs.".to_owned();
    }
    let mut output = format!(
        "Found {} entities and {} connecting edges:\n\n",
        results.len(),
        edges.len()
    );
    for (row, _) in results {
        output += &format!(
            "- **{}** ({})\n",
            get_display(row, "name"),
            get_display_or(row, "type", "unknown")
        );
        output += &format!("  ID: {}\n", get_display(row, "id"));
        if let Some(description) = row.get("description").filter(|v| py_truthy(v)) {
            output += &format!(
                "  Description: {}...\n",
                prefix_chars(&py_display(description), 100)
            );
        }
        output += "\n";
    }
    if !edges.is_empty() {
        output += "\n## Edges:\n";
        for edge in edges.iter().take(20) {
            output += &format!(
                "- {} --[{}]--> {}\n",
                py_display(&edge["source"]),
                py_display(&edge["type"]),
                py_display(&edge["target"])
            );
        }
        if edges.len() > 20 {
            output += &format!("  ...and {} more edges\n", edges.len() - 20);
        }
    }
    output
}

/// `_tool_get_entity_neighbors`: the entities within `depth` (1–3) hops,
/// in discovery order (Python iterated a set here, so its order varied).
fn get_entity_neighbors(
    view: &GraphView,
    params: &Map<String, Value>,
) -> Result<String, EngineError> {
    let json = wants_json(params, true);
    let origin = params.get("entity_id").cloned().unwrap_or(Value::Null);
    let depth = int_param(params, "depth", 1)?.clamp(1, 3);
    if !py_truthy(&origin) {
        return Ok(no_results(json, "No entity_id provided"));
    }
    let origin_text = py_display(&origin);
    let Some(origin_id) = origin.as_str().filter(|id| view.node(id).is_some()) else {
        return Ok(no_results(
            json,
            &format!("Entity '{origin_text}' not found in graph"),
        ));
    };
    let mut current: IndexSet<String> = IndexSet::from([origin_id.to_owned()]);
    let mut all = current.clone();
    for _ in 0..depth {
        let mut next = IndexSet::new();
        for id in &current {
            new_neighbours(view, id, &all, &mut next);
        }
        if next.is_empty() {
            break;
        }
        all.extend(next.iter().cloned());
        current = next;
    }
    let mut results = Vec::new();
    for id in &all {
        if let Some(mut row) = entity(view, id) {
            let is_origin = id == origin_id;
            row.insert("is_origin".to_owned(), json!(is_origin));
            results.push((row, if is_origin { 1.0 } else { 0.5 }));
        }
    }
    let edges = edges_within(view, &all);
    if json {
        let rows: Vec<Value> = results
            .into_iter()
            .map(|(row, score)| json!({"entity": public(row), "score": score}))
            .collect();
        return Ok(dumps(&json!({
            "results": rows,
            "edges": edges,
            "total_entities": rows.len(),
            "total_edges": edges.len(),
            "origin_entity_id": origin,
            "depth": depth,
        })));
    }
    let mut output = format!(
        "Found {} entities within {depth} hop(s) of '{origin_text}':\n\n",
        results.len()
    );
    for (row, _) in &results {
        let marker = if row.get("is_origin").is_some_and(py_truthy) {
            " (origin)"
        } else {
            ""
        };
        output += &format!(
            "- **{}** ({}){marker}\n",
            get_display(row, "name"),
            get_display_or(row, "type", "unknown")
        );
    }
    if !edges.is_empty() {
        output += &format!("\n{} edges connecting these entities.\n", edges.len());
    }
    Ok(output)
}

/// The JSON form of a `search_advanced` hit (`query_graph`).
pub(super) fn hit_row(view: &GraphView, hit: &query::Hit) -> Value {
    json!({"entity": public_row(view, &hit.id), "score": hit.score, "match_field": hit.match_field})
}

/// Re-exported for `query_graph`.
pub(super) fn advanced(view: &GraphView, filters: &AdvancedFilters, top_k: i64) -> Vec<query::Hit> {
    query::search_advanced(view, filters, top_k)
}
