//! Authorized ordinary application/ad-hoc ADK assembly.
//!
//! This boundary joins the already validated request, one-use runtime-context
//! redemption, the existing Main OpenAI-compatible endpoint and one fresh ADK
//! session. Application and ad-hoc differ only in their admitted frozen input;
//! claim, lease, output, settlement and Redis ownership stay in the shared
//! execution lifecycle.

#![allow(dead_code)] // Capability registration remains intentionally disabled.

use std::sync::Arc;

use async_trait::async_trait;
use tracing::Instrument as _;

use super::application_tools::{
    ApplicationToolDependencies, materialize_application_toolset, skipped_application_children,
};
use super::assembly::{OrdinaryModelProvider, OrdinaryNoToolProfile, ReasoningEffort};
use super::internal_tools::BuilderToolAuthority;
use super::runtime::{
    AdmittedNativeStart, AssembledNativeAgentInvocation, AuthorizedNativeAssembly,
    NativeAgentAssembler, NativeAgentAssemblyError, NativeAgentAssemblyErrorCode,
    RedeemedOrdinaryNativeAssembly,
};
use super::sensitive_tools::{
    SensitiveToolCatalog, policy_for_guardrails, sensitive_tools_for_kind_with_renames,
};
use super::session::{
    ApplicationRuntimeProjection, NativeSessionBackend, NativeToolExecutionMode,
    OrdinaryAgentCompletion, OrdinaryRuntimeBindings,
    assemble_delegated_authorization_resume_with_sessions,
    assemble_direct_hitl_resume_with_sessions_and_applications,
    assemble_ordinary_native_with_sessions_and_runtime_catalogs,
};
use super::tool_namespacing::{RenamedTool, apply_tool_namespacing, plan_tool_namespacing};
use crate::protocol::control::ClaimBoundRuntimeContextAuthority;
use crate::state::SessionLimits;
use crate::toolkits::{
    AdkHttpMcpConnector, AdmittedToolSnapshot, ArtifactToolAuthority, FrozenToolKind, McpConnector,
    McpMaterializationError, McpMaterializationErrorCode, ToolAdmissionPolicy,
    ToolsetMaterializationError, ToolsetMaterializationErrorCode,
    materialize_configured_toolsets_with_artifact_authority, materialize_mcp_toolsets_by_toolset,
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
        // Shared, not duplicated. The two builder tools (#940 A8) are called
        // DURING the run and must write under this same claim, so the single
        // minted authority has to outlive assembly — see
        // `internal_tools::BuilderToolAuthority` for why sharing it is the only
        // correct option and minting a second one is not.
        let runtime_context = Arc::new(runtime_context);
        tracing::Span::current().record("stage", "toolsets");
        let (runtime, fresh_execution_mode) = self
            .materialize_runtime(
                &tool_snapshot,
                mcp_tokens,
                &runtime_context,
                context.clone(),
                &profile,
                &tool_policy,
                plan.thread_id().to_owned(),
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

    // One more owner than the pedantic bound allows: the conversation thread is
    // a per-TURN identity a pipeline child namespaces its checkpoint under
    // (#973), and folding it into one of the frozen inputs beside it would hide
    // that it comes from the plan rather than from the agent's version.
    #[allow(clippy::too_many_arguments)]
    async fn materialize_runtime(
        &self,
        tool_snapshot: &AdmittedToolSnapshot<'_>,
        mcp_tokens: &serde_json::Map<String, serde_json::Value>,
        runtime_context: &Arc<ClaimBoundRuntimeContextAuthority>,
        context: Arc<ClaimScopedEliteaContext>,
        profile: &OrdinaryNoToolProfile,
        tool_policy: &Arc<ToolAdmissionPolicy>,
        conversation_thread_id: String,
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
        // The claim is lent to the `artifact` family here and nowhere else
        // (#906): this is the one assembly path whose authority outlives
        // assembly, which is exactly what a tool the model calls mid-run
        // needs. The same authority the two builder tools take, for the same
        // reason — see `internal_tools::BuilderToolAuthority`.
        let artifact_authority =
            ArtifactToolAuthority::new(Arc::clone(&self.platform), Arc::clone(runtime_context));
        let DirectToolsets {
            mut toolsets,
            sensitive: sensitive_tools,
            delegated_authorization,
            renamed_tools,
        } = materialize_direct_toolsets(
            tool_snapshot,
            self.mcp_connector.as_ref(),
            tool_policy,
            mcp_tokens,
            &artifact_authority,
        )
        .await?;
        let internal_tools = profile.internal_tools();
        toolsets.extend(internal_tools.toolsets(Some(&BuilderToolAuthority::new(
            Arc::clone(&self.platform),
            Arc::clone(runtime_context),
        ))));
        let mut application_runtime = ApplicationRuntimeProjection::default();
        // #973: read BEFORE materialization, off the same snapshot it reads.
        // A saved PIPELINE child IS built here now, so only the child types
        // this worker still cannot execute — `predict` — are skipped, and the
        // run says so once, the way a skipped internal tool does.
        let skipped_applications = skipped_application_children(tool_snapshot, None, true);
        if !skipped_applications.is_empty() {
            tracing::warn!(
                skipped = skipped_applications.len(),
                "attached application children this worker cannot build were skipped"
            );
        }
        if let Some(materialized) = materialize_application_toolset(
            tool_snapshot,
            self.platform.as_ref(),
            runtime_context.as_ref(),
            context,
            profile,
            ApplicationToolDependencies::new(
                self.model_facade.clone(),
                Arc::clone(tool_policy),
                self.mcp_connector.clone(),
                mcp_tokens,
            )
            .with_conversation_thread(conversation_thread_id),
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
            .with_internal_tools(internal_tools)
            .with_skipped_application_children(skipped_applications)
            .with_renamed_tools(renamed_tools),
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

/// Everything one agent's configured and MCP toolkits contribute to the run.
struct DirectToolsets {
    toolsets: Vec<Arc<dyn adk_rust::Toolset>>,
    sensitive: SensitiveToolCatalog,
    delegated_authorization: crate::toolkits::DelegatedAuthorizationCatalog,
    /// #983: the tools two toolsets both published, and what each is called
    /// now. Empty for every agent with no collision.
    renamed_tools: Vec<RenamedTool>,
}

/// Materialize the configured and MCP toolkits, then decide what the model is
/// allowed to CALL each tool.
///
/// The naming pass sits between materialization and every catalog built from
/// it, and that order is the whole point (#983). ADK hands the model one flat
/// list of function names and refuses the invocation when two toolsets
/// contribute the same one — after assembly has already reported success, as
/// an anonymous `native_agent.event_failed`. Planning the exposed names first
/// and building the sensitive-tool and delegated-authorization catalogs from
/// THOSE names keeps the three in agreement; building either catalog from the
/// published name would leave a renamed tool silently unguarded.
async fn materialize_direct_toolsets(
    snapshot: &AdmittedToolSnapshot<'_>,
    connector: &dyn McpConnector,
    policy: &Arc<ToolAdmissionPolicy>,
    mcp_tokens: &serde_json::Map<String, serde_json::Value>,
    artifacts: &ArtifactToolAuthority,
) -> Result<DirectToolsets, NativeAgentAssemblyError> {
    let (configured_toolsets, configured_authorization) =
        materialize_configured_toolsets_with_artifact_authority(
            snapshot,
            policy,
            mcp_tokens,
            Some(artifacts),
        )
        .map_err(tool_materialization_error)?;
    let (mcp_toolsets, mcp_authorization) =
        materialize_mcp_toolsets_by_toolset(snapshot, connector, policy, mcp_tokens)
            .await
            .map_err(|error| mcp_materialization_error(&error))?;

    let configured_count = configured_toolsets.len();
    let mut toolsets = configured_toolsets;
    toolsets.extend(mcp_toolsets.iter().map(Arc::clone));
    let plan = plan_tool_namespacing(&toolsets).await?;
    if !plan.is_empty() {
        tracing::warn!(
            renamed = plan.renamed().len(),
            "two toolsets published the same tool name; each is exposed under its own toolkit"
        );
    }
    let total = toolsets.len();

    let mut sensitive = sensitive_tools_for_kind_with_renames(
        snapshot,
        FrozenToolKind::Configured,
        &toolsets[..configured_count],
        policy.as_ref(),
        plan.renames_slice(0, configured_count),
    )
    .await?;
    sensitive.merge(
        sensitive_tools_for_kind_with_renames(
            snapshot,
            FrozenToolKind::Mcp,
            &toolsets[configured_count..],
            policy.as_ref(),
            plan.renames_slice(configured_count, total),
        )
        .await?,
    )?;

    let mut delegated_authorization = configured_authorization;
    for (offset, catalog) in mcp_authorization.into_iter().enumerate() {
        let renamed = match plan.renames_for(configured_count + offset) {
            Some(renames) => catalog
                .renamed(renames)
                .map_err(|()| invalid_tool_authorization_catalog())?,
            None => catalog,
        };
        delegated_authorization
            .merge(renamed)
            .map_err(|()| invalid_tool_authorization_catalog())?;
    }

    let renamed_tools = plan.renamed().to_vec();
    Ok(DirectToolsets {
        toolsets: apply_tool_namespacing(&plan, toolsets),
        sensitive,
        delegated_authorization,
        renamed_tools,
    })
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
    // #982: the requirement travels with the error so the lifecycle can name
    // the toolkit that challenged instead of failing the turn anonymously.
    NativeAgentAssemblyError::new(code, "the native MCP toolsets could not be materialized")
        .with_authorization(error.authorization().cloned())
}
