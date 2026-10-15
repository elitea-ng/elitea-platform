//! The tree format the worker and the workspace runner move a cloud turn's
//! files in (ADR-0034 decision 9: "the worker moves the files into and out
//! of the container through the sandbox helper protocol").
//!
//! A flat, length-prefixed stream: `ELWS1\n`, then entries, then an end
//! marker. Each entry is a kind byte (`F` file, `X` executable file, `L`
//! symlink), the path (u32 big-endian length, UTF-8), and the content (u64
//! big-endian length, bytes; a symlink's content is its target). `.git` is
//! never written or read, at any depth and in any case.
//!
//! The reader is the trust boundary in both directions (the runner reads
//! what the worker sends, the worker reads what an agent-controlled
//! container sends): it refuses absolute paths, `..`, `.git`, duplicate
//! paths and anything over its limits, and writes only into an empty
//! directory without following a symlink.

use std::collections::BTreeSet;
use std::fs::{self, File};
use std::io::{self, Read, Write};
use std::path::{Component, Path};

#[cfg(unix)]
use std::os::unix::fs::{PermissionsExt as _, symlink};

const MAGIC: &[u8] = b"ELWS1\n";
const END: u8 = b'E';
const FILE: u8 = b'F';
const EXECUTABLE: u8 = b'X';
const LINK: u8 = b'L';
const MAX_PATH_BYTES: u32 = 4_096;

/// What one read may hold.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ArchiveLimits {
    pub max_files: u64,
    pub max_file_bytes: u64,
    pub max_total_bytes: u64,
}

/// Why a tree cannot be moved. Data-free.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ArchiveError {
    /// The stream is not this format, or holds a refused path.
    Malformed,
    /// Over a limit.
    TooLarge,
    /// The tree holds something that cannot be moved (a socket, a fifo).
    Unsupported,
    Io,
}

impl ArchiveError {
    #[must_use]
    pub const fn code(self) -> &'static str {
        match self {
            Self::Malformed => "workspace_archive.malformed",
            Self::TooLarge => "workspace_archive.too_large",
            Self::Unsupported => "workspace_archive.unsupported_entry",
            Self::Io => "workspace_archive.io",
        }
    }
}

fn io_error(_: io::Error) -> ArchiveError {
    ArchiveError::Io
}

/// A path an archive may carry: relative, normal components only, no `.git`.
#[must_use]
pub fn safe_path(path: &str) -> bool {
    !path.is_empty()
        && !path.contains('\0')
        && Path::new(path)
            .components()
            .all(|component| match component {
                Component::Normal(name) => !name.eq_ignore_ascii_case(".git"),
                _ => false,
            })
}

/// What [`write_tree`] wrote.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct ArchiveStats {
    pub files: u64,
    pub bytes: u64,
}

/// Write the tree under `root` to `out`, in path order, `.git` left out.
///
/// # Errors
///
/// A special file, a name that is not UTF-8, a limit, or an I/O failure.
pub fn write_tree(
    root: &Path,
    out: &mut dyn Write,
    limits: ArchiveLimits,
) -> Result<ArchiveStats, ArchiveError> {
    out.write_all(MAGIC).map_err(io_error)?;
    let mut stats = ArchiveStats::default();
    write_directory(root, "", out, limits, &mut stats)?;
    out.write_all(&[END]).map_err(io_error)?;
    out.flush().map_err(io_error)?;
    Ok(stats)
}

fn write_directory(
    directory: &Path,
    prefix: &str,
    out: &mut dyn Write,
    limits: ArchiveLimits,
    stats: &mut ArchiveStats,
) -> Result<(), ArchiveError> {
    let mut names = Vec::new();
    for entry in fs::read_dir(directory).map_err(io_error)? {
        let entry = entry.map_err(io_error)?;
        let name = entry
            .file_name()
            .into_string()
            .map_err(|_| ArchiveError::Unsupported)?;
        if !name.eq_ignore_ascii_case(".git") {
            names.push(name);
        }
    }
    names.sort();
    for name in names {
        let path = directory.join(&name);
        let relative = if prefix.is_empty() {
            name.clone()
        } else {
            format!("{prefix}/{name}")
        };
        let metadata = fs::symlink_metadata(&path).map_err(io_error)?;
        let kind = metadata.file_type();
        if kind.is_dir() {
            write_directory(&path, &relative, out, limits, stats)?;
            continue;
        }
        stats.files += 1;
        if stats.files > limits.max_files {
            return Err(ArchiveError::TooLarge);
        }
        let (tag, content) = if kind.is_symlink() {
            let target = fs::read_link(&path).map_err(io_error)?;
            let target = target.to_str().ok_or(ArchiveError::Unsupported)?;
            (LINK, target.as_bytes().to_vec())
        } else if kind.is_file() {
            if metadata.len() > limits.max_file_bytes {
                return Err(ArchiveError::TooLarge);
            }
            let mut content = Vec::with_capacity(usize::try_from(metadata.len()).unwrap_or(0));
            File::open(&path)
                .map_err(io_error)?
                .take(limits.max_file_bytes + 1)
                .read_to_end(&mut content)
                .map_err(io_error)?;
            (
                if executable(&metadata) {
                    EXECUTABLE
                } else {
                    FILE
                },
                content,
            )
        } else {
            return Err(ArchiveError::Unsupported);
        };
        stats.bytes += content.len() as u64;
        if content.len() as u64 > limits.max_file_bytes || stats.bytes > limits.max_total_bytes {
            return Err(ArchiveError::TooLarge);
        }
        let path_len = u32::try_from(relative.len()).map_err(|_| ArchiveError::Unsupported)?;
        out.write_all(&[tag]).map_err(io_error)?;
        out.write_all(&path_len.to_be_bytes()).map_err(io_error)?;
        out.write_all(relative.as_bytes()).map_err(io_error)?;
        out.write_all(&(content.len() as u64).to_be_bytes())
            .map_err(io_error)?;
        out.write_all(&content).map_err(io_error)?;
    }
    Ok(())
}

#[cfg(unix)]
fn executable(metadata: &fs::Metadata) -> bool {
    metadata.permissions().mode() & 0o111 != 0
}

#[cfg(not(unix))]
fn executable(_: &fs::Metadata) -> bool {
    false
}

/// Read an archive from `input` into `root`, which must be an empty
/// directory. Nothing outside `root` is touched; a symlink is created, never
/// followed.
///
/// # Errors
///
/// A malformed stream, a refused path, a limit, or an I/O failure. On error
/// `root` may hold part of the tree; the caller discards it.
pub fn read_tree(
    input: &mut dyn Read,
    root: &Path,
    limits: ArchiveLimits,
) -> Result<ArchiveStats, ArchiveError> {
    if fs::read_dir(root).map_err(io_error)?.next().is_some() {
        return Err(ArchiveError::Malformed);
    }
    let mut magic = [0_u8; 6];
    input
        .read_exact(&mut magic)
        .map_err(|_| ArchiveError::Malformed)?;
    if magic != MAGIC {
        return Err(ArchiveError::Malformed);
    }
    let mut seen = BTreeSet::new();
    let mut stats = ArchiveStats::default();
    loop {
        let mut tag = [0_u8; 1];
        input
            .read_exact(&mut tag)
            .map_err(|_| ArchiveError::Malformed)?;
        if tag[0] == END {
            // Nothing may follow the end marker.
            let mut rest = [0_u8; 1];
            return match input.read(&mut rest) {
                Ok(0) => Ok(stats),
                _ => Err(ArchiveError::Malformed),
            };
        }
        if !matches!(tag[0], FILE | EXECUTABLE | LINK) {
            return Err(ArchiveError::Malformed);
        }
        let mut length = [0_u8; 4];
        input
            .read_exact(&mut length)
            .map_err(|_| ArchiveError::Malformed)?;
        let path_len = u32::from_be_bytes(length);
        if path_len == 0 || path_len > MAX_PATH_BYTES {
            return Err(ArchiveError::Malformed);
        }
        let mut path = vec![0_u8; path_len as usize];
        input
            .read_exact(&mut path)
            .map_err(|_| ArchiveError::Malformed)?;
        let path = String::from_utf8(path).map_err(|_| ArchiveError::Malformed)?;
        if !safe_path(&path) || !seen.insert(path.clone()) {
            return Err(ArchiveError::Malformed);
        }
        // A path below an entry already written as a file or a link would
        // write through it.
        let mut ancestor = Path::new(&path).parent();
        while let Some(parent) = ancestor.filter(|parent| !parent.as_os_str().is_empty()) {
            if seen.contains(parent.to_str().unwrap_or_default()) {
                return Err(ArchiveError::Malformed);
            }
            ancestor = parent.parent();
        }
        let mut size = [0_u8; 8];
        input
            .read_exact(&mut size)
            .map_err(|_| ArchiveError::Malformed)?;
        let size = u64::from_be_bytes(size);
        stats.files += 1;
        stats.bytes = stats.bytes.saturating_add(size);
        if stats.files > limits.max_files
            || size > limits.max_file_bytes
            || stats.bytes > limits.max_total_bytes
        {
            return Err(ArchiveError::TooLarge);
        }
        let target = root.join(&path);
        if let Some(parent) = target.parent() {
            create_directories(root, parent)?;
        }
        let mut content = Vec::with_capacity(usize::try_from(size).unwrap_or(0).min(1 << 20));
        input
            .take(size)
            .read_to_end(&mut content)
            .map_err(|_| ArchiveError::Malformed)?;
        if content.len() as u64 != size {
            return Err(ArchiveError::Malformed);
        }
        write_entry(tag[0], &target, &content)?;
    }
}

/// `create_dir_all` that refuses to pass through anything but a real
/// directory below `root`.
fn create_directories(root: &Path, directory: &Path) -> Result<(), ArchiveError> {
    let relative = directory
        .strip_prefix(root)
        .map_err(|_| ArchiveError::Malformed)?;
    let mut current = root.to_path_buf();
    for component in relative.components() {
        current.push(component);
        match fs::symlink_metadata(&current) {
            Ok(metadata) if metadata.file_type().is_dir() => {}
            Ok(_) => return Err(ArchiveError::Malformed),
            Err(_) => fs::create_dir(&current).map_err(io_error)?,
        }
    }
    Ok(())
}

#[cfg(unix)]
fn write_entry(tag: u8, target: &Path, content: &[u8]) -> Result<(), ArchiveError> {
    if tag == LINK {
        let link = std::str::from_utf8(content).map_err(|_| ArchiveError::Malformed)?;
        if link.is_empty() || link.contains('\0') {
            return Err(ArchiveError::Malformed);
        }
        return symlink(link, target).map_err(io_error);
    }
    let mut file = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(target)
        .map_err(io_error)?;
    file.write_all(content).map_err(io_error)?;
    let mode = if tag == EXECUTABLE { 0o755 } else { 0o644 };
    fs::set_permissions(target, fs::Permissions::from_mode(mode)).map_err(io_error)
}

#[cfg(not(unix))]
fn write_entry(_tag: u8, _target: &Path, _content: &[u8]) -> Result<(), ArchiveError> {
    Err(ArchiveError::Unsupported)
}

#[cfg(all(test, unix))]
mod tests {
    use super::*;

    const LIMITS: ArchiveLimits = ArchiveLimits {
        max_files: 100,
        max_file_bytes: 1 << 20,
        max_total_bytes: 4 << 20,
    };

    #[test]
    fn a_tree_round_trips_without_git() {
        let source = tempfile::tempdir().expect("source");
        fs::create_dir_all(source.path().join("src/deep")).expect("mkdir");
        fs::write(source.path().join("src/deep/a.rs"), "fn a() {}\n").expect("w");
        fs::write(source.path().join("run.sh"), "#!/bin/sh\n").expect("w");
        fs::set_permissions(
            source.path().join("run.sh"),
            fs::Permissions::from_mode(0o755),
        )
        .expect("p");
        symlink("run.sh", source.path().join("link")).expect("l");
        fs::create_dir_all(source.path().join(".git/hooks")).expect("git");
        fs::write(source.path().join(".git/hooks/pre-commit"), "evil").expect("w");
        let mut stream = Vec::new();
        let written = write_tree(source.path(), &mut stream, LIMITS).expect("write");
        assert_eq!(written.files, 3);

        let target = tempfile::tempdir().expect("target");
        let read = read_tree(&mut stream.as_slice(), target.path(), LIMITS).expect("read");
        assert_eq!(read, written);
        assert_eq!(
            fs::read_to_string(target.path().join("src/deep/a.rs")).expect("r"),
            "fn a() {}\n"
        );
        assert_eq!(
            fs::read_link(target.path().join("link")).expect("l"),
            Path::new("run.sh")
        );
        assert!(!target.path().join(".git").exists());
        assert_ne!(
            fs::metadata(target.path().join("run.sh"))
                .expect("m")
                .permissions()
                .mode()
                & 0o111,
            0
        );
    }

    fn entry(tag: u8, path: &str, content: &[u8]) -> Vec<u8> {
        let mut out = vec![tag];
        out.extend_from_slice(&u32::try_from(path.len()).expect("len").to_be_bytes());
        out.extend_from_slice(path.as_bytes());
        out.extend_from_slice(&(content.len() as u64).to_be_bytes());
        out.extend_from_slice(content);
        out
    }

    fn stream(entries: &[Vec<u8>]) -> Vec<u8> {
        let mut out = MAGIC.to_vec();
        for item in entries {
            out.extend_from_slice(item);
        }
        out.push(END);
        out
    }

    #[test]
    fn hostile_streams_are_refused() {
        for bad in [
            stream(&[entry(FILE, "../escape", b"x")]),
            stream(&[entry(FILE, "/etc/passwd", b"x")]),
            stream(&[entry(FILE, ".git/config", b"x")]),
            stream(&[entry(FILE, "a/.GIT/hooks/x", b"x")]),
            stream(&[entry(FILE, "a", b"x"), entry(FILE, "a", b"y")]),
            // Through a link written earlier.
            stream(&[entry(LINK, "out", b"/etc"), entry(FILE, "out/passwd", b"x")]),
            stream(&[entry(b'Z', "a", b"x")]),
        ] {
            let target = tempfile::tempdir().expect("target");
            assert_eq!(
                read_tree(&mut bad.as_slice(), target.path(), LIMITS),
                Err(ArchiveError::Malformed)
            );
            assert!(!Path::new("/etc/passwd-elitea").exists());
        }
        let target = tempfile::tempdir().expect("target");
        let big = stream(&[entry(FILE, "big", &vec![0; 2 << 20])]);
        assert_eq!(
            read_tree(&mut big.as_slice(), target.path(), LIMITS),
            Err(ArchiveError::TooLarge)
        );
        let mut trailing = stream(&[]);
        trailing.push(b'x');
        let target = tempfile::tempdir().expect("target");
        assert_eq!(
            read_tree(&mut trailing.as_slice(), target.path(), LIMITS),
            Err(ArchiveError::Malformed)
        );
    }
}
