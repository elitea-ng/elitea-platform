//! What the Office families (`pptx`, `docx`, `xlsx`; ADR-0032) share: the
//! settings (`bucket_name`, `selected_tools`), the argument helpers, the
//! sentence the model reads for each real failure cause, the object key and
//! name round-trip rules, the platform I/O of one toolkit (`OfficeIo`), the
//! catalogue schema shape and the toolset assembly.
//!
//! A family owns its tools, its schemas and its operations on a document;
//! everything that would otherwise be copied between families lives here, so a
//! fix to one of the rules below is a fix to all three.

use std::fmt;
use std::sync::Arc;

use adk_core::Tool;
use adk_tool::BasicToolset;
use elitea_office_core::{ChartKind, ChartSpec, Limits, OfficeError, Series};
use serde_json::{Map, Value, json};

use super::office_host::{
    MAX_OFFICE_OBJECT_BYTES, OfficeFailure, OfficeHostError, SharedOfficeToolHost,
};
use crate::toolkits::invocation::admit_materialized_toolset;
use crate::toolkits::policy::ToolAdmissionPolicy;

/// The longest object key a tool accepts.
pub(crate) const MAX_FILE_NAME_BYTES: usize = 1_024;
/// The longest free text one argument carries.
pub(crate) const MAX_TEXT_CHARS: usize = 16_000;
const MAX_BUCKET_BYTES: usize = 63;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;
const MAX_DESCRIPTION_BYTES: usize = 1_000;
/// How long a call waits for a free document slot: up to 8 retries at
/// 250 ms, 500 ms, ... (9 s in all), under the engine's 20 s call deadline.
const BUSY_RETRIES: u32 = 8;
const BUSY_BACKOFF: std::time::Duration = std::time::Duration::from_millis(250);

/// A failure as the model reads it.
pub(crate) type Sentence = String;

/// The sentence for a busy engine; the caller retries on exactly this.
pub(crate) const BUSY: &str = "The document engine is busy with other work. Try again in a moment.";

/// What one family is, in the words its sentences use.
#[derive(Clone, Copy, Debug)]
pub(crate) struct OfficeKind {
    /// `pptx`: the toolkit type, for logs.
    pub(crate) family: &'static str,
    /// `PowerPoint (.pptx)`.
    pub(crate) label: &'static str,
    /// `deck`: what "a smaller ..." means.
    pub(crate) noun: &'static str,
    /// What the file store is said to expect when it refuses a type.
    pub(crate) accepts: &'static str,
}

/// One tool of a family: its name, its description and its place in the list.
pub(crate) trait OfficeTool: Copy + Eq + 'static {
    /// The toolkit type (`pptx`).
    const FAMILY: &'static str;
    /// Every tool, in listing order.
    const ALL: &'static [Self];
    /// The tool's name.
    fn name(self) -> &'static str;
    /// The tool's description (the model-facing text).
    fn description(self) -> &'static str;

    /// The tool called `name`, if the family has one.
    fn parse(name: &str) -> Option<Self> {
        Self::ALL.iter().copied().find(|tool| tool.name() == name)
    }
}

// -- Settings ---------------------------------------------------------------

/// Why the settings were refused. Carries no value.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum OfficeConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable, data-free failure for claim-materialized Office settings.
pub(crate) struct OfficeConfigError {
    code: OfficeConfigErrorCode,
    family: &'static str,
}

impl OfficeConfigError {
    pub(crate) const fn code(&self) -> OfficeConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for OfficeConfigError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("OfficeConfigError")
            .field("family", &self.family)
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for OfficeConfigError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self.code {
            OfficeConfigErrorCode::InvalidConfiguration => {
                write!(f, "the {} toolkit configuration is invalid", self.family)
            }
            OfficeConfigErrorCode::ResourceExhausted => write!(
                f,
                "the {} toolkit configuration exceeds its approved limit",
                self.family
            ),
        }
    }
}

impl std::error::Error for OfficeConfigError {}

/// One invocation-scoped Office toolkit: its default bucket and the selected
/// tools. The settings are the SDK `pptx` toolkit's two fields; there is no
/// credential, because the authority is the live execution claim, lent as the
/// Office host.
pub(crate) struct OfficeToolkitConfig<T> {
    bucket: Box<str>,
    /// `None`: the SDK's "no selection means every tool" (an absent, null or
    /// empty list). `Some`: only these, possibly none.
    selected: Option<Vec<T>>,
}

impl<T: OfficeTool> OfficeToolkitConfig<T> {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, OfficeConfigError> {
        let bucket = settings
            .get("bucket_name")
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|value| !value.is_empty())
            .ok_or_else(invalid::<T>)?;
        if bucket.len() > MAX_BUCKET_BYTES {
            return Err(exhausted::<T>());
        }
        if !valid_bucket_name(bucket) {
            return Err(invalid::<T>());
        }
        Ok(Self {
            bucket: bucket.into(),
            selected: selected_tools::<T>(settings)?,
        })
    }

    pub(crate) fn bucket(&self) -> &str {
        &self.bucket
    }

    /// The tools to serve: every tool when none is selected, else the selected
    /// ones the family implements (an unknown name selects nothing, as for the
    /// `artifact` family: the stored catalogue is the SDK's and must not fail a
    /// toolkit created through the product's own form).
    pub(crate) fn tools(&self) -> Vec<T> {
        self.selected.clone().unwrap_or_else(|| T::ALL.to_vec())
    }
}

fn invalid<T: OfficeTool>() -> OfficeConfigError {
    OfficeConfigError {
        code: OfficeConfigErrorCode::InvalidConfiguration,
        family: T::FAMILY,
    }
}

fn exhausted<T: OfficeTool>() -> OfficeConfigError {
    OfficeConfigError {
        code: OfficeConfigErrorCode::ResourceExhausted,
        family: T::FAMILY,
    }
}

/// The bucket-name shape the platform enforces (see the `artifact` family).
fn valid_bucket_name(bucket: &str) -> bool {
    let bytes = bucket.as_bytes();
    bytes.len() >= 2
        && bytes[0].is_ascii_lowercase()
        && bytes[1..]
            .iter()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || *b == b'-')
}

fn selected_tools<T: OfficeTool>(
    settings: &Map<String, Value>,
) -> Result<Option<Vec<T>>, OfficeConfigError> {
    let Some(value) = settings.get("selected_tools") else {
        return Ok(None);
    };
    if value.is_null() {
        return Ok(None);
    }
    let values = value.as_array().ok_or_else(invalid::<T>)?;
    if values.len() > MAX_SELECTED_TOOLS {
        return Err(exhausted::<T>());
    }
    if values.is_empty() {
        return Ok(None);
    }
    let mut selected: Vec<T> = Vec::new();
    for value in values {
        let name = value.as_str().ok_or_else(invalid::<T>)?.trim();
        if name.is_empty() || name.chars().any(char::is_control) {
            return Err(invalid::<T>());
        }
        if name.len() > MAX_TOOL_NAME_BYTES {
            return Err(exhausted::<T>());
        }
        if let Some(tool) = T::parse(name)
            && !selected.contains(&tool)
        {
            selected.push(tool);
        }
    }
    Ok(Some(selected))
}

// -- Toolset ----------------------------------------------------------------

/// Stable family-toolset construction failure category.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum OfficeToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    InvalidDefinition,
}

pub(crate) struct OfficeToolsetError {
    code: OfficeToolsetErrorCode,
    family: &'static str,
}

impl OfficeToolsetError {
    pub(crate) const fn code(&self) -> OfficeToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for OfficeToolsetError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("OfficeToolsetError")
            .field("family", &self.family)
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for OfficeToolsetError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self.code {
            OfficeToolsetErrorCode::InvalidConfiguration => {
                write!(f, "the {} toolkit configuration is invalid", self.family)
            }
            OfficeToolsetErrorCode::ResourceExhausted => write!(
                f,
                "the {} toolkit configuration exceeds its approved limit",
                self.family
            ),
            OfficeToolsetErrorCode::InvalidDefinition => {
                write!(f, "the {} ADK tool definition is invalid", self.family)
            }
        }
    }
}

impl std::error::Error for OfficeToolsetError {}

impl From<OfficeConfigError> for OfficeToolsetError {
    fn from(source: OfficeConfigError) -> Self {
        Self {
            code: match source.code {
                OfficeConfigErrorCode::InvalidConfiguration => {
                    OfficeToolsetErrorCode::InvalidConfiguration
                }
                OfficeConfigErrorCode::ResourceExhausted => {
                    OfficeToolsetErrorCode::ResourceExhausted
                }
            },
            family: source.family,
        }
    }
}

/// The SDK's description rule: `Toolkit: <name>\n` before the description,
/// cut at 1000 characters.
pub(crate) fn tool_description(toolkit_name: &str, text: &str) -> Box<str> {
    format!("Toolkit: {toolkit_name}\n{text}")
        .chars()
        .take(MAX_DESCRIPTION_BYTES)
        .collect::<String>()
        .into_boxed_str()
}

/// The toolset of one Office toolkit: `make` builds the tool object of a tool
/// from its description; the policy admits the set.
pub(crate) fn build_toolset<T: OfficeTool>(
    toolkit_name: &str,
    config: &OfficeToolkitConfig<T>,
    policy: &Arc<ToolAdmissionPolicy>,
    make: impl Fn(T, Box<str>) -> Arc<dyn Tool>,
) -> Result<BasicToolset, OfficeToolsetError> {
    let tools: Vec<Arc<dyn Tool>> = config
        .tools()
        .into_iter()
        .map(|tool| make(tool, tool_description(toolkit_name, tool.description())))
        .collect();
    admit_materialized_toolset(toolkit_name, T::FAMILY, policy, tools).map_err(|_| {
        OfficeToolsetError {
            code: OfficeToolsetErrorCode::InvalidDefinition,
            family: T::FAMILY,
        }
    })
}

// -- Arguments --------------------------------------------------------------

pub(crate) fn req_str(args: &Value, key: &str) -> Result<String, Sentence> {
    opt_str(args, key).ok_or_else(|| format!("`{key}` is required and must be a non-empty string."))
}

pub(crate) fn opt_str(args: &Value, key: &str) -> Option<String> {
    let v = args.get(key)?.as_str()?;
    (!v.trim().is_empty()).then(|| v.to_owned())
}

pub(crate) fn opt_bool(args: &Value, key: &str) -> Option<bool> {
    args.get(key)?.as_bool()
}

pub(crate) fn opt_f64(args: &Value, key: &str) -> Option<f64> {
    args.get(key)?.as_f64().filter(|v| v.is_finite())
}

pub(crate) fn opt_uint(args: &Value, key: &str) -> Option<u64> {
    args.get(key)?.as_u64()
}

// -- Sentences --------------------------------------------------------------

/// An [`OfficeError`] as a sentence the model can act on. Internal detail
/// stays in the log, not in the sentence.
pub(crate) fn office_sentence(error: &OfficeError, kind: &OfficeKind) -> Sentence {
    match error {
        OfficeError::LimitExceeded { limit, value } => format!(
            "The document is over a safety limit ({limit}, measured {value}). Use a smaller file or fewer elements."
        ),
        OfficeError::Malformed(_) => format!(
            "The file is not a valid {} package, or it is damaged.",
            kind.label
        ),
        OfficeError::Unsupported(message)
        | OfficeError::NotFound(message)
        | OfficeError::InvalidInput(message) => message.clone(),
        OfficeError::DeadlineExceeded => format!(
            "Processing the document took too long and was abandoned. Try a smaller {}.",
            kind.noun
        ),
        OfficeError::Indeterminate => {
            "An edit timed out and may have half-applied; nothing was saved. Try again.".to_owned()
        }
        OfficeError::Busy => BUSY.to_owned(),
        OfficeError::Internal(_) | _ => {
            tracing::warn!(
                event = "office_tool_internal_error",
                family = kind.family,
                "the document engine failed internally"
            );
            "The document engine failed on this file.".to_owned()
        }
    }
}

/// A host failure as the model reads it: one sentence per real cause.
pub(crate) fn host_sentence(action: &str, error: &OfficeHostError, kind: &OfficeKind) -> Sentence {
    tracing::warn!(
        event = "office_tool_host_failed",
        family = kind.family,
        reason_code = error.reason_code(),
        action,
        "an Office tool's platform call failed"
    );
    match error.kind() {
        OfficeFailure::NotFound => {
            format!("Could not {action}: no such file in the bucket. Check the file name.")
        }
        OfficeFailure::AccessRefused => format!(
            "Could not {action}: the platform refused access to the file store. Do not retry; tell the user."
        ),
        OfficeFailure::AlreadyExists => format!(
            "Could not {action}: a file with that name already exists. Choose another name, or ask to replace it."
        ),
        OfficeFailure::UnsupportedType => format!(
            "Could not {action}: the file store does not accept that type of file ({} is expected).",
            kind.accepts
        ),
        OfficeFailure::InvalidRequest => format!(
            "Could not {action}: the file name or bucket is not valid, or the request was refused as invalid."
        ),
        OfficeFailure::TooLarge => {
            format!("Could not {action}: the file is larger than the platform accepts.")
        }
        OfficeFailure::BudgetRefused => format!(
            "Could not {action}: the model budget is exhausted. Do not retry; tell the user."
        ),
        OfficeFailure::NoModel => format!(
            "Could not {action}: this tool needs a chat model and none is bound in this run (a pipeline node binds none). Use list_placeholders and replace_placeholders with your own content, or run it from an agent."
        ),
        OfficeFailure::Unreachable => format!(
            "Could not {action}: the platform could not be reached. Tell the user it did not happen."
        ),
    }
}

// -- Names ------------------------------------------------------------------

/// The key a file is stored under: what the tool accepts as `file_name`. A
/// leading slash is dropped, and so is a leading `/{bucket}/` of the
/// toolkit's own bucket, so the path form the platform shows for a file works
/// as the next call's name.
pub(crate) fn object_key(bucket: &str, name: &str) -> Result<String, Sentence> {
    let trimmed = name.trim();
    let key = if let Some(rooted) = trimmed.strip_prefix('/') {
        let rooted = rooted.trim_start_matches('/');
        rooted
            .strip_prefix(bucket)
            .and_then(|rest| rest.strip_prefix('/'))
            .unwrap_or(rooted)
    } else {
        trimmed
    };
    if key.is_empty() || key.len() > MAX_FILE_NAME_BYTES {
        return Err("The file name is empty or too long.".to_owned());
    }
    if key.split('/').any(|segment| segment == "..") {
        return Err("The file name may not contain `..`.".to_owned());
    }
    Ok(key.to_owned())
}

/// A file the tool stored: the bucket and the key (the name every tool of the
/// family accepts), named separately.
pub(crate) struct Stored {
    pub(crate) bucket: String,
    pub(crate) key: String,
    pub(crate) bytes: u64,
}

impl Stored {
    pub(crate) fn said(&self) -> String {
        format!(
            "`{}` in bucket `{}` ({} bytes)",
            self.key, self.bucket, self.bytes
        )
    }
}

/// The directory prefix of a key, with its trailing slash (empty for a file at
/// the bucket's root): where an output with no name of its own goes, next to
/// the source.
pub(crate) fn directory_of(key: &str) -> &str {
    key.rfind('/').map_or("", |i| &key[..=i])
}

pub(crate) fn stem(name: &str) -> &str {
    let file = name.rsplit('/').next().unwrap_or(name);
    file.rsplit_once('.').map_or(file, |(stem, _)| stem)
}

/// The output of a tool that was given none: next to the source, with the
/// source's name and `suffix` (`.pdf`, `-page-1.png`).
pub(crate) fn beside(bucket: &str, source: &str, suffix: &str) -> String {
    let source = object_key(bucket, source).unwrap_or_else(|_| source.to_owned());
    format!("{}{}{suffix}", directory_of(&source), stem(&source))
}

// -- Platform I/O -----------------------------------------------------------

/// The platform I/O and the blocking pool of one toolkit.
pub(crate) struct OfficeIo {
    pub(crate) kind: OfficeKind,
    pub(crate) bucket: Arc<str>,
    pub(crate) host: SharedOfficeToolHost,
    pub(crate) limits: Arc<Limits>,
}

impl OfficeIo {
    pub(crate) fn host_sentence(&self, action: &str, error: &OfficeHostError) -> Sentence {
        host_sentence(action, error, &self.kind)
    }

    /// The bytes of one file of the toolkit's bucket.
    pub(crate) async fn read(&self, name: &str) -> Result<Vec<u8>, Sentence> {
        let key = object_key(&self.bucket, name)?;
        self.host
            .read_object(&self.bucket, &key, MAX_OFFICE_OBJECT_BYTES)
            .await
            .map_err(|e| self.host_sentence(&format!("read `{key}`"), &e))
    }

    /// Store `bytes` and report where. `overwrite` is false only for a file
    /// the caller must not replace by accident (the create tools); every other
    /// output is regenerated or an edit in place, and replaces.
    pub(crate) async fn write(
        &self,
        name: &str,
        media_type: &str,
        bytes: Vec<u8>,
        overwrite: bool,
    ) -> Result<Stored, OfficeHostError> {
        let key = object_key(&self.bucket, name).map_err(|_| {
            OfficeHostError::new(OfficeFailure::InvalidRequest, "office_host.invalid_name")
        })?;
        let written = self
            .host
            .write_object(&self.bucket, &key, media_type, bytes, overwrite)
            .await?;
        Ok(Stored {
            bucket: written.bucket,
            key: written.name,
            bytes: written.byte_length,
        })
    }

    /// `write` with replacement, the failure as the sentence the model reads.
    pub(crate) async fn save(
        &self,
        name: &str,
        media_type: &str,
        bytes: Vec<u8>,
    ) -> Result<Stored, Sentence> {
        self.write(name, media_type, bytes, true)
            .await
            .map_err(|e| self.host_sentence(&format!("save `{name}`"), &e))
    }

    /// Create a file that must not replace an existing one unless asked: the
    /// server's refusal of a taken name is "already exists".
    pub(crate) async fn create(
        &self,
        name: &str,
        media_type: &str,
        bytes: Vec<u8>,
        overwrite: bool,
    ) -> Result<Stored, Sentence> {
        self.write(name, media_type, bytes, overwrite)
            .await
            .map_err(|e| {
                if e.kind() == OfficeFailure::AlreadyExists {
                    format!("`{name}` already exists. Pass overwrite=true to replace it, or choose another name.")
                } else {
                    self.host_sentence(&format!("create `{name}`"), &e)
                }
            })
    }

    /// Run blocking document work off the async workers.
    ///
    /// The engine admits a few documents at a time per process
    /// (`Limits::max_concurrent`, ADR-0032 decision 3) and answers `Busy` at
    /// once rather than queueing; a call waits its turn here instead, a short
    /// while, before it tells the model to try again. `work` runs once per
    /// attempt and borrows what it needs.
    pub(crate) async fn blocking<T, F>(&self, work: F) -> Result<T, Sentence>
    where
        T: Send + 'static,
        F: Fn(&Limits) -> Result<T, Sentence> + Send + Sync + 'static,
    {
        let work = Arc::new(work);
        let mut attempt = 0u32;
        loop {
            let (limits, run) = (Arc::clone(&self.limits), Arc::clone(&work));
            let outcome = tokio::task::spawn_blocking(move || run(&limits))
                .await
                .map_err(|_| "The document engine stopped unexpectedly on this file.".to_owned())?;
            match outcome {
                Err(sentence) if sentence == BUSY && attempt < BUSY_RETRIES => {
                    attempt += 1;
                    tokio::time::sleep(BUSY_BACKOFF * attempt).await;
                }
                other => return other,
            }
        }
    }
}

// -- Catalogue schemas ------------------------------------------------------

/// A tool's argument schema as the toolkit editor's catalogue carries it: the
/// shape of the SDK's pydantic schemas (a `title` on the object and on every
/// property, no `additionalProperties`, `anyOf` for a union), so the form and
/// the test pane render it like any SDK tool.
pub(crate) fn catalogue_schema(tool_name: &str, mut schema: Value) -> Value {
    let Some(object) = schema.as_object_mut() else {
        return schema;
    };
    object.remove("additionalProperties");
    let camel = |text: &str, joiner: &str| -> String {
        text.split('_')
            .map(|w| {
                let mut c = w.chars();
                c.next().map_or_else(String::new, |f| {
                    f.to_uppercase().collect::<String>() + c.as_str()
                })
            })
            .collect::<Vec<_>>()
            .join(joiner)
    };
    object.insert(
        "title".into(),
        format!("{}Args", camel(tool_name, "")).into(),
    );
    if let Some(props) = object.get_mut("properties").and_then(Value::as_object_mut) {
        for (name, prop) in props.iter_mut() {
            if let Some(p) = prop.as_object_mut() {
                p.insert("title".into(), camel(name, " ").into());
                if let Some(Value::Array(kinds)) = p.get("type").cloned() {
                    p.remove("type");
                    p.insert(
                        "anyOf".into(),
                        Value::Array(kinds.into_iter().map(|k| json!({"type": k})).collect()),
                    );
                }
            }
        }
    }
    schema
}

pub(crate) fn object(properties: &[(&str, Value)], required: &[&str]) -> Value {
    let props: Map<String, Value> = properties
        .iter()
        .map(|(name, schema)| ((*name).to_owned(), schema.clone()))
        .collect();
    json!({
        "type": "object",
        "properties": props,
        "required": required,
        "additionalProperties": false
    })
}

pub(crate) fn file_param(description: &str) -> Value {
    json!({"type": "string", "minLength": 1, "maxLength": MAX_FILE_NAME_BYTES, "description": description})
}

pub(crate) fn out_param(description: &str) -> Value {
    json!({"type": "string", "maxLength": MAX_FILE_NAME_BYTES, "description": description})
}

pub(crate) fn text_param(description: &str, max: usize) -> Value {
    json!({"type": "string", "maxLength": max, "description": description})
}

/// A family's tools as the `rust_only_toolkit_tools.json` document carries
/// them: each tool name with its catalogue schema.
pub(crate) fn catalogue_tools<T: OfficeTool>(
    skip: impl Fn(T) -> bool,
    schema: impl Fn(T) -> Value,
) -> Map<String, Value> {
    T::ALL
        .iter()
        .copied()
        .filter(|t| !skip(*t))
        .map(|t| (t.name().to_owned(), catalogue_schema(t.name(), schema(t))))
        .collect()
}

// -- Charts -----------------------------------------------------------------

/// Every chart type name the families know, with its kind.
const CHART_TYPES: [(&str, ChartKind); 9] = [
    ("bar", ChartKind::ClusteredBar),
    ("stacked_bar", ChartKind::StackedBar),
    ("column", ChartKind::ClusteredColumn),
    ("stacked_column", ChartKind::StackedColumn),
    ("line", ChartKind::Line),
    ("pie", ChartKind::Pie),
    ("doughnut", ChartKind::Doughnut),
    ("area", ChartKind::Area),
    ("scatter", ChartKind::Scatter),
];

/// The kind named by `chart_type` (default `column`), when the family offers
/// it: `allowed` lists the names the family's schema carries.
pub(crate) fn chart_kind(name: Option<&str>, allowed: &[&str]) -> Result<ChartKind, Sentence> {
    let name = name.unwrap_or("column");
    let kind = CHART_TYPES
        .iter()
        .find(|(n, _)| *n == name)
        .map(|(_, k)| *k)
        .ok_or_else(|| format!("Unknown chart_type `{name}`."))?;
    if !allowed.contains(&name) {
        return Err(format!(
            "chart_type `{name}` is not available here: use one of {}.",
            allowed.join(", ")
        ));
    }
    Ok(kind)
}

/// A chart from `categories` and `series` (a name and one number per
/// category), refused as a whole when any series does not line up.
pub(crate) fn chart_spec(args: &Value, allowed: &[&str]) -> Result<ChartSpec, Sentence> {
    let categories: Vec<String> = args
        .get("categories")
        .and_then(Value::as_array)
        .ok_or_else(|| "`categories` is required: a list of labels.".to_owned())?
        .iter()
        .map(|c| match c {
            Value::String(s) => s.clone(),
            other => other.to_string(),
        })
        .collect();
    let series = args
        .get("series")
        .and_then(Value::as_array)
        .ok_or_else(|| "`series` is required: a list of {name, values}.".to_owned())?
        .iter()
        .map(|s| {
            let name = s
                .get("name")
                .and_then(Value::as_str)
                .ok_or_else(|| "Every series needs a `name`.".to_owned())?;
            let values = s
                .get("values")
                .and_then(Value::as_array)
                .ok_or_else(|| format!("Series `{name}` needs `values`."))?
                .iter()
                .map(|v| {
                    v.as_f64()
                        .filter(|f| f.is_finite())
                        .ok_or_else(|| format!("Series `{name}` has a value that is not a number."))
                })
                .collect::<Result<Vec<f64>, _>>()?;
            if values.len() != categories.len() {
                return Err(format!(
                    "Series `{name}` has {} values for {} categories: give exactly one per category.",
                    values.len(),
                    categories.len()
                ));
            }
            Ok(Series {
                name: name.to_owned(),
                values,
            })
        })
        .collect::<Result<Vec<_>, Sentence>>()?;
    Ok(ChartSpec {
        kind: chart_kind(args.get("chart_type").and_then(Value::as_str), allowed)?,
        categories,
        series,
        title: opt_str(args, "title"),
        legend_position: opt_str(args, "legend_position"),
        data_labels: opt_bool(args, "data_labels").unwrap_or(false),
    })
}

/// The JSON schema of the chart arguments the families share.
pub(crate) fn chart_properties(types: &[&str]) -> Vec<(&'static str, Value)> {
    vec![
        (
            "chart_type",
            json!({"type": "string", "enum": types, "description": "The chart type (default column)."}),
        ),
        (
            "categories",
            json!({"type": "array", "minItems": 1, "maxItems": 500, "items": {"type": "string", "maxLength": 200}, "description": "Category labels."}),
        ),
        (
            "series",
            json!({"type": "array", "minItems": 1, "maxItems": 20, "items": {"type": "object", "properties": {"name": {"type": "string", "maxLength": 200}, "values": {"type": "array", "items": {"type": "number"}, "maxItems": 500}}, "required": ["name", "values"], "additionalProperties": false}, "description": "The series: a name and one number per category."}),
        ),
        ("title", text_param("Chart title.", 500)),
        (
            "legend_position",
            json!({"type": "string", "enum": ["top", "bottom", "left", "right"], "description": "Where the legend goes. Omit for none."}),
        ),
        (
            "data_labels",
            json!({"type": "boolean", "description": "Show the value on each point."}),
        ),
    ]
}

/// A table's rows from `rows`: a non-empty list of equally long lists of
/// strings, within `max_rows` by `max_cols`.
pub(crate) fn table_rows(
    args: &Value,
    max_rows: usize,
    max_cols: usize,
) -> Result<Vec<Vec<String>>, Sentence> {
    let rows: Vec<Vec<String>> = args
        .get("rows")
        .and_then(Value::as_array)
        .ok_or_else(|| "`rows` is required: a list of rows, each a list of cell text.".to_owned())?
        .iter()
        .map(|row| {
            row.as_array()
                .ok_or_else(|| "Every row must be a list of cell text.".to_owned())
                .map(|cells| {
                    cells
                        .iter()
                        .map(|c| c.as_str().map_or_else(|| c.to_string(), str::to_owned))
                        .collect()
                })
        })
        .collect::<Result<_, _>>()?;
    let width = rows.first().map_or(0, Vec::len);
    if rows.is_empty() || width == 0 {
        return Err("A table needs at least one row and one column.".to_owned());
    }
    if rows.len() > max_rows || width > max_cols {
        return Err(format!(
            "A table may have at most {max_rows} rows and {max_cols} columns."
        ));
    }
    if rows.iter().any(|r| r.len() != width) {
        return Err("Every row must have the same number of cells.".to_owned());
    }
    Ok(rows)
}

/// The catalogue entry of a toolkit type that has no SDK counterpart (the
/// `new_types` section of `rust_only_toolkit_tools.json`): its metadata, the
/// settings form (`bucket_name`, `selected_tools`, as the SDK `pptx` toolkit
/// declares them) and the argument schema of every tool.
pub(crate) fn type_entry(
    toolkit_type: &str,
    label: &str,
    bucket_description: &str,
    extra_categories: &[&str],
    tools: Map<String, Value>,
) -> Value {
    let mut names: Vec<&String> = tools.keys().collect();
    // Sorted, so the document is the same with or without `preserve_order`.
    names.sort();
    json!({
        "metadata": {
            "categories": ["office"],
            "check_connection_supported": false,
            "extra_categories": extra_categories,
            "has_function_validators": false,
            "icon_url": null,
            "label": label,
        },
        "settings": {
            "properties": {
                "bucket_name": {
                    "description": bucket_description,
                    "title": "Bucket Name",
                    "type": "string",
                },
                "selected_tools": {
                    "default": [],
                    "items": {"enum": names, "type": "string"},
                    "title": "Selected Tools",
                    "type": "array",
                },
            },
            "required": ["bucket_name"],
            "title": toolkit_type,
            "type": "object",
        },
        "args_schemas": Value::Object(tools),
    })
}
