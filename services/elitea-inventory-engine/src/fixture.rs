//! The canned knowledge graph the fixture runner replays.
//!
//! Inventory has two fixture runners: this one and the Go host's
//! (`services/elitea-subapp-host/internal/apps/inventory/run/fixture.go`,
//! the E2E stack). A third, the retired Python engine's
//! (`elitea_inventory.fixture_graph`), is where these semantics came from
//! (`tests/fixtures/PROVENANCE.md`). Both answer from the SAME graph,
//! `conformance/provider/fixtures/inventory/spi/graph.json`, and both are
//! held to the same golden answers in
//! `.../inventory/{ingestion,retrieval,transfer}/` (`tests/conformance.rs`
//! here, `fixture_parity_test.go` there).
//!
//! The handlers keep the Python semantics they were ported with:
//! `first_truthy` is Python truthiness, a reference in a message is
//! `repr()`, a JSON answer is `json.dumps` byte for byte, and counts keep
//! first-seen order (a Python dict).
//!
//! The graph is packaged here (`fixtures/graph.json`, compiled in);
//! `ELITEA_INVENTORY_FIXTURES` points at another directory shaped like the
//! conformance one instead. A test asserts the packaged copy equals the
//! conformance file.
//!
//! WHAT IS CANNED IS ONLY WHAT THE ENGINE WOULD HAVE COMPUTED. Composition,
//! the artifact hand-back and the upload are the host's.

use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::pyjson::dumps;
use elitea_engine_core::pyvalue::{py_repr, py_str, py_truthy};
use serde_json::{Map, Value, json};
use std::collections::BTreeSet;
use std::path::Path;

/// The packaged graph: a checked-in copy of the conformance file.
pub const PACKAGED_GRAPH: &str = include_str!("fixtures/graph.json");

/// One node, in the shape the legacy handlers emit.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Entity {
    pub id: String,
    pub name: String,
    pub kind: String,
    pub layer: String,
    pub source_toolkit: String,
    pub file_path: String,
    pub content: String,
}

impl Entity {
    /// The row every retrieval tool answers a node in — no `content`.
    fn row(&self) -> Value {
        json!({
            "id": self.id,
            "name": self.name,
            "type": self.kind,
            "layer": self.layer,
            "source_toolkit": self.source_toolkit,
            "file_path": self.file_path,
        })
    }
}

/// One edge, `source` -> `target` under a relation type.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Relation {
    pub source: String,
    pub target: String,
    pub relation_type: String,
}

/// The graph and the read-only questions the fixture tools ask of it.
#[derive(Debug, Clone)]
pub struct FixtureGraph {
    pub entities: Vec<Entity>,
    pub relations: Vec<Relation>,
    /// Name -> description, in file order.
    pub presets: Map<String, Value>,
}

fn text_field(row: &Value, key: &str) -> Result<String, EngineError> {
    row.get(key)
        .and_then(Value::as_str)
        .map(str::to_owned)
        .ok_or_else(|| {
            EngineError::new(
                ErrorType::Value,
                format!("the fixture graph has a row without a string {key:?}"),
            )
        })
}

impl FixtureGraph {
    /// Parse a graph document (`entities`, `relations`, `presets`).
    ///
    /// # Errors
    ///
    /// A `ValueError` for a document that is not that shape.
    pub fn parse(text: &str) -> Result<Self, EngineError> {
        let document: Value = serde_json::from_str(text).map_err(|error| {
            EngineError::new(
                ErrorType::Value,
                format!("the fixture graph is not JSON: {error}"),
            )
        })?;
        let rows = |key: &str| -> Result<Vec<Value>, EngineError> {
            document
                .get(key)
                .and_then(Value::as_array)
                .cloned()
                .ok_or_else(|| {
                    EngineError::new(
                        ErrorType::Value,
                        format!("the fixture graph has no {key:?} list"),
                    )
                })
        };
        let entities = rows("entities")?
            .iter()
            .map(|row| {
                Ok(Entity {
                    id: text_field(row, "id")?,
                    name: text_field(row, "name")?,
                    kind: text_field(row, "type")?,
                    layer: text_field(row, "layer")?,
                    source_toolkit: text_field(row, "source_toolkit")?,
                    file_path: text_field(row, "file_path")?,
                    content: row
                        .get("content")
                        .and_then(Value::as_str)
                        .unwrap_or_default()
                        .to_owned(),
                })
            })
            .collect::<Result<Vec<_>, EngineError>>()?;
        let relations = rows("relations")?
            .iter()
            .map(|row| {
                Ok(Relation {
                    source: text_field(row, "source")?,
                    target: text_field(row, "target")?,
                    relation_type: text_field(row, "relation_type")?,
                })
            })
            .collect::<Result<Vec<_>, EngineError>>()?;
        let presets = document
            .get("presets")
            .and_then(Value::as_object)
            .cloned()
            .unwrap_or_default();
        Ok(Self {
            entities,
            relations,
            presets,
        })
    }

    /// `<dir>/spi/graph.json` when a directory is given, else the packaged
    /// copy.
    ///
    /// # Errors
    ///
    /// The file cannot be read, or is not a graph.
    pub fn load(fixtures_dir: Option<&Path>) -> Result<Self, EngineError> {
        match fixtures_dir {
            None => Self::parse(PACKAGED_GRAPH),
            Some(dir) => {
                let path = dir.join("spi").join("graph.json");
                let text = std::fs::read_to_string(&path).map_err(|error| {
                    EngineError::new(
                        ErrorType::FileNotFound,
                        format!(
                            "the fixture graph {} cannot be read: {error}",
                            path.display()
                        ),
                    )
                })?;
                Self::parse(&text)
            }
        }
    }

    fn lookup(&self, reference: &str) -> Option<&Entity> {
        let reference = reference.trim();
        if reference.is_empty() {
            return None;
        }
        self.entities
            .iter()
            .find(|entity| entity.id == reference)
            .or_else(|| {
                let lowered = reference.to_lowercase();
                // Python's dict: the LAST entity of a name wins.
                self.entities
                    .iter()
                    .rev()
                    .find(|entity| entity.name.to_lowercase() == lowered)
            })
    }

    fn source_of(&self, entity_id: &str) -> String {
        self.lookup(entity_id)
            .map(|entity| entity.source_toolkit.clone())
            .unwrap_or_default()
    }

    fn source_names(&self) -> Vec<String> {
        self.entities
            .iter()
            .map(|entity| entity.source_toolkit.clone())
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect()
    }

    fn matching(&self, query: &str) -> Vec<&Entity> {
        let query = query.trim().to_lowercase();
        self.entities
            .iter()
            .filter(|entity| {
                query.is_empty()
                    || format!("{} {} {}", entity.id, entity.name, entity.file_path)
                        .to_lowercase()
                        .contains(&query)
            })
            .collect()
    }

    fn count_by(&self, of: impl Fn(&Entity) -> &str) -> Map<String, Value> {
        let mut counts = Map::new();
        for entity in &self.entities {
            let key = of(entity).to_owned();
            let next = counts.get(&key).and_then(Value::as_u64).unwrap_or(0) + 1;
            counts.insert(key, json!(next));
        }
        counts
    }

    fn impacted_by(&self, entity_id: &str) -> Vec<String> {
        let mut affected = BTreeSet::new();
        let mut changed = true;
        while changed {
            changed = false;
            for relation in &self.relations {
                if relation.target != entity_id && !affected.contains(&relation.target) {
                    continue;
                }
                if affected.insert(relation.source.clone()) {
                    changed = true;
                }
            }
        }
        affected.into_iter().collect()
    }
}

/// Python's `first_truthy(*values)`: the first truthy value, else the last.
fn first_truthy<'a>(values: &[Option<&'a Value>], default: &'a Value) -> &'a Value {
    values
        .iter()
        .flatten()
        .find(|value| py_truthy(value))
        .copied()
        .unwrap_or(default)
}

fn param<'a>(params: &'a Map<String, Value>, key: &str) -> Option<&'a Value> {
    params.get(key)
}

fn first_str(params: &Map<String, Value>, keys: &[&str]) -> String {
    let empty = Value::String(String::new());
    let values: Vec<Option<&Value>> = keys.iter().map(|key| param(params, key)).collect();
    py_str(first_truthy(&values, &empty))
}

fn entity_ref(params: &Map<String, Value>) -> String {
    first_str(params, &["entity_id", "entity_name", "entity", "id"])
}

/// The legacy `output_format` switch: markdown by default, JSON on request.
fn answer(params: &Map<String, Value>, document: &Value, text: String) -> Value {
    let format = params
        .get("output_format")
        .filter(|value| py_truthy(value))
        .map(py_str)
        .unwrap_or_default();
    if format.trim().to_lowercase() == "json" {
        json!({"success": true, "result": dumps(document)})
    } else {
        json!({"success": true, "result": Value::String(text)})
    }
}

fn not_found(params: &Map<String, Value>, what: &str) -> Value {
    let reference = first_str(
        params,
        &[
            "entity_id",
            "entity_name",
            "entity",
            "id",
            "preset",
            "preset_name",
        ],
    );
    json!({
        "success": false,
        "error": format!("No {what} {} in this graph.", py_repr(&reference)),
        "error_category": "resource_not_found",
    })
}

/// The source an ingestion ran against, `{type}:{id}` — mirrors the Go
/// host's `SourceLabelFor`.
fn source_label_for(params: &Map<String, Value>) -> String {
    let empty = Map::new();
    let source = params
        .get("source")
        .and_then(Value::as_object)
        .unwrap_or(&empty);
    let kind = source
        .get("type")
        .filter(|value| py_truthy(value))
        .map(py_str)
        .unwrap_or_default()
        .trim()
        .to_owned();
    let identifier = first_str(source, &["id", "toolkit_id"]).trim().to_owned();
    match (kind.is_empty(), identifier.is_empty()) {
        (false, false) => format!("{kind}:{identifier}"),
        (false, true) => kind,
        (true, false) => identifier,
        (true, true) => "fixture-source".to_owned(),
    }
}

fn graph_document(graph: &FixtureGraph, source_label: &str) -> Value {
    let nodes: Vec<Value> = graph
        .entities
        .iter()
        .map(|entity| {
            json!({
                "id": entity.id, "name": entity.name, "type": entity.kind, "layer": entity.layer,
                "file_path": entity.file_path,
                "citations": [{"source_toolkit": entity.source_toolkit, "file_path": entity.file_path}],
            })
        })
        .collect();
    let edges: Vec<Value> = graph
        .relations
        .iter()
        .map(|relation| {
            json!({"source": relation.source, "target": relation.target, "relation_type": relation.relation_type})
        })
        .collect();
    json!({
        "nodes": nodes,
        "edges": edges,
        "_metadata": {
            "fixture": true,
            "schema_version": 1,
            "source_toolkits": graph.source_names(),
            "ingested_source": source_label,
            "node_count": graph.entities.len(),
            "edge_count": graph.relations.len(),
        },
    })
}

fn bulleted(lines: &[String]) -> String {
    lines.join("\n") + "\n"
}

fn graph_name(params: &Map<String, Value>) -> String {
    let name = first_str(params, &["graph_name", "toolkit_configuration_graph_name"]);
    let name = name.trim();
    if name.is_empty() {
        "inventory".to_owned()
    } else {
        name.to_owned()
    }
}

fn bucket(params: &Map<String, Value>) -> String {
    params
        .get("bucket")
        .filter(|value| py_truthy(value))
        .map_or_else(|| "inventory".to_owned(), py_str)
}

fn run_ingestion(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let label = source_label_for(params);
    let document = graph_document(graph, &label);
    let status = json!({"sources": [{
        "source": label, "status": "completed",
        "entity_count": graph.entities.len(), "relation_count": graph.relations.len(),
    }]});
    let checkpoint = json!({"source": label, "stage": "completed", "files_processed": 12});
    json!({
        "success": true,
        "result": format!(
            "Ingestion completed for {label}: {} entities, {} relations from 12 files.",
            graph.entities.len(),
            graph.relations.len()
        ),
        "artifacts": [
            {"name": "graph.json", "type": "application/json", "data": dumps(&document)},
            {"name": "sources_status.json", "type": "application/json", "data": dumps(&status)},
            {"name": format!(".ingestion-checkpoint-{label}.json"), "type": "application/json", "data": dumps(&checkpoint)},
        ],
    })
}

fn remove_source_entities(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    // Python stringifies whichever of the three is truthy first; a `source`
    // OBJECT there would read as its repr, which no caller sends — the
    // ingestion's own label is the answer then, as when none is given.
    let chosen = first_str(params, &["toolkit_id", "source_toolkit", "source"]);
    let label = match chosen.trim() {
        text if text.is_empty() || text.starts_with('{') => source_label_for(params),
        text => text.to_owned(),
    };
    let removed = graph
        .entities
        .iter()
        .filter(|entity| entity.source_toolkit == label)
        .count();
    answer(
        params,
        &json!({"source": label, "removed_entities": removed}),
        format!("Removed {removed} entities contributed by {label}."),
    )
}

fn list_ingested_sources(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let names = graph.source_names();
    let mut rows = Vec::new();
    let mut lines = vec![
        format!("# Ingested Sources ({})", names.len()),
        String::new(),
    ];
    for name in &names {
        let entities = graph
            .entities
            .iter()
            .filter(|entity| &entity.source_toolkit == name)
            .count();
        let relations = graph
            .relations
            .iter()
            .filter(|relation| &graph.source_of(&relation.source) == name)
            .count();
        rows.push(
            json!({"source_toolkit": name, "entity_count": entities, "relation_count": relations}),
        );
        lines.push(format!(
            "- **{name}**: {entities} entities, {relations} relations"
        ));
    }
    answer(
        params,
        &json!({"sources": rows, "total_sources": names.len()}),
        bulleted(&lines),
    )
}

fn get_sources_status(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let mut rows = Vec::new();
    let mut lines = vec!["# Sources".to_owned(), String::new()];
    for name in graph.source_names() {
        let entities = graph
            .entities
            .iter()
            .filter(|entity| entity.source_toolkit == name)
            .count();
        lines.push(format!("- **{name}**: completed, {entities} entities"));
        rows.push(json!({"source": name, "status": "completed", "entity_count": entities}));
    }
    answer(params, &json!({"sources": rows}), bulleted(&lines))
}

fn get_ingestion_status(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    answer(
        params,
        &json!({
            "running": false, "last_status": "completed",
            "entity_count": graph.entities.len(), "relation_count": graph.relations.len(),
        }),
        "No ingestion is running. The last one completed.".to_owned(),
    )
}

fn list_graphs(_graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let name = graph_name(params);
    let bucket = bucket(params);
    answer(
        params,
        &json!({"graphs": [{"name": name, "size": 4096}], "bucket": bucket}),
        format!("# Available Graphs in '{bucket}'\n\n- **{name}** (4.0 KB)\n"),
    )
}

fn load_graph(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let name = graph_name(params);
    answer(
        params,
        &json!({"graph_name": name, "node_count": graph.entities.len(), "edge_count": graph.relations.len()}),
        format!(
            "Loaded graph: {name}\nNodes: {}, Edges: {}",
            graph.entities.len(),
            graph.relations.len()
        ),
    )
}

fn get_graph_info(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let name = graph_name(params);
    let sources = graph.source_names();
    answer(
        params,
        &json!({
            "path": format!("{}/{name}/graph.json", bucket(params)),
            "node_count": graph.entities.len(), "edge_count": graph.relations.len(),
            "source_toolkits": sources,
        }),
        format!(
            "# Graph {name}\n\nNodes: {}\nEdges: {}\nSources: {}\n",
            graph.entities.len(),
            graph.relations.len(),
            sources.join(", ")
        ),
    )
}

fn search_graph(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let query = first_str(params, &["query", "search_query", "question"])
        .trim()
        .to_lowercase();
    let matches = graph.matching(&query);
    let mut lines = vec![
        format!("# Results for {} ({})", py_repr(&query), matches.len()),
        String::new(),
    ];
    for entity in &matches {
        lines.push(format!(
            "- **{}** ({}, {}) — {}",
            entity.name, entity.kind, entity.layer, entity.file_path
        ));
    }
    let rows: Vec<Value> = matches.iter().map(|entity| entity.row()).collect();
    answer(
        params,
        &json!({"query": query, "results": rows, "total": matches.len()}),
        bulleted(&lines),
    )
}

fn get_entity(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let Some(entity) = graph.lookup(&entity_ref(params)) else {
        return not_found(params, "entity");
    };
    answer(
        params,
        &entity.row(),
        format!(
            "# {}\n\n- id: {}\n- type: {}\n- layer: {}\n- source: {}\n- file: {}\n",
            entity.name,
            entity.id,
            entity.kind,
            entity.layer,
            entity.source_toolkit,
            entity.file_path
        ),
    )
}

fn get_entity_content(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let Some(entity) = graph.lookup(&entity_ref(params)) else {
        return not_found(params, "entity");
    };
    answer(
        params,
        &json!({"id": entity.id, "file_path": entity.file_path, "content": entity.content}),
        format!("```\n{}```\n", entity.content),
    )
}

fn get_entities_by_ids(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let ids: Vec<Value> = ["entity_ids", "ids"]
        .iter()
        .filter_map(|key| params.get(*key))
        .find(|value| py_truthy(value))
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default();
    let mut rows = Vec::new();
    let mut missing = Vec::new();
    for raw in &ids {
        let id = py_str(raw).trim().to_owned();
        match graph.lookup(&id) {
            Some(entity) => rows.push(entity.row()),
            None => missing.push(id),
        }
    }
    let text = format!(
        "Found {} of {} entities.",
        rows.len(),
        rows.len() + missing.len()
    );
    answer(params, &json!({"entities": rows, "missing": missing}), text)
}

fn get_related_entities(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let entity_id = entity_ref(params);
    if graph.lookup(&entity_id).is_none() {
        return not_found(params, "entity");
    }
    let mut rows = Vec::new();
    let mut lines = vec![format!("# Neighbours of {entity_id}"), String::new()];
    for relation in &graph.relations {
        if relation.source == entity_id {
            rows.push(json!({"entity_id": relation.target, "relation_type": relation.relation_type, "direction": "outgoing"}));
            lines.push(format!(
                "- {entity_id} → {} ({})",
                relation.target, relation.relation_type
            ));
        } else if relation.target == entity_id {
            rows.push(json!({"entity_id": relation.source, "relation_type": relation.relation_type, "direction": "incoming"}));
            lines.push(format!(
                "- {entity_id} ← {} ({})",
                relation.source, relation.relation_type
            ));
        }
    }
    let total = rows.len();
    answer(
        params,
        &json!({"entity_id": entity_id, "related": rows, "total": total}),
        bulleted(&lines),
    )
}

fn impact_analysis(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let entity_id = entity_ref(params);
    if graph.lookup(&entity_id).is_none() {
        return not_found(params, "entity");
    }
    let impacted = graph.impacted_by(&entity_id);
    let text = format!(
        "# Impact of {entity_id}\n\n{} entities depend on it: {}\n",
        impacted.len(),
        impacted.join(", ")
    );
    answer(
        params,
        &json!({"entity_id": entity_id, "impacted": impacted, "total": impacted.len()}),
        text,
    )
}

fn get_cross_source_relations(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let mut rows = Vec::new();
    let mut lines = vec!["# Cross-source relations".to_owned(), String::new()];
    for relation in &graph.relations {
        let from = graph.source_of(&relation.source);
        let to = graph.source_of(&relation.target);
        if from == to || from.is_empty() || to.is_empty() {
            continue;
        }
        lines.push(format!(
            "- {} ({from}) → {} ({to}) [{}]",
            relation.source, relation.target, relation.relation_type
        ));
        rows.push(json!({
            "source": relation.source, "target": relation.target, "relation_type": relation.relation_type,
            "from_source": from, "to_source": to,
        }));
    }
    let total = rows.len();
    answer(
        params,
        &json!({"relations": rows, "total": total}),
        bulleted(&lines),
    )
}

fn get_stats(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let by_type = graph.count_by(|entity| &entity.kind);
    let by_layer = graph.count_by(|entity| &entity.layer);
    let text = format!(
        "# Graph statistics\n\nNodes: {}\nEdges: {}\nTypes: {}\nLayers: {}\n",
        graph.entities.len(),
        graph.relations.len(),
        by_type.len(),
        by_layer.len()
    );
    answer(
        params,
        &json!({
            "node_count": graph.entities.len(), "edge_count": graph.relations.len(),
            "entities_by_type": by_type, "entities_by_layer": by_layer,
            "source_toolkits": graph.source_names(),
        }),
        text,
    )
}

fn filtered(
    graph: &FixtureGraph,
    params: &Map<String, Value>,
    key: &str,
    of: impl Fn(&Entity) -> &str,
) -> Value {
    let wanted = first_str(params, &[key, "type", "value"]).trim().to_owned();
    let mut rows = Vec::new();
    let mut lines = vec![
        format!("# Entities where {key} = {}", py_repr(&wanted)),
        String::new(),
    ];
    for entity in &graph.entities {
        if !wanted.is_empty() && of(entity).to_lowercase() != wanted.to_lowercase() {
            continue;
        }
        rows.push(entity.row());
        lines.push(format!(
            "- **{}** ({}) — {}",
            entity.name,
            of(entity),
            entity.file_path
        ));
    }
    let total = rows.len();
    let mut document = Map::new();
    document.insert(key.to_owned(), json!(wanted));
    document.insert("entities".to_owned(), json!(rows));
    document.insert("total".to_owned(), json!(total));
    answer(params, &Value::Object(document), bulleted(&lines))
}

fn list_entity_types(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let counts = graph.count_by(|entity| &entity.kind);
    let mut names: Vec<String> = counts.keys().cloned().collect();
    names.sort();
    let mut lines = vec!["# Entity types".to_owned(), String::new()];
    for name in &names {
        lines.push(format!("- **{name}**: {}", counts[name]));
    }
    answer(
        params,
        &json!({"types": names, "counts": counts}),
        bulleted(&lines),
    )
}

fn query_graph(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let query = first_str(params, &["query", "question"]).trim().to_owned();
    let matches = graph.matching(&query.to_lowercase());
    let rows: Vec<Value> = matches.iter().map(|entity| entity.row()).collect();
    let text = format!("# Query: {query}\n\n{} matching entities.\n", matches.len());
    answer(
        params,
        &json!({"query": query, "matches": rows, "total": matches.len()}),
        text,
    )
}

fn investigate(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let question = first_str(params, &["question", "query"]).trim().to_owned();
    let matches = graph.matching(&question.to_lowercase());
    let cited: Vec<&str> = matches.iter().map(|entity| entity.id.as_str()).collect();
    let lines: Vec<String> = matches
        .iter()
        .map(|entity| format!("- {} ({})", entity.name, entity.file_path))
        .collect();
    let mut text = format!(
        "[fixture] Answer to {}, from the canned graph:\n\n{}",
        py_repr(&question),
        lines.join("\n")
    );
    if !lines.is_empty() {
        text.push('\n');
    }
    answer(
        params,
        &json!({"question": question, "answer": text, "entities": cited}),
        text,
    )
}

fn list_presets(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let mut names: Vec<&String> = graph.presets.keys().collect();
    names.sort();
    let mut lines = vec!["# Presets".to_owned(), String::new()];
    for name in &names {
        lines.push(format!("- **{name}**: {}", py_str(&graph.presets[*name])));
    }
    answer(params, &json!({"presets": names}), bulleted(&lines))
}

fn get_preset_info(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let name = first_str(params, &["preset", "preset_name"])
        .trim()
        .to_owned();
    let Some(description) = graph.presets.get(&name) else {
        return not_found(params, "preset");
    };
    let description = py_str(description);
    answer(
        params,
        &json!({"preset": name, "description": description}),
        format!("# {name}\n\n{description}\n"),
    )
}

fn get_cache_stats(_graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    answer(
        params,
        &json!({"cached_graphs": 1, "cache_size_bytes": 4096, "hits": 0, "misses": 1}),
        "# Cache\n\nGraphs: 1\nSize: 4.0 KB\n".to_owned(),
    )
}

fn cleanup_cache(_graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    answer(
        params,
        &json!({"removed_graphs": 1, "freed_bytes": 4096}),
        "Removed 1 cached graph (4.0 KB).".to_owned(),
    )
}

fn normalize_types(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let counts = graph.count_by(|entity| &entity.kind);
    let text = format!(
        "Types are already normalised: {} distinct types.",
        counts.len()
    );
    answer(params, &json!({"normalized": 0, "types": counts}), text)
}

fn rebuild_indices(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    answer(
        params,
        &json!({"indexed_entities": graph.entities.len()}),
        format!(
            "Rebuilt the indices over {} entities.",
            graph.entities.len()
        ),
    )
}

/// `import_graph`: checks the document the host read from the bucket and
/// says plainly that nothing was stored — the fixture serves its canned
/// graph, and a claimed import would be a success with nothing behind it.
fn import_graph(_graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let name = params
        .get("artifact_name")
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|name| !name.is_empty())
        .unwrap_or("graph.json");
    let document = params
        .get("graph_document")
        .and_then(Value::as_str)
        .and_then(|text| serde_json::from_str::<Value>(text).ok())
        .filter(Value::is_object);
    let Some(document) = document else {
        return json!({
            "success": false,
            "error": "the graph document cannot be imported: it is not a JSON object",
            "error_category": "invalid_input",
        });
    };
    let count = |key: &str| document[key].as_array().map(Vec::len);
    let entities = count("nodes").unwrap_or(0);
    let relations = count("links").or_else(|| count("edges")).unwrap_or(0);
    let model = document["_metadata"]["embeddings_model"]
        .as_str()
        .filter(|model| !model.is_empty());
    answer(
        params,
        &json!({"stored": false, "entities": entities, "relations": relations, "embeddings_model": model}),
        format!(
            "Checked {name}: {entities} entities and {relations} relations. The fixture runner serves its canned graph, so nothing was imported."
        ),
    )
}

/// `export_graph`: the canned graph as `graph.json`, as the engine exports
/// the stored one.
fn export_graph(graph: &FixtureGraph, params: &Map<String, Value>) -> Value {
    let document = dumps(&graph_document(graph, &source_label_for(params)));
    // The canned graph is not stored, so it has no revision.
    let (summary, text) = crate::transfer::export_summary(
        &crate::transfer::export_bucket(params),
        graph.entities.len(),
        graph.relations.len(),
        None,
        document.len(),
    );
    let mut result = answer(params, &summary, text);
    result["artifacts"] = json!([
        {"name": crate::transfer::EXPORT_ARTIFACT, "type": "application/json", "data": document},
    ]);
    result
}

/// One canned answer.
pub type Handler = fn(&FixtureGraph, &Map<String, Value>) -> Value;

/// The canned tool table: one entry per name the descriptor advertises, the
/// same keys `FIXTURE_HANDLERS` (Python) and `fixtureHandlers()` (Go) carry,
/// aliases included.
#[must_use]
pub fn handler(tool: &str) -> Option<Handler> {
    let handler: Handler = match tool {
        "run_ingestion" => run_ingestion,
        "remove_source_entities" => remove_source_entities,
        "list_ingested_sources" => list_ingested_sources,
        "list_graphs" => list_graphs,
        "load_graph" => load_graph,
        "get_graph_info" => get_graph_info,
        "search_graph" | "search_knowledge_graph" => search_graph,
        "get_entity" | "get_entity_details" => get_entity,
        "get_entity_content" => get_entity_content,
        "get_entities_by_ids" => get_entities_by_ids,
        "get_related_entities" | "get_entity_neighbors" => get_related_entities,
        "impact_analysis" => impact_analysis,
        "get_cross_source_relations" => get_cross_source_relations,
        "get_stats" => get_stats,
        "list_entities_by_type" => {
            |graph, params| filtered(graph, params, "entity_type", |e| &e.kind)
        }
        "list_entities_by_layer" => |graph, params| filtered(graph, params, "layer", |e| &e.layer),
        "list_entities_by_source" => {
            |graph, params| filtered(graph, params, "source_toolkit", |e| &e.source_toolkit)
        }
        "list_entity_types" => list_entity_types,
        "query_graph" => query_graph,
        "investigate" => investigate,
        "list_presets" => list_presets,
        "get_preset_info" => get_preset_info,
        "get_cache_stats" => get_cache_stats,
        "cleanup_cache" => cleanup_cache,
        "get_ingestion_status" => get_ingestion_status,
        "get_sources_status" => get_sources_status,
        "normalize_types" | "smart_normalize_types" => normalize_types,
        "rebuild_indices" => rebuild_indices,
        "import_graph" => import_graph,
        "export_graph" => export_graph,
        _ => return None,
    };
    Some(handler)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_packaged_graph_parses_and_has_the_documented_shape() {
        let graph = FixtureGraph::parse(PACKAGED_GRAPH).unwrap_or_else(|e| panic!("{e}"));
        assert_eq!(graph.entities.len(), 6);
        assert_eq!(graph.relations.len(), 5);
        assert_eq!(graph.source_names(), ["code", "docs"]);
    }

    #[test]
    fn python_truthiness_picks_the_reference() {
        let params = json!({"entity_id": "", "entity_name": 0, "entity": "code:x"});
        let params = params.as_object().unwrap_or_else(|| unreachable!());
        assert_eq!(entity_ref(params), "code:x");
        assert_eq!(entity_ref(&Map::new()), "");
    }
}
