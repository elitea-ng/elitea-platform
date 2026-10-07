use crate::common::*;
use leiden_rs::{nmi, LfrConfig};

fn default_lfr_config(mu: f64) -> LfrConfig {
    LfrConfig {
        n: 250,
        tau1: 3.0,
        tau2: 1.5,
        mu,
        average_degree: Some(5.0),
        min_degree: None,
        max_degree: None,
        min_community: Some(20),
        max_community: None,
        seed: Some(42),
    }
}

#[test]
fn test_lfr_easy_mu01() {
    let (lfr, _, partition) = run_lfr_leiden(default_lfr_config(0.1));
    let score = nmi(&lfr.ground_truth, partition.as_slice());
    assert!(
        score > 0.85,
        "NMI at mu=0.1 should be > 0.85, got {:.4}",
        score,
    );
    assert!(partition.num_communities() >= 2);
}

#[test]
fn test_lfr_medium_mu03() {
    let (lfr, _, partition) = run_lfr_leiden(default_lfr_config(0.3));
    let score = nmi(&lfr.ground_truth, partition.as_slice());
    assert!(
        score > 0.70,
        "NMI at mu=0.3 should be > 0.70, got {:.4}",
        score,
    );
}

#[test]
fn test_lfr_hard_mu05() {
    let (lfr, _, partition) = run_lfr_leiden(default_lfr_config(0.5));
    let score = nmi(&lfr.ground_truth, partition.as_slice());
    assert!(
        score > 0.30,
        "NMI at mu=0.5 should be > 0.30, got {:.4}",
        score,
    );
}

#[test]
fn test_lfr_nmi_degrades_with_mu() {
    let mus = [0.1, 0.2, 0.3, 0.4, 0.5];
    let mut scores: Vec<f64> = Vec::with_capacity(mus.len());
    for &mu in &mus {
        let (lfr, _, partition) = run_lfr_leiden(default_lfr_config(mu));
        let score = nmi(&lfr.ground_truth, partition.as_slice());
        scores.push(score);
    }
    for i in 1..scores.len() {
        assert!(
            scores[i] <= scores[i - 1] + 0.05,
            "NMI should generally decrease as mu increases, but mu={:.1} NMI={:.4} > mu={:.1} NMI={:.4}",
            mus[i], scores[i], mus[i - 1], scores[i - 1],
        );
    }
}

#[test]
fn test_lfr_modularity_improvement() {
    let (lfr, graph, partition) = run_lfr_leiden(default_lfr_config(0.3));
    let trivial = Partition::new(lfr.node_count);
    let trivial_q = compute_modularity(&graph, &trivial);
    let leiden_q = compute_modularity(&graph, &partition);
    assert!(
        leiden_q > trivial_q,
        "Leiden modularity ({:.4}) should exceed trivial ({:.4})",
        leiden_q,
        trivial_q,
    );
}

#[test]
fn test_lfr_determinism() {
    let mut config = default_lfr_config(0.3);
    config.seed = Some(99);
    let (lfr1, _, _) = run_lfr_leiden(config.clone());
    let (lfr2, _, _) = run_lfr_leiden(config);
    assert_eq!(
        lfr1.ground_truth, lfr2.ground_truth,
        "same seed should produce identical ground truth",
    );
}

#[test]
fn test_lfr_cpm_resolution_sweep() {
    let (_, graph, _) = run_lfr_leiden(default_lfr_config(0.3));
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
