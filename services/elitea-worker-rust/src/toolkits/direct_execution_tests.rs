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

fn invocation(toolset: &str, tool: &str, arguments: Value) -> DirectToolkitInvocation {
    DirectToolkitInvocation::new(
        toolset.to_owned(),
        tool.to_owned(),
        "execution-1".to_owned(),
        "call-1".to_owned(),
        arguments,
    )
    .expect("direct toolkit invocation")
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
    )
    .await
    .expect_err("authorization challenge");

    assert_eq!(
        error.code(),
        DirectToolkitExecutionErrorCode::AuthorizationRequired
    );
    assert!(!error.retryable());
    assert!(!format!("{error:?} {error}").contains("sharepoint.example"));
}

#[tokio::test]
async fn refuses_unprojected_scopes_before_dispatch_and_bounds_results() {
    let scoped = Arc::new(FixtureTool::scoped_read("get_issue"));
    let scope_error = execute_read_only_toolsets(
        toolset("repo", vec![scoped.clone()]),
        invocation("repo", "get_issue", json!({})),
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
            return Ok(json!({"body": "x".repeat(1024 * 1_024)}));
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
