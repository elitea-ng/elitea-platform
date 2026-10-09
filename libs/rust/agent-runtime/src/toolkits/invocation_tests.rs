use std::collections::BTreeMap;
use std::future::pending;
use std::io::{self, Write};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use adk_core::{
    AdkError, ErrorCategory, ErrorComponent, ReadonlyContext, RetryHint, Tool, ToolContext, Toolset,
};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use serde_json::{Value, json};
use tokio::sync::Notify;
use tracing::instrument::WithSubscriber as _;
use tracing_subscriber::fmt::MakeWriter;
use tracing_subscriber::fmt::format::FmtSpan;

use super::invocation::{MaterializedToolsetErrorCode, admit_materialized_toolset};
use super::policy::ToolAdmissionPolicy;
use super::{delegated_authorization_error_fixture, delegated_authorization_requirement};

fn policy(blocked_tools: &[(&str, &[&str])]) -> Arc<ToolAdmissionPolicy> {
    let blocked_tools = blocked_tools
        .iter()
        .map(|(toolkit, tools)| {
            (
                (*toolkit).to_owned(),
                tools.iter().map(ToString::to_string).collect::<Vec<_>>(),
            )
        })
        .collect::<BTreeMap<_, _>>();
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked_tools).expect("fixture tool admission policy"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("elitea-tool-test")
            .with_session_id("session-1")
            .with_function_call_id("call-1"),
    )
}

#[tokio::test]
async fn native_toolset_filters_blocked_actions_and_preserves_adk_metadata() {
    let allowed = Arc::new(FixtureTool::new("read_issue"));
    let blocked = Arc::new(FixtureTool::new("github___Delete-Repo"));
    let toolset = admit_materialized_toolset(
        "github-19",
        "github",
        &policy(&[("GitHub", &["delete_repo"])]),
        vec![allowed, blocked.clone()],
    )
    .expect("admitted native toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("native ADK tools");

    assert_eq!(toolset.name(), "github-19");
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].name(), "read_issue");
    assert_eq!(tools[0].description(), "fixture description");
    assert!(tools[0].is_read_only());
    assert!(tools[0].is_concurrency_safe());
    assert_eq!(tools[0].required_scopes(), &["issues:read"]);
    assert_eq!(tools[0].parameters_schema(), Some(fixture_schema()));
    assert_eq!(blocked.calls.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn action_execution_preserves_call_identity_and_current_null_semantics() {
    let action = Arc::new(FixtureTool::new("read_issue"));
    let toolset =
        admit_materialized_toolset("github-19", "github", &policy(&[]), vec![action.clone()])
            .expect("admitted native toolset");
    let context = context();
    let readonly: Arc<dyn ReadonlyContext> = context.clone();
    let tool = toolset
        .tools(readonly)
        .await
        .expect("native ADK tools")
        .pop()
        .expect("fixture action");
    let invocation: Arc<dyn ToolContext> = context;

    let result = tool
        .execute(
            invocation,
            json!({
                "issue": 7,
                "optional": null,
                "nested": {"optional": null}
            }),
        )
        .await
        .expect("fixture action result");

    assert_eq!(result, json!({"call_id": "call-1", "ok": true}));
    assert_eq!(
        action
            .arguments
            .lock()
            .expect("fixture argument lock")
            .as_slice(),
        &[json!({"issue": 7, "nested": {"optional": null}})]
    );
}

#[tokio::test]
async fn invalid_and_over_limit_arguments_fail_before_the_action() {
    let action = Arc::new(FixtureTool::new("read_issue"));
    let toolset =
        admit_materialized_toolset("github-19", "github", &policy(&[]), vec![action.clone()])
            .expect("admitted native toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tool = toolset
        .tools(readonly)
        .await
        .expect("native ADK tools")
        .pop()
        .expect("fixture action");

    let invalid = tool
        .execute(context(), json!(["not", "an", "object"]))
        .await
        .expect_err("array arguments must fail");
    assert_eq!(invalid.code, "tool.arguments.invalid");

    let oversized = tool
        .execute(context(), json!({"secret": "x".repeat(64 * 1_024 + 1)}))
        .await
        .expect_err("oversized arguments must fail");
    assert_eq!(oversized.code, "tool.arguments.resource_exhausted");
    assert!(!format!("{oversized:?} {oversized}").contains(&"x".repeat(1_024)));

    let mut nested = json!({});
    for _ in 0..65 {
        nested = json!({"nested": nested});
    }
    let too_deep = tool
        .execute(context(), nested)
        .await
        .expect_err("deep arguments must fail");
    assert_eq!(too_deep.code, "tool.arguments.resource_exhausted");
    assert_eq!(action.calls.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn delegated_failures_keep_category_and_retry_without_secret_diagnostics() {
    let secret = "fixture-provider-secret";
    let action = Arc::new(FixtureTool::failing("write_issue", secret));
    let toolset = admit_materialized_toolset("github-19", "github", &policy(&[]), vec![action])
        .expect("admitted native toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tool = toolset
        .tools(readonly)
        .await
        .expect("native ADK tools")
        .pop()
        .expect("fixture action");

    let error = tool
        .execute(context(), json!({"title": "safe"}))
        .await
        .expect_err("fixture dependency failure");

    assert_eq!(error.component, ErrorComponent::Tool);
    assert_eq!(error.category, ErrorCategory::Unavailable);
    assert_eq!(error.code, "tool.execution.unavailable");
    assert!(error.is_retryable());
    assert_eq!(error.retry.retry_after_ms, Some(25));
    let diagnostics = format!("{error:?} {error}");
    assert!(!diagnostics.contains(secret));
    assert!(std::error::Error::source(&error).is_none());
}

#[tokio::test]
async fn configured_toolkit_policy_preserves_only_typed_delegated_authorization() {
    let action: Arc<dyn Tool> = Arc::new(DelegatedAuthorizationTool);
    let toolset =
        admit_materialized_toolset("team-documents", "sharepoint", &policy(&[]), vec![action])
            .expect("admitted delegated Toolkit");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tool = toolset
        .tools(readonly)
        .await
        .expect("native ADK tools")
        .pop()
        .expect("delegated action");

    let error = tool
        .execute(context(), json!({"document": "private-input"}))
        .await
        .expect_err("delegated authorization required");
    let requirement = delegated_authorization_requirement(&error)
        .expect("typed delegated authorization survives policy wrapping");
    assert_eq!(requirement.toolkit_type(), "sharepoint");
    assert_eq!(requirement.toolkit_name(), "Customer Support");
    assert_eq!(
        requirement.server_url(),
        "https://tenant.sharepoint.example.invalid/sites/support"
    );
    assert_eq!(
        requirement.resource_metadata_url(),
        Some(
            "https://login.microsoftonline.example.invalid/tenant/v2.0/.well-known/openid-configuration"
        )
    );
    assert!(!format!("{error:?} {error}").contains("private-input"));
}

#[tokio::test]
async fn tool_trace_contains_only_safe_correlation_and_outcome_fields() {
    let capture = CapturedOutput::default();
    let subscriber = tracing_subscriber::fmt()
        .without_time()
        .with_ansi(false)
        .with_target(false)
        .with_span_events(FmtSpan::CLOSE)
        .with_writer(capture.clone())
        .finish();
    let action = Arc::new(FixtureTool::new("read_issue"));
    let toolset = admit_materialized_toolset("github-19", "github", &policy(&[]), vec![action])
        .expect("admitted native toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tool = toolset
        .tools(readonly)
        .await
        .expect("native ADK tools")
        .pop()
        .expect("fixture action");

    tool.execute(
        context(),
        json!({"issue": 7, "body": "do-not-log-this-argument"}),
    )
    .with_subscriber(subscriber)
    .await
    .expect("traced tool call");

    let output = capture.text();
    assert!(output.contains("agent.tool.invoke"));
    assert!(output.contains("toolkit_type=github"));
    assert!(output.contains("tool_name=read_issue"));
    assert!(output.contains("function_call_id=call-1"));
    assert!(output.contains("outcome=\"succeeded\""));
    assert!(!output.contains("do-not-log-this-argument"));
}

#[tokio::test]
async fn dropping_execution_drops_the_delegated_future_without_detaching_work() {
    let started = Arc::new(Notify::new());
    let dropped = Arc::new(AtomicBool::new(false));
    let action: Arc<dyn Tool> = Arc::new(PendingTool {
        started: Arc::clone(&started),
        dropped: Arc::clone(&dropped),
    });
    let toolset = admit_materialized_toolset("github-19", "github", &policy(&[]), vec![action])
        .expect("admitted native toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tool = toolset
        .tools(readonly)
        .await
        .expect("native ADK tools")
        .pop()
        .expect("fixture action");

    let task = tokio::spawn(async move { tool.execute(context(), json!({})).await });
    started.notified().await;
    task.abort();
    let _ = task.await;

    assert!(dropped.load(Ordering::SeqCst));
}

#[test]
fn definitions_and_catalog_growth_are_bounded_with_data_free_errors() {
    let secret = format!("secret-{}", "x".repeat(1_024));
    let error = admit_materialized_toolset(
        "github-19",
        "github",
        &policy(&[]),
        vec![Arc::new(FixtureTool::new(&secret))],
    )
    .err()
    .expect("oversized identity must fail");
    assert_eq!(
        error.code(),
        MaterializedToolsetErrorCode::ResourceExhausted
    );
    assert!(!format!("{error:?} {error}").contains(&secret));

    let tools = (0..1_025)
        .map(|index| Arc::new(FixtureTool::new(&format!("tool-{index}"))) as Arc<dyn Tool>)
        .collect();
    assert_eq!(
        admit_materialized_toolset("github-19", "github", &policy(&[]), tools)
            .err()
            .expect("catalog bound")
            .code(),
        MaterializedToolsetErrorCode::ResourceExhausted
    );
}

struct FixtureTool {
    name: String,
    arguments: Mutex<Vec<Value>>,
    calls: AtomicUsize,
    failure_secret: Option<String>,
}

impl FixtureTool {
    fn new(name: &str) -> Self {
        Self {
            name: name.to_owned(),
            arguments: Mutex::new(Vec::new()),
            calls: AtomicUsize::new(0),
            failure_secret: None,
        }
    }

    fn failing(name: &str, secret: &str) -> Self {
        Self {
            failure_secret: Some(secret.to_owned()),
            ..Self::new(name)
        }
    }
}

#[async_trait]
impl Tool for FixtureTool {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &'static str {
        "fixture description"
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(fixture_schema())
    }

    fn required_scopes(&self) -> &[&str] {
        &["issues:read"]
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        self.arguments
            .lock()
            .expect("fixture argument lock")
            .push(arguments);
        if let Some(secret) = &self.failure_secret {
            return Err(AdkError::unavailable(
                ErrorComponent::Tool,
                "fixture.provider.secret",
                format!("dependency failed with {secret}"),
            )
            .with_retry(RetryHint {
                should_retry: true,
                retry_after_ms: Some(25),
                max_attempts: Some(2),
            }));
        }
        Ok(json!({
            "call_id": context.function_call_id(),
            "ok": true,
        }))
    }
}

struct PendingTool {
    started: Arc<Notify>,
    dropped: Arc<AtomicBool>,
}

struct DelegatedAuthorizationTool;

#[async_trait]
impl Tool for DelegatedAuthorizationTool {
    fn name(&self) -> &'static str {
        "read_document"
    }

    fn description(&self) -> &'static str {
        "delegated authorization fixture"
    }

    fn is_read_only(&self) -> bool {
        true
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_core::Result<Value> {
        Err(delegated_authorization_error_fixture("sharepoint"))
    }
}

struct ExecutionGuard(Arc<AtomicBool>);

impl Drop for ExecutionGuard {
    fn drop(&mut self) {
        self.0.store(true, Ordering::SeqCst);
    }
}

#[async_trait]
impl Tool for PendingTool {
    fn name(&self) -> &'static str {
        "pending_action"
    }

    fn description(&self) -> &'static str {
        "fixture pending action"
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_core::Result<Value> {
        let _guard = ExecutionGuard(Arc::clone(&self.dropped));
        self.started.notify_one();
        pending::<()>().await;
        Ok(Value::Null)
    }
}

fn fixture_schema() -> Value {
    json!({
        "type": "object",
        "properties": {"issue": {"type": "integer"}}
    })
}

#[derive(Clone, Default)]
struct CapturedOutput {
    bytes: Arc<Mutex<Vec<u8>>>,
}

impl CapturedOutput {
    fn text(&self) -> String {
        String::from_utf8(self.bytes.lock().expect("captured tracing lock").clone())
            .expect("captured tracing UTF-8")
    }
}

struct CapturedWriter {
    bytes: Arc<Mutex<Vec<u8>>>,
}

impl Write for CapturedWriter {
    fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
        self.bytes
            .lock()
            .map_err(|_| io::Error::other("captured tracing lock failed"))?
            .extend_from_slice(buffer);
        Ok(buffer.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

impl<'a> MakeWriter<'a> for CapturedOutput {
    type Writer = CapturedWriter;

    fn make_writer(&'a self) -> Self::Writer {
        CapturedWriter {
            bytes: Arc::clone(&self.bytes),
        }
    }
}
