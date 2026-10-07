use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Toolset};
use async_trait::async_trait;
use reqwest::header::{ACCEPT, CONTENT_TYPE};
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::azure_search::client::{
    AzureSearchApi, AzureSearchClient, AzureSearchClientError, AzureSearchClientErrorCode,
    AzureSearchRequestKind, AzureSearchTransport, test_http_status,
};
use super::families::azure_search::config::{AzureSearchConfigErrorCode, AzureSearchToolkitConfig};
use super::families::azure_search::tools::{
    AzureSearchToolsetErrorCode, build_azure_search_read_only_toolset, test_build_with_api,
};
use super::policy::ToolAdmissionPolicy;

fn settings(selected_tools: &[&str]) -> Map<String, Value> {
    settings_with_authority(
        selected_tools,
        "https://fixture.search.windows.net",
        "fixture-search-key",
        "documents-v1",
    )
}

fn settings_with_authority(
    selected_tools: &[&str],
    endpoint: &str,
    api_key: &str,
    index_name: &str,
) -> Map<String, Value> {
    json!({
        "azure_search_configuration": {
            "configuration_type": "azure_search",
            "configuration_uuid": "config-fixture",
            "configuration_project_id": 7,
            "endpoint": endpoint,
            "api_base": null,
            "api_key": api_key
        },
        "index_name": index_name,
        "api_version": "ignored-by-current-sdk",
        "openai_api_key": "unused-openai-key",
        "model_name": "unused-embedding-model",
        "selected_tools": selected_tools
    })
    .as_object()
    .expect("Azure Search settings fixture")
    .clone()
}

fn config(selected_tools: &[&str]) -> AzureSearchToolkitConfig {
    AzureSearchToolkitConfig::parse(&settings(selected_tools))
        .expect("valid Azure Search configuration")
}

fn policy(blocked: &[(&str, &[&str])]) -> Arc<ToolAdmissionPolicy> {
    let blocked = blocked
        .iter()
        .map(|(toolkit, tools)| {
            (
                (*toolkit).to_owned(),
                tools.iter().map(ToString::to_string).collect::<Vec<_>>(),
            )
        })
        .collect::<BTreeMap<_, _>>();
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Azure Search policy fixture"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("azure-search-tool-test")
            .with_session_id("session-1")
            .with_function_call_id("call-1"),
    )
}

#[test]
fn materialized_configuration_is_bounded_deduplicated_and_secret_safe() {
    let parsed = config(&["text_search", "text_search", "get_document"]);
    assert_eq!(
        parsed.selected_tools(),
        [
            Box::<str>::from("text_search"),
            Box::<str>::from("get_document")
        ]
    );

    for invalid in [
        json!({"selected_tools": []}),
        json!({
            "azure_search_configuration": {
                "endpoint": "http://fixture.search.windows.net",
                "api_key": "fixture-secret"
            },
            "index_name": "documents-v1",
            "selected_tools": []
        }),
        json!({
            "azure_search_configuration": {
                "endpoint": "https://fixture.search.windows.net/path",
                "api_key": "fixture-secret"
            },
            "index_name": "documents-v1",
            "selected_tools": []
        }),
        json!({
            "azure_search_configuration": {
                "endpoint": "https://fixture.search.windows.net",
                "api_key": "fixture\nsecret"
            },
            "index_name": "documents-v1",
            "selected_tools": []
        }),
        json!({
            "azure_search_configuration": {
                "endpoint": "https://fixture.search.windows.net",
                "api_key": "fixture-secret"
            },
            "index_name": "Invalid_Index",
            "selected_tools": []
        }),
    ] {
        let object = invalid
            .as_object()
            .expect("invalid Azure Search fixture object");
        let Err(error) = AzureSearchToolkitConfig::parse(object) else {
            panic!("malformed configuration must fail before client construction");
        };
        assert_eq!(
            error.code(),
            AzureSearchConfigErrorCode::InvalidConfiguration
        );
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains("fixture-secret"));
        assert!(!diagnostic.contains("fixture.search.windows.net"));
    }

    let oversized = settings_with_authority(
        &[],
        "https://fixture.search.windows.net",
        &"x".repeat(8 * 1_024 + 1),
        "documents-v1",
    );
    let Err(error) = AzureSearchToolkitConfig::parse(&oversized) else {
        panic!("oversized API key must fail closed");
    };
    assert_eq!(error.code(), AzureSearchConfigErrorCode::ResourceExhausted);
}

#[test]
fn requests_match_the_pinned_sdk_wire_and_bind_the_configured_authority() {
    let client = AzureSearchClient::new(config(&[])).expect("Azure Search client");
    let order_by = vec!["rank desc".to_owned(), "search.score()".to_owned()];
    let fields = vec!["id".to_owned(), "Address/City".to_owned()];
    let search = client
        .test_request(AzureSearchRequestKind::TextSearch {
            search_text: "rust worker",
            limit: 7,
            order_by: &order_by,
            selected_fields: &fields,
        })
        .expect("search request");
    assert_eq!(search.method(), Method::POST);
    assert_eq!(
        search.url().as_str(),
        "https://fixture.search.windows.net/indexes('documents-v1')/docs/search.post.search?api-version=2024-07-01"
    );
    assert_eq!(
        search.headers().get(ACCEPT).expect("Azure accept header"),
        "application/json;odata.metadata=none"
    );
    assert_eq!(
        search
            .headers()
            .get(CONTENT_TYPE)
            .expect("Azure content type"),
        "application/json"
    );
    assert!(
        search
            .headers()
            .get("api-key")
            .expect("Azure API key")
            .is_sensitive()
    );
    assert_eq!(search.timeout(), Some(&Duration::from_secs(20)));
    let body: Value = serde_json::from_slice(
        search
            .body()
            .and_then(reqwest::Body::as_bytes)
            .expect("bounded Azure request body"),
    )
    .expect("Azure request JSON");
    assert_eq!(
        body,
        json!({
            "search": "rust worker",
            "top": 7,
            "orderby": "rank desc,search.score()",
            "select": "id,Address/City"
        })
    );

    let document = client
        .test_request(AzureSearchRequestKind::GetDocument {
            document_id: "folder/doc'1",
            selected_fields: &fields,
        })
        .expect("document request");
    assert_eq!(document.method(), Method::GET);
    assert_eq!(
        document.url().path(),
        "/indexes('documents-v1')/docs('folder%2Fdoc%271')"
    );
    let query = document.url().query_pairs().collect::<Vec<_>>();
    assert!(
        query
            .iter()
            .any(|(key, value)| key == "$select" && value == "id,Address/City")
    );
    assert!(
        query
            .iter()
            .any(|(key, value)| key == "api-version" && value == "2024-07-01")
    );
}

#[test]
fn empty_projection_lists_are_omitted_and_query_controls_are_bounded() {
    let client = AzureSearchClient::new(config(&[])).expect("Azure Search client");
    let search = client
        .test_request(AzureSearchRequestKind::TextSearch {
            search_text: "",
            limit: 100,
            order_by: &[],
            selected_fields: &[],
        })
        .expect("empty lists are cleanly omitted");
    let body: Value = serde_json::from_slice(
        search
            .body()
            .and_then(reqwest::Body::as_bytes)
            .expect("search request body"),
    )
    .expect("search request JSON");
    assert_eq!(body, json!({"search": "", "top": 100}));

    let thirty_two = (0..32)
        .map(|index| format!("field_{index} asc"))
        .collect::<Vec<_>>();
    client
        .test_request(AzureSearchRequestKind::TextSearch {
            search_text: "bounded",
            limit: 1,
            order_by: &thirty_two,
            selected_fields: &["*".to_owned()],
        })
        .expect("32 order clauses are allowed");
    let thirty_three = (0..33)
        .map(|index| format!("field_{index} asc"))
        .collect::<Vec<_>>();
    let error = client
        .test_request(AzureSearchRequestKind::TextSearch {
            search_text: "bounded",
            limit: 1,
            order_by: &thirty_three,
            selected_fields: &[],
        })
        .expect_err("33 order clauses must fail closed");
    assert_eq!(error.code(), AzureSearchClientErrorCode::ResourceExhausted);
}

#[derive(Default)]
struct FixtureTransport {
    requests: Mutex<Vec<Request>>,
    responses: Mutex<Vec<Value>>,
}

impl FixtureTransport {
    fn with_responses(responses: Vec<Value>) -> Self {
        Self {
            requests: Mutex::new(Vec::new()),
            responses: Mutex::new(responses.into_iter().rev().collect()),
        }
    }

    fn request_count(&self) -> usize {
        self.requests
            .lock()
            .expect("Azure Search request fixture lock")
            .len()
    }
}

#[async_trait]
impl AzureSearchTransport for FixtureTransport {
    async fn execute_json(&self, request: Request) -> Result<Value, AzureSearchClientError> {
        self.requests
            .lock()
            .expect("Azure Search request fixture lock")
            .push(request);
        self.responses
            .lock()
            .expect("Azure Search response fixture lock")
            .pop()
            .ok_or_else(|| {
                test_http_status(StatusCode::SERVICE_UNAVAILABLE)
                    .expect_err("fixture status must fail")
            })
    }
}

#[tokio::test]
async fn search_projects_the_sdk_result_shape_without_following_continuation() {
    let transport = Arc::new(FixtureTransport::with_responses(vec![json!({
        "@odata.nextLink": "https://attacker.example.test/escape",
        "@search.nextPageParameters": {"skip": 2},
        "value": [
            {"id": "one", "@search.score": 0.9, "@search.rerankerScore": 2.5},
            {"id": "two"}
        ]
    })]));
    let client = AzureSearchClient::test_with_transport(
        config(&[]),
        transport.clone() as Arc<dyn AzureSearchTransport>,
    );
    let output = client
        .text_search("rust", 2, &[], &[])
        .await
        .expect("bounded search output");
    assert_eq!(
        output,
        json!([
            {
                "id": "one",
                "@search.score": 0.9,
                "@search.reranker_score": 2.5,
                "@search.highlights": null,
                "@search.captions": null
            },
            {
                "id": "two",
                "@search.score": null,
                "@search.reranker_score": null,
                "@search.highlights": null,
                "@search.captions": null
            }
        ])
    );
    assert_eq!(transport.request_count(), 1);
}

#[tokio::test]
async fn get_document_preserves_the_provider_dictionary() {
    let document = json!({
        "id": "one",
        "nested": {"value": true},
        "tags": ["rust", "worker"]
    });
    let transport = Arc::new(FixtureTransport::with_responses(vec![document.clone()]));
    let client = AzureSearchClient::test_with_transport(
        config(&[]),
        transport.clone() as Arc<dyn AzureSearchTransport>,
    );
    assert_eq!(
        client
            .get_document("one", &[])
            .await
            .expect("bounded document"),
        document
    );
    assert_eq!(transport.request_count(), 1);
}

#[tokio::test]
async fn provider_result_and_output_bounds_fail_closed() {
    let transport = Arc::new(FixtureTransport::with_responses(vec![json!({
        "value": [{"id": 1}, {"id": 2}]
    })]));
    let client = AzureSearchClient::test_with_transport(
        config(&[]),
        transport as Arc<dyn AzureSearchTransport>,
    );
    let error = client
        .text_search("bounded", 1, &[], &[])
        .await
        .expect_err("provider result count beyond top must fail");
    assert_eq!(error.code(), AzureSearchClientErrorCode::ResourceExhausted);

    let transport = Arc::new(FixtureTransport::with_responses(vec![json!({
        "id": "x",
        "payload": "x".repeat(512 * 1_024)
    })]));
    let client = AzureSearchClient::test_with_transport(
        config(&[]),
        transport as Arc<dyn AzureSearchTransport>,
    );
    let error = client
        .get_document("one", &[])
        .await
        .expect_err("oversized document output must fail");
    assert_eq!(error.code(), AzureSearchClientErrorCode::ResourceExhausted);
    assert!(!error.retryable());
}

#[test]
fn status_taxonomy_is_stable_and_only_transient_failures_retry() {
    for (status, code, retryable) in [
        (
            StatusCode::BAD_REQUEST,
            AzureSearchClientErrorCode::InvalidInput,
            false,
        ),
        (
            StatusCode::UNAUTHORIZED,
            AzureSearchClientErrorCode::Authentication,
            false,
        ),
        (
            StatusCode::FORBIDDEN,
            AzureSearchClientErrorCode::Authorization,
            false,
        ),
        (
            StatusCode::NOT_FOUND,
            AzureSearchClientErrorCode::NotFound,
            false,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            AzureSearchClientErrorCode::RateLimited,
            true,
        ),
        (
            StatusCode::GATEWAY_TIMEOUT,
            AzureSearchClientErrorCode::Timeout,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            AzureSearchClientErrorCode::DependencyUnavailable,
            true,
        ),
    ] {
        let error = test_http_status(status).expect_err("Azure Search HTTP failure");
        assert_eq!(error.code(), code);
        assert_eq!(error.retryable(), retryable);
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains("fixture-search-key"));
        assert!(!diagnostic.contains("fixture.search.windows.net"));
    }
}

#[test]
fn invocation_scoped_clients_do_not_overwrite_authority() {
    let first = AzureSearchClient::new(
        AzureSearchToolkitConfig::parse(&settings_with_authority(
            &[],
            "https://first.search.windows.net",
            "first-key",
            "first-index",
        ))
        .expect("first Azure Search config"),
    )
    .expect("first Azure Search client");
    let second = AzureSearchClient::new(
        AzureSearchToolkitConfig::parse(&settings_with_authority(
            &[],
            "https://second.search.windows.net",
            "second-key",
            "second-index",
        ))
        .expect("second Azure Search config"),
    )
    .expect("second Azure Search client");
    let first = first
        .test_request(AzureSearchRequestKind::TextSearch {
            search_text: "first",
            limit: 1,
            order_by: &[],
            selected_fields: &[],
        })
        .expect("first request");
    let second = second
        .test_request(AzureSearchRequestKind::TextSearch {
            search_text: "second",
            limit: 1,
            order_by: &[],
            selected_fields: &[],
        })
        .expect("second request");
    assert_eq!(first.url().host_str(), Some("first.search.windows.net"));
    assert!(first.url().path().contains("first-index"));
    assert_eq!(second.url().host_str(), Some("second.search.windows.net"));
    assert!(second.url().path().contains("second-index"));
    assert_ne!(
        first.headers().get("api-key"),
        second.headers().get("api-key")
    );
}

#[derive(Default)]
struct FixtureAzureSearchApi {
    calls: Mutex<Vec<Value>>,
}

#[async_trait]
impl AzureSearchApi for FixtureAzureSearchApi {
    async fn text_search(
        &self,
        search_text: &str,
        limit: usize,
        order_by: &[String],
        selected_fields: &[String],
    ) -> Result<Value, AzureSearchClientError> {
        self.calls
            .lock()
            .expect("Azure Search API fixture lock")
            .push(json!({
                "tool": "text_search",
                "search_text": search_text,
                "limit": limit,
                "order_by": order_by,
                "selected_fields": selected_fields
            }));
        Ok(json!([]))
    }

    async fn get_document(
        &self,
        document_id: &str,
        selected_fields: &[String],
    ) -> Result<Value, AzureSearchClientError> {
        self.calls
            .lock()
            .expect("Azure Search API fixture lock")
            .push(json!({
                "tool": "get_document",
                "document_id": document_id,
                "selected_fields": selected_fields
            }));
        Ok(json!({"id": document_id}))
    }
}

#[tokio::test]
async fn native_tools_preserve_order_defaults_read_classification_and_policy() {
    let api = Arc::new(FixtureAzureSearchApi::default());
    let toolset = test_build_with_api(
        "knowledge",
        &[],
        &policy(&[]),
        &(api.clone() as Arc<dyn AzureSearchApi>),
    )
    .expect("native Azure Search toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Azure Search tools");
    assert_eq!(tools.len(), 2);
    assert_eq!(tools[0].name(), "text_search");
    assert_eq!(tools[1].name(), "get_document");
    assert!(tools.iter().all(|tool| tool.is_read_only()));
    assert!(tools.iter().all(|tool| tool.is_concurrency_safe()));
    let search_schema = tools[0]
        .parameters_schema()
        .expect("text search has an argument schema");
    assert!(
        search_schema["properties"]["search_text"]["description"]
            .as_str()
            .expect("search text description is text")
            .contains("empty string to match all")
    );
    let ordering = search_schema["properties"]["order_by"]["description"]
        .as_str()
        .expect("order description is text");
    assert!(ordering.contains("rating desc"));
    assert!(ordering.contains("search.score() desc"));
    let selected = search_schema["properties"]["selected_fields"]["description"]
        .as_str()
        .expect("selected fields description is text");
    assert!(selected.contains("address/city"));
    assert!(selected.contains("`*`"));

    tools[0]
        .execute(
            context(),
            json!({
                "search_text": "rust",
                "limit": -1,
                "order_by": [],
                "selected_fields": []
            }),
        )
        .await
        .expect("bounded search call");
    tools[1]
        .execute(
            context(),
            json!({"document_id": "doc/1", "selected_fields": ["id", "body/text"]}),
        )
        .await
        .expect("document call");
    assert_eq!(
        api.calls
            .lock()
            .expect("Azure Search API fixture lock")
            .as_slice(),
        &[
            json!({
                "tool": "text_search",
                "search_text": "rust",
                "limit": 100,
                "order_by": [],
                "selected_fields": []
            }),
            json!({
                "tool": "get_document",
                "document_id": "doc/1",
                "selected_fields": ["id", "body/text"]
            })
        ]
    );

    let blocked = test_build_with_api(
        "knowledge",
        &[],
        &policy(&[("azure_search", &["text_search"])]),
        &(api.clone() as Arc<dyn AzureSearchApi>),
    )
    .expect("policy-filtered Azure Search toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = blocked
        .tools(readonly)
        .await
        .expect("policy-filtered tools");
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].name(), "get_document");
}

#[tokio::test]
async fn invalid_tool_arguments_never_reach_the_family_client() {
    let api = Arc::new(FixtureAzureSearchApi::default());
    let toolset = test_build_with_api(
        "knowledge",
        &[],
        &policy(&[]),
        &(api.clone() as Arc<dyn AzureSearchApi>),
    )
    .expect("native Azure Search toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Azure Search tools");
    for arguments in [
        json!({}),
        json!({"search_text": "x", "limit": 0}),
        json!({"search_text": "x", "limit": -2}),
        json!({"search_text": "x", "limit": 101}),
        json!({"search_text": "x", "selected_fields": ["id,name"]}),
        json!({"search_text": "x", "unexpected": true}),
    ] {
        assert!(tools[0].execute(context(), arguments).await.is_err());
    }
    for arguments in [
        json!({}),
        json!({"document_id": ""}),
        json!({"document_id": "one", "selected_fields": ["bad field"]}),
        json!({"document_id": "one", "unexpected": true}),
    ] {
        assert!(tools[1].execute(context(), arguments).await.is_err());
    }
    assert!(
        api.calls
            .lock()
            .expect("Azure Search API fixture lock")
            .is_empty()
    );
}

#[test]
fn unsupported_selection_fails_before_provider_use() {
    let Err(error) =
        build_azure_search_read_only_toolset("knowledge", config(&["vector_search"]), &policy(&[]))
    else {
        panic!("unregistered Azure Search tool must fail closed");
    };
    assert_eq!(
        error.code(),
        AzureSearchToolsetErrorCode::UnsupportedSelection
    );
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn AzureSearchApi> = Arc::new(FixtureAzureSearchApi::default());
    let toolset = test_build_with_api("gate", &[], &policy(&[]), &api)
        .expect("complete azure_search toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("azure_search tools");
    super::sdk_conformance::assert_sdk_conformance("azure_search", &tools);
}
