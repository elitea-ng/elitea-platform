//! The indexed consolidation passes against [`super::reference`] (the
//! scan Python does), and their time on inputs that made the scan
//! quadratic.

use super::super::{
    ClusterGraph, ClusterNode, Clustering, LeidenPartitioner, Pages, Phase3Flags, run_phase3,
};
use super::{consolidate_pages, consolidate_sections, reference, target_section_count};
use indexmap::IndexMap;
use std::time::{Duration, Instant};

/// A small deterministic generator (an LCG; the high bits).
struct Lcg(u64);

impl Lcg {
    fn below(&mut self, bound: usize) -> usize {
        self.0 = self
            .0
            .wrapping_mul(6_364_136_223_846_793_005)
            .wrapping_add(1_442_695_040_888_963_407);
        usize::try_from(self.0 >> 33).unwrap() % bound.max(1)
    }

    fn shuffle<T>(&mut self, items: &mut [T]) {
        for i in (1..items.len()).rev() {
            items.swap(i, self.below(i + 1));
        }
    }
}

/// A graph of `n` nodes over `dirs` directories, `edges` random edges
/// (parallel ones and self-loops included); predecessors in edge order.
fn random_graph(rng: &mut Lcg, n: usize, dirs: usize, edges: usize) -> ClusterGraph {
    let nodes: Vec<ClusterNode> = (0..n)
        .map(|i| {
            let dir = rng.below(dirs);
            ClusterNode {
                id: format!("n{i:05}"),
                rel_path: if dir == 0 {
                    format!("f{i}.py")
                } else {
                    format!("d{dir}/f{}.py", rng.below(4))
                },
                file_name: String::new(),
                is_doc: false,
            }
        })
        .collect();
    let mut list = Vec::new();
    let mut preds: Vec<Vec<String>> = vec![Vec::new(); n];
    for _ in 0..edges {
        let (u, v) = (rng.below(n), rng.below(n));
        let (source, target) = (nodes[u].id.clone(), nodes[v].id.clone());
        if !preds[v].contains(&source) {
            preds[v].push(source.clone());
        }
        list.push((source, target, 1.0));
    }
    ClusterGraph::from_parts(nodes, &list, &preds).unwrap()
}

/// Sections in order, each with its pages in order.
type Sections = Vec<(usize, Vec<(usize, Vec<usize>)>)>;

/// Nodes (some left out, as hubs are) dealt into `sections` sections of
/// up to `max_pages` pages; ids and orders scrambled, pages may be empty.
fn random_clustering(rng: &mut Lcg, n: usize, sections: usize, max_pages: usize) -> Clustering {
    let mut section_ids: Vec<usize> = (0..sections).map(|s| s * 3 + rng.below(3)).collect();
    rng.shuffle(&mut section_ids);
    let mut layout: Sections = section_ids
        .iter()
        .map(|&id| {
            let mut page_ids: Vec<usize> = (0..=rng.below(max_pages))
                .map(|p| p * 2 + rng.below(2))
                .collect();
            rng.shuffle(&mut page_ids);
            (id, page_ids.into_iter().map(|p| (p, Vec::new())).collect())
        })
        .collect();
    let mut nodes: Vec<usize> = (0..n).collect();
    rng.shuffle(&mut nodes);
    for node in nodes {
        if rng.below(10) == 0 {
            continue;
        }
        let (_, pages) = &mut layout[rng.below(sections)];
        let at = rng.below(pages.len());
        pages[at].1.push(node);
    }
    let mut clustering = Clustering::default();
    for (section, pages) in layout {
        let mut map = Pages::new();
        let mut micro = IndexMap::new();
        for (page, nodes) in pages {
            for &n in &nodes {
                clustering.macro_assignments.insert(n, section);
                micro.insert(n, page);
            }
            map.insert(page, nodes);
        }
        clustering.sections.insert(section, map);
        clustering.micro_assignments.insert(section, micro);
    }
    clustering
}

/// Everything in a [`Clustering`] the passes change, IN ORDER (the
/// `IndexMap` equality ignores order, and the order is the contract).
type Ordered = (
    Sections,
    Vec<(usize, usize)>,
    Vec<(usize, Vec<(usize, usize)>)>,
);

fn ordered(clustering: &Clustering) -> Ordered {
    (
        clustering
            .sections
            .iter()
            .map(|(s, pages)| (*s, pages.iter().map(|(p, n)| (*p, n.clone())).collect()))
            .collect(),
        clustering
            .macro_assignments
            .iter()
            .map(|(n, s)| (*n, *s))
            .collect(),
        clustering
            .micro_assignments
            .iter()
            .map(|(s, m)| (*s, m.iter().map(|(n, p)| (*n, *p)).collect()))
            .collect(),
    )
}

#[test]
fn the_indexed_passes_merge_exactly_as_the_scan_on_random_clusterings() {
    const ROUNDS: usize = 300;
    let mut rng = Lcg(2026);
    for round in 0..ROUNDS {
        let n = 10 + rng.below(300);
        let dirs = 1 + rng.below(8);
        // A third of the rounds have no edges: the directory path decides.
        let edges = if round % 3 == 0 { 0 } else { rng.below(n * 2) };
        let graph = random_graph(&mut rng, n, dirs, edges);
        let sections = 1 + rng.below(60);
        let max_pages = 1 + rng.below(12);
        let input = random_clustering(&mut rng, n, sections, max_pages);
        let n_files = (rng.below(2) == 0).then(|| rng.below(400));

        let mut want = input.clone();
        reference::consolidate_sections(&mut want, &graph, n_files);
        let mut got = input.clone();
        consolidate_sections(&mut got, &graph, n_files);
        assert_eq!(ordered(&got), ordered(&want), "round {round}: sections");

        // Pages, on the consolidated sections and on the raw input.
        for (label, start) in [("after sections", want), ("raw", input)] {
            let mut want = start.clone();
            reference::consolidate_pages(&mut want, &graph);
            let mut got = start;
            consolidate_pages(&mut got, &graph);
            assert_eq!(
                ordered(&got),
                ordered(&want),
                "round {round}: pages {label}"
            );
        }
    }
}

/// `count` nodes, one file each, no edges, over `dirs` directories.
fn isolated_nodes(count: usize, dirs: usize) -> ClusterGraph {
    let nodes: Vec<ClusterNode> = (0..count)
        .map(|i| ClusterNode {
            id: format!("n{i:06}"),
            rel_path: format!("d{}/f{i}.py", i % dirs),
            file_name: String::new(),
            is_doc: false,
        })
        .collect();
    ClusterGraph::from_parts(nodes, &[], &vec![Vec::new(); count]).unwrap()
}

#[test]
fn many_single_node_pages_consolidate_in_near_linear_time() {
    // The scan took 5.9 s (release) for 20k single-node pages.
    const PAGES: usize = 20_000;
    let graph = isolated_nodes(PAGES, 40);
    let mut clustering = Clustering::default();
    let pages: Pages = (0..PAGES).map(|n| (n, vec![n])).collect();
    clustering.macro_assignments = (0..PAGES).map(|n| (n, 0)).collect();
    clustering
        .micro_assignments
        .insert(0, (0..PAGES).map(|n| (n, n)).collect());
    clustering.sections.insert(0, pages);
    let started = Instant::now();
    consolidate_pages(&mut clustering, &graph);
    let elapsed = started.elapsed();
    assert_eq!(clustering.sections[&0].len(), 54);
    assert!(
        elapsed < Duration::from_secs(2),
        "{elapsed:?} for {PAGES} pages"
    );
}

#[test]
fn every_file_isolated_consolidates_in_near_linear_time() {
    // Each isolated file is its own section: the section scan took 2.5 s
    // (release) for 20k files. The bound covers all of Phase 3.
    const FILES: usize = 20_000;
    let graph = isolated_nodes(FILES, 40);
    let started = Instant::now();
    let out = run_phase3(&graph, &[], Phase3Flags::default(), &mut LeidenPartitioner).unwrap();
    let elapsed = started.elapsed();
    assert_eq!(out.stats.macro_.cluster_count, target_section_count(FILES));
    assert_eq!(out.stats.algorithm_metadata.isolated_files, Some(FILES));
    assert!(
        elapsed < Duration::from_secs(5),
        "{elapsed:?} for {FILES} isolated files"
    );
}
