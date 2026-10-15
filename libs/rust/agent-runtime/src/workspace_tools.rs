//! The cloud executor's tools (ADR-0034 decision 9): the desktop's local
//! tool names and schemas, run by the workspace runner inside the turn's
//! sandbox.
//!
//! [`WORKSPACE_TOOLS`] is `elitea-local-tools`' `TOOLS` minus `git_commit`
//! (the executor commits once, at the end of the turn), copied so this crate
//! keeps no dependency on local-tools. The workspace runner's contract test
//! (`libs/rust/workspace-runner/tests/contract.rs`) asserts the two tables
//! are equal, name for name and schema for schema, so a change to either
//! fails the build until the other follows.
//!
//! [`WorkspaceToolProvider`] is a [`ToolProvider`]: one toolset, named like
//! the local one, whose every call is sent to a [`WorkspaceRunner`] (the
//! worker's sandbox helper protocol). No tool runs in this process.

use std::sync::Arc;

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use serde_json::{Value, json};

use crate::host::{HostError, ToolProvider, ToolsetRequest};

/// The toolset's name: the local toolset's (`elitea_local_tools::provider::TOOLSET_NAME`).
pub const TOOLSET_NAME: &str = "local";

/// The tool the executor replaces with its end-of-turn commit.
pub const REPLACED_TOOL: &str = "git_commit";

/// One workspace tool: its name, description and schema.
pub struct WorkspaceToolSpec {
    pub name: &'static str,
    /// Whether the tool only reads (the local tools' `ToolKind::is_read_only`).
    pub read_only: bool,
    pub description: &'static str,
    pub parameters: fn() -> Value,
}

fn path_param(description: &str) -> Value {
    json!({ "type": "string", "description": description })
}

/// The tools, in the order the model sees them.
pub const WORKSPACE_TOOLS: &[WorkspaceToolSpec] = &[
    WorkspaceToolSpec {
        name: "read_file",
        read_only: true,
        description: "Read a UTF-8 text file in the workspace, with line numbers. Page long files with offset/limit. Reading a file is required before changing it.",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "path": path_param("Path relative to the workspace root."),
                    "offset": { "type": "integer", "minimum": 1, "description": "First line to return (1-based)." },
                    "limit": { "type": "integer", "minimum": 1, "description": "Number of lines (default 2000)." }
                },
                "required": ["path"]
            })
        },
    },
    WorkspaceToolSpec {
        name: "list_tree",
        read_only: true,
        description: "List directories and files under a workspace directory (ignored files left out).",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "path": path_param("Directory relative to the workspace root (default: the root)."),
                    "depth": { "type": "integer", "minimum": 1, "maximum": 12 },
                    "max_entries": { "type": "integer", "minimum": 1, "maximum": 5000 }
                }
            })
        },
    },
    WorkspaceToolSpec {
        name: "search_files",
        read_only: true,
        description: "Search text files in the workspace for a regular expression (or a literal string). Honours .gitignore.",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "pattern": { "type": "string" },
                    "path": path_param("Directory to search (default: the root)."),
                    "glob": { "type": "string", "description": "Only files matching this glob, e.g. *.rs" },
                    "fixed_strings": { "type": "boolean" },
                    "case_insensitive": { "type": "boolean" },
                    "max_results": { "type": "integer", "minimum": 1, "maximum": 1000 }
                },
                "required": ["pattern"]
            })
        },
    },
    WorkspaceToolSpec {
        name: "read_document",
        read_only: true,
        description: "Extract the text of a document in the workspace (PDF, Word, Excel, PowerPoint, e-mail, HTML, or plain text).",
        parameters: || {
            json!({
                "type": "object",
                "properties": { "path": path_param("Path relative to the workspace root.") },
                "required": ["path"]
            })
        },
    },
    WorkspaceToolSpec {
        name: "write_file",
        read_only: false,
        description: "Create a file, or replace one you have read, with the given content.",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "path": path_param("Path relative to the workspace root."),
                    "content": { "type": "string" }
                },
                "required": ["path", "content"]
            })
        },
    },
    WorkspaceToolSpec {
        name: "edit_file",
        read_only: false,
        description: "Replace an exact string in a file you have read. old_string must occur exactly once unless replace_all is true.",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "path": path_param("Path relative to the workspace root."),
                    "old_string": { "type": "string" },
                    "new_string": { "type": "string" },
                    "replace_all": { "type": "boolean" }
                },
                "required": ["path", "old_string", "new_string"]
            })
        },
    },
    WorkspaceToolSpec {
        name: "apply_patch",
        read_only: false,
        description: "Apply a unified diff (git diff format) to files in the workspace. Every hunk must match exactly; nothing is written if one does not.",
        parameters: || {
            json!({
                "type": "object",
                "properties": { "patch": { "type": "string" } },
                "required": ["patch"]
            })
        },
    },
    WorkspaceToolSpec {
        name: "run_command",
        read_only: false,
        description: "Run a command in the workspace under the OS sandbox. Plain commands run without a shell; pipes, redirects and && run under /bin/sh. Routine commands run without a question; destructive ones (recursive deletes, history rewrites, pushes, deploys, privilege), network and full-access are confirmed by the person.",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "command": { "type": "string" },
                    "cwd": path_param("Working directory relative to the workspace root."),
                    "timeout_seconds": { "type": "integer", "minimum": 1, "maximum": 600 },
                    "sandbox": { "type": "string", "enum": ["read-only", "workspace-write", "full-access"] },
                    "network": { "type": "boolean" }
                },
                "required": ["command"]
            })
        },
    },
    WorkspaceToolSpec {
        name: "git_status",
        read_only: true,
        description: "Show the git status of the workspace (short format, with the branch).",
        parameters: || json!({ "type": "object", "properties": {} }),
    },
    WorkspaceToolSpec {
        name: "git_diff",
        read_only: true,
        description: "Show git changes: the working tree against the index (default), staged changes, or against a revision.",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "staged": { "type": "boolean" },
                    "revision": { "type": "string", "description": "A commit: HEAD, a hash, a branch or tag, with ~n/^n, or A..B / A...B of those." },
                    "paths": { "type": "array", "items": { "type": "string" } },
                    "stat": { "type": "boolean" }
                }
            })
        },
    },
    WorkspaceToolSpec {
        name: "git_log",
        read_only: true,
        description: "Show recent commits (hash, date, author, subject).",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "max_count": { "type": "integer", "minimum": 1, "maximum": 200 },
                    "revision": { "type": "string", "description": "A commit: HEAD, a hash, a branch or tag, with ~n/^n, or A..B / A...B of those." },
                    "paths": { "type": "array", "items": { "type": "string" } }
                }
            })
        },
    },
    WorkspaceToolSpec {
        name: "git_branches",
        read_only: true,
        description: "List local branches, marking the current one.",
        parameters: || json!({ "type": "object", "properties": {} }),
    },
];

/// Runs one tool call in the sandbox's workspace runner.
#[async_trait]
pub trait WorkspaceRunner: Send + Sync {
    /// The JSON result the model reads. A tool failure is a result
    /// (`status: error`), not an `Err`; `Err` is the runner itself failing.
    async fn call(&self, tool: &str, call_id: &str, args: Value) -> Result<Value, HostError>;
}

/// One workspace tool bound to the runner.
pub struct WorkspaceTool {
    spec: &'static WorkspaceToolSpec,
    runner: Arc<dyn WorkspaceRunner>,
}

#[async_trait]
impl Tool for WorkspaceTool {
    fn name(&self) -> &str {
        self.spec.name
    }

    fn description(&self) -> &str {
        self.spec.description
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some((self.spec.parameters)())
    }

    fn is_read_only(&self) -> bool {
        self.spec.read_only
    }

    fn is_concurrency_safe(&self) -> bool {
        // One sandbox, one runner: calls are sent one at a time.
        false
    }

    async fn execute(&self, ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        // Failures are results the model reads, not run failures.
        Ok(
            match self
                .runner
                .call(self.spec.name, ctx.function_call_id(), args)
                .await
            {
                Ok(value) => value,
                Err(error) => json!({
                    "status": "error",
                    "code": error.reason_code(),
                    "message": error.to_string(),
                }),
            },
        )
    }
}

/// The workspace toolset.
pub struct WorkspaceToolset {
    tools: Vec<Arc<dyn Tool>>,
}

#[async_trait]
impl Toolset for WorkspaceToolset {
    fn name(&self) -> &str {
        TOOLSET_NAME
    }

    async fn tools(&self, _ctx: Arc<dyn ReadonlyContext>) -> adk_core::Result<Vec<Arc<dyn Tool>>> {
        Ok(self.tools.clone())
    }
}

/// The cloud executor's [`ToolProvider`]: the workspace toolset, and nothing
/// else (credentialed toolkits come from the host's own provider).
pub struct WorkspaceToolProvider {
    runner: Arc<dyn WorkspaceRunner>,
    shell: bool,
}

impl WorkspaceToolProvider {
    /// `shell`: the project's effective policy (decision 11); without it
    /// `run_command` is not offered.
    #[must_use]
    pub fn new(runner: Arc<dyn WorkspaceRunner>, shell: bool) -> Self {
        Self { runner, shell }
    }

    /// The tools offered, in model order.
    #[must_use]
    pub fn specs(&self) -> Vec<&'static WorkspaceToolSpec> {
        WORKSPACE_TOOLS
            .iter()
            .filter(|spec| self.shell || spec.name != "run_command")
            .collect()
    }

    /// The toolset, built once.
    #[must_use]
    pub fn toolset(&self) -> Arc<dyn Toolset> {
        Arc::new(WorkspaceToolset {
            tools: self
                .specs()
                .into_iter()
                .map(|spec| {
                    Arc::new(WorkspaceTool {
                        spec,
                        runner: Arc::clone(&self.runner),
                    }) as Arc<dyn Tool>
                })
                .collect(),
        })
    }
}

#[async_trait]
impl ToolProvider for WorkspaceToolProvider {
    async fn toolsets(
        &self,
        _request: &ToolsetRequest,
    ) -> Result<Vec<Arc<dyn Toolset>>, HostError> {
        Ok(vec![self.toolset()])
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Mutex;

    use super::*;
    use crate::host::HostErrorCode;

    struct Recorder(Mutex<Vec<(String, String, Value)>>);

    #[async_trait]
    impl WorkspaceRunner for Recorder {
        async fn call(&self, tool: &str, call_id: &str, args: Value) -> Result<Value, HostError> {
            self.0
                .lock()
                .expect("calls")
                .push((tool.to_owned(), call_id.to_owned(), args));
            if tool == "git_status" {
                return Err(HostError::new(
                    HostErrorCode::DependencyUnavailable,
                    "the workspace runner did not answer",
                ));
            }
            Ok(json!({"status": "ok"}))
        }
    }

    #[test]
    fn git_commit_is_replaced_and_shell_follows_the_policy() {
        let runner = Arc::new(Recorder(Mutex::new(Vec::new())));
        let names = |shell| {
            WorkspaceToolProvider::new(runner.clone(), shell)
                .specs()
                .iter()
                .map(|spec| spec.name)
                .collect::<Vec<_>>()
        };
        assert!(!names(true).contains(&REPLACED_TOOL));
        assert!(names(true).contains(&"run_command"));
        assert!(!names(false).contains(&"run_command"));
        assert_eq!(names(true).len(), 12);
        for spec in WORKSPACE_TOOLS {
            assert_eq!((spec.parameters)()["type"], "object", "{}", spec.name);
        }
    }

    #[tokio::test]
    async fn every_call_goes_to_the_runner_and_its_failure_is_a_result() {
        let runner = Arc::new(Recorder(Mutex::new(Vec::new())));
        let provider = WorkspaceToolProvider::new(runner.clone(), true);
        let toolset = provider.toolset();
        let tools = WorkspaceToolset {
            tools: provider
                .specs()
                .into_iter()
                .map(|spec| {
                    Arc::new(WorkspaceTool {
                        spec,
                        runner: runner.clone(),
                    }) as Arc<dyn Tool>
                })
                .collect(),
        };
        assert_eq!(toolset.name(), "local");
        let read = tools
            .tools
            .iter()
            .find(|tool| tool.name() == "read_file")
            .expect("read_file");
        assert!(read.is_read_only());
        let write = tools
            .tools
            .iter()
            .find(|tool| tool.name() == "write_file")
            .expect("write_file");
        assert!(!write.is_read_only());
        let status = tools
            .tools
            .iter()
            .find(|tool| tool.name() == "git_status")
            .expect("status");
        let ctx: Arc<dyn ToolContext> =
            Arc::new(adk_tool::SimpleToolContext::new("workspace-test"));
        let call_id = ctx.function_call_id().to_owned();
        let result = status.execute(ctx, json!({})).await.expect("result");
        assert_eq!(result["status"], "error");
        assert_eq!(result["code"], "runtime_host.dependency_unavailable");
        let calls = runner.0.lock().expect("calls");
        assert_eq!(calls[0].0, "git_status");
        assert_eq!(calls[0].1, call_id);
    }
}
