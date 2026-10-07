use crate::common::*;

fn load_email_enron() -> GraphData {
    EMAIL_ENRON.load().unwrap()
}

#[test]
fn test_email_enron_basic() {
    let graph = load_email_enron();
    let partition = run_leiden(&graph, 42);
    let quality = leiden_rs::compute_modularity(&graph, &partition);

    assert!(
        partition.num_communities() > 1,
        "expected >1 communities, got {}",
        partition.num_communities(),
    );
    assert!(
        quality > 0.2,
        "modularity should be > 0.2, got {:.4}",
        quality,
    );
    assert_eq!(partition.len(), EMAIL_ENRON.node_count);
}

#[test]
fn test_email_enron_determinism() {
    let graph = load_email_enron();
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
fn test_email_enron_modularity_improvement() {
    let graph = load_email_enron();
    let trivial = Partition::new(EMAIL_ENRON.node_count);
    let trivial_q = leiden_rs::compute_modularity(&graph, &trivial);
    let leiden_q = leiden_rs::compute_modularity(&graph, &run_leiden(&graph, 42));

    assert!(
        leiden_q > trivial_q,
        "Leiden modularity ({:.4}) should exceed trivial ({:.4})",
        leiden_q,
        trivial_q,
    );
}
