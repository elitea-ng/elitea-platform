use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{ConfluenceApi, ConfluenceClient, ConfluenceClientError, ConfluenceOperation};
use super::config::{ConfluenceConfigError, ConfluenceConfigErrorCode, ConfluenceToolkitConfig};

const MAX_ARGUMENT_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 1_000;
const MAX_LIST_ITEMS: usize = 64;
const MAX_LIST_ITEM_BYTES: usize = 1_024 * 1_024;

/// SDK tools this runtime does not serve, and why (`SOURCE_PARITY.md`):
/// the image-description read needs a vision model call, the attachment
/// reader the SDK's content parser and an LLM, and the file upload verified
/// artifact bytes; the six index tools need the indexing plane.
const UNSERVED_TOOLS: [&str; 9] = [
    "get_page_with_image_descriptions",
    "get_page_attachments",
    "add_file_to_page",
    "index_data",
    "search_index",
    "stepback_search_index",
    "stepback_summary_index",
    "list_indexes",
    "remove_index",
];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ConfluenceToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedCapability,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

pub(crate) struct ConfluenceToolsetError {
    code: ConfluenceToolsetErrorCode,
}

impl ConfluenceToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> ConfluenceToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for ConfluenceToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ConfluenceToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ConfluenceToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ConfluenceToolsetErrorCode::InvalidConfiguration => {
                "the Confluence toolkit configuration is invalid"
            }
            ConfluenceToolsetErrorCode::ResourceExhausted => {
                "the Confluence toolkit configuration exceeds its approved limit"
            }
            ConfluenceToolsetErrorCode::UnsupportedCapability => {
                "the Confluence toolkit configuration requires a capability this runtime does not provide"
            }
            ConfluenceToolsetErrorCode::UnsupportedSelection => {
                "the selected Confluence tool profile is not supported"
            }
            ConfluenceToolsetErrorCode::Client => "the Confluence client could not be created",
            ConfluenceToolsetErrorCode::InvalidDefinition => {
                "the Confluence ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for ConfluenceToolsetError {}

impl From<ConfluenceConfigError> for ConfluenceToolsetError {
    fn from(source: ConfluenceConfigError) -> Self {
        Self {
            code: match source.code() {
                ConfluenceConfigErrorCode::InvalidConfiguration => {
                    ConfluenceToolsetErrorCode::InvalidConfiguration
                }
                ConfluenceConfigErrorCode::ResourceExhausted => {
                    ConfluenceToolsetErrorCode::ResourceExhausted
                }
                ConfluenceConfigErrorCode::UnsupportedCapability => {
                    ConfluenceToolsetErrorCode::UnsupportedCapability
                }
            },
        }
    }
}

impl From<ConfluenceClientError> for ConfluenceToolsetError {
    fn from(_: ConfluenceClientError) -> Self {
        Self {
            code: ConfluenceToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for ConfluenceToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: ConfluenceToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the Confluence family: the SDK's sixteen REST tools.
///
/// Empty selection means every tool this runtime serves. An explicit
/// selection keeps the served tools and drops the SDK tools listed in
/// [`UNSERVED_TOOLS`] with one warning; a name the SDK does not have, or a
/// selection with nothing served left, is an unsupported selection.
pub(crate) fn build_confluence_toolset(
    toolkit_name: &str,
    config: ConfluenceToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, ConfluenceToolsetError> {
    let kinds = served_selection(config.selected_tools())?;
    let omitted = config.selected_tools().len().saturating_sub(kinds.len());
    if !config.selected_tools().is_empty() && omitted > 0 {
        tracing::warn!(
            event = "agent_toolkit_tools_skipped",
            reason_code = "unsupported_tool_selection",
            toolkit_type = "confluence",
            toolkit_name,
            selected_count = config.selected_tools().len(),
            materialized_count = kinds.len(),
            omitted_count = omitted,
            "Confluence tools this runtime does not serve were omitted from the native toolset"
        );
    }
    let client: Arc<dyn ConfluenceApi> = Arc::new(ConfluenceClient::new(config)?);
    admit_kinds(toolkit_name, &kinds, policy, &client)
}

fn served_selection(
    selected: &[Box<str>],
) -> Result<Vec<ConfluenceToolKind>, ConfluenceToolsetError> {
    if selected.iter().any(|name| {
        !UNSERVED_TOOLS.contains(&name.as_ref())
            && !ConfluenceToolKind::ALL
                .iter()
                .any(|kind| kind.name() == name.as_ref())
    }) {
        return Err(unsupported_selection());
    }
    let kinds = ConfluenceToolKind::ALL
        .into_iter()
        .filter(|kind| {
            selected.is_empty() || selected.iter().any(|name| name.as_ref() == kind.name())
        })
        .collect::<Vec<_>>();
    if kinds.is_empty() {
        return Err(unsupported_selection());
    }
    Ok(kinds)
}

fn admit_kinds(
    toolkit_name: &str,
    kinds: &[ConfluenceToolKind],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn ConfluenceApi>,
) -> Result<BasicToolset, ConfluenceToolsetError> {
    let tools = kinds
        .iter()
        .map(|kind| {
            Arc::new(ConfluenceTool::new(*kind, toolkit_name, Arc::clone(client))) as Arc<dyn Tool>
        })
        .collect();
    admit_materialized_toolset(toolkit_name, "confluence", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[&str],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn ConfluenceApi>,
) -> Result<BasicToolset, ConfluenceToolsetError> {
    let selected = selected
        .iter()
        .map(|name| (*name).into())
        .collect::<Vec<Box<str>>>();
    admit_kinds(toolkit_name, &served_selection(&selected)?, policy, client)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_catalog() -> Vec<(&'static str, bool)> {
    ConfluenceToolKind::ALL
        .into_iter()
        .map(|kind| (kind.name(), kind.is_read_only()))
        .collect()
}

#[derive(Clone, Copy)]
enum ConfluenceToolKind {
    CreatePage,
    CreatePages,
    DeletePage,
    UpdatePageById,
    UpdatePageByTitle,
    UpdatePages,
    UpdateLabels,
    GetPageTree,
    GetPagesWithLabel,
    ListPagesWithLabel,
    ReadPageById,
    SearchPages,
    SearchByTitle,
    SiteSearch,
    ExecuteGenericConfluence,
    GetPageIdByTitle,
}

impl ConfluenceToolKind {
    /// The SDK's `get_available_tools` order, without the unserved tools.
    const ALL: [Self; 16] = [
        Self::CreatePage,
        Self::CreatePages,
        Self::DeletePage,
        Self::UpdatePageById,
        Self::UpdatePageByTitle,
        Self::UpdatePages,
        Self::UpdateLabels,
        Self::GetPageTree,
        Self::GetPagesWithLabel,
        Self::ListPagesWithLabel,
        Self::ReadPageById,
        Self::SearchPages,
        Self::SearchByTitle,
        Self::SiteSearch,
        Self::ExecuteGenericConfluence,
        Self::GetPageIdByTitle,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::CreatePage => "create_page",
            Self::CreatePages => "create_pages",
            Self::DeletePage => "delete_page",
            Self::UpdatePageById => "update_page_by_id",
            Self::UpdatePageByTitle => "update_page_by_title",
            Self::UpdatePages => "update_pages",
            Self::UpdateLabels => "update_labels",
            Self::GetPageTree => "get_page_tree",
            Self::GetPagesWithLabel => "get_pages_with_label",
            Self::ListPagesWithLabel => "list_pages_with_label",
            Self::ReadPageById => "read_page_by_id",
            Self::SearchPages => "search_pages",
            Self::SearchByTitle => "search_by_title",
            Self::SiteSearch => "site_search",
            Self::ExecuteGenericConfluence => "execute_generic_confluence",
            Self::GetPageIdByTitle => "get_page_id_by_title",
        }
    }

    const fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::GetPageTree
                | Self::GetPagesWithLabel
                | Self::ListPagesWithLabel
                | Self::ReadPageById
                | Self::SearchPages
                | Self::SearchByTitle
                | Self::SiteSearch
                | Self::GetPageIdByTitle
        )
    }

    const fn description(self) -> &'static str {
        match self {
            Self::CreatePage => {
                "Creates a page in the Confluence space. Represents content in html (storage) or wiki (wiki) formats. Page could be either published status='current' or a draft with status='draft'. Without parent_id the page goes under the space home page. The toolkit's default labels are added. This remote write can create duplicates: an unknown outcome must be reconciled, not retried automatically."
            }
            Self::CreatePages => {
                "Creates a batch of pages in the Confluence space from pages_info, a JSON list of {\"page title\": \"page content\"} objects, at most 50 pages. This remote write can create duplicates: an unknown outcome must be reconciled, not retried automatically."
            }
            Self::DeletePage => {
                "Deletes a page by its page_id, or by page_title in the toolkit's space. This destructive remote effect must be reconciled, not retried automatically, after an unknown outcome."
            }
            Self::UpdatePageById => {
                "Updates an existing Confluence page by id, replacing its content, title and/or labels; without new_body the page keeps its current content. The toolkit's default labels are added. This remote write must be reconciled, not retried automatically, after an unknown outcome."
            }
            Self::UpdatePageByTitle => {
                "Updates an existing Confluence page found by title in the toolkit's space, replacing its content, title and/or labels. This remote write must be reconciled, not retried automatically, after an unknown outcome."
            }
            Self::UpdatePages => {
                "Updates a batch of pages (at most 50): new_contents gives one body per page id, or a single body for all of them; new_labels replaces each page's labels. This remote write must be reconciled, not retried automatically, after an unknown outcome."
            }
            Self::UpdateLabels => {
                "Replaces the labels of a batch of pages (at most 50) with new_labels, keeping their content. This remote write must be reconciled, not retried automatically, after an unknown outcome."
            }
            Self::GetPageTree => {
                "Gets the page tree under a Confluence page: every descendant page id with its title and parent id, at most 1000 pages."
            }
            Self::GetPagesWithLabel => {
                "Gets pages with a specific label in the Confluence space, with their Markdown content, up to the toolkit's max_pages."
            }
            Self::ListPagesWithLabel => {
                "Lists the id and title of pages with a specific label in the Confluence space, up to the toolkit's max_pages."
            }
            Self::ReadPageById => {
                "Reads a page by its id in the Confluence space and returns its content as Markdown; if the id is not known but the title is, use get_page_id_by_title first. content_format overrides the body format: 'view' (default), 'storage' for macro-heavy pages, 'export_view', 'editor', 'anonymous', 'styled_view', or 'atlas_doc_format' (Cloud only, returned as ADF JSON)."
            }
            Self::SearchPages => {
                "Search pages in Confluence by query text in title or page content; returns each match's Markdown content, id, title and url, up to the toolkit's max_pages."
            }
            Self::SearchByTitle => {
                "Search pages in Confluence by query text in title; returns each match's Markdown content, id, title and url, up to the toolkit's max_pages."
            }
            Self::SiteSearch => {
                "Search for pages in Confluence using site search by query text; returns up to 10 matches with id, title, url and preview text."
            }
            Self::ExecuteGenericConfluence => {
                "Generic Confluence tool for the official Atlassian Confluence REST API: searching, creating, updating pages, etc. relative_url starts with '/rest/...' and carries no query string; params is a JSON object string sent as query parameters for GET and as the JSON body otherwise. Non-GET methods are remote writes that must be reconciled, not retried automatically, after an unknown outcome."
            }
            Self::GetPageIdByTitle => {
                "Provide the page id of a page or blogpost found by its exact title in the toolkit's space."
            }
        }
    }
}

struct ConfluenceTool {
    kind: ConfluenceToolKind,
    client: Arc<dyn ConfluenceApi>,
    description: Box<str>,
}

impl ConfluenceTool {
    fn new(kind: ConfluenceToolKind, toolkit_name: &str, client: Arc<dyn ConfluenceApi>) -> Self {
        let description = format!("Toolkit: {toolkit_name}\n{}", kind.description());
        Self {
            kind,
            client,
            description: description
                .chars()
                .take(MAX_DESCRIPTION_BYTES)
                .collect::<String>()
                .into_boxed_str(),
        }
    }
}

#[async_trait]
impl Tool for ConfluenceTool {
    fn name(&self) -> &str {
        self.kind.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        self.kind.is_read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        self.kind.is_read_only()
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
        reject_unknown_keys(arguments, &schema_for(self.kind))?;
        let operation = operation(self.kind, arguments)?;
        self.client
            .execute(operation)
            .await
            .map_err(ConfluenceClientError::into_adk)
    }
}

fn operation(
    kind: ConfluenceToolKind,
    arguments: &Map<String, Value>,
) -> adk_core::Result<ConfluenceOperation<'_>> {
    Ok(match kind {
        ConfluenceToolKind::CreatePage => ConfluenceOperation::CreatePage {
            title: required_text(arguments, "title")?,
            body: required_text(arguments, "body")?,
            status: optional_text(arguments, "status")?,
            space: optional_text(arguments, "space")?,
            parent_id: optional_text(arguments, "parent_id")?,
            representation: optional_text(arguments, "representation")?,
            label: optional_text(arguments, "label")?,
        },
        ConfluenceToolKind::CreatePages => ConfluenceOperation::CreatePages {
            pages_info: required_text(arguments, "pages_info")?,
            status: optional_text(arguments, "status")?,
            space: optional_text(arguments, "space")?,
            parent_id: optional_text(arguments, "parent_id")?,
        },
        ConfluenceToolKind::DeletePage => ConfluenceOperation::DeletePage {
            page_id: optional_text(arguments, "page_id")?,
            page_title: optional_text(arguments, "page_title")?,
        },
        ConfluenceToolKind::UpdatePageById => ConfluenceOperation::UpdatePageById {
            page_id: required_text(arguments, "page_id")?,
            representation: optional_text(arguments, "representation")?,
            new_title: optional_text(arguments, "new_title")?,
            new_body: optional_text(arguments, "new_body")?,
            new_labels: optional_list(arguments, "new_labels")?,
        },
        ConfluenceToolKind::UpdatePageByTitle => ConfluenceOperation::UpdatePageByTitle {
            page_title: required_text(arguments, "page_title")?,
            representation: optional_text(arguments, "representation")?,
            new_title: optional_text(arguments, "new_title")?,
            new_body: optional_text(arguments, "new_body")?,
            new_labels: optional_list(arguments, "new_labels")?,
        },
        ConfluenceToolKind::UpdatePages => ConfluenceOperation::UpdatePages {
            page_ids: optional_list(arguments, "page_ids")?,
            new_contents: optional_list(arguments, "new_contents")?,
            new_labels: optional_list(arguments, "new_labels")?,
        },
        ConfluenceToolKind::UpdateLabels => ConfluenceOperation::UpdateLabels {
            page_ids: optional_list(arguments, "page_ids")?,
            new_labels: optional_list(arguments, "new_labels")?,
        },
        ConfluenceToolKind::GetPageTree => ConfluenceOperation::GetPageTree {
            page_id: required_text(arguments, "page_id")?,
        },
        ConfluenceToolKind::GetPagesWithLabel => ConfluenceOperation::GetPagesWithLabel {
            label: required_text(arguments, "label")?,
        },
        ConfluenceToolKind::ListPagesWithLabel => ConfluenceOperation::ListPagesWithLabel {
            label: required_text(arguments, "label")?,
        },
        ConfluenceToolKind::ReadPageById => ConfluenceOperation::ReadPageById {
            page_id: required_text(arguments, "page_id")?,
            skip_images: optional_bool(arguments, "skip_images")?.unwrap_or(false),
            content_format: optional_text(arguments, "content_format")?,
        },
        ConfluenceToolKind::SearchPages => ConfluenceOperation::SearchPages {
            query: required_text(arguments, "query")?,
            skip_images: optional_bool(arguments, "skip_images")?.unwrap_or(false),
        },
        ConfluenceToolKind::SearchByTitle => ConfluenceOperation::SearchByTitle {
            query: required_text(arguments, "query")?,
            skip_images: optional_bool(arguments, "skip_images")?.unwrap_or(false),
        },
        ConfluenceToolKind::SiteSearch => ConfluenceOperation::SiteSearch {
            query: required_text(arguments, "query")?,
        },
        ConfluenceToolKind::ExecuteGenericConfluence => {
            ConfluenceOperation::ExecuteGenericConfluence {
                method: required_text(arguments, "method")?,
                relative_url: required_text(arguments, "relative_url")?,
                params: optional_text(arguments, "params")?,
            }
        }
        ConfluenceToolKind::GetPageIdByTitle => ConfluenceOperation::GetPageIdByTitle {
            title: required_text(arguments, "title")?,
            page_type: optional_text(arguments, "type")?,
        },
    })
}

#[allow(clippy::too_many_lines)] // Keeps the sixteen SDK argument schemas auditable in one place.
fn schema_for(kind: ConfluenceToolKind) -> Value {
    let skip_images = || {
        nullable(
            json!({"type":"boolean","description":"Whether to replace embedded base64 images with [Image Removed]."}),
            Value::Bool(false),
        )
    };
    let representation = || {
        nullable(
            json!({"type":"string","description":"Content representation format: storage for html, wiki for markdown."}),
            Value::String("storage".to_owned()),
        )
    };
    let status = || {
        nullable(
            json!({"type":"string","description":"Page publishing option: 'current' to publish the page, 'draft' to create a draft."}),
            Value::String("current".to_owned()),
        )
    };
    let (properties, required): (Vec<(&str, Value)>, &[&str]) = match kind {
        ConfluenceToolKind::CreatePage => (
            vec![
                (
                    "space",
                    optional_text_schema(
                        "Confluence space used for the page's creation; defaults to the toolkit's space.",
                    ),
                ),
                ("title", text("Title of the page")),
                ("body", text("Body of the page")),
                ("status", status()),
                (
                    "parent_id",
                    optional_text_schema("Page parent id (optional)"),
                ),
                ("representation", representation()),
                ("label", optional_text_schema("Page label (optional)")),
            ],
            &["title", "body"],
        ),
        ConfluenceToolKind::CreatePages => (
            vec![
                (
                    "space",
                    optional_text_schema(
                        "Confluence space used for the pages' creation; defaults to the toolkit's space.",
                    ),
                ),
                (
                    "pages_info",
                    text(
                        "JSON string with the pages' names and contents: [{\"page1_name\": \"page1_content\"}, {\"page2_name\": \"page2_content\"}]",
                    ),
                ),
                (
                    "parent_id",
                    optional_text_schema("Page parent id (optional)"),
                ),
                ("status", status()),
            ],
            &["pages_info"],
        ),
        ConfluenceToolKind::DeletePage => (
            vec![
                ("page_id", optional_text_schema("Page id")),
                ("page_title", optional_text_schema("Page title")),
            ],
            &[],
        ),
        ConfluenceToolKind::UpdatePageById => (
            vec![
                ("page_id", text("Page id")),
                ("representation", representation()),
                ("new_title", optional_text_schema("New page title")),
                ("new_body", optional_text_schema("New page content")),
                (
                    "new_labels",
                    list("Page labels; replaces the page's current labels"),
                ),
            ],
            &["page_id"],
        ),
        ConfluenceToolKind::UpdatePageByTitle => (
            vec![
                ("page_title", text("Page title")),
                ("representation", representation()),
                ("new_title", optional_text_schema("New page title")),
                ("new_body", optional_text_schema("New page content")),
                (
                    "new_labels",
                    list("Page labels; replaces the page's current labels"),
                ),
            ],
            &["page_title"],
        ),
        ConfluenceToolKind::UpdatePages => (
            vec![
                ("page_ids", list("List of ids of pages to be updated")),
                (
                    "new_contents",
                    list(
                        "List of new contents for each page. If the content is the same for all pages, a list with a single entry",
                    ),
                ),
                ("new_labels", list("Page labels")),
            ],
            &[],
        ),
        ConfluenceToolKind::UpdateLabels => (
            vec![
                ("page_ids", list("List of ids of pages to be updated")),
                ("new_labels", list("Page labels")),
            ],
            &[],
        ),
        ConfluenceToolKind::GetPageTree => (vec![("page_id", text("Page id"))], &["page_id"]),
        ConfluenceToolKind::GetPagesWithLabel | ConfluenceToolKind::ListPagesWithLabel => {
            (vec![("label", text("Label of the pages"))], &["label"])
        }
        ConfluenceToolKind::ReadPageById => (
            vec![
                ("page_id", text("Id of page to be read")),
                ("skip_images", skip_images()),
                (
                    "content_format",
                    nullable(
                        json!({"type":"string","enum":["view","storage","export_view","editor","anonymous","styled_view","atlas_doc_format"],"description":"Override the body format for this call; defaults to view."}),
                        Value::Null,
                    ),
                ),
            ],
            &["page_id"],
        ),
        ConfluenceToolKind::SearchPages | ConfluenceToolKind::SearchByTitle => (
            vec![
                (
                    "query",
                    json!({"type":"string","minLength":1,"description":"Query text to search pages"}),
                ),
                ("skip_images", skip_images()),
            ],
            &["query"],
        ),
        ConfluenceToolKind::SiteSearch => (
            vec![(
                "query",
                text("Query text to execute site search in Confluence"),
            )],
            &["query"],
        ),
        ConfluenceToolKind::ExecuteGenericConfluence => (
            vec![
                (
                    "method",
                    text("The HTTP method to use for the request (GET, POST, PUT, PATCH, DELETE)."),
                ),
                (
                    "relative_url",
                    text(
                        "The relative URI for the Confluence API. It must start with a forward slash and '/rest/...'. Do not include query parameters; provide them in 'params'.",
                    ),
                ),
                (
                    "params",
                    nullable(
                        json!({"type":"string","description":"Optional JSON object string of parameters, sent as query parameters for GET and as the JSON request body otherwise. For searches generate a CQL query and pass it here."}),
                        Value::String(String::new()),
                    ),
                ),
            ],
            &["method", "relative_url"],
        ),
        ConfluenceToolKind::GetPageIdByTitle => (
            vec![
                ("title", text("Page title")),
                (
                    "type",
                    nullable(
                        json!({"type":"string","description":"Type of content: page or blogpost. Defaults to page."}),
                        Value::String("page".to_owned()),
                    ),
                ),
            ],
            &["title"],
        ),
    };
    json!({
        "type": "object",
        "properties": properties
            .into_iter()
            .map(|(name, schema)| (name.to_owned(), schema))
            .collect::<Map<_, _>>(),
        "required": required,
        "additionalProperties": false,
    })
}

fn text(description: &str) -> Value {
    json!({"type":"string","description":description})
}

fn optional_text_schema(description: &str) -> Value {
    nullable(
        json!({"type":"string","description":description}),
        Value::Null,
    )
}

/// The SDK declares these as untyped lists; items are ids, labels or page
/// bodies, accepted as strings or numbers.
fn list(description: &str) -> Value {
    nullable(
        json!({"type":"array","items":{"type":["string","number"]},"maxItems":MAX_LIST_ITEMS,"description":description}),
        Value::Null,
    )
}

fn nullable(value: Value, default: Value) -> Value {
    Value::Object(Map::from_iter([
        (
            "anyOf".to_owned(),
            Value::Array(vec![value, json!({"type":"null"})]),
        ),
        ("default".to_owned(), default),
    ]))
}

fn validate_argument_size(arguments: &Value) -> adk_core::Result<()> {
    if serde_json::to_vec(arguments)
        .map_err(|_| invalid_arguments())?
        .len()
        > MAX_ARGUMENT_BYTES
    {
        return Err(AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::InvalidInput,
            "confluence.arguments.resource_exhausted",
            "the Confluence tool arguments exceed the approved limit",
        ));
    }
    Ok(())
}

fn reject_unknown_keys(arguments: &Map<String, Value>, schema: &Value) -> adk_core::Result<()> {
    let allowed = schema
        .get("properties")
        .and_then(Value::as_object)
        .ok_or_else(invalid_arguments)?;
    if arguments.keys().any(|key| !allowed.contains_key(key)) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn required_text<'a>(arguments: &'a Map<String, Value>, name: &str) -> adk_core::Result<&'a str> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .ok_or_else(invalid_arguments)
}

/// The SDK drops explicit nulls before dispatch, so null means "absent".
fn optional_text<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
) -> adk_core::Result<Option<&'a str>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => Ok(Some(value)),
        _ => Err(invalid_arguments()),
    }
}

fn optional_bool(arguments: &Map<String, Value>, name: &str) -> adk_core::Result<Option<bool>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Bool(value)) => Ok(Some(*value)),
        _ => Err(invalid_arguments()),
    }
}

fn optional_list(
    arguments: &Map<String, Value>,
    name: &str,
) -> adk_core::Result<Option<Vec<String>>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Array(values)) if values.len() <= MAX_LIST_ITEMS => values
            .iter()
            .map(|value| match value {
                Value::String(value) if value.len() <= MAX_LIST_ITEM_BYTES => Ok(value.clone()),
                Value::Number(value) => Ok(value.to_string()),
                _ => Err(invalid_arguments()),
            })
            .collect::<adk_core::Result<Vec<_>>>()
            .map(Some),
        _ => Err(invalid_arguments()),
    }
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "confluence.arguments.invalid",
        "the Confluence tool arguments are invalid",
    )
}

const fn unsupported_selection() -> ConfluenceToolsetError {
    ConfluenceToolsetError {
        code: ConfluenceToolsetErrorCode::UnsupportedSelection,
    }
}
