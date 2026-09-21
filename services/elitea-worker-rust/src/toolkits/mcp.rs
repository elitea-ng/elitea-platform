//! Claim-bound remote MCP toolsets backed by ADK-Rust's native MCP client.
//!
//! Main owns the frozen server definition. The worker validates that exact
//! authority, establishes one Streamable HTTP session without redirects or
//! automatic request replay, asks ADK to discover the server catalog, and
//! wraps the resulting ADK tools with Elitea policy, metadata and result
//! bounds. Saved `mcp_config` definitions and arbitrary stdio processes have a
//! different authority owner and fail closed here.

#![allow(dead_code)] // Production agent registration remains capability-gated.

use std::collections::HashSet;
use std::fmt;
use std::io::{self, Write};
use std::sync::Arc;
use std::time::Duration;

use adk_rust::tool::mcp::rmcp::ServiceExt as _;
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
/// static headers and embedded OAuth credentials remain unsupported.
pub(crate) struct RemoteMcpConfig {
    toolkit_name: String,
    endpoint: String,
    timeout: Duration,
    selected_tools: Vec<String>,
    access_token: Option<Zeroizing<String>>,
}

impl RemoteMcpConfig {
    fn parse(
        reference: &FrozenToolReference<'_>,
        mcp_tokens: &Map<String, Value>,
    ) -> Result<Self, McpMaterializationError> {
        if reference.kind() != FrozenToolKind::Mcp || reference.tool_type() != "mcp" {
            return Err(unsupported_authority());
        }
        let settings = reference.settings().ok_or_else(invalid_configuration)?;
        reject_unowned_auth(settings)?;
        let endpoint = parse_endpoint(settings)?;
        let timeout = Duration::from_secs(parse_bounded_integer(
            settings.get("timeout"),
            DEFAULT_TIMEOUT_SECONDS,
            1,
            MAX_TIMEOUT_SECONDS,
        )?);
        let selected_tools = parse_selected_tools(settings)?;
        let access_token = resolve_access_token(mcp_tokens, &endpoint)?;
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
            endpoint,
            timeout,
            selected_tools,
            access_token,
        })
    }

    #[must_use]
    pub(crate) fn toolkit_name(&self) -> &str {
        &self.toolkit_name
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

    fn access_token(&self) -> Option<&str> {
        self.access_token.as_deref().map(String::as_str)
    }

    #[cfg(test)]
    pub(crate) fn access_token_for_test(&self) -> Option<&str> {
        self.access_token()
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
        if let Some(token) = config.access_token() {
            let mut bearer = Zeroizing::new(String::with_capacity("Bearer ".len() + token.len()));
            bearer.push_str("Bearer ");
            bearer.push_str(token);
            let mut value = reqwest_mcp::header::HeaderValue::from_str(&bearer)
                .map_err(|_| invalid_configuration())?;
            value.set_sensitive(true);
            let mut headers = reqwest_mcp::header::HeaderMap::new();
            headers.insert(reqwest_mcp::header::AUTHORIZATION, value);
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
                return Err(authorization_required(config, error.auth_challenge()));
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

/// Materialize MCP toolsets and retain the sanitized identity of selected
/// placeholder tools whose server rejected discovery with an OAuth challenge.
/// LLM runtimes use this catalog to pause before dispatching the original call.
pub(crate) async fn materialize_mcp_toolsets_with_tokens_and_authorization(
    snapshot: &AdmittedToolSnapshot<'_>,
    connector: &dyn McpConnector,
    policy: &Arc<ToolAdmissionPolicy>,
    mcp_tokens: &Map<String, Value>,
) -> Result<(Vec<Arc<dyn Toolset>>, DelegatedAuthorizationCatalog), McpMaterializationError> {
    let (toolsets, per_toolset) =
        materialize_mcp_toolsets_by_toolset(snapshot, connector, policy, mcp_tokens).await?;
    let mut authorization = DelegatedAuthorizationCatalog::default();
    for catalog in per_toolset {
        authorization
            .merge(catalog)
            .map_err(|()| invalid_configuration())?;
    }
    Ok((toolsets, authorization))
}

/// The same materialization, with the authorization requirements kept SPLIT
/// per toolset instead of merged into one catalog.
///
/// #983 renames a tool name two toolsets both publish, and the rename is
/// per-toolset by construction. A catalog already merged across servers has
/// lost which server each key came from, so the rename could not be applied
/// to it without guessing. The ordinary agent path therefore takes the split
/// form, renames each half, and merges afterwards; every other caller keeps
/// the merged shape it already had.
pub(crate) async fn materialize_mcp_toolsets_by_toolset(
    snapshot: &AdmittedToolSnapshot<'_>,
    connector: &dyn McpConnector,
    policy: &Arc<ToolAdmissionPolicy>,
    mcp_tokens: &Map<String, Value>,
) -> Result<(Vec<Arc<dyn Toolset>>, Vec<DelegatedAuthorizationCatalog>), McpMaterializationError> {
    let references = snapshot
        .iter()
        .filter(|reference| reference.kind() == FrozenToolKind::Mcp)
        .collect::<Vec<_>>();
    if references.len() > MAX_MCP_SERVERS {
        return Err(resource_exhausted());
    }

    let mut toolsets = Vec::with_capacity(references.len());
    let mut per_toolset = Vec::with_capacity(references.len());
    for reference in references {
        let mut authorization = DelegatedAuthorizationCatalog::default();
        let config = RemoteMcpConfig::parse(reference, mcp_tokens)?;
        let discovered = match connector.connect(&config).await {
            Ok(discovered) => discovered,
            Err(error) if error.code() == McpMaterializationErrorCode::AuthorizationRequired => {
                let requirement = error
                    .authorization()
                    .cloned()
                    .ok_or_else(invalid_configuration)?;
                if config.selected_tools().is_empty() {
                    return Err(error);
                }
                let guarded = config
                    .selected_tools()
                    .iter()
                    .map(|name| {
                        Arc::new(McpAuthorizationRequiredTool::new(name, requirement.clone()))
                            as Arc<dyn Tool>
                    })
                    .collect::<Vec<_>>();
                for name in config.selected_tools() {
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
                .map_err(|error| match error.code() {
                    super::invocation::MaterializedToolsetErrorCode::InvalidDefinition => {
                        invalid_configuration()
                    }
                    super::invocation::MaterializedToolsetErrorCode::ResourceExhausted => {
                        resource_exhausted()
                    }
                })?;
                toolsets.push(Arc::new(admitted) as Arc<dyn Toolset>);
                per_toolset.push(authorization);
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
        let tools = select_tools(tools, config.selected_tools())?;
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
        .map_err(|error| match error.code() {
            super::invocation::MaterializedToolsetErrorCode::InvalidDefinition => {
                invalid_configuration()
            }
            super::invocation::MaterializedToolsetErrorCode::ResourceExhausted => {
                resource_exhausted()
            }
        })?;
        toolsets.push(Arc::new(admitted) as Arc<dyn Toolset>);
        per_toolset.push(authorization);
    }
    Ok((toolsets, per_toolset))
}

fn select_tools(
    tools: Vec<Arc<dyn Tool>>,
    selected: &[String],
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
        return Ok(tools);
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
        .filter(|tool| selected.contains(&tool.name().to_lowercase()))
        .collect())
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

fn reject_unowned_auth(settings: &Map<String, Value>) -> Result<(), McpMaterializationError> {
    for key in ["client_id", "client_secret", "scopes"] {
        if settings.get(key).is_some_and(|value| !value.is_null()) {
            return Err(unsupported_authority());
        }
    }
    if settings.get("headers").is_some_and(|value| match value {
        Value::Null => false,
        Value::Object(headers) => !headers.is_empty(),
        _ => true,
    }) {
        return Err(unsupported_authority());
    }
    Ok(())
}

fn parse_endpoint(settings: &Map<String, Value>) -> Result<String, McpMaterializationError> {
    let endpoint = settings
        .get("url")
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty() && value.len() <= MAX_ENDPOINT_BYTES)
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

fn parse_selected_tools(
    settings: &Map<String, Value>,
) -> Result<Vec<String>, McpMaterializationError> {
    let Some(selected) = settings.get("selected_tools") else {
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
) -> Result<Option<Zeroizing<String>>, McpMaterializationError> {
    if tokens.len() > MAX_MCP_SERVERS {
        return Err(resource_exhausted());
    }
    let endpoint = reqwest_mcp::Url::parse(endpoint).map_err(|_| invalid_configuration())?;
    let mut matched = None;
    for (server, raw) in tokens {
        if server.is_empty() || server.len() > MAX_ENDPOINT_BYTES {
            return Err(invalid_configuration());
        }
        let candidate = reqwest_mcp::Url::parse(server).map_err(|_| invalid_configuration())?;
        if candidate != endpoint {
            continue;
        }
        if matched.is_some() {
            return Err(invalid_configuration());
        }
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
        matched = Some(Zeroizing::new(token.to_owned()));
    }
    Ok(matched)
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
            "mcp".to_owned(),
            config.endpoint().to_owned(),
            resource_metadata_url,
            challenge.map(ToOwned::to_owned),
        )
        .map(Box::new),
    }
}

#[cfg(test)]
pub(crate) fn mcp_authorization_required_fixture(
    config: &RemoteMcpConfig,
    challenge: &str,
) -> McpMaterializationError {
    authorization_required(config, Some(challenge))
}

fn resource_metadata_url(challenge: &str, endpoint: &str) -> Option<String> {
    let lowercase = challenge.to_ascii_lowercase();
    let offset = lowercase.find("resource_metadata=")? + "resource_metadata=".len();
    let value = challenge.get(offset..)?.trim_start();
    let raw = if let Some(quoted) = value.strip_prefix('"') {
        let mut escaped = false;
        let end = quoted.char_indices().find_map(|(index, character)| {
            if escaped {
                escaped = false;
                return None;
            }
            if character == '\\' {
                escaped = true;
                return None;
            }
            (character == '"').then_some(index)
        })?;
        quoted.get(..end)?.replace("\\\"", "\"")
    } else {
        value
            .split(|character: char| character == ',' || character.is_ascii_whitespace())
            .next()?
            .to_owned()
    };
    let server = valid_https_url(endpoint)?;
    let metadata = server.join(&raw).ok()?;
    (metadata.scheme() == server.scheme()
        && metadata.host_str() == server.host_str()
        && metadata.port_or_known_default() == server.port_or_known_default()
        && metadata.username().is_empty()
        && metadata.password().is_none()
        && metadata.fragment().is_none())
    .then(|| metadata.to_string())
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
