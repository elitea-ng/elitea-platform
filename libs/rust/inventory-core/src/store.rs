//! The data a graph store keeps beside the graph, without the store: what
//! the Inventory engine's PostgreSQL store and the desktop's local index
//! both hold.

use elitea_content_source::Acl;

/// What the store keeps of one document of a source (ADR-0028): its
/// version, media type and readers.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DocumentState {
    pub version: String,
    pub mime: String,
    pub acl: Acl,
}

/// A similarity ranking: `(entity id, cosine similarity)` best first, or
/// numpy's message when an entity's vector has another width than the
/// query's (Python's `semantic_search` raised it, the wrapper printed it).
pub type Ranking = std::result::Result<Vec<(String, f64)>, String>;
