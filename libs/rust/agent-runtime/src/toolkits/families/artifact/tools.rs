use std::fmt;
use std::sync::Arc;

use adk_core::{Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Value, json};

use crate::host::{HostError, HostErrorCode};
use crate::platform::{
    ArtifactDeleteRequest, ArtifactListRequest, ArtifactReadRequest, ArtifactWriteRequest,
};
use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::ArtifactToolAuthority;
use super::config::{ArtifactConfigError, ArtifactConfigErrorCode, ArtifactToolkitConfig};

/// The SDK's own tool names, so a prompt written against the Python worker
/// ports unchanged (`elitea_sdk.runtime.tools.artifact.ArtifactWrapper`). The
/// stored `selected_tools` of every artifact toolkit created through the
/// product's form names these, and the argument names below are the SDK's for
/// the same reason — `filename`, `filedata`, `bucket_name`, `folder`.
const LIST_FILES_TOOL: &str = "list_files";
const READ_FILE_TOOL: &str = "read_file";
const CREATE_FILE_TOOL: &str = "create_file";
const DELETE_FILE_TOOL: &str = "delete_file";

const MAX_FILENAME_BYTES: usize = 1_024;
const MAX_PREFIX_BYTES: usize = 1_024;
/// Main's own write cap (`maxRuntimeArtifactWriteChars`), restated so an
/// over-long document is refused with a sentence the model can act on instead
/// of a transport error it cannot.
const MAX_CONTENT_CHARS: usize = 64 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 1_000;
const DEFAULT_LIST_LIMIT: i32 = 200;

/// Stable family-toolset construction failure category.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ArtifactToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    InvalidDefinition,
}

pub struct ArtifactToolsetError {
    code: ArtifactToolsetErrorCode,
}

impl ArtifactToolsetError {
    #[must_use]
    pub const fn code(&self) -> ArtifactToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for ArtifactToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ArtifactToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ArtifactToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ArtifactToolsetErrorCode::InvalidConfiguration => {
                "the artifact toolkit configuration is invalid"
            }
            ArtifactToolsetErrorCode::ResourceExhausted => {
                "the artifact toolkit configuration exceeds its approved limit"
            }
            ArtifactToolsetErrorCode::InvalidDefinition => {
                "the artifact ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for ArtifactToolsetError {}

impl From<ArtifactConfigError> for ArtifactToolsetError {
    fn from(source: ArtifactConfigError) -> Self {
        Self {
            code: match source.code() {
                ArtifactConfigErrorCode::InvalidConfiguration => {
                    ArtifactToolsetErrorCode::InvalidConfiguration
                }
                ArtifactConfigErrorCode::ResourceExhausted => {
                    ArtifactToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<MaterializedToolsetError> for ArtifactToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: ArtifactToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the claim-scoped artifact toolset.
///
/// An empty selection means every tool this runtime implements, which is the
/// convention the other families follow and the behaviour the SDK's own
/// toolkit has when `selected_tools` is absent.
pub fn build_artifact_toolset(
    toolkit_name: &str,
    config: &ArtifactToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
    authority: &ArtifactToolAuthority,
) -> Result<BasicToolset, ArtifactToolsetError> {
    let bucket: Arc<str> = Arc::from(config.bucket());
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let include = |name: &str| selected.is_empty() || selected.iter().any(|value| value == name);
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(4);
    if include(LIST_FILES_TOOL) {
        tools.push(Arc::new(ListFilesTool {
            authority: authority.clone(),
            bucket: Arc::clone(&bucket),
            description: tool_description(
                toolkit_name,
                "List the files in this project's artifact bucket. Use `folder` to scope the listing to one prefix and `recursive` to include everything beneath it.",
            ),
        }));
    }
    if include(READ_FILE_TOOL) {
        tools.push(Arc::new(ReadFileTool {
            authority: authority.clone(),
            bucket: Arc::clone(&bucket),
            description: tool_description(
                toolkit_name,
                "Read one text file from this project's artifact bucket. A file larger than the agent-path limit is refused with the limit and the file's real size, not truncated.",
            ),
        }));
    }
    if include(CREATE_FILE_TOOL) {
        tools.push(Arc::new(CreateFileTool {
            authority: authority.clone(),
            bucket: Arc::clone(&bucket),
            description: tool_description(
                toolkit_name,
                "Create a file in this project's artifact bucket, or replace an existing file of the same name ENTIRELY with the content provided.",
            ),
        }));
    }
    if include(DELETE_FILE_TOOL) {
        tools.push(Arc::new(DeleteFileTool {
            authority: authority.clone(),
            bucket: Arc::clone(&bucket),
            description: tool_description(
                toolkit_name,
                "Delete one file from this project's artifact bucket. This cannot be undone: ask the user before deleting anything they did not name.",
            ),
        }));
    }
    admit_materialized_toolset(toolkit_name, "artifact", policy, tools).map_err(Into::into)
}

fn tool_description(toolkit_name: &str, sentence: &str) -> Box<str> {
    format!("Toolkit: {toolkit_name}\n{sentence}")
        .chars()
        .take(MAX_DESCRIPTION_BYTES)
        .collect::<String>()
        .into_boxed_str()
}

/// Turns one transport failure into the sentence the MODEL reads.
///
/// Every artifact failure comes back as a tool RESULT, never an `AdkError`
/// that aborts the turn — the rule the two builder tools already follow
/// (`internal_tools::builder_failure_text`) and the one the SDK's own toolkit
/// follows with `ToolException`. The buckets are chosen so the model's next
/// move differs: a refused bucket says stop asking, a missing file says check
/// the name, a rejected document says write less, and an unavailable platform
/// says tell the user nothing happened.
fn artifact_failure_text(action: &str, error: &HostError) -> String {
    match error.code() {
        HostErrorCode::AuthorizationFailed => format!(
            "Could not {action}: this conversation is not allowed to use that bucket. \
             Do not retry; tell the user."
        ),
        HostErrorCode::NotFound => {
            format!("Could not {action}: no such file or bucket. Check the name with list_files.")
        }
        // A refused document (main's 422) arrives as invalid input.
        HostErrorCode::InvalidInput | HostErrorCode::ResourceExhausted => format!(
            "Could not {action}: the content is empty, not text, or larger than this platform \
             accepts. Shorten it and try once more."
        ),
        _ => format!(
            "Could not {action}: the platform could not be reached. \
             Tell the user it did not happen."
        ),
    }
}

/// Reads one required string argument. Returns `None` so the caller answers
/// the model rather than failing the turn — the same contract the failures
/// above follow.
fn string_argument(arguments: &Value, key: &str) -> Option<String> {
    let value = arguments.as_object()?.get(key)?.as_str()?.trim();
    (!value.is_empty()).then(|| value.to_owned())
}

fn optional_string(arguments: &Value, key: &str) -> Option<String> {
    let value = arguments
        .as_object()?
        .get(key)
        .and_then(Value::as_str)?
        .trim();
    (!value.is_empty()).then(|| value.to_owned())
}

fn optional_bool(arguments: &Value, key: &str) -> Option<bool> {
    arguments
        .as_object()
        .and_then(|object| object.get(key))
        .and_then(Value::as_bool)
}

/// The bucket one call acts on: the argument when the model gave one, the
/// toolkit's configured bucket otherwise.
///
/// An override is safe to honour here because it is not what authorizes
/// anything: main resolves the name inside the CLAIM's project and applies the
/// claim actor's own access list to it, so naming another bucket reaches
/// exactly the buckets that person already reaches.
fn target_bucket(arguments: &Value, configured: &str) -> String {
    optional_string(arguments, "bucket_name").unwrap_or_else(|| configured.to_owned())
}

/// Strips the leading slash a model tends to add. Returns the key main
/// addresses.
///
/// It does NOT split a bucket off the front, and must not: this is what the
/// SDK does to a plain `filename` (`filename.lstrip('/')`, in
/// `elitea_sdk/runtime/tools/artifact.py`), and a folder-qualified name like
/// `/reports/q3.md` is an
/// ordinary key with a folder in it. The bucket-qualified form is a SEPARATE
/// argument — see `parse_filepath`.
fn normalized_key(name: &str) -> String {
    name.trim().trim_start_matches('/').to_owned()
}

/// Splits the SDK's `/{bucket}/{filename}` form into the bucket and the key.
///
/// THIS IS THE FORM THE ATTACHMENT HEADER HANDS THE MODEL. Admission renders
/// `filepath: /{bucket}/{name}` on every attached file (elitea-main's
/// `internal/application/agentexecution/attachments.go`) and tells the model a
/// file-reading tool can open it. The native `read_file` used to take only
/// `filename`, and trimming the slash off that value turned
/// `/chat-attachments/<uuid>/report.pdf` into the key
/// `chat-attachments/<uuid>/report.pdf` INSIDE the toolkit's own bucket — a
/// 404 for a file that was right there. The SDK never had that problem because
/// it exposes `filepath` as its own parameter and parses it (`parse_filepath`,
/// in `elitea_sdk/tools/utils/text_operations.py`); this is that function, rule for
/// rule: strip the leading slashes, split ONCE, and everything after the first
/// segment is the key, folders included.
///
/// `None` for anything that is not that form — no second segment, or an empty
/// half — so the caller can answer the model with a sentence instead of
/// addressing a bucket the model did not name.
fn parse_filepath(filepath: &str) -> Option<(String, String)> {
    let path = filepath.trim().trim_start_matches('/');
    let (bucket, key) = path.split_once('/')?;
    let key = key.trim();
    (!bucket.is_empty() && !key.is_empty()).then(|| (bucket.to_owned(), key.to_owned()))
}

/// The SDK's glob test (`fnmatch`, case-insensitive): `*` is any run of
/// characters including `/`, `?` one character, `[...]` a class (`[!...]`
/// negated). Used for `list_files`' include and skip patterns.
fn glob_matches(pattern: &str, name: &str) -> bool {
    let mut expression = String::from("(?is)^");
    let mut characters = pattern.chars().peekable();
    while let Some(character) = characters.next() {
        match character {
            '*' => expression.push_str(".*"),
            '?' => expression.push('.'),
            '[' => {
                let mut class = String::new();
                let mut closed = false;
                for next in characters.by_ref() {
                    if next == ']' && !class.is_empty() {
                        closed = true;
                        break;
                    }
                    class.push(next);
                }
                if closed {
                    let class = class
                        .strip_prefix('!')
                        .map_or_else(|| class.clone(), |rest| format!("^{rest}"));
                    expression.push('[');
                    expression.push_str(&class.replace('\\', "\\\\"));
                    expression.push(']');
                } else {
                    expression.push_str(&regex::escape(&format!("[{class}")));
                }
            }
            other => expression.push_str(&regex::escape(&other.to_string())),
        }
    }
    expression.push('$');
    regex::Regex::new(&expression).is_ok_and(|compiled| compiled.is_match(name))
}

/// A list of glob patterns argument (`include`, `skip`); absent, null or
/// empty is no patterns.
fn pattern_list(arguments: &Value, key: &str) -> Vec<String> {
    arguments
        .as_object()
        .and_then(|object| object.get(key))
        .and_then(Value::as_array)
        .map(|patterns| {
            patterns
                .iter()
                .filter_map(Value::as_str)
                .map(str::trim)
                .filter(|pattern| !pattern.is_empty())
                .map(str::to_owned)
                .collect()
        })
        .unwrap_or_default()
}

/// The SDK's include/skip rule: kept when it matches some include pattern
/// (or there are none) and no skip pattern.
fn listed(name: &str, include: &[String], skip: &[String]) -> bool {
    (include.is_empty() || include.iter().any(|pattern| glob_matches(pattern, name)))
        && !skip.iter().any(|pattern| glob_matches(pattern, name))
}

fn optional_line(arguments: &Value, key: &str) -> Option<i64> {
    arguments
        .as_object()
        .and_then(|object| object.get(key))
        .and_then(Value::as_i64)
}

/// Lines `start..=end` (1-indexed, inclusive; `end` absent reads to the end)
/// of `content`, as the SDK's partial read returns them. `None` when the range
/// is empty or inverted, so the caller answers with a sentence.
fn line_range(content: &str, start: Option<i64>, end: Option<i64>) -> Option<String> {
    let start = usize::try_from(start.unwrap_or(1).max(1)).ok()?;
    let lines: Vec<&str> = content.split_inclusive('\n').collect();
    let end = match end {
        Some(end) => usize::try_from(end).ok()?.min(lines.len()),
        None => lines.len(),
    };
    (start <= end).then(|| lines[start - 1..end].concat())
}

/// The options of the SDK's `read_file` this runtime reads past rather than
/// applies (page and sheet selection, per-type options, image capture): the
/// platform's artifact read returns a file's text, not a parsed document. A
/// call that sets one still succeeds, and the answer says so in one line
/// before the content, so neither the model nor a pipeline mistakes the
/// whole file for the slice it asked for.
fn unapplied_read_options(arguments: &Value) -> Vec<&'static str> {
    let object = arguments.as_object();
    let set = |key: &str| {
        object
            .and_then(|object| object.get(key))
            .is_some_and(|value| !value.is_null() && value != &Value::Bool(false))
    };
    [
        "page_number",
        "sheet_name",
        "extra_params",
        "is_capture_image",
    ]
    .into_iter()
    .filter(|key| set(key))
    .collect()
}

struct ListFilesTool {
    authority: ArtifactToolAuthority,
    bucket: Arc<str>,
    description: Box<str>,
}

#[async_trait]
impl Tool for ListFilesTool {
    fn name(&self) -> &str {
        LIST_FILES_TOOL
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "bucket_name": {"type": "string", "maxLength": 63, "description": "Bucket to list. Defaults to the toolkit's own bucket."},
                "folder": {"type": "string", "maxLength": MAX_PREFIX_BYTES, "description": "Folder or key prefix to scope the listing to."},
                "recursive": {"type": "boolean", "description": "List everything beneath the folder rather than its immediate children."},
                "include": {"type": "array", "items": {"type": "string"}, "maxItems": 64, "description": "Glob patterns to include (case-insensitive): ['*.md'], ['docs/*'], ['docs/*.md']. Empty includes every file."},
                "skip": {"type": "array", "items": {"type": "string"}, "maxItems": 64, "description": "Glob patterns to exclude (case-insensitive), same syntax as include: ['temp/*', '*.tmp']."}
            },
            "additionalProperties": false
        }))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let folder = optional_string(&arguments, "folder").unwrap_or_default();
        if folder.len() > MAX_PREFIX_BYTES {
            return Ok(Value::String(
                "Could not list the files: the folder name is too long.".to_owned(),
            ));
        }
        let request = ArtifactListRequest {
            bucket: target_bucket(&arguments, &self.bucket),
            prefix: normalized_key(&folder),
            recursive: optional_bool(&arguments, "recursive").unwrap_or(false),
            limit: DEFAULT_LIST_LIMIT,
        };
        let include = pattern_list(&arguments, "include");
        let skip = pattern_list(&arguments, "skip");
        match self.authority.writer().list_artifacts(&request).await {
            Ok(outcome) => {
                let rows = outcome
                    .files
                    .iter()
                    .filter(|file| listed(&file.name, &include, &skip))
                    .map(|file| {
                        json!({
                            "name": file.name,
                            "size": file.byte_length,
                            "media_type": file.media_type,
                            "modified": file.modified_at,
                        })
                    })
                    .collect::<Vec<_>>();
                Ok(json!({
                    "bucket": outcome.bucket,
                    "total": rows.len(),
                    "rows": rows,
                    // Stated rather than implied: a listing cut off at the page
                    // limit that said nothing would read as a complete bucket.
                    "truncated": outcome.truncated,
                }))
            }
            Err(error) => {
                tracing::warn!(
                    event = "agent_artifact_tool_failed",
                    tool = LIST_FILES_TOOL,
                    reason_code = error.reason_code(),
                    "the artifact listing failed"
                );
                Ok(Value::String(artifact_failure_text(
                    "list the files",
                    &error,
                )))
            }
        }
    }
}

struct ReadFileTool {
    authority: ArtifactToolAuthority,
    bucket: Arc<str>,
    description: Box<str>,
}

#[async_trait]
impl Tool for ReadFileTool {
    fn name(&self) -> &str {
        READ_FILE_TOOL
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "filename": {"type": "string", "minLength": 1, "maxLength": MAX_FILENAME_BYTES, "description": "Name of the file to read, as list_files reports it. Not needed when filepath is given."},
                "bucket_name": {"type": "string", "maxLength": 63, "description": "Bucket to read from. Defaults to the toolkit's own bucket. Ignored when filepath is given."},
                "filepath": {"type": "string", "minLength": 1, "maxLength": MAX_FILENAME_BYTES, "description": "Full path in /{bucket}/{filename} format, as an attached file's header reports it. An alternative to filename plus bucket_name."},
                "start_line": {"type": "integer", "minimum": 1, "description": "First line to return (1-indexed, inclusive) for a partial read of a text file."},
                "end_line": {"type": "integer", "minimum": 1, "description": "Last line to return (1-indexed, inclusive). Omit to read to the end."},
                "page_number": {"type": "integer", "description": "Accepted for compatibility; this runtime returns the whole file and says so."},
                "sheet_name": {"type": "string", "description": "Accepted for compatibility; this runtime returns the whole file and says so."},
                "is_capture_image": {"type": "boolean", "description": "Accepted for compatibility; images are not described by this runtime."},
                "extra_params": {"type": "string", "description": "Accepted for compatibility; per-file-type options are not applied by this runtime."}
            },
            "additionalProperties": false
        }))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        // `filepath` FIRST, and it settles the bucket as well as the key: the
        // SDK's own precedence (`if filepath: ... bucket_name = extracted`),
        // and the reason it exists is that the value the model was handed
        // names a bucket that is usually NOT this toolkit's.
        let target = match optional_string(&arguments, "filepath") {
            Some(filepath) => match parse_filepath(&filepath) {
                Some(target) => target,
                None => {
                    return Ok(Value::String(
                        "Could not read the file: filepath must be in /{bucket}/{filename} form."
                            .to_owned(),
                    ));
                }
            },
            None => match string_argument(&arguments, "filename") {
                Some(filename) => (
                    target_bucket(&arguments, &self.bucket),
                    normalized_key(&filename),
                ),
                None => {
                    return Ok(Value::String(
                        "Could not read the file: a non-empty filename or filepath is required."
                            .to_owned(),
                    ));
                }
            },
        };
        let (bucket, name) = target;
        if name.len() > MAX_FILENAME_BYTES {
            return Ok(Value::String(
                "Could not read the file: the file name is too long.".to_owned(),
            ));
        }
        let request = ArtifactReadRequest { bucket, name };
        match self.authority.writer().read_artifact(&request).await {
            // THE SIZE REFUSAL, in the SDK's own structured shape
            // (`build_over_limit_response`, elitea_sdk/tools/utils/
            // file_metadata.py). It NAMES the cap and the actual size, because
            // a refusal that does not leaves the model unable to choose a
            // slice that would fit — and it carries no content at all, because
            // a cap that reported itself and returned the file anyway would
            // have capped nothing.
            Ok(outcome) if outcome.over_limit => Ok(json!({
                "__result_status__": "content_too_large",
                "read_limits": {
                    "max_output_chars": outcome.max_chars,
                    "full_read_allowed": false,
                },
                "context": {
                    "limit_chars": outcome.max_chars,
                    "actual_chars": outcome.char_length,
                    "actual_bytes": outcome.byte_length,
                    "requested": "full file read",
                },
                "total_lines": outcome.total_lines,
                "file": outcome.name,
            })),
            Ok(outcome) => {
                let start = optional_line(&arguments, "start_line");
                let end = optional_line(&arguments, "end_line");
                let content = if start.is_some() || end.is_some() {
                    match line_range(&outcome.content, start, end) {
                        Some(slice) => slice,
                        None => {
                            return Ok(Value::String(format!(
                                "Could not read the file: the line range is empty; it has {} lines.",
                                outcome.total_lines
                            )));
                        }
                    }
                } else {
                    outcome.content
                };
                let unapplied = unapplied_read_options(&arguments);
                if unapplied.is_empty() {
                    Ok(Value::String(content))
                } else {
                    Ok(Value::String(format!(
                        "[{} not applied: this runtime returns the file's text as a whole]\n{content}",
                        unapplied.join(", ")
                    )))
                }
            }
            Err(error) => {
                tracing::warn!(
                    event = "agent_artifact_tool_failed",
                    tool = READ_FILE_TOOL,
                    reason_code = error.reason_code(),
                    "the artifact read failed"
                );
                Ok(Value::String(artifact_failure_text(
                    "read the file",
                    &error,
                )))
            }
        }
    }
}

struct CreateFileTool {
    authority: ArtifactToolAuthority,
    bucket: Arc<str>,
    description: Box<str>,
}

#[async_trait]
impl Tool for CreateFileTool {
    fn name(&self) -> &str {
        CREATE_FILE_TOOL
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        false
    }

    fn is_concurrency_safe(&self) -> bool {
        false
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "filename": {"type": "string", "minLength": 1, "maxLength": MAX_FILENAME_BYTES, "description": "Name of the file to create or replace."},
                "filedata": {"type": "string", "maxLength": MAX_CONTENT_CHARS, "description": "The file's complete new content. This REPLACES the file; include everything that should remain. Leave empty when copying with filepath."},
                "filepath": {"type": "string", "minLength": 1, "maxLength": MAX_FILENAME_BYTES, "description": "Existing file to copy, in /{bucket}/{filename} format, instead of filedata. Text files only in this runtime."},
                "bucket_name": {"type": "string", "maxLength": 63, "description": "Bucket to write to. Defaults to the toolkit's own bucket."}
            },
            "required": ["filename"],
            "additionalProperties": false
        }))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let Some(filename) = string_argument(&arguments, "filename") else {
            return Ok(Value::String(
                "Could not create the file: a non-empty filename is required.".to_owned(),
            ));
        };
        let given = arguments
            .as_object()
            .and_then(|object| object.get("filedata"))
            .and_then(Value::as_str)
            .filter(|value| !value.is_empty())
            .map(str::to_owned);
        let filedata = match (given, optional_string(&arguments, "filepath")) {
            (Some(filedata), _) => filedata,
            (None, Some(filepath)) => match self.copied_text(&filepath).await {
                Ok(text) => text,
                Err(sentence) => return Ok(Value::String(sentence)),
            },
            (None, None) => {
                return Ok(Value::String(
                    "Could not create the file: give filedata, or a filepath to copy.".to_owned(),
                ));
            }
        };
        let filedata = filedata.as_str();
        if filedata.chars().count() > MAX_CONTENT_CHARS {
            return Ok(Value::String(format!(
                "Could not create the file: the content is longer than the {MAX_CONTENT_CHARS} \
                 characters this platform accepts in one write. Write less."
            )));
        }
        let request = ArtifactWriteRequest {
            bucket: target_bucket(&arguments, &self.bucket),
            name: normalized_key(&filename),
            content: filedata.to_owned(),
        };
        match self.authority.writer().write_artifact(&request).await {
            Ok(outcome) => Ok(Value::String(format!(
                "File created successfully at /{}/{} ({} bytes). Tell the user by that path.",
                outcome.bucket, outcome.name, outcome.byte_length
            ))),
            Err(error) => {
                tracing::warn!(
                    event = "agent_artifact_tool_failed",
                    tool = CREATE_FILE_TOOL,
                    reason_code = error.reason_code(),
                    "the artifact write failed"
                );
                Ok(Value::String(artifact_failure_text(
                    "create the file",
                    &error,
                )))
            }
        }
    }
}

impl CreateFileTool {
    /// The text of the file `filepath` names, for the SDK's copy mode
    /// (`create_file` with `filepath` and no `filedata`). The platform's read
    /// returns a file's text and refuses one it cannot (a binary file), and a
    /// file over the read limit comes back without content — so neither is
    /// copied: the model gets a sentence instead of a corrupted copy.
    async fn copied_text(&self, filepath: &str) -> Result<String, String> {
        let Some((bucket, name)) = parse_filepath(filepath) else {
            return Err(
                "Could not copy the file: filepath must be in /{bucket}/{filename} form."
                    .to_owned(),
            );
        };
        match self
            .authority
            .writer()
            .read_artifact(&ArtifactReadRequest { bucket, name })
            .await
        {
            Ok(outcome) if outcome.over_limit => Err(
                "Could not copy the file: it is larger than this runtime copies; copy it in the product instead."
                    .to_owned(),
            ),
            Ok(outcome) => Ok(outcome.content),
            Err(error) => Err(artifact_failure_text("copy the file", &error)),
        }
    }
}

struct DeleteFileTool {
    authority: ArtifactToolAuthority,
    bucket: Arc<str>,
    description: Box<str>,
}

#[async_trait]
impl Tool for DeleteFileTool {
    fn name(&self) -> &str {
        DELETE_FILE_TOOL
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        false
    }

    fn is_concurrency_safe(&self) -> bool {
        false
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "filename": {"type": "string", "minLength": 1, "maxLength": MAX_FILENAME_BYTES, "description": "Name of the file to delete."},
                "bucket_name": {"type": "string", "maxLength": 63, "description": "Bucket to delete from. Defaults to the toolkit's own bucket."}
            },
            "required": ["filename"],
            "additionalProperties": false
        }))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let Some(filename) = string_argument(&arguments, "filename") else {
            return Ok(Value::String(
                "Could not delete the file: a non-empty filename is required.".to_owned(),
            ));
        };
        let request = ArtifactDeleteRequest {
            bucket: target_bucket(&arguments, &self.bucket),
            name: normalized_key(&filename),
        };
        match self.authority.writer().delete_artifact(&request).await {
            Ok(outcome) => Ok(Value::String(format!(
                "File \"{}\" deleted successfully.",
                outcome.name
            ))),
            Err(error) => {
                tracing::warn!(
                    event = "agent_artifact_tool_failed",
                    tool = DELETE_FILE_TOOL,
                    reason_code = error.reason_code(),
                    "the artifact delete failed"
                );
                Ok(Value::String(artifact_failure_text(
                    "delete the file",
                    &error,
                )))
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn globs_follow_fnmatch_case_insensitively() {
        assert!(glob_matches("*.md", "docs/Readme.MD"));
        assert!(glob_matches("docs/*", "docs/a/b.txt"));
        assert!(glob_matches("file[123].txt", "file2.txt"));
        assert!(!glob_matches("file[!123].txt", "file2.txt"));
        assert!(glob_matches("?.txt", "a.txt"));
        assert!(!glob_matches("*.pdf", "a.pdf.txt"));
        assert!(glob_matches("a+b(1).txt", "a+b(1).txt"));
        let include = vec!["*.md".to_owned()];
        let skip = vec!["draft/*".to_owned()];
        assert!(listed("notes.md", &include, &skip));
        assert!(!listed("draft/notes.md", &include, &skip));
        assert!(!listed("notes.txt", &include, &skip));
        assert!(listed("notes.txt", &[], &[]));
    }

    #[test]
    fn line_ranges_are_one_indexed_and_inclusive() {
        let text = "a\nb\nc";
        assert_eq!(line_range(text, Some(2), None).as_deref(), Some("b\nc"));
        assert_eq!(line_range(text, None, Some(1)).as_deref(), Some("a\n"));
        assert_eq!(line_range(text, Some(3), Some(9)).as_deref(), Some("c"));
        assert_eq!(line_range(text, Some(3), Some(2)), None);
        assert_eq!(line_range(text, Some(4), None), None);
    }
}
