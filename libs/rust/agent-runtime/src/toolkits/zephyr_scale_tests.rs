//! Zephyr Scale family: configuration, pagination, the SDK's text outputs,
//! folder-tree tools, client-side search and the SDK schema gate, over
//! fixtures shaped like the Zephyr Scale Cloud v2 API.

use std::sync::Arc;

use adk_core::Toolset;
use reqwest::{Method, StatusCode};
use serde_json::{Value, json};

use super::families::zephyr_rest::client::ZephyrRestResponse;
use super::families::zephyr_rest::config::ZephyrRestConfigErrorCode;
use super::families::zephyr_scale::client::ZephyrScaleClient;
use super::families::zephyr_scale::config::{CLOUD_API, ZephyrScaleToolkitConfig};
use super::families::zephyr_scale::render::{json_dumps_indent, py_repr};
use super::families::zephyr_scale::tools::{test_build_with_client, test_catalog};
use super::zephyr_rest_tests::{
    FixtureTransport, TOKEN, policy, readonly, rest_client, tool_context,
};

fn client(transport: &Arc<FixtureTransport>) -> Arc<ZephyrScaleClient> {
    Arc::new(ZephyrScaleClient::new(rest_client(CLOUD_API, transport)))
}

async fn run(
    transport: &Arc<FixtureTransport>,
    name: &str,
    arguments: Value,
) -> adk_core::Result<Value> {
    let tools = test_build_with_client("Scale QA", &[name], &policy(&[]), &client(transport))
        .expect("Zephyr Scale toolset")
        .tools(readonly())
        .await
        .expect("Zephyr Scale tools");
    assert_eq!(tools.len(), 1, "{name} is served");
    tools[0].execute(tool_context(), arguments).await
}

fn text(value: &Value) -> &str {
    value.as_str().expect("text result")
}

/// A Cloud API page; `next` carries the following page's query.
fn page(values: &Value, is_last: bool, next: Option<&str>) -> Value {
    json!({
        "next": next.map(|query| format!("{CLOUD_API}/testcases?{query}")),
        "startAt": 0,
        "maxResults": 10,
        "isLast": is_last,
        "values": values
    })
}

fn last(values: &Value) -> Value {
    page(values, true, None)
}

fn test_case(id: i64, key: &str, name: &str) -> Value {
    json!({
        "id": id, "key": key, "name": name,
        "project": {"id": 10005}, "precondition": null,
        "priority": {"id": 1}, "status": {"id": 2}, "owner": null
    })
}

#[test]
fn configuration_uses_only_the_token_against_the_cloud_api() {
    let settings = json!({"zephyr_configuration":{
        "base_url":"https://jira.example.test","token":TOKEN,"username":"u","password":"p"
    },"max_results":100});
    let config = ZephyrScaleToolkitConfig::parse(settings.as_object().expect("settings"))
        .expect("token configuration");
    assert!(config.selected_tools().is_empty());
    for invalid in [
        json!({"zephyr_configuration":{"base_url":"https://jira.example.test","username":"u","password":"p"}}),
        json!({"zephyr_configuration":{"cookies":"a=b"}}),
        json!({"zephyr_essential_configuration":{"token":TOKEN}}),
    ] {
        let Err(error) = ZephyrScaleToolkitConfig::parse(invalid.as_object().expect("object"))
        else {
            panic!("Zephyr Scale configuration without a token must fail");
        };
        assert_eq!(
            error.code(),
            ZephyrRestConfigErrorCode::InvalidConfiguration
        );
    }
}

#[test]
fn python_renderings_match_the_sdk_text() {
    assert_eq!(
        py_repr(&json!({"b":[1, true, null], "a":"it's", "c":"say \"hi\"\n"})),
        r#"{'a': "it's", 'b': [1, True, None], 'c': 'say "hi"\n'}"#
    );
    assert_eq!(
        json_dumps_indent(&[
            vec![
                ("key".to_owned(), json!("P-T1")),
                ("name".to_owned(), json!("Café ✓"))
            ],
            vec![],
        ]),
        "[\n  {\n    \"key\": \"P-T1\",\n    \"name\": \"Caf\\u00e9 \\u2713\"\n  },\n  {}\n]"
    );
    assert_eq!(json_dumps_indent(&[]), "[]");
}

#[tokio::test]
async fn catalog_serves_the_twenty_business_tools() {
    let catalog = test_catalog();
    assert_eq!(catalog.len(), 20);
    let effects = catalog
        .iter()
        .filter(|(_, read)| !read)
        .map(|(name, _)| *name)
        .collect::<Vec<_>>();
    assert_eq!(
        effects,
        vec![
            "create_test_case",
            "create_test_cases",
            "add_test_steps",
            "update_test_steps",
            "update_test_case",
            "create_issue_links",
            "create_web_links",
            "create_test_script",
        ]
    );
    let transport = FixtureTransport::new([]);
    let tools = test_build_with_client("Scale QA", &[], &policy(&[]), &client(&transport))
        .expect("toolset")
        .tools(readonly())
        .await
        .expect("tools");
    assert_eq!(tools.len(), 20);
    assert!(
        tools
            .iter()
            .all(|tool| tool.description().starts_with("Toolkit: Scale QA\n"))
    );
}

#[tokio::test]
async fn get_tests_follows_next_pages_and_renders_the_parsed_rows() {
    let transport = FixtureTransport::json([
        page(
            &json!([test_case(1, "P-T1", "Login")]),
            false,
            Some("maxResults=10&startAt=10&projectKey=P"),
        ),
        last(&json!([{"id":2,"key":"P-T2","name":"Can't log out","owner":{"accountId":"abc"}}])),
    ]);
    let output = run(
        &transport,
        "get_tests",
        json!({"project_key":"P","folder_id":"7"}),
    )
    .await
    .expect("tests");
    assert_eq!(
        text(&output),
        "Extracted tests: [['Test ID: 1', 'Key: P-T1', 'Name: Login', 'Project ID: 10005', 'Precondition: None', 'Priority ID: 1', 'Status ID: 2', 'Owner Account ID: None'], ['Test ID: 2', 'Key: P-T2', \"Name: Can't log out\", 'Project ID: None', 'Precondition: None', 'Priority ID: None', 'Status ID: None', 'Owner Account ID: abc']]"
    );
    assert_eq!(
        transport.urls(),
        vec![
            format!("GET {CLOUD_API}/testcases?projectKey=P&folderId=7&maxResults=10"),
            format!("GET {CLOUD_API}/testcases?projectKey=P&folderId=7&maxResults=10&startAt=10"),
        ]
    );
}

#[tokio::test]
async fn simple_reads_render_the_sdk_sentences() {
    let transport = FixtureTransport::json([
        json!({"id":1,"key":"P-T1","labels":["Smoke"]}),
        last(&json!([{"inline":{"description":"Open"}},{"inline":{"description":"Close"}}])),
        json!({"self":"s","issues":[],"webLinks":[]}),
        last(&json!([{"id":3},{"id":2}])),
        json!({"id":2,"name":"v2"}),
        json!({"id":9,"type":"plain","text":"Do it"}),
        last(&json!([{"id":5,"name":"Root"}])),
    ]);
    let outputs = [
        run(&transport, "get_test", json!({"test_case_key":"P-T1"})).await,
        run(
            &transport,
            "get_test_steps",
            json!({"test_case_key":"P-T1"}),
        )
        .await,
        run(&transport, "get_links", json!({"test_case_key":"P-T1"})).await,
        run(&transport, "get_versions", json!({"test_case_key":"P-T1"})).await,
        run(
            &transport,
            "get_version",
            json!({"test_case_key":"P-T1","version":"2"}),
        )
        .await,
        run(
            &transport,
            "get_test_script",
            json!({"test_case_key":"P-T1"}),
        )
        .await,
        run(&transport, "get_folders", json!({"projectKey":"P"})).await,
    ]
    .map(|output| output.expect("read").as_str().expect("text").to_owned());
    assert_eq!(
        outputs,
        [
            "Extracted tests: {'id': 1, 'key': 'P-T1', 'labels': ['Smoke']}".to_owned(),
            "Extracted test steps: {'inline': {'description': 'Open'}}\n{'inline': {'description': 'Close'}}".to_owned(),
            "Links for test case `P-T1`: {'issues': [], 'self': 's', 'webLinks': []}".to_owned(),
            "Versions for test case `P-T1`: {'id': 3}\n{'id': 2}".to_owned(),
            "Version 2 of test case `P-T1`: {'id': 2, 'name': 'v2'}".to_owned(),
            "Test script for test case `P-T1`: {'id': 9, 'text': 'Do it', 'type': 'plain'}".to_owned(),
            "Extracted folders: [{'id': 5, 'name': 'Root'}]".to_owned(),
        ]
    );
    assert_eq!(
        transport.urls(),
        vec![
            format!("GET {CLOUD_API}/testcases/P-T1"),
            format!("GET {CLOUD_API}/testcases/P-T1/teststeps"),
            format!("GET {CLOUD_API}/testcases/P-T1/links"),
            format!("GET {CLOUD_API}/testcases/P-T1/versions?maxResults=10&startAt=0"),
            format!("GET {CLOUD_API}/testcases/P-T1/versions/2"),
            format!("GET {CLOUD_API}/testcases/P-T1/testscript"),
            format!("GET {CLOUD_API}/folders?maxResults=10&startAt=0&projectKey=P"),
        ]
    );
}

#[tokio::test]
async fn create_test_case_merges_fields_and_appends_steps_to_the_created_key() {
    let transport = FixtureTransport::new([
        Ok(ZephyrRestResponse::json(
            StatusCode::CREATED,
            &json!({"id":11,"key":"P-T11","self":"https://api/testcases/P-T11"}),
        )),
        Ok(ZephyrRestResponse::json(
            StatusCode::CREATED,
            &json!({"id":101}),
        )),
    ]);
    let output = run(
        &transport,
        "create_test_case",
        json!({
            "project_key":"P","test_case_name":"Login",
            "additional_fields":"{\"objective\":\"Works\",\"labels\":[\"Smoke\"]}",
            "steps":"[{\"inline\":{\"description\":\"Open\"}}]"
        }),
    )
    .await
    .expect("created");
    assert_eq!(
        text(&output),
        "Test case with name `Login` was created: {'id': 11, 'key': 'P-T11', 'self': 'https://api/testcases/P-T11'}\nSteps for test case `P-T11` were added/updated: {'id': 101}"
    );
    let requests = transport.requests();
    assert_eq!(
        requests[0].body,
        Some(json!({"projectKey":"P","name":"Login","objective":"Works","labels":["Smoke"]}))
    );
    assert_eq!(
        requests[1].url,
        format!("{CLOUD_API}/testcases/P-T11/teststeps")
    );
    assert_eq!(
        requests[1].body,
        Some(json!({"mode":"APPEND","items":[{"inline":{"description":"Open"}}]}))
    );
}

#[tokio::test]
async fn create_test_cases_validates_first_and_reports_each_case() {
    let transport = FixtureTransport::new([]);
    let error = run(
        &transport,
        "create_test_cases",
        json!({"create_test_cases_data":"[{\"project_key\":\"P\",\"test_case_name\":\"A\"},{\"project_key\":\"P\"}]"}),
    )
    .await
    .expect_err("a malformed case refuses the batch");
    assert_eq!(error.code, "tool.execution.invalid_input");
    assert!(transport.requests().is_empty());

    let transport = FixtureTransport::new([
        Ok(ZephyrRestResponse::json(
            StatusCode::CREATED,
            &json!({"id":1,"key":"P-T1"}),
        )),
        Ok(ZephyrRestResponse::fixture(
            StatusCode::BAD_REQUEST,
            None,
            b"{}",
        )),
    ]);
    let output = run(
        &transport,
        "create_test_cases",
        json!({"create_test_cases_data":"[{\"project_key\":\"P\",\"test_case_name\":\"A\",\"additional_fields\":{},\"steps\":[]},{\"project_key\":\"P\",\"test_case_name\":\"B\",\"additional_fields\":\"{}\",\"steps\":null}]"}),
    )
    .await
    .expect("per-case results");
    assert_eq!(
        text(&output),
        "[\"Test case with name `A` was created: {'id': 1, 'key': 'P-T1'}\", 'Unable to create test case with name: B:\\nthe Zephyr Scale request is invalid']"
    );
    assert_eq!(transport.requests().len(), 2);
}

#[tokio::test]
async fn update_test_steps_overwrites_all_steps_after_validating_indexes() {
    let steps = json!([
        {"inline":{"description":"Open","testData":"","expectedResult":"Shown"},"testCase":null},
        {"inline":{"description":"Submit","testData":"x","expectedResult":"Saved"},"testCase":null}
    ]);
    let transport = FixtureTransport::new([
        Ok(ZephyrRestResponse::json(StatusCode::OK, &last(&steps))),
        Ok(ZephyrRestResponse::json(
            StatusCode::CREATED,
            &json!({"id":1}),
        )),
    ]);
    let output = run(
        &transport,
        "update_test_steps",
        json!({"test_case_key":"P-T1","steps_updates":"[{\"index\":1,\"expectedResult\":\"Persisted\"}]"}),
    )
    .await
    .expect("updated");
    assert_eq!(
        text(&output),
        "Test steps updated for test case `P-T1`: 1 step(s) modified"
    );
    let posted = transport.requests()[1]
        .body
        .clone()
        .expect("overwrite body");
    assert_eq!(posted["mode"], "OVERWRITE");
    assert_eq!(posted["items"][1]["inline"]["expectedResult"], "Persisted");
    assert_eq!(posted["items"][0], steps[0]);

    for (updates, message) in [
        (
            "[{\"index\":5}]",
            "Step index 5 is out of range. Valid range: 0-1",
        ),
        (
            "[{\"description\":\"x\"}]",
            "Each update must contain an 'index' field",
        ),
        ("{\"index\":0}", "Steps updates must be a JSON array"),
    ] {
        let transport = FixtureTransport::json([last(&steps)]);
        let output = run(
            &transport,
            "update_test_steps",
            json!({"test_case_key":"P-T1","steps_updates":updates}),
        )
        .await
        .expect("validation text");
        assert_eq!(text(&output), message);
        assert_eq!(
            transport.requests().len(),
            1,
            "no write after a refused update"
        );
    }
    let transport = FixtureTransport::json([last(&json!([]))]);
    let output = run(
        &transport,
        "update_test_steps",
        json!({"test_case_key":"P-T9","steps_updates":"[]"}),
    )
    .await
    .expect("no steps");
    assert_eq!(text(&output), "No test steps found for test case: P-T9");
}

#[tokio::test]
async fn effects_send_the_library_bodies() {
    let transport = FixtureTransport::new([
        Ok(ZephyrRestResponse::fixture(StatusCode::OK, None, b"")),
        Ok(ZephyrRestResponse::json(
            StatusCode::CREATED,
            &json!({"id":3}),
        )),
        Ok(ZephyrRestResponse::json(
            StatusCode::CREATED,
            &json!({"id":4}),
        )),
        Ok(ZephyrRestResponse::json(
            StatusCode::CREATED,
            &json!({"id":5}),
        )),
        Ok(ZephyrRestResponse::json(
            StatusCode::CREATED,
            &json!({"id":6}),
        )),
    ]);
    let updated = run(
        &transport,
        "update_test_case",
        json!({"test_case_key":"P-T1","test_case_id":1,"name":"N","project_id":10,"priority_id":2,"status_id":3,"additional_fields":"{\"objective\":\"O\"}"}),
    )
    .await
    .expect("update");
    assert_eq!(text(&updated), "Test case `P-T1` was updated: ");
    let link = run(
        &transport,
        "create_issue_links",
        json!({"test_case_key":"P-T1","issue_id":10100}),
    )
    .await
    .expect("issue link");
    assert_eq!(
        text(&link),
        "Issue link created for test case `P-T1` with issue ID `10100`: {'id': 3}"
    );
    let web = run(
        &transport,
        "create_web_links",
        json!({"test_case_key":"P-T1","url":"https://e.test","description":"Docs","additional_fields":"{\"type\":\"RELATED\"}"}),
    )
    .await
    .expect("web link");
    assert_eq!(
        text(&web),
        "Web link created for test case `P-T1` with URL `https://e.test` and link text `Docs`: {'id': 4}"
    );
    let script = run(
        &transport,
        "create_test_script",
        json!({"test_case_key":"P-T1","script_type":"plain","text":"Do"}),
    )
    .await
    .expect("script");
    assert_eq!(
        text(&script),
        "Test script created/updated for test case `P-T1`: {'id': 5}"
    );
    let steps = run(
        &transport,
        "add_test_steps",
        json!({"test_case_key":"P-T1","tc_mode":"OVERWRITE","items":"[{\"inline\":{\"description\":\"A\"}}]"}),
    )
    .await
    .expect("steps");
    assert_eq!(
        text(&steps),
        "Steps for test case `P-T1` were added/updated: {'id': 6}"
    );
    let requests = transport.requests();
    assert_eq!(requests[0].method, Method::PUT);
    assert_eq!(
        requests[0].body,
        Some(
            json!({"id":1,"key":"P-T1","name":"N","project":{"id":10},"priority":{"id":2},"status":{"id":3},"objective":"O"})
        )
    );
    assert_eq!(requests[1].body, Some(json!({"issueId":10100})));
    assert_eq!(
        requests[2].body,
        Some(json!({"url":"https://e.test","type":"RELATED","description":"Docs"}))
    );
    assert_eq!(requests[3].body, Some(json!({"type":"plain","text":"Do"})));
    assert_eq!(
        requests[4].body,
        Some(json!({"mode":"OVERWRITE","items":[{"inline":{"description":"A"}}]}))
    );
}

fn folder_tree() -> Value {
    json!([
        {"id":1,"name":"Root","parentId":null},
        {"id":2,"name":"Login","parentId":1},
        {"id":3,"name":"Edge","parentId":2},
        {"id":4,"name":"Other","parentId":null}
    ])
}

#[tokio::test]
async fn folder_path_and_name_tools_walk_the_tree() {
    let transport = FixtureTransport::json([
        last(&folder_tree()),
        last(&json!([test_case(1, "P-T1", "In Login")])),
        last(&json!([test_case(2, "P-T2", "In Edge")])),
    ]);
    let output = run(
        &transport,
        "get_tests_by_folder_path",
        json!({"project_key":"P","folder_path":"Root/Login"}),
    )
    .await
    .expect("by path");
    assert!(
        text(&output)
            .starts_with("Extracted 2 tests from folder path 'Root/Login': [['Test ID: 1'")
    );
    assert_eq!(
        transport.urls(),
        vec![
            format!("GET {CLOUD_API}/folders?maxResults=1000&projectKey=P&folderType=TEST_CASE"),
            format!("GET {CLOUD_API}/testcases?projectKey=P&maxResults=100&startAt=0&folderId=2"),
            format!("GET {CLOUD_API}/testcases?projectKey=P&maxResults=100&startAt=0&folderId=3"),
        ]
    );

    let transport = FixtureTransport::json([last(&folder_tree())]);
    let missing = run(
        &transport,
        "get_tests_by_folder_path",
        json!({"project_key":"P","folder_path":"Root/Nope"}),
    )
    .await
    .expect("missing path");
    assert_eq!(text(&missing), "No folder found with path: Root/Nope");

    let transport = FixtureTransport::new([
        Ok(ZephyrRestResponse::json(
            StatusCode::OK,
            &last(&folder_tree()),
        )),
        Ok(ZephyrRestResponse::fixture(
            StatusCode::FORBIDDEN,
            None,
            b"",
        )),
        Ok(ZephyrRestResponse::json(
            StatusCode::OK,
            &last(&json!([test_case(3, "P-T3", "Edge case")])),
        )),
    ]);
    let by_name = run(
        &transport,
        "get_tests_by_folder_name",
        json!({"project_key":"P","folder_name":"login"}),
    )
    .await
    .expect("by name");
    // A folder that cannot be read is skipped, as in the SDK.
    assert!(text(&by_name).starts_with("Extracted 1 tests from 2 folders matching 'login': "));
    let transport = FixtureTransport::json([last(&folder_tree())]);
    let none = run(
        &transport,
        "get_tests_by_folder_name",
        json!({"project_key":"P","folder_name":"login","exact_match":true}),
    )
    .await
    .expect("no exact match");
    assert_eq!(text(&none), "No folders found matching name: login");
}

#[tokio::test]
async fn get_tests_recursive_reads_the_folder_then_its_descendants() {
    let transport = FixtureTransport::json([
        last(&json!([test_case(1, "P-T1", "Root case")])),
        last(&folder_tree()),
        last(&json!([test_case(2, "P-T2", "Login case")])),
        last(&json!([])),
    ]);
    let output = run(
        &transport,
        "get_tests_recursive",
        json!({"project_key":"P","folder_id":"1"}),
    )
    .await
    .expect("recursive");
    assert!(text(&output).starts_with("Extracted 2 tests recursively: [['Test ID: 1'"));
    assert_eq!(
        transport.urls(),
        vec![
            format!("GET {CLOUD_API}/testcases?maxResults=100&startAt=0&projectKey=P&folderId=1"),
            format!("GET {CLOUD_API}/folders?maxResults=1000&projectKey=P&folderType=TEST_CASE"),
            format!("GET {CLOUD_API}/testcases?maxResults=100&startAt=0&projectKey=P&folderId=2"),
            format!("GET {CLOUD_API}/testcases?maxResults=100&startAt=0&projectKey=P&folderId=3"),
        ]
    );
}

#[tokio::test]
async fn search_filters_sorts_limits_and_projects_like_the_sdk() {
    let cases = json!([
        {"key":"P-T1","name":"Alpha login","labels":["Smoke"],"customFields":{"Country":"All","Tags":["a","b"]},"folder":{"id":2}},
        {"key":"P-T2","name":"Beta login","labels":["Smoke","UI"],"customFields":{"Country":"All","Tags":["c"]}},
        {"key":"P-T3","name":"Gamma login","labels":["API"],"customFields":{"Country":"All","Tags":["b"]}},
        {"key":"P-T4","name":"Delta logout","labels":["Smoke"],"customFields":{"Country":"DE","Tags":["b"]}}
    ]);
    let transport = FixtureTransport::json([last(&cases)]);
    let output = run(
        &transport,
        "search_test_cases",
        json!({
            "project_key":"P","search_term":"LOGIN","labels":["Smoke","API"],
            "custom_fields":"{\"Country\":\"All\",\"Tags\":[\"b\"]}",
            "order_direction":"DESC","fields":["key","customFields.Country","folder"],
            "limit_results":5
        }),
    )
    .await
    .expect("search");
    assert_eq!(
        text(&output),
        "Found 2 test cases matching search term 'LOGIN' and labels '['Smoke', 'API']' and custom fields '{\"Country\":\"All\",\"Tags\":[\"b\"]}': [\n  {\n    \"key\": \"P-T3\",\n    \"customFields\": {\n      \"Country\": \"All\"\n    }\n  },\n  {\n    \"key\": \"P-T1\",\n    \"customFields\": {\n      \"Country\": \"All\"\n    },\n    \"folder\": {\n      \"id\": 2\n    }\n  }\n]"
    );
    assert_eq!(
        transport.requests()[0].url,
        format!("{CLOUD_API}/testcases?projectKey=P&maxResults=1000&startAt=0")
    );
}

#[tokio::test]
async fn search_by_folder_and_steps_reads_each_matching_case() {
    let transport = FixtureTransport::json([
        last(&folder_tree()),
        last(&json!([{"key":"P-T1","name":"A"},{"key":"P-T2","name":"B"}])),
        last(&json!([])),
        last(
            &json!([{"inline":{"description":"Click SUBMIT","testData":null,"expectedResult":"ok"}}]),
        ),
        last(&json!([{"inline":null,"testCase":{"testCaseKey":"P-T9"}}])),
    ]);
    let output = run(
        &transport,
        "search_test_cases",
        json!({"project_key":"P","folder_name":"Login","steps_search":"submit","include_steps":true,"fields":["key","steps"],"order_by":""}),
    )
    .await
    .expect("folder search");
    assert_eq!(
        text(&output),
        "Found 1 test cases matching folder name 'Login' and folder ID '2' and steps containing 'submit': [\n  {\n    \"key\": \"P-T1\",\n    \"steps\": [\n      {\n        \"inline\": {\n          \"description\": \"Click SUBMIT\",\n          \"expectedResult\": \"ok\",\n          \"testData\": null\n        }\n      }\n    ]\n  }\n]"
    );
    assert_eq!(
        transport.urls(),
        vec![
            format!("GET {CLOUD_API}/folders?maxResults=1000&projectKey=P&folderType=TEST_CASE"),
            format!("GET {CLOUD_API}/testcases?projectKey=P&maxResults=1000&startAt=0&folderId=2"),
            format!("GET {CLOUD_API}/testcases?projectKey=P&maxResults=1000&startAt=0&folderId=3"),
            format!("GET {CLOUD_API}/testcases/P-T1/teststeps"),
            format!("GET {CLOUD_API}/testcases/P-T2/teststeps"),
        ]
    );
    let transport = FixtureTransport::json([last(&json!([]))]);
    let bad = run(
        &transport,
        "search_test_cases",
        json!({"project_key":"P","custom_fields":"[1]"}),
    )
    .await
    .expect("custom field text");
    assert_eq!(
        text(&bad),
        "Error processing custom fields: 'list' object has no attribute 'items'"
    );
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let transport = FixtureTransport::new([]);
    let names = super::sdk_conformance::sdk_tool_names("zephyr_scale");
    let names = names.iter().map(String::as_str).collect::<Vec<_>>();
    let tools = test_build_with_client("Scale QA", &names, &policy(&[]), &client(&transport))
        .expect("toolset")
        .tools(readonly())
        .await
        .expect("tools");
    super::sdk_conformance::assert_sdk_conformance("zephyr_scale", &tools);
}
