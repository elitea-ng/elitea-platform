//! Every tool of the turn, wrapped once: `tool_call`/`tool_result` events,
//! the recorded tool steps, the commands run, and the before-images of the
//! files a local file tool is about to change.

use std::sync::Arc;

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use elitea_local_tools::session::LocalSession;
use serde_json::{Value, json};

use super::events::TurnEvents;
use super::recorder::Recorder;

const SUMMARY_CHARS: usize = 240;

/// What the wrapper needs from the turn.
#[derive(Clone)]
pub struct ToolObserver {
    pub events: Arc<TurnEvents>,
    pub recorder: Arc<Recorder>,
    /// The workspace session, for local tools' before-images.
    pub session: Option<Arc<LocalSession>>,
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
        "write_file" | "edit_file" => (
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
        observer.events.send(
            "tool_call",
            json!({
                "call_id": call_id,
                "tool": tool,
                "args_summary": args_summary(&args),
                "remote": self.remote,
            }),
        );
        observer.recorder.tool_started(&call_id, &tool, &args);
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
        let result = self.inner.execute(ctx, args).await;
        let (ok, summary, truncated, error) = match &result {
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
        if let (Some(command), Ok(value)) = (&command, &result) {
            observer.recorder.command(command, value);
        }
        observer.recorder.tool_finished(
            &call_id,
            result.as_ref().unwrap_or(&Value::Null),
            error.as_deref(),
        );
        observer.events.send(
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
        let (paths, _) = changed_paths("write_file", &json!({"path": "a.rs", "content": ""}));
        assert_eq!(paths, ["a.rs"]);
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
