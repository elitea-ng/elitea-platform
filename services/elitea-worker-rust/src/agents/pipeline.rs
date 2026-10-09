//! Authorized stored-pipeline admission before any provider or tool authority.
//!
//! Pipeline HITL is a YAML graph node and never uses the direct sensitive-tool
//! confirmation path. This module binds the frozen application shell to the
//! graph compiler and one claim-fenced session/checkpoint boundary. The family
//! remains capability-disabled until lifecycle routing enables this assembler.

#![allow(dead_code)] // Capability routing remains intentionally disabled.
pub(crate) mod saved_child_http;
pub(crate) mod saved_child_scope_provider;

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
    ApplicationMaterializationPath, ApplicationToolDependencies, MaterializedApplicationRuntime,
    materialize_application_runtime,
};
use super::assembly::{OrdinaryModelProvider, OrdinaryNoToolProfile, ReasoningEffort};
use super::context_management::ContextManagementPlan;
use super::events::{ApplicationToolGuardCatalogs, ApplicationToolPresentationCatalog};
use super::graph::compiler::PipelineNodeRuntimes;
use super::graph::compiler::{PipelineConfigurationError, PipelineDefinition};
use super::graph::{
    ApplicationExecutionError, DirectToolExecutionError, DirectToolNodeKind, DirectToolSelection,
    LlmExecutionError, LlmExecutionInput, LlmNodeDefinition, PipelineApplicationResolver,
    PipelineApplicationSelection, PipelineDirectToolResolver, PipelineLlmAgentBinding,
    PipelineLlmAgentFactory, PipelineLlmReplayEnvelope, PipelineModelScope,
    PipelineNodeEventSender, PipelineToolGuard, ResolvedApplicationParticipant, ResolvedDirectTool,
    pipeline_node_event_channel, prepare_pipeline_llm_replay,
};
use super::internal_tools::{ASK_USER_TOOL_NAME, ASK_USER_TOOLSET_NAME};
use super::model_scope::ModelScopeSessions;
use super::ordinary::{mcp_materialization_error, tool_materialization_error};
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
    AdkHttpMcpConnector, AdmittedToolSnapshot, ArtifactToolAuthority,
    DelegatedAuthorizationCatalog, FrozenToolKind, FrozenToolSnapshot, FrozenToolset, McpConnector,
    SensitiveToolPolicy, ToolAdmissionDecision, ToolAdmissionPolicy, ToolBindingError,
    ToolBindingPlan, bind_frozen_toolsets, freeze_toolsets,
    materialize_configured_toolsets_with_artifact_authority,
    materialize_configured_toolsets_with_tokens_and_authorization,
    materialize_mcp_toolsets_with_tokens_and_authorization,
};
use crate::transport::model_facade::{
    ModelAdapterKind, ModelFacade, ModelInvocation, ModelReasoningEffort,
};
use crate::transport::platform_client::PlatformClient;
use crate::transport::platform_writer::ClaimPlatformWriter;
use crate::transport::runtime_context::{ClaimScopedEliteaContext, SavedAgentFingerprint};

pub(crate) mod composition;
// Source integration gate, not a new public flag. Keep false until the assembled
// producer/coordinator tests and durable/browser acceptance below pass.
pub(super) const SCOPED_APPLICATION_CONTINUATION_READY: bool = false;
pub(crate) mod scope_receipts;
pub(crate) mod scoped_applications;
pub(crate) mod scoped_runtime;

use composition::{AdmittedPipelineComposition, AdmittedSavedPipeline};

const MAX_PIPELINE_MATERIALIZED_TOOLS: usize = 1_024;
const MAX_NESTED_PIPELINE_PARTICIPANTS: usize = 25;

type PipelineApplicationBinding = (
    Option<Arc<dyn PipelineApplicationResolver>>,
    ApplicationRuntimeProjection,
    Option<Arc<scoped_applications::PipelineApplicationScopeRegistry>>,
);

struct SavedPipelineParticipantReference<'a> {
    alias: &'a str,
    project_id: Option<u64>,
    application_id: u64,
    version_id: u64,
}

#[derive(Clone)]
struct PipelineApplicationRuntime<'a> {
    platform: &'a PlatformClient,
    authority: &'a ClaimBoundRuntimeContextAuthority,
    connector: Arc<dyn McpConnector>,
    code: Option<Arc<dyn super::graph::CodeSandboxRuntime>>,
    context: Arc<ClaimScopedEliteaContext>,
    model_facade: Arc<ModelFacade>,
    node_events: PipelineNodeEventSender,
    mcp_tokens: &'a Map<String, Value>,
    tool_policy: Arc<ToolAdmissionPolicy>,
    model_scopes: ModelScopeSessions,
    conversation_thread_id: String,
    materialization: ApplicationMaterializationPath,
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

    pub(super) fn from_nested_version(
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
        // Each refusal names the node kind that selected the alias.
        type ScopeError = fn() -> NativeAgentAssemblyError;
        let mut aliases = BTreeMap::new();
        let llm_aliases = self
            .definition
            .llm_tool_selections()
            .map(super::graph::LlmToolkitSelection::alias)
            .map(|alias| (alias, invalid_pipeline_tool_scope as ScopeError));
        let direct_aliases = self
            .definition
            .direct_tool_selections()
            .map(super::graph::DirectToolSelection::alias)
            .map(|alias| (alias, invalid_direct_tool_scope as ScopeError));
        for (alias, invalid_scope) in llm_aliases.chain(direct_aliases) {
            if alias == ASK_USER_TOOLSET_NAME
                || snapshot
                    .iter()
                    .any(|reference| reference.toolkit_name() == alias)
            {
                continue;
            }
            let key = legacy_toolkit_key(alias);
            let mut matches = snapshot.iter().filter(|reference| {
                !key.is_empty() && legacy_toolkit_key(reference.toolkit_name()) == key
            });
            let canonical = matches.next().ok_or_else(invalid_scope)?;
            if matches.next().is_some() {
                return Err(invalid_scope());
            }
            aliases.insert(alias.to_owned(), canonical.toolkit_name().to_owned());
        }
        self.definition.resolve_legacy_toolkit_aliases(&aliases);
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
                return Err(invalid_direct_tool_scope());
            };
            let expected_kind = match selection.kind() {
                DirectToolNodeKind::Toolkit => FrozenToolKind::Configured,
                DirectToolNodeKind::Mcp => FrozenToolKind::Mcp,
            };
            if matches.next().is_some() || reference.kind() != expected_kind {
                return Err(invalid_direct_tool_scope());
            }
            if policy.toolkit_decision(reference.tool_type()) != ToolAdmissionDecision::Allowed {
                return Err(unsupported_direct_tool_scope());
            }
            if policy.tool_decision(reference.tool_type(), selection.tool())
                != ToolAdmissionDecision::Allowed
            {
                return Err(unsupported_direct_tool_scope());
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
                return Err(invalid_direct_tool_scope());
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
    sandbox: Option<Arc<super::graph::CodeRuntimeFactory>>,
    state: NativePipelineStateBackend,
    tool_policy: Arc<ToolAdmissionPolicy>,
    platform: Option<Arc<PlatformClient>>,
    model_facade: Option<Arc<ModelFacade>>,
    mcp_connector: Arc<dyn McpConnector>,
}

impl PipelineNativeAgentAssembler {
    pub(crate) fn with_sandbox(
        mut self,
        sandbox: Option<Arc<super::graph::CodeRuntimeFactory>>,
    ) -> Self {
        self.sandbox = sandbox;
        self
    }
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
            sandbox: None,
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
            sandbox: None,
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

    #[allow(clippy::too_many_arguments, clippy::too_many_lines)] // Keep distinct authority owners explicit at node binding.
    async fn bind_node_runtimes(
        &self,
        profile: &PipelineExecutionProfile,
        toolsets: AdmittedToolSnapshot<'_>,
        mcp_tokens: &Map<String, Value>,
        runtime_context: &Arc<ClaimBoundRuntimeContextAuthority>,
        tool_policy: &Arc<ToolAdmissionPolicy>,
        model_scopes: ModelScopeSessions,
        code: Option<Arc<dyn super::graph::CodeSandboxRuntime>>,
        composition: &AdmittedPipelineComposition,
        scoped_applications: bool,
        conversation_thread_id: String,
    ) -> Result<PipelineRuntimeBindings, NativeAgentAssemblyError> {
        let has_llm_nodes = profile.definition().has_llm_nodes();
        let has_direct_tool_nodes = profile.definition().has_direct_tool_nodes();
        let has_application_nodes = profile.definition().has_application_nodes();
        let (node_event_sender, node_events) = pipeline_node_event_channel();
        if !has_llm_nodes && !has_direct_tool_nodes && !has_application_nodes {
            return Ok(PipelineRuntimeBindings {
                nodes: PipelineNodeRuntimes::default().with_events(node_event_sender),
                applications: ApplicationRuntimeProjection::default(),
                node_events: Some(node_events),
            });
        }
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
        // The claim is lent to the `artifact` family here as on the ordinary
        // path (#906): its authority is the live claim, and a direct node or a
        // model calls it mid-run, after assembly. Without it the family is
        // skipped and every node on an attached artifact toolkit is refused.
        let artifacts = self.platform.as_ref().map(|platform| {
            ArtifactToolAuthority::new(Arc::new(ClaimPlatformWriter::new(
                Arc::clone(platform),
                Arc::clone(runtime_context),
            )))
        });
        let (mut materialized, mut delegated_authorization) =
            materialize_configured_toolsets_with_artifact_authority(
                &selected_snapshot,
                tool_policy,
                mcp_tokens,
                artifacts.as_ref(),
            )
            .await
            .map_err(tool_materialization_error)?;
        let (mut mcp, mcp_delegated_authorization) =
            materialize_mcp_toolsets_with_tokens_and_authorization(
                &selected_snapshot,
                self.mcp_connector.as_ref(),
                tool_policy,
                mcp_tokens,
            )
            .await
            .map_err(|error| mcp_materialization_error(&error))?;
        delegated_authorization
            .merge(mcp_delegated_authorization)
            .map_err(|()| unsupported_pipeline_runtime())?;
        materialized.append(&mut mcp);
        let mut toolsets = toolsets_by_alias(materialized)?;
        // `None`: a PIPELINE node does not bind the builder tools (#940 A8).
        // Both cases the modules exist for are chat turns, and a pipeline's
        // node profile carries no conversation the user toggled anything on
        // for — the shell's internal tools here come from the stored version,
        // not from a Modules menu. A toggle stored on a pipeline version is
        // therefore not silently honoured with a project write nobody asked
        // for in this run.
        for toolset in profile.shell().internal_tools().toolsets(None) {
            if toolsets
                .insert(toolset.name().to_owned(), toolset)
                .is_some()
            {
                return Err(invalid_pipeline_tool_scope());
            }
        }
        let toolsets = freeze_toolsets_by_alias(toolsets).await?;
        let direct_tool_resolver = build_direct_tool_resolver(profile, &toolsets)?;
        let application_runtime = context
            .clone()
            .zip(model_facade.clone())
            .zip(self.platform.as_ref())
            .map(
                |((context, model_facade), platform)| PipelineApplicationRuntime {
                    platform,
                    authority: runtime_context,
                    connector: Arc::clone(&self.mcp_connector),
                    code,
                    context,
                    model_facade,
                    node_events: node_event_sender.clone(),
                    mcp_tokens,
                    tool_policy: Arc::clone(tool_policy),
                    model_scopes: model_scopes.clone(),
                    conversation_thread_id,
                    materialization: ApplicationMaterializationPath::native_graph(
                        scoped_applications,
                    ),
                },
            );
        let (application_resolver, application_runtime, application_scopes) = if scoped_applications
        {
            self.build_scoped_application_resolver(
                profile,
                &selected_snapshot,
                runtime_context,
                application_runtime,
                composition,
            )
            .await?
        } else {
            self.build_application_resolver(
                profile,
                &selected_snapshot,
                runtime_context,
                application_runtime,
                composition,
            )
            .await?
        };
        let llm_factory = context.zip(model_facade).map(|(context, model_facade)| {
            Arc::new(NativePipelineLlmAgentFactory {
                profile: profile.shell().clone(),
                context,
                model_facade,
                toolsets,
                sensitive_tools: profile.sensitive_llm_tools(),
                delegated_authorization,
                ask_user_enabled: profile.shell().internal_tools().ask_user_enabled(),
                node_events: node_event_sender.clone(),
                model_scopes,
            }) as Arc<dyn PipelineLlmAgentFactory>
        });
        let mut nodes =
            PipelineNodeRuntimes::new(llm_factory, direct_tool_resolver, application_resolver)
                .with_events(node_event_sender)
                .with_composition_definitions(composition::composition_definitions_for(
                    profile.definition(),
                    &composition.children,
                ));
        if let Some(scopes) = application_scopes {
            let catalog = scopes.checkpoint_catalog()?;
            scopes.validate_catalog(profile.definition(), &catalog)?;
            nodes = nodes
                .with_checkpoint_catalog(catalog)
                .with_application_scopes(scopes);
        }
        Ok(PipelineRuntimeBindings {
            nodes,
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
        composition: &AdmittedPipelineComposition,
    ) -> Result<PipelineApplicationBinding, NativeAgentAssemblyError> {
        if !profile.definition().has_application_nodes() {
            return Ok((None, ApplicationRuntimeProjection::default(), None));
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
            )
            .with_model_scopes(runtime.model_scopes.clone())
            .with_code(runtime.code.clone())
            .with_materialization(runtime.materialization.clone()),
            Some(&direct_aliases),
        )
        .await?;
        // The pipeline PARENT admits no pipeline CHILD through this path (it
        // builds those itself), so the skipped list is always empty here.
        let (direct_runtime, _) = direct_runtime;
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
            let (participant, child_presentations) = self
                .materialize_saved_pipeline_participant(
                    SavedPipelineParticipantReference {
                        alias: reference.toolkit_name(),
                        project_id: reference.application_project_id(),
                        application_id,
                        version_id,
                    },
                    &runtime,
                    composition
                        .children
                        .get(reference.toolkit_name())
                        .cloned()
                        .ok_or_else(invalid_pipeline_tool_scope)?,
                )
                .await?;
            projection
                .insert_pipeline_presentation(
                    reference.toolkit_name().to_owned(),
                    reference.toolkit_name().to_owned(),
                    child_presentations,
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
        let resolver = Arc::new(NativePipelineApplicationResolver {
            participants,
            nodes: BTreeMap::new(),
            call_owners: BTreeMap::new(),
        });
        Ok((Some(resolver), projection, None))
    }

    // Producer and scoped installer are wired; source gate awaits integrated compilation/runtime proof.
    async fn build_scoped_application_resolver(
        &self,
        profile: &PipelineExecutionProfile,
        snapshot: &AdmittedToolSnapshot<'_>,
        _runtime_context: &ClaimBoundRuntimeContextAuthority,
        runtime: Option<PipelineApplicationRuntime<'_>>,
        composition: &AdmittedPipelineComposition,
    ) -> Result<PipelineApplicationBinding, NativeAgentAssemblyError> {
        if !profile.definition().has_application_nodes() {
            return Ok((None, ApplicationRuntimeProjection::default(), None));
        }
        let runtime = runtime.ok_or_else(unsupported_pipeline_runtime)?;
        let mut scopes = scoped_applications::PipelineApplicationScopeRegistry::default();
        scopes.register_scope(String::new(), profile.definition(), None)?;
        let mut nodes = BTreeMap::new();
        let mut call_owners = BTreeMap::new();
        let mut presentations = ApplicationToolPresentationCatalog::default();
        let mut presented = BTreeSet::new();
        for (node, selection) in profile.definition().application_nodes() {
            let alias = selection.alias();
            call_owners.insert(node.to_owned(), scopes.graph_call_owner("", node)?);
            let participant = if let Some(child) = composition.children.get(alias) {
                let bound = bind_admitted_saved_pipeline(
                    child.as_ref(),
                    &runtime,
                    self.mcp_connector.as_ref(),
                    node.to_owned(),
                )
                .await?;
                if let Some(child_scopes) = bound.runtimes.application_scopes() {
                    scopes.merge(child_scopes.clone())?;
                }
                if presented.insert(alias.to_owned()) {
                    presentations
                        .insert_runtime(
                            alias.to_owned(),
                            alias.to_owned(),
                            "pipeline".to_owned(),
                            "nested-model".to_owned(),
                            bound.presentations,
                            ApplicationToolGuardCatalogs::default(),
                        )
                        .map_err(|_| invalid_pipeline_tool_scope())?;
                }
                NativePipelineApplicationParticipant::Pipeline {
                    definition: Box::new(child.profile.definition().clone()),
                    runtimes: bound.runtimes,
                    events: runtime.node_events.clone(),
                    display_name: alias.to_owned(),
                }
            } else {
                let (tool, projection, fingerprint) =
                    bind_scoped_agent(snapshot, profile.shell(), &runtime, alias).await?;
                if presented.insert(alias.to_owned()) {
                    presentations = merge_application_presentations(
                        presentations,
                        projection.presentation_catalog(),
                    )?;
                }
                let scope = scopes.bind_application(
                    "",
                    node,
                    Arc::new(projection),
                    runtime.node_events.clone(),
                )?;
                NativePipelineApplicationParticipant::ScopedAgent {
                    tool,
                    scope,
                    fingerprint,
                }
            };
            if nodes
                .insert(node.to_owned(), (alias.to_owned(), participant))
                .is_some()
            {
                return Err(invalid_pipeline_tool_scope());
            }
        }
        Ok((
            Some(Arc::new(NativePipelineApplicationResolver {
                participants: BTreeMap::new(),
                nodes,
                call_owners,
            })),
            ApplicationRuntimeProjection::from_presentations(presentations),
            Some(Arc::new(scopes)),
        ))
    }

    async fn materialize_saved_pipeline_participant(
        &self,
        reference: SavedPipelineParticipantReference<'_>,
        runtime: &PipelineApplicationRuntime<'_>,
        admitted: Arc<AdmittedSavedPipeline>,
    ) -> Result<
        (
            NativePipelineApplicationParticipant,
            ApplicationToolPresentationCatalog,
        ),
        NativeAgentAssemblyError,
    > {
        let bound = bind_admitted_saved_pipeline(
            admitted.as_ref(),
            runtime,
            self.mcp_connector.as_ref(),
            String::new(),
        )
        .await?;
        let definition = admitted.profile.definition().clone();
        tracing::debug!(
            application_alias = reference.alias,
            "materialized saved pipeline participant"
        );
        Ok((
            NativePipelineApplicationParticipant::Pipeline {
                definition: Box::new(definition),
                runtimes: bound.runtimes,
                events: runtime.node_events.clone(),
                display_name: reference.alias.to_owned(),
            },
            bound.presentations,
        ))
    }

    /// The pipeline PARENT's own nested participant, bound by the shared
    /// admission below so the two callers cannot drift about what a nested
    /// pipeline may run (#990 review 7).
    async fn bind_nested_pipeline_runtimes(
        &self,
        profile: &PipelineExecutionProfile,
        snapshot: AdmittedToolSnapshot<'_>,
        runtime: &PipelineApplicationRuntime<'_>,
    ) -> Result<PipelineNodeRuntimes, NativeAgentAssemblyError> {
        bind_saved_pipeline_runtimes(
            profile,
            snapshot,
            runtime,
            self.mcp_connector.as_ref(),
            None,
        )
        .await
        .map(|bound| bound.runtimes)
    }
}

#[async_trait]
impl NativeAgentAssembler for PipelineNativeAgentAssembler {
    type Completion = PipelineAgentCompletion;

    async fn inspect_node_recovery(
        &self,
        request: &AgentExecutionRequest,
        command: &super::session::AuthorizedNativeCommandBinding,
        session: crate::protocol::control::ClaimBoundSessionAuthority,
        lease: Arc<dyn crate::state::StateWriterLease>,
        receipt: &super::graph::node_recovery_receipt::NodeRecoveryRequiredReceipt,
    ) -> Result<super::node_recovery_checkpoint::OpenedNodeRecoveryVisit, NativeAgentAssemblyError>
    {
        let policy = policy_for_guardrails(
            request.payload.toolkit_guardrails.as_ref(),
            &self.tool_policy,
        )?;
        let (profile, plan, _, _) = super::runtime::admit_pipeline_plan(
            request,
            command,
            &request.payload.input_attachments,
            &policy,
        )?;
        self.state
            .open(session, lease, &plan, profile.definition())
            .await?
            .inspect_node_recovery(&plan, profile.definition(), receipt)
            .await
    }

    fn sandbox_stop_delivery(
        &self,
    ) -> Option<Arc<dyn crate::sandbox::dispatch::SandboxStopDelivery>> {
        self.sandbox.as_ref().map(|factory| {
            factory.clone() as Arc<dyn crate::sandbox::dispatch::SandboxStopDelivery>
        })
    }

    async fn inspect_checkpoint(
        &self,
        request: &AgentExecutionRequest,
        command: &super::session::AuthorizedNativeCommandBinding,
        session: crate::protocol::control::ClaimBoundSessionAuthority,
        lease: Arc<dyn crate::state::StateWriterLease>,
    ) -> Result<super::session::ValidatedModelCheckpoint, NativeAgentAssemblyError> {
        let policy = policy_for_guardrails(
            request.payload.toolkit_guardrails.as_ref(),
            &self.tool_policy,
        )?;
        let (profile, plan, _, _) = super::runtime::admit_pipeline_plan(
            request,
            command,
            &request.payload.input_attachments,
            &policy,
        )?;
        self.state
            .open(session, lease, &plan, profile.definition())
            .await?
            .inspect_checkpoint(&plan, profile.definition())
            .await
    }

    async fn assemble_checkpoint(
        &self,
        assembly: AuthorizedNativeAssembly<'_>,
    ) -> Result<
        super::runtime::PendingRecoveredAgentInvocation<Self::Completion>,
        NativeAgentAssemblyError,
    > {
        let (assembled, evidence) = self.assemble_with_recovery(assembly, true, None).await?;
        let evidence = evidence.ok_or_else(unsupported_pipeline_runtime)?;
        Ok(super::runtime::PendingRecoveredAgentInvocation::new(
            assembled, evidence,
        ))
    }

    async fn assemble(
        &self,
        assembly: AuthorizedNativeAssembly<'_>,
    ) -> Result<AssembledNativeAgentInvocation<Self::Completion>, NativeAgentAssemblyError> {
        self.assemble_with_recovery(assembly, false, None)
            .await
            .map(|(assembled, _)| assembled)
    }

    async fn assemble_node_checkpoint(
        &self,
        assembly: AuthorizedNativeAssembly<'_>,
        authority: &crate::protocol::control::NodeRecoveryAssemblyAuthorization,
    ) -> Result<
        super::runtime::PendingRecoveredAgentInvocation<Self::Completion>,
        NativeAgentAssemblyError,
    > {
        let (assembled, evidence) = self
            .assemble_with_recovery(assembly, false, Some(authority))
            .await?;
        let evidence = evidence.ok_or_else(unsupported_pipeline_runtime)?;
        Ok(super::runtime::PendingRecoveredAgentInvocation::new(
            assembled, evidence,
        ))
    }
}

impl PipelineNativeAgentAssembler {
    #[allow(clippy::too_many_lines)] // Keep frozen admission, runtime binding, and coordinator ownership together.
    async fn assemble_with_recovery(
        &self,
        assembly: AuthorizedNativeAssembly<'_>,
        recovery: bool,
        node_recovery: Option<&crate::protocol::control::NodeRecoveryAssemblyAuthorization>,
    ) -> Result<
        (
            AssembledNativeAgentInvocation<PipelineAgentCompletion>,
            Option<super::session::ValidatedModelCheckpoint>,
        ),
        NativeAgentAssemblyError,
    > {
        let span = tracing::info_span!(
            "agent.pipeline.assemble",
            execution_kind = ?assembly.request().kind,
            stage = tracing::field::Empty,
            session_backend = "postgres_graph",
            outcome = tracing::field::Empty,
            error_code = tracing::field::Empty,
        );
        let result: Result<_, NativeAgentAssemblyError> = async {
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
            let assembly = match self.platform.as_ref().filter(|_| !recovery) {
                Some(platform) => {
                    // No attachment tools on a pipeline: the overview note
                    // must not offer them.
                    assembly
                        .resolve_attachment_contents(platform.as_ref(), false)
                        .await
                }
                None => assembly,
            };
            let sandbox_authority = assembly.sandbox_authority();
            let admitted = assembly.admit_pipeline_with_policy(tool_policy.as_ref())?;
            let (profile, plan, toolsets, mcp_tokens, start, runtime_context, session, lease) =
                admitted.into_parts();
            // Shared, never duplicated: the artifact family keeps it past assembly.
            let runtime_context = Arc::new(runtime_context);
            // Resolve the complete frozen tree before credentials or executable runtimes.
            let composition = if profile.definition().has_application_nodes() {
                let platform = self
                    .platform
                    .as_ref()
                    .ok_or_else(unsupported_pipeline_runtime)?;
                composition::admit_root_composition(
                    platform,
                    &runtime_context,
                    plan.resource_project_id()
                        .parse::<u64>()
                        .map_err(|_| invalid_pipeline_tool_scope())?,
                    tool_policy.as_ref(),
                    &profile,
                    &toolsets,
                )
                .await?
            } else {
                AdmittedPipelineComposition {
                    children: BTreeMap::new(),
                    checkpoint_paths: Vec::new(),
                }
            };
            tracing::Span::current().record("stage", "state");
            let mut state = self
                .state
                .open_with_application_paths(
                    session,
                    lease,
                    &plan,
                    profile.definition(),
                    &composition.checkpoint_paths,
                )
                .await?;
            let evidence = if let Some(authority) = node_recovery {
                let visit = state
                    .inspect_node_recovery(&plan, profile.definition(), authority.receipt())
                    .await?;
                visit
                    .journal
                    .verify_applied_revision(authority.journal_revision())
                    .await
                    .map_err(|_| invalid_pipeline_tool_scope())?;
                if !authority.matches(visit.checkpoint()) {
                    return Err(invalid_pipeline_tool_scope());
                }
                Some(visit.into_checkpoint())
            } else if recovery {
                let evidence = state
                    .inspect_checkpoint(&plan, profile.definition())
                    .await?;
                state.model_scopes = state.model_scopes.with_pending_model_recovery();
                Some(evidence)
            } else {
                None
            };
            let start = if recovery || node_recovery.is_some() {
                super::runtime::PipelineNativeStart::Checkpoint
            } else {
                start
            };
            let scoped_applications = if !SCOPED_APPLICATION_CONTINUATION_READY {
                false
            } else if matches!(
                &start,
                super::runtime::PipelineNativeStart::Fresh
                    | super::runtime::PipelineNativeStart::Regenerate
            ) {
                true
            } else {
                // Existing admitted root conversations keep their original resolver and coordinator.
                let checkpoint = state
                    .application_checkpoint(&plan)
                    .await?
                    .ok_or_else(invalid_pipeline_tool_scope)?;
                scope_receipts::has_graph_call_receipts(&checkpoint)
                    .map_err(|_| invalid_pipeline_tool_scope())?
            };
            let mut node_runtimes = self
                .bind_node_runtimes(
                    &profile,
                    toolsets,
                    mcp_tokens,
                    &runtime_context,
                    &tool_policy,
                    state.model_scopes.clone(),
                    self.sandbox
                        .as_ref()
                        .zip(sandbox_authority.clone())
                        .map(|(factory, authority)| factory.bind(authority)),
                    &composition,
                    scoped_applications,
                    plan.session_id().to_owned(),
                )
                .await?;
            if let (Some(factory), Some(authority)) = (&self.sandbox, sandbox_authority) {
                node_runtimes.nodes = factory.attach(node_runtimes.nodes, authority);
            }
            tracing::Span::current().record("stage", "state");
            let assembled = assemble_pipeline_native(
                plan,
                profile.into_definition(),
                start,
                state,
                node_runtimes,
            )
            .await?;
            Ok((assembled, evidence))
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
    model_scopes: ModelScopeSessions,
}

struct NativePipelineDirectToolResolver {
    tools: BTreeMap<(String, String), ResolvedDirectTool>,
}

#[path = "pipeline/parallel_application_resolver.rs"]
mod parallel_application_resolver;

struct NativePipelineApplicationResolver {
    call_owners: BTreeMap<String, Arc<scoped_runtime::PipelineGraphCallOwner>>,
    nodes: BTreeMap<String, (String, NativePipelineApplicationParticipant)>,
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
    ScopedAgent {
        tool: Arc<dyn Tool>,
        scope: Arc<scoped_runtime::PipelineApplicationNodeRuntime>,
        fingerprint: Option<SavedAgentFingerprint>,
    },
    Agent(Arc<dyn Tool>),
    Pipeline {
        definition: Box<PipelineDefinition>,
        runtimes: PipelineNodeRuntimes,
        events: PipelineNodeEventSender,
        display_name: String,
    },
}

impl NativePipelineApplicationResolver {
    fn resolved(
        participant: NativePipelineApplicationParticipant,
        checkpointer: Arc<dyn adk_rust::graph::Checkpointer>,
    ) -> Result<ResolvedApplicationParticipant, ApplicationExecutionError> {
        match participant {
            NativePipelineApplicationParticipant::ScopedAgent { tool, scope, .. } => {
                Ok(ResolvedApplicationParticipant::ScopedAgent { tool, scope })
            }
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
                    variable_types: definition.declared_variable_types(),
                    static_pauses: definition.static_pause_catalog(),
                    events: Some(events),
                    display_name,
                })
                .map_err(|_| ApplicationExecutionError::Unavailable),
        }
    }
}
impl PipelineApplicationResolver for NativePipelineApplicationResolver {
    fn map_worker_definition_digest(
        &self,
        node: &str,
    ) -> Result<[u8; 32], ApplicationExecutionError> {
        let (_, participant) = self
            .nodes
            .get(node)
            .ok_or(ApplicationExecutionError::Unavailable)?;
        match participant {
            NativePipelineApplicationParticipant::Pipeline { definition, .. }
                if definition.map_nonpausing_effectfree() =>
            {
                self.parallel_participant_digest(node)
            }
            _ => Err(ApplicationExecutionError::Unavailable),
        }
    }
    fn for_map_events(
        &self,
        events: PipelineNodeEventSender,
    ) -> Result<Arc<dyn PipelineApplicationResolver>, ApplicationExecutionError> {
        // The existing rebinder retains saved identity, call owners and exact scope.
        self.rebind_parallel_events(&events)
            .map(|resolver| Arc::new(resolver) as Arc<dyn PipelineApplicationResolver>)
    }

    fn for_parallel_events(
        &self,
        events: PipelineNodeEventSender,
    ) -> Result<Arc<dyn PipelineApplicationResolver>, ApplicationExecutionError> {
        self.rebind_parallel_events(&events)
            .map(|resolver| Arc::new(resolver) as Arc<dyn PipelineApplicationResolver>)
    }

    fn fixed_parallel_definition_digest(
        &self,
        node: &str,
    ) -> Result<[u8; 32], ApplicationExecutionError> {
        self.parallel_participant_digest(node)
    }

    fn graph_call_owner(&self, node: &str) -> Option<Arc<scoped_runtime::PipelineGraphCallOwner>> {
        self.call_owners.get(node).cloned()
    }

    fn resolve_node(
        &self,
        node: &str,
        selection: &PipelineApplicationSelection,
        checkpointer: Arc<dyn adk_rust::graph::Checkpointer>,
    ) -> Result<ResolvedApplicationParticipant, ApplicationExecutionError> {
        if self.nodes.is_empty() {
            return self.resolve(selection, checkpointer);
        }
        let (alias, participant) = self
            .nodes
            .get(node)
            .ok_or(ApplicationExecutionError::Unavailable)?;
        if alias != selection.alias() {
            return Err(ApplicationExecutionError::Unavailable);
        }
        Self::resolved(participant.clone(), checkpointer)
    }
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
        Self::resolved(participant, checkpointer)
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
    #[allow(clippy::too_many_lines)] // Keep model, replay, instruction, and tool binding in one ordered path.
    fn build(
        &self,
        definition: &LlmNodeDefinition,
        input: &LlmExecutionInput,
        output_schema: Option<serde_json::Value>,
        replay: Option<&PipelineLlmReplayEnvelope>,
        scope: &PipelineModelScope,
    ) -> Result<PipelineLlmAgentBinding, LlmExecutionError> {
        let system_instruction = if input.system().trim().is_empty() {
            "You are an AI assistant executing one bounded Elitea pipeline node.".to_owned()
        } else {
            input.system().to_owned()
        };
        let invocation = ModelInvocation {
            response_schema: output_schema.clone(),
            allow_text_continuation: output_schema.is_some(),
            context_budget: self.profile.context_budget(),
            model_name: self.profile.model_name().to_owned(),
            system_instruction,
            max_tokens: self.profile.max_tokens(),
            reasoning_effort: self.profile.reasoning_effort().map(model_reasoning_effort),
            temperature: self.profile.temperature(),
            max_model_turns: self.profile.step_limit()
                + crate::agents::request::MAX_OUTPUT_CONTINUATION_CALLS,
        };
        let adapter = match self.profile.model_provider() {
            OrdinaryModelProvider::OpenAiChat => ModelAdapterKind::OpenAiCompatible,
            OrdinaryModelProvider::NativeAnthropic => ModelAdapterKind::Anthropic,
        };
        let model = self
            .model_facade
            .bind_with_summary(
                adapter,
                self.context.as_ref(),
                self.profile.model_project_id(),
                invocation,
                self.profile.summary_model(),
            )
            .map_err(|_| LlmExecutionError::Unavailable)?;
        let plan = match self.profile.context_management() {
            ContextManagementPlan::Disabled => None,
            ContextManagementPlan::Summarize(plan) => Some(plan),
        };
        let checkpoint = self
            .model_scopes
            .for_node(scope.identity())
            .checkpoint(
                plan,
                model.request_budget(),
                model
                    .summarization_model()
                    .ok_or(LlmExecutionError::Unavailable)?,
                None,
                model.durable_completion(),
            )
            .with_replay_pending(replay.is_some());
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
        if binding
            .bindings()
            .any(|(_, _, name)| matches!(name, "load_skill" | "read_project_context"))
        {
            return Err(LlmExecutionError::Unavailable);
        }
        let mut selected_toolsets = binding.into_toolsets();
        selected_toolsets.extend(self.profile.instruction_plan().toolsets());
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
            .model(checkpoint.clone().delegation_model(model))
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
        let instruction_plan = self.profile.instruction_plan().for_pipeline_node();
        builder = instruction_plan.bind_builder(builder);
        builder = checkpoint.clone().bind(builder);
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
        let agent = checkpoint.wrap(agent);
        Ok(
            PipelineLlmAgentBinding::new(instruction_plan.wrap(agent), guards)
                .with_instruction_inheritance(
                    self.profile.instruction_plan().inherits_pipeline_parent(),
                ),
        )
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

/// Materialize one saved pipeline as a participant an ORDINARY agent can call
/// as a tool (#973).
///
/// This is the same admission the pipeline parent performs for an `agent`
/// node's pipeline participant — resolve the exact frozen version, admit its
/// node/tool scope against the live policy, admit the bounded frozen child
/// composition, and bind invocation-owned runtimes. What differs is only the caller:
/// there the compiled graph becomes an ADK `SubgraphNode` of the parent graph,
/// here it becomes the body of one `Tool` the parent model may call.
// Every owner is an explicit authority boundary — the claim, the project
// scope, the model facade, the tool policy and the child's own identity —
// and bundling them would hide which of them the admission below checks.
#[allow(clippy::too_many_arguments)]
pub(super) async fn materialize_saved_pipeline_tool(
    platform: &PlatformClient,
    runtime_context: &ClaimBoundRuntimeContextAuthority,
    context: Arc<ClaimScopedEliteaContext>,
    model_facade: Arc<ModelFacade>,
    mcp_connector: &Arc<dyn McpConnector>,
    mcp_tokens: &Map<String, Value>,
    tool_policy: Arc<ToolAdmissionPolicy>,
    fallback: &OrdinaryNoToolProfile,
    node_events: PipelineNodeEventSender,
    identity: (u64, u64),
    project_id: Option<u64>,
    model_scopes: ModelScopeSessions,
    code: Option<Arc<dyn super::graph::CodeSandboxRuntime>>,
    conversation_thread_id: String,
    materialization: ApplicationMaterializationPath,
) -> Result<
    (
        PipelineDefinition,
        PipelineNodeRuntimes,
        ApplicationToolPresentationCatalog,
    ),
    NativeAgentAssemblyError,
> {
    let runtime = PipelineApplicationRuntime {
        platform,
        authority: runtime_context,
        connector: Arc::clone(mcp_connector),
        code,
        context,
        model_facade,
        node_events,
        mcp_tokens,
        tool_policy,
        model_scopes,
        conversation_thread_id,
        materialization,
    };
    let (definition, bound) = admit_saved_pipeline_child(
        platform,
        runtime_context,
        &runtime,
        mcp_connector.as_ref(),
        fallback,
        identity,
        project_id,
    )
    .await?;
    // Static and scoped ordinary descendants use their typed coordinators.
    // Direct graph sensitive, AskUser, and MCP pauses remain unsupported here.
    let scoped_proof = if runtime.materialization.scoped_ready() {
        let scopes = bound
            .runtimes
            .application_scopes()
            .ok_or_else(invalid_pipeline_tool_scope)?;
        scopes.validate_catalog(
            &definition,
            bound
                .runtimes
                .checkpoint_catalog()
                .ok_or_else(invalid_pipeline_tool_scope)?,
        )?;
        scopes.has_ordinary_applications()
    } else {
        false
    };
    if bound
        .guarded_interrupt_kinds
        .iter()
        .any(|kind| match *kind {
            "static continuation" => !runtime.materialization.scoped_ready(),
            "ordinary child continuation" => !scoped_proof,
            _ => true,
        })
    {
        return Err(unsupported_pipeline_child_pauses(
            &bound.guarded_interrupt_kinds,
        ));
    }
    Ok((definition, bound.runtimes, bound.presentations))
}

/// The one refusal that names WHAT the child could pause on.
///
/// The message reaches the user through the skipped-child notice, so it says
/// the capability rather than a code: "needs sensitive tool approval, which
/// this worker cannot resume from inside an agent".
fn unsupported_pipeline_child_pauses(kinds: &[&'static str]) -> NativeAgentAssemblyError {
    tracing::debug!(?kinds, "pipeline child pause kinds refused");
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::UnsupportedCapability,
        "the attached pipeline pauses on a kind this worker cannot resume inside an agent",
    )
}

/// Resolve and ADMIT one saved pipeline child, up to but not including what
/// the caller will do with it (#990 review 7).
///
/// Both callers — the pipeline parent's `agent` node and the ordinary parent's
/// tool — use the same frozen composition admission. Project scope, exact
/// identities, cycles and resource bounds are checked before runtime binding.
/// Each child profile and selected tool scope are checked against the live policy.
async fn admit_saved_pipeline_child(
    platform: &PlatformClient,
    runtime_context: &ClaimBoundRuntimeContextAuthority,
    runtime: &PipelineApplicationRuntime<'_>,
    mcp_connector: &dyn McpConnector,
    fallback: &OrdinaryNoToolProfile,
    identity: (u64, u64),
    project_id: Option<u64>,
) -> Result<(PipelineDefinition, BoundSavedPipeline), NativeAgentAssemblyError> {
    let admitted = composition::admit_tool_composition_with_scoped(
        platform,
        runtime_context,
        runtime.context.resource_project_id(),
        runtime.tool_policy.as_ref(),
        identity,
        project_id,
        fallback,
        runtime.materialization.scoped_ready(),
    )
    .await?;
    let bound =
        bind_admitted_saved_pipeline(admitted.as_ref(), runtime, mcp_connector, String::new())
            .await?;
    Ok((admitted.profile.definition().clone(), bound))
}

/// Bind the invocation-owned node runtimes of one saved pipeline participant.
///
/// Extracted from `PipelineNativeAgentAssembler::bind_nested_pipeline_runtimes`
/// so the pipeline parent and the ordinary parent (#973) cannot drift about
/// what a nested pipeline is allowed to run: same alias retention, same
/// `None` for the builder tools, same direct-tool read-only gate.
/// One nested pipeline's bound runtimes, plus what its own nodes may PAUSE on.
///
/// The guard surface travels with the runtimes because only the caller can
/// decide which typed continuation routes its coordinator supports.
/// The ordinary parent keeps refusing direct graph sensitive, `AskUser`, and MCP pauses.
pub(super) struct BoundSavedPipeline {
    pub(super) runtimes: PipelineNodeRuntimes,
    /// The interrupt kinds this child's stored definition can raise besides
    /// its own `hitl` nodes, in a stable order, empty for a child that can
    /// raise none.
    pub(super) guarded_interrupt_kinds: Vec<&'static str>,
    pub(super) presentations: ApplicationToolPresentationCatalog,
}

type PipelineBindingFuture<'a> = std::pin::Pin<
    Box<
        dyn std::future::Future<Output = Result<BoundSavedPipeline, NativeAgentAssemblyError>>
            + Send
            + 'a,
    >,
>;

#[allow(clippy::too_many_lines)] // Keep frozen admission, runtime binding, and coordinator ownership together.
fn bind_admitted_saved_pipeline<'a>(
    admitted: &'a AdmittedSavedPipeline,
    runtime: &'a PipelineApplicationRuntime<'_>,
    connector: &'a dyn McpConnector,
    graph_path: String,
) -> PipelineBindingFuture<'a> {
    Box::pin(async move {
        let mut runtime = runtime.clone();
        runtime.materialization = runtime.materialization.enter_pipeline((
            admitted.revision.application_id,
            admitted.revision.version_id,
        ))?;
        runtime.materialization.admit_version(&admitted.version)?;
        let runtime = &runtime;
        let mut scopes = scoped_applications::PipelineApplicationScopeRegistry::default();
        scopes.register_scope(
            graph_path.clone(),
            admitted.profile.definition(),
            Some(admitted.revision.clone()),
        )?;
        let frozen = FrozenToolSnapshot::from_version_details(&admitted.version)
            .map_err(|_| invalid_pipeline_tool_scope())?;
        let snapshot = frozen.apply_policy(runtime.tool_policy.as_ref());
        let mut nodes = BTreeMap::new();
        let mut call_owners = BTreeMap::new();
        let mut descendant_guards = BTreeSet::new();
        let mut presentations = ApplicationToolPresentationCatalog::default();
        let mut presented = BTreeSet::new();
        for (node, selection) in admitted.profile.definition().application_nodes() {
            let alias = selection.alias();
            if runtime.materialization.scoped_ready() {
                call_owners.insert(node.to_owned(), scopes.graph_call_owner(&graph_path, node)?);
            }
            let participant = if let Some(child) = admitted.children.get(alias) {
                let child_path = if graph_path.is_empty() {
                    node.to_owned()
                } else {
                    format!("{graph_path}/{node}")
                };
                let bound =
                    bind_admitted_saved_pipeline(child, runtime, connector, child_path).await?;
                descendant_guards.extend(bound.guarded_interrupt_kinds);
                if let Some(child_scopes) = bound.runtimes.application_scopes() {
                    scopes.merge(child_scopes.clone())?;
                }
                if presented.insert(alias.to_owned()) {
                    presentations
                        .insert_runtime(
                            alias.to_owned(),
                            alias.to_owned(),
                            "pipeline".to_owned(),
                            "nested-model".to_owned(),
                            bound.presentations,
                            ApplicationToolGuardCatalogs::default(),
                        )
                        .map_err(|_| invalid_pipeline_tool_scope())?;
                }
                NativePipelineApplicationParticipant::Pipeline {
                    definition: Box::new(child.profile.definition().clone()),
                    runtimes: bound.runtimes,
                    events: runtime.node_events.clone(),
                    display_name: alias.to_owned(),
                }
            } else {
                // Bind one exact ordinary descendant to its registry-owned event receiver.
                let (tool, projection, fingerprint) =
                    bind_scoped_agent(&snapshot, admitted.profile.shell(), runtime, alias).await?;
                if presented.insert(alias.to_owned()) {
                    presentations = merge_application_presentations(
                        presentations,
                        projection.presentation_catalog(),
                    )?;
                }
                let scope = scopes.bind_application(
                    &graph_path,
                    node,
                    Arc::new(projection),
                    runtime.node_events.clone(),
                )?;
                descendant_guards.insert("ordinary child continuation");
                NativePipelineApplicationParticipant::ScopedAgent {
                    tool,
                    scope,
                    fingerprint,
                }
            };
            if nodes
                .insert(node.to_owned(), (alias.to_owned(), participant))
                .is_some()
            {
                return Err(invalid_pipeline_tool_scope());
            }
        }
        let application = (!nodes.is_empty()).then(|| {
            Arc::new(NativePipelineApplicationResolver {
                participants: BTreeMap::new(),
                nodes,
                call_owners,
            }) as Arc<dyn PipelineApplicationResolver>
        });
        let mut bound = bind_saved_pipeline_runtimes(
            &admitted.profile,
            snapshot,
            runtime,
            connector,
            application,
        )
        .await?;
        descendant_guards.extend(bound.guarded_interrupt_kinds);
        bound.guarded_interrupt_kinds = descendant_guards.into_iter().collect();
        bound.presentations = presentations;
        bound.runtimes = bound
            .runtimes
            .with_checkpoint_catalog(composition::checkpoint_catalog(admitted)?)
            .with_composition_definitions(composition::composition_definitions(admitted))
            .with_application_scope_path(graph_path)
            .with_application_scopes(Arc::new(scopes));
        Ok(bound)
    })
}

type ScopedAgentBinding = (
    Arc<dyn Tool>,
    ApplicationRuntimeProjection,
    Option<SavedAgentFingerprint>,
);

async fn bind_scoped_agent(
    snapshot: &AdmittedToolSnapshot<'_>,
    shell: &OrdinaryNoToolProfile,
    runtime: &PipelineApplicationRuntime<'_>,
    alias: &str,
) -> Result<ScopedAgentBinding, NativeAgentAssemblyError> {
    let selected = BTreeSet::from([alias.to_owned()]);
    let (materialized, skipped) = materialize_application_runtime(
        snapshot,
        runtime.platform,
        runtime.authority,
        Arc::clone(&runtime.context),
        shell,
        ApplicationToolDependencies::new(
            Arc::clone(&runtime.model_facade),
            Arc::clone(&runtime.tool_policy),
            Arc::clone(&runtime.connector),
            runtime.mcp_tokens,
        )
        .with_model_scopes(runtime.model_scopes.clone())
        .with_code(runtime.code.clone())
        .with_conversation_thread(runtime.conversation_thread_id.clone())
        .with_materialization(runtime.materialization.for_graph_agent()?),
        Some(&selected),
    )
    .await?;
    if !skipped.is_empty() {
        return Err(unsupported_pipeline_runtime());
    }
    let materialized = materialized.ok_or_else(invalid_pipeline_tool_scope)?;
    let fingerprint = materialized
        .tools
        .iter()
        .find(|entry| entry.alias == alias && entry.agent_type == "agent")
        .ok_or_else(invalid_pipeline_tool_scope)?
        .saved_agent_fingerprint;
    let (mut participants, projection) = application_participants(materialized)?;
    if participants.len() != 1 {
        return Err(invalid_pipeline_tool_scope());
    }
    match participants
        .remove(alias)
        .ok_or_else(invalid_pipeline_tool_scope)?
    {
        NativePipelineApplicationParticipant::Agent(tool) => Ok((tool, projection, fingerprint)),
        _ => Err(invalid_pipeline_tool_scope()),
    }
}

fn merge_application_presentations(
    mut target: ApplicationToolPresentationCatalog,
    source: ApplicationToolPresentationCatalog,
) -> Result<ApplicationToolPresentationCatalog, NativeAgentAssemblyError> {
    target
        .merge_exact(source)
        .map_err(|_| invalid_pipeline_tool_scope())?;
    Ok(target)
}

async fn bind_saved_pipeline_runtimes(
    profile: &PipelineExecutionProfile,
    snapshot: AdmittedToolSnapshot<'_>,
    runtime: &PipelineApplicationRuntime<'_>,
    mcp_connector: &dyn McpConnector,
    application: Option<Arc<dyn PipelineApplicationResolver>>,
) -> Result<BoundSavedPipeline, NativeAgentAssemblyError> {
    let aliases = profile.definition().runtime_toolkit_aliases();
    let selected = snapshot.retain_toolkit_names(&aliases);
    let (mut materialized, mut delegated_authorization) =
        materialize_configured_toolsets_with_tokens_and_authorization(
            &selected,
            &runtime.tool_policy,
            runtime.mcp_tokens,
        )
        .await
        .map_err(tool_materialization_error)?;
    let (mut mcp, mcp_delegated_authorization) =
        materialize_mcp_toolsets_with_tokens_and_authorization(
            &selected,
            mcp_connector,
            &runtime.tool_policy,
            runtime.mcp_tokens,
        )
        .await
        .map_err(|error| mcp_materialization_error(&error))?;
    delegated_authorization
        .merge(mcp_delegated_authorization)
        .map_err(|()| unsupported_pipeline_runtime())?;
    materialized.append(&mut mcp);
    let mut toolsets = toolsets_by_alias(materialized)?;
    // `None`, for the reason `bind_nested_pipeline_runtimes` states: a nested
    // pipeline's shell carries no conversation the user toggled anything on for.
    for toolset in profile.shell().internal_tools().toolsets(None) {
        if toolsets
            .insert(toolset.name().to_owned(), toolset)
            .is_some()
        {
            return Err(invalid_pipeline_tool_scope());
        }
    }
    let toolsets = freeze_toolsets_by_alias(toolsets).await?;
    let direct_tool_resolver = build_direct_tool_resolver(profile, &toolsets)?;
    let guarded_interrupt_kinds = guarded_interrupt_kinds(profile, &delegated_authorization);
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
            model_scopes: runtime.model_scopes.clone(),
        }) as Arc<dyn PipelineLlmAgentFactory>
    });
    let mut runtimes = PipelineNodeRuntimes::new(llm_factory, direct_tool_resolver, application)
        .with_events(runtime.node_events.clone());
    if let Some(code) = &runtime.code {
        runtimes = runtimes.with_code(code.clone());
    }
    Ok(BoundSavedPipeline {
        runtimes,
        guarded_interrupt_kinds,
        presentations: ApplicationToolPresentationCatalog::default(),
    })
}

/// The interrupt kinds one nested pipeline can raise besides its `hitl` nodes.
///
/// Read from the ADMITTED definition, never from the run: each of these is a
/// pause with its own resume contract in `graph::resume`, and a caller that
/// implements only the `hitl` one has to know BEFORE the child starts.
pub(super) fn guarded_interrupt_kinds(
    profile: &PipelineExecutionProfile,
    delegated_authorization: &DelegatedAuthorizationCatalog,
) -> Vec<&'static str> {
    let mut kinds = Vec::new();
    if profile.definition().has_static_interrupts() {
        kinds.push("static continuation");
    }
    if !profile.sensitive_llm_tools().is_empty()
        || profile
            .definition()
            .direct_tool_selections()
            .any(|selection| profile.sensitive_direct_tool(selection).is_some())
    {
        kinds.push("sensitive tool approval");
    }
    if profile.shell().internal_tools().ask_user_enabled() {
        kinds.push("clarifying questions");
    }
    if !delegated_authorization.is_empty() {
        kinds.push("MCP authorization");
    }
    kinds
}

// Match the SDK's historical whitespace/underscore spelling only within admitted toolkits.
fn legacy_toolkit_key(value: &str) -> String {
    value
        .chars()
        .filter(|value| !value.is_whitespace() && *value != '_')
        .flat_map(char::to_lowercase)
        .collect()
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
        // Admission already bound the alias to exactly one frozen reference, so
        // a missing toolset is a family this position cannot serve, not scope.
        let toolset = toolsets.get(alias).ok_or_else(unserved_direct_toolkit)?;
        let available = toolset.tools();
        if available.len() > MAX_PIPELINE_MATERIALIZED_TOOLS {
            return Err(invalid_direct_tool_scope());
        }
        let mut by_name = BTreeMap::new();
        for tool in available {
            if by_name
                .insert(tool.name().to_owned(), Arc::clone(tool))
                .is_some()
            {
                return Err(invalid_direct_tool_scope());
            }
        }
        for selection in selections
            .iter()
            .filter(|selection| selection.alias() == alias)
        {
            let tool = by_name
                .get(selection.tool())
                .cloned()
                .ok_or_else(invalid_direct_tool_scope)?;
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
    super::runtime::pipeline_configuration_assembly_error(
        error,
        "the stored pipeline definition could not be admitted",
    )
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

const fn invalid_direct_tool_scope() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidInput,
        "a pipeline direct tool node references a tool outside its frozen scope",
    )
}

const fn unserved_direct_toolkit() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::UnsupportedCapability,
        "a pipeline direct tool node selected a toolkit this runtime cannot serve in this position",
    )
}

const fn unsupported_direct_tool_scope() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::UnsupportedCapability,
        "a pipeline direct tool node selected a tool whose graph authorization is not enabled",
    )
}

const fn unsupported_pipeline_runtime() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::UnsupportedCapability,
        "the native pipeline LLM runtime is not available",
    )
}
