use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_core::{ErrorCategory, ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::header::{AUTHORIZATION, CONTENT_TYPE};
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

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
        assert_eq!(
            request
                .headers()
                .get(CONTENT_TYPE)
                .map(reqwest::header::HeaderValue::as_bytes),
            Some(&b"application/json"[..])
        );
        let body = request
            .body()
            .and_then(reqwest::Body::as_bytes)
            .map(|bytes| serde_json::from_slice(bytes).expect("JSON request body"));
        self.requests
            .lock()
            .expect("TestRail fixture lock")
            .push(Captured {
                method: request.method().clone(),
                endpoint: endpoint.clone(),
                body,
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
        ("POST", "update_case/42") => ok(json!({"id":42,"updated_on":1_700_000_100})),
        ("POST", "delete_case/42" | "delete_section/5") => {
            TestRailHttpResponse::fixture(StatusCode::OK, None)
        }
        ("POST", "add_section/2") => ok(json!({"id":6,"name":"Payments"})),
        ("POST", "add_case/500") => {
            TestRailHttpResponse::fixture(StatusCode::INTERNAL_SERVER_ERROR, None)
        }
        other => panic!("unexpected TestRail request {other:?}"),
    }
}

async fn tools_with(transport: Arc<FixtureTransport>) -> Vec<Arc<dyn Tool>> {
    let client: Arc<dyn TestRailApi> =
        Arc::new(TestRailClient::with_transport(config(), transport));
    let toolset = test_build_with_api(
        "qa",
        "https://tr.example.test/testrail",
        &[],
        &policy(&[]),
        &client,
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
    let Err(error) = build_testrail_toolset("qa", only_unserved, &policy(&[])) else {
        panic!("an unserved-only selection must not build");
    };
    assert_eq!(error.code(), TestRailToolsetErrorCode::UnsupportedSelection);
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let tools = tools_with(FixtureTransport::new(provider)).await;
    assert_eq!(tools.len(), 16);
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
            "delete_section"
        ]
    );
    super::sdk_conformance::assert_sdk_conformance("testrail", &tools);
}
