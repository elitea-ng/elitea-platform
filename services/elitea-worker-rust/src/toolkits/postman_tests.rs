//! Focused compatibility and safety tests for the capability-disabled Postman family.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, ToolContext, Toolset};
use async_trait::async_trait;
use reqwest::header::{CONTENT_TYPE, HeaderValue};
use reqwest::{Method, StatusCode};
use serde_json::{Map, Value, json};

use super::families::postman::analysis::analyze;
use super::families::postman::client::{
    DynamicEgressAuthority, DynamicRequest, DynamicResponse, ManagementHttpResponse,
    ManagementTransport, ManagementTransportError, PostmanApi, PostmanClient, PostmanClientError,
    PostmanClientErrorCode, PostmanOperation, test_dynamic_header_value, test_dynamic_parts,
    test_expand_variables, test_management_request, test_map_status, test_redact_request,
};
use super::families::postman::config::{PostmanConfigErrorCode, PostmanToolkitConfig};
use super::families::postman::tools::{
    PostmanToolsetErrorCode, build_postman_toolset, test_build_with_api, test_catalog,
};
use super::policy::ToolAdmissionPolicy;

const API_KEY: &str = "postman-private-api-key";

fn settings(selected: Option<Value>) -> Map<String, Value> {
    let mut settings = json!({
        "postman_configuration": {
            "base_url": "https://api.postman.example.test",
            "workspace_id": "workspace/one",
            "api_key": API_KEY
        },
        "collection_id": "collection/one",
        "environment_config": {
            "values": [{"key":"base_url","value":"https://service.example.test","enabled":true}],
            "auth": {"type":"bearer","bearer":[{"key":"token","value":"environment-secret"}]}
        }
    })
    .as_object()
    .cloned()
    .expect("Postman fixture is an object");
    if let Some(selected) = selected {
        settings.insert("selected_tools".to_owned(), selected);
    }
    settings
}

fn config() -> PostmanToolkitConfig {
    PostmanToolkitConfig::parse(&settings(Some(json!([])))).expect("valid Postman configuration")
}

fn policy(blocked: &[(&str, &[&str])]) -> Arc<ToolAdmissionPolicy> {
    let blocked = blocked
        .iter()
        .map(|(toolkit, tools)| {
            (
                (*toolkit).to_owned(),
                tools.iter().map(|tool| (*tool).to_owned()).collect(),
            )
        })
        .collect::<BTreeMap<_, _>>();
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Postman policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("postman-test").with_function_call_id("postman-call"))
}

#[test]
fn catalog_preserves_exact_source_order_and_groups() {
    assert_eq!(
        test_catalog(),
        vec![
            ("get_collections", "read"),
            ("get_collection", "read"),
            ("get_folder", "read"),
            ("get_request_by_path", "read"),
            ("get_request_by_id", "read"),
            ("get_request_script", "read"),
            ("search_requests", "read"),
            ("analyze", "read"),
            ("execute_request", "execute"),
            ("update_collection_description", "write"),
            ("update_collection_variables", "write"),
            ("update_collection_auth", "write"),
            ("delete_collection", "delete"),
            ("duplicate_collection", "write"),
            ("create_folder", "write"),
            ("update_folder", "write"),
            ("delete_folder", "delete"),
            ("move_folder", "write"),
            ("create_request", "write"),
            ("update_request_name", "write"),
            ("update_request_method", "write"),
            ("update_request_url", "write"),
            ("update_request_description", "write"),
            ("update_request_headers", "write"),
            ("update_request_body", "write"),
            ("update_request_auth", "write"),
            ("update_request_tests", "write"),
            ("update_request_pre_script", "write"),
            ("delete_request", "delete"),
            ("duplicate_request", "write"),
            ("move_request", "write"),
        ]
    );
    let catalog = test_catalog();
    assert_eq!(
        catalog.iter().filter(|(_, group)| *group == "read").count(),
        8
    );
    assert_eq!(
        catalog
            .iter()
            .filter(|(_, group)| *group == "execute")
            .count(),
        1
    );
    assert_eq!(
        catalog
            .iter()
            .filter(|(_, group)| *group == "write")
            .count(),
        19
    );
    assert_eq!(
        catalog
            .iter()
            .filter(|(_, group)| *group == "delete")
            .count(),
        3
    );
}

#[test]
fn nested_configuration_is_exact_origin_secret_safe_and_selection_is_deterministic() {
    for selected in [None, Some(Value::Null), Some(json!([]))] {
        let parsed = PostmanToolkitConfig::parse(&settings(selected))
            .expect("empty selection means all tools");
        assert!(parsed.selected_tools().is_empty());
    }
    let selected = PostmanToolkitConfig::parse(&settings(Some(json!([
        "move_request",
        "get_collection",
        "move_request"
    ]))))
    .expect("duplicates are order-preserving and deduplicated");
    assert_eq!(
        selected
            .selected_tools()
            .iter()
            .map(AsRef::as_ref)
            .collect::<Vec<&str>>(),
        ["move_request", "get_collection"]
    );

    for invalid in [
        "http://api.postman.example.test",
        "https://user@api.postman.example.test",
        "https://api.postman.example.test/path",
        "https://api.postman.example.test?x=y",
        "https://api.postman.example.test#fragment",
    ] {
        let mut value = settings(Some(json!([])));
        value
            .get_mut("postman_configuration")
            .and_then(Value::as_object_mut)
            .expect("nested config")
            .insert("base_url".to_owned(), Value::String(invalid.to_owned()));
        let Err(error) = PostmanToolkitConfig::parse(&value) else {
            panic!("invalid origin must fail");
        };
        assert_eq!(error.code(), PostmanConfigErrorCode::InvalidConfiguration);
        assert!(!format!("{error:?}").contains(API_KEY));
    }
    let mut invalid_secret = settings(Some(json!([])));
    invalid_secret
        .get_mut("postman_configuration")
        .and_then(Value::as_object_mut)
        .expect("nested config")
        .insert(
            "api_key".to_owned(),
            Value::String("secret\0tail".to_owned()),
        );
    let Err(error) = PostmanToolkitConfig::parse(&invalid_secret) else {
        panic!("control-bearing secret must fail");
    };
    assert!(!format!("{error:?}").contains("secret"));
}

struct FixtureApi {
    calls: Mutex<Vec<String>>,
    responses: Mutex<VecDeque<Result<Value, PostmanClientError>>>,
}

#[derive(Clone, Debug)]
struct RecordedRequest {
    method: Method,
    url: String,
    body: Option<Value>,
}

struct FixtureManagementTransport {
    calls: Mutex<Vec<RecordedRequest>>,
    responses: Mutex<VecDeque<Result<ManagementHttpResponse, ManagementTransportError>>>,
}

impl FixtureManagementTransport {
    fn new(responses: impl IntoIterator<Item = ManagementHttpResponse>) -> Self {
        Self {
            calls: Mutex::new(Vec::new()),
            responses: Mutex::new(responses.into_iter().map(Ok).collect()),
        }
    }

    fn calls(&self) -> Vec<RecordedRequest> {
        self.calls.lock().expect("management calls").clone()
    }
}

#[async_trait]
impl ManagementTransport for FixtureManagementTransport {
    async fn send(
        &self,
        request: reqwest::Request,
    ) -> Result<ManagementHttpResponse, ManagementTransportError> {
        let body = request
            .body()
            .and_then(|body| body.as_bytes())
            .map(serde_json::from_slice)
            .transpose()
            .map_err(|_| ManagementTransportError::Response)?;
        self.calls
            .lock()
            .expect("management calls")
            .push(RecordedRequest {
                method: request.method().clone(),
                url: request.url().to_string(),
                body,
            });
        self.responses
            .lock()
            .expect("management responses")
            .pop_front()
            .unwrap_or_else(|| Ok(json_response(StatusCode::OK, json!({}))))
    }
}

fn json_response(status: StatusCode, body: impl Into<Value>) -> ManagementHttpResponse {
    let body = body.into();
    ManagementHttpResponse {
        status,
        body: serde_json::to_vec(&body).expect("JSON fixture"),
    }
}

fn collection_response() -> Value {
    json!({
        "collection": {
            "info":{"_postman_id":"collection/one","name":"Fixture"},
            "item":[
                {"id":"folder-1","name":"Folder","item":[
                    {"id":"request-1","name":"Request","request":{"method":"GET","url":"https://service.example.test/items","body":{"mode":"raw","raw":"{\"id\":42,\"nested\":{\"id\":9}}"},"auth":{"type":"custom","id":"business-auth-id"}},"event":[{"listen":"prerequest","script":{"exec":["old pre"]}}]}
                ]},
                {"id":"target-1","name":"Target","item":[]}
            ]
        }
    })
}

fn client_with_transport(
    responses: impl IntoIterator<Item = ManagementHttpResponse>,
) -> (PostmanClient, Arc<FixtureManagementTransport>) {
    let transport = Arc::new(FixtureManagementTransport::new(responses));
    let transport_trait: Arc<dyn ManagementTransport> = transport.clone();
    let client = PostmanClient::fixture_with_management_transport(config(), transport_trait)
        .expect("fixture client");
    (client, transport)
}

#[derive(Clone, Debug)]
struct RecordedDynamicRequest {
    method: Method,
    url: String,
    headers: Vec<(String, String)>,
    query: Vec<(String, String)>,
    body: Option<Vec<u8>>,
}

struct FixtureDynamicAuthority {
    calls: Mutex<Vec<RecordedDynamicRequest>>,
    responses: Mutex<VecDeque<Result<DynamicResponse, PostmanClientError>>>,
}

impl FixtureDynamicAuthority {
    fn new(
        responses: impl IntoIterator<Item = Result<DynamicResponse, PostmanClientError>>,
    ) -> Self {
        Self {
            calls: Mutex::new(Vec::new()),
            responses: Mutex::new(responses.into_iter().collect()),
        }
    }
}

#[async_trait]
impl DynamicEgressAuthority for FixtureDynamicAuthority {
    async fn dispatch(
        &self,
        request: DynamicRequest,
    ) -> Result<DynamicResponse, PostmanClientError> {
        let headers = request
            .headers
            .iter()
            .map(|(name, value)| {
                value
                    .to_str()
                    .map(|value| (name.to_string(), value.to_owned()))
                    .map_err(|_| {
                        PostmanClientError::fixture(PostmanClientErrorCode::InvalidInput, false)
                    })
            })
            .collect::<Result<Vec<_>, _>>()?;
        self.calls
            .lock()
            .expect("dynamic calls")
            .push(RecordedDynamicRequest {
                method: request.method.clone(),
                url: request.url.to_string(),
                headers,
                query: request.query.clone(),
                body: request.body.clone(),
            });
        self.responses
            .lock()
            .expect("dynamic responses")
            .pop_front()
            .unwrap_or_else(|| {
                Ok(DynamicResponse {
                    status: StatusCode::OK,
                    reason: "OK".into(),
                    headers: Map::new(),
                    body: json!({"ok":true}),
                    size_bytes: 11,
                })
            })
    }
}

impl FixtureApi {
    fn new(responses: impl IntoIterator<Item = Result<Value, PostmanClientError>>) -> Self {
        Self {
            calls: Mutex::new(Vec::new()),
            responses: Mutex::new(responses.into_iter().collect()),
        }
    }
}

#[async_trait]
impl PostmanApi for FixtureApi {
    async fn execute(&self, operation: PostmanOperation) -> Result<Value, PostmanClientError> {
        let name = operation_name(&operation);
        self.calls
            .lock()
            .expect("Postman fixture calls")
            .push(name.to_owned());
        self.responses
            .lock()
            .expect("Postman fixture responses")
            .pop_front()
            .unwrap_or_else(|| Ok(json!({"tool":name})))
    }
}

fn operation_name(operation: &PostmanOperation) -> &'static str {
    match operation {
        PostmanOperation::GetCollections => "get_collections",
        PostmanOperation::GetCollection { .. } => "get_collection",
        PostmanOperation::GetFolder { .. } => "get_folder",
        PostmanOperation::GetRequestByPath { .. } => "get_request_by_path",
        PostmanOperation::GetRequestById { .. } => "get_request_by_id",
        PostmanOperation::GetRequestScript { .. } => "get_request_script",
        PostmanOperation::SearchRequests { .. } => "search_requests",
        PostmanOperation::Analyze { .. } => "analyze",
        PostmanOperation::ExecuteRequest { .. } => "execute_request",
        PostmanOperation::UpdateCollectionDescription { .. } => "update_collection_description",
        PostmanOperation::UpdateCollectionVariables { .. } => "update_collection_variables",
        PostmanOperation::UpdateCollectionAuth { .. } => "update_collection_auth",
        PostmanOperation::DeleteCollection { .. } => "delete_collection",
        PostmanOperation::DuplicateCollection { .. } => "duplicate_collection",
        PostmanOperation::CreateFolder { .. } => "create_folder",
        PostmanOperation::UpdateFolder { .. } => "update_folder",
        PostmanOperation::DeleteFolder { .. } => "delete_folder",
        PostmanOperation::MoveFolder { .. } => "move_folder",
        PostmanOperation::CreateRequest { .. } => "create_request",
        PostmanOperation::UpdateRequestName { .. } => "update_request_name",
        PostmanOperation::UpdateRequestMethod { .. } => "update_request_method",
        PostmanOperation::UpdateRequestUrl { .. } => "update_request_url",
        PostmanOperation::UpdateRequestDescription { .. } => "update_request_description",
        PostmanOperation::UpdateRequestHeaders { .. } => "update_request_headers",
        PostmanOperation::UpdateRequestBody { .. } => "update_request_body",
        PostmanOperation::UpdateRequestAuth { .. } => "update_request_auth",
        PostmanOperation::UpdateRequestTests { .. } => "update_request_tests",
        PostmanOperation::UpdateRequestPreScript { .. } => "update_request_pre_script",
        PostmanOperation::DeleteRequest { .. } => "delete_request",
        PostmanOperation::DuplicateRequest { .. } => "duplicate_request",
        PostmanOperation::MoveRequest { .. } => "move_request",
    }
}

#[tokio::test]
async fn tool_metadata_schemas_selection_and_policy_match_runtime() {
    let api = Arc::new(FixtureApi::new([]));
    let api_trait: Arc<dyn PostmanApi> = api;
    let toolset = test_build_with_api("api-work", &[], &policy(&[]), &api_trait)
        .expect("complete Postman toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Postman tools");
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        test_catalog()
            .iter()
            .map(|(name, _)| *name)
            .collect::<Vec<_>>()
    );
    for tool in &tools {
        let description = tool.description();
        assert!(description.starts_with("Toolkit: api-work\n"));
        assert!(description.len() > 100, "{} description", tool.name());
        let schema = tool.parameters_schema().expect("schema");
        assert_eq!(schema["type"], "object");
        assert_eq!(schema["additionalProperties"], false);
        let properties = schema["properties"].as_object().expect("properties");
        for required in schema["required"].as_array().expect("required") {
            assert!(properties.contains_key(required.as_str().expect("required string")));
        }
        if tool.is_read_only() {
            assert!(tool.is_concurrency_safe());
        } else {
            assert!(!tool.is_concurrency_safe());
            assert!(description.to_ascii_lowercase().contains("unknown outcome"));
            assert!(description.to_ascii_lowercase().contains("do not retry"));
        }
    }
    let body_schema = tools
        .iter()
        .find(|tool| tool.name() == "update_request_body")
        .and_then(|tool| tool.parameters_schema())
        .expect("body schema");
    assert_eq!(
        body_schema["properties"]["body"]["anyOf"][1]["maxLength"],
        65_536
    );

    let selected = vec!["move_request".to_owned(), "get_collection".to_owned()];
    let selected_set = test_build_with_api("api-work", &selected, &policy(&[]), &api_trait)
        .expect("selected toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert_eq!(
        selected_set
            .tools(readonly)
            .await
            .expect("selected tools")
            .iter()
            .map(|tool| tool.name())
            .collect::<Vec<_>>(),
        ["get_collection", "move_request"]
    );

    let mut unknown = settings(Some(json!(["unknown_tool"])));
    let parsed =
        PostmanToolkitConfig::parse(&unknown).expect("selection parses before catalog admission");
    let Err(error) = build_postman_toolset("api-work", parsed, &policy(&[])) else {
        panic!("unknown selection must fail closed");
    };
    assert_eq!(error.code(), PostmanToolsetErrorCode::UnsupportedSelection);
    unknown.clear();
}

#[tokio::test]
async fn source_defaults_and_null_body_admission_are_enforced_before_api() {
    let api = Arc::new(FixtureApi::new([]));
    let api_trait: Arc<dyn PostmanApi> = api.clone();
    let selected = vec![
        "get_request_script".to_owned(),
        "analyze".to_owned(),
        "update_request_body".to_owned(),
    ];
    let toolset = test_build_with_api("api-work", &selected, &policy(&[]), &api_trait)
        .expect("selected tools");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("tools");
    let script = tools
        .iter()
        .find(|tool| tool.name() == "get_request_script")
        .expect("script tool");
    script
        .execute(context(), json!({"request_path":"API/Get"}))
        .await
        .expect("default script type");
    let analyze_tool = tools
        .iter()
        .find(|tool| tool.name() == "analyze")
        .expect("analyze tool");
    analyze_tool
        .execute(context(), json!({}))
        .await
        .expect("default collection analysis");
    let body_tool = tools
        .iter()
        .find(|tool| tool.name() == "update_request_body")
        .expect("body tool");
    assert!(
        body_tool
            .execute(context(), json!({"request_path":"API/Get","body":null}))
            .await
            .is_err()
    );
    assert_eq!(
        api.calls.lock().expect("calls").as_slice(),
        ["get_request_script", "analyze"]
    );
}

fn analysis_fixture() -> Value {
    json!({
        "collection": {
            "info": {
                "_postman_id": "provider-metadata-id",
                "name": "Users API",
                "description": "Collection docs"
            },
            "auth": {"type":"bearer"},
            "variable": [{"key":"name","value":"Ada"}],
            "item": [{
                "name":"Users",
                "description":"User operations",
                "item":[{
                    "id":"request-1",
                    "name":"Create User",
                    "description":"parameter response example auth error",
                    "request": {
                        "method":"POST",
                        "url":"https://user:password@api.example.com/v1/users?token=secret&view=full",
                        "header":[
                            {"key":"Content-Type","value":"application/json"},
                            {"key":"Accept","value":"application/json"},
                            {"key":"Authorization","value":"Bearer stored-secret"}
                        ],
                        "body":{"mode":"raw","raw":"{\"name\":\"{{name}}\"}"},
                        "auth":{"type":"bearer"}
                    },
                    "event":[{"listen":"test","script":{"exec":["pm.response.code === 200", "if (status >= 400) fail"]}}],
                    "response":[{}]
                }]
            }]
        }
    })
}

#[test]
fn analyzer_preserves_collection_folder_request_shapes_and_scoring_with_safe_urls() {
    let fixture = analysis_fixture();
    let request = analyze(
        &fixture,
        "configured-collection",
        "request",
        Some("Users/Create User"),
        true,
    )
    .expect("request analysis");
    assert_eq!(request["collection_id"], "configured-collection");
    assert_eq!(request["name"], "Create User");
    assert_eq!(request["method"], "POST");
    assert_eq!(request["has_auth"], true);
    assert_eq!(request["has_hardcoded_url"], true);
    assert_eq!(request["has_security_issues"], true);
    assert_eq!(request["security_score"], 70);
    assert_eq!(request["performance_score"], 80);
    assert_eq!(request["documentation_quality"], "excellent");
    assert_eq!(request["test_coverage"], "basic");
    let rendered = request.to_string();
    for secret in ["user:password", "v1", "users", "secret", "stored-secret"] {
        assert!(!rendered.contains(secret));
    }
    assert!(rendered.contains("token=<redacted>"));
    assert!(request["improvement_count"].as_u64().is_some());

    let folder = analyze(
        &fixture,
        "configured-collection",
        "folder",
        Some("Users"),
        true,
    )
    .expect("folder analysis");
    assert!(folder.is_array());
    assert_eq!(folder[0]["name"], "Users");
    assert_eq!(folder[0]["request_count"], 1);
    assert_eq!(folder[0]["auth_consistency"], "consistent");
    assert!(folder[0]["improvements"].is_array());

    let collection = analyze(&fixture, "configured-collection", "collection", None, true)
        .expect("collection analysis");
    assert_eq!(collection["collection_id"], "provider-metadata-id");
    assert_eq!(collection["collection_name"], "Users API");
    assert_eq!(collection["total_requests"], 1);
    assert!(collection["folders"].is_array());
    assert!(collection["improvements"].is_array());

    let mut lower = analysis_fixture();
    let request_item = &mut lower["collection"]["item"][0]["item"][0];
    request_item["request"]["method"] = json!("post");
    request_item["request"]
        .as_object_mut()
        .expect("request")
        .remove("auth");
    request_item["request"]["header"] = json!([
        {"key":"Content-Type","value":"application/json"},
        {"key":"Accept","value":"application/json"}
    ]);
    lower["collection"]
        .as_object_mut()
        .expect("collection")
        .remove("auth");
    let lower = analyze(
        &lower,
        "configured-collection",
        "request",
        Some("Users/Create User"),
        false,
    )
    .expect("lowercase mutation method analysis");
    assert_eq!(
        lower["method"], "post",
        "projection preserves provider spelling"
    );
    assert_eq!(
        lower["security_score"], 30,
        "SDK uppercases for mutation penalty"
    );
    assert!(
        lower["issues"]
            .as_array()
            .expect("issues")
            .iter()
            .any(|issue| { issue["message"] == "Sensitive operation without authentication" })
    );
}

#[test]
fn direct_request_redaction_is_context_aware_and_url_safe() {
    let projected = test_redact_request(json!({
        "business": {"value":"keep-business-value"},
        "request": {
            "url":"https://user:password@service.example.test/customer/42?token=secret&view=full#frag",
            "header":[{"key":"X-Key","value":"header-secret"}],
            "variable":[{"key":"token","value":"variable-secret"}],
            "body":{"raw":"body-secret"},
            "auth":{"type":"bearer","token":"auth-secret"}
        },
        "tests":"script-secret"
    }));
    assert_eq!(projected["business"]["value"], "keep-business-value");
    assert_eq!(projected["request"]["header"][0]["value"], "<redacted>");
    assert_eq!(projected["request"]["variable"][0]["value"], "<redacted>");
    assert_eq!(projected["request"]["body"], "<redacted>");
    assert_eq!(projected["tests"], "<redacted>");
    let rendered = projected.to_string();
    for secret in [
        "user:password",
        "customer",
        "42",
        "secret",
        "header-secret",
        "auth-secret",
    ] {
        assert!(!rendered.contains(secret));
    }
    assert!(rendered.contains("token=<redacted>"));
}

#[test]
fn management_requests_use_encoded_components_fixed_authority_and_api_key() {
    let cases = [
        (
            Method::GET,
            vec!["collections"],
            vec![("workspace", "workspace/one")],
            "https://api.postman.example.test/collections?workspace=workspace%2Fone",
        ),
        (
            Method::GET,
            vec!["collections", "collection/one"],
            Vec::new(),
            "https://api.postman.example.test/collections/collection%2Fone",
        ),
        (
            Method::PUT,
            vec!["collections", "c one", "requests", "r/one"],
            Vec::new(),
            "https://api.postman.example.test/collections/c%20one/requests/r%2Fone",
        ),
        (
            Method::PUT,
            vec!["collections", "c one", "folders", "f/one"],
            Vec::new(),
            "https://api.postman.example.test/collections/c%20one/folders/f%2Fone",
        ),
    ];
    for (method, segments, query, expected) in cases {
        let body = (method == Method::PUT).then(|| json!({"name":"updated"}));
        let request =
            test_management_request(config(), method.clone(), &segments, &query, body.as_ref())
                .expect("bounded management request");
        assert_eq!(request.method(), method);
        assert_eq!(request.url().as_str(), expected);
        assert_eq!(
            request.headers().get("x-api-key"),
            Some(&HeaderValue::from_static(API_KEY))
        );
        if let Some(expected_body) = body.as_ref() {
            assert_eq!(
                request.headers().get(CONTENT_TYPE),
                Some(&HeaderValue::from_static("application/json"))
            );
            assert_eq!(
                serde_json::from_slice::<Value>(
                    request
                        .body()
                        .and_then(|body| body.as_bytes())
                        .expect("JSON body")
                )
                .expect("valid JSON"),
                *expected_body
            );
        }
    }
}

#[tokio::test]
async fn collection_effect_routes_bodies_and_identity_stripping_are_exact() {
    let base = collection_response();
    let collection_url = "https://api.postman.example.test/collections/collection%2Fone";
    let cases = [
        (
            PostmanOperation::UpdateCollectionDescription {
                description: "new docs".to_owned(),
                collection_id: None,
            },
            "description",
        ),
        (
            PostmanOperation::UpdateCollectionVariables {
                variables: Some(vec![json!({"key":"project","value":"secret"})]),
            },
            "variable",
        ),
        (
            PostmanOperation::UpdateCollectionAuth {
                auth: Some(json!({"type":"bearer","token":"secret"})),
            },
            "auth",
        ),
    ];
    for (operation, field) in cases {
        let (client, transport) = client_with_transport([
            json_response(StatusCode::OK, base.clone()),
            json_response(StatusCode::OK, json!({})),
            json_response(StatusCode::OK, base.clone()),
        ]);
        client.execute(operation).await.expect("collection update");
        let calls = transport.calls();
        assert_eq!(calls.len(), 3);
        assert_eq!(
            (calls[0].method.clone(), calls[0].url.as_str()),
            (Method::GET, collection_url)
        );
        assert_eq!(
            (calls[1].method.clone(), calls[1].url.as_str()),
            (Method::PUT, collection_url)
        );
        assert_eq!(
            (calls[2].method.clone(), calls[2].url.as_str()),
            (Method::GET, collection_url)
        );
        assert!(
            calls[1].body.as_ref().expect("PUT body")["collection"]
                .get(field)
                .is_some()
                || calls[1].body.as_ref().expect("PUT body")["collection"]["info"]
                    .get(field)
                    .is_some()
        );
    }

    let (client, transport) = client_with_transport([json_response(StatusCode::OK, json!({}))]);
    client
        .execute(PostmanOperation::DeleteCollection {
            collection_id: None,
        })
        .await
        .expect("collection delete");
    let calls = transport.calls();
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0].method, Method::DELETE);
    assert_eq!(calls[0].url, collection_url);
    assert!(calls[0].body.is_none());

    let (client, transport) = client_with_transport([
        json_response(StatusCode::OK, base),
        json_response(StatusCode::OK, json!({"collection":{"id":"copy-id"}})),
    ]);
    client
        .execute(PostmanOperation::DuplicateCollection {
            new_name: "Copy".to_owned(),
        })
        .await
        .expect("collection duplicate");
    let calls = transport.calls();
    assert_eq!(calls.len(), 2);
    assert_eq!(calls[1].method, Method::POST);
    assert_eq!(
        calls[1].url,
        "https://api.postman.example.test/collections?workspace=workspace%2Fone"
    );
    let duplicated = &calls[1].body.as_ref().expect("duplicate body")["collection"];
    assert_eq!(duplicated["info"]["name"], "Copy");
    assert!(duplicated["info"].get("_postman_id").is_none());
    assert!(duplicated["item"][0].get("id").is_none());
    assert!(duplicated["item"][0]["item"][0].get("id").is_none());
    assert_eq!(
        duplicated["item"][0]["item"][0]["request"]["body"]["raw"],
        "{\"id\":42,\"nested\":{\"id\":9}}"
    );
    assert_eq!(
        duplicated["item"][0]["item"][0]["request"]["auth"]["id"],
        "business-auth-id"
    );
}

#[tokio::test]
async fn full_collection_folder_and_request_mutations_use_one_fenced_put() {
    assert_full_collection_mutation_routes().await;
    assert_duplicate_request_preserves_business_ids().await;
    assert_create_request_payload_and_confirmation().await;
}

async fn assert_full_collection_mutation_routes() {
    let base = collection_response();
    let operations = [
        PostmanOperation::CreateFolder {
            name: "Child".to_owned(),
            description: Some("docs".to_owned()),
            parent_path: Some("Target".to_owned()),
            auth: Some(json!({"type":"bearer"})),
        },
        PostmanOperation::DeleteFolder {
            folder_path: "Folder".to_owned(),
        },
        PostmanOperation::MoveFolder {
            source_path: "Folder".to_owned(),
            target_path: Some("Target".to_owned()),
        },
        PostmanOperation::DeleteRequest {
            request_path: "Folder/Request".to_owned(),
        },
        PostmanOperation::DuplicateRequest {
            source_path: "Folder/Request".to_owned(),
            new_name: "Copy".to_owned(),
            target_path: Some("Target".to_owned()),
        },
        PostmanOperation::MoveRequest {
            source_path: "Folder/Request".to_owned(),
            target_path: Some("Target".to_owned()),
        },
    ];
    for operation in operations {
        let (client, transport) = client_with_transport([
            json_response(StatusCode::OK, base.clone()),
            json_response(StatusCode::OK, json!({})),
        ]);
        client
            .execute(operation)
            .await
            .expect("full collection effect");
        let calls = transport.calls();
        assert_eq!(calls.len(), 2, "one preflight and one effect");
        assert_eq!(calls[0].method, Method::GET);
        assert_eq!(calls[1].method, Method::PUT);
        assert_eq!(
            calls[1].url,
            "https://api.postman.example.test/collections/collection%2Fone"
        );
        assert!(
            calls[1]
                .body
                .as_ref()
                .is_some_and(|body| body["collection"].is_object())
        );
    }
}

async fn assert_duplicate_request_preserves_business_ids() {
    let base = collection_response();
    let (client, transport) = client_with_transport([
        json_response(StatusCode::OK, base.clone()),
        json_response(StatusCode::OK, json!({})),
    ]);
    client
        .execute(PostmanOperation::DuplicateRequest {
            source_path: "Folder/Request".to_owned(),
            new_name: "Copy".to_owned(),
            target_path: Some("Target".to_owned()),
        })
        .await
        .expect("request duplicate");
    let body = transport.calls()[1]
        .body
        .clone()
        .expect("duplicate request body");
    let copy = &body["collection"]["item"][1]["item"][0];
    assert_eq!(copy["name"], "Copy");
    assert!(copy.get("id").is_none());
    assert_eq!(
        copy["request"]["body"]["raw"],
        "{\"id\":42,\"nested\":{\"id\":9}}"
    );
    assert_eq!(copy["request"]["auth"]["id"], "business-auth-id");
}

async fn assert_create_request_payload_and_confirmation() {
    let base = collection_response();
    let mut confirmed = base;
    confirmed["collection"]["item"][1]["item"] = json!([{
        "id":"new-request-id","uid":"new-request-uid","name":"New Request",
        "request":{"method":"POST","header":[],"url":"https://service.example.test/new"}
    }]);
    let (client, transport) = client_with_transport([
        json_response(StatusCode::OK, collection_response()),
        json_response(StatusCode::OK, json!({})),
        json_response(StatusCode::OK, confirmed),
    ]);
    let result = client
        .execute(PostmanOperation::CreateRequest {
            folder_path: Some("Target".to_owned()),
            name: "New Request".to_owned(),
            method: "POST".to_owned(),
            url: "https://service.example.test/new".to_owned(),
            description: Some("new docs".to_owned()),
            headers: Some(vec![json!({"key":"Accept","value":"application/json"})]),
            body: Some(json!({"mode":"raw","raw":"{}"})),
            auth: None,
            tests: Some("pm.test('ok')".to_owned()),
            pre_request_script: Some("pm.variables.set('a','b')".to_owned()),
        })
        .await
        .expect("request create");
    assert_eq!(result["id"], "new-request-id");
    let calls = transport.calls();
    assert_eq!(calls.len(), 3);
    assert_eq!(calls[1].method, Method::PUT);
    let created = &calls[1].body.as_ref().expect("create body")["collection"]["item"][1]["item"][0];
    assert_eq!(created["name"], "New Request");
    assert_eq!(created["request"]["method"], "POST");
    assert_eq!(created["event"][0]["listen"], "prerequest");
    assert_eq!(created["event"][1]["listen"], "test");
}

#[tokio::test]
async fn direct_folder_and_request_effect_routes_and_payloads_are_exact() {
    assert_direct_request_field_updates().await;
    assert_direct_folder_update().await;
    assert_direct_request_body_update().await;
}

async fn assert_direct_request_field_updates() {
    let direct_cases = [
        (
            PostmanOperation::UpdateRequestName {
                request_path: "Folder/Request".to_owned(),
                name: "Renamed".to_owned(),
            },
            json!({"name":"Renamed"}),
        ),
        (
            PostmanOperation::UpdateRequestMethod {
                request_path: "Folder/Request".to_owned(),
                method: "trace".to_owned(),
            },
            json!({"method":"TRACE"}),
        ),
        (
            PostmanOperation::UpdateRequestUrl {
                request_path: "Folder/Request".to_owned(),
                url: "https://service.example.test/new".to_owned(),
            },
            json!({"url":"https://service.example.test/new"}),
        ),
        (
            PostmanOperation::UpdateRequestDescription {
                request_path: "Folder/Request".to_owned(),
                description: "docs".to_owned(),
            },
            json!({"description":"docs"}),
        ),
        (
            PostmanOperation::UpdateRequestHeaders {
                request_path: "Folder/Request".to_owned(),
                headers: "Accept: application/json\nX-Test: yes".to_owned(),
            },
            json!({"headers":"Accept: application/json\nX-Test: yes"}),
        ),
        (
            PostmanOperation::UpdateRequestAuth {
                request_path: "Folder/Request".to_owned(),
                auth: None,
            },
            json!({"auth":null}),
        ),
        (
            PostmanOperation::UpdateRequestTests {
                request_path: "Folder/Request".to_owned(),
                tests: "new test".to_owned(),
            },
            json!({"events":[{"listen":"prerequest","script":{"exec":["old pre"]}},{"listen":"test","script":{"exec":["new test"],"type":"text/javascript"}}]}),
        ),
        (
            PostmanOperation::UpdateRequestPreScript {
                request_path: "Folder/Request".to_owned(),
                pre_request_script: "new pre".to_owned(),
            },
            json!({"events":[{"listen":"prerequest","script":{"exec":["new pre"],"type":"text/javascript"}}]}),
        ),
    ];
    for (operation, expected_body) in direct_cases {
        let (client, transport) = client_with_transport([
            json_response(StatusCode::OK, collection_response()),
            json_response(StatusCode::OK, json!({})),
        ]);
        client
            .execute(operation)
            .await
            .expect("direct request effect");
        let calls = transport.calls();
        assert_eq!(calls.len(), 2);
        assert_eq!(calls[1].method, Method::PUT);
        assert_eq!(
            calls[1].url,
            "https://api.postman.example.test/collections/collection%2Fone/requests/request-1"
        );
        assert_eq!(calls[1].body, Some(expected_body));
    }
}

async fn assert_direct_folder_update() {
    let (client, transport) = client_with_transport([
        json_response(StatusCode::OK, collection_response()),
        json_response(StatusCode::OK, json!({})),
    ]);
    client
        .execute(PostmanOperation::UpdateFolder {
            folder_path: "Folder".to_owned(),
            name: Some("Renamed Folder".to_owned()),
            description: Some(String::new()),
            auth: None,
        })
        .await
        .expect("direct folder effect");
    let calls = transport.calls();
    assert_eq!(
        calls[1].url,
        "https://api.postman.example.test/collections/collection%2Fone/folders/folder-1"
    );
    assert_eq!(
        calls[1].body,
        Some(json!({"name":"Renamed Folder","description":""}))
    );
}

async fn assert_direct_request_body_update() {
    let (client, transport) = client_with_transport([
        json_response(StatusCode::OK, collection_response()),
        json_response(
            StatusCode::OK,
            json!({"meta":{"action":"update"},"data":{"id":"request-1"}}),
        ),
    ]);
    client
        .execute(PostmanOperation::UpdateRequestBody {
            request_path: "Folder/Request".to_owned(),
            body: json!({"mode":"raw","raw":"{\"updated\":true}","options":{"raw":{"language":"json"}}}),
        })
        .await
        .expect("body update");
    assert_eq!(
        transport.calls()[1].body,
        Some(
            json!({"dataMode":"raw","rawModeData":"{\"updated\":true}","dataOptions":{"raw":{"language":"json"}}})
        )
    );
}

#[tokio::test]
async fn path_ambiguity_and_unexpected_effect_success_fail_without_replay() {
    let mut ambiguous = collection_response();
    ambiguous["collection"]["item"] = json!([
        {"id":"one","name":"Same","item":[]},
        {"id":"two","name":"same","item":[]}
    ]);
    let (client, transport) = client_with_transport([json_response(StatusCode::OK, ambiguous)]);
    let error = client
        .execute(PostmanOperation::UpdateFolder {
            folder_path: "Same".to_owned(),
            name: Some("new".to_owned()),
            description: None,
            auth: None,
        })
        .await
        .expect_err("ambiguous paths fail before effect dispatch");
    assert_eq!(error.code(), PostmanClientErrorCode::Conflict);
    assert_eq!(transport.calls().len(), 1);

    let (client, transport) = client_with_transport([
        json_response(StatusCode::OK, collection_response()),
        json_response(StatusCode::CREATED, json!({})),
    ]);
    let error = client
        .execute(PostmanOperation::UpdateRequestName {
            request_path: "Folder/Request".to_owned(),
            name: "new".to_owned(),
        })
        .await
        .expect_err("unexpected accepted 2xx is ambiguous");
    assert_eq!(error.code(), PostmanClientErrorCode::UnknownOutcome);
    assert!(!error.retryable());
    assert_eq!(transport.calls().len(), 2, "effect is never replayed");
}

#[test]
fn effect_and_read_status_mapping_is_typed_and_retry_safe() {
    let read_cases = [
        (
            StatusCode::REQUEST_TIMEOUT,
            PostmanClientErrorCode::Timeout,
            true,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            PostmanClientErrorCode::RateLimited,
            true,
        ),
        (
            StatusCode::BAD_GATEWAY,
            PostmanClientErrorCode::DependencyUnavailable,
            true,
        ),
        (
            StatusCode::UNAUTHORIZED,
            PostmanClientErrorCode::Authentication,
            false,
        ),
        (
            StatusCode::FORBIDDEN,
            PostmanClientErrorCode::Authorization,
            false,
        ),
        (
            StatusCode::NOT_FOUND,
            PostmanClientErrorCode::NotFound,
            false,
        ),
    ];
    for (status, code, retryable) in read_cases {
        let error = test_map_status(status, false);
        assert_eq!(error.code(), code);
        assert_eq!(error.retryable(), retryable);
    }
    for status in [
        StatusCode::REQUEST_TIMEOUT,
        StatusCode::TOO_MANY_REQUESTS,
        StatusCode::BAD_GATEWAY,
        StatusCode::ACCEPTED,
        StatusCode::NO_CONTENT,
    ] {
        let error = test_map_status(status, true);
        assert_eq!(error.code(), PostmanClientErrorCode::UnknownOutcome);
        assert!(!error.retryable());
    }
}

#[tokio::test]
async fn dynamic_execution_is_denied_before_management_io_without_separate_authority() {
    let client = PostmanClient::new(config()).expect("client constructs without I/O");
    let error = client
        .execute(PostmanOperation::ExecuteRequest {
            request_path: "API/Get".to_owned(),
            override_variables: Map::new(),
        })
        .await
        .expect_err("production has no dynamic authority constructor");
    assert_eq!(error.code(), PostmanClientErrorCode::Authorization);
    assert!(!error.retryable());

    assert_eq!(
        test_expand_variables("{{a}}/users", &[("a", "{{b}}"), ("b", "v1")])
            .expect("bounded recursive expansion"),
        "v1/users"
    );
    assert!(test_expand_variables("{{missing}}", &[]).is_err());
    assert!(test_expand_variables("{{a}}", &[("a", "{{a}}")]).is_err());
}

#[tokio::test]
async fn dynamic_execution_uses_sealed_authority_once_and_maps_dispatch_failure_to_unknown() {
    assert_dynamic_execution_success().await;
    assert_dynamic_dispatch_failure().await;
    assert_parent_auth_is_not_forwarded().await;
}

async fn assert_dynamic_execution_success() {
    let management = Arc::new(FixtureManagementTransport::new([json_response(
        StatusCode::OK,
        collection_response(),
    )]));
    let management_trait: Arc<dyn ManagementTransport> = management;
    let dynamic = Arc::new(FixtureDynamicAuthority::new([Ok(DynamicResponse {
        status: StatusCode::OK,
        reason: "OK".into(),
        headers: Map::from_iter([("content-type".to_owned(), json!("application/json"))]),
        body: json!({"ok":true}),
        size_bytes: 11,
    })]));
    let dynamic_trait: Arc<dyn DynamicEgressAuthority> = dynamic.clone();
    let client = PostmanClient::fixture_with_management_and_dynamic(
        config(),
        management_trait,
        dynamic_trait,
    )
    .expect("sealed dynamic fixture");
    let output = client
        .execute(PostmanOperation::ExecuteRequest {
            request_path: "Folder/Request".to_owned(),
            override_variables: Map::new(),
        })
        .await
        .expect("one downstream request");
    {
        let calls = dynamic.calls.lock().expect("dynamic calls");
        assert_eq!(calls.len(), 1);
        assert_eq!(calls[0].method, Method::GET);
        assert_eq!(calls[0].url, "https://service.example.test/items");
        assert!(calls[0].query.is_empty());
        assert!(calls[0].body.is_none());
        assert!(calls[0].headers.iter().any(|(name, value)| {
            name == "authorization" && value == "Bearer environment-secret"
        }));
    }
    let rendered = output.to_string();
    assert!(!rendered.contains("environment-secret"));
    assert!(!rendered.contains("items"));
    assert_eq!(
        output["request"]["url"],
        "https://service.example.test/<segment>"
    );
}

async fn assert_dynamic_dispatch_failure() {
    let management = Arc::new(FixtureManagementTransport::new([json_response(
        StatusCode::OK,
        collection_response(),
    )]));
    let management_trait: Arc<dyn ManagementTransport> = management;
    let dynamic = Arc::new(FixtureDynamicAuthority::new([Err(
        PostmanClientError::fixture(PostmanClientErrorCode::Timeout, true),
    )]));
    let dynamic_trait: Arc<dyn DynamicEgressAuthority> = dynamic.clone();
    let client = PostmanClient::fixture_with_management_and_dynamic(
        config(),
        management_trait,
        dynamic_trait,
    )
    .expect("sealed dynamic failure fixture");
    let error = client
        .execute(PostmanOperation::ExecuteRequest {
            request_path: "Folder/Request".to_owned(),
            override_variables: Map::new(),
        })
        .await
        .expect_err("dispatch handoff is conservatively ambiguous");
    assert_eq!(error.code(), PostmanClientErrorCode::UnknownOutcome);
    assert!(!error.retryable());
    assert_eq!(dynamic.calls.lock().expect("dynamic calls").len(), 1);
}

async fn assert_parent_auth_is_not_forwarded() {
    let mut untrusted_parent_auth = collection_response();
    untrusted_parent_auth["collection"]["auth"] =
        json!({"type":"bearer","bearer":[{"key":"token","value":"collection-secret"}]});
    untrusted_parent_auth["collection"]["item"][0]["auth"] =
        json!({"type":"bearer","bearer":[{"key":"token","value":"folder-secret"}]});
    untrusted_parent_auth["collection"]["item"][0]["item"][0]["request"]
        .as_object_mut()
        .expect("request")
        .remove("auth");
    let management = Arc::new(FixtureManagementTransport::new([json_response(
        StatusCode::OK,
        untrusted_parent_auth,
    )]));
    let management_trait: Arc<dyn ManagementTransport> = management;
    let dynamic = Arc::new(FixtureDynamicAuthority::new([Ok(DynamicResponse {
        status: StatusCode::OK,
        reason: "OK".into(),
        headers: Map::new(),
        body: json!({"ok":true}),
        size_bytes: 11,
    })]));
    let dynamic_trait: Arc<dyn DynamicEgressAuthority> = dynamic.clone();
    let mut no_environment_auth = settings(Some(json!([])));
    no_environment_auth["environment_config"]
        .as_object_mut()
        .expect("environment")
        .remove("auth");
    let no_environment_auth =
        PostmanToolkitConfig::parse(&no_environment_auth).expect("profile without auth");
    let client = PostmanClient::fixture_with_management_and_dynamic(
        no_environment_auth,
        management_trait,
        dynamic_trait,
    )
    .expect("parent auth isolation fixture");
    client
        .execute(PostmanOperation::ExecuteRequest {
            request_path: "Folder/Request".to_owned(),
            override_variables: Map::new(),
        })
        .await
        .expect("request without own auth executes");
    assert!(
        dynamic.calls.lock().expect("dynamic calls")[0]
            .headers
            .iter()
            .all(|(name, _)| name != "authorization")
    );
}

#[test]
fn dynamic_auth_headers_cookies_and_bodies_are_bounded_and_source_compatible() {
    let custom = json!({
        "type":"custom",
        "params": {
            "headers":{"Authorization":"Bearer auth-secret","X-Auth":"{{token}}"},
            "query":{"api_key":"{{token}}","api_key_2":"second"},
            "cookies":{"first":"one","second":"{{token}}"}
        }
    });
    let raw_body = json!({"mode":"raw","raw":"{\"id\":\"{{id}}\"}"});
    let parts = test_dynamic_parts(
        Some(&custom),
        Some(&raw_body),
        &[("authorization", "Bearer stored-wins")],
        &[("token", "secret"), ("id", "42")],
    )
    .expect("bounded dynamic request components");
    assert_eq!(
        parts
            .headers
            .iter()
            .find(|(name, _)| name == "authorization")
            .map(|(_, value)| value.as_str()),
        Some("Bearer stored-wins"),
        "stored headers overwrite auth"
    );
    assert_eq!(
        parts
            .headers
            .iter()
            .find(|(name, _)| name == "cookie")
            .map(|(_, value)| value.as_str()),
        Some("first=one; second=secret"),
        "custom cookies are joined"
    );
    assert_eq!(
        parts
            .headers
            .iter()
            .find(|(name, _)| name == "content-type")
            .map(|(_, value)| value.as_str()),
        Some("application/json"),
        "valid raw JSON is detected without rewriting bytes"
    );
    assert_eq!(parts.body.as_deref(), Some(b"{\"id\":\"42\"}".as_slice()));
    assert_eq!(
        parts.query,
        [
            ("api_key".to_owned(), "secret".to_owned()),
            ("api_key_2".to_owned(), "second".to_owned())
        ]
    );

    let commented_body = json!({
        "mode":"raw",
        "raw":"{\n// safe comment\n\"url\":\"https://service.example.test/items//literal\",\n\"id\":1\n}"
    });
    let commented = test_dynamic_parts(None, Some(&commented_body), &[], &[])
        .expect("commented raw JSON is normalized");
    assert_eq!(
        commented
            .headers
            .iter()
            .find(|(name, _)| name == "content-type")
            .map(|(_, value)| value.as_str()),
        Some("application/json")
    );
    assert_eq!(
        serde_json::from_slice::<Value>(&commented.body.expect("raw body"))
            .expect("normalized JSON body"),
        json!({"url":"https://service.example.test/items//literal","id":1})
    );

    assert!(test_dynamic_header_value(65_536 - "x-test".len()).is_ok());
    let error = test_dynamic_header_value(65_537 - "x-test".len())
        .expect_err("aggregate header byte bound");
    assert_eq!(error.code(), PostmanClientErrorCode::ResourceExhausted);
}

#[test]
fn production_slice_has_no_forbidden_control_flow_macros() {
    let sources = [
        include_str!("../../../../libs/rust/agent-runtime/src/toolkits/families/postman/mod.rs"),
        include_str!("../../../../libs/rust/agent-runtime/src/toolkits/families/postman/config.rs"),
        include_str!("../../../../libs/rust/agent-runtime/src/toolkits/families/postman/client.rs"),
        include_str!(
            "../../../../libs/rust/agent-runtime/src/toolkits/families/postman/collection.rs"
        ),
        include_str!(
            "../../../../libs/rust/agent-runtime/src/toolkits/families/postman/analysis.rs"
        ),
        include_str!("../../../../libs/rust/agent-runtime/src/toolkits/families/postman/tools.rs"),
    ];
    for source in sources {
        for forbidden in [
            "panic!(",
            ".unwrap()",
            ".expect(",
            "todo!(",
            "unimplemented!(",
        ] {
            assert!(!source.contains(forbidden), "forbidden {forbidden}");
        }
    }
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn PostmanApi> = Arc::new(FixtureApi::new([]));
    let toolset =
        test_build_with_api("gate", &[], &policy(&[]), &api).expect("complete postman toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("postman tools");
    super::sdk_conformance::assert_sdk_conformance("postman", &tools);
}
