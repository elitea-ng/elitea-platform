//! The index steps that call a model (ADR-0030 I4): the proposal chunker's
//! chat calls, the statistical chunker's embeddings and the image-file
//! vision descriptions.
//!
//! `elitea-doc-chunk` stays model-free: it returns a [`ChunkPlan`] naming the
//! windows or splits, and this module fulfils it. What it owns:
//!
//! * the chat model: bound lazily and once per call through
//!   [`ChatModelBinder`] (the bound ADK model counts its own turns, so a
//!   call gets a fresh single-turn binding, as the step-back tools do),
//!   OpenAI-compatible over `/llm/v1` with the claim's resource project as
//!   the model owner;
//! * the policy. A document is never dropped silently:
//!   - a retryable failure (429, 5xx, a timeout, the transport, an answer
//!     that is not the requested JSON after its attempts) skips THAT
//!     document as `model_failed`; nothing of its stored version changes, so
//!     the next run retries it, and [`MAX_CONSECUTIVE_MODEL_FAILURES`] model-step
//!     documents in a row stop the run (any model-step success, proposal or
//!     statistical, resets the streak; documents with no model step and
//!     `no_model` / `model_cap_exceeded` skips neither count nor reset);
//!   - a refusal another attempt cannot cure (the credential, the project,
//!     the budget, an unknown model, any other 4xx) stops the run, its
//!     message carrying the gateway's stable error code;
//!   - no chat model bound (no `index.llm_model` entry, or a deployment
//!     without model access) skips the proposal chunker's documents as
//!     `no_model`;
//! * the per-run cap on model calls ([`IndexRunLimits::max_model_calls`]):
//!   every actual call counts, a retry included (charged before the attempt,
//!   atomically, so windows in flight share it). A document whose calls alone
//!   exceed the whole cap is skipped `model_cap_exceeded` (its stored
//!   version untouched, the run goes on); one that fits the cap but not what
//!   remains of it stops the run, and the next run resumes (finished
//!   documents are versioned). The documents already written stay;
//! * liveness (Stop, the lease, the deadline) is checked before every
//!   attempt and every retry wait;
//! * an image file is described by ONE chat call carrying the image as an
//!   `InlineData` part beside the prompt ([`VisionJob`]), under the same
//!   policy (every attempt charged, liveness before each, a retryable failure
//!   `model_failed` with the breaker, a refusal or a reply cut by `max_tokens`
//!   stopping the run, no model `no_model`). Descriptions are cached for the
//!   run by sha256(bytes) + sha256(prompt) + model name ([`ImageCache`], LRU
//!   of 500, as the SDK's `ImageDescriptionCache` but sha256 and keyed by the
//!   model); a hit costs no call and is not charged. Image bytes and
//!   descriptions are never logged;
//! * a document's proposal windows are asked [`WINDOWS_IN_FLIGHT`] at a time,
//!   answers kept in window order. Documents themselves are still fulfilled
//!   one after another by the run's loop (a follow-up, not done here).

use std::collections::{HashMap, VecDeque};
use std::sync::Arc;
use std::sync::atomic::{AtomicU32, Ordering};

use adk_rust::futures::StreamExt as _;
use adk_rust::futures::stream::FuturesOrdered;
use adk_rust::{AdkError, Content, FinishReason, Llm, LlmRequest, Part};
use elitea_doc_chunk::{
    Chunk, ChunkDetails, ChunkError, ChunkPlan, ProposalPlan, StatisticalPlan, VisionPlan,
    parse_chunk_analysis, proposal_system_prompt, proposal_user_message,
};
use sha2::{Digest as _, Sha256};

use super::IndexRunLimits;
use super::inputs::LlmSettings;
use super::pipeline::{
    EmbedOutcome, Gated, IndexRunFailure, SKIP_MODEL_CAP_EXCEEDED, SKIP_MODEL_FAILED,
    SKIP_NO_MODEL, SKIP_TOO_LARGE, classify_embedding_error, embed_gated,
};
use crate::agents::session::BoundOrdinaryAgentModel as _;
use crate::transport::embeddings_facade::EmbeddingsClient;
use crate::transport::model_facade::{
    ModelAdapterKind, ModelFacade, ModelFacadeError, ModelInvocation,
};
use crate::transport::runtime_context::ClaimScopedEliteaContext;

/// Model-step documents in a row whose model step failed (skipped
/// `model_failed`) before the run stops: a model outage must not end as every
/// document skipped.
///
/// The streak counts only documents that HAVE a model step. A model-step
/// document whose step succeeds (proposal or statistical) resets it. A
/// document without a model step, a `no_model` or `model_cap_exceeded` skip
/// (no model call failed), and a statistical embeddings failure (the run's
/// embedding-failure streak owns it) neither increment nor reset it.
pub(crate) const MAX_CONSECUTIVE_MODEL_FAILURES: u32 = 5;
/// Attempts per proposal window for a retryable failure or an unreadable
/// answer.
const CHAT_ATTEMPTS: u32 = 3;
/// Proposal windows of one document asked at the same time.
const WINDOWS_IN_FLIGHT: usize = 4;
/// The most bytes of one model reply.
const MAX_REPLY_BYTES: usize = 256 * 1024;

/// What one chat call is bound with.
pub(crate) struct ChatModelRequest<'a> {
    pub(crate) model_name: &'a str,
    pub(crate) system_instruction: &'a str,
    pub(crate) max_tokens: Option<u32>,
    pub(crate) temperature: Option<f32>,
}

/// Binds the chat model of an index step for one claim.
pub(crate) trait ChatModelBinder: Send + Sync {
    /// A single-use model over the gateway for `context`.
    ///
    /// # Errors
    ///
    /// The invocation is not admissible (an unusable model name).
    fn bind_chat(
        &self,
        context: &ClaimScopedEliteaContext,
        request: &ChatModelRequest<'_>,
    ) -> Result<Arc<dyn Llm>, ModelFacadeError>;
}

impl ChatModelBinder for ModelFacade {
    fn bind_chat(
        &self,
        context: &ClaimScopedEliteaContext,
        request: &ChatModelRequest<'_>,
    ) -> Result<Arc<dyn Llm>, ModelFacadeError> {
        let invocation = ModelInvocation {
            response_schema: None,
            allow_text_continuation: false,
            context_budget: None,
            model_name: request.model_name.to_owned(),
            system_instruction: request.system_instruction.to_owned(),
            max_tokens: request.max_tokens,
            reasoning_effort: None,
            temperature: request.temperature,
            max_model_turns: 1,
        };
        // The index's model is the claim's resource project's (or the shared
        // public fallback the gateway resolves), like the step-back binder.
        let owner = u32::try_from(context.resource_project_id())
            .map_err(|_| ModelFacadeError::InvalidInvocation)?;
        Self::bind(
            self,
            ModelAdapterKind::OpenAiCompatible,
            context,
            owner,
            invocation,
        )
        .map(|bound| bound.adk_model())
    }
}

/// The chat model an index run was given.
pub(crate) struct ChatBinding {
    pub(crate) binder: Arc<dyn ChatModelBinder>,
    pub(crate) model_name: String,
    pub(crate) settings: LlmSettings,
}

/// What fulfilling a plan came to.
pub(crate) enum StepEnd {
    /// The document's chunks.
    Chunks(Vec<Chunk>),
    /// Skip the document under this closed reason.
    Skip(&'static str),
    /// The statistical embeddings failed for a reason another run cures; the
    /// caller counts it with the embedding failures.
    EmbeddingFailed(String),
    /// Stop the run, for this reason.
    Stop(String),
    /// The lease was lost or Stop arrived while a call was pending.
    Liveness(IndexRunFailure),
}

/// A chat call that did not yield an answer.
enum CallFailure {
    /// Another attempt may succeed.
    Retryable(String),
    /// No attempt will: the run stops.
    Fatal(String),
    /// This document cannot be answered (an oversized reply), the model is
    /// fine.
    Unusable(String),
    /// Stop the run with this sentence (the cap, a reply cut by `max_tokens`).
    Stop(String),
    /// The lease was lost or Stop arrived.
    Liveness(IndexRunFailure),
}

/// Why a gate refused an attempt.
pub(super) enum GateStop {
    Cap(String),
    Liveness(IndexRunFailure),
}

impl From<GateStop> for CallFailure {
    fn from(stop: GateStop) -> Self {
        match stop {
            GateStop::Cap(reason) => Self::Stop(reason),
            GateStop::Liveness(failure) => Self::Liveness(failure),
        }
    }
}

type Alive<'a> = &'a (dyn Fn() -> Result<(), IndexRunFailure> + Sync);

/// What stands before every model attempt: liveness, then (for an attempt)
/// one atomic charge against the run's cap.
struct Gate<'a> {
    calls: &'a AtomicU32,
    cap: u32,
    alive: Alive<'a>,
}

impl Gate<'_> {
    /// Liveness, and when `charge` one call against the cap.
    fn check(&self, charge: bool) -> Result<(), GateStop> {
        (self.alive)().map_err(GateStop::Liveness)?;
        if charge {
            self.calls
                .fetch_update(Ordering::SeqCst, Ordering::SeqCst, |n| {
                    (n < self.cap).then_some(n + 1)
                })
                .map_err(|_| GateStop::Cap(cap_message(self.cap)))?;
        }
        Ok(())
    }
}

fn cap_message(cap: u32) -> String {
    format!(
        "the run reached its limit of {cap} model calls (every chat call per proposal window, per image description and every embeddings request of the statistical chunker, retries included); the documents written so far are kept; re-run to continue, or raise the deployment's index model call cap"
    )
}

/// The state of a run's model steps.
pub(crate) struct ModelSteps {
    chat: Option<ChatBinding>,
    cap: u32,
    calls: AtomicU32,
    consecutive_failures: u32,
    images: ImageCache,
}

impl ModelSteps {
    pub(crate) fn new(chat: Option<ChatBinding>, limits: &IndexRunLimits) -> Self {
        Self {
            chat,
            cap: limits.max_model_calls,
            calls: AtomicU32::new(0),
            consecutive_failures: 0,
            images: ImageCache::default(),
        }
    }

    fn gate<'a>(&'a self, alive: Alive<'a>) -> Gate<'a> {
        Gate {
            calls: &self.calls,
            cap: self.cap,
            alive,
        }
    }

    /// What `required` calls come to: fine, a document that alone exceeds the
    /// whole cap (skipped), or more than the run has left (the run stops, the
    /// next run resumes).
    fn admit(&self, required: usize) -> Result<(), StepEnd> {
        let required = u64::try_from(required).unwrap_or(u64::MAX);
        if required > u64::from(self.cap) {
            tracing::warn!(
                event = "index_document_exceeds_model_call_cap",
                required,
                cap = self.cap,
                "a document needs more model calls than the whole per-run cap; skipped model_cap_exceeded; raise the deployment's index model call cap to index it"
            );
            return Err(StepEnd::Skip(SKIP_MODEL_CAP_EXCEEDED));
        }
        let left = u64::from(self.cap.saturating_sub(self.calls.load(Ordering::SeqCst)));
        if required > left {
            return Err(StepEnd::Stop(cap_message(self.cap)));
        }
        Ok(())
    }

    /// Fulfils `plan`. `alive` is checked before every attempt.
    pub(crate) async fn fulfil(
        &mut self,
        plan: ChunkPlan,
        env: &StepEnv<'_>,
        alive: Alive<'_>,
    ) -> StepEnd {
        match plan {
            ChunkPlan::Chunks(chunks) => StepEnd::Chunks(chunks),
            ChunkPlan::Proposal(plan) => self.proposal(&plan, env, alive).await,
            ChunkPlan::Statistical(plan) => self.statistical(&plan, env, alive).await,
        }
    }

    async fn proposal(
        &mut self,
        plan: &ProposalPlan,
        env: &StepEnv<'_>,
        alive: Alive<'_>,
    ) -> StepEnd {
        let Some(chat) = self
            .chat
            .as_ref()
            .map(|c| (Arc::clone(&c.binder), c.model_name.clone(), c.settings))
        else {
            return StepEnd::Skip(SKIP_NO_MODEL);
        };
        if let Err(end) = self.admit(plan.windows().len()) {
            return end;
        }
        let (binder, model_name, settings) = chat;
        let outcome = {
            let gate = self.gate(alive);
            let mut pending = FuturesOrdered::new();
            let mut windows = plan.windows().iter();
            let mut answers: Vec<Vec<ChunkDetails>> = Vec::with_capacity(plan.windows().len());
            let mut failure = None;
            loop {
                while pending.len() < WINDOWS_IN_FLIGHT {
                    let Some(window) = windows.next() else {
                        break;
                    };
                    pending.push_back(chat_window(
                        &*binder,
                        env,
                        &gate,
                        &model_name,
                        settings,
                        window,
                    ));
                }
                let Some(result) = pending.next().await else {
                    break;
                };
                match result {
                    Ok(details) => answers.push(details),
                    Err(error) => {
                        failure = Some(error);
                        break;
                    }
                }
            }
            failure.map_or(Ok(answers), Err)
        };
        match outcome {
            Ok(answers) => match plan.chunks(&answers) {
                Ok(chunks) => {
                    self.model_step_succeeded();
                    StepEnd::Chunks(chunks)
                }
                Err(error) => self.failed_document(&error.to_string()),
            },
            Err(CallFailure::Fatal(code)) => StepEnd::Stop(format!(
                "the chunking model refused the run ({code}); nothing further was sent to it"
            )),
            Err(CallFailure::Stop(reason)) => StepEnd::Stop(reason),
            Err(CallFailure::Liveness(failure)) => StepEnd::Liveness(failure),
            Err(CallFailure::Retryable(cause) | CallFailure::Unusable(cause)) => {
                self.failed_document(&cause)
            }
        }
    }

    /// Describes one image file (ADR-0030 I4 part B): the chunks of the
    /// model's description, from the run's cache when the same image was
    /// described before with the same prompt and model.
    pub(crate) async fn describe(
        &mut self,
        job: &VisionJob,
        env: &StepEnv<'_>,
        alive: Alive<'_>,
    ) -> StepEnd {
        let Some(chat) = self
            .chat
            .as_ref()
            .map(|c| (Arc::clone(&c.binder), c.model_name.clone(), c.settings))
        else {
            return StepEnd::Skip(SKIP_NO_MODEL);
        };
        let (binder, model_name, settings) = chat;
        let key = ImageKey::new(&job.bytes, job.plan.prompt(), &model_name);
        if let Some(description) = self.images.get(&key) {
            // No call was made: the cap is not charged and the failure
            // streak is neither extended nor reset.
            return vision_chunks(&job.plan, &description);
        }
        if let Err(end) = self.admit(1) {
            return end;
        }
        let outcome = {
            let gate = self.gate(alive);
            vision_call(&*binder, env, &gate, &model_name, settings, job).await
        };
        match outcome {
            Ok(description) => {
                self.model_step_succeeded();
                let end = vision_chunks(&job.plan, &description);
                self.images.put(key, description);
                end
            }
            Err(CallFailure::Fatal(code)) => StepEnd::Stop(format!(
                "the vision model refused the run ({code}); nothing further was sent to it"
            )),
            Err(CallFailure::Stop(reason)) => StepEnd::Stop(reason),
            Err(CallFailure::Liveness(failure)) => StepEnd::Liveness(failure),
            Err(CallFailure::Retryable(cause) | CallFailure::Unusable(cause)) => {
                self.failed_document(&cause)
            }
        }
    }

    /// A model-step document whose step succeeded: the failure streak ends.
    fn model_step_succeeded(&mut self) {
        self.consecutive_failures = 0;
    }

    /// One document skipped `model_failed`, or the run stopped when that is
    /// the Nth in a row.
    fn failed_document(&mut self, cause: &str) -> StepEnd {
        self.consecutive_failures += 1;
        tracing::warn!(
            event = "index_model_step_failed",
            cause,
            in_a_row = self.consecutive_failures
        );
        if self.consecutive_failures >= MAX_CONSECUTIVE_MODEL_FAILURES {
            StepEnd::Stop(format!(
                "the chunking model failed for {} documents in a row ({cause}); the run stopped rather than skip every document, retry once the model is healthy",
                self.consecutive_failures
            ))
        } else {
            StepEnd::Skip(SKIP_MODEL_FAILED)
        }
    }

    async fn statistical(
        &mut self,
        plan: &StatisticalPlan,
        env: &StepEnv<'_>,
        alive: Alive<'_>,
    ) -> StepEnd {
        let batch = plan
            .batch_size()
            .clamp(1, crate::transport::embeddings_facade::MAX_BATCH_INPUTS);
        if let Err(end) = self.admit(plan.splits().len().div_ceil(batch)) {
            return end;
        }
        let gate = self.gate(alive);
        let mut vectors: Vec<Vec<f32>> = Vec::with_capacity(plan.splits().len());
        for inputs in plan.splits().chunks(batch) {
            let embedded = embed_gated(
                env.embeddings,
                env.context,
                env.embedding_model,
                inputs,
                env.limits.embedding_retry_backoff,
                &|charge| gate.check(charge),
            )
            .await;
            match embedded {
                Ok(embedded) => vectors.extend(embedded.vectors),
                Err(Gated::Gate(GateStop::Cap(reason))) => return StepEnd::Stop(reason),
                Err(Gated::Gate(GateStop::Liveness(failure))) => {
                    return StepEnd::Liveness(failure);
                }
                Err(Gated::Embeddings(error)) => {
                    return match classify_embedding_error(error) {
                        EmbedOutcome::Fatal(message) | EmbedOutcome::Mismatch(message) => {
                            StepEnd::Stop(message)
                        }
                        EmbedOutcome::TooLarge => StepEnd::Skip(SKIP_TOO_LARGE),
                        EmbedOutcome::Failed(cause) => StepEnd::EmbeddingFailed(cause),
                    };
                }
            }
        }
        match plan.chunks(&vectors) {
            Ok(chunks) => {
                self.model_step_succeeded();
                StepEnd::Chunks(chunks)
            }
            // The model answered with vectors that do not fit its own
            // request: the embedding space is not trustworthy.
            Err(ChunkError::InvalidModelStep(reason)) => StepEnd::Stop(format!(
                "the embedding model's answer for the statistical chunker is unusable ({reason})"
            )),
            Err(error) => StepEnd::Stop(format!("the statistical chunker failed: {error}")),
        }
    }
}

/// What the model steps call.
pub(crate) struct StepEnv<'a> {
    pub(crate) embeddings: &'a EmbeddingsClient,
    pub(crate) context: &'a ClaimScopedEliteaContext,
    pub(crate) embedding_model: &'a str,
    pub(crate) limits: &'a IndexRunLimits,
}

/// One image file waiting for its description.
pub(crate) struct VisionJob {
    pub(crate) bytes: Vec<u8>,
    /// The bytes' own media type, one the facade sends (png, jpeg, gif, webp).
    pub(crate) media_type: &'static str,
    pub(crate) plan: VisionPlan,
}

fn vision_chunks(plan: &VisionPlan, description: &str) -> StepEnd {
    match plan.chunks(description) {
        Ok(chunks) => StepEnd::Chunks(chunks),
        Err(error) => StepEnd::Stop(format!(
            "the image description could not be chunked: {error}"
        )),
    }
}

/// The description of one image, with the attempts the policy allows: one
/// chat call with the prompt and the image, every attempt charged before it
/// is made, liveness checked before every attempt and every retry wait.
async fn vision_call(
    binder: &dyn ChatModelBinder,
    env: &StepEnv<'_>,
    gate: &Gate<'_>,
    model_name: &str,
    settings: LlmSettings,
    job: &VisionJob,
) -> Result<String, CallFailure> {
    let mut attempt = 0_u32;
    loop {
        attempt += 1;
        gate.check(true)?;
        let model = binder
            .bind_chat(
                env.context,
                &ChatModelRequest {
                    model_name,
                    system_instruction: "",
                    max_tokens: settings.max_tokens,
                    temperature: settings.temperature,
                },
            )
            .map_err(|error| CallFailure::Fatal(format!("model_facade.{error:?}")))?;
        // The SDK's single HumanMessage: the text part, then the image part.
        let content = Content {
            role: "user".to_owned(),
            parts: vec![
                Part::Text {
                    text: job.plan.prompt().to_owned(),
                },
                Part::InlineData {
                    mime_type: job.media_type.to_owned(),
                    data: job.bytes.clone(),
                    uri: None,
                    annotations: None,
                },
            ],
        };
        match complete(&*model, model_name, content, settings).await {
            Ok(description) => return Ok(description),
            Err(CallFailure::Retryable(_)) if attempt < CHAT_ATTEMPTS => {
                gate.check(false)?;
                tokio::time::sleep(env.limits.embedding_retry_backoff * attempt).await;
            }
            Err(other) => return Err(other),
        }
    }
}

/// Descriptions kept for a run.
const IMAGE_CACHE_ENTRIES: usize = 500;
/// A description longer than this is used but not cached (500 entries of the
/// largest reply would be 128 MiB).
const IMAGE_CACHE_MAX_DESCRIPTION_BYTES: usize = 32 * 1024;

/// sha256(bytes) + sha256(prompt) + the model's name.
#[derive(Clone, PartialEq, Eq, Hash)]
struct ImageKey([u8; 32], [u8; 32], String);

impl ImageKey {
    fn new(bytes: &[u8], prompt: &str, model: &str) -> Self {
        Self(
            Sha256::digest(bytes).into(),
            Sha256::digest(prompt.as_bytes()).into(),
            model.to_owned(),
        )
    }
}

/// The run's image descriptions: a least-recently-used map of
/// [`IMAGE_CACHE_ENTRIES`]. Per run, in memory, never logged.
#[derive(Default)]
struct ImageCache {
    entries: HashMap<ImageKey, String>,
    /// Least recently used first.
    order: VecDeque<ImageKey>,
}

impl ImageCache {
    fn touch(&mut self, key: &ImageKey) {
        if let Some(at) = self.order.iter().position(|k| k == key) {
            self.order.remove(at);
        }
        self.order.push_back(key.clone());
    }

    fn get(&mut self, key: &ImageKey) -> Option<String> {
        let hit = self.entries.get(key).cloned()?;
        self.touch(key);
        Some(hit)
    }

    /// Like the SDK, a blank description is not kept.
    fn put(&mut self, key: ImageKey, description: String) {
        if description.trim().is_empty() || description.len() > IMAGE_CACHE_MAX_DESCRIPTION_BYTES {
            return;
        }
        if self.entries.len() >= IMAGE_CACHE_ENTRIES
            && !self.entries.contains_key(&key)
            && let Some(oldest) = self.order.pop_front()
        {
            self.entries.remove(&oldest);
        }
        self.touch(&key);
        self.entries.insert(key, description);
    }
}

/// The model's `ChunkDetails` for one window, with the attempts the policy
/// allows. Every attempt is charged against the run's cap before it is made,
/// and liveness is checked before every attempt and every retry wait.
async fn chat_window(
    binder: &dyn ChatModelBinder,
    env: &StepEnv<'_>,
    gate: &Gate<'_>,
    model_name: &str,
    settings: LlmSettings,
    window: &str,
) -> Result<Vec<ChunkDetails>, CallFailure> {
    let system = proposal_system_prompt();
    let user = proposal_user_message(window);
    let mut attempt = 0_u32;
    loop {
        attempt += 1;
        gate.check(true)?;
        let model = binder
            .bind_chat(
                env.context,
                &ChatModelRequest {
                    model_name,
                    system_instruction: &system,
                    max_tokens: settings.max_tokens,
                    temperature: settings.temperature,
                },
            )
            .map_err(|error| CallFailure::Fatal(format!("model_facade.{error:?}")))?;
        let failure = match complete(
            &*model,
            model_name,
            Content::new("user").with_text(&user),
            settings,
        )
        .await
        {
            Ok(reply) => match parse_chunk_analysis(&reply) {
                Ok(details) => return Ok(details),
                Err(error) => CallFailure::Retryable(error.to_string()),
            },
            Err(failure) => failure,
        };
        match failure {
            CallFailure::Retryable(_) if attempt < CHAT_ATTEMPTS => {
                gate.check(false)?;
                tokio::time::sleep(env.limits.embedding_retry_backoff * attempt).await;
            }
            other => return Err(other),
        }
    }
}

/// One non-streaming call, as the step-back model makes it: a streaming call
/// would hand back partial deltas and a final response carrying the whole
/// text.
async fn complete(
    model: &dyn Llm,
    model_name: &str,
    user: Content,
    settings: LlmSettings,
) -> Result<String, CallFailure> {
    let request = LlmRequest {
        model: model_name.to_owned(),
        contents: vec![user],
        config: None,
        tools: std::collections::HashMap::new(),
        previous_response_id: None,
    };
    let mut responses = model
        .generate_content(request, false)
        .await
        .map_err(|error| classify_model_error(&error))?;
    let mut text: Option<String> = None;
    while let Some(response) = responses.next().await {
        let response = response.map_err(|error| classify_model_error(&error))?;
        if response.partial {
            continue;
        }
        if response.error_code.is_some() {
            return Err(CallFailure::Retryable(
                "the model ended the reply with an error".to_owned(),
            ));
        }
        if response.finish_reason == Some(FinishReason::MaxTokens) {
            // Cut off by max_tokens: another attempt would be cut off too.
            let in_use = settings.max_tokens.map_or_else(
                || "unset, so the model's default".to_owned(),
                |n| n.to_string(),
            );
            return Err(CallFailure::Stop(format!(
                "the index model's reply was cut off by its max_tokens limit (the toolkit's llm `max_tokens` is {in_use}); raise `max_tokens` in the toolkit's llm settings and re-run"
            )));
        }
        if let Some(reason) = response.finish_reason
            && reason != FinishReason::Stop
        {
            return Err(CallFailure::Retryable(format!(
                "the model stopped for {reason:?}"
            )));
        }
        let mut reply = String::new();
        if let Some(content) = &response.content {
            for part in &content.parts {
                if let Part::Text { text: piece } = part {
                    if reply.len().saturating_add(piece.len()) > MAX_REPLY_BYTES {
                        return Err(CallFailure::Unusable(
                            "the model's reply exceeds the size bound".to_owned(),
                        ));
                    }
                    reply.push_str(piece);
                }
            }
        }
        text = Some(reply);
    }
    text.ok_or_else(|| CallFailure::Retryable("the model returned no reply".to_owned()))
}

/// The gateway's refusal, sorted: retryable (429, 5xx, a timeout, the
/// transport) fails the document; a size bound of this reply fails the
/// document; anything else (the credential, the project, the budget, an
/// unknown model, another 4xx) cannot be cured by another attempt and stops
/// the run. The code is the facade's stable one; no provider text is read.
fn classify_model_error(error: &AdkError) -> CallFailure {
    tracing::warn!(
        event = "index_model_call_failed",
        error_code = error.code,
        retryable = error.is_retryable(),
    );
    if error.is_retryable() {
        return CallFailure::Retryable(error.code.to_owned());
    }
    match error.code {
        "model_gateway.event_too_large"
        | "model_gateway.stream_too_large"
        | "model_gateway.request_too_large" => CallFailure::Unusable(error.code.to_owned()),
        code => CallFailure::Fatal(code.to_owned()),
    }
}
