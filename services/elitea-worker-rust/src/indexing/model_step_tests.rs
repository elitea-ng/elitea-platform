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
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
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
use super::pipeline::RunLiveness;
use super::tests::{
    Always, FIXTURE_SOURCE, Harness, MODEL, NS, binding, command, configuration, document, harness,
    parameters, stored,
};
use super::tools::ClaimIndexToolHost;
use super::{IndexRun, IndexRunFailure, run_index};
use crate::execution::ClaimLeaseError;
use crate::protocol::control::IndexRemovalBinding;
use crate::protocol::elitea::runtime::v1::{
    IndexIngestStatusV1, IndexIngestSummaryV1, IndexIngestTerminalStateV1,
};
use crate::transport::model_facade::ModelFacadeError;
use crate::transport::runtime_context::ClaimScopedEliteaContext;

/// What the scripted model answers for one call.
#[derive(Clone)]
pub(super) enum Reply {
    Text(String),
    /// A reply the model cut off at `max_tokens`.
    Cut(String),
    /// A reply the model declined to give (its safety filter).
    Declined,
    Fail(ErrorCategory, &'static str),
}

type Script = dyn Fn(usize, &str) -> Reply + Send + Sync;

/// An image part a request carried: (media type, bytes).
pub(super) type SeenImage = (String, Vec<u8>);

pub(super) struct ChatState {
    calls: AtomicUsize,
    script: Box<Script>,
    pub(super) binds: Mutex<Vec<(String, String)>>,
    /// Each request's image parts: (media type, bytes).
    pub(super) images: Mutex<Vec<Vec<SeenImage>>>,
    /// `bind_chat` refuses (an unusable model name).
    bind_fails: bool,
    /// Answers from the image a request carried instead of `script`: the
    /// reply, and how long the model takes to give it.
    by_image: Option<Box<ImageScript>>,
    in_flight: AtomicUsize,
    /// The most requests that were being answered at the same time.
    pub(super) max_in_flight: AtomicUsize,
}

type ImageScript = dyn Fn(&[SeenImage]) -> (u64, Reply) + Send + Sync;

/// A chat binder whose models answer from `script(call_index, user_text)`.
pub(super) struct ScriptedChat {
    pub(super) state: Arc<ChatState>,
}

impl ScriptedChat {
    pub(super) fn new(script: impl Fn(usize, &str) -> Reply + Send + Sync + 'static) -> Arc<Self> {
        Arc::new(Self {
            state: Arc::new(ChatState {
                calls: AtomicUsize::new(0),
                script: Box::new(script),
                binds: Mutex::new(Vec::new()),
                images: Mutex::new(Vec::new()),
                bind_fails: false,
                by_image: None,
                in_flight: AtomicUsize::new(0),
                max_in_flight: AtomicUsize::new(0),
            }),
        })
    }

    /// A binder whose models answer from the picture they were sent:
    /// `answer(images)` is the delay in milliseconds and the reply.
    pub(super) fn by_image(
        answer: impl Fn(&[SeenImage]) -> (u64, Reply) + Send + Sync + 'static,
    ) -> Arc<Self> {
        Arc::new(Self {
            state: Arc::new(ChatState {
                calls: AtomicUsize::new(0),
                script: Box::new(|_, _| Reply::Text(String::new())),
                binds: Mutex::new(Vec::new()),
                images: Mutex::new(Vec::new()),
                bind_fails: false,
                by_image: Some(Box::new(answer)),
                in_flight: AtomicUsize::new(0),
                max_in_flight: AtomicUsize::new(0),
            }),
        })
    }

    /// A binder that cannot bind any model.
    pub(super) fn unbindable() -> Arc<Self> {
        Arc::new(Self {
            state: Arc::new(ChatState {
                calls: AtomicUsize::new(0),
                script: Box::new(|_, _| Reply::Text(String::new())),
                binds: Mutex::new(Vec::new()),
                images: Mutex::new(Vec::new()),
                bind_fails: true,
                by_image: None,
                in_flight: AtomicUsize::new(0),
                max_in_flight: AtomicUsize::new(0),
            }),
        })
    }

    pub(super) fn calls(&self) -> usize {
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
        let images: Vec<SeenImage> = request
            .contents
            .iter()
            .flat_map(|content| content.parts.iter())
            .filter_map(|part| match part {
                adk_rust::Part::InlineData {
                    mime_type, data, ..
                } => Some((mime_type.clone(), data.clone())),
                _ => None,
            })
            .collect();
        self.state
            .images
            .lock()
            .expect("images")
            .push(images.clone());
        let reply_by_image = self.state.by_image.as_ref().map(|answer| answer(&images));
        let user = request
            .contents
            .first()
            .and_then(|content| content.parts.first())
            .and_then(|part| match part {
                adk_rust::Part::Text { text } => Some(text.clone()),
                _ => None,
            })
            .expect("a user message");
        let reply = if let Some((delay, reply)) = reply_by_image {
            let now = self.state.in_flight.fetch_add(1, Ordering::SeqCst) + 1;
            self.state.max_in_flight.fetch_max(now, Ordering::SeqCst);
            tokio::time::sleep(std::time::Duration::from_millis(delay)).await;
            self.state.in_flight.fetch_sub(1, Ordering::SeqCst);
            reply
        } else {
            (self.state.script)(index, &user)
        };
        match reply {
            Reply::Text(text) => Ok(Box::pin(stream::once(async move {
                Ok(LlmResponse {
                    content: Some(Content::new("model").with_text(text)),
                    turn_complete: true,
                    finish_reason: Some(FinishReason::Stop),
                    ..Default::default()
                })
            }))),
            Reply::Cut(text) => Ok(Box::pin(stream::once(async move {
                Ok(LlmResponse {
                    content: Some(Content::new("model").with_text(text)),
                    turn_complete: true,
                    finish_reason: Some(FinishReason::MaxTokens),
                    ..Default::default()
                })
            }))),
            Reply::Declined => Ok(Box::pin(stream::once(async move {
                Ok(LlmResponse {
                    content: Some(Content::new("model").with_text(String::new())),
                    turn_complete: true,
                    finish_reason: Some(FinishReason::Safety),
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
        if self.state.bind_fails {
            return Err(ModelFacadeError::InvalidInvocation);
        }
        Ok(Arc::new(ScriptedLlm {
            state: Arc::clone(&self.state),
        }))
    }
}

/// Installs `chat` as the runtime's chat model binder.
pub(super) fn with_chat(harness: &mut Harness, chat: &Arc<ScriptedChat>) {
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
pub(super) async fn run_model(
    harness: &Harness,
    documents: Vec<Document>,
    expected: Option<(&str, u32)>,
    parameters: &[u8],
    llm_model: Option<&str>,
) -> Result<IndexIngestSummaryV1, IndexRunFailure> {
    let model = llm_model.map(|name| json!(name).to_string().into_bytes());
    run_model_raw(
        harness,
        documents,
        expected,
        parameters,
        model.as_deref(),
        &Always,
    )
    .await
}

/// [`run_model`] with the `llm_model` entry as raw bytes and a lease.
pub(super) async fn run_model_raw(
    harness: &Harness,
    documents: Vec<Document>,
    expected: Option<(&str, u32)>,
    parameters: &[u8],
    model: Option<&[u8]>,
    lease: &dyn RunLiveness,
) -> Result<IndexIngestSummaryV1, IndexRunFailure> {
    FIXTURE_SOURCE.with(|slot| *slot.lock().expect("lock") = Some(documents));
    let now = || 1_700_000_000_000_i64;
    let settings = json!({"temperature": 0.5, "max_tokens": 2048, "top_p": 1}).to_string();
    run_index(
        &harness.runtime,
        IndexRun {
            command: &command(NS, expected),
            toolkit_configuration: &configuration(),
            tool_parameters: parameters,
            parsed: None,
            embedding_binding: Some(&binding()),
            llm_model: model,
            llm_configuration: model.map(|_| settings.as_bytes()),
            chat_entries: None,
            vector_token: Some(&harness.token),
            context: &harness.context,
            lease,
            deadline_unix_millis: i64::MAX,
            now_unix_millis: &now,
        },
    )
    .await
}

/// [`run_model_raw`] with the chat entries fetched on demand through
/// `entries` (as a production claim does) instead of handed over.
pub(super) async fn run_with_entries(
    harness: &Harness,
    documents: Vec<Document>,
    parameters: &[u8],
    entries: &dyn super::pipeline::ChatEntrySource,
) -> Result<IndexIngestSummaryV1, IndexRunFailure> {
    FIXTURE_SOURCE.with(|slot| *slot.lock().expect("lock") = Some(documents));
    let now = || 1_700_000_000_000_i64;
    run_index(
        &harness.runtime,
        IndexRun {
            command: &command(NS, None),
            toolkit_configuration: &configuration(),
            tool_parameters: parameters,
            parsed: None,
            embedding_binding: Some(&binding()),
            llm_model: None,
            llm_configuration: None,
            chat_entries: Some(entries),
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

pub(super) fn search_host(
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

pub(super) async fn search(host: &ClaimIndexToolHost, arguments: Value) -> Value {
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

pub(super) fn failed_with(summary: &IndexIngestSummaryV1) -> &str {
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

    // The statistical chunker's embeddings requests count too: two documents
    // of three requests each against a cap of four. The first is written;
    // the second fits the cap but not what remains, so the run stops.
    let mut harness = super::tests::harness(4, None).await;
    harness.runtime.limits.max_model_calls = 4;
    let text = "lorem ipsum dolor sit amet. ".repeat(140);
    let stat = parameters(
        false,
        json!({"chunking_tool": "statistical", "chunking_config": {"batch_size": 1}}),
    );
    let summary = run_model(
        &harness,
        vec![document("a.md", "v1", &text), document("b.md", "v1", &text)],
        None,
        &stat,
        None,
    )
    .await
    .expect("summary");
    let message = failed_with(&summary);
    assert!(message.contains("limit of 4 model calls"), "{message}");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_document_that_alone_exceeds_the_whole_cap_is_skipped_and_the_run_goes_on() {
    let mut harness = harness(4, None).await;
    harness.runtime.limits.max_model_calls = 2;
    let chat = ScriptedChat::new(food_dates);
    with_chat(&mut harness, &chat);
    // Three windows of 16 tokens do not fit a cap of two calls; the small
    // document after it does.
    let big = "alpha beta gamma delta epsilon zeta ".repeat(40);
    let params = parameters(
        false,
        json!({"chunking_tool": "proposal", "chunking_config": {"max_doc_tokens": 100}}),
    );
    let summary = run_model(
        &harness,
        vec![
            document("big.md", "v1", &big),
            document("small.md", "v1", "tiny text"),
        ],
        None,
        &params,
        Some("gpt-test"),
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.skipped.get("model_cap_exceeded"),
        Some(&1),
        "{summary:?}"
    );
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(chat.calls(), 1, "the oversized document made no call");
    assert_ne!(
        summary.terminal_state,
        IndexIngestTerminalStateV1::Failed as i32
    );

    // The statistical chunker: three requests against a cap of two.
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
    assert_eq!(
        summary.skipped.get("model_cap_exceeded"),
        Some(&1),
        "{summary:?}"
    );
    assert_eq!(harness.fake.point_count(), 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn retries_are_charged_against_the_cap_one_per_real_call() {
    // A flaky gateway: two rate limits, then an answer. Three real calls.
    let mut harness = harness(4, None).await;
    harness.runtime.limits.max_model_calls = 3;
    let chat = ScriptedChat::new(|index, _| {
        if index < 2 {
            Reply::Fail(ErrorCategory::RateLimited, "model_gateway.rate_limited")
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
    assert_eq!(chat.calls(), 3);

    // One call short of that, the third attempt is refused by the cap, not
    // made: the charged attempts equal the real calls.
    let mut harness = super::tests::harness(4, None).await;
    harness.runtime.limits.max_model_calls = 2;
    let chat = ScriptedChat::new(|index, _| {
        if index < 2 {
            Reply::Fail(ErrorCategory::RateLimited, "model_gateway.rate_limited")
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
    assert!(failed_with(&summary).contains("limit of 2 model calls"));
    assert_eq!(chat.calls(), 2);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_reply_cut_by_max_tokens_fails_the_run_naming_the_setting() {
    struct Truncated;
    #[async_trait]
    impl Llm for Truncated {
        fn name(&self) -> &'static str {
            "truncated"
        }
        async fn generate_content(
            &self,
            _: LlmRequest,
            _: bool,
        ) -> adk_rust::Result<LlmResponseStream> {
            Ok(Box::pin(stream::once(async {
                Ok(LlmResponse {
                    content: Some(Content::new("model").with_text("{\"chunks\": [")),
                    turn_complete: true,
                    finish_reason: Some(FinishReason::MaxTokens),
                    ..Default::default()
                })
            })))
        }
    }
    struct Binder(AtomicUsize);
    impl ChatModelBinder for Binder {
        fn bind_chat(
            &self,
            _: &ClaimScopedEliteaContext,
            _: &ChatModelRequest<'_>,
        ) -> Result<Arc<dyn Llm>, ModelFacadeError> {
            self.0.fetch_add(1, Ordering::SeqCst);
            Ok(Arc::new(Truncated))
        }
    }
    let mut harness = harness(4, None).await;
    let binder = Arc::new(Binder(AtomicUsize::new(0)));
    harness.runtime.chat_models = Some(Arc::clone(&binder) as Arc<dyn ChatModelBinder>);
    let summary = run_model(
        &harness,
        vec![document("a.md", "v1", GUIDE)],
        None,
        &proposal(),
        Some("gpt-test"),
    )
    .await
    .expect("summary");
    let message = failed_with(&summary);
    assert!(message.contains("max_tokens"), "{message}");
    assert!(message.contains("2048"), "the value in use: {message}");
    assert_eq!(binder.0.load(Ordering::SeqCst), 1, "not retried");
}

/// Reports Stop once `flag` is raised.
struct FlagLease(Arc<AtomicBool>);

impl RunLiveness for FlagLease {
    fn ensure_running(&self) -> Result<(), ClaimLeaseError> {
        if self.0.load(Ordering::SeqCst) {
            Err(ClaimLeaseError::Cancelled("stop requested"))
        } else {
            Ok(())
        }
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn stop_during_an_attempt_ends_the_retries() {
    let mut harness = harness(4, None).await;
    let flag = Arc::new(AtomicBool::new(false));
    let raise = Arc::clone(&flag);
    let chat = ScriptedChat::new(move |_, _| {
        raise.store(true, Ordering::SeqCst);
        Reply::Fail(ErrorCategory::Unavailable, "model_gateway.unavailable")
    });
    with_chat(&mut harness, &chat);
    let result = run_model_raw(
        &harness,
        vec![document("a.md", "v1", GUIDE)],
        None,
        &proposal(),
        Some(br#""gpt-test""#),
        &FlagLease(flag),
    )
    .await;
    assert!(
        matches!(result, Err(IndexRunFailure::Cancelled)),
        "{result:?}"
    );
    assert_eq!(chat.calls(), 1, "no further attempt after Stop");
}

#[tokio::test(flavor = "multi_thread")]
async fn the_chat_entries_are_read_only_when_a_chunker_calls_a_model() {
    let harness = harness(4, None).await;
    // Malformed `llm_model`, but the markdown chunker needs no model.
    let summary = run_model_raw(
        &harness,
        vec![document("a.md", "v1", "alpha text")],
        None,
        &parameters(false, json!({})),
        Some(b"{not a model name"),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");

    // The same entry when the proposal chunker is selected: refused clearly.
    let result = run_model_raw(
        &harness,
        vec![document("b.md", "v1", "alpha text")],
        None,
        &proposal(),
        Some(b"{not a model name"),
        &Always,
    )
    .await
    .expect("summary");
    let message = failed_with(&result);
    assert!(message.contains("chat model entries"), "{message}");
}

#[tokio::test(flavor = "multi_thread")]
async fn runs_without_a_proposal_chunker_are_not_failed_by_unreadable_chat_entries() {
    let harness = harness(4, None).await;
    let text = "lorem ipsum dolor sit amet. ".repeat(140);
    // A malformed `llm_model` and `llm_configuration`: only the proposal
    // chunker reads them.
    let settings_bad = b"[not settings";
    for config in [
        json!({"chunking_tool": "statistical"}),
        json!({"chunking_config": {".md": {"chunker": " Statistical "}}}),
        json!({"chunking_config": {"use_llm": true}}),
    ]
    .into_iter()
    .enumerate()
    {
        let (n, config) = config;
        FIXTURE_SOURCE.with(|slot| {
            *slot.lock().expect("lock") = Some(vec![document(&format!("s{n}.md"), "v1", &text)]);
        });
        let now = || 1_700_000_000_000_i64;
        let summary = run_index(
            &harness.runtime,
            IndexRun {
                command: &command(NS, None),
                toolkit_configuration: &configuration(),
                tool_parameters: &parameters(false, config.clone()),
                parsed: None,
                embedding_binding: Some(&binding()),
                llm_model: Some(b"{not a model name"),
                llm_configuration: Some(settings_bad),
                chat_entries: None,
                vector_token: Some(&harness.token),
                context: &harness.context,
                lease: &Always,
                deadline_unix_millis: i64::MAX,
                now_unix_millis: &now,
            },
        )
        .await
        .unwrap_or_else(|failure| panic!("{config}: {failure:?}"));
        // `use_llm` is the image-file step only: a markdown document is
        // indexed whatever the entries, and whatever `use_llm` says.
        let expected = 1_u64;
        assert_eq!(summary.indexed_documents, expected, "{config}: {summary:?}");
    }
    // The proposal chunker (any spelling) refuses the same entries clearly.
    let result = run_model_raw(
        &harness,
        vec![document("p.md", "v1", "alpha text")],
        None,
        &parameters(false, json!({"chunking_tool": " Proposal"})),
        Some(b"{not a model name"),
        &Always,
    )
    .await
    .expect("summary");
    let message = failed_with(&result);
    assert!(message.contains("chat model entries"), "{message}");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_statistical_success_resets_the_model_failure_streak() {
    let mut harness = harness(4, None).await;
    // The streak counts documents in the order their model steps run, which
    // with concurrent preparation is completion order: prepare one document
    // at a time so the listed order is the order the streak sees.
    harness.runtime.limits.document_concurrency = 1;
    let chat = ScriptedChat::new(|_, _| {
        Reply::Fail(ErrorCategory::Unavailable, "model_gateway.unavailable")
    });
    with_chat(&mut harness, &chat);
    let text = "lorem ipsum dolor sit amet. ".repeat(140);
    // Four failing proposal documents, a statistical success, four more
    // failures: nine failures, but never five in a row; a document with no
    // model step (c.json) between neither counts nor resets.
    let mut documents: Vec<Document> = (1..=4)
        .map(|n| document(&format!("a{n}.md"), "v1", GUIDE))
        .collect();
    documents.push(document("b.txt", "v1", &text));
    documents.push(document("bb.json", "v1", "{\"a\": 1}"));
    documents.extend((1..=4).map(|n| document(&format!("c{n}.md"), "v1", GUIDE)));
    let summary = run_model(
        &harness,
        documents,
        None,
        &parameters(
            false,
            json!({"chunking_config": {".md": {"chunker": "proposal"}, ".txt": {"chunker": "statistical"}}}),
        ),
        Some("gpt-test"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.skipped.get("model_failed"), Some(&8), "{summary:?}");
    assert_eq!(summary.indexed_documents, 2, "{summary:?}");
    assert_eq!(chat.calls(), 24, "eight documents, three attempts each");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_documents_windows_are_asked_together_and_kept_in_order() {
    let mut harness = harness(4, None).await;
    // Each window's answer echoes a marker taken from its own text.
    let chat = ScriptedChat::new(|_, user| {
        let marker = user
            .split_whitespace()
            .find(|w| w.starts_with("MARK"))
            .unwrap_or("none")
            .to_owned();
        Reply::Text(analysis(&[(&marker, "s", &["p"])]))
    });
    with_chat(&mut harness, &chat);
    let mut body = String::new();
    for n in 0..6 {
        body.push_str(&["MARK", &n.to_string(), " "].concat());
        body.push_str(&"filler word ".repeat(60));
    }
    let params = parameters(
        false,
        json!({"chunking_tool": "proposal", "chunking_config": {"max_doc_tokens": 130}}),
    );
    let summary = run_model(
        &harness,
        vec![document("a.md", "v1", &body)],
        None,
        &params,
        Some("gpt-test"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    let windows = chat.calls();
    assert!(windows >= 5, "{windows}");
    let Harness {
        runtime,
        token,
        _server,
        ..
    } = harness;
    let runtime = Arc::new(runtime);
    let host = search_host(&runtime, &token);
    // A window's chunk_id is its position, whatever order the answers came
    // back in; each window's text carries its own marker, and that marker
    // is the title the model returned for it.
    for id in 1..=windows {
        let found = search(
            &host,
            json!({"query": "MARK", "index_name": "docs", "cut_off": 0,
                   "filter": {"chunk_type": "document", "chunk_id": id.to_string()},
                   "output_fields": ["page_content"]}),
        )
        .await;
        let text = found[0]["page_content"]
            .as_str()
            .unwrap_or_default()
            .to_owned();
        let titled = search(
            &host,
            json!({"query": "MARK", "index_name": "docs", "cut_off": 0,
                   "filter": {"chunk_type": "title", "chunk_id": id.to_string()},
                   "output_fields": ["page_content"]}),
        )
        .await;
        let title = titled[0]["page_content"]
            .as_str()
            .unwrap_or_default()
            .to_owned();
        if text.contains("MARK") {
            let first = text
                .split_whitespace()
                .find(|w| w.starts_with("MARK"))
                .unwrap();
            assert_eq!(title, first, "window {id}");
        }
    }
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
