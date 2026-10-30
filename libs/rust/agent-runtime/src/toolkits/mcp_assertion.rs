//! The remote MCP session client that carries the run's delegation
//! assertion (ADR-0044 decision 4) on every request.
//!
//! An assertion lives at most 5 minutes and a session can live longer, so
//! the header is set per request from the run's cache, which mints a new
//! assertion shortly before the old one expires. A run that requires
//! assertions fails the request when none can be had; any other run sends it
//! without one.

use std::collections::HashMap;
use std::sync::Arc;

use adk_tool::mcp::rmcp::model::ClientJsonRpcMessage;
use adk_tool::mcp::rmcp::transport::common::client_side_sse::BoxedSseResponse;
use adk_tool::mcp::rmcp::transport::streamable_http_client::{
    StreamableHttpClient, StreamableHttpError, StreamableHttpPostResponse,
};
use http::{HeaderName, HeaderValue};

use crate::delegation::DelegationAssertions;
#[cfg(doc)]
use crate::delegation::HEADER_NAME;

type ClientError = StreamableHttpError<reqwest_mcp::Error>;

/// `reqwest_mcp::Client`, plus the run's delegation assertion on every
/// request when the run has assertions.
#[derive(Clone)]
pub(crate) struct AssertingClient {
    inner: reqwest_mcp::Client,
    assertions: Option<(Arc<DelegationAssertions>, Arc<str>)>,
}

impl AssertingClient {
    /// A client that sends no assertion.
    pub(crate) const fn plain(inner: reqwest_mcp::Client) -> Self {
        Self {
            inner,
            assertions: None,
        }
    }

    /// A client that sends the assertion for `endpoint` on every request.
    pub(crate) fn new(
        inner: reqwest_mcp::Client,
        assertions: Arc<DelegationAssertions>,
        endpoint: &str,
    ) -> Self {
        Self {
            inner,
            assertions: Some((assertions, Arc::from(endpoint))),
        }
    }

    /// `custom_headers` with the assertion added.
    async fn with_assertion(
        &self,
        mut custom_headers: HashMap<HeaderName, HeaderValue>,
    ) -> Result<HashMap<HeaderName, HeaderValue>, ClientError> {
        let Some((assertions, endpoint)) = &self.assertions else {
            return Ok(custom_headers);
        };
        let token = assertions
            .assertion_for(endpoint)
            .await
            .map_err(|_| unavailable())?;
        if let Some(token) = token {
            let mut value = HeaderValue::from_str(&token).map_err(|_| unavailable())?;
            value.set_sensitive(true);
            custom_headers.insert(ASSERTION_HEADER, value);
        }
        Ok(custom_headers)
    }
}

/// [`HEADER_NAME`] in the lower case the `http` crate stores (HTTP header
/// names are case insensitive).
pub(crate) const ASSERTION_HEADER: HeaderName = HeaderName::from_static("elitea-agent-assertion");

fn unavailable() -> ClientError {
    StreamableHttpError::UnexpectedServerResponse(crate::delegation::ASSERTION_UNAVAILABLE.into())
}

impl StreamableHttpClient for AssertingClient {
    type Error = reqwest_mcp::Error;

    async fn post_message(
        &self,
        uri: Arc<str>,
        message: ClientJsonRpcMessage,
        session_id: Option<Arc<str>>,
        auth_header: Option<String>,
        custom_headers: HashMap<HeaderName, HeaderValue>,
    ) -> Result<StreamableHttpPostResponse, ClientError> {
        let headers = self.with_assertion(custom_headers).await?;
        self.inner
            .post_message(uri, message, session_id, auth_header, headers)
            .await
    }

    async fn post_message_with_max_sse_event_size(
        &self,
        uri: Arc<str>,
        message: ClientJsonRpcMessage,
        session_id: Option<Arc<str>>,
        auth_header: Option<String>,
        custom_headers: HashMap<HeaderName, HeaderValue>,
        max_sse_event_size: usize,
    ) -> Result<StreamableHttpPostResponse, ClientError> {
        let headers = self.with_assertion(custom_headers).await?;
        self.inner
            .post_message_with_max_sse_event_size(
                uri,
                message,
                session_id,
                auth_header,
                headers,
                max_sse_event_size,
            )
            .await
    }

    async fn delete_session(
        &self,
        uri: Arc<str>,
        session_id: Arc<str>,
        auth_header: Option<String>,
        custom_headers: HashMap<HeaderName, HeaderValue>,
    ) -> Result<(), ClientError> {
        let headers = self.with_assertion(custom_headers).await?;
        self.inner
            .delete_session(uri, session_id, auth_header, headers)
            .await
    }

    async fn get_stream(
        &self,
        uri: Arc<str>,
        session_id: Option<Arc<str>>,
        last_event_id: Option<String>,
        auth_header: Option<String>,
        custom_headers: HashMap<HeaderName, HeaderValue>,
    ) -> Result<BoxedSseResponse, ClientError> {
        let headers = self.with_assertion(custom_headers).await?;
        self.inner
            .get_stream(uri, session_id, last_event_id, auth_header, headers)
            .await
    }

    async fn get_stream_with_max_sse_event_size(
        &self,
        uri: Arc<str>,
        session_id: Option<Arc<str>>,
        last_event_id: Option<String>,
        auth_header: Option<String>,
        custom_headers: HashMap<HeaderName, HeaderValue>,
        max_sse_event_size: usize,
    ) -> Result<BoxedSseResponse, ClientError> {
        let headers = self.with_assertion(custom_headers).await?;
        self.inner
            .get_stream_with_max_sse_event_size(
                uri,
                session_id,
                last_event_id,
                auth_header,
                headers,
                max_sse_event_size,
            )
            .await
    }
}

#[cfg(test)]
#[path = "mcp_assertion_tests.rs"]
mod tests;
