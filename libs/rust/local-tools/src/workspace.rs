//! The workspace: one folder the person opened, and the only place local
//! file tools read or write.
//!
//! Confinement is two layers:
//!
//! 1. **Lexical.** Every path argument is normalised against the
//!    canonical root (realpath). `..` that climbs above the root, and an
//!    absolute path that is not under the root (under any spelling of it the
//!    person used: `/tmp/x` and `/private/tmp/x` on macOS), are refused.
//!    `path_deny` globs and the protected `.git` directories are checked on
//!    every prefix of the result.
//! 2. **Physical.** No file operation ever passes a path string to the
//!    kernel. It walks from a descriptor of the root, one component at a
//!    time, with `openat(O_NOFOLLOW | O_DIRECTORY)`, then opens, creates
//!    (`O_EXCL`), renames or unlinks relative to the last directory
//!    descriptor. A symlink met on the way is read and followed only when its
//!    target, normalised the same way, stays inside the workspace; the walk
//!    then restarts from the root. A symlink swapped in between the check
//!    and the use therefore cannot redirect an operation outside: the
//!    descriptor already held is the directory that was checked.
//!
//! Hard links are not followed or refused: a write replaces the directory
//! entry (temporary file + `renameat`), so it never writes through a link
//! into a file elsewhere.

use std::collections::VecDeque;
use std::fmt;
use std::fs::File;
use std::io::{Read, Write};
use std::os::fd::OwnedFd;
use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::path::{Component, Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

use globset::{GlobBuilder, GlobSet, GlobSetBuilder};
use rustix::fs::{AtFlags, FileType, Mode, OFlags};
use rustix::io::Errno;
use sha2::{Digest, Sha256};
use unicode_normalization::UnicodeNormalization;

use crate::error::{ErrorCode, ToolError, ToolResult};

/// The most symlinks one resolution follows before giving up (Linux's
/// `MAXSYMLINKS` is 40).
const MAX_SYMLINK_HOPS: usize = 32;

/// The directory name no agent write may touch, at any depth: git's own
/// state (hooks and config there run code). Compared with
/// [`is_protected_name`], never with `==`.
pub const PROTECTED_DIR: &str = ".git";

/// Whether file systems that matter here treat `name` as [`PROTECTED_DIR`].
///
/// APFS (the macOS default) and NTFS are case-insensitive, APFS is also
/// normalisation-insensitive, and HFS+ ignores some zero-width code points
/// (the git CVE-2014-9390 family): `.GIT`, `.Git` and `.g\u{200c}it` all
/// name the same directory there. Lowercasing everywhere costs nothing on a
/// case-sensitive file system (no agent needs to write a `.GIT`).
#[must_use]
pub fn is_protected_name(name: &str) -> bool {
    let folded: String = name
        .nfc()
        .filter(|c| !is_hfs_ignorable(*c))
        .flat_map(char::to_lowercase)
        .collect();
    folded == PROTECTED_DIR
}

/// Code points HFS+ drops when comparing names (git's `is_hfs_dotgit` list).
const fn is_hfs_ignorable(c: char) -> bool {
    matches!(
        c,
        '\u{200c}'..='\u{200f}' | '\u{202a}'..='\u{202e}' | '\u{206a}'..='\u{206f}' | '\u{feff}'
    )
}

/// Whether this OS's default file system compares names case-insensitively
/// (path globs follow it).
pub(crate) const CASE_INSENSITIVE_FS: bool = cfg!(any(target_os = "macos", target_os = "windows"));

/// The `path_deny` patterns as one glob set over NFC `/`-joined workspace
/// paths (see [`Workspace::open`] for the pattern rules).
///
/// # Errors
///
/// When a pattern is not a valid glob.
pub fn deny_globset(path_deny: &[String]) -> ToolResult<GlobSet> {
    let mut builder = GlobSetBuilder::new();
    for pattern in path_deny {
        let normalised = nfc(pattern);
        let trimmed = normalised.trim().trim_start_matches("./");
        if trimmed.is_empty() {
            continue;
        }
        let anchored = if trimmed.contains('/') {
            trimmed.trim_start_matches('/').to_owned()
        } else {
            format!("**/{trimmed}")
        };
        for candidate in [anchored.as_str(), trimmed] {
            let glob = GlobBuilder::new(candidate)
                .case_insensitive(CASE_INSENSITIVE_FS)
                .build()
                .map_err(|_| {
                    ToolError::invalid(format!("path_deny pattern `{pattern}` is not a glob"))
                })?;
            builder.add(glob);
        }
    }
    builder
        .build()
        .map_err(|_| ToolError::invalid("path_deny patterns do not compile"))
}

/// `text` in NFC: how path rules compare names, so an NFD spelling (what
/// macOS keyboards and older HFS+ produce) matches an NFC pattern.
pub(crate) fn nfc(text: &str) -> String {
    text.nfc().collect()
}

static TEMP_COUNTER: AtomicU64 = AtomicU64::new(0);

/// What a resolution is for: writes are additionally refused inside
/// [`PROTECTED_DIR`].
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Intent {
    Read,
    Write,
}

/// A path inside the workspace: normalised UTF-8 components, relative to
/// the root. The empty path is the root itself.
#[derive(Clone, Debug, Default, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct WsPath(Vec<String>);

impl WsPath {
    /// The workspace root.
    #[must_use]
    pub fn root() -> Self {
        Self(Vec::new())
    }

    #[must_use]
    pub fn components(&self) -> &[String] {
        &self.0
    }

    #[must_use]
    pub fn is_root(&self) -> bool {
        self.0.is_empty()
    }

    /// The last component, if any.
    #[must_use]
    pub fn file_name(&self) -> Option<&str> {
        self.0.last().map(String::as_str)
    }

    /// The path with forward slashes (`.` for the root): how tools show it.
    #[must_use]
    pub fn display_string(&self) -> String {
        if self.0.is_empty() {
            ".".to_owned()
        } else {
            self.0.join("/")
        }
    }

    /// Build from a relative path (as a directory walk or git yields one),
    /// lexically, without touching the file system.
    ///
    /// # Errors
    ///
    /// When it is absolute, climbs out with `..`, or is not UTF-8.
    pub fn from_relative(path: &Path) -> ToolResult<Self> {
        let mut out = Vec::new();
        push_components(&mut out, path)?;
        Ok(Self(out))
    }

    fn join_relative(&self, path: &Path) -> ToolResult<Self> {
        let mut out = self.0.clone();
        push_components(&mut out, path)?;
        Ok(Self(out))
    }
}

impl fmt::Display for WsPath {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.display_string())
    }
}

fn outside() -> ToolError {
    ToolError::new(
        ErrorCode::OutsideWorkspace,
        "the path resolves outside the workspace",
    )
}

fn push_components(out: &mut Vec<String>, path: &Path) -> ToolResult<()> {
    for component in path.components() {
        match component {
            Component::CurDir => {}
            Component::ParentDir => {
                if out.pop().is_none() {
                    return Err(outside());
                }
            }
            Component::Normal(name) => {
                let name = name
                    .to_str()
                    .ok_or_else(|| ToolError::invalid("path components must be UTF-8"))?;
                out.push(name.to_owned());
            }
            Component::RootDir | Component::Prefix(_) => return Err(outside()),
        }
    }
    Ok(())
}

/// Lexically normalise an absolute path (no file system access).
fn normalise_absolute(path: &Path) -> Option<PathBuf> {
    let mut out = PathBuf::from("/");
    for component in path.components() {
        match component {
            Component::RootDir | Component::CurDir => {}
            Component::ParentDir => {
                out.pop();
            }
            Component::Normal(name) => out.push(name),
            Component::Prefix(_) => return None,
        }
    }
    Some(out)
}

/// What [`Workspace::stat`] reports about one entry.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum EntryKind {
    File,
    Dir,
    Symlink,
    Other,
}

/// Identity of a file's content as of one read: what the read-before-write
/// guard compares.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FileStamp {
    pub len: u64,
    /// Modification time, nanoseconds since the epoch.
    pub mtime_ns: i128,
    pub sha256: [u8; 32],
}

impl FileStamp {
    fn of(bytes: &[u8], metadata: &std::fs::Metadata) -> Self {
        Self {
            len: metadata.len(),
            mtime_ns: i128::from(metadata.mtime()) * 1_000_000_000
                + i128::from(metadata.mtime_nsec()),
            sha256: Sha256::digest(bytes).into(),
        }
    }
}

/// A file read whole, with the stamp of what was read.
#[derive(Debug)]
pub struct ReadFile {
    /// The path after following in-workspace symlinks.
    pub path: WsPath,
    pub bytes: Vec<u8>,
    pub stamp: FileStamp,
    /// Permission bits.
    pub mode: u32,
}

/// The opened folder: its canonical root, a descriptor of it, and the deny
/// rules.
pub struct Workspace {
    root: PathBuf,
    aliases: Vec<PathBuf>,
    root_fd: OwnedFd,
    deny: GlobSet,
    deny_patterns: Vec<String>,
}

impl fmt::Debug for Workspace {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Workspace")
            .field("root", &self.root)
            .field("deny", &self.deny_patterns)
            .finish_non_exhaustive()
    }
}

impl Workspace {
    /// Open `root` (canonicalised) with `path_deny` globs.
    ///
    /// A pattern without a `/` matches a name at any depth (`*.pem`,
    /// `.env`); one with a `/` matches from the root (`secrets/**`). A
    /// pattern that matches a directory denies everything under it.
    /// Patterns and paths compare in Unicode NFC, and without regard to case
    /// on macOS and Windows, whose file systems do the same.
    ///
    /// # Errors
    ///
    /// When `root` is not a directory, is the file system root, or a pattern
    /// is not a valid glob.
    pub fn open(root: &Path, path_deny: &[String]) -> ToolResult<Self> {
        let canonical = std::fs::canonicalize(root)
            .map_err(|error| ToolError::io("cannot open the workspace", &error))?;
        if canonical.parent().is_none() {
            return Err(ToolError::invalid(
                "the file system root cannot be a workspace",
            ));
        }
        let root_fd = rustix::fs::open(
            &canonical,
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC,
            Mode::empty(),
        )
        .map_err(|_| ToolError::invalid("the workspace is not a directory"))?;
        let mut aliases = vec![canonical.clone()];
        if root.is_absolute()
            && let Some(spelled) = normalise_absolute(root)
            && spelled != canonical
        {
            aliases.push(spelled);
        }
        let deny = deny_globset(path_deny)?;
        Ok(Self {
            root: canonical,
            aliases,
            root_fd,
            deny,
            deny_patterns: path_deny.to_vec(),
        })
    }

    /// The canonical root.
    #[must_use]
    pub fn root(&self) -> &Path {
        &self.root
    }

    /// The `path_deny` patterns it was opened with.
    #[must_use]
    pub fn deny_patterns(&self) -> &[String] {
        &self.deny_patterns
    }

    /// The absolute path of `path` (for a child process's working directory
    /// or a git pathspec; never used to open files here).
    #[must_use]
    pub fn absolute(&self, path: &WsPath) -> PathBuf {
        let mut out = self.root.clone();
        for component in path.components() {
            out.push(component);
        }
        out
    }

    /// Resolve a path argument lexically and check the deny rules.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::OutsideWorkspace`] for an escape,
    /// [`ErrorCode::Denied`] for a denied or protected path,
    /// [`ErrorCode::InvalidArgument`] for an empty or non-UTF-8 path.
    pub fn resolve(&self, argument: &str, intent: Intent) -> ToolResult<WsPath> {
        if argument.is_empty() || argument.contains('\0') {
            return Err(ToolError::invalid("a path argument is empty or holds NUL"));
        }
        let path = Path::new(argument);
        let resolved = if path.is_absolute() {
            self.strip_root(path).ok_or_else(outside)?
        } else {
            WsPath::from_relative(path)?
        };
        self.check(&resolved, intent)?;
        Ok(resolved)
    }

    /// A workspace path for an absolute path under the root, if it is one.
    fn strip_root(&self, absolute: &Path) -> Option<WsPath> {
        let normalised = normalise_absolute(absolute)?;
        self.aliases.iter().find_map(|alias| {
            normalised
                .strip_prefix(alias)
                .ok()
                .and_then(|rest| WsPath::from_relative(rest).ok())
        })
    }

    /// The deny rule `path` falls under, if any.
    #[must_use]
    pub fn denied_by(&self, path: &WsPath, intent: Intent) -> Option<String> {
        let components = path.components();
        if intent == Intent::Write && components.iter().any(|c| is_protected_name(c)) {
            return Some(format!("{PROTECTED_DIR} (protected)"));
        }
        (1..=components.len()).find_map(|end| {
            let prefix = nfc(&components[..end].join("/"));
            self.deny
                .is_match(&prefix)
                .then(|| format!("path_deny ({prefix})"))
        })
    }

    /// [`Self::denied_by`] as an error.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::Denied`] when a rule matches.
    pub fn check(&self, path: &WsPath, intent: Intent) -> ToolResult<()> {
        match self.denied_by(path, intent) {
            Some(rule) => Err(ToolError::new(
                ErrorCode::Denied,
                format!("`{path}` is denied by {rule}"),
            )),
            None => Ok(()),
        }
    }

    fn root_dir(&self) -> ToolResult<OwnedFd> {
        self.root_fd
            .try_clone()
            .map_err(|error| ToolError::io("cannot use the workspace", &error))
    }

    /// The target of the symlink `name` in `dir` (at workspace path
    /// `dir_path`), as a workspace path.
    fn symlink_target(&self, dir: &OwnedFd, dir_path: &[String], name: &str) -> ToolResult<WsPath> {
        let target = rustix::fs::readlinkat(dir, name, Vec::new())?;
        let target = target
            .to_str()
            .map_err(|_| ToolError::invalid("a symlink target is not UTF-8"))?;
        let target = Path::new(target);
        if target.is_absolute() {
            self.strip_root(target).ok_or_else(outside)
        } else {
            WsPath(dir_path.to_vec()).join_relative(target)
        }
    }

    /// Walk to the directory that holds `path`'s last component (see the
    /// module documentation). Returns that directory's descriptor and the
    /// path after following in-workspace symlinks.
    fn locate(
        &self,
        path: &WsPath,
        create_dirs: bool,
        follow_final: bool,
    ) -> ToolResult<(OwnedFd, WsPath)> {
        self.walk(path, create_dirs, follow_final, false)
    }

    /// Where a write of `path` would land: in-workspace symlinks on the way
    /// and at the end followed, a missing tail kept as written. What the
    /// approval rules match and the person is shown.
    ///
    /// # Errors
    ///
    /// A symlink that leads out of the workspace, or an unusable walk.
    pub fn final_target(&self, path: &WsPath) -> ToolResult<WsPath> {
        if path.is_root() {
            return Ok(WsPath::root());
        }
        self.walk(path, false, true, true).map(|(_, target)| target)
    }

    fn walk(
        &self,
        path: &WsPath,
        create_dirs: bool,
        follow_final: bool,
        probe: bool,
    ) -> ToolResult<(OwnedFd, WsPath)> {
        if path.is_root() {
            return Err(ToolError::invalid("the workspace root is not a file"));
        }
        let mut pending: VecDeque<String> = path.components().iter().cloned().collect();
        let mut done: Vec<String> = Vec::new();
        let mut dir = self.root_dir()?;
        let mut hops = 0usize;
        while let Some(name) = pending.pop_front() {
            let restart = if pending.is_empty() {
                if follow_final && is_symlink(&dir, &name) {
                    Some(self.symlink_target(&dir, &done, &name)?)
                } else {
                    done.push(name);
                    return Ok((dir, WsPath(done)));
                }
            } else {
                match rustix::fs::openat(
                    &dir,
                    name.as_str(),
                    OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
                    Mode::empty(),
                ) {
                    Ok(next) => {
                        dir = next;
                        done.push(name);
                        None
                    }
                    Err(Errno::NOENT) if create_dirs => {
                        // The directory a write would create must itself be
                        // writable: a symlink followed on the way may have
                        // led into `.git` or a denied path.
                        let mut created = done.clone();
                        created.push(name.clone());
                        self.check(&WsPath(created), Intent::Write)?;
                        match rustix::fs::mkdirat(&dir, name.as_str(), Mode::from_raw_mode(0o755)) {
                            Ok(()) | Err(Errno::EXIST) => {}
                            Err(errno) => return Err(errno.into()),
                        }
                        pending.push_front(name);
                        None
                    }
                    Err(Errno::NOENT) if probe && !is_symlink(&dir, &name) => {
                        done.push(name);
                        done.extend(pending.drain(..));
                        return Ok((dir, WsPath(done)));
                    }
                    Err(Errno::NOENT) if !is_symlink(&dir, &name) => {
                        return Err(ToolError::new(
                            ErrorCode::NotFound,
                            format!("`{path}` does not exist"),
                        ));
                    }
                    Err(errno) => {
                        if is_symlink(&dir, &name) {
                            Some(self.symlink_target(&dir, &done, &name)?)
                        } else {
                            return Err(ToolError::new(
                                if errno == Errno::NOTDIR {
                                    ErrorCode::InvalidArgument
                                } else {
                                    ErrorCode::Io
                                },
                                format!("cannot open a directory on the way to `{path}`"),
                            ));
                        }
                    }
                }
            };
            if let Some(target) = restart {
                hops += 1;
                if hops > MAX_SYMLINK_HOPS {
                    return Err(ToolError::invalid(format!(
                        "too many symlinks resolving `{path}`"
                    )));
                }
                for component in target.components().iter().rev() {
                    pending.push_front(component.clone());
                }
                done.clear();
                dir = self.root_dir()?;
                if pending.is_empty() {
                    return Err(ToolError::invalid(format!(
                        "`{path}` resolves to the workspace root"
                    )));
                }
            }
        }
        Err(ToolError::invalid("empty path"))
    }

    /// What `path` is, without following a final symlink; `None` when it
    /// does not exist.
    ///
    /// # Errors
    ///
    /// When the walk fails for any reason other than absence.
    pub fn stat(&self, path: &WsPath) -> ToolResult<Option<EntryKind>> {
        if path.is_root() {
            return Ok(Some(EntryKind::Dir));
        }
        let (dir, resolved) = match self.locate(path, false, false) {
            Ok(found) => found,
            Err(error) if error.code() == ErrorCode::NotFound => return Ok(None),
            Err(error) => return Err(error),
        };
        let name = resolved.file_name().unwrap_or_default();
        match rustix::fs::statat(&dir, name, AtFlags::SYMLINK_NOFOLLOW) {
            Ok(stat) => Ok(Some(match FileType::from_raw_mode(stat.st_mode) {
                FileType::RegularFile => EntryKind::File,
                FileType::Directory => EntryKind::Dir,
                FileType::Symlink => EntryKind::Symlink,
                _ => EntryKind::Other,
            })),
            Err(Errno::NOENT) => Ok(None),
            Err(errno) => Err(errno.into()),
        }
    }

    /// Open a regular file for reading, confined.
    ///
    /// # Errors
    ///
    /// Not found, not a regular file, outside the workspace, or denied.
    pub fn open_file(&self, path: &WsPath) -> ToolResult<(File, WsPath)> {
        let (dir, resolved) = self.locate(path, false, true)?;
        self.check(&resolved, Intent::Read)?;
        let name = resolved.file_name().unwrap_or_default();
        // O_NONBLOCK: opening a FIFO must not hang the tool.
        let fd = match rustix::fs::openat(
            &dir,
            name,
            OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::CLOEXEC | OFlags::NONBLOCK,
            Mode::empty(),
        ) {
            Ok(fd) => fd,
            Err(Errno::NOENT) => {
                return Err(ToolError::new(
                    ErrorCode::NotFound,
                    format!("`{path}` does not exist"),
                ));
            }
            Err(errno) => return Err(errno.into()),
        };
        let file = File::from(fd);
        let metadata = file
            .metadata()
            .map_err(|error| ToolError::io("cannot stat the file", &error))?;
        if !metadata.is_file() {
            return Err(ToolError::invalid(format!(
                "`{path}` is not a regular file"
            )));
        }
        Ok((file, resolved))
    }

    /// Read a whole regular file of at most `max_bytes`.
    ///
    /// # Errors
    ///
    /// As [`Self::open_file`], and [`ErrorCode::TooLarge`].
    pub fn read(&self, path: &WsPath, max_bytes: u64) -> ToolResult<ReadFile> {
        let (file, resolved) = self.open_file(path)?;
        let metadata = file
            .metadata()
            .map_err(|error| ToolError::io("cannot stat the file", &error))?;
        if metadata.len() > max_bytes {
            return Err(ToolError::new(
                ErrorCode::TooLarge,
                format!(
                    "`{path}` is {} bytes; the limit is {max_bytes}",
                    metadata.len()
                ),
            ));
        }
        let mut bytes = Vec::new();
        file.take(max_bytes.saturating_add(1))
            .read_to_end(&mut bytes)
            .map_err(|error| ToolError::io("cannot read the file", &error))?;
        if bytes.len() as u64 > max_bytes {
            return Err(ToolError::new(
                ErrorCode::TooLarge,
                format!("`{path}` grew past the limit"),
            ));
        }
        let stamp = FileStamp::of(&bytes, &metadata);
        Ok(ReadFile {
            path: resolved,
            bytes,
            stamp,
            mode: metadata.mode() & 0o7777,
        })
    }

    /// Replace (or create) a file atomically: a temporary file created with
    /// `O_EXCL | O_NOFOLLOW` beside it, then `renameat`. Missing parent
    /// directories are created. Keeps an existing file's permission bits
    /// unless `mode` is given.
    ///
    /// # Errors
    ///
    /// Outside the workspace, denied, a directory in the way, or I/O.
    pub fn write(&self, path: &WsPath, bytes: &[u8], mode: Option<u32>) -> ToolResult<WsPath> {
        self.check(path, Intent::Write)?;
        // Where it lands, checked before anything is created on the way
        // (the creating walk checks each directory it makes, too).
        self.check(&self.final_target(path)?, Intent::Write)?;
        let (dir, resolved) = self.locate(path, true, true)?;
        self.check(&resolved, Intent::Write)?;
        let name = resolved.file_name().unwrap_or_default().to_owned();
        let existing = match rustix::fs::statat(&dir, name.as_str(), AtFlags::SYMLINK_NOFOLLOW) {
            Ok(stat) => {
                if FileType::from_raw_mode(stat.st_mode) == FileType::Directory {
                    return Err(ToolError::invalid(format!("`{resolved}` is a directory")));
                }
                Some(mode_bits(stat.st_mode) & 0o7777)
            }
            Err(Errno::NOENT) => None,
            Err(errno) => return Err(errno.into()),
        };
        let permissions = mode.or(existing).unwrap_or(0o644) & 0o7777;
        let temp = format!(
            ".{name}.elitea-{}-{}.tmp",
            std::process::id(),
            TEMP_COUNTER.fetch_add(1, Ordering::Relaxed)
        );
        let fd = rustix::fs::openat(
            &dir,
            temp.as_str(),
            OFlags::WRONLY | OFlags::CREATE | OFlags::EXCL | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::from_raw_mode(0o600),
        )?;
        let written = (|| -> ToolResult<()> {
            let mut file = File::from(fd);
            file.set_permissions(std::fs::Permissions::from_mode(permissions))
                .map_err(|error| ToolError::io("cannot set the file mode", &error))?;
            file.write_all(bytes)
                .map_err(|error| ToolError::io("cannot write the file", &error))?;
            file.flush()
                .map_err(|error| ToolError::io("cannot write the file", &error))?;
            rustix::fs::renameat(&dir, temp.as_str(), &dir, name.as_str())?;
            Ok(())
        })();
        if written.is_err() {
            let _ = rustix::fs::unlinkat(&dir, temp.as_str(), AtFlags::empty());
        }
        written.map(|()| resolved)
    }

    /// Remove a file (or a symlink itself). `Ok(false)` when it was absent.
    /// The path after following symlinks on the way is checked against the
    /// deny rules before anything is removed.
    ///
    /// # Errors
    ///
    /// Outside the workspace, denied, a directory, or I/O.
    pub fn remove_file(&self, path: &WsPath) -> ToolResult<bool> {
        self.check(path, Intent::Write)?;
        let (dir, resolved) = match self.locate(path, false, false) {
            Ok(found) => found,
            Err(error) if error.code() == ErrorCode::NotFound => return Ok(false),
            Err(error) => return Err(error),
        };
        // A symlink on the way may have led into `.git` or a denied path.
        self.check(&resolved, Intent::Write)?;
        match rustix::fs::unlinkat(
            &dir,
            resolved.file_name().unwrap_or_default(),
            AtFlags::empty(),
        ) {
            Ok(()) => Ok(true),
            Err(Errno::NOENT) => Ok(false),
            Err(errno) => Err(errno.into()),
        }
    }
}

/// Permission and type bits as `u32` (`mode_t` is `u16` on macOS and `u32`
/// on Linux, where this conversion is the identity).
#[allow(clippy::useless_conversion)]
fn mode_bits(raw: rustix::fs::RawMode) -> u32 {
    u32::from(raw)
}

fn is_symlink(dir: &OwnedFd, name: &str) -> bool {
    rustix::fs::statat(dir, name, AtFlags::SYMLINK_NOFOLLOW)
        .is_ok_and(|stat| FileType::from_raw_mode(stat.st_mode) == FileType::Symlink)
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::symlink;

    use super::{EntryKind, Intent, Workspace, WsPath};
    use crate::error::ErrorCode;

    fn workspace(deny: &[&str]) -> (tempfile::TempDir, tempfile::TempDir, Workspace) {
        let root = tempfile::tempdir().expect("root");
        let outside = tempfile::tempdir().expect("outside");
        std::fs::write(outside.path().join("secret.txt"), "outside").expect("seed");
        let deny: Vec<String> = deny.iter().map(|s| (*s).to_owned()).collect();
        let ws = Workspace::open(root.path(), &deny).expect("open");
        (root, outside, ws)
    }

    fn code<T: std::fmt::Debug>(result: crate::error::ToolResult<T>) -> ErrorCode {
        result.expect_err("expected an error").code()
    }

    #[test]
    fn relative_paths_normalise_and_dotdot_cannot_escape() {
        let (_root, _outside, ws) = workspace(&[]);
        assert_eq!(
            ws.resolve("a/./b/../c", Intent::Read)
                .expect("ok")
                .display_string(),
            "a/c"
        );
        assert_eq!(
            code(ws.resolve("../x", Intent::Read)),
            ErrorCode::OutsideWorkspace
        );
        assert_eq!(
            code(ws.resolve("a/../../x", Intent::Read)),
            ErrorCode::OutsideWorkspace
        );
        assert_eq!(
            code(ws.resolve("", Intent::Read)),
            ErrorCode::InvalidArgument
        );
        assert_eq!(
            code(ws.resolve("a\0b", Intent::Read)),
            ErrorCode::InvalidArgument
        );
    }

    #[test]
    fn absolute_paths_must_be_under_the_root_under_any_spelling() {
        let (root, outside, ws) = workspace(&[]);
        let inside = root.path().join("dir/file.txt");
        assert_eq!(
            ws.resolve(inside.to_str().expect("utf8"), Intent::Read)
                .expect("ok")
                .display_string(),
            "dir/file.txt"
        );
        let canonical = ws.root().join("x");
        assert_eq!(
            ws.resolve(canonical.to_str().expect("utf8"), Intent::Read)
                .expect("ok")
                .display_string(),
            "x"
        );
        let elsewhere = outside.path().join("secret.txt");
        assert_eq!(
            code(ws.resolve(elsewhere.to_str().expect("utf8"), Intent::Read)),
            ErrorCode::OutsideWorkspace
        );
        assert_eq!(
            code(ws.resolve("/etc/passwd", Intent::Read)),
            ErrorCode::OutsideWorkspace
        );
        let sneaky = format!("{}/../{}", ws.root().display(), "x");
        assert_eq!(
            code(ws.resolve(&sneaky, Intent::Read)),
            ErrorCode::OutsideWorkspace
        );
    }

    #[test]
    fn deny_globs_match_names_at_any_depth_and_whole_directories() {
        let (_root, _outside, ws) = workspace(&["*.pem", "secrets/**", ".env"]);
        assert_eq!(
            code(ws.resolve("a/b/key.pem", Intent::Read)),
            ErrorCode::Denied
        );
        assert_eq!(code(ws.resolve("key.pem", Intent::Read)), ErrorCode::Denied);
        assert_eq!(
            code(ws.resolve("secrets/x/y", Intent::Read)),
            ErrorCode::Denied
        );
        assert_eq!(
            code(ws.resolve("deep/.env", Intent::Read)),
            ErrorCode::Denied
        );
        assert!(ws.resolve("src/main.rs", Intent::Read).is_ok());
        assert!(
            ws.resolve("nested/secrets/x", Intent::Read).is_ok(),
            "anchored patterns stay anchored"
        );
    }

    #[test]
    fn git_directories_are_readable_but_never_writable() {
        let (_root, _outside, ws) = workspace(&[]);
        assert!(ws.resolve(".git/HEAD", Intent::Read).is_ok());
        assert_eq!(
            code(ws.resolve(".git/hooks/pre-commit", Intent::Write)),
            ErrorCode::Denied
        );
        assert_eq!(
            code(ws.resolve("vendor/lib/.git/config", Intent::Write)),
            ErrorCode::Denied
        );
        let path = WsPath::from_relative(std::path::Path::new(".git/config")).expect("path");
        assert_eq!(code(ws.write(&path, b"x", None)), ErrorCode::Denied);
    }

    /// APFS (the macOS default) is case-insensitive and normalisation-
    /// insensitive, and HFS+ ignored some zero-width code points: every
    /// spelling the file system treats as `.git` is protected.
    #[test]
    fn git_directories_are_protected_under_every_spelling() {
        let (_root, _outside, ws) = workspace(&[]);
        for spelling in [
            ".GIT/hooks/pre-commit",
            ".Git/config",
            "sub/.gIt/config",
            ".g\u{200c}it/config",
            ".git\u{feff}/config",
        ] {
            assert_eq!(
                code(ws.resolve(spelling, Intent::Write)),
                ErrorCode::Denied,
                "{spelling}"
            );
        }
        assert!(
            ws.resolve(".github/workflows/ci.yml", Intent::Write)
                .is_ok()
        );
        assert!(ws.resolve(".gitignore", Intent::Write).is_ok());
    }

    #[test]
    fn deny_globs_ignore_case_on_macos_and_unicode_normalisation_everywhere() {
        let (_root, _outside, ws) = workspace(&[".env", "secrets/**", "caf\u{e9}.txt"]);
        assert_eq!(
            code(ws.resolve("cafe\u{301}.txt", Intent::Read)),
            ErrorCode::Denied,
            "an NFD spelling of an NFC pattern"
        );
        if cfg!(target_os = "macos") {
            assert_eq!(code(ws.resolve(".ENV", Intent::Read)), ErrorCode::Denied);
            assert_eq!(
                code(ws.resolve("Secrets/key", Intent::Read)),
                ErrorCode::Denied
            );
            assert_eq!(
                code(ws.resolve("deep/.Env", Intent::Write)),
                ErrorCode::Denied
            );
        }
    }

    #[test]
    fn write_then_read_round_trips_and_keeps_permissions() {
        use std::os::unix::fs::PermissionsExt;
        let (root, _outside, ws) = workspace(&[]);
        let path = ws.resolve("new/dir/file.sh", Intent::Write).expect("path");
        ws.write(&path, b"#!/bin/sh\n", Some(0o755)).expect("write");
        ws.write(&path, b"#!/bin/sh\necho\n", None)
            .expect("rewrite");
        let read = ws.read(&path, 1024).expect("read");
        assert_eq!(read.bytes, b"#!/bin/sh\necho\n");
        assert_eq!(read.mode, 0o755);
        let mode = std::fs::metadata(root.path().join("new/dir/file.sh"))
            .expect("meta")
            .permissions()
            .mode();
        assert_eq!(mode & 0o777, 0o755);
        assert_eq!(ws.stat(&path).expect("stat"), Some(EntryKind::File));
        let leftovers: Vec<_> = std::fs::read_dir(root.path().join("new/dir"))
            .expect("dir")
            .collect();
        assert_eq!(leftovers.len(), 1, "no temporary file is left behind");
    }

    #[test]
    fn symlinks_inside_the_workspace_are_followed() {
        let (root, _outside, ws) = workspace(&[]);
        std::fs::create_dir(root.path().join("real")).expect("dir");
        std::fs::write(root.path().join("real/f.txt"), "inside").expect("seed");
        symlink("real", root.path().join("alias")).expect("dir link");
        symlink("real/f.txt", root.path().join("file-link")).expect("file link");
        let via_dir = ws.resolve("alias/f.txt", Intent::Read).expect("path");
        let read = ws.read(&via_dir, 1024).expect("read");
        assert_eq!(read.bytes, b"inside");
        assert_eq!(read.path.display_string(), "real/f.txt");
        let via_file = ws.resolve("file-link", Intent::Read).expect("path");
        assert_eq!(ws.read(&via_file, 1024).expect("read").bytes, b"inside");
    }

    #[test]
    fn symlinks_resolving_outside_are_refused_for_reads_and_writes() {
        let (root, outside, ws) = workspace(&[]);
        symlink(outside.path(), root.path().join("out-dir")).expect("dir link");
        symlink(
            outside.path().join("secret.txt"),
            root.path().join("out-file"),
        )
        .expect("file link");
        symlink("../../etc", root.path().join("relative-escape")).expect("relative link");
        symlink(outside.path().join("new.txt"), root.path().join("dangling")).expect("dangling");
        for name in ["out-dir/secret.txt", "out-file", "relative-escape/passwd"] {
            let path = ws.resolve(name, Intent::Read).expect("lexically fine");
            assert_eq!(
                code(ws.read(&path, 1024)),
                ErrorCode::OutsideWorkspace,
                "{name}"
            );
        }
        let write_through_dir = ws.resolve("out-dir/new.txt", Intent::Write).expect("path");
        assert_eq!(
            code(ws.write(&write_through_dir, b"x", None)),
            ErrorCode::OutsideWorkspace
        );
        let dangling = ws.resolve("dangling", Intent::Write).expect("path");
        assert_eq!(
            code(ws.write(&dangling, b"x", None)),
            ErrorCode::OutsideWorkspace
        );
        assert!(!outside.path().join("new.txt").exists());
        assert_eq!(
            std::fs::read_to_string(outside.path().join("secret.txt")).expect("intact"),
            "outside"
        );
    }

    #[test]
    fn a_symlink_into_a_denied_path_is_denied() {
        let (root, _outside, ws) = workspace(&["secrets/**"]);
        std::fs::create_dir(root.path().join("secrets")).expect("dir");
        std::fs::write(root.path().join("secrets/key"), "k").expect("seed");
        symlink("secrets/key", root.path().join("innocent")).expect("link");
        let path = ws
            .resolve("innocent", Intent::Read)
            .expect("lexically fine");
        assert_eq!(code(ws.read(&path, 1024)), ErrorCode::Denied);
    }

    /// The race the descriptor walk closes: the path is resolved and checked
    /// while `a` is a real directory, then `a` is swapped for a symlink to a
    /// directory outside before the read and the write happen.
    #[test]
    fn a_directory_swapped_for_a_symlink_after_resolution_is_not_followed_out() {
        let (root, outside, ws) = workspace(&[]);
        std::fs::create_dir(root.path().join("a")).expect("dir");
        std::fs::write(root.path().join("a/secret.txt"), "inside").expect("seed");
        let read_path = ws.resolve("a/secret.txt", Intent::Read).expect("path");
        let write_path = ws.resolve("a/planted.txt", Intent::Write).expect("path");
        std::fs::remove_dir_all(root.path().join("a")).expect("remove");
        symlink(outside.path(), root.path().join("a")).expect("swap");
        assert_eq!(code(ws.read(&read_path, 1024)), ErrorCode::OutsideWorkspace);
        assert_eq!(
            code(ws.write(&write_path, b"x", None)),
            ErrorCode::OutsideWorkspace
        );
        let entries: Vec<_> = std::fs::read_dir(outside.path()).expect("dir").collect();
        assert_eq!(
            entries.len(),
            1,
            "nothing was created outside, not even a temporary file"
        );
    }

    #[test]
    fn a_file_swapped_for_a_symlink_is_replaced_not_written_through() {
        let (root, outside, ws) = workspace(&[]);
        std::fs::write(root.path().join("f.txt"), "v1").expect("seed");
        let path = ws.resolve("f.txt", Intent::Write).expect("path");
        std::fs::remove_file(root.path().join("f.txt")).expect("remove");
        symlink(outside.path().join("secret.txt"), root.path().join("f.txt")).expect("swap");
        assert_eq!(
            code(ws.write(&path, b"x", None)),
            ErrorCode::OutsideWorkspace
        );
        assert_eq!(
            std::fs::read_to_string(outside.path().join("secret.txt")).expect("intact"),
            "outside"
        );
    }

    #[test]
    fn fifos_and_directories_are_not_read() {
        let (root, _outside, ws) = workspace(&[]);
        std::fs::create_dir(root.path().join("d")).expect("dir");
        let dir = ws.resolve("d", Intent::Read).expect("path");
        assert_eq!(code(ws.read(&dir, 1024)), ErrorCode::InvalidArgument);
        let status = std::process::Command::new("mkfifo")
            .arg(root.path().join("pipe"))
            .status()
            .expect("mkfifo");
        assert!(status.success());
        let pipe = ws.resolve("pipe", Intent::Read).expect("path");
        assert_eq!(code(ws.read(&pipe, 1024)), ErrorCode::InvalidArgument);
    }

    #[test]
    fn size_caps_apply() {
        let (root, _outside, ws) = workspace(&[]);
        std::fs::write(root.path().join("big"), vec![b'a'; 2048]).expect("seed");
        let path = ws.resolve("big", Intent::Read).expect("path");
        assert_eq!(code(ws.read(&path, 1024)), ErrorCode::TooLarge);
    }

    /// A path that reaches `.git` (or a `path_deny` match) through an
    /// in-workspace symlink is refused before anything changes: no file is
    /// removed and no directory is created on the way.
    #[test]
    fn a_link_into_protected_or_denied_paths_changes_nothing() {
        let (root, _outside, ws) = workspace(&["secrets/**"]);
        std::fs::create_dir_all(root.path().join(".git/hooks")).expect("git dir");
        std::fs::write(root.path().join(".git/config"), "[core]\n").expect("config");
        std::fs::create_dir(root.path().join("secrets")).expect("secrets");
        std::fs::write(root.path().join("secrets/key"), "k").expect("key");
        symlink(".git", root.path().join("x")).expect("link");
        symlink("secrets", root.path().join("vault")).expect("link");

        let config = ws
            .resolve("x/config", Intent::Write)
            .expect("lexically fine");
        assert_eq!(code(ws.remove_file(&config)), ErrorCode::Denied);
        assert!(root.path().join(".git/config").exists(), "not removed");
        let key = ws
            .resolve("vault/key", Intent::Write)
            .expect("lexically fine");
        assert_eq!(code(ws.remove_file(&key)), ErrorCode::Denied);
        assert!(root.path().join("secrets/key").exists(), "not removed");

        let hook = ws
            .resolve("x/hooks/new/pre-commit", Intent::Write)
            .expect("lexically fine");
        assert_eq!(
            code(ws.write(&hook, b"#!/bin/sh\n", None)),
            ErrorCode::Denied
        );
        assert!(
            !root.path().join(".git/hooks/new").exists(),
            "no directory was created inside .git"
        );
        let planted = ws
            .resolve("vault/new/key", Intent::Write)
            .expect("lexically fine");
        assert_eq!(code(ws.write(&planted, b"x", None)), ErrorCode::Denied);
        assert!(
            !root.path().join("secrets/new").exists(),
            "no directory was created in a denied path"
        );
    }

    #[test]
    fn remove_deletes_a_link_not_its_target() {
        let (root, outside, ws) = workspace(&[]);
        symlink(outside.path().join("secret.txt"), root.path().join("link")).expect("link");
        let path = ws.resolve("link", Intent::Write).expect("path");
        assert!(ws.remove_file(&path).expect("remove"));
        assert!(outside.path().join("secret.txt").exists());
        assert!(!ws.remove_file(&path).expect("absent"));
    }
}
