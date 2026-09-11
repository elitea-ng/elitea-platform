//! Authorized ordinary application/ad-hoc ADK assembly.
//!
//! This boundary joins the already validated request, one-use runtime-context
//! redemption, the existing Main OpenAI-compatible endpoint and one fresh ADK
//! session. Application and ad-hoc differ only in their admitted frozen input;
//! claim, lease, output, settlement and Redis ownership stay in the shared
//! execution lifecycle.

#![allow(dead_code)] // Capability registration remains intentionally disabled.

use std::collections::BTreeSet;
use std::sync::Arc;

use async_trait::async_trait;
use tracing::Instrument as _;

use super::application_tools::{ApplicationToolDependencies, materialize_application_toolset};
use super::assembly::{OrdinaryModelProvider, OrdinaryNoToolProfile, ReasoningEffort};
use super::internal_tools::ASK_USER_TOOLSET_NAME;
use super::runtime::{
    AdmittedNativeStart, AssembledNativeAgentInvocation, AuthorizedNativeAssembly,
    NativeAgentAssembler, NativeAgentAssemblyError, NativeAgentAssemblyErrorCode,
    RedeemedOrdinaryNativeAssembly,
};
use super::sensitive_tools::{
    SensitiveToolCatalog, policy_for_guardrails, sensitive_tools_for_kind,
};
use super::session::{
    ApplicationRuntimeProjection, NativeSessionBackend, NativeToolExecutionMode,
    OrdinaryAgentCompletion, OrdinaryRuntimeBindings,
    assemble_delegated_authorization_resume_with_sessions,
    assemble_direct_hitl_resume_with_sessions_and_applications,
    assemble_ordinary_native_with_sessions_and_runtime_catalogs,
};
use crate::protocol::control::ClaimBoundRuntimeContextAuthority;
use crate::state::SessionLimits;
use crate::toolkits::{
    AdkHttpMcpConnector, AdmittedToolSnapshot, FrozenToolKind, McpConnector,
    McpMaterializationError, McpMaterializationErrorCode, ToolAdmissionPolicy, ToolBindingError,
    ToolsetMaterializationError, ToolsetMaterializationErrorCode, bind_toolsets,
    materialize_configured_toolsets_with_tokens_and_authorization,
    materialize_mcp_toolsets_with_tokens_and_authorization,
};
use crate::transport::model_facade::{
    BoundModelFacade, ModelAdapterKind, ModelFacade, ModelFacadeError, ModelInvocation,
    ModelReasoningEffort,
};
use crate::transport::platform_client::PlatformClient;
use crate::transport::runtime_context::ClaimScopedEliteaContext;
use sqlx::PgPool;

/// Shared ordinary application/ad-hoc assembler used after `AUTHORIZED_NOW`.
///
/// Both clients own reusable shared HTTP/2 channels whose total concurrency is
/// bounded by invocation admission. Invocation credentials, provider state,
/// session state and completion capture remain one-use values.
pub(crate) struct OrdinaryNativeAgentAssembler {
    platform: Arc<PlatformClient>,
    model_facade: Arc<ModelFacade>,
    tool_policy: Arc<ToolAdmissionPolicy>,
    mcp_connector: Arc<dyn McpConnector>,
    sessions: NativeSessionBackend,
}

impl OrdinaryNativeAgentAssembler {
    #[must_use]
    pub(crate) fn new(
        platform: Arc<PlatformClient>,
        model_facade: Arc<ModelFacade>,
        tool_policy: Arc<ToolAdmissionPolicy>,
    ) -> Self {
        Self {
            platform,
            model_facade,
            tool_policy,
            mcp_connector: Arc::new(AdkHttpMcpConnector::new()),
            sessions: NativeSessionBackend::invocation_local(),
        }
    }

    /// Select claim-fenced `PostgreSQL` session persistence for the common Runner.
    ///
    /// Production construction remains closed until worker bootstrap supplies
    /// the authorized `agentstate` pool and the capability is registered.
    #[must_use]
    pub(crate) fn with_postgres_sessions(mut self, pool: PgPool, limits: SessionLimits) -> Self {
        self.sessions = NativeSessionBackend::postgres(pool, limits);
        self
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) fn with_sessions(
        mut self,
        sessions: Arc<dyn adk_rust::session::SessionService>,
    ) -> Self {
        self.sessions = NativeSessionBackend::injected(sessions);
        self
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) fn with_mcp_connector(mut self, connector: Arc<dyn McpConnector>) -> Self {
        self.mcp_connector = connector;
        self
    }

    async fn assemble_redeemed(
        &self,
        redeemed: RedeemedOrdinaryNativeAssembly<'_>,
        tool_policy: Arc<ToolAdmissionPolicy>,
    ) -> Result<
        AssembledNativeAgentInvocation<OrdinaryAgentCompletion<BoundModelFacade>>,
        NativeAgentAssemblyError,
    > {
        let RedeemedOrdinaryNativeAssembly {
            profile,
            plan,
            toolsets: tool_snapshot,
            start,
            mcp_tokens,
            context: claim_context,
            runtime_context,
            session: session_authority,
            state_writer_lease,
        } = redeemed;
        let context = Arc::new(claim_context);
        tracing::Span::current().record("stage", "toolsets");
        let (runtime, fresh_execution_mode) = self
            .materialize_runtime(
                &tool_snapshot,
                mcp_tokens,
                &runtime_context,
                context.clone(),
                &profile,
                &tool_policy,
            )
            .await?;
        let output_continuation = matches!(&start, AdmittedNativeStart::OutputContinuation);
        tracing::Span::current().record("output_continuation", output_continuation);
        let model = self.bind_model(&profile, context.as_ref(), output_continuation)?;
        tracing::Span::current().record("stage", "runner");
        let sessions = self
            .sessions
            .open(session_authority, state_writer_lease, &plan)
            .await?;
        match start {
            AdmittedNativeStart::Fresh
            | AdmittedNativeStart::Regenerate
            | AdmittedNativeStart::OutputContinuation => {
                tracing::Span::current().record(
                    "tool_execution_mode",
                    match fresh_execution_mode {
                        NativeToolExecutionMode::Sequential => "sequential",
                        NativeToolExecutionMode::ParallelApplications => "parallel_applications",
                    },
                );
                assemble_ordinary_native_with_sessions_and_runtime_catalogs(
                    model,
                    plan,
                    runtime,
                    fresh_execution_mode,
                    sessions,
                )
                .await
            }
            AdmittedNativeStart::DirectHitl(decision) => {
                tracing::Span::current().record("tool_execution_mode", "direct_hitl_resume");
                assemble_direct_hitl_resume_with_sessions_and_applications(
                    model, plan, runtime, decision, sessions,
                )
                .await
            }
            AdmittedNativeStart::DelegatedAuthorization(continuation) => {
                tracing::Span::current()
                    .record("tool_execution_mode", "delegated_authorization_resume");
                assemble_delegated_authorization_resume_with_sessions(
                    model,
                    plan,
                    runtime,
                    continuation,
                    sessions,
                )
                .await
            }
        }
    }

    async fn materialize_runtime(
        &self,
        tool_snapshot: &AdmittedToolSnapshot<'_>,
        mcp_tokens: &serde_json::Map<String, serde_json::Value>,
        runtime_context: &ClaimBoundRuntimeContextAuthority,
        context: Arc<ClaimScopedEliteaContext>,
        profile: &OrdinaryNoToolProfile,
        tool_policy: &Arc<ToolAdmissionPolicy>,
    ) -> Result<(OrdinaryRuntimeBindings, NativeToolExecutionMode), NativeAgentAssemblyError> {
        let tool_reference_count = tool_snapshot.iter().count();
        let nested_application_count = tool_snapshot
            .iter()
            .filter(|reference| reference.kind() == FrozenToolKind::Application)
            .count();
        let application_only =
            nested_application_count > 0 && nested_application_count == tool_reference_count;
        tracing::Span::current().record("tool_reference_count", tool_reference_count);
        tracing::Span::current().record("nested_application_count", nested_application_count);
        let (mut toolsets, sensitive_tools, delegated_authorization) = materialize_direct_toolsets(
            tool_snapshot,
            self.mcp_connector.as_ref(),
            tool_policy,
            mcp_tokens,
        )
        .await?;
        let internal_tools = profile.internal_tools();
        toolsets.extend(internal_tools.toolsets());
        let mut application_runtime = ApplicationRuntimeProjection::default();
        if let Some(materialized) = materialize_application_toolset(
            tool_snapshot,
            self.platform.as_ref(),
            runtime_context,
            context,
            profile,
            ApplicationToolDependencies::new(
                self.model_facade.clone(),
                Arc::clone(tool_policy),
                self.mcp_connector.clone(),
                mcp_tokens,
            ),
        )
        .await?
        {
            validate_nested_application_hitl_scope(application_only, &materialized.presentations)?;
            toolsets.push(materialized.toolset);
            application_runtime = ApplicationRuntimeProjection::streaming(
                materialized.presentations,
                materialized.events,
                materialized.resume,
            );
        }
        let reserved_toolsets = BTreeSet::from([
            ASK_USER_TOOLSET_NAME.to_owned(),
            "elitea_nested_applications".to_owned(),
        ]);
        let binding = bind_toolsets(toolsets, &reserved_toolsets, "elitea_ordinary_tool_binding")
            .await
            .map_err(tool_binding_error)?;
        let sensitive_tools = sensitive_tools.bind_provider_names(&binding)?;
        let delegated_authorization = delegated_authorization
            .bind_provider_names(&binding)
            .map_err(|()| invalid_tool_authorization_catalog())?;
        let toolsets = binding.into_toolsets();
        tracing::Span::current().record("materialized_toolset_count", toolsets.len());
        let fresh_execution_mode = if application_only
            && sensitive_tools.is_empty()
            && delegated_authorization.is_empty()
            && internal_tools.is_empty()
        {
            NativeToolExecutionMode::ParallelApplications
        } else {
            NativeToolExecutionMode::Sequential
        };
        Ok((
            OrdinaryRuntimeBindings::new(
                toolsets,
                sensitive_tools,
                delegated_authorization,
                application_runtime,
            )
            .with_internal_tools(internal_tools),
            fresh_execution_mode,
        ))
    }

    fn bind_model(
        &self,
        profile: &super::assembly::OrdinaryNoToolProfile,
        context: &crate::transport::runtime_context::ClaimScopedEliteaContext,
        output_continuation: bool,
    ) -> Result<BoundModelFacade, NativeAgentAssemblyError> {
        let invocation = ModelInvocation {
            model_name: profile.model_name().to_owned(),
            system_instruction: profile.instructions().to_owned(),
            max_tokens: profile.max_tokens(),
            reasoning_effort: profile.reasoning_effort().map(|effort| {
                if output_continuation {
                    ModelReasoningEffort::Low
                } else {
                    model_reasoning_effort(effort)
                }
            }),
            temperature: profile.temperature(),
            max_model_turns: profile.step_limit(),
        };
        let (adapter, adapter_name) = match profile.model_provider() {
            OrdinaryModelProvider::OpenAiChat => {
                (ModelAdapterKind::OpenAiCompatible, "openai_compatible")
            }
            OrdinaryModelProvider::NativeAnthropic => (ModelAdapterKind::Anthropic, "anthropic"),
        };
        tracing::Span::current().record("model_adapter", adapter_name);
        tracing::Span::current().record("model_project_id", profile.model_project_id());
        tracing::Span::current().record("stage", "model_binding");
        self.model_facade
            .bind(adapter, context, profile.model_project_id(), invocation)
            .map_err(model_binding_error)
    }
}

fn tool_binding_error(error: ToolBindingError) -> NativeAgentAssemblyError {
    let code = match error {
        ToolBindingError::InvalidConfiguration => {
            NativeAgentAssemblyErrorCode::InvalidConfiguration
        }
        ToolBindingError::ResourceExhausted => NativeAgentAssemblyErrorCode::ResourceExhausted,
        ToolBindingError::DependencyUnavailable => {
            NativeAgentAssemblyErrorCode::DependencyUnavailable
        }
    };
    NativeAgentAssemblyError::new(code, "the model-callable toolkit namespace is invalid")
}

#[async_trait]
impl NativeAgentAssembler for OrdinaryNativeAgentAssembler {
    type Completion = OrdinaryAgentCompletion<BoundModelFacade>;

    async fn assemble(
        &self,
        assembly: AuthorizedNativeAssembly<'_>,
    ) -> Result<AssembledNativeAgentInvocation<Self::Completion>, NativeAgentAssemblyError> {
        let span = assembly_span(&assembly, self.sessions.name());
        let result = async {
            // Admission also constructs the command-bound projection/session
            // plan, so deterministic local failures happen before PAT issuance.
            tracing::Span::current().record("stage", "admission");
            let tool_policy = policy_for_guardrails(
                assembly.request().payload.toolkit_guardrails.as_ref(),
                &self.tool_policy,
            )?;
            // #606: this turn's attached documents are read HERE, before
            // admission builds the human message. It is infallible by design —
            // a file the platform will not serve reaches the model as its name
            // alone rather than failing the turn.
            tracing::Span::current().record("stage", "attachments");
            let assembly = assembly
                .resolve_attachment_contents(self.platform.as_ref())
                .await;
            tracing::Span::current().record("stage", "admission");
            let admitted = assembly.admit_llm_agent(tool_policy.as_ref())?;
            if admitted.is_resume() && !self.sessions.supports_resume() {
                return Err(unsupported_session_resume());
            }
            tracing::Span::current().record("stage", "runtime_context");
            let redeemed = admitted
                .redeem_runtime_context(self.platform.as_ref())
                .await
                .map_err(NativeAgentAssemblyError::from)?;
            self.assemble_redeemed(redeemed, tool_policy).await
        }
        .instrument(span.clone())
        .await;
        match &result {
            Ok(_) => {
                span.record("outcome", "assembled");
            }
            Err(error) => {
                span.record("outcome", "failed");
                span.record("error_code", error.code().as_str());
            }
        }
        result
    }
}

fn unsupported_session_resume() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::UnsupportedCapability,
        "direct tool continuation requires durable session persistence",
    )
}

fn unsupported_mixed_nested_hitl() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::UnsupportedCapability,
        "nested HITL pauses require an application-only delegation set",
    )
}

fn validate_nested_application_hitl_scope(
    application_only: bool,
    applications: &super::events::ApplicationToolPresentationCatalog,
) -> Result<(), NativeAgentAssemblyError> {
    if !application_only && applications.has_guarded_descendant() {
        return Err(unsupported_mixed_nested_hitl());
    }
    Ok(())
}

fn assembly_span(assembly: &AuthorizedNativeAssembly<'_>, session_backend: &str) -> tracing::Span {
    tracing::info_span!(
        "agent.assemble",
        execution_kind = ?assembly.request().kind,
        stage = tracing::field::Empty,
        model_adapter = tracing::field::Empty,
        model_project_id = tracing::field::Empty,
        tool_reference_count = tracing::field::Empty,
        nested_application_count = tracing::field::Empty,
        materialized_toolset_count = tracing::field::Empty,
        tool_execution_mode = tracing::field::Empty,
        output_continuation = tracing::field::Empty,
        session_backend,
        session_bootstrap = tracing::field::Empty,
        outcome = tracing::field::Empty,
        error_code = tracing::field::Empty,
    )
}

async fn materialize_direct_toolsets(
    snapshot: &AdmittedToolSnapshot<'_>,
    connector: &dyn McpConnector,
    policy: &Arc<ToolAdmissionPolicy>,
    mcp_tokens: &serde_json::Map<String, serde_json::Value>,
) -> Result<
    (
        Vec<Arc<dyn adk_rust::Toolset>>,
        SensitiveToolCatalog,
        crate::toolkits::DelegatedAuthorizationCatalog,
    ),
    NativeAgentAssemblyError,
> {
    let (mut toolsets, mut delegated_authorization) =
        materialize_configured_toolsets_with_tokens_and_authorization(snapshot, policy, mcp_tokens)
            .await
            .map_err(tool_materialization_error)?;
    let mut sensitive = sensitive_tools_for_kind(
        snapshot,
        FrozenToolKind::Configured,
        &toolsets,
        policy.as_ref(),
    )
    .await?;
    let (mut mcp_toolsets, mcp_delegated_authorization) =
        materialize_mcp_toolsets_with_tokens_and_authorization(
            snapshot, connector, policy, mcp_tokens,
        )
        .await
        .map_err(|error| mcp_materialization_error(&error))?;
    delegated_authorization
        .merge(mcp_delegated_authorization)
        .map_err(|()| invalid_tool_authorization_catalog())?;
    sensitive.merge(
        sensitive_tools_for_kind(
            snapshot,
            FrozenToolKind::Mcp,
            &mcp_toolsets,
            policy.as_ref(),
        )
        .await?,
    )?;
    toolsets.append(&mut mcp_toolsets);
    Ok((toolsets, sensitive, delegated_authorization))
}

fn invalid_tool_authorization_catalog() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidConfiguration,
        "the configured delegated authorization catalog is invalid",
    )
}

const fn model_reasoning_effort(effort: ReasoningEffort) -> ModelReasoningEffort {
    match effort {
        ReasoningEffort::Low => ModelReasoningEffort::Low,
        ReasoningEffort::Medium => ModelReasoningEffort::Medium,
        ReasoningEffort::High => ModelReasoningEffort::High,
        ReasoningEffort::None => ModelReasoningEffort::None,
    }
}

fn model_binding_error(error: ModelFacadeError) -> NativeAgentAssemblyError {
    let code = match error {
        ModelFacadeError::InvalidConfiguration => {
            NativeAgentAssemblyErrorCode::InvalidConfiguration
        }
        ModelFacadeError::InvalidInvocation => NativeAgentAssemblyErrorCode::InvalidInput,
        ModelFacadeError::ResourceExhausted => NativeAgentAssemblyErrorCode::ResourceExhausted,
        ModelFacadeError::DependencyUnavailable => {
            NativeAgentAssemblyErrorCode::DependencyUnavailable
        }
    };
    NativeAgentAssemblyError::new(code, "the ordinary native model could not be bound")
}

fn tool_materialization_error(error: ToolsetMaterializationError) -> NativeAgentAssemblyError {
    let code = match error.code() {
        ToolsetMaterializationErrorCode::InvalidConfiguration => {
            NativeAgentAssemblyErrorCode::InvalidConfiguration
        }
        ToolsetMaterializationErrorCode::UnsupportedToolkit => {
            NativeAgentAssemblyErrorCode::UnsupportedCapability
        }
        ToolsetMaterializationErrorCode::DependencyUnavailable => {
            NativeAgentAssemblyErrorCode::DependencyUnavailable
        }
        ToolsetMaterializationErrorCode::ResourceExhausted => {
            NativeAgentAssemblyErrorCode::ResourceExhausted
        }
    };
    NativeAgentAssemblyError::new(code, "the native agent toolsets could not be materialized")
}

fn mcp_materialization_error(error: &McpMaterializationError) -> NativeAgentAssemblyError {
    let code = match error.code() {
        McpMaterializationErrorCode::InvalidConfiguration => {
            NativeAgentAssemblyErrorCode::InvalidConfiguration
        }
        McpMaterializationErrorCode::UnsupportedAuthority => {
            NativeAgentAssemblyErrorCode::UnsupportedCapability
        }
        McpMaterializationErrorCode::AuthorizationRequired => {
            NativeAgentAssemblyErrorCode::AuthorizationFailed
        }
        McpMaterializationErrorCode::ResourceExhausted => {
            NativeAgentAssemblyErrorCode::ResourceExhausted
        }
        McpMaterializationErrorCode::DependencyUnavailable => {
            NativeAgentAssemblyErrorCode::DependencyUnavailable
        }
    };
    NativeAgentAssemblyError::new(code, "the native MCP toolsets could not be materialized")
}
