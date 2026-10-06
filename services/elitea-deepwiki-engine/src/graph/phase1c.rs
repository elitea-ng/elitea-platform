//! Phase 1c: the graph enrichment `filesystem_indexer._write_unified_db`
//! runs between the Phase 1 build and `from_networkx`.
//!
//! Order, as in Python: API surfaces, contract nodes, the cross-language
//! linker, the test linker, markdown structure. Each pass is flag-gated
//! ([`Phase1cFlags`]). Linker edges are added only when both endpoints
//! exist, with a fresh edge key.
//!
//! Python wraps the whole phase in one `try`: an exception logs "Phase 1c
//! enrichment failed (non-fatal)" and the index is written with whatever
//! the passes had added. The Rust passes have no failure path (a file that
//! cannot be read counts as empty, as Python's own `except` does), so
//! nothing here needs that fallback.

use super::cross_language::{self, HitsPerNode};
use super::flags::Phase1cFlags;
use super::{CodeGraph, EdgeData, api_surface, markdown_structure, test_linker};
use std::time::{Duration, Instant};

/// What Phase 1c did.
#[derive(Debug, Clone, Default)]
pub struct Phase1cReport {
    pub surface_nodes: usize,
    pub contract_nodes: usize,
    pub cross_language_edges: usize,
    pub test_link_edges: usize,
    pub markdown: markdown_structure::MarkdownStats,
    pub timings: Vec<(&'static str, Duration)>,
}

fn add_edges(graph: &mut CodeGraph, edges: Vec<(String, String, EdgeData)>) -> usize {
    let mut added = 0;
    for (source, target, data) in edges {
        if graph.has_node(&source) && graph.has_node(&target) {
            graph.add_edge(&source, &target, data);
            added += 1;
        }
    }
    added
}

/// Run Phase 1c over `graph`. `repo_root` is the canonical repository
/// path the API-surface pass reads files under.
pub fn run_phase1c(graph: &mut CodeGraph, repo_root: &str, flags: &Phase1cFlags) -> Phase1cReport {
    let mut report = Phase1cReport::default();
    let mut clock = Instant::now();
    let mut lap = |report: &mut Phase1cReport, name: &'static str| {
        report.timings.push((name, clock.elapsed()));
        clock = Instant::now();
    };

    let mut surfaces = api_surface::SurfacesByNode::new();
    if flags.api_surface_extraction {
        surfaces = api_surface::extract_api_surfaces_for_graph(graph, Some(repo_root));
        report.surface_nodes = surfaces.len();
        lap(&mut report, "api surfaces");
        if !surfaces.is_empty() {
            report.contract_nodes = api_surface::materialize_contract_nodes(graph, &surfaces);
        }
        lap(&mut report, "contract nodes");
    }
    if flags.cross_language_linking {
        let edges = cross_language::run_cross_language_linker(
            graph,
            &[],
            (!surfaces.is_empty()).then_some(&surfaces),
            &HitsPerNode::new(),
            &HitsPerNode::new(),
        );
        report.cross_language_edges = add_edges(graph, edges);
        lap(&mut report, "cross-language linker");
    }
    if flags.test_linker {
        let edges = test_linker::run_test_linker(graph);
        report.test_link_edges = add_edges(graph, edges);
        lap(&mut report, "test linker");
    }
    if flags.markdown_structure {
        report.markdown = markdown_structure::wire_markdown_structure(graph);
        lap(&mut report, "markdown structure");
    }
    tracing::info!(
        surface_nodes = report.surface_nodes,
        contract_nodes = report.contract_nodes,
        cross_language_edges = report.cross_language_edges,
        test_link_edges = report.test_link_edges,
        markdown_contains = report.markdown.contains_edges,
        markdown_references = report.markdown.references_edges,
        documents_synthesized = report.markdown.documents_synthesized,
        "phase 1c complete"
    );
    report
}
