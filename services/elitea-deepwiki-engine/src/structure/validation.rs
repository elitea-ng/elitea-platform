//! Page candidates, their validation, and the coverage ledger:
//! `wiki_structure_planner/candidate_builder.py`, `page_validator.py` and
//! `coverage_ledger.py` (the parts the cluster planner calls).
//!
//! `capability_validation` and `coverage_ledger` are hard-coded on in
//! `feature_flags.py`. The ledger's report is only logged: nothing in the
//! structure depends on it.

// `macro_id` / `micro_id` are Python's names for the section and page ids.
#![allow(clippy::similar_names)]

use super::index::PlannerIndex;
use std::collections::HashSet;

/// `PAGE_IDENTITY_SYMBOLS`.
pub const PAGE_IDENTITY_SYMBOLS: &[&str] = &[
    "class",
    "interface",
    "trait",
    "protocol",
    "enum",
    "struct",
    "record",
    "module",
    "namespace",
    "function",
    "sql_table",
    "sql_view",
    "sql_function",
];

/// `SUPPORTING_CODE_SYMBOLS`.
pub const SUPPORTING_CODE_SYMBOLS: &[&str] = &[
    "constant",
    "type_alias",
    "macro",
    "method",
    "property",
    "sql_column",
    "sql_index",
    "sql_trigger",
    "sql_schema",
];

/// `DOC_CLUSTER_SYMBOLS`.
pub const DOC_CLUSTER_SYMBOLS: &[&str] = &[
    "markdown_document",
    "plaintext_document",
    "yaml_document",
    "json_document",
    "config_document",
    "infrastructure_document",
    "script_document",
    "html_document",
    "schema_document",
    "markdown_section",
    "text_chunk",
    "module_doc",
    "file_doc",
];

/// `SYMBOL_TYPE_PRIORITY` (0 for a type it does not list).
#[must_use]
pub fn symbol_type_priority(symbol_type: &str) -> u8 {
    match symbol_type {
        "class" | "interface" | "trait" | "protocol" => 10,
        "enum" | "struct" | "record" | "sql_table" | "sql_view" => 9,
        "module" | "namespace" | "sql_schema" => 8,
        "function" | "sql_function" => 7,
        "constant" | "type_alias" | "macro" => 6,
        "contract" => 5,
        "method" | "sql_index" | "sql_trigger" => 3,
        "property" | "sql_column" => 2,
        other if DOC_CLUSTER_SYMBOLS.contains(&other) => 1,
        _ => 0,
    }
}

/// `_UTILITY_PATTERNS`.
const UTILITY_PATTERNS: &[&str] = &[
    "util", "helper", "misc", "common", "base", "mixin", "compat", "internal", "private",
];

/// `_MIN_IMPL_LENGTH`, in characters.
const MIN_IMPL_LENGTH: usize = 50;

/// A candidate's classification.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Classification {
    Code,
    Mixed,
    Docs,
    Bridge,
}

/// `CandidateRecord`: one page (micro-cluster) and its metrics.
#[derive(Debug, Clone, PartialEq)]
pub struct Candidate {
    pub macro_id: i64,
    pub micro_id: i64,
    /// Index positions.
    pub node_ids: Vec<usize>,
    pub classification: Classification,
    pub code_identity_score: f64,
    pub implementation_evidence: f64,
    pub docs_dominance: f64,
    pub public_api_presence: f64,
    pub utility_contamination: f64,
    pub file_spread: usize,
}

/// The planner's working map: section → page → node positions, in Python
/// dict order (insertion order, deletions closing the gap).
pub type ClusterMap = indexmap::IndexMap<i64, indexmap::IndexMap<i64, Vec<usize>>>;

/// `build_candidates`: one record per non-empty page, sections and pages
/// in id order.
#[must_use]
pub fn build_candidates(index: &PlannerIndex, cluster_map: &ClusterMap) -> Vec<Candidate> {
    let mut macros: Vec<&i64> = cluster_map.keys().collect();
    macros.sort_unstable();
    let mut candidates = Vec::new();
    for macro_id in macros {
        let pages = &cluster_map[macro_id];
        let mut micros: Vec<&i64> = pages.keys().collect();
        micros.sort_unstable();
        for micro_id in micros {
            let node_ids = &pages[micro_id];
            if node_ids.is_empty() {
                continue;
            }
            candidates.push(build_one(index, *macro_id, *micro_id, node_ids));
        }
    }
    candidates
}

#[allow(clippy::cast_precision_loss)]
fn ratio(count: usize, total: usize) -> f64 {
    count as f64 / total as f64
}

fn build_one(index: &PlannerIndex, macro_id: i64, micro_id: i64, node_ids: &[usize]) -> Candidate {
    // `SELECT * … WHERE node_id IN (…)`: each node once.
    let unique: HashSet<usize> = node_ids.iter().copied().collect();
    let total = unique.len().max(1);
    let (mut identity, mut supporting, mut docs, mut implemented, mut public, mut utility) =
        (0, 0, 0, 0, 0, 0);
    let mut files: HashSet<&str> = HashSet::new();
    for &position in &unique {
        let node = index.node(position);
        let stype = node.symbol_type.to_lowercase();
        let name = node.symbol_name.to_lowercase();
        let is_doc = DOC_CLUSTER_SYMBOLS.contains(&stype.as_str());
        if PAGE_IDENTITY_SYMBOLS.contains(&stype.as_str()) {
            identity += 1;
        } else if SUPPORTING_CODE_SYMBOLS.contains(&stype.as_str()) {
            supporting += 1;
        } else if is_doc {
            docs += 1;
        }
        let source = node.source_text.as_deref().unwrap_or("");
        if !is_doc && source.chars().nth(MIN_IMPL_LENGTH - 1).is_some() {
            implemented += 1;
        }
        if !is_doc && !node.symbol_name.is_empty() && !node.symbol_name.starts_with('_') {
            public += 1;
        }
        if UTILITY_PATTERNS
            .iter()
            .any(|pattern| name.contains(pattern))
        {
            utility += 1;
        }
        if !node.rel_path.is_empty() {
            files.insert(&node.rel_path);
        }
    }
    let code = identity + supporting;
    let code_identity_score = ratio(identity, total);
    let docs_dominance = ratio(docs, total);
    let file_spread = files.len();
    let classification = classify(docs_dominance, code, docs, file_spread, total);
    Candidate {
        macro_id,
        micro_id,
        node_ids: node_ids.to_vec(),
        classification,
        code_identity_score,
        implementation_evidence: ratio(implemented, code.max(1)),
        docs_dominance,
        public_api_presence: ratio(public, code.max(1)),
        utility_contamination: ratio(utility, total),
        file_spread,
    }
}

/// `_classify`.
fn classify(
    docs_dominance: f64,
    code: usize,
    docs: usize,
    file_spread: usize,
    total: usize,
) -> Classification {
    if docs_dominance >= 0.7 {
        return Classification::Docs;
    }
    #[allow(clippy::cast_precision_loss)]
    if file_spread >= 3 && file_spread as f64 > total as f64 / 2.0 {
        return Classification::Bridge;
    }
    if docs == 0 && code > 0 {
        return Classification::Code;
    }
    if code > 0 && docs > 0 {
        return Classification::Mixed;
    }
    Classification::Code
}

/// A shaping action (`page_validator`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Shape {
    Keep,
    MergeWith,
    SplitBy,
    PromoteDocs,
    Demote,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Severity {
    Pass,
    Warn,
    Fail,
}

/// `MIN_IDENTITY_SCORE`.
const MIN_IDENTITY_SCORE: f64 = 0.1;
/// `MIN_PAGE_SIZE`.
const MIN_PAGE_SIZE: usize = 2;
/// `MAX_PAGE_SIZE`, before scaling.
const MAX_PAGE_SIZE: usize = 60;
/// `MAX_UTILITY_CONTAMINATION`.
const MAX_UTILITY_CONTAMINATION: f64 = 0.5;
/// `MIN_IMPL_EVIDENCE`.
const MIN_IMPL_EVIDENCE: f64 = 0.2;

/// One candidate's checks and decision (`ValidationResult`).
#[derive(Debug, Clone, PartialEq)]
pub struct Validation {
    pub macro_id: i64,
    pub micro_id: i64,
    pub shape: Shape,
}

/// `validate_all`: the five checks per candidate, with `max_page_size`
/// scaled to `max(60, int(1.5 × average size))`.
#[must_use]
pub fn validate_all(candidates: &[Candidate]) -> Vec<Validation> {
    let max_page_size = if candidates.is_empty() {
        MAX_PAGE_SIZE
    } else {
        let total: usize = candidates.iter().map(|c| c.node_ids.len()).sum();
        let average = ratio(total, candidates.len());
        #[allow(clippy::cast_possible_truncation, clippy::cast_sign_loss)]
        let scaled = (average * 1.5) as usize;
        MAX_PAGE_SIZE.max(scaled)
    };
    let validations: Vec<Validation> = candidates
        .iter()
        .map(|candidate| {
            let has_sibling = candidates
                .iter()
                .any(|s| s.macro_id == candidate.macro_id && s.micro_id != candidate.micro_id);
            Validation {
                macro_id: candidate.macro_id,
                micro_id: candidate.micro_id,
                shape: decide(candidate, max_page_size, has_sibling),
            }
        })
        .collect();
    tracing::info!(
        candidates = validations.len(),
        max_page_size,
        "[PAGE_VALIDATOR] validated"
    );
    validations
}

/// The five checks' (severity, suggested action), then `_decide_shape`.
fn decide(candidate: &Candidate, max_page_size: usize, has_sibling: bool) -> Shape {
    let docs = candidate.classification == Classification::Docs;
    let identity = if docs || candidate.code_identity_score >= MIN_IDENTITY_SCORE {
        (Severity::Pass, None)
    } else if candidate.code_identity_score > 0.0 {
        (Severity::Warn, Some(Shape::MergeWith))
    } else {
        (Severity::Fail, Some(Shape::Demote))
    };
    let coherence = if candidate.utility_contamination >= MAX_UTILITY_CONTAMINATION
        || candidate.node_ids.len() > max_page_size
    {
        (Severity::Warn, Some(Shape::SplitBy))
    } else {
        (Severity::Pass, None)
    };
    let grounding = if docs || candidate.implementation_evidence >= MIN_IMPL_EVIDENCE {
        (Severity::Pass, None)
    } else {
        (Severity::Warn, Some(Shape::MergeWith))
    };
    let coverage = if candidate.node_ids.len() < MIN_PAGE_SIZE {
        (Severity::Fail, Some(Shape::MergeWith))
    } else {
        (Severity::Pass, None)
    };
    let shape = match candidate.classification {
        Classification::Docs => (Severity::Pass, Some(Shape::PromoteDocs)),
        Classification::Bridge => (Severity::Warn, Some(Shape::SplitBy)),
        _ => (Severity::Pass, None),
    };
    let checks = [identity, coherence, grounding, coverage, shape];
    let failing = |severity: Severity, action: Shape| {
        checks
            .iter()
            .any(|(s, a)| *s == severity && *a == Some(action))
    };
    if failing(Severity::Fail, Shape::Demote) {
        return Shape::Demote;
    }
    if failing(Severity::Fail, Shape::MergeWith) {
        return Shape::MergeWith;
    }
    if docs {
        return Shape::PromoteDocs;
    }
    let warned = |action: Shape| {
        checks
            .iter()
            .any(|(s, a)| matches!(s, Severity::Fail | Severity::Warn) && *a == Some(action))
    };
    if warned(Shape::SplitBy) {
        return Shape::SplitBy;
    }
    if warned(Shape::MergeWith) && has_sibling {
        return Shape::MergeWith;
    }
    Shape::Keep
}

/// `CoverageLedger(...).report()` for the whole wiki: the counts the
/// planner logs.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct CoverageReport {
    pub total_symbols: usize,
    pub covered_symbols: usize,
    /// High-value (priority ≥ 7) architectural symbols on no page.
    pub uncovered_high_value: usize,
    pub total_doc_domains: usize,
    pub covered_doc_domains: usize,
    pub total_directories: usize,
    pub covered_directories: usize,
    /// Candidate pairs sharing more than half of the smaller one.
    pub page_overlap_pairs: usize,
}

fn dir_of(rel_path: &str) -> &str {
    match rel_path.rfind('/') {
        Some(at) if at > 0 => &rel_path[..at],
        _ => ".",
    }
}

/// The ledger over `candidates` (every architectural node is the
/// universe, test nodes included, as Python's query has no test filter).
#[must_use]
pub fn coverage_report(index: &PlannerIndex, candidates: &[Candidate]) -> CoverageReport {
    let mut doc_dirs: HashSet<&str> = HashSet::new();
    let mut source_dirs: HashSet<&str> = HashSet::new();
    let mut universe: HashSet<usize> = HashSet::new();
    for (position, node) in index.nodes().iter().enumerate() {
        if !node.is_architectural {
            continue;
        }
        universe.insert(position);
        if !node.rel_path.is_empty() {
            if node.is_doc {
                doc_dirs.insert(dir_of(&node.rel_path));
            } else {
                source_dirs.insert(dir_of(&node.rel_path));
            }
        }
    }
    let mut covered: HashSet<usize> = HashSet::new();
    let mut covered_dirs: HashSet<&str> = HashSet::new();
    let mut covered_doc_dirs: HashSet<&str> = HashSet::new();
    for candidate in candidates {
        for &position in &candidate.node_ids {
            covered.insert(position);
            if !universe.contains(&position) {
                continue;
            }
            let node = index.node(position);
            if !node.rel_path.is_empty() {
                covered_dirs.insert(dir_of(&node.rel_path));
                if node.is_doc {
                    covered_doc_dirs.insert(dir_of(&node.rel_path));
                }
            }
        }
    }
    let uncovered_high_value = universe
        .iter()
        .filter(|p| !covered.contains(p))
        .filter(|&&p| symbol_type_priority(&index.node(p).symbol_type.to_lowercase()) >= 7)
        .count()
        .min(20);
    let all_dirs: HashSet<&str> = source_dirs.union(&doc_dirs).copied().collect();
    let mut overlaps = 0;
    for (i, a) in candidates.iter().enumerate() {
        let a_set: HashSet<usize> = a.node_ids.iter().copied().collect();
        for b in &candidates[i + 1..] {
            let b_set: HashSet<usize> = b.node_ids.iter().copied().collect();
            let shared = a_set.intersection(&b_set).count();
            let smaller = a_set.len().min(b_set.len());
            if smaller > 0 && shared * 2 > smaller {
                overlaps += 1;
            }
        }
    }
    CoverageReport {
        total_symbols: universe.len(),
        covered_symbols: universe.intersection(&covered).count(),
        uncovered_high_value,
        total_doc_domains: doc_dirs.len(),
        covered_doc_domains: doc_dirs.intersection(&covered_doc_dirs).count(),
        total_directories: all_dirs.len(),
        covered_directories: all_dirs.intersection(&covered_dirs).count(),
        page_overlap_pairs: overlaps,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn candidate(classification: Classification, size: usize) -> Candidate {
        Candidate {
            macro_id: 0,
            micro_id: 0,
            node_ids: (0..size).collect(),
            classification,
            code_identity_score: 0.5,
            implementation_evidence: 0.5,
            docs_dominance: 0.0,
            public_api_presence: 1.0,
            utility_contamination: 0.0,
            file_spread: 1,
        }
    }

    #[test]
    fn decisions_follow_the_validator_precedence() {
        assert_eq!(
            decide(&candidate(Classification::Code, 5), 60, true),
            Shape::Keep
        );
        assert_eq!(
            decide(&candidate(Classification::Code, 1), 60, true),
            Shape::MergeWith
        );
        assert_eq!(
            decide(&candidate(Classification::Docs, 5), 60, true),
            Shape::PromoteDocs
        );
        assert_eq!(
            decide(&candidate(Classification::Bridge, 5), 60, true),
            Shape::SplitBy
        );
        assert_eq!(
            decide(&candidate(Classification::Code, 61), 60, true),
            Shape::SplitBy
        );
        let mut none = candidate(Classification::Code, 5);
        none.code_identity_score = 0.0;
        assert_eq!(decide(&none, 60, true), Shape::Demote);
        let mut low = candidate(Classification::Code, 5);
        low.code_identity_score = 0.05;
        assert_eq!(decide(&low, 60, true), Shape::MergeWith);
        assert_eq!(decide(&low, 60, false), Shape::Keep);
    }
}
