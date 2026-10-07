use leiden_rs::{from_petgraph, Leiden, LeidenConfig};

fn main() {
    let mut graph = petgraph::Graph::<&str, f64>::new();
    let a = graph.add_node("Alice");
    let b = graph.add_node("Bob");
    let c = graph.add_node("Carol");
    let d = graph.add_node("Dave");
    let e = graph.add_node("Eve");

    graph.add_edge(a, b, 1.0);
    graph.add_edge(b, c, 1.0);
    graph.add_edge(a, c, 1.0);
    graph.add_edge(d, e, 1.0);

    let data = from_petgraph(&graph).expect("conversion failed");

    println!(
        "petgraph directed: {} nodes, is_directed={}",
        data.node_count(),
        data.is_directed()
    );

    let result = Leiden::new(LeidenConfig {
        seed: Some(42),
        ..Default::default()
    })
    .run(&data)
    .expect("leiden failed");

    println!(
        "Detected {} communities (quality: {:.4})",
        result.partition.num_communities(),
        result.quality
    );
    for node in graph.node_indices() {
        println!(
            "  {} → Community {}",
            graph[node],
            result.partition.community_of(node.index())
        );
    }

    let mut ugraph = petgraph::Graph::<&str, f64, petgraph::Undirected>::new_undirected();
    let n0 = ugraph.add_node("X");
    let n1 = ugraph.add_node("Y");
    let n2 = ugraph.add_node("Z");
    ugraph.add_edge(n0, n1, 2.0);
    ugraph.add_edge(n1, n2, 2.0);

    let udata = from_petgraph(&ugraph).expect("conversion failed");
    println!(
        "\npetgraph undirected: {} nodes, is_directed={}",
        udata.node_count(),
        udata.is_directed()
    );
}
