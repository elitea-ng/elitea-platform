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
//!   `InlineData` part beside the prompt ([`VisionJob`]), through the same
//!   call helper ([`call`]: charge, bind, retry, backoff). An image NEVER
//!   stops a run for a reason of its own and never feeds the failure streak:
//!   a retryable failure is `model_failed`, the gateway's generic rejection
//!   `vision_refused`, an over-size request `too_large`, an unbindable model
//!   `no_model` (warned once), an exhausted call cap `model_cap_exceeded`;
//!   a reply cut by `max_tokens` is kept. Only the credential, the project
//!   and the budget stop the run. Images have their own budget inside the
//!   run's cap: at most half of it ([`image_cap`], at least one call when the
//!   cap is at least one), so a run with many pictures always leaves the
//!   proposal and statistical chunkers at least half of the cap; a spent
//!   image budget skips the remaining images `model_cap_exceeded`. Three
//!   `vision_refused` in a row for the run's model ([`REFUSED_IN_A_ROW`])
//!   mark it as one that takes no images: the rest are skipped
//!   `vision_refused` without a call. The cheap checks come before the
//!   expensive ones: the chat model, the sha256 of the original bytes and the
//!   cache, the budget, and only then the decode and resize of the picture;
//! * the chat model entries are read lazily ([`ChatLoader`]): on the first
//!   image that needs a description or the first proposal document, once per
//!   run, both entries together. If either cannot be read the run has no chat
//!   model (images `no_model`, a proposal document fails the run), never
//!   default settings for a configuration that failed; Descriptions are cached for the run by
//!   sha256(original bytes) + sha256(prompt) + model name ([`ImageCache`],
//!   O(1) LRU of 500, as the SDK's `ImageDescriptionCache` but sha256 and
//!   keyed by the model); a hit costs no call and is not charged. Image
//!   bytes and descriptions are never logged;
//! * a document's proposal windows are asked [`WINDOWS_IN_FLIGHT`] at a time,
//!   answers kept in window order. Documents themselves are still fulfilled
//!   one after another by the run's loop (a follow-up, not done here).

use std::collections::{HashMap, VecDeque};
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};

use adk_rust::futures::StreamExt as _;
use adk_rust::futures::stream::FuturesOrdered;
use adk_rust::{AdkError, Content, FinishReason, Part};
use async_trait::async_trait;
use bytes::Bytes;
use elitea_doc_chunk::{
    Chunk, ChunkDetails, ChunkError, ChunkPlan, ProposalPlan, StatisticalPlan, VisionPlan,
    parse_chunk_analysis, proposal_system_prompt, proposal_user_message,
};
use sha2::{Digest as _, Sha256};

use super::IndexRunLimits;
use super::image_fit::{self, FitError, ImageKind};
use super::inputs::LlmSettings;
use super::pipeline::{
    EmbedOutcome, Gated, IndexRunFailure, SKIP_EXTRACT_TIMEOUT, SKIP_MODEL_CAP_EXCEEDED,
    SKIP_MODEL_FAILED, SKIP_NO_MODEL, SKIP_REASONING_EXHAUSTED_TOKENS, SKIP_REPLY_UNUSABLE,
    SKIP_TOO_LARGE, SKIP_UNREADABLE, SKIP_VISION_REFUSED, classify_embedding_error, embed_gated,
};
use crate::transport::embeddings_facade::EmbeddingsClient;
use crate::transport::model_facade::{
    ModelAdapterKind, ModelFacade, ModelFacadeError, ModelInvocation,
};
use crate::transport::model_text::{AdmittedChat, ReplyText, TextFailure, stream_reply};
use crate::transport::openai_compatible_facade::MAX_INLINE_IMAGE_BYTES;
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
    ) -> Result<AdmittedChat, ModelFacadeError>;
}

impl ChatModelBinder for ModelFacade {
    fn bind_chat(
        &self,
        context: &ClaimScopedEliteaContext,
        request: &ChatModelRequest<'_>,
    ) -> Result<AdmittedChat, ModelFacadeError> {
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
        .map(|bound| bound.admitted_chat())
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
    /// Stop the run with this sentence (a reply cut by `max_tokens`).
    Stop(String),
    /// The run's model call cap (or an image budget inside it) is spent.
    Cap(String),
    /// An image call that cannot be answered for this image alone: skip it
    /// under this closed reason. Never stops the run, never counts toward
    /// the failure streak.
    ImageSkip(&'static str),
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
            GateStop::Cap(reason) => Self::Cap(reason),
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
    /// An image call is also charged against the images' own budget.
    images: Option<(&'a AtomicU32, u32)>,
    alive: Alive<'a>,
}

impl Gate<'_> {
    /// Liveness, and when `charge` one call against the cap.
    fn check(&self, charge: bool) -> Result<(), GateStop> {
        (self.alive)().map_err(GateStop::Liveness)?;
        if charge {
            if let Some((spent, budget)) = self.images {
                spent
                    .try_update(Ordering::SeqCst, Ordering::SeqCst, |n| {
                        (n < budget).then_some(n + 1)
                    })
                    .map_err(|_| GateStop::Cap(cap_message(self.cap)))?;
            }
            self.calls
                .try_update(Ordering::SeqCst, Ordering::SeqCst, |n| {
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

/// Loads the chat model of a run: both `index.llm_model` and
/// `index.llm_configuration`, together, the first time a step needs them.
#[async_trait]
pub(crate) trait ChatLoader: Send + Sync {
    /// The binding (`None`: the claim names no model, or the deployment has
    /// no binder).
    ///
    /// # Errors
    ///
    /// Either entry could not be fetched or read: the run has no chat model.
    async fn load(&self) -> Result<Option<ChatBinding>, ChatUnavailable>;
}

/// The chat model entries could not be fetched or read.
#[derive(Debug, Clone, Copy)]
pub(crate) struct ChatUnavailable;

/// Where the run's chat model stands.
enum ChatSlot {
    /// Not asked for yet: no entry has been fetched.
    Unloaded,
    Loaded(Option<ChatBinding>),
    /// The entries could not be read: no model, and no default settings.
    Failed,
}

/// The most image calls of a run: half of the cap, rounded down, at least one
/// when the cap is at least one.
pub(crate) fn image_cap(cap: u32) -> u32 {
    if cap == 0 { 0 } else { (cap / 2).max(1) }
}

/// `vision_refused` images in a row after which the model is taken to accept
/// no images.
pub(crate) const REFUSED_IN_A_ROW: u32 = 3;

/// Whether a document's embedded pictures are worth asking for, as far as
/// the run knows now ([`ModelSteps::picture_gate_now`]).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum PictureGateNow {
    /// The chat model has not been loaded.
    Unknown,
    Open,
    Closed(&'static str),
}

/// The state of a run's model steps.
pub(crate) struct ModelSteps {
    chat: ChatSlot,
    cap: u32,
    calls: AtomicU32,
    /// Calls made for images, against [`image_cap`].
    image_calls: AtomicU32,
    consecutive_failures: u32,
    /// Behind a lock (never held across an await) because a document's
    /// pictures are described a few at a time.
    images: Mutex<ImageCache>,
    /// The chat model is not available for images: the rest of the run's
    /// images are skipped `no_model` without asking again.
    images_without_model: AtomicBool,
    /// Consecutive `vision_refused` images.
    refused_in_a_row: AtomicU32,
    /// The model refused [`REFUSED_IN_A_ROW`] images in a row: the rest are
    /// skipped `vision_refused` without a call.
    images_refused: AtomicBool,
}

impl ModelSteps {
    /// Steps whose chat model is loaded on first use through
    /// [`StepEnv::chat_loader`].
    pub(crate) fn lazy(limits: &IndexRunLimits) -> Self {
        Self::with_slot(ChatSlot::Unloaded, limits)
    }

    fn with_slot(chat: ChatSlot, limits: &IndexRunLimits) -> Self {
        Self {
            chat,
            cap: limits.max_model_calls,
            calls: AtomicU32::new(0),
            image_calls: AtomicU32::new(0),
            consecutive_failures: 0,
            images: Mutex::new(ImageCache::default()),
            images_without_model: AtomicBool::new(false),
            refused_in_a_row: AtomicU32::new(0),
            images_refused: AtomicBool::new(false),
        }
    }

    /// Loads the chat model once, on first need.
    async fn ensure_chat(&mut self, env: &StepEnv<'_>) {
        if !matches!(self.chat, ChatSlot::Unloaded) {
            return;
        }
        self.chat = match env.chat_loader {
            None => ChatSlot::Loaded(None),
            Some(loader) => match loader.load().await {
                Ok(binding) => ChatSlot::Loaded(binding),
                Err(ChatUnavailable) => ChatSlot::Failed,
            },
        };
    }

    fn chat_parts(&self) -> Option<(Arc<dyn ChatModelBinder>, String, LlmSettings)> {
        match &self.chat {
            ChatSlot::Loaded(Some(c)) => {
                Some((Arc::clone(&c.binder), c.model_name.clone(), c.settings))
            }
            _ => None,
        }
    }

    fn gate<'a>(&'a self, alive: Alive<'a>) -> Gate<'a> {
        Gate {
            calls: &self.calls,
            cap: self.cap,
            images: None,
            alive,
        }
    }

    fn image_gate<'a>(&'a self, alive: Alive<'a>) -> Gate<'a> {
        Gate {
            images: Some((&self.image_calls, image_cap(self.cap))),
            ..self.gate(alive)
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
        self.ensure_chat(env).await;
        if matches!(self.chat, ChatSlot::Failed) {
            return StepEnd::Stop(
                "the chat model entries (`index.llm_model`, `index.llm_configuration`) could not be read, and the proposal chunker needs them; nothing was chunked with default settings"
                    .to_owned(),
            );
        }
        let Some(chat) = self.chat_parts() else {
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
            Err(CallFailure::Stop(reason) | CallFailure::Cap(reason)) => StepEnd::Stop(reason),
            Err(CallFailure::Liveness(failure)) => StepEnd::Liveness(failure),
            // Only an image call yields this.
            Err(CallFailure::ImageSkip(reason)) => StepEnd::Skip(reason),
            Err(CallFailure::Retryable(cause) | CallFailure::Unusable(cause)) => {
                self.failed_document(&cause)
            }
        }
    }

    /// Describes one image file (ADR-0030 I4 part B): the chunks of the
    /// model's description, from the run's cache when the same image was
    /// described before with the same prompt and model.
    ///
    /// An image never stops the run for a reason of its own: everything
    /// image-specific is a skip with a precise reason, a failure of the model
    /// step on an image is never counted toward the proposal chunker's
    /// failure streak, and a success does not reset it. Only the refusals of
    /// the credential, the project or the budget stop the run.
    ///
    /// The cheap checks come first (the chat model, the digest and the cache,
    /// the image budget); the decode and resize of the picture only when a
    /// call will really be made.
    pub(crate) async fn describe(
        &mut self,
        raw: &RawImage,
        env: &StepEnv<'_>,
        alive: Alive<'_>,
    ) -> StepEnd {
        self.ensure_chat(env).await;
        match self
            .describe_one(&raw.bytes, raw.kind, raw.plan.prompt(), None, env, alive)
            .await
        {
            Ok(description) => vision_chunks(&raw.plan, &description),
            Err(end) => end,
        }
    }

    /// The model's description of one picture's `bytes` under `prompt`:
    /// the single path both an image FILE and a picture EMBEDDED in a
    /// document go through, so they share the cache, the image budget, the
    /// refused-model breaker, the downscale and the skip reasons. `digest` is
    /// the sha256 of `bytes` when the caller already has it.
    ///
    /// Takes `&self`, so a document's pictures can be described a few at a
    /// time; the chat model must already be loaded ([`Self::ensure_chat`] /
    /// [`Self::picture_gate_now`]).
    ///
    /// # Errors
    ///
    /// The step's end when there is no description: a skip (the picture's
    /// own reason), or the stop of the run for the credential, the project
    /// or the budget, or a lost lease.
    pub(crate) async fn describe_one(
        &self,
        bytes: &Bytes,
        kind: ImageKind,
        prompt: &str,
        digest: Option<[u8; 32]>,
        env: &StepEnv<'_>,
        alive: Alive<'_>,
    ) -> Result<String, StepEnd> {
        if self.images_without_model.load(Ordering::SeqCst) {
            return Err(StepEnd::Skip(SKIP_NO_MODEL));
        }
        let Some((binder, model_name, settings)) = self.chat_parts() else {
            self.images_without_model.store(true, Ordering::SeqCst);
            return Err(StepEnd::Skip(SKIP_NO_MODEL));
        };
        let digest = match digest {
            Some(digest) => digest,
            None => image_fit::digest(bytes.clone())
                .await
                .ok_or(StepEnd::Skip(SKIP_UNREADABLE))?,
        };
        let key = ImageKey::new(digest, prompt, &model_name);
        let cached = self
            .images
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .get(&key);
        if let Some(description) = cached {
            // No call was made: the cap is not charged.
            return Ok(description);
        }
        if self.images_refused.load(Ordering::SeqCst) {
            return Err(StepEnd::Skip(SKIP_VISION_REFUSED));
        }
        if self.image_calls.load(Ordering::SeqCst) >= image_cap(self.cap) {
            return Err(StepEnd::Skip(SKIP_MODEL_CAP_EXCEEDED));
        }
        match self.admit(1) {
            Ok(()) => {}
            Err(end @ StepEnd::Skip(_)) => return Err(end),
            // `admit` stops only when no call is left in the run's cap.
            Err(StepEnd::Stop(_)) => return Err(StepEnd::Skip(SKIP_MODEL_CAP_EXCEEDED)),
            Err(other) => return Err(other),
        }
        let fitted = match image_fit::fit(bytes.clone(), kind, MAX_INLINE_IMAGE_BYTES).await {
            Ok(fitted) => fitted,
            Err(FitError::TooLarge) => return Err(StepEnd::Skip(SKIP_TOO_LARGE)),
            Err(FitError::Unreadable) => return Err(StepEnd::Skip(SKIP_UNREADABLE)),
            Err(FitError::TimedOut) => return Err(StepEnd::Skip(SKIP_EXTRACT_TIMEOUT)),
        };
        let outcome = {
            let gate = self.image_gate(alive);
            vision_call(&*binder, env, &gate, &model_name, settings, prompt, &fitted).await
        };
        match outcome {
            Ok(description) => {
                self.refused_in_a_row.store(0, Ordering::SeqCst);
                self.images
                    .lock()
                    .unwrap_or_else(std::sync::PoisonError::into_inner)
                    .put(key, description.clone());
                Ok(description)
            }
            Err(CallFailure::Fatal(code)) => Err(StepEnd::Stop(format!(
                "the vision model refused the run ({code}); nothing further was sent to it"
            ))),
            Err(CallFailure::Liveness(failure)) => Err(StepEnd::Liveness(failure)),
            Err(CallFailure::ImageSkip(reason)) => {
                if reason == SKIP_NO_MODEL {
                    if !self.images_without_model.swap(true, Ordering::SeqCst) {
                        tracing::warn!(
                            event = "index_vision_model_unavailable",
                            "the chat model cannot be bound for images; the rest of the run's images are skipped no_model"
                        );
                    }
                } else if reason == SKIP_VISION_REFUSED {
                    let in_a_row = self.refused_in_a_row.fetch_add(1, Ordering::SeqCst) + 1;
                    if in_a_row >= REFUSED_IN_A_ROW
                        && !self.images_refused.swap(true, Ordering::SeqCst)
                    {
                        tracing::warn!(
                            event = "index_vision_model_refuses_images",
                            model = %model_name,
                            "the model refused several images in a row and is likely text-only; the rest of the run's images are skipped vision_refused without a call"
                        );
                    }
                }
                Err(StepEnd::Skip(reason))
            }
            // The run's cap or the images' budget: the picture is retried by
            // the next run.
            Err(CallFailure::Cap(_)) => Err(StepEnd::Skip(SKIP_MODEL_CAP_EXCEEDED)),
            // Not an image call's failure; reported as it is, never as the cap.
            Err(CallFailure::Stop(reason)) => Err(StepEnd::Stop(reason)),
            // A reply no other attempt will change (the model ended it for
            // a reason of its own, over the size bound): final for this
            // picture, so no later run is scheduled for it.
            Err(CallFailure::Unusable(cause)) => {
                tracing::warn!(event = "index_image_reply_unusable", cause);
                Err(StepEnd::Skip(SKIP_REPLY_UNUSABLE))
            }
            Err(CallFailure::Retryable(cause)) => {
                tracing::warn!(event = "index_image_description_failed", cause);
                Err(StepEnd::Skip(SKIP_MODEL_FAILED))
            }
        }
    }

    /// Whether a document's embedded pictures are worth asking the extractor
    /// for, BEFORE it is asked, as far as the run knows NOW: unknown while
    /// the chat model has not been loaded (the caller starts the load and
    /// asks again), closed (with the reason) when no picture of this run can
    /// be described and reading them would be wasted work, open when they
    /// can.
    ///
    /// Only the two FINAL reasons close the gate: no chat model, and a model
    /// that refuses images. A spent image budget does not: the pictures are
    /// still read, so a document that has some can be recorded as needing
    /// another run (its pictures would be described then), and a document
    /// with none is not.
    pub(crate) fn picture_gate_now(&self) -> PictureGateNow {
        if self.images_without_model.load(Ordering::SeqCst) {
            return PictureGateNow::Closed(SKIP_NO_MODEL);
        }
        if matches!(self.chat, ChatSlot::Unloaded) {
            return PictureGateNow::Unknown;
        }
        if self.chat_parts().is_none() {
            self.images_without_model.store(true, Ordering::SeqCst);
            return PictureGateNow::Closed(SKIP_NO_MODEL);
        }
        if self.images_refused.load(Ordering::SeqCst) {
            return PictureGateNow::Closed(SKIP_VISION_REFUSED);
        }
        PictureGateNow::Open
    }

    /// Takes the chat model a load (started elsewhere, so that the run did
    /// not wait for it) came back with. A model already loaded stays.
    pub(crate) fn install_chat(&mut self, loaded: Result<Option<ChatBinding>, ChatUnavailable>) {
        if !matches!(self.chat, ChatSlot::Unloaded) {
            return;
        }
        self.chat = match loaded {
            Ok(binding) => ChatSlot::Loaded(binding),
            Err(ChatUnavailable) => ChatSlot::Failed,
        };
    }

    /// Loads the chat model, for callers that describe pictures through
    /// [`Self::describe_one`] without going through [`Self::describe`].
    pub(crate) async fn load_chat(&mut self, env: &StepEnv<'_>) {
        self.ensure_chat(env).await;
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
    /// Loads the chat model on first need; `None`: the run has none.
    pub(crate) chat_loader: Option<&'a dyn ChatLoader>,
}

/// One image file waiting for its description, as it was fetched.
pub(crate) struct RawImage {
    /// The ORIGINAL file. Shared: hashed and fitted without a copy.
    pub(crate) bytes: Bytes,
    pub(crate) kind: ImageKind,
    pub(crate) plan: VisionPlan,
}

fn vision_chunks(plan: &VisionPlan, description: &str) -> StepEnd {
    match plan.chunks(description) {
        Ok(chunks) => StepEnd::Chunks(chunks),
        Err(error) => {
            tracing::warn!(event = "index_image_description_unchunkable", %error);
            StepEnd::Skip(SKIP_VISION_REFUSED)
        }
    }
}

/// What one family of calls sends and how it treats a model that says no.
struct CallSpec<'a> {
    system: &'a str,
    /// The request content. Called once per attempt, so a retry builds it
    /// again from the shared source instead of holding a copy.
    content: &'a (dyn Fn() -> Content + Sync),
    /// An image call: its refusals that are not the credential's, the
    /// project's or the budget's are the image's alone, and an unbindable
    /// model is `no_model`, never a failed run.
    image: bool,
    /// How the reply's text is handed to `read`: the proposal chunker takes
    /// it RAW and applies its own reasoning rule (#1292 round 4).
    text: ReplyText,
}

/// What a model replied: its text, and whether `max_tokens` cut it off.
struct Reply {
    text: String,
    truncated: bool,
    /// Nothing but reasoning came back (see `TextReply::reasoning_only`).
    reasoning_only: bool,
}

/// The reply to `spec`, with the attempts the policy allows: every attempt
/// is charged before it is made, liveness is checked before every attempt
/// and every retry wait, a retryable failure (or a reply `read` calls
/// retryable) backs off and goes again, anything else ends the call.
async fn call<T>(
    binder: &dyn ChatModelBinder,
    env: &StepEnv<'_>,
    gate: &Gate<'_>,
    model_name: &str,
    settings: LlmSettings,
    spec: &CallSpec<'_>,
    read: impl Fn(Reply) -> Result<T, CallFailure>,
) -> Result<T, CallFailure> {
    let mut attempt = 0_u32;
    loop {
        attempt += 1;
        gate.check(true)?;
        let chat = binder
            .bind_chat(
                env.context,
                &ChatModelRequest {
                    model_name,
                    system_instruction: spec.system,
                    max_tokens: settings.max_tokens,
                    temperature: settings.temperature,
                },
            )
            .map_err(|error| {
                if spec.image {
                    tracing::warn!(event = "index_vision_bind_failed", ?error);
                    CallFailure::ImageSkip(SKIP_NO_MODEL)
                } else {
                    CallFailure::Fatal(format!("model_facade.{error:?}"))
                }
            })?;
        let failure =
            match complete(&chat, model_name, (spec.content)(), spec.image, spec.text).await {
                Ok(reply) => match read(reply) {
                    Ok(value) => return Ok(value),
                    Err(failure) => failure,
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

/// The description of one image: one chat call with the prompt and the image
/// (the SDK's single `HumanMessage`: the text part, then the image part). A
/// reply cut by `max_tokens` is kept, as the SDK keeps it.
async fn vision_call(
    binder: &dyn ChatModelBinder,
    env: &StepEnv<'_>,
    gate: &Gate<'_>,
    model_name: &str,
    settings: LlmSettings,
    prompt: &str,
    fitted: &image_fit::Fitted,
) -> Result<String, CallFailure> {
    let content = || Content {
        role: "user".to_owned(),
        parts: vec![
            Part::Text {
                text: prompt.to_owned(),
            },
            Part::InlineData {
                mime_type: fitted.media_type.to_owned(),
                // The request API takes an owned buffer: the one copy an
                // attempt makes, dropped with the request.
                data: fitted.bytes.to_vec(),
                uri: None,
                annotations: None,
            },
        ],
    };
    let spec = CallSpec {
        system: "",
        content: &content,
        image: true,
        text: ReplyText::Answer,
    };
    call(
        binder,
        env,
        gate,
        model_name,
        settings,
        &spec,
        |reply: Reply| {
            // Reasoning the model wrote into its reply is already removed.
            // A reply that held nothing but reasoning and was CUT by
            // `max_tokens`: the model spent its budget before it began to
            // describe. The same request (same `max_tokens`) is cut the same
            // way, in this run or the next, so it is a FINAL skip of its own
            // (`reasoning_exhausted_tokens`, no pending mark), and the
            // summary says to raise `max_tokens`.
            if reply.reasoning_only && reply.truncated {
                tracing::warn!(
                    event = "index_image_description_reasoning_exhausted_tokens",
                    max_tokens = settings.max_tokens,
                    "the model spent max_tokens on reasoning and described nothing; raise `max_tokens` in the toolkit's llm settings"
                );
                return Err(CallFailure::ImageSkip(SKIP_REASONING_EXHAUSTED_TOKENS));
            }
            // Reasoning only, not cut (a `<think>` never closed, a reply that
            // was only a closed block): retried by the next run.
            if reply.reasoning_only {
                tracing::warn!(
                    event = "index_image_description_reasoning_only",
                    max_tokens = settings.max_tokens,
                    "the model produced only reasoning and described nothing (cut off by max_tokens while thinking?); raise `max_tokens` in the toolkit's llm settings, the picture is retried by the next run"
                );
                return Err(CallFailure::ImageSkip(SKIP_MODEL_FAILED));
            }
            if reply.truncated {
                tracing::warn!(
                    event = "index_image_description_truncated",
                    max_tokens = settings.max_tokens,
                    "the model's description was cut off by max_tokens; kept as it is"
                );
            }
            let description = reply.text;
            Ok(description)
        },
    )
    .await
}

/// Descriptions kept for a run.
const IMAGE_CACHE_ENTRIES: usize = 500;
/// A description longer than this is used but not cached (500 entries of the
/// largest reply would be 128 MiB).
const IMAGE_CACHE_MAX_DESCRIPTION_BYTES: usize = 32 * 1024;

/// sha256(original bytes) + sha256(prompt) + the model's name.
#[derive(Clone, PartialEq, Eq, Hash)]
struct ImageKey([u8; 32], [u8; 32], String);

impl ImageKey {
    fn new(digest: [u8; 32], prompt: &str, model: &str) -> Self {
        Self(
            digest,
            Sha256::digest(prompt.as_bytes()).into(),
            model.to_owned(),
        )
    }
}

/// The run's image descriptions: a least-recently-used map of
/// [`IMAGE_CACHE_ENTRIES`], O(1) amortised per operation. Each entry
/// carries the generation of its last use; the queue holds `(key,
/// generation)` records oldest first, and a record whose generation is no
/// longer its entry's is stale and skipped when eviction reaches it. Per
/// run, in memory, never logged.
#[derive(Default)]
struct ImageCache {
    entries: HashMap<Arc<ImageKey>, (String, u64)>,
    order: VecDeque<(Arc<ImageKey>, u64)>,
    generation: u64,
}

impl ImageCache {
    fn touch(&mut self, key: &Arc<ImageKey>) -> u64 {
        self.generation += 1;
        self.order.push_back((Arc::clone(key), self.generation));
        // Hits leave stale records behind: drop them before they pile up.
        if self.order.len() > IMAGE_CACHE_ENTRIES * 4 {
            let entries = &self.entries;
            self.order
                .retain(|(k, g)| entries.get(k).is_some_and(|(_, current)| current == g));
        }
        self.generation
    }

    fn get(&mut self, key: &ImageKey) -> Option<String> {
        let (shared, (hit, _)) = self
            .entries
            .get_key_value(key)
            .map(|(k, v)| (Arc::clone(k), v.clone()))?;
        let generation = self.touch(&shared);
        if let Some(entry) = self.entries.get_mut(key) {
            entry.1 = generation;
        }
        Some(hit)
    }

    /// Like the SDK, a blank description is not kept.
    fn put(&mut self, key: ImageKey, description: String) {
        if description.trim().is_empty() || description.len() > IMAGE_CACHE_MAX_DESCRIPTION_BYTES {
            return;
        }
        let key = Arc::new(key);
        if self.entries.len() >= IMAGE_CACHE_ENTRIES && !self.entries.contains_key(&*key) {
            while let Some((oldest, generation)) = self.order.pop_front() {
                if self
                    .entries
                    .get(&oldest)
                    .is_some_and(|(_, current)| *current == generation)
                {
                    self.entries.remove(&oldest);
                    break;
                }
            }
        }
        let generation = self.touch(&key);
        self.entries.insert(key, (description, generation));
    }
}

/// The model's `ChunkDetails` for one window, with the attempts the policy
/// allows (see [`call`]). An answer that is not the requested JSON is a
/// retryable failure; a reply cut off by `max_tokens` stops the run (another
/// attempt would be cut off too).
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
    let content = || Content::new("user").with_text(&user);
    let spec = CallSpec {
        system: &system,
        content: &content,
        image: false,
        text: ReplyText::Raw,
    };
    call(
        binder,
        env,
        gate,
        model_name,
        settings,
        &spec,
        |reply: Reply| {
            if reply.truncated {
                let in_use = settings.max_tokens.map_or_else(
                    || "unset, so the model's default".to_owned(),
                    |n| n.to_string(),
                );
                return Err(CallFailure::Stop(format!(
                    "the index model's reply was cut off by its max_tokens limit (the toolkit's llm `max_tokens` is {in_use}); raise `max_tokens` in the toolkit's llm settings and re-run"
                )));
            }
            parse_chunk_analysis(&reply.text)
                .map_err(|error| CallFailure::Retryable(error.to_string()))
        },
    )
    .await
}

/// One chat call through the shared admitted-request helper
/// ([`stream_reply`]): streaming, with the config the bound model admits.
async fn complete(
    chat: &AdmittedChat,
    model_name: &str,
    user: Content,
    image: bool,
    text: ReplyText,
) -> Result<Reply, CallFailure> {
    match stream_reply(chat, model_name, user, MAX_REPLY_BYTES, text).await {
        Ok(reply) => Ok(Reply {
            text: reply.text,
            truncated: reply.length_cut,
            reasoning_only: reply.reasoning_only,
        }),
        Err(TextFailure::Model(error)) => Err(classify_model_error(&error, image)),
        Err(TextFailure::ErrorCode) => Err(CallFailure::Retryable(
            "the model ended the reply with an error".to_owned(),
        )),
        Err(TextFailure::Stopped(reason)) => {
            let cause = format!("the model stopped for {reason:?}");
            // An image the model declined to answer (its safety filter, a
            // recitation stop) is declined again by the next run: final.
            // Any other stop, and a document step, keep their older meaning.
            Err(
                if image && matches!(reason, FinishReason::Safety | FinishReason::Recitation) {
                    CallFailure::Unusable(cause)
                } else {
                    CallFailure::Retryable(cause)
                },
            )
        }
        Err(TextFailure::TooLarge) => Err(if image {
            CallFailure::ImageSkip(SKIP_VISION_REFUSED)
        } else {
            CallFailure::Unusable("the model's reply exceeds the size bound".to_owned())
        }),
        Err(TextFailure::NoReply) => Err(CallFailure::Retryable(
            "the model returned no reply".to_owned(),
        )),
        Err(TextFailure::BudgetExhausted) => Err(CallFailure::Cap(
            crate::transport::model_text::TOOLKIT_BUDGET_EXHAUSTED.to_owned(),
        )),
    }
}

/// The gateway's refusal, sorted: retryable (429, 5xx, a timeout, the
/// transport) fails the document; a size bound of this request or reply
/// fails the document; anything else (the credential, the project, the
/// budget, an unknown model, another 4xx) cannot be cured by another attempt
/// and stops the run. The code is the facade's stable one; no provider text
/// is read.
///
/// For an IMAGE call only the credential, the project and the budget stop
/// the run: the gateway's generic rejection (a model that takes no images, a
/// provider's 400 for one picture) is that picture's alone, `vision_refused`,
/// and a request over the gateway's size bound is `too_large`.
fn classify_model_error(error: &AdkError, image: bool) -> CallFailure {
    tracing::warn!(
        event = "index_model_call_failed",
        error_code = error.code,
        retryable = error.is_retryable(),
    );
    if error.is_retryable() {
        return CallFailure::Retryable(error.code.to_owned());
    }
    let too_large = matches!(
        error.code,
        "model_gateway.event_too_large"
            | "model_gateway.stream_too_large"
            | "model_gateway.request_too_large"
    );
    if image {
        return match error.code {
            _ if too_large => CallFailure::ImageSkip(SKIP_TOO_LARGE),
            "model_gateway.unauthorized"
            | "model_gateway.forbidden"
            | "model_gateway.budget_exhausted"
            | "model_gateway.member_budget_exhausted"
            | "model_gateway.project_budget_exhausted"
            | "model_gateway.resource_exhausted" => CallFailure::Fatal(error.code.to_owned()),
            _ => CallFailure::ImageSkip(SKIP_VISION_REFUSED),
        };
    }
    if too_large {
        CallFailure::Unusable(error.code.to_owned())
    } else {
        CallFailure::Fatal(error.code.to_owned())
    }
}

#[cfg(test)]
mod cache_tests {
    use super::*;

    fn key(n: u8) -> ImageKey {
        ImageKey::new([n; 32], "prompt", "model")
    }

    #[test]
    fn the_least_recently_used_description_is_evicted() {
        let mut cache = ImageCache::default();
        let many = |n: usize| ImageKey::new([0; 32], &format!("p{n}"), "m");
        for n in 0..IMAGE_CACHE_ENTRIES {
            cache.put(many(n), format!("d{n}"));
        }
        // Touch the oldest: the next insert evicts the second oldest.
        assert_eq!(cache.get(&many(0)).as_deref(), Some("d0"));
        cache.put(many(IMAGE_CACHE_ENTRIES), "new".to_owned());
        assert_eq!(cache.entries.len(), IMAGE_CACHE_ENTRIES);
        assert!(cache.get(&many(1)).is_none());
        assert_eq!(cache.get(&many(0)).as_deref(), Some("d0"));
        assert_eq!(
            cache.get(&many(IMAGE_CACHE_ENTRIES)).as_deref(),
            Some("new")
        );
    }

    #[test]
    fn hits_do_not_grow_the_queue_without_bound() {
        let mut cache = ImageCache::default();
        cache.put(key(1), "one".to_owned());
        cache.put(key(2), "two".to_owned());
        for _ in 0..50_000 {
            assert!(cache.get(&key(1)).is_some());
        }
        assert!(
            cache.order.len() <= IMAGE_CACHE_ENTRIES * 4 + 1,
            "{}",
            cache.order.len()
        );
        assert_eq!(cache.get(&key(2)).as_deref(), Some("two"));
    }

    #[test]
    fn blank_and_huge_descriptions_are_not_kept() {
        let mut cache = ImageCache::default();
        cache.put(key(1), "  ".to_owned());
        cache.put(key(2), "x".repeat(IMAGE_CACHE_MAX_DESCRIPTION_BYTES + 1));
        assert!(cache.entries.is_empty());
    }
}
