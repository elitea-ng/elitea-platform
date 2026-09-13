use std::collections::{BTreeMap, HashMap, VecDeque};
use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::PathBuf;
use std::sync::atomic::{AtomicI64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::futures::stream;
use adk_rust::runner::Runner;
use adk_rust::session::{CreateRequest, InMemorySessionService, SessionService};
use adk_rust::{
    Agent, Content, Event, EventStream, FinishReason, InvocationContext, Part, SessionId,
    ToolConfirmationRequest, UserId,
};
use async_trait::async_trait;
use chrono::{TimeZone, Utc};
use prost::Message;
use ring::digest;
use serde_json::json;
use tokio::sync::{Notify, Semaphore, oneshot};
use tonic::{Request, Response, Status};

use super::agent_coordinator::native_agent_coordinator;
use super::agent_delivery::{FreshAgentDelivery, test_fresh_agent_delivery};
use super::agent_delivery_processor::native_agent_delivery_processor;
use super::agent_invocation::{
    AgentAuthorizationJob, AgentAuthorizationJobCompletion, AgentAuthorizedLifecycleCompletion,
    AgentAuthorizedLifecycleDisposition, AuthorizedAgentLifecycle,
};
use super::agent_lease::ClaimLeaseMonitorConfig;
use super::agent_preparation::{
    AgentInputMaterializer, AgentPreparationConfig, AgentPreparationOutcome, PreInvocationTerminal,
    prepare_fresh_agent_invocation_with,
};
use super::invocation_admission::{InvocationAdmission, InvocationAdmissionConfig};
use super::invocation_supervisor::InvocationSupervisor;
use super::native_agent_lifecycle::{
    AgentPauseAccumulator, AgentPauseAggregationError, NativeAuthorizedAgentLifecycle,
};
use super::output_delivery::{
    AcceptedTerminalOutputRecovery, AgentOutputPreflight, AgentOutputPreflightError,
    AgentOutputPreflightKind, AgentOutputPreflightOutcome, AgentOutputRecoveryRequiredKind,
    AgentProgressConnector, AgentProgressPublishOutcome, AgentProgressPublisherConfig,
    AgentProgressReplaySession, AgentProgressSession, AgentTerminalRecoveryConfig,
    AgentTerminalRecoveryError, AgentTerminalReplay, FreshAgentProgressPublisher,
    publish_pre_invocation_terminal, recover_accepted_terminal,
};
use super::redis_delivery::RedisDeliveryProcessor;
use crate::agents::events::{
    AgentEventProjectionContext, AgentEventProjector, CompletedAgentBrowserOutput,
};
use crate::agents::runtime::{
    AssembledNativeAgentInvocation, AuthorizedNativeAssembly, NativeAgentAssembler,
    NativeAgentAssemblyError, NativeAgentAssemblyErrorCode, NativeAgentCompletionSelector,
    NativeAgentInvocation,
};
use crate::agents::sensitive_tools::SensitiveToolCatalog;
use crate::agents::{AgentExecutionKind, AgentTerminalState};
use crate::protocol::command::{
    SignedCommandAuthenticator, TestOnlyConformanceHmacAuthenticator,
    parse_and_verify_agent_command,
};
use crate::protocol::control::{
    AgentControlClient, test_accepted_agent_claim, test_agent_output_authority,
    test_agent_output_authority_with_handoff, test_lease_monitored_input_execution,
};
use crate::protocol::elitea::runtime::v1::{
    AgentExecutionTerminalStateV1, AuthorizeInvocationDispositionV1, AuthorizeInvocationRequestV1,
    AuthorizeInvocationResponseV1, BeginExecutionDispositionV1, BeginExecutionRequestV1,
    BeginExecutionResponseV1, ClaimCommandRequestV1, ClaimCommandResponseV1,
    DesiredExecutionStateV1, DigestAlgorithmV1, DigestV1, ExecutionFenceV1, ExecutionIdentityV1,
    ExecutionOutputEventTypeV1, ExecutionOutputFrameV1, ObserveDesiredStateRequestV1,
    ObserveDesiredStateResponseV1, PrepareSettlementRequestV1, PrepareSettlementResponseV1,
    RenewLeaseRequestV1, RenewLeaseResponseV1, RuntimeErrorCodeV1, execution_output_frame_v1,
};
use crate::protocol::node_event::decode_current_node_event_json;
use crate::protocol::output::{
    OUTPUT_SCHEMA_REVISION, RuntimeFailureKind, restored_terminal_failure_kind,
};
use crate::spool::{SpoolError, SpoolLimits, SpoolMasterKey};
use crate::toolkits::ToolAdmissionPolicy;
use crate::transport::output_grpc::{
    ProgressRejectionWinner, ProgressReplayDecision, test_acknowledged_progress,
    test_acknowledged_terminal, test_rejected_progress,
};
use crate::transport::redis_commands::{
    RedisCommandDelivery, RedisCommandLimits, RedisCommandRetirer, RedisRetirementClient,
    RedisRetirementClientError, RedisRetirementConfig, RedisRetirementRequest,
    RedisRetirementResponse,
};
use crate::transport::{
    ControlGrpcConfig, ControlRpc, DurablyAckedTerminal, OutputGrpcConfig, OutputGrpcError,
    OutputProtocolError, PreparedOutputSpool,
};
use crate::transport::{InputContentError, MaterializedInput};

const NOW: i64 = 1_700_000_000_000;

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

fn output_bytes(name: &str) -> Vec<u8> {
    let output_vectors: BTreeMap<_, _> =
        include_str!("../../tests/fixtures/agent_output_vectors.txt")
            .lines()
            .map(|line| line.split_once('=').expect("named output fixture"))
            .collect();
    decode_hex(output_vectors[name])
}

fn claim_response() -> ClaimCommandResponseV1 {
    ClaimCommandResponseV1::decode(bytes("accepted_claim").as_slice()).expect("claim fixture")
}

fn fresh() -> FreshAgentDelivery {
    fresh_for_kind(AgentExecutionKind::Application)
}

fn fresh_for_kind(kind: AgentExecutionKind) -> FreshAgentDelivery {
    let raw = signed_command_for_kind(kind);
    let verified = parse_and_verify_agent_command(
        &raw,
        Some(&TestOnlyConformanceHmacAuthenticator as &dyn SignedCommandAuthenticator),
    )
    .expect("verified command");
    let claim =
        test_accepted_agent_claim(&verified, claim_response(), "workload-1", "worker-1", NOW)
            .expect("accepted claim");
    test_fresh_agent_delivery(redis_delivery(raw), verified, claim)
}

fn signed_command_for_kind(kind: AgentExecutionKind) -> Vec<u8> {
    bytes(match kind {
        AgentExecutionKind::Application => "signed_command",
        AgentExecutionKind::Adhoc => "signed_command_adhoc",
    })
}

fn redis_delivery(raw: Vec<u8>) -> RedisCommandDelivery {
    RedisCommandDelivery::decode(
        b"runtime.commands.v1",
        b"1700000000000-0",
        vec![(b"signed_envelope".to_vec(), raw)],
        RedisCommandLimits {
            max_entry_bytes: 64 * 1024,
            max_field_bytes: 48 * 1024,
        },
    )
    .expect("Redis delivery")
}

fn output_config(producer_id: &str) -> OutputGrpcConfig {
    OutputGrpcConfig {
        max_queued_frames: 1,
        max_queued_bytes: 64 * 1024,
        max_frame_bytes: 64 * 1024,
        max_server_credit_frames: 1,
        max_server_credit_bytes: 64 * 1024,
        stream_deadline: Duration::from_mins(1),
        ack_timeout: Duration::from_secs(1),
        workload_session_id: "workload-1".to_owned(),
        producer_id: producer_id.to_owned(),
    }
}

fn spool_limits() -> SpoolLimits {
    SpoolLimits {
        max_frames: 1,
        max_encrypted_bytes: 65 * 1024,
        max_frame_bytes: 64 * 1024,
    }
}

fn root() -> (tempfile::TempDir, PathBuf) {
    let base = std::env::temp_dir()
        .canonicalize()
        .expect("canonical temporary root");
    let temporary = tempfile::tempdir_in(base).expect("temporary output root");
    fs::set_permissions(temporary.path(), fs::Permissions::from_mode(0o700))
        .expect("private output root");
    let root = temporary.path().to_path_buf();
    (temporary, root)
}

fn preflight(root: PathBuf, producer_id: &str) -> AgentOutputPreflight {
    AgentOutputPreflight::new(
        root,
        SpoolMasterKey::new([b'p'; 32]),
        spool_limits(),
        output_config(producer_id),
    )
}

#[derive(Clone, Copy)]
enum ReplayResult {
    Acknowledged,
    AdvanceAndAcknowledged,
    Unavailable,
    DependencyUnavailable,
    AuthorizationFailed,
    CancellationWon,
    DeadlineWon,
}

struct FakeReplay {
    results: Mutex<VecDeque<ReplayResult>>,
    frames: Mutex<Vec<ExecutionOutputFrameV1>>,
    trace: Arc<Mutex<Vec<&'static str>>>,
}

impl FakeReplay {
    fn new(
        results: impl IntoIterator<Item = ReplayResult>,
        trace: Arc<Mutex<Vec<&'static str>>>,
    ) -> Self {
        Self {
            results: Mutex::new(results.into_iter().collect()),
            frames: Mutex::new(Vec::new()),
            trace,
        }
    }
}

#[async_trait]
impl AgentTerminalReplay for FakeReplay {
    async fn replay_terminal(
        &self,
        mut spool: PreparedOutputSpool,
        verified: &crate::protocol::command::VerifiedAgentCommand,
        expected: &ExecutionOutputFrameV1,
    ) -> Result<DurablyAckedTerminal, OutputGrpcError> {
        assert_eq!(spool.pending_frame_count(), 1);
        assert!(spool.replays(expected));
        self.trace.lock().expect("trace").push("replay");
        self.frames.lock().expect("frames").push(expected.clone());
        let result = self
            .results
            .lock()
            .expect("replay results")
            .pop_front()
            .expect("scripted replay result");
        match result {
            ReplayResult::Acknowledged => {
                spool.reconcile_pending_through(expected.sequence)?;
                test_acknowledged_terminal(verified, expected)
            }
            ReplayResult::AdvanceAndAcknowledged => {
                tokio::task::yield_now().await;
                tokio::time::advance(Duration::from_millis(1)).await;
                tokio::task::yield_now().await;
                spool.reconcile_pending_through(expected.sequence)?;
                test_acknowledged_terminal(verified, expected)
            }
            ReplayResult::Unavailable => Err(OutputGrpcError::Unavailable(
                "the test output endpoint is unavailable",
            )),
            ReplayResult::DependencyUnavailable => Err(OutputGrpcError::Protocol(
                OutputProtocolError::DependencyUnavailable,
            )),
            ReplayResult::AuthorizationFailed => Err(OutputGrpcError::Protocol(
                OutputProtocolError::AuthorizationFailed("the test output was rejected"),
            )),
            ReplayResult::CancellationWon => Err(OutputGrpcError::Protocol(
                OutputProtocolError::CancellationWon,
            )),
            ReplayResult::DeadlineWon => {
                Err(OutputGrpcError::Protocol(OutputProtocolError::DeadlineWon))
            }
        }
    }
}

struct RecoveryControl {
    trace: Arc<Mutex<Vec<&'static str>>>,
    claim_fixture: &'static str,
    settlement_fails: bool,
    authorize_disposition: Option<AuthorizeInvocationDispositionV1>,
    authorize_unavailable: bool,
    renew_fail_after: Option<usize>,
    renew_attempts: AtomicUsize,
    observe_states: Mutex<VecDeque<i32>>,
}

#[async_trait]
impl ControlRpc for RecoveryControl {
    async fn claim_command(
        &self,
        request: Request<ClaimCommandRequestV1>,
    ) -> Result<Response<ClaimCommandResponseV1>, Status> {
        self.trace.lock().expect("trace").push("claim");
        if self.claim_fixture == "checkpoint" {
            assert!(request.get_ref().agent_model_checkpoint_recovery);
            let mut response = claim_response();
            response.receipt.as_mut().expect("receipt").disposition =
                crate::protocol::elitea::runtime::v1::ClaimDispositionV1::RecoverAgentModelCheckpoint as i32;
            return Ok(Response::new(response));
        }
        Ok(Response::new(
            ClaimCommandResponseV1::decode(bytes(self.claim_fixture).as_slice())
                .expect("claim fixture"),
        ))
    }

    async fn authorize_agent_model_checkpoint(
        &self,
        request: Request<
            crate::protocol::elitea::runtime::v1::AuthorizeAgentModelCheckpointRequestV1,
        >,
    ) -> Result<
        Response<crate::protocol::elitea::runtime::v1::AuthorizeAgentModelCheckpointResponseV1>,
        Status,
    > {
        self.trace
            .lock()
            .expect("trace")
            .push("checkpoint_authorize");
        assert_eq!(
            request
                .get_ref()
                .checkpoint_digest
                .as_ref()
                .expect("digest")
                .value,
            vec![42; 32]
        );
        if self.authorize_unavailable {
            return Err(Status::unavailable("lost authorization response"));
        }
        Ok(Response::new(
            crate::protocol::elitea::runtime::v1::AuthorizeAgentModelCheckpointResponseV1 {
                disposition: self
                    .authorize_disposition
                    .expect("authorization disposition") as i32,
                rejection: None,
            },
        ))
    }

    async fn begin_execution(
        &self,
        _request: Request<BeginExecutionRequestV1>,
    ) -> Result<Response<BeginExecutionResponseV1>, Status> {
        self.trace.lock().expect("trace").push("begin");
        Ok(Response::new(BeginExecutionResponseV1 {
            disposition: BeginExecutionDispositionV1::StartedNow as i32,
            rejection: None,
        }))
    }

    async fn authorize_invocation(
        &self,
        _request: Request<AuthorizeInvocationRequestV1>,
    ) -> Result<Response<AuthorizeInvocationResponseV1>, Status> {
        self.trace.lock().expect("trace").push("authorize");
        if self.authorize_unavailable {
            return Err(Status::unavailable(
                "test authorization transport unavailable",
            ));
        }
        let disposition = self
            .authorize_disposition
            .expect("terminal recovery must not authorize");
        Ok(Response::new(AuthorizeInvocationResponseV1 {
            disposition: disposition as i32,
            rejection: None,
        }))
    }

    async fn renew_lease(
        &self,
        _request: Request<RenewLeaseRequestV1>,
    ) -> Result<Response<RenewLeaseResponseV1>, Status> {
        self.trace.lock().expect("trace").push("renew");
        let attempt = self.renew_attempts.fetch_add(1, Ordering::SeqCst);
        if self
            .renew_fail_after
            .is_some_and(|successful| attempt >= successful)
        {
            return Err(Status::unavailable("test lease renewal unavailable"));
        }
        Ok(Response::new(RenewLeaseResponseV1 {
            lease_expires_at_unix_millis: NOW + 600_000,
            desired_state: DesiredExecutionStateV1::Running as i32,
            rejection: None,
        }))
    }

    async fn observe_desired_state(
        &self,
        _request: Request<ObserveDesiredStateRequestV1>,
    ) -> Result<Response<ObserveDesiredStateResponseV1>, Status> {
        self.trace.lock().expect("trace").push("observe");
        Ok(Response::new(ObserveDesiredStateResponseV1 {
            desired_state: self
                .observe_states
                .lock()
                .expect("observe states")
                .pop_front()
                .unwrap_or(DesiredExecutionStateV1::Running as i32),
            rejection: None,
        }))
    }

    async fn prepare_settlement(
        &self,
        request: Request<PrepareSettlementRequestV1>,
    ) -> Result<Response<PrepareSettlementResponseV1>, Status> {
        self.trace.lock().expect("trace").push("settlement");
        if self.settlement_fails {
            return Err(Status::unavailable("test settlement unavailable"));
        }
        let outcome = request
            .into_inner()
            .proposal
            .expect("settlement proposal")
            .requested_outcome;
        Ok(Response::new(PrepareSettlementResponseV1 {
            settlement_receipt_id: "settlement-receipt-1".to_owned(),
            outcome,
            rejection: None,
        }))
    }
}

struct InvalidAgentInput {
    trace: Arc<Mutex<Vec<&'static str>>>,
}

struct ValidAgentInput {
    trace: Arc<Mutex<Vec<&'static str>>>,
}

struct KindAgentInput {
    trace: Arc<Mutex<Vec<&'static str>>>,
    kind: AgentExecutionKind,
}

#[async_trait]
impl AgentInputMaterializer for ValidAgentInput {
    async fn materialize_checkpoint(
        &self,
        _: &crate::protocol::control::LiveModelCheckpointInspection,
    ) -> Result<MaterializedInput, InputContentError> {
        self.trace.lock().expect("trace").push("checkpoint_input");
        Ok(MaterializedInput::for_test(decode_hex(include_str!(
            "../../tests/fixtures/agent_application_input.hex"
        ))))
    }
    async fn materialize(
        &self,
        _execution: &crate::protocol::control::LeaseMonitoredAgentExecution,
    ) -> Result<MaterializedInput, InputContentError> {
        self.trace.lock().expect("trace").push("input");
        Ok(MaterializedInput::for_test(decode_hex(include_str!(
            "../../tests/fixtures/agent_application_input.hex"
        ))))
    }
}

#[async_trait]
impl AgentInputMaterializer for KindAgentInput {
    async fn materialize(
        &self,
        _execution: &crate::protocol::control::LeaseMonitoredAgentExecution,
    ) -> Result<MaterializedInput, InputContentError> {
        self.trace.lock().expect("trace").push("input");
        let fixture = match self.kind {
            AgentExecutionKind::Application => {
                include_str!("../../tests/fixtures/agent_application_input.hex")
            }
            AgentExecutionKind::Adhoc => {
                include_str!("../../tests/fixtures/agent_adhoc_input.hex")
            }
        };
        Ok(MaterializedInput::for_test(decode_hex(fixture)))
    }
}

#[async_trait]
impl AgentInputMaterializer for InvalidAgentInput {
    async fn materialize(
        &self,
        _execution: &crate::protocol::control::LeaseMonitoredAgentExecution,
    ) -> Result<MaterializedInput, InputContentError> {
        self.trace.lock().expect("trace").push("input");
        Err(InputContentError::InvalidInput(
            "the test agent input is invalid",
        ))
    }
}

fn recovery_control(
    trace: Arc<Mutex<Vec<&'static str>>>,
    settlement_fails: bool,
) -> Arc<AgentControlClient<RecoveryControl>> {
    recovery_control_with_policy(trace, settlement_fails, None, [])
}

fn recovery_control_with_policy(
    trace: Arc<Mutex<Vec<&'static str>>>,
    settlement_fails: bool,
    renew_fail_after: Option<usize>,
    observe_states: impl IntoIterator<Item = DesiredExecutionStateV1>,
) -> Arc<AgentControlClient<RecoveryControl>> {
    Arc::new(
        AgentControlClient::new(
            RecoveryControl {
                trace,
                claim_fixture: "accepted_claim",
                settlement_fails,
                authorize_disposition: None,
                authorize_unavailable: false,
                renew_fail_after,
                renew_attempts: AtomicUsize::new(0),
                observe_states: Mutex::new(
                    observe_states
                        .into_iter()
                        .map(|state| state as i32)
                        .collect(),
                ),
            },
            ControlGrpcConfig {
                deadline: Duration::from_secs(1),
                workload_session_id: "workload-1".to_owned(),
                producer_id: "worker-1".to_owned(),
            },
        )
        .expect("recovery control"),
    )
}

fn authorization_control_with_disposition(
    trace: Arc<Mutex<Vec<&'static str>>>,
    disposition: AuthorizeInvocationDispositionV1,
) -> Arc<AgentControlClient<RecoveryControl>> {
    Arc::new(
        AgentControlClient::new(
            RecoveryControl {
                trace,
                claim_fixture: "accepted_claim",
                settlement_fails: false,
                authorize_disposition: Some(disposition),
                authorize_unavailable: false,
                renew_fail_after: None,
                renew_attempts: AtomicUsize::new(0),
                observe_states: Mutex::new(VecDeque::new()),
            },
            ControlGrpcConfig {
                deadline: Duration::from_secs(1),
                workload_session_id: "workload-1".to_owned(),
                producer_id: "worker-1".to_owned(),
            },
        )
        .expect("authorization control"),
    )
}

fn authorization_control(
    trace: Arc<Mutex<Vec<&'static str>>>,
) -> Arc<AgentControlClient<RecoveryControl>> {
    authorization_control_with_disposition(
        trace,
        AuthorizeInvocationDispositionV1::AlreadyAuthorized,
    )
}

fn authorized_control(
    trace: Arc<Mutex<Vec<&'static str>>>,
) -> Arc<AgentControlClient<RecoveryControl>> {
    authorized_control_with_policy(trace, [])
}

fn authorized_control_with_policy(
    trace: Arc<Mutex<Vec<&'static str>>>,
    observe_states: impl IntoIterator<Item = DesiredExecutionStateV1>,
) -> Arc<AgentControlClient<RecoveryControl>> {
    Arc::new(
        AgentControlClient::new(
            RecoveryControl {
                trace,
                claim_fixture: "accepted_claim",
                settlement_fails: false,
                authorize_disposition: Some(AuthorizeInvocationDispositionV1::AuthorizedNow),
                authorize_unavailable: false,
                renew_fail_after: None,
                renew_attempts: AtomicUsize::new(0),
                observe_states: Mutex::new(
                    observe_states
                        .into_iter()
                        .map(|state| state as i32)
                        .collect(),
                ),
            },
            ControlGrpcConfig {
                deadline: Duration::from_secs(1),
                workload_session_id: "workload-1".to_owned(),
                producer_id: "worker-1".to_owned(),
            },
        )
        .expect("authorized control"),
    )
}

fn ambiguous_output_recovery_control(
    trace: Arc<Mutex<Vec<&'static str>>>,
) -> Arc<AgentControlClient<RecoveryControl>> {
    Arc::new(
        AgentControlClient::new(
            RecoveryControl {
                trace,
                claim_fixture: "claim_recover_ambiguous_invocation_noack",
                settlement_fails: false,
                authorize_disposition: None,
                authorize_unavailable: false,
                renew_fail_after: None,
                renew_attempts: AtomicUsize::new(0),
                observe_states: Mutex::new(VecDeque::new()),
            },
            ControlGrpcConfig {
                deadline: Duration::from_secs(1),
                workload_session_id: "workload-1".to_owned(),
                producer_id: "worker-1".to_owned(),
            },
        )
        .expect("ambiguous output recovery control"),
    )
}

fn unavailable_authorization_control(
    trace: Arc<Mutex<Vec<&'static str>>>,
) -> Arc<AgentControlClient<RecoveryControl>> {
    Arc::new(
        AgentControlClient::new(
            RecoveryControl {
                trace,
                claim_fixture: "accepted_claim",
                settlement_fails: false,
                authorize_disposition: None,
                authorize_unavailable: true,
                renew_fail_after: None,
                renew_attempts: AtomicUsize::new(0),
                observe_states: Mutex::new(VecDeque::new()),
            },
            ControlGrpcConfig {
                deadline: Duration::from_secs(1),
                workload_session_id: "workload-1".to_owned(),
                producer_id: "worker-1".to_owned(),
            },
        )
        .expect("unavailable authorization control"),
    )
}

struct GatedAuthorizedLifecycle {
    trace: Arc<Mutex<Vec<&'static str>>>,
    started: Mutex<Option<oneshot::Sender<()>>>,
    release: Arc<Semaphore>,
}

impl AuthorizedAgentLifecycle for GatedAuthorizedLifecycle {
    fn inspect_checkpoint<'a>(
        &'a self,
        _: &'a crate::agents::AgentExecutionRequest,
        _: &'a crate::agents::session::AuthorizedNativeCommandBinding,
        _: crate::protocol::control::ClaimBoundSessionAuthority,
        _: Arc<dyn crate::state::StateWriterLease>,
    ) -> std::pin::Pin<
        Box<
            dyn std::future::Future<
                    Output = Result<
                        crate::agents::session::ValidatedModelCheckpoint,
                        crate::agents::runtime::NativeAgentAssemblyError,
                    >,
                > + Send
                + 'a,
        >,
    > {
        Box::pin(async move {
            self.trace.lock().expect("trace").push("checkpoint_inspect");
            let identity = claim_response()
                .receipt
                .expect("receipt")
                .identity
                .expect("identity");
            Ok(
                crate::agents::session::ValidatedModelCheckpoint::test_evidence(
                    identity.execution_id,
                    identity.generation,
                ),
            )
        })
    }
    fn run(
        &self,
        run: super::agent_preparation::AuthorizedAgentRun,
    ) -> std::pin::Pin<
        Box<dyn std::future::Future<Output = AgentAuthorizedLifecycleCompletion> + Send + 'static>,
    > {
        let trace = Arc::clone(&self.trace);
        let started = self.started.lock().expect("started").take();
        let release = Arc::clone(&self.release);
        Box::pin(async move {
            trace.lock().expect("trace").push("authorized");
            let execution_kind = run.execution_kind();
            if let Some(started) = started {
                let _ignored = started.send(());
            }
            release
                .acquire()
                .await
                .expect("authorized lifecycle release")
                .forget();
            let (_request, lease) = run.into_test_cleanup();
            lease.close().await.expect("authorized lease close");
            AgentAuthorizedLifecycleCompletion::for_test(execution_kind)
        })
    }
}

fn recovery_control_with_renew_failure(
    trace: Arc<Mutex<Vec<&'static str>>>,
) -> Arc<AgentControlClient<RecoveryControl>> {
    recovery_control_with_policy(trace, false, Some(0), [])
}

struct RecoveryRedis {
    trace: Arc<Mutex<Vec<&'static str>>>,
    result: Result<RedisRetirementResponse, RedisRetirementClientError>,
}

#[async_trait]
impl RedisRetirementClient for RecoveryRedis {
    async fn retire_delivery(
        &self,
        request: RedisRetirementRequest,
    ) -> Result<RedisRetirementResponse, RedisRetirementClientError> {
        assert_eq!(request.stream(), "runtime.commands.v1");
        self.trace.lock().expect("trace").push("redis");
        self.result
    }
}

fn recovery_retirer(
    trace: Arc<Mutex<Vec<&'static str>>>,
    result: Result<RedisRetirementResponse, RedisRetirementClientError>,
) -> RedisCommandRetirer<RecoveryRedis> {
    RedisCommandRetirer::new(
        RecoveryRedis { trace, result },
        RedisRetirementConfig {
            stream: "runtime.commands.v1".to_owned(),
            group: "runtime-workers".to_owned(),
            consumer: "worker-1".to_owned(),
        },
    )
    .expect("recovery retirer")
}

async fn pending_terminal_recovery() -> (
    tempfile::TempDir,
    AgentOutputPreflight,
    AcceptedTerminalOutputRecovery,
    ExecutionOutputFrameV1,
) {
    let (temporary, root) = root();
    let preflight = preflight(root, "worker-1");
    let empty = preflight.prepare(fresh()).await.expect("empty preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = empty else {
        panic!("new execution spool must be empty");
    };
    let (fresh, output) = empty.into_parts();
    let mut spool = output.into_test_spool();
    let frame = terminal_frame(&fresh);
    spool.persist(frame.clone()).expect("durable terminal");
    drop(spool);
    let recovered = preflight.prepare(fresh).await.expect("terminal recovery");
    let AgentOutputPreflightOutcome::TerminalRecovery(recovery) = recovered else {
        panic!("durable terminal must route to recovery");
    };
    (temporary, preflight, *recovery, frame)
}

async fn pre_invocation_terminal_fixture(
    trace: Arc<Mutex<Vec<&'static str>>>,
) -> (
    tempfile::TempDir,
    Arc<AgentControlClient<RecoveryControl>>,
    PreInvocationTerminal,
    InvocationAdmission,
) {
    pre_invocation_terminal_fixture_with_policy(trace, None, []).await
}

async fn pre_invocation_terminal_fixture_with_policy(
    trace: Arc<Mutex<Vec<&'static str>>>,
    renew_fail_after: Option<usize>,
    observe_states: impl IntoIterator<Item = DesiredExecutionStateV1>,
) -> (
    tempfile::TempDir,
    Arc<AgentControlClient<RecoveryControl>>,
    PreInvocationTerminal,
    InvocationAdmission,
) {
    let (temporary, root) = root();
    let preflight = preflight(root, "worker-1");
    let empty = preflight.prepare(fresh()).await.expect("empty preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = empty else {
        panic!("new execution spool must be empty");
    };
    let control =
        recovery_control_with_policy(Arc::clone(&trace), false, renew_fail_after, observe_states);
    let admission = InvocationAdmission::new(
        InvocationAdmissionConfig::new(1, Duration::from_secs(1)).expect("invocation admission"),
    );
    let outcome = prepare_fresh_agent_invocation_with(
        *empty,
        Arc::clone(&control),
        &admission,
        &InvalidAgentInput { trace },
        Arc::new(|| NOW),
        AgentPreparationConfig::new(Duration::from_secs(10)).expect("preparation config"),
    )
    .await
    .expect("pre-invocation terminal");
    let AgentPreparationOutcome::PreInvocationTerminal(terminal) = outcome else {
        panic!("invalid input must produce a pre-invocation terminal");
    };
    (temporary, control, *terminal, admission)
}

fn lease_config() -> ClaimLeaseMonitorConfig {
    ClaimLeaseMonitorConfig::new(Duration::from_secs(10)).expect("lease config")
}

fn recovery_config(max_output_sessions: usize) -> AgentTerminalRecoveryConfig {
    AgentTerminalRecoveryConfig::new(max_output_sessions).expect("recovery config")
}

fn progress_frame(
    fresh: &FreshAgentDelivery,
    fence: ExecutionFenceV1,
    sequence: u64,
    claim_handoff_watermark: u64,
) -> ExecutionOutputFrameV1 {
    let identity = fresh.spool_identity();
    let event = decode_current_node_event_json(
        br#"{"type":"agent_response","content":"durable progress"}"#,
    )
    .expect("valid current NodeEvent");
    let event_bytes = event.encode_to_vec();
    ExecutionOutputFrameV1 {
        output_schema_revision: OUTPUT_SCHEMA_REVISION.to_owned(),
        stream_id: format!("{}:{}", identity.execution_id, identity.generation),
        identity: Some(ExecutionIdentityV1 {
            tenant_id: identity.tenant_id,
            resource_project_id: identity.resource_project_id,
            projection_project_id: identity.projection_project_id,
            command_id: identity.command_id,
            execution_id: identity.execution_id,
            generation: identity.generation,
        }),
        fence: Some(fence),
        logical_output_id: format!("node-event:execution-1:{sequence}"),
        event_id: format!("command-1:{sequence}"),
        sequence,
        claim_handoff_watermark,
        event_type: ExecutionOutputEventTypeV1::NodeEvent as i32,
        occurred_at_unix_millis: NOW,
        payload_digest: Some(DigestV1 {
            algorithm: DigestAlgorithmV1::Sha256 as i32,
            value: digest::digest(&digest::SHA256, &event_bytes)
                .as_ref()
                .to_vec(),
        }),
        terminal: false,
        settlement_proposal: None,
        payload: Some(execution_output_frame_v1::Payload::NodeEvent(event)),
    }
}

fn terminal_frame(fresh: &FreshAgentDelivery) -> ExecutionOutputFrameV1 {
    let identity = fresh.spool_identity();
    let claim = claim_response();
    let receipt = claim.receipt.expect("claim receipt");
    let input_bundle_digest = receipt
        .input_bundle_ref
        .as_ref()
        .and_then(|reference| reference.digest.clone())
        .expect("claim input bundle digest");
    let request = receipt
        .input_bundle
        .as_ref()
        .and_then(|bundle| bundle.entries.first())
        .expect("claim request entry");
    let request_digest = request
        .content
        .as_ref()
        .and_then(|content| content.digest.clone())
        .expect("claim request digest");
    let request_version = request.immutable_version.clone();
    let mut frame =
        ExecutionOutputFrameV1::decode(output_bytes("completed_output_frame").as_slice())
            .expect("Python terminal output fixture");
    frame.stream_id = format!("{}:{}", identity.execution_id, identity.generation);
    frame.identity = Some(ExecutionIdentityV1 {
        tenant_id: identity.tenant_id,
        resource_project_id: identity.resource_project_id,
        projection_project_id: identity.projection_project_id,
        command_id: identity.command_id,
        execution_id: identity.execution_id,
        generation: identity.generation,
    });
    frame.fence = receipt.fence;
    frame.claim_handoff_watermark = fresh.claim_handoff_watermark();
    let Some(execution_output_frame_v1::Payload::AgentExecution(result)) = frame.payload.as_mut()
    else {
        panic!("terminal agent result");
    };
    result.input_bundle_digest = Some(input_bundle_digest);
    result.request_immutable_version = request_version;
    result.request_content_digest = Some(request_digest);
    let payload_digest = DigestV1 {
        algorithm: DigestAlgorithmV1::Sha256 as i32,
        value: digest::digest(&digest::SHA256, &result.encode_to_vec())
            .as_ref()
            .to_vec(),
    };
    frame.payload_digest = Some(payload_digest.clone());
    frame
        .settlement_proposal
        .as_mut()
        .expect("terminal settlement proposal")
        .terminal_payload_digest = Some(payload_digest);
    frame
}

#[derive(Clone, Copy)]
enum LiveProgressAction {
    Acknowledge,
    PersistThenUnavailable,
    PersistChangedFrameThenUnavailable,
    AuthorizationFailed,
    RejectCancellation,
    RetainAcknowledgementThenWait,
}

#[derive(Clone, Copy)]
enum ReplayProgressAction {
    Acknowledge,
    AuthorizationFailed,
    RetainAcknowledgementThenWait,
}

struct FakeProgressState {
    live_actions: Mutex<VecDeque<LiveProgressAction>>,
    replay_actions: Mutex<VecDeque<ReplayProgressAction>>,
    connects: AtomicUsize,
    sends: AtomicUsize,
    replays: AtomicUsize,
    live_closes: AtomicUsize,
    replay_closes: AtomicUsize,
    frames: Mutex<Vec<ExecutionOutputFrameV1>>,
    live_started: Semaphore,
    live_release: Semaphore,
    replay_started: Semaphore,
    replay_release: Semaphore,
}

impl FakeProgressState {
    fn new(
        live_actions: impl IntoIterator<Item = LiveProgressAction>,
        replay_actions: impl IntoIterator<Item = ReplayProgressAction>,
    ) -> Arc<Self> {
        Arc::new(Self {
            live_actions: Mutex::new(live_actions.into_iter().collect()),
            replay_actions: Mutex::new(replay_actions.into_iter().collect()),
            connects: AtomicUsize::new(0),
            sends: AtomicUsize::new(0),
            replays: AtomicUsize::new(0),
            live_closes: AtomicUsize::new(0),
            replay_closes: AtomicUsize::new(0),
            frames: Mutex::new(Vec::new()),
            live_started: Semaphore::new(0),
            live_release: Semaphore::new(0),
            replay_started: Semaphore::new(0),
            replay_release: Semaphore::new(0),
        })
    }
}

#[derive(Clone)]
struct FakeProgressConnector {
    state: Arc<FakeProgressState>,
}

struct FakeProgressSession {
    prepared: Option<PreparedOutputSpool>,
    state: Arc<FakeProgressState>,
    retained: Mutex<Option<ProgressReplayDecision>>,
}

#[async_trait]
impl AgentProgressSession for FakeProgressSession {
    async fn publish_progress(
        &mut self,
        frame: &ExecutionOutputFrameV1,
    ) -> Result<ProgressReplayDecision, OutputGrpcError> {
        self.state.sends.fetch_add(1, Ordering::SeqCst);
        self.state
            .frames
            .lock()
            .expect("progress frames")
            .push(frame.clone());
        let action = self
            .state
            .live_actions
            .lock()
            .expect("live actions")
            .pop_front()
            .expect("scripted live progress action");
        match action {
            LiveProgressAction::Acknowledge => Ok(ProgressReplayDecision::Acknowledged(
                test_acknowledged_progress(frame).expect("synthetic live progress ACK"),
            )),
            LiveProgressAction::PersistThenUnavailable => {
                self.prepared
                    .as_mut()
                    .expect("fresh prepared spool")
                    .persist(frame.clone())?;
                Err(OutputGrpcError::Unavailable(
                    "the scripted live progress stream is unavailable",
                ))
            }
            LiveProgressAction::PersistChangedFrameThenUnavailable => {
                let mut changed = frame.clone();
                changed.occurred_at_unix_millis += 1;
                self.prepared
                    .as_mut()
                    .expect("fresh prepared spool")
                    .persist(changed)?;
                Err(OutputGrpcError::Unavailable(
                    "the scripted changed progress stream is unavailable",
                ))
            }
            LiveProgressAction::AuthorizationFailed => Err(OutputGrpcError::Protocol(
                OutputProtocolError::AuthorizationFailed(
                    "the scripted progress session is not authorized",
                ),
            )),
            LiveProgressAction::RejectCancellation => Ok(ProgressReplayDecision::Rejected(
                test_rejected_progress(frame, ProgressRejectionWinner::Cancelled)
                    .expect("synthetic progress rejection"),
            )),
            LiveProgressAction::RetainAcknowledgementThenWait => {
                *self.retained.lock().expect("retained progress decision") =
                    Some(ProgressReplayDecision::Acknowledged(
                        test_acknowledged_progress(frame).expect("synthetic retained progress ACK"),
                    ));
                self.state.live_started.add_permits(1);
                self.state
                    .live_release
                    .acquire()
                    .await
                    .expect("live progress release")
                    .forget();
                self.retained
                    .lock()
                    .expect("retained progress decision")
                    .take()
                    .ok_or(OutputGrpcError::Unavailable(
                        "the scripted retained progress decision is unavailable",
                    ))
            }
        }
    }

    fn take_progress_decision(
        &self,
        _frame: &ExecutionOutputFrameV1,
    ) -> Result<Option<ProgressReplayDecision>, OutputGrpcError> {
        Ok(self
            .retained
            .lock()
            .map_err(|_| {
                OutputGrpcError::Unavailable(
                    "the scripted retained progress decision is unavailable",
                )
            })?
            .take())
    }

    async fn close(&mut self) -> Result<(), OutputGrpcError> {
        self.state.live_closes.fetch_add(1, Ordering::SeqCst);
        Ok(())
    }
}

struct FakeProgressReplaySession {
    prepared: Option<PreparedOutputSpool>,
    expected: ExecutionOutputFrameV1,
    state: Arc<FakeProgressState>,
    action: ReplayProgressAction,
    retained: Option<ProgressReplayDecision>,
}

#[async_trait]
impl AgentProgressReplaySession for FakeProgressReplaySession {
    async fn wait(&mut self) -> Result<ProgressReplayDecision, OutputGrpcError> {
        if self.retained.is_none() {
            if matches!(self.action, ReplayProgressAction::AuthorizationFailed) {
                return Err(OutputGrpcError::Protocol(
                    OutputProtocolError::AuthorizationFailed(
                        "the scripted progress replay is not authorized",
                    ),
                ));
            }
            let prepared = self.prepared.as_mut().ok_or(OutputGrpcError::Unavailable(
                "the scripted replay spool is unavailable",
            ))?;
            assert!(prepared.replays(&self.expected));
            prepared.reconcile_pending_through(self.expected.sequence)?;
            self.retained = Some(ProgressReplayDecision::Acknowledged(
                test_acknowledged_progress(&self.expected)
                    .expect("synthetic restored progress ACK"),
            ));
            if matches!(
                self.action,
                ReplayProgressAction::RetainAcknowledgementThenWait
            ) {
                self.state.replay_started.add_permits(1);
                self.state
                    .replay_release
                    .acquire()
                    .await
                    .expect("replay progress release")
                    .forget();
            }
        }
        self.retained.take().ok_or(OutputGrpcError::Unavailable(
            "the scripted replay decision is unavailable",
        ))
    }

    async fn close(&mut self) -> Result<(), OutputGrpcError> {
        self.state.replay_closes.fetch_add(1, Ordering::SeqCst);
        Ok(())
    }
}

#[async_trait]
impl AgentProgressConnector for FakeProgressConnector {
    type Session = FakeProgressSession;
    type Replay = FakeProgressReplaySession;

    async fn connect_progress(
        &self,
        prepared: PreparedOutputSpool,
    ) -> Result<Self::Session, OutputGrpcError> {
        assert_eq!(prepared.pending_frame_count(), 0);
        self.state.connects.fetch_add(1, Ordering::SeqCst);
        Ok(FakeProgressSession {
            prepared: Some(prepared),
            state: Arc::clone(&self.state),
            retained: Mutex::new(None),
        })
    }

    async fn start_progress_replay(
        &self,
        prepared: PreparedOutputSpool,
        expected: &ExecutionOutputFrameV1,
    ) -> Result<Self::Replay, OutputGrpcError> {
        assert_eq!(prepared.pending_frame_count(), 1);
        assert!(prepared.replays(expected));
        self.state.replays.fetch_add(1, Ordering::SeqCst);
        let action = self
            .state
            .replay_actions
            .lock()
            .expect("replay actions")
            .pop_front()
            .expect("scripted replay progress action");
        Ok(FakeProgressReplaySession {
            prepared: Some(prepared),
            expected: expected.clone(),
            state: Arc::clone(&self.state),
            action,
            retained: None,
        })
    }
}

#[async_trait]
impl AgentTerminalReplay for FakeProgressConnector {
    async fn replay_terminal(
        &self,
        mut spool: PreparedOutputSpool,
        verified: &crate::protocol::command::VerifiedAgentCommand,
        expected: &ExecutionOutputFrameV1,
    ) -> Result<DurablyAckedTerminal, OutputGrpcError> {
        assert_eq!(spool.pending_frame_count(), 1);
        assert!(spool.replays(expected));
        self.state
            .frames
            .lock()
            .expect("progress frames")
            .push(expected.clone());
        spool.reconcile_pending_through(expected.sequence)?;
        test_acknowledged_terminal(verified, expected)
    }
}

struct ImmediateTextAgent;

#[async_trait]
impl Agent for ImmediateTextAgent {
    fn name(&self) -> &'static str {
        "root-agent"
    }

    fn description(&self) -> &'static str {
        "native lifecycle fixture"
    }

    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        &[]
    }

    async fn run(&self, context: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        let mut event = Event::with_id("llm-1", context.invocation_id());
        event.timestamp = Utc
            .timestamp_millis_opt(NOW + 1)
            .single()
            .expect("valid fixture time");
        event.author = self.name().to_owned();
        event.llm_response.content = Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::Text {
                text: "hello".to_owned(),
            }],
        });
        event.llm_response.partial = false;
        event.llm_response.turn_complete = true;
        event.llm_response.finish_reason = Some(FinishReason::Stop);
        Ok(Box::pin(stream::iter([Ok(event)])))
    }
}

struct SensitiveInterruptAgent;

#[async_trait]
impl Agent for SensitiveInterruptAgent {
    fn name(&self) -> &'static str {
        "root-agent"
    }

    fn description(&self) -> &'static str {
        "sensitive interrupt lifecycle fixture"
    }

    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        &[]
    }

    async fn run(&self, context: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        let invocation_id = context.invocation_id().to_owned();
        let arguments = json!({"value": 21, "api_token": "never-publish"});
        let mut call = Event::with_id("llm-sensitive", &invocation_id);
        call.timestamp = Utc
            .timestamp_millis_opt(NOW + 1)
            .single()
            .expect("valid fixture time");
        call.author = self.name().to_owned();
        call.llm_response.content = Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::FunctionCall {
                name: "double".to_owned(),
                args: arguments.clone(),
                id: Some("call-sensitive".to_owned()),
                thought_signature: None,
            }],
        });
        call.llm_response.partial = false;
        call.llm_response.turn_complete = true;
        call.llm_response.finish_reason = Some(FinishReason::Stop);

        let mut interrupt = Event::with_id("confirm-sensitive", invocation_id);
        interrupt.timestamp = Utc
            .timestamp_millis_opt(NOW + 2)
            .single()
            .expect("valid fixture time");
        interrupt.author = self.name().to_owned();
        interrupt.llm_response.interrupted = true;
        interrupt.llm_response.turn_complete = true;
        interrupt.actions.tool_confirmation = Some(ToolConfirmationRequest {
            tool_name: "double".to_owned(),
            function_call_id: Some("call-sensitive".to_owned()),
            args: arguments,
        });
        Ok(Box::pin(stream::iter([Ok(call), Ok(interrupt)])))
    }
}

struct GatedTextAgent {
    started: Arc<Notify>,
    release: Arc<Semaphore>,
}

#[async_trait]
impl Agent for GatedTextAgent {
    fn name(&self) -> &'static str {
        "root-agent"
    }

    fn description(&self) -> &'static str {
        "gated native lifecycle fixture"
    }

    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        &[]
    }

    async fn run(&self, context: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        let started = Arc::clone(&self.started);
        let release = Arc::clone(&self.release);
        let invocation_id = context.invocation_id().to_owned();
        Ok(Box::pin(stream::unfold(
            Some((started, release, invocation_id)),
            |state| async move {
                let (started, release, invocation_id) = state?;
                started.notify_one();
                release.acquire().await.ok()?.forget();
                let mut event = Event::with_id("llm-1", invocation_id);
                event.timestamp = Utc
                    .timestamp_millis_opt(NOW + 1)
                    .single()
                    .expect("valid fixture time");
                event.author = "root-agent".to_owned();
                event.llm_response.content = Some(Content {
                    role: "model".to_owned(),
                    parts: vec![Part::Text {
                        text: "hello".to_owned(),
                    }],
                });
                event.llm_response.partial = false;
                event.llm_response.turn_complete = true;
                event.llm_response.finish_reason = Some(FinishReason::Stop);
                Some((Ok(event), None))
            },
        )))
    }
}

struct FixedCompletion;

#[async_trait]
impl NativeAgentCompletionSelector for FixedCompletion {
    async fn select(self) -> Result<CompletedAgentBrowserOutput, NativeAgentAssemblyError> {
        Ok(CompletedAgentBrowserOutput::fixture("hello"))
    }
}

struct TestNativeAssembler {
    trace: Arc<Mutex<Vec<&'static str>>>,
    agent: Arc<dyn Agent>,
    sensitive: bool,
}

struct GatedNativeAssembler {
    started: Arc<Notify>,
}

#[async_trait]
impl NativeAgentAssembler for GatedNativeAssembler {
    type Completion = FixedCompletion;

    async fn assemble(
        &self,
        _assembly: AuthorizedNativeAssembly<'_>,
    ) -> Result<AssembledNativeAgentInvocation<Self::Completion>, NativeAgentAssemblyError> {
        self.started.notify_one();
        std::future::pending().await
    }
}

#[async_trait]
impl NativeAgentAssembler for TestNativeAssembler {
    type Completion = FixedCompletion;

    async fn assemble(
        &self,
        assembly: AuthorizedNativeAssembly<'_>,
    ) -> Result<AssembledNativeAgentInvocation<Self::Completion>, NativeAgentAssemblyError> {
        let request = assembly.request();
        self.trace.lock().expect("trace").push(match request.kind {
            AgentExecutionKind::Application => "assemble_application",
            AgentExecutionKind::Adhoc => "assemble_adhoc",
        });
        let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
        sessions
            .create(CreateRequest {
                app_name: "elitea".to_owned(),
                user_id: "user-1".to_owned(),
                session_id: Some("session-1".to_owned()),
                state: HashMap::new(),
            })
            .await
            .map_err(|_| {
                NativeAgentAssemblyError::new(
                    NativeAgentAssemblyErrorCode::DependencyUnavailable,
                    "the test session could not be created",
                )
            })?;
        let runner = Runner::builder()
            .app_name("elitea")
            .agent(Arc::clone(&self.agent))
            .session_service(sessions)
            .build()
            .map_err(|_| {
                NativeAgentAssemblyError::new(
                    NativeAgentAssemblyErrorCode::InvalidConfiguration,
                    "the test native runner is invalid",
                )
            })?;
        let invocation = NativeAgentInvocation::new(
            runner,
            UserId::new("user-1").map_err(|_| {
                NativeAgentAssemblyError::new(
                    NativeAgentAssemblyErrorCode::InvalidConfiguration,
                    "the test native user is invalid",
                )
            })?,
            SessionId::new("session-1").map_err(|_| {
                NativeAgentAssemblyError::new(
                    NativeAgentAssemblyErrorCode::InvalidConfiguration,
                    "the test native session is invalid",
                )
            })?,
            Content::new("user").with_text("hello"),
        );
        let application_details = match request.kind {
            AgentExecutionKind::Application => json!({"id": 11, "version_id": 22}),
            AgentExecutionKind::Adhoc => json!({}),
        };
        let projection = AgentEventProjectionContext::fixture(application_details);
        let projector = if self.sensitive {
            AgentEventProjector::with_sensitive_tools(projection, sensitive_catalog_fixture())
        } else {
            AgentEventProjector::new(projection)
        }
        .map_err(|_| {
            NativeAgentAssemblyError::new(
                NativeAgentAssemblyErrorCode::InvalidConfiguration,
                "the test event projector is invalid",
            )
        })?;
        Ok(AssembledNativeAgentInvocation::new(
            invocation,
            projector,
            FixedCompletion,
        ))
    }
}

fn sensitive_catalog_fixture() -> SensitiveToolCatalog {
    let runtime = json!({"toolkit_security": {
        "sensitive_tools": {"fixture": ["double"]},
        "sensitive_action_company_name": "Example Org"
    }});
    let policy = ToolAdmissionPolicy::from_runtime_config(
        runtime.as_object().expect("runtime security dictionary"),
    )
    .expect("runtime policy");
    SensitiveToolCatalog::fixture(
        "double",
        policy
            .sensitive_tool("fixture", "Fixture Tools", "double")
            .expect("sensitive fixture policy"),
        false,
    )
    .expect("sensitive fixture catalog")
}

async fn fresh_progress_publisher(
    state: Arc<FakeProgressState>,
    max_output_sessions: usize,
) -> (
    tempfile::TempDir,
    crate::protocol::command::VerifiedAgentCommand,
    FreshAgentProgressPublisher<FakeProgressConnector>,
) {
    fresh_progress_publisher_with_handoff(state, max_output_sessions, None).await
}

async fn fresh_progress_publisher_with_handoff(
    state: Arc<FakeProgressState>,
    max_output_sessions: usize,
    handoff: Option<u64>,
) -> (
    tempfile::TempDir,
    crate::protocol::command::VerifiedAgentCommand,
    FreshAgentProgressPublisher<FakeProgressConnector>,
) {
    let (temporary, output_root) = root();
    let outcome = preflight(output_root, "worker-1")
        .prepare(fresh())
        .await
        .expect("fresh progress preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = outcome else {
        panic!("new progress spool must be empty");
    };
    let (fresh, output) = empty.into_parts();
    let (_delivery, verified, claim) = fresh.into_parts();
    let authority = match handoff {
        Some(handoff) => test_agent_output_authority_with_handoff(claim, handoff),
        None => test_agent_output_authority(claim),
    };
    let cursor = authority
        .into_output_cursor(&verified)
        .expect("claim-bound output cursor");
    let result_binding = crate::agents::result::AgentResultBinding::from_input_binding(
        &crate::agents::AgentInputBinding {
            input_bundle_id: "bundle-1".to_owned(),
            input_bundle_digest: [0x41; 32],
            request_entry_id: "request.json".to_owned(),
            request_immutable_version: "version-1".to_owned(),
            request_content_digest: [0x42; 32],
        },
    );
    let publisher = FreshAgentProgressPublisher::new(
        cursor,
        result_binding,
        output,
        FakeProgressConnector { state },
        AgentProgressPublisherConfig::new(max_output_sessions).expect("progress publisher config"),
    );
    (temporary, verified, publisher)
}

fn browser_progress(content: &'static str) -> crate::protocol::elitea::runtime::v1::NodeEventV1 {
    let raw = format!(r#"{{"type":"agent_response","content":"{content}"}}"#);
    decode_current_node_event_json(raw.as_bytes()).expect("valid browser progress event")
}

fn browser_full_message(
    content: &'static str,
) -> crate::protocol::elitea::runtime::v1::NodeEventV1 {
    let raw = format!(r#"{{"type":"full_message","content":"{content}"}}"#);
    decode_current_node_event_json(raw.as_bytes()).expect("valid full message event")
}

fn browser_hitl_interrupt() -> crate::protocol::elitea::runtime::v1::NodeEventV1 {
    decode_current_node_event_json(
        br#"{"type":"agent_hitl_interrupt","content":"approval required"}"#,
    )
    .expect("valid HITL interrupt event")
}

fn browser_authorization_card(
    interrupt_id: &str,
    tool_call_id: &str,
    parent_name: &str,
    parent_call_id: &str,
) -> crate::protocol::elitea::runtime::v1::NodeEventV1 {
    let raw = json!({
        "type": "mcp_authorization_required",
        "content": "Toolkit authorization is required.",
        "response_metadata": {
            "thread_id": "thread-1",
            "guardrail_type": "mcp_auth",
            "interrupt_id": interrupt_id,
            "tool_call_id": tool_call_id,
            "tool_name": "search_records",
            "toolkit_name": "Customer Records",
            "toolkit_type": "openapi",
            "server_url": "https://records.example.invalid/api",
            "parent_agent_name": parent_name,
            "parent_agent_call_id": parent_call_id,
            "parent_agent_path": [{"name": parent_name, "call_id": parent_call_id}],
            "available_actions": ["authorize", "skip"],
            "resume_strategy": "root"
        }
    });
    decode_current_node_event_json(&serde_json::to_vec(&raw).expect("authorization browser JSON"))
        .expect("valid authorization browser event")
}

#[test]
fn progress_session_budget_matches_the_deployed_v1_bounds() {
    assert!(AgentProgressPublisherConfig::new(1).is_ok());
    assert!(AgentProgressPublisherConfig::new(8).is_ok());
    for invalid in [0, 9] {
        let error = AgentProgressPublisherConfig::new(invalid)
            .expect_err("progress session budget must be bounded");
        assert_eq!(error.code(), "agent_progress.invalid_configuration");
        assert!(!error.retryable());
    }
}

#[tokio::test]
async fn fresh_progress_keeps_one_live_session_across_exact_acked_events() {
    let state = FakeProgressState::new(
        [
            LiveProgressAction::Acknowledge,
            LiveProgressAction::Acknowledge,
        ],
        [],
    );
    let (_temporary, verified, mut publisher) =
        fresh_progress_publisher(Arc::clone(&state), 2).await;

    let first = publisher
        .publish(&verified, browser_progress("first"), NOW)
        .await
        .expect("first progress ACK");
    let second = publisher
        .publish(&verified, browser_progress("second"), NOW + 1)
        .await
        .expect("second progress ACK");

    assert_eq!(
        first,
        AgentProgressPublishOutcome::Acknowledged { sequence: 5 }
    );
    assert_eq!(
        second,
        AgentProgressPublishOutcome::Acknowledged { sequence: 6 }
    );
    assert_eq!(state.connects.load(Ordering::SeqCst), 1);
    assert_eq!(state.sends.load(Ordering::SeqCst), 2);
    assert_eq!(state.replays.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn acked_full_message_binds_the_exact_canonical_browser_bytes() {
    let state = FakeProgressState::new([LiveProgressAction::Acknowledge], []);
    let (_temporary, verified, mut publisher) =
        fresh_progress_publisher(Arc::clone(&state), 2).await;
    let full_message = browser_full_message("final answer");
    let browser_json = crate::protocol::node_event::encode_current_node_event_json(&full_message)
        .expect("canonical full message JSON");
    let expected_digest = digest::digest(&digest::SHA256, &browser_json);

    let outcome = publisher
        .publish_full_message(&verified, full_message, NOW)
        .await
        .expect("durable full message ACK");
    assert_eq!(
        outcome,
        AgentProgressPublishOutcome::Acknowledged { sequence: 5 }
    );
    let later = publisher
        .publish(&verified, browser_progress("must not follow"), NOW + 1)
        .await
        .expect_err("full_message must remain the last progress event");
    assert_eq!(later.code(), "agent_progress.invalid_state");

    let artifact = publisher
        .into_test_acked_full_message()
        .expect("ACKed full message artifact proof");
    assert_eq!(
        artifact.artifact_id,
        format!(
            "node-event:{}:full-message",
            verified.command().execution_id
        )
    );
    assert_eq!(artifact.byte_length, browser_json.len() as u64);
    assert_eq!(artifact.digest.as_slice(), expected_digest.as_ref());
    assert_eq!(
        artifact.immutable_version,
        format!("sha256:{}", hex_lower(expected_digest.as_ref()))
    );
}

#[tokio::test]
async fn acked_sensitive_interrupt_mints_only_paused_hitl_result_authority() {
    let state = FakeProgressState::new([LiveProgressAction::Acknowledge], []);
    let (_temporary, verified, mut publisher) =
        fresh_progress_publisher(Arc::clone(&state), 2).await;
    let event = browser_hitl_interrupt();
    let browser_json = crate::protocol::node_event::encode_current_node_event_json(&event)
        .expect("canonical HITL JSON");

    let outcome = publisher
        .publish_result_event(&verified, event, NOW)
        .await
        .expect("durable HITL ACK");
    assert_eq!(
        outcome,
        AgentProgressPublishOutcome::Acknowledged { sequence: 5 }
    );
    let (terminal_state, artifact) = publisher
        .into_test_acked_result()
        .expect("ACKed HITL result proof");
    assert_eq!(terminal_state, AgentTerminalState::PausedHitl);
    assert_eq!(
        artifact.artifact_id,
        format!(
            "node-event:{}:hitl-interrupt",
            verified.command().execution_id
        )
    );
    assert_eq!(artifact.byte_length, browser_json.len() as u64);
}

#[tokio::test]
async fn acked_authorization_aggregate_mints_only_paused_authorization_result_authority() {
    let state = FakeProgressState::new([LiveProgressAction::Acknowledge], []);
    let (_temporary, verified, mut publisher) =
        fresh_progress_publisher(Arc::clone(&state), 2).await;
    let event = browser_authorization_card("auth-1", "call-1", "resolver", "parent-1");

    let outcome = publisher
        .publish_result_event(&verified, event, NOW)
        .await
        .expect("durable authorization aggregate ACK");
    assert_eq!(
        outcome,
        AgentProgressPublishOutcome::Acknowledged { sequence: 5 }
    );
    let (terminal_state, artifact) = publisher
        .into_test_acked_result()
        .expect("ACKed authorization result proof");
    assert_eq!(terminal_state, AgentTerminalState::PausedMcpAuth);
    assert_eq!(
        artifact.artifact_id,
        format!(
            "node-event:{}:mcp-authorization-required",
            verified.command().execution_id
        )
    );
}

#[test]
fn parallel_authorization_cards_aggregate_exact_ids_calls_and_hierarchy_in_order() {
    let first = browser_authorization_card("auth-1", "call-1", "resolver", "parent-1");
    let second = browser_authorization_card("auth-2", "call-2", "resolver", "parent-2");
    let mut pauses = AgentPauseAccumulator::default();
    assert!(pauses.observe(&first).expect("first authorization card"));
    assert!(pauses.observe(&second).expect("second authorization card"));

    let aggregate = pauses.finish().expect("authorization aggregate");
    assert!(matches!(
        aggregate.selection,
        super::output_delivery::FreshAgentTerminalSelection::PausedMcpAuth
    ));
    let metadata: serde_json::Value = serde_json::from_slice(&aggregate.event.response_metadata)
        .expect("aggregate authorization metadata");
    let requests = metadata["authorization_requests"]
        .as_array()
        .expect("authorization requests");
    assert_eq!(requests.len(), 2);
    assert_eq!(requests[0]["interrupt_id"], "auth-1");
    assert_eq!(requests[0]["tool_call_id"], "call-1");
    assert_eq!(requests[0]["parent_agent_call_id"], "parent-1");
    assert_eq!(requests[1]["interrupt_id"], "auth-2");
    assert_eq!(requests[1]["tool_call_id"], "call-2");
    assert_eq!(requests[1]["parent_agent_call_id"], "parent-2");
    assert_eq!(metadata["interrupt_id"], "auth-2");
    assert_eq!(metadata["tool_call_id"], "call-2");
}

#[test]
fn mixed_guardrail_cards_fail_before_terminal_authority_is_selected() {
    let sensitive = decode_current_node_event_json(
        &serde_json::to_vec(&json!({
            "type": "agent_hitl_interrupt",
            "content": "approval required",
            "response_metadata": {
                "hitl_interrupts": [{
                    "interrupt_id": "sensitive-1",
                    "tool_call_id": "sensitive-call-1"
                }]
            }
        }))
        .expect("sensitive browser JSON"),
    )
    .expect("sensitive browser event");
    let authorization = browser_authorization_card("auth-1", "call-1", "resolver", "parent-1");
    let mut pauses = AgentPauseAccumulator::default();
    assert!(pauses.observe(&sensitive).expect("sensitive card"));
    assert!(pauses.observe(&authorization).expect("authorization card"));

    assert!(matches!(
        pauses.finish(),
        Err(AgentPauseAggregationError::MixedGuardrails)
    ));
}

#[tokio::test]
async fn generic_or_unacknowledged_progress_cannot_mint_a_completed_terminal() {
    let state = FakeProgressState::new([LiveProgressAction::PersistThenUnavailable], []);
    let (_temporary, verified, mut publisher) =
        fresh_progress_publisher(Arc::clone(&state), 1).await;

    let generic = publisher
        .publish(&verified, browser_full_message("wrong path"), NOW)
        .await
        .expect_err("generic progress cannot mint a result artifact");
    assert_eq!(generic.code(), "agent_progress.invalid_state");
    publisher
        .publish_full_message(&verified, browser_full_message("not ACKed"), NOW)
        .await
        .expect_err("transport uncertainty must retain the full message");
    assert!(publisher.into_test_acked_full_message().is_none());
    assert_eq!(state.sends.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn restored_full_message_ack_retains_the_same_terminal_artifact_authority() {
    let state = FakeProgressState::new(
        [LiveProgressAction::PersistThenUnavailable],
        [ReplayProgressAction::Acknowledge],
    );
    let (_temporary, verified, mut publisher) =
        fresh_progress_publisher(Arc::clone(&state), 2).await;
    let outcome = publisher
        .publish_full_message(&verified, browser_full_message("restored result"), NOW)
        .await
        .expect("restored full message ACK");
    assert_eq!(
        outcome,
        AgentProgressPublishOutcome::Acknowledged { sequence: 5 }
    );
    assert!(publisher.into_test_acked_full_message().is_some());
    assert_eq!(state.replays.load(Ordering::SeqCst), 1);
}

fn hex_lower(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut output = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        output.push(char::from(HEX[usize::from(byte >> 4)]));
        output.push(char::from(HEX[usize::from(byte & 0x0f)]));
    }
    output
}

#[tokio::test]
async fn uncertain_live_progress_reopens_and_replays_the_exact_durable_frame() {
    let state = FakeProgressState::new(
        [LiveProgressAction::PersistThenUnavailable],
        [ReplayProgressAction::Acknowledge],
    );
    let (_temporary, verified, mut publisher) =
        fresh_progress_publisher(Arc::clone(&state), 2).await;

    let outcome = publisher
        .publish(&verified, browser_progress("recover me"), NOW)
        .await
        .expect("restored progress ACK");

    assert_eq!(
        outcome,
        AgentProgressPublishOutcome::Acknowledged { sequence: 5 }
    );
    assert_eq!(state.connects.load(Ordering::SeqCst), 1);
    assert_eq!(state.sends.load(Ordering::SeqCst), 1);
    assert_eq!(state.live_closes.load(Ordering::SeqCst), 1);
    assert_eq!(state.replays.load(Ordering::SeqCst), 1);
    assert_eq!(state.replay_closes.load(Ordering::SeqCst), 1);
    let frames = state.frames.lock().expect("progress frames");
    assert_eq!(frames.len(), 1);
    assert_eq!(frames[0].sequence, 5);
}

#[tokio::test]
async fn changed_durable_progress_is_rejected_before_a_replay_session_starts() {
    let state = FakeProgressState::new(
        [LiveProgressAction::PersistChangedFrameThenUnavailable],
        [ReplayProgressAction::Acknowledge],
    );
    let (_temporary, verified, mut publisher) =
        fresh_progress_publisher(Arc::clone(&state), 2).await;

    let error = publisher
        .publish(&verified, browser_progress("bind exact bytes"), NOW)
        .await
        .expect_err("changed durable bytes must fail closed");

    assert_eq!(error.code(), "agent_output.invalid_durable_state");
    assert!(!error.retryable());
    assert_eq!(state.connects.load(Ordering::SeqCst), 1);
    assert_eq!(state.replays.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn nonretryable_live_failure_latches_without_a_second_attempt() {
    let state = FakeProgressState::new(
        [
            LiveProgressAction::AuthorizationFailed,
            LiveProgressAction::Acknowledge,
        ],
        [],
    );
    let (_temporary, verified, mut publisher) =
        fresh_progress_publisher(Arc::clone(&state), 2).await;

    let error = publisher
        .publish(&verified, browser_progress("fail closed"), NOW)
        .await
        .expect_err("authorization failure must fail closed");
    assert!(!error.retryable());
    let resumed = publisher
        .resume_pending()
        .await
        .expect_err("failed-closed progress must not retry");
    assert_eq!(resumed.code(), "agent_progress.invalid_state");
    assert_eq!(state.connects.load(Ordering::SeqCst), 1);
    assert_eq!(state.sends.load(Ordering::SeqCst), 1);
    assert_eq!(state.replays.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn nonretryable_replay_failure_latches_without_a_second_replay() {
    let state = FakeProgressState::new(
        [LiveProgressAction::PersistThenUnavailable],
        [
            ReplayProgressAction::AuthorizationFailed,
            ReplayProgressAction::Acknowledge,
        ],
    );
    let (_temporary, verified, mut publisher) =
        fresh_progress_publisher(Arc::clone(&state), 3).await;

    let error = publisher
        .publish(&verified, browser_progress("replay fail closed"), NOW)
        .await
        .expect_err("replay authorization failure must fail closed");
    assert!(!error.retryable());
    let resumed = publisher
        .resume_pending()
        .await
        .expect_err("failed-closed replay must not retry");
    assert_eq!(resumed.code(), "agent_progress.invalid_state");
    assert_eq!(state.connects.load(Ordering::SeqCst), 1);
    assert_eq!(state.sends.load(Ordering::SeqCst), 1);
    assert_eq!(state.replays.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn durable_ack_at_bigint_limit_cannot_be_resent_after_cursor_exhaustion() {
    let state = FakeProgressState::new([LiveProgressAction::Acknowledge], []);
    let (_temporary, verified, mut publisher) =
        fresh_progress_publisher_with_handoff(Arc::clone(&state), 2, Some(i64::MAX as u64 - 1))
            .await;

    let error = publisher
        .publish(&verified, browser_progress("last durable sequence"), NOW)
        .await
        .expect_err("the ACKed BIGINT ceiling must exhaust future output");
    assert_eq!(error.code(), "agent_progress.invalid_state");
    assert!(!error.retryable());
    let resumed = publisher
        .resume_pending()
        .await
        .expect_err("an ACKed exhausted frame must not be resent");
    assert_eq!(resumed.code(), "agent_progress.invalid_state");
    assert_eq!(state.connects.load(Ordering::SeqCst), 1);
    assert_eq!(state.sends.load(Ordering::SeqCst), 1);
    assert_eq!(state.replays.load(Ordering::SeqCst), 0);
    assert_eq!(
        state.frames.lock().expect("progress frames")[0].sequence,
        i64::MAX as u64
    );
}

#[tokio::test]
async fn retryable_progress_exhaustion_never_exceeds_the_session_budget() {
    let state = FakeProgressState::new([LiveProgressAction::PersistThenUnavailable], []);
    let (_temporary, verified, mut publisher) =
        fresh_progress_publisher(Arc::clone(&state), 1).await;

    let first = publisher
        .publish(&verified, browser_progress("bounded retry"), NOW)
        .await
        .expect_err("one unavailable attempt must exhaust the budget");
    assert!(first.retryable());
    let second = publisher
        .resume_pending()
        .await
        .expect_err("exhausted publisher must retain the exact frame");
    assert!(second.retryable());
    assert_eq!(state.connects.load(Ordering::SeqCst), 1);
    assert_eq!(state.sends.load(Ordering::SeqCst), 1);
    assert_eq!(state.replays.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn frame_bound_main_rejection_is_retained_without_resending() {
    let state = FakeProgressState::new([LiveProgressAction::RejectCancellation], []);
    let (_temporary, verified, mut publisher) =
        fresh_progress_publisher(Arc::clone(&state), 1).await;

    let outcome = publisher
        .publish(&verified, browser_progress("stop wins"), NOW)
        .await
        .expect("bound Main rejection");
    assert_eq!(
        outcome,
        AgentProgressPublishOutcome::Rejected { sequence: 5 }
    );
    assert_eq!(
        publisher
            .resume_pending()
            .await
            .expect("retained bound Main rejection"),
        AgentProgressPublishOutcome::Rejected { sequence: 5 }
    );
    assert_eq!(state.sends.load(Ordering::SeqCst), 1);
    assert_eq!(state.replays.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn cancelled_live_wait_recovers_the_retained_ack_without_resending() {
    let state = FakeProgressState::new([LiveProgressAction::RetainAcknowledgementThenWait], []);
    let (_temporary, verified, mut publisher) =
        fresh_progress_publisher(Arc::clone(&state), 2).await;

    {
        let publish = publisher.publish(&verified, browser_progress("cancel wait"), NOW);
        tokio::pin!(publish);
        tokio::select! {
            permit = state.live_started.acquire() => {
                permit.expect("live progress started").forget();
            }
            result = &mut publish => {
                panic!("live progress unexpectedly completed: {}", result.is_ok());
            }
        }
    }

    let outcome = publisher
        .resume_pending()
        .await
        .expect("retained live progress ACK");
    assert_eq!(
        outcome,
        AgentProgressPublishOutcome::Acknowledged { sequence: 5 }
    );
    assert_eq!(state.connects.load(Ordering::SeqCst), 1);
    assert_eq!(state.sends.load(Ordering::SeqCst), 1);
    assert_eq!(state.replays.load(Ordering::SeqCst), 0);
    assert_eq!(state.live_closes.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn cancelled_replay_wait_resumes_the_same_owned_replay_session() {
    let state = FakeProgressState::new(
        [LiveProgressAction::PersistThenUnavailable],
        [ReplayProgressAction::RetainAcknowledgementThenWait],
    );
    let (_temporary, verified, mut publisher) =
        fresh_progress_publisher(Arc::clone(&state), 2).await;

    {
        let publish = publisher.publish(&verified, browser_progress("replay wait"), NOW);
        tokio::pin!(publish);
        tokio::select! {
            permit = state.replay_started.acquire() => {
                permit.expect("progress replay started").forget();
            }
            result = &mut publish => {
                panic!("progress replay unexpectedly completed: {}", result.is_ok());
            }
        }
    }

    let outcome = publisher
        .resume_pending()
        .await
        .expect("retained restored progress ACK");
    assert_eq!(
        outcome,
        AgentProgressPublishOutcome::Acknowledged { sequence: 5 }
    );
    assert_eq!(state.connects.load(Ordering::SeqCst), 1);
    assert_eq!(state.sends.load(Ordering::SeqCst), 1);
    assert_eq!(state.replays.load(Ordering::SeqCst), 1);
    assert_eq!(state.replay_closes.load(Ordering::SeqCst), 1);
}

#[test]
fn claim_bound_output_cursor_advances_only_after_the_exact_progress_ack() {
    let fresh = fresh();
    let handoff = fresh.claim_handoff_watermark();
    let (_delivery, verified, claim) = fresh.into_parts();
    let authority = test_agent_output_authority(claim);
    let mut cursor = authority
        .into_output_cursor(&verified)
        .expect("matching output authority");
    assert_eq!(cursor.next_sequence(), Some(handoff + 1));

    let event = decode_current_node_event_json(
        br#"{"type":"agent_response","content":"claim-bound progress"}"#,
    )
    .expect("valid current NodeEvent");
    let progress = cursor
        .bind_progress(&verified, event, NOW)
        .expect("claim-bound progress");
    let progress_sequence = progress.sequence();
    assert_eq!(progress_sequence, handoff + 1);
    let frame = progress.into_frame();
    assert_eq!(frame.sequence, progress_sequence);
    assert_eq!(frame.claim_handoff_watermark, handoff);
    assert!(frame.fence.is_some());
    assert_eq!(cursor.next_sequence(), None);

    let second_event =
        decode_current_node_event_json(br#"{"type":"agent_response","content":"must wait"}"#)
            .expect("valid current NodeEvent");
    let Err(error) = cursor.bind_progress(&verified, second_event, NOW + 1) else {
        panic!("a second frame cannot be allocated before the first ACK");
    };
    assert!(matches!(
        error,
        crate::protocol::ProtocolError::AuthorizationFailed(_)
    ));

    let mut substituted = frame.clone();
    substituted.occurred_at_unix_millis += 1;
    let wrong_ack = test_acknowledged_progress(&substituted).expect("synthetic bound ACK");
    let error = cursor
        .commit_progress(wrong_ack)
        .expect_err("a same-sequence ACK for different bytes cannot advance the cursor");
    assert!(matches!(
        error,
        crate::protocol::ProtocolError::AuthorizationFailed(_)
    ));
    assert_eq!(cursor.next_sequence(), None);

    let exact_ack = test_acknowledged_progress(&frame).expect("synthetic bound ACK");
    cursor
        .commit_progress(exact_ack)
        .expect("exact progress ACK");
    assert_eq!(cursor.next_sequence(), Some(progress_sequence + 1));
    let terminal = cursor
        .bind_failure_terminal(&verified, RuntimeFailureKind::Internal, NOW + 1)
        .expect("next claim-bound terminal")
        .into_frame();
    assert!(terminal.terminal);
    assert_eq!(terminal.sequence, progress_sequence + 1);
    assert_eq!(terminal.claim_handoff_watermark, handoff);
}

#[test]
fn only_an_exact_bound_winner_can_replace_pending_progress_at_the_same_sequence() {
    for (winner, expected_code) in [
        (
            ProgressRejectionWinner::Cancelled,
            RuntimeErrorCodeV1::Cancelled,
        ),
        (
            ProgressRejectionWinner::DeadlineExceeded,
            RuntimeErrorCodeV1::DeadlineExceeded,
        ),
    ] {
        let fresh = fresh();
        let handoff = fresh.claim_handoff_watermark();
        let (_delivery, verified, claim) = fresh.into_parts();
        let authority = test_agent_output_authority(claim);
        let mut cursor = authority
            .into_output_cursor(&verified)
            .expect("matching output authority");
        let event = decode_current_node_event_json(
            br#"{"type":"agent_response","content":"rejected progress"}"#,
        )
        .expect("valid current NodeEvent");
        let progress = cursor
            .bind_progress(&verified, event, NOW)
            .expect("claim-bound progress");
        let frame = progress.into_frame();
        let rejected = test_rejected_progress(&frame, winner).expect("bound Main winner");
        let terminal = cursor
            .bind_rejected_progress_terminal(&verified, rejected, NOW + 25)
            .expect("same-sequence failure terminal")
            .into_frame();

        assert!(terminal.terminal);
        assert_eq!(terminal.sequence, frame.sequence);
        assert_eq!(terminal.claim_handoff_watermark, handoff);
        assert_eq!(terminal.occurred_at_unix_millis, NOW + 25);
        let Some(execution_output_frame_v1::Payload::RuntimeError(error)) = terminal.payload else {
            panic!("bound winner must create a runtime failure terminal");
        };
        assert_eq!(error.code, expected_code as i32);
    }

    let fresh = fresh();
    let (_delivery, verified, claim) = fresh.into_parts();
    let authority = test_agent_output_authority(claim);
    let mut cursor = authority
        .into_output_cursor(&verified)
        .expect("matching output authority");
    let event = decode_current_node_event_json(br#"{"type":"agent_response"}"#)
        .expect("valid current NodeEvent");
    let progress = cursor
        .bind_progress(&verified, event, NOW)
        .expect("claim-bound progress");
    let frame = progress.into_frame();
    let mut substituted = frame.clone();
    substituted.occurred_at_unix_millis += 1;
    let wrong = test_rejected_progress(&substituted, ProgressRejectionWinner::Cancelled)
        .expect("different frame winner");
    let Err(error) = cursor.bind_rejected_progress_terminal(&verified, wrong, NOW + 25) else {
        panic!("a same-sequence decision for different bytes cannot replace progress");
    };
    assert!(matches!(
        error,
        crate::protocol::ProtocolError::AuthorizationFailed(_)
    ));
}

#[test]
fn output_cursor_rejects_same_identity_changed_intent_before_exposing_a_fence() {
    let fresh = fresh();
    let (_delivery, original, claim) = fresh.into_parts();
    let changed = parse_and_verify_agent_command(
        &bytes("signed_command_same_identity_changed_intent"),
        Some(&TestOnlyConformanceHmacAuthenticator as &dyn SignedCommandAuthenticator),
    )
    .expect("verified changed-intent command");
    assert_eq!(original.command().command_id, changed.command().command_id);
    assert_ne!(
        original.command().deadline_unix_millis,
        changed.command().deadline_unix_millis
    );

    let authority = test_agent_output_authority(claim);
    let Err(error) = authority.into_output_cursor(&changed) else {
        panic!("changed immutable command intent must not reuse output authority");
    };
    assert!(matches!(
        error,
        crate::protocol::ProtocolError::AuthorizationFailed(_)
    ));
}

#[test]
fn output_cursor_rejects_a_different_valid_claim_before_exposing_a_fence() {
    let fresh = fresh();
    let (_delivery, verified, _claim) = fresh.into_parts();
    let authority = test_lease_monitored_input_execution(1, [0x61; 32]).into_output_authority();

    let Err(error) = authority.into_output_cursor(&verified) else {
        panic!("cross-execution output authority must fail");
    };
    assert!(matches!(
        error,
        crate::protocol::ProtocolError::AuthorizationFailed(_)
    ));
}

#[test]
fn output_cursor_latches_exhaustion_after_the_largest_durable_progress_ack() {
    let fresh = fresh();
    let (_delivery, verified, claim) = fresh.into_parts();
    let authority = test_agent_output_authority_with_handoff(claim, i64::MAX as u64 - 1);
    let mut cursor = authority
        .into_output_cursor(&verified)
        .expect("largest durable progress sequence");
    assert_eq!(cursor.next_sequence(), Some(i64::MAX as u64));

    let event = decode_current_node_event_json(br#"{"type":"agent_response"}"#)
        .expect("valid current NodeEvent");
    let progress = cursor
        .bind_progress(&verified, event, NOW)
        .expect("largest durable progress frame");
    let frame = progress.into_frame();
    let acknowledged = test_acknowledged_progress(&frame).expect("synthetic bound ACK");
    let error = cursor
        .commit_progress(acknowledged)
        .expect_err("the ACKed maximum has no contiguous successor");
    assert!(matches!(
        error,
        crate::protocol::ProtocolError::ResourceExhausted(_)
    ));
    assert_eq!(cursor.next_sequence(), None);

    let event = decode_current_node_event_json(br#"{"type":"agent_response"}"#)
        .expect("valid current NodeEvent");
    let Err(error) = cursor.bind_progress(&verified, event, NOW) else {
        panic!("an exhausted cursor must not reuse the ACKed sequence");
    };
    assert!(matches!(
        error,
        crate::protocol::ProtocolError::ResourceExhausted(_)
    ));
}

#[tokio::test]
async fn empty_spool_is_locked_before_fresh_preparation_can_exist() {
    let (_temporary, root) = root();
    let preflight = preflight(root, "worker-1");
    let first = preflight.prepare(fresh()).await.expect("empty preflight");
    assert_eq!(first.kind(), AgentOutputPreflightKind::Empty);

    let Err(error) = preflight.prepare(fresh()).await else {
        panic!("a second live spool owner must be rejected");
    };
    assert!(matches!(
        error,
        AgentOutputPreflightError::Output(OutputGrpcError::Spool(
            SpoolError::OwnershipUnavailable(_)
        ))
    ));
    assert_eq!(error.code(), "agent_output.spool_unavailable");
    assert!(error.retryable());

    drop(first);
    let reopened = preflight
        .prepare(fresh())
        .await
        .expect("reopened preflight");
    assert_eq!(reopened.kind(), AgentOutputPreflightKind::Empty);
}

#[tokio::test]
async fn producer_mismatch_fails_before_creating_an_execution_spool() {
    let (_temporary, root) = root();
    let preflight = preflight(root.clone(), "worker-other");

    let Err(error) = preflight.prepare(fresh()).await else {
        panic!("producer substitution must fail");
    };

    assert!(matches!(
        error,
        AgentOutputPreflightError::InvalidConfiguration(_)
    ));
    assert_eq!(error.code(), "agent_output.invalid_configuration");
    assert!(!error.retryable());
    assert!(fs::read_dir(root).expect("output root").next().is_none());
}

#[tokio::test]
async fn workload_session_mismatch_fails_before_creating_an_execution_spool() {
    let (_temporary, root) = root();
    let mut config = output_config("worker-1");
    config.workload_session_id = "workload-other".to_owned();
    let preflight = AgentOutputPreflight::new(
        root.clone(),
        SpoolMasterKey::new([b'p'; 32]),
        spool_limits(),
        config,
    );

    let Err(error) = preflight.prepare(fresh()).await else {
        panic!("workload-session substitution must fail");
    };

    assert!(matches!(
        error,
        AgentOutputPreflightError::InvalidConfiguration(_)
    ));
    assert_eq!(error.code(), "agent_output.invalid_configuration");
    assert!(!error.retryable());
    assert!(fs::read_dir(root).expect("output root").next().is_none());
}

#[tokio::test]
async fn sole_claim_bound_pending_progress_routes_to_recovery_not_fresh_work() {
    let (_temporary, root) = root();
    let preflight = preflight(root, "worker-1");
    let empty = preflight.prepare(fresh()).await.expect("empty preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = empty else {
        panic!("new execution spool must be empty");
    };
    let (fresh, output) = empty.into_parts();
    let mut spool = output.into_test_spool();
    let fence = claim_response()
        .receipt
        .expect("claim receipt")
        .fence
        .expect("claim fence");
    let sequence = fresh.claim_handoff_watermark() + 1;
    let frame = progress_frame(&fresh, fence, sequence, fresh.claim_handoff_watermark());
    spool.persist(frame).expect("durable progress");
    drop(spool);

    let outcome = preflight.prepare(fresh).await.expect("pending preflight");
    let AgentOutputPreflightOutcome::RecoveryRequiredNoAck(recovery) = outcome else {
        panic!("durable progress must require server-side recovery");
    };
    assert_eq!(
        recovery.kind(),
        AgentOutputRecoveryRequiredKind::PendingProgress
    );
    assert_eq!(recovery.sequence(), sequence);
}

#[tokio::test]
async fn authenticated_handoff_reconciles_a_covered_stale_progress_frame() {
    let (_temporary, root) = root();
    let preflight = preflight(root, "worker-1");
    let empty = preflight.prepare(fresh()).await.expect("empty preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = empty else {
        panic!("new execution spool must be empty");
    };
    let (accepted, output) = empty.into_parts();
    let mut spool = output.into_test_spool();
    let mut old_fence = claim_response()
        .receipt
        .expect("claim receipt")
        .fence
        .expect("claim fence");
    old_fence.fence_token[0] ^= 1;
    let sequence = accepted.claim_handoff_watermark();
    let stale_handoff = sequence.checked_sub(1).expect("positive handoff watermark");
    spool
        .persist(progress_frame(
            &accepted,
            old_fence,
            sequence,
            stale_handoff,
        ))
        .expect("durable stale progress");
    drop(spool);

    let outcome = preflight
        .prepare(accepted)
        .await
        .expect("covered preflight");
    let AgentOutputPreflightOutcome::RecoveryRequiredNoAck(recovery) = outcome else {
        panic!("covered progress must remain a no-ACK recovery outcome");
    };
    assert_eq!(
        recovery.kind(),
        AgentOutputRecoveryRequiredKind::ReconciledStaleProgress
    );
    assert_eq!(recovery.sequence(), sequence);

    let reopened = preflight
        .prepare(fresh())
        .await
        .expect("reconciled spool reopens");
    assert_eq!(reopened.kind(), AgentOutputPreflightKind::Empty);
}

#[tokio::test]
async fn sole_claim_bound_pending_terminal_routes_to_recovery_not_fresh_work() {
    let (_temporary, root) = root();
    let preflight = preflight(root, "worker-1");
    let empty = preflight.prepare(fresh()).await.expect("empty preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = empty else {
        panic!("new execution spool must be empty");
    };
    let (fresh, output) = empty.into_parts();
    let mut spool = output.into_test_spool();
    let frame = terminal_frame(&fresh);
    let sequence = frame.sequence;
    spool.persist(frame).expect("durable terminal");
    drop(spool);

    let outcome = preflight.prepare(fresh).await.expect("pending preflight");
    let AgentOutputPreflightOutcome::TerminalRecovery(pending) = outcome else {
        panic!("durable terminal must never become fresh work");
    };
    assert_eq!(pending.sequence(), sequence);
}

#[tokio::test]
async fn terminal_reopener_rejects_changed_bytes_between_attempts() {
    let (_temporary, root) = root();
    let preflight = preflight(root, "worker-1");
    let empty = preflight.prepare(fresh()).await.expect("empty preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = empty else {
        panic!("new execution spool must be empty");
    };
    let (fresh, output) = empty.into_parts();
    let mut spool = output.into_test_spool();
    let frame = terminal_frame(&fresh);
    spool
        .persist(frame.clone())
        .expect("durable terminal output");
    drop(spool);

    let recovery = preflight.prepare(fresh).await.expect("terminal recovery");
    let AgentOutputPreflightOutcome::TerminalRecovery(recovery) = recovery else {
        panic!("durable terminal must route to recovery");
    };
    let (_delivery, _verified, _claim, mut spool, expected, reopener) = recovery.into_parts();
    let mut replacement = expected.clone();
    replacement.occurred_at_unix_millis += 1;
    spool
        .replace_pending_exact(&expected, &replacement)
        .expect("canonical same-execution replacement");
    drop(spool);

    let Err(error) = reopener.reopen().await else {
        panic!("reopen must retain the exact classified terminal proof");
    };
    assert!(matches!(
        error,
        AgentOutputPreflightError::InvalidDurableState(_)
    ));
}

#[tokio::test]
async fn restored_terminal_payload_digest_is_revalidated_before_recovery() {
    let (_temporary, root) = root();
    let preflight = preflight(root, "worker-1");
    let empty = preflight.prepare(fresh()).await.expect("empty preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = empty else {
        panic!("new execution spool must be empty");
    };
    let (fresh, output) = empty.into_parts();
    let mut spool = output.into_test_spool();
    let mut frame = terminal_frame(&fresh);
    frame
        .payload_digest
        .as_mut()
        .expect("terminal payload digest")
        .value[0] ^= 1;
    spool.persist(frame).expect("durable malformed terminal");
    drop(spool);

    let Err(error) = preflight.prepare(fresh).await else {
        panic!("a malformed terminal must not mint recovery authority");
    };
    assert!(matches!(
        error,
        AgentOutputPreflightError::InvalidDurableState(_)
    ));
    assert_eq!(error.code(), "agent_output.invalid_durable_state");
    assert!(!error.retryable());
}

#[tokio::test]
async fn restored_terminal_must_match_the_admitted_request_binding() {
    let (_temporary, root) = root();
    let preflight = preflight(root, "worker-1");
    let empty = preflight.prepare(fresh()).await.expect("empty preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = empty else {
        panic!("new execution spool must be empty");
    };
    let (fresh, output) = empty.into_parts();
    let mut spool = output.into_test_spool();
    let mut frame = terminal_frame(&fresh);
    let Some(execution_output_frame_v1::Payload::AgentExecution(result)) = frame.payload.as_mut()
    else {
        panic!("terminal agent result");
    };
    result.request_immutable_version = "different-request-version".to_owned();
    let payload_digest = DigestV1 {
        algorithm: DigestAlgorithmV1::Sha256 as i32,
        value: digest::digest(&digest::SHA256, &result.encode_to_vec())
            .as_ref()
            .to_vec(),
    };
    frame.payload_digest = Some(payload_digest.clone());
    frame
        .settlement_proposal
        .as_mut()
        .expect("terminal settlement proposal")
        .terminal_payload_digest = Some(payload_digest);
    spool.persist(frame).expect("durable substituted terminal");
    drop(spool);

    let Err(error) = preflight.prepare(fresh).await else {
        panic!("a terminal for another admitted request must fail closed");
    };
    assert!(matches!(
        error,
        AgentOutputPreflightError::InvalidDurableState(_)
    ));
}

#[tokio::test]
async fn pending_frame_from_another_fence_fails_closed() {
    let (_temporary, root) = root();
    let preflight = preflight(root, "worker-1");
    let empty = preflight.prepare(fresh()).await.expect("empty preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = empty else {
        panic!("new execution spool must be empty");
    };
    let (fresh, output) = empty.into_parts();
    let mut spool = output.into_test_spool();
    let mut fence = claim_response()
        .receipt
        .expect("claim receipt")
        .fence
        .expect("claim fence");
    fence.fence_token[0] ^= 1;
    let sequence = fresh.claim_handoff_watermark() + 1;
    spool
        .persist(progress_frame(
            &fresh,
            fence,
            sequence,
            fresh.claim_handoff_watermark(),
        ))
        .expect("durable foreign-fence frame");
    drop(spool);

    let Err(error) = preflight.prepare(fresh).await else {
        panic!("foreign-fence output must fail closed");
    };
    assert!(matches!(
        error,
        AgentOutputPreflightError::InvalidDurableState(_)
    ));
    assert_eq!(error.code(), "agent_output.invalid_durable_state");
    assert!(!error.retryable());
}

#[tokio::test]
async fn pre_invocation_failure_polls_persists_replays_settles_and_retires_in_order() {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (_temporary, control, terminal, admission) =
        pre_invocation_terminal_fixture(Arc::clone(&trace)).await;
    let replay = FakeReplay::new(
        [ReplayResult::Unavailable, ReplayResult::Acknowledged],
        Arc::clone(&trace),
    );
    let retirer = recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    );

    let completion = publish_pre_invocation_terminal(
        control,
        &retirer,
        &replay,
        terminal,
        Arc::new(|| NOW),
        recovery_config(2),
    )
    .await
    .expect("failure terminal completion");

    assert_eq!(completion.execution_kind(), AgentExecutionKind::Application);
    assert_eq!(completion.sequence(), 5);
    assert_eq!(completion.failure(), RuntimeFailureKind::InvalidInput);
    assert_eq!(completion.settlement_receipt_id(), "settlement-receipt-1");
    assert_eq!(admission.available_capacity(), 1);
    let frames = replay.frames.lock().expect("frames");
    assert_eq!(frames.len(), 2);
    assert_eq!(frames[0], frames[1]);
    assert_eq!(frames[0].claim_handoff_watermark, 4);
    assert_eq!(frames[0].occurred_at_unix_millis, NOW);
    let Some(execution_output_frame_v1::Payload::RuntimeError(error)) = frames[0].payload.as_ref()
    else {
        panic!("canonical invalid-input terminal");
    };
    assert_eq!(error.code, RuntimeErrorCodeV1::InvalidInput as i32);
    assert_eq!(
        *trace.lock().expect("trace"),
        [
            "begin",
            "renew",
            "observe",
            "input",
            "renew",
            "observe",
            "renew",
            "observe",
            "replay",
            "replay",
            "settlement",
            "redis",
        ]
    );
}

#[tokio::test]
async fn already_authorized_drops_request_authority_and_publishes_one_bound_internal_terminal() {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (temporary, root) = root();
    let preflight = preflight(root, "worker-1");
    let outcome = preflight.prepare(fresh()).await.expect("empty preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = outcome else {
        panic!("new execution spool must be empty");
    };
    let control = authorization_control(Arc::clone(&trace));
    let admission = InvocationAdmission::new(
        InvocationAdmissionConfig::new(1, Duration::from_secs(1)).expect("invocation admission"),
    );
    let supervisor = InvocationSupervisor::new(admission.clone());
    let prepared = prepare_fresh_agent_invocation_with(
        *empty,
        Arc::clone(&control),
        &admission,
        &ValidAgentInput {
            trace: Arc::clone(&trace),
        },
        Arc::new(|| NOW),
        AgentPreparationConfig::new(Duration::from_secs(10)).expect("preparation config"),
    )
    .await
    .expect("prepared invocation");
    let AgentPreparationOutcome::Prepared(prepared) = prepared else {
        panic!("valid application input must reach authorization");
    };
    let replay = Arc::new(FakeReplay::new(
        [ReplayResult::Acknowledged],
        Arc::clone(&trace),
    ));
    let retirer = Arc::new(recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    ));
    let lifecycle = Arc::new(GatedAuthorizedLifecycle {
        trace: Arc::clone(&trace),
        started: Mutex::new(None),
        release: Arc::new(Semaphore::new(1)),
    });
    let (reservation, job) = AgentAuthorizationJob::new(
        *prepared,
        control,
        retirer,
        Arc::clone(&replay),
        Arc::new(|| NOW),
        recovery_config(1),
        lifecycle,
    );
    let invocation = match supervisor.submit(reservation, job) {
        Ok(invocation) => invocation,
        Err(rejected) => panic!("authorization supervision failed: {}", rejected.error()),
    };
    let completion = invocation.wait().await.expect("authorization job result");
    let AgentAuthorizationJobCompletion::Terminal(Ok(completion)) = completion else {
        panic!("the scripted response must publish a terminal without running ADK");
    };
    supervisor.close().await.expect("supervisor drain");

    assert_eq!(completion.execution_kind(), AgentExecutionKind::Application);
    assert_eq!(completion.sequence(), 5);
    assert_eq!(completion.failure(), RuntimeFailureKind::Internal);
    assert_eq!(completion.settlement_receipt_id(), "settlement-receipt-1");
    assert_eq!(admission.available_capacity(), 1);
    assert_eq!(replay.frames.lock().expect("frames").len(), 1);
    assert_eq!(
        *trace.lock().expect("trace"),
        [
            "begin",
            "renew",
            "observe",
            "input",
            "authorize",
            "renew",
            "observe",
            "replay",
            "settlement",
            "redis",
        ]
    );
    drop(temporary);
}

#[tokio::test]
async fn dropped_authorization_waiter_cannot_cancel_the_owned_authorized_lifecycle() {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (temporary, root) = root();
    let preflight = preflight(root, "worker-1");
    let outcome = preflight.prepare(fresh()).await.expect("empty preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = outcome else {
        panic!("new execution spool must be empty");
    };
    let control = authorized_control(Arc::clone(&trace));
    let admission = InvocationAdmission::new(
        InvocationAdmissionConfig::new(1, Duration::from_secs(1)).expect("invocation admission"),
    );
    let supervisor = Arc::new(InvocationSupervisor::new(admission.clone()));
    let prepared = prepare_fresh_agent_invocation_with(
        *empty,
        Arc::clone(&control),
        &admission,
        &ValidAgentInput {
            trace: Arc::clone(&trace),
        },
        Arc::new(|| NOW),
        AgentPreparationConfig::new(Duration::from_secs(10)).expect("preparation config"),
    )
    .await
    .expect("prepared invocation");
    let AgentPreparationOutcome::Prepared(prepared) = prepared else {
        panic!("valid application input must reach authorization");
    };
    let (started_sender, started_receiver) = oneshot::channel();
    let release = Arc::new(Semaphore::new(0));
    let lifecycle = Arc::new(GatedAuthorizedLifecycle {
        trace: Arc::clone(&trace),
        started: Mutex::new(Some(started_sender)),
        release: Arc::clone(&release),
    });
    let replay = Arc::new(FakeReplay::new(
        [ReplayResult::Acknowledged],
        Arc::clone(&trace),
    ));
    let retirer = Arc::new(recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    ));
    let (reservation, job) = AgentAuthorizationJob::new(
        *prepared,
        control,
        retirer,
        replay,
        Arc::new(|| NOW),
        recovery_config(1),
        lifecycle,
    );
    let invocation = match supervisor.submit(reservation, job) {
        Ok(invocation) => invocation,
        Err(rejected) => panic!("authorization supervision failed: {}", rejected.error()),
    };
    started_receiver.await.expect("authorized lifecycle start");
    assert_eq!(supervisor.active_count(), 1);
    assert_eq!(admission.available_capacity(), 0);

    drop(invocation);
    let closing = Arc::clone(&supervisor);
    let close = tokio::spawn(async move { closing.close().await });
    tokio::task::yield_now().await;
    assert!(!close.is_finished());
    assert_eq!(supervisor.active_count(), 1);
    assert_eq!(admission.available_capacity(), 0);

    release.add_permits(1);
    close.await.expect("close task").expect("supervisor drain");
    assert_eq!(supervisor.active_count(), 0);
    assert_eq!(admission.available_capacity(), 1);
    assert_eq!(
        *trace.lock().expect("trace"),
        [
            "begin",
            "renew",
            "observe",
            "input",
            "authorize",
            "authorized",
        ]
    );
    drop(temporary);
}

#[tokio::test]
async fn application_and_adhoc_share_native_events_terminal_settlement_and_redis_retirement() {
    for kind in [AgentExecutionKind::Application, AgentExecutionKind::Adhoc] {
        Box::pin(run_native_lifecycle_case(kind)).await;
    }
}

#[allow(clippy::too_many_lines)] // One end-to-end trace is clearer than split authority fixtures.
async fn run_native_lifecycle_case(kind: AgentExecutionKind) {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (temporary, output_root) = root();
    let outcome = preflight(output_root, "worker-1")
        .prepare(fresh_for_kind(kind))
        .await
        .expect("empty native lifecycle preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = outcome else {
        panic!("new native lifecycle spool must be empty");
    };
    let control = authorized_control(Arc::clone(&trace));
    let admission = InvocationAdmission::new(
        InvocationAdmissionConfig::new(1, Duration::from_secs(1)).expect("invocation admission"),
    );
    let prepared = prepare_fresh_agent_invocation_with(
        *empty,
        Arc::clone(&control),
        &admission,
        &KindAgentInput {
            trace: Arc::clone(&trace),
            kind,
        },
        Arc::new(|| NOW),
        AgentPreparationConfig::new(Duration::from_secs(10)).expect("preparation config"),
    )
    .await
    .expect("prepared native invocation");
    let AgentPreparationOutcome::Prepared(prepared) = prepared else {
        panic!("valid native input must reach authorization");
    };
    let progress_state =
        FakeProgressState::new(std::iter::repeat_n(LiveProgressAction::Acknowledge, 8), []);
    let connector = FakeProgressConnector {
        state: Arc::clone(&progress_state),
    };
    let retirer = Arc::new(recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    ));
    let coordinator = native_agent_coordinator(
        admission.clone(),
        Arc::new(TestNativeAssembler {
            trace: Arc::clone(&trace),
            agent: Arc::new(ImmediateTextAgent),
            sensitive: false,
        }),
        connector.clone(),
        Arc::clone(&control),
        Arc::clone(&retirer),
        Arc::new(|| NOW + 2),
        1,
        recovery_config(1),
    );
    let invocation = match coordinator.submit(*prepared) {
        Ok(invocation) => invocation,
        Err(rejected) => panic!("native coordination failed: {}", rejected.error()),
    };
    let completion = invocation.wait().await.expect("native lifecycle result");
    let AgentAuthorizationJobCompletion::Authorized(completion) = completion else {
        panic!("the native lifecycle must own the authorized outcome");
    };
    coordinator.close().await.expect("native lifecycle drain");

    assert_eq!(completion.execution_kind(), kind);
    let AgentAuthorizedLifecycleDisposition::ExecutedSettledAcked {
        sequence,
        settlement_receipt_id,
    } = completion.disposition()
    else {
        panic!("the native lifecycle must settle and retire the command");
    };
    assert_eq!(*sequence, 13);
    assert_eq!(settlement_receipt_id, "settlement-receipt-1");
    assert_eq!(admission.available_capacity(), 1);

    let frames = progress_state
        .frames
        .lock()
        .expect("native lifecycle frames");
    assert_eq!(frames.len(), 9);
    let event_types: Vec<_> = frames
        .iter()
        .filter(|frame| !frame.terminal)
        .map(|frame| match frame.payload.as_ref() {
            Some(execution_output_frame_v1::Payload::NodeEvent(event)) => event.r#type.as_str(),
            _ => panic!("progress frame must carry a NodeEvent"),
        })
        .collect();
    assert_eq!(
        event_types,
        [
            "agent_start",
            "agent_llm_start",
            "agent_llm_chunk",
            "agent_llm_end",
            "partial_message",
            "pipeline_finish",
            "agent_response",
            "full_message",
        ]
    );
    assert_eq!(frames[7].sequence + 1, frames[8].sequence);
    assert!(frames[8].terminal);
    assert!(matches!(
        frames[8].payload,
        Some(execution_output_frame_v1::Payload::AgentExecution(_))
    ));
    drop(frames);

    let expected_assembly = match kind {
        AgentExecutionKind::Application => "assemble_application",
        AgentExecutionKind::Adhoc => "assemble_adhoc",
    };
    assert_eq!(
        *trace.lock().expect("trace"),
        [
            "begin",
            "renew",
            "observe",
            "input",
            "authorize",
            expected_assembly,
            "renew",
            "observe",
            "settlement",
            "redis",
        ]
    );
    drop(temporary);
}

#[tokio::test]
async fn redis_delivery_processor_owns_both_agent_kinds_through_retirement() {
    for kind in [AgentExecutionKind::Application, AgentExecutionKind::Adhoc] {
        Box::pin(run_delivery_processor_case(kind)).await;
    }
}

#[tokio::test]
async fn ambiguous_output_recovery_terminalizes_without_reentering_business_execution() {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (temporary, output_root) = root();
    let control = ambiguous_output_recovery_control(Arc::clone(&trace));
    let admission = InvocationAdmission::new(
        InvocationAdmissionConfig::new(1, Duration::from_secs(1)).expect("invocation admission"),
    );
    let progress_state = FakeProgressState::new([], []);
    let connector = FakeProgressConnector {
        state: Arc::clone(&progress_state),
    };
    let retirer = Arc::new(recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    ));
    let processor = native_agent_delivery_processor(
        Arc::new(TestOnlyConformanceHmacAuthenticator),
        preflight(output_root, "worker-1"),
        control,
        retirer,
        Arc::new(KindAgentInput {
            trace: Arc::clone(&trace),
            kind: AgentExecutionKind::Application,
        }),
        Arc::new(|| NOW),
        admission.clone(),
        AgentPreparationConfig::new(Duration::from_secs(10)).expect("preparation config"),
        Arc::new(TestNativeAssembler {
            trace: Arc::clone(&trace),
            agent: Arc::new(ImmediateTextAgent),
            sensitive: false,
        }),
        connector,
        1,
        recovery_config(1),
    );

    processor
        .process(redis_delivery(signed_command_for_kind(
            AgentExecutionKind::Application,
        )))
        .await;
    processor.close().await.expect("delivery processor drain");

    assert_eq!(
        *trace.lock().expect("trace"),
        ["claim", "renew", "observe", "settlement", "redis"]
    );
    assert_eq!(admission.available_capacity(), 1);
    let frames = progress_state.frames.lock().expect("frames");
    assert_eq!(frames.len(), 1);
    assert!(frames[0].terminal);
    assert_eq!(frames[0].sequence, 5);
    assert_eq!(frames[0].claim_handoff_watermark, 4);
    let Some(execution_output_frame_v1::Payload::RuntimeError(error)) = frames[0].payload.as_ref()
    else {
        panic!("ambiguous recovery must publish one safe runtime terminal");
    };
    assert_eq!(error.code, RuntimeErrorCodeV1::Internal as i32);
    drop(frames);
    drop(processor);
    drop(temporary);
}

async fn run_delivery_processor_case(kind: AgentExecutionKind) {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (temporary, output_root) = root();
    let control = authorized_control(Arc::clone(&trace));
    let admission = InvocationAdmission::new(
        InvocationAdmissionConfig::new(1, Duration::from_secs(1)).expect("invocation admission"),
    );
    let progress_state =
        FakeProgressState::new(std::iter::repeat_n(LiveProgressAction::Acknowledge, 8), []);
    let connector = FakeProgressConnector {
        state: Arc::clone(&progress_state),
    };
    let retirer = Arc::new(recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    ));
    let processor = native_agent_delivery_processor(
        Arc::new(TestOnlyConformanceHmacAuthenticator),
        preflight(output_root, "worker-1"),
        control,
        retirer,
        Arc::new(KindAgentInput {
            trace: Arc::clone(&trace),
            kind,
        }),
        Arc::new(|| NOW),
        admission.clone(),
        AgentPreparationConfig::new(Duration::from_secs(10)).expect("preparation config"),
        Arc::new(TestNativeAssembler {
            trace: Arc::clone(&trace),
            agent: Arc::new(ImmediateTextAgent),
            sensitive: false,
        }),
        connector,
        1,
        recovery_config(1),
    );

    processor
        .process(redis_delivery(signed_command_for_kind(kind)))
        .await;
    processor.close().await.expect("delivery processor drain");

    let expected_assembly = match kind {
        AgentExecutionKind::Application => "assemble_application",
        AgentExecutionKind::Adhoc => "assemble_adhoc",
    };
    assert_eq!(
        *trace.lock().expect("trace"),
        [
            "claim",
            "begin",
            "renew",
            "observe",
            "input",
            "authorize",
            expected_assembly,
            "renew",
            "observe",
            "settlement",
            "redis",
        ]
    );
    assert_eq!(progress_state.frames.lock().expect("frames").len(), 9);
    assert_eq!(admission.available_capacity(), 1);
    drop(processor);
    drop(temporary);
}

#[tokio::test]
async fn stopped_delivery_processor_closes_prepared_work_without_authorize_or_ack() {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (temporary, output_root) = root();
    let control = authorized_control(Arc::clone(&trace));
    let admission = InvocationAdmission::new(
        InvocationAdmissionConfig::new(1, Duration::from_secs(1)).expect("invocation admission"),
    );
    let connector = FakeProgressConnector {
        state: FakeProgressState::new([], []),
    };
    let retirer = Arc::new(recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    ));
    let processor = native_agent_delivery_processor(
        Arc::new(TestOnlyConformanceHmacAuthenticator),
        preflight(output_root, "worker-1"),
        control,
        retirer,
        Arc::new(KindAgentInput {
            trace: Arc::clone(&trace),
            kind: AgentExecutionKind::Application,
        }),
        Arc::new(|| NOW),
        admission.clone(),
        AgentPreparationConfig::new(Duration::from_secs(10)).expect("preparation config"),
        Arc::new(TestNativeAssembler {
            trace: Arc::clone(&trace),
            agent: Arc::new(ImmediateTextAgent),
            sensitive: false,
        }),
        connector,
        1,
        recovery_config(1),
    );
    processor.stop().expect("stop delivery processor");

    processor
        .process(redis_delivery(signed_command_for_kind(
            AgentExecutionKind::Application,
        )))
        .await;
    processor.close().await.expect("stopped processor drain");

    let trace = trace.lock().expect("trace");
    assert_eq!(trace.as_slice(), ["claim"]);
    assert!(!trace.contains(&"begin"));
    assert!(!trace.contains(&"input"));
    assert!(!trace.contains(&"authorize"));
    assert!(!trace.contains(&"redis"));
    assert_eq!(admission.available_capacity(), 1);
    drop(trace);
    drop(processor);
    drop(temporary);
}

#[tokio::test]
async fn stopped_native_coordinator_returns_an_explicitly_closeable_unstarted_job() {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (temporary, output_root) = root();
    let outcome = preflight(output_root, "worker-1")
        .prepare(fresh_for_kind(AgentExecutionKind::Application))
        .await
        .expect("empty native coordinator preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = outcome else {
        panic!("new coordinator spool must be empty");
    };
    let control = authorized_control(Arc::clone(&trace));
    let admission = InvocationAdmission::new(
        InvocationAdmissionConfig::new(1, Duration::from_secs(1)).expect("invocation admission"),
    );
    let prepared = prepare_fresh_agent_invocation_with(
        *empty,
        Arc::clone(&control),
        &admission,
        &KindAgentInput {
            trace: Arc::clone(&trace),
            kind: AgentExecutionKind::Application,
        },
        Arc::new(|| NOW),
        AgentPreparationConfig::new(Duration::from_secs(10)).expect("preparation config"),
    )
    .await
    .expect("prepared native invocation");
    let AgentPreparationOutcome::Prepared(prepared) = prepared else {
        panic!("valid input must reach native coordination");
    };
    let connector = FakeProgressConnector {
        state: FakeProgressState::new([], []),
    };
    let retirer = Arc::new(recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    ));
    let coordinator = native_agent_coordinator(
        admission.clone(),
        Arc::new(TestNativeAssembler {
            trace: Arc::clone(&trace),
            agent: Arc::new(ImmediateTextAgent),
            sensitive: false,
        }),
        connector,
        control,
        retirer,
        Arc::new(|| NOW + 2),
        1,
        recovery_config(1),
    );
    coordinator.stop().expect("stop native coordinator");

    let Err(rejected) = coordinator.submit(*prepared) else {
        panic!("stopped coordinator accepted an invocation");
    };
    assert_eq!(rejected.error().code(), "invocation_supervision.closed");
    assert!(
        rejected
            .close()
            .await
            .expect("close rejected invocation")
            .is_none()
    );
    coordinator
        .close()
        .await
        .expect("drain stopped coordinator");

    assert_eq!(coordinator.active_count(), 0);
    assert_eq!(admission.available_capacity(), 1);
    assert!(!trace.lock().expect("trace").contains(&"authorize"));
    drop(temporary);
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One end-to-end pause proof keeps authority ordering explicit.
async fn sensitive_interrupt_is_the_acked_paused_hitl_terminal_and_skips_completion() {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (temporary, output_root) = root();
    let outcome = preflight(output_root, "worker-1")
        .prepare(fresh())
        .await
        .expect("empty HITL lifecycle preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = outcome else {
        panic!("new HITL lifecycle spool must be empty");
    };
    let control = authorized_control(Arc::clone(&trace));
    let admission = InvocationAdmission::new(
        InvocationAdmissionConfig::new(1, Duration::from_secs(1)).expect("invocation admission"),
    );
    let supervisor = InvocationSupervisor::new(admission.clone());
    let prepared = prepare_fresh_agent_invocation_with(
        *empty,
        Arc::clone(&control),
        &admission,
        &KindAgentInput {
            trace: Arc::clone(&trace),
            kind: AgentExecutionKind::Application,
        },
        Arc::new(|| NOW),
        AgentPreparationConfig::new(Duration::from_secs(10)).expect("preparation config"),
    )
    .await
    .expect("prepared HITL invocation");
    let AgentPreparationOutcome::Prepared(prepared) = prepared else {
        panic!("valid HITL input must reach authorization");
    };
    let progress_state =
        FakeProgressState::new(std::iter::repeat_n(LiveProgressAction::Acknowledge, 8), []);
    let connector = FakeProgressConnector {
        state: Arc::clone(&progress_state),
    };
    let retirer = Arc::new(recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    ));
    let lifecycle = Arc::new(NativeAuthorizedAgentLifecycle::new(
        Arc::new(TestNativeAssembler {
            trace: Arc::clone(&trace),
            agent: Arc::new(SensitiveInterruptAgent),
            sensitive: true,
        }),
        connector.clone(),
        Arc::clone(&control),
        Arc::clone(&retirer),
        Arc::new(|| NOW + 3),
        1,
        recovery_config(1),
    ));
    let (reservation, job) = AgentAuthorizationJob::new(
        *prepared,
        control,
        retirer,
        Arc::new(connector),
        Arc::new(|| NOW + 3),
        recovery_config(1),
        lifecycle,
    );
    let invocation = match supervisor.submit(reservation, job) {
        Ok(invocation) => invocation,
        Err(rejected) => panic!("HITL lifecycle supervision failed: {}", rejected.error()),
    };
    let completion = invocation.wait().await.expect("HITL lifecycle result");
    let AgentAuthorizationJobCompletion::Authorized(completion) = completion else {
        panic!("the HITL lifecycle must own the authorized outcome");
    };
    supervisor.close().await.expect("HITL lifecycle drain");

    assert!(matches!(
        completion.disposition(),
        AgentAuthorizedLifecycleDisposition::ExecutedSettledAcked { sequence: 13, .. }
    ));
    let frames = progress_state.frames.lock().expect("HITL lifecycle frames");
    assert_eq!(frames.len(), 9);
    let event_types = frames[..8]
        .iter()
        .map(|frame| match frame.payload.as_ref() {
            Some(execution_output_frame_v1::Payload::NodeEvent(event)) => event.r#type.as_str(),
            _ => panic!("HITL progress frame must carry a NodeEvent"),
        })
        .collect::<Vec<_>>();
    assert_eq!(
        event_types,
        [
            "agent_start",
            "agent_llm_start",
            "agent_llm_end",
            "partial_message",
            "agent_tool_start",
            "partial_message",
            "agent_hitl_interrupt",
            "agent_hitl_interrupt",
        ]
    );
    let Some(execution_output_frame_v1::Payload::NodeEvent(interrupt)) = frames[7].payload.as_ref()
    else {
        panic!("aggregate HITL result frame must carry the interrupt");
    };
    let metadata: serde_json::Value =
        serde_json::from_slice(&interrupt.response_metadata).expect("HITL response metadata");
    assert_eq!(
        metadata["hitl_interrupts"][0]["tool_args"]["api_token"],
        "***"
    );
    let encoded_interrupt = crate::protocol::node_event::encode_current_node_event_json(interrupt)
        .expect("canonical HITL interrupt");
    assert!(
        !encoded_interrupt
            .windows(b"never-publish".len())
            .any(|window| window == b"never-publish")
    );
    let Some(execution_output_frame_v1::Payload::AgentExecution(result)) =
        frames[8].payload.as_ref()
    else {
        panic!("HITL terminal must carry the bound agent result");
    };
    assert_eq!(
        AgentExecutionTerminalStateV1::try_from(result.terminal_state),
        Ok(AgentExecutionTerminalStateV1::PausedHitl)
    );
    assert!(
        result
            .result_artifact
            .as_ref()
            .is_some_and(|artifact| artifact.artifact_id.ends_with(":hitl-interrupt"))
    );
    assert_eq!(admission.available_capacity(), 1);
    drop(frames);
    drop(temporary);
}

#[tokio::test(start_paused = true)]
async fn durable_stop_interrupts_only_the_owned_run_then_settles_cancelled() {
    Box::pin(run_durable_stop_case()).await;
}

#[allow(clippy::too_many_lines)] // Keep the Stop ownership trace in one fixture.
async fn run_durable_stop_case() {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (temporary, output_root) = root();
    let outcome = preflight(output_root, "worker-1")
        .prepare(fresh())
        .await
        .expect("empty Stop lifecycle preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = outcome else {
        panic!("new Stop lifecycle spool must be empty");
    };
    let control = authorized_control_with_policy(
        Arc::clone(&trace),
        [
            DesiredExecutionStateV1::Running,
            DesiredExecutionStateV1::Cancelled,
        ],
    );
    let admission = InvocationAdmission::new(
        InvocationAdmissionConfig::new(1, Duration::from_secs(1)).expect("invocation admission"),
    );
    let supervisor = InvocationSupervisor::new(admission.clone());
    let prepared = prepare_fresh_agent_invocation_with(
        *empty,
        Arc::clone(&control),
        &admission,
        &ValidAgentInput {
            trace: Arc::clone(&trace),
        },
        Arc::new(|| NOW),
        AgentPreparationConfig::new(Duration::from_secs(10)).expect("preparation config"),
    )
    .await
    .expect("prepared Stop invocation");
    let AgentPreparationOutcome::Prepared(prepared) = prepared else {
        panic!("valid Stop input must reach authorization");
    };
    let progress_state = FakeProgressState::new([LiveProgressAction::Acknowledge], []);
    let connector = FakeProgressConnector {
        state: Arc::clone(&progress_state),
    };
    let retirer = Arc::new(recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    ));
    let started = Arc::new(Notify::new());
    let release = Arc::new(Semaphore::new(0));
    let lifecycle = Arc::new(NativeAuthorizedAgentLifecycle::new(
        Arc::new(TestNativeAssembler {
            trace: Arc::clone(&trace),
            agent: Arc::new(GatedTextAgent {
                started: Arc::clone(&started),
                release: Arc::clone(&release),
            }),
            sensitive: false,
        }),
        connector.clone(),
        Arc::clone(&control),
        Arc::clone(&retirer),
        Arc::new(|| NOW + 2),
        1,
        recovery_config(1),
    ));
    let (reservation, job) = AgentAuthorizationJob::new(
        *prepared,
        control,
        retirer,
        Arc::new(connector),
        Arc::new(|| NOW + 2),
        recovery_config(1),
        lifecycle,
    );
    let invocation = match supervisor.submit(reservation, job) {
        Ok(invocation) => invocation,
        Err(rejected) => panic!("Stop lifecycle supervision failed: {}", rejected.error()),
    };
    started.notified().await;
    tokio::time::advance(Duration::from_secs(10)).await;
    tokio::task::yield_now().await;
    let completion = invocation.wait().await.expect("Stop lifecycle result");
    release.add_permits(1);
    let AgentAuthorizationJobCompletion::Authorized(completion) = completion else {
        panic!("the Stop lifecycle must own the authorized outcome");
    };
    supervisor.close().await.expect("Stop lifecycle drain");

    assert!(matches!(
        completion.disposition(),
        AgentAuthorizedLifecycleDisposition::ExecutedSettledAcked { sequence: 6, .. }
    ));
    let frames = progress_state.frames.lock().expect("Stop lifecycle frames");
    assert_eq!(frames.len(), 2);
    assert_eq!(
        restored_terminal_failure_kind(&frames[1]).expect("cancelled terminal"),
        RuntimeFailureKind::Cancelled
    );
    assert_eq!(admission.available_capacity(), 1);
    assert!(
        trace
            .lock()
            .expect("trace")
            .ends_with(&["settlement", "redis"])
    );
    drop(temporary);
}

#[tokio::test(start_paused = true)]
async fn durable_stop_cancels_stalled_post_authorization_assembly_before_runner_start() {
    Box::pin(run_stop_during_assembly_case()).await;
}

#[allow(clippy::too_many_lines)] // Keep the cancellation-safe assembly proof in one trace.
async fn run_stop_during_assembly_case() {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (temporary, output_root) = root();
    let outcome = preflight(output_root, "worker-1")
        .prepare(fresh())
        .await
        .expect("empty assembly lifecycle preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = outcome else {
        panic!("new assembly lifecycle spool must be empty");
    };
    let control = authorized_control_with_policy(
        Arc::clone(&trace),
        [
            DesiredExecutionStateV1::Running,
            DesiredExecutionStateV1::Cancelled,
        ],
    );
    let admission = InvocationAdmission::new(
        InvocationAdmissionConfig::new(1, Duration::from_secs(1))
            .expect("assembly invocation admission"),
    );
    let supervisor = InvocationSupervisor::new(admission.clone());
    let prepared = prepare_fresh_agent_invocation_with(
        *empty,
        Arc::clone(&control),
        &admission,
        &ValidAgentInput {
            trace: Arc::clone(&trace),
        },
        Arc::new(|| NOW),
        AgentPreparationConfig::new(Duration::from_secs(10)).expect("assembly preparation config"),
    )
    .await
    .expect("prepared assembly invocation");
    let AgentPreparationOutcome::Prepared(prepared) = prepared else {
        panic!("valid assembly input must reach authorization");
    };
    let progress_state = FakeProgressState::new([LiveProgressAction::Acknowledge], []);
    let connector = FakeProgressConnector {
        state: Arc::clone(&progress_state),
    };
    let retirer = Arc::new(recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    ));
    let assembly_started = Arc::new(Notify::new());
    let lifecycle = Arc::new(NativeAuthorizedAgentLifecycle::new(
        Arc::new(GatedNativeAssembler {
            started: Arc::clone(&assembly_started),
        }),
        connector.clone(),
        Arc::clone(&control),
        Arc::clone(&retirer),
        Arc::new(|| NOW + 2),
        1,
        recovery_config(1),
    ));
    let (reservation, job) = AgentAuthorizationJob::new(
        *prepared,
        control,
        retirer,
        Arc::new(connector),
        Arc::new(|| NOW + 2),
        recovery_config(1),
        lifecycle,
    );
    let invocation = match supervisor.submit(reservation, job) {
        Ok(invocation) => invocation,
        Err(rejected) => panic!(
            "assembly lifecycle supervision failed: {}",
            rejected.error()
        ),
    };
    assembly_started.notified().await;
    tokio::time::advance(Duration::from_secs(10)).await;
    tokio::task::yield_now().await;
    let completion = invocation.wait().await.expect("assembly lifecycle result");
    let AgentAuthorizationJobCompletion::Authorized(completion) = completion else {
        panic!("the assembly lifecycle must own the authorized outcome");
    };
    supervisor.close().await.expect("assembly lifecycle drain");

    assert!(matches!(
        completion.disposition(),
        AgentAuthorizedLifecycleDisposition::ExecutedSettledAcked { sequence: 5, .. }
    ));
    let frames = progress_state
        .frames
        .lock()
        .expect("assembly lifecycle frames");
    assert_eq!(frames.len(), 1);
    assert_eq!(
        restored_terminal_failure_kind(&frames[0]).expect("assembly cancelled terminal"),
        RuntimeFailureKind::Cancelled
    );
    assert_eq!(admission.available_capacity(), 1);
    assert!(
        trace
            .lock()
            .expect("trace")
            .ends_with(&["settlement", "redis"])
    );
    drop(temporary);
}

#[tokio::test]
async fn deadline_during_native_run_preserves_completion_events_but_replaces_terminal_success() {
    Box::pin(run_deadline_during_native_case()).await;
}

#[allow(clippy::too_many_lines)] // Keep the non-cancellable deadline trace in one fixture.
async fn run_deadline_during_native_case() {
    const DEADLINE: i64 = NOW + 100_000;

    let trace = Arc::new(Mutex::new(Vec::new()));
    let (temporary, output_root) = root();
    let outcome = preflight(output_root, "worker-1")
        .prepare(fresh())
        .await
        .expect("empty deadline lifecycle preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = outcome else {
        panic!("new deadline lifecycle spool must be empty");
    };
    let control = authorized_control(Arc::clone(&trace));
    let admission = InvocationAdmission::new(
        InvocationAdmissionConfig::new(1, Duration::from_secs(1)).expect("invocation admission"),
    );
    let supervisor = InvocationSupervisor::new(admission.clone());
    let clock = Arc::new(AtomicI64::new(NOW));
    let preparation_clock = Arc::clone(&clock);
    let prepared = prepare_fresh_agent_invocation_with(
        *empty,
        Arc::clone(&control),
        &admission,
        &ValidAgentInput {
            trace: Arc::clone(&trace),
        },
        Arc::new(move || preparation_clock.load(Ordering::SeqCst)),
        AgentPreparationConfig::new(Duration::from_secs(10)).expect("preparation config"),
    )
    .await
    .expect("prepared deadline invocation");
    let AgentPreparationOutcome::Prepared(prepared) = prepared else {
        panic!("valid deadline input must reach authorization");
    };
    let progress_state =
        FakeProgressState::new(std::iter::repeat_n(LiveProgressAction::Acknowledge, 8), []);
    let connector = FakeProgressConnector {
        state: Arc::clone(&progress_state),
    };
    let retirer = Arc::new(recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    ));
    let started = Arc::new(Notify::new());
    let release = Arc::new(Semaphore::new(0));
    let lifecycle_clock = Arc::clone(&clock);
    let lifecycle = Arc::new(NativeAuthorizedAgentLifecycle::new(
        Arc::new(TestNativeAssembler {
            trace: Arc::clone(&trace),
            agent: Arc::new(GatedTextAgent {
                started: Arc::clone(&started),
                release: Arc::clone(&release),
            }),
            sensitive: false,
        }),
        connector.clone(),
        Arc::clone(&control),
        Arc::clone(&retirer),
        Arc::new(move || lifecycle_clock.load(Ordering::SeqCst)),
        1,
        recovery_config(1),
    ));
    let job_clock = Arc::clone(&clock);
    let (reservation, job) = AgentAuthorizationJob::new(
        *prepared,
        control,
        retirer,
        Arc::new(connector),
        Arc::new(move || job_clock.load(Ordering::SeqCst)),
        recovery_config(1),
        lifecycle,
    );
    let invocation = match supervisor.submit(reservation, job) {
        Ok(invocation) => invocation,
        Err(rejected) => panic!(
            "deadline lifecycle supervision failed: {}",
            rejected.error()
        ),
    };
    started.notified().await;
    clock.store(DEADLINE, Ordering::SeqCst);
    release.add_permits(1);
    let completion = invocation.wait().await.expect("deadline lifecycle result");
    let AgentAuthorizationJobCompletion::Authorized(completion) = completion else {
        panic!("the deadline lifecycle must own the authorized outcome");
    };
    supervisor.close().await.expect("deadline lifecycle drain");

    assert!(matches!(
        completion.disposition(),
        AgentAuthorizedLifecycleDisposition::ExecutedSettledAcked { sequence: 13, .. }
    ));
    let frames = progress_state
        .frames
        .lock()
        .expect("deadline lifecycle frames");
    assert_eq!(frames.len(), 9);
    let event_types: Vec<_> = frames[..8]
        .iter()
        .map(|frame| match frame.payload.as_ref() {
            Some(execution_output_frame_v1::Payload::NodeEvent(event)) => event.r#type.as_str(),
            _ => panic!("deadline progress frame must carry a NodeEvent"),
        })
        .collect();
    assert_eq!(
        event_types,
        [
            "agent_start",
            "agent_llm_start",
            "agent_llm_chunk",
            "agent_llm_end",
            "partial_message",
            "pipeline_finish",
            "agent_response",
            "full_message",
        ]
    );
    assert_eq!(
        restored_terminal_failure_kind(&frames[8]).expect("deadline terminal"),
        RuntimeFailureKind::DeadlineExceeded
    );
    assert_eq!(admission.available_capacity(), 1);
    assert!(
        trace
            .lock()
            .expect("trace")
            .ends_with(&["settlement", "redis"])
    );
    drop(temporary);
}

#[tokio::test]
async fn supervisor_stop_race_returns_unpolled_authorization_for_noack_cleanup() {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (temporary, root) = root();
    let preflight = preflight(root, "worker-1");
    let outcome = preflight.prepare(fresh()).await.expect("empty preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = outcome else {
        panic!("new execution spool must be empty");
    };
    let control = authorized_control(Arc::clone(&trace));
    let admission = InvocationAdmission::new(
        InvocationAdmissionConfig::new(1, Duration::from_secs(1)).expect("invocation admission"),
    );
    let supervisor = InvocationSupervisor::new(admission.clone());
    let prepared = prepare_fresh_agent_invocation_with(
        *empty,
        Arc::clone(&control),
        &admission,
        &ValidAgentInput {
            trace: Arc::clone(&trace),
        },
        Arc::new(|| NOW),
        AgentPreparationConfig::new(Duration::from_secs(10)).expect("preparation config"),
    )
    .await
    .expect("prepared invocation");
    let AgentPreparationOutcome::Prepared(prepared) = prepared else {
        panic!("valid application input must reach authorization");
    };
    let (started_sender, started_receiver) = oneshot::channel();
    let lifecycle = Arc::new(GatedAuthorizedLifecycle {
        trace: Arc::clone(&trace),
        started: Mutex::new(Some(started_sender)),
        release: Arc::new(Semaphore::new(0)),
    });
    let (reservation, job) = AgentAuthorizationJob::new(
        *prepared,
        control,
        Arc::new(recovery_retirer(
            Arc::clone(&trace),
            Ok(RedisRetirementResponse {
                acknowledged: 1,
                deleted: 1,
                unmapped: 1,
            }),
        )),
        Arc::new(FakeReplay::new(
            [ReplayResult::Acknowledged],
            Arc::clone(&trace),
        )),
        Arc::new(|| NOW),
        recovery_config(1),
        lifecycle,
    );
    supervisor.stop().expect("stop supervisor");

    let Err(rejected) = supervisor.submit(reservation, job) else {
        panic!("stopped supervisor must return unpolled authorization");
    };
    let (error, reservation, job) = rejected.into_parts();
    assert_eq!(error.code(), "invocation_supervision.closed");
    assert!(
        job.close_unstarted()
            .await
            .expect("unstarted cleanup")
            .is_none()
    );
    drop(reservation);
    assert!(started_receiver.await.is_err());
    assert_eq!(admission.available_capacity(), 1);
    assert_eq!(
        *trace.lock().expect("trace"),
        ["begin", "renew", "observe", "input"]
    );
    supervisor.close().await.expect("close supervisor");
    drop(temporary);
}

#[tokio::test]
async fn unknown_authorization_effect_closes_locally_without_output_or_redis_ack() {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (temporary, root) = root();
    let preflight = preflight(root, "worker-1");
    let outcome = preflight.prepare(fresh()).await.expect("empty preflight");
    let AgentOutputPreflightOutcome::Empty(empty) = outcome else {
        panic!("new execution spool must be empty");
    };
    let control = unavailable_authorization_control(Arc::clone(&trace));
    let admission = InvocationAdmission::new(
        InvocationAdmissionConfig::new(1, Duration::from_secs(1)).expect("invocation admission"),
    );
    let supervisor = InvocationSupervisor::new(admission.clone());
    let prepared = prepare_fresh_agent_invocation_with(
        *empty,
        Arc::clone(&control),
        &admission,
        &ValidAgentInput {
            trace: Arc::clone(&trace),
        },
        Arc::new(|| NOW),
        AgentPreparationConfig::new(Duration::from_secs(10)).expect("preparation config"),
    )
    .await
    .expect("prepared invocation");
    let AgentPreparationOutcome::Prepared(prepared) = prepared else {
        panic!("valid application input must reach authorization");
    };
    let lifecycle = Arc::new(GatedAuthorizedLifecycle {
        trace: Arc::clone(&trace),
        started: Mutex::new(None),
        release: Arc::new(Semaphore::new(1)),
    });
    let (reservation, job) = AgentAuthorizationJob::new(
        *prepared,
        control,
        Arc::new(recovery_retirer(
            Arc::clone(&trace),
            Ok(RedisRetirementResponse {
                acknowledged: 1,
                deleted: 1,
                unmapped: 1,
            }),
        )),
        Arc::new(FakeReplay::new(
            [ReplayResult::Acknowledged],
            Arc::clone(&trace),
        )),
        Arc::new(|| NOW),
        recovery_config(1),
        lifecycle,
    );
    let invocation = match supervisor.submit(reservation, job) {
        Ok(invocation) => invocation,
        Err(rejected) => panic!("authorization supervision failed: {}", rejected.error()),
    };
    let completion = invocation.wait().await.expect("authorization job result");
    let AgentAuthorizationJobCompletion::Unknown(completion) = completion else {
        panic!("transport uncertainty must remain a no-ACK cleanup outcome");
    };
    assert_eq!(completion.execution_kind(), AgentExecutionKind::Application);
    assert!(completion.authorization_error().retryable());
    assert!(completion.lease_error().is_none());
    supervisor.close().await.expect("supervisor drain");

    assert_eq!(admission.available_capacity(), 1);
    assert_eq!(
        *trace.lock().expect("trace"),
        ["begin", "renew", "observe", "input", "authorize"]
    );
    drop(temporary);
}

#[tokio::test]
async fn publication_deadline_is_sampled_after_the_final_lease_poll() {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (_temporary, control, terminal, admission) =
        pre_invocation_terminal_fixture(Arc::clone(&trace)).await;
    let replay = FakeReplay::new([ReplayResult::Acknowledged], Arc::clone(&trace));
    let retirer = recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    );

    let completion = publish_pre_invocation_terminal(
        control,
        &retirer,
        &replay,
        terminal,
        Arc::new(|| i64::MAX),
        recovery_config(1),
    )
    .await
    .expect("deadline terminal completion");

    assert_eq!(completion.failure(), RuntimeFailureKind::DeadlineExceeded);
    assert_eq!(admission.available_capacity(), 1);
    let frames = replay.frames.lock().expect("frames");
    assert_eq!(frames[0].occurred_at_unix_millis, i64::MAX);
    let Some(execution_output_frame_v1::Payload::RuntimeError(error)) = frames[0].payload.as_ref()
    else {
        panic!("canonical deadline terminal");
    };
    assert_eq!(error.code, RuntimeErrorCodeV1::DeadlineExceeded as i32);
    let trace = trace.lock().expect("trace");
    let final_poll = trace
        .windows(3)
        .position(|window| window == ["renew", "observe", "replay"]);
    assert!(final_poll.is_some(), "final lease poll must precede output");
}

#[tokio::test]
async fn final_stop_beats_both_the_proposed_failure_and_deadline() {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (_temporary, control, terminal, admission) = pre_invocation_terminal_fixture_with_policy(
        Arc::clone(&trace),
        None,
        [
            DesiredExecutionStateV1::Running,
            DesiredExecutionStateV1::Running,
            DesiredExecutionStateV1::Cancelled,
        ],
    )
    .await;
    let replay = FakeReplay::new([ReplayResult::Acknowledged], Arc::clone(&trace));
    let retirer = recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    );

    let completion = publish_pre_invocation_terminal(
        control,
        &retirer,
        &replay,
        terminal,
        Arc::new(|| i64::MAX),
        recovery_config(1),
    )
    .await
    .expect("cancellation terminal completion");

    assert_eq!(completion.failure(), RuntimeFailureKind::Cancelled);
    assert_eq!(admission.available_capacity(), 1);
    let frames = replay.frames.lock().expect("frames");
    let Some(execution_output_frame_v1::Payload::RuntimeError(error)) = frames[0].payload.as_ref()
    else {
        panic!("canonical cancellation terminal");
    };
    assert_eq!(error.code, RuntimeErrorCodeV1::Cancelled as i32);
}

#[tokio::test]
async fn fatal_final_lease_loss_suppresses_output_settlement_and_redis() {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (_temporary, control, terminal, admission) =
        pre_invocation_terminal_fixture_with_policy(Arc::clone(&trace), Some(2), []).await;
    let replay = FakeReplay::new([ReplayResult::Acknowledged], Arc::clone(&trace));
    let retirer = recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    );

    let error = publish_pre_invocation_terminal(
        control,
        &retirer,
        &replay,
        terminal,
        Arc::new(|| NOW),
        recovery_config(1),
    )
    .await
    .expect_err("fatal lease loss must suppress terminal output");

    assert_eq!(error.code(), "agent_failure_terminal.lease_lost");
    assert!(error.retryable());
    assert_eq!(admission.available_capacity(), 1);
    let trace = trace.lock().expect("trace");
    assert!(!trace.contains(&"replay"));
    assert!(!trace.contains(&"settlement"));
    assert!(!trace.contains(&"redis"));
}

#[tokio::test]
async fn exhausted_fresh_terminal_replay_retains_bytes_and_reports_safe_policy() {
    let trace = Arc::new(Mutex::new(Vec::new()));
    let (_temporary, control, terminal, admission) =
        pre_invocation_terminal_fixture(Arc::clone(&trace)).await;
    let replay = FakeReplay::new([ReplayResult::Unavailable], Arc::clone(&trace));
    let retirer = recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    );

    let error = publish_pre_invocation_terminal(
        control,
        &retirer,
        &replay,
        terminal,
        Arc::new(|| NOW),
        recovery_config(1),
    )
    .await
    .expect_err("bounded fresh terminal replay exhaustion");

    assert_eq!(error.code(), "agent_failure_terminal.output_unavailable");
    assert!(error.retryable());
    assert_eq!(admission.available_capacity(), 1);
    assert!(!trace.lock().expect("trace").contains(&"settlement"));
    assert!(!trace.lock().expect("trace").contains(&"redis"));
}

#[tokio::test]
async fn accepted_terminal_replays_settles_and_only_then_retires_redis() {
    let (_temporary, _preflight, recovery, frame) = pending_terminal_recovery().await;
    let trace = Arc::new(Mutex::new(Vec::new()));
    let replay = FakeReplay::new([ReplayResult::Acknowledged], Arc::clone(&trace));
    let retirer = recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    );

    let completion = recover_accepted_terminal(
        recovery_control(Arc::clone(&trace), false),
        &retirer,
        &replay,
        recovery,
        Arc::new(|| NOW),
        lease_config(),
        recovery_config(2),
    )
    .await
    .expect("terminal recovery");

    assert_eq!(completion.execution_kind(), AgentExecutionKind::Application);
    assert_eq!(completion.sequence(), frame.sequence);
    assert_eq!(completion.settlement_receipt_id(), "settlement-receipt-1");
    assert_eq!(
        *trace.lock().expect("trace"),
        ["replay", "settlement", "redis"]
    );
}

#[tokio::test(start_paused = true)]
async fn a_late_lease_failure_cannot_revoke_confirmed_terminal_retirement() {
    let (_temporary, _preflight, recovery, frame) = pending_terminal_recovery().await;
    let trace = Arc::new(Mutex::new(Vec::new()));
    let replay = FakeReplay::new([ReplayResult::AdvanceAndAcknowledged], Arc::clone(&trace));
    let retirer = recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    );

    let completion = recover_accepted_terminal(
        recovery_control_with_renew_failure(Arc::clone(&trace)),
        &retirer,
        &replay,
        recovery,
        Arc::new(|| NOW),
        ClaimLeaseMonitorConfig::new(Duration::from_millis(1)).expect("lease config"),
        recovery_config(1),
    )
    .await
    .expect("durable retirement wins late lease failure");

    assert_eq!(completion.sequence(), frame.sequence);
    assert_eq!(
        *trace.lock().expect("trace"),
        ["replay", "renew", "settlement", "redis"]
    );
}

#[tokio::test]
async fn reconnect_uses_a_fresh_exact_spool_and_stops_at_the_bound() {
    let (_temporary, preflight, recovery, _frame) = pending_terminal_recovery().await;
    let trace = Arc::new(Mutex::new(Vec::new()));
    let replay = FakeReplay::new(
        [
            ReplayResult::Unavailable,
            ReplayResult::DependencyUnavailable,
        ],
        Arc::clone(&trace),
    );
    let retirer = recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    );

    let error = recover_accepted_terminal(
        recovery_control(Arc::clone(&trace), false),
        &retirer,
        &replay,
        recovery,
        Arc::new(|| NOW),
        lease_config(),
        recovery_config(2),
    )
    .await
    .expect_err("bounded replay exhaustion");

    assert_eq!(error.code(), "agent_terminal_recovery.output_unavailable");
    assert!(error.retryable());
    assert_eq!(*trace.lock().expect("trace"), ["replay", "replay"]);
    let restored = preflight
        .prepare(fresh())
        .await
        .expect("exact terminal remains durable");
    assert_eq!(restored.kind(), AgentOutputPreflightKind::TerminalRecovery);
}

#[tokio::test]
async fn nonretryable_output_rejection_never_opens_a_second_session() {
    let (_temporary, _preflight, recovery, _frame) = pending_terminal_recovery().await;
    let trace = Arc::new(Mutex::new(Vec::new()));
    let replay = FakeReplay::new(
        [
            ReplayResult::AuthorizationFailed,
            ReplayResult::Acknowledged,
        ],
        Arc::clone(&trace),
    );
    let retirer = recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    );

    let error = recover_accepted_terminal(
        recovery_control(Arc::clone(&trace), false),
        &retirer,
        &replay,
        recovery,
        Arc::new(|| NOW),
        lease_config(),
        recovery_config(2),
    )
    .await
    .expect_err("authorization failure");

    assert_eq!(error.code(), "agent_terminal_recovery.output_rejected");
    assert!(!error.retryable());
    assert_eq!(*trace.lock().expect("trace"), ["replay"]);
}

#[tokio::test]
async fn frame_bound_cancellation_replaces_exact_bytes_and_gets_a_fresh_budget() {
    let (_temporary, _preflight, recovery, original) = pending_terminal_recovery().await;
    let trace = Arc::new(Mutex::new(Vec::new()));
    let replay = FakeReplay::new(
        [
            ReplayResult::Unavailable,
            ReplayResult::CancellationWon,
            ReplayResult::Unavailable,
            ReplayResult::Acknowledged,
        ],
        Arc::clone(&trace),
    );
    let retirer = recovery_retirer(
        Arc::clone(&trace),
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        }),
    );

    recover_accepted_terminal(
        recovery_control(Arc::clone(&trace), false),
        &retirer,
        &replay,
        recovery,
        Arc::new(|| NOW),
        lease_config(),
        recovery_config(2),
    )
    .await
    .expect("cancellation replacement");

    let frames = replay.frames.lock().expect("frames");
    assert_eq!(frames.len(), 4);
    assert_eq!(frames[0], original);
    assert_eq!(frames[1], original);
    assert_eq!(frames[2].sequence, original.sequence);
    assert_eq!(
        frames[2].occurred_at_unix_millis,
        original.occurred_at_unix_millis
    );
    assert_eq!(frames[2], frames[3]);
    let Some(execution_output_frame_v1::Payload::RuntimeError(error)) = frames[2].payload.as_ref()
    else {
        panic!("canonical cancellation terminal");
    };
    assert_eq!(error.code, RuntimeErrorCodeV1::Cancelled as i32);
    assert_eq!(
        *trace.lock().expect("trace"),
        [
            "replay",
            "replay",
            "replay",
            "replay",
            "settlement",
            "redis"
        ]
    );
}

#[tokio::test]
async fn settlement_or_redis_failure_cannot_skip_the_authority_order() {
    for (settlement_fails, redis_result, expected_trace, expected_code) in [
        (
            true,
            Ok(RedisRetirementResponse {
                acknowledged: 1,
                deleted: 1,
                unmapped: 1,
            }),
            vec!["replay", "settlement"],
            "agent_terminal_recovery.settlement_failed",
        ),
        (
            false,
            Err(RedisRetirementClientError::DependencyUnavailable),
            vec!["replay", "settlement", "redis"],
            "redis_command.dependency_unavailable",
        ),
    ] {
        let (_temporary, _preflight, recovery, _frame) = pending_terminal_recovery().await;
        let trace = Arc::new(Mutex::new(Vec::new()));
        let replay = FakeReplay::new([ReplayResult::Acknowledged], Arc::clone(&trace));
        let retirer = recovery_retirer(Arc::clone(&trace), redis_result);

        let error = recover_accepted_terminal(
            recovery_control(Arc::clone(&trace), settlement_fails),
            &retirer,
            &replay,
            recovery,
            Arc::new(|| NOW),
            lease_config(),
            recovery_config(2),
        )
        .await
        .expect_err("terminal completion failure");

        assert_eq!(error.code(), expected_code);
        assert!(error.retryable());
        assert_eq!(*trace.lock().expect("trace"), expected_trace);
    }
}

#[tokio::test]
async fn a_second_frame_bound_winner_does_not_enter_a_replacement_loop() {
    for first_winner in [ReplayResult::CancellationWon, ReplayResult::DeadlineWon] {
        let (_temporary, _preflight, recovery, _frame) = pending_terminal_recovery().await;
        let trace = Arc::new(Mutex::new(Vec::new()));
        let replay = FakeReplay::new([first_winner, first_winner], Arc::clone(&trace));
        let retirer = recovery_retirer(
            Arc::clone(&trace),
            Ok(RedisRetirementResponse {
                acknowledged: 1,
                deleted: 1,
                unmapped: 1,
            }),
        );

        let error = recover_accepted_terminal(
            recovery_control(Arc::clone(&trace), false),
            &retirer,
            &replay,
            recovery,
            Arc::new(|| NOW),
            lease_config(),
            recovery_config(2),
        )
        .await
        .expect_err("a canonical replacement winner must not loop");

        assert_eq!(error.code(), "agent_terminal_recovery.output_rejected");
        assert_eq!(*trace.lock().expect("trace"), ["replay", "replay"]);
    }
}

#[test]
fn terminal_recovery_session_policy_matches_the_deployed_v1_bounds() {
    assert!(AgentTerminalRecoveryConfig::new(1).is_ok());
    assert!(AgentTerminalRecoveryConfig::new(8).is_ok());
    for invalid in [0, 9] {
        let error = AgentTerminalRecoveryConfig::new(invalid).expect_err("invalid session limit");
        assert_eq!(
            error.code(),
            "agent_terminal_recovery.invalid_configuration"
        );
        assert!(!error.retryable());
        assert!(matches!(
            error,
            AgentTerminalRecoveryError::InvalidConfiguration(_)
        ));
    }
}

fn checkpoint_delivery() -> super::agent_delivery::CheckpointAgentDelivery {
    let raw = bytes("signed_command");
    let verified =
        parse_and_verify_agent_command(&raw, Some(&TestOnlyConformanceHmacAuthenticator))
            .expect("command");
    let mut response = claim_response();
    response.receipt.as_mut().expect("receipt").disposition =
        crate::protocol::elitea::runtime::v1::ClaimDispositionV1::RecoverAgentModelCheckpoint
            as i32;
    let inspection = crate::protocol::control::ModelCheckpointInspection::parse(
        &verified,
        response,
        "workload-1",
        "worker-1",
        NOW,
    )
    .expect("inspection");
    super::agent_delivery::test_checkpoint_delivery(redis_delivery(raw), verified, inspection)
}

#[tokio::test]
async fn checkpoint_output_reconciles_only_accepted_progress() {
    for case in 0..4 {
        let (_temporary, root) = root();
        let preflight = preflight(root, "worker-1");
        let AgentOutputPreflightOutcome::Empty(empty) =
            preflight.prepare(fresh()).await.expect("initial spool")
        else {
            panic!("empty spool")
        };
        let (accepted, output) = empty.into_parts();
        let mut spool = output.into_test_spool();
        if case != 0 {
            let mut fence = claim_response()
                .receipt
                .expect("receipt")
                .fence
                .expect("fence");
            fence.fence_token[0] ^= 1;
            let watermark = accepted.claim_handoff_watermark();
            let frame = match case {
                1 => progress_frame(&accepted, fence, watermark, watermark - 1),
                2 => progress_frame(&accepted, fence, watermark + 1, watermark),
                3 => terminal_frame(&accepted),
                _ => unreachable!(),
            };
            spool.persist(frame).expect("persisted output");
        }
        drop(spool);
        let result = preflight
            .prepare_checkpoint(&checkpoint_delivery())
            .await
            .expect("checkpoint preflight");
        if case <= 1 {
            let output = result.expect("ready checkpoint output");
            let mut spool = output.into_test_spool();
            assert_eq!(spool.pending_frame_count(), 0);
            let watermark = accepted.claim_handoff_watermark();
            let fence = claim_response()
                .receipt
                .expect("receipt")
                .fence
                .expect("fence");
            spool
                .persist(progress_frame(&accepted, fence, watermark + 1, watermark))
                .expect("reopened spool accepts the next output");
        } else {
            assert!(
                result.is_none(),
                "unacknowledged output must remain for replay"
            );
            assert!(
                preflight
                    .prepare_checkpoint(&checkpoint_delivery())
                    .await
                    .expect("reinspect")
                    .is_none()
            );
        }
    }
}

#[tokio::test]
async fn checkpoint_output_rejects_transport_identity_mismatch() {
    let (_temporary, root) = root();
    let preflight = preflight(root, "another-producer");
    assert!(matches!(
        preflight.prepare_checkpoint(&checkpoint_delivery()).await,
        Err(AgentOutputPreflightError::InvalidConfiguration(_))
    ));
}

#[tokio::test]
async fn checkpoint_supervisor_owns_authorization_after_waiter_drop_and_rejects_unstarted_work() {
    for stopped in [false, true] {
        let trace = Arc::new(Mutex::new(Vec::new()));
        let (_temporary, root) = root();
        let delivery = checkpoint_delivery();
        let output = preflight(root, "worker-1")
            .prepare_checkpoint(&delivery)
            .await
            .expect("preflight")
            .expect("empty output");
        let admission = InvocationAdmission::new(
            InvocationAdmissionConfig::new(1, Duration::from_secs(1)).expect("admission"),
        );
        let control = authorized_control(trace.clone());
        let retirer = Arc::new(recovery_retirer(
            trace.clone(),
            Ok(RedisRetirementResponse {
                acknowledged: 1,
                deleted: 1,
                unmapped: 1,
            }),
        ));
        let replay = Arc::new(FakeReplay::new([], trace.clone()));
        let (started_tx, started_rx) = oneshot::channel();
        let release = Arc::new(Semaphore::new(0));
        let lifecycle = Arc::new(GatedAuthorizedLifecycle {
            trace: trace.clone(),
            started: Mutex::new(Some(started_tx)),
            release: release.clone(),
        });
        let coordinator = super::agent_coordinator::AgentInvocationCoordinator::new(
            admission.clone(),
            control,
            retirer,
            replay,
            Arc::new(|| NOW),
            recovery_config(1),
            lifecycle,
        );
        let reservation = admission.reserve().await.expect("capacity");
        if stopped {
            coordinator.stop().expect("stop");
        }
        let result = coordinator.submit_checkpoint(
            delivery,
            output,
            reservation,
            Arc::new(ValidAgentInput {
                trace: trace.clone(),
            }),
            super::agent_lease::ClaimLeaseMonitorConfig::new(Duration::from_secs(10))
                .expect("lease config"),
        );
        if stopped {
            assert!(result.is_err());
            assert!(trace.lock().expect("trace").is_empty());
        } else {
            let waiter = result.expect("supervised recovery");
            started_rx.await.expect("authorized lifecycle started");
            drop(waiter);
            assert_eq!(admission.available_capacity(), 0);
            release.add_permits(1);
        }
        coordinator.close().await.expect("supervisor drain");
        assert_eq!(admission.available_capacity(), 1);
        assert_eq!(coordinator.active_count(), 0);
        if !stopped {
            let trace = trace.lock().expect("trace");
            assert!(!trace.contains(&"begin") && !trace.contains(&"authorize"));
            assert_eq!(
                trace
                    .iter()
                    .filter(|event| **event == "checkpoint_authorize")
                    .count(),
                1
            );
            assert_eq!(
                trace.iter().filter(|event| **event == "authorized").count(),
                1
            );
        }
    }
}

#[tokio::test]
async fn checkpoint_progress_replay_preserves_exact_bytes_and_closes_on_error() {
    for authorized in [true, false] {
        let (_temporary, root) = root();
        let preflight = preflight(root, "worker-1");
        let AgentOutputPreflightOutcome::Empty(empty) =
            preflight.prepare(fresh()).await.expect("initial spool")
        else {
            panic!("empty spool")
        };
        let (accepted, output) = empty.into_parts();
        let mut spool = output.into_test_spool();
        let fence = claim_response()
            .receipt
            .expect("receipt")
            .fence
            .expect("fence");
        let watermark = accepted.claim_handoff_watermark();
        let frame = progress_frame(&accepted, fence, watermark + 1, watermark);
        spool.persist(frame.clone()).expect("pending output");
        drop(spool);
        let state = FakeProgressState::new(
            [],
            [if authorized {
                ReplayProgressAction::Acknowledge
            } else {
                ReplayProgressAction::AuthorizationFailed
            }],
        );
        let connector = FakeProgressConnector {
            state: state.clone(),
        };
        let result = preflight
            .replay_checkpoint_progress(checkpoint_delivery(), &connector)
            .await;
        assert_eq!(result.is_ok(), authorized);
        assert_eq!(state.replays.load(Ordering::SeqCst), 1);
        assert_eq!(state.replay_closes.load(Ordering::SeqCst), 1);
        assert_eq!(state.connects.load(Ordering::SeqCst), 0);
        assert_eq!(state.sends.load(Ordering::SeqCst), 0);
        // ACK makes the spool empty; errors preserve the uncovered frame.
        let ready = preflight
            .prepare_checkpoint(&checkpoint_delivery())
            .await
            .expect("reinspect after replay");
        assert_eq!(ready.is_some(), authorized);
        if !authorized {
            state
                .replay_actions
                .lock()
                .expect("actions")
                .push_back(ReplayProgressAction::Acknowledge);
            preflight
                .replay_checkpoint_progress(checkpoint_delivery(), &connector)
                .await
                .expect("retry exact retained frame");
            assert_eq!(state.replays.load(Ordering::SeqCst), 2);
        }
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // Keep the two whole-delivery cases and their ownership assertions together.
async fn checkpoint_delivery_processor_joins_supervision_or_replays_before_reclaim() {
    for pending in [false, true] {
        let trace = Arc::new(Mutex::new(Vec::new()));
        let (_temporary, output_root) = root();
        let output = preflight(output_root, "worker-1");
        if pending {
            let AgentOutputPreflightOutcome::Empty(empty) =
                output.prepare(fresh()).await.expect("spool")
            else {
                panic!("empty")
            };
            let (accepted, prepared) = empty.into_parts();
            let watermark = accepted.claim_handoff_watermark();
            let fence = claim_response()
                .receipt
                .expect("receipt")
                .fence
                .expect("fence");
            prepared
                .into_test_spool()
                .persist(progress_frame(&accepted, fence, watermark + 1, watermark))
                .expect("pending");
        }
        let control = Arc::new(
            AgentControlClient::new(
                RecoveryControl {
                    trace: trace.clone(),
                    claim_fixture: "checkpoint",
                    settlement_fails: false,
                    authorize_disposition: Some(AuthorizeInvocationDispositionV1::AuthorizedNow),
                    authorize_unavailable: false,
                    renew_fail_after: None,
                    renew_attempts: AtomicUsize::new(0),
                    observe_states: Mutex::new(VecDeque::new()),
                },
                ControlGrpcConfig {
                    deadline: Duration::from_secs(1),
                    workload_session_id: "workload-1".to_owned(),
                    producer_id: "worker-1".to_owned(),
                },
            )
            .expect("control"),
        );
        let admission = InvocationAdmission::new(
            InvocationAdmissionConfig::new(1, Duration::from_secs(1)).expect("admission"),
        );
        let retirer = Arc::new(recovery_retirer(
            trace.clone(),
            Ok(RedisRetirementResponse {
                acknowledged: 1,
                deleted: 1,
                unmapped: 1,
            }),
        ));
        let state = FakeProgressState::new([], [ReplayProgressAction::Acknowledge]);
        let replay = Arc::new(FakeProgressConnector {
            state: state.clone(),
        });
        let lifecycle = Arc::new(GatedAuthorizedLifecycle {
            trace: trace.clone(),
            started: Mutex::new(None),
            release: Arc::new(Semaphore::new(1)),
        });
        let clock = Arc::new(|| NOW);
        let coordinator = super::agent_coordinator::AgentInvocationCoordinator::new(
            admission.clone(),
            control.clone(),
            retirer.clone(),
            replay.clone(),
            clock.clone(),
            recovery_config(1),
            lifecycle,
        );
        let processor = super::agent_delivery_processor::AgentDeliveryProcessor::new(
            Arc::new(TestOnlyConformanceHmacAuthenticator),
            output,
            control,
            retirer,
            replay,
            Arc::new(ValidAgentInput {
                trace: trace.clone(),
            }),
            clock.clone(),
            admission.clone(),
            AgentPreparationConfig::new(Duration::from_secs(10)).expect("preparation"),
            recovery_config(1),
            coordinator,
        );
        let raw = bytes("signed_command");
        let verified =
            parse_and_verify_agent_command(&raw, Some(&TestOnlyConformanceHmacAuthenticator))
                .expect("verified");
        assert!(
            processor
                .process_checkpoint_verified(redis_delivery(raw), verified)
                .await
                .is_ok(),
            "checkpoint processing"
        );
        processor.close().await.expect("drain");
        let events = trace.lock().expect("trace");
        assert_eq!(
            events
                .iter()
                .filter(|x| **x == "checkpoint_authorize")
                .count(),
            usize::from(!pending)
        );
        assert_eq!(
            events.iter().filter(|x| **x == "authorized").count(),
            usize::from(!pending)
        );
        assert!(
            !events.contains(&"begin")
                && !events.contains(&"authorize")
                && !events.contains(&"redis")
        );
        assert_eq!(state.replays.load(Ordering::SeqCst), usize::from(pending));
        assert_eq!(admission.available_capacity(), 1);
    }
}
