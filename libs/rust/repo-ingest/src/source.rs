//! An artifact-folder source: `artifact://bucket[/prefix]`, parsed and
//! checked against elitea-main's bucket and object-key rules.
//!
//! A port of `elitea_deepwiki.artifact_source.parse_artifact_source`. What a
//! consumer NAMES a folder (a wiki id, a graph id) is the consumer's own
//! business; this module only says what the folder is.

use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::pyvalue::py_repr;
use regex::Regex;
use std::sync::LazyLock;

/// The scheme that marks an artifact folder rather than a git repository.
pub const ARTIFACT_SCHEME: &str = "artifact://";

/// elitea-main's bucket-name rule.
static BUCKET_PATTERN: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[a-z][a-z0-9-]{1,62}$").unwrap_or_else(|_| unreachable!()));

/// The longest object key elitea-main accepts, in bytes.
pub const MAX_KEY_BYTES: usize = 1024;

/// One artifact folder: a bucket, and an optional folder prefix without a
/// trailing slash ("" is the whole bucket).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ArtifactSource {
    pub bucket: String,
    pub prefix: String,
}

impl ArtifactSource {
    /// `artifact://{bucket}[/{prefix}]`, the canonical spelling.
    #[must_use]
    pub fn url(&self) -> String {
        if self.prefix.is_empty() {
            format!("{ARTIFACT_SCHEME}{}", self.bucket)
        } else {
            format!("{ARTIFACT_SCHEME}{}/{}", self.bucket, self.prefix)
        }
    }

    /// The prefix sent to the listing route: with a trailing slash, so
    /// `docs` does not also list `docs-archive/…` ("" for the whole bucket).
    #[must_use]
    pub fn list_prefix(&self) -> String {
        if self.prefix.is_empty() {
            String::new()
        } else {
            format!("{}/", self.prefix)
        }
    }

    /// Python's `slug`: `bucket[_prefix]` with every run of characters
    /// outside `A-Za-z0-9._-` folded to one `_`.
    #[must_use]
    pub fn slug(&self) -> String {
        let raw = if self.prefix.is_empty() {
            self.bucket.clone()
        } else {
            format!("{}_{}", self.bucket, self.prefix)
        };
        fold_unsafe_runs(&raw)
    }
}

/// `re.sub(r"[^A-Za-z0-9._-]+", "_", text)`.
#[must_use]
pub fn fold_unsafe_runs(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut in_run = false;
    for c in text.chars() {
        if c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-') {
            out.push(c);
            in_run = false;
        } else if !in_run {
            out.push('_');
            in_run = true;
        }
    }
    out
}

/// Report whether a repository string names an artifact folder.
#[must_use]
pub fn is_artifact_source(repository: &str) -> bool {
    repository
        .trim()
        .to_lowercase()
        .starts_with(ARTIFACT_SCHEME)
}

fn source_error(message: String) -> EngineError {
    EngineError::new(ErrorType::Value, message)
}

/// Parse `artifact://bucket[/prefix]`, refusing anything else.
///
/// # Errors
///
/// A `ValueError` (Python's `ArtifactSourceError`) for a string that is not
/// an artifact source, an unusable bucket name or an unsafe prefix.
pub fn parse_artifact_source(repository: &str) -> Result<ArtifactSource, EngineError> {
    if !is_artifact_source(repository) {
        return Err(source_error(format!(
            "{} is not an artifact source; expected {ARTIFACT_SCHEME}bucket[/prefix]",
            py_repr(repository)
        )));
    }
    let stripped = repository.trim();
    let remainder: String = stripped
        .chars()
        .skip(ARTIFACT_SCHEME.chars().count())
        .collect();
    let (bucket, prefix) = remainder
        .split_once('/')
        .unwrap_or((remainder.as_str(), ""));
    let bucket = bucket.trim().to_lowercase();
    if !BUCKET_PATTERN.is_match(&bucket) {
        return Err(source_error(format!(
            "{} is not a usable bucket name: a bucket starts with a letter and holds 2 to 63 lowercase letters, digits or hyphens",
            py_repr(&bucket)
        )));
    }
    Ok(ArtifactSource {
        bucket,
        prefix: normalise_prefix(prefix)?,
    })
}

fn normalise_prefix(prefix: &str) -> Result<String, EngineError> {
    let prefix = prefix.trim().trim_matches('/');
    if prefix.is_empty() {
        return Ok(String::new());
    }
    refuse_unsafe_key(prefix, "folder prefix")?;
    Ok(prefix.to_owned())
}

/// elitea-main's object-key rules (`internal/infra/storage/ref.go`): at
/// most [`MAX_KEY_BYTES`], no NUL or backslash, no empty, `.` or `..`
/// segment (so no leading or trailing slash either).
///
/// # Errors
///
/// A `ValueError` naming `what` and the rule the key breaks.
pub fn refuse_unsafe_key(key: &str, what: &str) -> Result<(), EngineError> {
    if key.len() > MAX_KEY_BYTES {
        return Err(source_error(format!(
            "{what} is longer than {MAX_KEY_BYTES} bytes"
        )));
    }
    if key.contains('\0') || key.contains('\\') {
        return Err(source_error(format!(
            "{what} {} holds a character a key may not hold",
            py_repr(key)
        )));
    }
    if key
        .split('/')
        .any(|segment| matches!(segment, "" | "." | ".."))
    {
        return Err(source_error(format!(
            "{what} {} holds an empty or relative segment",
            py_repr(key)
        )));
    }
    Ok(())
}
