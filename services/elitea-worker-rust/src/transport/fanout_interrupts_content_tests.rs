use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

use serde_json::{Value, json};

use super::*;

const FETCH_FIXTURE: &[u8] = include_bytes!(
    "../../../../libs/jsonschema/runtime/v1/fixtures/fanout-interrupt-fetch-v1.json"
);
const ACK_FIXTURE: &[u8] = include_bytes!(
    "../../../../libs/jsonschema/runtime/v1/fixtures/fanout-interrupt-ack-request-v1.json"
);
const EXECUTION: &str = "0123456789abcdef0123456789abcdef";

fn authority() -> FanoutDecisionAuthority {
    FanoutDecisionAuthority::new(EXECUTION, 1, "claim-1", &[b'f'; 32]).expect("authority")
}

fn fixture() -> Value {
    serde_json::from_slice(FETCH_FIXTURE).expect("fixture")
}

/// Recompute a decision's digest after editing it, as Main would.
fn redigest(entry: &mut Value) {
    let wire: FetchedDecisionWire = serde_json::from_value(entry.clone()).expect("wire");
    entry["decision_sha256"] = json!(decision_sha256(&wire).expect("digest"));
}

struct ReplyRpc {
    status: u16,
    reply: Value,
    operation: &'static str,
    calls: Arc<AtomicUsize>,
    body: Arc<std::sync::Mutex<Vec<u8>>>,
}

impl ReplyRpc {
    /// The HTTP-era status of a case as the gRPC status Main answers (§7.1).
    fn answer(&self) -> Result<Vec<u8>, tonic::Status> {
        match self.status {
            200 => Ok(serde_json::to_vec(&self.reply).expect("json")),
            401 | 403 => Err(tonic::Status::permission_denied("claim denied")),
            409 => Err(tonic::Status::already_exists("conflict")),
            404 => Err(tonic::Status::not_found("not found")),
            400 => Err(tonic::Status::invalid_argument("invalid")),
            _ => Err(tonic::Status::unavailable("unavailable")),
        }
    }

    fn expect_claim<T>(&self, request: &tonic::Request<T>, operation: &str) {
        self.calls.fetch_add(1, Ordering::SeqCst);
        assert_eq!(self.operation, operation);
        let (claim, fence) = super::super::content_fake::claim_of(request);
        assert_eq!(claim, "claim-1");
        assert_eq!(fence, [b'f'; 32]);
    }
}

#[tonic::async_trait]
impl super::super::content_fake::ContentFake for ReplyRpc {
    async fn fetch_execution_interrupts(
        &self,
        request: tonic::Request<crate::protocol::elitea::runtime::v1::FetchExecutionInterruptsRequestV1>,
    ) -> Result<
        tonic::Response<crate::protocol::elitea::runtime::v1::FetchExecutionInterruptsResponseV1>,
        tonic::Status,
    > {
        self.expect_claim(&request, "fetch");
        let scope = request.get_ref().scope.clone().expect("scope");
        assert_eq!(scope.execution_id, EXECUTION);
        assert_eq!(scope.generation, 1);
        Ok(tonic::Response::new(
            crate::protocol::elitea::runtime::v1::FetchExecutionInterruptsResponseV1 {
                fetch_json: self.answer()?,
            },
        ))
    }

    async fn acknowledge_execution_interrupt(
        &self,
        request: tonic::Request<
            crate::protocol::elitea::runtime::v1::AcknowledgeExecutionInterruptRequestV1,
        >,
    ) -> Result<
        tonic::Response<
            crate::protocol::elitea::runtime::v1::AcknowledgeExecutionInterruptResponseV1,
        >,
        tonic::Status,
    > {
        self.expect_claim(&request, "ack");
        *self.body.lock().expect("body") = request.get_ref().ack_json.clone();
        Ok(tonic::Response::new(
            crate::protocol::elitea::runtime::v1::AcknowledgeExecutionInterruptResponseV1 {
                outcome_json: self.answer()?,
            },
        ))
    }
}

fn make_client(
    status: u16,
    reply: Value,
    operation: &'static str,
) -> (
    InputContentClient,
    Arc<AtomicUsize>,
    Arc<std::sync::Mutex<Vec<u8>>>,
) {
    let calls = Arc::new(AtomicUsize::new(0));
    let body = Arc::new(std::sync::Mutex::new(Vec::new()));
    let client = InputContentClient::with_rpc(
        ReplyRpc {
            status,
            reply,
            operation,
            calls: Arc::clone(&calls),
            body: Arc::clone(&body),
        },
        super::super::InputContentConfig {
            origin: "https://content.invalid".into(),
            deadline: Duration::from_secs(1),
            max_materialized_bytes: 8192,
        },
    )
    .expect("client");
    (client, calls, body)
}

#[test]
fn the_fixture_digests_recompute_exactly() {
    let reply: FanoutFetchReply = serde_json::from_slice(FETCH_FIXTURE).expect("reply");
    assert_eq!(reply.decisions.len(), 2);
    for decision in &reply.decisions {
        validate_decision(decision).expect("valid fixture decision");
        assert_eq!(
            decision_sha256(decision).expect("digest"),
            decision.decision_sha256
        );
    }
}

#[test]
fn a_tampered_decision_is_refused_by_its_digest() {
    let mut value = fixture();
    value["decisions"][1]["value"] = json!("prod only");
    let reply: FanoutFetchReply = serde_json::from_value(value).expect("reply");
    assert!(validate_decision(&reply.decisions[1]).is_err());
}

#[test]
fn decision_value_limit_and_limit_plus_one() {
    for (chars, valid) in [
        (MAX_DECISION_VALUE_CHARS, true),
        (MAX_DECISION_VALUE_CHARS + 1, false),
    ] {
        let mut entry = fixture()["decisions"][1].clone();
        entry["value"] = json!("é".repeat(chars));
        redigest(&mut entry);
        let wire: FetchedDecisionWire = serde_json::from_value(entry).expect("wire");
        assert_eq!(validate_decision(&wire).is_ok(), valid, "{chars} chars");
    }
}

#[test]
fn credential_references_travel_only_with_authorize() {
    let mut entry = fixture()["decisions"][0].clone();
    entry["credential_ref"] = json!("cr_0123456789abcdef");
    redigest(&mut entry);
    let wire: FetchedDecisionWire = serde_json::from_value(entry.clone()).expect("wire");
    assert!(validate_decision(&wire).is_err(), "credential on approve");
    entry["action"] = json!("authorize");
    redigest(&mut entry);
    let wire: FetchedDecisionWire = serde_json::from_value(entry.clone()).expect("wire");
    assert!(validate_decision(&wire).is_ok());
    entry["credential_ref"] = Value::Null;
    redigest(&mut entry);
    let wire: FetchedDecisionWire = serde_json::from_value(entry).expect("wire");
    assert!(
        validate_decision(&wire).is_err(),
        "authorize without credential"
    );
}

#[test]
fn fetch_wire_rejects_unknown_and_missing_fields() {
    let mut extra = fixture();
    extra["decisions"][0]["decided_by"] = json!("u_1");
    assert!(serde_json::from_value::<FanoutFetchReply>(extra).is_err());
    let mut missing = fixture();
    missing["decisions"][0]
        .as_object_mut()
        .expect("entry")
        .remove("credential_ref");
    assert!(serde_json::from_value::<FanoutFetchReply>(missing).is_err());
}

#[tokio::test]
async fn fetch_is_claim_fenced_and_bound_to_the_execution() {
    let (client, calls, body) = make_client(200, fixture(), "fetch");
    let reply = client
        .fetch_fanout_decisions(&authority())
        .await
        .expect("fetch");
    assert_eq!(reply.decision_revision, 2);
    assert_eq!(calls.load(Ordering::SeqCst), 1, "one attempt");
    assert!(body.lock().expect("body").is_empty(), "empty fetch body");
    for (field, wrong) in [
        ("schema", json!("elitea.pipeline.fanout-interrupt-fetch.v2")),
        ("execution_id", json!("ffffffffffffffffffffffffffffffff")),
        ("generation", json!(2)),
    ] {
        let mut value = fixture();
        value[field] = wrong;
        let (client, _, _) = make_client(200, value, "fetch");
        assert!(
            client.fetch_fanout_decisions(&authority()).await.is_err(),
            "{field}"
        );
    }
}

#[tokio::test]
async fn a_claims_decision_source_fetches_under_that_claim_and_fence_only() {
    use crate::execution::fanout_decisions::FanoutDecisionClient;
    use elitea_agent_runtime::graph::fanout::runner::DecisionSource;
    let (client, calls, _) = make_client(200, fixture(), "fetch");
    let source =
        FanoutDecisionClient::for_claim(Arc::new(client), EXECUTION, 1, "claim-1", &[b'f'; 32])
            .expect("an ordinary claim of this execution");
    let batch = source.fetch().await.expect("fetch");
    assert_eq!(batch.decision_revision, 2);
    assert_eq!(batch.decisions.len(), 2);
    assert_eq!(calls.load(Ordering::SeqCst), 1, "one bounded attempt");
    // An identity outside the private route grammar never builds a source.
    for (execution, generation, claim, fence) in [
        ("execution-start", 1, "claim-1", [b'f'; 32].as_slice()),
        (EXECUTION, 0, "claim-1", [b'f'; 32].as_slice()),
        (EXECUTION, 1, "", [b'f'; 32].as_slice()),
        (EXECUTION, 1, "claim-1", [b'f'; 31].as_slice()),
    ] {
        let (client, calls, _) = make_client(200, fixture(), "fetch");
        assert!(
            FanoutDecisionClient::for_claim(Arc::new(client), execution, generation, claim, fence)
                .is_err()
        );
        assert_eq!(calls.load(Ordering::SeqCst), 0);
    }
}

#[tokio::test]
async fn fetch_entries_limit_and_limit_plus_one() {
    let entry = fixture()["decisions"][0].clone();
    for (count, valid) in [
        (MAX_FETCH_DECISIONS, true),
        (MAX_FETCH_DECISIONS + 1, false),
    ] {
        let mut value = fixture();
        value["decisions"] = Value::Array(vec![entry.clone(); count]);
        let (client, _, _) = make_client(200, value, "fetch");
        assert_eq!(
            client.fetch_fanout_decisions(&authority()).await.is_ok(),
            valid,
            "{count} entries"
        );
    }
}

#[tokio::test]
async fn refused_claims_are_lease_loss_and_other_failures_are_transient() {
    for status in [401, 403, 409] {
        let (client, _, _) = make_client(status, json!({}), "fetch");
        assert!(matches!(
            client.fetch_fanout_decisions(&authority()).await,
            Err(InputContentError::AuthorizationFailed(_))
        ));
    }
    for status in [500, 502, 503] {
        let (client, _, _) = make_client(status, json!({}), "fetch");
        assert!(matches!(
            client.fetch_fanout_decisions(&authority()).await,
            Err(InputContentError::DependencyUnavailable(_))
        ));
    }
}

fn ack_request() -> FanoutAckRequest<'static> {
    FanoutAckRequest {
        child_checkpoint_id: Some("ckpt-0002"),
        decision_sha256: "b29bdb8d311ecbc4b50cb697a73882a6ec2e9f6faf73ec0b4e272da4c71b2364",
        interrupt_key: "93ba421dbbbcc4f07e5b3366f081d1ef3ac983c422a3b9e48ed1fbe05b2d6a87",
        outcome: "applied",
        request_id: "0b7672bbbf4eb882481748fb8a0ce16c20f873b838e311a64e6442376c7a5a25",
        revision: 2,
    }
}

fn ack_reply(state: &str) -> Value {
    json!({"schema": ACK_RESULT_SCHEMA,
        "interrupt_key": "93ba421dbbbcc4f07e5b3366f081d1ef3ac983c422a3b9e48ed1fbe05b2d6a87",
        "state": state, "revision": 3, "replay": false})
}

#[tokio::test]
async fn the_ack_body_is_the_canonical_fixture_bytes() {
    let (client, _, body) = make_client(200, ack_reply("CONSUMED"), "ack");
    let FanoutAckOutcome::Recorded(reply) = client
        .ack_fanout_decision(&authority(), &ack_request())
        .await
        .expect("ack")
    else {
        panic!("expected a recorded ACK");
    };
    assert_eq!(reply.state, "CONSUMED");
    let (client, _, _) = make_client(409, json!({}), "ack");
    assert!(matches!(
        client
            .ack_fanout_decision(&authority(), &ack_request())
            .await,
        Ok(FanoutAckOutcome::AlreadyResolved)
    ));
    assert_eq!(body.lock().expect("body").as_slice(), ACK_FIXTURE);
}

#[tokio::test]
async fn ack_outcome_and_checkpoint_must_agree_and_the_reply_state_must_match() {
    let (client, calls, _) = make_client(200, ack_reply("CONSUMED"), "ack");
    let mut stale_with_checkpoint = ack_request();
    stale_with_checkpoint.outcome = "stale";
    assert!(
        client
            .ack_fanout_decision(&authority(), &stale_with_checkpoint)
            .await
            .is_err()
    );
    let mut applied_without = ack_request();
    applied_without.child_checkpoint_id = None;
    assert!(
        client
            .ack_fanout_decision(&authority(), &applied_without)
            .await
            .is_err()
    );
    assert_eq!(
        calls.load(Ordering::SeqCst),
        0,
        "refused before the network"
    );
    let (client, _, _) = make_client(200, ack_reply("SUPERSEDED"), "ack");
    assert!(
        client
            .ack_fanout_decision(&authority(), &ack_request())
            .await
            .is_err(),
        "applied answered as superseded"
    );
}

#[test]
fn the_authority_validates_and_never_prints_the_claim() {
    assert!(FanoutDecisionAuthority::new("execution-1", 1, "claim", &[0; 32]).is_err());
    assert!(FanoutDecisionAuthority::new(EXECUTION, 0, "claim", &[0; 32]).is_err());
    assert!(FanoutDecisionAuthority::new(EXECUTION, 1, "", &[0; 32]).is_err());
    assert!(FanoutDecisionAuthority::new(EXECUTION, 1, "claim", &[0; 31]).is_err());
    let printed = format!("{:?}", authority());
    assert!(!printed.contains("claim-1") && !printed.contains(EXECUTION));
}
