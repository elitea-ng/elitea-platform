use crate::common::*;

/// Zachary's Karate Club (Zachary, 1977).
/// 34 members, 78 undirected edges. Source: NetworkX karate_club_graph().
fn make_karate() -> GraphData {
    load_edgelist(include_str!("../../data/karate.edgelist"), 34).unwrap()
}

#[test]
fn test_karate_basic() {
    let graph = make_karate();
    let partition = run_leiden(&graph, 42);
    let quality = compute_modularity(&graph, &partition);

    let num_comm = partition.num_communities();
    assert!(
        (2..=4).contains(&num_comm),
        "expected 2-4 communities, got {num_comm}",
    );
    assert!(
        quality > 0.35,
        "modularity should be > 0.35, got {:.4}",
        quality,
    );
    assert_eq!(partition.len(), 34);
}

#[test]
fn test_karate_nmi_against_ground_truth() {
    let graph = make_karate();
    let partition = run_leiden(&graph, 42);

    // Ground truth: Mr. Hi's faction vs Officer faction (Zachary 1977)
    let ground_truth: Vec<usize> = vec![
        1, 1, 1, 1, 1, 1, 1, 1, 0, 0, 1, 1, 1, 1, 0, 0, 1, 1, 0, 1, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0,
        0, 0, 0, 0,
    ];
    let nmi = leiden_rs::nmi(&ground_truth, partition.as_slice());
    assert!(
        nmi > 0.5,
        "NMI against ground truth should be > 0.5, got {:.4}",
        nmi,
    );
}

#[test]
fn test_karate_determinism() {
    let graph = make_karate();
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
fn test_karate_cpm_resolution_sweep() {
    let graph = make_karate();
    let low = run_leiden_cpm(&graph, 42, 0.1);
    let high = run_leiden_cpm(&graph, 42, 2.0);
    assert!(
        high.num_communities() >= low.num_communities(),
        "higher resolution should produce at least as many communities: \
         gamma=2.0 gave {} vs gamma=0.1 gave {}",
        high.num_communities(),
        low.num_communities(),
    );
}
