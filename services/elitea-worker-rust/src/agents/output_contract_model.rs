//! Output contract enforcement on the model (ADR-0043 decision 2, #1500).
//!
//! [`OutputContractModel`] wraps the bound provider model of an agent or a
//! pipeline LLM node that declares an output contract
//! (`elitea_agent_runtime::output_contract`). It holds each model turn until
//! the turn is complete:
//!
//! * a turn that calls a tool passes through;
//! * an output continuation call of a pipeline node passes through: the node
//!   checks the joined answer itself (`graph::llm`). Only the code that issues
//!   the continuation (`model_scope_output`) marks a call as a continuation,
//!   with [`continuation_call`]. The text of the request never marks it, so an
//!   end user cannot skip the contract with the continuation prompt. An agent
//!   never has a continuation call (see the next item), so a marked call of an
//!   agent is validated as a full answer;
//! * a turn that the provider stopped at its output token limit passes
//!   through for a pipeline node (the output continuation owns that text).
//!   For an agent it is an invalid answer (`output_limit`): an agent's
//!   continued answer is never joined in one place, so the contract could
//!   not see it;
//! * a final answer is validated. A valid answer passes through. The first
//!   invalid answer is dropped (never streamed, never stored) and the model
//!   is asked once more, with the answer and the validation error as
//!   feedback. A second invalid answer ends the agent or node with
//!   [`elitea_agent_runtime::output_contract::VIOLATION_CODE`], so a pipeline
//!   never invokes the next node. Each final answer gets its own retry: after
//!   the binding accepts an answer, the next final answer is attempt 1 again.
//!
//! An agent or node with a contract therefore does not stream its answer:
//! the answer reaches the caller only after it was validated.
//!
//! Every validation is one step record (ADR-0042): kind `output`, name
//! [`CHECK_STEP_NAME`], with the contract type, the attempt, the result and,
//! for a failure, the failed keyword and the JSON pointer inside the answer.
//! The answer itself is not in the record: the `model_call` step of the turn
//! already holds it under the step content rules.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use adk_rust::futures::{StreamExt as _, stream};
use adk_rust::{
    AdkError, Content, FinishReason, Llm, LlmRequest, LlmResponse, LlmResponseStream, Part,
    SchemaAdapter,
};
use async_trait::async_trait;
use elitea_agent_runtime::output_contract::{
    MAX_ATTEMPTS, OutputContract, Violation, violation_error,
};
use serde_json::{Value, json};

use super::session::DurableModelCompletion;
use crate::protocol::elitea::runtime::v1::RunStepKindV1;
use crate::protocol::run_steps::{RunStepRegistry, StepDraft, now_unix_ms};

/// The name of a contract validation step.
pub(crate) const CHECK_STEP_NAME: &str = "output_contract_check";

tokio::task_local! {
    /// Set only while `model_scope_output` dispatches an output continuation
    /// call. A task-local value is internal state: request text cannot set it.
    static CONTINUATION_CALL: ();
}

/// Dispatch `call` as an output continuation call. Only the code that issues
/// the continuation (`model_scope_output`) calls this function.
pub(crate) async fn continuation_call<F: std::future::Future>(call: F) -> F::Output {
    CONTINUATION_CALL.scope((), call).await
}

/// Whether the current model call is an output continuation call.
pub(crate) fn is_continuation_call() -> bool {
    CONTINUATION_CALL.try_with(|()| ()).is_ok()
}

/// Where the validation steps of one binding go.
#[derive(Clone, Debug, Default)]
pub(crate) struct ContractStepScope {
    /// The execution whose recorder takes the steps; empty records nothing.
    pub(crate) execution_id: String,
    /// The pipeline node; empty for an agent. A node checks an answer
    /// joined from output continuations itself; an agent does not.
    pub(crate) node_id: String,
}

/// The bound model of one agent or node with an output contract.
pub(crate) struct OutputContractModel {
    inner: Arc<dyn Llm>,
    contract: Arc<OutputContract>,
    completion: Option<Arc<dyn DurableModelCompletion>>,
    scope: ContractStepScope,
    /// One retry per final answer: set after an invalid answer, cleared
    /// when the binding accepts a final answer. While it is set, the next
    /// invalid final answer ends the run. The retry can call a tool first, so
    /// the flag lives across calls.
    retried: AtomicBool,
}

impl OutputContractModel {
    pub(crate) fn new(
        inner: Arc<dyn Llm>,
        contract: Arc<OutputContract>,
        completion: Option<Arc<dyn DurableModelCompletion>>,
        scope: ContractStepScope,
    ) -> Self {
        Self {
            inner,
            contract,
            completion,
            scope,
            retried: AtomicBool::new(false),
        }
    }

    fn record(&self, attempt: u32, outcome: Result<(), &Violation>) {
        if self.scope.execution_id.is_empty() {
            return;
        }
        let Some(recorder) = RunStepRegistry::global().find_execution(&self.scope.execution_id)
        else {
            return;
        };
        let mut step = StepDraft::new(RunStepKindV1::Output, CHECK_STEP_NAME, now_unix_ms())
            .summary(
                "contract_type",
                json!(self.contract.contract_type().as_str()),
            )
            .summary("attempt", json!(attempt))
            .summary("valid", json!(outcome.is_ok()));
        if let Err(violation) = outcome {
            step = step
                .summary("keyword", json!(violation.keyword()))
                .summary("path", json!(violation.path()))
                .summary("final", json!(attempt >= MAX_ATTEMPTS));
        }
        step.node_id.clone_from(&self.scope.node_id);
        recorder.record(step);
    }
}

#[async_trait]
impl Llm for OutputContractModel {
    fn name(&self) -> &str {
        self.inner.name()
    }

    fn schema_adapter(&self) -> &dyn SchemaAdapter {
        self.inner.schema_adapter()
    }

    fn uses_interactions_api(&self) -> bool {
        self.inner.uses_interactions_api()
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        stream_response: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        if is_continuation_call() && !self.scope.node_id.is_empty() {
            return self.inner.generate_content(request, stream_response).await;
        }
        let mut request = request;
        // The JSON Schema is the response format where the binding admits one
        // and the caller set none (an agent; a node sets it itself).
        if let (Some(schema), Some(config)) =
            (self.contract.response_schema(), request.config.as_mut())
            && config.response_schema.is_none()
        {
            config.response_schema = Some(schema.clone());
        }
        let first = collect(
            self.inner
                .generate_content(request.clone(), stream_response)
                .await?,
        )
        .await?;
        let Some(answer) = self.final_answer(&first) else {
            return Ok(replay(first));
        };
        let attempt = if self.retried.load(Ordering::Acquire) {
            MAX_ATTEMPTS
        } else {
            1
        };
        let outcome = answer
            .as_ref()
            .map_err(Clone::clone)
            .and_then(|text| self.contract.validate(text));
        let violation = match outcome {
            Ok(()) => {
                self.record(attempt, Ok(()));
                // The next final answer gets its own retry.
                self.retried.store(false, Ordering::Release);
                return Ok(replay(first));
            }
            Err(violation) => violation,
        };
        self.record(attempt, Err(&violation));
        if self.retried.swap(true, Ordering::AcqRel) {
            self.retried.store(false, Ordering::Release);
            return Err(violation_error());
        }
        // The rejected answer was never accepted: the bound facade must take
        // the retry's answer as the turn's completion.
        if let Some(completion) = &self.completion {
            completion.discard_unaccepted()?;
        }
        let mut retry = request;
        if let Ok(text) = answer {
            retry.contents.push(Content::new("model").with_text(text));
        }
        retry
            .contents
            .push(Content::new("user").with_text(violation.feedback()));
        let second = collect(self.inner.generate_content(retry, stream_response).await?).await?;
        let Some(answer) = self.final_answer(&second) else {
            // The model called a tool on the retry: the agent loop goes on,
            // and its next final answer is the last attempt.
            return Ok(replay(second));
        };
        // The retry gave a final answer: this answer's retry is spent.
        self.retried.store(false, Ordering::Release);
        match answer.and_then(|answer| self.contract.validate(&answer)) {
            Ok(()) => {
                self.record(MAX_ATTEMPTS, Ok(()));
                Ok(replay(second))
            }
            Err(violation) => {
                self.record(MAX_ATTEMPTS, Err(&violation));
                Err(violation_error())
            }
        }
    }
}

async fn collect(mut responses: LlmResponseStream) -> adk_rust::Result<Vec<LlmResponse>> {
    let mut collected = Vec::new();
    while let Some(response) = responses.next().await {
        collected.push(response?);
    }
    Ok(collected)
}

fn replay(responses: Vec<LlmResponse>) -> LlmResponseStream {
    Box::pin(stream::iter(responses.into_iter().map(Ok::<_, AdkError>)))
}

impl OutputContractModel {
    /// The final answer of a turn: `None` for a turn that passes through (a
    /// tool call, or a node's answer cut at the output token limit); the
    /// `output_limit` violation for an agent's cut answer.
    fn final_answer(&self, responses: &[LlmResponse]) -> Option<Result<String, Violation>> {
        if responses
            .iter()
            .filter_map(|response| response.content.as_ref())
            .flat_map(|content| content.parts.iter())
            .any(|part| matches!(part, Part::FunctionCall { .. }))
        {
            return None;
        }
        if responses
            .iter()
            .any(|response| response.finish_reason == Some(FinishReason::MaxTokens))
        {
            return self
                .scope
                .node_id
                .is_empty()
                .then(|| Err(Violation::output_limit()));
        }
        Some(Ok(answer_text(responses)))
    }
}

/// The text of a complete answer turn.
fn answer_text(responses: &[LlmResponse]) -> String {
    let text_of = |response: &LlmResponse| -> String {
        response
            .content
            .iter()
            .flat_map(|content| content.parts.iter())
            .filter_map(Part::text)
            .collect()
    };
    // A non-partial response that restates the whole turn wins.
    if let Some(complete) = responses.iter().rev().find(|response| {
        !response.partial
            && response
                .provider_metadata
                .as_ref()
                .and_then(|metadata| metadata.get("content_complete"))
                .and_then(Value::as_bool)
                == Some(true)
    }) {
        return text_of(complete);
    }
    let streamed: String = responses
        .iter()
        .filter(|response| response.partial)
        .map(text_of)
        .collect();
    if !streamed.is_empty() {
        return streamed;
    }
    responses.iter().map(text_of).collect()
}

#[cfg(test)]
#[path = "output_contract_model_tests.rs"]
mod tests;
