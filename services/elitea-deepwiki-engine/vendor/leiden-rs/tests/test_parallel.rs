use leiden_rs::resolution::resolution_scan;
use leiden_rs::{GraphDataBuilder, Leiden, LeidenConfig, QualityType};

#[test]
fn test_small_graph_sequential_path() {
    let mut b1 = GraphDataBuilder::new(50);
    let mut edge_count = 0;
    for u in 0..50 {
        for v in (u + 1)..50 {
            if edge_count < 100 {
                b1.add_edge(u, v, 1.0).unwrap();
                edge_count += 1;
            }
        }
    }
    let small_graph = b1.build().unwrap();
    let leiden_small = Leiden::new(LeidenConfig {
        seed: Some(42),
        resolution: 1.0,
        quality: QualityType::Modularity,
        ..Default::default()
    });
    let result_small = leiden_small.run(&small_graph).unwrap();
    assert!(result_small.partition.num_communities() >= 1);
    assert!(result_small.quality.is_finite());
}

#[test]
fn test_medium_graph_parallel_path() {
    let mut b2 = GraphDataBuilder::new(200);
    let mut edge_count = 0;
    for u in 0..200 {
        for v in (u + 1)..200 {
            if edge_count < 2500 {
                b2.add_edge(u, v, 1.0).unwrap();
                edge_count += 1;
            }
        }
    }
    let medium_graph = b2.build().unwrap();
    let leiden_medium = Leiden::new(LeidenConfig {
        seed: Some(42),
        resolution: 1.0,
        quality: QualityType::Modularity,
        ..Default::default()
    });
    let result_medium = leiden_medium.run(&medium_graph).unwrap();
    assert!(result_medium.partition.num_communities() >= 1);
    assert!(result_medium.quality.is_finite());
}

#[test]
fn test_large_graph_parallel_aggregation() {
    let mut b3 = GraphDataBuilder::new(400);
    let mut edge_count = 0;
    for u in 0..400 {
        for v in (u + 1)..400 {
            if edge_count < 12000 {
                b3.add_edge(u, v, 1.0).unwrap();
                edge_count += 1;
            }
        }
    }
    let large_graph = b3.build().unwrap();
    let leiden_large = Leiden::new(LeidenConfig {
        seed: Some(42),
        resolution: 1.0,
        quality: QualityType::Modularity,
        ..Default::default()
    });
    let result_large = leiden_large.run(&large_graph).unwrap();
    assert!(result_large.partition.num_communities() >= 1);
    assert!(result_large.quality.is_finite());
}

#[test]
fn test_resolution_scan_parallel() {
    let mut b = GraphDataBuilder::new(200);
    let mut edge_count = 0;
    for u in 0..200 {
        for v in (u + 1)..200 {
            if edge_count < 2500 {
                b.add_edge(u, v, 1.0).unwrap();
                edge_count += 1;
            }
        }
    }
    let medium_graph = b.build().unwrap();

    let scan_results = resolution_scan(
        &medium_graph,
        QualityType::Modularity,
        (0.1, 2.0),
        5,
        Some(42),
    )
    .unwrap();
    assert_eq!(scan_results.len(), 5);
    for entry in &scan_results {
        assert!(entry.quality.is_finite());
    }
}

#[test]
fn test_reproducibility_same_seed() {
    let mut b = GraphDataBuilder::new(200);
    let mut edge_count = 0;
    for u in 0..200 {
        for v in (u + 1)..200 {
            if edge_count < 2500 {
                b.add_edge(u, v, 1.0).unwrap();
                edge_count += 1;
            }
        }
    }
    let medium_graph = b.build().unwrap();

    let config = LeidenConfig {
        seed: Some(42),
        resolution: 1.0,
        quality: QualityType::Modularity,
        ..Default::default()
    };

    let leiden_run1 = Leiden::new(config.clone());
    let result_run1 = leiden_run1.run(&medium_graph).unwrap();

    let leiden_run2 = Leiden::new(config);
    let result_run2 = leiden_run2.run(&medium_graph).unwrap();

    assert_eq!(
        result_run1.partition.as_slice(),
        result_run2.partition.as_slice(),
        "Same seed produced different results"
    );
}
