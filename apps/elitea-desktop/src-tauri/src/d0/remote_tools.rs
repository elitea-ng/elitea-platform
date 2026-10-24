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
//!
//! An MCP server that needs the person's sign-in answers 409
//! `mcp_authorization_required` (issue 1231): the person is asked through
//! the same channel (an `mcp_authorization` question: sign in in the
//! browser, on the web app's page for that toolkit, then Continue or
//! Cancel; [`MCP_AUTHORIZATION_WAIT`] at most). Continue sends the call
//! again as a new call; Cancel, the timeout or a second
//! `mcp_authorization_required` end only that tool call. The desktop never
//! talks to the MCP server and never holds what the browser signs in with:
//! of the 409 it keeps the toolkit's name and the server's address (scheme,
//! host and path), nothing else.
//!
//! A `web` or `host_intel` toolkit (#1238) is offered only while the
//! `local_work` policy's `web` / `host_intel` is on ([`offered`]), and each
//! call passes the session's approval rules first: web reads are allowed,
//! host-intel calls are asked ("approve for this session" answers the rest
//! of the app session). Its result is framed as untrusted data under the
//! thread's nonce. The search and fetch run on the cloud worker; no key and
//! no fetch happens on the device.

use std::sync::Arc;
use std::time::Duration;

use adk_core::{Tool, ToolContext, Toolset};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use elitea_agent_runtime::host::{
    ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError, ToolProvider, ToolsetRequest,
};
use serde_json::{Value, json};

use elitea_local_tools::approvals::{APPROVE_SESSION, RulesEngine, ToolCall, ToolKind, Verdict};
use elitea_local_tools::policy::LocalWorkPolicy;

use super::api::PlatformApi;
use super::approvals::{
    MCP_AUTHORIZATION_PAYLOAD_KEY, REMOTE_PAYLOAD_KEY, TIMEOUT_ACTION, WAIT_PAYLOAD_KEY,
};
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

/// How long an MCP sign-in question waits for Continue or Cancel.
pub const MCP_AUTHORIZATION_WAIT: Duration = Duration::from_secs(10 * 60);

/// What every remote call of one turn shares.
pub struct RemoteContext {
    pub api: Arc<PlatformApi>,
    pub project_id: i64,
    pub execution_id: String,
    pub application_id: i64,
    pub version_id: i64,
    pub prompt: Arc<dyn ApprovalChannel>,
    pub retry: RetryPolicy,
    /// How long an MCP sign-in waits ([`MCP_AUTHORIZATION_WAIT`]).
    pub authorization_wait: Duration,
    /// The session's approval rules, for the web toolkits' calls; `None`
    /// refuses them.
    pub rules: Option<Arc<RulesEngine>>,
    /// The thread's nonce, which frames a web toolkit's result.
    pub nonce: String,
}

/// The tag a web toolkit's result is framed in.
const WEB_RESULT_TAG: &str = "web_result";

/// Whether the turn offers `spec`: a `web` or `host_intel` toolkit only
/// while the policy allows it, every other toolkit as before.
#[must_use]
pub fn offered(spec: &RemoteToolSpec, policy: &LocalWorkPolicy) -> bool {
    match ToolKind::web_toolkit(&spec.toolkit_type) {
        Some(ToolKind::WebRead) => policy.web,
        Some(_) => policy.host_intel,
        None => true,
    }
}

/// What the approval card shows of a web call: the query, URL or address.
fn web_target(arguments: &Value) -> String {
    ["query", "url", "ip"]
        .iter()
        .find_map(|key| arguments.get(*key).and_then(Value::as_str))
        .map(|text| cut(text, 512))
        .unwrap_or_default()
}

/// The approval rules' answer to a web call. `Err` is the result to return.
async fn web_gate(
    context: &RemoteContext,
    spec: &RemoteToolSpec,
    call_id: &str,
    tool_name: &str,
    arguments: &Value,
    kind: ToolKind,
) -> Result<(), Value> {
    let refused = |reason: &str| error_result("local_work_policy_refused", reason);
    let Some(rules) = context.rules.as_ref() else {
        return Err(refused("the web tools are disabled by policy"));
    };
    let call = ToolCall::new(kind);
    let decision = rules.decide(&call);
    match decision.verdict {
        Verdict::Allow => Ok(()),
        Verdict::Deny => Err(refused(&decision.reason)),
        Verdict::Ask => {
            let title = format!("Run {tool_name} on {}", spec.toolkit_name);
            let request = ApprovalRequest {
                subject: call_id.to_owned(),
                message: title.clone(),
                available_actions: vec![
                    "approve".to_owned(),
                    APPROVE_SESSION.to_owned(),
                    "reject".to_owned(),
                ],
                payload: json!({ REMOTE_PAYLOAD_KEY: {
                    "tool": tool_name,
                    "toolkit": spec.toolkit_name,
                    "title": title,
                    "detail": web_target(arguments),
                    "reason": decision.reason,
                }}),
            };
            match context.prompt.request(request).await {
                Ok(ApprovalOutcome::Decided { action, .. }) if action == "approve" => Ok(()),
                Ok(ApprovalOutcome::Decided { action, .. }) if action == APPROVE_SESSION => {
                    // Only a host-intel call can be remembered for a session.
                    let _ = rules.remember_for_session(&call);
                    Ok(())
                }
                Ok(ApprovalOutcome::Decided { value, .. }) => {
                    let mut result = error_result("rejected", "the person rejected this call");
                    if let Value::Object(fields) = &mut result {
                        fields.extend(elitea_local_tools::session::declined_details(
                            elitea_local_tools::session::feedback_of(&value).as_deref(),
                        ));
                    }
                    Err(result)
                }
                _ => Err(error_result("rejected", "the person rejected this call")),
            }
        }
    }
}

/// A web toolkit's result as untrusted data: one block under the thread's
/// nonce, which the fetched text cannot close.
fn framed_web_result(nonce: &str, spec: &RemoteToolSpec, result: &Value) -> Value {
    let text = match result {
        Value::String(text) => text.clone(),
        other => other.to_string(),
    };
    json!(format!(
        "{}\n{}",
        super::framing::ends_only_at(WEB_RESULT_TAG, nonce),
        super::framing::block(WEB_RESULT_TAG, nonce, "toolkit", &spec.toolkit_name, &text)
    ))
}

pub struct RemoteToolkitTool {
    spec: RemoteToolSpec,
    description: String,
    context: Arc<RemoteContext>,
}

/// The generic tool's description lists at most this many bytes of its
/// tools' descriptions; past it, only their names.
const MAX_LISTED_DESCRIPTIONS: usize = 6 * 1024;

impl RemoteToolkitTool {
    #[must_use]
    pub fn new(spec: RemoteToolSpec, context: Arc<RemoteContext>) -> Self {
        let what = spec.tool_name.as_deref().map_or_else(
            || format!("Call any tool of the {} toolkit", spec.toolkit_name),
            |tool| format!("{}: {tool}", spec.toolkit_name),
        );
        let mut description = if spec.description.is_empty() {
            format!("{what} (runs on the Elitea platform).")
        } else {
            format!("{what} (runs on the Elitea platform). {}", spec.description)
        };
        if spec.tool_name.is_none() && !spec.tools.is_empty() {
            description.push_str(&tools_listing(&spec));
        }
        Self {
            spec,
            description,
            context,
        }
    }
}

/// "Its tools: `a` — what a does; `b`; …", the names the platform server
/// listed, so the model can pick one for `tool_name`.
fn tools_listing(spec: &RemoteToolSpec) -> String {
    let mut listing = String::from(" Its tools (pass one as `tool_name`):");
    let mut described = 0;
    for tool in &spec.tools {
        listing.push_str("\n- ");
        listing.push_str(&tool.name);
        let text = tool.description.trim();
        if !text.is_empty() && described + text.len() <= MAX_LISTED_DESCRIPTIONS {
            described += text.len();
            listing.push_str(": ");
            listing.push_str(text);
        }
    }
    listing
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
            let mut tool_name =
                json!({ "type": "string", "description": "The toolkit's tool to run." });
            if !self.spec.tools.is_empty() {
                tool_name["enum"] = self.spec.tools.iter().map(|t| t.name.clone()).collect();
            }
            json!({
                "type": "object",
                "properties": {
                    "tool_name": tool_name,
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
    let web = ToolKind::web_toolkit(&spec.toolkit_type);
    if let Some(kind) = web
        && let Err(result) = web_gate(context, spec, call_id, tool_name, &arguments, kind).await
    {
        return result;
    }
    let mut key = uuid::Uuid::new_v4().to_string();
    let mut confirmation: Option<Value> = None;
    let mut authorization_reference: Option<String> = None;
    let mut authorized = false;
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
        if let Some(reference) = &authorization_reference {
            body["mcp_authorization_reference"] = json!(reference);
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
                let mut payload = answer.body.get("result").cloned().unwrap_or(Value::Null);
                if web.is_some() && !payload.is_null() {
                    payload = framed_web_result(&context.nonce, spec, &payload);
                }
                let mut result = json!({
                    "status": if ok { "ok" } else { "error" },
                    "result": payload,
                });
                if answer.body.get("truncated") == Some(&Value::Bool(true)) {
                    result["truncated"] = json!(true);
                }
                if !ok {
                    // A tool that raised says why in `error` (the answer has
                    // no `message`).
                    let message = if message.is_empty() {
                        code.clone()
                    } else {
                        message
                    };
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
                if let Err(feedback) =
                    confirmed(context, spec, call_id, tool_name, &interrupt, &message).await
                {
                    // #1225: the person's denial is a typed failure, with
                    // their feedback when they wrote one.
                    let mut result = error_result("rejected", "the person rejected this call");
                    if let Value::Object(fields) = &mut result {
                        fields.extend(elitea_local_tools::session::declined_details(
                            feedback.as_deref(),
                        ));
                    }
                    return result;
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
            (409, "mcp_authorization_required") if !authorized => {
                match authorize(context, spec, call_id, tool_name, &answer.body).await {
                    Authorization::Continue => {}
                    Authorization::Cancelled => {
                        return error_result(
                            "authorization_cancelled",
                            "the person did not sign in to this tool's MCP server, so the tool did not run",
                        );
                    }
                    Authorization::TimedOut => {
                        return error_result(
                            "authorization_timeout",
                            "nobody signed in to this tool's MCP server within 10 minutes, so the tool did not run",
                        );
                    }
                }
                authorized = true;
                authorization_reference = answer
                    .body
                    .pointer("/authorization_retry/mcp_authorization_reference")
                    .and_then(Value::as_str)
                    .filter(|reference| valid_authorization_reference(reference))
                    .map(str::to_owned);
                // The signed-in call is a new request: its own key, its own
                // retries, and a sensitive tool is confirmed again.
                confirmation = None;
                key = uuid::Uuid::new_v4().to_string();
                attempts = 0;
            }
            (409, "mcp_authorization_required") => {
                return error_result(
                    "authorization_required",
                    "this tool's MCP server still needs your sign-in; sign in in the Elitea web app, then try again",
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

/// Ask the person to confirm a sensitive remote call. `Err` is a denial,
/// with the person's feedback when they wrote one (#1225).
async fn confirmed(
    context: &RemoteContext,
    spec: &RemoteToolSpec,
    call_id: &str,
    tool_name: &str,
    interrupt: &Value,
    message: &str,
) -> Result<(), Option<String>> {
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
    match context.prompt.request(request).await {
        Ok(ApprovalOutcome::Decided { action, .. }) if action == "approve" => Ok(()),
        Ok(ApprovalOutcome::Decided { value, .. }) => {
            Err(elitea_local_tools::session::feedback_of(&value))
        }
        _ => Err(None),
    }
}

enum Authorization {
    Continue,
    Cancelled,
    TimedOut,
}

/// Ask the person to sign in to the MCP server behind `spec`, and wait.
async fn authorize(
    context: &RemoteContext,
    spec: &RemoteToolSpec,
    call_id: &str,
    tool_name: &str,
    answer: &Value,
) -> Authorization {
    // Allowlisted: the 409 may carry the server's OAuth metadata and its
    // WWW-Authenticate challenge; neither goes further than this function.
    let required = answer.get("authorization_required");
    let text = |key: &str| {
        required
            .and_then(|r| r.get(key))
            .and_then(Value::as_str)
            .unwrap_or_default()
    };
    let toolkit = if spec.toolkit_name.is_empty() {
        cut(text("toolkit_name"), 256)
    } else {
        spec.toolkit_name.clone()
    };
    let server_url = display_url(text("server_url")).unwrap_or_default();
    let title = format!("{toolkit} needs your sign-in");
    let mut question = json!({
        "tool": tool_name,
        "toolkit": toolkit,
        "title": title,
        "detail": if server_url.is_empty() { toolkit.clone() } else { server_url.clone() },
        "server_url": server_url,
    });
    if let Some(path) = authorize_path(context.project_id, spec, required) {
        question["authorize_path"] = json!(path);
    }
    let wait = context.authorization_wait;
    let request = ApprovalRequest {
        subject: call_id.to_owned(),
        message: title,
        available_actions: vec!["continue".to_owned(), "cancel".to_owned()],
        // The channel closes the question at the wait (the card goes and
        // the exchange records the timeout); the timeout below only bounds
        // a channel that does not.
        payload: json!({
            MCP_AUTHORIZATION_PAYLOAD_KEY: question,
            WAIT_PAYLOAD_KEY: u64::try_from(wait.as_millis()).unwrap_or(u64::MAX),
        }),
    };
    let backstop = wait.saturating_add(wait.min(Duration::from_secs(1)));
    match tokio::time::timeout(backstop, context.prompt.request(request)).await {
        Err(_) => Authorization::TimedOut,
        Ok(Ok(ApprovalOutcome::Decided { action, .. })) if action == "approve" => {
            Authorization::Continue
        }
        Ok(Ok(ApprovalOutcome::Decided { action, .. })) if action == TIMEOUT_ACTION => {
            Authorization::TimedOut
        }
        Ok(_) => Authorization::Cancelled,
    }
}

/// The web app's page that signs in to the toolkit: the MCP toolkit's page
/// for an MCP toolkit, else the toolkit's. The numbers come from the
/// definition, never from the 409. Whether it is an MCP toolkit is the
/// platform's word: the 409's `authorization_required.toolkit_type` (the
/// saved toolkit's type, which the platform checks against the run it
/// admitted) when it names this toolkit, else the definition's `type`; an
/// MCP type is `mcp` or `mcp_*`, the web app's own rule. A platform server
/// (toolkit id 0) has no page.
fn authorize_path(
    project_id: i64,
    spec: &RemoteToolSpec,
    required: Option<&Value>,
) -> Option<String> {
    if spec.toolkit_id <= 0 || project_id <= 0 {
        return None;
    }
    let same_toolkit = |r: &&Value| match r.get("toolkit_id") {
        None | Some(Value::Null) => true,
        Some(Value::String(id)) => id.parse::<i64>().ok() == Some(spec.toolkit_id),
        Some(id) => id.as_i64() == Some(spec.toolkit_id),
    };
    let toolkit_type = required
        .filter(same_toolkit)
        .and_then(|r| r.get("toolkit_type"))
        .and_then(Value::as_str)
        .filter(|t| !t.is_empty())
        .unwrap_or(&spec.toolkit_type);
    let section = if toolkit_type == "mcp" || toolkit_type.starts_with("mcp_") {
        "mcps"
    } else {
        "toolkits"
    };
    Some(format!(
        "/app/{project_id}/{section}/all/{}",
        spec.toolkit_id
    ))
}

/// The server's address to show: http(s) only, without credentials, query
/// or fragment.
fn display_url(raw: &str) -> Option<String> {
    let mut url = url::Url::parse(raw).ok()?;
    if !matches!(url.scheme(), "http" | "https") {
        return None;
    }
    url.set_username("").ok()?;
    url.set_password(None).ok()?;
    url.set_query(None);
    url.set_fragment(None);
    Some(cut(url.as_str(), 2048))
}

/// An MCP authorisation reference (`toolkitcalltool.validMCPReference`):
/// 43 characters of `[A-Za-z0-9_-]`.
fn valid_authorization_reference(value: &str) -> bool {
    value.len() == 43
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}

fn cut(text: &str, max: usize) -> String {
    if text.len() <= max {
        return text.to_owned();
    }
    let mut end = max;
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    text[..end].to_owned()
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
