//! Focused compatibility and safety tests for the capability-disabled Elasticsearch family.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::StatusCode;
use serde_json::{Map, Value, json};

use super::families::elastic::client::{
    ElasticApi, ElasticClient, ElasticClientError, ElasticClientErrorCode, ElasticHttpResponse,
    ElasticTransport, validate_index, validate_query,
};
use super::families::elastic::config::{ElasticConfigErrorCode, ElasticToolkitConfig};
use super::families::elastic::tools::{ElasticToolsetErrorCode, test_build_with_api, test_catalog};
use super::policy::ToolAdmissionPolicy;

const CLUSTER_URL: &str = "https://elastic.example.test:9243";
const API_KEY: &str = "ZWxhc3RpYy1rZXktaWQ6ZWxhc3RpYy1rZXktc2VjcmV0";

fn settings(api_key: &Value, selected_tools: &Value) -> Map<String, Value> {
    json!({
        "url": CLUSTER_URL,
        "api_key": api_key,
        "selected_tools": selected_tools
    })
    .as_object()
    .cloned()
    .expect("Elasticsearch settings fixture is an object")
}

fn config() -> ElasticToolkitConfig {
    ElasticToolkitConfig::parse(&settings(&json!(API_KEY), &json!([])))
        .expect("valid Elasticsearch fixture config")
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Elasticsearch fixture policy"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("elastic-test")
            .with_session_id("session-1")
            .with_function_call_id("elastic-call-4"),
    )
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct RecordedRequest {
    method: String,
    url: String,
    authorization: Option<String>,
    accept: Option<String>,
    content_type: Option<String>,
    body: Vec<u8>,
}

struct FixtureTransport {
    responses: Mutex<VecDeque<Result<ElasticHttpResponse, ElasticClientError>>>,
    requests: Mutex<Vec<RecordedRequest>>,
}

impl FixtureTransport {
    fn new(
        responses: impl IntoIterator<Item = Result<ElasticHttpResponse, ElasticClientError>>,
    ) -> Self {
        Self {
            responses: Mutex::new(responses.into_iter().collect()),
            requests: Mutex::new(Vec::new()),
        }
    }

    fn requests(&self) -> Vec<RecordedRequest> {
        self.requests
            .lock()
            .expect("Elasticsearch request fixture lock")
            .clone()
    }
}

#[async_trait]
impl ElasticTransport for FixtureTransport {
    async fn execute(
        &self,
        request: reqwest::Request,
    ) -> Result<ElasticHttpResponse, ElasticClientError> {
        self.requests
            .lock()
            .expect("Elasticsearch request fixture lock")
            .push(RecordedRequest {
                method: request.method().to_string(),
                url: request.url().to_string(),
                authorization: header(&request, reqwest::header::AUTHORIZATION),
                accept: header(&request, reqwest::header::ACCEPT),
                content_type: header(&request, reqwest::header::CONTENT_TYPE),
                body: request
                    .body()
                    .and_then(reqwest::Body::as_bytes)
                    .map_or_else(Vec::new, <[u8]>::to_vec),
            });
        self.responses
            .lock()
            .expect("Elasticsearch response fixture lock")
            .pop_front()
            .unwrap_or_else(|| {
                Err(ElasticClientError::fixture(
                    ElasticClientErrorCode::InvalidResponse,
                ))
            })
    }
}

fn header(request: &reqwest::Request, name: reqwest::header::HeaderName) -> Option<String> {
    request
        .headers()
        .get(name)
        .and_then(|value| value.to_str().ok())
        .map(str::to_owned)
}

struct FixtureApi {
    calls: Mutex<Vec<Value>>,
}

impl FixtureApi {
    fn new() -> Self {
        Self {
            calls: Mutex::new(Vec::new()),
        }
    }

    fn calls(&self) -> Vec<Value> {
        self.calls
            .lock()
            .expect("Elasticsearch API fixture lock")
            .clone()
    }
}

#[async_trait]
impl ElasticApi for FixtureApi {
    async fn search(&self, index: &str, query: &Value) -> Result<Value, ElasticClientError> {
        self.calls
            .lock()
            .expect("Elasticsearch API fixture lock")
            .push(json!({"index":index,"query":query}));
        Ok(json!({"hits":{"hits":[]}}))
    }
}

fn client_with(transport: Arc<FixtureTransport>) -> ElasticClient {
    let transport_trait: Arc<dyn ElasticTransport> = transport;
    ElasticClient::with_transport(config(), transport_trait)
}

#[test]
fn inline_configuration_is_fixed_origin_optional_auth_and_redacted() {
    let parsed = ElasticToolkitConfig::parse(&settings(
        &json!(API_KEY),
        &json!(["search_elastic_index", "search_elastic_index"]),
    ))
    .expect("valid Elasticsearch config");
    assert_eq!(
        parsed.selected_tools(),
        [Box::<str>::from("search_elastic_index")]
    );
    assert!(ElasticToolkitConfig::parse(&settings(&Value::Null, &json!([]))).is_ok());

    for (url, key) in [
        ("", json!(API_KEY)),
        ("http://elastic.example.test", json!(API_KEY)),
        ("https://user@elastic.example.test", json!(API_KEY)),
        ("https://elastic.example.test/base", json!(API_KEY)),
        ("https://elastic.example.test?query=1", json!(API_KEY)),
        (CLUSTER_URL, json!("bad key")),
        (CLUSTER_URL, json!("")),
    ] {
        let mut values = settings(&key, &json!([]));
        values.insert("url".to_owned(), json!(url));
        let Err(error) = ElasticToolkitConfig::parse(&values) else {
            panic!("invalid Elasticsearch configuration must fail");
        };
        assert_eq!(error.code(), ElasticConfigErrorCode::InvalidConfiguration);
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains(API_KEY));
        assert!(!diagnostic.contains("elastic.example"));
    }
}

#[tokio::test]
async fn catalog_description_schema_selection_and_policy_are_model_truthful() {
    let api: Arc<dyn ElasticApi> = Arc::new(FixtureApi::new());
    let toolset = test_build_with_api("logs-prod", &[], &policy(&[]), &api)
        .expect("complete Elasticsearch fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Elasticsearch tools");
    assert_eq!(test_catalog(), [("search_elastic_index", "read")]);
    assert_eq!(tools.len(), 1);
    assert!(tools[0].is_read_only());
    assert!(tools[0].is_concurrency_safe());
    assert!(tools[0].description().len() <= 1_000);
    assert!(!tools[0].description().contains(CLUSTER_URL));
    assert!(!tools[0].description().contains(API_KEY));
    for cue in [
        "Elasticsearch or REST-compatible OpenSearch",
        "index, data stream, alias",
        "size defaults",
        "POST /{index}/_search",
        "without redirect, automatic retry, scroll, or continuation",
        "512 KiB",
        "confidential indexed data",
        "Broad wildcards, scripts, runtime fields",
    ] {
        assert!(tools[0].description().contains(cue), "missing cue: {cue}");
    }
    let schema = tools[0].parameters_schema().expect("Elasticsearch schema");
    assert_eq!(schema["title"], "SearchElasticIndexModel");
    assert_eq!(schema["required"], json!(["index", "query"]));
    assert_eq!(schema["additionalProperties"], false);
    assert_eq!(schema["properties"]["index"]["maxLength"], 1024);
    assert_eq!(schema["properties"]["query"]["maxLength"], 16_384);
    assert!(
        schema["properties"]["index"]["description"]
            .as_str()
            .is_some_and(|value| value.contains("Elasticsearch or OpenSearch"))
    );
    assert!(
        schema["properties"]["query"]["description"]
            .as_str()
            .is_some_and(|value| value.contains("diverged since Elasticsearch 7.10.2"))
    );

    let blocked = test_build_with_api(
        "logs-prod",
        &[],
        &policy(&[("elastic", &["search_elastic_index"])]),
        &api,
    )
    .expect("blocked Elasticsearch definition");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert!(
        blocked
            .tools(readonly)
            .await
            .expect("blocked tools")
            .is_empty()
    );
    let Err(error) = test_build_with_api("logs-prod", &["unknown".to_owned()], &policy(&[]), &api)
    else {
        panic!("unknown Elasticsearch selection must fail");
    };
    assert_eq!(error.code(), ElasticToolsetErrorCode::UnsupportedSelection);
}

#[tokio::test]
async fn tool_dispatch_parses_query_and_rejects_bad_arguments_before_api() {
    let api = Arc::new(FixtureApi::new());
    let api_trait: Arc<dyn ElasticApi> = api.clone();
    let toolset = test_build_with_api("elastic", &[], &policy(&[]), &api_trait)
        .expect("Elasticsearch fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tool = toolset
        .tools(readonly)
        .await
        .expect("Elasticsearch tools")
        .remove(0);
    let result = tool
        .execute(
            context(),
            json!({
                "index":"logs-2026.08-*",
                "query":"{\"size\":25,\"query\":{\"match\":{\"message\":\"timeout\"}}}"
            }),
        )
        .await
        .expect("Elasticsearch tool result");
    assert_eq!(result, json!({"hits":{"hits":[]}}));
    assert_eq!(api.calls().len(), 1);

    for arguments in [
        json!({"index":"logs","query":"[]"}),
        json!({"index":"logs","query":"not-json"}),
        json!({"index":"../logs","query":"{}"}),
        json!({"index":"logs","query":"{\"size\":101}"}),
        json!({"index":"logs","query":"{}","unknown":true}),
    ] {
        assert!(tool.execute(context(), arguments).await.is_err());
    }
    assert_eq!(api.calls().len(), 1);
}

#[test]
fn index_and_query_admission_are_bounded_and_search_specific() {
    for index in [
        "logs",
        "logs-2026.08-*",
        "logs-current,logs-archive",
        "metrics-?",
    ] {
        assert!(
            validate_index(index).is_ok(),
            "rejected valid index: {index}"
        );
    }
    for index in [
        "",
        ",logs",
        "logs,",
        "../logs",
        "_all",
        "-logs",
        "remote:logs",
        "logs/path",
        "logs%2A",
        "*",
        "?logs",
    ] {
        assert!(
            validate_index(index).is_err(),
            "accepted invalid index: {index}"
        );
    }
    assert!(validate_query(&json!({})).is_ok());
    assert!(validate_query(&json!({"size":100,"from":10_000})).is_ok());
    assert!(validate_query(&json!([])).is_err());
    assert!(validate_query(&json!({"size":101})).is_err());
    assert!(validate_query(&json!({"from":10_001})).is_err());
    assert!(validate_query(&json!({"size":"10"})).is_err());
    assert!(validate_query(&json!({"value":"x".repeat(64 * 1_024)})).is_err());
}

#[test]
fn request_wire_is_exact_post_search_json_and_encoded_api_key() {
    let client = ElasticClient::with_transport(config(), Arc::new(FixtureTransport::new([])));
    let query = json!({"size":10,"query":{"term":{"service":"api"}}});
    let request = client
        .test_request("logs-2026.08-*", &query)
        .expect("Elasticsearch request");
    assert_eq!(request.method(), reqwest::Method::POST);
    assert_eq!(
        request.url().as_str(),
        "https://elastic.example.test:9243/logs-2026.08-*/_search"
    );
    assert_eq!(
        request
            .headers()
            .get(reqwest::header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok()),
        Some(format!("ApiKey {API_KEY}").as_str())
    );
    assert_eq!(
        request
            .headers()
            .get(reqwest::header::CONTENT_TYPE)
            .and_then(|value| value.to_str().ok()),
        Some("application/json")
    );
    assert_eq!(
        request.body().and_then(reqwest::Body::as_bytes),
        Some(serde_json::to_vec(&query).expect("query JSON").as_slice())
    );
}

#[test]
fn unauthenticated_configuration_emits_no_authorization_header() {
    let config = ElasticToolkitConfig::parse(&settings(&Value::Null, &json!([])))
        .expect("anonymous Elasticsearch config");
    let client = ElasticClient::with_transport(config, Arc::new(FixtureTransport::new([])));
    let request = client
        .test_request("public-logs", &json!({}))
        .expect("anonymous Elasticsearch request");
    assert!(
        request
            .headers()
            .get(reqwest::header::AUTHORIZATION)
            .is_none()
    );
}

#[tokio::test]
async fn native_and_vendor_json_success_are_projected_without_following_pages() {
    for content_type in [
        "application/json",
        "application/vnd.elasticsearch+json;compatible-with=8",
        "application/vnd.opensearch+json",
    ] {
        let transport = Arc::new(FixtureTransport::new([Ok(ElasticHttpResponse::fixture(
            StatusCode::OK,
            Some(content_type),
            br#"{"took":3,"hits":{"total":{"value":1},"hits":[{"_id":"1"}]}}"#,
        ))]));
        let result = client_with(transport.clone())
            .search("logs", &json!({"size":10}))
            .await
            .expect("Elasticsearch result");
        assert_eq!(result["took"], 3);
        assert_eq!(result["hits"]["hits"][0]["_id"], "1");
        let requests = transport.requests();
        assert_eq!(requests.len(), 1);
        assert_eq!(requests[0].method, "POST");
        assert_eq!(
            requests[0].authorization.as_deref(),
            Some(format!("ApiKey {API_KEY}").as_str())
        );
        assert_eq!(requests[0].accept.as_deref(), Some("application/json"));
        assert_eq!(
            requests[0].content_type.as_deref(),
            Some("application/json")
        );
    }
}

#[tokio::test]
async fn status_taxonomy_is_stable_retryable_and_provider_data_free() {
    for (status, code, retryable) in [
        (
            StatusCode::BAD_REQUEST,
            ElasticClientErrorCode::InvalidInput,
            false,
        ),
        (
            StatusCode::UNAUTHORIZED,
            ElasticClientErrorCode::Authentication,
            false,
        ),
        (
            StatusCode::FORBIDDEN,
            ElasticClientErrorCode::Authorization,
            false,
        ),
        (
            StatusCode::NOT_FOUND,
            ElasticClientErrorCode::NotFound,
            false,
        ),
        (
            StatusCode::REQUEST_TIMEOUT,
            ElasticClientErrorCode::Timeout,
            true,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            ElasticClientErrorCode::RateLimited,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            ElasticClientErrorCode::DependencyUnavailable,
            true,
        ),
    ] {
        let transport = Arc::new(FixtureTransport::new([Ok(ElasticHttpResponse::fixture(
            status,
            Some("application/json"),
            br#"{"error":"provider-secret"}"#,
        ))]));
        let error = client_with(transport)
            .search("logs", &json!({}))
            .await
            .expect_err("Elasticsearch status failure");
        assert_eq!(error.code(), code);
        assert_eq!(error.retryable(), retryable);
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains("provider-secret"));
        assert!(!diagnostic.contains(CLUSTER_URL));
        assert!(!diagnostic.contains(API_KEY));
    }
}

#[tokio::test]
async fn malformed_nonobject_and_oversized_results_fail_closed() {
    for (body, content_type, code) in [
        (
            b"not-json".to_vec(),
            Some("application/json"),
            ElasticClientErrorCode::InvalidResponse,
        ),
        (
            b"[]".to_vec(),
            Some("application/json"),
            ElasticClientErrorCode::InvalidResponse,
        ),
        (
            br#"{"hits":{}}"#.to_vec(),
            Some("text/html"),
            ElasticClientErrorCode::InvalidResponse,
        ),
        (
            json!({"value":"x".repeat(512 * 1_024)})
                .to_string()
                .into_bytes(),
            Some("application/json"),
            ElasticClientErrorCode::ResourceExhausted,
        ),
    ] {
        let transport = Arc::new(FixtureTransport::new([Ok(ElasticHttpResponse::fixture(
            StatusCode::OK,
            content_type,
            body,
        ))]));
        let error = client_with(transport)
            .search("logs", &json!({}))
            .await
            .expect_err("invalid Elasticsearch result");
        assert_eq!(error.code(), code);
    }
}

#[test]
fn two_clients_never_cross_cluster_or_api_key_authority() {
    let first = ElasticToolkitConfig::parse(&settings(&json!("Zmlyc3Q="), &json!([])))
        .expect("first Elasticsearch config");
    let mut second_settings = settings(&json!("c2Vjb25k"), &json!([]));
    second_settings.insert(
        "url".to_owned(),
        json!("https://second-elastic.example.test:443"),
    );
    let second =
        ElasticToolkitConfig::parse(&second_settings).expect("second Elasticsearch config");
    let first = ElasticClient::with_transport(first, Arc::new(FixtureTransport::new([])));
    let second = ElasticClient::with_transport(second, Arc::new(FixtureTransport::new([])));
    let first = first
        .test_request("logs", &json!({}))
        .expect("first request");
    let second = second
        .test_request("logs", &json!({}))
        .expect("second request");
    assert_eq!(first.url().host_str(), Some("elastic.example.test"));
    assert_eq!(second.url().host_str(), Some("second-elastic.example.test"));
    assert_eq!(
        first
            .headers()
            .get(reqwest::header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok()),
        Some("ApiKey Zmlyc3Q=")
    );
    assert_eq!(
        second
            .headers()
            .get(reqwest::header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok()),
        Some("ApiKey c2Vjb25k")
    );
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn ElasticApi> = Arc::new(FixtureApi::new());
    let toolset =
        test_build_with_api("gate", &[], &policy(&[]), &api).expect("complete elastic toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("elastic tools");
    super::sdk_conformance::assert_sdk_conformance("elastic", &tools);
}
