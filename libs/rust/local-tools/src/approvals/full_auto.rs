//! Full auto (ADR-0061 amendment of 2026-11-26, issue 3079).
//!
//! [`super::PermissionProfile::FullAuto`] answers every ask Bypass answers, and
//! the taint gate's ask too (decision 5f): after an agent read outside
//! content, a host command no longer waits for the person. That is the
//! residual risk the person accepts when they turn it on: instructions in
//! outside content can now drive commands. What stays in front of a
//! command:
//!
//! - every deny: the policy ceiling, forbidden rules, a hook's deny;
//! - what Bypass leaves to the person: a `computer` ask, a consequential
//!   browser action, a browser effect in an unattended turn, a hook's ask
//!   (the unattended grant's `hook_asks` answers that one);
//! - the **hard-deny list** ([`hold`]): commands whose damage outlives the
//!   folder and its checkpoints. Full auto never answers them: they are
//!   asked in an attended turn, and declined at once in an unattended one
//!   (fail closed: nobody is there to ask).
//!
//! The list is a heuristic over the command text, like the classifier and
//! the deny rules ([`crate::command`]): it catches a model naming such a
//! path or program, not a script that does it. The OS sandbox, where the
//! command runs in one, still bounds it; in a trusted folder nothing does.

use std::path::{Component, Path, PathBuf};

use super::{Decision, RulesEngine, Source, ToolCall, Verdict};

#[cfg(test)]
mod tests;
use crate::command::{analyse, basename};

/// Why Full auto did not answer a call.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Hold {
    /// A recursive delete whose target is outside the folder, or cannot be
    /// told apart from one (a variable, `~`, a substitution).
    RecursiveDeleteOutside,
    /// It names a credential store: SSH and GPG keys, cloud, container and
    /// git credentials, the keychains, browser profiles
    /// ([`crate::sandbox::credential_paths`]), or a keychain program.
    CredentialStore,
    /// It names a shell start-up file (`~/.zshrc`, `~/.profile`, …).
    ShellProfile,
    /// It names the app's own data (its rules, trust and sandbox settings)
    /// or turns off an OS protection (SIP, Gatekeeper, `SELinux`,
    /// `AppArmor`).
    SandboxControl,
}

impl Hold {
    /// The reason the person (or the model) reads.
    #[must_use]
    pub const fn reason(self) -> &'static str {
        match self {
            Self::RecursiveDeleteOutside => {
                "Full auto does not answer a recursive delete outside this folder"
            }
            Self::CredentialStore => {
                "Full auto does not answer a command that touches a credential store"
            }
            Self::ShellProfile => {
                "Full auto does not answer a command that touches a shell profile"
            }
            Self::SandboxControl => {
                "Full auto does not answer a command that changes the app's sandbox or approval settings or an OS protection"
            }
        }
    }
}

/// Credential stores under the home directory, as path components
/// (the same list the sandbox hides, [`crate::sandbox::credential_paths`],
/// without the app's own directories, which are [`APP_STATE`]).
const CREDENTIAL_STORES: &[&str] = &[
    ".ssh",
    ".gnupg",
    ".aws",
    ".azure",
    ".config/gcloud",
    ".kube",
    ".docker/config.json",
    ".netrc",
    ".git-credentials",
    ".config/git/credentials",
    ".config/gh",
    ".config/hub",
    ".cargo/credentials",
    ".cargo/credentials.toml",
    ".pypirc",
    ".npmrc",
    ".pgpass",
    ".terraform.d/credentials.tfrc.json",
    ".password-store",
    ".local/share/keyrings",
    ".mozilla",
    ".config/google-chrome",
    ".config/chromium",
    "library/keychains",
    "library/cookies",
    "appdata/roaming/microsoft/credentials",
    "appdata/local/microsoft/credentials",
    "appdata/roaming/microsoft/protect",
    "appdata/roaming/gcloud",
    "appdata/roaming/github cli",
];

/// Shell start-up files (any directory: `~/.zshrc`, `/etc/zshrc`).
const SHELL_PROFILES: &[&str] = &[
    ".bashrc",
    ".bash_profile",
    ".bash_login",
    ".bash_logout",
    ".profile",
    ".zshrc",
    ".zshenv",
    ".zprofile",
    ".zlogin",
    ".zlogout",
    ".cshrc",
    ".tcshrc",
    ".kshrc",
    ".config/fish",
    "etc/profile",
    "etc/profile.d",
    "etc/bashrc",
    "etc/bash.bashrc",
    "etc/zshrc",
    "etc/zprofile",
    "etc/zshenv",
    "etc/environment",
    "etc/paths",
    "etc/paths.d",
    "microsoft.powershell_profile.ps1",
    "profile.ps1",
];

/// The app's own directories (its rules, choices, trust and sandbox
/// settings), as [`crate::sandbox::credential_paths`] names them; a last
/// component ending in `*` is a name prefix.
const APP_STATE: &[&str] = &[
    ".config/elitea*",
    ".local/share/elitea*",
    "library/application support/elitea*",
    "library/preferences/elitea*",
    "library/preferences/ai.elitea*",
    "library/application support/ai.elitea*",
    "appdata/roaming/elitea*",
    "appdata/local/elitea*",
];

/// Programs that read or change a keychain.
const KEYCHAIN_PROGRAMS: &[&str] = &["security", "secret-tool", "cmdkey", "keyring"];

/// `(program, argument)` pairs that turn an OS protection off; an empty
/// argument matches the program alone.
const PROTECTION_OFF: &[(&str, &str)] = &[
    ("csrutil", "disable"),
    ("spctl", "--master-disable"),
    ("spctl", "--global-disable"),
    ("setenforce", "0"),
    ("setenforce", "permissive"),
    ("aa-disable", ""),
    ("aa-teardown", ""),
    ("aa-complain", ""),
    ("apparmor_parser", "-r"),
    ("apparmor_parser", "--remove"),
    ("tccutil", ""),
    ("set-mppreference", ""),
];

/// The hard-deny list: why Full auto must not answer `call`, if it must
/// not. Only calls that carry a command are judged.
#[must_use]
pub fn hold(call: &ToolCall, root: &crate::workspace::Workspace) -> Option<Hold> {
    let command = call.command.as_deref()?;
    let text = Text::of(command);
    if text.names(APP_STATE) || protection_off(command) {
        return Some(Hold::SandboxControl);
    }
    if text.names(CREDENTIAL_STORES) || keychain(command) {
        return Some(Hold::CredentialStore);
    }
    if text.names(SHELL_PROFILES) {
        return Some(Hold::ShellProfile);
    }
    recursive_delete_outside(command, text.moves, root).then_some(Hold::RecursiveDeleteOutside)
}

/// A command's text as the hard-deny list reads it.
struct Text {
    /// Lowercased, `\` read as `/`.
    lower: String,
    /// `lower` split on whitespace, quotes and shell operators (redirection
    /// targets included).
    words: Vec<String>,
    /// It changes directory (`cd`, `pushd`): a relative path may then point
    /// anywhere.
    moves: bool,
}

impl Text {
    fn of(command: &str) -> Self {
        let lower = command.to_lowercase().replace('\\', "/");
        let words = lower
            .split(|c: char| {
                c.is_whitespace()
                    || matches!(
                        c,
                        '\'' | '"' | '`' | '=' | '<' | '>' | '|' | ';' | '&' | '(' | ')' | ','
                    )
            })
            .filter(|word| !word.is_empty())
            .map(str::to_owned)
            .collect();
        let moves = lower
            .split(|c: char| c.is_whitespace() || matches!(c, ';' | '&' | '|' | '(' | '`'))
            .any(|word| matches!(word, "cd" | "pushd" | "set-location" | "chdir" | "sl"));
        Self {
            lower,
            words,
            moves,
        }
    }

    /// Whether the command names one of `entries` (lowercase path
    /// components; a last one ending in `*` is a name prefix). A word that
    /// starts at the home directory or the root (`~`, `$HOME`, `/`, a
    /// drive) may hold it anywhere. A relative word counts only from its
    /// start, and only when the command changes directory: otherwise it is
    /// inside the folder (a project's own `.npmrc`). An entry with a space
    /// (`library/application support/…`) is looked for in the whole text.
    fn names(&self, entries: &[&str]) -> bool {
        entries.iter().any(|entry| {
            if entry.contains(' ') {
                return self.lower.contains(entry.trim_end_matches('*'));
            }
            let wanted: Vec<&str> = entry.split('/').collect();
            self.words.iter().any(|word| {
                let homeish =
                    word.starts_with(['~', '$', '%', '/']) || word.as_bytes().get(1) == Some(&b':');
                if !homeish && !self.moves {
                    return false;
                }
                let components: Vec<&str> =
                    word.split('/').filter(|part| !part.is_empty()).collect();
                let starts = if homeish { components.len() } else { 1 };
                (0..starts).any(|start| {
                    components
                        .get(start..)
                        .is_some_and(|rest| matches_at(rest, &wanted))
                })
            })
        })
    }
}

fn matches_at(components: &[&str], wanted: &[&str]) -> bool {
    if components.len() < wanted.len() {
        return false;
    }
    wanted.iter().zip(components).all(|(want, have)| {
        want.strip_suffix('*')
            .map_or_else(|| want == have, |prefix| have.starts_with(prefix))
    })
}

fn segments(command: &str) -> Vec<Vec<String>> {
    analyse(command).segments()
}

fn program(argv: &[String]) -> Option<String> {
    argv.first().map(|program| basename(program).to_lowercase())
}

fn keychain(command: &str) -> bool {
    segments(command).iter().any(|argv| {
        program(argv).is_some_and(|program| KEYCHAIN_PROGRAMS.contains(&program.as_str()))
    })
}

fn protection_off(command: &str) -> bool {
    segments(command).iter().any(|argv| {
        let Some(program) = program(argv) else {
            return false;
        };
        PROTECTION_OFF.iter().any(|(name, argument)| {
            program == *name
                && (argument.is_empty()
                    || argv[1..]
                        .iter()
                        .any(|word| word.eq_ignore_ascii_case(argument)))
        })
    })
}

/// A recursive delete (`rm -r`, `find … -delete`, `Remove-Item -Recurse`,
/// `rmdir /s`) whose target lies outside the folder, or whose target a
/// variable, `~` or a substitution spells (the analysis cannot tell where
/// it points, so it holds). After a `cd` a relative target may point
/// anywhere, so it holds too.
fn recursive_delete_outside(
    command: &str,
    moves: bool,
    root: &crate::workspace::Workspace,
) -> bool {
    // Each piece between shell operators on its own: a variable in another
    // piece (`rm -rf dist && echo "$(date)"`) does not make the delete
    // opaque. The analysis drops such words from the argv, so the raw piece
    // decides.
    command.split([';', '|', '&', '\n']).any(|piece| {
        let opaque = piece.contains(['$', '~', '`', '%']);
        segments(piece).iter().any(|argv| {
            let Some(program) = program(argv) else {
                return false;
            };
            let Some(targets) = delete_targets(&program, argv) else {
                return false;
            };
            opaque || targets.iter().any(|target| outside(target, moves, root))
        })
    })
}

/// Whether a delete's switch asks for recursion.
fn recursive(program: &str, lower: &[String]) -> bool {
    match program {
        // `rm`'s short flags may be grouped (`-rf`, `-fR`).
        "rm" => lower.iter().any(|word| {
            word == "--recursive"
                || (word.starts_with('-') && !word.starts_with("--") && word.contains('r'))
        }),
        // PowerShell takes any prefix of a parameter name (`-r`, `-rec`);
        // `del` and `erase` are also cmd's, whose `/s` recurses.
        "remove-item" | "ri" | "del" | "erase" => lower
            .iter()
            .any(|word| word == "/s" || (word.len() >= 2 && "-recurse".starts_with(word.as_str()))),
        "rmdir" | "rd" => lower
            .iter()
            .any(|word| word == "/s" || word == "--recursive"),
        "find" => lower
            .iter()
            .any(|word| word == "-delete" || word == "rm" || word.ends_with("/rm")),
        _ => false,
    }
}

/// The targets of a recursive delete, or `None` when `argv` is not one.
fn delete_targets(program: &str, argv: &[String]) -> Option<Vec<String>> {
    let rest = &argv[1..];
    let lower: Vec<String> = rest.iter().map(|word| word.to_lowercase()).collect();
    if !recursive(program, &lower) {
        return None;
    }
    if program == "find" {
        // `find` takes its options (`-H`, `-L`, `-P`, `-D …`, `-O3`), then
        // its paths before the first expression; no path is `.`.
        let mut words = rest.iter().peekable();
        while let Some(word) = words.next_if(|word| {
            matches!(word.as_str(), "-H" | "-L" | "-P" | "-D") || word.starts_with("-O")
        }) {
            if *word == "-D" {
                words.next();
            }
        }
        return Some(
            words
                .take_while(|word| !word.starts_with('-') && !matches!(word.as_str(), "(" | "!"))
                .cloned()
                .collect(),
        );
    }
    // `/s` and `/q` are switches of the Windows programs only: elsewhere a
    // word starting with `/` is an absolute path.
    let windows = matches!(program, "rmdir" | "rd" | "del" | "erase");
    let switch = |word: &String| {
        word.starts_with('-') || (windows && word.len() == 2 && word.starts_with('/'))
    };
    let mut after_dashes = false;
    Some(
        rest.iter()
            .filter(|word| {
                if after_dashes {
                    return true;
                }
                if word.as_str() == "--" {
                    after_dashes = true;
                    return false;
                }
                !switch(word)
            })
            .cloned()
            .collect(),
    )
}

/// Whether `target` lies outside the folder (or cannot be told inside it).
/// Only containment counts: a protected or denied path inside the folder is
/// inside, written absolute or relative.
fn outside(target: &str, moves: bool, root: &crate::workspace::Workspace) -> bool {
    let path = Path::new(target);
    if path.is_absolute() {
        return !contained(path, root.root());
    }
    // Rooted but not absolute: on Windows `/usr/local` or `\x` (no drive) is
    // the current drive's root, never a folder's inside.
    if target.starts_with(['/', '\\']) {
        return true;
    }
    // A drive path (`C:…`, its `\` possibly eaten by the shell's quoting) is
    // never inside a folder here; on Windows it is absolute, above.
    if moves || target.as_bytes().get(1) == Some(&b':') {
        return true;
    }
    // Relative: only an escape (`..`) leaves the folder.
    let mut depth: i64 = 0;
    for part in target.split(['/', '\\']) {
        match part {
            "" | "." => {}
            ".." => {
                depth -= 1;
                if depth < 0 {
                    return true;
                }
            }
            _ => depth += 1,
        }
    }
    false
}

/// Whether the absolute `path` lies under the canonical `root`: its `..`
/// folded, and its longest existing ancestor canonicalised (a `/tmp` that is
/// `/private/tmp`).
fn contained(path: &Path, root: &Path) -> bool {
    let mut folded = PathBuf::new();
    for component in path.components() {
        match component {
            Component::ParentDir => {
                folded.pop();
            }
            Component::CurDir => {}
            other => folded.push(other),
        }
    }
    let mut existing = folded.as_path();
    let mut rest = Vec::new();
    while !existing.exists() {
        let (Some(parent), Some(name)) = (existing.parent(), existing.file_name()) else {
            break;
        };
        rest.push(name.to_owned());
        existing = parent;
    }
    let Ok(mut resolved) = std::fs::canonicalize(existing) else {
        return false;
    };
    resolved.extend(rest.iter().rev());
    resolved.starts_with(root) && resolved != root
}

impl RulesEngine {
    /// [`super::PermissionProfile::FullAuto`]'s answer to an ask (`asked`, what
    /// the layers said): allowed, unless the call is on the hard-deny list
    /// ([`hold`]), which stays asked, or is declined in an unattended turn.
    pub(super) fn decide_full_auto(&self, call: &ToolCall, asked: Decision) -> Decision {
        if let Some(held) = hold(call, &self.policy_paths) {
            let verdict = if self.unattended().is_some() {
                Verdict::Deny
            } else {
                Verdict::Ask
            };
            return Decision {
                rule: asked.rule,
                ..Decision::new(verdict, Source::FullAutoHold, held.reason())
            };
        }
        let source = if asked.source == Source::Taint {
            Source::FullAutoTaint
        } else {
            Source::FullAuto
        };
        Decision {
            verdict: Verdict::Allow,
            source,
            reason: asked.reason,
            rule: asked.rule,
        }
    }
}
