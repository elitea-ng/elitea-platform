use std::collections::{BTreeMap, VecDeque};
use std::fs;
use std::future::pending;
use std::os::unix::fs::PermissionsExt;
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use prost::Message;
use tonic::transport::Channel;
use tonic::{Request, Response, Status};

use super::agent_delivery::test_fresh_agent_delivery;
use super::agent_preparation::{
    AgentInputMaterializer, AgentPreparationConfig, AgentPreparationErrorCode,
    AgentPreparationOutcome, PreInvocationTerminalCause, prepare_fresh_agent_invocation,
    prepare_fresh_agent_invocation_with,
};
use super::invocation_admission::{InvocationAdmission, InvocationAdmissionConfig};
use super::output_delivery::{AgentOutputPreflight, AgentOutputPreflightOutcome, EmptyAgentOutput};
use crate::agents::AgentExecutionKind;
use crate::protocol::command::{
    SignedCommandAuthenticator, TestOnlyConformanceHmacAuthenticator,
    parse_and_verify_agent_command,
};
use crate::protocol::control::{
    AgentControlClient, AgentControlError, InvocationAuthorizationDecision,
    LeaseMonitoredAgentExecution,
};
use crate::protocol::elitea::runtime::v1::{
    AuthorizeInvocationDispositionV1, AuthorizeInvocationRequestV1, AuthorizeInvocationResponseV1,
    BeginExecutionDispositionV1, BeginExecutionRequestV1, BeginExecutionResponseV1,
    ClaimCommandRequestV1, ClaimCommandResponseV1, DesiredExecutionStateV1,
    ObserveDesiredStateRequestV1, ObserveDesiredStateResponseV1, PrepareSettlementRequestV1,
    PrepareSettlementResponseV1, RenewLeaseRequestV1, RenewLeaseResponseV1, RuntimeErrorCodeV1,
    RuntimeErrorV1,
};
use crate::protocol::output::RuntimeFailureKind;
use crate::spool::{SpoolLimits, SpoolMasterKey};
use crate::transport::redis_commands::{RedisCommandDelivery, RedisCommandLimits};
use crate::transport::{
    ControlGrpcConfig, ControlRpc, InputContentError, MaterializedInput, OutputGrpcConfig,
};

const NOW: i64 = 1_700_000_000_000;
const DEADLINE: i64 = 1_700_000_100_000;

fn vectors() -> BTreeMap<&'static str, &'static str> {
    include_str!("../../tests/fixtures/agent_control_vectors.txt")
        .lines()
        .map(|line| line.split_once('=').expect("named fixture"))
        .collect()
}

fn decode_hex(raw: &str) -> Vec<u8> {
    raw.trim()
        .as_bytes()
        .chunks_exact(2)
        .map(|pair| {
            u8::from_str_radix(std::str::from_utf8(pair).expect("ASCII hex"), 16)
                .expect("fixture hex")
        })
        .collect()
}

fn bytes(name: &str) -> Vec<u8> {
    decode_hex(vectors()[name])
}

fn input_fixture(kind: AgentExecutionKind) -> Vec<u8> {
    match kind {
        AgentExecutionKind::Application => decode_hex(include_str!(
            "../../tests/fixtures/agent_application_input.hex"
        )),
        AgentExecutionKind::Adhoc => {
            decode_hex(include_str!("../../tests/fixtures/agent_adhoc_input.hex"))
        }
    }
}

struct TestControlState {
    calls: Mutex<Vec<&'static str>>,
    claim: ClaimCommandResponseV1,
    begin: Mutex<BeginExecutionResponseV1>,
    authorize: Mutex<AuthorizeInvocationResponseV1>,
    authorize_unavailable: AtomicBool,
    renew: Mutex<RenewLeaseResponseV1>,
    observe: Mutex<ObserveDesiredStateResponseV1>,
    observe_queue: Mutex<VecDeque<ObserveDesiredStateResponseV1>>,
}

impl Default for TestControlState {
    fn default() -> Self {
        Self {
            calls: Mutex::new(Vec::new()),
            claim: ClaimCommandResponseV1::decode(bytes("accepted_claim").as_slice())
                .expect("claim fixture"),
            begin: Mutex::new(BeginExecutionResponseV1 {
                disposition: BeginExecutionDispositionV1::StartedNow as i32,
                rejection: None,
            }),
            authorize: Mutex::new(AuthorizeInvocationResponseV1 {
                disposition: AuthorizeInvocationDispositionV1::AuthorizedNow as i32,
                rejection: None,
            }),
            authorize_unavailable: AtomicBool::new(false),
            renew: Mutex::new(RenewLeaseResponseV1 {
                lease_expires_at_unix_millis: NOW + 60_000,
                desired_state: DesiredExecutionStateV1::Running as i32,
                rejection: None,
            }),
            observe: Mutex::new(ObserveDesiredStateResponseV1 {
                desired_state: DesiredExecutionStateV1::Running as i32,
                rejection: None,
            }),
            observe_queue: Mutex::new(VecDeque::new()),
        }
    }
}

#[derive(Clone)]
struct TestControlRpc(Arc<TestControlState>);

#[async_trait]
impl ControlRpc for TestControlRpc {
    async fn claim_command(
        &self,
        _request: Request<ClaimCommandRequestV1>,
    ) -> Result<Response<ClaimCommandResponseV1>, Status> {
        self.0.calls.lock().expect("calls").push("claim");
        Ok(Response::new(self.0.claim.clone()))
    }

    async fn begin_execution(
        &self,
        _request: Request<BeginExecutionRequestV1>,
    ) -> Result<Response<BeginExecutionResponseV1>, Status> {
        self.0.calls.lock().expect("calls").push("begin");
        Ok(Response::new(self.0.begin.lock().expect("begin").clone()))
    }

    async fn authorize_invocation(
        &self,
        _request: Request<AuthorizeInvocationRequestV1>,
    ) -> Result<Response<AuthorizeInvocationResponseV1>, Status> {
        self.0.calls.lock().expect("calls").push("authorize");
        if self.0.authorize_unavailable.load(Ordering::SeqCst) {
            return Err(Status::unavailable("not exposed"));
        }
        Ok(Response::new(
            self.0.authorize.lock().expect("authorize").clone(),
        ))
    }

    async fn renew_lease(
        &self,
        _request: Request<RenewLeaseRequestV1>,
    ) -> Result<Response<RenewLeaseResponseV1>, Status> {
        self.0.calls.lock().expect("calls").push("renew");
        Ok(Response::new(self.0.renew.lock().expect("renew").clone()))
    }

    async fn observe_desired_state(
        &self,
        _request: Request<ObserveDesiredStateRequestV1>,
    ) -> Result<Response<ObserveDesiredStateResponseV1>, Status> {
        self.0.calls.lock().expect("calls").push("observe");
        let response = self
            .0
            .observe_queue
            .lock()
            .expect("observe queue")
            .pop_front()
            .unwrap_or_else(|| self.0.observe.lock().expect("observe").clone());
        Ok(Response::new(response))
    }

    async fn prepare_settlement(
        &self,
        _request: Request<PrepareSettlementRequestV1>,
    ) -> Result<Response<PrepareSettlementResponseV1>, Status> {
        panic!("preparation must stop before settlement")
    }
}

fn control() -> (
    Arc<AgentControlClient<TestControlRpc>>,
    Arc<TestControlState>,
) {
    let state = Arc::new(TestControlState::default());
    let control = AgentControlClient::new(
        TestControlRpc(Arc::clone(&state)),
        ControlGrpcConfig {
            deadline: Duration::from_secs(1),
            workload_session_id: "workload-1".to_owned(),
            producer_id: "worker-1".to_owned(),
        },
    )
    .expect("control client");
    (Arc::new(control), state)
}

async fn fresh(
    control: &AgentControlClient<TestControlRpc>,
    state: &TestControlState,
    kind: AgentExecutionKind,
) -> super::agent_delivery::FreshAgentDelivery {
    let fixture = match kind {
        AgentExecutionKind::Application => "signed_command",
        AgentExecutionKind::Adhoc => "signed_command_adhoc",
    };
    let raw = bytes(fixture);
    let verified = parse_and_verify_agent_command(
        &raw,
        Some(&TestOnlyConformanceHmacAuthenticator as &dyn SignedCommandAuthenticator),
    )
    .expect("verified command");
    let claim = control
        .claim_agent(&verified, NOW)
        .await
        .expect("accepted claim");
    let delivery = RedisCommandDelivery::decode(
        b"runtime.commands.v1",
        b"1700000000000-0",
        vec![(b"signed_envelope".to_vec(), raw)],
        RedisCommandLimits {
            max_entry_bytes: 64 * 1024,
            max_field_bytes: 48 * 1024,
        },
    )
    .expect("Redis delivery");
    state.calls.lock().expect("calls").clear();
    test_fresh_agent_delivery(delivery, verified, claim)
}

fn admission(capacity: usize, wait_timeout: Duration) -> InvocationAdmission {
    InvocationAdmission::new(
        InvocationAdmissionConfig::new(capacity, wait_timeout)
            .expect("valid admission configuration"),
    )
}

async fn empty_output(
    fresh: super::agent_delivery::FreshAgentDelivery,
) -> (tempfile::TempDir, EmptyAgentOutput) {
    let temporary_base = std::env::temp_dir()
        .canonicalize()
        .expect("canonical temporary root");
    let temporary = tempfile::tempdir_in(temporary_base).expect("temporary output root");
    fs::set_permissions(temporary.path(), fs::Permissions::from_mode(0o700))
        .expect("private output root");
    let preflight = AgentOutputPreflight::new(
        temporary.path().to_path_buf(),
        SpoolMasterKey::new([b'p'; 32]),
        SpoolLimits {
            max_frames: 1,
            max_encrypted_bytes: 2_048,
            max_frame_bytes: 1_024,
        },
        OutputGrpcConfig {
            max_queued_frames: 1,
            max_queued_bytes: 1_024,
            max_frame_bytes: 1_024,
            max_server_credit_frames: 1,
            max_server_credit_bytes: 1_024,
            stream_deadline: Duration::from_mins(1),
            ack_timeout: Duration::from_secs(1),
            workload_session_id: "workload-1".to_owned(),
            producer_id: "worker-1".to_owned(),
        },
    );
    let outcome = preflight.prepare(fresh).await.expect("output preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = outcome else {
        panic!("fresh preparation fixture must have an empty output spool");
    };
    (temporary, *empty)
}

struct TestClock(AtomicI64);

impl TestClock {
    fn new(now: i64) -> Self {
        Self(AtomicI64::new(now))
    }
}

impl super::agent_lease::UnixMillisClock for TestClock {
    fn now_unix_millis(&self) -> i64 {
        self.0.load(Ordering::SeqCst)
    }
}

struct SteppingClock {
    samples: Mutex<VecDeque<i64>>,
    fallback: i64,
}

impl SteppingClock {
    fn new(samples: impl IntoIterator<Item = i64>, fallback: i64) -> Self {
        Self {
            samples: Mutex::new(samples.into_iter().collect()),
            fallback,
        }
    }
}

impl super::agent_lease::UnixMillisClock for SteppingClock {
    fn now_unix_millis(&self) -> i64 {
        self.samples
            .lock()
            .expect("clock samples")
            .pop_front()
            .unwrap_or(self.fallback)
    }
}

enum InputMode {
    Bytes(Vec<u8>),
    DependencyUnavailable,
    Pending,
}

struct TestInput {
    state: Arc<TestControlState>,
    mode: InputMode,
    observed_after_call: Option<DesiredExecutionStateV1>,
    dropped: Arc<AtomicBool>,
}

impl TestInput {
    fn bytes(state: Arc<TestControlState>, value: Vec<u8>) -> Self {
        Self {
            state,
            mode: InputMode::Bytes(value),
            observed_after_call: None,
            dropped: Arc::new(AtomicBool::new(false)),
        }
    }
}

struct InputFutureGuard(Arc<AtomicBool>);

impl Drop for InputFutureGuard {
    fn drop(&mut self) {
        self.0.store(true, Ordering::SeqCst);
    }
}

#[async_trait]
impl AgentInputMaterializer for TestInput {
    async fn materialize(
        &self,
        _execution: &LeaseMonitoredAgentExecution,
    ) -> Result<MaterializedInput, InputContentError> {
        self.state.calls.lock().expect("calls").push("input");
        if let Some(desired) = self.observed_after_call {
            self.state.observe.lock().expect("observe").desired_state = desired as i32;
        }
        let _guard = InputFutureGuard(Arc::clone(&self.dropped));
        match &self.mode {
            InputMode::Bytes(value) => Ok(MaterializedInput::for_test(value.clone())),
            InputMode::DependencyUnavailable => Err(InputContentError::DependencyUnavailable(
                "the input content service is unavailable",
            )),
            InputMode::Pending => pending().await,
        }
    }
}

fn config(interval: Duration) -> AgentPreparationConfig {
    AgentPreparationConfig::new(interval).expect("valid preparation configuration")
}

async fn prepare_test(
    fresh: super::agent_delivery::FreshAgentDelivery,
    control: Arc<AgentControlClient<TestControlRpc>>,
    admission: &InvocationAdmission,
    input: &TestInput,
    clock: Arc<TestClock>,
    config: AgentPreparationConfig,
) -> Result<AgentPreparationOutcome, super::agent_preparation::AgentPreparationError> {
    let (_temporary, fresh) = empty_output(fresh).await;
    Box::pin(prepare_fresh_agent_invocation_with(
        fresh, control, admission, input, clock, config,
    ))
    .await
}

async fn prepared_for_authorization(
    control: Arc<AgentControlClient<TestControlRpc>>,
    state: Arc<TestControlState>,
    admission: &InvocationAdmission,
    kind: AgentExecutionKind,
) -> Box<super::agent_preparation::PreparedAgentInvocation> {
    let fresh = fresh(control.as_ref(), &state, kind).await;
    let input = TestInput::bytes(Arc::clone(&state), input_fixture(kind));
    let outcome = prepare_test(
        fresh,
        control,
        admission,
        &input,
        Arc::new(TestClock::new(NOW)),
        config(Duration::from_secs(10)),
    )
    .await
    .expect("prepared invocation");
    let AgentPreparationOutcome::Prepared(prepared) = outcome else {
        panic!("valid input must reach the authorization boundary")
    };
    prepared
}

#[tokio::test(flavor = "current_thread")]
async fn application_and_adhoc_share_one_ordered_preparation_path() {
    for kind in [AgentExecutionKind::Application, AgentExecutionKind::Adhoc] {
        let (control, state) = control();
        let fresh = fresh(control.as_ref(), &state, kind).await;
        let admission = admission(1, Duration::from_secs(1));
        let input = TestInput::bytes(Arc::clone(&state), input_fixture(kind));

        let outcome = prepare_test(
            fresh,
            Arc::clone(&control),
            &admission,
            &input,
            Arc::new(TestClock::new(NOW)),
            config(Duration::from_secs(10)),
        )
        .await
        .expect("preparation");
        let AgentPreparationOutcome::Prepared(prepared) = outcome else {
            panic!("fresh input must be prepared")
        };
        assert_eq!(prepared.execution_kind(), kind);
        assert_eq!(prepared.request().binding.request_entry_id, "agent-request");
        assert_eq!(
            *state.calls.lock().expect("calls"),
            ["begin", "renew", "observe", "input"]
        );
        assert_eq!(admission.available_capacity(), 0);

        let (reservation, lease) = (*prepared).into_test_cleanup();
        lease.close().await.expect("lease close");
        drop(reservation);
        assert_eq!(admission.available_capacity(), 1);
    }
}

#[tokio::test(flavor = "current_thread")]
async fn authorization_carries_the_matching_prepared_request_to_submission() {
    for kind in [AgentExecutionKind::Application, AgentExecutionKind::Adhoc] {
        let (control, state) = control();
        let admission = admission(1, Duration::from_secs(1));
        let prepared =
            prepared_for_authorization(Arc::clone(&control), Arc::clone(&state), &admission, kind)
                .await;

        let (reservation, prepared) = (*prepared).into_supervised_authorization();
        let decision = control
            .authorize_agent_invocation(prepared.into_candidate())
            .await;
        let InvocationAuthorizationDecision::AuthorizedNow(authorized) = decision else {
            panic!("the fixture must authorize exactly once")
        };
        let (request, lease) = authorized.into_test_cleanup();
        let observed_kind = request.kind;
        let request_kind = request.kind;
        assert_eq!(observed_kind, kind);
        assert_eq!(request_kind, kind);
        assert_eq!(
            *state.calls.lock().expect("calls"),
            ["begin", "renew", "observe", "input", "authorize"]
        );
        lease.close().await.expect("lease close");
        drop(reservation);
        assert_eq!(admission.available_capacity(), 1);
    }
}

#[tokio::test(flavor = "current_thread")]
async fn invalid_progress_policy_returns_the_complete_unstarted_run_for_cleanup() {
    for kind in [AgentExecutionKind::Application, AgentExecutionKind::Adhoc] {
        let (control, state) = control();
        let admission = admission(1, Duration::from_secs(1));
        let prepared =
            prepared_for_authorization(Arc::clone(&control), state, &admission, kind).await;
        let (reservation, prepared) = (*prepared).into_supervised_authorization();
        let InvocationAuthorizationDecision::AuthorizedNow(authorized) = control
            .authorize_agent_invocation(prepared.into_candidate())
            .await
        else {
            panic!("the fixture must authorize exactly once")
        };
        let connector = Channel::from_static("https://output.invalid").connect_lazy();
        let Err(failure) = authorized.bind_progress_publisher(connector, 0) else {
            panic!("an invalid progress policy must not consume the authorized run")
        };
        assert_eq!(
            failure.error().code(),
            "agent_progress.invalid_configuration"
        );
        let (authorized, _connector, error) = (*failure).into_parts();
        assert_eq!(error.code(), "agent_progress.invalid_configuration");
        assert_eq!(authorized.execution_kind(), kind);
        let (_request, lease) = authorized.into_test_cleanup();
        lease.close().await.expect("lease close");
        drop(reservation);
        assert_eq!(admission.available_capacity(), 1);
    }
}

#[tokio::test(flavor = "current_thread")]
async fn every_non_authorized_outcome_is_closed_without_submission() {
    #[derive(Clone, Copy)]
    enum Case {
        Already,
        Rejected,
        Unknown,
    }

    for case in [Case::Already, Case::Rejected, Case::Unknown] {
        let (control, state) = control();
        match case {
            Case::Already => {
                *state.authorize.lock().expect("authorize") = AuthorizeInvocationResponseV1 {
                    disposition: AuthorizeInvocationDispositionV1::AlreadyAuthorized as i32,
                    rejection: None,
                };
            }
            Case::Rejected => {
                *state.authorize.lock().expect("authorize") = AuthorizeInvocationResponseV1 {
                    disposition: AuthorizeInvocationDispositionV1::Unspecified as i32,
                    rejection: Some(RuntimeErrorV1 {
                        code: RuntimeErrorCodeV1::DependencyUnavailable as i32,
                        safe_message: "server detail must not escape".to_owned(),
                        retryable: true,
                    }),
                };
            }
            Case::Unknown => state.authorize_unavailable.store(true, Ordering::SeqCst),
        }
        let admission = admission(1, Duration::from_secs(1));
        let prepared = prepared_for_authorization(
            Arc::clone(&control),
            Arc::clone(&state),
            &admission,
            AgentExecutionKind::Application,
        )
        .await;
        let (reservation, prepared) = (*prepared).into_supervised_authorization();
        let decision = control
            .authorize_agent_invocation(prepared.into_candidate())
            .await;

        let (observed_kind, failure, lease, error_retryable) = match decision {
            InvocationAuthorizationDecision::AlreadyAuthorized(terminal)
            | InvocationAuthorizationDecision::Rejected(terminal) => {
                let (kind, failure, lease) = terminal.into_test_cleanup();
                (kind, Some(failure), Some(lease), None)
            }
            InvocationAuthorizationDecision::Unknown(unknown) => {
                assert!(matches!(unknown.error(), AgentControlError::Transport(_)));
                assert!(!unknown.error().to_string().contains("not exposed"));
                let completion = unknown.close_no_ack().await;
                assert!(completion.lease_error().is_none());
                (
                    completion.execution_kind(),
                    None,
                    None,
                    Some(completion.authorization_error().retryable()),
                )
            }
            InvocationAuthorizationDecision::AuthorizedNow(_) => {
                panic!("the configured response must not grant submission")
            }
        };
        assert_eq!(observed_kind, AgentExecutionKind::Application);
        match case {
            Case::Already => assert_eq!(failure, Some(RuntimeFailureKind::Internal)),
            Case::Rejected => {
                assert_eq!(failure, Some(RuntimeFailureKind::DependencyUnavailable));
            }
            Case::Unknown => assert_eq!(failure, None),
        }
        if let Some(retryable) = error_retryable {
            assert!(retryable);
        }
        if let Some(lease) = lease {
            lease.close().await.expect("lease close");
        }
        drop(reservation);
        assert_eq!(admission.available_capacity(), 1);
    }
}

#[tokio::test(flavor = "current_thread")]
async fn mixed_authorization_response_carries_payload_to_terminal_only() {
    let (control, state) = control();
    *state.authorize.lock().expect("authorize") = AuthorizeInvocationResponseV1 {
        disposition: AuthorizeInvocationDispositionV1::AuthorizedNow as i32,
        rejection: Some(RuntimeErrorV1 {
            code: RuntimeErrorCodeV1::Internal as i32,
            safe_message: String::new(),
            retryable: false,
        }),
    };
    let admission = admission(1, Duration::from_secs(1));
    let prepared = prepared_for_authorization(
        Arc::clone(&control),
        state,
        &admission,
        AgentExecutionKind::Adhoc,
    )
    .await;
    let (reservation, prepared) = (*prepared).into_supervised_authorization();
    let InvocationAuthorizationDecision::Rejected(terminal) = control
        .authorize_agent_invocation(prepared.into_candidate())
        .await
    else {
        panic!("a mixed response must not mint submission authority")
    };
    let (kind, failure, lease) = terminal.into_test_cleanup();
    assert_eq!(kind, AgentExecutionKind::Adhoc);
    assert_eq!(failure, RuntimeFailureKind::InvalidInput);
    lease.close().await.expect("lease close");
    drop(reservation);
}

#[tokio::test(flavor = "current_thread", start_paused = true)]
async fn admission_saturation_returns_retry_without_begin_or_input() {
    let (control, state) = control();
    let fresh = fresh(control.as_ref(), &state, AgentExecutionKind::Application).await;
    let admission = admission(1, Duration::from_millis(50));
    let held = admission.reserve().await.expect("held reservation");
    let input = TestInput::bytes(
        Arc::clone(&state),
        input_fixture(AgentExecutionKind::Application),
    );

    let outcome = prepare_test(
        fresh,
        control,
        &admission,
        &input,
        Arc::new(TestClock::new(NOW)),
        config(Duration::from_secs(10)),
    )
    .await
    .expect("retry disposition");
    assert!(matches!(outcome, AgentPreparationOutcome::RetryLaterNoAck));
    assert!(state.calls.lock().expect("calls").is_empty());
    assert_eq!(admission.available_capacity(), 0);
    drop(held);
    assert_eq!(admission.available_capacity(), 1);
}

#[tokio::test(flavor = "current_thread")]
async fn begin_replay_requires_recovery_and_releases_unused_capacity() {
    let (control, state) = control();
    state.begin.lock().expect("begin").disposition =
        BeginExecutionDispositionV1::AlreadyStarted as i32;
    let fresh = fresh(control.as_ref(), &state, AgentExecutionKind::Application).await;
    let admission = admission(1, Duration::from_secs(1));
    let input = TestInput::bytes(
        Arc::clone(&state),
        input_fixture(AgentExecutionKind::Application),
    );

    let outcome = prepare_test(
        fresh,
        control,
        &admission,
        &input,
        Arc::new(TestClock::new(NOW)),
        config(Duration::from_secs(10)),
    )
    .await
    .expect("recovery disposition");
    assert!(matches!(
        outcome,
        AgentPreparationOutcome::RecoveryRequiredNoAck
    ));
    assert_eq!(*state.calls.lock().expect("calls"), ["begin"]);
    assert_eq!(admission.available_capacity(), 1);
}

#[tokio::test(flavor = "current_thread")]
async fn immediate_stop_is_a_typed_terminal_and_never_fetches_input() {
    let (control, state) = control();
    state.observe.lock().expect("observe").desired_state =
        DesiredExecutionStateV1::Cancelled as i32;
    let fresh = fresh(control.as_ref(), &state, AgentExecutionKind::Application).await;
    let admission = admission(1, Duration::from_secs(1));
    let input = TestInput::bytes(
        Arc::clone(&state),
        input_fixture(AgentExecutionKind::Application),
    );

    let outcome = prepare_test(
        fresh,
        control,
        &admission,
        &input,
        Arc::new(TestClock::new(NOW)),
        config(Duration::from_secs(10)),
    )
    .await
    .expect("terminal Stop");
    let AgentPreparationOutcome::PreInvocationTerminal(terminal) = outcome else {
        panic!("Stop must retain terminal output authority")
    };
    assert_eq!(
        terminal.cause().runtime_failure_kind(),
        RuntimeFailureKind::Cancelled
    );
    assert_eq!(terminal.cause().code(), "agent_preparation.cancelled");
    assert_eq!(
        *state.calls.lock().expect("calls"),
        ["begin", "renew", "observe"]
    );
    assert_eq!(admission.available_capacity(), 0);

    let (_, verified, output, _spool, reservation, lease, _) = (*terminal).into_test_parts();
    let frame = output
        .bind_failure_terminal(&verified, RuntimeFailureKind::Cancelled, NOW)
        .expect("claim-bound cancellation terminal");
    assert_eq!(frame.claim_handoff_watermark, 4);
    assert_eq!(frame.sequence, 5);
    assert_eq!(frame.fence.expect("bound fence").fence_token.len(), 32);
    lease.close().await.expect("cancelled lease close");
    drop(reservation);
    assert_eq!(admission.available_capacity(), 1);
}

#[tokio::test(flavor = "current_thread")]
async fn malformed_input_is_terminal_only_after_a_final_live_lease_poll() {
    let (control, state) = control();
    let fresh = fresh(control.as_ref(), &state, AgentExecutionKind::Application).await;
    let admission = admission(1, Duration::from_secs(1));
    let input = TestInput::bytes(Arc::clone(&state), vec![0xff]);

    let outcome = prepare_test(
        fresh,
        control,
        &admission,
        &input,
        Arc::new(TestClock::new(NOW)),
        config(Duration::from_secs(10)),
    )
    .await
    .expect("input terminal");
    let AgentPreparationOutcome::PreInvocationTerminal(terminal) = outcome else {
        panic!("malformed input must retain terminal output authority")
    };
    assert_eq!(
        terminal.cause().runtime_failure_kind(),
        RuntimeFailureKind::InvalidInput
    );
    assert_eq!(terminal.cause().code(), "agent_input.invalid_input");
    assert_eq!(
        *state.calls.lock().expect("calls"),
        ["begin", "renew", "observe", "input", "renew", "observe"]
    );
    let (_, _, _, _spool, reservation, lease, cause) = (*terminal).into_test_parts();
    assert!(matches!(
        cause,
        PreInvocationTerminalCause::InputProtocol(_)
    ));
    lease.close().await.expect("lease close");
    drop(reservation);
}

#[tokio::test(flavor = "current_thread")]
async fn fatal_lease_loss_after_input_failure_suppresses_terminal_output() {
    let (control, state) = control();
    let fresh = fresh(control.as_ref(), &state, AgentExecutionKind::Application).await;
    let admission = admission(1, Duration::from_secs(1));
    let input = TestInput {
        state: Arc::clone(&state),
        mode: InputMode::DependencyUnavailable,
        observed_after_call: Some(DesiredExecutionStateV1::Draining),
        dropped: Arc::new(AtomicBool::new(false)),
    };

    let result = prepare_test(
        fresh,
        control,
        &admission,
        &input,
        Arc::new(TestClock::new(NOW)),
        config(Duration::from_secs(10)),
    )
    .await;
    let Err(error) = result else {
        panic!("fatal lease state must fence terminal output")
    };
    assert_eq!(error.code(), AgentPreparationErrorCode::Lease);
    assert!(error.retryable());
    assert_eq!(
        error.to_string(),
        "agent lease activation failed: the execution is draining and cannot continue on this worker"
    );
    assert_eq!(
        *state.calls.lock().expect("calls"),
        ["begin", "renew", "observe", "input", "renew", "observe"]
    );
    assert_eq!(admission.available_capacity(), 1);
}

#[tokio::test(flavor = "current_thread")]
async fn inclusive_deadline_is_terminal_after_lease_validation_and_before_input() {
    let (control, state) = control();
    state
        .renew
        .lock()
        .expect("renew")
        .lease_expires_at_unix_millis = DEADLINE + 60_000;
    let fresh = fresh(control.as_ref(), &state, AgentExecutionKind::Application).await;
    let admission = admission(1, Duration::from_secs(1));
    let input = TestInput::bytes(
        Arc::clone(&state),
        input_fixture(AgentExecutionKind::Application),
    );

    let outcome = prepare_test(
        fresh,
        control,
        &admission,
        &input,
        Arc::new(TestClock::new(DEADLINE)),
        config(Duration::from_secs(10)),
    )
    .await
    .expect("deadline terminal");
    let AgentPreparationOutcome::PreInvocationTerminal(terminal) = outcome else {
        panic!("inclusive deadline must retain terminal output authority")
    };
    assert_eq!(
        terminal.cause().runtime_failure_kind(),
        RuntimeFailureKind::DeadlineExceeded
    );
    assert_eq!(
        *state.calls.lock().expect("calls"),
        ["begin", "renew", "observe", "renew", "observe"]
    );
    let (_, _, _, _spool, reservation, lease, _) = (*terminal).into_test_parts();
    lease.close().await.expect("lease close");
    drop(reservation);
}

#[tokio::test(flavor = "current_thread")]
async fn deadline_crossing_during_request_validation_cannot_mint_prepared_state() {
    let (control, state) = control();
    state
        .renew
        .lock()
        .expect("renew")
        .lease_expires_at_unix_millis = DEADLINE + 60_000;
    let fresh = fresh(control.as_ref(), &state, AgentExecutionKind::Application).await;
    let admission = admission(1, Duration::from_secs(1));
    let input = TestInput::bytes(
        Arc::clone(&state),
        input_fixture(AgentExecutionKind::Application),
    );
    let clock = Arc::new(SteppingClock::new([NOW, NOW, DEADLINE, DEADLINE], DEADLINE));
    let (_temporary, fresh) = empty_output(fresh).await;

    let outcome = Box::pin(prepare_fresh_agent_invocation_with(
        fresh,
        control,
        &admission,
        &input,
        clock,
        config(Duration::from_secs(10)),
    ))
    .await
    .expect("deadline terminal");
    let AgentPreparationOutcome::PreInvocationTerminal(terminal) = outcome else {
        panic!("request validation must not return Prepared after the deadline")
    };
    assert_eq!(
        terminal.cause().runtime_failure_kind(),
        RuntimeFailureKind::DeadlineExceeded
    );
    assert_eq!(
        *state.calls.lock().expect("calls"),
        ["begin", "renew", "observe", "input", "renew", "observe"]
    );
    let (_, _, _, _spool, reservation, lease, _) = (*terminal).into_test_parts();
    lease.close().await.expect("lease close");
    drop(reservation);
}

#[tokio::test(flavor = "current_thread", start_paused = true)]
async fn periodic_stop_cancels_and_drops_an_inflight_input_future() {
    let (control, state) = control();
    state.observe_queue.lock().expect("observe queue").extend([
        ObserveDesiredStateResponseV1 {
            desired_state: DesiredExecutionStateV1::Running as i32,
            rejection: None,
        },
        ObserveDesiredStateResponseV1 {
            desired_state: DesiredExecutionStateV1::Cancelled as i32,
            rejection: None,
        },
    ]);
    let fresh = fresh(control.as_ref(), &state, AgentExecutionKind::Application).await;
    let admission = admission(1, Duration::from_secs(1));
    let dropped = Arc::new(AtomicBool::new(false));
    let input = TestInput {
        state: Arc::clone(&state),
        mode: InputMode::Pending,
        observed_after_call: None,
        dropped: Arc::clone(&dropped),
    };

    let outcome = tokio::time::timeout(
        Duration::from_millis(20),
        prepare_test(
            fresh,
            control,
            &admission,
            &input,
            Arc::new(TestClock::new(NOW)),
            config(Duration::from_millis(1)),
        ),
    )
    .await
    .expect("periodic Stop must win")
    .expect("terminal Stop");
    let AgentPreparationOutcome::PreInvocationTerminal(terminal) = outcome else {
        panic!("Stop during input must retain terminal output authority")
    };
    assert!(dropped.load(Ordering::SeqCst));
    assert_eq!(
        terminal.cause().runtime_failure_kind(),
        RuntimeFailureKind::Cancelled
    );
    assert_eq!(
        *state.calls.lock().expect("calls"),
        ["begin", "renew", "observe", "input", "renew", "observe"]
    );
    let (_, _, _, _spool, reservation, lease, _) = (*terminal).into_test_parts();
    lease.close().await.expect("cancelled lease close");
    drop(reservation);
}

#[tokio::test(flavor = "current_thread")]
async fn input_dependency_failure_preserves_retryable_terminal_semantics() {
    let (control, state) = control();
    let fresh = fresh(control.as_ref(), &state, AgentExecutionKind::Application).await;
    let admission = admission(1, Duration::from_secs(1));
    let input = TestInput {
        state,
        mode: InputMode::DependencyUnavailable,
        observed_after_call: None,
        dropped: Arc::new(AtomicBool::new(false)),
    };

    let outcome = prepare_test(
        fresh,
        control,
        &admission,
        &input,
        Arc::new(TestClock::new(NOW)),
        config(Duration::from_secs(10)),
    )
    .await
    .expect("dependency terminal");
    let AgentPreparationOutcome::PreInvocationTerminal(terminal) = outcome else {
        panic!("dependency failure must retain terminal output authority")
    };
    assert_eq!(
        terminal.cause().runtime_failure_kind(),
        RuntimeFailureKind::DependencyUnavailable
    );
    assert_eq!(
        terminal.cause().code(),
        "input_content.dependency_unavailable"
    );
    let (_, _, _, _spool, reservation, lease, _) = (*terminal).into_test_parts();
    lease.close().await.expect("lease close");
    drop(reservation);
}

#[test]
fn pre_invocation_error_messages_and_codes_are_operator_safe() {
    std::hint::black_box(prepare_fresh_agent_invocation::<TestControlRpc>);
    let invalid = PreInvocationTerminalCause::InputContent(InputContentError::InvalidInput(
        "the materialized input failed its content binding",
    ));
    assert_eq!(invalid.code(), "input_content.invalid_response");
    assert_eq!(
        invalid.runtime_failure_kind(),
        RuntimeFailureKind::InvalidInput
    );
    assert_eq!(
        invalid.to_string(),
        "agent input materialization failed: the materialized input failed its content binding"
    );
    let deadline = PreInvocationTerminalCause::DeadlineExceeded;
    assert_eq!(deadline.code(), "agent_preparation.deadline_exceeded");
    assert_eq!(
        deadline.to_string(),
        "the agent command deadline was exceeded before invocation"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn toolkit_invocation_authority_preserves_all_control_outcomes() {
    use super::agent_lease::{ClaimLeaseActivation, ClaimLeaseMonitor};
    use crate::protocol::control::{BeginAgentExecution, ToolkitInvocationPayload};

    for case in ["authorized", "already", "rejected", "unknown"] {
        let (control, state) = control();
        match case {
            "already" => {
                state.authorize.lock().expect("authorize").disposition =
                    AuthorizeInvocationDispositionV1::AlreadyAuthorized as i32;
            }
            "rejected" => {
                *state.authorize.lock().expect("authorize") = AuthorizeInvocationResponseV1 {
                    disposition: AuthorizeInvocationDispositionV1::Unspecified as i32,
                    rejection: Some(RuntimeErrorV1 {
                        code: RuntimeErrorCodeV1::Cancelled as i32,
                        safe_message: "Execution was cancelled.".to_owned(),
                        retryable: false,
                    }),
                }
            }
            "unknown" => state.authorize_unavailable.store(true, Ordering::SeqCst),
            _ => {}
        }
        // Reuse the signed claim fixture: this tests the shared control contract,
        // not toolkit payload decoding or a provider implementation.
        let verified = parse_and_verify_agent_command(
            &bytes("signed_command"),
            Some(&TestOnlyConformanceHmacAuthenticator as &dyn SignedCommandAuthenticator),
        )
        .expect("verified fixture");
        let claim = control.claim_agent(&verified, NOW).await.expect("claim");
        let BeginAgentExecution::Preparing(preparing) =
            control.begin_agent_execution(claim).await.expect("begin")
        else {
            panic!("fresh claim must prepare")
        };
        let mut lease = ClaimLeaseMonitor::start(
            Arc::clone(&control),
            preparing.start_lease_monitor(),
            Arc::new(TestClock::new(NOW)),
            config(Duration::from_secs(10)).lease_config(),
        );
        let ClaimLeaseActivation::Active(execution) = lease.activate().await else {
            panic!("fixture lease must activate")
        };
        let decision = control
            .authorize_agent_invocation(execution.bind_invocation(ToolkitInvocationPayload))
            .await;
        match decision {
            InvocationAuthorizationDecision::AuthorizedNow(authorized) => {
                assert_eq!(case, "authorized");
                assert_eq!(
                    authorized.execution().request_entry().entry_id,
                    "agent-request"
                );
                drop(authorized.into_output_authority());
            }
            InvocationAuthorizationDecision::AlreadyAuthorized(terminal) => {
                assert_eq!(case, "already");
                assert_eq!(
                    terminal.cause.runtime_failure_kind(),
                    RuntimeFailureKind::Internal
                );
            }
            InvocationAuthorizationDecision::Rejected(terminal) => {
                assert_eq!(case, "rejected");
                assert_eq!(
                    terminal.cause.runtime_failure_kind(),
                    RuntimeFailureKind::Cancelled
                );
            }
            InvocationAuthorizationDecision::Unknown(_) => assert_eq!(case, "unknown"),
        }
        lease.close().await.expect("lease closed");
    }
}
