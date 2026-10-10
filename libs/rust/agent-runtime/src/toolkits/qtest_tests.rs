use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::header::AUTHORIZATION;
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::qtest::client::{
    QtestApi, QtestClient, QtestClientError, QtestHttpResponse, QtestTransport,
};
use super::families::qtest::config::{QtestConfigErrorCode, QtestToolkitConfig};
use super::families::qtest::tools::{
    QtestToolsetErrorCode, build_qtest_toolset, test_build_with_api,
};
use super::policy::ToolAdmissionPolicy;

const ROOT: &str = "https://qtest.example.test/api/v3/projects/7/";

fn settings(selected_tools: &[&str]) -> Map<String, Value> {
    json!({
        "qtest_configuration":{"base_url":"https://qtest.example.test/","qtest_api_token":"qtest-secret"},
        "qtest_project_id":7,
        "no_of_tests_shown_in_dql_search":null,
        "selected_tools":selected_tools
    })
    .as_object()
    .cloned()
    .expect("qTest fixture settings are an object")
}

fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("qTest policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("qtest-test").with_function_call_id("qtest-call"))
}

#[derive(Clone, Debug)]
struct Captured {
    method: Method,
    target: String,
    body: Option<Value>,
}

type Handler = dyn Fn(&Method, &str, Option<&Value>) -> QtestHttpResponse + Send + Sync;

struct FixtureTransport {
    requests: Mutex<Vec<Captured>>,
    handler: Box<Handler>,
}

impl FixtureTransport {
    fn new(
        handler: impl Fn(&Method, &str, Option<&Value>) -> QtestHttpResponse + Send + Sync + 'static,
    ) -> Arc<Self> {
        Arc::new(Self {
            requests: Mutex::new(Vec::new()),
            handler: Box::new(handler),
        })
    }

    fn captured(&self) -> Vec<Captured> {
        self.requests.lock().expect("qTest fixture lock").clone()
    }

    fn targets(&self) -> Vec<String> {
        self.captured()
            .into_iter()
            .map(|captured| format!("{} {}", captured.method, captured.target))
            .collect()
    }
}

#[async_trait]
impl QtestTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        _effect: bool,
    ) -> Result<QtestHttpResponse, QtestClientError> {
        let target = request
            .url()
            .as_str()
            .strip_prefix(ROOT)
            .expect("project-scoped qTest URL")
            .to_owned();
        let authorization = request.headers().get(AUTHORIZATION).expect("bearer");
        assert_eq!(
            authorization.to_str().expect("ASCII"),
            "Bearer qtest-secret"
        );
        assert!(authorization.is_sensitive());
        let body = request
            .body()
            .and_then(reqwest::Body::as_bytes)
            .map(|bytes| serde_json::from_slice(bytes).expect("JSON body"));
        self.requests
            .lock()
            .expect("qTest fixture lock")
            .push(Captured {
                method: request.method().clone(),
                target: target.clone(),
                body: body.clone(),
            });
        Ok((self.handler)(request.method(), &target, body.as_ref()))
    }
}

fn ok(body: Value) -> QtestHttpResponse {
    QtestHttpResponse::fixture(StatusCode::OK, Some(body))
}

fn test_case(pid: &str, id: u64, name: &str) -> Value {
    json!({
        "pid":pid,"id":id,"name":name,"description":"<p>Check &amp; log in</p>","precondition":"",
        "web_url":"https://qtest/tc","test_steps":[{"description":"Open","expected":"Shown"}],
        "properties":[
            {"field_name":"Priority","field_value":3,"field_value_name":"High"},
            {"field_name":"Team","field_value":"[21]","field_value_name":"[A]"}
        ]
    })
}

fn search(body: &Value) -> QtestHttpResponse {
    let object_type = body["object_type"].as_str().expect("object_type");
    let query = body["query"].as_str().expect("query");
    let page = |items: Value, next: bool| {
        let links = if next {
            json!([{"rel":"next","href":"x"}])
        } else {
            json!([])
        };
        ok(
            json!({"page":1,"page_size":100,"total":items.as_array().map_or(0, Vec::len),"items":items,"links":links}),
        )
    };
    match (object_type, query) {
        ("test-cases", "Name ~ 'login'") => page(
            json!([
                test_case("TC-1", 501, "Login"),
                test_case("TC-2", 502, "Logout")
            ]),
            true,
        ),
        ("test-cases", "Id = 'TC-1'") => page(json!([test_case("TC-1", 501, "Login")]), false),
        ("test-cases", "Id = 'TC-9'" | "") | ("test-runs", "Id = 'TR-404'") => {
            page(json!([]), false)
        }
        ("test-cases", "'id' = '501'") => page(json!([{"id":501,"pid":"TC-1"}]), false),
        ("test-runs", "Id = 'TR-3'") => page(
            json!([{"pid":"TR-3","id":9,"name":"Nightly","testCaseId":501,
                "latest_test_log":{"id":77,"status":"Failed","exe_start_date":"s","exe_end_date":"e"},
                "properties":[]}]),
            false,
        ),
        ("defects", "Id = 'DF-1'") => page(
            json!([{"pid":"DF-1","id":31,"name":"Crash","properties":[{"field_name":"Severity","field_value":"<b>Major</b>"}]}]),
            false,
        ),
        ("requirements", "Id = 'RQ-15'") => page(json!([{"pid":"RQ-15","id":41}]), false),
        other => panic!("unexpected qTest search {other:?}"),
    }
}

/// qTest Manager v3 responses for project 7.
fn provider(method: &Method, target: &str, body: Option<&Value>) -> QtestHttpResponse {
    let (path, query) = target.split_once('?').unwrap_or((target, ""));
    match (method.as_str(), path) {
        ("POST", "search") if query.contains("page=2") => ok(json!({
            "total":3,"items":[test_case("TC-3", 503, "Login again")],"links":[]
        })),
        ("POST", "search") => search(body.expect("search body")),
        ("GET", "settings/test-cases/fields") => ok(json!([
            {"id":1,"label":"Priority","required":true,"allowed_values":[
                {"label":"High","value":11,"is_active":true},{"label":"Low","value":12,"is_active":true}
            ]},
            {"id":2,"label":"Team","multiple":true,"allowed_values":[{"label":"A","value":21,"is_active":true}]},
            {"id":3,"label":"Notes"}
        ])),
        ("GET", "modules") if query == "expand=descendants" => ok(json!([
            {"id":100,"pid":"MD-1","name":"Root","children":[{"id":101,"pid":"MD-2","name":"Login","children":[]}]}
        ])),
        ("GET", "modules") => ok(json!([
            {"id":101,"pid":"MD-2","name":"Login","children":[{"id":102,"pid":"MD-3","name":"SSO"}]}
        ])),
        ("POST", "test-cases") => {
            ok(json!({"id":601,"pid":"TC-6","name":"New","web_url":"https://qtest/tc6"}))
        }
        ("PUT", "test-cases/501") => ok(json!({"id":501,"pid":"TC-1"})),
        ("DELETE", "test-cases/501") => QtestHttpResponse::fixture(StatusCode::OK, None),
        ("GET", "linked-artifacts") if query == "type=test-runs&ids=9" => ok(json!([
            {"id":9,"pid":"TR-3","objects":[{"id":31,"pid":"DF-1"},{"id":501,"pid":"TC-1"}]}
        ])),
        ("POST", "requirements/41/link") => ok(json!([
            {"id":41,"pid":"RQ-15","objects":[{"id":501,"pid":"TC-1"}]}
        ])),
        ("GET", "test-runs/execution-statuses") => ok(json!([
            {"id":601,"name":"Passed"},{"id":602,"name":"Failed"}
        ])),
        ("GET", "test-cases/501/versions/999") => {
            QtestHttpResponse::fixture(StatusCode::NOT_FOUND, None)
        }
        ("GET", "test-cases/501/versions/4001") => ok(json!({"test_case_version_id":4001})),
        ("GET", "test-cases/501/versions") => ok(json!([
            {"test_case_version_id":4001,"version":"1.0","name":"Login"},
            {"test_case_version_id":4002,"version":"2.0","name":"Login"}
        ])),
        ("POST", "test-runs/9/test-logs") => ok(json!({"id":880})),
        other => panic!("unexpected qTest request {other:?} {query}"),
    }
}

async fn tools_with(transport: Arc<FixtureTransport>) -> Vec<Arc<dyn Tool>> {
    let config = QtestToolkitConfig::parse(&settings(&[])).expect("valid qTest configuration");
    let client: Arc<dyn QtestApi> = Arc::new(QtestClient::with_transport(config, transport));
    let toolset = test_build_with_api("qa", 7, &[], &policy(), client).expect("qTest toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    toolset.tools(readonly).await.expect("qTest tools")
}

async fn call(tools: &[Arc<dyn Tool>], name: &str, arguments: Value) -> Value {
    tools
        .iter()
        .find(|tool| tool.name() == name)
        .unwrap_or_else(|| panic!("qTest tool {name}"))
        .execute(context(), arguments)
        .await
        .unwrap_or_else(|error| panic!("{name}: {error}"))
}

#[test]
fn configuration_reads_the_project_and_redacts_the_token() {
    let mut legacy = settings(&[]);
    legacy.remove("qtest_project_id");
    legacy.insert("project_id".to_owned(), json!("7"));
    QtestToolkitConfig::parse(&legacy).expect("legacy project_id is honoured");
    for invalid in [
        json!({}),
        json!({"qtest_configuration":{"base_url":"https://q.test","qtest_api_token":"t"}}),
        json!({"qtest_configuration":{"base_url":"http://q.test","qtest_api_token":"t"},"qtest_project_id":7}),
        json!({"qtest_configuration":{"base_url":"https://q.test","qtest_api_token":"a b"},"qtest_project_id":7}),
        json!({"qtest_configuration":{"base_url":"https://q.test","qtest_api_token":"t"},"qtest_project_id":"x"}),
    ] {
        let Err(error) = QtestToolkitConfig::parse(invalid.as_object().expect("object")) else {
            panic!("invalid qTest configuration accepted: {invalid}");
        };
        assert_eq!(error.code(), QtestConfigErrorCode::InvalidConfiguration);
        assert!(!format!("{error:?}").contains("qtest-secret"));
    }
}

#[tokio::test]
async fn searches_page_parse_and_render_sdk_text() {
    let transport = FixtureTransport::new(provider);
    let tools = tools_with(Arc::clone(&transport)).await;
    assert!(
        tools[0]
            .description()
            .starts_with("Search test cases in qTest using Data Query Language (DQL).")
    );
    assert!(
        tools[1]
            .description()
            .ends_with("\nUrl: https://qtest.example.test. Project id: 7\nToolkit: qa")
    );
    let found = call(
        &tools,
        "search_by_dql",
        json!({"dql":"Name ~ 'login'","max_results":0}),
    )
    .await;
    let found = found.as_str().expect("text");
    assert!(found.starts_with("Found 3 Qtest test cases:\n[{'Id': 'TC-1', 'Name': 'Login', 'Description': ' Check & log in ', 'Precondition': '', 'QTest Id': 501, 'Steps': [{'Test Step Number': 1, 'Test Step Description': 'Open', 'Test Step Expected Result': 'Shown'}], 'Priority': 'High', 'Team': ['A']}"));
    assert!(found.contains("'Id': 'TC-3'"));
    let first = &transport.captured()[0];
    assert_eq!(
        first.target,
        "search?appendTestSteps=false&includeExternalProperties=false&pageSize=100&page=1"
    );
    assert_eq!(
        first.body,
        Some(json!({"object_type":"test-cases","fields":["*"],"query":"Name ~ 'login'"}))
    );

    let entity = call(&tools, "find_entity_by_id", json!({"entity_id":"TR-3"})).await;
    assert_eq!(entity["Latest Test Log"]["Status"], "Failed");
    assert_eq!(
        call(&tools, "find_entity_by_id", json!({"entity_id":"XX-1"})).await,
        json!(
            "Invalid entity ID format 'XX-1'. Expected prefix to be one of: BL, CL, DF, RL, RQ, TC, TR, TS"
        )
    );
    assert_eq!(
        call(&tools, "find_entity_by_id", json!({"entity_id":"TR-404"})).await,
        json!("Test Run 'TR-404' not found in project 7")
    );
    let defects = call(
        &tools,
        "find_defects_by_test_run_id",
        json!({"test_run_id":"TR-3"}),
    )
    .await;
    assert_eq!(
        defects,
        json!({
            "test_run_id":"TR-3","total":1,"source_test_case_id":"TC-1",
            "defects":[{"Id":"DF-1","QTest Id":31,"Name":"Crash","Severity":" Major "}]
        })
    );
    let modules = call(
        &tools,
        "get_modules",
        json!({"parent_id":101,"search":"SSO"}),
    )
    .await;
    assert_eq!(
        modules,
        json!(
            "Found 2 module(s):\n[{'id': 101, 'name': 'Login', 'pid': 'MD-2', 'full_name': 'MD-2 Login', 'level': 0, 'has_children': True}, {'id': 102, 'name': 'SSO', 'pid': 'MD-3', 'full_name': 'MD-3 SSO', 'level': 1, 'has_children': False}]"
        )
    );
    let versions = call(
        &tools,
        "get_test_case_versions",
        json!({"test_case_id":"501","version_name":" 2.0 "}),
    )
    .await;
    assert_eq!(
        versions,
        json!({"test_case_id":"501","qtest_test_case_id":501,"total":1,
            "versions":[{"version_id":4002,"version":"2.0","name":"Login"}]})
    );
    let targets = transport.targets();
    assert!(targets.contains(&"GET linked-artifacts?type=test-runs&ids=9".to_owned()));
    assert!(targets.contains(&"GET modules?parentId=101&search=SSO".to_owned()));
}

#[tokio::test]
async fn fields_fall_back_to_properties_without_field_management() {
    let transport = FixtureTransport::new(|method, target, body| {
        let (path, query) = target.split_once('?').unwrap_or((target, ""));
        match (method.as_str(), path) {
            ("GET", "settings/test-cases/fields") => {
                QtestHttpResponse::fixture(StatusCode::FORBIDDEN, None)
            }
            ("POST", "search") => {
                assert_eq!(
                    query,
                    "appendTestSteps=false&includeExternalProperties=false&pageSize=1&page=1"
                );
                assert_eq!(body.expect("body")["query"], "");
                ok(json!({"total":1,"items":[{"id":501}],"links":[]}))
            }
            ("GET", "test-cases/501/properties") => {
                assert_eq!(query, "calledBy=testcase_properties");
                ok(json!([
                    {"id":1,"name":"Priority","required":true},
                    {"id":2,"name":"Assigned To"},
                    {"id":3,"name":"Shared"}
                ]))
            }
            ("GET", "test-cases/501/properties-info") => ok(json!({"metadata":[
                {"id":1,"data_type":"ComboboxDataType","allowed_values":[{"id":11,"value_text":"High"}]},
                {"id":2,"data_type":"UserListDataType","allowed_values":[{"id":91,"value_text":"Ann"}]}
            ]})),
            other => panic!("unexpected {other:?}"),
        }
    });
    let tools = tools_with(transport).await;
    let text = call(&tools, "get_all_test_cases_fields_for_project", json!({})).await;
    assert_eq!(
        text,
        json!(
            "Available Test Case Fields for Project 7:\n\n\nAssigned To (Multi-select):\n  - Ann\n\nPriority (Single-select (Required)):\n  - High\n\n\n--- Field Type Guide ---\n\nText fields: Use null to clear, provide string value to set.\n\nSingle-select: Provide exact value name from the list above. Cannot be cleared via API.\n\nMulti-select: Provide value as array [\"val1\", \"val2\"]. Use null to clear."
        )
    );
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One ordered corpus proves every effect's wire body.
async fn effects_validate_locally_then_send_sdk_bodies() {
    let transport = FixtureTransport::new(provider);
    let tools = tools_with(Arc::clone(&transport)).await;
    let created = call(
        &tools,
        "create_test_cases",
        json!({
            "test_case_content":"{\"Name\":\"New\",\"Description\":\"d\",\"Priority\":\"Low\",\"Team\":[\"A\"],\"Notes\":null,\"Steps\":[{\"Test Step Description\":\"Open\",\"Test Step Expected Result\":\"Shown\"}]}",
            "folder_to_place_test_cases_to":"MD-2 Login"
        }),
    )
    .await;
    assert_eq!(
        created,
        json!({"qtest_folder":"MD-2 Login","test_cases":[
            {"test_case_id":"TC-6","qtest_id":601,"test_case_name":"New","url":"https://qtest/tc6"}
        ]})
    );
    let post = transport
        .captured()
        .into_iter()
        .find(|request| request.target == "test-cases")
        .expect("create request");
    let body = post.body.expect("create body");
    assert_eq!(body["name"], "New");
    assert_eq!(body["description"], "d");
    assert_eq!(body["parent_id"], 101);
    assert_eq!(
        body["test_steps"],
        json!([{"description":"Open","expected":"Shown"}])
    );
    let mut properties = body["properties"].as_array().expect("properties").clone();
    properties.sort_by_key(|property| property["field_id"].as_u64());
    assert_eq!(
        properties,
        [
            json!({"field_id":1,"field_name":"Priority","field_value":12,"field_value_name":"Low"}),
            json!({"field_id":2,"field_name":"Team","field_value":"[21]","field_value_name":"A"}),
            json!({"field_id":3,"field_name":"Notes","field_value":"","field_value_name":""}),
        ]
    );

    let before = transport.captured().len();
    let refused = call(
        &tools,
        "create_test_cases",
        json!({"test_case_content":"{\"Name\":\"x\",\"Priority\":\"Urgent\",\"Colour\":\"red\"}"}),
    )
    .await;
    let refused = refused.as_str().expect("validation report");
    assert!(refused.starts_with("Found 2 validation error(s) in test case properties:"));
    assert!(refused.contains("Allowed values: High, Low"));
    assert!(
        transport
            .captured()
            .iter()
            .skip(before)
            .all(|request| request.target != "test-cases"),
        "an invalid case is never sent"
    );

    let updated = call(
        &tools,
        "update_test_case",
        json!({"test_id":"TC-1","test_case_content":"{\"Priority\":\"Low\"}"}),
    )
    .await;
    let updated = updated.as_str().expect("update text");
    assert!(updated.starts_with("Successfully updated test case in project with id - 7.\n            Updated test case id - TC-1.\n            Test id of updated test case - TC-1.\n            Updated with content:\n{'Id': 'TC-1', 'Name': 'Login',"));
    assert!(updated.contains("'Priority': 'Low'"));
    let put = transport
        .captured()
        .into_iter()
        .find(|request| request.method == Method::PUT)
        .expect("update request");
    assert_eq!(put.target, "test-cases/501");

    assert_eq!(
        call(
            &tools,
            "update_test_run_status",
            json!({"test_run_id":"TR-3","status":"passed","testcase_version_id":999})
        )
        .await,
        json!(
            "Test case version ID 999 does not exist for test case 501 in project 7. Known versions of test case 501: 1.0 (id=4001), 2.0 (id=4002). Use the get_test_case_versions tool to resolve a version name to its ID."
        )
    );
    assert_eq!(
        call(
            &tools,
            "update_test_run_status",
            json!({"test_run_id":"TR-3","status":"Broken"})
        )
        .await,
        json!(
            "Status 'Broken' is not a valid execution status in project 7. Allowed values: Failed, Passed."
        )
    );
    assert_eq!(
        call(
            &tools,
            "update_test_run_status",
            json!({"test_run_id":"TR-3","status":"passed","note":"ok","testcase_version_id":4001})
        )
        .await,
        json!(
            "Successfully recorded test run TR-3 status as 'passed' in project 7 by creating a new manual execution log. Test case version id: 4001. Test log id: 880."
        )
    );
    let log = transport
        .captured()
        .into_iter()
        .find(|request| request.target == "test-runs/9/test-logs")
        .and_then(|request| request.body)
        .expect("test log body");
    assert_eq!(log["status"], json!({"id":601}));
    assert_eq!(log["note"], "ok");
    assert_eq!(log["test_case_version_id"], 4001);
    let end = log["exe_end_date"].as_str().expect("end");
    assert!(end.ends_with("+00:00") && !end.contains('.'), "{end}");

    assert_eq!(
        call(
            &tools,
            "link_tests_to_qtest_requirement",
            json!({"requirement_id":"RQ-15","json_list_of_test_case_ids":"[\"TC-1\"]"})
        )
        .await,
        json!(
            "Successfully linked 1 test case(s) to QTest requirement 'RQ-15' in project 7.\nLinked test cases: TC-1"
        )
    );
    let link = transport
        .captured()
        .into_iter()
        .find(|request| request.target.starts_with("requirements/41/link"))
        .expect("link request");
    assert_eq!(link.target, "requirements/41/link?type=test-cases");
    assert_eq!(link.body, Some(json!([501])));
    assert_eq!(
        call(
            &tools,
            "link_tests_to_qtest_requirement",
            json!({"requirement_id":"RQ-15","json_list_of_test_case_ids":"[\"TC-9\"]"})
        )
        .await,
        json!("Test case 'TC-9' not found in project 7.")
    );
    assert_eq!(
        call(&tools, "delete_test_case", json!({"qtest_id":501})).await,
        json!("Successfully deleted test case in project with id - 7 and qtest id - 501.")
    );
    assert!(
        tools
            .iter()
            .find(|tool| tool.name() == "search_by_dql")
            .expect("search tool")
            .execute(context(), json!({"dql":"Id = 'x'","bogus":1}))
            .await
            .is_err()
    );
}

#[tokio::test]
async fn selection_skips_artifact_and_index_tools_and_the_contract_holds() {
    let only_unserved = QtestToolkitConfig::parse(&settings(&[
        "add_file_to_test_case",
        "upload_attachment_to_test_run",
        "index_data",
    ]))
    .expect("bounded selection parses");
    let Err(error) = build_qtest_toolset("qa", only_unserved, &policy()) else {
        panic!("an unserved-only selection must not build");
    };
    assert_eq!(error.code(), QtestToolsetErrorCode::UnsupportedSelection);

    let tools = tools_with(FixtureTransport::new(provider)).await;
    assert_eq!(tools.len(), 17);
    assert_eq!(
        tools
            .iter()
            .filter(|tool| !tool.is_read_only())
            .map(|tool| tool.name())
            .collect::<Vec<_>>(),
        [
            "create_test_cases",
            "update_test_case",
            "update_test_run_status",
            "delete_test_case",
            "link_tests_to_jira_requirement",
            "link_tests_to_qtest_requirement"
        ]
    );
    super::sdk_conformance::assert_sdk_conformance("qtest", &tools);
}
