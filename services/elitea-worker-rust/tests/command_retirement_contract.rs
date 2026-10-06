use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use elitea_worker_rust::protocol::command::{
    SignedCommandAuthenticator, TestOnlyConformanceHmacAuthenticator, VerifiedAgentCommand,
    parse_and_verify_agent_command,
};
use elitea_worker_rust::protocol::control::{
    AgentClaimDecision, AgentCommandRetirementAuthority, AgentControlClient,
};
use elitea_worker_rust::protocol::elitea::runtime::v1::{
    AuthorizeInvocationRequestV1, AuthorizeInvocationResponseV1, BeginExecutionRequestV1,
    BeginExecutionResponseV1, ClaimCommandRequestV1, ClaimCommandResponseV1,
    ObserveDesiredStateRequestV1, ObserveDesiredStateResponseV1, PrepareSettlementRequestV1,
    PrepareSettlementResponseV1, RenewLeaseRequestV1, RenewLeaseResponseV1,
    SignedWorkerCommandEnvelopeV1,
};
use elitea_worker_rust::transport::command_bus::{
    CommandBusError, CommandBusLimits, CommandDelivery, CommandRetirementClient,
    CommandRetirementClientError, CommandRetirementConfig, CommandRetirementRequest,
    CommandRetirer, delivery_subject,
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

fn verified(name: &str) -> VerifiedAgentCommand {
    let authenticator = TestOnlyConformanceHmacAuthenticator;
    parse_and_verify_agent_command(
        &bytes(name),
        Some(&authenticator as &dyn SignedCommandAuthenticator),
    )
    .expect("verified command")
}

struct ClaimRpc {
    response: ClaimCommandResponseV1,
}

#[async_trait]
impl ControlRpc for ClaimRpc {
    async fn claim_command(
        &self,
        _request: Request<ClaimCommandRequestV1>,
    ) -> Result<Response<ClaimCommandResponseV1>, Status> {
        Ok(Response::new(self.response.clone()))
    }

    async fn begin_execution(
        &self,
        _request: Request<BeginExecutionRequestV1>,
    ) -> Result<Response<BeginExecutionResponseV1>, Status> {
        Err(Status::unimplemented("not used"))
    }

    async fn authorize_invocation(
        &self,
        _request: Request<AuthorizeInvocationRequestV1>,
    ) -> Result<Response<AuthorizeInvocationResponseV1>, Status> {
        Err(Status::unimplemented("not used"))
    }

    async fn renew_lease(
        &self,
        _request: Request<RenewLeaseRequestV1>,
    ) -> Result<Response<RenewLeaseResponseV1>, Status> {
        Err(Status::unimplemented("not used"))
    }

    async fn observe_desired_state(
        &self,
        _request: Request<ObserveDesiredStateRequestV1>,
    ) -> Result<Response<ObserveDesiredStateResponseV1>, Status> {
        Err(Status::unimplemented("not used"))
    }

    async fn prepare_settlement(
        &self,
        _request: Request<PrepareSettlementRequestV1>,
    ) -> Result<Response<PrepareSettlementResponseV1>, Status> {
        Err(Status::unimplemented("not used"))
    }
}

async fn terminal_authority(command: &VerifiedAgentCommand) -> AgentCommandRetirementAuthority {
    let response = ClaimCommandResponseV1::decode(bytes("claim_obsolete_ack").as_slice())
        .expect("obsolete claim");
    let control = AgentControlClient::new(
        ClaimRpc { response },
        ControlGrpcConfig {
            deadline: Duration::from_secs(1),
            workload_session_id: "workload-1".to_owned(),
            producer_id: "worker-1".to_owned(),
        },
    )
    .expect("control client");
    let AgentClaimDecision::ObsoleteAck(authority) = control
        .claim_agent_delivery(command, NOW)
        .await
        .expect("obsolete authority")
    else {
        panic!("expected obsolete command retirement")
    };
    authority.into()
}

fn limits() -> CommandBusLimits {
    CommandBusLimits::runtime_v1()
}

const STREAM: &str = "ELITEA_RT_V1_AGENT";
const CONSUMER: &str = "elitea-agent-worker-v1";
const REPLY: &str =
    "$JS.ACK.ELITEA_RT_V1_AGENT.elitea-agent-worker-v1.2.41.40.1700000000000000000.0";

fn delivery(vector: &str) -> CommandDelivery {
    delivery_bytes(bytes(vector))
}

/// The fixtures' idempotency key, which names the delivery subject.
fn delivery_bytes(signed_envelope: Vec<u8>) -> CommandDelivery {
    delivery_on(&subject_for("outbox-1"), signed_envelope)
}

fn subject_for(delivery_id: &str) -> String {
    delivery_subject(STREAM, delivery_id).expect("contract subject")
}

fn delivery_on(subject: &str, signed_envelope: Vec<u8>) -> CommandDelivery {
    CommandDelivery::decode(subject, REPLY, signed_envelope, limits()).expect("command delivery")
}

fn push_varint(target: &mut Vec<u8>, mut value: u64) {
    while value >= 0x80 {
        target.push(u8::try_from(value & 0x7f).expect("seven-bit varint chunk") | 0x80);
        value >>= 7;
    }
    target.push(u8::try_from(value).expect("terminal varint byte"));
}

fn push_length_field(target: &mut Vec<u8>, field: u8, value: &[u8]) {
    target.push((field << 3) | 2);
    push_varint(target, value.len() as u64);
    target.extend_from_slice(value);
}

fn reordered_outer_envelope(vector: &str) -> Vec<u8> {
    let signed = SignedWorkerCommandEnvelopeV1::decode(bytes(vector).as_slice())
        .expect("signed command fixture");
    let mut raw = Vec::new();
    push_length_field(&mut raw, 6, &signed.signature);
    push_length_field(
        &mut raw,
        5,
        &signed
            .worker_command_digest
            .expect("worker command digest")
            .encode_to_vec(),
    );
    push_length_field(&mut raw, 4, &signed.worker_command_bytes);
    push_length_field(&mut raw, 3, signed.key_id.as_bytes());
    raw.push(2 << 3);
    push_varint(
        &mut raw,
        u64::try_from(signed.signature_profile).expect("nonnegative signature profile"),
    );
    push_length_field(&mut raw, 1, signed.envelope_schema_revision.as_bytes());
    raw
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct CapturedRequest {
    stream: String,
    consumer: String,
    subject: String,
    reply: String,
    stream_sequence: u64,
}

struct FakeRetirementState {
    response: Mutex<Result<(), CommandRetirementClientError>>,
    requests: Mutex<Vec<CapturedRequest>>,
}

struct FakeRetirementClient(Arc<FakeRetirementState>);

#[async_trait]
impl CommandRetirementClient for FakeRetirementClient {
    async fn retire_delivery(
        &self,
        request: CommandRetirementRequest,
    ) -> Result<(), CommandRetirementClientError> {
        self.0
            .requests
            .lock()
            .expect("retirement requests")
            .push(CapturedRequest {
                stream: request.stream().to_owned(),
                consumer: request.consumer().to_owned(),
                subject: request.subject().to_owned(),
                reply: request.reply().to_owned(),
                stream_sequence: request.stream_sequence(),
            });
        *self.0.response.lock().expect("retirement response")
    }
}

fn retirer(
    response: Result<(), CommandRetirementClientError>,
) -> (
    CommandRetirer<FakeRetirementClient>,
    Arc<FakeRetirementState>,
) {
    let state = Arc::new(FakeRetirementState {
        response: Mutex::new(response),
        requests: Mutex::new(Vec::new()),
    });
    let retirer = CommandRetirer::new(
        FakeRetirementClient(Arc::clone(&state)),
        CommandRetirementConfig {
            stream: STREAM.to_owned(),
            consumer: CONSUMER.to_owned(),
        },
    )
    .expect("command retirer");
    (retirer, state)
}

#[tokio::test(flavor = "current_thread")]
async fn durable_command_authority_double_acks_the_exact_verified_delivery() {
    let verified = verified("signed_command");
    let authority = terminal_authority(&verified).await;
    let (retirer, client) = retirer(Ok(()));
    let delivery = delivery("signed_command");
    let settlement = delivery.settlement();
    assert!(!settlement.retired());

    retirer
        .retire_agent_command(delivery, &verified, authority)
        .await
        .expect("confirmed double ack");

    assert!(
        settlement.retired(),
        "the runtime must see the confirmed ack"
    );
    let requests = client.requests.lock().expect("retirement requests");
    assert_eq!(requests.len(), 1);
    assert_eq!(
        requests[0],
        CapturedRequest {
            stream: STREAM.to_owned(),
            consumer: CONSUMER.to_owned(),
            subject: subject_for("outbox-1"),
            reply: REPLY.to_owned(),
            stream_sequence: 41,
        }
    );
}

#[tokio::test(flavor = "current_thread")]
async fn command_and_exact_envelope_substitution_fail_before_the_ack() {
    let original = verified("signed_command");
    let authority = terminal_authority(&original).await;
    let changed = verified("signed_command_output_session");
    let (retirer, client) = retirer(Ok(()));

    assert!(matches!(
        retirer
            .retire_agent_command(
                delivery("signed_command_output_session"),
                &changed,
                authority
            )
            .await,
        Err(CommandBusError::AuthorizationFailed(_))
    ));
    assert!(
        client
            .requests
            .lock()
            .expect("retirement requests")
            .is_empty()
    );

    let authority = terminal_authority(&original).await;
    assert!(matches!(
        retirer
            .retire_agent_command(
                delivery("signed_command_output_session"),
                &original,
                authority
            )
            .await,
        Err(CommandBusError::AuthorizationFailed(_))
    ));
    assert!(
        client
            .requests
            .lock()
            .expect("retirement requests")
            .is_empty()
    );
}

#[tokio::test(flavor = "current_thread")]
async fn a_subject_that_names_another_delivery_is_never_acked() {
    let verified = verified("signed_command");
    let authority = terminal_authority(&verified).await;
    let (retirer, client) = retirer(Ok(()));
    let misrouted = delivery_on(&subject_for("outbox-2"), bytes("signed_command"));
    let settlement = misrouted.settlement();

    assert!(matches!(
        retirer
            .retire_agent_command(misrouted, &verified, authority)
            .await,
        Err(CommandBusError::AuthorizationFailed(_))
    ));
    assert!(!settlement.retired());
    assert!(
        client
            .requests
            .lock()
            .expect("retirement requests")
            .is_empty()
    );
}

#[tokio::test(flavor = "current_thread")]
async fn same_identity_changed_intent_cannot_reuse_retirement_authority() {
    let original = verified("signed_command");
    let authority = terminal_authority(&original).await;
    let changed = verified("signed_command_same_identity_changed_intent");
    assert_eq!(original.command().command_id, changed.command().command_id);
    assert_eq!(
        original.command().idempotency_key,
        changed.command().idempotency_key
    );
    assert_ne!(
        original.command().deadline_unix_millis,
        changed.command().deadline_unix_millis
    );
    let (retirer, client) = retirer(Ok(()));

    assert!(matches!(
        retirer
            .retire_agent_command(
                delivery("signed_command_same_identity_changed_intent"),
                &changed,
                authority
            )
            .await,
        Err(CommandBusError::AuthorizationFailed(_))
    ));
    assert!(
        client
            .requests
            .lock()
            .expect("retirement requests")
            .is_empty()
    );
}

#[tokio::test(flavor = "current_thread")]
async fn verified_noncanonical_outer_envelope_retires_by_exact_bytes() {
    let canonical = bytes("signed_command");
    let raw = reordered_outer_envelope("signed_command");
    assert_ne!(raw, canonical);
    let authenticator = TestOnlyConformanceHmacAuthenticator;
    let verified = parse_and_verify_agent_command(
        &raw,
        Some(&authenticator as &dyn SignedCommandAuthenticator),
    )
    .expect("verified noncanonical command envelope");
    let authority = terminal_authority(&verified).await;
    let (retirer, client) = retirer(Ok(()));

    retirer
        .retire_agent_command(delivery_bytes(raw), &verified, authority)
        .await
        .expect("exact noncanonical delivery retirement");
    assert_eq!(
        client.requests.lock().expect("retirement requests").len(),
        1
    );
}

#[tokio::test(flavor = "current_thread")]
async fn only_a_confirmed_double_ack_retires() {
    // "Already acknowledged" is confirmed by the server like a first ack, so
    // the client reports Ok for both; anything unconfirmed stays retryable
    // and leaves the settlement marker unset (the runtime then naks).
    for (response, retryable) in [
        (CommandRetirementClientError::Timeout, true),
        (CommandRetirementClientError::DependencyUnavailable, true),
        (CommandRetirementClientError::Authentication, false),
        (CommandRetirementClientError::Protocol, false),
    ] {
        let verified = verified("signed_command");
        let authority = terminal_authority(&verified).await;
        let (retirer, _) = retirer(Err(response));
        let delivery = delivery("signed_command");
        let settlement = delivery.settlement();
        let error = retirer
            .retire_agent_command(delivery, &verified, authority)
            .await
            .expect_err("unconfirmed retirement");
        assert_eq!(error.retryable(), retryable);
        assert!(!settlement.retired());
    }
}

#[tokio::test(flavor = "current_thread")]
async fn the_retirer_binds_only_a_contract_route() {
    let state = Arc::new(FakeRetirementState {
        response: Mutex::new(Ok(())),
        requests: Mutex::new(Vec::new()),
    });
    for (stream, consumer) in [
        ("runtime.commands.v1", CONSUMER),
        (STREAM, "elitea-index-worker-v1"),
        (STREAM, "worker-1"),
    ] {
        assert!(
            CommandRetirer::new(
                FakeRetirementClient(Arc::clone(&state)),
                CommandRetirementConfig {
                    stream: stream.to_owned(),
                    consumer: consumer.to_owned(),
                },
            )
            .is_err()
        );
    }
    // A delivery from another durable of the same contract is refused before
    // the transport is asked.
    let index_retirer = CommandRetirer::new(
        FakeRetirementClient(Arc::clone(&state)),
        CommandRetirementConfig {
            stream: "ELITEA_RT_V1_INDEX".to_owned(),
            consumer: "elitea-index-worker-v1".to_owned(),
        },
    )
    .expect("index retirer");
    let verified = verified("signed_command");
    let authority = terminal_authority(&verified).await;
    assert!(matches!(
        index_retirer
            .retire_agent_command(delivery("signed_command"), &verified, authority)
            .await,
        Err(CommandBusError::InvalidInput(_))
    ));
    assert!(state.requests.lock().expect("requests").is_empty());
}

#[test]
fn delivery_decode_enforces_the_subject_ack_subject_and_complete_bounds() {
    let signed = bytes("signed_command");
    let subject = subject_for("outbox-1");
    assert!(CommandDelivery::decode(&subject, REPLY, signed.clone(), limits()).is_ok());
    // Not an ack subject, another durable's ack subject, and a subject outside
    // the stream's route.
    assert!(CommandDelivery::decode(&subject, "_INBOX.reply", signed.clone(), limits()).is_err());
    assert!(
        CommandDelivery::decode(
            &subject,
            "$JS.ACK.ELITEA_RT_V1_AGENT.other-durable.1.1.1.1700000000000000000.0",
            signed.clone(),
            limits(),
        )
        .is_err()
    );
    assert!(
        CommandDelivery::decode(
            &delivery_subject("ELITEA_RT_V1_INDEX", "outbox-1").expect("subject"),
            REPLY,
            signed.clone(),
            limits(),
        )
        .is_err()
    );
    assert!(
        CommandDelivery::decode(
            "elitea.rt.v1.agent.d.outbox-1",
            REPLY,
            signed.clone(),
            limits()
        )
        .is_err()
    );
    assert!(matches!(
        CommandDelivery::decode(&subject, REPLY, Vec::new(), limits()),
        Err(CommandBusError::ResourceExhausted(_))
    ));
    assert!(matches!(
        CommandDelivery::decode(
            &subject,
            REPLY,
            signed,
            CommandBusLimits {
                max_message_bytes: 64,
                max_payload_bytes: 8,
            },
        ),
        Err(CommandBusError::ResourceExhausted(_))
    ));
}
