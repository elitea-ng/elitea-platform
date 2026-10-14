//! Smart Tools Selection on its own: what is moved behind the meta-tools and
//! what is not, the meta-tools' answers, and the generated index. The worker's
//! suites run the same partition through a real agent graph.

use std::sync::Mutex;

use adk_core::{ErrorComponent, Toolset};
use adk_tool::{BasicToolset, SimpleToolContext};

use super::*;

/// A tool that records the arguments it ran with.
struct FakeTool {
    name: String,
    schema: Value,
    outcome: fn(&Value) -> adk_core::Result<Value>,
    seen: Arc<Mutex<Vec<Value>>>,
}

#[async_trait]
impl Tool for FakeTool {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &'static str {
        "fake"
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(self.schema.clone())
    }

    async fn execute(&self, _ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        self.seen.lock().expect("seen").push(args.clone());
        (self.outcome)(&args)
    }
}

fn fake(name: &str, seen: &Arc<Mutex<Vec<Value>>>) -> Arc<dyn Tool> {
    Arc::new(FakeTool {
        name: name.to_owned(),
        schema: json!({
            "type": "object",
            "properties": {
                "path": {"type": "string", "description": "File path"},
                "limit": {"type": "integer"},
            },
            "required": ["path"],
        }),
        outcome: |args| Ok(json!({"echo": args})),
        seen: Arc::clone(seen),
    })
}

fn with_outcome(
    name: &str,
    seen: &Arc<Mutex<Vec<Value>>>,
    outcome: fn(&Value) -> adk_core::Result<Value>,
) -> Arc<dyn Tool> {
    Arc::new(FakeTool {
        name: name.to_owned(),
        schema: json!({"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}),
        outcome,
        seen: Arc::clone(seen),
    })
}

async fn frozen(name: &str, tools: Vec<Arc<dyn Tool>>) -> FrozenToolset {
    let toolset: Arc<dyn Toolset> = Arc::new(BasicToolset::new(name, tools));
    crate::toolkits::tool_binding::freeze_toolsets(vec![toolset], "lazy-test")
        .await
        .expect("freeze")
        .remove(0)
}

fn numbered(prefix: &str, count: usize, seen: &Arc<Mutex<Vec<Value>>>) -> Vec<Arc<dyn Tool>> {
    (0..count)
        .map(|n| fake(&format!("{prefix}_{n:02}"), seen))
        .collect()
}

fn never_direct(_: &str, _: &str) -> bool {
    false
}

fn kind(name: &str) -> Option<String> {
    (name != "unkinded").then(|| name.split('-').next().unwrap_or(name).to_owned())
}

fn ctx() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("lazy-test"))
}

fn meta_tool(partition: &Partition, name: &str) -> Arc<dyn Tool> {
    let meta = partition
        .toolsets
        .iter()
        .find(|toolset| toolset.name() == TOOLSET_NAME)
        .expect("meta toolset");
    meta.tools()
        .iter()
        .find(|tool| tool.name() == name)
        .cloned()
        .expect("meta tool")
}

async fn registry_of(seen: &Arc<Mutex<Vec<Value>>>) -> Partition {
    let github = frozen("github-main", numbered("gh", 12, seen)).await;
    let jira = frozen("jira-main", numbered("jr", 10, seen)).await;
    partition(vec![github, jira], |_| true, never_direct, kind)
}

#[tokio::test]
async fn below_the_threshold_nothing_moves_and_the_prompt_is_untouched() {
    let seen = Arc::default();
    let small = frozen("github-main", numbered("gh", MIN_TOOLS - 1, &seen)).await;
    let result = partition(vec![small], |_| true, never_direct, kind);
    assert!(result.index.is_none());
    assert_eq!(result.toolsets.len(), 1);
    assert_eq!(result.toolsets[0].name(), "github-main");
    assert_eq!(result.toolsets[0].tools().len(), MIN_TOOLS - 1);
}

#[tokio::test]
async fn above_the_threshold_candidate_tools_hide_behind_two_meta_tools() {
    let seen = Arc::default();
    let result = registry_of(&seen).await;
    assert!(result.index.is_some());
    let names: Vec<&str> = result.toolsets.iter().map(FrozenToolset::name).collect();
    assert_eq!(names, [TOOLSET_NAME], "no candidate tool stays bound");
    let meta: Vec<&str> = result.toolsets[0]
        .tools()
        .iter()
        .map(|tool| tool.name())
        .collect();
    assert_eq!(meta, [GET_TOOL_SCHEMA, INVOKE_TOOL]);
}

#[tokio::test]
async fn guarded_tools_other_toolsets_and_internal_tools_stay_bound_directly() {
    let seen = Arc::default();
    let mut sensitive = numbered("gh", 22, &seen);
    sensitive.push(fake("gh_delete_repo", &seen));
    let github = frozen("github-main", sensitive).await;
    let nested = frozen("elitea_nested_applications", numbered("app", 3, &seen)).await;
    let result = partition(
        vec![github, nested],
        |toolset| toolset.starts_with("github"),
        |toolset, tool| toolset == "github-main" && tool == "gh_delete_repo",
        kind,
    );
    assert!(result.index.is_some());
    let by_name = |name: &str| {
        result
            .toolsets
            .iter()
            .find(|toolset| toolset.name() == name)
            .map(|toolset| toolset.tools().len())
    };
    assert_eq!(by_name("github-main"), Some(1), "only the guarded tool");
    assert_eq!(by_name("elitea_nested_applications"), Some(3));
    assert_eq!(by_name(TOOLSET_NAME), Some(2));
    let index = result.index.expect("index");
    assert!(index.contains("Total: 1 toolkits, 22 tools"));
    assert!(
        !index.contains("gh_delete_repo"),
        "a guarded tool is not advertised"
    );
}

#[tokio::test]
async fn a_direct_tool_named_like_a_meta_tool_switches_the_mode_off() {
    let seen = Arc::default();
    let github = frozen("github-main", numbered("gh", 25, &seen)).await;
    let clash = frozen("other", vec![fake(INVOKE_TOOL, &seen)]).await;
    let result = partition(
        vec![github, clash],
        |toolset| toolset == "github-main",
        never_direct,
        kind,
    );
    assert!(result.index.is_none());
    assert_eq!(result.toolsets.len(), 2);
}

#[tokio::test]
async fn a_toolset_that_repeats_a_tool_name_is_left_to_the_binder() {
    let seen = Arc::default();
    let mut tools = numbered("gh", 25, &seen);
    tools.push(fake("gh_00", &seen));
    let repeated = frozen("github-main", tools).await;
    let result = partition(vec![repeated], |_| true, never_direct, kind);
    assert!(result.index.is_none());
    assert_eq!(result.toolsets[0].tools().len(), 26);
}

#[tokio::test]
async fn the_index_is_the_sdks_text() {
    let seen = Arc::default();
    let github_a = frozen("github-a", numbered("gh", 8, &seen)).await;
    let github_b = frozen("github-b", numbered("gh", 8, &seen)).await;
    let jira = frozen("jira-main", numbered("jr", 6, &seen)).await;
    let result = partition(vec![github_a, github_b, jira], |_| true, never_direct, kind);
    let index = result.index.expect("index");
    let expected = [
        "# Available Tools",
        "",
        "You have 2 meta-tools to access all functionality:",
        "- get_tool_schema(toolkit, tool) - Get parameter schema for a tool BEFORE calling it",
        "- invoke_tool(toolkit, tool, arguments) - Execute a tool",
        "",
        "IMPORTANT: Each toolkit name (e.g., 'sdk', 'docs', 'core') targets a DIFFERENT repository.",
        "Use the EXACT toolkit name to work with that specific repo. The toolkit name IS the repo selector.",
        "",
        "SCHEMA EFFICIENCY: Toolkits of the same type share identical schemas.",
        "Get schema once per tool (e.g., get_tool_schema('sdk', 'search_code')), then reuse that knowledge",
        "for other toolkits of same type - just change the toolkit name in invoke_tool.",
        "",
        "Example: invoke_tool(toolkit=\"github-a\", tool=\"gh_00\", arguments={...})",
        "",
        "## Type: github",
        "  Toolkit names (use one of these exactly):",
        "    - \"github-a\": github-a toolkit",
        "    - \"github-b\": github-b toolkit",
        "  Tools (8): gh_00, gh_01, gh_02, gh_03, gh_04, gh_05, gh_06, gh_07",
        "",
        "## Type: jira",
        "  Toolkit name: \"jira-main\"",
        "  Description: jira-main toolkit",
        "  Tools (6): jr_00, jr_01, jr_02, jr_03, jr_04, jr_05",
        "",
        "Total: 3 toolkits, 22 tools",
    ]
    .join("\n");
    assert_eq!(index, expected);
}

#[tokio::test]
async fn get_tool_schema_answers_exact_misplaced_and_unknown_lookups() {
    let seen = Arc::default();
    let result = registry_of(&seen).await;
    let tool = meta_tool(&result, GET_TOOL_SCHEMA);
    let run = |args: Value| {
        let tool = Arc::clone(&tool);
        async move { tool.execute(ctx(), args).await.expect("schema answer") }
    };
    let exact = run(json!({"toolkit": "github-main", "tool": "gh_03"})).await;
    assert_eq!(exact["name"], "gh_03");
    assert_eq!(exact["parameters"]["required"], json!(["path"]));
    assert!(exact.get("note").is_none());

    let misplaced = run(json!({"toolkit": "jira-main", "tool": "gh_03"})).await;
    assert_eq!(misplaced["name"], "gh_03");
    assert_eq!(
        misplaced["note"],
        "Tool found in toolkit 'github-main' (not 'jira-main')"
    );

    let missing_tool = run(json!({"toolkit": "jira-main", "tool": "nope"})).await;
    assert_eq!(
        missing_tool["error"],
        "Tool 'nope' not found in toolkit 'jira-main'"
    );
    assert_eq!(
        missing_tool["available_tools"]
            .as_array()
            .expect("list")
            .len(),
        10
    );

    let missing_toolkit = run(json!({"toolkit": "wiki", "tool": "nope"})).await;
    assert_eq!(missing_toolkit["error"], "Toolkit 'wiki' not found");
    assert_eq!(
        missing_toolkit["available_toolkits"],
        json!(["github-main", "jira-main"])
    );
}

#[tokio::test]
async fn invoke_tool_runs_the_tool_drops_null_arguments_and_serialises_the_result() {
    let seen: Arc<Mutex<Vec<Value>>> = Arc::default();
    let result = registry_of(&seen).await;
    let invoke = meta_tool(&result, INVOKE_TOOL);
    let out = invoke
        .execute(
            ctx(),
            json!({"toolkit": "github-main", "tool": "gh_01",
                   "arguments": {"path": "a.rs", "limit": null}}),
        )
        .await
        .expect("invocation");
    assert_eq!(out, json!({"echo": {"path": "a.rs"}}));
    assert_eq!(
        seen.lock().expect("seen").as_slice(),
        [json!({"path": "a.rs"})],
        "an explicit null never reaches the tool"
    );

    // A wrong toolkit still finds the tool by name.
    let by_name = invoke
        .execute(
            ctx(),
            json!({"toolkit": "jira-main", "tool": "gh_01", "arguments": {"path": "b"}}),
        )
        .await
        .expect("invocation");
    assert_eq!(by_name, json!({"echo": {"path": "b"}}));

    let unknown_tool = invoke
        .execute(ctx(), json!({"toolkit": "jira-main", "tool": "nope"}))
        .await
        .expect("answer");
    let text = unknown_tool.as_str().expect("text");
    assert!(text.starts_with(
        "Tool 'nope' not found in toolkit 'jira-main'. Available tools in jira-main: jr_00,"
    ));
    let unknown_toolkit = invoke
        .execute(ctx(), json!({"toolkit": "wiki", "tool": "nope"}))
        .await
        .expect("answer");
    assert_eq!(
        unknown_toolkit,
        Value::String(
            "Toolkit 'wiki' not found. Available toolkits: github-main, jira-main".to_owned()
        )
    );
}

#[tokio::test]
async fn invoke_tool_passes_results_through_and_names_an_empty_one() {
    let seen: Arc<Mutex<Vec<Value>>> = Arc::default();
    let mut tools = numbered("gh", 20, &seen);
    tools.push(with_outcome("null_tool", &seen, |_| Ok(Value::Null)));
    tools.push(with_outcome("text_tool", &seen, |_| Ok(json!("plain"))));
    tools.push(with_outcome("true_tool", &seen, |_| Ok(json!(true))));
    tools.push(with_outcome("number_tool", &seen, |_| Ok(json!(7))));
    let result = partition(
        vec![frozen("github-main", tools).await],
        |_| true,
        never_direct,
        kind,
    );
    let invoke = meta_tool(&result, INVOKE_TOOL);
    let run = |name: &'static str| {
        let invoke = Arc::clone(&invoke);
        async move {
            invoke
                .execute(ctx(), json!({"toolkit": "github-main", "tool": name}))
                .await
                .expect("invocation")
        }
    };
    assert_eq!(
        run("null_tool").await,
        Value::String("Tool executed successfully (no output)".to_owned())
    );
    assert_eq!(run("text_tool").await, Value::String("plain".to_owned()));
    // Values other than the SDK's "no output" sentence pass through as they
    // are; the facade encodes them once, as it does for a bound tool.
    assert_eq!(run("true_tool").await, json!(true));
    assert_eq!(run("number_tool").await, json!(7));
}

#[tokio::test]
async fn a_failing_tool_answers_with_the_expected_parameters_and_an_example() {
    let seen: Arc<Mutex<Vec<Value>>> = Arc::default();
    let mut tools = numbered("gh", 22, &seen);
    tools.push(fake("strict", &seen));
    tools.push(with_outcome("boom", &seen, |_| {
        Err(AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::InvalidInput,
            "test.boom",
            "path is required",
        ))
    }));
    tools.push(with_outcome("cancelled", &seen, |_| {
        Err(AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::Cancelled,
            "test.cancelled",
            "stop",
        ))
    }));
    let result = partition(
        vec![frozen("github-main", tools).await],
        |_| true,
        never_direct,
        kind,
    );
    let invoke = meta_tool(&result, INVOKE_TOOL);
    let failed = invoke
        .execute(
            ctx(),
            json!({"toolkit": "github-main", "tool": "boom", "arguments": {"x": 1}}),
        )
        .await
        .expect("a tool failure is an answer");
    assert_eq!(
        failed.as_str().expect("text"),
        "ERROR: Invalid arguments for github-main.boom\n\nYou provided: {\"x\": 1}\n\nREQUIRED parameters:\n  - path (string)\n\nCORRECT usage:\ninvoke_tool(toolkit=\"github-main\", tool=\"boom\", arguments={\"path\": \"<path>\"})"
    );
    // A cancellation is not an answer: it ends the turn like a direct call.
    let cancelled = invoke
        .execute(
            ctx(),
            json!({"toolkit": "github-main", "tool": "cancelled"}),
        )
        .await;
    assert_eq!(
        cancelled.expect_err("cancellation propagates").category,
        ErrorCategory::Cancelled
    );
}

#[test]
fn python_style_dumps_matches_the_sdks_separators() {
    assert_eq!(
        py_dumps(&json!({"a": [1, 2], "b": {"c": "d"}})),
        r#"{"a": [1, 2], "b": {"c": "d"}}"#
    );
}
