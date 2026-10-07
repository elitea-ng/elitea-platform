use leiden_rs::{GraphDataBuilder, Leiden, LeidenConfig};

fn main() {
    let mut b = GraphDataBuilder::new(6).directed();

    // Cluster 1: 0→1→2→0 (directed cycle)
    b.add_edge(0, 1, 1.0).unwrap();
    b.add_edge(1, 2, 1.0).unwrap();
    b.add_edge(2, 0, 1.0).unwrap();

    // Cluster 2: 3→4→5→3 (directed cycle)
    b.add_edge(3, 4, 1.0).unwrap();
    b.add_edge(4, 5, 1.0).unwrap();
    b.add_edge(5, 3, 1.0).unwrap();

    // Weak bridge between clusters
    b.add_edge(2, 3, 0.1).unwrap();

    let graph = b.build().unwrap();

    println!("Directed graph: {} nodes", graph.node_count());

    let result = Leiden::new(LeidenConfig {
        seed: Some(42),
        ..Default::default()
    })
    .run(&graph)
    .expect("leiden failed");

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
