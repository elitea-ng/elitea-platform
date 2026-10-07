//! The result `wiki_subprocess_worker.py` composes around the export
//! (its lines ~283–509), and the hybrid wrapper's result before it.
//!
//! * `wiki_id` = `registry_manager.normalize_wiki_id(repo_identifier)`:
//!   `owner--repo--branch`, lower-case, every character outside
//!   `[a-z0-9-]` a dash, runs collapsed, per path segment;
//! * `analysis_key` = `{repo_identifier}@{wiki_version_id}`,
//!   `wiki_version_id` = `%Y%m%dT%H%M%SZ-{uuid4().hex[:8]}` (UTC);
//! * the two `RepositoryAnalysisStore` records (legacy key and
//!   versioned key), returned for the caller to store;
//! * the page and structure artifacts rebased under `{wiki_id}/`, the
//!   manifest `{wiki_id}/wiki_manifest_{wiki_version_id}.json`
//!   (`json.dumps(…, indent=2, ensure_ascii=False)`), and the top-level
//!   fields.
//!
//! DELIBERATE DIFFERENCE (ADR-0026): the manifest carries no
//! `faiss_cache_key`, `graph_cache_key`, `docstore_cache_key` /
//! `docstore_files`, `bm25_cache_key` / `bm25_files`, `unified_db_key` /
//! `unified_db_files`. They named per-wiki files (FAISS, pickled graph,
//! docstore, BM25 SQLite, `.wiki.db`) that no longer exist; the index is
//! PostgreSQL rows. `analysis_cache_key` stays: it names the analysis
//! record, which still exists.

use super::export::Artifact;
use crate::graph::topology::digest::md5;
use crate::pyjson;
use serde_json::{Map, Value, json};
use std::fmt::Write as _;
use std::hash::{BuildHasher, Hasher};
use std::time::{SystemTime, UNIX_EPOCH};

/// `hashlib.md5(text.encode()).hexdigest()`.
fn md5_hex(data: &[u8]) -> String {
    md5(data)
        .iter()
        .fold(String::with_capacity(32), |mut out, byte| {
            let _ = write!(out, "{byte:02x}");
            out
        })
}

/// `registry_manager.normalize_wiki_id`.
#[must_use]
pub fn normalize_wiki_id(repo_identifier: &str) -> String {
    // rsplit(":", 2): owner/repo[:branch[:commit]]
    let mut parts: Vec<&str> = repo_identifier.rsplitn(3, ':').collect();
    parts.reverse();
    let repo_part = parts.first().copied().unwrap_or("");
    let branch = parts.get(1).copied().unwrap_or("main");
    let normalize = |text: &str| -> String {
        let mut out = String::new();
        for c in text.chars() {
            let c = if c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-' {
                c
            } else {
                '-'
            };
            if c == '-' && out.ends_with('-') {
                continue;
            }
            out.push(c);
        }
        out.trim_matches('-').to_owned()
    };
    let repo_lower = repo_part.to_lowercase();
    let segments: Vec<String> = repo_lower
        .trim_matches('/')
        .split('/')
        .filter(|s| !s.is_empty())
        .map(normalize)
        .filter(|s| !s.is_empty())
        .collect();
    format!(
        "{}--{}",
        segments.join("--"),
        normalize(&branch.to_lowercase())
    )
}

/// `repository_identity.rebase_artifact_name`.
#[must_use]
pub fn rebase_artifact_name(name: &str, wiki_id: &str, subfolder: &str) -> String {
    let name = name.trim_start_matches('/');
    if name.starts_with(&format!("{wiki_id}/")) {
        return name.to_owned();
    }
    let prefix = format!("{subfolder}/");
    let marker = format!("/{subfolder}/");
    let suffix = if let Some(rest) = name.strip_prefix(&prefix) {
        rest
    } else if let Some((_, rest)) = name.split_once(&marker) {
        rest
    } else {
        name
    };
    format!("{wiki_id}/{subfolder}/{}", suffix.trim_start_matches('/'))
}

/// `repository_identity.build_repo_identifier`.
#[must_use]
pub fn build_repo_identifier(repo_path: &str, branch: &str, commit_hash: Option<&str>) -> String {
    let stripped = crate::graph::pystr::strip(branch);
    let branch = if stripped.is_empty() {
        "main"
    } else {
        stripped
    };
    match commit_hash.filter(|c| !c.is_empty()) {
        Some(commit) => format!(
            "{repo_path}:{branch}:{}",
            crate::graph::pystr::prefix_chars(commit, 8)
        ),
        None => format!("{repo_path}:{branch}"),
    }
}

/// A point in time, split as the worker formats it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Instant {
    pub year: i64,
    pub month: u32,
    pub day: u32,
    pub hour: u32,
    pub minute: u32,
    pub second: u32,
    pub micros: u32,
}

impl Instant {
    /// The UTC fields of `time`.
    #[must_use]
    pub fn utc(time: SystemTime) -> Self {
        let since = time.duration_since(UNIX_EPOCH).unwrap_or_default();
        let secs = i64::try_from(since.as_secs()).unwrap_or(0);
        let days = secs.div_euclid(86_400);
        let rem = secs.rem_euclid(86_400);
        // Howard Hinnant's civil_from_days.
        let z = days + 719_468;
        let era = z.div_euclid(146_097);
        let doe = z.rem_euclid(146_097);
        let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
        let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
        let mp = (5 * doy + 2) / 153;
        let day = doy - (153 * mp + 2) / 5 + 1;
        let month = if mp < 10 { mp + 3 } else { mp - 9 };
        let year = yoe + era * 400 + i64::from(month <= 2);
        let field = |v: i64| u32::try_from(v).unwrap_or(0);
        Self {
            year,
            month: field(month),
            day: field(day),
            hour: field(rem / 3600),
            minute: field(rem % 3600 / 60),
            second: field(rem % 60),
            micros: since.subsec_micros(),
        }
    }

    /// `%Y%m%dT%H%M%SZ`.
    #[must_use]
    pub fn version_stamp(&self) -> String {
        format!(
            "{:04}{:02}{:02}T{:02}{:02}{:02}Z",
            self.year, self.month, self.day, self.hour, self.minute, self.second
        )
    }

    /// `%Y%m%d_%H%M%S` (the structure artifact).
    #[must_use]
    pub fn file_stamp(&self) -> String {
        format!(
            "{:04}{:02}{:02}_{:02}{:02}{:02}",
            self.year, self.month, self.day, self.hour, self.minute, self.second
        )
    }

    /// `datetime.isoformat()` without a zone: the fraction only when
    /// non-zero, as Python prints it.
    #[must_use]
    pub fn iso(&self) -> String {
        let mut out = format!(
            "{:04}-{:02}-{:02}T{:02}:{:02}:{:02}",
            self.year, self.month, self.day, self.hour, self.minute, self.second
        );
        if self.micros != 0 {
            let _ = write!(out, ".{:06}", self.micros);
        }
        out
    }
}

/// The clock of one composition: the instant and the 8 hex characters of
/// the version id (injected so a test is reproducible).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VersionClock {
    pub now: Instant,
    pub uuid8: String,
}

impl VersionClock {
    /// Now, with 8 random hex characters.
    #[must_use]
    pub fn system() -> Self {
        let now = SystemTime::now();
        // Uniqueness, not secrecy: the std hasher's per-process random keys
        // over the clock.
        let mut hasher = std::collections::hash_map::RandomState::new().build_hasher();
        hasher.write_u128(
            now.duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_nanos(),
        );
        let uuid8 = format!("{:08x}", hasher.finish() & 0xffff_ffff);
        Self {
            now: Instant::utc(now),
            uuid8,
        }
    }

    #[must_use]
    pub fn wiki_version_id(&self) -> String {
        format!("{}-{}", self.now.version_stamp(), self.uuid8)
    }
}

/// A failed page of the result.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FailedPage {
    pub page_id: String,
    pub title: String,
    pub status: String,
}

/// One `RepositoryAnalysisStore.save_analysis` record.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AnalysisRecord {
    /// `{md5(analysis_key)}_analysis.json`.
    pub file_name: String,
    /// `json.dump(data, indent=2, ensure_ascii=False)`.
    pub data: String,
}

/// What the composition starts from: the hybrid wrapper's result and the
/// worker's inputs.
#[derive(Debug, Clone, PartialEq)]
pub struct ComposeInput {
    /// `repo_config["repository"]` (the toolkit's `owner/repo`).
    pub repository: String,
    /// `canonical_repository_path`: the provider's `repo_identifier`, else
    /// the repository stripped of spaces and slashes.
    pub canonical_repository: String,
    /// The branch the clone actually checked out.
    pub branch: String,
    pub commit_hash: Option<String>,
    pub provider_type: String,
    pub indexing_method: String,
    pub repository_context: String,
    pub artifacts: Vec<Artifact>,
    pub errors: Vec<String>,
    pub failed_pages: Vec<FailedPage>,
    /// The wrapper's `result` message.
    pub message: String,
    pub execution_time: f64,
}

/// The composed result.
#[derive(Debug, Clone, PartialEq)]
pub struct Composed {
    /// The worker's output object, key for key.
    pub result: Map<String, Value>,
    pub wiki_id: String,
    pub analysis_records: Vec<AnalysisRecord>,
}

/// The hybrid wrapper's `result` message (its f-string, whitespace kept).
#[must_use]
pub fn wrapper_message(
    repository: &str,
    query: &str,
    total_pages: usize,
    total_sections: usize,
    execution_time: f64,
    warnings: bool,
) -> String {
    let wiki_title = format!(
        "{} Wiki",
        repository.rsplit('/').next().unwrap_or(repository)
    );
    let pad = "            ";
    let mut message = format!(
        "\n{pad}**🚀 Wiki Generation Complete!**\n\n{pad}Repository: {repository}\n{pad}Title: {wiki_title}\n{pad}Query: {query}\n\n{pad}**📊 Processing Results:**\n{pad}- Status: ✅ Complete\n\n{pad}**🎯 Generated Content:**\n{pad}- Wiki Pages: {total_pages}\n{pad}- Sections: {total_sections}\n\n{pad}**📈 Processing Statistics:**\n{pad}- Execution Time: {execution_time:.2}s\n\n{pad}The comprehensive wiki has been successfully generated.\n{pad}"
    );
    if warnings {
        message.push_str("\n\n⚠️ Note: Some steps reported warnings/errors during generation.");
    }
    message
}

fn analysis_record(
    repo_identifier: &str,
    analysis_key: &str,
    commit_hash: Option<&str>,
    analysis: &str,
    metadata: &Value,
    stored_at: &str,
) -> AnalysisRecord {
    let data = json!({
        "repo_identifier": repo_identifier,
        "commit_hash": commit_hash,
        "analysis_key": analysis_key,
        "analysis": analysis,
        "stored_at": stored_at,
        "metadata": metadata,
    });
    AnalysisRecord {
        file_name: format!("{}_analysis.json", md5_hex(analysis_key.as_bytes())),
        data: pyjson::dumps_with(&data, Some(2), false),
    }
}

/// `executive_summary core_purpose` of a JSON repository context.
fn description_of(repo_context: &str) -> String {
    if !crate::graph::pystr::strip(repo_context).starts_with('{') {
        return String::new();
    }
    let Ok(Value::Object(context)) = serde_json::from_str::<Value>(repo_context) else {
        return String::new();
    };
    let text = |key: &str| {
        context
            .get(key)
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_owned()
    };
    let (summary, purpose) = (text("executive_summary"), text("core_purpose"));
    if summary.is_empty() && purpose.is_empty() {
        return String::new();
    }
    crate::graph::pystr::strip(&format!("{summary} {purpose}")).to_owned()
}

/// Compose the worker result.
#[must_use]
#[allow(clippy::too_many_lines)]
pub fn compose(input: ComposeInput, clock: &VersionClock) -> Composed {
    let commit = input.commit_hash.as_deref();
    let repo_identifier = build_repo_identifier(&input.canonical_repository, &input.branch, commit);
    let wiki_version_id = clock.wiki_version_id();
    let analysis_key = format!("{repo_identifier}@{wiki_version_id}");
    let stored_at = clock.now.iso();

    let mut analysis_records = Vec::new();
    if !input.repository_context.is_empty() {
        analysis_records.push(analysis_record(
            &repo_identifier,
            &format!(
                "{repo_identifier}:{}",
                commit.filter(|c| !c.is_empty()).unwrap_or("HEAD")
            ),
            commit,
            &input.repository_context,
            &json!({
                "branch": input.branch,
                "indexing_method": input.indexing_method,
                "commit_hash": commit,
                "provider_type": input.provider_type,
            }),
            &stored_at,
        ));
        analysis_records.push(analysis_record(
            &repo_identifier,
            &analysis_key,
            commit,
            &input.repository_context,
            &json!({
                "branch": input.branch,
                "indexing_method": input.indexing_method,
                "commit_hash": commit,
                "wiki_version_id": wiki_version_id,
            }),
            &stored_at,
        ));
    }

    let wiki_id = normalize_wiki_id(&repo_identifier);
    let mut artifacts = input.artifacts;
    let mut pages = Vec::new();
    for artifact in &mut artifacts {
        // `name.endswith(".md")`: case-sensitive, as in Python.
        #[allow(clippy::case_sensitive_file_extension_comparisons)]
        let markdown = artifact.mime == "text/markdown" && artifact.name.ends_with(".md");
        if markdown {
            artifact.name = rebase_artifact_name(&artifact.name, &wiki_id, "wiki_pages");
            pages.push(artifact.name.clone());
        }
    }
    let wiki_title = artifacts
        .iter()
        .find(|a| a.mime == "application/json" && a.name.contains("wiki_structure"))
        .and_then(|a| serde_json::from_str::<Value>(&a.data).ok())
        .and_then(|v| {
            v.get("wiki_title")
                .and_then(Value::as_str)
                .map(str::to_owned)
        })
        .unwrap_or_default();
    let wiki_description = description_of(&input.repository_context);
    for artifact in &mut artifacts {
        if artifact.mime == "application/json" && artifact.name.contains("wiki_structure") {
            artifact.name = rebase_artifact_name(&artifact.name, &wiki_id, "analysis");
        }
    }

    let mut manifest = Map::new();
    manifest.insert("schema_version".into(), json!(2));
    manifest.insert("wiki_id".into(), json!(wiki_id));
    manifest.insert("wiki_title".into(), json!(wiki_title));
    manifest.insert("description".into(), json!(wiki_description));
    manifest.insert("wiki_version_id".into(), json!(wiki_version_id));
    manifest.insert(
        "created_at".into(),
        json!(format!("{}+00:00", clock.now.iso())),
    );
    manifest.insert("canonical_repo_identifier".into(), json!(repo_identifier));
    manifest.insert("repository".into(), json!(input.canonical_repository));
    manifest.insert("branch".into(), json!(input.branch));
    manifest.insert("commit_hash".into(), json!(commit));
    manifest.insert("analysis_key".into(), json!(analysis_key));
    manifest.insert("pages".into(), json!(pages));
    manifest.insert("provider_type".into(), json!(input.provider_type));
    manifest.insert(
        "analysis_cache_key".into(),
        json!(md5_hex(analysis_key.as_bytes())),
    );
    artifacts.push(Artifact {
        name: format!("{wiki_id}/wiki_manifest_{wiki_version_id}.json"),
        object_type: Some("wiki_manifest".to_owned()),
        mime: "application/json".to_owned(),
        data: pyjson::dumps_with(&Value::Object(manifest), Some(2), false),
    });

    let failed: Vec<Value> = input
        .failed_pages
        .iter()
        .map(|p| json!({"page_id": p.page_id, "title": p.title, "status": p.status}))
        .collect();
    let mut result = Map::new();
    result.insert("success".into(), json!(true));
    result.insert("result".into(), json!(input.message));
    result.insert(
        "artifacts".into(),
        Value::Array(artifacts.iter().map(Artifact::to_json).collect()),
    );
    result.insert("execution_time".into(), json!(input.execution_time));
    result.insert("errors".into(), json!(input.errors));
    result.insert("failed_pages".into(), Value::Array(failed));
    result.insert("repository_context".into(), json!(input.repository_context));
    result.insert("commit_hash".into(), json!(commit));
    result.insert("branch".into(), json!(input.branch));
    result.insert("provider_type".into(), json!(input.provider_type));
    result.insert("wiki_version_id".into(), json!(wiki_version_id));
    result.insert("wiki_id".into(), json!(wiki_id));
    result.insert("analysis_key".into(), json!(analysis_key));
    result.insert("canonical_repo_identifier".into(), json!(repo_identifier));
    result.insert("wiki_title".into(), json!(wiki_title));
    result.insert("wiki_description".into(), json!(wiki_description));
    let _ = &input.repository;
    Composed {
        result,
        wiki_id,
        analysis_records,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn wiki_ids_follow_the_registry_rule() {
        assert_eq!(
            normalize_wiki_id("owner/repo:main:abc123"),
            "owner--repo--main"
        );
        assert_eq!(
            normalize_wiki_id("org/project/repo:main:abc123"),
            "org--project--repo--main"
        );
        assert_eq!(
            normalize_wiki_id("Owner/Repo:feature/test:abc123"),
            "owner--repo--feature-test"
        );
        assert_eq!(normalize_wiki_id("owner/repo"), "owner--repo--main");
        assert_eq!(
            normalize_wiki_id("Owner/sdk_plugin:main"),
            "owner--sdk-plugin--main"
        );
    }

    #[test]
    fn rebase_keeps_or_moves_names() {
        assert_eq!(
            rebase_artifact_name("w/wiki_pages/a.md", "w", "wiki_pages"),
            "w/wiki_pages/a.md"
        );
        assert_eq!(
            rebase_artifact_name("wiki_pages/s/a.md", "w", "wiki_pages"),
            "w/wiki_pages/s/a.md"
        );
        assert_eq!(
            rebase_artifact_name("old/wiki_pages/a.md", "w", "wiki_pages"),
            "w/wiki_pages/a.md"
        );
        assert_eq!(
            rebase_artifact_name("a.md", "w", "wiki_pages"),
            "w/wiki_pages/a.md"
        );
    }

    #[test]
    fn timestamps_format_like_python() {
        // 2026-01-01T00:00:00Z
        let at = Instant::utc(UNIX_EPOCH + std::time::Duration::from_hours(490_896));
        assert_eq!(at.version_stamp(), "20260101T000000Z");
        assert_eq!(at.file_stamp(), "20260101_000000");
        assert_eq!(at.iso(), "2026-01-01T00:00:00");
        let at = Instant::utc(UNIX_EPOCH + std::time::Duration::from_micros(1_767_225_600_000_250));
        assert_eq!(at.iso(), "2026-01-01T00:00:00.000250");
    }
}
