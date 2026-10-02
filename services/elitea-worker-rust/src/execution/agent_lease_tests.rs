use std::future::pending;
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use tokio::sync::Notify;
use tokio::time::timeout;
use tonic::{Request, Response, Status};

use super::agent_lease::{
    ClaimLeaseActivation, ClaimLeaseError, ClaimLeaseErrorCode, ClaimLeaseMonitor,
    ClaimLeaseMonitorConfig,
};
use crate::protocol::control::{
    AgentControlClient, test_lease_starting_execution, test_terminal_claim_recovery,
};
use crate::protocol::elitea::runtime::v1::{
    AuthorizeInvocationRequestV1, AuthorizeInvocationResponseV1, BeginExecutionRequestV1,
    BeginExecutionResponseV1, ClaimCommandRequestV1, ClaimCommandResponseV1,
    DesiredExecutionStateV1, ObserveDesiredStateRequestV1, ObserveDesiredStateResponseV1,
    PrepareSettlementRequestV1, PrepareSettlementResponseV1, RenewLeaseRequestV1,
    RenewLeaseResponseV1, RuntimeErrorCodeV1, RuntimeErrorV1,
};
use crate::transport::{ControlGrpcConfig, ControlRpc};

const NOW: i64 = 1_700_000_000_000;

struct FakeState {
    renew: Mutex<RenewLeaseResponseV1>,
    observe: Mutex<ObserveDesiredStateResponseV1>,
    calls: Mutex<Vec<&'static str>>,
    renew_requests: Mutex<Vec<RenewLeaseRequestV1>>,
    clock_after_observe: Mutex<Option<(Arc<AtomicI64>, i64)>>,
    stall_renew: AtomicBool,
    renew_started: Notify,
    renew_active: AtomicBool,
}

impl Default for FakeState {
    fn default() -> Self {
        Self {
            renew: Mutex::new(RenewLeaseResponseV1 {
                lease_expires_at_unix_millis: NOW + 60_000,
                desired_state: DesiredExecutionStateV1::Running as i32,
                rejection: None,
            }),
            observe: Mutex::new(ObserveDesiredStateResponseV1 {
                desired_state: DesiredExecutionStateV1::Running as i32,
                rejection: None,
            }),
            calls: Mutex::new(Vec::new()),
            renew_requests: Mutex::new(Vec::new()),
            clock_after_observe: Mutex::new(None),
            stall_renew: AtomicBool::new(false),
            renew_started: Notify::new(),
            renew_active: AtomicBool::new(false),
        }
    }
}

#[derive(Clone)]
struct FakeRpc(Arc<FakeState>);

struct ActiveRenew<'a>(&'a AtomicBool);

impl Drop for ActiveRenew<'_> {
    fn drop(&mut self) {
        self.0.store(false, Ordering::SeqCst);
    }
}

#[async_trait]
impl ControlRpc for FakeRpc {
    async fn claim_command(
        &self,
        _request: Request<ClaimCommandRequestV1>,
    ) -> Result<Response<ClaimCommandResponseV1>, Status> {
        panic!("the lease monitor must not claim")
    }

    async fn begin_execution(
        &self,
        _request: Request<BeginExecutionRequestV1>,
    ) -> Result<Response<BeginExecutionResponseV1>, Status> {
        panic!("the lease monitor must not begin execution")
    }

    async fn authorize_invocation(
        &self,
        _request: Request<AuthorizeInvocationRequestV1>,
    ) -> Result<Response<AuthorizeInvocationResponseV1>, Status> {
        panic!("the lease monitor must not authorize invocation")
    }

    async fn renew_lease(
        &self,
        request: Request<RenewLeaseRequestV1>,
    ) -> Result<Response<RenewLeaseResponseV1>, Status> {
        self.0.calls.lock().expect("calls").push("renew");
        self.0
            .renew_requests
            .lock()
            .expect("renew requests")
            .push(request.into_inner());
        if self.0.stall_renew.load(Ordering::SeqCst) {
            self.0.renew_active.store(true, Ordering::SeqCst);
            let _active = ActiveRenew(&self.0.renew_active);
            self.0.renew_started.notify_one();
            pending::<()>().await;
        }
        Ok(Response::new(self.0.renew.lock().expect("renew").clone()))
    }

    async fn observe_desired_state(
        &self,
        _request: Request<ObserveDesiredStateRequestV1>,
    ) -> Result<Response<ObserveDesiredStateResponseV1>, Status> {
        self.0.calls.lock().expect("calls").push("observe");
        if let Some((clock, value)) = self
            .0
            .clock_after_observe
            .lock()
            .expect("clock update")
            .as_ref()
        {
            clock.store(*value, Ordering::SeqCst);
        }
        Ok(Response::new(
            self.0.observe.lock().expect("observe").clone(),
        ))
    }

    async fn prepare_settlement(
        &self,
        _request: Request<PrepareSettlementRequestV1>,
    ) -> Result<Response<PrepareSettlementResponseV1>, Status> {
        panic!("the lease monitor must not prepare settlement")
    }
}

fn client(state: Arc<FakeState>) -> Arc<AgentControlClient<FakeRpc>> {
    Arc::new(
        AgentControlClient::new(
            FakeRpc(state),
            ControlGrpcConfig {
                deadline: Duration::from_secs(1),
                workload_session_id: "session-1".to_owned(),
                producer_id: "producer-1".to_owned(),
            },
        )
        .expect("control client"),
    )
}

fn config(poll_interval: Duration) -> ClaimLeaseMonitorConfig {
    ClaimLeaseMonitorConfig::new(poll_interval).expect("valid lease interval")
}

#[tokio::test(flavor = "current_thread")]
async fn immediate_poll_is_sequential_and_uses_unique_renewal_keys() {
    let state = Arc::new(FakeState::default());
    let clock = Arc::new(|| NOW);
    let monitor = ClaimLeaseMonitor::start(
        client(Arc::clone(&state)),
        test_lease_starting_execution(NOW + 60_000),
        clock,
        config(Duration::from_secs(10)),
    );

    monitor.check_now().await.expect("first poll");
    monitor.check_now().await.expect("second poll");
    assert_eq!(
        *state.calls.lock().expect("calls"),
        ["renew", "observe", "renew", "observe"]
    );
    {
        let requests = state.renew_requests.lock().expect("renew requests");
        assert!(requests[0].idempotency_key.ends_with(":1"));
        assert!(requests[1].idempotency_key.ends_with(":2"));
        assert_ne!(requests[0].idempotency_key, requests[1].idempotency_key);
    }
    monitor.close().await.expect("close");
}

#[tokio::test(flavor = "current_thread")]
async fn state_probe_keeps_one_poll_for_renewal_and_fails_closed_before_expiry() {
    let state = Arc::new(FakeState::default());
    let clock = Arc::new(AtomicI64::new(NOW));
    let sampled_clock = {
        let clock = Arc::clone(&clock);
        Arc::new(move || clock.load(Ordering::SeqCst))
    };
    let monitor = ClaimLeaseMonitor::start(
        client(state),
        test_lease_starting_execution(NOW + 30_000),
        sampled_clock,
        config(Duration::from_secs(10)),
    );
    let probe = monitor.state_probe();
    probe.ensure_running().expect("initial state-write margin");

    clock.store(NOW + 10_001, Ordering::SeqCst);
    probe
        .ensure_running()
        .expect("periodic renewal boundary remains writable");

    clock.store(NOW + 20_001, Ordering::SeqCst);
    let expired = probe.ensure_running().expect_err("insufficient margin");
    assert_eq!(expired.code(), ClaimLeaseErrorCode::LeaseLost);

    monitor.close().await.expect("close monitor");
    let closed = probe.ensure_running().expect_err("closed monitor");
    assert_eq!(closed.code(), ClaimLeaseErrorCode::MonitorClosed);
}

#[tokio::test(flavor = "current_thread")]
async fn successful_renewal_extends_the_state_probe_deadline() {
    let state = Arc::new(FakeState::default());
    let clock = Arc::new(AtomicI64::new(NOW));
    let sampled_clock = {
        let clock = Arc::clone(&clock);
        Arc::new(move || clock.load(Ordering::SeqCst))
    };
    let monitor = ClaimLeaseMonitor::start(
        client(state),
        test_lease_starting_execution(NOW + 20_500),
        sampled_clock,
        config(Duration::from_secs(10)),
    );
    let probe = monitor.state_probe();
    probe.ensure_running().expect("initial state-write margin");

    monitor.check_now().await.expect("renew lease");
    clock.store(NOW + 30_000, Ordering::SeqCst);
    probe
        .ensure_running()
        .expect("renewed state-write margin is published");
    monitor.close().await.expect("close monitor");
    assert_eq!(
        probe
            .ensure_running()
            .expect_err("closed monitor after unseen renewal")
            .code(),
        ClaimLeaseErrorCode::MonitorClosed
    );
}

#[tokio::test(flavor = "current_thread")]
async fn margin_uses_clock_sampled_after_desired_state_observation() {
    let state = Arc::new(FakeState::default());
    let clock = Arc::new(AtomicI64::new(NOW));
    *state.clock_after_observe.lock().expect("clock update") =
        Some((Arc::clone(&clock), NOW + 40_001));
    let sampled_clock = {
        let clock = Arc::clone(&clock);
        Arc::new(move || clock.load(Ordering::SeqCst))
    };
    let monitor = ClaimLeaseMonitor::start(
        client(state),
        test_lease_starting_execution(NOW + 60_000),
        sampled_clock,
        config(Duration::from_secs(10)),
    );

    let error = monitor.check_now().await.expect_err("insufficient margin");
    assert_eq!(error.code(), ClaimLeaseErrorCode::LeaseLost);
    assert!(!error.retryable());
    assert_eq!(
        error.to_string(),
        "the renewed claim lease has insufficient execution margin"
    );
    assert!(matches!(
        monitor.close().await,
        Err(ClaimLeaseError::LeaseLost(_))
    ));
}

#[tokio::test(flavor = "current_thread")]
async fn cancellation_is_latched_while_manual_renewal_continues() {
    let state = Arc::new(FakeState::default());
    state.renew.lock().expect("renew").desired_state = DesiredExecutionStateV1::Cancelled as i32;
    let monitor = ClaimLeaseMonitor::start(
        client(Arc::clone(&state)),
        test_lease_starting_execution(NOW + 60_000),
        Arc::new(|| NOW),
        config(Duration::from_secs(10)),
    );

    let first = monitor.check_now().await.expect_err("cancelled");
    assert_eq!(first.code(), ClaimLeaseErrorCode::Cancelled);
    state.renew.lock().expect("renew").desired_state = DesiredExecutionStateV1::Running as i32;
    let second = monitor
        .check_now()
        .await
        .expect_err("cancellation remains latched");
    assert_eq!(second.code(), ClaimLeaseErrorCode::Cancelled);
    assert_eq!(
        state.renew_requests.lock().expect("renew requests").len(),
        2
    );
    assert!(matches!(
        monitor.ensure_running(),
        Err(ClaimLeaseError::Cancelled(_))
    ));
    monitor.close().await.expect("cancelled close");
}

#[tokio::test(flavor = "current_thread")]
async fn a_fatal_failure_after_cancellation_becomes_authoritative() {
    let state = Arc::new(FakeState::default());
    state.renew.lock().expect("renew").desired_state = DesiredExecutionStateV1::Cancelled as i32;
    let monitor = ClaimLeaseMonitor::start(
        client(Arc::clone(&state)),
        test_lease_starting_execution(NOW + 60_000),
        Arc::new(|| NOW),
        config(Duration::from_secs(10)),
    );
    let mut probe = monitor.state_probe();
    probe.ensure_running().expect("initial probe state");
    assert_eq!(
        monitor.check_now().await.expect_err("cancelled").code(),
        ClaimLeaseErrorCode::Cancelled
    );
    assert_eq!(
        probe
            .wait_for_change()
            .await
            .expect_err("probe observes cancellation")
            .code(),
        ClaimLeaseErrorCode::Cancelled
    );

    *state.renew.lock().expect("renew") = RenewLeaseResponseV1 {
        lease_expires_at_unix_millis: 0,
        desired_state: DesiredExecutionStateV1::Unspecified as i32,
        rejection: Some(RuntimeErrorV1 {
            code: RuntimeErrorCodeV1::Internal as i32,
            safe_message: "must not be exposed".to_owned(),
            retryable: true,
        }),
    };
    let fatal = monitor
        .check_now()
        .await
        .expect_err("fatal renewal failure");
    assert_eq!(fatal.code(), ClaimLeaseErrorCode::Control);
    assert!(fatal.retryable());
    assert_eq!(
        fatal.to_string(),
        "claim lease control failed: a required runtime control dependency is unavailable"
    );
    assert!(matches!(
        monitor.ensure_running(),
        Err(ClaimLeaseError::Control(_))
    ));
    assert_eq!(
        probe
            .wait_for_change()
            .await
            .expect_err("probe observes later fatal state")
            .code(),
        ClaimLeaseErrorCode::Control
    );
    assert!(monitor.close().await.is_err());
}

#[tokio::test(flavor = "current_thread", start_paused = true)]
async fn periodic_schedule_skips_missed_intervals_without_a_burst() {
    let state = Arc::new(FakeState::default());
    let monitor = ClaimLeaseMonitor::start(
        client(Arc::clone(&state)),
        test_lease_starting_execution(NOW + 60_000),
        Arc::new(|| NOW),
        config(Duration::from_secs(10)),
    );
    monitor.check_now().await.expect("immediate poll");

    tokio::time::advance(Duration::from_secs(35)).await;
    tokio::task::yield_now().await;
    assert_eq!(
        state.renew_requests.lock().expect("renew requests").len(),
        2,
        "only one overdue periodic poll may run"
    );

    tokio::time::advance(Duration::from_secs(5)).await;
    tokio::task::yield_now().await;
    assert_eq!(
        state.renew_requests.lock().expect("renew requests").len(),
        3
    );
    monitor.close().await.expect("close");
}

#[tokio::test(flavor = "current_thread", start_paused = true)]
async fn terminal_recovery_starts_periodic_supervision_without_a_pre_replay_poll() {
    let state = Arc::new(FakeState::default());
    let monitor = ClaimLeaseMonitor::start_recovery(
        client(Arc::clone(&state)),
        test_terminal_claim_recovery(NOW + 60_000),
        Arc::new(|| NOW),
        config(Duration::from_secs(10)),
    );

    tokio::task::yield_now().await;
    assert!(
        state.calls.lock().expect("calls").is_empty(),
        "the exact durable terminal must be replayed before a new desired-state poll"
    );

    tokio::time::advance(Duration::from_secs(10)).await;
    tokio::task::yield_now().await;
    assert_eq!(*state.calls.lock().expect("calls"), ["renew", "observe"]);
    monitor.close().await.expect("close");
}

#[tokio::test(flavor = "current_thread")]
async fn draining_is_a_retryable_fatal_state_with_stable_operator_text() {
    let state = Arc::new(FakeState::default());
    state.observe.lock().expect("observe").desired_state = DesiredExecutionStateV1::Draining as i32;
    let monitor = ClaimLeaseMonitor::start(
        client(state),
        test_lease_starting_execution(NOW + 60_000),
        Arc::new(|| NOW),
        config(Duration::from_secs(10)),
    );

    let error = monitor.check_now().await.expect_err("draining");
    assert_eq!(error.code().as_str(), "claim_lease_draining");
    assert!(error.retryable());
    assert_eq!(
        error.to_string(),
        "the execution is draining and cannot continue on this worker"
    );
    assert!(matches!(
        monitor.close().await,
        Err(ClaimLeaseError::Draining(_))
    ));
}

struct ActiveOperation(Arc<AtomicBool>);

impl Drop for ActiveOperation {
    fn drop(&mut self) {
        self.0.store(false, Ordering::SeqCst);
    }
}

#[tokio::test(flavor = "current_thread")]
async fn durable_stop_cancels_only_a_pre_invocation_future() {
    let state = Arc::new(FakeState::default());
    state.renew.lock().expect("renew").desired_state = DesiredExecutionStateV1::Cancelled as i32;
    let mut monitor = ClaimLeaseMonitor::start(
        client(state),
        test_lease_starting_execution(NOW + 60_000),
        Arc::new(|| NOW),
        config(Duration::from_millis(10)),
    );
    let active = Arc::new(AtomicBool::new(false));
    let operation_active = Arc::clone(&active);
    let operation = async move {
        operation_active.store(true, Ordering::SeqCst);
        let _active = ActiveOperation(Arc::clone(&operation_active));
        pending::<()>().await;
    };

    let error = timeout(
        Duration::from_secs(1),
        monitor.run_pre_invocation(operation),
    )
    .await
    .expect("bounded cancellation")
    .expect_err("durable stop");
    assert_eq!(error.code(), ClaimLeaseErrorCode::Cancelled);
    assert!(!active.load(Ordering::SeqCst));
    monitor.close().await.expect("close");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn activation_revision_does_not_starve_pre_invocation_work() {
    let state = Arc::new(FakeState::default());
    let mut monitor = ClaimLeaseMonitor::start(
        client(Arc::clone(&state)),
        test_lease_starting_execution(NOW + 60_000),
        Arc::new(|| NOW),
        config(Duration::from_secs(10)),
    );
    let ClaimLeaseActivation::Active(execution) = monitor.activate().await else {
        panic!("lease activation must succeed");
    };

    let value = timeout(
        Duration::from_secs(1),
        monitor.run_pre_invocation(async { "materialized" }),
    )
    .await
    .expect("pre-invocation operation must not be starved")
    .expect("lease remains active");

    assert_eq!(value, "materialized");
    assert_eq!(
        state.renew_requests.lock().expect("renew requests").len(),
        1
    );
    drop(execution);
    monitor.close().await.expect("close");
}

#[tokio::test(flavor = "current_thread")]
async fn close_cancels_a_stalled_control_attempt_and_releases_authority() {
    let state = Arc::new(FakeState::default());
    state.stall_renew.store(true, Ordering::SeqCst);
    let monitor = ClaimLeaseMonitor::start(
        client(Arc::clone(&state)),
        test_lease_starting_execution(NOW + 60_000),
        Arc::new(|| NOW),
        config(Duration::from_millis(1)),
    );

    timeout(Duration::from_secs(1), state.renew_started.notified())
        .await
        .expect("renew started");
    assert!(state.renew_active.load(Ordering::SeqCst));
    timeout(Duration::from_secs(1), monitor.close())
        .await
        .expect("bounded close")
        .expect("close");
    assert!(!state.renew_active.load(Ordering::SeqCst));
}

#[test]
fn polling_configuration_matches_the_integer_millisecond_v1_profile() {
    assert!(ClaimLeaseMonitorConfig::new(Duration::from_millis(1)).is_ok());
    assert!(ClaimLeaseMonitorConfig::new(Duration::from_secs(10)).is_ok());
    for invalid in [
        Duration::ZERO,
        Duration::from_nanos(999_999),
        Duration::from_nanos(1_000_001),
        Duration::from_millis(10_001),
    ] {
        let error = ClaimLeaseMonitorConfig::new(invalid).expect_err("invalid interval");
        assert_eq!(error.code(), ClaimLeaseErrorCode::InvalidConfiguration);
        assert!(!error.retryable());
    }
}

#[tokio::test]
async fn checkpoint_inspection_requires_one_live_poll_without_fresh_authority() {
    let state = Arc::new(FakeState::default());
    let mut monitor = ClaimLeaseMonitor::start_checkpoint_inspection(
        client(state.clone()),
        crate::protocol::control::test_model_checkpoint_inspection(NOW + 60_000),
        Arc::new(|| NOW),
        config(Duration::from_secs(10)),
    );
    assert!(matches!(
        monitor.activate().await,
        ClaimLeaseActivation::Unavailable(_)
    ));
    assert!(state.calls.lock().expect("calls").is_empty());
    let inspection = monitor
        .activate_checkpoint_inspection()
        .await
        .unwrap_or_else(|_| panic!("live inspection"));
    assert_eq!(*state.calls.lock().expect("calls"), ["renew", "observe"]);
    let (_pending, session) = inspection.into_session_inspection();
    assert_eq!(session.into_writer_binding().execution_id, "execution/one");
    assert!(monitor.activate_checkpoint_inspection().await.is_err());
    monitor.close().await.expect("close");
}

#[tokio::test]
async fn cancelled_or_expired_checkpoint_poll_never_issues_session_access() {
    for cancelled in [true, false] {
        let state = Arc::new(FakeState::default());
        let clock = Arc::new(AtomicI64::new(NOW));
        if cancelled {
            state.observe.lock().expect("observe").desired_state =
                DesiredExecutionStateV1::Cancelled as i32;
        } else {
            *state.clock_after_observe.lock().expect("clock") = Some((clock.clone(), NOW + 70_000));
        }
        let observed_clock = clock.clone();
        let mut monitor = ClaimLeaseMonitor::start_checkpoint_inspection(
            client(state),
            crate::protocol::control::test_model_checkpoint_inspection(NOW + 60_000),
            Arc::new(move || observed_clock.load(Ordering::SeqCst)),
            config(Duration::from_secs(10)),
        );
        assert!(monitor.activate_checkpoint_inspection().await.is_err());
        assert!(monitor.activate_checkpoint_inspection().await.is_err());
        let closed = monitor.close().await;
        if cancelled {
            closed.expect("cancelled close");
        } else {
            assert!(matches!(closed, Err(ClaimLeaseError::LeaseLost(_))));
        }
    }
}
