//! The SDK's `search_documents` on top of `elitea-vector` (ADR-0030
//! decision 5).
//!
//! * **Over-fetch.** A plain search asks for `k = min(30, 3 × search_top)`
//!   results by cosine similarity, reranks them, applies the cut-off and
//!   keeps `search_top`.
//! * **`extended_search`.** Searches `chunk_type = document` and each listed
//!   type (`title`, `summary`, `propositions`, `keywords`), `search_top`
//!   results each. A hit on a non-document chunk brings in its document
//!   chunk (same document and chunk id), scored by the query; the
//!   non-document chunk itself is not returned.
//! * **Duplicates.** Results are keyed `{document_key}_{chunk_id}`. When a
//!   key repeats, the **better** score is kept (the SDK kept the worse).
//! * **Rerank, cut-off, truncate.** See [`crate::rerank`]. The cut-off is
//!   on `|score|`; 0 turns it off.
//! * **`full_text_search`.** Calls `HybridSearch`: BM25 over the chunk text
//!   fused with the dense search by weighted RRF, 0.3 text / 0.7 dense (the
//!   SDK's `weight` is the text weight). The SDK's version never ran.
//!   Fused scores are rank-based and far below any cosine cut-off, so in a
//!   hybrid search the cut-off is sent as the dense side's score threshold
//!   instead of being applied to the fused score.

use std::collections::HashSet;
use std::future::Future;

use serde_json::Value;

use crate::client::{
    ClaimToken, HybridSearchParams, MAX_LIMIT, SearchParams, VectorClient, toolkit_namespace,
};
use crate::error::{Error, Result};
use crate::filter::{keyword, with_must};
use crate::hit::Hit;
use crate::pb;
use crate::rerank::{self, FieldRerank};

/// The SDK's ceiling on the over-fetch.
const OVERFETCH_CEILING: usize = 30;
/// The chunk types `extended_search` understands besides `document`.
const EXTENDED_CHUNK_TYPES: [&str; 4] = ["title", "summary", "propositions", "keywords"];
/// The SDK's default text weight of a full-text search.
const DEFAULT_TEXT_WEIGHT: f64 = 0.3;

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
    /// Removes every point of one toolkit-index namespace.
    fn delete_namespace(
        &self,
        space: pb::EmbeddingSpace,
        namespace_id: &str,
    ) -> impl Future<Output = Result<()>> + Send;
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
    client: &'a VectorClient,
    token: &'a dyn ClaimToken,
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

    async fn delete_namespace(&self, space: pb::EmbeddingSpace, namespace_id: &str) -> Result<()> {
        self.client
            .delete(
                self.token,
                Some(space),
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
        let enabled = config.get("enabled").is_some_and(is_truthy);
        let has_fields = config.get("fields").is_some_and(is_truthy);
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

/// Python truthiness of a JSON value.
fn is_truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(flag) => *flag,
        Value::Number(number) => number.as_f64().is_some_and(|number| number != 0.0),
        Value::String(text) => !text.is_empty(),
        Value::Array(items) => !items.is_empty(),
        Value::Object(map) => !map.is_empty(),
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
    /// The cut-off on `|score|`; 0 turns it off.
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
        let document_filter = with_must(plan.filter.as_ref(), keyword("chunk_type", "document"));
        raw = fetch(backend, plan, Some(&document_filter), plan.top).await?;
        let mut seen: HashSet<String> = raw.iter().map(|hit| hit.key.clone()).collect();
        let mut types: Vec<&str> = Vec::new();
        for chunk_type in &plan.extended {
            if EXTENDED_CHUNK_TYPES.contains(&chunk_type.as_str())
                && !types.contains(&chunk_type.as_str())
            {
                types.push(chunk_type);
            }
        }
        for chunk_type in types {
            let type_filter = with_must(plan.filter.as_ref(), keyword("chunk_type", chunk_type));
            for found in fetch(backend, plan, Some(&type_filter), plan.top).await? {
                if !seen.insert(found.key.clone()) {
                    continue;
                }
                // Bring in the document chunk this one belongs to.
                let mut document = with_must(
                    plan.filter.as_ref(),
                    keyword("document_key", &found.document_key),
                );
                if !found.chunk_id.is_empty() {
                    document.must.push(keyword("chunk_id", &found.chunk_id));
                }
                document.must.push(keyword("chunk_type", "document"));
                if let Some(first) = fetch(backend, plan, Some(&document), 1)
                    .await?
                    .into_iter()
                    .next()
                {
                    raw.push(first);
                }
            }
        }
    }
    let mut hits = keep_best_duplicates(raw);
    hits.sort_by(|left, right| right.score.total_cmp(&left.score));
    let mut hits = rerank::apply(hits, &plan.rerank)?;
    if plan.cut_off != 0.0 && plan.full_text.is_none() {
        hits.retain(|hit| hit.score.abs() >= plan.cut_off);
    }
    hits.truncate(plan.top);
    Ok(hits)
}

/// One backend call: dense, or hybrid when the plan has full-text weights.
async fn fetch<B: VectorBackend>(
    backend: &B,
    plan: &SearchPlan,
    filter: Option<&pb::Filter>,
    k: usize,
) -> Result<Vec<Hit>> {
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
            let search = SearchParams {
                score_threshold: (plan.cut_off != 0.0).then(|| narrow(plan.cut_off)),
                ..search
            };
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
    Ok(points.iter().map(Hit::from_point).collect())
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
    }

    /// A recorded call: kind, limit, filter, score threshold.
    type Call = (String, u32, Option<pb::Filter>, Option<f32>);

    /// Returns chunks that satisfy the request's `must` conditions, by
    /// score, and records every call.
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
            let wants =
                |key: &str, value: &str| {
                    filter.is_none_or(|filter| {
                        filter.must.iter().filter(|c| c.key == key).all(|c| {
                            c.r#match == Some(pb::condition::Match::Keyword(value.to_owned()))
                        })
                    })
                };
            let mut found: Vec<&Chunk> = self
                .chunks
                .iter()
                .filter(|c| {
                    wants("chunk_type", c.chunk_type)
                        && wants("document_key", c.document_key)
                        && wants("chunk_id", c.chunk_id)
                })
                .collect();
            found.sort_by(|a, b| b.score.total_cmp(&a.score));
            found
                .into_iter()
                .take(limit as usize)
                .map(|c| pb::ScoredPoint {
                    score: c.score,
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
        async fn delete_namespace(&self, _: pb::EmbeddingSpace, _: &str) -> Result<()> {
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
        }
    }

    fn plan() -> SearchPlan {
        SearchPlan {
            scope: crate::client::toolkit_scope(vec!["ns".to_owned()]),
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
        assert!(search_documents(&fake, &p).await.unwrap().is_empty());
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
        // 1 document search + 1 per valid type + 1 document fetch per new key
        // (d2 from title, d3 from keywords; d1's title hit is already known).
        let calls = fake.calls.lock().unwrap();
        assert_eq!(calls.len(), 1 + 2 + 2);
        assert!(calls.iter().all(|call| call.1 <= 6));
        assert_eq!(
            (calls[2].1, calls[4].1),
            (1, 1),
            "a document chunk is fetched with limit 1"
        );
    }

    #[tokio::test]
    async fn extended_search_keeps_the_callers_filter_on_every_call() {
        let fake = Fake {
            chunks: vec![
                chunk("d", "1", "document", 0.5),
                chunk("e", "0", "title", 0.9),
                chunk("e", "0", "document", 0.4),
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
        assert!(fetch.must.contains(&keyword("document_key", "e")));
        assert!(fetch.must.contains(&keyword("chunk_id", "0")));
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
        assert_eq!(
            calls[0].3,
            Some(0.1),
            "the cut-off becomes the dense threshold"
        );
        assert_eq!(
            hits.len(),
            1,
            "a fused score is not cut by the cosine cut-off"
        );
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
}
