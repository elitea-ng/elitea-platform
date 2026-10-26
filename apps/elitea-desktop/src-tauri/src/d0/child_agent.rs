//! Nested agents as cloud child turns (#1239, ADR-0029 decision 1).
//!
//! An `application` tool of the agent a workspace turn runs is offered as
//! one adk tool named after the nested agent ([`ChildAgentSpec`]). A call
//! runs the nested agent as an ordinary cloud turn on a worker, in a hidden
//! child conversation linked to this turn (`startLocalChildTurn`): the
//! desktop never joins the worker protocol and never holds the child's
//! toolkit credentials. It reads the child back (`readLocalChildTurn`) until
//! it settles, backing off from [`PollPolicy::first`] to [`PollPolicy::max`].
//!
//! * One `idempotency_key` per call, reused on every retry of the start
//!   (no answer, 429, 502, 503, 504), so a retry never starts a second child.
//! * A child paused for a person's decision (its own sensitive-tool HITL)
//!   is asked through the turn's approval channel, as a remote toolkit's
//!   confirmation is, and the answer resumes it (`continueChatExecution` on
//!   the child conversation). A pause only the web chat answers ends the
//!   call with the conversation to open.
//! * Dropping the call (the turn is cancelled, or the tool times out) stops
//!   the child through the chat's stop route. The start runs as its own
//!   task, so a call dropped while its start is still in flight lets that
//!   start finish (its retries included, under the same key) and stops the
//!   child as soon as the platform names it: no child outlives its call.
//! * The calls share the runtime's sub-agent limits (`MAX_TASK_CHARS`,
//!   `MAX_CHILDREN_PER_TURN`, `MAX_RESULT_BYTES`) and report to the webview
//!   as sub-agent lanes (`subagent_started`, `subagent_finished`).
//!
//! The tools sit in the remote toolset, so plan mode does not offer them (a
//! child may write) and the turn's local sub-agents never get them.

use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

use adk_core::{Tool, ToolContext};
use async_trait::async_trait;
use elitea_agent_runtime::host::{ApprovalOutcome, ApprovalRequest};
use elitea_agent_runtime::subagents::{MAX_CHILDREN_PER_TURN, MAX_RESULT_BYTES, MAX_TASK_CHARS};
use elitea_agent_runtime::text::clip_bytes;
use serde_json::{Value, json};

use super::approvals::REMOTE_PAYLOAD_KEY;
use super::definition::ChildAgentSpec;
use super::events::TurnEvents;
use super::remote_tools::{RemoteContext, RetryPolicy};

/// How a child is read back while it runs.
#[derive(Clone, Copy, Debug)]
pub struct PollPolicy {
    pub first: Duration,
    pub max: Duration,
}

impl PollPolicy {
    /// The quickest a child is ever polled: a zero retry delay must not
    /// read it in a tight loop.
    pub const FLOOR: Duration = Duration::from_millis(50);

    /// The turn's polling, from its retry delay: half a second backing off
    /// to three under the default delay (2 s), and as quick as a short
    /// test delay makes it, but never under [`Self::FLOOR`].
    #[must_use]
    pub fn for_retry(retry: RetryPolicy) -> Self {
        let first = retry.delay.min(Duration::from_millis(500)).max(Self::FLOOR);
        Self {
            first,
            max: retry
                .delay
                .saturating_mul(4)
                .min(Duration::from_secs(3))
                .max(first),
        }
    }
}

/// At most this many pauses one child call answers before it gives up (a
/// child that pauses on every step is the web chat's to finish).
const MAX_DECISIONS: usize = 8;

/// What every child call of one turn shares.
pub struct ChildAgentContext {
    /// The turn's platform client, execution, approval channel and retries.
    pub remote: Arc<RemoteContext>,
    pub events: Arc<TurnEvents>,
    pub poll: PollPolicy,
    started: AtomicUsize,
}

impl ChildAgentContext {
    #[must_use]
    pub fn new(remote: Arc<RemoteContext>, events: Arc<TurnEvents>, poll: PollPolicy) -> Self {
        Self {
            remote,
            events,
            poll,
            started: AtomicUsize::new(0),
        }
    }
}

pub struct CloudChildAgentTool {
    spec: ChildAgentSpec,
    description: String,
    context: Arc<ChildAgentContext>,
}

impl CloudChildAgentTool {
    #[must_use]
    pub fn new(spec: ChildAgentSpec, context: Arc<ChildAgentContext>) -> Self {
        let mut description = format!(
            "Delegate a task to the agent \"{}\". It runs on the Elitea platform as its own conversation, \
             with its own tools, and answers with its result. Give it everything it needs in `task`.",
            spec.name
        );
        if !spec.description.is_empty() {
            description.push(' ');
            description.push_str(&spec.description);
        }
        Self {
            spec,
            description,
            context,
        }
    }
}

fn error_result(code: &str, message: &str) -> Value {
    json!({ "status": "error", "code": code, "message": message })
}

#[async_trait]
impl Tool for CloudChildAgentTool {
    #[expect(
        clippy::misnamed_getters,
        reason = "the tool's name is the exposed (unique, sanitized) one; `spec.name` is the agent's display name"
    )]
    fn name(&self) -> &str {
        &self.spec.exposed_name
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "task": {
                    "type": "string",
                    "description": "What the agent should do, with the context it needs."
                }
            },
            "required": ["task"]
        }))
    }

    async fn execute(&self, ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        let Some(task) = args
            .get("task")
            .and_then(Value::as_str)
            .filter(|task| !task.trim().is_empty())
        else {
            return Ok(error_result("invalid_arguments", "`task` is required"));
        };
        if task.chars().count() > MAX_TASK_CHARS {
            return Ok(error_result(
                "invalid_arguments",
                &format!("`task` is longer than {MAX_TASK_CHARS} characters"),
            ));
        }
        Ok(call(&self.context, &self.spec, ctx.function_call_id(), task).await)
    }
}

/// The lane one child call reports to.
struct Lane {
    events: Arc<TurnEvents>,
    agent_id: String,
}

impl Lane {
    fn finish(&self, status: &str, result: &Value, usage: Option<&Value>) {
        let tokens = |key: &str| {
            usage
                .and_then(|usage| usage.get(key))
                .and_then(Value::as_u64)
                .unwrap_or(0)
        };
        let mut payload = json!({
            "agent_id": self.agent_id,
            "status": status,
            "steps": 0,
            "input_tokens": tokens("prompt_tokens"),
            "output_tokens": tokens("completion_tokens"),
        });
        match result.get("result").and_then(Value::as_str) {
            Some(text) if status == "completed" => payload["result"] = json!(text),
            _ => {
                if let Some(message) = result.get("message").and_then(Value::as_str) {
                    payload["error"] = json!(message);
                }
            }
        }
        self.events.send("subagent_finished", payload);
    }
}

/// Run one child call to its end, the person's decisions included.
pub async fn call(
    context: &ChildAgentContext,
    spec: &ChildAgentSpec,
    call_id: &str,
    task: &str,
) -> Value {
    let ordinal = context.started.fetch_add(1, Ordering::SeqCst);
    if ordinal >= MAX_CHILDREN_PER_TURN {
        return error_result(
            "child_limit",
            &format!("a turn starts at most {MAX_CHILDREN_PER_TURN} child agents"),
        );
    }
    let lane = Lane {
        events: context.events.clone(),
        agent_id: format!("cloud_{}", ordinal + 1),
    };
    context.events.send(
        "subagent_started",
        json!({
            "agent_id": lane.agent_id,
            "parent_call_id": call_id,
            "ordinal": ordinal + 1,
            "label": spec.name,
        }),
    );
    let body = json!({
        "application_id": spec.application_id,
        "version_id": spec.version_id,
        "input": task,
        "idempotency_key": uuid::Uuid::new_v4().to_string(),
    });
    let mut guard = CancelOnDrop {
        context: context.remote.clone(),
        child: Child::Starting(tokio::spawn(start(context.remote.clone(), body))),
        lane: Some(lane),
    };
    let started = match guard.started().await {
        Ok(started) => started,
        Err(result) => {
            if let Some(lane) = guard.disarm() {
                lane.finish("failed", &result, None);
            }
            return result;
        }
    };
    let end = follow(context, spec, call_id, &started).await;
    if !end.settled {
        // The child may still run (its reads failed, or its state is one
        // this app does not know): stop it rather than leave it running
        // unwatched. Best effort; the call answers its error either way.
        stop_child(&context.remote, &started.response_message_id).await;
    }
    if let Some(lane) = guard.disarm() {
        lane.finish(end.status, &end.result, end.usage.as_ref());
    }
    end.result
}

/// How [`follow`] ended: its lane status, the model's result, the last usage
/// read, and whether the child itself reached a state that ends the call (a
/// terminal one, or a pause left for the web chat). An unsettled end leaves
/// a child that may still run, and the call stops it.
struct FollowEnd {
    status: &'static str,
    result: Value,
    usage: Option<Value>,
    settled: bool,
}

impl FollowEnd {
    fn settled(status: &'static str, result: Value, usage: Option<Value>) -> Self {
        Self {
            status,
            result,
            usage,
            settled: true,
        }
    }

    fn unsettled(result: Value, usage: Option<Value>) -> Self {
        Self {
            status: "failed",
            result,
            usage,
            settled: false,
        }
    }
}

struct Started {
    execution_id: String,
    response_message_id: String,
    conversation_uuid: String,
}

/// `startLocalChildTurn`, retried with the body's one key. Spawned as its
/// own task (owned arguments), so a dropped call does not cut it short.
async fn start(remote: Arc<RemoteContext>, body: Value) -> Result<Started, Value> {
    let mut attempts = 0;
    loop {
        attempts += 1;
        let answer = remote
            .api
            .child_turn_start(remote.project_id, &remote.execution_id, &body)
            .await;
        let retry = match &answer {
            Err(_) => true,
            Ok(answer) => matches!(answer.status, 429 | 502 | 503 | 504),
        };
        if retry {
            if attempts >= remote.retry.attempts {
                return Err(error_result(
                    "child_agent_unavailable",
                    "the platform did not start the agent; try again later",
                ));
            }
            tokio::time::sleep(remote.retry.delay).await;
            continue;
        }
        let Ok(answer) = answer else { continue };
        let text = |key: &str| {
            answer
                .body
                .get(key)
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned()
        };
        if answer.status == 202 {
            let started = Started {
                execution_id: text("child_execution_id"),
                response_message_id: text("response_message_id"),
                conversation_uuid: text("conversation_uuid"),
            };
            if started.execution_id.is_empty() || started.response_message_id.is_empty() {
                return Err(error_result(
                    "invalid_response",
                    "the platform's answer named no child execution",
                ));
            }
            return Ok(started);
        }
        let code = match text("error") {
            code if code.is_empty() => format!("http_{}", answer.status),
            code => code,
        };
        let message = match (answer.status, text("message")) {
            (404, message) if code == "http_404" || message.is_empty() => {
                "this platform cannot run nested agents from a workspace; update the platform or run the agent in the cloud".to_owned()
            }
            (_, message) if message.is_empty() => "the platform refused to start the agent".to_owned(),
            (_, message) => message,
        };
        return Err(error_result(&code, &message));
    }
}

/// Read the child until it settles.
async fn follow(
    context: &ChildAgentContext,
    spec: &ChildAgentSpec,
    call_id: &str,
    started: &Started,
) -> FollowEnd {
    let remote = &context.remote;
    let mut interval = context.poll.first;
    let mut failures = 0;
    let mut decisions = 0;
    let mut usage = None;
    // The pause last answered: until the child moves past it, a read may
    // still report it, and it is not asked again.
    let mut answered: Option<String> = None;
    loop {
        tokio::time::sleep(interval).await;
        interval = (interval * 2).min(context.poll.max);
        let answer = remote
            .api
            .child_turn_read(
                remote.project_id,
                &remote.execution_id,
                &started.execution_id,
            )
            .await;
        let state = match answer {
            Ok(answer) if answer.status == 200 => answer.body,
            Ok(answer) if !matches!(answer.status, 429 | 502 | 503 | 504) => {
                let code = answer
                    .body
                    .get("error")
                    .and_then(Value::as_str)
                    .map_or_else(|| format!("http_{}", answer.status), str::to_owned);
                return FollowEnd::unsettled(
                    error_result(&code, "the platform could not read the agent's run"),
                    usage,
                );
            }
            _ => {
                failures += 1;
                if failures >= remote.retry.attempts {
                    return FollowEnd::unsettled(
                        error_result(
                            "child_agent_unavailable",
                            "the platform stopped answering about the agent's run",
                        ),
                        usage,
                    );
                }
                continue;
            }
        };
        failures = 0;
        if let Some(read) = state.get("usage").filter(|u| u.is_object()) {
            usage = Some(read.clone());
        }
        let text = |key: &str| state.get(key).and_then(Value::as_str).unwrap_or_default();
        let open_in_chat = |what: &str| {
            let mut result = error_result(
                "child_agent_paused",
                &format!(
                    "the agent {what}; the person can finish it in the Elitea web app (its conversation is linked)"
                ),
            );
            result["child_conversation_uuid"] = json!(started.conversation_uuid);
            result
        };
        let interrupt_id = state
            .get("interrupt")
            .and_then(|interrupt| interrupt.get("interrupt_id"))
            .and_then(Value::as_str)
            .filter(|id| !id.is_empty());
        match text("status") {
            "running" => {}
            // Answered already: the resume has not reached the read yet.
            "interrupted" if interrupt_id.is_some() && interrupt_id == answered.as_deref() => {}
            "completed" => {
                return FollowEnd::settled("completed", completed(&state, started), usage);
            }
            "failed" => {
                let message = match text("error") {
                    "" => "the agent's run failed",
                    message => message,
                };
                return FollowEnd::settled(
                    "failed",
                    error_result("child_agent_failed", message),
                    usage,
                );
            }
            "cancelled" => {
                return FollowEnd::settled(
                    "cancelled",
                    error_result("child_agent_cancelled", "the agent's run was stopped"),
                    usage,
                );
            }
            "interrupted" if decisions < MAX_DECISIONS => {
                decisions += 1;
                let Some(action) = decide(context, spec, call_id, state.get("interrupt")).await
                else {
                    return FollowEnd::settled(
                        "failed",
                        open_in_chat("asked a question this app cannot answer"),
                        usage,
                    );
                };
                let body = json!({
                    "project_id": remote.project_id,
                    "conversation_uuid": started.conversation_uuid,
                    "message_id": started.response_message_id,
                    "hitl_resume": true,
                    "hitl_action": action,
                    "mcp_tokens": {},
                });
                match remote
                    .api
                    .child_turn_decide(remote.project_id, &started.conversation_uuid, &body)
                    .await
                {
                    // Resumed, or already answered elsewhere: the reads
                    // follow it either way.
                    Ok(answer) if (200..300).contains(&answer.status) || answer.status == 409 => {}
                    _ => {
                        return FollowEnd::settled(
                            "failed",
                            open_in_chat("is waiting on a decision that could not be sent"),
                            usage,
                        );
                    }
                }
                answered = interrupt_id.map(str::to_owned);
                interval = context.poll.first;
            }
            "interrupted" => {
                return FollowEnd::settled(
                    "failed",
                    open_in_chat("keeps asking for decisions"),
                    usage,
                );
            }
            "authorization_required" => {
                return FollowEnd::settled(
                    "failed",
                    open_in_chat("needs a sign-in to one of its tools"),
                    usage,
                );
            }
            "output_limit" => {
                return FollowEnd::settled(
                    "failed",
                    open_in_chat("stopped at its output limit"),
                    usage,
                );
            }
            _ => {
                return FollowEnd::unsettled(
                    error_result(
                        "invalid_response",
                        "the platform answered an unknown run state",
                    ),
                    usage,
                );
            }
        }
    }
}

/// The model's result of a completed child: its answer (at most
/// `MAX_RESULT_BYTES`), the tools it called, and its conversation.
fn completed(state: &Value, started: &Started) -> Value {
    let output = state
        .get("output")
        .and_then(Value::as_str)
        .unwrap_or_default();
    let clipped = clip_bytes(output, MAX_RESULT_BYTES);
    let calls: Vec<Value> = state
        .get("tool_calls_summary")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(|call| {
            let name = call.get("tool_name").and_then(Value::as_str)?;
            Some(
                json!({ "tool": name, "failed": call.get("is_error") == Some(&Value::Bool(true)) }),
            )
        })
        .collect();
    let mut result = json!({
        "status": "ok",
        "result": clipped,
        "tool_calls": calls,
        "child_conversation_uuid": started.conversation_uuid,
    });
    if clipped.len() < output.len() || state.get("output_truncated") == Some(&Value::Bool(true)) {
        result["truncated"] = json!(true);
    }
    result
}

/// Ask the person about a child's pause; the action to resume it with, or
/// `None` for a pause that is not an approve/reject question.
async fn decide(
    context: &ChildAgentContext,
    spec: &ChildAgentSpec,
    call_id: &str,
    interrupt: Option<&Value>,
) -> Option<&'static str> {
    let interrupt = interrupt?;
    let actions: Vec<&str> = interrupt
        .get("available_actions")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(Value::as_str)
        .collect();
    if !actions.contains(&"approve") || !actions.contains(&"reject") {
        return None;
    }
    let text = |key: &str| {
        interrupt
            .get(key)
            .and_then(Value::as_str)
            .unwrap_or_default()
    };
    let tool = text("tool_name");
    let title = match (text("action_label"), tool) {
        ("", "") => format!("{} asks to continue", spec.name),
        ("", tool) => format!("{} asks to run {tool}", spec.name),
        (label, _) => label.to_owned(),
    };
    let reason = match text("policy_message") {
        "" => text("message"),
        message => message,
    };
    let request = ApprovalRequest {
        subject: call_id.to_owned(),
        message: title.clone(),
        available_actions: vec!["approve".to_owned(), "reject".to_owned()],
        payload: json!({ REMOTE_PAYLOAD_KEY: {
            "tool": if tool.is_empty() { spec.name.as_str() } else { tool },
            "toolkit": spec.name,
            "title": title,
            "detail": format!("{} · runs in the cloud", spec.name),
            "reason": reason,
        }}),
    };
    match context.remote.prompt.request(request).await {
        Ok(ApprovalOutcome::Decided { action, .. }) if action == "approve" => Some("approve"),
        _ => Some("reject"),
    }
}

/// Where the call's child is: its start still running (as a task), or the
/// child the platform named.
enum Child {
    Starting(tokio::task::JoinHandle<Result<Started, Value>>),
    Started(String),
    /// The start failed (nothing to stop), or the guard already acted.
    None,
}

/// Stops the child when its call is dropped before it settled, also while
/// its start is still in flight.
struct CancelOnDrop {
    context: Arc<RemoteContext>,
    child: Child,
    lane: Option<Lane>,
}

impl CancelOnDrop {
    /// Wait for the start; once it named the child, a drop stops that child.
    async fn started(&mut self) -> Result<Started, Value> {
        let Child::Starting(handle) = &mut self.child else {
            return Err(error_result(
                "invalid_state",
                "the agent was already started",
            ));
        };
        let outcome = handle.await.unwrap_or_else(|_| {
            Err(error_result(
                "child_agent_unavailable",
                "the platform did not start the agent; try again later",
            ))
        });
        self.child = match &outcome {
            Ok(started) => Child::Started(started.response_message_id.clone()),
            Err(_) => Child::None,
        };
        outcome
    }

    fn disarm(&mut self) -> Option<Lane> {
        self.child = Child::None;
        self.lane.take()
    }
}

/// `cancelChatExecution` of a child, best effort (logged when it fails).
async fn stop_child(context: &RemoteContext, response_message_id: &str) {
    match context
        .api
        .child_turn_cancel(context.project_id, response_message_id)
        .await
    {
        Ok(answer) if (200..300).contains(&answer.status) || matches!(answer.status, 404 | 409) => {
        }
        Ok(answer) => log::warn!(
            "could not stop a cloud child turn: the platform answered {}",
            answer.status
        ),
        Err(error) => log::warn!("could not stop a cloud child turn: {error}"),
    }
}

impl Drop for CancelOnDrop {
    fn drop(&mut self) {
        let Some(lane) = self.lane.take() else {
            return;
        };
        lane.finish(
            "cancelled",
            &error_result("child_agent_cancelled", "the turn was stopped"),
            None,
        );
        let Ok(runtime) = tokio::runtime::Handle::try_current() else {
            return;
        };
        let context = self.context.clone();
        match std::mem::replace(&mut self.child, Child::None) {
            Child::Started(response) => {
                runtime.spawn(async move { stop_child(&context, &response).await });
            }
            // The start is in flight: let it answer, then stop what it started.
            Child::Starting(handle) => {
                runtime.spawn(async move {
                    if let Ok(Ok(started)) = handle.await {
                        stop_child(&context, &started.response_message_id).await;
                    }
                });
            }
            Child::None => {}
        }
    }
}
