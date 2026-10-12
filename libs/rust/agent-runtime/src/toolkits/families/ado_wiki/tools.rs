use std::sync::Arc;

use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::families::ado::client::{AdoClientError, IntoAdk};
use crate::toolkits::families::ado::toolset::{
    AdoToolExecutor, AdoToolKind, AdoToolsetError, MAX_IDENTIFIER_BYTES, MAX_TEXT_BYTES,
    build_toolset, invalid_arguments, optional_bool, optional_id, optional_str,
    reject_unknown_keys, required_id, required_str, schema,
};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{AdoWikiClient, GetWikiPage, ModifyWikiPage, PageAddress, RenameWikiPage};
use super::config::AdoWikiToolkitConfig;

pub(crate) const TOOLKIT_TYPE: &str = "ado_wiki";

const WIKI_IDENTIFIED: &str = "Wiki ID or wiki name. If not provided, uses the default wiki identifier from toolkit configuration.";
const RECURSION_LEVELS: [&str; 4] = ["none", "oneLevel", "oneLevelPlusNestedEmptyFolders", "full"];

/// Every non-index `ado_wiki` tool.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum AdoWikiToolKind {
    GetWiki,
    GetWikiPage,
    GetWikiPageByPath,
    GetWikiPageById,
    DeletePageByPath,
    DeletePageById,
    ModifyWikiPage,
    RenameWikiPage,
}

impl AdoWikiToolKind {
    pub(crate) const ALL: [Self; 8] = [
        Self::GetWiki,
        Self::GetWikiPage,
        Self::GetWikiPageByPath,
        Self::GetWikiPageById,
        Self::DeletePageByPath,
        Self::DeletePageById,
        Self::ModifyWikiPage,
        Self::RenameWikiPage,
    ];
}

fn wiki() -> Value {
    schema::optional_string(WIKI_IDENTIFIED, None)
}

fn image_prompt() -> Value {
    schema::optional_string("Prompt which is used for image description", None)
}

fn process_images() -> Value {
    schema::optional_bool(
        "Whether to process images in page content. Set to False to get raw content without image description processing.",
        true,
    )
}

fn version_type() -> Value {
    schema::optional_string(
        "Version type (branch, tag, or commit). Determines how Id is interpreted",
        Some("branch"),
    )
}

impl AdoToolKind for AdoWikiToolKind {
    fn name(self) -> &'static str {
        match self {
            Self::GetWiki => "get_wiki",
            Self::GetWikiPage => "get_wiki_page",
            Self::GetWikiPageByPath => "get_wiki_page_by_path",
            Self::GetWikiPageById => "get_wiki_page_by_id",
            Self::DeletePageByPath => "delete_page_by_path",
            Self::DeletePageById => "delete_page_by_id",
            Self::ModifyWikiPage => "modify_wiki_page",
            Self::RenameWikiPage => "rename_wiki_page",
        }
    }

    fn description(self) -> &'static str {
        match self {
            Self::GetWiki => "Extract ADO wiki information.",
            Self::GetWikiPage => {
                "Get wiki page metadata and optionally content. Retrieves eTag, id, path, git_item_path, remote_url, url, sub_pages, order, is_parent_page and is_non_conformant, by page_id (takes precedence) or page_path. recursion_level controls sub-page depth: none, oneLevel (default), oneLevelPlusNestedEmptyFolders or full. Image description is not available in this runtime; image references in content are returned unchanged."
            }
            Self::GetWikiPageByPath | Self::GetWikiPageById => {
                "Extract ADO wiki page content. Image description is not available in this runtime; image references are returned unchanged."
            }
            Self::DeletePageByPath => "Delete ADO wiki page by path.",
            Self::DeletePageById => "Delete ADO wiki page by ID.",
            Self::ModifyWikiPage => {
                "Create or Update ADO wiki page content. Creates the wiki when it does not exist and the page when the path is new."
            }
            Self::RenameWikiPage => {
                "Rename page: move old_page_name (e.g. '/old_page_name') to new_page_name (e.g. '/new_page_name') at the given branch, tag or commit version."
            }
        }
    }

    #[allow(clippy::too_many_lines)] // One schema per SDK tool keeps the catalogue auditable.
    fn schema(self) -> Value {
        match self {
            Self::GetWiki => schema::object(&[("wiki_identified", wiki())], &[]),
            Self::GetWikiPage => schema::object(
                &[
                    ("wiki_identified", wiki()),
                    (
                        "page_path",
                        schema::optional_string("Wiki page path (e.g., '/MB_Heading/MB_2')", None),
                    ),
                    ("page_id", schema::optional_integer("Wiki page ID")),
                    (
                        "include_content",
                        schema::optional_bool(
                            "Whether to include page content in the response.",
                            false,
                        ),
                    ),
                    ("image_description_prompt", image_prompt()),
                    ("process_images", process_images()),
                    (
                        "recursion_level",
                        json!({
                            "type":["string","null"],
                            "enum":["none","oneLevel","oneLevelPlusNestedEmptyFolders","full",null],
                            "default":"oneLevel",
                            "description":"Controls how many levels of sub-pages are retrieved along with the main page. Options: 'none' (No subpages retrieved - only the requested page metadata), 'oneLevel' (Direct children only - immediate sub-pages) [default], 'oneLevelPlusNestedEmptyFolders' (Direct children plus recursive chains of nested child folders that only contain a single folder), 'full' (All descendants - entire page hierarchy)."
                        }),
                    ),
                ],
                &[],
            ),
            Self::GetWikiPageByPath => schema::object(
                &[
                    ("wiki_identified", wiki()),
                    ("page_name", schema::string("Wiki page path")),
                    ("image_description_prompt", image_prompt()),
                    ("process_images", process_images()),
                ],
                &["page_name"],
            ),
            Self::GetWikiPageById => schema::object(
                &[
                    ("wiki_identified", wiki()),
                    ("page_id", schema::integer("Wiki page ID")),
                    ("image_description_prompt", image_prompt()),
                    ("process_images", process_images()),
                ],
                &["page_id"],
            ),
            Self::DeletePageByPath => schema::object(
                &[
                    ("wiki_identified", wiki()),
                    ("page_name", schema::string("Wiki page path")),
                ],
                &["page_name"],
            ),
            Self::DeletePageById => schema::object(
                &[
                    ("wiki_identified", wiki()),
                    ("page_id", schema::integer("Wiki page ID")),
                ],
                &["page_id"],
            ),
            Self::ModifyWikiPage => schema::object(
                &[
                    ("wiki_identified", wiki()),
                    ("page_name", schema::string("Wiki page name")),
                    ("page_content", schema::string("Wiki page content")),
                    (
                        "version_identifier",
                        schema::string(
                            "Version string identifier (name of tag/branch, SHA1 of commit). Usually for wiki the branch is 'wikiMaster'",
                        ),
                    ),
                    ("version_type", version_type()),
                    (
                        "expanded",
                        schema::optional_bool(
                            "Whether to return the full page object or just its simplified version.",
                            false,
                        ),
                    ),
                ],
                &["page_name", "page_content", "version_identifier"],
            ),
            Self::RenameWikiPage => schema::object(
                &[
                    ("wiki_identified", wiki()),
                    (
                        "old_page_name",
                        schema::string("Old Wiki page name to be renamed, e.g. /TestPageName"),
                    ),
                    (
                        "new_page_name",
                        schema::string("New Wiki page name, e.g. /RenamedName"),
                    ),
                    (
                        "version_identifier",
                        schema::string(
                            "Version string identifier (name of tag/branch, SHA1 of commit)",
                        ),
                    ),
                    ("version_type", version_type()),
                ],
                &["old_page_name", "new_page_name", "version_identifier"],
            ),
        }
    }

    fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::GetWiki | Self::GetWikiPage | Self::GetWikiPageByPath | Self::GetWikiPageById
        )
    }
}

/// Build the served `ado_wiki` tools.
pub(crate) fn build_ado_wiki_toolset(
    toolkit_name: &str,
    config: AdoWikiToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, AdoToolsetError> {
    let selected = config.selected_tools().to_vec();
    let client = Arc::new(AdoWikiClient::new(config.into_inner())?);
    build_with_client(toolkit_name, &selected, policy, client)
}

pub(in crate::toolkits) fn build_with_client(
    toolkit_name: &str,
    selected: &[Box<str>],
    policy: &Arc<ToolAdmissionPolicy>,
    client: Arc<AdoWikiClient>,
) -> Result<BasicToolset, AdoToolsetError> {
    let suffix = client
        .default_wiki()
        .map(|wiki| format!("\nDefault wiki: {wiki}"))
        .unwrap_or_default();
    let executor: Arc<dyn AdoToolExecutor<AdoWikiToolKind>> = client;
    build_toolset(
        toolkit_name,
        TOOLKIT_TYPE,
        &AdoWikiToolKind::ALL,
        selected,
        policy,
        &executor,
        &suffix,
    )
}

#[async_trait]
impl AdoToolExecutor<AdoWikiToolKind> for AdoWikiClient {
    async fn execute(
        &self,
        kind: AdoWikiToolKind,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        dispatch(self, kind, arguments)
            .await?
            .map_err(AdoClientError::into_adk)
    }
}

fn path_argument<'a>(arguments: &'a Map<String, Value>, key: &str) -> adk_core::Result<&'a str> {
    let path = required_str(arguments, key, MAX_IDENTIFIER_BYTES)?;
    if path.is_empty() {
        return Err(invalid_arguments());
    }
    Ok(path)
}

#[allow(clippy::too_many_lines)] // One source-ordered argument ledger per tool.
async fn dispatch(
    client: &AdoWikiClient,
    kind: AdoWikiToolKind,
    arguments: &Map<String, Value>,
) -> adk_core::Result<Result<Value, AdoClientError>> {
    let wiki = optional_str(arguments, "wiki_identified", MAX_IDENTIFIER_BYTES)?;
    Ok(match kind {
        AdoWikiToolKind::GetWiki => {
            reject_unknown_keys(arguments, &["wiki_identified"])?;
            client.get_wiki(wiki).await
        }
        AdoWikiToolKind::GetWikiPage => {
            reject_unknown_keys(
                arguments,
                &[
                    "wiki_identified",
                    "page_path",
                    "page_id",
                    "include_content",
                    "image_description_prompt",
                    "process_images",
                    "recursion_level",
                ],
            )?;
            optional_str(arguments, "image_description_prompt", MAX_TEXT_BYTES)?;
            optional_bool(arguments, "process_images", true)?;
            let recursion_level = optional_str(arguments, "recursion_level", MAX_IDENTIFIER_BYTES)?
                .unwrap_or("oneLevel");
            if !RECURSION_LEVELS.contains(&recursion_level) {
                return Err(invalid_arguments());
            }
            // `if page_id:` takes precedence; zero and "" are falsy.
            let page_id = match arguments.get("page_id") {
                Some(Value::Number(number)) if number.as_i64() == Some(0) => None,
                _ => optional_id(arguments, "page_id")?,
            };
            let page_path = optional_str(arguments, "page_path", MAX_IDENTIFIER_BYTES)?
                .filter(|path| !path.is_empty());
            let address = page_id
                .map(PageAddress::Id)
                .or_else(|| page_path.map(PageAddress::Path));
            client
                .get_wiki_page(GetWikiPage {
                    wiki_identified: wiki,
                    address,
                    include_content: optional_bool(arguments, "include_content", false)?,
                    recursion_level,
                })
                .await
        }
        AdoWikiToolKind::GetWikiPageByPath => {
            reject_unknown_keys(
                arguments,
                &[
                    "wiki_identified",
                    "page_name",
                    "image_description_prompt",
                    "process_images",
                ],
            )?;
            optional_str(arguments, "image_description_prompt", MAX_TEXT_BYTES)?;
            optional_bool(arguments, "process_images", true)?;
            client
                .page_content(
                    wiki,
                    PageAddress::Path(path_argument(arguments, "page_name")?),
                )
                .await
        }
        AdoWikiToolKind::GetWikiPageById => {
            reject_unknown_keys(
                arguments,
                &[
                    "wiki_identified",
                    "page_id",
                    "image_description_prompt",
                    "process_images",
                ],
            )?;
            optional_str(arguments, "image_description_prompt", MAX_TEXT_BYTES)?;
            optional_bool(arguments, "process_images", true)?;
            client
                .page_content(wiki, PageAddress::Id(required_id(arguments, "page_id")?))
                .await
        }
        AdoWikiToolKind::DeletePageByPath => {
            reject_unknown_keys(arguments, &["wiki_identified", "page_name"])?;
            client
                .delete_page(
                    wiki,
                    PageAddress::Path(path_argument(arguments, "page_name")?),
                )
                .await
        }
        AdoWikiToolKind::DeletePageById => {
            reject_unknown_keys(arguments, &["wiki_identified", "page_id"])?;
            client
                .delete_page(wiki, PageAddress::Id(required_id(arguments, "page_id")?))
                .await
        }
        AdoWikiToolKind::ModifyWikiPage => {
            reject_unknown_keys(
                arguments,
                &[
                    "wiki_identified",
                    "page_name",
                    "page_content",
                    "version_identifier",
                    "version_type",
                    "expanded",
                ],
            )?;
            client
                .modify_wiki_page(ModifyWikiPage {
                    wiki_identified: wiki,
                    page_name: path_argument(arguments, "page_name")?,
                    page_content: required_str(arguments, "page_content", MAX_TEXT_BYTES)?,
                    version_identifier: required_str(
                        arguments,
                        "version_identifier",
                        MAX_IDENTIFIER_BYTES,
                    )?,
                    version_type: optional_str(arguments, "version_type", MAX_IDENTIFIER_BYTES)?
                        .unwrap_or("branch"),
                    expanded: optional_bool(arguments, "expanded", false)?,
                })
                .await
        }
        AdoWikiToolKind::RenameWikiPage => {
            reject_unknown_keys(
                arguments,
                &[
                    "wiki_identified",
                    "old_page_name",
                    "new_page_name",
                    "version_identifier",
                    "version_type",
                ],
            )?;
            client
                .rename_wiki_page(RenameWikiPage {
                    wiki_identified: wiki,
                    old_page_name: path_argument(arguments, "old_page_name")?,
                    new_page_name: path_argument(arguments, "new_page_name")?,
                    version_identifier: required_str(
                        arguments,
                        "version_identifier",
                        MAX_IDENTIFIER_BYTES,
                    )?,
                    version_type: optional_str(arguments, "version_type", MAX_IDENTIFIER_BYTES)?
                        .unwrap_or("branch"),
                })
                .await
        }
    })
}
