use std::cell::RefCell;
use std::collections::BTreeMap;

use leiden_rs::{
    GraphData, GraphDataBuilder, Leiden, LeidenConfig, Modularity, Partition, QualityFunction,
    QualityType, CPM,
};

fn run_leiden(data: &GraphData, seed: Option<u64>) -> Partition {
    let config = LeidenConfig {
        seed,
        ..Default::default()
    };
    Leiden::new(config).run(data).unwrap().partition
}

fn compute_modularity(data: &GraphData, partition: &Partition) -> f64 {
    let modularity = Modularity::with_resolution(1.0);
    modularity.total_quality(data, partition)
}

fn partition_to_membership(partition: &Partition, n: usize) -> Vec<usize> {
    (0..n).map(|i| partition.community_of(i)).collect()
}

fn normalize_membership(membership: &[usize]) -> Vec<usize> {
    let n = membership.len();
    if n == 0 {
        return Vec::new();
    }
    let mut communities: BTreeMap<usize, Vec<usize>> = BTreeMap::new();
    for (node, &comm) in membership.iter().enumerate() {
        communities.entry(comm).or_default().push(node);
    }
    let mut sorted: Vec<&Vec<usize>> = communities.values().collect();
    sorted.sort_by_key(|nodes| nodes[0]);
    let mut normalized = vec![0usize; n];
    for (label, nodes) in sorted.into_iter().enumerate() {
        for &node in nodes {
            normalized[node] = label;
        }
    }
    normalized
}

fn assert_community_structure(partition: &Partition, n: usize, expected: &[usize]) {
    let actual = partition_to_membership(partition, n);
    let actual_norm = normalize_membership(&actual);
    let expected_norm = normalize_membership(expected);
    assert_eq!(
        actual_norm, expected_norm,
        "Community structure mismatch.\n  actual:   {:?}\n  expected: {:?}",
        actual_norm, expected_norm
    );
}

fn assert_modularity_close(actual: f64, expected: f64, tolerance: f64) {
    let diff = (actual - expected).abs();
    assert!(
        diff < tolerance,
        "Modularity mismatch: actual={:.10}, expected={:.10}, diff={:.2e} (tol={:.2e})",
        actual,
        expected,
        diff,
        tolerance
    );
}

fn make_two_cliques() -> GraphData {
    let mut b = GraphDataBuilder::new(10);
    for i in 0..5 {
        for j in (i + 1)..5 {
            b.add_edge(i, j, 1.0).unwrap();
        }
    }
    for i in 5..10 {
        for j in (i + 1)..10 {
            b.add_edge(i, j, 1.0).unwrap();
        }
    }
    b.add_edge(0, 5, 1.0).unwrap();
    b.build().unwrap()
}

fn make_ring_of_cliques() -> GraphData {
    let mut b = GraphDataBuilder::new(12);
    for base in [0, 4, 8] {
        for i in base..base + 4 {
            for j in (i + 1)..base + 4 {
                b.add_edge(i, j, 1.0).unwrap();
            }
        }
    }
    b.add_edge(0, 4, 0.1).unwrap();
    b.add_edge(4, 8, 0.1).unwrap();
    b.add_edge(8, 0, 0.1).unwrap();
    b.build().unwrap()
}

fn make_weighted_graph() -> GraphData {
    let mut b = GraphDataBuilder::new(6);
    for i in 0..3 {
        for j in (i + 1)..3 {
            b.add_edge(i, j, 5.0).unwrap();
        }
    }
    for i in 3..6 {
        for j in (i + 1)..6 {
            b.add_edge(i, j, 5.0).unwrap();
        }
    }
    b.add_edge(0, 3, 0.1).unwrap();
    b.build().unwrap()
}

fn make_disconnected_graph() -> GraphData {
    let mut b = GraphDataBuilder::new(6);
    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(0, 2, 1.0).unwrap();
    b.add_edge(1, 2, 1.0).unwrap();
    b.add_edge(3, 4, 1.0).unwrap();
    b.add_edge(3, 5, 1.0).unwrap();
    b.add_edge(4, 5, 1.0).unwrap();
    b.build().unwrap()
}

const ZACHARY_EDGES: [(usize, usize); 78] = [
    (0, 1),
    (0, 2),
    (0, 3),
    (0, 4),
    (0, 5),
    (0, 6),
    (0, 7),
    (0, 8),
    (0, 10),
    (0, 11),
    (0, 12),
    (0, 13),
    (0, 17),
    (0, 19),
    (0, 21),
    (0, 31),
    (1, 2),
    (1, 3),
    (1, 7),
    (1, 13),
    (1, 17),
    (1, 19),
    (1, 21),
    (1, 30),
    (2, 3),
    (2, 7),
    (2, 27),
    (2, 28),
    (2, 32),
    (2, 9),
    (2, 8),
    (2, 13),
    (3, 7),
    (3, 12),
    (3, 13),
    (4, 6),
    (4, 10),
    (5, 6),
    (5, 10),
    (5, 16),
    (6, 16),
    (8, 30),
    (8, 32),
    (8, 33),
    (9, 33),
    (13, 33),
    (14, 32),
    (14, 33),
    (15, 32),
    (15, 33),
    (18, 32),
    (18, 33),
    (19, 33),
    (20, 32),
    (20, 33),
    (22, 32),
    (22, 33),
    (23, 25),
    (23, 27),
    (23, 32),
    (23, 33),
    (23, 29),
    (24, 25),
    (24, 27),
    (24, 31),
    (25, 31),
    (26, 29),
    (26, 33),
    (27, 33),
    (28, 31),
    (28, 33),
    (29, 32),
    (29, 33),
    (30, 32),
    (30, 33),
    (31, 32),
    (31, 33),
    (32, 33),
];

fn make_zachary() -> GraphData {
    let mut b = GraphDataBuilder::new(34);
    for &(u, v) in &ZACHARY_EDGES {
        b.add_edge(u, v, 1.0).unwrap();
    }
    b.build().unwrap()
}

const LARGE_CLUSTERED_EDGES: [(usize, usize, f64); 379] = [
    (0, 2, 1.0),
    (0, 3, 1.0),
    (0, 4, 1.0),
    (0, 8, 1.0),
    (0, 10, 1.0),
    (0, 11, 1.0),
    (0, 13, 1.0),
    (0, 14, 1.0),
    (0, 17, 1.0),
    (0, 20, 1.0),
    (0, 24, 1.0),
    (1, 4, 1.0),
    (1, 5, 1.0),
    (1, 19, 1.0),
    (1, 20, 1.0),
    (1, 21, 1.0),
    (1, 22, 1.0),
    (1, 23, 1.0),
    (1, 24, 1.0),
    (2, 3, 1.0),
    (2, 7, 1.0),
    (2, 8, 1.0),
    (2, 12, 1.0),
    (2, 14, 1.0),
    (2, 22, 1.0),
    (2, 23, 1.0),
    (3, 4, 1.0),
    (3, 5, 1.0),
    (3, 13, 1.0),
    (3, 14, 1.0),
    (3, 16, 1.0),
    (3, 20, 1.0),
    (3, 23, 1.0),
    (3, 24, 1.0),
    (4, 5, 1.0),
    (4, 9, 1.0),
    (4, 15, 1.0),
    (4, 19, 1.0),
    (4, 21, 1.0),
    (5, 7, 1.0),
    (5, 9, 1.0),
    (5, 12, 1.0),
    (5, 15, 1.0),
    (5, 20, 1.0),
    (5, 22, 1.0),
    (6, 9, 1.0),
    (6, 12, 1.0),
    (6, 14, 1.0),
    (6, 17, 1.0),
    (6, 20, 1.0),
    (6, 23, 1.0),
    (7, 9, 1.0),
    (7, 15, 1.0),
    (7, 16, 1.0),
    (7, 19, 1.0),
    (7, 20, 1.0),
    (7, 21, 1.0),
    (7, 23, 1.0),
    (8, 10, 1.0),
    (8, 11, 1.0),
    (8, 13, 1.0),
    (8, 14, 1.0),
    (8, 20, 1.0),
    (8, 21, 1.0),
    (9, 13, 1.0),
    (9, 17, 1.0),
    (9, 18, 1.0),
    (9, 21, 1.0),
    (9, 22, 1.0),
    (10, 13, 1.0),
    (10, 19, 1.0),
    (10, 21, 1.0),
    (10, 23, 1.0),
    (11, 13, 1.0),
    (11, 21, 1.0),
    (11, 22, 1.0),
    (12, 15, 1.0),
    (12, 19, 1.0),
    (12, 21, 1.0),
    (12, 22, 1.0),
    (13, 14, 1.0),
    (13, 15, 1.0),
    (13, 17, 1.0),
    (13, 24, 1.0),
    (14, 16, 1.0),
    (14, 19, 1.0),
    (14, 22, 1.0),
    (15, 17, 1.0),
    (15, 18, 1.0),
    (15, 19, 1.0),
    (15, 24, 1.0),
    (16, 22, 1.0),
    (16, 24, 1.0),
    (17, 20, 1.0),
    (17, 21, 1.0),
    (17, 22, 1.0),
    (17, 24, 1.0),
    (18, 21, 1.0),
    (18, 23, 1.0),
    (19, 22, 1.0),
    (19, 24, 1.0),
    (20, 21, 1.0),
    (20, 24, 1.0),
    (22, 23, 1.0),
    (22, 24, 1.0),
    (25, 30, 1.0),
    (25, 31, 1.0),
    (25, 37, 1.0),
    (25, 38, 1.0),
    (25, 39, 1.0),
    (25, 44, 1.0),
    (26, 27, 1.0),
    (26, 34, 1.0),
    (26, 36, 1.0),
    (26, 39, 1.0),
    (26, 44, 1.0),
    (26, 45, 1.0),
    (26, 46, 1.0),
    (27, 28, 1.0),
    (27, 34, 1.0),
    (27, 36, 1.0),
    (27, 38, 1.0),
    (27, 39, 1.0),
    (27, 47, 1.0),
    (27, 49, 1.0),
    (28, 35, 1.0),
    (28, 45, 1.0),
    (28, 46, 1.0),
    (29, 34, 1.0),
    (29, 35, 1.0),
    (29, 41, 1.0),
    (29, 45, 1.0),
    (29, 46, 1.0),
    (30, 34, 1.0),
    (30, 35, 1.0),
    (30, 36, 1.0),
    (30, 37, 1.0),
    (30, 40, 1.0),
    (30, 43, 1.0),
    (30, 46, 1.0),
    (30, 48, 1.0),
    (31, 33, 1.0),
    (31, 35, 1.0),
    (31, 38, 1.0),
    (31, 39, 1.0),
    (32, 39, 1.0),
    (32, 40, 1.0),
    (32, 45, 1.0),
    (32, 46, 1.0),
    (32, 47, 1.0),
    (32, 48, 1.0),
    (33, 35, 1.0),
    (33, 36, 1.0),
    (33, 39, 1.0),
    (33, 43, 1.0),
    (34, 36, 1.0),
    (34, 45, 1.0),
    (34, 46, 1.0),
    (34, 48, 1.0),
    (35, 38, 1.0),
    (35, 40, 1.0),
    (35, 42, 1.0),
    (35, 45, 1.0),
    (35, 47, 1.0),
    (35, 49, 1.0),
    (36, 38, 1.0),
    (36, 44, 1.0),
    (36, 45, 1.0),
    (36, 46, 1.0),
    (37, 43, 1.0),
    (37, 44, 1.0),
    (37, 45, 1.0),
    (37, 46, 1.0),
    (37, 49, 1.0),
    (38, 41, 1.0),
    (38, 44, 1.0),
    (38, 47, 1.0),
    (38, 48, 1.0),
    (39, 44, 1.0),
    (39, 47, 1.0),
    (40, 42, 1.0),
    (41, 44, 1.0),
    (42, 45, 1.0),
    (43, 45, 1.0),
    (43, 48, 1.0),
    (44, 45, 1.0),
    (44, 49, 1.0),
    (45, 47, 1.0),
    (45, 48, 1.0),
    (47, 48, 1.0),
    (47, 49, 1.0),
    (50, 51, 1.0),
    (50, 59, 1.0),
    (50, 60, 1.0),
    (50, 70, 1.0),
    (50, 71, 1.0),
    (50, 72, 1.0),
    (50, 73, 1.0),
    (50, 74, 1.0),
    (51, 54, 1.0),
    (51, 55, 1.0),
    (51, 60, 1.0),
    (51, 61, 1.0),
    (51, 62, 1.0),
    (51, 63, 1.0),
    (51, 64, 1.0),
    (51, 65, 1.0),
    (51, 67, 1.0),
    (51, 71, 1.0),
    (52, 56, 1.0),
    (52, 57, 1.0),
    (52, 62, 1.0),
    (52, 64, 1.0),
    (52, 65, 1.0),
    (52, 69, 1.0),
    (52, 73, 1.0),
    (53, 63, 1.0),
    (53, 64, 1.0),
    (53, 66, 1.0),
    (53, 67, 1.0),
    (53, 68, 1.0),
    (53, 69, 1.0),
    (53, 72, 1.0),
    (53, 73, 1.0),
    (54, 68, 1.0),
    (54, 69, 1.0),
    (54, 73, 1.0),
    (54, 74, 1.0),
    (55, 61, 1.0),
    (55, 62, 1.0),
    (55, 65, 1.0),
    (55, 66, 1.0),
    (55, 73, 1.0),
    (55, 74, 1.0),
    (56, 61, 1.0),
    (56, 65, 1.0),
    (56, 72, 1.0),
    (57, 69, 1.0),
    (57, 74, 1.0),
    (58, 63, 1.0),
    (58, 66, 1.0),
    (58, 67, 1.0),
    (58, 70, 1.0),
    (58, 74, 1.0),
    (59, 64, 1.0),
    (59, 67, 1.0),
    (59, 74, 1.0),
    (60, 66, 1.0),
    (60, 67, 1.0),
    (60, 71, 1.0),
    (61, 65, 1.0),
    (61, 68, 1.0),
    (61, 70, 1.0),
    (62, 67, 1.0),
    (63, 67, 1.0),
    (64, 71, 1.0),
    (64, 72, 1.0),
    (65, 69, 1.0),
    (66, 67, 1.0),
    (66, 69, 1.0),
    (66, 71, 1.0),
    (67, 68, 1.0),
    (67, 69, 1.0),
    (67, 72, 1.0),
    (67, 74, 1.0),
    (68, 70, 1.0),
    (68, 71, 1.0),
    (68, 72, 1.0),
    (69, 71, 1.0),
    (70, 73, 1.0),
    (71, 72, 1.0),
    (72, 73, 1.0),
    (75, 77, 1.0),
    (75, 80, 1.0),
    (75, 84, 1.0),
    (75, 88, 1.0),
    (76, 82, 1.0),
    (76, 83, 1.0),
    (76, 85, 1.0),
    (76, 86, 1.0),
    (76, 92, 1.0),
    (76, 98, 1.0),
    (77, 78, 1.0),
    (77, 87, 1.0),
    (77, 95, 1.0),
    (77, 96, 1.0),
    (77, 97, 1.0),
    (78, 80, 1.0),
    (78, 81, 1.0),
    (78, 86, 1.0),
    (78, 90, 1.0),
    (78, 92, 1.0),
    (78, 99, 1.0),
    (79, 81, 1.0),
    (79, 85, 1.0),
    (79, 90, 1.0),
    (79, 96, 1.0),
    (79, 97, 1.0),
    (80, 84, 1.0),
    (80, 86, 1.0),
    (80, 87, 1.0),
    (80, 89, 1.0),
    (80, 91, 1.0),
    (80, 92, 1.0),
    (80, 95, 1.0),
    (80, 97, 1.0),
    (80, 99, 1.0),
    (81, 82, 1.0),
    (81, 83, 1.0),
    (81, 84, 1.0),
    (81, 86, 1.0),
    (81, 87, 1.0),
    (81, 89, 1.0),
    (81, 91, 1.0),
    (81, 92, 1.0),
    (81, 99, 1.0),
    (82, 85, 1.0),
    (82, 91, 1.0),
    (82, 97, 1.0),
    (82, 98, 1.0),
    (83, 84, 1.0),
    (83, 85, 1.0),
    (83, 93, 1.0),
    (83, 99, 1.0),
    (84, 86, 1.0),
    (84, 90, 1.0),
    (84, 94, 1.0),
    (84, 96, 1.0),
    (84, 98, 1.0),
    (84, 99, 1.0),
    (85, 92, 1.0),
    (86, 89, 1.0),
    (86, 91, 1.0),
    (86, 92, 1.0),
    (87, 91, 1.0),
    (87, 94, 1.0),
    (87, 95, 1.0),
    (87, 99, 1.0),
    (88, 92, 1.0),
    (88, 94, 1.0),
    (88, 96, 1.0),
    (89, 93, 1.0),
    (89, 94, 1.0),
    (89, 95, 1.0),
    (89, 98, 1.0),
    (90, 93, 1.0),
    (90, 95, 1.0),
    (90, 97, 1.0),
    (91, 93, 1.0),
    (91, 96, 1.0),
    (91, 98, 1.0),
    (91, 99, 1.0),
    (92, 93, 1.0),
    (92, 95, 1.0),
    (92, 97, 1.0),
    (93, 97, 1.0),
    (93, 99, 1.0),
    (94, 95, 1.0),
    (95, 98, 1.0),
    (95, 99, 1.0),
    (98, 99, 1.0),
    (4, 33, 0.1),
    (9, 49, 0.1),
    (17, 46, 0.1),
    (24, 53, 0.1),
    (16, 60, 0.1),
    (21, 61, 0.1),
    (3, 80, 0.1),
    (16, 80, 0.1),
    (12, 92, 0.1),
    (38, 72, 0.1),
    (34, 51, 0.1),
    (38, 66, 0.1),
    (31, 97, 0.1),
    (28, 78, 0.1),
    (45, 97, 0.1),
    (54, 91, 0.1),
    (71, 93, 0.1),
    (70, 97, 0.1),
];

fn make_large_clustered() -> GraphData {
    let mut b = GraphDataBuilder::new(100);
    for &(u, v, w) in &LARGE_CLUSTERED_EDGES {
        b.add_edge(u, v, w).unwrap();
    }
    b.build().unwrap()
}

#[test]
fn leidenalg_two_cliques() {
    let graph = make_two_cliques();
    let partition = run_leiden(&graph, Some(42));
    assert_eq!(partition.num_communities(), 2);
    assert_community_structure(&partition, 10, &[0, 0, 0, 0, 0, 1, 1, 1, 1, 1]);
    assert_modularity_close(compute_modularity(&graph, &partition), 0.4523809524, 1e-4);
}

#[test]
fn leidenalg_ring_of_cliques() {
    let graph = make_ring_of_cliques();
    let partition = run_leiden(&graph, Some(42));
    assert_eq!(partition.num_communities(), 3);
    assert_community_structure(&partition, 12, &[0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2]);
    let q = compute_modularity(&graph, &partition);
    assert!(
        q >= 0.5238095238 - 0.01,
        "Modularity should be at least as good as leidenalg: got {:.10}, expected >= {:.10}",
        q,
        0.5238095238 - 0.01
    );
}

#[test]
fn leidenalg_weighted_graph() {
    let graph = make_weighted_graph();
    let partition = run_leiden(&graph, Some(42));
    assert_eq!(partition.num_communities(), 2);
    assert_community_structure(&partition, 6, &[0, 0, 0, 1, 1, 1]);
    let q = compute_modularity(&graph, &partition);
    assert!(
        q >= 0.3571428571 - 0.01,
        "Modularity should be at least as good as leidenalg: got {:.10}, expected >= {:.10}",
        q,
        0.3571428571 - 0.01
    );
}

#[test]
fn leidenalg_disconnected() {
    let graph = make_disconnected_graph();
    let partition = run_leiden(&graph, Some(42));
    assert_eq!(partition.num_communities(), 2);
    assert_community_structure(&partition, 6, &[0, 0, 0, 1, 1, 1]);
    assert_modularity_close(compute_modularity(&graph, &partition), 0.5, 1e-4);
}

fn run_leiden_cpm(data: &GraphData, resolution: f64, seed: Option<u64>) -> Partition {
    let config = LeidenConfig {
        seed,
        resolution,
        quality: QualityType::CPM,
        ..Default::default()
    };
    Leiden::new(config).run(data).unwrap().partition
}

fn compute_cpm_quality(data: &GraphData, partition: &Partition, resolution: f64) -> f64 {
    let cpm = CPM::new(resolution);
    cpm.total_quality(data, partition)
}

#[test]
fn leidenalg_cpm_two_cliques_low_resolution() {
    let graph = make_two_cliques();
    let partition = run_leiden_cpm(&graph, 0.1, Some(42));
    assert_eq!(partition.num_communities(), 2);
    assert_community_structure(&partition, 10, &[0, 0, 0, 0, 0, 1, 1, 1, 1, 1]);
    let q = compute_cpm_quality(&graph, &partition, 0.1);
    assert_modularity_close(q, 18.0, 0.1);
}

#[test]
fn leidenalg_cpm_two_cliques_high_resolution() {
    let graph = make_two_cliques();
    let partition = run_leiden_cpm(&graph, 1.0, Some(42));
    assert!(
        partition.num_communities() >= 8,
        "High CPM resolution should fragment into many communities, got {}",
        partition.num_communities()
    );
}

#[test]
fn leidenalg_cpm_large_clustered() {
    let graph = make_large_clustered();
    let partition = run_leiden_cpm(&graph, 0.05, Some(42));
    let nc = partition.num_communities();
    assert!(
        (3..=5).contains(&nc),
        "Expected 3-5 communities, got {}",
        nc
    );

    let membership = partition_to_membership(&partition, 100);
    let normalized = normalize_membership(&membership);
    assert_eq!(normalized[0], normalized[24], "cluster 0 intact");
    assert_eq!(normalized[25], normalized[49], "cluster 1 intact");
    assert_eq!(normalized[50], normalized[74], "cluster 2 intact");
    assert_eq!(normalized[75], normalized[99], "cluster 3 intact");
    assert_ne!(normalized[0], normalized[25], "clusters 0 vs 1");
    assert_ne!(normalized[25], normalized[50], "clusters 1 vs 2");
    assert_ne!(normalized[50], normalized[75], "clusters 2 vs 3");

    let q = compute_cpm_quality(&graph, &partition, 0.05);
    assert_modularity_close(q, 301.0, 1.0);
}

#[test]
fn leidenalg_self_loop() {
    let mut b = GraphDataBuilder::new(4);
    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(2, 3, 1.0).unwrap();
    b.add_edge(0, 0, 2.0).unwrap();
    let graph = b.build().unwrap();

    let partition = run_leiden(&graph, Some(42));
    assert_eq!(partition.num_communities(), 2);
    assert_community_structure(&partition, 4, &[0, 0, 1, 1]);
    assert_modularity_close(compute_modularity(&graph, &partition), 0.375, 1e-4);
}

#[test]
fn leidenalg_zachary_modularity() {
    let graph = make_zachary();
    let partition = run_leiden(&graph, Some(42));
    let q = compute_modularity(&graph, &partition);
    let nc = partition.num_communities();
    assert!(
        (2..=5).contains(&nc),
        "Expected 2-5 communities, got {}",
        nc
    );
    assert_modularity_close(q, 0.4197896121, 0.05);
}

#[test]
fn leidenalg_large_clustered_modularity() {
    let graph = make_large_clustered();
    let partition = run_leiden(&graph, Some(42));
    let q = compute_modularity(&graph, &partition);
    let nc = partition.num_communities();
    assert!(
        (3..=5).contains(&nc),
        "Expected 3-5 communities, got {}",
        nc
    );
    assert_modularity_close(q, 0.7002596752, 0.05);

    let membership = partition_to_membership(&partition, 100);
    let normalized = normalize_membership(&membership);
    assert_eq!(
        normalized[0], normalized[24],
        "cluster 0: nodes 0-24 together"
    );
    assert_eq!(
        normalized[25], normalized[49],
        "cluster 1: nodes 25-49 together"
    );
    assert_eq!(
        normalized[50], normalized[74],
        "cluster 2: nodes 50-74 together"
    );
    assert_eq!(
        normalized[75], normalized[99],
        "cluster 3: nodes 75-99 together"
    );
    assert_ne!(normalized[0], normalized[25], "clusters 0 vs 1");
    assert_ne!(normalized[25], normalized[50], "clusters 1 vs 2");
    assert_ne!(normalized[50], normalized[75], "clusters 2 vs 3");
}

fn fa_extract_membership(
    hierarchy: &fa_leiden_cd::Graph<fa_leiden_cd::Community, ()>,
    n: usize,
) -> Vec<usize> {
    let membership = RefCell::new(vec![0usize; n]);
    for (comm_idx, community) in hierarchy.node_data_slice().iter().enumerate() {
        community.collect_nodes(&|orig_node| {
            membership.borrow_mut()[orig_node] = comm_idx;
        });
    }
    membership.into_inner()
}

fn make_fa_graph_from_edges(edges: &[(usize, usize)]) -> fa_leiden_cd::Graph<String, ()> {
    let mut graph = fa_leiden_cd::Graph::new();
    let max_node = edges.iter().flat_map(|&(u, v)| [u, v]).max().unwrap_or(0);
    let node_ids: Vec<usize> = (0..=max_node)
        .map(|i| graph.add_node(format!("n{}", i)))
        .collect();
    for &(u, v) in edges {
        graph.add_edge(node_ids[u], node_ids[v], (), 1.0);
    }
    graph
}

fn make_three_triangles() -> GraphData {
    let mut b = GraphDataBuilder::new(9);
    for &(u, v) in &[
        (0, 1),
        (0, 2),
        (1, 2),
        (3, 4),
        (3, 5),
        (4, 5),
        (6, 7),
        (6, 8),
        (7, 8),
    ] {
        b.add_edge(u, v, 1.0).unwrap();
    }
    b.build().unwrap()
}

#[test]
fn fa_compare_disconnected() {
    let graph = make_disconnected_graph();
    let our_partition = run_leiden(&graph, None);
    assert_eq!(our_partition.num_communities(), 2);

    let fa_edges = vec![(0, 1), (0, 2), (1, 2), (3, 4), (3, 5), (4, 5)];
    let fa_graph = make_fa_graph_from_edges(&fa_edges);
    let mut optimizer = fa_leiden_cd::TrivialModularityOptimizer {
        parallel_scale: 128,
        tol: 1e-11,
    };
    let hierarchy = fa_graph.leiden(Some(100), &mut optimizer);
    let fa_membership = fa_extract_membership(&hierarchy, 6);

    let our_membership = partition_to_membership(&our_partition, 6);
    let our_norm = normalize_membership(&our_membership);
    let fa_norm = normalize_membership(&fa_membership);

    assert_eq!(
        our_norm, fa_norm,
        "Structure mismatch.\n  ours: {:?}\n  fa:   {:?}",
        our_norm, fa_norm
    );
}

#[test]
fn fa_compare_three_triangles() {
    let graph = make_three_triangles();
    let our_partition = run_leiden(&graph, None);
    assert!(our_partition.num_communities() >= 2);

    let fa_edges = vec![
        (0, 1),
        (0, 2),
        (1, 2),
        (3, 4),
        (3, 5),
        (4, 5),
        (6, 7),
        (6, 8),
        (7, 8),
    ];
    let fa_graph = make_fa_graph_from_edges(&fa_edges);
    let mut optimizer = fa_leiden_cd::TrivialModularityOptimizer {
        parallel_scale: 128,
        tol: 1e-11,
    };
    let hierarchy = fa_graph.leiden(Some(100), &mut optimizer);
    let fa_membership = fa_extract_membership(&hierarchy, 9);

    let our_membership = partition_to_membership(&our_partition, 9);
    let our_norm = normalize_membership(&our_membership);
    let fa_norm = normalize_membership(&fa_membership);

    assert_eq!(
        our_norm, fa_norm,
        "Structure mismatch.\n  ours: {:?}\n  fa:   {:?}",
        our_norm, fa_norm
    );
}

// ---------------------------------------------------------------------------
// Multi-seed invariants: run with multiple seeds and check that results are
// reasonable.  Different seeds may yield different community counts or
// modularity values, but the structure must remain sensible.
// ---------------------------------------------------------------------------

#[test]
fn multiseed_two_cliques() {
    let graph = make_two_cliques();
    for seed in [0u64, 1, 42, 123, 999] {
        let partition = run_leiden(&graph, Some(seed));
        assert_eq!(
            partition.num_communities(),
            2,
            "seed={seed}: two_cliques should always split into 2 communities"
        );
        let q = compute_modularity(&graph, &partition);
        assert!(
            q > 0.4,
            "seed={seed}: modularity should be well above zero, got {q:.10}"
        );
    }
}

#[test]
fn multiseed_zachary() {
    let graph = make_zachary();
    for seed in [0u64, 1, 42, 123, 999] {
        let partition = run_leiden(&graph, Some(seed));
        let nc = partition.num_communities();
        assert!(
            (2..=6).contains(&nc),
            "seed={seed}: zachary should have 2-6 communities, got {nc}"
        );
        let q = compute_modularity(&graph, &partition);
        assert!(
            q > 0.35,
            "seed={seed}: modularity should be > 0.35, got {q:.10}"
        );
    }
}

#[test]
fn multiseed_large_clustered() {
    let graph = make_large_clustered();
    for seed in [0u64, 1, 42, 123, 999] {
        let partition = run_leiden(&graph, Some(seed));
        let nc = partition.num_communities();
        assert!(
            nc >= 3,
            "seed={seed}: large_clustered should have >= 3 communities, got {nc}"
        );
        let q = compute_modularity(&graph, &partition);
        assert!(
            q > 0.5,
            "seed={seed}: modularity should be > 0.5, got {q:.10}"
        );
    }
}

// ---------------------------------------------------------------------------
// CPM edge-case tests: self-loops and disconnected graphs with CPM
// ---------------------------------------------------------------------------

#[test]
fn leidenalg_cpm_self_loop() {
    let mut b = GraphDataBuilder::new(4);
    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(2, 3, 1.0).unwrap();
    b.add_edge(0, 0, 2.0).unwrap();
    let graph = b.build().unwrap();

    let partition = run_leiden_cpm(&graph, 0.1, Some(42));
    assert_eq!(
        partition.num_communities(),
        2,
        "CPM res=0.1 should split self-loop graph into 2 communities"
    );
    assert_community_structure(&partition, 4, &[0, 0, 1, 1]);
}

#[test]
fn leidenalg_cpm_self_loop_high_resolution() {
    let mut b = GraphDataBuilder::new(4);
    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(2, 3, 1.0).unwrap();
    b.add_edge(0, 0, 2.0).unwrap();
    let graph = b.build().unwrap();

    let partition = run_leiden_cpm(&graph, 1.0, Some(42));
    assert!(
        partition.num_communities() >= 3,
        "CPM res=1.0 should fragment self-loop graph, got {}",
        partition.num_communities()
    );
}

#[test]
fn leidenalg_cpm_disconnected_low_resolution() {
    let graph = make_disconnected_graph();
    let partition = run_leiden_cpm(&graph, 0.1, Some(42));
    assert_eq!(partition.num_communities(), 2);
    assert_community_structure(&partition, 6, &[0, 0, 0, 1, 1, 1]);
    let q = compute_cpm_quality(&graph, &partition, 0.1);
    assert_modularity_close(q, 5.4, 0.1);
}

#[test]
fn leidenalg_cpm_disconnected_high_resolution() {
    let graph = make_disconnected_graph();
    let partition = run_leiden_cpm(&graph, 1.0, Some(42));
    assert!(
        partition.num_communities() >= 5,
        "CPM res=1.0 on disconnected should fragment heavily, got {}",
        partition.num_communities()
    );
}

#[test]
fn config_max_comm_size_limits_communities() {
    let graph = make_two_cliques();
    let config = LeidenConfig {
        max_comm_size: 3,
        seed: Some(42),
        ..Default::default()
    };
    let partition = Leiden::new(config).run(&graph).unwrap().partition;
    for (node, comm) in partition.iter() {
        let size = partition.nodes_in_community(comm).len();
        assert!(
            size <= 3,
            "node {node}: community size {size} exceeds max_comm_size=3"
        );
    }
}

#[test]
fn config_large_epsilon_stops_early() {
    let graph = make_large_clustered();
    let strict = LeidenConfig {
        seed: Some(42),
        epsilon: 1e-10,
        ..Default::default()
    };
    let loose = LeidenConfig {
        seed: Some(42),
        epsilon: 0.1,
        ..Default::default()
    };
    let p_strict = Leiden::new(strict).run(&graph).unwrap().partition;
    let p_loose = Leiden::new(loose).run(&graph).unwrap().partition;
    let q_strict = compute_modularity(&graph, &p_strict);
    let q_loose = compute_modularity(&graph, &p_loose);
    assert!(
        q_strict >= q_loose - 0.01,
        "strict epsilon ({q_strict:.6}) should be >= loose ({q_loose:.6})"
    );
}

#[test]
fn partition_communities_method() {
    let graph = make_two_cliques();
    let partition = run_leiden(&graph, Some(42));
    let comms = partition.communities();
    assert_eq!(comms.len(), 2, "two_cliques should have 2 communities");
    for (_id, nodes) in &comms {
        assert!(!nodes.is_empty());
    }
    let total_nodes: usize = comms.iter().map(|(_, n)| n.len()).sum();
    assert_eq!(total_nodes, 10);
}

#[test]
fn partition_iter_method() {
    let graph = make_two_cliques();
    let partition = run_leiden(&graph, Some(42));
    let pairs: Vec<_> = partition.iter().collect();
    assert_eq!(pairs.len(), 10);
    for (node, comm) in &pairs {
        assert_eq!(partition.community_of(*node), *comm);
    }
}

#[test]
#[should_panic(expected = "InvalidEdgeWeight")]
fn input_validation_rejects_negative_weight() {
    let mut b = GraphDataBuilder::new(2);
    b.add_edge(0, 1, -1.0).unwrap();
}

#[test]
#[should_panic(expected = "InvalidEdgeWeight")]
fn input_validation_rejects_nan_weight() {
    let mut b = GraphDataBuilder::new(2);
    b.add_edge(0, 1, f64::NAN).unwrap();
}

// ---------------------------------------------------------------------------
// Directed graph integration tests
// ---------------------------------------------------------------------------

fn make_directed_two_cliques() -> GraphData {
    let mut b = GraphDataBuilder::new(10).directed();
    for i in 0..5 {
        for j in 0..5 {
            if i != j {
                b.add_edge(i, j, 1.0).unwrap();
            }
        }
    }
    for i in 5..10 {
        for j in 5..10 {
            if i != j {
                b.add_edge(i, j, 1.0).unwrap();
            }
        }
    }
    b.add_edge(0, 5, 0.1).unwrap();
    b.build().unwrap()
}

#[test]
fn directed_two_cliques() {
    let graph = make_directed_two_cliques();
    assert!(graph.is_directed());

    let partition = run_leiden(&graph, Some(42));
    assert_eq!(
        partition.num_communities(),
        2,
        "directed two cliques should split into 2 communities"
    );
    assert_community_structure(&partition, 10, &[0, 0, 0, 0, 0, 1, 1, 1, 1, 1]);

    let q = compute_modularity(&graph, &partition);
    assert!(q > 0.0, "modularity should be positive, got {:.10}", q);
}

fn make_directed_triangles() -> GraphData {
    let mut b = GraphDataBuilder::new(6).directed();
    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(1, 0, 1.0).unwrap();
    b.add_edge(1, 2, 1.0).unwrap();
    b.add_edge(2, 1, 1.0).unwrap();
    b.add_edge(0, 2, 1.0).unwrap();
    b.add_edge(2, 0, 1.0).unwrap();
    b.add_edge(3, 4, 1.0).unwrap();
    b.add_edge(4, 3, 1.0).unwrap();
    b.add_edge(4, 5, 1.0).unwrap();
    b.add_edge(5, 4, 1.0).unwrap();
    b.add_edge(3, 5, 1.0).unwrap();
    b.add_edge(5, 3, 1.0).unwrap();
    b.add_edge(2, 3, 0.1).unwrap();
    b.add_edge(3, 2, 0.1).unwrap();
    b.build().unwrap()
}

#[test]
fn directed_cpm_same_as_undirected() {
    let directed = make_directed_triangles();

    let mut b = GraphDataBuilder::new(6);
    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(1, 2, 1.0).unwrap();
    b.add_edge(0, 2, 1.0).unwrap();
    b.add_edge(3, 4, 1.0).unwrap();
    b.add_edge(4, 5, 1.0).unwrap();
    b.add_edge(3, 5, 1.0).unwrap();
    b.add_edge(2, 3, 0.1).unwrap();
    let undirected = b.build().unwrap();

    let resolution = 0.5;
    let p_directed = run_leiden_cpm(&directed, resolution, Some(42));
    let p_undirected = run_leiden_cpm(&undirected, resolution, Some(42));

    assert_eq!(
        p_directed.num_communities(),
        p_undirected.num_communities(),
        "directed and undirected should have same number of communities"
    );

    let d_norm = normalize_membership(&partition_to_membership(&p_directed, 6));
    let u_norm = normalize_membership(&partition_to_membership(&p_undirected, 6));
    assert_eq!(
        d_norm, u_norm,
        "directed and undirected CPM should produce the same community structure"
    );
}

#[test]
fn directed_determinism() {
    let mut b = GraphDataBuilder::new(8).directed();
    for i in 0..4 {
        for j in 0..4 {
            if i != j {
                b.add_edge(i, j, 1.0).unwrap();
            }
        }
    }
    for i in 4..8 {
        for j in 4..8 {
            if i != j {
                b.add_edge(i, j, 1.0).unwrap();
            }
        }
    }
    b.add_edge(0, 4, 0.1).unwrap();
    b.add_edge(5, 1, 0.1).unwrap();
    let graph = b.build().unwrap();

    let p1 = run_leiden(&graph, Some(123));
    let p2 = run_leiden(&graph, Some(123));

    let m1 = partition_to_membership(&p1, 8);
    let m2 = partition_to_membership(&p2, 8);
    assert_eq!(
        normalize_membership(&m1),
        normalize_membership(&m2),
        "same seed should produce identical partitions"
    );
}

#[test]
fn directed_weighted() {
    let mut b = GraphDataBuilder::new(6).directed();
    b.add_edge(0, 1, 10.0).unwrap();
    b.add_edge(1, 0, 10.0).unwrap();
    b.add_edge(1, 2, 10.0).unwrap();
    b.add_edge(2, 1, 10.0).unwrap();
    b.add_edge(0, 2, 10.0).unwrap();
    b.add_edge(2, 0, 10.0).unwrap();
    b.add_edge(3, 4, 10.0).unwrap();
    b.add_edge(4, 3, 10.0).unwrap();
    b.add_edge(4, 5, 10.0).unwrap();
    b.add_edge(5, 4, 10.0).unwrap();
    b.add_edge(3, 5, 10.0).unwrap();
    b.add_edge(5, 3, 10.0).unwrap();
    b.add_edge(0, 3, 0.5).unwrap();
    b.add_edge(3, 0, 0.5).unwrap();
    let graph = b.build().unwrap();

    let partition = run_leiden(&graph, Some(42));
    assert_eq!(
        partition.num_communities(),
        2,
        "directed weighted should split into 2 communities"
    );
    assert_community_structure(&partition, 6, &[0, 0, 0, 1, 1, 1]);

    let q = compute_modularity(&graph, &partition);
    assert!(q > 0.0, "modularity should be positive, got {:.10}", q);
}

#[test]
fn directed_single_edge() {
    let mut b = GraphDataBuilder::new(2).directed();
    b.add_edge(0, 1, 1.0).unwrap();
    let graph = b.build().unwrap();
    assert!(graph.is_directed());

    let partition = run_leiden(&graph, Some(42));
    assert_eq!(
        partition.num_communities(),
        2,
        "directed single edge 0->1 should yield 2 communities (isolated nodes)"
    );
}

#[test]
fn directed_self_loop() {
    let mut b = GraphDataBuilder::new(3).directed();
    b.add_edge(0, 0, 1.0).unwrap();
    b.add_edge(1, 2, 1.0).unwrap();
    b.add_edge(2, 1, 1.0).unwrap();
    let graph = b.build().unwrap();

    let _ = run_leiden(&graph, Some(42));
}

#[test]
fn directed_disconnected() {
    let mut b = GraphDataBuilder::new(6).directed();
    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(1, 0, 1.0).unwrap();
    b.add_edge(0, 2, 1.0).unwrap();
    b.add_edge(2, 0, 1.0).unwrap();
    b.add_edge(1, 2, 1.0).unwrap();
    b.add_edge(2, 1, 1.0).unwrap();
    b.add_edge(3, 4, 1.0).unwrap();
    b.add_edge(4, 3, 1.0).unwrap();
    b.add_edge(3, 5, 1.0).unwrap();
    b.add_edge(5, 3, 1.0).unwrap();
    b.add_edge(4, 5, 1.0).unwrap();
    b.add_edge(5, 4, 1.0).unwrap();
    let graph = b.build().unwrap();

    let partition = run_leiden(&graph, Some(42));
    assert_eq!(
        partition.num_communities(),
        2,
        "disconnected directed triangles should yield 2 communities"
    );
    assert_community_structure(&partition, 6, &[0, 0, 0, 1, 1, 1]);
}

// ============================================================================
// Warm-start API Tests
// ============================================================================

#[test]
fn test_warm_start_basic() {
    let data = make_two_cliques();
    let leiden = Leiden::new(LeidenConfig::default());
    
    // First run from singleton partition
    let result1 = leiden.run(&data).unwrap();
    
    // Warm-start from result1's partition
    let result2 = leiden.run_with_initial_partition(&data, result1.partition.clone()).unwrap();
    
    // Should reach same or better quality
    assert!(result2.quality >= result1.quality - 1e-10);
}

#[test]
fn test_warm_start_invalid_partition_size() {
    let data = make_two_cliques();
    let leiden = Leiden::new(LeidenConfig::default());
    
    // Create partition with wrong size
    let wrong_partition = Partition::new(data.node_count() + 5);
    
    // Should return error
    let result = leiden.run_with_initial_partition(&data, wrong_partition);
    assert!(result.is_err());
}

#[test]
fn test_warm_start_preserves_good_partition() {
    let data = make_two_cliques();
    let leiden = Leiden::new(LeidenConfig {
        seed: Some(42),
        ..Default::default()
    });
    
    // Get optimal partition
    let optimal = leiden.run(&data).unwrap();
    
    // Warm-start should not degrade it
    let result = leiden.run_with_initial_partition(&data, optimal.partition.clone()).unwrap();
    assert!((result.quality - optimal.quality).abs() < 1e-10);
}

// ============================================================================
// Directed Graph Tests
// ============================================================================

fn make_directed_full_cliques() -> GraphData {
    let mut b = GraphDataBuilder::new(10).directed();
    // Clique 1: nodes 0-4 (all pairs)
    for i in 0..5 {
        for j in 0..5 {
            if i != j {
                b.add_edge(i, j, 1.0).unwrap();
            }
        }
    }
    // Clique 2: nodes 5-9 (all pairs)
    for i in 5..10 {
        for j in 5..10 {
            if i != j {
                b.add_edge(i, j, 1.0).unwrap();
            }
        }
    }
    b.build().unwrap()
}

#[test]
fn test_directed_two_cliques() {
    let data = make_directed_full_cliques();
    let partition = run_leiden(&data, Some(42));
    
    assert_eq!(partition.num_communities(), 2);
    // Verify clique structure
    let c0 = partition.community_of(0);
    let c5 = partition.community_of(5);
    assert_ne!(c0, c5, "two cliques should be in different communities");
    
    for i in 0..5 {
        assert_eq!(partition.community_of(i), c0);
    }
    for i in 5..10 {
        assert_eq!(partition.community_of(i), c5);
    }
}

#[test]
fn test_directed_dag() {
    let mut b = GraphDataBuilder::new(6).directed();
    // DAG: 0 -> 1 -> 2 -> 3
    //           -> 4 -> 5
    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(1, 2, 1.0).unwrap();
    b.add_edge(2, 3, 1.0).unwrap();
    b.add_edge(1, 4, 1.0).unwrap();
    b.add_edge(4, 5, 1.0).unwrap();
    let data = b.build().unwrap();
    
    let partition = run_leiden(&data, Some(42));
    // Should find some community structure
    assert!(partition.num_communities() >= 1);
    assert!(partition.num_communities() <= 6);
}

#[test]
fn test_directed_strongly_connected() {
    // Two strongly connected triangles with weak bridge
    let mut b = GraphDataBuilder::new(6).directed();
    // Triangle 1: 0 <-> 1 <-> 2 <-> 0
    for &(u, v) in &[(0, 1), (1, 0), (1, 2), (2, 1), (2, 0), (0, 2)] {
        b.add_edge(u, v, 1.0).unwrap();
    }
    // Triangle 2: 3 <-> 4 <-> 5 <-> 3
    for &(u, v) in &[(3, 4), (4, 3), (4, 5), (5, 4), (5, 3), (3, 5)] {
        b.add_edge(u, v, 1.0).unwrap();
    }
    // Weak bridge
    b.add_edge(2, 3, 0.1).unwrap();
    let data = b.build().unwrap();

    let partition = run_leiden(&data, Some(42));
    assert_eq!(partition.num_communities(), 2);
    assert_eq!(partition.community_of(0), partition.community_of(1));
    assert_eq!(partition.community_of(0), partition.community_of(2));
    assert_eq!(partition.community_of(3), partition.community_of(4));
    assert_eq!(partition.community_of(3), partition.community_of(5));
    assert_ne!(partition.community_of(0), partition.community_of(3));
}

#[test]
fn test_directed_self_loops() {
    let mut b = GraphDataBuilder::new(3).directed();
    b.add_edge(0, 0, 2.0).unwrap(); // self-loop
    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(1, 0, 1.0).unwrap();
    b.add_edge(1, 2, 1.0).unwrap();
    b.add_edge(2, 1, 1.0).unwrap();
    let data = b.build().unwrap();
    
    // Should handle self-loops correctly
    let partition = run_leiden(&data, Some(42));
    assert!(partition.num_communities() >= 1);
}

#[test]
fn test_directed_modularity_calculation() {
    let data = make_directed_full_cliques();
    let partition = run_leiden(&data, Some(42));
    
    let modularity = Modularity::with_resolution(1.0);
    let q = modularity.total_quality(&data, &partition);
    
    // Two perfect cliques should have high modularity
    assert!(q > 0.3, "directed two-cliques modularity should be > 0.3, got {}", q);
}
