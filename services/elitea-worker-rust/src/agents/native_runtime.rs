//! One native ADK assembly entry point for direct agents and stored pipelines.
//!
//! Selection uses only the already frozen request shape. It happens before an
//! assembler can redeem runtime context, session, model, tool or checkpoint
//! authority, and it never falls back from one runtime to the other.

#![allow(dead_code)] // Production capability routing remains intentionally disabled.

use std::sync::Arc;

use async_trait::async_trait;
use serde_json::Value;
use sqlx::PgPool;

use super::events::CompletedAgentBrowserOutput;
use super::ordinary::OrdinaryNativeAgentAssembler;
use super::pipeline::PipelineNativeAgentAssembler;
use super::request::{AgentExecutionKind, AgentExecutionRequest};
use super::runtime::{
    AssembledNativeAgentInvocation, AuthorizedNativeAssembly, NativeAgentAssembler,
    NativeAgentAssemblyError, NativeAgentAssemblyErrorCode, NativeAgentCompletionSelector,
};
use crate::state::{CheckpointLimits, SessionLimits};
use crate::toolkits::ToolAdmissionPolicy;
use crate::transport::model_facade::ModelFacade;
use crate::transport::platform_client::PlatformClient;

/// The two native ADK execution shapes admitted by this worker slice.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum NativeRuntimeKind {
    Direct,
    Pipeline,
}

impl NativeRuntimeKind {
    /// Classify one frozen request without touching any execution authority.
    pub(crate) fn from_request(
        request: &AgentExecutionRequest,
    ) -> Result<Self, NativeAgentAssemblyError> {
        if request.kind == AgentExecutionKind::Adhoc {
            return Ok(Self::Direct);
        }
        let version = request
            .payload
            .application
            .get("version_details")
            .and_then(Value::as_object)
            .ok_or_else(invalid_runtime_kind)?;
        match version.get("agent_type") {
            None => Ok(Self::Direct),
            Some(Value::String(value)) if value == "agent" => Ok(Self::Direct),
            Some(Value::String(value)) if value == "pipeline" => Ok(Self::Pipeline),
            Some(Value::String(_)) => Err(unsupported_runtime_kind()),
            Some(_) => Err(invalid_runtime_kind()),
        }
    }
}

/// Completion owner for either native runtime without weakening the common
/// authorized lifecycle's consuming post-EOS selection contract.
pub(crate) enum NativeRuntimeCompletion<D, P> {
    Direct(D),
    Pipeline(P),
}

#[async_trait]
impl<D, P> NativeAgentCompletionSelector for NativeRuntimeCompletion<D, P>
where
    D: NativeAgentCompletionSelector,
    P: NativeAgentCompletionSelector,
{
    async fn select(self) -> Result<CompletedAgentBrowserOutput, NativeAgentAssemblyError> {
        match self {
            Self::Direct(completion) => completion.select().await,
            Self::Pipeline(completion) => completion.select().await,
        }
    }
}

/// Strict router over the concrete direct-agent and pipeline assemblers.
pub(crate) struct NativeRuntimeAssembler<D, P> {
    direct: D,
    pipeline: P,
}

impl<D, P> NativeRuntimeAssembler<D, P> {
    #[must_use]
    pub(crate) const fn new(direct: D, pipeline: P) -> Self {
        Self { direct, pipeline }
    }
}

impl NativeRuntimeAssembler<OrdinaryNativeAgentAssembler, PipelineNativeAgentAssembler> {
    /// Compose both native modes over the same authorized `agentstate` pool.
    ///
    /// Direct agents consume only the session writer. Pipelines consume a
    /// separately fenced session writer plus checkpointer writer from the same
    /// immutable invocation authority; neither mode can borrow the other's
    /// state or fall back after admission.
    #[must_use]
    pub(crate) fn postgres(
        platform: Arc<PlatformClient>,
        model_facade: Arc<ModelFacade>,
        tool_policy: Arc<ToolAdmissionPolicy>,
        pool: PgPool,
        session_limits: SessionLimits,
        checkpoint_limits: CheckpointLimits,
    ) -> Self {
        let direct = OrdinaryNativeAgentAssembler::new(
            Arc::clone(&platform),
            Arc::clone(&model_facade),
            Arc::clone(&tool_policy),
        )
        .with_postgres_sessions(pool.clone(), session_limits);
        let pipeline = PipelineNativeAgentAssembler::postgres(
            pool,
            session_limits,
            checkpoint_limits,
            tool_policy,
            platform,
            model_facade,
        );
        Self::new(direct, pipeline)
    }
}

#[async_trait]
impl<D, P> NativeAgentAssembler for NativeRuntimeAssembler<D, P>
where
    D: NativeAgentAssembler,
    P: NativeAgentAssembler,
{
    type Completion = NativeRuntimeCompletion<D::Completion, P::Completion>;

    async fn inspect_checkpoint(
        &self,
        request: &super::request::AgentExecutionRequest,
        command: &super::session::AuthorizedNativeCommandBinding,
        session: crate::protocol::control::ClaimBoundSessionAuthority,
        state_writer_lease: std::sync::Arc<dyn crate::state::StateWriterLease>,
    ) -> Result<super::session::ValidatedModelCheckpoint, NativeAgentAssemblyError> {
        match NativeRuntimeKind::from_request(request)? {
            NativeRuntimeKind::Direct => {
                self.direct
                    .inspect_checkpoint(request, command, session, state_writer_lease)
                    .await
            }
            NativeRuntimeKind::Pipeline => {
                self.pipeline
                    .inspect_checkpoint(request, command, session, state_writer_lease)
                    .await
            }
        }
    }

    async fn assemble(
        &self,
        assembly: AuthorizedNativeAssembly<'_>,
    ) -> Result<AssembledNativeAgentInvocation<Self::Completion>, NativeAgentAssemblyError> {
        match NativeRuntimeKind::from_request(assembly.request())? {
            NativeRuntimeKind::Direct => self
                .direct
                .assemble(assembly)
                .await
                .map(|assembled| assembled.map_completion(NativeRuntimeCompletion::Direct)),
            NativeRuntimeKind::Pipeline => self
                .pipeline
                .assemble(assembly)
                .await
                .map(|assembled| assembled.map_completion(NativeRuntimeCompletion::Pipeline)),
        }
    }
}

const fn invalid_runtime_kind() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidInput,
        "the frozen native runtime kind is malformed",
    )
}

const fn unsupported_runtime_kind() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::UnsupportedCapability,
        "the frozen native runtime kind is not supported",
    )
}
