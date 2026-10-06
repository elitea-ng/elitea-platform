//! A page's context: `_get_relevant_content_for_page`, the routing
//! between cluster expansion and the documentation path.
//!
//! What is live under the cluster planner (the default, and the only
//! planner the port has):
//!
//! 1. a page the cluster planner made ("graph clustering" in the
//!    rationale) with target symbols goes through cluster expansion
//!    ([`super::expansion`]); nearly every page;
//! 2. a page without symbols — a documentation cluster — goes to the
//!    documentation path: the planner's `target_docs` by path
//!    (`_fetch_explicit_target_docs`: a scan of the graph's document
//!    nodes, then the file on disk), else a scored scan of every document
//!    node (`_get_doc_nodes_from_graph`). The FTS5 strategies of both are
//!    dead (the graph text index is never built since the `.code_graph.gz`
//!    writes were removed) and not ported.
//!
//! Then the token budget (80,000 `o200k_base` tokens with the metadata
//! heading), ranked truncation when over it, and `_format_simple_context`.
//!
//! DELIBERATE DIFFERENCES:
//!
//! * a page whose symbols cluster expansion cannot resolve went, in
//!   Python, to the legacy networkx expansion (`_get_docs_by_target_symbols`,
//!   ~450 lines over the whole in-memory graph). Not ported: such a page is
//!   treated as Python treated one whose legacy lookup found nothing — the
//!   documentation path. Cluster-planner symbols always resolve inside
//!   their own cluster, so this needs a planner bug or `exclude_tests`
//!   removing every seed;
//! * when every retrieval comes back empty, Python ran a vector search
//!   over the retriever stack (the FAISS / BM25 ensemble that no longer
//!   exists, ADR-0026). The port uses the fallback Python itself used
//!   when that search failed: the repository context as the page context,
//!   no files.

use super::context::{PylonPlugin, RelevantContent, format_simple_context};
use super::doc::{ContextDoc, count_documents};
use super::expansion::{ExpansionFlags, ExpansionRequest, expand_for_page};
use super::index::{IndexNode, PageIndex};
use super::pyregex::{Flags, PyRe};
use super::ranker::ranked_truncation;
use super::repo_files;
use super::search::PageSearch;
use super::spec::PageSpec;
use crate::errors::EngineError;
use crate::graph::{constants::DOC_SYMBOL_TYPES, pystr};
use serde_json::Value;
use std::collections::HashSet;
use std::path::{Path, PathBuf};
use std::sync::{Arc, LazyLock};

/// `CONTEXT_TOKEN_BUDGET` (`DEEPWIKI_CONTEXT_TOKEN_BUDGET`, default 80000).
pub const CONTEXT_TOKEN_BUDGET: usize = 80_000;

/// `TARGET_DOC_TOKEN_CAP`.
const TARGET_DOC_TOKEN_CAP: usize = 8_000;

/// Everything a page's retrieval reads.
pub struct RetrievalContext<S: PageSearch> {
    pub index: Arc<PageIndex>,
    pub search: Arc<S>,
    /// The checked-out repository (`indexer.get_repo_root()`).
    pub repo_root: Option<PathBuf>,
    pub flags: ExpansionFlags,
    pub budget: usize,
    pub pylon: PylonPlugin,
}

/// How a page's context was built (for the progress log).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Route {
    Cluster,
    Documentation,
    RepositoryContext,
}

fn int_of(value: Option<&Value>) -> Option<i64> {
    match value? {
        Value::Number(n) => n.as_i64(),
        Value::String(s) => s.trim().parse().ok(),
        _ => None,
    }
}

/// `cluster_utils.extract_macro_id`.
fn macro_from_rationale(rationale: &str) -> Option<i64> {
    static MACRO: LazyLock<Option<PyRe>> =
        LazyLock::new(|| PyRe::new(r"macro=(\d+)", Flags::NONE).ok());
    let m = MACRO.as_ref()?.search(rationale).ok()??;
    m.group(1).parse().ok()
}

impl<S: PageSearch> RetrievalContext<S> {
    /// `_get_relevant_content_for_page`.
    ///
    /// # Errors
    ///
    /// A failure Python raised out of the page (an empty `key_files`
    /// pattern in ranked truncation, a broken tokenizer); the page then
    /// fails.
    pub fn relevant_content(
        &self,
        page: &PageSpec,
        repo_context: &str,
    ) -> Result<(RelevantContent, Route), EngineError> {
        if let Some(content) = self.try_cluster_expansion(page)? {
            return Ok((content, Route::Cluster));
        }
        let mut docs: Vec<ContextDoc> = Vec::new();
        if !page.target_docs.is_empty() {
            docs.extend(self.explicit_target_docs(&page.target_docs));
        }
        if docs.is_empty() {
            let targets = (!page.target_docs.is_empty()).then_some(page.target_docs.as_slice());
            docs.extend(doc_nodes_from_graph(
                &self.index,
                targets,
                &page.page_name,
                &page.target_symbols,
                10,
            )?);
        }
        if docs.is_empty() {
            return Ok((
                RelevantContent {
                    content: repo_context.to_owned(),
                    files: Vec::new(),
                },
                Route::RepositoryContext,
            ));
        }
        Ok((self.fit(docs, page)?, Route::Documentation))
    }

    /// Format within the budget, truncating by rank when over it.
    fn fit(&self, docs: Vec<ContextDoc>, page: &PageSpec) -> Result<RelevantContent, EngineError> {
        let docs = if count_documents(&docs)? <= self.budget {
            docs
        } else {
            ranked_truncation(&self.index, &docs, page, self.budget)?
        };
        Ok(format_simple_context(
            &docs,
            page,
            self.index.facts(),
            &self.pylon,
        ))
    }

    /// `_try_cluster_expansion`.
    fn try_cluster_expansion(
        &self,
        page: &PageSpec,
    ) -> Result<Option<RelevantContent>, EngineError> {
        if !page.rationale.contains("graph clustering") {
            return Ok(None);
        }
        if page.target_symbols.is_empty() {
            return Ok(None);
        }
        let macro_id = int_of(page.metadata.get("section_id"))
            .or_else(|| macro_from_rationale(&page.rationale));
        let micro_id = int_of(page.metadata.get("page_id"));
        let cluster_node_ids: Vec<String> = page
            .metadata
            .get("cluster_node_ids")
            .and_then(Value::as_array)
            .map(|ids| {
                ids.iter()
                    .filter_map(Value::as_str)
                    .map(str::to_owned)
                    .collect()
            })
            .unwrap_or_default();
        let request = ExpansionRequest {
            page_symbols: &page.target_symbols,
            macro_id,
            micro_id,
            cluster_node_ids: &cluster_node_ids,
            token_budget: i64::try_from(self.budget).unwrap_or(i64::MAX),
        };
        // Python caught any expansion error and fell back to the legacy
        // path; so does this (to the documentation path, see the module
        // comment).
        let docs = match expand_for_page(&self.index, self.search.as_ref(), &request, self.flags) {
            Ok(docs) => docs,
            Err(error) => {
                tracing::warn!(page = %page.page_name, %error, "cluster expansion failed; documentation path");
                return Ok(None);
            }
        };
        if docs.is_empty() {
            return Ok(None);
        }
        self.fit(docs, page).map(Some)
    }

    /// `_fetch_explicit_target_docs` (strategies 2 and 3; see the module
    /// comment), with the containment rules of [`repo_files`].
    fn explicit_target_docs(&self, targets: &[String]) -> Vec<ContextDoc> {
        let char_cap = TARGET_DOC_TOKEN_CAP * 7 / 2;
        let mut collected: Vec<ContextDoc> = Vec::new();
        let mut total_chars = 0usize;
        let mut seen: HashSet<String> = HashSet::new();
        // Strategy 2: the graph's document nodes.
        for node in self.index.nodes() {
            if total_chars >= char_cap {
                break;
            }
            let symbol_type = node.symbol_type.to_lowercase();
            if !DOC_SYMBOL_TYPES.contains(&symbol_type.as_str()) {
                continue;
            }
            let fp = node.rel_path.as_str();
            if seen.contains(fp) {
                continue;
            }
            if !targets
                .iter()
                .any(|t| fp.contains(t.as_str()) || fp.ends_with(t.as_str()))
            {
                continue;
            }
            seen.insert(fp.to_owned());
            if node.text().is_empty() {
                continue;
            }
            let content = cap_chars(node.text(), char_cap, &mut total_chars);
            collected.push(explicit_doc(fp, fp, &symbol_type, content));
        }
        // Strategy 3: the file on disk.
        let still_missed: Vec<&String> = targets
            .iter()
            .filter(|t| !collected.iter().any(|d| d.source.contains(t.as_str())))
            .collect();
        let Some(root) = self.repo_root.as_deref().filter(|r| r.is_dir()) else {
            return collected;
        };
        if still_missed.is_empty() || total_chars >= char_cap {
            return collected;
        }
        for target in still_missed {
            if total_chars >= char_cap {
                break;
            }
            let mut candidates: Vec<PathBuf> = vec![PathBuf::from(target)];
            if !target.contains('/')
                && let Some(found) = repo_files::find_by_name(root, target)
            {
                candidates.push(found);
            }
            for candidate in candidates {
                let Some(bytes) = repo_files::read_contained(root, &candidate) else {
                    continue;
                };
                let text = pystr::decode_text(&bytes, pystr::Errors::Replace);
                if text.is_empty() {
                    continue;
                }
                let rel = normalized_rel(&candidate);
                if seen.contains(&rel) {
                    continue;
                }
                seen.insert(rel.clone());
                let ext = pystr::suffix(&rel).to_lowercase();
                let doc_type = crate::graph::discover::DOCUMENTATION_EXTENSIONS
                    .iter()
                    .find(|(e, _)| *e == ext)
                    .map_or("document", |(_, t)| *t);
                let symbol_type = format!("{doc_type}_document");
                let content = cap_chars(&text, char_cap, &mut total_chars);
                collected.push(explicit_doc(&rel, &rel, &symbol_type, content));
                break;
            }
        }
        collected
    }
}

/// `os.path.relpath` of a contained relative path: its plain components.
fn normalized_rel(path: &Path) -> String {
    path.components()
        .filter_map(|c| match c {
            std::path::Component::Normal(p) => p.to_str(),
            _ => None,
        })
        .collect::<Vec<_>>()
        .join("/")
}

/// The text cut to what is left of the character cap
/// (`content[: char_cap - total_chars]`), counting it.
fn cap_chars(text: &str, char_cap: usize, total: &mut usize) -> String {
    let length = text.chars().count();
    let content = if *total + length > char_cap {
        pystr::prefix_chars(text, char_cap.saturating_sub(*total)).to_owned()
    } else {
        text.to_owned()
    };
    *total += content.chars().count();
    content
}

fn explicit_doc(source: &str, file_path: &str, symbol_type: &str, content: String) -> ContextDoc {
    ContextDoc {
        content,
        source: source.to_owned(),
        rel_path: source.to_owned(),
        file_path: file_path.to_owned(),
        symbol_name: String::new(),
        symbol_type: symbol_type.to_owned(),
        is_documentation: true,
        language: "text".to_owned(),
        ..ContextDoc::default()
    }
}

/// `_score_doc_candidate`.
fn score_doc(
    file_path: &str,
    content: &str,
    has_targets: bool,
    page_keywords: &HashSet<String>,
    symbol_keywords: &HashSet<String>,
    page_name: &str,
) -> i64 {
    if has_targets {
        return 1000;
    }
    let mut score = 0;
    let path_lower = file_path.to_lowercase();
    let doc_name = file_path
        .rsplit('/')
        .next()
        .unwrap_or(file_path)
        .to_lowercase();
    let mut topic: HashSet<String> =
        pystr::split_whitespace(&page_name.to_lowercase().replace(['-', '_'], " "))
            .map(str::to_owned)
            .collect();
    for stop in ["the", "a", "an", "and", "or", "for", "in", "of", "to"] {
        topic.remove(stop);
    }
    let doc_keywords: HashSet<String> = pystr::split_whitespace(
        &doc_name
            .replace(".md", "")
            .replace(".rst", "")
            .replace(".txt", "")
            .replace(['-', '_'], " "),
    )
    .map(str::to_owned)
    .collect();
    if topic.is_disjoint(&doc_keywords) && !doc_name.contains("readme") {
        score -= 50;
    }
    for kw in symbol_keywords {
        if path_lower.contains(kw.as_str()) {
            score += 100;
        }
    }
    for kw in page_keywords {
        if path_lower.contains(kw.as_str()) {
            score += 50;
        }
    }
    if path_lower.contains("readme") {
        score += 30;
    }
    if path_lower.contains("/docs/") || path_lower.starts_with("docs/") {
        score += 20;
    }
    let sample = pystr::prefix_chars(content, 500).to_lowercase();
    for kw in symbol_keywords {
        if sample.contains(kw.as_str()) {
            score += 10;
        }
    }
    score
}

/// `_get_doc_nodes_from_graph` with its brute-force scan.
///
/// # Errors
///
/// Never in practice (one constant pattern).
pub fn doc_nodes_from_graph(
    index: &PageIndex,
    targets: Option<&[String]>,
    page_name: &str,
    target_symbols: &[String],
    max_docs: usize,
) -> Result<Vec<ContextDoc>, EngineError> {
    static CAMEL: LazyLock<Result<PyRe, super::pyregex::ReError>> =
        LazyLock::new(|| PyRe::new(r"[A-Z]?[a-z]+|[A-Z]+(?=[A-Z][a-z]|\b)", Flags::NONE));
    let camel = CAMEL
        .as_ref()
        .map_err(|e| EngineError::new(crate::errors::ErrorType::Runtime, e.to_string()))?;
    let page_keywords: HashSet<String> =
        pystr::split_whitespace(&page_name.to_lowercase().replace(['-', '_'], " "))
            .filter(|w| w.chars().count() > 2)
            .map(str::to_owned)
            .collect();
    let mut symbol_keywords: HashSet<String> = HashSet::new();
    for symbol in target_symbols {
        symbol_keywords.insert(symbol.to_lowercase());
        for part in camel
            .find_all(symbol)
            .map_err(|e| EngineError::new(crate::errors::ErrorType::Runtime, e.to_string()))?
        {
            if part.whole().chars().count() > 2 {
                symbol_keywords.insert(part.whole().to_lowercase());
            }
        }
    }
    let mut candidates: Vec<(i64, &IndexNode, String)> = Vec::new();
    let mut seen: HashSet<&str> = HashSet::new();
    for node in index.nodes() {
        let symbol_type = node.symbol_type.to_lowercase();
        if !DOC_SYMBOL_TYPES.contains(&symbol_type.as_str()) {
            continue;
        }
        let fp = node.rel_path.as_str();
        if let Some(targets) = targets
            && !targets
                .iter()
                .any(|t| fp.contains(t.as_str()) || fp.ends_with(t.as_str()))
        {
            continue;
        }
        if !seen.insert(fp) {
            continue;
        }
        if node.text().is_empty() {
            continue;
        }
        let score = score_doc(
            fp,
            node.text(),
            targets.is_some(),
            &page_keywords,
            &symbol_keywords,
            page_name,
        );
        candidates.push((score, node, symbol_type));
    }
    candidates.sort_by_key(|c| std::cmp::Reverse(c.0));
    Ok(candidates
        .into_iter()
        .take(max_docs)
        .map(|(_, node, symbol_type)| ContextDoc {
            content: node.text().to_owned(),
            source: node.rel_path.clone(),
            symbol_type: symbol_type.clone(),
            start_line: node.start_line.unwrap_or(0),
            end_line: node.end_line.unwrap_or(0),
            is_documentation: true,
            language: if symbol_type.contains("markdown") {
                "markdown".to_owned()
            } else {
                "text".to_owned()
            },
            node_id: node.node_id.clone(),
            ..ContextDoc::default()
        })
        .collect())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_character_cap_counts_characters() {
        let mut total = 0;
        assert_eq!(cap_chars("ééé", 2, &mut total), "éé");
        assert_eq!(total, 2);
    }

    #[test]
    fn macro_comes_from_the_rationale() {
        assert_eq!(
            macro_from_rationale("Grouped by graph clustering (macro=3, micro=7)"),
            Some(3)
        );
        assert_eq!(macro_from_rationale("none"), None);
    }
}
