use std::fmt;
use std::sync::Arc;

use adk_rust::tool::BasicToolset;
use adk_rust::{Tool, ToolContext};
use async_trait::async_trait;
use serde_json::{Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;
use crate::transport::runtime_context::{
    ArtifactDeleteRequest, ArtifactListRequest, ArtifactReadRequest, ArtifactWriteRequest,
    RuntimeContextError,
};

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
pub(crate) enum ArtifactToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    InvalidDefinition,
}

pub(crate) struct ArtifactToolsetError {
    code: ArtifactToolsetErrorCode,
}

impl ArtifactToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> ArtifactToolsetErrorCode {
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
pub(crate) fn build_artifact_toolset(
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
fn artifact_failure_text(action: &str, error: &RuntimeContextError) -> String {
    match error {
        RuntimeContextError::AuthorizationFailed(_) => format!(
            "Could not {action}: this conversation is not allowed to use that bucket. \
             Do not retry; tell the user."
        ),
        RuntimeContextError::NotFound(_) => {
            format!("Could not {action}: no such file or bucket. Check the name with list_files.")
        }
        RuntimeContextError::Rejected(_) | RuntimeContextError::ResourceExhausted(_) => format!(
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
                "recursive": {"type": "boolean", "description": "List everything beneath the folder rather than its immediate children."}
            },
            "additionalProperties": false
        }))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
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
        match self
            .authority
            .platform()
            .list_artifacts(self.authority.authority(), &request)
            .await
        {
            Ok(outcome) => {
                let rows = outcome
                    .files
                    .iter()
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
                    reason_code = error.code(),
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
                "filepath": {"type": "string", "minLength": 1, "maxLength": MAX_FILENAME_BYTES, "description": "Full path in /{bucket}/{filename} format, as an attached file's header reports it. An alternative to filename plus bucket_name."}
            },
            "additionalProperties": false
        }))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
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
        match self
            .authority
            .platform()
            .read_artifact(self.authority.authority(), &request)
            .await
        {
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
            Ok(outcome) => Ok(Value::String(outcome.content)),
            Err(error) => {
                tracing::warn!(
                    event = "agent_artifact_tool_failed",
                    tool = READ_FILE_TOOL,
                    reason_code = error.code(),
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
                "filedata": {"type": "string", "minLength": 1, "maxLength": MAX_CONTENT_CHARS, "description": "The file's complete new content. This REPLACES the file; include everything that should remain."},
                "bucket_name": {"type": "string", "maxLength": 63, "description": "Bucket to write to. Defaults to the toolkit's own bucket."}
            },
            "required": ["filename", "filedata"],
            "additionalProperties": false
        }))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let Some(filename) = string_argument(&arguments, "filename") else {
            return Ok(Value::String(
                "Could not create the file: a non-empty filename is required.".to_owned(),
            ));
        };
        let Some(filedata) = arguments
            .as_object()
            .and_then(|object| object.get("filedata"))
            .and_then(Value::as_str)
            .filter(|value| !value.is_empty())
        else {
            return Ok(Value::String(
                "Could not create the file: non-empty filedata is required.".to_owned(),
            ));
        };
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
        match self
            .authority
            .platform()
            .write_artifact(self.authority.authority(), &request)
            .await
        {
            Ok(outcome) => Ok(Value::String(format!(
                "File created successfully at /{}/{} ({} bytes). Tell the user by that path.",
                outcome.bucket, outcome.name, outcome.byte_length
            ))),
            Err(error) => {
                tracing::warn!(
                    event = "agent_artifact_tool_failed",
                    tool = CREATE_FILE_TOOL,
                    reason_code = error.code(),
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
    ) -> adk_rust::Result<Value> {
        let Some(filename) = string_argument(&arguments, "filename") else {
            return Ok(Value::String(
                "Could not delete the file: a non-empty filename is required.".to_owned(),
            ));
        };
        let request = ArtifactDeleteRequest {
            bucket: target_bucket(&arguments, &self.bucket),
            name: normalized_key(&filename),
        };
        match self
            .authority
            .platform()
            .delete_artifact(self.authority.authority(), &request)
            .await
        {
            Ok(outcome) => Ok(Value::String(format!(
                "File \"{}\" deleted successfully.",
                outcome.name
            ))),
            Err(error) => {
                tracing::warn!(
                    event = "agent_artifact_tool_failed",
                    tool = DELETE_FILE_TOOL,
                    reason_code = error.code(),
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
