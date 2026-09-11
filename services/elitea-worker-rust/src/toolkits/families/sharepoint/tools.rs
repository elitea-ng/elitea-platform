use std::fmt;
use std::sync::Arc;

use adk_rust::tool::BasicToolset;
use adk_rust::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::delegated_auth::{
    DelegatedAuthorizationCatalog, DelegatedAuthorizationRequirement, delegated_authorization_error,
};
use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::{ToolAdmissionDecision, ToolAdmissionPolicy};

use super::client::{
    MAX_FILES, MAX_ONENOTE_PAGES, SharePointApi, SharePointClient, SharePointClientError,
    SharePointClientErrorCode, SharePointFileListOptions, normalize_patterns,
};
use super::config::{SharePointConfigError, SharePointConfigErrorCode, SharePointToolkitConfig};

const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_TEXT_BYTES: usize = 8 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 1_000;
const MAX_SELECT_FIELDS: usize = 128;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SharePointToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedCapability,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

pub(crate) struct SharePointToolsetError {
    code: SharePointToolsetErrorCode,
}

impl SharePointToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> SharePointToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for SharePointToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SharePointToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for SharePointToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            SharePointToolsetErrorCode::InvalidConfiguration => {
                "the SharePoint toolkit configuration is invalid"
            }
            SharePointToolsetErrorCode::ResourceExhausted => {
                "the SharePoint toolkit exceeds its approved limit"
            }
            SharePointToolsetErrorCode::UnsupportedCapability => {
                "the SharePoint toolkit requires an unavailable runtime authority"
            }
            SharePointToolsetErrorCode::UnsupportedSelection => {
                "the selected SharePoint tool profile is not supported"
            }
            SharePointToolsetErrorCode::Client => "the SharePoint client could not be created",
            SharePointToolsetErrorCode::InvalidDefinition => {
                "the SharePoint ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for SharePointToolsetError {}

impl From<SharePointConfigError> for SharePointToolsetError {
    fn from(source: SharePointConfigError) -> Self {
        Self {
            code: match source.code() {
                SharePointConfigErrorCode::InvalidConfiguration => {
                    SharePointToolsetErrorCode::InvalidConfiguration
                }
                SharePointConfigErrorCode::ResourceExhausted => {
                    SharePointToolsetErrorCode::ResourceExhausted
                }
                SharePointConfigErrorCode::UnsupportedCapability => {
                    SharePointToolsetErrorCode::UnsupportedCapability
                }
            },
        }
    }
}

impl From<SharePointClientError> for SharePointToolsetError {
    fn from(_: SharePointClientError) -> Self {
        Self {
            code: SharePointToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for SharePointToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: SharePointToolsetErrorCode::InvalidDefinition,
        }
    }
}

pub(crate) struct MaterializedSharePointToolset {
    pub(crate) toolset: BasicToolset,
    pub(crate) delegated_authorization: DelegatedAuthorizationCatalog,
}

/// Build the explicitly selected, artifact-free `SharePoint` Graph read core.
///
/// Empty selection and selections without a supported read fail closed while
/// this family is partial. A mixed saved selection exposes only its explicitly
/// selected supported reads. Content, indexing, and effect operations remain
/// unavailable until their separate authorities exist.
pub(crate) fn build_sharepoint_toolset(
    toolkit_name: &str,
    config: SharePointToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<MaterializedSharePointToolset, SharePointToolsetError> {
    let selected = validate_selection(config.selected_tools())?;
    let site_url = config.site_url().to_string();
    let requirement = config.authorization().clone();
    let mut delegated_authorization = DelegatedAuthorizationCatalog::default();
    let tools: Vec<Arc<dyn Tool>> = if config.requires_authorization() {
        selected
            .into_iter()
            .map(|kind| {
                if policy.tool_decision("sharepoint", kind.name()) == ToolAdmissionDecision::Allowed
                {
                    delegated_authorization
                        .insert(kind.name(), requirement.clone())
                        .map_err(|()| invalid_definition())?;
                }
                Ok(Arc::new(SharePointAuthorizationRequiredTool::new(
                    kind,
                    toolkit_name,
                    &site_url,
                    requirement.clone(),
                )) as Arc<dyn Tool>)
            })
            .collect::<Result<Vec<_>, SharePointToolsetError>>()?
    } else {
        let client: Arc<dyn SharePointApi> =
            Arc::new(SharePointClient::new(config.into_client_parts()?)?);
        selected
            .into_iter()
            .map(|kind| {
                Arc::new(SharePointTool::new(
                    kind,
                    toolkit_name,
                    &site_url,
                    Arc::clone(&client),
                )) as Arc<dyn Tool>
            })
            .collect()
    };
    let toolset = admit_materialized_toolset(toolkit_name, "sharepoint", policy, tools)?;
    Ok(MaterializedSharePointToolset {
        toolset,
        delegated_authorization,
    })
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    site_url: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn SharePointApi>,
) -> Result<BasicToolset, SharePointToolsetError> {
    let selected = selected
        .iter()
        .map(|name| name.clone().into_boxed_str())
        .collect::<Vec<_>>();
    let selected = validate_selection(&selected)?;
    let tools = selected
        .into_iter()
        .map(|kind| {
            Arc::new(SharePointTool::new(
                kind,
                toolkit_name,
                site_url,
                Arc::clone(client),
            )) as Arc<dyn Tool>
        })
        .collect();
    admit_materialized_toolset(toolkit_name, "sharepoint", policy, tools).map_err(Into::into)
}

fn validate_selection(
    selected: &[Box<str>],
) -> Result<Vec<SharePointToolKind>, SharePointToolsetError> {
    if selected.is_empty() {
        return Err(unsupported_selection());
    }
    let supported = selected
        .iter()
        .filter_map(|name| {
            SharePointToolKind::ALL
                .iter()
                .copied()
                .find(|kind| kind.name() == name.as_ref())
        })
        .collect::<Vec<_>>();
    if supported.is_empty() {
        return Err(unsupported_selection());
    }
    let omitted = selected.len().saturating_sub(supported.len());
    if omitted > 0 {
        tracing::warn!(
            event = "sharepoint_tool_selection_partially_materialized",
            selected_tool_count = selected.len(),
            materialized_tool_count = supported.len(),
            omitted_tool_count = omitted,
            "deferred SharePoint operations were omitted from the native toolset"
        );
    }
    Ok(supported)
}

#[derive(Clone, Copy)]
enum SharePointToolKind {
    ReadList,
    GetLists,
    GetListColumns,
    GetFilesList,
    OneNoteGetNotebooks,
    OneNoteGetSections,
    OneNoteGetPages,
    OneNoteGetPageContent,
}

impl SharePointToolKind {
    const ALL: [Self; 8] = [
        Self::ReadList,
        Self::GetLists,
        Self::GetListColumns,
        Self::GetFilesList,
        Self::OneNoteGetNotebooks,
        Self::OneNoteGetSections,
        Self::OneNoteGetPages,
        Self::OneNoteGetPageContent,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::ReadList => "read_list",
            Self::GetLists => "get_lists",
            Self::GetListColumns => "get_list_columns",
            Self::GetFilesList => "get_files_list",
            Self::OneNoteGetNotebooks => "onenote_get_notebooks",
            Self::OneNoteGetSections => "onenote_get_sections",
            Self::OneNoteGetPages => "onenote_get_pages",
            Self::OneNoteGetPageContent => "onenote_get_page_content",
        }
    }

    const fn description(self) -> &'static str {
        match self {
            Self::ReadList => {
                "Read fields from one exact SharePoint list by title. limit defaults to 1000 and is bounded to 1 through 1000. The client resolves the list case-insensitively, follows at most 64 Graph pages, and returns only the item fields."
            }
            Self::GetLists => {
                "List visible SharePoint lists for the configured site with Title, Id, Description, ItemCount, and BaseTemplate. Hidden lists, document libraries, and site-page libraries are excluded; use get_files_list for document-library metadata."
            }
            Self::GetListColumns => {
                "Return writable non-lookup column metadata for one SharePoint list. Use this to discover internal names, display names, simplified types, required flags, and choice values before planning a separately authorized write."
            }
            Self::GetFilesList => {
                "Recursively list bounded file metadata across the configured site's Graph drives. folder_name scopes a subfolder, form_name pins one document library, and include_extensions/skip_extensions accept case-insensitive extension, filename, or star patterns. No file content is downloaded."
            }
            Self::OneNoteGetNotebooks => {
                "List OneNote notebook metadata in the configured SharePoint site. Omit select for the SDK default fields, pass an empty list for all provider fields, or pass an explicit bounded field list."
            }
            Self::OneNoteGetSections => {
                "List OneNote section metadata for one exact notebook ID. Omit select for the SDK default fields, pass an empty list for all provider fields, or pass an explicit bounded field list."
            }
            Self::OneNoteGetPages => {
                "List at most 100 OneNote page metadata objects for one exact section ID. contentUrl is removed from explicit select because Microsoft Graph rejects it; use onenote_get_page_content for raw XHTML."
            }
            Self::OneNoteGetPageContent => {
                "Return one bounded UTF-8 raw XHTML document for an exact OneNote page ID. This tool does not download embedded images or attachments and does not invoke a vision or document parser."
            }
        }
    }
}

struct SharePointTool {
    kind: SharePointToolKind,
    client: Arc<dyn SharePointApi>,
    description: Box<str>,
}

impl SharePointTool {
    fn new(
        kind: SharePointToolKind,
        toolkit_name: &str,
        site_url: &str,
        client: Arc<dyn SharePointApi>,
    ) -> Self {
        Self {
            kind,
            client,
            description: bounded_description(kind, toolkit_name, site_url),
        }
    }
}

#[async_trait]
impl Tool for SharePointTool {
    fn name(&self) -> &str {
        self.kind.name()
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
        Some(schema_for(self.kind))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        validate_argument_size(&arguments)?;
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        let result = match self.kind {
            SharePointToolKind::ReadList => {
                reject_unknown_keys(arguments, &["list_title", "limit"])?;
                self.client
                    .read_list(
                        required_text(arguments, "list_title")?,
                        optional_positive_usize(arguments, "limit", 1_000, 1_000)?,
                    )
                    .await
            }
            SharePointToolKind::GetLists => {
                reject_unknown_keys(arguments, &[])?;
                self.client.get_lists().await
            }
            SharePointToolKind::GetListColumns => {
                reject_unknown_keys(arguments, &["list_title"])?;
                self.client
                    .get_list_columns(required_text(arguments, "list_title")?)
                    .await
            }
            SharePointToolKind::GetFilesList => {
                reject_unknown_keys(
                    arguments,
                    &[
                        "folder_name",
                        "form_name",
                        "limit_files",
                        "include_extensions",
                        "skip_extensions",
                    ],
                )?;
                let include = optional_pattern_list(arguments, "include_extensions")?;
                let skip = optional_pattern_list(arguments, "skip_extensions")?;
                let options = SharePointFileListOptions {
                    folder_name: optional_text(arguments, "folder_name")?.map(str::to_owned),
                    form_name: optional_text(arguments, "form_name")?.map(str::to_owned),
                    limit: optional_positive_usize(arguments, "limit_files", 100, MAX_FILES)?,
                    include_patterns: normalize_patterns(include.as_deref())
                        .map_err(map_client_error_without_requirement)?,
                    skip_patterns: normalize_patterns(skip.as_deref())
                        .map_err(map_client_error_without_requirement)?,
                };
                self.client.get_files_list(options).await
            }
            SharePointToolKind::OneNoteGetNotebooks => {
                reject_unknown_keys(arguments, &["select"])?;
                let select = optional_string_list(arguments, "select")?;
                self.client.onenote_get_notebooks(select.as_deref()).await
            }
            SharePointToolKind::OneNoteGetSections => {
                reject_unknown_keys(arguments, &["notebook_id", "select"])?;
                let select = optional_string_list(arguments, "select")?;
                self.client
                    .onenote_get_sections(
                        required_text(arguments, "notebook_id")?,
                        select.as_deref(),
                    )
                    .await
            }
            SharePointToolKind::OneNoteGetPages => {
                reject_unknown_keys(arguments, &["section_id", "limit", "select"])?;
                let select = optional_string_list(arguments, "select")?;
                self.client
                    .onenote_get_pages(
                        required_text(arguments, "section_id")?,
                        optional_positive_usize(arguments, "limit", 100, MAX_ONENOTE_PAGES)?,
                        select.as_deref(),
                    )
                    .await
            }
            SharePointToolKind::OneNoteGetPageContent => {
                reject_unknown_keys(arguments, &["page_id"])?;
                self.client
                    .onenote_get_page_content(required_text(arguments, "page_id")?)
                    .await
            }
        };
        result.map_err(|error| map_client_error(self.client.as_ref(), error))
    }
}

struct SharePointAuthorizationRequiredTool {
    kind: SharePointToolKind,
    requirement: DelegatedAuthorizationRequirement,
    description: Box<str>,
}

impl SharePointAuthorizationRequiredTool {
    fn new(
        kind: SharePointToolKind,
        toolkit_name: &str,
        site_url: &str,
        requirement: DelegatedAuthorizationRequirement,
    ) -> Self {
        Self {
            kind,
            requirement,
            description: bounded_description(kind, toolkit_name, site_url),
        }
    }
}

#[async_trait]
impl Tool for SharePointAuthorizationRequiredTool {
    fn name(&self) -> &str {
        self.kind.name()
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
        Some(schema_for(self.kind))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        Err(delegated_authorization_error(&self.requirement))
    }
}

fn schema_for(kind: SharePointToolKind) -> Value {
    let (properties, required) = match kind {
        SharePointToolKind::ReadList => (
            json!({
                "list_title":{"type":"string","minLength":1,"maxLength":8192,"description":"Name of the SharePoint list to read."},
                "limit":{"type":["integer","null"],"minimum":1,"maximum":1000,"default":1000,"description":"Maximum number of item-field objects to return."}
            }),
            json!(["list_title"]),
        ),
        SharePointToolKind::GetLists => (json!({}), json!([])),
        SharePointToolKind::GetListColumns => (
            json!({
                "list_title":{"type":"string","minLength":1,"maxLength":8192,"description":"Title of the SharePoint list whose columns should be returned."}
            }),
            json!(["list_title"]),
        ),
        SharePointToolKind::GetFilesList => (
            json!({
                "folder_name":{"type":["string","null"],"minLength":1,"maxLength":8192,"default":null,"description":"Optional drive-relative or library-prefixed subfolder path."},
                "form_name":{"type":["string","null"],"minLength":1,"maxLength":8192,"default":null,"description":"Optional exact document-library name."},
                "limit_files":{"type":["integer","null"],"minimum":1,"maximum":1000,"default":100,"description":"Maximum number of file metadata objects to return."},
                "include_extensions":{"type":["array","null"],"items":{"type":"string","minLength":1,"maxLength":1024},"maxItems":128,"default":null,"description":"Optional case-insensitive extensions, filenames, or star patterns to include."},
                "skip_extensions":{"type":["array","null"],"items":{"type":"string","minLength":1,"maxLength":1024},"maxItems":128,"default":null,"description":"Optional case-insensitive extensions, filenames, or star patterns to exclude."}
            }),
            json!([]),
        ),
        SharePointToolKind::OneNoteGetNotebooks => (select_schema(), json!([])),
        SharePointToolKind::OneNoteGetSections => (
            merge_schema_properties(
                select_schema(),
                "notebook_id",
                "The exact notebook ID whose sections should be listed.",
            ),
            json!(["notebook_id"]),
        ),
        SharePointToolKind::OneNoteGetPages => {
            let mut properties = merge_schema_properties(
                select_schema(),
                "section_id",
                "The exact section ID whose pages should be listed.",
            );
            properties["limit"] = json!({
                "type":["integer","null"],"minimum":1,"maximum":100,"default":100,
                "description":"Maximum number of page metadata objects to return."
            });
            (properties, json!(["section_id"]))
        }
        SharePointToolKind::OneNoteGetPageContent => (
            json!({
                "page_id":{"type":"string","minLength":1,"maxLength":8192,"description":"The exact OneNote page ID whose raw XHTML should be returned."}
            }),
            json!(["page_id"]),
        ),
    };
    json!({
        "type":"object",
        "properties":properties,
        "required":required,
        "additionalProperties":false
    })
}

fn select_schema() -> Value {
    json!({
        "select":{
            "type":["array","null"],
            "items":{"type":"string","minLength":1,"maxLength":1024},
            "maxItems":128,
            "default":null,
            "description":"Optional Graph fields. Omit for SDK defaults; pass an empty list to omit $select."
        }
    })
}

fn merge_schema_properties(mut properties: Value, name: &str, description: &str) -> Value {
    properties[name] = json!({
        "type":"string",
        "minLength":1,
        "maxLength":8192,
        "description":description
    });
    properties
}

fn bounded_description(kind: SharePointToolKind, toolkit_name: &str, site_url: &str) -> Box<str> {
    let description = format!(
        "SharePoint {site_url}\n{}\nToolkit: {toolkit_name}",
        kind.description()
    );
    if description.len() <= MAX_DESCRIPTION_BYTES {
        return description.into_boxed_str();
    }
    let boundary = description
        .char_indices()
        .map(|(index, _)| index)
        .take_while(|index| *index <= MAX_DESCRIPTION_BYTES)
        .last()
        .unwrap_or(0);
    description[..boundary].to_owned().into_boxed_str()
}

fn validate_argument_size(arguments: &Value) -> adk_rust::Result<()> {
    let bytes = serde_json::to_vec(arguments).map_err(|_| invalid_arguments())?;
    if bytes.len() > MAX_ARGUMENT_BYTES {
        return Err(resource_exhausted_arguments());
    }
    Ok(())
}

fn reject_unknown_keys(arguments: &Map<String, Value>, allowed: &[&str]) -> adk_rust::Result<()> {
    if arguments.keys().any(|key| !allowed.contains(&key.as_str())) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn required_text<'a>(arguments: &'a Map<String, Value>, name: &str) -> adk_rust::Result<&'a str> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .filter(|value| {
            !value.trim().is_empty()
                && value.len() <= MAX_TEXT_BYTES
                && !value.chars().any(char::is_control)
        })
        .ok_or_else(invalid_arguments)
}

fn optional_text<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
) -> adk_rust::Result<Option<&'a str>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value))
            if !value.trim().is_empty()
                && value.len() <= MAX_TEXT_BYTES
                && !value.chars().any(char::is_control) =>
        {
            Ok(Some(value))
        }
        Some(_) => Err(invalid_arguments()),
    }
}

fn optional_positive_usize(
    arguments: &Map<String, Value>,
    name: &str,
    default: usize,
    maximum: usize,
) -> adk_rust::Result<usize> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(default),
        Some(Value::Number(value)) => value
            .as_u64()
            .and_then(|value| usize::try_from(value).ok())
            .filter(|value| *value > 0 && *value <= maximum)
            .ok_or_else(invalid_arguments),
        Some(_) => Err(invalid_arguments()),
    }
}

fn optional_string_list(
    arguments: &Map<String, Value>,
    name: &str,
) -> adk_rust::Result<Option<Vec<String>>> {
    let Some(value) = arguments.get(name) else {
        return Ok(None);
    };
    if value.is_null() {
        return Ok(None);
    }
    let values = value.as_array().ok_or_else(invalid_arguments)?;
    if values.len() > MAX_SELECT_FIELDS {
        return Err(resource_exhausted_arguments());
    }
    let mut output = Vec::with_capacity(values.len());
    for value in values {
        let value = value.as_str().filter(|value| {
            !value.is_empty()
                && value.len() <= 1_024
                && !value.chars().any(char::is_control)
                && value.bytes().all(|byte| {
                    byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.' | b'@' | b'*')
                })
        });
        let value = value.ok_or_else(invalid_arguments)?;
        output.push(value.to_owned());
    }
    Ok(Some(output))
}

fn optional_pattern_list(
    arguments: &Map<String, Value>,
    name: &str,
) -> adk_rust::Result<Option<Vec<String>>> {
    let Some(value) = arguments.get(name) else {
        return Ok(None);
    };
    if value.is_null() {
        return Ok(None);
    }
    let values = value.as_array().ok_or_else(invalid_arguments)?;
    if values.len() > MAX_SELECT_FIELDS {
        return Err(resource_exhausted_arguments());
    }
    let mut output = Vec::with_capacity(values.len());
    for value in values {
        let value = value.as_str().filter(|value| {
            !value.trim().is_empty()
                && value.len() <= 1_024
                && !value.contains(['/', '\\'])
                && !value.chars().any(char::is_control)
        });
        output.push(value.ok_or_else(invalid_arguments)?.to_owned());
    }
    Ok(Some(output))
}

fn map_client_error(client: &dyn SharePointApi, error: SharePointClientError) -> AdkError {
    if error.code() == SharePointClientErrorCode::Authentication {
        delegated_authorization_error(client.authorization())
    } else {
        error.into_adk()
    }
}

fn map_client_error_without_requirement(error: SharePointClientError) -> AdkError {
    error.into_adk()
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "sharepoint.arguments.invalid",
        "the SharePoint tool arguments are invalid",
    )
}

fn resource_exhausted_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "sharepoint.arguments.resource_exhausted",
        "the SharePoint tool arguments exceed their approved limit",
    )
}

const fn unsupported_selection() -> SharePointToolsetError {
    SharePointToolsetError {
        code: SharePointToolsetErrorCode::UnsupportedSelection,
    }
}

const fn invalid_definition() -> SharePointToolsetError {
    SharePointToolsetError {
        code: SharePointToolsetErrorCode::InvalidDefinition,
    }
}
