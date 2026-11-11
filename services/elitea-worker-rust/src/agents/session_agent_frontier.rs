//! FU-323a: a root Agent node running a saved Agent as a recovery frontier.
//!
//! The inspect step proves where that Agent stopped from two durable records
//! the run already owns, both read under this claim's fenced authority:
//!
//! 1. the root row's call receipt for the node (`scope_receipts.rs`), which
//!    must be this pipeline's own call of its frozen saved Agent at the
//!    pending step;
//! 2. the Agent's own model scope (`model_scope.rs` `inspect_agent_scope`).
//!
//! Every state that cannot be proven is refused typed, with an operator cause
//! per check and nothing repeated. The proof is bound into the evidence Main
//! authorizes (`recovery.v3`), and assembly re-enters the node only for the
//! proven `(node, step, call_id)` (`scoped_runtime.rs` `RootAgentReentry`).

use std::collections::BTreeMap;

use adk_rust::graph::{Checkpoint, GraphError};
use serde_json::{Value, json};

use super::{
    NativeAgentAssemblyError, NativeAgentAssemblyErrorCode, OrdinaryNativeAgentPlan,
    PipelineDefinition, PipelineStateServices, dependency_unavailable, invalid_configuration,
};
use crate::agents::model_scope::{AgentScopeRefusal, AgentScopeState};
use crate::agents::pipeline::scope_receipts::{GraphCallOutcome, receipt_for_node};
use crate::agents::pipeline::scoped_applications::PipelineApplicationScope;

/// The frozen saved Agent an admitted root Agent node runs: its alias and the
/// application version Main froze into the command (`(application, version)`).
pub(crate) type FrozenAgents = BTreeMap<String, (u64, u64)>;

/// Where one root saved-Agent node stopped, as the inspect step proved it.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct AgentFrontierProof {
    node: String,
    alias: String,
    application: (u64, u64),
    step: usize,
    /// The receipt's activation call id; `None` before the node prepared its
    /// call at this step.
    call_id: Option<String>,
    class: AgentFrontierClass,
}

/// The proven class of a root saved-Agent node's stop.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) enum AgentFrontierClass {
    /// No receipt at this step: the node never started its Agent. It runs
    /// once, fresh.
    NotStarted,
    /// The receipt is complete: the result is served from it.
    Completed,
    /// The receipt started and the Agent persisted no model step: nothing was
    /// sent, so the Agent runs fresh.
    Fresh,
    /// An unfinished model step of this execution (its checkpoint digest):
    /// the restored request is re-issued once.
    Restorable([u8; 32]),
    /// The Agent's answer is persisted with its receipt (the event id): it is
    /// served without a model call.
    Answered(String),
}

impl AgentFrontierProof {
    /// The re-entry this proof admits: a started receipt whose Agent can
    /// continue from its own scope.
    pub(crate) fn reentry(&self) -> Option<(&str, usize, &str)> {
        match self.class {
            AgentFrontierClass::Fresh
            | AgentFrontierClass::Restorable(_)
            | AgentFrontierClass::Answered(_) => self
                .call_id
                .as_deref()
                .map(|call_id| (self.node.as_str(), self.step, call_id)),
            AgentFrontierClass::NotStarted | AgentFrontierClass::Completed => None,
        }
    }

    /// The evidence of this proof Main's grant binds (`recovery.v3`): ids,
    /// the class and a checkpoint digest or event id. No content.
    pub(crate) fn evidence(&self) -> Value {
        let (class, bound) = match &self.class {
            AgentFrontierClass::NotStarted => ("not_started", Value::Null),
            AgentFrontierClass::Completed => ("completed", Value::Null),
            AgentFrontierClass::Fresh => ("fresh", Value::Null),
            AgentFrontierClass::Restorable(digest) => ("restorable", json!(digest)),
            AgentFrontierClass::Answered(event_id) => ("answered", json!(event_id)),
        };
        json!({
            "node": self.node,
            "alias": self.alias,
            "application": [self.application.0, self.application.1],
            "step": self.step,
            "call_id": self.call_id,
            "class": class,
            "bound": bound,
        })
    }

    #[cfg(test)]
    pub(crate) fn for_tests(
        node: &str,
        step: usize,
        call_id: Option<&str>,
        class: AgentFrontierClass,
    ) -> Self {
        Self {
            node: node.to_owned(),
            alias: "release-agent".to_owned(),
            application: (3, 4),
            step,
            call_id: call_id.map(str::to_owned),
            class,
        }
    }
}

impl PipelineStateServices {
    /// Prove where the root Agent node `node`, running the saved Agent
    /// `application` under `alias`, stopped in `checkpoint` (this execution's
    /// own latest root row). Read only; a refusal logs ids and the cause only.
    #[allow(clippy::too_many_lines)] // One ordered proof: receipt, then the Agent's own scope, each refusal typed.
    pub(super) async fn prove_agent_frontier(
        &self,
        plan: &OrdinaryNativeAgentPlan,
        definition: &PipelineDefinition,
        checkpoint: &Checkpoint,
        node: &str,
        alias: &str,
        application: (u64, u64),
    ) -> Result<AgentFrontierProof, NativeAgentAssemblyError> {
        let refuse = |code: elitea_faults::Code| {
            elitea_faults::fault!(
                tracing::Level::WARN,
                code = code,
                event = "pipeline_recovery_frontier_refused",
                execution_id = %plan.execution_id,
                generation = plan.generation,
                node_id = node,
                support_ref = %plan.execution_id
            );
            invalid_configuration().with_cause(code.as_str(), None)
        };
        let mut proof = AgentFrontierProof {
            node: node.to_owned(),
            alias: alias.to_owned(),
            application,
            step: checkpoint.step,
            call_id: None,
            class: AgentFrontierClass::NotStarted,
        };
        let receipt = receipt_for_node(checkpoint, node)
            .map_err(|_| refuse(elitea_faults::codes::PIPELINE_RECOVERY_AGENT_RECEIPT_MISMATCH))?;
        // No receipt, or one of an earlier visit of the node: this visit has
        // not prepared its call, so its Agent never started here.
        let Some(receipt) =
            receipt.filter(|receipt| receipt.activation().step() == checkpoint.step)
        else {
            return Ok(proof);
        };
        let agent_name = crate::agents::application_tools::application_tool_name(application);
        let own = PipelineApplicationScope::new(String::new(), definition, None)
            .and_then(|scope| scope.activate(&checkpoint.thread_id, node, checkpoint.step))
            .is_ok_and(|expected| *receipt.activation() == expected)
            && receipt
                .original_start()
                .tool_calls()
                .first()
                .is_some_and(|call| call.name == agent_name);
        if !own {
            return Err(refuse(
                elitea_faults::codes::PIPELINE_RECOVERY_AGENT_RECEIPT_MISMATCH,
            ));
        }
        proof.call_id = Some(receipt.activation().call_id().to_owned());
        proof.class = match receipt.outcome() {
            GraphCallOutcome::Completed { .. } => AgentFrontierClass::Completed,
            GraphCallOutcome::Paused { .. } | GraphCallOutcome::Resuming { .. } => {
                return Err(refuse(
                    elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_PAUSED,
                ));
            }
            GraphCallOutcome::Started => {
                let child = crate::agents::application_tools::child_instruction_session_id(
                    &checkpoint.thread_id,
                    receipt.activation().call_id(),
                    &agent_name,
                );
                let state = self
                    .model_scopes
                    .inspect_agent_scope(plan.session_identity()?, &child, &agent_name)
                    .await
                    .map_err(|error| storage_failure(error.code))?;
                match state {
                    AgentScopeState::Absent => AgentFrontierClass::Fresh,
                    AgentScopeState::Restorable(digest) => AgentFrontierClass::Restorable(digest),
                    AgentScopeState::Answered(event_id) => AgentFrontierClass::Answered(event_id),
                    AgentScopeState::MayHaveStarted => {
                        let code =
                            elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_MAY_HAVE_STARTED;
                        elitea_faults::fault!(
                            tracing::Level::WARN,
                            code = code,
                            event = "pipeline_recovery_frontier_refused",
                            execution_id = %plan.execution_id,
                            generation = plan.generation,
                            node_id = node,
                            support_ref = %plan.execution_id
                        );
                        // The registered "may have started, not repeated" end.
                        return Err(NativeAgentAssemblyError::new(
                            NativeAgentAssemblyErrorCode::UnsupportedCapability,
                            "a tool call of the agent may have started",
                        )
                        .with_cause(
                            crate::protocol::output::RECOVERY_REFUSED_UPSTREAM_CODE,
                            None,
                        ));
                    }
                    AgentScopeState::Refused(refusal) => {
                        return Err(refuse(match refusal {
                            AgentScopeRefusal::Invalid => {
                                elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_INVALID
                            }
                            AgentScopeRefusal::ForeignExecution => {
                                elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_FOREIGN_EXECUTION
                            }
                            AgentScopeRefusal::SessionWithoutCheckpoint => {
                                elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_SESSION_WITHOUT_CHECKPOINT
                            }
                        }));
                    }
                }
            }
        };
        Ok(proof)
    }
}

/// The causes an Agent-frontier refusal carries: each ends the run with the
/// Agent-step text (`RuntimeFailureKind::RecoveryFrontierAgentStep`).
pub(crate) fn is_agent_frontier_cause(code: &str) -> bool {
    [
        elitea_faults::codes::PIPELINE_RECOVERY_AGENT_RECEIPT_MISMATCH,
        elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_PAUSED,
        elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_INVALID,
        elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_FOREIGN_EXECUTION,
        elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_SESSION_WITHOUT_CHECKPOINT,
    ]
    .iter()
    .any(|cause| cause.as_str() == code)
}

/// A storage failure of an inspect read (D-6, C2): only a transient one
/// (connection, timeout, serialization or deadlock, lock not available: the
/// stores' own `storage_unavailable`) carries the retained cause; any other
/// stays the terminal `DependencyUnavailable` it was.
pub(super) fn storage_failure(code: &str) -> NativeAgentAssemblyError {
    if matches!(
        code,
        "session.storage_unavailable" | "checkpoint.storage_unavailable"
    ) {
        dependency_unavailable().with_cause(
            elitea_faults::codes::PIPELINE_RECOVERY_INSPECT_TRANSIENT.as_str(),
            None,
        )
    } else {
        dependency_unavailable()
    }
}

/// [`storage_failure`] for a graph checkpoint read: the checkpointer puts its
/// stable code first in the message (`postgres_checkpointer.rs`).
pub(super) fn checkpoint_load_failure(error: &GraphError) -> NativeAgentAssemblyError {
    match error {
        GraphError::CheckpointError(message) => {
            storage_failure(message.split(':').next().unwrap_or_default())
        }
        _ => dependency_unavailable(),
    }
}

/// Whether `error` is a transient inspect failure the reclaim retains.
pub(crate) fn is_transient_inspect_failure(error: &NativeAgentAssemblyError) -> bool {
    error.code() == NativeAgentAssemblyErrorCode::DependencyUnavailable
        && error.cause().is_some_and(|cause| {
            cause.code() == elitea_faults::codes::PIPELINE_RECOVERY_INSPECT_TRANSIENT.as_str()
        })
}

#[cfg(test)]
mod tests {
    use super::super::recovery_evidence_digest;
    use super::*;
    use crate::agents::pipeline::scoped_runtime::RootAgentReentry;
    use crate::protocol::output::RuntimeFailureKind;

    fn row() -> Checkpoint {
        serde_json::from_value(json!({
            "thread_id": "root-thread",
            "checkpoint_id": "cp-1",
            "state": {"answer": "x"},
            "step": 2,
            "pending_nodes": ["delegate"],
            "metadata": {"elitea.pipeline.execution.v1": ["execution-1", 3]},
            "created_at": "2026-11-11T00:00:00Z"
        }))
        .expect("a root row")
    }

    /// The evidence formula of `main` before FU-323a (`session.rs`
    /// `recovery_evidence` at b6633e6e6), restated here: v1 and v2 must stay
    /// byte-identical (D1 a, U3).
    fn base_digest(checkpoint: &Checkpoint, children: &[Value]) -> [u8; 32] {
        let value = serde_json::to_value(checkpoint).expect("row value");
        let bytes = if children.is_empty() {
            serde_json::to_vec(&("elitea.pipeline.recovery.v1", [7_u8; 32], value))
        } else {
            serde_json::to_vec(&("elitea.pipeline.recovery.v2", [7_u8; 32], value, children))
        }
        .expect("bytes");
        ring::digest::digest(&ring::digest::SHA256, &bytes)
            .as_ref()
            .try_into()
            .expect("32 bytes")
    }

    fn digest(children: &[Value], agents: &[AgentFrontierProof]) -> [u8; 32] {
        recovery_evidence_digest(
            "execution-1".to_owned(),
            3,
            [7; 32],
            &row(),
            children.to_vec(),
            agents,
        )
        .expect("evidence")
        .digest()
    }

    fn hex(bytes: [u8; 32]) -> String {
        bytes.iter().fold(String::new(), |mut out, byte| {
            use std::fmt::Write as _;
            let _ = write!(out, "{byte:02x}");
            out
        })
    }

    #[test]
    fn v1_and_v2_evidence_digests_are_unchanged() {
        let child = [json!({"thread_id": "root-thread/delegate", "step": 1})];
        assert_eq!(digest(&[], &[]), base_digest(&row(), &[]));
        assert_eq!(digest(&child, &[]), base_digest(&row(), &child));
        // Golden values: a change to the v1/v2 inputs fails here.
        assert_eq!(hex(digest(&[], &[])), GOLDEN_V1);
        assert_eq!(hex(digest(&child, &[])), GOLDEN_V2);
    }

    const GOLDEN_V1: &str = "f753e5d98700420543cd1d2fbe1fca2582dce5b2b5f7cb5e6d1f454ee9825c23";
    const GOLDEN_V2: &str = "35b91e61b6154a76b3393cfe492b5c9aed110131c79ab0865520792e1c9955ef";

    #[test]
    fn v3_evidence_binds_every_field_of_the_agent_proof() {
        let proof = |step, call: Option<&str>, class| {
            AgentFrontierProof::for_tests("delegate", step, call, class)
        };
        let base = proof(2, Some("call-1"), AgentFrontierClass::Restorable([1; 32]));
        let reference = digest(&[], std::slice::from_ref(&base));
        assert_eq!(reference, digest(&[], std::slice::from_ref(&base)));
        assert_ne!(reference, digest(&[], &[]), "v3 is not v1");
        let mut changed = vec![
            proof(3, Some("call-1"), AgentFrontierClass::Restorable([1; 32])),
            proof(2, Some("call-2"), AgentFrontierClass::Restorable([1; 32])),
            proof(2, Some("call-1"), AgentFrontierClass::Restorable([2; 32])),
            proof(
                2,
                Some("call-1"),
                AgentFrontierClass::Answered("event-1".into()),
            ),
            proof(2, Some("call-1"), AgentFrontierClass::Fresh),
            proof(2, None, AgentFrontierClass::NotStarted),
            proof(2, Some("call-1"), AgentFrontierClass::Completed),
        ];
        let mut other_agent = base.clone();
        other_agent.application = (3, 5);
        changed.push(other_agent);
        let mut other_alias = base.clone();
        other_alias.alias = "other-agent".into();
        changed.push(other_alias);
        let mut other_node = base.clone();
        other_node.node = "other".into();
        changed.push(other_node);
        for proof in &changed {
            assert_ne!(
                digest(&[], std::slice::from_ref(proof)),
                reference,
                "{proof:?}"
            );
        }
    }

    /// U5 / C1: only a started receipt whose Agent can continue admits a
    /// re-entry, and each proven re-entry is taken once.
    #[test]
    fn a_root_reentry_is_proof_bound_and_taken_once() {
        let proofs = [
            AgentFrontierProof::for_tests(
                "delegate",
                2,
                Some("call-1"),
                AgentFrontierClass::Restorable([1; 32]),
            ),
            AgentFrontierProof::for_tests("second", 2, None, AgentFrontierClass::NotStarted),
            AgentFrontierProof::for_tests(
                "third",
                2,
                Some("call-3"),
                AgentFrontierClass::Completed,
            ),
        ];
        let reentry = RootAgentReentry::from_proofs(&proofs).expect("one admitted re-entry");
        assert!(!reentry.take("delegate", 3, "call-1"), "another step");
        assert!(!reentry.take("delegate", 2, "call-9"), "another call");
        assert!(!reentry.take("third", 2, "call-3"), "a completed receipt");
        assert!(reentry.take("delegate", 2, "call-1"));
        assert!(
            !reentry.take("delegate", 2, "call-1"),
            "a second re-entry within the claim takes today's refusal"
        );
        for class in [
            AgentFrontierClass::Fresh,
            AgentFrontierClass::Answered("event-1".into()),
        ] {
            let proof = AgentFrontierProof::for_tests("delegate", 2, Some("call-1"), class);
            assert!(RootAgentReentry::from_proofs(&[proof]).is_some());
        }
        assert!(RootAgentReentry::from_proofs(&proofs[1..]).is_none());
        assert!(RootAgentReentry::from_proofs(&[]).is_none());
    }

    /// C2: only the stores' transient failure is retained; everything else
    /// stays the terminal it was.
    #[test]
    fn only_a_transient_storage_failure_is_retained() {
        assert!(is_transient_inspect_failure(&storage_failure(
            "session.storage_unavailable"
        )));
        for code in [
            "session.storage_failure",
            "session.corrupt_state",
            "session.invalid_scope",
            "session.writer_not_current",
            "checkpoint.corrupt_state",
        ] {
            let error = storage_failure(code);
            assert!(!is_transient_inspect_failure(&error), "{code}");
            assert_eq!(
                error.code(),
                NativeAgentAssemblyErrorCode::DependencyUnavailable
            );
        }
        assert!(is_transient_inspect_failure(&checkpoint_load_failure(
            &GraphError::CheckpointError(
                "checkpoint.storage_unavailable: PostgreSQL checkpoint storage is temporarily unavailable"
                    .to_owned()
            )
        )));
        for error in [
            GraphError::CheckpointError("checkpoint.corrupt_state: malformed".to_owned()),
            GraphError::CheckpointError("checkpoint.storage_failure: failed".to_owned()),
            GraphError::CheckpointError("checkpoint.writer_not_current: lost".to_owned()),
        ] {
            assert!(!is_transient_inspect_failure(&checkpoint_load_failure(
                &error
            )));
        }
        // A refusal is never retained, whatever its cause.
        assert!(!is_transient_inspect_failure(
            &invalid_configuration().with_cause(
                elitea_faults::codes::PIPELINE_RECOVERY_INSPECT_TRANSIENT.as_str(),
                None
            )
        ));
    }

    /// D5: every Agent-step refusal ends with the Agent-step text; the
    /// "may have started" one with the registered recovery-refused text.
    #[test]
    fn every_agent_frontier_refusal_ends_with_its_registered_kind() {
        for code in [
            elitea_faults::codes::PIPELINE_RECOVERY_AGENT_RECEIPT_MISMATCH,
            elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_PAUSED,
            elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_INVALID,
            elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_FOREIGN_EXECUTION,
            elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_SESSION_WITHOUT_CHECKPOINT,
        ] {
            assert!(is_agent_frontier_cause(code.as_str()));
            assert_eq!(
                crate::execution::assembly_failure(
                    &invalid_configuration().with_cause(code.as_str(), None)
                ),
                RuntimeFailureKind::RecoveryFrontierAgentStep,
                "{code:?}"
            );
        }
        assert!(!is_agent_frontier_cause(
            elitea_faults::codes::PIPELINE_RECOVERY_INSPECT_TRANSIENT.as_str()
        ));
        assert_eq!(
            crate::execution::assembly_failure(
                &NativeAgentAssemblyError::new(
                    NativeAgentAssemblyErrorCode::UnsupportedCapability,
                    "a tool call of the agent may have started",
                )
                .with_cause(
                    crate::protocol::output::RECOVERY_REFUSED_UPSTREAM_CODE,
                    None
                )
            ),
            RuntimeFailureKind::RecoveryRefused
        );
    }
}
