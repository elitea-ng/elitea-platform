//! The pattern, semantic, community and admin tools against the Python
//! engine's own answers.
//!
//! `fixtures/retrieval_more/generate.py` builds `graph.json` (and the bare
//! `bare.json`) with the Python `KnowledgeGraph`, loads them back as
//! production did, and records in `goldens.json` what the real Python code
//! answered (see the generator for the two things it arranges: start-set
//! order, and the chat closures around the wrapper methods).

mod common;

use elitea_inventory_engine::graph::Graph;
use elitea_inventory_engine::store::{self, GraphKey, vectors};
use elitea_inventory_engine::retrieval::view::GraphView;
use elitea_inventory_engine::retrieval::{
    Call, admin_tools, community_tools, dispatch, pattern, semantic,
};
use serde_json::{Map, Value, json};

const GRAPH: &str = include_str!("fixtures/retrieval_more/graph.json");
const BARE: &str = include_str!("fixtures/retrieval_more/bare.json");
const GOLDENS: &str = include_str!("fixtures/retrieval_more/goldens.json");

fn view_of(document: &str) -> GraphView {
    let document: Value = serde_json::from_str(document).expect("graph json");
    GraphView::new(Graph::from_node_link(&document).expect("node-link"), 1)
}

/// `GRAPH` saved to a fresh database (`None` without one), to rank in
/// PostgreSQL.
async fn stored(name: &str) -> Option<(sqlx::PgPool, GraphKey)> {
    let pool = common::database(name).await?;
    let key = GraphKey::new(1, 1).expect("key");
    let document: Value = serde_json::from_str(GRAPH).expect("graph json");
    store::save(&pool, key, &Graph::from_node_link(&document).expect("node-link"))
        .await
        .expect("save");
    Some((pool, key))
}

async fn rank(pool: &sqlx::PgPool, key: GraphKey, vector: &[f64], min_score: f64) -> vectors::Ranking {
    vectors::rank(pool, key, vector, min_score).await.expect("rank")
}

fn goldens() -> Value {
    serde_json::from_str(GOLDENS).expect("goldens")
}

fn text(value: &Value) -> &str {
    value.as_str().expect("a string")
}

#[test]
fn query_pattern_answers_as_the_python_chat_tool() {
    let view = view_of(GRAPH);
    let goldens = goldens();
    let cases = goldens["patterns"].as_array().expect("patterns");
    assert!(cases.len() > 40);
    for case in cases {
        let pattern = text(&case["pattern"]);
        assert_eq!(
            pattern::query_pattern(&view, pattern),
            text(&case["text"]),
            "pattern {pattern:?}"
        );
    }
}

#[test]
fn find_paths_reports_the_value_error() {
    let view = view_of(GRAPH);
    let paths = pattern::find_paths(&view, "(UserService)-[:calls]->(?)", 10).expect("paths");
    assert_eq!(paths.len(), 1);
    assert_eq!(paths[0].path, ["n1", "n3"]);
    assert_eq!(paths[0].edges, ["calls"]);
    let refused = pattern::find_paths(&view, "(A)-[:x*0..1]->(B)", 10);
    assert_eq!(refused, Err("Minimum hops must be >= 1, got 0".to_owned()));
}

#[test]
fn the_pattern_vocabulary_answers_as_the_python_chat_tool() {
    let goldens = goldens();
    assert_eq!(
        pattern::pattern_vocabulary(&view_of(GRAPH)),
        text(&goldens["vocabulary"])
    );
    assert_eq!(
        pattern::pattern_vocabulary(&view_of(BARE)),
        text(&goldens["vocabulary_bare"])
    );
}

fn opt<'a>(case: &'a Value, key: &str) -> Option<&'a str> {
    case.get(key).and_then(Value::as_str)
}

#[tokio::test]
async fn semantic_search_answers_as_the_python_wrapper() {
    let Some((pool, key)) = stored("semantic_goldens").await else {
        return;
    };
    let view = view_of(GRAPH);
    let goldens = goldens();
    for case in goldens["semantic"].as_array().expect("semantic") {
        let vector: Vec<f64> = case["vector"]
            .as_array()
            .expect("vector")
            .iter()
            .map(|v| v.as_f64().expect("a float"))
            .collect();
        let query = semantic::SemanticQuery {
            query: text(&case["query"]),
            top_k: case
                .get("top_k")
                .and_then(Value::as_u64)
                .map_or(10, |k| usize::try_from(k).expect("top_k")),
            entity_type: opt(case, "entity_type"),
            layer: opt(case, "layer"),
            file_pattern: opt(case, "file_pattern"),
            min_score: case
                .get("min_score")
                .and_then(Value::as_f64)
                .unwrap_or(semantic::DEFAULT_MIN_SCORE),
        };
        let ranking = rank(&pool, key, &vector, query.min_score).await;
        assert_eq!(
            semantic::semantic_search_text(&view, &query, &ranking),
            text(&case["text"]),
            "case {case}"
        );
    }
    let bare = view_of(BARE);
    let query = semantic::SemanticQuery {
        query: "auth",
        top_k: 10,
        entity_type: None,
        layer: None,
        file_pattern: None,
        min_score: semantic::DEFAULT_MIN_SCORE,
    };
    assert_eq!(
        semantic::semantic_search_text(&bare, &query, &Ok(Vec::new())),
        text(&goldens["semantic_unavailable"])
    );
}

#[tokio::test]
async fn the_semantic_tool_checks_the_space_and_caps_top_k() {
    let Some((pool, key)) = stored("semantic_tool").await else {
        return;
    };
    let view = view_of(GRAPH);
    assert_eq!(semantic::stamped_model(&view), Some("text-embed-x"));
    let ranking = rank(&pool, key, &[1.0, 0.0, 0.0, 0.0], semantic::DEFAULT_MIN_SCORE).await;
    let refused = semantic::semantic_search_tool(
        &view,
        "auth",
        &ranking,
        10,
        None,
        None,
        Some("all-MiniLM-L6-v2"),
    );
    assert!(refused.is_err());
    let answered = semantic::semantic_search_tool(
        &view,
        "auth",
        &ranking,
        500,
        None,
        None,
        Some("text-embed-x"),
    )
    .expect("same space");
    assert!(answered.starts_with("# Semantic Search Results for 'auth'"));
    let everything = rank(&pool, key, &[1.0, 0.0, 0.0, 0.0], 0.0).await;
    let hits = semantic::semantic_search(
        &view,
        &semantic::SemanticQuery {
            query: "auth",
            top_k: 2,
            entity_type: None,
            layer: None,
            file_pattern: None,
            min_score: 0.0,
        },
        &everything,
    )
    .expect("hits");
    assert_eq!(hits.len(), 2);
    assert_eq!(hits[0].id, "n3");

    // Another width than the graph's is numpy's error, and the zero vector
    // ranks nothing.
    let wider = rank(&pool, key, &[1.0, 0.0, 0.0, 0.0, 0.0], 0.0).await;
    assert_eq!(
        wider,
        Err("shapes (5,) and (4,) not aligned: 5 (dim 0) != 4 (dim 0)".to_owned())
    );
    assert_eq!(rank(&pool, key, &[0.0; 4], 0.0).await, Ok(Vec::new()));
}

#[test]
fn the_community_tools_answer_as_the_python_wrapper() {
    let view = view_of(GRAPH);
    let bare = view_of(BARE);
    assert!(community_tools::has_communities(&view));
    assert!(!community_tools::has_communities(&bare));
    let goldens = goldens();
    let tools = &goldens["communities"];
    let pick = |case: &Value| {
        if case.get("bare").is_some() {
            &bare
        } else {
            &view
        }
    };
    for case in tools["list"].as_array().expect("list") {
        let top_n = case["top_n"]
            .as_u64()
            .map(|n| usize::try_from(n).expect("top_n"));
        assert_eq!(
            community_tools::list_communities(pick(case), top_n),
            text(&case["text"]),
            "list {case}"
        );
    }
    for case in tools["detail"].as_array().expect("detail") {
        assert_eq!(
            community_tools::get_community_detail(pick(case), text(&case["community_id"])),
            text(&case["text"]),
            "detail {case}"
        );
    }
    for case in tools["find"].as_array().expect("find") {
        assert_eq!(
            community_tools::find_entity_community(&view, text(&case["entity_name"])),
            text(&case["text"]),
            "find {case}"
        );
    }
    for case in tools["search"].as_array().expect("search") {
        assert_eq!(
            community_tools::search_within_community(
                &view,
                text(&case["community_id"]),
                text(&case["query"])
            ),
            text(&case["text"]),
            "search {case}"
        );
    }
}

/// Run `tool` through the dispatcher with `params`.
fn run(view: &GraphView, tool: &str, params: &Value) -> Option<Result<Value, String>> {
    let empty = Map::new();
    let params = params.as_object().unwrap_or(&empty);
    let call = Call {
        tool,
        family: "inventory",
        params,
        view,
    };
    dispatch(&call)
        .map(|handled| handled.map_err(|e| format!("{}: {}", e.error_type.wire_name(), e.message)))
}

/// The golden `{ok}` / `{error_type, message}` as the dispatcher's answer.
fn expected(golden: &Value) -> Result<Value, String> {
    match golden.get("ok") {
        Some(result) => Ok(json!({"success": true, "result": result})),
        None => Err(format!(
            "{}: {}",
            text(&golden["error_type"]),
            text(&golden["message"])
        )),
    }
}

#[test]
fn the_admin_tools_answer_as_the_python_handlers() {
    let view = view_of(GRAPH);
    let goldens = goldens();
    let admin = &goldens["admin"];
    assert_eq!(
        run(&view, "list_presets", &json!({})),
        Some(expected(&admin["list_presets"]))
    );
    for case in admin["get_preset_info"].as_array().expect("cases") {
        // The intended answer: the patterns the Python handler never found.
        let answer = run(&view, "get_preset_info", &case["params"]);
        assert_eq!(answer, Some(expected(&case["intended"])), "{case}");
        // ... which only ADDS to Python's.
        if let (Some(Ok(answer)), Some(python)) = (&answer, case["python"].get("ok")) {
            assert!(text(&answer["result"]).starts_with(text(python)));
        }
    }
    for tool in ["cleanup_cache", "normalize_types", "smart_normalize_types"] {
        for case in admin[tool].as_array().expect("cases") {
            assert_eq!(
                run(&view, tool, &case["params"]),
                Some(expected(&case["result"])),
                "{tool} {case}"
            );
        }
    }
}

#[test]
fn rebuild_indices_reports_the_counts_python_lost() {
    let view = view_of(GRAPH);
    let goldens = goldens();
    let cases = goldens["admin"]["rebuild_indices"]
        .as_array()
        .expect("cases");
    // JSON: Python's answer but for the two counts it read from keys that
    // do not exist.
    let python: Value = serde_json::from_str(text(&cases[0]["result"]["ok"])).expect("python json");
    let answer = run(&view, "rebuild_indices", &cases[0]["params"])
        .expect("served")
        .expect("answered");
    let mut ours: Value = serde_json::from_str(text(&answer["result"])).expect("json");
    assert_eq!(ours["entity_count"], json!(17));
    assert_eq!(ours["relation_count"], json!(22));
    ours["entity_count"] = json!(0);
    ours["relation_count"] = json!(0);
    assert_eq!(ours, python);
    let answer = run(&view, "rebuild_indices", &cases[1]["params"])
        .expect("served")
        .expect("answered");
    assert_eq!(
        text(&answer["result"]),
        text(&cases[1]["result"]["ok"])
            .replace("Entities: 0", "Entities: 17")
            .replace("Relations: 0", "Relations: 22")
    );
}

#[test]
fn the_cache_tools_report_no_file_cache() {
    let view = view_of(GRAPH);
    let answer = run(&view, "get_cache_stats", &json!({}))
        .expect("served")
        .expect("answered");
    assert_eq!(
        text(&answer["result"]),
        format!(
            "# Graph Cache Statistics\n\n**Total Graphs:** 0\n**Total Size:** 0.0 MB\n\n{}\n",
            admin_tools::NO_CACHE_NOTE
        )
    );
    let answer = run(&view, "get_cache_stats", &json!({"output_format": "json"}))
        .expect("served")
        .expect("answered");
    let stats: Value = serde_json::from_str(text(&answer["result"])).expect("json");
    assert_eq!(stats["stats"]["total_graphs"], json!(0));
    assert_eq!(stats["graphs"], json!([]));
}

#[test]
fn maintenance_that_needs_a_model_is_refused_or_reported_skipped() {
    let view = view_of(GRAPH);
    // `class` (5 entities) is canonical; nothing qualifies under 1000 only
    // when every type is canonical — add a non-canonical one.
    let mut graph = view.graph.clone();
    let mut node = Map::new();
    node.insert("name".to_owned(), json!("odd"));
    node.insert("type".to_owned(), json!("gizmo"));
    graph.insert_node("x1".to_owned(), node);
    let odd = GraphView::new(graph, 2);
    let refused = run(&odd, "smart_normalize_types", &json!({}));
    assert!(
        matches!(refused, Some(Err(message)) if message.starts_with("RuntimeError: smart_normalize_types would map 1 entity type(s) through a model (gizmo)"))
    );
    let skipped = run(&odd, "normalize_types", &json!({"smart_threshold": 3}))
        .expect("served")
        .expect("answered");
    let report: Value = serde_json::from_str(text(&skipped["result"])).expect("json");
    assert_eq!(report["smart_normalization_ran"], json!(false));
    assert!(report["smart_normalization_skipped"].is_string());
    let falsy = run(&odd, "normalize_types", &json!({"smart": 0}))
        .expect("served")
        .expect("answered");
    let report: Value = serde_json::from_str(text(&falsy["result"])).expect("json");
    assert_eq!(report["smart_normalization_ran"], json!(0));
    assert!(report.get("smart_normalization_skipped").is_none());
}

#[test]
fn only_the_routed_admin_tools_are_served_here() {
    let view = view_of(GRAPH);
    for tool in [
        "remove_source_entities",
        "query_pattern",
        "semantic_search",
        "list_communities",
    ] {
        let call = Call {
            tool,
            family: "inventory",
            params: &Map::new(),
            view: &view,
        };
        assert!(
            admin_tools::handle(&call).is_none()
                && pattern::handle(&call).is_none()
                && semantic::handle(&call).is_none()
                && community_tools::handle(&call).is_none(),
            "{tool}"
        );
    }
}
