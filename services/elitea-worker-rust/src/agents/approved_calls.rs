//! One-shot approved calls (ADR-0034 decision 9): the durable ledger behind
//! `elitea_agent_runtime::workspace_tools::ApprovedCallLedger`.
//!
//! A person's approval of a destructive cloud `run_command` (a `git push`, a
//! deploy) resumes the turn with the call replayed approved. When that
//! resumed turn fails after the call ran, but before ADK persisted the tool
//! result, a retry of the resume with the same approval would run the call
//! again. This ledger stops that.
//!
//! The records are content-free events in the turn's own ADK session (the
//! store the resume is resolved from), keyed by the HITL interrupt id of the
//! approval:
//!
//! * `started` is appended BEFORE the call runs. It consumes the approval.
//! * `completed` (with the result) is appended as soon as the call returns,
//!   before ADK yields the result or the turn does anything else that can
//!   fail.
//!
//! The check and the `started` append are atomic per writer: the session
//! append is fenced by the claim's writer lease, so at most one execution can
//! append to the session at a time, and a call runs only after its `started`
//! record is durable. In process, one lock serializes the check and the
//! append.
//!
//! The records carry no `content` and no `state_delta`, so the model history
//! skips them and the direct HITL replay proof ignores them (they are not
//! semantic events).

use std::sync::Arc;

use adk_rust::session::{AppendEventRequest, GetRequest, SessionService};
use adk_rust::{AdkIdentity, Event};
use async_trait::async_trait;
use elitea_agent_runtime::host::{HostError, HostErrorCode};
use elitea_agent_runtime::workspace_tools::{ApprovedCall, ApprovedCallClaim, ApprovedCallLedger};
use serde_json::{Value, json};
use tokio::sync::Mutex;

use super::direct_hitl::sensitive_call_identity;

/// The event author of a ledger record.
pub(crate) const APPROVED_CALL_AUTHOR: &str = "elitea-approved-call";
/// The event metadata key that holds a ledger record (JSON text).
pub(crate) const APPROVED_CALL_METADATA_KEY: &str = "elitea.approved_call.v1";

const PHASE_STARTED: &str = "started";
const PHASE_COMPLETED: &str = "completed";

/// The ledger over one turn's ADK session.
pub(crate) struct SessionApprovedCallLedger {
    sessions: Arc<dyn SessionService>,
    identity: AdkIdentity,
    lock: Mutex<()>,
}

impl SessionApprovedCallLedger {
    #[must_use]
    pub(crate) fn new(sessions: Arc<dyn SessionService>, identity: AdkIdentity) -> Self {
        Self {
            sessions,
            identity,
            lock: Mutex::new(()),
        }
    }

    async fn events(&self) -> Result<Vec<Event>, HostError> {
        let session = self
            .sessions
            .get(GetRequest {
                app_name: self.identity.app_name.as_ref().to_owned(),
                user_id: self.identity.user_id.as_ref().to_owned(),
                session_id: self.identity.session_id.as_ref().to_owned(),
                num_recent_events: None,
                after: None,
            })
            .await
            .map_err(|_| unavailable())?;
        Ok(session.events().all())
    }

    async fn append(
        &self,
        invocation_id: &str,
        interrupt_id: &str,
        phase: &str,
        result: Option<&Value>,
    ) -> Result<(), HostError> {
        let mut record = json!({
            "version": 1,
            "interrupt_id": interrupt_id,
            "phase": phase,
        });
        if let Some(result) = result {
            record["result"] = result.clone();
        }
        let mut event = Event::new(invocation_id);
        APPROVED_CALL_AUTHOR.clone_into(&mut event.author);
        event.provider_metadata.insert(
            APPROVED_CALL_METADATA_KEY.to_owned(),
            serde_json::to_string(&record).map_err(|_| invalid())?,
        );
        self.sessions
            .append_event_for_identity(AppendEventRequest {
                identity: self.identity.clone(),
                event,
            })
            .await
            .map_err(|_| unavailable())
    }
}

/// The interrupt id of the persisted card this approval answers: the latest
/// confirmation of exactly this call.
fn interrupt_for(events: &[Event], call: &ApprovedCall<'_>) -> Result<String, HostError> {
    let confirmation = events
        .iter()
        .rev()
        .find(|event| {
            event
                .actions
                .tool_confirmation
                .as_ref()
                .is_some_and(|request| {
                    request.function_call_id.as_deref() == Some(call.call_id)
                        && request.tool_name == call.tool
                        && &request.args == call.args
                })
        })
        .ok_or_else(invalid)?;
    sensitive_call_identity(
        &confirmation.invocation_id,
        call.call_id,
        call.tool,
        call.args,
    )
    .map(|(interrupt_id, _)| interrupt_id)
    .map_err(|_| invalid())
}

/// The ledger records of one interrupt, in session order.
fn records<'a>(events: &'a [Event], interrupt_id: &'a str) -> impl Iterator<Item = Value> + 'a {
    events
        .iter()
        .filter(|event| event.author == APPROVED_CALL_AUTHOR)
        .filter_map(|event| event.provider_metadata.get(APPROVED_CALL_METADATA_KEY))
        .filter_map(|raw| serde_json::from_str::<Value>(raw).ok())
        .filter(move |record| record["interrupt_id"] == interrupt_id)
}

/// The ledger key: the interrupt id and the invocation that consumed it.
fn encode_key(interrupt_id: &str, invocation_id: &str) -> String {
    format!("{interrupt_id}\n{invocation_id}")
}

fn decode_key(key: &str) -> Result<(&str, &str), HostError> {
    key.split_once('\n').ok_or_else(invalid)
}

#[async_trait]
impl ApprovedCallLedger for SessionApprovedCallLedger {
    async fn claim(&self, call: ApprovedCall<'_>) -> Result<ApprovedCallClaim, HostError> {
        let _serial = self.lock.lock().await;
        let events = self.events().await?;
        let interrupt_id = interrupt_for(&events, &call)?;
        let mut started = false;
        for record in records(&events, &interrupt_id) {
            match record["phase"].as_str() {
                Some(PHASE_COMPLETED) => {
                    return Ok(ApprovedCallClaim::Completed(record["result"].clone()));
                }
                Some(PHASE_STARTED) => started = true,
                _ => {}
            }
        }
        if started {
            return Ok(ApprovedCallClaim::Started);
        }
        // Durable before the call runs: this consumes the approval.
        self.append(call.invocation_id, &interrupt_id, PHASE_STARTED, None)
            .await?;
        Ok(ApprovedCallClaim::Run {
            key: encode_key(&interrupt_id, call.invocation_id),
        })
    }

    async fn complete(&self, key: &str, result: &Value) -> Result<(), HostError> {
        let _serial = self.lock.lock().await;
        let (interrupt_id, invocation_id) = decode_key(key)?;
        self.append(invocation_id, interrupt_id, PHASE_COMPLETED, Some(result))
            .await
            .map_err(|_| {
                HostError::new(
                    HostErrorCode::DependencyUnavailable,
                    "the approved call ran, but its result was not recorded",
                )
            })
    }
}

fn unavailable() -> HostError {
    HostError::new(
        HostErrorCode::DependencyUnavailable,
        "the approval ledger is unavailable; the approved call was not run",
    )
}

fn invalid() -> HostError {
    HostError::new(
        HostErrorCode::InvalidInput,
        "the approved call has no matching persisted approval; it was not run",
    )
}
