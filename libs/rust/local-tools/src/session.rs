//! One desktop session in one workspace: the tools' shared state (the
//! workspace, the read ledger, the rules, the checkpoints, the shell
//! settings) and the dispatch every tool call goes through:
//!
//! 1. parse and resolve the arguments (paths confined, deny rules);
//! 2. for changes, check the read-before-write guard first, so the person
//!    is never asked about a write that would be refused;
//! 3. ask the [`ApprovalChannel`] (rules, then the person);
//! 4. before the turn's first change, take a checkpoint;
//! 5. run it, and return a JSON result, failures included, to the model.

use std::fmt::Write as _;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use elitea_agent_runtime::host::{ApprovalChannel, ApprovalOutcome, ApprovalRequest};
use serde::Deserialize;
use serde_json::{Value, json};

use crate::approvals::{
    ChoiceStore, PAYLOAD_KEY, RuleApprovals, RulesEngine, ToolCall, ToolKind, WorkspaceSettings,
};
use crate::checkpoint::{CheckpointInfo, Checkpoints, RestoreReport};
use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::files;
use crate::git::{Repo, capped, check_revision};
use crate::ledger::ReadLedger;
use crate::policy::{LocalWorkPolicy, SandboxMode};
use crate::sandbox::credential_paths;
use crate::shell::{self, CommandSpec, ShellConfig};
use crate::workspace::{Intent, Workspace, WsPath};

/// Everything a host decides when it opens a workspace.
pub struct SessionConfig {
    /// The folder the person opened.
    pub root: PathBuf,
    /// The conversation's session id (`[A-Za-z0-9_-]{1,64}`): names the
    /// checkpoint refs and directories.
    pub session_id: String,
    pub policy: LocalWorkPolicy,
    /// The person's rules for this folder, from the host's data directory.
    pub settings: WorkspaceSettings,
    pub choices: Arc<dyn ChoiceStore>,
    /// The host's UI for questions the rules leave open.
    pub prompt: Arc<dyn ApprovalChannel>,
    /// The host's data directory: copy checkpoints and the session's
    /// temporary directory live under it.
    pub data_dir: PathBuf,
    /// Command settings; `None`: defaults, with the temporary directory
    /// under `data_dir` and no Linux sandbox helper.
    pub shell: Option<ShellConfig>,
}

#[derive(Debug, Default)]
struct Turn {
    label: String,
    checkpoint: Option<u64>,
    /// Why the turn has no checkpoint, when taking one was not possible.
    skipped: Option<String>,
}

/// One session's local tools.
pub struct LocalSession {
    workspace: Workspace,
    ledger: ReadLedger,
    engine: Arc<RulesEngine>,
    approvals: Arc<dyn ApprovalChannel>,
    checkpoints: Checkpoints,
    shell: ShellConfig,
    turn: Mutex<Turn>,
}

/// A tool this crate offers: its name, kind, description and schema.
pub struct ToolSpec {
    pub name: &'static str,
    pub kind: ToolKind,
    pub description: &'static str,
    pub parameters: fn() -> Value,
}

fn path_param(description: &str) -> Value {
    json!({ "type": "string", "description": description })
}

/// The tools, in the order the model sees them.
pub const TOOLS: &[ToolSpec] = &[
    ToolSpec {
        name: "read_file",
        kind: ToolKind::ReadFile,
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
    ToolSpec {
        name: "list_tree",
        kind: ToolKind::ListTree,
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
    ToolSpec {
        name: "search_files",
        kind: ToolKind::SearchFiles,
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
    ToolSpec {
        name: "read_document",
        kind: ToolKind::ReadDocument,
        description: "Extract the text of a document in the workspace (PDF, Word, Excel, PowerPoint, e-mail, HTML, or plain text).",
        parameters: || {
            json!({
                "type": "object",
                "properties": { "path": path_param("Path relative to the workspace root.") },
                "required": ["path"]
            })
        },
    },
    ToolSpec {
        name: "write_file",
        kind: ToolKind::WriteFile,
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
    ToolSpec {
        name: "edit_file",
        kind: ToolKind::EditFile,
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
    ToolSpec {
        name: "apply_patch",
        kind: ToolKind::ApplyPatch,
        description: "Apply a unified diff (git diff format) to files in the workspace. Every hunk must match exactly; nothing is written if one does not.",
        parameters: || {
            json!({
                "type": "object",
                "properties": { "patch": { "type": "string" } },
                "required": ["patch"]
            })
        },
    },
    ToolSpec {
        name: "run_command",
        kind: ToolKind::RunCommand,
        description: "Run a command in the workspace under the OS sandbox. Plain commands run without a shell; pipes, redirects and && run under /bin/sh and are always confirmed by the person.",
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
    ToolSpec {
        name: "git_status",
        kind: ToolKind::GitRead,
        description: "Show the git status of the workspace (short format, with the branch).",
        parameters: || json!({ "type": "object", "properties": {} }),
    },
    ToolSpec {
        name: "git_diff",
        kind: ToolKind::GitRead,
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
    ToolSpec {
        name: "git_log",
        kind: ToolKind::GitRead,
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
    ToolSpec {
        name: "git_commit",
        kind: ToolKind::GitCommit,
        description: "Commit to git through the host (hooks and signing off, your global git identity). With paths, commits exactly those files as they are now; without, commits what is staged. Always confirmed by the person.",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "message": { "type": "string" },
                    "paths": { "type": "array", "items": { "type": "string" } }
                },
                "required": ["message"]
            })
        },
    },
    ToolSpec {
        name: "git_branches",
        kind: ToolKind::GitRead,
        description: "List local branches, marking the current one.",
        parameters: || json!({ "type": "object", "properties": {} }),
    },
];

#[derive(Deserialize)]
struct CommandArgs {
    command: String,
    #[serde(default)]
    cwd: Option<String>,
    #[serde(default)]
    timeout_seconds: Option<u64>,
    #[serde(default)]
    sandbox: Option<SandboxMode>,
    #[serde(default)]
    network: bool,
}

#[derive(Deserialize)]
struct CommitArgs {
    message: String,
    #[serde(default)]
    paths: Vec<String>,
}

/// The longest commit message `git_commit` takes.
const MAX_COMMIT_MESSAGE: usize = 16 * 1024;

#[derive(Default, Deserialize)]
struct GitArgs {
    #[serde(default)]
    staged: bool,
    #[serde(default)]
    revision: Option<String>,
    #[serde(default)]
    paths: Vec<String>,
    #[serde(default)]
    stat: bool,
    #[serde(default)]
    max_count: Option<u32>,
}

fn parse<T: for<'de> Deserialize<'de>>(args: Value) -> ToolResult<T> {
    serde_json::from_value(args)
        .map_err(|error| ToolError::invalid(format!("bad arguments: {error}")))
}

/// Pathspecs that keep `path_deny` matches and the person's credentials
/// (when the workspace holds them, as a dotfiles repository does) out of
/// `git diff`, case-insensitively.
fn diff_exclusions(workspace: &Workspace) -> Vec<String> {
    let mut out = Vec::new();
    let mut exclude = |glob: String| {
        out.push(format!(":(exclude,icase,glob){glob}"));
        out.push(format!(":(exclude,icase,glob){glob}/**"));
    };
    for pattern in workspace.deny_patterns() {
        let trimmed = pattern.trim().trim_start_matches("./");
        if trimmed.is_empty() {
            continue;
        }
        if trimmed.contains('/') {
            exclude(trimmed.trim_start_matches('/').to_owned());
        } else {
            exclude(format!("**/{trimmed}"));
        }
    }
    if let Some(home) = std::env::var_os("HOME").filter(|home| !home.is_empty()) {
        for credential in credential_paths(std::path::Path::new(&home)) {
            if let Ok(relative) = credential.strip_prefix(workspace.root()) {
                let relative = relative.to_string_lossy();
                let escaped: String = relative
                    .chars()
                    .flat_map(|c| {
                        let special = matches!(c, '*' | '?' | '[' | ']' | '\\');
                        let star = c == '*' && relative.ends_with('*');
                        (if special && !star {
                            vec!['\\', c]
                        } else {
                            vec![c]
                        })
                        .into_iter()
                    })
                    .collect();
                exclude(escaped);
            }
        }
    }
    out
}

/// The staged paths (`git diff --cached --name-only -z` output, relative
/// to the repository's top) that `path_deny` or the credential list
/// covers.
fn denied_staged(workspace: &Workspace, top: &Path, staged: &[u8]) -> Vec<String> {
    let credentials = std::env::var_os("HOME")
        .filter(|home| !home.is_empty())
        .map(|home| credential_paths(Path::new(&home)))
        .unwrap_or_default();
    staged
        .split(|byte| *byte == 0)
        .filter(|name| !name.is_empty())
        .map(|name| String::from_utf8_lossy(name).into_owned())
        .filter(|name| {
            let absolute = top.join(name);
            let path_deny = absolute
                .strip_prefix(workspace.root())
                .ok()
                .and_then(|relative| WsPath::from_relative(relative).ok())
                .is_some_and(|path| workspace.denied_by(&path, Intent::Read).is_some());
            path_deny
                || credentials.iter().any(|credential| {
                    let text = credential.to_string_lossy();
                    match text.strip_suffix('*') {
                        Some(prefix) => absolute.to_string_lossy().starts_with(prefix),
                        None => absolute.starts_with(credential),
                    }
                })
        })
        .collect()
}

/// After `git add`: refuse the commit, unstaging them, when denied paths
/// are staged under `selected` (what the exclusions should have kept out).
fn unstage_denied(
    workspace: &Workspace,
    repo: &Repo,
    selected: &[String],
    writes: &[PathBuf],
    protected: &[PathBuf],
) -> ToolResult<()> {
    let root = workspace.root();
    let mut words = vec![
        "diff",
        "--cached",
        "--name-only",
        "-z",
        "--no-ext-diff",
        "--no-textconv",
        "--ignore-submodules=all",
        "--",
    ];
    words.extend(selected.iter().map(String::as_str));
    let staged = repo.git(root).literal(false).run(&words)?;
    let denied = denied_staged(workspace, repo.top(), &staged);
    if denied.is_empty() {
        return Ok(());
    }
    let unstage: Vec<String> = denied
        .iter()
        .map(|path| format!(":(top,literal){path}"))
        .collect();
    let has_head = repo
        .git(root)
        .run(&["rev-parse", "-q", "--verify", "HEAD"])
        .is_ok();
    let mut words = if has_head {
        vec!["reset", "-q", "--"]
    } else {
        vec!["rm", "--cached", "-q", "--ignore-unmatch", "--"]
    };
    words.extend(unstage.iter().map(String::as_str));
    repo.git(root)
        .literal(false)
        .writes(writes)
        .protect(protected)
        .run(&words)?;
    Err(ToolError::new(
        ErrorCode::Denied,
        format!(
            "the commit would include denied paths ({}); they were unstaged",
            denied.join(", ")
        ),
    ))
}

impl LocalSession {
    /// Open a session.
    ///
    /// # Errors
    ///
    /// The folder cannot be opened, a rule does not parse, or the session id
    /// is invalid.
    pub fn open(config: SessionConfig) -> ToolResult<Arc<Self>> {
        let workspace = Workspace::open(&config.root, &config.policy.path_deny)?;
        let engine = Arc::new(RulesEngine::new(
            config.policy,
            &workspace,
            config.settings,
            config.choices,
        )?);
        let approvals: Arc<dyn ApprovalChannel> =
            Arc::new(RuleApprovals::new(engine.clone(), config.prompt));
        let mut shell = config.shell.unwrap_or_else(|| {
            ShellConfig::new(config.data_dir.join("tmp").join(&config.session_id))
        });
        // Copy checkpoints, remembered choices and the host's state live in
        // the data directory: no command reads them (the session's temporary
        // directory inside it stays usable).
        shell.deny_read.push(config.data_dir.clone());
        let checkpoints = Checkpoints::open_with(
            &workspace,
            &config.session_id,
            &config.data_dir,
            &shell.sandbox,
        )?
        .with_git_global_config(shell.git_global_config.clone());
        Ok(Arc::new(Self {
            workspace,
            ledger: ReadLedger::new(),
            engine,
            approvals,
            checkpoints,
            shell,
            turn: Mutex::new(Turn::default()),
        }))
    }

    #[must_use]
    pub fn workspace(&self) -> &Workspace {
        &self.workspace
    }

    /// The approval channel (rules in front of the host's prompt): what the
    /// host puts in the runtime's `Host::approvals`, so HITL nodes and local
    /// tools share one channel.
    #[must_use]
    pub fn approvals(&self) -> Arc<dyn ApprovalChannel> {
        self.approvals.clone()
    }

    #[must_use]
    pub fn rules(&self) -> &Arc<RulesEngine> {
        &self.engine
    }

    /// Enter or leave plan mode: read-only tools only.
    pub fn set_plan_mode(&self, on: bool) {
        self.engine.set_plan_mode(on);
    }

    /// Start a turn: its first change takes a checkpoint labelled `label`.
    pub fn begin_turn(&self, label: &str) {
        if let Ok(mut turn) = self.turn.lock() {
            *turn = Turn {
                label: label.to_owned(),
                checkpoint: None,
                skipped: None,
            };
        }
    }

    /// The checkpoint taken for the current turn, if it has changed
    /// anything.
    #[must_use]
    pub fn turn_checkpoint(&self) -> Option<u64> {
        self.turn.lock().ok().and_then(|turn| turn.checkpoint)
    }

    #[must_use]
    pub fn checkpoints(&self) -> &Checkpoints {
        &self.checkpoints
    }

    /// Undo back to checkpoint `seq`. Every read stamp is dropped: the agent
    /// must read files again before changing them.
    ///
    /// # Errors
    ///
    /// See [`Checkpoints::restore`].
    pub fn restore_checkpoint(&self, seq: u64) -> ToolResult<RestoreReport> {
        let report = self.checkpoints.restore(seq)?;
        self.ledger.clear();
        Ok(report)
    }

    /// Undo one file back to checkpoint `seq`.
    ///
    /// # Errors
    ///
    /// See [`Checkpoints::restore_file`].
    pub fn restore_file(&self, seq: u64, path: &str) -> ToolResult<RestoreReport> {
        let path = self.workspace.resolve(path, Intent::Read)?;
        let report = self.checkpoints.restore_file(seq, &path)?;
        self.ledger.forget(&path);
        Ok(report)
    }

    /// Every checkpoint of this session.
    ///
    /// # Errors
    ///
    /// See [`Checkpoints::list`].
    pub fn list_checkpoints(&self) -> ToolResult<Vec<CheckpointInfo>> {
        self.checkpoints.list()
    }

    /// The tools the model is offered now: none when local work is off, no
    /// shell when the policy says so, no git tools outside git, and only
    /// read-only tools in plan mode.
    #[must_use]
    pub fn available_tools(&self) -> Vec<&'static ToolSpec> {
        let policy = self.engine.policy();
        if !policy.allowed {
            return Vec::new();
        }
        // An unsafe repository keeps its git tools, which answer with the
        // refusal, so the agent and the person see why.
        let git = self.checkpoints.kind() == "git" || self.checkpoints.git_refusal().is_some();
        let plan = self.engine.plan_mode();
        TOOLS
            .iter()
            .filter(|tool| tool.kind != ToolKind::RunCommand || policy.shell)
            .filter(|tool| !matches!(tool.kind, ToolKind::GitRead | ToolKind::GitCommit) || git)
            .filter(|tool| !plan || tool.kind.is_read_only())
            .collect()
    }

    /// Run one tool call; the JSON result the model sees.
    pub async fn call(self: &Arc<Self>, tool: &str, call_id: &str, args: Value) -> Value {
        match self.dispatch(tool, call_id, args).await {
            Ok(mut value) => {
                if let Value::Object(map) = &mut value {
                    map.insert("status".to_owned(), Value::String("ok".to_owned()));
                }
                value
            }
            Err(error) => error.to_result(),
        }
    }

    async fn blocking<T, F>(self: &Arc<Self>, work: F) -> ToolResult<T>
    where
        T: Send + 'static,
        F: FnOnce(&Self) -> ToolResult<T> + Send + 'static,
    {
        let this = Arc::clone(self);
        tokio::task::spawn_blocking(move || work(&this))
            .await
            .map_err(|_| ToolError::new(ErrorCode::Io, "the tool stopped unexpectedly"))?
    }

    async fn dispatch(
        self: &Arc<Self>,
        tool: &str,
        call_id: &str,
        args: Value,
    ) -> ToolResult<Value> {
        let spec = self
            .available_tools()
            .into_iter()
            .find(|spec| spec.name == tool)
            .ok_or_else(|| {
                ToolError::new(ErrorCode::Denied, format!("`{tool}` is not available"))
            })?;
        match spec.kind {
            ToolKind::ReadFile
            | ToolKind::ListTree
            | ToolKind::SearchFiles
            | ToolKind::ReadDocument => self.read_only(spec, call_id, args).await,
            ToolKind::GitRead => self.git_read(tool, call_id, args).await,
            ToolKind::GitCommit => self.git_commit(call_id, args).await,
            ToolKind::WriteFile => {
                let mut args: files::WriteArgs = parse(args)?;
                let (typed, target) = self.write_target(&args.path)?;
                self.blocking({
                    let target = target.clone();
                    move |this| {
                        files::check_fresh(&this.workspace, &this.ledger, &target).map(|_| ())
                    }
                })
                .await?;
                self.authorize_change(
                    call_id,
                    ToolKind::WriteFile,
                    &typed,
                    &target,
                    "write a file",
                )
                .await?;
                args.path = target.display_string();
                self.blocking(move |this| {
                    this.unchanged_target(&target)?;
                    files::write(&this.workspace, &this.ledger, &args)
                })
                .await
            }
            ToolKind::EditFile => {
                let mut args: files::EditArgs = parse(args)?;
                let (typed, target) = self.write_target(&args.path)?;
                self.blocking({
                    let target = target.clone();
                    move |this| {
                        files::check_fresh(&this.workspace, &this.ledger, &target).map(|_| ())
                    }
                })
                .await?;
                self.authorize_change(call_id, ToolKind::EditFile, &typed, &target, "edit a file")
                    .await?;
                args.path = target.display_string();
                self.blocking(move |this| {
                    this.unchanged_target(&target)?;
                    files::edit(&this.workspace, &this.ledger, &args)
                })
                .await
            }
            ToolKind::ApplyPatch => {
                let patch = args
                    .get("patch")
                    .and_then(Value::as_str)
                    .ok_or_else(|| ToolError::invalid("`patch` is required"))?
                    .to_owned();
                let planned = self
                    .blocking(move |this| files::plan_patch(&this.workspace, &this.ledger, &patch))
                    .await?;
                let typed: Vec<WsPath> =
                    planned.iter().map(|change| change.typed.clone()).collect();
                let targets: Vec<WsPath> =
                    planned.iter().map(|change| change.path.clone()).collect();
                self.authorize_changes(
                    call_id,
                    ToolKind::ApplyPatch,
                    &typed,
                    &targets,
                    "apply a patch",
                )
                .await?;
                self.blocking(move |this| {
                    for change in &planned {
                        this.unchanged_target(&change.path)?;
                    }
                    files::apply_patch(&this.workspace, &this.ledger, &planned)
                })
                .await
            }
            ToolKind::RunCommand => self.run_command(call_id, args).await,
        }
    }

    async fn read_only(
        self: &Arc<Self>,
        spec: &ToolSpec,
        call_id: &str,
        args: Value,
    ) -> ToolResult<Value> {
        let path = args.get("path").and_then(Value::as_str).unwrap_or(".");
        let resolved = self.workspace.resolve(path, Intent::Read)?;
        let mut call = ToolCall::new(spec.kind);
        call.paths = vec![resolved.display_string()];
        self.authorize(call_id, call, spec.name).await?;
        let kind = spec.kind;
        self.blocking(move |this| match kind {
            ToolKind::ReadFile => files::read(&this.workspace, &this.ledger, args),
            ToolKind::ListTree => files::tree(&this.workspace, args),
            ToolKind::SearchFiles => files::search(&this.workspace, args),
            _ => files::document(&this.workspace, args),
        })
        .await
    }

    /// Ask the approval channel about `call`.
    async fn authorize(&self, call_id: &str, call: ToolCall, message: &str) -> ToolResult<()> {
        let request = ApprovalRequest {
            subject: call_id.to_owned(),
            message: message.to_owned(),
            available_actions: vec!["approve".to_owned(), "reject".to_owned()],
            payload: json!({ PAYLOAD_KEY: call }),
        };
        let outcome = self
            .approvals
            .request(request)
            .await
            .map_err(|error| ToolError::new(ErrorCode::ApprovalPending, error.to_string()))?;
        match outcome {
            ApprovalOutcome::Decided { action, .. } if action == "approve" => Ok(()),
            ApprovalOutcome::Decided { value, .. } => {
                let reason = value
                    .get("reason")
                    .and_then(Value::as_str)
                    .unwrap_or("the person rejected it");
                Err(ToolError::new(
                    ErrorCode::Rejected,
                    format!("not allowed: {reason}"),
                ))
            }
            ApprovalOutcome::Deferred => Err(ToolError::new(
                ErrorCode::ApprovalPending,
                "waiting for the person's approval",
            )),
        }
    }

    /// The typed path of a write and where it lands (in-workspace symlinks
    /// followed), both checked against the deny rules.
    fn write_target(&self, argument: &str) -> ToolResult<(WsPath, WsPath)> {
        let typed = self.workspace.resolve(argument, Intent::Write)?;
        let target = self.workspace.final_target(&typed)?;
        self.workspace.check(&target, Intent::Write)?;
        Ok((typed, target))
    }

    /// The approved target is still where the write lands (a symlink was
    /// not swapped in while the person decided).
    fn unchanged_target(&self, target: &WsPath) -> ToolResult<()> {
        if &self.workspace.final_target(target)? == target {
            Ok(())
        } else {
            Err(ToolError::new(
                ErrorCode::Conflict,
                format!("`{target}` changed while the approval was pending"),
            ))
        }
    }

    async fn authorize_change(
        self: &Arc<Self>,
        call_id: &str,
        kind: ToolKind,
        typed: &WsPath,
        target: &WsPath,
        message: &str,
    ) -> ToolResult<()> {
        self.authorize_changes(
            call_id,
            kind,
            std::slice::from_ref(typed),
            std::slice::from_ref(target),
            message,
        )
        .await
    }

    /// Ask about a change. The rules match the final targets (and the typed
    /// paths, so a deny on either holds and an allow must cover both); the
    /// message names every target reached through a link.
    async fn authorize_changes(
        self: &Arc<Self>,
        call_id: &str,
        kind: ToolKind,
        typed: &[WsPath],
        targets: &[WsPath],
        message: &str,
    ) -> ToolResult<()> {
        let mut call = ToolCall::new(kind);
        let mut message = message.to_owned();
        for (typed, target) in typed.iter().zip(targets) {
            call.paths.push(target.display_string());
            if typed != target {
                call.paths.push(typed.display_string());
                let _ = write!(message, " (`{typed}` is a link to `{target}`)");
            }
        }
        self.authorize(call_id, call, &message).await?;
        self.blocking(Self::ensure_checkpoint).await
    }

    /// Take the turn's checkpoint before its first change. A copy
    /// checkpoint of a folder too large to copy is skipped (and reported);
    /// any other failure refuses the change.
    fn ensure_checkpoint(&self) -> ToolResult<()> {
        let mut turn = self
            .turn
            .lock()
            .map_err(|_| ToolError::new(ErrorCode::Io, "the turn state is unavailable"))?;
        if turn.checkpoint.is_some() || turn.skipped.is_some() {
            return Ok(());
        }
        let label = if turn.label.is_empty() {
            "turn".to_owned()
        } else {
            turn.label.clone()
        };
        match self.checkpoints.create(&label) {
            Ok(info) => {
                turn.checkpoint = Some(info.seq);
                Ok(())
            }
            Err(error) if error.code() == ErrorCode::TooLarge => {
                tracing::warn!(
                    reason = error.message(),
                    "local turn runs without a checkpoint"
                );
                turn.skipped = Some(error.message().to_owned());
                Ok(())
            }
            Err(error) => Err(ToolError::new(
                error.code(),
                format!(
                    "could not checkpoint before the change: {}",
                    error.message()
                ),
            )),
        }
    }

    async fn run_command(self: &Arc<Self>, call_id: &str, args: Value) -> ToolResult<Value> {
        let args: CommandArgs = parse(args)?;
        let cwd = match &args.cwd {
            Some(cwd) => self.workspace.resolve(cwd, Intent::Read)?,
            None => WsPath::root(),
        };
        let max = self.engine.policy().max_sandbox_mode;
        let mode = args.sandbox.unwrap_or(SandboxMode::WorkspaceWrite.min(max));
        let call = ToolCall {
            tool: ToolKind::RunCommand,
            paths: vec![cwd.display_string()],
            command: Some(args.command.clone()),
            sandbox: Some(mode),
            network: args.network,
        };
        self.authorize(call_id, call, "run a command").await?;
        if mode != SandboxMode::ReadOnly {
            self.blocking(Self::ensure_checkpoint).await?;
        }
        let spec = CommandSpec {
            command: args.command,
            cwd,
            timeout: args.timeout_seconds.map(Duration::from_secs),
            mode,
            network: args.network,
        };
        let output = shell::run(&self.workspace, &self.shell, &spec).await?;
        serde_json::to_value(output)
            .map_err(|_| ToolError::new(ErrorCode::Io, "cannot encode the result"))
    }

    /// `git_commit`: through the hardened host git (the repository is
    /// checked, hooks and signing are off, the process is sandboxed and
    /// may write only the git directory's data), always asked.
    async fn git_commit(self: &Arc<Self>, call_id: &str, args: Value) -> ToolResult<Value> {
        let args: CommitArgs = parse(args)?;
        let message = args.message.trim().to_owned();
        if message.is_empty() || message.len() > MAX_COMMIT_MESSAGE || message.contains('\0') {
            return Err(ToolError::invalid(
                "a commit message is 1 to 16384 characters, without NUL",
            ));
        }
        let paths = args
            .paths
            .iter()
            .map(|path| self.workspace.resolve(path, Intent::Read))
            .collect::<ToolResult<Vec<_>>>()?;
        let mut call = ToolCall::new(ToolKind::GitCommit);
        call.paths = if paths.is_empty() {
            vec![".".to_owned()]
        } else {
            paths.iter().map(WsPath::display_string).collect()
        };
        let subject = message.lines().next().unwrap_or_default().to_owned();
        let prompt = if paths.is_empty() {
            format!("commit what is staged: {subject}")
        } else {
            format!("commit {}: {subject}", call.paths.join(", "))
        };
        self.authorize(call_id, call, &prompt).await?;
        // Literal paths, minus what `path_deny` and the credential list
        // keep out (the same exclusions as `git_diff`): a directory never
        // stages a denied file.
        let selected: Vec<String> = paths
            .iter()
            .map(|path| format!(":(literal){}", path.display_string()))
            .collect();
        let mut pathspecs = selected.clone();
        if !pathspecs.is_empty() {
            pathspecs.extend(diff_exclusions(&self.workspace));
        }
        self.blocking(move |this| {
            let repo = match (this.checkpoints.repo(), this.checkpoints.git_refusal()) {
                (Some(repo), _) => repo,
                (None, Some(refusal)) => return Err(refusal.clone()),
                (None, None) => {
                    return Err(ToolError::new(ErrorCode::Git, "the folder is not in git"));
                }
            };
            let (name, email) = repo.global_identity()?;
            let (writes, protected) = repo.commit_access();
            let root = this.workspace.root();
            if !pathspecs.is_empty() {
                let mut words = vec!["add", "--"];
                words.extend(pathspecs.iter().map(String::as_str));
                repo.git(root)
                    .literal(false)
                    .writes(&writes)
                    .protect(&protected)
                    .worktree()
                    .run(&words)?;
                unstage_denied(&this.workspace, repo, &selected, &writes, &protected)?;
            }
            let (name, email) = (
                std::ffi::OsString::from(name),
                std::ffi::OsString::from(email),
            );
            let mut commit = vec![
                "commit",
                "--no-verify",
                "--no-gpg-sign",
                "--no-edit",
                "--cleanup=strip",
                "--file=-",
            ];
            if !pathspecs.is_empty() {
                commit.push("--");
                commit.extend(pathspecs.iter().map(String::as_str));
            }
            repo.git(root)
                .literal(false)
                .env("GIT_AUTHOR_NAME", &name)
                .env("GIT_AUTHOR_EMAIL", &email)
                .env("GIT_COMMITTER_NAME", &name)
                .env("GIT_COMMITTER_EMAIL", &email)
                .stdin(message.as_bytes())
                .writes(&writes)
                .protect(&protected)
                .worktree()
                .run(&commit)?;
            let id = repo.git(root).text(&["rev-parse", "HEAD"])?;
            Ok(json!({ "commit": id, "subject": subject }))
        })
        .await
    }

    async fn git_read(
        self: &Arc<Self>,
        tool: &str,
        call_id: &str,
        args: Value,
    ) -> ToolResult<Value> {
        let args: GitArgs = if args.is_null() {
            GitArgs::default()
        } else {
            parse(args)?
        };
        let paths = args
            .paths
            .iter()
            .map(|path| {
                self.workspace
                    .resolve(path, Intent::Read)
                    .map(|path| path.display_string())
            })
            .collect::<ToolResult<Vec<_>>>()?;
        if let Some(revision) = &args.revision {
            check_revision(revision)?;
        }
        let mut call = ToolCall::new(ToolKind::GitRead);
        call.paths.clone_from(&paths);
        self.authorize(call_id, call, tool).await?;
        let mut command_line: Vec<String> = match tool {
            "git_status" => vec![
                "status".into(),
                "--short".into(),
                "--branch".into(),
                "--ignore-submodules=all".into(),
            ],
            "git_branches" => vec!["branch".into(), "--list".into(), "-vv".into()],
            "git_log" => vec![
                "log".into(),
                format!("--max-count={}", args.max_count.unwrap_or(20).clamp(1, 200)),
                "--date=short".into(),
                "--format=%h %ad %an %s".into(),
                "--no-show-signature".into(),
            ],
            _ => {
                let mut diff = vec![
                    "diff".into(),
                    "--no-ext-diff".into(),
                    "--no-textconv".into(),
                    "--ignore-submodules=all".into(),
                ];
                if args.staged {
                    diff.push("--cached".into());
                }
                if args.stat {
                    diff.push("--stat".into());
                }
                diff
            }
        };
        let literal = tool != "git_diff";
        if matches!(tool, "git_log" | "git_diff") {
            command_line.extend(args.revision.clone());
            command_line.push("--".into());
            if literal {
                command_line.extend(paths);
            } else {
                // git_diff prints contents: path_deny and the credential
                // list are excluded with pathspec magic, so the paths the
                // model gave are spelled literally.
                if paths.is_empty() {
                    command_line.push(":(literal).".into());
                }
                command_line.extend(paths.iter().map(|path| format!(":(literal){path}")));
                command_line.extend(diff_exclusions(&self.workspace));
            }
        }
        let reads_worktree = matches!(tool, "git_status" | "git_diff");
        let output = self
            .blocking(move |this| {
                let repo = match (this.checkpoints.repo(), this.checkpoints.git_refusal()) {
                    (Some(repo), _) => repo,
                    (None, Some(refusal)) => return Err(refusal.clone()),
                    (None, None) => {
                        return Err(ToolError::new(ErrorCode::Git, "the folder is not in git"));
                    }
                };
                let root = this.workspace.root();
                let words: Vec<&str> = command_line.iter().map(String::as_str).collect();
                let git = repo.git(root).literal(literal);
                if reads_worktree {
                    git.worktree().run(&words)
                } else {
                    git.run(&words)
                }
            })
            .await?;
        let (text, truncated) = capped(&output);
        Ok(json!({ "output": text, "truncated": truncated }))
    }
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use super::denied_staged;
    use crate::workspace::Workspace;

    #[test]
    fn staged_paths_under_path_deny_are_found() {
        let dir = tempfile::tempdir().expect("dir");
        let top = std::fs::canonicalize(dir.path()).expect("canonical");
        std::fs::create_dir(top.join("ws")).expect("ws");
        let workspace =
            Workspace::open(&top.join("ws"), &[".env".to_owned(), "secrets".to_owned()])
                .expect("workspace");
        let staged = b"ws/a.txt\0ws/.env\0ws/deep/.env\0ws/secrets/key\0outside/.env\0";
        assert_eq!(
            denied_staged(&workspace, Path::new(&top), staged),
            ["ws/.env", "ws/deep/.env", "ws/secrets/key"]
        );
    }
}
