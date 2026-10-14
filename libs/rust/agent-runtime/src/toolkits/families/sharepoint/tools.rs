use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::delegated_auth::{
    DelegatedAuthorizationCatalog, DelegatedAuthorizationRequirement, delegated_authorization_error,
};
use crate::toolkits::families::integer_like::integer_like;
use crate::toolkits::families::office_host::{
    MAX_OFFICE_OBJECT_BYTES, OfficeFailure, OfficeHostError, SharedOfficeToolHost,
};
use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::{ToolAdmissionDecision, ToolAdmissionPolicy};

use super::client::{
    MAX_FILES, MAX_ONENOTE_PAGES, MAX_READ_BYTES, SharePointApi, SharePointClient,
    SharePointClientError, SharePointClientErrorCode, SharePointFileListOptions, SharePointUpload,
    normalize_patterns,
};
use super::config::{SharePointConfigError, SharePointConfigErrorCode, SharePointToolkitConfig};
use super::reader::{self, Failure, PageOptions};

const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
/// The most text `upload_file` takes as `filedata`, and the most markup a
/// `OneNote` page tool takes: the runtime refuses any single argument string
/// longer than 64 KiB, so a larger file goes up as an artifact (`filepath`).
const MAX_FILEDATA_BYTES: usize = 64 * 1_024;
const MAX_HTML_ARGUMENT_BYTES: usize = 64 * 1_024;
const MAX_FIELDS_BYTES: usize = 64 * 1_024;
const MAX_PATCH_COMMANDS: usize = 100;
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

/// Build the explicitly selected `SharePoint` toolset: lists, files, document
/// reads, uploads and the `OneNote` reads and edits, all as the signed-in
/// person (a delegated Graph token).
///
/// Empty selection and selections without a supported tool fail closed. A
/// mixed saved selection exposes only its explicitly selected supported
/// tools; the index tools are the index family's. `host` is the Office host
/// of the live claim: it reads artifacts for `upload_file` and turns
/// documents into text for the readers; without one those parts answer that
/// this runtime lends no such authority.
pub(crate) fn build_sharepoint_toolset(
    toolkit_name: &str,
    config: SharePointToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
    host: Option<&SharedOfficeToolHost>,
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
                    host.cloned(),
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
    host: Option<&SharedOfficeToolHost>,
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
                host.cloned(),
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
    CreateListItem,
    GetFilesList,
    ReadDocument,
    UploadFile,
    ReadFileFromSharingLink,
    OneNoteGetNotebooks,
    OneNoteGetSections,
    OneNoteGetPages,
    OneNoteGetPageContent,
    OneNoteReadPage,
    OneNoteReadPageItems,
    OneNoteCreateNotebook,
    OneNoteCreateSection,
    OneNoteCreatePage,
    OneNoteUpdatePage,
    OneNoteReplacePageContent,
    OneNoteDeletePage,
    OneNoteListAttachments,
    OneNoteReadAttachment,
}

impl SharePointToolKind {
    const ALL: [Self; 22] = [
        Self::ReadList,
        Self::GetLists,
        Self::GetListColumns,
        Self::CreateListItem,
        Self::GetFilesList,
        Self::ReadDocument,
        Self::UploadFile,
        Self::ReadFileFromSharingLink,
        Self::OneNoteGetNotebooks,
        Self::OneNoteGetSections,
        Self::OneNoteGetPages,
        Self::OneNoteGetPageContent,
        Self::OneNoteReadPage,
        Self::OneNoteReadPageItems,
        Self::OneNoteCreateNotebook,
        Self::OneNoteCreateSection,
        Self::OneNoteCreatePage,
        Self::OneNoteUpdatePage,
        Self::OneNoteReplacePageContent,
        Self::OneNoteDeletePage,
        Self::OneNoteListAttachments,
        Self::OneNoteReadAttachment,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::ReadList => "read_list",
            Self::GetLists => "get_lists",
            Self::GetListColumns => "get_list_columns",
            Self::CreateListItem => "create_list_item",
            Self::GetFilesList => "get_files_list",
            Self::ReadDocument => "read_document",
            Self::UploadFile => "upload_file",
            Self::ReadFileFromSharingLink => "read_file_from_sharing_link",
            Self::OneNoteGetNotebooks => "onenote_get_notebooks",
            Self::OneNoteGetSections => "onenote_get_sections",
            Self::OneNoteGetPages => "onenote_get_pages",
            Self::OneNoteGetPageContent => "onenote_get_page_content",
            Self::OneNoteReadPage => "onenote_read_page",
            Self::OneNoteReadPageItems => "onenote_read_page_items",
            Self::OneNoteCreateNotebook => "onenote_create_notebook",
            Self::OneNoteCreateSection => "onenote_create_section",
            Self::OneNoteCreatePage => "onenote_create_page",
            Self::OneNoteUpdatePage => "onenote_update_page",
            Self::OneNoteReplacePageContent => "onenote_replace_page_content",
            Self::OneNoteDeletePage => "onenote_delete_page",
            Self::OneNoteListAttachments => "onenote_list_attachments",
            Self::OneNoteReadAttachment => "onenote_read_attachment",
        }
    }

    /// The tools that change something: created once, never retried, never
    /// run by a direct (read-only) toolkit call.
    const fn is_effect(self) -> bool {
        matches!(
            self,
            Self::CreateListItem
                | Self::UploadFile
                | Self::OneNoteCreateNotebook
                | Self::OneNoteCreateSection
                | Self::OneNoteCreatePage
                | Self::OneNoteUpdatePage
                | Self::OneNoteReplacePageContent
                | Self::OneNoteDeletePage
        )
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
                "Return writable non-lookup column metadata for one SharePoint list. Use this before create_list_item to discover internal names, display names, simplified types, required flags, and choice values."
            }
            Self::CreateListItem => {
                "Create a new item in a SharePoint list. fields maps column internal names to values; call get_list_columns first for the writable columns, the required ones and the valid choice values (Title is usually required; a dateTime is ISO 8601, e.g. 2026-02-01T00:00:00Z). Returns {id, fields, webUrl}. Creates one item per call and is never retried; a lost answer is reported as an unknown outcome, so check the list before repeating it."
            }
            Self::GetFilesList => {
                "Recursively list bounded file metadata across the configured site's Graph drives. folder_name scopes a subfolder, form_name pins one document library, and include_extensions/skip_extensions accept case-insensitive extension, filename, or star patterns. No file content is downloaded."
            }
            Self::ReadDocument => {
                "Reads the file at path and returns its text. path is the Path get_files_list returned, a server-relative path including the library ('/sites/Site/Shared Documents/folder/file.docx'), or a path relative to a library. start_line/end_line (1-indexed, inclusive) read a slice; text over 200000 characters comes back as a content_too_large guide to retry with a range. Files over 20 MB, executables and images are refused; page_number, sheet_name and is_capture_image are not applied."
            }
            Self::UploadFile => {
                "Upload a file to a SharePoint document library: an artifact (filepath, /{bucket}/{filename}) or text (filedata with filename), into folder_path such as '/sites/MySite/Shared Documents/folder' (the library segment picks the drive). replace (default true) overwrites a file of that name; false refuses when it exists. Up to 50 MB. Returns {id, webUrl, path, size, mime_type}. Sent once and never retried; a lost answer is reported as an unknown outcome, so check the folder before repeating it."
            }
            Self::ReadFileFromSharingLink => {
                "Read a file from a SharePoint/OneDrive sharing link (https://company-my.sharepoint.com/:x:/p/user/... or https://company.sharepoint.com/:x:/s/site/...), resolved by Microsoft Graph as the signed-in person: the link must be shared with them or with anyone. Up to 20 MB. start_line/end_line read a slice; text over 200000 characters comes back as a guide to retry with a range. A link Graph refuses is not fetched directly."
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
            Self::OneNoteReadPage => {
                "Read a OneNote page as plain text: text blocks, image placeholders ([image: alt]) and attachment entries ([attachment: name] and its download address), split by '-----'. read_attachment_content=true also parses up to 20 attachments inline. Pictures are not described in this runtime."
            }
            Self::OneNoteReadPageItems => {
                "Read a OneNote page as an ordered list of items rendered as text, split by '-----': text blocks, image placeholders ([image: alt]) and attachments (name, download_url and, with read_attachment_content=true, the parsed content). Pictures are not described in this runtime."
            }
            Self::OneNoteCreateNotebook => {
                "Create a new OneNote notebook in this SharePoint site. Returns id, displayName, createdDateTime and webUrl. Sent once and never retried; a lost answer is reported as an unknown outcome, so list the notebooks before repeating it."
            }
            Self::OneNoteCreateSection => {
                "Create a new section in a OneNote notebook on this site. Returns id, displayName, createdDateTime and pagesUrl. Sent once and never retried; a lost answer is reported as an unknown outcome, so list the sections before repeating it."
            }
            Self::OneNoteCreatePage => {
                "Create a OneNote page in a section from raw HTML, which must be a valid HTML document containing a <title> tag. Returns id, title, createdDateTime, webUrl and contentUrl. Sent once and never retried; a lost answer is reported as an unknown outcome, so list the pages before repeating it."
            }
            Self::OneNoteUpdatePage => {
                "Update a OneNote page with Graph PATCH commands. Each command has target (a CSS selector, 'body', 'title' or an element id), action ('append', 'prepend', 'replace', 'insert' or 'delete'), content (HTML; not needed for delete) and optionally position ('after' or 'before', for insert). Sent once and never retried; a lost answer is reported as an unknown outcome, so read the page before repeating it."
            }
            Self::OneNoteReplacePageContent => {
                "Replace the entire body of a OneNote page with new HTML content (a fragment; no <html> or <head> wrapper). Sent once and never retried; a lost answer is reported as an unknown outcome, so read the page before repeating it."
            }
            Self::OneNoteDeletePage => {
                "Permanently delete a OneNote page on this site. This action is irreversible. Sent once and never retried; a lost answer is reported as an unknown outcome, so list the pages before repeating it."
            }
            Self::OneNoteListAttachments => {
                "List the file attachments of a OneNote page: name, resource_id and the download_url of each. Embedded images are not attachments."
            }
            Self::OneNoteReadAttachment => {
                "Download and parse one file attachment of a OneNote page, named exactly as onenote_list_attachments returned it (case-sensitive), and return its text (PDF, DOCX, XLSX, PPTX, text and code files). Up to 20 MB. Images are not described in this runtime."
            }
        }
    }
}

struct SharePointTool {
    kind: SharePointToolKind,
    client: Arc<dyn SharePointApi>,
    host: Option<SharedOfficeToolHost>,
    description: Box<str>,
}

impl SharePointTool {
    fn new(
        kind: SharePointToolKind,
        toolkit_name: &str,
        site_url: &str,
        client: Arc<dyn SharePointApi>,
        host: Option<SharedOfficeToolHost>,
    ) -> Self {
        Self {
            kind,
            client,
            host,
            description: bounded_description(kind, toolkit_name, site_url),
        }
    }

    /// A Graph failure of one of the original reads, as the runtime reports a
    /// tool error: a rejected token asks for authorization.
    fn graph_error(&self, error: SharePointClientError) -> AdkError {
        map_client_error(self.client.as_ref(), error)
    }

    /// The end of a tool that words its own failures.
    ///
    /// The runtime reduces every tool ERROR to a generic line for the model
    /// ("tool dependency is unavailable"), which would hide the one thing a
    /// change must say: whether it happened. These tools therefore answer a
    /// failure as text, one sentence per cause, the way the artifact and the
    /// Office families do. A rejected token is the exception: it is a typed
    /// error, so the run can ask for authorization and replay the call (a 401
    /// means nothing was applied).
    fn finish(
        &self,
        action: &'static str,
        outcome: Result<Value, Failure>,
    ) -> adk_core::Result<Value> {
        let sentence = match outcome {
            Ok(value) => return Ok(value),
            Err(Failure::Client(error)) => {
                if error.code() == SharePointClientErrorCode::Authentication {
                    return Err(delegated_authorization_error(self.client.authorization()));
                }
                error.sentence().to_owned()
            }
            Err(Failure::Tool(error)) => error.message,
        };
        Ok(Value::String(format!(
            "Could not {action}: {}.",
            sentence.trim_end_matches('.')
        )))
    }
}

#[async_trait]
#[allow(
    clippy::too_many_lines,
    reason = "one dispatch over the tools of the family; each arm is a call"
)]
impl Tool for SharePointTool {
    fn name(&self) -> &str {
        self.kind.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        !self.kind.is_effect()
    }

    fn is_concurrency_safe(&self) -> bool {
        !self.kind.is_effect()
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(schema_for(self.kind))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        validate_argument_size(&arguments)?;
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        match self.kind {
            SharePointToolKind::ReadList => {
                reject_unknown_keys(arguments, &["list_title", "limit"])?;
                self.client
                    .read_list(
                        required_text(arguments, "list_title")?,
                        optional_positive_usize(arguments, "limit", 1_000, 1_000)?,
                    )
                    .await
                    .map_err(|error| self.graph_error(error))
            }
            SharePointToolKind::GetLists => {
                reject_unknown_keys(arguments, &[])?;
                self.client
                    .get_lists()
                    .await
                    .map_err(|error| self.graph_error(error))
            }
            SharePointToolKind::GetListColumns => {
                reject_unknown_keys(arguments, &["list_title"])?;
                self.client
                    .get_list_columns(required_text(arguments, "list_title")?)
                    .await
                    .map_err(|error| self.graph_error(error))
            }
            SharePointToolKind::CreateListItem => self.create_list_item(arguments).await,
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
                self.client
                    .get_files_list(options)
                    .await
                    .map_err(|error| self.graph_error(error))
            }
            SharePointToolKind::ReadDocument => self.read_document(arguments).await,
            SharePointToolKind::UploadFile => self.upload_file(arguments).await,
            SharePointToolKind::ReadFileFromSharingLink => {
                self.read_file_from_sharing_link(arguments).await
            }
            SharePointToolKind::OneNoteGetNotebooks => {
                reject_unknown_keys(arguments, &["select"])?;
                let select = optional_string_list(arguments, "select")?;
                self.client
                    .onenote_get_notebooks(select.as_deref())
                    .await
                    .map_err(|error| self.graph_error(error))
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
                    .map_err(|error| self.graph_error(error))
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
                    .map_err(|error| self.graph_error(error))
            }
            SharePointToolKind::OneNoteGetPageContent => {
                reject_unknown_keys(arguments, &["page_id"])?;
                self.client
                    .onenote_get_page_content(required_text(arguments, "page_id")?)
                    .await
                    .map_err(|error| self.graph_error(error))
            }
            SharePointToolKind::OneNoteReadPage | SharePointToolKind::OneNoteReadPageItems => {
                self.read_page(arguments).await
            }
            SharePointToolKind::OneNoteCreateNotebook => {
                reject_unknown_keys(arguments, &["display_name"])?;
                let name = required_text(arguments, "display_name")?.trim();
                let outcome = self
                    .client
                    .onenote_create_notebook(name)
                    .await
                    .map_err(Failure::from);
                self.finish("create the notebook", outcome)
            }
            SharePointToolKind::OneNoteCreateSection => {
                reject_unknown_keys(arguments, &["notebook_id", "display_name"])?;
                let notebook = required_text(arguments, "notebook_id")?;
                let name = required_text(arguments, "display_name")?.trim();
                let outcome = self
                    .client
                    .onenote_create_section(notebook, name)
                    .await
                    .map_err(Failure::from);
                self.finish("create the section", outcome)
            }
            SharePointToolKind::OneNoteCreatePage => {
                reject_unknown_keys(arguments, &["section_id", "html_content"])?;
                let section = required_text(arguments, "section_id")?;
                let html = required_markup(arguments, "html_content", true)?;
                let outcome = self
                    .client
                    .onenote_create_page(section, html)
                    .await
                    .map_err(Failure::from);
                self.finish("create the page", outcome)
            }
            SharePointToolKind::OneNoteUpdatePage => {
                reject_unknown_keys(arguments, &["page_id", "patch_commands"])?;
                let page = required_text(arguments, "page_id")?;
                let commands = patch_commands(arguments)?;
                let outcome = self
                    .client
                    .onenote_update_page(page, commands)
                    .await
                    .map(|()| Value::String(updated_message(page, commands.len())))
                    .map_err(Failure::from);
                self.finish("update the page", outcome)
            }
            SharePointToolKind::OneNoteReplacePageContent => {
                reject_unknown_keys(arguments, &["page_id", "html_content"])?;
                let page = required_text(arguments, "page_id")?;
                let html = required_markup(arguments, "html_content", false)?;
                let commands = [json!({"target": "body", "action": "replace", "content": html})];
                let outcome = self
                    .client
                    .onenote_update_page(page, &commands)
                    .await
                    .map(|()| Value::String(updated_message(page, 1)))
                    .map_err(Failure::from);
                self.finish("replace the page content", outcome)
            }
            SharePointToolKind::OneNoteDeletePage => {
                reject_unknown_keys(arguments, &["page_id"])?;
                let page = required_text(arguments, "page_id")?;
                let outcome = self
                    .client
                    .onenote_delete_page(page)
                    .await
                    .map(|()| Value::String(format!("OneNote page '{page}' deleted successfully.")))
                    .map_err(Failure::from);
                self.finish("delete the page", outcome)
            }
            SharePointToolKind::OneNoteListAttachments => {
                reject_unknown_keys(arguments, &["page_id"])?;
                let page = required_text(arguments, "page_id")?;
                let outcome = reader::list_attachments(self.client.as_ref(), page)
                    .await
                    .map_err(Failure::from);
                self.finish("list the attachments", outcome)
            }
            SharePointToolKind::OneNoteReadAttachment => {
                reject_unknown_keys(arguments, &["page_id", "attachment_name", "capture_images"])?;
                let page = required_text(arguments, "page_id")?;
                let name = required_text(arguments, "attachment_name")?;
                lax_bool(arguments, "capture_images", true)?;
                let outcome = reader::read_attachment(&self.client, self.host.as_ref(), page, name)
                    .await
                    .map(Value::String);
                self.finish("read the attachment", outcome)
            }
        }
    }
}

impl SharePointTool {
    async fn create_list_item(&self, arguments: &Map<String, Value>) -> adk_core::Result<Value> {
        reject_unknown_keys(arguments, &["list_title", "fields"])?;
        let list_title = required_text(arguments, "list_title")?;
        let fields = arguments
            .get("fields")
            .and_then(Value::as_object)
            .filter(|fields| {
                !fields.is_empty()
                    && fields.keys().all(|key| {
                        !key.is_empty() && key.len() <= 1_024 && !key.chars().any(char::is_control)
                    })
                    && serde_json::to_vec(fields).is_ok_and(|bytes| bytes.len() <= MAX_FIELDS_BYTES)
            })
            .ok_or_else(invalid_arguments)?;
        let outcome = self
            .client
            .create_list_item(list_title, fields)
            .await
            .map_err(Failure::from);
        self.finish("create the list item", outcome)
    }

    async fn read_document(&self, arguments: &Map<String, Value>) -> adk_core::Result<Value> {
        reject_unknown_keys(
            arguments,
            &[
                "path",
                "is_capture_image",
                "page_number",
                "sheet_name",
                "start_line",
                "end_line",
            ],
        )?;
        let path = required_text(arguments, "path")?;
        let mut unapplied = Vec::new();
        if optional_integer(arguments, "page_number")?.is_some() {
            unapplied.push("page_number");
        }
        if optional_text(arguments, "sheet_name")?.is_some() {
            unapplied.push("sheet_name");
        }
        if lax_bool(arguments, "is_capture_image", false)? {
            unapplied.push("is_capture_image");
        }
        let start = optional_line(arguments, "start_line")?;
        let end = optional_line(arguments, "end_line")?;
        let outcome: Result<Value, Failure> = async {
            if reader::is_blocked_binary(path) {
                return Err(Failure::Tool(AdkError::new(
                    ErrorComponent::Tool,
                    ErrorCategory::InvalidInput,
                    "sharepoint.file.type_blocked",
                    reader::UNSUPPORTED_BLOCKED,
                )));
            }
            // The reader is checked before a byte is downloaded.
            let host = reader::reader_host(self.host.as_ref())?;
            reader::check_readable(host, reader::file_name_of(path))?;
            let file = self.client.download_document(path, MAX_READ_BYTES).await?;
            let text = reader::extract(host, &file.name, file.bytes).await?;
            Ok(with_notice(
                reader::bound_text(&text, path, start, end)?,
                &unapplied,
            ))
        }
        .await;
        self.finish("read the document", outcome)
    }

    async fn read_file_from_sharing_link(
        &self,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        reject_unknown_keys(
            arguments,
            &["sharing_url", "is_capture_image", "start_line", "end_line"],
        )?;
        let url = required_text(arguments, "sharing_url")?;
        let mut unapplied = Vec::new();
        if lax_bool(arguments, "is_capture_image", false)? {
            unapplied.push("is_capture_image");
        }
        let start = optional_line(arguments, "start_line")?;
        let end = optional_line(arguments, "end_line")?;
        let outcome: Result<Value, Failure> = async {
            let host = reader::reader_host(self.host.as_ref())?;
            let host_for_check = self.host.clone();
            let readable =
                move |name: &str| reader::unreadable_reason(host_for_check.as_ref(), name);
            let file = self.client.download_shared_file(url, &readable).await?;
            let name = file.name.clone();
            let text = reader::extract(host, &file.name, file.bytes).await?;
            Ok(with_notice(
                reader::bound_text(&text, &name, start, end)?,
                &unapplied,
            ))
        }
        .await;
        self.finish("read the file behind the sharing link", outcome)
    }

    async fn upload_file(&self, arguments: &Map<String, Value>) -> adk_core::Result<Value> {
        reject_unknown_keys(
            arguments,
            &["folder_path", "filepath", "filedata", "filename", "replace"],
        )?;
        let folder_path = required_text(arguments, "folder_path")?;
        let filepath = optional_text(arguments, "filepath")?;
        let filedata = optional_content(arguments, "filedata", MAX_FILEDATA_BYTES)?;
        let filename = optional_text(arguments, "filename")?;
        let replace = lax_bool(arguments, "replace", true)?;
        // The SDK treats an empty string as not given.
        let filedata = filedata.filter(|data| !data.is_empty());
        let outcome: Result<Value, Failure> = async {
            let (bytes, default_name) = match (filepath, filedata) {
                (None, None) => {
                    return Err(upload_refusal(
                        "sharepoint.upload.source_missing",
                        "Either filepath or filedata must be provided",
                    ));
                }
                (Some(_), Some(_)) => {
                    return Err(upload_refusal(
                        "sharepoint.upload.source_ambiguous",
                        "Cannot specify both filepath and filedata",
                    ));
                }
                (None, Some(_)) if filename.is_none() => {
                    return Err(upload_refusal(
                        "sharepoint.upload.name_missing",
                        "filename is required when using filedata",
                    ));
                }
                (None, Some(data)) => (data.as_bytes().to_vec(), None),
                (Some(path), None) => {
                    let (bucket, key) = artifact_location(path)?;
                    let host = self.host.as_ref().ok_or_else(|| {
                        Failure::Tool(AdkError::new(
                            ErrorComponent::Tool,
                            ErrorCategory::Unsupported,
                            "sharepoint.upload.no_artifacts",
                            "this runtime lends no artifact store, so a file cannot be uploaded from filepath; pass filedata with filename",
                        ))
                    })?;
                    let bytes = host
                        .read_object(bucket, key, MAX_OFFICE_OBJECT_BYTES)
                        .await
                        .map_err(|error| Failure::Tool(artifact_failure(&error)))?;
                    let name = key.rsplit('/').next().unwrap_or(key).to_owned();
                    (bytes, Some(name))
                }
            };
            let target_name = filename
                .map(str::to_owned)
                .or(default_name)
                .ok_or_else(|| {
                    upload_refusal(
                        "sharepoint.upload.name_missing",
                        "filename is required when the artifact has no file name",
                    )
                })?;
            Ok(self
                .client
                .upload_file(SharePointUpload {
                    folder_path: folder_path.to_owned(),
                    file_name: target_name,
                    bytes,
                    replace,
                })
                .await?)
        }
        .await;
        self.finish("upload the file", outcome)
    }

    async fn read_page(&self, arguments: &Map<String, Value>) -> adk_core::Result<Value> {
        reject_unknown_keys(
            arguments,
            &[
                "page_id",
                "capture_images",
                "include_attachments",
                "read_attachment_content",
            ],
        )?;
        let page_id = required_text(arguments, "page_id")?;
        lax_bool(arguments, "capture_images", true)?;
        let options = PageOptions {
            include_attachments: lax_bool(arguments, "include_attachments", true)?,
            read_attachment_content: lax_bool(arguments, "read_attachment_content", false)?,
        };
        let items_view = !matches!(self.kind, SharePointToolKind::OneNoteReadPage);
        let outcome: Result<Value, Failure> = async {
            let items =
                reader::page_items(self.client.as_ref(), self.host.as_ref(), page_id, options)
                    .await?;
            let rendered = if items_view {
                reader::render_items(&items)
            } else {
                reader::render_page(&items)
            };
            let mut notice = String::new();
            if options.read_attachment_content
                && options.include_attachments
                && self.host.is_none()
            {
                notice.push_str(
                    "[read_attachment_content not applied: this runtime lends no document reader]\n",
                );
            }
            Ok(reader::limit_page_text(format!("{notice}{rendered}")))
        }
        .await;
        self.finish("read the page", outcome)
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
        !self.kind.is_effect()
    }

    fn is_concurrency_safe(&self) -> bool {
        !self.kind.is_effect()
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(schema_for(self.kind))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_core::Result<Value> {
        Err(delegated_authorization_error(&self.requirement))
    }
}

fn updated_message(page_id: &str, commands: usize) -> String {
    format!("OneNote page '{page_id}' updated successfully with {commands} patch command(s).")
}

/// A notice before a text answer; a guidance object is returned as it is.
fn with_notice(result: Value, unapplied: &[&'static str]) -> Value {
    match (result, reader::unapplied_notice(unapplied)) {
        (Value::String(text), Some(notice)) => Value::String(format!("{notice}{text}")),
        (other, _) => other,
    }
}

#[allow(
    clippy::too_many_lines,
    reason = "one input schema per tool, side by side with the SDK's"
)]
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
        SharePointToolKind::CreateListItem => (
            json!({
                "list_title":{"type":"string","minLength":1,"maxLength":8192,"description":"Title of the SharePoint list to create an item in."},
                "fields":{"type":"object","additionalProperties":true,"minProperties":1,"description":"Dictionary of field name -> value pairs. Use get_list_columns first to discover available fields, required fields, and valid choice values. Title is typically required. A choice value must match one of the allowed choices exactly; a dateTime value is ISO 8601 (e.g. '2026-02-01T00:00:00Z'). At most 64 KiB."}
            }),
            json!(["list_title", "fields"]),
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
        SharePointToolKind::ReadDocument => (
            json!({
                "path":{"type":"string","minLength":1,"maxLength":8192,"description":"Path of the document to read: the Path get_files_list returned, a full server-relative path including the document-library name (e.g. '/sites/SiteName/Shared Documents/folder/file.docx'), or a path relative to a library (e.g. 'folder/file.docx'). The library segment is resolved automatically to the correct drive."},
                "is_capture_image":{"type":["boolean","null"],"default":false,"description":"Not applied in this runtime: pictures in the document are not recognized."},
                "page_number":{"type":["integer","null"],"default":null,"description":"Not applied in this runtime: the whole document's text is returned."},
                "sheet_name":{"type":["string","null"],"maxLength":8192,"default":null,"description":"Not applied in this runtime: the whole workbook's text is returned."},
                "start_line":{"type":["integer","null"],"minimum":1,"default":null,"description":"Starting line number (1-indexed, inclusive) for a partial read."},
                "end_line":{"type":["integer","null"],"minimum":1,"default":null,"description":"Ending line number (1-indexed, inclusive) for a partial read."}
            }),
            json!(["path"]),
        ),
        SharePointToolKind::UploadFile => (
            json!({
                "folder_path":{"type":"string","minLength":1,"maxLength":8192,"description":"Server-relative folder path for upload, including the document-library name: '/sites/MySite/Shared Documents/folder', '/teams/MyTeam/PrivateDocs/folder', or a library root. The document-library segment is resolved automatically to the correct drive."},
                "filepath":{"type":["string","null"],"minLength":1,"maxLength":8192,"default":null,"description":"File path in format /{bucket}/{filename} from artifact storage. Either filepath or filedata must be provided."},
                "filedata":{"type":["string","null"],"maxLength":MAX_FILEDATA_BYTES,"default":null,"description":"String content to upload as a file. Either filepath or filedata must be provided."},
                "filename":{"type":["string","null"],"minLength":1,"maxLength":8192,"default":null,"description":"Target filename. Required when using filedata, optional when using filepath (uses the artifact's name if not specified)."},
                "replace":{"type":["boolean","null"],"default":true,"description":"If True, overwrite an existing file. If False, refuse when the file already exists."}
            }),
            json!(["folder_path"]),
        ),
        SharePointToolKind::ReadFileFromSharingLink => (
            json!({
                "sharing_url":{"type":"string","minLength":1,"maxLength":8192,"description":"SharePoint or OneDrive sharing link URL. Accepts personal OneDrive links (company-my.sharepoint.com/:x:/...) and SharePoint site sharing links (company.sharepoint.com/:x:/s/...). The link must be shared as 'Anyone with the link' or with your account."},
                "is_capture_image":{"type":["boolean","null"],"default":false,"description":"Not applied in this runtime: pictures in the document are not recognized."},
                "start_line":{"type":["integer","null"],"minimum":1,"default":null,"description":"Starting line number (1-indexed, inclusive) for a partial read. Omit to read from the beginning."},
                "end_line":{"type":["integer","null"],"minimum":1,"default":null,"description":"Ending line number (1-indexed, inclusive) for a partial read. Omit to read to the end."}
            }),
            json!(["sharing_url"]),
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
        SharePointToolKind::OneNoteReadPage => (
            read_page_schema(
                "The ID of the OneNote page to read.",
                "If True, include attachment entries in the output (download URL + optional content). The SDK lists them whatever this is set to; it gates only the content.",
                "If True, download and parse each attached file (at most 20) and inline its text. Has no effect when include_attachments=False.",
            ),
            json!(["page_id"]),
        ),
        SharePointToolKind::OneNoteReadPageItems => (
            read_page_schema(
                "The ID of the OneNote page to read.",
                "If True, include attachment items in the returned list with their download URLs. The SDK lists them whatever this is set to; it gates only the content.",
                "If True, download and parse each attachment's content inline (at most 20). Has no effect when include_attachments=False.",
            ),
            json!(["page_id"]),
        ),
        SharePointToolKind::OneNoteCreateNotebook => (
            json!({
                "display_name":{"type":"string","minLength":1,"maxLength":8192,"description":"Display name for the new notebook."}
            }),
            json!(["display_name"]),
        ),
        SharePointToolKind::OneNoteCreateSection => (
            json!({
                "notebook_id":{"type":"string","minLength":1,"maxLength":8192,"description":"The ID of the notebook to create the section in."},
                "display_name":{"type":"string","minLength":1,"maxLength":8192,"description":"Display name for the new section."}
            }),
            json!(["notebook_id", "display_name"]),
        ),
        SharePointToolKind::OneNoteCreatePage => (
            json!({
                "section_id":{"type":"string","minLength":1,"maxLength":8192,"description":"The ID of the section to create the page in."},
                "html_content":{"type":"string","minLength":1,"maxLength":MAX_HTML_ARGUMENT_BYTES,"description":"Full HTML document for the page. Must include a <title> tag. Example: '<!DOCTYPE html><html><head><title>My Page</title></head><body><p>Hello World</p></body></html>'"}
            }),
            json!(["section_id", "html_content"]),
        ),
        SharePointToolKind::OneNoteUpdatePage => (
            json!({
                "page_id":{"type":"string","minLength":1,"maxLength":8192,"description":"The ID of the OneNote page to update."},
                "patch_commands":{"type":"array","minItems":1,"maxItems":MAX_PATCH_COMMANDS,"items":{"type":"object","additionalProperties":true},"description":"List of OneNote PATCH command objects. Each must have: 'target' (e.g. 'body', or a CSS selector such as '#div-id'), 'action' ('append', 'prepend', 'replace', 'insert', or 'delete'), 'content' (HTML string; not needed for 'delete'), 'position' (optional: 'after' or 'before', used with 'insert'). Example: [{\"target\": \"body\", \"action\": \"append\", \"content\": \"<p>New paragraph</p>\"}]"}
            }),
            json!(["page_id", "patch_commands"]),
        ),
        SharePointToolKind::OneNoteReplacePageContent => (
            json!({
                "page_id":{"type":"string","minLength":1,"maxLength":8192,"description":"The ID of the OneNote page whose body will be replaced."},
                "html_content":{"type":"string","minLength":1,"maxLength":MAX_HTML_ARGUMENT_BYTES,"description":"New HTML content to set as the entire page body. Plain HTML fragment (no <html>/<head> wrapper needed)."}
            }),
            json!(["page_id", "html_content"]),
        ),
        SharePointToolKind::OneNoteDeletePage => (
            json!({
                "page_id":{"type":"string","minLength":1,"maxLength":8192,"description":"The ID of the OneNote page to delete."}
            }),
            json!(["page_id"]),
        ),
        SharePointToolKind::OneNoteListAttachments => (
            json!({
                "page_id":{"type":"string","minLength":1,"maxLength":8192,"description":"The ID of the OneNote page whose file attachments should be listed."}
            }),
            json!(["page_id"]),
        ),
        SharePointToolKind::OneNoteReadAttachment => (
            json!({
                "page_id":{"type":"string","minLength":1,"maxLength":8192,"description":"The ID of the OneNote page containing the attachment."},
                "attachment_name":{"type":"string","minLength":1,"maxLength":8192,"description":"Filename of the attachment to read, exactly as returned by onenote_list_attachments (e.g. 'report.pdf'). Case-sensitive."},
                "capture_images":{"type":["boolean","null"],"default":true,"description":"Not applied in this runtime: images are not described."}
            }),
            json!(["page_id", "attachment_name"]),
        ),
    };
    json!({
        "type":"object",
        "properties":properties,
        "required":required,
        "additionalProperties":false
    })
}

fn read_page_schema(page: &str, include: &str, content: &str) -> Value {
    json!({
        "page_id":{"type":"string","minLength":1,"maxLength":8192,"description":page},
        "capture_images":{"type":["boolean","null"],"default":true,"description":"Not applied in this runtime: embedded images are shown as [image: <alt>] placeholders, as the SDK shows them without a vision model."},
        "include_attachments":{"type":["boolean","null"],"default":true,"description":include},
        "read_attachment_content":{"type":["boolean","null"],"default":false,"description":content}
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

fn validate_argument_size(arguments: &Value) -> adk_core::Result<()> {
    let bytes = serde_json::to_vec(arguments).map_err(|_| invalid_arguments())?;
    if bytes.len() > MAX_ARGUMENT_BYTES {
        return Err(resource_exhausted_arguments());
    }
    Ok(())
}

fn reject_unknown_keys(arguments: &Map<String, Value>, allowed: &[&str]) -> adk_core::Result<()> {
    if arguments.keys().any(|key| !allowed.contains(&key.as_str())) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn required_text<'a>(arguments: &'a Map<String, Value>, name: &str) -> adk_core::Result<&'a str> {
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
) -> adk_core::Result<Option<&'a str>> {
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

/// Text that may span lines (a file's content): any characters but the
/// control ones other than line breaks and tabs.
fn optional_content<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> adk_core::Result<Option<&'a str>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value))
            if value.len() <= limit
                && !value
                    .chars()
                    .any(|c| c.is_control() && !matches!(c, '\n' | '\r' | '\t')) =>
        {
            Ok(Some(value))
        }
        Some(Value::String(value)) if value.len() > limit => Err(resource_exhausted_arguments()),
        Some(_) => Err(invalid_arguments()),
    }
}

/// The markup of a `OneNote` page tool. `blank_is_empty` refuses a whitespace-only
/// body too (a new page needs a document); a replacement only needs some text.
fn required_markup<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
    blank_is_empty: bool,
) -> adk_core::Result<&'a str> {
    let value = optional_content(arguments, name, MAX_HTML_ARGUMENT_BYTES)?
        .filter(|value| !value.is_empty() && (!blank_is_empty || !value.trim().is_empty()))
        .ok_or_else(invalid_arguments)?;
    Ok(value)
}

fn patch_commands(arguments: &Map<String, Value>) -> adk_core::Result<&[Value]> {
    let commands = arguments
        .get("patch_commands")
        .and_then(Value::as_array)
        .filter(|commands| !commands.is_empty() && commands.len() <= MAX_PATCH_COMMANDS)
        .ok_or_else(invalid_arguments)?;
    for command in commands {
        let command = command.as_object().ok_or_else(invalid_arguments)?;
        let action_ok = command
            .get("action")
            .and_then(Value::as_str)
            .is_some_and(|action| {
                matches!(
                    action,
                    "append" | "prepend" | "replace" | "insert" | "delete"
                )
            });
        let target_ok = command
            .get("target")
            .and_then(Value::as_str)
            .is_some_and(|target| !target.is_empty() && target.len() <= MAX_TEXT_BYTES);
        let content_ok = match command.get("content") {
            None | Some(Value::Null) => true,
            Some(Value::String(content)) => content.len() <= MAX_HTML_ARGUMENT_BYTES,
            Some(_) => false,
        };
        let position_ok = match command.get("position") {
            None | Some(Value::Null) => true,
            Some(Value::String(position)) => matches!(position.as_str(), "after" | "before"),
            Some(_) => false,
        };
        if !(action_ok && target_ok && content_ok && position_ok) {
            return Err(invalid_arguments());
        }
    }
    Ok(commands)
}

fn optional_integer(arguments: &Map<String, Value>, name: &str) -> adk_core::Result<Option<i64>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(value) => integer_like(value).map(Some).ok_or_else(invalid_arguments),
    }
}

/// A 1-indexed line number.
fn optional_line(arguments: &Map<String, Value>, name: &str) -> adk_core::Result<Option<usize>> {
    match optional_integer(arguments, name)? {
        None => Ok(None),
        Some(line) => usize::try_from(line)
            .ok()
            .filter(|line| *line > 0)
            .map(Some)
            .ok_or_else(invalid_arguments),
    }
}

fn optional_positive_usize(
    arguments: &Map<String, Value>,
    name: &str,
    default: usize,
    maximum: usize,
) -> adk_core::Result<usize> {
    match optional_integer(arguments, name)? {
        None => Ok(default),
        Some(value) => usize::try_from(value)
            .ok()
            .filter(|value| *value > 0 && *value <= maximum)
            .ok_or_else(invalid_arguments),
    }
}

/// A boolean, as pydantic's lax mode reads one: `true`/`false`, the integers
/// 0 and 1, and the strings it accepts.
fn lax_bool(arguments: &Map<String, Value>, name: &str, default: bool) -> adk_core::Result<bool> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(default),
        Some(Value::Bool(value)) => Ok(*value),
        Some(number @ Value::Number(_)) => match integer_like(number) {
            Some(1) => Ok(true),
            Some(0) => Ok(false),
            _ => Err(invalid_arguments()),
        },
        Some(Value::String(text)) => match text.trim().to_ascii_lowercase().as_str() {
            "true" | "t" | "yes" | "y" | "on" | "1" => Ok(true),
            "false" | "f" | "no" | "n" | "off" | "0" => Ok(false),
            _ => Err(invalid_arguments()),
        },
        Some(_) => Err(invalid_arguments()),
    }
}

fn optional_string_list(
    arguments: &Map<String, Value>,
    name: &str,
) -> adk_core::Result<Option<Vec<String>>> {
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
) -> adk_core::Result<Option<Vec<String>>> {
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

/// `/{bucket}/{filename}` as the SDK reads an artifact path.
fn artifact_location(filepath: &str) -> Result<(&str, &str), Failure> {
    filepath
        .strip_prefix('/')
        .and_then(|rest| rest.split_once('/'))
        .filter(|(bucket, key)| !bucket.is_empty() && !key.is_empty())
        .ok_or_else(|| {
            upload_refusal(
                "sharepoint.upload.filepath_invalid",
                "filepath must have the format /{bucket}/{filename}",
            )
        })
}

fn upload_refusal(code: &'static str, message: &'static str) -> Failure {
    Failure::Tool(AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        code,
        message,
    ))
}

/// An artifact read failure, one sentence per cause.
fn artifact_failure(error: &OfficeHostError) -> AdkError {
    let (category, code, message) = match error.kind() {
        OfficeFailure::NotFound => (
            ErrorCategory::NotFound,
            "sharepoint.upload.artifact_missing",
            "the artifact named by filepath does not exist",
        ),
        OfficeFailure::AccessRefused => (
            ErrorCategory::Forbidden,
            "sharepoint.upload.artifact_refused",
            "the artifact store refused access to the file named by filepath",
        ),
        OfficeFailure::TooLarge => (
            ErrorCategory::InvalidInput,
            "sharepoint.upload.artifact_too_large",
            "the artifact is larger than the 50 MB this tool uploads",
        ),
        OfficeFailure::InvalidRequest | OfficeFailure::UnsupportedType => (
            ErrorCategory::InvalidInput,
            "sharepoint.upload.artifact_invalid",
            "the artifact store did not accept the bucket or file name in filepath",
        ),
        _ => (
            ErrorCategory::Unavailable,
            "sharepoint.upload.artifact_unreachable",
            "the artifact store could not be reached, so nothing was uploaded",
        ),
    };
    AdkError::new(ErrorComponent::Tool, category, code, message)
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
