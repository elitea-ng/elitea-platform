//! The page gate's Rust side (developer tool; `deepwiki-parity pages`).
//!
//! Reads a dump `parity/python_pages_dump.py` wrote — the index rows after
//! Phase 3, the graph facts, the recorded full-text searches, the
//! structure — runs [`super::run::generate_wiki_pages`] against
//! [`StubModel`], and writes the same files the dump wrote (`pages.json`,
//! `result.json`, `requests.jsonl`, `summary.json`) for
//! `parity/compare_pages.py`.
//!
//! [`StubModel`] is `services/elitea-deepwiki/e2e/llm_stub.py` in Rust:
//! the same canned answers, chosen the same way from the prompt, streamed
//! as SSE when asked, and every request body recorded. The golden tests
//! use it too, so they need neither Python nor a network.

use super::compose::VersionClock;
use super::context::PylonPlugin;
use super::expansion::ExpansionFlags;
use super::index::{GraphFacts, IndexEdge, IndexNode, PageIndex};
use super::pages::{PageGenerator, PageSettings};
use super::retrieve::{CONTEXT_TOKEN_BUDGET, RetrievalContext};
use super::run::{PagesOutcome, WikiIdentity, generate_wiki_pages};
use super::search::ReplaySearch;
use super::spec::WikiStructureSpec;
use crate::errors::{EngineError, ErrorType};
use crate::llm::{ChatClient, ModelSettings, Transport, TransportSettings};
use crate::pyjson;
use crate::runner::StopSignal;
use axum::Router;
use axum::body::Body;
use axum::extract::{Request, State};
use axum::response::{IntoResponse, Response};
use serde::de::DeserializeOwned;
use serde_json::{Map, Value, json};
use std::fmt::Write as _;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use tokio::net::TcpListener;

fn invalid(message: impl Into<String>) -> EngineError {
    EngineError::new(ErrorType::Value, message)
}

/// `llm_stub.STRUCTURE`, key order kept.
fn stub_structure() -> Value {
    json!({
        "wiki_title": "notes-service",
        "overview": "A small notes service with SQLite persistence, ranked search and bearer-token auth.",
        "total_pages": 3,
        "sections": [
            {
                "section_name": "Overview", "section_order": 0,
                "description": "What the service is and how a request flows through it.",
                "rationale": "Readers need orientation before any component detail.",
                "pages": [
                    {"page_name": "Getting Started", "page_order": 0,
                     "description": "What the notes service does.",
                     "content_focus": "purpose and entry points",
                     "rationale": "First contact for a new reader.",
                     "target_symbols": ["handle_create_note", "handle_search"],
                     "key_files": ["api.py", "README.md"], "retrieval_query": "notes service overview"},
                ],
            },
            {
                "section_name": "Components", "section_order": 1,
                "description": "The three modules and what each owns.",
                "rationale": "Each module has a distinct responsibility worth its own page.",
                "pages": [
                    {"page_name": "Note Storage", "page_order": 0,
                     "description": "How notes are persisted.",
                     "content_focus": "NoteStore and the SQLite schema",
                     "rationale": "Persistence is the core of the service.",
                     "target_symbols": ["NoteStore", "save_note", "load_note", "delete_note"],
                     "key_files": ["notes/store.py"], "retrieval_query": "how are notes stored"},
                    {"page_name": "Bearer Tokens", "page_order": 1,
                     "description": "How requests are authenticated.",
                     "content_focus": "issue_token and verify_token",
                     "rationale": "Auth gates every write.",
                     "target_symbols": ["issue_token", "verify_token"],
                     "key_files": ["auth/tokens.py"], "retrieval_query": "verify bearer token signature"},
                ],
            },
        ],
    })
}

/// The page answer of the dump's `--mermaid-pages` mode (after a
/// `# {page}` heading): broken diagrams for the sanitizer.
pub const MERMAID_PAGE: &str = concat!(
    "## Overview\n\nThis page documents the component.\n\n",
    "```mermaid\nA[Start here] --> B[Load (config)]\nB -.-> end[\"Done\"]\nB -->| yes | C[\"x\"]\n```\n\n",
    "Then the sequence:\n\n",
    "```mermaid\nsequenceDiagram\n  participant end\n  User->>API: call(a ,b)\n  API-->>end: ok; done\n  deactivate X\n```\n",
    "\nAn inline closer:\n```mermaid\ngraph\n  X --> Y```\nAfter.\n"
);

/// How the stub answers a page prompt.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum PageAnswer {
    /// `llm_stub.answer` unchanged.
    #[default]
    Stub,
    /// [`MERMAID_PAGE`] under the page name.
    Mermaid,
}

/// `llm_stub.answer(prompt)`, with the page mode.
#[must_use]
pub fn stub_answer_with(prompt: &str, mode: PageAnswer) -> String {
    if mode == PageAnswer::Mermaid && prompt.contains("**Context Variables:**") {
        let page = prompt
            .lines()
            .find_map(|l| l.strip_prefix("- Page: "))
            .unwrap_or("");
        return format!("# {page}\n\n{MERMAID_PAGE}");
    }
    stub_answer(prompt)
}

/// `llm_stub.answer(prompt)`.
#[must_use]
pub fn stub_answer(prompt: &str) -> String {
    let lowered = prompt.to_lowercase();
    let wants_json =
        lowered.contains("json") || lowered.contains("wiki_title") || lowered.contains("sections");
    if wants_json
        && (lowered.contains("structure")
            || lowered.contains("sections")
            || lowered.contains("wiki_title"))
    {
        return pyjson::dumps(&stub_structure());
    }
    if wants_json {
        return pyjson::dumps(&json!({
            "executive_summary": "A small notes service: store, search and authorise notes.",
            "core_purpose": "Persist notes and answer ranked searches over them.",
            "key_components": ["NoteStore", "rank_notes", "verify_token"],
            "architecture": "A thin HTTP layer over a SQLite store and an in-memory index.",
        }));
    }
    concat!(
        "## Overview\n\n",
        "The notes service stores notes in SQLite, ranks search results by term ",
        "overlap, and authenticates writes with signed bearer tokens.\n\n",
        "```mermaid\nflowchart LR\n  api --> store\n  api --> auth\n```\n\n",
        "See `notes/store.py` for persistence and `auth/tokens.py` for signing.\n"
    )
    .to_owned()
}

/// The stub model on loopback, recording every chat request body.
#[derive(Debug, Clone)]
pub struct StubModel {
    pub base_url: String,
    recorded: Arc<Mutex<Vec<Value>>>,
}

/// The stub's shared state.
#[derive(Debug, Clone)]
struct StubState {
    recorded: Arc<Mutex<Vec<Value>>>,
    mode: PageAnswer,
}

impl StubModel {
    /// Serve on a free loopback port.
    ///
    /// # Errors
    ///
    /// When loopback cannot be bound.
    pub async fn start(mode: PageAnswer) -> Result<Self, EngineError> {
        let recorded: Arc<Mutex<Vec<Value>>> = Arc::new(Mutex::new(Vec::new()));
        let listener = TcpListener::bind("127.0.0.1:0")
            .await
            .map_err(|e| invalid(format!("bind: {e}")))?;
        let address = listener
            .local_addr()
            .map_err(|e| invalid(format!("address: {e}")))?;
        let app = Router::new().fallback(handle).with_state(StubState {
            recorded: Arc::clone(&recorded),
            mode,
        });
        tokio::spawn(async move {
            let _ = axum::serve(listener, app).await;
        });
        Ok(Self {
            base_url: format!("http://{address}/v1"),
            recorded,
        })
    }

    /// The chat request bodies so far.
    #[must_use]
    pub fn requests(&self) -> Vec<Value> {
        self.recorded.lock().map(|r| r.clone()).unwrap_or_default()
    }
}

async fn handle(State(state): State<StubState>, request: Request) -> Response {
    let path = request.uri().path().to_owned();
    let bytes = axum::body::to_bytes(request.into_body(), 256 * 1024 * 1024)
        .await
        .unwrap_or_default();
    let body: Value = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    if !path.ends_with("/chat/completions") {
        return (axum::http::StatusCode::NOT_FOUND, "").into_response();
    }
    if let Ok(mut all) = state.recorded.lock() {
        all.push(body.clone());
    }
    let prompt = body["messages"]
        .as_array()
        .map(|messages| {
            messages
                .iter()
                .map(|m| match &m["content"] {
                    Value::String(text) => text.clone(),
                    Value::Null => String::new(),
                    other => other.to_string(),
                })
                .collect::<Vec<_>>()
                .join("\n")
        })
        .unwrap_or_default();
    let content = stub_answer_with(&prompt, state.mode);
    let model = body["model"].as_str().unwrap_or("stub").to_owned();
    if body["stream"].as_bool() == Some(true) {
        let mut sse = String::new();
        let chars: Vec<char> = content.chars().collect();
        for chunk in chars.chunks(400) {
            let text: String = chunk.iter().collect();
            let event = json!({"id": "chatcmpl-stub", "object": "chat.completion.chunk", "created": 0, "model": model,
                "choices": [{"index": 0, "finish_reason": null, "delta": {"content": text}}]});
            let _ = write!(sse, "data: {event}\n\n");
        }
        let last = json!({"id": "chatcmpl-stub", "object": "chat.completion.chunk", "created": 0, "model": model,
            "choices": [{"index": 0, "finish_reason": "stop", "delta": {}}]});
        let _ = write!(sse, "data: {last}\n\ndata: [DONE]\n\n");
        return Response::builder()
            .header("content-type", "text/event-stream")
            .body(Body::from(sse))
            .unwrap_or_default();
    }
    axum::Json(json!({"id": "chatcmpl-stub", "object": "chat.completion", "created": 0, "model": model,
        "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": content}}],
        "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}))
    .into_response()
}

fn read(path: &Path) -> Result<String, EngineError> {
    std::fs::read_to_string(path).map_err(|e| invalid(format!("{}: {e}", path.display())))
}

fn jsonl<T: DeserializeOwned>(path: &Path) -> Result<Vec<T>, EngineError> {
    read(path)?
        .lines()
        .filter(|l| !l.trim().is_empty())
        .map(|l| serde_json::from_str(l).map_err(|e| invalid(format!("{}: {e}", path.display()))))
        .collect()
}

fn json_file(path: &Path) -> Result<Value, EngineError> {
    serde_json::from_str(&read(path)?).map_err(|e| invalid(format!("{}: {e}", path.display())))
}

fn string_map(value: &Value) -> std::collections::HashMap<String, String> {
    value
        .as_object()
        .map(|m| {
            m.iter()
                .filter_map(|(k, v)| v.as_str().map(|s| (k.clone(), s.to_owned())))
                .collect()
        })
        .unwrap_or_default()
}

/// A loaded Python dump.
pub struct Dump {
    pub index: PageIndex,
    pub search: ReplaySearch,
    pub structure: WikiStructureSpec,
    pub repo_context: String,
    pub identity: WikiIdentity,
    pub repo_root: Option<PathBuf>,
    /// `summary.json`'s `page_answer`.
    pub page_answer: PageAnswer,
}

/// Load `dir` (see the module comment).
///
/// # Errors
///
/// A missing or malformed file.
pub fn load_dump(dir: &Path) -> Result<Dump, EngineError> {
    let nodes: Vec<IndexNode> = jsonl(&dir.join("nodes.jsonl"))?;
    let edges: Vec<IndexEdge> = jsonl(&dir.join("edges.jsonl"))?;
    let graph = json_file(&dir.join("graph.json"))?;
    let facts = GraphFacts {
        node_count: graph["node_count"]
            .as_u64()
            .and_then(|n| usize::try_from(n).ok())
            .unwrap_or(0),
        imports: string_map(&graph["imports"]),
        name_paths: string_map(&graph["name_paths"]),
    };
    let structure: WikiStructureSpec =
        serde_json::from_value(json_file(&dir.join("structure.json"))?)
            .map_err(|e| invalid(format!("structure.json: {e}")))?;
    let result = json_file(&dir.join("result.json"))?;
    let summary = json_file(&dir.join("summary.json"))?;
    let repository = summary["repository"].as_str().unwrap_or("").to_owned();
    let text = |key: &str| result[key].as_str().map(str::to_owned);
    Ok(Dump {
        index: PageIndex::new(nodes, edges, facts),
        search: ReplaySearch::from_jsonl(&read(&dir.join("fts.jsonl"))?)?,
        structure,
        repo_context: text("repository_context").unwrap_or_default(),
        identity: WikiIdentity {
            repository: repository.clone(),
            canonical_repository: repository,
            branch: text("branch").unwrap_or_else(|| "main".to_owned()),
            commit_hash: text("commit_hash"),
            provider_type: text("provider_type").unwrap_or_else(|| "github".to_owned()),
            query: "Document the repository".to_owned(),
        },
        // An absolute path is where the dump ran; a relative one (the
        // committed golden fixture) is relative to the dump directory.
        repo_root: summary["repo"].as_str().map(|repo| dir.join(repo)),
        page_answer: if summary["page_answer"] == "mermaid" {
            PageAnswer::Mermaid
        } else {
            PageAnswer::Stub
        },
    })
}

/// The chat client the Python worker built for the e2e `llm_settings`.
///
/// # Errors
///
/// Invalid settings or a TLS setup failure.
pub fn stub_chat(base_url: &str) -> Result<ChatClient, EngineError> {
    let settings = ModelSettings::from_llm_settings(&json!({
        "model_name": "gpt-4o", "api_base": base_url, "api_key": "mock", "max_tokens": 4000,
    }))?;
    Ok(ChatClient::new(
        Transport::new(&TransportSettings::default())?,
        settings,
    ))
}

/// Wall times of one parity run.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Timing {
    /// The page phase: split, drafts (four at a time), export, compose.
    pub page_seconds: f64,
    /// Context building alone over every page, sequentially.
    pub context_seconds: f64,
}

/// Run the page phase over a dump against `model`.
///
/// # Errors
///
/// A failure of the run (a replayed search missing is a failed page, not
/// an error).
pub async fn run_dump(
    dump: Dump,
    model: &StubModel,
) -> Result<(PagesOutcome, Timing), EngineError> {
    let generator = Arc::new(PageGenerator {
        retrieval: RetrievalContext {
            index: Arc::new(dump.index),
            search: Arc::new(dump.search),
            repo_root: dump.repo_root.clone(),
            flags: ExpansionFlags::from_env()?,
            budget: CONTEXT_TOKEN_BUDGET,
            pylon: PylonPlugin::new(dump.repo_root),
        },
        chat: stub_chat(&model.base_url)?,
        settings: PageSettings::new(dump.identity.repository.clone()),
        stop: StopSignal::default(),
        thinking: None,
    });
    let repo_context = dump.repo_context.clone();
    let started = std::time::Instant::now();
    let outcome = generate_wiki_pages(
        Arc::clone(&generator),
        dump.structure,
        &dump.repo_context,
        &dump.identity,
        &VersionClock::system(),
        started,
    )
    .await?;
    let page_seconds = started.elapsed().as_secs_f64();
    // Context building alone (retrieval, budget, formatting; no model),
    // a second pass over the split structure.
    let started = std::time::Instant::now();
    for page in outcome.structure.sections.iter().flat_map(|s| &s.pages) {
        generator.retrieval.relevant_content(page, &repo_context)?;
    }
    let timing = Timing {
        page_seconds,
        context_seconds: started.elapsed().as_secs_f64(),
    };
    Ok((outcome, timing))
}

/// Write the outcome as the dump's files.
///
/// # Errors
///
/// A write failure.
pub fn write_outcome(
    out: &Path,
    outcome: &PagesOutcome,
    requests: &[Value],
    timing: Timing,
) -> Result<(), EngineError> {
    let write = |name: &str, text: String| {
        std::fs::write(out.join(name), text).map_err(|e| invalid(format!("{name}: {e}")))
    };
    std::fs::create_dir_all(out).map_err(|e| invalid(format!("{}: {e}", out.display())))?;
    let pages: Vec<Value> = outcome
        .pages
        .pages
        .iter()
        .map(|p| json!({"page_id": p.page_id, "title": p.title, "content": p.content, "status": p.status.as_str()}))
        .collect();
    let mut doc = Map::new();
    doc.insert(
        "structure_after_split".into(),
        outcome
            .structure
            .to_value()
            .map_err(|e| invalid(e.to_string()))?,
    );
    doc.insert("pages".into(), Value::Array(pages));
    doc.insert("errors".into(), json!(outcome.pages.errors));
    write(
        "pages.json",
        pyjson::dumps_with(&Value::Object(doc), Some(2), false) + "\n",
    )?;
    write(
        "result.json",
        pyjson::dumps(&Value::Object(outcome.composed.result.clone())),
    )?;
    let mut lines = String::new();
    for body in requests {
        lines.push_str(&pyjson::dumps_with(body, None, false));
        lines.push('\n');
    }
    write("requests.jsonl", lines)?;
    write(
        "summary.json",
        pyjson::dumps_with(
            &json!({"pages": outcome.pages.pages.len(), "requests": requests.len(),
                "page_seconds": timing.page_seconds, "context_seconds": timing.context_seconds}),
            Some(2),
            false,
        ) + "\n",
    )?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_stub_routes_like_python() {
        // python3 -c "import sys; sys.path.insert(0,'services/elitea-deepwiki/e2e'); import llm_stub; print(llm_stub.answer('json')[:40])"
        assert!(
            stub_answer("give me json")
                .starts_with("{\"executive_summary\": \"A small notes service")
        );
        assert!(
            stub_answer("the wiki structure as JSON")
                .starts_with("{\"wiki_title\": \"notes-service\"")
        );
        assert!(stub_answer("write a page").starts_with("## Overview"));
    }
}
