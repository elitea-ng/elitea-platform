//! The harvested env's values in the FILES a cloud turn wrote (contract 6.4,
//! ADR-0034 decision 5): before the end-of-turn commit, every changed or
//! added file is scanned for each value of 4 or more characters, its base64
//! forms (standard and URL-safe, at each of the three byte alignments a
//! value can take inside a longer encoding) and its URL-encoded forms.
//!
//! * In a text file each occurrence becomes `[REDACTED:<NAME>]`.
//! * A binary file (a NUL byte in its first 8000 bytes, git's rule) that
//!   holds a value cannot be rewritten safely: the turn's change to it is
//!   dropped (an added file is removed, a changed one gets its parent
//!   content back), and the notice says so.
//! * A file over the in-memory bound is scanned in overlapping windows and,
//!   when it holds a value, dropped like a binary.
//!
//! The notice lists paths and variable NAMES, never a value. Nothing here
//! formats a value.

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;
use std::fs;
use std::io::Read as _;
use std::path::Path;

use base64::engine::general_purpose::{STANDARD, URL_SAFE};
use zeroize::Zeroizing;

use super::redact::{MIN_REDACTED_CHARS, SecretEnv};

/// Git's binary test reads this many bytes.
const BINARY_PROBE_BYTES: usize = 8000;
/// Files up to this size are rewritten in memory; larger ones are scanned
/// in windows.
pub(crate) const MAX_REWRITE_BYTES: u64 = 32 * 1024 * 1024;
const WINDOW_BYTES: usize = 1024 * 1024;

/// One needle: a form of one variable's value.
struct Needle {
    name: String,
    bytes: Zeroizing<Vec<u8>>,
}

/// What happened to one file.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct FileRedaction {
    /// Relative to the workspace, `/`-separated.
    pub(crate) path: String,
    /// The variables whose values were found, sorted.
    pub(crate) names: Vec<String>,
    /// `true` when the file's change was dropped from the commit.
    pub(crate) dropped: bool,
}

/// Why the scan could not finish. Data-free; the turn then publishes
/// nothing rather than something unscanned.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum FileRedactError {
    Io,
}

/// The values to look for. Not `Debug` with values.
#[derive(Default)]
pub(crate) struct FileRedactor {
    needles: Vec<Needle>,
}

impl std::fmt::Debug for FileRedactor {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("FileRedactor")
            .field("needles", &self.needles.len())
            .finish()
    }
}

/// The base64 characters that depend on `value` alone when it is encoded
/// after `offset` (0, 1 or 2) other bytes: the encoding with the edge
/// characters that mix in neighbouring bits cut off.
fn base64_inner(engine: &impl base64::Engine, value: &[u8], offset: usize) -> Option<String> {
    let mut input = vec![0_u8; offset];
    input.extend_from_slice(value);
    let encoded = engine.encode(&input);
    let skip = (offset * 8).div_ceil(6);
    let full = (input.len() * 8) / 6;
    let inner = encoded.get(skip..full)?;
    (inner.len() >= MIN_REDACTED_CHARS).then(|| inner.to_owned())
}

/// Percent-encoding of everything but RFC 3986 unreserved characters, with
/// upper- or lower-case hex; `plus` writes a space as `+` (forms).
fn url_encoded(value: &str, upper: bool, plus: bool) -> String {
    let mut out = String::with_capacity(value.len() * 3);
    for byte in value.bytes() {
        match byte {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'.' | b'_' | b'~' => {
                out.push(char::from(byte));
            }
            b' ' if plus => out.push('+'),
            _ if upper => {
                let _ = write!(out, "%{byte:02X}");
            }
            _ => {
                let _ = write!(out, "%{byte:02x}");
            }
        }
    }
    out
}

impl FileRedactor {
    #[must_use]
    pub(crate) fn new(env: &SecretEnv) -> Self {
        let mut needles: Vec<Needle> = Vec::new();
        for (name, value) in env.redaction_pairs() {
            if value.chars().count() < MIN_REDACTED_CHARS {
                continue;
            }
            let raw = value.as_bytes();
            let mut forms: Vec<String> = vec![value.to_owned()];
            for offset in 0..3 {
                forms.extend(base64_inner(&STANDARD, raw, offset));
                forms.extend(base64_inner(&URL_SAFE, raw, offset));
            }
            for (upper, plus) in [(true, false), (false, false), (true, true), (false, true)] {
                forms.push(url_encoded(value, upper, plus));
            }
            for form in forms {
                let form = Zeroizing::new(form.into_bytes());
                if !needles
                    .iter()
                    .any(|seen| seen.name == name && *seen.bytes == *form)
                {
                    needles.push(Needle {
                        name: name.to_owned(),
                        bytes: form,
                    });
                }
            }
        }
        // Longest first, so a value is replaced before a shorter one in it.
        needles.sort_by_key(|needle| std::cmp::Reverse(needle.bytes.len()));
        Self { needles }
    }

    #[must_use]
    pub(crate) fn is_empty(&self) -> bool {
        self.needles.is_empty()
    }

    fn longest(&self) -> usize {
        self.needles.first().map_or(0, |needle| needle.bytes.len())
    }

    /// The names whose values occur in `data`.
    fn names_in(&self, data: &[u8]) -> BTreeSet<String> {
        self.needles
            .iter()
            .filter(|needle| memchr::memmem::find(data, &needle.bytes).is_some())
            .map(|needle| needle.name.clone())
            .collect()
    }

    /// Replace every occurrence with `[REDACTED:<NAME>]`; the names found.
    fn rewrite(&self, data: &[u8]) -> (Vec<u8>, BTreeSet<String>) {
        let mut out = data.to_vec();
        let mut names = BTreeSet::new();
        for needle in &self.needles {
            if memchr::memmem::find(&out, &needle.bytes).is_none() {
                continue;
            }
            names.insert(needle.name.clone());
            let marker = format!("[REDACTED:{}]", needle.name).into_bytes();
            let mut next = Vec::with_capacity(out.len());
            let mut rest = out.as_slice();
            while let Some(at) = memchr::memmem::find(rest, &needle.bytes) {
                next.extend_from_slice(&rest[..at]);
                next.extend_from_slice(&marker);
                rest = &rest[at + needle.bytes.len()..];
            }
            next.extend_from_slice(rest);
            out = next;
        }
        (out, names)
    }

    /// Scan a large file in overlapping windows.
    fn names_in_file(&self, path: &Path) -> Result<BTreeSet<String>, FileRedactError> {
        let mut file = fs::File::open(path).map_err(|_| FileRedactError::Io)?;
        let overlap = self.longest().saturating_sub(1);
        let mut window: Vec<u8> = Vec::with_capacity(WINDOW_BYTES + overlap);
        let mut buffer = vec![0_u8; WINDOW_BYTES];
        let mut names = BTreeSet::new();
        loop {
            let count = file.read(&mut buffer).map_err(|_| FileRedactError::Io)?;
            if count == 0 {
                break;
            }
            window.extend_from_slice(&buffer[..count]);
            names.extend(self.names_in(&window));
            let keep = window.len().min(overlap);
            window.drain(..window.len() - keep);
        }
        Ok(names)
    }

    /// Scan and redact the turn's tree `worktree` in place before it is
    /// committed. `parent` maps each path of the parent commit to its blob
    /// id and content reader, so unchanged files are skipped and a dropped
    /// change can be undone.
    ///
    /// # Errors
    ///
    /// A file could not be read or rewritten.
    pub(crate) fn redact_tree(
        &self,
        worktree: &Path,
        parent: &dyn ParentTree,
    ) -> Result<Vec<FileRedaction>, FileRedactError> {
        let mut out = Vec::new();
        if self.is_empty() {
            return Ok(out);
        }
        let mut files = BTreeMap::new();
        walk(worktree, worktree, &mut files)?;
        for (relative, path) in files {
            let metadata = fs::symlink_metadata(&path).map_err(|_| FileRedactError::Io)?;
            if metadata.file_type().is_symlink() {
                let target = fs::read_link(&path).map_err(|_| FileRedactError::Io)?;
                let bytes = target.to_string_lossy().into_owned().into_bytes();
                if parent.unchanged(&relative, &bytes) {
                    continue;
                }
                let names = self.names_in(&bytes);
                if !names.is_empty() {
                    drop_change(&path, &relative, parent)?;
                    out.push(FileRedaction {
                        path: relative,
                        names: names.into_iter().collect(),
                        dropped: true,
                    });
                }
                continue;
            }
            if metadata.len() > MAX_REWRITE_BYTES {
                if parent.unchanged_file(&relative, &path, metadata.len()) {
                    continue;
                }
                let names = self.names_in_file(&path)?;
                if !names.is_empty() {
                    drop_change(&path, &relative, parent)?;
                    out.push(FileRedaction {
                        path: relative,
                        names: names.into_iter().collect(),
                        dropped: true,
                    });
                }
                continue;
            }
            let data = fs::read(&path).map_err(|_| FileRedactError::Io)?;
            if parent.unchanged(&relative, &data) {
                continue;
            }
            let binary = data[..data.len().min(BINARY_PROBE_BYTES)].contains(&0);
            if binary {
                let names = self.names_in(&data);
                if !names.is_empty() {
                    drop_change(&path, &relative, parent)?;
                    out.push(FileRedaction {
                        path: relative,
                        names: names.into_iter().collect(),
                        dropped: true,
                    });
                }
                continue;
            }
            let (rewritten, names) = self.rewrite(&data);
            if !names.is_empty() {
                fs::write(&path, &rewritten).map_err(|_| FileRedactError::Io)?;
                out.push(FileRedaction {
                    path: relative,
                    names: names.into_iter().collect(),
                    dropped: false,
                });
            }
        }
        Ok(out)
    }
}

/// The parent commit's files, as the redaction needs them.
pub(crate) trait ParentTree {
    /// `true` when the parent has `path` with exactly `content`.
    fn unchanged(&self, path: &str, content: &[u8]) -> bool;
    /// As [`Self::unchanged`] for a file too large to read whole (by its
    /// blob id, computed streaming).
    fn unchanged_file(&self, path: &str, file: &Path, length: u64) -> bool;
    /// The parent's content and executable bit of `path`, if it had it as
    /// a regular file.
    fn content(&self, path: &str) -> Option<(Vec<u8>, bool)>;
}

/// Undo the turn's change to one file: the parent's content back, or the
/// file removed when the turn added it.
fn drop_change(
    path: &Path,
    relative: &str,
    parent: &dyn ParentTree,
) -> Result<(), FileRedactError> {
    use std::os::unix::fs::PermissionsExt as _;
    let _ = fs::remove_file(path);
    if let Some((content, executable)) = parent.content(relative) {
        fs::write(path, content).map_err(|_| FileRedactError::Io)?;
        let mode = if executable { 0o755 } else { 0o644 };
        fs::set_permissions(path, fs::Permissions::from_mode(mode))
            .map_err(|_| FileRedactError::Io)?;
    }
    Ok(())
}

/// Every file and symlink under `root`, `.git` skipped (the codec never
/// commits it), symlinks not followed.
fn walk(
    root: &Path,
    directory: &Path,
    out: &mut BTreeMap<String, std::path::PathBuf>,
) -> Result<(), FileRedactError> {
    for entry in fs::read_dir(directory).map_err(|_| FileRedactError::Io)? {
        let entry = entry.map_err(|_| FileRedactError::Io)?;
        if entry.file_name().eq_ignore_ascii_case(".git") {
            continue;
        }
        let path = entry.path();
        let kind = entry.file_type().map_err(|_| FileRedactError::Io)?;
        if kind.is_dir() {
            walk(root, &path, out)?;
        } else if kind.is_file() || kind.is_symlink() {
            let relative = path
                .strip_prefix(root)
                .map_err(|_| FileRedactError::Io)?
                .to_string_lossy()
                .replace('\\', "/");
            out.insert(relative, path);
        }
    }
    Ok(())
}

/// The visible notice of a turn whose files held harvested values: paths
/// and NAMES only.
#[must_use]
pub(crate) fn notice(redactions: &[FileRedaction]) -> Option<String> {
    if redactions.is_empty() {
        return None;
    }
    let mut text = String::from(
        "Note: files this turn wrote contained values of the environment variables handed off with this thread. Before the work was saved:",
    );
    for redaction in redactions {
        let names = redaction.names.join(", ");
        if redaction.dropped {
            let _ = write!(
                text,
                "\n- `{}` (binary, holds {names}): the change was not saved",
                redaction.path
            );
        } else {
            let _ = write!(
                text,
                "\n- `{}`: {names} replaced with [REDACTED:<NAME>]",
                redaction.path
            );
        }
    }
    Some(text)
}

/// The quarantine's view of the parent commit.
pub(crate) struct QuarantineParent {
    db: elitea_workspace_store::objects::ObjectDb,
    files: BTreeMap<String, elitea_workspace_store::objects::FileEntry>,
}

impl QuarantineParent {
    /// # Errors
    ///
    /// The parent commit is not in the quarantine.
    pub(crate) fn open(git_dir: &Path, parent: &str) -> Result<Self, FileRedactError> {
        use elitea_workspace_store::objects::{ObjectDb, oid};
        let db =
            ObjectDb::open_objects(&git_dir.join("objects")).map_err(|_| FileRedactError::Io)?;
        let id = oid(parent).map_err(|_| FileRedactError::Io)?;
        let commit = db.commit(&id).map_err(|_| FileRedactError::Io)?;
        let files = db.flatten(&commit.tree).map_err(|_| FileRedactError::Io)?;
        Ok(Self { db, files })
    }
}

fn blob_id(length: u64, mut reader: impl std::io::Read) -> Option<gix_hash::ObjectId> {
    let mut hasher = gix_hash::hasher(gix_hash::Kind::Sha1);
    hasher.update(format!("blob {length}\0").as_bytes());
    let mut buffer = vec![0_u8; 64 * 1024];
    loop {
        let count = reader.read(&mut buffer).ok()?;
        if count == 0 {
            break;
        }
        hasher.update(&buffer[..count]);
    }
    hasher.try_finalize().ok()
}

impl ParentTree for QuarantineParent {
    fn unchanged(&self, path: &str, content: &[u8]) -> bool {
        self.files.get(path).is_some_and(|entry| {
            blob_id(content.len() as u64, content).is_some_and(|id| id == entry.oid)
        })
    }

    fn unchanged_file(&self, path: &str, file: &Path, length: u64) -> bool {
        self.files.get(path).is_some_and(|entry| {
            fs::File::open(file)
                .ok()
                .and_then(|file| blob_id(length, file))
                .is_some_and(|id| id == entry.oid)
        })
    }

    fn content(&self, path: &str) -> Option<(Vec<u8>, bool)> {
        let entry = self.files.get(path)?;
        if entry.is_link() || entry.is_gitlink() {
            return None;
        }
        let content = self.db.blob(&entry.oid).ok()?;
        Some((content, entry.is_executable()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use base64::Engine as _;
    use base64::engine::general_purpose::STANDARD_NO_PAD;

    struct Parent(BTreeMap<String, Vec<u8>>);

    impl ParentTree for Parent {
        fn unchanged(&self, path: &str, content: &[u8]) -> bool {
            self.0.get(path).is_some_and(|seen| seen == content)
        }
        fn unchanged_file(&self, path: &str, file: &Path, _: u64) -> bool {
            self.unchanged(path, &fs::read(file).expect("read"))
        }
        fn content(&self, path: &str) -> Option<(Vec<u8>, bool)> {
            self.0.get(path).map(|content| (content.clone(), false))
        }
    }

    fn env() -> SecretEnv {
        SecretEnv::new(vec![
            ("API_TOKEN".into(), "tok-9f8e7d6c5b".into()),
            ("DB_PASSWORD".into(), "p@ss word/1".into()),
            ("SHORT".into(), "abc".into()),
        ])
    }

    fn values() -> [&'static str; 2] {
        ["tok-9f8e7d6c5b", "p@ss word/1"]
    }

    #[test]
    #[allow(clippy::too_many_lines, reason = "one tree, every case")]
    fn every_form_is_redacted_in_text_and_binaries_are_dropped() {
        let dir = tempfile::tempdir().expect("dir");
        let root = dir.path();
        let write = |name: &str, content: &[u8]| {
            let path = root.join(name);
            fs::create_dir_all(path.parent().expect("parent")).expect("mkdir");
            fs::write(path, content).expect("write");
        };
        // `env > out.txt`.
        write(
            "out.txt",
            b"PATH=/usr/bin\nAPI_TOKEN=tok-9f8e7d6c5b\nDB_PASSWORD=p@ss word/1\nSHORT=abc\n",
        );
        // Base64 at every alignment, padded and not, both alphabets.
        let token = values()[0].as_bytes();
        let mut encoded = String::new();
        for prefix in ["", "x", "xy"] {
            let mut input = prefix.as_bytes().to_vec();
            input.extend_from_slice(token);
            input.extend_from_slice(b"!?");
            encoded.push_str(&STANDARD.encode(&input));
            encoded.push('\n');
            encoded.push_str(&URL_SAFE.encode(&input));
            encoded.push('\n');
        }
        encoded.push_str(&STANDARD_NO_PAD.encode(values()[1]));
        write("config/encoded.txt", encoded.as_bytes());
        // URL-encoded in a connection string.
        write(
            "conn.txt",
            b"postgres://app:p%40ss%20word%2F1@db/app\nform=p%40ss+word%2F1\n",
        );
        // A binary that holds a value: added, and changed.
        let mut binary = vec![0_u8, 1, 2, 3];
        binary.extend_from_slice(token);
        write("added.bin", &binary);
        write("changed.bin", &binary);
        // Unchanged from the parent: never touched even though it holds one.
        write("kept.env", b"API_TOKEN=tok-9f8e7d6c5b\n");
        write("clean.txt", b"nothing here abc\n");
        let parent = Parent(BTreeMap::from([
            (
                "kept.env".to_owned(),
                b"API_TOKEN=tok-9f8e7d6c5b\n".to_vec(),
            ),
            ("changed.bin".to_owned(), b"\0original".to_vec()),
        ]));

        let redactor = FileRedactor::new(&env());
        let report = redactor.redact_tree(root, &parent).expect("redacted");
        let by_path: BTreeMap<&str, &FileRedaction> = report
            .iter()
            .map(|redaction| (redaction.path.as_str(), redaction))
            .collect();
        assert_eq!(
            by_path.keys().copied().collect::<Vec<_>>(),
            [
                "added.bin",
                "changed.bin",
                "config/encoded.txt",
                "conn.txt",
                "out.txt"
            ]
        );
        assert_eq!(by_path["out.txt"].names, ["API_TOKEN", "DB_PASSWORD"]);
        assert!(!by_path["out.txt"].dropped);
        assert!(by_path["added.bin"].dropped && by_path["changed.bin"].dropped);

        let out = fs::read_to_string(root.join("out.txt")).expect("out");
        assert!(out.contains("API_TOKEN=[REDACTED:API_TOKEN]"), "{out}");
        assert!(out.contains("DB_PASSWORD=[REDACTED:DB_PASSWORD]"), "{out}");
        assert!(
            out.contains("SHORT=abc"),
            "a value under 4 characters stays"
        );
        // No published byte holds a value in any form.
        for file in ["out.txt", "config/encoded.txt", "conn.txt"] {
            let content = fs::read(root.join(file)).expect("read");
            for needle in &redactor.needles {
                assert!(
                    memchr::memmem::find(&content, &needle.bytes).is_none(),
                    "{file} still holds a form of {}",
                    needle.name
                );
            }
        }
        assert!(
            !root.join("added.bin").exists(),
            "an added binary is dropped"
        );
        assert_eq!(
            fs::read(root.join("changed.bin")).expect("parent back"),
            b"\0original"
        );
        assert_eq!(
            fs::read(root.join("kept.env")).expect("kept"),
            b"API_TOKEN=tok-9f8e7d6c5b\n"
        );

        // The notice names paths and variables, never a value.
        let text = notice(&report).expect("notice");
        assert!(text.contains("out.txt") && text.contains("API_TOKEN"));
        assert!(text.contains("not saved"));
        for value in values() {
            assert!(!text.contains(value));
        }
        assert!(!format!("{redactor:?}").contains("tok-"));
    }

    #[test]
    fn a_large_file_is_scanned_in_windows_and_dropped() {
        let dir = tempfile::tempdir().expect("dir");
        let path = dir.path().join("big.log");
        let mut data = vec![b'a'; usize::try_from(MAX_REWRITE_BYTES).expect("usize") + 10];
        // Across a window boundary.
        let at = WINDOW_BYTES - 5;
        data[at..at + values()[0].len()].copy_from_slice(values()[0].as_bytes());
        fs::write(&path, &data).expect("write");
        let report = FileRedactor::new(&env())
            .redact_tree(dir.path(), &Parent(BTreeMap::new()))
            .expect("scan");
        assert_eq!(report.len(), 1);
        assert!(report[0].dropped);
        assert!(!path.exists());
    }

    #[test]
    fn no_env_is_no_scan() {
        let dir = tempfile::tempdir().expect("dir");
        fs::write(dir.path().join("a"), "tok-9f8e7d6c5b").expect("w");
        let report = FileRedactor::new(&SecretEnv::default())
            .redact_tree(dir.path(), &Parent(BTreeMap::new()))
            .expect("scan");
        assert_eq!(report, []);
    }
}
