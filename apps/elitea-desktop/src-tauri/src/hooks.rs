//! The person's hooks (#1237): the two `hooks.json` files in host data,
//! the repository proposals answered into them, and what a turn does with
//! them.
//!
//! * `<app config dir>/hooks.json`: the hooks of every workspace;
//! * `<app data>/workspaces/<id>/hooks.json`: one workspace's (a hook of
//!   the same id replaces the user one there, unless a repository
//!   proposal brought it), and the digests of the repository proposals
//!   the person answered.
//!
//! Both are `0600` files in `0700` folders, read without following a
//! symlink and refused when another user owns them or others can read them
//! (`elitea_local_tools::hooks::config`). Nothing is read from the
//! workspace folder except a proposal (`.elitea/hooks.json`,
//! `.claude/settings.json`, through confined reads), which runs nothing:
//! the hooks the person trusts are copied into the workspace's file, for
//! that file's digest; a changed file is proposed again.
//!
//! Every turn takes both files as they are at its start: the session runs
//! the tool hooks; this module runs `UserPromptSubmit` before the model
//! (a deny refuses the turn, annotations are added to the turn's context,
//! framed under the thread's nonce) and `Stop` when a turn that ran ends
//! (not a cancelled one, nor one `UserPromptSubmit` refused; what they
//! wrote is shown to the person in a `status` note, never to the model). Local-only: a cloud turn runs none, and the handoff note says
//! so.

use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use elitea_local_tools::error::{ErrorCode, ToolError, ToolResult};
use elitea_local_tools::hooks::config::{self, Hook, HooksConfig, Scope};
use elitea_local_tools::hooks::proposal::{self, Proposal};
use elitea_local_tools::hooks::{HookFramer, HookOutcome, RunResult};
use elitea_local_tools::session::LocalSession;
use elitea_local_tools::workspace::Workspace;

use crate::d0::events::{Phase, TurnEvents};
use crate::d0::turn::TurnError;
use crate::workspaces::WorkspaceStore;

/// The app channel a workspace's unanswered hook proposal is sent on
/// (IPC.md), as `{kind: "hooks_proposal", …}`.
pub const EVENT_NAME: &str = "hooks://event";

/// The tag a hook's text is framed in, for the model.
pub const HOOK_OUTPUT_TAG: &str = "hook_output";

/// Whether a write may replace a file that does not parse.
#[derive(Clone, Copy, Eq, PartialEq)]
enum Repair {
    Yes,
    No,
}

/// The two files and the writes to them (one at a time).
pub struct HookFiles {
    config_dir: Option<PathBuf>,
    workspaces: Arc<WorkspaceStore>,
    write: Mutex<()>,
}

impl HookFiles {
    /// `config_dir` holds the user file (`None`: no user hooks).
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
                "a workspace hook needs its workspace_id",
            )),
        }
    }

    /// One file (absent: empty).
    ///
    /// # Errors
    ///
    /// The file is refused or does not parse.
    pub fn load(&self, scope: Scope, workspace_id: Option<&str>) -> ToolResult<HooksConfig> {
        config::load(&self.path(scope, workspace_id)?)
    }

    /// The hooks a turn in `workspace_id` runs, in order, and why a file
    /// was left out (a broken or refused file adds no hook, and is said).
    #[must_use]
    pub fn merged(&self, workspace_id: &str) -> (Vec<(Scope, Hook)>, Vec<String>) {
        let mut problems = Vec::new();
        let mut read = |scope: Scope| match self.load(scope, Some(workspace_id)) {
            Ok(file) => file,
            Err(error) if error.code() == ErrorCode::NotFound => HooksConfig::default(),
            Err(error) => {
                problems.push(format!(
                    "The {} hooks were not used: {}.",
                    scope.as_str(),
                    error.message()
                ));
                HooksConfig::default()
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
        repair: Repair,
        edit: impl FnOnce(&mut HooksConfig) -> ToolResult<()>,
    ) -> ToolResult<()> {
        let _guard = self
            .write
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let path = self.path(scope, workspace_id)?;
        if let Some(dir) = path.parent() {
            crate::credentials_file::create_private_dir(dir)
                .map_err(|error| ToolError::new(ErrorCode::Io, error.to_string()))?;
        }
        let mut file = match config::load(&path) {
            // A file of ours that does not parse is replaced whole; a
            // refused one (a symlink, another user's) still refuses.
            Err(error) if repair == Repair::Yes && error.code() == ErrorCode::InvalidArgument => {
                log::warn!(
                    "replacing a hooks.json that does not parse: {}",
                    error.message()
                );
                HooksConfig::default()
            }
            loaded => loaded?,
        };
        edit(&mut file)?;
        config::save(&path, &file)
    }

    /// Replace the `scope`'s hooks with `hooks` (the settings pane's
    /// editor); the answered proposals are kept. A file that does not
    /// parse is replaced (its answered proposals are lost with it), so the
    /// pane can repair it.
    ///
    /// # Errors
    ///
    /// A hook breaks the schema, or the file is refused.
    pub fn save_hooks(
        &self,
        scope: Scope,
        workspace_id: Option<&str>,
        hooks: Vec<Hook>,
    ) -> ToolResult<()> {
        self.change(scope, workspace_id, Repair::Yes, |file| {
            file.hooks = hooks;
            file.validate()
        })
    }

    /// Turn the hook `id` on or off; whether it was there.
    ///
    /// # Errors
    ///
    /// The file is refused, or does not parse (saving the whole list
    /// repairs it).
    pub fn set_enabled(
        &self,
        scope: Scope,
        workspace_id: Option<&str>,
        id: &str,
        enabled: bool,
    ) -> ToolResult<bool> {
        let mut found = false;
        self.change(scope, workspace_id, Repair::No, |file| {
            if let Some(hook) = file.hooks.iter_mut().find(|hook| hook.id == id) {
                hook.enabled = enabled;
                found = true;
            }
            Ok(())
        })?;
        Ok(found)
    }

    /// What the folder at `root` proposes that the person has not answered.
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
        let workspace_file = self
            .load(Scope::Workspace, Some(workspace_id))
            .unwrap_or_default();
        let user = self.load(Scope::User, None).unwrap_or_default();
        proposal::read(&workspace)
            .into_iter()
            .filter(|proposal| {
                !workspace_file
                    .answered_proposals
                    .contains(&proposal.file_sha256)
            })
            .map(|mut proposal| {
                proposal::mark_taken(&mut proposal, &user, &workspace_file);
                proposal
            })
            .collect()
    }

    /// The person's answer to the proposal of the file whose bytes hash to
    /// `file_sha256`: the hooks named in `accept` are copied into the
    /// workspace's file (never over a hook with the same id there or in
    /// the person's own file), and the proposal is not shown again. The
    /// ids copied.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::Conflict`] when no file proposes that digest now;
    /// [`ErrorCode::InvalidArgument`] for a hook that cannot be accepted or
    /// whose id is taken; the file is refused.
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
        let accepted = proposal::accept(&proposal, accept)?;
        let ids = accepted.iter().map(|hook| hook.id.clone()).collect();
        // A user file that cannot be read refuses the answer: its ids are
        // what a proposal may not take.
        let user = match self.load(Scope::User, None) {
            Err(error) if error.code() == ErrorCode::NotFound => HooksConfig::default(),
            loaded => loaded?,
        };
        self.change(Scope::Workspace, Some(workspace_id), Repair::No, |file| {
            proposal::apply(file, &user, file_sha256, accepted)
        })?;
        Ok(ids)
    }
}

/// A hook's text framed for the model under the thread's nonce.
pub struct HookFrame {
    pub nonce: String,
}

impl HookFramer for HookFrame {
    fn frame(&self, hook: &str, text: &str) -> String {
        format!(
            "{}\n{}",
            crate::d0::framing::ends_only_at(HOOK_OUTPUT_TAG, &self.nonce),
            crate::d0::framing::block(HOOK_OUTPUT_TAG, &self.nonce, "hook", hook, text)
        )
    }
}

/// A turn starts: the session takes both files as they are now (a refused
/// file is said in a `status` event), then the `UserPromptSubmit` hooks
/// run over `message`. The context they add (framed), or the refusal.
///
/// # Errors
///
/// `hook_denied` when a hook refuses the message.
pub async fn before_turn(
    files: &HookFiles,
    workspace_id: &str,
    session: &Arc<LocalSession>,
    events: &TurnEvents,
    nonce: &str,
    message: &str,
) -> Result<String, TurnError> {
    let (hooks, problems) = files.merged(workspace_id);
    session.hooks().configure(hooks);
    session.hooks().set_framer(Some(Arc::new(HookFrame {
        nonce: nonce.to_owned(),
    })));
    if !problems.is_empty() {
        let message = problems.join(" ");
        log::warn!("{message}");
        events.status(Phase::Running, Some(&message));
    }
    let outcome = session.run_prompt_hooks(message).await;
    report_failures(&outcome, events);
    if let Some(denial) = &outcome.denied {
        return Err(TurnError::new(
            "hook_denied",
            format!(
                "Your hook `{}` refused this message: {}",
                denial.hook,
                elitea_agent_runtime::text::clip_bytes(&denial.reason, 1024)
            ),
        ));
    }
    let frame = HookFrame {
        nonce: nonce.to_owned(),
    };
    Ok(outcome
        .annotations
        .iter()
        .map(|(hook, text)| format!("\n\n{}", frame.frame(hook, text)))
        .collect())
}

/// The turn ended: the `Stop` hooks run; what they wrote, and their
/// failures, go to the person as `status` notes (`hook_stop`).
pub async fn after_turn(session: &Arc<LocalSession>, events: &TurnEvents) {
    let outcome = session.run_stop_hooks().await;
    report_failures(&outcome, events);
    for (hook, text) in &outcome.annotations {
        events.noted(
            Phase::Running,
            &format!("Hook `{hook}`: {text}"),
            "hook_stop",
        );
    }
}

/// A failed or timed-out hook outside a tool call is said in a `status`
/// note (`hook_failed`); one in a tool call is in its result.
fn report_failures(outcome: &HookOutcome, events: &TurnEvents) {
    for run in &outcome.runs {
        if matches!(run.result, RunResult::Failed | RunResult::TimedOut) {
            events.noted(
                Phase::Running,
                &format!(
                    "Hook `{}` failed: {}.",
                    run.id,
                    run.message.as_deref().unwrap_or("it did not finish")
                ),
                "hook_failed",
            );
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn files() -> (tempfile::TempDir, HookFiles) {
        let dir = tempfile::tempdir().expect("dir");
        let store = Arc::new(WorkspaceStore::new(dir.path().join("data")));
        let files = HookFiles::new(Some(dir.path().join("config")), store);
        (dir, files)
    }

    #[test]
    fn saved_hooks_merge_and_toggle() {
        let (_dir, files) = files();
        let hook = Hook::new(
            "done",
            elitea_local_tools::hooks::HookEvent::Stop,
            vec!["/bin/echo".to_owned()],
        );
        files
            .save_hooks(Scope::User, None, vec![hook.clone()])
            .expect("save");
        assert_eq!(files.merged("w1").0.len(), 1);
        assert!(
            files
                .set_enabled(Scope::User, None, "done", false)
                .expect("toggle")
        );
        assert!(
            files.merged("w1").0.is_empty(),
            "a disabled hook does not run"
        );
        assert!(
            files
                .save_hooks(Scope::Workspace, None, vec![hook])
                .is_err()
        );
    }

    #[test]
    fn a_broken_file_is_repaired_by_a_save_and_reported_by_a_toggle() {
        use std::os::unix::fs::PermissionsExt as _;
        let (_dir, files) = files();
        let path = files.user_path().expect("user path");
        std::fs::create_dir_all(path.parent().expect("dir")).expect("dir");
        std::fs::set_permissions(
            path.parent().expect("dir"),
            std::fs::Permissions::from_mode(0o700),
        )
        .expect("chmod dir");
        std::fs::write(&path, b"{not json").expect("write");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).expect("chmod");
        let toggled = files
            .set_enabled(Scope::User, None, "done", false)
            .expect_err("the parse error is reported");
        assert_eq!(toggled.code(), ErrorCode::InvalidArgument, "{toggled:?}");
        let hook = Hook::new(
            "done",
            elitea_local_tools::hooks::HookEvent::Stop,
            vec!["/bin/echo".to_owned()],
        );
        files
            .save_hooks(Scope::User, None, vec![hook])
            .expect("a full save replaces the broken file");
        assert_eq!(files.merged("w1").0.len(), 1);
    }

    #[test]
    fn a_proposal_cannot_replace_a_user_hook() {
        let (dir, files) = files();
        let mine = Hook::new(
            "g",
            elitea_local_tools::hooks::HookEvent::PreToolUse,
            vec!["/bin/echo".to_owned(), "mine".to_owned()],
        );
        files
            .save_hooks(Scope::User, None, vec![mine])
            .expect("save");
        let root = dir.path().join("repo");
        std::fs::create_dir_all(root.join(".elitea")).expect("repo");
        std::fs::write(
            root.join(".elitea/hooks.json"),
            br#"{"version":1,"hooks":[{"id":"g","event":"PreToolUse","command":["/bin/echo","theirs"]}]}"#,
        )
        .expect("write");
        let pending = files.pending_proposals("w1", &root, &[]);
        assert!(
            pending[0].hooks[0]
                .problem
                .as_deref()
                .is_some_and(|why| why.contains("your own hook")),
            "the card says why: {:?}",
            pending[0].hooks[0].problem
        );
        let refused = files
            .answer("w1", &root, &[], &pending[0].file_sha256, &["g".to_owned()])
            .expect_err("refused");
        assert_eq!(refused.code(), ErrorCode::InvalidArgument);
        let merged = files.merged("w1").0;
        assert_eq!(merged.len(), 1);
        assert_eq!(merged[0].1.command, ["/bin/echo", "mine"]);
    }

    #[test]
    fn an_answered_proposal_is_not_proposed_again_until_the_file_changes() {
        let (dir, files) = files();
        let root = dir.path().join("repo");
        std::fs::create_dir_all(root.join(".elitea")).expect("repo");
        std::fs::write(
            root.join(".elitea/hooks.json"),
            br#"{"version":1,"hooks":[{"id":"g","event":"Stop","command":["/bin/echo","hi"]}]}"#,
        )
        .expect("write");
        let pending = files.pending_proposals("w1", &root, &[]);
        assert_eq!(pending.len(), 1);
        assert!(
            files.merged("w1").0.is_empty(),
            "nothing runs before it is trusted"
        );
        let digest = pending[0].file_sha256.clone();
        assert!(
            files
                .answer("w1", &root, &[], &"0".repeat(64), &["g".to_owned()])
                .is_err_and(|e| e.code() == ErrorCode::Conflict)
        );
        let ids = files
            .answer("w1", &root, &[], &digest, &["g".to_owned()])
            .expect("answer");
        assert_eq!(ids, ["g"]);
        let merged = files.merged("w1").0;
        assert_eq!(merged[0].1.source, format!("proposal:{digest}"));
        assert!(files.pending_proposals("w1", &root, &[]).is_empty());
        std::fs::write(
            root.join(".elitea/hooks.json"),
            br#"{"version":1,"hooks":[{"id":"g","event":"Stop","command":["/bin/echo","bye"]}]}"#,
        )
        .expect("rewrite");
        assert_eq!(
            files.pending_proposals("w1", &root, &[]).len(),
            1,
            "a changed file asks again"
        );
        assert_eq!(
            files.merged("w1").0[0].1.command,
            ["/bin/echo", "hi"],
            "an accepted entry is never updated from the file"
        );
    }
}
