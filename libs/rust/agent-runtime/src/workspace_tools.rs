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

use crate::edit_profile::{
    APPLY_PATCH, APPLY_PATCH_DESCRIPTION, APPLY_PATCH_ENVELOPE_DESCRIPTION, EditProfile,
    MULTI_EDIT_DESCRIPTION, multi_edit_params, patch_param,
};
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

/// The tools that run commands: offered only with the shell policy.
const RUNS_COMMANDS: &[&str] = &["run_command", "run_tests"];

/// The tools, in the order the model sees them.
pub const WORKSPACE_TOOLS: &[WorkspaceToolSpec] = &[
    WorkspaceToolSpec {
        name: "read_file",
        read_only: true,
        description: "Read a UTF-8 text file in the workspace, with line numbers. Page long files with offset/limit; a line too long for one page is cut, and next_offset with next_column continue inside it. Reading a file is required before changing it.",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "path": path_param("Path relative to the workspace root."),
                    "offset": { "type": "integer", "minimum": 1, "description": "First line to return (1-based)." },
                    "column": { "type": "integer", "minimum": 0, "description": "Byte offset inside the first line to continue from: the next_column of a cut line." },
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
        description: "Search the text inside workspace files for a regular expression (or a literal string), like rg: matching lines with optional context lines (output_mode content), only the matching paths (files), or matches per file (count). Honours .gitignore. To find files by name use find_files.",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "pattern": { "type": "string" },
                    "path": path_param("Directory to search (default: the root)."),
                    "glob": { "type": "string", "description": "Only files matching this glob, e.g. *.rs" },
                    "fixed_strings": { "type": "boolean" },
                    "case_insensitive": { "type": "boolean" },
                    "max_results": { "type": "integer", "minimum": 1, "maximum": 1000 },
                    "output_mode": { "type": "string", "enum": ["content", "files", "count"], "description": "content (default): matching lines; files: paths only; count: matches per file." },
                    "context_before": { "type": "integer", "minimum": 0, "maximum": 20 },
                    "context_after": { "type": "integer", "minimum": 0, "maximum": 20 },
                    "context": { "type": "integer", "minimum": 0, "maximum": 20, "description": "Lines before and after (like -C)." },
                    "multiline": { "type": "boolean", "description": "Let the pattern span lines (like rg -U)." },
                    "max_per_file": { "type": "integer", "minimum": 1, "maximum": 1000 }
                },
                "required": ["pattern"]
            })
        },
    },
    WorkspaceToolSpec {
        name: "find_files",
        read_only: true,
        description: "Find files in the workspace by name: a glob (src/**/*.rs, *.toml, !*.md for every file but those; newest first) or, with fuzzy=true, a name fragment (usrctrl; best match first). A glob with / matches the path under `path` (or from the workspace root); one without matches the file name. Lists paths only and opens nothing; .gitignore always honoured. For text inside files use search_files.",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "pattern": { "type": "string", "description": "A glob (src/**/*.rs, !*.md) or, with fuzzy=true, a name fragment (usrctrl)." },
                    "path": path_param("Directory relative to the workspace root (default: the root)."),
                    "fuzzy": { "type": "boolean", "description": "Rank by fuzzy match instead of glob filtering." },
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
        name: "read_output",
        read_only: true,
        description: "Read a page of a tool output that was too long to show in full: the output_id a run_command, run_tests, git_diff, git_log or read_document result gave, with line numbers. Only this conversation's outputs, kept for a day; page with offset/limit (and next_column inside a long line). output_expired means it is gone: re-run the tool.",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "output_id": { "type": "string" },
                    "offset": { "type": "integer", "minimum": 1, "description": "First line (1-based)." },
                    "column": { "type": "integer", "minimum": 0, "description": "Byte offset inside the first line to continue from: the next_column of a cut line." },
                    "limit": { "type": "integer", "minimum": 1 }
                },
                "required": ["output_id"]
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
        description: "Replace a string in a file you have read. old_string must occur exactly once unless replace_all is true; when it is not there exactly, a match ignoring line endings, trailing whitespace and a uniform indentation shift is used and reported. The result shows the changed lines.",
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
        name: "multi_edit",
        read_only: false,
        description: MULTI_EDIT_DESCRIPTION,
        parameters: multi_edit_params,
    },
    WorkspaceToolSpec {
        name: APPLY_PATCH,
        read_only: false,
        description: APPLY_PATCH_DESCRIPTION,
        parameters: patch_param,
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
        name: "run_tests",
        read_only: false,
        description: "Run the project's tests (or its build check) under the OS sandbox and get back the counts and the failing tests with file, line and a short message, not the whole log (output_id pages it with read_output). Without command it runs the detected test command for cwd (the environment's Project line); a given command must be one test runner invocation (cargo, go, vitest, jest, pytest, mvn, gradle, or the package manager's test script), and its structured output is switched on. A build that fails answers errors (the compiler's messages) instead of failures. The network is off unless network is true, which the person confirms.",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "command": { "type": "string", "description": "Optional. Defaults to the detected test command; a given command must be a test runner invocation." },
                    "filter": { "type": "string", "description": "Optional test name filter, appended the runner's way (cargo: positional; go: -run; pytest: -k; vitest/jest: -t)." },
                    "cwd": path_param("Working directory relative to the workspace root (default: the detected project's folder)."),
                    "timeout_seconds": { "type": "integer", "minimum": 1, "maximum": 1800 },
                    "network": { "type": "boolean", "description": "Allow the runner to fetch dependencies. Asked." }
                },
                "required": []
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

/// `apply_patch` as [`EditProfile::Patch`] offers it: the same name and
/// schema, described as the Codex envelope.
pub static APPLY_PATCH_ENVELOPE: WorkspaceToolSpec = WorkspaceToolSpec {
    name: APPLY_PATCH,
    read_only: false,
    description: APPLY_PATCH_ENVELOPE_DESCRIPTION,
    parameters: patch_param,
};

/// The tools a turn with `profile` is offered, in model order: the same
/// selection as `elitea-local-tools`' `session::tools_for`.
#[must_use]
pub fn workspace_tools_for(profile: EditProfile) -> Vec<&'static WorkspaceToolSpec> {
    profile.select_specs(WORKSPACE_TOOLS, |spec| spec.name, &APPLY_PATCH_ENVELOPE)
}

/// Runs one tool call in the sandbox's workspace runner.
#[async_trait]
pub trait WorkspaceRunner: Send + Sync {
    /// The JSON result the model reads. A tool failure is a result
    /// (`status: error`), not an `Err`; `Err` is the runner itself failing.
    async fn call(&self, tool: &str, call_id: &str, args: Value) -> Result<Value, HostError>;

    /// As [`Self::call`], for a call a person approved through the server's
    /// HITL interrupt (ADR-0034 decision 9): the runner runs it although its
    /// rules would ask. The default ignores the approval.
    async fn call_approved(
        &self,
        tool: &str,
        call_id: &str,
        args: Value,
        approved: bool,
    ) -> Result<Value, HostError> {
        let _ = approved;
        self.call(tool, call_id, args).await
    }
}

/// One approved call, as the ledger sees it.
pub struct ApprovedCall<'a> {
    pub tool: &'a str,
    pub call_id: &'a str,
    pub args: &'a Value,
    /// The invocation the call runs in (for the ledger's records).
    pub invocation_id: &'a str,
}

/// What the ledger knows about one approval.
#[derive(Clone, Debug, PartialEq)]
pub enum ApprovedCallClaim {
    /// First use: the approval is now consumed. Run the call, then
    /// [`ApprovedCallLedger::complete`] with this key.
    Run { key: String },
    /// An earlier attempt ran the call; this is its result.
    Completed(Value),
    /// An earlier attempt consumed the approval and recorded no result (it
    /// stopped while the call ran). The call is not run again.
    Started,
}

/// Makes an approved call one-shot (ADR-0034 decision 9): the approval of a
/// call is consumed durably before the call runs, and its result is
/// recorded durably as soon as it returns. A retried resume with the same
/// approval then reads the earlier result instead of running the call (a
/// `git push`, a deploy) a second time.
#[async_trait]
pub trait ApprovedCallLedger: Send + Sync {
    /// Consume the approval of `call`. Only an `Ok(Run)` lets the call run;
    /// the claim is durable before it returns.
    async fn claim(&self, call: ApprovedCall<'_>) -> Result<ApprovedCallClaim, HostError>;

    /// Record the result of a claimed call.
    async fn complete(&self, key: &str, result: &Value) -> Result<(), HostError>;
}

/// The result the model reads when an earlier attempt consumed the approval
/// and its outcome is unknown.
fn approved_call_started_result() -> Value {
    json!({
        "status": "error",
        "code": "workspace.approved_call_already_started",
        "message": "An earlier attempt of this turn already started this approved call, and its result was not recorded. The call was not run again. Check its effect (for example with git status or git log) before you ask to run it again.",
    })
}

fn host_error_result(error: &HostError) -> Value {
    json!({
        "status": "error",
        "code": error.reason_code(),
        "message": error.to_string(),
    })
}

/// Whether the run this call belongs to carries an APPROVE decision for
/// exactly this call id (the direct HITL resume of an interrupted call). The
/// decision is the run's, set by the host from the person's answer, never a
/// model argument; ADK has already matched it to the call's argument
/// fingerprint before the tool runs.
fn approved_call(ctx: &dyn ToolContext) -> bool {
    ctx.run_config().is_some_and(|config| {
        config
            .tool_confirmation_decisions
            .get(ctx.function_call_id())
            == Some(&adk_core::ToolConfirmationDecision::Approve)
    })
}

/// One workspace tool bound to the runner.
pub struct WorkspaceTool {
    spec: &'static WorkspaceToolSpec,
    runner: Arc<dyn WorkspaceRunner>,
    approved_calls: Option<Arc<dyn ApprovedCallLedger>>,
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
        let approved = approved_call(ctx.as_ref());
        // With a ledger, an approved call is one-shot: its approval is
        // consumed before it runs, and its result is recorded before anything
        // else of the turn can fail.
        let ledger = self.approved_calls.as_ref().filter(|_| approved);
        let key = match ledger {
            None => None,
            Some(ledger) => match ledger
                .claim(ApprovedCall {
                    tool: self.spec.name,
                    call_id: ctx.function_call_id(),
                    args: &args,
                    invocation_id: ctx.invocation_id(),
                })
                .await
            {
                Ok(ApprovedCallClaim::Run { key }) => Some(key),
                Ok(ApprovedCallClaim::Completed(result)) => return Ok(result),
                Ok(ApprovedCallClaim::Started) => return Ok(approved_call_started_result()),
                // Not consumed, so not run.
                Err(error) => return Ok(host_error_result(&error)),
            },
        };
        // Failures are results the model reads, not run failures.
        let result = match self
            .runner
            .call_approved(self.spec.name, ctx.function_call_id(), args, approved)
            .await
        {
            Ok(value) => value,
            Err(error) => host_error_result(&error),
        };
        if let (Some(ledger), Some(key)) = (ledger, key)
            && let Err(error) = ledger.complete(&key, &result).await
        {
            // The claim stays: a retry reports the call as started and does
            // not run it again. This attempt still returns what it ran.
            tracing::warn!(
                tool.name = self.spec.name,
                error.code = error.reason_code(),
                "the approved workspace call ran, but its result was not recorded"
            );
        }
        Ok(result)
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
    edit_profile: EditProfile,
    approved_calls: Option<Arc<dyn ApprovedCallLedger>>,
}

impl WorkspaceToolProvider {
    /// `shell`: the project's effective policy (decision 11); without it
    /// `run_command` and `run_tests` are not offered.
    #[must_use]
    pub fn new(runner: Arc<dyn WorkspaceRunner>, shell: bool) -> Self {
        Self {
            runner,
            shell,
            edit_profile: EditProfile::default(),
            approved_calls: None,
        }
    }

    /// Offer the edit tools that suit the turn's model
    /// ([`EditProfile`]); the default is [`EditProfile::StrReplace`].
    #[must_use]
    pub fn with_edit_profile(mut self, profile: EditProfile) -> Self {
        self.edit_profile = profile;
        self
    }

    /// Make every approved call one-shot through `ledger`
    /// ([`ApprovedCallLedger`]).
    #[must_use]
    pub fn with_approved_calls(mut self, ledger: Arc<dyn ApprovedCallLedger>) -> Self {
        self.approved_calls = Some(ledger);
        self
    }

    /// The tools offered, in model order.
    #[must_use]
    pub fn specs(&self) -> Vec<&'static WorkspaceToolSpec> {
        workspace_tools_for(self.edit_profile)
            .into_iter()
            .filter(|spec| self.shell || !RUNS_COMMANDS.contains(&spec.name))
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
                        approved_calls: self.approved_calls.clone(),
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
        assert!(names(true).contains(&"run_tests"));
        assert!(!names(false).contains(&"run_tests"));
        assert_eq!(names(true).len(), 16);
        let patch = WorkspaceToolProvider::new(runner.clone(), true)
            .with_edit_profile(EditProfile::Patch)
            .specs();
        let patch_names: Vec<_> = patch.iter().map(|spec| spec.name).collect();
        assert!(!patch_names.contains(&"edit_file") && !patch_names.contains(&"multi_edit"));
        assert!(patch_names.contains(&"run_command") && patch_names.contains(&"run_tests"));
        let patch_no_shell: Vec<_> = WorkspaceToolProvider::new(runner.clone(), false)
            .with_edit_profile(EditProfile::Patch)
            .specs()
            .iter()
            .map(|spec| spec.name)
            .collect();
        assert!(
            !patch_no_shell.contains(&"run_command") && !patch_no_shell.contains(&"run_tests"),
            "{patch_no_shell:?}"
        );
        let apply = patch
            .iter()
            .find(|spec| spec.name == "apply_patch")
            .expect("apply_patch");
        assert!(apply.description.contains("*** Begin Patch"));
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
                        approved_calls: None,
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
