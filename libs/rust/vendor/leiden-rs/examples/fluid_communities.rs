//! Fluid Communities example: detect communities using fluid propagation.
//!
//! Run with: cargo run --example fluid_communities

use leiden_rs::{FluidCommunities, FluidCommunitiesConfig, GraphDataBuilder};

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

    // Bridge between communities
    b.add_edge(9, 10, 1.0).unwrap();

    let graph = b.build().unwrap();

    // Run Fluid Communities with reproducibility
    let fluid = FluidCommunities::new(FluidCommunitiesConfig {
        k: 2,
        seed: Some(42),
        max_iterations: 100,
    });
    let result = fluid.run(&graph).unwrap();

    println!("Fluid Communities Results");
    println!("------------------------");
    println!("Graph: {} nodes", graph.node_count());
    println!("Communities: {}", result.partition.num_communities());
    println!("Converged: {}", result.converged);
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
