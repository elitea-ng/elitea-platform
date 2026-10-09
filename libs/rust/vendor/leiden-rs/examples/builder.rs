//! Builder pattern example: configure Leiden algorithm with builder API.
//!
//! Run with: cargo run --example builder

use leiden_rs::{GraphDataBuilder, Leiden, LeidenConfig, QualityType};

fn main() {
    let mut b = GraphDataBuilder::new(12);

    // Triangle edges (strong intra-cluster connections)
    for t in 0..4 {
        let base = t * 3;
        b.add_edge(base, base + 1, 1.0).unwrap();
        b.add_edge(base + 1, base + 2, 1.0).unwrap();
        b.add_edge(base, base + 2, 1.0).unwrap();
    }

    // Bridge edges between triangles (weak inter-cluster connections)
    b.add_edge(2, 3, 0.1).unwrap();
    b.add_edge(5, 6, 0.1).unwrap();
    b.add_edge(8, 9, 0.1).unwrap();
    b.add_edge(11, 0, 0.1).unwrap();

    let graph = b.build().unwrap();

    println!("Ring of triangles: {} nodes", graph.node_count(),);

    // Configure using builder pattern with modularity
    let config = LeidenConfig::builder()
        .resolution(1.0)
        .seed(42)
        .epsilon(1e-10)
        .build();

    let leiden = Leiden::new(config);
    let result = leiden.run(&graph).expect("leiden failed");
    println!(
        "Modularity (resolution=1.0): {} communities (quality: {:.4})\n",
        result.partition.num_communities(),
        result.quality
    );

    // Try CPM with low resolution (favors larger communities)
    let cpm_config = LeidenConfig::builder()
        .quality(QualityType::CPM)
        .resolution(0.1)
        .seed(42)
        .build();

    let cpm_leiden = Leiden::new(cpm_config);
    let cpm_result = cpm_leiden.run(&graph).expect("leiden failed");
    println!(
        "CPM (resolution=0.1): {} communities (quality: {:.4})",
        cpm_result.partition.num_communities(),
        cpm_result.quality
    );

    // Try CPM with higher resolution (favors smaller communities)
    let cpm_config2 = LeidenConfig::builder()
        .quality(QualityType::CPM)
        .resolution(0.5)
        .seed(42)
        .build();

    let cpm_leiden2 = Leiden::new(cpm_config2);
    let cpm_result2 = cpm_leiden2.run(&graph).expect("leiden failed");
    println!(
        "CPM (resolution=0.5): {} communities (quality: {:.4})",
        cpm_result2.partition.num_communities(),
        cpm_result2.quality
    );
}
