//! Authorized stored-pipeline admission before any provider or tool authority.
//!
//! Pipeline HITL is a YAML graph node and never uses the direct sensitive-tool
//! confirmation path. This module binds the frozen application shell to the
//! graph compiler and one claim-fenced session/checkpoint boundary. The family
//! remains capability-disabled until lifecycle routing enables this assembler.

#![allow(dead_code)] // Capability routing remains intentionally disabled.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;
use std::time::Duration;

use adk_rust::agent::LlmAgentBuilder;
use adk_rust::{Agent, GenerateContentConfig, ReadonlyContext, Tool, Toolset};
use async_trait::async_trait;
use serde_json::{Map, Value};
use sqlx::PgPool;
use tracing::Instrument as _;

use super::application_tools::{
    ApplicationToolDependencies, MaterializedApplicationRuntime, materialize_application_runtime,
};
use super::assembly::{OrdinaryModelProvider, OrdinaryNoToolProfile, ReasoningEffort};
use super::graph::compiler::PipelineNodeRuntimes;
use super::graph::compiler::{PipelineConfigurationError, PipelineDefinition};
use super::graph::{
    ApplicationExecutionError, DirectToolExecutionError, DirectToolNodeKind, DirectToolSelection,
    LlmExecutionError, LlmExecutionInput, LlmNodeDefinition, PipelineApplicationResolver,
    PipelineApplicationSelection, PipelineDirectToolResolver, PipelineLlmAgentBinding,
    PipelineLlmAgentFactory, PipelineLlmReplayEnvelope, PipelineNodeEventSender, PipelineToolGuard,
    ResolvedApplicationParticipant, ResolvedDirectTool, pipeline_node_event_channel,
    prepare_pipeline_llm_replay,
};
use super::internal_tools::{ASK_USER_TOOL_NAME, ASK_USER_TOOLSET_NAME};
use super::request::AgentExecutionRequest;
use super::runtime::{
    AssembledNativeAgentInvocation, AuthorizedNativeAssembly, NativeAgentAssembler,
    NativeAgentAssemblyError, NativeAgentAssemblyErrorCode,
};
use super::sensitive_tools::policy_for_guardrails;
use super::session::{
    ApplicationRuntimeProjection, BoundOrdinaryAgentModel, NativePipelineStateBackend,
    PipelineAgentCompletion, PipelineRuntimeBindings, assemble_pipeline_native,
};
use crate::protocol::control::ClaimBoundRuntimeContextAuthority;
use crate::state::{CheckpointLimits, SessionLimits};
use crate::toolkits::{
    AdkHttpMcpConnector, AdmittedToolSnapshot, DelegatedAuthorizationCatalog, FrozenToolKind,
    FrozenToolSnapshot, FrozenToolset, McpConnector, SensitiveToolPolicy, ToolAdmissionDecision,
    ToolAdmissionPolicy, ToolBindingError, ToolBindingPlan, bind_frozen_toolsets, freeze_toolsets,
    materialize_configured_toolsets_with_tokens_and_authorization,
    materialize_mcp_toolsets_with_tokens_and_authorization,
};
use crate::transport::model_facade::{
    ModelAdapterKind, ModelFacade, ModelInvocation, ModelReasoningEffort,
};
use crate::transport::platform_client::PlatformClient;
use crate::transport::runtime_context::ClaimScopedEliteaContext;

const MAX_PIPELINE_MATERIALIZED_TOOLS: usize = 1_024;
const MAX_NESTED_PIPELINE_PARTICIPANTS: usize = 25;

type PipelineApplicationBinding = (
    Option<Arc<dyn PipelineApplicationResolver>>,
    ApplicationRuntimeProjection,
);

struct SavedPipelineParticipantReference<'a> {
    alias: &'a str,
    project_id: Option<u64>,
    application_id: u64,
    version_id: u64,
}

struct PipelineApplicationRuntime<'a> {
    context: Arc<ClaimScopedEliteaContext>,
    model_facade: Arc<ModelFacade>,
    node_events: PipelineNodeEventSender,
    mcp_tokens: &'a Map<String, Value>,
    tool_policy: Arc<ToolAdmissionPolicy>,
}

/// Frozen, fully admitted application pipeline definition.
pub(crate) struct PipelineExecutionProfile {
    shell: OrdinaryNoToolProfile,
    definition: PipelineDefinition,
    sensitive_direct_tools: BTreeMap<(String, String), SensitiveToolPolicy>,
    sensitive_llm_tools: BTreeMap<(String, String), SensitiveToolPolicy>,
}

impl PipelineExecutionProfile {
    /// Admit a saved pipeline without constructing a model, tool or credential.
    pub(crate) fn validate(
        request: &AgentExecutionRequest,
        resume: bool,
    ) -> Result<Self, NativeAgentAssemblyError> {
        let shell = OrdinaryNoToolProfile::validate_pipeline_shell(request, resume)?;
        let definition = PipelineDefinition::from_yaml(shell.instructions())
            .map_err(|error| pipeline_configuration_error(&error))?;
        Ok(Self {
            shell,
            definition,
            sensitive_direct_tools: BTreeMap::new(),
            sensitive_llm_tools: BTreeMap::new(),
        })
    }

    pub(crate) fn validate_mcp_authorization_resume(
        request: &AgentExecutionRequest,
    ) -> Result<Self, NativeAgentAssemblyError> {
        let shell = OrdinaryNoToolProfile::validate_pipeline_mcp_authorization_shell(request)?;
        let definition = PipelineDefinition::from_yaml(shell.instructions())
            .map_err(|error| pipeline_configuration_error(&error))?;
        Ok(Self {
            shell,
            definition,
            sensitive_direct_tools: BTreeMap::new(),
            sensitive_llm_tools: BTreeMap::new(),
        })
    }

    pub(crate) fn validate_guardrail_authorization_resume(
        request: &AgentExecutionRequest,
    ) -> Result<Self, NativeAgentAssemblyError> {
        let shell =
            OrdinaryNoToolProfile::validate_pipeline_guardrail_authorization_shell(request)?;
        let definition = PipelineDefinition::from_yaml(shell.instructions())
            .map_err(|error| pipeline_configuration_error(&error))?;
        Ok(Self {
            shell,
            definition,
            sensitive_direct_tools: BTreeMap::new(),
            sensitive_llm_tools: BTreeMap::new(),
        })
    }

    fn from_nested_version(
        version: &serde_json::Map<String, serde_json::Value>,
        fallback: &OrdinaryNoToolProfile,
    ) -> Result<Self, NativeAgentAssemblyError> {
        let shell = OrdinaryNoToolProfile::from_nested_pipeline_version(version, fallback)?;
        let definition = PipelineDefinition::from_yaml(shell.instructions())
            .map_err(|error| pipeline_configuration_error(&error))?;
        Ok(Self {
            shell,
            definition,
            sensitive_direct_tools: BTreeMap::new(),
            sensitive_llm_tools: BTreeMap::new(),
        })
    }

    #[must_use]
    pub(crate) const fn shell(&self) -> &OrdinaryNoToolProfile {
        &self.shell
    }

    #[must_use]
    pub(crate) const fn definition(&self) -> &PipelineDefinition {
        &self.definition
    }

    #[must_use]
    pub(crate) fn into_definition(self) -> PipelineDefinition {
        self.definition
    }

    /// Bind node selections to the exact frozen toolkit aliases before any
    /// credential or client is constructed.
    #[allow(clippy::too_many_lines)] // Security admission stays visibly ordered before redemption.
    pub(crate) fn validate_tool_snapshot(
        &mut self,
        snapshot: &FrozenToolSnapshot<'_>,
        policy: &ToolAdmissionPolicy,
    ) -> Result<(), NativeAgentAssemblyError> {
        self.sensitive_direct_tools.clear();
        self.sensitive_llm_tools.clear();
        for selection in self.definition.llm_tool_selections() {
            if selection.alias() == ASK_USER_TOOLSET_NAME {
                if !self.shell.internal_tools().ask_user_enabled()
                    || selection.tools() != [ASK_USER_TOOL_NAME]
                {
                    return Err(invalid_pipeline_tool_scope());
                }
                continue;
            }
            let mut matches = snapshot
                .iter()
                .filter(|reference| reference.toolkit_name() == selection.alias());
            let Some(reference) = matches.next() else {
                return Err(invalid_pipeline_tool_scope());
            };
            if matches.next().is_some() || reference.kind() == FrozenToolKind::Application {
                return Err(invalid_pipeline_tool_scope());
            }
            if policy.toolkit_decision(reference.tool_type()) != ToolAdmissionDecision::Allowed {
                return Err(unsupported_pipeline_tool_scope());
            }
            for tool_name in selection.tools() {
                if policy.tool_decision(reference.tool_type(), tool_name)
                    != ToolAdmissionDecision::Allowed
                {
                    return Err(unsupported_pipeline_tool_scope());
                }
                if let Some(sensitive) = policy.sensitive_tool(
                    reference.tool_type(),
                    reference.toolkit_name(),
                    tool_name,
                ) {
                    self.sensitive_llm_tools.insert(
                        (selection.alias().to_owned(), tool_name.to_owned()),
                        sensitive,
                    );
                }
                let configured = reference
                    .settings()
                    .and_then(|settings| settings.get("selected_tools"))
                    .and_then(serde_json::Value::as_array);
                if configured.is_some_and(|configured| {
                    !configured.is_empty()
                        && !configured
                            .iter()
                            .any(|name| name.as_str() == Some(tool_name))
                }) {
                    return Err(invalid_pipeline_tool_scope());
                }
            }
        }
        for selection in self.definition.direct_tool_selections() {
            let mut matches = snapshot
                .iter()
                .filter(|reference| reference.toolkit_name() == selection.alias());
            let Some(reference) = matches.next() else {
                return Err(invalid_pipeline_tool_scope());
            };
            let expected_kind = match selection.kind() {
                DirectToolNodeKind::Toolkit => FrozenToolKind::Configured,
                DirectToolNodeKind::Mcp => FrozenToolKind::Mcp,
            };
            if matches.next().is_some() || reference.kind() != expected_kind {
                return Err(invalid_pipeline_tool_scope());
            }
            if policy.toolkit_decision(reference.tool_type()) != ToolAdmissionDecision::Allowed {
                return Err(unsupported_pipeline_tool_scope());
            }
            if policy.tool_decision(reference.tool_type(), selection.tool())
                != ToolAdmissionDecision::Allowed
            {
                return Err(unsupported_pipeline_tool_scope());
            }
            if let Some(sensitive) = policy.sensitive_tool(
                reference.tool_type(),
                reference.toolkit_name(),
                selection.tool(),
            ) {
                self.sensitive_direct_tools.insert(
                    (selection.alias().to_owned(), selection.tool().to_owned()),
                    sensitive,
                );
            }
            let configured = reference
                .settings()
                .and_then(|settings| settings.get("selected_tools"))
                .and_then(serde_json::Value::as_array);
            if configured.is_some_and(|configured| {
                !configured.is_empty()
                    && !configured
                        .iter()
                        .any(|name| name.as_str() == Some(selection.tool()))
            }) {
                return Err(invalid_pipeline_tool_scope());
            }
        }
        let mut application_identities = BTreeMap::new();
        for selection in self.definition.application_selections() {
            let identity = validate_application_selection(selection, snapshot, policy)?;
            if application_identities
                .insert(identity, selection.alias())
                .is_some_and(|alias| alias != selection.alias())
            {
                return Err(invalid_pipeline_tool_scope());
            }
        }
        Ok(())
    }

    fn sensitive_direct_tool(
        &self,
        selection: &DirectToolSelection,
    ) -> Option<SensitiveToolPolicy> {
        self.sensitive_direct_tools
            .get(&(selection.alias().to_owned(), selection.tool().to_owned()))
            .cloned()
    }

    fn sensitive_llm_tools(&self) -> BTreeMap<(String, String), SensitiveToolPolicy> {
        self.sensitive_llm_tools.clone()
    }
}

/// Authorized assembler for stored pipelines backed by ADK `GraphAgent`.
pub(crate) struct PipelineNativeAgentAssembler {
    state: NativePipelineStateBackend,
    tool_policy: Arc<ToolAdmissionPolicy>,
    platform: Option<Arc<PlatformClient>>,
    model_facade: Option<Arc<ModelFacade>>,
    mcp_connector: Arc<dyn McpConnector>,
}

impl PipelineNativeAgentAssembler {
    /// Use the shared `agentstate` database for both ADK sessions and graph
    /// checkpoints, with separate claim-fenced tables and one immutable lease.
    #[must_use]
    pub(crate) fn postgres(
        pool: PgPool,
        session_limits: SessionLimits,
        checkpoint_limits: CheckpointLimits,
        tool_policy: Arc<ToolAdmissionPolicy>,
        platform: Arc<PlatformClient>,
        model_facade: Arc<ModelFacade>,
    ) -> Self {
        Self {
            state: NativePipelineStateBackend::postgres(pool, session_limits, checkpoint_limits),
            tool_policy,
            platform: Some(platform),
            model_facade: Some(model_facade),
            mcp_connector: Arc::new(AdkHttpMcpConnector::new()),
        }
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) fn with_state(
        sessions: Arc<dyn adk_rust::session::SessionService>,
        checkpointer: Arc<dyn adk_rust::graph::Checkpointer>,
    ) -> Self {
        let tool_policy = Arc::new(
            ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new())
                .expect("empty tool policy"),
        );
        Self {
            state: NativePipelineStateBackend::injected(sessions, checkpointer),
            tool_policy,
            platform: None,
            model_facade: None,
            mcp_connector: Arc::new(AdkHttpMcpConnector::new()),
        }
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) fn with_mcp_connector(mut self, connector: Arc<dyn McpConnector>) -> Self {
        self.mcp_connector = connector;
        self
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) fn with_runtime_clients(
        mut self,
        platform: Arc<PlatformClient>,
        model_facade: Arc<ModelFacade>,
    ) -> Self {
        self.platform = Some(platform);
        self.model_facade = Some(model_facade);
        self
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) fn with_tool_policy(mut self, policy: Arc<ToolAdmissionPolicy>) -> Self {
        self.tool_policy = policy;
        self
    }

    #[allow(clippy::too_many_lines)] // Keep node authority handoff in one auditable sequence.
    async fn bind_node_runtimes(
        &self,
        profile: &PipelineExecutionProfile,
        toolsets: AdmittedToolSnapshot<'_>,
        mcp_tokens: &Map<String, Value>,
        runtime_context: &ClaimBoundRuntimeContextAuthority,
        tool_policy: &Arc<ToolAdmissionPolicy>,
    ) -> Result<PipelineRuntimeBindings, NativeAgentAssemblyError> {
        let has_llm_nodes = profile.definition().has_llm_nodes();
        let has_direct_tool_nodes = profile.definition().has_direct_tool_nodes();
        let has_application_nodes = profile.definition().has_application_nodes();
        if !has_llm_nodes && !has_direct_tool_nodes && !has_application_nodes {
            return Ok(PipelineRuntimeBindings {
                nodes: PipelineNodeRuntimes::default(),
                applications: ApplicationRuntimeProjection::default(),
                node_events: None,
            });
        }
        let (node_event_sender, node_events) = pipeline_node_event_channel();
        let (context, model_facade) = if has_llm_nodes || has_application_nodes {
            let platform = self
                .platform
                .as_ref()
                .ok_or_else(unsupported_pipeline_runtime)?;
            let model_facade = self
                .model_facade
                .clone()
                .ok_or_else(unsupported_pipeline_runtime)?;
            tracing::Span::current().record("stage", "runtime_context");
            let context = platform
                .redeem_elitea_context(runtime_context)
                .await
                .map_err(NativeAgentAssemblyError::from)?;
            (Some(Arc::new(context)), Some(model_facade))
        } else {
            (None, None)
        };
        tracing::Span::current().record("stage", "toolsets");
        let aliases = profile.definition().runtime_toolkit_aliases();
        let selected_snapshot = toolsets.retain_toolkit_names(&aliases);
        let (mut materialized, mut delegated_authorization) =
            materialize_configured_toolsets_with_tokens_and_authorization(
                &selected_snapshot,
                tool_policy,
                mcp_tokens,
            )
            .map_err(|_| unsupported_pipeline_runtime())?;
        let (mut mcp, mcp_delegated_authorization) =
            materialize_mcp_toolsets_with_tokens_and_authorization(
                &selected_snapshot,
                self.mcp_connector.as_ref(),
                tool_policy,
                mcp_tokens,
            )
            .await
            .map_err(|_| unsupported_pipeline_runtime())?;
        delegated_authorization
            .merge(mcp_delegated_authorization)
            .map_err(|()| unsupported_pipeline_runtime())?;
        materialized.append(&mut mcp);
        let mut toolsets = toolsets_by_alias(materialized)?;
        for toolset in profile.shell().internal_tools().toolsets() {
            if toolsets
                .insert(toolset.name().to_owned(), toolset)
                .is_some()
            {
                return Err(invalid_pipeline_tool_scope());
            }
        }
        let toolsets = freeze_toolsets_by_alias(toolsets).await?;
        let direct_tool_resolver = build_direct_tool_resolver(profile, &toolsets)?;
        let application_runtime =
            context
                .clone()
                .zip(model_facade.clone())
                .map(|(context, model_facade)| PipelineApplicationRuntime {
                    context,
                    model_facade,
                    node_events: node_event_sender.clone(),
                    mcp_tokens,
                    tool_policy: Arc::clone(tool_policy),
                });
        let (application_resolver, application_runtime) = self
            .build_application_resolver(
                profile,
                &selected_snapshot,
                runtime_context,
                application_runtime,
            )
            .await?;
        let llm_factory = context.zip(model_facade).map(|(context, model_facade)| {
            Arc::new(NativePipelineLlmAgentFactory {
                profile: profile.shell().clone(),
                context,
                model_facade,
                toolsets,
                sensitive_tools: profile.sensitive_llm_tools(),
                delegated_authorization,
                ask_user_enabled: profile.shell().internal_tools().ask_user_enabled(),
                node_events: node_event_sender,
            }) as Arc<dyn PipelineLlmAgentFactory>
        });
        Ok(PipelineRuntimeBindings {
            nodes: PipelineNodeRuntimes::new(
                llm_factory,
                direct_tool_resolver,
                application_resolver,
            ),
            applications: application_runtime,
            node_events: Some(node_events),
        })
    }

    async fn build_application_resolver(
        &self,
        profile: &PipelineExecutionProfile,
        snapshot: &AdmittedToolSnapshot<'_>,
        runtime_context: &ClaimBoundRuntimeContextAuthority,
        runtime: Option<PipelineApplicationRuntime<'_>>,
    ) -> Result<PipelineApplicationBinding, NativeAgentAssemblyError> {
        if !profile.definition().has_application_nodes() {
            return Ok((None, ApplicationRuntimeProjection::default()));
        }
        let platform = self
            .platform
            .as_ref()
            .ok_or_else(unsupported_pipeline_runtime)?;
        let runtime = runtime.ok_or_else(unsupported_pipeline_runtime)?;
        let expected = profile
            .definition()
            .application_selections()
            .map(PipelineApplicationSelection::alias)
            .map(str::to_owned)
            .collect::<BTreeSet<_>>();
        if expected.len() > MAX_NESTED_PIPELINE_PARTICIPANTS {
            return Err(invalid_pipeline_tool_scope());
        }
        let direct_aliases = snapshot
            .iter()
            .filter(|reference| reference.kind() == FrozenToolKind::Application)
            .filter(|reference| reference.application_agent_type() == Some("agent"))
            .map(|reference| reference.toolkit_name().to_owned())
            .filter(|alias| expected.contains(alias))
            .collect::<BTreeSet<_>>();
        let direct_runtime = materialize_application_runtime(
            snapshot,
            platform,
            runtime_context,
            Arc::clone(&runtime.context),
            profile.shell(),
            ApplicationToolDependencies::new(
                Arc::clone(&runtime.model_facade),
                Arc::clone(&runtime.tool_policy),
                Arc::clone(&self.mcp_connector),
                runtime.mcp_tokens,
            ),
            Some(&direct_aliases),
        )
        .await?;
        let (mut participants, mut projection) = direct_runtime.map_or_else(
            || Ok((BTreeMap::new(), ApplicationRuntimeProjection::default())),
            application_participants,
        )?;
        for reference in snapshot.iter().filter(|reference| {
            reference.kind() == FrozenToolKind::Application
                && reference.application_agent_type() == Some("pipeline")
                && expected.contains(reference.toolkit_name())
        }) {
            let (application_id, version_id) = reference
                .application_identity()
                .ok_or_else(invalid_pipeline_tool_scope)?;
            let participant = self
                .materialize_saved_pipeline_participant(
                    SavedPipelineParticipantReference {
                        alias: reference.toolkit_name(),
                        project_id: reference.application_project_id(),
                        application_id,
                        version_id,
                    },
                    profile.shell(),
                    runtime_context,
                    &runtime,
                )
                .await?;
            projection
                .insert_presentation(
                    reference.toolkit_name().to_owned(),
                    reference.toolkit_name().to_owned(),
                    "pipeline".to_owned(),
                )
                .map_err(|_| invalid_pipeline_tool_scope())?;
            if participants
                .insert(reference.toolkit_name().to_owned(), participant)
                .is_some()
            {
                return Err(invalid_pipeline_tool_scope());
            }
        }
        let actual = participants.keys().cloned().collect::<BTreeSet<_>>();
        if actual != expected {
            return Err(invalid_pipeline_tool_scope());
        }
        let resolver = Arc::new(NativePipelineApplicationResolver { participants });
        Ok((Some(resolver), projection))
    }

    async fn materialize_saved_pipeline_participant(
        &self,
        reference: SavedPipelineParticipantReference<'_>,
        fallback: &OrdinaryNoToolProfile,
        runtime_context: &ClaimBoundRuntimeContextAuthority,
        runtime: &PipelineApplicationRuntime<'_>,
    ) -> Result<NativePipelineApplicationParticipant, NativeAgentAssemblyError> {
        if reference
            .project_id
            .is_some_and(|project_id| project_id != runtime.context.resource_project_id())
        {
            return Err(invalid_pipeline_tool_scope());
        }
        let platform = self
            .platform
            .as_ref()
            .ok_or_else(unsupported_pipeline_runtime)?;
        let loaded = platform
            .resolve_application_version(
                runtime_context,
                reference.application_id,
                reference.version_id,
            )
            .await
            .map_err(NativeAgentAssemblyError::from)?;
        let version = loaded.into_version_details();
        let mut child = PipelineExecutionProfile::from_nested_version(&version, fallback)?;
        let frozen = FrozenToolSnapshot::from_version_details(&version)
            .map_err(|_| invalid_pipeline_tool_scope())?;
        child.validate_tool_snapshot(&frozen, runtime.tool_policy.as_ref())?;
        if child.definition().has_application_nodes() {
            // The recursive materializer must retain the same loaded-version
            // cycle/hop owner before deeper saved participants activate.
            return Err(unsupported_pipeline_runtime());
        }
        let admitted = frozen.apply_policy(runtime.tool_policy.as_ref());
        let runtimes = self
            .bind_nested_pipeline_runtimes(&child, admitted, runtime)
            .await?;
        tracing::debug!(
            application_alias = reference.alias,
            "materialized saved pipeline participant"
        );
        Ok(NativePipelineApplicationParticipant::Pipeline {
            definition: Box::new(child.into_definition()),
            runtimes,
            events: runtime.node_events.clone(),
            display_name: reference.alias.to_owned(),
        })
    }

    async fn bind_nested_pipeline_runtimes(
        &self,
        profile: &PipelineExecutionProfile,
        snapshot: AdmittedToolSnapshot<'_>,
        runtime: &PipelineApplicationRuntime<'_>,
    ) -> Result<PipelineNodeRuntimes, NativeAgentAssemblyError> {
        let aliases = profile.definition().runtime_toolkit_aliases();
        let selected = snapshot.retain_toolkit_names(&aliases);
        let (mut materialized, mut delegated_authorization) =
            materialize_configured_toolsets_with_tokens_and_authorization(
                &selected,
                &runtime.tool_policy,
                runtime.mcp_tokens,
            )
            .map_err(|_| unsupported_pipeline_runtime())?;
        let (mut mcp, mcp_delegated_authorization) =
            materialize_mcp_toolsets_with_tokens_and_authorization(
                &selected,
                self.mcp_connector.as_ref(),
                &runtime.tool_policy,
                runtime.mcp_tokens,
            )
            .await
            .map_err(|_| unsupported_pipeline_runtime())?;
        delegated_authorization
            .merge(mcp_delegated_authorization)
            .map_err(|()| unsupported_pipeline_runtime())?;
        materialized.append(&mut mcp);
        let mut toolsets = toolsets_by_alias(materialized)?;
        for toolset in profile.shell().internal_tools().toolsets() {
            if toolsets
                .insert(toolset.name().to_owned(), toolset)
                .is_some()
            {
                return Err(invalid_pipeline_tool_scope());
            }
        }
        let toolsets = freeze_toolsets_by_alias(toolsets).await?;
        let direct_tool_resolver = build_direct_tool_resolver(profile, &toolsets)?;
        let llm_factory = profile.definition().has_llm_nodes().then(|| {
            Arc::new(NativePipelineLlmAgentFactory {
                profile: profile.shell().clone(),
                context: Arc::clone(&runtime.context),
                model_facade: Arc::clone(&runtime.model_facade),
                toolsets,
                sensitive_tools: profile.sensitive_llm_tools(),
                delegated_authorization,
                ask_user_enabled: profile.shell().internal_tools().ask_user_enabled(),
                node_events: runtime.node_events.clone(),
            }) as Arc<dyn PipelineLlmAgentFactory>
        });
        Ok(PipelineNodeRuntimes::new(
            llm_factory,
            direct_tool_resolver,
            None,
        ))
    }
}

#[async_trait]
impl NativeAgentAssembler for PipelineNativeAgentAssembler {
    type Completion = PipelineAgentCompletion;

    async fn assemble(
        &self,
        assembly: AuthorizedNativeAssembly<'_>,
    ) -> Result<AssembledNativeAgentInvocation<Self::Completion>, NativeAgentAssemblyError> {
        let span = tracing::info_span!(
            "agent.pipeline.assemble",
            execution_kind = ?assembly.request().kind,
            stage = tracing::field::Empty,
            session_backend = "postgres_graph",
            outcome = tracing::field::Empty,
            error_code = tracing::field::Empty,
        );
        let result = async {
            tracing::Span::current().record("stage", "admission");
            let tool_policy = policy_for_guardrails(
                assembly.request().payload.toolkit_guardrails.as_ref(),
                &self.tool_policy,
            )?;
            // #606, the pipeline twin of the ordinary path's read. `platform`
            // is optional here only because the injected-state test constructor
            // has none; a stored pipeline that runs for real always does, and a
            // missing client leaves attachments rendered by their headers, the
            // same as an unreadable file.
            let assembly = match self.platform.as_ref() {
                Some(platform) => {
                    assembly
                        .resolve_attachment_contents(platform.as_ref())
                        .await
                }
                None => assembly,
            };
            let admitted = assembly.admit_pipeline_with_policy(tool_policy.as_ref())?;
            let (profile, plan, toolsets, mcp_tokens, start, runtime_context, session, lease) =
                admitted.into_parts();
            let node_runtimes = self
                .bind_node_runtimes(
                    &profile,
                    toolsets,
                    mcp_tokens,
                    &runtime_context,
                    &tool_policy,
                )
                .await?;
            tracing::Span::current().record("stage", "state");
            assemble_pipeline_native(
                plan,
                profile.into_definition(),
                start,
                session,
                lease,
                &self.state,
                node_runtimes,
            )
            .await
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

struct NativePipelineLlmAgentFactory {
    profile: OrdinaryNoToolProfile,
    context: Arc<ClaimScopedEliteaContext>,
    model_facade: Arc<ModelFacade>,
    toolsets: BTreeMap<String, FrozenToolset>,
    sensitive_tools: BTreeMap<(String, String), SensitiveToolPolicy>,
    delegated_authorization: DelegatedAuthorizationCatalog,
    ask_user_enabled: bool,
    node_events: PipelineNodeEventSender,
}

struct NativePipelineDirectToolResolver {
    tools: BTreeMap<(String, String), ResolvedDirectTool>,
}

struct NativePipelineApplicationResolver {
    participants: BTreeMap<String, NativePipelineApplicationParticipant>,
}

impl NativePipelineLlmAgentFactory {
    fn bind_selected_tools(
        &self,
        definition: &LlmNodeDefinition,
    ) -> Result<ToolBindingPlan, LlmExecutionError> {
        let mut selected = Vec::new();
        for selection in definition.tool_selections() {
            if selection.tools().is_empty() {
                continue;
            }
            selected.push(
                self.toolsets
                    .get(selection.alias())
                    .ok_or(LlmExecutionError::Unavailable)?
                    .select(selection.tools())
                    .map_err(|_| LlmExecutionError::Unavailable)?,
            );
        }
        bind_frozen_toolsets(
            &selected,
            &BTreeSet::from([ASK_USER_TOOLSET_NAME.to_owned()]),
        )
        .map_err(|_| LlmExecutionError::Unavailable)
    }

    fn guards_for_binding(
        &self,
        binding: &ToolBindingPlan,
    ) -> Result<BTreeMap<String, PipelineToolGuard>, LlmExecutionError> {
        let mut guards = BTreeMap::new();
        for (toolkit_name, logical_name, provider_name) in binding.bindings() {
            let guard = if let Some(requirement) = self
                .delegated_authorization
                .requirement_for_scoped(toolkit_name, logical_name)
            {
                Some(PipelineToolGuard::DelegatedAuthorization(
                    requirement.clone(),
                ))
            } else if self.ask_user_enabled
                && toolkit_name == ASK_USER_TOOLSET_NAME
                && logical_name == ASK_USER_TOOL_NAME
            {
                Some(PipelineToolGuard::AskUser)
            } else {
                self.sensitive_tools
                    .get(&(toolkit_name.to_owned(), logical_name.to_owned()))
                    .cloned()
                    .map(PipelineToolGuard::Sensitive)
            };
            if let Some(guard) = guard
                && guards.insert(provider_name.to_owned(), guard).is_some()
            {
                return Err(LlmExecutionError::Unavailable);
            }
        }
        Ok(guards)
    }
}

#[derive(Clone)]
enum NativePipelineApplicationParticipant {
    Agent(Arc<dyn Tool>),
    Pipeline {
        definition: Box<PipelineDefinition>,
        runtimes: PipelineNodeRuntimes,
        events: PipelineNodeEventSender,
        display_name: String,
    },
}

impl PipelineApplicationResolver for NativePipelineApplicationResolver {
    fn resolve(
        &self,
        selection: &PipelineApplicationSelection,
        checkpointer: Arc<dyn adk_rust::graph::Checkpointer>,
    ) -> Result<ResolvedApplicationParticipant, ApplicationExecutionError> {
        let participant = self
            .participants
            .get(selection.alias())
            .cloned()
            .ok_or(ApplicationExecutionError::Unavailable)?;
        match participant {
            NativePipelineApplicationParticipant::Agent(tool) => {
                Ok(ResolvedApplicationParticipant::Agent(tool))
            }
            NativePipelineApplicationParticipant::Pipeline {
                definition,
                runtimes,
                events,
                display_name,
            } => definition
                .compile_subgraph_with_runtime(checkpointer, &runtimes)
                .map(|graph| ResolvedApplicationParticipant::Pipeline {
                    graph: Arc::new(graph),
                    events: Some(events),
                    display_name,
                })
                .map_err(|_| ApplicationExecutionError::Unavailable),
        }
    }
}

fn application_participants(
    runtime: MaterializedApplicationRuntime,
) -> Result<
    (
        BTreeMap<String, NativePipelineApplicationParticipant>,
        ApplicationRuntimeProjection,
    ),
    NativeAgentAssemblyError,
> {
    let MaterializedApplicationRuntime {
        tools,
        presentations,
        events,
        resume,
    } = runtime;
    let mut participants = BTreeMap::new();
    for entry in tools {
        if participants
            .insert(
                entry.alias,
                NativePipelineApplicationParticipant::Agent(entry.tool),
            )
            .is_some()
        {
            return Err(invalid_pipeline_tool_scope());
        }
    }
    Ok((
        participants,
        ApplicationRuntimeProjection::streaming(presentations, events, resume),
    ))
}

impl PipelineDirectToolResolver for NativePipelineDirectToolResolver {
    fn resolve(
        &self,
        selection: &DirectToolSelection,
    ) -> Result<ResolvedDirectTool, DirectToolExecutionError> {
        self.tools
            .get(&(selection.alias().to_owned(), selection.tool().to_owned()))
            .cloned()
            .ok_or(DirectToolExecutionError::Unavailable)
    }
}

impl PipelineLlmAgentFactory for NativePipelineLlmAgentFactory {
    fn build(
        &self,
        definition: &LlmNodeDefinition,
        input: &LlmExecutionInput,
        output_schema: Option<serde_json::Value>,
        replay: Option<&PipelineLlmReplayEnvelope>,
    ) -> Result<PipelineLlmAgentBinding, LlmExecutionError> {
        let system_instruction = if input.system().trim().is_empty() {
            "You are an AI assistant executing one bounded Elitea pipeline node.".to_owned()
        } else {
            input.system().to_owned()
        };
        let invocation = ModelInvocation {
            model_name: self.profile.model_name().to_owned(),
            system_instruction,
            max_tokens: self.profile.max_tokens(),
            reasoning_effort: self.profile.reasoning_effort().map(model_reasoning_effort),
            temperature: self.profile.temperature(),
            max_model_turns: self.profile.step_limit(),
        };
        let adapter = match self.profile.model_provider() {
            OrdinaryModelProvider::OpenAiChat => ModelAdapterKind::OpenAiCompatible,
            OrdinaryModelProvider::NativeAnthropic => ModelAdapterKind::Anthropic,
        };
        let model = self
            .model_facade
            .bind(
                adapter,
                self.context.as_ref(),
                self.profile.model_project_id(),
                invocation,
            )
            .map_err(|_| LlmExecutionError::Unavailable)?;
        let binding = self.bind_selected_tools(definition)?;
        let mut guards = self.guards_for_binding(&binding)?;
        let mut authorization = DelegatedAuthorizationCatalog::default();
        for (name, guard) in &guards {
            if let PipelineToolGuard::DelegatedAuthorization(requirement) = guard {
                authorization
                    .insert(name, requirement.clone())
                    .map_err(|()| LlmExecutionError::Unavailable)?;
            }
        }
        if let Some(replay) = replay {
            replay.apply_authorization_scope(&mut authorization)?;
        }
        let selected_toolsets = binding.into_toolsets();
        let (model, selected_toolsets) =
            prepare_pipeline_llm_replay(model.provider_model(), selected_toolsets, replay);
        let (model, selected_toolsets) = crate::toolkits::bind_authorization_model_tools(
            model,
            selected_toolsets,
            &mut authorization,
        )
        .map_err(|_| LlmExecutionError::Unavailable)?;
        guards.retain(|name, _| !authorization.is_declined(name));
        for name in authorization.tool_names() {
            if let Some(requirement) = authorization.requirement_for(name) {
                guards.insert(
                    name.to_owned(),
                    PipelineToolGuard::DelegatedAuthorization(requirement.clone()),
                );
            }
        }
        if replay.is_some_and(|replay| {
            guards.iter().any(|(tool_name, guard)| {
                matches!(guard, PipelineToolGuard::DelegatedAuthorization(_))
                    && replay.has_deferred_authorization_for(tool_name)
            })
        }) {
            // An authorize continuation must rematerialize the real tool. If
            // the protected server still yields a placeholder, do not turn
            // that failed authorization into a model-visible tool error.
            return Err(LlmExecutionError::Unavailable);
        }
        let mut builder = LlmAgentBuilder::new(definition.id())
            .description("Elitea stored-pipeline LLM node")
            .model(model)
            .generate_content_config(GenerateContentConfig {
                temperature: self.profile.temperature(),
                max_output_tokens: self
                    .profile
                    .max_tokens()
                    .and_then(|value| i32::try_from(value).ok()),
                ..GenerateContentConfig::default()
            })
            .max_iterations(self.profile.step_limit())
            .tool_timeout(Duration::from_secs(definition.tool_execution_timeout()))
            .disallow_transfer_to_parent(true)
            .disallow_transfer_to_peers(true);
        if let Some(schema) = output_schema {
            builder = builder.output_schema(schema).output_max_retries(2);
        }
        for toolset in selected_toolsets {
            builder = builder.toolset(toolset);
        }
        for tool_name in guards.keys() {
            builder = builder.require_tool_confirmation(tool_name);
        }
        let agent = builder
            .build()
            .map(|agent| Arc::new(agent) as Arc<dyn Agent>)
            .map_err(|_| LlmExecutionError::Unavailable)?;
        Ok(PipelineLlmAgentBinding::new(agent, guards))
    }

    fn event_sender(&self) -> Option<PipelineNodeEventSender> {
        Some(self.node_events.clone())
    }
}

pub(super) struct StrictNodeToolset {
    name: String,
    inner: Arc<dyn Toolset>,
    selected: Vec<String>,
}

impl StrictNodeToolset {
    pub(super) fn new(alias: &str, inner: Arc<dyn Toolset>, selected: &[String]) -> Self {
        Self {
            name: format!("pipeline_{alias}"),
            inner,
            selected: selected.to_vec(),
        }
    }
}

#[async_trait]
impl Toolset for StrictNodeToolset {
    fn name(&self) -> &str {
        &self.name
    }

    async fn tools(
        &self,
        context: Arc<dyn ReadonlyContext>,
    ) -> adk_rust::Result<Vec<Arc<dyn Tool>>> {
        let available = self.inner.tools(context).await?;
        let mut by_name = std::collections::BTreeMap::new();
        for tool in available {
            if by_name.insert(tool.name().to_owned(), tool).is_some() {
                return Err(adk_rust::AdkError::config(
                    "a pipeline toolkit exposes duplicate tool names",
                ));
            }
        }
        self.selected
            .iter()
            .map(|name| {
                by_name.get(name).cloned().ok_or_else(|| {
                    adk_rust::AdkError::config("a pipeline LLM node selected an unavailable tool")
                })
            })
            .collect()
    }
}

fn toolsets_by_alias(
    toolsets: Vec<Arc<dyn Toolset>>,
) -> Result<std::collections::BTreeMap<String, Arc<dyn Toolset>>, NativeAgentAssemblyError> {
    let mut by_alias = std::collections::BTreeMap::new();
    for toolset in toolsets {
        if by_alias
            .insert(toolset.name().to_owned(), toolset)
            .is_some()
        {
            return Err(invalid_pipeline_tool_scope());
        }
    }
    Ok(by_alias)
}

async fn freeze_toolsets_by_alias(
    toolsets: BTreeMap<String, Arc<dyn Toolset>>,
) -> Result<BTreeMap<String, FrozenToolset>, NativeAgentAssemblyError> {
    let frozen = freeze_toolsets(
        toolsets.into_values().collect(),
        "elitea_pipeline_tool_binding",
    )
    .await
    .map_err(tool_binding_error)?;
    let mut by_alias = BTreeMap::new();
    for toolset in frozen {
        if by_alias
            .insert(toolset.name().to_owned(), toolset)
            .is_some()
        {
            return Err(invalid_pipeline_tool_scope());
        }
    }
    Ok(by_alias)
}

fn build_direct_tool_resolver(
    profile: &PipelineExecutionProfile,
    toolsets: &BTreeMap<String, FrozenToolset>,
) -> Result<Option<Arc<dyn PipelineDirectToolResolver>>, NativeAgentAssemblyError> {
    let selections = profile
        .definition()
        .direct_tool_selections()
        .collect::<Vec<_>>();
    if selections.is_empty() {
        return Ok(None);
    }
    let aliases = selections
        .iter()
        .map(|selection| selection.alias())
        .collect::<BTreeSet<_>>();
    let mut tools = BTreeMap::new();
    for alias in aliases {
        let toolset = toolsets
            .get(alias)
            .ok_or_else(invalid_pipeline_tool_scope)?;
        let available = toolset.tools();
        if available.len() > MAX_PIPELINE_MATERIALIZED_TOOLS {
            return Err(invalid_pipeline_tool_scope());
        }
        let mut by_name = BTreeMap::new();
        for tool in available {
            if by_name
                .insert(tool.name().to_owned(), Arc::clone(tool))
                .is_some()
            {
                return Err(invalid_pipeline_tool_scope());
            }
        }
        for selection in selections
            .iter()
            .filter(|selection| selection.alias() == alias)
        {
            let tool = by_name
                .get(selection.tool())
                .cloned()
                .ok_or_else(invalid_pipeline_tool_scope)?;
            if !tool.is_read_only() {
                // Direct effects need graph-native durable confirmation and an
                // effect receipt/reconciliation owner before activation.
                return Err(unsupported_pipeline_tool_scope());
            }
            tools.insert(
                (selection.alias().to_owned(), selection.tool().to_owned()),
                ResolvedDirectTool::new(tool, profile.sensitive_direct_tool(selection)),
            );
        }
    }
    Ok(Some(Arc::new(NativePipelineDirectToolResolver { tools })))
}

fn validate_application_selection(
    selection: &PipelineApplicationSelection,
    snapshot: &FrozenToolSnapshot<'_>,
    policy: &ToolAdmissionPolicy,
) -> Result<(u64, u64), NativeAgentAssemblyError> {
    let mut matches = snapshot
        .iter()
        .filter(|reference| reference.toolkit_name() == selection.alias());
    let Some(reference) = matches.next() else {
        return Err(invalid_pipeline_tool_scope());
    };
    if matches.next().is_some()
        || reference.kind() != FrozenToolKind::Application
        || !matches!(
            reference.application_agent_type(),
            Some("agent" | "pipeline")
        )
    {
        return Err(invalid_pipeline_tool_scope());
    }
    if policy.toolkit_decision(reference.tool_type()) != ToolAdmissionDecision::Allowed {
        return Err(unsupported_pipeline_tool_scope());
    }
    reference
        .application_identity()
        .ok_or_else(invalid_pipeline_tool_scope)
}

const fn model_reasoning_effort(effort: ReasoningEffort) -> ModelReasoningEffort {
    match effort {
        ReasoningEffort::Low => ModelReasoningEffort::Low,
        ReasoningEffort::Medium => ModelReasoningEffort::Medium,
        ReasoningEffort::High => ModelReasoningEffort::High,
        ReasoningEffort::None => ModelReasoningEffort::None,
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
    NativeAgentAssemblyError::new(
        code,
        "the pipeline model-callable toolkit namespace is invalid",
    )
}

fn pipeline_configuration_error(error: &PipelineConfigurationError) -> NativeAgentAssemblyError {
    let code = match error.code() {
        "graph.pipeline.configuration_resource_exhausted" => {
            NativeAgentAssemblyErrorCode::ResourceExhausted
        }
        "graph.pipeline.unsupported_capability" => {
            NativeAgentAssemblyErrorCode::UnsupportedCapability
        }
        "graph.pipeline.malformed_yaml" | "graph.pipeline.invalid_configuration" => {
            NativeAgentAssemblyErrorCode::InvalidInput
        }
        _ => NativeAgentAssemblyErrorCode::InvalidConfiguration,
    };
    NativeAgentAssemblyError::new(code, "the stored pipeline definition could not be admitted")
}

const fn invalid_pipeline_tool_scope() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidInput,
        "a pipeline LLM node references a tool outside its frozen scope",
    )
}

const fn unsupported_pipeline_tool_scope() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::UnsupportedCapability,
        "a pipeline LLM node selected a tool whose graph authorization is not enabled",
    )
}

const fn unsupported_pipeline_runtime() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::UnsupportedCapability,
        "the native pipeline LLM runtime is not available",
    )
}
