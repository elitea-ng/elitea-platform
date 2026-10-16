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

async fn two_github_toolkits() -> (Partition, Arc<Mutex<Vec<Value>>>, Arc<Mutex<Vec<Value>>>) {
    let seen_a = Arc::default();
    let seen_b = Arc::default();
    let a = frozen("github-a", numbered("gh", 12, &seen_a)).await;
    let b = frozen("github-b", numbered("gh", 12, &seen_b)).await;
    (
        partition(vec![a, b], |_| true, never_direct, kind),
        seen_a,
        seen_b,
    )
}

async fn schema_of(partition: &Partition, args: Value) -> Value {
    meta_tool(partition, GET_TOOL_SCHEMA)
        .execute(ctx(), args)
        .await
        .expect("schema answer")
}

async fn invoke(partition: &Partition, args: Value) -> adk_core::Result<Value> {
    meta_tool(partition, INVOKE_TOOL).execute(ctx(), args).await
}

#[tokio::test]
async fn get_tool_schema_answers_exact_and_refused_lookups_and_names_the_toolkit() {
    let seen = Arc::default();
    let result = registry_of(&seen).await;
    let exact = schema_of(&result, json!({"toolkit": "github-main", "tool": "gh_03"})).await;
    assert_eq!(exact["name"], "gh_03");
    assert_eq!(exact["toolkit"], "github-main");
    assert_eq!(exact["parameters"]["required"], json!(["path"]));
    assert!(exact.get("note").is_none());

    // A tool of ANOTHER toolkit is not borrowed when a toolkit is named.
    let misplaced = schema_of(&result, json!({"toolkit": "jira-main", "tool": "gh_03"})).await;
    assert_eq!(
        misplaced["error"],
        "Tool 'gh_03' not found in toolkit 'jira-main'"
    );
    assert_eq!(
        misplaced["available_tools"].as_array().expect("list").len(),
        10
    );
    assert!(misplaced.get("name").is_none());

    let missing_toolkit = schema_of(&result, json!({"toolkit": "wiki", "tool": "gh_03"})).await;
    assert_eq!(missing_toolkit["error"], "Toolkit 'wiki' not found");
    assert_eq!(
        missing_toolkit["available_toolkits"],
        json!(["github-main", "jira-main"])
    );

    // No toolkit given: an unambiguous tool name resolves, and says where.
    let unnamed = schema_of(&result, json!({"tool": "jr_02"})).await;
    assert_eq!(unnamed["toolkit"], "jira-main");

    // Letter case alone is normalised, with a note.
    let cased = schema_of(&result, json!({"toolkit": "GitHub-Main", "tool": "gh_03"})).await;
    assert_eq!(cased["toolkit"], "github-main");
    assert_eq!(
        cased["note"],
        "Toolkit 'GitHub-Main' resolved to 'github-main'"
    );
}

#[tokio::test]
async fn a_typo_in_the_toolkit_runs_nothing_even_when_another_toolkit_has_the_tool() {
    let (result, seen_a, seen_b) = two_github_toolkits().await;
    let answer = invoke(
        &result,
        json!({"toolkit": "github-c", "tool": "gh_01", "arguments": {"path": "x"}}),
    )
    .await
    .expect("a refusal is an answer");
    assert_eq!(answer["error"], "Toolkit 'github-c' not found");
    assert_eq!(
        answer["available_toolkits"],
        json!(["github-a", "github-b"])
    );
    assert!(seen_a.lock().expect("a").is_empty() && seen_b.lock().expect("b").is_empty());
    let schema = schema_of(&result, json!({"toolkit": "github-c", "tool": "gh_01"})).await;
    assert_eq!(schema["error"], "Toolkit 'github-c' not found");
}

#[tokio::test]
async fn an_ambiguous_tool_name_without_a_toolkit_is_refused_and_lists_the_candidates() {
    let (result, seen_a, seen_b) = two_github_toolkits().await;
    let answer = invoke(
        &result,
        json!({"tool": "gh_01", "arguments": {"path": "x"}}),
    )
    .await
    .expect("a refusal is an answer");
    assert!(
        answer["error"]
            .as_str()
            .expect("error")
            .contains("more than one toolkit")
    );
    assert_eq!(
        answer["toolkits_with_this_tool"],
        json!(["github-a", "github-b"])
    );
    assert!(seen_a.lock().expect("a").is_empty() && seen_b.lock().expect("b").is_empty());
    // The same name with a toolkit runs exactly that toolkit's tool.
    let ran = invoke(
        &result,
        json!({"toolkit": "github-b", "tool": "gh_01", "arguments": {"path": "x"}}),
    )
    .await
    .expect("invocation");
    assert_eq!(ran["toolkit"], "github-b");
    assert!(seen_a.lock().expect("a").is_empty());
    assert_eq!(seen_b.lock().expect("b").len(), 1);
}

#[tokio::test]
async fn invoke_tool_runs_the_tool_drops_null_arguments_and_states_which_toolkit_ran() {
    let seen: Arc<Mutex<Vec<Value>>> = Arc::default();
    let result = registry_of(&seen).await;
    let out = invoke(
        &result,
        json!({"toolkit": "github-main", "tool": "gh_01",
               "arguments": {"path": "a.rs", "limit": null}}),
    )
    .await
    .expect("invocation");
    assert_eq!(
        out,
        json!({"toolkit": "github-main", "tool": "gh_01",
               "result": {"echo": {"path": "a.rs"}}})
    );
    assert_eq!(
        seen.lock().expect("seen").as_slice(),
        [json!({"path": "a.rs"})],
        "an explicit null never reaches the tool"
    );

    let unnamed = invoke(
        &result,
        json!({"tool": "jr_01", "arguments": {"path": "b"}}),
    )
    .await
    .expect("invocation");
    assert_eq!(unnamed["toolkit"], "jira-main");

    let unknown_tool = invoke(&result, json!({"toolkit": "jira-main", "tool": "nope"}))
        .await
        .expect("answer");
    assert_eq!(
        unknown_tool["error"],
        "Tool 'nope' not found in toolkit 'jira-main'"
    );
    let unknown_name = invoke(&result, json!({"tool": "nope"}))
        .await
        .expect("answer");
    assert_eq!(unknown_name["error"], "Tool 'nope' not found");
}

#[tokio::test]
async fn invoke_tool_names_an_empty_result_and_passes_every_value_through() {
    let seen: Arc<Mutex<Vec<Value>>> = Arc::default();
    let mut tools = numbered("gh", 20, &seen);
    tools.push(with_outcome("null_tool", &seen, |_| Ok(Value::Null)));
    tools.push(with_outcome("text_tool", &seen, |_| Ok(json!("plain"))));
    tools.push(with_outcome("number_tool", &seen, |_| Ok(json!(7))));
    let result = partition(
        vec![frozen("github-main", tools).await],
        |_| true,
        never_direct,
        kind,
    );
    let run = |name: &'static str| {
        let result = &result;
        async move {
            invoke(result, json!({"toolkit": "github-main", "tool": name}))
                .await
                .expect("invocation")["result"]
                .clone()
        }
    };
    assert_eq!(
        run("null_tool").await,
        Value::String("Tool executed successfully (no output)".to_owned())
    );
    assert_eq!(run("text_tool").await, json!("plain"));
    assert_eq!(run("number_tool").await, json!(7));
}

fn failing(category: ErrorCategory) -> fn(&Value) -> adk_core::Result<Value> {
    match category {
        ErrorCategory::InvalidInput => |_| {
            Err(AdkError::new(
                ErrorComponent::Tool,
                ErrorCategory::InvalidInput,
                "test.invalid",
                "path is required",
            ))
        },
        ErrorCategory::Unauthorized => |_| {
            Err(AdkError::new(
                ErrorComponent::Tool,
                ErrorCategory::Unauthorized,
                "test.unauthorized",
                "sign in first",
            ))
        },
        ErrorCategory::Forbidden => |_| {
            Err(AdkError::new(
                ErrorComponent::Tool,
                ErrorCategory::Forbidden,
                "test.forbidden",
                "admission refused this tool",
            ))
        },
        ErrorCategory::Unavailable => |_| {
            Err(AdkError::new(
                ErrorComponent::Tool,
                ErrorCategory::Unavailable,
                "test.unavailable",
                "backend down",
            ))
        },
        ErrorCategory::Timeout => |_| {
            Err(AdkError::new(
                ErrorComponent::Tool,
                ErrorCategory::Timeout,
                "test.timeout",
                "too slow",
            ))
        },
        ErrorCategory::Cancelled => |_| {
            Err(AdkError::new(
                ErrorComponent::Tool,
                ErrorCategory::Cancelled,
                "test.cancelled",
                "stop",
            ))
        },
        _ => |_| {
            Err(AdkError::new(
                ErrorComponent::Tool,
                ErrorCategory::Internal,
                "test.internal",
                "the tool itself failed",
            ))
        },
    }
}

async fn with_failing_tool(category: ErrorCategory) -> Partition {
    let seen: Arc<Mutex<Vec<Value>>> = Arc::default();
    let mut tools = numbered("gh", 22, &seen);
    tools.push(with_outcome("boom", &seen, failing(category)));
    partition(
        vec![frozen("github-main", tools).await],
        |_| true,
        never_direct,
        kind,
    )
}

#[tokio::test]
async fn only_a_rejected_argument_set_becomes_the_invalid_arguments_answer() {
    let result = with_failing_tool(ErrorCategory::InvalidInput).await;
    let failed = invoke(
        &result,
        json!({"toolkit": "github-main", "tool": "boom", "arguments": {"x": 1}}),
    )
    .await
    .expect("an invalid argument set is an answer");
    assert_eq!(
        failed.as_str().expect("text"),
        "ERROR: Invalid arguments for github-main.boom\n\nYou provided: {\"x\": 1}\n\nREQUIRED parameters:\n  - path (string)\n\nCORRECT usage:\ninvoke_tool(toolkit=\"github-main\", tool=\"boom\", arguments={\"path\": \"<path>\"})"
    );
}

/// Every other category reaches the loop with its own category and text, as
/// it would from a bound tool: it is not dressed up as an argument mistake.
#[tokio::test]
async fn every_other_failure_passes_through_with_its_own_category_and_text() {
    for (category, text) in [
        (ErrorCategory::Unauthorized, "sign in first"),
        (ErrorCategory::Forbidden, "admission refused this tool"),
        (ErrorCategory::Unavailable, "backend down"),
        (ErrorCategory::Timeout, "too slow"),
        (ErrorCategory::Cancelled, "stop"),
        (ErrorCategory::Internal, "the tool itself failed"),
    ] {
        let result = with_failing_tool(category).await;
        let error = invoke(&result, json!({"toolkit": "github-main", "tool": "boom"}))
            .await
            .expect_err("the failure is not an answer");
        assert_eq!(error.category, category);
        assert_eq!(error.message, text);
    }
}

#[tokio::test]
async fn a_toolkit_normalised_by_case_is_noted_in_the_invalid_arguments_answer() {
    let result = with_failing_tool(ErrorCategory::InvalidInput).await;
    let failed = invoke(
        &result,
        json!({"toolkit": "GITHUB-MAIN", "tool": "boom", "arguments": {}}),
    )
    .await
    .expect("answer");
    let text = failed.as_str().expect("text");
    assert!(text.starts_with("ERROR: Invalid arguments for github-main.boom\n(Note: toolkit 'GITHUB-MAIN' resolved to 'github-main')"), "{text}");
    // The usage example names the toolkit that ran.
    assert!(text.contains("invoke_tool(toolkit=\"github-main\""));
}

#[tokio::test]
async fn the_partition_directory_resolves_like_invoke_tool() {
    let (result, _, _) = two_github_toolkits().await;
    assert_eq!(
        result
            .directory
            .resolve("GITHUB-B", "gh_01")
            .expect("resolved"),
        ("github-b".to_owned(), "gh_01".to_owned())
    );
    assert!(result.directory.resolve("", "gh_01").is_err());
    assert!(result.directory.resolve("github-c", "gh_01").is_err());
}

#[test]
fn python_style_dumps_matches_the_sdks_separators() {
    assert_eq!(
        py_dumps(&json!({"a": [1, 2], "b": {"c": "d"}})),
        r#"{"a": [1, 2], "b": {"c": "d"}}"#
    );
}
