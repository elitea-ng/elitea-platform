//! Governed remote MCP connections (ADR-0039, issue #1123).
//!
//! On a governed run no MCP connection holds a credential from claim time.
//! A remote server's access token is redeemed through the run's
//! [`GovernedOperationClient`] (the same `RedeemCredential` path the
//! configured toolkits use) and checked against the server's canonical
//! resource URI before any byte reaches the server:
//!
//! - building the toolkit redeems `mcp.tools.list` ONCE (empty resource),
//!   lists the server's tools over a session that carries only that token,
//!   keeps the tools' declarations and drops the session and the token. The
//!   connections are listed concurrently, at most
//!   [`MAX_CONCURRENT_LISTINGS`] at a time. A listing the server refuses or
//!   cannot answer (a 401 for a narrow scope, a timeout, a malformed list)
//!   leaves that one connection out, named in the run notice; it does not
//!   end the run;
//! - each tool call redeems `mcp.tool.call` on `mcp:tool/<name>`, opens a
//!   FRESH session with only `Authorization: Bearer <token>` to that one
//!   server, lists the tools on that session and calls the tool through
//!   ADK's own MCP tool (task polling and input rounds included), and drops
//!   the session and the token with the call's frame.
//!
//! A fresh session per call costs one MCP `initialize` and one `tools/list`
//! more per call than the legacy path. That is accepted for now: no session,
//! client or token outlives the one invocation its envelope was minted for,
//! and the call runs on the one execution path every MCP tool uses.
//!
//! Only a saved direct `mcp` toolkit is governable: a connection with no
//! toolkit id Main can bind a grant to, a prebuilt or internal server, or a
//! toolkit whose own headers carry a credential is left out as not
//! governable, before any redemption. The tool-list cache is never used.
//! The token is never logged, never put in a tool result and never sent to
//! another server.

use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use futures::stream::{self, StreamExt as _};
use serde_json::{Map, Value};
use zeroize::Zeroizing;

use super::{
    BoundedMcpTool, MAX_MCP_SERVERS, McpAuthorizationRequiredTool, McpConnector,
    McpMaterializationError, McpMaterializationErrorCode, McpSession, RemoteMcpConfig,
    contains_tool_name, dependency_unavailable, discover_tools, invalid_configuration,
    materialized_toolset_error, resource_exhausted, selection_description,
};
use crate::toolkits::delegated_auth::{
    DelegatedAuthorizationCatalog, DelegatedAuthorizationRequirement, delegated_authorization_error,
};
use crate::toolkits::governed::adapters::McpAdapter;
use crate::toolkits::governed::adapters::mcp::MCP_PROVIDER_TYPE;
use crate::toolkits::governed::client::{Redeemed, credential_reference_id};
use crate::toolkits::governed::{
    AuthAdapter, BoundToolInvocation, GovernedError, GovernedOperationClient,
    canonical_resource_uri,
};
use crate::toolkits::invocation::admit_materialized_toolset;
use crate::toolkits::materialize::LeftOutToolkit;
use crate::toolkits::policy::{ToolAdmissionDecision, ToolAdmissionPolicy};
use crate::toolkits::snapshot::{AdmittedToolSnapshot, FrozenToolKind, FrozenToolReference};

/// The tool call id of the listing redemption: a build has no model tool
/// call, and Main requires a non-empty printable one.
const LISTING_TOOL_CALL_ID: &str = "elitea.mcp.tools.list";

/// How many governed listings one build runs at a time.
pub(crate) const MAX_CONCURRENT_LISTINGS: usize = 4;

/// The reason code of a connection whose listing its server refused or
/// could not answer.
const LISTING_FAILED: &str = "governed.mcp_listing_failed";

/// The governed MCP half of an agent's toolsets.
#[derive(Default)]
pub struct GovernedMcpToolsets {
    pub toolsets: Vec<Arc<dyn Toolset>>,
    pub delegated_authorization: DelegatedAuthorizationCatalog,
    /// Connections left out as not governable (named in the run notice).
    pub left_out: Vec<LeftOutToolkit>,
}

/// Build the MCP connections of a governed run (#1123).
///
/// # Errors
///
/// A [`McpMaterializationError`] for a malformed connection, too many
/// connections, a listing that cannot be reached, or a server whose listing
/// the run refuses; the same answers the legacy materializer gives.
pub async fn materialize_governed_mcp_toolsets(
    snapshot: &AdmittedToolSnapshot<'_>,
    connector: &Arc<dyn McpConnector>,
    policy: &Arc<ToolAdmissionPolicy>,
    governed: &Arc<GovernedOperationClient>,
) -> Result<GovernedMcpToolsets, McpMaterializationError> {
    let references = snapshot
        .iter()
        .filter(|reference| reference.kind() == FrozenToolKind::Mcp)
        .collect::<Vec<_>>();
    if references.len() > MAX_MCP_SERVERS {
        return Err(resource_exhausted());
    }
    let mut out = GovernedMcpToolsets::default();
    let mut admitted = Vec::with_capacity(references.len());
    for reference in references {
        let Some(server) = GovernedServer::admit(reference, policy)? else {
            tracing::warn!(
                event = "agent_toolkit_refused",
                reason_code = "toolkit_not_governable",
                toolkit_type = reference.tool_type(),
                toolkit_id = reference.tool_id(),
                "a governed run left out an MCP connection it cannot govern"
            );
            out.left_out.push(LeftOutToolkit::not_governable(
                reference.toolkit_name(),
                reference.tool_type(),
            ));
            continue;
        };
        admitted.push((reference, Arc::new(server)));
    }
    // Each listing redeems and dials its own server; they run together,
    // bounded, and come back in the snapshot's order.
    // The futures are built first: a stream that maps through a borrowing
    // closure is not `Send` for every lifetime the worker's spawn needs.
    let pending = admitted
        .iter()
        .map(|(_, server)| list(server, connector.as_ref(), governed))
        .collect::<Vec<_>>();
    let listings = stream::iter(pending)
        .buffered(MAX_CONCURRENT_LISTINGS)
        .collect::<Vec<_>>()
        .await;
    for ((reference, server), listing) in admitted.into_iter().zip(listings) {
        match listing? {
            Listing::Tools(tools) => {
                let tools = tools
                    .into_iter()
                    .map(|declaration| {
                        Arc::new(GovernedMcpTool {
                            declaration,
                            server: Arc::clone(&server),
                            connector: Arc::clone(connector),
                            client: Arc::clone(governed),
                        }) as Arc<dyn Tool>
                    })
                    .collect::<Vec<_>>();
                out.toolsets.push(admit(reference, policy, tools)?);
            }
            Listing::Authorization(requirement) => {
                authorization_placeholders(reference, &server, policy, requirement, &mut out)?;
            }
            Listing::Refused(reason_code) => {
                tracing::warn!(
                    event = "agent_toolkit_refused",
                    reason_code,
                    toolkit_type = reference.tool_type(),
                    toolkit_id = reference.tool_id(),
                    "a governed MCP listing was refused; the connection was left out"
                );
                out.left_out.push(LeftOutToolkit::not_governable(
                    reference.toolkit_name(),
                    reference.tool_type(),
                ));
            }
        }
    }
    Ok(out)
}

/// One governable server: its frozen connection with no credential, the
/// canonical target an envelope must name, and what Main binds a grant to.
struct GovernedServer {
    config: RemoteMcpConfig,
    toolkit_id: i64,
    target: String,
    credential_reference_id: String,
}

impl GovernedServer {
    /// `None`: not governable (left out before any redemption).
    fn admit(
        reference: &FrozenToolReference<'_>,
        policy: &ToolAdmissionPolicy,
    ) -> Result<Option<Self>, McpMaterializationError> {
        let Some(toolkit_id) = reference
            .tool_id()
            .and_then(|id| i64::try_from(id).ok())
            .filter(|id| *id > 0)
        else {
            return Ok(None);
        };
        if reference.tool_type() != MCP_PROVIDER_TYPE || reference.is_internal_builder() {
            return Ok(None);
        }
        let settings = reference.settings().ok_or_else(invalid_configuration)?;
        let config = RemoteMcpConfig::parse(reference, &Map::new())?
            .with_assertions(policy.egress().assertions());
        // A credential in the toolkit's own headers is a claim-time credential;
        // the internal tool service is the platform's authority, not a grant.
        if config.served_internally() || !config.static_headers.is_empty() {
            return Ok(None);
        }
        // The target is the CONFIGURED URL's canonical resource URI, and the
        // endpoint the session dials must be that same resource.
        let Ok(target) = McpAdapter.configured_target(settings) else {
            return Ok(None);
        };
        if canonical_resource_uri(config.endpoint()).as_deref() != Some(target.as_str()) {
            return Ok(None);
        }
        Ok(Some(Self {
            config,
            toolkit_id,
            target,
            credential_reference_id: credential_reference_id(settings),
        }))
    }

    fn name(&self) -> &str {
        self.config.toolkit_name()
    }

    fn invocation(
        &self,
        client: &GovernedOperationClient,
        tool_call_id: &str,
        operation: &crate::toolkits::governed::BoundOperation,
    ) -> Result<BoundToolInvocation, GovernedError> {
        Ok(BoundToolInvocation {
            invocation_id: client.invocation_id()?,
            tool_call_id: tool_call_id.to_owned(),
            toolkit_id: self.toolkit_id,
            operation_id: operation.operation_id().to_owned(),
            resource_ref: operation.resource_ref().to_owned(),
            arguments_digest: *operation.arguments_digest(),
            credential_reference_id: self.credential_reference_id.clone(),
        })
    }

    /// The frozen connection carrying `token` as its ONLY credential, for one
    /// session. Not cached: the tool-list cache is off.
    fn session_config(&self, token: Zeroizing<String>) -> RemoteMcpConfig {
        let base = &self.config;
        RemoteMcpConfig {
            toolkit_name: base.toolkit_name.clone(),
            toolkit_type: base.toolkit_type.clone(),
            endpoint: base.endpoint.clone(),
            timeout: base.timeout,
            selected_tools: base.selected_tools.clone(),
            excluded_tools: base.excluded_tools.clone(),
            requested_scopes: base.requested_scopes.clone(),
            static_headers: reqwest_mcp::header::HeaderMap::new(),
            access_token: Some(token),
            oauth_client: None,
            toolkit_id: base.toolkit_id,
            tool_list_ttl: None,
            internal_builder: false,
            pinned_tools: base.pinned_tools.clone(),
            assertions: base.assertions.clone(),
        }
    }

    const fn timeout(&self) -> Duration {
        self.config.timeout
    }
}

/// What one redeemed listing produced.
enum Listing {
    Tools(Vec<ToolDeclaration>),
    Authorization(DelegatedAuthorizationRequirement),
    /// Left out for this reason code; the rest of the run goes on.
    Refused(&'static str),
}

/// Redeem `mcp.tools.list`, list the server's tools over a session carrying
/// only that token, and keep their declarations. The session, its tools
/// and the token drop before this returns.
///
/// Only an error of the worker or of Main's broker as a whole (no operation,
/// no invocation id, the redeem RPC unavailable, a client the worker cannot
/// build) ends the run. Anything this one server answers or fails to answer
/// is [`Listing::Refused`].
async fn list(
    server: &GovernedServer,
    connector: &dyn McpConnector,
    governed: &GovernedOperationClient,
) -> Result<Listing, McpMaterializationError> {
    let operation = McpAdapter::listing_operation().map_err(|_| invalid_configuration())?;
    let invocation = server
        .invocation(governed, LISTING_TOOL_CALL_ID, &operation)
        .map_err(|_| dependency_unavailable())?;
    let envelope = match governed
        .redeem_checked(&McpAdapter, server.name(), &server.target, invocation)
        .await
    {
        Ok(Redeemed::Envelope(envelope)) => envelope,
        Ok(Redeemed::Authorization(requirement)) => return Ok(Listing::Authorization(requirement)),
        Err(GovernedError::RedeemUnavailable {
            retryable: true, ..
        }) => return Err(dependency_unavailable()),
        Err(error) => return Ok(Listing::Refused(error.code())),
    };
    let token = match access_token(&envelope) {
        Ok(token) => token,
        Err(error) => return Ok(Listing::Refused(error.code())),
    };
    drop(envelope);
    let config = server.session_config(token);
    let session = match connector.connect_session(&config).await {
        Ok(session) => session,
        // The worker could not build the client: not this server's answer.
        Err(error) if error.code() == McpMaterializationErrorCode::InvalidConfiguration => {
            return Err(error);
        }
        // The run must sign the call and cannot (ADR-0044 decision 4).
        Err(error) if error.assertion_refusal().is_some() => {
            return Ok(Listing::Refused(crate::remote_assertion::ASSERTION_UNAVAILABLE));
        }
        // The server refused the redeemed token (never a fallback to the
        // browser flow on a governed run) or did not answer.
        Err(error) => {
            return Ok(Listing::Refused(
                if error.code() == McpMaterializationErrorCode::AuthorizationRequired {
                    "governed.mcp_token_rejected"
                } else {
                    LISTING_FAILED
                },
            ));
        }
    };
    let listed = declarations(server, &config, &session).await;
    drop(session);
    Ok(listed.map_or(Listing::Refused(LISTING_FAILED), Listing::Tools))
}

/// The selected tools' declarations from one `tools/list`.
async fn declarations(
    server: &GovernedServer,
    config: &RemoteMcpConfig,
    session: &McpSession,
) -> Result<Vec<ToolDeclaration>, McpMaterializationError> {
    let (tools, _) = discover_tools(config, session).await?;
    let tools = config.select_pinned_tools(tools)?;
    tools
        .iter()
        .map(|tool| ToolDeclaration::of(tool.as_ref(), server.name()))
        .collect()
}

/// The envelope's access token, copied into zeroizing memory.
fn access_token(
    envelope: &crate::toolkits::governed::CredentialEnvelope,
) -> Result<Zeroizing<String>, GovernedError> {
    envelope
        .material("access_token")
        .filter(|token| !token.is_empty())
        .map(|token| Zeroizing::new(token.to_owned()))
        .ok_or(GovernedError::MaterialNotAllowed)
}

fn admit(
    reference: &FrozenToolReference<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    tools: Vec<Arc<dyn Tool>>,
) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
    let admitted = admit_materialized_toolset(
        reference.toolkit_name(),
        reference.tool_type(),
        policy,
        tools,
    )
    .map_err(|error| materialized_toolset_error(error.code()))?;
    Ok(Arc::new(admitted))
}

/// Main asked for the user's consent before the listing. The selected tools
/// (when the toolkit names them) are placeholders that raise the governed
/// authorization requirement; with no selection the requirement is a
/// discovery one, exactly as the legacy path pauses for a browser grant.
fn authorization_placeholders(
    reference: &FrozenToolReference<'_>,
    server: &GovernedServer,
    policy: &Arc<ToolAdmissionPolicy>,
    requirement: DelegatedAuthorizationRequirement,
    out: &mut GovernedMcpToolsets,
) -> Result<(), McpMaterializationError> {
    let config = &server.config;
    if config.selected_tools().is_empty() {
        return out
            .delegated_authorization
            .insert_discovery_requirement(requirement)
            .map_err(|()| invalid_configuration());
    }
    let names = config
        .selected_tools()
        .iter()
        .filter(|name| !contains_tool_name(config.excluded_tools(), name))
        .collect::<Vec<_>>();
    if names.is_empty() {
        return Err(invalid_configuration());
    }
    let mut placeholders = Vec::with_capacity(names.len());
    for name in names {
        if policy.tool_decision(reference.tool_type(), name) == ToolAdmissionDecision::Allowed {
            out.delegated_authorization
                .insert(name, requirement.clone())
                .map_err(|()| invalid_configuration())?;
        }
        placeholders.push(
            Arc::new(McpAuthorizationRequiredTool::new(name, requirement.clone())) as Arc<dyn Tool>,
        );
    }
    out.toolsets.push(admit(reference, policy, placeholders)?);
    Ok(())
}

/// What the model sees of one listed tool. It holds no session, no client
/// and no token: it is copied from the listing and outlives it.
struct ToolDeclaration {
    name: Box<str>,
    description: Box<str>,
    parameters_schema: Option<Value>,
    response_schema: Option<Value>,
    long_running: bool,
    read_only: bool,
    concurrency_safe: bool,
}

impl ToolDeclaration {
    fn of(tool: &dyn Tool, toolkit_name: &str) -> Result<Self, McpMaterializationError> {
        Ok(Self {
            name: tool.name().into(),
            description: selection_description(tool, toolkit_name)?.into_boxed_str(),
            parameters_schema: tool.parameters_schema(),
            response_schema: tool.response_schema(),
            long_running: tool.is_long_running(),
            read_only: tool.is_read_only(),
            concurrency_safe: tool.is_concurrency_safe(),
        })
    }
}

/// One governed MCP tool: every call redeems, checks and dials afresh.
struct GovernedMcpTool {
    declaration: ToolDeclaration,
    server: Arc<GovernedServer>,
    connector: Arc<dyn McpConnector>,
    client: Arc<GovernedOperationClient>,
}

impl GovernedMcpTool {
    async fn call(&self, ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        let name = &*self.declaration.name;
        // 1. The operation, before anything leaves the process.
        let operation = McpAdapter
            .operation_for(name, &args)
            .map_err(|_| GovernedError::OperationNotMapped.into_adk_error())?;
        let invocation = self
            .server
            .invocation(&self.client, ctx.function_call_id(), &operation)
            .map_err(GovernedError::into_adk_error)?;
        // 2. One redemption, checked against THIS server's resource URI.
        let envelope = match self
            .client
            .redeem_checked(
                &McpAdapter,
                self.server.name(),
                &self.server.target,
                invocation,
            )
            .await
        {
            Ok(Redeemed::Envelope(envelope)) => envelope,
            Ok(Redeemed::Authorization(requirement)) => {
                return Err(delegated_authorization_error(&requirement));
            }
            Err(error) => return Err(error.into_adk_error()),
        };
        let token = access_token(&envelope).map_err(GovernedError::into_adk_error)?;
        drop(envelope);
        // 3. A fresh session to the frozen endpoint, the token its only
        //    credential; the tool, the session and the token drop with this
        //    frame. The call goes through ADK's own MCP tool from a listing
        //    on this session: one execution path for every MCP tool.
        let config = self.server.session_config(token);
        let session = self
            .connector
            .connect_session(&config)
            .await
            .map_err(|error| session_error(&error))?;
        let (tools, _) = discover_tools(&config, &session)
            .await
            .map_err(|error| session_error(&error))?;
        // A tool changed since its approval is not served (#1501).
        let tool = crate::toolkits::mcp_pin::keep_pinned(tools, config.pinned_tools.as_ref())
            .into_iter()
            .find(|tool| tool.name() == name)
            .ok_or_else(|| GovernedError::ToolNotServed.into_adk_error())?;
        let bounded = BoundedMcpTool::new(
            tool,
            self.server.name(),
            self.server.timeout(),
            Arc::new(config.error_redaction()),
        )
        .map_err(|error| session_error(&error))?;
        let result = bounded.execute(ctx, args).await;
        drop(bounded);
        drop(session);
        drop(config);
        result
    }
}

/// A session failure as the model reads it: a stable code, never the
/// endpoint, a header or a body.
fn session_error(error: &McpMaterializationError) -> AdkError {
    if error.assertion_refusal().is_some() {
        return AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::Forbidden,
            crate::remote_assertion::ASSERTION_UNAVAILABLE,
            "the MCP call needs a signed agent assertion and none could be minted",
        );
    }
    if error.code() == McpMaterializationErrorCode::AuthorizationRequired {
        return AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::Forbidden,
            "governed.mcp_token_rejected",
            "the MCP server refused the governed credential",
        );
    }
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::Unavailable,
        "governed.mcp_unavailable",
        "the MCP server could not serve the governed call",
    )
}

#[async_trait]
impl Tool for GovernedMcpTool {
    fn name(&self) -> &str {
        &self.declaration.name
    }

    fn description(&self) -> &str {
        &self.declaration.description
    }

    fn enhanced_description(&self) -> String {
        self.declaration.description.to_string()
    }

    fn is_long_running(&self) -> bool {
        self.declaration.long_running
    }

    fn parameters_schema(&self) -> Option<Value> {
        self.declaration.parameters_schema.clone()
    }

    fn response_schema(&self) -> Option<Value> {
        self.declaration.response_schema.clone()
    }

    fn is_read_only(&self) -> bool {
        self.declaration.read_only
    }

    fn is_concurrency_safe(&self) -> bool {
        self.declaration.concurrency_safe
    }

    async fn execute(&self, ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        self.call(ctx, args).await
    }
}

/// Test seam: the tool list the governed toolset declares.
#[cfg(test)]
pub(crate) async fn declared_names(toolset: &Arc<dyn Toolset>) -> Vec<String> {
    let context: Arc<dyn adk_core::ReadonlyContext> =
        Arc::new(adk_tool::SimpleToolContext::new("governed-mcp-test"));
    toolset
        .tools(context)
        .await
        .expect("tools")
        .iter()
        .map(|tool| tool.name().to_owned())
        .collect()
}
