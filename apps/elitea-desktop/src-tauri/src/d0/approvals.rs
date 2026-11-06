//! The approval bridge: the person's prompt behind the local tools' rules
//! (`elitea_local_tools::approvals::RuleApprovals`) and the remote
//! toolkits' `confirmation_required`, answered in the UI over IPC.
//!
//! A question becomes an `approval_request` event with a fresh
//! `request_id`; `approval_respond` resolves it. A turn that ends while a
//! question is open drops it (the waiting tool call is dropped with the
//! run), and a late answer is ignored. A question that carries a wait
//! ([`WAIT_PAYLOAD_KEY`]) and is not answered within it is closed: an
//! `approval_closed` event, a `timeout` exchange, and the asker reads
//! [`TIMEOUT_ACTION`]; a late answer then finds it closed.
//!
//! A plan an `exit_plan_mode` call proposes (#1225) is a `plan_proposed`
//! event instead, answered `accept` or `decline`: only `accept` accepts it,
//! and no rule or remembered choice ever answers it.
//!
//! No question waits forever (ADR-0045 decision 3): a question without its
//! own wait gets its kind's ([`super::unattended::default_wait`]), and a
//! plan card [`super::unattended::PLAN_WAIT`]. In an unattended turn
//! ([`TurnBinding::unattended`]) the grant answers a remote confirmation it
//! covers at once. Every other question goes to the person as in every
//! other turn and takes its default past its wait; a question of a kind
//! without a wait gets [`super::unattended::RULE_ASK_WAIT`] there, so an
//! unattended chain never waits forever. Each exchange records its
//! `source`: `person`, `grant`, `bypass` (an ask the rules answered inline,
//! [`AnswerSink`]) or `timeout`.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use elitea_agent_runtime::host::{
    ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError, HostErrorCode,
};
use elitea_agent_runtime::step_limit::{CONTINUE_KIND, CONTINUE_PAYLOAD_KEY};
use elitea_local_tools::approvals::{
    ACCEPT, APPROVE_ALWAYS, APPROVE_SESSION, AnswerSink, DECLINE, Decision, PAYLOAD_KEY,
    PLAN_PAYLOAD_KEY, Source, ToolCall, Verdict,
};
use elitea_local_tools::session::{BY_PERSON, DECIDED_BY};
use serde_json::{Value, json};
use tokio::sync::oneshot;

use super::events::TurnEvents;
use super::recorder::Recorder;
use super::unattended::{
    PLAN_WAIT, RULE_ASK_WAIT, SOURCE_BYPASS, SOURCE_GRANT, SOURCE_PERSON, SOURCE_TIMEOUT,
    UnattendedGrant, default_wait,
};

/// The payload key a remote toolkit confirmation travels under.
pub const REMOTE_PAYLOAD_KEY: &str = "remote_toolkit_call";

/// The payload key an MCP sign-in question travels under (issue 1231).
pub const MCP_AUTHORIZATION_PAYLOAD_KEY: &str = "mcp_authorization";

/// The payload key of a question's wait, in milliseconds: past it the
/// question is closed and answered [`TIMEOUT_ACTION`].
pub const WAIT_PAYLOAD_KEY: &str = "wait_ms";

/// The action a question closed by its wait is answered with.
pub const TIMEOUT_ACTION: &str = "timeout";

impl UiDecision {
    /// The person's text for the model, carried by a denial or a decline.
    fn feedback(&self) -> Option<String> {
        match self {
            Self::Decline(feedback) => feedback.clone(),
            Self::DenyWithFeedback(feedback) => Some(feedback.clone()),
            _ => None,
        }
    }
}

/// The `hitl_exchanges` kind of a plan card.
pub const PLAN_KIND: &str = "plan_approval";

/// The person's answer, as `approval_respond` names it (`continue` and
/// `cancel`, the MCP sign-in card's answers, are `allow_once` and `deny`;
/// `accept` and `decline` answer a plan card).
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum UiDecision {
    AllowOnce,
    AllowAlways,
    Deny,
    /// Accept a proposed plan. On any other card it is a denial.
    Accept,
    /// Decline a proposed plan, with the person's feedback for the model.
    Decline(Option<String>),
    /// Deny any card and tell the agent why (#1225): the model reads the
    /// text as the call's typed failure (`declined_by_person`).
    DenyWithFeedback(String),
}

impl UiDecision {
    /// `decision` as `approval_respond` sends it, with its `feedback`
    /// (taken by `decline` and `deny_with_feedback`; without any,
    /// `deny_with_feedback` is a plain `deny`).
    #[must_use]
    pub fn parse(value: &str, feedback: Option<&str>) -> Option<Self> {
        // Feedback is never an approval: it only travels with a denial.
        let feedback = feedback
            .map(clean)
            .map(|text| cut(text.trim(), FEEDBACK_BYTES))
            .filter(|text| !text.is_empty());
        match value {
            "allow_once" | "continue" => Some(Self::AllowOnce),
            "allow_always" => Some(Self::AllowAlways),
            "deny" | "cancel" => Some(Self::Deny),
            "accept" => Some(Self::Accept),
            "decline" => Some(Self::Decline(feedback)),
            "deny_with_feedback" => Some(feedback.map_or(Self::Deny, Self::DenyWithFeedback)),
            _ => None,
        }
    }
}

type Pending = HashMap<String, (String, oneshot::Sender<UiDecision>)>;

/// Open questions of every turn, by request id.
#[derive(Default)]
pub struct ApprovalBroker {
    pending: Mutex<Pending>,
}

impl ApprovalBroker {
    fn register(&self, turn_id: &str) -> (String, oneshot::Receiver<UiDecision>) {
        let request_id = uuid::Uuid::new_v4().to_string();
        let (sender, receiver) = oneshot::channel();
        if let Ok(mut pending) = self.pending.lock() {
            pending.insert(request_id.clone(), (turn_id.to_owned(), sender));
        }
        (request_id, receiver)
    }

    /// Answer one open question. False when no such question is open.
    pub fn respond(&self, request_id: &str, decision: UiDecision) -> bool {
        let entry = self
            .pending
            .lock()
            .ok()
            .and_then(|mut pending| pending.remove(request_id));
        entry.is_some_and(|(_, sender)| sender.send(decision).is_ok())
    }

    /// Close one open question without an answer. False when it is no
    /// longer open (answered meanwhile).
    fn close(&self, request_id: &str) -> bool {
        self.pending
            .lock()
            .ok()
            .and_then(|mut pending| pending.remove(request_id))
            .is_some()
    }

    /// Drop every open question of a turn.
    pub fn forget_turn(&self, turn_id: &str) {
        if let Ok(mut pending) = self.pending.lock() {
            pending.retain(|_, (turn, _)| turn != turn_id);
        }
    }

    #[cfg(test)]
    pub fn open(&self) -> usize {
        self.pending.lock().map(|p| p.len()).unwrap_or_default()
    }
}

/// The turn a workspace's prompt currently answers for.
#[derive(Clone)]
pub struct TurnBinding {
    pub events: Arc<TurnEvents>,
    pub recorder: Arc<Recorder>,
    /// The turn's unattended grant: it answers the remote confirmations it
    /// covers; the person answers every other question.
    pub unattended: Option<Arc<UnattendedGrant>>,
}

/// The host prompt: the `prompt` of a workspace's local session, and the
/// channel remote toolkits ask through.
pub struct UiPrompt {
    broker: Arc<ApprovalBroker>,
    current: Mutex<Option<TurnBinding>>,
}

impl UiPrompt {
    #[must_use]
    pub fn new(broker: Arc<ApprovalBroker>) -> Self {
        Self {
            broker,
            current: Mutex::new(None),
        }
    }

    pub fn bind(&self, binding: Option<TurnBinding>) {
        if let Ok(mut current) = self.current.lock() {
            *current = binding;
        }
    }

    fn binding(&self) -> Option<TurnBinding> {
        self.current.lock().ok().and_then(|current| current.clone())
    }
}

fn ended() -> HostError {
    HostError::new(
        HostErrorCode::ExecutionEnded,
        "the turn ended before the question was answered",
    )
}

#[async_trait]
impl ApprovalChannel for UiPrompt {
    async fn request(&self, request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        let Some(binding) = self.binding() else {
            return Ok(ApprovalOutcome::Deferred);
        };
        if let Some(plan) = request.payload.get(PLAN_PAYLOAD_KEY) {
            return self.propose_plan(&binding, &request, plan).await;
        }
        let described = describe(&request);
        let can_remember = described["can_remember"] == Value::Bool(true);
        // A host-intel question (#1238) remembers for the app session only.
        let remember_action = if request
            .available_actions
            .iter()
            .any(|a| a == APPROVE_SESSION)
        {
            APPROVE_SESSION
        } else {
            APPROVE_ALWAYS
        };
        let (request_id, answer) = self.broker.register(binding.events.turn_id());
        let mut payload = described;
        payload["request_id"] = Value::String(request_id.clone());
        let title = payload["title"].as_str().unwrap_or_default().to_owned();
        let kind = if request.payload.get(REMOTE_PAYLOAD_KEY).is_some() {
            "tool_confirmation"
        } else if request.payload.get(CONTINUE_PAYLOAD_KEY).is_some() {
            CONTINUE_KIND
        } else if request.payload.get(MCP_AUTHORIZATION_PAYLOAD_KEY).is_some() {
            MCP_AUTHORIZATION_PAYLOAD_KEY
        } else {
            "command_approval"
        };
        if let Some(grant) = &binding.unattended
            && kind == "tool_confirmation"
            && grant.remote_confirmations
        {
            // The grant covers it: no card, no wait.
            self.broker.close(&request_id);
            return Ok(granted_confirmation(
                &binding,
                &request,
                &request_id,
                kind,
                &title,
            ));
        }
        binding.events.send("approval_request", payload);
        let wait = request
            .payload
            .get(WAIT_PAYLOAD_KEY)
            .and_then(Value::as_u64)
            .map(Duration::from_millis)
            .or_else(|| default_wait(kind))
            // An unattended chain never waits forever.
            .or_else(|| binding.unattended.is_some().then_some(RULE_ASK_WAIT));
        let decision = match wait {
            None => answer.await.map_err(|_| ended())?,
            Some(wait) => {
                let mut answer = answer;
                match tokio::time::timeout(wait, &mut answer).await {
                    Ok(decision) => decision.map_err(|_| ended())?,
                    // Answered just as the wait ran out: the answer stands.
                    Err(_) if !self.broker.close(&request_id) => {
                        answer.try_recv().map_err(|_| ended())?
                    }
                    Err(_) => {
                        // The card goes, and the exchange says nobody answered.
                        binding.events.send(
                            "approval_closed",
                            json!({ "request_id": request_id, "reason": TIMEOUT_ACTION }),
                        );
                        binding.recorder.hitl_exchange(exchange_of(
                            &request,
                            &request_id,
                            kind,
                            &title,
                            TIMEOUT_ACTION,
                            SOURCE_TIMEOUT,
                        ));
                        return Ok(ApprovalOutcome::Decided {
                            action: TIMEOUT_ACTION.to_owned(),
                            value: json!({ "reason": "nobody answered in time" }),
                        });
                    }
                }
            }
        };
        let approved = matches!(decision, UiDecision::AllowOnce | UiDecision::AllowAlways);
        let feedback = decision.feedback();
        let mut exchange = exchange_of(
            &request,
            &request_id,
            kind,
            &title,
            if approved { "approve" } else { "reject" },
            SOURCE_PERSON,
        );
        if let Some(feedback) = &feedback {
            exchange["value"] = Value::String(feedback.clone());
        }
        binding.recorder.hitl_exchange(exchange);
        Ok(match decision {
            UiDecision::AllowAlways if can_remember => ApprovalOutcome::Decided {
                action: remember_action.to_owned(),
                value: json!({}),
            },
            UiDecision::AllowOnce | UiDecision::AllowAlways => ApprovalOutcome::Decided {
                action: "approve".to_owned(),
                value: json!({}),
            },
            // `accept` and `decline` belong to plan cards: here they deny.
            UiDecision::Deny
            | UiDecision::Accept
            | UiDecision::Decline(_)
            | UiDecision::DenyWithFeedback(_) => {
                let mut value = json!({
                    "reason": "the person rejected it",
                    DECIDED_BY: BY_PERSON,
                });
                if let Some(feedback) = feedback {
                    value["feedback"] = Value::String(feedback);
                }
                ApprovalOutcome::Decided {
                    action: "reject".to_owned(),
                    value,
                }
            }
        })
    }
}

impl UiPrompt {
    /// A plan card: `plan_proposed {request_id, call_id, plan}`, answered
    /// `accept` (the only answer that accepts) or `decline`, with feedback.
    async fn propose_plan(
        &self,
        binding: &TurnBinding,
        request: &ApprovalRequest,
        proposal: &Value,
    ) -> Result<ApprovalOutcome, HostError> {
        let plan = proposal
            .get("plan")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let (request_id, answer) = self.broker.register(binding.events.turn_id());
        binding.events.send(
            "plan_proposed",
            json!({
                "request_id": request_id,
                "call_id": request.subject,
                "plan": plan,
            }),
        );
        let mut answer = answer;
        let decision = match tokio::time::timeout(PLAN_WAIT, &mut answer).await {
            Ok(decision) => decision.map_err(|_| ended())?,
            Err(_) if !self.broker.close(&request_id) => answer.try_recv().map_err(|_| ended())?,
            Err(_) => {
                // Nobody answered: the plan is declined.
                binding.events.send(
                    "approval_closed",
                    json!({ "request_id": request_id, "reason": TIMEOUT_ACTION }),
                );
                binding.recorder.hitl_exchange(exchange_of(
                    request,
                    &request_id,
                    PLAN_KIND,
                    &clean(plan),
                    DECLINE,
                    SOURCE_TIMEOUT,
                ));
                return Ok(ApprovalOutcome::Decided {
                    action: DECLINE.to_owned(),
                    value: json!({ "reason": "nobody answered in time" }),
                });
            }
        };
        let (action, value) = match decision {
            UiDecision::Accept => (ACCEPT, json!({})),
            UiDecision::Decline(Some(feedback)) | UiDecision::DenyWithFeedback(feedback) => {
                (DECLINE, json!({ "feedback": feedback }))
            }
            _ => (DECLINE, json!({})),
        };
        let mut exchange = exchange_of(
            request,
            &request_id,
            PLAN_KIND,
            &clean(plan),
            action,
            SOURCE_PERSON,
        );
        if let Some(feedback) = value.get("feedback").and_then(Value::as_str) {
            exchange["value"] = Value::String(cut(feedback, FEEDBACK_BYTES));
        }
        binding.recorder.hitl_exchange(exchange);
        Ok(ApprovalOutcome::Decided {
            action: action.to_owned(),
            value,
        })
    }
}

/// The most of a person's feedback a `hitl_exchanges` entry keeps (the
/// tool result bounds it the same way).
const FEEDBACK_BYTES: usize = elitea_local_tools::session::MAX_FEEDBACK_BYTES;

/// The answer of a remote confirmation that the unattended grant covers
/// (`remote_confirmations`): approved, `source: "grant"`.
fn granted_confirmation(
    binding: &TurnBinding,
    request: &ApprovalRequest,
    request_id: &str,
    kind: &str,
    title: &str,
) -> ApprovalOutcome {
    binding.recorder.hitl_exchange(exchange_of(
        request,
        request_id,
        kind,
        title,
        "approve",
        SOURCE_GRANT,
    ));
    ApprovalOutcome::Decided {
        action: "approve".to_owned(),
        value: json!({ "source": SOURCE_GRANT }),
    }
}

/// The rules answered a question inline: the exchange records it, with
/// `source` `bypass` or `grant`.
impl AnswerSink for UiPrompt {
    fn answered(&self, call_id: &str, call: &ToolCall, decision: &Decision) {
        let Some(binding) = self.binding() else {
            return;
        };
        let source = match decision.source {
            Source::Grant => SOURCE_GRANT,
            Source::Bypass => SOURCE_BYPASS,
            _ => return,
        };
        let request = ApprovalRequest {
            subject: call_id.to_owned(),
            message: decision.reason.clone(),
            available_actions: Vec::new(),
            payload: json!({ PAYLOAD_KEY: call }),
        };
        let described = describe(&request);
        let detail = described["detail"].as_str().unwrap_or_default();
        let title = if detail.is_empty() {
            described["tool"].as_str().unwrap_or_default().to_owned()
        } else {
            format!(
                "{}: {detail}",
                described["tool"].as_str().unwrap_or_default()
            )
        };
        let action = if decision.verdict == Verdict::Allow {
            "approve"
        } else {
            TIMEOUT_ACTION
        };
        binding.recorder.hitl_exchange(exchange_of(
            &request,
            &uuid::Uuid::new_v4().to_string(),
            "command_approval",
            &clean(&title),
            action,
            source,
        ));
    }
}

/// One `hitl_exchanges` entry; `source` says who answered (`person`,
/// `grant`, `bypass`, `timeout`).
fn exchange_of(
    request: &ApprovalRequest,
    request_id: &str,
    kind: &str,
    title: &str,
    decision: &str,
    source: &str,
) -> Value {
    let mut exchange = json!({
        "interrupt_id": request_id,
        "kind": kind,
        "prompt": cut(title, 4096),
        "decision": decision,
        "source": source,
    });
    // "Continue?" belongs to the turn, not to a tool step.
    if kind != CONTINUE_KIND {
        exchange["tool_run_id"] = Value::String(request.subject.clone());
    }
    exchange
}

/// `text` without control characters other than newlines and tabs (the
/// server refuses a NUL in a stored exchange).
fn clean(text: &str) -> String {
    text.chars()
        .filter(|c| matches!(c, '\n' | '\t') || !c.is_control())
        .collect()
}

fn cut(text: &str, max: usize) -> String {
    elitea_agent_runtime::text::clip_bytes(text, max).to_owned()
}

/// [`cut`], with an ellipsis when it cut something.
fn clipped(text: &str, max: usize) -> String {
    let kept = elitea_agent_runtime::text::clip_bytes(text, max);
    if kept.len() < text.len() {
        format!("{kept}…")
    } else {
        kept.to_owned()
    }
}

/// The `approval_request` payload (without `request_id`).
fn describe(request: &ApprovalRequest) -> Value {
    let reason = request
        .payload
        .get("reason")
        .and_then(Value::as_str)
        .unwrap_or_default();
    if let Some(call) = request
        .payload
        .get(PAYLOAD_KEY)
        .cloned()
        .and_then(|value| serde_json::from_value::<ToolCall>(value).ok())
    {
        let tool = serde_json::to_value(call.tool)
            .ok()
            .and_then(|v| v.as_str().map(str::to_owned))
            .unwrap_or_default();
        let mut detail = call
            .command
            .clone()
            .unwrap_or_else(|| call.paths.join(", "));
        // A runtime memory call (#1233): the text it saves or searches for,
        // or the memory it deletes and what that memory says (the runtime
        // looked it up before asking).
        if let Some(memory) = request
            .payload
            .get(elitea_agent_runtime::memory_tools::APPROVAL_PAYLOAD_KEY)
        {
            detail = match (
                memory.get("content").and_then(Value::as_str),
                memory.get("id"),
            ) {
                (Some(text), Some(id)) => format!("memory {id}: {}", clipped(text, 1024)),
                (Some(text), None) => cut(text, 4096),
                (None, Some(id)) => format!("memory {id}"),
                (None, None) => detail,
            };
        }
        let mut payload = json!({
            "tool": tool,
            "title": capitalised(&request.message),
            "detail": detail,
            "paths": call.paths,
            "reason": reason,
            "can_remember": request.available_actions.iter().any(|a| a == APPROVE_ALWAYS),
        });
        if let Some(command) = &call.command {
            payload["command"] =
                json!(shlex::split(command).unwrap_or_else(|| vec![command.clone()]));
        }
        // A background process that serves on localhost, or lives until
        // the session ends, says so on the card (#1223).
        if call.loopback {
            payload["loopback"] = json!(true);
        }
        if call.keep_alive {
            payload["keep_alive"] = json!(true);
        }
        // A local MCP server's start or tool (#1230): which server and tool,
        // and for a start whether it gets the network or runs unconfined.
        if let Some(target) = &call.mcp {
            let mut mcp = json!({ "server": target.server, "read_only": target.read_only });
            if let Some(tool) = &target.tool {
                mcp["tool"] = json!(tool);
            }
            payload["mcp"] = mcp;
            if call.tool == elitea_local_tools::approvals::ToolKind::McpStart {
                payload["network"] = json!(call.network);
                payload["unconfined"] = json!(call.unconfined);
            }
        }
        // A `computer` call (ADR-0047): the app, window and action the
        // rules named, and whether only the person at the machine can
        // answer it (an unattended act).
        if let Some(target) = &call.computer {
            if let Some(bundle) = &target.bundle_id {
                payload["detail"] = json!(cut(
                    &[
                        Some(bundle.as_str()),
                        target.window_title.as_deref(),
                        target.action.as_deref()
                    ]
                    .into_iter()
                    .flatten()
                    .filter(|part| !part.is_empty())
                    .collect::<Vec<_>>()
                    .join(" · "),
                    1024
                ));
            }
            payload["computer"] = serde_json::to_value(target).unwrap_or(Value::Null);
            if request.payload.get("attended_only") == Some(&Value::Bool(true)) {
                payload["attended_only"] = json!(true);
            }
        }
        // Which layer asks (`workspace`, `user`, `default`, …) and, for a
        // rule, which one and its justification (the card shows them).
        for key in ["source", "rule", "justification"] {
            if let Some(text) = request.payload.get(key).and_then(Value::as_str) {
                payload[key] = Value::String(cut(text, 4096));
            }
        }
        return payload;
    }
    if let Some(question) = request.payload.get(CONTINUE_PAYLOAD_KEY) {
        // "Continue?" at the step limit: asked every time, never remembered.
        return json!({
            "kind": CONTINUE_KIND,
            "tool": "",
            "title": request.message,
            "detail": question.get("detail").and_then(Value::as_str).unwrap_or_default(),
            "steps": question.get("steps").and_then(Value::as_u64).unwrap_or_default(),
            "reason": "",
            "can_remember": false,
        });
    }
    if let Some(question) = request.payload.get(MCP_AUTHORIZATION_PAYLOAD_KEY) {
        // An MCP server's sign-in: a card in the transcript, never
        // remembered. `call_id` lets the UI drop it when the call ends.
        let text = |key: &str| {
            question
                .get(key)
                .and_then(Value::as_str)
                .unwrap_or_default()
        };
        let mut payload = json!({
            "kind": MCP_AUTHORIZATION_PAYLOAD_KEY,
            "call_id": request.subject,
            "tool": text("tool"),
            "toolkit": text("toolkit"),
            "title": text("title"),
            "detail": text("detail"),
            "server_url": text("server_url"),
            "reason": "",
            "can_remember": false,
        });
        if !text("authorize_path").is_empty() {
            payload["authorize_path"] = json!(text("authorize_path"));
        }
        return payload;
    }
    if let Some(hand_over) = request
        .payload
        .get(crate::computer_tools::REQUEST_USER_PAYLOAD_KEY)
    {
        // `computer_request_user` (ADR-0047): what the agent needs the
        // person to do; approve means "done", anything else declines.
        let text = |key: &str| {
            hand_over
                .get(key)
                .and_then(Value::as_str)
                .unwrap_or_default()
        };
        return json!({
            "tool": crate::computer_tools::COMPUTER_REQUEST_USER,
            "title": request.message,
            "detail": cut(text("reason"), 1024),
            "bundle_id": text("bundle_id"),
            "reason": "",
            "can_remember": false,
        });
    }
    if let Some(remote) = request.payload.get(REMOTE_PAYLOAD_KEY) {
        let text = |key: &str| remote.get(key).and_then(Value::as_str).unwrap_or_default();
        let session = request
            .available_actions
            .iter()
            .any(|a| a == APPROVE_SESSION);
        let mut payload = json!({
            "tool": text("tool"),
            "title": text("title"),
            "detail": text("detail"),
            "reason": text("reason"),
            "can_remember": session,
        });
        if session {
            payload["remember_scope"] = json!("session");
        }
        return payload;
    }
    json!({
        "tool": request.subject,
        "title": capitalised(&request.message),
        "detail": "",
        "reason": reason,
        "can_remember": false,
    })
}

fn capitalised(text: &str) -> String {
    let mut chars = text.chars();
    chars.next().map_or_else(String::new, |first| {
        first.to_uppercase().chain(chars).collect()
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::d0::events::VecEmitter;
    use elitea_local_tools::approvals::ToolKind;

    fn binding(emitter: &Arc<VecEmitter>) -> TurnBinding {
        TurnBinding {
            events: Arc::new(TurnEvents::new("t1".into(), emitter.clone())),
            recorder: Arc::new(Recorder::default()),
            unattended: None,
        }
    }

    fn command_request(actions: &[&str]) -> ApprovalRequest {
        let mut call = ToolCall::new(ToolKind::RunCommand);
        call.command = Some("rm -rf 'build dir'".into());
        ApprovalRequest {
            subject: "call-1".into(),
            message: "run a command".into(),
            available_actions: actions.iter().map(|a| (*a).to_owned()).collect(),
            payload: json!({ PAYLOAD_KEY: call, "reason": "a recursive delete" }),
        }
    }

    #[tokio::test]
    async fn a_question_goes_to_the_ui_and_its_answer_comes_back() {
        let emitter = Arc::new(VecEmitter::default());
        let broker = Arc::new(ApprovalBroker::default());
        let prompt = Arc::new(UiPrompt::new(broker.clone()));
        let binding = binding(&emitter);
        prompt.bind(Some(binding.clone()));
        let asking = tokio::spawn({
            let prompt = prompt.clone();
            async move {
                prompt
                    .request(command_request(&["approve", APPROVE_ALWAYS, "reject"]))
                    .await
            }
        });
        let request_id = loop {
            if let Some(event) = emitter.all().first().cloned() {
                break event.payload["request_id"].as_str().unwrap().to_owned();
            }
            tokio::task::yield_now().await;
        };
        let event = &emitter.all()[0];
        assert_eq!(event.kind, "approval_request");
        assert_eq!(event.payload["tool"], "run_command");
        assert_eq!(event.payload["title"], "Run a command");
        assert_eq!(event.payload["command"], json!(["rm", "-rf", "build dir"]));
        assert_eq!(event.payload["reason"], "a recursive delete");
        assert_eq!(event.payload["can_remember"], true);
        assert!(!broker.respond("unknown", UiDecision::AllowOnce));
        assert!(broker.respond(&request_id, UiDecision::AllowAlways));
        let outcome = asking.await.unwrap().unwrap();
        assert_eq!(
            outcome,
            ApprovalOutcome::Decided {
                action: APPROVE_ALWAYS.into(),
                value: json!({})
            }
        );
        // Answered once: a second answer finds nothing open.
        assert!(!broker.respond(&request_id, UiDecision::Deny));
        let exchanges = binding.recorder.hitl_exchanges();
        assert_eq!(exchanges[0]["decision"], "approve");
        assert_eq!(exchanges[0]["tool_run_id"], "call-1");
    }

    #[tokio::test]
    async fn allow_always_is_only_once_when_the_rules_cannot_remember() {
        let emitter = Arc::new(VecEmitter::default());
        let broker = Arc::new(ApprovalBroker::default());
        let prompt = Arc::new(UiPrompt::new(broker.clone()));
        prompt.bind(Some(binding(&emitter)));
        emitter.on_event.lock().unwrap().replace(Box::new({
            let broker = broker.clone();
            move |event| {
                let id = event.payload["request_id"].as_str().unwrap().to_owned();
                broker.respond(&id, UiDecision::AllowAlways);
            }
        }));
        let outcome = prompt
            .request(command_request(&["approve", "reject"]))
            .await
            .unwrap();
        assert_eq!(
            outcome,
            ApprovalOutcome::Decided {
                action: "approve".into(),
                value: json!({})
            }
        );
        assert_eq!(emitter.all()[0].payload["can_remember"], false);
    }

    #[tokio::test]
    async fn a_denial_rejects_and_a_forgotten_turn_ends_the_wait() {
        let emitter = Arc::new(VecEmitter::default());
        let broker = Arc::new(ApprovalBroker::default());
        let prompt = Arc::new(UiPrompt::new(broker.clone()));
        prompt.bind(Some(binding(&emitter)));
        let asking = tokio::spawn({
            let prompt = prompt.clone();
            async move {
                prompt
                    .request(command_request(&["approve", "reject"]))
                    .await
            }
        });
        while broker.open() == 0 {
            tokio::task::yield_now().await;
        }
        broker.forget_turn("t1");
        let error = asking.await.unwrap().unwrap_err();
        assert_eq!(error.code(), HostErrorCode::ExecutionEnded);

        emitter.on_event.lock().unwrap().replace(Box::new({
            let broker = broker.clone();
            move |event| {
                let id = event.payload["request_id"].as_str().unwrap().to_owned();
                broker.respond(&id, UiDecision::Deny);
            }
        }));
        let outcome = prompt
            .request(command_request(&["approve", "reject"]))
            .await
            .unwrap();
        assert!(matches!(outcome, ApprovalOutcome::Decided { action, .. } if action == "reject"));
    }

    /// A rule's ask shows which rule and its justification (#1216).
    #[test]
    fn a_rules_question_names_the_rule_and_its_justification() {
        let mut request = command_request(&["approve", "reject"]);
        request.payload["source"] = json!("user");
        request.payload["rule"] = json!("user#2");
        request.payload["justification"] = json!("Deletes go through review.");
        let described = describe(&request);
        assert_eq!(described["source"], "user");
        assert_eq!(described["rule"], "user#2");
        assert_eq!(described["justification"], "Deletes go through review.");
        let plain = describe(&command_request(&["approve", "reject"]));
        assert!(plain.get("rule").is_none() && plain.get("justification").is_none());
    }

    fn remote_request() -> ApprovalRequest {
        ApprovalRequest {
            subject: "call-r".into(),
            message: "Create a Jira issue".into(),
            available_actions: vec!["approve".into(), "reject".into()],
            payload: json!({ REMOTE_PAYLOAD_KEY: {"tool": "create_issue", "toolkit": "Jira", "title": "Create a Jira issue"} }),
        }
    }

    /// ADR-0045 decision 3: a rule's ask without its own wait declines
    /// after 30 minutes, a remote confirmation after 15; each exchange says
    /// nobody answered (`source: "timeout"`).
    #[tokio::test(start_paused = true)]
    async fn every_local_question_declines_after_its_wait() {
        let emitter = Arc::new(VecEmitter::default());
        let broker = Arc::new(ApprovalBroker::default());
        let prompt = Arc::new(UiPrompt::new(broker.clone()));
        let binding = binding(&emitter);
        prompt.bind(Some(binding.clone()));
        for (request, wait) in [
            (
                command_request(&["approve", "reject"]),
                super::super::unattended::RULE_ASK_WAIT,
            ),
            (
                remote_request(),
                super::super::unattended::REMOTE_CONFIRMATION_WAIT,
            ),
        ] {
            let started = tokio::time::Instant::now();
            let outcome = prompt.request(request).await.unwrap();
            assert!(
                matches!(outcome, ApprovalOutcome::Decided { ref action, .. } if action == TIMEOUT_ACTION)
            );
            assert_eq!(started.elapsed(), wait);
        }
        assert_eq!(broker.open(), 0);
        let exchanges = binding.recorder.hitl_exchanges();
        assert_eq!(exchanges.len(), 2);
        assert!(
            exchanges
                .iter()
                .all(|e| e["source"] == SOURCE_TIMEOUT && e["decision"] == TIMEOUT_ACTION)
        );
        assert_eq!(exchanges[1]["kind"], "tool_confirmation");
    }

    /// An answered question records the person as its source.
    #[tokio::test]
    async fn an_answer_records_the_person() {
        let emitter = Arc::new(VecEmitter::default());
        let broker = Arc::new(ApprovalBroker::default());
        let prompt = Arc::new(UiPrompt::new(broker.clone()));
        let binding = binding(&emitter);
        prompt.bind(Some(binding.clone()));
        emitter.on_event.lock().unwrap().replace(Box::new({
            let broker = broker.clone();
            move |event| {
                let id = event.payload["request_id"].as_str().unwrap().to_owned();
                broker.respond(&id, UiDecision::AllowOnce);
            }
        }));
        prompt.request(remote_request()).await.unwrap();
        assert_eq!(
            binding.recorder.hitl_exchanges()[0]["source"],
            SOURCE_PERSON
        );
    }

    fn unattended_binding(emitter: &Arc<VecEmitter>, remote_confirmations: bool) -> TurnBinding {
        TurnBinding {
            unattended: Some(Arc::new(UnattendedGrant {
                remote_confirmations,
                ..UnattendedGrant::default()
            })),
            ..binding(emitter)
        }
    }

    /// In an unattended turn the grant approves a remote confirmation it
    /// covers at once: no card, no wait, `source: "grant"`.
    #[tokio::test(start_paused = true)]
    async fn an_unattended_grant_confirms_what_it_covers_at_once() {
        let emitter = Arc::new(VecEmitter::default());
        let broker = Arc::new(ApprovalBroker::default());
        let prompt = Arc::new(UiPrompt::new(broker.clone()));
        let covered = unattended_binding(&emitter, true);
        prompt.bind(Some(covered.clone()));
        let started = tokio::time::Instant::now();
        let approved = prompt.request(remote_request()).await.unwrap();
        assert_eq!(
            approved,
            ApprovalOutcome::Decided {
                action: "approve".into(),
                value: json!({ "source": SOURCE_GRANT })
            }
        );
        let exchanges = covered.recorder.hitl_exchanges();
        assert_eq!(exchanges.len(), 1);
        assert_eq!(exchanges[0]["kind"], "tool_confirmation");
        assert_eq!(exchanges[0]["source"], SOURCE_GRANT);
        assert_eq!(started.elapsed(), Duration::ZERO, "nothing waited");
        assert!(emitter.all().is_empty(), "no card showed");
        assert_eq!(broker.open(), 0);
    }

    /// In an unattended turn a question the grant does not cover reaches
    /// the person (a card), and the person's answer is used.
    #[tokio::test]
    async fn an_unattended_turn_asks_the_person_what_its_grant_does_not_cover() {
        let emitter = Arc::new(VecEmitter::default());
        let broker = Arc::new(ApprovalBroker::default());
        let prompt = Arc::new(UiPrompt::new(broker.clone()));
        let uncovered = unattended_binding(&emitter, false);
        prompt.bind(Some(uncovered.clone()));
        emitter.on_event.lock().unwrap().replace(Box::new({
            let broker = broker.clone();
            move |event| {
                let id = event.payload["request_id"].as_str().unwrap().to_owned();
                broker.respond(&id, UiDecision::AllowOnce);
            }
        }));
        for request in [command_request(&["approve", "reject"]), remote_request()] {
            let outcome = prompt.request(request).await.unwrap();
            assert!(
                matches!(outcome, ApprovalOutcome::Decided { ref action, .. } if action == "approve")
            );
        }
        let cards = emitter.all();
        assert_eq!(cards.len(), 2);
        assert!(cards.iter().all(|e| e.kind == "approval_request"));
        let exchanges = uncovered.recorder.hitl_exchanges();
        assert_eq!(
            exchanges
                .iter()
                .map(|e| (
                    e["kind"].as_str().unwrap(),
                    e["decision"].as_str().unwrap(),
                    e["source"].as_str().unwrap()
                ))
                .collect::<Vec<_>>(),
            [
                ("command_approval", "approve", SOURCE_PERSON),
                ("tool_confirmation", "approve", SOURCE_PERSON),
            ]
        );
    }

    /// In an unattended turn a question nobody answers takes its default
    /// after its own wait (`source: "timeout"`), not at once. A question of
    /// a kind without a wait (an MCP sign-in) gets the rule ask's wait, so
    /// the chain never waits forever.
    #[tokio::test(start_paused = true)]
    async fn an_unattended_turn_declines_an_unanswered_question_after_its_wait() {
        let emitter = Arc::new(VecEmitter::default());
        let broker = Arc::new(ApprovalBroker::default());
        let prompt = Arc::new(UiPrompt::new(broker.clone()));
        let uncovered = unattended_binding(&emitter, false);
        prompt.bind(Some(uncovered.clone()));
        let sign_in = ApprovalRequest {
            subject: "call-m".into(),
            message: "Sign in to the server".into(),
            available_actions: vec!["approve".into(), "reject".into()],
            payload: json!({ MCP_AUTHORIZATION_PAYLOAD_KEY: {"server": "docs"} }),
        };
        for (request, wait) in [
            (command_request(&["approve", "reject"]), RULE_ASK_WAIT),
            (
                remote_request(),
                super::super::unattended::REMOTE_CONFIRMATION_WAIT,
            ),
            (sign_in, RULE_ASK_WAIT),
        ] {
            let started = tokio::time::Instant::now();
            let outcome = prompt.request(request).await.unwrap();
            assert!(
                matches!(outcome, ApprovalOutcome::Decided { ref action, .. } if action == TIMEOUT_ACTION)
            );
            assert_eq!(started.elapsed(), wait);
        }
        assert_eq!(broker.open(), 0);
        let exchanges = uncovered.recorder.hitl_exchanges();
        assert_eq!(exchanges.len(), 3);
        assert!(
            exchanges
                .iter()
                .all(|e| e["source"] == SOURCE_TIMEOUT && e["decision"] == TIMEOUT_ACTION)
        );
        assert_eq!(
            emitter
                .all()
                .iter()
                .filter(|e| e.kind == "approval_closed")
                .count(),
            3
        );
    }

    /// The rules' inline answers become exchanges of the bound turn, with
    /// their source.
    #[test]
    fn inline_answers_are_recorded_with_their_source() {
        let emitter = Arc::new(VecEmitter::default());
        let prompt = UiPrompt::new(Arc::new(ApprovalBroker::default()));
        let binding = binding(&emitter);
        prompt.bind(Some(binding.clone()));
        let call = ToolCall {
            command: Some("git reset --hard".into()),
            ..ToolCall::new(ToolKind::RunCommand)
        };
        let decision = |verdict, source| Decision {
            verdict,
            source,
            reason: "why".into(),
            rule: None,
        };
        prompt.answered("c1", &call, &decision(Verdict::Allow, Source::Bypass));
        prompt.answered("c2", &call, &decision(Verdict::Allow, Source::Grant));
        // Another layer's answer is no inline answer to a question.
        prompt.answered("c3", &call, &decision(Verdict::Allow, Source::Default));
        prompt.answered("c4", &call, &decision(Verdict::Deny, Source::Hook));
        let exchanges = binding.recorder.hitl_exchanges();
        assert_eq!(
            exchanges
                .iter()
                .map(|e| (
                    e["tool_run_id"].as_str().unwrap(),
                    e["decision"].as_str().unwrap(),
                    e["source"].as_str().unwrap()
                ))
                .collect::<Vec<_>>(),
            [
                ("c1", "approve", SOURCE_BYPASS),
                ("c2", "approve", SOURCE_GRANT),
            ]
        );
        assert_eq!(exchanges[0]["kind"], "command_approval");
        assert_eq!(exchanges[0]["prompt"], "run_command: git reset --hard");
    }

    /// A question with a wait that nobody answers is closed: the card goes
    /// (`approval_closed`), the exchange records the timeout, the asker
    /// reads `timeout`, and a late answer finds nothing open.
    #[tokio::test]
    async fn a_question_past_its_wait_is_closed_and_recorded() {
        let emitter = Arc::new(VecEmitter::default());
        let broker = Arc::new(ApprovalBroker::default());
        let prompt = Arc::new(UiPrompt::new(broker.clone()));
        let binding = binding(&emitter);
        prompt.bind(Some(binding.clone()));
        let request = ApprovalRequest {
            subject: "call-9".into(),
            message: "Jira needs your sign-in".into(),
            available_actions: vec!["continue".into(), "cancel".into()],
            payload: json!({
                MCP_AUTHORIZATION_PAYLOAD_KEY: {"tool": "create_issue", "toolkit": "Jira", "title": "Jira needs your sign-in"},
                WAIT_PAYLOAD_KEY: 20,
            }),
        };
        let outcome = prompt.request(request).await.unwrap();
        assert!(
            matches!(outcome, ApprovalOutcome::Decided { ref action, .. } if action == TIMEOUT_ACTION)
        );
        assert_eq!(broker.open(), 0);
        let events = emitter.all();
        let request_id = events[0].payload["request_id"].as_str().unwrap().to_owned();
        assert_eq!(events[1].kind, "approval_closed");
        assert_eq!(events[1].payload["request_id"], request_id.as_str());
        assert!(
            !broker.respond(&request_id, UiDecision::AllowOnce),
            "a late Continue finds it closed"
        );
        let exchanges = binding.recorder.hitl_exchanges();
        assert_eq!(exchanges.len(), 1);
        assert_eq!(exchanges[0]["kind"], MCP_AUTHORIZATION_PAYLOAD_KEY);
        assert_eq!(exchanges[0]["decision"], "timeout");
        assert_eq!(exchanges[0]["tool_run_id"], "call-9");
        assert_eq!(exchanges[0]["interrupt_id"], request_id.as_str());

        // Answered within its wait, it is an ordinary answer.
        emitter.on_event.lock().unwrap().replace(Box::new({
            let broker = broker.clone();
            move |event| {
                if event.kind == "approval_request" {
                    let id = event.payload["request_id"].as_str().unwrap().to_owned();
                    broker.respond(&id, UiDecision::AllowOnce);
                }
            }
        }));
        let mut answered = command_request(&["approve", "reject"]);
        answered.payload[WAIT_PAYLOAD_KEY] = json!(60_000);
        let outcome = prompt.request(answered).await.unwrap();
        assert!(
            matches!(outcome, ApprovalOutcome::Decided { ref action, .. } if action == "approve")
        );
    }

    #[tokio::test]
    async fn without_a_turn_a_question_is_deferred() {
        let prompt = UiPrompt::new(Arc::new(ApprovalBroker::default()));
        let outcome = prompt.request(command_request(&["approve"])).await.unwrap();
        assert_eq!(outcome, ApprovalOutcome::Deferred);
    }
}
