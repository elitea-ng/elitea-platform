//! `StorageQueryService` (`code_graph/storage_query_service.py`): symbol
//! resolution, search, relationship traversal and JQL over an
//! [`IndexStore`].
//!
//! Deliberate differences: a storage failure is an error the tool reports
//! (Python caught the `search_fts5` and `_connection_counts` failures and
//! answered as if nothing matched); a traversal expands at most
//! [`MAX_EXPANSIONS`] nodes, so a crafted `related:`/`max_depth` cannot walk
//! the whole graph.

use super::jql;
use super::store::{IndexStore, NodeRecord};
use super::symbols::classify_symbol_layer;
use crate::errors::EngineError;
use crate::storage::adapter::EdgeRecord;
use serde_json::Value;
use std::collections::{HashMap, HashSet, VecDeque};

/// Nodes one traversal may expand.
pub const MAX_EXPANSIONS: usize = 2_000;

/// `SymbolResult`.
#[derive(Debug, Clone, PartialEq)]
pub struct SymbolResult {
    pub node_id: String,
    pub symbol_name: String,
    pub symbol_type: String,
    pub layer: String,
    pub rel_path: String,
    pub connections: i64,
    pub docstring: String,
}

/// `RelationshipResult`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RelationshipResult {
    pub source_name: String,
    pub target_name: String,
    pub relationship_type: String,
    pub source_type: String,
    pub target_type: String,
    pub hop_distance: usize,
    pub edge_class: String,
    pub confidence: String,
    pub via: Vec<String>,
}

/// `EDGE_TYPE_ALIASES`.
const EDGE_TYPE_ALIASES: &[(&str, &str)] = &[
    ("inherits", "inheritance"),
    ("extends", "inheritance"),
    ("implements", "implementation"),
    ("invokes", "calls"),
    ("has_member", "contains"),
    ("has_method", "contains"),
    ("composes", "composition"),
    ("constructs", "creates"),
    ("accesses", "reads"),
    ("modifies", "writes"),
    ("file_imports", "imports"),
    ("implements_interface", "implementation"),
    ("field_type", "uses_type"),
];

fn edge_type(edge: &EdgeRecord) -> String {
    if edge.rel_type.is_empty() {
        "related".to_owned()
    } else {
        edge.rel_type.to_lowercase()
    }
}

/// `_edge_via`: the `via` anchors of the edge's annotations (the record's
/// `metadata`).
fn edge_via(edge: &EdgeRecord) -> Vec<String> {
    match edge.metadata.get("via") {
        Some(Value::String(text)) => {
            let text = crate::graph::pystr::strip(text);
            if text.is_empty() {
                Vec::new()
            } else {
                vec![text.to_owned()]
            }
        }
        Some(Value::Array(items)) => items
            .iter()
            .map(super::pyfmt::str_of)
            .map(|text| crate::graph::pystr::strip(&text).to_owned())
            .filter(|text| !text.is_empty())
            .collect(),
        _ => Vec::new(),
    }
}

fn edge_confidence(edge: &EdgeRecord) -> String {
    match edge.metadata.get("confidence") {
        None | Some(Value::Null) => String::new(),
        Some(Value::String(text)) => text.clone(),
        Some(other) if super::pyfmt::truthy(other) => super::pyfmt::str_of(other),
        Some(_) => String::new(),
    }
}

fn merge_provenance(existing: &mut RelationshipResult, edge: &EdgeRecord) {
    for anchor in edge_via(edge) {
        if !existing.via.contains(&anchor) {
            existing.via.push(anchor);
        }
    }
    if existing.confidence.is_empty() {
        existing.confidence = edge_confidence(edge);
    }
}

fn normalise_edge_types(edge_types: &[String]) -> HashSet<String> {
    let mut expanded = HashSet::new();
    for edge_type in edge_types {
        let lowered = edge_type.to_lowercase();
        for (alias, canonical) in EDGE_TYPE_ALIASES {
            if *alias == lowered {
                expanded.insert((*canonical).to_owned());
            }
            if *canonical == lowered {
                expanded.insert((*alias).to_owned());
            }
        }
        expanded.insert(lowered);
    }
    expanded
}

/// `_matches_path`: a glob pattern by `fnmatch`, else a path prefix.
fn matches_path(path: &str, pattern: &str) -> bool {
    if path.is_empty() || pattern.is_empty() {
        return false;
    }
    if pattern.contains('*') || pattern.contains('?') {
        return crate::graph::pystr::fnmatch(path, pattern);
    }
    let prefix = pattern.trim_end_matches('/');
    path == prefix || path.starts_with(&format!("{prefix}/"))
}

/// The search filters (`_matches_filters`).
#[derive(Debug, Clone, Default)]
pub struct Filters<'a> {
    pub symbol_types: Option<&'a HashSet<String>>,
    pub exclude_types: Option<&'a [&'a str]>,
    pub layer: Option<&'a str>,
    pub path_prefix: Option<&'a str>,
}

impl Filters<'_> {
    fn matches(&self, result: &SymbolResult) -> bool {
        if self
            .symbol_types
            .is_some_and(|t| !t.is_empty() && !t.contains(&result.symbol_type))
        {
            return false;
        }
        if self
            .exclude_types
            .is_some_and(|t| t.contains(&result.symbol_type.as_str()))
        {
            return false;
        }
        if self
            .layer
            .is_some_and(|l| !l.is_empty() && l != result.layer)
        {
            return false;
        }
        if self
            .path_prefix
            .is_some_and(|p| !p.is_empty() && !matches_path(&result.rel_path, p))
        {
            return false;
        }
        true
    }
}

fn sorted_types(types: Option<&HashSet<String>>) -> Option<Vec<String>> {
    types.filter(|t| !t.is_empty()).map(|t| {
        let mut sorted: Vec<String> = t.iter().cloned().collect();
        sorted.sort();
        sorted
    })
}

/// `_row_to_result`.
#[must_use]
pub fn row_to_result(row: &NodeRecord) -> SymbolResult {
    let symbol_type = row.symbol_type.to_lowercase();
    let layer = classify_symbol_layer(
        &symbol_type,
        &row.symbol_name,
        row.parent_symbol.as_deref().unwrap_or(""),
        &row.rel_path,
    );
    SymbolResult {
        node_id: row.node_id.clone(),
        symbol_name: row.symbol_name.clone(),
        symbol_type,
        layer: layer.to_owned(),
        rel_path: row.rel_path.clone(),
        connections: 0,
        docstring: row.docstring.clone(),
    }
}

/// `_select_best`: the row of `file_path` / `language` when given, else the
/// first of the architectural rows with the longest span (Python `max`).
fn select_best<'r>(
    rows: &'r [NodeRecord],
    file_path: &str,
    language: &str,
) -> Option<&'r NodeRecord> {
    if rows.is_empty() {
        return None;
    }
    if !file_path.is_empty() || !language.is_empty() {
        for row in rows {
            if !file_path.is_empty() && row.rel_path == file_path {
                return Some(row);
            }
            if !language.is_empty() && row.language.to_lowercase() == language.to_lowercase() {
                return Some(row);
            }
        }
    }
    let key = |row: &NodeRecord| {
        (
            i64::from(row.is_architectural),
            i64::from(row.end_line) - i64::from(row.start_line),
        )
    };
    let mut best = &rows[0];
    for row in &rows[1..] {
        if key(row) > key(best) {
            best = row;
        }
    }
    Some(best)
}

/// The query service of one index.
pub struct QueryService<'s, S: IndexStore> {
    pub store: &'s S,
}

impl<S: IndexStore> QueryService<'_, S> {
    /// `get_node`.
    ///
    /// # Errors
    ///
    /// A storage failure.
    pub async fn get_node(&self, node_id: &str) -> Result<Option<NodeRecord>, EngineError> {
        if node_id.is_empty() {
            return Ok(None);
        }
        self.store.get_node(node_id).await
    }

    /// `resolve_symbol`.
    ///
    /// # Errors
    ///
    /// A storage failure.
    pub async fn resolve_symbol(
        &self,
        symbol_name: &str,
        file_path: &str,
    ) -> Result<Option<String>, EngineError> {
        let name = crate::graph::pystr::strip(symbol_name);
        if name.is_empty() {
            return Ok(None);
        }
        if self.get_node(name).await?.is_some() {
            return Ok(Some(name.to_owned()));
        }
        let rows = self.store.name_rows(name, true, 15).await?;
        if let Some(best) = select_best(&rows, file_path, "") {
            return Ok(Some(best.node_id.clone()));
        }
        let simple = name.rsplit('.').next().unwrap_or(name);
        let simple = simple.rsplit("::").next().unwrap_or(simple);
        if simple != name {
            let rows = self.store.name_rows(simple, true, 15).await?;
            if let Some(best) = select_best(&rows, file_path, "") {
                return Ok(Some(best.node_id.clone()));
            }
        }
        let rows = self.store.name_rows(name, false, 15).await?;
        if let Some(best) = select_best(&rows, file_path, "") {
            return Ok(Some(best.node_id.clone()));
        }
        let rows: Vec<NodeRecord> = self
            .store
            .search_fts(name, None, None, 5)
            .await?
            .into_iter()
            .map(|row| row.node)
            .collect();
        Ok(select_best(&rows, file_path, "").map(|row| row.node_id.clone()))
    }

    /// `search`.
    ///
    /// # Errors
    ///
    /// A storage failure.
    pub async fn search(
        &self,
        query: &str,
        k: usize,
        filters: &Filters<'_>,
    ) -> Result<Vec<SymbolResult>, EngineError> {
        if crate::graph::pystr::strip(query).is_empty() {
            return Ok(Vec::new());
        }
        let pool = k.saturating_mul(3).max(k);
        let prefix = filters
            .path_prefix
            .filter(|p| !p.is_empty() && !p.contains('*'));
        let types = sorted_types(filters.symbol_types);
        let mut rows: Vec<NodeRecord> = self
            .store
            .search_fts(query, prefix, types.as_deref(), pool)
            .await?
            .into_iter()
            .map(|row| row.node)
            .collect();
        if rows.is_empty() {
            rows = self.store.name_rows(query, false, pool).await?;
        }
        let mut results = Vec::new();
        for row in &rows {
            let result = row_to_result(row);
            if !filters.matches(&result) {
                continue;
            }
            results.push(result);
            if results.len() >= k {
                break;
            }
        }
        self.count_connections(&mut results).await?;
        Ok(results)
    }

    async fn count_connections(&self, results: &mut [SymbolResult]) -> Result<(), EngineError> {
        let mut ids: Vec<String> = Vec::new();
        for result in results.iter() {
            if !result.node_id.is_empty() && !ids.contains(&result.node_id) {
                ids.push(result.node_id.clone());
            }
        }
        if ids.is_empty() {
            return Ok(());
        }
        let counts = self.store.connection_counts(&ids).await?;
        for result in results.iter_mut() {
            result.connections = counts.get(&result.node_id).copied().unwrap_or(0);
        }
        Ok(())
    }

    async fn nodes_by_id(
        &self,
        ids: &HashSet<String>,
    ) -> Result<HashMap<String, NodeRecord>, EngineError> {
        let ids: Vec<String> = ids.iter().filter(|id| !id.is_empty()).cloned().collect();
        if ids.is_empty() {
            return Ok(HashMap::new());
        }
        Ok(self
            .store
            .nodes_by_ids(&ids)
            .await?
            .into_iter()
            .map(|node| (node.node_id.clone(), node))
            .collect())
    }

    /// `get_relationships`: breadth first, at most `max_results` edges,
    /// sorted by hop distance (stable).
    ///
    /// # Errors
    ///
    /// A storage failure.
    pub async fn get_relationships(
        &self,
        node_id: &str,
        direction: &str,
        max_depth: usize,
        max_results: usize,
    ) -> Result<Vec<RelationshipResult>, EngineError> {
        if node_id.is_empty() || self.get_node(node_id).await?.is_none() {
            return Ok(Vec::new());
        }
        let outgoing = matches!(direction, "outgoing" | "out" | "both");
        let incoming = matches!(direction, "incoming" | "in" | "both");
        let mut results: Vec<RelationshipResult> = Vec::new();
        let mut seen: HashMap<(String, String, String), usize> = HashMap::new();
        let mut visited: HashSet<String> = HashSet::from([node_id.to_owned()]);
        let mut frontier: VecDeque<(String, usize)> = VecDeque::from([(node_id.to_owned(), 0)]);
        let mut expansions = 0;
        while let Some((current, depth)) = frontier.pop_front() {
            if results.len() >= max_results || expansions >= MAX_EXPANSIONS {
                break;
            }
            if depth >= max_depth {
                continue;
            }
            expansions += 1;
            // (source, target, rel_type, other, edge)
            let mut edges: Vec<(String, String, String, String, EdgeRecord)> = Vec::new();
            if outgoing {
                for edge in self.store.edges_from(&current).await? {
                    if !edge.target_id.is_empty() {
                        let target = edge.target_id.clone();
                        edges.push((
                            current.clone(),
                            target.clone(),
                            edge_type(&edge),
                            target,
                            edge,
                        ));
                    }
                }
            }
            if incoming {
                for edge in self.store.edges_to(&current).await? {
                    if !edge.source_id.is_empty() {
                        let source = edge.source_id.clone();
                        edges.push((
                            source.clone(),
                            current.clone(),
                            edge_type(&edge),
                            source,
                            edge,
                        ));
                    }
                }
            }
            let ids: HashSet<String> = edges
                .iter()
                .flat_map(|(s, t, ..)| [s.clone(), t.clone()])
                .collect();
            let rows = self.nodes_by_id(&ids).await?;
            for (source, target, rel_type, other, edge) in edges {
                if results.len() >= max_results {
                    break;
                }
                let key = (source.clone(), target.clone(), rel_type.clone());
                if let Some(&at) = seen.get(&key) {
                    merge_provenance(&mut results[at], &edge);
                    continue;
                }
                let name = |id: &str| {
                    rows.get(id)
                        .map(|r| r.symbol_name.clone())
                        .filter(|n| !n.is_empty())
                        .unwrap_or_else(|| id.to_owned())
                };
                let kind = |id: &str| {
                    rows.get(id)
                        .map(|r| r.symbol_type.to_lowercase())
                        .unwrap_or_default()
                };
                results.push(RelationshipResult {
                    source_name: name(&source),
                    target_name: name(&target),
                    relationship_type: rel_type,
                    source_type: kind(&source),
                    target_type: kind(&target),
                    hop_distance: depth + 1,
                    edge_class: edge.edge_class.clone().unwrap_or_default(),
                    confidence: edge_confidence(&edge),
                    via: edge_via(&edge),
                });
                seen.insert(key, results.len() - 1);
                if visited.insert(other.clone()) {
                    frontier.push_back((other, depth + 1));
                }
            }
        }
        results.sort_by_key(|r| r.hop_distance);
        Ok(results)
    }

    /// `resolve_and_traverse`.
    ///
    /// # Errors
    ///
    /// A storage failure.
    pub async fn resolve_and_traverse(
        &self,
        symbol_name: &str,
        direction: &str,
        max_depth: usize,
        max_results: usize,
    ) -> Result<Option<(String, Vec<RelationshipResult>)>, EngineError> {
        let Some(node_id) = self.resolve_symbol(symbol_name, "").await? else {
            return Ok(None);
        };
        let rels = self
            .get_relationships(&node_id, direction, max_depth, max_results)
            .await?;
        Ok(Some((node_id, rels)))
    }

    /// `query`: a JQL expression.
    ///
    /// # Errors
    ///
    /// A storage failure.
    pub async fn query(&self, expression: &str) -> Result<Vec<SymbolResult>, EngineError> {
        let jql = jql::parse(expression);
        if jql.is_empty() {
            return Ok(Vec::new());
        }
        let mut results = self.index_clauses(&jql).await?;
        results = self.post_filters(results, &jql).await?;
        results.truncate(jql.limit);
        Ok(results)
    }

    async fn index_clauses(&self, jql: &jql::Query) -> Result<Vec<SymbolResult>, EngineError> {
        let types: HashSet<String> = jql.type_values().into_iter().collect();
        let types_opt = (!types.is_empty()).then_some(&types);
        let layer = jql.layer_value();
        let path = jql.file_value();
        let fetch_k = (jql.limit * 5).max(20);
        let filters = Filters {
            symbol_types: types_opt,
            exclude_types: None,
            layer: layer.as_deref(),
            path_prefix: path,
        };
        if let Some(text) = jql.text_value() {
            let mut results = self.search(text, fetch_k, &filters).await?;
            if let Some(name) = jql.name_value() {
                let name = name.to_lowercase();
                results.retain(|r| r.symbol_name.to_lowercase().contains(&name));
            }
            return Ok(results);
        }
        let rows = if let Some(name) = jql.name_value() {
            let exact = !name.contains('*') && !name.contains('?');
            self.store.name_rows(name, exact, fetch_k).await?
        } else {
            let prefix = path.filter(|p| !p.is_empty() && !p.contains('*') && !p.contains('?'));
            let sorted = sorted_types(types_opt);
            self.store
                .scan_rows(fetch_k, sorted.as_deref(), prefix)
                .await?
        };
        Ok(rows
            .iter()
            .map(row_to_result)
            .filter(|r| filters.matches(r))
            .collect())
    }

    async fn post_filters(
        &self,
        mut results: Vec<SymbolResult>,
        jql: &jql::Query,
    ) -> Result<Vec<SymbolResult>, EngineError> {
        let has_rel = jql.has_rel_values();
        if let Some(related) = jql.related_value() {
            results = self
                .filter_related(results, related, &jql.direction_value(), &has_rel)
                .await?;
        } else if !has_rel.is_empty() {
            let allowed = normalise_edge_types(&has_rel);
            let mut kept = Vec::new();
            for result in results {
                let mut edges = self.store.edges_from(&result.node_id).await?;
                edges.extend(self.store.edges_to(&result.node_id).await?);
                if edges.iter().any(|e| allowed.contains(&edge_type(e))) {
                    kept.push(result);
                }
            }
            results = kept;
        }
        if let Some(clause) = jql.connections_clause() {
            self.count_connections(&mut results).await?;
            results.retain(|r| clause.matches_numeric(r.connections));
        }
        Ok(results)
    }

    async fn filter_related(
        &self,
        results: Vec<SymbolResult>,
        related: &str,
        direction: &str,
        edge_types: &[String],
    ) -> Result<Vec<SymbolResult>, EngineError> {
        let Some(anchor) = self.resolve_symbol(related, "").await? else {
            return Ok(Vec::new());
        };
        let allowed = (!edge_types.is_empty()).then(|| normalise_edge_types(edge_types));
        let mut reachable: HashSet<String> = HashSet::new();
        let mut visited: HashSet<String> = HashSet::from([anchor.clone()]);
        let mut frontier: VecDeque<(String, usize)> = VecDeque::from([(anchor, 0)]);
        let mut expansions = 0;
        while let Some((current, depth)) = frontier.pop_front() {
            if depth >= 3 {
                continue;
            }
            if expansions >= MAX_EXPANSIONS {
                break;
            }
            expansions += 1;
            let mut edges: Vec<(String, String)> = Vec::new();
            if matches!(direction, "outgoing" | "out" | "both") {
                for e in self.store.edges_from(&current).await? {
                    edges.push((edge_type(&e), e.target_id));
                }
            }
            if matches!(direction, "incoming" | "in" | "both") {
                for e in self.store.edges_to(&current).await? {
                    edges.push((edge_type(&e), e.source_id));
                }
            }
            for (rel_type, other) in edges {
                if other.is_empty() || allowed.as_ref().is_some_and(|a| !a.contains(&rel_type)) {
                    continue;
                }
                reachable.insert(other.clone());
                if visited.insert(other.clone()) {
                    frontier.push_back((other, depth + 1));
                }
            }
        }
        if results.is_empty() {
            let rows = self.nodes_by_id(&reachable).await?;
            let mut materialized: Vec<SymbolResult> = rows.values().map(row_to_result).collect();
            materialized.sort_by(|a, b| a.node_id.cmp(&b.node_id));
            self.count_connections(&mut materialized).await?;
            materialized.sort_by(|a, b| {
                b.connections
                    .cmp(&a.connections)
                    .then_with(|| a.symbol_name.cmp(&b.symbol_name))
            });
            return Ok(materialized);
        }
        Ok(results
            .into_iter()
            .filter(|r| reachable.contains(&r.node_id))
            .collect())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn aliases_expand_both_ways() {
        let set = normalise_edge_types(&["inherits".to_owned()]);
        assert!(set.contains("inheritance") && set.contains("inherits"));
        let set = normalise_edge_types(&["inheritance".to_owned()]);
        assert!(set.contains("extends") && set.contains("inherits"));
        assert!(matches_path("src/a/b.py", "src/a"));
        assert!(matches_path("src/a/b.py", "src/*"));
        assert!(!matches_path("src/ab.py", "src/a"));
    }

    #[test]
    fn via_and_confidence_come_from_the_annotations() {
        let edge = EdgeRecord {
            source_id: "a".into(),
            target_id: "b".into(),
            rel_type: "Calls".into(),
            edge_class: None,
            weight: 1.0,
            metadata: serde_json::json!({"via": [" x@L1 ", ""], "confidence": "high"}),
        };
        assert_eq!(edge_via(&edge), vec!["x@L1"]);
        assert_eq!(edge_confidence(&edge), "high");
        assert_eq!(edge_type(&edge), "calls");
    }
}
