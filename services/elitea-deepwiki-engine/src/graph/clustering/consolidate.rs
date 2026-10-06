//! `_consolidate_sections` and `_consolidate_pages`: merge the smallest
//! clusters into their best sibling until the counts reach the targets.
//!
//! The best sibling has the most edges to the merging cluster (in the FULL
//! graph, both directions, parallel edges counted), then the largest
//! directory overlap, then the smallest size; the first in Python's dict
//! order wins a full tie. WHY edges first: they are semantic coupling,
//! directory overlap is only a hint.
//!
//! Python scores every sibling for every merge: quadratic in the cluster
//! count (20k single-node pages took 5.9 s, 100k about 2.5 min). Here a
//! `Pool` finds the same sibling without looking at most of them:
//!
//! * Edges: one pass over the merging cluster's out- and in-edges counts
//!   them for all siblings at once, using an owner per node. A sibling
//!   with an edge beats every sibling without one, so when there is one,
//!   only those siblings are scored.
//! * Directory overlap: only a sibling that shares a directory scores above
//!   0. Per directory, the siblings that have it are kept ordered by
//!   (size, order); walking those lists together in that order, the first
//!   sibling with the highest overlap is the winner, and the walk stops
//!   once no sibling further on can overlap more.
//! * Otherwise the smallest sibling, first in order, wins: the first entry
//!   of the per-group (size, order) set that is not the merging cluster.
//!
//! The choices, and so the result, are those of Python's scan; the tests
//! compare the two on random clusterings.

use super::sizing::{dir_of, target_section_count, target_total_pages};
use super::{ClusterGraph, Clustering};
use indexmap::IndexMap;
use std::collections::{BTreeSet, HashMap, HashSet};
use std::ops::Bound::{Excluded, Unbounded};

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

/// A `(size, rank)` key: ascending is Python's "smallest, then first".
type SizeRank = (usize, usize);

/// The clusters of one consolidation pass, by RANK (their position in
/// Python's dict, which breaks ties), grouped into sibling sets: one group
/// for the sections, one per section for the pages.
struct Pool {
    group: Vec<usize>,
    nodes: Vec<Vec<usize>>,
    dirs: Vec<Dirs>,
    alive: Vec<bool>,
    /// Node → the rank of its cluster.
    owner: Vec<Option<usize>>,
    /// (group, directory) → the `(size, rank)` of the live clusters of the
    /// group that have the directory. An entry may hold an older, smaller
    /// size, or a rank merged away: a walk that meets it moves it to the
    /// current size, or drops it. Sizes only grow, so a walk in ascending
    /// order always meets an old entry before the place it moves to.
    by_dir: HashMap<(usize, usize), BTreeSet<SizeRank>>,
    /// Group → the `(size, rank)` of its live clusters, always current.
    siblings: HashMap<usize, BTreeSet<SizeRank>>,
}

impl Pool {
    /// `members[rank]` is `(group, nodes)`.
    fn new(graph: &ClusterGraph, members: Vec<(usize, Vec<usize>)>) -> Self {
        let node_dir = node_dirs(graph);
        let mut pool = Self {
            group: Vec::with_capacity(members.len()),
            nodes: Vec::with_capacity(members.len()),
            dirs: Vec::with_capacity(members.len()),
            alive: vec![true; members.len()],
            owner: vec![None; graph.node_count()],
            by_dir: HashMap::new(),
            siblings: HashMap::new(),
        };
        for (rank, (group, nodes)) in members.into_iter().enumerate() {
            for &n in &nodes {
                pool.owner[n] = Some(rank);
            }
            let dirs = histogram(&nodes, &node_dir);
            for &dir in dirs.keys() {
                pool.by_dir
                    .entry((group, dir))
                    .or_default()
                    .insert((nodes.len(), rank));
            }
            pool.siblings
                .entry(group)
                .or_default()
                .insert((nodes.len(), rank));
            pool.group.push(group);
            pool.nodes.push(nodes);
            pool.dirs.push(dirs);
        }
        pool
    }

    fn size(&self, rank: usize) -> usize {
        self.nodes[rank].len()
    }

    /// The number of live clusters in `rank`'s group, itself included.
    fn group_len(&self, rank: usize) -> usize {
        self.siblings
            .get(&self.group[rank])
            .map_or(0, BTreeSet::len)
    }

    /// The sibling `source` merges into: the highest `(edges,
    /// dir_similarity, -size)`, the first in order on a tie. `None` when
    /// the group has no other cluster.
    fn best_sibling(&mut self, graph: &ClusterGraph, source: usize) -> Option<usize> {
        let group = self.group[source];
        let counts = cross_edges(graph, &self.nodes[source], source, &self.owner);
        // (edges, similarity, size, rank): higher edges, then higher
        // similarity, then lower size, then lower rank wins.
        let mut best: Option<(usize, usize, usize, usize)> = None;
        for (&other, &edges) in &counts {
            if edges == 0 || self.group[other] != group {
                continue;
            }
            let candidate = (
                edges,
                dir_similarity(&self.dirs[source], &self.dirs[other]),
                self.size(other),
                other,
            );
            if best.is_none_or(|top| beats(candidate, top)) {
                best = Some(candidate);
            }
        }
        if let Some((.., rank)) = best {
            return Some(rank);
        }
        self.best_by_directory(source)
            .or_else(|| self.smallest_sibling(source))
    }

    /// Among the siblings sharing a directory with `source` (none has an
    /// edge to it), the highest overlap, then the smallest, then the first.
    fn best_by_directory(&mut self, source: usize) -> Option<usize> {
        let group = self.group[source];
        let Self {
            by_dir,
            nodes,
            alive,
            dirs,
            ..
        } = self;
        // (own count, directory, next entry) per walked list.
        let mut walks: Vec<(usize, usize, Option<SizeRank>)> = Vec::new();
        let mut bound = 0;
        for (&dir, &count) in &dirs[source] {
            if let Some(list) = by_dir.get_mut(&(group, dir))
                && let Some(next) = next_entry(list, None, nodes, alive)
            {
                walks.push((count, dir, Some(next)));
                bound += count;
            }
        }
        // (similarity, rank) of the best so far; visited in ascending
        // (size, rank), so a later sibling must overlap MORE to win.
        let mut best: Option<(usize, usize)> = None;
        loop {
            if best.is_some_and(|(similarity, _)| similarity >= bound) {
                break;
            }
            let Some(at) = walks.iter().filter_map(|w| w.2).min() else {
                break;
            };
            for (count, dir, next) in &mut walks {
                if *next != Some(at) {
                    continue;
                }
                *next = by_dir
                    .get_mut(&(group, *dir))
                    .and_then(|list| next_entry(list, Some(at), nodes, alive));
                if next.is_none() {
                    // No sibling further on is in this list.
                    bound -= *count;
                }
            }
            let rank = at.1;
            if rank == source {
                continue;
            }
            let similarity = dir_similarity(&dirs[source], &dirs[rank]);
            if best.is_none_or(|(top, _)| similarity > top) {
                best = Some((similarity, rank));
            }
        }
        best.map(|(_, rank)| rank)
    }

    /// The smallest sibling of `source`, the first on a tie.
    fn smallest_sibling(&self, source: usize) -> Option<usize> {
        self.siblings
            .get(&self.group[source])?
            .iter()
            .map(|&(_, rank)| rank)
            .find(|&rank| rank != source)
    }

    /// Merge `source` into `target` (same group).
    fn merge(&mut self, source: usize, target: usize) {
        let group = self.group[source];
        let (source_size, target_size) = (self.size(source), self.size(target));
        let size = source_size + target_size;
        if let Some(set) = self.siblings.get_mut(&group) {
            set.remove(&(source_size, source));
            set.remove(&(target_size, target));
            set.insert((size, target));
        }
        let moved = std::mem::take(&mut self.nodes[source]);
        for &n in &moved {
            self.owner[n] = Some(target);
        }
        self.nodes[target].extend(moved);
        let source_dirs = std::mem::take(&mut self.dirs[source]);
        for (dir, count) in source_dirs {
            let slot = self.dirs[target].entry(dir).or_insert(0);
            if *slot == 0 {
                // A directory new to the target: enter it in that list.
                self.by_dir
                    .entry((group, dir))
                    .or_default()
                    .insert((size, target));
            }
            *slot += count;
        }
        self.alive[source] = false;
    }
}

/// `a` beats `b`: more edges, then more overlap, then smaller, then first.
fn beats(a: (usize, usize, usize, usize), b: (usize, usize, usize, usize)) -> bool {
    (a.0, a.1, std::cmp::Reverse(a.2), std::cmp::Reverse(a.3))
        > (b.0, b.1, std::cmp::Reverse(b.2), std::cmp::Reverse(b.3))
}

/// The first current entry of `list` after `after`, dropping the ranks
/// merged away and moving the entries with an old size on the way.
fn next_entry(
    list: &mut BTreeSet<SizeRank>,
    after: Option<SizeRank>,
    nodes: &[Vec<usize>],
    alive: &[bool],
) -> Option<SizeRank> {
    loop {
        let entry = match after {
            None => list.first().copied(),
            Some(at) => list.range((Excluded(at), Unbounded)).next().copied(),
        }?;
        let (size, rank) = entry;
        if !alive[rank] {
            list.remove(&entry);
        } else if size != nodes[rank].len() {
            list.remove(&entry);
            list.insert((nodes[rank].len(), rank));
        } else {
            return Some(entry);
        }
    }
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
    let mut count = clustering.sections.len();
    if count <= target {
        return;
    }
    let ids: Vec<usize> = clustering.sections.keys().copied().collect();
    let mut last_page: Vec<Option<usize>> = clustering
        .sections
        .values()
        .map(|pages| pages.keys().max().copied())
        .collect();
    let members = clustering
        .sections
        .values()
        .map(|pages| (0, pages.values().flatten().copied().collect()))
        .collect();
    let mut pool = Pool::new(graph, members);
    let mut merged: HashSet<usize> = HashSet::new();

    while count > target {
        // `min(sec_sizes, key=sec_sizes.get)`: the first smallest.
        let Some(smallest) = pool
            .siblings
            .get(&0)
            .and_then(|set| set.first())
            .map(|&(_, rank)| rank)
        else {
            break;
        };
        let Some(best) = pool.best_sibling(graph, smallest) else {
            break;
        };
        let (smallest_id, best_id) = (ids[smallest], ids[best]);

        // Sections and micro maps leave their maps at the end, in one pass:
        // removing one at a time shifts the rest every time.
        let source_pages = clustering
            .sections
            .get_mut(&smallest_id)
            .map(std::mem::take)
            .unwrap_or_default();
        let target_micro = clustering.micro_assignments.entry(best_id).or_default();
        if let Some(target_pages) = clustering.sections.get_mut(&best_id) {
            let first_page = last_page[best].map_or(0, |m| m + 1);
            for (next_page, (_, nodes)) in (first_page..).zip(source_pages) {
                for &n in &nodes {
                    clustering.macro_assignments.insert(n, best_id);
                    target_micro.insert(n, next_page);
                }
                target_pages.insert(next_page, nodes);
                last_page[best] = Some(next_page);
            }
        }
        merged.insert(smallest_id);
        pool.merge(smallest, best);
        count -= 1;
    }
    clustering.sections.retain(|id, _| !merged.contains(id));
    clustering
        .micro_assignments
        .retain(|id, _| !merged.contains(id));
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
    // rank → (section, page), in Python's `pg_sizes` order.
    let mut by_rank: Vec<(usize, usize)> = Vec::new();
    let mut members: Vec<(usize, Vec<usize>)> = Vec::new();
    for (&section, pages) in &clustering.sections {
        for (&page, nodes) in pages {
            by_rank.push((section, page));
            members.push((section, nodes.clone()));
        }
    }
    let mut pool = Pool::new(graph, members);
    // (size, rank): Python's `sorted(pg_sizes.items(), key=size)`.
    let mut order: BTreeSet<SizeRank> = (0..by_rank.len()).map(|r| (pool.size(r), r)).collect();
    let mut merged: HashSet<(usize, usize)> = HashSet::new();

    while total > target {
        // The first candidate whose section has a sibling page. A page
        // without one never gets one (pages only leave), so it is dropped.
        let chosen = loop {
            let Some(&(size, rank)) = order.first() else {
                break None;
            };
            if pool.group_len(rank) > 1 {
                break Some(rank);
            }
            order.remove(&(size, rank));
        };
        let Some(source) = chosen else {
            break;
        };
        let Some(best) = pool.best_sibling(graph, source) else {
            break;
        };
        let ((section, page), (_, best_page)) = (by_rank[source], by_rank[best]);

        let pages = clustering.sections.entry(section).or_default();
        let moved = pages.get_mut(&page).map(std::mem::take).unwrap_or_default();
        let micro = clustering.micro_assignments.entry(section).or_default();
        for &n in &moved {
            micro.insert(n, best_page);
        }
        pages.entry(best_page).or_default().extend(moved);
        merged.insert((section, page));

        order.remove(&(pool.size(source), source));
        order.remove(&(pool.size(best), best));
        pool.merge(source, best);
        order.insert((pool.size(best), best));
        total -= 1;
    }
    for (&section, pages) in &mut clustering.sections {
        pages.retain(|page, _| !merged.contains(&(section, *page)));
    }
}

#[cfg(test)]
mod reference;

#[cfg(test)]
mod tests;
