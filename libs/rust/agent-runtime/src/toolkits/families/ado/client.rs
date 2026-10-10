//! The Azure DevOps client lives in `elitea-connectors` (ADR-0030 decision
//! 3), shared with the indexing connector. The families keep their tool
//! layer and reach the client here; this module adds what is theirs alone:
//! the worker's transport, the `AdkError` mapping and the tool-result bound.

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use serde_json::Value;

// The families and their suites each use a subset.
#[allow(unused_imports)]
pub(crate) use elitea_connectors::ado::client::{
    AdoBody, AdoClient, AdoClientError, AdoClientErrorCode, AdoHttpResponse, AdoRequest,
    AdoRequestBody, AdoScope, AdoTransport, CLIENT_POLICY, MAX_RESPONSE_BYTES,
    invalid_configuration, invalid_input, invalid_response, resource_exhausted,
    response_bound_failure, response_shape_failure, unknown_outcome,
};

use super::config::AdoConnection;
pub(crate) use crate::toolkits::families::connector_client::IntoAdk;
use crate::toolkits::families::connector_client::transport;

/// One tool result, serialized.
pub(crate) const MAX_OUTPUT_BYTES: usize = 512 * 1_024;

/// One claim-scoped client over the worker's transport.
pub(crate) fn client(connection: AdoConnection) -> Result<AdoClient, AdoClientError> {
    let transport = transport(&CLIENT_POLICY).map_err(|_| invalid_configuration())?;
    Ok(AdoClient::new(connection, transport))
}

/// Refuse a result larger than one tool result may be.
pub(crate) fn bounded_output(value: Value) -> Result<Value, AdoClientError> {
    let encoded = serde_json::to_vec(&value).map_err(|_| invalid_response())?;
    if encoded.len() > MAX_OUTPUT_BYTES {
        return Err(resource_exhausted());
    }
    Ok(value)
}

impl IntoAdk for AdoClientError {
    fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code() {
            AdoClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "ado.configuration.invalid",
                "the Azure DevOps toolkit configuration is invalid",
            ),
            AdoClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "ado.request.invalid",
                "the Azure DevOps request is invalid",
            ),
            AdoClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "ado.authentication.failed",
                "Azure DevOps authentication failed",
            ),
            AdoClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "ado.authorization.failed",
                "Azure DevOps did not authorize the request",
            ),
            AdoClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "ado.resource.not_found",
                "the requested Azure DevOps resource was not found",
            ),
            AdoClientErrorCode::Conflict => (
                ErrorCategory::InvalidInput,
                "ado.resource.conflict",
                "the Azure DevOps request conflicts with current provider state",
            ),
            AdoClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "ado.rate_limited",
                "Azure DevOps rate limited the request",
            ),
            AdoClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "ado.timeout",
                "the Azure DevOps request timed out",
            ),
            AdoClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "ado.unavailable",
                "Azure DevOps is unavailable",
            ),
            AdoClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "ado.response.invalid",
                "Azure DevOps returned an invalid response",
            ),
            AdoClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "ado.response.resource_exhausted",
                "the Azure DevOps response exceeds the approved limit",
            ),
            AdoClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "ado.effect.unknown_outcome",
                "Azure DevOps may have applied the requested effect; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable(),
            retry_after_ms: None,
            max_attempts: None,
        })
    }
}
