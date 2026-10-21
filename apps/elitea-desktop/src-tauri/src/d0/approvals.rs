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

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use elitea_agent_runtime::host::{
    ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError, HostErrorCode,
};
use elitea_agent_runtime::step_limit::{CONTINUE_KIND, CONTINUE_PAYLOAD_KEY};
use elitea_local_tools::approvals::{APPROVE_ALWAYS, PAYLOAD_KEY, ToolCall};
use serde_json::{Value, json};
use tokio::sync::oneshot;

use super::events::TurnEvents;
use super::recorder::Recorder;

/// The payload key a remote toolkit confirmation travels under.
pub const REMOTE_PAYLOAD_KEY: &str = "remote_toolkit_call";

/// The payload key an MCP sign-in question travels under (issue 1231).
pub const MCP_AUTHORIZATION_PAYLOAD_KEY: &str = "mcp_authorization";

/// The payload key of a question's wait, in milliseconds: past it the
/// question is closed and answered [`TIMEOUT_ACTION`].
pub const WAIT_PAYLOAD_KEY: &str = "wait_ms";

/// The action a question closed by its wait is answered with.
pub const TIMEOUT_ACTION: &str = "timeout";

/// The person's answer, as `approval_respond` names it (`continue` and
/// `cancel`, the MCP sign-in card's answers, are `allow_once` and `deny`).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum UiDecision {
    AllowOnce,
    AllowAlways,
    Deny,
}

impl UiDecision {
    #[must_use]
    pub fn parse(value: &str) -> Option<Self> {
        match value {
            "allow_once" | "continue" => Some(Self::AllowOnce),
            "allow_always" => Some(Self::AllowAlways),
            "deny" | "cancel" => Some(Self::Deny),
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
        let described = describe(&request);
        let can_remember = described["can_remember"] == Value::Bool(true);
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
        binding.events.send("approval_request", payload);
        let wait = request
            .payload
            .get(WAIT_PAYLOAD_KEY)
            .and_then(Value::as_u64)
            .map(Duration::from_millis);
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
                        ));
                        return Ok(ApprovalOutcome::Decided {
                            action: TIMEOUT_ACTION.to_owned(),
                            value: json!({ "reason": "nobody answered in time" }),
                        });
                    }
                }
            }
        };
        let approved = decision != UiDecision::Deny;
        binding.recorder.hitl_exchange(exchange_of(
            &request,
            &request_id,
            kind,
            &title,
            if approved { "approve" } else { "reject" },
        ));
        Ok(match decision {
            UiDecision::AllowAlways if can_remember => ApprovalOutcome::Decided {
                action: APPROVE_ALWAYS.to_owned(),
                value: json!({}),
            },
            UiDecision::AllowOnce | UiDecision::AllowAlways => ApprovalOutcome::Decided {
                action: "approve".to_owned(),
                value: json!({}),
            },
            UiDecision::Deny => ApprovalOutcome::Decided {
                action: "reject".to_owned(),
                value: json!({ "reason": "the person rejected it" }),
            },
        })
    }
}

/// One `hitl_exchanges` entry.
fn exchange_of(
    request: &ApprovalRequest,
    request_id: &str,
    kind: &str,
    title: &str,
    decision: &str,
) -> Value {
    let mut exchange = json!({
        "interrupt_id": request_id,
        "kind": kind,
        "prompt": cut(title, 4096),
        "decision": decision,
    });
    // "Continue?" belongs to the turn, not to a tool step.
    if kind != CONTINUE_KIND {
        exchange["tool_run_id"] = Value::String(request.subject.clone());
    }
    exchange
}

fn cut(text: &str, max: usize) -> String {
    if text.len() <= max {
        return text.to_owned();
    }
    let mut end = max;
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    text[..end].to_owned()
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
        let detail = call
            .command
            .clone()
            .unwrap_or_else(|| call.paths.join(", "));
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
    if let Some(remote) = request.payload.get(REMOTE_PAYLOAD_KEY) {
        let text = |key: &str| remote.get(key).and_then(Value::as_str).unwrap_or_default();
        return json!({
            "tool": text("tool"),
            "title": text("title"),
            "detail": text("detail"),
            "reason": text("reason"),
            "can_remember": false,
        });
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
