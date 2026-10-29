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
    ApplicationRuntimeProjection, NativeSessionBackend, NativeToolExecutionMode,
    OrdinaryAgentCompletion, OrdinaryRuntimeBindings,
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
    IndexToolAuthority, IndexToolSupport, IndexToolsWithheld, LeftOutToolkit, McpConnector,
    McpMaterializationError, McpMaterializationErrorCode, ToolAdmissionPolicy, ToolBindingError,
    ToolsetMaterializationError, ToolsetMaterializationErrorCode, bind_frozen_toolsets,
    freeze_toolsets, materialize_configured_toolsets_reporting_refusals_governed,
    materialize_governed_mcp_toolsets, materialize_mcp_toolsets_with_tokens_and_authorization,
};
use crate::transport::credential_redeemer::governed_client;
use crate::transport::model_facade::{
    BoundModelFacade, ModelAdapterKind, ModelFacade, ModelFacadeError, ModelInvocation,
    ModelReasoningEffort,
};
use crate::transport::model_text::ModelCallBudget;
use crate::transport::office_host::{ClaimOfficeHost, OfficeRuntime};
use crate::transport::platform_client::PlatformClient;
use crate::transport::platform_writer::ClaimPlatformWriter;
use crate::transport::runtime_context::ClaimScopedEliteaContext;
use elitea_agent_runtime::host::PlatformWriter;
use elitea_agent_runtime::toolkits::DataAnalysisAuthority;
use elitea_agent_runtime::toolkits::governed::GovernedOperationClient;
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
    /// Completions one claim's toolkits may make in total (step-back tools,
    /// the Office host's prompts), `toolkit_model_call_cap` in the runtime
    /// config.
    toolkit_model_call_cap: u32,
    /// The platform object client the Office families (`pptx`, `docx`, `xlsx`) read and write
    /// documents with (ADR-0032); `None` leaves them out as unsupported.
    office_runtime: Option<Arc<OfficeRuntime>>,
    /// The web families' egress allowlist and fetch origin (#1238); `None`
    /// leaves `web` and `host_intel` toolkits refusing every call.
    web_runtime: Option<Arc<crate::transport::metering::WebRuntime>>,
    /// The cloud executor (ADR-0034): `None` on a worker that cannot run a
    /// handed-off thread's turn, which then fails such a turn.
    workspace_runs: Option<Arc<crate::workspace_run::turn::WorkspaceRunFactory>>,
}

/// Shared provider/session dependencies; constructing them never starts Runner.
struct OrdinaryRunnerInputs {
    model: BoundModelFacade,
    plan: super::session::OrdinaryNativeAgentPlan,
    runtime: OrdinaryRuntimeBindings,
    mode: NativeToolExecutionMode,
    sessions: Arc<dyn adk_rust::session::SessionService>,
    start: AdmittedNativeStart,
    /// The hydrated workspace of a cloud-executor turn.
    workspace: Option<Arc<crate::workspace_run::turn::WorkspaceTurn>>,
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
            toolkit_model_call_cap: crate::transport::model_text::DEFAULT_TOOLKIT_MODEL_CALLS,
            office_runtime: None,
            web_runtime: None,
            workspace_runs: None,
        }
    }

    /// Lend the cloud executor's workspace runs (ADR-0034).
    #[must_use]
    pub(crate) fn with_workspace_runs(
        mut self,
        workspace_runs: Option<Arc<crate::workspace_run::turn::WorkspaceRunFactory>>,
    ) -> Self {
        self.workspace_runs = workspace_runs;
        self
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

    /// Lend the web families' runtime (#1238).
    #[must_use]
    pub(crate) fn with_web_runtime(
        mut self,
        web_runtime: Option<Arc<crate::transport::metering::WebRuntime>>,
    ) -> Self {
        self.web_runtime = web_runtime;
        self
    }

    /// The completions one claim's toolkits may make in total.
    #[must_use]
    pub(crate) fn with_toolkit_model_call_cap(mut self, cap: u32) -> Self {
        self.toolkit_model_call_cap = cap;
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
            workspace,
        } = self
            .prepare_runner_inputs(redeemed, tool_policy, false)
            .await?;
        let assembled = match start {
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
        }?;
        Ok(assembled.map_completion(|completion| completion.with_workspace(workspace)))
    }

    #[allow(
        clippy::too_many_lines,
        reason = "one ordered owner of the claim, the workspace, the tools and the model"
    )]
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
        // Client contract 1.27: the claim's live lease also fences the
        // steering queue of this turn.
        let steering = state_writer_lease.steering_source();
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
        // ADR-0034: a cloud turn of a handed-off thread hydrates its workspace
        // before anything else is bound, and is refused where it cannot run.
        let workspace = match context.workspace() {
            None => None,
            Some(field) => {
                // A recovered checkpoint believes tool calls ran in a sandbox
                // that no longer exists; it is not resumed.
                if checkpoint_recovery {
                    return Err(super::workspace::workspace_run_unavailable());
                }
                let runs = self
                    .workspace_runs
                    .as_ref()
                    .ok_or_else(super::workspace::workspace_run_unavailable)?;
                let claim = runtime_context.workspace_claim(&field.thread, field.epoch);
                Some(
                    runs.begin(
                        field,
                        claim,
                        context.execution_id(),
                        runs.grants(sandbox_authority.clone()),
                    )
                    .await
                    .map_err(|error| super::workspace::hydrate_error(&error))?,
                )
            }
        };
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
                plan.conversation_id().to_owned(),
                self.sandbox
                    .as_ref()
                    .zip(sandbox_authority)
                    .map(|(factory, authority)| factory.bind(authority)),
                attachment_library,
            )
            .await?;
        let runtime = runtime.with_steering(steering);
        // The workspace's tools: the local tool names, run in the sandbox.
        let runtime = match (&workspace, context.workspace()) {
            (Some(turn), Some(field)) => {
                // An approved call (a person's yes to a destructive command)
                // runs at most once: a retried resume reads its recorded
                // result from the turn's session instead of running it again.
                let approved_calls =
                    Arc::new(super::approved_calls::SessionApprovedCallLedger::new(
                        Arc::clone(&sessions),
                        plan.session_identity()?,
                    ));
                let runtime = runtime.with_workspace_toolset(
                    crate::workspace_run::tools::turn_provider(turn, field.shell)
                        // The edit tools that suit the agent's model, as
                        // the desktop offers them (#1214).
                        .with_edit_profile(profile.edit_profile())
                        .with_approved_calls(approved_calls)
                        .toolset(),
                );
                // A destructive or escaping command is a server HITL
                // interrupt before it runs (ADR-0034 decision 9).
                let runtime = if field.shell {
                    runtime.with_workspace_gate(Arc::new(
                        crate::workspace_run::tools::WorkspaceCommandGate::new(turn.sandbox()),
                    ))?
                } else {
                    runtime
                };
                // ADR-0034 decision 10 (amended 2026-10-26): git_push and
                // open_pull_request, only with an upstream, a policy that
                // allows them, and a credential: the owner's choice for the
                // thread (redeemed per call, outside the sandbox) or a bound
                // GitHub or GitLab toolkit.
                let pull_request = if field.has_upstream
                    && field.pull_requests != crate::workspace_run::context::PullRequestMode::Off
                {
                    use elitea_agent_runtime::toolkits::workspace_pull_request as pr;
                    pr::WorkspaceForgeTools {
                        thread: field.pull_request_credential.then(|| {
                            pr::ThreadCredential::new(Arc::new(
                                crate::workspace_run::tools::TurnCredentialSource::new(Arc::clone(
                                    turn,
                                )),
                            ))
                        }),
                        // Bound to the run's egress policy (#1490).
                        targets: pr::pull_request_targets(&tool_snapshot, tool_policy.egress()),
                        diff: Arc::new(crate::workspace_run::tools::TurnDiffSource::new(
                            Arc::clone(turn),
                        )),
                        push: Some(Arc::new(crate::workspace_run::tools::TurnPushSource::new(
                            Arc::clone(turn),
                        ))),
                        push_transport: None,
                        audit: Arc::new(
                            crate::workspace_run::tools::LogPullRequestAudit::new(
                                context.execution_id(),
                            )
                            .with_audit(turn.audit()),
                        ),
                        egress: Arc::clone(tool_policy.egress()),
                    }
                    .toolset()
                } else {
                    None
                };
                match pull_request {
                    // A person approves each push and pull request unless the
                    // policy lets them run without one.
                    Some(toolset)
                        if field.pull_requests
                            == crate::workspace_run::context::PullRequestMode::Ask =>
                    {
                        runtime
                            .with_workspace_toolset(toolset)
                            .with_workspace_pull_request_approval()?
                    }
                    Some(toolset) => runtime.with_workspace_toolset(toolset),
                    None => runtime,
                }
            }
            _ => runtime,
        };
        let output_continuation = matches!(&start, AdmittedNativeStart::OutputContinuation);
        tracing::Span::current().record("output_continuation", output_continuation);
        // The read-only sub-agents (#1226): on the toggle, and on every
        // cloud-executor workspace turn. Bound last, so a child is offered
        // the read-only tools of every toolset above.
        let runtime =
            if profile.internal_tools().subagents_enabled() || context.workspace().is_some() {
                let (adapter, _) = model_adapter(&profile);
                let invocation = model_invocation(&profile, "", output_continuation);
                let facade = Arc::clone(&self.model_facade);
                let context = Arc::clone(&context);
                let model_project_id = profile.model_project_id();
                runtime.with_subagents(move |toolsets, excluded| {
                    super::subagents::toolset(
                        super::subagents::WorkerSubAgents {
                            facade,
                            context,
                            adapter,
                            model_project_id,
                            invocation,
                            toolsets: super::subagents::WorkerSubAgents::child_toolsets(toolsets),
                            config: std::sync::OnceLock::new(),
                        },
                        excluded,
                    )
                })
            } else {
                runtime
            };
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
            workspace,
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
        conversation_id: String,
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
        // ONE completion budget for everything this claim's toolkits ask a
        // model (step-back tools, Office prompts). It is not the agent's
        // `max_model_turns`: each binding is single-use.
        let toolkit_model_budget = ModelCallBudget::new(self.toolkit_model_call_cap);
        // The Data Analysis toggle (ADR-0033) reads the attachment bucket
        // through the same host, and runs free-form code on the sandbox the
        // pipeline `code` nodes use, when this run has one.
        let wants_data_analysis = profile.internal_tools().data_analysis_enabled();
        let mut data_analysis_authority: Option<DataAnalysisAuthority> = None;
        if (carries_office_family || wants_data_analysis)
            && let Some(office) = self.office_host(
                &context,
                profile,
                &toolkit_model_budget,
                crate::transport::office_host::OfficeClaim::new(
                    runtime_context,
                    Some(conversation_id.clone()),
                )
                .with_attachment_bucket(Arc::new(
                    super::attachments::ClaimAttachmentBucket::new(
                        Arc::clone(&self.platform),
                        Arc::clone(runtime_context),
                    ),
                )),
            )
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
        // The web families (#1238) meter every provider call through the
        // gateway under this claim; built only when the snapshot has one.
        if let Some(web) = self.web_runtime.as_ref()
            && snapshot_carries_web_family(tool_snapshot)
        {
            artifact_authority =
                artifact_authority.with_web(web.host(&self.model_facade, &context, None));
        }
        // The index tools (ADR-0030 I1) act under the same claim: its vector
        // token, its redeemed context for the query embedding, and a chat
        // model bound on the agent's own model for the step-back tools. The
        // host (and the second model binding it carries) is built only when
        // the snapshot has an I1 toolkit carrying a catalog — the only case
        // anything would call it (review round 1).
        let index_authority = if snapshot_carries_index_catalog(tool_snapshot) {
            self.index_tool_authority(runtime_context, &context, profile, &toolkit_model_budget)
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
            left_out: left_out_toolkits,
        } = materialize_direct_toolsets(
            tool_snapshot,
            &self.mcp_connector,
            tool_policy,
            mcp_tokens,
            &artifact_authority,
            index_support,
            // A governed command (#1120): every credentialed toolkit redeems
            // per invocation; `None` (legacy) installs no wrapper at all.
            governed_client(&self.platform, runtime_context).as_ref(),
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
            elitea_agent_runtime::subagents::TOOLSET_NAME.to_owned(),
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
            .with_left_out_toolkits(left_out_toolkits)
            .with_toolkit_attribution(toolkit_attribution)
            .with_egress(Arc::clone(tool_policy.egress())),
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
        budget: &ModelCallBudget,
    ) -> Result<IndexToolAuthority, IndexToolsWithheld> {
        let runtime = self
            .index_runtime
            .as_ref()
            .ok_or(IndexToolsWithheld::NoVectorStore)?;
        let token = runtime_context
            .vector_token()
            .ok_or(IndexToolsWithheld::NoVectorToken)?;
        let model = LazyStepbackModel::new(self.stepback_model_binder(profile, context, budget));
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
        budget: &ModelCallBudget,
        claim: crate::transport::office_host::OfficeClaim,
    ) -> Option<Arc<dyn elitea_agent_runtime::toolkits::OfficeToolHost>> {
        let runtime = self.office_runtime.as_ref()?;
        let bearer = context.model_facade_token();
        let model = LazyStepbackModel::new(self.stepback_model_binder(profile, context, budget));
        let host = ClaimOfficeHost::new(
            Arc::clone(runtime),
            &bearer,
            &context.resource_project_id().to_string(),
            model,
        );
        if let Ok(host) = host {
            // The `imagegen` toolkit's images API rides on the same host and
            // the same credential (#864).
            let host = host
                .with_claim(claim)
                .with_images(self.model_facade.images(), Arc::clone(context))
                .with_budget(budget.clone());
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
        budget: &ModelCallBudget,
    ) -> StepbackBinder {
        let budget = budget.clone();
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
                // Single use, enforced by the facade; never the agent's turns.
                max_model_turns: 1,
            };
            let bound: Result<BoundModelFacade, ModelFacadeError> =
                facade.bind(adapter, context.as_ref(), model_project_id, invocation);
            let chat = bound?.admitted_chat();
            Ok(StepbackBinding {
                model: chat.model,
                model_name: model_name.clone(),
                config: chat.config,
                budget: Some(budget.clone()),
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
        let invocation = model_invocation(profile, system_instruction, output_continuation);
        let (adapter, adapter_name) = model_adapter(profile);
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

/// The agent's model call: its model and generation controls, and its turns.
fn model_invocation(
    profile: &super::assembly::OrdinaryNoToolProfile,
    system_instruction: &str,
    output_continuation: bool,
) -> ModelInvocation {
    ModelInvocation {
        response_schema: None,
        allow_text_continuation: false,
        context_budget: profile.context_budget(),
        model_name: profile.model_name().to_owned(),
        system_instruction: system_instruction.to_owned(),
        max_tokens: profile.max_tokens(),
        reasoning_effort: profile
            .reasoning_effort()
            .map(|effort| continuation_reasoning_effort(effort, output_continuation)),
        temperature: profile.temperature(),
        max_model_turns: profile.step_limit(),
    }
}

/// The facade adapter for the agent's model provider, and its span name.
fn model_adapter(
    profile: &super::assembly::OrdinaryNoToolProfile,
) -> (ModelAdapterKind, &'static str) {
    match profile.model_provider() {
        OrdinaryModelProvider::OpenAiChat => {
            (ModelAdapterKind::OpenAiCompatible, "openai_compatible")
        }
        OrdinaryModelProvider::NativeAnthropic => (ModelAdapterKind::Anthropic, "anthropic"),
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
            assembly.egress(),
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
                assembly.egress(),
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
    left_out: Vec<LeftOutToolkit>,
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
/// `pptx`, `docx`, `xlsx`) or one that reads and writes files through the same
/// host (`imagegen`; `sharepoint`, whose `upload_file` reads artifacts and
/// whose readers turn documents into text).
/// Whether a configured `web` or `host_intel` toolkit is in the snapshot.
pub(super) fn snapshot_carries_web_family(snapshot: &AdmittedToolSnapshot<'_>) -> bool {
    snapshot.iter().any(|reference| {
        reference.kind() == FrozenToolKind::Configured
            && matches!(reference.tool_type(), "web" | "host_intel")
    })
}

pub(super) fn snapshot_carries_office_family(snapshot: &AdmittedToolSnapshot<'_>) -> bool {
    snapshot.iter().any(|reference| {
        reference.kind() == FrozenToolKind::Configured
            && toolkit_needs_office_host(reference.tool_type(), reference.settings())
    })
}

/// Whether a configured toolkit needs the Office host built for the run. The
/// Office families and `imagegen` always do; `sharepoint` only when its
/// selected tools include one that reads a document or an artifact, so a
/// `get_lists`-only toolkit redeems nothing.
pub(crate) fn toolkit_needs_office_host(
    tool_type: &str,
    settings: Option<&serde_json::Map<String, serde_json::Value>>,
) -> bool {
    match tool_type {
        "pptx" | "docx" | "xlsx" | "imagegen" => true,
        "sharepoint" => settings
            .and_then(|settings| settings.get("selected_tools"))
            .and_then(serde_json::Value::as_array)
            .is_some_and(|selected| {
                selected
                    .iter()
                    .filter_map(serde_json::Value::as_str)
                    .any(elitea_agent_runtime::toolkits::sharepoint_tool_uses_office_host)
            }),
        // Review round 3: every family whose build serves file-store tools
        // only with the host (the tracker attachment tools, carrier's
        // reports), so the agent turn builds what discovery lists.
        family => elitea_agent_runtime::toolkits::toolkit_needs_file_store(family, settings),
    }
}

/// Whether any configured I1 indexing toolkit in the snapshot carries Main's
/// frozen index catalog — the only case the index tools' host is worth
/// building (ADR-0030 I1).
pub(super) fn snapshot_carries_index_catalog(snapshot: &AdmittedToolSnapshot<'_>) -> bool {
    snapshot.iter().any(|reference| {
        reference.kind() == FrozenToolKind::Configured
            && ((crate::toolkits::is_index_family(reference.tool_type())
                && reference.indexes().is_some())
                // The `memory` toolkit acts under the same authority (M9).
                || reference.tool_type() == elitea_agent_runtime::toolkits::MEMORY_TOOLKIT_TYPE)
    })
}

async fn materialize_direct_toolsets(
    snapshot: &AdmittedToolSnapshot<'_>,
    connector: &Arc<dyn McpConnector>,
    policy: &Arc<ToolAdmissionPolicy>,
    mcp_tokens: &serde_json::Map<String, serde_json::Value>,
    artifacts: &ArtifactToolAuthority,
    index_tools: IndexToolSupport<'_>,
    governed: Option<&Arc<GovernedOperationClient>>,
) -> Result<DirectToolsets, NativeAgentAssemblyError> {
    let crate::toolkits::ConfiguredToolsets {
        mut toolsets,
        mut delegated_authorization,
        mut left_out,
    } = materialize_configured_toolsets_reporting_refusals_governed(
        snapshot,
        policy,
        mcp_tokens,
        Some(artifacts),
        index_tools,
        governed,
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
    // A governed run (#1123): every MCP access token is redeemed through
    // the broker, one listing per connection and one session per call, and
    // no claim-time MCP token is used. A connection it cannot govern is left
    // out and named.
    let (mut mcp, mcp_authorization) = if let Some(governed) = governed {
        let built = materialize_governed_mcp_toolsets(snapshot, connector, policy, governed)
            .await
            .map_err(|error| mcp_materialization_error(&error))?;
        left_out.extend(built.left_out);
        (built.toolsets, built.delegated_authorization)
    } else {
        materialize_mcp_toolsets_with_tokens_and_authorization(
            snapshot,
            connector.as_ref(),
            policy,
            mcp_tokens,
        )
        .await
        .map_err(|error| mcp_materialization_error(&error))?
    };
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
        left_out,
    })
}

fn invalid_tool_authorization_catalog() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidConfiguration,
        "the configured delegated authorization catalog is invalid",
    )
}

/// The reasoning effort an output continuation's model call runs at.
///
/// Which knob each model family gets, and why:
///
/// - **An effort is set** (the catalogue marks the model `supports_reasoning`;
///   Main defaults it to `medium` then, `normalizeCurrentAgentModelFamily`):
///   the continuation runs at `low`. That is the SAME parameter the model
///   already receives, at a lower value: `reasoning_effort` on the
///   OpenAI-compatible wire, and the minimum 2048-token thinking budget (or
///   adaptive `low`) on native Anthropic.
/// - **The effort is `none`**: it stays `none`. Turning it into `low` would
///   enable reasoning the user turned off, and `validate_invocation` refuses
///   a non-`none` effort beside a temperature, so every Continue would fail at
///   binding.
/// - **No effort** (the common case: the catalogue does not mark the model
///   `supports_reasoning`, so Main sends `reasoning_effort: null`): NO knob is
///   sent. Nothing proves one is safe. The catalogue has no field that names a
///   vLLM/Qwen-style model, so `chat_template_kwargs.enable_thinking` cannot be
///   targeted. The gateway also drops that field: its chat-request decode keeps
///   no extra parameters. And `reasoning_effort` on a model the catalogue says
///   does not reason is an unsupported parameter. On native Anthropic, no
///   effort already means thinking off. For this family only the direct-answer
///   prompt reduces reasoning. The cap in `AgentEventProjector::finish_after_eos`
///   ends a second reasoning-only cut.
const fn continuation_reasoning_effort(
    effort: ReasoningEffort,
    output_continuation: bool,
) -> ModelReasoningEffort {
    match effort {
        ReasoningEffort::None => ModelReasoningEffort::None,
        _ if output_continuation => ModelReasoningEffort::Low,
        effort => model_reasoning_effort(effort),
    }
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
        ModelFacadeError::InvalidInvocation | ModelFacadeError::InvalidClaimContext => {
            NativeAgentAssemblyErrorCode::InvalidInput
        }
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

#[cfg(test)]
mod office_host_need_tests {
    use super::*;
    use crate::toolkits::{FrozenToolSnapshot, ToolAdmissionPolicy};
    use serde_json::json;
    use std::collections::BTreeMap;

    fn carries(tools: &serde_json::Value) -> bool {
        let version = json!({ "tools": tools });
        let policy = Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy"));
        let snapshot =
            FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
                .expect("snapshot")
                .apply_policy(policy.as_ref());
        snapshot_carries_office_family(&snapshot)
    }

    fn sharepoint(selected: &[&str]) -> serde_json::Value {
        json!({"id": 1, "type": "sharepoint", "toolkit_name": "SP", "settings": {
            "sharepoint_configuration": {
                "client_id": "c", "client_secret": "s",
                "site_url": "https://t.sharepoint.com/sites/x",
                "oauth_discovery_endpoint": "https://login.microsoftonline.com/t"
            },
            "selected_tools": selected
        }})
    }

    #[test]
    fn a_sharepoint_toolkit_redeems_the_host_only_for_tools_that_use_it() {
        // Lists, columns, file metadata, OneNote lists and writes use no host.
        assert!(!carries(&json!([sharepoint(&[
            "get_lists",
            "read_list",
            "get_files_list",
            "create_list_item",
            "onenote_get_pages",
            "onenote_create_page"
        ])])));
        // Each reader and the upload do.
        // Every tool the family says uses the host builds it, and no other.
        for tool in elitea_agent_runtime::toolkits::sharepoint_tool_names() {
            assert_eq!(
                carries(&json!([sharepoint(&["get_lists", tool])])),
                elitea_agent_runtime::toolkits::sharepoint_tool_uses_office_host(tool),
                "{tool}"
            );
        }
        assert!(elitea_agent_runtime::toolkits::sharepoint_tool_uses_office_host("upload_file"));
        assert!(!elitea_agent_runtime::toolkits::sharepoint_tool_uses_office_host("get_lists"));
        // The Office families always do; an unrelated toolkit never.
        assert!(carries(
            &json!([{"id": 2, "type": "pptx", "toolkit_name": "P", "settings": {}}])
        ));
        assert!(!carries(
            &json!([{"id": 3, "type": "github", "toolkit_name": "G", "settings": {}}])
        ));
    }
}
