//! `RemoteToolkit` (ADR-0029 decision 3): a credentialed toolkit's tools
//! as adk tools that run on a cloud worker through
//! `executeRemoteToolkitTool` (decision 5b). No credential reaches the
//! desktop: the call names the toolkit by its opaque `toolkit_ref`.
//!
//! One `Idempotency-Key` per call, reused on every retry of it (no answer,
//! 503, 504, 409 `remote_toolkit_in_progress`), so a retry joins the run
//! the first attempt admitted and never runs the tool twice. A sensitive
//! tool answers 409 `confirmation_required`: the person is asked through
//! the approval channel, and an approved call is sent again, as a new call
//! with its own key, echoing the `interrupt_id`.

use std::sync::Arc;
use std::time::Duration;

use adk_core::{Tool, ToolContext, Toolset};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use elitea_agent_runtime::host::{
    ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError, ToolProvider, ToolsetRequest,
};
use serde_json::{Value, json};

use super::api::PlatformApi;
use super::approvals::REMOTE_PAYLOAD_KEY;
use super::definition::RemoteToolSpec;

/// The toolset's name.
pub const TOOLSET_NAME: &str = "remote_toolkits";

/// Retries of one call.
#[derive(Clone, Copy, Debug)]
pub struct RetryPolicy {
    pub attempts: u32,
    pub delay: Duration,
}

impl Default for RetryPolicy {
    fn default() -> Self {
        Self {
            attempts: 6,
            delay: Duration::from_secs(2),
        }
    }
}

/// What every remote call of one turn shares.
pub struct RemoteContext {
    pub api: Arc<PlatformApi>,
    pub project_id: i64,
    pub execution_id: String,
    pub application_id: i64,
    pub version_id: i64,
    pub prompt: Arc<dyn ApprovalChannel>,
    pub retry: RetryPolicy,
}

pub struct RemoteToolkitTool {
    spec: RemoteToolSpec,
    description: String,
    context: Arc<RemoteContext>,
}

impl RemoteToolkitTool {
    #[must_use]
    pub fn new(spec: RemoteToolSpec, context: Arc<RemoteContext>) -> Self {
        let what = spec.tool_name.as_deref().map_or_else(
            || format!("Call any tool of the {} toolkit", spec.toolkit_name),
            |tool| format!("{}: {tool}", spec.toolkit_name),
        );
        let description = if spec.description.is_empty() {
            format!("{what} (runs on the Elitea platform).")
        } else {
            format!("{what} (runs on the Elitea platform). {}", spec.description)
        };
        Self {
            spec,
            description,
            context,
        }
    }
}

fn error_result(code: &str, message: &str) -> Value {
    json!({ "status": "error", "code": code, "message": message })
}

#[async_trait]
impl Tool for RemoteToolkitTool {
    fn name(&self) -> &str {
        &self.spec.exposed_name
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(if self.spec.tool_name.is_some() {
            json!({ "type": "object", "properties": {}, "additionalProperties": true })
        } else {
            json!({
                "type": "object",
                "properties": {
                    "tool_name": { "type": "string", "description": "The toolkit's tool to run." },
                    "arguments": { "type": "object", "description": "That tool's arguments." }
                },
                "required": ["tool_name"]
            })
        })
    }

    async fn execute(&self, ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        let (tool_name, arguments) = match &self.spec.tool_name {
            Some(name) => (name.clone(), args),
            None => {
                let Some(name) = args.get("tool_name").and_then(Value::as_str) else {
                    return Ok(error_result("invalid_arguments", "`tool_name` is required"));
                };
                let arguments = args.get("arguments").cloned().unwrap_or_else(|| json!({}));
                (name.to_owned(), arguments)
            }
        };
        if !arguments.is_object() {
            return Ok(error_result(
                "invalid_arguments",
                "the arguments must be one JSON object",
            ));
        }
        Ok(call(
            &self.context,
            &self.spec,
            ctx.function_call_id(),
            &tool_name,
            arguments,
        )
        .await)
    }
}

/// Run one remote call to its end, the person's confirmation included.
pub async fn call(
    context: &RemoteContext,
    spec: &RemoteToolSpec,
    call_id: &str,
    tool_name: &str,
    arguments: Value,
) -> Value {
    let mut key = uuid::Uuid::new_v4().to_string();
    let mut confirmation: Option<Value> = None;
    let mut attempts = 0;
    loop {
        let mut body = json!({
            "execution_id": context.execution_id,
            "application_id": context.application_id,
            "version_id": context.version_id,
            "toolkit_ref": spec.toolkit_ref,
            "tool_name": tool_name,
            "arguments": arguments,
        });
        if let Some(confirmation) = &confirmation {
            body["confirmation"] = confirmation.clone();
        }
        attempts += 1;
        let answer = context
            .api
            .remote_toolkit_call(context.project_id, spec.toolkit_id, &key, &body)
            .await;
        let retry = match &answer {
            Err(_) => true,
            Ok(answer) => {
                let code = answer.body.get("error").and_then(Value::as_str);
                matches!(answer.status, 429 | 502 | 503 | 504)
                    || (answer.status == 409 && code == Some("remote_toolkit_in_progress"))
            }
        };
        if retry {
            if attempts >= context.retry.attempts {
                return error_result(
                    "remote_toolkit_unavailable",
                    "the platform did not finish the call; try again later",
                );
            }
            tokio::time::sleep(context.retry.delay).await;
            continue;
        }
        let Ok(answer) = answer else {
            continue;
        };
        let code = answer
            .body
            .get("error")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned();
        let message = answer
            .body
            .get("message")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned();
        match (answer.status, code.as_str()) {
            (200, _) => {
                let ok = answer.body.get("ok") == Some(&Value::Bool(true));
                let mut result = json!({
                    "status": if ok { "ok" } else { "error" },
                    "result": answer.body.get("result").cloned().unwrap_or(Value::Null),
                });
                if answer.body.get("truncated") == Some(&Value::Bool(true)) {
                    result["truncated"] = json!(true);
                }
                if !ok {
                    result["code"] = json!("tool_error");
                    result["message"] = json!(message);
                }
                return result;
            }
            (409, "confirmation_required") if confirmation.is_none() => {
                let interrupt = answer
                    .body
                    .get("hitl_interrupt")
                    .cloned()
                    .unwrap_or_default();
                let Some(interrupt_id) = interrupt
                    .get("interrupt_id")
                    .and_then(Value::as_str)
                    .map(str::to_owned)
                else {
                    return error_result("invalid_response", "the confirmation request had no id");
                };
                if !confirmed(context, spec, call_id, tool_name, &interrupt, &message).await {
                    return error_result("rejected", "the person rejected this call");
                }
                confirmation = Some(json!({
                    "approved": true,
                    "approved_at": chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Secs, true),
                    "interrupt_id": interrupt_id,
                }));
                // The approved call is a new request: its own key, its own retries.
                key = uuid::Uuid::new_v4().to_string();
                attempts = 0;
            }
            (409, "mcp_authorization_required") => {
                return error_result(
                    "authorization_required",
                    "this tool needs your authorisation; authorise it in the web app, then try again",
                );
            }
            _ => {
                let code = if code.is_empty() {
                    format!("http_{}", answer.status)
                } else {
                    code
                };
                let message = if message.is_empty() {
                    "the platform refused the call".to_owned()
                } else {
                    message
                };
                return error_result(&code, &message);
            }
        }
    }
}

async fn confirmed(
    context: &RemoteContext,
    spec: &RemoteToolSpec,
    call_id: &str,
    tool_name: &str,
    interrupt: &Value,
    message: &str,
) -> bool {
    let text = |key: &str| {
        interrupt
            .get(key)
            .and_then(Value::as_str)
            .unwrap_or_default()
    };
    let title = if text("action_label").is_empty() {
        format!("Run {tool_name} on {}", spec.toolkit_name)
    } else {
        text("action_label").to_owned()
    };
    let reason = if text("policy_message").is_empty() {
        message
    } else {
        text("policy_message")
    };
    let request = ApprovalRequest {
        subject: call_id.to_owned(),
        message: title.clone(),
        available_actions: vec!["approve".to_owned(), "reject".to_owned()],
        payload: json!({ REMOTE_PAYLOAD_KEY: {
            "tool": tool_name,
            "toolkit": spec.toolkit_name,
            "title": title,
            "detail": format!("{} · {tool_name}", spec.toolkit_name),
            "reason": reason,
        }}),
    };
    matches!(
        context.prompt.request(request).await,
        Ok(ApprovalOutcome::Decided { action, .. }) if action == "approve"
    )
}

/// The host's remote tools for one turn.
pub struct RemoteToolProvider {
    tools: Vec<Arc<dyn Tool>>,
}

impl RemoteToolProvider {
    #[must_use]
    pub fn new(specs: &[RemoteToolSpec], context: &Arc<RemoteContext>) -> Self {
        Self {
            tools: specs
                .iter()
                .map(|spec| {
                    Arc::new(RemoteToolkitTool::new(spec.clone(), context.clone())) as Arc<dyn Tool>
                })
                .collect(),
        }
    }
}

#[async_trait]
impl ToolProvider for RemoteToolProvider {
    async fn toolsets(
        &self,
        _request: &ToolsetRequest,
    ) -> Result<Vec<Arc<dyn Toolset>>, HostError> {
        if self.tools.is_empty() {
            return Ok(Vec::new());
        }
        Ok(vec![Arc::new(BasicToolset::new(
            TOOLSET_NAME,
            self.tools.clone(),
        ))])
    }
}
