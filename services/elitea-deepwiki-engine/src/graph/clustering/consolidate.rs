//! `_consolidate_sections` and `_consolidate_pages`: merge the smallest
//! clusters into their best sibling until the counts reach the targets.
//!
//! The best sibling has the most edges to the merging cluster (in the FULL
//! graph, both directions, parallel edges counted), then the largest
//! directory overlap, then the smallest size; the first in Python's dict
//! order wins a full tie. WHY edges first: they are semantic coupling,
//! directory overlap is only a hint.
//!
//! Python recounts the edges between the merging cluster and every sibling
//! with `G.out_edges(set)`; here one pass over the merging cluster's out-
//! and in-edges counts them for all siblings at once, using an owner per
//! node. The counts are the same integers.

use super::sizing::{dir_of, target_section_count, target_total_pages};
use super::{ClusterGraph, Clustering};
use indexmap::IndexMap;
use std::collections::{BTreeSet, HashMap};

/// A directory histogram (`Counter` of `_dir_of_node`), by interned
/// directory id.
type Dirs = HashMap<usize, usize>;

/// Directory ids per node, interned.
fn node_dirs(graph: &ClusterGraph) -> Vec<usize> {
    let mut ids: HashMap<&str, usize> = HashMap::new();
    (0..graph.node_count())
        .map(|node| {
            let next = ids.len();
            *ids.entry(dir_of(graph.path_of(node))).or_insert(next)
        })
        .collect()
}

fn histogram(nodes: &[usize], dirs: &[usize]) -> Dirs {
    let mut out = Dirs::new();
    for &node in nodes {
        *out.entry(dirs[node]).or_insert(0) += 1;
    }
    out
}

/// `_dir_similarity`: the shared count per directory, summed.
fn dir_similarity(a: &Dirs, b: &Dirs) -> usize {
    let (small, large) = if a.len() <= b.len() { (a, b) } else { (b, a) };
    small
        .iter()
        .filter_map(|(dir, count)| large.get(dir).map(|other| (*count).min(*other)))
        .sum()
}

fn merge_into(target: &mut Dirs, source: &Dirs) {
    for (dir, count) in source {
        *target.entry(*dir).or_insert(0) += count;
    }
}

/// Edges between `nodes` and every other owner, both directions: owner →
/// count. `owner[n]` is `None` for a node in no cluster (hubs, excluded
/// tests), which is in no sibling either.
fn cross_edges<K: Copy + Eq + std::hash::Hash>(
    graph: &ClusterGraph,
    nodes: &[usize],
    own: K,
    owner: &[Option<K>],
) -> HashMap<K, usize> {
    let mut counts: HashMap<K, usize> = HashMap::new();
    for &u in nodes {
        for entry in graph.successors(u) {
            if let Some(other) = owner[entry.target]
                && other != own
            {
                *counts.entry(other).or_insert(0) += entry.weights.len();
            }
        }
        for &(source, parallel) in graph.predecessors(u) {
            if let Some(other) = owner[source]
                && other != own
            {
                *counts.entry(other).or_insert(0) += parallel;
            }
        }
    }
    counts
}

/// The merge score `(edges, dir_similarity, -size)`, compared as Python
/// compares tuples.
fn score(edges: usize, similarity: usize, size: usize) -> (i128, i128, i128) {
    (
        i128::try_from(edges).unwrap_or(i128::MAX),
        i128::try_from(similarity).unwrap_or(i128::MAX),
        -i128::try_from(size).unwrap_or(i128::MAX),
    )
}

/// The score every candidate beats (`(-1, -1, 1)`).
const FLOOR: (i128, i128, i128) = (-1, -1, 1);

struct SectionInfo {
    dirs: Dirs,
    nodes: Vec<usize>,
}

/// `_consolidate_sections`: while there are more sections than
/// `target_section_count(n_files or node count)`, merge the smallest
/// (first in order on a tie) into its best sibling, appending its pages
/// with fresh ids after the sibling's largest.
pub fn consolidate_sections(
    clustering: &mut Clustering,
    graph: &ClusterGraph,
    n_files: Option<usize>,
) {
    let scale = n_files.unwrap_or(clustering.macro_assignments.len());
    let target = target_section_count(scale);
    if clustering.sections.len() <= target {
        return;
    }
    let dirs = node_dirs(graph);
    let mut owner: Vec<Option<usize>> = vec![None; graph.node_count()];
    let mut info: IndexMap<usize, SectionInfo> = IndexMap::new();
    for (&section, pages) in &clustering.sections {
        let nodes: Vec<usize> = pages.values().flatten().copied().collect();
        for &n in &nodes {
            owner[n] = Some(section);
        }
        info.insert(
            section,
            SectionInfo {
                dirs: histogram(&nodes, &dirs),
                nodes,
            },
        );
    }

    while clustering.sections.len() > target {
        // `min(sec_sizes, key=sec_sizes.get)`: the first smallest.
        let Some(smallest) = info
            .iter()
            .min_by_key(|(_, s)| s.nodes.len())
            .map(|(id, _)| *id)
        else {
            break;
        };
        let source = &info[&smallest];
        let counts = cross_edges(graph, &source.nodes, smallest, &owner);
        let mut best: Option<usize> = None;
        let mut best_key = FLOOR;
        for (&section, candidate) in &info {
            if section == smallest {
                continue;
            }
            let key = score(
                counts.get(&section).copied().unwrap_or(0),
                dir_similarity(&source.dirs, &candidate.dirs),
                candidate.nodes.len(),
            );
            if key > best_key {
                best_key = key;
                best = Some(section);
            }
        }
        let Some(best) = best else {
            break;
        };

        let source_pages = clustering
            .sections
            .shift_remove(&smallest)
            .unwrap_or_default();
        let target_pages = clustering.sections.entry(best).or_default();
        let first_page = target_pages.keys().max().map_or(0, |m| m + 1);
        let target_micro = clustering.micro_assignments.entry(best).or_default();
        for (next_page, (_, nodes)) in (first_page..).zip(source_pages) {
            for &n in &nodes {
                clustering.macro_assignments.insert(n, best);
                target_micro.insert(n, next_page);
            }
            target_pages.insert(next_page, nodes);
        }
        clustering.micro_assignments.shift_remove(&smallest);

        let Some(source) = info.shift_remove(&smallest) else {
            break;
        };
        for &n in &source.nodes {
            owner[n] = Some(best);
        }
        if let Some(target_info) = info.get_mut(&best) {
            merge_into(&mut target_info.dirs, &source.dirs);
            target_info.nodes.extend(source.nodes);
        }
    }
}

struct PageInfo {
    dirs: Dirs,
    nodes: Vec<usize>,
    /// Insertion rank in Python's `pg_sizes` dict: the tie-break of its
    /// stable sort by size.
    rank: usize,
}

/// `_consolidate_pages`: while the total page count exceeds
/// `target_total_pages(clustered nodes)`, merge the globally smallest page
/// that has a sibling (first in `pg_sizes` order on a tie) into its best
/// sibling in the same section.
pub fn consolidate_pages(clustering: &mut Clustering, graph: &ClusterGraph) {
    let target = target_total_pages(clustering.macro_assignments.len());
    let mut total: usize = clustering.sections.values().map(IndexMap::len).sum();
    if total <= target {
        return;
    }
    let dirs = node_dirs(graph);
    let mut owner: Vec<Option<(usize, usize)>> = vec![None; graph.node_count()];
    let mut info: HashMap<(usize, usize), PageInfo> = HashMap::new();
    // (size, rank) → page: Python's `sorted(pg_sizes.items(), key=size)`.
    let mut order: BTreeSet<(usize, usize)> = BTreeSet::new();
    let mut by_rank: Vec<(usize, usize)> = Vec::new();
    for (&section, pages) in &clustering.sections {
        for (&page, nodes) in pages {
            for &n in nodes {
                owner[n] = Some((section, page));
            }
            let rank = by_rank.len();
            by_rank.push((section, page));
            order.insert((nodes.len(), rank));
            info.insert(
                (section, page),
                PageInfo {
                    dirs: histogram(nodes, &dirs),
                    nodes: nodes.clone(),
                    rank,
                },
            );
        }
    }

    while total > target {
        // The first candidate whose section has a sibling page.
        let chosen = order.iter().find_map(|&(size, rank)| {
            let (section, page) = by_rank[rank];
            let siblings = clustering.sections.get(&section).map_or(0, IndexMap::len);
            (siblings > 1).then_some((size, section, page))
        });
        let Some((size, section, page)) = chosen else {
            break;
        };
        let source = &info[&(section, page)];
        let counts = cross_edges(graph, &source.nodes, (section, page), &owner);
        let mut best: Option<usize> = None;
        let mut best_key = FLOOR;
        for &other in clustering.sections[&section].keys() {
            if other == page {
                continue;
            }
            let candidate = &info[&(section, other)];
            let key = score(
                counts.get(&(section, other)).copied().unwrap_or(0),
                dir_similarity(&source.dirs, &candidate.dirs),
                candidate.nodes.len(),
            );
            if key > best_key {
                best_key = key;
                best = Some(other);
            }
        }
        let Some(best) = best else {
            break;
        };

        let pages = clustering.sections.entry(section).or_default();
        let moved = pages.shift_remove(&page).unwrap_or_default();
        let micro = clustering.micro_assignments.entry(section).or_default();
        for &n in &moved {
            micro.insert(n, best);
            owner[n] = Some((section, best));
        }
        pages.entry(best).or_default().extend(moved);

        let Some(source) = info.remove(&(section, page)) else {
            break;
        };
        order.remove(&(size, source.rank));
        if let Some(target_info) = info.get_mut(&(section, best)) {
            order.remove(&(target_info.nodes.len(), target_info.rank));
            merge_into(&mut target_info.dirs, &source.dirs);
            target_info.nodes.extend(source.nodes);
            order.insert((target_info.nodes.len(), target_info.rank));
        }
        total -= 1;
    }
}
