//! The four sub-agent tools. Every refusal is a result the model reads
//! (`{"status": "error", "code", "message"}`), never a run failure.

use std::sync::Arc;
use std::time::Duration;

use adk_core::{Tool, ToolContext};
use async_trait::async_trait;
use ring::rand::{SecureRandom as _, SystemRandom};
use serde_json::{Value, json};

use super::child::{self, Child, InputRefused};
use super::profile::{
    self, BRIEF_ATTACHMENT_NOT_READABLE, BRIEF_EXCERPT_NOT_VISIBLE, BRIEF_MEMORY_NOT_RECALLED,
    Brief, MAX_BRIEF_ITEMS, MAX_BRIEF_TEXT_CHARS, MAX_DISPLAY_NAME_CHARS, MAX_PROFILE_TOOLS,
    MAX_SOUL_BYTES, Profile, SUBAGENT_MODEL_NOT_ALLOWED, SpawnRecord,
};
use super::{
    CHILD_AGENT_PREFIX, CLOSE_AGENT, ChildIdentity, DEFAULT_WAIT_SECONDS, DEPTH_EXCEEDED,
    MAX_CHILD_STEPS, MAX_LABEL_CHARS, MAX_TASK_CHARS, MAX_WAIT_IDS, MAX_WAIT_SECONDS, Nesting,
    Registry, RunRefusal, SEND_INPUT, SPAWN_AGENT, WAIT_AGENT,
};
use crate::delegation::Delegation;
use crate::tool_envelope::{RequestedTool, SUBAGENT_TOOL_NOT_GRANTED};

/// One spawn, checked and on its reserved slot.
struct Spawn {
    identity: ChildIdentity,
    task: String,
    steps: u32,
    isolated: bool,
    profile: Profile,
    brief: Brief,
    delegation: Option<Delegation>,
}

#[derive(Clone, Copy)]
pub(super) enum Action {
    Spawn,
    Send,
    Wait,
    Close,
}

impl Action {
    pub(super) const ALL: [Self; 4] = [Self::Spawn, Self::Send, Self::Wait, Self::Close];
}

pub(super) fn tool(action: Action, registry: Arc<Registry>) -> Arc<dyn Tool> {
    Arc::new(SubAgentTool { action, registry })
}

struct SubAgentTool {
    action: Action,
    registry: Arc<Registry>,
}

fn refused(code: &str, message: impl Into<String>) -> Value {
    json!({"status": "error", "code": code, "message": message.into()})
}

/// `a_` and 8 hex digits.
fn new_agent_id() -> Option<String> {
    let mut bytes = [0_u8; 4];
    SystemRandom::new().fill(&mut bytes).ok()?;
    Some(format!("a_{:08x}", u32::from_be_bytes(bytes)))
}

fn bounded_text(args: &Value, key: &str) -> Result<String, Value> {
    let text = args
        .get(key)
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|text| !text.is_empty())
        .ok_or_else(|| {
            refused(
                "invalid_arguments",
                format!("`{key}` must be a non-empty string."),
            )
        })?;
    if text.chars().count() > MAX_TASK_CHARS {
        return Err(refused(
            "invalid_arguments",
            format!("`{key}` is longer than {MAX_TASK_CHARS} characters; shorten it."),
        ));
    }
    Ok(text.to_owned())
}

/// `spawn_agent`'s `max_steps` (the default when absent), or the refusal.
fn max_steps(args: &Value, default: u32) -> Result<u32, Value> {
    match args.get("max_steps") {
        None | Some(Value::Null) => Ok(default),
        Some(value) => value
            .as_u64()
            .and_then(|n| u32::try_from(n).ok())
            .filter(|n| (1..=MAX_CHILD_STEPS).contains(n))
            .ok_or_else(|| {
                refused(
                    "invalid_arguments",
                    format!("`max_steps` must be an integer from 1 to {MAX_CHILD_STEPS}."),
                )
            }),
    }
}

/// `spawn_agent`'s task, label, `profile` and `context`, or the refusal.
/// Without a label, the profile's display name is the label.
fn spawn_arguments(args: &Value) -> Result<(String, Option<String>, Profile, Brief), Value> {
    let task = bounded_text(args, "task")?;
    let profile =
        profile::parse_profile(args).map_err(|message| refused("invalid_arguments", message))?;
    let brief =
        profile::parse_brief(args).map_err(|message| refused("invalid_arguments", message))?;
    let label = args
        .get("label")
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|label| !label.is_empty());
    if label.is_some_and(|label| label.chars().any(char::is_control)) {
        return Err(refused(
            "invalid_arguments",
            "`label` must not contain control characters.",
        ));
    }
    let label = label
        .or(profile.display_name.as_deref())
        .map(|label| label.chars().take(MAX_LABEL_CHARS).collect::<String>());
    Ok((task, label, profile, brief))
}

/// The answer to a spawn whose child could not be started, by its code.
fn start_refused(code: &'static str) -> Value {
    match code {
        SUBAGENT_TOOL_NOT_GRANTED => refused(SUBAGENT_TOOL_NOT_GRANTED, TOOL_NOT_GRANTED_MESSAGE),
        SUBAGENT_MODEL_NOT_ALLOWED => {
            refused(SUBAGENT_MODEL_NOT_ALLOWED, MODEL_NOT_ALLOWED_MESSAGE)
        }
        code => refused(code, "The sub-agent could not be started."),
    }
}

const TOOL_NOT_GRANTED_MESSAGE: &str = "A tool in `profile.tools` is not one of your tools a sub-agent may get. Name only your own tools that the agent's sub-agent tools allow (read-only tools when it sets none).";
const MODEL_NOT_ALLOWED_MESSAGE: &str = "The model in `profile.model` is not one a sub-agent may use here. Name a model of this project that you may use, or \"inherit\".";

/// A brief the host refused, by its code.
fn brief_refused(code: &str) -> Value {
    let message = match code {
        BRIEF_EXCERPT_NOT_VISIBLE => {
            "An excerpt in `context.excerpts` is not an exact quote of a message you can see here. Name the message by the id on its `[message m-...]` line and quote its text after that line."
        }
        BRIEF_ATTACHMENT_NOT_READABLE => {
            "An attachment in `context.attachments` is not one you can read in this turn. Name only this turn's attachment ids."
        }
        BRIEF_MEMORY_NOT_RECALLED => {
            "A memory entry in `context.memory_entries` is not one you were given in this turn. Name it exactly as its mark shows it, `N·TAG` from a `[memory N·TAG, ...]` line of your notes, or the `memory_ref` that `recall` gave you; a bare number or a mark without this turn's tag is not one of your notes. Recall it first, or pass it as a fact."
        }
        _ => "The brief could not be checked.",
    };
    refused(code, message)
}

/// `spawn_agent`'s arguments (without `isolation`).
fn spawn_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "task": {
                "type": "string",
                "maxLength": MAX_TASK_CHARS,
                "description": "Self-contained instructions; the child sees nothing else from this conversation."
            },
            "label": {
                "type": "string",
                "maxLength": MAX_LABEL_CHARS,
                "description": "A short name shown for the sub-agent"
            },
            "max_steps": {
                "type": "integer",
                "minimum": 1,
                "maximum": MAX_CHILD_STEPS,
                "description": "The most model steps it may take"
            },
            "profile": {
                "type": "object",
                "description": "Who the sub-agent is",
                "properties": {
                    "display_name": {
                        "type": "string",
                        "maxLength": MAX_DISPLAY_NAME_CHARS,
                        "description": "The name shown for it"
                    },
                    "soul": {
                        "type": "string",
                        "maxLength": MAX_SOUL_BYTES,
                        "description": "A short role and style (at most 2048 bytes of UTF-8), in place of the default role text; it never widens what the sub-agent may do"
                    },
                    "tools": {
                        "type": "array",
                        "maxItems": MAX_PROFILE_TOOLS,
                        "items": {"type": "string"},
                        "description": "Only these of your tools, as \"toolkit/tool\" or \"toolkit/*\"; each must be one a sub-agent may get"
                    },
                    "model": {
                        "type": "string",
                        "maxLength": profile::MAX_MODEL_NAME_BYTES,
                        "description": "\"inherit\" (the default): it uses your model. Or the name of another model of this project that you may use; any other is refused"
                    },
                    "avatar": {
                        "type": "string",
                        "enum": profile::DEFAULT_AVATAR_KEYS,
                        "description": "One of the default avatar keys, such as \"default:owl\"; any other key is ignored and the sub-agent has no avatar"
                    }
                },
                "additionalProperties": false
            },
            "context": {
                "type": "object",
                "description": "A brief passed before the task: only what you yourself can see",
                "properties": {
                    "excerpts": {
                        "type": "array",
                        "maxItems": MAX_BRIEF_ITEMS,
                        "items": {
                            "type": "object",
                            "properties": {
                                "message_id": {"type": "string", "description": "The id on the message's `[message m-...]` line, such as \"m-0123456789\""},
                                "quote": {"type": "string", "maxLength": MAX_BRIEF_TEXT_CHARS}
                            },
                            "required": ["message_id", "quote"],
                            "additionalProperties": false
                        },
                        "description": "Exact quotes of messages in this conversation that carry a `[message m-...]` line"
                    },
                    "attachments": {
                        "type": "array",
                        "maxItems": MAX_BRIEF_ITEMS,
                        "items": {"type": "string"},
                        "description": "Ids of this turn's attachments (\"att-...\")"
                    },
                    "facts": {
                        "type": "array",
                        "maxItems": MAX_BRIEF_ITEMS,
                        "items": {"type": "string", "maxLength": MAX_BRIEF_TEXT_CHARS},
                        "description": "Short facts in your own words"
                    },
                    "memory_entries": {
                        "type": "array",
                        "maxItems": MAX_BRIEF_ITEMS,
                        "items": {"type": "string"},
                        "description": "Memory entries you were given in this turn, each as N·TAG exactly as its [memory N·TAG, ...] line of your notes shows it, or a memory_ref from recall"
                    }
                },
                "additionalProperties": false
            }
        },
        "required": ["task"]
    })
}

impl SubAgentTool {
    fn children(&self) -> Vec<Child> {
        self.registry
            .children
            .lock()
            .map(|children| children.clone())
            .unwrap_or_default()
    }

    fn running(&self) -> usize {
        self.children()
            .iter()
            .filter(|child| !child.status().is_settled())
            .count()
    }

    fn find(&self, agent_id: &str) -> Option<Child> {
        self.children()
            .into_iter()
            .find(|child| child.identity().agent_id == agent_id)
    }

    fn unknown(agent_id: &str) -> Value {
        refused(
            "subagent_unknown",
            format!("No sub-agent {agent_id} was started in this turn."),
        )
    }

    /// `spawn_agent`'s `isolation`: whether the child gets a checkout of
    /// its own, or the refusal.
    fn isolation(&self, args: &Value) -> Result<bool, Value> {
        match args.get("isolation").filter(|value| !value.is_null()) {
            None => Ok(false),
            Some(value) => match value.as_str() {
                Some("none") => Ok(false),
                Some("worktree") if self.registry.isolation_allowed() => Ok(true),
                Some("worktree") => Err(refused(
                    "subagent_isolation_unsupported",
                    "Sub-agents here cannot get a checkout of their own; spawn a read-only one.",
                )),
                _ => Err(refused(
                    "invalid_arguments",
                    "`isolation` must be \"none\" or \"worktree\".",
                )),
            },
        }
    }

    /// `isolation` and `profile.tools`, which do not combine.
    fn child_tools(&self, args: &Value, profile: &Profile) -> Result<bool, Value> {
        let isolated = self.isolation(args)?;
        if isolated && profile.tools.is_some() {
            return Err(refused(
                "invalid_arguments",
                "`profile.tools` cannot be combined with `isolation: \"worktree\"`.",
            ));
        }
        Ok(isolated)
    }

    /// ADR-0044 decision 3: a child is one delegation deeper. Its tools are
    /// a subset of this agent's by construction (the read-only or isolated
    /// filter), so only the depth can refuse it. The child's own scope comes
    /// back with the evidence: a child that starts children of its own
    /// delegates from it (#1824).
    fn delegate(
        &self,
        isolated: bool,
        requested: Option<&[RequestedTool]>,
    ) -> Result<Option<Delegation>, Value> {
        let Some(scope) = self.registry.delegation.as_ref() else {
            return Ok(None);
        };
        // The filter names what the child may be given, so the effective
        // grant digest changes with the grant and the requested tools.
        let filter = if isolated {
            "isolated".to_owned()
        } else {
            let grant = self
                .registry
                .grant
                .as_ref()
                .filter(|grant| grant.grants_writes());
            match (grant, requested) {
                (None, None) => "read_only".to_owned(),
                (grant, requested) => {
                    let granted = grant.map(|grant| grant.granted_keys().join(","));
                    let mut asked: Vec<String> = requested
                        .unwrap_or_default()
                        .iter()
                        .map(RequestedTool::canonical)
                        .collect();
                    asked.sort();
                    format!(
                        "subagent_tools:{};requested:{}",
                        granted.unwrap_or_else(|| "read_only".to_owned()),
                        asked.join(",")
                    )
                }
            }
        };
        scope
            .delegate_subagent_scope(&filter, &self.registry.excluded)
            .map(Some)
            .map_err(|refusal| {
                let mut value = refused(refusal.code(), refusal.message());
                value["delegation"] = refusal.to_value();
                value
            })
    }

    /// The next child's identity, or the refusal when this agent or the run
    /// may start no more sub-agents. The run's spawn slot is reserved here
    /// (#1824: the budget is the run's, every level counted); the caller
    /// gives it back through [`super::RunChildren::settle`].
    fn identity(
        &self,
        ctx: &dyn ToolContext,
        label: Option<String>,
        record: SpawnRecord,
    ) -> Result<ChildIdentity, Value> {
        let limits = self.registry.limits;
        let reservation = match self.registry.run.reserve(limits.max_per_turn, new_agent_id) {
            Ok(reservation) => reservation,
            Err(RunRefusal::Limit) => {
                return Err(refused(
                    "subagent_limit",
                    format!(
                        "This turn already started {} sub-agents, the most it may; use their results.",
                        limits.max_per_turn
                    ),
                ));
            }
            Err(RunRefusal::NoId) => {
                return Err(refused(
                    "subagent_unavailable",
                    "No sub-agent id could be made.",
                ));
            }
        };
        // This agent's own running cap, after the run's budget.
        let running = self.running();
        if running >= limits.max_concurrent {
            self.registry.run.settle(reservation.ordinal, None);
            return Err(refused(
                "subagent_busy",
                format!(
                    "{running} sub-agents are already running, the most at once; wait for one (`{WAIT_AGENT}`) or close one (`{CLOSE_AGENT}`) first."
                ),
            ));
        }
        Ok(ChildIdentity {
            agent_id: reservation.agent_id,
            parent_call_id: ctx.function_call_id().to_owned(),
            label,
            ordinal: reservation.ordinal,
            depth: self.registry.level + 1,
            parent_agent_id: self
                .registry
                .owner
                .as_ref()
                .map(|owner| owner.agent_id.clone()),
            record,
        })
    }

    async fn spawn(&self, ctx: &dyn ToolContext, args: &Value) -> Value {
        let (task, label, profile, brief) = match spawn_arguments(args) {
            Ok(arguments) => arguments,
            Err(refusal) => return refusal,
        };
        let isolated = match self.child_tools(args, &profile) {
            Ok(isolated) => isolated,
            Err(refusal) => return refusal,
        };
        let delegation = match self.delegate(isolated, profile.tools.as_deref()) {
            Ok(delegation) => delegation,
            Err(refusal) => return refusal,
        };
        let steps = match max_steps(args, self.registry.limits.default_steps) {
            Ok(steps) => steps,
            Err(refusal) => return refusal,
        };
        let identity = match self.identity(ctx, label, SpawnRecord::new(&profile, &brief)) {
            Ok(identity) => identity,
            Err(refusal) => return refusal,
        };
        let ordinal = identity.ordinal;
        let (value, child) = self
            .start(Spawn {
                identity,
                task,
                steps,
                isolated,
                profile,
                brief,
                delegation,
            })
            .await;
        // The reserved slot: the run's child, or given back.
        self.registry.run.settle(ordinal, child);
        value
    }

    /// Start the child of `spawn` on its reserved slot: the answer, and the
    /// child when it started.
    async fn start(&self, spawn: Spawn) -> (Value, Option<Child>) {
        let Spawn {
            mut identity,
            task,
            steps,
            isolated,
            profile,
            brief,
            delegation,
        } = spawn;
        // ADR-0050 decision 9d: the brief names only what this agent itself
        // sees; the host checks each reference before anything starts.
        let task = if brief.is_empty() {
            task
        } else {
            match self.registry.host.check_brief(&identity, &brief) {
                Ok(checked) => {
                    identity.record.resolve_messages(&checked);
                    match profile::render(&brief, &checked) {
                        Some(rendered) => format!("{rendered}\n\n{task}"),
                        None => task,
                    }
                }
                Err(code) => return (brief_refused(code), None),
            }
        };
        let agent_id = identity.agent_id.clone();
        let requested = profile.tools.map(Arc::new);
        // #1825: the named model, else the one this agent runs on.
        let model = profile
            .model
            .clone()
            .or_else(|| self.registry.inherited_model.clone());
        let nests = self.registry.child_nests(isolated);
        // #1824: a child that may start children gets the four tools over a
        // registry of its own, one level deeper, under its own scope.
        let nested = nests.then(|| {
            self.registry.nested(Nesting {
                owner: identity.clone(),
                delegation: delegation
                    .as_ref()
                    .map(|delegation| delegation.scope.clone()),
                model: model.clone(),
                requested: requested.clone(),
            })
        });
        let started = child::start(child::Start {
            identity,
            task,
            steps,
            budget: self.registry.limits.input_token_budget,
            instruction: self
                .registry
                .instruction_for(isolated, nests, profile.soul.as_deref()),
            excluded: Arc::new(self.registry.excluded.clone()),
            host: Arc::clone(&self.registry.host),
            isolated,
            grant: self.registry.grant.clone(),
            requested,
            ancestors: self.registry.ancestors.clone(),
            model,
            nested,
        })
        .await;
        match started {
            Ok(child) => {
                if let Ok(mut children) = self.registry.children.lock() {
                    children.push(child.clone());
                } else {
                    child.close();
                    return (
                        refused(
                            "subagent_unavailable",
                            "The sub-agent could not be started.",
                        ),
                        None,
                    );
                }
                let mut value = json!({"agent_id": agent_id, "status": "running"});
                if let Some(delegation) = delegation {
                    value["delegation"] = json!({
                        "depth": delegation.evidence.depth,
                        "effective_grant_digest": delegation.evidence.effective_grant_digest,
                    });
                }
                (value, Some(child))
            }
            Err(code) => (start_refused(code), None),
        }
    }

    fn send(&self, args: &Value) -> Value {
        let Some(agent_id) = args.get("agent_id").and_then(Value::as_str) else {
            return refused("invalid_arguments", "`agent_id` must be a string.");
        };
        let message = match bounded_text(args, "message") {
            Ok(message) => message,
            Err(refusal) => return refusal,
        };
        let Some(child) = self.find(agent_id) else {
            return Self::unknown(agent_id);
        };
        match child.send(message, self.running(), self.registry.limits.max_concurrent) {
            Ok(()) => {
                self.registry.host.resumed(child.identity());
                json!({"agent_id": agent_id, "status": "running"})
            }
            Err(InputRefused::Busy) => refused(
                "subagent_busy",
                format!(
                    "The most sub-agents are already running; wait for one (`{WAIT_AGENT}`) first."
                ),
            ),
            Err(InputRefused::Ended(status)) => refused(
                "subagent_ended",
                format!(
                    "Sub-agent {agent_id} has ended ({}); start a new one for more work.",
                    status.as_str()
                ),
            ),
        }
    }

    async fn wait(&self, args: &Value) -> Value {
        let ids = match args.get("agent_ids") {
            // None named: every sub-agent still running, or (none running)
            // every one this turn started, so a spawn and a wait can share
            // one round of calls.
            None | Some(Value::Null) => {
                let all = self.children();
                let running: Vec<Value> = all
                    .iter()
                    .filter(|child| !child.status().is_settled())
                    .map(|child| json!(child.identity().agent_id))
                    .collect();
                if running.is_empty() {
                    all.iter()
                        .map(|child| json!(child.identity().agent_id))
                        .collect()
                } else {
                    running
                }
            }
            Some(Value::Array(ids)) if !ids.is_empty() && ids.len() <= MAX_WAIT_IDS => ids.clone(),
            Some(_) => {
                return refused(
                    "invalid_arguments",
                    format!("`agent_ids` names 1 to {MAX_WAIT_IDS} sub-agents."),
                );
            }
        };
        if ids.is_empty() {
            return refused("subagent_unknown", "No sub-agent was started in this turn.");
        }
        let timeout = match args.get("timeout_seconds") {
            None | Some(Value::Null) => DEFAULT_WAIT_SECONDS,
            Some(value) => match value
                .as_u64()
                .filter(|n| (1..=MAX_WAIT_SECONDS).contains(n))
            {
                Some(seconds) => seconds,
                None => {
                    return refused(
                        "invalid_arguments",
                        format!(
                            "`timeout_seconds` must be an integer from 1 to {MAX_WAIT_SECONDS}."
                        ),
                    );
                }
            },
        };
        let mut children = Vec::new();
        for id in &ids {
            let Some(id) = id.as_str() else {
                return refused("invalid_arguments", "`agent_ids` must be strings.");
            };
            let Some(child) = self.find(id) else {
                return Self::unknown(id);
            };
            if !children
                .iter()
                .any(|known: &Child| known.identity().agent_id == id)
            {
                children.push(child);
            }
        }
        // Every one settled, or the timeout: those still running say so.
        let all = futures::future::join_all(children.iter().map(Child::settled));
        let _ = tokio::time::timeout(Duration::from_secs(timeout), all).await;
        json!({"results": children.iter().map(|child| child.report().to_value()).collect::<Vec<_>>()})
    }

    fn close(&self, args: &Value) -> Value {
        let Some(agent_id) = args.get("agent_id").and_then(Value::as_str) else {
            return refused("invalid_arguments", "`agent_id` must be a string.");
        };
        let Some(child) = self.find(agent_id) else {
            return Self::unknown(agent_id);
        };
        if let Some(report) = child.close() {
            self.registry.host.finished(child.identity(), &report);
        }
        child.report().to_value()
    }
}

#[async_trait]
impl Tool for SubAgentTool {
    fn name(&self) -> &str {
        match self.action {
            Action::Spawn => SPAWN_AGENT,
            Action::Send => SEND_INPUT,
            Action::Wait => WAIT_AGENT,
            Action::Close => CLOSE_AGENT,
        }
    }

    fn description(&self) -> &str {
        match self.action {
            Action::Spawn => {
                if self.registry.isolation_allowed() {
                    "Start a sub-agent on a self-contained task and return at once with its agent_id. It has its own fresh context (it sees nothing of this conversation but the task) and runs alongside other sub-agents. By default it can read and search but never change anything: use it for independent investigations that would fill your context. With isolation \"worktree\" it works in a git checkout of its own and may change files there, never yours. A `profile` names it and gives it a role; a `context` brief passes it quotes, attachments, facts and memory entries that you can see. Then wait_agent for the results."
                } else {
                    "Start a read-only sub-agent on a self-contained task and return at once with its agent_id. It has its own fresh context (it sees nothing of this conversation but the task), can read and search but never change anything, and runs alongside other sub-agents. Use it for independent investigations that would fill your context. A `profile` names it and gives it a role; a `context` brief passes it quotes, attachments, facts and memory entries that you can see. Then wait_agent for the results."
                }
            }
            Action::Send => {
                "Send another message to a sub-agent: it continues on its own context (queued if it is still running)."
            }
            Action::Wait => {
                "Wait for sub-agents to finish and return their results (status, final report, steps and tokens): the ones named, or every one still running when none is named. Those still running at the timeout are reported as running."
            }
            Action::Close => "Stop a sub-agent and free its slot; returns what it had.",
        }
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(match self.action {
            Action::Spawn if self.registry.isolation_allowed() => {
                let mut schema = spawn_schema();
                schema["properties"]["isolation"] = json!({
                    "type": "string",
                    "enum": ["none", "worktree"],
                    "description": "\"worktree\": the sub-agent works in a git checkout of its own and may change files there (the person reviews and merges them); \"none\" (the default): it only reads"
                });
                schema
            }
            Action::Spawn => spawn_schema(),
            Action::Send => json!({
                "type": "object",
                "properties": {
                    "agent_id": {"type": "string"},
                    "message": {"type": "string", "maxLength": MAX_TASK_CHARS}
                },
                "required": ["agent_id", "message"]
            }),
            Action::Wait => json!({
                "type": "object",
                "properties": {
                    "agent_ids": {
                        "type": "array",
                        "items": {"type": "string"},
                        "minItems": 1,
                        "maxItems": MAX_WAIT_IDS
                    },
                    "timeout_seconds": {"type": "integer", "minimum": 1, "maximum": MAX_WAIT_SECONDS}
                }
            }),
            Action::Close => json!({
                "type": "object",
                "properties": {"agent_id": {"type": "string"}},
                "required": ["agent_id"]
            }),
        })
    }

    // Only waiting changes nothing: several waits may run together with
    // the parent's reads. Spawning, sending and closing run in call order.
    fn is_read_only(&self) -> bool {
        matches!(self.action, Action::Wait)
    }

    fn is_concurrency_safe(&self) -> bool {
        matches!(self.action, Action::Wait)
    }

    async fn execute(&self, ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        // The run's agent calls the run's registry; a child calls only the
        // registry it owns (#1824). Any other caller (a child of a run where
        // children never start children, a grandchild, a child calling
        // another's tools) is refused.
        let caller = ctx.agent_name();
        let allowed = match &self.registry.owner {
            None => !caller.starts_with(CHILD_AGENT_PREFIX),
            Some(owner) => caller == owner.agent_name(),
        };
        if !allowed {
            return Ok(refused(
                DEPTH_EXCEEDED,
                "A sub-agent cannot start or manage other sub-agents.",
            ));
        }
        Ok(match self.action {
            Action::Spawn => self.spawn(ctx.as_ref(), &args).await,
            Action::Send => self.send(&args),
            Action::Wait => self.wait(&args).await,
            Action::Close => self.close(&args),
        })
    }
}
