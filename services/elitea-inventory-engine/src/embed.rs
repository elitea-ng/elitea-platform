//! Entity embeddings (ADR-0027 P3e): `KnowledgeGraph.generate_embeddings`
//! with the v1 stamp (`embeddings.stamp`).
//!
//! Each entity without a vector is embedded through the gateway, from the
//! text `_compose_embedding_text` builds; the graph records the model and
//! the dimension, which retrieval later checks before comparing a query's
//! vector with the graph's.
//!
//! One difference, deliberately: when the graph is stamped with another
//! model than this run's, every entity is embedded again. The Python step
//! skipped every entity that already had a vector, so a changed model left
//! old vectors in one space and new ones in another, and the stamp named
//! only the new one.

use crate::graph::Graph;
use elitea_engine_core::errors::EngineError;
use elitea_engine_core::stream::StopSignal;
use elitea_model_client::embeddings::EmbeddingClient;
use serde_json::{Value, json};

/// The retrieval task an instruction-tuned embedding model is told a
/// query is for ([`query_text`]).
pub const QUERY_TASK: &str = "Given a question about a software repository, retrieve the code entities, rules and facts that answer it";

/// Whether `model` is an instruction-tuned embedding model: one trained
/// with the query (not the document) prefixed by
/// `Instruct: <task>\nQuery:` (Qwen3-Embedding, gte-Qwen2-instruct, the
/// e5 instruct models). Their model cards report retrieval a few points
/// worse without it.
#[must_use]
pub fn instruction_tuned(model: &str) -> bool {
    let model = model.to_lowercase();
    [
        "qwen3-embedding",
        "gte-qwen",
        "e5-mistral",
        "e5-large-instruct",
    ]
    .iter()
    .any(|family| model.contains(family))
}

/// The text a semantic search embeds for `query`: the query in the
/// instruction format its model was trained with, or the query itself.
/// Entities are embedded plain ([`compose_text`]), as those models expect
/// documents to be.
#[must_use]
pub fn query_text(model: &str, query: &str) -> String {
    if instruction_tuned(model) {
        format!("Instruct: {QUERY_TASK}\nQuery:{query}")
    } else {
        query.to_owned()
    }
}

/// `_metadata` keys of the embedding space.
pub const MODEL_KEY: &str = "embeddings_model";
pub const DIMENSION_KEY: &str = "embeddings_dimension";

/// `_compose_embedding_text`: name, type (underscores as spaces),
/// description (the node's, else its `properties`' one), then the
/// `properties`' purpose, summary, signature and docstring.
#[must_use]
pub fn compose_text(node: &serde_json::Map<String, Value>) -> String {
    let text = |value: Option<&Value>| value.and_then(Value::as_str).unwrap_or_default().to_owned();
    let mut parts: Vec<String> = Vec::new();
    let name = text(node.get("name"));
    if !name.is_empty() {
        parts.push(name);
    }
    let kind = text(node.get("type"));
    if !kind.is_empty() {
        parts.push(kind.replace('_', " "));
    }
    let properties = node.get("properties").and_then(Value::as_object);
    let mut description = text(node.get("description"));
    if description.is_empty() {
        description = text(properties.and_then(|p| p.get("description")));
    }
    if !description.is_empty() {
        parts.push(description);
    }
    if let Some(properties) = properties {
        for key in ["purpose", "summary", "signature", "docstring"] {
            let value = text(properties.get(key));
            if !value.is_empty() {
                parts.push(value);
            }
        }
    }
    elitea_engine_core::pystr::strip(&parts.join(" ")).to_owned()
}

/// An `f32` from the gateway as the decimal it was sent as (`0.1`, not
/// `0.10000000149011612`), which is what Python's JSON parse kept.
fn widen(value: f32) -> f64 {
    value
        .to_string()
        .parse()
        .unwrap_or_else(|_| f64::from(value))
}

fn has_vector(node: &serde_json::Map<String, Value>) -> bool {
    node.get("embedding")
        .and_then(Value::as_array)
        .is_some_and(|vector| !vector.is_empty())
}

/// Embed the graph's entities with `client`'s model and stamp the space.
/// Returns how many entities were embedded.
///
/// # Errors
///
/// The gateway refused or failed, or a stop was requested.
pub async fn embed_graph(
    graph: &mut Graph,
    client: &EmbeddingClient,
    generated_at: &str,
    stop: &StopSignal,
) -> Result<usize, EngineError> {
    let model = client.model().to_owned();
    let stamped = graph.metadata.get(MODEL_KEY).and_then(Value::as_str);
    let rebuild = stamped.is_some_and(|stamped| stamped != model);
    let mut ids = Vec::new();
    let mut texts = Vec::new();
    for (id, node) in graph.nodes() {
        if !rebuild && has_vector(node) {
            continue;
        }
        let text = compose_text(node);
        if text.is_empty() {
            continue;
        }
        ids.push(id.to_owned());
        texts.push(text);
    }
    if ids.is_empty() {
        return Ok(0);
    }
    let vectors = client.embed_documents(&texts, stop).await?;
    let mut dimension = None;
    for (id, vector) in ids.iter().zip(vectors) {
        dimension = Some(vector.len());
        let widened: Vec<f64> = vector.into_iter().map(widen).collect();
        graph.set_embedding(id, &widened);
    }
    graph.metadata.insert(MODEL_KEY.to_owned(), json!(model));
    if let Some(dimension) = dimension.or_else(|| client.dimension()) {
        graph
            .metadata
            .insert(DIMENSION_KEY.to_owned(), json!(dimension));
    }
    graph
        .metadata
        .insert("embeddings_generated_at".to_owned(), json!(generated_at));
    graph
        .metadata
        .insert("embeddings_count".to_owned(), json!(ids.len()));
    Ok(ids.len())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_text_is_composed_as_python_composed_it() {
        let node = json!({
            "name": "UserService", "type": "api_endpoint",
            "properties": {"description": "nested", "purpose": "users", "summary": 7, "docstring": "Doc."}
        });
        assert_eq!(
            compose_text(node.as_object().unwrap_or(&serde_json::Map::new())),
            "UserService api endpoint nested users Doc."
        );
        let flat =
            json!({"name": "f", "description": "flat wins", "properties": {"description": "no"}});
        assert_eq!(
            compose_text(flat.as_object().unwrap_or(&serde_json::Map::new())),
            "f flat wins"
        );
        assert_eq!(compose_text(&serde_json::Map::new()), "");
    }

    #[test]
    fn an_instruction_tuned_model_gets_the_query_in_its_instruction_format() {
        assert_eq!(
            query_text("vllm/Qwen/Qwen3-Embedding-4B", "signatures expired by age"),
            format!("Instruct: {QUERY_TASK}\nQuery:signatures expired by age")
        );
        assert!(instruction_tuned("intfloat/multilingual-e5-large-instruct"));
        assert!(instruction_tuned("Alibaba-NLP/gte-Qwen2-7B-instruct"));
        for plain in [
            "text-embedding-3-small",
            "nomic-embed-text",
            "all-MiniLM-L6-v2",
        ] {
            assert_eq!(query_text(plain, "q"), "q", "{plain}");
        }
    }

    #[test]
    #[allow(clippy::float_cmp)]
    fn a_gateway_float_keeps_its_decimal() {
        assert_eq!(widen(0.1), 0.1);
        assert_eq!(widen(-0.25), -0.25);
        assert_eq!(widen(1e-7), 1e-7);
    }
}
