use leiden_rs::{GraphDataBuilder, Leiden, LeidenConfig};

#[test]
fn test_empty_graph() {
    let builder = GraphDataBuilder::new(0);
    let graph = builder.build().unwrap();
    
    let config = LeidenConfig::default();
    let result = Leiden::new(config).run(&graph).unwrap();
    
    assert_eq!(result.partition.num_communities(), 0);
    assert_eq!(result.quality, 0.0);
}

#[test]
fn test_single_node() {
    let builder = GraphDataBuilder::new(1);
    let graph = builder.build().unwrap();
    
    let config = LeidenConfig::default();
    let result = Leiden::new(config).run(&graph).unwrap();
    
    assert_eq!(result.partition.num_communities(), 1);
}

#[test]
fn test_self_loops() {
    let mut builder = GraphDataBuilder::new(4);
    builder.add_edge(0, 1, 1.0).unwrap();
    builder.add_edge(1, 2, 1.0).unwrap();
    builder.add_edge(2, 0, 1.0).unwrap();
    builder.add_edge(0, 0, 2.0).unwrap(); // self-loop
    let graph = builder.build().unwrap();
    
    let config = LeidenConfig { seed: Some(42), ..Default::default() };
    let result = Leiden::new(config).run(&graph).unwrap();
    
    assert!(result.partition.num_communities() >= 1);
}

#[test]
fn test_determinism() {
    let mut builder = GraphDataBuilder::new(10);
    for i in 0..5 {
        for j in (i+1)..5 {
            builder.add_edge(i, j, 1.0).unwrap();
        }
    }
    for i in 5..10 {
        for j in (i+1)..10 {
            builder.add_edge(i, j, 1.0).unwrap();
        }
    }
    builder.add_edge(0, 5, 1.0).unwrap();
    let graph = builder.build().unwrap();
    
    let seed = Some(12345);
    let config = LeidenConfig { seed, ..Default::default() };
    
    let result1 = Leiden::new(config.clone()).run(&graph).unwrap();
    let result2 = Leiden::new(config).run(&graph).unwrap();
    
    // Same seed should produce identical results
    assert_eq!(result1.partition.as_slice(), result2.partition.as_slice());
    assert!((result1.quality - result2.quality).abs() < 1e-10);
}

#[test]
fn test_directed_graph() {
    let mut builder = GraphDataBuilder::new(4).directed();
    builder.add_edge(0, 1, 1.0).unwrap();
    builder.add_edge(1, 2, 1.0).unwrap();
    builder.add_edge(2, 0, 1.0).unwrap();
    let graph = builder.build().unwrap();
    
    let config = LeidenConfig { seed: Some(42), ..Default::default() };
    let result = Leiden::new(config).run(&graph).unwrap();
    
    assert!(result.partition.num_communities() >= 1);
}

#[test]
fn test_parallel_path_produces_same_results() {
    // Create a larger graph to trigger parallel path
    let mut builder = GraphDataBuilder::new(200);
    for i in 0..100 {
        for j in (i+1)..100 {
            if j - i < 10 { // Create local connections
                builder.add_edge(i, j, 1.0).unwrap();
            }
        }
    }
    for i in 100..200 {
        for j in (i+1)..200 {
            if j - i < 10 { // Create local connections
                builder.add_edge(i, j, 1.0).unwrap();
            }
        }
    }
    builder.add_edge(50, 150, 0.1).unwrap(); // Weak connection between clusters
    let graph = builder.build().unwrap();
    
    let seed = Some(42);
    let config = LeidenConfig { seed, ..Default::default() };
    
    // Run multiple times with same seed
    let result1 = Leiden::new(config.clone()).run(&graph).unwrap();
    let result2 = Leiden::new(config).run(&graph).unwrap();
    
    // Should be deterministic
    assert_eq!(result1.partition.as_slice(), result2.partition.as_slice());
    assert!((result1.quality - result2.quality).abs() < 1e-10);
}
