//! A governed index run redeems its toolkit credential once and reads the
//! source with that credential only (#1473); a legacy run is unchanged.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use elitea_agent_runtime::toolkits::governed::adapters::reference_registry;
use elitea_agent_runtime::toolkits::governed::{
    BoundToolInvocation, CredentialEnvelope, CredentialRedeemDenial, CredentialRedeemer,
    EnvelopeKind, GovernedOperationClient, RedeemFailure, RedeemOutcome,
};
use serde_json::{Value, json};
use zeroize::Zeroizing;

use super::governed::governed_toolkit_configuration;
use crate::protocol::output::RuntimeFailureKind;

type Respond = dyn Fn() -> Result<RedeemOutcome, RedeemFailure> + Send + Sync;

struct Recording {
    calls: Mutex<Vec<BoundToolInvocation>>,
    respond: Box<Respond>,
}

#[async_trait]
impl CredentialRedeemer for Recording {
    async fn redeem(
        &self,
        invocation: BoundToolInvocation,
    ) -> Result<RedeemOutcome, RedeemFailure> {
        self.calls.lock().expect("calls").push(invocation);
        (self.respond)()
    }
}

fn client(respond: Box<Respond>) -> (GovernedOperationClient, Arc<Recording>) {
    let recording = Arc::new(Recording {
        calls: Mutex::new(Vec::new()),
        respond,
    });
    let redeemer: Arc<dyn CredentialRedeemer> = recording.clone();
    (
        GovernedOperationClient::new(Arc::new(reference_registry()), redeemer),
        recording,
    )
}

fn redeemed_token() -> Result<RedeemOutcome, RedeemFailure> {
    Ok(RedeemOutcome::Envelope(CredentialEnvelope::new(
        "redemption-1".to_owned(),
        EnvelopeKind::Static,
        BTreeMap::from([(
            "token".to_owned(),
            Zeroizing::new("redeemed-token".to_owned()),
        )]),
        i64::MAX,
        "https://acme.atlassian.net".to_owned(),
    )))
}

fn confluence_entry() -> Vec<u8> {
    serde_json::to_vec(&json!({
        "id": 21,
        "type": "confluence",
        "toolkit_name": "wiki",
        "settings": {"confluence_configuration": {
            "base_url": "https://acme.atlassian.net",
            "token": "claim-time-secret",
        }, "space": "DOC"},
    }))
    .expect("entry")
}

const PARAMETERS: &[u8] = br#"{"index_name":"docs","cql":"space = DOC"}"#;

#[tokio::test]
async fn a_legacy_run_reads_its_claimed_configuration_unchanged() {
    let entry = confluence_entry();
    let configuration = governed_toolkit_configuration(None, &entry, PARAMETERS)
        .await
        .expect("legacy");
    assert!(configuration.is_none());
}

#[tokio::test]
async fn a_governed_run_reads_the_source_with_the_redeemed_credential_only() {
    let (client, recording) = client(Box::new(redeemed_token));
    let entry = confluence_entry();
    let configuration = governed_toolkit_configuration(Some(&client), &entry, PARAMETERS)
        .await
        .expect("governed")
        .expect("rewritten");
    let text = String::from_utf8(configuration.to_vec()).expect("utf-8");
    assert!(!text.contains("claim-time-secret"), "{text}");
    let parsed = crate::indexing::inputs::toolkit_configuration(&configuration).expect("entry");
    assert_eq!(parsed.toolkit_type, "confluence");
    assert_eq!(parsed.toolkit_name, "wiki");
    assert_eq!(parsed.settings["space"], "DOC");
    assert_eq!(
        parsed.settings["confluence_configuration"]["token"],
        "redeemed-token"
    );
    let calls = recording.calls.lock().expect("calls").clone();
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0].toolkit_id, 21);
    assert_eq!(calls[0].operation_id, "confluence.page.search");
}

#[tokio::test]
async fn a_refused_or_unavailable_redemption_fails_the_run_closed() {
    let entry = confluence_entry();
    for (respond, want) in [
        (
            Box::new(|| {
                Ok(RedeemOutcome::Denial(CredentialRedeemDenial::new(
                    "DENIED",
                    "decision-1".to_owned(),
                    String::new(),
                )))
            }) as Box<Respond>,
            RuntimeFailureKind::AuthorizationFailed,
        ),
        (
            Box::new(|| Err(RedeemFailure::new("unavailable", true))) as Box<Respond>,
            RuntimeFailureKind::DependencyUnavailable,
        ),
    ] {
        let (client, _) = client(respond);
        let failure = governed_toolkit_configuration(Some(&client), &entry, PARAMETERS)
            .await
            .expect_err("refused");
        assert!(failure == want, "unexpected failure kind");
    }
}

#[tokio::test]
async fn a_source_without_a_governed_adapter_is_refused_before_any_redemption() {
    let (client, recording) = client(Box::new(redeemed_token));
    let entry = serde_json::to_vec(&json!({
        "id": 3, "type": "github", "toolkit_name": "code",
        "settings": {"github_configuration": {"access_token": "claim-time-secret"}},
    }))
    .expect("entry");
    let failure = governed_toolkit_configuration(Some(&client), &entry, PARAMETERS)
        .await
        .expect_err("refused");
    assert!(failure == RuntimeFailureKind::UnsupportedCapability);
    assert!(recording.calls.lock().expect("calls").is_empty());
}

#[tokio::test]
async fn a_credential_free_toolkit_is_unchanged_and_redeems_nothing() {
    let (client, recording) = client(Box::new(redeemed_token));
    let entry = serde_json::to_vec(
        &json!({"id": 4, "type": "artifact", "toolkit_name": "files", "settings": {"bucket": "b"}}),
    )
    .expect("entry");
    let configuration = governed_toolkit_configuration(Some(&client), &entry, PARAMETERS)
        .await
        .expect("artifact");
    assert!(configuration.is_none());
    assert!(recording.calls.lock().expect("calls").is_empty());
    let _: Value = serde_json::from_slice(&entry).expect("json");
}
