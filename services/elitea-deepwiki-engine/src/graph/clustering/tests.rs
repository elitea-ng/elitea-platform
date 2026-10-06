use super::hierarchical::{contract_to_file_graph, hierarchical_leiden_cluster};
use super::*;

/// A graph from `(id, rel_path, file_name)` nodes and weighted edges; the
/// predecessor order is the edge order.
fn graph(nodes: &[(&str, &str, &str)], edges: &[(&str, &str, f64)]) -> ClusterGraph {
    let list: Vec<ClusterNode> = nodes
        .iter()
        .map(|(id, rel_path, file_name)| ClusterNode {
            id: (*id).to_owned(),
            rel_path: (*rel_path).to_owned(),
            file_name: (*file_name).to_owned(),
            is_doc: false,
        })
        .collect();
    let mut preds: Vec<Vec<String>> = vec![Vec::new(); list.len()];
    for (u, v, _) in edges {
        let target = list.iter().position(|n| n.id == *v).unwrap();
        if !preds[target].iter().any(|p| p == u) {
            preds[target].push((*u).to_owned());
        }
    }
    let edges: Vec<(String, String, f64)> = edges
        .iter()
        .map(|(u, v, w)| ((*u).to_owned(), (*v).to_owned(), *w))
        .collect();
    ClusterGraph::from_parts(list, &edges, &preds).unwrap()
}

/// Answers with a function of the request and logs every call.
struct Scripted<F> {
    answer: F,
    log: Vec<(Level, Vec<String>, f64)>,
}

impl<F: FnMut(&PartitionRequest<'_>) -> Vec<usize>> Partitioner for Scripted<F> {
    fn partition(&mut self, request: &PartitionRequest<'_>) -> Result<Vec<usize>, PartitionError> {
        self.log.push((
            request.level,
            request.names.iter().map(|n| (*n).to_owned()).collect(),
            request.resolution,
        ));
        Ok((self.answer)(request))
    }
}

fn scripted<F: FnMut(&PartitionRequest<'_>) -> Vec<usize>>(answer: F) -> Scripted<F> {
    Scripted {
        answer,
        log: Vec::new(),
    }
}

/// Every vertex in community 0.
fn one_community(request: &PartitionRequest<'_>) -> Vec<usize> {
    vec![0; request.names.len()]
}

#[test]
fn contraction_sums_cross_file_weights_in_edge_order_and_drops_same_file_edges() {
    let g = graph(
        &[
            ("a1", "f/a.py", "a.py"),
            ("a2", "f/a.py", "a.py"),
            ("b1", "f/b.py", "b.py"),
            ("c1", "", "c.py"),
            ("d1", "", ""),
        ],
        &[
            ("a1", "b1", 0.1),
            ("b1", "a2", 0.2),
            ("a1", "a2", 5.0),
            ("c1", "d1", 1.0),
        ],
    );
    let (files, pairs) = contract_to_file_graph(&g, &[true; 5]);
    let names: Vec<&str> = files.keys().copied().collect();
    assert_eq!(names, ["f/a.py", "f/b.py", "c.py", "<unknown>"]);
    assert_eq!(files["f/a.py"], vec![0, 1]);
    // (lesser path, greater path): "<unknown>" sorts before "c.py".
    let pairs: Vec<((usize, usize), f64)> = pairs.into_iter().collect();
    assert_eq!(pairs.len(), 2);
    assert_eq!(pairs[0].0, (0, 1));
    assert_eq!(pairs[0].1.to_bits(), (0.1_f64 + 0.2).to_bits());
    assert_eq!(pairs[1], ((3, 2), 1.0));
}

#[test]
fn isolated_files_join_the_section_sharing_their_directory_else_the_first() {
    let g = graph(
        &[
            ("a", "x/a.py", ""),
            ("b", "x/b.py", ""),
            ("c", "y/c.py", ""),
            ("d", "y/d.py", ""),
            ("e", "y/e.py", ""),
            ("f", "z/f.py", ""),
        ],
        &[("a", "b", 1.0), ("c", "d", 1.0)],
    );
    let mut partitioner = scripted(|request: &PartitionRequest<'_>| match request.level {
        // Sorted files x/a, x/b, y/c, y/d: y first so section ids are not
        // in file order.
        Level::Section => vec![1, 1, 0, 0],
        Level::Page => vec![0; request.names.len()],
    });
    let clustering = hierarchical_leiden_cluster(
        &g,
        &[true; 6],
        &IndexSet::new(),
        0.5,
        1.0,
        SEED,
        &mut partitioner,
    )
    .unwrap();
    let section_of = |id: &str| clustering.macro_assignments[&g.index_of(id).unwrap()];
    assert_eq!(section_of("e"), 0, "y/e.py shares y/ with section 0");
    assert_eq!(
        section_of("f"),
        1,
        "no shared directory: the first section in file order"
    );
    assert_eq!(clustering.metadata.file_nodes, Some(6));
    assert_eq!(clustering.metadata.connected_files, Some(4));
    assert_eq!(clustering.metadata.isolated_files, Some(2));
    // Sections in first-appearance order over the sorted connected files.
    assert_eq!(
        clustering.sections.keys().copied().collect::<Vec<_>>(),
        vec![1, 0]
    );
    assert_eq!(partitioner.log[0].0, Level::Section);
    assert!((partitioner.log[0].2 - 0.5).abs() < f64::EPSILON);
}

#[test]
fn without_cross_file_edges_every_file_is_a_section_and_lone_pages_are_sorted() {
    let g = graph(
        &[
            ("p2", "m/one.py", ""),
            ("p1", "m/one.py", ""),
            ("q1", "m/two.py", ""),
            ("q2", "m/two.py", ""),
            ("r", "m/three.py", ""),
        ],
        &[("q1", "q2", 1.0)],
    );
    let mut partitioner = scripted(one_community);
    let clustering = hierarchical_leiden_cluster(
        &g,
        &[true; 5],
        &IndexSet::new(),
        1.0,
        1.0,
        SEED,
        &mut partitioner,
    )
    .unwrap();
    // No section call; one page call, for the only section with an edge.
    assert_eq!(partitioner.log.len(), 1);
    assert_eq!(partitioner.log[0].0, Level::Page);
    assert_eq!(clustering.sections.len(), 3);
    // m/one.py: two nodes, no edge: one page, sorted by id.
    assert_eq!(clustering.sections[&0][&0], vec![1, 0]);
    assert_eq!(clustering.sections[&2][&0], vec![4]);
}

#[test]
fn no_cluster_nodes_gives_an_empty_result_with_a_note() {
    let g = graph(&[("h", "a.py", "")], &[]);
    let hubs: IndexSet<usize> = IndexSet::from([0]);
    let clustering = hierarchical_leiden_cluster(
        &g,
        &[true],
        &hubs,
        1.0,
        1.0,
        SEED,
        &mut scripted(one_community),
    )
    .unwrap();
    assert!(clustering.sections.is_empty());
    assert_eq!(clustering.metadata.note, Some("no non-hub nodes"));
    assert_eq!(clustering.metadata.file_nodes, None);
}

/// One page per section: `sections[i]` lists node indices.
fn clustering_of(sections: &[(usize, Vec<Vec<usize>>)]) -> Clustering {
    let mut clustering = Clustering::default();
    for (section, pages) in sections {
        let mut map = Pages::new();
        let mut micro = IndexMap::new();
        for (page, nodes) in pages.iter().enumerate() {
            for &n in nodes {
                clustering.macro_assignments.insert(n, *section);
                micro.insert(n, page);
            }
            map.insert(page, nodes.clone());
        }
        clustering.sections.insert(*section, map);
        clustering.micro_assignments.insert(*section, micro);
    }
    clustering
}

#[test]
fn section_consolidation_merges_the_smallest_by_edges_then_directories() {
    // Sections 10..15; 15 (one node, `q/`) is the smallest. It has two
    // edges each to 12 and 14; 14 also shares the `q/` directory.
    let mut nodes: Vec<(String, String)> = Vec::new();
    let mut sections = Vec::new();
    for (section, size) in [(10, 5), (11, 3), (12, 3), (13, 3), (14, 3)] {
        let first = nodes.len();
        for i in 0..size {
            let dir = if section == 14 && i == 0 { "q" } else { "p" };
            nodes.push((format!("s{section}n{i}"), format!("{dir}/s{section}.py")));
        }
        sections.push((section, vec![(first..nodes.len()).collect::<Vec<_>>()]));
    }
    nodes.push(("lone".to_owned(), "q/lone.py".to_owned()));
    sections.push((15, vec![vec![nodes.len() - 1]]));
    let node_refs: Vec<(&str, &str, &str)> = nodes
        .iter()
        .map(|(id, p)| (id.as_str(), p.as_str(), ""))
        .collect();
    let g = graph(
        &node_refs,
        &[
            ("lone", "s12n0", 1.0),
            ("s12n1", "lone", 1.0),
            ("lone", "s14n1", 1.0),
            ("lone", "s14n1", 2.0),
        ],
    );
    let mut clustering = clustering_of(&sections);
    // Section 14 gets a second page first, so the appended page id is 2.
    clustering.sections[&14].insert(1, Vec::new());
    consolidate::consolidate_sections(&mut clustering, &g, Some(1));
    assert_eq!(
        clustering.sections.keys().copied().collect::<Vec<_>>(),
        vec![10, 11, 12, 13, 14]
    );
    let lone = g.index_of("lone").unwrap();
    assert_eq!(clustering.sections[&14][&2], vec![lone]);
    assert_eq!(clustering.macro_assignments[&lone], 14);
    assert_eq!(clustering.micro_assignments[&14][&lone], 2);
    assert!(!clustering.micro_assignments.contains_key(&15));
}

#[test]
fn section_consolidation_ties_go_to_the_smaller_then_the_first_sibling() {
    // No edges, no shared directories: the smaller sibling wins; among
    // equals the first in order.
    let ids: Vec<String> = (0..9).map(|i| format!("n{i}")).collect();
    let node_refs: Vec<(&str, &str, &str)> = ids
        .iter()
        .enumerate()
        .map(|(i, id)| {
            (
                id.as_str(),
                ["a/x.py", "b/x.py", "c/x.py", "d/x.py", "e/x.py", "f/x.py"][i.min(5)],
                "",
            )
        })
        .collect();
    let g = graph(&node_refs, &[]);
    let mut clustering = clustering_of(&[
        (0, vec![vec![0]]),
        (1, vec![vec![1]]),
        (2, vec![vec![2]]),
        (3, vec![vec![3]]),
        (4, vec![vec![4]]),
        (5, vec![vec![5, 6, 7, 8]]),
    ]);
    consolidate::consolidate_sections(&mut clustering, &g, Some(1));
    // Section 0 (first smallest) joins section 1 (first of the smallest).
    assert_eq!(clustering.sections.len(), 5);
    assert_eq!(clustering.macro_assignments[&0], 1);
    assert_eq!(clustering.sections[&1][&1], vec![0]);
}

#[test]
fn page_consolidation_merges_the_smallest_page_that_has_a_sibling() {
    // Section 0: pages p0..p8 (sizes 4, 2, 1, 3×6); section 1: one page of
    // one node, the smallest but without a sibling. 26 nodes: target 8.
    let sizes = [4, 2, 1, 3, 3, 3, 3, 3, 3];
    let mut nodes: Vec<(String, String)> = Vec::new();
    let mut pages = Vec::new();
    for (page, size) in sizes.iter().enumerate() {
        let first = nodes.len();
        for i in 0..*size {
            let dir = if page == 1 || page == 2 || (page == 5 && i == 0) {
                "y"
            } else {
                "x"
            };
            nodes.push((format!("p{page}n{i}"), format!("{dir}/f.py")));
        }
        pages.push((first..nodes.len()).collect::<Vec<_>>());
    }
    nodes.push(("other".to_owned(), "z/f.py".to_owned()));
    let node_refs: Vec<(&str, &str, &str)> = nodes
        .iter()
        .map(|(id, p)| (id.as_str(), p.as_str(), ""))
        .collect();
    let g = graph(&node_refs, &[("p2n0", "p1n1", 1.0)]);
    let mut clustering = clustering_of(&[(0, pages), (1, vec![vec![nodes.len() - 1]])]);
    consolidate::consolidate_pages(&mut clustering, &g);
    let index = |id: &str| g.index_of(id).unwrap();
    // p2 → p1 (the edge); then p1 (now 3, first of the 3s) → p5 (the
    // shared `y/` directory).
    assert_eq!(clustering.sections[&0].len(), 7);
    assert_eq!(clustering.sections[&1].len(), 1);
    assert_eq!(
        clustering.sections[&0][&5],
        vec![
            index("p5n0"),
            index("p5n1"),
            index("p5n2"),
            index("p1n0"),
            index("p1n1"),
            index("p2n0")
        ]
    );
    for id in ["p1n0", "p1n1", "p2n0"] {
        assert_eq!(clustering.micro_assignments[&0][&index(id)], 5);
    }
    assert_eq!(
        clustering.sections[&0].keys().copied().collect::<Vec<_>>(),
        vec![0, 3, 4, 5, 6, 7, 8]
    );
}

#[test]
fn run_phase3_leaves_tests_out_and_hands_hubs_back() {
    let g = graph(
        &[
            ("a", "src/a.py", ""),
            ("b", "src/b.py", ""),
            ("t", "tests/test_a.py", ""),
            ("h", "src/log.py", ""),
        ],
        &[
            ("a", "b", 1.0),
            ("t", "a", 1.0),
            ("a", "h", 1.0),
            ("b", "h", 1.0),
        ],
    );
    let flags = Phase3Flags {
        exclude_tests: true,
        calibrated_weights: true,
    };
    let mut partitioner = scripted(one_community);
    let out = run_phase3(
        &g,
        &["h".to_owned(), "missing".to_owned()],
        flags,
        &mut partitioner,
    )
    .unwrap();
    // γ_sec from the 3 non-test rel_paths, hubs included.
    assert_eq!(
        partitioner.log[0].2.to_bits(),
        sizing::auto_resolution(3).to_bits()
    );
    let rows = out.assignments(&g);
    assert_eq!(rows[2].macro_cluster, None, "test node excluded");
    assert_eq!(rows[3].macro_cluster, Some(0));
    assert_eq!(rows[3].micro_cluster, Some(0));
    assert!(rows[3].is_hub);
    assert_eq!(rows[3].hub_assignment.as_deref(), Some("0"));
    assert_eq!(out.stats.graph.test_nodes_excluded, 1);
    assert_eq!(out.stats.graph.hubs, 1);
    assert_eq!(out.stats.macro_.nodes_assigned, 2);
    assert_eq!(out.stats.persistence.nodes_clustered, 4);

    let legacy = Phase3Flags {
        exclude_tests: false,
        calibrated_weights: false,
    };
    let mut partitioner = scripted(one_community);
    let out = run_phase3(&g, &[], legacy, &mut partitioner).unwrap();
    assert!((partitioner.log[0].2 - 1.0).abs() < f64::EPSILON);
    assert!(
        out.assignments(&g)
            .iter()
            .all(|row| row.macro_cluster == Some(0) && !row.is_hub)
    );
}
