//! The remote MCP server adapter (#1123). A remote MCP toolkit is not a
//! configured family: its tools are the server's own, discovered at run
//! time, so this adapter maps operations and the target only. The session
//! side (one redeemed listing per build, one redeemed session per call)
//! lives in `toolkits::mcp::governed`, next to the MCP client it drives.
//!
//! - `mcp.tools.list` (read, resource kind `server`, EMPTY resource): the one
//!   listing a governed build makes.
//! - `mcp.tool.call` (write, resource kind `tool`): every `tools/call`, bound
//!   to `mcp:tool/<tool name>`. A tool may change anything, so a call is a
//!   write.
//!
//! The target is the server's canonical resource URI
//! ([`canonical_resource_uri`]), the string Main binds the MCP access token
//! to, so a token released for one server is never sent to another.

use std::sync::Arc;

use adk_core::Toolset;
use serde_json::{Map, Value};

use crate::toolkits::governed::{
    AuthAdapter, BoundOperation, CredentialClass, CredentialEnvelope, GovernedBuild, GovernedError,
    OperationNotMapped, TargetKind, ToolOperation, canonical_resource_uri, resource_identifier,
};

/// The provider type of a remote MCP toolkit.
pub const MCP_PROVIDER_TYPE: &str = "mcp";
/// The listing operation.
pub const MCP_TOOLS_LIST: &str = "mcp.tools.list";
/// The resource kind of the listing operation.
pub const MCP_SERVER_RESOURCE_KIND: &str = "server";
/// The call operation.
pub const MCP_TOOL_CALL: &str = "mcp.tool.call";
/// The resource kind of the call operation.
pub const MCP_TOOL_RESOURCE_KIND: &str = "tool";

const ACCESS_TOKEN: &[&str] = &["access_token"];

/// The row every `tools/call` binds through: the resource is the tool.
const CALL: ToolOperation = ToolOperation::new("", MCP_TOOL_CALL, MCP_TOOL_RESOURCE_KIND, None);

/// The remote MCP server adapter.
pub struct McpAdapter;

impl McpAdapter {
    /// The listing operation of one governed build: empty resource, digest
    /// of `{}`.
    ///
    /// # Errors
    ///
    /// [`OperationNotMapped`] only if `{}` cannot be encoded.
    pub fn listing_operation() -> Result<BoundOperation, OperationNotMapped> {
        BoundOperation::new(MCP_TOOLS_LIST, String::new(), &Value::Object(Map::new()))
    }
}

impl AuthAdapter for McpAdapter {
    fn provider_type(&self) -> &'static str {
        MCP_PROVIDER_TYPE
    }

    fn target_kind(&self) -> TargetKind {
        TargetKind::ResourceUri
    }

    fn material_keys(&self, class: CredentialClass) -> &'static [&'static str] {
        match class {
            CredentialClass::OauthDelegated => ACCESS_TOKEN,
            CredentialClass::StaticSecret => &[],
        }
    }

    /// No probe runs for an MCP server: a listing needs the user's consent.
    fn probe_operation(&self) -> &'static str {
        ""
    }

    /// No fixed table: a server's tools are its own.
    fn tool_operations(&self) -> &'static [ToolOperation] {
        &[]
    }

    fn configured_target(&self, settings: &Map<String, Value>) -> Result<String, GovernedError> {
        settings
            .get("url")
            .and_then(Value::as_str)
            .and_then(canonical_resource_uri)
            .ok_or(GovernedError::InvalidConfiguration)
    }

    /// Every tool of the server is `mcp.tool.call` on `mcp:tool/<name>`. A
    /// name that cannot be a bounded resource identifier is not mapped.
    fn operation_for(
        &self,
        tool_name: &str,
        arguments: &Value,
    ) -> Result<BoundOperation, OperationNotMapped> {
        let name = resource_identifier(&Value::String(tool_name.to_owned()))
            .filter(|name| name == tool_name)
            .ok_or(OperationNotMapped)?;
        let resource = CALL.resource(MCP_PROVIDER_TYPE, &name);
        if resource.is_empty() {
            return Err(OperationNotMapped);
        }
        BoundOperation::new(MCP_TOOL_CALL, resource, arguments)
    }

    /// Never a configured family: a remote MCP connection is served by the
    /// governed MCP session path, so a configured toolkit that names this
    /// type is refused as not governable.
    fn build_client(
        &self,
        _build: &GovernedBuild<'_>,
        _envelope: &CredentialEnvelope,
    ) -> Result<Arc<dyn Toolset>, GovernedError> {
        Err(GovernedError::ToolkitNotGovernable)
    }
}
