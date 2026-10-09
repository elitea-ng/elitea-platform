//! Shared by the `ask` / deep research tests: the recorded Python run
//! (`tests/fixtures/ask/ref`, made by `parity/python_ask_dump.py`), the
//! replay index over it, and a scripted model that records the request
//! bodies the live client would send.

#![allow(dead_code, clippy::missing_panics_doc)]

use elitea_deepwiki_engine::ask::agent::Model;
use elitea_deepwiki_engine::ask::store::{ReplayIndex, edge_from_dump, node_from_dump};
use elitea_deepwiki_engine::errors::EngineError;
use elitea_deepwiki_engine::llm::{
    ChatClient, ChatRequest, ChatResponse, ModelSettings, ToolCall, Transport, TransportSettings,
    Usage,
};
use elitea_deepwiki_engine::runner::{Context, Line, StopSignal};
use elitea_deepwiki_engine::storage::search::Scores;
use serde_json::{Value, json};
use std::path::PathBuf;
use std::sync::Mutex;
use tokio::sync::mpsc;

pub fn fixtures() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures")
}

pub fn read(relative: &str) -> String {
    let path = fixtures().join(relative);
    std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()))
}

pub fn json_file(relative: &str) -> Value {
    serde_json::from_str(&read(relative)).expect("JSON fixture")
}

pub fn jsonl(relative: &str) -> Vec<Value> {
    read(relative)
        .lines()
        .filter(|l| !l.trim().is_empty())
        .map(|l| serde_json::from_str(l).expect("JSONL fixture"))
        .collect()
}

/// The recorded index rows (equal to the wiki golden's).
pub fn rows() -> (Vec<Value>, Vec<Value>) {
    (
        jsonl("wiki/golden/ref/nodes.jsonl"),
        jsonl("wiki/golden/ref/edges.jsonl"),
    )
}

/// The replay index of the recorded run.
pub fn replay_index() -> ReplayIndex {
    let (nodes, edges) = rows();
    let replay = json_file("ask/ref/replay.json");
    let mut index = ReplayIndex {
        nodes: nodes
            .iter()
            .map(|n| node_from_dump(n).expect("node"))
            .collect(),
        edges: edges.iter().map(edge_from_dump).collect(),
        ..ReplayIndex::default()
    };
    for (key, rows) in replay["fts"].as_object().expect("fts") {
        let rows = rows
            .as_array()
            .expect("rows")
            .iter()
            .map(|r| {
                (
                    r[0].as_str().expect("id").to_owned(),
                    r[1].as_f64().unwrap_or(0.0),
                    r[2].as_f64().unwrap_or(0.0),
                )
            })
            .collect();
        index.fts.insert(key.clone(), rows);
    }
    for (key, ids) in replay["fts_any"].as_object().expect("fts_any") {
        index.fts_any.insert(key.clone(), strings(ids));
    }
    for (key, ids) in replay["hybrid"].as_object().expect("hybrid") {
        index.hybrid.insert(
            key.clone(),
            strings(ids)
                .into_iter()
                .map(|id| (id, Scores::default()))
                .collect(),
        );
    }
    index
}

pub fn strings(value: &Value) -> Vec<String> {
    value
        .as_array()
        .expect("array")
        .iter()
        .map(|v| v.as_str().expect("string").to_owned())
        .collect()
}

/// The client whose request builder the scripted model uses.
pub fn client(api_base: &str) -> ChatClient {
    let settings = ModelSettings::from_llm_settings(
        &json!({"api_base": api_base, "api_key": "stub-key", "model_name": "gpt-4o"}),
    )
    .expect("settings");
    let transport = Transport::new(&TransportSettings::default()).expect("transport");
    ChatClient::new(transport, settings)
}

/// A model that answers the stub script's turns and records each request
/// body exactly as the live client would send it.
pub struct ScriptedModel {
    pub client: ChatClient,
    pub turns: Mutex<Vec<Value>>,
    pub bodies: Mutex<Vec<Value>>,
    /// Requested a stop before answering this turn (0-based), when set.
    pub stop_at: Option<(usize, StopSignal)>,
}

impl ScriptedModel {
    pub fn new(turns: &Value) -> Self {
        Self {
            client: client("http://127.0.0.1:9/v1"),
            turns: Mutex::new(turns.as_array().cloned().unwrap_or_default()),
            bodies: Mutex::new(Vec::new()),
            stop_at: None,
        }
    }

    pub fn bodies(&self) -> Vec<Value> {
        self.bodies.lock().expect("lock").clone()
    }
}

impl Model for ScriptedModel {
    fn model_name(&self) -> &'static str {
        "gpt-4o"
    }

    fn anthropic(&self) -> bool {
        false
    }

    async fn call(
        &self,
        request: &ChatRequest,
        stream: bool,
        stop: &StopSignal,
        on_text: &mut (dyn FnMut(&str) + Send),
    ) -> Result<ChatResponse, EngineError> {
        let index = {
            let mut bodies = self.bodies.lock().expect("lock");
            bodies.push(self.client.body(request, stream)?);
            bodies.len() - 1
        };
        if let Some((at, signal)) = &self.stop_at
            && *at == index
        {
            signal.request();
        }
        if stop.is_requested() {
            return Err(EngineError::cancelled());
        }
        let turn = if request.tools.is_empty() {
            None
        } else {
            let mut turns = self.turns.lock().expect("lock");
            (!turns.is_empty()).then(|| turns.remove(0))
        };
        let turn = turn.unwrap_or_else(|| json!({"content": "summary"}));
        let content = turn["content"].as_str().unwrap_or_default().to_owned();
        if stream {
            let chars: Vec<char> = content.chars().collect();
            for chunk in chars.chunks(400) {
                on_text(&chunk.iter().collect::<String>());
            }
        }
        let tool_calls = turn["tool_calls"]
            .as_array()
            .cloned()
            .unwrap_or_default()
            .iter()
            .map(|c| ToolCall {
                id: c["id"].as_str().unwrap_or_default().to_owned(),
                name: c["name"].as_str().unwrap_or_default().to_owned(),
                arguments: match &c["arguments"] {
                    Value::String(raw) => raw.clone(),
                    other => serde_json::to_string(other).expect("arguments"),
                },
            })
            .collect::<Vec<_>>();
        Ok(ChatResponse {
            finish_reason: Some(
                if tool_calls.is_empty() {
                    "stop"
                } else {
                    "tool_calls"
                }
                .into(),
            ),
            content,
            tool_calls,
            usage: (!stream).then_some(Usage {
                prompt_tokens: 1,
                completion_tokens: 1,
                total_tokens: 2,
                ..Usage::default()
            }),
            ..ChatResponse::default()
        })
    }
}

/// A context whose lines are collected.
pub fn context() -> (Context, mpsc::UnboundedReceiver<Line>, StopSignal) {
    let (sender, receiver) = mpsc::unbounded_channel();
    let stop = StopSignal::default();
    (Context::new(sender, stop.clone()), receiver, stop)
}

/// The lines sent, as the JSON objects the socket writes.
pub fn drain(receiver: &mut mpsc::UnboundedReceiver<Line>) -> Vec<Value> {
    let mut out = Vec::new();
    while let Ok(line) = receiver.try_recv() {
        out.push(line.to_json());
    }
    out
}

/// `$repeat` markers of the script expanded (`python_ask_dump.expand`).
pub fn expand(value: &Value) -> Value {
    match value {
        Value::Object(map) if map.len() == 1 && map.contains_key("$repeat") => {
            let text = map["$repeat"][0].as_str().unwrap_or_default();
            let count = usize::try_from(map["$repeat"][1].as_u64().unwrap_or(0)).unwrap_or(0);
            Value::from(text.repeat(count))
        }
        Value::Object(map) => {
            Value::Object(map.iter().map(|(k, v)| (k.clone(), expand(v))).collect())
        }
        Value::Array(items) => Value::Array(items.iter().map(expand).collect()),
        other => other.clone(),
    }
}
