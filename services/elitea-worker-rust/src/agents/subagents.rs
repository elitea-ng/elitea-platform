//! The runtime's sub-agents on a cloud turn (#1226): this worker's
//! [`SubAgentHost`].
//!
//! Bound when the agent version or the conversation enables `subagents`,
//! and on every cloud-executor workspace turn (ADR-0034), the same four
//! tools the desktop offers. A child calls the model facade under the
//! turn's own claim context (the same execution, so the spend is the turn's
//! and the gateway's budget covers it), with the agent's model and controls
//! and its own system prompt. It is offered the turn's bound toolsets, which
//! the runtime filters to the read-only tools; this host also leaves out the
//! nested applications (one depth budget: a sub-agent never runs an
//! application child), Smart Tools Selection's meta-tools and every tool
//! that needs a confirmation or a delegated authorization (a child has no
//! way to ask).
//!
//! A child's own tool calls are not projected to the transcript on this
//! host: the parent's `spawn_agent` and `wait_agent` calls and their results
//! are. Children stop with the run (the toolset that owns them is dropped
//! with the agent), so a turn that pauses for a person and resumes under a
//! new claim starts with none.

use std::sync::{Arc, OnceLock};

use adk_rust::{GenerateContentConfig, Llm, Toolset};
use async_trait::async_trait;
use elitea_agent_runtime::delegation::DelegationScope;
use elitea_agent_runtime::host::{HostError, HostErrorCode};
use elitea_agent_runtime::subagents::{
    ChildIdentity, MAX_CHILD_STEPS, SubAgentConfig, SubAgentHost, SubAgents,
};

use crate::transport::model_facade::{ModelAdapterKind, ModelFacade, ModelInvocation};
use crate::transport::runtime_context::ClaimScopedEliteaContext;

/// Toolsets a child is never offered, by name.
pub(crate) const WITHHELD_TOOLSETS: [&str; 2] = [
    "elitea_nested_applications",
    elitea_agent_runtime::lazy_tools::TOOLSET_NAME,
];

pub(crate) struct WorkerSubAgents {
    pub(crate) facade: Arc<ModelFacade>,
    pub(crate) context: Arc<ClaimScopedEliteaContext>,
    pub(crate) adapter: ModelAdapterKind,
    pub(crate) model_project_id: u32,
    /// The agent's own invocation; a child replaces its system prompt and
    /// its turns.
    pub(crate) invocation: ModelInvocation,
    pub(crate) toolsets: Vec<Arc<dyn Toolset>>,
    /// The generation config the facade admits, from the first binding.
    pub(crate) config: OnceLock<GenerateContentConfig>,
}

impl WorkerSubAgents {
    /// The turn's toolsets a child may be offered (before the runtime's
    /// read-only filter).
    pub(crate) fn child_toolsets(toolsets: &[Arc<dyn Toolset>]) -> Vec<Arc<dyn Toolset>> {
        toolsets
            .iter()
            .filter(|toolset| !WITHHELD_TOOLSETS.contains(&toolset.name()))
            .cloned()
            .collect()
    }
}

#[async_trait]
impl SubAgentHost for WorkerSubAgents {
    fn bind_model(
        &self,
        _child: &ChildIdentity,
        system_instruction: &str,
    ) -> Result<Arc<dyn Llm>, HostError> {
        let invocation = ModelInvocation {
            system_instruction: system_instruction.to_owned(),
            max_model_turns: MAX_CHILD_STEPS,
            ..self.invocation.clone()
        };
        self.facade
            .bind(
                self.adapter,
                self.context.as_ref(),
                self.model_project_id,
                invocation,
            )
            .map(|bound| {
                let chat = bound.admitted_chat();
                let _ = self.config.set(chat.config);
                chat.model
            })
            .map_err(|_| {
                HostError::new(
                    HostErrorCode::DependencyUnavailable,
                    "the sub-agent's model could not be bound",
                )
            })
    }

    async fn toolsets(&self, _child: &ChildIdentity) -> Result<Vec<Arc<dyn Toolset>>, HostError> {
        Ok(self.toolsets.clone())
    }

    /// The facade admits only the config its binding fixes, the one it
    /// answered the first binding with: every child shares the agent's
    /// controls, so it is the same for each.
    fn generation_config(&self) -> Option<GenerateContentConfig> {
        self.config.get().cloned()
    }
}

/// The four tools, over `host`; a child is never offered a tool in `excluded`.
/// A cloud child's prompt is the runtime's preamble alone: its task is the
/// whole of what it is told.
/// Every child counts one level of the run's delegation depth from
/// `delegation` (ADR-0044 decision 3).
pub(crate) fn toolset(
    host: WorkerSubAgents,
    excluded: Vec<String>,
    delegation: DelegationScope,
) -> Arc<dyn Toolset> {
    SubAgents::new(
        Arc::new(host),
        SubAgentConfig::new("")
            .with_excluded_tools(excluded)
            .with_delegation(delegation),
    )
    .toolset()
}
