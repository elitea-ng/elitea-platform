//! The consolidation passes as they were before [`super::Pool`]: every
//! sibling scored for every merge, as Python does. The reference the
//! indexed passes must equal; test-only.

use super::super::Clustering;
use super::super::sizing::{target_section_count, target_total_pages};
use super::{ClusterGraph, Dirs, cross_edges, dir_similarity, histogram, node_dirs};
use indexmap::IndexMap;
use std::collections::{BTreeSet, HashMap};

fn merge_into(target: &mut Dirs, source: &Dirs) {
    for (dir, count) in source {
        *target.entry(*dir).or_insert(0) += count;
    }
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
