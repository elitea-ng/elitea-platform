//! The communities stage against the Python engine: `two_clusters.json`
//! and what `CommunityAnalyzer` (real igraph) produced for it
//! (`two_clusters.expected.json`, written by `fixtures/communities/generate.py`).

use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::stream::StopSignal;
use elitea_inventory_engine::communities::{detect, label_and_summarize_with};
use elitea_inventory_engine::extract::{Answer, Tuning};
use elitea_inventory_engine::graph::Graph;
use serde_json::{Value, json};
use std::sync::{Arc, Mutex};

const GRAPH: &str = include_str!("fixtures/communities/two_clusters.json");
const EXPECTED: &str = include_str!("fixtures/communities/two_clusters.expected.json");

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

fn expected() -> Value {
    serde_json::from_str(EXPECTED).unwrap()
}

/// `actual` equals `expected` with `modularity` compared within 1e-12.
fn assert_same_data(actual: &Value, expected: &Value) {
    let modularity = |value: &Value| value["modularity"].as_f64().unwrap();
    assert!(
        (modularity(actual) - modularity(expected)).abs() < 1e-12,
        "{} vs {}",
        modularity(actual),
        modularity(expected)
    );
    let mut actual = actual.clone();
    let mut expected = expected.clone();
    actual["modularity"] = json!(0);
    expected["modularity"] = json!(0);
    assert_eq!(actual, expected);
}

#[test]
fn two_dense_clusters_come_out_as_python_found_them() {
    let graph = load(GRAPH);
    let expected = expected();
    let Some(data) = detect(&graph, 1.0) else {
        panic!("no communities");
    };
    assert_same_data(&data, &expected["detected"]);
    // The spot checks a reader looks for.
    assert_eq!(data["num_communities"], 2);
    assert_eq!(data["communities"]["community_0"]["members"][0], "a_cls");
    assert_eq!(
        data["communities"]["community_0"]["centroids"][0],
        json!({"id": "a_cls", "score": 1.0, "name": "AuthService", "type": "class"})
    );
    assert_eq!(
        data["communities"]["community_1"]["label"],
        "Documentation: Single sign-on, README.md"
    );
    // igraph's modularity is networkx's too.
    let networkx = expected["networkx_modularity"].as_f64().unwrap();
    assert!((data["modularity"].as_f64().unwrap() - networkx).abs() < 1e-12);
}

#[test]
fn a_graph_under_ten_nodes_has_no_communities() {
    let graph = load(GRAPH);
    let mut small = Graph::new();
    for (id, node) in graph.nodes().take(9) {
        small.insert_node(id.to_owned(), node.clone());
    }
    small.insert_edge("a_cls", "a_login", serde_json::Map::new());
    assert!(detect(&small, 1.0).is_none());
}

/// A model that labels the `AuthService` community, fails every other
/// label, and summarises by echoing the label; it records every prompt.
fn scripted() -> (impl Fn(String) -> Answer, Arc<Mutex<Vec<String>>>) {
    let seen = Arc::new(Mutex::new(Vec::new()));
    let log = Arc::clone(&seen);
    let ask = move |prompt: String| -> Answer {
        log.lock().unwrap().push(prompt.clone());
        Box::pin(async move {
            if prompt.starts_with("Given this code community") {
                if prompt.contains("- AuthService (class)") {
                    Ok("  \"Authentication  &\n Token Handling.\"  ".to_owned())
                } else {
                    Err(EngineError::new(
                        ErrorType::Runtime,
                        "model down".to_owned(),
                    ))
                }
            } else {
                let label = prompt
                    .split_once("labeled '")
                    .and_then(|(_, rest)| rest.split_once("' in 5-8"))
                    .map(|(label, _)| label.to_owned())
                    .unwrap();
                Ok(format!("\n  Summary of {label}.  \n"))
            }
        })
    };
    (ask, seen)
}

#[tokio::test]
async fn labels_and_summaries_use_python_prompts_and_keep_the_heuristic_label_on_failure() {
    let graph = load(GRAPH);
    let expected = expected();
    let mut data = detect(&graph, 1.0).unwrap();
    let (ask, seen) = scripted();
    let tuning = Tuning {
        parallel_files: 1,
        ..Tuning::default()
    };
    let counts = label_and_summarize_with(ask, &mut data, &graph, tuning, &StopSignal::default())
        .await
        .unwrap();
    assert_eq!(counts, (1, 2));
    assert_same_data(&data, &expected["labelled"]);

    let prompts = seen.lock().unwrap().clone();
    assert_eq!(prompts.len(), 4);
    let python =
        |kind: &str, community: &str| expected[kind][community].as_str().unwrap().to_owned();
    // One call at a time, communities in order: labels, then summaries.
    assert_eq!(prompts[0], python("label_prompts", "community_0"));
    assert_eq!(prompts[1], python("label_prompts", "community_1"));
    assert_eq!(prompts[2], python("summary_prompts", "community_0"));
    assert_eq!(prompts[3], python("summary_prompts", "community_1"));
    assert!(prompts[2].contains("labeled 'Authentication & Token Handling'"));
}

#[tokio::test]
async fn parallel_calls_set_the_same_labels() {
    let graph = load(GRAPH);
    let mut data = detect(&graph, 1.0).unwrap();
    let (ask, seen) = scripted();
    let counts = label_and_summarize_with(
        ask,
        &mut data,
        &graph,
        Tuning::default(),
        &StopSignal::default(),
    )
    .await
    .unwrap();
    assert_eq!(counts, (1, 2));
    assert_same_data(&data, &expected()["labelled"]);
    assert_eq!(seen.lock().unwrap().len(), 4);
}

#[tokio::test]
async fn a_stop_cancels_and_nothing_without_communities_is_asked() {
    let graph = load(GRAPH);
    let mut data = detect(&graph, 1.0).unwrap();
    let before = data.clone();
    let stop = StopSignal::default();
    stop.request();
    let (ask, seen) = scripted();
    let result = label_and_summarize_with(ask, &mut data, &graph, Tuning::default(), &stop).await;
    assert_eq!(result, Err(EngineError::cancelled()));
    assert_eq!(data, before);
    assert!(seen.lock().unwrap().is_empty());

    // A cancelled call is a stop too.
    let cancelled = |_: String| -> Answer { Box::pin(async { Err(EngineError::cancelled()) }) };
    let result = label_and_summarize_with(
        cancelled,
        &mut data,
        &graph,
        Tuning::default(),
        &StopSignal::default(),
    )
    .await;
    assert_eq!(result, Err(EngineError::cancelled()));

    let (ask, seen) = scripted();
    let mut empty = json!({"communities": {}});
    let counts = label_and_summarize_with(
        ask,
        &mut empty,
        &graph,
        Tuning::default(),
        &StopSignal::default(),
    )
    .await
    .unwrap();
    assert_eq!(counts, (0, 0));
    assert!(seen.lock().unwrap().is_empty());
}
