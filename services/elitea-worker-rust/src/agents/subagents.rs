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
//! turn ([`BriefView`]): an excerpt must quote, exactly (each run of
//! whitespace read as one space), the text the model reads of a message it
//! was shown with a `[message m-...]` line (#1831), by that short id: a
//! user-role history entry of the reader's `chat_history_refs`, or this
//! turn's question; an attachment must be one
//! of this turn's input attachments (`att-` ids); a memory entry must have
//! been returned by `recall` earlier in the turn. Each spawn and its end go
//! into the execution's sub-agent ledger, which the settlement path reports
//! to Main (`ReportEphemeralAgents`).
//!
//! # The live lane (#1823)
//!
//! Each child's start (with its display name and avatar key), its own tool
//! calls and results, its text and its end also go to the ledger's
//! [`super::subagent_lane::SubAgentLane`], which the run's event loop
//! projects as `agent_subagent` frames on the run's own output stream: the
//! lane a client shows under the `spawn_agent` row. They are not trace steps
//! of the parent: the parent's `spawn_agent` and `wait_agent` calls and
//! their results are. Children stop with the run (the toolset that owns them is dropped
//! with the agent), so a turn that pauses for a person and resumes under a
//! new claim starts with none.
//!
//! # Depth and model (ADR-0050 decision 4, #1824, #1825)
//!
//! The run's frozen sub-agent policy sets the depth: with 2 a child gets
//! the four tools too, inside the run's budget, and its children never do.
//! A `profile.model` must be one of the policy's models, which Main froze
//! from the project catalogue the agent's own model came from; the child
//! then calls that model with its limits, adapter and thinking control,
//! under the same claim (so the same budget gate and egress). Any other
//! name is refused with `SUBAGENT_MODEL_NOT_ALLOWED`.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use adk_rust::{GenerateContentConfig, Llm, Toolset};
use async_trait::async_trait;
use elitea_agent_runtime::delegation::DelegationScope;
use elitea_agent_runtime::host::{HostError, HostErrorCode};
use elitea_agent_runtime::subagents::message_ref::normalize_whitespace;
use elitea_agent_runtime::subagents::profile::{
    BRIEF_ATTACHMENT_NOT_READABLE, BRIEF_EXCERPT_NOT_VISIBLE, BRIEF_MEMORY_NOT_RECALLED,
};
use elitea_agent_runtime::subagents::{
    Brief, CheckedBrief, ChildIdentity, ChildReport, MAX_CHILD_STEPS, SubAgentConfig, SubAgentHost,
    SubAgents,
};
use elitea_agent_runtime::tool_envelope::SubAgentGrant;

use super::assembly::{OrdinaryModelProvider, SummaryModelProfile};
use crate::transport::model_facade::{ModelAdapterKind, ModelFacade, ModelInvocation};
use crate::transport::runtime_context::ClaimScopedEliteaContext;

/// Toolsets a child is never offered, by name.
pub(crate) const WITHHELD_TOOLSETS: [&str; 2] = [
    "elitea_nested_applications",
    elitea_agent_runtime::lazy_tools::TOOLSET_NAME,
];

/// One message a brief's excerpt may quote (#1831).
pub(crate) struct QuotableMessage {
    /// The short id the model reads on its `[message m-...]` line.
    pub(crate) short_id: String,
    /// The chat message uuid it stands for, for the record.
    pub(crate) message_id: String,
    /// The text the model reads after the id line, whitespace-normalized.
    pub(crate) text: String,
}

/// What the creator (the turn's root agent) itself sees, for checking a
/// brief: its history by message, this turn's attachments by id, and the
/// memory entries it recalled.
#[derive(Default)]
pub(crate) struct BriefView {
    /// Each quotable message, in order
    /// (`OrdinaryNativeAgentPlan::tag_message_refs`).
    pub(crate) history: Vec<QuotableMessage>,
    /// This turn's input attachments: id and display name.
    pub(crate) attachments: Vec<(String, String)>,
    /// The memory entries given this turn: what Main recalled into the
    /// instructions (#1826) and what `recall` returned, less what `forget`
    /// deleted; `None` without agent memory.
    pub(crate) recalled: Option<Arc<super::agent_memory_tools::RecalledEntries>>,
}

impl BriefView {
    /// The view of this turn. `history` is each quotable message's short
    /// id, chat message uuid and text as the model reads it.
    pub(crate) fn new(
        history: Vec<(String, String, String)>,
        attachments: &[super::attachments::AttachmentReference],
        recalled: Option<Arc<super::agent_memory_tools::RecalledEntries>>,
    ) -> Self {
        Self {
            history: history
                .into_iter()
                .map(|(short_id, message_id, text)| QuotableMessage {
                    short_id,
                    message_id,
                    text: normalize_whitespace(&text),
                })
                .collect(),
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
        let mut message_ids: Vec<(String, String)> = Vec::new();
        for excerpt in &brief.excerpts {
            // `spawn_agent` admits no empty or blank quote.
            let quote = normalize_whitespace(&excerpt.quote);
            let message = self
                .history
                .iter()
                .find(|message| {
                    message.short_id == excerpt.message_id && message.text.contains(quote.as_str())
                })
                .ok_or(BRIEF_EXCERPT_NOT_VISIBLE)?;
            if !message_ids
                .iter()
                .any(|(short, _)| *short == message.short_id)
            {
                message_ids.push((message.short_id.clone(), message.message_id.clone()));
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
            message_ids,
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
    /// The other catalogue models a child may name (#1825), admitted from
    /// the run's sub-agent policy.
    pub(crate) models: Arc<Vec<SummaryModelProfile>>,
    /// The generation config the facade admits for each model (`None`: the
    /// agent's own), from that model's first binding.
    pub(crate) configs: Mutex<HashMap<Option<String>, GenerateContentConfig>>,
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
    /// #1825: `model` is `None` for the agent's own model, or a name the
    /// run's sub-agent policy admitted (Main froze it from the project
    /// catalogue, with its limits); any other name is refused.
    fn bind_model(
        &self,
        _child: &ChildIdentity,
        system_instruction: &str,
        model: Option<&str>,
    ) -> Result<Arc<dyn Llm>, HostError> {
        let (adapter, model_project_id, invocation) = child_invocation(
            &self.invocation,
            self.adapter,
            self.model_project_id,
            &self.models,
            model,
        )
        .ok_or_else(|| {
            HostError::new(
                HostErrorCode::AuthorizationFailed,
                "the sub-agent's model is not one this run may use",
            )
        })?;
        let invocation = ModelInvocation {
            system_instruction: system_instruction.to_owned(),
            max_model_turns: MAX_CHILD_STEPS,
            ..invocation
        };
        let bound = self
            .facade
            .bind(adapter, self.context.as_ref(), model_project_id, invocation)
            .map_err(|_| {
                HostError::new(
                    HostErrorCode::DependencyUnavailable,
                    "the sub-agent's model could not be bound",
                )
            })?;
        let chat = bound.admitted_chat();
        if let Ok(mut configs) = self.configs.lock() {
            configs
                .entry(model.map(str::to_owned))
                .or_insert(chat.config);
        }
        Ok(chat.model)
    }

    async fn toolsets(&self, _child: &ChildIdentity) -> Result<Vec<Arc<dyn Toolset>>, HostError> {
        Ok(self.toolsets.clone())
    }

    /// The facade admits only the config its binding fixes, the one it
    /// answered the first binding of that model with: every child on one
    /// model shares its controls, so it is the same for each.
    fn generation_config(&self, model: Option<&str>) -> Option<GenerateContentConfig> {
        self.configs
            .lock()
            .ok()
            .and_then(|configs| configs.get(&model.map(str::to_owned)).cloned())
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
        self.ledger.lane().started(child);
    }

    fn resumed(&self, child: &ChildIdentity) {
        self.ledger.resumed(child);
        self.ledger.lane().resumed(child);
    }

    fn event(&self, child: &ChildIdentity, event: &adk_rust::Event) {
        self.ledger.lane().event(child, event);
    }

    fn finished(&self, child: &ChildIdentity, report: &ChildReport) {
        self.ledger
            .finished(child, report, crate::protocol::run_steps::now_unix_ms());
        self.ledger.lane().finished(child, report);
    }
}

/// What a child's model call binds (#1825): the agent's own model for
/// `None` or the agent's own name, else the named model of `models` with
/// its adapter, project, output cap, temperature, limits and thinking
/// control. The agent's turn caps and preamble stay; its response schema
/// does not (a child answers its parent in text). `None` when `models`
/// does not name it.
pub(crate) fn child_invocation(
    agent: &ModelInvocation,
    adapter: ModelAdapterKind,
    model_project_id: u32,
    models: &[SummaryModelProfile],
    model: Option<&str>,
) -> Option<(ModelAdapterKind, u32, ModelInvocation)> {
    let Some(name) = model.filter(|name| *name != agent.model_name) else {
        return Some((adapter, model_project_id, agent.clone()));
    };
    let named = models
        .iter()
        .find(|candidate| candidate.model_name == name)?;
    let adapter = match named.model_provider {
        OrdinaryModelProvider::OpenAiChat => ModelAdapterKind::OpenAiCompatible,
        OrdinaryModelProvider::NativeAnthropic => ModelAdapterKind::Anthropic,
    };
    Some((
        adapter,
        named.model_project_id,
        ModelInvocation {
            response_schema: None,
            allow_text_continuation: false,
            context_budget: Some(named.context_budget),
            model_name: named.model_name.clone(),
            max_tokens: Some(named.max_tokens),
            // The agent's thinking setting applies only where the named
            // model declares how its thinking is controlled.
            reasoning_effort: agent
                .reasoning_effort
                .filter(|_| named.model_reasoning.is_some()),
            reasoning: named.model_reasoning,
            temperature: named.temperature,
            ..agent.clone()
        },
    ))
}

/// The four tools, over `host`; a child is never offered a tool in `excluded`.
/// A cloud child's prompt is the runtime's preamble alone: its task is the
/// whole of what it is told.
/// Every child counts one level of the run's delegation depth from
/// `delegation` (ADR-0044 decision 3).
/// `grant` is the agent's `subagent_tools` (ADR-0050 decision 3): a child
/// is offered the tools it names, writes included; without them, the
/// read-only tools.
/// `max_depth` is the run's frozen `subagents_max_depth` (#1824): with 2 a
/// child may start children of its own, inside the same budget.
pub(crate) fn toolset(
    host: WorkerSubAgents,
    excluded: Vec<String>,
    delegation: DelegationScope,
    grant: SubAgentGrant,
    max_depth: u32,
) -> Arc<dyn Toolset> {
    let config = SubAgentConfig::new("")
        .with_excluded_tools(excluded)
        .with_delegation(delegation)
        .with_grant(grant)
        .with_max_depth(max_depth);
    SubAgents::new(Arc::new(host), config).toolset()
}
