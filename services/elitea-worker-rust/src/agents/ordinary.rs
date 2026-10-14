//! Authorized ordinary application/ad-hoc ADK assembly.
//!
//! This boundary joins the already validated request, one-use runtime-context
//! redemption, the existing Main OpenAI-compatible endpoint and one fresh ADK
//! session. Application and ad-hoc differ only in their admitted frozen input;
//! claim, lease, output, settlement and command-bus ownership stay in the shared
//! execution lifecycle.

#![allow(dead_code)] // Capability registration remains intentionally disabled.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

use async_trait::async_trait;
use tracing::Instrument as _;

use super::application_tools::{
    ApplicationToolDependencies, materialize_application_toolset, skipped_application_children,
};
use super::assembly::{OrdinaryModelProvider, OrdinaryNoToolProfile, ReasoningEffort};
use super::events::ToolkitAttributionCatalog;
use super::internal_tools::ASK_USER_TOOLSET_NAME;
use super::internal_tools::BuilderToolAuthority;
use super::runtime::{
    AdmittedNativeStart, AssembledNativeAgentInvocation, AuthorizedNativeAssembly,
    NativeAgentAssembler, NativeAgentAssemblyError, NativeAgentAssemblyErrorCode,
    RedeemedOrdinaryNativeAssembly,
};
use super::sensitive_tools::{
    SensitiveToolCatalog, policy_for_guardrails, sensitive_tools_for_kind,
};
use super::session::{
    ApplicationRuntimeProjection, BoundOrdinaryAgentModel as _, NativeSessionBackend,
    NativeToolExecutionMode, OrdinaryAgentCompletion, OrdinaryRuntimeBindings,
    assemble_delegated_authorization_resume_with_sessions,
    assemble_direct_hitl_resume_with_sessions_and_applications,
    assemble_ordinary_native_with_sessions_and_runtime_catalogs,
};
use super::tool_namespacing::RenamedTool;
use crate::indexing::IndexIngestRuntime;
use crate::indexing::tools::{
    ClaimIndexToolHost, LazyStepbackModel, StepbackBinder, StepbackBinding,
};
use crate::protocol::control::ClaimBoundRuntimeContextAuthority;
use crate::state::SessionLimits;
use crate::toolkits::{
    AdkHttpMcpConnector, AdmittedToolSnapshot, ArtifactToolAuthority, FrozenToolKind,
    IndexToolAuthority, IndexToolSupport, IndexToolsWithheld, McpConnector,
    McpMaterializationError, McpMaterializationErrorCode, RefusedToolkit, ToolAdmissionPolicy,
    ToolBindingError, ToolsetMaterializationError, ToolsetMaterializationErrorCode,
    bind_frozen_toolsets, freeze_toolsets, materialize_configured_toolsets_reporting_refusals,
    materialize_mcp_toolsets_with_tokens_and_authorization,
};
use crate::transport::model_facade::{
    BoundModelFacade, ModelAdapterKind, ModelFacade, ModelFacadeError, ModelInvocation,
    ModelReasoningEffort,
};
use crate::transport::office_host::{ClaimOfficeHost, OfficeRuntime};
use crate::transport::platform_client::PlatformClient;
use crate::transport::platform_writer::ClaimPlatformWriter;
use crate::transport::runtime_context::ClaimScopedEliteaContext;
use elitea_agent_runtime::host::PlatformWriter;
use elitea_agent_runtime::toolkits::DataAnalysisAuthority;
use sqlx::PgPool;

/// Shared ordinary application/ad-hoc assembler used after `AUTHORIZED_NOW`.
///
/// Both clients own reusable shared HTTP/2 channels whose total concurrency is
/// bounded by invocation admission. Invocation credentials, provider state,
/// session state and completion capture remain one-use values.
pub(crate) struct OrdinaryNativeAgentAssembler {
    sandbox: Option<Arc<super::graph::CodeRuntimeFactory>>,
    platform: Arc<PlatformClient>,
    model_facade: Arc<ModelFacade>,
    tool_policy: Arc<ToolAdmissionPolicy>,
    mcp_connector: Arc<dyn McpConnector>,
    sessions: NativeSessionBackend,
    /// The vector client and embeddings route the index tools act over
    /// (ADR-0030 I1); `None` on a deployment without a `vector_target`,
    /// where the indexing families carry no index tools.
    index_runtime: Option<Arc<IndexIngestRuntime>>,
    /// The platform object client the Office families (`pptx`, `docx`, `xlsx`) read and write
    /// documents with (ADR-0032); `None` leaves them out as unsupported.
    office_runtime: Option<Arc<OfficeRuntime>>,
}

/// Shared provider/session dependencies; constructing them never starts Runner.
struct OrdinaryRunnerInputs {
    model: BoundModelFacade,
    plan: super::session::OrdinaryNativeAgentPlan,
    runtime: OrdinaryRuntimeBindings,
    mode: NativeToolExecutionMode,
    sessions: Arc<dyn adk_rust::session::SessionService>,
    start: AdmittedNativeStart,
}

impl OrdinaryNativeAgentAssembler {
    pub(crate) fn with_sandbox(
        mut self,
        sandbox: Option<Arc<super::graph::CodeRuntimeFactory>>,
    ) -> Self {
        self.sandbox = sandbox;
        self
    }

    #[must_use]
    pub(crate) fn new(
        platform: Arc<PlatformClient>,
        model_facade: Arc<ModelFacade>,
        tool_policy: Arc<ToolAdmissionPolicy>,
    ) -> Self {
        Self {
            sandbox: None,
            platform,
            model_facade,
            tool_policy,
            mcp_connector: Arc::new(AdkHttpMcpConnector::new()),
            sessions: NativeSessionBackend::invocation_local(),
            index_runtime: None,
            office_runtime: None,
        }
    }

    /// Lend the Office runtime, so an Office toolkit reads and writes its
    /// documents under the claim's credential (ADR-0032 P3).
    #[must_use]
    pub(crate) fn with_office_runtime(
        mut self,
        office_runtime: Option<Arc<OfficeRuntime>>,
    ) -> Self {
        self.office_runtime = office_runtime;
        self
    }

    /// Lend the indexing runtime, so the indexing families serve the index
    /// tools under the claim's vector token (ADR-0030 I1).
    #[must_use]
    pub(crate) fn with_index_runtime(
        mut self,
        index_runtime: Option<Arc<IndexIngestRuntime>>,
    ) -> Self {
        self.index_runtime = index_runtime;
        self
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
        let OrdinaryRunnerInputs {
            model,
            plan,
            runtime,
            mode: fresh_execution_mode,
            sessions,
            start,
        } = self
            .prepare_runner_inputs(redeemed, tool_policy, false)
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

    async fn prepare_runner_inputs(
        &self,
        redeemed: RedeemedOrdinaryNativeAssembly<'_>,
        tool_policy: Arc<ToolAdmissionPolicy>,
        checkpoint_recovery: bool,
    ) -> Result<OrdinaryRunnerInputs, NativeAgentAssemblyError> {
        let RedeemedOrdinaryNativeAssembly {
            current_attachments,
            attachment_read_tokens,
            attachment_library,
            sandbox_authority,
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
        let (sessions, model_scopes) = self
            .sessions
            .open_with_model_scopes(session_authority, state_writer_lease, &plan)
            .await?;
        let model_scopes = if checkpoint_recovery {
            model_scopes.with_pending_model_recovery()
        } else {
            model_scopes
        };
        let context = Arc::new(claim_context);
        let runtime_context = Arc::new(runtime_context);
        // The Attachments module (`attachments`): the same tools as an
        // overview turn gets, bound on every turn and reaching every stored
        // file of the conversation, not only this message's.
        let attachment_library = if profile.internal_tools().attachments_enabled() {
            let library = attachment_library.unwrap_or_else(|| {
                Arc::new(super::attachment_tools::AttachmentLibrary::new(
                    Vec::new(),
                    attachment_read_tokens,
                ))
            });
            library.attach_source(
                Arc::new(super::attachments::ClaimAttachmentSource::new(
                    Arc::clone(&self.platform),
                    Arc::clone(&runtime_context),
                    current_attachments.clone(),
                )),
                &current_attachments,
            );
            Some(library)
        } else {
            attachment_library
        };
        tracing::Span::current().record("stage", "toolsets");
        let (runtime, fresh_execution_mode) = self
            .materialize_runtime(
                &tool_snapshot,
                mcp_tokens,
                &runtime_context,
                context.clone(),
                &profile,
                &tool_policy,
                model_scopes,
                plan.thread_id().to_owned(),
                self.sandbox
                    .as_ref()
                    .zip(sandbox_authority)
                    .map(|(factory, authority)| factory.bind(authority)),
                attachment_library,
            )
            .await?;
        let output_continuation = matches!(&start, AdmittedNativeStart::OutputContinuation);
        tracing::Span::current().record("output_continuation", output_continuation);
        let model = self.bind_model(
            &profile,
            &runtime.instruction_suffix(profile.instructions()),
            context.as_ref(),
            output_continuation,
        )?;
        tracing::Span::current().record("stage", "runner");
        Ok(OrdinaryRunnerInputs {
            model,
            plan,
            runtime,
            mode: fresh_execution_mode,
            sessions,
            start,
        })
    }

    async fn redeem_for_runner<'a>(
        &self,
        assembly: AuthorizedNativeAssembly<'a>,
        tool_policy: &ToolAdmissionPolicy,
    ) -> Result<RedeemedOrdinaryNativeAssembly<'a>, NativeAgentAssemblyError> {
        let admitted = assembly.admit_llm_agent(tool_policy)?;
        if admitted.is_resume() && !self.sessions.supports_resume() {
            return Err(unsupported_session_resume());
        }
        tracing::Span::current().record("stage", "runtime_context");
        admitted
            .redeem_runtime_context(self.platform.as_ref())
            .await
            .map_err(NativeAgentAssemblyError::from)
    }

    #[allow(clippy::too_many_arguments, clippy::too_many_lines)] // Keep claim, policy, and model storage owners together.
    async fn materialize_runtime(
        &self,
        tool_snapshot: &AdmittedToolSnapshot<'_>,
        mcp_tokens: &serde_json::Map<String, serde_json::Value>,
        runtime_context: &Arc<ClaimBoundRuntimeContextAuthority>,
        context: Arc<ClaimScopedEliteaContext>,
        profile: &OrdinaryNoToolProfile,
        tool_policy: &Arc<ToolAdmissionPolicy>,
        model_scopes: super::model_scope::ModelScopeSessions,
        conversation_thread_id: String,
        code: Option<Arc<dyn super::graph::CodeSandboxRuntime>>,
        attachment_library: Option<Arc<super::attachment_tools::AttachmentLibrary>>,
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
        let platform_writer: Arc<dyn PlatformWriter> = Arc::new(ClaimPlatformWriter::new(
            Arc::clone(&self.platform),
            Arc::clone(runtime_context),
        ));
        let mut artifact_authority = ArtifactToolAuthority::new(Arc::clone(&platform_writer));
        // The Office host (ADR-0032) rides on the artifact authority, built
        // only when the snapshot has an Office toolkit: it mints a credential
        // and binds nothing until a tool calls the model.
        let carries_office_family = snapshot_carries_office_family(tool_snapshot);
        // The Data Analysis toggle (ADR-0033) reads the attachment bucket
        // through the same host, and runs free-form code on the sandbox the
        // pipeline `code` nodes use, when this run has one.
        let wants_data_analysis = profile.internal_tools().data_analysis_enabled();
        let mut data_analysis_authority: Option<DataAnalysisAuthority> = None;
        if (carries_office_family || wants_data_analysis)
            && let Some(office) = self.office_host(&context, profile)
        {
            if wants_data_analysis {
                let mut authority = DataAnalysisAuthority::new(Arc::clone(&office));
                authority = authority.with_bucket_resolver(Arc::new(
                    super::attachments::ClaimAttachmentBucket::new(
                        Arc::clone(&self.platform),
                        Arc::clone(runtime_context),
                    ),
                ));
                if let Some(runtime) = code.clone() {
                    authority = authority.with_code_runner(Arc::new(
                        super::graph::SandboxAnalysisRunner::new(runtime),
                    ));
                }
                data_analysis_authority = Some(authority);
            }
            if carries_office_family {
                artifact_authority = artifact_authority.with_office(office);
            }
        }
        // The index tools (ADR-0030 I1) act under the same claim: its vector
        // token, its redeemed context for the query embedding, and a chat
        // model bound on the agent's own model for the step-back tools. The
        // host (and the second model binding it carries) is built only when
        // the snapshot has an I1 toolkit carrying a catalog — the only case
        // anything would call it (review round 1).
        let index_authority = if snapshot_carries_index_catalog(tool_snapshot) {
            self.index_tool_authority(runtime_context, &context, profile)
        } else {
            Err(IndexToolsWithheld::NotRequested)
        };
        let index_support = match index_authority.as_ref() {
            Ok(authority) => IndexToolSupport::Lent(authority),
            Err(reason) => IndexToolSupport::Withheld(*reason),
        };
        let DirectToolsets {
            mut toolsets,
            sensitive: sensitive_tools,
            delegated_authorization,
            refused: refused_toolkits,
        } = materialize_direct_toolsets(
            tool_snapshot,
            self.mcp_connector.as_ref(),
            tool_policy,
            mcp_tokens,
            &artifact_authority,
            index_support,
        )
        .await?;
        // The configured toolkits and MCP connections, by name: the only
        // toolsets Smart Tools Selection may move behind its meta-tools.
        let configured_toolsets: BTreeSet<String> = toolsets
            .iter()
            .map(|toolset| toolset.name().to_owned())
            .collect();
        let mut internal_tools = profile.internal_tools();
        // Bound only when the run has the host and the tool policy consents; a
        // toggle that binds nothing keeps its "not available" notice.
        let data_toolset =
            internal_tools.data_analysis_toolset(data_analysis_authority.as_ref(), tool_policy);
        if data_toolset.is_some() {
            internal_tools = internal_tools.serving_data_analysis();
        }
        toolsets.extend(internal_tools.toolsets(Some(&BuilderToolAuthority::new(platform_writer))));
        toolsets.extend(data_toolset);
        toolsets.extend(profile.instruction_plan().toolsets());
        // The attachment tools exist only when a document of this turn was
        // shown as an overview; the note in the prompt offers them by name.
        let has_attachment_tools = attachment_library.is_some();
        if let Some(library) = &attachment_library {
            toolsets.push(library.toolset());
        }
        let mut application_runtime = ApplicationRuntimeProjection::default();
        // #973: read BEFORE materialization, off the same snapshot it reads.
        // A saved PIPELINE child IS built here now, so the reference scan only
        // reports the child TYPES this worker never executes — `predict`. A
        // pipeline whose own build fails is added below from the materializer's
        // own outcome (#990 review 4), because only the build knows that.
        let mut skipped_applications = skipped_application_children(tool_snapshot, None, true);
        let (materialized, skipped_pipelines) = materialize_application_toolset(
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
            .with_model_scopes(model_scopes)
            .with_conversation_thread(conversation_thread_id)
            .with_code(code),
        )
        .await?;
        // #990 review 4: named whether or not a single child survived — an
        // agent whose ONLY attached pipeline could not be built binds no
        // application toolset at all, and that is exactly the case the notice
        // exists for.
        skipped_applications.extend(skipped_pipelines);
        if let Some(materialized) = materialized {
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
            super::instruction_authority::TOOLSET_NAME.to_owned(),
            super::attachment_tools::TOOLSET_NAME.to_owned(),
            elitea_agent_runtime::planner::TOOLSET_NAME.to_owned(),
            elitea_agent_runtime::lazy_tools::TOOLSET_NAME.to_owned(),
            "elitea_nested_applications".to_owned(),
        ]);
        let frozen = freeze_toolsets(toolsets, "elitea_ordinary_tool_binding")
            .await
            .map_err(tool_binding_error)?;
        // Smart Tools Selection: a tool that needs a confirmation or a
        // delegated authorization stays bound directly, because both are
        // properties of the call the model makes (see `lazy_tools`).
        let (frozen, lazy_tools_index, lazy_directory) = if internal_tools.lazy_tools_enabled() {
            let kinds: BTreeMap<String, String> = tool_snapshot
                .iter()
                .filter(|reference| reference.kind() != FrozenToolKind::Application)
                .map(|reference| {
                    (
                        reference.toolkit_name().to_owned(),
                        reference.tool_type().to_owned(),
                    )
                })
                .collect();
            let unpartitioned = frozen.clone();
            let partition = elitea_agent_runtime::lazy_tools::partition(
                frozen,
                |toolset| configured_toolsets.contains(toolset),
                |toolset, tool| {
                    sensitive_tools.policy_for_scoped(toolset, tool).is_some()
                        || delegated_authorization
                            .requirement_for_direct(toolset, tool)
                            .is_some()
                },
                |toolset| kinds.get(toolset).cloned(),
            );
            match partition.index {
                // The index goes into the system prompt. If it cannot fit
                // beside the agent's own instructions, do not hide the tools:
                // a model with meta-tools and no index cannot find anything.
                Some(index)
                    if !super::session::lazy_index_fits(
                        profile.instructions(),
                        internal_tools,
                        &index,
                    ) =>
                {
                    tracing::warn!(
                        event = "agent_internal_tool_lazy_tools_skipped",
                        reason_code = "instruction_budget",
                        index_bytes = index.len(),
                        "smart tools selection is off for this turn: its tool index does not fit beside the agent's instructions, so every tool stays bound"
                    );
                    (
                        unpartitioned,
                        None,
                        elitea_agent_runtime::lazy_tools::Directory::default(),
                    )
                }
                index => (partition.toolsets, index, partition.directory),
            }
        } else {
            (
                frozen,
                None,
                elitea_agent_runtime::lazy_tools::Directory::default(),
            )
        };
        let binding =
            bind_frozen_toolsets(&frozen, &reserved_toolsets).map_err(tool_binding_error)?;
        let sensitive_tools = sensitive_tools.bind_provider_names(&binding)?;
        let delegated_authorization = delegated_authorization
            .bind_provider_names(&binding)
            .map_err(|()| invalid_tool_authorization_catalog())?;
        let renamed_tools = binding
            .bindings()
            .filter(|(_, original, exposed)| original != exposed)
            .map(|(toolkit, original, exposed)| RenamedTool {
                toolkit: toolkit.to_owned(),
                original: original.to_owned(),
                exposed: exposed.to_owned(),
            })
            .collect();
        // Demo issue 6: name each bound tool's toolkit on its frames, so the
        // tool-call record Main writes (and the Analytics Tools tab) is not
        // left with an empty toolkit. Nested applications carry their own
        // presentation and are not toolkits here.
        let attributable = || {
            tool_snapshot
                .iter()
                .filter(|reference| reference.kind() != FrozenToolKind::Application)
                .map(|reference| {
                    (
                        reference.toolkit_name(),
                        reference.tool_id(),
                        reference.tool_type(),
                    )
                })
        };
        // Calls made through `invoke_tool` are recorded against the toolkit and
        // tool they resolve to (`with_lazy_tools`).
        let toolkit_attribution =
            ToolkitAttributionCatalog::from_bindings(attributable(), binding.bindings())
                .with_lazy_tools(lazy_directory, attributable());
        let toolsets = binding.into_toolsets();
        if !skipped_applications.is_empty() {
            tracing::warn!(
                skipped = skipped_applications.len(),
                "attached application children this worker cannot build were skipped"
            );
        }
        tracing::Span::current().record("materialized_toolset_count", toolsets.len());
        let fresh_execution_mode = if application_only
            && sensitive_tools.is_empty()
            && delegated_authorization.is_empty()
            && internal_tools.is_empty()
            && profile.instruction_plan().is_empty()
            && !has_attachment_tools
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
            .with_lazy_tools_index(lazy_tools_index)
            .with_instruction_plan(profile.instruction_plan().clone())
            .with_skipped_application_children(skipped_applications)
            .with_renamed_tools(renamed_tools)
            .with_refused_toolkits(refused_toolkits)
            .with_toolkit_attribution(toolkit_attribution),
            fresh_execution_mode,
        ))
    }

    /// The index tools' authority for this claim, or the reason there is
    /// none: this worker has no vector store, or the claim carries no vector
    /// token (the families then serve no index tools). The step-back model
    /// is bound on the agent's model lazily, on the first step-back call
    /// (review round 2): a turn that selects or calls no step-back tool
    /// binds no second model. A binding failure leaves the two step-back
    /// tools refusing with a sentence and the other three working.
    fn index_tool_authority(
        &self,
        runtime_context: &ClaimBoundRuntimeContextAuthority,
        context: &Arc<ClaimScopedEliteaContext>,
        profile: &super::assembly::OrdinaryNoToolProfile,
    ) -> Result<IndexToolAuthority, IndexToolsWithheld> {
        let runtime = self
            .index_runtime
            .as_ref()
            .ok_or(IndexToolsWithheld::NoVectorStore)?;
        let token = runtime_context
            .vector_token()
            .ok_or(IndexToolsWithheld::NoVectorToken)?;
        let model = LazyStepbackModel::new(self.stepback_model_binder(profile, context));
        Ok(IndexToolAuthority::new(Arc::new(ClaimIndexToolHost::new(
            Arc::clone(runtime),
            token,
            Arc::clone(context),
            Some(model),
            runtime_context.index_removal_binding(),
        ))))
    }

    /// The Office host for this claim, or `None` (logged) when the worker has
    /// no Office runtime or the credential does not fit the object API: the
    /// toolkit is then skipped as unsupported.
    fn office_host(
        &self,
        context: &Arc<ClaimScopedEliteaContext>,
        profile: &super::assembly::OrdinaryNoToolProfile,
    ) -> Option<Arc<dyn elitea_agent_runtime::toolkits::OfficeToolHost>> {
        let runtime = self.office_runtime.as_ref()?;
        let bearer = context.model_facade_token();
        let model = LazyStepbackModel::new(self.stepback_model_binder(profile, context));
        let host = ClaimOfficeHost::new(
            Arc::clone(runtime),
            &bearer,
            &context.resource_project_id().to_string(),
            model,
        );
        if let Ok(host) = host {
            // The `imagegen` toolkit's images API rides on the same host and
            // the same credential (#864).
            let host = host.with_images(self.model_facade.images(), Arc::clone(context));
            return Some(Arc::new(host));
        }
        tracing::warn!(
            event = "office_host_unavailable",
            "the Office families are left out: the object API client could not be built"
        );
        None
    }

    /// A second binding of the agent's model, with no instruction and no
    /// tools, for the step-back prompts: the agent's own binding captures
    /// its final answer, which a tool's model call must not touch. The
    /// binder holds only what the binding needs, so it can run after
    /// assembly, inside the tool call.
    fn stepback_model_binder(
        &self,
        profile: &super::assembly::OrdinaryNoToolProfile,
        context: &Arc<ClaimScopedEliteaContext>,
    ) -> StepbackBinder {
        let facade = Arc::clone(&self.model_facade);
        let context = Arc::clone(context);
        let adapter = match profile.model_provider() {
            OrdinaryModelProvider::OpenAiChat => ModelAdapterKind::OpenAiCompatible,
            OrdinaryModelProvider::NativeAnthropic => ModelAdapterKind::Anthropic,
        };
        let model_project_id = profile.model_project_id();
        let model_name = profile.model_name().to_owned();
        let context_budget = profile.context_budget();
        let max_tokens = profile.max_tokens();
        let temperature = profile.temperature();
        let max_model_turns = profile.step_limit();
        Box::new(move || {
            let invocation = ModelInvocation {
                response_schema: None,
                allow_text_continuation: false,
                context_budget,
                model_name: model_name.clone(),
                system_instruction: String::new(),
                max_tokens,
                reasoning_effort: None,
                temperature,
                max_model_turns,
            };
            let bound: Result<BoundModelFacade, ModelFacadeError> =
                facade.bind(adapter, context.as_ref(), model_project_id, invocation);
            Ok(StepbackBinding {
                model: bound?.adk_model(),
                model_name: model_name.clone(),
            })
        })
    }

    fn bind_model(
        &self,
        profile: &super::assembly::OrdinaryNoToolProfile,
        system_instruction: &str,
        context: &crate::transport::runtime_context::ClaimScopedEliteaContext,
        output_continuation: bool,
    ) -> Result<BoundModelFacade, NativeAgentAssemblyError> {
        let invocation = ModelInvocation {
            response_schema: None,
            allow_text_continuation: false,
            context_budget: profile.context_budget(),
            model_name: profile.model_name().to_owned(),
            system_instruction: system_instruction.to_owned(),
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
            .bind_with_summary(
                adapter,
                context,
                profile.model_project_id(),
                invocation,
                profile.summary_model(),
            )
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

    fn sandbox_stop_delivery(
        &self,
    ) -> Option<Arc<dyn crate::sandbox::dispatch::SandboxStopDelivery>> {
        self.sandbox.as_ref().map(|factory| {
            factory.clone() as Arc<dyn crate::sandbox::dispatch::SandboxStopDelivery>
        })
    }

    async fn inspect_checkpoint(
        &self,
        request: &super::request::AgentExecutionRequest,
        command: &super::session::AuthorizedNativeCommandBinding,
        session: crate::protocol::control::ClaimBoundSessionAuthority,
        state_writer_lease: Arc<dyn crate::state::StateWriterLease>,
    ) -> Result<super::session::ValidatedModelCheckpoint, NativeAgentAssemblyError> {
        let (_, plan, _) = super::runtime::admit_ordinary_plan(
            request,
            command,
            &request.payload.input_attachments,
        )?;
        self.sessions
            .inspect_model_checkpoint(session, state_writer_lease, &plan)
            .await
    }

    async fn assemble_checkpoint(
        &self,
        assembly: AuthorizedNativeAssembly<'_>,
    ) -> Result<
        super::runtime::PendingRecoveredAgentInvocation<Self::Completion>,
        NativeAgentAssemblyError,
    > {
        if !self.sessions.supports_resume() {
            return Err(unsupported_session_resume());
        }
        let tool_policy = policy_for_guardrails(
            assembly.request().payload.toolkit_guardrails.as_ref(),
            &self.tool_policy,
        )?;
        // The saved model request already contains resolved attachments. Do not
        // reread mutable documents while reconstructing this execution.
        let redeemed = self
            .redeem_for_runner(assembly, tool_policy.as_ref())
            .await?;
        let OrdinaryRunnerInputs {
            model,
            plan,
            runtime,
            mode,
            sessions,
            ..
        } = self
            .prepare_runner_inputs(redeemed, tool_policy, true)
            .await?;
        super::session::assemble_ordinary_native_from_checkpoint(
            model, plan, runtime, mode, sessions,
        )
        .await
    }

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
                .resolve_attachment_contents(self.platform.as_ref(), true)
                .await;
            tracing::Span::current().record("stage", "admission");
            let redeemed = self
                .redeem_for_runner(assembly, tool_policy.as_ref())
                .await?;
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
    /// Configured toolkits left out for a setting this runtime refuses
    /// (#1207 review round 2), named in the session's opening notice.
    refused: Vec<RefusedToolkit>,
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
/// Whether the snapshot has a toolkit of the Office families (ADR-0032 P3, P4:
/// `pptx`, `docx`, `xlsx`).
pub(super) fn snapshot_carries_office_family(snapshot: &AdmittedToolSnapshot<'_>) -> bool {
    snapshot.iter().any(|reference| {
        reference.kind() == FrozenToolKind::Configured
            && matches!(reference.tool_type(), "pptx" | "docx" | "xlsx" | "imagegen")
    })
}

/// Whether any configured I1 indexing toolkit in the snapshot carries Main's
/// frozen index catalog — the only case the index tools' host is worth
/// building (ADR-0030 I1).
pub(super) fn snapshot_carries_index_catalog(snapshot: &AdmittedToolSnapshot<'_>) -> bool {
    snapshot.iter().any(|reference| {
        reference.kind() == FrozenToolKind::Configured
            && crate::toolkits::is_index_family(reference.tool_type())
            && reference.indexes().is_some()
    })
}

async fn materialize_direct_toolsets(
    snapshot: &AdmittedToolSnapshot<'_>,
    connector: &dyn McpConnector,
    policy: &Arc<ToolAdmissionPolicy>,
    mcp_tokens: &serde_json::Map<String, serde_json::Value>,
    artifacts: &ArtifactToolAuthority,
    index_tools: IndexToolSupport<'_>,
) -> Result<DirectToolsets, NativeAgentAssemblyError> {
    let crate::toolkits::ConfiguredToolsets {
        mut toolsets,
        mut delegated_authorization,
        refused,
    } = materialize_configured_toolsets_reporting_refusals(
        snapshot,
        policy,
        mcp_tokens,
        Some(artifacts),
        index_tools,
    )
    .await
    .map_err(tool_materialization_error)?;
    let mut sensitive = sensitive_tools_for_kind(
        snapshot,
        FrozenToolKind::Configured,
        &toolsets,
        policy.as_ref(),
    )
    .await?;
    let (mut mcp, mcp_authorization) = materialize_mcp_toolsets_with_tokens_and_authorization(
        snapshot, connector, policy, mcp_tokens,
    )
    .await
    .map_err(|error| mcp_materialization_error(&error))?;
    sensitive.merge(
        sensitive_tools_for_kind(snapshot, FrozenToolKind::Mcp, &mcp, policy.as_ref()).await?,
    )?;
    delegated_authorization
        .merge(mcp_authorization)
        .map_err(|()| invalid_tool_authorization_catalog())?;
    toolsets.append(&mut mcp);
    Ok(DirectToolsets {
        toolsets,
        sensitive,
        delegated_authorization,
        refused,
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

pub(super) fn tool_materialization_error(
    error: ToolsetMaterializationError,
) -> NativeAgentAssemblyError {
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
    let assembly =
        NativeAgentAssemblyError::new(code, "the native agent toolsets could not be materialized");
    // A refused setting (#1207 review round 2) on a path with no opening
    // notice: the turn ends as "Configuration type is not supported." and
    // the structured log names the setting.
    match error.refusal_reason() {
        Some(reason) => assembly.with_cause(REFUSED_TOOLKIT_SETTING_CODE, Some(reason)),
        None => assembly,
    }
}

/// Cause code of a toolkit refused for a setting this runtime does not honour.
pub(crate) const REFUSED_TOOLKIT_SETTING_CODE: &str = "toolkit.configuration_setting_refused";

pub(super) fn mcp_materialization_error(
    error: &McpMaterializationError,
) -> NativeAgentAssemblyError {
    let code = match error.code() {
        McpMaterializationErrorCode::InvalidConfiguration => {
            NativeAgentAssemblyErrorCode::InvalidConfiguration
        }
        McpMaterializationErrorCode::UnsupportedAuthority
        | McpMaterializationErrorCode::RetiredSseEndpoint
        | McpMaterializationErrorCode::SavedToolkitRequired
        | McpMaterializationErrorCode::OAuthAppUnattachable => {
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
    let message = error
        .visible_message()
        .unwrap_or("the native MCP toolsets could not be materialized");
    // #982: the requirement travels with the error so the lifecycle can name
    // the toolkit that challenged instead of failing the turn anonymously.
    NativeAgentAssemblyError::new(code, message).with_authorization(error.authorization().cloned())
}
