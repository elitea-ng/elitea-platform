//! Claim-bound remote MCP toolsets backed by ADK-Rust's native MCP client.
//!
//! Main owns the frozen server definition. The worker validates that exact
//! authority, establishes one Streamable HTTP session that follows only a few
//! same-origin redirects and never replays a request automatically, asks ADK
//! to discover the server catalog (or reuses a TTL-bounded cached listing), and
//! wraps the resulting ADK tools with Elitea policy, metadata and result
//! bounds. Main may also claim-materialize one fixed prebuilt HTTP definition.
//! Arbitrary stdio processes remain outside this worker.

#![allow(dead_code)] // Production agent registration remains capability-gated.

use std::collections::HashSet;
use std::fmt;
use std::io::{self, Write};
use std::sync::Arc;
use std::time::Duration;

use adk_core::{
    AdkError, ErrorCategory, ErrorComponent, ReadonlyContext, Tool, ToolContext, Toolset,
};
use adk_tool::mcp::rmcp::ServiceExt as _;
use adk_tool::mcp::rmcp::transport::auth::{
    AuthorizationManager, AuthorizationMetadata, WWWAuthenticateParams,
};
use adk_tool::mcp::rmcp::transport::streamable_http_client::{
    StreamableHttpClientTransport, StreamableHttpClientTransportConfig,
};
use adk_tool::{McpToolset, SimpleToolContext};
use async_trait::async_trait;
use serde_json::{Map, Value, json};
use zeroize::Zeroizing;

use super::delegated_auth::{
    DelegatedAuthorizationCatalog, DelegatedAuthorizationRequirement, delegated_authorization_error,
};
use super::invocation::admit_materialized_toolset;
use super::mcp_error::{McpErrorRedaction, model_visible_mcp_error};
use super::mcp_tool_cache::{
    CachedMcpTool, LiveMcpTools, McpToolDescriptor, McpToolListCache, McpToolListKey,
};
use super::policy::ToolAdmissionPolicy;
use super::snapshot::{AdmittedToolSnapshot, FrozenToolKind, FrozenToolReference};

const DEFAULT_TIMEOUT_SECONDS: u64 = 300;
const MAX_TIMEOUT_SECONDS: u64 = 3_600;
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const MAX_MCP_SERVERS: usize = 32;
const MAX_ENDPOINT_BYTES: usize = 2_048;
const MAX_DISCOVERED_TOOLS: usize = 256;
const MAX_DESCRIPTION_BYTES: usize = 16 * 1_024;
const MAX_RESULT_BYTES: usize = 512 * 1_024;
const MAX_RESULT_DEPTH: usize = 64;
const MAX_RESULT_NODES: usize = 65_536;
const MAX_RESULT_STRING_BYTES: usize = 512 * 1_024;
const MAX_SSE_EVENT_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_ACCESS_TOKEN_BYTES: usize = 16 * 1_024;
const MAX_AUTH_CHALLENGE_BYTES: usize = 16 * 1_024;
const AUTH_METADATA_TIMEOUT: Duration = Duration::from_secs(30);
const MAX_AUTH_METADATA_LIST_ITEMS: usize = 64;
const MAX_AUTH_METADATA_STRING_BYTES: usize = 4 * 1_024;
const MAX_OAUTH_CLIENT_BYTES: usize = 1_024;
const MAX_STATIC_HEADERS: usize = 64;
const MAX_HEADER_NAME_BYTES: usize = 256;
const MAX_HEADER_VALUE_BYTES: usize = 16 * 1_024;
const DEFAULT_CACHE_TTL_SECONDS: u64 = 300;
const MIN_CACHE_TTL_SECONDS: u64 = 60;
const MAX_CACHE_TTL_SECONDS: u64 = 3_600;
/// A sub-path mount commonly redirects `/mcp` to `/mcp/`; more hops than this
/// is a loop or a login bounce, neither of which is an MCP endpoint.
const MAX_REDIRECT_HOPS: usize = 3;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum McpMaterializationErrorCode {
    InvalidConfiguration,
    UnsupportedAuthority,
    ResourceExhausted,
    AuthorizationRequired,
    DependencyUnavailable,
    /// The URL names the deprecated HTTP+SSE transport (`…/sse`) and the
    /// server refuses it (405/410). Retrying cannot succeed; the toolkit must
    /// be pointed at the streamable HTTP endpoint (`…/mcp`) instead.
    RetiredSseEndpoint,
    /// A confidential OAuth app (`client_id` with `client_secret`) needs a
    /// saved toolkit: Main holds the secret and finds it by the toolkit's id.
    SavedToolkitRequired,
    /// The OAuth app could not be attached to the authorization request, so
    /// signing in would silently use the wrong client. Refused, not dropped.
    OAuthAppUnattachable,
}

/// A stable, data-free remote MCP assembly failure.
#[derive(Clone)]
pub struct McpMaterializationError {
    code: McpMaterializationErrorCode,
    authorization: Option<Box<DelegatedAuthorizationRequirement>>,
}

impl McpMaterializationError {
    #[must_use]
    pub const fn code(&self) -> McpMaterializationErrorCode {
        self.code
    }

    pub fn authorization(&self) -> Option<&DelegatedAuthorizationRequirement> {
        self.authorization.as_deref()
    }
}

impl fmt::Debug for McpMaterializationError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("McpMaterializationError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for McpMaterializationError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            McpMaterializationErrorCode::InvalidConfiguration => {
                "the frozen MCP server configuration is invalid"
            }
            McpMaterializationErrorCode::UnsupportedAuthority => {
                "the frozen MCP server requires an unavailable authority"
            }
            McpMaterializationErrorCode::ResourceExhausted => {
                "the MCP server catalog exceeds its approved limit"
            }
            McpMaterializationErrorCode::AuthorizationRequired => {
                "the MCP server requires delegated authorization"
            }
            McpMaterializationErrorCode::DependencyUnavailable => "the MCP server is unavailable",
            McpMaterializationErrorCode::RetiredSseEndpoint => RETIRED_SSE_MESSAGE,
            McpMaterializationErrorCode::SavedToolkitRequired => SAVED_TOOLKIT_REQUIRED_MESSAGE,
            McpMaterializationErrorCode::OAuthAppUnattachable => OAUTH_APP_UNATTACHABLE_MESSAGE,
        })
    }
}

impl std::error::Error for McpMaterializationError {}

impl McpMaterializationError {
    /// Data-free text that is safe and useful to show for the codes whose
    /// cause the user can fix; `None` for every other code.
    #[must_use]
    pub const fn visible_message(&self) -> Option<&'static str> {
        match self.code {
            McpMaterializationErrorCode::RetiredSseEndpoint => Some(RETIRED_SSE_MESSAGE),
            McpMaterializationErrorCode::SavedToolkitRequired => {
                Some(SAVED_TOOLKIT_REQUIRED_MESSAGE)
            }
            McpMaterializationErrorCode::OAuthAppUnattachable => {
                Some(OAUTH_APP_UNATTACHABLE_MESSAGE)
            }
            _ => None,
        }
    }
}

/// Text for [`McpMaterializationErrorCode::SavedToolkitRequired`].
pub const SAVED_TOOLKIT_REQUIRED_MESSAGE: &str = "an MCP server with a client secret needs a \
     saved toolkit: save the toolkit, then run it again so the secret can be used for sign-in";

/// Text for [`McpMaterializationErrorCode::OAuthAppUnattachable`].
pub const OAUTH_APP_UNATTACHABLE_MESSAGE: &str = "the MCP server's OAuth client settings are too \
     large to send with the sign-in request; shorten the client id or scopes";

/// Data-free text for [`McpMaterializationErrorCode::RetiredSseEndpoint`],
/// shared with the assembly error mappers so every surface says the same.
pub const RETIRED_SSE_MESSAGE: &str = "the MCP server no longer serves the legacy HTTP+SSE \
     endpoint; HTTP+SSE is not supported, configure the server's streamable HTTP /mcp endpoint";

/// The public half of a toolkit's `client_id` / `client_secret` pair.
///
/// The SDK uses these only to identify the OAuth app in the authorization-code
/// flow (it never runs a client-credentials grant), and the worker follows:
/// the secret value is never read into this process.
pub(super) struct OAuthClient {
    client_id: String,
    secret_present: bool,
    configuration_uuid: Option<String>,
}

/// Validated, immutable connection settings for one direct remote MCP server.
///
/// This value intentionally has no `Debug` implementation because endpoints
/// may contain tenant-identifying paths. A continuation access token is accepted
/// only from Main's claim-fetched token map and applied to the exact frozen URL.
/// Static headers come from the toolkit (a direct `mcp` toolkit's own headers,
/// such as a PAT) or from Main's materialized prebuilt definition; both pass
/// the same validation, and transport-owned headers are refused.
pub struct RemoteMcpConfig {
    toolkit_name: String,
    toolkit_type: String,
    endpoint: String,
    timeout: Duration,
    selected_tools: Vec<String>,
    excluded_tools: Vec<String>,
    requested_scopes: Vec<String>,
    static_headers: reqwest_mcp::header::HeaderMap,
    access_token: Option<Zeroizing<String>>,
    /// The toolkit's registered OAuth app for the delegated browser flow.
    /// Public identifiers only: the secret stays in Main, which performs the
    /// code exchange and refresh, so only its presence is kept here.
    oauth_client: Option<OAuthClient>,
    /// The saved toolkit's id, stamped on the authorization signal so Main can
    /// load its stored secret and save the token against it.
    toolkit_id: Option<u64>,
    /// `None` when `enable_caching` is false: every materialization lists.
    tool_list_ttl: Option<Duration>,
}

impl RemoteMcpConfig {
    fn parse(
        reference: &FrozenToolReference<'_>,
        mcp_tokens: &Map<String, Value>,
    ) -> Result<Self, McpMaterializationError> {
        if reference.kind() != FrozenToolKind::Mcp {
            return Err(unsupported_authority());
        }
        let settings = reference.settings().ok_or_else(invalid_configuration)?;
        let prebuilt = parse_mcp_authority(reference.tool_type())?;
        let toolkit_id = reference.tool_id();
        // An internal builder's identity comes only from Main's materializer.
        let oauth_client = if reference.is_internal_builder() {
            None
        } else {
            parse_oauth_client(settings, toolkit_id.is_some())?
        };
        let requested_scopes = parse_requested_scopes(settings)?;
        let endpoint = parse_endpoint(settings)?;
        let timeout = Duration::from_secs(parse_bounded_integer(
            settings.get("timeout"),
            DEFAULT_TIMEOUT_SECONDS,
            1,
            MAX_TIMEOUT_SECONDS,
        )?);
        let selected_tools = parse_selected_tools(settings)?;
        let excluded_tools = parse_excluded_tools(settings)?;
        let static_headers = parse_static_headers(settings)?;
        let server_name = if prebuilt {
            Some(
                settings
                    .get("server_name")
                    .and_then(Value::as_str)
                    .filter(|value| valid_mcp_token_alias(value))
                    .ok_or_else(invalid_configuration)?
                    .to_owned(),
            )
        } else {
            None
        };
        let access_token = if reference.is_internal_builder() {
            // The actor credential comes from Main's claim materializer. A
            // delegated token must not replace this execution's actor identity.
            if !configured_authorization(&static_headers) {
                return Err(unsupported_authority());
            }
            None
        } else {
            resolve_access_token(
                mcp_tokens,
                &endpoint,
                reference.tool_type(),
                server_name.as_deref(),
                prebuilt,
            )?
        };
        let enable_caching = settings.get("enable_caching").map_or(Ok(true), |value| {
            value.as_bool().ok_or_else(invalid_configuration)
        })?;
        let cache_ttl = parse_bounded_integer(
            settings.get("cache_ttl"),
            DEFAULT_CACHE_TTL_SECONDS,
            MIN_CACHE_TTL_SECONDS,
            MAX_CACHE_TTL_SECONDS,
        )?;
        if settings
            .get("ssl_verify")
            .is_some_and(|value| value != &Value::Bool(true))
        {
            return Err(unsupported_authority());
        }
        Ok(Self {
            toolkit_name: reference.toolkit_name().to_owned(),
            toolkit_type: reference.tool_type().to_owned(),
            endpoint,
            timeout,
            selected_tools,
            excluded_tools,
            requested_scopes,
            static_headers,
            access_token,
            // Identity comes only from Main's materializer for an internal
            // builder; it is never offered a registered OAuth app.
            oauth_client,
            toolkit_id,
            // An internal builder's bearer is minted per execution, so its
            // listing could never be reused: caching it would only pin memory.
            tool_list_ttl: (enable_caching && !reference.is_internal_builder())
                .then(|| Duration::from_secs(cache_ttl)),
        })
    }

    /// The identity a cached tool listing is valid for: toolkit, endpoint and
    /// the exact credentials sent. A different PAT, token or header is a
    /// different listing, so one caller never sees another caller's catalog.
    fn tool_list_key(&self) -> Result<McpToolListKey, McpMaterializationError> {
        let headers = self.request_headers()?;
        let mut header_fields = headers
            .iter()
            .map(|(name, value)| (name.as_str(), value.as_bytes()))
            .collect::<Vec<_>>();
        header_fields.sort_unstable();
        let mut fields: Vec<&[u8]> = vec![
            self.toolkit_type.as_bytes(),
            self.toolkit_name.as_bytes(),
            self.endpoint.as_bytes(),
        ];
        for (name, value) in &header_fields {
            fields.push(name.as_bytes());
            fields.push(value);
        }
        Ok(McpToolListKey::from_fields(&fields))
    }

    #[must_use]
    pub(crate) fn toolkit_name(&self) -> &str {
        &self.toolkit_name
    }

    #[must_use]
    pub(crate) fn toolkit_type(&self) -> &str {
        &self.toolkit_type
    }

    #[must_use]
    pub fn endpoint(&self) -> &str {
        &self.endpoint
    }

    #[must_use]
    pub(crate) const fn timeout(&self) -> Duration {
        self.timeout
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[String] {
        &self.selected_tools
    }

    #[must_use]
    pub(crate) fn excluded_tools(&self) -> &[String] {
        &self.excluded_tools
    }

    /// A configured `Authorization` header (a PAT) wins over a delegated
    /// OAuth token, matching Load Tools in Main and the Python SDK's
    /// `setdefault`, so discovery and execution act as one identity (#6691).
    ///
    /// Only a non-blank header counts, as in Main's `mcpDiscoveryHeaders`: a
    /// blank one is replaced by the delegated token, so Load Tools and a run
    /// never disagree about which identity is used.
    fn request_headers(&self) -> Result<reqwest_mcp::header::HeaderMap, McpMaterializationError> {
        let mut headers = self.static_headers.clone();
        if configured_authorization(&headers) {
            return Ok(headers);
        }
        if let Some(token) = self.access_token() {
            headers.remove(reqwest_mcp::header::AUTHORIZATION);
            let mut bearer = Zeroizing::new(String::with_capacity("Bearer ".len() + token.len()));
            bearer.push_str("Bearer ");
            bearer.push_str(token);
            let mut value = reqwest_mcp::header::HeaderValue::from_str(&bearer)
                .map_err(|_| invalid_configuration())?;
            value.set_sensitive(true);
            headers.insert(reqwest_mcp::header::AUTHORIZATION, value);
        }
        Ok(headers)
    }

    fn access_token(&self) -> Option<&str> {
        self.access_token.as_deref().map(String::as_str)
    }

    /// The values a failed call must not echo to the model: every static
    /// header value, the delegated token, and the endpoint (demo issue 3).
    fn error_redaction(&self) -> McpErrorRedaction {
        let headers = self
            .static_headers
            .values()
            .filter_map(|value| value.to_str().ok());
        McpErrorRedaction::new(headers.chain(self.access_token()), &self.endpoint)
    }

    #[cfg(test)]
    pub(crate) fn oauth_client_for_test(&self) -> Option<&str> {
        self.oauth_client
            .as_ref()
            .map(|client| client.client_id.as_str())
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn access_token_for_test(&self) -> Option<&str> {
        self.access_token()
    }

    #[must_use]
    pub(crate) const fn tool_list_ttl(&self) -> Option<Duration> {
        self.tool_list_ttl
    }

    #[cfg(test)]
    pub(crate) fn request_headers_for_test(
        &self,
    ) -> Result<reqwest_mcp::header::HeaderMap, McpMaterializationError> {
        self.request_headers()
    }

    #[cfg(test)]
    pub(crate) fn parse_for_test(
        reference: &FrozenToolReference<'_>,
        mcp_tokens: &Map<String, Value>,
    ) -> Result<Self, McpMaterializationError> {
        Self::parse(reference, mcp_tokens)
    }

    /// Points a parsed config at a plain-HTTP test server. Production
    /// parsing refuses anything but HTTPS.
    #[cfg(test)]
    #[must_use]
    pub(crate) fn with_endpoint_for_test(mut self, endpoint: &str) -> Self {
        endpoint.clone_into(&mut self.endpoint);
        self
    }
}

/// Connection seam for ADK's MCP implementation.
///
/// Production uses [`AdkHttpMcpConnector`]. Tests may inject an in-memory ADK
/// toolset without granting network authority.
#[async_trait]
pub trait McpConnector: Send + Sync {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError>;

    /// Connect and, when the connection can call a tool by name without
    /// listing first, also return that caller. Only such a connection can
    /// serve a cached tool listing; the default never does.
    async fn connect_session(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<McpSession, McpMaterializationError> {
        Ok(McpSession {
            toolset: self.connect(config).await?,
            reuses_listings: false,
        })
    }
}

/// One established MCP connection.
pub struct McpSession {
    pub(crate) toolset: Arc<dyn Toolset>,
    /// Whether this connection may serve a cached listing: its toolset must
    /// list lazily on the session when a cached tool is first called.
    pub(crate) reuses_listings: bool,
}

/// `true` when the headers carry a non-blank `Authorization` value.
fn configured_authorization(headers: &reqwest_mcp::header::HeaderMap) -> bool {
    headers
        .get(reqwest_mcp::header::AUTHORIZATION)
        .is_some_and(|value| !value.as_bytes().iter().all(u8::is_ascii_whitespace))
}

/// The worker's runtime client identity for ONE origin: elitea-main's
/// internal platform listener, which serves the internal MCP builders
/// (`<origin>/app/{project_id}/mcp/...`) and requires a runtime-CA client
/// certificate. An MCP endpoint on that exact origin (scheme, host, port)
/// presents the certificate and trusts the runtime CA; any other endpoint
/// presents nothing, so a third-party MCP server never sees the worker's
/// workload identity.
///
/// No `Debug`: it holds a private key.
pub struct PlatformMcpIdentity {
    origin: reqwest_mcp::Url,
    roots: Vec<reqwest_mcp::Certificate>,
    identity: reqwest_mcp::Identity,
}

/// The platform origin or the PEM material is not usable. Carries nothing
/// from either.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PlatformMcpIdentityError;

impl fmt::Display for PlatformMcpIdentityError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("the platform MCP client identity is invalid")
    }
}

impl std::error::Error for PlatformMcpIdentityError {}

impl PlatformMcpIdentity {
    /// The identity for `platform_origin` (an `https://host[:port]`
    /// origin): `ca_pem` is the runtime CA bundle, `identity_pem` the
    /// workload certificate chain followed by its private key.
    ///
    /// # Errors
    ///
    /// The origin is not an HTTPS origin, or the PEM material does not
    /// parse.
    pub fn new(
        platform_origin: &str,
        ca_pem: &[u8],
        identity_pem: &[u8],
    ) -> Result<Self, PlatformMcpIdentityError> {
        let origin =
            reqwest_mcp::Url::parse(platform_origin).map_err(|_| PlatformMcpIdentityError)?;
        if origin.scheme() != "https" || origin.host_str().is_none() {
            return Err(PlatformMcpIdentityError);
        }
        let roots = reqwest_mcp::Certificate::from_pem_bundle(ca_pem)
            .map_err(|_| PlatformMcpIdentityError)?;
        if roots.is_empty() {
            return Err(PlatformMcpIdentityError);
        }
        let identity =
            reqwest_mcp::Identity::from_pem(identity_pem).map_err(|_| PlatformMcpIdentityError)?;
        Ok(Self {
            origin,
            roots,
            identity,
        })
    }

    /// Whether `endpoint` is on the platform origin: same scheme, host and
    /// effective port, and no user information.
    #[must_use]
    pub fn serves(&self, endpoint: &str) -> bool {
        reqwest_mcp::Url::parse(endpoint).is_ok_and(|url| {
            url.scheme() == self.origin.scheme()
                && url
                    .host_str()
                    .zip(self.origin.host_str())
                    .is_some_and(|(host, origin)| host.eq_ignore_ascii_case(origin))
                && url.port_or_known_default() == self.origin.port_or_known_default()
                && url.username().is_empty()
                && url.password().is_none()
        })
    }
}

/// Secure Streamable HTTP connector for ADK's native [`McpToolset`].
#[derive(Default)]
pub struct AdkHttpMcpConnector {
    platform: Option<Arc<PlatformMcpIdentity>>,
}

impl AdkHttpMcpConnector {
    #[must_use]
    pub const fn new() -> Self {
        Self { platform: None }
    }

    /// Present `platform`'s client identity to MCP endpoints on its origin
    /// (the internal MCP builders), and only to them.
    #[must_use]
    pub fn with_platform_identity(mut self, platform: Option<Arc<PlatformMcpIdentity>>) -> Self {
        self.platform = platform;
        self
    }
}

#[async_trait]
impl McpConnector for AdkHttpMcpConnector {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        self.connect_session(config)
            .await
            .map(|session| session.toolset)
    }

    async fn connect_session(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<McpSession, McpMaterializationError> {
        let client = session_client_builder(config, self.platform.as_deref())?
            .build()
            .map_err(|_| invalid_configuration())?;
        let transport_config = StreamableHttpClientTransportConfig::with_uri(config.endpoint())
            .max_sse_event_size(MAX_SSE_EVENT_BYTES)
            .reinit_on_expired_session(false);
        let transport = StreamableHttpClientTransport::with_client(client, transport_config);
        let running = match tokio::time::timeout(config.timeout(), ().serve(transport)).await {
            Ok(Ok(running)) => running,
            Ok(Err(error)) if error.is_authorization_required() => {
                return Err(
                    authorization_required_with_metadata(config, error.auth_challenge()).await,
                );
            }
            Ok(Err(error)) if retired_sse_failure(config.endpoint(), &error) => {
                return Err(retired_sse_endpoint());
            }
            Err(_) | Ok(Err(_)) => return Err(dependency_unavailable()),
        };
        Ok(McpSession {
            toolset: Arc::new(McpToolset::new(running)),
            reuses_listings: true,
        })
    }
}

/// The HTTP client every MCP session uses: HTTPS only, the request headers
/// (static headers and the bearer) as defaults, no automatic replay, and
/// redirects limited to the frozen endpoint's origin. An endpoint on the
/// platform origin also presents the worker's runtime client certificate
/// and trusts the runtime CA ([`PlatformMcpIdentity`]); the same-origin
/// redirect policy keeps that certificate on that origin. Factored out so
/// tests exercise exactly what `connect_session` builds.
pub(crate) fn session_client_builder(
    config: &RemoteMcpConfig,
    platform: Option<&PlatformMcpIdentity>,
) -> Result<reqwest_mcp::ClientBuilder, McpMaterializationError> {
    let origin = reqwest_mcp::Url::parse(config.endpoint()).map_err(|_| invalid_configuration())?;
    let mut client = reqwest_mcp::Client::builder()
        .https_only(true)
        .redirect(same_origin_redirect_policy(origin))
        .retry(reqwest_mcp::retry::never())
        .connect_timeout(CONNECT_TIMEOUT)
        .timeout(config.timeout());
    if let Some(platform) = platform_identity_for(config.endpoint(), platform) {
        client = client
            .tls_certs_merge(platform.roots.iter().cloned())
            .identity(platform.identity.clone());
    }
    let headers = config.request_headers()?;
    if !headers.is_empty() {
        client = client.default_headers(headers);
    }
    Ok(client)
}

/// The platform identity when `endpoint` is on its origin, else `None`.
pub(crate) fn platform_identity_for<'a>(
    endpoint: &str,
    platform: Option<&'a PlatformMcpIdentity>,
) -> Option<&'a PlatformMcpIdentity> {
    platform.filter(|platform| platform.serves(endpoint))
}

/// Follow at most `MAX_REDIRECT_HOPS` redirects, and only while every hop
/// keeps the frozen endpoint's scheme, host and port. The claim-bound
/// authority is the origin, so a same-origin hop (`/mcp` -> `/mcp/`) keeps it
/// and any other hop (a login page, another host) is refused.
pub(crate) fn same_origin_redirect_policy(
    origin: reqwest_mcp::Url,
) -> reqwest_mcp::redirect::Policy {
    reqwest_mcp::redirect::Policy::custom(move |attempt| {
        if redirect_allowed(&origin, attempt.url(), attempt.previous().len()) {
            attempt.follow()
        } else {
            attempt.stop()
        }
    })
}

/// `previous` counts the requests already sent, the original included.
pub(crate) fn redirect_allowed(
    origin: &reqwest_mcp::Url,
    next: &reqwest_mcp::Url,
    previous: usize,
) -> bool {
    previous <= MAX_REDIRECT_HOPS
        && next.scheme() == origin.scheme()
        && next.host_str() == origin.host_str()
        && next.port_or_known_default() == origin.port_or_known_default()
        && next.username().is_empty()
        && next.password().is_none()
}

/// A `/sse` URL refused with 405 or 410 is a retired HTTP+SSE endpoint. The
/// status is read from the transport's error chain; the text is never copied
/// anywhere.
fn retired_sse_failure(endpoint: &str, error: &(dyn std::error::Error + 'static)) -> bool {
    if !names_sse_endpoint(endpoint) {
        return false;
    }
    let mut source = Some(error);
    while let Some(current) = source {
        let text = current.to_string();
        if text.contains("HTTP 405") || text.contains("HTTP 410") {
            return true;
        }
        source = current.source();
    }
    false
}

pub(crate) fn names_sse_endpoint(endpoint: &str) -> bool {
    reqwest_mcp::Url::parse(endpoint).is_ok_and(|url| {
        url.path()
            .trim_end_matches('/')
            .to_ascii_lowercase()
            .ends_with("/sse")
    })
}

#[cfg(test)]
pub(crate) fn retired_sse_failure_for_test(
    endpoint: &str,
    error: &(dyn std::error::Error + 'static),
) -> bool {
    retired_sse_failure(endpoint, error)
}

/// Connect and discover every admitted direct remote MCP toolkit.
///
/// Discovery happens during the authorized assembly phase, matching the
/// current SDK's materialization timing. No server error text, URL, schema or
/// argument value is copied into an assembly error.
pub(crate) async fn materialize_mcp_toolsets(
    snapshot: &AdmittedToolSnapshot<'_>,
    connector: &dyn McpConnector,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<Vec<Arc<dyn Toolset>>, McpMaterializationError> {
    materialize_mcp_toolsets_with_tokens(snapshot, connector, policy, &Map::new()).await
}

/// Materialize remote MCP toolsets with claim-fetched continuation tokens.
///
/// Tokens are applied only when their canonical server URL exactly matches the
/// frozen endpoint. They are never retained in tool metadata or errors.
pub(crate) async fn materialize_mcp_toolsets_with_tokens(
    snapshot: &AdmittedToolSnapshot<'_>,
    connector: &dyn McpConnector,
    policy: &Arc<ToolAdmissionPolicy>,
    mcp_tokens: &Map<String, Value>,
) -> Result<Vec<Arc<dyn Toolset>>, McpMaterializationError> {
    materialize_mcp_toolsets_with_tokens_and_authorization(snapshot, connector, policy, mcp_tokens)
        .await
        .map(|(toolsets, _)| toolsets)
}

/// Retain toolkit authorization even when the server protects tool discovery.
/// LLM runtimes expose an authorization tool before any protected operations.
pub async fn materialize_mcp_toolsets_with_tokens_and_authorization(
    snapshot: &AdmittedToolSnapshot<'_>,
    connector: &dyn McpConnector,
    policy: &Arc<ToolAdmissionPolicy>,
    mcp_tokens: &Map<String, Value>,
) -> Result<(Vec<Arc<dyn Toolset>>, DelegatedAuthorizationCatalog), McpMaterializationError> {
    let references = snapshot
        .iter()
        .filter(|reference| reference.kind() == FrozenToolKind::Mcp)
        .collect::<Vec<_>>();
    if references.len() > MAX_MCP_SERVERS {
        return Err(resource_exhausted());
    }

    let mut toolsets = Vec::with_capacity(references.len());
    let mut authorization = DelegatedAuthorizationCatalog::default();
    for reference in references {
        let config = RemoteMcpConfig::parse(reference, mcp_tokens)?;
        let session = match connector.connect_session(&config).await {
            Ok(session) => session,
            Err(error) if error.code() == McpMaterializationErrorCode::AuthorizationRequired => {
                let requirement = error
                    .authorization()
                    .cloned()
                    .ok_or_else(invalid_configuration)?;
                if config.selected_tools().is_empty() {
                    authorization
                        .insert_discovery_requirement(requirement)
                        .map_err(|()| invalid_configuration())?;
                    continue;
                }
                let guarded_names = config
                    .selected_tools()
                    .iter()
                    .filter(|name| !contains_tool_name(config.excluded_tools(), name))
                    .collect::<Vec<_>>();
                if guarded_names.is_empty() {
                    return Err(error);
                }
                let guarded = guarded_names
                    .iter()
                    .map(|name| {
                        Arc::new(McpAuthorizationRequiredTool::new(name, requirement.clone()))
                            as Arc<dyn Tool>
                    })
                    .collect::<Vec<_>>();
                for name in guarded_names {
                    if policy.tool_decision(reference.tool_type(), name)
                        == super::policy::ToolAdmissionDecision::Allowed
                    {
                        authorization
                            .insert(name, requirement.clone())
                            .map_err(|()| invalid_configuration())?;
                    }
                }
                let admitted = admit_materialized_toolset(
                    reference.toolkit_name(),
                    reference.tool_type(),
                    policy,
                    guarded,
                )
                .map_err(|error| materialized_toolset_error(error.code()))?;
                toolsets.push(Arc::new(admitted) as Arc<dyn Toolset>);
                continue;
            }
            Err(error) => return Err(error),
        };
        let (tools, listing) = discover_tools(&config, &session).await?;
        let tools = select_tools(tools, config.selected_tools(), config.excluded_tools())?;
        let redaction = Arc::new(config.error_redaction());
        let wrapped = tools
            .into_iter()
            .map(|tool| {
                Ok(Arc::new(BoundedMcpTool::new(
                    tool,
                    reference.toolkit_name(),
                    config.timeout(),
                    Arc::clone(&redaction),
                )?) as Arc<dyn Tool>)
            })
            .collect::<Result<Vec<_>, McpMaterializationError>>()?;
        let admitted = admit_materialized_toolset(
            reference.toolkit_name(),
            reference.tool_type(),
            policy,
            wrapped,
        )
        .map_err(|error| materialized_toolset_error(error.code()))?;
        // Cached only once the listing passed selection, the per-tool bounds
        // and admission: a listing this run refused is never kept for the next.
        if let Some(listing) = listing {
            listing.store();
        }
        toolsets.push(Arc::new(admitted) as Arc<dyn Toolset>);
    }
    Ok((toolsets, authorization))
}

/// The process-wide tool-listing cache that honours `enable_caching` and
/// `cache_ttl` (#6691). It holds descriptors only, never a connection, so a
/// cached run still opens its own session and authenticates as itself.
static TOOL_LIST_CACHE: std::sync::LazyLock<McpToolListCache> =
    std::sync::LazyLock::new(McpToolListCache::default);

/// A fresh listing waiting to be cached until the run has validated it.
struct PendingListing {
    key: McpToolListKey,
    ttl: Duration,
    descriptors: Vec<McpToolDescriptor>,
}

impl PendingListing {
    fn store(self) {
        TOOL_LIST_CACHE.insert(
            self.key,
            self.descriptors,
            tokio::time::Instant::now() + self.ttl,
        );
    }
}

async fn discover_tools(
    config: &RemoteMcpConfig,
    session: &McpSession,
) -> Result<(Vec<Arc<dyn Tool>>, Option<PendingListing>), McpMaterializationError> {
    let cache = match config.tool_list_ttl() {
        Some(ttl) if session.reuses_listings => Some((ttl, config.tool_list_key()?)),
        _ => None,
    };
    if let Some((_, key)) = &cache
        && let Some(descriptors) = TOOL_LIST_CACHE.get(key, tokio::time::Instant::now())
    {
        let live = Arc::new(LiveMcpTools::new(
            Arc::clone(&session.toolset),
            config.timeout(),
        ));
        let tools = descriptors
            .iter()
            .map(|descriptor| {
                Arc::new(CachedMcpTool::new(descriptor.clone(), Arc::clone(&live))) as Arc<dyn Tool>
            })
            .collect();
        return Ok((tools, None));
    }
    let context: Arc<dyn ReadonlyContext> =
        Arc::new(SimpleToolContext::new("elitea_mcp_discovery"));
    let tools = tokio::time::timeout(config.timeout(), session.toolset.tools(context))
        .await
        .map_err(|_| dependency_unavailable())?
        .map_err(|_| dependency_unavailable())?;
    let pending = cache
        .filter(|_| tools.len() <= MAX_DISCOVERED_TOOLS)
        .map(|(ttl, key)| PendingListing {
            key,
            ttl,
            descriptors: tools
                .iter()
                .map(|tool| McpToolDescriptor::from_tool(tool.as_ref()))
                .collect(),
        });
    Ok((tools, pending))
}

#[cfg(test)]
pub(crate) fn tool_list_cache_for_test() -> &'static McpToolListCache {
    &TOOL_LIST_CACHE
}

#[cfg(test)]
pub(crate) fn tool_list_key_for_test(
    config: &RemoteMcpConfig,
) -> Result<McpToolListKey, McpMaterializationError> {
    config.tool_list_key()
}

fn materialized_toolset_error(
    code: super::invocation::MaterializedToolsetErrorCode,
) -> McpMaterializationError {
    match code {
        super::invocation::MaterializedToolsetErrorCode::InvalidDefinition => {
            invalid_configuration()
        }
        super::invocation::MaterializedToolsetErrorCode::ResourceExhausted => resource_exhausted(),
    }
}

fn select_tools(
    tools: Vec<Arc<dyn Tool>>,
    selected: &[String],
    excluded: &[String],
) -> Result<Vec<Arc<dyn Tool>>, McpMaterializationError> {
    if tools.len() > MAX_DISCOVERED_TOOLS {
        return Err(resource_exhausted());
    }
    let mut discovered_names = HashSet::with_capacity(tools.len());
    for tool in &tools {
        let canonical = tool.name().to_lowercase();
        if canonical.is_empty() || !discovered_names.insert(canonical) {
            return Err(invalid_configuration());
        }
    }
    if selected.is_empty() {
        return Ok(tools
            .into_iter()
            .filter(|tool| !contains_tool_name(excluded, tool.name()))
            .collect());
    }
    let selected = selected
        .iter()
        .map(|name| name.to_lowercase())
        .collect::<HashSet<_>>();
    if !selected.is_subset(&discovered_names) {
        return Err(invalid_configuration());
    }
    Ok(tools
        .into_iter()
        .filter(|tool| {
            selected.contains(&tool.name().to_lowercase())
                && !contains_tool_name(excluded, tool.name())
        })
        .collect())
}

fn contains_tool_name(names: &[String], candidate: &str) -> bool {
    names
        .iter()
        .any(|name| name.eq_ignore_ascii_case(candidate))
}

struct McpAuthorizationRequiredTool {
    name: Box<str>,
    description: Box<str>,
    requirement: DelegatedAuthorizationRequirement,
}

impl McpAuthorizationRequiredTool {
    fn new(name: &str, requirement: DelegatedAuthorizationRequirement) -> Self {
        Self {
            name: name.into(),
            description: format!(
                "This MCP operation is unavailable until the user authorizes the {} toolkit.",
                requirement.toolkit_name()
            )
            .into_boxed_str(),
            requirement,
        }
    }
}

#[async_trait]
impl Tool for McpAuthorizationRequiredTool {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "additionalProperties": true,
        }))
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_core::Result<Value> {
        authorization_error(&self.requirement)
    }
}

fn authorization_error(requirement: &DelegatedAuthorizationRequirement) -> adk_core::Result<Value> {
    Err(delegated_authorization_error(requirement))
}

struct BoundedMcpTool {
    inner: Arc<dyn Tool>,
    name: Box<str>,
    description: Box<str>,
    parameters_schema: Option<Value>,
    response_schema: Option<Value>,
    long_running: bool,
    read_only: bool,
    concurrency_safe: bool,
    timeout: Duration,
    redaction: Arc<McpErrorRedaction>,
}

impl BoundedMcpTool {
    fn new(
        inner: Arc<dyn Tool>,
        toolkit_name: &str,
        timeout: Duration,
        redaction: Arc<McpErrorRedaction>,
    ) -> Result<Self, McpMaterializationError> {
        let description = selection_description(inner.as_ref(), toolkit_name)?;
        Ok(Self {
            name: inner.name().into(),
            parameters_schema: inner.parameters_schema(),
            response_schema: inner.response_schema(),
            long_running: inner.is_long_running(),
            read_only: inner.is_read_only(),
            concurrency_safe: inner.is_concurrency_safe(),
            inner,
            description: description.into_boxed_str(),
            timeout,
            redaction,
        })
    }
}

#[async_trait]
impl Tool for BoundedMcpTool {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn enhanced_description(&self) -> String {
        self.description.to_string()
    }

    fn is_long_running(&self) -> bool {
        self.long_running
    }

    fn parameters_schema(&self) -> Option<Value> {
        self.parameters_schema.clone()
    }

    fn response_schema(&self) -> Option<Value> {
        self.response_schema.clone()
    }

    fn required_scopes(&self) -> &[&str] {
        self.inner.required_scopes()
    }

    fn is_read_only(&self) -> bool {
        self.read_only
    }

    fn is_concurrency_safe(&self) -> bool {
        self.concurrency_safe
    }

    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let value = tokio::time::timeout(self.timeout, self.inner.execute(context, arguments))
            .await
            .map_err(|_| mcp_timeout())?
            .map_err(|error| model_visible_mcp_error(&error, &self.name, &self.redaction))?;
        validate_result(&value).map_err(|_| invalid_mcp_result())?;
        Ok(value)
    }
}

fn selection_description(
    tool: &dyn Tool,
    toolkit_name: &str,
) -> Result<String, McpMaterializationError> {
    let effect_guidance = if tool.is_read_only() {
        "The MCP server marks this operation read-only; policy may still require authorization."
    } else {
        "This remote operation may cause effects; do not retry after an unknown outcome without reconciliation."
    };
    let description = format!(
        "{}\nMCP server: {toolkit_name}. {effect_guidance}",
        tool.description().trim()
    );
    if description.len() > MAX_DESCRIPTION_BYTES {
        return Err(resource_exhausted());
    }
    Ok(description)
}

fn parse_mcp_authority(tool_type: &str) -> Result<bool, McpMaterializationError> {
    match tool_type {
        "mcp" => Ok(false),
        "mcp_config" => Ok(true),
        value if value.starts_with("mcp_") => Ok(true),
        _ => Err(unsupported_authority()),
    }
}

/// Accepts the SDK's `client_id` / `client_secret` pair without reading the
/// secret. Blank values are unset; a secret with no client id names no app and
/// is refused rather than dropped; a non-string value is a configuration error.
pub(super) fn parse_oauth_client(
    settings: &Map<String, Value>,
    has_toolkit_id: bool,
) -> Result<Option<OAuthClient>, McpMaterializationError> {
    let text = |key: &str| -> Result<Option<&str>, McpMaterializationError> {
        match settings.get(key) {
            None | Some(Value::Null) => Ok(None),
            Some(Value::String(value)) if value.trim().is_empty() => Ok(None),
            Some(Value::String(value))
                if value.len() <= MAX_OAUTH_CLIENT_BYTES
                    && !value.chars().any(char::is_control) =>
            {
                Ok(Some(value.as_str()))
            }
            Some(_) => Err(invalid_configuration()),
        }
    };
    let client_id = text("client_id")?;
    let secret_present = match settings.get("client_secret") {
        None | Some(Value::Null) => false,
        Some(Value::String(value)) => !value.trim().is_empty(),
        // Main resolves only text (`{{secret.NAME}}` or a literal); another
        // shape would be dropped there and the sign-in would run without it.
        Some(_) => return Err(invalid_configuration()),
    };
    let Some(client_id) = client_id else {
        return if secret_present {
            Err(unsupported_authority())
        } else {
            Ok(None)
        };
    };
    if secret_present && !has_toolkit_id {
        return Err(saved_toolkit_required());
    }
    Ok(Some(OAuthClient {
        client_id: client_id.to_owned(),
        secret_present,
        configuration_uuid: text("configuration_uuid")?.map(str::to_owned),
    }))
}

fn parse_requested_scopes(
    settings: &Map<String, Value>,
) -> Result<Vec<String>, McpMaterializationError> {
    let Some(value) = settings.get("scopes").filter(|value| !value.is_null()) else {
        return Ok(Vec::new());
    };
    let values = value.as_array().ok_or_else(invalid_configuration)?;
    if values.len() > MAX_AUTH_METADATA_LIST_ITEMS {
        return Err(invalid_configuration());
    }
    let mut bytes = 0_usize;
    for value in values {
        let scope = value.as_str().ok_or_else(invalid_configuration)?;
        bytes = bytes.saturating_add(scope.len());
        if scope.is_empty()
            || bytes > MAX_AUTH_METADATA_STRING_BYTES
            || !scope
                .bytes()
                .all(|byte| matches!(byte, 0x21 | 0x23..=0x5b | 0x5d..=0x7e))
        {
            return Err(invalid_configuration());
        }
    }
    Ok(values
        .iter()
        .filter_map(Value::as_str)
        .map(str::to_owned)
        .collect())
}

fn parse_static_headers(
    settings: &Map<String, Value>,
) -> Result<reqwest_mcp::header::HeaderMap, McpMaterializationError> {
    let Some(raw) = settings.get("headers") else {
        return Ok(reqwest_mcp::header::HeaderMap::new());
    };
    if raw.is_null() {
        return Ok(reqwest_mcp::header::HeaderMap::new());
    }
    let values = raw.as_object().ok_or_else(invalid_configuration)?;
    if values.is_empty() {
        return Ok(reqwest_mcp::header::HeaderMap::new());
    }
    // A direct `mcp` toolkit's own headers (for example a PAT, #6691) pass
    // the same validation as a prebuilt definition's: transport-owned headers
    // are refused and values stay bounded. The Python SDK sends them too.
    if values.len() > MAX_STATIC_HEADERS {
        return Err(resource_exhausted());
    }

    let mut headers = reqwest_mcp::header::HeaderMap::with_capacity(values.len());
    for (name, raw_value) in values {
        if name.is_empty() || name.len() > MAX_HEADER_NAME_BYTES {
            return Err(invalid_configuration());
        }
        let name = reqwest_mcp::header::HeaderName::from_bytes(name.as_bytes())
            .map_err(|_| invalid_configuration())?;
        if reserved_mcp_header(&name) {
            return Err(unsupported_authority());
        }
        let raw_value = raw_value
            .as_str()
            .filter(|value| {
                value.len() <= MAX_HEADER_VALUE_BYTES && !contains_template_delimiter(value)
            })
            .ok_or_else(invalid_configuration)?;
        let mut value = reqwest_mcp::header::HeaderValue::from_str(raw_value)
            .map_err(|_| invalid_configuration())?;
        value.set_sensitive(true);
        headers.insert(name, value);
    }
    Ok(headers)
}

fn reserved_mcp_header(name: &reqwest_mcp::header::HeaderName) -> bool {
    super::is_reserved_platform_header(name.as_str())
        || matches!(
            name.as_str(),
            "accept"
                | "connection"
                | "content-length"
                | "content-type"
                | "host"
                | "mcp-protocol-version"
                | "mcp-session-id"
                | "transfer-encoding"
                | "upgrade"
        )
}

fn parse_endpoint(settings: &Map<String, Value>) -> Result<String, McpMaterializationError> {
    let endpoint = settings
        .get("url")
        .and_then(Value::as_str)
        .filter(|value| {
            !value.is_empty()
                && value.len() <= MAX_ENDPOINT_BYTES
                && !contains_template_delimiter(value)
        })
        .ok_or_else(invalid_configuration)?;
    let parsed = reqwest_mcp::Url::parse(endpoint).map_err(|_| invalid_configuration())?;
    if parsed.scheme() != "https"
        || parsed.host_str().is_none()
        || !parsed.username().is_empty()
        || parsed.password().is_some()
        || parsed.query().is_some()
        || parsed.fragment().is_some()
        || parsed.cannot_be_a_base()
    {
        return Err(unsupported_authority());
    }
    Ok(parsed.to_string())
}

fn contains_template_delimiter(value: &str) -> bool {
    value.contains('{') || value.contains('}')
}

fn parse_selected_tools(
    settings: &Map<String, Value>,
) -> Result<Vec<String>, McpMaterializationError> {
    parse_tool_names(settings, "selected_tools")
}

fn parse_excluded_tools(
    settings: &Map<String, Value>,
) -> Result<Vec<String>, McpMaterializationError> {
    parse_tool_names(settings, "excluded_tools")
}

fn parse_tool_names(
    settings: &Map<String, Value>,
    field: &str,
) -> Result<Vec<String>, McpMaterializationError> {
    let Some(selected) = settings.get(field) else {
        return Ok(Vec::new());
    };
    let selected = selected.as_array().ok_or_else(invalid_configuration)?;
    if selected.len() > MAX_DISCOVERED_TOOLS {
        return Err(resource_exhausted());
    }
    let mut seen = HashSet::with_capacity(selected.len());
    let mut result = Vec::with_capacity(selected.len());
    for value in selected {
        let name = value
            .as_str()
            .filter(|name| !name.is_empty())
            .ok_or_else(invalid_configuration)?;
        if seen.insert(name.to_lowercase()) {
            result.push(name.to_owned());
        }
    }
    Ok(result)
}

fn resolve_access_token(
    tokens: &Map<String, Value>,
    endpoint: &str,
    tool_type: &str,
    server_name: Option<&str>,
    prebuilt: bool,
) -> Result<Option<Zeroizing<String>>, McpMaterializationError> {
    if tokens.len() > MAX_MCP_SERVERS {
        return Err(resource_exhausted());
    }
    let endpoint = reqwest_mcp::Url::parse(endpoint).map_err(|_| invalid_configuration())?;
    let mut matched = None;
    for (server, raw) in tokens {
        if !valid_mcp_token_alias(server) {
            return Err(invalid_configuration());
        }
        let Ok(candidate) = reqwest_mcp::Url::parse(server) else {
            continue;
        };
        if candidate != endpoint {
            continue;
        }
        if matched.is_some() {
            return Err(invalid_configuration());
        }
        matched = Some(parse_access_token(raw)?);
    }
    if matched.is_some() || !prebuilt {
        return Ok(matched);
    }

    let mut aliases = Vec::with_capacity(3);
    if tool_type != "mcp_config" {
        aliases.push(tool_type.to_owned());
    }
    if let Some(server_name) = server_name {
        aliases.push(server_name.to_owned());
        let prefixed = format!("mcp_{server_name}");
        if prefixed != tool_type {
            aliases.push(prefixed);
        }
    }
    for alias in aliases {
        if let Some(raw) = tokens.get(&alias) {
            return Ok(Some(parse_access_token(raw)?));
        }
    }
    Ok(None)
}

fn valid_mcp_token_alias(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_ENDPOINT_BYTES
        && !value
            .chars()
            .any(|character| matches!(character, '\0' | '\r' | '\n'))
}

fn parse_access_token(raw: &Value) -> Result<Zeroizing<String>, McpMaterializationError> {
    let token = match raw {
        Value::String(token) => Some(token.as_str()),
        Value::Object(value) => value.get("access_token").and_then(Value::as_str),
        _ => None,
    }
    .filter(|token| {
        !token.is_empty()
            && token.len() <= MAX_ACCESS_TOKEN_BYTES
            && !token
                .chars()
                .any(|character| matches!(character, '\0' | '\r' | '\n'))
    })
    .ok_or_else(invalid_configuration)?;
    Ok(Zeroizing::new(token.to_owned()))
}

fn authorization_required(
    config: &RemoteMcpConfig,
    challenge: Option<&str>,
) -> McpMaterializationError {
    let challenge = challenge.filter(|value| {
        !value.is_empty()
            && value.len() <= MAX_AUTH_CHALLENGE_BYTES
            && !value.chars().any(char::is_control)
    });
    let resource_metadata_url =
        challenge.and_then(|challenge| resource_metadata_url(challenge, config.endpoint()));
    McpMaterializationError {
        code: McpMaterializationErrorCode::AuthorizationRequired,
        authorization: DelegatedAuthorizationRequirement::new(
            config.toolkit_name().to_owned(),
            config.toolkit_type().to_owned(),
            config.endpoint().to_owned(),
            resource_metadata_url,
            challenge.map(ToOwned::to_owned),
        )
        .map(Box::new),
    }
}

async fn authorization_required_with_metadata(
    config: &RemoteMcpConfig,
    challenge: Option<&str>,
) -> McpMaterializationError {
    let mut error = authorization_required(config, challenge);
    let timeout = config.timeout().min(AUTH_METADATA_TIMEOUT);
    let resolution = tokio::time::timeout(timeout, async {
        let manager = AuthorizationManager::new(config.endpoint()).await?;
        manager.resolve_metadata_from_challenge(challenge).await
    })
    .await
    .ok()
    .and_then(Result::ok);
    if let Some(resource_metadata) = resolution
        .as_ref()
        .filter(|resolution| resolution.source.is_discovered())
        .and_then(|resolution| {
            authorization_resource_metadata(
                &resolution.metadata,
                config.endpoint(),
                &config.requested_scopes,
            )
        })
    {
        error.authorization = error.authorization.take().and_then(|requirement| {
            requirement
                .with_resource_metadata(resource_metadata)
                .map(Box::new)
        });
    }
    finish_authorization(config, &mut error);
    error
}

/// Completes the authorization signal for Main: the saved toolkit's id and,
/// when configured, its registered OAuth app. Both attach only to discovered
/// metadata; a requirement without any stays as it was.
fn finish_authorization(config: &RemoteMcpConfig, error: &mut McpMaterializationError) {
    let Some(requirement) = error.authorization.take() else {
        return;
    };
    let mut requirement = *requirement;
    if let Some(client) = &config.oauth_client {
        let (attached, ok) = requirement.with_oauth_client(
            &client.client_id,
            client.secret_present,
            &config.requested_scopes,
            client.configuration_uuid.as_deref(),
        );
        if !ok {
            // Dropping the app would send the user to sign in with a
            // different client than the one configured.
            tracing::warn!(
                event = "mcp_oauth_app_unattachable",
                "the OAuth app exceeds the authorization metadata limits"
            );
            error.code = McpMaterializationErrorCode::OAuthAppUnattachable;
            return;
        }
        requirement = attached;
    }
    error.authorization = Some(Box::new(requirement.with_toolkit_id(config.toolkit_id)));
}

#[cfg(any(test, feature = "test-support"))]
pub fn mcp_authorization_required_fixture(
    config: &RemoteMcpConfig,
    challenge: &str,
) -> McpMaterializationError {
    let mut error = authorization_required(config, Some(challenge));
    finish_authorization(config, &mut error);
    error
}

/// The signal a server with discoverable authorization metadata produces.
#[cfg(test)]
pub(crate) fn mcp_authorization_required_with_metadata_fixture(
    config: &RemoteMcpConfig,
    challenge: &str,
    resource_metadata: Value,
) -> McpMaterializationError {
    let mut error = authorization_required(config, Some(challenge));
    error.authorization = error.authorization.take().and_then(|requirement| {
        requirement
            .with_resource_metadata(resource_metadata)
            .map(Box::new)
    });
    finish_authorization(config, &mut error);
    error
}

fn resource_metadata_url(challenge: &str, endpoint: &str) -> Option<String> {
    let server = valid_https_url(endpoint)?;
    WWWAuthenticateParams::parse(challenge, &server)
        .resource_metadata_url
        .map(|url| url.to_string())
}

pub(super) fn authorization_resource_metadata(
    metadata: &AuthorizationMetadata,
    endpoint: &str,
    requested_scopes: &[String],
) -> Option<Value> {
    let authorization_endpoint = safe_auth_url(&metadata.authorization_endpoint)?;
    let token_endpoint = safe_auth_url(&metadata.token_endpoint)?;
    let authorization_server = metadata
        .issuer
        .as_deref()
        .and_then(safe_auth_url)
        .or_else(|| endpoint_origin(endpoint))?;
    let mut server = Map::from_iter([
        (
            "authorization_endpoint".to_owned(),
            Value::String(authorization_endpoint),
        ),
        ("token_endpoint".to_owned(), Value::String(token_endpoint)),
    ]);
    for (key, value) in [
        (
            "registration_endpoint",
            metadata.registration_endpoint.as_deref(),
        ),
        ("issuer", metadata.issuer.as_deref()),
        ("jwks_uri", metadata.jwks_uri.as_deref()),
        (
            "revocation_endpoint",
            metadata
                .additional_fields
                .get("revocation_endpoint")
                .and_then(Value::as_str),
        ),
        (
            "userinfo_endpoint",
            metadata
                .additional_fields
                .get("userinfo_endpoint")
                .and_then(Value::as_str),
        ),
    ] {
        if let Some(value) = value.and_then(safe_auth_url) {
            server.insert(key.to_owned(), Value::String(value));
        }
    }
    for (key, values) in [
        ("scopes_supported", metadata.scopes_supported.as_deref()),
        (
            "response_types_supported",
            metadata.response_types_supported.as_deref(),
        ),
        (
            "code_challenge_methods_supported",
            metadata.code_challenge_methods_supported.as_deref(),
        ),
    ] {
        if let Some(values) = values.and_then(safe_auth_string_list) {
            server.insert(key.to_owned(), values.clone());
        }
    }
    for key in [
        "grant_types_supported",
        "token_endpoint_auth_methods_supported",
    ] {
        if let Some(values) = metadata
            .additional_fields
            .get(key)
            .and_then(Value::as_array)
            .and_then(|values| safe_auth_value_list(values))
        {
            server.insert(key.to_owned(), values);
        }
    }
    let mut resource = Map::from_iter([
        (
            "authorization_servers".to_owned(),
            json!([authorization_server]),
        ),
        (
            "oauth_authorization_server".to_owned(),
            Value::Object(server),
        ),
    ]);
    // The root list supplies resource consent defaults to the existing UI.
    // The nested authorization-server document retains its discovered scope list.
    let resource_scopes = if requested_scopes.is_empty() {
        metadata.scopes_supported.as_deref()
    } else {
        Some(requested_scopes)
    };
    if let Some(scopes) = resource_scopes.and_then(safe_auth_string_list) {
        resource.insert("scopes_supported".to_owned(), scopes.clone());
    }
    Some(Value::Object(resource))
}

fn endpoint_origin(endpoint: &str) -> Option<String> {
    let mut url = valid_https_url(endpoint)?;
    url.set_path("");
    url.set_query(None);
    Some(url.to_string().trim_end_matches('/').to_owned())
}

fn safe_auth_url(value: &str) -> Option<String> {
    (value.len() <= MAX_AUTH_METADATA_STRING_BYTES)
        .then(|| valid_https_url(value).map(|url| url.to_string()))
        .flatten()
}

fn safe_auth_string_list(values: &[String]) -> Option<Value> {
    (values.len() <= MAX_AUTH_METADATA_LIST_ITEMS
        && !values.is_empty()
        && values.iter().all(|value| {
            !value.is_empty()
                && value.len() <= MAX_AUTH_METADATA_STRING_BYTES
                && !value.chars().any(char::is_control)
        }))
    .then(|| json!(values))
}

fn safe_auth_value_list(values: &[Value]) -> Option<Value> {
    (!values.is_empty()
        && values.len() <= MAX_AUTH_METADATA_LIST_ITEMS
        && values.iter().all(|value| {
            value.as_str().is_some_and(|value| {
                !value.is_empty()
                    && value.len() <= MAX_AUTH_METADATA_STRING_BYTES
                    && !value.chars().any(char::is_control)
            })
        }))
    .then(|| Value::Array(values.to_vec()))
}

fn valid_https_url(value: &str) -> Option<reqwest_mcp::Url> {
    let parsed = reqwest_mcp::Url::parse(value).ok()?;
    (parsed.scheme() == "https"
        && parsed.host_str().is_some()
        && parsed.username().is_empty()
        && parsed.password().is_none()
        && parsed.fragment().is_none())
    .then_some(parsed)
}

fn parse_bounded_integer(
    value: Option<&Value>,
    default: u64,
    minimum: u64,
    maximum: u64,
) -> Result<u64, McpMaterializationError> {
    let value = match value {
        None | Some(Value::Null) => default,
        Some(Value::Number(number)) => number.as_u64().ok_or_else(invalid_configuration)?,
        Some(Value::String(text)) => text.parse().map_err(|_| invalid_configuration())?,
        Some(_) => return Err(invalid_configuration()),
    };
    if !(minimum..=maximum).contains(&value) {
        return Err(invalid_configuration());
    }
    Ok(value)
}

fn validate_result(value: &Value) -> Result<(), McpMaterializationError> {
    let mut nodes = 0_usize;
    let mut stack = vec![(value, 1_usize)];
    while let Some((current, depth)) = stack.pop() {
        nodes = nodes.checked_add(1).ok_or_else(resource_exhausted)?;
        if nodes > MAX_RESULT_NODES || depth > MAX_RESULT_DEPTH {
            return Err(resource_exhausted());
        }
        match current {
            Value::String(text) if text.len() > MAX_RESULT_STRING_BYTES => {
                return Err(resource_exhausted());
            }
            Value::Array(values) => {
                stack.extend(values.iter().map(|entry| (entry, depth + 1)));
            }
            Value::Object(values) => {
                if values.keys().any(|key| key.len() > MAX_RESULT_STRING_BYTES) {
                    return Err(resource_exhausted());
                }
                stack.extend(values.values().map(|entry| (entry, depth + 1)));
            }
            Value::Null | Value::Bool(_) | Value::Number(_) | Value::String(_) => {}
        }
    }
    let mut writer = BoundedWriter::new(MAX_RESULT_BYTES);
    serde_json::to_writer(&mut writer, value).map_err(|_| resource_exhausted())?;
    Ok(())
}

struct BoundedWriter {
    written: usize,
    limit: usize,
}

impl BoundedWriter {
    const fn new(limit: usize) -> Self {
        Self { written: 0, limit }
    }
}

impl Write for BoundedWriter {
    fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
        let next = self
            .written
            .checked_add(buffer.len())
            .ok_or_else(|| io::Error::other("MCP result byte limit exceeded"))?;
        if next > self.limit {
            return Err(io::Error::other("MCP result byte limit exceeded"));
        }
        self.written = next;
        Ok(buffer.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

fn mcp_timeout() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::Timeout,
        "mcp.tool.unknown_outcome",
        "the remote MCP tool timed out with an unknown outcome",
    )
}

fn invalid_mcp_result() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::Internal,
        "mcp.tool.unknown_outcome",
        "the remote MCP tool returned an unusable result with an unknown outcome",
    )
}

const fn invalid_configuration() -> McpMaterializationError {
    McpMaterializationError {
        code: McpMaterializationErrorCode::InvalidConfiguration,
        authorization: None,
    }
}

const fn unsupported_authority() -> McpMaterializationError {
    McpMaterializationError {
        code: McpMaterializationErrorCode::UnsupportedAuthority,
        authorization: None,
    }
}

const fn resource_exhausted() -> McpMaterializationError {
    McpMaterializationError {
        code: McpMaterializationErrorCode::ResourceExhausted,
        authorization: None,
    }
}

const fn saved_toolkit_required() -> McpMaterializationError {
    McpMaterializationError {
        code: McpMaterializationErrorCode::SavedToolkitRequired,
        authorization: None,
    }
}

const fn retired_sse_endpoint() -> McpMaterializationError {
    McpMaterializationError {
        code: McpMaterializationErrorCode::RetiredSseEndpoint,
        authorization: None,
    }
}

const fn dependency_unavailable() -> McpMaterializationError {
    McpMaterializationError {
        code: McpMaterializationErrorCode::DependencyUnavailable,
        authorization: None,
    }
}
