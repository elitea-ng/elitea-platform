//! Governed toolkit credentials of one index run (ADR-0039, issue #1473).
//!
//! A governed `index.ingest.v1` command never uses a credential from its
//! claimed toolkit configuration. Before the run reads the source, it
//! redeems the toolkit's credential once through
//! `RuntimeContentService.RedeemCredential`, under the live claim and the
//! mandate the index schedule names
//! ([`GovernedOperationClient::redeem_index_settings`]). The run then reads
//! the source with a configuration whose only credential is the redeemed
//! one. A legacy command has no governed client: its configuration is used
//! unchanged.
//!
//! A source with no governed adapter, a denial, a consent requirement or an
//! unavailable broker fails the run closed, before any provider I/O.

use elitea_agent_runtime::toolkits::governed::index::IndexCredentialSpec;
use elitea_agent_runtime::toolkits::governed::{
    GovernedError, GovernedOperationClient, is_credential_free,
};
use serde_json::Value;
use zeroize::Zeroizing;

use crate::protocol::output::RuntimeFailureKind;

/// The toolkit configuration entry the run reads: the claimed bytes for a
/// legacy command or a credential-free toolkit (`None`), or the same entry
/// with the redeemed credential (`Some`), zeroized on drop.
///
/// # Errors
///
/// The run's failure: `InvalidInput` for a malformed entry,
/// `UnsupportedCapability` for a source a governed run cannot read,
/// `AuthorizationFailed` for a refused credential and
/// `DependencyUnavailable` when the broker did not answer.
pub(crate) async fn governed_toolkit_configuration(
    client: Option<&GovernedOperationClient>,
    configuration: &[u8],
    tool_parameters: &[u8],
) -> Result<Option<Zeroizing<Vec<u8>>>, RuntimeFailureKind> {
    let Some(client) = client else {
        return Ok(None);
    };
    let Ok(Value::Object(mut entry)) = serde_json::from_slice::<Value>(configuration) else {
        return Err(RuntimeFailureKind::InvalidInput);
    };
    let toolkit_type = entry
        .get("type")
        .and_then(Value::as_str)
        .ok_or(RuntimeFailureKind::InvalidInput)?
        .to_owned();
    if is_credential_free(&toolkit_type) {
        return Ok(None);
    }
    let toolkit_name = entry
        .get("toolkit_name")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_owned();
    let toolkit_id = entry.get("id").and_then(Value::as_i64).unwrap_or_default();
    let Some(Value::Object(settings)) = entry.remove("settings") else {
        return Err(RuntimeFailureKind::InvalidInput);
    };
    let arguments: Value =
        serde_json::from_slice(tool_parameters).map_err(|_| RuntimeFailureKind::InvalidInput)?;
    let redeemed = client
        .redeem_index_settings(&IndexCredentialSpec {
            toolkit_type: &toolkit_type,
            toolkit_name: &toolkit_name,
            toolkit_id,
            settings: &settings,
            arguments: &arguments,
        })
        .await
        .map_err(|error| refusal(&toolkit_type, &error))?;
    drop(settings);
    entry.insert("settings".to_owned(), Value::Object(redeemed));
    let encoded =
        serde_json::to_vec(&Value::Object(entry)).map_err(|_| RuntimeFailureKind::Internal)?;
    Ok(Some(Zeroizing::new(encoded)))
}

fn refusal(toolkit_type: &str, error: &GovernedError) -> RuntimeFailureKind {
    tracing::warn!(
        event = "index_ingest_governed_credential_refused",
        toolkit_type,
        reason_code = error.code(),
        "a governed index run could not redeem its toolkit credential; the run fails closed"
    );
    match error {
        GovernedError::ToolkitNotGovernable
        | GovernedError::OperationNotMapped
        | GovernedError::ClassNotSupported => RuntimeFailureKind::UnsupportedCapability,
        GovernedError::RedeemUnavailable { .. } => RuntimeFailureKind::DependencyUnavailable,
        GovernedError::InvalidConfiguration => RuntimeFailureKind::InvalidInput,
        GovernedError::CredentialDenied(_)
        | GovernedError::TargetMismatch
        | GovernedError::EnvelopeExpired
        | GovernedError::MaterialNotAllowed
        | GovernedError::ToolNotServed => RuntimeFailureKind::AuthorizationFailed,
    }
}
