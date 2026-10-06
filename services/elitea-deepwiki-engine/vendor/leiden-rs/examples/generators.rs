//! Graph generators example: generate graphs using different random graph models.
//!
//! Run with: cargo run --example generators

use leiden_rs::generators::{generate_ba_graph, generate_er_graph, generate_planted_partition};

fn main() {
    // ── Erdős-Rényi G(n, p) ──────────────────────────────────────────────
    println!("=== Erdős-Rényi G(10, 0.5) ===");
    let er = generate_er_graph(10, 0.5, Some(42)).unwrap();
    println!("  Nodes: {}", er.node_count());
    println!("  Edges: {}\n", er.total_weight() as usize);

    // ── Planted Partition ────────────────────────────────────────────────
    println!("=== Planted Partition (20 nodes, 2 communities, p_in=0.8, p_out=0.1) ===");
    let (pp, ground_truth) = generate_planted_partition(20, 2, 0.8, 0.1, Some(42)).unwrap();
    let num_communities = ground_truth.iter().max().unwrap_or(&0) + 1;
    println!("  Nodes: {}", pp.node_count());
    println!("  Edges: {}", pp.total_weight() as usize);
    println!("  Ground-truth communities: {num_communities}");
    for (node, &community) in ground_truth.iter().enumerate() {
        println!("  Node {node:>2} → Community {community}");
    }
    println!();

    // ── Barabási-Albert ──────────────────────────────────────────────────
    println!("=== Barabási-Albert (20 nodes, m=2) ===");
    let ba = generate_ba_graph(20, 2, None, Some(42)).unwrap();
    println!("  Nodes: {}", ba.node_count());
    println!("  Edges: {}", ba.total_weight() as usize);
}
