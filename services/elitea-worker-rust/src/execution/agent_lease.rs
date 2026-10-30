//! Supervised ownership of one exact agent claim lease.
//!
//! The actor polls `RenewLease` and `ObserveDesiredState` sequentially. It
//! commits a renewal only after both RPCs complete and the clock sampled after
//! observation still leaves two polling intervals of safety margin. Desired
//! cancellation is latched but does not stop renewal: a synchronous ADK call
//! may still be running and must retain its fence until it exits.

use std::fmt;
use std::future::Future;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use elitea_agent_runtime::host::{ExecutionEnd, ExecutionGuard};
use tokio::sync::{mpsc, oneshot, watch};
use tokio::task::JoinHandle;
use tokio::time::{Instant, MissedTickBehavior, interval_at};

use crate::protocol::control::{
    AcceptedTerminalClaimRecovery, AgentControlClient, AgentControlError, ClaimLeaseHandle,
    ControlSemanticError, DesiredExecutionState, InactiveAgentExecution,
    LeaseMonitoredAgentExecution, LeaseStartingAgentExecution, LiveModelCheckpointInspection,
    ModelCheckpointInspection, PendingLeaseActivation, PendingModelCheckpointInspection,
    RuntimeControlRejectionKind,
};
use crate::state::{StateWriterLease, StateWriterLeaseLost};
use crate::transport::ControlRpc;

const MIN_LEASE_POLL_INTERVAL: Duration = Duration::from_millis(1);
const MAX_LEASE_POLL_INTERVAL: Duration = Duration::from_secs(10);
/// The tries to confirm the steering acknowledgements when the turn ends.
pub(crate) const STEERING_ACK_FLUSH_ATTEMPTS: u32 = 3;
/// The wait before the second try; it doubles before each next try.
const STEERING_ACK_FLUSH_BACKOFF: Duration = Duration::from_millis(100);

/// Immutable lease polling policy.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ClaimLeaseMonitorConfig {
    validated: ValidatedLeaseConfig,
}

impl ClaimLeaseMonitorConfig {
    /// Validate the polling cadence before execution authority is attached.
    ///
    /// # Errors
    ///
    /// Rejects zero, excessive, or unrepresentable two-poll safety margins.
    pub fn new(poll_interval: Duration) -> Result<Self, ClaimLeaseError> {
        if poll_interval < MIN_LEASE_POLL_INTERVAL
            || poll_interval > MAX_LEASE_POLL_INTERVAL
            || !poll_interval.subsec_nanos().is_multiple_of(1_000_000)
        {
            return Err(ClaimLeaseError::InvalidConfiguration(
                "the claim lease polling interval is outside the approved range",
            ));
        }
        let poll_nanos = poll_interval.as_nanos();
        let state_write_margin_millis =
            i64::try_from(poll_nanos.div_ceil(1_000_000)).map_err(|_| {
                ClaimLeaseError::InvalidConfiguration(
                    "the claim lease polling interval is outside the approved range",
                )
            })?;
        let doubled_nanos =
            poll_nanos
                .checked_mul(2)
                .ok_or(ClaimLeaseError::InvalidConfiguration(
                    "the claim lease polling interval is outside the approved range",
                ))?;
        let renewal_margin_millis =
            i64::try_from(doubled_nanos.div_ceil(1_000_000)).map_err(|_| {
                ClaimLeaseError::InvalidConfiguration(
                    "the claim lease polling interval is outside the approved range",
                )
            })?;
        Ok(Self {
            validated: ValidatedLeaseConfig {
                poll_interval,
                renewal_margin_millis,
                state_write_margin_millis,
            },
        })
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct ValidatedLeaseConfig {
    poll_interval: Duration,
    renewal_margin_millis: i64,
    state_write_margin_millis: i64,
}

/// Wall-clock source used only for server-issued Unix-millisecond deadlines.
pub trait UnixMillisClock: Send + Sync + 'static {
    fn now_unix_millis(&self) -> i64;
}

impl<F> UnixMillisClock for F
where
    F: Fn() -> i64 + Send + Sync + 'static,
{
    fn now_unix_millis(&self) -> i64 {
        self()
    }
}

/// Production wall clock. Invalid host time is reported at the lease boundary.
#[derive(Clone, Copy, Debug, Default)]
pub struct SystemUnixMillisClock;

impl UnixMillisClock for SystemUnixMillisClock {
    fn now_unix_millis(&self) -> i64 {
        let Ok(elapsed) = SystemTime::now().duration_since(UNIX_EPOCH) else {
            return 0;
        };
        i64::try_from(elapsed.as_millis()).unwrap_or(0)
    }
}

/// Stable categories suitable for metrics, alerts, and retry policy.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ClaimLeaseErrorCode {
    InvalidConfiguration,
    InvalidClock,
    LeaseLost,
    Cancelled,
    Draining,
    Control,
    MonitorClosed,
}

impl ClaimLeaseErrorCode {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::InvalidConfiguration => "claim_lease_invalid_configuration",
            Self::InvalidClock => "claim_lease_invalid_clock",
            Self::LeaseLost => "claim_lease_lost",
            Self::Cancelled => "claim_lease_cancelled",
            Self::Draining => "claim_lease_draining",
            Self::Control => "claim_lease_control_failure",
            Self::MonitorClosed => "claim_lease_monitor_closed",
        }
    }
}

/// Data-free lease failure with its original control error retained as source.
#[derive(Clone, Debug)]
pub enum ClaimLeaseError {
    InvalidConfiguration(&'static str),
    InvalidClock(&'static str),
    LeaseLost(&'static str),
    Cancelled(&'static str),
    Draining(&'static str),
    Control(AgentControlError),
    MonitorClosed(&'static str),
}

impl ClaimLeaseError {
    #[must_use]
    pub const fn code(&self) -> ClaimLeaseErrorCode {
        match self {
            Self::InvalidConfiguration(_) => ClaimLeaseErrorCode::InvalidConfiguration,
            Self::InvalidClock(_) => ClaimLeaseErrorCode::InvalidClock,
            Self::LeaseLost(_) => ClaimLeaseErrorCode::LeaseLost,
            Self::Cancelled(_) => ClaimLeaseErrorCode::Cancelled,
            Self::Draining(_) => ClaimLeaseErrorCode::Draining,
            Self::Control(_) => ClaimLeaseErrorCode::Control,
            Self::MonitorClosed(_) => ClaimLeaseErrorCode::MonitorClosed,
        }
    }

    #[must_use]
    pub fn retryable(&self) -> bool {
        match self {
            Self::Draining(_) | Self::MonitorClosed(_) => true,
            Self::Control(error) => error.retryable(),
            Self::InvalidConfiguration(_)
            | Self::InvalidClock(_)
            | Self::LeaseLost(_)
            | Self::Cancelled(_) => false,
        }
    }

    fn from_control(error: AgentControlError) -> Self {
        match &error {
            AgentControlError::Semantic(ControlSemanticError::Cancelled(_)) => Self::cancelled(),
            AgentControlError::Semantic(ControlSemanticError::Rejected(rejection))
                if rejection.kind() == RuntimeControlRejectionKind::Cancelled =>
            {
                Self::cancelled()
            }
            _ => Self::Control(error),
        }
    }

    const fn cancelled() -> Self {
        Self::Cancelled("the server requested execution cancellation")
    }

    const fn closed() -> Self {
        Self::MonitorClosed("the claim lease monitor is no longer available")
    }
}

impl fmt::Display for ClaimLeaseError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidConfiguration(message)
            | Self::InvalidClock(message)
            | Self::LeaseLost(message)
            | Self::Cancelled(message)
            | Self::Draining(message)
            | Self::MonitorClosed(message) => formatter.write_str(message),
            Self::Control(error) => write!(formatter, "claim lease control failed: {error}"),
        }
    }
}

impl std::error::Error for ClaimLeaseError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Control(error) => Some(error),
            _ => None,
        }
    }
}

#[derive(Clone)]
enum MonitorState {
    Running { lease_expires_at_unix_millis: i64 },
    Cancelled,
    Failed(ClaimLeaseError),
}

enum MonitorCommand {
    CheckNow(oneshot::Sender<Result<(), ClaimLeaseError>>),
}

/// One steering call (client contract 1.27): the acknowledgements, whether
/// it only acknowledges, and the reply. The reply is `None` when Main did not
/// answer (the acknowledgements are not confirmed), else the claimed
/// `(id, text)` pairs, oldest first.
struct SteeringClaim {
    acknowledged: Vec<String>,
    acknowledge_only: bool,
    reply: oneshot::Sender<Option<Vec<(String, String)>>>,
}

/// One delegation assertion mint (ADR-0044 decision 4): the audience and the
/// reply. The reply is `None` when Main refused or did not answer.
struct AssertionClaim {
    audience: String,
    reply: oneshot::Sender<Option<elitea_agent_runtime::remote_assertion::MintedAssertion>>,
}

/// What one claim's turn did with its steering messages. Shared by every
/// probe of the monitor.
#[derive(Default)]
struct SteeringLedger {
    /// Every id the turn took or restored, in order.
    taken: Vec<String>,
    /// The ids a persisted checkpoint holds (or, without a checkpoint, every
    /// taken id).
    persisted: Vec<String>,
    /// The ids Main confirmed as acknowledged.
    acknowledged: std::collections::HashSet<String>,
    /// A model checkpoint acknowledges; without it `take` does.
    checkpointed: bool,
}

impl SteeringLedger {
    fn unacknowledged(&self) -> Vec<String> {
        self.persisted
            .iter()
            .filter(|id| !self.acknowledged.contains(*id))
            .cloned()
            .collect()
    }

    fn add_persisted(&mut self, ids: &[String]) {
        for id in ids {
            if !self.taken.contains(id) {
                self.taken.push(id.clone());
            }
            if !self.persisted.contains(id) {
                self.persisted.push(id.clone());
            }
        }
    }
}

/// Result of the mandatory first lease poll after `BeginExecution`.
pub enum ClaimLeaseActivation {
    Active(LeaseMonitoredAgentExecution),
    Inactive {
        execution: InactiveAgentExecution,
        error: ClaimLeaseError,
    },
    Unavailable(ClaimLeaseError),
}

/// Cancellation-safe actor handle for one unique claim lease.
///
/// The value is intentionally non-cloneable. Dropping it signals shutdown and
/// cancels an in-flight control wait; [`Self::close`] additionally waits for
/// the actor to release its authority.
pub struct ClaimLeaseMonitor {
    commands: mpsc::Sender<MonitorCommand>,
    steering: mpsc::Sender<SteeringClaim>,
    assertions: mpsc::Sender<AssertionClaim>,
    steering_ledger: Arc<std::sync::Mutex<SteeringLedger>>,
    state: watch::Receiver<MonitorState>,
    /// ADR-0034 take back: latched true once a renewal says
    /// `workspace_wrap_up`.
    wrap_up: watch::Receiver<bool>,
    shutdown: watch::Sender<bool>,
    actor: Option<JoinHandle<()>>,
    activation: Option<PendingLeaseActivation>,
    checkpoint_activation: Option<Box<PendingModelCheckpointInspection>>,
    clock: Arc<dyn UnixMillisClock>,
    margin_millis: i64,
    supervision_live: Arc<AtomicBool>,
}

/// Cloneable read-only view of one supervised claim lease.
///
/// The probe carries no claim, fence, input, output, or renewal authority. A
/// post-authorization lifecycle uses it to signal cooperative ADK cancellation
/// after durable Stop while continuing to observe a later fatal lease loss.
/// Each call to [`Self::wait_for_change`] consumes exactly one watch revision,
/// so a latched cancellation cannot create a busy loop or hide a newer fatal
/// state.
#[allow(dead_code)] // Consumed by the capability-disabled native invocation lifecycle.
pub(crate) struct ClaimLeaseStateProbe {
    state: watch::Receiver<MonitorState>,
    wrap_up: watch::Receiver<bool>,
    clock: Arc<dyn UnixMillisClock>,
    margin_millis: i64,
    supervision_live: Arc<AtomicBool>,
    steering: mpsc::Sender<SteeringClaim>,
    steering_ledger: Arc<std::sync::Mutex<SteeringLedger>>,
    assertions: mpsc::Sender<AssertionClaim>,
}

#[allow(dead_code)] // Consumed by the capability-disabled native invocation lifecycle.
impl Clone for ClaimLeaseStateProbe {
    fn clone(&self) -> Self {
        Self {
            state: self.state.clone(),
            wrap_up: self.wrap_up.clone(),
            clock: Arc::clone(&self.clock),
            margin_millis: self.margin_millis,
            supervision_live: Arc::clone(&self.supervision_live),
            steering: self.steering.clone(),
            steering_ledger: Arc::clone(&self.steering_ledger),
            assertions: self.assertions.clone(),
        }
    }
}

#[allow(dead_code)] // Consumed by the capability-disabled native invocation lifecycle.
impl ClaimLeaseStateProbe {
    /// Read the current authoritative state without consuming a watch revision.
    ///
    /// # Errors
    ///
    /// Returns the latched durable Stop or fatal lease cause.
    pub(crate) fn ensure_running(&self) -> Result<(), ClaimLeaseError> {
        self.ensure_live_margin()
    }

    /// The take-back signal (ADR-0034): a receiver that turns true, and stays
    /// true, once Main's lease poll says `workspace_wrap_up`. Separate from
    /// the lease state, so waiting on it never consumes a lease revision.
    #[must_use]
    pub(crate) fn wrap_up_signal(&self) -> watch::Receiver<bool> {
        self.wrap_up.clone()
    }

    /// Wait for and consume the next authoritative state transition.
    ///
    /// Cancellation is returned as a typed error but leaves this probe usable;
    /// a subsequent call can therefore observe fatal lease loss after Stop.
    ///
    /// # Errors
    ///
    /// Returns the new cancellation/fatal state, or a stable closed-monitor
    /// error when the actor disappears without publishing a transition.
    pub(crate) async fn wait_for_change(&mut self) -> Result<(), ClaimLeaseError> {
        self.state
            .changed()
            .await
            .map_err(|_| ClaimLeaseError::closed())?;
        monitor_state_result(&self.state.borrow_and_update())
    }

    /// Take the steering messages queued against this claim's turn, through
    /// the lease actor, so the claim never overlaps a renewal. The call
    /// also acknowledges what persisted checkpoints hold. It never fails: a
    /// stopped or lost lease, a closed actor or a refused claim gives no
    /// messages, and the queued ones wait for the next safe point (or for the
    /// next turn, once this one ends).
    pub(crate) async fn claim_steering(&self) -> Vec<String> {
        let acknowledged = self.ledger(|ledger| ledger.unacknowledged());
        let Some(claimed) = self.steering_call(acknowledged.clone(), false).await else {
            return Vec::new();
        };
        let (fresh, checkpointed) = self.ledger(|ledger| {
            ledger.acknowledged.extend(acknowledged);
            // Main also gives again the rows that this or an older lease
            // epoch claimed and that are not acknowledged, so a lost reply
            // loses nothing. An id that this turn already holds was given to
            // the model before: do not give it again.
            let mut fresh = Vec::new();
            for (id, text) in claimed {
                if !ledger.taken.contains(&id) {
                    ledger.taken.push(id.clone());
                    fresh.push((id, text));
                }
            }
            (fresh, ledger.checkpointed)
        });
        if !checkpointed && !fresh.is_empty() {
            // No checkpoint will hold them: the model request that follows
            // is the only proof, so acknowledge now.
            let ids: Vec<String> = fresh.iter().map(|(id, _)| id.clone()).collect();
            self.ledger(|ledger| ledger.add_persisted(&ids));
            self.acknowledge().await;
        }
        fresh.into_iter().map(|(_, text)| text).collect()
    }

    /// Send again, before the turn settles, the acknowledgements that Main
    /// did not confirm. After the last checkpoint no safe point comes to
    /// send them, and Main gives an unacknowledged message to a new turn: the
    /// model would see it two times. The call tries at most
    /// [`STEERING_ACK_FLUSH_ATTEMPTS`] times and never fails. It returns
    /// `true` when nothing stays unconfirmed.
    pub(crate) async fn flush_steering_acknowledgements(&self) -> bool {
        let mut backoff = STEERING_ACK_FLUSH_BACKOFF;
        for attempt in 1..=STEERING_ACK_FLUSH_ATTEMPTS {
            if self.ledger(|ledger| ledger.unacknowledged().is_empty()) {
                return true;
            }
            if self.ensure_running().is_err() {
                break;
            }
            if attempt > 1 {
                tokio::time::sleep(backoff).await;
                backoff = backoff.saturating_mul(2);
            }
            self.acknowledge().await;
        }
        let pending = self.ledger(|ledger| ledger.unacknowledged().len());
        if pending == 0 {
            return true;
        }
        tracing::warn!(
            pending,
            "the steering acknowledgements are not confirmed before the turn settles"
        );
        false
    }

    /// Acknowledge, without a claim, what persisted checkpoints hold.
    async fn acknowledge(&self) {
        let acknowledged = self.ledger(|ledger| ledger.unacknowledged());
        if acknowledged.is_empty() {
            return;
        }
        if self
            .steering_call(acknowledged.clone(), true)
            .await
            .is_some()
        {
            self.ledger(|ledger| ledger.acknowledged.extend(acknowledged));
        }
    }

    async fn steering_call(
        &self,
        acknowledged: Vec<String>,
        acknowledge_only: bool,
    ) -> Option<Vec<(String, String)>> {
        self.ensure_running().ok()?;
        let (reply, answer) = oneshot::channel();
        self.steering
            .send(SteeringClaim {
                acknowledged,
                acknowledge_only,
                reply,
            })
            .await
            .ok()?;
        answer.await.ok().flatten()
    }

    fn ledger<T>(&self, use_ledger: impl FnOnce(&mut SteeringLedger) -> T) -> T {
        let mut ledger = self
            .steering_ledger
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        use_ledger(&mut ledger)
    }

    fn ensure_live_margin(&self) -> Result<(), ClaimLeaseError> {
        if !self.supervision_live.load(Ordering::Acquire) {
            return Err(ClaimLeaseError::closed());
        }
        if self.state.has_changed().is_err() {
            return Err(ClaimLeaseError::closed());
        }
        let state = self.state.borrow();
        monitor_state_result(&state)?;
        let MonitorState::Running {
            lease_expires_at_unix_millis,
        } = &*state
        else {
            return Err(ClaimLeaseError::LeaseLost(
                "the supervised claim lease is unavailable for state persistence",
            ));
        };
        let now_unix_millis = self.clock.now_unix_millis();
        if now_unix_millis <= 0 {
            return Err(ClaimLeaseError::InvalidClock(
                "the wall clock cannot validate the supervised claim lease",
            ));
        }
        if lease_expires_at_unix_millis
            .checked_sub(now_unix_millis)
            .is_none_or(|remaining| remaining < self.margin_millis)
        {
            return Err(ClaimLeaseError::LeaseLost(
                "the supervised claim lease has insufficient state-write margin",
            ));
        }
        Ok(())
    }
}

impl StateWriterLease for ClaimLeaseStateProbe {
    fn ensure_current(&self) -> Result<(), StateWriterLeaseLost> {
        self.ensure_live_margin().map_err(|_| StateWriterLeaseLost)
    }

    fn steering_source(&self) -> Option<Arc<dyn crate::state::TurnSteering>> {
        Some(Arc::new(self.clone()))
    }
}

#[async_trait::async_trait]
impl crate::state::TurnSteering for ClaimLeaseStateProbe {
    fn bind_checkpoint(&self) {
        self.ledger(|ledger| ledger.checkpointed = true);
    }

    fn checkpoint_ids(&self) -> Vec<String> {
        self.ledger(|ledger| ledger.taken.clone())
    }

    fn restored(&self, ids: &[String]) {
        self.ledger(|ledger| ledger.add_persisted(ids));
    }

    async fn persisted(&self, ids: &[String]) {
        self.ledger(|ledger| ledger.add_persisted(ids));
        self.acknowledge().await;
    }
}

/// The agent loop's safe point (client contract 1.27): each claimed text is
/// a new user message of the same turn.
#[async_trait::async_trait]
impl adk_rust::agent::SteeringSource for ClaimLeaseStateProbe {
    async fn take(&self, _ctx: Arc<dyn adk_rust::InvocationContext>) -> Vec<adk_rust::Content> {
        self.claim_steering()
            .await
            .into_iter()
            .map(|text| adk_rust::Content::new("user").with_text(text))
            .collect()
    }
}

/// The cloud host's delegation assertions (ADR-0044 decision 4): Main mints
/// each one under the live claim, through the lease actor, so a mint never
/// overlaps a renewal. A stopped or lost lease mints nothing.
#[async_trait::async_trait]
impl elitea_agent_runtime::remote_assertion::DelegationAssertionSource for ClaimLeaseStateProbe {
    async fn mint(
        &self,
        audience: &str,
    ) -> Result<
        elitea_agent_runtime::remote_assertion::MintedAssertion,
        elitea_agent_runtime::remote_assertion::DelegationAssertionError,
    > {
        use elitea_agent_runtime::remote_assertion::DelegationAssertionError;
        self.ensure_running()
            .map_err(|_| DelegationAssertionError::Unavailable)?;
        let (reply, answer) = oneshot::channel();
        self.assertions
            .send(AssertionClaim {
                audience: audience.to_owned(),
                reply,
            })
            .await
            .map_err(|_| DelegationAssertionError::Unavailable)?;
        answer
            .await
            .ok()
            .flatten()
            .ok_or(DelegationAssertionError::Unavailable)
    }
}

/// Durable Stop is a cooperative stop; every other lease state is lost
/// authority, which fails closed.
const fn execution_end(error: &ClaimLeaseError) -> ExecutionEnd {
    match error {
        ClaimLeaseError::Cancelled(_) => ExecutionEnd::Stopped,
        _ => ExecutionEnd::Lost,
    }
}

/// The cloud host's [`ExecutionGuard`] (ADR-0029): the claim lease is the
/// execution's lease, fence and stop signal in one.
#[async_trait::async_trait]
impl ExecutionGuard for ClaimLeaseStateProbe {
    fn ensure_current(&self) -> Result<(), ExecutionEnd> {
        self.ensure_live_margin()
            .map_err(|error| execution_end(&error))
    }

    async fn ended(&mut self) -> ExecutionEnd {
        loop {
            // `Ok` is a renewal (still running); keep waiting for an end.
            if let Err(error) = self.wait_for_change().await {
                return execution_end(&error);
            }
        }
    }
}

impl ClaimLeaseMonitor {
    /// Start periodic supervision for one exact, non-cloneable execution.
    pub fn start<R, K>(
        control: Arc<AgentControlClient<R>>,
        execution: LeaseStartingAgentExecution,
        clock: Arc<K>,
        config: ClaimLeaseMonitorConfig,
    ) -> Self
    where
        R: ControlRpc + 'static,
        K: UnixMillisClock,
    {
        let (activation, lease) = execution.split();
        Self::start_inner(control, lease, clock, config, Some(activation))
    }

    /// Supervise inspection without calling the ordinary `BeginExecution` path.
    #[allow(dead_code)] // Recovery routing is not enabled yet.
    pub(crate) fn start_checkpoint_inspection<R, K>(
        control: Arc<AgentControlClient<R>>,
        inspection: ModelCheckpointInspection,
        clock: Arc<K>,
        config: ClaimLeaseMonitorConfig,
    ) -> Self
    where
        R: ControlRpc + 'static,
        K: UnixMillisClock,
    {
        let (pending, lease) = inspection.into_lease_supervision();
        let mut monitor = Self::start_inner(control, lease, clock, config, None);
        monitor.checkpoint_activation = Some(Box::new(pending));
        monitor
    }

    /// Issue inspection access only after the exact lease passes its first poll.
    #[allow(dead_code)]
    pub(crate) async fn activate_checkpoint_inspection(
        &mut self,
    ) -> Result<LiveModelCheckpointInspection, ClaimLeaseError> {
        let pending = self
            .checkpoint_activation
            .take()
            .ok_or_else(ClaimLeaseError::closed)?;
        self.check_now().await?;
        Ok((*pending).into_live())
    }

    /// Start periodic supervision for terminal-only recovery without polling
    /// before the exact durable frame gets its first replay attempt.
    ///
    /// The recovery value contains no fresh-input or Begin authority. The
    /// periodic actor still renews the accepted claim while output settlement
    /// is in progress, matching the Python recovery boundary.
    #[allow(dead_code)] // Called by the disabled terminal-recovery coordinator.
    pub(crate) fn start_recovery<R, K>(
        control: Arc<AgentControlClient<R>>,
        recovery: AcceptedTerminalClaimRecovery,
        clock: Arc<K>,
        config: ClaimLeaseMonitorConfig,
    ) -> Self
    where
        R: ControlRpc + 'static,
        K: UnixMillisClock,
    {
        Self::start_inner(control, recovery.into_lease_handle(), clock, config, None)
    }

    /// Start supervision for an input-free replacement claim which must
    /// reconcile a stale RUNNING execution. Unlike fresh activation, this
    /// monitor can never mint business-input or invocation authority.
    pub(crate) fn start_output_recovery<R, K>(
        control: Arc<AgentControlClient<R>>,
        lease: ClaimLeaseHandle,
        clock: Arc<K>,
        config: ClaimLeaseMonitorConfig,
    ) -> Self
    where
        R: ControlRpc + 'static,
        K: UnixMillisClock,
    {
        Self::start_inner(control, lease, clock, config, None)
    }

    fn start_inner<R, K>(
        control: Arc<AgentControlClient<R>>,
        lease: ClaimLeaseHandle,
        clock: Arc<K>,
        config: ClaimLeaseMonitorConfig,
        activation: Option<PendingLeaseActivation>,
    ) -> Self
    where
        R: ControlRpc + 'static,
        K: UnixMillisClock,
    {
        let config = config.validated;
        let initial_expiry = lease.lease_expires_at_unix_millis();
        let probe_clock: Arc<dyn UnixMillisClock> = clock.clone();
        let (command_sender, command_receiver) = mpsc::channel(1);
        let (steering_sender, steering_receiver) = mpsc::channel(1);
        let (assertion_sender, assertion_receiver) = mpsc::channel(1);
        let (state_sender, state_receiver) = watch::channel(MonitorState::Running {
            lease_expires_at_unix_millis: initial_expiry,
        });
        let (shutdown_sender, shutdown_receiver) = watch::channel(false);
        let (wrap_up_sender, wrap_up_receiver) = watch::channel(false);
        let supervision_live = Arc::new(AtomicBool::new(true));
        let actor = tokio::spawn(run_lease_actor(
            control,
            lease,
            clock,
            config,
            LeaseActorChannels {
                commands: command_receiver,
                steering: steering_receiver,
                assertions: assertion_receiver,
                state: state_sender,
                wrap_up: wrap_up_sender,
                shutdown: shutdown_receiver,
                supervision_live: Arc::clone(&supervision_live),
            },
        ));
        Self {
            commands: command_sender,
            steering: steering_sender,
            assertions: assertion_sender,
            steering_ledger: Arc::default(),
            state: state_receiver,
            wrap_up: wrap_up_receiver,
            shutdown: shutdown_sender,
            actor: Some(actor),
            activation,
            checkpoint_activation: None,
            clock: probe_clock,
            margin_millis: config.state_write_margin_millis,
            supervision_live,
        }
    }

    /// Perform one serialized renewal and desired-state observation now.
    ///
    /// # Errors
    ///
    /// Returns cancellation, draining, lease loss, control failure, or a
    /// closed-monitor error. Cancellation remains latched while renewal
    /// continues in the background.
    pub async fn check_now(&self) -> Result<(), ClaimLeaseError> {
        if let Some(error) = self.current_fatal_error() {
            return Err(error);
        }
        let (result_sender, result_receiver) = oneshot::channel();
        self.commands
            .send(MonitorCommand::CheckNow(result_sender))
            .await
            .map_err(|_| self.current_error().unwrap_or_else(ClaimLeaseError::closed))?;
        result_receiver
            .await
            .map_err(|_| self.current_error().unwrap_or_else(ClaimLeaseError::closed))?
    }

    /// Complete the mandatory first poll and mint business-input authority only
    /// while the exact claim remains live and RUNNING.
    pub async fn activate(&mut self) -> ClaimLeaseActivation {
        let Some(execution) = self.activation.take() else {
            return ClaimLeaseActivation::Unavailable(ClaimLeaseError::MonitorClosed(
                "the claim lease activation was already consumed",
            ));
        };
        match self.check_now().await {
            Ok(()) => ClaimLeaseActivation::Active(execution.into_monitored()),
            Err(error) => ClaimLeaseActivation::Inactive {
                execution: execution.into_inactive(),
                error,
            },
        }
    }

    /// Fail immediately if cancellation or a fatal lease condition is latched.
    ///
    /// # Errors
    ///
    /// Returns the authoritative, data-free lease state.
    pub fn ensure_running(&self) -> Result<(), ClaimLeaseError> {
        self.current_error().map_or(Ok(()), Err)
    }

    /// Create a read-only post-authorization state observer.
    #[must_use]
    #[allow(dead_code)] // Consumed by the capability-disabled native invocation lifecycle.
    pub(crate) fn state_probe(&self) -> ClaimLeaseStateProbe {
        ClaimLeaseStateProbe {
            state: self.state.clone(),
            wrap_up: self.wrap_up.clone(),
            clock: Arc::clone(&self.clock),
            margin_millis: self.margin_millis,
            supervision_live: Arc::clone(&self.supervision_live),
            steering: self.steering.clone(),
            steering_ledger: Arc::clone(&self.steering_ledger),
            assertions: self.assertions.clone(),
        }
    }

    /// Wait until the server requests Stop or supervision fails.
    ///
    /// # Errors
    ///
    /// Always returns the state change cause; a successful return is impossible.
    pub async fn wait_for_state_change(&mut self) -> Result<(), ClaimLeaseError> {
        loop {
            if let Some(error) = self.current_error() {
                return Err(error);
            }
            self.state
                .changed()
                .await
                .map_err(|_| ClaimLeaseError::closed())?;
        }
    }

    /// Race a pre-invocation async edge against durable Stop/lease loss.
    ///
    /// Stop wins a ready tie. Dropping the operation is safe only before the
    /// synchronous ADK submission boundary; callers must not wrap a submitted
    /// or running synchronous invocation with this method.
    ///
    /// # Errors
    ///
    /// Returns the authoritative lease cause and cancels the pre-invocation
    /// future by dropping it.
    pub async fn run_pre_invocation<F>(
        &mut self,
        operation: F,
    ) -> Result<F::Output, ClaimLeaseError>
    where
        F: Future,
    {
        self.run_cancellation_safe_phase(operation).await
    }

    /// Race a locally cancellation-safe lifecycle phase against lease state.
    ///
    /// The supplied future must not own a submitted business effect. Durable
    /// Stop or fatal lease loss wins a ready tie and drops that future, while
    /// this monitor retains the unique lease authority for terminal/no-ACK
    /// cleanup. Native dependency assembly and post-EOS result selection use
    /// this boundary; Runner execution never does.
    pub(crate) async fn run_cancellation_safe_phase<F>(
        &mut self,
        operation: F,
    ) -> Result<F::Output, ClaimLeaseError>
    where
        F: Future,
    {
        self.ensure_running()?;
        tokio::pin!(operation);
        loop {
            tokio::select! {
                biased;
                changed = self.state.changed() => {
                    changed.map_err(|_| ClaimLeaseError::closed())?;
                    // `changed` does not mark the observed revision as seen.
                    // Without this acknowledgement, the biased branch stays
                    // ready forever and starves the protected operation.
                    drop(self.state.borrow_and_update());
                    self.ensure_running()?;
                }
                output = &mut operation => {
                    self.ensure_running()?;
                    return Ok(output);
                }
            }
        }
    }

    /// Signal shutdown and wait until the actor drops the unique lease handle.
    ///
    /// # Errors
    ///
    /// Returns a previously latched fatal state or an actor lifecycle failure.
    pub async fn close(mut self) -> Result<(), ClaimLeaseError> {
        self.supervision_live.store(false, Ordering::Release);
        let _ = self.shutdown.send(true);
        let actor = self.actor.take().ok_or_else(ClaimLeaseError::closed)?;
        actor.await.map_err(|_| ClaimLeaseError::closed())?;
        // The actor may have completed a poll between close entry and the
        // shutdown signal. Read state only after join so a newly latched fatal
        // cause cannot be hidden by an earlier Running or Cancelled snapshot.
        match self.current_error() {
            Some(ClaimLeaseError::Cancelled(_)) | None => Ok(()),
            Some(error) => Err(error),
        }
    }

    fn current_error(&self) -> Option<ClaimLeaseError> {
        match &*self.state.borrow() {
            MonitorState::Running { .. } => None,
            MonitorState::Cancelled => Some(ClaimLeaseError::cancelled()),
            MonitorState::Failed(error) => Some(error.clone()),
        }
    }

    fn current_fatal_error(&self) -> Option<ClaimLeaseError> {
        match &*self.state.borrow() {
            MonitorState::Failed(error) => Some(error.clone()),
            MonitorState::Running { .. } | MonitorState::Cancelled => None,
        }
    }
}

#[allow(dead_code)] // Consumed by the capability-disabled native invocation lifecycle.
fn monitor_state_result(state: &MonitorState) -> Result<(), ClaimLeaseError> {
    match state {
        MonitorState::Running { .. } => Ok(()),
        MonitorState::Cancelled => Err(ClaimLeaseError::cancelled()),
        MonitorState::Failed(error) => Err(error.clone()),
    }
}

impl Drop for ClaimLeaseMonitor {
    fn drop(&mut self) {
        self.supervision_live.store(false, Ordering::Release);
        let _ = self.shutdown.send(true);
    }
}

struct LeaseActorLifetime(Arc<AtomicBool>);

impl Drop for LeaseActorLifetime {
    fn drop(&mut self) {
        self.0.store(false, Ordering::Release);
    }
}

struct LeaseActorChannels {
    commands: mpsc::Receiver<MonitorCommand>,
    steering: mpsc::Receiver<SteeringClaim>,
    assertions: mpsc::Receiver<AssertionClaim>,
    state: watch::Sender<MonitorState>,
    wrap_up: watch::Sender<bool>,
    shutdown: watch::Receiver<bool>,
    supervision_live: Arc<AtomicBool>,
}

async fn run_lease_actor<R, K>(
    control: Arc<AgentControlClient<R>>,
    mut lease: ClaimLeaseHandle,
    clock: Arc<K>,
    config: ValidatedLeaseConfig,
    channels: LeaseActorChannels,
) where
    R: ControlRpc + 'static,
    K: UnixMillisClock,
{
    let LeaseActorChannels {
        mut commands,
        mut steering,
        mut assertions,
        state,
        wrap_up,
        mut shutdown,
        supervision_live,
    } = channels;
    let _lifetime = LeaseActorLifetime(supervision_live);
    let start = Instant::now() + config.poll_interval;
    let mut ticker = interval_at(start, config.poll_interval);
    ticker.set_missed_tick_behavior(MissedTickBehavior::Skip);
    let mut cancellation_latched = false;
    // A Main without the steering RPC answers the same way every time: stop
    // asking for the rest of this claim.
    let mut steering_served = true;

    loop {
        if *shutdown.borrow() {
            return;
        }
        tokio::select! {
            biased;
            changed = shutdown.changed() => {
                let _ = changed;
                return;
            }
            command = commands.recv() => {
                let Some(MonitorCommand::CheckNow(reply)) = command else {
                    return;
                };
                let Some(result) = poll_or_shutdown(
                    &control,
                    &mut lease,
                    clock.as_ref(),
                    config,
                    &mut shutdown,
                ).await else {
                    let _ = reply.send(Err(ClaimLeaseError::closed()));
                    return;
                };
                record_wrap_up(&result, &wrap_up);
                let (reply_result, keep_running) = record_poll_result(
                    result.map(|(expiry, _)| expiry),
                    &mut cancellation_latched,
                    &state,
                );
                let _ = reply.send(reply_result);
                if !keep_running {
                    return;
                }
            }
            Some(call) = steering.recv() => {
                let claimed = if steering_served {
                    tokio::select! {
                        biased;
                        changed = shutdown.changed() => {
                            let _ = changed;
                            return;
                        }
                        claimed = control.claim_steering(
                            &lease,
                            call.acknowledged,
                            call.acknowledge_only,
                        ) => steering_claim_result(claimed, &mut steering_served),
                    }
                } else {
                    None
                };
                let _ = call.reply.send(claimed);
            }
            Some(call) = assertions.recv() => {
                if !mint_assertion(&control, &lease, call, &mut shutdown).await {
                    return;
                }
            }
            _ = ticker.tick() => {
                let Some(result) = poll_or_shutdown(
                    &control,
                    &mut lease,
                    clock.as_ref(),
                    config,
                    &mut shutdown,
                ).await else {
                    return;
                };
                record_wrap_up(&result, &wrap_up);
                let (_, keep_running) = record_poll_result(
                    result.map(|(expiry, _)| expiry),
                    &mut cancellation_latched,
                    &state,
                );
                if !keep_running {
                    return;
                }
            }
        }
    }
}

/// One delegation assertion mint (ADR-0044 decision 4), answered to the
/// probe; `None` when Main refused or did not answer. `false` when shutdown
/// interrupted it.
async fn mint_assertion<R>(
    control: &AgentControlClient<R>,
    lease: &ClaimLeaseHandle,
    call: AssertionClaim,
    shutdown: &mut watch::Receiver<bool>,
) -> bool
where
    R: ControlRpc + 'static,
{
    let AssertionClaim { audience, reply } = call;
    let minted = tokio::select! {
        biased;
        changed = shutdown.changed() => {
            let _ = changed;
            return false;
        }
        minted = control.mint_delegation_assertion(lease, audience) => minted,
    };
    let minted = minted
        .map_err(|error| {
            tracing::warn!(error = %error, "the delegation assertion was not minted");
        })
        .ok();
    let _ = reply.send(minted);
    true
}

/// The answer of one steering call, or `None`. A refusal never ends the
/// turn: the messages wait. Only a Main that does not serve the RPC turns
/// steering off for the rest of the claim.
fn steering_claim_result(
    claimed: Result<Vec<(String, String)>, AgentControlError>,
    served: &mut bool,
) -> Option<Vec<(String, String)>> {
    match claimed {
        Ok(messages) => Some(messages),
        Err(error) => {
            let unsupported = match &error {
                AgentControlError::Transport(
                    crate::transport::ControlGrpcError::InvalidConfiguration(_),
                ) => true,
                AgentControlError::Semantic(ControlSemanticError::Rejected(rejection)) => {
                    rejection.kind() == RuntimeControlRejectionKind::UnsupportedCapability
                }
                _ => false,
            };
            if unsupported {
                *served = false;
            }
            tracing::warn!(error = %error, unsupported, "the steering claim was refused");
            None
        }
    }
}

async fn poll_or_shutdown<R, K>(
    control: &AgentControlClient<R>,
    lease: &mut ClaimLeaseHandle,
    clock: &K,
    config: ValidatedLeaseConfig,
    shutdown: &mut watch::Receiver<bool>,
) -> Option<Result<(i64, bool), ClaimLeaseError>>
where
    R: ControlRpc,
    K: UnixMillisClock,
{
    if *shutdown.borrow() {
        return None;
    }
    tokio::select! {
        biased;
        changed = shutdown.changed() => {
            let _ = changed;
            None
        }
        result = poll_once(control, lease, clock, config) => Some(result),
    }
}

async fn poll_once<R, K>(
    control: &AgentControlClient<R>,
    lease: &mut ClaimLeaseHandle,
    clock: &K,
    config: ValidatedLeaseConfig,
) -> Result<(i64, bool), ClaimLeaseError>
where
    R: ControlRpc,
    K: UnixMillisClock,
{
    let renewal = control
        .renew_lease(lease)
        .await
        .map_err(ClaimLeaseError::from_control)?;
    let observed = control
        .observe_lease(lease)
        .await
        .map_err(ClaimLeaseError::from_control)?;
    let now_unix_millis = clock.now_unix_millis();
    if now_unix_millis <= 0 {
        return Err(ClaimLeaseError::InvalidClock(
            "the wall clock cannot validate the renewed claim lease",
        ));
    }
    if renewal
        .lease_expires_at_unix_millis
        .checked_sub(now_unix_millis)
        .is_none_or(|remaining| remaining < config.renewal_margin_millis)
    {
        return Err(ClaimLeaseError::LeaseLost(
            "the renewed claim lease has insufficient execution margin",
        ));
    }

    // Match the Python boundary: the valid renewal becomes the next monotonic
    // baseline before either desired-state value is interpreted.
    lease.commit_renewal(renewal);
    require_running(renewal.desired_state)?;
    require_running(observed)?;
    Ok((
        renewal.lease_expires_at_unix_millis,
        renewal.workspace_wrap_up,
    ))
}

/// Latch the take-back signal (it never turns back off for this claim).
fn record_wrap_up(result: &Result<(i64, bool), ClaimLeaseError>, wrap_up: &watch::Sender<bool>) {
    if matches!(result, Ok((_, true))) && !*wrap_up.borrow() {
        wrap_up.send_replace(true);
    }
}

fn require_running(state: DesiredExecutionState) -> Result<(), ClaimLeaseError> {
    match state {
        DesiredExecutionState::Running => Ok(()),
        DesiredExecutionState::Suspended => Err(ClaimLeaseError::LeaseLost(
            "the execution is suspended and requires recovery-only authority",
        )),
        DesiredExecutionState::Cancelled => Err(ClaimLeaseError::cancelled()),
        DesiredExecutionState::Draining => Err(ClaimLeaseError::Draining(
            "the execution is draining and cannot continue on this worker",
        )),
    }
}

fn record_poll_result(
    result: Result<i64, ClaimLeaseError>,
    cancellation_latched: &mut bool,
    state: &watch::Sender<MonitorState>,
) -> (Result<(), ClaimLeaseError>, bool) {
    match result {
        Ok(_) if *cancellation_latched => (Err(ClaimLeaseError::cancelled()), true),
        Ok(lease_expires_at_unix_millis) => {
            state.send_replace(MonitorState::Running {
                lease_expires_at_unix_millis,
            });
            (Ok(()), true)
        }
        Err(error @ ClaimLeaseError::Cancelled(_)) => {
            *cancellation_latched = true;
            state.send_replace(MonitorState::Cancelled);
            (Err(error), true)
        }
        Err(error) => {
            state.send_replace(MonitorState::Failed(error.clone()));
            (Err(error), false)
        }
    }
}
