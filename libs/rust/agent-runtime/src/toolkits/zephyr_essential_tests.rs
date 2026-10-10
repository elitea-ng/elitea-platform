//! Zephyr Essential family: configuration, every route, the SDK's special
//! cases and the SDK schema gate, over fixtures shaped like the Zephyr Scale
//! Cloud v2 API.

use std::sync::Arc;

use adk_core::{Tool, Toolset};
use reqwest::{Method, StatusCode};
use serde_json::{Value, json};

use super::families::zephyr_essential::client::ZephyrEssentialClient;
use super::families::zephyr_essential::config::{DEFAULT_BASE_URL, ZephyrEssentialToolkitConfig};
use super::families::zephyr_essential::tools::{test_build_with_client, test_catalog};
use super::families::zephyr_rest::client::ZephyrRestResponse;
use super::families::zephyr_rest::config::ZephyrRestConfigErrorCode;
use super::families::zephyr_rest::tools::ZephyrRestToolsetErrorCode;
use super::zephyr_rest_tests::{
    FixtureTransport, TOKEN, policy, readonly, rest_client, tool_context,
};

const BASE: &str = "https://api.zephyrscale.example.test/v2";

fn client(transport: &Arc<FixtureTransport>) -> Arc<ZephyrEssentialClient> {
    Arc::new(ZephyrEssentialClient::new(rest_client(BASE, transport)))
}

async fn tools(transport: &Arc<FixtureTransport>, selected: &[&str]) -> Vec<Arc<dyn Tool>> {
    test_build_with_client("Essential QA", selected, &policy(&[]), &client(transport))
        .expect("Zephyr Essential toolset")
        .tools(readonly())
        .await
        .expect("Zephyr Essential tools")
}

async fn run(
    transport: &Arc<FixtureTransport>,
    name: &str,
    arguments: Value,
) -> adk_core::Result<Value> {
    let tools = tools(transport, &[name]).await;
    assert_eq!(tools.len(), 1, "{name} is served");
    tools[0].execute(tool_context(), arguments).await
}

fn folders_page(values: &Value, is_last: bool) -> Value {
    json!({"startAt":0,"maxResults":100,"isLast":is_last,"values":values})
}

#[test]
fn configuration_defaults_the_cloud_base_and_requires_the_token() {
    for (configuration, expected) in [
        (json!({"token":TOKEN}), DEFAULT_BASE_URL),
        (json!({"token":TOKEN,"base_url":""}), DEFAULT_BASE_URL),
        (
            json!({"token":TOKEN,"base_url":"https://eu.api.zephyrscale.smartbear.com/v2/"}),
            "https://eu.api.zephyrscale.smartbear.com/v2",
        ),
    ] {
        let settings = json!({"zephyr_essential_configuration":configuration});
        ZephyrEssentialToolkitConfig::parse(settings.as_object().expect("settings"))
            .unwrap_or_else(|_| panic!("valid configuration for {expected}"));
    }
    for invalid in [
        json!({"zephyr_essential_configuration":{"base_url":BASE}}),
        json!({"zephyr_essential_configuration":{"base_url":"http://plain.example.test","token":TOKEN}}),
        json!({"zephyr_configuration":{"base_url":BASE,"token":TOKEN}}),
    ] {
        let Err(error) = ZephyrEssentialToolkitConfig::parse(invalid.as_object().expect("object"))
        else {
            panic!("invalid Zephyr Essential configuration must fail");
        };
        assert_eq!(
            error.code(),
            ZephyrRestConfigErrorCode::InvalidConfiguration
        );
        assert!(!format!("{error:?} {error}").contains(TOKEN));
    }
}

#[tokio::test]
async fn catalog_serves_forty_one_operations_with_truthful_groups() {
    let catalog = test_catalog();
    assert_eq!(catalog.len(), 41);
    assert_eq!(catalog.iter().filter(|(_, read)| *read).count(), 24);
    for unserved in [
        "create_custom_executions",
        "create_cucumber_executions",
        "create_junit_executions",
        "retrieve_bdd_test_cases",
        "index_data",
    ] {
        assert!(catalog.iter().all(|(name, _)| *name != unserved));
    }
    let transport = FixtureTransport::new([]);
    let all = tools(&transport, &[]).await;
    assert_eq!(all.len(), 41);
    for tool in &all {
        assert!(tool.description().starts_with("Toolkit: Essential QA\n"));
        assert!(tool.description().chars().count() <= 1_000);
        assert_eq!(tool.is_read_only(), tool.is_concurrency_safe());
        if !tool.is_read_only() {
            assert!(
                tool.description()
                    .contains("read the current state before retrying")
            );
        }
    }
    let Err(error) = test_build_with_client(
        "Essential QA",
        &["create_junit_executions", "index_data"],
        &policy(&[]),
        &client(&transport),
    ) else {
        panic!("a selection of only unserved tools must be unsupported");
    };
    assert_eq!(
        error.code(),
        ZephyrRestToolsetErrorCode::UnsupportedSelection
    );
}

/// Every served operation, its arguments, and the exact request it sends.
#[allow(clippy::too_many_lines)] // One corpus keeps every route of the catalogue together.
#[tokio::test]
async fn every_operation_sends_the_sdk_route_query_and_body() {
    let body = r#"{"name":"N","projectKey":"PRJ"}"#;
    let cases: Vec<(&str, Value, &str, Option<Value>)> = vec![
        (
            "list_test_cases",
            json!({"project_key":"PRJ","folder_id":"12","max_results":5,"start_at":10}),
            "GET /testcases?projectKey=PRJ&folderId=12&maxResults=5&startAt=10",
            None,
        ),
        (
            "create_test_case",
            json!({"json":body}),
            "POST /testcases",
            Some(json!({"name":"N","projectKey":"PRJ"})),
        ),
        (
            "get_test_case",
            json!({"test_case_key":"PRJ-T1"}),
            "GET /testcases/PRJ-T1",
            None,
        ),
        (
            "update_test_case",
            json!({"test_case_key":"PRJ-T1","json":body}),
            "PUT /testcases/PRJ-T1",
            Some(json!({"name":"N","projectKey":"PRJ"})),
        ),
        (
            "get_test_case_links",
            json!({"test_case_key":"PRJ-T1"}),
            "GET /testcases/PRJ-T1/links",
            None,
        ),
        (
            "create_test_case_issue_link",
            json!({"test_case_key":"PRJ-T1","json":"{\"issueId\":10100}"}),
            "POST /testcases/PRJ-T1/links/issues",
            Some(json!({"issueId":10100})),
        ),
        (
            "create_test_case_web_link",
            json!({"test_case_key":"PRJ-T1","json":"{\"url\":\"https://example.com\"}"}),
            "POST /testcases/PRJ-T1/links/weblinks",
            Some(json!({"url":"https://example.com"})),
        ),
        (
            "list_test_case_versions",
            json!({"test_case_key":"PRJ-T1","max_results":2}),
            "GET /testcases/PRJ-T1/versions?maxResults=2",
            None,
        ),
        (
            "get_test_case_version",
            json!({"test_case_key":"PRJ-T1","version":3}),
            "GET /testcases/PRJ-T1/versions/3",
            None,
        ),
        (
            "get_test_case_test_script",
            json!({"test_case_key":"PRJ-T1"}),
            "GET /testcases/PRJ-T1/testscript",
            None,
        ),
        (
            "create_test_case_test_script",
            json!({"test_case_key":"PRJ-T1","json":"{\"type\":\"plain\",\"text\":\"t\"}"}),
            "POST /testcases/PRJ-T1/testscript",
            Some(json!({"type":"plain","text":"t"})),
        ),
        (
            "create_test_case_test_steps",
            json!({"test_case_key":"PRJ-T1","json":"{\"mode\":\"APPEND\",\"items\":[]}"}),
            "POST /testcases/PRJ-T1/teststeps",
            Some(json!({"mode":"APPEND","items":[]})),
        ),
        (
            "list_test_cycles",
            json!({"project_key":"PRJ","jira_project_version_id":"10000"}),
            "GET /testcycles?projectKey=PRJ&jiraProjectVersionId=10000",
            None,
        ),
        (
            "create_test_cycle",
            json!({"json":body}),
            "POST /testcycles",
            Some(json!({"name":"N","projectKey":"PRJ"})),
        ),
        (
            "get_test_cycle",
            json!({"test_cycle_id_or_key":"PRJ-R1"}),
            "GET /testcycles/PRJ-R1",
            None,
        ),
        (
            "update_test_cycle",
            json!({"test_cycle_id_or_key":"PRJ-R1","json":body}),
            "PUT /testcycles/PRJ-R1",
            Some(json!({"name":"N","projectKey":"PRJ"})),
        ),
        (
            "get_test_cycle_links",
            json!({"test_cycle_id_or_key":"PRJ-R1"}),
            "GET /testcycles/PRJ-R1/links",
            None,
        ),
        (
            "create_test_cycle_issue_link",
            json!({"test_cycle_id_or_key":"PRJ-R1","json":"{\"issueId\":1}"}),
            "POST /testcycles/PRJ-R1/links/issues",
            Some(json!({"issueId":1})),
        ),
        (
            "create_test_cycle_web_link",
            json!({"test_cycle_id_or_key":"PRJ-R1","json":"{\"url\":\"https://e.test\"}"}),
            "POST /testcycles/PRJ-R1/links/weblinks",
            Some(json!({"url":"https://e.test"})),
        ),
        (
            "list_test_executions",
            json!({"test_cycle":"PRJ-R1","test_case":"PRJ-T1","start_at":0}),
            "GET /testexecutions?testCycle=PRJ-R1&testCase=PRJ-T1&startAt=0",
            None,
        ),
        (
            "create_test_execution",
            json!({"json":"{\"projectKey\":\"PRJ\",\"testCaseKey\":\"PRJ-T1\",\"testCycleKey\":\"PRJ-R1\",\"statusName\":\"Pass\"}"}),
            "POST /testexecutions",
            Some(
                json!({"projectKey":"PRJ","testCaseKey":"PRJ-T1","testCycleKey":"PRJ-R1","statusName":"Pass"}),
            ),
        ),
        (
            "get_test_execution",
            json!({"test_execution_id_or_key":"PRJ-E1"}),
            "GET /testexecutions/PRJ-E1",
            None,
        ),
        (
            "update_test_execution",
            json!({"test_execution_id_or_key":"PRJ-E1","json":"{\"statusName\":\"Fail\"}"}),
            "PUT /testexecutions/PRJ-E1",
            Some(json!({"statusName":"Fail"})),
        ),
        (
            "get_test_execution_test_steps",
            json!({"test_execution_id_or_key":"PRJ-E1"}),
            "GET /testexecutions/PRJ-E1/teststeps",
            None,
        ),
        (
            "update_test_execution_test_steps",
            json!({"test_execution_id_or_key":"PRJ-E1","json":"{\"steps\":[]}"}),
            "PUT /testexecutions/PRJ-E1/teststeps",
            Some(json!({"steps":[]})),
        ),
        (
            "sync_test_execution_script",
            json!({"test_execution_id_or_key":"PRJ-E1"}),
            "POST /testexecutions/PRJ-E1/teststeps/sync",
            None,
        ),
        (
            "list_test_execution_links",
            json!({"test_execution_id_or_key":"PRJ-E1"}),
            "GET /testexecutions/PRJ-E1/links",
            None,
        ),
        (
            "create_test_execution_issue_link",
            json!({"test_execution_id_or_key":"PRJ-E1","json":"{\"issueId\":7}"}),
            "POST /testexecutions/PRJ-E1/links/issues",
            Some(json!({"issueId":7})),
        ),
        (
            "list_projects",
            json!({"max_results":50}),
            "GET /projects?maxResults=50",
            None,
        ),
        (
            "get_project",
            json!({"project_id_or_key":"PRJ"}),
            "GET /projects/PRJ",
            None,
        ),
        (
            "list_folders",
            json!({"project_key":"PRJ","folder_type":"TEST_CASE"}),
            "GET /folders?projectKey=PRJ&folderType=TEST_CASE",
            None,
        ),
        (
            "create_folder",
            json!({"json":"{\"parentId\":5,\"name\":\"F\",\"projectKey\":\"PRJ\",\"folderType\":\"TEST_CASE\"}"}),
            "POST /folders",
            Some(json!({"parentId":5,"name":"F","projectKey":"PRJ","folderType":"TEST_CASE"})),
        ),
        (
            "get_folder",
            json!({"folder_id":"5"}),
            "GET /folders/5",
            None,
        ),
        (
            "delete_link",
            json!({"link_id":"99"}),
            "DELETE /links/99",
            None,
        ),
        (
            "get_issue_link_test_cases",
            json!({"issue_key":"JIRA-1"}),
            "GET /issuelinks/JIRA-1/testcases",
            None,
        ),
        (
            "get_issue_link_test_cycles",
            json!({"issue_key":"JIRA-1"}),
            "GET /issuelinks/JIRA-1/testcycles",
            None,
        ),
        (
            "get_issue_link_test_plans",
            json!({"issue_key":"JIRA-1"}),
            "GET /issuelinks/JIRA-1/testplans",
            None,
        ),
        (
            "get_issue_link_test_executions",
            json!({"issue_key":"JIRA-1"}),
            "GET /issuelinks/JIRA-1/executions",
            None,
        ),
        ("healthcheck", json!({}), "GET /healthcheck", None),
    ];
    // find_folder_by_name and get_test_case_test_steps have their own tests.
    assert_eq!(cases.len(), 39);
    for (name, arguments, route, body) in cases {
        let transport = FixtureTransport::json([json!({"values":[],"ok":true})]);
        run(&transport, name, arguments)
            .await
            .unwrap_or_else(|error| panic!("{name}: {error:?}"));
        let requests = transport.requests();
        assert_eq!(requests.len(), 1, "{name}");
        let (method, path) = route.split_once(' ').expect("route");
        assert_eq!(
            requests[0].method,
            Method::from_bytes(method.as_bytes()).expect("method"),
            "{name}"
        );
        assert_eq!(requests[0].url, format!("{BASE}{path}"), "{name}");
        assert_eq!(requests[0].body, body, "{name}");
        assert_eq!(
            requests[0].authorization.as_deref(),
            Some("Bearer zephyr-test-bearer-token")
        );
    }
}

#[tokio::test]
async fn list_test_cases_and_steps_return_the_values_array() {
    let transport = FixtureTransport::json([
        json!({"startAt":0,"maxResults":10,"isLast":true,"values":[{"key":"PRJ-T1"}]}),
        json!({"startAt":0,"maxResults":10,"isLast":true,"values":[{"inline":{"description":"Open"}}]}),
        json!({"total":0}),
    ]);
    assert_eq!(
        run(&transport, "list_test_cases", json!({"project_key":"PRJ"}))
            .await
            .expect("cases"),
        json!([{"key":"PRJ-T1"}])
    );
    assert_eq!(
        run(
            &transport,
            "get_test_case_test_steps",
            json!({"test_case_key":"PRJ-T1","max_results":10})
        )
        .await
        .expect("steps"),
        json!([{"inline":{"description":"Open"}}])
    );
    assert_eq!(
        transport.requests()[1].url,
        format!("{BASE}/testcases/PRJ-T1/teststeps?maxResults=10")
    );
    // The SDK's `['values']` raises on a page without values.
    let error = run(&transport, "list_test_cases", json!({}))
        .await
        .expect_err("no values");
    assert_eq!(error.code, "tool.execution.internal");
}

#[tokio::test]
async fn issue_links_require_the_numeric_issue_id_before_any_request() {
    let transport = FixtureTransport::new([]);
    let by_key = run(
        &transport,
        "create_test_case_issue_link",
        json!({"test_case_key":"PRJ-T1","json":"{\"issueKey\":\"JIRA-7\"}"}),
    )
    .await
    .expect("guidance text");
    assert_eq!(
        by_key,
        json!(
            "Zephyr Essential API requires 'issueId' (numeric Jira issue ID), not 'issueKey'. You provided issueKey='JIRA-7'. To find the issueId, Jira toolkit can be used or raw Jira API: GET /rest/api/2/issue/JIRA-7 and look for the 'id' field in the response."
        )
    );
    let missing = run(
        &transport,
        "create_test_execution_issue_link",
        json!({"test_execution_id_or_key":"PRJ-E1","json":"{}"}),
    )
    .await
    .expect("guidance text");
    assert_eq!(
        missing,
        json!(
            "Missing required field 'issueId' in JSON payload for test execution issue link. Example: {\"issueId\": 10100}"
        )
    );
    assert!(transport.requests().is_empty());
}

#[tokio::test]
async fn find_folder_by_name_ignores_case_and_walks_pages() {
    let transport = FixtureTransport::json([
        folders_page(&json!([{"id":1,"name":"Alpha"}]), false),
        folders_page(&json!([{"id":2,"name":"Regression"}]), true),
        folders_page(&json!([{"id":1,"name":"Alpha"}]), true),
    ]);
    let found = run(
        &transport,
        "find_folder_by_name",
        json!({"name":"REGRESSION","project_key":"PRJ"}),
    )
    .await
    .expect("found");
    assert_eq!(found, json!({"id":2,"name":"Regression"}));
    let missing = run(&transport, "find_folder_by_name", json!({"name":"Nope"}))
        .await
        .expect("missing");
    assert_eq!(missing, Value::Null);
    assert_eq!(
        transport.urls(),
        vec![
            format!("GET {BASE}/folders?projectKey=PRJ&maxResults=100&startAt=0"),
            format!("GET {BASE}/folders?projectKey=PRJ&maxResults=100&startAt=1"),
            format!("GET {BASE}/folders?maxResults=100&startAt=0"),
        ]
    );
}

#[tokio::test]
async fn create_folder_resolves_parent_name_in_the_payload_project() {
    let transport = FixtureTransport::new([
        Ok(ZephyrRestResponse::json(
            StatusCode::OK,
            &folders_page(&json!([{"id":77,"name":"Parent"}]), true),
        )),
        Ok(ZephyrRestResponse::json(
            StatusCode::CREATED,
            &json!({"id":78,"self":"https://api/folders/78"}),
        )),
    ]);
    let created = run(
        &transport,
        "create_folder",
        json!({"json":"{\"parentName\":\"parent\",\"name\":\"Child\",\"projectKey\":\"PRJ\",\"folderType\":\"TEST_CASE\"}"}),
    )
    .await
    .expect("created");
    assert_eq!(created["id"], 78);
    let requests = transport.requests();
    assert_eq!(
        requests[0].url,
        format!("{BASE}/folders?projectKey=PRJ&folderType=TEST_CASE&maxResults=100&startAt=0")
    );
    assert_eq!(
        requests[1].body,
        Some(
            json!({"parentName":"parent","parentId":77,"name":"Child","projectKey":"PRJ","folderType":"TEST_CASE"})
        )
    );

    let transport = FixtureTransport::json([folders_page(&json!([]), true)]);
    let missing = run(
        &transport,
        "create_folder",
        json!({"json":"{\"parentName\":\"Ghost\",\"name\":\"Child\",\"projectKey\":\"PRJ\"}"}),
    )
    .await
    .expect("not-found text");
    assert_eq!(missing, json!("Parent folder with name 'Ghost' not found."));
    assert_eq!(transport.requests().len(), 1);
}

#[tokio::test]
async fn text_and_empty_replies_and_bad_arguments() {
    let transport = FixtureTransport::new([
        Ok(ZephyrRestResponse::fixture(
            StatusCode::NO_CONTENT,
            None,
            b"",
        )),
        Ok(ZephyrRestResponse::fixture(
            StatusCode::OK,
            Some("text/plain"),
            b"OK",
        )),
    ]);
    assert_eq!(
        run(&transport, "delete_link", json!({"link_id":"9"}))
            .await
            .expect("deleted"),
        json!("")
    );
    assert_eq!(
        run(&transport, "healthcheck", json!({}))
            .await
            .expect("health"),
        json!("OK")
    );
    for (name, arguments) in [
        ("create_test_case", json!({"json":"{oops"})),
        ("get_test_case", json!({"test_case_key":".."})),
        ("get_test_case", json!({"test_case_key":"PRJ-T1","extra":1})),
        (
            "get_test_case_version",
            json!({"test_case_key":"PRJ-T1","version":"three"}),
        ),
    ] {
        let error = run(&transport, name, arguments)
            .await
            .expect_err("bad arguments");
        assert_eq!(error.code, "tool.execution.invalid_input", "{name}");
    }
    assert_eq!(transport.requests().len(), 2);
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let transport = FixtureTransport::new([]);
    let names = super::sdk_conformance::sdk_tool_names("zephyr_essential");
    let names = names.iter().map(String::as_str).collect::<Vec<_>>();
    let tools = tools(&transport, &names).await;
    super::sdk_conformance::assert_sdk_conformance("zephyr_essential", &tools);
}
