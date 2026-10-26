//! The `subagents` internal tool (#1226): child agents with their own fresh
//! context, a read-only toolset and a token budget, several at once, that a
//! parent agent starts, talks to, waits for and closes.
//!
//! # The tool surface
//!
//! Four tools, after the Codex multi-agent design (`openai/codex`,
//! `codex-rs/core/src/tools/handlers/multi_agents*.rs`, Apache-2.0; the tool
//! names, arguments and lifecycle only, no code): [`SPAWN_AGENT`] starts a
//! child and returns at once, [`SEND_INPUT`] gives a child another message
//! (it continues on its own session), [`WAIT_AGENT`] waits for one or more
//! children and [`CLOSE_AGENT`] stops one. Codex's `resume_agent` is not
//! offered: a child never outlives the parent's turn.
//!
//! # What a child is
//!
//! An adk `LlmAgent` of its own, on a fresh in-memory session seeded with the
//! parent-authored `task` only: it sees none of the parent's history. Its
//! system instruction is [`CHILD_PREAMBLE`] followed by the host's sections
//! (the environment, AGENTS.md). It runs as its own tokio task, so children
//! run concurrently whatever the parent's tool strategy, and stops at its
//! step limit or its input-token budget ([`ChildStatus::BudgetExhausted`],
//! with the text it had). Its result reaches the parent as tool output,
//! bounded to [`MAX_RESULT_BYTES`].
//!
//! # What a child may call
//!
//! Only read-only tools: every tool the host offers a child passes the
//! runtime's filter ([`ChildToolset`]), which drops any tool whose
//! `is_read_only()` is false, the four sub-agent tools themselves (one level
//! deep: a child never spawns children) and `exit_plan_mode`. A static list
//! with a writing tool in it is refused at construction
//! ([`ChildTools::new`]).
//!
//! A host that can give a child a checkout of its own
//! ([`SubAgentHost::supports_worktree_isolation`]: the desktop, a linked git
//! worktree of the folder, #1235) offers `spawn_agent`'s `isolation:
//! "worktree"`. Such a child gets the host's [`SubAgentHost::isolated_toolsets`]
//! (the same filter without the read-only rule: it may write, in its own
//! checkout only) and [`ISOLATED_CHILD_PREAMBLE`]. Elsewhere the argument is
//! not in the schema and is refused.
//!
//! # What the host supplies
//!
//! A [`SubAgentHost`]: the model a child calls (the parent's binding, so the
//! spend is the parent turn's and the gateway budget covers it), the tools
//! the child is offered before the filter, and lifecycle notices for the
//! host's UI and transcript. The desktop and the cloud worker each implement
//! it; the runtime owns the lifecycle, the limits and the filter.
//!
//! # Limits
//!
//! At most [`MAX_CONCURRENT_CHILDREN`] children run at once and
//! [`MAX_CHILDREN_PER_TURN`] start in one parent run; a host may lower them
//! ([`SubAgentLimits::capped`]), never raise them. They are held to the
//! fan-out budget the graph's Parallel and Map nodes use
//! ([`crate::graph::fanout_budget`]). Dropping the [`SubAgents`] (the turn
//! ended, was cancelled or handed off) stops every child it started.

mod child;
mod tools;

#[cfg(test)]
mod tests;

use std::collections::BTreeSet;
use std::sync::{Arc, Mutex};

use adk_core::{Event, Llm, Tool, Toolset};
use async_trait::async_trait;
use serde_json::{Value, json};

use crate::graph::fanout_budget;
use crate::host::HostError;

pub use child::{ChildTools, ChildToolset, WritableTool};

/// The internal-tool name an agent version or conversation enables.
pub const INTERNAL_TOOL_NAME: &str = "subagents";
/// The reserved toolset name: no configured toolkit may take it, so none can
/// shadow [`SPAWN_AGENT`].
pub const TOOLSET_NAME: &str = "elitea_subagents";

pub const SPAWN_AGENT: &str = "spawn_agent";
pub const SEND_INPUT: &str = "send_input";
pub const WAIT_AGENT: &str = "wait_agent";
pub const CLOSE_AGENT: &str = "close_agent";
/// The four tools, in the order the toolset offers them.
pub const TOOL_NAMES: [&str; 4] = [SPAWN_AGENT, SEND_INPUT, WAIT_AGENT, CLOSE_AGENT];

/// The most children running at once.
pub const MAX_CONCURRENT_CHILDREN: usize = 4;
/// The most children one parent run starts.
pub const MAX_CHILDREN_PER_TURN: usize = 12;
/// A child's model steps when `spawn_agent` names none.
pub const DEFAULT_CHILD_STEPS: u32 = 30;
/// The most model steps `spawn_agent` may ask for.
pub const MAX_CHILD_STEPS: u32 = 60;
/// A child's cumulative input tokens before it is stopped.
pub const DEFAULT_INPUT_TOKEN_BUDGET: u64 = 60_000;
/// The largest input-token budget a host may give a child.
pub const MAX_INPUT_TOKEN_BUDGET: u64 = 200_000;
/// The longest `task` or `send_input` message, in characters.
pub const MAX_TASK_CHARS: usize = 8_000;
/// The longest label, in characters (a longer one is cut).
pub const MAX_LABEL_CHARS: usize = 60;
/// The most children one `wait_agent` names.
pub const MAX_WAIT_IDS: usize = MAX_CONCURRENT_CHILDREN;
/// How long `wait_agent` waits when it names no timeout, in seconds.
pub const DEFAULT_WAIT_SECONDS: u64 = 300;
/// The longest `wait_agent` timeout, in seconds.
pub const MAX_WAIT_SECONDS: u64 = 900;
/// A child's result as the parent reads it, in bytes.
pub const MAX_RESULT_BYTES: usize = 8 * 1024;
/// Every child agent's name starts so: a sub-agent tool called from a
/// child's context is refused ([`DEPTH_EXCEEDED`]).
pub const CHILD_AGENT_PREFIX: &str = "subagent_";

/// The refusal of a sub-agent tool called from a child.
pub const DEPTH_EXCEEDED: &str = "subagent_depth_exceeded";

// The parallel cap is a fan-out like the graph's: held to the same budget.
const _: () = assert!(MAX_CONCURRENT_CHILDREN <= fanout_budget::MAX_PARALLEL_BRANCHES);
const _: () = assert!(MAX_CHILDREN_PER_TURN <= fanout_budget::MAX_FANOUT_CHILDREN);
const _: () = assert!(MAX_CONCURRENT_CHILDREN <= MAX_CHILDREN_PER_TURN);
const _: () = assert!(DEFAULT_CHILD_STEPS <= MAX_CHILD_STEPS);
const _: () = assert!(DEFAULT_INPUT_TOKEN_BUDGET <= MAX_INPUT_TOKEN_BUDGET);

/// What a child is told before the host's sections.
pub const CHILD_PREAMBLE: &str = "You are a read-only sub-agent working for another agent. \
You see only the task below, not that agent's conversation. Use your tools to investigate \
(you cannot change files, run commands or start other agents), then answer with a concise, \
self-contained report of what you found: the facts, the file paths and symbols that matter, \
and anything you could not determine. The other agent reads only your final answer.";

/// What a child with a checkout of its own is told before the host's
/// sections.
pub const ISOLATED_CHILD_PREAMBLE: &str = "You are a sub-agent working for another agent, in a \
separate checkout of the repository (a git worktree of its own): what you change there does not \
touch the other agent's files. You see only the task below, not that agent's conversation. Do \
the task with your tools, then answer with a concise, self-contained report: what you changed \
(the files and why), how you checked it, and anything left undone. The other agent reads only \
your final answer; the person reviews your checkout's changes and merges them.";

/// The limits one parent run's children are held to.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SubAgentLimits {
    pub max_concurrent: usize,
    pub max_per_turn: usize,
    /// Model steps when `spawn_agent` names none.
    pub default_steps: u32,
    /// Cumulative input tokens of one child.
    pub input_token_budget: u64,
}

impl Default for SubAgentLimits {
    fn default() -> Self {
        Self {
            max_concurrent: MAX_CONCURRENT_CHILDREN,
            max_per_turn: MAX_CHILDREN_PER_TURN,
            default_steps: DEFAULT_CHILD_STEPS,
            input_token_budget: DEFAULT_INPUT_TOKEN_BUDGET,
        }
    }
}

impl SubAgentLimits {
    /// These limits within the runtime's own: a host may lower them, never
    /// raise them, and none is zero.
    #[must_use]
    pub fn capped(self) -> Self {
        Self {
            max_concurrent: self.max_concurrent.clamp(1, MAX_CONCURRENT_CHILDREN),
            max_per_turn: self.max_per_turn.clamp(1, MAX_CHILDREN_PER_TURN),
            default_steps: self.default_steps.clamp(1, MAX_CHILD_STEPS),
            input_token_budget: self.input_token_budget.clamp(1, MAX_INPUT_TOKEN_BUDGET),
        }
    }
}

/// One child, as the host's notices name it.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ChildIdentity {
    /// `a_` and 8 hex digits, unique in the parent run.
    pub agent_id: String,
    /// The `spawn_agent` call that started it: the hierarchy key its own
    /// tool calls are recorded under (`parent_agent_call_id`).
    pub parent_call_id: String,
    pub label: Option<String>,
    /// 1 for the parent run's first child, then in spawn order
    /// (`sibling_ordinal`).
    pub ordinal: usize,
}

impl ChildIdentity {
    /// The child agent's name: [`CHILD_AGENT_PREFIX`] and its id.
    #[must_use]
    pub fn agent_name(&self) -> String {
        format!("{CHILD_AGENT_PREFIX}{}", self.agent_id)
    }
}

/// Where a child stands.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ChildStatus {
    Running,
    /// It answered.
    Completed,
    /// Its run failed (the model, or a tool, ended it with an error).
    Failed,
    /// It used its input-token budget or its steps: the result is what it
    /// had then.
    BudgetExhausted,
    /// Closed, or stopped with the parent's turn.
    Cancelled,
}

impl ChildStatus {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Running => "running",
            Self::Completed => "completed",
            Self::Failed => "failed",
            Self::BudgetExhausted => "budget_exhausted",
            Self::Cancelled => "cancelled",
        }
    }

    /// Whether the child is done (it may still be continued when
    /// [`ChildStatus::Completed`]).
    #[must_use]
    pub const fn is_settled(self) -> bool {
        !matches!(self, Self::Running)
    }
}

/// What a child has done so far.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ChildReport {
    pub agent_id: String,
    pub status: ChildStatus,
    /// Its final (or, stopped early, its last) text, at most
    /// [`MAX_RESULT_BYTES`].
    pub result: Option<String>,
    /// Model steps taken.
    pub steps: u32,
    pub input_tokens: u64,
    pub output_tokens: u64,
    /// A stable code when it failed or was stopped.
    pub error: Option<&'static str>,
}

impl ChildReport {
    /// The report as the tools answer it.
    #[must_use]
    pub fn to_value(&self) -> Value {
        let mut value = json!({
            "agent_id": self.agent_id,
            "status": self.status.as_str(),
            "steps": self.steps,
            "input_tokens": self.input_tokens,
            "output_tokens": self.output_tokens,
        });
        if let Some(result) = &self.result {
            value["result"] = json!(result);
        }
        if let Some(error) = self.error {
            value["error"] = json!(error);
        }
        value
    }
}

/// What the runtime needs from the host to run children.
#[async_trait]
pub trait SubAgentHost: Send + Sync {
    /// The model one child calls, with `system_instruction` as its system
    /// prompt: the parent turn's binding (the same execution, so the spend
    /// is the turn's and the gateway's budget gate covers it). A host that
    /// streams the parent's text binds it quiet.
    ///
    /// # Errors
    ///
    /// When no model can be bound: the spawn is refused.
    fn bind_model(
        &self,
        child: &ChildIdentity,
        system_instruction: &str,
    ) -> Result<Arc<dyn Llm>, HostError>;

    /// The tools offered to one child, before the runtime's read-only filter
    /// ([`ChildToolset`]). A host wraps them for its own recording here (the
    /// child's calls go to the transcript under [`ChildIdentity`]).
    ///
    /// # Errors
    ///
    /// When the tools cannot be prepared: the spawn is refused.
    async fn toolsets(&self, child: &ChildIdentity) -> Result<Vec<Arc<dyn Toolset>>, HostError>;

    /// Whether a child may ask for a checkout of its own (`isolation:
    /// "worktree"`); `false` keeps the argument out of `spawn_agent`.
    fn supports_worktree_isolation(&self) -> bool {
        false
    }

    /// The tools offered to a child with a checkout of its own (made by the
    /// host for it), before the runtime's filter, which keeps writing tools
    /// for it. Only called when [`Self::supports_worktree_isolation`].
    ///
    /// # Errors
    ///
    /// When no checkout can be made: the spawn is refused.
    async fn isolated_toolsets(
        &self,
        _child: &ChildIdentity,
    ) -> Result<Vec<Arc<dyn Toolset>>, HostError> {
        Err(HostError::new(
            crate::host::HostErrorCode::InvalidConfiguration,
            "this host gives sub-agents no checkout of their own",
        ))
    }

    /// The generation config a child's requests carry, for a model that
    /// admits only the config it was bound with; `None` leaves the default.
    fn generation_config(&self) -> Option<adk_core::GenerateContentConfig> {
        None
    }

    /// The child was started.
    fn started(&self, _child: &ChildIdentity) {}

    /// One of the child's adk events, in its stream's order.
    fn event(&self, _child: &ChildIdentity, _event: &Event) {}

    /// The child settled (answered, failed, ran out, or was stopped). Once
    /// per run of the child: a `send_input` that continues it settles again.
    fn finished(&self, _child: &ChildIdentity, _report: &ChildReport) {}
}

/// How one parent run's children are made.
pub struct SubAgentConfig {
    /// The host's sections of a child's system prompt (the environment,
    /// AGENTS.md), after [`CHILD_PREAMBLE`]. Never the parent's history.
    pub instruction: String,
    pub limits: SubAgentLimits,
    /// Tools a child is never offered beyond the runtime's own exclusions,
    /// by name (the cloud's tools that need a confirmation or a delegated
    /// authorization: a child has no way to ask).
    pub excluded_tools: BTreeSet<String>,
}

impl SubAgentConfig {
    #[must_use]
    pub fn new(instruction: impl Into<String>) -> Self {
        Self {
            instruction: instruction.into(),
            limits: SubAgentLimits::default(),
            excluded_tools: BTreeSet::new(),
        }
    }

    #[must_use]
    pub fn with_limits(mut self, limits: SubAgentLimits) -> Self {
        self.limits = limits;
        self
    }

    #[must_use]
    pub fn with_excluded_tools(mut self, names: impl IntoIterator<Item = String>) -> Self {
        self.excluded_tools.extend(names);
        self
    }

    /// A child's whole system instruction, after `preamble`.
    fn system_instruction(&self, preamble: &str) -> String {
        if self.instruction.trim().is_empty() {
            preamble.to_owned()
        } else {
            format!("{preamble}\n\n{}", self.instruction)
        }
    }
}

/// One parent run's children: the four tools over one registry. Clones share
/// it; dropping the last one stops every child still running.
#[derive(Clone)]
pub struct SubAgents {
    inner: Arc<Registry>,
}

pub(crate) struct Registry {
    host: Arc<dyn SubAgentHost>,
    instruction: String,
    /// A child with a checkout of its own: [`ISOLATED_CHILD_PREAMBLE`] and
    /// the host's sections.
    isolated_instruction: String,
    limits: SubAgentLimits,
    excluded: BTreeSet<String>,
    children: Mutex<Vec<child::Child>>,
}

impl SubAgents {
    #[must_use]
    pub fn new(host: Arc<dyn SubAgentHost>, config: SubAgentConfig) -> Self {
        let instruction = config.system_instruction(CHILD_PREAMBLE);
        let isolated_instruction = config.system_instruction(ISOLATED_CHILD_PREAMBLE);
        Self {
            inner: Arc::new(Registry {
                host,
                instruction,
                isolated_instruction,
                limits: config.limits.capped(),
                excluded: config.excluded_tools,
                children: Mutex::new(Vec::new()),
            }),
        }
    }

    /// The four tools as one toolset, named [`TOOLSET_NAME`].
    #[must_use]
    pub fn toolset(&self) -> Arc<dyn Toolset> {
        let tools: Vec<Arc<dyn Tool>> = tools::Action::ALL
            .into_iter()
            .map(|action| tools::tool(action, Arc::clone(&self.inner)))
            .collect();
        Arc::new(adk_tool::BasicToolset::new(TOOLSET_NAME, tools))
    }

    /// Stop every child still running (the turn ended, was cancelled, or is
    /// being handed off). Each one stopped settles as
    /// [`ChildStatus::Cancelled`], and the host is told
    /// ([`SubAgentHost::finished`]); a drop stops them without telling it.
    pub fn close_all(&self) {
        self.inner.close_all();
    }

    /// Every child's report, in spawn order.
    #[must_use]
    pub fn reports(&self) -> Vec<ChildReport> {
        self.inner
            .children
            .lock()
            .map(|children| children.iter().map(child::Child::report).collect())
            .unwrap_or_default()
    }
}

impl Registry {
    fn close_all(&self) {
        let children = self
            .children
            .lock()
            .map(|children| children.clone())
            .unwrap_or_default();
        for child in children {
            if let Some(report) = child.close() {
                self.host.finished(child.identity(), &report);
            }
        }
    }
}

impl Drop for Registry {
    fn drop(&mut self) {
        // No child outlives the run that started it.
        if let Ok(children) = self.children.get_mut() {
            for child in children.iter() {
                child.close();
            }
        }
    }
}
