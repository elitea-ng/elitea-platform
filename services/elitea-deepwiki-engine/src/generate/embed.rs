//! Node embeddings for the build space (`UnifiedWikiDB.populate_embeddings`)
//! and the Phase 2 fallback embedder (`embed_query`).
//!
//! Python embedded every `repo_nodes` row whose `source_text` is not empty
//! after SQLite's `trim(…, ' \t\n\r')`, in row order, in batches of
//! `WIKI_EMBED_BATCH_SIZE`, through `OpenAIEmbeddings.embed_documents`.
//! Here the texts come from the graph in node order (the staged rows are
//! that order) and go to the gateway through [`EmbeddingClient`], whose
//! first answer fixes the dimension for the run.
//!
//! DELIBERATE DIFFERENCE: a failed request fails the run. Python logged a
//! failed batch and went on, so a wiki could publish with a fraction of its
//! vectors and no sign of it (and hybrid search silently degraded).

use crate::errors::EngineError;
use crate::graph::topology::{StoreError, TextEmbedder};
use crate::graph::{CodeGraph, node_row};
use crate::llm::EmbeddingClient;
use crate::runner::{Context, StopSignal};
use crate::storage::build::Build;
use tokio::runtime::Handle;

/// SQLite's `trim(x, ' ' || char(9) || char(10) || char(13))`.
fn sqlite_trim(text: &str) -> &str {
    text.trim_matches([' ', '\t', '\n', '\r'])
}

/// The stored `source_text` of a node, when Python would embed it.
fn embeddable_text(graph: &CodeGraph, id: &str) -> Option<String> {
    let data = graph.node(id)?;
    let text = node_row(id, data).source_text?;
    (!sqlite_trim(&text).is_empty()).then_some(text)
}

/// The nodes Python embedded, in node order. Only the ids are held; the
/// texts are read again per round, so the run never holds a second copy
/// of every text.
#[must_use]
pub fn embeddable(graph: &CodeGraph) -> Vec<&str> {
    graph
        .nodes()
        .map(|(id, _)| id)
        .filter(|id| embeddable_text(graph, id).is_some())
        .collect()
}

/// What the population did.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Embedded {
    pub stored: u64,
    pub candidates: usize,
    pub dimension: Option<usize>,
}

/// Embed every candidate node and stage its vector, `round` texts per
/// client call (the client splits a call into requests and runs them
/// concurrently). Reports progress about every tenth of the work.
///
/// # Errors
///
/// A failed request (after the client's retries) or the stop line, as
/// the client reports it, and a staging failure as a `RuntimeError`.
pub async fn populate(
    graph: &CodeGraph,
    client: &EmbeddingClient,
    build: &mut Build,
    round: usize,
    context: &Context,
) -> Result<Embedded, EngineError> {
    let stop = context.stop_signal();
    let candidates = embeddable(graph);
    let total = candidates.len();
    let mut stored = 0_u64;
    let mut reported = 0_usize;
    for chunk in candidates.chunks(round.max(1)) {
        context.checkpoint()?;
        let texts: Vec<String> = chunk
            .iter()
            .map(|id| embeddable_text(graph, id).unwrap_or_default())
            .collect();
        let vectors = client
            .embed_documents(&texts, &stop)
            .await
            .map_err(|error| {
                EngineError::new(
                    error.error_type,
                    format!(
                        "Embedding the repository with {} failed: {}",
                        client.model(),
                        error.message
                    ),
                )
            })?;
        let vectors: Vec<Vec<f64>> = vectors
            .into_iter()
            .map(|vector| vector.into_iter().map(f64::from).collect())
            .collect();
        stored += build
            .stage_embeddings(
                chunk
                    .iter()
                    .zip(&vectors)
                    .map(|(id, vector)| (*id, vector.as_slice())),
            )
            .await
            .map_err(|error| super::storage_failure("staging the node embeddings", &error))?;
        let done = usize::try_from(stored).unwrap_or(total);
        if total > 0 && done < total && done * 10 / total > reported {
            reported = done * 10 / total;
            context.thinking(format!("Embedded {done}/{total} nodes"));
        }
    }
    Ok(Embedded {
        stored,
        candidates: total,
        dimension: client.dimension(),
    })
}

/// Phase 2's `embedding_fn` (`embed_query`) over the run's client, for the
/// blocking thread Phase 2 runs on.
#[derive(Debug, Clone)]
pub struct BlockingEmbedder {
    client: EmbeddingClient,
    handle: Handle,
    stop: StopSignal,
}

impl BlockingEmbedder {
    #[must_use]
    pub fn new(client: EmbeddingClient, handle: Handle, stop: StopSignal) -> Self {
        Self {
            client,
            handle,
            stop,
        }
    }
}

impl TextEmbedder for BlockingEmbedder {
    fn embed(&mut self, text: &str) -> Result<Vec<f64>, StoreError> {
        self.handle
            .block_on(self.client.embed_query(text, &self.stop))
            .map(|vector| vector.into_iter().map(f64::from).collect())
            .map_err(StoreError::from_engine)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_sqlite_blanks_are_blank() {
        assert_eq!(sqlite_trim(" \t\r\n x \n"), "x");
        assert_eq!(sqlite_trim(" \t\r\n"), "");
        // A form feed or a no-break space is text to SQLite's trim.
        assert_eq!(sqlite_trim("\u{c}"), "\u{c}");
        assert_eq!(sqlite_trim("\u{a0}"), "\u{a0}");
    }
}
