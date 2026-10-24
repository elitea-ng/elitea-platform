//! A repository's hooks as proposals (#1237): `.elitea/hooks.json` (this
//! crate's own format) and the `hooks` of `.claude/settings.json`, read
//! through the [`Workspace`] (`O_NOFOLLOW`, confined to the folder,
//! `path_deny` honoured). Nothing here runs a hook: the host shows the
//! proposal (each command, sandbox and network switch) and copies the hooks
//! the person trusts into the workspace's own `hooks.json` in host data,
//! with `source: "proposal:<sha256 of the file>"`. A later change to the
//! file is a new proposal (a new digest), asked again; an accepted entry is
//! never updated from it.
//!
//! A Claude hook (`{"matcher": "Edit|Write", "hooks": [{"type": "command",
//! "command": "…", "timeout": 60}]}`) becomes `["/bin/sh", "-c", "…"]` in the
//! read-only sandbox, its matcher's tool names mapped to the local tools
//! (`Bash` → `run_command`, `Edit` → `edit_file`, `multi_edit` and
//! `apply_patch`, `Write` → `write_file`, …) and its timeout capped
//! at [`MAX_TIMEOUT_MS`]. It receives this crate's stdin contract, not
//! Claude's.

use serde::Serialize;
use serde_json::Value;
use sha2::{Digest as _, Sha256};

use super::config::{Hook, HookEvent, HookMatch, HooksConfig, MAX_HOOKS, MAX_TIMEOUT_MS};
use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::policy::SandboxMode;
use crate::workspace::{Intent, Workspace};

/// Where a repository proposes hooks, in the order they are read.
pub const FILES: &[&str] = &[".elitea/hooks.json", ".claude/settings.json"];

/// The largest proposal file read.
const MAX_PROPOSAL_BYTES: u64 = 256 * 1024;

/// One proposed hook, as the trust card shows it.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct ProposedHook {
    #[serde(flatten)]
    pub hook: Hook,
    /// Why it cannot be accepted as written, if it cannot.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub problem: Option<String>,
}

/// One repository file's proposal.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct Proposal {
    /// Workspace-relative.
    pub file: String,
    /// Of the file's bytes, lowercase hex: what an answer names.
    pub file_sha256: String,
    pub hooks: Vec<ProposedHook>,
}

fn proposed(mut hook: Hook) -> ProposedHook {
    "user".clone_into(&mut hook.source);
    hook.enabled = true;
    let problem = hook
        .validate()
        .err()
        .map(|error| error.message().to_owned());
    ProposedHook { hook, problem }
}

/// Claude's tool names, as the local tools are named.
fn local_tools(claude: &str) -> Option<&'static [&'static str]> {
    Some(match claude {
        "Bash" => &["run_command", "run_tests"],
        // Claude's `Edit` is every in-place edit here: the edit tools a
        // model is offered depend on its edit profile.
        "Edit" => &["edit_file", "multi_edit", "apply_patch"],
        "MultiEdit" => &["multi_edit", "apply_patch"],
        "Write" => &["write_file"],
        "Read" => &["read_file"],
        "Grep" => &["search_files"],
        "Glob" => &["find_files"],
        "LS" => &["list_tree"],
        _ => return None,
    })
}

fn event(name: &str) -> Option<HookEvent> {
    Some(match name {
        "PreToolUse" => HookEvent::PreToolUse,
        "PostToolUse" => HookEvent::PostToolUse,
        "Stop" => HookEvent::Stop,
        "UserPromptSubmit" => HookEvent::UserPromptSubmit,
        _ => return None,
    })
}

/// The hooks of a `.claude/settings.json` (`None`: it has none).
fn claude_hooks(value: &Value) -> Vec<ProposedHook> {
    let Some(events) = value.get("hooks").and_then(Value::as_object) else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for (name, groups) in events {
        let Some(kind) = event(name) else {
            continue;
        };
        for group in groups.as_array().into_iter().flatten() {
            let matcher = group.get("matcher").and_then(Value::as_str).unwrap_or("");
            let mut problem = None;
            let mut tools: Vec<String> = Vec::new();
            if kind.is_tool_event() && !matches!(matcher.trim(), "" | "*") {
                for part in matcher.split('|').map(str::trim) {
                    match local_tools(part) {
                        Some(names) => {
                            for name in names {
                                if !tools.iter().any(|known| known == name) {
                                    tools.push((*name).to_owned());
                                }
                            }
                        }
                        None => problem = Some(format!("`{part}` names no local tool")),
                    }
                }
            }
            for entry in group
                .get("hooks")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
            {
                let Some(command) = entry.get("command").and_then(Value::as_str) else {
                    continue;
                };
                if entry.get("type").and_then(Value::as_str) != Some("command") {
                    continue;
                }
                let timeout_ms = entry
                    .get("timeout")
                    .and_then(Value::as_u64)
                    .map_or(super::config::DEFAULT_TIMEOUT_MS, |secs| {
                        secs.saturating_mul(1000).clamp(1, MAX_TIMEOUT_MS)
                    });
                let id = format!("claude-{}-{}", name.to_ascii_lowercase(), out.len() + 1);
                let mut hook = Hook::new(
                    &id,
                    kind,
                    vec!["/bin/sh".to_owned(), "-c".to_owned(), command.to_owned()],
                );
                hook.timeout_ms = timeout_ms;
                hook.sandbox = SandboxMode::ReadOnly;
                hook.matcher = HookMatch {
                    tools: tools.clone(),
                    paths: Vec::new(),
                };
                let mut shown = proposed(hook);
                if shown.problem.is_none() {
                    shown.problem.clone_from(&problem);
                }
                out.push(shown);
            }
        }
    }
    out
}

/// The proposal in `bytes` (`None`: it proposes no hook).
///
/// # Errors
///
/// [`ErrorCode::InvalidArgument`] when the file is not JSON, or proposes
/// more than [`MAX_HOOKS`] hooks.
pub fn parse(file: &str, bytes: &[u8]) -> ToolResult<Option<Proposal>> {
    let value: Value = serde_json::from_slice(bytes)
        .map_err(|error| ToolError::invalid(format!("`{file}` is not JSON: {error}")))?;
    let hooks = if file == ".claude/settings.json" {
        claude_hooks(&value)
    } else {
        // Our own format, read leniently per hook so the card can show what
        // is wrong with one instead of dropping the file.
        let list = value.get("hooks").and_then(Value::as_array);
        list.into_iter()
            .flatten()
            .map(
                |entry| match serde_json::from_value::<Hook>(entry.clone()) {
                    Ok(hook) => proposed(hook),
                    Err(error) => {
                        let id = entry.get("id").and_then(Value::as_str).unwrap_or("?");
                        let mut hook = Hook::new(id, HookEvent::Stop, Vec::new());
                        hook.command = entry
                            .get("command")
                            .and_then(Value::as_array)
                            .map(|items| {
                                items
                                    .iter()
                                    .filter_map(Value::as_str)
                                    .map(str::to_owned)
                                    .collect()
                            })
                            .unwrap_or_default();
                        ProposedHook {
                            hook,
                            problem: Some(format!("not a valid hook: {error}")),
                        }
                    }
                },
            )
            .collect()
    };
    if hooks.is_empty() {
        return Ok(None);
    }
    if hooks.len() > MAX_HOOKS {
        return Err(ToolError::invalid(format!(
            "`{file}` proposes more than {MAX_HOOKS} hooks"
        )));
    }
    Ok(Some(Proposal {
        file: file.to_owned(),
        file_sha256: crate::mcp::config::hex(&Sha256::digest(bytes)),
        hooks,
    }))
}

/// What the workspace proposes now, file by file (a file that cannot be
/// read, is denied or does not parse proposes nothing).
#[must_use]
pub fn read(workspace: &Workspace) -> Vec<Proposal> {
    FILES
        .iter()
        .filter_map(|file| {
            let path = workspace.resolve(file, Intent::Read).ok()?;
            let read = workspace.read(&path, MAX_PROPOSAL_BYTES).ok()?;
            parse(file, &read.bytes).ok().flatten()
        })
        .collect()
}

/// The hooks of `proposal` named in `accept`, as entries of the
/// workspace's `hooks.json`, `source` naming the proposal's digest.
///
/// # Errors
///
/// [`ErrorCode::NotFound`] for an id the proposal does not hold;
/// [`ErrorCode::InvalidArgument`] for one that cannot be accepted.
pub fn accept(proposal: &Proposal, accept: &[String]) -> ToolResult<Vec<Hook>> {
    accept
        .iter()
        .map(|id| {
            let shown = proposal
                .hooks
                .iter()
                .find(|shown| &shown.hook.id == id)
                .ok_or_else(|| {
                    ToolError::new(ErrorCode::NotFound, format!("`{id}` is not proposed"))
                })?;
            if let Some(problem) = &shown.problem {
                return Err(ToolError::invalid(format!("`{id}`: {problem}")));
            }
            let mut hook = shown.hook.clone();
            hook.source = format!("proposal:{}", proposal.file_sha256);
            hook.validate()?;
            Ok(hook)
        })
        .collect()
}

/// Why `id` cannot be trusted here: the person's own file has a hook with
/// it (a workspace hook replaces a user hook of its id, so a repository
/// could otherwise swap one of theirs for its own), or the workspace's
/// does.
fn taken(id: &str, user: &HooksConfig, workspace: &HooksConfig) -> Option<String> {
    if user.hooks.iter().any(|hook| hook.id == id) {
        Some(format!(
            "your own hook `{id}` has this id, and a repository's hook never replaces one of yours"
        ))
    } else if workspace.hooks.iter().any(|hook| hook.id == id) {
        Some(format!(
            "this workspace already has a hook `{id}`; remove it first"
        ))
    } else {
        None
    }
}

/// Mark each proposed hook whose id the person's file or the workspace's
/// already has, so the trust card says why it cannot be trusted (and
/// [`accept`] refuses it).
pub fn mark_taken(proposal: &mut Proposal, user: &HooksConfig, workspace: &HooksConfig) {
    for shown in &mut proposal.hooks {
        if shown.problem.is_none() {
            shown.problem = taken(&shown.hook.id, user, workspace);
        }
    }
}

/// `config` (the workspace's file) with `accepted` added, refused when an
/// id is taken there or in `user` (the person's file), and the digest
/// answered.
///
/// # Errors
///
/// [`ErrorCode::InvalidArgument`] when either file already has a hook with
/// an accepted id, or the result breaks a bound.
pub fn apply(
    config: &mut HooksConfig,
    user: &HooksConfig,
    digest: &str,
    accepted: Vec<Hook>,
) -> ToolResult<()> {
    if let Some(why) = accepted
        .iter()
        .find_map(|hook| taken(&hook.id, user, config))
    {
        return Err(ToolError::invalid(why));
    }
    config.hooks.extend(accepted);
    config.answered(digest);
    config.validate()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn both_shapes_parse_and_problems_are_shown() {
        let ours = br#"{"version":1,"hooks":[
            {"id":"guard","event":"PreToolUse","command":["/usr/bin/true"]},
            {"id":"bad","event":"PreToolUse","command":["./guard.sh"]}
        ]}"#;
        let proposal = parse(".elitea/hooks.json", ours)
            .expect("parse")
            .expect("some");
        assert_eq!(proposal.hooks.len(), 2);
        assert!(proposal.hooks[0].problem.is_none());
        assert!(proposal.hooks[1].problem.is_some());
        assert_eq!(proposal.file_sha256.len(), 64);
        assert!(accept(&proposal, &["bad".to_owned()]).is_err());
        let accepted = accept(&proposal, &["guard".to_owned()]).expect("accept");
        assert_eq!(
            accepted[0].source,
            format!("proposal:{}", proposal.file_sha256)
        );

        let claude = br#"{"permissions":{},"hooks":{"PostToolUse":[
            {"matcher":"Edit|Write","hooks":[{"type":"command","command":"cargo fmt","timeout":120}]}
        ],"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}"#;
        let proposal = parse(".claude/settings.json", claude)
            .expect("parse")
            .expect("some");
        let post = proposal
            .hooks
            .iter()
            .find(|shown| shown.hook.event == HookEvent::PostToolUse)
            .expect("post");
        assert_eq!(post.hook.command, ["/bin/sh", "-c", "cargo fmt"]);
        assert_eq!(
            post.hook.matcher.tools,
            ["edit_file", "multi_edit", "apply_patch", "write_file"]
        );
        assert_eq!(post.hook.timeout_ms, MAX_TIMEOUT_MS);
        assert_eq!(post.hook.sandbox, SandboxMode::ReadOnly);
        assert!(post.problem.is_none(), "{:?}", post.problem);
        assert!(
            parse(".claude/settings.json", b"{}")
                .expect("parse")
                .is_none()
        );
    }

    #[test]
    fn an_accepted_proposal_is_answered_and_ids_are_not_replaced() {
        let ours =
            br#"{"version":1,"hooks":[{"id":"guard","event":"Stop","command":["/usr/bin/true"]}]}"#;
        let proposal = parse(".elitea/hooks.json", ours)
            .expect("parse")
            .expect("some");
        let mut config = HooksConfig::default();
        let user = HooksConfig::default();
        let accepted = accept(&proposal, &["guard".to_owned()]).expect("accept");
        apply(&mut config, &user, &proposal.file_sha256, accepted.clone()).expect("apply");
        assert_eq!(
            config.answered_proposals,
            std::slice::from_ref(&proposal.file_sha256)
        );
        assert!(apply(&mut config, &user, &proposal.file_sha256, accepted).is_err());
    }

    #[test]
    fn a_proposal_cannot_take_the_id_of_a_user_hook() {
        let ours =
            br#"{"version":1,"hooks":[{"id":"guard","event":"Stop","command":["/usr/bin/true"]}]}"#;
        let mut proposal = parse(".elitea/hooks.json", ours)
            .expect("parse")
            .expect("some");
        let mut user = HooksConfig::default();
        user.hooks.push(Hook::new(
            "guard",
            HookEvent::PreToolUse,
            vec!["/usr/local/bin/my-guard".to_owned()],
        ));
        let mut workspace = HooksConfig::default();
        // Accepted before the card was marked (an older card, a direct
        // call): `apply` refuses it anyway.
        let accepted = accept(&proposal, &["guard".to_owned()]).expect("accept");
        let refused = apply(&mut workspace, &user, &proposal.file_sha256, accepted)
            .expect_err("a user id is never replaced");
        assert!(refused.message().contains("your own hook"), "{refused:?}");
        assert!(workspace.hooks.is_empty(), "nothing copied");
        assert!(workspace.answered_proposals.is_empty(), "not answered");
        // The card says why, and accepting it is refused.
        mark_taken(&mut proposal, &user, &workspace);
        assert!(
            proposal.hooks[0]
                .problem
                .as_deref()
                .is_some_and(|why| why.contains("your own hook `guard`")),
            "{:?}",
            proposal.hooks[0].problem
        );
        assert!(accept(&proposal, &["guard".to_owned()]).is_err());
    }
}
