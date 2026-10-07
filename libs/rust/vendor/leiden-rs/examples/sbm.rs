//! Stochastic Block Model (SBM) example: generate SBM graphs and detect communities.
//!
//! Run with: cargo run --example sbm

use leiden_rs::{
    generate_sbm, generate_sbm_symmetric, Leiden, LeidenConfig, nmi,
};

fn main() {
    // ── Symmetric SBM ──────────────────────────────────────────────────
    println!("=== Symmetric SBM (100 nodes, 4 equal communities) ===");
    let (graph, truth) = generate_sbm_symmetric(100, 4, 0.3, 0.01, Some(42)).unwrap();
    println!(
        "Generated {} nodes, {:.0} edges, {} ground-truth communities",
        graph.node_count(),
        graph.total_weight(),
        truth.iter().max().unwrap() + 1
    );

    let leiden = Leiden::new(LeidenConfig::default());
    let result = leiden.run(&graph).unwrap();
    let score = nmi(&truth, result.partition.as_slice());
    println!("Leiden communities: {}", result.partition.num_communities());
    println!("NMI vs ground truth: {:.4}", score);
    println!();

    // ── General SBM with core-periphery structure ──────────────────────
    println!("=== General SBM (core-periphery, 3 communities) ===");
    let community_sizes = vec![20, 30, 50];
    let affinity = vec![
        vec![0.5, 0.01, 0.01],
        vec![0.01, 0.3, 0.01],
        vec![0.01, 0.01, 0.2],
    ];
    let (graph2, truth2) = generate_sbm(&community_sizes, &affinity, Some(42)).unwrap();
    println!(
        "Generated {} nodes, {:.0} edges, {} ground-truth communities",
        graph2.node_count(),
        graph2.total_weight(),
        truth2.iter().max().unwrap() + 1
    );

    let result2 = leiden.run(&graph2).unwrap();
    let score2 = nmi(&truth2, result2.partition.as_slice());
    println!("Leiden communities: {}", result2.partition.num_communities());
    println!("NMI vs ground truth: {:.4}", score2);
}
