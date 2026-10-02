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
    state: watch::Receiver<MonitorState>,
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
    clock: Arc<dyn UnixMillisClock>,
    margin_millis: i64,
    supervision_live: Arc<AtomicBool>,
}

#[allow(dead_code)] // Consumed by the capability-disabled native invocation lifecycle.
impl Clone for ClaimLeaseStateProbe {
    fn clone(&self) -> Self {
        Self {
            state: self.state.clone(),
            clock: Arc::clone(&self.clock),
            margin_millis: self.margin_millis,
            supervision_live: Arc::clone(&self.supervision_live),
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
        let (state_sender, state_receiver) = watch::channel(MonitorState::Running {
            lease_expires_at_unix_millis: initial_expiry,
        });
        let (shutdown_sender, shutdown_receiver) = watch::channel(false);
        let supervision_live = Arc::new(AtomicBool::new(true));
        let actor = tokio::spawn(run_lease_actor(
            control,
            lease,
            clock,
            config,
            LeaseActorChannels {
                commands: command_receiver,
                state: state_sender,
                shutdown: shutdown_receiver,
                supervision_live: Arc::clone(&supervision_live),
            },
        ));
        Self {
            commands: command_sender,
            state: state_receiver,
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
            clock: Arc::clone(&self.clock),
            margin_millis: self.margin_millis,
            supervision_live: Arc::clone(&self.supervision_live),
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
    state: watch::Sender<MonitorState>,
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
        state,
        mut shutdown,
        supervision_live,
    } = channels;
    let _lifetime = LeaseActorLifetime(supervision_live);
    let start = Instant::now() + config.poll_interval;
    let mut ticker = interval_at(start, config.poll_interval);
    ticker.set_missed_tick_behavior(MissedTickBehavior::Skip);
    let mut cancellation_latched = false;

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
                let (reply_result, keep_running) = record_poll_result(
                    result,
                    &mut cancellation_latched,
                    &state,
                );
                let _ = reply.send(reply_result);
                if !keep_running {
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
                let (_, keep_running) = record_poll_result(
                    result,
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

async fn poll_or_shutdown<R, K>(
    control: &AgentControlClient<R>,
    lease: &mut ClaimLeaseHandle,
    clock: &K,
    config: ValidatedLeaseConfig,
    shutdown: &mut watch::Receiver<bool>,
) -> Option<Result<i64, ClaimLeaseError>>
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
) -> Result<i64, ClaimLeaseError>
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
    Ok(renewal.lease_expires_at_unix_millis)
}

fn require_running(state: DesiredExecutionState) -> Result<(), ClaimLeaseError> {
    match state {
        DesiredExecutionState::Running => Ok(()),
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
