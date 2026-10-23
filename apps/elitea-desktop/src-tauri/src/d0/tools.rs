//! Every tool of the turn, wrapped once: `tool_call`/`tool_result` events,
//! the recorded tool steps, the commands run, and the before-images of the
//! files a local file tool is about to change.
//!
//! Plan mode (#1225): [`PlanGatedToolset`] offers a toolset's tools only
//! once the session has left plan mode (the remote toolkits),
//! [`AcceptedPlanNote`] gives the model the build-mode environment once
//! it has, and an accepted `exit_plan_mode` copies the plan into the
//! runtime's planner (one host-side call, `Planner::seed_from_markdown`)
//! and tells the UI (`plan_accepted`).

use std::sync::Arc;

use adk_agent::LlmAgentBuilder;
use adk_core::callbacks::BeforeModelResult;
use adk_core::{Content, ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use elitea_agent_runtime::planner::Planner;
use elitea_local_tools::approvals::PermissionProfile;
use elitea_local_tools::policy::LocalWorkPolicy;
use elitea_local_tools::session::{EXIT_PLAN_MODE, LocalSession};
use serde_json::{Value, json};
use tokio::sync::OnceCell;

use super::environment;
use super::events::TurnEvents;
use super::recorder::Recorder;
use super::subagents::ChildLineage;

const SUMMARY_CHARS: usize = 240;

/// What the wrapper needs from the turn.
#[derive(Clone)]
pub struct ToolObserver {
    pub events: Arc<TurnEvents>,
    pub recorder: Arc<Recorder>,
    /// The workspace session, for local tools' before-images.
    pub session: Option<Arc<LocalSession>>,
    /// The turn's planner: an accepted `exit_plan_mode` seeds it.
    pub planner: Option<Planner>,
    /// A sub-agent's tools (#1226): its calls are recorded under its
    /// `spawn_agent` call and shown in its lane (`subagent_event`), not as
    /// the turn's own `tool_call`s.
    pub child: Option<Arc<ChildLineage>>,
}

/// A toolset offered only outside plan mode (the remote toolkits: a remote
/// tool may write). adk resolves the toolsets again after every tool batch,
/// so once the person accepts the plan the next model step of the same run
/// is offered these tools too.
pub struct PlanGatedToolset {
    inner: Arc<dyn Toolset>,
    session: Arc<LocalSession>,
}

impl PlanGatedToolset {
    #[must_use]
    pub fn new(inner: Arc<dyn Toolset>, session: Arc<LocalSession>) -> Self {
        Self { inner, session }
    }
}

#[async_trait]
impl Toolset for PlanGatedToolset {
    fn name(&self) -> &str {
        self.inner.name()
    }

    async fn tools(&self, ctx: Arc<dyn ReadonlyContext>) -> adk_core::Result<Vec<Arc<dyn Tool>>> {
        if self.session.profile() == PermissionProfile::Plan {
            return Ok(Vec::new());
        }
        self.inner.tools(ctx).await
    }
}

/// The build-mode environment for a plan-mode turn's model steps after the
/// person accepted the plan. The system prompt is fixed for the run and
/// says "plan (read-only)"; from the first model step outside plan mode on,
/// a system part ([`environment::accepted_plan`]) replaces that section:
/// the facts gathered again with the tools offered now (the write and
/// command guidance, the sandbox line), once per run.
pub struct AcceptedPlanNote {
    pub session: Arc<LocalSession>,
    /// The folder's name, as [`environment::gather`] takes it.
    pub folder: String,
    pub policy: LocalWorkPolicy,
    pub mentioned: Vec<String>,
    /// Tools offered beside the session's own (the index's, the planner's).
    pub extra_tools: Vec<String>,
}

impl AcceptedPlanNote {
    /// `builder` with the note as a callback before every model step.
    #[must_use]
    pub fn bind_builder(self, builder: LlmAgentBuilder) -> LlmAgentBuilder {
        let note = Arc::new(self);
        let text: Arc<OnceCell<String>> = Arc::new(OnceCell::new());
        builder.before_model_callback(Box::new(move |_ctx, mut request| {
            let note = note.clone();
            let text = text.clone();
            Box::pin(async move {
                if note.session.profile() == PermissionProfile::Plan {
                    return Ok(BeforeModelResult::Continue(request));
                }
                let section = text.get_or_init(|| note.section()).await;
                request
                    .contents
                    .insert(0, Content::new("system").with_text(section.clone()));
                Ok(BeforeModelResult::Continue(request))
            })
        }))
    }

    async fn section(&self) -> String {
        let mut tools: Vec<String> = self
            .session
            .available_tools()
            .iter()
            .map(|tool| tool.name.to_owned())
            .collect();
        tools.extend(self.extra_tools.iter().cloned());
        let facts = environment::gather(
            &self.session,
            self.folder.clone(),
            &self.policy,
            false,
            tools,
            &self.mentioned,
        )
        .await;
        environment::accepted_plan(&facts)
    }
}

/// A toolset whose tools report to the turn.
pub struct ObservedToolset {
    inner: Arc<dyn Toolset>,
    observer: ToolObserver,
    remote: bool,
}

impl ObservedToolset {
    #[must_use]
    pub fn new(inner: Arc<dyn Toolset>, observer: ToolObserver, remote: bool) -> Self {
        Self {
            inner,
            observer,
            remote,
        }
    }
}

#[async_trait]
impl Toolset for ObservedToolset {
    fn name(&self) -> &str {
        self.inner.name()
    }

    async fn tools(&self, ctx: Arc<dyn ReadonlyContext>) -> adk_core::Result<Vec<Arc<dyn Tool>>> {
        Ok(self
            .inner
            .tools(ctx)
            .await?
            .into_iter()
            .map(|tool| {
                Arc::new(ObservedTool {
                    inner: tool,
                    observer: self.observer.clone(),
                    remote: self.remote,
                }) as Arc<dyn Tool>
            })
            .collect())
    }
}

struct ObservedTool {
    inner: Arc<dyn Tool>,
    observer: ToolObserver,
    remote: bool,
}

impl ObservedTool {
    /// The person accepted the plan: the planner gets its steps when the
    /// model wrote none (the state rides this call's result event), and the
    /// UI turns plan mode off.
    fn plan_accepted(&self, ctx: &dyn ToolContext, plan: &str) {
        if let Some(planner) = &self.observer.planner
            && let Err(error) = planner.seed_from_markdown(ctx, plan)
        {
            log::warn!("the accepted plan was not copied to the planner: {error}");
        }
        self.observer.events.send(
            "plan_accepted",
            json!({ "call_id": ctx.function_call_id() }),
        );
    }
}

fn shorten(text: &str) -> (String, bool) {
    let mut chars = text.chars();
    let cut: String = chars.by_ref().take(SUMMARY_CHARS).collect();
    if chars.next().is_some() {
        (format!("{cut}…"), true)
    } else {
        (cut, false)
    }
}

fn args_summary(args: &Value) -> String {
    for key in ["command", "path", "pattern", "tool_name"] {
        if let Some(value) = args.get(key).and_then(Value::as_str) {
            return shorten(value).0;
        }
    }
    shorten(&args.to_string()).0
}

/// The paths a local file tool is about to change.
fn changed_paths(tool: &str, args: &Value) -> (Vec<String>, Vec<(String, String)>) {
    match tool {
        "write_file" | "edit_file" | "multi_edit" => (
            args.get("path")
                .and_then(Value::as_str)
                .map(|p| vec![p.to_owned()])
                .unwrap_or_default(),
            Vec::new(),
        ),
        "apply_patch" => {
            let Some(patch) = args.get("patch").and_then(Value::as_str) else {
                return (Vec::new(), Vec::new());
            };
            if elitea_local_tools::apply_patch::is_envelope(patch) {
                return elitea_local_tools::apply_patch::named_paths(patch).unwrap_or_default();
            }
            let Ok(files) = elitea_local_tools::patch::parse(patch) else {
                return (Vec::new(), Vec::new());
            };
            let mut paths = Vec::new();
            let mut renames = Vec::new();
            for file in &files {
                paths.extend(file.old_path.iter().cloned());
                paths.extend(file.new_path.iter().cloned());
                if let (true, Some(from), Some(to)) =
                    (file.is_rename(), &file.old_path, &file.new_path)
                {
                    renames.push((from.clone(), to.clone()));
                }
            }
            (paths, renames)
        }
        _ => (Vec::new(), Vec::new()),
    }
}

/// One call's share of the tools' wall time; ends when dropped, so a call
/// a cancel drops mid-way still ends.
struct ToolTimer<'a>(&'a TurnEvents);

impl<'a> ToolTimer<'a> {
    fn begin(events: &'a TurnEvents) -> Self {
        events.tool_began();
        Self(events)
    }
}

impl Drop for ToolTimer<'_> {
    fn drop(&mut self) {
        self.0.tool_ended();
    }
}

#[async_trait]
impl Tool for ObservedTool {
    fn name(&self) -> &str {
        self.inner.name()
    }

    fn description(&self) -> &str {
        self.inner.description()
    }

    fn declaration(&self) -> Value {
        self.inner.declaration()
    }

    fn enhanced_description(&self) -> String {
        self.inner.enhanced_description()
    }

    fn is_long_running(&self) -> bool {
        self.inner.is_long_running()
    }

    fn parameters_schema(&self) -> Option<Value> {
        self.inner.parameters_schema()
    }

    fn response_schema(&self) -> Option<Value> {
        self.inner.response_schema()
    }

    fn is_read_only(&self) -> bool {
        self.inner.is_read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        self.inner.is_concurrency_safe()
    }

    async fn execute(&self, ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        let call_id = ctx.function_call_id().to_owned();
        let tool = self.inner.name().to_owned();
        let observer = &self.observer;
        // Past the most tool calls a commit may carry the call is not run
        // (nor shown): the turn stops after this step (`turn.rs` `run_once`).
        let hierarchy = observer.child.as_ref().map(|child| child.hierarchy());
        if !observer
            .recorder
            .tool_started_under(&call_id, &tool, &args, hierarchy)
        {
            return Ok(json!({
                "status": "error",
                "message": format!(
                    "Not run: this turn reached its limit of {} tool calls.",
                    super::recorder::MAX_TOOL_CALLS
                ),
            }));
        }
        let call = json!({
            "call_id": call_id,
            "tool": tool,
            "args_summary": args_summary(&args),
            "remote": self.remote,
        });
        self.send("tool_call", call);
        if !self.remote
            && let Some(session) = &observer.session
        {
            let (paths, renames) = changed_paths(&tool, &args);
            if !paths.is_empty() {
                observer.recorder.before_change(session.workspace(), &paths);
            }
            for (from, to) in renames {
                observer.recorder.renamed(&from, &to);
            }
        }
        let command = (tool == "run_command" && !self.remote)
            .then(|| {
                args.get("command")
                    .and_then(Value::as_str)
                    .map(str::to_owned)
            })
            .flatten();
        let plan = (tool == EXIT_PLAN_MODE.name && !self.remote)
            .then(|| args.get("plan").and_then(Value::as_str).map(str::to_owned))
            .flatten();
        let running = ToolTimer::begin(&observer.events);
        let result = self.inner.execute(ctx.clone(), args).await;
        drop(running);
        if let (Some(plan), Ok(value)) = (&plan, &result)
            && value.get("accepted") == Some(&Value::Bool(true))
        {
            self.plan_accepted(ctx.as_ref(), plan);
        }
        // What the summary, the commit's steps and the history see of the
        // result: an image's bytes go to the model only (`read_file`).
        let observed = match &result {
            Ok(value) => Ok(elitea_agent_runtime::image_result::without_image(value)),
            Err(error) => Err(error),
        };
        let (ok, summary, truncated, error) = match &observed {
            Ok(value) => {
                let failed = value.get("status").and_then(Value::as_str) == Some("error");
                let message = value
                    .get("message")
                    .and_then(Value::as_str)
                    .map(str::to_owned);
                let (summary, cut) = shorten(&message.clone().unwrap_or_else(|| value.to_string()));
                let truncated = cut || value.get("truncated") == Some(&Value::Bool(true));
                (
                    !failed,
                    summary,
                    truncated,
                    failed.then(|| message.unwrap_or_else(|| "the tool failed".to_owned())),
                )
            }
            Err(error) => {
                let text = error.to_string();
                (false, shorten(&text).0, false, Some(text))
            }
        };
        // run_tests: the command it planned (and ran) is in its answer.
        let planned = (tool == "run_tests" && !self.remote)
            .then(|| observed.as_ref().ok())
            .flatten()
            .and_then(|value| value.get("command"))
            .and_then(Value::as_str)
            .map(str::to_owned);
        if let (Some(command), Ok(value)) = (command.as_ref().or(planned.as_ref()), &observed) {
            observer.recorder.command(command, value);
        }
        // A local MCP server's tool (#1230): the report names the server and
        // the tool.
        if !self.remote
            && tool.starts_with(elitea_local_tools::mcp::naming::PREFIX)
            && let Ok(value) = &observed
        {
            observer.recorder.mcp_call(value);
        }
        observer.recorder.tool_finished(
            &call_id,
            observed.as_ref().map_or(&Value::Null, |value| &**value),
            error.as_deref(),
        );
        self.send(
            "tool_result",
            json!({
                "call_id": call_id,
                "ok": ok,
                "summary": summary,
                "truncated": truncated,
            }),
        );
        result
    }
}

impl ObservedTool {
    /// A `tool_call` or `tool_result`: the turn's own, or, for a
    /// sub-agent's tool, inside its `subagent_event`.
    fn send(&self, kind: &'static str, mut payload: Value) {
        match &self.observer.child {
            Some(child) => {
                payload["kind"] = json!(kind);
                child.send(&self.observer.events, payload);
            }
            None => self.observer.events.send(kind, payload),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn patches_name_every_path_they_change() {
        let patch = "diff --git a/old.txt b/new.txt\nsimilarity index 100%\nrename from old.txt\nrename to new.txt\ndiff --git a/x.txt b/x.txt\n--- a/x.txt\n+++ b/x.txt\n@@ -1 +1 @@\n-a\n+b\n";
        let (paths, renames) = changed_paths("apply_patch", &json!({ "patch": patch }));
        assert!(paths.contains(&"old.txt".to_owned()));
        assert!(paths.contains(&"new.txt".to_owned()));
        assert!(paths.contains(&"x.txt".to_owned()));
        assert_eq!(renames, vec![("old.txt".to_owned(), "new.txt".to_owned())]);
        let envelope = "*** Begin Patch\n*** Add File: n.txt\n+x\n*** Delete File: d.txt\n*** Update File: u.txt\n*** Move to: v.txt\n@@\n-a\n+b\n*** End Patch";
        let (paths, renames) = changed_paths("apply_patch", &json!({ "patch": envelope }));
        assert_eq!(paths, ["n.txt", "d.txt", "u.txt", "v.txt"]);
        assert_eq!(renames, vec![("u.txt".to_owned(), "v.txt".to_owned())]);
        let (paths, _) = changed_paths("write_file", &json!({"path": "a.rs", "content": ""}));
        assert_eq!(paths, ["a.rs"]);
        let (paths, _) = changed_paths("multi_edit", &json!({"path": "m.rs", "edits": []}));
        assert_eq!(paths, ["m.rs"]);
        assert!(
            changed_paths("read_file", &json!({"path": "a.rs"}))
                .0
                .is_empty()
        );
    }

    #[test]
    fn summaries_are_short_and_say_when_cut() {
        assert_eq!(
            args_summary(&json!({"command": "cargo test"})),
            "cargo test"
        );
        let (text, cut) = shorten(&"x".repeat(500));
        assert!(cut);
        assert_eq!(text.chars().count(), SUMMARY_CHARS + 1);
    }
}
