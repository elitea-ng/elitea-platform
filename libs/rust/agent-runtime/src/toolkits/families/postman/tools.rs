use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{PostmanApi, PostmanClient, PostmanClientError, PostmanOperation};
use super::config::{PostmanConfigError, PostmanToolkitConfig};

const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_STRING_BYTES: usize = 64 * 1_024;
const MAX_IDENTIFIER_BYTES: usize = 1_024;
const MAX_PATH_BYTES: usize = 4 * 1_024;
const MAX_LIST_ITEMS: usize = 4_096;
const MAX_DESCRIPTION_BYTES: usize = 3_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PostmanToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

pub struct PostmanToolsetError {
    code: PostmanToolsetErrorCode,
}

impl PostmanToolsetError {
    #[must_use]
    pub const fn code(&self) -> PostmanToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for PostmanToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("PostmanToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for PostmanToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            PostmanToolsetErrorCode::InvalidConfiguration => {
                "the Postman toolkit configuration is invalid"
            }
            PostmanToolsetErrorCode::ResourceExhausted => {
                "the Postman toolkit configuration exceeds its approved limit"
            }
            PostmanToolsetErrorCode::UnsupportedSelection => {
                "the selected Postman tool profile is unsupported"
            }
            PostmanToolsetErrorCode::Client => "the Postman client could not be created",
            PostmanToolsetErrorCode::InvalidDefinition => {
                "the Postman ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for PostmanToolsetError {}

impl From<PostmanConfigError> for PostmanToolsetError {
    fn from(source: PostmanConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::PostmanConfigErrorCode::InvalidConfiguration => {
                    PostmanToolsetErrorCode::InvalidConfiguration
                }
                super::config::PostmanConfigErrorCode::ResourceExhausted => {
                    PostmanToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<PostmanClientError> for PostmanToolsetError {
    fn from(_: PostmanClientError) -> Self {
        Self {
            code: PostmanToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for PostmanToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: PostmanToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the complete capability-disabled 31-tool Postman family.
#[allow(clippy::needless_pass_by_value)]
pub fn build_postman_toolset(
    toolkit_name: &str,
    config: PostmanToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, PostmanToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn PostmanApi> = Arc::new(PostmanClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), PostmanToolsetError> {
    if selected.iter().any(|name| {
        !PostmanToolKind::ALL
            .iter()
            .any(|kind| kind.name() == name.as_ref())
    }) {
        return Err(PostmanToolsetError {
            code: PostmanToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn PostmanApi>,
) -> Result<BasicToolset, PostmanToolsetError> {
    let all = selected.is_empty();
    let tools = PostmanToolKind::ALL
        .into_iter()
        .filter(|kind| all || selected.iter().any(|name| name == kind.name()))
        .map(|kind| {
            Arc::new(PostmanTool::new(kind, toolkit_name, Arc::clone(client))) as Arc<dyn Tool>
        })
        .collect();
    admit_materialized_toolset(toolkit_name, "postman", policy, tools).map_err(Into::into)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn PostmanApi>,
) -> Result<BasicToolset, PostmanToolsetError> {
    build_with_api(toolkit_name, selected, policy, client)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_catalog() -> Vec<(&'static str, &'static str)> {
    PostmanToolKind::ALL
        .into_iter()
        .map(|kind| (kind.name(), kind.group()))
        .collect()
}

#[derive(Clone, Copy)]
enum PostmanToolKind {
    GetCollections,
    GetCollection,
    GetFolder,
    GetRequestByPath,
    GetRequestById,
    GetRequestScript,
    SearchRequests,
    Analyze,
    ExecuteRequest,
    UpdateCollectionDescription,
    UpdateCollectionVariables,
    UpdateCollectionAuth,
    DeleteCollection,
    DuplicateCollection,
    CreateFolder,
    UpdateFolder,
    DeleteFolder,
    MoveFolder,
    CreateRequest,
    UpdateRequestName,
    UpdateRequestMethod,
    UpdateRequestUrl,
    UpdateRequestDescription,
    UpdateRequestHeaders,
    UpdateRequestBody,
    UpdateRequestAuth,
    UpdateRequestTests,
    UpdateRequestPreScript,
    DeleteRequest,
    DuplicateRequest,
    MoveRequest,
}

impl PostmanToolKind {
    const ALL: [Self; 31] = [
        Self::GetCollections,
        Self::GetCollection,
        Self::GetFolder,
        Self::GetRequestByPath,
        Self::GetRequestById,
        Self::GetRequestScript,
        Self::SearchRequests,
        Self::Analyze,
        Self::ExecuteRequest,
        Self::UpdateCollectionDescription,
        Self::UpdateCollectionVariables,
        Self::UpdateCollectionAuth,
        Self::DeleteCollection,
        Self::DuplicateCollection,
        Self::CreateFolder,
        Self::UpdateFolder,
        Self::DeleteFolder,
        Self::MoveFolder,
        Self::CreateRequest,
        Self::UpdateRequestName,
        Self::UpdateRequestMethod,
        Self::UpdateRequestUrl,
        Self::UpdateRequestDescription,
        Self::UpdateRequestHeaders,
        Self::UpdateRequestBody,
        Self::UpdateRequestAuth,
        Self::UpdateRequestTests,
        Self::UpdateRequestPreScript,
        Self::DeleteRequest,
        Self::DuplicateRequest,
        Self::MoveRequest,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::GetCollections => "get_collections",
            Self::GetCollection => "get_collection",
            Self::GetFolder => "get_folder",
            Self::GetRequestByPath => "get_request_by_path",
            Self::GetRequestById => "get_request_by_id",
            Self::GetRequestScript => "get_request_script",
            Self::SearchRequests => "search_requests",
            Self::Analyze => "analyze",
            Self::ExecuteRequest => "execute_request",
            Self::UpdateCollectionDescription => "update_collection_description",
            Self::UpdateCollectionVariables => "update_collection_variables",
            Self::UpdateCollectionAuth => "update_collection_auth",
            Self::DeleteCollection => "delete_collection",
            Self::DuplicateCollection => "duplicate_collection",
            Self::CreateFolder => "create_folder",
            Self::UpdateFolder => "update_folder",
            Self::DeleteFolder => "delete_folder",
            Self::MoveFolder => "move_folder",
            Self::CreateRequest => "create_request",
            Self::UpdateRequestName => "update_request_name",
            Self::UpdateRequestMethod => "update_request_method",
            Self::UpdateRequestUrl => "update_request_url",
            Self::UpdateRequestDescription => "update_request_description",
            Self::UpdateRequestHeaders => "update_request_headers",
            Self::UpdateRequestBody => "update_request_body",
            Self::UpdateRequestAuth => "update_request_auth",
            Self::UpdateRequestTests => "update_request_tests",
            Self::UpdateRequestPreScript => "update_request_pre_script",
            Self::DeleteRequest => "delete_request",
            Self::DuplicateRequest => "duplicate_request",
            Self::MoveRequest => "move_request",
        }
    }

    const fn group(self) -> &'static str {
        match self {
            Self::GetCollections
            | Self::GetCollection
            | Self::GetFolder
            | Self::GetRequestByPath
            | Self::GetRequestById
            | Self::GetRequestScript
            | Self::SearchRequests
            | Self::Analyze => "read",
            Self::ExecuteRequest => "execute",
            Self::DeleteCollection | Self::DeleteFolder | Self::DeleteRequest => "delete",
            _ => "write",
        }
    }

    const fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::GetCollections
                | Self::GetCollection
                | Self::GetFolder
                | Self::GetRequestByPath
                | Self::GetRequestById
                | Self::GetRequestScript
                | Self::SearchRequests
                | Self::Analyze
        )
    }

    const fn description(self) -> &'static str {
        match self {
            Self::GetCollections => {
                "List collections in the configured workspace. Returns a bounded Postman collection summary and validates the workspace when the collection list is empty. Stored secrets are not returned; independent sensitivity policy may still require approval."
            }
            Self::GetCollection => {
                "Read one collection as a bounded flattened path map. Omit collection_id to use the configured collection. Authentication, variable values, header values, bodies, scripts, URL userinfo, URL path values, and query values are redacted."
            }
            Self::GetFolder => {
                "Read one folder by exact case-insensitive slash-separated path such as API/Users. Ambiguous sibling names are rejected. Returns a bounded flattened subtree with stored secrets and URL values redacted."
            }
            Self::GetRequestByPath => {
                "Read one request using its exact case-insensitive collection path, for example API/Users/Get User. The path is resolved from the collection before the individual request is fetched; ambiguous matches are rejected and sensitive fields are redacted."
            }
            Self::GetRequestById => {
                "Read one request by its Postman request ID. Returns a bounded projection with authentication, variables, headers, body, scripts, userinfo, URL path values, and query values redacted."
            }
            Self::GetRequestScript => {
                "Read only a request's requested test or prerequest script by exact path. This tool intentionally reveals that bounded script while other request reads redact scripts."
            }
            Self::SearchRequests => {
                "Search raw stored request names, descriptions, or URLs with a case-insensitive query, optionally filtered by HTTP method. Matching uses original values for compatibility, but returned URLs and stored secrets are redacted. Results are bounded to 4096 entries."
            }
            Self::Analyze => {
                "Analyze collection, folder, or request quality using deterministic security, hardcoded-data, header, tests, documentation, performance, naming, and authentication-consistency rules. Folder/request scope requires an exact unambiguous target_path. Results preserve SDK scoring shapes while URL values are redacted; improvements are optional."
            }
            Self::ExecuteRequest => {
                "Execute one stored request after bounded variable expansion (environment, then collection, then overrides). Raw JSON accepts bounded string-aware // line comments without changing // inside quoted values. Scripts never run. This is separate dynamic egress and requires an approved resolved-origin/DNS grant; redirects and automatic retries are disabled. It can cause a remote effect. After dispatch, timeout or response failure has unknown outcome; do not retry until the downstream state is reconciled."
            }
            Self::UpdateCollectionDescription => effect_description(
                "Replace a collection description, then read a bounded confirmation. Omit collection_id to use the configured collection.",
            ),
            Self::UpdateCollectionVariables => effect_description(
                "Replace collection variables. Omit or pass null to set the provider field to null; confirmation redacts variable values.",
            ),
            Self::UpdateCollectionAuth => effect_description(
                "Replace or clear collection authentication. Omit or pass null to clear it; confirmation never returns credentials.",
            ),
            Self::DeleteCollection => delete_description(
                "Permanently delete one collection. Omit collection_id to use the configured collection.",
            ),
            Self::DuplicateCollection => effect_description(
                "Duplicate the configured collection with new_name after removing collection, folder, and request IDs.",
            ),
            Self::CreateFolder => full_collection_description(
                "Create a folder at collection root or under exact parent_path. Optional auth is stored but never returned.",
            ),
            Self::UpdateFolder => effect_description(
                "Update one folder through its direct endpoint. Null or omitted auth means unchanged; when every optional field is omitted, no remote write occurs.",
            ),
            Self::DeleteFolder => delete_description(
                "Delete one exact folder path and its complete subtree through a full-collection replacement.",
            ),
            Self::MoveFolder => full_collection_description(
                "Move one exact folder subtree to target_path, or to collection root when target_path is null.",
            ),
            Self::CreateRequest => full_collection_description(
                "Create one stored request at collection root or exact folder_path. Supports explicit HTTP method, URL, headers, body, auth, test script, and pre-request script.",
            ),
            Self::UpdateRequestName => effect_description(
                "Replace one exact request path's name through the direct request endpoint.",
            ),
            Self::UpdateRequestMethod => effect_description(
                "Replace one exact request path's method with a bounded RFC HTTP token such as GET, TRACE, or a provider extension; it is stored uppercase.",
            ),
            Self::UpdateRequestUrl => {
                effect_description("Replace one exact request path's stored URL.")
            }
            Self::UpdateRequestDescription => {
                effect_description("Replace one exact request path's description.")
            }
            Self::UpdateRequestHeaders => effect_description(
                "Replace one exact request path's headers using newline-separated Header-Name: value text.",
            ),
            Self::UpdateRequestBody => effect_description(
                "Replace one exact request path's raw, urlencoded, or formdata body and require Postman's update receipt.",
            ),
            Self::UpdateRequestAuth => effect_description(
                "Replace or clear one exact request path's authentication. Omit or pass null to clear it.",
            ),
            Self::UpdateRequestTests => effect_description(
                "Replace one exact request path's test event while preserving other event kinds. Stored scripts are bounded to 65536 UTF-8 bytes.",
            ),
            Self::UpdateRequestPreScript => effect_description(
                "Replace one exact request path's prerequest event while preserving other event kinds. Stored scripts are bounded to 65536 UTF-8 bytes.",
            ),
            Self::DeleteRequest => delete_description(
                "Permanently delete one exact request path through a full-collection replacement.",
            ),
            Self::DuplicateRequest => full_collection_description(
                "Duplicate one exact request as new_name, remove its IDs, and place it in target_path or beside the source when omitted.",
            ),
            Self::MoveRequest => full_collection_description(
                "Move one exact request to target_path, or collection root when target_path is null.",
            ),
        }
    }

    fn schema(self) -> Value {
        schema_for(self)
    }
}

const fn effect_description(action: &'static str) -> &'static str {
    action
}

const fn full_collection_description(action: &'static str) -> &'static str {
    action
}

const fn delete_description(action: &'static str) -> &'static str {
    action
}

struct PostmanTool {
    kind: PostmanToolKind,
    client: Arc<dyn PostmanApi>,
    description: Box<str>,
}

impl PostmanTool {
    fn new(kind: PostmanToolKind, toolkit_name: &str, client: Arc<dyn PostmanApi>) -> Self {
        let mut description = format!("Toolkit: {toolkit_name}\n{}", kind.description());
        if !kind.is_read_only() && kind != PostmanToolKind::ExecuteRequest {
            if matches!(
                kind,
                PostmanToolKind::CreateFolder
                    | PostmanToolKind::DeleteFolder
                    | PostmanToolKind::MoveFolder
                    | PostmanToolKind::CreateRequest
                    | PostmanToolKind::DeleteRequest
                    | PostmanToolKind::DuplicateRequest
                    | PostmanToolKind::MoveRequest
            ) {
                description.push_str(" Full-collection writes are serialized only inside this invocation; production activation requires a cross-worker collection fence and reconciliation receipt.");
            }
            description.push_str(" This is a remote effect. After dispatch, timeout, cancellation, unexpected response, or confirmation failure has unknown outcome; do not retry until Postman state is reconciled.");
        }
        if matches!(
            kind,
            PostmanToolKind::DeleteCollection
                | PostmanToolKind::DeleteFolder
                | PostmanToolKind::DeleteRequest
        ) {
            description.push_str(" The destructive effect may already have completed even when the outcome is unknown.");
        }
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

impl PartialEq for PostmanToolKind {
    fn eq(&self, other: &Self) -> bool {
        self.name() == other.name()
    }
}

#[async_trait]
impl Tool for PostmanTool {
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
        Some(self.kind.schema())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        if serde_json::to_vec(&arguments)
            .map_err(|_| invalid_arguments())?
            .len()
            > MAX_ARGUMENT_BYTES
        {
            return Err(resource_exhausted());
        }
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        let operation = operation_from_arguments(self.kind, arguments)?;
        self.client
            .execute(operation)
            .await
            .map_err(PostmanClientError::into_adk)
    }
}

fn schema_for(kind: PostmanToolKind) -> Value {
    let SchemaParts {
        properties,
        required,
    } = match kind {
        PostmanToolKind::GetCollections
        | PostmanToolKind::GetCollection
        | PostmanToolKind::GetFolder
        | PostmanToolKind::GetRequestByPath
        | PostmanToolKind::GetRequestById
        | PostmanToolKind::GetRequestScript
        | PostmanToolKind::SearchRequests
        | PostmanToolKind::Analyze
        | PostmanToolKind::ExecuteRequest => read_schema_parts(kind),
        PostmanToolKind::UpdateCollectionDescription
        | PostmanToolKind::UpdateCollectionVariables
        | PostmanToolKind::UpdateCollectionAuth
        | PostmanToolKind::DeleteCollection
        | PostmanToolKind::DuplicateCollection => collection_schema_parts(kind),
        PostmanToolKind::CreateFolder
        | PostmanToolKind::UpdateFolder
        | PostmanToolKind::DeleteFolder
        | PostmanToolKind::MoveFolder => folder_schema_parts(kind),
        PostmanToolKind::CreateRequest => create_request_schema_parts(),
        PostmanToolKind::UpdateRequestName
        | PostmanToolKind::UpdateRequestMethod
        | PostmanToolKind::UpdateRequestUrl
        | PostmanToolKind::UpdateRequestDescription
        | PostmanToolKind::UpdateRequestHeaders
        | PostmanToolKind::UpdateRequestBody
        | PostmanToolKind::UpdateRequestAuth
        | PostmanToolKind::UpdateRequestTests
        | PostmanToolKind::UpdateRequestPreScript => request_field_schema_parts(kind),
        _ => request_tree_schema_parts(kind),
    };
    json!({
        "type":"object",
        "properties": properties,
        "required": required,
        "additionalProperties": false
    })
}

struct SchemaParts {
    properties: Map<String, Value>,
    required: Vec<&'static str>,
}

fn read_schema_parts(kind: PostmanToolKind) -> SchemaParts {
    let mut properties = Map::new();
    let required: Vec<&str> = match kind {
        PostmanToolKind::GetCollections => Vec::new(),
        PostmanToolKind::GetCollection => {
            properties.insert(
                "collection_id".to_owned(),
                optional_id("Postman collection ID; null uses the configured collection."),
            );
            Vec::new()
        }
        PostmanToolKind::GetFolder => {
            properties.insert(
                "folder_path".to_owned(),
                path_schema("Exact slash-separated folder path, for example API/Users."),
            );
            vec!["folder_path"]
        }
        PostmanToolKind::GetRequestByPath => {
            properties.insert(
                "request_path".to_owned(),
                path_schema("Exact slash-separated request path, for example API/Users/Get User."),
            );
            vec!["request_path"]
        }
        PostmanToolKind::GetRequestById => {
            properties.insert(
                "request_id".to_owned(),
                id_schema("Unique Postman request ID."),
            );
            vec!["request_id"]
        }
        PostmanToolKind::GetRequestScript => {
            properties.insert(
                "request_path".to_owned(),
                path_schema("Exact request path."),
            );
            properties.insert("script_type".to_owned(), json!({"type":"string","enum":["test","prerequest"],"default":"prerequest","description":"Script kind: test or prerequest; default prerequest."}));
            vec!["request_path"]
        }
        PostmanToolKind::SearchRequests => {
            properties.insert("query".to_owned(), text_schema("Case-insensitive search text; raw stored values are matched but sensitive values are redacted from results.", false));
            properties.insert("search_in".to_owned(), json!({"type":"string","enum":["name","url","description","all"],"default":"all","description":"Search field; default all."}));
            properties.insert(
                "method".to_owned(),
                optional_method("Optional HTTP method filter; null means any method."),
            );
            vec!["query"]
        }
        PostmanToolKind::Analyze => {
            properties.insert("scope".to_owned(), json!({"type":"string","enum":["collection","folder","request"],"default":"collection","description":"Analysis scope; folder or request requires target_path."}));
            properties.insert(
                "target_path".to_owned(),
                optional_path("Exact folder/request path; required by folder/request scope."),
            );
            properties.insert(
                "include_improvements".to_owned(),
                bool_schema(
                    false,
                    "Include deterministic improvement objects; default false.",
                ),
            );
            Vec::new()
        }
        PostmanToolKind::ExecuteRequest => {
            properties.insert(
                "request_path".to_owned(),
                path_schema("Exact stored request path."),
            );
            properties.insert("override_variables".to_owned(), json!({"type":"string","minLength":2,"maxLength":MAX_STRING_BYTES,"default":"{}","description":"JSON object string of scalar variable overrides, for example {\"base_url\":\"https://api.example.com\"}; default {}."}));
            vec!["request_path"]
        }
        _ => {
            return SchemaParts {
                properties,
                required: Vec::new(),
            };
        }
    };
    SchemaParts {
        properties,
        required,
    }
}

fn collection_schema_parts(kind: PostmanToolKind) -> SchemaParts {
    let mut properties = Map::new();
    let required = match kind {
        PostmanToolKind::UpdateCollectionDescription => {
            properties.insert(
                "description".to_owned(),
                text_schema(
                    "New collection description; may be empty to clear text.",
                    true,
                ),
            );
            properties.insert(
                "collection_id".to_owned(),
                optional_id("Collection ID; null uses configured collection."),
            );
            vec!["description"]
        }
        PostmanToolKind::UpdateCollectionVariables => {
            properties.insert("variables".to_owned(), optional_object_array("Replacement collection variable objects; null stores null. Values are secret-bearing and never returned."));
            Vec::new()
        }
        PostmanToolKind::UpdateCollectionAuth => {
            properties.insert(
                "auth".to_owned(),
                optional_object("Authentication object; null clears collection authentication."),
            );
            Vec::new()
        }
        PostmanToolKind::DeleteCollection => {
            properties.insert(
                "collection_id".to_owned(),
                optional_id("Collection ID to delete; null uses configured collection."),
            );
            Vec::new()
        }
        PostmanToolKind::DuplicateCollection => {
            properties.insert(
                "new_name".to_owned(),
                name_schema("Name for the duplicated collection."),
            );
            vec!["new_name"]
        }
        _ => Vec::new(),
    };
    SchemaParts {
        properties,
        required,
    }
}

fn folder_schema_parts(kind: PostmanToolKind) -> SchemaParts {
    let mut properties = Map::new();
    let required = match kind {
        PostmanToolKind::CreateFolder => {
            properties.insert("name".to_owned(), name_schema("New folder name."));
            properties.insert(
                "description".to_owned(),
                optional_text("Optional folder description."),
            );
            properties.insert(
                "parent_path".to_owned(),
                optional_path("Parent folder path; null creates at collection root."),
            );
            properties.insert(
                "auth".to_owned(),
                optional_object(
                    "Optional folder authentication; null creates without folder auth.",
                ),
            );
            vec!["name"]
        }
        PostmanToolKind::UpdateFolder => {
            properties.insert(
                "folder_path".to_owned(),
                path_schema("Exact folder path to update."),
            );
            properties.insert(
                "name".to_owned(),
                optional_name("Optional new folder name."),
            );
            properties.insert(
                "description".to_owned(),
                optional_text("Optional new description; empty clears text."),
            );
            properties.insert(
                "auth".to_owned(),
                optional_object("Optional new folder auth; null means unchanged."),
            );
            vec!["folder_path"]
        }
        PostmanToolKind::DeleteFolder => {
            properties.insert(
                "folder_path".to_owned(),
                path_schema("Exact folder path whose complete subtree will be deleted."),
            );
            vec!["folder_path"]
        }
        PostmanToolKind::MoveFolder => {
            properties.insert(
                "source_path".to_owned(),
                path_schema("Exact current folder path."),
            );
            properties.insert(
                "target_path".to_owned(),
                optional_path("Exact new parent folder path; null means collection root."),
            );
            vec!["source_path"]
        }
        _ => Vec::new(),
    };
    SchemaParts {
        properties,
        required,
    }
}

fn create_request_schema_parts() -> SchemaParts {
    let mut properties = Map::new();
    properties.insert(
        "folder_path".to_owned(),
        optional_path("Destination folder path; null means collection root."),
    );
    properties.insert("name".to_owned(), name_schema("New request name."));
    properties.insert(
        "method".to_owned(),
        method_schema("Stored request HTTP method."),
    );
    properties.insert(
        "url".to_owned(),
        text_schema("Stored request URL; limited to 65536 UTF-8 bytes.", false),
    );
    properties.insert(
        "description".to_owned(),
        optional_text("Optional stored request description."),
    );
    properties.insert(
        "headers".to_owned(),
        optional_object_array("Optional stored header objects; at most 4096."),
    );
    properties.insert(
        "body".to_owned(),
        optional_object("Optional Postman request body object."),
    );
    properties.insert(
        "auth".to_owned(),
        optional_object("Optional Postman authentication object."),
    );
    properties.insert(
        "tests".to_owned(),
        optional_text("Optional test JavaScript, limited to 65536 UTF-8 bytes."),
    );
    properties.insert(
        "pre_request_script".to_owned(),
        optional_text("Optional prerequest JavaScript, limited to 65536 UTF-8 bytes."),
    );
    SchemaParts {
        properties,
        required: vec!["name", "method", "url"],
    }
}

fn request_field_schema_parts(kind: PostmanToolKind) -> SchemaParts {
    let mut properties = Map::new();
    let required = match kind {
        PostmanToolKind::UpdateRequestName => {
            insert_request_pair(&mut properties, "name", name_schema("New request name."));
            vec!["request_path", "name"]
        }
        PostmanToolKind::UpdateRequestMethod => {
            insert_request_pair(
                &mut properties,
                "method",
                method_schema("New stored HTTP method."),
            );
            vec!["request_path", "method"]
        }
        PostmanToolKind::UpdateRequestUrl => {
            insert_request_pair(
                &mut properties,
                "url",
                text_schema("New stored URL.", false),
            );
            vec!["request_path", "url"]
        }
        PostmanToolKind::UpdateRequestDescription => {
            insert_request_pair(
                &mut properties,
                "description",
                text_schema("New description; may be empty.", true),
            );
            vec!["request_path", "description"]
        }
        PostmanToolKind::UpdateRequestHeaders => {
            insert_request_pair(
                &mut properties,
                "headers",
                text_schema(
                    "Newline-separated Header-Name: value text; may be empty.",
                    true,
                ),
            );
            vec!["request_path", "headers"]
        }
        PostmanToolKind::UpdateRequestBody => {
            properties.insert(
                "request_path".to_owned(),
                path_schema("Exact request path."),
            );
            properties.insert("body".to_owned(), json!({
                "anyOf": [
                    {"type":"object","maxProperties":MAX_LIST_ITEMS},
                    {"type":"string","minLength":2,"maxLength":MAX_STRING_BYTES}
                ],
                "description":"Non-null raw, urlencoded, or formdata Postman body object; JSON strings are parsed as objects. Complete arguments are limited to 256 KiB, individual strings to 64 KiB, and nested data to 65536 nodes."
            }));
            vec!["request_path", "body"]
        }
        PostmanToolKind::UpdateRequestAuth => {
            properties.insert(
                "request_path".to_owned(),
                path_schema("Exact request path."),
            );
            properties.insert(
                "auth".to_owned(),
                optional_object("Authentication object; null clears request authentication."),
            );
            vec!["request_path"]
        }
        PostmanToolKind::UpdateRequestTests => {
            insert_request_pair(
                &mut properties,
                "tests",
                text_schema("Replacement test JavaScript; may be empty.", true),
            );
            vec!["request_path", "tests"]
        }
        PostmanToolKind::UpdateRequestPreScript => {
            insert_request_pair(
                &mut properties,
                "pre_request_script",
                text_schema("Replacement prerequest JavaScript; may be empty.", true),
            );
            vec!["request_path", "pre_request_script"]
        }
        _ => Vec::new(),
    };
    SchemaParts {
        properties,
        required,
    }
}

fn request_tree_schema_parts(kind: PostmanToolKind) -> SchemaParts {
    let mut properties = Map::new();
    let required = match kind {
        PostmanToolKind::DeleteRequest => {
            properties.insert(
                "request_path".to_owned(),
                path_schema("Exact request path to delete."),
            );
            vec!["request_path"]
        }
        PostmanToolKind::DuplicateRequest => {
            properties.insert(
                "source_path".to_owned(),
                path_schema("Exact source request path."),
            );
            properties.insert(
                "new_name".to_owned(),
                name_schema("Name for the duplicate."),
            );
            properties.insert(
                "target_path".to_owned(),
                optional_path("Destination folder; null places beside source."),
            );
            vec!["source_path", "new_name"]
        }
        PostmanToolKind::MoveRequest => {
            properties.insert(
                "source_path".to_owned(),
                path_schema("Exact current request path."),
            );
            properties.insert(
                "target_path".to_owned(),
                optional_path("Destination folder; null means collection root."),
            );
            vec!["source_path"]
        }
        _ => Vec::new(),
    };
    SchemaParts {
        properties,
        required,
    }
}

fn insert_request_pair(properties: &mut Map<String, Value>, name: &str, schema: Value) {
    properties.insert(
        "request_path".to_owned(),
        path_schema("Exact request path."),
    );
    properties.insert(name.to_owned(), schema);
}

fn id_schema(description: &str) -> Value {
    json!({"type":"string","minLength":1,"maxLength":MAX_IDENTIFIER_BYTES,"description":description})
}

fn optional_id(description: &str) -> Value {
    json!({"type":["string","null"],"minLength":1,"maxLength":MAX_IDENTIFIER_BYTES,"default":null,"description":description})
}

fn name_schema(description: &str) -> Value {
    id_schema(description)
}

fn optional_name(description: &str) -> Value {
    optional_id(description)
}

fn path_schema(description: &str) -> Value {
    json!({"type":"string","minLength":1,"maxLength":MAX_PATH_BYTES,"description":description})
}

fn optional_path(description: &str) -> Value {
    json!({"type":["string","null"],"minLength":1,"maxLength":MAX_PATH_BYTES,"default":null,"description":description})
}

fn text_schema(description: &str, allow_empty: bool) -> Value {
    json!({"type":"string","minLength":usize::from(!allow_empty),"maxLength":MAX_STRING_BYTES,"description":description})
}

fn optional_text(description: &str) -> Value {
    json!({"type":["string","null"],"minLength":0,"maxLength":MAX_STRING_BYTES,"default":null,"description":description})
}

fn method_schema(description: &str) -> Value {
    json!({"type":"string","minLength":1,"maxLength":32,"pattern":"^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$","description":format!("{description} Any bounded RFC HTTP method token is accepted and stored uppercase, for example GET, TRACE, or a provider extension.")})
}

fn optional_method(description: &str) -> Value {
    json!({"type":["string","null"],"minLength":1,"maxLength":32,"pattern":"^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$","default":null,"description":format!("{description} When present, any bounded RFC HTTP method token is accepted case-insensitively.")})
}

fn optional_object(description: &str) -> Value {
    json!({"type":["object","null"],"default":null,"maxProperties":MAX_LIST_ITEMS,"description":format!("{description} Complete arguments are limited to 256 KiB, JSON strings to 64 KiB, and nested data to 65536 nodes.")})
}

fn optional_object_array(description: &str) -> Value {
    json!({"type":["array","null"],"items":{"type":"object"},"maxItems":MAX_LIST_ITEMS,"default":null,"description":description})
}

fn bool_schema(default: bool, description: &str) -> Value {
    json!({"type":"boolean","default":default,"description":description})
}

fn operation_from_arguments(
    kind: PostmanToolKind,
    arguments: &Map<String, Value>,
) -> adk_core::Result<PostmanOperation> {
    let schema = kind.schema();
    let known = schema["properties"]
        .as_object()
        .map(|properties| properties.keys().map(String::as_str).collect::<Vec<_>>())
        .ok_or_else(invalid_arguments)?;
    reject_unknown_keys(arguments, &known)?;
    match kind {
        PostmanToolKind::GetCollections
        | PostmanToolKind::GetCollection
        | PostmanToolKind::GetFolder
        | PostmanToolKind::GetRequestByPath
        | PostmanToolKind::GetRequestById
        | PostmanToolKind::GetRequestScript
        | PostmanToolKind::SearchRequests
        | PostmanToolKind::Analyze
        | PostmanToolKind::ExecuteRequest => read_operation_from_arguments(kind, arguments),
        PostmanToolKind::UpdateCollectionDescription
        | PostmanToolKind::UpdateCollectionVariables
        | PostmanToolKind::UpdateCollectionAuth
        | PostmanToolKind::DeleteCollection
        | PostmanToolKind::DuplicateCollection
        | PostmanToolKind::CreateFolder
        | PostmanToolKind::UpdateFolder
        | PostmanToolKind::DeleteFolder
        | PostmanToolKind::MoveFolder => organizational_operation_from_arguments(kind, arguments),
        PostmanToolKind::CreateRequest => create_request_operation(arguments),
        _ => request_operation_from_arguments(kind, arguments),
    }
}

fn read_operation_from_arguments(
    kind: PostmanToolKind,
    arguments: &Map<String, Value>,
) -> adk_core::Result<PostmanOperation> {
    Ok(match kind {
        PostmanToolKind::GetCollections => PostmanOperation::GetCollections,
        PostmanToolKind::GetCollection => PostmanOperation::GetCollection {
            collection_id: optional_string(arguments, "collection_id")?,
        },
        PostmanToolKind::GetFolder => PostmanOperation::GetFolder {
            folder_path: required_string(arguments, "folder_path")?,
        },
        PostmanToolKind::GetRequestByPath => PostmanOperation::GetRequestByPath {
            request_path: required_string(arguments, "request_path")?,
        },
        PostmanToolKind::GetRequestById => PostmanOperation::GetRequestById {
            request_id: required_string(arguments, "request_id")?,
        },
        PostmanToolKind::GetRequestScript => PostmanOperation::GetRequestScript {
            request_path: required_string(arguments, "request_path")?,
            script_type: optional_string(arguments, "script_type")?
                .unwrap_or_else(|| "prerequest".to_owned()),
        },
        PostmanToolKind::SearchRequests => PostmanOperation::SearchRequests {
            query: required_string(arguments, "query")?,
            search_in: optional_string(arguments, "search_in")?.unwrap_or_else(|| "all".to_owned()),
            method: optional_string(arguments, "method")?,
        },
        PostmanToolKind::Analyze => PostmanOperation::Analyze {
            scope: optional_string(arguments, "scope")?.unwrap_or_else(|| "collection".to_owned()),
            target_path: optional_string(arguments, "target_path")?,
            include_improvements: optional_bool(arguments, "include_improvements", false)?,
        },
        PostmanToolKind::ExecuteRequest => {
            let encoded = optional_string(arguments, "override_variables")?
                .unwrap_or_else(|| "{}".to_owned());
            let override_variables = serde_json::from_str::<Value>(&encoded)
                .map_err(|_| invalid_arguments())?
                .as_object()
                .cloned()
                .ok_or_else(invalid_arguments)?;
            PostmanOperation::ExecuteRequest {
                request_path: required_string(arguments, "request_path")?,
                override_variables,
            }
        }
        _ => return Err(invalid_arguments()),
    })
}

fn organizational_operation_from_arguments(
    kind: PostmanToolKind,
    arguments: &Map<String, Value>,
) -> adk_core::Result<PostmanOperation> {
    Ok(match kind {
        PostmanToolKind::UpdateCollectionDescription => {
            PostmanOperation::UpdateCollectionDescription {
                description: required_string_allow_empty(arguments, "description")?,
                collection_id: optional_string(arguments, "collection_id")?,
            }
        }
        PostmanToolKind::UpdateCollectionVariables => PostmanOperation::UpdateCollectionVariables {
            variables: optional_object_array_value(arguments, "variables")?,
        },
        PostmanToolKind::UpdateCollectionAuth => PostmanOperation::UpdateCollectionAuth {
            auth: optional_object_value(arguments, "auth")?,
        },
        PostmanToolKind::DeleteCollection => PostmanOperation::DeleteCollection {
            collection_id: optional_string(arguments, "collection_id")?,
        },
        PostmanToolKind::DuplicateCollection => PostmanOperation::DuplicateCollection {
            new_name: required_string(arguments, "new_name")?,
        },
        PostmanToolKind::CreateFolder => PostmanOperation::CreateFolder {
            name: required_string(arguments, "name")?,
            description: optional_string_allow_empty(arguments, "description")?,
            parent_path: optional_string(arguments, "parent_path")?,
            auth: optional_object_value(arguments, "auth")?,
        },
        PostmanToolKind::UpdateFolder => PostmanOperation::UpdateFolder {
            folder_path: required_string(arguments, "folder_path")?,
            name: optional_string(arguments, "name")?,
            description: optional_string_allow_empty(arguments, "description")?,
            auth: optional_object_value(arguments, "auth")?,
        },
        PostmanToolKind::DeleteFolder => PostmanOperation::DeleteFolder {
            folder_path: required_string(arguments, "folder_path")?,
        },
        PostmanToolKind::MoveFolder => PostmanOperation::MoveFolder {
            source_path: required_string(arguments, "source_path")?,
            target_path: optional_string(arguments, "target_path")?,
        },
        _ => return Err(invalid_arguments()),
    })
}

fn create_request_operation(arguments: &Map<String, Value>) -> adk_core::Result<PostmanOperation> {
    Ok(PostmanOperation::CreateRequest {
        folder_path: optional_string(arguments, "folder_path")?,
        name: required_string(arguments, "name")?,
        method: required_string(arguments, "method")?,
        url: required_string(arguments, "url")?,
        description: optional_string_allow_empty(arguments, "description")?,
        headers: optional_object_array_value(arguments, "headers")?,
        body: optional_object_value(arguments, "body")?,
        auth: optional_object_value(arguments, "auth")?,
        tests: optional_string_allow_empty(arguments, "tests")?,
        pre_request_script: optional_string_allow_empty(arguments, "pre_request_script")?,
    })
}

fn request_operation_from_arguments(
    kind: PostmanToolKind,
    arguments: &Map<String, Value>,
) -> adk_core::Result<PostmanOperation> {
    Ok(match kind {
        PostmanToolKind::UpdateRequestName => PostmanOperation::UpdateRequestName {
            request_path: required_string(arguments, "request_path")?,
            name: required_string(arguments, "name")?,
        },
        PostmanToolKind::UpdateRequestMethod => PostmanOperation::UpdateRequestMethod {
            request_path: required_string(arguments, "request_path")?,
            method: required_string(arguments, "method")?,
        },
        PostmanToolKind::UpdateRequestUrl => PostmanOperation::UpdateRequestUrl {
            request_path: required_string(arguments, "request_path")?,
            url: required_string(arguments, "url")?,
        },
        PostmanToolKind::UpdateRequestDescription => PostmanOperation::UpdateRequestDescription {
            request_path: required_string(arguments, "request_path")?,
            description: required_string_allow_empty(arguments, "description")?,
        },
        PostmanToolKind::UpdateRequestHeaders => PostmanOperation::UpdateRequestHeaders {
            request_path: required_string(arguments, "request_path")?,
            headers: required_string_allow_empty(arguments, "headers")?,
        },
        PostmanToolKind::UpdateRequestBody => {
            let body = arguments
                .get("body")
                .cloned()
                .ok_or_else(invalid_arguments)?;
            let body = match body {
                Value::String(encoded) => {
                    serde_json::from_str(&encoded).map_err(|_| invalid_arguments())?
                }
                Value::Object(_) => body,
                _ => return Err(invalid_arguments()),
            };
            PostmanOperation::UpdateRequestBody {
                request_path: required_string(arguments, "request_path")?,
                body,
            }
        }
        PostmanToolKind::UpdateRequestAuth => PostmanOperation::UpdateRequestAuth {
            request_path: required_string(arguments, "request_path")?,
            auth: optional_object_value(arguments, "auth")?,
        },
        PostmanToolKind::UpdateRequestTests => PostmanOperation::UpdateRequestTests {
            request_path: required_string(arguments, "request_path")?,
            tests: required_string_allow_empty(arguments, "tests")?,
        },
        PostmanToolKind::UpdateRequestPreScript => PostmanOperation::UpdateRequestPreScript {
            request_path: required_string(arguments, "request_path")?,
            pre_request_script: required_string_allow_empty(arguments, "pre_request_script")?,
        },
        PostmanToolKind::DeleteRequest => PostmanOperation::DeleteRequest {
            request_path: required_string(arguments, "request_path")?,
        },
        PostmanToolKind::DuplicateRequest => PostmanOperation::DuplicateRequest {
            source_path: required_string(arguments, "source_path")?,
            new_name: required_string(arguments, "new_name")?,
            target_path: optional_string(arguments, "target_path")?,
        },
        PostmanToolKind::MoveRequest => PostmanOperation::MoveRequest {
            source_path: required_string(arguments, "source_path")?,
            target_path: optional_string(arguments, "target_path")?,
        },
        _ => return Err(invalid_arguments()),
    })
}

fn required_string(arguments: &Map<String, Value>, name: &str) -> adk_core::Result<String> {
    let value = required_string_allow_empty(arguments, name)?;
    if value.trim().is_empty() {
        return Err(invalid_arguments());
    }
    Ok(value)
}

fn required_string_allow_empty(
    arguments: &Map<String, Value>,
    name: &str,
) -> adk_core::Result<String> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .map(str::to_owned)
        .ok_or_else(invalid_arguments)
}

fn optional_string(arguments: &Map<String, Value>, name: &str) -> adk_core::Result<Option<String>> {
    optional_string_inner(arguments, name, false)
}

fn optional_string_allow_empty(
    arguments: &Map<String, Value>,
    name: &str,
) -> adk_core::Result<Option<String>> {
    optional_string_inner(arguments, name, true)
}

fn optional_string_inner(
    arguments: &Map<String, Value>,
    name: &str,
    retain_empty: bool,
) -> adk_core::Result<Option<String>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => {
            if retain_empty || !value.trim().is_empty() {
                Ok(Some(value.clone()))
            } else {
                Ok(None)
            }
        }
        Some(_) => Err(invalid_arguments()),
    }
}

fn optional_bool(
    arguments: &Map<String, Value>,
    name: &str,
    default: bool,
) -> adk_core::Result<bool> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(default),
        Some(Value::Bool(value)) => Ok(*value),
        Some(_) => Err(invalid_arguments()),
    }
}

fn optional_object_value(
    arguments: &Map<String, Value>,
    name: &str,
) -> adk_core::Result<Option<Value>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Object(_)) => Ok(arguments.get(name).cloned()),
        Some(_) => Err(invalid_arguments()),
    }
}

fn optional_object_array_value(
    arguments: &Map<String, Value>,
    name: &str,
) -> adk_core::Result<Option<Vec<Value>>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Array(values)) if values.iter().all(Value::is_object) => {
            Ok(Some(values.clone()))
        }
        Some(_) => Err(invalid_arguments()),
    }
}

fn reject_unknown_keys(arguments: &Map<String, Value>, allowed: &[&str]) -> adk_core::Result<()> {
    if arguments.keys().any(|key| !allowed.contains(&key.as_str())) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "postman.arguments.invalid",
        "the Postman tool arguments are invalid",
    )
}

fn resource_exhausted() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "postman.arguments.resource_exhausted",
        "the Postman tool arguments exceed the approved limit",
    )
}
