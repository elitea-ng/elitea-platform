//! Zephyr Enterprise family: configuration, routes, SDK output shapes and the
//! SDK schema gate, over fixture responses shaped like the flex REST API.

use std::sync::Arc;

use adk_core::{Tool, Toolset};
use reqwest::{Method, StatusCode};
use serde_json::{Value, json};

use super::families::zephyr_enterprise::client::ZephyrEnterpriseClient;
use super::families::zephyr_enterprise::config::ZephyrEnterpriseToolkitConfig;
use super::families::zephyr_enterprise::tools::{test_build_with_client, test_catalog};
use super::families::zephyr_rest::client::{ZephyrRestErrorCode, ZephyrRestResponse};
use super::families::zephyr_rest::config::ZephyrRestConfigErrorCode;
use super::families::zephyr_rest::tools::ZephyrRestToolsetErrorCode;
use super::zephyr_rest_tests::{
    FixtureTransport, TOKEN, policy, readonly, rest_client, tool_context,
};

const BASE: &str = "https://zephyr.example.test/zephyr";
const API: &str = "https://zephyr.example.test/zephyr/flex/services/rest/latest";

fn client(transport: &Arc<FixtureTransport>) -> Arc<ZephyrEnterpriseClient> {
    Arc::new(ZephyrEnterpriseClient::new(rest_client(BASE, transport)))
}

async fn tools(transport: &Arc<FixtureTransport>, selected: &[&str]) -> Vec<Arc<dyn Tool>> {
    test_build_with_client("Enterprise QA", selected, &policy(&[]), &client(transport))
        .expect("Zephyr Enterprise toolset")
        .tools(readonly())
        .await
        .expect("Zephyr Enterprise tools")
}

async fn run(
    transport: &Arc<FixtureTransport>,
    name: &str,
    arguments: Value,
) -> adk_core::Result<Value> {
    let tools = tools(transport, &[name]).await;
    tools[0].execute(tool_context(), arguments).await
}

#[test]
fn configuration_is_nested_https_and_requires_the_token() {
    let settings = json!({
        "zephyr_configuration":{"base_url":"https://zephyr.example.test/zephyr/","token":TOKEN},
        "selected_tools":["get_test_case","index_data","get_test_case"]
    });
    let config = ZephyrEnterpriseToolkitConfig::parse(settings.as_object().expect("settings"))
        .expect("valid config");
    assert_eq!(
        config.selected_tools(),
        [
            Box::<str>::from("get_test_case"),
            Box::<str>::from("index_data")
        ]
    );
    for invalid in [
        json!({"zephyr_configuration":{"base_url":BASE}}),
        json!({"zephyr_configuration":{"base_url":BASE,"token":null}}),
        json!({"zephyr_configuration":{"base_url":"http://zephyr.example.test","token":TOKEN}}),
        json!({"base_url":BASE,"token":TOKEN}),
    ] {
        let Err(error) = ZephyrEnterpriseToolkitConfig::parse(invalid.as_object().expect("object"))
        else {
            panic!("invalid Zephyr Enterprise configuration must fail");
        };
        assert_eq!(
            error.code(),
            ZephyrRestConfigErrorCode::InvalidConfiguration
        );
        assert!(!format!("{error:?} {error}").contains(TOKEN));
    }
}

#[tokio::test]
async fn catalog_descriptions_and_selection_follow_the_sdk() {
    assert_eq!(
        test_catalog(),
        vec![
            ("get_test_case", true),
            ("search_zql", true),
            ("create_testcase", false),
            ("add_steps", false),
            ("get_testcases_by_zql", true),
        ]
    );
    let transport = FixtureTransport::new([]);
    let all = tools(&transport, &[]).await;
    assert_eq!(all.len(), 5);
    for tool in &all {
        assert!(tool.description().starts_with("Toolkit: Enterprise QA\n"));
        assert!(
            tool.description()
                .ends_with("\nZephyr Enterprise instance: https://zephyr.example.test/zephyr")
        );
        assert!(tool.description().chars().count() <= 1_000);
        assert!(!tool.description().contains(TOKEN));
    }
    // Indexing tools are not served; a selection of only those is skipped.
    let Err(error) = test_build_with_client(
        "Enterprise QA",
        &["index_data", "search_index"],
        &policy(&[]),
        &client(&transport),
    ) else {
        panic!("an index-only selection must be unsupported");
    };
    assert_eq!(
        error.code(),
        ZephyrRestToolsetErrorCode::UnsupportedSelection
    );
    let partial = tools(&transport, &["index_data", "add_steps"]).await;
    assert_eq!(partial.len(), 1);
    assert_eq!(partial[0].name(), "add_steps");
    // Deployment policy can still remove a served tool.
    let blocked = test_build_with_client(
        "Enterprise QA",
        &[],
        &policy(&[("zephyr_enterprise", &["create_testcase"])]),
        &client(&transport),
    )
    .expect("policy-filtered toolset")
    .tools(readonly())
    .await
    .expect("policy-filtered tools");
    assert!(blocked.iter().all(|tool| tool.name() != "create_testcase"));
}

#[tokio::test]
async fn reads_use_the_flex_routes() {
    let transport = FixtureTransport::json([
        json!({"id":137,"testcase":{"testcaseId":42,"name":"Login"}}),
        json!({"resultSize":1,"results":[{"id":7}]}),
    ]);
    let test_case = run(&transport, "get_test_case", json!({"testcase_id":"137"}))
        .await
        .expect("get_test_case");
    assert_eq!(test_case["testcase"]["testcaseId"], 42);
    let search = run(
        &transport,
        "search_zql",
        json!({"zql_json":"{\"entitytype\":\"testcase\",\"word\":\"id = 358380\"}"}),
    )
    .await
    .expect("search_zql");
    assert_eq!(search["resultSize"], 1);
    let requests = transport.requests();
    assert_eq!(
        transport.urls(),
        vec![
            format!("GET {API}/testcase/137"),
            format!("POST {API}/advancesearch/zql"),
        ]
    );
    assert_eq!(
        requests[1].body,
        Some(json!({"entitytype":"testcase","word":"id = 358380"}))
    );
    assert_eq!(
        requests[0].authorization.as_deref(),
        Some("Bearer zephyr-test-bearer-token")
    );
}

#[tokio::test]
async fn zql_test_case_listing_renders_the_sdk_lines() {
    let transport = FixtureTransport::json([
        json!({"resultSize":2,"results":[
            {"id":11,"testcase":{"name":"Login","testcaseId":101}},
            {"testcase":{"name":"Logout","testcaseId":102}}
        ]}),
        json!({"resultSize":0,"results":[]}),
    ]);
    let found = run(
        &transport,
        "get_testcases_by_zql",
        json!({"zql":"folder=\"TestToolkit\""}),
    )
    .await
    .expect("listing");
    assert_eq!(
        found,
        json!(
            "Test case ID: 11, Test case: {\"name\":\"Login\",\"testcaseId\":101}\nTest case ID: None, Test case: {\"name\":\"Logout\",\"testcaseId\":102}"
        )
    );
    let empty = run(
        &transport,
        "get_testcases_by_zql",
        json!({"zql":"name~\"none\""}),
    )
    .await
    .expect("empty listing");
    assert_eq!(
        empty,
        json!("No test cases found for the provided ZQL query.")
    );
    assert_eq!(
        transport.requests()[0].url,
        format!("{API}/testcase?zqlquery=folder%3D%22TestToolkit%22")
    );
}

#[tokio::test]
async fn create_posts_the_decoded_body_to_the_trailing_slash_route() {
    let transport = FixtureTransport::new([Ok(ZephyrRestResponse::json(
        StatusCode::CREATED,
        &json!({"id":555,"testcase":{"name":"TestToolkit"}}),
    ))]);
    let created = run(
        &transport,
        "create_testcase",
        json!({"create_testcase_json":"{\"tcrCatalogTreeId\":137973,\"testcase\":{\"name\":\"TestToolkit\",\"projectId\":75}}"}),
    )
    .await
    .expect("create");
    assert_eq!(created["id"], 555);
    let request = &transport.requests()[0];
    assert_eq!(request.method, Method::POST);
    assert_eq!(request.url, format!("{API}/testcase/"));
    assert_eq!(
        request.body,
        Some(json!({"tcrCatalogTreeId":137_973,"testcase":{"name":"TestToolkit","projectId":75}}))
    );
    let error = run(
        &transport,
        "create_testcase",
        json!({"create_testcase_json":"{oops"}),
    )
    .await
    .expect_err("invalid JSON argument");
    assert_eq!(error.code, "tool.execution.invalid_input");
}

#[tokio::test]
async fn add_steps_resolves_the_last_version_and_chains_step_ids() {
    let transport = FixtureTransport::json([
        json!({"id":137,"testcase":{"testcaseId":42}}),
        json!([{"id":900},{"id":901}]),
        json!({"id":7000,"steps":[{"orderId":1},{"orderId":"3"}]}),
        json!({"id":7001}),
        json!({"id":7002}),
    ]);
    let output = run(
        &transport,
        "add_steps",
        json!({"testcase_tree_id":"137","steps":[
            {"step":"Open login","data":"user=a","result":"Form shown"},
            {"step":"Submit"}
        ]}),
    )
    .await
    .expect("add_steps");
    assert_eq!(
        output,
        json!(
            "Step added: Open login, data: user=a, result: Form shown;Step added: Submit, data: , result: "
        )
    );
    let requests = transport.requests();
    assert_eq!(
        transport.urls(),
        vec![
            format!("GET {API}/testcase/137"),
            format!("GET {API}/testcase/versions?testcaseid=42"),
            format!("GET {API}/testcase/901/teststep"),
            format!("POST {API}/testcase/901/teststep/detail/137"),
            format!("POST {API}/testcase/901/teststep/detail/137"),
        ]
    );
    assert_eq!(
        requests[3].body,
        Some(json!({
            "tcId":901,"maxId":4,"tctId":"137","id":7000,
            "step":{"step":"Open login","data":"user=a","result":"Form shown","orderId":4}
        }))
    );
    assert_eq!(requests[4].body.as_ref().expect("second body")["id"], 7001);
    assert_eq!(requests[4].body.as_ref().expect("second body")["maxId"], 5);
}

#[tokio::test]
async fn add_steps_to_an_empty_case_starts_at_one_and_reports_partial_effects() {
    let transport = FixtureTransport::new([
        Ok(ZephyrRestResponse::json(
            StatusCode::OK,
            &json!({"testcase":{"testcaseId":42}}),
        )),
        Ok(ZephyrRestResponse::json(
            StatusCode::OK,
            &json!([{"id":901}]),
        )),
        Ok(ZephyrRestResponse::fixture(StatusCode::OK, None, b"")),
        Ok(ZephyrRestResponse::json(
            StatusCode::OK,
            &json!({"id":7001}),
        )),
        Ok(ZephyrRestResponse::fixture(
            StatusCode::BAD_REQUEST,
            None,
            b"",
        )),
    ]);
    let error = run(
        &transport,
        "add_steps",
        json!({"testcase_tree_id":"137","steps":[{"step":"a"},{"step":"b"}]}),
    )
    .await
    .expect_err("second append fails after the first succeeded");
    assert_eq!(error.code, "tool.execution.internal");
    let first = transport.requests()[3].body.clone().expect("first body");
    assert_eq!(first["maxId"], 1);
    assert_eq!(first["id"], Value::Null);

    let transport = FixtureTransport::new([]);
    let empty = run(
        &transport,
        "add_steps",
        json!({"testcase_tree_id":"137","steps":[]}),
    )
    .await
    .expect("empty steps");
    assert_eq!(empty, json!("Steps cannot be empty."));
    let null = run(
        &transport,
        "add_steps",
        json!({"testcase_tree_id":"137","steps":null}),
    )
    .await
    .expect("null steps");
    assert_eq!(null, json!("Steps cannot be empty."));
    assert!(transport.requests().is_empty());
}

#[tokio::test]
async fn provider_failures_are_stable_and_redacted() {
    let transport = FixtureTransport::new([Ok(ZephyrRestResponse::fixture(
        StatusCode::UNAUTHORIZED,
        Some("application/json"),
        br#"{"error":"token expired for qa@example.test"}"#,
    ))]);
    let client = client(&transport);
    let error = client.get_test_case("1").await.expect_err("401");
    assert_eq!(error.code(), ZephyrRestErrorCode::Authentication);
    let adk = error.into_adk(&super::families::zephyr_enterprise::client::FAMILY);
    assert_eq!(adk.code, "zephyr_enterprise.authentication.failed");
    assert!(!adk.message.contains("qa@example.test"));
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let transport = FixtureTransport::new([]);
    let names = super::sdk_conformance::sdk_tool_names("zephyr_enterprise");
    let names = names.iter().map(String::as_str).collect::<Vec<_>>();
    let tools = tools(&transport, &names).await;
    super::sdk_conformance::assert_sdk_conformance("zephyr_enterprise", &tools);
}
