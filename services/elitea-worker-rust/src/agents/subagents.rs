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
//! # Profiles, briefs and the record (#1614)
//!
//! A `spawn_agent` brief is checked against the creator's own view of this
//! turn ([`BriefView`]): an excerpt must quote, exactly, a history entry of
//! a message in the reader's `chat_history_refs`; an attachment must be one
//! of this turn's input attachments (`att-` ids); a memory entry must have
//! been returned by `recall` earlier in the turn. Each spawn and its end go
//! into the execution's sub-agent ledger, which the settlement path reports
//! to Main (`ReportEphemeralAgents`).
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
use elitea_agent_runtime::subagents::profile::{
    BRIEF_ATTACHMENT_NOT_READABLE, BRIEF_EXCERPT_NOT_VISIBLE, BRIEF_MEMORY_NOT_RECALLED,
};
use elitea_agent_runtime::subagents::{
    Brief, CheckedBrief, ChildIdentity, ChildReport, MAX_CHILD_STEPS, SubAgentConfig, SubAgentHost,
    SubAgents,
};
use elitea_agent_runtime::tool_envelope::SubAgentGrant;

use crate::transport::model_facade::{ModelAdapterKind, ModelFacade, ModelInvocation};
use crate::transport::runtime_context::ClaimScopedEliteaContext;

/// Toolsets a child is never offered, by name.
pub(crate) const WITHHELD_TOOLSETS: [&str; 2] = [
    "elitea_nested_applications",
    elitea_agent_runtime::lazy_tools::TOOLSET_NAME,
];

/// What the creator (the turn's root agent) itself sees, for checking a
/// brief: its history by message, this turn's attachments by id, and the
/// memory entries it recalled.
#[derive(Default)]
pub(crate) struct BriefView {
    /// Each history entry's message id and text, in order.
    pub(crate) history: Vec<(String, String)>,
    /// This turn's input attachments: id and display name.
    pub(crate) attachments: Vec<(String, String)>,
    /// What `recall` returned this turn; `None` without agent memory.
    pub(crate) recalled: Option<Arc<super::agent_memory_tools::RecalledEntries>>,
}

impl BriefView {
    /// The view of this turn.
    pub(crate) fn new(
        history: Vec<(String, String)>,
        attachments: &[super::attachments::AttachmentReference],
        recalled: Option<Arc<super::agent_memory_tools::RecalledEntries>>,
    ) -> Self {
        Self {
            history,
            attachments: attachments
                .iter()
                .map(|reference| {
                    (
                        super::attachment_context::attachment_id(reference),
                        super::attachment_context::display_name(reference.name()),
                    )
                })
                .collect(),
            recalled,
        }
    }

    /// ADR-0050 decision 9d: every reference of `brief` is in this view.
    pub(crate) fn check(&self, brief: &Brief) -> Result<CheckedBrief, &'static str> {
        for excerpt in &brief.excerpts {
            let quoted = self.history.iter().any(|(message_id, text)| {
                *message_id == excerpt.message_id && text.contains(excerpt.quote.as_str())
            });
            if !quoted {
                return Err(BRIEF_EXCERPT_NOT_VISIBLE);
            }
        }
        let attachments = brief
            .attachments
            .iter()
            .map(|id| {
                self.attachments
                    .iter()
                    .find(|(known, _)| known == id)
                    .map(|(id, name)| (id.clone(), format!("the file {name} of this turn")))
                    .ok_or(BRIEF_ATTACHMENT_NOT_READABLE)
            })
            .collect::<Result<Vec<_>, _>>()?;
        let memory_entries = brief
            .memory_entries
            .iter()
            .map(|id| {
                self.recalled
                    .as_ref()
                    .and_then(|recalled| recalled.content(id))
                    .map(|content| (id.clone(), content))
                    .ok_or(BRIEF_MEMORY_NOT_RECALLED)
            })
            .collect::<Result<Vec<_>, _>>()?;
        Ok(CheckedBrief {
            attachments,
            memory_entries,
        })
    }
}

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
    /// What the creator sees, for its briefs (#1614).
    pub(crate) view: BriefView,
    /// The execution's sub-agent ledger, reported before settlement.
    pub(crate) ledger: Arc<crate::protocol::ephemeral_agents::EphemeralAgentLedger>,
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

#[cfg(test)]
#[path = "subagents_tests.rs"]
mod tests;

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

    fn check_brief(
        &self,
        _child: &ChildIdentity,
        brief: &Brief,
    ) -> Result<CheckedBrief, &'static str> {
        self.view.check(brief)
    }

    fn started(&self, child: &ChildIdentity) {
        self.ledger
            .started(child, crate::protocol::run_steps::now_unix_ms());
    }

    fn resumed(&self, child: &ChildIdentity) {
        self.ledger.resumed(child);
    }

    fn finished(&self, child: &ChildIdentity, report: &ChildReport) {
        self.ledger
            .finished(child, report, crate::protocol::run_steps::now_unix_ms());
    }
}

/// The four tools, over `host`; a child is never offered a tool in `excluded`.
/// A cloud child's prompt is the runtime's preamble alone: its task is the
/// whole of what it is told.
/// Every child counts one level of the run's delegation depth from
/// `delegation` (ADR-0044 decision 3).
/// `grant` is the agent's `subagent_tools` (ADR-0050 decision 3): a child
/// is offered the tools it names, writes included; without them, the
/// read-only tools.
pub(crate) fn toolset(
    host: WorkerSubAgents,
    excluded: Vec<String>,
    delegation: DelegationScope,
    grant: SubAgentGrant,
) -> Arc<dyn Toolset> {
    let config = SubAgentConfig::new("")
        .with_excluded_tools(excluded)
        .with_delegation(delegation)
        .with_grant(grant);
    SubAgents::new(Arc::new(host), config).toolset()
}
