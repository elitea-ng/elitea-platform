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
//!
//! The person's `PreToolUse` hooks run before step 1, after the policy
//! ceiling and plan mode; their `PostToolUse` hooks after step 5
//! ([`crate::hooks`], `session/hooks.rs`).

use std::collections::HashMap;
use std::fmt::Write as _;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

pub use elitea_agent_runtime::edit_profile::EditProfile;
use elitea_agent_runtime::edit_profile::{
    APPLY_PATCH, APPLY_PATCH_DESCRIPTION, APPLY_PATCH_ENVELOPE_DESCRIPTION, MULTI_EDIT_DESCRIPTION,
    multi_edit_params, patch_param,
};
use elitea_agent_runtime::host::{ApprovalChannel, ApprovalOutcome, ApprovalRequest};
use serde::Deserialize;
use serde_json::{Value, json};

use crate::approvals::{
    ACCEPT, ChoiceStore, DECLINE, PAYLOAD_KEY, PLAN_PAYLOAD_KEY, PermissionProfile, RuleApprovals,
    RulesEngine, ToolCall, ToolKind, WorkspaceSettings,
};
use crate::budget::{self, Budgets, SpillStore};
use crate::checkpoint::{CheckpointInfo, Checkpoints, RestoreReport};
use crate::deny::DenyList;
use crate::document::{self, Documents, InProcessExtractor, SidecarExtractor};
use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::files;
use crate::git::{Repo, StatusSummary, capped, check_revision, parse_status_summary};
use crate::hooks::HookSet;
use crate::ledger::ReadLedger;
use crate::lsp::{self, LanguageServers};
use crate::mcp::{McpServers, ServerStatus};
use crate::policy::{LocalWorkPolicy, SandboxMode};
use crate::process::{KeepAlive, ProcessRegistry};
use crate::project::{self, ProjectProfile};
use crate::run_tests;
use crate::sandbox::{self, Enforcement, SandboxRequest};
use crate::shell::{self, CommandSpec, ShellConfig};
use crate::workspace::{Intent, Workspace, WsPath};

mod hooks;
mod processes;

pub use processes::{PROCESS_TOOLS, RUN_COMMAND_PROCESSES};

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
    /// Where the session's spilled outputs live: the host's CACHE directory
    /// (spills are ephemeral: kept for [`crate::budget::SPILL_RETENTION`],
    /// bounded, and a gone id is an expected answer). `None`: under
    /// `data_dir` (hosts without a cache directory, the library's tests).
    /// The host keeps this directory on the session's deny list, as it does
    /// its data directory.
    pub spill_dir: Option<PathBuf>,
    /// Command settings; `None`: defaults, with the temporary directory
    /// under `data_dir` and no Linux sandbox helper.
    pub shell: Option<ShellConfig>,
    /// More directories no tool may read, whatever `shell` says: the host
    /// app's own config, data, log and cache directories as the host
    /// resolved them (the authority for those). Added to
    /// [`ShellConfig::deny_read`]; a writable root inside one (the
    /// session's temporary directory) stays usable.
    pub deny_read: Vec<PathBuf>,
    /// The host app's identifier (its bundle id): its default directories
    /// under the home are denied too ([`crate::deny::app_paths`]); sets
    /// [`ShellConfig::app_id`] when `shell` names none.
    pub app_id: Option<String>,
    /// Each tool's output budget in estimated tokens
    /// ([`Budgets::default`]: 12k for reads, 6k for commands, 8k for
    /// searches and git). Output over a budget is cut; a command's, git's
    /// and a document's whole output is spilled for `read_output`.
    pub budgets: Budgets,
    /// Where `read_document` gets PDF and Office text
    /// ([`Documents::default`]: this process, which has no extractor on the
    /// desktop; the host passes the bundled sidecar).
    pub documents: Documents,
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
    budgets: Budgets,
    documents: Documents,
    /// The session's spilled outputs, per conversation (under the data
    /// directory; kept across a reopen for [`budget::SPILL_RETENTION`]).
    spill: SpillStore,
    turn: Mutex<Turn>,
    /// The edit tools the current model is offered.
    edit_profile: Mutex<EditProfile>,
    /// Whether the current model can see images: `read_file` returns an
    /// image to one that can (set per turn from the model catalogue).
    vision: AtomicBool,
    /// The project profile and the mentioned folders it was detected
    /// with: detected again only when those change or a marker does.
    project: Mutex<Option<(Vec<WsPath>, Arc<ProjectProfile>)>>,
    /// The background processes (#1223); dropped with the session, which
    /// stops them.
    processes: ProcessRegistry,
    /// The person's local MCP servers (#1230); dropped with the session,
    /// which stops them.
    mcp: McpServers,
    /// The person's hooks (#1237), configured by the host each turn.
    hooks: HookSet,
    /// The session id hooks are told.
    session_id: String,
    /// The conversation the current turn runs for (hooks are told it).
    conversation: Mutex<Option<String>>,
    /// The `PreToolUse` hooks that asked, by call id, until the call ends:
    /// its approval request carries them ([`crate::approvals::HOOK_ASK_PAYLOAD_KEY`]).
    hook_asks: Mutex<HashMap<String, Value>>,
    /// The language servers behind edit diagnostics (#1236); dropped with
    /// the session, which stops them.
    lsp: LanguageServers,
}

/// How a command would be confined (see [`LocalSession::command_confinement`]).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Confinement {
    pub enforcement: Enforcement,
    /// Whether the sandbox hides everything the session denies; when not,
    /// every such command is asked.
    pub hides_denied: bool,
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

const RUN_TESTS_DESCRIPTION: &str = "Run the project's tests (or its build check) under the OS sandbox and get back the counts and the failing tests with file, line and a short message, not the whole log (output_id pages it with read_output). Without command it runs the detected test command for cwd (the environment's Project line); a given command must be one test runner invocation (cargo, go, vitest, jest, pytest, mvn, gradle, or the package manager's test script), and its structured output is switched on. A build that fails answers errors (the compiler's messages) instead of failures. The network is off unless network is true, which the person confirms.";

fn run_tests_parameters() -> Value {
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
}

/// The tools, in the order the model sees them.
pub const TOOLS: &[ToolSpec] = &[
    ToolSpec {
        name: "read_file",
        kind: ToolKind::ReadFile,
        description: "Read a UTF-8 text file in the workspace, with line numbers. Page long files with offset/limit; a line too long for one page is cut, and next_offset with next_column continue inside it. Reading a file is required before changing it. Images (PNG, JPEG, WebP, GIF) are returned as images when the model can see them.",
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
    ToolSpec {
        name: "find_files",
        kind: ToolKind::FindFiles,
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
    ToolSpec {
        name: "read_document",
        kind: ToolKind::ReadDocument,
        description: "Extract the text of a document in the workspace (PDF, Word, Excel, PowerPoint, OpenDocument, RTF, EPUB, e-mail, HTML, or plain text), page by page: a PDF page, a slide or a sheet is one page, marked [page N]. The result says page_count and which pages it shows; pass pages (\"1-20\", \"5\", \"30-\") to read a range of a long document, and when the text is cut, next_offset (with the same pages) continues it and next_pages names the pages after it.",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "path": path_param("Path relative to the workspace root."),
                    "pages": { "type": "string", "description": "Page range, e.g. \"1-20\" or \"5\". Default: from the first page until the output budget is reached." },
                    "offset": { "type": "integer", "minimum": 0, "description": "Character offset into the selected text, for continuing a cut result." }
                },
                "required": ["path"]
            })
        },
    },
    ToolSpec {
        name: "read_output",
        kind: ToolKind::ReadOutput,
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
    ToolSpec {
        name: "multi_edit",
        kind: ToolKind::EditFile,
        description: MULTI_EDIT_DESCRIPTION,
        parameters: multi_edit_params,
    },
    ToolSpec {
        name: APPLY_PATCH,
        kind: ToolKind::ApplyPatch,
        description: APPLY_PATCH_DESCRIPTION,
        parameters: patch_param,
    },
    ToolSpec {
        name: "run_command",
        kind: ToolKind::RunCommand,
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
    ToolSpec {
        name: "run_tests",
        kind: ToolKind::RunTests,
        description: RUN_TESTS_DESCRIPTION,
        parameters: run_tests_parameters,
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
        description: "Commit to git through the host (hooks and signing off, your global git identity). With paths, commits exactly those files as they are now; without, commits what is staged. Paths the policy denies are never committed: they are left out of the paths you give, and a commit of what is staged is refused while the index holds one. Workspace or policy rules may ask the person first.",
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

/// `diagnostics` (#1236), offered after every tool of [`TOOLS`] while the
/// session's language servers are on and one serves the workspace.
/// Local-only: the cloud executor runs no language server, so it is not in
/// [`TOOLS`] and not in the workspace tool contract.
pub static DIAGNOSTICS: ToolSpec = ToolSpec {
    name: "diagnostics",
    kind: ToolKind::Diagnostics,
    description: "Current compiler/linter errors from the language server for one file, or for every file changed in this session when path is omitted.",
    parameters: || {
        json!({
            "type": "object",
            "properties": {
                "path": { "type": "string" },
                "severity": { "type": "string", "enum": ["error", "warning", "all"], "default": "error" },
                "max": { "type": "integer", "minimum": 1, "maximum": 200, "default": 50 }
            },
            "additionalProperties": false
        })
    },
};

/// `apply_patch` as [`EditProfile::Patch`] offers it: the same name, kind
/// and schema, described as the Codex envelope.
pub static APPLY_PATCH_ENVELOPE: ToolSpec = ToolSpec {
    name: APPLY_PATCH,
    kind: ToolKind::ApplyPatch,
    description: APPLY_PATCH_ENVELOPE_DESCRIPTION,
    parameters: patch_param,
};

/// The longest plan `exit_plan_mode` takes, in bytes.
pub const MAX_PLAN_BYTES: usize = 16 * 1024;

/// The longest feedback a person's decline carries to the model, in bytes.
pub const MAX_FEEDBACK_BYTES: usize = 2 * 1024;

/// `exit_plan_mode` (#1225), offered in [`PermissionProfile::Plan`] only and
/// after every tool of [`TOOLS`]. Local-only: the cloud executor has no plan
/// mode, so it is not in [`TOOLS`] and not in the workspace tool contract.
pub static EXIT_PLAN_MODE: ToolSpec = ToolSpec {
    name: "exit_plan_mode",
    kind: ToolKind::ExitPlanMode,
    description: "Present your plan to the person and ask to leave plan mode. Call it once the plan is ready (after reading what you need). If the person accepts, every tool becomes available and you carry the plan out in this same turn; if they decline, you stay in plan mode and get their feedback: revise the plan and call it again.",
    parameters: || {
        json!({
            "type": "object",
            "properties": {
                "plan": {
                    "type": "string",
                    "maxLength": MAX_PLAN_BYTES,
                    "description": "The plan in Markdown, for the person to accept or decline."
                }
            },
            "required": ["plan"]
        })
    },
};

/// The tools a turn with `profile` may be offered, in model order:
/// [`EditProfile::StrReplace`] is [`TOOLS`]; [`EditProfile::Patch`] leaves
/// out `edit_file` and `multi_edit` and describes `apply_patch` as the
/// Codex envelope. Names never change, so the cloud executor's contract
/// holds per profile (`elitea_agent_runtime::workspace_tools::workspace_tools_for`).
#[must_use]
pub fn tools_for(profile: EditProfile) -> Vec<&'static ToolSpec> {
    profile.select_specs(TOOLS, |spec| spec.name, &APPLY_PATCH_ENVELOPE)
}

#[derive(Deserialize)]
#[allow(clippy::struct_excessive_bools)] // the tool's independent switches
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
    /// The rest are [`RUN_COMMAND_PROCESSES`]' (#1223).
    #[serde(default)]
    background: bool,
    #[serde(default)]
    tty: bool,
    #[serde(default)]
    loopback: bool,
    #[serde(default)]
    yield_ms: Option<u64>,
    #[serde(default)]
    keep_alive: Option<KeepAlive>,
}

impl CommandArgs {
    /// Whether it starts a background process: asked for, or implied (a
    /// terminal program is written to later; keeping a process alive needs
    /// one).
    fn background(&self) -> bool {
        self.background || self.tty || self.keep_alive == Some(KeepAlive::Session)
    }

    /// Whether it uses any of the background-process switches.
    fn uses_processes(&self) -> bool {
        self.background() || self.loopback || self.yield_ms.is_some() || self.keep_alive.is_some()
    }
}

/// An `edit_file` or a `multi_edit` call.
enum EditCall {
    One(files::EditArgs),
    Many(files::MultiEditArgs),
}

impl EditCall {
    fn path(&self) -> &str {
        match self {
            EditCall::One(args) => &args.path,
            EditCall::Many(args) => &args.path,
        }
    }

    fn set_path(&mut self, path: String) {
        match self {
            EditCall::One(args) => args.path = path,
            EditCall::Many(args) => args.path = path,
        }
    }
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

/// Pathspecs that keep `path_deny` matches and `denied` (the credential
/// list and the app's own directories, when the workspace holds them, as
/// a dotfiles repository does) out of `git diff`, case-insensitively.
pub(crate) fn diff_exclusions(workspace: &Workspace, denied: &[PathBuf]) -> Vec<String> {
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
    for credential in denied {
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
    out
}

/// The staged paths (`git diff --cached --name-only -z` output, relative
/// to the repository's top) that `path_deny` or the session's deny list
/// covers, compared without regard to case where the file system folds it
/// (as `diff_exclusions`' `icase` pathspecs do).
fn denied_staged(
    workspace: &Workspace,
    top: &Path,
    staged: &[u8],
    denied: &DenyList,
) -> Vec<String> {
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
            path_deny || denied.covers(&absolute)
        })
        .collect()
}

/// The staged paths under `selected` (all of the index when empty) that
/// `path_deny` or `denied` covers.
fn staged_denied(
    workspace: &Workspace,
    repo: &Repo,
    selected: &[String],
    denied: &DenyList,
) -> ToolResult<Vec<String>> {
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
    Ok(denied_staged(workspace, repo.top(), &staged, denied))
}

/// Before committing what is staged: refuse, naming them, when the index
/// holds denied paths (staged outside the session, say). Nothing is
/// unstaged: the person decides what to do with their index.
fn refuse_staged_denied(workspace: &Workspace, repo: &Repo, denied: &DenyList) -> ToolResult<()> {
    let staged = staged_denied(workspace, repo, &[], denied)?;
    if staged.is_empty() {
        return Ok(());
    }
    Err(ToolError::new(
        ErrorCode::Denied,
        format!(
            "the index holds paths the policy denies ({}); nothing was committed. Unstage them \
             (git restore --staged <path>) or commit chosen paths instead",
            staged.join(", ")
        ),
    ))
}

/// After `git add`: refuse the commit, unstaging them, when denied paths
/// are staged under `selected` (what the exclusions should have kept out).
fn unstage_denied(
    workspace: &Workspace,
    repo: &Repo,
    selected: &[String],
    denied_paths: &DenyList,
    writes: &[PathBuf],
    protected: &[PathBuf],
) -> ToolResult<()> {
    let root = workspace.root();
    let denied = staged_denied(workspace, repo, selected, denied_paths)?;
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
        let mut shell = config.shell.unwrap_or_else(|| {
            ShellConfig::new(config.data_dir.join("tmp").join(&config.session_id))
        });
        // One deny list for the session, built once: the host's own
        // directories, the app's identifier directories and the
        // credentials under the home, and the data directory (copy
        // checkpoints, remembered choices; the session's temporary
        // directory inside it stays usable). Commands, host git and the
        // file tools all read it.
        // The build tools' caches outlive the session: every session over
        // this data directory shares them.
        if shell.cache_dir.is_none() {
            shell.cache_dir = Some(config.data_dir.join("build-cache"));
        }
        shell.deny_read.extend(config.deny_read);
        shell.deny_read.push(config.data_dir.clone());
        // The spill files live in the host's cache directory, not its data
        // directory: spills are ephemeral (kept for
        // [`crate::budget::SPILL_RETENTION`], bounded, and a gone id is an
        // expected answer), and the cache may be cleared without the app
        // noticing. The host keeps it on the deny list, as it does the data
        // directory.
        let spill_base = config
            .spill_dir
            .clone()
            .unwrap_or_else(|| config.data_dir.clone());
        if spill_base != config.data_dir {
            shell.deny_read.push(spill_base.clone());
        }
        // A task worktree the host registered lives in the host's data: its
        // folder (and only its own) is opened again for the file tools, as
        // the sandbox opens it again for commands (its writable root).
        if let Some(entry) = crate::git::registered_worktree(&config.root) {
            shell.reopened.push(entry.canonical().worktree);
        }
        if shell.app_id.is_none() {
            shell.app_id = config.app_id;
        }
        let deny = shell.freeze_deny_list();
        let workspace =
            Workspace::open(&config.root, &config.policy.path_deny)?.with_deny_list(deny);
        // Whether a command is confined is decided per command, from the
        // sandbox chosen for it (`run_command`).
        let engine = Arc::new(RulesEngine::new(
            config.policy,
            &workspace,
            config.settings,
            config.choices,
        )?);
        let approvals: Arc<dyn ApprovalChannel> =
            Arc::new(RuleApprovals::new(engine.clone(), config.prompt));
        let checkpoints = Checkpoints::open_with(
            &workspace,
            &config.session_id,
            &config.data_dir,
            &shell.sandbox,
        )?
        .with_git_global_config(shell.git_global_config.clone());
        // What an earlier session of this id spilled (before a policy
        // change, or an app restart) stays readable within its retention:
        // the conversations' history names those ids. Expired ones go now.
        // Under the cache directory when the host names one (spills are
        // ephemeral), else under the data directory.
        let spill = SpillStore::new(&spill_base, &config.session_id);
        spill.sweep();
        Ok(Arc::new(Self {
            workspace,
            ledger: ReadLedger::new(),
            engine,
            approvals,
            checkpoints,
            shell,
            budgets: config.budgets,
            documents: config.documents,
            spill,
            turn: Mutex::new(Turn::default()),
            edit_profile: Mutex::new(EditProfile::default()),
            vision: AtomicBool::new(false),
            project: Mutex::new(None),
            processes: ProcessRegistry::new(),
            mcp: McpServers::new(),
            hooks: HookSet::new(),
            session_id: config.session_id,
            conversation: Mutex::new(None),
            hook_asks: Mutex::new(HashMap::new()),
            lsp: LanguageServers::new(),
        }))
    }

    #[must_use]
    pub fn workspace(&self) -> &Workspace {
        &self.workspace
    }

    /// The session's spilled outputs.
    #[must_use]
    pub fn spill_store(&self) -> &SpillStore {
        &self.spill
    }

    /// The conversation the next turn runs for: the outputs its tools
    /// spill are its own, and `read_output` sees only those
    /// ([`SpillStore::set_conversation`]). Hosts set it with
    /// [`Self::begin_turn`]; `None` is the default scope.
    pub fn set_conversation(&self, conversation: Option<&str>) {
        self.spill.set_conversation(conversation);
        if let Ok(mut current) = self.conversation.lock() {
            *current = conversation.map(str::to_owned);
        }
    }

    /// The workspace's test runners and build checks
    /// ([`project::detect`]), looking also in the folders of `mentioned`
    /// (the turn's `@` references; paths the workspace refuses are left
    /// out). Cached for the session: detected again only when `mentioned`
    /// differs or a marker file changed. Reads the file system; call it
    /// off the async runtime.
    pub fn project_profile(&self, mentioned: &[String]) -> Arc<ProjectProfile> {
        let mentioned: Vec<WsPath> = mentioned
            .iter()
            .filter_map(|path| {
                self.workspace
                    .resolve(path.trim_end_matches('/'), Intent::Read)
                    .ok()
            })
            .collect();
        self.profile_for(Some(mentioned))
    }

    /// The cached profile when it still holds for `mentioned` (`None`: the
    /// folders the last detection used), else a fresh one.
    fn profile_for(&self, mentioned: Option<Vec<WsPath>>) -> Arc<ProjectProfile> {
        let Ok(mut cached) = self.project.lock() else {
            return Arc::new(project::detect(
                &self.workspace,
                &mentioned.unwrap_or_default(),
            ));
        };
        let mentioned = mentioned
            .or_else(|| cached.as_ref().map(|(dirs, _)| dirs.clone()))
            .unwrap_or_default();
        if let Some((dirs, profile)) = cached.as_ref()
            && *dirs == mentioned
            && profile.is_fresh(&self.workspace)
        {
            return Arc::clone(profile);
        }
        let profile = Arc::new(project::detect(&self.workspace, &mentioned));
        *cached = Some((mentioned, Arc::clone(&profile)));
        profile
    }

    /// The session's local MCP servers (#1230): the host configures them
    /// ([`McpServers::configure`]) and reads their status.
    #[must_use]
    pub fn mcp(&self) -> &McpServers {
        &self.mcp
    }

    /// The session's language servers (#1236): the host configures them
    /// ([`LanguageServers::configure`]) and hears their status
    /// ([`LanguageServers::set_events`]).
    #[must_use]
    pub fn lsp(&self) -> &LanguageServers {
        &self.lsp
    }

    /// Start the configured MCP servers that have not started yet (the
    /// questions asked are named after `subject`, the turn); their
    /// statuses. Nothing starts when the policy's `local_mcp` is off.
    pub async fn start_mcp_servers(self: &Arc<Self>, subject: &str) -> Vec<ServerStatus> {
        let policy = self.engine.policy();
        if !(policy.allowed && policy.local_mcp) {
            return self.mcp.status();
        }
        self.mcp.start_all(self, subject).await
    }

    /// The command settings (the MCP servers start from them).
    pub(crate) fn shell_config(&self) -> &ShellConfig {
        &self.shell
    }

    /// The tools' output budgets.
    pub(crate) fn budgets(&self) -> Budgets {
        self.budgets
    }

    /// The session's deny list (built once at open).
    #[must_use]
    pub fn deny_list(&self) -> Arc<DenyList> {
        self.shell.deny_list()
    }

    /// The sandbox request a command in `mode` would run under: what it
    /// may write, and what it cannot read.
    ///
    /// # Errors
    ///
    /// When the temporary directory cannot be created.
    pub fn sandbox_request(&self, mode: SandboxMode, network: bool) -> ToolResult<SandboxRequest> {
        shell::sandbox_request(&self.workspace, &self.shell, mode, network)
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

    /// Switch the [`PermissionProfile`]: [`PermissionProfile::Plan`] offers
    /// the read-only tools and `exit_plan_mode` only. A host sets it at each
    /// turn's start; the person accepting the plan sets `Build`.
    pub fn set_profile(&self, profile: PermissionProfile) {
        self.engine.set_profile(profile);
    }

    #[must_use]
    pub fn profile(&self) -> PermissionProfile {
        self.engine.profile()
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

    /// Why the current turn changed files without a checkpoint (the folder
    /// was too large to copy), if it did: such a turn cannot be undone.
    #[must_use]
    pub fn turn_checkpoint_skipped(&self) -> Option<String> {
        self.turn.lock().ok().and_then(|turn| turn.skipped.clone())
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
        let report = self.checkpoints.restore(seq);
        self.shell.sandbox.masks.invalidate();
        let report = report?;
        self.ledger.clear();
        Ok(report)
    }

    /// What [`Self::restore_checkpoint`] would write back and delete now,
    /// without changing anything.
    ///
    /// # Errors
    ///
    /// See [`Checkpoints::preview`].
    pub fn preview_checkpoint(&self, seq: u64) -> ToolResult<RestoreReport> {
        self.checkpoints.preview(seq)
    }

    /// Undo one file back to checkpoint `seq`.
    ///
    /// # Errors
    ///
    /// See [`Checkpoints::restore_file`].
    pub fn restore_file(&self, seq: u64, path: &str) -> ToolResult<RestoreReport> {
        let path = self.workspace.resolve(path, Intent::Read)?;
        let report = self.checkpoints.restore_file(seq, &path);
        self.shell.sandbox.masks.invalidate();
        let report = report?;
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

    /// Offer the edit tools that suit the turn's model ([`EditProfile`]);
    /// [`EditProfile::StrReplace`] until set. Set it per turn from the
    /// admitted model, like plan mode.
    pub fn set_edit_profile(&self, profile: EditProfile) {
        if let Ok(mut current) = self.edit_profile.lock() {
            *current = profile;
        }
    }

    /// Say whether the turn's model can see images; `false` until set.
    /// `read_file` on an image returns it only when it can
    /// (see the `read_file` description).
    pub fn set_vision(&self, vision: bool) {
        self.vision.store(vision, Ordering::Relaxed);
    }

    #[must_use]
    pub fn vision(&self) -> bool {
        self.vision.load(Ordering::Relaxed)
    }

    #[must_use]
    pub fn edit_profile(&self) -> EditProfile {
        self.edit_profile
            .lock()
            .map(|profile| *profile)
            .unwrap_or_default()
    }

    /// The tools the model is offered now: none when local work is off, no
    /// shell (nor background processes) when the policy says so, no git
    /// tools outside git, only read-only tools and `exit_plan_mode` in plan
    /// mode, and the edit tools of the session's [`EditProfile`]. With
    /// [`ShellConfig::background_processes`], `run_command` is
    /// [`RUN_COMMAND_PROCESSES`] and the [`PROCESS_TOOLS`] follow
    /// [`TOOLS`].
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
        let processes = self.shell.background_processes;
        let mut tools: Vec<&'static ToolSpec> = tools_for(self.edit_profile())
            .into_iter()
            // With background processes, `run_command` takes their switches
            // (a local-only schema: the cloud executor keeps the plain one).
            .map(|tool| {
                if processes && tool.name == RUN_COMMAND_PROCESSES.name {
                    &RUN_COMMAND_PROCESSES
                } else {
                    tool
                }
            })
            .chain(PROCESS_TOOLS.iter().filter(|_| processes))
            .chain(
                std::iter::once(&DIAGNOSTICS)
                    .filter(|_| self.lsp.offered(&self.workspace, policy.shell)),
            )
            .filter(|tool| !(tool.kind.runs_commands() || tool.kind.is_process()) || policy.shell)
            .filter(|tool| !matches!(tool.kind, ToolKind::GitRead | ToolKind::GitCommit) || git)
            .filter(|tool| !plan || tool.kind.plan_allows())
            .collect();
        if plan {
            tools.push(&EXIT_PLAN_MODE);
        }
        tools
    }

    /// What a command in `mode` would run under now, decided as
    /// `run_command` decides it but without running anything: for the
    /// host's note to the model. `Err` when such a command is refused (the
    /// sandbox cannot be enforced and the host does not allow that).
    ///
    /// # Errors
    ///
    /// [`ErrorCode::SandboxUnavailable`], or the temporary directory cannot
    /// be created.
    pub fn command_confinement(&self, mode: SandboxMode, network: bool) -> ToolResult<Confinement> {
        let request = self.sandbox_request(mode, network)?;
        let prepared = sandbox::prepare(&request, &["true".to_owned()], &self.shell.sandbox)?;
        Ok(Confinement {
            enforcement: prepared.enforcement,
            hides_denied: prepared.hides_denied,
        })
    }

    /// The branch and the number of changed paths, from the same hardened
    /// `git status` the `git_status` tool runs. Asked by the host for its
    /// note to the model, so no approval is asked; only those two facts
    /// leave. `None` outside git, for a repository git refuses, or when git
    /// fails.
    pub async fn git_summary(self: &Arc<Self>) -> Option<StatusSummary> {
        self.blocking(|this| {
            let Some(repo) = this.checkpoints.repo() else {
                return Err(ToolError::new(ErrorCode::Git, "the folder is not in git"));
            };
            repo.git(this.workspace.root()).worktree().run(&[
                "status",
                "--porcelain=v1",
                "--branch",
                "--ignore-submodules=all",
            ])
        })
        .await
        .ok()
        .map(|output| parse_status_summary(&output))
    }

    /// Run one tool call; the JSON result the model sees.
    pub async fn call(self: &Arc<Self>, tool: &str, call_id: &str, args: Value) -> Value {
        let (args, mut trail) = match self.pre_tool_hooks(tool, call_id, args).await {
            Ok(proceed) => proceed,
            Err(refusal) => return refusal,
        };
        // Kept only for the `PostToolUse` hooks (a write's input is large).
        let post_input = self
            .hooks
            .has(crate::hooks::HookEvent::PostToolUse)
            .then(|| args.clone());
        let result = self.dispatch(tool, call_id, args).await;
        self.forget_hook_ask(call_id);
        if !TOOLS
            .iter()
            .chain(PROCESS_TOOLS)
            .chain(std::iter::once(&DIAGNOSTICS))
            .any(|spec| spec.name == tool && spec.kind.is_read_only())
        {
            // A change: what the sandbox masks may differ now.
            self.shell.sandbox.masks.invalidate();
        }
        match result {
            Ok(mut value) => {
                // A process tool's `status` is the process's (`running`,
                // `exited`, …); every other success is `ok`.
                if let Value::Object(map) = &mut value {
                    map.entry("status")
                        .or_insert_with(|| Value::String("ok".to_owned()));
                }
                if let Some(input) = post_input
                    && self
                        .post_tool_hooks(tool, &input, &mut value, &mut trail)
                        .await
                    && matches!(
                        tool,
                        "write_file" | "edit_file" | "multi_edit" | "apply_patch"
                    )
                {
                    self.restamp_after_hooks(tool, &input, &mut value).await;
                }
                trail.attach(&mut value);
                value
            }
            Err(error) => {
                let mut value = error.to_result();
                trail.attach(&mut value);
                value
            }
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
            | ToolKind::FindFiles
            | ToolKind::ReadDocument => self.read_only(spec, call_id, args).await,
            ToolKind::ReadOutput => self.read_output(call_id, args).await,
            ToolKind::GitRead => self.git_read(tool, call_id, args).await,
            ToolKind::GitCommit => self.git_commit(call_id, args).await,
            ToolKind::WriteFile => self.write_file(call_id, args).await,
            ToolKind::EditFile => self.edit_file(spec, call_id, args).await,
            ToolKind::ApplyPatch => self.apply_patch(call_id, &args).await,
            ToolKind::RunCommand => self.run_command(call_id, args).await,
            ToolKind::RunTests => self.run_tests(call_id, args).await,
            ToolKind::ExitPlanMode => self.exit_plan_mode(call_id, &args).await,
            ToolKind::Diagnostics => self.diagnostics(call_id, args).await,
            ToolKind::ProcessRead | ToolKind::ProcessWrite | ToolKind::ProcessKill => {
                self.process_tool(spec.name, call_id, args).await
            }
            // MCP and memory tools are no `ToolSpec`: their own toolsets
            // call them.
            ToolKind::McpStart
            | ToolKind::McpCall
            | ToolKind::SearchMemory
            | ToolKind::SaveMemory
            | ToolKind::DeleteMemory
            | ToolKind::WebRead
            | ToolKind::HostIntel => Err(ToolError::new(
                ErrorCode::Denied,
                format!("`{tool}` is not available"),
            )),
        }
    }

    /// `exit_plan_mode`: the plan goes to the person (never a rule). Accepted:
    /// the session leaves plan mode, so the next model step of the same run
    /// is offered every tool (adk resolves the toolsets again after each
    /// tool batch). Declined: it stays in plan mode, and the person's
    /// feedback, if any, goes back to the model.
    async fn exit_plan_mode(&self, call_id: &str, args: &Value) -> ToolResult<Value> {
        let plan = args
            .get("plan")
            .and_then(Value::as_str)
            .filter(|plan| !plan.trim().is_empty())
            .ok_or_else(|| ToolError::invalid("`plan` is required"))?;
        if plan.len() > MAX_PLAN_BYTES {
            return Err(ToolError::new(
                ErrorCode::TooLarge,
                format!(
                    "the plan is longer than {} KiB; shorten it",
                    MAX_PLAN_BYTES / 1024
                ),
            ));
        }
        let request = ApprovalRequest {
            subject: call_id.to_owned(),
            message: "accept the plan".to_owned(),
            available_actions: vec![ACCEPT.to_owned(), DECLINE.to_owned()],
            payload: json!({
                PAYLOAD_KEY: ToolCall::new(ToolKind::ExitPlanMode),
                PLAN_PAYLOAD_KEY: { "plan": plan },
            }),
        };
        let outcome = self
            .approvals
            .request(request)
            .await
            .map_err(|error| ToolError::new(ErrorCode::ApprovalPending, error.to_string()))?;
        match outcome {
            ApprovalOutcome::Decided { action, .. } if action == ACCEPT => {
                self.engine.set_profile(PermissionProfile::Build);
                Ok(json!({
                    "accepted": true,
                    "message": "The person accepted the plan. Plan mode is over: every tool is available now. Carry out the plan.",
                }))
            }
            ApprovalOutcome::Decided { action, value } if action == DECLINE => {
                let mut result = json!({
                    "accepted": false,
                    "message": "The person declined the plan. You are still in plan mode: revise the plan and call exit_plan_mode again.",
                });
                if let Some(feedback) = feedback_of(&value) {
                    result["feedback"] = Value::String(feedback);
                }
                Ok(result)
            }
            ApprovalOutcome::Decided { value, .. } => {
                let reason = value
                    .get("reason")
                    .and_then(Value::as_str)
                    .unwrap_or("the session is not in plan mode");
                Err(ToolError::new(
                    ErrorCode::Denied,
                    format!("not allowed: {reason}"),
                ))
            }
            ApprovalOutcome::Deferred => Err(ToolError::new(
                ErrorCode::ApprovalPending,
                "waiting for the person's approval",
            )),
        }
    }

    /// `write_file`.
    async fn write_file(self: &Arc<Self>, call_id: &str, args: Value) -> ToolResult<Value> {
        let mut args: files::WriteArgs = parse(args)?;
        let (typed, target) = self.write_target(&args.path)?;
        self.blocking({
            let target = target.clone();
            move |this| files::check_fresh(&this.workspace, &this.ledger, &target).map(|_| ())
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
        let changed = vec![lsp::Changed {
            path: target.clone(),
            deleted: false,
        }];
        let (value, before) = self
            .blocking(move |this| {
                this.unchanged_target(&target)?;
                let before = this.lsp.before(&this.workspace, &[target]);
                Ok((files::write(&this.workspace, &this.ledger, &args)?, before))
            })
            .await?;
        Ok(self.with_diagnostics(value, before, changed).await)
    }

    /// `apply_patch`: every file of the patch checked and approved first.
    async fn apply_patch(self: &Arc<Self>, call_id: &str, args: &Value) -> ToolResult<Value> {
        let patch = args
            .get("patch")
            .and_then(Value::as_str)
            .ok_or_else(|| ToolError::invalid("`patch` is required"))?
            .to_owned();
        let planned = self
            .blocking(move |this| files::plan_patch(&this.workspace, &this.ledger, &patch))
            .await?;
        let typed: Vec<WsPath> = planned.iter().map(|change| change.typed.clone()).collect();
        let targets: Vec<WsPath> = planned.iter().map(|change| change.path.clone()).collect();
        self.authorize_changes(
            call_id,
            ToolKind::ApplyPatch,
            &typed,
            &targets,
            "apply a patch",
        )
        .await?;
        let changed: Vec<lsp::Changed> = planned
            .iter()
            .map(|change| lsp::Changed {
                path: change.path.clone(),
                deleted: change.content.is_none(),
            })
            .collect();
        let (value, before) = self
            .blocking(move |this| {
                for change in &planned {
                    this.unchanged_target(&change.path)?;
                }
                let before = this.lsp.before(&this.workspace, &targets);
                Ok((
                    files::apply_patch(&this.workspace, &this.ledger, &planned)?,
                    before,
                ))
            })
            .await?;
        Ok(self.with_diagnostics(value, before, changed).await)
    }

    /// `edit_file` and `multi_edit`: one path, the same approval row.
    async fn edit_file(
        self: &Arc<Self>,
        spec: &ToolSpec,
        call_id: &str,
        args: Value,
    ) -> ToolResult<Value> {
        let mut edit = if spec.name == "multi_edit" {
            EditCall::Many(parse(args)?)
        } else {
            EditCall::One(parse(args)?)
        };
        let (typed, target) = self.write_target(edit.path())?;
        self.blocking({
            let target = target.clone();
            move |this| files::check_fresh(&this.workspace, &this.ledger, &target).map(|_| ())
        })
        .await?;
        self.authorize_change(call_id, ToolKind::EditFile, &typed, &target, "edit a file")
            .await?;
        edit.set_path(target.display_string());
        let changed = vec![lsp::Changed {
            path: target.clone(),
            deleted: false,
        }];
        let (value, before) = self
            .blocking(move |this| {
                this.unchanged_target(&target)?;
                let before = this.lsp.before(&this.workspace, &[target]);
                let value = match &edit {
                    EditCall::One(args) => files::edit(&this.workspace, &this.ledger, args),
                    EditCall::Many(args) => files::multi_edit(&this.workspace, &this.ledger, args),
                }?;
                Ok((value, before))
            })
            .await?;
        Ok(self.with_diagnostics(value, before, changed).await)
    }

    /// An edit's result with the language servers' `diagnostics` added
    /// (#1236): an additive field, absent when there is nothing to say.
    /// Diagnostics never fail the edit.
    async fn with_diagnostics(
        self: &Arc<Self>,
        mut value: Value,
        before: lsp::Before,
        changed: Vec<lsp::Changed>,
    ) -> Value {
        if let Some(diagnostics) = self.lsp.after(self, before, changed).await
            && let Value::Object(map) = &mut value
        {
            map.insert("diagnostics".to_owned(), diagnostics);
        }
        value
    }

    /// `diagnostics` (#1236): read-only, allowed by default.
    async fn diagnostics(self: &Arc<Self>, call_id: &str, args: Value) -> ToolResult<Value> {
        let args: lsp::DiagnosticsArgs = parse(args)?;
        let mut call = ToolCall::new(ToolKind::Diagnostics);
        if let Some(path) = &args.path {
            call.paths = vec![self.workspace.resolve(path, Intent::Read)?.display_string()];
        }
        self.authorize(call_id, call, "diagnostics").await?;
        self.lsp
            .tool(self, &args, budget::bytes(self.budgets.search))
            .await
    }

    /// `read_output`: no path, the id names one of this session's outputs.
    async fn read_output(self: &Arc<Self>, call_id: &str, args: Value) -> ToolResult<Value> {
        self.authorize(call_id, ToolCall::new(ToolKind::ReadOutput), "read_output")
            .await?;
        self.blocking(move |this| {
            budget::read_output(&this.spill, args, budget::bytes(this.budgets.read_file))
        })
        .await
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
        self.blocking(move |this| {
            let budgets = this.budgets;
            match kind {
                ToolKind::ReadFile => files::read(
                    &this.workspace,
                    &this.ledger,
                    args,
                    budget::bytes(budgets.read_file),
                    this.vision(),
                ),
                ToolKind::ListTree => files::tree(&this.workspace, args),
                ToolKind::SearchFiles => {
                    files::search(&this.workspace, args, budget::bytes(budgets.search))
                }
                ToolKind::FindFiles => {
                    files::find_files(&this.workspace, args, budget::bytes(budgets.search))
                }
                _ => this.read_document(args, budget::bytes(budgets.document)),
            }
        })
        .await
    }

    /// `read_document`, with the extractor the host chose.
    fn read_document(&self, args: Value, max_bytes: usize) -> ToolResult<Value> {
        match &self.documents {
            Documents::InProcess => document::read(
                &self.workspace,
                args,
                &self.spill,
                max_bytes,
                &InProcessExtractor,
            ),
            Documents::Custom(extractor) => {
                document::read(&self.workspace, args, &self.spill, max_bytes, &**extractor)
            }
            Documents::Sidecar(program) => {
                // Read-only, no network: the parser gets bytes on stdin and
                // has nowhere to send anything.
                let request = self.sandbox_request(SandboxMode::ReadOnly, false)?;
                let sidecar = SidecarExtractor::new(program, &request, &self.shell.sandbox);
                document::read(&self.workspace, args, &self.spill, max_bytes, &sidecar)
            }
        }
    }

    /// Ask the approval channel about `call`.
    pub(crate) async fn authorize(
        &self,
        call_id: &str,
        call: ToolCall,
        message: &str,
    ) -> ToolResult<()> {
        let mut payload = json!({ PAYLOAD_KEY: call });
        if let Some(asked) = self.hook_ask(call_id) {
            payload[crate::approvals::HOOK_ASK_PAYLOAD_KEY] = asked;
        }
        let request = ApprovalRequest {
            subject: call_id.to_owned(),
            message: message.to_owned(),
            available_actions: vec!["approve".to_owned(), "reject".to_owned()],
            payload,
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
                if let Some(refusal) = forbidden_by_rule(&value, reason) {
                    return Err(refusal);
                }
                if value.get(DECIDED_BY).and_then(Value::as_str) == Some(BY_PERSON) {
                    return Err(ToolError::new(
                        ErrorCode::Rejected,
                        format!("not allowed: {reason}"),
                    )
                    .with_details(declined_details(feedback_of(&value).as_deref())));
                }
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
        if args.uses_processes() && !self.shell.background_processes {
            return Err(ToolError::new(
                ErrorCode::Unsupported,
                "background processes and loopback are not available here",
            ));
        }
        if args.yield_ms.is_some_and(|ms| ms > processes::MAX_YIELD_MS) {
            return Err(ToolError::invalid("`yield_ms` is 0 to 30000"));
        }
        let background = args.background();
        let cwd = match &args.cwd {
            Some(cwd) => self.workspace.resolve(cwd, Intent::Read)?,
            None => WsPath::root(),
        };
        let max = self.engine.policy().max_sandbox_mode;
        let mode = args.sandbox.unwrap_or(SandboxMode::WorkspaceWrite.min(max));
        let spec = CommandSpec {
            command: args.command,
            cwd,
            timeout: args.timeout_seconds.map(Duration::from_secs),
            mode,
            network: args.network,
            loopback: args.loopback,
        };
        if background {
            // Refused before the person is asked about a process that
            // could not start.
            self.processes.check_room()?;
        }
        // The sandbox this command gets now decides whether it is
        // confined: one that cannot hide what it denies (no sandbox,
        // Landlock alone, a bubblewrap walk cut short), or that cannot keep
        // a loopback command to the loopback, makes it the person's call,
        // every time. A sandbox that cannot run at all is refused after the
        // rules, as before.
        let unconfined = shell::runs_unconfined(&self.workspace, &self.shell, &spec);
        let keep_alive = args.keep_alive.unwrap_or_default();
        let call = ToolCall {
            tool: ToolKind::RunCommand,
            paths: vec![spec.cwd.display_string()],
            command: Some(spec.command.clone()),
            sandbox: Some(mode),
            network: args.network,
            unconfined,
            runner: None,
            loopback: args.loopback,
            keep_alive: keep_alive == KeepAlive::Session,
            mcp: None,
        };
        let message = match (background, args.tty) {
            (_, true) => "run a command on a terminal",
            (true, false) => "run a command in the background",
            (false, false) => "run a command",
        };
        self.authorize(call_id, call, message).await?;
        if mode != SandboxMode::ReadOnly {
            self.blocking(Self::ensure_checkpoint).await?;
        }
        if background {
            return self
                .start_process(
                    call_id,
                    &spec,
                    processes::Start {
                        tty: args.tty,
                        keep_alive,
                        unconfined,
                        yield_for: Duration::from_millis(
                            args.yield_ms.unwrap_or(processes::DEFAULT_YIELD_MS),
                        ),
                    },
                )
                .await;
        }
        // Approved as confined and the sandbox changed since: refused.
        let mut output =
            shell::run_checked(&self.workspace, &self.shell, &spec, unconfined).await?;
        let max_bytes = budget::bytes(self.budgets.run_command);
        if output.stdout.len() + output.stderr.len() > max_bytes {
            output = self
                .blocking(move |this| {
                    budget::shape_command(&mut output, max_bytes, &this.spill);
                    Ok(output)
                })
                .await?;
        }
        serde_json::to_value(output)
            .map_err(|_| ToolError::new(ErrorCode::Io, "cannot encode the result"))
    }

    /// `run_tests` ([`crate::run_tests`]): planned from the project profile
    /// (or the given runner command), asked and run exactly as
    /// `run_command` runs a command, answered with the failing tests.
    async fn run_tests(self: &Arc<Self>, call_id: &str, args: Value) -> ToolResult<Value> {
        let args: run_tests::TestArgs = if args.is_null() {
            run_tests::TestArgs::default()
        } else {
            parse(args)?
        };
        let junit = run_tests::junit_path(&self.shell.temp_dir)?;
        if args
            .timeout_seconds
            .is_some_and(|secs| !(1..=1800).contains(&secs))
        {
            return Err(ToolError::invalid("`timeout_seconds` is 1 to 1800"));
        }
        let network = args.network;
        let timeout = Duration::from_secs(
            args.timeout_seconds
                .unwrap_or_else(|| self.shell.max_timeout.as_secs()),
        );
        let planned = self
            .blocking({
                let junit = junit.clone();
                move |this| {
                    let profile = this.profile_for(None);
                    run_tests::plan(&this.workspace, &profile, &args, &junit)
                }
            })
            .await?;
        let max = self.engine.policy().max_sandbox_mode;
        let mode = SandboxMode::WorkspaceWrite.min(max);
        let spec = CommandSpec {
            command: planned.command.clone(),
            cwd: planned.cwd.clone(),
            timeout: Some(timeout),
            mode,
            network,
            loopback: false,
        };
        let unconfined = shell::runs_unconfined(&self.workspace, &self.shell, &spec);
        let call = ToolCall {
            command: Some(spec.command.clone()),
            sandbox: Some(mode),
            network,
            unconfined,
            runner: Some(planned.kind.as_str().to_owned()),
            paths: vec![spec.cwd.display_string()],
            ..ToolCall::new(ToolKind::RunTests)
        };
        self.authorize(call_id, call, "run the tests").await?;
        if mode != SandboxMode::ReadOnly {
            self.blocking(Self::ensure_checkpoint).await?;
        }
        let started = std::time::SystemTime::now();
        // Cargo's artifact records are left out of what is kept: they
        // would push the test results out of the capture's head and tail.
        let keep = (planned.kind == project::ProjectKind::Cargo)
            .then_some(crate::test_report::cargo_keeps_line as shell::KeepLine);
        let output = shell::run_checked_up_to(
            &self.workspace,
            &self.shell,
            &spec,
            unconfined,
            self.shell.max_test_timeout,
            keep,
        )
        .await?;
        self.blocking(move |this| {
            let (report, junit_text) =
                run_tests::report(&this.workspace, &planned, &output, &junit, started);
            Ok(run_tests::answer(
                &planned,
                &output,
                &report,
                junit_text.as_deref(),
                &this.spill,
                budget::bytes(this.budgets.run_command),
                network,
            ))
        })
        .await
    }

    /// `git_commit`: through the hardened host git (the repository is
    /// checked, hooks and signing are off, the process is sandboxed and
    /// may write only the git directory's data). Allowed by default; a
    /// workspace rule may ask or deny it.
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
        let denied = self.shell.deny_list();
        let mut pathspecs = selected.clone();
        if !pathspecs.is_empty() {
            pathspecs.extend(diff_exclusions(&self.workspace, denied.paths()));
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
            if pathspecs.is_empty() {
                // Without paths the commit takes the whole index: denied
                // paths staged there are never committed.
                refuse_staged_denied(&this.workspace, repo, &denied)?;
            } else {
                let mut words = vec!["add", "--"];
                words.extend(pathspecs.iter().map(String::as_str));
                repo.git(root)
                    .literal(false)
                    .writes(&writes)
                    .protect(&protected)
                    .worktree()
                    .run(&words)?;
                unstage_denied(
                    &this.workspace,
                    repo,
                    &selected,
                    &denied,
                    &writes,
                    &protected,
                )?;
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
                command_line.extend(diff_exclusions(
                    &self.workspace,
                    self.shell.deny_list().paths(),
                ));
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
        let spills = matches!(tool, "git_diff" | "git_log");
        self.blocking(move |this| Ok(this.git_answer(&output, spills)))
            .await
    }

    /// A git read's result: capped; `git_diff` and `git_log` within their
    /// token budget, the whole output spilled for `read_output`.
    fn git_answer(&self, output: &[u8], spills: bool) -> Value {
        let (text, capped_off) = capped(output);
        if !spills {
            return json!({ "output": text, "truncated": capped_off });
        }
        let max_bytes = budget::bytes(self.budgets.git);
        let (shown, output_id, cut) = budget::spill_over(&self.spill, &text, max_bytes);
        let mut answer = json!({
            "output": shown,
            "truncated": cut || capped_off,
            "bytes": output.len(),
        });
        if let (Some(id), Value::Object(map)) = (output_id, &mut answer) {
            map.insert("output_id".to_owned(), Value::String(id));
        }
        answer
    }
}

/// The key of a rejection's value that says who decided: [`BY_PERSON`] when
/// the person declined in the host's prompt (the call's result then carries
/// [`declined_details`]); absent when a rule or the policy decided.
pub const DECIDED_BY: &str = "by";

/// [`DECIDED_BY`]'s value when the person declined the call themselves.
pub const BY_PERSON: &str = "person";

/// The `error.code` of a call the person declined (#1225), in
/// [`declined_details`].
pub const DECLINED_BY_PERSON: &str = "declined_by_person";

/// The typed failure a call the person declined carries in its result
/// (#1225): `ok: false` and `error: {code: "declined_by_person", message,
/// feedback?}`. `feedback` is the person's own text, why they declined (not
/// repository data, and never an approval): the model reads it and
/// adjusts its next step. Hosts add these fields to a declined remote
/// call's result too.
#[must_use]
pub fn declined_details(feedback: Option<&str>) -> serde_json::Map<String, Value> {
    let mut error = json!({
        "code": DECLINED_BY_PERSON,
        "message": if feedback.is_some() {
            "The person declined this call. `feedback` is what they wrote, in their own words."
        } else {
            "The person declined this call."
        },
    });
    if let Some(feedback) = feedback {
        error["feedback"] = Value::String(feedback.to_owned());
    }
    let mut details = serde_json::Map::new();
    details.insert("ok".to_owned(), Value::Bool(false));
    details.insert("error".to_owned(), error);
    details
}

/// The person's feedback in a decision's value (`feedback`), bounded to
/// [`MAX_FEEDBACK_BYTES`] at a character boundary; `None` when empty.
#[must_use]
pub fn feedback_of(value: &Value) -> Option<String> {
    let text = value.get("feedback").and_then(Value::as_str)?.trim();
    if text.is_empty() {
        return None;
    }
    Some(elitea_agent_runtime::text::clip_bytes(text, MAX_FEEDBACK_BYTES).to_owned())
}

/// A `forbidden` prefix rule's refusal, as the model sees it:
/// `{"error": "forbidden_by_rule", "justification": "…", "rule":
/// "workspace#3"}` beside the usual fields, so it can take the suggested
/// alternative instead of retrying.
fn forbidden_by_rule(value: &Value, reason: &str) -> Option<ToolError> {
    if value.get("decision").and_then(Value::as_str) != Some("forbidden") {
        return None;
    }
    let rule = value.get("rule").and_then(Value::as_str)?;
    let mut details = serde_json::Map::new();
    details.insert("error".to_owned(), json!("forbidden_by_rule"));
    details.insert("rule".to_owned(), json!(rule));
    let justification = value.get("justification").and_then(Value::as_str);
    if let Some(justification) = justification {
        details.insert("justification".to_owned(), json!(justification));
    }
    let message = match justification {
        Some(justification) => format!("forbidden by {rule}: {justification}"),
        None => format!("forbidden by {rule}: {reason}"),
    };
    Some(ToolError::new(ErrorCode::Denied, message).with_details(details))
}
#[cfg(test)]
mod tests {
    use std::path::Path;

    use super::{EditProfile, TOOLS, denied_staged, tools_for};
    use crate::deny::DenyList;
    use crate::workspace::{CASE_INSENSITIVE_FS, Workspace};

    /// #1214: a GPT-5 / codex agent is offered `write_file` and
    /// `apply_patch` described as the Codex envelope; a Claude agent the
    /// str-replace tools too. Names and order never change otherwise.
    #[test]
    fn tools_for_patch_profile_hides_str_replace() {
        let names = |profile| {
            tools_for(profile)
                .into_iter()
                .map(|spec| spec.name)
                .collect::<Vec<_>>()
        };
        let patch = tools_for(EditProfile::select("gpt-5", None));
        let patch_names: Vec<_> = patch.iter().map(|spec| spec.name).collect();
        assert!(patch_names.contains(&"write_file") && patch_names.contains(&"apply_patch"));
        assert!(patch_names.contains(&"run_command") && patch_names.contains(&"run_tests"));
        assert!(!patch_names.contains(&"edit_file") && !patch_names.contains(&"multi_edit"));
        let apply = patch
            .iter()
            .find(|spec| spec.name == "apply_patch")
            .expect("apply_patch");
        assert!(apply.description.contains("*** Begin Patch"));
        assert!(apply.description.contains("*** Update File:"));
        let claude = tools_for(EditProfile::select("claude-sonnet-4-5", None));
        let claude_names: Vec<_> = claude.iter().map(|spec| spec.name).collect();
        for tool in ["edit_file", "multi_edit", "apply_patch", "write_file"] {
            assert!(claude_names.contains(&tool), "{tool}");
        }
        assert!(
            claude
                .iter()
                .find(|spec| spec.name == "apply_patch")
                .expect("apply_patch")
                .description
                .starts_with("Apply a patch to files in the workspace: a unified diff")
        );
        assert_eq!(
            claude_names,
            TOOLS.iter().map(|spec| spec.name).collect::<Vec<_>>()
        );
        // meta.edit_format overrides the model in both directions.
        assert_eq!(
            names(EditProfile::select("claude-sonnet-4-5", Some("patch"))),
            patch_names
        );
        assert_eq!(
            names(EditProfile::select("gpt-5", Some("str_replace"))),
            claude_names
        );
        // Patch is the str-replace list minus two names, in the same order.
        let mut expected = claude_names.clone();
        expected.retain(|name| !matches!(*name, "edit_file" | "multi_edit"));
        assert_eq!(patch_names, expected);
    }

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
            denied_staged(&workspace, Path::new(&top), staged, &DenyList::default()),
            ["ws/.env", "ws/deep/.env", "ws/secrets/key"]
        );
    }

    /// A staged path under the deny list (outside the workspace, in the
    /// repository) is found under any case where the file system folds
    /// case, as `diff_exclusions`' `icase` pathspecs match it.
    #[test]
    fn staged_paths_under_the_deny_list_match_case_insensitively() {
        let dir = tempfile::tempdir().expect("dir");
        let top = std::fs::canonicalize(dir.path()).expect("canonical");
        std::fs::create_dir(top.join("ws")).expect("ws");
        let workspace = Workspace::open(&top.join("ws"), &[]).expect("workspace");
        let denied = DenyList::new([top.join("home/.ssh")]);
        let staged = b"home/.ssh/id\0home/.SSH/id_ed25519\0home/sshx\0";
        let found = denied_staged(&workspace, Path::new(&top), staged, &denied);
        if CASE_INSENSITIVE_FS {
            assert_eq!(found, ["home/.ssh/id", "home/.SSH/id_ed25519"]);
        } else {
            assert_eq!(found, ["home/.ssh/id"]);
        }
    }
}
