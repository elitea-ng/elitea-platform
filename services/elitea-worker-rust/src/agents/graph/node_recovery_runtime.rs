//! Attempt dispatch follows a fenced durable Started append.

use std::collections::BTreeMap;
use std::sync::Arc;

use adk_rust::graph::{Checkpoint, GraphError, Node, NodeContext, NodeOutput, State};
use async_trait::async_trait;
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::Value;

use super::node_recovery::codec;
use super::node_recovery::{
    NodeAttemptLedger, NodeAttemptPhase, NodeFailure, NodeFailureClass, NodeRecoveryPolicy,
    RecoveryDecision,
};
use super::parallel::ParallelCheckpointAppender;
use crate::state::StateWriterLease;

const JOURNAL_KEY: &str = "elitea.pipeline.node-attempt-journal.v1";
const MAX_RESULT_BYTES: usize = 1024 * 1024;

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct NodeAttemptActivation {
    pub(crate) root_thread_id: String,
    pub(crate) node_id: String,
    pub(crate) step: u64,
    pub(crate) node_digest: [u8; 32],
    pub(crate) input_digest: [u8; 32],
}

impl NodeAttemptActivation {
    pub(crate) fn from_context(
        node: &str,
        node_digest: [u8; 32],
        context: &NodeContext,
    ) -> Result<Self, GraphError> {
        if !super::yaml::valid_graph_id(node)
            || context.config.thread_id.is_empty()
            || context.config.thread_id.len() > 512
        {
            return Err(recovery_error("pipeline.node_recovery.invalid_activation"));
        }
        let input = serde_json::to_value(&context.state)
            .map_err(|_| recovery_error("pipeline.node_recovery.invalid_input"))?;
        let bytes = serde_json::to_vec(&input)
            .map_err(|_| recovery_error("pipeline.node_recovery.invalid_input"))?;
        if bytes.len() > 8 * 1024 * 1024 {
            return Err(recovery_error("pipeline.node_recovery.input_limit"));
        }
        Ok(Self {
            root_thread_id: context.config.thread_id.clone(),
            node_id: node.to_owned(),
            step: u64::try_from(context.step)
                .map_err(|_| recovery_error("pipeline.node_recovery.invalid_activation"))?,
            node_digest,
            input_digest: sha256(&bytes),
        })
    }
}

/// Only the journal owner creates this value after a durable Started record.
pub(crate) struct NodeAttemptAuthority {
    node_id: String,
    node_digest: [u8; 32],
    step: u64,
    attempt: u16,
    activation: [u8; 32],
    dispatch_activation: [u8; 32],
    recovering_started: bool,
    recovery_receipt_sha256: Option<[u8; 32]>,
}

impl NodeAttemptAuthority {
    fn committed(
        activation: &NodeAttemptActivation,
        scoped_activation: [u8; 32],
        attempt: u16,
        recovering_started: bool,
    ) -> Self {
        let mut hash = digest::Context::new(&digest::SHA256);
        hash.update(b"elitea.pipeline.node-attempt-dispatch.v1\0");
        hash.update(&scoped_activation);
        hash.update(&attempt.to_be_bytes());
        Self {
            node_id: activation.node_id.clone(),
            node_digest: activation.node_digest,
            step: activation.step,
            attempt,
            activation: scoped_activation,
            dispatch_activation: finish(hash),
            recovering_started,
            recovery_receipt_sha256: None,
        }
    }
    pub(crate) fn matches(&self, node: &str, digest: [u8; 32], step: usize) -> bool {
        self.node_id == node
            && self.node_digest == digest
            && u64::try_from(step).ok() == Some(self.step)
            && self.attempt > 0
    }
    pub(crate) const fn dispatch_activation(&self) -> [u8; 32] {
        self.dispatch_activation
    }
    pub(crate) const fn recovering_started(&self) -> bool {
        self.recovering_started
    }
    pub(crate) const fn logical_activation(&self) -> [u8; 32] {
        self.activation
    }
    pub(crate) const fn node_digest(&self) -> [u8; 32] {
        self.node_digest
    }
    pub(crate) const fn attempt(&self) -> u16 {
        self.attempt
    }
    pub(crate) const fn recovery_receipt_sha256(&self) -> Option<[u8; 32]> {
        self.recovery_receipt_sha256
    }
}

/// Ephemeral safe diagnostics do not change the durable replay contract.
pub(crate) struct NodeAttemptReportedFailure {
    pub(crate) failure: NodeFailure,
    pub(crate) terminal_code: Option<&'static str>,
}

impl From<NodeFailure> for NodeAttemptReportedFailure {
    fn from(failure: NodeFailure) -> Self {
        Self {
            failure,
            terminal_code: None,
        }
    }
}

#[async_trait]
pub(crate) trait NodeAttemptBody: Node {
    /// Only the migration of an omitted stop policy may retain this identity.
    /// Explicit retry policies cannot inherit an old uncertain effect.
    fn legacy_dispatch_identity(&self, _: &NodeContext) -> Result<Option<[u8; 32]>, GraphError> {
        Ok(None)
    }
    /// Reuse the same authority to reconcile an interrupted attempt.
    async fn execute_attempt(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
    ) -> Result<NodeOutput, NodeFailure>;

    /// Preserve an optional safe cause until the failure append commits.
    async fn execute_attempt_reported(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
    ) -> Result<NodeOutput, NodeAttemptReportedFailure> {
        self.execute_attempt(context, authority)
            .await
            .map_err(Into::into)
    }

    /// Report only an already committed terminal stop. Pauses and retries retain control.
    async fn report_terminal_failure(
        &self,
        _: &NodeContext,
        _: &NodeAttemptAuthority,
        _: &'static str,
    ) -> Result<(), GraphError> {
        Ok(())
    }

    /// Project a validated terminal class. Never execute, reconcile, or change authority.
    async fn report_restored_terminal_failure(
        &self,
        _: &NodeContext,
        _: NodeFailureClass,
    ) -> Result<(), GraphError> {
        Ok(())
    }
}

/// Read-only owning projection. It must never invoke an external operation.
pub(crate) trait NodeResultRecovery: Node {
    fn verify_no_effect_receipt(
        &self,
        _: &NodeContext,
        _: &NodeAttemptAuthority,
        _: &super::node_recovery_owner::NodeRecoveryOwnerProof,
        _: &[u8],
    ) -> Result<(), NodeFailure> {
        Err(NodeFailure::new(
            NodeFailureClass::AuthorizationDenied,
            super::node_recovery::ReplaySafety::NoExternalEffect,
        ))
    }
    fn project_committed_result(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
        proof: &super::node_recovery_owner::NodeRecoveryOwnerProof,
        receipt_wire: &[u8],
    ) -> Result<NodeOutput, NodeFailure>;
}

#[async_trait]
pub(crate) trait NodeRecoveryFactory: Send + Sync {
    async fn open(
        &self,
        activation: &NodeAttemptActivation,
        policy: &NodeRecoveryPolicy,
    ) -> Result<NodeAttemptJournal, GraphError>;
}

#[async_trait]
pub(crate) trait NodeRecoveryOperatorAuthorizer: Send + Sync {
    /// Authenticate the server-owned action and exact current receipt/fence.
    /// Business state and browser fields alone are never operator authority.
    async fn authorize_retry(
        &self,
        receipt: &super::node_recovery_receipt::NodeRecoveryRequiredReceipt,
        request: &super::node_recovery::OperatorRetryRequest,
    ) -> Result<(), GraphError>;
}

#[async_trait]
pub(crate) trait NodeRecoveryOwnerAuthorizer: Send + Sync {
    async fn authorize_verified_no_effect(
        &self,
        _: &super::node_recovery_receipt::NodeRecoveryRequiredReceipt,
        _: &super::node_recovery::OperatorRetryRequest,
        _: &super::node_recovery_owner::NodeRecoveryOwnerProof,
    ) -> Result<(), GraphError> {
        Err(recovery_error(
            "pipeline.node_recovery.no_effect_authority_missing",
        ))
    }
    async fn authorize_committed_result(
        &self,
        receipt: &super::node_recovery_receipt::NodeRecoveryRequiredReceipt,
        request: &super::node_recovery::OperatorRetryRequest,
        proof: &super::node_recovery_owner::NodeRecoveryOwnerProof,
    ) -> Result<(), GraphError>;
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct JournalRecord {
    schema: String,
    ledger: Value,
    // Output remains private checkpoint data. Never log this record.
    completed_updates: Option<BTreeMap<String, Value>>,
}

pub(crate) struct NodeAttemptJournal {
    checkpoints: Arc<dyn ParallelCheckpointAppender>,
    lease: Arc<dyn StateWriterLease>,
    thread_id: String,
    activation_id: [u8; 32],
    policy: NodeRecoveryPolicy,
}

pub(crate) struct NodeJournalSnapshot {
    checkpoint: Option<Checkpoint>,
    ledger: NodeAttemptLedger,
    updates: Option<BTreeMap<String, Value>>,
}

impl NodeAttemptJournal {
    /// Apply one exact owner tombstone. This grants no attempt execution.
    #[allow(
        clippy::too_many_arguments,
        reason = "Keep the existing authenticated identity fields explicit."
    )]
    #[allow(
        clippy::too_many_lines,
        reason = "Keep ordered authority checks and durable phases in one owner operation."
    )]
    pub(crate) async fn reconcile_verified_no_effect(
        &self,
        activation: &NodeAttemptActivation,
        context: &NodeContext,
        request: super::node_recovery::OperatorRetryRequest,
        proof: &super::node_recovery_owner::NodeRecoveryOwnerProof,
        owner_wire: &[u8],
        projector: &dyn NodeResultRecovery,
        authorizer: &dyn NodeRecoveryOwnerAuthorizer,
        now_ms: u64,
    ) -> Result<AppliedNodeRecoveryAction, GraphError> {
        self.ensure_current()?;
        if *activation
            != NodeAttemptActivation::from_context(
                &activation.node_id,
                activation.node_digest,
                context,
            )?
        {
            return Err(recovery_error(
                "pipeline.node_recovery.result_context_mismatch",
            ));
        }
        let snapshot = self.load().await?;
        let prior = codec::at_revision(&snapshot.ledger, request.expected_revision)
            .map_err(policy_error)?;
        let receipt = super::node_recovery_receipt::NodeRecoveryRequiredReceipt::from_ledger(
            activation, &prior,
        )
        .ok_or_else(|| recovery_error("pipeline.node_recovery.no_effect_denied"))?;
        if request.activation_id != self.activation_id
            || !receipt.matches(activation, &prior)
            || proof.kind
                != super::node_recovery_owner::NodeRecoveryOwnerProofKind::VerifiedNoEffect
            || !proof.validates(&proof.execution_id, proof.generation, &receipt)
            || !proof.matches_owner_receipt_bytes(owner_wire)
        {
            return Err(recovery_error("pipeline.node_recovery.no_effect_denied"));
        }
        authorizer
            .authorize_verified_no_effect(&receipt, &request, proof)
            .await?;
        self.ensure_current()?;
        let mut authority =
            NodeAttemptAuthority::committed(activation, self.activation_id, receipt.attempt, true);
        authority.recovery_receipt_sha256 = Some(recovery_receipt_hash(&receipt)?);
        projector
            .verify_no_effect_receipt(context, &authority, proof, owner_wire)
            .map_err(|_| recovery_error("pipeline.node_recovery.no_effect_invalid"))?;
        let effect = parse_hex_id(&proof.effect_id)?;
        let owner_hash = parse_hex_id(&proof.owner_receipt_sha256)?;
        let applied = if snapshot.ledger.revision() == request.expected_revision {
            let next = snapshot
                .ledger
                .reconcile_verified_no_effect(request, effect, owner_hash, now_ms)
                .map_err(policy_error)?;
            self.append(&snapshot, next, None).await?
        } else {
            if !codec::is_no_effect_replay(&snapshot.ledger, request, effect, owner_hash)
                .map_err(policy_error)?
            {
                return Err(recovery_error(
                    "pipeline.node_recovery.stale_operator_action",
                ));
            }
            snapshot
        };
        let mut result = AppliedNodeRecoveryAction::retry(request, applied.ledger.revision());
        match applied.ledger.phase() {
            NodeAttemptPhase::Failed(RecoveryDecision::Stop(
                super::node_recovery::StopReason::OperatorApprovalRequired,
            )) => {
                let continuation =
                    super::node_recovery_receipt::NodeRecoveryRequiredReceipt::from_ledger(
                        activation,
                        &applied.ledger,
                    )
                    .ok_or_else(|| {
                        recovery_error("pipeline.node_recovery.no_effect_continuation_invalid")
                    })?;
                if !continuation.validate() {
                    return Err(recovery_error(
                        "pipeline.node_recovery.no_effect_continuation_invalid",
                    ));
                }
                result.continuation_receipt = Some(continuation);
            }
            NodeAttemptPhase::Failed(RecoveryDecision::Stop(
                reason @ (super::node_recovery::StopReason::AttemptsExhausted
                | super::node_recovery::StopReason::ElapsedLimit
                | super::node_recovery::StopReason::NotRetryable
                | super::node_recovery::StopReason::RetryDisabled),
            )) => result.terminal_stop_reason = Some(reason),
            NodeAttemptPhase::Failed(RecoveryDecision::ErrorRoute { route_id, failed }) => {
                result.failure_route_continuation = Some(NodeRecoveryFailureRouteContinuation {
                    schema: "elitea.pipeline.node-recovery-failure-route.v1".into(),
                    route_id: hex_id(&route_id),
                    failed: NodeRecoveryFailureRouteFailure {
                        activation_id: hex_id(&failed.activation_id),
                        attempt: failed.attempt,
                        failure_class: failed.class,
                        stop_reason: failed.reason,
                    },
                });
            }
            _ => {
                return Err(recovery_error(
                    "pipeline.node_recovery.no_effect_continuation_invalid",
                ));
            }
        }
        Ok(result)
    }
    /// Owner-backed result restoration never dispatches another attempt.
    /// The owning node validates and projects exact immutable receipt bytes.
    #[allow(
        clippy::too_many_arguments,
        reason = "Keep the existing authenticated identity fields explicit."
    )]
    pub(crate) async fn resume_owner_result(
        &self,
        activation: &NodeAttemptActivation,
        context: &NodeContext,
        request: super::node_recovery::OperatorRetryRequest,
        proof: &super::node_recovery_owner::NodeRecoveryOwnerProof,
        receipt_wire: &[u8],
        projector: &dyn NodeResultRecovery,
        authorizer: &dyn NodeRecoveryOwnerAuthorizer,
        now_ms: u64,
    ) -> Result<AppliedNodeRecoveryAction, GraphError> {
        self.ensure_current()?;
        if *activation
            != NodeAttemptActivation::from_context(
                &activation.node_id,
                activation.node_digest,
                context,
            )?
        {
            return Err(recovery_error(
                "pipeline.node_recovery.result_context_mismatch",
            ));
        }
        let snapshot = self.load().await?;
        let prior = codec::at_revision(&snapshot.ledger, request.expected_revision)
            .map_err(policy_error)?;
        let receipt = super::node_recovery_receipt::NodeRecoveryRequiredReceipt::from_ledger(
            activation, &prior,
        )
        .ok_or_else(|| recovery_error("pipeline.node_recovery.owner_result_denied"))?;
        if request.activation_id != self.activation_id
            || !receipt.matches(activation, &prior)
            || proof.kind != super::node_recovery_owner::NodeRecoveryOwnerProofKind::CommittedResult
            || !proof.validates(&proof.execution_id, proof.generation, &receipt)
            || !proof.matches_result_bytes(receipt_wire)
        {
            return Err(recovery_error("pipeline.node_recovery.owner_result_denied"));
        }
        authorizer
            .authorize_committed_result(&receipt, &request, proof)
            .await?;
        self.ensure_current()?;
        let mut authority =
            NodeAttemptAuthority::committed(activation, self.activation_id, receipt.attempt, true);
        authority.recovery_receipt_sha256 = Some(recovery_receipt_hash(&receipt)?);
        let output = projector
            .project_committed_result(context, &authority, proof, receipt_wire)
            .map_err(|_| recovery_error("pipeline.node_recovery.owner_result_invalid"))?;
        if output.interrupt.is_some()
            || !output.events.is_empty()
            || output.goto.is_some()
            || output.goto_parent.is_some()
        {
            return Err(recovery_error("pipeline.node_recovery.unsupported_output"));
        }
        let updates = output.updates.into_iter().collect::<BTreeMap<_, _>>();
        let result_id = node_result_receipt(self.activation_id, &updates)?;
        let effect_id = parse_hex_id(&proof.effect_id)?;
        let owner_receipt_sha256 = parse_hex_id(&proof.owner_receipt_sha256)?;
        if snapshot.ledger.revision() != request.expected_revision {
            if codec::is_owner_result_replay(
                &snapshot.ledger,
                request,
                effect_id,
                owner_receipt_sha256,
                result_id,
            )
            .map_err(policy_error)?
                && snapshot.updates.as_ref() == Some(&updates)
            {
                return Ok(AppliedNodeRecoveryAction::retry(
                    request,
                    snapshot.ledger.revision(),
                ));
            }
            return Err(recovery_error(
                "pipeline.node_recovery.stale_operator_action",
            ));
        }
        let completed = snapshot
            .ledger
            .resume_owner_result(request, effect_id, owner_receipt_sha256, result_id, now_ms)
            .map_err(policy_error)?;
        let applied = self.append(&snapshot, completed, Some(updates)).await?;
        Ok(AppliedNodeRecoveryAction::retry(
            request,
            applied.ledger.revision(),
        ))
    }

    pub(crate) async fn verify_pending_receipt(
        &self,
        activation: &NodeAttemptActivation,
        receipt: &super::node_recovery_receipt::NodeRecoveryRequiredReceipt,
    ) -> Result<(), GraphError> {
        let snapshot = self.load().await?;
        if snapshot.ledger.revision() < receipt.journal_revision
            || snapshot.ledger.revision()
                > receipt
                    .journal_revision
                    .checked_add(1)
                    .ok_or_else(|| recovery_error("pipeline.node_recovery.invalid_revision"))?
        {
            return Err(recovery_error("pipeline.node_recovery.stale_receipt"));
        }
        let prior =
            codec::at_revision(&snapshot.ledger, receipt.journal_revision).map_err(policy_error)?;
        if !receipt.matches(activation, &prior) {
            return Err(recovery_error("pipeline.node_recovery.receipt_mismatch"));
        }
        Ok(())
    }

    pub(crate) async fn verify_applied_revision(&self, revision: u64) -> Result<(), GraphError> {
        let snapshot = self.load().await?;
        if snapshot.ledger.revision() != revision {
            return Err(recovery_error("pipeline.node_recovery.stale_receipt"));
        }
        Ok(())
    }

    pub(crate) fn bound(
        checkpoints: Arc<dyn ParallelCheckpointAppender>,
        lease: Arc<dyn StateWriterLease>,
        thread_id: String,
        activation_id: [u8; 32],
        policy: NodeRecoveryPolicy,
    ) -> Self {
        Self {
            checkpoints,
            lease,
            thread_id,
            activation_id,
            policy,
        }
    }
    fn ensure_current(&self) -> Result<(), GraphError> {
        self.lease
            .ensure_current()
            .map_err(|_| recovery_error("pipeline.node_recovery.writer_not_current"))
    }
    async fn load(&self) -> Result<NodeJournalSnapshot, GraphError> {
        self.ensure_current()?;
        let checkpoint = self.checkpoints.load(&self.thread_id).await?;
        let Some(checkpoint) = checkpoint else {
            return Ok(NodeJournalSnapshot {
                checkpoint: None,
                ledger: NodeAttemptLedger::new(self.activation_id, self.policy.clone())
                    .map_err(policy_error)?,
                updates: None,
            });
        };
        if checkpoint.thread_id != self.thread_id
            || !checkpoint.state.is_empty()
            || !checkpoint.pending_nodes.is_empty()
            || !checkpoint.attempts.is_empty()
            || !checkpoint.child_ledger.is_empty()
            || checkpoint.metadata.len() != 1
        {
            return Err(recovery_error("pipeline.node_recovery.corrupt_journal"));
        }
        let raw = checkpoint
            .metadata
            .get(JOURNAL_KEY)
            .ok_or_else(|| recovery_error("pipeline.node_recovery.corrupt_journal"))?;
        if serde_json::to_vec(raw)
            .map_err(|_| recovery_error("pipeline.node_recovery.corrupt_journal"))?
            .len()
            > MAX_RESULT_BYTES + codec::MAX_NODE_RECOVERY_BYTES
        {
            return Err(recovery_error("pipeline.node_recovery.result_limit"));
        }
        let stored: JournalRecord = serde_json::from_value(raw.clone())
            .map_err(|_| recovery_error("pipeline.node_recovery.corrupt_journal"))?;
        if stored.schema != JOURNAL_KEY {
            return Err(recovery_error("pipeline.node_recovery.corrupt_journal"));
        }
        let ledger = codec::decode(&stored.ledger, self.activation_id, &self.policy)
            .map_err(policy_error)?;
        if checkpoint.step
            != usize::try_from(ledger.revision())
                .map_err(|_| recovery_error("pipeline.node_recovery.corrupt_journal"))?
        {
            return Err(recovery_error("pipeline.node_recovery.corrupt_journal"));
        }
        validate_result(&ledger, stored.completed_updates.as_ref())?;
        Ok(NodeJournalSnapshot {
            checkpoint: Some(checkpoint),
            ledger,
            updates: stored.completed_updates,
        })
    }
    async fn append(
        &self,
        parent: &NodeJournalSnapshot,
        ledger: NodeAttemptLedger,
        updates: Option<BTreeMap<String, Value>>,
    ) -> Result<NodeJournalSnapshot, GraphError> {
        self.ensure_current()?;
        codec::require_successor(&parent.ledger, &ledger).map_err(policy_error)?;
        validate_result(&ledger, updates.as_ref())?;
        let mut checkpoint = Checkpoint::new(
            &self.thread_id,
            State::new(),
            usize::try_from(ledger.revision())
                .map_err(|_| recovery_error("pipeline.node_recovery.corrupt_journal"))?,
            Vec::new(),
        );
        checkpoint.metadata.insert(
            JOURNAL_KEY.to_owned(),
            serde_json::to_value(JournalRecord {
                schema: JOURNAL_KEY.to_owned(),
                ledger: codec::encode(&ledger).map_err(policy_error)?,
                completed_updates: updates.clone(),
            })
            .map_err(|_| recovery_error("pipeline.node_recovery.corrupt_journal"))?,
        );
        self.checkpoints
            .append_after(parent.checkpoint.as_ref(), &checkpoint)
            .await?;
        self.ensure_current()?;
        Ok(NodeJournalSnapshot {
            checkpoint: Some(checkpoint),
            ledger,
            updates,
        })
    }

    /// A recovery-only claim invokes this before the suspended graph can resume.
    #[allow(
        clippy::too_many_lines,
        reason = "Keep ordered authority checks and durable phases in one owner operation."
    )]
    pub(crate) async fn resume_operator_retry(
        &self,
        activation: &NodeAttemptActivation,
        request: super::node_recovery::OperatorRetryRequest,
        authorizer: &dyn NodeRecoveryOperatorAuthorizer,
        now_ms: u64,
    ) -> Result<AppliedNodeRecoveryAction, GraphError> {
        self.ensure_current()?;
        let snapshot = self.load().await?;
        if request.expected_revision.checked_add(1) == Some(snapshot.ledger.revision())
            && matches!(
                snapshot.ledger.phase(),
                NodeAttemptPhase::Failed(
                    RecoveryDecision::Stop(super::node_recovery::StopReason::ElapsedLimit)
                        | RecoveryDecision::ErrorRoute {
                            failed: super::node_recovery::FailedNodeOutput {
                                reason: super::node_recovery::StopReason::ElapsedLimit,
                                ..
                            },
                            ..
                        }
                )
            )
        {
            let prior = codec::at_revision(&snapshot.ledger, request.expected_revision)
                .map_err(policy_error)?;
            let receipt = super::node_recovery_receipt::NodeRecoveryRequiredReceipt::from_ledger(
                activation, &prior,
            )
            .ok_or_else(|| recovery_error("pipeline.node_recovery.operator_action_not_allowed"))?;
            if request.activation_id != self.activation_id {
                return Err(recovery_error(
                    "pipeline.node_recovery.stale_operator_action",
                ));
            }
            authorizer.authorize_retry(&receipt, &request).await?;
            self.ensure_current()?;
            return Ok(match snapshot.ledger.phase() {
                NodeAttemptPhase::Failed(RecoveryDecision::ErrorRoute { .. }) => {
                    AppliedNodeRecoveryAction::retry(request, snapshot.ledger.revision())
                }
                _ => AppliedNodeRecoveryAction::stopped(
                    request,
                    snapshot.ledger.revision(),
                    super::node_recovery::StopReason::ElapsedLimit,
                ),
            });
        }
        if let Some(prior) =
            codec::before_operator_request(&snapshot.ledger, request).map_err(policy_error)?
        {
            let receipt = super::node_recovery_receipt::NodeRecoveryRequiredReceipt::from_ledger(
                activation, &prior,
            )
            .ok_or_else(|| recovery_error("pipeline.node_recovery.operator_action_not_allowed"))?;
            authorizer.authorize_retry(&receipt, &request).await?;
            self.ensure_current()?;
            if request.expected_revision.checked_add(1) != Some(snapshot.ledger.revision()) {
                return Err(recovery_error(
                    "pipeline.node_recovery.action_already_advanced",
                ));
            }
            return Ok(AppliedNodeRecoveryAction::retry(
                request,
                snapshot.ledger.revision(),
            ));
        }
        let receipt = super::node_recovery_receipt::NodeRecoveryRequiredReceipt::from_ledger(
            activation,
            &snapshot.ledger,
        )
        .ok_or_else(|| recovery_error("pipeline.node_recovery.operator_action_not_allowed"))?;
        if receipt.allowed_actions != [super::node_recovery_receipt::NodeRecoveryAction::Retry]
            || request.activation_id != self.activation_id
            || request.expected_revision != snapshot.ledger.revision()
        {
            return Err(recovery_error(
                "pipeline.node_recovery.stale_operator_action",
            ));
        }
        authorizer.authorize_retry(&receipt, &request).await?;
        self.ensure_current()?;
        if let Some(expired) = snapshot.ledger.expire_wait(now_ms).map_err(policy_error)? {
            let error_route = match expired.phase() {
                NodeAttemptPhase::Failed(RecoveryDecision::ErrorRoute { .. }) => true,
                NodeAttemptPhase::Failed(RecoveryDecision::Stop(
                    super::node_recovery::StopReason::ElapsedLimit,
                )) => false,
                _ => {
                    return Err(recovery_error(
                        "pipeline.node_recovery.operator_action_not_allowed",
                    ));
                }
            };
            let applied = self.append(&snapshot, expired, None).await?;
            return Ok(if error_route {
                AppliedNodeRecoveryAction::retry(request, applied.ledger.revision())
            } else {
                AppliedNodeRecoveryAction::stopped(
                    request,
                    applied.ledger.revision(),
                    super::node_recovery::StopReason::ElapsedLimit,
                )
            });
        }
        let next = snapshot
            .ledger
            .operator_retry(request, now_ms)
            .map_err(policy_error)?;
        let applied = self.append(&snapshot, next, None).await?;
        Ok(AppliedNodeRecoveryAction::retry(
            request,
            applied.ledger.revision(),
        ))
    }
}

fn recovery_receipt_hash(
    receipt: &super::node_recovery_receipt::NodeRecoveryRequiredReceipt,
) -> Result<[u8; 32], GraphError> {
    let mut value = serde_json::to_value(receipt)
        .map_err(|_| recovery_error("pipeline.node_recovery.invalid_receipt"))?;
    value.sort_all_objects();
    Ok(sha256(&serde_json::to_vec(&value).map_err(|_| {
        recovery_error("pipeline.node_recovery.invalid_receipt")
    })?))
}
fn hex_id(value: &[u8; 32]) -> String {
    crate::sandbox::code_recovery::hex(value)
}
fn parse_hex_id(value: &str) -> Result<[u8; 32], GraphError> {
    if value.len() != 64
        || !value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
    {
        return Err(recovery_error(
            "pipeline.node_recovery.invalid_owner_identity",
        ));
    }
    let mut result = [0; 32];
    for (index, byte) in result.iter_mut().enumerate() {
        *byte = u8::from_str_radix(&value[index * 2..index * 2 + 2], 16)
            .map_err(|_| recovery_error("pipeline.node_recovery.invalid_owner_identity"))?;
    }
    if result == [0; 32] {
        return Err(recovery_error(
            "pipeline.node_recovery.invalid_owner_identity",
        ));
    }
    Ok(result)
}

/// Issued only after an exact current-writer CAS, including its durable replay.
/// It cannot authorize graph dispatch; Main must ACK this precise transition.
pub(crate) struct AppliedNodeRecoveryAction {
    request_id: [u8; 32],
    activation_id: [u8; 32],
    expected_revision: u64,
    applied_revision: u64,
    terminal_stop_reason: Option<super::node_recovery::StopReason>,
    continuation_receipt: Option<super::node_recovery_receipt::NodeRecoveryRequiredReceipt>,
    failure_route_continuation: Option<NodeRecoveryFailureRouteContinuation>,
}
#[derive(Clone, serde::Serialize, serde::Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub(crate) struct NodeRecoveryFailureRouteContinuation {
    pub(crate) schema: String,
    pub(crate) route_id: String,
    pub(crate) failed: NodeRecoveryFailureRouteFailure,
}
#[derive(Clone, serde::Serialize, serde::Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub(crate) struct NodeRecoveryFailureRouteFailure {
    pub(crate) activation_id: String,
    pub(crate) attempt: u16,
    pub(crate) failure_class: super::node_recovery::NodeFailureClass,
    pub(crate) stop_reason: super::node_recovery::StopReason,
}
impl AppliedNodeRecoveryAction {
    fn retry(request: super::node_recovery::OperatorRetryRequest, applied_revision: u64) -> Self {
        Self {
            request_id: request.request_id,
            activation_id: request.activation_id,
            expected_revision: request.expected_revision,
            applied_revision,
            terminal_stop_reason: None,
            continuation_receipt: None,
            failure_route_continuation: None,
        }
    }
    fn stopped(
        request: super::node_recovery::OperatorRetryRequest,
        revision: u64,
        reason: super::node_recovery::StopReason,
    ) -> Self {
        Self {
            terminal_stop_reason: Some(reason),
            ..Self::retry(request, revision)
        }
    }
    pub(crate) fn terminal_stop_reason(&self) -> Option<super::node_recovery::StopReason> {
        self.terminal_stop_reason
    }
    pub(crate) fn continuation_receipt(
        &self,
    ) -> Option<&super::node_recovery_receipt::NodeRecoveryRequiredReceipt> {
        self.continuation_receipt.as_ref()
    }
    pub(crate) fn failure_route_continuation(
        &self,
    ) -> Option<&NodeRecoveryFailureRouteContinuation> {
        self.failure_route_continuation.as_ref()
    }
    pub(crate) fn matches(&self, request: super::node_recovery::OperatorRetryRequest) -> bool {
        self.request_id == request.request_id
            && self.activation_id == request.activation_id
            && self.expected_revision == request.expected_revision
            && self.expected_revision.checked_add(1) == Some(self.applied_revision)
    }
    pub(crate) fn applied_revision(&self) -> u64 {
        self.applied_revision
    }
}

pub(crate) struct RecoverableNode {
    body: Arc<dyn NodeAttemptBody>,
    node_digest: [u8; 32],
    policy: NodeRecoveryPolicy,
    journal_factory: Arc<dyn NodeRecoveryFactory>,
    error_route: Option<(String, String)>,
    clock: Arc<dyn NodeRecoveryClock>,
    legacy_first_attempt: bool,
}

#[async_trait]
pub(crate) trait NodeRecoveryClock: Send + Sync {
    fn now_ms(&self) -> Result<u64, GraphError>;
    async fn wait_until(&self, target_ms: u64) -> Result<(), GraphError>;
}
struct NodeRecoverySystemClock;
#[async_trait]
impl NodeRecoveryClock for NodeRecoverySystemClock {
    fn now_ms(&self) -> Result<u64, GraphError> {
        now_ms()
    }
    async fn wait_until(&self, target_ms: u64) -> Result<(), GraphError> {
        let now = now_ms()?;
        if target_ms > now {
            tokio::time::sleep(std::time::Duration::from_millis((target_ms - now).min(100))).await;
        }
        Ok(())
    }
}

impl RecoverableNode {
    pub(crate) fn new(
        body: Arc<dyn NodeAttemptBody>,
        node_digest: [u8; 32],
        policy: NodeRecoveryPolicy,
        journal_factory: Arc<dyn NodeRecoveryFactory>,
        error_route: Option<(String, String)>,
    ) -> Self {
        Self {
            body,
            node_digest,
            policy,
            journal_factory,
            error_route,
            clock: Arc::new(NodeRecoverySystemClock),
            legacy_first_attempt: false,
        }
    }
    pub(crate) fn with_legacy_first_attempt(mut self) -> Self {
        self.legacy_first_attempt = true;
        self
    }
    #[allow(
        clippy::too_many_lines,
        reason = "Keep ordered authority checks and durable phases in one owner operation."
    )]
    async fn execute_inner(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let activation =
            NodeAttemptActivation::from_context(self.body.name(), self.node_digest, context)?;
        let journal = self.journal_factory.open(&activation, &self.policy).await?;
        let mut snapshot = journal.load().await?;
        loop {
            journal.ensure_current()?;
            if let Some(expired) = snapshot
                .ledger
                .expire_wait(self.clock.now_ms()?)
                .map_err(policy_error)?
            {
                snapshot = journal.append(&snapshot, expired, None).await?;
            }
            if context
                .config
                .parent_context
                .as_ref()
                .is_some_and(|parent| parent.is_cancelled())
            {
                // The writer can already be revoked. Never dispatch after that failure.
                if !matches!(snapshot.ledger.phase(), NodeAttemptPhase::Completed { .. }) {
                    let stopped = snapshot
                        .ledger
                        .record_control_stop(NodeFailureClass::Cancelled, self.clock.now_ms()?)
                        .map_err(policy_error)?;
                    journal.append(&snapshot, stopped, None).await?;
                }
                return Err(recovery_error("pipeline.node_recovery.cancelled"));
            }
            let recovering_started = snapshot.ledger.phase() == NodeAttemptPhase::Running;
            match snapshot.ledger.phase() {
                NodeAttemptPhase::Completed { .. } => {
                    return output_from_updates(snapshot.updates.as_ref().ok_or_else(|| {
                        recovery_error("pipeline.node_recovery.corrupt_journal")
                    })?);
                }
                NodeAttemptPhase::Failed(
                    RecoveryDecision::Stop(
                        super::node_recovery::StopReason::OperatorApprovalRequired,
                    )
                    | RecoveryDecision::Reconcile { .. },
                ) => {
                    let receipt =
                        super::node_recovery_receipt::NodeRecoveryRequiredReceipt::from_ledger(
                            &activation,
                            &snapshot.ledger,
                        )
                        .ok_or_else(|| recovery_error("pipeline.node_recovery.corrupt_journal"))?;
                    return Ok(NodeOutput::new().with_interrupt(adk_rust::graph::interrupt::Interrupt::Dynamic {
                        message: "Node recovery requires an authorized operator action.".to_owned(),
                        data: Some(serde_json::json!({ "guardrail_type": "pipeline_node_recovery", "receipt": receipt })),
                    }));
                }
                NodeAttemptPhase::Failed(RecoveryDecision::Stop(_)) => {
                    let failure = snapshot.ledger.effective_failure().map_err(policy_error)?;
                    journal.ensure_current()?;
                    if failure.class != NodeFailureClass::LeaseLost {
                        self.body
                            .report_restored_terminal_failure(context, failure.class)
                            .await?;
                    }
                    return Err(recovery_error("pipeline.node_recovery.failed"));
                }
                NodeAttemptPhase::ControlStopped { class, .. } => {
                    journal.ensure_current()?;
                    if class != NodeFailureClass::LeaseLost {
                        self.body
                            .report_restored_terminal_failure(context, class)
                            .await?;
                    }
                    return Err(recovery_error("pipeline.node_recovery.failed"));
                }
                NodeAttemptPhase::Failed(RecoveryDecision::ErrorRoute { failed, .. }) => {
                    let (target, error_input) = self.error_route.as_ref().ok_or_else(|| {
                        recovery_error("pipeline.node_recovery.invalid_error_route")
                    })?;
                    return Ok(NodeOutput::new()
                        .with_update(
                            error_input,
                            serde_json::to_value(failed).map_err(|_| {
                                recovery_error("pipeline.node_recovery.invalid_error_route")
                            })?,
                        )
                        .with_goto([target.as_str()]));
                }
                NodeAttemptPhase::Failed(RecoveryDecision::RetryAt { not_before_ms, .. }) => {
                    let now = self.clock.now_ms()?;
                    if now < not_before_ms {
                        // Short waits observe cancellation and the current lease.
                        self.clock.wait_until(not_before_ms).await?;
                        continue;
                    }
                    let started = snapshot.ledger.start_attempt(now).map_err(policy_error)?;
                    snapshot = journal.append(&snapshot, started, None).await?;
                }
                NodeAttemptPhase::Ready => {
                    let started = snapshot
                        .ledger
                        .start_attempt(self.clock.now_ms()?)
                        .map_err(policy_error)?;
                    snapshot = journal.append(&snapshot, started, None).await?;
                }
                NodeAttemptPhase::Running => {}
            }
            let attempt = snapshot
                .ledger
                .history()
                .last()
                .ok_or_else(|| recovery_error("pipeline.node_recovery.corrupt_journal"))?
                .attempt;
            let mut authority = NodeAttemptAuthority::committed(
                &activation,
                journal.activation_id,
                attempt,
                recovering_started,
            );
            if self.legacy_first_attempt {
                if attempt != 1 {
                    return Err(recovery_error(
                        "pipeline.node_recovery.invalid_legacy_attempt",
                    ));
                }
                authority.dispatch_activation = self
                    .body
                    .legacy_dispatch_identity(context)?
                    .ok_or_else(|| {
                        recovery_error("pipeline.node_recovery.invalid_legacy_attempt")
                    })?;
            }
            journal.ensure_current()?;
            let outcome = self
                .body
                .execute_attempt_reported(context, &authority)
                .await;
            match outcome {
                Ok(output) => {
                    if output.interrupt.is_some()
                        || !output.events.is_empty()
                        || output.goto.is_some()
                        || output.goto_parent.is_some()
                    {
                        return Err(recovery_error("pipeline.node_recovery.unsupported_output"));
                    }
                    let updates = output.updates.into_iter().collect::<BTreeMap<_, _>>();
                    let receipt = node_result_receipt(journal.activation_id, &updates)?;
                    let completed = snapshot
                        .ledger
                        .record_success(receipt, self.clock.now_ms()?)
                        .map_err(policy_error)?;
                    snapshot = journal.append(&snapshot, completed, Some(updates)).await?;
                }
                Err(failure) => {
                    let failed = snapshot
                        .ledger
                        .record_failure(failure.failure, self.clock.now_ms()?)
                        .map_err(policy_error)?;
                    snapshot = journal.append(&snapshot, failed, None).await?;
                    if failure.failure.class != NodeFailureClass::LeaseLost
                        && let Some(code) = failure.terminal_code
                        && matches!(snapshot.ledger.phase(), NodeAttemptPhase::Failed(
                            RecoveryDecision::Stop(reason)
                        ) if reason != super::node_recovery::StopReason::OperatorApprovalRequired)
                    {
                        journal.ensure_current()?;
                        self.body
                            .report_terminal_failure(context, &authority, code)
                            .await?;
                        return Err(recovery_error("pipeline.node_recovery.failed"));
                    }
                }
            }
        }
    }
}

#[async_trait]
impl Node for RecoverableNode {
    fn name(&self) -> &str {
        self.body.name()
    }
    fn description(&self) -> &str {
        self.body.description()
    }
    fn capabilities(&self) -> adk_rust::AgentCapabilities {
        self.body.capabilities()
    }
    fn validate(&self) -> Result<(), GraphError> {
        self.body.validate()
    }
    fn validate_against(&self, parent: &adk_rust::graph::StateSchema) -> Result<(), GraphError> {
        self.body.validate_against(parent)
    }
    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        self.execute_inner(context).await
    }
}

fn validate_result(
    ledger: &NodeAttemptLedger,
    updates: Option<&BTreeMap<String, Value>>,
) -> Result<(), GraphError> {
    match (ledger.phase(), updates) {
        (NodeAttemptPhase::Completed { receipt_id }, Some(updates))
            if node_result_receipt(ledger.logical_activation(), updates)? == receipt_id =>
        {
            Ok(())
        }
        (NodeAttemptPhase::Completed { .. }, _) | (_, Some(_)) => {
            Err(recovery_error("pipeline.node_recovery.corrupt_result"))
        }
        (_, None) => Ok(()),
    }
}

fn node_result_receipt(
    activation: [u8; 32],
    updates: &BTreeMap<String, Value>,
) -> Result<[u8; 32], GraphError> {
    let bytes = serde_json::to_vec(updates)
        .map_err(|_| recovery_error("pipeline.node_recovery.invalid_result"))?;
    if bytes.len() > MAX_RESULT_BYTES || updates.len() > 256 {
        return Err(recovery_error("pipeline.node_recovery.result_limit"));
    }
    let mut hash = digest::Context::new(&digest::SHA256);
    hash.update(b"elitea.pipeline.node-result.v1\0");
    hash.update(&activation);
    hash.update(&bytes);
    Ok(finish(hash))
}
#[allow(
    clippy::unnecessary_wraps,
    reason = "Retain the typed failure contract used by recovery assembly."
)]
fn output_from_updates(updates: &BTreeMap<String, Value>) -> Result<NodeOutput, GraphError> {
    let mut output = NodeOutput::new();
    for (key, value) in updates {
        output = output.with_update(key, value.clone());
    }
    Ok(output)
}
fn now_ms() -> Result<u64, GraphError> {
    u64::try_from(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_err(|_| recovery_error("pipeline.node_recovery.invalid_clock"))?
            .as_millis(),
    )
    .map_err(|_| recovery_error("pipeline.node_recovery.invalid_clock"))
}
fn sha256(bytes: &[u8]) -> [u8; 32] {
    finish({
        let mut hash = digest::Context::new(&digest::SHA256);
        hash.update(bytes);
        hash
    })
}
fn finish(hash: digest::Context) -> [u8; 32] {
    let mut value = [0; 32];
    value.copy_from_slice(hash.finish().as_ref());
    value
}
fn policy_error(_: super::node_recovery::NodeRecoveryError) -> GraphError {
    recovery_error("pipeline.node_recovery.invalid_transition")
}
pub(crate) fn recovery_error(code: &'static str) -> GraphError {
    GraphError::Other(code.to_owned())
}

#[cfg(test)]
#[path = "node_recovery_runtime_tests.rs"]
mod tests;
