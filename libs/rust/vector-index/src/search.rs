//! The SDK's `search_documents` on top of `elitea-vector` (ADR-0030
//! decision 5).
//!
//! * **Over-fetch.** A plain search asks for `k = min(30, 3 × search_top)`
//!   results by cosine similarity, reranks them, applies the cut-off and
//!   keeps `search_top`.
//! * **`extended_search`.** Searches `chunk_type = document` and each listed
//!   type (`title`, `summary`, `propositions`, `keywords`), `search_top`
//!   results each, concurrently; the results are merged in the listed
//!   order, so the answer does not depend on which search finished first. A
//!   hit on a non-document chunk brings in its document chunk (same document
//!   and chunk id), scored by the query; the non-document chunk itself is
//!   not returned. The document chunks of all new hits are read together,
//!   up to [`DOCUMENT_BATCH`] (document, chunk) pairs per call (fewer when
//!   the caller's filter leaves less room under the service's condition
//!   limits), not one call per hit. The conditions the search adds are
//!   checked against those limits on the filter actually sent.
//! * **Duplicates.** Results are keyed `{document_key}_{chunk_id}`. When a
//!   key repeats, the **better** score is kept (the SDK kept the worse).
//! * **Rerank, cut-off, truncate.** See [`crate::rerank`]. The cut-off is
//!   on the absolute reranked **dense** score ([`Hit::dense_score`]), after
//!   reranking; 0 turns it off. No score threshold is ever sent to the
//!   service, so one rule decides in both modes.
//! * **`full_text_search`.** Calls `HybridSearch`: BM25 over the chunk text
//!   fused with the dense search by weighted RRF, 0.3 text / 0.7 dense (the
//!   SDK's `weight` is the text weight). The SDK's version never ran.
//!   Results are ordered by the fused score (rank-based, far below any
//!   cosine), but cut by their dense cosine score, which the service returns
//!   beside it, exactly as a dense search is cut.

use std::collections::HashSet;
use std::future::Future;

use elitea_engine_core::pyvalue::py_truthy;
use futures_util::future::try_join_all;
use serde_json::Value;

use crate::client::{
    ClaimToken, HybridSearchParams, MAX_LIMIT, SearchParams, VectorClient, toolkit_namespace,
};
use crate::error::{Error, Result};
use crate::filter::{
    FilterError, MAX_CONDITIONS, MAX_TOTAL_CONDITIONS, check_limits, condition_count, keyword,
    nest, with_must,
};
use crate::hit::Hit;
use crate::pb;
use crate::rerank::{self, FieldRerank};

/// The SDK's ceiling on the over-fetch.
const OVERFETCH_CEILING: usize = 30;
/// The chunk types `extended_search` understands besides `document`.
const EXTENDED_CHUNK_TYPES: [&str; 4] = ["title", "summary", "propositions", "keywords"];
/// The SDK's default text weight of a full-text search.
const DEFAULT_TEXT_WEIGHT: f64 = 0.3;
/// The most (document, chunk) pairs one document-chunk read of
/// `extended_search` asks for. A caller's filter that leaves less room under
/// the service's condition limits lowers it (`batch_size`).
pub const DOCUMENT_BATCH: usize = 32;
/// The results one document-chunk read asks for, per pair and per searched
/// namespace: a pair can match one point per namespace, and the
/// active-generation and the unversioned copy of a chunk both pass the
/// scope, so a read asks for twice that and a pair's duplicates cannot push
/// another pair out.
const PAIR_HEADROOM: usize = 2;

/// What the search needs from `elitea-vector`. [`VectorSession`] is the real
/// one; tests use fakes.
pub trait VectorBackend: Send + Sync {
    /// Dense cosine search.
    fn search(
        &self,
        params: SearchParams,
    ) -> impl Future<Output = Result<Vec<pb::ScoredPoint>>> + Send;
    /// Dense plus BM25 search.
    fn hybrid_search(
        &self,
        params: HybridSearchParams,
    ) -> impl Future<Output = Result<Vec<pb::ScoredPoint>>> + Send;
    /// Removes every point of one toolkit-index namespace, in every
    /// embedding space: an index re-embedded with another model leaves
    /// points in the old model's collection, and they go too.
    fn delete_namespace(&self, namespace_id: &str) -> impl Future<Output = Result<()>> + Send;
}

/// Embeds a search query with the model of an index's embedding space.
pub trait QueryEmbedder: Send + Sync {
    /// The query's embedding. Its length must be `space.dimension`.
    fn embed_query(
        &self,
        text: &str,
        space: &pb::EmbeddingSpace,
    ) -> impl Future<Output = Result<Vec<f32>>> + Send;
}

/// A [`VectorClient`] bound to the claim's token.
#[derive(Clone, Copy)]
pub struct VectorSession<'a> {
    pub(crate) client: &'a VectorClient,
    pub(crate) token: &'a dyn ClaimToken,
}

impl<'a> VectorSession<'a> {
    /// Binds `client` to `token`.
    #[must_use]
    pub fn new(client: &'a VectorClient, token: &'a dyn ClaimToken) -> Self {
        Self { client, token }
    }
}

impl VectorBackend for VectorSession<'_> {
    async fn search(&self, params: SearchParams) -> Result<Vec<pb::ScoredPoint>> {
        self.client.search(self.token, params).await
    }

    async fn hybrid_search(&self, params: HybridSearchParams) -> Result<Vec<pb::ScoredPoint>> {
        self.client.hybrid_search(self.token, params).await
    }

    async fn delete_namespace(&self, namespace_id: &str) -> Result<()> {
        self.client
            .delete(
                self.token,
                None,
                toolkit_namespace(namespace_id),
                crate::client::DeleteSelector::WholeNamespace,
            )
            .await
            .map(|_| ())
    }
}

/// The weights of a hybrid search.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct FullText {
    /// Weight of the BM25 ranks.
    pub text_weight: f32,
    /// Weight of the dense ranks.
    pub dense_weight: f32,
}

impl FullText {
    /// Reads the SDK's `full_text_search` argument:
    /// `{enabled, fields, language, weight}`. It applies when `enabled` is
    /// true and `fields` is non-empty (the SDK's condition); the fields and
    /// the language are not used, since `elitea-vector` runs BM25 over the
    /// chunk text. `weight` is the text weight, default 0.3, and the dense
    /// weight is its complement (0.7 by default).
    ///
    /// # Errors
    ///
    /// [`Error::InvalidArgument`] for a malformed argument or a weight
    /// outside 0 to 1.
    pub fn from_value(config: Option<&Value>) -> Result<Option<Self>> {
        let Some(Value::Object(config)) = config else {
            return match config {
                None | Some(Value::Null) => Ok(None),
                Some(_) => Err(Error::InvalidArgument(
                    "full_text_search must be an object".to_owned(),
                )),
            };
        };
        let enabled = config.get("enabled").is_some_and(py_truthy);
        let has_fields = config.get("fields").is_some_and(py_truthy);
        if !enabled || !has_fields {
            return Ok(None);
        }
        let text_weight = match config.get("weight") {
            None | Some(Value::Null) => DEFAULT_TEXT_WEIGHT,
            Some(value) => value.as_f64().ok_or_else(|| {
                Error::InvalidArgument("full_text_search.weight must be a number".to_owned())
            })?,
        };
        if !(0.0..=1.0).contains(&text_weight) {
            return Err(Error::InvalidArgument(
                "full_text_search.weight is the text weight and must be between 0 and 1".to_owned(),
            ));
        }
        Ok(Some(Self {
            text_weight: narrow(text_weight),
            dense_weight: narrow(1.0 - text_weight),
        }))
    }
}

#[allow(
    clippy::cast_possible_truncation,
    reason = "weights and thresholds are small f32-range values"
)]
fn narrow(value: f64) -> f32 {
    value as f32
}

/// Everything one search needs, resolved.
#[derive(Clone, Debug)]
pub struct SearchPlan {
    /// The namespaces searched.
    pub scope: pb::Scope,
    /// Their embedding space.
    pub space: pb::EmbeddingSpace,
    /// The query text (BM25 input).
    pub query: String,
    /// The query embedding.
    pub vector: Vec<f32>,
    /// The caller's translated filter.
    pub filter: Option<pb::Filter>,
    /// `search_top`, at least 1.
    pub top: usize,
    /// The cut-off on the reranked `|dense score|`; 0 turns it off.
    pub cut_off: f64,
    /// `extended_search` chunk types; empty for a plain search.
    pub extended: Vec<String>,
    /// Hybrid weights, when `full_text_search` is on.
    pub full_text: Option<FullText>,
    /// Reranking rules.
    pub rerank: Vec<FieldRerank>,
}

/// Runs the search and returns at most `top` hits, best first (or in the
/// order a `sort` rule gave).
///
/// A failing sub-search of `extended_search` fails the whole search. (The
/// SDK logged it and carried on, which turned an outage into "No documents
/// found".)
///
/// # Errors
///
/// The facade's refusal, or a malformed reranking rule.
pub async fn search_documents<B: VectorBackend>(
    backend: &B,
    plan: &SearchPlan,
) -> Result<Vec<Hit>> {
    let mut raw: Vec<Hit>;
    if plan.extended.is_empty() {
        let k = OVERFETCH_CEILING.min(plan.top.saturating_mul(3));
        raw = fetch(backend, plan, plan.filter.as_ref(), k).await?;
    } else {
        let mut types: Vec<&str> = Vec::new();
        for chunk_type in &plan.extended {
            if EXTENDED_CHUNK_TYPES.contains(&chunk_type.as_str())
                && !types.contains(&chunk_type.as_str())
            {
                types.push(chunk_type);
            }
        }
        // One filter per sub-search, each checked against the service's
        // limits as it will be sent: the caller's filter passed them alone,
        // and the chunk-type condition may be the one too many.
        let filters = std::iter::once("document")
            .chain(types.iter().copied())
            .map(|chunk_type| narrowed(plan.filter.as_ref(), keyword("chunk_type", chunk_type)))
            .collect::<Result<Vec<_>>>()?;
        // The sub-searches run together; their results are merged in the
        // listed order, so the outcome is the same as one after the other.
        let mut results = try_join_all(
            filters
                .iter()
                .map(|filter| fetch(backend, plan, Some(filter), plan.top)),
        )
        .await?
        .into_iter();
        raw = results.next().unwrap_or_default();
        let mut seen: HashSet<String> = raw.iter().map(|hit| hit.key.clone()).collect();
        // The (document, chunk) of every new non-document hit, in the order
        // found, once each (a title and a summary of the same chunk name
        // the same document chunk).
        let mut wanted: Vec<(String, String)> = Vec::new();
        for found in results.flatten() {
            if seen.insert(found.key.clone()) {
                let pair = (found.document_key, found.chunk_id);
                if !wanted.contains(&pair) {
                    wanted.push(pair);
                }
            }
        }
        raw.extend(document_chunks(backend, plan, &wanted).await?);
    }
    // `rerank::apply` orders the hits (by score, or as a `sort` rule says).
    let hits = keep_best_duplicates(raw);
    let mut hits = rerank::apply(hits, &plan.rerank)?;
    if plan.cut_off != 0.0 {
        hits.retain(|hit| hit.dense_score.abs() >= plan.cut_off);
    }
    hits.truncate(plan.top);
    Ok(hits)
}

/// The caller's filter plus one `must` condition, checked against the
/// service's limits as it will be sent. The caller's filter passed them on
/// its own ([`crate::filter::translate`]); the added condition can be the
/// one too many, and the refusal says so instead of the service's.
fn narrowed(filter: Option<&pb::Filter>, condition: pb::Condition) -> Result<pb::Filter> {
    let filter = with_must(filter, condition);
    check_limits(&filter).map_err(added_conditions)?;
    Ok(filter)
}

/// Rewords a limit refusal for a filter the search extended.
fn added_conditions(error: FilterError) -> Error {
    match error {
        FilterError::NotExpressible(reason) => Error::Filter(FilterError::NotExpressible(format!(
            "with the conditions extended_search adds, {reason}"
        ))),
        other => Error::Filter(other),
    }
}

/// The document chunk of each `(document_key, chunk_id)` pair, read up to
/// [`DOCUMENT_BATCH`] pairs per call: the caller's filter, `chunk_type =
/// document`, and "at least one of" the pairs. Per pair the first (best)
/// point is kept, as the SDK's per-hit fetch of limit 1 did. A pair without
/// a chunk id (elitea-vector requires one, so only a foreign point) matches
/// its document's best document chunk.
///
/// A pair can match one point per searched namespace, and within a
/// namespace both the active-generation and the unversioned copy of a chunk
/// pass the scope. So a read asks for pairs x namespaces x
/// [`PAIR_HEADROOM`] results, collapses the copies of one chunk (the
/// active-generation one wins, as the scope's publish semantics say), and
/// when the answer was full and a pair is still unresolved, reads that pair
/// on its own: at most one extra call per missing pair, and none when the
/// answer had room left (then the chunk does not exist).
///
/// The batch is keyed by pairs, not by `document_key` alone: the SDK's
/// chunkers give a title, summary, propositions and document chunk of one
/// passage the same `chunk_id`, and a document has many passages. A
/// `document_key IN [...]` read would return the document's *best-scoring*
/// passage instead of the one the title or summary was found for, and a
/// document of many passages would fill the batch limit and starve the
/// other documents' reads.
async fn document_chunks<B: VectorBackend>(
    backend: &B,
    plan: &SearchPlan,
    wanted: &[(String, String)],
) -> Result<Vec<Hit>> {
    let namespaces = plan.scope.namespace_ids.len().max(1);
    let per_pair = namespaces.saturating_mul(PAIR_HEADROOM);
    let base = with_must(plan.filter.as_ref(), keyword("chunk_type", "document"));
    let mut found = Vec::with_capacity(wanted.len());
    let mut rest = wanted;
    while !rest.is_empty() {
        let (batch, remaining) = rest.split_at(batch_size(&base, rest)?);
        rest = remaining;
        let limit = batch.len().saturating_mul(per_pair);
        let points = fetch_points(backend, plan, Some(&pairs_filter(&base, batch)?), limit).await?;
        // The answer could hold no more: a pair not in it may have been
        // pushed out by another pair's copies.
        let full = points.len() >= limit.min(MAX_LIMIT as usize);
        let points = prefer_active(points);
        for pair in batch {
            if let Some(point) = points.iter().find(|point| pair_matches(point, pair)) {
                found.push(Hit::from_point(point));
            } else if full {
                let alone = fetch_points(
                    backend,
                    plan,
                    Some(&pairs_filter(&base, std::slice::from_ref(pair))?),
                    per_pair,
                )
                .await?;
                if let Some(point) = prefer_active(alone)
                    .iter()
                    .find(|point| pair_matches(point, pair))
                {
                    found.push(Hit::from_point(point));
                }
            }
        }
    }
    Ok(found)
}

/// How many of `pairs` the next document-chunk read can name: at most
/// [`DOCUMENT_BATCH`], and no more than fit under the service's limits
/// beside `base` (the caller's filter with the `chunk_type` condition). The
/// pairs group is one nested condition in `base`'s `must`; each pair costs
/// one condition (a bare `document_key` match) or three (a nested filter
/// holding `document_key` and `chunk_id`).
///
/// # Errors
///
/// [`FilterError::NotExpressible`] when not even one pair fits.
fn batch_size(base: &pb::Filter, pairs: &[(String, String)]) -> Result<usize> {
    if base.must.len() >= MAX_CONDITIONS {
        return Err(added_conditions(FilterError::NotExpressible(format!(
            "it needs {} conditions in one must list, and the vector service accepts at most {MAX_CONDITIONS}",
            base.must.len() + 1
        ))));
    }
    let used = condition_count(base) + 1;
    let mut budget = MAX_TOTAL_CONDITIONS.saturating_sub(used);
    let mut size = 0;
    for (_, chunk_id) in pairs.iter().take(DOCUMENT_BATCH) {
        let cost = if chunk_id.is_empty() { 1 } else { 3 };
        if cost > budget {
            break;
        }
        budget -= cost;
        size += 1;
    }
    if size == 0 {
        return Err(added_conditions(FilterError::NotExpressible(format!(
            "it holds {used} conditions and the document-chunk read of extended_search needs {} more, and the vector service accepts at most {MAX_TOTAL_CONDITIONS}",
            if pairs
                .first()
                .is_some_and(|(_, chunk_id)| chunk_id.is_empty())
            {
                1
            } else {
                3
            }
        ))));
    }
    Ok(size)
}

/// `base` and "at least one of `pairs`", checked against the limits.
fn pairs_filter(base: &pb::Filter, pairs: &[(String, String)]) -> Result<pb::Filter> {
    let group = pairs
        .iter()
        .map(|(document_key, chunk_id)| {
            let mut must = vec![keyword("document_key", document_key)];
            if !chunk_id.is_empty() {
                must.push(keyword("chunk_id", chunk_id));
            }
            nest(pb::Filter {
                must,
                ..pb::Filter::default()
            })
        })
        .collect();
    let filter = with_must(
        Some(base),
        nest(pb::Filter {
            should: group,
            ..pb::Filter::default()
        }),
    );
    check_limits(&filter).map_err(added_conditions)?;
    Ok(filter)
}

fn pair_matches(point: &pb::ScoredPoint, (document_key, chunk_id): &(String, String)) -> bool {
    point.document_key == *document_key && (chunk_id.is_empty() || point.chunk_id == *chunk_id)
}

/// Collapses the copies of one chunk: points of the same namespace,
/// document and chunk id. The first (best-scoring) copy stays unless a later
/// one has a generation and it has none: the scope reads a namespace's
/// active generation plus its unversioned points, and where both hold the
/// chunk the published one is the index's.
fn prefer_active(points: Vec<pb::ScoredPoint>) -> Vec<pb::ScoredPoint> {
    let mut kept: Vec<pb::ScoredPoint> = Vec::with_capacity(points.len());
    for point in points {
        let same = kept.iter_mut().find(|existing| {
            existing.namespace_id == point.namespace_id
                && existing.document_key == point.document_key
                && existing.chunk_id == point.chunk_id
        });
        match same {
            Some(existing) => {
                if existing.generation.is_empty() && !point.generation.is_empty() {
                    *existing = point;
                }
            }
            None => kept.push(point),
        }
    }
    kept
}

/// One backend call, as hits.
async fn fetch<B: VectorBackend>(
    backend: &B,
    plan: &SearchPlan,
    filter: Option<&pb::Filter>,
    k: usize,
) -> Result<Vec<Hit>> {
    let points = prefer_active(fetch_points(backend, plan, filter, k).await?);
    Ok(points.iter().map(Hit::from_point).collect())
}

/// One backend call: dense, or hybrid when the plan has full-text weights.
/// No score threshold is sent: the cut-off is the caller's, after reranking.
async fn fetch_points<B: VectorBackend>(
    backend: &B,
    plan: &SearchPlan,
    filter: Option<&pb::Filter>,
    k: usize,
) -> Result<Vec<pb::ScoredPoint>> {
    let limit = u32::try_from(k.clamp(1, MAX_LIMIT as usize)).unwrap_or(MAX_LIMIT);
    let search = SearchParams {
        scope: plan.scope.clone(),
        space: plan.space.clone(),
        vector: plan.vector.clone(),
        filter: filter.cloned(),
        limit,
        score_threshold: None,
    };
    let points = match plan.full_text {
        Some(weights) => {
            backend
                .hybrid_search(HybridSearchParams {
                    search,
                    text: plan.query.clone(),
                    dense_weight: weights.dense_weight,
                    text_weight: weights.text_weight,
                })
                .await?
        }
        None => backend.search(search).await?,
    };
    Ok(points)
}

/// Collapses repeated keys, keeping the better score. Each key stays at the
/// position of its first occurrence; the hit that holds it is the better
/// one.
#[must_use]
pub fn keep_best_duplicates(hits: Vec<Hit>) -> Vec<Hit> {
    let mut kept: Vec<Hit> = Vec::with_capacity(hits.len());
    for hit in hits {
        match kept.iter_mut().find(|existing| existing.key == hit.key) {
            Some(existing) => {
                if hit.score > existing.score {
                    *existing = hit;
                }
            }
            None => kept.push(hit),
        }
    }
    kept
}

#[cfg(test)]
mod tests {
    use std::sync::Mutex;

    use serde_json::json;

    use super::*;

    /// One stored chunk of the fake backend.
    #[derive(Clone)]
    #[allow(clippy::struct_field_names, reason = "named after the payload keys")]
    struct Chunk {
        document_key: &'static str,
        chunk_id: &'static str,
        chunk_type: &'static str,
        text: &'static str,
        /// The score the fake returns for every query.
        score: f32,
        metadata: &'static str,
        /// The point's generation, `""` for an unversioned point.
        generation: &'static str,
    }

    /// A recorded call: kind, limit, filter, score threshold.
    type Call = (String, u32, Option<pb::Filter>, Option<f32>);

    /// Returns chunks that satisfy the request's filter (Qdrant semantics,
    /// nested filters included), by score, and records every call. A hybrid
    /// call returns rank-based fused scores and the chunk's score as the
    /// dense score, as if the text matched nothing.
    #[derive(Default)]
    struct Fake {
        chunks: Vec<Chunk>,
        calls: Mutex<Vec<Call>>,
    }

    impl Fake {
        fn answer(
            &self,
            kind: &str,
            limit: u32,
            filter: Option<&pb::Filter>,
            threshold: Option<f32>,
        ) -> Vec<pb::ScoredPoint> {
            self.calls
                .lock()
                .unwrap()
                .push((kind.to_owned(), limit, filter.cloned(), threshold));
            let mut found: Vec<&Chunk> = self
                .chunks
                .iter()
                .filter(|c| {
                    let point = pb::Point {
                        document_key: c.document_key.to_owned(),
                        chunk_id: c.chunk_id.to_owned(),
                        chunk_type: c.chunk_type.to_owned(),
                        metadata_json: c.metadata.to_owned(),
                        ..pb::Point::default()
                    };
                    crate::testing::matches_filter(&point, filter)
                })
                .collect();
            found.sort_by(|a, b| b.score.total_cmp(&a.score));
            let hybrid = kind.starts_with("hybrid");
            found
                .into_iter()
                .take(limit as usize)
                .enumerate()
                .map(|(rank, c)| pb::ScoredPoint {
                    score: if hybrid {
                        0.7 / (61.0 + f32::from(u8::try_from(rank).unwrap_or(u8::MAX)))
                    } else {
                        c.score
                    },
                    dense_score: Some(c.score),
                    namespace_id: "ns".to_owned(),
                    generation: c.generation.to_owned(),
                    document_key: c.document_key.to_owned(),
                    chunk_id: c.chunk_id.to_owned(),
                    chunk_type: c.chunk_type.to_owned(),
                    text: c.text.to_owned(),
                    metadata_json: c.metadata.to_owned(),
                    ..pb::ScoredPoint::default()
                })
                .collect()
        }
    }

    #[allow(
        clippy::unused_async_trait_impl,
        reason = "a synchronous implementation of an async trait; its work stays lazy (runs when polled, as in the async implementations)"
    )]
    impl VectorBackend for Fake {
        async fn search(&self, params: SearchParams) -> Result<Vec<pb::ScoredPoint>> {
            Ok(self.answer(
                "dense",
                params.limit,
                params.filter.as_ref(),
                params.score_threshold,
            ))
        }
        async fn hybrid_search(&self, params: HybridSearchParams) -> Result<Vec<pb::ScoredPoint>> {
            let search = params.search;
            Ok(self.answer(
                &format!(
                    "hybrid {:.1}/{:.1} {}",
                    params.text_weight, params.dense_weight, params.text
                ),
                search.limit,
                search.filter.as_ref(),
                search.score_threshold,
            ))
        }
        async fn delete_namespace(&self, _: &str) -> Result<()> {
            Ok(())
        }
    }

    fn chunk(
        document_key: &'static str,
        chunk_id: &'static str,
        chunk_type: &'static str,
        score: f32,
    ) -> Chunk {
        Chunk {
            document_key,
            chunk_id,
            chunk_type,
            text: "text",
            score,
            metadata: "{}",
            generation: "",
        }
    }

    fn plan() -> SearchPlan {
        SearchPlan {
            scope: crate::client::toolkit_scope(vec!["ns".to_owned()], None),
            space: pb::EmbeddingSpace {
                model_slug: "m".to_owned(),
                dimension: 2,
            },
            query: "q".to_owned(),
            vector: vec![1.0, 0.0],
            filter: None,
            top: 10,
            cut_off: 0.1,
            extended: Vec::new(),
            full_text: None,
            rerank: Vec::new(),
        }
    }

    fn keys(hits: &[Hit]) -> Vec<&str> {
        hits.iter().map(|hit| hit.key.as_str()).collect()
    }

    #[tokio::test]
    async fn plain_search_overfetches_thirty_at_most_and_truncates() {
        let chunks = (0_u8..40)
            .map(|n| Chunk {
                chunk_id: Box::leak(n.to_string().into_boxed_str()),
                ..chunk("d", "0", "document", 0.9 - f32::from(n) * 0.01)
            })
            .collect();
        let fake = Fake {
            chunks,
            ..Fake::default()
        };
        let mut p = plan();
        p.top = 12;
        let hits = search_documents(&fake, &p).await.unwrap();
        assert_eq!(hits.len(), 12);
        assert_eq!(fake.calls.lock().unwrap()[0].1, 30, "min(30, 3 x 12)");
        p.top = 4;
        search_documents(&fake, &p).await.unwrap();
        assert_eq!(fake.calls.lock().unwrap()[1].1, 12, "min(30, 3 x 4)");
    }

    #[tokio::test]
    async fn the_cut_off_applies_to_the_absolute_score_and_zero_disables_it() {
        let fake = Fake {
            chunks: vec![
                chunk("a", "0", "document", 0.5),
                chunk("b", "0", "document", 0.09),
                chunk("c", "0", "document", -0.3),
            ],
            ..Fake::default()
        };
        let mut p = plan();
        let hits = search_documents(&fake, &p).await.unwrap();
        assert_eq!(keys(&hits), ["a_0", "c_0"], "|-0.3| passes, 0.09 does not");
        p.cut_off = 0.0;
        assert_eq!(search_documents(&fake, &p).await.unwrap().len(), 3);
    }

    #[tokio::test]
    async fn cut_off_runs_after_reranking() {
        let fake = Fake {
            chunks: vec![Chunk {
                metadata: r#"{"kind": "spec"}"#,
                ..chunk("a", "0", "document", 0.06)
            }],
            ..Fake::default()
        };
        let mut p = plan();
        assert_eq!(search_documents(&fake, &p).await.unwrap(), []);
        p.rerank = rerank::parse(&json!({"kind": {"weight": 1.0, "rules": {"priority": "spec"}}}))
            .unwrap();
        let hits = search_documents(&fake, &p).await.unwrap();
        assert_eq!(keys(&hits), ["a_0"], "0.06 x 2 = 0.12 clears 0.1");
    }

    #[test]
    fn a_repeated_key_keeps_the_better_score() {
        let make = |score: f64, text: &str| Hit {
            key: "d_1".to_owned(),
            page_content: text.to_owned(),
            metadata: serde_json::Map::new(),
            score,
            dense_score: score,
            document_key: "d".to_owned(),
            chunk_id: "1".to_owned(),
        };
        let other = Hit {
            key: "e_1".to_owned(),
            ..make(0.5, "other")
        };
        // The worse hit first, the better later: the better one wins, at the
        // first position.
        let kept =
            keep_best_duplicates(vec![make(0.2, "worse"), other.clone(), make(0.9, "better")]);
        assert_eq!(kept.len(), 2);
        assert_eq!(
            (kept[0].page_content.as_str(), kept[0].score),
            ("better", 0.9)
        );
        // The better hit first: it stays.
        let kept = keep_best_duplicates(vec![make(0.9, "better"), make(0.2, "worse")]);
        assert_eq!(
            (kept[0].page_content.as_str(), kept[0].score),
            ("better", 0.9)
        );
        assert_eq!(kept.len(), 1);
    }

    #[tokio::test]
    async fn a_chunk_that_shares_a_key_with_a_document_chunk_does_not_lower_its_score() {
        // A title chunk and a document chunk of the same document and chunk
        // id collide in a plain search. The SDK kept the later (worse) one.
        let fake = Fake {
            chunks: vec![
                chunk("d", "0", "document", 0.8),
                chunk("d", "0", "title", 0.3),
            ],
            ..Fake::default()
        };
        let mut p = plan();
        p.cut_off = 0.0;
        let hits = search_documents(&fake, &p).await.unwrap();
        assert_eq!(hits.len(), 1);
        assert!((hits[0].score - 0.8).abs() < 1e-6);
    }

    #[tokio::test]
    async fn extended_search_adds_the_document_chunk_of_a_title_hit() {
        let spec = r#"{"kind": "spec"}"#;
        let fake = Fake {
            chunks: vec![
                // Five filler documents and d1 fill the document search.
                chunk("f1", "0", "document", 0.80),
                chunk("f2", "0", "document", 0.79),
                chunk("f3", "0", "document", 0.78),
                chunk("f4", "0", "document", 0.77),
                chunk("f5", "0", "document", 0.76),
                chunk("d1", "0", "document", 0.70),
                chunk("d1", "0", "title", 0.90),
                // d2 is found through its title; its document chunk scores
                // 0.4 against the query, below the document search's top.
                chunk("d2", "0", "title", 0.95),
                Chunk {
                    metadata: spec,
                    ..chunk("d2", "0", "document", 0.40)
                },
                // d3 is found through a keywords chunk.
                chunk("d3", "0", "keywords", 0.60),
                Chunk {
                    metadata: spec,
                    ..chunk("d3", "0", "document", 0.35)
                },
                // propositions are not asked for.
                chunk("d4", "0", "propositions", 0.99),
                Chunk {
                    metadata: spec,
                    ..chunk("d4", "0", "document", 0.20)
                },
            ],
            ..Fake::default()
        };
        let mut p = plan();
        p.cut_off = 0.0;
        p.top = 6;
        p.extended = vec!["title".into(), "keywords".into(), "bogus".into()];
        // Without a boost the added document chunks sort below the top 6
        // and are cut: the SDK behaves the same.
        let plain = search_documents(&fake, &p).await.unwrap();
        assert_eq!(
            keys(&plain),
            ["f1_0", "f2_0", "f3_0", "f4_0", "f5_0", "d1_0"]
        );
        fake.calls.lock().unwrap().clear();
        p.rerank = rerank::parse(&json!({"kind": {"weight": 5.0, "rules": {"priority": "spec"}}}))
            .unwrap();
        let hits = search_documents(&fake, &p).await.unwrap();
        assert_eq!(
            keys(&hits),
            ["d2_0", "d3_0", "f1_0", "f2_0", "f3_0", "f4_0"]
        );
        assert!(
            (hits[0].score - 2.4).abs() < 1e-6,
            "the document chunk's own score, boosted: {}",
            hits[0].score
        );
        for hit in &hits {
            assert_eq!(hit.metadata["chunk_type"], "document");
        }
        // 1 document search + 1 per valid type + ONE read of the document
        // chunks of every new key (d2 from title, d3 from keywords; d1's
        // title hit is already known).
        let calls = fake.calls.lock().unwrap();
        assert_eq!(calls.len(), 1 + 2 + 1);
        assert!(calls.iter().all(|call| call.1 <= 6));
        assert_eq!(
            calls[3].1, 4,
            "two pairs x one namespace x the headroom for their copies"
        );
    }

    #[tokio::test]
    async fn extended_search_keeps_the_callers_filter_on_every_call() {
        let fake = Fake {
            chunks: vec![
                Chunk {
                    metadata: r#"{"team": "x"}"#,
                    ..chunk("d", "1", "document", 0.5)
                },
                Chunk {
                    metadata: r#"{"team": "x"}"#,
                    ..chunk("e", "0", "title", 0.9)
                },
                Chunk {
                    metadata: r#"{"team": "x"}"#,
                    ..chunk("e", "0", "document", 0.4)
                },
                // Another team's chunks never appear, whatever they score.
                chunk("z", "0", "title", 0.99),
                chunk("z", "0", "document", 0.98),
            ],
            ..Fake::default()
        };
        let mut p = plan();
        p.top = 1;
        p.filter = Some(pb::Filter {
            must: vec![keyword("metadata.team", "x")],
            ..pb::Filter::default()
        });
        p.extended = vec!["title".into()];
        p.cut_off = 0.0;
        search_documents(&fake, &p).await.unwrap();
        for (_, _, filter, _) in fake.calls.lock().unwrap().iter() {
            let filter = filter.as_ref().unwrap();
            assert!(filter.must.contains(&keyword("metadata.team", "x")));
            assert!(
                filter.must.contains(&keyword("chunk_type", "document"))
                    || filter.must.contains(&keyword("chunk_type", "title"))
            );
        }
        let calls = fake.calls.lock().unwrap();
        let fetch = calls.last().unwrap().2.as_ref().unwrap();
        assert!(fetch.must.contains(&keyword("chunk_type", "document")));
        let Some(pb::condition::Match::Filter(pairs)) = &fetch.must.last().unwrap().r#match else {
            panic!("the pairs group: {fetch:?}");
        };
        assert_eq!(
            pairs.should,
            vec![nest(pb::Filter {
                must: vec![keyword("document_key", "e"), keyword("chunk_id", "0")],
                ..pb::Filter::default()
            })]
        );
    }

    #[tokio::test]
    async fn extended_search_reads_document_chunks_in_bounded_batches() {
        // 40 documents whose document chunks score below the 40 fillers'
        // (so the document search never returns them) but whose titles
        // score highest: 40 new pairs, found only through their titles.
        let mut chunks = Vec::new();
        for n in 0_u8..40 {
            let key: &'static str = Box::leak(format!("t{n}").into_boxed_str());
            chunks.push(chunk(key, "0", "title", 0.9 - f32::from(n) * 0.01));
            chunks.push(chunk(key, "0", "document", 0.3 - f32::from(n) * 0.001));
            let filler: &'static str = Box::leak(format!("f{n}").into_boxed_str());
            chunks.push(chunk(filler, "0", "document", 0.5 - f32::from(n) * 0.001));
        }
        let fake = Fake {
            chunks,
            ..Fake::default()
        };
        let mut p = plan();
        p.cut_off = 0.0;
        p.top = 40;
        p.extended = vec!["title".into()];
        let hits = search_documents(&fake, &p).await.unwrap();
        assert_eq!(hits.len(), 40);
        let calls = fake.calls.lock().unwrap();
        // document search, title search, then ceil(40 / 32) = 2 reads; never
        // one per hit.
        assert_eq!(calls.len(), 2 + 40_usize.div_ceil(DOCUMENT_BATCH));
        assert_eq!(
            (calls[2].1, calls[3].1),
            (64, 16),
            "pairs x 1 namespace x 2"
        );
    }

    #[tokio::test]
    async fn full_text_search_calls_hybrid_with_the_default_weights() {
        let fake = Fake {
            chunks: vec![chunk("a", "0", "document", 0.4)],
            ..Fake::default()
        };
        let mut p = plan();
        p.full_text =
            FullText::from_value(Some(&json!({"enabled": true, "fields": ["text"]}))).unwrap();
        let hits = search_documents(&fake, &p).await.unwrap();
        let calls = fake.calls.lock().unwrap();
        assert_eq!(calls[0].0, "hybrid 0.3/0.7 q");
        assert_eq!(calls[0].3, None, "no server-side threshold");
        assert_eq!(
            hits.len(),
            1,
            "the cut-off reads the dense score, not the rank-based fused one"
        );
    }

    #[tokio::test]
    async fn hybrid_and_dense_cut_and_rerank_alike_when_text_adds_nothing() {
        let spec = r#"{"kind": "spec"}"#;
        let fake = Fake {
            chunks: vec![
                chunk("a", "0", "document", 0.9),
                chunk("b", "0", "document", 0.3),
                // Below the cut-off alone, above it once boosted.
                Chunk {
                    metadata: spec,
                    ..chunk("c", "0", "document", 0.12)
                },
                chunk("d", "0", "document", 0.15),
                chunk("e", "0", "document", 0.05),
                chunk("f", "0", "document", -0.4),
            ],
            ..Fake::default()
        };
        let mut p = plan();
        p.cut_off = 0.2;
        p.rerank = rerank::parse(&json!({"kind": {"weight": 1.0, "rules": {"priority": "spec"}}}))
            .unwrap();
        let mut dense = keys(&search_documents(&fake, &p).await.unwrap())
            .into_iter()
            .map(str::to_owned)
            .collect::<Vec<_>>();
        p.full_text =
            FullText::from_value(Some(&json!({"enabled": true, "fields": ["text"]}))).unwrap();
        let mut hybrid = keys(&search_documents(&fake, &p).await.unwrap())
            .into_iter()
            .map(str::to_owned)
            .collect::<Vec<_>>();
        dense.sort();
        hybrid.sort();
        assert_eq!(dense, ["a_0", "b_0", "c_0", "f_0"]);
        assert_eq!(hybrid, dense);
        for call in fake.calls.lock().unwrap().iter() {
            assert_eq!(call.3, None, "{call:?}");
        }
    }

    #[test]
    fn full_text_config_follows_the_sdk_condition() {
        let on = FullText::from_value(Some(
            &json!({"enabled": true, "fields": ["a"], "weight": 0.5}),
        ))
        .unwrap()
        .unwrap();
        assert_eq!((on.text_weight, on.dense_weight), (0.5, 0.5));
        for off in [
            json!({"enabled": false, "fields": ["a"]}),
            json!({"enabled": true, "fields": []}),
            json!({"enabled": true}),
            json!({}),
        ] {
            assert_eq!(FullText::from_value(Some(&off)).unwrap(), None, "{off}");
        }
        assert_eq!(FullText::from_value(None).unwrap(), None);
        assert!(
            FullText::from_value(Some(
                &json!({"enabled": true, "fields": ["a"], "weight": 2})
            ))
            .is_err()
        );
        assert!(FullText::from_value(Some(&json!([1]))).is_err());
    }

    #[tokio::test]
    async fn sort_rules_order_the_results_before_truncation() {
        let fake = Fake {
            chunks: vec![
                Chunk {
                    metadata: r#"{"year": 2020}"#,
                    ..chunk("a", "0", "document", 0.9)
                },
                Chunk {
                    metadata: r#"{"year": 2024}"#,
                    ..chunk("b", "0", "document", 0.5)
                },
                Chunk {
                    metadata: r#"{"year": 2022}"#,
                    ..chunk("c", "0", "document", 0.7)
                },
            ],
            ..Fake::default()
        };
        let mut p = plan();
        p.top = 2;
        p.rerank = rerank::parse(&json!({"year": {"rules": {"sort": "desc"}}})).unwrap();
        let hits = search_documents(&fake, &p).await.unwrap();
        assert_eq!(keys(&hits), ["b_0", "c_0"]);
    }

    #[tokio::test]
    async fn extended_search_merges_sub_searches_in_the_listed_type_order() {
        // Each type finds a different document; the document search finds
        // only the filler. The pairs read must list the keywords hit before
        // the title hit because the caller listed keywords first, whatever
        // order the concurrent sub-searches finished in.
        let fake = Fake {
            chunks: vec![
                chunk("filler", "0", "document", 0.9),
                chunk("by-title", "0", "title", 0.8),
                chunk("by-title", "0", "document", 0.2),
                chunk("by-keywords", "0", "keywords", 0.7),
                chunk("by-keywords", "0", "document", 0.3),
                chunk("by-summary", "0", "summary", 0.6),
                chunk("by-summary", "0", "document", 0.1),
            ],
            ..Fake::default()
        };
        let mut p = plan();
        p.cut_off = 0.0;
        p.top = 1;
        p.extended = vec!["keywords".into(), "title".into(), "summary".into()];
        let hits = search_documents(&fake, &p).await.unwrap();
        assert_eq!(keys(&hits), ["filler_0"]);
        let calls = fake.calls.lock().unwrap().clone();
        assert_eq!(calls.len(), 1 + 3 + 1);
        let read = calls.last().unwrap().2.as_ref().unwrap();
        let Some(pb::condition::Match::Filter(pairs)) = &read.must.last().unwrap().r#match else {
            panic!("the pairs group: {read:?}");
        };
        let pair_keys: Vec<&str> = pairs
            .should
            .iter()
            .map(|condition| match &condition.r#match {
                Some(pb::condition::Match::Filter(pair)) => pair.must[0].r#match.as_ref(),
                _ => None,
            })
            .map(|m| match m {
                Some(pb::condition::Match::Keyword(key)) => key.as_str(),
                other => panic!("{other:?}"),
            })
            .collect();
        assert_eq!(pair_keys, ["by-keywords", "by-title", "by-summary"]);
        // And the same with the types the other way round.
        drop(calls);
        fake.calls.lock().unwrap().clear();
        p.extended = vec!["summary".into(), "title".into(), "keywords".into()];
        search_documents(&fake, &p).await.unwrap();
        let calls = fake.calls.lock().unwrap();
        let read = calls.last().unwrap().2.as_ref().unwrap();
        let Some(pb::condition::Match::Filter(pairs)) = &read.must.last().unwrap().r#match else {
            panic!("the pairs group: {read:?}");
        };
        let first = match &pairs.should[0].r#match {
            Some(pb::condition::Match::Filter(pair)) => pair.must[0].clone(),
            other => panic!("{other:?}"),
        };
        assert_eq!(first, keyword("document_key", "by-summary"));
    }

    #[tokio::test]
    async fn copies_of_one_pair_no_longer_starve_the_other_pairs() {
        // Document a's chunk 0 is stored five times (five copies pass the
        // filter and all outscore b's chunk); a and b were both found
        // through their titles.
        let mut chunks = vec![chunk("b", "0", "document", 0.1)];
        for n in 0_u8..5 {
            chunks.push(chunk("a", "0", "document", 0.5 - f32::from(n) * 0.01));
        }
        let fake = Fake {
            chunks,
            ..Fake::default()
        };
        let wanted = [
            ("a".to_owned(), "0".to_owned()),
            ("b".to_owned(), "0".to_owned()),
        ];
        let hits = document_chunks(&fake, &plan(), &wanted).await.unwrap();
        assert_eq!(
            keys(&hits),
            ["a_0", "b_0"],
            "b's document chunk was starved"
        );
        let calls = fake.calls.lock().unwrap();
        // The pairs read (2 pairs x 1 ns x 2, full of a's copies), then b
        // alone.
        assert_eq!(calls.len(), 2, "{calls:?}");
        assert_eq!(calls[0].1, 4);
        assert_eq!(calls[1].1, 2);
        let alone = calls[1].2.as_ref().unwrap();
        assert!(
            alone.must.contains(&keyword("chunk_type", "document")),
            "{alone:?}"
        );
        assert_eq!(
            alone.must.last().unwrap(),
            &nest(pb::Filter {
                should: vec![nest(pb::Filter {
                    must: vec![keyword("document_key", "b"), keyword("chunk_id", "0")],
                    ..pb::Filter::default()
                })],
                ..pb::Filter::default()
            })
        );
    }

    #[tokio::test]
    async fn a_missing_document_chunk_costs_no_extra_read_when_the_answer_had_room() {
        // A title whose document chunk does not exist: the pairs read comes
        // back short, so nothing was starved and no second read is made.
        let fake = Fake {
            chunks: vec![
                chunk("filler", "0", "document", 0.99),
                chunk("orphan", "0", "title", 0.9),
            ],
            ..Fake::default()
        };
        let mut p = plan();
        p.cut_off = 0.0;
        p.top = 2;
        p.extended = vec!["title".into()];
        let hits = search_documents(&fake, &p).await.unwrap();
        assert_eq!(keys(&hits), ["filler_0"]);
        assert_eq!(fake.calls.lock().unwrap().len(), 3);
    }

    #[tokio::test]
    async fn the_published_copy_of_a_document_chunk_beats_the_unversioned_one() {
        let fake = Fake {
            chunks: vec![
                // The unversioned copy scores higher, the published one is
                // the index's.
                Chunk {
                    text: "stale",
                    ..chunk("d", "0", "document", 0.6)
                },
                Chunk {
                    text: "published",
                    generation: "g7",
                    ..chunk("d", "0", "document", 0.5)
                },
            ],
            ..Fake::default()
        };
        let wanted = [("d".to_owned(), "0".to_owned())];
        let hits = document_chunks(&fake, &plan(), &wanted).await.unwrap();
        assert_eq!(hits.len(), 1);
        assert_eq!(hits[0].page_content, "published");
        assert!((hits[0].score - 0.5).abs() < 1e-6);
        // The same holds for a plain search that reads both copies.
        let mut p = plan();
        p.cut_off = 0.0;
        let hits = search_documents(&fake, &p).await.unwrap();
        assert_eq!(hits.len(), 1);
        assert_eq!(hits[0].page_content, "published");
    }

    #[test]
    fn copies_collapse_per_namespace_document_and_chunk() {
        let point = |namespace: &str, generation: &str, score: f32| pb::ScoredPoint {
            namespace_id: namespace.to_owned(),
            generation: generation.to_owned(),
            document_key: "d".to_owned(),
            chunk_id: "0".to_owned(),
            score,
            ..pb::ScoredPoint::default()
        };
        let kept = prefer_active(vec![
            point("n1", "", 0.9),
            point("n2", "", 0.8),
            point("n1", "g", 0.7),
            point("n1", "", 0.6),
        ]);
        assert_eq!(kept.len(), 2);
        assert_eq!((kept[0].namespace_id.as_str(), kept[0].score), ("n1", 0.7));
        assert_eq!((kept[1].namespace_id.as_str(), kept[1].score), ("n2", 0.8));
        // A published copy already kept stays over a later unversioned one.
        let kept = prefer_active(vec![point("n1", "g", 0.9), point("n1", "", 0.95)]);
        assert_eq!((kept.len(), kept[0].generation.as_str()), (1, "g"));
    }

    /// A filter of exactly `total` conditions (nested groups of up to 63
    /// keyword matches, each on `metadata.k == v`), with a short `must`.
    fn filter_with_total(total: usize) -> pb::Filter {
        let mut must = Vec::new();
        let mut remaining = total;
        while remaining > 0 {
            let take = remaining.min(MAX_CONDITIONS);
            if take <= 2 {
                must.push(keyword("metadata.k", "v"));
                remaining -= 1;
            } else {
                must.push(nest(pb::Filter {
                    must: vec![keyword("metadata.k", "v"); take - 1],
                    ..pb::Filter::default()
                }));
                remaining -= take;
            }
        }
        let filter = pb::Filter {
            must,
            ..pb::Filter::default()
        };
        assert_eq!(condition_count(&filter), total);
        check_limits(&filter).unwrap();
        filter
    }

    /// 20 documents found only through their titles (their document chunks
    /// score below the fillers'), all carrying `k: v`.
    fn twenty_titled_documents() -> Fake {
        let mut chunks = Vec::new();
        for n in 0_u8..20 {
            let key: &'static str = Box::leak(format!("t{n}").into_boxed_str());
            chunks.push(Chunk {
                metadata: r#"{"k": "v"}"#,
                ..chunk(key, "0", "title", 0.9 - f32::from(n) * 0.01)
            });
            chunks.push(Chunk {
                metadata: r#"{"k": "v"}"#,
                ..chunk(key, "0", "document", 0.3 - f32::from(n) * 0.001)
            });
            let filler: &'static str = Box::leak(format!("f{n}").into_boxed_str());
            chunks.push(Chunk {
                metadata: r#"{"k": "v"}"#,
                ..chunk(filler, "0", "document", 0.5 - f32::from(n) * 0.001)
            });
        }
        Fake {
            chunks,
            ..Fake::default()
        }
    }

    #[tokio::test]
    async fn document_chunk_batches_shrink_to_fit_the_condition_limits() {
        // 977 conditions + chunk_type + the pairs group leaves 45: 15 pairs
        // of 3 conditions per read instead of 32.
        let fake = twenty_titled_documents();
        let mut p = plan();
        p.cut_off = 0.0;
        p.top = 20;
        p.extended = vec!["title".into()];
        p.filter = Some(filter_with_total(977));
        let hits = search_documents(&fake, &p).await.unwrap();
        assert_eq!(hits.len(), 20);
        let calls = fake.calls.lock().unwrap();
        assert_eq!(calls.len(), 2 + 2, "{}", calls.len());
        assert_eq!((calls[2].1, calls[3].1), (30, 10), "15 and 5 pairs x 2");
        for call in &calls[2..] {
            let sent = call.2.as_ref().unwrap();
            check_limits(sent).unwrap();
            assert!(condition_count(sent) <= MAX_TOTAL_CONDITIONS);
        }
        assert_eq!(condition_count(calls[2].2.as_ref().unwrap()), 977 + 2 + 45);
    }

    #[tokio::test]
    async fn a_filter_leaving_no_room_for_one_pair_is_refused_readably() {
        // 1022 conditions: the sub-searches still fit (1023), the read of
        // even one pair (3 more) does not.
        let fake = twenty_titled_documents();
        let mut p = plan();
        p.cut_off = 0.0;
        p.top = 2;
        p.extended = vec!["title".into()];
        p.filter = Some(filter_with_total(1022));
        let error = search_documents(&fake, &p).await.expect_err("refused");
        let message = error.to_string();
        assert!(
            message.contains("cannot be expressed")
                && message.contains("extended_search")
                && message.contains("1024"),
            "{message}"
        );
        // The sub-searches did run; the read did not.
        assert_eq!(fake.calls.lock().unwrap().len(), 2);
        // Without extended_search the same filter searches fine.
        p.extended = Vec::new();
        search_documents(&fake, &p).await.unwrap();
    }

    #[tokio::test]
    async fn the_chunk_type_condition_is_counted_against_the_must_list_limit() {
        // Exactly 64 must conditions pass translation; with the chunk_type
        // condition extended_search adds the list would hold 65.
        let fake = twenty_titled_documents();
        let mut p = plan();
        p.cut_off = 0.0;
        p.extended = vec!["title".into()];
        p.filter = Some(pb::Filter {
            must: (0..MAX_CONDITIONS)
                .map(|n| keyword("metadata.k", if n == 0 { "v" } else { "w" }))
                .collect(),
            ..pb::Filter::default()
        });
        check_limits(p.filter.as_ref().unwrap()).unwrap();
        let error = search_documents(&fake, &p).await.expect_err("refused");
        let message = error.to_string();
        assert!(
            message.contains("extended_search") && message.contains("must list"),
            "{message}"
        );
        assert!(
            fake.calls.lock().unwrap().is_empty(),
            "refused before any call"
        );
        p.extended = Vec::new();
        search_documents(&fake, &p).await.unwrap();
    }
}
