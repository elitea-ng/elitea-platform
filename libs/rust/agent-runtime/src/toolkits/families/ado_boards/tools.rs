use std::sync::Arc;

use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value};

use crate::toolkits::families::ado::client::AdoClientError;
use crate::toolkits::families::ado::toolset::{
    AdoToolExecutor, AdoToolKind, AdoToolsetError, MAX_IDENTIFIER_BYTES, MAX_TEXT_BYTES,
    build_toolset, invalid_arguments, optional_bool, optional_i64, optional_str,
    optional_string_list, reject_unknown_keys, required_id, required_id_list, required_id_text,
    required_str, schema,
};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{AdoBoardsClient, GetComments, SearchWorkItems};
use super::config::AdoBoardsToolkitConfig;

pub(crate) const TOOLKIT_TYPE: &str = "ado_boards";

const CREATE_WI_FIELD: &str = "JSON of the work item fields to create in Azure DevOps, i.e.\n                    {\n                       \"fields\":{\n                          \"System.Title\":\"Implement Registration Form Validation\",\n                          \"field2\":\"Value 2\",\n                       }\n                    }\n                    ";

/// The `ado_boards` tools this runtime serves. `get_image_by_url` (vision
/// LLM) and `attach_file_to_work_item` (artifact storage) need authorities
/// a toolkit does not hold here, and the index tools need indexing; they are
/// absent, and the capability snapshot lists exactly these.
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
        }
    }

    #[allow(clippy::too_many_lines)] // One schema per SDK tool keeps the catalogue auditable.
    fn schema(self) -> Value {
        match self {
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
            Self::SearchWorkItems
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
) -> Result<BasicToolset, AdoToolsetError> {
    let selected = config.selected_tools().to_vec();
    let client = Arc::new(AdoBoardsClient::new(config.into_inner())?);
    build_with_client(toolkit_name, &selected, policy, client)
}

pub(in crate::toolkits) fn build_with_client(
    toolkit_name: &str,
    selected: &[Box<str>],
    policy: &Arc<ToolAdmissionPolicy>,
    client: Arc<AdoBoardsClient>,
) -> Result<BasicToolset, AdoToolsetError> {
    let executor: Arc<dyn AdoToolExecutor<AdoBoardsToolKind>> = client;
    build_toolset(
        toolkit_name,
        TOOLKIT_TYPE,
        &AdoBoardsToolKind::ALL,
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
