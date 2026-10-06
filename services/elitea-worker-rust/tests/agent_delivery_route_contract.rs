use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use elitea_worker_rust::agents::AgentExecutionKind;
use elitea_worker_rust::execution::{
    AgentDeliveryCompletionKind, AgentDeliveryError, AgentDeliveryRoute, AgentDeliveryRouteKind,
    AgentDeliveryRouter,
};
use elitea_worker_rust::protocol::command::TestOnlyConformanceHmacAuthenticator;
use elitea_worker_rust::protocol::control::{
    AgentControlClient, AgentOutputRecoveryKind, TerminalRedeliveryKind,
};
use elitea_worker_rust::protocol::elitea::runtime::v1::{
    AuthorizeInvocationRequestV1, AuthorizeInvocationResponseV1, BeginExecutionRequestV1,
    BeginExecutionResponseV1, ClaimCommandRequestV1, ClaimCommandResponseV1,
    ObserveDesiredStateRequestV1, ObserveDesiredStateResponseV1, PrepareSettlementRequestV1,
    PrepareSettlementResponseV1, RenewLeaseRequestV1, RenewLeaseResponseV1,
};
use elitea_worker_rust::transport::command_bus::{
    CommandBusLimits, CommandDelivery, CommandRetirementClient, CommandRetirementClientError,
    CommandRetirementConfig, CommandRetirementRequest, CommandRetirer, delivery_subject,
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
    vectors()[name]
        .as_bytes()
        .chunks_exact(2)
        .map(|pair| {
            u8::from_str_radix(std::str::from_utf8(pair).expect("ASCII hex"), 16)
                .expect("fixture hex")
        })
        .collect()
}

fn delivery(name: &str) -> CommandDelivery {
    delivery_bytes(bytes(name))
}

fn delivery_bytes(signed_envelope: Vec<u8>) -> CommandDelivery {
    // The fixtures' idempotency key names the delivery subject.
    CommandDelivery::decode(
        &delivery_subject("ELITEA_RT_V1_AGENT", "outbox-1").expect("contract subject"),
        "$JS.ACK.ELITEA_RT_V1_AGENT.elitea-agent-worker-v1.1.9.9.1700000000000000000.0",
        signed_envelope,
        CommandBusLimits::runtime_v1(),
    )
    .expect("command delivery")
}

struct RouteState {
    events: Mutex<Vec<&'static str>>,
    retirement_requests: Mutex<usize>,
    retirement_result: Mutex<Result<(), CommandRetirementClientError>>,
}

struct RouteControl {
    state: Arc<RouteState>,
    claim: ClaimCommandResponseV1,
    settlement: PrepareSettlementResponseV1,
    prepare_fails: bool,
}

#[async_trait]
impl ControlRpc for RouteControl {
    async fn claim_command(
        &self,
        _request: Request<ClaimCommandRequestV1>,
    ) -> Result<Response<ClaimCommandResponseV1>, Status> {
        self.state.events.lock().expect("events").push("claim");
        Ok(Response::new(self.claim.clone()))
    }

    async fn begin_execution(
        &self,
        _request: Request<BeginExecutionRequestV1>,
    ) -> Result<Response<BeginExecutionResponseV1>, Status> {
        panic!("the claim router must not call BeginExecution")
    }

    async fn authorize_invocation(
        &self,
        _request: Request<AuthorizeInvocationRequestV1>,
    ) -> Result<Response<AuthorizeInvocationResponseV1>, Status> {
        panic!("the claim router must not call AuthorizeInvocation")
    }

    async fn renew_lease(
        &self,
        _request: Request<RenewLeaseRequestV1>,
    ) -> Result<Response<RenewLeaseResponseV1>, Status> {
        panic!("the claim router must not call RenewLease")
    }

    async fn observe_desired_state(
        &self,
        _request: Request<ObserveDesiredStateRequestV1>,
    ) -> Result<Response<ObserveDesiredStateResponseV1>, Status> {
        panic!("the claim router must not call ObserveDesiredState")
    }

    async fn prepare_settlement(
        &self,
        _request: Request<PrepareSettlementRequestV1>,
    ) -> Result<Response<PrepareSettlementResponseV1>, Status> {
        self.state.events.lock().expect("events").push("settlement");
        if self.prepare_fails {
            return Err(Status::unavailable("test-only settlement failure"));
        }
        Ok(Response::new(self.settlement.clone()))
    }
}

struct RouteRetirementClient(Arc<RouteState>);

#[async_trait]
impl CommandRetirementClient for RouteRetirementClient {
    async fn retire_delivery(
        &self,
        _request: CommandRetirementRequest,
    ) -> Result<(), CommandRetirementClientError> {
        self.0.events.lock().expect("events").push("retire");
        *self
            .0
            .retirement_requests
            .lock()
            .expect("retirement request count") += 1;
        *self.0.retirement_result.lock().expect("retirement result")
    }
}

fn router(
    claim_fixture: &str,
) -> (
    AgentDeliveryRouter<RouteControl, RouteRetirementClient>,
    Arc<RouteState>,
) {
    router_with(claim_fixture, false, Ok(()))
}

fn router_with(
    claim_fixture: &str,
    prepare_fails: bool,
    retirement_result: Result<(), CommandRetirementClientError>,
) -> (
    AgentDeliveryRouter<RouteControl, RouteRetirementClient>,
    Arc<RouteState>,
) {
    let state = Arc::new(RouteState {
        events: Mutex::new(Vec::new()),
        retirement_requests: Mutex::new(0),
        retirement_result: Mutex::new(retirement_result),
    });
    let control = AgentControlClient::new(
        RouteControl {
            state: Arc::clone(&state),
            claim: ClaimCommandResponseV1::decode(bytes(claim_fixture).as_slice())
                .expect("claim fixture"),
            settlement: PrepareSettlementResponseV1::decode(
                bytes("settlement_succeeded").as_slice(),
            )
            .expect("settlement fixture"),
            prepare_fails,
        },
        ControlGrpcConfig {
            deadline: Duration::from_secs(1),
            workload_session_id: "workload-1".to_owned(),
            producer_id: "worker-1".to_owned(),
        },
    )
    .expect("control client");
    let retirer = CommandRetirer::new(
        RouteRetirementClient(Arc::clone(&state)),
        CommandRetirementConfig {
            stream: "ELITEA_RT_V1_AGENT".to_owned(),
            consumer: "elitea-agent-worker-v1".to_owned(),
        },
    )
    .expect("retirement client");
    (AgentDeliveryRouter::new(control, retirer), state)
}

fn events(state: &RouteState) -> Vec<&'static str> {
    state.events.lock().expect("events").clone()
}

#[tokio::test(flavor = "current_thread")]
async fn application_and_adhoc_share_the_fresh_claim_route() {
    for (command_fixture, expected_kind) in [
        ("signed_command", AgentExecutionKind::Application),
        ("signed_command_adhoc", AgentExecutionKind::Adhoc),
    ] {
        let (router, state) = router("accepted_claim");
        let route = router
            .route(
                delivery(command_fixture),
                &TestOnlyConformanceHmacAuthenticator,
                NOW,
            )
            .await
            .expect("fresh route");

        assert_eq!(route.kind(), AgentDeliveryRouteKind::Fresh);
        let AgentDeliveryRoute::Fresh(fresh) = route else {
            panic!("expected fresh route")
        };
        assert_eq!(fresh.execution_kind(), expected_kind);
        assert_eq!(fresh.claim_handoff_watermark(), 4);
        assert_eq!(events(&state), ["claim"]);
        assert_eq!(
            *state
                .retirement_requests
                .lock()
                .expect("retirement request count"),
            0
        );
    }
}

#[tokio::test(flavor = "current_thread")]
async fn terminal_redelivery_dispositions_retire_without_business_planes() {
    for (claim_fixture, expected_kind) in [
        ("claim_settled_ack", TerminalRedeliveryKind::Settled),
        ("claim_obsolete_ack", TerminalRedeliveryKind::Obsolete),
        ("claim_retired_ack", TerminalRedeliveryKind::Retired),
    ] {
        let (router, state) = router(claim_fixture);
        let route = router
            .route(
                delivery("signed_command"),
                &TestOnlyConformanceHmacAuthenticator,
                NOW,
            )
            .await
            .expect("terminal redelivery route");

        let AgentDeliveryRoute::Completed(completion) = route else {
            panic!("expected completed route")
        };
        assert_eq!(
            completion.kind(),
            AgentDeliveryCompletionKind::TerminalRedelivery(expected_kind)
        );
        assert_eq!(completion.settlement_receipt_id(), None);
        assert_eq!(events(&state), ["claim", "retire"]);
    }
}

#[tokio::test(flavor = "current_thread")]
async fn terminal_ack_recovery_prepares_settlement_before_retirement() {
    let (router, state) = router("claim_recover_terminal_ack");
    let route = router
        .route(
            delivery("signed_command"),
            &TestOnlyConformanceHmacAuthenticator,
            NOW,
        )
        .await
        .expect("terminal ACK recovery route");

    let AgentDeliveryRoute::Completed(completion) = route else {
        panic!("expected completed route")
    };
    assert_eq!(
        completion.kind(),
        AgentDeliveryCompletionKind::RecoveredTerminalSettlement
    );
    assert_eq!(
        completion.settlement_receipt_id(),
        Some("settlement-receipt-1")
    );
    assert_eq!(events(&state), ["claim", "settlement", "retire"]);
}

#[tokio::test(flavor = "current_thread")]
async fn prepared_settlement_recovery_retires_without_second_rpc() {
    let (router, state) = router("claim_recover_settlement");
    let route = router
        .route(
            delivery("signed_command"),
            &TestOnlyConformanceHmacAuthenticator,
            NOW,
        )
        .await
        .expect("prepared settlement recovery route");

    let AgentDeliveryRoute::Completed(completion) = route else {
        panic!("expected completed route")
    };
    assert_eq!(
        completion.kind(),
        AgentDeliveryCompletionKind::RecoveredSettlement
    );
    assert_eq!(
        completion.settlement_receipt_id(),
        Some("settlement-receipt-recovery-1")
    );
    assert_eq!(events(&state), ["claim", "retire"]);
}

#[tokio::test(flavor = "current_thread")]
async fn every_input_free_output_recovery_is_routed_without_an_ack() {
    for (claim_fixture, expected_kind) in [
        (
            "claim_active_lease_noack",
            AgentOutputRecoveryKind::ActiveLease,
        ),
        (
            "claim_recover_running_noack",
            AgentOutputRecoveryKind::Running,
        ),
        (
            "claim_recover_ambiguous_invocation_noack",
            AgentOutputRecoveryKind::AmbiguousInvocation,
        ),
    ] {
        let (router, state) = router(claim_fixture);
        let route = router
            .route(
                delivery("signed_command"),
                &TestOnlyConformanceHmacAuthenticator,
                NOW,
            )
            .await
            .expect("output recovery route");

        let AgentDeliveryRoute::OutputRecovery(recovery) = route else {
            panic!("expected output recovery route")
        };
        assert_eq!(recovery.recovery_kind(), expected_kind);
        assert_eq!(events(&state), ["claim"]);
        assert_eq!(
            *state
                .retirement_requests
                .lock()
                .expect("retirement request count"),
            0
        );
    }
}

#[tokio::test(flavor = "current_thread")]
async fn retry_later_remains_unacknowledged() {
    let (router, state) = router("claim_retry_later_noack");
    let route = router
        .route(
            delivery("signed_command"),
            &TestOnlyConformanceHmacAuthenticator,
            NOW,
        )
        .await
        .expect("retry route");

    assert_eq!(route.kind(), AgentDeliveryRouteKind::RetryLaterNoAck);
    assert_eq!(events(&state), ["claim"]);
    assert_eq!(
        *state
            .retirement_requests
            .lock()
            .expect("retirement request count"),
        0
    );
}

#[tokio::test(flavor = "current_thread")]
async fn settlement_failure_never_retires_the_delivery() {
    let (router, state) = router_with("claim_recover_terminal_ack", true, Ok(()));

    assert!(matches!(
        router
            .route(
                delivery("signed_command"),
                &TestOnlyConformanceHmacAuthenticator,
                NOW,
            )
            .await,
        Err(AgentDeliveryError::Control(_))
    ));
    assert_eq!(events(&state), ["claim", "settlement"]);
    assert_eq!(
        *state
            .retirement_requests
            .lock()
            .expect("retirement request count"),
        0
    );
}

#[tokio::test(flavor = "current_thread")]
async fn retirement_failure_never_returns_completed() {
    let (router, state) = router_with(
        "claim_recover_settlement",
        false,
        Err(CommandRetirementClientError::DependencyUnavailable),
    );

    assert!(matches!(
        router
            .route(
                delivery("signed_command"),
                &TestOnlyConformanceHmacAuthenticator,
                NOW,
            )
            .await,
        Err(AgentDeliveryError::Retirement(_))
    ));
    assert_eq!(events(&state), ["claim", "retire"]);
}

#[tokio::test(flavor = "current_thread")]
async fn authentication_failure_never_reaches_claim_or_retirement() {
    let (router, state) = router("claim_settled_ack");
    let mut signed = bytes("signed_command");
    let last = signed.last_mut().expect("nonempty signed fixture");
    *last ^= 1;

    assert!(matches!(
        router
            .route(
                delivery_bytes(signed),
                &TestOnlyConformanceHmacAuthenticator,
                NOW,
            )
            .await,
        Err(AgentDeliveryError::Protocol(_))
    ));
    assert!(events(&state).is_empty());
    assert_eq!(
        *state
            .retirement_requests
            .lock()
            .expect("retirement request count"),
        0
    );
}
