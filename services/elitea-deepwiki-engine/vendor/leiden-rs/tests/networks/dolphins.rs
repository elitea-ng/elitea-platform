use crate::common::*;

/// Dolphins social network (Lusseau et al., 2003).
/// 62 bottlenose dolphins, 159 undirected edges. Source: Newman's network collection.
fn make_dolphins() -> GraphData {
    load_edgelist(include_str!("../../data/dolphins.edgelist"), 62).unwrap()
}

#[test]
fn test_dolphins_basic() {
    let graph = make_dolphins();
    let partition = run_leiden(&graph, 42);
    let quality = compute_modularity(&graph, &partition);

    let num_comm = partition.num_communities();
    assert!(
        (2..=6).contains(&num_comm),
        "expected 2-6 communities, got {num_comm}",
    );
    assert!(
        quality > 0.37,
        "modularity should be > 0.37, got {:.4}",
        quality,
    );
    assert_eq!(partition.len(), 62);
}

#[test]
fn test_dolphins_two_community_split() {
    let graph = make_dolphins();
    let partition = Leiden::new(LeidenConfig {
        seed: Some(42),
        resolution: 0.5,
        ..Default::default()
    })
    .run(&graph)
    .unwrap()
    .partition;

    assert!(
        partition.num_communities() <= 4,
        "low resolution should find <= 4 communities, got {}",
        partition.num_communities(),
    );
}

#[test]
fn test_dolphins_determinism() {
    let graph = make_dolphins();
    let config = LeidenConfig {
        seed: Some(99),
        ..Default::default()
    };
    let r1 = Leiden::new(config.clone()).run(&graph).unwrap();
    let r2 = Leiden::new(config).run(&graph).unwrap();
    assert_eq!(
        r1.partition.as_slice(),
        r2.partition.as_slice(),
        "seeded runs should produce identical partitions",
    );
}

#[test]
fn test_dolphins_modularity_improvement() {
    let graph = make_dolphins();
    let trivial = Partition::new(62);
    let trivial_q = compute_modularity(&graph, &trivial);
    let leiden_q = compute_modularity(&graph, &run_leiden(&graph, 42));

    assert!(
        leiden_q > trivial_q,
        "Leiden modularity ({:.4}) should exceed trivial ({:.4})",
        leiden_q,
        trivial_q,
    );
}
