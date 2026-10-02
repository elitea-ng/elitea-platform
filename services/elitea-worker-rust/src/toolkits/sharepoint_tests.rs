use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Toolset};
use async_trait::async_trait;
use reqwest::header::AUTHORIZATION;
use reqwest::{Request, StatusCode};
use serde_json::{Map, Value, json};

use super::DelegatedAuthorizationRequirement;
use super::delegated_auth::delegated_authorization_requirement;
use super::families::sharepoint::client::{
    SharePointApi, SharePointClient, SharePointClientError, SharePointClientErrorCode,
    SharePointFileListOptions, SharePointHttpResponse, SharePointTransport, normalize_patterns,
};
use super::families::sharepoint::config::{SharePointConfigErrorCode, SharePointToolkitConfig};
use super::families::sharepoint::tools::{
    SharePointToolsetErrorCode, build_sharepoint_toolset, test_build_with_api,
};
use super::materialize::materialize_configured_toolsets_with_tokens_and_authorization;
use super::policy::ToolAdmissionPolicy;
use super::snapshot::FrozenToolSnapshot;

const SITE_URL: &str = "https://tenant.sharepoint.com/teams/project";
const DISCOVERY: &str = "https://login.microsoftonline.com/tenant";

fn settings(selected: &[&str]) -> Map<String, Value> {
    json!({
        "sharepoint_configuration":{
            "client_id":"client-id",
            "client_secret":"stored-client-secret",
            "site_url":"https://tenant.sharepoint.com/sites/default",
            "oauth_discovery_endpoint":DISCOVERY,
            "configuration_uuid":"00000000-0000-4000-8000-000000000123",
            "scopes":["Sites.Read.All","Notes.Read"]
        },
        "site_path":"teams/project",
        "selected_tools":selected
    })
    .as_object()
    .expect("SharePoint settings")
    .clone()
}

fn tokens(key: &str) -> Map<String, Value> {
    Map::from_iter([(
        key.to_owned(),
        json!({"access_token":"claim-fetched-sharepoint-token"}),
    )])
}

fn policy(blocked: &[&str]) -> Arc<ToolAdmissionPolicy> {
    let blocked = if blocked.is_empty() {
        BTreeMap::new()
    } else {
        BTreeMap::from([(
            "sharepoint".to_owned(),
            blocked.iter().map(ToString::to_string).collect(),
        )])
    };
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("SharePoint policy"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("sharepoint-test")
            .with_session_id("session-1")
            .with_function_call_id("call-1"),
    )
}

#[test]
fn delegated_config_resolves_site_path_and_sdk_token_key_precedence() {
    let composite = format!("00000000-0000-4000-8000-000000000123:{DISCOVERY}");
    let config = SharePointToolkitConfig::parse(
        "Project SharePoint",
        &settings(&["get_lists"]),
        &tokens(&composite),
    )
    .expect("composite delegated token");
    assert_eq!(config.site_url().as_str(), SITE_URL);
    assert!(!config.requires_authorization());
    assert_eq!(config.authorization().toolkit_type(), "sharepoint");
    assert_eq!(config.authorization().server_url(), SITE_URL);
    assert_eq!(
        config.authorization().resource_metadata_url(),
        Some("https://login.microsoftonline.com/tenant/v2.0/.well-known/openid-configuration")
    );

    for fallback in [DISCOVERY, SITE_URL] {
        let config = SharePointToolkitConfig::parse(
            "Project SharePoint",
            &settings(&["get_lists"]),
            &tokens(fallback),
        )
        .expect("delegated token fallback");
        assert!(!config.requires_authorization());
    }

    let mut trailing_discovery = settings(&["get_lists"]);
    trailing_discovery
        .get_mut("sharepoint_configuration")
        .and_then(Value::as_object_mut)
        .expect("SharePoint configuration")["oauth_discovery_endpoint"] =
        Value::String(format!("{DISCOVERY}/"));
    let trailing_composite = format!("00000000-0000-4000-8000-000000000123:{DISCOVERY}/");
    let config = SharePointToolkitConfig::parse(
        "Project SharePoint",
        &trailing_discovery,
        &tokens(&trailing_composite),
    )
    .expect("exact configured discovery key");
    assert!(!config.requires_authorization());

    let mut trailing_site = settings(&["get_lists"]);
    trailing_site.remove("site_path");
    trailing_site
        .get_mut("sharepoint_configuration")
        .and_then(Value::as_object_mut)
        .expect("SharePoint configuration")["site_url"] = Value::String(format!("{SITE_URL}/"));
    let config = SharePointToolkitConfig::parse(
        "Project SharePoint",
        &trailing_site,
        &tokens(&format!("{SITE_URL}/")),
    )
    .expect("exact configured site key");
    assert!(!config.requires_authorization());
}

#[test]
fn app_only_empty_and_content_profiles_fail_closed_without_secrets() {
    let mut app_only = settings(&["get_lists"]);
    app_only
        .get_mut("sharepoint_configuration")
        .and_then(Value::as_object_mut)
        .expect("SharePoint configuration")
        .remove("oauth_discovery_endpoint");
    let Err(error) = SharePointToolkitConfig::parse("SharePoint", &app_only, &Map::new()) else {
        panic!("app-only SharePoint needs its own admitted client");
    };
    assert_eq!(
        error.code(),
        SharePointConfigErrorCode::UnsupportedCapability
    );
    assert!(!format!("{error:?} {error}").contains("stored-client-secret"));

    let empty = SharePointToolkitConfig::parse("SharePoint", &settings(&[]), &Map::new())
        .expect("guarded empty configuration");
    let Err(error) = build_sharepoint_toolset("SharePoint", empty, &policy(&[])) else {
        panic!("empty means the complete SDK catalog and cannot be partially admitted");
    };
    assert_eq!(
        error.code(),
        SharePointToolsetErrorCode::UnsupportedSelection
    );

    let content =
        SharePointToolkitConfig::parse("SharePoint", &settings(&["read_document"]), &Map::new())
            .expect("guarded content configuration");
    let Err(error) = build_sharepoint_toolset("SharePoint", content, &policy(&[])) else {
        panic!("artifact-dependent reads must remain closed");
    };
    assert_eq!(
        error.code(),
        SharePointToolsetErrorCode::UnsupportedSelection
    );
}

#[tokio::test]
async fn mixed_selection_exposes_supported_reads_and_keeps_deferred_tools_closed() {
    let config = SharePointToolkitConfig::parse(
        "Project SharePoint",
        &settings(&[
            "index_data",
            "get_files_list",
            "read_document",
            "get_lists",
            "upload_file",
        ]),
        &Map::new(),
    )
    .expect("mixed SharePoint configuration");
    let materialized = build_sharepoint_toolset("Project SharePoint", config, &policy(&[]))
        .expect("supported SharePoint subset");
    assert_eq!(
        materialized
            .delegated_authorization
            .tool_names()
            .collect::<Vec<_>>(),
        ["get_files_list", "get_lists"]
    );

    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = materialized
        .toolset
        .tools(readonly)
        .await
        .expect("mixed SharePoint catalog");
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        ["get_files_list", "get_lists"]
    );
}

#[tokio::test]
async fn missing_token_preserves_original_tools_schemas_and_policy() {
    let config = SharePointToolkitConfig::parse(
        "Project SharePoint",
        &settings(&["get_lists", "onenote_get_pages"]),
        &Map::new(),
    )
    .expect("guarded SharePoint configuration");
    let materialized =
        build_sharepoint_toolset("Project SharePoint", config, &policy(&["get_lists"]))
            .expect("guarded SharePoint tools");
    assert_eq!(
        materialized
            .delegated_authorization
            .tool_names()
            .collect::<Vec<_>>(),
        ["onenote_get_pages"]
    );
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = materialized
        .toolset
        .tools(readonly)
        .await
        .expect("guarded SharePoint catalog");
    assert_eq!(tools.len(), 1);
    let tool = &tools[0];
    assert_eq!(tool.name(), "onenote_get_pages");
    assert!(tool.description().contains(SITE_URL));
    assert_eq!(
        tool.parameters_schema().expect("page schema")["required"],
        json!(["section_id"])
    );
    let error = tool
        .execute(context(), json!({"section_id":"private-section"}))
        .await
        .expect_err("authorization required");
    let requirement = delegated_authorization_requirement(&error).expect("typed auth requirement");
    assert_eq!(requirement.server_url(), SITE_URL);
    assert!(!format!("{error:?} {error}").contains("private-section"));
}

struct FixtureTransport {
    requests: Mutex<Vec<Request>>,
    responses: Mutex<Vec<SharePointHttpResponse>>,
}

#[async_trait]
impl SharePointTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
    ) -> Result<SharePointHttpResponse, SharePointClientError> {
        self.requests
            .lock()
            .expect("SharePoint requests")
            .push(request);
        Ok(self
            .responses
            .lock()
            .expect("SharePoint responses")
            .pop()
            .expect("SharePoint response"))
    }
}

fn response(
    status: StatusCode,
    content_type: &str,
    body: impl serde::Serialize,
) -> SharePointHttpResponse {
    SharePointHttpResponse {
        status,
        content_type: Some(content_type.into()),
        body: serde_json::to_vec(&body).expect("fixture JSON"),
    }
}

fn authorized_client(
    selected: &[&str],
    responses_in_request_order: Vec<SharePointHttpResponse>,
) -> (SharePointClient, Arc<FixtureTransport>) {
    let config = SharePointToolkitConfig::parse(
        "Project SharePoint",
        &settings(selected),
        &tokens(SITE_URL),
    )
    .expect("authorized SharePoint configuration");
    let mut responses = responses_in_request_order;
    responses.reverse();
    let transport = Arc::new(FixtureTransport {
        requests: Mutex::new(Vec::new()),
        responses: Mutex::new(responses),
    });
    let client = SharePointClient::with_transport(
        config.into_client_parts().expect("client"),
        transport.clone(),
    );
    (client, transport)
}

fn site_response() -> SharePointHttpResponse {
    response(
        StatusCode::OK,
        "application/json; charset=utf-8",
        json!({"id":"tenant.sharepoint.com,site-id,web-id"}),
    )
}

#[tokio::test]
async fn graph_list_projection_uses_exact_origin_and_sensitive_bearer() {
    let (client, transport) = authorized_client(
        &["get_lists"],
        vec![
            site_response(),
            response(
                StatusCode::OK,
                "application/json",
                json!({"value":[
                    {"id":"tasks","displayName":"Tasks","description":"Work","list":{"hidden":false,"template":"genericList","itemCount":3}},
                    {"id":"docs","displayName":"Documents","description":"","list":{"hidden":false,"template":"documentLibrary","itemCount":5}},
                    {"id":"hidden","displayName":"Hidden","description":"","list":{"hidden":true,"template":"genericList","itemCount":1}}
                ]}),
            ),
        ],
    );
    let result = client.get_lists().await.expect("SharePoint lists");
    assert_eq!(result.as_array().expect("list result").len(), 1);
    assert_eq!(result[0]["Title"], "Tasks");

    let requests = transport.requests.lock().expect("SharePoint requests");
    assert_eq!(requests.len(), 2);
    assert_eq!(
        requests[0].url().as_str(),
        "https://graph.microsoft.com/v1.0/sites/tenant.sharepoint.com:/teams/project"
    );
    assert_eq!(requests[1].url().host_str(), Some("graph.microsoft.com"));
    assert!(
        requests[1]
            .url()
            .query()
            .expect("list query")
            .contains("%24select")
    );
    for request in requests.iter() {
        let authorization = request.headers().get(AUTHORIZATION).expect("Graph bearer");
        assert_eq!(authorization, "Bearer claim-fetched-sharepoint-token");
        assert!(authorization.is_sensitive());
    }
}

#[tokio::test]
async fn list_pagination_preserves_provider_next_link_and_exact_limit() {
    let next_link = "https://graph.microsoft.com/v1.0/sites/tenant.sharepoint.com%2Csite-id%2Cweb-id/lists/list-1/items?%24skiptoken=opaque";
    let (client, transport) = authorized_client(
        &["read_list"],
        vec![
            site_response(),
            response(
                StatusCode::OK,
                "application/json",
                json!({"value":[{"id":"list-1","displayName":"Tasks","name":"Tasks"}]}),
            ),
            response(
                StatusCode::OK,
                "application/json",
                json!({"value":[{"fields":{"Title":"One"}}],"@odata.nextLink":next_link}),
            ),
            response(
                StatusCode::OK,
                "application/json",
                json!({"value":[{"fields":{"Title":"Two"}}]}),
            ),
        ],
    );
    let result = client.read_list("tasks", 2).await.expect("list items");
    assert_eq!(result, json!([{"Title":"One"},{"Title":"Two"}]));
    let requests = transport.requests.lock().expect("SharePoint requests");
    assert_eq!(requests.len(), 4);
    assert_eq!(requests[3].url().as_str(), next_link);
}

#[tokio::test]
async fn file_metadata_walk_is_bounded_filtered_and_never_downloads_content() {
    let (client, transport) = authorized_client(
        &["get_files_list"],
        vec![
            site_response(),
            response(
                StatusCode::OK,
                "application/json",
                json!({"value":[{"id":"drive-1","name":"Shared Documents","webUrl":"https://tenant.sharepoint.com/teams/project/Shared%20Documents"}]}),
            ),
            response(
                StatusCode::OK,
                "application/json",
                json!({"value":[
                    {"id":"folder-1","name":"nested","folder":{}},
                    {"id":"file-1","name":"report.pdf","file":{},"parentReference":{"path":"/drives/drive-1/root:"},"webUrl":"https://tenant.sharepoint.com/report.pdf","createdDateTime":"2026-01-01T00:00:00Z","lastModifiedDateTime":"2026-01-02T00:00:00Z"},
                    {"id":"file-2","name":"image.png","file":{},"parentReference":{"path":"/drives/drive-1/root:"},"webUrl":"https://tenant.sharepoint.com/image.png","createdDateTime":"","lastModifiedDateTime":""}
                ]}),
            ),
            response(
                StatusCode::OK,
                "application/json",
                json!({"value":[
                    {"id":"file-3","name":"notes.docx","file":{},"parentReference":{"path":"/drives/drive-1/root:/nested"},"webUrl":"https://tenant.sharepoint.com/notes.docx","createdDateTime":"","lastModifiedDateTime":""}
                ]}),
            ),
        ],
    );
    let result = client
        .get_files_list(SharePointFileListOptions {
            folder_name: None,
            form_name: Some("shared documents".to_owned()),
            limit: 10,
            include_patterns: normalize_patterns(Some(&["pdf".to_owned(), "*.docx".to_owned()]))
                .expect("include patterns"),
            skip_patterns: Vec::new(),
        })
        .await
        .expect("file metadata");
    assert_eq!(result.as_array().expect("files").len(), 2);
    assert_eq!(result[0]["Name"], "report.pdf");
    assert_eq!(result[1]["Name"], "notes.docx");
    let requests = transport.requests.lock().expect("SharePoint requests");
    assert_eq!(requests.len(), 4);
    assert!(
        requests
            .iter()
            .all(|request| !request.url().path().ends_with("/content"))
    );
}

#[tokio::test]
async fn file_library_path_selects_exact_drive_and_becomes_drive_relative() {
    let (client, transport) = authorized_client(
        &["get_files_list"],
        vec![
            site_response(),
            response(
                StatusCode::OK,
                "application/json",
                json!({"value":[
                    {"id":"shared","name":"Documents","webUrl":"https://tenant.sharepoint.com/teams/project/Shared%20Documents"},
                    {"id":"private","name":"Private Docs","webUrl":"https://tenant.sharepoint.com/teams/project/Private%20Docs"}
                ]}),
            ),
            response(
                StatusCode::OK,
                "application/json",
                json!({"value":[
                    {"id":"file-1","name":"plan.pdf","file":{},"parentReference":{"path":"/drives/private/root:/archive"},"webUrl":"https://tenant.sharepoint.com/plan.pdf","createdDateTime":"","lastModifiedDateTime":""}
                ]}),
            ),
        ],
    );
    let result = client
        .get_files_list(SharePointFileListOptions {
            folder_name: Some("/teams/project/Private%20Docs/archive".to_owned()),
            form_name: None,
            limit: 10,
            include_patterns: Vec::new(),
            skip_patterns: Vec::new(),
        })
        .await
        .expect("private library files");
    assert_eq!(result[0]["Name"], "plan.pdf");
    let requests = transport.requests.lock().expect("SharePoint requests");
    assert_eq!(requests.len(), 3);
    assert_eq!(
        requests[2].url().path(),
        "/v1.0/drives/private/root:/archive:/children"
    );
}

struct AuthenticationApi {
    requirement: DelegatedAuthorizationRequirement,
}

impl AuthenticationApi {
    fn error() -> SharePointClientError {
        SharePointClientError::fixture(SharePointClientErrorCode::Authentication)
    }
}

#[async_trait]
impl SharePointApi for AuthenticationApi {
    fn authorization(&self) -> &DelegatedAuthorizationRequirement {
        &self.requirement
    }

    async fn read_list(&self, _: &str, _: usize) -> Result<Value, SharePointClientError> {
        Err(Self::error())
    }

    async fn get_lists(&self) -> Result<Value, SharePointClientError> {
        Err(Self::error())
    }

    async fn get_list_columns(&self, _: &str) -> Result<Value, SharePointClientError> {
        Err(Self::error())
    }

    async fn get_files_list(
        &self,
        _: SharePointFileListOptions,
    ) -> Result<Value, SharePointClientError> {
        Err(Self::error())
    }

    async fn onenote_get_notebooks(
        &self,
        _: Option<&[String]>,
    ) -> Result<Value, SharePointClientError> {
        Err(Self::error())
    }

    async fn onenote_get_sections(
        &self,
        _: &str,
        _: Option<&[String]>,
    ) -> Result<Value, SharePointClientError> {
        Err(Self::error())
    }

    async fn onenote_get_pages(
        &self,
        _: &str,
        _: usize,
        _: Option<&[String]>,
    ) -> Result<Value, SharePointClientError> {
        Err(Self::error())
    }

    async fn onenote_get_page_content(&self, _: &str) -> Result<Value, SharePointClientError> {
        Err(Self::error())
    }
}

#[tokio::test]
async fn runtime_401_becomes_the_common_typed_authorization_signal() {
    let requirement = DelegatedAuthorizationRequirement::new(
        "Project SharePoint".to_owned(),
        "sharepoint".to_owned(),
        SITE_URL.to_owned(),
        None,
        None,
    )
    .expect("SharePoint requirement");
    let client: Arc<dyn SharePointApi> = Arc::new(AuthenticationApi { requirement });
    let toolset = test_build_with_api(
        "Project SharePoint",
        SITE_URL,
        &["get_lists".to_owned()],
        &policy(&[]),
        &client,
    )
    .expect("401 fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tool = toolset
        .tools(readonly)
        .await
        .expect("401 fixture tools")
        .pop()
        .expect("get_lists");
    let error = tool
        .execute(context(), json!({}))
        .await
        .expect_err("401 must request authorization");
    let requirement = delegated_authorization_requirement(&error).expect("typed 401 requirement");
    assert_eq!(requirement.server_url(), SITE_URL);
}

#[tokio::test]
async fn configured_materializer_merges_sharepoint_with_the_common_auth_catalog() {
    let version = json!({
        "tools":[{
            "id":81,
            "type":"sharepoint",
            "toolkit_name":"Project SharePoint",
            "settings":settings(&["get_lists"])
        }]
    });
    let version = version.as_object().expect("version details");
    let snapshot = FrozenToolSnapshot::from_version_details(version)
        .expect("SharePoint snapshot")
        .apply_policy(policy(&[]).as_ref());
    let (toolsets, authorization) = materialize_configured_toolsets_with_tokens_and_authorization(
        &snapshot,
        &policy(&[]),
        &Map::new(),
    )
    .await
    .expect("SharePoint materializer");
    assert_eq!(toolsets.len(), 1);
    assert_eq!(
        authorization.tool_names().collect::<Vec<_>>(),
        ["get_lists"]
    );
}
