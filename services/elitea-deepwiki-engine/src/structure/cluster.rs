//! The cluster structure planner: `ClusterStructurePlanner.plan_structure`
//! (`wiki_structure_planner/cluster_planner.py`), the planner the web app
//! always asks for (`planner_type=cluster`).
//!
//! 1. Load the architectural cluster map: every architectural node with a
//!    section (non-test ones under `exclude_tests`), section → page →
//!    nodes in row order; a node without a page is page 0.
//! 2. Validate the pages (`validation`): `DEMOTE` removes a page; `SPLIT_BY`
//!    cuts one into per-file pages (at most 5, the smaller files joining the
//!    largest), numbered after the section's highest page id; `MERGE_WITH`
//!    moves a page into the sibling it shares the most edges with (ties: the
//!    smaller sibling, then the first in map order), counted once per
//!    section BEFORE any merge; `PROMOTE_DOCS` keeps the page. Empty sections
//!    go. The coverage ledger is computed and logged.
//! 3. Name each section with one model call (`BATCHED_NAMING_*`, on unless
//!    `DEEPWIKI_NAMING_BATCHED` / `WIKI_NAMING_BATCHED` is false): every
//!    page from its own top symbols, the section from its pages. A call
//!    that fails, or an answer without a section name, without a `pages`
//!    list, or missing a page, falls back to one call per page
//!    (`PAGE_NAMING_*`) and then a section name from the page names
//!    (`SECTION_FROM_PAGES_*`) — or, with `DEEPWIKI_NAMING_ORDER` other than
//!    `pages_first`, a section name from its top symbols first
//!    (`SECTION_NAMING_*`). A section whose naming fails outright gets the
//!    fallback section (`Module: <common path>`).
//! 4. Each page's `target_symbols` are its most central symbols
//!    (`centrality`), `k = max(3, min(15, ⌈√size⌉))`, code before
//!    supporting code before other before docs; a page of at least 70 %
//!    docs takes docs first.
//!
//! Quirks kept (they decide names and orders a reader sees):
//!
//! * the section's file count looks at the first 200 nodes of the section
//!   (a page's at its first 100);
//! * a section's symbol count is its distinct nodes, its fallback
//!   description counts them with repeats;
//! * a model answer that is valid JSON but not an object, or a name that
//!   is not a string, fails the section (Python's `AttributeError` /
//!   validation error escaped to `plan_structure`), not just the call;
//! * `page_id` is read with `int()` (`"3"` and `3.9` are page 3).
//!
//! Deliberate difference: Python collected the section's nodes, and the
//! nodes a page's centrality starts from, in `set`s whose order follows the
//! hash seed; that order reached the file-count sample above and the
//! order of `target_symbols`. The order here is insertion order.

// `macro_id` / `micro_id` are Python's names for the section and page ids;
// they are kept so the port reads next to the source.
#![allow(clippy::similar_names)]

use super::centrality::CentralGraph;
use super::index::PlannerIndex;
use super::model::{self, ChatModel, NotAString, is_cancelled, truthy, truthy_str, user_request};
use super::parse::naming_json;
use super::prompts;
use super::spec::{PageSpec, SectionSpec, WikiStructureSpec};
use super::validation::{
    self, ClusterMap, DOC_CLUSTER_SYMBOLS, PAGE_IDENTITY_SYMBOLS, SUPPORTING_CODE_SYMBOLS, Shape,
};
use crate::errors::EngineError;
use crate::llm::ChatMessage;
use crate::pyjson;
use indexmap::{IndexMap, IndexSet};
use serde_json::{Map, Value, json};
use std::collections::HashMap;

/// `_MAX_SPLIT_SUBPAGES`.
const MAX_SPLIT_SUBPAGES: usize = 5;
/// `MAX_DOMINANT_SYMBOLS`.
const MAX_DOMINANT_SYMBOLS: usize = 10;
/// `MAX_MICRO_SUMMARY_SYMBOLS`.
const MAX_MICRO_SUMMARY_SYMBOLS: usize = 8;
/// `MAX_PAGE_NAMING_SYMBOLS`.
const MAX_PAGE_NAMING_SYMBOLS: usize = 12;
/// `SIGNATURE_TRUNC` / `DOCSTRING_TRUNC`, in characters.
const TRUNCATE: usize = 200;

/// The planner's switches.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ClusterSettings {
    /// `exclude_tests` (`DEEPWIKI_EXCLUDE_TESTS`, the request's choice).
    pub exclude_tests: bool,
    /// `DEEPWIKI_NAMING_BATCHED` (default on).
    pub naming_batched: bool,
    /// `DEEPWIKI_NAMING_ORDER == "pages_first"` (the default).
    pub pages_first: bool,
}

impl Default for ClusterSettings {
    fn default() -> Self {
        Self {
            exclude_tests: false,
            naming_batched: true,
            pages_first: true,
        }
    }
}

impl ClusterSettings {
    /// The naming switches from the environment (`_env_bool` /
    /// `_env_value`: the `DEEPWIKI_` name, else the `WIKI_` one; blank is
    /// unset; an unknown boolean spelling is the default).
    #[must_use]
    pub fn from_lookup(exclude_tests: bool, lookup: impl Fn(&str) -> Option<String>) -> Self {
        let value = |primary: &str, fallback: &str| {
            [primary, fallback].iter().find_map(|name| {
                lookup(name)
                    .map(|v| v.trim().to_owned())
                    .filter(|v| !v.is_empty())
            })
        };
        let naming_batched = !matches!(
            value("DEEPWIKI_NAMING_BATCHED", "WIKI_NAMING_BATCHED")
                .map(|v| v.to_lowercase())
                .as_deref(),
            Some("0" | "false" | "no" | "off")
        );
        let pages_first = value("DEEPWIKI_NAMING_ORDER", "WIKI_NAMING_ORDER")
            .is_none_or(|v| v.to_lowercase() == "pages_first");
        Self {
            exclude_tests,
            naming_batched,
            pages_first,
        }
    }
}

/// Why a section's naming failed outright (Python's exception out of
/// `_name_macro_cluster`): the section gets the fallback.
enum SectionFailure {
    /// Stop: not a naming failure.
    Cancelled(EngineError),
    /// Anything Python's `except Exception` caught.
    Failed(String),
}

impl From<NotAString> for SectionFailure {
    fn from(_: NotAString) -> Self {
        Self::Failed("a name is not a string".to_owned())
    }
}

/// The planner over one index.
pub struct ClusterPlanner<'a> {
    index: &'a PlannerIndex,
    settings: ClusterSettings,
    graph: CentralGraph,
    wiki_title: String,
}

impl<'a> ClusterPlanner<'a> {
    #[must_use]
    pub fn new(index: &'a PlannerIndex, settings: ClusterSettings) -> Self {
        Self {
            index,
            settings,
            graph: CentralGraph::new(index, settings.exclude_tests),
            wiki_title: derive_wiki_title(index.repo_identifier.as_deref()),
        }
    }

    /// `plan_structure`.
    ///
    /// # Errors
    ///
    /// Only a stop, or a broken prompt template. A failed or malformed
    /// naming call is replaced, as in Python.
    pub async fn plan_structure(
        &self,
        model: &impl ChatModel,
    ) -> Result<WikiStructureSpec, EngineError> {
        let mut cluster_map = self.load_cluster_map();
        if cluster_map.is_empty() {
            tracing::warn!("ClusterStructurePlanner: no clusters in DB — returning fallback");
            return Ok(self.fallback_spec());
        }
        self.apply_validation(&mut cluster_map);
        let ledger = validation::coverage_report(
            self.index,
            &validation::build_candidates(self.index, &cluster_map),
        );
        tracing::info!(?ledger, "CoverageLedger");

        let mut macros: Vec<i64> = cluster_map.keys().copied().collect();
        macros.sort_unstable();
        let mut sections = Vec::with_capacity(macros.len());
        for (order, macro_id) in macros.into_iter().enumerate() {
            let pages = &cluster_map[&macro_id];
            let section_order = i64::try_from(order + 1).unwrap_or(i64::MAX);
            let section = match self
                .name_section(model, macro_id, pages, section_order)
                .await
            {
                Ok(section) => section,
                Err(SectionFailure::Cancelled(error)) => return Err(error),
                Err(SectionFailure::Failed(reason)) => {
                    tracing::warn!(
                        "LLM naming failed for macro {macro_id}, using fallback: {reason}"
                    );
                    self.fallback_section(macro_id, pages, section_order)
                }
            };
            sections.push(section);
        }
        let total_pages = sections.iter().map(|s| s.pages.len()).sum::<usize>();
        Ok(WikiStructureSpec {
            wiki_title: self.wiki_title.clone(),
            overview: build_overview(&sections),
            sections,
            total_pages: i64::try_from(total_pages).unwrap_or(i64::MAX),
        })
    }

    /// `_load_architectural_cluster_map`.
    #[must_use]
    pub fn load_cluster_map(&self) -> ClusterMap {
        let mut map = ClusterMap::new();
        for (position, node) in self.index.nodes().iter().enumerate() {
            let Some(macro_id) = node.macro_cluster else {
                continue;
            };
            if !node.is_architectural || (self.settings.exclude_tests && node.is_test) {
                continue;
            }
            let micro_id = node.micro_cluster.unwrap_or(0);
            map.entry(macro_id)
                .or_default()
                .entry(micro_id)
                .or_default()
                .push(position);
        }
        map
    }

    /// Steps 1b of `plan_structure`: demote, split, merge.
    pub fn apply_validation(&self, cluster_map: &mut ClusterMap) {
        let candidates = validation::build_candidates(self.index, cluster_map);
        let validations = validation::validate_all(&candidates);

        for v in validations.iter().filter(|v| v.shape == Shape::Demote) {
            if let Some(pages) = cluster_map.get_mut(&v.macro_id) {
                pages.shift_remove(&v.micro_id);
            }
        }

        for v in validations.iter().filter(|v| v.shape == Shape::SplitBy) {
            let Some(pages) = cluster_map.get_mut(&v.macro_id) else {
                continue;
            };
            let Some(nodes) = pages.get(&v.micro_id) else {
                continue;
            };
            let mut by_file: IndexMap<&str, Vec<usize>> = IndexMap::new();
            for &position in nodes {
                let rel = self.index.node(position).rel_path.as_str();
                let rel = if rel.is_empty() { "_unknown" } else { rel };
                by_file.entry(rel).or_default().push(position);
            }
            if by_file.len() <= 1 {
                continue;
            }
            let mut ordered: Vec<Vec<usize>> = by_file.into_values().collect();
            // `sorted(key=-len)` is stable.
            ordered.sort_by_key(|part| std::cmp::Reverse(part.len()));
            if ordered.len() > MAX_SPLIT_SUBPAGES {
                let extra: Vec<usize> = ordered.drain(MAX_SPLIT_SUBPAGES..).flatten().collect();
                ordered[0].extend(extra);
            }
            pages.shift_remove(&v.micro_id);
            let first = pages.keys().copied().max().unwrap_or(-1) + 1;
            for (page_id, part) in (first..).zip(ordered) {
                pages.insert(page_id, part);
            }
        }

        let merge_macros: IndexSet<i64> = validations
            .iter()
            .filter(|v| v.shape == Shape::MergeWith && cluster_map.contains_key(&v.macro_id))
            .map(|v| v.macro_id)
            .collect();
        let edge_counts: HashMap<i64, HashMap<(i64, i64), usize>> = merge_macros
            .iter()
            .map(|&m| (m, self.macro_edge_index(&cluster_map[&m])))
            .collect();
        for v in validations.iter().filter(|v| v.shape == Shape::MergeWith) {
            let Some(pages) = cluster_map.get_mut(&v.macro_id) else {
                continue;
            };
            if !pages.contains_key(&v.micro_id) {
                continue;
            }
            let counts = edge_counts.get(&v.macro_id);
            let mut best: Option<(i64, usize, usize)> = None;
            for (&other, nodes) in pages.iter() {
                if other == v.micro_id {
                    continue;
                }
                let key = if v.micro_id < other {
                    (v.micro_id, other)
                } else {
                    (other, v.micro_id)
                };
                let edges = counts.and_then(|c| c.get(&key)).copied().unwrap_or(0);
                let better = match best {
                    None => true,
                    Some((_, best_edges, best_size)) => {
                        edges > best_edges || (edges == best_edges && nodes.len() < best_size)
                    }
                };
                if better {
                    best = Some((other, edges, nodes.len()));
                }
            }
            if let Some((target, _, _)) = best
                && let Some(moved) = pages.shift_remove(&v.micro_id)
                && let Some(into) = pages.get_mut(&target)
            {
                into.extend(moved);
            }
        }
        cluster_map.retain(|_, pages| !pages.is_empty());
    }

    /// `_build_macro_edge_index`: edges between two different pages of a
    /// section, per unordered page pair.
    fn macro_edge_index(&self, pages: &IndexMap<i64, Vec<usize>>) -> HashMap<(i64, i64), usize> {
        let mut page_of: HashMap<usize, i64> = HashMap::new();
        for (&page, nodes) in pages {
            for &node in nodes {
                page_of.insert(node, page);
            }
        }
        let mut counts = HashMap::new();
        for (&node, &a) in &page_of {
            for edge in self.index.edges_from(node) {
                let Some(&b) = page_of.get(&edge.target) else {
                    continue;
                };
                if a != b {
                    *counts
                        .entry(if a < b { (a, b) } else { (b, a) })
                        .or_insert(0) += 1;
                }
            }
        }
        counts
    }

    /// `_name_macro_cluster`.
    async fn name_section(
        &self,
        model: &impl ChatModel,
        macro_id: i64,
        pages: &IndexMap<i64, Vec<usize>>,
        section_order: i64,
    ) -> Result<SectionSpec, SectionFailure> {
        let all_nodes: IndexSet<usize> = pages.values().flatten().copied().collect();
        let file_count = distinct_files(self.index, all_nodes.iter().take(200).copied());
        let node_count = all_nodes.len();

        if self.settings.naming_batched
            && let Some((name, description, names)) = self
                .batched_naming(model, macro_id, pages, node_count, file_count)
                .await?
        {
            return self.assemble_section(
                macro_id,
                pages,
                section_order,
                &name,
                &description,
                &names,
                node_count,
                true,
            );
        }

        let mut section_name = Value::String(format!("Section {macro_id}"));
        let mut section_desc = Value::String(format!("Section covering {node_count} symbols"));
        if !self.settings.pages_first {
            let object = self
                .name_from_symbols(model, macro_id, pages, node_count, file_count)
                .await?;
            section_name = or_value(object.get("section_name"), section_name);
            section_desc = or_value(object.get("section_description"), section_desc);
        }
        let names = self
            .name_pages(model, macro_id, pages, &section_name)
            .await?;
        if self.settings.pages_first {
            if let Some(object) = self
                .name_from_pages(model, macro_id, pages, &names, node_count)
                .await?
            {
                section_name = or_value(object.get("section_name"), section_name);
                section_desc = or_value(object.get("section_description"), section_desc);
            }
            if section_name == Value::String(format!("Section {macro_id}"))
                && !names.values().any(truthy)
            {
                return Err(SectionFailure::Failed(format!(
                    "All naming calls failed for macro {macro_id}"
                )));
            }
        }
        let Value::String(section_name) = section_name else {
            return Err(NotAString.into());
        };
        let Value::String(section_desc) = section_desc else {
            return Err(NotAString.into());
        };
        self.assemble_section(
            macro_id,
            pages,
            section_order,
            &section_name,
            &section_desc,
            &names,
            node_count,
            false,
        )
    }

    /// The legacy section call (`DEEPWIKI_NAMING_ORDER` other than
    /// `pages_first`): a name from the section's top symbols. Python did not
    /// catch its failure, so a failed call fails the section.
    async fn name_from_symbols(
        &self,
        model: &impl ChatModel,
        macro_id: i64,
        pages: &IndexMap<i64, Vec<usize>>,
        node_count: usize,
        file_count: usize,
    ) -> Result<Map<String, Value>, SectionFailure> {
        let user = prompts::format(
            prompts::SECTION_NAMING_USER,
            &[
                ("node_count", &node_count.to_string()),
                ("file_count", &file_count.to_string()),
                (
                    "dominant_symbols_json",
                    &pyjson::dumps_indent2(&self.dominant_symbols(macro_id)),
                ),
                (
                    "micro_summaries_json",
                    &pyjson::dumps_indent2(&self.micro_summaries(pages)),
                ),
            ],
        )
        .map_err(|e| SectionFailure::Cancelled(super::template_error(&e)))?;
        let answer = self
            .ask(model, prompts::SECTION_NAMING_SYSTEM, user)
            .await
            .map_err(|e| failure(&e))?;
        match naming_json(&answer) {
            Value::Object(object) => Ok(object),
            _ => Err(SectionFailure::Failed(
                "the section answer is not an object".to_owned(),
            )),
        }
    }

    /// One `PAGE_NAMING_*` call per page, in page id order (every prompt is
    /// built first, as in Python). A failed call names nothing (`{}`).
    async fn name_pages(
        &self,
        model: &impl ChatModel,
        macro_id: i64,
        pages: &IndexMap<i64, Vec<usize>>,
        section_name: &Value,
    ) -> Result<IndexMap<i64, Value>, SectionFailure> {
        let mut micros: Vec<i64> = pages.keys().copied().collect();
        micros.sort_unstable();
        let mut prompts_by_page = Vec::with_capacity(micros.len());
        for &micro_id in &micros {
            let nodes = &pages[&micro_id];
            let user = prompts::format(
                prompts::PAGE_NAMING_USER,
                &[
                    ("symbol_count", &nodes.len().to_string()),
                    (
                        "file_count",
                        &distinct_files(self.index, nodes.iter().take(100).copied()).to_string(),
                    ),
                    ("section_name", &python_str(section_name)),
                    (
                        "page_symbols_json",
                        &pyjson::dumps_indent2(&self.page_symbols(nodes)),
                    ),
                    (
                        "directories_json",
                        &pyjson::dumps(&json!(folders(self.index, nodes))),
                    ),
                ],
            )
            .map_err(|e| SectionFailure::Cancelled(super::template_error(&e)))?;
            prompts_by_page.push((micro_id, user));
        }
        let mut names: IndexMap<i64, Value> = IndexMap::new();
        for (micro_id, user) in prompts_by_page {
            let parsed = match self.ask(model, prompts::PAGE_NAMING_SYSTEM, user).await {
                Ok(answer) => naming_json(&answer),
                Err(error) if is_cancelled(&error) => {
                    return Err(SectionFailure::Cancelled(error));
                }
                Err(error) => {
                    tracing::warn!(
                        "Page naming failed for macro {macro_id} micro {micro_id}: {error}"
                    );
                    Value::Object(Map::new())
                }
            };
            names.insert(micro_id, parsed);
        }
        Ok(names)
    }

    /// The `SECTION_FROM_PAGES_*` call: `Ok(None)` when it failed or did
    /// not answer an object (Python caught both and kept the names).
    async fn name_from_pages(
        &self,
        model: &impl ChatModel,
        macro_id: i64,
        pages: &IndexMap<i64, Vec<usize>>,
        names: &IndexMap<i64, Value>,
        node_count: usize,
    ) -> Result<Option<Map<String, Value>>, SectionFailure> {
        let mut micros: Vec<i64> = pages.keys().copied().collect();
        micros.sort_unstable();
        let mut payload = Vec::with_capacity(micros.len());
        for &micro_id in &micros {
            // Outside Python's `try`: a truthy non-object fails the section.
            let naming = naming_object(names.get(&micro_id))?;
            let nodes = &pages[&micro_id];
            payload.push(json!({
                "page_name": or_value(naming.get("page_name"), Value::String(format!("Page {micro_id}"))),
                "description": or_value(
                    naming.get("description"),
                    Value::String(format!("Page covering {} symbols", nodes.len())),
                ),
            }));
        }
        let user = prompts::format(
            prompts::SECTION_FROM_PAGES_USER,
            &[
                ("page_count", &payload.len().to_string()),
                ("node_count", &node_count.to_string()),
                ("pages_json", &pyjson::dumps_indent2(&Value::Array(payload))),
            ],
        )
        .map_err(|e| SectionFailure::Cancelled(super::template_error(&e)))?;
        match self
            .ask(model, prompts::SECTION_FROM_PAGES_SYSTEM, user)
            .await
        {
            Ok(answer) => {
                let Value::Object(object) = naming_json(&answer) else {
                    tracing::warn!("Section-from-pages naming failed for macro {macro_id}");
                    return Ok(None);
                };
                Ok(Some(object))
            }
            Err(error) if is_cancelled(&error) => Err(SectionFailure::Cancelled(error)),
            Err(error) => {
                tracing::warn!("Section-from-pages naming failed for macro {macro_id}: {error}");
                Ok(None)
            }
        }
    }

    /// `_batched_macro_naming`: `Ok(None)` sends the section down the
    /// multi-call path.
    async fn batched_naming(
        &self,
        model: &impl ChatModel,
        macro_id: i64,
        pages: &IndexMap<i64, Vec<usize>>,
        node_count: usize,
        file_count: usize,
    ) -> Result<Option<(String, String, IndexMap<i64, Value>)>, SectionFailure> {
        let mut micros: Vec<i64> = pages.keys().copied().collect();
        micros.sort_unstable();
        let payload: Vec<Value> = micros
            .iter()
            .map(|micro_id| {
                let nodes = &pages[micro_id];
                json!({
                    "page_id": micro_id,
                    "symbol_count": nodes.len(),
                    "page_symbols": self.page_symbols(nodes),
                })
            })
            .collect();
        let user = prompts::format(
            prompts::BATCHED_NAMING_USER,
            &[
                ("node_count", &node_count.to_string()),
                ("file_count", &file_count.to_string()),
                ("pages_json", &pyjson::dumps_indent2(&Value::Array(payload))),
            ],
        )
        .map_err(|e| SectionFailure::Cancelled(super::template_error(&e)))?;
        let parsed = match self.ask(model, prompts::BATCHED_NAMING_SYSTEM, user).await {
            Ok(answer) => naming_json(&answer),
            Err(error) if is_cancelled(&error) => return Err(SectionFailure::Cancelled(error)),
            Err(error) => {
                tracing::warn!(
                    "Batched naming failed for macro {macro_id}: {error}; falling back to multi-call naming"
                );
                return Ok(None);
            }
        };
        // `parsed.get(...)` outside the `try`: a non-object fails the section.
        let Value::Object(parsed) = parsed else {
            return Err(SectionFailure::Failed(
                "the batched answer is not an object".to_owned(),
            ));
        };
        let section_name = parsed.get("section_name").cloned().unwrap_or(Value::Null);
        let raw_pages = parsed
            .get("pages")
            .filter(|v| truthy(v))
            .cloned()
            .unwrap_or(json!([]));
        let Value::Array(raw_pages) = raw_pages else {
            return Ok(None);
        };
        if !truthy(&section_name) {
            tracing::warn!(
                "Batched naming response malformed for macro {macro_id}; falling back to multi-call naming"
            );
            return Ok(None);
        }
        let mut names: IndexMap<i64, Value> = IndexMap::new();
        for entry in raw_pages {
            let Value::Object(entry) = entry else {
                continue;
            };
            let page_id = match model::py_int(entry.get("page_id").unwrap_or(&Value::Null)) {
                Ok(Some(id)) => id,
                Ok(None) => continue,
                Err(model::Overflow) => {
                    return Err(SectionFailure::Failed(
                        "cannot convert float infinity to integer".to_owned(),
                    ));
                }
            };
            let field = |key: &str| entry.get(key).cloned().unwrap_or(Value::Null);
            names.insert(
                page_id,
                json!({
                    "page_name": field("page_name"),
                    "description": field("description"),
                    "retrieval_query": field("retrieval_query"),
                }),
            );
        }
        if micros.iter().any(|m| !names.contains_key(m)) {
            tracing::warn!(
                "Batched naming missing pages for macro {macro_id}; falling back to multi-call naming"
            );
            return Ok(None);
        }
        let Value::String(section_name) = section_name else {
            return Err(NotAString.into());
        };
        let description = truthy_str(parsed.get("section_description"))?
            .unwrap_or_else(|| format!("Section covering {node_count} symbols"));
        Ok(Some((section_name, description, names)))
    }

    /// `_assemble_section`.
    #[allow(clippy::too_many_arguments)]
    fn assemble_section(
        &self,
        macro_id: i64,
        pages: &IndexMap<i64, Vec<usize>>,
        section_order: i64,
        section_name: &str,
        section_desc: &str,
        names: &IndexMap<i64, Value>,
        node_count: usize,
        batched: bool,
    ) -> Result<SectionSpec, SectionFailure> {
        let mut micros: Vec<i64> = pages.keys().copied().collect();
        micros.sort_unstable();
        let mut specs = Vec::with_capacity(micros.len());
        for (order, micro_id) in micros.iter().enumerate() {
            let nodes = &pages[micro_id];
            let naming = naming_object(names.get(micro_id))?;
            let page_name = truthy_str(naming.get("page_name"))?
                .unwrap_or_else(|| format!("{section_name} \u{2014} Page {micro_id}"));
            let description = truthy_str(naming.get("description"))?
                .unwrap_or_else(|| format!("Page covering {} symbols", nodes.len()));
            let retrieval_query =
                truthy_str(naming.get("retrieval_query"))?.unwrap_or_else(|| page_name.clone());
            let central = self.central_node_ids(nodes, adaptive_k(nodes.len()));
            specs.push(PageSpec {
                page_name,
                page_order: i64::try_from(order + 1).unwrap_or(i64::MAX),
                content_focus: description.clone(),
                description,
                rationale: page_rationale(macro_id, *micro_id, nodes.len()),
                target_symbols: self.symbol_names(&central),
                target_docs: doc_paths(self.index, nodes),
                target_folders: folders(self.index, nodes),
                key_files: paths(self.index, nodes),
                retrieval_query,
                metadata: self.page_metadata(macro_id, *micro_id, nodes),
            });
        }
        let suffix = if batched { " (batched naming)" } else { "" };
        Ok(SectionSpec {
            section_name: section_name.to_owned(),
            section_order,
            description: section_desc.to_owned(),
            rationale: format!(
                "Mathematical clustering: macro={macro_id}, {} pages, {node_count} symbols{suffix}",
                pages.len()
            ),
            pages: specs,
        })
    }

    fn page_metadata(&self, macro_id: i64, micro_id: i64, nodes: &[usize]) -> Map<String, Value> {
        let mut metadata = Map::new();
        metadata.insert("planner_mode".to_owned(), json!("cluster"));
        metadata.insert("section_id".to_owned(), json!(macro_id));
        metadata.insert("page_id".to_owned(), json!(micro_id));
        metadata.insert(
            "cluster_node_ids".to_owned(),
            Value::Array(
                nodes
                    .iter()
                    .map(|&p| Value::String(self.index.node(p).node_id.clone()))
                    .collect(),
            ),
        );
        metadata
    }

    /// `_fallback_section`.
    fn fallback_section(
        &self,
        macro_id: i64,
        pages: &IndexMap<i64, Vec<usize>>,
        section_order: i64,
    ) -> SectionSpec {
        let all_nodes: Vec<usize> = pages.values().flatten().copied().collect();
        let first_paths = paths(self.index, &all_nodes[..all_nodes.len().min(50)]);
        let section_name = match first_paths.as_slice() {
            [] => format!("Section {macro_id}"),
            [only] => format!("Module: {only}"),
            many => {
                let common = common_path(many);
                if common.is_empty() {
                    format!("Section {macro_id}")
                } else {
                    format!("Module: {common}")
                }
            }
        };
        let mut micros: Vec<i64> = pages.keys().copied().collect();
        micros.sort_unstable();
        let specs = micros
            .iter()
            .enumerate()
            .map(|(order, micro_id)| {
                let nodes = &pages[micro_id];
                let central = self.central_node_ids(nodes, adaptive_k(nodes.len()));
                let target_symbols = self.symbol_names(&central);
                let retrieval_query = target_symbols
                    .iter()
                    .take(5)
                    .cloned()
                    .collect::<Vec<_>>()
                    .join(" ");
                PageSpec {
                    page_name: format!("{section_name} \u{2014} Page {micro_id}"),
                    page_order: i64::try_from(order + 1).unwrap_or(i64::MAX),
                    description: format!("Page with {} symbols", nodes.len()),
                    content_focus: format!("Symbols in cluster {macro_id}/{micro_id}"),
                    rationale: page_rationale(macro_id, *micro_id, nodes.len()),
                    target_symbols,
                    target_docs: doc_paths(self.index, nodes),
                    target_folders: folders(self.index, nodes),
                    key_files: paths(self.index, nodes),
                    retrieval_query,
                    metadata: self.page_metadata(macro_id, *micro_id, nodes),
                }
            })
            .collect();
        SectionSpec {
            section_name,
            section_order,
            description: format!("Auto-generated section with {} symbols", all_nodes.len()),
            rationale: format!("Fallback: LLM naming failed for macro={macro_id}"),
            pages: specs,
        }
    }

    /// `_fallback_spec`: no section at all.
    fn fallback_spec(&self) -> WikiStructureSpec {
        WikiStructureSpec {
            wiki_title: self.wiki_title.clone(),
            overview: "No clusters found \u{2014} using fallback structure.".to_owned(),
            sections: vec![SectionSpec {
                section_name: "Repository Overview".to_owned(),
                section_order: 1,
                description: "General repository documentation".to_owned(),
                rationale: "Fallback: no clusters available".to_owned(),
                pages: vec![PageSpec {
                    page_name: "Overview".to_owned(),
                    page_order: 1,
                    description: "General overview of the repository".to_owned(),
                    content_focus: "Repository structure and key components".to_owned(),
                    rationale: "Fallback page".to_owned(),
                    target_symbols: Vec::new(),
                    target_docs: Vec::new(),
                    target_folders: Vec::new(),
                    key_files: Vec::new(),
                    retrieval_query: "repository overview main components".to_owned(),
                    metadata: Map::new(),
                }],
            }],
            total_pages: 1,
        }
    }

    async fn ask(
        &self,
        model: &impl ChatModel,
        system: &'static str,
        user: String,
    ) -> Result<String, EngineError> {
        model
            .complete(&user_request(vec![
                ChatMessage::System(system),
                ChatMessage::User(user),
            ]))
            .await
    }

    /// The scored symbol list of a page (`_get_page_symbols`) or section
    /// (`_get_dominant_symbols`): architectural (non-test under
    /// `exclude_tests`) nodes by `min(out-edges, 10) + 2 if documented`,
    /// highest first, ties in node order.
    fn scored_symbols(&self, nodes: impl Iterator<Item = usize>, limit: usize) -> Value {
        let mut scored: Vec<(usize, usize)> = nodes
            .filter(|&p| {
                let node = self.index.node(p);
                node.is_architectural && !(self.settings.exclude_tests && node.is_test)
            })
            .map(|p| {
                let node = self.index.node(p);
                let score = self.index.out_degree(p).min(10)
                    + if node.docstring.is_empty() { 0 } else { 2 };
                (score, p)
            })
            .collect();
        scored.sort_by_key(|&(score, _)| std::cmp::Reverse(score));
        Value::Array(
            scored
                .into_iter()
                .take(limit)
                .map(|(_, p)| {
                    let node = self.index.node(p);
                    json!({
                        "name": node.symbol_name,
                        "type": node.symbol_type,
                        "path": node.rel_path,
                        "signature": crate::graph::pystr::prefix_chars(&node.signature, TRUNCATE),
                        "docstring": crate::graph::pystr::prefix_chars(&node.docstring, TRUNCATE),
                    })
                })
                .collect(),
        )
    }

    /// `_get_page_symbols`.
    fn page_symbols(&self, nodes: &[usize]) -> Value {
        self.scored_symbols(nodes.iter().copied(), MAX_PAGE_NAMING_SYMBOLS)
    }

    /// `_get_dominant_symbols`: `get_nodes_by_cluster(macro)` is every row
    /// of the section (hubs and non-architectural rows too, `LIMIT 1000`)
    /// in row order, then filtered.
    fn dominant_symbols(&self, macro_id: i64) -> Value {
        let rows = self
            .index
            .nodes()
            .iter()
            .enumerate()
            .filter(|(_, node)| node.macro_cluster == Some(macro_id))
            .map(|(p, _)| p)
            .take(1000);
        self.scored_symbols(rows, MAX_DOMINANT_SYMBOLS)
    }

    /// `_get_micro_summaries`.
    fn micro_summaries(&self, pages: &IndexMap<i64, Vec<usize>>) -> Value {
        let mut micros: Vec<i64> = pages.keys().copied().collect();
        micros.sort_unstable();
        Value::Array(
            micros
                .iter()
                .map(|micro_id| {
                    let nodes = &pages[micro_id];
                    let sample: Vec<usize> = nodes
                        .iter()
                        .take(MAX_MICRO_SUMMARY_SYMBOLS * 3)
                        .copied()
                        .filter(|&p| self.index.node(p).is_architectural)
                        .take(MAX_MICRO_SUMMARY_SYMBOLS)
                        .collect();
                    let symbols: Vec<Value> = sample
                        .iter()
                        .map(|&p| {
                            let node = self.index.node(p);
                            json!({"name": node.symbol_name, "type": node.symbol_type, "path": node.rel_path})
                        })
                        .collect();
                    let mut dirs: Vec<&str> = sample
                        .iter()
                        .filter_map(|&p| self.index.node(p).rel_path.rsplit_once('/').map(|(d, _)| d))
                        .collect();
                    dirs.sort_unstable();
                    dirs.dedup();
                    dirs.truncate(5);
                    json!({
                        "micro_id": micro_id,
                        "symbol_count": nodes.len(),
                        "symbols": symbols,
                        "directories": dirs,
                    })
                })
                .collect(),
        )
    }

    /// `_node_ids_to_symbol_names`: architectural code nodes only.
    fn symbol_names(&self, nodes: &[usize]) -> Vec<String> {
        nodes
            .iter()
            .map(|&p| self.index.node(p))
            .filter(|node| node.is_architectural && !node.is_doc)
            .map(|node| node.symbol_name.clone())
            .collect()
    }

    /// `_select_central_node_ids`.
    fn central_node_ids(&self, nodes: &[usize], k: usize) -> Vec<usize> {
        let cluster: Vec<usize> = nodes
            .iter()
            .copied()
            .filter(|&p| self.graph.contains(p))
            .collect::<IndexSet<usize>>()
            .into_iter()
            .collect();
        if cluster.len() <= k {
            return cluster;
        }
        let (mut identity, mut supporting, mut docs, mut other) =
            (Vec::new(), Vec::new(), Vec::new(), Vec::new());
        for &p in &cluster {
            let stype = self.index.node(p).symbol_type.to_lowercase();
            if PAGE_IDENTITY_SYMBOLS.contains(&stype.as_str()) {
                identity.push(p);
            } else if SUPPORTING_CODE_SYMBOLS.contains(&stype.as_str()) {
                supporting.push(p);
            } else if DOC_CLUSTER_SYMBOLS.contains(&stype.as_str()) {
                docs.push(p);
            } else {
                other.push(p);
            }
        }
        let total = identity.len() + supporting.len() + docs.len() + other.len();
        #[allow(clippy::cast_precision_loss)]
        let doc_dominant = total > 0 && docs.len() as f64 / total as f64 >= 0.7;
        if doc_dominant {
            let mut central = if docs.is_empty() {
                Vec::new()
            } else {
                self.graph.select_central(&docs, k)
            };
            if central.len() < k {
                let rest: Vec<usize> = cluster
                    .iter()
                    .copied()
                    .filter(|p| !central.contains(p))
                    .collect();
                let extra = self.graph.select_central(&rest, k - central.len());
                central.extend(extra);
            }
            central.truncate(k);
            return central;
        }
        let mut result: Vec<usize> = Vec::new();
        for tier in [identity, supporting, other, docs] {
            if result.len() >= k {
                break;
            }
            let remaining = k - result.len();
            let tier: Vec<usize> = tier.into_iter().filter(|p| !result.contains(p)).collect();
            if tier.is_empty() {
                continue;
            }
            if tier.len() <= remaining {
                let ranked = self.graph.select_central(&tier, tier.len());
                result.extend(if ranked.is_empty() { tier } else { ranked });
            } else {
                let ranked = self.graph.select_central(&tier, remaining);
                if ranked.is_empty() {
                    result.extend(tier.into_iter().take(remaining));
                } else {
                    result.extend(ranked);
                }
            }
        }
        if result.is_empty() {
            let central = self.graph.select_central(&cluster, k);
            return if central.is_empty() {
                nodes.iter().take(k).copied().collect()
            } else {
                central
            };
        }
        result.truncate(k);
        result
    }
}

fn failure(error: &EngineError) -> SectionFailure {
    if is_cancelled(error) {
        SectionFailure::Cancelled(error.clone())
    } else {
        SectionFailure::Failed(error.to_string())
    }
}

/// `x or fallback` on JSON values.
fn or_value(value: Option<&Value>, fallback: Value) -> Value {
    match value {
        Some(value) if truthy(value) => value.clone(),
        _ => fallback,
    }
}

/// `page_naming_by_micro.get(micro) or {}` read with `.get`: a falsy value
/// is `{}`, a truthy non-object fails the section (`AttributeError`).
fn naming_object(value: Option<&Value>) -> Result<Map<String, Value>, SectionFailure> {
    match value {
        Some(Value::Object(map)) => Ok(map.clone()),
        Some(value) if truthy(value) => Err(SectionFailure::Failed(
            "a page answer is not an object".to_owned(),
        )),
        _ => Ok(Map::new()),
    }
}

/// `str(value)` as an f-string shows a JSON value: only strings reach a
/// prompt (any other section name fails the section later), so other
/// values are shown as JSON.
fn python_str(value: &Value) -> String {
    match value {
        Value::String(text) => text.clone(),
        other => other.to_string(),
    }
}

/// `_adaptive_central_k(cluster_size)`.
fn adaptive_k(size: usize) -> usize {
    #[allow(
        clippy::cast_precision_loss,
        clippy::cast_possible_truncation,
        clippy::cast_sign_loss
    )]
    let root = (size as f64).sqrt().ceil() as usize;
    root.clamp(3, 15)
}

fn page_rationale(macro_id: i64, micro_id: i64, size: usize) -> String {
    format!("Grouped by graph clustering (macro={macro_id}, micro={micro_id}, {size} symbols)")
}

/// The number of distinct non-empty `rel_path`s.
fn distinct_files(index: &PlannerIndex, nodes: impl Iterator<Item = usize>) -> usize {
    nodes
        .map(|p| index.node(p).rel_path.as_str())
        .filter(|rel| !rel.is_empty())
        .collect::<IndexSet<_>>()
        .len()
}

/// `_node_ids_to_paths`: sorted unique `rel_path`s, at most 20.
fn paths(index: &PlannerIndex, nodes: &[usize]) -> Vec<String> {
    sorted_unique(
        nodes
            .iter()
            .map(|&p| index.node(p).rel_path.as_str())
            .filter(|r| !r.is_empty()),
        20,
    )
}

/// `_node_ids_to_folders`: sorted unique directories, at most 10.
fn folders(index: &PlannerIndex, nodes: &[usize]) -> Vec<String> {
    sorted_unique(
        nodes
            .iter()
            .filter_map(|&p| index.node(p).rel_path.rsplit_once('/').map(|(dir, _)| dir)),
        10,
    )
}

/// `_node_ids_to_doc_paths`: sorted unique paths of doc nodes, at most 20.
fn doc_paths(index: &PlannerIndex, nodes: &[usize]) -> Vec<String> {
    sorted_unique(
        nodes
            .iter()
            .map(|&p| index.node(p))
            .filter(|node| node.is_doc && !node.rel_path.is_empty())
            .map(|node| node.rel_path.as_str()),
        20,
    )
}

fn sorted_unique<'s>(items: impl Iterator<Item = &'s str>, limit: usize) -> Vec<String> {
    let mut items: Vec<&str> = items.collect();
    items.sort_unstable();
    items.dedup();
    items.into_iter().take(limit).map(str::to_owned).collect()
}

/// `os.path.commonpath` of relative POSIX paths.
fn common_path(paths: &[String]) -> String {
    let split: Vec<Vec<&str>> = paths
        .iter()
        .map(|p| {
            p.split('/')
                .filter(|c| !c.is_empty() && *c != ".")
                .collect()
        })
        .collect();
    let (Some(low), Some(high)) = (split.iter().min(), split.iter().max()) else {
        return String::new();
    };
    let common = low.iter().zip(high).take_while(|(a, b)| a == b).count();
    low[..common].join("/")
}

/// `_derive_wiki_title`.
#[must_use]
pub fn derive_wiki_title(repo_identifier: Option<&str>) -> String {
    match repo_identifier.filter(|id| !id.is_empty()) {
        Some(id) => {
            let name = if id.contains('/') {
                let last = id.rsplit('/').next().unwrap_or(id);
                last.split(':').next().unwrap_or(last)
            } else {
                id
            };
            format!("{name} \u{2014} Technical Documentation")
        }
        None => "Repository Technical Documentation".to_owned(),
    }
}

/// `_build_overview`.
fn build_overview(sections: &[SectionSpec]) -> String {
    let names: Vec<&str> = sections.iter().map(|s| s.section_name.as_str()).collect();
    format!(
        "This wiki covers {} pages across {} sections: {}.",
        sections.iter().map(|s| s.pages.len()).sum::<usize>(),
        sections.len(),
        names.join(", ")
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn titles_and_paths_follow_python() {
        assert_eq!(
            derive_wiki_title(Some("acme/petclinic:main:0123abcd")),
            "petclinic \u{2014} Technical Documentation"
        );
        assert_eq!(
            derive_wiki_title(Some("solo:main:1")),
            "solo:main:1 \u{2014} Technical Documentation"
        );
        assert_eq!(
            derive_wiki_title(None),
            "Repository Technical Documentation"
        );
        let paths: Vec<String> = ["src/a/x.py", "src/a/y.py", "src/b.py"]
            .iter()
            .map(|s| (*s).to_owned())
            .collect();
        assert_eq!(common_path(&paths), "src");
        let paths: Vec<String> = ["a.py", "b.py"].iter().map(|s| (*s).to_owned()).collect();
        assert_eq!(common_path(&paths), "");
        assert_eq!(adaptive_k(1), 3);
        assert_eq!(adaptive_k(30), 6);
        assert_eq!(adaptive_k(100), 10);
        assert_eq!(adaptive_k(1000), 15);
    }

    #[test]
    fn naming_switches_read_both_spellings() {
        let env = |pairs: &'static [(&'static str, &'static str)]| {
            move |name: &str| {
                pairs
                    .iter()
                    .find(|(k, _)| *k == name)
                    .map(|(_, v)| (*v).to_owned())
            }
        };
        assert_eq!(
            ClusterSettings::from_lookup(false, env(&[])),
            ClusterSettings::default()
        );
        let off = ClusterSettings::from_lookup(true, env(&[("WIKI_NAMING_BATCHED", " off ")]));
        assert!(!off.naming_batched && off.exclude_tests);
        let blank = ClusterSettings::from_lookup(
            false,
            env(&[
                ("DEEPWIKI_NAMING_BATCHED", " "),
                ("WIKI_NAMING_BATCHED", "no"),
            ]),
        );
        assert!(!blank.naming_batched);
        let odd = ClusterSettings::from_lookup(false, env(&[("DEEPWIKI_NAMING_BATCHED", "maybe")]));
        assert!(odd.naming_batched);
        let sections_first = ClusterSettings::from_lookup(
            false,
            env(&[("DEEPWIKI_NAMING_ORDER", "sections_first")]),
        );
        assert!(!sections_first.pages_first);
    }
}
