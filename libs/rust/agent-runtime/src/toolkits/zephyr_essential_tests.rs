//! Zephyr Essential family: configuration, every route, the SDK's special
//! cases and the SDK schema gate, over fixtures shaped like the Zephyr Scale
//! Cloud v2 API.

use std::sync::Arc;

use adk_core::{Tool, Toolset};
use reqwest::{Method, StatusCode};
use serde_json::{Value, json};

use super::attachment_test_support::FakePlatform;
use super::families::zephyr_essential::client::ZephyrEssentialClient;
use super::families::zephyr_essential::config::{DEFAULT_BASE_URL, ZephyrEssentialToolkitConfig};
use super::families::zephyr_essential::tools::{test_build_with_client, test_catalog};
use super::families::zephyr_rest::client::{
    ZephyrRestError, ZephyrRestErrorCode, ZephyrRestResponse,
};
use super::families::zephyr_rest::config::ZephyrRestConfigErrorCode;
use super::families::zephyr_rest::tools::ZephyrRestToolsetErrorCode;
use super::zephyr_rest_tests::{
    FixtureTransport, TOKEN, policy, readonly, rest_client, tool_context,
};

const BASE: &str = "https://api.zephyrscale.example.test/v2";

fn client(transport: &Arc<FixtureTransport>) -> Arc<ZephyrEssentialClient> {
    Arc::new(ZephyrEssentialClient::new(rest_client(BASE, transport)))
}

/// A client that lends the artifact store (the uploads and the BDD download).
fn host_client(
    transport: &Arc<FixtureTransport>,
    platform: &Arc<FakePlatform>,
) -> Arc<ZephyrEssentialClient> {
    Arc::new(
        ZephyrEssentialClient::new(rest_client(BASE, transport)).with_attachments(platform.host()),
    )
}

async fn host_tools(
    transport: &Arc<FixtureTransport>,
    platform: &Arc<FakePlatform>,
    selected: &[&str],
) -> Vec<Arc<dyn Tool>> {
    test_build_with_client(
        "Essential QA",
        selected,
        &policy(&[]),
        &host_client(transport, platform),
    )
    .expect("Zephyr Essential toolset")
    .tools(readonly())
    .await
    .expect("Zephyr Essential tools")
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
async fn catalog_serves_forty_five_operations_with_truthful_groups() {
    let catalog = test_catalog();
    assert_eq!(catalog.len(), 45);
    // `retrieve_bdd_test_cases` is a GET but stores an artifact: not a read.
    assert_eq!(catalog.iter().filter(|(_, read)| *read).count(), 24);
    assert!(
        catalog
            .iter()
            .any(|(name, read)| *name == "retrieve_bdd_test_cases" && !*read)
    );
    assert!(catalog.iter().all(|(name, _)| *name != "index_data"));
    // Without the artifact store the four file tools are omitted.
    let transport = FixtureTransport::new([]);
    let without = tools(&transport, &[]).await;
    assert_eq!(without.len(), 41);
    for omitted in [
        "create_custom_executions",
        "create_cucumber_executions",
        "create_junit_executions",
        "retrieve_bdd_test_cases",
    ] {
        assert!(without.iter().all(|tool| tool.name() != omitted));
    }
    let platform = FakePlatform::new();
    let all = host_tools(&transport, &platform, &[]).await;
    assert_eq!(all.len(), 45);
    for tool in &all {
        assert!(tool.description().starts_with("Toolkit: Essential QA\n"));
        assert!(tool.description().chars().count() <= 1_000);
        assert_eq!(tool.is_read_only(), tool.is_concurrency_safe());
        if !tool.is_read_only() {
            assert!(
                tool.description()
                    .contains("read the current state before retrying"),
                "{}",
                tool.name()
            );
        }
    }
    let Err(error) = test_build_with_client(
        "Essential QA",
        &["create_junit_executions", "index_data"],
        &policy(&[]),
        &client(&transport),
    ) else {
        panic!("a selection of only host-bound tools must be unsupported without a host");
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
    let platform = FakePlatform::new();
    let names = super::sdk_conformance::sdk_tool_names("zephyr_essential");
    let names = names.iter().map(String::as_str).collect::<Vec<_>>();
    let tools = host_tools(&transport, &platform, &names).await;
    super::sdk_conformance::assert_sdk_conformance("zephyr_essential", &tools);
}

#[allow(clippy::unnecessary_wraps, reason = "matches the transport queue type")]
fn upload_reply() -> Result<ZephyrRestResponse, ZephyrRestError> {
    Ok(ZephyrRestResponse::json(
        StatusCode::OK,
        &json!({"testCycle":{"id":"1","key":"PRJ-R1"}}),
    ))
}

const JUNIT: &[u8] = b"<testsuites><testsuite name=\"s\"/></testsuites>";

#[tokio::test]
async fn result_uploads_send_one_multipart_file_read_from_the_artifact() {
    for (name, segment, media) in [
        ("create_custom_executions", "custom", "application/json"),
        ("create_cucumber_executions", "cucumber", "application/json"),
        ("create_junit_executions", "junit", "application/xml"),
    ] {
        let transport = FixtureTransport::new([upload_reply()]);
        let platform = FakePlatform::new();
        platform.put("results", "run 1.xml", JUNIT);
        let tools = host_tools(&transport, &platform, &[name]).await;
        let reply = tools[0]
            .execute(
                tool_context(),
                json!({"project_key":"PRJ","files":"/results/run 1.xml","auto_create_test_cases":true}),
            )
            .await
            .expect("uploaded");
        assert_eq!(reply, json!({"testCycle":{"id":"1","key":"PRJ-R1"}}));
        let sent = transport.requests();
        assert_eq!(sent.len(), 1);
        assert_eq!(sent[0].method, Method::POST);
        assert_eq!(
            sent[0].url,
            format!(
                "{BASE}/automations/executions/{segment}?projectKey=PRJ&autoCreateTestCases=true"
            )
        );
        assert_eq!(
            sent[0].authorization.as_deref(),
            Some("Bearer zephyr-test-bearer-token")
        );
        let content_type = sent[0].content_type.clone().expect("content type");
        assert!(content_type.starts_with("multipart/form-data; boundary="));
        let body = String::from_utf8(sent[0].raw_body.clone().expect("body")).expect("text body");
        assert!(body.contains("name=\"file\"; filename=\"run 1.xml\""));
        assert!(body.contains(&format!("Content-Type: {media}\r\n\r\n")));
        assert!(body.contains(std::str::from_utf8(JUNIT).expect("utf8")));
        assert!(!tools[0].is_read_only());
        assert!(!tools[0].is_concurrency_safe());
    }
    // The default for auto_create_test_cases is false.
    let transport = FixtureTransport::new([upload_reply()]);
    let platform = FakePlatform::new();
    platform.put("results", "r.json", b"{}");
    let tools = host_tools(&transport, &platform, &["create_custom_executions"]).await;
    tools[0]
        .execute(
            tool_context(),
            json!({"project_key":"PRJ","files":"/results/r.json"}),
        )
        .await
        .expect("uploaded");
    assert!(
        transport.requests()[0]
            .url
            .ends_with("?projectKey=PRJ&autoCreateTestCases=false")
    );
}

#[allow(clippy::too_many_lines, reason = "one corpus of failure causes")]
#[tokio::test]
async fn result_upload_failures_name_their_cause_and_never_retry() {
    // Host failures answer with their sentence and send nothing.
    let transport = FixtureTransport::new([]);
    let platform = FakePlatform::new();
    let tools = host_tools(&transport, &platform, &["create_junit_executions"]).await;
    for (arguments, words) in [
        (
            json!({"project_key":"PRJ","files":"/results/missing.xml"}),
            "was not found",
        ),
        (
            json!({"project_key":"PRJ","files":"no-bucket"}),
            "not valid",
        ),
    ] {
        let answer = tools[0]
            .execute(tool_context(), arguments.clone())
            .await
            .expect("a sentence, not an error");
        let text = answer.as_str().expect("text");
        assert!(text.contains(words), "{arguments}: {text}");
        assert!(text.contains("nothing was sent") || text.contains("not valid"));
    }
    // Arguments the schema rejects are errors.
    for arguments in [
        json!({"project_key":"PRJ","files":"/results/x.xml","auto_create_test_cases":"yes"}),
        json!({"project_key":"PRJ"}),
        json!({"project_key":"PRJ","files":"/r/x.xml","extra":1}),
    ] {
        let error = tools[0]
            .execute(tool_context(), arguments.clone())
            .await
            .expect_err("refused");
        assert_eq!(
            error.category,
            adk_core::ErrorCategory::InvalidInput,
            "{arguments}"
        );
    }
    platform.put("results", "huge.xml", &[]);
    let answer = tools[0]
        .execute(
            tool_context(),
            json!({"project_key":"PRJ","files":"/results/huge.xml"}),
        )
        .await
        .expect("sentence");
    assert!(answer.as_str().expect("text").contains("empty"));
    platform.fail_reads(super::families::office_host::OfficeFailure::AccessRefused);
    platform.put("results", "r.xml", JUNIT);
    let answer = tools[0]
        .execute(
            tool_context(),
            json!({"project_key":"PRJ","files":"/results/r.xml"}),
        )
        .await
        .expect("sentence");
    assert!(answer.as_str().expect("text").contains("refused access"));
    assert!(transport.requests().is_empty());

    // Provider failures after a send: one request each, 5xx is unknown.
    for (status, category) in [
        (
            StatusCode::UNAUTHORIZED,
            adk_core::ErrorCategory::Unauthorized,
        ),
        (StatusCode::FORBIDDEN, adk_core::ErrorCategory::Forbidden),
        (StatusCode::NOT_FOUND, adk_core::ErrorCategory::NotFound),
        (StatusCode::CONFLICT, adk_core::ErrorCategory::InvalidInput),
        (
            StatusCode::BAD_REQUEST,
            adk_core::ErrorCategory::InvalidInput,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            adk_core::ErrorCategory::Internal,
        ),
        (StatusCode::BAD_GATEWAY, adk_core::ErrorCategory::Internal),
    ] {
        let transport =
            FixtureTransport::new([Ok(ZephyrRestResponse::fixture(status, None, b"x"))]);
        let platform = FakePlatform::new();
        platform.put("results", "r.xml", JUNIT);
        let tools = host_tools(&transport, &platform, &["create_junit_executions"]).await;
        let outcome = tools[0]
            .execute(
                tool_context(),
                json!({"project_key":"PRJ","files":"/results/r.xml"}),
            )
            .await;
        if category == adk_core::ErrorCategory::Internal {
            // An unknown outcome is an answer: an Err message would be replaced.
            let text = outcome.expect("an unknown outcome is an answer");
            assert!(
                text.as_str().expect("text").contains("may have applied"),
                "{status}"
            );
        } else {
            let error = outcome.expect_err("provider failure");
            assert_eq!(error.category, category, "{status}");
            assert!(!error.is_retryable(), "{status}");
        }
        assert_eq!(transport.requests().len(), 1);
    }
    // A lost answer is an unknown outcome.
    let transport = FixtureTransport::new([Err(ZephyrRestError::fixture(
        ZephyrRestErrorCode::UnknownOutcome,
        false,
    ))]);
    let platform = FakePlatform::new();
    platform.put("results", "r.xml", JUNIT);
    let tools = host_tools(&transport, &platform, &["create_junit_executions"]).await;
    let client = host_client(&transport, &platform);
    let error = client
        .upload(
            &["automations", "executions", "junit"],
            &[],
            &super::families::zephyr_rest::client::MultipartFile {
                field: "file",
                file_name: "r.xml",
                media_type: "application/xml",
                bytes: JUNIT,
            },
        )
        .await
        .expect_err("lost");
    assert_eq!(error.code(), ZephyrRestErrorCode::UnknownOutcome);
    assert!(
        error
            .into_adk(&super::families::zephyr_essential::client::FAMILY)
            .message
            .contains("may have applied")
    );
    drop(tools);
}

#[tokio::test]
async fn upload_refuses_a_file_over_the_cap_before_sending() {
    let transport = FixtureTransport::new([]);
    let platform = FakePlatform::new();
    // The fake enforces the cap it is given: one byte over 50 MiB.
    platform.put("results", "big.xml", &vec![b'a'; 50 * 1024 * 1024 + 1]);
    let tools = host_tools(&transport, &platform, &["create_junit_executions"]).await;
    let answer = tools[0]
        .execute(
            tool_context(),
            json!({"project_key":"PRJ","files":"/results/big.xml"}),
        )
        .await
        .expect("a sentence");
    assert!(
        answer
            .as_str()
            .expect("text")
            .contains("larger than the 50 MiB this tool accepts; nothing was sent")
    );
    assert!(transport.requests().is_empty());
}

#[tokio::test]
async fn bdd_archive_is_stored_as_a_new_artifact() {
    let zip = b"PK\x03\x04rest-of-archive".to_vec();
    let transport = FixtureTransport::new([
        Ok(ZephyrRestResponse::fixture(
            StatusCode::OK,
            Some("application/zip"),
            &zip,
        )),
        Ok(ZephyrRestResponse::fixture(
            StatusCode::OK,
            Some("application/zip"),
            &zip,
        )),
    ]);
    let platform = FakePlatform::new();
    let tools = host_tools(&transport, &platform, &["retrieve_bdd_test_cases"]).await;
    assert!(!tools[0].is_read_only());
    assert!(!tools[0].is_concurrency_safe());
    let first = tools[0]
        .execute(tool_context(), json!({"project_key":"PR/J"}))
        .await
        .expect("stored");
    assert_eq!(
        first,
        json!(
            "BDD test cases for project PR/J retrieved as a ZIP archive (19 bytes) and stored as artifact /zephyr-bdd/zephyr-bdd-PR_J.zip"
        )
    );
    let second = tools[0]
        .execute(
            tool_context(),
            json!({"project_key":"PR/J","bucket":"mine"}),
        )
        .await
        .expect("stored");
    assert!(
        second
            .as_str()
            .expect("text")
            .ends_with("/mine/zephyr-bdd-PR_J.zip")
    );
    let sent = transport.requests();
    assert_eq!(
        sent[0].url,
        format!("{BASE}/automations/testcases?projectKey=PR%2FJ")
    );
    assert_eq!(sent[0].accept.as_deref(), Some("application/zip"));
    assert_eq!(
        platform.get("zephyr-bdd", "zephyr-bdd-PR_J.zip"),
        Some(zip.clone())
    );
    assert!(platform.writes.lock().expect("writes").iter().all(|w| !w.3));

    // A taken name is never replaced: the next free suffix is used.
    let transport = FixtureTransport::new([Ok(ZephyrRestResponse::fixture(
        StatusCode::OK,
        Some("application/zip"),
        &zip,
    ))]);
    let platform = FakePlatform::new();
    platform.put("zephyr-bdd", "zephyr-bdd-PRJ.zip", b"old");
    let tools = host_tools(&transport, &platform, &["retrieve_bdd_test_cases"]).await;
    let reply = tools[0]
        .execute(tool_context(), json!({"project_key":"PRJ"}))
        .await
        .expect("stored under a free name");
    assert!(
        reply
            .as_str()
            .expect("text")
            .ends_with("/zephyr-bdd/zephyr-bdd-PRJ-2.zip")
    );
    assert_eq!(
        platform.get("zephyr-bdd", "zephyr-bdd-PRJ.zip"),
        Some(b"old".to_vec())
    );
}

#[tokio::test]
async fn bdd_archive_failures_are_distinct() {
    for (response, category) in [
        (
            ZephyrRestResponse::fixture(StatusCode::UNAUTHORIZED, None, b""),
            adk_core::ErrorCategory::Unauthorized,
        ),
        (
            ZephyrRestResponse::fixture(StatusCode::NOT_FOUND, None, b""),
            adk_core::ErrorCategory::NotFound,
        ),
        (
            ZephyrRestResponse::fixture(StatusCode::TOO_MANY_REQUESTS, None, b""),
            adk_core::ErrorCategory::RateLimited,
        ),
        (
            ZephyrRestResponse::fixture(StatusCode::OK, Some("application/zip"), b"not a zip"),
            adk_core::ErrorCategory::Internal,
        ),
    ] {
        let transport = FixtureTransport::new([Ok(response)]);
        let platform = FakePlatform::new();
        let tools = host_tools(&transport, &platform, &["retrieve_bdd_test_cases"]).await;
        let error = tools[0]
            .execute(tool_context(), json!({"project_key":"PRJ"}))
            .await
            .expect_err("failure");
        assert_eq!(error.category, category);
        assert!(platform.writes.lock().expect("writes").is_empty());
    }
    // An empty archive stores nothing.
    let transport = FixtureTransport::new([Ok(ZephyrRestResponse::fixture(
        StatusCode::OK,
        Some("application/zip"),
        b"",
    ))]);
    let platform = FakePlatform::new();
    let tools = host_tools(&transport, &platform, &["retrieve_bdd_test_cases"]).await;
    let reply = tools[0]
        .execute(tool_context(), json!({"project_key":"PRJ"}))
        .await
        .expect("empty");
    assert!(reply.as_str().expect("text").contains("nothing was stored"));
    assert!(platform.writes.lock().expect("writes").is_empty());
    // A response over the cap is refused (the fake transport ignores the
    // transport-level cap, so the client re-check is what fires).
    let transport = FixtureTransport::new([Ok(ZephyrRestResponse::fixture(
        StatusCode::OK,
        Some("application/zip"),
        &vec![0_u8; 50 * 1024 * 1024 + 1],
    ))]);
    let platform = FakePlatform::new();
    let tools = host_tools(&transport, &platform, &["retrieve_bdd_test_cases"]).await;
    let error = tools[0]
        .execute(tool_context(), json!({"project_key":"PRJ"}))
        .await
        .expect_err("too large");
    assert_eq!(error.category, adk_core::ErrorCategory::InvalidInput);
    // Write failure: the lost answer is reported as may-or-may-not.
    let zip = b"PK\x03\x04x".to_vec();
    let transport = FixtureTransport::new([Ok(ZephyrRestResponse::fixture(
        StatusCode::OK,
        Some("application/zip"),
        &zip,
    ))]);
    let platform = FakePlatform::new();
    platform.fail_writes(super::families::office_host::OfficeFailure::Unreachable);
    let tools = host_tools(&transport, &platform, &["retrieve_bdd_test_cases"]).await;
    let answer = tools[0]
        .execute(tool_context(), json!({"project_key":"PRJ"}))
        .await
        .expect("sentence");
    assert!(
        answer
            .as_str()
            .expect("text")
            .contains("may or may not have been stored")
    );
}
