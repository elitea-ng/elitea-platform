//! The `planner` internal tool: a per-conversation execution plan the model
//! writes and ticks off (Python SDK `PlanningMiddleware` +
//! `PlanningWrapper`, `elitea_sdk/runtime/middleware/planning`).
//!
//! # What the SDK does, and what is the same here
//!
//! Five tools — `update_plan`, `start_step`, `complete_step`,
//! `get_plan_status`, `delete_plan` — with the SDK's names, descriptions,
//! argument names, result texts and plan rendering, plus the `PLAN_ADDON`
//! guidance appended to the system prompt whenever the toggle is on.
//!
//! # What is different, on purpose
//!
//! * **State.** The SDK keeps the plan in a JSON file under `$ELITEA_DIR/plans`
//!   (the worker passes no connection string), keyed by the model-visible
//!   `conversation_id` argument. Here the plan lives in ADK session state, as
//!   a `state_delta` on the tool-result event, so it is persisted with the
//!   conversation thread, survives context compaction (state is never
//!   summarised) and worker replacement, and cannot leak between
//!   conversations. The optional `conversation_id` tool argument is dropped
//!   from the schema and ignored if a model sends it: the scope is the
//!   session, and a model-supplied scope would only be a way to address
//!   someone else's plan.
//! * **In-turn consistency.** ADK publishes a model's tool batch before the
//!   state deltas of its result events are applied, so two plan calls in one
//!   batch would both start from the old session state. The toolset therefore
//!   holds the live plan for the run and every result event carries the full
//!   latest state; the last event wins and equals the sequential result.
//! * **Bounds.** At most [`MAX_STEPS`] steps, [`MAX_STEP_BYTES`] bytes per
//!   step and [`MAX_TITLE_CHARS`] characters of title (the SDK's database
//!   column is `varchar(255)`; the file store had no bound at all). An
//!   over-long plan is refused with a sentence the model can act on.

use std::sync::{Arc, Mutex};

use adk_core::{AdkError, ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

/// The toolset name; also the toolkit label on the model-visible tools.
pub const TOOLSET_NAME: &str = "planner";
/// Prefix of the session-state key that holds a scope's plan.
pub const STATE_PREFIX: &str = "elitea.plan.v1:";

/// The most steps one plan may hold.
pub const MAX_STEPS: usize = 100;
/// The longest step description, in bytes.
pub const MAX_STEP_BYTES: usize = 2_000;
/// The longest plan title, in characters.
pub const MAX_TITLE_CHARS: usize = 255;

const SCHEMA: u32 = 1;

pub const UPDATE_PLAN: &str = "update_plan";
pub const START_STEP: &str = "start_step";
pub const COMPLETE_STEP: &str = "complete_step";
pub const GET_PLAN_STATUS: &str = "get_plan_status";
pub const DELETE_PLAN: &str = "delete_plan";

/// The planning guidance appended to the system prompt: the SDK's
/// `PLAN_ADDON` (`elitea_sdk/runtime/langchain/constants.py`), verbatim.
pub const PLAN_ADDON: &str = "
---

## Planning

Use planning tools for organizing and tracking multi-step tasks.

### When to create a plan

Use `update_plan` when:
- Tasks involve multiple phases
- The sequence of activities matters
- Breaking down a complex task into trackable steps
- The user requests step-wise execution

### Planning workflow

1. **Create plan**: `update_plan(title=\"...\", steps=[...])`
2. **Start step**: `start_step(step_number=1)` — mark what you're working on
3. **Complete step**: `complete_step(step_number=1)` — mark when finished
4. **Check progress**: `get_plan_status()` — review current state

### Step status indicators

- ☐ pending — Not yet started
- ▶ in_progress — Currently working on this step
- ☑ completed — Step finished

### Resuming existing plans

**Important**: Before creating a new plan, check if one exists:

- If steps are already completed (☑), **do not recreate the plan**
- Continue from the next uncompleted step
- Use `start_step` to mark which step you're working on
- Use `complete_step` when finished

When resuming after interruption:
1. Review which steps are completed (☑) and in progress (▶)
2. Continue from the in-progress or next pending step
3. Do NOT recreate the plan — just continue execution

### Best practices

- Use `start_step` before beginning work on each step
- Mark `complete_step` immediately after finishing
- Only one step should be in progress at a time
- Keep plans concise (3-7 steps typically)

Example of a **high-quality plan**:

```
📋 API Test Investigation
   Progress: 1/5 completed, 1 in progress

   ☑ 1. Reproduce the failing test locally ✓
   ▶ 2. Capture error logs and stack trace (in progress)
   ☐ 3. Identify root cause in test or code
   ☐ 4. Apply fix with proper error handling
   ☐ 5. Run whole suite to confirm no regressions
```

Low-quality plans (\"run tests → fix things → done\") are not acceptable.
";

const PENDING: &str = "pending";
const IN_PROGRESS: &str = "in_progress";
const COMPLETED: &str = "completed";

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
struct Step {
    description: String,
    status: String,
}

impl Step {
    fn completed(&self) -> bool {
        self.status == COMPLETED
    }

    fn in_progress(&self) -> bool {
        self.status == IN_PROGRESS
    }
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
struct Plan {
    title: String,
    status: String,
    steps: Vec<Step>,
}

/// What a scope's state key holds. `plan: None` is a deleted (or never
/// written) plan: ADK state has no delete, so deletion is a written absence.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
struct Envelope {
    schema: u32,
    scope: String,
    plan: Option<Plan>,
}

impl Plan {
    /// The SDK's `PlanState.render`, character for character.
    fn render(&self) -> String {
        if self.steps.is_empty() {
            return "No plan created yet.".to_owned();
        }
        let mut lines: Vec<String> = Vec::new();
        if !self.title.is_empty() {
            lines.push(format!("📋 {}", self.title));
        }
        let completed = self.steps.iter().filter(|step| step.completed()).count();
        let in_progress = self.steps.iter().filter(|step| step.in_progress()).count();
        lines.push(format!(
            "   Progress: {completed}/{} completed, {in_progress} in progress",
            self.steps.len()
        ));
        lines.push(String::new());
        for (index, step) in self.steps.iter().enumerate() {
            let (indicator, suffix) = if step.completed() {
                ("☑", " ✓")
            } else if step.in_progress() {
                ("▶", " (in progress)")
            } else {
                ("☐", "")
            };
            lines.push(format!(
                "   {indicator} {}. {}{suffix}",
                index + 1,
                step.description
            ));
        }
        lines.join("\n")
    }
}

fn scope_of(ctx: &dyn ReadonlyContext) -> String {
    format!("{}:{}", ctx.session_id(), ctx.agent_name())
}

fn state_key(scope: &str) -> String {
    format!(
        "{STATE_PREFIX}{}",
        crate::instruction_authority::content_digest(scope)
    )
}

fn bounded(plan: &Plan) -> bool {
    plan.title.chars().count() <= MAX_TITLE_CHARS
        && plan.steps.len() <= MAX_STEPS
        && plan
            .steps
            .iter()
            .all(|step| step.description.len() <= MAX_STEP_BYTES && valid_status(&step.status))
        && matches!(plan.status.as_str(), IN_PROGRESS | COMPLETED)
}

fn valid_status(status: &str) -> bool {
    matches!(status, PENDING | IN_PROGRESS | COMPLETED)
}

/// Whether `value` is the well-formed plan state `key` must hold. The worker's
/// event projection admits a `state_delta` only when every entry passes
/// [`crate::instruction_authority::valid_state_delta`], which defers to this
/// for plan keys.
#[must_use]
pub fn valid_state_entry(key: &str, value: &Value) -> bool {
    let Ok(envelope) = serde_json::from_value::<Envelope>(value.clone()) else {
        return false;
    };
    envelope.schema == SCHEMA
        && key == state_key(&envelope.scope)
        && envelope.plan.as_ref().is_none_or(bounded)
}

/// Whether `key` names a plan state entry (valid or not).
#[must_use]
pub fn is_state_key(key: &str) -> bool {
    key.starts_with(STATE_PREFIX)
}

/// What a scope's state key held.
enum Stored {
    Plan(Option<Plan>),
    Unreadable,
}

const UNREADABLE: &str = "The stored plan is unreadable. Create a new plan with update_plan, or remove the stored one with delete_plan.";

struct Live {
    scope: String,
    plan: Option<Plan>,
}

/// The five tools over one shared live plan.
struct Shared {
    live: Mutex<Option<Live>>,
}

impl Shared {
    /// Read the plan, writing nothing. The first access of a run seeds the
    /// live copy from session state. A stored entry that cannot be read is
    /// reported as [`Stored::Unreadable`], never as a turn-failing error: the
    /// tools that only read say so, and the two that replace or remove the plan
    /// (`update_plan`, `delete_plan`) do not need the old value at all.
    fn current(&self, ctx: &dyn ToolContext) -> Result<(String, Stored), AdkError> {
        let scope = scope_of(ctx);
        let mut live = self.live.lock().map_err(|_| unavailable())?;
        if let Some(existing) = live.as_ref().filter(|existing| existing.scope == scope) {
            return Ok((scope, Stored::Plan(existing.plan.clone())));
        }
        let key = state_key(&scope);
        let stored = ctx.session().and_then(|session| session.state().get(&key));
        let plan = match stored {
            None => None,
            Some(value) => {
                if !valid_state_entry(&key, &value) {
                    return Ok((scope, Stored::Unreadable));
                }
                match serde_json::from_value::<Envelope>(value) {
                    Ok(envelope) => envelope.plan,
                    Err(_) => return Ok((scope, Stored::Unreadable)),
                }
            }
        };
        *live = Some(Live {
            scope: scope.clone(),
            plan: plan.clone(),
        });
        Ok((scope, Stored::Plan(plan)))
    }

    /// Make `plan` the live plan and carry it on the tool-result event.
    fn commit(
        &self,
        ctx: &dyn ToolContext,
        scope: &str,
        plan: Option<Plan>,
    ) -> Result<(), AdkError> {
        let envelope = Envelope {
            schema: SCHEMA,
            scope: scope.to_owned(),
            plan: plan.clone(),
        };
        let encoded = serde_json::to_value(&envelope).map_err(|_| corrupt())?;
        let mut live = self.live.lock().map_err(|_| unavailable())?;
        let mut actions = ctx.actions();
        actions.state_delta.insert(state_key(scope), encoded);
        ctx.set_actions(actions);
        *live = Some(Live {
            scope: scope.to_owned(),
            plan,
        });
        Ok(())
    }
}

/// The five plan tools as one toolset.
#[must_use]
pub fn toolset() -> Arc<dyn Toolset> {
    let shared = Arc::new(Shared {
        live: Mutex::new(None),
    });
    let tools: Vec<Arc<dyn Tool>> = [
        Action::UpdatePlan,
        Action::StartStep,
        Action::CompleteStep,
        Action::GetPlanStatus,
        Action::DeletePlan,
    ]
    .into_iter()
    .map(|action| {
        Arc::new(PlanTool {
            action,
            shared: Arc::clone(&shared),
        }) as Arc<dyn Tool>
    })
    .collect();
    Arc::new(BasicToolset::new(TOOLSET_NAME, tools))
}

#[derive(Clone, Copy)]
enum Action {
    UpdatePlan,
    StartStep,
    CompleteStep,
    GetPlanStatus,
    DeletePlan,
}

struct PlanTool {
    action: Action,
    shared: Arc<Shared>,
}

#[async_trait]
impl Tool for PlanTool {
    fn name(&self) -> &str {
        match self.action {
            Action::UpdatePlan => UPDATE_PLAN,
            Action::StartStep => START_STEP,
            Action::CompleteStep => COMPLETE_STEP,
            Action::GetPlanStatus => GET_PLAN_STATUS,
            Action::DeletePlan => DELETE_PLAN,
        }
    }

    fn description(&self) -> &str {
        match self.action {
            Action::UpdatePlan => {
                "Create or replace the current execution plan with a title and list of steps."
            }
            Action::StartStep => {
                "Mark a step as in progress. Only one step can be in progress at a time."
            }
            Action::CompleteStep => "Mark a step as completed.",
            Action::GetPlanStatus => "Get the current plan status and progress.",
            Action::DeletePlan => "Delete the current plan.",
        }
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(match self.action {
            Action::UpdatePlan => json!({
                "type": "object",
                "properties": {
                    "title": {"type": "string", "description": "Title for the plan"},
                    "steps": {
                        "type": "array",
                        "items": {"type": "string"},
                        "description": "List of step descriptions in order"
                    }
                },
                "required": ["title", "steps"]
            }),
            Action::StartStep => step_schema("Step number to start (1-indexed)"),
            Action::CompleteStep => step_schema("Step number to complete (1-indexed)"),
            Action::GetPlanStatus | Action::DeletePlan => json!({
                "type": "object",
                "properties": {}
            }),
        })
    }

    fn response_schema(&self) -> Option<Value> {
        Some(json!({"type": "string"}))
    }

    // Every plan tool reads the live plan and four of the five write it: they
    // run one at a time, in call order, so a batch equals the sequential run.
    fn is_read_only(&self) -> bool {
        false
    }

    fn is_concurrency_safe(&self) -> bool {
        false
    }

    async fn execute(&self, ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        let text = match self.action {
            Action::UpdatePlan => self.update_plan(ctx.as_ref(), &args)?,
            Action::StartStep => self.step(ctx.as_ref(), &args, true)?,
            Action::CompleteStep => self.step(ctx.as_ref(), &args, false)?,
            Action::GetPlanStatus => self.get_plan_status(ctx.as_ref())?,
            Action::DeletePlan => self.delete_plan(ctx.as_ref())?,
        };
        Ok(Value::String(text))
    }
}

fn step_schema(description: &str) -> Value {
    json!({
        "type": "object",
        "properties": {"step_number": {"type": "integer", "description": description}},
        "required": ["step_number"]
    })
}

impl PlanTool {
    fn update_plan(&self, ctx: &dyn ToolContext, args: &Value) -> Result<String, AdkError> {
        let Some(title) = args.get("title").and_then(Value::as_str) else {
            return Ok("Error updating plan: title is required".to_owned());
        };
        let Some(steps) = args.get("steps").and_then(Value::as_array) else {
            return Ok("Error updating plan: steps is required".to_owned());
        };
        let Some(steps) = steps
            .iter()
            .map(|step| step.as_str().map(str::to_owned))
            .collect::<Option<Vec<_>>>()
        else {
            return Ok("Error updating plan: every step must be a string".to_owned());
        };
        if title.chars().count() > MAX_TITLE_CHARS {
            return Ok(format!(
                "Error updating plan: the title is longer than {MAX_TITLE_CHARS} characters."
            ));
        }
        if steps.len() > MAX_STEPS {
            return Ok(format!(
                "Error updating plan: a plan holds at most {MAX_STEPS} steps."
            ));
        }
        if steps.iter().any(|step| step.len() > MAX_STEP_BYTES) {
            return Ok(format!(
                "Error updating plan: a step is longer than {MAX_STEP_BYTES} bytes."
            ));
        }
        let (scope, stored) = self.shared.current(ctx)?;
        // An unreadable old plan is simply replaced.
        let existing = !matches!(stored, Stored::Plan(None));
        let plan = Plan {
            title: title.to_owned(),
            status: IN_PROGRESS.to_owned(),
            steps: steps
                .into_iter()
                .map(|description| Step {
                    description,
                    status: PENDING.to_owned(),
                })
                .collect(),
        };
        let action = if existing { "updated" } else { "created" };
        let rendered = plan.render();
        self.shared.commit(ctx, &scope, Some(plan))?;
        Ok(format!("Plan {action}:\n\n{rendered}"))
    }

    fn step(&self, ctx: &dyn ToolContext, args: &Value, start: bool) -> Result<String, AdkError> {
        let verb = if start { "starting" } else { "completing" };
        let Some(number) = args.get("step_number").and_then(Value::as_i64) else {
            return Ok(format!("Error {verb} step: step_number must be an integer"));
        };
        let (scope, stored) = self.shared.current(ctx)?;
        let plan = match stored {
            Stored::Plan(plan) => plan,
            Stored::Unreadable => return Ok(UNREADABLE.to_owned()),
        };
        let Some(mut plan) = plan.filter(|plan| !plan.steps.is_empty()) else {
            return Ok("No plan exists. Use update_plan first to create a plan.".to_owned());
        };
        let total = plan.steps.len();
        let Some(index) = usize::try_from(number)
            .ok()
            .filter(|number| (1..=total).contains(number))
        else {
            return Ok(format!(
                "Invalid step number. Plan has {total} steps (1-{total})."
            ));
        };
        let number = index;
        let index = number - 1;
        if start {
            if plan.steps[index].completed() {
                return Ok(format!(
                    "Step {number} is already completed.\n\n{}",
                    plan.render()
                ));
            }
            if plan.steps[index].in_progress() {
                return Ok(format!(
                    "Step {number} is already in progress.\n\n{}",
                    plan.render()
                ));
            }
            for step in &mut plan.steps {
                if step.in_progress() {
                    PENDING.clone_into(&mut step.status);
                }
            }
            IN_PROGRESS.clone_into(&mut plan.steps[index].status);
            let description = plan.steps[index].description.clone();
            let rendered = plan.render();
            self.shared.commit(ctx, &scope, Some(plan))?;
            return Ok(format!(
                "Started step {number}: {description}\n\n{rendered}"
            ));
        }
        if plan.steps[index].completed() {
            return Ok(format!(
                "Step {number} was already completed.\n\n{}",
                plan.render()
            ));
        }
        COMPLETED.clone_into(&mut plan.steps[index].status);
        if plan.steps.iter().all(Step::completed) {
            COMPLETED.clone_into(&mut plan.status);
        }
        let done = plan.steps.iter().filter(|step| step.completed()).count();
        let rendered = plan.render();
        self.shared.commit(ctx, &scope, Some(plan))?;
        Ok(format!(
            "Step {number} completed ({done}/{total} done)\n\n{rendered}"
        ))
    }

    fn get_plan_status(&self, ctx: &dyn ToolContext) -> Result<String, AdkError> {
        let (_, stored) = self.shared.current(ctx)?;
        let plan = match stored {
            Stored::Plan(plan) => plan,
            Stored::Unreadable => return Ok(UNREADABLE.to_owned()),
        };
        Ok(plan.map_or_else(
            || "No plan exists for the current conversation.".to_owned(),
            |plan| plan.render(),
        ))
    }

    fn delete_plan(&self, ctx: &dyn ToolContext) -> Result<String, AdkError> {
        let (scope, stored) = self.shared.current(ctx)?;
        let plan = match stored {
            Stored::Plan(plan) => plan,
            Stored::Unreadable => {
                self.shared.commit(ctx, &scope, None)?;
                return Ok("The unreadable stored plan was deleted.".to_owned());
            }
        };
        let Some(plan) = plan else {
            return Ok("No plan exists for the current conversation.".to_owned());
        };
        let title = plan.title;
        self.shared.commit(ctx, &scope, None)?;
        Ok(format!("Plan '{title}' deleted successfully."))
    }
}

fn corrupt() -> AdkError {
    AdkError::new(
        adk_core::ErrorComponent::Tool,
        adk_core::ErrorCategory::InvalidInput,
        "agent.planner.invalid_state",
        "the stored plan state is invalid",
    )
}

fn unavailable() -> AdkError {
    AdkError::new(
        adk_core::ErrorComponent::Tool,
        adk_core::ErrorCategory::Unavailable,
        "agent.planner.unavailable",
        "the plan state is unavailable",
    )
}

#[cfg(test)]
#[path = "planner_tests.rs"]
mod tests;
