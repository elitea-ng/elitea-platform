//! Bounded Code progress. Runtime operations, not graph state, produce phase results.

use super::{
    code::CodeLanguage,
    node_events::{
        PIPELINE_NODE_EVENT_SCOPE_STATE_KEY, PipelineNodeEventScope, PipelineNodeEventSender,
    },
};
use adk_rust::{
    Event,
    graph::{GraphError, NodeContext},
};
use chrono::{DateTime, Utc};
use ring::digest;
use serde::{Deserialize, Serialize};
use std::future::Future;

pub(crate) const CODE_TRACE_METADATA_KEY: &str = "elitea.code.lifecycle.v1";
const MAX_ENCODED_BYTES: usize = 2048;

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "lowercase")]
pub(crate) enum CodePhase {
    Preparation,
    Hydration,
    Execution,
}
impl CodePhase {
    pub(crate) fn name(self) -> &'static str {
        match self {
            Self::Preparation => "preparation",
            Self::Hydration => "hydration",
            Self::Execution => "execution",
        }
    }
}
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "lowercase")]
pub(crate) enum CodePhaseStatus {
    Started,
    Completed,
    Failed,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct CodeLifecycle {
    pub(crate) revision: u8,
    pub(crate) execution_id: String,
    pub(crate) generation: String,
    pub(crate) activation_id: String,
    pub(crate) node_id: String,
    pub(crate) graph_thread_id: String,
    pub(crate) graph_step: String,
    pub(crate) language: String,
    pub(crate) phase: CodePhase,
    pub(crate) status: CodePhaseStatus,
}
impl CodeLifecycle {
    pub(crate) fn run_id(&self) -> String {
        let mut hash = digest::Context::new(&digest::SHA256);
        hash.update(b"elitea.graph.code.trace.v1\0");
        for field in [
            self.execution_id.as_str(),
            self.generation.as_str(),
            self.activation_id.as_str(),
            self.phase.name(),
        ] {
            hash.update(&(field.len() as u64).to_be_bytes());
            hash.update(field.as_bytes());
        }
        format!("code-{}", hex(hash.finish().as_ref()))
    }
    fn validate(&self) -> Result<(), GraphError> {
        if self.revision != 1
            || !identity(&self.execution_id, 256)
            || !decimal(&self.generation, true)
            || !decimal(&self.graph_step, false)
            || !identity(&self.graph_thread_id, 512)
            || !super::yaml::valid_graph_id(&self.node_id)
            || self.activation_id.len() != 64
            || !self
                .activation_id
                .bytes()
                .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
            || !matches!(
                self.language.as_str(),
                "python" | "javascript" | "typescript" | "rust"
            )
        {
            return Err(invalid());
        }
        Ok(())
    }
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct CodeTraceEvent {
    pub(crate) lifecycle: CodeLifecycle,
    pub(crate) started_at: DateTime<Utc>,
}
impl CodeTraceEvent {
    pub(crate) fn from_event(event: &Event) -> Result<Option<Self>, GraphError> {
        let Some(encoded) = event.provider_metadata.get(CODE_TRACE_METADATA_KEY) else {
            return Ok(None);
        };
        if encoded.len() > MAX_ENCODED_BYTES
            || event.content().is_some()
            || !event.llm_response.partial
            || event.llm_response.turn_complete
            || !event.actions.state_delta.is_empty()
        {
            return Err(invalid());
        }
        let trace: Self = serde_json::from_str(encoded).map_err(|_| invalid())?;
        trace.lifecycle.validate()?;
        if event.timestamp < trace.started_at {
            return Err(invalid());
        }
        Ok(Some(trace))
    }
    fn event(&self) -> Result<Event, GraphError> {
        self.lifecycle.validate()?;
        let encoded = serde_json::to_string(self).map_err(|_| invalid())?;
        if encoded.len() > MAX_ENCODED_BYTES {
            return Err(invalid());
        }
        let mut event = Event::new("pipeline_code_lifecycle");
        event.timestamp = Utc::now().max(self.started_at);
        event.llm_response.partial = true;
        event
            .provider_metadata
            .insert(CODE_TRACE_METADATA_KEY.into(), encoded);
        Ok(event)
    }
}

/// Construct only from the admitted node and original durable graph context.
pub(in crate::agents) struct CodeTraceContext {
    activation_id: String,
    node_id: String,
    graph_thread_id: String,
    graph_step: String,
    language: String,
    sender: PipelineNodeEventSender,
    scope: Option<PipelineNodeEventScope>,
}
impl CodeTraceContext {
    pub(super) fn new(
        node_id: &str,
        language: CodeLanguage,
        activation: &[u8; 32],
        context: &NodeContext,
        sender: PipelineNodeEventSender,
    ) -> Result<Self, GraphError> {
        let scope = PipelineNodeEventScope::from_state(
            context.state.get(PIPELINE_NODE_EVENT_SCOPE_STATE_KEY),
        )
        .map_err(|_| invalid())?;
        Ok(Self {
            activation_id: hex(activation),
            node_id: node_id.to_owned(),
            graph_thread_id: context.config.thread_id.clone(),
            graph_step: context.step.to_string(),
            language: match language {
                CodeLanguage::Python => "python",
                CodeLanguage::JavaScript => "javascript",
                CodeLanguage::TypeScript => "typescript",
                CodeLanguage::Rust => "rust",
            }
            .into(),
            sender,
            scope,
        })
    }
    pub(super) fn bind(&self, identity: (&str, u64)) -> Result<BoundCodeTrace<'_>, GraphError> {
        let trace = BoundCodeTrace {
            context: self,
            execution_id: identity.0.to_owned(),
            generation: identity.1.to_string(),
        };
        trace
            .lifecycle(CodePhase::Preparation, CodePhaseStatus::Started)
            .validate()?;
        Ok(trace)
    }
}
pub(super) struct BoundCodeTrace<'a> {
    context: &'a CodeTraceContext,
    execution_id: String,
    generation: String,
}
impl BoundCodeTrace<'_> {
    fn lifecycle(&self, phase: CodePhase, status: CodePhaseStatus) -> CodeLifecycle {
        CodeLifecycle {
            revision: 1,
            execution_id: self.execution_id.clone(),
            generation: self.generation.clone(),
            activation_id: self.context.activation_id.clone(),
            node_id: self.context.node_id.clone(),
            graph_thread_id: self.context.graph_thread_id.clone(),
            graph_step: self.context.graph_step.clone(),
            language: self.context.language.clone(),
            phase,
            status,
        }
    }
    async fn send(
        &self,
        phase: CodePhase,
        status: CodePhaseStatus,
        started_at: DateTime<Utc>,
    ) -> Result<(), GraphError> {
        let event = CodeTraceEvent {
            lifecycle: self.lifecycle(phase, status),
            started_at,
        }
        .event()?;
        self.context
            .sender
            .send(&self.context.node_id, self.context.scope.as_ref(), event)
            .await
            .map_err(|_| invalid())
    }
}

/// A dropped observation emits no completion or cleanup claim.
pub(super) async fn observe<T, F>(
    trace: Option<&BoundCodeTrace<'_>>,
    phase: CodePhase,
    operation: F,
) -> Result<T, GraphError>
where
    F: Future<Output = Result<T, GraphError>>,
{
    let started_at = Utc::now();
    if let Some(trace) = trace {
        trace
            .send(phase, CodePhaseStatus::Started, started_at)
            .await?;
    }
    let result = operation.await;
    if let Some(trace) = trace {
        trace
            .send(
                phase,
                if result.is_ok() {
                    CodePhaseStatus::Completed
                } else {
                    CodePhaseStatus::Failed
                },
                started_at,
            )
            .await?;
    }
    result
}
/// Observe the same safe phase lifecycle without erasing typed producer failures.
pub(super) async fn observe_typed<T, E, F>(
    trace: Option<&BoundCodeTrace<'_>>,
    phase: CodePhase,
    operation: F,
    trace_failure: impl Fn(GraphError) -> E,
) -> Result<T, E>
where
    F: Future<Output = Result<T, E>>,
{
    let started_at = Utc::now();
    if let Some(trace) = trace {
        trace
            .send(phase, CodePhaseStatus::Started, started_at)
            .await
            .map_err(&trace_failure)?;
    }
    let result = operation.await;
    if let Some(trace) = trace {
        trace
            .send(
                phase,
                if result.is_ok() {
                    CodePhaseStatus::Completed
                } else {
                    CodePhaseStatus::Failed
                },
                started_at,
            )
            .await
            .map_err(trace_failure)?;
    }
    result
}

fn identity(value: &str, maximum: usize) -> bool {
    !value.is_empty() && value.len() <= maximum && !value.chars().any(char::is_control)
}
fn decimal(value: &str, positive: bool) -> bool {
    value
        .parse::<u64>()
        .is_ok_and(|n| (!positive || n > 0) && n.to_string() == value)
}
fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes
        .iter()
        .fold(String::with_capacity(bytes.len() * 2), |mut s, b| {
            let _ = write!(&mut s, "{b:02x}");
            s
        })
}
fn invalid() -> GraphError {
    GraphError::Other(
        "graph.code.trace_invalid: Code progress identity is invalid or unavailable.".into(),
    )
}

#[cfg(test)]
#[path = "code_trace_tests.rs"]
mod tests;

impl BoundCodeTrace<'_> {
    pub(super) async fn debug(
        &self,
        original_visit: &crate::sandbox::code_recovery::OriginalCodeVisitRef,
        attempt: u16,
        original_activation: &[u8; 32],
        request: &[u8; 32],
        result: Result<
            super::code_debug::CodeDebugArtifactReference,
            super::code_debug::CodeDebugFailure,
        >,
    ) -> Result<(), GraphError> {
        let (status, artifact) = match result {
            Ok(artifact) => ("committed", Some(artifact)),
            Err(super::code_debug::CodeDebugFailure::Denied) => ("denied", None),
            Err(super::code_debug::CodeDebugFailure::Unavailable) => ("unavailable", None),
        };
        let proof = super::code_debug::CodeDebugProof {
            revision: 1,
            original_visit: original_visit.clone(),
            attempt,
            execution_id: self.execution_id.clone(),
            generation: self.generation.clone(),
            node_id: self.context.node_id.clone(),
            activation_id: hex(original_activation),
            request_sha256: hex(request),
            status: status.into(),
            artifact,
        };
        proof.validate()?;
        let raw = serde_json::to_string(&proof).map_err(|_| invalid())?;
        if raw.len() > 4096 {
            return Err(invalid());
        }
        let mut event = Event::new("pipeline_code_debug");
        event.llm_response.partial = true;
        event
            .provider_metadata
            .insert(super::code_debug::CODE_DEBUG_METADATA_KEY.into(), raw);
        self.context
            .sender
            .send(&self.context.node_id, self.context.scope.as_ref(), event)
            .await
            .map_err(|_| invalid())
    }
}
