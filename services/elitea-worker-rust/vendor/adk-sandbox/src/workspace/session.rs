//! Sandbox session trait for workspace operations.
//!
//! The [`SandboxSession`] trait provides a live session handle for
//! workspace operations including command execution, file I/O,
//! directory listing, and patch application.
//!
//! All file paths are relative to the workspace root directory.
//! Attempts to access paths outside the workspace return `PathTraversal` errors.

use async_trait::async_trait;

use super::types::{DirEntry, ExecOutput};
use crate::error::SandboxError;

/// A live sandbox session providing workspace operations.
///
/// All file paths are relative to the workspace root directory.
/// Attempts to access paths outside the workspace return `PathTraversal` errors.
///
/// # Requirements
///
/// - Implementations must be `Send + Sync` to support use across async
///   task boundaries.
///
/// # Example
///
/// ```rust,ignore
/// use adk_sandbox::workspace::SandboxSession;
///
/// async fn use_session(session: &dyn SandboxSession) {
///     let output = session.exec_command("ls -la", None).await.unwrap();
///     println!("stdout: {}", output.stdout);
///
///     session.write_file("hello.txt", b"Hello, world!").await.unwrap();
///     let content = session.read_file("hello.txt").await.unwrap();
///     assert_eq!(content, b"Hello, world!");
/// }
/// ```
#[async_trait]
pub trait SandboxSession: Send + Sync {
    /// Executes a shell command in the workspace.
    ///
    /// Returns stdout, stderr, and exit code. Does not propagate
    /// non-zero exit codes as errors — the caller inspects the
    /// `exit_code` field to determine success.
    ///
    /// # Arguments
    ///
    /// * `command` - The shell command to execute.
    /// * `working_dir` - Optional working directory relative to the
    ///   workspace root. If `None`, uses the workspace root.
    ///
    /// # Errors
    ///
    /// Returns `SandboxError::PathTraversal` if `working_dir` escapes
    /// the workspace root.
    async fn exec_command(
        &self,
        command: &str,
        working_dir: Option<&str>,
    ) -> Result<ExecOutput, SandboxError>;

    /// Reads a file from the workspace.
    ///
    /// # Arguments
    ///
    /// * `path` - File path relative to the workspace root.
    ///
    /// # Errors
    ///
    /// Returns `SandboxError::PathTraversal` if the path escapes the
    /// workspace root.
    /// Returns `SandboxError::ExecutionFailed` if the file does not exist.
    async fn read_file(&self, path: &str) -> Result<Vec<u8>, SandboxError>;

    /// Writes content to a file in the workspace, creating parent
    /// directories as needed.
    ///
    /// # Arguments
    ///
    /// * `path` - File path relative to the workspace root.
    /// * `content` - The bytes to write to the file.
    ///
    /// # Errors
    ///
    /// Returns `SandboxError::PathTraversal` if the path escapes the
    /// workspace root.
    async fn write_file(&self, path: &str, content: &[u8]) -> Result<(), SandboxError>;

    /// Lists entries in a directory within the workspace.
    ///
    /// # Arguments
    ///
    /// * `path` - Directory path relative to the workspace root.
    ///
    /// # Errors
    ///
    /// Returns `SandboxError::PathTraversal` if the path escapes the
    /// workspace root.
    /// Returns `SandboxError::ExecutionFailed` if the directory does not exist.
    async fn list_dir(&self, path: &str) -> Result<Vec<DirEntry>, SandboxError>;

    /// Applies a unified diff patch to the workspace.
    ///
    /// # Arguments
    ///
    /// * `patch` - A unified diff string to apply.
    ///
    /// # Errors
    ///
    /// Returns `SandboxError::ExecutionFailed` if the patch cannot be applied.
    async fn apply_patch(&self, patch: &str) -> Result<(), SandboxError>;
}
