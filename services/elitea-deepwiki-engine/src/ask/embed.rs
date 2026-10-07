//! Query embeddings and `EmbeddingsFilter`.
//!
//! The Python retriever embedded each search query (`UnifiedRetriever.
//! _embed`; a failure meant a lexical-only search), and `search_codebase` /
//! `search_docs` re-ranked their dense results with `LangChain`'s
//! `EmbeddingsFilter` (the query and every document embedded, cosine
//! similarity, the top `k`, then a strict `> 0.75` threshold; a failure
//! meant the first `k` unfiltered).

use crate::errors::EngineError;
use crate::llm::EmbeddingClient;
use crate::runner::StopSignal;

/// The similarity threshold the research tools gave `EmbeddingsFilter`.
pub const SIMILARITY_THRESHOLD: f64 = 0.75;

/// Where query vectors come from.
#[derive(Clone, Default)]
pub enum Embedder {
    /// No embedding model: lexical search only, no re-ranking.
    #[default]
    None,
    /// The invocation's embedding model.
    Client(EmbeddingClient),
    /// A deterministic function (the parity gate's stand-in embedding).
    Fixed(fn(&str) -> Vec<f64>),
}

impl std::fmt::Debug for Embedder {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::None => "Embedder::None",
            Self::Client(_) => "Embedder::Client",
            Self::Fixed(_) => "Embedder::Fixed",
        })
    }
}

fn is_stop(error: &EngineError) -> bool {
    *error == EngineError::cancelled()
}

impl Embedder {
    #[must_use]
    pub fn available(&self) -> bool {
        !matches!(self, Self::None)
    }

    /// The vectors of `texts`; `Ok(None)` when there is no model or it
    /// failed (logged), the stop line when stopped.
    ///
    /// # Errors
    ///
    /// The stop line.
    pub async fn documents(
        &self,
        texts: &[String],
        stop: &StopSignal,
    ) -> Result<Option<Vec<Vec<f64>>>, EngineError> {
        match self {
            Self::None => Ok(None),
            Self::Fixed(function) => Ok(Some(texts.iter().map(|t| function(t)).collect())),
            Self::Client(client) => match client.embed_documents(texts, stop).await {
                Ok(vectors) => Ok(Some(
                    vectors
                        .into_iter()
                        .map(|v| v.into_iter().map(f64::from).collect())
                        .collect(),
                )),
                Err(error) if is_stop(&error) => Err(error),
                Err(error) => {
                    tracing::warn!(error = %error, "embedding failed; continuing without vectors");
                    Ok(None)
                }
            },
        }
    }

    /// The vector of one query, as [`Embedder::documents`].
    ///
    /// # Errors
    ///
    /// The stop line.
    pub async fn query(
        &self,
        text: &str,
        stop: &StopSignal,
    ) -> Result<Option<Vec<f64>>, EngineError> {
        Ok(self
            .documents(&[text.to_owned()], stop)
            .await?
            .and_then(|mut v| v.pop()))
    }
}

/// `cosine_similarity` (`NumPy` path): a zero norm gives 0.
fn cosine(a: &[f64], b: &[f64]) -> f64 {
    let dot: f64 = a.iter().zip(b).map(|(x, y)| x * y).sum();
    let na = a.iter().map(|x| x * x).sum::<f64>().sqrt();
    let nb = b.iter().map(|x| x * x).sum::<f64>().sqrt();
    let value = dot / (na * nb);
    if value.is_finite() { value } else { 0.0 }
}

/// `EmbeddingsFilter(k, similarity_threshold).compress_documents`: the
/// indices of `texts` to keep, best first. `None` when there is no model
/// or it failed (the caller then keeps the first `k`).
///
/// # Errors
///
/// The stop line.
pub async fn filter(
    embedder: &Embedder,
    query: &str,
    texts: &[String],
    k: usize,
    stop: &StopSignal,
) -> Result<Option<Vec<usize>>, EngineError> {
    if !embedder.available() {
        return Ok(None);
    }
    if texts.is_empty() {
        return Ok(Some(Vec::new()));
    }
    let Some(documents) = embedder.documents(texts, stop).await? else {
        return Ok(None);
    };
    let Some(query) = embedder.query(query, stop).await? else {
        return Ok(None);
    };
    if documents.iter().any(|d| d.len() != query.len()) {
        return Ok(None);
    }
    let similarity: Vec<f64> = documents.iter().map(|d| cosine(&query, d)).collect();
    // `np.argsort(similarity)[::-1]`: ascending (stable for the small
    // pools here), reversed.
    let mut order: Vec<usize> = (0..similarity.len()).collect();
    order.sort_by(|&a, &b| similarity[a].total_cmp(&similarity[b]));
    order.reverse();
    order.truncate(k);
    Ok(Some(
        order
            .into_iter()
            .filter(|&i| similarity[i] > SIMILARITY_THRESHOLD)
            .collect(),
    ))
}

/// The parity gate's stand-in embedding (`python_phase3_dump.
/// stand_in_embedding`): the first 32 bytes of SHA-256, centred, unit norm.
#[must_use]
pub fn stand_in_embedding(text: &str) -> Vec<f64> {
    use sha2::{Digest, Sha256};
    let digest = Sha256::digest(text.as_bytes());
    let vector: Vec<f64> = digest
        .iter()
        .map(|&b| (f64::from(b) - 127.5) / 127.5)
        .collect();
    let norm = vector.iter().map(|x| x * x).sum::<f64>().sqrt();
    let norm = if norm == 0.0 { 1.0 } else { norm };
    vector.into_iter().map(|x| x / norm).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn the_filter_keeps_the_top_k_above_the_threshold() {
        fn axis(text: &str) -> Vec<f64> {
            match text {
                "q" | "same" => vec![1.0, 0.0],
                "near" => vec![0.9, 0.1],
                _ => vec![0.0, 1.0],
            }
        }
        let embedder = Embedder::Fixed(axis);
        let texts = ["far", "near", "same"].map(str::to_owned);
        let stop = StopSignal::default();
        let kept = filter(&embedder, "q", &texts, 3, &stop).await;
        assert_eq!(kept, Ok(Some(vec![2, 1])));
        let kept = filter(&embedder, "q", &texts, 1, &stop).await;
        assert_eq!(kept, Ok(Some(vec![2])));
        assert_eq!(
            filter(&Embedder::None, "q", &texts, 1, &stop).await,
            Ok(None)
        );
        assert_eq!(stand_in_embedding("x").len(), 32);
    }
}
