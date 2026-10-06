//! Parity only: read a Phase 3 dump written by
//! `parity/python_phase3_dump.py` or `parity/python_phase3_fixture.py`.
//!
//! Until the Rust Phase 2 lands, the dump's Phase 2 output graph is the
//! input of the Rust Phase 3, so the clustering gate does not wait for it.

use super::{ClusterAssignment, ClusterGraph, ClusterNode, InputError, RecordedCall};
use serde::Deserialize;
use serde_json::Value;
use std::fs;
use std::path::Path;

/// One dump directory.
#[derive(Debug, Clone)]
pub struct Phase3Dump {
    /// The Phase 2 output graph.
    pub graph: ClusterGraph,
    /// The hub ids Python's Phase 3 received.
    pub hubs: Vec<String>,
    /// Every leidenalg call, in call order.
    pub calls: Vec<RecordedCall>,
    /// Python's cluster columns, sorted by node id.
    pub expected: Vec<ClusterAssignment>,
    /// `p3_summary.json`.
    pub summary: Value,
}

/// A dump that cannot be read.
#[derive(Debug, thiserror::Error)]
pub enum DumpError {
    #[error("{path}: {source}")]
    Io {
        path: String,
        source: std::io::Error,
    },
    #[error("{path}:{line}: {source}")]
    Json {
        path: String,
        line: usize,
        source: serde_json::Error,
    },
    #[error(transparent)]
    Graph(#[from] InputError),
}

#[derive(Deserialize)]
struct NodeLine {
    id: String,
    rel_path: String,
    file_name: String,
    is_doc: bool,
    pred: Vec<String>,
}

#[derive(Deserialize)]
struct HubsFile {
    phase3_hubs: Vec<String>,
}

fn read(dir: &Path, name: &str) -> Result<String, DumpError> {
    let path = dir.join(name);
    fs::read_to_string(&path).map_err(|source| DumpError::Io {
        path: path.display().to_string(),
        source,
    })
}

fn parse<T: for<'de> Deserialize<'de>>(
    text: &str,
    name: &str,
    line: usize,
) -> Result<T, DumpError> {
    serde_json::from_str(text).map_err(|source| DumpError::Json {
        path: name.to_owned(),
        line,
        source,
    })
}

fn lines<T: for<'de> Deserialize<'de>>(dir: &Path, name: &str) -> Result<Vec<T>, DumpError> {
    read(dir, name)?
        .lines()
        .enumerate()
        .filter(|(_, line)| !line.trim().is_empty())
        .map(|(i, line)| parse(line, name, i + 1))
        .collect()
}

/// Read the dump in `dir`.
///
/// # Errors
///
/// A missing or malformed file, or a graph that does not hold together.
pub fn load(dir: &Path) -> Result<Phase3Dump, DumpError> {
    let node_lines: Vec<NodeLine> = lines(dir, "p3_nodes.jsonl")?;
    let edges: Vec<(String, String, f64)> = lines(dir, "p3_edges.jsonl")?;
    let mut preds = Vec::with_capacity(node_lines.len());
    let mut nodes = Vec::with_capacity(node_lines.len());
    for line in node_lines {
        preds.push(line.pred);
        nodes.push(ClusterNode {
            id: line.id,
            rel_path: line.rel_path,
            file_name: line.file_name,
            is_doc: line.is_doc,
        });
    }
    let graph = ClusterGraph::from_parts(nodes, &edges, &preds)?;
    let hubs: HubsFile = parse(&read(dir, "p3_hubs.json")?, "p3_hubs.json", 1)?;
    Ok(Phase3Dump {
        graph,
        hubs: hubs.phase3_hubs,
        calls: lines(dir, "p3_leiden_calls.jsonl")?,
        expected: lines(dir, "p3_assignments.jsonl")?,
        summary: parse(&read(dir, "p3_summary.json")?, "p3_summary.json", 1)?,
    })
}
