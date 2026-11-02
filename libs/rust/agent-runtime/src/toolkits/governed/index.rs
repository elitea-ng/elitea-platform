//! Governed credentials of one index run (ADR-0039, issue #1473).
//!
//! A governed `index.ingest.v1` run reads one toolkit's source. It never
//! trusts a credential in its claimed toolkit settings. Before it reads the
//! source, it redeems the toolkit's credential ONCE through the run's
//! [`CredentialRedeemer`] (`RuntimeContentService.RedeemCredential`, under
//! the live claim and the mandate the index schedule names). The redemption
//! binds the provider's index operation ([`index_operation`]), an empty
//! resource (the run reads the whole source the arguments select) and the
//! digest of the `index_data` arguments. The envelope is checked against the
//! toolkit's configured target, its expiry and the adapter's material rule
//! before any provider I/O, exactly as a tool call's is
//! ([`GovernedOperationClient::redeem_checked`]). The settings then carry the
//! envelope's material as their only credential ([`index_settings`]).
//!
//! The run is the invocation: the material serves the one run it was
//! redeemed for, and the claim's lease bounds the run. A mandate revoked
//! during the run cancels the run at the next lease renewal.
//!
//! An index run has no user in front of it, so a redemption that asks for
//! the user's consent (`AUTHORIZATION_REQUIRED`) is a refusal.

use std::sync::Arc;

use serde_json::{Map, Value};

use super::adapters::with_credential;
use super::client::{Redeemed, credential_reference_id};
use super::{
    AuthAdapter, BoundOperation, BoundToolInvocation, CredentialClass, CredentialEnvelope,
    GovernedError, GovernedOperationClient, admit_material,
};

/// The tool call id an index run's redemption names: the run has no model
/// tool call.
pub const INDEX_TOOL_CALL_ID: &str = "index.ingest";

/// The catalogue operation an index run of `provider_type` performs: the
/// provider's search or read operation. `None` for a provider whose index
/// source has no governed adapter.
#[must_use]
pub fn index_operation(provider_type: &str) -> Option<&'static str> {
    match provider_type {
        "jira" => Some("jira.issue.search"),
        "confluence" => Some("confluence.page.search"),
        "qtest" => Some("qtest.test_case.search"),
        "sharepoint" => Some("sharepoint.file.read"),
        _ => None,
    }
}

/// The keys an Atlassian configuration may carry a credential under.
const ATLASSIAN_CREDENTIAL_KEYS: [&str; 4] = ["token", "api_key", "username", "password"];

/// `settings` with the envelope's material as the index source's only
/// credential, in the shape the index connector of `provider_type` reads.
///
/// # Errors
///
/// [`GovernedError::ToolkitNotGovernable`] for a provider with no index
/// mapping; [`GovernedError::ClassNotSupported`] or
/// [`GovernedError::MaterialNotAllowed`] for material the adapter refuses;
/// [`GovernedError::InvalidConfiguration`] when the settings have no
/// configuration object.
pub fn index_settings(
    adapter: &dyn AuthAdapter,
    settings: &Map<String, Value>,
    envelope: &CredentialEnvelope,
) -> Result<Map<String, Value>, GovernedError> {
    let class = admit_material(adapter, envelope)?;
    let missing = || GovernedError::MaterialNotAllowed;
    match adapter.provider_type() {
        provider @ ("jira" | "confluence") => {
            let configuration = format!("{provider}_configuration");
            match class {
                CredentialClass::StaticSecret => {
                    let token = envelope.material("token").ok_or_else(missing)?;
                    match envelope.material("username") {
                        Some(username) => with_credential(
                            settings,
                            &configuration,
                            &ATLASSIAN_CREDENTIAL_KEYS,
                            &[("username", username), ("api_key", token)],
                        ),
                        None => with_credential(
                            settings,
                            &configuration,
                            &ATLASSIAN_CREDENTIAL_KEYS,
                            &[("token", token)],
                        ),
                    }
                }
                CredentialClass::OauthDelegated => {
                    let token = envelope.material("access_token").ok_or_else(missing)?;
                    with_credential(
                        settings,
                        &configuration,
                        &ATLASSIAN_CREDENTIAL_KEYS,
                        &[("token", token)],
                    )
                }
            }
        }
        "qtest" => {
            let token = envelope.material("token").ok_or_else(missing)?;
            with_credential(
                settings,
                "qtest_configuration",
                &["qtest_api_token", "token"],
                &[("qtest_api_token", token)],
            )
        }
        "sharepoint" => {
            // The index connector uses a configured access token before any
            // client credential: the app secret is removed, so the
            // envelope's delegated token is the only credential.
            let token = envelope.material("access_token").ok_or_else(missing)?;
            with_credential(
                settings,
                "sharepoint_configuration",
                &["client_secret", "access_token"],
                &[("access_token", token)],
            )
        }
        _ => Err(GovernedError::ToolkitNotGovernable),
    }
}

/// The inputs of one index run's redemption.
pub struct IndexCredentialSpec<'a> {
    /// The toolkit's type (`jira`, `confluence`, ...).
    pub toolkit_type: &'a str,
    /// The toolkit's name, for a consent requirement's text.
    pub toolkit_name: &'a str,
    /// The toolkit id Main froze the credential for.
    pub toolkit_id: i64,
    /// The claimed toolkit settings. A credential they carry is removed.
    pub settings: &'a Map<String, Value>,
    /// The `index_data` arguments, as the run reads them. Their digest is
    /// the invocation's.
    pub arguments: &'a Value,
}

impl GovernedOperationClient {
    /// Redeem the index run's toolkit credential once and answer the
    /// settings that carry it as their only credential.
    ///
    /// # Errors
    ///
    /// [`GovernedError::ToolkitNotGovernable`] when no adapter or no index
    /// operation governs the type, or the toolkit id is not positive; the
    /// redemption's [`GovernedError`] (a denial, an unavailable broker, a
    /// target, expiry or material refusal); a consent requirement is
    /// [`GovernedError::CredentialDenied`] with `AUTHORIZATION_REQUIRED`.
    pub async fn redeem_index_settings(
        &self,
        spec: &IndexCredentialSpec<'_>,
    ) -> Result<Map<String, Value>, GovernedError> {
        let adapter: Arc<dyn AuthAdapter> = Arc::clone(
            self.registry()
                .adapter(spec.toolkit_type)
                .ok_or(GovernedError::ToolkitNotGovernable)?,
        );
        let operation_id =
            index_operation(spec.toolkit_type).ok_or(GovernedError::ToolkitNotGovernable)?;
        if spec.toolkit_id <= 0 {
            return Err(GovernedError::ToolkitNotGovernable);
        }
        let target = adapter.configured_target(spec.settings)?;
        let operation = BoundOperation::new(operation_id, String::new(), spec.arguments)
            .map_err(|_| GovernedError::OperationNotMapped)?;
        let invocation = BoundToolInvocation {
            invocation_id: self.invocation_id()?,
            tool_call_id: INDEX_TOOL_CALL_ID.to_owned(),
            toolkit_id: spec.toolkit_id,
            operation_id: operation.operation_id().to_owned(),
            resource_ref: String::new(),
            arguments_digest: *operation.arguments_digest(),
            credential_reference_id: credential_reference_id(spec.settings),
        };
        let envelope = match self
            .redeem_checked(adapter.as_ref(), spec.toolkit_name, &target, invocation)
            .await?
        {
            Redeemed::Envelope(envelope) => envelope,
            Redeemed::Authorization(_) => {
                return Err(GovernedError::CredentialDenied(
                    "AUTHORIZATION_REQUIRED".to_owned(),
                ));
            }
        };
        index_settings(adapter.as_ref(), spec.settings, &envelope)
    }
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;
    use std::sync::Mutex;

    use async_trait::async_trait;
    use serde_json::json;
    use zeroize::Zeroizing;

    use super::*;
    use crate::toolkits::governed::adapters::reference_registry;
    use crate::toolkits::governed::{
        CredentialRedeemDenial, CredentialRedeemer, EnvelopeKind, RedeemFailure, RedeemOutcome,
    };

    const TARGET: &str = "https://acme.atlassian.net";

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

    fn material(kind: EnvelopeKind, values: &[(&str, &str)], target: &str) -> CredentialEnvelope {
        CredentialEnvelope::new(
            "redemption-1".to_owned(),
            kind,
            values
                .iter()
                .map(|(key, value)| ((*key).to_owned(), Zeroizing::new((*value).to_owned())))
                .collect::<BTreeMap<_, _>>(),
            i64::MAX,
            target.to_owned(),
        )
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

    fn object(value: &Value) -> Map<String, Value> {
        value.as_object().cloned().expect("object")
    }

    fn jira_settings() -> Map<String, Value> {
        object(&json!({"jira_configuration": {
            "base_url": "https://acme.atlassian.net/",
            "username": "claim-time-user",
            "api_key": "claim-time-secret",
        }}))
    }

    fn spec<'a>(
        toolkit_type: &'a str,
        settings: &'a Map<String, Value>,
        arguments: &'a Value,
    ) -> IndexCredentialSpec<'a> {
        IndexCredentialSpec {
            toolkit_type,
            toolkit_name: "jira-prod",
            toolkit_id: 21,
            settings,
            arguments,
        }
    }

    #[tokio::test]
    async fn the_run_redeems_once_and_the_envelope_is_the_only_credential() {
        let (client, recording) = client(Box::new(|| {
            Ok(RedeemOutcome::Envelope(material(
                EnvelopeKind::Static,
                &[("token", "redeemed-token"), ("username", "bot@acme")],
                TARGET,
            )))
        }));
        let settings = jira_settings();
        let arguments = json!({"index_name": "docs", "jql": "project = DOC"});
        let redeemed = client
            .redeem_index_settings(&spec("jira", &settings, &arguments))
            .await
            .expect("redeemed");
        let configuration = &redeemed["jira_configuration"];
        assert_eq!(configuration["username"], "bot@acme");
        assert_eq!(configuration["api_key"], "redeemed-token");
        assert_eq!(configuration["base_url"], "https://acme.atlassian.net/");
        assert!(
            !Value::Object(redeemed.clone())
                .to_string()
                .contains("claim-time-secret")
        );
        let calls = recording.calls.lock().expect("calls").clone();
        assert_eq!(calls.len(), 1);
        let call = &calls[0];
        assert_eq!(call.operation_id, "jira.issue.search");
        assert_eq!(call.tool_call_id, INDEX_TOOL_CALL_ID);
        assert_eq!(call.toolkit_id, 21);
        assert_eq!(call.resource_ref, "");
        assert_eq!(call.credential_reference_id, "");
        let expected =
            BoundOperation::new("jira.issue.search", String::new(), &arguments).expect("bound");
        assert_eq!(&call.arguments_digest, expected.arguments_digest());
    }

    #[tokio::test]
    async fn a_denial_or_a_consent_requirement_refuses_the_run() {
        for code in ["DENIED", "AUTHORIZATION_REQUIRED"] {
            let (client, _) = client(Box::new(move || {
                Ok(RedeemOutcome::Denial(CredentialRedeemDenial::new(
                    code,
                    "decision-1".to_owned(),
                    "car_reference".to_owned(),
                )))
            }));
            let settings = jira_settings();
            let arguments = json!({"index_name": "docs"});
            let error = client
                .redeem_index_settings(&spec("jira", &settings, &arguments))
                .await
                .expect_err("refused");
            assert_eq!(error, GovernedError::CredentialDenied(code.to_owned()));
        }
    }

    #[tokio::test]
    async fn an_envelope_for_another_target_is_refused() {
        let (client, _) = client(Box::new(|| {
            Ok(RedeemOutcome::Envelope(material(
                EnvelopeKind::Static,
                &[("token", "t")],
                "https://other.example",
            )))
        }));
        let settings = jira_settings();
        let arguments = json!({"index_name": "docs"});
        let error = client
            .redeem_index_settings(&spec("jira", &settings, &arguments))
            .await
            .expect_err("refused");
        assert_eq!(error, GovernedError::TargetMismatch);
    }

    #[tokio::test]
    async fn an_ungoverned_source_is_refused_before_any_redemption() {
        let (client, recording) = client(Box::new(|| Err(RedeemFailure::new("unexpected", false))));
        let settings = object(&json!({"github_configuration": {"access_token": "x"}}));
        let arguments = json!({"index_name": "docs"});
        for toolkit_type in ["github", "aha"] {
            let error = client
                .redeem_index_settings(&spec(toolkit_type, &settings, &arguments))
                .await
                .expect_err("refused");
            assert_eq!(error, GovernedError::ToolkitNotGovernable);
        }
        assert!(recording.calls.lock().expect("calls").is_empty());
    }

    #[test]
    fn each_index_family_gets_the_material_its_connector_reads() {
        let registry = reference_registry();
        let qtest = object(&json!({"qtest_configuration": {
            "base_url": "https://q.example", "qtest_api_token": "old",
        }}));
        let settings = index_settings(
            registry.adapter("qtest").expect("qtest").as_ref(),
            &qtest,
            &material(
                EnvelopeKind::Static,
                &[("token", "new")],
                "https://q.example",
            ),
        )
        .expect("qtest");
        assert_eq!(settings["qtest_configuration"]["qtest_api_token"], "new");

        let sharepoint = object(&json!({"sharepoint_configuration": {
            "site_url": "https://t.sharepoint.com/sites/a", "client_id": "c", "client_secret": "s",
        }}));
        let settings = index_settings(
            registry.adapter("sharepoint").expect("sharepoint").as_ref(),
            &sharepoint,
            &material(
                EnvelopeKind::OauthAccess,
                &[("access_token", "delegated")],
                "https://t.sharepoint.com/sites/a",
            ),
        )
        .expect("sharepoint");
        let configuration = &settings["sharepoint_configuration"];
        assert_eq!(configuration["access_token"], "delegated");
        assert!(configuration.get("client_secret").is_none());
    }
}
