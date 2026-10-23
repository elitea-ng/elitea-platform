//! The person's local stdio MCP servers (#1230): the two `mcp.json` files
//! in host data and the repository proposals answered into them.
//!
//! * `<app config dir>/mcp.json`: the servers of every workspace;
//! * `<app data>/workspaces/<id>/mcp.json`: one workspace's (a server of
//!   the same name replaces the user one there), and the digests of the
//!   repository proposals the person answered.
//!
//! Both are `0600` files in `0700` folders, read without following a
//! symlink and refused when another user owns them or others can read them
//! (`elitea_local_tools::mcp::config`). Nothing is ever read from the
//! workspace folder except a proposal (`.mcp.json`, `.cursor/mcp.json`,
//! `.vscode/mcp.json`, through confined reads), which starts nothing: the
//! servers the person accepts are copied into the workspace's file.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use elitea_local_tools::error::{ErrorCode, ToolError, ToolResult};
use elitea_local_tools::mcp::config::{self, McpConfig, Scope, ServerConfig};
use elitea_local_tools::mcp::proposal::{self, Proposal};
use elitea_local_tools::workspace::Workspace;

use crate::workspaces::WorkspaceStore;

/// The app channel a workspace's unanswered proposal is sent on (IPC.md),
/// as `{kind: "mcp_proposal", …}`.
pub const EVENT_NAME: &str = "mcp://event";

/// The two files and the writes to them (one at a time: a save reads,
/// changes and writes the whole file).
pub struct McpFiles {
    config_dir: Option<PathBuf>,
    workspaces: Arc<WorkspaceStore>,
    write: Mutex<()>,
}

impl McpFiles {
    /// `config_dir` holds the user file (`None`: no user servers).
    #[must_use]
    pub fn new(config_dir: Option<PathBuf>, workspaces: Arc<WorkspaceStore>) -> Self {
        Self {
            config_dir,
            workspaces,
            write: Mutex::new(()),
        }
    }

    /// The user file, if the app has a config folder.
    #[must_use]
    pub fn user_path(&self) -> Option<PathBuf> {
        self.config_dir
            .as_ref()
            .map(|dir| dir.join(config::FILE_NAME))
    }

    /// A workspace's file.
    #[must_use]
    pub fn workspace_path(&self, workspace_id: &str) -> PathBuf {
        self.workspaces
            .data_dir(workspace_id)
            .join(config::FILE_NAME)
    }

    fn path(&self, scope: Scope, workspace_id: Option<&str>) -> ToolResult<PathBuf> {
        match (scope, workspace_id) {
            (Scope::User, _) => self.user_path().ok_or_else(|| {
                ToolError::new(ErrorCode::NotFound, "this app has no settings folder")
            }),
            (Scope::Workspace, Some(id)) => Ok(self.workspace_path(id)),
            (Scope::Workspace, None) => Err(ToolError::new(
                ErrorCode::InvalidArgument,
                "a workspace server needs its workspace_id",
            )),
        }
    }

    /// One file (absent: empty).
    ///
    /// # Errors
    ///
    /// The file is refused or does not parse.
    pub fn load(&self, scope: Scope, workspace_id: Option<&str>) -> ToolResult<McpConfig> {
        config::load(&self.path(scope, workspace_id)?)
    }

    /// The servers a turn in `workspace_id` runs with, and why a file was
    /// left out (a broken or refused file adds no server, and is said).
    #[must_use]
    pub fn merged(
        &self,
        workspace_id: &str,
    ) -> (BTreeMap<String, (Scope, ServerConfig)>, Vec<String>) {
        let mut problems = Vec::new();
        let mut read = |scope: Scope| match self.load(scope, Some(workspace_id)) {
            Ok(config) => config,
            Err(error) if error.code() == ErrorCode::NotFound => McpConfig::default(),
            Err(error) => {
                problems.push(format!(
                    "The {} MCP settings were not used: {}.",
                    scope.as_str(),
                    error.message()
                ));
                McpConfig::default()
            }
        };
        let user = read(Scope::User);
        let workspace = read(Scope::Workspace);
        (config::merged(&user, &workspace), problems)
    }

    fn change(
        &self,
        scope: Scope,
        workspace_id: Option<&str>,
        edit: impl FnOnce(&mut McpConfig) -> ToolResult<()>,
    ) -> ToolResult<()> {
        let _guard = self
            .write
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let path = self.path(scope, workspace_id)?;
        // The folder narrowed to `0700` as the app's other stores narrow it
        // (the save refuses one that others can read).
        if let Some(dir) = path.parent() {
            crate::credentials_file::create_private_dir(dir)
                .map_err(|error| ToolError::new(ErrorCode::Io, error.to_string()))?;
        }
        let mut file = config::load(&path)?;
        edit(&mut file)?;
        config::save(&path, &file)
    }

    /// Add or replace `name` in the `scope`'s file. A server saved here is
    /// the person's own (`source: "user"`).
    ///
    /// # Errors
    ///
    /// The entry breaks the schema, or the file is refused.
    pub fn save_server(
        &self,
        scope: Scope,
        workspace_id: Option<&str>,
        name: &str,
        mut server: ServerConfig,
    ) -> ToolResult<()> {
        if !config::server_name_ok(name) {
            return Err(ToolError::new(
                ErrorCode::InvalidArgument,
                format!(
                    "server name `{name}` must match [A-Za-z0-9_-]{{1,{}}}",
                    config::MAX_SERVER_NAME
                ),
            ));
        }
        "user".clone_into(&mut server.source);
        server.validate()?;
        self.change(scope, workspace_id, |file| {
            file.servers.insert(name.to_owned(), server);
            Ok(())
        })
    }

    /// Remove `name` from the `scope`'s file; whether it was there.
    ///
    /// # Errors
    ///
    /// The file is refused.
    pub fn remove_server(
        &self,
        scope: Scope,
        workspace_id: Option<&str>,
        name: &str,
    ) -> ToolResult<bool> {
        let mut removed = false;
        self.change(scope, workspace_id, |file| {
            removed = file.servers.remove(name).is_some();
            Ok(())
        })?;
        Ok(removed)
    }

    /// What the folder at `root` proposes that the person has not answered
    /// (read under `path_deny`; a workspace file that cannot be read answers
    /// nothing, so everything is proposed).
    #[must_use]
    pub fn pending_proposals(
        &self,
        workspace_id: &str,
        root: &Path,
        path_deny: &[String],
    ) -> Vec<Proposal> {
        let Ok(workspace) = Workspace::open(root, path_deny) else {
            return Vec::new();
        };
        let answered = self
            .load(Scope::Workspace, Some(workspace_id))
            .map(|file| file.answered_proposals)
            .unwrap_or_default();
        proposal::read(&workspace)
            .into_iter()
            .filter(|proposal| !answered.contains(&proposal.file_sha256))
            .collect()
    }

    /// The person's answer to the proposal of the file whose bytes hash to
    /// `file_sha256`: the servers named in `accept` are copied into the
    /// workspace's file (never over a server of the same name already
    /// there), and the proposal is not shown again. The names copied.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::Conflict`] when no file proposes that digest now (it
    /// changed since it was shown); [`ErrorCode::InvalidArgument`] for a
    /// server that cannot be accepted or whose name is taken; the file is
    /// refused.
    pub fn answer(
        &self,
        workspace_id: &str,
        root: &Path,
        path_deny: &[String],
        file_sha256: &str,
        accept: &[String],
    ) -> ToolResult<Vec<String>> {
        let workspace = Workspace::open(root, path_deny)?;
        let proposal = proposal::read(&workspace)
            .into_iter()
            .find(|proposal| proposal.file_sha256 == file_sha256)
            .ok_or_else(|| {
                ToolError::new(
                    ErrorCode::Conflict,
                    "the proposal changed since it was shown; look at it again",
                )
            })?;
        let accepted = proposal::accept(
            &proposal,
            accept,
            &elitea_local_tools::command::search_path(),
        )?;
        let names = accepted.iter().map(|(name, _)| name.clone()).collect();
        self.change(Scope::Workspace, Some(workspace_id), |file| {
            if let Some((name, _)) = accepted
                .iter()
                .find(|(name, _)| file.servers.contains_key(name))
            {
                return Err(ToolError::new(
                    ErrorCode::InvalidArgument,
                    format!("this workspace already has an MCP server `{name}`; remove it first"),
                ));
            }
            file.servers.extend(accepted);
            file.answered(file_sha256);
            Ok(())
        })?;
        Ok(names)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn files() -> (tempfile::TempDir, McpFiles) {
        let dir = tempfile::tempdir().expect("dir");
        let config = dir.path().join("config");
        let store = Arc::new(WorkspaceStore::new(dir.path().join("data")));
        (dir, McpFiles::new(Some(config), store))
    }

    #[test]
    fn saved_servers_merge_with_the_workspace_one_winning() {
        let (_dir, files) = files();
        let mut user = ServerConfig::new("/bin/cat");
        user.source = "proposal:x".to_owned();
        files
            .save_server(Scope::User, None, "fx", user)
            .expect("user");
        files
            .save_server(
                Scope::Workspace,
                Some("w1"),
                "fx",
                ServerConfig::new("/bin/echo"),
            )
            .expect("workspace");
        let (merged, problems) = files.merged("w1");
        assert!(problems.is_empty(), "{problems:?}");
        assert_eq!(merged["fx"].0, Scope::Workspace);
        assert_eq!(merged["fx"].1.command, "/bin/echo");
        let (other, _) = files.merged("w2");
        assert_eq!(other["fx"].0, Scope::User);
        assert_eq!(
            other["fx"].1.source, "user",
            "a saved server is the person's"
        );
        assert!(
            files
                .remove_server(Scope::User, None, "fx")
                .expect("remove")
        );
        assert!(files.merged("w2").0.is_empty());
        assert!(
            files
                .save_server(Scope::Workspace, None, "fx", ServerConfig::new("/bin/cat"))
                .is_err()
        );
    }

    #[test]
    fn an_answered_proposal_is_not_proposed_again_until_the_file_changes() {
        let (dir, files) = files();
        let root = dir.path().join("repo");
        std::fs::create_dir(&root).expect("repo");
        std::fs::write(
            root.join(".mcp.json"),
            br#"{"mcpServers": {"t": {"command": "/bin/cat"}}}"#,
        )
        .expect("write");
        let pending = files.pending_proposals("w1", &root, &[]);
        assert_eq!(pending.len(), 1);
        let digest = pending[0].file_sha256.clone();
        assert!(
            files
                .answer("w1", &root, &[], &"0".repeat(64), &["t".to_owned()])
                .is_err_and(|e| e.code() == ErrorCode::Conflict)
        );
        let names = files
            .answer("w1", &root, &[], &digest, &["t".to_owned()])
            .expect("answer");
        assert_eq!(names, ["t"]);
        let stored = files.load(Scope::Workspace, Some("w1")).expect("load");
        assert_eq!(stored.servers["t"].source, format!("proposal:{digest}"));
        assert!(files.pending_proposals("w1", &root, &[]).is_empty());
        std::fs::write(
            root.join(".mcp.json"),
            br#"{"mcpServers": {"t": {"command": "/bin/echo"}}}"#,
        )
        .expect("rewrite");
        assert_eq!(files.pending_proposals("w1", &root, &[]).len(), 1);
        assert_eq!(
            files
                .load(Scope::Workspace, Some("w1"))
                .expect("load")
                .servers["t"]
                .command,
            "/bin/cat",
            "an accepted entry is never updated from the file"
        );
    }
}
