use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_core::{ErrorCategory, ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::header::{AUTHORIZATION, CONTENT_TYPE};
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

use super::attachment_test_support::{FakePlatform, PNG};
use super::families::attachment_host::AttachmentHost;
use super::families::office_host::OfficeFailure;
use super::families::testrail::client::{
    TestRailApi, TestRailClient, TestRailClientError, TestRailHttpResponse, TestRailTransport,
};
use super::families::testrail::config::{TestRailConfigErrorCode, TestRailToolkitConfig};
use super::families::testrail::tools::{
    TestRailToolsetErrorCode, build_testrail_toolset, test_build_with_api,
};
use super::policy::ToolAdmissionPolicy;

const ORIGIN: &str = "https://tr.example.test/testrail/index.php?/api/v2/";

fn settings(selected_tools: &[&str]) -> Map<String, Value> {
    json!({
        "testrail_configuration":{
            "url":"https://tr.example.test/testrail/",
            "email":"qa@example.test",
            "password":"testrail-secret"
        },
        "selected_tools":selected_tools
    })
    .as_object()
    .cloned()
    .expect("TestRail fixture settings are an object")
}

fn config() -> TestRailToolkitConfig {
    TestRailToolkitConfig::parse(&settings(&[])).expect("valid TestRail configuration")
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("TestRail policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("testrail-test").with_function_call_id("testrail-call"))
}

#[derive(Clone, Debug)]
struct Captured {
    method: Method,
    endpoint: String,
    body: Option<Value>,
    /// The raw body and content type of a multipart upload.
    upload: Option<(String, Vec<u8>)>,
}

type Handler = dyn Fn(&Method, &str) -> TestRailHttpResponse + Send + Sync;

struct FixtureTransport {
    requests: Mutex<Vec<Captured>>,
    handler: Box<Handler>,
}

impl FixtureTransport {
    fn new(
        handler: impl Fn(&Method, &str) -> TestRailHttpResponse + Send + Sync + 'static,
    ) -> Arc<Self> {
        Arc::new(Self {
            requests: Mutex::new(Vec::new()),
            handler: Box::new(handler),
        })
    }

    fn captured(&self) -> Vec<Captured> {
        self.requests.lock().expect("TestRail fixture lock").clone()
    }

    fn endpoints(&self) -> Vec<String> {
        self.captured()
            .into_iter()
            .map(|captured| captured.endpoint)
            .collect()
    }
}

#[async_trait]
impl TestRailTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        _effect: bool,
    ) -> Result<TestRailHttpResponse, TestRailClientError> {
        let url = request.url().as_str().to_owned();
        let endpoint = url
            .strip_prefix(ORIGIN)
            .expect("fixed TestRail API root")
            .to_owned();
        let authorization = request
            .headers()
            .get(AUTHORIZATION)
            .expect("TestRail authorization");
        // base64("qa@example.test:testrail-secret")
        assert_eq!(
            authorization.to_str().expect("ASCII"),
            "Basic cWFAZXhhbXBsZS50ZXN0OnRlc3RyYWlsLXNlY3JldA=="
        );
        assert!(authorization.is_sensitive());
        let content_type = request
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default()
            .to_owned();
        let raw = request.body().and_then(reqwest::Body::as_bytes);
        let (body, upload) = if endpoint.starts_with("add_attachment_to_case/") {
            assert!(
                content_type.starts_with("multipart/form-data; boundary="),
                "{content_type}"
            );
            (
                None,
                Some((content_type, raw.expect("upload body").to_vec())),
            )
        } else {
            assert_eq!(content_type, "application/json");
            (
                raw.map(|bytes| serde_json::from_slice(bytes).expect("JSON request body")),
                None,
            )
        };
        self.requests
            .lock()
            .expect("TestRail fixture lock")
            .push(Captured {
                method: request.method().clone(),
                endpoint: endpoint.clone(),
                body,
                upload,
            });
        Ok((self.handler)(request.method(), &endpoint))
    }
}

fn ok(body: Value) -> TestRailHttpResponse {
    TestRailHttpResponse::fixture(StatusCode::OK, Some(body))
}

/// Responses modelled on `TestRail` API v2 (6.7+ paginated envelopes).
fn provider(method: &Method, endpoint: &str) -> TestRailHttpResponse {
    let route = endpoint.split('&').next().unwrap_or_default();
    match (method.as_str(), route) {
        ("GET", "get_project/1") => ok(json!({"id":1,"name":"Multi","suite_mode":3})),
        ("GET", "get_project/2") => ok(json!({"id":2,"name":"Single","suite_mode":1})),
        ("GET", "get_suites/1") => ok(json!([
            {"id":10,"name":"Smoke","completed_on":null,"is_master":false,"url":"https://tr/s/10"},
            {"id":11,"name":"Broken"}
        ])),
        ("GET", "get_suites/2") => ok(json!([])),
        ("GET", "get_cases/1") if endpoint.contains("suite_id=11") => {
            TestRailHttpResponse::fixture(StatusCode::BAD_REQUEST, Some(json!({"error":"x"})))
        }
        ("GET", "get_cases/1" | "get_cases/2") => ok(json!({
            "offset":0,"limit":250,"size":2,"_links":{"next":null,"prev":null},
            "cases":[
                {"id":1,"title":"Login","section_id":5,"priority_id":2,"custom_steps":"a"},
                {"id":2,"title":"Logout","section_id":5}
            ]
        })),
        ("GET", "get_case/7") => ok(json!({"id":7,"is_deleted":0,"refs":null,"title":"It's done"})),
        ("GET", "get_sections/2") => ok(json!({
            "offset":0,"limit":250,"size":1,
            "sections":[{"id":5,"name":"Auth","depth":0,"suite_id":20,"extra":true}]
        })),
        ("GET", "get_run/3") => ok(json!({
            "id":3,"name":"Nightly","created_on":1_700_000_000,"completed_on":null,
            "passed_count":4,"custom_env":"prod","config":"ignored"
        })),
        ("GET", "get_runs/2") => ok(json!({"offset":0,"limit":250,"size":1,"runs":[
            {"id":3,"name":"Nightly","is_completed":true}
        ]})),
        ("GET", "get_results_for_run/3") if endpoint.contains("offset=0") => ok(json!({
            "offset":0,"limit":250,"size":250,
            "results":[{"id":100,"test_id":9,"status_id":5,"comment":"boom, again","created_on":1_700_000_000}]
        })),
        ("GET", "get_results_for_run/3") => ok(json!({
            "offset":250,"limit":250,"size":1,
            "results":[{"id":101,"test_id":9,"status_id":1,"comment":null,"created_on":1_700_000_060}]
        })),
        ("POST", "add_case/5") => ok(json!({"id":42,"title":"New","created_on":1_700_000_000})),
        ("GET", "get_case/42") => ok(json!({"id":42,"custom_preconds":"<p>Existing</p>"})),
        ("GET", "get_case/50" | "get_case/51") => ok(json!({"id":50,"custom_steps":null})),
        ("GET", "get_case/52") => TestRailHttpResponse::fixture(StatusCode::NOT_FOUND, None),
        ("POST", "add_attachment_to_case/52") => ok(json!({"attachment_id":890})),
        ("POST", "update_case/42") => ok(json!({"id":42,"updated_on":1_700_000_100})),
        ("POST", "delete_case/42" | "delete_section/5") => {
            TestRailHttpResponse::fixture(StatusCode::OK, None)
        }
        ("POST", "add_attachment_to_case/42") => ok(json!({"attachment_id":777})),
        ("POST", "add_attachment_to_case/43") => ok(json!({})),
        ("POST", "add_attachment_to_case/45") => {
            TestRailHttpResponse::fixture(StatusCode::NOT_FOUND, Some(json!({"error":"x"})))
        }
        ("POST", "add_attachment_to_case/46") => {
            TestRailHttpResponse::fixture(StatusCode::UNAUTHORIZED, None)
        }
        ("POST", "update_case/50") => {
            TestRailHttpResponse::fixture(StatusCode::BAD_REQUEST, Some(json!({"error":"field"})))
        }
        ("POST", "add_attachment_to_case/50") => ok(json!({"attachment_id":888})),
        ("POST", "add_attachment_to_case/51") => ok(json!({"attachment_id":889})),
        ("POST", "add_section/2") => ok(json!({"id":6,"name":"Payments"})),
        ("POST", "add_case/500" | "update_case/51" | "add_attachment_to_case/44") => {
            TestRailHttpResponse::fixture(StatusCode::INTERNAL_SERVER_ERROR, None)
        }
        other => panic!("unexpected TestRail request {other:?}"),
    }
}

async fn tools_with(transport: Arc<FixtureTransport>) -> Vec<Arc<dyn Tool>> {
    tools_with_host(transport, None).await
}

async fn tools_with_host(
    transport: Arc<FixtureTransport>,
    host: Option<&AttachmentHost>,
) -> Vec<Arc<dyn Tool>> {
    let client: Arc<dyn TestRailApi> =
        Arc::new(TestRailClient::with_transport(config(), transport));
    let toolset = test_build_with_api(
        "qa",
        "https://tr.example.test/testrail",
        &[],
        &policy(&[]),
        &client,
        host,
    )
    .expect("TestRail toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    toolset.tools(readonly).await.expect("TestRail tools")
}

fn tool<'a>(tools: &'a [Arc<dyn Tool>], name: &str) -> &'a Arc<dyn Tool> {
    tools
        .iter()
        .find(|tool| tool.name() == name)
        .unwrap_or_else(|| panic!("TestRail tool {name}"))
}

async fn call(tools: &[Arc<dyn Tool>], name: &str, arguments: Value) -> Value {
    tool(tools, name)
        .execute(context(), arguments)
        .await
        .unwrap_or_else(|error| panic!("{name}: {error}"))
}

#[test]
fn configuration_keeps_the_instance_path_and_redacts_the_credential() {
    let parsed = config();
    assert!(parsed.selected_tools().is_empty());
    for invalid in [
        json!({}),
        json!({"testrail_configuration":{"url":"https://tr.example.test","email":"a"}}),
        json!({"testrail_configuration":{"url":"http://tr.example.test","email":"a","password":"p"}}),
        json!({"testrail_configuration":{"url":"https://tr.example.test/?x=1","email":"a","password":"p"}}),
        json!({"testrail_configuration":{"url":"https://tr.example.test","email":"","password":"p"}}),
    ] {
        let Err(error) = TestRailToolkitConfig::parse(invalid.as_object().expect("object")) else {
            panic!("invalid TestRail configuration accepted: {invalid}");
        };
        assert_eq!(error.code(), TestRailConfigErrorCode::InvalidConfiguration);
        assert!(!format!("{error:?}").contains("testrail-secret"));
    }
}

#[tokio::test]
async fn case_reads_follow_suite_mode_and_render_sdk_text() {
    let transport = FixtureTransport::new(provider);
    let tools = tools_with(Arc::clone(&transport)).await;
    assert!(
        tools[0]
            .description()
            .ends_with("\nTestrail instance: https://tr.example.test/testrail")
    );

    let case = call(&tools, "get_case", json!({"testcase_id":"7"})).await;
    assert_eq!(
        case,
        json!(
            "Extracted test case:\n{'id': 7, 'is_deleted': 0, 'refs': None, 'title': \"It's done\"}"
        )
    );

    // Multiple-suite project without a suite: every suite, a failing one skipped.
    let cases = call(
        &tools,
        "get_cases",
        json!({"project_id":"1","keys":["id","title","bogus"]}),
    )
    .await;
    assert_eq!(
        cases,
        json!(
            "Extracted data:\n[{'id': 1, 'title': 'Login', 'bogus': 'N/A'}, {'id': 2, 'title': 'Logout', 'bogus': 'N/A'}]\n\nInvalid keys: ['bogus']"
        )
    );
    let filtered = call(
        &tools,
        "get_cases_by_filter",
        json!({
            "project_id":"2",
            "json_case_arguments":"{\"priority_id\":[1,2],\"filter\":\"log in\",\"suite_id\":null}",
            "output_format":"markdown",
            "keys":["id","priority_id"]
        }),
    )
    .await;
    assert_eq!(
        filtered,
        json!(
            "|   id |   priority_id |\n|-----:|--------------:|\n|    1 |             2 |\n|    2 |           nan |"
        )
    );
    let bad_format = call(
        &tools,
        "get_cases",
        json!({"project_id":"2","output_format":"xml"}),
    )
    .await;
    assert_eq!(
        bad_format,
        json!("Invalid format `xml`. Supported formats: 'json', 'csv', 'markdown'.")
    );
    let endpoints = transport.endpoints();
    assert_eq!(
        &endpoints[..6],
        [
            "get_case/7",
            "get_project/1",
            "get_suites/1",
            "get_cases/1&suite_id=10",
            "get_cases/1&suite_id=11",
            "get_project/2",
        ]
    );
    assert!(endpoints[6].starts_with("get_cases/2&"));
    assert!(endpoints[6].contains("filter=log+in"));
    assert!(endpoints[6].contains("priority_id=1%2C2"));
    assert!(!endpoints[6].contains("suite_id"));
}

#[tokio::test]
async fn structure_and_run_reads_project_allowlists_and_page_results() {
    let transport = FixtureTransport::new(provider);
    let tools = tools_with(Arc::clone(&transport)).await;
    assert_eq!(
        call(&tools, "get_suites", json!({"project_id":"2"})).await,
        json!("No test suites found for the specified project.")
    );
    assert_eq!(
        call(
            &tools,
            "get_suites",
            json!({"project_id":"1","output_format":"csv"})
        )
        .await,
        json!(
            "id,name,completed_on,url,is_master\n10,Smoke,,https://tr/s/10,False\n11,Broken,,,\n"
        )
    );
    assert_eq!(
        call(&tools, "get_sections", json!({"project_id":"2"})).await,
        json!("Extracted data:\n[{'id': 5, 'suite_id': 20, 'name': 'Auth', 'depth': 0}]")
    );
    assert_eq!(
        call(&tools, "get_run", json!({"run_id":"3"})).await,
        json!(
            "Extracted data:\n[{'id': 3, 'name': 'Nightly', 'completed_on': None, 'passed_count': 4, 'created_on': '2023-11-14T22:13:20+00:00', 'custom_env': 'prod'}]"
        )
    );
    assert_eq!(
        call(&tools, "get_run", json!({"run_id":"abc"})).await,
        json!("run_id must be numeric, got: 'abc'")
    );
    let runs = call(
        &tools,
        "get_runs",
        json!({"project_id":"2","run_filter":{"is_completed":"yes","limit":5}}),
    )
    .await;
    assert!(
        runs.as_str()
            .is_some_and(|text| text.contains("'is_completed': True"))
    );
    assert_eq!(
        call(
            &tools,
            "get_runs",
            json!({"project_id":"2","run_filter":"[1]"})
        )
        .await,
        json!("run_filter must be a JSON object of filters, got list.")
    );
    let results = call(
        &tools,
        "get_results_for_run",
        json!({"run_id":"3","result_filter":{"status_id":[5,1],"limit":3},"output_format":"csv"}),
    )
    .await;
    assert_eq!(
        results,
        json!(
            "id,test_id,status_id,comment,created_on\n100,9,5,\"boom, again\",2023-11-14T22:13:20+00:00\n101,9,1,,2023-11-14T22:14:20+00:00\n"
        )
    );
    let endpoints = transport.endpoints();
    assert!(endpoints.contains(&"get_sections/2&limit=250&offset=0".to_owned()));
    assert!(endpoints.contains(&"get_runs/2&is_completed=1&limit=5".to_owned()));
    assert!(
        endpoints.contains(&"get_results_for_run/3&offset=0&limit=250&status_id=5%2C1".to_owned())
    );
    assert!(
        endpoints
            .contains(&"get_results_for_run/3&offset=250&limit=250&status_id=5%2C1".to_owned())
    );
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One ordered corpus proves every effect wire body.
async fn effects_send_sdk_bodies_and_never_retry_an_unknown_outcome() {
    let transport = FixtureTransport::new(provider);
    let tools = tools_with(Arc::clone(&transport)).await;
    assert_eq!(
        call(
            &tools,
            "add_case",
            json!({"section_id":"5","title":"New","case_properties":"{\"priority_id\":2,\"custom_steps_separated\":[{\"content\":\"a\",\"expected\":\"b\"}]}"}),
        )
        .await,
        json!("New test case has been created: id - 42 at '1700000000'")
    );
    assert_eq!(
        call(
            &tools,
            "add_cases",
            json!({"add_test_cases_data":"[{\"section_id\":5,\"title\":\"New\"},{\"section_id\":\"5\",\"title\":\"New\",\"case_properties\":{\"type_id\":1}}]"}),
        )
        .await,
        json!([
            "New test case has been created: id - 42 at '1700000000'",
            "New test case has been created: id - 42 at '1700000000'"
        ])
    );
    assert_eq!(
        call(
            &tools,
            "update_case",
            json!({"case_id":"42","case_properties":"{\"title\":\"Renamed\"}"})
        )
        .await,
        json!("Test case #42 has been updated at '1700000100'")
    );
    assert_eq!(
        call(&tools, "delete_case", json!({"case_id":"42"})).await,
        json!("Test case #42 has been soft deleted (marked as deleted) successfully.")
    );
    assert_eq!(
        call(
            &tools,
            "add_section",
            json!({"project_id":"2","name":"Payments","section_properties":{"suite_id":20}}),
        )
        .await,
        json!("New section has been created: id - 6 - 'Payments'")
    );
    assert_eq!(
        call(
            &tools,
            "delete_section",
            json!({"section_id":"5","soft_delete":true})
        )
        .await,
        json!(
            "Section #5 has been previewed for deletion (soft dry run, nothing removed) successfully."
        )
    );
    let captured = transport.captured();
    assert!(
        captured
            .iter()
            .all(|request| request.method == Method::POST)
    );
    assert_eq!(
        captured[0].body,
        Some(
            json!({"title":"New","priority_id":2,"custom_steps_separated":[{"content":"a","expected":"b"}]})
        )
    );
    assert_eq!(captured[2].body, Some(json!({"title":"New","type_id":1})));
    assert_eq!(captured[3].body, Some(json!({"title":"Renamed"})));
    assert_eq!(captured[4].endpoint, "delete_case/42&soft=1");
    assert_eq!(captured[4].body, Some(json!({})));
    assert_eq!(
        captured[5].body,
        Some(json!({"name":"Payments","suite_id":20}))
    );
    assert_eq!(captured[6].endpoint, "delete_section/5&soft=1");

    let unknown = tool(&tools, "add_case")
        .execute(context(), json!({"section_id":"500","title":"x"}))
        .await
        .expect_err("a 5xx effect is an unknown outcome");
    assert_eq!(unknown.category, ErrorCategory::Internal);
    assert!(!unknown.retry.should_retry);

    let before = transport.captured().len();
    for (name, arguments) in [
        (
            "add_case",
            json!({"section_id":"5","title":"x","case_properties":"[1]"}),
        ),
        (
            "add_case",
            json!({"section_id":"5","title":"x","case_properties":"{\"title\":\"y\"}"}),
        ),
        ("add_case", json!({"section_id":"../5","title":"x"})),
        (
            "add_cases",
            json!({"add_test_cases_data":"[{\"title\":\"no section\"}]"}),
        ),
        (
            "add_section",
            json!({"project_id":"2","name":"x","section_properties":"[]"}),
        ),
        ("delete_case", json!({"case_id":"42","soft_delete":"yes"})),
        ("get_case", json!({"testcase_id":"7","extra":1})),
    ] {
        assert!(
            tool(&tools, name)
                .execute(context(), arguments.clone())
                .await
                .is_err(),
            "{name} accepted {arguments}"
        );
    }
    assert_eq!(transport.captured().len(), before);
}

#[tokio::test]
async fn selection_skips_unserved_tools_and_policy_filters() {
    let client: Arc<dyn TestRailApi> = Arc::new(TestRailClient::with_transport(
        config(),
        FixtureTransport::new(provider),
    ));
    let selected = test_build_with_api(
        "qa",
        "https://tr.example.test/testrail",
        &["get_case", "add_file_to_case", "index_data"],
        &policy(&[("testrail", &["delete_case"])]),
        &client,
        None,
    )
    .expect("selected TestRail tools");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let names = selected
        .tools(readonly)
        .await
        .expect("tools")
        .iter()
        .map(|tool| tool.name().to_owned())
        .collect::<Vec<_>>();
    assert_eq!(names, ["get_case"]);
    let only_unserved = TestRailToolkitConfig::parse(&settings(&["add_file_to_case"]))
        .expect("bounded selection parses");
    let Err(error) = build_testrail_toolset("qa", only_unserved, &policy(&[]), None) else {
        panic!("an unserved-only selection must not build");
    };
    assert_eq!(error.code(), TestRailToolsetErrorCode::UnsupportedSelection);
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let platform = FakePlatform::new();
    let host = platform.host();
    let tools = tools_with_host(FixtureTransport::new(provider), Some(&host)).await;
    assert_eq!(tools.len(), 17);
    let writes = tools
        .iter()
        .filter(|tool| !tool.is_read_only())
        .map(|tool| tool.name())
        .collect::<Vec<_>>();
    assert_eq!(
        writes,
        [
            "add_case",
            "add_cases",
            "update_case",
            "delete_case",
            "add_section",
            "delete_section",
            "add_file_to_case"
        ]
    );
    super::sdk_conformance::assert_sdk_conformance("testrail", &tools);
}

async fn attach_tools(platform: &Arc<FakePlatform>) -> (Arc<FixtureTransport>, Vec<Arc<dyn Tool>>) {
    let transport = FixtureTransport::new(provider);
    let host = platform.host();
    let tools = tools_with_host(Arc::clone(&transport), Some(&host)).await;
    (transport, tools)
}

/// A failure the model reads as the tool result (not an error).
async fn failure(tools: &[Arc<dyn Tool>], arguments: Value) -> String {
    call(tools, "add_file_to_case", arguments)
        .await
        .as_str()
        .expect("a sentence")
        .to_owned()
}

async fn refusal(tools: &[Arc<dyn Tool>], arguments: Value) -> adk_core::AdkError {
    tool(tools, "add_file_to_case")
        .execute(context(), arguments)
        .await
        .expect_err("the call must be refused")
}

#[tokio::test]
async fn add_file_to_case_is_served_only_with_a_host_and_is_an_effect() {
    let none = tools_with(FixtureTransport::new(provider)).await;
    assert_eq!(none.len(), 16);
    assert!(none.iter().all(|tool| tool.name() != "add_file_to_case"));
    let platform = FakePlatform::new();
    let (_, tools) = attach_tools(&platform).await;
    assert_eq!(tools.len(), 17);
    let attach = tool(&tools, "add_file_to_case");
    assert!(!attach.is_read_only());
    assert!(!attach.is_concurrency_safe());
    assert!(attach.description().contains("append"));
    super::sdk_conformance::assert_sdk_conformance("testrail", &tools);
    // A selection of only the host tool builds with a host, not without.
    let client: Arc<dyn TestRailApi> = Arc::new(TestRailClient::with_transport(
        config(),
        FixtureTransport::new(provider),
    ));
    let host = platform.host();
    assert!(
        test_build_with_api(
            "qa",
            "i",
            &["add_file_to_case"],
            &policy(&[]),
            &client,
            Some(&host)
        )
        .is_ok()
    );
    assert_eq!(
        test_build_with_api(
            "qa",
            "i",
            &["add_file_to_case"],
            &policy(&[]),
            &client,
            None
        )
        .err()
        .map(|error| error.code()),
        Some(TestRailToolsetErrorCode::UnsupportedSelection)
    );
}

#[tokio::test]
async fn add_file_to_case_uploads_multipart_and_reports_the_attachment() {
    let platform = FakePlatform::new();
    platform.put("bucket", "shots/login.png", PNG);
    let (transport, tools) = attach_tools(&platform).await;
    let text = call(
        &tools,
        "add_file_to_case",
        json!({"case_id":"42","filepath":"/bucket/shots/login.png"}),
    )
    .await;
    assert_eq!(
        text,
        json!(
            "File 'login.png' successfully attached to test case 42. Attachment ID: 777. To embed this file inline in a rich-text field (e.g. custom_steps, custom_preconds, custom_expected), use update_case with: <img src=\"index.php?/attachments/get/777\" id=\"attachment-777\" data-attachment-id=\"777\" />"
        )
    );
    let captured = transport.captured();
    assert_eq!(captured.len(), 1);
    assert_eq!(captured[0].method, Method::POST);
    let (content_type, body) = captured[0].upload.clone().expect("upload");
    let boundary = content_type.split("boundary=").nth(1).expect("boundary");
    let head = format!(
        "--{boundary}\r\nContent-Disposition: form-data; name=\"attachment\"; filename=\"login.png\"\r\nContent-Type: application/octet-stream\r\n\r\n"
    );
    assert!(body.starts_with(head.as_bytes()));
    assert!(body.ends_with(format!("\r\n--{boundary}--\r\n").as_bytes()));
    assert_eq!(&body[head.len()..head.len() + PNG.len()], PNG);
    // The filename argument wins over the artifact's name.
    call(
        &tools,
        "add_file_to_case",
        json!({"case_id":42,"filepath":"bucket/shots/login.png","filename":"renamed.png"}),
    )
    .await;
    let body = transport.captured()[1].upload.clone().expect("upload").1;
    assert!(String::from_utf8_lossy(&body).contains("filename=\"renamed.png\""));
    // The same name twice is two uploads: TestRail keeps both, as the SDK does.
    assert_eq!(transport.endpoints().len(), 2);
}

#[tokio::test]
async fn add_file_to_case_embeds_inline_and_reports_a_failed_embed_exactly() {
    let platform = FakePlatform::new();
    platform.put("bucket", "a.txt", b"hello");
    let (transport, tools) = attach_tools(&platform).await;
    let text = call(
        &tools,
        "add_file_to_case",
        json!({"case_id":"50","filepath":"/bucket/a.txt","embed_in_field":"custom_steps","embed_text_before":"Before:<br/>","embed_text_after":null}),
    )
    .await;
    assert_eq!(
        text,
        json!(
            "File 'a.txt' attached to test case 50 (attachment_id: 888), but failed to embed in field 'custom_steps': TestRail rejected the update as invalid (does the field exist and accept HTML?). The file is attached; the field was not confirmed updated."
        )
    );
    let captured = transport.captured();
    assert_eq!(captured[1].endpoint, "get_case/50");
    assert_eq!(captured[2].endpoint, "update_case/50");
    assert_eq!(
        captured[2].body,
        Some(
            json!({"custom_steps":"Before:<br/><img src=\"index.php?/attachments/get/888\" id=\"attachment-888\" data-attachment-id=\"888\" />"})
        )
    );
    // A lost answer on the second request: attached, outcome of the embed unknown.
    let text = call(
        &tools,
        "add_file_to_case",
        json!({"case_id":"51","filepath":"/bucket/a.txt","embed_in_field":"custom_expected"}),
    )
    .await;
    let text = text.as_str().expect("text");
    assert!(
        text.contains("(attachment_id: 889), but failed to embed"),
        "{text}"
    );
    assert!(
        text.contains("may have applied the requested effect"),
        "{text}"
    );
    assert!(!text.contains("successfully"), "{text}");
}

#[tokio::test]
async fn add_file_to_case_embeds_on_success() {
    let platform = FakePlatform::new();
    platform.put("bucket", "a.txt", b"hello");
    let (transport, tools) = attach_tools(&platform).await;
    // case 42 answers update_case/42 with a success.
    let text = call(
        &tools,
        "add_file_to_case",
        json!({"case_id":"42","filepath":"/bucket/a.txt","embed_in_field":"custom_preconds"}),
    )
    .await;
    assert_eq!(
        text,
        json!(
            "File 'a.txt' attached to test case 42 (attachment_id: 777) and embedded inline in field 'custom_preconds'."
        )
    );
    assert_eq!(
        transport.endpoints(),
        ["add_attachment_to_case/42", "get_case/42", "update_case/42"]
    );
    // The existing value is kept and the reference follows it.
    let captured = transport.captured();
    assert_eq!(
        captured[2].body,
        Some(
            json!({"custom_preconds":"<p>Existing</p><br/><img src=\"index.php?/attachments/get/777\" id=\"attachment-777\" data-attachment-id=\"777\" />"})
        )
    );
}

#[tokio::test]
async fn add_file_to_case_leaves_a_field_it_cannot_read_unchanged() {
    let platform = FakePlatform::new();
    platform.put("bucket", "a.txt", b"hello");
    let (transport, tools) = attach_tools(&platform).await;
    let text = call(
        &tools,
        "add_file_to_case",
        json!({"case_id":"52","filepath":"/bucket/a.txt","embed_in_field":"custom_steps"}),
    )
    .await;
    let text = text.as_str().expect("text");
    assert!(text.contains("(attachment_id: 890)"), "{text}");
    assert!(text.contains("could not be read"), "{text}");
    assert!(text.contains("left unchanged"), "{text}");
    assert_eq!(
        transport.endpoints(),
        ["add_attachment_to_case/52", "get_case/52"]
    );
}

#[tokio::test]
async fn add_file_to_case_names_each_failure_and_never_retries() {
    let platform = FakePlatform::new();
    platform.put("bucket", "a.txt", b"hello");
    let (transport, tools) = attach_tools(&platform).await;
    let args = |case: &str| json!({"case_id":case,"filepath":"/bucket/a.txt"});
    let lost = failure(&tools, args("44")).await;
    assert!(
        lost.contains("may have applied the requested effect"),
        "{lost}"
    );
    assert!(
        lost.contains("may or may not be attached to test case 44"),
        "{lost}"
    );
    let missing = failure(&tools, args("45")).await;
    assert_eq!(
        missing,
        "TestRail did not find test case 45; nothing was attached."
    );
    let denied = failure(&tools, args("46")).await;
    assert!(denied.contains("rejected the credentials"), "{denied}");
    let no_id = failure(&tools, args("43")).await;
    assert!(no_id.contains("returned no attachment id"), "{no_id}");
    assert!(no_id.contains("may be attached"), "{no_id}");
    // One request each: nothing was retried and no embed was attempted.
    assert_eq!(transport.endpoints().len(), 4);
}

#[tokio::test]
async fn add_file_to_case_validates_before_touching_the_store_or_provider() {
    let platform = FakePlatform::new();
    platform.put("bucket", "a.txt", b"hello");
    let (transport, tools) = attach_tools(&platform).await;
    for arguments in [
        json!({"case_id":"abc","filepath":"/bucket/a.txt"}),
        json!({"case_id":"42"}),
        json!({"case_id":"42","filepath":"/bucket/a.txt","extra":1}),
        json!({"case_id":"42","filepath":"/bucket/a.txt","embed_in_field":"a b"}),
        json!({"case_id":"42","filepath":"/bucket/a.txt","filename":"x/y.png"}),
        json!({"case_id":"42","filepath":"/bucket/a.txt","filename":"x\"y.png"}),
        json!({"case_id":"42","filepath":"/bucket/a.txt","filename":".."}),
        json!({"case_id":"42","filepath":"/bucket/a.txt","embed_text_before":5}),
    ] {
        let error = refusal(&tools, arguments.clone()).await;
        assert_eq!(error.category, ErrorCategory::InvalidInput, "{arguments}");
    }
    assert!(transport.endpoints().is_empty());
}

#[tokio::test]
async fn add_file_to_case_reports_host_failures_before_sending_anything() {
    let platform = FakePlatform::new();
    platform.put("bucket", "a.txt", b"hello");
    platform.put("bucket", "empty.txt", b"");
    platform.put("bucket", "big.bin", &vec![1_u8; (50 << 20) + 1]);
    let (transport, tools) = attach_tools(&platform).await;
    let ask = |path: &str| json!({"case_id":"42","filepath":path});
    let bad_path = failure(&tools, ask("/bucket/../x")).await;
    assert!(
        bad_path.contains("artifact path is not valid"),
        "{bad_path}"
    );
    let missing = failure(&tools, ask("/bucket/none.txt")).await;
    assert!(missing.contains("was not found"), "{missing}");
    let empty = failure(&tools, ask("/bucket/empty.txt")).await;
    assert!(empty.contains("empty"), "{empty}");
    let big = failure(&tools, ask("/bucket/big.bin")).await;
    assert!(big.contains("50 MiB"), "{big}");
    assert_eq!(
        platform.read_count(),
        2,
        "the over-cap file is refused from its size"
    );
    platform.fail_reads(OfficeFailure::AccessRefused);
    let denied = failure(&tools, ask("/bucket/a.txt")).await;
    assert!(denied.contains("refused access"), "{denied}");
    assert!(denied.contains("Nothing was sent"), "{denied}");
    assert!(transport.endpoints().is_empty());
}

#[tokio::test]
async fn add_file_to_case_sends_an_ascii_fallback_name_beside_the_real_one() {
    let platform = FakePlatform::new();
    platform.put("bucket", "отчёт.png", b"png");
    let (transport, tools) = attach_tools(&platform).await;
    call(
        &tools,
        "add_file_to_case",
        json!({"case_id":"42","filepath":"/bucket/отчёт.png"}),
    )
    .await;
    let body = String::from_utf8_lossy(&transport.captured()[0].upload.clone().expect("upload").1)
        .into_owned();
    assert!(body.contains("filename=\"attachment-"), "{body}");
    assert!(!body.contains("filename=\".png\""), "{body}");
    assert!(body.contains("filename*=UTF-8''"), "{body}");
}
