//! `hierarchical_leiden_cluster`: sections on the file graph, pages per
//! section on the nodes.
//!
//! WHY a file graph for sections: parameters, variables and fields
//! fragment a node-level partition; summed into file-pair weights, their
//! cross-file edges still pull files together without adding vertices.

use super::partitioner::{Level, PartitionRequest, Partitioner};
use super::sizing::dir_of;
use super::{ALGORITHM, AlgorithmMetadata, ClusterError, ClusterGraph, Clustering, Pages};
use indexmap::{IndexMap, IndexSet};

/// The contracted graph: files (path → nodes) and file-pair weights.
pub type FileGraph<'g> = (IndexMap<&'g str, Vec<usize>>, IndexMap<(usize, usize), f64>);

/// The path a node contracts to: `rel_path or file_name or "<unknown>"`.
fn file_of(graph: &ClusterGraph, node: usize) -> &str {
    let path = graph.path_of(node);
    if path.is_empty() { "<unknown>" } else { path }
}

/// The file graph of the nodes in `sub` (`_contract_to_file_graph`): the
/// files in first-node order with their nodes in graph order, and the
/// summed weight per unordered file pair, keyed `(lesser, greater)` by
/// path, in first-edge order.
///
/// Weights are summed in networkx edge order, as Python does, so the sums
/// are bit-equal.
#[must_use]
pub fn contract_to_file_graph<'g>(graph: &'g ClusterGraph, sub: &[bool]) -> FileGraph<'g> {
    let mut files: IndexMap<&str, Vec<usize>> = IndexMap::new();
    let mut file_index = vec![usize::MAX; graph.node_count()];
    for node in (0..graph.node_count()).filter(|n| sub[*n]) {
        let entry = files.entry(file_of(graph, node));
        file_index[node] = entry.index();
        entry.or_default().push(node);
    }
    let mut pairs: IndexMap<(usize, usize), f64> = IndexMap::new();
    for u in (0..graph.node_count()).filter(|n| sub[*n]) {
        for entry in graph.successors(u) {
            if !sub[entry.target] {
                continue;
            }
            let (fu, fv) = (file_index[u], file_index[entry.target]);
            if fu == fv {
                continue;
            }
            let (name_u, name_v) = (
                files.get_index(fu).map(|f| *f.0),
                files.get_index(fv).map(|f| *f.0),
            );
            let key = if name_u <= name_v { (fu, fv) } else { (fv, fu) };
            for weight in &entry.weights {
                *pairs.entry(key).or_insert(0.0) += weight;
            }
        }
    }
    (files, pairs)
}

/// `hierarchical_leiden_cluster` on `G_cluster` (`in_cluster`) without
/// `hubs`.
///
/// # Errors
///
/// When the partitioner fails.
#[allow(clippy::too_many_lines)] // one Python function, kept in one piece
pub fn hierarchical_leiden_cluster(
    graph: &ClusterGraph,
    in_cluster: &[bool],
    hubs: &IndexSet<usize>,
    section_resolution: f64,
    page_resolution: f64,
    seed: u64,
    partitioner: &mut dyn Partitioner,
) -> Result<Clustering, ClusterError> {
    let sub: Vec<bool> = (0..graph.node_count())
        .map(|n| in_cluster[n] && !hubs.contains(&n))
        .collect();
    if !sub.iter().any(|keep| *keep) {
        return Ok(Clustering {
            metadata: AlgorithmMetadata {
                algorithm: ALGORITHM,
                section_resolution,
                page_resolution,
                note: Some("no non-hub nodes"),
                ..AlgorithmMetadata::default()
            },
            ..Clustering::default()
        });
    }

    // ── Pass 1: sections on the file graph ──
    let (files, pairs) = contract_to_file_graph(graph, &sub);
    let mut connected_flag = vec![false; files.len()];
    for &(a, b) in pairs.keys() {
        connected_flag[a] = true;
        connected_flag[b] = true;
    }
    let connected: Vec<usize> = (0..files.len()).filter(|f| connected_flag[*f]).collect();
    let isolated: Vec<usize> = (0..files.len()).filter(|f| !connected_flag[*f]).collect();
    let file_name = |f: usize| files.get_index(f).map_or("", |(name, _)| *name);

    // file → section, in the order Python fills `file_to_section`.
    let mut file_to_section: IndexMap<usize, usize> = IndexMap::new();
    if !connected.is_empty() {
        let mut sorted = connected.clone();
        sorted.sort_by(|a, b| file_name(*a).cmp(file_name(*b)));
        let mut vertex = vec![usize::MAX; files.len()];
        for (i, f) in sorted.iter().enumerate() {
            vertex[*f] = i;
        }
        let names: Vec<&str> = sorted.iter().map(|f| file_name(*f)).collect();
        let edges: Vec<(usize, usize, f64)> = pairs
            .iter()
            .map(|(&(a, b), &w)| (vertex[a], vertex[b], w))
            .collect();
        let membership = partition(
            partitioner,
            &PartitionRequest {
                level: Level::Section,
                names: &names,
                edges: &edges,
                resolution: section_resolution,
                seed,
            },
        )?;
        for (i, f) in sorted.iter().enumerate() {
            file_to_section.insert(*f, membership[i]);
        }
    }
    if !isolated.is_empty() && !file_to_section.is_empty() {
        // Directory histogram per section over the connected files.
        let mut section_dirs: IndexMap<usize, IndexMap<&str, usize>> = IndexMap::new();
        for (&f, &section) in &file_to_section {
            *section_dirs
                .entry(section)
                .or_default()
                .entry(dir_of(file_name(f)))
                .or_insert(0) += 1;
        }
        for &f in &isolated {
            let dir = dir_of(file_name(f));
            // `max(sec_dirs, key=...)`: the FIRST section with the most.
            let mut best: Option<(usize, usize)> = None;
            for (&section, dirs) in &section_dirs {
                let count = dirs.get(dir).copied().unwrap_or(0);
                if best.is_none_or(|(_, top)| count > top) {
                    best = Some((section, count));
                }
            }
            if let Some((section, _)) = best {
                file_to_section.insert(f, section);
            }
        }
    } else {
        for (i, &f) in isolated.iter().enumerate() {
            file_to_section.insert(f, i);
        }
    }

    let mut macro_assignments: IndexMap<usize, usize> = IndexMap::new();
    for (&f, &section) in &file_to_section {
        if let Some((_, nodes)) = files.get_index(f) {
            for &node in nodes {
                macro_assignments.insert(node, section);
            }
        }
    }

    // ── Pass 2: pages per section on the nodes ──
    let mut section_nodes: IndexMap<usize, Vec<usize>> = IndexMap::new();
    for (&node, &section) in &macro_assignments {
        section_nodes.entry(section).or_default().push(node);
    }
    let mut sections: IndexMap<usize, Pages> = IndexMap::new();
    let mut page_of: IndexMap<usize, IndexMap<usize, usize>> = IndexMap::new();
    let mut member = vec![false; graph.node_count()];
    for (&section, nodes) in &section_nodes {
        let single_page = |list: Vec<usize>| {
            let micro: IndexMap<usize, usize> = list.iter().map(|n| (*n, 0)).collect();
            (IndexMap::from([(0, list)]), micro)
        };
        if nodes.len() < 2 {
            let (pages, micro) = single_page(nodes.clone());
            sections.insert(section, pages);
            page_of.insert(section, micro);
            continue;
        }
        let mut sorted = nodes.clone();
        sorted.sort_by(|a, b| graph.node(*a).id.cmp(&graph.node(*b).id));
        for &n in nodes {
            member[n] = true;
        }
        let edges = section_edges(graph, &sorted, &member);
        for &n in nodes {
            member[n] = false;
        }
        if edges.is_empty() {
            let (pages, micro) = single_page(sorted);
            sections.insert(section, pages);
            page_of.insert(section, micro);
            continue;
        }
        let names: Vec<&str> = sorted.iter().map(|n| graph.node(*n).id.as_str()).collect();
        let membership = partition(
            partitioner,
            &PartitionRequest {
                level: Level::Page,
                names: &names,
                edges: &edges,
                resolution: page_resolution,
                seed,
            },
        )?;
        let mut raw: IndexMap<usize, Vec<usize>> = IndexMap::new();
        for (i, page) in membership.iter().enumerate() {
            raw.entry(*page).or_default().push(sorted[i]);
        }
        // Renumber to 0..k in page id order (`sorted(pages.items())`).
        raw.sort_keys();
        let mut pages = Pages::new();
        let mut micro = IndexMap::new();
        for (new_id, (_, list)) in raw.into_iter().enumerate() {
            for &n in &list {
                micro.insert(n, new_id);
            }
            pages.insert(new_id, list);
        }
        sections.insert(section, pages);
        page_of.insert(section, micro);
    }

    let n_pages = sections.values().map(IndexMap::len).sum();
    Ok(Clustering {
        metadata: AlgorithmMetadata {
            algorithm: ALGORITHM,
            section_resolution,
            page_resolution,
            sections: sections.len(),
            pages: n_pages,
            seed: Some(seed),
            file_nodes: Some(file_to_section.len()),
            connected_files: Some(connected.len()),
            isolated_files: Some(isolated.len()),
            note: None,
        },
        sections,
        macro_assignments,
        micro_assignments: page_of,
    })
}

/// `_to_weighted_undirected` of the section subgraph, as vertex pairs of
/// `sorted` (the section's nodes in id order): parallel and opposite edges
/// summed per unordered pair, self-loops kept.
///
/// Python visits the edges in the subgraph's node order, which for a small
/// section is a `set`'s hash order; this visits them in graph order, so a
/// pair with several edges may differ from Python in the last bits of its
/// sum. The replay gate checks the weights within 1e-9.
fn section_edges(
    graph: &ClusterGraph,
    sorted: &[usize],
    member: &[bool],
) -> Vec<(usize, usize, f64)> {
    let mut vertex: std::collections::HashMap<usize, usize> = std::collections::HashMap::new();
    for (i, n) in sorted.iter().enumerate() {
        vertex.insert(*n, i);
    }
    let mut in_graph_order: Vec<usize> = sorted.to_vec();
    in_graph_order.sort_unstable();
    let mut pairs: IndexMap<(usize, usize), f64> = IndexMap::new();
    for u in in_graph_order {
        for entry in graph.successors(u) {
            if !member[entry.target] {
                continue;
            }
            let (a, b) = (vertex[&u], vertex[&entry.target]);
            // Vertices are in id order, so the lesser vertex is the lesser id.
            let key = (a.min(b), a.max(b));
            for weight in &entry.weights {
                *pairs.entry(key).or_insert(0.0) += weight;
            }
        }
    }
    pairs.into_iter().map(|((a, b), w)| (a, b, w)).collect()
}

/// Call the partitioner and check the membership length.
fn partition(
    partitioner: &mut dyn Partitioner,
    request: &PartitionRequest<'_>,
) -> Result<Vec<usize>, ClusterError> {
    let membership = partitioner.partition(request)?;
    if membership.len() == request.names.len() {
        Ok(membership)
    } else {
        Err(super::PartitionError::WrongLength {
            got: membership.len(),
            expected: request.names.len(),
        }
        .into())
    }
}
