use super::*;

const CENTRALITY: &str = include_str!("../../tests/fixtures/communities/centrality.json");
const CENTRALITY_EXPECTED: &str =
    include_str!("../../tests/fixtures/communities/centrality.expected.json");

fn load(text: &str) -> Graph {
    let document: Value = serde_json::from_str(text).unwrap();
    let mut graph = Graph::new();
    for node in document["nodes"].as_array().unwrap() {
        graph.insert_node(
            node["id"].as_str().unwrap().to_owned(),
            node["attributes"].as_object().unwrap().clone(),
        );
    }
    for edge in document["edges"].as_array().unwrap() {
        graph.insert_edge(
            edge["source"].as_str().unwrap(),
            edge["target"].as_str().unwrap(),
            edge["attributes"].as_object().unwrap().clone(),
        );
    }
    graph
}

fn close(actual: f64, expected: f64, tolerance: f64) -> bool {
    (actual - expected).abs() <= tolerance
}

/// The centrality fixture as igraph sees it, and the node ids by vertex.
fn centrality_graph() -> (Undirected, Vec<String>) {
    let graph = load(CENTRALITY);
    let index: IndexMap<&str, usize> = graph
        .nodes()
        .enumerate()
        .map(|(position, (id, _))| (id, position))
        .collect();
    let ids = index.keys().map(|id| (*id).to_owned()).collect();
    (undirected(&graph, &index), ids)
}

#[test]
fn the_multigraph_keeps_one_edge_per_directed_edge_weighted_by_relation() {
    let (multigraph, _) = centrality_graph();
    assert_eq!(multigraph.vertices, 8);
    // p→q contains, q→p calls: two parallel edges, 3 and 2.
    assert_eq!(multigraph.edges[0], (0, 1, 3.0));
    assert_eq!(multigraph.edges.len(), 10);
    let weights: Vec<f64> = multigraph.edges.iter().map(|edge| edge.2).collect();
    // p→s imports, s→t extends, t→u (unknown), u→s documents.
    assert!(weights.contains(&1.0) && weights.contains(&3.0) && weights.contains(&2.0));
}

#[test]
fn pagerank_strength_and_betweenness_match_igraph() {
    let expected: Value = serde_json::from_str(CENTRALITY_EXPECTED).unwrap();
    let (multigraph, ids) = centrality_graph();
    let pagerank = multigraph.pagerank();
    let betweenness = multigraph.betweenness();
    let strength = multigraph.strength();
    for (vertex, id) in ids.iter().enumerate() {
        let igraph = &expected["igraph"];
        let networkx = &expected["networkx"];
        assert!(
            close(
                pagerank[vertex],
                igraph["pagerank"][id].as_f64().unwrap(),
                1e-12
            ),
            "pagerank {id}: {}",
            pagerank[vertex]
        );
        // networkx agrees on PageRank (parallel edges summed either way)…
        assert!(close(
            pagerank[vertex],
            networkx["pagerank"][id].as_f64().unwrap(),
            1e-10
        ));
        assert!(
            close(
                betweenness[vertex],
                igraph["betweenness"][id].as_f64().unwrap(),
                1e-12
            ),
            "betweenness {id}: {}",
            betweenness[vertex]
        );
        assert!(close(
            strength[vertex],
            igraph["strength"][id].as_f64().unwrap(),
            0.0
        ));
    }
    // …but not on betweenness: igraph counts a path once per parallel edge
    // (r–t is two edges), networkx collapses them. igraph is what Python ran.
    let s = ids.iter().position(|id| id == "s").unwrap();
    assert!(close(betweenness[s], 10.0 / 3.0, 1e-12));
    assert!(close(
        expected["networkx"]["betweenness"]["s"].as_f64().unwrap(),
        3.5,
        1e-12
    ));
}

#[test]
fn modularity_matches_igraph_at_any_resolution() {
    let expected: Value = serde_json::from_str(CENTRALITY_EXPECTED).unwrap();
    let (multigraph, ids) = centrality_graph();
    let membership: Vec<usize> = ids
        .iter()
        .map(|id| {
            usize::try_from(expected["modularity"]["membership"][id].as_u64().unwrap()).unwrap()
        })
        .collect();
    let q = multigraph.modularity(&membership, 1.0);
    assert!(
        close(q, expected["modularity"]["igraph"].as_f64().unwrap(), 1e-12),
        "{q}"
    );
    let q = multigraph.modularity(&membership, 0.5);
    assert!(close(
        q,
        expected["modularity"]["igraph_resolution_0_5"]
            .as_f64()
            .unwrap(),
        1e-12
    ));
    // Without edges there is no modularity (igraph's NaN).
    assert!(
        Undirected {
            vertices: 2,
            edges: Vec::new()
        }
        .modularity(&[0, 1], 1.0)
        .is_nan()
    );
}

#[test]
fn a_self_loop_counts_twice_in_strength_and_pagerank() {
    // Probed against igraph 1.0: loop 0–0 (3), 0–1 (1), 1–2 (2).
    let multigraph = Undirected {
        vertices: 3,
        edges: vec![(0, 0, 3.0), (0, 1, 1.0), (1, 2, 2.0)],
    };
    assert_eq!(multigraph.strength(), vec![7.0, 3.0, 2.0]);
    let pagerank = multigraph.pagerank();
    for (actual, expected) in pagerank.iter().zip([
        0.490_423_387_096_774_2,
        0.293_346_774_193_548_4,
        0.216_229_838_709_677_4,
    ]) {
        assert!(close(*actual, expected, 1e-12), "{pagerank:?}");
    }
    let q = multigraph.modularity(&[0, 0, 1], 1.0);
    assert!(close(q, -0.055_555_555_555_555_47, 1e-12));
}

#[test]
fn relation_types_weigh_by_category() {
    assert!(close(edge_weight("contains"), 3.0, 0.0));
    assert!(close(edge_weight(" Calls "), 2.0, 0.0));
    assert!(close(edge_weight("mentions"), 1.0, 0.0));
    assert!(close(edge_weight("frobnicates"), 1.0, 0.0));
    assert!(close(edge_weight(""), 1.0, 0.0));
}

#[test]
fn scores_normalise_and_round_as_python() {
    assert_eq!(normalize(&[2.0, 4.0, 3.0]), vec![0.0, 1.0, 0.5]);
    assert_eq!(normalize(&[5.0, 5.0, 5.0, 5.0]), vec![0.25; 4]);
    assert!(close(round4(0.424_449_9), 0.4244, 0.0));
    // 1/32 is exact: a true tie, to even.
    assert!(close(round4(0.031_25), 0.0312, 0.0));
    assert!(close(round4(0.093_75), 0.0938, 0.0));
}

fn node(name: &str, entity_type: &str, layer: &str) -> Map<String, Value> {
    let Value::Object(fields) = json!({"name": name, "type": entity_type, "layer": layer}) else {
        unreachable!()
    };
    fields
}

#[test]
fn the_auto_label_names_a_dominant_type_then_documentation_then_the_top_centroid() {
    let mut graph = Graph::new();
    for (id, entity_type, layer) in [
        ("f1", "function", "code"),
        ("f2", "function", "code"),
        ("f3", "function", "code"),
        ("c1", "class", "code"),
        ("v1", "variable", "code"),
        ("d1", "concept", "documentation"),
        ("d2", "fact", "documentation"),
    ] {
        graph.insert_node(id.to_owned(), node(&id.to_uppercase(), entity_type, layer));
    }
    let centroid =
        |id: &str| json!({"id": id, "score": 1.0, "name": id.to_uppercase(), "type": "x"});
    let label = |members: &[&str], centroids: &[Value]| {
        let types = dominant_types(&graph, members);
        let layers = dominant_layers(&graph, members);
        auto_label(&graph, members, centroids, &types, &layers)
    };
    // Every architectural member is a function (≥ 60 %).
    assert_eq!(
        label(
            &["f1", "f2", "f3", "v1"],
            &[
                centroid("f2"),
                centroid("f1"),
                centroid("v1"),
                centroid("f3")
            ]
        ),
        "Function cluster: F2, F1, V1"
    );
    // Priority first, then count: one class outranks three functions, and
    // holds 1 of 4 architectural members, so no type dominates.
    let members = ["f1", "f2", "f3", "c1", "v1"];
    let types = dominant_types(&graph, &members);
    assert_eq!(
        types,
        vec![("class".to_owned(), 1), ("function".to_owned(), 3)]
    );
    assert_eq!(
        label(&members, &[centroid("f1"), centroid("c1")]),
        "F1 & related (class)"
    );
    // No architectural member: noise types, and the documentation layer.
    assert_eq!(
        label(&["d1", "d2", "v1"], &[centroid("d1"), centroid("d2")]),
        "Documentation: D1, D2"
    );
    assert_eq!(
        dominant_types(&graph, &["d1", "d2", "v1"]),
        vec![
            ("variable".to_owned(), 1),
            ("concept".to_owned(), 1),
            ("fact".to_owned(), 1)
        ]
    );
    // One class, one function: neither dominates; code layer.
    assert_eq!(
        label(&["c1", "f1"], &[centroid("c1")]),
        "C1 & related (class)"
    );
    assert_eq!(label(&["c1"], &[]), "Empty community");
}

#[test]
fn labels_are_cleaned_as_python_cleans_them() {
    use super::labels::clean_label_for_tests as clean;
    assert_eq!(clean("  \"Auth  &\n Tokens.\"  "), "Auth & Tokens");
    assert_eq!(clean("'Request Handling!?'"), "Request Handling");
    assert_eq!(clean("Data (pipeline)"), "Data (pipeline");
    assert_eq!(clean("...\"\""), "");
    let long = "word ".repeat(30);
    assert_eq!(clean(&long), "word ".repeat(16));
}
