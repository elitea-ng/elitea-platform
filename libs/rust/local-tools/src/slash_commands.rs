//! Custom slash commands: reusable prompt templates kept next to the code
//! (`.elitea/commands/*.md`, and Claude Code's `.claude/commands/*.md` for
//! compatibility) or in the person's own folder of the host's data.
//!
//! [`list`] discovers them, [`find`] reads one again at send time (never a
//! cached copy) and [`expand`] fills in the person's arguments. Nothing
//! else is done with a file: a template is text the host frames as
//! untrusted, never instructions, tools, a model or a command to run.
//!
//! **Where, in precedence order:** the workspace's `.elitea/commands/`,
//! the workspace's `.claude/commands/`, then the person's folder. Each is
//! read one level deep, plus one level of sub-folders whose files become
//! `folder:name`. The first command of a name wins: a workspace command
//! shadows the person's command of the same name.
//!
//! **Reads.** Workspace files go through the confined [`Workspace`]
//! (`openat` + `O_NOFOLLOW`, `path_deny` and the session's deny list): a
//! symlinked file or folder is not read, a denied one is skipped. The
//! person's folder is opened without following a symlink, and it, its
//! sub-folders and its files must be the person's own and writable by no
//! one else (another user could otherwise swap a template). A file is at
//! most [`MAX_FILE_BYTES`] and UTF-8; at most [`MAX_COMMANDS`] are listed.
//!
//! **Names** are the file stem lowercased, matching
//! `^[a-z0-9][a-z0-9_-]{0,63}$` (a sub-folder's name likewise); a file
//! whose name does not is skipped.
//!
//! **Front matter** (an optional `---` block at the top): only
//! `description` (at most [`MAX_DESCRIPTION_CHARS`]) and `argument-hint`
//! are read. Every other key (Claude Code's `allowed-tools`, `model`, …)
//! is ignored on purpose and listed in [`CommandEntry::ignored`], as are
//! `!`-prefixed shell lines and `@file` includes in the body: they stay
//! plain text, never run or read. A repository cannot grant tools, pick a
//! model or run a command through a template.

use std::fs::File;
use std::io::Read as _;
use std::os::fd::{AsFd, OwnedFd};
use std::os::unix::fs::MetadataExt as _;
use std::path::Path;

use rustix::fs::{Mode, OFlags};

use crate::workspace::{EntryKind, Intent, Workspace, WsPath};

/// The workspace folders read, in precedence order.
pub const WORKSPACE_FOLDERS: [&str; 2] = [".elitea/commands", ".claude/commands"];
/// The most commands listed.
pub const MAX_COMMANDS: usize = 200;
/// The most bytes of one command file.
pub const MAX_FILE_BYTES: u64 = 16 * 1024;
/// The longest description kept, in characters.
pub const MAX_DESCRIPTION_CHARS: usize = 200;
/// The longest argument hint kept, in characters.
pub const MAX_ARGUMENT_HINT_CHARS: usize = 200;
/// The longest name part (a file stem or a sub-folder), in bytes.
pub const MAX_NAME_PART: usize = 64;
/// The most entries of one folder looked at (a huge folder is not walked).
const MAX_FOLDER_ENTRIES: usize = 1000;

/// Where a command comes from.
#[derive(Clone, Copy, Debug, PartialEq, Eq, serde::Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Source {
    /// The workspace's `.elitea/commands/` or `.claude/commands/`.
    Workspace,
    /// The person's own folder in the host's data.
    User,
}

impl Source {
    #[must_use]
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Workspace => "workspace",
            Self::User => "user",
        }
    }
}

/// One command, read.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CommandEntry {
    /// `name`, or `folder:name` for a file in a sub-folder.
    pub name: String,
    /// From the front matter, else the template's first line; at most
    /// [`MAX_DESCRIPTION_CHARS`].
    pub description: Option<String>,
    pub argument_hint: Option<String>,
    pub source: Source,
    /// Workspace-relative for a workspace command (`.claude/commands/x.md`),
    /// relative to the person's folder for theirs (`x.md`).
    pub path: String,
    /// The text after the front matter, unchanged.
    pub template: String,
    /// What the file asks for that is ignored: front matter keys, then
    /// `!` shell lines and `@` file includes when it has any.
    pub ignored: Vec<String>,
}

/// What [`list`] found.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Listing {
    /// By name.
    pub commands: Vec<CommandEntry>,
    /// Found but not listed, with why (a symlink, denied, too large, a bad
    /// name, shadowed, past the limit).
    pub skipped: Vec<(String, String)>,
}

/// Whether `part` is a valid name part (already lowercased).
#[must_use]
pub fn is_name_part(part: &str) -> bool {
    let bytes = part.as_bytes();
    !bytes.is_empty()
        && bytes.len() <= MAX_NAME_PART
        && (bytes[0].is_ascii_lowercase() || bytes[0].is_ascii_digit())
        && bytes
            .iter()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || *b == b'_' || *b == b'-')
}

/// Whether `name` is a valid command name: a name part, or two joined by
/// `:`.
#[must_use]
pub fn is_command_name(name: &str) -> bool {
    match name.split_once(':') {
        Some((folder, file)) => is_name_part(folder) && is_name_part(file),
        None => is_name_part(name),
    }
}

/// The command name of a file `stem.md` (any case of `.md`), or `None`.
fn stem_of(file_name: &str) -> Option<String> {
    let cut = file_name.len().checked_sub(3)?;
    if !file_name.is_char_boundary(cut) || !file_name[cut..].eq_ignore_ascii_case(".md") {
        return None;
    }
    Some(file_name[..cut].to_lowercase())
}

/// One file found in a folder, before it is read.
struct Found {
    name: String,
    shown: String,
}

/// The workspace's command files, in precedence order.
fn workspace_files(workspace: &Workspace, out: &mut Listing) -> Vec<(Found, WsPath)> {
    let mut found = Vec::new();
    for folder in WORKSPACE_FOLDERS {
        let Ok(dir) = WsPath::from_relative(Path::new(folder)) else {
            continue;
        };
        for (names, path) in workspace_dir(workspace, &dir, out) {
            match names {
                Entry::File(stem) => found.push((
                    Found {
                        name: stem,
                        shown: path.display_string(),
                    },
                    path,
                )),
                Entry::Folder(prefix) => {
                    for (inner, inner_path) in workspace_dir(workspace, &path, out) {
                        if let Entry::File(stem) = inner {
                            found.push((
                                Found {
                                    name: format!("{prefix}:{stem}"),
                                    shown: inner_path.display_string(),
                                },
                                inner_path,
                            ));
                        }
                    }
                }
            }
        }
    }
    found
}

enum Entry {
    File(String),
    Folder(String),
}

fn child(dir: &WsPath, name: &str) -> Option<WsPath> {
    let mut parts: Vec<&str> = dir.components().iter().map(String::as_str).collect();
    parts.push(name);
    WsPath::from_relative(Path::new(&parts.join("/"))).ok()
}

/// The `.md` files and sub-folders of `dir`, by name. Nothing when `dir`
/// is denied, reached through a symlink or not a folder.
fn workspace_dir(workspace: &Workspace, dir: &WsPath, out: &mut Listing) -> Vec<(Entry, WsPath)> {
    let shown = dir.display_string();
    if let Some(rule) = workspace.denied_by(dir, Intent::Read) {
        out.skipped.push((shown, format!("denied by {rule}")));
        return Vec::new();
    }
    match workspace.stat(dir) {
        Ok(Some(EntryKind::Dir)) => {}
        Ok(Some(EntryKind::Symlink)) => {
            out.skipped.push((shown, "a symlink".to_owned()));
            return Vec::new();
        }
        _ => return Vec::new(),
    }
    // A folder on the way (`.claude`) may itself be a symlink.
    if workspace.final_target(dir).ok().as_ref() != Some(dir) {
        out.skipped.push((shown, "a symlink".to_owned()));
        return Vec::new();
    }
    let Ok(listing) = std::fs::read_dir(workspace.absolute(dir)) else {
        return Vec::new();
    };
    let mut names: Vec<String> = listing
        .take(MAX_FOLDER_ENTRIES)
        .flatten()
        .filter_map(|entry| entry.file_name().into_string().ok())
        .collect();
    names.sort();
    let mut entries = Vec::new();
    for name in names {
        let Some(path) = child(dir, &name) else {
            continue;
        };
        match workspace.stat(&path) {
            Ok(Some(EntryKind::File)) => {}
            Ok(Some(EntryKind::Dir)) => {
                let folder = name.to_lowercase();
                if is_name_part(&folder) {
                    entries.push((Entry::Folder(folder), path));
                }
                continue;
            }
            Ok(Some(EntryKind::Symlink)) => {
                if stem_of(&name).is_some() {
                    out.skipped
                        .push((path.display_string(), "a symlink".to_owned()));
                }
                continue;
            }
            _ => continue,
        }
        let Some(stem) = stem_of(&name) else {
            continue;
        };
        if !is_name_part(&stem) {
            out.skipped
                .push((path.display_string(), "not a valid command name".to_owned()));
            continue;
        }
        entries.push((Entry::File(stem), path));
    }
    entries
}

/// Read a workspace command file, or say why not.
fn read_workspace_file(workspace: &Workspace, path: &WsPath) -> Result<Vec<u8>, String> {
    if let Some(rule) = workspace.denied_by(path, Intent::Read) {
        return Err(format!("denied by {rule}"));
    }
    match workspace.stat(path) {
        Ok(Some(EntryKind::File)) => {}
        Ok(Some(EntryKind::Symlink)) => return Err("a symlink".to_owned()),
        _ => return Err("not a regular file".to_owned()),
    }
    let (file, resolved) = workspace
        .open_file(path)
        .map_err(|error| error.message().to_owned())?;
    if resolved != *path {
        return Err("a symlink".to_owned());
    }
    read_capped(file)
}

fn read_capped(file: File) -> Result<Vec<u8>, String> {
    let too_large = || format!("over {} KiB", MAX_FILE_BYTES / 1024);
    let metadata = file.metadata().map_err(|_| "unreadable".to_owned())?;
    if metadata.len() > MAX_FILE_BYTES {
        return Err(too_large());
    }
    let mut bytes = Vec::new();
    file.take(MAX_FILE_BYTES + 1)
        .read_to_end(&mut bytes)
        .map_err(|_| "unreadable".to_owned())?;
    if bytes.len() as u64 > MAX_FILE_BYTES {
        return Err(too_large());
    }
    Ok(bytes)
}

/// The person's own: their uid, and writable by no one else.
fn owned_privately(metadata: &std::fs::Metadata) -> bool {
    metadata.uid() == rustix::process::geteuid().as_raw() && metadata.mode() & 0o022 == 0
}

fn open_at(dir: &impl AsFd, name: &str, flags: OFlags) -> Option<OwnedFd> {
    rustix::fs::openat(
        dir,
        name,
        flags | OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::CLOEXEC | OFlags::NONBLOCK,
        Mode::empty(),
    )
    .ok()
}

/// A folder of the person's, opened without following a symlink, if it is
/// their own and writable by no one else.
fn private_dir(fd: Option<OwnedFd>) -> Option<OwnedFd> {
    let fd = fd?;
    let metadata = File::from(fd.try_clone().ok()?).metadata().ok()?;
    (metadata.is_dir() && owned_privately(&metadata)).then_some(fd)
}

/// The names in the folder `fd`, sorted.
fn names_in(fd: &OwnedFd) -> Vec<String> {
    let Ok(dir) = rustix::fs::Dir::read_from(fd) else {
        return Vec::new();
    };
    let mut names: Vec<String> = dir
        .take(MAX_FOLDER_ENTRIES)
        .flatten()
        .filter_map(|entry| entry.file_name().to_str().ok().map(str::to_owned))
        .filter(|name| name != "." && name != "..")
        .collect();
    names.sort();
    names
}

/// Read a file of the person's folder, or say why not.
fn read_user_file(dir: &OwnedFd, name: &str) -> Result<Vec<u8>, String> {
    let Some(fd) = open_at(dir, name, OFlags::empty()) else {
        return Err("a symlink or unreadable".to_owned());
    };
    let file = File::from(fd);
    let metadata = file.metadata().map_err(|_| "unreadable".to_owned())?;
    if !metadata.is_file() {
        return Err("not a regular file".to_owned());
    }
    if !owned_privately(&metadata) {
        return Err("another user's, or writable by others".to_owned());
    }
    read_capped(file)
}

/// The person's command files, each read (or the reason it was not).
fn user_files(user_dir: &Path) -> Vec<(Found, Result<Vec<u8>, String>)> {
    let root = rustix::fs::open(
        user_dir,
        OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .ok();
    let Some(root) = private_dir(root) else {
        return Vec::new();
    };
    let mut found = Vec::new();
    for name in names_in(&root) {
        if let Some(stem) = stem_of(&name) {
            if is_name_part(&stem) {
                let read = read_user_file(&root, &name);
                found.push((
                    Found {
                        name: stem,
                        shown: name,
                    },
                    read,
                ));
            }
            continue;
        }
        let folder = name.to_lowercase();
        if !is_name_part(&folder) {
            continue;
        }
        let Some(sub) = private_dir(open_at(&root, &name, OFlags::DIRECTORY)) else {
            continue;
        };
        for inner in names_in(&sub) {
            let Some(stem) = stem_of(&inner).filter(|stem| is_name_part(stem)) else {
                continue;
            };
            let read = read_user_file(&sub, &inner);
            found.push((
                Found {
                    name: format!("{folder}:{stem}"),
                    shown: format!("{name}/{inner}"),
                },
                read,
            ));
        }
    }
    found
}

/// `value` without surrounding blanks and one pair of matching quotes,
/// control characters as spaces, cut at `max` characters.
fn clean_value(value: &str, max: usize) -> Option<String> {
    let value = value.trim();
    let value = ['"', '\'']
        .iter()
        .find_map(|quote| {
            value
                .strip_prefix(*quote)
                .and_then(|rest| rest.strip_suffix(*quote))
        })
        .unwrap_or(value);
    let cleaned: String = value
        .chars()
        .map(|c| if c.is_control() { ' ' } else { c })
        .take(max)
        .collect();
    let cleaned = cleaned.trim().to_owned();
    (!cleaned.is_empty()).then_some(cleaned)
}

/// What one file holds.
struct Parsed {
    description: Option<String>,
    argument_hint: Option<String>,
    template: String,
    ignored: Vec<String>,
}

/// Split a file into its front matter (flat `key: value` lines between
/// `---` lines at the very top) and its template.
fn parse(text: &str) -> Parsed {
    let text = text.strip_prefix('\u{feff}').unwrap_or(text);
    let mut description = None;
    let mut argument_hint = None;
    let mut ignored: Vec<String> = Vec::new();
    let mut template = text;
    let first_end = text.find('\n').unwrap_or(text.len());
    if text[..first_end].trim_end() == "---" {
        let rest = &text[(first_end + 1).min(text.len())..];
        let mut offset = 0;
        let mut closed = None;
        for line in rest.split_inclusive('\n') {
            if line.trim_end() == "---" {
                closed = Some(offset + line.len());
                break;
            }
            offset += line.len();
        }
        if let Some(end) = closed {
            for line in rest[..offset].lines() {
                // Nested values and list items belong to the key above.
                if line.starts_with([' ', '\t', '-', '#']) || line.trim().is_empty() {
                    continue;
                }
                let Some((key, value)) = line.split_once(':') else {
                    continue;
                };
                match key.trim() {
                    "description" => {
                        description = clean_value(value, MAX_DESCRIPTION_CHARS);
                    }
                    "argument-hint" => {
                        argument_hint = clean_value(value, MAX_ARGUMENT_HINT_CHARS);
                    }
                    other => {
                        let other = other.chars().take(64).collect::<String>();
                        if !other.is_empty() && !ignored.contains(&other) {
                            ignored.push(other);
                        }
                    }
                }
            }
            template = &rest[end..];
        }
    }
    let template = template.trim_start_matches(['\r', '\n']).to_owned();
    if template
        .lines()
        .any(|line| line.trim_start().starts_with('!'))
        || template.contains("!`")
    {
        ignored.push("! shell lines".to_owned());
    }
    if template.split_whitespace().any(|word| {
        word.strip_prefix('@').is_some_and(|rest| {
            rest.starts_with(|c: char| c.is_alphanumeric() || "./_~".contains(c))
        })
    }) {
        ignored.push("@ file includes".to_owned());
    }
    let description = description.or_else(|| {
        template
            .lines()
            .map(|line| line.trim().trim_start_matches('#').trim())
            .find(|line| !line.is_empty())
            .and_then(|line| clean_value(line, MAX_DESCRIPTION_CHARS))
    });
    Parsed {
        description,
        argument_hint,
        template,
        ignored,
    }
}

fn entry(found: Found, source: Source, bytes: &[u8]) -> Result<CommandEntry, String> {
    let text = std::str::from_utf8(bytes).map_err(|_| "not UTF-8".to_owned())?;
    let parsed = parse(text);
    Ok(CommandEntry {
        name: found.name,
        description: parsed.description,
        argument_hint: parsed.argument_hint,
        source,
        path: found.shown,
        template: parsed.template,
        ignored: parsed.ignored,
    })
}

/// Every command, by name: the workspace's (`workspace`, when given) then
/// the person's (`user_dir`, when given), the first of a name winning.
#[must_use]
pub fn list(workspace: Option<&Workspace>, user_dir: Option<&Path>) -> Listing {
    let mut out = Listing::default();
    let mut read: Vec<(Found, Source, Result<Vec<u8>, String>)> = Vec::new();
    if let Some(workspace) = workspace {
        for (found, path) in workspace_files(workspace, &mut out) {
            let bytes = read_workspace_file(workspace, &path);
            read.push((found, Source::Workspace, bytes));
        }
    }
    if let Some(user_dir) = user_dir {
        for (found, bytes) in user_files(user_dir) {
            read.push((found, Source::User, bytes));
        }
    }
    let mut commands: Vec<CommandEntry> = Vec::new();
    for (found, source, bytes) in read {
        if let Some(winner) = commands.iter().find(|command| command.name == found.name) {
            let why = format!("shadowed by {} ({})", winner.path, winner.source.as_str());
            out.skipped.push((found.shown, why));
            continue;
        }
        if commands.len() >= MAX_COMMANDS {
            out.skipped.push((
                found.shown,
                format!("past the {MAX_COMMANDS}-command limit"),
            ));
            continue;
        }
        let shown = found.shown.clone();
        match bytes.and_then(|bytes| entry(found, source, &bytes)) {
            Ok(command) => commands.push(command),
            Err(why) => out.skipped.push((shown, why)),
        }
    }
    commands.sort_by(|a, b| a.name.cmp(&b.name));
    out.commands = commands;
    out
}

/// The command `name`, read again now (as [`list`] would list it).
#[must_use]
pub fn find(
    workspace: Option<&Workspace>,
    user_dir: Option<&Path>,
    name: &str,
) -> Option<CommandEntry> {
    if !is_command_name(name) {
        return None;
    }
    list(workspace, user_dir)
        .commands
        .into_iter()
        .find(|command| command.name == name)
}

/// The person's arguments as positional words: split as a shell would
/// (quotes group words), or on blanks when the quotes do not balance.
fn positional(arguments: &str) -> Vec<String> {
    shlex::split(arguments)
        .unwrap_or_else(|| arguments.split_whitespace().map(str::to_owned).collect())
}

/// The template with `$ARGUMENTS` replaced by all of `arguments` (trimmed)
/// and `$1`…`$9` by its positional words (empty when absent), in one pass:
/// replaced text is never expanded again. Nothing else is expanded.
#[must_use]
pub fn expand(entry: &CommandEntry, arguments: &str) -> String {
    const ALL: &str = "$ARGUMENTS";
    let arguments = arguments.trim();
    let words = positional(arguments);
    let template = entry.template.as_str();
    let mut out = String::with_capacity(template.len() + arguments.len());
    let mut rest = template;
    while let Some(at) = rest.find('$') {
        out.push_str(&rest[..at]);
        let tail = &rest[at..];
        if let Some(after) = tail.strip_prefix(ALL) {
            out.push_str(arguments);
            rest = after;
            continue;
        }
        if let Some(digit @ b'1'..=b'9') = tail.as_bytes().get(1) {
            if let Some(word) = words.get(usize::from(digit - b'1')) {
                out.push_str(word);
            }
            rest = &tail[2..];
        } else {
            out.push('$');
            rest = &tail[1..];
        }
    }
    out.push_str(rest);
    out
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::os::unix::fs::PermissionsExt as _;

    use super::*;

    fn open(dir: &tempfile::TempDir, deny: &[&str]) -> Workspace {
        let deny: Vec<String> = deny.iter().map(|s| (*s).to_owned()).collect();
        Workspace::open(dir.path(), &deny).unwrap()
    }

    fn write(dir: &Path, path: &str, text: &str) {
        let path = dir.join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, text).unwrap();
    }

    /// The person's folder as the host creates it: `0700`.
    fn user_dir() -> tempfile::TempDir {
        let dir = tempfile::tempdir().unwrap();
        fs::set_permissions(dir.path(), fs::Permissions::from_mode(0o700)).unwrap();
        dir
    }

    fn names(listing: &Listing) -> Vec<(&str, Source)> {
        listing
            .commands
            .iter()
            .map(|c| (c.name.as_str(), c.source))
            .collect()
    }

    #[test]
    fn discovers_elitea_and_claude_command_folders() {
        let dir = tempfile::tempdir().unwrap();
        write(
            dir.path(),
            ".elitea/commands/review.md",
            "---\ndescription: Review the diff\nargument-hint: \"[files]\"\n---\nReview $ARGUMENTS.\n",
        );
        write(
            dir.path(),
            ".claude/commands/Fix-Issue.MD",
            "# Fix an issue\nFix #$1.",
        );
        write(dir.path(), ".claude/commands/review.md", "the other review");
        write(dir.path(), ".claude/commands/frontend/lint.md", "Lint it.");
        write(
            dir.path(),
            ".claude/commands/frontend/deep/no.md",
            "too deep",
        );
        write(dir.path(), ".claude/commands/notes.txt", "not markdown");
        write(dir.path(), ".claude/commands/bad name.md", "bad");
        let listing = list(Some(&open(&dir, &[])), None);
        assert_eq!(
            names(&listing),
            [
                ("fix-issue", Source::Workspace),
                ("frontend:lint", Source::Workspace),
                ("review", Source::Workspace),
            ]
        );
        let review = &listing.commands[2];
        assert_eq!(review.path, ".elitea/commands/review.md");
        assert_eq!(review.description.as_deref(), Some("Review the diff"));
        assert_eq!(review.argument_hint.as_deref(), Some("[files]"));
        assert_eq!(review.template, "Review $ARGUMENTS.\n");
        assert!(review.ignored.is_empty(), "{review:?}");
        // No front matter: the first line describes it.
        assert_eq!(
            listing.commands[0].description.as_deref(),
            Some("Fix an issue")
        );
        assert!(listing.skipped.contains(&(
            ".claude/commands/review.md".to_owned(),
            "shadowed by .elitea/commands/review.md (workspace)".to_owned()
        )));
        assert!(listing.skipped.contains(&(
            ".claude/commands/bad name.md".to_owned(),
            "not a valid command name".to_owned()
        )));
        assert!(list(None, None).commands.is_empty());
    }

    #[test]
    fn workspace_command_shadows_user_command() {
        let dir = tempfile::tempdir().unwrap();
        write(dir.path(), ".claude/commands/review.md", "workspace review");
        let user = user_dir();
        write(user.path(), "review.md", "my review");
        write(user.path(), "standup.md", "my standup");
        write(user.path(), "team/sync.md", "my sync");
        let listing = list(Some(&open(&dir, &[])), Some(user.path()));
        assert_eq!(
            names(&listing),
            [
                ("review", Source::Workspace),
                ("standup", Source::User),
                ("team:sync", Source::User),
            ]
        );
        assert_eq!(listing.commands[0].template, "workspace review");
        assert_eq!(listing.commands[2].path, "team/sync.md");
        assert_eq!(
            listing.skipped,
            [(
                "review.md".to_owned(),
                "shadowed by .claude/commands/review.md (workspace)".to_owned()
            )]
        );
        assert_eq!(
            find(Some(&open(&dir, &[])), Some(user.path()), "review")
                .unwrap()
                .source,
            Source::Workspace
        );
        assert!(find(None, Some(user.path()), "nope").is_none());
        assert!(find(None, Some(user.path()), "../review").is_none());
    }

    #[test]
    fn a_user_folder_or_file_others_can_write_is_not_read() {
        let user = user_dir();
        write(user.path(), "mine.md", "mine");
        write(user.path(), "shared.md", "shared");
        fs::set_permissions(
            user.path().join("shared.md"),
            fs::Permissions::from_mode(0o666),
        )
        .unwrap();
        let listing = list(None, Some(user.path()));
        assert_eq!(names(&listing), [("mine", Source::User)]);
        assert_eq!(listing.skipped[0].0, "shared.md");
        fs::set_permissions(user.path(), fs::Permissions::from_mode(0o777)).unwrap();
        assert!(list(None, Some(user.path())).commands.is_empty());
        // The folder itself reached through a symlink is not read either.
        fs::set_permissions(user.path(), fs::Permissions::from_mode(0o700)).unwrap();
        let other = tempfile::tempdir().unwrap();
        let link = other.path().join("commands");
        std::os::unix::fs::symlink(user.path(), &link).unwrap();
        assert!(list(None, Some(&link)).commands.is_empty());
    }

    #[test]
    fn symlinked_command_file_is_not_read() {
        let outside = tempfile::tempdir().unwrap();
        write(outside.path(), "evil.md", "OUTSIDE");
        let dir = tempfile::tempdir().unwrap();
        write(dir.path(), ".claude/commands/ok.md", "fine");
        write(dir.path(), "inside.md", "INSIDE");
        std::os::unix::fs::symlink(
            outside.path().join("evil.md"),
            dir.path().join(".claude/commands/evil.md"),
        )
        .unwrap();
        std::os::unix::fs::symlink(
            dir.path().join("inside.md"),
            dir.path().join(".claude/commands/inside.md"),
        )
        .unwrap();
        // A symlinked folder: `.elitea` leads out of the workspace.
        std::os::unix::fs::symlink(outside.path(), dir.path().join(".elitea")).unwrap();
        fs::create_dir_all(outside.path().join("commands")).unwrap();
        fs::write(outside.path().join("commands/out.md"), "OUTSIDE").unwrap();
        let listing = list(Some(&open(&dir, &[])), None);
        assert_eq!(names(&listing), [("ok", Source::Workspace)]);
        assert!(!format!("{listing:?}").contains("OUTSIDE"));
        assert!(!format!("{listing:?}").contains("INSIDE"));
        for path in [".claude/commands/evil.md", ".claude/commands/inside.md"] {
            assert!(
                listing
                    .skipped
                    .contains(&(path.to_owned(), "a symlink".to_owned())),
                "{listing:?}"
            );
        }
        // In the person's folder too.
        let user = user_dir();
        std::os::unix::fs::symlink(outside.path().join("evil.md"), user.path().join("evil.md"))
            .unwrap();
        let listing = list(None, Some(user.path()));
        assert!(listing.commands.is_empty(), "{listing:?}");
    }

    #[test]
    fn path_deny_command_file_is_skipped() {
        let dir = tempfile::tempdir().unwrap();
        write(dir.path(), ".claude/commands/secret.md", "SECRET");
        write(dir.path(), ".claude/commands/ok.md", "fine");
        write(dir.path(), ".elitea/commands/x.md", "DENIED FOLDER");
        let listing = list(Some(&open(&dir, &["secret.md", ".elitea/**"])), None);
        assert_eq!(names(&listing), [("ok", Source::Workspace)]);
        assert!(!format!("{listing:?}").contains("SECRET"));
        assert!(!format!("{listing:?}").contains("DENIED FOLDER"));
        assert_eq!(listing.skipped.len(), 2, "{listing:?}");
        assert!(
            listing
                .skipped
                .iter()
                .all(|(_, why)| why.starts_with("denied by"))
        );
    }

    #[test]
    fn an_oversize_or_binary_file_is_skipped() {
        let dir = tempfile::tempdir().unwrap();
        write(
            dir.path(),
            ".claude/commands/big.md",
            &"a".repeat(usize::try_from(MAX_FILE_BYTES).unwrap() + 1),
        );
        write(
            dir.path(),
            ".claude/commands/edge.md",
            &"a".repeat(usize::try_from(MAX_FILE_BYTES).unwrap()),
        );
        fs::write(
            dir.path().join(".claude/commands/bin.md"),
            [0xff, 0xfe, 0x00],
        )
        .unwrap();
        let listing = list(Some(&open(&dir, &[])), None);
        assert_eq!(names(&listing), [("edge", Source::Workspace)]);
        assert!(listing.skipped.contains(&(
            ".claude/commands/big.md".to_owned(),
            "over 16 KiB".to_owned()
        )));
        assert!(
            listing
                .skipped
                .contains(&(".claude/commands/bin.md".to_owned(), "not UTF-8".to_owned()))
        );
    }

    #[test]
    fn at_most_the_limit_is_listed() {
        let dir = tempfile::tempdir().unwrap();
        for i in 0..=MAX_COMMANDS {
            write(dir.path(), &format!(".claude/commands/c{i:03}.md"), "x");
        }
        let listing = list(Some(&open(&dir, &[])), None);
        assert_eq!(listing.commands.len(), MAX_COMMANDS);
        assert_eq!(listing.skipped.len(), 1);
    }

    #[test]
    fn front_matter_allowed_tools_is_ignored() {
        let parsed = parse(
            "---\nallowed-tools: Bash(*), Write\nmodel: claude-opus\ndescription: 'Ship it'\n\
             permissions:\n  allow: [\"*\"]\nallowed-tools: again\n---\nDo it.\n",
        );
        assert_eq!(parsed.description.as_deref(), Some("Ship it"));
        assert_eq!(parsed.ignored, ["allowed-tools", "model", "permissions"]);
        assert_eq!(parsed.template, "Do it.\n");
        // An unclosed block is not front matter: all of it is the template.
        let open = parse("---\nmodel: x\nDo it.");
        assert_eq!(open.template, "---\nmodel: x\nDo it.");
        assert!(open.ignored.is_empty());
        // A long description is cut; control characters become spaces.
        let long = parse(&format!(
            "---\ndescription: a\u{7}{}\n---\n",
            "b".repeat(300)
        ));
        let description = long.description.unwrap();
        assert_eq!(description.chars().count(), MAX_DESCRIPTION_CHARS);
        assert!(description.starts_with("a b"));
    }

    #[test]
    fn bang_lines_are_not_executed_or_expanded() {
        let dir = tempfile::tempdir().unwrap();
        let marker = dir.path().join("ran");
        let template = format!(
            "Context: !`touch {}`\n!touch {}\nSee @src/main.rs and mail me@example.com.\n$ARGUMENTS",
            marker.display(),
            marker.display()
        );
        write(dir.path(), ".claude/commands/ctx.md", &template);
        let listing = list(Some(&open(&dir, &[])), None);
        let command = &listing.commands[0];
        assert_eq!(command.ignored, ["! shell lines", "@ file includes"]);
        let expanded = expand(command, "now");
        assert_eq!(expanded, template.replace("$ARGUMENTS", "now"));
        assert!(!marker.exists(), "nothing ran");
        assert!(parse("plain text, mail me@example.com").ignored.is_empty());
    }

    #[test]
    fn arguments_expand_positional_and_all() {
        let command = CommandEntry {
            name: "fix".into(),
            description: None,
            argument_hint: None,
            source: Source::User,
            path: "fix.md".into(),
            template: "Fix $1 in \"$2\" ($3): $ARGUMENTS; cost $5 $$ $x $".into(),
            ignored: Vec::new(),
        };
        assert_eq!(
            expand(&command, "  42 'src/a b.rs'  "),
            "Fix 42 in \"src/a b.rs\" (): 42 'src/a b.rs'; cost  $$ $x $"
        );
        // Replaced text is not expanded again.
        assert_eq!(
            expand(&command, "$2 $ARGUMENTS"),
            "Fix $2 in \"$ARGUMENTS\" (): $2 $ARGUMENTS; cost  $$ $x $"
        );
        // Unbalanced quotes split on blanks.
        assert!(expand(&command, "it's x").starts_with("Fix it's in \"x\""));
    }

    #[test]
    fn names_are_checked() {
        for good in ["a", "review", "fix-issue_2", "ns:name", &"a".repeat(64)] {
            assert!(is_command_name(good), "{good}");
        }
        for bad in [
            "",
            "-a",
            "_a",
            "A",
            "a b",
            "a:b:c",
            ":a",
            "a:",
            "../x",
            &"a".repeat(65),
            "é",
        ] {
            assert!(!is_command_name(bad), "{bad}");
        }
    }
}
