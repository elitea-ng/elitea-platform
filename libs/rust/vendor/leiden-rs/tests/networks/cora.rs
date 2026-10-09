use crate::common::*;

/// Cora citation network (McCallum et al., 2000).
/// 2708 papers, 5278 undirected edges. Source: LINQS/UCSC Cora dataset.
///
/// Note: Cora has 7 ground-truth paper topic classes, but modularity-based
/// community detection typically finds 80–120 topological communities. This is
/// expected — the topological structure is far more fragmented than the semantic
/// classification. The reference leidenalg Python library produces ~106 communities
/// with modularity ~0.82 at gamma=1.0.
fn make_cora() -> GraphData {
    load_edgelist(include_str!("../../data/cora.edgelist"), 2708).unwrap()
}

#[test]
fn test_cora_basic() {
    let graph = make_cora();
    let partition = run_leiden(&graph, 42);
    let quality = compute_modularity(&graph, &partition);

    let num_comm = partition.num_communities();
    assert!(
        (50..=200).contains(&num_comm),
        "expected 50-200 communities (reference: ~106), got {num_comm}",
    );
    assert!(
        quality > 0.75,
        "modularity should be > 0.75 (reference: ~0.82), got {:.4}",
        quality,
    );
    assert_eq!(partition.len(), 2708);
}

#[test]
fn test_cora_determinism() {
    let graph = make_cora();
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
fn test_cora_modularity_improvement() {
    let graph = make_cora();
    let trivial = Partition::new(2708);
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
fn test_cora_cpm_resolution_sweep() {
    let graph = make_cora();
    let low = run_leiden_cpm(&graph, 42, 0.001);
    let high = run_leiden_cpm(&graph, 42, 0.1);
    assert!(
        high.num_communities() >= low.num_communities(),
        "higher resolution should produce at least as many communities: \
         gamma=0.1 gave {} vs gamma=0.001 gave {}",
        high.num_communities(),
        low.num_communities(),
    );
}
