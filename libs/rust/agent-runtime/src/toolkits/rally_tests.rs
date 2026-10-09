use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::header::{AUTHORIZATION, HeaderName};
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::rally::client::{
    RallyApi, RallyClient, RallyClientError, RallyClientErrorCode, RallyHttpResponse,
    RallyTransport,
};
use super::families::rally::config::{RallyConfigErrorCode, RallyToolkitConfig};
use super::families::rally::tools::{
    RallyToolsetErrorCode, build_rally_toolset, test_build_with_api,
};
use super::policy::ToolAdmissionPolicy;

const ZSESSIONID: HeaderName = HeaderName::from_static("zsessionid");

fn settings(selected_tools: &[&str], basic: bool) -> Map<String, Value> {
    let credential = if basic {
        json!({
            "server":"https://rally.example.test",
            "api_key":null,
            "username":"engineer@example.com",
            "password":"basic-secret"
        })
    } else {
        json!({
            "server":"rally.example.test",
            "api_key":"api-super-secret",
            "username":"stale-user",
            "password":"stale-password"
        })
    };
    json!({
        "rally_configuration":credential,
        "workspace":null,
        "project":null,
        "selected_tools":selected_tools
    })
    .as_object()
    .cloned()
    .expect("Rally fixture settings are an object")
}

fn config(selected_tools: &[&str]) -> RallyToolkitConfig {
    RallyToolkitConfig::parse(&settings(selected_tools, false)).expect("valid Rally configuration")
}

fn basic_config() -> RallyToolkitConfig {
    RallyToolkitConfig::parse(&settings(&[], true)).expect("valid Basic Rally configuration")
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Rally policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("rally-test").with_function_call_id("rally-call"))
}

#[test]
fn materialized_configuration_normalizes_origin_auth_and_selection_without_network() {
    let parsed = config(&["get_types", "get_types", "create_artifact"]);
    assert_eq!(parsed.test_origin().as_str(), "https://rally.example.test/");
    assert_eq!(
        parsed.selected_tools(),
        [
            Box::<str>::from("get_types"),
            Box::<str>::from("create_artifact")
        ]
    );
    let https = RallyToolkitConfig::parse(&settings(&[], true)).expect("HTTPS origin");
    assert_eq!(https.test_origin().as_str(), "https://rally.example.test/");
    let mut empty_api_key_uses_basic = settings(&[], true);
    empty_api_key_uses_basic
        .get_mut("rally_configuration")
        .and_then(Value::as_object_mut)
        .expect("nested Rally configuration")
        .insert("api_key".to_owned(), Value::String(String::new()));
    RallyToolkitConfig::parse(&empty_api_key_uses_basic)
        .expect("empty API key falls back to complete Basic credentials");
    let rendered = format!(
        "{:?}",
        RallyToolkitConfig::parse(&settings(&[], false)).err()
    );
    assert!(!rendered.contains("api-super-secret"));
    assert!(!rendered.contains("basic-secret"));
}

#[test]
fn malformed_origin_partial_auth_context_and_bounds_fail_closed() {
    for invalid in [
        json!({"rally_configuration":{"server":"http://rally.test","api_key":"key"}}),
        json!({"rally_configuration":{"server":"https://user@rally.test","api_key":"key"}}),
        json!({"rally_configuration":{"server":"https://rally.test/path","api_key":"key"}}),
        json!({"rally_configuration":{"server":"rally.test","username":"only-user"}}),
        json!({"rally_configuration":{"server":"rally.test","api_key":"key"},"project":"bad\"name"}),
    ] {
        let Err(error) = RallyToolkitConfig::parse(
            invalid
                .as_object()
                .expect("invalid Rally fixture is an object"),
        ) else {
            panic!("invalid Rally configuration must fail");
        };
        assert_eq!(error.code(), RallyConfigErrorCode::InvalidConfiguration);
    }
    let mut oversized = settings(&[], false);
    oversized
        .get_mut("rally_configuration")
        .and_then(Value::as_object_mut)
        .expect("nested Rally config")
        .insert(
            "api_key".to_owned(),
            Value::String("x".repeat(16 * 1_024 + 1)),
        );
    let Err(error) = RallyToolkitConfig::parse(&oversized) else {
        panic!("oversized Rally secret must fail");
    };
    assert_eq!(error.code(), RallyConfigErrorCode::ResourceExhausted);
}

#[derive(Clone)]
struct CapturedRequest {
    method: Method,
    path: String,
    query: Vec<(String, String)>,
    body: Option<Value>,
    api_key: Option<String>,
    api_key_sensitive: bool,
    basic: Option<String>,
    basic_sensitive: bool,
    effect: bool,
}

type Handler =
    dyn Fn(&Request, bool, usize) -> Result<RallyHttpResponse, RallyClientError> + Send + Sync;

struct FixtureTransport {
    requests: Mutex<Vec<CapturedRequest>>,
    handler: Box<Handler>,
}

impl FixtureTransport {
    fn new(
        handler: impl Fn(&Request, bool, usize) -> Result<RallyHttpResponse, RallyClientError>
        + Send
        + Sync
        + 'static,
    ) -> Self {
        Self {
            requests: Mutex::new(Vec::new()),
            handler: Box::new(handler),
        }
    }

    fn requests(&self) -> Vec<CapturedRequest> {
        self.requests
            .lock()
            .expect("Rally request fixture lock")
            .clone()
    }
}

#[async_trait]
impl RallyTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<RallyHttpResponse, RallyClientError> {
        let body = request
            .body()
            .and_then(reqwest::Body::as_bytes)
            .and_then(|bytes| serde_json::from_slice(bytes).ok());
        let api_key = request
            .headers()
            .get(&ZSESSIONID)
            .and_then(|value| value.to_str().ok())
            .map(ToOwned::to_owned);
        let basic = request
            .headers()
            .get(AUTHORIZATION)
            .and_then(|value| value.to_str().ok())
            .map(ToOwned::to_owned);
        let api_key_sensitive = request
            .headers()
            .get(&ZSESSIONID)
            .is_some_and(reqwest::header::HeaderValue::is_sensitive);
        let basic_sensitive = request
            .headers()
            .get(AUTHORIZATION)
            .is_some_and(reqwest::header::HeaderValue::is_sensitive);
        let captured = CapturedRequest {
            method: request.method().clone(),
            path: request.url().path().to_owned(),
            query: request
                .url()
                .query_pairs()
                .map(|(key, value)| (key.into_owned(), value.into_owned()))
                .collect(),
            body,
            api_key,
            api_key_sensitive,
            basic,
            basic_sensitive,
            effect,
        };
        let mut requests = self.requests.lock().expect("Rally request fixture lock");
        let index = requests.len();
        requests.push(captured);
        drop(requests);
        (self.handler)(&request, effect, index)
    }
}

fn ok(body: Value) -> RallyHttpResponse {
    RallyHttpResponse::fixture(StatusCode::OK, Some(body))
}

fn query(results: &Value) -> RallyHttpResponse {
    ok(json!({
        "QueryResult":{
            "Errors":[],"Warnings":[],"TotalResultCount":results.as_array().map_or(0, Vec::len),
            "Results":results
        }
    }))
}

#[tokio::test]
async fn api_key_reads_are_origin_bound_one_page_and_structurally_encoded() {
    let transport = Arc::new(FixtureTransport::new(|_, effect, index| {
        assert!(!effect);
        match index {
            0 => Ok(query(&json!([
                {"ElementName":"Defect"},{"ElementName":"HierarchicalRequirement"}
            ]))),
            1 => Ok(query(&json!([{"FormattedID":"US1","Name":"story"}]))),
            _ => Err(RallyClientError::fixture_for_test(
                RallyClientErrorCode::InvalidInput,
                false,
            )),
        }
    }));
    let client = RallyClient::with_transport(config(&[]), transport.clone());
    assert_eq!(
        client.get_types().await.expect("Rally types"),
        json!(["Defect", "HierarchicalRequirement"])
    );
    assert_eq!(
        client
            .get_entities(
                "UserStory",
                Some("ScheduleState = \"In-Progress\""),
                true,
                7,
            )
            .await
            .expect("Rally entities"),
        json!([{"FormattedID":"US1","Name":"story"}])
    );
    let requests = transport.requests();
    assert_eq!(requests.len(), 2);
    assert_eq!(requests[0].path, "/slm/webservice/v2.0/TypeDefinition");
    assert_eq!(
        requests[1].path,
        "/slm/webservice/v2.0/HierarchicalRequirement"
    );
    assert!(requests.iter().all(|request| {
        request.api_key.as_deref() == Some("api-super-secret")
            && request.api_key_sensitive
            && request.basic.is_none()
            && !request.effect
    }));
    assert!(
        requests[1]
            .query
            .contains(&("pagesize".to_owned(), "7".to_owned()))
    );
    assert!(requests[1].query.contains(&(
        "query".to_owned(),
        "(ScheduleState = \"In-Progress\")".to_owned()
    )));
}

#[tokio::test]
async fn basic_create_uses_same_client_security_token_put_and_no_presentation_get() {
    let transport = Arc::new(FixtureTransport::new(
        |request, effect, index| match index {
            0 => {
                assert!(!effect);
                assert!(request.url().path().ends_with("/security/authorize"));
                Ok(ok(json!({
                    "OperationResult":{"Errors":[],"Warnings":[],"SecurityToken":"csrf-token"}
                })))
            }
            1 => {
                assert!(effect);
                Ok(ok(json!({
                    "CreateResult":{"Errors":[],"Warnings":[],"Object":{"FormattedID":"DE123"}}
                })))
            }
            _ => Err(RallyClientError::fixture_for_test(
                RallyClientErrorCode::InvalidInput,
                false,
            )),
        },
    ));
    let client = RallyClient::with_transport(basic_config(), transport.clone());
    assert_eq!(
        client
            .create_artifact(
                "Defect",
                &Map::from_iter([
                    ("Name".to_owned(), json!("broken checkout")),
                    ("Severity".to_owned(), json!("Major Problem")),
                    (
                        "Children".to_owned(),
                        json!(["hierarchicalrequirement/123"]),
                    ),
                    ("Value".to_owned(), json!(["section/123"])),
                ]),
            )
            .await
            .expect("create Rally defect"),
        json!("Entity DE123 created successfully.")
    );
    let requests = transport.requests();
    assert_eq!(requests.len(), 2);
    assert_eq!(requests[1].method, Method::PUT);
    assert_eq!(requests[1].path, "/slm/webservice/v2.0/defect/create");
    assert!(
        requests[1]
            .query
            .contains(&("key".to_owned(), "csrf-token".to_owned()))
    );
    assert_eq!(
        requests[1]
            .body
            .as_ref()
            .and_then(|value| value.get("Defect"))
            .and_then(|value| value.get("Name")),
        Some(&json!("broken checkout"))
    );
    assert_eq!(
        requests[1]
            .body
            .as_ref()
            .and_then(|value| value.get("Defect"))
            .and_then(|value| value.get("Children")),
        Some(&json!([{"_ref":"hierarchicalrequirement/123"}]))
    );
    assert_eq!(
        requests[1]
            .body
            .as_ref()
            .and_then(|value| value.get("Defect"))
            .and_then(|value| value.get("Value")),
        Some(&json!(["section/123"]))
    );
    assert!(requests.iter().all(|request| {
        request
            .basic
            .as_deref()
            .is_some_and(|value| value.starts_with("Basic "))
            && request.basic_sensitive
    }));
}

#[tokio::test]
async fn update_by_formatted_id_resolves_once_then_posts_without_followup() {
    let transport = Arc::new(FixtureTransport::new(|_, effect, index| match index {
        0 => Ok(query(&json!([{"ObjectID":123_456_789}]))),
        1 => {
            assert!(effect);
            Ok(ok(json!({
                "UpdateResult":{"Errors":[],"Warnings":[],"Object":{"ObjectID":123_456_789}}
            })))
        }
        _ => Err(RallyClientError::fixture_for_test(
            RallyClientErrorCode::InvalidInput,
            false,
        )),
    }));
    let client = RallyClient::with_transport(config(&[]), transport.clone());
    assert_eq!(
        client
            .update_artifact(
                "Defect",
                &Map::from_iter([
                    ("FormattedID".to_owned(), json!("DE123")),
                    ("Description".to_owned(), json!("fixed")),
                ]),
            )
            .await
            .expect("update Rally defect"),
        json!("Artifact DE123 updated successfully.")
    );
    let requests = transport.requests();
    assert_eq!(requests.len(), 2);
    assert_eq!(requests[0].method, Method::GET);
    assert_eq!(requests[1].method, Method::POST);
    assert_eq!(requests[1].path, "/slm/webservice/v2.0/defect/123456789");
    assert!(requests[1].effect);
}

#[tokio::test]
async fn malformed_successful_effect_is_unknown_and_never_retryable() {
    let missing = Arc::new(FixtureTransport::new(|_, effect, _| {
        assert!(effect);
        Ok(ok(json!({"CreateResult":{"Errors":[],"Warnings":[]}})))
    }));
    let invalid_create = Arc::new(FixtureTransport::new(|_, effect, _| {
        assert!(effect);
        Ok(ok(json!({
            "CreateResult":{"Errors":[],"Warnings":[],"Object":{"FormattedID":"bad id"}}
        })))
    }));
    let oversized_update = Arc::new(FixtureTransport::new(|_, effect, _| {
        assert!(effect);
        Ok(ok(json!({
            "UpdateResult":{"Errors":[],"Warnings":[],"Object":{"FormattedID":"x".repeat(65)}}
        })))
    }));
    let errors = [
        RallyClient::with_transport(config(&[]), missing)
            .create_artifact("Defect", &Map::from_iter([("Name".to_owned(), json!("x"))]))
            .await
            .expect_err("missing committed create identity is ambiguous"),
        RallyClient::with_transport(config(&[]), invalid_create)
            .create_artifact("Defect", &Map::from_iter([("Name".to_owned(), json!("x"))]))
            .await
            .expect_err("invalid committed create identity is ambiguous"),
        RallyClient::with_transport(config(&[]), oversized_update)
            .update_artifact(
                "Defect",
                &Map::from_iter([
                    ("ObjectID".to_owned(), json!(123_456_789)),
                    ("Description".to_owned(), json!("fixed")),
                ]),
            )
            .await
            .expect_err("oversized committed update identity is ambiguous"),
    ];
    for error in errors {
        assert_eq!(error.code(), RallyClientErrorCode::UnknownOutcome);
        assert!(!error.retryable());
        let rendered = format!("{error:?} {error}");
        assert!(!rendered.contains("api-super-secret"));
        assert!(!rendered.contains("bad id"));
    }
}

#[derive(Default)]
struct FixtureRallyApi {
    calls: Mutex<Vec<Value>>,
}

impl FixtureRallyApi {
    fn record(&self, value: Value) -> Value {
        self.calls
            .lock()
            .expect("Rally API fixture lock")
            .push(value.clone());
        value
    }
}

#[async_trait]
impl RallyApi for FixtureRallyApi {
    async fn get_types(&self) -> Result<Value, RallyClientError> {
        Ok(self.record(json!({"tool":"get_types"})))
    }

    async fn get_entities(
        &self,
        entity_type: &str,
        query: Option<&str>,
        fetch: bool,
        limit: usize,
    ) -> Result<Value, RallyClientError> {
        Ok(self.record(json!({
            "tool":"get_entities","entity_type":entity_type,"query":query,
            "fetch":fetch,"limit":limit
        })))
    }

    async fn get_project(&self, project_name: Option<&str>) -> Result<Value, RallyClientError> {
        Ok(self.record(json!({"tool":"get_project","project_name":project_name})))
    }

    async fn get_workspace(&self, workspace_name: Option<&str>) -> Result<Value, RallyClientError> {
        Ok(self.record(json!({"tool":"get_workspace","workspace_name":workspace_name})))
    }

    async fn get_user(&self, user_name: Option<&str>) -> Result<Value, RallyClientError> {
        Ok(self.record(json!({"tool":"get_user","user_name":user_name})))
    }

    async fn get_context(&self) -> Result<Value, RallyClientError> {
        Ok(self.record(json!({"tool":"get_context"})))
    }

    async fn create_artifact(
        &self,
        entity_type: &str,
        fields: &Map<String, Value>,
    ) -> Result<Value, RallyClientError> {
        Ok(self.record(json!({
            "tool":"create_artifact","entity_type":entity_type,"fields":fields
        })))
    }

    async fn update_artifact(
        &self,
        entity_type: &str,
        fields: &Map<String, Value>,
    ) -> Result<Value, RallyClientError> {
        Ok(self.record(json!({
            "tool":"update_artifact","entity_type":entity_type,"fields":fields
        })))
    }
}

fn assert_model_contract(tools: &[Arc<dyn Tool>]) {
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        [
            "get_types",
            "get_entities",
            "get_project",
            "get_workspace",
            "get_user",
            "get_context",
            "create_artifact",
            "update_artifact"
        ]
    );
    for (index, tool) in tools.iter().enumerate() {
        let read_only = index < 6;
        assert_eq!(tool.is_read_only(), read_only);
        assert_eq!(tool.is_concurrency_safe(), read_only);
        assert!(tool.description().contains("Toolkit: delivery"));
        let schema = tool.parameters_schema().expect("Rally schema");
        for property in schema["properties"]
            .as_object()
            .expect("Rally properties")
            .values()
        {
            assert!(
                property["description"]
                    .as_str()
                    .is_some_and(|description| !description.trim().is_empty())
            );
        }
    }
    assert!(tools[0].description().contains("entity type names"));
    assert!(tools[1].description().contains("no continuation"));
    assert!(tools[5].description().contains("run concurrently"));
    assert!(tools[6].description().contains("not safe to retry"));
    assert!(tools[7].description().contains("ObjectID or FormattedID"));

    let entity_schema = tools[1].parameters_schema().expect("get entities schema");
    assert_eq!(entity_schema["properties"]["limit"]["default"], json!(10));
    assert_eq!(entity_schema["properties"]["limit"]["maximum"], json!(100));
    assert!(
        entity_schema["properties"]["query"]["description"]
            .as_str()
            .is_some_and(|value| value.contains("ScheduleState"))
    );
    let create = tools[6].parameters_schema().expect("create schema");
    assert_eq!(create["required"], json!(["entity_json"]));
    assert!(
        create["properties"]["entity_json"]["description"]
            .as_str()
            .is_some_and(|value| value.contains("Severity") && value.contains("_ref"))
    );
    let update = tools[7].parameters_schema().expect("update schema");
    assert_eq!(update["required"], json!(["entity_json", "entity_type"]));
}

#[tokio::test]
async fn all_eight_tools_preserve_order_metadata_arguments_and_policy() {
    let api = Arc::new(FixtureRallyApi::default());
    let api_trait: Arc<dyn RallyApi> = api.clone();
    let toolset = test_build_with_api("delivery", &[], &policy(&[]), &api_trait)
        .expect("complete Rally toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Rally tools");
    assert_model_contract(&tools);
    tools[1]
        .execute(
            context(),
            json!({"entity_type":"Defect","query":"State = \"Open\"","limit":5}),
        )
        .await
        .expect("get entities tool");
    tools[6]
        .execute(
            context(),
            json!({"entity_type":"Defect","entity_json":"{\"Name\":\"new defect\"}"}),
        )
        .await
        .expect("create artifact tool");
    assert_eq!(api.calls.lock().expect("Rally calls").len(), 2);

    let blocked = test_build_with_api(
        "delivery",
        &[],
        &policy(&[("rally", &["create_artifact"])]),
        &api_trait,
    )
    .expect("policy-filtered Rally toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert!(
        blocked
            .tools(readonly)
            .await
            .expect("filtered Rally tools")
            .iter()
            .all(|tool| tool.name() != "create_artifact")
    );
}

#[tokio::test]
async fn invalid_selection_and_effect_arguments_fail_before_provider_use() {
    let api = Arc::new(FixtureRallyApi::default());
    let api_trait: Arc<dyn RallyApi> = api.clone();
    let toolset = test_build_with_api(
        "delivery",
        &["create_artifact".to_owned(), "update_artifact".to_owned()],
        &policy(&[]),
        &api_trait,
    )
    .expect("selected Rally tools");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("selected Rally tools");
    for invalid in [
        json!({}),
        json!({"entity_json":"[]"}),
        json!({"entity_json":"{}"}),
        json!({"entity_json":"{\"bad-key\":1}"}),
    ] {
        assert!(tools[0].execute(context(), invalid).await.is_err());
    }
    for invalid in [
        json!({"entity_json":"{\"FormattedID\":\"DE1\",\"Name\":\"x\"}"}),
        json!({"entity_type":"Defect","entity_json":"{\"FormattedID\":\"DE1\"}"}),
        json!({"entity_type":"Defect","entity_json":"{\"ObjectID\":1,\"FormattedID\":\"DE1\",\"Name\":\"x\"}"}),
        json!({"entity_type":"../Defect","entity_json":"{\"ObjectID\":1,\"Name\":\"x\"}"}),
        json!({"entity_type":"PortfolioItem/Feature/Child","entity_json":"{\"ObjectID\":1,\"Name\":\"x\"}"}),
    ] {
        assert!(tools[1].execute(context(), invalid).await.is_err());
    }
    assert!(api.calls.lock().expect("Rally calls").is_empty());

    let unknown = RallyToolkitConfig::parse(&settings(&["delete_artifact"], false))
        .expect("bounded unknown Rally selection parses");
    let Err(error) = build_rally_toolset("delivery", unknown, &policy(&[])) else {
        panic!("unknown Rally tool must fail");
    };
    assert_eq!(error.code(), RallyToolsetErrorCode::UnsupportedSelection);
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn RallyApi> = Arc::new(FixtureRallyApi::default());
    let toolset =
        test_build_with_api("gate", &[], &policy(&[]), &api).expect("complete rally toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("rally tools");
    super::sdk_conformance::assert_sdk_conformance("rally", &tools);
}
