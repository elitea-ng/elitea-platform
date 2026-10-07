//! JSON output example: serialize community detection results to JSON.
//!
//! Run with: cargo run --example json_output

use leiden_rs::{GraphDataBuilder, Leiden, LeidenConfig, ToJson};

fn main() {
    // Build a 6-node graph with two communities and a bridge
    let mut b = GraphDataBuilder::new(6);

    // Community A: v0, v1, v2 (triangle)
    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(0, 2, 1.0).unwrap();

    // Community B: v3, v4, v5 (path)
    b.add_edge(3, 4, 1.0).unwrap();
    b.add_edge(4, 5, 1.0).unwrap();

    // Bridge between communities
    b.add_edge(2, 3, 0.5).unwrap();

    let graph = b.build().unwrap();

    // Run Leiden with reproducible seed
    let leiden = Leiden::new(LeidenConfig {
        seed: Some(42),
        ..Default::default()
    });
    let result = leiden.run(&graph).unwrap();

    // Print as compact JSON (single line)
    println!("Compact JSON:");
    println!("{}", result.to_json());

    println!();

    // Print as pretty-printed JSON (multi-line)
    println!("Pretty JSON:");
    println!("{}", result.to_json_pretty());
}
