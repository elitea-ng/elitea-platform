//! Presets, cache and maintenance tools (the Python `tool_operations.py`),
//! and the tables the retrieval modules read from the Python source
//! (`assets/python_retrieval.json`, frozen; its generator is recorded in
//! `tests/fixtures/PROVENANCE.md`).
//!
//! What each tool answers here, and why:
//!
//! * `list_presets` — Python's text, byte for byte, from `presets.py`'s
//!   `PRESETS` (no preset has a `description`, so each line ends `: `, as in
//!   Python).
//! * `get_preset_info` — Python's text, with ONE fix: the handler read
//!   `include_patterns` / `exclude_patterns`, which no preset has (they are
//!   `whitelist` / `blacklist`), so it never showed the file patterns its
//!   descriptor promises. Here it shows them, in the handler's own format.
//!   An unknown preset is Python's `KeyError`, message included.
//! * `get_cache_stats` / `cleanup_cache` — the Python engine cached graph
//!   FILES on local disk (`GraphCacheManager`). This engine has no such
//!   cache: graphs live in PostgreSQL, and a loaded view is reused only
//!   while the stored revision is unchanged (`super::ViewCache`), so there
//!   is nothing stale to report or remove. `cleanup_cache` answers exactly
//!   what Python answered for a cache with nothing stale;
//!   `get_cache_stats` reports zero cached graphs and says why.
//! * `normalize_types` / `rebuild_indices` — maintenance WRITES in Python
//!   (`_rebuild_indices` re-normalised types and the graph was saved). Here
//!   the graph is read-only and needs neither: ingestion stores canonical
//!   types (ADR-0027 P3c) and every index is derived from the stored rows
//!   when a view is loaded. Both answer as no-op reports in Python's shape.
//!   For a graph whose types are canonical, `normalize_types` is Python's
//!   answer byte for byte (its rule-based pass changes nothing, and smart
//!   normalisation ran only above `len(CANONICAL_TYPES)` types — when it
//!   would have run, the report says it was skipped and names
//!   `smart_normalize_types`, which does it). A graph imported from a
//!   Python `graph.json` (`crate::transfer`) keeps the types the file
//!   holds; `smart_normalize_types` re-normalises every type when it
//!   applies a mapping. `rebuild_indices`
//!   fixes one Python defect: its `entity_count` / `relation_count` read
//!   stats keys that do not exist (`entity_count` instead of `node_count`)
//!   and were always 0; here they are the counts.
//! * `smart_normalize_types` — when no type qualifies, Python's "No types
//!   to normalize" answer (no model is called). Otherwise it WRITES, so the
//!   native runner serves it (`crate::native`) with the pieces here:
//!   [`smart_plan`], [`smart_prompt`] and [`mapping_tool`] (Python's prompt
//!   and the tool `LangChain`'s `with_structured_output` bound for its
//!   pydantic `TypeMappingResponse`), [`mappings_from_reply`],
//!   [`apply_mappings`] and [`smart_report`], held to what the Python
//!   handler answered and saved (`tests/fixtures/smart_normalize`).
//!   Deliberate differences, each a way Python damaged a graph: a batch the
//!   model cannot answer REFUSES the whole run with nothing written (Python
//!   mapped every type of that batch to `fact` and saved); a mapping is
//!   applied only to a type the batch asked about (Python applied whatever
//!   `original` the model named, so a model answering
//!   `{"original": "class", …}` rewrote every class); and a `batch_size`
//!   below 1 is refused (Python divided by it). Served without a native
//!   runner (this dispatch, over a read-only view), a graph that has types
//!   to map is refused.
//! * `remove_source_entities` is NOT served here ([`handle`] returns
//!   `None`): it writes, and handlers receive a read-only view. It belongs
//!   in `native.rs` with store access, with these semantics (the Python
//!   handler deleted every NODE any of whose citations named the toolkit —
//!   entities shared with another source went too, with their edges — and
//!   left `sources_status.json` and the checkpoint claiming the source):
//!   for the source `toolkit_id` names, (1) drop that source's citations
//!   from every entity, (2) drop the relations that source contributed
//!   (an edge's `source_toolkit`), (3) delete the entities that HAD
//!   citations and are left with none (an entity never cited stays), with
//!   every edge touching them — `Graph::remove_file` per file of the
//!   source, widened to every relation of the source — (4) delete its
//!   `source_files` and `sources` rows, then (5) bump the graph revision
//!   so cached views reload — one transaction. The answer keeps Python's
//!   text, `Removed {n} entities from toolkit {toolkit_id}`, `n` counting
//!   the entities deleted in (3).

// The answers are built line by line, as the Python handlers built them.
#![allow(clippy::format_push_string)]

use super::view::GraphView;
use super::{Call, Handled, answer};
use crate::graph::Graph;
use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::pyjson::dumps;
use elitea_engine_core::pyvalue::{py_repr, py_str, py_truthy};
use indexmap::IndexMap;
use serde::Deserialize;
use serde_json::{Map, Value, json};
use std::collections::HashMap;
use std::sync::OnceLock;

const ASSET: &str = include_str!("../../assets/python_retrieval.json");

/// What the retrieval modules read from the Python source.
#[derive(Debug, Deserialize)]
pub struct Tables {
    /// `presets.PRESETS`, in source order.
    pub presets: IndexMap<String, Map<String, Value>>,
    /// `KnowledgeGraph.RELATION_SYNONYMS`.
    pub relation_synonyms: HashMap<String, String>,
    /// `KnowledgeGraph.PATTERN_SYNTAX_HELP`.
    pub pattern_syntax_help: String,
    /// `KnowledgeGraph.MAX_PATTERN_HOPS`.
    pub max_pattern_hops: i64,
    /// `KnowledgeGraph.MAX_PATTERN_RESULTS`.
    pub max_pattern_results: usize,
    /// `KnowledgeGraph.MAX_CHAIN_SEGMENTS`.
    pub max_chain_segments: usize,
    /// `communities.TYPE_PRIORITY`.
    pub community_type_priority: HashMap<String, i64>,
    /// `communities.ARCHITECTURAL_MIN_PRIORITY`.
    pub architectural_min_priority: i64,
    /// `constants.CANONICAL_TYPES` (what smart normalisation maps onto).
    pub canonical_types: Vec<String>,
}

/// The parsed tables. They are compiled in, so a parse failure is a build
/// defect a test catches; the engine never runs with a broken asset.
///
/// # Panics
///
/// Never on a tested build (`the_tables_parse`).
#[must_use]
pub fn tables() -> &'static Tables {
    static PARSED: OnceLock<Tables> = OnceLock::new();
    #[allow(clippy::expect_used)]
    PARSED.get_or_init(|| serde_json::from_str(ASSET).expect("assets/python_retrieval.json parses"))
}

/// Why the cache tools have nothing to report.
pub const NO_CACHE_NOTE: &str = "The native Inventory engine keeps no graph file cache: graphs are read from PostgreSQL, and a loaded graph is reused only while its stored revision is unchanged.";

/// See [`super::dispatch`].
#[must_use]
pub fn handle(call: &Call<'_>) -> Handled {
    let params = call.params;
    let view = call.view;
    Some(match call.tool {
        "list_presets" => Ok(answer(list_presets())),
        "get_preset_info" => get_preset_info(params).map(answer),
        "get_cache_stats" => Ok(answer(cache_stats(params))),
        "cleanup_cache" => Ok(answer(cleanup_cache(params))),
        "normalize_types" => Ok(answer(normalize_types(view, params))),
        "rebuild_indices" => Ok(answer(rebuild_indices(view, params))),
        "smart_normalize_types" => smart_normalize_types(view, params).map(answer),
        _ => return None,
    })
}

/// Whether `output_format` (default `default`) asks for JSON.
fn wants_json(params: &Map<String, Value>, default: &str) -> bool {
    match params.get("output_format") {
        None => default == "json",
        Some(value) => value.as_str() == Some("json"),
    }
}

/// `list_presets`.
#[must_use]
pub fn list_presets() -> String {
    let presets = &tables().presets;
    let mut names: Vec<&String> = presets.keys().collect();
    names.sort();
    let mut output = "# Available Ingestion Presets\n\n".to_owned();
    for name in names {
        let description = presets[name]
            .get("description")
            .map_or_else(String::new, py_str);
        output += &format!("- **{name}**: {description}\n");
    }
    output
}

/// A preset's patterns under `key`, as text lines.
fn patterns(preset: &Map<String, Value>, key: &str) -> Vec<String> {
    preset
        .get(key)
        .and_then(Value::as_array)
        .map(|items| items.iter().map(py_str).collect())
        .unwrap_or_default()
}

/// `get_preset_info` (`get_preset` raises `KeyError` for an unknown name).
///
/// # Errors
///
/// The preset does not exist.
pub fn get_preset_info(params: &Map<String, Value>) -> Result<String, EngineError> {
    let name = params.get("preset_name").map_or_else(String::new, py_str);
    let presets = &tables().presets;
    let Some(preset) = presets.get(&name) else {
        let mut available: Vec<&str> = presets.keys().map(String::as_str).collect();
        available.sort_unstable();
        let message = format!(
            "Unknown preset '{name}'. Available presets: {}",
            available.join(", ")
        );
        return Err(EngineError::new(ErrorType::Key, py_repr(&message)));
    };
    let description = preset
        .get("description")
        .map_or_else(|| "N/A".to_owned(), py_str);
    let mut output = format!("# Preset: {name}\n\n**Description:** {description}\n\n");
    let include = patterns(preset, "whitelist");
    if !include.is_empty() {
        output += "**Include Patterns:**\n";
        for pattern in &include {
            output += &format!("- `{pattern}`\n");
        }
    }
    let exclude = patterns(preset, "blacklist");
    if !exclude.is_empty() {
        output += "\n**Exclude Patterns:**\n";
        for pattern in &exclude {
            output += &format!("- `{pattern}`\n");
        }
    }
    Ok(output)
}

/// `get_cache_stats`: no graph file cache (see the module docs).
#[must_use]
pub fn cache_stats(params: &Map<String, Value>) -> String {
    if wants_json(params, "text") {
        return dumps(&json!({
            "stats": {"total_graphs": 0, "total_size_bytes": 0, "total_size_mb": 0.0},
            "graphs": [],
            "message": NO_CACHE_NOTE,
        }));
    }
    format!(
        "# Graph Cache Statistics\n\n**Total Graphs:** 0\n**Total Size:** 0.0 MB\n\n{NO_CACHE_NOTE}\n"
    )
}

/// `cleanup_cache`: Python's answer for a cache with nothing stale.
#[must_use]
pub fn cleanup_cache(params: &Map<String, Value>) -> String {
    if wants_json(params, "text") {
        return dumps(&json!({"removed": 0, "freed_bytes": 0}));
    }
    "No stale graphs found. Cache is within limits.".to_owned()
}

/// `get_stats()['entity_types']`: each node's `type`, counted in first-seen
/// order.
fn entity_types(view: &GraphView) -> IndexMap<String, usize> {
    graph_entity_types(&view.graph)
}

/// `get_stats()['entity_types']` of a graph: each node's `type`, counted
/// in first-seen order.
#[must_use]
pub fn graph_entity_types(graph: &Graph) -> IndexMap<String, usize> {
    let mut counts = IndexMap::new();
    for (_, node) in graph.nodes() {
        if let Some(kind) = node.get("type") {
            *counts.entry(py_str(kind)).or_insert(0) += 1;
        }
    }
    counts
}

/// `normalize_types`: a no-op report (see the module docs).
#[must_use]
pub fn normalize_types(view: &GraphView, params: &Map<String, Value>) -> String {
    let types = entity_types(view);
    let count = types.len();
    let smart = params.get("smart").cloned().unwrap_or(Value::Bool(true));
    #[allow(clippy::cast_precision_loss, reason = "a type count")]
    let threshold = params
        .get("smart_threshold")
        .and_then(Value::as_f64)
        .unwrap_or(tables().canonical_types.len() as f64);
    #[allow(clippy::cast_precision_loss, reason = "a type count")]
    let would_run = py_truthy(&smart) && count as f64 > threshold;
    // `run_smart and …`: Python's `and` answers the falsy operand itself.
    let ran = if py_truthy(&smart) {
        Value::Bool(false)
    } else {
        smart
    };
    let message = format!(
        "Normalized entity types: {count} -> {count} unique types (rule-based: {count}, smart: {count})"
    );
    let skipped = "smart normalization maps types through a model and rewrites the stored graph, which normalize_types does not do here; run smart_normalize_types for it";
    if wants_json(params, "json") {
        let mut result = json!({
            "success": true,
            "types_before": count,
            "types_after_rules": count,
            "types_after": count,
            "types_reduced": 0,
            "rule_based_reduction": 0,
            "smart_reduction": 0,
            "smart_normalization_ran": ran,
            "entity_types": types,
            "message": message,
        });
        if would_run && let Some(fields) = result.as_object_mut() {
            fields.insert("smart_normalization_skipped".to_owned(), json!(skipped));
        }
        return dumps(&result);
    }
    let mut text = format!(
        "Type normalization complete:\n- Types before: {count}\n- After rule-based: {count}\n- After smart: {count}\n- Total reduced: 0"
    );
    if would_run {
        text += &format!("\n- Smart normalization skipped: {skipped}");
    }
    text
}

/// `rebuild_indices`: the derived indices, reported (see the module docs).
#[must_use]
pub fn rebuild_indices(view: &GraphView, params: &Map<String, Value>) -> String {
    let indices = view.graph.indices();
    let entities = view.graph.node_count();
    let relations = view.graph.edge_count();
    let types = entity_types(view).len();
    let files = indices.file_index.len();
    if wants_json(params, "json") {
        return dumps(&json!({
            "success": true,
            "entity_count": entities,
            "relation_count": relations,
            "unique_types": types,
            "unique_names": indices.entity_index.len(),
            "indexed_files": files,
            "message": "Indices rebuilt successfully",
        }));
    }
    format!(
        "Indices rebuilt:\n- Entities: {entities}\n- Relations: {relations}\n- Types: {types}\n- Files: {files}"
    )
}

/// Python's `int(value)` of a parameter, or its `ValueError`.
fn py_int_param(value: &Value) -> Result<i128, EngineError> {
    let refuse = |text: &str| {
        EngineError::new(
            ErrorType::Value,
            format!("invalid literal for int() with base 10: {}", py_repr(text)),
        )
    };
    match value {
        Value::Bool(flag) => Ok(i128::from(*flag)),
        Value::Number(number) => number
            .as_i64()
            .map(i128::from)
            .or_else(|| {
                #[allow(clippy::cast_possible_truncation, reason = "int() truncates")]
                number.as_f64().map(|n| n.trunc() as i128)
            })
            .ok_or_else(|| refuse(&number.to_string())),
        Value::String(text) => {
            let trimmed = elitea_engine_core::pystr::strip(text);
            let (sign, digits) = match trimmed.strip_prefix('-') {
                Some(rest) => (-1, rest),
                None => (1, trimmed.strip_prefix('+').unwrap_or(trimmed)),
            };
            let valid = !digits.is_empty()
                && !digits.starts_with('_')
                && !digits.ends_with('_')
                && !digits.contains("__")
                && digits.chars().all(|c| c.is_ascii_digit() || c == '_');
            if !valid {
                return Err(refuse(text));
            }
            digits
                .replace('_', "")
                .parse::<i128>()
                .map(|n| sign * n)
                .map_err(|_| refuse(text))
        }
        other => Err(EngineError::new(
            ErrorType::Type,
            format!(
                "int() argument must be a string, a bytes-like object or a real number, not '{}'",
                if other.is_null() {
                    "NoneType"
                } else if other.is_array() {
                    "list"
                } else {
                    "dict"
                }
            ),
        )),
    }
}

/// A smart normalisation to run: its parameters and the types to map.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SmartPlan {
    /// Types per batch (`batch_size`, default 100).
    pub batch_size: usize,
    /// `dry_run`: report the mapping, change nothing.
    pub dry_run: bool,
    /// Every type of the graph and its entity count, in first-seen order.
    pub types: IndexMap<String, usize>,
    /// The types to map: fewer than `threshold` entities and not canonical.
    pub candidates: IndexMap<String, usize>,
    /// Whether the answer is JSON (`output_format`, default `json`).
    pub json: bool,
}

impl SmartPlan {
    /// The candidate types, batch by batch.
    pub fn batches(&self) -> impl Iterator<Item = Vec<String>> + '_ {
        let names: Vec<String> = self.candidates.keys().cloned().collect();
        let size = self.batch_size;
        (0..names.len().div_ceil(size))
            .map(move |index| names[index * size..((index + 1) * size).min(names.len())].to_vec())
    }
}

/// What `smart_normalize_types` does for a graph.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SmartStep {
    /// Nothing qualifies: Python's answer, no model is called.
    Answer(String),
    /// Types to map through the model.
    Map(SmartPlan),
}

/// `smart_normalize_types` up to the model call: the parameters as Python
/// read them, and either its "No types to normalize" answer or the plan.
///
/// # Errors
///
/// `threshold` or `batch_size` is not an integer (Python's `int()`
/// errors), or `batch_size` is below 1.
pub fn smart_plan(graph: &Graph, params: &Map<String, Value>) -> Result<SmartStep, EngineError> {
    let threshold = py_int_param(params.get("threshold").unwrap_or(&json!(1000)))?;
    let batch_size = py_int_param(params.get("batch_size").unwrap_or(&json!(100)))?;
    let batch_size = usize::try_from(batch_size)
        .ok()
        .filter(|size| *size >= 1)
        .ok_or_else(|| {
            EngineError::new(
                ErrorType::Value,
                format!("batch_size must be a positive integer, got {batch_size}"),
            )
        })?;
    let dry_run = super::flag(params.get("dry_run"));
    let canonical = &tables().canonical_types;
    let types = graph_entity_types(graph);
    let candidates: IndexMap<String, usize> = types
        .iter()
        .filter(|(kind, count)| {
            i128::try_from(**count).is_ok_and(|count| count < threshold)
                && !canonical.contains(kind)
        })
        .map(|(kind, count)| (kind.clone(), *count))
        .collect();
    let json_answer = wants_json(params, "json");
    if candidates.is_empty() {
        let message = format!(
            "No types to normalize (all types either have count >= {threshold} or are already canonical)"
        );
        if json_answer {
            return Ok(SmartStep::Answer(dumps(&json!({
                "success": true,
                "message": message,
                "types_checked": types.len(),
                "canonical_types": canonical.len(),
            }))));
        }
        return Ok(SmartStep::Answer(message));
    }
    Ok(SmartStep::Map(SmartPlan {
        batch_size,
        dry_run,
        types,
        candidates,
        json: json_answer,
    }))
}

/// The name of the structured-output tool the model is made to call.
pub const MAPPING_TOOL: &str = "TypeMappingResponse";

/// The tool `LangChain`'s `with_structured_output(TypeMappingResponse)`
/// binds (`convert_to_openai_tool` of the handler's pydantic model):
/// `(name, description, JSON schema)`.
#[must_use]
pub fn mapping_tool() -> (&'static str, &'static str, Value) {
    (
        MAPPING_TOOL,
        "Response containing all type mappings.",
        json!({
            "properties": {
                "mappings": {
                    "description": "List of type mappings",
                    "items": {
                        "description": "Mapping of original type to canonical type.",
                        "properties": {
                            "original": {"description": "The original entity type name", "type": "string"},
                            "canonical": {"description": "The canonical type to map to", "type": "string"},
                            "confidence": {"description": "Confidence score 0-1", "maximum": 1, "minimum": 0, "type": "number"},
                        },
                        "required": ["original", "canonical", "confidence"],
                        "type": "object",
                    },
                    "type": "array",
                },
            },
            "required": ["mappings"],
            "type": "object",
        }),
    )
}

/// The Python handler's prompt for one batch of types.
#[must_use]
pub fn smart_prompt(batch: &[String]) -> String {
    let mut canonical: Vec<&str> = tables()
        .canonical_types
        .iter()
        .map(String::as_str)
        .collect();
    canonical.sort_unstable();
    let types = elitea_engine_core::pyjson::dumps_with(&json!(batch), Some(2), true);
    format!(
        r#"You are a knowledge graph type normalizer. Map each entity type to the most appropriate canonical type.

CANONICAL TYPES (you MUST map to one of these):
{}

RULES:
1. Map each type to the SINGLE most appropriate canonical type from the list above
2. Consider semantic meaning, not just string similarity
3. Types ending in _rule, _policy, _constraint → "rule"
4. Types ending in _requirement → "requirement"
5. Types ending in _step, _procedure → "process" or "step"
6. Types ending in _example, _sample → "example"
7. Types ending in _guide, _guideline, _note, _documentation → "documentation"
8. Types ending in _parameter, _field, _attribute → "parameter"
9. Types ending in _feature, _capability → "feature"
10. Types ending in _config, _setting, _option → "configuration"
11. Types about facts, behaviors, details, info → "fact"
12. Types about UI, interaction, presentation → "component" or "feature"
13. Unknown or unclear types → "fact" (safest default)

TYPES TO NORMALIZE:
{types}

Map each type to exactly one canonical type. Be aggressive in consolidation - we want fewer unique types."#,
        canonical.join(", ")
    )
}

/// One batch's reply (the tool call's arguments) as `(original, canonical)`
/// pairs, validated as pydantic validated `TypeMappingResponse`.
///
/// # Errors
///
/// The reply is not a `TypeMappingResponse`: the reason.
pub fn mappings_from_reply(reply: &Value) -> Result<Vec<(String, String)>, String> {
    let mappings = reply
        .get("mappings")
        .and_then(Value::as_array)
        .ok_or("it has no `mappings` list")?;
    mappings
        .iter()
        .enumerate()
        .map(|(index, mapping)| {
            let text = |key: &str| {
                mapping
                    .get(key)
                    .and_then(Value::as_str)
                    .map(str::to_owned)
                    .ok_or_else(|| format!("mapping {index} has no string `{key}`"))
            };
            let confidence = mapping.get("confidence").and_then(Value::as_f64);
            if !confidence.is_some_and(|c| (0.0..=1.0).contains(&c)) {
                return Err(format!(
                    "mapping {index} has no `confidence` between 0 and 1"
                ));
            }
            Ok((text("original")?, text("canonical")?))
        })
        .collect()
}

/// Add one batch's mappings to `all`: a canonical answer as given, anything
/// else `fact` (Python's rule); a type the batch did not ask about is
/// ignored (see the module docs).
pub fn merge_batch(
    all: &mut IndexMap<String, String>,
    batch: &[String],
    pairs: Vec<(String, String)>,
) {
    let canonical = &tables().canonical_types;
    for (original, target) in pairs {
        if !batch.contains(&original) {
            continue;
        }
        let target = if canonical.contains(&target) {
            target
        } else {
            "fact".to_owned()
        };
        all.insert(original, target);
    }
}

/// Map the graph's types (`node['type'] = mapping`), then re-normalise
/// every type as Python's `_rebuild_indices` did. Returns how many
/// entities were mapped.
pub fn apply_mappings(graph: &mut Graph, mappings: &IndexMap<String, String>) -> usize {
    let mut mapped = 0;
    for (_, node) in graph.nodes_mut() {
        let Some(Value::String(kind)) = node.get("type") else {
            continue;
        };
        if let Some(target) = mappings.get(kind) {
            node.insert("type".to_owned(), json!(target));
            mapped += 1;
        }
        if let Some(Value::String(kind)) = node.get("type")
            && !kind.is_empty()
        {
            let normalized = crate::extract::types::normalize_graph(kind);
            if normalized != *kind {
                node.insert("type".to_owned(), json!(normalized));
            }
        }
    }
    mapped
}

/// What an applied run changed.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Applied {
    /// Distinct types after the run.
    pub types_after: usize,
    /// Entities whose type was mapped.
    pub entities_normalized: usize,
}

/// The handler's answer: the dry-run preview (`applied` is `None`) or the
/// applied run's report.
#[must_use]
pub fn smart_report(
    plan: &SmartPlan,
    mappings: &IndexMap<String, String>,
    applied: Option<Applied>,
    llm_model: &str,
) -> String {
    let Some(applied) = applied else {
        let affected: usize = mappings
            .keys()
            .map(|kind| plan.candidates.get(kind).copied().unwrap_or(0))
            .sum();
        let message = format!(
            "DRY RUN: Would normalize {} types affecting {affected} entities",
            mappings.len()
        );
        if !plan.json {
            return message;
        }
        let mut distribution: IndexMap<&str, usize> = IndexMap::new();
        for (original, target) in mappings {
            *distribution.entry(target.as_str()).or_insert(0) +=
                plan.candidates.get(original).copied().unwrap_or(0);
        }
        let targets: std::collections::HashSet<&String> = mappings.values().collect();
        let preview: IndexMap<&String, &String> = mappings.iter().take(50).collect();
        return dumps(&json!({
            "success": true,
            "dry_run": true,
            "types_to_normalize": mappings.len(),
            "entities_affected": affected,
            "target_types": targets.len(),
            "mapping_preview": preview,
            "target_type_distribution": distribution,
            "message": message,
        }));
    };
    let before = plan.types.len();
    let message = format!(
        "Smart normalization complete: {before} → {} types ({} entities updated)",
        applied.types_after, applied.entities_normalized
    );
    if !plan.json {
        return message;
    }
    #[allow(clippy::cast_possible_wrap, reason = "type counts")]
    let reduced = before as i64 - applied.types_after as i64;
    dumps(&json!({
        "success": true,
        "dry_run": false,
        "types_before": before,
        "types_after": applied.types_after,
        "types_reduced": reduced,
        "entities_normalized": applied.entities_normalized,
        "mappings_applied": mappings.len(),
        "llm_model": llm_model,
        "message": message,
    }))
}

/// `smart_normalize_types` over a read-only view: Python's answer when no
/// type qualifies, otherwise refused (the native runner serves the rest).
///
/// # Errors
///
/// A parameter is not an integer, or some type would need a model to map.
pub fn smart_normalize_types(
    view: &GraphView,
    params: &Map<String, Value>,
) -> Result<String, EngineError> {
    match smart_plan(&view.graph, params)? {
        SmartStep::Answer(text) => Ok(text),
        SmartStep::Map(plan) => {
            let names: Vec<&str> = plan.candidates.keys().map(String::as_str).collect();
            Err(EngineError::new(
                ErrorType::Runtime,
                format!(
                    "smart_normalize_types would map {} entity type(s) through a model ({}), which only the native runner (ELITEA_INVENTORY_RUNNER=native) does",
                    names.len(),
                    names.join(", ")
                ),
            ))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_tables_parse() {
        let tables = tables();
        assert!(tables.presets.contains_key("python"));
        assert!(tables.pattern_syntax_help.starts_with("Pattern syntax:"));
        assert_eq!(
            tables.relation_synonyms.get("inherits").map(String::as_str),
            Some("extends")
        );
        assert_eq!(tables.max_pattern_hops, 5);
        assert_eq!(tables.community_type_priority.get("class"), Some(&10));
        assert!(!tables.canonical_types.is_empty());
    }

    #[test]
    fn int_parameters_read_as_python_int_reads_them() {
        assert_eq!(py_int_param(&json!(" 7 ")).ok(), Some(7));
        assert_eq!(py_int_param(&json!(2.9)).ok(), Some(2));
        assert_eq!(py_int_param(&json!(true)).ok(), Some(1));
        let error = py_int_param(&json!("x")).err();
        assert_eq!(
            error.map(|e| e.message),
            Some("invalid literal for int() with base 10: 'x'".to_owned())
        );
    }
}
