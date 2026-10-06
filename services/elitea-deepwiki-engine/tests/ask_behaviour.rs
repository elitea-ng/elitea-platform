//! `ask`, deep research and `resolve_wiki` behaviour beyond the recorded
//! run: the token channel (`conformance/.../stream/token_events.json`),
//! the todo list, the step limits, a stop in the middle of the loop,
//! arguments a model can steer, and `resolve_wiki` over HTTP.

mod ask_common;

use ask_common as common;
use axum::Router;
use axum::body::Body;
use axum::extract::State;
use axum::http::{Request, Response};
use elitea_deepwiki_engine::ask::agent::{self, ASK_BUDGET_SPENT, Clock};
use elitea_deepwiki_engine::ask::embed::{Embedder, stand_in_embedding};
use elitea_deepwiki_engine::ask::summarize::Msg;
use elitea_deepwiki_engine::ask::tools::Codebase;
use elitea_deepwiki_engine::ask::{self, Limits, args, resolve};
use elitea_deepwiki_engine::errors::{EngineError, ErrorType};
use elitea_deepwiki_engine::llm::transport::Backoff;
use elitea_deepwiki_engine::llm::{
    ChatClient, ModelSettings, Timeouts, Transport, TransportSettings,
};
use elitea_deepwiki_engine::runner::{Line, StopSignal};
use serde_json::{Map, Value, json};
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tokio::net::TcpListener;

fn clock() -> Clock {
    Clock::Fixed {
        date: "2026-01-02".into(),
        timestamp: "T".into(),
    }
}

fn request() -> ask::QueryRequest {
    let payload = json!({
        "question": "How are notes stored?",
        "repo_config": {"repository": "acme/notes", "branch": "main"},
    });
    ask::parse_request(payload.as_object().expect("object")).expect("request")
}

fn call(id: &str, name: &str, arguments: impl Into<Value>) -> Value {
    let arguments: Value = arguments.into();
    json!({"id": id, "name": name, "arguments": arguments})
}

fn thinking(lines: &[Value]) -> Vec<String> {
    lines
        .iter()
        .filter_map(|l| l["thinking"].as_str().map(str::to_owned))
        .collect()
}

#[tokio::test]
async fn answer_fragments_are_token_lines_between_progress() {
    // Narration before a tool call streams too (Python joined it into the
    // answer); the result's answer is the fragments joined, no separator.
    let script = json!([
        {"content": "Looking. ", "tool_calls": [call("c1", "think", json!({"reflection": "x"}))]},
        {"content": format!("{}{}", "a".repeat(400), "tail")},
    ]);
    let spec = ask::ask_spec(
        &request(),
        None,
        "gpt-4o",
        false,
        true,
        Limits::default(),
        clock(),
    )
    .expect("spec");
    let model = common::ScriptedModel::new(&script);
    let (context, mut receiver, _) = common::context();
    let result = ask::run_agent(
        &spec,
        &request(),
        &model,
        &common::replay_index(),
        &Embedder::None,
        &context,
    )
    .await
    .expect("runs");
    let lines = common::drain(&mut receiver);
    let kinds: Vec<&str> = lines
        .iter()
        .map(|l| {
            if l.get("token").is_some() {
                "token"
            } else {
                "thinking"
            }
        })
        .collect();
    let first = kinds.iter().position(|k| *k == "token").expect("a token");
    assert!(kinds[..first].iter().all(|k| *k == "thinking"));
    // Narration, then the tool's progress, then the answer.
    assert_eq!(kinds[first + 1], "thinking");
    let tokens: Vec<&str> = lines.iter().filter_map(|l| l["token"].as_str()).collect();
    assert_eq!(tokens, vec!["Looking. ", &"a".repeat(400), "tail"]);
    assert_eq!(
        result["answer"],
        json!(format!("Looking. {}tail", "a".repeat(400)))
    );
    assert!(tokens.iter().all(|t| !t.is_empty()));
    // The token envelope the host builds from these lines is the
    // conformance fixture's; the engine side is the line shape.
    let fixture: Value = serde_json::from_str(
        &std::fs::read_to_string(
            std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                .join("../../conformance/provider/fixtures/deepwiki/stream/token_events.json"),
        )
        .expect("fixture"),
    )
    .expect("JSON");
    for line in fixture["golden_sequence"]["engine_lines"]
        .as_array()
        .expect("lines")
    {
        let parsed = match (line.get("thinking"), line.get("token")) {
            (Some(Value::String(t)), _) => Line::Thinking(t.clone()),
            (_, Some(Value::String(t))) => Line::Token(t.clone()),
            _ => continue,
        };
        assert_eq!(&parsed.to_json(), line);
    }
}

#[tokio::test]
async fn a_non_streaming_ask_sends_only_the_final_answer_as_a_token() {
    let script = json!([
        {"content": "narration", "tool_calls": [call("c1", "think", json!({"reflection": "x"}))]},
        {"content": "final"},
    ]);
    let spec = ask::ask_spec(
        &request(),
        None,
        "gpt-4o",
        false,
        false,
        Limits::default(),
        clock(),
    )
    .expect("spec");
    let model = common::ScriptedModel::new(&script);
    let (context, mut receiver, _) = common::context();
    let result = ask::run_agent(
        &spec,
        &request(),
        &model,
        &common::replay_index(),
        &Embedder::None,
        &context,
    )
    .await
    .expect("runs");
    let tokens: Vec<Value> = common::drain(&mut receiver)
        .into_iter()
        .filter_map(|l| l.get("token").cloned())
        .collect();
    assert_eq!(tokens, vec![json!("final")]);
    assert_eq!(result["answer"], json!("final"));
    assert!(model.bodies().iter().all(|b| b["stream"] == json!(false)));
}

#[tokio::test]
async fn todos_are_normalised_and_sent_when_they_change() {
    let todos = json!([
        {"content": "A", "status": "pending"},
        {"content": "B", "status": "in_progress"},
        {"content": "C", "status": "completed"},
    ]);
    let script = json!([
        {"tool_calls": [call("t1", "write_todos", json!({"todos": todos}))]},
        {"tool_calls": [call("t2", "write_todos", json!({"todos": todos}))]},
        {"tool_calls": [call("t3", "write_todos", json!({"todos": []})),
                        call("t4", "write_todos", json!({"todos": []}))]},
        {"content": "report"},
    ]);
    let spec = ask::research_spec(
        &request(),
        None,
        "gpt-4o",
        false,
        Limits::default(),
        clock(),
    )
    .expect("spec");
    let model = common::ScriptedModel::new(&script);
    let (context, mut receiver, _) = common::context();
    let result = ask::run_agent(
        &spec,
        &request(),
        &model,
        &common::replay_index(),
        &Embedder::None,
        &context,
    )
    .await
    .expect("runs");
    let updates: Vec<Value> = thinking(&common::drain(&mut receiver))
        .iter()
        .filter_map(|t| serde_json::from_str::<Value>(t).ok())
        .filter(|v| v["event"] == json!("todo_update"))
        .collect();
    // The same list twice is one update; the parallel calls change nothing.
    assert_eq!(updates.len(), 1);
    assert_eq!(
        updates[0]["data"]["items"],
        json!([
            {"id": 0, "title": "A", "description": "", "status": "not-started"},
            {"id": 1, "title": "B", "description": "", "status": "in-progress"},
            {"id": 2, "title": "C", "description": "", "status": "completed"},
        ])
    );
    assert_eq!(result["todos"], todos);
    assert_eq!(result["report"], json!("report"));
    let last = model.bodies().last().cloned().expect("a body");
    let messages = last["messages"].as_array().expect("messages");
    let parallel: Vec<&Value> = messages
        .iter()
        .filter(|m| m["tool_call_id"] == json!("t3") || m["tool_call_id"] == json!("t4"))
        .collect();
    assert_eq!(parallel.len(), 2);
    assert!(parallel.iter().all(|m| {
        m["content"]
            .as_str()
            .is_some_and(|c| c.contains("should never be called multiple times in parallel"))
    }));
}

#[tokio::test]
async fn the_ask_budget_is_enforced() {
    let limits = Limits {
        ask_tool_calls: 2,
        ..Limits::default()
    };
    let think = |id: &str| call(id, "think", json!({"reflection": id}));
    let script = json!([
        {"tool_calls": [think("a"), think("b"), think("c")]},
        {"content": "Found it: d.", "tool_calls": [think("d")]},
    ]);
    let spec =
        ask::ask_spec(&request(), None, "gpt-4o", false, false, limits, clock()).expect("spec");
    assert!(
        spec.system.parts()[0].contains("maximum of **2 tool calls**"),
        "the prompt states the enforced budget"
    );
    let model = common::ScriptedModel::new(&script);
    let (context, mut receiver, _) = common::context();
    let index = common::replay_index();
    let outcome = agent::run(&spec, &model, &index, &Embedder::None, &context)
        .await
        .expect("runs");
    let results: Vec<String> = outcome
        .messages
        .iter()
        .filter_map(|m| match m {
            Msg::Tool { content, .. } => Some(content.clone()),
            _ => None,
        })
        .collect();
    assert_eq!(results.len(), 3);
    assert!(results[0].contains("Reflection:\na"));
    assert_eq!(results[2], ASK_BUDGET_SPENT);
    let bodies = model.bodies();
    assert_eq!(bodies.len(), 2);
    assert!(bodies[0].get("tool_choice").is_none());
    assert_eq!(bodies[1]["tool_choice"], json!("none"));
    // A model that ignores `tool_choice: none` ends the run: its calls ran
    // nowhere, and its text is the answer.
    assert!(matches!(outcome.messages.last(), Some(Msg::Ai { .. })));
    assert_eq!(outcome.answer, "Found it: d.");
    let tokens: Vec<String> = std::iter::from_fn(|| receiver.try_recv().ok())
        .filter_map(|line| match line {
            Line::Token(text) => Some(text),
            _ => None,
        })
        .collect();
    assert_eq!(tokens, ["Found it: d."]);

    // The same reply without text: a failure, not an empty answer.
    let script = json!([
        {"tool_calls": [think("a"), think("b"), think("c")]},
        {"tool_calls": [think("d")]},
    ]);
    let model = common::ScriptedModel::new(&script);
    let (context, _receiver, _) = common::context();
    let failed = agent::run(&spec, &model, &index, &Embedder::None, &context).await;
    let error = failed.expect_err("no answer is a failure");
    assert_eq!(error.error_type, ErrorType::Runtime);
    assert!(
        error.message.contains("DEEPWIKI_ASK_MAX_ITERATIONS=2"),
        "{}",
        error.message
    );
}

#[tokio::test]
async fn tool_calls_without_ids_keep_one_id_each() {
    let think =
        |reflection: &str| json!({"name": "think", "arguments": {"reflection": reflection}});
    let script = json!([
        {"tool_calls": [think("one"), think("two")]},
        {"content": "done"},
    ]);
    let spec = ask::ask_spec(
        &request(),
        None,
        "gpt-4o",
        false,
        false,
        Limits::default(),
        clock(),
    )
    .expect("spec");
    let model = common::ScriptedModel::new(&script);
    let (context, _receiver, _) = common::context();
    let outcome = agent::run(
        &spec,
        &model,
        &common::replay_index(),
        &Embedder::None,
        &context,
    )
    .await
    .expect("runs");
    let ids = |kind: &str| -> Vec<String> {
        outcome
            .thinking_steps
            .iter()
            .filter(|s| s["type"] == kind)
            .map(|s| s["tool_call_id"].as_str().unwrap_or_default().to_owned())
            .collect()
    };
    // Announced as call_1 and call_2 (steps 1 and 2); each result names
    // its own call, not the step it happens at.
    assert_eq!(ids("tool_call"), ["call_1", "call_2"]);
    assert_eq!(ids("tool_result"), ["call_1", "call_2"]);
    let called: Vec<&str> = outcome
        .messages
        .iter()
        .flat_map(|m| match m {
            Msg::Ai { calls, .. } => calls.iter().map(|c| c.id.as_str()).collect(),
            _ => Vec::new(),
        })
        .collect();
    let answered: Vec<&str> = outcome
        .messages
        .iter()
        .filter_map(|m| match m {
            Msg::Tool { call_id, .. } => Some(call_id.as_str()),
            _ => None,
        })
        .collect();
    assert_eq!(called, ["call_1", "call_2"]);
    assert_eq!(answered, ["call_1", "call_2"]);
    // The model saw the same ids in the next request.
    let second = &model.bodies()[1]["messages"];
    let sent: Vec<&str> = second
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|m| m["tool_call_id"].as_str())
        .collect();
    assert_eq!(sent, ["call_1", "call_2"]);
}

#[tokio::test]
async fn the_research_step_limit_is_enforced() {
    let limits = Limits {
        research_iterations: 1,
        ..Limits::default()
    };
    let many: Vec<Value> = (0..30)
        .map(|i| call(&format!("c{i}"), "think", json!({"reflection": "x"})))
        .collect();
    let script = json!([{"tool_calls": many}, {"content": "report"}]);
    let spec =
        ask::research_spec(&request(), None, "gpt-4o", false, limits, clock()).expect("spec");
    let model = common::ScriptedModel::new(&script);
    let (context, _receiver, _) = common::context();
    let outcome = agent::run(
        &spec,
        &model,
        &common::replay_index(),
        &Embedder::None,
        &context,
    )
    .await
    .expect("runs");
    let refused = outcome
        .messages
        .iter()
        .filter(|m| matches!(m, Msg::Tool { content, .. } if content.starts_with("Error: one step may call at most 25")))
        .count();
    assert_eq!(refused, 5);
    let bodies = model.bodies();
    assert_eq!(bodies[1]["tool_choice"], json!("none"));
    assert_eq!(outcome.answer, "report");
}

#[tokio::test]
async fn a_stop_ends_the_loop_at_the_next_checkpoint() {
    let script = json!([
        {"tool_calls": [call("c1", "think", json!({"reflection": "x"}))]},
        {"content": "never"},
    ]);
    let spec = ask::ask_spec(
        &request(),
        None,
        "gpt-4o",
        false,
        true,
        Limits::default(),
        clock(),
    )
    .expect("spec");
    let (context, mut receiver, stop) = common::context();
    let mut model = common::ScriptedModel::new(&script);
    model.stop_at = Some((1, stop));
    let outcome = ask::run_agent(
        &spec,
        &request(),
        &model,
        &common::replay_index(),
        &Embedder::None,
        &context,
    )
    .await;
    assert_eq!(outcome, Err(EngineError::cancelled()));
    let lines = common::drain(&mut receiver);
    assert!(lines.iter().all(|l| l.get("token").is_none()));
    assert!(
        !thinking(&lines).iter().any(|t| t.contains("Ask Complete")),
        "a stopped run does not complete"
    );
    assert_eq!(model.bodies().len(), 2);
}

#[tokio::test]
async fn the_document_results_follow_the_limit() {
    // No recorded fused search: a search of the documents fails the tool,
    // and the failure names the pool it asked for (`min(k_doc * 4, 20)`).
    let mut index = common::replay_index();
    index.hybrid.clear();
    let embedder = Embedder::Fixed(stand_in_embedding);
    let stop = StopSignal::default();
    let raw = json!({"query": "note store save"});
    let raw = raw.as_object().cloned().unwrap_or_default();
    let parsed =
        args::validate(args::params("search_codebase").expect("tool"), &raw).expect("valid");
    let mut texts = Vec::new();
    for doc_results in [Limits::default().doc_results, 2, 0] {
        let codebase = Codebase {
            store: &index,
            embedder: &embedder,
            stop: &stop,
            doc_results,
        };
        texts.push(
            codebase
                .run("search_codebase", &parsed)
                .await
                .expect("runs")
                .expect("a text"),
        );
    }
    assert!(
        texts[0].contains(r#"["note store save",12,30,30]"#),
        "{}",
        texts[0]
    );
    assert!(
        texts[1].contains(r#"["note store save",8,30,30]"#),
        "{}",
        texts[1]
    );
    // 0: no document search at all, only the code results.
    assert!(
        texts[2].starts_with("## Search Results for: note store save\n\nFound 10 relevant items:"),
        "{}",
        texts[2]
    );
    assert!(!texts[2].contains("(vectorstore)"));
}

#[tokio::test]
async fn arguments_a_model_steers_are_checked() {
    let index = common::replay_index();
    let embedder = Embedder::Fixed(stand_in_embedding);
    let stop = StopSignal::default();
    let codebase = Codebase {
        store: &index,
        embedder: &embedder,
        stop: &stop,
        doc_results: Limits::default().doc_results,
    };
    let run = |name: &'static str, raw: Value| {
        let codebase = &codebase;
        async move {
            let raw = raw.as_object().cloned().unwrap_or_default();
            let parsed = args::validate(args::params(name).expect("tool"), &raw).expect("valid");
            codebase
                .run(name, &parsed)
                .await
                .expect("runs")
                .expect("a text")
        }
    };
    // A negative line budget is one line, not a Python negative slice.
    let code = run(
        "get_code",
        json!({"symbol_name": "NoteStore", "max_lines": -5}),
    )
    .await;
    assert!(code.contains("```\nclass NoteStore(BaseStore):\n```\n... (truncated at 1 lines)"));
    // An absurd depth is bounded.
    let deep = run(
        "get_relationships_tool",
        json!({"symbol_name": "NoteStore", "max_depth": 1_000_000_000_i64}),
    )
    .await;
    assert!(deep.lines().count() <= 52, "at most 50 edges: {deep}");
    let limited = run(
        "query_graph",
        json!({"expression": "type:class limit:99999999"}),
    )
    .await;
    assert!(limited.contains("(5 matches)"), "{limited}");

    // The file system: escapes, the bound, malformed calls.
    let script = json!([
        {"tool_calls": [
            call("v1", "read_file", json!({"file_path": "/../etc/passwd"})),
            call("v2", "write_file", json!({"file_path": "../../x", "content": "x"})),
            call("v3", "ls", json!({"path": "~"})),
            call("v4", "glob", json!({"pattern": "../*"})),
            call("v5", "grep", json!({"pattern": "x", "path": "/a/../../b"})),
            call("v6", "write_file", json!({"file_path": "/big", "content": "x".repeat(17 * 1024 * 1024)})),
            call("v7", "read_file", "{not json"),
            call("v8", "read_file", json!([1, 2])),
            call("v9", "rm_rf", json!({})),
            call("v10", "read_file", json!({"file_path": "C:/Windows/win.ini"})),
        ]},
        {"content": "done"},
    ]);
    let spec = ask::research_spec(
        &request(),
        None,
        "gpt-4o",
        false,
        Limits::default(),
        clock(),
    )
    .expect("spec");
    let model = common::ScriptedModel::new(&script);
    let (context, _receiver, _) = common::context();
    let outcome = agent::run(&spec, &model, &index, &Embedder::None, &context)
        .await
        .expect("runs");
    let results: Vec<String> = outcome
        .messages
        .iter()
        .filter_map(|m| match m {
            Msg::Tool { content, .. } => Some(content.clone()),
            _ => None,
        })
        .collect();
    assert_eq!(
        results[0],
        "Error: Path traversal not allowed: /../etc/passwd"
    );
    assert_eq!(results[1], "Error: Path traversal not allowed: ../../x");
    assert_eq!(results[2], "Error: Path traversal not allowed: ~");
    assert_eq!(
        results[3],
        "Error: Path traversal not allowed in glob pattern '../*'"
    );
    assert_eq!(results[4], "Error: Path traversal not allowed: /a/../../b");
    assert!(results[5].starts_with("Error: the file system is full"));
    assert!(
        results[6]
            .starts_with("Error: the arguments of tool call 'read_file' are not a JSON object")
    );
    assert!(
        results[7]
            .starts_with("Error: the arguments of tool call 'read_file' are not a JSON object")
    );
    assert!(results[8].starts_with("Error: rm_rf is not a valid tool, try one of [ls, read_file"));
    assert!(results[9].starts_with("Error: Windows absolute paths are not supported"));
    assert_eq!(outcome.answer, "done", "the loop goes on after bad calls");
}

// ---- resolve_wiki over HTTP ------------------------------------------

#[derive(Clone, Default)]
struct Seen(Arc<Mutex<Vec<(String, Value)>>>);

async fn gateway(status: u16, content: &'static str) -> (Seen, String) {
    let seen = Seen::default();
    let app = Router::new()
        .fallback(move |State(seen): State<Seen>, request: Request<Body>| async move {
            let path = request.uri().path().to_owned();
            let body = axum::body::to_bytes(request.into_body(), 1 << 20)
                .await
                .unwrap_or_default();
            let parsed: Value = serde_json::from_slice(&body).unwrap_or(Value::Null);
            seen.0.lock().expect("lock").push((path, parsed));
            let reply = json!({"id": "x", "object": "chat.completion", "created": 0, "model": "m",
                "choices": [{"index": 0, "finish_reason": "stop",
                             "message": {"role": "assistant", "content": content}}]});
            Response::builder()
                .status(status)
                .header("content-type", "application/json")
                .body(Body::from(reply.to_string()))
                .unwrap_or_default()
        })
        .with_state(seen.clone());
    let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
    let address = listener.local_addr().expect("address");
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    (seen, format!("http://{address}/gw"))
}

fn transport() -> Transport {
    Transport::new(&TransportSettings {
        ca_file: None,
        timeouts: Timeouts {
            connect: Duration::from_secs(2),
            request: Duration::from_secs(5),
            stream_idle: Duration::from_secs(2),
            stream_total: Duration::from_secs(5),
        },
        backoff: Backoff {
            initial: Duration::from_millis(1),
            max: Duration::from_millis(2),
            max_retry_after: Duration::from_millis(5),
        },
    })
    .expect("transport")
}

#[allow(clippy::needless_pass_by_value)]
fn resolve_arguments(base: &str, wikis: Value, model: Option<&str>) -> Map<String, Value> {
    let mut settings = json!({"api_base": base, "api_key": "k", "max_retries": 0});
    if let Some(model) = model {
        settings["model_name"] = json!(model);
    }
    json!({"question": "Where are notes?", "wikis": wikis, "llm_settings": settings})
        .as_object()
        .cloned()
        .unwrap_or_default()
}

#[tokio::test]
async fn resolve_wiki_answers_one_trimmed_id() {
    let (seen, base) = gateway(200, "  \"acme--notes--main\"\n").await;
    let (context, _receiver, _) = common::context();
    let wikis =
        json!([{"wiki_id": "acme--notes--main", "wiki_title": "Notes", "description": "d"}]);
    let result = resolve::resolve_wiki(
        &resolve_arguments(&base, wikis, Some("gpt-4o")),
        &transport(),
        &context,
    )
    .await
    .expect("resolves");
    assert_eq!(
        result,
        json!({"success": true, "wiki_id": "acme--notes--main"})
    );
    let seen = seen.0.lock().expect("lock").clone();
    assert_eq!(seen.len(), 1, "one model call");
    let (path, body) = &seen[0];
    assert_eq!(path, "/gw/v1/chat/completions", "create_llm appends /v1");
    assert_eq!(body["temperature"], json!(0.0));
    assert_eq!(body["max_completion_tokens"], json!(4000));
    assert_eq!(body["stream"], json!(false));
    assert_eq!(body["messages"].as_array().map(Vec::len), Some(1));
    assert_eq!(body["messages"][0]["role"], json!("user"));
}

#[tokio::test]
async fn resolve_wiki_refusals_are_results() {
    let (seen, base) = gateway(500, "x").await;
    let (context, _receiver, _) = common::context();
    let none = resolve::resolve_wiki(
        &resolve_arguments(&base, json!([]), Some("gpt-4o")),
        &transport(),
        &context,
    )
    .await
    .expect("answers");
    assert_eq!(none, json!({"success": true, "wiki_id": "NONE"}));
    let wikis = json!(["a--b--main"]);
    let no_model = resolve::resolve_wiki(
        &resolve_arguments(&base, wikis.clone(), None),
        &transport(),
        &context,
    )
    .await
    .expect("answers");
    assert_eq!(no_model["success"], json!(false));
    assert_eq!(no_model["error_category"], json!("invalid_input"));
    let failed = resolve::resolve_wiki(
        &resolve_arguments(&base, wikis, Some("gpt-4o")),
        &transport(),
        &context,
    )
    .await
    .expect("a failure is a result");
    assert_eq!(failed["success"], json!(false));
    assert!(
        failed["error"]
            .as_str()
            .is_some_and(|e| e.starts_with("Wiki resolution failed: ")),
        "{failed}"
    );
    assert_eq!(
        seen.0.lock().expect("lock").len(),
        1,
        "no call without wikis or a model"
    );
}

#[tokio::test]
async fn the_live_client_streams_the_answer() {
    // The SSE path of `ChatClient` as the `ask` model.
    let app = Router::new().fallback(|| async {
        let chunk = |text: &str| {
            format!(
                "data: {}\n\n",
                json!({"id": "x", "object": "chat.completion.chunk", "created": 0, "model": "m",
                       "choices": [{"index": 0, "finish_reason": null, "delta": {"content": text}}]})
            )
        };
        let end = format!(
            "data: {}\n\ndata: [DONE]\n\n",
            json!({"id": "x", "object": "chat.completion.chunk", "created": 0, "model": "m",
                   "choices": [{"index": 0, "finish_reason": "stop", "delta": {}}]})
        );
        Response::builder()
            .header("content-type", "text/event-stream")
            .body(Body::from(format!("{}{}{end}", chunk("Notes live "), chunk("in NoteStore."))))
            .unwrap_or_default()
    });
    let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
    let address = listener.local_addr().expect("address");
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    let settings = ModelSettings::from_llm_settings(&json!({
        "api_base": format!("http://{address}/v1"), "api_key": "k", "model_name": "gpt-4o",
    }))
    .expect("settings");
    let client = ChatClient::new(transport(), settings);
    let spec = ask::ask_spec(
        &request(),
        None,
        "gpt-4o",
        false,
        true,
        Limits::default(),
        clock(),
    )
    .expect("spec");
    let (context, mut receiver, _) = common::context();
    let result = ask::run_agent(
        &spec,
        &request(),
        &client,
        &common::replay_index(),
        &Embedder::None,
        &context,
    )
    .await
    .expect("runs");
    let tokens: Vec<Value> = common::drain(&mut receiver)
        .into_iter()
        .filter_map(|l| l.get("token").cloned())
        .collect();
    assert_eq!(tokens, vec![json!("Notes live "), json!("in NoteStore.")]);
    assert_eq!(result["answer"], json!("Notes live in NoteStore."));
}

#[tokio::test]
async fn a_long_history_is_summarised_before_the_next_call() {
    let think = |id: &str| call(id, "think", json!({"reflection": "x".repeat(400)}));
    let script = json!([
        {"tool_calls": [think("a")]},
        {"tool_calls": [think("b")]},
        {"tool_calls": [think("c")]},
        {"tool_calls": [think("d")]},
        {"content": "answer"},
    ]);
    let mut spec = ask::research_spec(
        &request(),
        None,
        "some-model",
        false,
        Limits::default(),
        clock(),
    )
    .expect("spec");
    // No profile: keep the last 6 messages; a low trigger for the test.
    spec.policy.trigger_tokens = 300;
    let model = common::ScriptedModel::new(&script);
    let (context, _receiver, _) = common::context();
    let outcome = agent::run(
        &spec,
        &model,
        &common::replay_index(),
        &Embedder::None,
        &context,
    )
    .await
    .expect("runs");
    assert_eq!(outcome.answer, "answer");
    let bodies = model.bodies();
    let summary_calls: Vec<&Value> = bodies.iter().filter(|b| b.get("tools").is_none()).collect();
    assert!(!summary_calls.is_empty(), "a summary was asked for");
    let asked = summary_calls[0]["messages"][0]["content"]
        .as_str()
        .unwrap_or_default();
    assert!(asked.starts_with("<role>\nContext Extraction Assistant\n</role>"));
    assert!(asked.contains("<message type=\"human\">## Research Question"));
    assert!(asked.ends_with("</messages>"));
    // The next agent call starts from the summary.
    let after = bodies
        .iter()
        .skip_while(|b| b.get("tools").is_some())
        .find(|b| b.get("tools").is_some())
        .expect("an agent call after the summary");
    assert_eq!(
        after["messages"][1]["content"],
        json!("Here is a summary of the conversation to date:\n\nsummary")
    );
}
