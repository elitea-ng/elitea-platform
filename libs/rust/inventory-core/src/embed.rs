//! The model-free half of entity embeddings: the text an entity is
//! embedded from ([`compose_text`]), the text a query is embedded from
//! ([`query_text`]), and the `_metadata` keys that name a graph's embedding
//! space. Calling an embedding model is the caller's: the Inventory engine's
//! gateway client (`elitea_inventory_engine::embed`), or the desktop's.

use serde_json::Value;

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

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

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
}
