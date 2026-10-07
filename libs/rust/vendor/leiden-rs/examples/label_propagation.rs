//! Label Propagation example: detect communities using fast label propagation.
//!
//! Run with: cargo run --example label_propagation

use leiden_rs::{
    label_propagation::{LabelPropagation, LabelPropagationConfig},
    GraphDataBuilder,
};

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

    // Run Label Propagation with reproducible seed
    let config = LabelPropagationConfig {
        seed: Some(42),
        ..Default::default()
    };
    let lp = LabelPropagation::new(config);
    let result = lp.run(&graph);

    println!("Label Propagation Results");
    println!("------------------------");
    println!("Graph: {} nodes", graph.node_count());
    println!("Communities found: {}", result.partition.num_communities());
    println!("Iterations: {}", result.iterations);
    println!("Converged: {}", result.converged);
    println!();

    for node in 0..graph.node_count() {
        println!(
            "  Node {} → Community {}",
            node,
            result.partition.community_of(node)
        );
    }
}
