//! `smart_normalize_types` against the Python handler's own run
//! (`fixtures/smart_normalize/generate.py`): the prompt of every batch, the
//! structured-output tool, the mapping of a canned reply, the answer and
//! the types the saved graph holds — then through the native runner over a
//! socket, a mock gateway and PostgreSQL (needs `INVENTORY_TEST_DSN`).

mod common;

use elitea_engine_sidecar::server;
use elitea_inventory_engine::config::Settings;
use elitea_inventory_engine::graph::Graph;
use elitea_inventory_engine::native::NativeRunner;
use elitea_inventory_engine::retrieval::admin_tools::{
    self as admin, Applied, SmartStep, apply_mappings, graph_entity_types, mapping_tool,
    mappings_from_reply, merge_batch, smart_plan, smart_prompt, smart_report,
};
use elitea_inventory_engine::runner::Runner;
use elitea_inventory_engine::store::{self, GraphKey};
use indexmap::IndexMap;
use serde_json::{Map, Value, json};
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::OnceLock;
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;

const GRAPH: &str = include_str!("fixtures/smart_normalize/graph.json");
const GOLDENS: &str = include_str!("fixtures/smart_normalize/goldens.json");

fn goldens() -> &'static Value {
    static PARSED: OnceLock<Value> = OnceLock::new();
    PARSED.get_or_init(|| serde_json::from_str(GOLDENS).expect("goldens"))
}

fn graph() -> Graph {
    Graph::from_node_link(&serde_json::from_str(GRAPH).expect("graph json")).expect("node-link")
}

fn node_types(graph: &Graph) -> Value {
    Value::Object(
        graph
            .nodes()
            .map(|(id, node)| (id.to_owned(), node["type"].clone()))
            .collect(),
    )
}

#[test]
fn the_tool_and_the_canonical_list_are_pythons() {
    let golden = &goldens()["tool"]["function"];
    let (name, description, schema) = mapping_tool();
    assert_eq!(json!(name), golden["name"]);
    assert_eq!(json!(description), golden["description"]);
    assert_eq!(schema, golden["parameters"]);
    let mut ours = admin::tables().canonical_types.clone();
    ours.sort();
    assert_eq!(json!(ours), goldens()["canonical_types"]);
}

#[test]
fn every_case_answers_and_saves_what_python_did() {
    for case in goldens()["cases"].as_array().expect("cases") {
        let params = case["params"].as_object().expect("params");
        let mut graph = graph();
        let Ok(SmartStep::Map(plan)) = smart_plan(&graph, params) else {
            panic!("{case}: there are types to map");
        };
        let batches: Vec<Vec<String>> = plan.batches().collect();
        let prompts = case["prompts"].as_array().expect("prompts");
        assert_eq!(batches.len(), prompts.len(), "{params:?}");
        let mut mappings = IndexMap::new();
        let mut failed = false;
        for (index, batch) in batches.iter().enumerate() {
            assert_eq!(
                json!(smart_prompt(batch)),
                prompts[index],
                "{params:?} batch {index}"
            );
            match mappings_from_reply(&case["replies"][index]) {
                Ok(pairs) => merge_batch(&mut mappings, batch, pairs),
                Err(_) => failed = true,
            }
        }
        if failed {
            // Python mapped the failed batch's types to `fact` and saved;
            // the port refuses the run (native: nothing is written).
            assert!(case["types_after"].is_object(), "Python saved");
            continue;
        }
        if plan.dry_run {
            assert_eq!(
                json!(smart_report(&plan, &mappings, None, "mapper-model")),
                case["result"],
                "{params:?}"
            );
            assert!(case["types_after"].is_null(), "a dry run saves nothing");
            continue;
        }
        let entities_normalized = apply_mappings(&mut graph, &mappings);
        let applied = Applied {
            types_after: graph_entity_types(&graph).len(),
            entities_normalized,
        };
        assert_eq!(
            json!(smart_report(
                &plan,
                &mappings,
                Some(applied),
                "mapper-model"
            )),
            case["result"],
            "{params:?}"
        );
        assert_eq!(node_types(&graph), case["types_after"], "{params:?}");
        assert_eq!(case["model_config"]["temperature"], json!(0.0));
        assert_eq!(case["model_config"]["max_tokens"], json!(4096));
    }
}

#[test]
fn replies_are_validated_and_only_asked_types_are_mapped() {
    for (reply, needle) in [
        (json!({}), "no `mappings` list"),
        (json!({"mappings": [{"original": "a"}]}), "`confidence`"),
        (
            json!({"mappings": [{"original": "a", "canonical": "fact", "confidence": 2}]}),
            "`confidence`",
        ),
        (
            json!({"mappings": [{"canonical": "fact", "confidence": 1}]}),
            "string `original`",
        ),
    ] {
        let refused = mappings_from_reply(&reply);
        assert!(
            refused.as_ref().is_err_and(|e| e.contains(needle)),
            "{reply}: {refused:?}"
        );
    }
    // Python applied any `original` the model named: `class` -> `fact`
    // would have rewritten every class. Only the batch's types map here.
    let mut mappings = IndexMap::new();
    merge_batch(
        &mut mappings,
        &["widget".to_owned()],
        vec![
            ("class".to_owned(), "fact".to_owned()),
            ("widget".to_owned(), "gadget".to_owned()),
        ],
    );
    assert_eq!(
        mappings.into_iter().collect::<Vec<_>>(),
        [("widget".to_owned(), "fact".to_owned())]
    );
}

#[test]
fn parameters_are_read_as_python_read_them() {
    let graph = graph();
    let plan = |params: Value| smart_plan(&graph, params.as_object().expect("object"));
    // Every type at or above the threshold: Python's answer, no model.
    let Ok(SmartStep::Answer(text)) = plan(json!({"threshold": 1, "output_format": "text"})) else {
        panic!("answered");
    };
    assert_eq!(
        text,
        "No types to normalize (all types either have count >= 1 or are already canonical)"
    );
    assert!(
        plan(json!({"batch_size": 0}))
            .is_err_and(|e| e.message.contains("batch_size must be a positive integer"))
    );
    assert!(plan(json!({"threshold": "x"})).is_err());
    let Ok(SmartStep::Map(dry)) = plan(json!({"dry_run": true})) else {
        panic!("a plan");
    };
    assert!(dry.dry_run, "str(True).lower() == 'true'");
    let Ok(SmartStep::Map(wet)) = plan(json!({"dry_run": "yes"})) else {
        panic!("a plan");
    };
    assert!(!wet.dry_run);
}

/// The mock gateway's model: the canned mapping of the golden's first case,
/// for the types the prompt lists (answered as JSON text, not a tool call:
/// the runner reads both).
fn mapper(prompt: &str) -> String {
    let known: HashMap<String, Value> = goldens()["cases"][0]["replies"][0]["mappings"]
        .as_array()
        .expect("mappings")
        .iter()
        .map(|m| {
            (
                m["original"].as_str().expect("original").to_owned(),
                m.clone(),
            )
        })
        .collect();
    let listed = prompt
        .split_once("TYPES TO NORMALIZE:\n")
        .and_then(|(_, rest)| rest.split_once("\n\nMap each type"))
        .map_or("[]", |(types, _)| types);
    let types: Vec<String> = serde_json::from_str(listed).unwrap_or_default();
    let mappings: Vec<&Value> = types.iter().filter_map(|t| known.get(t)).collect();
    format!("```json\n{}\n```", json!({ "mappings": mappings }))
}

async fn invoke(socket: &PathBuf, tool: &str, arguments: &Value) -> Value {
    let mut stream = UnixStream::connect(socket).await.expect("connect");
    let payload =
        json!({"invocation_id": format!("i-{tool}"), "tool": tool, "arguments": arguments})
            .to_string();
    let request = format!(
        "POST /engine/invoke HTTP/1.1\r\nhost: engine\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{payload}",
        payload.len()
    );
    stream.write_all(request.as_bytes()).await.expect("write");
    let mut raw = Vec::new();
    tokio::time::timeout(Duration::from_mins(1), stream.read_to_end(&mut raw))
        .await
        .expect("an answer in time")
        .expect("read");
    String::from_utf8_lossy(&raw)
        .lines()
        .filter(|line| line.starts_with('{'))
        .map(|line| serde_json::from_str::<Value>(line).expect("NDJSON"))
        .next_back()
        .unwrap_or_default()
}

#[tokio::test]
async fn the_native_runner_maps_through_the_model_and_writes_the_store() {
    let Some(pool) = common::database("smart_normalize").await else {
        return;
    };
    let key = GraphKey::new(9, 90).expect("key");
    let first = store::save(&pool, key, &graph()).await.expect("save");
    let gateway = common::gateway(mapper).await;
    let env: HashMap<&str, String> = [
        ("ELITEA_INVENTORY_RUNNER", "native".to_owned()),
        (
            "ELITEA_INVENTORY_DATABASE_URL",
            common::database_url("smart_normalize"),
        ),
    ]
    .into_iter()
    .collect();
    let settings = Settings::from_lookup(|name| env.get(name).cloned()).expect("settings");
    let runner = Runner::Native(NativeRunner::new(settings).expect("runner"));
    let socket = PathBuf::from(format!("/tmp/ismart-{}/e.sock", std::process::id()));
    let listener = server::bind(&socket).expect("bind");
    tokio::spawn(server::serve(
        listener,
        runner,
        std::future::pending::<()>(),
    ));
    let arguments = |params: Value| {
        let mut base = Map::new();
        base.insert("family".to_owned(), json!("inventory"));
        base.insert("project_id".to_owned(), json!(9));
        base.insert("application_id".to_owned(), json!(90));
        let mut params = params.as_object().cloned().unwrap_or_default();
        params.insert(
            "llm_settings".to_owned(),
            json!({"api_base": gateway, "api_key": "k", "organization": "9"}),
        );
        params.insert("llm_model".to_owned(), json!("mapper-model"));
        base.insert("params".to_owned(), Value::Object(params));
        Value::Object(base)
    };
    let case = &goldens()["cases"][0];

    // A dry run answers the preview and writes nothing.
    let dry = invoke(
        &socket,
        "smart_normalize_types",
        &arguments(json!({"dry_run": "true"})),
    )
    .await;
    assert_eq!(
        dry["result"]["result"],
        goldens()["cases"][2]["result"],
        "{dry}"
    );
    assert_eq!(
        store::revision(&pool, key).await.expect("revision"),
        Some(first)
    );

    let applied = invoke(&socket, "smart_normalize_types", &arguments(json!({}))).await;
    assert_eq!(applied["result"]["success"], json!(true), "{applied}");
    assert_eq!(applied["result"]["result"], case["result"]);
    let (stored, revision) = store::load(&pool, key)
        .await
        .expect("load")
        .expect("a graph");
    assert_eq!(revision, first + 1);
    assert_eq!(node_types(&stored), case["types_after"]);

    // Nothing left to map: Python's answer, and no model call needed.
    let again = invoke(
        &socket,
        "smart_normalize_types",
        &arguments(json!({"output_format": "text"})),
    )
    .await;
    assert_eq!(
        again["result"]["result"],
        json!(
            "No types to normalize (all types either have count >= 1000 or are already canonical)"
        )
    );

    // No model configured: refused before the model is asked.
    let mut no_model = arguments(json!({}));
    no_model["params"]
        .as_object_mut()
        .map(|p| p.remove("llm_model"));
    store::save(&pool, key, &graph()).await.expect("save");
    let refused = invoke(&socket, "smart_normalize_types", &no_model).await;
    assert!(
        refused["error"]["message"]
            .as_str()
            .is_some_and(|m| m.starts_with("no LLM model is configured")),
        "{refused}"
    );
}
