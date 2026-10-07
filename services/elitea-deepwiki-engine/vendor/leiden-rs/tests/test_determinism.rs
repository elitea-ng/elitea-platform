use leiden_rs::{GraphData, GraphDataBuilder, Leiden, LeidenConfig};
use rand::rngs::StdRng;
use rand::Rng;
use rand::SeedableRng;

fn make_large_graph(n: usize, edges_per_node: usize) -> GraphData {
    let mut rng = StdRng::seed_from_u64(12345);
    let mut b = GraphDataBuilder::new(n);

    for u in 0..n {
        for _ in 0..edges_per_node {
            let v = rng.random_range(0..n);
            if v != u {
                b.add_edge(u, v, 1.0).unwrap();
            }
        }
    }

    b.build().unwrap()
}

#[test]
fn test_same_seed_identical_results() {
    let graph = make_large_graph(500, 10);

    let config = LeidenConfig {
        seed: Some(42),
        ..Default::default()
    };
    let leiden1 = Leiden::new(config.clone());
    let result1 = leiden1.run(&graph).unwrap();

    let leiden2 = Leiden::new(config);
    let result2 = leiden2.run(&graph).unwrap();

    let p1 = result1.partition.as_slice();
    let p2 = result2.partition.as_slice();

    assert_eq!(p1, p2, "Same seed produced different results!");
}

#[test]
fn test_different_seeds_different_results() {
    let graph = make_large_graph(500, 10);

    let config1 = LeidenConfig {
        seed: Some(42),
        ..Default::default()
    };
    let leiden1 = Leiden::new(config1.clone());
    let result1 = leiden1.run(&graph).unwrap();

    let config3 = LeidenConfig {
        seed: Some(999),
        ..Default::default()
    };
    let leiden3 = Leiden::new(config3);
    let result3 = leiden3.run(&graph).unwrap();

    let p1 = result1.partition.as_slice();
    let p3 = result3.partition.as_slice();

    assert_ne!(p1, p3, "Different seeds produced same results!");
}

#[test]
fn test_at_parallel_threshold_boundary() {
    let mut b = GraphDataBuilder::new(100);
    let mut edge_count = 0;
    for u in 0..100 {
        let num_edges = std::cmp::min(20, 2000 - edge_count);
        for v_offset in 1..=num_edges {
            let v = (u + v_offset) % 100;
            if edge_count < 2000 {
                b.add_edge(u, v, 1.0).unwrap();
                edge_count += 1;
            }
        }
    }
    let boundary_graph = b.build().unwrap();

    let config = LeidenConfig {
        seed: Some(42),
        ..Default::default()
    };
    let leiden = Leiden::new(config);
    let result = leiden.run(&boundary_graph).unwrap();

    assert!(result.partition.num_communities() >= 1);
    assert!(result.quality.is_finite());
}

#[test]
fn test_empty_graph_handling() {
    let empty_graph = GraphDataBuilder::new(0).build().unwrap();
    let leiden = Leiden::new(LeidenConfig::default());
    let result = leiden.run(&empty_graph).unwrap();
    assert_eq!(result.partition.num_communities(), 0);
}

#[test]
fn test_single_node_handling() {
    let single_graph = GraphDataBuilder::new(1).build().unwrap();
    let leiden = Leiden::new(LeidenConfig {
        seed: Some(42),
        ..Default::default()
    });
    let result = leiden.run(&single_graph).unwrap();
    assert_eq!(result.partition.num_communities(), 1);
}

#[test]
fn test_two_nodes_handling() {
    let mut b2 = GraphDataBuilder::new(2);
    b2.add_edge(0, 1, 1.0).unwrap();
    let two_node_graph = b2.build().unwrap();
    let leiden = Leiden::new(LeidenConfig {
        seed: Some(42),
        ..Default::default()
    });
    let result = leiden.run(&two_node_graph).unwrap();
    assert_eq!(result.partition.num_communities(), 1);
}

#[test]
fn test_clique_handling() {
    let mut b3 = GraphDataBuilder::new(20);
    for i in 0..20 {
        for j in (i + 1)..20 {
            b3.add_edge(i, j, 1.0).unwrap();
        }
    }
    let clique_graph = b3.build().unwrap();
    let leiden = Leiden::new(LeidenConfig {
        seed: Some(42),
        ..Default::default()
    });
    let result = leiden.run(&clique_graph).unwrap();
    assert!(result.partition.num_communities() >= 1);
    assert!(result.quality.is_finite());
}

#[test]
fn test_isolated_nodes_handling() {
    let isolated_graph = GraphDataBuilder::new(10).build().unwrap();
    let leiden = Leiden::new(LeidenConfig {
        seed: Some(42),
        ..Default::default()
    });
    let result = leiden.run(&isolated_graph).unwrap();
    assert_eq!(result.partition.num_communities(), 10);
}

#[test]
fn test_at_aggregation_threshold() {
    let mut b4 = GraphDataBuilder::new(200);
    let mut edge_count = 0;
    for u in 0..200 {
        let num_edges = std::cmp::min(50, 10000 - edge_count);
        for v_offset in 1..=num_edges {
            let v = (u + v_offset) % 200;
            if edge_count < 10000 {
                b4.add_edge(u, v, 1.0).unwrap();
                edge_count += 1;
            }
        }
    }
    let agg_graph = b4.build().unwrap();
    let leiden = Leiden::new(LeidenConfig {
        seed: Some(42),
        ..Default::default()
    });
    let result = leiden.run(&agg_graph).unwrap();
    assert!(result.partition.num_communities() >= 1);
    assert!(result.quality.is_finite());
}

#[test]
fn test_quality_values_are_valid() {
    let graph = make_large_graph(500, 10);

    let config1 = LeidenConfig {
        seed: Some(42),
        ..Default::default()
    };
    let result1 = Leiden::new(config1).run(&graph).unwrap();

    let mut b = GraphDataBuilder::new(100);
    let mut edge_count = 0;
    for u in 0..100 {
        let num_edges = std::cmp::min(20, 2000 - edge_count);
        for v_offset in 1..=num_edges {
            let v = (u + v_offset) % 100;
            if edge_count < 2000 {
                b.add_edge(u, v, 1.0).unwrap();
                edge_count += 1;
            }
        }
    }
    let boundary_graph = b.build().unwrap();
    let result4 = Leiden::new(LeidenConfig {
        seed: Some(42),
        ..Default::default()
    })
    .run(&boundary_graph)
    .unwrap();

    let mut b3 = GraphDataBuilder::new(20);
    for i in 0..20 {
        for j in (i + 1)..20 {
            b3.add_edge(i, j, 1.0).unwrap();
        }
    }
    let clique_graph = b3.build().unwrap();
    let result8 = Leiden::new(LeidenConfig {
        seed: Some(42),
        ..Default::default()
    })
    .run(&clique_graph)
    .unwrap();

    assert!(result1.quality.is_finite() && result1.quality >= 0.0);
    assert!(result4.quality.is_finite() && result4.quality >= 0.0);
    assert!(result8.quality.is_finite() && result8.quality >= 0.0);
}
