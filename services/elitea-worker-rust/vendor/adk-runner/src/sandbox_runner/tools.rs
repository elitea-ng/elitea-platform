//! Sandbox tool implementations for shell and filesystem capabilities.
//!
//! This module provides the tool structs that are bound to the agent when
//! sandbox capabilities are enabled:
//!
//! - [`ExecCommandTool`] — executes shell commands in the sandbox workspace
//! - [`ReadFileTool`] — reads files from the sandbox workspace
//! - [`WriteFileTool`] — writes files to the sandbox workspace
//! - [`ListDirTool`] — lists directory entries in the sandbox workspace
//! - [`ApplyPatchTool`] — applies unified diff patches to the sandbox workspace
//!
//! All tools return errors as JSON in the response (not as Rust `Result::Err`)
//! so the LLM can reason about failures and self-correct.

use adk_core::Tool;
use adk_sandbox::workspace::SandboxSession;
use async_trait::async_trait;
use serde_json::Value;
use std::sync::Arc;
use std::time::Duration;

/// Tool that executes shell commands in the sandbox workspace.
///
/// Bound to the agent when [`Capability::Shell`](adk_sandbox::workspace::Capability::Shell)
/// is enabled. Returns JSON with `stdout`, `stderr`, `exit_code`, and `timed_out` fields.
pub struct ExecCommandTool {
    /// The live sandbox session to execute commands against.
    pub(crate) session: Arc<dyn SandboxSession>,
    /// Maximum duration for command execution.
    pub(crate) timeout: Duration,
}

impl ExecCommandTool {
    /// Creates a new `ExecCommandTool` bound to the given session.
    pub fn new(session: Arc<dyn SandboxSession>, timeout: Duration) -> Self {
        Self { session, timeout }
    }
}

#[async_trait]
impl Tool for ExecCommandTool {
    fn name(&self) -> &str {
        "exec_command"
    }

    fn description(&self) -> &str {
        "Execute a shell command in the sandbox workspace"
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(serde_json::json!({
            "type": "object",
            "properties": {
                "command": {
                    "type": "string",
                    "description": "Shell command to execute"
                },
                "working_dir": {
                    "type": "string",
                    "description": "Optional working directory relative to workspace root"
                }
            },
            "required": ["command"]
        }))
    }

    async fn execute(
        &self,
        _ctx: Arc<dyn adk_core::ToolContext>,
        args: Value,
    ) -> adk_core::Result<Value> {
        let command = match args.get("command").and_then(|v| v.as_str()) {
            Some(cmd) => cmd,
            None => {
                return Ok(serde_json::json!({
                    "error": "invalid_arguments",
                    "message": "Missing required parameter 'command'"
                }));
            }
        };

        let working_dir = args.get("working_dir").and_then(|v| v.as_str());

        // Use tokio timeout to enforce command timeout
        let result =
            tokio::time::timeout(self.timeout, self.session.exec_command(command, working_dir))
                .await;

        match result {
            Ok(Ok(output)) => Ok(serde_json::json!({
                "stdout": output.stdout,
                "stderr": output.stderr,
                "exit_code": output.exit_code,
                "timed_out": output.timed_out
            })),
            Ok(Err(e)) => {
                // Return error as JSON for LLM self-correction
                let error_type = match &e {
                    adk_sandbox::SandboxError::PathTraversal { .. } => "path_traversal",
                    _ => "execution_error",
                };
                Ok(serde_json::json!({
                    "error": error_type,
                    "message": e.to_string()
                }))
            }
            Err(_elapsed) => {
                // Timeout — return as JSON, not as Rust error
                Ok(serde_json::json!({
                    "stdout": "",
                    "stderr": "",
                    "exit_code": -1,
                    "timed_out": true
                }))
            }
        }
    }
}

/// Tool that reads files from the sandbox workspace.
///
/// Bound to the agent when [`Capability::Filesystem`](adk_sandbox::workspace::Capability::Filesystem)
/// is enabled.
pub struct ReadFileTool {
    /// The live sandbox session to read files from.
    pub(crate) session: Arc<dyn SandboxSession>,
}

impl ReadFileTool {
    /// Creates a new `ReadFileTool` bound to the given session.
    pub fn new(session: Arc<dyn SandboxSession>) -> Self {
        Self { session }
    }
}

#[async_trait]
impl Tool for ReadFileTool {
    fn name(&self) -> &str {
        "read_file"
    }

    fn description(&self) -> &str {
        "Read a file from the sandbox workspace"
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(serde_json::json!({
            "type": "object",
            "properties": {
                "path": {
                    "type": "string",
                    "description": "File path relative to workspace root"
                }
            },
            "required": ["path"]
        }))
    }

    async fn execute(
        &self,
        _ctx: Arc<dyn adk_core::ToolContext>,
        args: Value,
    ) -> adk_core::Result<Value> {
        let path = match args.get("path").and_then(|v| v.as_str()) {
            Some(p) => p,
            None => {
                return Ok(serde_json::json!({
                    "error": "invalid_arguments",
                    "message": "Missing required parameter 'path'"
                }));
            }
        };

        match self.session.read_file(path).await {
            Ok(content) => {
                // Return file contents as a string
                let text = String::from_utf8_lossy(&content);
                Ok(serde_json::json!({
                    "content": text
                }))
            }
            Err(e) => {
                let error_type = match &e {
                    adk_sandbox::SandboxError::PathTraversal { .. } => "path_traversal",
                    adk_sandbox::SandboxError::ExecutionFailed(msg)
                        if msg.contains("not found")
                            || msg.contains("No such file")
                            || msg.contains("does not exist") =>
                    {
                        "not_found"
                    }
                    _ => "read_error",
                };
                Ok(serde_json::json!({
                    "error": error_type,
                    "message": e.to_string(),
                    "path": path
                }))
            }
        }
    }
}

/// Tool that writes files to the sandbox workspace.
///
/// Bound to the agent when [`Capability::Filesystem`](adk_sandbox::workspace::Capability::Filesystem)
/// is enabled.
pub struct WriteFileTool {
    /// The live sandbox session to write files to.
    pub(crate) session: Arc<dyn SandboxSession>,
}

impl WriteFileTool {
    /// Creates a new `WriteFileTool` bound to the given session.
    pub fn new(session: Arc<dyn SandboxSession>) -> Self {
        Self { session }
    }
}

#[async_trait]
impl Tool for WriteFileTool {
    fn name(&self) -> &str {
        "write_file"
    }

    fn description(&self) -> &str {
        "Write content to a file in the sandbox workspace"
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(serde_json::json!({
            "type": "object",
            "properties": {
                "path": {
                    "type": "string",
                    "description": "File path relative to workspace root"
                },
                "content": {
                    "type": "string",
                    "description": "Content to write to the file"
                }
            },
            "required": ["path", "content"]
        }))
    }

    async fn execute(
        &self,
        _ctx: Arc<dyn adk_core::ToolContext>,
        args: Value,
    ) -> adk_core::Result<Value> {
        let path = match args.get("path").and_then(|v| v.as_str()) {
            Some(p) => p,
            None => {
                return Ok(serde_json::json!({
                    "error": "invalid_arguments",
                    "message": "Missing required parameter 'path'"
                }));
            }
        };

        let content = match args.get("content").and_then(|v| v.as_str()) {
            Some(c) => c,
            None => {
                return Ok(serde_json::json!({
                    "error": "invalid_arguments",
                    "message": "Missing required parameter 'content'"
                }));
            }
        };

        match self.session.write_file(path, content.as_bytes()).await {
            Ok(()) => Ok(serde_json::json!({
                "success": true,
                "path": path,
                "message": format!("Successfully wrote {} bytes to '{path}'", content.len())
            })),
            Err(e) => {
                let error_type = match &e {
                    adk_sandbox::SandboxError::PathTraversal { .. } => "path_traversal",
                    _ => "write_error",
                };
                Ok(serde_json::json!({
                    "error": error_type,
                    "message": e.to_string(),
                    "path": path
                }))
            }
        }
    }
}

/// Tool that lists directory entries in the sandbox workspace.
///
/// Bound to the agent when [`Capability::Filesystem`](adk_sandbox::workspace::Capability::Filesystem)
/// is enabled.
pub struct ListDirTool {
    /// The live sandbox session to list directories from.
    pub(crate) session: Arc<dyn SandboxSession>,
}

impl ListDirTool {
    /// Creates a new `ListDirTool` bound to the given session.
    pub fn new(session: Arc<dyn SandboxSession>) -> Self {
        Self { session }
    }
}

#[async_trait]
impl Tool for ListDirTool {
    fn name(&self) -> &str {
        "list_dir"
    }

    fn description(&self) -> &str {
        "List entries in a directory within the sandbox workspace"
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(serde_json::json!({
            "type": "object",
            "properties": {
                "path": {
                    "type": "string",
                    "description": "Directory path relative to workspace root"
                }
            },
            "required": ["path"]
        }))
    }

    async fn execute(
        &self,
        _ctx: Arc<dyn adk_core::ToolContext>,
        args: Value,
    ) -> adk_core::Result<Value> {
        let path = match args.get("path").and_then(|v| v.as_str()) {
            Some(p) => p,
            None => {
                return Ok(serde_json::json!({
                    "error": "invalid_arguments",
                    "message": "Missing required parameter 'path'"
                }));
            }
        };

        match self.session.list_dir(path).await {
            Ok(entries) => {
                // Serialize entries as JSON array of {name, type}
                let entries_json: Vec<Value> = entries
                    .iter()
                    .map(|entry| {
                        serde_json::json!({
                            "name": entry.name,
                            "type": serde_json::to_value(entry.entry_type)
                                .unwrap_or(Value::String("unknown".to_string()))
                        })
                    })
                    .collect();
                Ok(Value::Array(entries_json))
            }
            Err(e) => {
                let error_type = match &e {
                    adk_sandbox::SandboxError::PathTraversal { .. } => "path_traversal",
                    _ => "list_error",
                };
                Ok(serde_json::json!({
                    "error": error_type,
                    "message": e.to_string(),
                    "path": path
                }))
            }
        }
    }
}

/// Tool that applies unified diff patches to the sandbox workspace.
///
/// Bound to the agent when [`Capability::Filesystem`](adk_sandbox::workspace::Capability::Filesystem)
/// is enabled.
pub struct ApplyPatchTool {
    /// The live sandbox session to apply patches to.
    pub(crate) session: Arc<dyn SandboxSession>,
}

impl ApplyPatchTool {
    /// Creates a new `ApplyPatchTool` bound to the given session.
    pub fn new(session: Arc<dyn SandboxSession>) -> Self {
        Self { session }
    }
}

#[async_trait]
impl Tool for ApplyPatchTool {
    fn name(&self) -> &str {
        "apply_patch"
    }

    fn description(&self) -> &str {
        "Apply a unified diff patch to the sandbox workspace"
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(serde_json::json!({
            "type": "object",
            "properties": {
                "patch": {
                    "type": "string",
                    "description": "Unified diff patch content to apply"
                }
            },
            "required": ["patch"]
        }))
    }

    async fn execute(
        &self,
        _ctx: Arc<dyn adk_core::ToolContext>,
        args: Value,
    ) -> adk_core::Result<Value> {
        let patch = match args.get("patch").and_then(|v| v.as_str()) {
            Some(p) => p,
            None => {
                return Ok(serde_json::json!({
                    "error": "invalid_arguments",
                    "message": "Missing required parameter 'patch'"
                }));
            }
        };

        match self.session.apply_patch(patch).await {
            Ok(()) => Ok(serde_json::json!({
                "success": true,
                "message": "Patch applied successfully"
            })),
            Err(e) => {
                let error_type = match &e {
                    adk_sandbox::SandboxError::PathTraversal { .. } => "path_traversal",
                    _ => "patch_error",
                };
                Ok(serde_json::json!({
                    "error": error_type,
                    "message": e.to_string()
                }))
            }
        }
    }
}
