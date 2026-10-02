use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use elitea_worker_rust::execution::{
    ClaimLeaseActivation, ClaimLeaseError, ClaimLeaseMonitor, ClaimLeaseMonitorConfig,
};
use elitea_worker_rust::protocol::command::{
    SignedCommandAuthenticator, TestOnlyConformanceHmacAuthenticator,
    parse_and_verify_agent_command,
};
use elitea_worker_rust::protocol::control::{
    AgentClaimDecision, AgentControlClient, AgentControlError, AgentOutputRecoveryKind,
    BeginAgentExecution, ControlSemanticError, DesiredExecutionState, RuntimeControlRejectionKind,
    TerminalRedeliveryKind,
};
use elitea_worker_rust::protocol::elitea::runtime::v1::{
    AuthorizeInvocationResponseV1, BeginExecutionResponseV1, ClaimCommandRequestV1,
    ClaimCommandResponseV1, DesiredExecutionStateV1, ObserveDesiredStateRequestV1,
    ObserveDesiredStateResponseV1, PrepareSettlementRequestV1, PrepareSettlementResponseV1,
    RenewLeaseRequestV1, RenewLeaseResponseV1, RuntimeErrorCodeV1, RuntimeErrorV1,
};
use elitea_worker_rust::transport::{ControlGrpcConfig, ControlRpc};
use prost::Message;
use tonic::{Request, Response, Status};

const NOW: i64 = 1_700_000_000_000;

fn vectors() -> BTreeMap<&'static str, &'static str> {
    include_str!("fixtures/agent_control_vectors.txt")
        .lines()
        .map(|line| line.split_once('=').expect("named fixture"))
        .collect()
}

fn bytes(name: &str) -> Vec<u8> {
    let value = vectors()[name];
    assert_eq!(value.len() % 2, 0);
    value
        .as_bytes()
        .chunks_exact(2)
        .map(|pair| {
            u8::from_str_radix(std::str::from_utf8(pair).expect("ASCII hex"), 16)
                .expect("fixture hex")
        })
        .collect()
}

fn verified() -> elitea_worker_rust::protocol::command::VerifiedAgentCommand {
    verified_fixture("signed_command")
}

fn verified_fixture(name: &str) -> elitea_worker_rust::protocol::command::VerifiedAgentCommand {
    let authenticator = TestOnlyConformanceHmacAuthenticator;
    parse_and_verify_agent_command(
        &bytes(name),
        Some(&authenticator as &dyn SignedCommandAuthenticator),
    )
    .expect("signed command fixture")
}

fn claim_response() -> ClaimCommandResponseV1 {
    ClaimCommandResponseV1::decode(bytes("accepted_claim").as_slice()).expect("claim fixture")
}

fn claim_fixture(name: &str) -> ClaimCommandResponseV1 {
    ClaimCommandResponseV1::decode(bytes(name).as_slice()).expect("claim fixture")
}

#[derive(Default)]
struct FakeState {
    claim: Mutex<ClaimCommandResponseV1>,
    begin: Mutex<BeginExecutionResponseV1>,
    authorize: Mutex<AuthorizeInvocationResponseV1>,
    renew: Mutex<RenewLeaseResponseV1>,
    observe: Mutex<ObserveDesiredStateResponseV1>,
    settlement: Mutex<PrepareSettlementResponseV1>,
    claim_requests: Mutex<Vec<ClaimCommandRequestV1>>,
    renew_requests: Mutex<Vec<RenewLeaseRequestV1>>,
    settlement_requests: Mutex<Vec<PrepareSettlementRequestV1>>,
}

#[derive(Clone)]
struct FakeRpc(Arc<FakeState>);

#[async_trait]
impl ControlRpc for FakeRpc {
    async fn claim_command(
        &self,
        request: Request<ClaimCommandRequestV1>,
    ) -> Result<Response<ClaimCommandResponseV1>, Status> {
        self.0
            .claim_requests
            .lock()
            .expect("claim requests")
            .push(request.into_inner());
        Ok(Response::new(self.0.claim.lock().expect("claim").clone()))
    }

    async fn begin_execution(
        &self,
        _request: Request<
            elitea_worker_rust::protocol::elitea::runtime::v1::BeginExecutionRequestV1,
        >,
    ) -> Result<Response<BeginExecutionResponseV1>, Status> {
        Ok(Response::new(self.0.begin.lock().expect("begin").clone()))
    }

    async fn authorize_invocation(
        &self,
        _request: Request<
            elitea_worker_rust::protocol::elitea::runtime::v1::AuthorizeInvocationRequestV1,
        >,
    ) -> Result<Response<AuthorizeInvocationResponseV1>, Status> {
        Ok(Response::new(
            self.0.authorize.lock().expect("authorize").clone(),
        ))
    }

    async fn renew_lease(
        &self,
        request: Request<RenewLeaseRequestV1>,
    ) -> Result<Response<RenewLeaseResponseV1>, Status> {
        self.0
            .renew_requests
            .lock()
            .expect("renew requests")
            .push(request.into_inner());
        Ok(Response::new(self.0.renew.lock().expect("renew").clone()))
    }

    async fn observe_desired_state(
        &self,
        _request: Request<ObserveDesiredStateRequestV1>,
    ) -> Result<Response<ObserveDesiredStateResponseV1>, Status> {
        Ok(Response::new(
            self.0.observe.lock().expect("observe").clone(),
        ))
    }

    async fn prepare_settlement(
        &self,
        request: Request<PrepareSettlementRequestV1>,
    ) -> Result<Response<PrepareSettlementResponseV1>, Status> {
        self.0
            .settlement_requests
            .lock()
            .expect("settlement requests")
            .push(request.into_inner());
        Ok(Response::new(
            self.0.settlement.lock().expect("settlement").clone(),
        ))
    }
}

fn client() -> (AgentControlClient<FakeRpc>, Arc<FakeState>) {
    let state = Arc::new(FakeState::default());
    *state.claim.lock().expect("claim") = claim_response();
    *state.begin.lock().expect("begin") =
        BeginExecutionResponseV1::decode(bytes("begin_started").as_slice()).expect("begin fixture");
    *state.authorize.lock().expect("authorize") =
        AuthorizeInvocationResponseV1::decode(bytes("authorize_now").as_slice())
            .expect("authorize fixture");
    *state.renew.lock().expect("renew") =
        RenewLeaseResponseV1::decode(bytes("renew_running").as_slice()).expect("renew fixture");
    *state.observe.lock().expect("observe") =
        ObserveDesiredStateResponseV1::decode(bytes("observe_running").as_slice())
            .expect("observe fixture");
    *state.settlement.lock().expect("settlement") =
        PrepareSettlementResponseV1::decode(bytes("settlement_succeeded").as_slice())
            .expect("settlement fixture");
    let control = AgentControlClient::new(
        FakeRpc(Arc::clone(&state)),
        ControlGrpcConfig {
            deadline: Duration::from_secs(1),
            workload_session_id: "workload-1".to_owned(),
            producer_id: "worker-1".to_owned(),
        },
    )
    .expect("semantic control client");
    (control, state)
}

async fn fresh_claim(
    control: &AgentControlClient<FakeRpc>,
) -> elitea_worker_rust::protocol::control::AcceptedAgentClaim {
    control
        .claim_agent(&verified(), NOW)
        .await
        .expect("accepted claim")
}

#[tokio::test(flavor = "current_thread")]
async fn python_vectors_cross_authenticated_claim_and_begin_fence() {
    let (control, state) = client();
    let claim = fresh_claim(&control).await;
    let claim_request = state.claim_requests.lock().expect("claim requests")[0].clone();
    assert_eq!(claim_request.workload_session_id, "workload-1");
    assert_eq!(claim_request.producer_id, "worker-1");
    assert_eq!(
        claim_request.signed_command.as_ref(),
        Some(verified().signed())
    );

    let BeginAgentExecution::Preparing(preparing) = control
        .begin_agent_execution(claim)
        .await
        .expect("preparing execution")
    else {
        panic!("fresh begin must prepare")
    };
    let starting = preparing.start_lease_monitor();
    let mut monitor = ClaimLeaseMonitor::start(
        Arc::new(control.clone()),
        starting,
        Arc::new(|| NOW),
        ClaimLeaseMonitorConfig::new(Duration::from_secs(10)).expect("lease config"),
    );
    let ClaimLeaseActivation::Active(preparing) = monitor.activate().await else {
        panic!("running immediate lease poll must activate input authority")
    };
    assert_eq!(preparing.input_bundle().input_bundle_id, "bundle-1");
    assert_eq!(preparing.input_bundle_ref().immutable_version, "v1");
    assert_eq!(preparing.request_entry().entry_id, "agent-request");

    monitor.close().await.expect("lease close");
}

#[tokio::test(flavor = "current_thread")]
async fn every_python_restart_disposition_maps_to_an_input_free_closed_type() {
    let (control, state) = client();

    let AgentClaimDecision::Accepted(_accepted) = control
        .claim_agent_delivery(&verified(), NOW)
        .await
        .expect("fresh accepted delivery")
    else {
        panic!("accepted claim must be the only input-bearing decision")
    };
    *state.claim.lock().expect("claim") = claim_fixture("claim_settled_ack");
    let AgentClaimDecision::SettledAck(authority) = control
        .claim_agent_delivery(&verified(), NOW)
        .await
        .expect("settled redelivery")
    else {
        panic!("settled claim must return terminal ACK authority")
    };
    assert_eq!(authority.kind(), TerminalRedeliveryKind::Settled);

    *state.claim.lock().expect("claim") = claim_fixture("claim_obsolete_ack");
    let AgentClaimDecision::ObsoleteAck(authority) = control
        .claim_agent_delivery(&verified(), NOW)
        .await
        .expect("obsolete redelivery")
    else {
        panic!("obsolete claim must return terminal ACK authority")
    };
    assert_eq!(authority.kind(), TerminalRedeliveryKind::Obsolete);

    *state.claim.lock().expect("claim") = claim_fixture("claim_retry_later_noack");
    assert!(matches!(
        control
            .claim_agent_delivery(&verified(), NOW)
            .await
            .expect("retry decision"),
        AgentClaimDecision::RetryLaterNoAck(_)
    ));

    *state.claim.lock().expect("claim") = claim_fixture("claim_retired_ack");
    let AgentClaimDecision::RetiredAck(authority) = control
        .claim_agent_delivery(&verified(), NOW)
        .await
        .expect("deadline retirement")
    else {
        panic!("retired claim must return terminal ACK authority")
    };
    assert_eq!(authority.kind(), TerminalRedeliveryKind::Retired);

    *state.claim.lock().expect("claim") = claim_fixture("claim_active_lease_noack");
    let AgentClaimDecision::ActiveLeaseNoAck(recovery) = control
        .claim_agent_delivery(&verified(), NOW)
        .await
        .expect("active lease recovery")
    else {
        panic!("active lease must return output-only recovery")
    };
    assert_eq!(recovery.kind(), AgentOutputRecoveryKind::ActiveLease);
    assert_eq!(recovery.desired_state(), DesiredExecutionState::Running);
    let (recovery, _lease) = recovery.split_lease_authority();
    assert_eq!(recovery.claim_handoff_watermark(), 4);

    *state.claim.lock().expect("claim") = claim_fixture("claim_recover_running_noack");
    let AgentClaimDecision::RecoverRunningNoAck(recovery) = control
        .claim_agent_delivery(&verified(), NOW)
        .await
        .expect("running recovery")
    else {
        panic!("running claim must return output-only recovery")
    };
    assert_eq!(recovery.kind(), AgentOutputRecoveryKind::Running);
    assert_eq!(recovery.desired_state(), DesiredExecutionState::Cancelled);

    *state.claim.lock().expect("claim") = claim_fixture("claim_recover_ambiguous_invocation_noack");
    let AgentClaimDecision::RecoverAmbiguousInvocationNoAck(recovery) = control
        .claim_agent_delivery(&verified(), NOW)
        .await
        .expect("ambiguous invocation recovery")
    else {
        panic!("ambiguous claim must return output-only recovery")
    };
    assert_eq!(
        recovery.kind(),
        AgentOutputRecoveryKind::AmbiguousInvocation
    );
    assert_eq!(recovery.desired_state(), DesiredExecutionState::Running);
}

#[tokio::test(flavor = "current_thread")]
async fn persisted_terminal_and_settlement_recovery_never_recreate_business_authority() {
    let (control, state) = client();
    *state.claim.lock().expect("claim") = claim_fixture("claim_recover_terminal_ack");
    let AgentClaimDecision::RecoverTerminalAck(recovery) = control
        .claim_agent_delivery(&verified(), NOW)
        .await
        .expect("terminal ACK recovery")
    else {
        panic!("terminal ACK recovery must return proposal authority")
    };
    let receipt = control
        .prepare_recovered_agent_settlement(recovery)
        .await
        .expect("exact settlement replay");
    assert_eq!(receipt.receipt_id(), "settlement-receipt-1");
    {
        let requests = state
            .settlement_requests
            .lock()
            .expect("settlement requests");
        let request = requests.last().expect("settlement replay request");
        let proposal = request.proposal.as_ref().expect("persisted proposal");
        assert_eq!(proposal.proposal_id, "command-1:settlement");
        assert_eq!(request.idempotency_key, "command-1:prepare-settlement");
        assert_eq!(
            request
                .proposal_digest
                .as_ref()
                .expect("proposal digest")
                .value,
            ring::digest::digest(&ring::digest::SHA256, &proposal.encode_to_vec()).as_ref()
        );
    }

    *state.claim.lock().expect("claim") = claim_fixture("claim_recover_settlement");
    let AgentClaimDecision::RecoverSettlement(recovery) = control
        .claim_agent_delivery(&verified(), NOW)
        .await
        .expect("prepared settlement recovery")
    else {
        panic!("prepared settlement must return receipt authority")
    };
    assert_eq!(recovery.receipt_id(), "settlement-receipt-recovery-1");
    assert_eq!(
        recovery.outcome(),
        elitea_worker_rust::protocol::elitea::runtime::v1::ExecutionOutcomeV1::Succeeded
    );
    assert_eq!(
        state
            .settlement_requests
            .lock()
            .expect("settlement requests")
            .len(),
        1,
        "an already prepared settlement must not call PrepareSettlement"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn malformed_recovery_shapes_fail_closed_before_authority_is_minted() {
    let (control, state) = client();

    for disposition in [0, 999] {
        let mut malformed = claim_response();
        malformed.receipt.as_mut().expect("receipt").disposition = disposition;
        *state.claim.lock().expect("claim") = malformed;
        assert!(
            control
                .claim_agent_delivery(&verified(), NOW)
                .await
                .is_err()
        );
    }

    let mut business_leak = claim_fixture("claim_recover_running_noack");
    business_leak
        .receipt
        .as_mut()
        .expect("receipt")
        .input_bundle = claim_response()
        .receipt
        .expect("accepted receipt")
        .input_bundle;
    *state.claim.lock().expect("claim") = business_leak;
    assert!(
        control
            .claim_agent_delivery(&verified(), NOW)
            .await
            .is_err()
    );

    let mut forged_no_authority = claim_fixture("claim_obsolete_ack");
    forged_no_authority
        .receipt
        .as_mut()
        .expect("receipt")
        .claim_id = "forged".to_owned();
    *state.claim.lock().expect("claim") = forged_no_authority;
    assert!(
        control
            .claim_agent_delivery(&verified(), NOW)
            .await
            .is_err()
    );

    let mut changed_proposal = claim_fixture("claim_recover_terminal_ack");
    changed_proposal
        .receipt
        .as_mut()
        .expect("receipt")
        .settlement_recovery
        .as_mut()
        .expect("recovery")
        .proposal
        .as_mut()
        .expect("proposal")
        .terminal_sequence += 1;
    *state.claim.lock().expect("claim") = changed_proposal;
    assert!(matches!(
        control.claim_agent_delivery(&verified(), NOW).await,
        Err(AgentControlError::Semantic(
            ControlSemanticError::InvalidInput(_) | ControlSemanticError::AuthorizationFailed(_)
        ))
    ));

    let mut malformed_retirement = claim_fixture("claim_retired_ack");
    malformed_retirement
        .receipt
        .as_mut()
        .expect("receipt")
        .retirement
        .as_mut()
        .expect("retirement")
        .retryable = false;
    *state.claim.lock().expect("claim") = malformed_retirement;
    assert!(
        control
            .claim_agent_delivery(&verified(), NOW)
            .await
            .is_err()
    );
}

#[tokio::test(flavor = "current_thread")]
async fn supervised_lease_key_state_and_monotonicity_match_python_semantics() {
    let (control, state) = client();
    let claim = fresh_claim(&control).await;
    let BeginAgentExecution::Preparing(preparing) = control
        .begin_agent_execution(claim)
        .await
        .expect("preparing execution")
    else {
        panic!("fresh begin must prepare")
    };
    let starting = preparing.start_lease_monitor();
    let mut monitor = ClaimLeaseMonitor::start(
        Arc::new(control.clone()),
        starting,
        Arc::new(|| NOW),
        ClaimLeaseMonitorConfig::new(Duration::from_secs(10)).expect("lease config"),
    );
    assert!(matches!(
        monitor.activate().await,
        ClaimLeaseActivation::Active(_)
    ));
    assert_eq!(
        state.renew_requests.lock().expect("renew requests")[0].idempotency_key,
        vectors()["renewal_key_1"]
    );

    *state.renew.lock().expect("renew") = RenewLeaseResponseV1 {
        lease_expires_at_unix_millis: NOW + 19_999,
        desired_state: DesiredExecutionStateV1::Cancelled as i32,
        rejection: None,
    };
    let error = monitor.check_now().await.expect_err("shortened lease");
    assert!(matches!(
        error,
        ClaimLeaseError::Control(AgentControlError::Semantic(
            ControlSemanticError::AuthorizationFailed(_)
        ))
    ));
    assert!(monitor.close().await.is_err());
}

#[tokio::test(flavor = "current_thread")]
async fn claim_authority_rejects_every_binding_mutation() {
    let mutations: [fn(&mut ClaimCommandResponseV1); 7] = [
        |response| {
            response
                .receipt
                .as_mut()
                .expect("receipt")
                .identity
                .as_mut()
                .expect("identity")
                .generation += 1;
        },
        |response| {
            response
                .receipt
                .as_mut()
                .expect("receipt")
                .fence
                .as_mut()
                .expect("fence")
                .producer_id = "other-worker".to_owned();
        },
        |response| {
            response
                .receipt
                .as_mut()
                .expect("receipt")
                .fence
                .as_mut()
                .expect("fence")
                .fence_token = vec![0; 32];
        },
        |response| {
            response
                .receipt
                .as_mut()
                .expect("receipt")
                .input_bundle
                .as_mut()
                .expect("manifest")
                .entries[0]
                .semantic_role = "unrelated.input".to_owned();
        },
        |response| {
            let manifest = response
                .receipt
                .as_mut()
                .expect("receipt")
                .input_bundle
                .as_mut()
                .expect("manifest");
            manifest.entries.push(manifest.entries[0].clone());
        },
        |response| {
            response
                .receipt
                .as_mut()
                .expect("receipt")
                .claim_started_at_unix_micros = 0;
        },
        |response| {
            let receipt = response.receipt.as_mut().expect("receipt");
            receipt.claim_started_at_unix_micros = receipt
                .lease_expires_at_unix_millis
                .checked_mul(1_000)
                .expect("fixture expiry in microseconds");
        },
    ];
    for mutate in mutations {
        let (control, state) = client();
        mutate(&mut state.claim.lock().expect("claim"));
        assert!(control.claim_agent(&verified(), NOW).await.is_err());
    }
}

#[tokio::test(flavor = "current_thread")]
async fn rejection_mapping_retains_wire_retryability_without_server_text() {
    let (control, state) = client();
    *state.begin.lock().expect("begin") =
        BeginExecutionResponseV1::decode(bytes("authorization_rejection").as_slice())
            .expect("rejection fixture");
    let claim = fresh_claim(&control).await;
    let Err(error) = control.begin_agent_execution(claim).await else {
        panic!("stale fence must not create execution typestate")
    };
    let AgentControlError::Semantic(ControlSemanticError::Rejected(rejection)) = error else {
        panic!("registered runtime rejection")
    };
    assert_eq!(
        rejection.kind(),
        RuntimeControlRejectionKind::AuthorizationFailed
    );
    assert!(!rejection.retryable());
    assert!(!rejection.to_string().contains("fixture detail"));
}

#[tokio::test(flavor = "current_thread")]
async fn every_registered_runtime_rejection_maps_without_rederiving_retryability() {
    for (index, (code, expected)) in [
        (
            RuntimeErrorCodeV1::UnsupportedCapability,
            RuntimeControlRejectionKind::UnsupportedCapability,
        ),
        (
            RuntimeErrorCodeV1::IncompatibleVersion,
            RuntimeControlRejectionKind::IncompatibleVersion,
        ),
        (
            RuntimeErrorCodeV1::InvalidInput,
            RuntimeControlRejectionKind::InvalidInput,
        ),
        (
            RuntimeErrorCodeV1::ProtocolViolation,
            RuntimeControlRejectionKind::InvalidInput,
        ),
        (
            RuntimeErrorCodeV1::ResourceExhausted,
            RuntimeControlRejectionKind::ResourceExhausted,
        ),
        (
            RuntimeErrorCodeV1::DependencyUnavailable,
            RuntimeControlRejectionKind::DependencyUnavailable,
        ),
        (
            RuntimeErrorCodeV1::Internal,
            RuntimeControlRejectionKind::DependencyUnavailable,
        ),
        (
            RuntimeErrorCodeV1::AuthenticationFailed,
            RuntimeControlRejectionKind::AuthorizationFailed,
        ),
        (
            RuntimeErrorCodeV1::AuthorizationFailed,
            RuntimeControlRejectionKind::AuthorizationFailed,
        ),
        (
            RuntimeErrorCodeV1::StaleFence,
            RuntimeControlRejectionKind::AuthorizationFailed,
        ),
        (
            RuntimeErrorCodeV1::Cancelled,
            RuntimeControlRejectionKind::Cancelled,
        ),
        (
            RuntimeErrorCodeV1::DeadlineExceeded,
            RuntimeControlRejectionKind::DeadlineExceeded,
        ),
    ]
    .into_iter()
    .enumerate()
    {
        let retryable = index % 2 == 0;
        let (control, state) = client();
        *state.begin.lock().expect("begin") = BeginExecutionResponseV1 {
            disposition: 0,
            rejection: Some(RuntimeErrorV1 {
                code: code as i32,
                safe_message: "never exposed".to_owned(),
                retryable,
            }),
        };
        let claim = fresh_claim(&control).await;
        let Err(AgentControlError::Semantic(ControlSemanticError::Rejected(rejection))) =
            control.begin_agent_execution(claim).await
        else {
            panic!("registered rejection must remain typed")
        };
        assert_eq!(rejection.kind(), expected);
        assert_eq!(rejection.retryable(), retryable);
    }
}

#[tokio::test(flavor = "current_thread")]
async fn mixed_control_response_shapes_fail_closed() {
    let (control, state) = client();
    *state.begin.lock().expect("begin") = BeginExecutionResponseV1 {
        disposition: elitea_worker_rust::protocol::elitea::runtime::v1::BeginExecutionDispositionV1::StartedNow as i32,
        rejection: Some(RuntimeErrorV1 {
            code: RuntimeErrorCodeV1::Internal as i32,
            safe_message: String::new(),
            retryable: false,
        }),
    };
    let claim = fresh_claim(&control).await;
    assert!(matches!(
        control.begin_agent_execution(claim).await,
        Err(AgentControlError::Semantic(
            ControlSemanticError::InvalidInput(_)
        ))
    ));

    let (control, state) = client();
    *state.renew.lock().expect("renew") = RenewLeaseResponseV1 {
        lease_expires_at_unix_millis: NOW + 60_000,
        desired_state: DesiredExecutionStateV1::Running as i32,
        rejection: Some(RuntimeErrorV1 {
            code: RuntimeErrorCodeV1::Internal as i32,
            safe_message: String::new(),
            retryable: false,
        }),
    };
    let claim = fresh_claim(&control).await;
    let BeginAgentExecution::Preparing(preparing) = control
        .begin_agent_execution(claim)
        .await
        .expect("preparing execution")
    else {
        panic!("fresh begin must prepare")
    };
    let starting = preparing.start_lease_monitor();
    let mut monitor = ClaimLeaseMonitor::start(
        Arc::new(control),
        starting,
        Arc::new(|| NOW),
        ClaimLeaseMonitorConfig::new(Duration::from_secs(10)).expect("lease config"),
    );
    let ClaimLeaseActivation::Inactive { error, .. } = monitor.activate().await else {
        panic!("ambiguous renewal must not activate the execution")
    };
    assert!(matches!(
        error,
        ClaimLeaseError::Control(AgentControlError::Semantic(
            ControlSemanticError::InvalidInput(_)
        ))
    ));
    assert!(monitor.close().await.is_err());
}

#[tokio::test(flavor = "current_thread")]
async fn expired_and_oversized_authority_is_rejected() {
    let (control, state) = client();
    state
        .claim
        .lock()
        .expect("claim")
        .receipt
        .as_mut()
        .expect("receipt")
        .lease_expires_at_unix_millis = NOW;
    assert!(control.claim_agent(&verified(), NOW).await.is_err());

    let (control, state) = client();
    state
        .claim
        .lock()
        .expect("claim")
        .receipt
        .as_mut()
        .expect("receipt")
        .input_bundle
        .as_mut()
        .expect("manifest")
        .entries[0]
        .content
        .as_mut()
        .expect("content")
        .byte_length = 1024 * 1024 + 1;
    assert!(control.claim_agent(&verified(), NOW).await.is_err());
}

#[tokio::test(flavor = "current_thread")]
async fn authoritative_eight_mebibyte_agent_reference_boundary_is_exact() {
    let (control, state) = client();
    *state.claim.lock().expect("claim") =
        ClaimCommandResponseV1::decode(bytes("accepted_claim_agent_input_at_limit").as_slice())
            .expect("at-limit claim fixture");
    assert!(
        control
            .claim_agent(
                &verified_fixture("signed_command_agent_input_at_limit"),
                NOW
            )
            .await
            .is_ok()
    );

    let (control, state) = client();
    *state.claim.lock().expect("claim") =
        ClaimCommandResponseV1::decode(bytes("accepted_claim_agent_input_over_limit").as_slice())
            .expect("over-limit claim fixture");
    assert!(matches!(
        control
            .claim_agent(
                &verified_fixture("signed_command_agent_input_over_limit"),
                NOW
            )
            .await,
        Err(AgentControlError::Semantic(
            ControlSemanticError::InvalidInput(_)
        ))
    ));
}
