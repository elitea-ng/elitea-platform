//! The index steps that call a model (ADR-0030 I4): the proposal chunker's
//! chat calls and the statistical chunker's embeddings.
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
//!     the next run retries it, and [`MAX_CONSECUTIVE_MODEL_FAILURES`] in a
//!     row stop the run;
//!   - a refusal another attempt cannot cure (the credential, the project,
//!     the budget, an unknown model, any other 4xx) stops the run, its
//!     message carrying the gateway's stable error code;
//!   - no chat model bound (no `index.llm_model` entry, or a deployment
//!     without model access) skips the proposal chunker's documents as
//!     `no_model`;
//! * the per-run cap on model calls ([`IndexRunLimits::max_model_calls`]):
//!   one call per proposal window and per statistical embeddings request.
//!   Reaching it stops the run. The documents already written stay.

use std::sync::Arc;

use adk_rust::futures::StreamExt as _;
use adk_rust::{AdkError, Content, FinishReason, Llm, LlmRequest, Part};
use elitea_doc_chunk::{
    AnalysisError, Chunk, ChunkDetails, ChunkError, ChunkPlan, ProposalPlan, StatisticalPlan,
    parse_chunk_analysis, proposal_system_prompt, proposal_user_message,
};

use super::IndexRunLimits;
use super::inputs::LlmSettings;
use super::pipeline::{
    EmbedOutcome, IndexRunFailure, SKIP_MODEL_FAILED, SKIP_NO_MODEL, SKIP_TOO_LARGE,
    classify_embedding_error, embed_with_retry,
};
use crate::agents::session::BoundOrdinaryAgentModel as _;
use crate::transport::embeddings_facade::EmbeddingsClient;
use crate::transport::model_facade::{
    ModelAdapterKind, ModelFacade, ModelFacadeError, ModelInvocation,
};
use crate::transport::runtime_context::ClaimScopedEliteaContext;

/// Documents in a row skipped `model_failed` before the run stops: a model
/// outage must not end as every document skipped.
pub(crate) const MAX_CONSECUTIVE_MODEL_FAILURES: u32 = 5;
/// Attempts per proposal window for a retryable failure or an unreadable
/// answer.
const CHAT_ATTEMPTS: u32 = 3;
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
}

/// The state of a run's model steps.
pub(crate) struct ModelSteps {
    chat: Option<ChatBinding>,
    cap: u32,
    calls: u32,
    consecutive_failures: u32,
}

impl ModelSteps {
    pub(crate) fn new(chat: Option<ChatBinding>, limits: &IndexRunLimits) -> Self {
        Self {
            chat,
            cap: limits.max_model_calls,
            calls: 0,
            consecutive_failures: 0,
        }
    }

    fn charge(&mut self) -> Result<(), String> {
        if self.calls >= self.cap {
            return Err(format!(
                "the run reached its limit of {} model calls (one per proposal window and per statistical embeddings request); the documents written so far are kept; re-run to continue, or raise the deployment's index model call cap",
                self.cap
            ));
        }
        self.calls += 1;
        Ok(())
    }

    /// Fulfils `plan`. `alive` is checked before every call.
    pub(crate) async fn fulfil(
        &mut self,
        plan: ChunkPlan,
        env: &StepEnv<'_>,
        alive: &(dyn Fn() -> Result<(), IndexRunFailure> + Sync),
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
        alive: &(dyn Fn() -> Result<(), IndexRunFailure> + Sync),
    ) -> StepEnd {
        let Some(chat) = self
            .chat
            .as_ref()
            .map(|c| (Arc::clone(&c.binder), c.model_name.clone(), c.settings))
        else {
            return StepEnd::Skip(SKIP_NO_MODEL);
        };
        let (binder, model_name, settings) = chat;
        let mut answers: Vec<Vec<ChunkDetails>> = Vec::with_capacity(plan.windows().len());
        for window in plan.windows() {
            if let Err(failure) = alive() {
                return StepEnd::Liveness(failure);
            }
            if let Err(reason) = self.charge() {
                return StepEnd::Stop(reason);
            }
            match chat_window(&*binder, env, &model_name, settings, window).await {
                Ok(details) => answers.push(details),
                Err(CallFailure::Fatal(code)) => {
                    return StepEnd::Stop(format!(
                        "the chunking model refused the run ({code}); nothing further was sent to it"
                    ));
                }
                Err(CallFailure::Retryable(cause) | CallFailure::Unusable(cause)) => {
                    return self.failed_document(&cause);
                }
            }
        }
        match plan.chunks(&answers) {
            Ok(chunks) => {
                self.consecutive_failures = 0;
                StepEnd::Chunks(chunks)
            }
            Err(error) => self.failed_document(&error.to_string()),
        }
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
        alive: &(dyn Fn() -> Result<(), IndexRunFailure> + Sync),
    ) -> StepEnd {
        let batch = plan
            .batch_size()
            .clamp(1, crate::transport::embeddings_facade::MAX_BATCH_INPUTS);
        let mut vectors: Vec<Vec<f32>> = Vec::with_capacity(plan.splits().len());
        for inputs in plan.splits().chunks(batch) {
            if let Err(failure) = alive() {
                return StepEnd::Liveness(failure);
            }
            if let Err(reason) = self.charge() {
                return StepEnd::Stop(reason);
            }
            let embedded = embed_with_retry(
                env.embeddings,
                env.context,
                env.embedding_model,
                inputs,
                env.limits.embedding_retry_backoff,
            )
            .await;
            match embedded {
                Ok(embedded) => vectors.extend(embedded.vectors),
                Err(error) => {
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
            Ok(chunks) => StepEnd::Chunks(chunks),
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

/// The model's `ChunkDetails` for one window, with the attempts the policy
/// allows.
async fn chat_window(
    binder: &dyn ChatModelBinder,
    env: &StepEnv<'_>,
    model_name: &str,
    settings: LlmSettings,
    window: &str,
) -> Result<Vec<ChunkDetails>, CallFailure> {
    let system = proposal_system_prompt();
    let user = proposal_user_message(window);
    let mut attempt = 0_u32;
    loop {
        attempt += 1;
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
        let failure = match complete(&*model, model_name, &user).await {
            Ok(reply) => match parse_chunk_analysis(&reply) {
                Ok(details) => return Ok(details),
                Err(error) => CallFailure::Retryable(analysis_text(&error)),
            },
            Err(failure) => failure,
        };
        match failure {
            CallFailure::Retryable(_) if attempt < CHAT_ATTEMPTS => {
                tokio::time::sleep(env.limits.embedding_retry_backoff * attempt).await;
            }
            other => return Err(other),
        }
    }
}

fn analysis_text(error: &AnalysisError) -> String {
    error.to_string()
}

/// One non-streaming call, as the step-back model makes it: a streaming call
/// would hand back partial deltas and a final response carrying the whole
/// text.
async fn complete(model: &dyn Llm, model_name: &str, user: &str) -> Result<String, CallFailure> {
    let request = LlmRequest {
        model: model_name.to_owned(),
        contents: vec![Content::new("user").with_text(user)],
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
