//! `semantic_search`: entities ranked by cosine similarity between the
//! query's vector and each entity's (`KnowledgeGraph.semantic_search` and
//! the `InventoryRetrievalApiWrapper.semantic_search` text the chat agent's
//! tool returned).
//!
//! No routed tool is a semantic search (`crate::tools`): the chat agent
//! built it as a closure for `inventory_chat` / `investigate`, so [`handle`]
//! serves nothing and `investigate` calls [`semantic_search_tool`].
//!
//! THE QUERY VECTOR IS THE CALLER'S. The Python wrapper embedded the query
//! with a local `all-MiniLM-L6-v2`, while v1 graphs hold vectors from the
//! gateway model the toolkit configures — two spaces, compared as if one.
//! Here the caller embeds the query with the model the graph is stamped
//! with ([`stamped_model`], `_metadata.embeddings_model`) through the
//! gateway, and [`check_space`] (v1's `embeddings.check`) refuses a call
//! whose configured model differs from the stamp.
//!
//! Similarity is computed as numpy computed it: vectors as `float32`, the
//! dot products and norms rounded to `float32` (accumulated here in `f64`,
//! where a BLAS `sdot` accumulates in `float32` lanes — the difference is
//! below the 4-decimal rounding of the score except on a rounding
//! boundary), the score rounded to 4 decimals.
//!
//! `file_pattern` is Python's glob-to-regex (`.` literal, `*` any run, `?`
//! one character, searched anywhere in the entity's first cited file, any
//! case). Other regex metacharacters a caller might write were regex syntax
//! in Python and are literal here; the chat tool never passed a pattern.

// The answers are built line by line, as the Python handlers built them.
#![allow(clippy::format_push_string)]

use super::pattern::lexical::{description, primary_file};
use super::view::GraphView;
use super::{Call, Handled};
use crate::embed::MODEL_KEY;
use crate::graph::{layer_of, layer_types};
use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::pyjson::float_repr;
use elitea_engine_core::pyvalue::{py_repr, py_str, py_truthy};
use serde_json::{Map, Value};

/// The wrapper's `min_score` default (what the chat tool used).
pub const DEFAULT_MIN_SCORE: f64 = 0.3;

/// The chat tool's cap on `top_k`.
pub const CHAT_MAX_TOP_K: usize = 50;

/// See [`super::dispatch`]: no routed tool is a semantic search.
#[must_use]
pub fn handle(_call: &Call<'_>) -> Handled {
    None
}

/// One semantic search.
#[derive(Debug, Clone, Copy)]
pub struct SemanticQuery<'a> {
    /// The query text (for the answer's wording).
    pub query: &'a str,
    /// The query embedded with the graph's stamped model.
    pub vector: &'a [f64],
    pub top_k: usize,
    /// Only entities of this type (any case).
    pub entity_type: Option<&'a str>,
    /// Only entities of this layer, or of a type the layer holds.
    pub layer: Option<&'a str>,
    /// A glob over the entity's first cited file.
    pub file_pattern: Option<&'a str>,
    pub min_score: f64,
}

/// One ranked entity.
#[derive(Debug, Clone, PartialEq)]
pub struct SemanticHit {
    pub id: String,
    /// Cosine similarity, rounded to 4 decimals.
    pub score: f64,
}

/// The embedding model the graph's vectors were built with.
#[must_use]
pub fn stamped_model(view: &GraphView) -> Option<&str> {
    view.graph
        .metadata
        .get(MODEL_KEY)
        .and_then(Value::as_str)
        .filter(|model| !model.is_empty())
}

/// `embeddings.resolve_model`: the toolkit's configured embedding model.
#[must_use]
pub fn configured_model(params: &Map<String, Value>) -> Option<&str> {
    ["embedding_model", "toolkit_configuration_embedding_model"]
        .iter()
        .find_map(|key| {
            params
                .get(*key)
                .and_then(Value::as_str)
                .map(elitea_engine_core::pystr::strip)
                .filter(|model| !model.is_empty())
        })
}

/// `embeddings.check`: refuse a query in another space than the graph's.
/// A graph with no stamp, or a call with no configured model, passes.
///
/// # Errors
///
/// The configured model is not the one the graph is stamped with
/// (`EmbeddingsModelMismatch`, a `ValueError`).
pub fn check_space(view: &GraphView, configured: Option<&str>) -> Result<(), EngineError> {
    let Some(configured) = configured.filter(|model| !model.is_empty()) else {
        return Ok(());
    };
    match stamped_model(view) {
        Some(stamped) if stamped != configured => Err(EngineError::new(
            ErrorType::Value,
            format!(
                "this graph's entity vectors were built with embedding model {stamped}, but this toolkit is configured with {configured}. Similarity between two embedding spaces is meaningless, so semantic search is refused rather than answered wrongly. Either set embedding_model back to {stamped}, or re-ingest the sources to rebuild the graph in the new space.",
                stamped = py_repr(stamped),
                configured = py_repr(configured),
            ),
        )),
        _ => Ok(()),
    }
}

/// Whether any entity has a vector (`get_stats()['has_embeddings']`).
#[must_use]
pub fn has_embeddings(view: &GraphView) -> bool {
    view.graph
        .nodes()
        .any(|(_, node)| node.get("embedding").is_some_and(py_truthy))
}

/// Python's glob-to-regex, searched anywhere in `text`, any case.
fn glob_search(pattern: &[char], text: &[char]) -> bool {
    fn prefix(pattern: &[char], text: &[char]) -> bool {
        match pattern.split_first() {
            None => true,
            Some(('*', rest)) => (0..=text.len()).any(|skip| prefix(rest, &text[skip..])),
            Some(('?', rest)) => {
                text.first().is_some_and(|c| *c != '\n') && prefix(rest, &text[1..])
            }
            Some((wanted, rest)) => text.first() == Some(wanted) && prefix(rest, &text[1..]),
        }
    }
    (0..=text.len()).any(|start| prefix(pattern, &text[start..]))
}

fn lowered_chars(text: &str) -> Vec<char> {
    text.to_lowercase().chars().collect()
}

/// `float32` rounding of an `f64`.
#[allow(clippy::cast_possible_truncation, reason = "numpy's float32")]
fn f32_of(value: f64) -> f32 {
    value as f32
}

/// numpy's `float32` dot product.
fn dot32(a: &[f32], b: &[f32]) -> f32 {
    f32_of(
        a.iter()
            .zip(b)
            .map(|(x, y)| f64::from(*x) * f64::from(*y))
            .sum(),
    )
}

/// Python's `round(value, 4)`.
fn round4(value: f64) -> f64 {
    format!("{value:.4}").parse().unwrap_or(value)
}

fn as_vector(value: &Value) -> Vec<f32> {
    value
        .as_array()
        .map(|items| {
            items
                .iter()
                .map(|item| f32_of(item.as_f64().unwrap_or(0.0)))
                .collect()
        })
        .unwrap_or_default()
}

/// `KnowledgeGraph.semantic_search` with the query already embedded: the
/// entities at or above `min_score`, best first (ties by lowercased name),
/// at most `top_k`.
///
/// # Errors
///
/// An entity's vector has another width than the query's — numpy's
/// `ValueError` message.
pub fn semantic_search(
    view: &GraphView,
    query: &SemanticQuery<'_>,
) -> Result<Vec<SemanticHit>, String> {
    let wanted: Vec<f32> = query.vector.iter().map(|v| f32_of(*v)).collect();
    let wanted_norm = dot32(&wanted, &wanted).sqrt();
    if wanted_norm == 0.0 {
        return Ok(Vec::new());
    }
    let pattern: Option<Vec<char>> = query
        .file_pattern
        .filter(|p| !p.is_empty())
        .map(lowered_chars);
    let layer = query.layer.filter(|l| !l.is_empty()).map(str::to_lowercase);
    let layer_members = layer.as_deref().and_then(layer_types).unwrap_or(&[]);
    let entity_type = query
        .entity_type
        .filter(|t| !t.is_empty())
        .map(str::to_lowercase);
    let text = |node: &Map<String, Value>, key: &str| {
        node.get(key)
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_lowercase()
    };
    let mut hits: Vec<(SemanticHit, String)> = Vec::new();
    for (id, node) in view.graph.nodes() {
        let Some(embedding) = node.get("embedding").filter(|e| py_truthy(e)) else {
            continue;
        };
        let kind = text(node, "type");
        if entity_type.as_ref().is_some_and(|wanted| *wanted != kind) {
            continue;
        }
        if let Some(layer) = &layer
            && text(node, "layer") != *layer
            && !layer_members.contains(&kind.as_str())
        {
            continue;
        }
        if let Some(pattern) = &pattern {
            let file = primary_file(node);
            if !file.is_empty() && !glob_search(pattern, &lowered_chars(file)) {
                continue;
            }
        }
        let vector = as_vector(embedding);
        if vector.len() != wanted.len() {
            return Err(format!(
                "shapes ({},) and ({},) not aligned: {} (dim 0) != {} (dim 0)",
                wanted.len(),
                vector.len(),
                wanted.len(),
                vector.len()
            ));
        }
        let norm = dot32(&vector, &vector).sqrt();
        if norm == 0.0 {
            continue;
        }
        let score = f64::from(dot32(&wanted, &vector) / (wanted_norm * norm));
        if score < query.min_score {
            continue;
        }
        hits.push((
            SemanticHit {
                id: id.to_owned(),
                score: round4(score),
            },
            text(node, "name"),
        ));
    }
    hits.sort_by(|(a, a_name), (b, b_name)| {
        b.score.total_cmp(&a.score).then_with(|| a_name.cmp(b_name))
    });
    Ok(hits
        .into_iter()
        .take(query.top_k)
        .map(|(hit, _)| hit)
        .collect())
}

/// A citation's `:start-end` suffix (`:start` without an end).
fn line_info(citation: &Map<String, Value>) -> String {
    match citation.get("line_start").filter(|v| py_truthy(v)) {
        None => String::new(),
        Some(start) => match citation.get("line_end").filter(|v| py_truthy(v)) {
            Some(end) => format!(":{}-{}", py_str(start), py_str(end)),
            None => format!(":{}", py_str(start)),
        },
    }
}

/// The `📍` location line and the description line of a result entity, as
/// the wrapper's search answers print them (`show_empty_path`: a citation
/// with no `file_path` still prints `unknown`, as `semantic_search` did).
pub(super) fn location_and_description(node: &Map<String, Value>, show_empty_path: bool) -> String {
    let mut output = String::new();
    let mut citations: Vec<&Value> = node
        .get("citations")
        .and_then(Value::as_array)
        .map(|items| items.iter().collect())
        .unwrap_or_default();
    if citations.is_empty()
        && show_empty_path
        && let Some(citation) = node.get("citation")
    {
        citations.push(citation);
    }
    if let Some(citation) = citations.first() {
        let empty = Map::new();
        let citation = citation.as_object().unwrap_or(&empty);
        let path = if show_empty_path {
            Some(
                citation
                    .get("file_path")
                    .map_or_else(|| "unknown".to_owned(), py_str),
            )
        } else {
            citation
                .get("file_path")
                .filter(|path| py_truthy(path))
                .map(py_str)
        };
        if let Some(path) = path {
            output += &format!("    📍 `{path}{}`\n", line_info(citation));
        }
    } else if let Some(path) = node.get("file_path").filter(|v| py_truthy(v)) {
        output += &format!("    📍 `{}`\n", py_str(path));
    }
    let description = description(node);
    if !description.is_empty() {
        let short: String = description.chars().take(120).collect();
        let more = if description.chars().count() > 120 {
            "..."
        } else {
            ""
        };
        output += &format!("    {short}{more}\n");
    }
    output
}

/// The wrapper's `semantic_search` text for an embedded query.
#[must_use]
pub fn semantic_search_text(view: &GraphView, query: &SemanticQuery<'_>) -> String {
    if !has_embeddings(view) {
        return "Semantic search unavailable — no entity embeddings found.\nRe-run ingestion with an embedding model configured to enable semantic search.".to_owned();
    }
    let results = match semantic_search(view, query) {
        Ok(results) => results,
        Err(error) => return format!("Semantic search failed: {error}"),
    };
    if results.is_empty() {
        return format!(
            "No semantically similar entities found for '{}' (min_score={})",
            query.query,
            float_repr(query.min_score)
        );
    }
    let mut output = format!(
        "# Semantic Search Results for '{}'\n\nFound {} similar entities:\n\n",
        query.query,
        results.len()
    );
    let empty = Map::new();
    for (index, hit) in results.iter().enumerate() {
        let node = view.node(&hit.id).unwrap_or(&empty);
        let kind = node
            .get("type")
            .map_or_else(|| "unknown".to_owned(), py_str);
        let layer = node
            .get("layer")
            .filter(|v| py_truthy(v))
            .map(py_str)
            .or_else(|| layer_of(&kind).map(str::to_owned))
            .unwrap_or_default();
        let shown = if layer.is_empty() {
            kind
        } else {
            format!("{layer}/{kind}")
        };
        let name = node.get("name").map_or_else(|| "None".to_owned(), py_str);
        output += &format!(
            "{:2}. **{name}** ({shown}) — similarity: {:.3}\n",
            index + 1,
            hit.score
        );
        output += &location_and_description(node, true);
        output += "\n";
    }
    output
}

/// The chat agent's `semantic_search` tool: `top_k` capped at 50, the
/// wrapper's `min_score` 0.3, the type and layer filters the chat applied
/// (one each at most), and v1's embedding-space check against the
/// toolkit's `configured` model. `vector` is `query` embedded with the
/// graph's [`stamped_model`].
///
/// # Errors
///
/// [`check_space`] refused.
pub fn semantic_search_tool(
    view: &GraphView,
    query: &str,
    vector: &[f64],
    top_k: usize,
    entity_type: Option<&str>,
    layer: Option<&str>,
    configured: Option<&str>,
) -> Result<String, EngineError> {
    check_space(view, configured)?;
    Ok(semantic_search_text(
        view,
        &SemanticQuery {
            query,
            vector,
            top_k: top_k.min(CHAT_MAX_TOP_K),
            entity_type,
            layer,
            file_pattern: None,
            min_score: DEFAULT_MIN_SCORE,
        },
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::graph::Graph;
    use serde_json::json;

    fn chars(text: &str) -> Vec<char> {
        text.chars().collect()
    }

    #[test]
    fn globs_search_as_the_python_regex_did() {
        assert!(glob_search(&chars("*auth*"), &chars("src/auth.py")));
        assert!(glob_search(&chars("auth"), &chars("src/auth.py")));
        assert!(glob_search(
            &chars("user?service.py"),
            &chars("src/user_service.py")
        ));
        assert!(!glob_search(&chars("*.ts"), &chars("src/auth.py")));
        assert!(glob_search(&chars(""), &chars("")));
    }

    #[test]
    fn a_query_in_another_space_is_refused() {
        let mut graph = Graph::new();
        graph
            .metadata
            .insert(MODEL_KEY.to_owned(), json!("model-a"));
        let view = GraphView::new(graph, 1);
        assert!(check_space(&view, None).is_ok());
        assert!(check_space(&view, Some("model-a")).is_ok());
        let refused = check_space(&view, Some("model-b")).err();
        assert_eq!(
            refused.as_ref().map(|e| e.error_type),
            Some(ErrorType::Value)
        );
        assert!(refused.is_some_and(|e| e.message.starts_with(
            "this graph's entity vectors were built with embedding model 'model-a', but this toolkit is configured with 'model-b'."
        )));
        let unstamped = GraphView::default();
        assert!(check_space(&unstamped, Some("model-b")).is_ok());
    }

    #[test]
    fn the_configured_model_is_read_as_v1_read_it() {
        let params =
            json!({"embedding_model": "  ", "toolkit_configuration_embedding_model": " m "});
        assert_eq!(
            configured_model(params.as_object().unwrap_or(&Map::new())),
            Some("m")
        );
        assert_eq!(configured_model(&Map::new()), None);
    }
}
