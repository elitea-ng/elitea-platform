//! The step records the native lifecycle owns (ADR-0042 decision 1, #1499):
//! approval requests and decisions, and the cap or error a run ends on. The
//! projector records the model, tool, retrieval, delegation and output steps
//! (`agents/events_run_steps.rs`).

use serde_json::{Map, Value, json};

use crate::agents::AgentExecutionPayload;
use crate::protocol::elitea::runtime::v1::{NodeEventV1, RunStepKindV1};
use crate::protocol::output::{RuntimeFailureKind, runtime_error_policy};
use crate::protocol::run_steps::{StepDraft, StepText};

/// Whether this generation continues an earlier one, whose model calls its
/// replay adapters re-emit (see `record_model_step`).
#[must_use]
pub(super) fn is_continuation(payload: &AgentExecutionPayload) -> bool {
    payload.should_continue
        || payload.hitl_resume
        || payload.hitl_action.is_some()
        || !payload.hitl_decisions.is_empty()
        || !payload.mcp_tokens.is_empty()
        || !payload.user_declined_mcp_servers.is_empty()
}

fn text_field(object: &Map<String, Value>, key: &str) -> Value {
    object
        .get(key)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .map_or(Value::Null, |value| json!(value))
}

/// The decisions this continuation carries, one approval step each: the
/// person's HITL decisions (`hitl_decisions`, or the single `hitl_action`)
/// and connection authorizations (`mcp_tokens` authorized, declined servers
/// skipped). A reviewer's comment is user content and is not recorded; the
/// decision, the card and the call it answers are.
///
/// Each step id is deterministic (UUID v5 over the command and the decision),
/// so a takeover that re-records the same decisions repeats ids Main already
/// stored and skips.
#[must_use]
pub(super) fn approval_decision_steps(
    payload: &AgentExecutionPayload,
    command_id: &str,
    at_unix_ms: i64,
) -> Vec<StepDraft> {
    let mut steps = Vec::new();
    for (index, decision) in payload
        .hitl_decisions
        .iter()
        .filter_map(Value::as_object)
        .enumerate()
    {
        let action = decision
            .get("action")
            .and_then(Value::as_str)
            .unwrap_or("unknown");
        let key = card_key(decision, index);
        steps.push(
            StepDraft::new(RunStepKindV1::Approval, action, at_unix_ms)
                .stable_id(command_id, &["decision", &key, action])
                .summary("decision", json!(action))
                .summary("interrupt_id", text_field(decision, "interrupt_id"))
                .summary("tool_call_id", text_field(decision, "tool_call_id"))
                .summary("guardrail_type", text_field(decision, "guardrail_type")),
        );
    }
    if steps.is_empty()
        && let Some(action) = payload.hitl_action.as_deref()
    {
        steps.push(
            StepDraft::new(RunStepKindV1::Approval, action, at_unix_ms)
                .stable_id(command_id, &["hitl_action", action])
                .summary("decision", json!(action)),
        );
    }
    for server in payload.mcp_tokens.keys() {
        steps.push(
            StepDraft::new(RunStepKindV1::Approval, "authorize", at_unix_ms)
                .stable_id(command_id, &["mcp_authorize", server])
                .summary("decision", json!("authorize"))
                .summary("guardrail_type", json!("mcp_auth"))
                .summary("server_url", json!(server)),
        );
    }
    for (index, server) in payload.user_declined_mcp_servers.iter().enumerate() {
        let url = server
            .as_str()
            .or_else(|| server.get("url").and_then(Value::as_str))
            .or_else(|| server.get("server_url").and_then(Value::as_str));
        let key = url.map_or_else(|| format!("#{index}"), str::to_owned);
        steps.push(
            StepDraft::new(RunStepKindV1::Approval, "skip", at_unix_ms)
                .stable_id(command_id, &["mcp_skip", &key])
                .summary("decision", json!("skip"))
                .summary("guardrail_type", json!("mcp_auth"))
                .summary("server_url", url.map_or(Value::Null, |url| json!(url))),
        );
    }
    steps
}

/// The approval requests one pause event carries: one step per card, with
/// the (already masked) arguments the card shows. A connection
/// authorization notice is one request too; a fan-out park is not a card.
/// Step ids are deterministic like the decisions' (a re-run pause repeats
/// them).
#[must_use]
pub(super) fn approval_request_steps(
    event: &NodeEventV1,
    command_id: &str,
    at_unix_ms: i64,
) -> Vec<StepDraft> {
    let Ok(Value::Object(metadata)) = serde_json::from_slice::<Value>(&event.response_metadata)
    else {
        return Vec::new();
    };
    if let Some(cards) = metadata.get("hitl_interrupts").and_then(Value::as_array) {
        return cards
            .iter()
            .filter_map(Value::as_object)
            .enumerate()
            .map(|(index, card)| {
                let name = ["tool_name", "node_name"]
                    .iter()
                    .find_map(|key| card.get(*key).and_then(Value::as_str))
                    .unwrap_or("approval");
                let key = card_key(card, index);
                let mut step = StepDraft::new(RunStepKindV1::Approval, name, at_unix_ms)
                    .stable_id(command_id, &["request", &key])
                    .summary("decision", json!("requested"))
                    .summary("interrupt_id", text_field(card, "interrupt_id"))
                    .summary("tool_call_id", text_field(card, "tool_call_id"))
                    .summary("guardrail_type", text_field(card, "guardrail_type"))
                    .summary("toolkit_name", text_field(card, "toolkit_name"));
                if let Some(arguments) = card.get("tool_args") {
                    step.inputs = Some(StepText::json(arguments));
                }
                step
            })
            .collect();
    }
    if metadata.contains_key("fanout_parked_v1") {
        return Vec::new();
    }
    vec![
        StepDraft::new(RunStepKindV1::Approval, "authorization", at_unix_ms)
            .stable_id(command_id, &["authorization_request"])
            .summary("decision", json!("requested"))
            .summary("guardrail_type", json!("mcp_auth")),
    ]
}

/// What identifies one card or decision: its interrupt id, else the tool
/// call it answers, else its position.
fn card_key(object: &Map<String, Value>, index: usize) -> String {
    ["interrupt_id", "tool_call_id"]
        .iter()
        .find_map(|key| {
            object
                .get(*key)
                .and_then(Value::as_str)
                .filter(|value| !value.is_empty())
        })
        .map_or_else(|| format!("#{index}"), str::to_owned)
}

/// The limit a failure kind is, when the run ended on one.
const fn cap_name(kind: RuntimeFailureKind) -> Option<&'static str> {
    match kind {
        RuntimeFailureKind::DeadlineExceeded => Some("deadline"),
        RuntimeFailureKind::OutputContinuationExhausted => Some("output_tokens"),
        RuntimeFailureKind::ModelBudgetExhausted(_) => Some("budget"),
        RuntimeFailureKind::ContextBudgetExceeded => Some("context_window"),
        RuntimeFailureKind::ModelRateLimited => Some("rate_limit"),
        RuntimeFailureKind::ResourceExhausted
        | RuntimeFailureKind::ExecutionInputLimit
        | RuntimeFailureKind::ExecutionInputFieldLimit(_)
        | RuntimeFailureKind::OutputProjectionLimit
        | RuntimeFailureKind::PipelineInputLimit
        | RuntimeFailureKind::PipelineResultLimit
        | RuntimeFailureKind::ModelRequestTooLarge
        | RuntimeFailureKind::ApplicationCompositionTooLarge => Some("resource_limit"),
        _ => None,
    }
}

/// The step a failed run ends on: a cap step for a limit it hit, an error
/// step otherwise. Both carry the registered error code and its safe message.
#[must_use]
pub(super) fn terminal_failure_step(
    kind: RuntimeFailureKind,
    at_unix_ms: i64,
    deadline_unix_ms: i64,
) -> StepDraft {
    let (code, message, retryable) = runtime_error_policy(kind);
    let code = code.as_str_name();
    let step = match cap_name(kind) {
        Some(cap) => StepDraft::new(RunStepKindV1::Cap, cap, at_unix_ms)
            .summary("cap", json!(cap))
            .summary(
                "deadline_unix_ms",
                if matches!(kind, RuntimeFailureKind::DeadlineExceeded) {
                    json!(deadline_unix_ms)
                } else {
                    Value::Null
                },
            ),
        // The violation step names the contract (ADR-0043 decision 2).
        None if matches!(kind, RuntimeFailureKind::OutputContractViolation) => StepDraft::new(
            RunStepKindV1::Error,
            "output_contract_violation",
            at_unix_ms,
        ),
        None => StepDraft::new(RunStepKindV1::Error, code, at_unix_ms),
    };
    step.summary("error_code", json!(code))
        .summary("message", json!(message))
        .summary("retryable", json!(retryable))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_deadline_is_a_cap_and_a_provider_failure_an_error() {
        let cap = terminal_failure_step(RuntimeFailureKind::DeadlineExceeded, 5, 4);
        assert_eq!(cap.kind, RunStepKindV1::Cap);
        assert_eq!(cap.name, "deadline");
        assert_eq!(cap.summary["deadline_unix_ms"], json!(4));
        let error = terminal_failure_step(RuntimeFailureKind::ModelProviderFailure, 5, 4);
        assert_eq!(error.kind, RunStepKindV1::Error);
        assert_eq!(error.summary["error_code"], json!(error.name));
    }

    #[test]
    fn a_pause_card_is_one_requested_approval_with_its_masked_arguments() {
        let event = NodeEventV1 {
            response_metadata: serde_json::to_vec(&json!({
                "hitl_interrupts": [{
                    "interrupt_id": "i-1", "tool_call_id": "c-1", "tool_name": "delete_page",
                    "guardrail_type": "sensitive_tool", "tool_args": {"id": "***"},
                }],
            }))
            .unwrap(),
            ..NodeEventV1::default()
        };
        let steps = approval_request_steps(&event, "command-1", 9);
        let again = approval_request_steps(&event, "command-1", 10);
        assert_eq!(
            steps[0].step_id, again[0].step_id,
            "a re-run pause repeats its id"
        );
        assert_eq!(steps.len(), 1);
        assert_eq!(steps[0].name, "delete_page");
        assert_eq!(steps[0].summary["decision"], json!("requested"));
        assert_eq!(steps[0].summary["tool_call_id"], json!("c-1"));
    }

    /// A takeover re-records the same decisions with the SAME step ids, so
    /// Main's step-id idempotency skips them; another command's ids differ.
    #[test]
    fn approval_decisions_have_replay_stable_step_ids() {
        let mut payload = crate::agents::assembly_tests::ordinary_request(
            crate::agents::AgentExecutionKind::Adhoc,
        )
        .payload;
        payload.hitl_decisions = vec![
            json!({"interrupt_id": "i-1", "tool_call_id": "c-1", "action": "approve"}),
            json!({"interrupt_id": "i-2", "tool_call_id": "c-2", "action": "reject"}),
        ];
        payload
            .mcp_tokens
            .insert("https://mcp.example".to_owned(), json!("token"));
        payload.user_declined_mcp_servers = vec![json!("https://other.example")];
        let ids = |command: &str, at: i64| {
            approval_decision_steps(&payload, command, at)
                .into_iter()
                .map(|step| step.step_id.expect("a stable id"))
                .collect::<Vec<_>>()
        };
        let first = ids("command-1", 5);
        assert_eq!(first.len(), 4);
        assert_eq!(first, ids("command-1", 6), "replayed decisions repeat ids");
        let unique: std::collections::HashSet<&String> = first.iter().collect();
        assert_eq!(unique.len(), 4, "each decision its own id");
        assert!(
            first.iter().all(|id| !ids("command-2", 5).contains(id)),
            "another command's decisions are other steps"
        );
        assert!(first.iter().all(|id| uuid::Uuid::parse_str(id).is_ok()));
    }
}
