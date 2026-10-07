//! Infomap community detection example: detect communities using the Map Equation.
//!
//! Run with: cargo run --example infomap

use leiden_rs::{GraphDataBuilder, Infomap, InfomapConfig};

fn main() {
    // Build a 20-node graph with two planted communities and a weak bridge
    let mut b = GraphDataBuilder::new(20);

    // Community A: nodes 0-9 (complete graph)
    for i in 0..10 {
        for j in i + 1..10 {
            b.add_edge(i, j, 1.0).unwrap();
        }
    }

    // Community B: nodes 10-19 (complete graph)
    for i in 10..20 {
        for j in i + 1..20 {
            b.add_edge(i, j, 1.0).unwrap();
        }
    }

    // Weak bridge between communities
    b.add_edge(9, 10, 0.1).unwrap();

    let graph = b.build().unwrap();

    // Run Infomap with reproducibility
    let infomap = Infomap::new(InfomapConfig {
        seed: Some(42),
        num_trials: 5,
        ..Default::default()
    });
    let result = infomap.run(&graph);

    println!("Infomap Results");
    println!("---------------");
    println!("Graph: {} nodes", graph.node_count());
    println!("Codelength: {:.4}", result.codelength);
    println!("Communities: {}", result.partition.num_communities());
    println!("Iterations: {}", result.iterations);
    println!();

    for node in 0..graph.node_count() {
        println!(
            "  Node {} → Community {}",
            node,
            result.partition.community_of(node)
        );
    }
}
