use crate::common::*;

const CORA_NODE_COUNT: usize = 2708;

fn load_cora() -> GraphData {
    let manifest_dir = std::env::var("CARGO_MANIFEST_DIR").unwrap();
    let path = std::path::Path::new(&manifest_dir)
        .join("data")
        .join("cora.edgelist");
    let text = std::fs::read_to_string(&path)
        .unwrap_or_else(|e| panic!("Failed to read {}: {e}", path.display()));
    leiden_rs::load_edgelist(&text, CORA_NODE_COUNT).unwrap()
}

#[test]
fn test_cora_basic() {
    let graph = load_cora();
    let partition = run_leiden(&graph, 42);
    let quality = leiden_rs::compute_modularity(&graph, &partition);

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
    assert_eq!(partition.len(), CORA_NODE_COUNT);
}

#[test]
fn test_cora_determinism() {
    let graph = load_cora();
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
    let graph = load_cora();
    let trivial = Partition::new(CORA_NODE_COUNT);
    let trivial_q = leiden_rs::compute_modularity(&graph, &trivial);
    let leiden_q = leiden_rs::compute_modularity(&graph, &run_leiden(&graph, 42));

    assert!(
        leiden_q > trivial_q,
        "Leiden modularity ({:.4}) should exceed trivial ({:.4})",
        leiden_q,
        trivial_q,
    );
}

#[test]
fn test_cora_cpm_resolution_sweep() {
    let graph = load_cora();
    let config_low = LeidenConfig {
        seed: Some(42),
        quality: leiden_rs::QualityType::CPM,
        resolution: 0.001,
        ..Default::default()
    };
    let config_high = LeidenConfig {
        seed: Some(42),
        quality: leiden_rs::QualityType::CPM,
        resolution: 0.1,
        ..Default::default()
    };
    let low = Leiden::new(config_low).run(&graph).unwrap().partition;
    let high = Leiden::new(config_high).run(&graph).unwrap().partition;
    assert!(
        high.num_communities() >= low.num_communities(),
        "higher resolution should produce at least as many communities: \
         gamma=0.1 gave {} vs gamma=0.001 gave {}",
        high.num_communities(),
        low.num_communities(),
    );
}
