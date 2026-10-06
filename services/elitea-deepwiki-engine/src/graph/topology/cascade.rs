//! Orphan resolution: `resolve_orphans`, Mode A (`_resolve_orphans_v2`),
//! with Pass 1 from `graph_orphan_cascade_v2` and Pass 4
//! `_resolve_orphans_by_directory`.
//!
//! Python quirks kept, because they decide which edges exist:
//!
//! * Under the `calibrated` profile an orphan is a node with in-degree 0,
//!   and every synthetic edge goes FROM the orphan. So after Passes 1–3 a
//!   resolved orphan is still an orphan, and Pass 4 gives it a directory
//!   edge too: nearly every orphan gets one.
//! * Pass 1 marks an orphan resolved when it had hits, even if none of
//!   them became an edge.
//! * `stats["resolved"]` counts orphans that are no longer isolated (in-
//!   AND out-degree 0, whatever the profile).

use super::{CalibrationProfile, Ctx, StoreError, find_orphans, synthetic_edge};
use crate::graph::markdown_structure as links;
use crate::graph::pystr;
use serde_json::{Map, Value, json};
use std::collections::{HashMap, HashSet};

/// The edge budget per orphan of the hybrid and lexical passes
/// (`max_lexical_edges`).
pub const MAX_LEXICAL_EDGES: usize = 2;

/// The counters `resolve_orphans` returns, in Python's key order.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct OrphanStats {
    pub orphans_found: usize,
    pub lexical: usize,
    pub semantic: usize,
    pub directory: usize,
    pub explicit_ref: usize,
    pub hybrid: usize,
    pub resolved: usize,
    pub orphans_remaining: usize,
}

impl OrphanStats {
    /// The stats dict, with `_sync_legacy_aliases` applied.
    #[must_use]
    pub fn to_value(&self) -> Value {
        let mut map = Map::new();
        let mut put = |key: &str, value: usize| {
            map.insert(key.to_owned(), json!(value));
        };
        put("orphans_found", self.orphans_found);
        put("orphan_count", self.orphans_found);
        put("lexical", self.lexical);
        put("semantic", self.semantic);
        put("directory", self.directory);
        put("explicit_ref", self.explicit_ref);
        put("hybrid", self.hybrid);
        put("resolved", self.resolved);
        put("orphans_remaining", self.orphans_remaining);
        put("lexical_edges_added", self.lexical);
        put("semantic_edges_added", self.semantic);
        put("directory_edges_added", self.directory);
        put("explicit_ref_edges_added", self.explicit_ref);
        put("hybrid_edges_added", self.hybrid);
        put("orphans_resolved", self.resolved);
        Value::Object(map)
    }
}

/// `resolve_orphans` (Mode A, the only mode Python runs).
///
/// # Errors
///
/// The store's, including a `get_embeddings` answer with the wrong length.
/// An embedder failure is not an error: that orphan has no vector.
pub fn resolve_orphans(ctx: &mut Ctx<'_>) -> Result<Value, StoreError> {
    let orphans = find_orphans(ctx.graph, ctx.profile, false);
    let mut stats = OrphanStats {
        orphans_found: orphans.len(),
        ..OrphanStats::default()
    };
    if orphans.is_empty() {
        return Ok(stats.to_value());
    }
    let orphan_refs: Vec<&str> = orphans.iter().map(String::as_str).collect();
    // Every pass reads each orphan's stored row (`db.get_node`).
    ctx.fetch_rows(&orphan_refs)?;
    let mut resolved: HashSet<String> = HashSet::new();

    explicit_ref_pass(ctx, &orphans, &mut resolved, &mut stats);

    let pending: Vec<&str> = orphan_refs
        .iter()
        .copied()
        .filter(|id| !resolved.contains(*id))
        .collect();
    if !pending.is_empty() {
        // `collect_orphan_embeddings`, then Pass 2 per orphan.
        let embeddings = stored_embeddings(ctx, &pending)?;
        for &id in &pending {
            let stored = embeddings.get(id).cloned().flatten();
            let hits = super::hybrid::resolve_orphan_hybrid(ctx, id, stored)?;
            let mut added = 0;
            for hit in hits.iter().take(MAX_LEXICAL_EDGES) {
                if hit.node_id.is_empty() || hit.node_id == id || !ctx.graph.has_node(&hit.node_id)
                {
                    continue;
                }
                let provenance = json!({
                    "source": "hybrid_rrf",
                    "rrf_score": hit.rrf_score,
                    "fts_rank": hit.fts_rank,
                    "vec_rank": Value::Null,
                });
                ctx.graph.add_edge(
                    id,
                    &hit.node_id,
                    synthetic_edge(
                        "hybrid_link",
                        "semantic",
                        "hybrid_rrf",
                        Some(hit.rrf_score),
                        Some(provenance),
                    ),
                );
                stats.hybrid += 1;
                added += 1;
            }
            if added > 0 {
                resolved.insert(id.to_owned());
            }
        }
    }

    let pending: Vec<&str> = orphan_refs
        .iter()
        .copied()
        .filter(|id| !resolved.contains(*id))
        .collect();
    for &id in &pending {
        let hits = super::lexical::resolve_orphan_tiered(ctx, id)?;
        if hits.is_empty() {
            continue;
        }
        for hit in &hits {
            if hit.hit.node_id.is_empty()
                || hit.hit.node_id == id
                || !ctx.graph.has_node(&hit.hit.node_id)
            {
                continue;
            }
            let provenance = json!({
                "source": "fts_lexical_tiered",
                "tier": hit.tier,
                "score_norm": hit.hit.score_norm,
            });
            ctx.graph.add_edge(
                id,
                &hit.hit.node_id,
                synthetic_edge(
                    "lexical_link",
                    "lexical",
                    "fts5_lexical_v2",
                    None,
                    Some(provenance),
                ),
            );
            stats.lexical += 1;
        }
        resolved.insert(id.to_owned());
    }

    let remaining = find_orphans(ctx.graph, ctx.profile, false);
    if !remaining.is_empty() {
        stats.directory = resolve_by_directory(ctx, &remaining)?;
    }
    stats.orphans_remaining = find_orphans(ctx.graph, CalibrationProfile::Legacy, true).len();
    stats.resolved = stats.orphans_found.saturating_sub(stats.orphans_remaining);
    Ok(stats.to_value())
}

/// `collect_orphan_embeddings`: the stored vector of each of `ids`. An
/// answer of the wrong length is an error, as in [`Ctx::fetch_rows`]:
/// pairing it with the ids would shift or drop vectors.
fn stored_embeddings<'i>(
    ctx: &mut Ctx<'_>,
    ids: &[&'i str],
) -> Result<HashMap<&'i str, Option<Vec<f64>>>, StoreError> {
    let embeddings = ctx.store.get_embeddings(ids)?;
    if embeddings.len() != ids.len() {
        return Err(StoreError::new(format!(
            "get_embeddings returned {} vectors for {} ids",
            embeddings.len(),
            ids.len()
        )));
    }
    Ok(ids.iter().copied().zip(embeddings).collect())
}

/// One explicit-reference hit (`{"node_id", "_matcher", "_raw_score"}`).
struct ExplicitHit {
    node_id: String,
    matcher: &'static str,
}

/// Pass 1: `resolve_orphans_explicit_refs`, then its edges.
fn explicit_ref_pass(
    ctx: &mut Ctx<'_>,
    orphans: &[String],
    resolved: &mut HashSet<String>,
    stats: &mut OrphanStats,
) {
    let path_index = links::build_path_index(ctx.graph);
    let name_index = links::build_simple_name_index(ctx.graph);
    let mut found: Vec<(&str, Vec<ExplicitHit>)> = Vec::new();
    for id in orphans {
        let Some(node) = ctx.graph.node(id) else {
            continue;
        };
        let row = ctx.row(id);
        let hits = if links::is_doc_node(node) {
            // `db_node.get("source_text", "") or node_data.get(...)`.
            let mut source_text = row
                .and_then(|r| r.source_text.as_deref())
                .unwrap_or_default();
            if source_text.is_empty() {
                source_text = &node.source_text;
            }
            if source_text.is_empty() {
                continue;
            }
            let rel_path = row
                .map(|r| r.rel_path.as_str())
                .filter(|p| !p.is_empty())
                .unwrap_or(&node.rel_path);
            let source_dir = pystr::dirname(rel_path);
            links::resolve_doc_links(id, source_text, source_dir, &path_index, &name_index)
                .into_iter()
                .map(|hit| ExplicitHit {
                    node_id: hit.node_id,
                    matcher: hit.matcher,
                })
                .collect()
        } else {
            code_orphan_imports(id, node, &path_index, &name_index)
        };
        if !hits.is_empty() {
            found.push((id, hits));
        }
    }
    for (id, hits) in found {
        for hit in &hits {
            if hit.node_id.is_empty() || hit.node_id == id || !ctx.graph.has_node(&hit.node_id) {
                continue;
            }
            let edge_class = if hit.matcher == "md_link" {
                "doc"
            } else {
                "lexical"
            };
            let provenance = json!({
                "source": "explicit_ref",
                "matcher": hit.matcher,
                "raw_score": 0.95,
            });
            ctx.graph.add_edge(
                id,
                &hit.node_id,
                synthetic_edge(
                    "explicit_ref",
                    edge_class,
                    "explicit_ref_v2",
                    Some(0.95),
                    Some(provenance),
                ),
            );
            stats.explicit_ref += 1;
        }
        resolved.insert(id.to_owned());
    }
}

/// `_resolve_code_orphan_imports`: the parser's `imports` list, when a node
/// carries one (the index has no such column, so only the graph's). Each
/// entry, a string or the `rel_path` / `module` / `name` of a dict, is a
/// path in the index, else a symbol name (its last dotted part, at most
/// two nodes).
fn code_orphan_imports(
    id: &str,
    node: &crate::graph::NodeData,
    path_index: &HashMap<String, String>,
    name_index: &HashMap<String, Vec<String>>,
) -> Vec<ExplicitHit> {
    let mut out = Vec::new();
    let Some(Value::Array(imports)) = node.extra.get("imports") else {
        return out;
    };
    let mut seen: HashSet<&str> = HashSet::new();
    for import in imports {
        let mut candidates: Vec<&str> = Vec::new();
        match import {
            Value::String(text) => candidates.push(text),
            Value::Object(map) => {
                for key in ["rel_path", "module", "name"] {
                    if let Some(Value::String(text)) = map.get(key)
                        && !text.is_empty()
                    {
                        candidates.push(text);
                    }
                }
            }
            _ => {}
        }
        for candidate in candidates {
            if let Some(target) = path_index.get(candidate)
                && target != id
                && !seen.contains(target.as_str())
            {
                seen.insert(target);
                out.push(ExplicitHit {
                    node_id: target.clone(),
                    matcher: "import_path",
                });
                continue;
            }
            let symbol = candidate.rsplit('.').next().unwrap_or("");
            for target in name_index.get(symbol).into_iter().flatten().take(2) {
                if target == id || seen.contains(target.as_str()) {
                    continue;
                }
                seen.insert(target);
                out.push(ExplicitHit {
                    node_id: target.clone(),
                    matcher: "import_symbol",
                });
            }
        }
    }
    out
}

/// `_expanding_prefixes`: the file's directory, each parent, then `""`.
/// Backslashes count as separators here but NOT in the directory index
/// (`os.path.dirname`), as in Python.
#[must_use]
pub fn expanding_prefixes(rel_path: &str) -> Vec<String> {
    let normalised = rel_path.replace('\\', "/");
    let parts: Vec<&str> = normalised.split('/').collect();
    let mut dir_parts: &[&str] = if parts.len() > 1 {
        &parts[..parts.len() - 1]
    } else {
        &[]
    };
    let mut prefixes = Vec::new();
    while !dir_parts.is_empty() {
        prefixes.push(dir_parts.join("/"));
        dir_parts = &dir_parts[..dir_parts.len() - 1];
    }
    prefixes.push(String::new());
    prefixes
}

/// Pass 4: `_resolve_orphans_by_directory`. Each orphan links to the
/// highest-degree non-orphan of its directory, else of the nearest parent
/// directory, else of the root (`<root>`).
fn resolve_by_directory(ctx: &mut Ctx<'_>, orphans: &[String]) -> Result<usize, StoreError> {
    let orphan_set: HashSet<&str> = orphans.iter().map(String::as_str).collect();
    // The stored `rel_path` of every non-orphan whose graph node has none.
    let need_rows: Vec<String> = ctx
        .graph
        .nodes()
        .filter(|(id, data)| !orphan_set.contains(id) && data.rel_path.is_empty())
        .map(|(id, _)| id.to_owned())
        .collect();
    let refs: Vec<&str> = need_rows.iter().map(String::as_str).collect();
    ctx.fetch_rows(&refs)?;

    let table = super::degrees(ctx.graph);
    let mut dir_index: HashMap<String, Vec<(String, usize)>> = HashMap::new();
    for (id, data) in ctx.graph.nodes() {
        if orphan_set.contains(id) {
            continue;
        }
        let rel_path: &str = if data.rel_path.is_empty() {
            ctx.row(id).map_or("", |r| r.rel_path.as_str())
        } else {
            &data.rel_path
        };
        if rel_path.is_empty() {
            continue;
        }
        let dir = pystr::dirname(rel_path).replace('\\', "/");
        let dir = if dir.is_empty() {
            "<root>".to_owned()
        } else {
            dir
        };
        let (inbound, outbound) = table.get(id).copied().unwrap_or_default();
        dir_index
            .entry(dir)
            .or_default()
            .push((id.to_owned(), inbound + outbound));
    }
    for bucket in dir_index.values_mut() {
        // Stable: equal degrees keep node order (Python's `sort(reverse=True)`).
        bucket.sort_by_key(|entry| std::cmp::Reverse(entry.1));
    }

    let mut added = 0;
    let mut new_edges: Vec<(String, String)> = Vec::new();
    for orphan in orphans {
        let Some(row) = ctx.row(orphan) else {
            continue;
        };
        if row.rel_path.is_empty() {
            continue;
        }
        for prefix in expanding_prefixes(&row.rel_path) {
            let dir = if prefix.is_empty() { "<root>" } else { &prefix };
            if let Some((anchor, _)) = dir_index.get(dir).and_then(|bucket| bucket.first()) {
                new_edges.push((orphan.clone(), anchor.clone()));
                added += 1;
                break;
            }
        }
    }
    for (source, target) in new_edges {
        ctx.graph.add_edge(
            &source,
            &target,
            synthetic_edge(
                "directory_link",
                "directory",
                "dir_proximity_fallback",
                None,
                None,
            ),
        );
    }
    Ok(added)
}

#[cfg(test)]
#[allow(clippy::float_cmp)] // bit-exact parity is the point
mod tests {
    use super::*;

    use crate::graph::topology::{SearchHit, StoredNode, TextEmbedder, TopologyStore};
    use crate::graph::{CodeGraph, EdgeData, EdgeRow, NodeData};

    /// An index that knows three rows and finds `t` for every search.
    #[derive(Default)]
    struct FakeStore {
        /// `get_embeddings` answers one vector fewer than asked.
        short_embeddings: bool,
        /// `search_dense` fails.
        dense_fails: bool,
    }

    impl TopologyStore for FakeStore {
        fn get_nodes(&mut self, ids: &[&str]) -> Result<Vec<Option<StoredNode>>, StoreError> {
            Ok(ids
                .iter()
                .map(|id| {
                    (["o", "t", "x"].contains(id)).then(|| StoredNode {
                        symbol_name: if *id == "o" { "Widget" } else { "x" }.to_owned(),
                        rel_path: format!("src/{id}.py"),
                        source_text: Some("def widget(): return 42".to_owned()),
                        ..StoredNode::default()
                    })
                })
                .collect())
        }

        fn node_count(&mut self) -> Result<u64, StoreError> {
            Ok(10)
        }

        fn count_phrase_matches(&mut self, _query: &str) -> Result<u64, StoreError> {
            Ok(1)
        }

        fn search_lexical(
            &mut self,
            _query: &str,
            _path_prefix: Option<&str>,
            _limit: usize,
        ) -> Result<Vec<SearchHit>, StoreError> {
            Ok(vec![SearchHit {
                node_id: "t".to_owned(),
                rel_path: "src/t.py".to_owned(),
                fts_rank: Some(-1.0),
                score_norm: Some(0.9),
                ..SearchHit::default()
            }])
        }

        fn get_embeddings(&mut self, ids: &[&str]) -> Result<Vec<Option<Vec<f64>>>, StoreError> {
            let answered = ids.len() - usize::from(self.short_embeddings && !ids.is_empty());
            Ok(vec![None; answered])
        }

        fn search_dense(
            &mut self,
            _embedding: &[f64],
            _k: usize,
            _path_prefix: Option<&str>,
        ) -> Result<Vec<SearchHit>, StoreError> {
            if self.dense_fails {
                return Err(StoreError::new("vector index down"));
            }
            Ok(vec![SearchHit {
                node_id: "t".to_owned(),
                vec_distance: Some(0.1),
                ..SearchHit::default()
            }])
        }

        fn set_hubs(&mut self, _hubs: &[&str]) -> Result<(), StoreError> {
            Ok(())
        }

        fn replace_edges(
            &mut self,
            _rows: &mut dyn Iterator<Item = EdgeRow>,
        ) -> Result<u64, StoreError> {
            Ok(0)
        }

        fn set_meta(&mut self, _key: &str, _value: &Value) -> Result<(), StoreError> {
            Ok(())
        }
    }

    /// An embedder that always fails, counting the calls.
    #[derive(Default)]
    struct FailingEmbedder {
        calls: usize,
    }

    impl TextEmbedder for FailingEmbedder {
        fn embed(&mut self, _text: &str) -> Result<Vec<f64>, StoreError> {
            self.calls += 1;
            Err(StoreError::new("model unavailable"))
        }
    }

    /// `o` is an orphan with no stored vector; `x → t` makes `t` a target.
    fn orphan_graph() -> CodeGraph {
        let mut graph = CodeGraph::new();
        graph.add_node(
            "o",
            NodeData {
                symbol_name: "Widget".to_owned(),
                rel_path: "src/o.py".into(),
                ..NodeData::default()
            },
        );
        for id in ["t", "x"] {
            graph.add_node(
                id,
                NodeData {
                    symbol_name: id.to_owned(),
                    rel_path: format!("src/{id}.py").as_str().into(),
                    ..NodeData::default()
                },
            );
        }
        graph.add_edge("x", "t", EdgeData::default());
        graph
    }

    /// The embedder a run gets.
    #[derive(Clone, Copy)]
    enum Model {
        Absent,
        Failing,
        Refusing,
        Standin,
    }

    /// An embedder whose model service refuses the credential.
    struct RefusingEmbedder;

    impl TextEmbedder for RefusingEmbedder {
        fn embed(&mut self, _text: &str) -> Result<Vec<f64>, StoreError> {
            Err(StoreError::from_engine(crate::errors::EngineError::new(
                crate::errors::ErrorType::Value,
                "The model budget is exhausted: HTTP 402 for model 'e'",
            )))
        }
    }

    /// What one `resolve_orphans` run gave.
    #[derive(Debug, PartialEq)]
    struct Run {
        stats: Value,
        edges: Vec<(String, String, String)>,
        embed_calls: usize,
    }

    fn resolve(store: &mut FakeStore, model: Model) -> Result<Run, StoreError> {
        let mut graph = orphan_graph();
        let mut failing = FailingEmbedder::default();
        let mut standin = super::super::replay::StandinEmbedder;
        let mut refusing = RefusingEmbedder;
        let stats = {
            let embedder: Option<&mut dyn TextEmbedder> = match model {
                Model::Absent => None,
                Model::Failing => Some(&mut failing),
                Model::Refusing => Some(&mut refusing),
                Model::Standin => Some(&mut standin),
            };
            let mut ctx = Ctx::new(&mut graph, store, embedder, CalibrationProfile::Calibrated);
            resolve_orphans(&mut ctx)?
        };
        let edges = graph
            .edges()
            .map(|e| {
                (
                    e.source.to_owned(),
                    e.target.to_owned(),
                    e.data.rel_type.to_string(),
                )
            })
            .collect();
        Ok(Run {
            stats,
            edges,
            embed_calls: failing.calls,
        })
    }

    #[test]
    fn a_failing_embedder_leaves_the_orphan_without_a_vector() {
        let failed = resolve(&mut FakeStore::default(), Model::Failing).unwrap();
        assert!(failed.embed_calls > 0, "the fallback embedding was tried");
        // The same as having no embedder at all, as in Python.
        let without = resolve(&mut FakeStore::default(), Model::Absent).unwrap();
        assert_eq!(
            (&failed.stats, &failed.edges),
            (&without.stats, &without.edges)
        );
        assert_eq!(failed.stats["hybrid"], 0);
        // A working embedder does add the hybrid edge.
        let working = resolve(&mut FakeStore::default(), Model::Standin).unwrap();
        assert_eq!(working.stats["hybrid"], 1);
    }

    #[test]
    fn a_model_service_refusal_fails_the_phase_with_its_type() {
        let error = resolve(&mut FakeStore::default(), Model::Refusing).unwrap_err();
        let cause = error.engine_error().cloned();
        assert_eq!(
            cause.map(|e| (e.error_type, e.category())),
            Some((crate::errors::ErrorType::Value, "invalid_input"))
        );
    }

    #[test]
    fn a_store_failure_still_fails_the_phase() {
        let mut store = FakeStore {
            dense_fails: true,
            ..FakeStore::default()
        };
        let error = resolve(&mut store, Model::Standin).unwrap_err();
        assert_eq!(error, StoreError::new("vector index down"));
    }

    #[test]
    fn get_embeddings_of_the_wrong_length_is_an_error() {
        let mut store = FakeStore {
            short_embeddings: true,
            ..FakeStore::default()
        };
        let error = resolve(&mut store, Model::Absent).unwrap_err();
        assert_eq!(
            error,
            StoreError::new("get_embeddings returned 1 vectors for 2 ids")
        );
    }

    #[test]
    fn prefixes_climb_to_the_root() {
        assert_eq!(
            expanding_prefixes("src/auth/handlers/login.py"),
            ["src/auth/handlers", "src/auth", "src", ""]
        );
        assert_eq!(expanding_prefixes("README.md"), [""]);
        assert_eq!(expanding_prefixes("a\\b\\c.py"), ["a/b", "a", ""]);
        // A leading slash leaves an empty first part, as `str.split` does.
        assert_eq!(expanding_prefixes("/abs/x.py"), ["/abs", "", ""]);
    }

    #[test]
    fn the_stats_dict_has_python_key_order_and_aliases() {
        let stats = OrphanStats {
            orphans_found: 3,
            lexical: 1,
            resolved: 2,
            orphans_remaining: 1,
            ..OrphanStats::default()
        };
        assert_eq!(
            crate::pyjson::dumps(&stats.to_value()),
            "{\"orphans_found\": 3, \"orphan_count\": 3, \"lexical\": 1, \"semantic\": 0, \
             \"directory\": 0, \"explicit_ref\": 0, \"hybrid\": 0, \"resolved\": 2, \
             \"orphans_remaining\": 1, \"lexical_edges_added\": 1, \"semantic_edges_added\": 0, \
             \"directory_edges_added\": 0, \"explicit_ref_edges_added\": 0, \
             \"hybrid_edges_added\": 0, \"orphans_resolved\": 2}"
        );
    }
}
