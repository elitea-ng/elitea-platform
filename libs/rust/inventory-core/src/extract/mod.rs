//! What ingestion needs of the model stage without a model: the Python
//! engine's prompts and type tables ([`assets`]), its two type normalisers
//! ([`types`]), and what the model stage gives one file
//! ([`ModelExtraction`]). The model calls themselves are the Inventory
//! engine's (`elitea_inventory_engine::extract`).

pub mod assets;
pub mod types;

use crate::ingest::parse::ParsedEntity;

/// What the model stage gave one file.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct ModelExtraction {
    /// File-level deduplicated entities, then facts.
    pub entities: Vec<ParsedEntity>,
    /// Chunks whose entity extraction failed after every retry.
    pub failed_chunks: usize,
}
