//! The model steps of an index run (ADR-0030 I4): the proposal chunker's
//! chat calls (a scripted `Llm` standing in for the model) and the
//! statistical chunker's embeddings (the scripted embeddings gateway of the
//! pipeline tests), against the in-process fake of `elitea-vector`.
//!
//! What they pin: the structural output (four chunk types per entry, stored
//! and searchable through `extended_search`), the failure policy (no model:
//! `no_model`; a retryable failure: `model_failed` and the stored version
//! untouched, with a breaker; a refusal: the run FAILED with the gateway's
//! code; the call cap), and that an unchanged document never calls a model.

use std::collections::BTreeSet;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use adk_rust::futures::stream;
use adk_rust::{
    AdkError, Content, ErrorCategory, ErrorComponent, FinishReason, Llm, LlmRequest, LlmResponse,
    LlmResponseStream,
};
use async_trait::async_trait;
use elitea_agent_runtime::toolkits::{
    IndexCatalog, IndexCatalogEntry, IndexToolCall, IndexToolHost, IndexToolName,
    SharedIndexCatalog,
};
use elitea_content_source::Document;
use serde_json::{Value, json};

use super::model_steps::{ChatModelBinder, ChatModelRequest};
use super::tests::{
    Always, FIXTURE_SOURCE, Harness, MODEL, NS, binding, command, configuration, document, harness,
    parameters, stored,
};
use super::tools::ClaimIndexToolHost;
use super::{IndexRun, IndexRunFailure, run_index};
use crate::protocol::control::IndexRemovalBinding;
use crate::protocol::elitea::runtime::v1::{
    IndexIngestStatusV1, IndexIngestSummaryV1, IndexIngestTerminalStateV1,
};
use crate::transport::model_facade::ModelFacadeError;
use crate::transport::runtime_context::ClaimScopedEliteaContext;

/// What the scripted model answers for one call.
#[derive(Clone)]
enum Reply {
    Text(String),
    Fail(ErrorCategory, &'static str),
}

type Script = dyn Fn(usize, &str) -> Reply + Send + Sync;

struct ChatState {
    calls: AtomicUsize,
    script: Box<Script>,
    binds: Mutex<Vec<(String, String)>>,
}

/// A chat binder whose models answer from `script(call_index, user_text)`.
struct ScriptedChat {
    state: Arc<ChatState>,
}

impl ScriptedChat {
    fn new(script: impl Fn(usize, &str) -> Reply + Send + Sync + 'static) -> Arc<Self> {
        Arc::new(Self {
            state: Arc::new(ChatState {
                calls: AtomicUsize::new(0),
                script: Box::new(script),
                binds: Mutex::new(Vec::new()),
            }),
        })
    }

    fn calls(&self) -> usize {
        self.state.calls.load(Ordering::SeqCst)
    }
}

struct ScriptedLlm {
    state: Arc<ChatState>,
}

#[async_trait]
impl Llm for ScriptedLlm {
    fn name(&self) -> &'static str {
        "scripted-chat"
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        streaming: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        assert!(!streaming, "the step reads one complete reply");
        let index = self.state.calls.fetch_add(1, Ordering::SeqCst);
        let user = request
            .contents
            .first()
            .and_then(|content| content.parts.first())
            .and_then(|part| match part {
                adk_rust::Part::Text { text } => Some(text.clone()),
                _ => None,
            })
            .expect("a user message");
        match (self.state.script)(index, &user) {
            Reply::Text(text) => Ok(Box::pin(stream::once(async move {
                Ok(LlmResponse {
                    content: Some(Content::new("model").with_text(text)),
                    turn_complete: true,
                    finish_reason: Some(FinishReason::Stop),
                    ..Default::default()
                })
            }))),
            Reply::Fail(category, code) => Err(AdkError::new(
                ErrorComponent::Model,
                category,
                code,
                "scripted failure",
            )),
        }
    }
}

impl ChatModelBinder for ScriptedChat {
    fn bind_chat(
        &self,
        _context: &ClaimScopedEliteaContext,
        request: &ChatModelRequest<'_>,
    ) -> Result<Arc<dyn Llm>, ModelFacadeError> {
        self.state.binds.lock().expect("binds").push((
            request.model_name.to_owned(),
            request.system_instruction.to_owned(),
        ));
        Ok(Arc::new(ScriptedLlm {
            state: Arc::clone(&self.state),
        }))
    }
}

/// Installs `chat` as the runtime's chat model binder.
fn with_chat(harness: &mut Harness, chat: &Arc<ScriptedChat>) {
    harness.runtime.chat_models = Some(Arc::clone(chat) as Arc<dyn ChatModelBinder>);
}

fn analysis(entries: &[(&str, &str, &[&str])]) -> String {
    json!({"chunks": entries.iter().map(|(title, summary, propositions)| json!({
        "chunk_title": title, "chunk_summary": summary, "propositions": propositions,
    })).collect::<Vec<_>>()})
    .to_string()
}

fn food_dates(_: usize, _: &str) -> Reply {
    Reply::Text(analysis(&[
        (
            "Food",
            "About food.",
            &["Greg likes pizza.", "Pizza is food."],
        ),
        ("Dates", "About dates.", &["It is May."]),
    ]))
}

/// One run with a chat model (`llm_model`, a JSON string) or none.
async fn run_model(
    harness: &Harness,
    documents: Vec<Document>,
    expected: Option<(&str, u32)>,
    parameters: &[u8],
    llm_model: Option<&str>,
) -> Result<IndexIngestSummaryV1, IndexRunFailure> {
    FIXTURE_SOURCE.with(|slot| *slot.lock().expect("lock") = Some(documents));
    let now = || 1_700_000_000_000_i64;
    let model = llm_model.map(|name| json!(name).to_string().into_bytes());
    let settings = json!({"temperature": 0.5, "max_tokens": 2048, "top_p": 1}).to_string();
    run_index(
        &harness.runtime,
        IndexRun {
            command: &command(NS, expected),
            toolkit_configuration: &configuration(),
            tool_parameters: parameters,
            embedding_binding: Some(&binding()),
            llm_model: model.as_deref(),
            llm_configuration: model.as_ref().map(|_| settings.as_bytes()),
            vector_token: Some(&harness.token),
            context: &harness.context,
            lease: &Always,
            deadline_unix_millis: i64::MAX,
            now_unix_millis: &now,
        },
    )
    .await
}

fn proposal() -> Vec<u8> {
    parameters(false, json!({"chunking_tool": "proposal"}))
}

fn search_host(
    runtime: &Arc<super::IndexIngestRuntime>,
    token: &crate::protocol::vector_token::VectorClaimToken,
) -> ClaimIndexToolHost {
    ClaimIndexToolHost::new(
        Arc::clone(runtime),
        token,
        Arc::new(ClaimScopedEliteaContext::fixture(1, "pat")),
        None,
        IndexRemovalBinding::fixture(),
    )
}

async fn search(host: &ClaimIndexToolHost, arguments: Value) -> Value {
    let catalog = SharedIndexCatalog::new(IndexCatalog::new(vec![IndexCatalogEntry {
        name: "docs".to_owned(),
        index_id: NS.to_owned(),
        embedding_model: MODEL.to_owned(),
        embedding_dimension: 4,
    }]));
    host.call(IndexToolCall {
        tool: IndexToolName::SearchIndex,
        toolkit_id: 41,
        catalog: &catalog,
        doctype: "document",
        arguments: &arguments,
    })
    .await
    .unwrap_or_else(|failure| Value::String(failure.message().to_owned()))
}

const GUIDE: &str = "Greg likes pizza. Pizza is food. It is May, and the month is warm.";

#[tokio::test(flavor = "multi_thread")]
async fn the_proposal_chunker_stores_four_chunks_per_entry_and_extended_search_finds_the_document_chunk()
 {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(food_dates);
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![document("guide.md", "v1", GUIDE)],
        None,
        &proposal(),
        Some("gpt-test"),
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Ok as i32,
        "{summary:?}"
    );
    assert_eq!(summary.indexed_documents, 1);
    // Two entries, four chunks each.
    assert_eq!(summary.indexed_chunks, 8);
    assert_eq!(harness.fake.point_count(), 8);
    assert_eq!(chat.calls(), 1, "one window, one call");
    let binds = chat.state.binds.lock().expect("binds").clone();
    assert_eq!(binds[0].0, "gpt-test");
    assert!(binds[0].1.contains("steward of a group of chunks"));

    let Harness {
        runtime,
        token,
        fake,
        _server,
        ..
    } = harness;
    assert_eq!(fake.point_count(), 8);
    let runtime = Arc::new(runtime);
    let host = search_host(&runtime, &token);
    // Each sibling is stored under its own type with the entry's chunk id.
    for (chunk_type, expected) in [
        ("title", "Food"),
        ("summary", "About food."),
        ("propositions", "Greg likes pizza.\nPizza is food."),
    ] {
        let found = search(
            &host,
            json!({"query": expected, "index_name": "docs", "cut_off": 0,
                   "filter": {"chunk_type": chunk_type, "chunk_id": "1"},
                   "output_fields": ["page_content", "metadata"]}),
        )
        .await;
        let hits = found.as_array().unwrap_or_else(|| panic!("{found}"));
        assert_eq!(hits.len(), 1, "{chunk_type}: {found}");
        assert_eq!(hits[0]["page_content"], expected);
        assert_eq!(hits[0]["metadata"]["chunk_type"], chunk_type);
        assert_eq!(hits[0]["metadata"]["chunk_id"], 1);
    }
    // The document chunk carries the entry's title and summary.
    let documents = search(
        &host,
        json!({"query": "Dates", "index_name": "docs", "cut_off": 0,
               "filter": {"chunk_type": "document", "chunk_id": "2"},
               "output_fields": ["page_content", "metadata"]}),
    )
    .await;
    assert_eq!(documents[0]["page_content"], GUIDE);
    assert_eq!(documents[0]["metadata"]["chunk_title"], "Dates");
    assert_eq!(documents[0]["metadata"]["chunk_summary"], "About dates.");
    assert_eq!(documents[0]["metadata"]["source"], "guide.md");
    // A search over the titles returns the document chunk of the entry whose
    // title matched.
    let extended = search(
        &host,
        json!({"query": "Dates", "index_name": "docs", "cut_off": 0,
               "extended_search": ["title"],
               "output_fields": ["page_content", "metadata"]}),
    )
    .await;
    let hits = extended.as_array().unwrap_or_else(|| panic!("{extended}"));
    assert!(
        hits.iter().all(|hit| hit["page_content"] == GUIDE),
        "only document chunks come back: {extended}"
    );
    let ids: BTreeSet<String> = hits
        .iter()
        .map(|hit| {
            hit["metadata"]["chunk_title"]
                .as_str()
                .unwrap_or("")
                .to_owned()
        })
        .collect();
    assert!(ids.contains("Dates"), "{extended}");
}

fn failed_with(summary: &IndexIngestSummaryV1) -> &str {
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Error as i32,
        "{summary:?}"
    );
    assert_eq!(
        summary.terminal_state,
        IndexIngestTerminalStateV1::Failed as i32
    );
    &summary.message
}

#[tokio::test(flavor = "multi_thread")]
async fn windows_are_numbered_on_across_a_long_document() {
    let mut harness = harness(4, None).await;
    let chat =
        ScriptedChat::new(|index, _| Reply::Text(analysis(&[(&format!("W{index}"), "S", &["p"])])));
    with_chat(&mut harness, &chat);
    let long = "word ".repeat(2100);
    let summary = run_model(
        &harness,
        vec![document("long.txt", "v1", &long)],
        None,
        &proposal(),
        Some("gpt-test"),
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Ok as i32,
        "{summary:?}"
    );
    assert_eq!(chat.calls(), 3, "2100 tokens are three 1024-token windows");
    assert_eq!(summary.indexed_chunks, 12);
    let Harness {
        runtime,
        token,
        _server,
        ..
    } = harness;
    let runtime = Arc::new(runtime);
    let host = search_host(&runtime, &token);
    let third = search(
        &host,
        json!({"query": "W2", "index_name": "docs", "cut_off": 0,
               "filter": {"chunk_type": "title", "chunk_id": "3"},
               "output_fields": ["page_content"]}),
    )
    .await;
    assert_eq!(third[0]["page_content"], "W2", "{third}");
}

#[tokio::test(flavor = "multi_thread")]
async fn without_a_chat_model_proposal_documents_skip_no_model_but_statistical_ones_index() {
    // No binder in the runtime.
    let harness = harness(4, None).await;
    let summary = run_model(
        &harness,
        vec![document("a.md", "v1", GUIDE)],
        None,
        &proposal(),
        Some("gpt-test"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.skipped.get("no_model"), Some(&1), "{summary:?}");
    assert_eq!(summary.indexed_documents, 0);

    // A binder, but the claim names no model.
    let mut harness = super::tests::harness(4, None).await;
    let chat = ScriptedChat::new(food_dates);
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![document("a.md", "v1", GUIDE)],
        None,
        &proposal(),
        None,
    )
    .await
    .expect("summary");
    assert_eq!(summary.skipped.get("no_model"), Some(&1), "{summary:?}");
    assert_eq!(chat.calls(), 0);

    // The statistical chunker needs only the index's embedding model.
    let text = "lorem ipsum dolor sit amet. ".repeat(140);
    let before = harness.embeddings.calls.load(Ordering::SeqCst);
    let summary = run_model(
        &harness,
        vec![document("stat.md", "v1", &text)],
        None,
        &parameters(false, json!({"chunking_tool": "statistical"})),
        None,
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Ok as i32,
        "{summary:?}"
    );
    assert_eq!(summary.indexed_documents, 1);
    assert!(summary.indexed_chunks >= 1);
    assert!(
        harness.embeddings.calls.load(Ordering::SeqCst) >= before + 2,
        "the splits were embedded, then the chunks"
    );
    assert_eq!(chat.calls(), 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_refusal_the_model_cannot_cure_fails_the_run_with_the_gateways_code() {
    let mut harness = harness(4, None).await;
    let chat =
        ScriptedChat::new(|_, _| Reply::Fail(ErrorCategory::Forbidden, "model_gateway.forbidden"));
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![document("a.md", "v1", GUIDE), document("b.md", "v1", GUIDE)],
        None,
        &proposal(),
        Some("gpt-test"),
    )
    .await
    .expect("summary");
    let message = failed_with(&summary);
    assert!(message.contains("model_gateway.forbidden"), "{message}");
    assert_eq!(chat.calls(), 1, "a refusal is not retried and ends the run");
    assert_eq!(harness.fake.point_count(), 0);
    assert_eq!(summary.indexed_documents, 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_retryable_failure_skips_the_document_and_keeps_its_stored_version() {
    let mut harness = harness(4, None).await;
    let first = run_model(
        &harness,
        vec![document("a.md", "v1", "alpha text one")],
        None,
        &parameters(false, json!({})),
        None,
    )
    .await
    .expect("first");
    assert_eq!(first.indexed_documents, 1);
    let chat = ScriptedChat::new(|_, _| {
        Reply::Fail(ErrorCategory::RateLimited, "model_gateway.rate_limited")
    });
    with_chat(&mut harness, &chat);
    let second = run_model(
        &harness,
        vec![document("a.md", "v2", GUIDE)],
        Some((MODEL, 4)),
        &proposal(),
        Some("gpt-test"),
    )
    .await
    .expect("second");
    assert_eq!(second.skipped.get("model_failed"), Some(&1), "{second:?}");
    assert_eq!(second.status, IndexIngestStatusV1::PartlyIndexed as i32);
    assert_eq!(
        chat.calls(),
        3,
        "three attempts, then the document is skipped"
    );
    let kept = stored(&harness).await;
    assert_eq!(kept.len(), 1);
    assert_eq!(
        kept[0].document_version, "v1",
        "the stored version is untouched"
    );
    assert_eq!(second.total_documents, Some(1));
}

#[tokio::test(flavor = "multi_thread")]
async fn documents_in_a_row_failing_for_the_model_stop_the_run() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(|_, _| {
        Reply::Fail(ErrorCategory::Unavailable, "model_gateway.unavailable")
    });
    with_chat(&mut harness, &chat);
    let documents: Vec<Document> = (0..8)
        .map(|n| document(&format!("d{n}.md"), "v1", GUIDE))
        .collect();
    let summary = run_model(&harness, documents, None, &proposal(), Some("gpt-test"))
        .await
        .expect("summary");
    let message = failed_with(&summary);
    assert!(message.contains("5 documents in a row"), "{message}");
    // The store was never touched, so the failure carries no counts.
    assert_eq!(harness.fake.point_count(), 0);
    assert_eq!(chat.calls(), 15, "five documents, three attempts each");
}

#[tokio::test(flavor = "multi_thread")]
async fn an_unreadable_answer_is_asked_again_and_then_skips_the_document() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(|index, _| {
        if index == 0 {
            Reply::Text("I cannot do that".to_owned())
        } else {
            food_dates(index, "")
        }
    });
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![document("a.md", "v1", GUIDE)],
        None,
        &proposal(),
        Some("gpt-test"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(chat.calls(), 2);

    let mut harness = super::tests::harness(4, None).await;
    let chat = ScriptedChat::new(|_, _| Reply::Text("{\"chunks\": []}".to_owned()));
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![document("b.md", "v1", GUIDE)],
        None,
        &proposal(),
        Some("gpt-test"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.skipped.get("model_failed"), Some(&1), "{summary:?}");
    assert_eq!(chat.calls(), 3);
    assert_eq!(
        harness.fake.point_count(),
        0,
        "a window is never dropped silently"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn the_per_run_model_call_cap_stops_the_run_with_a_sentence() {
    let mut harness = harness(4, None).await;
    harness.runtime.limits.max_model_calls = 2;
    let chat = ScriptedChat::new(food_dates);
    with_chat(&mut harness, &chat);
    let documents: Vec<Document> = (0..3)
        .map(|n| document(&format!("d{n}.md"), "v1", GUIDE))
        .collect();
    let summary = run_model(&harness, documents, None, &proposal(), Some("gpt-test"))
        .await
        .expect("summary");
    let message = failed_with(&summary);
    assert!(message.contains("limit of 2 model calls"), "{message}");
    assert_eq!(chat.calls(), 2);
    assert_eq!(
        summary.indexed_documents, 2,
        "the documents already written stay"
    );
    assert_eq!(harness.fake.point_count(), 16);

    // The statistical chunker's embeddings requests count too.
    let mut harness = super::tests::harness(4, None).await;
    harness.runtime.limits.max_model_calls = 2;
    let text = "lorem ipsum dolor sit amet. ".repeat(140);
    let summary = run_model(
        &harness,
        vec![document("stat.md", "v1", &text)],
        None,
        &parameters(
            false,
            json!({"chunking_tool": "statistical", "chunking_config": {"batch_size": 1}}),
        ),
        None,
    )
    .await
    .expect("summary");
    let message = failed_with(&summary);
    assert!(message.contains("limit of 2 model calls"), "{message}");
}

#[tokio::test(flavor = "multi_thread")]
async fn an_unchanged_document_never_calls_the_model_and_a_changed_one_replaces_its_points() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(|index, _| {
        if index == 0 {
            food_dates(index, "")
        } else {
            Reply::Text(analysis(&[("Only", "One.", &["p"])]))
        }
    });
    with_chat(&mut harness, &chat);
    let first = run_model(
        &harness,
        vec![document("guide.md", "v1", GUIDE)],
        None,
        &proposal(),
        Some("gpt-test"),
    )
    .await
    .expect("first");
    assert_eq!(first.indexed_chunks, 8);
    assert_eq!(chat.calls(), 1);

    let second = run_model(
        &harness,
        vec![document("guide.md", "v1", GUIDE)],
        Some((MODEL, 4)),
        &proposal(),
        Some("gpt-test"),
    )
    .await
    .expect("second");
    assert_eq!(second.indexed_documents, 0, "{second:?}");
    assert_eq!(
        chat.calls(),
        1,
        "an unchanged version is not sent to the model"
    );

    let third = run_model(
        &harness,
        vec![document("guide.md", "v2", "Only one thing is said here.")],
        Some((MODEL, 4)),
        &proposal(),
        Some("gpt-test"),
    )
    .await
    .expect("third");
    assert_eq!(third.updated, 1, "{third:?}");
    assert_eq!(chat.calls(), 2);
    let kept = stored(&harness).await;
    assert_eq!(kept.len(), 1);
    assert_eq!(kept[0].document_version, "v2");
    assert_eq!(kept[0].chunk_count, 4);
    assert_eq!(harness.fake.point_count(), 4, "the old points are gone");
}
