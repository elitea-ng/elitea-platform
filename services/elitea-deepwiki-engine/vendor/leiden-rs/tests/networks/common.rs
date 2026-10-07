pub use leiden_rs::{
    compute_modularity, generate_lfr_graph, load_edgelist, GraphData, Leiden, LeidenConfig,
    LfrConfig, LfrGraph, Partition, QualityType,
};

pub fn run_leiden(data: &GraphData, seed: u64) -> Partition {
    Leiden::new(LeidenConfig {
        seed: Some(seed),
        ..Default::default()
    })
    .run(data)
    .unwrap()
    .partition
}

pub fn run_leiden_cpm(data: &GraphData, seed: u64, resolution: f64) -> Partition {
    Leiden::new(LeidenConfig {
        seed: Some(seed),
        quality: QualityType::CPM,
        resolution,
        ..Default::default()
    })
    .run(data)
    .unwrap()
    .partition
}

pub fn lfr_to_graph_data(lfr: &LfrGraph) -> GraphData {
    let mut builder = leiden_rs::graph::GraphDataBuilder::new(lfr.node_count);
    for &(src, dst, w) in &lfr.edges {
        if src < lfr.node_count && dst < lfr.node_count && src != dst {
            builder.add_edge(src, dst, w).unwrap();
        }
    }
    builder.build().unwrap()
}

pub fn run_lfr_leiden(config: LfrConfig) -> (LfrGraph, GraphData, Partition) {
    let lfr = generate_lfr_graph(config).unwrap();
    let data = lfr_to_graph_data(&lfr);
    let partition = run_leiden(&data, 42);
    (lfr, data, partition)
}
