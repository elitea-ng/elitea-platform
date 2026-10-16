use std::sync::Arc;

use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value};

use crate::toolkits::families::ado::client::{AdoClientError, IntoAdk};
use crate::toolkits::families::ado::toolset::{
    AdoToolExecutor, AdoToolKind, AdoToolsetError, MAX_IDENTIFIER_BYTES, MAX_TEXT_BYTES,
    build_toolset, invalid_arguments, optional_bool, optional_i64, optional_str,
    optional_string_list, reject_unknown_keys, required_id, required_id_list, required_id_text,
    required_str, schema,
};
use crate::toolkits::families::attachment_host::AttachmentHost;
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::attach::{AttachFile, MAX_NAME_BYTES, MAX_PATH_BYTES, MAX_URL_BYTES};
use super::client::{AdoBoardsClient, GetComments, SearchWorkItems};
use super::config::AdoBoardsToolkitConfig;

pub(crate) const TOOLKIT_TYPE: &str = "ado_boards";

const CREATE_WI_FIELD: &str = "JSON of the work item fields to create in Azure DevOps, i.e.\n                    {\n                       \"fields\":{\n                          \"System.Title\":\"Implement Registration Form Validation\",\n                          \"field2\":\"Value 2\",\n                       }\n                    }\n                    ";

/// The `ado_boards` tools this runtime serves. `get_image_by_url` (the run's
/// chat model) and `attach_file_to_work_item` (the artifact store) are served
/// only when the run lends an [`AttachmentHost`]; the index tools need
/// indexing and are absent.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum AdoBoardsToolKind {
    SearchWorkItems,
    CreateWorkItem,
    UpdateWorkItem,
    DeleteWorkItem,
    GetWorkItem,
    LinkWorkItems,
    GetRelationTypes,
    GetComments,
    LinkWorkItemsToWikiPage,
    UnlinkWorkItemsFromWikiPage,
    GetWorkItemTypeFields,
    AttachFileToWorkItem,
    GetImageByUrl,
}

impl AdoBoardsToolKind {
    pub(crate) const ALL: [Self; 11] = [
        Self::SearchWorkItems,
        Self::CreateWorkItem,
        Self::UpdateWorkItem,
        Self::DeleteWorkItem,
        Self::GetWorkItem,
        Self::LinkWorkItems,
        Self::GetRelationTypes,
        Self::GetComments,
        Self::LinkWorkItemsToWikiPage,
        Self::UnlinkWorkItemsFromWikiPage,
        Self::GetWorkItemTypeFields,
    ];

    /// The tools that need the run's artifact store or chat model.
    pub(crate) const WITH_HOST: [Self; 2] = [Self::AttachFileToWorkItem, Self::GetImageByUrl];

    /// The tools served: always [`Self::ALL`], plus [`Self::WITH_HOST`] when
    /// the run lends an [`AttachmentHost`].
    pub(crate) fn served(has_host: bool) -> Vec<Self> {
        let mut kinds = Self::ALL.to_vec();
        if has_host {
            kinds.extend(Self::WITH_HOST);
        }
        kinds
    }
}

impl AdoToolKind for AdoBoardsToolKind {
    fn name(self) -> &'static str {
        match self {
            Self::SearchWorkItems => "search_work_items",
            Self::CreateWorkItem => "create_work_item",
            Self::UpdateWorkItem => "update_work_item",
            Self::DeleteWorkItem => "delete_work_item",
            Self::GetWorkItem => "get_work_item",
            Self::LinkWorkItems => "link_work_items",
            Self::GetRelationTypes => "get_relation_types",
            Self::GetComments => "get_comments",
            Self::LinkWorkItemsToWikiPage => "link_work_items_to_wiki_page",
            Self::UnlinkWorkItemsFromWikiPage => "unlink_work_items_from_wiki_page",
            Self::GetWorkItemTypeFields => "get_work_item_type_fields",
            Self::AttachFileToWorkItem => "attach_file_to_work_item",
            Self::GetImageByUrl => "get_image_by_url",
        }
    }

    fn description(self) -> &'static str {
        match self {
            Self::SearchWorkItems => {
                "Search for work items using a WIQL query and dynamically fetch fields based on the query. Returns id, the work item URL and each requested field (\"N/A\" when absent); without fields: Title, State, AssignedTo, CreatedDate and ChangedDate."
            }
            Self::CreateWorkItem => {
                "Create a work item in Azure DevOps from work_item_json {\"fields\": {...}} using field reference names. Use get_work_item_type_fields to discover required fields. Creation is not safe to retry after an unknown outcome."
            }
            Self::UpdateWorkItem => {
                "Updates existing work item per defined data: every entry of work_item_json.fields is set on the work item."
            }
            Self::DeleteWorkItem => {
                "Delete a work item from Azure DevOps by ID. The item moves to the recycle bin."
            }
            Self::GetWorkItem => {
                "Get a single work item by ID with all fields, or only the listed fields. expand Relations or All also returns the work item's relations. Image and attachment description is not available in this runtime; parse_attachments returns the work item without attachment content."
            }
            Self::LinkWorkItems => {
                "Add the relation to the source work item with an appropriate attributes if any. User may pass attributes like name, etc. link_type must be a relation reference name from get_relation_types."
            }
            Self::GetRelationTypes => {
                "Returns dict of possible relation types per syntax: 'relation name': 'relation reference name'. NOTE: reference name is used for adding links to the work item"
            }
            Self::GetComments => {
                "Get comments for work item by ID, up to limit_total (default: the toolkit limit). Image description is not available in this runtime; process_images returns the comments with their image references unchanged."
            }
            Self::LinkWorkItemsToWikiPage => {
                "Links one or more work items to a specific wiki page using an ArtifactLink."
            }
            Self::UnlinkWorkItemsFromWikiPage => {
                "Unlinks one or more work items from a specific wiki page by removing the ArtifactLink."
            }
            Self::GetWorkItemTypeFields => {
                "Get formatted information about available fields for a specific work item type. This method helps discover which fields are required for work item creation."
            }
            Self::AttachFileToWorkItem => {
                "Attach a file from artifact storage (filepath /{bucket}/{filename}; at most 50 MiB in this runtime) to an Azure DevOps work item: uploads it, then adds an AttachedFile relation, optionally appending an <img>/<a> reference to an HTML field (inline_field, existing content is kept) and/or a work item comment (add_as_comment). Attaching the same file name again adds a second attachment. If the upload works but linking fails the reply says the file is uploaded but NOT linked."
            }
            Self::GetImageByUrl => {
                "Analyze an image attached to an Azure DevOps work item (the src of an <img> in work item fields or comments) with the run's vision model and return what it shows. The image is downloaded by its attachment id through the configured connection (png, jpg, jpeg, gif, webp; at most 5 MB)."
            }
        }
    }

    #[allow(clippy::too_many_lines)] // One schema per SDK tool keeps the catalogue auditable.
    fn schema(self) -> Value {
        match self {
            Self::AttachFileToWorkItem => schema::object(
                &[
                    (
                        "work_item_id",
                        schema::integer("ID of the work item to attach the file to"),
                    ),
                    (
                        "filepath",
                        schema::string(
                            "File path in format /{bucket}/{filename} pointing to the artifact to attach. Any file type is supported (image, PDF, document, etc.). Get this from a file/image generation or upload tool response.",
                        ),
                    ),
                    (
                        "filename",
                        schema::optional_string(
                            "Filename to use for the ADO attachment, e.g. 'diagram.png'. If not provided, uses the original filename from the artifact. Should include file extension.",
                            None,
                        ),
                    ),
                    (
                        "inline_field",
                        schema::optional_string(
                            "Optional HTML-typed work item field reference name (e.g. 'System.Description', 'Microsoft.VSTS.TCM.ReproSteps'). If provided, an <img> tag (for images) or <a> link (for other file types) is appended to that field's current value so the attachment renders inline. Requires the field to accept HTML.",
                            None,
                        ),
                    ),
                    (
                        "add_as_comment",
                        schema::optional_bool(
                            "If True, also add a work item comment containing the inline image/link reference. Default is False.",
                            false,
                        ),
                    ),
                    (
                        "comment",
                        schema::optional_string(
                            "Optional 'comment' attribute stored on the AttachedFile relation itself (a short caption/description of the attachment).",
                            None,
                        ),
                    ),
                ],
                &["work_item_id", "filepath"],
            ),
            Self::GetImageByUrl => schema::object(
                &[
                    (
                        "attachment_url",
                        schema::string(
                            "Full Azure DevOps work item attachment URL, e.g. https://dev.azure.com/{org}/{project}/_apis/wit/attachments/{guid}?fileName=screen.png \u{2014} typically the src of an <img> tag or markdown image found in work item fields or comments. Both dev.azure.com and {org}.visualstudio.com URL forms are accepted.",
                        ),
                    ),
                    (
                        "file_name",
                        schema::optional_string(
                            "File name with extension (e.g. 'screen.png'). Overrides or supplies the fileName query parameter; required when the URL does not contain one.",
                            None,
                        ),
                    ),
                    (
                        "prompt",
                        schema::optional_string(
                            "Custom instruction for the image analysis. Defaults to the standard detailed image description prompt.",
                            None,
                        ),
                    ),
                ],
                &["attachment_url"],
            ),
            Self::SearchWorkItems => schema::object(
                &[
                    (
                        "query",
                        schema::string("WIQL query for searching Azure DevOps work items"),
                    ),
                    (
                        "limit",
                        schema::optional_integer(
                            "Number of items to return. IMPORTANT: Tool returns all items if limit=-1. If parameter is not provided then the value will be taken from tool configuration.",
                        ),
                    ),
                    (
                        "fields",
                        schema::optional_string_array("Comma-separated list of requested fields"),
                    ),
                ],
                &["query"],
            ),
            Self::CreateWorkItem => schema::object(
                &[
                    ("work_item_json", schema::string(CREATE_WI_FIELD)),
                    (
                        "wi_type",
                        schema::optional_string(
                            "Work item type, e.g. 'Task', 'Issue' or  'EPIC'",
                            Some("Task"),
                        ),
                    ),
                ],
                &["work_item_json"],
            ),
            Self::UpdateWorkItem => schema::object(
                &[
                    (
                        "id",
                        schema::string("ID of work item required to be updated"),
                    ),
                    ("work_item_json", schema::string(CREATE_WI_FIELD)),
                ],
                &["id", "work_item_json"],
            ),
            Self::DeleteWorkItem => schema::object(
                &[("id", schema::integer("ID of work item to be deleted"))],
                &["id"],
            ),
            Self::GetWorkItem => schema::object(
                &[
                    ("id", schema::integer("The work item id")),
                    (
                        "fields",
                        schema::optional_string_array("Comma-separated list of requested fields"),
                    ),
                    (
                        "as_of",
                        schema::optional_string("AsOf UTC date time string", None),
                    ),
                    (
                        "expand",
                        schema::optional_string(
                            "The expand parameters for work item attributes. Possible options are { None, Relations, Fields, Links, All }.",
                            None,
                        ),
                    ),
                    (
                        "parse_attachments",
                        schema::optional_bool(
                            "Value that defines is attachment should be parsed.",
                            false,
                        ),
                    ),
                    (
                        "image_description_prompt",
                        schema::optional_string("Prompt which is used for image description", None),
                    ),
                    (
                        "process_images",
                        schema::optional_bool(
                            "Whether to process images in work item fields and attachments. Set to False to skip image description processing and return raw content.",
                            true,
                        ),
                    ),
                ],
                &["id"],
            ),
            Self::LinkWorkItems => schema::object(
                &[
                    (
                        "source_id",
                        schema::integer("ID of the work item you plan to add link to"),
                    ),
                    (
                        "target_id",
                        schema::integer("ID of the work item linked to source one"),
                    ),
                    (
                        "link_type",
                        schema::string("Link type: System.LinkTypes.Dependency-forward, etc."),
                    ),
                    (
                        "attributes",
                        serde_json::json!({
                            "type":["object","null"],
                            "default":null,
                            "description":"Dict with attributes used for work items linking. Example: `comment`, etc. and syntax 'comment': 'Some linking comment'"
                        }),
                    ),
                ],
                &["source_id", "target_id", "link_type"],
            ),
            Self::GetRelationTypes => schema::object(&[], &[]),
            Self::GetComments => schema::object(
                &[
                    ("work_item_id", schema::integer("The work item id")),
                    (
                        "limit_total",
                        schema::optional_integer("Max number of total comments to return"),
                    ),
                    (
                        "include_deleted",
                        schema::optional_bool(
                            "Specify if the deleted comments should be retrieved",
                            false,
                        ),
                    ),
                    (
                        "expand",
                        schema::optional_string(
                            "The expand parameters for comments. Possible options are { all, none, reactions, renderedText, renderedTextOnly }.",
                            Some("none"),
                        ),
                    ),
                    (
                        "order",
                        schema::optional_string(
                            "Order in which the comments should be returned. Possible options are { asc, desc }",
                            None,
                        ),
                    ),
                    (
                        "process_images",
                        schema::optional_bool(
                            "Whether to fetch and analyze images embedded in comment text. Image description is not available in this runtime.",
                            false,
                        ),
                    ),
                    (
                        "image_description_prompt",
                        schema::optional_string("Prompt which is used for image description", None),
                    ),
                ],
                &["work_item_id"],
            ),
            Self::LinkWorkItemsToWikiPage | Self::UnlinkWorkItemsFromWikiPage => {
                let verb = if self == Self::LinkWorkItemsToWikiPage {
                    "List of work item IDs to link to the wiki page"
                } else {
                    "List of work item IDs to unlink from the wiki page"
                };
                schema::object(
                    &[
                        ("work_item_ids", schema::integer_array(verb)),
                        ("wiki_identified", schema::string("Wiki ID or wiki name")),
                        (
                            "page_name",
                            schema::string("Wiki page path, for example /TargetPage"),
                        ),
                    ],
                    &["work_item_ids", "wiki_identified", "page_name"],
                )
            }
            Self::GetWorkItemTypeFields => schema::object(
                &[
                    (
                        "work_item_type",
                        schema::optional_string(
                            "Work item type to get fields for (e.g., 'Task', 'Bug', 'Test Case', 'Epic'). Default is 'Task'.",
                            Some("Task"),
                        ),
                    ),
                    (
                        "force_refresh",
                        schema::optional_bool(
                            "If True, reload field definitions from Azure DevOps. Use this if project configuration has changed.",
                            false,
                        ),
                    ),
                ],
                &[],
            ),
        }
    }

    fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::GetImageByUrl
                | Self::SearchWorkItems
                | Self::GetWorkItem
                | Self::GetRelationTypes
                | Self::GetComments
                | Self::GetWorkItemTypeFields
        )
    }
}

/// Build the served `ado_boards` tools.
pub(crate) fn build_ado_boards_toolset(
    toolkit_name: &str,
    config: AdoBoardsToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
    attachments: Option<AttachmentHost>,
) -> Result<BasicToolset, AdoToolsetError> {
    let selected = config.selected_tools().to_vec();
    if attachments.is_none() {
        tracing::warn!(
            event = "ado_boards_tools_unserved",
            tools = ?AdoBoardsToolKind::WITH_HOST.map(AdoToolKind::name),
            reason = "this run lends no artifact authority",
            "Azure DevOps tools that need the artifact store or chat model are left out"
        );
    }
    let client = Arc::new(AdoBoardsClient::new(config.into_inner())?.with_attachments(attachments));
    build_with_client(toolkit_name, &selected, policy, client)
}

pub(in crate::toolkits) fn build_with_client(
    toolkit_name: &str,
    selected: &[Box<str>],
    policy: &Arc<ToolAdmissionPolicy>,
    client: Arc<AdoBoardsClient>,
) -> Result<BasicToolset, AdoToolsetError> {
    let served = AdoBoardsToolKind::served(client.attachments.is_some());
    let executor: Arc<dyn AdoToolExecutor<AdoBoardsToolKind>> = client;
    build_toolset(
        toolkit_name,
        TOOLKIT_TYPE,
        &served,
        selected,
        policy,
        &executor,
        "",
    )
}

#[async_trait]
impl AdoToolExecutor<AdoBoardsToolKind> for AdoBoardsClient {
    async fn execute(
        &self,
        kind: AdoBoardsToolKind,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        dispatch(self, kind, arguments)
            .await?
            .map_err(AdoClientError::into_adk)
    }
}

#[allow(clippy::too_many_lines)] // One source-ordered argument ledger per tool.
async fn dispatch(
    client: &AdoBoardsClient,
    kind: AdoBoardsToolKind,
    arguments: &Map<String, Value>,
) -> adk_core::Result<Result<Value, AdoClientError>> {
    Ok(match kind {
        AdoBoardsToolKind::AttachFileToWorkItem => {
            reject_unknown_keys(
                arguments,
                &[
                    "work_item_id",
                    "filepath",
                    "filename",
                    "inline_field",
                    "add_as_comment",
                    "comment",
                ],
            )?;
            client
                .attach_file_to_work_item(AttachFile {
                    work_item_id: required_id(arguments, "work_item_id")?,
                    filepath: required_str(arguments, "filepath", MAX_PATH_BYTES)?,
                    filename: optional_str(arguments, "filename", MAX_NAME_BYTES)?,
                    inline_field: optional_str(arguments, "inline_field", MAX_IDENTIFIER_BYTES)?,
                    add_as_comment: optional_bool(arguments, "add_as_comment", false)?,
                    comment: optional_str(arguments, "comment", MAX_TEXT_BYTES)?,
                })
                .await
        }
        AdoBoardsToolKind::GetImageByUrl => {
            reject_unknown_keys(arguments, &["attachment_url", "file_name", "prompt"])?;
            client
                .get_image_by_url(
                    required_str(arguments, "attachment_url", MAX_URL_BYTES)?,
                    optional_str(arguments, "file_name", MAX_NAME_BYTES)?,
                    optional_str(arguments, "prompt", MAX_TEXT_BYTES)?,
                )
                .await
        }
        AdoBoardsToolKind::SearchWorkItems => {
            reject_unknown_keys(arguments, &["query", "limit", "fields"])?;
            client
                .search_work_items(SearchWorkItems {
                    query: required_str(arguments, "query", MAX_TEXT_BYTES)?,
                    limit: optional_i64(arguments, "limit")?,
                    fields: optional_string_list(arguments, "fields")?,
                })
                .await
        }
        AdoBoardsToolKind::CreateWorkItem => {
            reject_unknown_keys(arguments, &["work_item_json", "wi_type"])?;
            let work_item_type =
                optional_str(arguments, "wi_type", MAX_IDENTIFIER_BYTES)?.unwrap_or("Task");
            if work_item_type.trim().is_empty() {
                return Err(invalid_arguments());
            }
            client
                .create_work_item(
                    required_str(arguments, "work_item_json", MAX_TEXT_BYTES)?,
                    work_item_type,
                )
                .await
        }
        AdoBoardsToolKind::UpdateWorkItem => {
            reject_unknown_keys(arguments, &["id", "work_item_json"])?;
            client
                .update_work_item(
                    &required_id_text(arguments, "id")?,
                    required_str(arguments, "work_item_json", MAX_TEXT_BYTES)?,
                )
                .await
        }
        AdoBoardsToolKind::DeleteWorkItem => {
            reject_unknown_keys(arguments, &["id"])?;
            client.delete_work_item(required_id(arguments, "id")?).await
        }
        AdoBoardsToolKind::GetWorkItem => {
            reject_unknown_keys(
                arguments,
                &[
                    "id",
                    "fields",
                    "as_of",
                    "expand",
                    "parse_attachments",
                    "image_description_prompt",
                    "process_images",
                ],
            )?;
            // Accepted for the SDK call shape; see the tool description.
            optional_bool(arguments, "parse_attachments", false)?;
            optional_bool(arguments, "process_images", true)?;
            optional_str(arguments, "image_description_prompt", MAX_TEXT_BYTES)?;
            let fields = optional_string_list(arguments, "fields")?;
            client
                .get_work_item(
                    required_id(arguments, "id")?,
                    fields.as_deref(),
                    optional_str(arguments, "as_of", MAX_IDENTIFIER_BYTES)?,
                    optional_str(arguments, "expand", MAX_IDENTIFIER_BYTES)?,
                )
                .await
        }
        AdoBoardsToolKind::LinkWorkItems => {
            reject_unknown_keys(
                arguments,
                &["source_id", "target_id", "link_type", "attributes"],
            )?;
            let attributes = match arguments.get("attributes") {
                None => None,
                Some(Value::Object(attributes)) => Some(attributes),
                Some(_) => return Err(invalid_arguments()),
            };
            client
                .link_work_items(
                    required_id(arguments, "source_id")?,
                    required_id(arguments, "target_id")?,
                    required_str(arguments, "link_type", MAX_IDENTIFIER_BYTES)?,
                    attributes,
                )
                .await
        }
        AdoBoardsToolKind::GetRelationTypes => {
            reject_unknown_keys(arguments, &[])?;
            client.relation_types().await.map(|types| {
                Value::Object(
                    types
                        .into_iter()
                        .map(|(name, reference)| (name, Value::String(reference)))
                        .collect(),
                )
            })
        }
        AdoBoardsToolKind::GetComments => {
            reject_unknown_keys(
                arguments,
                &[
                    "work_item_id",
                    "limit_total",
                    "include_deleted",
                    "expand",
                    "order",
                    "process_images",
                    "image_description_prompt",
                ],
            )?;
            optional_str(arguments, "image_description_prompt", MAX_TEXT_BYTES)?;
            client
                .get_comments(GetComments {
                    work_item_id: required_id(arguments, "work_item_id")?,
                    limit_total: optional_i64(arguments, "limit_total")?,
                    include_deleted: optional_bool(arguments, "include_deleted", false)?,
                    expand: Some(
                        optional_str(arguments, "expand", MAX_IDENTIFIER_BYTES)?.unwrap_or("none"),
                    ),
                    order: optional_str(arguments, "order", MAX_IDENTIFIER_BYTES)?,
                    process_images: optional_bool(arguments, "process_images", false)?,
                })
                .await
        }
        AdoBoardsToolKind::LinkWorkItemsToWikiPage
        | AdoBoardsToolKind::UnlinkWorkItemsFromWikiPage => {
            reject_unknown_keys(
                arguments,
                &["work_item_ids", "wiki_identified", "page_name"],
            )?;
            let ids = required_id_list(arguments, "work_item_ids")?;
            let wiki = required_str(arguments, "wiki_identified", MAX_IDENTIFIER_BYTES)?;
            let page = required_str(arguments, "page_name", MAX_IDENTIFIER_BYTES)?;
            if wiki.is_empty() {
                return Err(invalid_arguments());
            }
            if kind == AdoBoardsToolKind::LinkWorkItemsToWikiPage {
                client.link_work_items_to_wiki_page(&ids, wiki, page).await
            } else {
                client
                    .unlink_work_items_from_wiki_page(&ids, wiki, page)
                    .await
            }
        }
        AdoBoardsToolKind::GetWorkItemTypeFields => {
            reject_unknown_keys(arguments, &["work_item_type", "force_refresh"])?;
            let work_item_type =
                optional_str(arguments, "work_item_type", MAX_IDENTIFIER_BYTES)?.unwrap_or("Task");
            if work_item_type.trim().is_empty() {
                return Err(invalid_arguments());
            }
            client
                .get_work_item_type_fields(
                    work_item_type,
                    optional_bool(arguments, "force_refresh", false)?,
                )
                .await
        }
    })
}

/// Every tool this family serves, with whether it needs the run's file store
/// (`materialize::omitted_selection` reads it for the run's notice).
pub(crate) fn tool_catalog() -> Vec<(&'static str, bool)> {
    AdoBoardsToolKind::ALL
        .iter()
        .map(|kind| (kind.name(), false))
        .chain(
            AdoBoardsToolKind::WITH_HOST
                .iter()
                .map(|kind| (kind.name(), true)),
        )
        .collect()
}
