//! Louvain comparison example: compare Leiden vs Louvain (skip_refinement) results.
//!
//! Run with: cargo run --example louvain

use leiden_rs::{GraphDataBuilder, Leiden, LeidenConfig};

fn main() {
    // Build a 6-node graph with two clear communities
    let mut b = GraphDataBuilder::new(6);

    // Community A: v0, v1, v2 (triangle)
    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(1, 2, 1.0).unwrap();
    b.add_edge(0, 2, 1.0).unwrap();

    // Community B: v3, v4, v5 (triangle)
    b.add_edge(3, 4, 1.0).unwrap();
    b.add_edge(4, 5, 1.0).unwrap();
    b.add_edge(3, 5, 1.0).unwrap();

    // Weak bridge between communities
    b.add_edge(2, 3, 0.1).unwrap();

    let graph = b.build().unwrap();

    // Run full Leiden (with refinement)
    let leiden_config = LeidenConfig {
        seed: Some(42),
        skip_refinement: false,
        ..Default::default()
    };
    let leiden = Leiden::new(leiden_config);
    let leiden_result = leiden.run(&graph).expect("leiden failed");

    // Run Louvain mode (skip refinement)
    let louvain_config = LeidenConfig {
        seed: Some(42),
        skip_refinement: true,
        ..Default::default()
    };
    let louvain = Leiden::new(louvain_config);
    let louvain_result = louvain.run(&graph).expect("louvain failed");

    // Print per-node assignments
    println!("Leiden communities (with refinement):");
    for node in 0..graph.node_count() {
        println!(
            "  Node {} → Community {}",
            node,
            leiden_result.partition.community_of(node)
        );
    }
    println!();

    println!("Louvain communities (skip_refinement):");
    for node in 0..graph.node_count() {
        println!(
            "  Node {} → Community {}",
            node,
            louvain_result.partition.community_of(node)
        );
    }
    println!();

    // Print comparison summary
    println!("Comparison:");
    println!(
        "  Leiden quality:  {:.6}  ({} communities)",
        leiden_result.quality,
        leiden_result.partition.num_communities()
    );
    println!(
        "  Louvain quality: {:.6}  ({} communities)",
        louvain_result.quality,
        louvain_result.partition.num_communities()
    );
}
