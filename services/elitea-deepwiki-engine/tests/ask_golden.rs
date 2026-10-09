//! The ADR-0026 phase 6 parity gate, without Python or a network.
//!
//! `tests/fixtures/ask/ref` is the Python engine's run of the scripted
//! conversation in `tests/fixtures/ask/script.json` against
//! `services/elitea-deepwiki-engine/testdata/llm_stub.py` (see
//! `parity/python_ask_dump.py`). Here the Rust agents run the same script
//! over the replay index of the same rows, and:
//!
//! * every request body is the Python one, turn by turn (system and user
//!   messages, assistant tool calls, tool results, tool definitions,
//!   model, temperature, token budget, stream flag); the one field Python
//!   did not send is `stream_options` (usage on a stream);
//! * the progress lines and tokens are Python's (timestamps masked; the
//!   order of the results of one step, which Python's threads decided, is
//!   compared as a set);
//! * the results are Python's;
//! * every tool called directly gives Python's text, and every file-system
//!   call of the probe gives `deepagents`' text.

mod ask_common;

use ask_common as common;
use elitea_deepwiki_engine::ask::agent::{self, AgentSpec, Clock, Mode};
use elitea_deepwiki_engine::ask::embed::{Embedder, stand_in_embedding};
use elitea_deepwiki_engine::ask::summarize::Msg;
use elitea_deepwiki_engine::ask::tools::Codebase;
use elitea_deepwiki_engine::ask::{self, Limits, args, resolve};
use elitea_deepwiki_engine::runner::StopSignal;
use serde_json::{Map, Value, json};

fn clock() -> Clock {
    Clock::Fixed {
        date: "2026-01-02".into(),
        timestamp: "2026-01-02T03:04:05".into(),
    }
}

fn payload() -> Map<String, Value> {
    let script = common::json_file("ask/script.json");
    let value = json!({
        "question": script["question"],
        "llm_settings": {"api_base": "http://127.0.0.1:9/v1", "api_key": "stub-key", "model_name": "gpt-4o"},
        "repo_config": {"provider_type": "github", "repository": "acme/notes", "branch": "main",
                        "provider_config": {}, "project": null},
        "chat_history": [],
        "k": 15,
        "repo_identifier_override": "acme/notes:main:01234567",
    });
    value.as_object().cloned().unwrap_or_default()
}

/// Python's request bodies, and ours without `stream_options`.
fn compare_bodies(ours: &[Value], tool: &str) {
    let python = common::jsonl(&format!("ask/ref/{tool}/requests.jsonl"));
    assert_eq!(ours.len(), python.len(), "{tool}: number of model calls");
    for (turn, (ours, python)) in ours.iter().zip(&python).enumerate() {
        let mut ours = ours.clone();
        if let Some(map) = ours.as_object_mut() {
            map.remove("stream_options");
        }
        for key in [
            "model",
            "temperature",
            "max_completion_tokens",
            "stream",
            "tools",
        ] {
            assert_eq!(
                ours[key], python[key],
                "{tool}: request {turn}, field {key}"
            );
        }
        let (a, b) = (
            ours["messages"].as_array().cloned().unwrap_or_default(),
            python["messages"].as_array().cloned().unwrap_or_default(),
        );
        for (index, (a, b)) in a.iter().zip(&b).enumerate() {
            assert_eq!(a, b, "{tool}: request {turn}, message {index}");
        }
        assert_eq!(a.len(), b.len(), "{tool}: request {turn}, message count");
        assert_eq!(
            &ours, python,
            "{tool}: request {turn} differs from the Python request"
        );
    }
}

/// Mask the timestamps of `ask` events.
fn masked(line: &Value) -> Value {
    let mut line = line.clone();
    if let Some(text) = line.get("thinking").and_then(Value::as_str)
        && let Ok(mut event) = serde_json::from_str::<Value>(text)
    {
        if let Some(map) = event.as_object_mut()
            && map.contains_key("timestamp")
        {
            map.insert("timestamp".into(), json!("T"));
        }
        line = json!({"thinking": event});
    }
    line
}

/// Sort the runs of consecutive result lines (Python's threads ordered
/// them).
fn settle(lines: &[Value], is_result: impl Fn(&Value) -> bool) -> Vec<Value> {
    let mut out: Vec<Value> = Vec::new();
    let mut run: Vec<Value> = Vec::new();
    for line in lines {
        if is_result(line) {
            run.push(line.clone());
            continue;
        }
        run.sort_by_key(ToString::to_string);
        out.append(&mut run);
        out.push(line.clone());
    }
    run.sort_by_key(ToString::to_string);
    out.append(&mut run);
    out
}

fn ask_result_line(line: &Value) -> bool {
    let text = line["thinking"].to_string();
    text.contains("tool_result") || text.contains("tool_end")
}

fn research_result_line(line: &Value) -> bool {
    line["thinking"]
        .as_str()
        .is_some_and(|t| t.starts_with('\u{2713}'))
}

fn mask_steps(value: &mut Value) {
    if let Some(steps) = value
        .get_mut("thinking_steps")
        .and_then(Value::as_array_mut)
    {
        for step in steps.iter_mut() {
            if let Some(map) = step.as_object_mut() {
                map.insert("timestamp".into(), json!("T"));
                // A result's step number is its completion order among
                // Python's threads.
                if map.get("type") == Some(&json!("tool_result")) {
                    map.insert("step".into(), json!(0));
                }
            }
        }
        steps.sort_by_key(|s| s["type"].to_string() + &s["call_id"].to_string());
    }
}

#[tokio::test]
async fn ask_matches_the_python_run() {
    let script = common::json_file("ask/script.json");
    let request = ask::parse_request(&payload()).expect("request");
    assert_eq!(request.wiki_id, "acme--notes--main");
    let spec = ask::ask_spec(
        &request,
        None,
        "gpt-4o",
        false,
        true,
        Limits::default(),
        clock(),
    )
    .expect("spec");
    let model = common::ScriptedModel::new(&script["ask"]);
    let index = common::replay_index();
    let embedder = Embedder::Fixed(stand_in_embedding);
    let (context, mut lines, _) = common::context();
    let mut result = ask::run_agent(&spec, &request, &model, &index, &embedder, &context)
        .await
        .expect("ask runs");
    compare_bodies(&model.bodies(), "ask");

    let ours: Vec<Value> = common::drain(&mut lines).iter().map(masked).collect();
    let python: Vec<Value> = common::jsonl("ask/ref/ask/lines.jsonl")
        .iter()
        .map(masked)
        .collect();
    assert_eq!(
        settle(&ours, ask_result_line),
        settle(&python, ask_result_line),
        "the ask lines differ"
    );
    let mut expected = common::json_file("ask/ref/ask/result.json");
    mask_steps(&mut result);
    mask_steps(&mut expected);
    assert_eq!(result, expected);
}

#[tokio::test]
async fn deep_research_matches_the_python_run() {
    let script = common::json_file("ask/script.json");
    let request = ask::parse_request(&payload()).expect("request");
    let spec = ask::research_spec(&request, None, "gpt-4o", false, Limits::default(), clock())
        .expect("spec");
    let model = common::ScriptedModel::new(&script["deep_research"]);
    let index = common::replay_index();
    let embedder = Embedder::Fixed(stand_in_embedding);
    let (context, mut lines, _) = common::context();
    let mut result = ask::run_agent(&spec, &request, &model, &index, &embedder, &context)
        .await
        .expect("deep research runs");
    compare_bodies(&model.bodies(), "deep_research");

    let ours = common::drain(&mut lines);
    // Python's worker also printed its cache selection (scratch paths):
    // a deliberate omission.
    let python: Vec<Value> = common::jsonl("ask/ref/deep_research/lines.jsonl")
        .into_iter()
        .filter(|l| {
            !l["thinking"]
                .as_str()
                .is_some_and(|t| t.starts_with("Cache Selection"))
        })
        .collect();
    assert!(ours.iter().all(|l| l.get("token").is_none()), "no tokens");
    assert_eq!(
        settle(&ours, research_result_line),
        settle(&python, research_result_line),
        "the deep research lines differ"
    );
    let mut expected = common::json_file("ask/ref/deep_research/result.json");
    mask_steps(&mut result);
    mask_steps(&mut expected);
    assert_eq!(result, expected);
}

#[tokio::test]
async fn every_tool_gives_the_python_text() {
    let index = common::replay_index();
    let embedder = Embedder::Fixed(stand_in_embedding);
    let stop = StopSignal::default();
    let codebase = Codebase {
        store: &index,
        embedder: &embedder,
        stop: &stop,
        doc_results: Limits::default().doc_results,
    };
    let mut compared = 0;
    for record in common::jsonl("ask/ref/tools.jsonl") {
        let name = record["name"].as_str().expect("name");
        let raw = record["arguments"].as_object().cloned().unwrap_or_default();
        let params = args::params(name).expect("known tool");
        let parsed = args::validate(params, &raw).expect("valid arguments");
        let text = codebase.run(name, &parsed).await.expect("runs");
        assert_eq!(
            text.as_deref(),
            record["result"].as_str(),
            "{name} {raw:?} differs from Python"
        );
        compared += 1;
    }
    assert_eq!(compared, 33);
}

/// The probe's calls, one per step, through the deep research agent.
#[tokio::test]
async fn the_file_system_and_todo_tools_give_the_deepagents_text() {
    let records = common::jsonl("ask/ref/vfs.jsonl");
    let turns: Vec<Value> = records
        .iter()
        .enumerate()
        .map(|(i, r)| {
            json!({"tool_calls": [{"id": format!("call_v{i}"), "name": r["name"],
                                   "arguments": common::expand(&r["arguments"])}]})
        })
        .chain(std::iter::once(json!({"content": "done"})))
        .collect();
    let request = ask::parse_request(&payload()).expect("request");
    let base = ask::research_spec(&request, None, "gpt-4o", false, Limits::default(), clock())
        .expect("spec");
    let spec = AgentSpec {
        known_tools: vec![
            "ls",
            "read_file",
            "write_file",
            "edit_file",
            "delete",
            "glob",
            "grep",
            "execute",
            "write_todos",
        ],
        budget: records.len() + 1,
        ..base
    };
    let model = common::ScriptedModel::new(&Value::Array(turns));
    let index = common::replay_index();
    let (context, _lines, _) = common::context();
    let outcome = agent::run(&spec, &model, &index, &Embedder::None, &context)
        .await
        .expect("runs");
    let results: Vec<&String> = outcome
        .messages
        .iter()
        .filter_map(|m| match m {
            Msg::Tool { content, .. } => Some(content),
            _ => None,
        })
        .collect();
    assert_eq!(results.len(), records.len());
    for (record, ours) in records.iter().zip(results) {
        let what = format!("{} {}", record["name"], record["arguments"]);
        if let Some(Value::String(text)) = record.get("result") {
            assert_eq!(ours, text, "{what}");
        } else {
            {
                use sha2::{Digest, Sha256};
                assert_eq!(
                    ours.chars().count() as u64,
                    record["result_len"].as_u64().expect("len"),
                    "{what}"
                );
                assert_eq!(
                    format!("{:x}", Sha256::digest(ours.as_bytes())),
                    record["result_sha256"].as_str().expect("sha"),
                    "{what}"
                );
            }
        }
    }
    assert_eq!(spec.mode, Mode::Research);
}

#[test]
fn resolve_wiki_sends_the_python_request() {
    let script = common::json_file("ask/script.json");
    let recorded = common::json_file("ask/ref/resolve.json");
    let wikis = script["resolve"]["wikis"]
        .as_array()
        .cloned()
        .unwrap_or_default();
    let question = script["resolve"]["question"].as_str().unwrap_or_default();
    let prompt = resolve::prompt(question, &wikis).expect("prompt");
    let body = common::client("http://127.0.0.1:9/v1")
        .body(&resolve::request(prompt, 4000), false)
        .expect("body");
    assert_eq!(body, recorded["request"]);
}
