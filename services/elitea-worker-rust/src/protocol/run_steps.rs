//! Step records of one claimed execution (ADR-0042 decision 1, #1499).
//!
//! The runtime records a step for every model call, tool call, retrieval,
//! approval, delegation, cap, run-ending error and final output into the
//! command's one [`RunStepRecorder`]. The recorder bounds every step as the
//! contract states (content 256 KiB per side cut at a UTF-8 boundary, a
//! summary of at most 8 KiB, at most 100 retrieved documents) and buffers it
//! in the order it happened. `AgentControlClient` sends the buffer under the
//! live claim with `ReportRunSteps`: early, once 64 steps are waiting, and
//! completely before `PrepareSettlement`. Main assigns the sequence and chain
//! hash in arrival order, so one recorder's reports are serialized and a
//! batch leaves the buffer only once Main accepted it (or it must be dropped:
//! that is logged with the execution id and counted, never silent).
//!
//! Like the egress ledger, recorders live in a process registry keyed by
//! command (execution, generation, command id): the claim path binds the
//! fence into it, the lifecycle and the projector record into it, and the
//! settlement path drains and forgets it.

use std::collections::{HashMap, VecDeque};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use prost::Message as _;
use serde_json::{Map, Value, json};

use super::elitea::runtime::v1::{
    ExecutionFenceV1, ExecutionIdentityV1, RetrievalSummaryV1, RetrievedDocumentV1,
    RunStepContentV1, RunStepKindV1, RunStepV1,
};

/// At most this many steps ride one `ReportRunSteps` request.
pub(crate) const MAX_STEPS_PER_REPORT: usize = 64;
/// Each side (inputs, outputs) of a step carries at most this much content.
pub(crate) const MAX_STEP_CONTENT_BYTES: usize = 256 * 1024;
/// A step's `summary_json` is at most this long.
pub(crate) const MAX_STEP_SUMMARY_BYTES: usize = 8 * 1024;
/// A retrieval step lists at most this many documents.
pub(crate) const MAX_RETRIEVED_DOCUMENTS: usize = 100;
/// One request's encoded size. Every bounded step fits (two content sides,
/// the summary and a full retrieval stay well under it), so a batch is cut
/// by count or by this, whichever comes first. Main's control codec admits
/// this size for `ReportRunStepsRequestV1`.
pub(crate) const MAX_REPORT_BYTES: usize = 1024 * 1024;
/// Steps one execution may hold unsent. Early flushing keeps the buffer near
/// 64; only a Main that stays unreachable for a long run fills it.
const MAX_BUFFERED_STEPS: usize = 4_096;
const MAX_NAME_BYTES: usize = 256;
const MAX_ID_BYTES: usize = 128;
const MAX_QUERY_BYTES: usize = 8 * 1024;
const MAX_FILTERS_BYTES: usize = 8 * 1024;
const MAX_DOCUMENT_ID_BYTES: usize = 512;
const MAX_SOURCE_BYTES: usize = 2 * 1024;
/// A summary string value longer than this is cut when the summary must
/// shrink to fit [`MAX_STEP_SUMMARY_BYTES`].
const MAX_SUMMARY_STRING_BYTES: usize = 512;
/// A document id or source shorter than this is too generic to count as a
/// citation (`"1"` appears in nearly every answer).
const MIN_CITATION_NEEDLE_CHARS: usize = 4;
/// Room kept for the `summary_truncated` marker.
const SUMMARY_MARKER_BYTES: usize = 32;
/// A buffered step older than this is reported early, before a full batch
/// waits: a worker that dies mid-run then loses at most this much.
pub(crate) const MAX_UNSENT_STEP_AGE: Duration = Duration::from_secs(2);
/// After a failed early report the next one waits this long, doubling per
/// consecutive failure up to [`MAX_EARLY_RETRY_DELAY`], so an unavailable
/// Main costs the run one bounded attempt per window, not one per step.
const EARLY_RETRY_DELAY: Duration = Duration::from_secs(2);
const MAX_EARLY_RETRY_DELAY: Duration = Duration::from_secs(30);
/// The most executions the registry holds; past it the oldest is dropped.
const MAX_TRACKED_EXECUTIONS: usize = 4_096;
/// An execution not settled within this is dropped with its unsent steps.
const MAX_TRACKED_AGE: Duration = Duration::from_hours(12);

/// Process-wide count of steps that were dropped instead of reported (a
/// non-retryable rejection, a Main that does not serve the RPC, retries
/// exhausted at settlement, a full buffer or an evicted execution). The crate
/// has no metrics exporter; the count rides every drop's WARN event and is
/// readable by tests.
static DROPPED_TOTAL: AtomicU64 = AtomicU64::new(0);
/// Process-wide count of `ReportRunSteps` attempts Main rejected or the
/// transport failed.
static REPORT_FAILURES_TOTAL: AtomicU64 = AtomicU64::new(0);

#[cfg(test)]
#[must_use]
pub(crate) fn dropped_run_steps_total() -> u64 {
    DROPPED_TOTAL.load(Ordering::Relaxed)
}

pub(crate) fn count_report_failure() -> u64 {
    REPORT_FAILURES_TOTAL.fetch_add(1, Ordering::Relaxed) + 1
}

#[cfg(test)]
#[must_use]
pub(crate) fn run_step_report_failures_total() -> u64 {
    REPORT_FAILURES_TOTAL.load(Ordering::Relaxed)
}

/// One side of a step before it is bounded.
pub(crate) struct StepText {
    text: String,
    media_type: &'static str,
}

impl StepText {
    pub(crate) fn plain(text: impl Into<String>) -> Self {
        Self {
            text: text.into(),
            media_type: "text/plain",
        }
    }

    /// A JSON value; a JSON string is stored as its text.
    pub(crate) fn json(value: &Value) -> Self {
        match value {
            Value::String(text) => Self::plain(text.clone()),
            other => Self {
                text: other.to_string(),
                media_type: "application/json",
            },
        }
    }
}

/// One hit a retrieval returned, before it is bounded.
#[derive(Clone, Debug, PartialEq)]
pub(crate) struct RetrievedDocument {
    pub(crate) id: String,
    pub(crate) source: String,
    pub(crate) score: f64,
}

/// What a retrieval searched and got back, before it is bounded.
#[derive(Clone, Debug, Default, PartialEq)]
pub(crate) struct RetrievalDraft {
    pub(crate) index_name: String,
    pub(crate) query: String,
    pub(crate) filters: Option<Value>,
    pub(crate) top_k: u32,
    pub(crate) documents: Vec<RetrievedDocument>,
}

/// One step as the runtime describes it; [`RunStepRecorder::record`] bounds
/// it and gives it its id.
pub(crate) struct StepDraft {
    pub(crate) kind: RunStepKindV1,
    pub(crate) name: String,
    pub(crate) node_id: String,
    pub(crate) toolkit_id: String,
    pub(crate) started_at_unix_ms: i64,
    pub(crate) ended_at_unix_ms: i64,
    pub(crate) summary: Map<String, Value>,
    pub(crate) inputs: Option<StepText>,
    pub(crate) outputs: Option<StepText>,
    pub(crate) retrieval: Option<RetrievalDraft>,
}

impl StepDraft {
    pub(crate) fn new(kind: RunStepKindV1, name: impl Into<String>, at_unix_ms: i64) -> Self {
        Self {
            kind,
            name: name.into(),
            node_id: String::new(),
            toolkit_id: String::new(),
            started_at_unix_ms: at_unix_ms,
            ended_at_unix_ms: at_unix_ms,
            summary: Map::new(),
            inputs: None,
            outputs: None,
            retrieval: None,
        }
    }

    #[must_use]
    pub(crate) fn summary(mut self, key: &str, value: Value) -> Self {
        if !value.is_null() {
            self.summary.insert(key.to_owned(), value);
        }
        self
    }
}

/// The text cut to at most `max` bytes at a UTF-8 boundary, and whether it
/// was cut.
#[must_use]
pub(crate) fn cut_utf8(text: &str, max: usize) -> (&str, bool) {
    if text.len() <= max {
        return (text, false);
    }
    let mut end = max;
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    (&text[..end], true)
}

fn bounded(text: &str, max: usize) -> String {
    cut_utf8(text, max).0.to_owned()
}

fn bounded_owned(mut text: String, max: usize) -> String {
    let kept = cut_utf8(&text, max).0.len();
    text.truncate(kept);
    text
}

/// One content side as the contract carries it.
#[must_use]
pub(crate) fn step_content(side: &StepText) -> RunStepContentV1 {
    let (kept, truncated) = cut_utf8(&side.text, MAX_STEP_CONTENT_BYTES);
    RunStepContentV1 {
        content: kept.as_bytes().to_vec(),
        // A cut JSON document is no longer JSON.
        media_type: if truncated {
            "text/plain".to_owned()
        } else {
            side.media_type.to_owned()
        },
        truncated,
        original_size_bytes: i64::try_from(side.text.len()).unwrap_or(i64::MAX),
    }
}

/// The summary as a JSON object of at most [`MAX_STEP_SUMMARY_BYTES`]. One
/// that is too long keeps its keys in order while they fit (long strings cut
/// first) and says `"summary_truncated": true`.
#[must_use]
pub(crate) fn bounded_summary(summary: &Map<String, Value>) -> String {
    let whole = Value::Object(summary.clone()).to_string();
    if whole.len() <= MAX_STEP_SUMMARY_BYTES {
        return whole;
    }
    let budget = MAX_STEP_SUMMARY_BYTES - SUMMARY_MARKER_BYTES;
    let mut kept = Map::new();
    for (key, value) in summary {
        let value = match value {
            Value::String(text) => Value::String(bounded(text, MAX_SUMMARY_STRING_BYTES)),
            other => other.clone(),
        };
        kept.insert(key.clone(), value);
        if Value::Object(kept.clone()).to_string().len() > budget {
            kept.remove(key);
        }
    }
    kept.insert("summary_truncated".to_owned(), Value::Bool(true));
    Value::Object(kept).to_string()
}

fn retrieval_summary(draft: &RetrievalDraft) -> RetrievalSummaryV1 {
    let filters_json = match &draft.filters {
        None | Some(Value::Null) => String::new(),
        Some(Value::Object(object)) if object.is_empty() => String::new(),
        Some(Value::Object(object)) => {
            let text = Value::Object(object.clone()).to_string();
            if text.len() <= MAX_FILTERS_BYTES {
                text
            } else {
                json!({"filters_truncated": true}).to_string()
            }
        }
        // A filter given as a string or another shape is kept as the JSON
        // object the contract asks for.
        Some(other) => {
            let text = json!({"filter": other}).to_string();
            if text.len() <= MAX_FILTERS_BYTES {
                text
            } else {
                json!({"filters_truncated": true}).to_string()
            }
        }
    };
    RetrievalSummaryV1 {
        index_name: bounded(&draft.index_name, MAX_NAME_BYTES),
        query: bounded(&draft.query, MAX_QUERY_BYTES),
        filters_json,
        top_k: draft.top_k,
        documents: draft
            .documents
            .iter()
            .take(MAX_RETRIEVED_DOCUMENTS)
            .enumerate()
            .map(|(index, document)| RetrievedDocumentV1 {
                document_id: bounded(&document.id, MAX_DOCUMENT_ID_BYTES),
                source: bounded(&document.source, MAX_SOURCE_BYTES),
                score: if document.score.is_finite() {
                    document.score
                } else {
                    0.0
                },
                rank: u32::try_from(index + 1).unwrap_or(u32::MAX),
            })
            .collect(),
    }
}

/// The base name of a provider-visible tool name: toolkit tools may be
/// namespaced (`<toolkit>___<tool>`, `<toolkit>.<tool>`), so the suffix after
/// the last separator names the tool.
fn base_tool_name(name: &str) -> &str {
    let after_triple = name.rsplit("___").next().unwrap_or(name);
    after_triple.rsplit('.').next().unwrap_or(after_triple)
}

/// The retrieval a tool call performed, when the tool is a retrieval tool:
/// the index tools (`search_index`, `stepback_search_index`,
/// `stepback_summary_index`) and the attachment search (`search_attachment`).
/// Documents come from the tool's result: index hits are
/// `{page_content, metadata, score}` (the id is `metadata.id`, else
/// `metadata.chunk_id`, the source is `metadata.source`, else `file_path`,
/// `filename`, `url` or `title`); attachment hits are `{attachment_id, name,
/// offset, score}`. A result with no hits (a "no documents" message, an
/// error, the summary variant's model answer) is a retrieval with none.
#[must_use]
pub(crate) fn retrieval_from_tool(
    tool_name: &str,
    arguments: &Value,
    response: &Value,
) -> Option<RetrievalDraft> {
    let base = base_tool_name(tool_name);
    let text_argument = |key: &str| {
        arguments
            .get(key)
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned()
    };
    let top_k = |key: &str, default: u32| {
        arguments
            .get(key)
            .and_then(Value::as_u64)
            .map_or(default, |value| u32::try_from(value).unwrap_or(u32::MAX))
    };
    match base {
        "search_index" | "stepback_search_index" | "stepback_summary_index" => {
            let hits = match response {
                Value::Array(hits) => hits.clone(),
                // `stepback_search_index` answers "Found N documents ...\n<json>".
                Value::String(text) => text
                    .split_once('\n')
                    .and_then(|(_, rest)| serde_json::from_str::<Vec<Value>>(rest).ok())
                    .unwrap_or_default(),
                _ => Vec::new(),
            };
            let filters = arguments.get("filter").and_then(|filter| match filter {
                Value::String(text) if text.trim().is_empty() => None,
                Value::String(text) => serde_json::from_str::<Value>(text)
                    .ok()
                    .or_else(|| Some(Value::String(text.clone()))),
                other => Some(other.clone()),
            });
            Some(RetrievalDraft {
                index_name: text_argument("index_name"),
                query: text_argument("query"),
                filters,
                top_k: top_k("search_top", 10),
                documents: hits
                    .iter()
                    .enumerate()
                    .map(|(index, hit)| index_document(hit, index))
                    .collect(),
            })
        }
        "search_attachment" => {
            let hits = response
                .get("results")
                .and_then(Value::as_array)
                .cloned()
                .unwrap_or_default();
            Some(RetrievalDraft {
                index_name: "attachments".to_owned(),
                query: text_argument("query"),
                filters: arguments
                    .get("attachment_id")
                    .filter(|value| !value.is_null())
                    .map(|id| json!({"attachment_id": id})),
                top_k: top_k("top_k", 0),
                documents: hits
                    .iter()
                    .map(|hit| RetrievedDocument {
                        id: json_text(hit.get("attachment_id")),
                        source: json_text(hit.get("name")),
                        score: hit.get("score").and_then(Value::as_f64).unwrap_or(0.0),
                    })
                    .collect(),
            })
        }
        _ => None,
    }
}

fn json_text(value: Option<&Value>) -> String {
    match value {
        Some(Value::String(text)) => text.clone(),
        Some(Value::Number(number)) => number.to_string(),
        _ => String::new(),
    }
}

fn index_document(hit: &Value, index: usize) -> RetrievedDocument {
    let metadata = hit.get("metadata");
    let field = |key: &str| json_text(metadata.and_then(|metadata| metadata.get(key)));
    let source = ["source", "file_path", "filename", "url", "title"]
        .iter()
        .map(|key| field(key))
        .find(|value| !value.is_empty())
        .unwrap_or_default();
    let id = ["id", "chunk_id"]
        .iter()
        .map(|key| field(key))
        .find(|value| !value.is_empty())
        .unwrap_or_else(|| {
            if source.is_empty() {
                format!("hit-{}", index + 1)
            } else {
                source.clone()
            }
        });
    RetrievedDocument {
        id,
        source,
        score: hit.get("score").and_then(Value::as_f64).unwrap_or(0.0),
    }
}

/// A retrieval step's documents, kept to cite them from the output.
struct RecordedRetrieval {
    step_id: String,
    documents: Vec<(String, String)>,
}

/// The retrieval steps and document ids an output cites.
///
/// THE RULE: a document is cited when the output text contains its id or its
/// source verbatim (case-sensitive substring), counting only ids and sources
/// of at least four characters. A retrieval step is cited when any of its
/// documents is. Steps keep the order they happened in; document ids keep the
/// order of first citation, each once. Deterministic and cheap; it misses a
/// paraphrased citation, which is the price of never inventing one.
fn citations(text: &str, retrievals: &[RecordedRetrieval]) -> (Vec<String>, Vec<String>) {
    let named =
        |needle: &str| needle.chars().count() >= MIN_CITATION_NEEDLE_CHARS && text.contains(needle);
    let mut steps = Vec::new();
    let mut documents: Vec<String> = Vec::new();
    for retrieval in retrievals {
        let mut cited = false;
        for (id, source) in &retrieval.documents {
            if named(id) || named(source) {
                cited = true;
                if !documents.contains(id) {
                    documents.push(id.clone());
                }
            }
        }
        if cited {
            steps.push(retrieval.step_id.clone());
        }
    }
    (steps, documents)
}

fn into_step(draft: StepDraft, retrievals: &[RecordedRetrieval]) -> RunStepV1 {
    let step_id = uuid::Uuid::new_v4().to_string();
    let (cited_step_ids, cited_document_ids) = match (&draft.kind, &draft.outputs) {
        (RunStepKindV1::Output, Some(outputs)) => citations(&outputs.text, retrievals),
        _ => (Vec::new(), Vec::new()),
    };
    RunStepV1 {
        step_id,
        kind: draft.kind as i32,
        started_at_unix_ms: draft.started_at_unix_ms,
        ended_at_unix_ms: draft.ended_at_unix_ms.max(draft.started_at_unix_ms),
        node_id: bounded_owned(draft.node_id, MAX_NAME_BYTES),
        name: bounded_owned(draft.name, MAX_NAME_BYTES),
        toolkit_id: bounded_owned(draft.toolkit_id, MAX_ID_BYTES),
        summary_json: bounded_summary(&draft.summary),
        retrieval: draft.retrieval.as_ref().map(retrieval_summary),
        inputs: draft.inputs.as_ref().map(step_content),
        outputs: draft.outputs.as_ref().map(step_content),
        cited_step_ids,
        cited_document_ids: cited_document_ids
            .iter()
            .map(|id| bounded(id, MAX_DOCUMENT_ID_BYTES))
            .collect(),
    }
}

#[derive(Default)]
struct RecorderState {
    pending: VecDeque<RunStepV1>,
    /// When each pending step was recorded, oldest first.
    recorded_at: VecDeque<Instant>,
    /// No early report before this (set by a failed one).
    early_retry_at: Option<Instant>,
    early_failures: u32,
    retrievals: Vec<RecordedRetrieval>,
    /// The live claim's full identity and fence, once bound.
    claim: Option<(ExecutionIdentityV1, ExecutionFenceV1)>,
    /// While above zero, nothing is recorded: the projector is replaying
    /// events an earlier generation already reported.
    suspended: usize,
    /// This generation continues an earlier one (a HITL decision, an
    /// authorization, a Continue).
    resumed: bool,
}

/// The step buffer of one command. Shared by every path that runs part of
/// the command; cheap to record into (one short lock, no I/O).
pub(crate) struct RunStepRecorder {
    execution_id: String,
    state: Mutex<RecorderState>,
    /// Serializes this command's reports, so steps reach Main in order.
    send: tokio::sync::Mutex<()>,
}

impl RunStepRecorder {
    #[must_use]
    pub(crate) fn new(execution_id: &str) -> Arc<Self> {
        Arc::new(Self {
            execution_id: execution_id.to_owned(),
            state: Mutex::new(RecorderState::default()),
            send: tokio::sync::Mutex::new(()),
        })
    }

    fn with_state<T>(&self, apply: impl FnOnce(&mut RecorderState) -> T) -> T {
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        apply(&mut state)
    }

    /// Bound the step, give it its id and buffer it after every earlier one.
    /// Returns the step id, or `None` while recording is suspended (or the
    /// buffer is full, which is logged and counted).
    pub(crate) fn record(&self, draft: StepDraft) -> Option<String> {
        let recorded = self.with_state(|state| {
            if state.suspended > 0 {
                return Ok(None);
            }
            if state.pending.len() >= MAX_BUFFERED_STEPS {
                return Err(());
            }
            let documents = draft.retrieval.as_ref().map(|retrieval| {
                retrieval
                    .documents
                    .iter()
                    .take(MAX_RETRIEVED_DOCUMENTS)
                    .map(|document| (document.id.clone(), document.source.clone()))
                    .collect::<Vec<_>>()
            });
            let step = into_step(draft, &state.retrievals);
            let step_id = step.step_id.clone();
            if let Some(documents) = documents {
                state.retrievals.push(RecordedRetrieval {
                    step_id: step_id.clone(),
                    documents,
                });
            }
            state.pending.push_back(step);
            state.recorded_at.push_back(Instant::now());
            Ok(Some(step_id))
        });
        recorded.unwrap_or_else(|()| {
            self.count_dropped(1, "buffer_full");
            None
        })
    }

    /// Stop recording until the guard is dropped.
    #[must_use]
    pub(crate) fn suspend(self: &Arc<Self>) -> SuspendedRecording {
        self.with_state(|state| state.suspended += 1);
        SuspendedRecording {
            recorder: Arc::clone(self),
        }
    }

    pub(crate) fn mark_resumed(&self) {
        self.with_state(|state| state.resumed = true);
    }

    #[must_use]
    pub(crate) fn is_resumed(&self) -> bool {
        self.with_state(|state| state.resumed)
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) fn pending_len(&self) -> usize {
        self.with_state(|state| state.pending.len())
    }

    /// Whether to report before settlement: a full batch waits, or the
    /// oldest buffered step is older than [`MAX_UNSENT_STEP_AGE`] — unless a
    /// failed early report's backoff window is still open.
    #[must_use]
    pub(crate) fn should_flush_early(&self) -> bool {
        let now = Instant::now();
        self.with_state(|state| {
            let Some(oldest) = state.recorded_at.front() else {
                return false;
            };
            if state.early_retry_at.is_some_and(|retry_at| now < retry_at) {
                return false;
            }
            state.pending.len() >= MAX_STEPS_PER_REPORT
                || now.saturating_duration_since(*oldest) >= MAX_UNSENT_STEP_AGE
        })
    }

    /// An early report failed: its steps stay buffered, and the next early
    /// attempt waits for the backoff window.
    pub(crate) fn note_early_failure(&self) {
        self.with_state(|state| {
            state.early_failures = state.early_failures.saturating_add(1);
            let delay = EARLY_RETRY_DELAY
                .saturating_mul(1 << state.early_failures.saturating_sub(1).min(4))
                .min(MAX_EARLY_RETRY_DELAY);
            state.early_retry_at = Some(Instant::now() + delay);
        });
    }

    /// Test-only: pretend every buffered step was recorded `by` earlier.
    #[cfg(test)]
    pub(crate) fn age_pending(&self, by: Duration) {
        self.with_state(|state| {
            for at in &mut state.recorded_at {
                *at = at.checked_sub(by).unwrap_or(*at);
            }
        });
    }

    /// Test-only: close the early backoff window now.
    #[cfg(test)]
    pub(crate) fn expire_early_backoff(&self) {
        self.with_state(|state| state.early_retry_at = None);
    }

    fn bind_claim(&self, identity: &ExecutionIdentityV1, fence: &ExecutionFenceV1) {
        self.with_state(|state| state.claim = Some((identity.clone(), fence.clone())));
    }

    /// The live claim's identity and fence, which early reports go under.
    pub(crate) fn claim(&self) -> Option<(ExecutionIdentityV1, ExecutionFenceV1)> {
        self.with_state(|state| state.claim.clone())
    }

    /// Hold the send order for one report sequence.
    pub(crate) async fn lock_sending(&self) -> tokio::sync::MutexGuard<'_, ()> {
        self.send.lock().await
    }

    /// The oldest buffered steps that fit one request: at most 64, and at
    /// most [`MAX_REPORT_BYTES`] encoded (always at least one step). They
    /// stay buffered until [`Self::acknowledge`].
    #[must_use]
    pub(crate) fn next_batch(&self) -> Vec<RunStepV1> {
        self.with_state(|state| {
            let mut batch = Vec::new();
            // Room for the identity, the fence and the field tags.
            let mut size = 4 * 1024;
            for step in state.pending.iter().take(MAX_STEPS_PER_REPORT) {
                let encoded = step.encoded_len() + 8;
                if !batch.is_empty() && size + encoded > MAX_REPORT_BYTES {
                    break;
                }
                size += encoded;
                batch.push(step.clone());
            }
            batch
        })
    }

    /// Main accepted the oldest `count` steps.
    pub(crate) fn acknowledge(&self, count: usize) {
        self.with_state(|state| {
            let count = count.min(state.pending.len());
            state.pending.drain(..count);
            state
                .recorded_at
                .drain(..count.min(state.recorded_at.len()));
            state.early_retry_at = None;
            state.early_failures = 0;
        });
    }

    /// Drop every buffered step, logged with the execution and counted.
    pub(crate) fn drop_pending(&self, reason: &'static str) -> usize {
        let dropped = self.with_state(|state| {
            let count = state.pending.len();
            state.pending.clear();
            state.recorded_at.clear();
            count
        });
        if dropped > 0 {
            self.count_dropped(dropped, reason);
        }
        dropped
    }

    fn count_dropped(&self, count: usize, reason: &'static str) {
        let count = u64::try_from(count).unwrap_or(u64::MAX);
        let total = DROPPED_TOTAL.fetch_add(count, Ordering::Relaxed) + count;
        tracing::warn!(
            event = "worker_run_steps_dropped",
            execution_id = %self.execution_id,
            dropped = count,
            dropped_total = total,
            reason,
            "run step records were dropped instead of reported; the run continues"
        );
    }
}

/// Recording stays suspended while this lives.
pub(crate) struct SuspendedRecording {
    recorder: Arc<RunStepRecorder>,
}

impl Drop for SuspendedRecording {
    fn drop(&mut self) {
        self.recorder
            .with_state(|state| state.suspended = state.suspended.saturating_sub(1));
    }
}

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
struct CommandKey {
    execution_id: String,
    generation: u64,
    command_id: String,
}

impl CommandKey {
    fn of_identity(identity: &ExecutionIdentityV1) -> Self {
        Self {
            execution_id: identity.execution_id.clone(),
            generation: identity.generation,
            command_id: identity.command_id.clone(),
        }
    }
}

struct Tracked {
    recorder: Arc<RunStepRecorder>,
    since: Instant,
}

/// The commands this process is running, each with its one recorder.
/// Production uses [`RunStepRegistry::global`]; a test may hold its own.
#[derive(Default)]
pub struct RunStepRegistry {
    tracked: Mutex<HashMap<CommandKey, Tracked>>,
}

impl RunStepRegistry {
    /// The process's registry.
    #[must_use]
    pub fn global() -> Arc<Self> {
        static GLOBAL: OnceLock<Arc<RunStepRegistry>> = OnceLock::new();
        Arc::clone(GLOBAL.get_or_init(|| Arc::new(Self::default())))
    }

    fn with_tracked<T>(&self, apply: impl FnOnce(&mut HashMap<CommandKey, Tracked>) -> T) -> T {
        let mut tracked = self
            .tracked
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        apply(&mut tracked)
    }

    /// The command's recorder, created on first use. A newer generation of
    /// the execution drops the older generations' recorders (their claims
    /// are gone; their unsent steps are logged and counted).
    #[must_use]
    pub(crate) fn recorder(&self, identity: &ExecutionIdentityV1) -> Arc<RunStepRecorder> {
        let key = CommandKey::of_identity(identity);
        let (recorder, evicted) = self.with_tracked(|tracked| {
            if let Some(existing) = tracked.get(&key) {
                return (Arc::clone(&existing.recorder), Vec::new());
            }
            let now = Instant::now();
            let mut evicted = Vec::new();
            tracked.retain(|other, entry| {
                let keep = now.duration_since(entry.since) < MAX_TRACKED_AGE
                    && !(other.execution_id == key.execution_id
                        && other.generation < key.generation);
                if !keep {
                    evicted.push(Arc::clone(&entry.recorder));
                }
                keep
            });
            if tracked.len() >= MAX_TRACKED_EXECUTIONS
                && let Some(oldest) = tracked
                    .iter()
                    .min_by_key(|(_, entry)| entry.since)
                    .map(|(key, _)| key.clone())
                && let Some(entry) = tracked.remove(&oldest)
            {
                evicted.push(entry.recorder);
            }
            let recorder = RunStepRecorder::new(&key.execution_id);
            tracked.insert(
                key,
                Tracked {
                    recorder: Arc::clone(&recorder),
                    since: now,
                },
            );
            (recorder, evicted)
        });
        for stale in evicted {
            stale.drop_pending("execution_evicted");
        }
        recorder
    }

    /// Bind the live claim (its full identity and fence): early reports
    /// are sent under it.
    pub(crate) fn bind_claim(&self, identity: &ExecutionIdentityV1, fence: &ExecutionFenceV1) {
        self.recorder(identity).bind_claim(identity, fence);
    }

    /// The command's recorder when it has one.
    #[must_use]
    pub(crate) fn find(&self, identity: &ExecutionIdentityV1) -> Option<Arc<RunStepRecorder>> {
        self.with_tracked(|tracked| {
            tracked
                .get(&CommandKey::of_identity(identity))
                .map(|entry| Arc::clone(&entry.recorder))
        })
    }

    /// Stop tracking a settled command; anything still buffered is dropped
    /// (logged and counted).
    pub(crate) fn forget(&self, identity: &ExecutionIdentityV1) {
        if let Some(entry) =
            self.with_tracked(|tracked| tracked.remove(&CommandKey::of_identity(identity)))
        {
            entry.recorder.drop_pending("settled_with_unsent_steps");
        }
    }
}

/// The identity of a command as its settlement names it.
#[must_use]
pub(crate) fn command_identity(
    execution_id: &str,
    generation: u64,
    command_id: &str,
) -> ExecutionIdentityV1 {
    ExecutionIdentityV1 {
        execution_id: execution_id.to_owned(),
        generation,
        command_id: command_id.to_owned(),
        ..ExecutionIdentityV1::default()
    }
}

/// The wall clock in Unix milliseconds.
#[must_use]
pub(crate) fn now_unix_ms() -> i64 {
    chrono::Utc::now().timestamp_millis()
}

/// Unix milliseconds of an RFC 3339 timestamp; 0 when it does not parse.
#[must_use]
pub(crate) fn rfc3339_unix_ms(timestamp: &str) -> i64 {
    chrono::DateTime::parse_from_rfc3339(timestamp).map_or(0, |time| time.timestamp_millis())
}

#[cfg(test)]
#[path = "run_steps_tests.rs"]
mod tests;
