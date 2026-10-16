//! The six SDK `artifact` tools beyond list/read/create/delete
//! (`read_multiple_files`, `grep_file`, `edit_file`, `append_data`,
//! `get_file_metadata`, `create_new_bucket`), with the SDK's names, arguments
//! and answer texts (`elitea_sdk/runtime/tools/artifact.py` and
//! `runtime/clients/artifact.py` of the pinned revision).
//!
//! Reads and writes go through the same claim-bound text routes the first four
//! tools use, so the bucket access list is applied to the person the turn runs
//! as and a file moves as text (read cap 200000 characters, write cap 65536).
//! `create_new_bucket` has no such route: it goes through the Office host's
//! platform object API client (`OfficeToolHost::create_bucket`), under the
//! turn's own credential. A host without one (the desktop) answers that the
//! runtime cannot create buckets.
//!
//! Deliberate differences from the SDK, each stated where it applies:
//!
//! * `append_data` creates a missing file only when the platform says the
//!   file does not exist. The SDK creates it on ANY download error, so a
//!   refused bucket or an outage would overwrite the file with the new data.
//! * `edit_file` and `append_data` write the whole file back. The platform's
//!   text routes have no version or conditional write, so each reads the file
//!   again just before writing and refuses if it changed; the race between
//!   that read and the write remains.
//! * A write that would not fit the platform's write cap is refused before
//!   anything is read back or written, with the real size, instead of failing
//!   at the platform.
//! * `grep_file` stops at 2000 matches or 200000 output characters and says
//!   how many matches it did not show; the SDK returns every match.
//! * `get_file_metadata` has no per-type structure (sheet names, embedded
//!   images, page counts): it reports size, type and, for text, the line
//!   count, and says so in `notes`.

use std::sync::Arc;

use adk_core::{Tool, ToolContext};
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::host::HostErrorCode;
use crate::platform::{ArtifactListRequest, ArtifactReadRequest, ArtifactWriteRequest};
use crate::toolkits::families::gitlab_org::edit::{EditErrorCode, apply_pairs, editable};
use crate::toolkits::families::integer_like::unsigned_like;
use crate::toolkits::families::office_host::OfficeFailure;
use crate::toolkits::families::vcs_text::{
    MAX_BATCH_FILES, MAX_CONTEXT_LINES, MAX_OUTPUT_CHARS, MAX_PATTERN_BYTES, batch_skip_notice,
    compile_pattern, is_python_line_break, measure_result_chars, python_line_ranges,
};

use super::ArtifactToolAuthority;
use super::tools::{
    MAX_CONTENT_CHARS, MAX_FILENAME_BYTES, artifact_effect_failure_text, artifact_failure_text,
    failure_cause, line_range, normalized_key, optional_bool, optional_line, optional_string,
    over_limit_value, parse_filepath, string_argument, target_bucket, tool_description,
};

const READ_MULTIPLE_FILES: &str = "read_multiple_files";
const GREP_FILE: &str = "grep_file";
const EDIT_FILE: &str = "edit_file";
const APPEND_DATA: &str = "append_data";
const GET_FILE_METADATA: &str = "get_file_metadata";
const CREATE_NEW_BUCKET: &str = "create_new_bucket";

/// The SDK's `DEFAULT_MAX_OUTPUT_CHARS`, which the platform read also applies.
const MAX_GREP_MATCHES: usize = 2_000;
/// Extensions the SDK appends to through a binary handler (`.docx`) or cannot
/// append to at all; this runtime writes text, so it never appends text to them.
const NOT_TEXT_EXTENSIONS: [&str; 5] = [".docx", ".xlsx", ".pptx", ".pdf", ".doc"];

#[derive(Clone, Copy)]
enum Kind {
    ReadMultipleFiles,
    GrepFile,
    EditFile,
    AppendData,
    GetFileMetadata,
    CreateNewBucket,
}

impl Kind {
    const ALL: [Self; 6] = [
        Self::ReadMultipleFiles,
        Self::GrepFile,
        Self::EditFile,
        Self::AppendData,
        Self::GetFileMetadata,
        Self::CreateNewBucket,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::ReadMultipleFiles => READ_MULTIPLE_FILES,
            Self::GrepFile => GREP_FILE,
            Self::EditFile => EDIT_FILE,
            Self::AppendData => APPEND_DATA,
            Self::GetFileMetadata => GET_FILE_METADATA,
            Self::CreateNewBucket => CREATE_NEW_BUCKET,
        }
    }

    const fn read_only(self) -> bool {
        matches!(
            self,
            Self::ReadMultipleFiles | Self::GrepFile | Self::GetFileMetadata
        )
    }

    const fn sentence(self) -> &'static str {
        match self {
            Self::ReadMultipleFiles => {
                "Read up to 32 text files from the artifact bucket in one call, each optionally limited to limit lines from offset. Returns a map of path to content; reading stops at a cumulative 200000 characters and later files are reported as skipped."
            }
            Self::GrepFile => {
                "Search for text or a regular expression INSIDE one artifact file's content (like grep), case-insensitively, with context_lines of context. Not for finding files by name; use list_files for that."
            }
            Self::EditFile => {
                "Partially edit specific sections of an existing text file using OLD/NEW marker pairs: replace, insert or delete targeted text while keeping everything else. NOT for overwriting the whole file; use create_file for that. Edits that do not match, or match more than once, are reported and skipped. This is a remote write."
            }
            Self::AppendData => {
                "Append content to the END of an existing file, preserving all existing content. Use this when the user wants to add, append, or extend a file, NOT replace it. A missing file is created when create_if_missing is true. This is a remote write."
            }
            Self::GetFileMetadata => {
                "Inspect a file before reading it. Returns file type, size and, for text files, the line count, with the line-range options read_file accepts. Call this when the user asks about a file's structure or properties, or before reading a large file."
            }
            Self::CreateNewBucket => {
                "Create a new bucket. ONLY use when the user explicitly requests bucket creation (for example 'create bucket named xyz'). For file operations (save, create, append), use the toolkit's configured bucket or buckets from chat history. Do not infer bucket creation from filenames. This is a remote write."
            }
        }
    }
}

/// Every tool name this module serves.
pub(super) fn tool_names() -> impl Iterator<Item = &'static str> {
    Kind::ALL.into_iter().map(Kind::name)
}

/// The tools of this module that `include` selects.
pub(super) fn build(
    include: &dyn Fn(&str) -> bool,
    toolkit_name: &str,
    authority: &ArtifactToolAuthority,
    bucket: &Arc<str>,
) -> Vec<Arc<dyn Tool>> {
    Kind::ALL
        .into_iter()
        .filter(|kind| include(kind.name()))
        .map(|kind| {
            Arc::new(ExtraTool {
                kind,
                authority: authority.clone(),
                bucket: Arc::clone(bucket),
                description: tool_description(toolkit_name, kind.sentence()),
            }) as Arc<dyn Tool>
        })
        .collect()
}

struct ExtraTool {
    kind: Kind,
    authority: ArtifactToolAuthority,
    bucket: Arc<str>,
    description: Box<str>,
}

#[async_trait]
impl Tool for ExtraTool {
    fn name(&self) -> &str {
        self.kind.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        self.kind.read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        self.kind.read_only()
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(schema(self.kind))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        // Every failure is a tool RESULT the model can act on, as in the first
        // four tools; nothing here aborts the turn.
        Ok(match self.kind {
            Kind::ReadMultipleFiles => self.read_multiple_files(&arguments).await,
            Kind::GrepFile => self.grep_file(&arguments).await,
            Kind::EditFile => self.edit_file(&arguments).await,
            Kind::AppendData => self.append_data(&arguments).await,
            Kind::GetFileMetadata => self.get_file_metadata(&arguments).await,
            Kind::CreateNewBucket => self.create_new_bucket(&arguments).await,
        })
    }
}

const BUCKET_NAME_DESCRIPTION: &str = "Name of the bucket to work with. Leave null/omit to use the toolkit's configured default bucket. Only specify a bucket name when the user explicitly names one. ***IMPORTANT*** Underscore `_` is prohibited in bucket name and should be replaced by `-`";

fn schema(kind: Kind) -> Value {
    let bucket = json!({"type": ["string", "null"], "maxLength": 63, "description": BUCKET_NAME_DESCRIPTION});
    match kind {
        Kind::ReadMultipleFiles => json!({
            "type": "object",
            "properties": {
                "file_paths": {"type": "array", "minItems": 1, "maxItems": MAX_BATCH_FILES, "items": {"type": "string", "minLength": 1, "maxLength": MAX_FILENAME_BYTES}, "description": "List of file paths to read. Do NOT include bucket name - use 'folder/file.txt' or 'file.txt', NOT 'bucket-name/folder/file.txt'. A path that starts with / is read as /{bucket}/{filename}."},
                "bucket_name": bucket,
                "offset": {"type": ["integer", "null"], "minimum": 1, "description": "Starting line number for all files (1-indexed)"},
                "limit": {"type": ["integer", "null"], "minimum": 1, "description": "Number of lines to read from offset for all files"}
            },
            "required": ["file_paths"],
            "additionalProperties": false
        }),
        Kind::GrepFile => json!({
            "type": "object",
            "properties": {
                "file_path": {"type": "string", "minLength": 1, "maxLength": MAX_FILENAME_BYTES, "description": "Path to the file to search. Do NOT include bucket name - use 'folder/file.txt' or 'file.txt', NOT 'bucket-name/folder/file.txt'"},
                "pattern": {"type": "string", "minLength": 1, "maxLength": MAX_PATTERN_BYTES, "description": "Search pattern. Treated as regex by default unless is_regex=False."},
                "bucket_name": bucket,
                "is_regex": {"type": "boolean", "default": true, "description": "Whether pattern is a regex. Default is True for flexible matching."},
                "context_lines": {"type": "integer", "minimum": 0, "maximum": MAX_CONTEXT_LINES, "default": 2, "description": "Number of lines before/after match to include for context"}
            },
            "required": ["file_path", "pattern"],
            "additionalProperties": false
        }),
        Kind::EditFile => json!({
            "type": "object",
            "properties": {
                "file_path": {"type": "string", "minLength": 1, "maxLength": MAX_FILENAME_BYTES, "description": "Path to the file to edit. Must be a text file (markdown, txt, csv, json, xml, html, yaml, etc.). Do NOT include bucket name - use 'folder/file.txt' or 'file.txt', NOT 'bucket-name/folder/file.txt'"},
                "file_query": {"type": "string", "multiline": true, "description": "Edit instructions using OLD/NEW marker pairs. Each pair must have an OLD block (opening line: 'OLD <<<<', closing line: '>>>> OLD') containing the exact text to replace, and a NEW block (opening line: 'NEW <<<<', closing line: '>>>> NEW') containing the replacement text. Markers must be on their own dedicated lines. Multiple OLD/NEW pairs are supported for multiple edits in a single call."},
                "bucket_name": bucket,
                "commit_message": {"type": ["string", "null"], "description": "Not used for artifacts (kept for API consistency)"}
            },
            "required": ["file_path", "file_query"],
            "additionalProperties": false
        }),
        Kind::AppendData => json!({
            "type": "object",
            "properties": {
                "filename": {"type": "string", "minLength": 1, "maxLength": MAX_FILENAME_BYTES, "description": "Filename of the file to append to. Do NOT include bucket name - use 'folder/file.txt' or 'file.txt', NOT 'bucket-name/folder/file.txt'"},
                "filedata": {"type": "string", "maxLength": MAX_CONTENT_CHARS, "description": "Stringified content to append. The file may not grow past 65536 characters in total."},
                "bucket_name": bucket,
                "create_if_missing": {"type": ["boolean", "null"], "default": true, "description": "If True (default), creates the file if it doesn't exist before appending. If False, returns an error when file is not found."}
            },
            "required": ["filename", "filedata"],
            "additionalProperties": false
        }),
        Kind::GetFileMetadata => json!({
            "type": "object",
            "properties": {
                "filepath": {"type": ["string", "null"], "maxLength": MAX_FILENAME_BYTES, "description": "Full path in /{bucket}/{filename} format. This filepath is returned when files are uploaded or generated."},
                "filename": {"type": ["string", "null"], "maxLength": MAX_FILENAME_BYTES, "description": "Filename of the file. Alternative to filepath when used together with bucket_name."},
                "bucket_name": bucket
            },
            "additionalProperties": false
        }),
        Kind::CreateNewBucket => json!({
            "type": "object",
            "properties": {
                "bucket_name": {"type": "string", "pattern": "^[a-z][a-z0-9_-]*$", "maxLength": 64, "description": "Bucket name to create. Must start with lowercase letter and contain only lowercase letters, numbers, and hyphens. Underscores will be automatically converted to hyphens."},
                "expiration_measure": {"type": ["string", "null"], "default": "weeks", "description": "Measure of expiration time for bucket configuration. Possible values: `days`, `weeks`, `months`, `years`."},
                "expiration_value": {"type": ["integer", "null"], "default": 1, "description": "Expiration time values."}
            },
            "required": ["bucket_name"],
            "additionalProperties": false
        }),
    }
}

fn sentence(text: impl Into<String>) -> Value {
    Value::String(text.into())
}

fn log_failure(tool: &str, error: &crate::host::HostError) {
    tracing::warn!(
        event = "agent_artifact_tool_failed",
        tool,
        reason_code = error.reason_code(),
        "an artifact tool's platform call failed"
    );
}

/// What a listing says about a file, to tell whether it changed.
#[derive(Debug, PartialEq, Eq)]
enum Marker {
    Absent,
    Present(u64, String),
    /// Listed without a modified time: not comparable.
    Unknown,
}

/// One whole text file, or the sentence that says why not.
enum Read {
    Text(crate::platform::ArtifactReadOutcome),
    Missing,
    Failed(String),
}

impl ExtraTool {
    /// The file's cheap change marker: its size and modified time from a
    /// listing of that one key (no file content). `Unknown` when the platform
    /// gives no modified time, which sends the guard to a content comparison.
    async fn marker(
        &self,
        tool: &str,
        action: &str,
        bucket: &str,
        key: &str,
    ) -> Result<Marker, String> {
        let request = ArtifactListRequest {
            bucket: bucket.to_owned(),
            prefix: key.to_owned(),
            recursive: true,
            limit: 50,
            bucket_only: false,
        };
        match self.authority.writer().list_artifacts(&request).await {
            Ok(listed) => Ok(listed.files.iter().find(|file| file.name == key).map_or(
                Marker::Absent,
                |file| {
                    if file.modified_at.is_empty() {
                        Marker::Unknown
                    } else {
                        Marker::Present(file.byte_length, file.modified_at.clone())
                    }
                },
            )),
            Err(error) => {
                log_failure(tool, &error);
                Err(artifact_failure_text(action, &error))
            }
        }
    }

    /// The write is a whole-file replace and the platform's text routes carry
    /// no version, etag or conditional write. The guard is the file's size and
    /// modified time, listed before the file is read and again just before it
    /// is written (a small listing each, not a second full read): a change in
    /// either, or a file that appeared or vanished, refuses the write. When
    /// the platform gives no modified time the file is read again and its
    /// content compared. The race between the last check and the write
    /// remains; only a conditional write on the platform could close it. A
    /// same-size rewrite inside one timestamp tick goes unseen by the marker.
    async fn changed_since(
        &self,
        tool: &str,
        action: &str,
        bucket: &str,
        key: &str,
        before: &Marker,
        original: Option<&str>,
    ) -> Option<String> {
        let changed = || {
            format!(
                "Could not {action}: the file was changed by someone else while this change was being made, so nothing was written. Read the file again and redo the change."
            )
        };
        let now = match self.marker(tool, action, bucket, key).await {
            Ok(now) => now,
            Err(text) => return Some(text),
        };
        if *before != Marker::Unknown && now != Marker::Unknown {
            return (now != *before).then(changed);
        }
        match self.read_text(tool, action, bucket, key).await {
            Read::Text(now) if original == Some(now.content.as_str()) => None,
            Read::Missing if original.is_none() => None,
            Read::Text(_) | Read::Missing => Some(changed()),
            Read::Failed(text) => Some(text),
        }
    }

    async fn read_text(&self, tool: &str, action: &str, bucket: &str, key: &str) -> Read {
        let request = ArtifactReadRequest {
            bucket: bucket.to_owned(),
            name: key.to_owned(),
        };
        match self.authority.writer().read_artifact(&request).await {
            Ok(outcome) if outcome.over_limit => {
                let size = if outcome.char_length > 0 {
                    format!("{} characters", outcome.char_length)
                } else {
                    format!("{} bytes", outcome.byte_length)
                };
                Read::Failed(format!(
                    "Could not {action}: the file has {size}, over the {} characters this runtime reads at once. Nothing was changed. Use read_file with start_line and end_line to see parts of it.",
                    outcome.max_chars
                ))
            }
            Ok(outcome) => Read::Text(outcome),
            Err(error) if error.code() == HostErrorCode::NotFound => Read::Missing,
            Err(error) => {
                log_failure(tool, &error);
                Read::Failed(artifact_failure_text(action, &error))
            }
        }
    }

    // ----- read_multiple_files ------------------------------------------

    async fn read_multiple_files(&self, arguments: &Value) -> Value {
        let paths: Vec<&str> = arguments
            .get("file_paths")
            .and_then(Value::as_array)
            .map(|paths| paths.iter().filter_map(Value::as_str).collect())
            .unwrap_or_default();
        if paths.is_empty() {
            return sentence(
                "Could not read the files: file_paths needs at least one non-empty path.",
            );
        }
        if paths.len() > MAX_BATCH_FILES {
            return sentence(format!(
                "Could not read the files: at most {MAX_BATCH_FILES} files are read in one call, and {} were given. Split the list; nothing was read.",
                paths.len()
            ));
        }
        if paths
            .iter()
            .any(|path| path.trim().is_empty() || path.len() > MAX_FILENAME_BYTES)
        {
            return sentence(
                "Could not read the files: a path is empty or too long. Nothing was read.",
            );
        }
        let offset = optional_line(arguments, "offset");
        let limit = optional_line(arguments, "limit");
        if offset.is_some_and(|value| value < 1) || limit.is_some_and(|value| value < 1) {
            return sentence(
                "Could not read the files: offset and limit are 1-indexed line counts and must be at least 1. Nothing was read.",
            );
        }
        let start = offset;
        let end = offset
            .zip(limit)
            .map(|(offset, limit)| offset.saturating_add(limit).saturating_sub(1));
        let default_bucket = target_bucket(arguments, &self.bucket);
        let mut results = Map::new();
        let mut cumulative = 0_usize;
        for path in paths {
            if results.contains_key(path) {
                continue;
            }
            if cumulative >= MAX_OUTPUT_CHARS {
                results.insert(path.to_owned(), Value::String(batch_skip_notice()));
                continue;
            }
            match self.read_one(path, &default_bucket, start, end).await {
                Ok(value) => {
                    cumulative = cumulative.saturating_add(measure_result_chars(&value));
                    results.insert(path.to_owned(), value);
                }
                Err(cause) => {
                    results.insert(
                        path.to_owned(),
                        Value::String(format!("Error reading file: {cause}")),
                    );
                }
            }
        }
        Value::Object(results)
    }

    /// One file of the batch: its text (or slice), the `content_too_large`
    /// guidance object, or the cause.
    async fn read_one(
        &self,
        path: &str,
        default_bucket: &str,
        start: Option<i64>,
        end: Option<i64>,
    ) -> Result<Value, String> {
        let (bucket, key) = if path.starts_with('/') {
            parse_filepath(path)
                .ok_or_else(|| "filepath must be in /{bucket}/{filename} form.".to_owned())?
        } else {
            (default_bucket.to_owned(), normalized_key(path))
        };
        let request = ArtifactReadRequest { bucket, name: key };
        match self.authority.writer().read_artifact(&request).await {
            Ok(outcome) if outcome.over_limit => Ok(over_limit_value(&outcome)),
            Ok(outcome) => {
                if start.is_none() && end.is_none() {
                    return Ok(Value::String(outcome.content));
                }
                line_range(&outcome.content, start, end)
                    .map(Value::String)
                    .ok_or_else(|| {
                        format!(
                            "the line range is empty; the file has {} lines.",
                            outcome.total_lines
                        )
                    })
            }
            Err(error) => {
                log_failure(READ_MULTIPLE_FILES, &error);
                Err(failure_cause(&error, false))
            }
        }
    }

    // ----- grep_file -------------------------------------------------------

    async fn grep_file(&self, arguments: &Value) -> Value {
        let Some(file_path) = string_argument(arguments, "file_path") else {
            return sentence("Could not search the file: a non-empty file_path is required.");
        };
        let Some(pattern) = arguments
            .get("pattern")
            .and_then(Value::as_str)
            .filter(|pattern| !pattern.is_empty())
        else {
            return sentence("Could not search the file: a non-empty pattern is required.");
        };
        if pattern.len() > MAX_PATTERN_BYTES {
            return sentence(format!(
                "Could not search the file: the pattern is longer than {MAX_PATTERN_BYTES} bytes."
            ));
        }
        let is_regex = optional_bool(arguments, "is_regex").unwrap_or(true);
        let context_lines = match arguments.get("context_lines") {
            None | Some(Value::Null) => 2,
            Some(value) => match unsigned_like(value).and_then(|value| usize::try_from(value).ok())
            {
                Some(value) if value <= MAX_CONTEXT_LINES => value,
                _ => {
                    return sentence(format!(
                        "Could not search the file: context_lines is a count from 0 to {MAX_CONTEXT_LINES}."
                    ));
                }
            },
        };
        let bucket = target_bucket(arguments, &self.bucket);
        let key = normalized_key(&file_path);
        let outcome = match self
            .read_text(GREP_FILE, "search the file", &bucket, &key)
            .await
        {
            Read::Text(outcome) => outcome,
            Read::Missing => {
                return sentence(
                    "Could not search the file: no such file or bucket. Check the name with list_files.",
                );
            }
            Read::Failed(text) => return Value::String(text),
        };
        sentence(grep_text(
            &outcome.content,
            &file_path,
            pattern,
            is_regex,
            context_lines,
        ))
    }

    // ----- edit_file ---------------------------------------------------------

    async fn edit_file(&self, arguments: &Value) -> Value {
        let Some(file_path) = string_argument(arguments, "file_path") else {
            return sentence("Could not edit the file: a non-empty file_path is required.");
        };
        let Some(query) = arguments.get("file_query").and_then(Value::as_str) else {
            return sentence("Could not edit the file: file_query is required.");
        };
        if !editable(&file_path) {
            return sentence(format!(
                "Could not edit the file: File '{file_path}' is not a text-editable file type. Supported text formats: markdown, txt, csv, json, xml, html, yaml, code files."
            ));
        }
        let bucket = target_bucket(arguments, &self.bucket);
        let key = normalized_key(&file_path);
        let before = match self.marker(EDIT_FILE, "edit the file", &bucket, &key).await {
            Ok(before) => before,
            Err(text) => return Value::String(text),
        };
        let outcome = match self
            .read_text(EDIT_FILE, "edit the file", &bucket, &key)
            .await
        {
            Read::Text(outcome) => outcome,
            Read::Missing => {
                return sentence(
                    "Could not edit the file: no such file or bucket. Check the name with list_files; create_file makes a new file.",
                );
            }
            Read::Failed(text) => return Value::String(text),
        };
        let result = match apply_pairs(&file_path, &outcome.content, query) {
            Ok(result) => result,
            Err(EditErrorCode::InvalidMarkers) => {
                return sentence(
                    "Could not edit the file: No valid OLD/NEW marker pairs found in edit instructions. Markers must be on their own dedicated line.",
                );
            }
            Err(_) => {
                return sentence(
                    "Could not edit the file: the edit instructions or the file are larger than this runtime edits. Nothing was changed.",
                );
            }
        };
        if result.applied == 0 {
            let details = if result.warnings.is_empty() {
                "The OLD blocks may not match the file content.".to_owned()
            } else {
                result.warnings.join("; ")
            };
            return sentence(format!("No edits were applied to {file_path}. {details}"));
        }
        let chars = result.updated.chars().count();
        if chars > MAX_CONTENT_CHARS {
            return sentence(format!(
                "Could not edit the file: the edited file would have {chars} characters, over the {MAX_CONTENT_CHARS} this platform accepts in one write. Nothing was changed; make smaller edits."
            ));
        }
        if let Some(text) = self
            .changed_since(
                EDIT_FILE,
                "edit the file",
                &bucket,
                &key,
                &before,
                Some(&outcome.content),
            )
            .await
        {
            return Value::String(text);
        }
        let request = ArtifactWriteRequest {
            bucket,
            name: key,
            content: result.updated,
        };
        if let Err(error) = self.authority.writer().write_artifact(&request).await {
            log_failure(EDIT_FILE, &error);
            return sentence(artifact_effect_failure_text("save the edited file", &error));
        }
        let applied = format!(
            "Successfully applied {} edit(s) to {file_path}",
            result.applied
        );
        if result.warnings.is_empty() {
            return sentence(applied);
        }
        sentence(format!(
            "{applied}. WARNING: {} edit(s) were NOT applied: {}",
            result.warnings.len(),
            result.warnings.join("; ")
        ))
    }

    // ----- append_data -------------------------------------------------------

    async fn append_data(&self, arguments: &Value) -> Value {
        let Some(filename) = string_argument(arguments, "filename") else {
            return sentence("Could not append to the file: a non-empty filename is required.");
        };
        let Some(filedata) = arguments.get("filedata").and_then(Value::as_str) else {
            return sentence("Could not append to the file: filedata is required.");
        };
        let create_if_missing = optional_bool(arguments, "create_if_missing").unwrap_or(true);
        let bucket = target_bucket(arguments, &self.bucket);
        let key = normalized_key(&filename);
        let lower = key.to_ascii_lowercase();
        if NOT_TEXT_EXTENSIONS
            .iter()
            .any(|extension| lower.ends_with(extension))
        {
            return sentence(format!(
                "Cannot append to '{filename}': File type is not text-editable and this runtime has no binary append handler (it appends text only). Binary files (PDF, images, Office documents) cannot be safely appended to as text. Consider creating a new file instead using the create_file tool."
            ));
        }
        let before = match self
            .marker(APPEND_DATA, "append to the file", &bucket, &key)
            .await
        {
            Ok(before) => before,
            Err(text) => return Value::String(text),
        };
        let mut original: Option<String> = None;
        let combined = match self
            .read_text(APPEND_DATA, "append to the file", &bucket, &key)
            .await
        {
            Read::Text(outcome) => {
                if !editable(&key) {
                    return sentence(format!(
                        "Cannot append to '{filename}': File type is not text-editable and no binary append handler is available. Binary files (PDF, images, etc.) cannot be safely appended to as text. Consider creating a new file instead using the create_file tool."
                    ));
                }
                let joined = if outcome.content.is_empty() {
                    filedata.to_owned()
                } else {
                    format!("{}\n{filedata}", outcome.content)
                };
                original = Some(outcome.content);
                joined
            }
            // Only the platform's own "no such file" creates the file. Any
            // other failure (a refused bucket, an outage) leaves the file
            // alone: the SDK creates it on every download error.
            Read::Missing if create_if_missing => filedata.to_owned(),
            Read::Missing => {
                return sentence(format!(
                    "Failed to append to file '{filename}': Cannot append to file '{filename}'. No such file in bucket '{bucket}', and create_if_missing is false."
                ));
            }
            Read::Failed(text) => return Value::String(text),
        };
        let chars = combined.chars().count();
        if chars > MAX_CONTENT_CHARS {
            return sentence(format!(
                "Could not append to the file: it would have {chars} characters after the append, over the {MAX_CONTENT_CHARS} this platform accepts in one write. Nothing was changed. Put the new data in a new file instead."
            ));
        }
        if let Some(text) = self
            .changed_since(
                APPEND_DATA,
                "append to the file",
                &bucket,
                &key,
                &before,
                original.as_deref(),
            )
            .await
        {
            return Value::String(text);
        }
        let request = ArtifactWriteRequest {
            bucket,
            name: key,
            content: combined,
        };
        match self.authority.writer().write_artifact(&request).await {
            Ok(written) => sentence(python_json_object(&[
                ("filepath", format!("/{}/{}", written.bucket, written.name)),
                ("filename", written.name.clone()),
                ("bucket", written.bucket.clone()),
                ("message", "Data appended successfully".to_owned()),
            ])),
            Err(error) => {
                log_failure(APPEND_DATA, &error);
                sentence(format!(
                    "Failed to append to file '{filename}': {}",
                    artifact_effect_failure_text("save the appended file", &error)
                ))
            }
        }
    }

    // ----- get_file_metadata -------------------------------------------------

    #[allow(
        clippy::too_many_lines,
        reason = "the SDK's metadata object, field by field"
    )]
    async fn get_file_metadata(&self, arguments: &Value) -> Value {
        let filepath = optional_string(arguments, "filepath");
        let (bucket, key) = if let Some(path) = &filepath {
            match parse_filepath(path) {
                Some(target) => target,
                None => {
                    return metadata_error(
                        "Invalid filepath format: filepath must be in /{bucket}/{filename} form.",
                        None,
                        &[("filepath", path.as_str())],
                    );
                }
            }
        } else if let Some(filename) = string_argument(arguments, "filename") {
            (
                target_bucket(arguments, &self.bucket),
                normalized_key(&filename),
            )
        } else {
            return metadata_error("Must provide either 'filepath' or 'filename'.", None, &[]);
        };
        let listing = ArtifactListRequest {
            bucket: bucket.clone(),
            prefix: key.clone(),
            recursive: true,
            limit: 500,
            bucket_only: false,
        };
        let listed = match self.authority.writer().list_artifacts(&listing).await {
            Ok(listed) => listed,
            Err(error) => {
                log_failure(GET_FILE_METADATA, &error);
                return metadata_error(
                    &format!(
                        "Error reading file metadata: {}",
                        failure_cause(&error, false)
                    ),
                    Some(&key),
                    &[("bucket", bucket.as_str())],
                );
            }
        };
        let Some(file) = listed.files.iter().find(|file| file.name == key) else {
            return metadata_error(
                &format!("File '{key}' was not found in bucket '{bucket}'."),
                Some(&key),
                &[("bucket", bucket.as_str())],
            );
        };
        let extension =
            crate::toolkits::families::vcs_text::file_extension(&key).to_ascii_lowercase();
        let media_type = if file.media_type.is_empty() {
            "application/octet-stream".to_owned()
        } else {
            file.media_type.clone()
        };
        let mut meta = Map::new();
        meta.insert("__result_status__".to_owned(), json!("file_metadata"));
        meta.insert("schema_version".to_owned(), json!("1.0"));
        meta.insert("filename".to_owned(), json!(key));
        meta.insert("type".to_owned(), json!(media_type));
        meta.insert("extension".to_owned(), json!(extension));
        meta.insert("filesize".to_owned(), json!(file.byte_length));
        meta.insert("bucket".to_owned(), json!(bucket));
        if let Some(path) = &filepath {
            meta.insert("filepath".to_owned(), json!(path));
        }
        let line_oriented = editable(&key) || extension == ".svg";
        let mut counted = None;
        if line_oriented {
            let request = ArtifactReadRequest {
                bucket: bucket.clone(),
                name: key.clone(),
            };
            match self.authority.writer().read_artifact(&request).await {
                Ok(outcome) => counted = Some(outcome),
                Err(error) => {
                    log_failure(GET_FILE_METADATA, &error);
                    return metadata_error(
                        &format!(
                            "Error reading file metadata: {}",
                            failure_cause(&error, false)
                        ),
                        Some(&key),
                        &[("bucket", bucket.as_str())],
                    );
                }
            }
        }
        if let Some(outcome) = counted {
            let lines = outcome.total_lines;
            let content_size = if outcome.over_limit {
                outcome.char_length.max(outcome.max_chars.saturating_add(1))
            } else {
                outcome.char_length
            };
            meta.insert("unit".to_owned(), json!("lines"));
            meta.insert("total_lines".to_owned(), json!(lines));
            let single_line_too_big = lines <= 1 && content_size > MAX_OUTPUT_CHARS as u64;
            meta.insert(
                "read_limits".to_owned(),
                json!({
                    "max_output_chars": MAX_OUTPUT_CHARS,
                    "full_read_allowed": content_size <= MAX_OUTPUT_CHARS as u64
                }),
            );
            let instruction = if single_line_too_big {
                json!({
                    "first_class_params": {},
                    "extra_params": {},
                    "notes": format!("This file has no usable line breaks ({content_size} characters on a single line) and exceeds the {MAX_OUTPUT_CHARS}-character read limit. Line slicing would return the whole file, so a bounded read is not possible - the full read is refused.")
                })
            } else {
                let range_hint = if lines > 0 {
                    format!("Valid range 1..{lines}. ")
                } else {
                    String::new()
                };
                json!({
                    "first_class_params": {
                        "start_line": format!("integer (1-indexed, inclusive) - first line to read. {range_hint}Omit to read from the beginning."),
                        "end_line": format!("integer (1-indexed, inclusive) - last line to read. {range_hint}Omit to read to the end.")
                    },
                    "extra_params": {},
                    "notes": "Use start_line/end_line together to read a bounded slice of a large file and keep tokens bounded."
                })
            };
            meta.insert("instruction_for_readFile".to_owned(), instruction);
        } else {
            meta.insert("unit".to_owned(), Value::Null);
            meta.insert(
                "read_limits".to_owned(),
                json!({"max_output_chars": MAX_OUTPUT_CHARS, "full_read_allowed": true}),
            );
            meta.insert(
                "instruction_for_readFile".to_owned(),
                json!({
                    "first_class_params": {},
                    "extra_params": {},
                    "notes": "Per-type details (sheet names, page counts, embedded images) are not available in this runtime; read_file returns a text file's text only."
                }),
            );
        }
        sentence(serde_json::to_string(&Value::Object(meta)).unwrap_or_default())
    }

    // ----- create_new_bucket -------------------------------------------------

    async fn create_new_bucket(&self, arguments: &Value) -> Value {
        let Some(requested) = string_argument(arguments, "bucket_name") else {
            return sentence("Could not create the bucket: a non-empty bucket_name is required.");
        };
        let name = requested.replace('_', "-").to_ascii_lowercase();
        if !valid_new_bucket_name(&name) {
            return sentence(format!(
                "Could not create the bucket: '{name}' is not a valid bucket name. It must start with a lowercase letter, contain only lowercase letters, numbers and hyphens (underscores become hyphens), and be 2 to 63 characters long. Nothing was created."
            ));
        }
        let measure = optional_string(arguments, "expiration_measure")
            .unwrap_or_else(|| "weeks".to_owned())
            .to_ascii_lowercase();
        let per_unit: u64 = match measure.as_str() {
            "days" => 1,
            "weeks" => 7,
            "months" => 30,
            "years" => 365,
            other => {
                return sentence(format!(
                    "Could not create the bucket: expiration_measure '{other}' is not one of days, weeks, months, years. Nothing was created."
                ));
            }
        };
        let value = match arguments.get("expiration_value") {
            None | Some(Value::Null) => 1,
            Some(value) => match unsigned_like(value) {
                Some(value) if value >= 1 => value,
                _ => {
                    return sentence(
                        "Could not create the bucket: expiration_value must be a whole number of at least 1. Nothing was created.",
                    );
                }
            },
        };
        let days = value
            .checked_mul(per_unit)
            .and_then(|days| u32::try_from(days).ok())
            .filter(|days| i32::try_from(*days).is_ok());
        let Some(days) = days else {
            return sentence(
                "Could not create the bucket: that expiration is too long. Nothing was created.",
            );
        };
        let Some(host) = self.authority.office() else {
            return sentence(
                "Could not create the bucket: this runtime has no access to the platform's bucket API (a desktop or pipeline run without it). Nothing was created; ask the user to create the bucket in the product.",
            );
        };
        match host.create_bucket(&name, Some(days)).await {
            Ok(created) => {
                let renamed = if created.name == requested {
                    String::new()
                } else {
                    format!(
                        " (the name '{requested}' was changed to '{}')",
                        created.name
                    )
                };
                json!({
                    "name": created.name,
                    "retention_days": created.retention_days,
                    "message": format!("Bucket '{}' created{renamed}. Files in it expire after {days} day(s).", created.name)
                })
            }
            Err(error) => {
                tracing::warn!(
                    event = "agent_artifact_tool_failed",
                    tool = CREATE_NEW_BUCKET,
                    reason_code = error.reason_code(),
                    "the bucket creation failed"
                );
                sentence(match error.kind() {
                    OfficeFailure::AlreadyExists => format!(
                        "Could not create the bucket: a bucket named '{name}' already exists in this project. Use it, or choose another name."
                    ),
                    OfficeFailure::AccessRefused => "Could not create the bucket: the platform refused it. This conversation may not create buckets, or the expiry is above the project's retention ceiling. Do not retry; tell the user.".to_owned(),
                    OfficeFailure::InvalidRequest => format!(
                        "Could not create the bucket: the platform refused '{name}' as invalid. Nothing was created."
                    ),
                    OfficeFailure::Unsupported => "Could not create the bucket: this runtime does not offer bucket creation. Nothing was created.".to_owned(),
                    _ => format!(
                        "Could not create the bucket: the platform could not be reached, so it is not known whether '{name}' was created. Check with list_files on it before trying again, and tell the user."
                    ),
                })
            }
        }
    }
}

/// The platform's bucket-name rule (`^[a-z][a-z0-9-]{1,62}$`).
fn valid_new_bucket_name(name: &str) -> bool {
    let bytes = name.as_bytes();
    (2..=63).contains(&bytes.len())
        && bytes[0].is_ascii_lowercase()
        && bytes[1..]
            .iter()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || *byte == b'-')
}

/// `json.dumps` of a flat string object, with Python's separators.
fn python_json_object(pairs: &[(&str, String)]) -> String {
    let members = pairs
        .iter()
        .map(|(key, value)| {
            format!(
                "{}: {}",
                serde_json::to_string(key).unwrap_or_default(),
                serde_json::to_string(value).unwrap_or_default()
            )
        })
        .collect::<Vec<_>>();
    format!("{{{}}}", members.join(", "))
}

/// The SDK's `build_error_response` as the JSON text `get_file_metadata`
/// returns.
fn metadata_error(message: &str, filename: Option<&str>, extra: &[(&str, &str)]) -> Value {
    let mut body = Map::new();
    body.insert("__result_status__".to_owned(), json!("error"));
    body.insert("schema_version".to_owned(), json!("1.0"));
    body.insert(
        "filename".to_owned(),
        filename.map_or(Value::Null, |name| json!(name)),
    );
    body.insert("extension".to_owned(), json!(""));
    body.insert("message".to_owned(), json!(message));
    for (key, value) in extra {
        body.insert((*key).to_owned(), json!(value));
    }
    sentence(serde_json::to_string(&Value::Object(body)).unwrap_or_default())
}

/// `search_file` of the artifact wrapper: `Found N match(es) in {path}:` and a
/// block per match, a line of `>` for the match and `  ` for its context. An
/// invalid regular expression finds nothing (the SDK logs it and answers "no
/// matches"). Stops at [`MAX_GREP_MATCHES`] matches or [`MAX_OUTPUT_CHARS`]
/// characters and says how many matches it did not show.
fn grep_text(
    content: &str,
    file_path: &str,
    pattern: &str,
    is_regex: bool,
    context_lines: usize,
) -> String {
    let none = || format!("No matches found for pattern '{pattern}' in {file_path}");
    let Some(expression) = compile_pattern(pattern, is_regex) else {
        return none();
    };
    let lines = python_line_ranges(content)
        .into_iter()
        .map(|(start, end)| content[start..end].trim_end_matches(is_python_line_break))
        .collect::<Vec<_>>();
    let matched = lines
        .iter()
        .enumerate()
        .filter(|(_, line)| expression.is_match(line))
        .map(|(index, _)| index)
        .collect::<Vec<_>>();
    if matched.is_empty() {
        return none();
    }
    let mut parts = vec![format!(
        "Found {} match(es) in {file_path}:\n",
        matched.len()
    )];
    let mut size = parts[0].chars().count();
    let mut shown = 0_usize;
    for &index in &matched {
        let mut block = vec![format!("\n--- Line {} ---", index + 1)];
        let before = &lines[index.saturating_sub(context_lines)..index];
        if !before.is_empty() {
            block.push(
                before
                    .iter()
                    .map(|line| format!("  {line}"))
                    .collect::<Vec<_>>()
                    .join("\n"),
            );
        }
        block.push(format!("> {}", lines[index]));
        let after = &lines[index + 1
            ..index
                .saturating_add(context_lines)
                .saturating_add(1)
                .min(lines.len())];
        if !after.is_empty() {
            block.push(
                after
                    .iter()
                    .map(|line| format!("  {line}"))
                    .collect::<Vec<_>>()
                    .join("\n"),
            );
        }
        let block_chars: usize = block.iter().map(|part| part.chars().count() + 1).sum();
        if shown >= MAX_GREP_MATCHES || size.saturating_add(block_chars) > MAX_OUTPUT_CHARS {
            break;
        }
        size += block_chars;
        shown += 1;
        parts.extend(block);
    }
    if shown < matched.len() {
        parts.push(format!(
            "\n[{} more match(es) are not shown: the output is limited to {MAX_GREP_MATCHES} matches and {MAX_OUTPUT_CHARS} characters. Use a narrower pattern or fewer context_lines.]",
            matched.len() - shown
        ));
    }
    parts.join("\n")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn grep_text_follows_the_sdk_layout() {
        let found = grep_text("alpha\nbeta\ngamma\nBETA two", "f.txt", "beta", true, 1);
        assert_eq!(
            found,
            "Found 2 match(es) in f.txt:\n\n\n--- Line 2 ---\n  alpha\n> beta\n  gamma\n\n--- Line 4 ---\n  gamma\n> BETA two"
        );
        assert_eq!(
            grep_text("a", "f.txt", "(", true, 0),
            "No matches found for pattern '(' in f.txt"
        );
        assert_eq!(
            grep_text("a(b", "f.txt", "(", false, 0),
            "Found 1 match(es) in f.txt:\n\n\n--- Line 1 ---\n> a(b"
        );
    }

    #[test]
    fn grep_text_says_what_it_did_not_show() {
        let content = "x\n".repeat(MAX_GREP_MATCHES + 5);
        let found = grep_text(&content, "f.txt", "x", true, 0);
        assert!(found.starts_with(&format!("Found {} match(es)", MAX_GREP_MATCHES + 5)));
        assert!(found.contains("5 more match(es) are not shown"), "{found}");
    }

    #[test]
    fn bucket_names_follow_the_platform_rule() {
        assert!(valid_new_bucket_name("my-bucket-2"));
        assert!(!valid_new_bucket_name("a"));
        assert!(!valid_new_bucket_name("9abc"));
        assert!(!valid_new_bucket_name("ab_c"));
        assert!(!valid_new_bucket_name(&"a".repeat(64)));
    }

    #[test]
    fn python_json_uses_python_separators() {
        assert_eq!(
            python_json_object(&[("a", "x".to_owned()), ("b", "y\"z".to_owned())]),
            r#"{"a": "x", "b": "y\"z"}"#
        );
    }
}
