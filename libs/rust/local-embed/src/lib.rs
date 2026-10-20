//! The desktop's local embeddings (ADR-0029 decision 7, "embeddings").
//!
//! The local index is built WITHOUT any embedding model by default: the
//! lexical index (FTS5) and the code graph answer on their own. Vectors are
//! an option the person turns on per workspace, either with a model run on
//! this computer (chosen by the person from [`catalog`], downloaded only
//! once they choose it) or, when the organisation's policy allows it, with
//! the platform's `/llm/v1/embeddings` route.
//!
//! * [`Embedder`] — what the index embeds chunks and queries with;
//! * [`catalog`] — the models the person can choose, pinned by SHA-256;
//! * [`vector`] — normalisation, Matryoshka truncation and the int8 blobs
//!   the index stores;
//! * [`manager`] — the owner-only model cache shared by every workspace,
//!   manual import, and (feature `download`) resumable verified downloads;
//! * `candle_backend` (feature `candle`; Metal on macOS) — the
//!   catalogue's models on this computer.
#![cfg_attr(
    not(test),
    deny(
        clippy::expect_used,
        clippy::panic,
        clippy::todo,
        clippy::unimplemented,
        clippy::unwrap_used
    )
)]

pub mod catalog;
pub mod fsx;
pub mod manager;
pub mod vector;

#[cfg(feature = "candle")]
pub mod bert;
#[cfg(feature = "candle")]
pub mod candle_backend;
#[cfg(feature = "candle")]
pub mod qwen3;

/// Why embedding failed.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum EmbedError {
    /// The model's files are missing, damaged or do not load.
    #[error("the embedding model could not be loaded: {0}")]
    Load(String),
    /// The model ran and failed.
    #[error("the embedding model failed: {0}")]
    Run(String),
    /// A remote embedder was refused or failed (status, transport).
    #[error("the embeddings service failed: {0}")]
    Remote(String),
    /// The answer does not fit (count or dimension).
    #[error("the embeddings answer is unusable: {0}")]
    Shape(String),
    /// Stopped (a turn started, the index closed).
    #[error("embedding was paused")]
    Paused,
}

/// What the index embeds with. Blocking: call it from a blocking thread.
///
/// Vectors come back L2-normalised and already truncated to
/// [`Embedder::dimension`].
pub trait Embedder: Send + Sync {
    /// The stamp every vector it makes carries: `local:<model id>` or
    /// `remote:<model name>`. Vectors with another stamp are never compared
    /// with its queries.
    fn stamp(&self) -> String;

    /// The width of its vectors.
    fn dimension(&self) -> usize;

    /// Embed document chunks, in order.
    ///
    /// # Errors
    ///
    /// The model failed or the answer does not fit.
    fn embed_documents(&self, texts: &[&str]) -> Result<Vec<Vec<f32>>, EmbedError>;

    /// Embed a search query (with the model's query instruction, if any).
    ///
    /// # Errors
    ///
    /// As [`Embedder::embed_documents`].
    fn embed_query(&self, text: &str) -> Result<Vec<f32>, EmbedError>;

    /// [`Embedder::embed_query`] on behalf of an agent turn
    /// (`execution_id`): a remote embedder attributes the call to it; a
    /// local one ignores it.
    ///
    /// # Errors
    ///
    /// As [`Embedder::embed_documents`].
    fn embed_query_for(
        &self,
        text: &str,
        execution_id: Option<&str>,
    ) -> Result<Vec<f32>, EmbedError> {
        let _ = execution_id;
        self.embed_query(text)
    }
}
