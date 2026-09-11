use std::collections::BTreeMap;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use adk_rust::tool::BasicToolset;
use adk_rust::{Tool, ToolContext, Toolset};
use async_trait::async_trait;
use serde_json::{Value, json};

use super::delegated_authorization_error_fixture;
use super::direct_execution::{
    DirectToolkitExecutionErrorCode, DirectToolkitInvocation, execute_read_only_toolsets,
};
use super::policy::ToolAdmissionPolicy;

fn invocation(toolset: &str, tool: &str, arguments: Value) -> DirectToolkitInvocation {
    DirectToolkitInvocation::new(
        "github".to_owned(),
        toolset.to_owned(),
        tool.to_owned(),
        "execution-1".to_owned(),
        "call-1".to_owned(),
        arguments,
    )
    .expect("direct toolkit invocation")
}

fn allow_all() -> ToolAdmissionPolicy {
    ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("empty admission policy")
}

fn toolset(name: &str, tools: Vec<Arc<dyn Tool>>) -> Vec<Arc<dyn Toolset>> {
    vec![Arc::new(BasicToolset::new(name, tools))]
}

#[tokio::test]
async fn invokes_one_exact_read_only_tool_without_a_model_turn() {
    let tool = Arc::new(FixtureTool::read_only("get_issue"));
    let result = execute_read_only_toolsets(
        toolset("repo", vec![tool.clone()]),
        invocation("repo", "get_issue", json!({"issue": 7})),
        &allow_all(),
    )
    .await
    .expect("read-only direct execution");

    assert_eq!(result, json!({"call_id": "call-1", "issue": 7}));
    assert_eq!(tool.calls.load(Ordering::SeqCst), 1);
    assert_eq!(
        tool.arguments.lock().expect("arguments").as_slice(),
        &[json!({"issue": 7})]
    );
}

#[tokio::test]
async fn refuses_effectful_tools_before_the_provider_is_called() {
    let tool = Arc::new(FixtureTool::effectful("create_issue"));
    let error = execute_read_only_toolsets(
        toolset("repo", vec![tool.clone()]),
        invocation("repo", "create_issue", json!({"title": "safe"})),
        &allow_all(),
    )
    .await
    .expect_err("effectful direct execution must stay closed");

    assert_eq!(
        error.code(),
        DirectToolkitExecutionErrorCode::EffectfulToolUnavailable
    );
    assert!(!error.retryable());
    assert_eq!(tool.calls.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn refuses_missing_and_ambiguous_original_tool_names() {
    let first: Arc<dyn Tool> = Arc::new(FixtureTool::read_only("get_issue"));
    let missing = execute_read_only_toolsets(
        toolset("repo", vec![Arc::clone(&first)]),
        invocation("repo", "list_issues", json!({})),
        &allow_all(),
    )
    .await
    .expect_err("unselected tool must fail");
    assert_eq!(
        missing.code(),
        DirectToolkitExecutionErrorCode::ToolNotSelected
    );

    let second: Arc<dyn Tool> = Arc::new(FixtureTool::read_only("get_issue"));
    let ambiguous = execute_read_only_toolsets(
        toolset("repo", vec![first, second]),
        invocation("repo", "get_issue", json!({})),
        &allow_all(),
    )
    .await
    .expect_err("ambiguous original name must fail");
    assert_eq!(
        ambiguous.code(),
        DirectToolkitExecutionErrorCode::InvalidConfiguration
    );
}

#[tokio::test]
async fn preserves_delegated_authorization_as_a_typed_outcome() {
    let tool: Arc<dyn Tool> = Arc::new(AuthorizationTool);
    let error = execute_read_only_toolsets(
        toolset("documents", vec![tool]),
        invocation("documents", "read_document", json!({"path": "/shared"})),
        &allow_all(),
    )
    .await
    .expect_err("authorization challenge");

    assert_eq!(
        error.code(),
        DirectToolkitExecutionErrorCode::AuthorizationRequired
    );
    assert!(!error.retryable());
    assert!(error.authorization().is_some());
    assert!(!format!("{error:?} {error}").contains("sharepoint.example"));
}

#[tokio::test]
async fn refuses_unprojected_scopes_before_dispatch_and_bounds_results() {
    let scoped = Arc::new(FixtureTool::scoped_read("get_issue"));
    let scope_error = execute_read_only_toolsets(
        toolset("repo", vec![scoped.clone()]),
        invocation("repo", "get_issue", json!({})),
        &allow_all(),
    )
    .await
    .expect_err("out-of-loop scopes must be projected explicitly");
    assert_eq!(
        scope_error.code(),
        DirectToolkitExecutionErrorCode::AuthorizationRequired
    );
    assert_eq!(scoped.calls.load(Ordering::SeqCst), 0);

    let oversized = Arc::new(FixtureTool::oversized_read("get_issue"));
    let result_error = execute_read_only_toolsets(
        toolset("repo", vec![oversized.clone()]),
        invocation("repo", "get_issue", json!({})),
        &allow_all(),
    )
    .await
    .expect_err("over-limit provider result must fail");
    assert_eq!(
        result_error.code(),
        DirectToolkitExecutionErrorCode::ResourceExhausted
    );
    assert_eq!(oversized.calls.load(Ordering::SeqCst), 1);
}

#[test]
fn validates_argument_shape_size_and_depth_before_materialization() {
    let invalid = DirectToolkitInvocation::new(
        "github".to_owned(),
        "repo".to_owned(),
        "get_issue".to_owned(),
        "execution-1".to_owned(),
        "call-1".to_owned(),
        json!([1, 2, 3]),
    )
    .err()
    .expect("arguments must be an object");
    assert_eq!(
        invalid.code(),
        DirectToolkitExecutionErrorCode::InvalidInput
    );

    let oversized = DirectToolkitInvocation::new(
        "github".to_owned(),
        "repo".to_owned(),
        "get_issue".to_owned(),
        "execution-1".to_owned(),
        "call-1".to_owned(),
        json!({"value": "x".repeat(256 * 1_024)}),
    )
    .err()
    .expect("oversized arguments must fail");
    assert_eq!(
        oversized.code(),
        DirectToolkitExecutionErrorCode::ResourceExhausted
    );

    let mut too_deep = json!({});
    for _ in 0..65 {
        too_deep = json!({"nested": too_deep});
    }
    let deep = DirectToolkitInvocation::new(
        "github".to_owned(),
        "repo".to_owned(),
        "get_issue".to_owned(),
        "execution-1".to_owned(),
        "call-1".to_owned(),
        too_deep,
    )
    .err()
    .expect("over-depth arguments must fail");
    assert_eq!(
        deep.code(),
        DirectToolkitExecutionErrorCode::ResourceExhausted
    );
}

#[tokio::test]
async fn refuses_blocked_and_sensitive_tools_before_provider_dispatch() {
    let tool = Arc::new(FixtureTool::read_only("get_issue"));
    let mut blocked = BTreeMap::new();
    blocked.insert("github".to_owned(), vec!["get_issue".to_owned()]);
    let blocked_policy = ToolAdmissionPolicy::new(&[], &blocked).expect("blocked policy");
    let blocked_error = execute_read_only_toolsets(
        toolset("repo", vec![tool.clone()]),
        invocation("repo", "get_issue", json!({})),
        &blocked_policy,
    )
    .await
    .expect_err("blocked direct tool must fail");
    assert_eq!(
        blocked_error.code(),
        DirectToolkitExecutionErrorCode::ToolBlocked
    );

    let runtime = json!({
        "toolkit_security": {
            "sensitive_tools": {"repo": ["get_issue"]}
        }
    });
    let sensitive_policy =
        ToolAdmissionPolicy::from_runtime_config(runtime.as_object().expect("runtime object"))
            .expect("sensitive policy");
    let sensitive_error = execute_read_only_toolsets(
        toolset("repo", vec![tool.clone()]),
        invocation("repo", "get_issue", json!({})),
        &sensitive_policy,
    )
    .await
    .expect_err("sensitive direct tool must fail");
    assert_eq!(
        sensitive_error.code(),
        DirectToolkitExecutionErrorCode::SensitiveToolUnavailable
    );
    assert_eq!(tool.calls.load(Ordering::SeqCst), 0);
}

struct FixtureTool {
    name: String,
    read_only: bool,
    required_scopes: &'static [&'static str],
    oversized_result: bool,
    calls: AtomicUsize,
    arguments: Mutex<Vec<Value>>,
}

impl FixtureTool {
    fn read_only(name: &str) -> Self {
        Self {
            name: name.to_owned(),
            read_only: true,
            required_scopes: &[],
            oversized_result: false,
            calls: AtomicUsize::new(0),
            arguments: Mutex::new(Vec::new()),
        }
    }

    fn effectful(name: &str) -> Self {
        Self {
            read_only: false,
            ..Self::read_only(name)
        }
    }

    fn scoped_read(name: &str) -> Self {
        Self {
            required_scopes: &["issues:read"],
            ..Self::read_only(name)
        }
    }

    fn oversized_read(name: &str) -> Self {
        Self {
            oversized_result: true,
            ..Self::read_only(name)
        }
    }
}

#[async_trait]
impl Tool for FixtureTool {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &'static str {
        "direct execution fixture"
    }

    fn is_read_only(&self) -> bool {
        self.read_only
    }

    fn required_scopes(&self) -> &[&str] {
        self.required_scopes
    }

    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        self.arguments
            .lock()
            .expect("arguments")
            .push(arguments.clone());
        if self.oversized_result {
            return Ok(json!({"body": "x".repeat(48 * 1_024)}));
        }
        Ok(json!({
            "call_id": context.function_call_id(),
            "issue": arguments.get("issue").cloned().unwrap_or(Value::Null),
        }))
    }
}

struct AuthorizationTool;

#[async_trait]
impl Tool for AuthorizationTool {
    fn name(&self) -> &'static str {
        "read_document"
    }

    fn description(&self) -> &'static str {
        "authorization fixture"
    }

    fn is_read_only(&self) -> bool {
        true
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        Err(delegated_authorization_error_fixture("sharepoint"))
    }
}

#[tokio::test]
async fn shared_test_preserves_values_and_tool_failures_without_opening_effects() {
    use super::direct_execution::execute_test_toolsets;
    for value in [
        Value::Null,
        json!(""),
        json!(false),
        json!(0),
        json!([]),
        json!({"nested": null}),
    ] {
        let tool: Arc<dyn Tool> = Arc::new(OutcomeTool(Ok(value.clone())));
        let actual = execute_test_toolsets(
            toolset("repo", vec![tool]),
            invocation("repo", "result", json!({})),
            &allow_all(),
        )
        .await
        .expect("test value");
        assert_eq!(actual, value);
    }
    let failed = execute_test_toolsets(
        toolset("repo", vec![Arc::new(OutcomeTool(Err(())))]),
        invocation("repo", "result", json!({})),
        &allow_all(),
    )
    .await
    .expect_err("tool failure");
    assert_eq!(failed.code(), DirectToolkitExecutionErrorCode::ToolError);
    assert!(!format!("{failed:?} {failed}").contains("protected-provider-detail"));

    let effect = Arc::new(FixtureTool::effectful("write"));
    let refused = execute_test_toolsets(
        toolset("repo", vec![effect.clone()]),
        invocation("repo", "write", json!({})),
        &allow_all(),
    )
    .await
    .expect_err("effect refusal");
    assert_eq!(
        refused.code(),
        DirectToolkitExecutionErrorCode::EffectfulToolUnavailable
    );
    assert_eq!(effect.calls.load(Ordering::SeqCst), 0);
}

struct OutcomeTool(Result<Value, ()>);

#[async_trait]
impl Tool for OutcomeTool {
    fn name(&self) -> &'static str {
        "result"
    }
    fn description(&self) -> &'static str {
        "test result"
    }
    fn is_read_only(&self) -> bool {
        true
    }
    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        self.0.clone().map_err(|()| {
            adk_rust::AdkError::unavailable(
                adk_rust::error::ErrorComponent::Tool,
                "test.failure",
                "protected-provider-detail",
            )
        })
    }
}
