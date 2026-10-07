//! An ARTIFACT-FOLDER source: a folder in the invoking project's artifact
//! store instead of a git repository. A port of
//! `elitea_deepwiki.artifact_source` and the listing and download half of
//! `engine.artifacts_platform_client`.
//!
//! The engine indexes a directory. A clone is one way to get it; this
//! module produces the same directory from a folder, so discovery, the
//! graph, the index, the manifest and the wiki id are unchanged after it.
//!
//! * **The source** is `artifact://{bucket}[/{prefix}]` in
//!   `repo_config.repository` (the Go host also sets `provider_type:
//!   artifact`; [`crate::source::parse_artifact_source`] parses it).
//! * **The credential** is the invocation's callback bearer, the one its
//!   model calls use: `llm_settings.api_base` without its `/llm/v1`-style
//!   suffix is the platform, `api_key` the bearer, `organization` (or
//!   `openai_organization`, `project_id`) the project. Python read the same
//!   three (`extract_artifact_settings`). The project in the URL is the
//!   grant's own (elitea-main checks the bearer against it), so a source can
//!   only name a bucket of the caller's project.
//! * **The routes** are elitea-main's object API:
//!   `GET {platform}/api/v2/artifacts/objects/{project}/{bucket}?prefix=&cursor=`
//!   lists (`objects[].key`, `size_bytes`, `modified_at`, `next_cursor`) and
//!   `GET …/{bucket}/{key}` downloads, each with `Authorization: Bearer`.
//! * **The identity** is the sha256 over the sorted listing, one line per
//!   object, `{key}\0{size}\0{modified}\n`, byte for byte Python's
//!   `listing_digest`. It stands where a clone's commit sha stands, so the
//!   repo identifier is `artifact://bucket/prefix:{branch}:{digest[:8]}` and
//!   the wiki id `artifact--bucket--prefix--{branch}`, as Python's worker
//!   derives them. The branch is a label: `repo_config.branch`, else `main`.
//!
//! # Security
//!
//! The listing is data from a server, and the object keys are names a user
//! chose. So:
//!
//! * every key is held to elitea-main's own key rules (no NUL, no
//!   backslash, no empty, `.` or `..` segment, so no absolute path), and to
//!   the folder it was listed under, before anything is written; a key that
//!   breaks one refuses the whole source, as Python's did;
//! * each file is created with `O_CREAT|O_EXCL` in a directory this module
//!   created, after the parent's canonical path is checked to be inside the
//!   job directory. No symbolic link is ever created;
//! * the git ingest's limits apply (`MAX_FILE_COUNT`,
//!   `MAX_FILE_BYTES`, `MAX_CLONE_BYTES` over the total, `MAX_PARSED_BYTES`,
//!   `CLONE_TIMEOUT_SECONDS`), once over the listed sizes before any
//!   download and again over the bytes actually received, plus Python's own
//!   caps (`ARTIFACT_MAX_FILES` 5000,
//!   `ARTIFACT_MAX_BYTES` 512 MiB);
//! * the only host is the invocation's `api_base`. Nothing in
//!   `repo_config` or in a listing reaches the URL except as an encoded path
//!   segment or query value; redirects are not followed (the model
//!   transport's client: rustls, the engine's CA bundle). The git
//!   allowlist does not apply, as in Go's `CheckEgress`: the platform is not
//!   an external git host;
//! * the bearer is sent as a sensitive header and never appears in a log
//!   line or an error;
//! * every request has a timeout (60 s a listing page, 300 s a download, as
//!   Python), the whole ingest has `CLONE_TIMEOUT_SECONDS`, and a stop ends
//!   it within 100 ms. A partial directory is removed.
//!
//! # Deliberate differences from Python
//!
//! * A listing that fails (an HTTP status other than 200 or 404, a body
//!   that is not JSON, the 1000-page ceiling) is an error. Python logged a
//!   warning and indexed what it had read so far, under an identity of that
//!   partial listing. A 404 on the first page is still an empty listing,
//!   so an unknown bucket is still "holds no objects to index"; a 404 on a
//!   later page (after a cursor) is an error, as the listing is partial.
//! * The folder is downloaded into the job's own scratch directory and
//!   removed with it; there is no cache to reuse across runs and no marker
//!   file (every native run builds).
//! * An `artifact://` repository is a folder whatever `provider_type`
//!   says; Python failed such a request with `github` (`'owner/repo'
//!   format`). A `provider_type: artifact` whose repository is not
//!   `artifact://…` is refused (the Go host never sends one).
//! * The project must be named: Python sent an empty one and read a 404.
//! * Python's caps read an unparsable value as the default; here, as every
//!   other limit, it fails the start.

use super::ClonedRepository;
use super::identity::RepoIdentity;
use super::limits::{IngestLimits, TreeBudget, TreeStats};
use super::secret::Secret;
use crate::names::SettingNames;
use crate::source::{
    ArtifactSource, fold_unsafe_runs, is_artifact_source, parse_artifact_source, refuse_unsafe_key,
};
use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::pystr;
use elitea_engine_core::pyvalue::{py_repr, py_str, py_truthy};
use regex::Regex;
use reqwest::{Client, Response, StatusCode, Url};
use serde_json::{Map, Value};
use sha2::{Digest, Sha256};
use std::fmt;
use std::fmt::Write as _;
use std::future::Future;
use std::path::{Path, PathBuf};
use std::sync::LazyLock;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;
use tokio::io::AsyncWriteExt;

/// The `provider_type` the Go host and Python's extractors emit.
pub const ARTIFACT_PROVIDER_TYPE: &str = "artifact";

/// Python's `ARTIFACT_MAX_FILES` default.
pub const DEFAULT_MAX_FILES: u64 = 5000;

/// Python's `ARTIFACT_MAX_BYTES` default.
pub const DEFAULT_MAX_BYTES: u64 = 512 * 1024 * 1024;

/// Python's `MAX_LIST_PAGES`.
pub const MAX_LIST_PAGES: usize = 1000;

/// One listing page (Python: `timeout=60`).
const LIST_TIMEOUT: Duration = Duration::from_mins(1);

/// One download (Python: `timeout=300`).
const DOWNLOAD_TIMEOUT: Duration = Duration::from_mins(5);

/// The largest listing page read. elitea-main pages at 1000 keys of at
/// most 1 KiB each.
const MAX_LIST_PAGE_BYTES: usize = 16 * 1024 * 1024;

/// How often a transfer looks at the stop flag.
const CANCEL_POLL: Duration = Duration::from_millis(100);

/// The listing route below the platform base.
const OBJECTS_ROUTE: [&str; 4] = ["api", "v2", "artifacts", "objects"];

/// `re.sub(r'/llm(/api)?(/v\d+)?/?$', '', api_base)`.
static LLM_SUFFIX: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"/llm(/api)?(/v\d+)?/?$").unwrap_or_else(|_| unreachable!()));

/// Python's two artifact caps, on top of the ingest limits.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ArtifactCaps {
    /// `ARTIFACT_MAX_FILES`.
    pub max_files: u64,
    /// `ARTIFACT_MAX_BYTES`.
    pub max_bytes: u64,
}

impl Default for ArtifactCaps {
    fn default() -> Self {
        Self {
            max_files: DEFAULT_MAX_FILES,
            max_bytes: DEFAULT_MAX_BYTES,
        }
    }
}

fn value_error(message: String) -> EngineError {
    EngineError::new(ErrorType::Value, message)
}

fn runtime_error(message: String) -> EngineError {
    EngineError::new(ErrorType::Runtime, message)
}

// ---------------------------------------------------------------------------
// The platform and its credential
// ---------------------------------------------------------------------------

/// Python's `base_url`: `api_base` without its `/llm[/api][/vN][/]` suffix.
#[must_use]
pub fn platform_base(api_base: &str) -> String {
    LLM_SUFFIX.replace(api_base, "").into_owned()
}

/// `settings.get(a) or settings.get(b) or …` as a string.
fn first_text(settings: &Map<String, Value>, keys: &[&str]) -> String {
    keys.iter()
        .find_map(|key| settings.get(*key).filter(|v| py_truthy(v)).map(py_str))
        .unwrap_or_default()
}

/// The platform object API one invocation may read: the base URL, the
/// callback bearer and the project, from its `llm_settings`.
///
/// `Debug` is safe to print: the bearer is a [`Secret`].
#[derive(Clone, PartialEq, Eq)]
pub struct PlatformObjects {
    base: Url,
    api_key: Secret,
    project_id: String,
}

impl fmt::Debug for PlatformObjects {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("PlatformObjects")
            .field("base", &self.base.as_str())
            .field("api_key", &self.api_key)
            .field("project_id", &self.project_id)
            .finish()
    }
}

impl PlatformObjects {
    /// Read `api_base` / `api_key` / `organization` as Python's
    /// `extract_artifact_settings` did.
    ///
    /// # Errors
    ///
    /// A `ValueError` when the block has no base URL, no bearer or no
    /// project, or the base is not an absolute http(s) URL without
    /// credentials, query or fragment.
    pub fn from_llm_settings(llm_settings: &Value) -> Result<Self, EngineError> {
        let empty = Map::new();
        let settings = llm_settings.as_object().unwrap_or(&empty);
        let api_base = first_text(settings, &["api_base", "openai_api_base"]);
        let api_key = Secret::new(first_text(settings, &["api_key", "openai_api_key"]));
        let (Some(api_key), false) = (api_key, api_base.trim().is_empty()) else {
            return Err(value_error(
                "an artifact source needs the platform artifact credentials (llm_settings.api_base and api_key), and this invocation has none".to_owned(),
            ));
        };
        let project_id = first_text(
            settings,
            &["organization", "openai_organization", "project_id"],
        );
        if project_id.trim().is_empty() {
            return Err(value_error(
                "an artifact source needs the invoking project (llm_settings.organization), and this invocation names none".to_owned(),
            ));
        }
        let base = platform_base(api_base.trim());
        let invalid = || {
            value_error(
                "llm_settings.api_base must be an absolute http(s) URL without credentials, query or fragment".to_owned(),
            )
        };
        let base = Url::parse(&base).map_err(|_| invalid())?;
        if !matches!(base.scheme(), "http" | "https")
            || base.host_str().is_none()
            || !base.username().is_empty()
            || base.password().is_some()
            || base.query().is_some()
            || base.fragment().is_some()
        {
            return Err(invalid());
        }
        Ok(Self {
            base,
            api_key,
            project_id,
        })
    }

    /// The platform base URL (no credential in it).
    #[must_use]
    pub fn base(&self) -> &Url {
        &self.base
    }

    #[must_use]
    pub fn project_id(&self) -> &str {
        &self.project_id
    }

    /// `{base}/api/v2/artifacts/objects/{project}/{bucket}[/{key…}]`, every
    /// segment percent-encoded on its own (a key's `/` stays a separator,
    /// as Python's `quote(name, safe="/")`).
    fn url(&self, bucket: &str, key: Option<&str>) -> Url {
        let mut url = self.base.clone();
        if let Ok(mut segments) = url.path_segments_mut() {
            segments.pop_if_empty();
            segments.extend(OBJECTS_ROUTE);
            segments.push(&self.project_id);
            segments.push(bucket);
            if let Some(key) = key {
                segments.extend(key.split('/'));
            }
        }
        url
    }

    async fn get(
        &self,
        client: &Client,
        url: Url,
        timeout: Duration,
        cancel: &AtomicBool,
    ) -> Result<Result<Response, reqwest::Error>, EngineError> {
        interruptible(
            client
                .get(url)
                .bearer_auth(self.api_key.expose())
                .timeout(timeout)
                .send(),
            cancel,
        )
        .await
    }
}

// ---------------------------------------------------------------------------
// The source in a repo_config
// ---------------------------------------------------------------------------

/// Whether a `repo_config` names an artifact folder: `provider_type:
/// artifact` (the Go host) or an `artifact://` repository.
#[must_use]
pub fn names_artifact_folder(repo_config: &Value) -> bool {
    let field = |key: &str| {
        repo_config
            .get(key)
            .filter(|v| py_truthy(v))
            .map(py_str)
            .unwrap_or_default()
    };
    field("provider_type")
        .trim()
        .eq_ignore_ascii_case(ARTIFACT_PROVIDER_TYPE)
        || is_artifact_source(&field("repository"))
}

/// An artifact folder a generation reads, from its `repo_config`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ArtifactTarget {
    source: ArtifactSource,
    repository: String,
    branch: String,
}

impl ArtifactTarget {
    /// The folder `repo_config` names, or `None` for a git source.
    ///
    /// # Errors
    ///
    /// A `ValueError` for an unusable `artifact://` string, or a
    /// `provider_type: artifact` whose repository is not one.
    pub fn from_repo_config(repo_config: &Value) -> Result<Option<Self>, EngineError> {
        if !names_artifact_folder(repo_config) {
            return Ok(None);
        }
        let text = |key: &str| {
            repo_config
                .get(key)
                .filter(|v| py_truthy(v))
                .map(py_str)
                .unwrap_or_default()
        };
        let repository = text("repository");
        if !is_artifact_source(&repository) {
            return Err(value_error(format!(
                "repo_config names provider_type 'artifact' but its repository {} is not artifact://bucket[/prefix]",
                py_repr(&repository)
            )));
        }
        let source = parse_artifact_source(&repository)?;
        // `canonical_repository_path`: what the identifier is built from.
        let canonical = pystr::strip_chars(pystr::strip(&repository), "/").to_owned();
        // `(branch or "main").strip() or "main"`.
        let branch = text("branch");
        let branch = match pystr::strip(&branch) {
            "" => "main".to_owned(),
            stripped => stripped.to_owned(),
        };
        Ok(Some(Self {
            source,
            repository: canonical,
            branch,
        }))
    }

    #[must_use]
    pub fn source(&self) -> &ArtifactSource {
        &self.source
    }

    /// The repository the identifier is built from (Python's
    /// `canonical_repository_path`: the string as given, stripped).
    #[must_use]
    pub fn repository(&self) -> &str {
        &self.repository
    }

    /// The branch label.
    #[must_use]
    pub fn branch(&self) -> &str {
        &self.branch
    }
}

// ---------------------------------------------------------------------------
// The listing and the identity
// ---------------------------------------------------------------------------

/// One object to download.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ArtifactObject {
    pub key: String,
    /// Python's `int(size)`; may be anything the server sent.
    pub size: i128,
    pub modified: String,
    /// The path inside the materialised directory.
    pub relative_path: String,
}

impl ArtifactObject {
    /// The size as a byte count for the limits (a negative size counts 0).
    #[must_use]
    pub fn byte_size(&self) -> u64 {
        u64::try_from(self.size.max(0)).unwrap_or(u64::MAX)
    }
}

/// Python's `int(value)` with `(TypeError, ValueError)` read as 0.
fn python_int(value: Option<&Value>) -> i128 {
    match value {
        Some(Value::Bool(flag)) => i128::from(*flag),
        Some(Value::Number(number)) => {
            if let Some(n) = number.as_i64() {
                i128::from(n)
            } else if let Some(n) = number.as_u64() {
                i128::from(n)
            } else {
                // A float truncates toward zero (`int(7.9) == 7`).
                #[allow(clippy::cast_possible_truncation)]
                number
                    .as_f64()
                    .filter(|f| f.is_finite())
                    .map_or(0, |f| f.trunc() as i128)
            }
        }
        Some(Value::String(text)) => parse_python_int(text).unwrap_or(0),
        _ => 0,
    }
}

/// `int(str)`: surrounding whitespace, a sign, ASCII digits with single
/// underscores between them.
fn parse_python_int(text: &str) -> Option<i128> {
    let text = pystr::strip(text);
    let (negative, digits) = match text.as_bytes().first() {
        Some(b'-') => (true, &text[1..]),
        Some(b'+') => (false, &text[1..]),
        _ => (false, text),
    };
    if digits.is_empty()
        || digits.starts_with('_')
        || digits.ends_with('_')
        || digits.contains("__")
        || !digits.chars().all(|c| c.is_ascii_digit() || c == '_')
    {
        return None;
    }
    let value: i128 = digits.replace('_', "").parse().ok()?;
    Some(if negative { -value } else { value })
}

/// The objects a raw listing holds, in listing order, refusing the unsafe.
///
/// `items` are the route's `objects[]` entries; Python's client mapped each
/// to `{name: key, size: size_bytes, modified: modified_at}` and
/// `collect_objects` read those.
///
/// # Errors
///
/// A `ValueError` for a key that breaks elitea-main's key rules or is not
/// inside the folder (Python's messages).
pub fn collect_objects(
    source: &ArtifactSource,
    items: &[Value],
) -> Result<Vec<ArtifactObject>, EngineError> {
    let prefix = source.list_prefix();
    let mut objects = Vec::new();
    for item in items {
        let Value::Object(item) = item else {
            continue;
        };
        let key = match item.get("key") {
            Some(value) if py_truthy(value) => pystr::strip(&py_str(value)).to_owned(),
            _ => String::new(),
        };
        if key.is_empty() || key.ends_with('/') {
            // A folder placeholder holds no bytes to index.
            continue;
        }
        refuse_unsafe_key(&key, "object key")?;
        if !prefix.is_empty() && !key.starts_with(&prefix) {
            return Err(value_error(format!(
                "object key {} is not inside the folder {}",
                py_repr(&key),
                py_repr(&prefix)
            )));
        }
        let relative = key[prefix.len()..].to_owned();
        if relative.is_empty() {
            continue;
        }
        let modified = match item.get("modified_at") {
            Some(value) if py_truthy(value) => py_str(value),
            _ => String::new(),
        };
        objects.push(ArtifactObject {
            size: python_int(item.get("size_bytes")),
            key,
            modified,
            relative_path: relative,
        });
    }
    Ok(objects)
}

/// The sha256 that stands in for a commit sha: over the listing sorted by
/// key, one `{key}\0{size}\0{modified}\n` line per object (Python's
/// `listing_digest`, byte for byte).
#[must_use]
pub fn listing_digest(objects: &[ArtifactObject]) -> String {
    let mut sorted: Vec<&ArtifactObject> = objects.iter().collect();
    // Stable, and `str` order is code-point order in both languages.
    sorted.sort_by(|a, b| a.key.cmp(&b.key));
    let mut digest = Sha256::new();
    for entry in sorted {
        digest.update(format!("{}\0{}\0{}\n", entry.key, entry.size, entry.modified).as_bytes());
    }
    digest
        .finalize()
        .iter()
        .fold(String::with_capacity(64), |mut hex, byte| {
            let _ = write!(hex, "{byte:02x}");
            hex
        })
}

/// Python's `check_caps`: no objects, too many, too many bytes.
///
/// # Errors
///
/// A `ValueError` naming the cap, in Python's words.
pub fn check_caps(
    objects: &[ArtifactObject],
    source: &ArtifactSource,
    caps: ArtifactCaps,
    names: &SettingNames,
) -> Result<(), EngineError> {
    let url = source.url();
    if objects.is_empty() {
        return Err(value_error(format!(
            "the artifact folder {url} holds no objects to index"
        )));
    }
    let count = objects.len() as u64;
    if count > caps.max_files {
        return Err(value_error(format!(
            "the artifact folder {url} holds {count} objects, over the limit of {} ({})",
            caps.max_files, names.artifact_max_files
        )));
    }
    let total: i128 = objects.iter().map(|o| o.size).sum();
    if total > i128::from(caps.max_bytes) {
        return Err(value_error(format!(
            "the artifact folder {url} holds {total} bytes, over the limit of {} ({})",
            caps.max_bytes, names.artifact_max_bytes
        )));
    }
    Ok(())
}

/// Python's directory name: `{slug}_{branch label}_{digest[:8]}`, the
/// branch folded to safe characters.
#[must_use]
pub fn directory_name(source: &ArtifactSource, branch: &str, digest: &str) -> String {
    let label = match fold_unsafe_runs(branch) {
        label if label.is_empty() => "main".to_owned(),
        label => label,
    };
    format!(
        "{}_{label}_{}",
        source.slug(),
        digest.get(..8).unwrap_or(digest)
    )
}

// ---------------------------------------------------------------------------
// The transfer
// ---------------------------------------------------------------------------

/// Run `future`, giving up within [`CANCEL_POLL`] of `cancel` being set.
async fn interruptible<F: Future>(
    future: F,
    cancel: &AtomicBool,
) -> Result<F::Output, EngineError> {
    tokio::pin!(future);
    loop {
        if cancel.load(Ordering::Acquire) {
            return Err(EngineError::cancelled());
        }
        tokio::select! {
            output = &mut future => return Ok(output),
            () = tokio::time::sleep(CANCEL_POLL) => {}
        }
    }
}

/// A request that never got a status, or a body cut short. The message
/// names the transfer and the cause; neither ever holds the header.
fn transfer_error(action: &str, error: &reqwest::Error) -> EngineError {
    let cause = if error.is_timeout() {
        "timed out".to_owned()
    } else if error.is_connect() {
        "could not connect".to_owned()
    } else {
        elitea_engine_core::errors::error_chain(error)
    };
    runtime_error(format!("Failed to {action}: {cause}"))
}

/// The `objects[]` of every listing page under `source`'s folder.
///
/// Stops early, with Python's count message, once there are more objects
/// than any limit admits: a server that pages forever cannot hold the run.
async fn list_folder(
    platform: &PlatformObjects,
    client: &Client,
    source: &ArtifactSource,
    most_objects: u64,
    names: &SettingNames,
    cancel: &AtomicBool,
) -> Result<Vec<Value>, EngineError> {
    let prefix = source.list_prefix();
    let target = format!("list the artifact folder {}", source.url());
    let mut items = Vec::new();
    let mut objects: u64 = 0;
    let mut cursor = String::new();
    for _ in 0..MAX_LIST_PAGES {
        let mut url = platform.url(&source.bucket, None);
        if !prefix.is_empty() || !cursor.is_empty() {
            let mut query = url.query_pairs_mut();
            if !prefix.is_empty() {
                query.append_pair("prefix", &prefix);
            }
            if !cursor.is_empty() {
                query.append_pair("cursor", &cursor);
            }
        }
        let response = platform
            .get(client, url, LIST_TIMEOUT, cancel)
            .await?
            .map_err(|e| transfer_error(&target, &e))?;
        match response.status() {
            // Python: an unknown bucket lists as empty. Only the first
            // page can say so: a 404 after a cursor (the bucket removed
            // mid-listing, a stale cursor) leaves the listing partial, and
            // a partial listing must not be indexed as the whole folder.
            StatusCode::NOT_FOUND if cursor.is_empty() => return Ok(items),
            StatusCode::NOT_FOUND => {
                return Err(runtime_error(format!(
                    "Failed to {target}: HTTP 404 on a later listing page; the listing is incomplete"
                )));
            }
            StatusCode::OK => {}
            status => {
                return Err(runtime_error(format!(
                    "Failed to {target}: HTTP {}",
                    status.as_u16()
                )));
            }
        }
        let body = read_bounded(response, MAX_LIST_PAGE_BYTES, cancel)
            .await?
            .map_err(|e| transfer_error(&target, &e))?
            .ok_or_else(|| {
                runtime_error(format!(
                    "Failed to {target}: a listing page is larger than {MAX_LIST_PAGE_BYTES} bytes"
                ))
            })?;
        let page: Value = serde_json::from_slice(&body).map_err(|_| {
            runtime_error(format!("Failed to {target}: a listing page is not JSON"))
        })?;
        let Value::Object(page) = page else {
            return Err(runtime_error(format!(
                "Failed to {target}: a listing page is not a JSON object"
            )));
        };
        if let Some(Value::Array(page_items)) = page.get("objects") {
            for item in page_items {
                let holds_bytes = item
                    .get("key")
                    .filter(|v| py_truthy(v))
                    .map(py_str)
                    .is_some_and(|key| {
                        let key = pystr::strip(&key);
                        !key.is_empty() && !key.ends_with('/')
                    });
                if holds_bytes {
                    objects += 1;
                }
                items.push(item.clone());
            }
        }
        if objects > most_objects {
            return Err(value_error(format!(
                "the artifact folder {} holds more than {most_objects} objects, over the limit of {} / {}",
                source.url(),
                names.artifact_max_files,
                names.max_file_count
            )));
        }
        // Omitted when the listing is exhausted.
        cursor = page
            .get("next_cursor")
            .filter(|v| py_truthy(v))
            .map(py_str)
            .unwrap_or_default();
        if cursor.is_empty() {
            return Ok(items);
        }
    }
    Err(runtime_error(format!(
        "Failed to {target}: the listing did not end within {MAX_LIST_PAGES} pages"
    )))
}

/// A body of at most `limit` bytes (`Ok(None)` when it is longer).
async fn read_bounded(
    mut response: Response,
    limit: usize,
    cancel: &AtomicBool,
) -> Result<Result<Option<Vec<u8>>, reqwest::Error>, EngineError> {
    let mut body = Vec::new();
    loop {
        match interruptible(response.chunk(), cancel).await? {
            Err(error) => return Ok(Err(error)),
            Ok(None) => return Ok(Ok(Some(body))),
            Ok(Some(chunk)) => {
                if body.len() + chunk.len() > limit {
                    return Ok(Ok(None));
                }
                body.extend_from_slice(&chunk);
            }
        }
    }
}

/// The path `relative` takes under `root`, its parent directories created.
///
/// The key rules already refuse `..`; this is the second check, made
/// against the filesystem: every directory on the way must be a real
/// directory (not a link, not a file), and the parent's canonical path
/// must be inside the root's.
fn prepare_path(
    root: &Path,
    canonical_root: &Path,
    relative: &str,
) -> Result<PathBuf, EngineError> {
    let outside = || {
        value_error(format!(
            "object {} would be written outside the source",
            py_repr(relative)
        ))
    };
    let segments: Vec<&str> = relative.split('/').collect();
    if segments
        .iter()
        .any(|segment| matches!(*segment, "" | "." | "..") || segment.contains(['\\', '\0']))
    {
        return Err(outside());
    }
    let Some((file, parents)) = segments.split_last() else {
        return Err(outside());
    };
    let mut directory = root.to_path_buf();
    for segment in parents {
        directory.push(segment);
        match std::fs::symlink_metadata(&directory) {
            Ok(meta) if meta.is_dir() && !meta.file_type().is_symlink() => {}
            Ok(_) => {
                return Err(value_error(format!(
                    "object {} would be written below a file",
                    py_repr(relative)
                )));
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                std::fs::create_dir(&directory).map_err(|e| {
                    runtime_error(format!(
                        "Cannot create a directory for the artifact {}: {e}",
                        py_repr(relative)
                    ))
                })?;
            }
            Err(error) => {
                return Err(runtime_error(format!(
                    "Cannot inspect a directory for the artifact {}: {error}",
                    py_repr(relative)
                )));
            }
        }
    }
    let canonical = std::fs::canonicalize(&directory).map_err(|_| outside())?;
    if !canonical.starts_with(canonical_root) {
        return Err(outside());
    }
    Ok(directory.join(file))
}

/// The bytes received so far, against every limit.
struct Received<'a> {
    limits: &'a IngestLimits,
    caps: ArtifactCaps,
    repo: &'a str,
    url: String,
    total: u64,
}

impl Received<'_> {
    fn admit(&mut self, relative: &str, file_bytes: u64, more: u64) -> Result<(), EngineError> {
        if file_bytes > self.limits.max_file_bytes {
            return Err(value_error(format!(
                "The file {} in {} is at least {file_bytes} bytes, over {}={}",
                py_repr(relative),
                py_repr(self.repo),
                self.limits.names.max_file_bytes,
                self.limits.max_file_bytes
            )));
        }
        self.total = self.total.saturating_add(more);
        if self.total > self.limits.max_clone_bytes {
            return Err(self.limits.clone_bytes_error(self.repo, self.total));
        }
        if self.total > self.caps.max_bytes {
            return Err(value_error(format!(
                "the artifact folder {} holds at least {} bytes, over the limit of {} ({})",
                self.url, self.total, self.caps.max_bytes, self.limits.names.artifact_max_bytes
            )));
        }
        Ok(())
    }
}

/// Download one object into `path` (created new), returning its size.
async fn download(
    platform: &PlatformObjects,
    client: &Client,
    bucket: &str,
    object: &ArtifactObject,
    path: &Path,
    received: &mut Received<'_>,
    cancel: &AtomicBool,
) -> Result<u64, EngineError> {
    let name = format!("{bucket}/{}", object.key);
    let action = format!("download artifact {}", py_repr(&name));
    let url = platform.url(bucket, Some(&object.key));
    let mut response = platform
        .get(client, url, DOWNLOAD_TIMEOUT, cancel)
        .await?
        .map_err(|e| transfer_error(&action, &e))?;
    match response.status() {
        StatusCode::OK => {}
        StatusCode::FORBIDDEN => {
            return Err(runtime_error(format!(
                "Not authorized to access artifact (HTTP 403): {name}"
            )));
        }
        StatusCode::NOT_FOUND => {
            return Err(runtime_error(format!("Artifact not found: {name}")));
        }
        status => {
            return Err(runtime_error(format!(
                "Failed to download artifact: HTTP {} ({name})",
                status.as_u16()
            )));
        }
    }
    let write_error = |e: std::io::Error| {
        runtime_error(format!(
            "Cannot write the artifact {}: {e}",
            py_repr(&object.relative_path)
        ))
    };
    // O_CREAT|O_EXCL: never through an existing entry, link or not.
    let mut file = tokio::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(path)
        .await
        .map_err(write_error)?;
    let mut size: u64 = 0;
    loop {
        let chunk = interruptible(response.chunk(), cancel)
            .await?
            .map_err(|e| transfer_error(&action, &e))?;
        let Some(chunk) = chunk else { break };
        let more = chunk.len() as u64;
        size = size.saturating_add(more);
        received.admit(&object.relative_path, size, more)?;
        file.write_all(&chunk).await.map_err(write_error)?;
    }
    file.flush().await.map_err(write_error)?;
    Ok(size)
}

/// Removes a partly written directory unless disarmed.
struct PartialDirectory(Option<PathBuf>);

impl Drop for PartialDirectory {
    fn drop(&mut self) {
        if let Some(path) = self.0.take() {
            let _ = std::fs::remove_dir_all(path);
        }
    }
}

/// Everything one materialisation needs.
pub struct Materialise<'a> {
    pub target: &'a ArtifactTarget,
    pub platform: &'a PlatformObjects,
    /// The model transport's client (rustls, no redirects).
    pub client: &'a Client,
    pub limits: &'a IngestLimits,
    pub caps: ArtifactCaps,
    pub job_scratch: &'a Path,
    pub cancel: &'a AtomicBool,
}

impl Materialise<'_> {
    /// List the folder, admit it, and download every object into
    /// `{job_scratch}/{slug}_{branch}_{digest8}`.
    ///
    /// # Errors
    ///
    /// A refused key, a limit, a transfer failure, or the stop.
    pub async fn run(self) -> Result<ClonedRepository, EngineError> {
        let source = self.target.source();
        let url = source.url();
        let most = self.caps.max_files.min(self.limits.max_file_count);
        let items = list_folder(
            self.platform,
            self.client,
            source,
            most,
            self.limits.names,
            self.cancel,
        )
        .await?;
        let objects = collect_objects(source, &items)?;
        drop(items);
        check_caps(&objects, source, self.caps, self.limits.names)?;
        let listed = admit_listing(&objects, self.limits, &url)?;
        let digest = listing_digest(&objects);

        std::fs::create_dir_all(self.job_scratch).map_err(|e| {
            runtime_error(format!("Cannot create {}: {e}", self.job_scratch.display()))
        })?;
        let destination =
            self.job_scratch
                .join(directory_name(source, self.target.branch(), &digest));
        if destination.symlink_metadata().is_ok() {
            std::fs::remove_dir_all(&destination).map_err(|e| {
                runtime_error(format!("Cannot clear {}: {e}", destination.display()))
            })?;
        }
        std::fs::create_dir(&destination)
            .map_err(|e| runtime_error(format!("Cannot create {}: {e}", destination.display())))?;
        let mut partial = PartialDirectory(Some(destination.clone()));
        let canonical_root = std::fs::canonicalize(&destination)
            .map_err(|e| runtime_error(format!("Cannot read {}: {e}", destination.display())))?;

        let mut received = Received {
            limits: self.limits,
            caps: self.caps,
            repo: &url,
            url: url.clone(),
            total: 0,
        };
        let mut actual = TreeBudget::new(self.limits, &url, 0);
        for object in &objects {
            if self.cancel.load(Ordering::Acquire) {
                return Err(EngineError::cancelled());
            }
            let path = prepare_path(&destination, &canonical_root, &object.relative_path)?;
            let size = download(
                self.platform,
                self.client,
                &source.bucket,
                object,
                &path,
                &mut received,
                self.cancel,
            )
            .await?;
            // The limits again, over what was received.
            actual.add_blob(&object.relative_path, size, false)?;
        }
        let tree = actual.finish();
        partial.0 = None;
        tracing::info!(
            source = %url,
            objects = tree.files,
            bytes = tree.bytes,
            listed_bytes = listed.bytes,
            identity = digest.get(..8).unwrap_or(&digest),
            "materialised"
        );
        Ok(ClonedRepository {
            path: destination,
            identity: RepoIdentity::new(self.target.repository(), self.target.branch(), digest),
            tree,
        })
    }
}

/// The ingest limits over the listed sizes, before anything is downloaded.
fn admit_listing(
    objects: &[ArtifactObject],
    limits: &IngestLimits,
    url: &str,
) -> Result<TreeStats, EngineError> {
    let mut budget = TreeBudget::new(limits, url, 0);
    for object in objects {
        budget.add_blob(&object.relative_path, object.byte_size(), false)?;
    }
    Ok(budget.finish())
}

/// The error for a folder that did not download within the ingest deadline.
#[must_use]
pub fn timeout_error(
    target: &ArtifactTarget,
    limit: Duration,
    names: &SettingNames,
) -> EngineError {
    runtime_error(format!(
        "Artifact folder timeout: {} (branch {}) did not download within {} seconds ({})",
        py_repr(&target.source().url()),
        py_repr(target.branch()),
        limit.as_secs_f64(),
        names.clone_timeout_seconds
    ))
}

#[cfg(test)]
mod tests;
