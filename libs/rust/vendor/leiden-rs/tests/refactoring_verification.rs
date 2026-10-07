/// Custom integration test to verify the refactoring claim:
/// Single-layer multiplex with weights=[1.0] should equal standard Leiden
///
/// This test:
/// 1. Creates a known graph (two cliques connected by a bridge edge)
/// 2. Runs Leiden with seed 42
/// 3. Runs multiplex Leiden with same graph as single layer, seed 42
/// 4. Verifies both produce IDENTICAL results
/// 5. Tests with CPM quality function
/// 6. Tests with directed graph
use leiden_rs::{
    run_multiplex, GraphDataBuilder, Leiden, LeidenConfig, MultiplexConfig, QualityType,
};

#[test]
fn test_single_layer_multiplex_equals_standard_leiden_modularity() {
    let mut b = GraphDataBuilder::new(10);
    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(0, 2, 1.0).unwrap();
    b.add_edge(0, 3, 1.0).unwrap();
    b.add_edge(0, 4, 1.0).unwrap();
    b.add_edge(1, 2, 1.0).unwrap();
    b.add_edge(1, 3, 1.0).unwrap();
    b.add_edge(1, 4, 1.0).unwrap();
    b.add_edge(2, 3, 1.0).unwrap();
    b.add_edge(2, 4, 1.0).unwrap();
    b.add_edge(3, 4, 1.0).unwrap();

    b.add_edge(5, 6, 1.0).unwrap();
    b.add_edge(5, 7, 1.0).unwrap();
    b.add_edge(5, 8, 1.0).unwrap();
    b.add_edge(5, 9, 1.0).unwrap();
    b.add_edge(6, 7, 1.0).unwrap();
    b.add_edge(6, 8, 1.0).unwrap();
    b.add_edge(6, 9, 1.0).unwrap();
    b.add_edge(7, 8, 1.0).unwrap();
    b.add_edge(7, 9, 1.0).unwrap();
    b.add_edge(8, 9, 1.0).unwrap();

    b.add_edge(4, 5, 1.0).unwrap();

    let graph = b.build().unwrap();

    let config = LeidenConfig {
        seed: Some(42),
        ..Default::default()
    };
    let leiden = Leiden::new(config.clone());
    let result1 = leiden.run(&graph).unwrap();

    let config = MultiplexConfig {
        seed: Some(42),
        layer_weights: vec![1.0],
        quality: QualityType::Modularity,
        ..Default::default()
    };
    let result2 = run_multiplex(std::slice::from_ref(&graph), &config).unwrap();

    assert_eq!(
        result1.partition.as_slice(),
        result2.partition.as_slice(),
        "Single-layer multiplex should match standard Leiden"
    );

    assert!(
        (result1.quality - result2.quality).abs() < 1e-10,
        "Qualities should match: {} vs {}",
        result1.quality,
        result2.quality
    );

    assert_eq!(
        result2.layer_qualities[0], result2.quality,
        "Single layer quality should equal total quality"
    );

    println!(
        "Modularity test passed: {} communities, quality {:.4}",
        result1.partition.num_communities(),
        result1.quality
    );
}

#[test]
fn test_single_layer_multiplex_equals_standard_leiden_cpm() {
    let mut b = GraphDataBuilder::new(10);

    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(0, 2, 1.0).unwrap();
    b.add_edge(0, 3, 1.0).unwrap();
    b.add_edge(0, 4, 1.0).unwrap();
    b.add_edge(1, 2, 1.0).unwrap();
    b.add_edge(1, 3, 1.0).unwrap();
    b.add_edge(1, 4, 1.0).unwrap();
    b.add_edge(2, 3, 1.0).unwrap();
    b.add_edge(2, 4, 1.0).unwrap();
    b.add_edge(3, 4, 1.0).unwrap();

    b.add_edge(5, 6, 1.0).unwrap();
    b.add_edge(5, 7, 1.0).unwrap();
    b.add_edge(5, 8, 1.0).unwrap();
    b.add_edge(5, 9, 1.0).unwrap();
    b.add_edge(6, 7, 1.0).unwrap();
    b.add_edge(6, 8, 1.0).unwrap();
    b.add_edge(6, 9, 1.0).unwrap();
    b.add_edge(7, 8, 1.0).unwrap();
    b.add_edge(7, 9, 1.0).unwrap();
    b.add_edge(8, 9, 1.0).unwrap();

    b.add_edge(4, 5, 1.0).unwrap();

    let graph = b.build().unwrap();

    let config = LeidenConfig {
        seed: Some(42),
        quality: QualityType::CPM,
        ..Default::default()
    };
    let leiden = Leiden::new(config.clone());
    let result1 = leiden.run(&graph).unwrap();

    let config = MultiplexConfig {
        seed: Some(42),
        layer_weights: vec![1.0],
        quality: QualityType::CPM,
        ..Default::default()
    };
    let result2 = run_multiplex(std::slice::from_ref(&graph), &config).unwrap();

    assert_eq!(
        result1.partition.as_slice(),
        result2.partition.as_slice(),
        "Single-layer multiplex with CPM should match standard Leiden"
    );

    assert!(
        (result1.quality - result2.quality).abs() < 1e-10,
        "CPM qualities should match: {} vs {}",
        result1.quality,
        result2.quality
    );

    println!(
        "CPM test passed: {} communities, quality {:.4}",
        result1.partition.num_communities(),
        result1.quality
    );
}

#[test]
fn test_determinism_single_layer() {
    let mut b = GraphDataBuilder::new(6);
    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(1, 2, 1.0).unwrap();
    b.add_edge(2, 3, 1.0).unwrap();
    b.add_edge(3, 4, 1.0).unwrap();
    b.add_edge(4, 5, 1.0).unwrap();
    b.add_edge(0, 5, 0.5).unwrap();
    let graph = b.build().unwrap();

    let config = LeidenConfig {
        seed: Some(42),
        ..Default::default()
    };
    let leiden = Leiden::new(config.clone());

    let result1 = leiden.run(&graph).unwrap();
    let result2 = leiden.run(&graph).unwrap();

    assert_eq!(
        result1.partition.as_slice(),
        result2.partition.as_slice(),
        "Same seed should produce identical results"
    );

    println!("Determinism test passed");
}

#[test]
fn test_determinism_multiplex() {
    let mut b1 = GraphDataBuilder::new(6);
    b1.add_edge(0, 1, 1.0).unwrap();
    b1.add_edge(1, 2, 1.0).unwrap();
    b1.add_edge(2, 3, 1.0).unwrap();
    b1.add_edge(3, 4, 1.0).unwrap();
    b1.add_edge(4, 5, 1.0).unwrap();
    b1.add_edge(0, 5, 0.5).unwrap();
    let layer1 = b1.build().unwrap();

    let mut b2 = GraphDataBuilder::new(6);
    b2.add_edge(0, 1, 1.0).unwrap();
    b2.add_edge(1, 2, 1.0).unwrap();
    b2.add_edge(2, 3, 1.0).unwrap();
    b2.add_edge(3, 4, 1.0).unwrap();
    b2.add_edge(4, 5, 1.0).unwrap();
    b2.add_edge(1, 4, 0.5).unwrap();
    let layer2 = b2.build().unwrap();

    let config = MultiplexConfig {
        seed: Some(42),
        layer_weights: vec![1.0, 1.0],
        ..Default::default()
    };

    let result1 = run_multiplex(&[layer1.clone(), layer2.clone()], &config).unwrap();
    let result2 = run_multiplex(&[layer1, layer2], &config).unwrap();

    assert_eq!(
        result1.partition.as_slice(),
        result2.partition.as_slice(),
        "Same seed should produce identical multiplex results"
    );

    println!("Multiplex determinism test passed");
}

#[test]
fn test_directed_graph_equivalence() {
    let mut b = GraphDataBuilder::new(5);

    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(1, 2, 1.0).unwrap();
    b.add_edge(2, 3, 1.0).unwrap();
    b.add_edge(3, 4, 1.0).unwrap();
    b.add_edge(4, 0, 1.0).unwrap();

    b.add_edge(1, 0, 0.5).unwrap();
    b.add_edge(2, 1, 0.5).unwrap();

    let graph = b.directed().build().unwrap();

    let config = LeidenConfig {
        seed: Some(42),
        ..Default::default()
    };
    let leiden = Leiden::new(config.clone());
    let result1 = leiden.run(&graph).unwrap();

    let config = MultiplexConfig {
        seed: Some(42),
        layer_weights: vec![1.0],
        ..Default::default()
    };
    let result2 = run_multiplex(std::slice::from_ref(&graph), &config).unwrap();

    assert_eq!(
        result1.partition.as_slice(),
        result2.partition.as_slice(),
        "Single-layer multiplex on directed graph should match standard Leiden"
    );

    println!(
        "Directed graph test passed: {} communities, quality {:.4}",
        result1.partition.num_communities(),
        result1.quality
    );
}

#[test]
fn test_all_quality_functions_equivalence() {
    let mut b = GraphDataBuilder::new(8);

    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(1, 2, 1.0).unwrap();
    b.add_edge(2, 3, 1.0).unwrap();
    b.add_edge(3, 0, 1.0).unwrap();
    b.add_edge(4, 5, 1.0).unwrap();
    b.add_edge(5, 6, 1.0).unwrap();
    b.add_edge(6, 7, 1.0).unwrap();
    b.add_edge(7, 4, 1.0).unwrap();
    b.add_edge(0, 4, 0.5).unwrap();
    b.add_edge(1, 5, 0.5).unwrap();

    let graph = b.build().unwrap();

    let quality_functions = [
        QualityType::Modularity,
        QualityType::CPM,
        QualityType::RBConfiguration,
        QualityType::RBER,
    ];

    for quality in quality_functions.iter() {
        let config = LeidenConfig {
            seed: Some(42),
            quality: *quality,
            ..Default::default()
        };
        let leiden = Leiden::new(config.clone());
        let result1 = leiden.run(&graph).unwrap();

        let config = MultiplexConfig {
            seed: Some(42),
            layer_weights: vec![1.0],
            quality: *quality,
            ..Default::default()
        };
        let result2 = run_multiplex(std::slice::from_ref(&graph), &config).unwrap();

        assert_eq!(
            result1.partition.as_slice(),
            result2.partition.as_slice(),
            "Quality function {:?} should produce identical results",
            quality
        );

        assert!(
            (result1.quality - result2.quality).abs() < 1e-10,
            "Quality function {:?} quality mismatch: {} vs {}",
            quality,
            result1.quality,
            result2.quality
        );

        println!(
            "{:?} test passed: {} communities, quality {:.4}",
            quality,
            result1.partition.num_communities(),
            result1.quality
        );
    }
}
