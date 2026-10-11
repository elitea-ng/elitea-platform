//! A search result, as the SDK's tools see it.

use serde_json::{Map, Value};

use crate::pb;
use crate::pyfmt;

/// One chunk found by a search.
#[derive(Clone, Debug, PartialEq)]
pub struct Hit {
    /// `{document_key}_{chunk_id}`: what duplicates are detected by.
    pub key: String,
    /// The chunk text.
    pub page_content: String,
    /// The chunk's metadata: the point's `metadata` object, plus its
    /// `chunk_id` and `chunk_type` when the object does not hold them (the
    /// SDK kept both in the metadata dict).
    pub metadata: Map<String, Value>,
    /// Cosine similarity, or the fused score of a hybrid search; reranking
    /// changes it.
    pub score: f64,
    /// The point's `document_key`.
    pub document_key: String,
    /// The point's `chunk_id`.
    pub chunk_id: String,
}

impl Hit {
    /// Reads a scored point. A `metadata_json` that is not an object is
    /// treated as empty (and logged): the facade only stores objects.
    #[must_use]
    pub fn from_point(point: &pb::ScoredPoint) -> Self {
        let mut metadata =
            if let Ok(Value::Object(map)) = serde_json::from_str::<Value>(&point.metadata_json) {
                map
            } else {
                if !point.metadata_json.is_empty() {
                    tracing::warn!(
                        point = point.id,
                        "a point's metadata_json is not a JSON object; treating it as empty"
                    );
                }
                Map::new()
            };
        if !point.chunk_id.is_empty() && !metadata.contains_key("chunk_id") {
            metadata.insert("chunk_id".to_owned(), Value::String(point.chunk_id.clone()));
        }
        if !point.chunk_type.is_empty() && !metadata.contains_key("chunk_type") {
            metadata.insert(
                "chunk_type".to_owned(),
                Value::String(point.chunk_type.clone()),
            );
        }
        Self {
            key: format!("{}_{}", point.document_key, point.chunk_id),
            page_content: point.text.clone(),
            metadata,
            score: f64::from(point.score),
            document_key: point.document_key.clone(),
            chunk_id: point.chunk_id.clone(),
        }
    }

    /// The SDK's `{page_content, metadata, score}` dict.
    #[must_use]
    pub fn to_value(&self) -> Value {
        let mut map = Map::new();
        map.insert(
            "page_content".to_owned(),
            Value::String(self.page_content.clone()),
        );
        map.insert("metadata".to_owned(), Value::Object(self.metadata.clone()));
        map.insert("score".to_owned(), score_value(self.score));
        Value::Object(map)
    }
}

/// A score as a JSON number (`null` for NaN or infinity, which a cosine
/// similarity is not).
#[must_use]
pub fn score_value(score: f64) -> Value {
    serde_json::Number::from_f64(score).map_or(Value::Null, Value::Number)
}

/// `str(score)` as Python prints it.
#[must_use]
pub fn score_text(score: f64) -> String {
    pyfmt::float_repr(score)
}
