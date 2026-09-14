//! Claim-bound remote MCP toolsets backed by ADK-Rust's native MCP client.
//!
//! Main owns the frozen server definition. The worker validates that exact
//! authority, establishes one Streamable HTTP session without redirects or
//! automatic request replay, asks ADK to discover the server catalog, and
//! wraps the resulting ADK tools with Elitea policy, metadata and result
//! bounds. Main may also claim-materialize one fixed prebuilt HTTP definition.
//! Arbitrary stdio processes remain outside this worker.

#![allow(dead_code)] // Production agent registration remains capability-gated.

use std::collections::HashSet;
use std::fmt;
use std::io::{self, Write};
use std::sync::Arc;
use std::time::Duration;

use adk_rust::tool::mcp::rmcp::ServiceExt as _;
use adk_rust::tool::mcp::rmcp::transport::auth::{
    AuthorizationManager, AuthorizationMetadata, WWWAuthenticateParams,
};
use adk_rust::tool::mcp::rmcp::transport::streamable_http_client::{
    StreamableHttpClientTransport, StreamableHttpClientTransportConfig,
};
use adk_rust::tool::{McpToolset, SimpleToolContext};
use adk_rust::{
    AdkError, ErrorCategory, ErrorComponent, ReadonlyContext, RetryHint, Tool, ToolContext, Toolset,
};
use async_trait::async_trait;
use serde_json::{Map, Value, json};
use zeroize::Zeroizing;

use super::delegated_auth::{
    DelegatedAuthorizationCatalog, DelegatedAuthorizationRequirement, delegated_authorization_error,
};
use super::invocation::admit_materialized_toolset;
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
const MAX_STATIC_HEADERS: usize = 64;
const MAX_HEADER_NAME_BYTES: usize = 256;
const MAX_HEADER_VALUE_BYTES: usize = 16 * 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum McpMaterializationErrorCode {
    InvalidConfiguration,
    UnsupportedAuthority,
    ResourceExhausted,
    AuthorizationRequired,
    DependencyUnavailable,
}

/// A stable, data-free remote MCP assembly failure.
#[derive(Clone)]
pub(crate) struct McpMaterializationError {
    code: McpMaterializationErrorCode,
    authorization: Option<Box<DelegatedAuthorizationRequirement>>,
}

impl McpMaterializationError {
    #[must_use]
    pub(crate) const fn code(&self) -> McpMaterializationErrorCode {
        self.code
    }

    pub(crate) fn authorization(&self) -> Option<&DelegatedAuthorizationRequirement> {
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
        })
    }
}

impl std::error::Error for McpMaterializationError {}

/// Validated, immutable connection settings for one direct remote MCP server.
///
/// This value intentionally has no `Debug` implementation because endpoints
/// may contain tenant-identifying paths. A continuation access token is accepted
/// only from Main's claim-fetched token map and applied to the exact frozen URL;
/// static headers require a Main-materialized prebuilt toolkit type.
pub(crate) struct RemoteMcpConfig {
    toolkit_name: String,
    toolkit_type: String,
    endpoint: String,
    timeout: Duration,
    selected_tools: Vec<String>,
    excluded_tools: Vec<String>,
    requested_scopes: Vec<String>,
    static_headers: reqwest_mcp::header::HeaderMap,
    access_token: Option<Zeroizing<String>>,
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
        reject_unowned_auth(settings)?;
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
        let static_headers = parse_static_headers(settings, prebuilt)?;
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
            if !static_headers.contains_key(reqwest_mcp::header::AUTHORIZATION) {
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
        settings.get("enable_caching").map_or(Ok(true), |value| {
            value.as_bool().ok_or_else(invalid_configuration)
        })?;
        parse_bounded_integer(settings.get("cache_ttl"), 300, 60, 3_600)?;
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
        })
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
    pub(crate) fn endpoint(&self) -> &str {
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

    fn request_headers(&self) -> Result<reqwest_mcp::header::HeaderMap, McpMaterializationError> {
        let mut headers = self.static_headers.clone();
        if let Some(token) = self.access_token() {
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

    #[cfg(test)]
    pub(crate) fn access_token_for_test(&self) -> Option<&str> {
        self.access_token()
    }

    #[cfg(test)]
    pub(crate) fn request_headers_for_test(
        &self,
    ) -> Result<reqwest_mcp::header::HeaderMap, McpMaterializationError> {
        self.request_headers()
    }
}

/// Connection seam for ADK's MCP implementation.
///
/// Production uses [`AdkHttpMcpConnector`]. Tests may inject an in-memory ADK
/// toolset without granting network authority.
#[async_trait]
pub(crate) trait McpConnector: Send + Sync {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError>;
}

/// Secure Streamable HTTP connector for ADK's native [`McpToolset`].
pub(crate) struct AdkHttpMcpConnector;

impl AdkHttpMcpConnector {
    #[must_use]
    pub(crate) const fn new() -> Self {
        Self
    }
}

#[async_trait]
impl McpConnector for AdkHttpMcpConnector {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        let mut client = reqwest_mcp::Client::builder()
            .https_only(true)
            .redirect(reqwest_mcp::redirect::Policy::none())
            .retry(reqwest_mcp::retry::never())
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(config.timeout());
        let headers = config.request_headers()?;
        if !headers.is_empty() {
            client = client.default_headers(headers);
        }
        let client = client.build().map_err(|_| invalid_configuration())?;
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
            Err(_) | Ok(Err(_)) => return Err(dependency_unavailable()),
        };
        Ok(Arc::new(McpToolset::new(running)))
    }
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
pub(crate) async fn materialize_mcp_toolsets_with_tokens_and_authorization(
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
        let discovered = match connector.connect(&config).await {
            Ok(discovered) => discovered,
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
        let context: Arc<dyn ReadonlyContext> =
            Arc::new(SimpleToolContext::new("elitea_mcp_discovery"));
        let tools = tokio::time::timeout(config.timeout(), discovered.tools(context))
            .await
            .map_err(|_| dependency_unavailable())?
            .map_err(|_| dependency_unavailable())?;
        let tools = select_tools(tools, config.selected_tools(), config.excluded_tools())?;
        let wrapped = tools
            .into_iter()
            .map(|tool| {
                Ok(Arc::new(BoundedMcpTool::new(
                    tool,
                    reference.toolkit_name(),
                    config.timeout(),
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
        toolsets.push(Arc::new(admitted) as Arc<dyn Toolset>);
    }
    Ok((toolsets, authorization))
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
    ) -> adk_rust::Result<Value> {
        authorization_error(&self.requirement)
    }
}

fn authorization_error(requirement: &DelegatedAuthorizationRequirement) -> adk_rust::Result<Value> {
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
}

impl BoundedMcpTool {
    fn new(
        inner: Arc<dyn Tool>,
        toolkit_name: &str,
        timeout: Duration,
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
    ) -> adk_rust::Result<Value> {
        let value = tokio::time::timeout(self.timeout, self.inner.execute(context, arguments))
            .await
            .map_err(|_| mcp_timeout())?
            .map_err(|error| sanitize_mcp_error(&error))?;
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

fn reject_unowned_auth(settings: &Map<String, Value>) -> Result<(), McpMaterializationError> {
    for key in ["client_id", "client_secret"] {
        if settings.get(key).is_some_and(|value| !value.is_null()) {
            return Err(unsupported_authority());
        }
    }
    Ok(())
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
    prebuilt: bool,
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
    if !prebuilt {
        return Err(unsupported_authority());
    }
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
    matches!(
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
    let Some(resource_metadata) = resolution
        .as_ref()
        .filter(|resolution| resolution.source.is_discovered())
        .and_then(|resolution| {
            authorization_resource_metadata(
                &resolution.metadata,
                config.endpoint(),
                &config.requested_scopes,
            )
        })
    else {
        return error;
    };
    error.authorization = error.authorization.take().and_then(|requirement| {
        requirement
            .with_resource_metadata(resource_metadata)
            .map(Box::new)
    });
    error
}

#[cfg(test)]
pub(crate) fn mcp_authorization_required_fixture(
    config: &RemoteMcpConfig,
    challenge: &str,
) -> McpMaterializationError {
    authorization_required(config, Some(challenge))
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

fn sanitize_mcp_error(error: &AdkError) -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        error.category,
        "mcp.tool.failed",
        "the remote MCP tool failed",
    )
    .with_retry(RetryHint {
        should_retry: false,
        retry_after_ms: None,
        max_attempts: Some(1),
    })
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

const fn dependency_unavailable() -> McpMaterializationError {
    McpMaterializationError {
        code: McpMaterializationErrorCode::DependencyUnavailable,
        authorization: None,
    }
}
