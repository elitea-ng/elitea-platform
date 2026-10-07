//! Basic example: detect communities in a small graph using leiden-rs.
//!
//! Run with: cargo run --example basic

use leiden_rs::{GraphDataBuilder, Leiden, LeidenConfig};

fn main() {
    let mut b = GraphDataBuilder::new(6);

    // Cluster 1: v0, v1, v2 (strongly connected)
    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(1, 2, 1.0).unwrap();
    b.add_edge(0, 2, 1.0).unwrap();

    // Cluster 2: v3, v4, v5 (strongly connected)
    b.add_edge(3, 4, 1.0).unwrap();
    b.add_edge(4, 5, 1.0).unwrap();
    b.add_edge(3, 5, 1.0).unwrap();

    // Weak bridge between clusters
    b.add_edge(2, 3, 0.1).unwrap();

    let graph = b.build().unwrap();

    // Run Leiden algorithm with default settings
    let leiden = Leiden::new(LeidenConfig::default());
    let result = leiden.run(&graph).expect("leiden failed");

    println!("Graph has {} nodes", graph.node_count());
    println!(
        "Detected {} communities (quality: {:.4})",
        result.partition.num_communities(),
        result.quality
    );
    println!();

    for node in 0..graph.node_count() {
        println!(
            "  Node {} → Community {}",
            node,
            result.partition.community_of(node)
        );
    }
}
