//! Focused compatibility and safety tests for the capability-disabled Aha family.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, ToolContext, Toolset};
use async_trait::async_trait;
use bytes::Bytes;
use reqwest::header::{AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE};
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::aha::artifact::{AhaArtifactFixture, AhaArtifactResolver};
use super::families::aha::client::{
    AhaApi, AhaClient, AhaClientError, AhaClientErrorCode, AhaHttpResponse, AhaOperation,
    AhaTransport, PageOptions, ReadOptions,
};
use super::families::aha::config::{AhaConfigErrorCode, AhaToolkitConfig};
use super::families::aha::format::OutputFormat;
use super::families::aha::tools::{
    AhaToolsetErrorCode, build_aha_toolset, test_build_with_api, test_catalog,
};
use super::policy::ToolAdmissionPolicy;

const API_KEY: &str = "aha-private-api-key";

fn settings(selected: Option<Value>) -> Map<String, Value> {
    let mut settings = json!({
        "aha_configuration": {
            "base_url": "https://tenant.example.test",
            "api_key": API_KEY
        }
    })
    .as_object()
    .cloned()
    .expect("Aha settings fixture is an object");
    if let Some(selected) = selected {
        settings.insert("selected_tools".to_owned(), selected);
    }
    settings
}

fn config() -> AhaToolkitConfig {
    AhaToolkitConfig::parse(&settings(Some(json!([])))).expect("valid Aha fixture")
}

fn empty_artifacts() -> Arc<AhaArtifactResolver> {
    Arc::new(AhaArtifactResolver::fixture(Vec::new()).expect("empty artifact fixture"))
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Aha policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("aha-test").with_function_call_id("aha-call"))
}

fn read(fields: &[Box<str>], output_format: OutputFormat) -> ReadOptions<'_> {
    ReadOptions {
        output_format,
        fields,
    }
}

const PAGE: PageOptions = PageOptions {
    per_page: 25,
    max_records: 100,
};

#[test]
fn catalog_preserves_source_order_and_groups() {
    assert_eq!(
        test_catalog(),
        vec![
            ("get_feature", "read"),
            ("get_requirement", "read"),
            ("get_release", "read"),
            ("get_initiative", "read"),
            ("get_epic", "read"),
            ("get_idea", "read"),
            ("get_product", "read"),
            ("list_products", "read"),
            ("list_features", "read"),
            ("list_requirements", "read"),
            ("list_releases", "read"),
            ("list_initiatives", "read"),
            ("list_epics", "read"),
            ("list_ideas", "read"),
            ("search", "read"),
            ("get_page", "read"),
            ("search_documents", "read"),
            ("get_feature_gql", "read"),
            ("get_requirement_gql", "read"),
            ("find_project", "read"),
            ("search_records", "read"),
            ("read_records", "read"),
            ("add_comment", "write"),
            ("list_comments", "read"),
            ("manage_record", "execute"),
            ("create_record", "write"),
            ("update_record", "write"),
            ("delete_record", "delete"),
            ("create_record_link", "write"),
            ("copy_record", "write"),
            ("fields_metadata", "read"),
            ("field_options_metadata", "read"),
            ("attach_file", "write"),
        ]
    );
    assert_eq!(
        test_catalog()
            .iter()
            .filter(|(_, group)| *group == "read")
            .count(),
        25
    );
}

#[test]
fn config_is_nested_exact_origin_secret_safe_and_selection_is_deterministic() {
    for selected in [None, Some(Value::Null), Some(json!([]))] {
        let parsed = AhaToolkitConfig::parse(&settings(selected)).expect("all-tools selection");
        assert!(parsed.selected_tools().is_empty());
    }

    let selected = AhaToolkitConfig::parse(&settings(Some(json!([
        "attach_file",
        "get_feature",
        "attach_file"
    ]))))
    .expect("deduplicated selection");
    assert_eq!(
        selected
            .selected_tools()
            .iter()
            .map(AsRef::as_ref)
            .collect::<Vec<&str>>(),
        ["attach_file", "get_feature"]
    );

    for invalid_url in [
        "http://tenant.example.test",
        "https://user@tenant.example.test",
        "https://tenant.example.test/path",
        "https://tenant.example.test?query=yes",
        "https://tenant.example.test#fragment",
    ] {
        let mut invalid = settings(Some(json!([])));
        invalid
            .get_mut("aha_configuration")
            .and_then(Value::as_object_mut)
            .expect("nested config")
            .insert("base_url".to_owned(), Value::String(invalid_url.to_owned()));
        let Err(error) = AhaToolkitConfig::parse(&invalid) else {
            panic!("invalid origin must fail");
        };
        assert_eq!(error.code(), AhaConfigErrorCode::InvalidConfiguration);
        assert!(!format!("{error:?}").contains(API_KEY));
    }

    let mut custom = settings(Some(json!([])));
    custom
        .get_mut("aha_configuration")
        .and_then(Value::as_object_mut)
        .expect("nested config")
        .insert(
            "base_url".to_owned(),
            Value::String("https://roadmaps.corporate.example".to_owned()),
        );
    assert!(
        AhaToolkitConfig::parse(&custom).is_ok(),
        "custom Aha CNAME is valid"
    );
}

#[derive(Default)]
struct FixtureApi {
    calls: Mutex<Vec<String>>,
}

#[async_trait]
impl AhaApi for FixtureApi {
    async fn execute(&self, operation: AhaOperation<'_>) -> Result<Value, AhaClientError> {
        let name = match operation {
            AhaOperation::GetFeature { .. } => "get_feature",
            AhaOperation::GetRequirement { .. } => "get_requirement",
            AhaOperation::GetRelease { .. } => "get_release",
            AhaOperation::GetInitiative { .. } => "get_initiative",
            AhaOperation::GetEpic { .. } => "get_epic",
            AhaOperation::GetIdea { .. } => "get_idea",
            AhaOperation::GetProduct { .. } => "get_product",
            AhaOperation::ListProducts { .. } => "list_products",
            AhaOperation::ListFeatures { .. } => "list_features",
            AhaOperation::ListRequirements { .. } => "list_requirements",
            AhaOperation::ListReleases { .. } => "list_releases",
            AhaOperation::ListInitiatives { .. } => "list_initiatives",
            AhaOperation::ListEpics { .. } => "list_epics",
            AhaOperation::ListIdeas { .. } => "list_ideas",
            AhaOperation::Search { .. } => "search",
            AhaOperation::GetPage { .. } => "get_page",
            AhaOperation::SearchDocuments { .. } => "search_documents",
            AhaOperation::GetFeatureGql { .. } => "get_feature_gql",
            AhaOperation::GetRequirementGql { .. } => "get_requirement_gql",
            AhaOperation::FindProject { .. } => "find_project",
            AhaOperation::SearchRecords { .. } => "search_records",
            AhaOperation::ReadRecords { .. } => "read_records",
            AhaOperation::AddComment { .. } => "add_comment",
            AhaOperation::ListComments { .. } => "list_comments",
            AhaOperation::ManageRecord { .. } => "manage_record",
            AhaOperation::CreateRecord { .. } => "create_record",
            AhaOperation::UpdateRecord { .. } => "update_record",
            AhaOperation::DeleteRecord { .. } => "delete_record",
            AhaOperation::CreateRecordLink { .. } => "create_record_link",
            AhaOperation::CopyRecord { .. } => "copy_record",
            AhaOperation::FieldsMetadata { .. } => "fields_metadata",
            AhaOperation::FieldOptionsMetadata { .. } => "field_options_metadata",
            AhaOperation::AttachFile { .. } => "attach_file",
        };
        self.calls
            .lock()
            .expect("Aha API calls")
            .push(name.to_owned());
        Ok(json!({"tool": name}))
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // Keeps exact catalog metadata in one contract test.
async fn catalog_metadata_selection_policy_and_schemas_match_runtime_contract() {
    let api = Arc::new(FixtureApi::default());
    let api_trait: Arc<dyn AhaApi> = api.clone();
    let toolset = test_build_with_api("product", &[], &policy(&[]), &api_trait)
        .expect("complete Aha toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Aha tools");
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        test_catalog()
            .iter()
            .map(|(name, _)| *name)
            .collect::<Vec<_>>()
    );

    for tool in &tools {
        let description = tool.description();
        assert!(description.starts_with("Toolkit: product\n"));
        assert!(
            description.len() > 90,
            "{} description has selection cues",
            tool.name()
        );
        let schema = tool.parameters_schema().expect("Aha schema");
        assert_eq!(schema.get("type"), Some(&json!("object")));
        assert_eq!(
            schema.get("additionalProperties"),
            Some(&Value::Bool(false))
        );
        assert_eq!(tool.is_concurrency_safe(), tool.is_read_only());
        if !tool.is_read_only() {
            assert!(description.contains("unknown") || description.contains("Unknown"));
            assert!(description.contains("reconcil"));
            assert!(description.contains("not") && description.contains("retr"));
        }
    }

    let find = |name: &str| tools.iter().find(|tool| tool.name() == name).expect("tool");
    let manage = find("manage_record")
        .parameters_schema()
        .expect("manage schema");
    assert_eq!(
        manage["properties"]["action"]["enum"],
        json!(["create", "update", "delete"])
    );
    assert_eq!(
        manage["properties"]["record_type"]["enum"]
            .as_array()
            .map(Vec::len),
        Some(7)
    );
    let search_records = find("search_records")
        .parameters_schema()
        .expect("search schema");
    assert_eq!(
        search_records["properties"]["record_type"]["enum"]
            .as_array()
            .map(Vec::len),
        Some(7)
    );
    let read_records = find("read_records")
        .parameters_schema()
        .expect("read schema");
    assert_eq!(
        read_records["properties"]["record_type"]["enum"]
            .as_array()
            .map(Vec::len),
        Some(8)
    );
    let attach = find("attach_file")
        .parameters_schema()
        .expect("attach schema");
    assert_eq!(attach["properties"]["filepath"]["maxLength"], json!(2048));
    assert!(
        find("attach_file")
            .description()
            .contains("strictly below 300 MB")
    );
    let attach_resource = attach["properties"]["resource_type"]["description"]
        .as_str()
        .expect("attachment resource description");
    for value in [
        "feature",
        "release_phase",
        "product",
        "to_do",
        "to-do",
        "tasks",
    ] {
        assert!(attach_resource.contains(value));
    }
    let add_comment = find("add_comment")
        .parameters_schema()
        .expect("comment schema");
    assert_eq!(add_comment["properties"]["body"]["maxLength"], json!(65536));
    let documents = find("search_documents")
        .parameters_schema()
        .expect("documents schema");
    assert_eq!(
        documents["properties"]["searchable_type"]["anyOf"][0]["maxLength"],
        json!(1024)
    );
    let output = &find("get_feature")
        .parameters_schema()
        .expect("read schema")["properties"]["output_format"]["anyOf"][0];
    assert_eq!(output["enum"], json!(["json", "csv", "markdown"]));
    let link = find("create_record_link")
        .parameters_schema()
        .expect("link schema");
    let link_description = link["properties"]["link_type"]["description"]
        .as_str()
        .expect("link type description");
    for mapping in [
        "10 relates to",
        "20 depends on",
        "30 duplicated by",
        "40 contained by",
        "50 impacted by",
        "60 blocked by",
        "80 research for",
    ] {
        assert!(link_description.contains(mapping));
        assert!(find("create_record_link").description().contains(mapping));
    }

    let selected = vec!["attach_file".to_owned(), "get_feature".to_owned()];
    let selected_set = test_build_with_api("product", &selected, &policy(&[]), &api_trait)
        .expect("selected Aha toolset");
    let selected_tools = selected_set.tools(context()).await.expect("selected tools");
    assert_eq!(
        selected_tools
            .iter()
            .map(|tool| tool.name())
            .collect::<Vec<_>>(),
        ["get_feature", "attach_file"]
    );

    let blocked = test_build_with_api(
        "product",
        &[],
        &policy(&[("aha", &["attach_file"])]),
        &api_trait,
    )
    .expect("policy filtered Aha toolset");
    assert_eq!(
        blocked.tools(context()).await.expect("blocked tools").len(),
        32
    );

    let unknown = AhaToolkitConfig::parse(&settings(Some(json!(["unknown_tool"]))))
        .expect("unknown selection remains data until closed by catalog");
    let Err(error) = build_aha_toolset("product", unknown, &policy(&[]), empty_artifacts()) else {
        panic!("unknown selection must fail closed");
    };
    assert_eq!(error.code(), AhaToolsetErrorCode::UnsupportedSelection);
}

#[tokio::test]
async fn shared_argument_envelope_and_properties_object_contract_fail_before_api_dispatch() {
    let api = Arc::new(FixtureApi::default());
    let api_trait: Arc<dyn AhaApi> = api.clone();
    let selected = vec![
        "add_comment".to_owned(),
        "manage_record".to_owned(),
        "create_record".to_owned(),
    ];
    let toolset = test_build_with_api("product", &selected, &policy(&[]), &api_trait)
        .expect("bounded Aha tools");
    let tools = toolset
        .tools(context())
        .await
        .expect("bounded Aha tool list");
    let tool = |name: &str| {
        tools
            .iter()
            .find(|tool| tool.name() == name)
            .expect("Aha tool")
    };

    tool("manage_record")
        .execute(
            context(),
            json!({
                "action":"create",
                "record_type":"feature",
                "parent_id":"REL-1",
                "properties":[]
            }),
        )
        .await
        .expect("legacy manage accepts the SDK empty-list default");
    assert_eq!(api.calls.lock().expect("calls").len(), 1);

    assert!(
        tool("create_record")
            .execute(
                context(),
                json!({
                    "record_type":"feature",
                    "parent_id":"REL-1",
                    "properties":[]
                }),
            )
            .await
            .is_err(),
        "typed create requires an object"
    );
    assert_eq!(api.calls.lock().expect("calls").len(), 1);

    for body in ["a".repeat(65_537), "🙂".repeat(16_385), "\0".repeat(45_000)] {
        assert!(
            tool("add_comment")
                .execute(
                    context(),
                    json!({"resource_type":"feature","resource_id":"1","body":body}),
                )
                .await
                .is_err(),
            "string bytes or serialized escaping must fit the shared envelope"
        );
    }
    assert_eq!(api.calls.lock().expect("calls").len(), 1);

    let client_properties = (0..4)
        .map(|index| (format!("field_{index}"), json!("x".repeat(61_500))))
        .collect::<Map<_, _>>();
    let transport = Arc::new(FixtureTransport::new(Vec::new()));
    let client = AhaClient::with_transport(&config(), transport.clone(), empty_artifacts());
    let error = client
        .execute(AhaOperation::CreateRecord {
            record_type: "feature",
            parent_id: "REL-1",
            properties: &client_properties,
        })
        .await
        .expect_err("properties exceed the 240 KiB family bound");
    assert_eq!(error.code(), AhaClientErrorCode::ResourceExhausted);
    assert!(transport.snapshots().is_empty(), "no provider dispatch");

    let properties = (0..4)
        .map(|index| (format!("field_{index}"), json!("x".repeat(65_520))))
        .collect::<Map<_, _>>();
    assert!(
        tool("create_record")
            .execute(
                context(),
                json!({
                    "record_type":"feature",
                    "parent_id":"REL-1",
                    "properties":properties
                }),
            )
            .await
            .is_err(),
        "whole argument JSON stays below 256 KiB"
    );
    assert_eq!(api.calls.lock().expect("calls").len(), 1);
}

#[derive(Clone, Debug)]
struct RequestSnapshot {
    method: Method,
    path: String,
    query: Option<String>,
    authorization: String,
    authorization_sensitive: bool,
    content_type: Option<String>,
    content_length: Option<u64>,
    body: Option<Value>,
    effect: bool,
}

struct FixtureTransport {
    responses: Mutex<VecDeque<Result<AhaHttpResponse, AhaClientError>>>,
    requests: Mutex<Vec<RequestSnapshot>>,
}

impl FixtureTransport {
    fn new(responses: Vec<AhaHttpResponse>) -> Self {
        Self {
            responses: Mutex::new(responses.into_iter().map(Ok).collect()),
            requests: Mutex::new(Vec::new()),
        }
    }

    fn snapshots(&self) -> Vec<RequestSnapshot> {
        self.requests.lock().expect("Aha requests").clone()
    }
}

#[async_trait]
impl AhaTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<AhaHttpResponse, AhaClientError> {
        let body = request
            .body()
            .and_then(reqwest::Body::as_bytes)
            .map(|bytes| serde_json::from_slice(bytes).expect("JSON request body"));
        let snapshot = RequestSnapshot {
            method: request.method().clone(),
            path: request.url().path().to_owned(),
            query: request.url().query().map(ToOwned::to_owned),
            authorization: request
                .headers()
                .get(AUTHORIZATION)
                .and_then(|value| value.to_str().ok())
                .unwrap_or_default()
                .to_owned(),
            authorization_sensitive: request
                .headers()
                .get(AUTHORIZATION)
                .is_some_and(reqwest::header::HeaderValue::is_sensitive),
            content_type: request
                .headers()
                .get(CONTENT_TYPE)
                .and_then(|value| value.to_str().ok())
                .map(ToOwned::to_owned),
            content_length: request
                .headers()
                .get(CONTENT_LENGTH)
                .and_then(|value| value.to_str().ok())
                .and_then(|value| value.parse().ok()),
            body,
            effect,
        };
        self.requests.lock().expect("Aha requests").push(snapshot);
        self.responses
            .lock()
            .expect("Aha responses")
            .pop_front()
            .unwrap_or_else(|| {
                Err(AhaClientError::fixture(
                    AhaClientErrorCode::DependencyUnavailable,
                    false,
                ))
            })
    }
}

fn client_with(responses: Vec<AhaHttpResponse>) -> (AhaClient, Arc<FixtureTransport>) {
    let transport = Arc::new(FixtureTransport::new(responses));
    let client = AhaClient::with_transport(&config(), transport.clone(), empty_artifacts());
    (client, transport)
}

fn ok(body: Value) -> AhaHttpResponse {
    AhaHttpResponse::fixture(StatusCode::OK, Some(body))
}

fn assert_common_request(request: &RequestSnapshot, method: &Method, path: &str, effect: bool) {
    assert_eq!(&request.method, method);
    assert_eq!(request.path, path);
    assert_eq!(request.authorization, format!("Bearer {API_KEY}"));
    assert!(request.authorization_sensitive);
    assert_eq!(request.effect, effect);
}

#[tokio::test]
async fn rest_reads_encode_routes_queries_project_page_results_and_require_exact_keys() {
    let fields = vec![Box::<str>::from("name")];
    let (client, transport) = client_with(vec![
        ok(json!({
            "unrelated": [{"name":"wrong"}],
            "features": [{"id":"1","name":"Roadmap"}],
            "pagination":{"current_page":1,"total_pages":1}
        })),
        ok(json!({"feature":{"id":"7","name":"Encoded"}})),
    ]);
    let result = client
        .execute(AhaOperation::ListFeatures {
            product_id: None,
            release_id: Some("  REL/1  "),
            query: Some("  urgent  "),
            updated_since: Some("   "),
            page: PAGE,
            read: read(&fields, OutputFormat::Csv),
        })
        .await
        .expect("features list");
    assert_eq!(result, json!("name\nRoadmap\n"));

    let feature = client
        .execute(AhaOperation::GetFeature {
            reference: "DEV/7",
            read: read(&[], OutputFormat::Json),
        })
        .await
        .expect("feature read");
    assert_eq!(feature, json!({"id":"7","name":"Encoded"}));

    let requests = transport.snapshots();
    assert_common_request(
        &requests[0],
        &Method::GET,
        "/api/v1/releases/REL%2F1/features",
        false,
    );
    assert_eq!(
        requests[0].query.as_deref(),
        Some("page=1&per_page=25&q=urgent")
    );
    assert!(requests[0].body.is_none());
    assert_common_request(
        &requests[1],
        &Method::GET,
        "/api/v1/features/DEV%2F7",
        false,
    );

    let (missing, missing_transport) = client_with(vec![ok(json!({
        "unrelated": [],
        "pagination":{"current_page":1,"total_pages":1}
    }))]);
    let error = missing
        .execute(AhaOperation::ListFeatures {
            product_id: None,
            release_id: None,
            query: None,
            updated_since: None,
            page: PAGE,
            read: read(&[], OutputFormat::Json),
        })
        .await
        .expect_err("missing exact collection key");
    assert_eq!(error.code(), AhaClientErrorCode::InvalidResponse);
    assert_eq!(missing_transport.snapshots().len(), 1);
}

#[tokio::test]
async fn pagination_is_finite_and_dense_tabular_work_is_bounded() {
    let responses = (1..=10)
        .map(|page| {
            ok(json!({
                "features": [{"id":page}],
                "pagination":{"current_page":page,"total_pages":11}
            }))
        })
        .collect();
    let (client, transport) = client_with(responses);
    let error = client
        .execute(AhaOperation::ListFeatures {
            product_id: None,
            release_id: None,
            query: None,
            updated_since: None,
            page: PageOptions {
                per_page: 1,
                max_records: 100,
            },
            read: read(&[], OutputFormat::Json),
        })
        .await
        .expect_err("ten-page traversal bound");
    assert_eq!(error.code(), AhaClientErrorCode::ResourceExhausted);
    assert_eq!(transport.snapshots().len(), 10);

    let dense = (0..129)
        .map(|index| (format!("field_{index}"), json!(index)))
        .collect::<Map<_, _>>();
    let (dense_client, _) = client_with(vec![ok(json!({
        "features":[Value::Object(dense)],
        "pagination":{"current_page":1,"total_pages":1}
    }))]);
    let dense_error = dense_client
        .execute(AhaOperation::ListFeatures {
            product_id: None,
            release_id: None,
            query: None,
            updated_since: None,
            page: PAGE,
            read: read(&[], OutputFormat::Markdown),
        })
        .await
        .expect_err("column work budget");
    assert_eq!(dense_error.code(), AhaClientErrorCode::ResourceExhausted);
}

#[tokio::test]
async fn graphql_nulls_are_source_compatible_and_page_read_honors_projection_and_format() {
    let fields = vec![Box::<str>::from("name")];
    let (client, transport) = client_with(vec![
        ok(json!({"data":{"page":null}})),
        ok(json!({"data":{"feature":null}})),
        ok(json!({"data":{"requirement":null}})),
        ok(json!({"data":{"searchDocuments":{"nodes":null}}})),
        ok(json!({"data":{"page":{"id":"9","name":"Planning","extra":true}}})),
    ]);
    assert_eq!(
        client
            .execute(AhaOperation::GetPage {
                reference: "ABC-N-1",
                include_parent: false,
            })
            .await
            .expect("null page"),
        json!({})
    );
    assert_eq!(
        client
            .execute(AhaOperation::GetFeatureGql { reference: "ABC-1" })
            .await
            .expect("null feature"),
        json!({})
    );
    assert_eq!(
        client
            .execute(AhaOperation::GetRequirementGql {
                reference: "ABC-1-2"
            })
            .await
            .expect("null requirement"),
        json!({})
    );
    assert_eq!(
        client
            .execute(AhaOperation::SearchDocuments {
                query: "roadmap",
                searchable_type: None,
            })
            .await
            .expect("null search nodes"),
        json!([])
    );
    assert_eq!(
        client
            .execute(AhaOperation::ReadRecords {
                record_type: "PAGE",
                reference: "ABC-N-2",
                read: read(&fields, OutputFormat::Csv),
            })
            .await
            .expect("page projection"),
        json!("name\nPlanning\n")
    );

    for request in transport.snapshots() {
        assert_common_request(&request, &Method::POST, "/api/v2/graphql", false);
        assert_eq!(request.content_type.as_deref(), Some("application/json"));
        assert!(
            request
                .body
                .as_ref()
                .is_some_and(|body| body.get("query").is_some())
        );
    }
}

#[tokio::test]
async fn read_status_and_result_bounds_have_typed_retry_semantics() {
    for (status, code, retryable) in [
        (
            StatusCode::REQUEST_TIMEOUT,
            AhaClientErrorCode::Timeout,
            true,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            AhaClientErrorCode::RateLimited,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            AhaClientErrorCode::DependencyUnavailable,
            true,
        ),
        (
            StatusCode::UNAUTHORIZED,
            AhaClientErrorCode::Authentication,
            false,
        ),
        (
            StatusCode::FORBIDDEN,
            AhaClientErrorCode::Authorization,
            false,
        ),
        (StatusCode::NOT_FOUND, AhaClientErrorCode::NotFound, false),
    ] {
        let (client, transport) = client_with(vec![AhaHttpResponse::fixture(status, None)]);
        let error = client
            .execute(AhaOperation::GetFeature {
                reference: "ABC-1",
                read: read(&[], OutputFormat::Json),
            })
            .await
            .expect_err("read status failure");
        assert_eq!(error.code(), code);
        assert_eq!(error.retryable(), retryable);
        assert_eq!(transport.snapshots().len(), 1, "no automatic retry");
    }

    let (oversized, _) = client_with(vec![ok(json!({
        "feature":{"body":"x".repeat(512 * 1024)}
    }))]);
    let error = oversized
        .execute(AhaOperation::GetFeature {
            reference: "ABC-1",
            read: read(&[], OutputFormat::Json),
        })
        .await
        .expect_err("bounded read result");
    assert_eq!(error.code(), AhaClientErrorCode::ResourceExhausted);
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn remote_effect_routes_bodies_statuses_and_lowercase_normalization_are_exact() {
    let properties = json!({"name":"New name"});
    let properties = properties.as_object().expect("properties fixture");
    let (client, transport) = client_with(vec![
        AhaHttpResponse::fixture(StatusCode::CREATED, Some(json!({"comment":{"id":"1"}}))),
        AhaHttpResponse::fixture(StatusCode::CREATED, Some(json!({"page":{"id":"2"}}))),
        AhaHttpResponse::fixture(StatusCode::CREATED, Some(json!({"feature":{"id":"3"}}))),
        AhaHttpResponse::fixture(StatusCode::OK, Some(json!({"release":{"id":"4"}}))),
        AhaHttpResponse::fixture(StatusCode::NO_CONTENT, None),
        AhaHttpResponse::fixture(StatusCode::NO_CONTENT, None),
        AhaHttpResponse::fixture(StatusCode::CREATED, Some(json!({"release":{"id":"8"}}))),
    ]);

    client
        .execute(AhaOperation::AddComment {
            resource_type: "FEATURE",
            resource_id: "ABC/1",
            body: "First line\nSecond line",
        })
        .await
        .expect("comment effect");
    client
        .execute(AhaOperation::ManageRecord {
            action: "CREATE",
            record_type: "PAGE",
            record_id: None,
            parent_id: Some("PROD/1"),
            properties,
        })
        .await
        .expect("legacy create effect");
    client
        .execute(AhaOperation::CreateRecord {
            record_type: "Feature",
            parent_id: "REL/2",
            properties,
        })
        .await
        .expect("create effect");
    client
        .execute(AhaOperation::UpdateRecord {
            record_type: "RELEASE",
            record_id: "REL/2",
            parent_id: Some("PROD/1"),
            properties,
        })
        .await
        .expect("update effect");
    client
        .execute(AhaOperation::DeleteRecord {
            record_type: "INITIATIVE",
            record_id: "INIT/2",
            parent_id: Some("PROD/1"),
        })
        .await
        .expect("delete effect");
    let link = client
        .execute(AhaOperation::CreateRecordLink {
            from_record_type: "FEATURE",
            from_id: "101",
            to_record_type: "IDEA",
            to_id: "202",
            link_type: 20,
        })
        .await
        .expect("record link effect");
    assert_eq!(link["created"], Value::Bool(true));
    client
        .execute(AhaOperation::CopyRecord {
            record_type: "RELEASE",
            record_id: "REL/2",
        })
        .await
        .expect("copy effect");

    let requests = transport.snapshots();
    let expected = [
        (
            Method::POST,
            "/api/v1/features/ABC%2F1/comments",
            StatusCode::CREATED,
        ),
        (
            Method::POST,
            "/api/v1/products/PROD%2F1/pages",
            StatusCode::CREATED,
        ),
        (
            Method::POST,
            "/api/v1/releases/REL%2F2/features",
            StatusCode::CREATED,
        ),
        (
            Method::PUT,
            "/api/v1/products/PROD%2F1/releases/REL%2F2",
            StatusCode::OK,
        ),
        (
            Method::DELETE,
            "/api/v1/products/PROD%2F1/initiatives/INIT%2F2",
            StatusCode::NO_CONTENT,
        ),
        (
            Method::POST,
            "/api/v1/features/101/record_links",
            StatusCode::NO_CONTENT,
        ),
        (
            Method::POST,
            "/api/v1/releases/REL%2F2/duplicate",
            StatusCode::CREATED,
        ),
    ];
    assert_eq!(requests.len(), expected.len());
    for (request, (method, path, _)) in requests.iter().zip(expected) {
        assert_common_request(request, &method, path, true);
    }
    assert_eq!(
        requests[0].body,
        Some(json!({"comment":{"body":"First line\nSecond line"}}))
    );
    assert_eq!(requests[1].body, Some(json!({"page":properties})));
    assert_eq!(requests[2].body, Some(json!({"feature":properties})));
    assert_eq!(requests[3].body, Some(json!({"release":properties})));
    assert!(requests[4].body.is_none());
    assert_eq!(
        requests[5].body,
        Some(json!({"record_link":{"record_type":"idea","record_id":202,"link_type":20}}))
    );
    assert!(requests[6].body.is_none());
}

#[tokio::test]
async fn effects_are_one_attempt_and_all_transient_or_unexpected_success_statuses_are_unknown() {
    for status in [
        StatusCode::REQUEST_TIMEOUT,
        StatusCode::TOO_MANY_REQUESTS,
        StatusCode::SERVICE_UNAVAILABLE,
        StatusCode::ACCEPTED,
        StatusCode::PARTIAL_CONTENT,
    ] {
        let (client, transport) = client_with(vec![AhaHttpResponse::fixture(status, None)]);
        let error = client
            .execute(AhaOperation::AddComment {
                resource_type: "feature",
                resource_id: "ABC-1",
                body: "Comment",
            })
            .await
            .expect_err("effect outcome is ambiguous");
        assert_eq!(error.code(), AhaClientErrorCode::UnknownOutcome);
        assert!(!error.retryable());
        assert_eq!(transport.snapshots().len(), 1, "effect is never retried");
    }

    for response in [
        AhaHttpResponse::fixture(StatusCode::CREATED, Some(json!([]))),
        AhaHttpResponse::non_json_fixture(StatusCode::CREATED),
    ] {
        let (client, _) = client_with(vec![response]);
        let error = client
            .execute(AhaOperation::AddComment {
                resource_type: "feature",
                resource_id: "ABC-1",
                body: "Comment",
            })
            .await
            .expect_err("post-accept shape failure");
        assert_eq!(error.code(), AhaClientErrorCode::UnknownOutcome);
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn every_effect_projection_failure_after_acceptance_is_unknown_outcome() {
    let huge = "x".repeat(512 * 1024);
    let properties = Map::new();

    let (comment, _) = client_with(vec![AhaHttpResponse::fixture(
        StatusCode::CREATED,
        Some(json!({"comment":{"body":huge}})),
    )]);
    assert_eq!(
        comment
            .execute(AhaOperation::AddComment {
                resource_type: "feature",
                resource_id: "1",
                body: "ok",
            })
            .await
            .expect_err("comment projection")
            .code(),
        AhaClientErrorCode::UnknownOutcome
    );

    for (action, status) in [
        ("create", StatusCode::CREATED),
        ("update", StatusCode::OK),
        ("delete", StatusCode::NO_CONTENT),
    ] {
        let (client, _) = client_with(vec![AhaHttpResponse::fixture(
            status,
            Some(json!({"feature":{"body":huge}})),
        )]);
        let (record_id, parent_id) = if action == "create" {
            (None, Some("REL-1"))
        } else {
            (Some("ABC-1"), None)
        };
        let error = client
            .execute(AhaOperation::ManageRecord {
                action,
                record_type: "feature",
                record_id,
                parent_id,
                properties: &properties,
            })
            .await
            .expect_err("manage projection");
        assert_eq!(error.code(), AhaClientErrorCode::UnknownOutcome);
    }

    let (link, _) = client_with(vec![AhaHttpResponse::fixture(
        StatusCode::CREATED,
        Some(json!({"record_link":{"body":huge}})),
    )]);
    assert_eq!(
        link.execute(AhaOperation::CreateRecordLink {
            from_record_type: "feature",
            from_id: "1",
            to_record_type: "idea",
            to_id: "2",
            link_type: 10,
        })
        .await
        .expect_err("link projection")
        .code(),
        AhaClientErrorCode::UnknownOutcome
    );

    let (copy, _) = client_with(vec![AhaHttpResponse::fixture(
        StatusCode::CREATED,
        Some(json!({"release":{"body":huge}})),
    )]);
    assert_eq!(
        copy.execute(AhaOperation::CopyRecord {
            record_type: "release",
            record_id: "REL-1",
        })
        .await
        .expect_err("copy projection")
        .code(),
        AhaClientErrorCode::UnknownOutcome
    );
}

#[test]
fn artifact_claims_enforce_exact_path_decimal_limit_and_immutable_content() {
    assert!(
        AhaArtifactResolver::fixture(vec![
            AhaArtifactFixture::new("/bucket/file.bin", "file.bin", Bytes::from_static(b"x"))
                .declared_length(299_999_999),
        ])
        .is_ok(),
        "strictly below 300 MB is admitted without allocating the declared body"
    );
    let rejected = AhaArtifactResolver::fixture(vec![
        AhaArtifactFixture::new("/bucket/file.bin", "file.bin", Bytes::from_static(b"x"))
            .declared_length(300_000_000),
    ]);
    assert!(rejected.is_err(), "300 MB exactly is rejected");

    for path in [
        "bucket/file.bin",
        "/bucket/nested/file.bin",
        "/../file.bin",
        "/bucket/file.bin?query",
        "/bucket/../file.bin",
    ] {
        assert!(
            AhaArtifactResolver::fixture(vec![AhaArtifactFixture::new(
                path,
                "file.bin",
                Bytes::from_static(b"x"),
            )])
            .is_err(),
            "invalid artifact authority path: {path}"
        );
    }
}

#[tokio::test]
async fn attachment_verifies_digest_version_and_authority_before_one_streaming_effect() {
    for fixture in [
        AhaArtifactFixture::new(
            "/bucket/file.bin",
            "file.bin",
            Bytes::from_static(b"trusted"),
        )
        .expected_digest([0; 32]),
        AhaArtifactFixture::new(
            "/bucket/file.bin",
            "file.bin",
            Bytes::from_static(b"trusted"),
        )
        .returned_version("substituted"),
    ] {
        let transport = Arc::new(FixtureTransport::new(vec![AhaHttpResponse::fixture(
            StatusCode::CREATED,
            Some(json!({"attachment":{"id":"1"}})),
        )]));
        let artifacts =
            Arc::new(AhaArtifactResolver::fixture(vec![fixture]).expect("artifact fixture"));
        let client = AhaClient::with_transport(&config(), transport.clone(), artifacts);
        let error = client
            .execute(AhaOperation::AttachFile {
                resource_type: "TO-DO",
                resource_id: "77",
                filepath: "/bucket/file.bin",
                filename: None,
            })
            .await
            .expect_err("substituted artifact rejected");
        assert!(matches!(
            error.code(),
            AhaClientErrorCode::Authorization | AhaClientErrorCode::InvalidResponse
        ));
        assert!(
            transport.snapshots().is_empty(),
            "Aha sees no effect request"
        );
    }

    let transport = Arc::new(FixtureTransport::new(vec![
        AhaHttpResponse::non_json_fixture(StatusCode::NO_CONTENT),
    ]));
    let artifacts = Arc::new(
        AhaArtifactResolver::fixture(vec![AhaArtifactFixture::new(
            "/bucket/file.bin",
            "file.bin",
            Bytes::from_static(b"trusted"),
        )])
        .expect("artifact fixture"),
    );
    let client = AhaClient::with_transport(&config(), transport.clone(), artifacts);
    assert_eq!(
        client
            .execute(AhaOperation::AttachFile {
                resource_type: "To-Do",
                resource_id: "77",
                filepath: "/bucket/file.bin",
                filename: Some("sent.bin"),
            })
            .await
            .expect("empty non-JSON 204 receipt"),
        json!({})
    );
    let requests = transport.snapshots();
    assert_eq!(requests.len(), 1);
    assert_common_request(
        &requests[0],
        &Method::POST,
        "/api/v1/tasks/77/attachments",
        true,
    );
    assert!(
        requests[0]
            .content_type
            .as_deref()
            .is_some_and(|value| value.starts_with("multipart/form-data; boundary="))
    );
    assert!(requests[0].content_length.is_some_and(|length| length > 7));

    let unauthorized_transport = Arc::new(FixtureTransport::new(Vec::new()));
    let unauthorized =
        AhaClient::with_transport(&config(), unauthorized_transport.clone(), empty_artifacts());
    assert_eq!(
        unauthorized
            .execute(AhaOperation::AttachFile {
                resource_type: "to_do",
                resource_id: "77",
                filepath: "/other/file.bin",
                filename: None,
            })
            .await
            .expect_err("artifact authority required")
            .code(),
        AhaClientErrorCode::Authorization
    );
    assert!(unauthorized_transport.snapshots().is_empty());
}

#[tokio::test]
async fn description_attachment_route_and_post_accept_bound_are_effect_aware() {
    let fixture = || {
        AhaArtifactFixture::new(
            "/bucket/file.bin",
            "file.bin",
            Bytes::from_static(b"trusted"),
        )
    };
    let transport = Arc::new(FixtureTransport::new(vec![
        ok(json!({"feature":{"description":{"id":"note/9"}}})),
        AhaHttpResponse::fixture(
            StatusCode::CREATED,
            Some(json!({"attachment":{"id":"attachment-1"}})),
        ),
    ]));
    let artifacts = Arc::new(AhaArtifactResolver::fixture(vec![fixture()]).expect("artifact"));
    let client = AhaClient::with_transport(&config(), transport.clone(), artifacts);
    client
        .execute(AhaOperation::AttachFile {
            resource_type: "FEATURE",
            resource_id: "ABC/1",
            filepath: "/bucket/file.bin",
            filename: None,
        })
        .await
        .expect("description attachment");
    let requests = transport.snapshots();
    assert_eq!(requests.len(), 2);
    assert_common_request(
        &requests[0],
        &Method::GET,
        "/api/v1/features/ABC%2F1",
        false,
    );
    assert_common_request(
        &requests[1],
        &Method::POST,
        "/api/v1/notes/note%2F9/attachments",
        true,
    );

    let huge_transport = Arc::new(FixtureTransport::new(vec![AhaHttpResponse::fixture(
        StatusCode::CREATED,
        Some(json!({"attachment":{"body":"x".repeat(512 * 1024)}})),
    )]));
    let huge_artifacts = Arc::new(AhaArtifactResolver::fixture(vec![fixture()]).expect("artifact"));
    let huge_client = AhaClient::with_transport(&config(), huge_transport.clone(), huge_artifacts);
    let error = huge_client
        .execute(AhaOperation::AttachFile {
            resource_type: "to_do",
            resource_id: "1",
            filepath: "/bucket/file.bin",
            filename: None,
        })
        .await
        .expect_err("accepted attachment projection bound");
    assert_eq!(error.code(), AhaClientErrorCode::UnknownOutcome);
    assert_eq!(huge_transport.snapshots().len(), 1);
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn AhaApi> = Arc::new(FixtureApi::default());
    let toolset =
        test_build_with_api("gate", &[], &policy(&[]), &api).expect("complete aha toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("aha tools");
    super::sdk_conformance::assert_sdk_conformance("aha", &tools);
}
