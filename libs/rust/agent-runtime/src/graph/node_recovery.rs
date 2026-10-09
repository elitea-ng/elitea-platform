//! Typed node recovery decisions. The caller owns durable writes and dispatch.
//!
//! Persist each returned ledger before dispatch, backoff, or failure routing.
//! Validate the current claim lease before each durable write and dispatch.
//! This module grants no authorization and writes no business output.

use std::collections::BTreeSet;

use serde::{Deserialize, Serialize};

#[path = "node_recovery_codec.rs"]
pub mod codec;

pub const MAX_NODE_ATTEMPTS: u16 = 16;
const MAX_BACKOFF_MS: u64 = 300_000;
const MAX_ELAPSED_MS: u64 = 3_600_000;
const MAX_OPERATOR_ACTIONS: usize = 16;

/// Safe categories contain no provider messages or business payloads.
#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, Ord, PartialEq, PartialOrd)]
#[serde(rename_all = "snake_case")]
pub enum NodeFailureClass {
    DependencyUnavailable,
    RateLimited,
    AttemptTimeout,
    WorkerInterrupted,
    InvalidConfiguration,
    InvalidInput,
    InvalidResult,
    ModelOutputIncomplete,
    AuthenticationDenied,
    AuthorizationDenied,
    SensitiveRejected,
    Cancelled,
    LeaseLost,
    Unknown,
}

impl NodeFailureClass {
    const fn retry_condition(self) -> Option<RetryCondition> {
        match self {
            Self::DependencyUnavailable => Some(RetryCondition::DependencyUnavailable),
            Self::RateLimited => Some(RetryCondition::RateLimited),
            Self::AttemptTimeout => Some(RetryCondition::AttemptTimeout),
            Self::WorkerInterrupted => Some(RetryCondition::WorkerInterrupted),
            Self::InvalidConfiguration
            | Self::InvalidInput
            | Self::InvalidResult
            | Self::ModelOutputIncomplete
            | Self::AuthenticationDenied
            | Self::AuthorizationDenied
            | Self::SensitiveRejected
            | Self::Cancelled
            | Self::LeaseLost
            | Self::Unknown => None,
        }
    }

    const fn denial(self) -> bool {
        matches!(
            self,
            Self::AuthenticationDenied | Self::AuthorizationDenied | Self::SensitiveRejected
        )
    }

    const fn control_stop(self) -> bool {
        matches!(self, Self::Cancelled | Self::LeaseLost)
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, Ord, PartialEq, PartialOrd)]
#[serde(rename_all = "snake_case")]
pub enum RetryCondition {
    DependencyUnavailable,
    RateLimited,
    AttemptTimeout,
    WorkerInterrupted,
}

/// The effect owner supplies these facts from admitted capabilities or receipts.
#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub enum ReplaySafety {
    NoExternalEffect,
    IdempotentEffectNotCommitted {
        #[serde(with = "codec::hex_id")]
        effect_id: [u8; 32],
    },
    UnknownExternalEffect {
        #[serde(with = "codec::hex_id")]
        effect_id: [u8; 32],
    },
    CompletedExternalEffect {
        #[serde(with = "codec::hex_id")]
        receipt_id: [u8; 32],
    },
    Unclassified,
}

impl ReplaySafety {
    const fn repeatable(self) -> bool {
        matches!(
            self,
            Self::NoExternalEffect | Self::IdempotentEffectNotCommitted { .. }
        )
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct NodeFailure {
    pub class: NodeFailureClass,
    pub replay: ReplaySafety,
}

impl NodeFailure {
    #[must_use]
    pub const fn new(class: NodeFailureClass, replay: ReplaySafety) -> Self {
        Self { class, replay }
    }
}

#[derive(Clone, Copy, Default, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
pub enum NodeRetryMode {
    #[default]
    Automatic,
    Operator,
}

/// These values represent facts checked by the graph compiler.
#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
#[allow(
    clippy::struct_excessive_bools,
    reason = "Preserve the existing explicit persisted state and authority fields."
)]
pub struct ErrorRouteContract {
    pub dedicated_typed_error_input: bool,
    pub requires_success_output: bool,
    pub reexecutes_failed_operation: bool,
    pub explicitly_handles_denial: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct NodeErrorRoute {
    identity: [u8; 32],
    classes: BTreeSet<NodeFailureClass>,
}

impl NodeErrorRoute {
    pub fn admit(
        identity: [u8; 32],
        classes: BTreeSet<NodeFailureClass>,
        contract: ErrorRouteContract,
    ) -> Result<Self, NodeRecoveryError> {
        if zero_id(identity)
            || classes.is_empty()
            || !contract.dedicated_typed_error_input
            || contract.requires_success_output
            || contract.reexecutes_failed_operation
            || classes.iter().any(|class| class.control_stop())
            || (classes.iter().any(|class| class.denial()) && !contract.explicitly_handles_denial)
        {
            return Err(NodeRecoveryError::InvalidErrorRoute);
        }
        Ok(Self { identity, classes })
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct NodeBackoff {
    pub initial_ms: u64,
    pub maximum_ms: u64,
    pub multiplier: u16,
}

impl NodeBackoff {
    fn valid(self) -> bool {
        self.initial_ms > 0
            && self.initial_ms <= self.maximum_ms
            && self.maximum_ms <= MAX_BACKOFF_MS
            && (1..=16).contains(&self.multiplier)
    }

    fn delay(self, attempts_started: u16) -> u64 {
        let mut value = self.initial_ms;
        for _ in 1..attempts_started {
            value = value
                .saturating_mul(u64::from(self.multiplier))
                .min(self.maximum_ms);
        }
        value
    }
}

#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct NodeRecoveryPolicy {
    max_attempts: u16,
    #[serde(default)]
    retry_mode: NodeRetryMode,
    retry_on: BTreeSet<RetryCondition>,
    backoff: Option<NodeBackoff>,
    max_elapsed_ms: Option<u64>,
    error_route: Option<NodeErrorRoute>,
}

impl Default for NodeRecoveryPolicy {
    fn default() -> Self {
        Self {
            max_attempts: 1,
            retry_mode: NodeRetryMode::Automatic,
            retry_on: BTreeSet::new(),
            backoff: None,
            max_elapsed_ms: None,
            error_route: None,
        }
    }
}

impl NodeRecoveryPolicy {
    pub fn admit(
        max_attempts: u16,
        retry_on: BTreeSet<RetryCondition>,
        backoff: Option<NodeBackoff>,
        max_elapsed_ms: Option<u64>,
        error_route: Option<NodeErrorRoute>,
    ) -> Result<Self, NodeRecoveryError> {
        let retry_enabled = max_attempts > 1;
        if !(1..=MAX_NODE_ATTEMPTS).contains(&max_attempts)
            || (retry_enabled
                && (retry_on.is_empty() || backoff.is_none() || max_elapsed_ms.is_none()))
            || (!retry_enabled
                && (!retry_on.is_empty() || backoff.is_some() || max_elapsed_ms.is_some()))
            || backoff.is_some_and(|value| !value.valid())
            || max_elapsed_ms.is_some_and(|value| value == 0 || value > MAX_ELAPSED_MS)
        {
            return Err(NodeRecoveryError::InvalidPolicy);
        }
        Ok(Self {
            max_attempts,
            retry_mode: NodeRetryMode::Automatic,
            retry_on,
            backoff,
            max_elapsed_ms,
            error_route,
        })
    }

    pub fn with_retry_mode(mut self, mode: NodeRetryMode) -> Result<Self, NodeRecoveryError> {
        if mode == NodeRetryMode::Operator && self.max_attempts == 1 {
            return Err(NodeRecoveryError::InvalidPolicy);
        }
        self.retry_mode = mode;
        Ok(self)
    }

    fn deadline(&self, first_started_ms: u64) -> Result<Option<u64>, NodeRecoveryError> {
        self.max_elapsed_ms
            .map(|limit| {
                first_started_ms
                    .checked_add(limit)
                    .ok_or(NodeRecoveryError::InvalidClock)
            })
            .transpose()
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
pub enum StopReason {
    ControlDecision,
    NotRetryable,
    RetryDisabled,
    AttemptsExhausted,
    ElapsedLimit,
    OperatorApprovalRequired,
    EffectReconciliationRequired,
}

/// Only a typed error route receives this value. It has no successful output.
#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct FailedNodeOutput {
    pub activation_id: [u8; 32],
    pub attempt: u16,
    pub class: NodeFailureClass,
    pub reason: StopReason,
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub enum RecoveryDecision {
    Stop(StopReason),
    RetryAt {
        not_before_ms: u64,
        next_attempt: u16,
        effect_id: Option<[u8; 32]>,
    },
    ErrorRoute {
        route_id: [u8; 32],
        failed: FailedNodeOutput,
    },
    Reconcile {
        effect_id: Option<[u8; 32]>,
        completed_receipt: Option<[u8; 32]>,
    },
}

/// Decide after one recorded failure. Never parse a display string into policy.
pub fn plan_after_failure(
    policy: &NodeRecoveryPolicy,
    activation_id: [u8; 32],
    attempts_started: u16,
    first_started_ms: u64,
    now_ms: u64,
    failure: NodeFailure,
) -> Result<RecoveryDecision, NodeRecoveryError> {
    if zero_id(activation_id) || attempts_started == 0 || attempts_started > policy.max_attempts {
        return Err(NodeRecoveryError::InvalidHistory);
    }
    if now_ms < first_started_ms {
        return Err(NodeRecoveryError::InvalidClock);
    }
    if failure.class.control_stop() {
        return Ok(RecoveryDecision::Stop(StopReason::ControlDecision));
    }
    if failure.class.denial() && !failure.replay.repeatable() {
        return Ok(RecoveryDecision::Stop(StopReason::NotRetryable));
    }
    match failure.replay {
        ReplaySafety::UnknownExternalEffect { effect_id } => {
            if zero_id(effect_id) {
                return Err(NodeRecoveryError::InvalidEffectIdentity);
            }
            return Ok(RecoveryDecision::Reconcile {
                effect_id: Some(effect_id),
                completed_receipt: None,
            });
        }
        ReplaySafety::CompletedExternalEffect { receipt_id } => {
            if zero_id(receipt_id) {
                return Err(NodeRecoveryError::InvalidEffectIdentity);
            }
            return Ok(RecoveryDecision::Reconcile {
                effect_id: None,
                completed_receipt: Some(receipt_id),
            });
        }
        ReplaySafety::Unclassified => {
            return Ok(RecoveryDecision::Reconcile {
                effect_id: None,
                completed_receipt: None,
            });
        }
        ReplaySafety::IdempotentEffectNotCommitted { effect_id } if zero_id(effect_id) => {
            return Err(NodeRecoveryError::InvalidEffectIdentity);
        }
        ReplaySafety::NoExternalEffect | ReplaySafety::IdempotentEffectNotCommitted { .. } => {}
    }
    let retry_allowed = failure
        .class
        .retry_condition()
        .is_some_and(|class| policy.retry_on.contains(&class));
    let reason = if policy.max_attempts == 1 {
        StopReason::RetryDisabled
    } else if !retry_allowed {
        StopReason::NotRetryable
    } else if attempts_started >= policy.max_attempts {
        StopReason::AttemptsExhausted
    } else {
        let delay = policy
            .backoff
            .ok_or(NodeRecoveryError::InvalidPolicy)?
            .delay(attempts_started);
        let retry_at = now_ms
            .checked_add(delay)
            .ok_or(NodeRecoveryError::InvalidClock)?;
        let deadline = policy
            .deadline(first_started_ms)?
            .ok_or(NodeRecoveryError::InvalidPolicy)?;
        if retry_at < deadline {
            if policy.retry_mode == NodeRetryMode::Operator {
                return Ok(RecoveryDecision::Stop(StopReason::OperatorApprovalRequired));
            }
            let effect_id = match failure.replay {
                ReplaySafety::IdempotentEffectNotCommitted { effect_id } => Some(effect_id),
                _ => None,
            };
            return Ok(RecoveryDecision::RetryAt {
                not_before_ms: retry_at,
                next_attempt: attempts_started + 1,
                effect_id,
            });
        }
        StopReason::ElapsedLimit
    };
    if let Some(route) = &policy.error_route
        && route.classes.contains(&failure.class)
    {
        return Ok(RecoveryDecision::ErrorRoute {
            route_id: route.identity,
            failed: FailedNodeOutput {
                activation_id,
                attempt: attempts_started,
                class: failure.class,
                reason,
            },
        });
    }
    Ok(RecoveryDecision::Stop(reason))
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub enum AttemptOutcome {
    Running,
    Failed {
        failure: NodeFailure,
        decision: RecoveryDecision,
    },
    Completed {
        receipt_id: [u8; 32],
    },
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct NodeAttemptRecord {
    pub attempt: u16,
    pub started_ms: u64,
    pub finished_ms: Option<u64>,
    pub outcome: AttemptOutcome,
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub enum NodeAttemptPhase {
    Ready,
    Running,
    Failed(RecoveryDecision),
    ControlStopped {
        class: NodeFailureClass,
        observed_ms: u64,
    },
    Completed {
        receipt_id: [u8; 32],
    },
}

/// Main must authenticate and authorize this command before calling the helper.
#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct OperatorRetryRequest {
    pub request_id: [u8; 32],
    pub activation_id: [u8; 32],
    pub expected_revision: u64,
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct OperatorRetryAudit {
    pub request: OperatorRetryRequest,
    pub requested_ms: u64,
}

/// A replacement value must commit under the existing claim-fenced writer lock.
#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct NodeAttemptLedger {
    activation_id: [u8; 32],
    policy: NodeRecoveryPolicy,
    revision: u64,
    phase: NodeAttemptPhase,
    history: Vec<NodeAttemptRecord>,
    operator_audit: Vec<OperatorRetryAudit>,
    events: Vec<codec::NodeRecoveryEvent>,
}

impl NodeAttemptLedger {
    /// Preserve the ambiguous failure. Only authenticated owner evidence may
    /// append this transition. Reconciliation itself never starts another attempt.
    pub fn reconcile_verified_no_effect(
        &self,
        request: OperatorRetryRequest,
        owner_effect_id: [u8; 32],
        owner_receipt_sha256: [u8; 32],
        now_ms: u64,
    ) -> Result<Self, NodeRecoveryError> {
        if zero_id(request.request_id)
            || request.activation_id != self.activation_id
            || request.expected_revision != self.revision
        {
            return Err(NodeRecoveryError::StaleOperatorRequest);
        }
        if zero_id(owner_effect_id)
            || zero_id(owner_receipt_sha256)
            || !matches!(
                self.phase,
                NodeAttemptPhase::Failed(RecoveryDecision::Reconcile { .. })
            )
        {
            return Err(NodeRecoveryError::InvalidTransition);
        }
        let last = self
            .history
            .last()
            .ok_or(NodeRecoveryError::InvalidHistory)?;
        let AttemptOutcome::Failed { failure, .. } = last.outcome else {
            return Err(NodeRecoveryError::InvalidHistory);
        };
        if failure.class.control_stop() || failure.class.denial() {
            return Err(NodeRecoveryError::OperatorRetryDenied);
        }
        if !matches!(failure.replay, ReplaySafety::UnknownExternalEffect { effect_id } if effect_id == owner_effect_id)
        {
            return Err(NodeRecoveryError::InvalidEffectIdentity);
        }
        if now_ms < self.last_recorded_ms() {
            return Err(NodeRecoveryError::InvalidClock);
        }
        let first = self
            .history
            .first()
            .ok_or(NodeRecoveryError::InvalidHistory)?;
        let resolved = NodeFailure::new(failure.class, ReplaySafety::NoExternalEffect);
        let decision = plan_after_failure(
            &self.policy,
            self.activation_id,
            self.attempts_started()?,
            first.started_ms,
            now_ms,
            resolved,
        )?;
        let mut next = self.next_revision()?;
        next.phase = NodeAttemptPhase::Failed(match decision {
            RecoveryDecision::RetryAt { .. } => {
                RecoveryDecision::Stop(StopReason::OperatorApprovalRequired)
            }
            other => other,
        });
        next.events
            .push(codec::NodeRecoveryEvent::NoEffectReconciled {
                request,
                owner_effect_id,
                owner_receipt_sha256,
                attempt: last.attempt,
                now_ms,
            });
        Ok(next)
    }

    /// The original recorded outcome remains immutable. The proof event can
    /// change replay eligibility only for its exact original failed attempt.
    pub fn effective_failure(&self) -> Result<NodeFailure, NodeRecoveryError> {
        let last = self
            .history
            .last()
            .ok_or(NodeRecoveryError::InvalidHistory)?;
        let AttemptOutcome::Failed { failure, .. } = last.outcome else {
            return Err(NodeRecoveryError::InvalidHistory);
        };
        if self.events.iter().any(|event| matches!(event,
            codec::NodeRecoveryEvent::NoEffectReconciled { attempt, owner_effect_id, .. }
                if *attempt == last.attempt && matches!(failure.replay,ReplaySafety::UnknownExternalEffect { effect_id } if effect_id == *owner_effect_id)
        )) { return Ok(NodeFailure::new(failure.class, ReplaySafety::NoExternalEffect)); }
        Ok(failure)
    }
    /// Preserve the original failed attempt. The caller must authenticate the
    /// owning receipt and project it through the original node contract first.
    pub fn resume_owner_result(
        &self,
        request: OperatorRetryRequest,
        owner_effect_id: [u8; 32],
        owner_receipt_sha256: [u8; 32],
        result_receipt_id: [u8; 32],
        now_ms: u64,
    ) -> Result<Self, NodeRecoveryError> {
        if zero_id(request.request_id)
            || request.activation_id != self.activation_id
            || request.expected_revision != self.revision
        {
            return Err(NodeRecoveryError::StaleOperatorRequest);
        }
        if zero_id(owner_effect_id)
            || zero_id(owner_receipt_sha256)
            || zero_id(result_receipt_id)
            || !matches!(
                self.phase,
                NodeAttemptPhase::Failed(RecoveryDecision::Reconcile { .. })
            )
        {
            return Err(NodeRecoveryError::InvalidTransition);
        }
        let last = self
            .history
            .last()
            .ok_or(NodeRecoveryError::InvalidHistory)?;
        let AttemptOutcome::Failed { failure, .. } = last.outcome else {
            return Err(NodeRecoveryError::InvalidHistory);
        };
        if failure.class.control_stop() || failure.class.denial() {
            return Err(NodeRecoveryError::OperatorRetryDenied);
        }
        if !matches!(failure.replay,ReplaySafety::UnknownExternalEffect{effect_id} if effect_id==owner_effect_id)
            && !matches!(failure.replay,ReplaySafety::CompletedExternalEffect{receipt_id} if receipt_id==owner_effect_id)
        {
            return Err(NodeRecoveryError::InvalidEffectIdentity);
        }
        if now_ms < self.last_recorded_ms() {
            return Err(NodeRecoveryError::InvalidClock);
        }
        let mut next = self.next_revision()?;
        next.phase = NodeAttemptPhase::Completed {
            receipt_id: result_receipt_id,
        };
        next.events
            .push(codec::NodeRecoveryEvent::OwnerResultResumed {
                request,
                owner_effect_id,
                owner_receipt_sha256,
                result_receipt_id,
                now_ms,
            });
        Ok(next)
    }

    pub fn new(
        activation_id: [u8; 32],
        policy: NodeRecoveryPolicy,
    ) -> Result<Self, NodeRecoveryError> {
        if zero_id(activation_id) {
            return Err(NodeRecoveryError::InvalidHistory);
        }
        Ok(Self {
            activation_id,
            policy,
            revision: 0,
            phase: NodeAttemptPhase::Ready,
            history: Vec::new(),
            operator_audit: Vec::new(),
            events: Vec::new(),
        })
    }

    #[must_use]
    pub const fn logical_activation(&self) -> [u8; 32] {
        self.activation_id
    }

    #[must_use]
    pub const fn phase(&self) -> NodeAttemptPhase {
        self.phase
    }
    #[must_use]
    pub const fn revision(&self) -> u64 {
        self.revision
    }
    #[must_use]
    pub fn history(&self) -> &[NodeAttemptRecord] {
        &self.history
    }
    #[cfg(any(test, feature = "test-support"))]
    #[must_use]
    pub fn operator_audit(&self) -> &[OperatorRetryAudit] {
        &self.operator_audit
    }

    /// A late wake consumes no new attempt and preserves the original record.
    pub fn expire_wait(&self, now_ms: u64) -> Result<Option<Self>, NodeRecoveryError> {
        if !matches!(
            self.phase,
            NodeAttemptPhase::Failed(
                RecoveryDecision::RetryAt { .. }
                    | RecoveryDecision::Stop(StopReason::OperatorApprovalRequired)
            )
        ) {
            return Ok(None);
        }
        let first = self
            .history
            .first()
            .ok_or(NodeRecoveryError::InvalidHistory)?;
        let Some(deadline) = self.policy.deadline(first.started_ms)? else {
            return Ok(None);
        };
        if now_ms < self.last_recorded_ms() {
            return Err(NodeRecoveryError::InvalidClock);
        }
        if now_ms < deadline {
            return Ok(None);
        }
        let last = self
            .history
            .last()
            .ok_or(NodeRecoveryError::InvalidHistory)?;
        let AttemptOutcome::Failed { .. } = last.outcome else {
            return Err(NodeRecoveryError::InvalidHistory);
        };
        let failure = self.effective_failure()?;
        let mut next = self.next_revision()?;
        next.phase = NodeAttemptPhase::Failed(plan_after_failure(
            &self.policy,
            self.activation_id,
            self.attempts_started()?,
            first.started_ms,
            now_ms,
            failure,
        )?);
        next.events
            .push(codec::NodeRecoveryEvent::WaitExpired { now_ms });
        Ok(Some(next))
    }

    /// Persist the returned Started record before invoking the exact activation.
    pub fn start_attempt(&self, now_ms: u64) -> Result<Self, NodeRecoveryError> {
        let next_attempt = self
            .attempts_started()?
            .checked_add(1)
            .ok_or(NodeRecoveryError::InvalidHistory)?;
        match self.phase {
            NodeAttemptPhase::Ready if self.history.is_empty() => {}
            NodeAttemptPhase::Failed(RecoveryDecision::RetryAt {
                not_before_ms,
                next_attempt: expected,
                ..
            }) if now_ms >= not_before_ms && next_attempt == expected => {}
            _ => return Err(NodeRecoveryError::InvalidTransition),
        }
        if next_attempt > self.policy.max_attempts {
            return Err(NodeRecoveryError::InvalidTransition);
        }
        if let Some(first) = self.history.first() {
            if now_ms < self.last_recorded_ms() {
                return Err(NodeRecoveryError::InvalidClock);
            }
            if self
                .policy
                .deadline(first.started_ms)?
                .is_some_and(|deadline| now_ms >= deadline)
            {
                return Err(NodeRecoveryError::ElapsedLimit);
            }
        }
        let mut next = self.next_revision()?;
        next.history.push(NodeAttemptRecord {
            attempt: next_attempt,
            started_ms: now_ms,
            finished_ms: None,
            outcome: AttemptOutcome::Running,
        });
        next.phase = NodeAttemptPhase::Running;
        next.events.push(codec::NodeRecoveryEvent::Start { now_ms });
        Ok(next)
    }

    /// Persist the failure and exact retry time before waiting or routing.
    pub fn record_failure(
        &self,
        failure: NodeFailure,
        now_ms: u64,
    ) -> Result<Self, NodeRecoveryError> {
        self.require_running(now_ms)?;
        self.require_stable_effect(failure)?;
        let first = self
            .history
            .first()
            .ok_or(NodeRecoveryError::InvalidHistory)?;
        let decision = plan_after_failure(
            &self.policy,
            self.activation_id,
            self.attempts_started()?,
            first.started_ms,
            now_ms,
            failure,
        )?;
        let mut next = self.next_revision()?;
        let record = next
            .history
            .last_mut()
            .ok_or(NodeRecoveryError::InvalidHistory)?;
        record.finished_ms = Some(now_ms);
        record.outcome = AttemptOutcome::Failed { failure, decision };
        next.phase = NodeAttemptPhase::Failed(decision);
        next.events
            .push(codec::NodeRecoveryEvent::Failure { failure, now_ms });
        Ok(next)
    }

    /// The caller must validate the complete node result before this transition.
    pub fn record_success(
        &self,
        receipt_id: [u8; 32],
        now_ms: u64,
    ) -> Result<Self, NodeRecoveryError> {
        self.require_running(now_ms)?;
        if zero_id(receipt_id) {
            return Err(NodeRecoveryError::InvalidEffectIdentity);
        }
        let mut next = self.next_revision()?;
        let record = next
            .history
            .last_mut()
            .ok_or(NodeRecoveryError::InvalidHistory)?;
        record.finished_ms = Some(now_ms);
        record.outcome = AttemptOutcome::Completed { receipt_id };
        next.phase = NodeAttemptPhase::Completed { receipt_id };
        next.events
            .push(codec::NodeRecoveryEvent::Success { receipt_id, now_ms });
        Ok(next)
    }

    /// Reconstruct a crashed Started attempt from the effect owner's evidence.
    #[cfg(any(test, feature = "test-support"))]
    pub fn recover_interrupted(
        &self,
        replay: ReplaySafety,
        now_ms: u64,
    ) -> Result<Self, NodeRecoveryError> {
        self.record_failure(
            NodeFailure {
                class: NodeFailureClass::WorkerInterrupted,
                replay,
            },
            now_ms,
        )
    }

    /// Persist a current cancellation or lease loss before any further dispatch.
    pub fn record_control_stop(
        &self,
        class: NodeFailureClass,
        now_ms: u64,
    ) -> Result<Self, NodeRecoveryError> {
        if !class.control_stop() || matches!(self.phase, NodeAttemptPhase::Completed { .. }) {
            return Err(NodeRecoveryError::InvalidTransition);
        }
        if now_ms < self.last_recorded_ms() {
            return Err(NodeRecoveryError::InvalidClock);
        }
        let mut next = self.next_revision()?;
        if self.phase == NodeAttemptPhase::Running {
            let record = next
                .history
                .last_mut()
                .ok_or(NodeRecoveryError::InvalidHistory)?;
            record.finished_ms = Some(now_ms);
            record.outcome = AttemptOutcome::Failed {
                failure: NodeFailure {
                    class,
                    replay: ReplaySafety::Unclassified,
                },
                decision: RecoveryDecision::Stop(StopReason::ControlDecision),
            };
        }
        next.phase = NodeAttemptPhase::ControlStopped {
            class,
            observed_ms: now_ms,
        };
        next.events
            .push(codec::NodeRecoveryEvent::ControlStop { class, now_ms });
        Ok(next)
    }

    /// Preserve the consumed budget. The caller must authorize and audit its actor.
    pub fn operator_retry(
        &self,
        request: OperatorRetryRequest,
        now_ms: u64,
    ) -> Result<Self, NodeRecoveryError> {
        if let Some(prior) = self
            .operator_audit
            .iter()
            .find(|prior| prior.request.request_id == request.request_id)
        {
            return if prior.request == request {
                Ok(self.clone())
            } else {
                Err(NodeRecoveryError::StaleOperatorRequest)
            };
        }
        if zero_id(request.request_id)
            || request.activation_id != self.activation_id
            || request.expected_revision != self.revision
        {
            return Err(NodeRecoveryError::StaleOperatorRequest);
        }
        if !matches!(
            self.phase,
            NodeAttemptPhase::Failed(RecoveryDecision::RetryAt { .. } | RecoveryDecision::Stop(_))
        ) {
            return Err(NodeRecoveryError::InvalidTransition);
        }
        let last = self
            .history
            .last()
            .ok_or(NodeRecoveryError::InvalidHistory)?;
        let AttemptOutcome::Failed { .. } = last.outcome else {
            return Err(NodeRecoveryError::InvalidHistory);
        };
        if now_ms < self.last_recorded_ms() {
            return Err(NodeRecoveryError::InvalidClock);
        }
        let failure = self.effective_failure()?;
        if !failure.replay.repeatable() || failure.class.retry_condition().is_none() {
            return Err(NodeRecoveryError::OperatorRetryDenied);
        }
        let first = self
            .history
            .first()
            .ok_or(NodeRecoveryError::InvalidHistory)?;
        let automatic_policy = self
            .policy
            .clone()
            .with_retry_mode(NodeRetryMode::Automatic)?;
        let planned = plan_after_failure(
            &automatic_policy,
            self.activation_id,
            self.attempts_started()?,
            first.started_ms,
            last.finished_ms.ok_or(NodeRecoveryError::InvalidHistory)?,
            failure,
        )?;
        if self
            .policy
            .deadline(first.started_ms)?
            .is_some_and(|deadline| now_ms >= deadline)
        {
            return Err(NodeRecoveryError::OperatorRetryDenied);
        }
        let RecoveryDecision::RetryAt {
            not_before_ms,
            next_attempt,
            effect_id,
            ..
        } = planned
        else {
            return Err(NodeRecoveryError::OperatorRetryDenied);
        };
        if self.operator_audit.len() >= MAX_OPERATOR_ACTIONS {
            return Err(NodeRecoveryError::AuditLimit);
        }
        let mut next = self.next_revision()?;
        next.phase = NodeAttemptPhase::Failed(RecoveryDecision::RetryAt {
            not_before_ms,
            next_attempt,
            effect_id,
        });
        next.operator_audit.push(OperatorRetryAudit {
            request,
            requested_ms: now_ms,
        });
        next.events
            .push(codec::NodeRecoveryEvent::OperatorRetry { request, now_ms });
        Ok(next)
    }

    fn attempts_started(&self) -> Result<u16, NodeRecoveryError> {
        u16::try_from(self.history.len()).map_err(|_| NodeRecoveryError::InvalidHistory)
    }

    fn require_running(&self, now_ms: u64) -> Result<(), NodeRecoveryError> {
        if self.phase != NodeAttemptPhase::Running
            || self
                .history
                .last()
                .is_none_or(|record| record.outcome != AttemptOutcome::Running)
        {
            return Err(NodeRecoveryError::InvalidTransition);
        }
        if now_ms < self.last_recorded_ms() {
            return Err(NodeRecoveryError::InvalidClock);
        }
        Ok(())
    }

    fn last_recorded_ms(&self) -> u64 {
        let attempt = self
            .history
            .last()
            .map_or(0, |record| record.finished_ms.unwrap_or(record.started_ms));
        let operator = self
            .operator_audit
            .last()
            .map_or(0, |record| record.requested_ms);
        let control = match self.phase {
            NodeAttemptPhase::ControlStopped { observed_ms, .. } => observed_ms,
            _ => 0,
        };
        let wait = self
            .events
            .iter()
            .rev()
            .find_map(|event| match event {
                codec::NodeRecoveryEvent::WaitExpired { now_ms }
                | codec::NodeRecoveryEvent::OwnerResultResumed { now_ms, .. }
                | codec::NodeRecoveryEvent::NoEffectReconciled { now_ms, .. } => Some(*now_ms),
                _ => None,
            })
            .unwrap_or(0);
        attempt.max(operator).max(control).max(wait)
    }

    fn require_stable_effect(&self, failure: NodeFailure) -> Result<(), NodeRecoveryError> {
        let Some(previous) =
            self.history
                .iter()
                .rev()
                .skip(1)
                .find_map(|record| match record.outcome {
                    AttemptOutcome::Failed {
                        decision: RecoveryDecision::RetryAt { effect_id, .. },
                        ..
                    } => effect_id,
                    _ => None,
                })
        else {
            return Ok(());
        };
        match failure.replay {
            ReplaySafety::IdempotentEffectNotCommitted { effect_id }
            | ReplaySafety::UnknownExternalEffect { effect_id }
                if effect_id == previous =>
            {
                Ok(())
            }
            ReplaySafety::CompletedExternalEffect { .. } => Ok(()),
            _ => Err(NodeRecoveryError::InvalidEffectIdentity),
        }
    }

    fn next_revision(&self) -> Result<Self, NodeRecoveryError> {
        if self.events.len() >= codec::MAX_RECOVERY_EVENTS {
            return Err(NodeRecoveryError::InvalidHistory);
        }
        let mut next = self.clone();
        next.revision = self
            .revision
            .checked_add(1)
            .ok_or(NodeRecoveryError::InvalidHistory)?;
        Ok(next)
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub enum NodeRecoveryError {
    InvalidPolicy,
    InvalidErrorRoute,
    InvalidHistory,
    InvalidClock,
    InvalidEffectIdentity,
    InvalidTransition,
    ElapsedLimit,
    StaleOperatorRequest,
    OperatorRetryDenied,
    AuditLimit,
}

const fn zero_id(identity: [u8; 32]) -> bool {
    let mut index = 0;
    while index < identity.len() {
        if identity[index] != 0 {
            return false;
        }
        index += 1;
    }
    true
}

#[cfg(test)]
#[path = "node_recovery_tests.rs"]
mod tests;

#[cfg(test)]
#[path = "node_recovery_no_effect_tests.rs"]
mod no_effect_tests;
