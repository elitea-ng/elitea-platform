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

// The model-free half (the composed texts and the space's keys) lives in
// the shared core (ADR-0029 decision 7); re-exported so every path in this
// crate stays the same.
pub use elitea_inventory_core::embed::{
    DIMENSION_KEY, MODEL_KEY, QUERY_TASK, compose_text, instruction_tuned, query_text,
};

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
    #[allow(clippy::float_cmp)]
    fn a_gateway_float_keeps_its_decimal() {
        assert_eq!(widen(0.1), 0.1);
        assert_eq!(widen(-0.25), -0.25);
        assert_eq!(widen(1e-7), 1e-7);
    }
}
