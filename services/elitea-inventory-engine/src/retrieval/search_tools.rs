//! The `inventory_search` family's tools, except `investigate`: the
//! read-only surface another agent's toolkit references
//! (`_handle_inventory_search_tool`'s `tool_mapping`).
//!
//! Three are the `inventory` handlers under other names —
//! `search_knowledge_graph` is `_tool_search_graph`, `get_entity_details` is
//! `_tool_get_entity`, `get_related_entities` is `_tool_get_related_entities`
//! (the same name in both families, so routing is by family) — and two are
//! this family's own: `query_graph` (`_tool_query_graph`, the JQL-like
//! grammar of `_parse_graph_query`) and `list_entity_types`
//! (`_tool_list_entity_types_only`). The deviations of the shared handlers
//! are documented in [`super::inventory_tools`]; `query_graph` shares D1
//! (rows carry `id`), D4 (an id is accepted for `related:`) and D5 (no raw
//! vectors).
//!
//! `shlex.split` is ported as `query_graph` uses it ([`shlex_split`]): POSIX
//! mode, whitespace splitting, no comments; an unbalanced quote or a
//! trailing backslash falls back to `str.split()` as Python's `ValueError`
//! did.

// The answers are built line by line, as the Python handlers built them.
#![allow(clippy::format_push_string)]

use super::inventory_tools::{
    advanced, error_answer_for, get_entity, get_related_entities, hit_row, int_param, public_row,
    search_graph, wants_json,
};
use super::query::{
    AdvancedFilters, entity, find_entity_by_reference, get_display_or, py_display, py_repr_value,
    relations, text, type_layer,
};
use super::view::GraphView;
use super::{Call, Handled, answer};
use crate::graph::layer_types;
use elitea_engine_core::errors::EngineError;
use elitea_engine_core::pyjson::dumps;
use elitea_engine_core::pystr;
use elitea_engine_core::pyvalue::py_truthy;
use indexmap::IndexMap;
use serde_json::{Map, Value, json};

/// See [`super::dispatch`].
#[must_use]
pub fn handle(call: &Call<'_>) -> Handled {
    if call.family != "inventory_search" {
        return None;
    }
    let (view, params) = (call.view, call.params);
    let result = match call.tool {
        "search_knowledge_graph" => search_graph(view, params),
        "get_entity_details" => Ok(get_entity(view, params)),
        "get_related_entities" => Ok(get_related_entities(view, params)),
        "query_graph" => query_graph(view, params),
        "list_entity_types" => Ok(list_entity_types(view)),
        _ => return None,
    };
    Some(result.map(answer))
}

/// `_tool_list_entity_types_only`.
fn list_entity_types(view: &GraphView) -> String {
    let mut counts: IndexMap<String, usize> = IndexMap::new();
    for (_, node) in view.graph.nodes() {
        *counts
            .entry(get_display_or(node, "type", "unknown"))
            .or_insert(0) += 1;
    }
    let mut rows: Vec<(String, usize)> = counts.into_iter().collect();
    rows.sort_by_key(|row| std::cmp::Reverse(row.1));
    let mut lines = vec![
        "Entity Types in Knowledge Graph:".to_owned(),
        "=".repeat(40),
    ];
    let mut total = 0;
    for (kind, count) in rows {
        lines.push(format!("  {kind}: {count}"));
        total += count;
    }
    lines.push("-".repeat(40));
    lines.push(format!("  Total: {total} entities"));
    lines.join("\n")
}

/// `shlex.split(text)`: `None` where Python raised `ValueError`.
#[must_use]
pub fn shlex_split(text: &str) -> Option<Vec<String>> {
    let mut tokens = Vec::new();
    let mut token = String::new();
    // A token is open (even an empty quoted one).
    let mut open = false;
    let mut chars = text.chars();
    while let Some(c) = chars.next() {
        match c {
            ' ' | '\t' | '\r' | '\n' => {
                if open {
                    tokens.push(std::mem::take(&mut token));
                    open = false;
                }
            }
            '\\' => {
                token.push(chars.next()?);
                open = true;
            }
            '\'' => {
                open = true;
                loop {
                    match chars.next()? {
                        '\'' => break,
                        other => token.push(other),
                    }
                }
            }
            '"' => {
                open = true;
                loop {
                    match chars.next()? {
                        '"' => break,
                        '\\' => {
                            let escaped = chars.next()?;
                            if escaped != '"' && escaped != '\\' {
                                token.push('\\');
                            }
                            token.push(escaped);
                        }
                        other => token.push(other),
                    }
                }
            }
            other => {
                token.push(other);
                open = true;
            }
        }
    }
    if open {
        tokens.push(token);
    }
    Some(tokens)
}

/// `_parse_graph_query`: the JQL-like query string as parameters, in the
/// order Python inserted them.
#[must_use]
pub fn parse_graph_query(query: &str) -> Map<String, Value> {
    let mut params = Map::new();
    let query = pystr::strip(query);
    if query.is_empty() {
        return params;
    }
    let tokens = shlex_split(query)
        .unwrap_or_else(|| pystr::split_whitespace(query).map(str::to_owned).collect());
    let mut unmatched: Vec<String> = Vec::new();
    for token in tokens {
        let Some((key, value)) = token.split_once(':') else {
            unmatched.push(token);
            continue;
        };
        let key = pystr::strip(&key.to_lowercase()).to_owned();
        let name = match key.as_str() {
            "type" | "types" => "types",
            "layer" | "layers" => "layers",
            "file" | "files" => "files",
            "name" | "text" | "query" => "name",
            "related" | "related_to" => "related_to",
            "rel" | "relation" => "relation_types",
            "dir" | "direction" => "direction",
            "has_rel" | "has_relations" => "has_relations",
            "limit" => "limit",
            _ => {
                unmatched.push(token);
                continue;
            }
        };
        match name {
            "types" | "layers" | "files" | "relation_types" => {
                let values: Vec<Value> = value
                    .split(',')
                    .map(pystr::strip)
                    .filter(|v| !v.is_empty())
                    .map(|v| json!(v))
                    .collect();
                match params.get_mut(name) {
                    Some(Value::Array(existing)) => existing.extend(values),
                    _ => {
                        params.insert(name.to_owned(), Value::Array(values));
                    }
                }
            }
            "limit" => {
                if let Some(limit) = super::inventory_tools::py_int(&json!(value))
                    .filter(|_| !value.trim().is_empty())
                {
                    params.insert(name.to_owned(), json!(limit));
                }
            }
            "has_relations" => {
                let truthy = matches!(value.to_lowercase().as_str(), "true" | "yes" | "1");
                params.insert(name.to_owned(), json!(truthy));
            }
            _ => {
                params.insert(name.to_owned(), json!(value));
            }
        }
    }
    if !unmatched.is_empty() && !params.contains_key("name") {
        params.insert("name".to_owned(), json!(unmatched.join(" ")));
    }
    params
}

/// `to_list`: a list as it is, a non-blank string split on commas, else `[]`.
fn to_list(value: Option<&Value>) -> Vec<Value> {
    match value {
        Some(Value::Array(items)) => items.clone(),
        Some(Value::String(text)) if !pystr::strip(text).is_empty() => text
            .split(',')
            .map(pystr::strip)
            .filter(|v| !v.is_empty())
            .map(|v| json!(v))
            .collect(),
        _ => Vec::new(),
    }
}

fn strings(values: &[Value]) -> Vec<String> {
    values.iter().map(py_display).collect()
}

/// `params.get(key, params.get(fallback, default))`.
fn either<'a>(params: &'a Map<String, Value>, key: &str, fallback: &str) -> Option<&'a Value> {
    if params.contains_key(key) {
        params.get(key)
    } else {
        params.get(fallback)
    }
}

/// `_tool_query_graph`.
fn query_graph(view: &GraphView, call_params: &Map<String, Value>) -> Result<String, EngineError> {
    let json = wants_json(call_params, false);
    let mut params = call_params.clone();
    if let Some(jql) = params
        .get("query")
        .and_then(Value::as_str)
        .filter(|q| !q.is_empty())
        && !["types", "layers", "files", "related_to"]
            .iter()
            .any(|key| params.get(*key).is_some_and(py_truthy))
    {
        for (key, value) in parse_graph_query(jql) {
            if !params.get(&key).is_some_and(py_truthy) {
                params.insert(key, value);
            }
        }
    }
    let entity_types = to_list(either(&params, "types", "entity_types"));
    let layers = to_list(params.get("layers"));
    let file_patterns = to_list(either(&params, "files", "file_patterns"));
    let text_value = either(&params, "name", "text")
        .cloned()
        .unwrap_or_else(|| json!(""));
    let text_filter = Some(&text_value).filter(|v| py_truthy(v)).map(py_display);
    let has_relations = params
        .get("has_relations")
        .filter(|v| !v.is_null())
        .map(py_truthy);
    let limit = int_param(&params, "limit", 30)?.min(100);
    let related_to = params
        .get("related_to")
        .filter(|v| py_truthy(v))
        .map(py_display);
    let relation_types = strings(&to_list(params.get("relation_types")));
    let direction = match params.get("direction") {
        None | Some(Value::Null) => "both".to_owned(),
        Some(value) => py_display(value),
    };
    let type_names = strings(&entity_types);
    let layer_names = strings(&layers);

    let q = GraphQuery {
        json,
        entity_types,
        layers,
        file_patterns,
        text_value,
        text_filter,
        has_relations,
        limit,
        relation_types,
        direction,
        type_names,
        layer_names,
    };
    match related_to {
        Some(related_to) => Ok(related_query(view, &q, &related_to)),
        None => Ok(structured_query(view, q)),
    }
}

/// `query_graph`'s parameters, after the JQL merge.
struct GraphQuery {
    json: bool,
    entity_types: Vec<Value>,
    layers: Vec<Value>,
    file_patterns: Vec<Value>,
    text_value: Value,
    text_filter: Option<String>,
    has_relations: Option<bool>,
    limit: i64,
    relation_types: Vec<String>,
    direction: String,
    type_names: Vec<String>,
    layer_names: Vec<String>,
}

/// The `related:` form of `query_graph`.
fn related_query(view: &GraphView, q: &GraphQuery, related_to: &str) -> String {
    let resolved = match find_entity_by_reference(view, related_to) {
        Ok(resolved) => resolved,
        Err(message) => return error_answer_for(q.json, message),
    };
    let base = &resolved.id;
    let wanted_relations: Vec<String> = q.relation_types.iter().map(|t| t.to_lowercase()).collect();
    let mut expanded_types: Vec<String> = q.type_names.iter().map(|t| t.to_lowercase()).collect();
    for kind in q.type_names.iter().map(|t| t.to_lowercase()) {
        if let Some(types) = layer_types(&kind) {
            expanded_types.extend(types.iter().map(|t| (*t).to_owned()));
        }
    }
    let wanted_layers: Vec<String> = q.layer_names.iter().map(|l| l.to_lowercase()).collect();
    let mut results: Vec<(String, Value, &'static str)> = Vec::new();
    let mut seen: Vec<String> = Vec::new();
    for relation in relations(view, base, &q.direction) {
        let kind = relation.relation_type();
        if !wanted_relations.is_empty()
            && !wanted_relations.contains(&kind.as_str().unwrap_or("").to_lowercase())
        {
            continue;
        }
        let (other, way) = if relation.source == base {
            (relation.target, "outgoing")
        } else {
            (relation.source, "incoming")
        };
        if seen.iter().any(|id| id == other) {
            continue;
        }
        seen.push(other.to_owned());
        let Some(node) = view.node(other) else {
            continue;
        };
        let etype = text(node, "type").to_lowercase();
        let mut elayer = text(node, "layer").to_owned();
        if elayer.is_empty() {
            type_layer(&etype).clone_into(&mut elayer);
        }
        if !q.type_names.is_empty() && !expanded_types.contains(&etype) {
            continue;
        }
        if !q.layer_names.is_empty() && !wanted_layers.contains(&elayer.to_lowercase()) {
            continue;
        }
        if let Some(filter) = &q.text_filter
            && !text(node, "name")
                .to_lowercase()
                .contains(&filter.to_lowercase())
        {
            continue;
        }
        results.push((other.to_owned(), kind, way));
        if i64::try_from(results.len()).is_ok_and(|n| n >= q.limit) {
            break;
        }
    }
    if q.json {
        let rows: Vec<Value> = results
            .iter()
            .map(|(id, kind, way)| {
                json!({"entity": public_row(view, id), "relation_type": kind, "direction": way})
            })
            .collect();
        return dumps(&json!({
            "base_entity": public_row(view, base),
            "related": rows,
            "total": rows.len(),
        }));
    }
    if results.is_empty() {
        return format!("No related entities found for '{related_to}' matching filters.");
    }
    let base_row = entity(view, base).unwrap_or_default();
    let mut output = format!(
        "# Entities related to {} ({})\n",
        get_display_or(&base_row, "name", "Unknown"),
        get_display_or(&base_row, "type", "")
    );
    output += &format!("Found {} results\n\n", results.len());
    for (id, kind, way) in &results {
        let row = entity(view, id).unwrap_or_default();
        let arrow = if *way == "outgoing" { "→" } else { "←" };
        output += &format!(
            "- {arrow} [{}] **{}** ({})",
            py_display(kind),
            get_display_or(&row, "name", "Unknown"),
            get_display_or(&row, "type", "")
        );
        output += &located(&row);
        output += "\n";
    }
    output
}

/// The structured (`search_advanced`) form of `query_graph`.
fn structured_query(view: &GraphView, q: GraphQuery) -> String {
    let filters = AdvancedFilters {
        query: q.text_filter.clone(),
        entity_types: q.type_names.clone(),
        layers: q.layer_names.clone(),
        file_patterns: strings(&q.file_patterns),
        has_relations: q.has_relations,
    };
    let hits = advanced(view, &filters, q.limit);
    if q.json {
        let rows: Vec<Value> = hits.iter().map(|hit| hit_row(view, hit)).collect();
        return dumps(&json!({
            "results": rows,
            "total": rows.len(),
            "filters": {
                "types": q.entity_types,
                "layers": q.layers,
                "files": q.file_patterns,
                "text": q.text_value,
            },
        }));
    }
    if hits.is_empty() {
        return no_match(q);
    }
    let mut by_layer: IndexMap<String, Vec<String>> = IndexMap::new();
    for hit in &hits {
        let node = view.node(&hit.id).cloned().unwrap_or_default();
        let mut layer = text(&node, "layer").to_owned();
        if layer.is_empty() {
            let etype = text(&node, "type").to_lowercase();
            layer = match type_layer(&etype) {
                "" => "other".to_owned(),
                known => known.to_owned(),
            };
        }
        by_layer.entry(layer).or_default().push(hit.id.clone());
    }
    let mut output = format!("# Query Results | {} entities\n", hits.len());
    let mut named = Vec::new();
    if !q.type_names.is_empty() {
        named.push(format!("types: {}", q.type_names.join(", ")));
    }
    if !q.layer_names.is_empty() {
        named.push(format!("layers: {}", q.layer_names.join(", ")));
    }
    if !q.file_patterns.is_empty() {
        named.push(format!("files: {}", strings(&q.file_patterns).join(", ")));
    }
    if let Some(filter) = &q.text_filter {
        named.push(format!("text: '{filter}'"));
    }
    if !named.is_empty() {
        output += &format!("Filters: {}\n", named.join(" | "));
    }
    output += "\n";
    for layer in [
        "code",
        "service",
        "data",
        "testing",
        "configuration",
        "documentation",
        "domain",
        "product",
        "knowledge",
        "structure",
        "tooling",
        "other",
    ] {
        let Some(ids) = by_layer.get(layer) else {
            continue;
        };
        output += &format!("## {} ({})\n", pystr::upper_first(layer), ids.len());
        for id in ids {
            let row = entity(view, id).unwrap_or_default();
            output += &format!(
                "- **{}** ({})",
                get_display_or(&row, "name", "Unknown"),
                get_display_or(&row, "type", "")
            );
            output += &located(&row);
            output += "\n";
        }
        output += "\n";
    }
    output
}

/// The answer of a structured query nothing matched.
fn no_match(q: GraphQuery) -> String {
    let mut named = Vec::new();
    if !q.entity_types.is_empty() {
        named.push(format!(
            "types={}",
            py_repr_value(&Value::Array(q.entity_types))
        ));
    }
    if !q.layers.is_empty() {
        named.push(format!("layers={}", py_repr_value(&Value::Array(q.layers))));
    }
    if !q.file_patterns.is_empty() {
        named.push(format!(
            "files={}",
            py_repr_value(&Value::Array(q.file_patterns))
        ));
    }
    if let Some(filter) = &q.text_filter {
        named.push(format!("text='{filter}'"));
    }
    let joined = if named.is_empty() {
        "none".to_owned()
    } else {
        named.join(", ")
    };
    format!("No entities found matching filters: {joined}")
}

/// ` @ source_toolkit` and ` - file_path`, each when truthy.
fn located(row: &Map<String, Value>) -> String {
    let mut out = String::new();
    if let Some(source) = row.get("source_toolkit").filter(|v| py_truthy(v)) {
        out += &format!(" @ {}", py_display(source));
    }
    if let Some(path) = row.get("file_path").filter(|v| py_truthy(v)) {
        out += &format!(" - {}", py_display(path));
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn shlex_splits_as_python() {
        assert_eq!(
            shlex_split(r#"related:"Foo (class) @ a - b" type:x"#),
            Some(vec![
                "related:Foo (class) @ a - b".to_owned(),
                "type:x".to_owned()
            ])
        );
        assert_eq!(
            shlex_split(r#"a\ b "c\"d" 'e\f' """#),
            Some(vec![
                "a b".to_owned(),
                "c\"d".to_owned(),
                "e\\f".to_owned(),
                String::new(),
            ])
        );
        assert_eq!(
            shlex_split(r#"x "y\z""#),
            Some(vec!["x".to_owned(), "y\\z".to_owned()])
        );
        assert_eq!(shlex_split("open \"quote"), None);
        assert_eq!(shlex_split("trailing\\"), None);
    }

    #[test]
    fn the_query_grammar_parses_into_parameters() {
        let parsed = parse_graph_query(
            "type:class,method layer:code type:fact limit:4 free text has_rel:yes bogus:1",
        );
        assert_eq!(
            Value::Object(parsed),
            json!({
                "types": ["class", "method", "fact"],
                "layers": ["code"],
                "limit": 4,
                "has_relations": true,
                "name": "free text bogus:1",
            })
        );
        let parsed = parse_graph_query("limit:x name:a b");
        assert_eq!(Value::Object(parsed), json!({"name": "a"}));
    }
}
