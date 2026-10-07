use crate::common::*;

/// Jazz Musicians collaboration network (Gleiser & Danon, 2003).
/// 198 musicians, 2742 undirected edges. Source: KONECT arenas-jazz dataset.
fn make_jazz() -> GraphData {
    load_edgelist(include_str!("../../data/jazz.edgelist"), 198).unwrap()
}

#[test]
fn test_jazz_basic() {
    let graph = make_jazz();
    let partition = run_leiden(&graph, 42);
    let quality = compute_modularity(&graph, &partition);

    let num_comm = partition.num_communities();
    assert!(
        (3..=8).contains(&num_comm),
        "expected 3-8 communities, got {num_comm}",
    );
    assert!(
        quality > 0.35,
        "modularity should be > 0.35, got {:.4}",
        quality,
    );
    assert_eq!(partition.len(), 198);
}

#[test]
fn test_jazz_determinism() {
    let graph = make_jazz();
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
fn test_jazz_modularity_improvement() {
    let graph = make_jazz();
    let trivial = Partition::new(198);
    let trivial_q = compute_modularity(&graph, &trivial);
    let leiden_q = compute_modularity(&graph, &run_leiden(&graph, 42));

    assert!(
        leiden_q > trivial_q,
        "Leiden modularity ({:.4}) should exceed trivial ({:.4})",
        leiden_q,
        trivial_q,
    );
}

#[test]
fn test_jazz_cpm_resolution_sweep() {
    let graph = make_jazz();
    let low = run_leiden_cpm(&graph, 42, 0.1);
    let high = run_leiden_cpm(&graph, 42, 1.5);
    assert!(
        high.num_communities() >= low.num_communities(),
        "higher resolution should produce at least as many communities: \
         gamma=1.5 gave {} vs gamma=0.1 gave {}",
        high.num_communities(),
        low.num_communities(),
    );
}
