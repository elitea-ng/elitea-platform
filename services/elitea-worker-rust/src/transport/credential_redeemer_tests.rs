//! The governed redemption transport (#1120): `RedeemCredential` under the
//! live claim, and the governed client the worker installs per turn.

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, ToolContext};
use elitea_agent_runtime::toolkits::governed::client::GovernedToolkitSpec;
use elitea_agent_runtime::toolkits::governed::{
    BoundToolInvocation, CredentialRedeemer, RedeemOutcome,
};
use serde_json::json;
use tonic::{Request, Response, Status};

use super::credential_redeemer::{ClaimCredentialRedeemer, governed_client};
use super::input_content::content_fake::{ContentFake, claim_of};
use super::platform_client::PlatformClient;
use super::runtime_context::{RuntimeContextClient, RuntimeContextConfig};
use crate::protocol::control::test_runtime_context_authority;
use crate::protocol::elitea::runtime::v1 as pb;
use crate::toolkits::ToolAdmissionPolicy;

const DIGEST: [u8; 32] = [7; 32];
const TARGET: &str = "https://jira.example.test";

#[derive(Default)]
struct Fake {
    token_calls: AtomicUsize,
    redeemed: Mutex<Vec<pb::RedeemCredentialRequestV1>>,
    claims: Mutex<Vec<(String, Vec<u8>)>>,
    deny: Option<&'static str>,
}

#[tonic::async_trait]
impl ContentFake for Arc<Fake> {
    async fn get_elitea_client_token(
        &self,
        _request: Request<pb::GetEliteaClientTokenRequestV1>,
    ) -> Result<Response<pb::GetEliteaClientTokenResponseV1>, Status> {
        self.token_calls.fetch_add(1, Ordering::SeqCst);
        Err(Status::permission_denied("not on a governed toolkit call"))
    }

    async fn redeem_credential(
        &self,
        request: Request<pb::RedeemCredentialRequestV1>,
    ) -> Result<Response<pb::RedeemCredentialResponseV1>, Status> {
        self.claims.lock().expect("claims").push(claim_of(&request));
        self.redeemed
            .lock()
            .expect("redeemed")
            .push(request.into_inner());
        let result = match self.deny {
            Some(code) => {
                pb::redeem_credential_response_v1::Result::Denial(pb::CredentialRedeemDenialV1 {
                    code: code.to_owned(),
                    decision_id: "decision-1".to_owned(),
                    authorization_reference: "authz-ref".to_owned(),
                })
            }
            None => pb::redeem_credential_response_v1::Result::Envelope(pb::CredentialEnvelopeV1 {
                redemption_id: "redemption-1".to_owned(),
                kind: pb::CredentialEnvelopeKindV1::OauthAccess as i32,
                material: [("access_token".to_owned(), "envelope-token".to_owned())].into(),
                not_after_unix_millis: 4_102_444_800_000,
                target_identity: TARGET.to_owned(),
                obligations: Vec::new(),
                decision_id: "decision-1".to_owned(),
            }),
        };
        Ok(Response::new(pb::RedeemCredentialResponseV1 {
            result: Some(result),
        }))
    }
}

fn platform(fake: &Arc<Fake>) -> Arc<PlatformClient> {
    let config = RuntimeContextConfig {
        origin: "https://CONTENT.internal:443".to_owned(),
        deadline: Duration::from_secs(5),
        max_response_bytes: 32 * 1024,
        max_application_response_bytes: 1024 * 1024,
        max_attachment_response_bytes: 1024 * 1024,
        max_artifact_response_bytes: 2 * 1024 * 1024,
    };
    Arc::new(PlatformClient::new(Arc::new(
        RuntimeContextClient::with_rpc(Arc::clone(fake), config).expect("client"),
    )))
}

fn invocation() -> BoundToolInvocation {
    BoundToolInvocation {
        invocation_id: "0190a0b0-0000-7000-8000-000000000001".to_owned(),
        tool_call_id: "call-1".to_owned(),
        toolkit_id: 31,
        operation_id: "jira.issue.read".to_owned(),
        resource_ref: "jira:issue/ENG-7".to_owned(),
        arguments_digest: [3; 32],
        credential_reference_id: String::new(),
    }
}

#[tokio::test]
async fn a_redemption_carries_the_claim_scope_and_security_context_digest() {
    let fake = Arc::new(Fake::default());
    let authority = Arc::new(test_runtime_context_authority().governed_for_test(DIGEST));
    let redeemer = ClaimCredentialRedeemer::new(platform(&fake), Arc::clone(&authority));
    let RedeemOutcome::Envelope(envelope) = redeemer.redeem(invocation()).await.expect("redeemed")
    else {
        panic!("an envelope");
    };
    assert_eq!(envelope.material("access_token"), Some("envelope-token"));
    assert_eq!(envelope.target_identity(), TARGET);
    assert!(!format!("{envelope:?}").contains("envelope-token"));
    let requests = fake.redeemed.lock().expect("redeemed").clone();
    assert_eq!(requests.len(), 1);
    let request = &requests[0];
    assert_eq!(
        request
            .security_context_digest
            .as_ref()
            .expect("digest")
            .value,
        DIGEST
    );
    let scope = request.scope.as_ref().expect("scope");
    assert_eq!(scope.execution_id, "execution-one");
    assert_eq!(scope.generation, 2);
    let sent = request.invocation.as_ref().expect("invocation");
    assert_eq!(sent.operation_id, "jira.issue.read");
    assert_eq!(sent.resource_ref, "jira:issue/ENG-7");
    assert_eq!(sent.toolkit_id, 31);
    assert_eq!(
        sent.arguments_digest
            .as_ref()
            .expect("arguments digest")
            .value,
        [3; 32]
    );
    let claims = fake.claims.lock().expect("claims").clone();
    assert_eq!(claims, [("claim-1".to_owned(), vec![b'f'; 32])]);
}

#[tokio::test]
async fn a_legacy_command_installs_no_governed_client_and_never_redeems() {
    let fake = Arc::new(Fake::default());
    let platform = platform(&fake);
    let legacy = Arc::new(test_runtime_context_authority());
    assert!(governed_client(&platform, &legacy).is_none());
    let failure = ClaimCredentialRedeemer::new(Arc::clone(&platform), legacy)
        .redeem(invocation())
        .await
        .expect_err("not governed");
    assert_eq!(failure.code(), "governed.not_governed");
    assert_eq!(fake.redeemed.lock().expect("redeemed").len(), 0);
}

#[tokio::test]
async fn a_denial_is_a_typed_outcome_with_its_stable_code() {
    let fake = Arc::new(Fake {
        deny: Some("AUTHORIZATION_REQUIRED"),
        ..Fake::default()
    });
    let authority = Arc::new(test_runtime_context_authority().governed_for_test(DIGEST));
    let RedeemOutcome::Denial(denial) = ClaimCredentialRedeemer::new(platform(&fake), authority)
        .redeem(invocation())
        .await
        .expect("answered")
    else {
        panic!("a denial");
    };
    assert_eq!(denial.code(), "AUTHORIZATION_REQUIRED");
    assert_eq!(denial.authorization_reference(), Some("authz-ref"));
}

/// A governed toolkit call redeems once through `RedeemCredential` and
/// never reaches the execution-actor token route: no toolkit sees the PAT.
#[tokio::test]
async fn a_governed_toolkit_call_redeems_once_and_never_asks_for_the_client_token() {
    let fake = Arc::new(Fake::default());
    let authority = Arc::new(test_runtime_context_authority().governed_for_test(DIGEST));
    let client = governed_client(&platform(&fake), &authority).expect("a governed command");
    let settings = json!({"jira_configuration": {
        "base_url": TARGET, "token": "claim-time-secret"}})
    .as_object()
    .cloned()
    .expect("settings");
    let policy = Arc::new(
        ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new()).expect("policy"),
    );
    let toolset = client
        .toolset(&GovernedToolkitSpec::new(
            "tracker",
            "jira",
            Some(31),
            &settings,
            &policy,
        ))
        .expect("governed jira");
    let context: Arc<dyn ToolContext> =
        Arc::new(SimpleToolContext::new("governed").with_function_call_id("call-1"));
    let readonly: Arc<dyn ReadonlyContext> = context.clone();
    let tool = toolset
        .tools(readonly)
        .await
        .expect("tools")
        .into_iter()
        .find(|tool| tool.name() == "list_comments")
        .expect("list_comments");
    // Malformed arguments: the family refuses them after the redemption,
    // before any provider I/O.
    let error = tool
        .execute(context, json!({"issue_key": ["ENG-7"]}))
        .await
        .expect_err("the family refuses the arguments");
    assert!(!error.code.starts_with("governed."), "{}", error.code);
    assert_eq!(fake.redeemed.lock().expect("redeemed").len(), 1);
    assert_eq!(fake.token_calls.load(Ordering::SeqCst), 0);
}
