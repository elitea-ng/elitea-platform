//! The four sub-agent tools. Every refusal is a result the model reads
//! (`{"status": "error", "code", "message"}`), never a run failure.

use std::sync::Arc;
use std::time::Duration;

use adk_core::{Tool, ToolContext};
use async_trait::async_trait;
use ring::rand::{SecureRandom as _, SystemRandom};
use serde_json::{Value, json};

use super::child::{self, Child, InputRefused};
use super::{
    CHILD_AGENT_PREFIX, CLOSE_AGENT, ChildIdentity, DEFAULT_WAIT_SECONDS, DEPTH_EXCEEDED,
    MAX_CHILD_STEPS, MAX_LABEL_CHARS, MAX_TASK_CHARS, MAX_WAIT_IDS, MAX_WAIT_SECONDS, Registry,
    SEND_INPUT, SPAWN_AGENT, WAIT_AGENT,
};
use crate::delegation::DelegationEvidence;

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
                Some("worktree") if self.registry.host.supports_worktree_isolation() => Ok(true),
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

    /// ADR-0044 decision 3: a child is one delegation deeper. Its tools are
    /// a subset of this agent's by construction (the read-only or isolated
    /// filter), so only the depth can refuse it.
    fn delegate(&self, isolated: bool) -> Result<Option<DelegationEvidence>, Value> {
        let Some(scope) = self.registry.delegation.as_ref() else {
            return Ok(None);
        };
        scope
            .delegate_subagent(
                if isolated { "isolated" } else { "read_only" },
                &self.registry.excluded,
            )
            .map(Some)
            .map_err(|refusal| {
                let mut value = refused(refusal.code(), refusal.message());
                value["delegation"] = refusal.to_value();
                value
            })
    }

    async fn spawn(&self, ctx: &dyn ToolContext, args: &Value) -> Value {
        let task = match bounded_text(args, "task") {
            Ok(task) => task,
            Err(refusal) => return refusal,
        };
        let label = args
            .get("label")
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|label| !label.is_empty())
            .map(|label| label.chars().take(MAX_LABEL_CHARS).collect::<String>());
        let isolated = match self.isolation(args) {
            Ok(isolated) => isolated,
            Err(refusal) => return refusal,
        };
        let delegation = match self.delegate(isolated) {
            Ok(evidence) => evidence,
            Err(refusal) => return refusal,
        };
        let limits = self.registry.limits;
        let steps = match max_steps(args, limits.default_steps) {
            Ok(steps) => steps,
            Err(refusal) => return refusal,
        };
        let children = self.children();
        if children.len() >= limits.max_per_turn {
            return refused(
                "subagent_limit",
                format!(
                    "This turn already started {} sub-agents, the most it may; use their results.",
                    limits.max_per_turn
                ),
            );
        }
        let running = children
            .iter()
            .filter(|child| !child.status().is_settled())
            .count();
        if running >= limits.max_concurrent {
            return refused(
                "subagent_busy",
                format!(
                    "{running} sub-agents are already running, the most at once; wait for one (`{WAIT_AGENT}`) or close one (`{CLOSE_AGENT}`) first."
                ),
            );
        }
        let Some(agent_id) = (0..4).find_map(|_| {
            new_agent_id().filter(|id| {
                children
                    .iter()
                    .all(|child| &child.identity().agent_id != id)
            })
        }) else {
            return refused("subagent_unavailable", "No sub-agent id could be made.");
        };
        let identity = ChildIdentity {
            agent_id: agent_id.clone(),
            parent_call_id: ctx.function_call_id().to_owned(),
            label,
            ordinal: children.len() + 1,
        };
        let started = child::start(child::Start {
            identity,
            task,
            steps,
            budget: limits.input_token_budget,
            instruction: if isolated {
                self.registry.isolated_instruction.clone()
            } else {
                self.registry.instruction.clone()
            },
            excluded: Arc::new(self.registry.excluded.clone()),
            host: Arc::clone(&self.registry.host),
            isolated,
        })
        .await;
        match started {
            Ok(child) => {
                if let Ok(mut children) = self.registry.children.lock() {
                    children.push(child);
                } else {
                    child.close();
                    return refused(
                        "subagent_unavailable",
                        "The sub-agent could not be started.",
                    );
                }
                let mut value = json!({"agent_id": agent_id, "status": "running"});
                if let Some(evidence) = delegation {
                    value["delegation"] = json!({
                        "depth": evidence.depth,
                        "effective_grant_digest": evidence.effective_grant_digest,
                    });
                }
                value
            }
            Err(code) => refused(code, "The sub-agent could not be started."),
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
            Ok(()) => json!({"agent_id": agent_id, "status": "running"}),
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
                if self.registry.host.supports_worktree_isolation() {
                    "Start a sub-agent on a self-contained task and return at once with its agent_id. It has its own fresh context (it sees nothing of this conversation but the task) and runs alongside other sub-agents. By default it can read and search but never change anything: use it for independent investigations that would fill your context. With isolation \"worktree\" it works in a git checkout of its own and may change files there, never yours. Then wait_agent for the results."
                } else {
                    "Start a read-only sub-agent on a self-contained task and return at once with its agent_id. It has its own fresh context (it sees nothing of this conversation but the task), can read and search but never change anything, and runs alongside other sub-agents. Use it for independent investigations that would fill your context; then wait_agent for the results."
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
            Action::Spawn if self.registry.host.supports_worktree_isolation() => {
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
        // One level deep: a child is never given these tools, and a call
        // from a child's context is refused all the same.
        if ctx.agent_name().starts_with(CHILD_AGENT_PREFIX) {
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
