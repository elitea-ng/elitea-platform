//! The platform's internal MCP tool categories (the internal builders:
//! applications, toolkits, configurations, ...) over
//! `elitea.runtime.v1.InternalToolService` on elitea-main's platform
//! listener (#1359 U5). It replaced the MCP Streamable HTTP session to
//! `<platform>/app/{project_id}/mcp/{scope}`: each listing and each call is
//! one RPC that elitea-main runs through the same MCP handler, so the tools
//! and their results are the MCP shapes, converted here exactly as ADK's
//! [`adk_tool::McpToolset`] converts them for a remote server.
//!
//! The frozen endpoint still names the category: its path is
//! `/app/{project_id}/mcp/{scope}`. Main writes it on the reserved origin
//! [`INTERNAL_TOOLS_ORIGIN`], which no network resolves, so an endpoint on
//! it can only be served here. The bearer is the toolkit's `Authorization`
//! static header (the per-execution actor token Main materialized).

use std::sync::Arc;

use adk_core::{AdkError, ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::mcp::rmcp::model::{
    CallToolResult, ContentBlock, ResourceContents, Tool as McpTool, ToolAnnotations,
};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use bytes::Bytes;
use elitea_platform_rpc::Bearer;
use elitea_platform_rpc::internal_tools::{InternalToolClient, Outcome, Scope, ToolCallError};
use serde_json::{Value, json};

/// The origin Main writes an internal builder's endpoint on. `.invalid`
/// never resolves (RFC 6761), so nothing but the internal tool service can
/// serve it.
pub const INTERNAL_TOOLS_ORIGIN: &str = "https://internal-tools.elitea.invalid";

/// Whether `endpoint` is on [`INTERNAL_TOOLS_ORIGIN`] exactly (scheme, host
/// and default port, no user information).
#[must_use]
pub(crate) fn is_internal_tools_endpoint(endpoint: &str) -> bool {
    reqwest_mcp::Url::parse(endpoint).is_ok_and(|url| {
        url.scheme() == "https"
            && url.host_str() == Some("internal-tools.elitea.invalid")
            && url.port().is_none()
            && url.username().is_empty()
            && url.password().is_none()
    })
}

/// The category an endpoint names: `/app/{project_id}/mcp[/{scope}]`, the
/// scope being the rest of the path (empty for the bare endpoint).
pub(crate) fn scope_of(endpoint: &str) -> Option<Scope> {
    let url = reqwest_mcp::Url::parse(endpoint).ok()?;
    if url.query().is_some() || url.fragment().is_some() {
        return None;
    }
    let path = url.path().trim_end_matches('/');
    let rest = path.strip_prefix("/app/")?;
    let (project, rest) = rest.split_once('/')?;
    let project_id = project
        .parse::<i64>()
        .ok()
        .filter(|id| *id > 0 && project.bytes().all(|byte| byte.is_ascii_digit()))?;
    let scope = match rest.strip_prefix("mcp") {
        Some("") => "",
        Some(scope) => scope.strip_prefix('/')?,
        None => return None,
    };
    if scope
        .split('/')
        .any(|segment| segment.is_empty() || matches!(segment, "." | ".."))
        && !scope.is_empty()
    {
        return None;
    }
    Some(Scope {
        project_id,
        scope: scope.to_owned(),
    })
}

/// One internal category's tools, listed and called over the service.
pub(crate) struct InternalMcpToolset {
    name: String,
    client: InternalToolClient,
    bearer: Bearer,
    scope: Scope,
}

impl InternalMcpToolset {
    pub(crate) fn new(
        name: &str,
        client: InternalToolClient,
        bearer: Bearer,
        scope: Scope,
    ) -> Self {
        Self {
            name: name.to_owned(),
            client,
            bearer,
            scope,
        }
    }
}

/// The text of a call or listing that produced no MCP answer. Never the
/// refusal body: it is the platform's, and the model sees this text.
fn hop_failure(error: &ToolCallError) -> String {
    match error {
        ToolCallError::Refused(refusal) => format!(
            "the platform refused the request (HTTP {})",
            refusal.http_status
        ),
        ToolCallError::Hop(_) => "the platform's internal tool service is unavailable".to_owned(),
        ToolCallError::Protocol(reason) => (*reason).to_owned(),
    }
}

#[derive(serde::Deserialize)]
struct Listing {
    tools: Vec<McpTool>,
}

#[async_trait]
impl Toolset for InternalMcpToolset {
    fn name(&self) -> &str {
        &self.name
    }

    async fn tools(&self, _ctx: Arc<dyn ReadonlyContext>) -> adk_core::Result<Vec<Arc<dyn Tool>>> {
        let listed = match self.client.list_tools(&self.bearer, &self.scope).await {
            Ok(Outcome::Result(result)) => result,
            Ok(Outcome::Error(error)) => {
                return Err(AdkError::tool(format!(
                    "Failed to list MCP tools: Mcp error: {}: {}",
                    error.code, error.message
                )));
            }
            Err(error) => {
                return Err(AdkError::tool(format!(
                    "Failed to list MCP tools: {}",
                    hop_failure(&error)
                )));
            }
        };
        let listing: Listing = serde_json::from_slice(&listed)
            .map_err(|_| AdkError::tool("Failed to list MCP tools: an invalid tool listing"))?;
        Ok(listing
            .tools
            .into_iter()
            .map(|tool| {
                Arc::new(InternalMcpTool {
                    name: tool.name.to_string(),
                    description: tool.description.map(|d| d.to_string()).unwrap_or_default(),
                    input_schema: Some(Value::Object(tool.input_schema.as_ref().clone())),
                    output_schema: tool
                        .output_schema
                        .map(|schema| Value::Object(schema.as_ref().clone())),
                    annotations: tool.annotations,
                    client: self.client.clone(),
                    bearer: self.bearer.clone(),
                    scope: self.scope.clone(),
                }) as Arc<dyn Tool>
            })
            .collect())
    }
}

/// `(read_only, concurrency_safe)` from the tool's hints, as ADK reads them.
fn tool_safety(annotations: Option<&ToolAnnotations>) -> (bool, bool) {
    let read_only = annotations
        .and_then(|value| value.read_only_hint)
        .unwrap_or(false);
    let idempotent = annotations
        .and_then(|value| value.idempotent_hint)
        .unwrap_or(false);
    (read_only, read_only || idempotent)
}

struct InternalMcpTool {
    name: String,
    description: String,
    input_schema: Option<Value>,
    output_schema: Option<Value>,
    annotations: Option<ToolAnnotations>,
    client: InternalToolClient,
    bearer: Bearer,
    scope: Scope,
}

#[async_trait]
impl Tool for InternalMcpTool {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        tool_safety(self.annotations.as_ref()).0
    }

    fn is_concurrency_safe(&self) -> bool {
        tool_safety(self.annotations.as_ref()).1
    }

    fn parameters_schema(&self) -> Option<Value> {
        self.input_schema.clone()
    }

    fn response_schema(&self) -> Option<Value> {
        self.output_schema.clone()
    }

    async fn execute(&self, _ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        let arguments = match args {
            Value::Null => Bytes::new(),
            Value::Object(map) if map.is_empty() => Bytes::new(),
            Value::Object(map) => Bytes::from(
                serde_json::to_vec(&map)
                    .map_err(|_| AdkError::tool("Tool arguments must be an object"))?,
            ),
            _ => return Err(AdkError::tool("Tool arguments must be an object")),
        };
        let failed = |reason: String| {
            AdkError::tool(format!("Failed to call MCP tool '{}': {reason}", self.name))
        };
        // Progress frames only say the call is running: skipped. Dropping
        // the future (the bounded timeout) cancels the call on the platform.
        let outcome = self
            .client
            .call_tool(&self.bearer, &self.scope, &self.name, arguments)
            .await
            .map_err(|error| failed(hop_failure(&error)))?
            .outcome()
            .await
            .map_err(|error| failed(hop_failure(&error)))?;
        let result = match outcome {
            Outcome::Result(result) => result,
            Outcome::Error(error) => {
                return Err(failed(format!(
                    "Mcp error: {}: {}",
                    error.code, error.message
                )));
            }
        };
        let result: CallToolResult = serde_json::from_slice(&result).map_err(|_| {
            AdkError::tool(format!(
                "MCP tool '{}' result invalid: not a tool result",
                self.name
            ))
        })?;
        if result.is_error.unwrap_or(false) {
            let mut message = format!("MCP tool '{}' execution failed", self.name);
            if let Some(text) = result.content.iter().find_map(ContentBlock::as_text) {
                message.push_str(": ");
                message.push_str(&text.text);
            }
            return Err(AdkError::tool(message));
        }
        call_result_value(&result).map_err(|error| {
            AdkError::tool(format!("MCP tool '{}' result invalid: {error}", self.name))
        })
    }
}

/// Every MCP content block in ADK's multimodal tool-result envelope, as
/// `adk_tool`'s `McpToolset` builds it (`FunctionResponseData::from_tool_result`
/// reads this shape).
fn call_result_value(result: &CallToolResult) -> Result<Value, String> {
    let mut text_parts = Vec::new();
    let mut inline_data = Vec::new();
    let mut file_data = Vec::new();
    for content in &result.content {
        match content {
            ContentBlock::Text(text) => text_parts.push(text.text.clone()),
            ContentBlock::Image(image) => {
                let data = STANDARD
                    .decode(&image.data)
                    .map_err(|error| format!("invalid MCP image base64: {error}"))?;
                inline_data.push(json!({ "mime_type": image.mime_type, "data": data }));
            }
            ContentBlock::Audio(audio) => {
                let data = STANDARD
                    .decode(&audio.data)
                    .map_err(|error| format!("invalid MCP audio base64: {error}"))?;
                inline_data.push(json!({ "mime_type": audio.mime_type, "data": data }));
            }
            ContentBlock::Resource(resource) => match &resource.resource {
                ResourceContents::TextResourceContents {
                    uri,
                    mime_type,
                    text,
                    ..
                } => {
                    text_parts.push(text.clone());
                    file_data.push(json!({
                        "mime_type": mime_type.as_deref().unwrap_or("text/plain"),
                        "file_uri": uri,
                    }));
                }
                ResourceContents::BlobResourceContents {
                    uri,
                    mime_type,
                    blob,
                    ..
                } => {
                    let data = STANDARD
                        .decode(blob)
                        .map_err(|error| format!("invalid MCP resource base64: {error}"))?;
                    let mime_type = mime_type.as_deref().unwrap_or("application/octet-stream");
                    inline_data.push(json!({ "mime_type": mime_type, "data": data }));
                    file_data.push(json!({ "mime_type": mime_type, "file_uri": uri }));
                }
                _ => return Err("unsupported MCP embedded resource content".to_owned()),
            },
            ContentBlock::ResourceLink(link) => file_data.push(json!({
                "mime_type": link.mime_type.as_deref().unwrap_or("application/octet-stream"),
                "file_uri": link.uri,
            })),
            _ => {}
        }
    }
    let output = match (&result.structured_content, text_parts.is_empty()) {
        (Some(structured), true) => json!({ "output": structured }),
        (Some(structured), false) => json!({ "output": structured, "text": text_parts }),
        (None, false) => json!({ "output": text_parts.join("\n") }),
        (None, true) if !inline_data.is_empty() || !file_data.is_empty() => Value::Null,
        (None, true) => return Err("MCP tool returned no content".to_owned()),
    };
    if inline_data.is_empty() && file_data.is_empty() {
        Ok(output)
    } else {
        Ok(json!({
            "response": output,
            "inline_data": inline_data,
            "file_data": file_data,
        }))
    }
}
