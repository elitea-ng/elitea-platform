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
                    .map_err(|error| storage_failure(&error.code))?;
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
