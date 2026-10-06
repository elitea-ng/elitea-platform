//! Pass 2: hybrid lexical + vector search fused by Reciprocal Rank Fusion
//! (`graph_orphan_hybrid`).
//!
//! RRF scores a candidate `Σ 1 / (k + rank)` over the lists it appears in,
//! with `k = 60` and a threshold of 0.02. One list contributes at most
//! `1/61 ≈ 0.0164`, so ONLY a node found by both the lexical and the
//! vector search can pass: the threshold makes the pass an intersection.

use super::{Ctx, SearchHit, StoreError};
use crate::graph::pystr;

/// `flags.orphan_rrf_k`.
pub const RRF_K: u32 = 60;
/// `flags.orphan_rrf_threshold`.
pub const RRF_THRESHOLD: f64 = 0.02;
/// `flags.orphan_hybrid_top_n`: each branch's size and the result cap.
pub const HYBRID_TOP_N: usize = 20;
/// `flags.fts_min_score_norm`: lexical hits below are dropped.
pub const FTS_MIN_SCORE_NORM: f64 = 0.15;

/// One fused candidate: the fields the cascade reads from the merged
/// payload.
#[derive(Debug, Clone, PartialEq)]
pub struct FusedHit {
    pub node_id: String,
    pub rrf_score: f64,
    /// From the lexical hit (a dense hit carries none, and a later list's
    /// payload only overrides the keys it has).
    pub fts_rank: Option<f64>,
}

/// `rrf_fuse(*ranked_lists, k=k)`: candidates in first-seen order, then
/// sorted by `(rrf_score, node_id)` descending.
#[must_use]
pub fn rrf_fuse(lists: &[&[SearchHit]], k: u32) -> Vec<FusedHit> {
    let mut fused: Vec<FusedHit> = Vec::new();
    let mut position: std::collections::HashMap<&str, usize> = std::collections::HashMap::new();
    for list in lists {
        for (index, hit) in list.iter().enumerate() {
            // `1.0 / (k + rank)`: an int sum, then a float division.
            let contribution = 1.0 / super::pynum::count_f64(k as usize + index + 1);
            if let Some(&at) = position.get(hit.node_id.as_str()) {
                let entry = &mut fused[at];
                entry.rrf_score += contribution;
                if hit.fts_rank.is_some() {
                    entry.fts_rank = hit.fts_rank;
                }
            } else {
                position.insert(&hit.node_id, fused.len());
                fused.push(FusedHit {
                    node_id: hit.node_id.clone(),
                    rrf_score: contribution,
                    fts_rank: hit.fts_rank,
                });
            }
        }
    }
    fused.sort_by(|a, b| {
        b.rrf_score
            .total_cmp(&a.rrf_score)
            .then_with(|| b.node_id.cmp(&a.node_id))
    });
    fused
}

/// The orphan's symbol name: the stored row's, else the graph's.
pub(crate) fn orphan_symbol_name<'c>(ctx: &'c Ctx<'_>, id: &str) -> &'c str {
    if let Some(row) = ctx.row(id)
        && !row.symbol_name.is_empty()
    {
        return &row.symbol_name;
    }
    ctx.graph.node(id).map_or("", |n| n.symbol_name.as_str())
}

/// The orphan's path: the stored row's, else the graph's.
pub(crate) fn orphan_rel_path<'c>(ctx: &'c Ctx<'_>, id: &str) -> &'c str {
    if let Some(row) = ctx.row(id)
        && !row.rel_path.is_empty()
    {
        return &row.rel_path;
    }
    ctx.graph.node(id).map_or("", |n| &*n.rel_path)
}

/// `_local_dir`: the path before the last `/`, or `""`.
#[must_use]
pub fn local_dir(rel_path: &str) -> &str {
    rel_path.rsplit_once('/').map_or("", |(dir, _)| dir)
}

/// The text the embedding fallback embeds: the row's source text, else
/// its docstring, else the graph's; `None` when shorter than 10
/// characters once stripped.
fn fallback_text(ctx: &Ctx<'_>, id: &str) -> Option<String> {
    let mut text = String::new();
    if let Some(row) = ctx.row(id) {
        text = row
            .source_text
            .clone()
            .filter(|t| !t.is_empty())
            .or_else(|| row.docstring.clone())
            .unwrap_or_default();
    }
    if text.is_empty()
        && let Some(node) = ctx.graph.node(id)
    {
        text = if node.source_text.is_empty() {
            node.docstring.clone()
        } else {
            node.source_text.clone()
        };
    }
    (pystr::strip(&text).chars().count() >= 10).then_some(text)
}

/// `resolve_orphans_hybrid` for one orphan. `stored` is the orphan's
/// stored vector (`collect_orphan_embeddings`). When there is none and the
/// embedder fails, the orphan has no vector (a warning is logged) and no
/// hybrid hits, as in Python.
///
/// # Errors
///
/// The store's.
pub fn resolve_orphan_hybrid(
    ctx: &mut Ctx<'_>,
    id: &str,
    stored: Option<Vec<f64>>,
) -> Result<Vec<FusedHit>, StoreError> {
    let name = orphan_symbol_name(ctx, id).to_owned();
    if name.chars().count() < 2 {
        return Ok(Vec::new());
    }
    let local = local_dir(orphan_rel_path(ctx, id)).to_owned();
    let prefix = (!local.is_empty()).then_some(local.as_str());

    let mut lexical = ctx.store.search_lexical(&name, prefix, HYBRID_TOP_N)?;
    lexical.retain(|hit| {
        hit.node_id != id && hit.score_norm.is_none_or(|norm| norm >= FTS_MIN_SCORE_NORM)
    });

    let embedding = match stored {
        Some(vector) => Some(vector),
        None => match (fallback_text(ctx, id), ctx.embedder.as_deref_mut()) {
            (Some(text), Some(embedder)) => {
                let vector = embedder.embed(&text).ok();
                if vector.is_none() {
                    // Python's `embedding_fn` failing meant "no vector": the
                    // orphan goes on to the lexical pass. The error text is
                    // not logged: a model error can quote its request.
                    tracing::warn!(node_id = %id, "orphan embedding failed; no vector for it");
                }
                vector
            }
            _ => None,
        },
    };
    let Some(embedding) = embedding else {
        return Ok(Vec::new());
    };
    let mut dense = Vec::new();
    if !embedding.is_empty() {
        dense = ctx.store.search_dense(&embedding, HYBRID_TOP_N, prefix)?;
        dense.retain(|hit| hit.node_id != id);
    }

    let mut fused = rrf_fuse(&[&lexical, &dense], RRF_K);
    fused.retain(|hit| hit.rrf_score >= RRF_THRESHOLD);
    fused.truncate(HYBRID_TOP_N);
    Ok(fused)
}

#[cfg(test)]
#[allow(clippy::float_cmp)] // bit-exact parity is the point
mod tests {
    use super::*;

    fn hit(id: &str, fts_rank: Option<f64>) -> SearchHit {
        SearchHit {
            node_id: id.to_owned(),
            fts_rank,
            ..SearchHit::default()
        }
    }

    #[test]
    fn only_a_candidate_in_both_lists_passes_the_threshold() {
        let lexical = [hit("a", Some(-3.0)), hit("b", Some(-2.0))];
        let dense = [hit("c", None), hit("b", None)];
        let fused = rrf_fuse(&[&lexical, &dense], RRF_K);
        let ids: Vec<&str> = fused.iter().map(|h| h.node_id.as_str()).collect();
        // b: 1/62 + 1/62; then a and c tie at 1/61 and order by id, descending.
        assert_eq!(ids, ["b", "c", "a"]);
        assert_eq!(fused[0].rrf_score, 1.0 / 62.0 + 1.0 / 62.0);
        assert_eq!(fused[0].fts_rank, Some(-2.0));
        let passing: Vec<&str> = fused
            .iter()
            .filter(|h| h.rrf_score >= RRF_THRESHOLD)
            .map(|h| h.node_id.as_str())
            .collect();
        assert_eq!(passing, ["b"]);
    }

    #[test]
    fn local_dir_is_the_part_before_the_last_slash() {
        assert_eq!(local_dir("a/b/c.py"), "a/b");
        assert_eq!(local_dir("c.py"), "");
        assert_eq!(local_dir(""), "");
    }
}
