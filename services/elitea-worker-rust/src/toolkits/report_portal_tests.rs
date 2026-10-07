use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Tool, Toolset};
use async_trait::async_trait;
use reqwest::header::{ACCEPT, AUTHORIZATION};
use reqwest::{Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::report_portal::client::{
    ReportFormat, ReportPortalApi, ReportPortalClient, ReportPortalClientError,
    ReportPortalClientErrorCode, ReportPortalHttpResponse, ReportPortalRequestKind,
    ReportPortalTransport, test_http_status,
};
use super::families::report_portal::config::{
    ReportPortalConfigErrorCode, ReportPortalToolkitConfig,
};
use super::families::report_portal::tools::{
    ReportPortalToolsetErrorCode, build_report_portal_toolset, test_build_with_api,
};
use super::policy::ToolAdmissionPolicy;

fn settings_with(
    selected_tools: Option<Value>,
    endpoint: &str,
    project: &str,
    api_key: &str,
) -> Map<String, Value> {
    let mut settings = json!({
        "report_portal_configuration": {
            "configuration_type": "report_portal",
            "endpoint": endpoint,
            "project": project,
            "api_key": api_key
        }
    })
    .as_object()
    .expect("ReportPortal settings fixture")
    .clone();
    if let Some(selected_tools) = selected_tools {
        settings.insert("selected_tools".to_owned(), selected_tools);
    }
    settings
}

fn settings(selected_tools: &[&str]) -> Map<String, Value> {
    settings_with(
        Some(json!(selected_tools)),
        "https://reportportal.example.test/",
        "quality team/α",
        "fixture-report-portal-key",
    )
}

fn config(selected_tools: &[&str]) -> ReportPortalToolkitConfig {
    ReportPortalToolkitConfig::parse(&settings(selected_tools))
        .expect("valid ReportPortal configuration")
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("ReportPortal policy fixture"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("report-portal-tool-test")
            .with_session_id("session-1")
            .with_function_call_id("call-1"),
    )
}

#[test]
fn nested_configuration_is_bounded_deduplicated_and_secret_safe() {
    let parsed = config(&[
        "get_launch_details",
        "get_launch_details",
        "get_dashboard_data",
    ]);
    assert_eq!(
        parsed.selected_tools(),
        [
            Box::<str>::from("get_launch_details"),
            Box::<str>::from("get_dashboard_data")
        ]
    );

    for all_selection in [None, Some(Value::Null), Some(json!([]))] {
        let parsed = ReportPortalToolkitConfig::parse(&settings_with(
            all_selection,
            "https://reportportal.example.test",
            "quality",
            "fixture-secret",
        ))
        .expect("empty or null selection means all tools");
        assert!(parsed.selected_tools().is_empty());
    }

    for invalid in [
        json!({}),
        json!({
            "report_portal_configuration": {
                "endpoint":"http://reportportal.test", "project":"quality", "api_key":"secret"
            }
        }),
        json!({
            "report_portal_configuration": {
                "endpoint":"https://user@reportportal.test", "project":"quality", "api_key":"secret"
            }
        }),
        json!({
            "report_portal_configuration": {
                "endpoint":"https://reportportal.test/base", "project":"quality", "api_key":"secret"
            }
        }),
        json!({
            "report_portal_configuration": {
                "endpoint":"https://reportportal.test?leak=1", "project":"quality", "api_key":"secret"
            }
        }),
        json!({
            "report_portal_configuration": {
                "endpoint":"https://reportportal.test", "project":"", "api_key":"secret"
            }
        }),
        json!({
            "report_portal_configuration": {
                "endpoint":"https://reportportal.test", "project":"quality", "api_key":"fixture\nsecret"
            }
        }),
    ] {
        let Err(error) = ReportPortalToolkitConfig::parse(
            invalid
                .as_object()
                .expect("invalid ReportPortal configuration object"),
        ) else {
            panic!("invalid configuration must fail");
        };
        assert_eq!(
            error.code(),
            ReportPortalConfigErrorCode::InvalidConfiguration
        );
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains("fixture"));
        assert!(!diagnostic.contains("reportportal.test"));
    }

    let oversized = settings_with(
        Some(json!([])),
        "https://reportportal.test",
        "quality",
        &"x".repeat(16 * 1_024 + 1),
    );
    let Err(error) = ReportPortalToolkitConfig::parse(&oversized) else {
        panic!("oversized secret must fail without rendering it");
    };
    assert_eq!(error.code(), ReportPortalConfigErrorCode::ResourceExhausted);
}

#[test]
fn exact_requests_encode_paths_queries_and_sensitive_bearer_header() {
    let client = ReportPortalClient::new(config(&[])).expect("ReportPortal client");
    let cases = [
        (
            ReportPortalRequestKind::RawExport {
                launch_id: "launch /一",
                format: ReportFormat::Pdf,
            },
            "/api/v1/quality%20team%2F%CE%B1/launch/launch%20%2F%E4%B8%80/report",
            vec![("view", "pdf")],
        ),
        (
            ReportPortalRequestKind::ReadableExport {
                launch_id: "launch-1",
            },
            "/api/v1/quality%20team%2F%CE%B1/launch/launch-1/report",
            vec![("view", "html")],
        ),
        (
            ReportPortalRequestKind::LaunchDetails {
                launch_id: "launch-1",
            },
            "/api/v1/quality%20team%2F%CE%B1/launch/launch-1",
            vec![],
        ),
        (
            ReportPortalRequestKind::AllLaunches { page_number: 0 },
            "/api/v1/quality%20team%2F%CE%B1/launch",
            vec![("page.page", "0")],
        ),
        (
            ReportPortalRequestKind::TestItem { item_id: "item/1" },
            "/api/v1/quality%20team%2F%CE%B1/item/item%2F1",
            vec![],
        ),
        (
            ReportPortalRequestKind::TestItemsForLaunch {
                launch_id: "launch /一",
                page_number: 2,
            },
            "/api/v1/quality%20team%2F%CE%B1/item",
            vec![("filter.eq.launchId", "launch /一"), ("page.page", "2")],
        ),
        (
            ReportPortalRequestKind::LogsForTestItem {
                item_id: "item/1",
                page_number: 3,
            },
            "/api/v1/quality%20team%2F%CE%B1/log",
            vec![("filter.eq.item", "item/1"), ("page.page", "3")],
        ),
        (
            ReportPortalRequestKind::UserInformation {
                username: "qa/lead",
            },
            "/api/users/qa%2Flead",
            vec![],
        ),
        (
            ReportPortalRequestKind::Dashboard {
                dashboard_id: "dash/1",
            },
            "/api/v1/quality%20team%2F%CE%B1/dashboard/dash%2F1",
            vec![],
        ),
    ];
    for (kind, path, query) in cases {
        let request = client.test_request(kind).expect("bounded exact request");
        assert_eq!(request.method(), reqwest::Method::GET);
        assert_eq!(request.url().scheme(), "https");
        assert_eq!(request.url().host_str(), Some("reportportal.example.test"));
        assert_eq!(request.url().path(), path);
        assert_eq!(
            request
                .url()
                .query_pairs()
                .map(|(key, value)| (key.into_owned(), value.into_owned()))
                .collect::<Vec<_>>(),
            query
                .into_iter()
                .map(|(key, value)| (key.to_owned(), value.to_owned()))
                .collect::<Vec<_>>()
        );
        assert_eq!(
            request.headers().get(ACCEPT).expect("Accept header"),
            "application/json"
        );
        let authorization = request
            .headers()
            .get(AUTHORIZATION)
            .expect("Bearer authorization");
        assert!(authorization.is_sensitive());
        assert_eq!(authorization, "Bearer fixture-report-portal-key");
        assert_eq!(request.timeout(), Some(&Duration::from_secs(20)));
    }
}

#[test]
fn invocation_scoped_clients_keep_authority_isolated() {
    let first = ReportPortalClient::new(
        ReportPortalToolkitConfig::parse(&settings_with(
            Some(json!([])),
            "https://first.example.test",
            "first project",
            "first-secret",
        ))
        .expect("first config"),
    )
    .expect("first client");
    let second = ReportPortalClient::new(
        ReportPortalToolkitConfig::parse(&settings_with(
            Some(json!([])),
            "https://second.example.test",
            "second project",
            "second-secret",
        ))
        .expect("second config"),
    )
    .expect("second client");
    let first = first
        .test_request(ReportPortalRequestKind::LaunchDetails { launch_id: "1" })
        .expect("first request");
    let second = second
        .test_request(ReportPortalRequestKind::LaunchDetails { launch_id: "1" })
        .expect("second request");
    assert_ne!(first.url().host_str(), second.url().host_str());
    assert_ne!(first.url().path(), second.url().path());
    assert_ne!(
        first.headers().get(AUTHORIZATION),
        second.headers().get(AUTHORIZATION)
    );
}

struct FixtureTransport {
    requests: Mutex<Vec<Request>>,
    responses: Mutex<VecDeque<Result<ReportPortalHttpResponse, ReportPortalClientError>>>,
}

impl FixtureTransport {
    fn new(responses: Vec<ReportPortalHttpResponse>) -> Self {
        Self {
            requests: Mutex::new(Vec::new()),
            responses: Mutex::new(responses.into_iter().map(Ok).collect()),
        }
    }

    fn request_count(&self) -> usize {
        self.requests
            .lock()
            .expect("ReportPortal request fixture lock")
            .len()
    }
}

#[async_trait]
impl ReportPortalTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
    ) -> Result<ReportPortalHttpResponse, ReportPortalClientError> {
        self.requests
            .lock()
            .expect("ReportPortal request fixture lock")
            .push(request);
        self.responses
            .lock()
            .expect("ReportPortal response fixture lock")
            .pop_front()
            .expect("one ReportPortal response")
    }
}

fn response(
    content_type: &str,
    disposition: Option<&str>,
    body: impl Into<Vec<u8>>,
) -> ReportPortalHttpResponse {
    ReportPortalHttpResponse::fixture(StatusCode::OK, Some(content_type), disposition, body)
}

#[tokio::test]
async fn raw_html_and_pdf_are_explicit_bounded_one_request_results() {
    let html_transport = Arc::new(FixtureTransport::new(vec![response(
        "text/html; charset=utf-8",
        Some("attachment; filename=report.html"),
        b"<html><body>passed</body></html>".to_vec(),
    )]));
    let client = ReportPortalClient::fixture(
        config(&[]),
        html_transport.clone() as Arc<dyn ReportPortalTransport>,
    );
    assert_eq!(
        client
            .get_extended_launch_data_as_raw("launch-1", ReportFormat::Html)
            .await
            .expect("raw HTML"),
        json!({
            "format":"html",
            "content_type":"text/html",
            "encoding":"utf-8",
            "byte_length":32,
            "content":"<html><body>passed</body></html>"
        })
    );
    assert_eq!(html_transport.request_count(), 1);

    let pdf_transport = Arc::new(FixtureTransport::new(vec![response(
        "application/pdf",
        Some("ATTACHMENT; filename=report.pdf"),
        b"%PDF-1.7\nfixture".to_vec(),
    )]));
    let client = ReportPortalClient::fixture(
        config(&[]),
        pdf_transport.clone() as Arc<dyn ReportPortalTransport>,
    );
    let output = client
        .get_extended_launch_data_as_raw("launch-1", ReportFormat::Pdf)
        .await
        .expect("raw PDF");
    assert_eq!(output["format"], "pdf");
    assert_eq!(output["content_type"], "application/pdf");
    assert_eq!(output["encoding"], "base64");
    assert_eq!(output["byte_length"], 16);
    assert_eq!(output["content"], "JVBERi0xLjcKZml4dHVyZQ==");
    assert_eq!(pdf_transport.request_count(), 1);
}

#[tokio::test]
async fn export_disposition_type_utf8_pdf_magic_and_size_fail_closed() {
    let cases = [
        response("text/html", None, b"<p>missing</p>".to_vec()),
        response(
            "text/html",
            Some("inline; filename=report.html"),
            b"<p>inline</p>".to_vec(),
        ),
        response(
            "text/html",
            Some(&format!("attachment; {}", "x".repeat(4 * 1_024))),
            b"<p>oversized header</p>".to_vec(),
        ),
        response(
            "text/html",
            Some("attachment; filename=\"unterminated"),
            b"<p>malformed header</p>".to_vec(),
        ),
        response("application/pdf", Some("attachment"), b"%PDF-1.7".to_vec()),
        response("text/html", Some("attachment"), b"%PDF-1.7".to_vec()),
        response("text/html", Some("attachment"), vec![0xff, 0xfe]),
    ];
    for response in cases {
        let transport = Arc::new(FixtureTransport::new(vec![response]));
        let client = ReportPortalClient::fixture(
            config(&[]),
            transport.clone() as Arc<dyn ReportPortalTransport>,
        );
        let error = client
            .get_extended_launch_data_as_raw("launch-1", ReportFormat::Html)
            .await
            .expect_err("unusable HTML export must fail");
        assert_eq!(error.code(), ReportPortalClientErrorCode::InvalidResponse);
        assert!(!error.retryable());
        assert_eq!(transport.request_count(), 1);
    }

    let mut boundary_pdf = b"%PDF-1.7".to_vec();
    boundary_pdf.resize(383 * 1_024, b'x');
    let transport = Arc::new(FixtureTransport::new(vec![response(
        "application/pdf",
        Some("attachment"),
        boundary_pdf,
    )]));
    let client =
        ReportPortalClient::fixture(config(&[]), transport as Arc<dyn ReportPortalTransport>);
    let output = client
        .get_extended_launch_data_as_raw("launch-1", ReportFormat::Pdf)
        .await
        .expect("383 KiB PDF source fits the bounded JSON envelope");
    assert_eq!(output["byte_length"], 383 * 1_024);
    assert!(serde_json::to_vec(&output).expect("bounded PDF JSON").len() <= 512 * 1_024);

    for body in [b"not a pdf".to_vec(), {
        let mut body = b"%PDF-1.7".to_vec();
        body.resize(383 * 1_024 + 1, b'x');
        body
    }] {
        let transport = Arc::new(FixtureTransport::new(vec![response(
            "application/pdf",
            Some("attachment"),
            body,
        )]));
        let client =
            ReportPortalClient::fixture(config(&[]), transport as Arc<dyn ReportPortalTransport>);
        let error = client
            .get_extended_launch_data_as_raw("launch-1", ReportFormat::Pdf)
            .await
            .expect_err("invalid or oversized PDF must fail");
        assert!(matches!(
            error.code(),
            ReportPortalClientErrorCode::InvalidResponse
                | ReportPortalClientErrorCode::ResourceExhausted
        ));
    }
}

#[tokio::test]
async fn readable_export_is_deterministic_html_text_with_hidden_content_removed() {
    let html = br"
        <html><head><style>.secret { color:red }</style><script>alert('secret')</script></head>
        <body><!-- hidden --> <h1>Launch&nbsp;42</h1>
        <p>Passed &amp; stable &#x2014; owner: qa&#46;lead.</p>
        <div>Second<br>line &lt;ok&gt; &copy;</div></body></html>
    ";
    let transport = Arc::new(FixtureTransport::new(vec![response(
        "text/html",
        Some("attachment; filename=report.html"),
        html.to_vec(),
    )]));
    let client = ReportPortalClient::fixture(
        config(&[]),
        transport.clone() as Arc<dyn ReportPortalTransport>,
    );
    assert_eq!(
        client
            .get_extended_launch_data("launch-42")
            .await
            .expect("readable launch export"),
        Value::String("Launch 42 Passed & stable — owner: qa.lead. Second line <ok> ©".to_owned())
    );
    let request = transport
        .requests
        .lock()
        .expect("ReportPortal request fixture")
        .pop()
        .expect("one readable export request");
    assert_eq!(
        request.url().query_pairs().collect::<Vec<_>>(),
        vec![("view".into(), "html".into())]
    );
}

#[tokio::test]
async fn readable_export_removes_dense_hidden_tags_with_linear_work() {
    let mut html = String::with_capacity(1_900_000);
    html.push_str("<p>before</p>");
    for _ in 0..100_000 {
        html.push_str("<script>x</script>");
    }
    html.push_str("<p>after</p>");
    let transport = Arc::new(FixtureTransport::new(vec![response(
        "text/html",
        Some("attachment; filename=report.html"),
        html.into_bytes(),
    )]));
    let client = ReportPortalClient::fixture(
        config(&[]),
        transport.clone() as Arc<dyn ReportPortalTransport>,
    );

    let result = client
        .get_extended_launch_data("launch-dense")
        .await
        .expect("dense bounded HTML should project");

    assert_eq!(result, Value::String("before after".to_owned()));
    assert_eq!(transport.request_count(), 1);
}

#[tokio::test]
async fn json_reads_require_one_bounded_object_and_never_return_provider_text() {
    for bad in [
        ReportPortalHttpResponse::fixture(
            StatusCode::OK,
            Some("text/html"),
            None,
            b"<html>login</html>".to_vec(),
        ),
        ReportPortalHttpResponse::fixture(
            StatusCode::OK,
            Some("application/json"),
            None,
            br"[1,2,3]".to_vec(),
        ),
        ReportPortalHttpResponse::fixture(
            StatusCode::OK,
            Some("application/json"),
            None,
            b"provider malformed detail".to_vec(),
        ),
        ReportPortalHttpResponse::fixture(
            StatusCode::OK,
            Some("application/json"),
            None,
            serde_json::to_vec(&json!({"payload":"x".repeat(512 * 1_024)}))
                .expect("oversized JSON fixture"),
        ),
    ] {
        let transport = Arc::new(FixtureTransport::new(vec![bad]));
        let client =
            ReportPortalClient::fixture(config(&[]), transport as Arc<dyn ReportPortalTransport>);
        let error = client
            .get_launch_details("launch-1")
            .await
            .expect_err("invalid JSON shape must fail");
        assert!(matches!(
            error.code(),
            ReportPortalClientErrorCode::InvalidResponse
                | ReportPortalClientErrorCode::ResourceExhausted
        ));
        assert!(!format!("{error:?} {error}").contains("provider malformed detail"));
    }

    let transport = Arc::new(FixtureTransport::new(vec![
        ReportPortalHttpResponse::fixture(
            StatusCode::OK,
            Some("application/json; charset=utf-8"),
            None,
            br#"{"id":"launch-1","status":"PASSED"}"#.to_vec(),
        ),
    ]));
    let client = ReportPortalClient::fixture(
        config(&[]),
        transport.clone() as Arc<dyn ReportPortalTransport>,
    );
    assert_eq!(
        client
            .get_launch_details("launch-1")
            .await
            .expect("JSON object read"),
        json!({"id":"launch-1","status":"PASSED"})
    );
    assert_eq!(transport.request_count(), 1);
}

#[test]
fn status_errors_are_data_free_and_only_safe_transient_reads_retry() {
    for (status, code, retryable) in [
        (
            StatusCode::BAD_REQUEST,
            ReportPortalClientErrorCode::InvalidInput,
            false,
        ),
        (
            StatusCode::UNAUTHORIZED,
            ReportPortalClientErrorCode::Authentication,
            false,
        ),
        (
            StatusCode::FORBIDDEN,
            ReportPortalClientErrorCode::Authorization,
            false,
        ),
        (
            StatusCode::NOT_FOUND,
            ReportPortalClientErrorCode::NotFound,
            false,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            ReportPortalClientErrorCode::RateLimited,
            true,
        ),
        (
            StatusCode::GATEWAY_TIMEOUT,
            ReportPortalClientErrorCode::Timeout,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            ReportPortalClientErrorCode::DependencyUnavailable,
            true,
        ),
    ] {
        let error = test_http_status(status).expect_err("ReportPortal HTTP failure");
        assert_eq!(error.code(), code);
        assert_eq!(error.retryable(), retryable);
        let rendered = format!("{error:?} {error}");
        assert!(!rendered.contains("fixture-report-portal-key"));
        assert!(!rendered.contains("quality team"));
    }
}

#[derive(Default)]
struct FixtureApi {
    calls: Mutex<Vec<Value>>,
}

impl FixtureApi {
    fn record(&self, call: Value) -> Value {
        self.calls
            .lock()
            .expect("ReportPortal API fixture lock")
            .push(call.clone());
        call
    }
}

#[async_trait]
impl ReportPortalApi for FixtureApi {
    async fn get_extended_launch_data_as_raw(
        &self,
        launch_id: &str,
        format: ReportFormat,
    ) -> Result<Value, ReportPortalClientError> {
        Ok(self.record(json!({"tool":"raw","launch_id":launch_id,"format":format.as_str()})))
    }

    async fn get_extended_launch_data(
        &self,
        launch_id: &str,
    ) -> Result<Value, ReportPortalClientError> {
        Ok(self.record(json!({"tool":"readable","launch_id":launch_id})))
    }

    async fn get_launch_details(&self, launch_id: &str) -> Result<Value, ReportPortalClientError> {
        Ok(self.record(json!({"tool":"launch","launch_id":launch_id})))
    }

    async fn get_all_launches(&self, page_number: u64) -> Result<Value, ReportPortalClientError> {
        Ok(self.record(json!({"tool":"launches","page_number":page_number})))
    }

    async fn find_test_item_by_id(&self, item_id: &str) -> Result<Value, ReportPortalClientError> {
        Ok(self.record(json!({"tool":"item","item_id":item_id})))
    }

    async fn get_test_items_for_launch(
        &self,
        launch_id: &str,
        page_number: u64,
    ) -> Result<Value, ReportPortalClientError> {
        Ok(self.record(json!({"tool":"items","launch_id":launch_id,"page_number":page_number})))
    }

    async fn get_logs_for_test_items(
        &self,
        item_id: &str,
        page_number: u64,
    ) -> Result<Value, ReportPortalClientError> {
        Ok(self.record(json!({"tool":"logs","item_id":item_id,"page_number":page_number})))
    }

    async fn get_user_information(&self, username: &str) -> Result<Value, ReportPortalClientError> {
        Ok(self.record(json!({"tool":"user","username":username})))
    }

    async fn get_dashboard_data(
        &self,
        dashboard_id: &str,
    ) -> Result<Value, ReportPortalClientError> {
        Ok(self.record(json!({"tool":"dashboard","dashboard_id":dashboard_id})))
    }
}

fn assert_model_contract(tools: &[Arc<dyn Tool>]) {
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        [
            "get_extended_launch_data_as_raw",
            "get_extended_launch_data",
            "get_launch_details",
            "get_all_launches",
            "find_test_item_by_id",
            "get_test_items_for_launch",
            "get_logs_for_test_items",
            "get_user_information",
            "get_dashboard_data",
        ]
    );
    for tool in tools {
        assert!(tool.is_read_only());
        assert!(tool.is_concurrency_safe());
        assert!(tool.description().starts_with("Toolkit: quality-reports\n"));
        assert!(tool.description().len() <= 1_000);
        let schema = tool.parameters_schema().expect("ReportPortal schema");
        assert_eq!(schema["type"], "object");
        assert_eq!(schema["additionalProperties"], false);
        for property in schema["properties"]
            .as_object()
            .expect("ReportPortal schema properties")
            .values()
        {
            assert!(
                property["description"]
                    .as_str()
                    .is_some_and(|description| !description.trim().is_empty())
            );
        }
        for forbidden in [
            "reportportal.example.test",
            "quality team/α",
            "fixture-report-portal-key",
        ] {
            assert!(!tool.description().contains(forbidden));
            assert!(!schema.to_string().contains(forbidden));
        }
    }
    for (tool, cue) in tools.iter().zip([
        "base64",
        "script/style",
        "root-cause",
        "defaults to 0",
        "flaky-test",
        "failure concentration",
        "debugging evidence",
        "assignment",
        "KPI",
    ]) {
        assert!(tool.description().contains(cue));
    }

    let schemas = tools
        .iter()
        .map(|tool| tool.parameters_schema().expect("ReportPortal schema"))
        .collect::<Vec<_>>();
    assert_eq!(schemas[0]["required"], json!(["launch_id"]));
    assert_eq!(schemas[0]["properties"]["format"]["default"], "html");
    assert_eq!(
        schemas[0]["properties"]["format"]["enum"],
        json!(["html", "pdf", null])
    );
    assert!(schemas[1]["properties"].get("launch_id").is_some());
    assert!(schemas[1]["properties"].get("name").is_none());
    assert_eq!(schemas[1]["required"], json!(["launch_id"]));
    for index in [3, 5, 6] {
        assert_eq!(schemas[index]["properties"]["page_number"]["default"], 0);
        assert_eq!(schemas[index]["properties"]["page_number"]["minimum"], 0);
        assert_eq!(
            schemas[index]["properties"]["page_number"]["maximum"],
            10_000
        );
    }
}

#[tokio::test]
async fn catalog_order_groups_schemas_descriptions_defaults_and_policy_are_exact() {
    let api = Arc::new(FixtureApi::default());
    let api_trait: Arc<dyn ReportPortalApi> = api.clone();
    let toolset = test_build_with_api("quality-reports", &[], &policy(&[]), &api_trait)
        .expect("complete ReportPortal toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("ReportPortal tools");
    assert_model_contract(&tools);

    tools[0]
        .execute(context(), json!({"launch_id":"launch-1"}))
        .await
        .expect("default HTML raw export");
    for page_number in [json!(-1), json!(10_001)] {
        assert!(
            tools[3]
                .execute(context(), json!({"page_number":page_number}))
                .await
                .is_err()
        );
    }
    tools[3]
        .execute(context(), json!({}))
        .await
        .expect("default first launch page");
    tools[6]
        .execute(context(), json!({"item_id":"item-1","page_number":2}))
        .await
        .expect("explicit log page");
    assert_eq!(
        *api.calls.lock().expect("ReportPortal API calls"),
        vec![
            json!({"tool":"raw","launch_id":"launch-1","format":"html"}),
            json!({"tool":"launches","page_number":0}),
            json!({"tool":"logs","item_id":"item-1","page_number":2}),
        ]
    );

    let blocked_tools: &[&str] = &["get_user_information"];
    let blocked = test_build_with_api(
        "quality-reports",
        &[],
        &policy(&[("report_portal", blocked_tools)]),
        &api_trait,
    )
    .expect("policy-filtered ReportPortal toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert!(
        blocked
            .tools(readonly)
            .await
            .expect("policy-filtered tools")
            .iter()
            .all(|tool| tool.name() != "get_user_information")
    );
}

#[tokio::test]
async fn subset_keeps_source_order_and_invalid_selection_or_arguments_fail_closed() {
    let api = Arc::new(FixtureApi::default());
    let api_trait: Arc<dyn ReportPortalApi> = api.clone();
    let selected = vec![
        "get_dashboard_data".to_owned(),
        "get_launch_details".to_owned(),
    ];
    let toolset = test_build_with_api("quality-reports", &selected, &policy(&[]), &api_trait)
        .expect("selected ReportPortal toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset
        .tools(readonly)
        .await
        .expect("selected ReportPortal tools");
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        ["get_launch_details", "get_dashboard_data"]
    );
    for invalid in [
        json!({}),
        json!({"launch_id":""}),
        json!({"launch_id":"launch-1","extra":true}),
    ] {
        assert!(tools[0].execute(context(), invalid).await.is_err());
    }
    assert!(api.calls.lock().expect("ReportPortal API calls").is_empty());

    let unknown = ReportPortalToolkitConfig::parse(&settings(&["delete_everything"]))
        .expect("bounded unknown selected name parses");
    let Err(error) = build_report_portal_toolset("quality-reports", unknown, &policy(&[])) else {
        panic!("unknown ReportPortal selection must fail");
    };
    assert_eq!(
        error.code(),
        ReportPortalToolsetErrorCode::UnsupportedSelection
    );
}

#[test]
fn production_family_has_no_environment_global_or_debug_output_escape_hatches() {
    let sources = [
        include_str!("families/report_portal/mod.rs"),
        include_str!("families/report_portal/config.rs"),
        include_str!("families/report_portal/client.rs"),
        include_str!("families/report_portal/tools.rs"),
    ]
    .join("\n");
    for forbidden in [
        "std::env",
        "env!(",
        "option_env!(",
        "static mut",
        "OnceLock",
        "lazy_static",
        "println!(",
        "dbg!(",
        "unwrap(",
        "expect(",
    ] {
        assert!(
            !sources.contains(forbidden),
            "production family contains forbidden escape hatch: {forbidden}"
        );
    }
}

#[test]
fn fixture_error_constructor_remains_data_free() {
    let error = ReportPortalClientError::fixture(ReportPortalClientErrorCode::InvalidResponse);
    assert_eq!(error.code(), ReportPortalClientErrorCode::InvalidResponse);
    assert!(!format!("{error:?} {error}").contains("secret"));
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn ReportPortalApi> = Arc::new(FixtureApi::default());
    let toolset = test_build_with_api("gate", &[], &policy(&[]), &api)
        .expect("complete report_portal toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("report_portal tools");
    super::sdk_conformance::assert_sdk_conformance("report_portal", &tools);
}
