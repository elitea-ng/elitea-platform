//! The planner's tools on their own: the SDK's names, schemas and result
//! texts, the plan state they write, and the bounds they hold. The worker's
//! suites run the same toolset through a real agent graph and session.

use std::collections::HashMap;

use adk_tool::SimpleToolContext;
use serde_json::json;

use super::*;

async fn tools() -> Vec<Arc<dyn Tool>> {
    toolset()
        .tools(Arc::new(SimpleToolContext::new("planner-test")))
        .await
        .expect("planner tools")
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("planner-test").with_session_id("session-1"))
}

async fn call(
    tools: &[Arc<dyn Tool>],
    ctx: &Arc<dyn ToolContext>,
    name: &str,
    args: Value,
) -> String {
    let tool = tools
        .iter()
        .find(|tool| tool.name() == name)
        .expect("planner tool");
    let value = tool.execute(Arc::clone(ctx), args).await.expect("result");
    value.as_str().expect("text result").to_owned()
}

fn delta(ctx: &Arc<dyn ToolContext>) -> HashMap<String, Value> {
    ctx.actions().state_delta
}

#[tokio::test]
async fn the_toolset_serves_the_sdks_five_tools_with_its_descriptions_and_arguments() {
    let tools = tools().await;
    let names: Vec<&str> = tools.iter().map(|tool| tool.name()).collect();
    assert_eq!(
        names,
        [
            "update_plan",
            "start_step",
            "complete_step",
            "get_plan_status",
            "delete_plan"
        ]
    );
    let descriptions: Vec<&str> = tools.iter().map(|tool| tool.description()).collect();
    assert_eq!(
        descriptions,
        [
            "Create or replace the current execution plan with a title and list of steps.",
            "Mark a step as in progress. Only one step can be in progress at a time.",
            "Mark a step as completed.",
            "Get the current plan status and progress.",
            "Delete the current plan.",
        ]
    );
    let update = tools[0].parameters_schema().expect("schema");
    assert_eq!(update["required"], json!(["title", "steps"]));
    assert_eq!(update["properties"]["steps"]["items"]["type"], "string");
    for tool in &tools[1..3] {
        let schema = tool.parameters_schema().expect("schema");
        assert_eq!(schema["required"], json!(["step_number"]));
        assert_eq!(schema["properties"]["step_number"]["type"], "integer");
    }
    // The SDK's `conversation_id` argument is deliberately not offered.
    assert!(tools.iter().all(|tool| {
        tool.parameters_schema().expect("schema")["properties"]
            .get("conversation_id")
            .is_none()
    }));
    assert!(tools.iter().all(|tool| !tool.is_read_only()));
}

#[tokio::test]
async fn a_plan_is_created_started_completed_and_rendered_like_the_sdk() {
    let tools = tools().await;
    let ctx = context();
    assert_eq!(
        call(&tools, &ctx, "get_plan_status", json!({})).await,
        "No plan exists for the current conversation."
    );
    assert_eq!(
        call(&tools, &ctx, "start_step", json!({"step_number": 1})).await,
        "No plan exists. Use update_plan first to create a plan."
    );
    let created = call(
        &tools,
        &ctx,
        "update_plan",
        json!({"title": "Fix it", "steps": ["Reproduce", "Patch", "Verify"]}),
    )
    .await;
    assert_eq!(
        created,
        "Plan created:\n\n📋 Fix it\n   Progress: 0/3 completed, 0 in progress\n\n   ☐ 1. Reproduce\n   ☐ 2. Patch\n   ☐ 3. Verify"
    );
    let started = call(&tools, &ctx, "start_step", json!({"step_number": 1})).await;
    assert_eq!(
        started,
        "Started step 1: Reproduce\n\n📋 Fix it\n   Progress: 0/3 completed, 1 in progress\n\n   ▶ 1. Reproduce (in progress)\n   ☐ 2. Patch\n   ☐ 3. Verify"
    );
    assert!(
        call(&tools, &ctx, "start_step", json!({"step_number": 1}))
            .await
            .starts_with("Step 1 is already in progress.\n\n")
    );
    // Starting another step puts the first back to pending.
    let second = call(&tools, &ctx, "start_step", json!({"step_number": 2})).await;
    assert!(second.contains("☐ 1. Reproduce\n   ▶ 2. Patch (in progress)"));
    let done = call(&tools, &ctx, "complete_step", json!({"step_number": 2})).await;
    assert!(done.starts_with("Step 2 completed (1/3 done)\n\n"));
    assert!(
        call(&tools, &ctx, "complete_step", json!({"step_number": 2}))
            .await
            .starts_with("Step 2 was already completed.\n\n")
    );
    assert!(
        call(&tools, &ctx, "start_step", json!({"step_number": 2}))
            .await
            .starts_with("Step 2 is already completed.\n\n")
    );
    call(&tools, &ctx, "complete_step", json!({"step_number": 1})).await;
    let last = call(&tools, &ctx, "complete_step", json!({"step_number": 3})).await;
    assert!(last.starts_with("Step 3 completed (3/3 done)\n\n"));
    assert!(last.contains("☑ 3. Verify ✓"));
    let status = call(&tools, &ctx, "get_plan_status", json!({})).await;
    assert!(status.contains("Progress: 3/3 completed, 0 in progress"));
    let state = delta(&ctx).into_values().next().expect("plan state");
    assert_eq!(state["plan"]["status"], "completed");

    // Replacing the plan says "updated" and starts over.
    let replaced = call(
        &tools,
        &ctx,
        "update_plan",
        json!({"title": "Next", "steps": ["One"]}),
    )
    .await;
    assert!(replaced.starts_with("Plan updated:\n\n📋 Next"));
}

#[tokio::test]
async fn step_numbers_outside_the_plan_are_answered_not_failed() {
    let tools = tools().await;
    let ctx = context();
    call(
        &tools,
        &ctx,
        "update_plan",
        json!({"title": "T", "steps": ["a", "b"]}),
    )
    .await;
    for number in [0, 3, -1] {
        for name in ["start_step", "complete_step"] {
            assert_eq!(
                call(&tools, &ctx, name, json!({"step_number": number})).await,
                "Invalid step number. Plan has 2 steps (1-2)."
            );
        }
    }
    assert!(
        call(&tools, &ctx, "start_step", json!({"step_number": "x"}))
            .await
            .starts_with("Error starting step:")
    );
}

#[tokio::test]
async fn a_deleted_plan_is_a_written_absence_not_a_missing_key() {
    let tools = tools().await;
    let ctx = context();
    call(
        &tools,
        &ctx,
        "update_plan",
        json!({"title": "Gone", "steps": ["a"]}),
    )
    .await;
    assert_eq!(
        call(&tools, &ctx, "delete_plan", json!({})).await,
        "Plan 'Gone' deleted successfully."
    );
    assert_eq!(
        call(&tools, &ctx, "delete_plan", json!({})).await,
        "No plan exists for the current conversation."
    );
    let state = delta(&ctx).into_iter().next().expect("state");
    assert!(state.1["plan"].is_null());
    assert!(valid_state_entry(&state.0, &state.1));
}

/// ADK publishes a tool batch before any of its state deltas are applied, so
/// two calls in one batch both read the OLD session state. The live plan makes
/// the last event carry the sequential result.
#[tokio::test]
async fn two_calls_in_one_batch_leave_the_sequential_result_in_the_last_delta() {
    let tools = tools().await;
    let ctx = context();
    call(
        &tools,
        &ctx,
        "update_plan",
        json!({"title": "T", "steps": ["a", "b"]}),
    )
    .await;
    call(&tools, &ctx, "start_step", json!({"step_number": 1})).await;
    call(&tools, &ctx, "complete_step", json!({"step_number": 1})).await;
    call(&tools, &ctx, "start_step", json!({"step_number": 2})).await;
    let (key, value) = delta(&ctx).into_iter().next().expect("last delta");
    assert!(valid_state_entry(&key, &value));
    let steps = value["plan"]["steps"].as_array().expect("steps");
    assert_eq!(steps[0]["status"], "completed");
    assert_eq!(steps[1]["status"], "in_progress");
}

#[tokio::test]
async fn the_plan_is_bounded_and_a_refusal_writes_nothing() {
    let tools = tools().await;
    let ctx = context();
    let too_many: Vec<String> = (0..=MAX_STEPS).map(|n| format!("step {n}")).collect();
    for args in [
        json!({"title": "T", "steps": too_many}),
        json!({"title": "x".repeat(MAX_TITLE_CHARS + 1), "steps": ["a"]}),
        json!({"title": "T", "steps": ["y".repeat(MAX_STEP_BYTES + 1)]}),
        json!({"title": "T", "steps": [1]}),
        json!({"steps": ["a"]}),
    ] {
        assert!(
            call(&tools, &ctx, "update_plan", args)
                .await
                .starts_with("Error updating plan:")
        );
    }
    assert!(delta(&ctx).is_empty(), "a refused plan must write no state");
}

#[test]
fn only_a_well_formed_plan_entry_under_its_own_key_is_valid() {
    let scope = "session-1:agent";
    let key = state_key(scope);
    let good = json!({
        "schema": 1, "scope": scope,
        "plan": {"title": "T", "status": "in_progress",
                 "steps": [{"description": "a", "status": "pending"}]}
    });
    assert!(valid_state_entry(&key, &good));
    assert!(valid_state_entry(
        &key,
        &json!({"schema": 1, "scope": scope, "plan": null})
    ));
    // Another scope's key, a wrong schema, an unknown field, an unknown step
    // status and an over-long step are all refused.
    assert!(!valid_state_entry(&state_key("other"), &good));
    let mut wrong_schema = good.clone();
    wrong_schema["schema"] = json!(2);
    assert!(!valid_state_entry(&key, &wrong_schema));
    let mut extra = good.clone();
    extra["extra"] = json!(true);
    assert!(!valid_state_entry(&key, &extra));
    let mut status = good.clone();
    status["plan"]["steps"][0]["status"] = json!("done");
    assert!(!valid_state_entry(&key, &status));
    let mut long = good.clone();
    long["plan"]["steps"][0]["description"] = json!("z".repeat(MAX_STEP_BYTES + 1));
    assert!(!valid_state_entry(&key, &long));
    assert!(!valid_state_entry(&key, &json!("not an object")));
}

/// The worker admits a tool-result event's `state_delta` only through
/// `valid_state_delta`; a plan delta must pass it without being read as skill
/// activation.
#[test]
fn a_plan_delta_passes_the_event_gate_and_activates_no_skill() {
    let scope = "s:a";
    let key = state_key(scope);
    let plan = json!({"schema": 1, "scope": scope, "plan": null});
    let delta = HashMap::from([(key.clone(), plan.clone())]);
    assert!(crate::instruction_authority::valid_state_delta(&delta));
    assert!(crate::instruction_authority::public_active_delta(&delta).is_none());
    let bad = HashMap::from([(key, json!({"schema": 9, "scope": scope, "plan": null}))]);
    assert!(!crate::instruction_authority::valid_state_delta(&bad));
}

#[test]
fn the_plan_addon_is_the_sdks_text() {
    assert!(PLAN_ADDON.starts_with("\n---\n\n## Planning\n\nUse planning tools"));
    assert!(PLAN_ADDON.contains("1. **Create plan**: `update_plan(title=\"...\", steps=[...])`"));
    assert!(PLAN_ADDON.contains("   ☑ 1. Reproduce the failing test locally ✓"));
    assert!(PLAN_ADDON.ends_with("are not acceptable.\n"));
}

// ---------------------------------------------------------------------------
// A stored entry the tool cannot read.
// ---------------------------------------------------------------------------

struct FixedState(HashMap<String, Value>);

impl adk_core::State for FixedState {
    fn get(&self, key: &str) -> Option<Value> {
        self.0.get(key).cloned()
    }

    fn set(&mut self, key: String, value: Value) {
        self.0.insert(key, value);
    }

    fn all(&self) -> HashMap<String, Value> {
        self.0.clone()
    }
}

struct FixedSession(FixedState);

impl adk_core::Session for FixedSession {
    fn id(&self) -> &'static str {
        "session-1"
    }

    fn app_name(&self) -> &'static str {
        "planner-test"
    }

    fn user_id(&self) -> &'static str {
        "user"
    }

    fn state(&self) -> &dyn adk_core::State {
        &self.0
    }

    fn conversation_history(&self) -> Vec<adk_core::Content> {
        Vec::new()
    }
}

/// A tool context whose session already holds `stored` under the planner key.
struct SessionContext {
    inner: SimpleToolContext,
    session: FixedSession,
}

impl SessionContext {
    fn with_entry(value: Value) -> Arc<dyn ToolContext> {
        let inner = SimpleToolContext::new("planner-test").with_session_id("session-1");
        let key = state_key(&scope_of(&inner));
        Arc::new(Self {
            inner,
            session: FixedSession(FixedState(HashMap::from([(key, value)]))),
        })
    }
}

#[async_trait::async_trait]
impl adk_core::ReadonlyContext for SessionContext {
    fn invocation_id(&self) -> &str {
        self.inner.invocation_id()
    }
    fn agent_name(&self) -> &str {
        self.inner.agent_name()
    }
    fn user_id(&self) -> &str {
        self.inner.user_id()
    }
    fn app_name(&self) -> &str {
        self.inner.app_name()
    }
    fn session_id(&self) -> &str {
        self.inner.session_id()
    }
    fn branch(&self) -> &str {
        self.inner.branch()
    }
    fn user_content(&self) -> &adk_core::Content {
        self.inner.user_content()
    }
}

#[async_trait::async_trait]
impl adk_core::CallbackContext for SessionContext {
    fn artifacts(&self) -> Option<Arc<dyn adk_core::Artifacts>> {
        None
    }
}

#[async_trait::async_trait]
impl ToolContext for SessionContext {
    fn function_call_id(&self) -> &str {
        self.inner.function_call_id()
    }
    fn actions(&self) -> adk_core::EventActions {
        self.inner.actions()
    }
    fn set_actions(&self, actions: adk_core::EventActions) {
        self.inner.set_actions(actions);
    }
    async fn search_memory(&self, query: &str) -> adk_core::Result<Vec<adk_core::MemoryEntry>> {
        self.inner.search_memory(query).await
    }
    fn session(&self) -> Option<&dyn adk_core::Session> {
        Some(&self.session)
    }
}

fn corrupt_entry() -> Value {
    json!({"schema": 1, "scope": "session-1:planner-test", "plan": {"title": 7}})
}

#[tokio::test]
async fn tools_that_only_read_say_the_stored_plan_is_unreadable_instead_of_failing_the_turn() {
    for name in ["get_plan_status", "start_step", "complete_step"] {
        let tools = tools().await;
        let ctx = SessionContext::with_entry(corrupt_entry());
        let answer = call(&tools, &ctx, name, json!({"step_number": 1})).await;
        assert!(
            answer.starts_with("The stored plan is unreadable."),
            "{name}: {answer}"
        );
        assert!(answer.contains("update_plan") && answer.contains("delete_plan"));
        assert!(delta(&ctx).is_empty(), "{name} must write nothing");
    }
}

#[tokio::test]
async fn update_and_delete_do_not_need_the_old_value_and_clear_an_unreadable_one() {
    let tools = tools().await;
    let ctx = SessionContext::with_entry(corrupt_entry());
    let created = call(
        &tools,
        &ctx,
        "update_plan",
        json!({"title": "Fresh", "steps": ["a"]}),
    )
    .await;
    assert!(
        created.starts_with("Plan updated:\n\n📋 Fresh"),
        "{created}"
    );
    let (key, value) = delta(&ctx).into_iter().next().expect("replacement written");
    assert!(valid_state_entry(&key, &value));

    let tools = self::tools().await;
    let ctx = SessionContext::with_entry(corrupt_entry());
    assert_eq!(
        call(&tools, &ctx, "delete_plan", json!({})).await,
        "The unreadable stored plan was deleted."
    );
    let (key, value) = delta(&ctx).into_iter().next().expect("removal written");
    assert!(value["plan"].is_null() && valid_state_entry(&key, &value));
    // After the removal the run sees no plan, not an unreadable one.
    assert_eq!(
        call(&tools, &ctx, "get_plan_status", json!({})).await,
        "No plan exists for the current conversation."
    );
}

#[test]
fn the_state_key_uses_the_shared_content_digest() {
    assert_eq!(
        state_key("s:a"),
        format!(
            "{STATE_PREFIX}{}",
            crate::instruction_authority::content_digest("s:a")
        )
    );
}
