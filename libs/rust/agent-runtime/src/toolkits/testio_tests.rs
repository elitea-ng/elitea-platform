use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_core::{ErrorCategory, ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::header::AUTHORIZATION;
use reqwest::{Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::testio::client::{
    TestIoApi, TestIoClient, TestIoClientError, TestIoHttpResponse, TestIoTransport,
};
use super::families::testio::config::{TestIoConfigErrorCode, TestIoToolkitConfig};
use super::families::testio::tools::{
    TestIoToolsetErrorCode, build_testio_toolset, test_build_with_api,
};
use super::policy::ToolAdmissionPolicy;

const READS: [&str; 13] = [
    "list_products",
    "get_product",
    "list_features",
    "get_feature",
    "list_user_stories",
    "get_user_story",
    "list_exploratory_tests",
    "get_exploratory_test",
    "list_test_cases",
    "get_test_case",
    "get_test_cases_for_test",
    "get_test_cases_statuses_for_test",
    "list_bugs_for_test_with_filter",
];

fn settings(selected_tools: &[&str]) -> Map<String, Value> {
    json!({
        "testio_configuration":{
            "endpoint":"https://api.test.io/",
            "api_key":"testio-secret-token"
        },
        "selected_tools":selected_tools
    })
    .as_object()
    .cloned()
    .expect("TestIO fixture settings are an object")
}

fn config() -> TestIoToolkitConfig {
    TestIoToolkitConfig::parse(&settings(&[])).expect("valid TestIO configuration")
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("TestIO policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("testio-test").with_function_call_id("testio-call"))
}

type Handler = dyn Fn(&str) -> Result<TestIoHttpResponse, TestIoClientError> + Send + Sync;

struct FixtureTransport {
    requests: Mutex<Vec<(String, String, bool)>>,
    handler: Box<Handler>,
}

impl FixtureTransport {
    fn new(
        handler: impl Fn(&str) -> Result<TestIoHttpResponse, TestIoClientError> + Send + Sync + 'static,
    ) -> Arc<Self> {
        Arc::new(Self {
            requests: Mutex::new(Vec::new()),
            handler: Box::new(handler),
        })
    }

    fn urls(&self) -> Vec<String> {
        self.requests
            .lock()
            .expect("TestIO request fixture lock")
            .iter()
            .map(|(url, _, _)| url.clone())
            .collect()
    }
}

#[async_trait]
impl TestIoTransport for FixtureTransport {
    async fn execute(&self, request: Request) -> Result<TestIoHttpResponse, TestIoClientError> {
        let url = request.url().as_str().to_owned();
        let authorization = request
            .headers()
            .get(AUTHORIZATION)
            .expect("TestIO authorization header");
        self.requests
            .lock()
            .expect("TestIO request fixture lock")
            .push((
                url.clone(),
                authorization
                    .to_str()
                    .expect("ASCII authorization")
                    .to_owned(),
                authorization.is_sensitive(),
            ));
        (self.handler)(&url)
    }
}

/// Bodies modelled on the Test IO customer API v2 responses.
#[allow(clippy::unnecessary_wraps)] // The transport handler signature returns a Result.
fn provider(url: &str) -> Result<TestIoHttpResponse, TestIoClientError> {
    let path = url
        .strip_prefix("https://api.test.io/customer/v2/")
        .expect("fixed TestIO origin");
    let path = path.split('?').next().unwrap_or_default();
    let body = match path {
        "products" => json!({"products":[
            {"id":11,"name":"Shop","default_section_id":5,"connection":null},
            {"id":12,"name":"Blog","default_section_id":6,"connection":null}
        ]}),
        "products/11" => json!({"product":{"id":11,"name":"Shop","sections":[{"id":5}]}}),
        "products/11/features" => json!({"features":[
            {"id":31,"title":"Checkout","description":"pay","howtofind":"cart"}
        ]}),
        "products/11/user_stories" => json!({"user_stories":[
            {"id":41,"title":"As a buyer","path":"/cart"}
        ]}),
        "products/11/exploratory_tests" => json!({"exploratory_tests":[
            {"id":51,"title":"Smoke","status":"running"},
            {"id":52,"title":"Regression","status":"locked"}
        ]}),
        "products/11/test_case_tests/61" => json!({
            "test_case_test":{"id":61,"title":"Launch"},
            "test_cases":[{"id":71,"title":"Login","section_id":5}]
        }),
        "products/11/test_case_tests/61/results" => json!({
            "results":[{"test_case_id":71,"result":"passed"}]
        }),
        "products/11/test_cases/71" => json!({"test_case":{"id":71,"title":"Login","steps":[]}}),
        "bugs" => json!({"bugs":[{"id":81,"title":"Crash","severity":"high"}]}),
        "products/404" => return Ok(TestIoHttpResponse::fixture(StatusCode::NOT_FOUND, None)),
        "products/401" => {
            return Ok(TestIoHttpResponse::fixture(StatusCode::UNAUTHORIZED, None));
        }
        other => panic!("unexpected TestIO path {other}"),
    };
    Ok(TestIoHttpResponse::fixture(StatusCode::OK, Some(body)))
}

async fn tools_with(transport: Arc<FixtureTransport>) -> Vec<Arc<dyn Tool>> {
    let client: Arc<dyn TestIoApi> = Arc::new(TestIoClient::with_transport(config(), transport));
    let toolset =
        test_build_with_api("qa", &[], &policy(&[]), &client).expect("complete TestIO toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    toolset.tools(readonly).await.expect("TestIO tools")
}

fn tool<'a>(tools: &'a [Arc<dyn Tool>], name: &str) -> &'a Arc<dyn Tool> {
    tools
        .iter()
        .find(|tool| tool.name() == name)
        .unwrap_or_else(|| panic!("TestIO tool {name}"))
}

#[test]
fn configuration_normalizes_the_endpoint_and_redacts_the_token() {
    let parsed = TestIoToolkitConfig::parse(&settings(&["get_product", "get_product"]))
        .expect("valid TestIO configuration");
    assert_eq!(parsed.selected_tools(), [Box::<str>::from("get_product")]);

    for invalid in [
        json!({}),
        json!({"testio_configuration":{"endpoint":"https://api.test.io"}}),
        json!({"testio_configuration":{"api_key":"k"}}),
        json!({"testio_configuration":{"endpoint":"http://api.test.io","api_key":"k"}}),
        json!({"testio_configuration":{"endpoint":"https://u:p@api.test.io","api_key":"k"}}),
        json!({"testio_configuration":{"endpoint":"https://api.test.io","api_key":"  "}}),
        json!({"testio_configuration":{"endpoint":"https://api.test.io","api_key":"a b"}}),
    ] {
        let Err(error) = TestIoToolkitConfig::parse(invalid.as_object().expect("object")) else {
            panic!("invalid TestIO configuration was accepted: {invalid}");
        };
        assert_eq!(error.code(), TestIoConfigErrorCode::InvalidConfiguration);
        assert!(!format!("{error:?}").contains("testio-secret-token"));
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One ordered corpus proves every read's wire contract.
async fn reads_use_token_auth_fixed_paths_and_sdk_result_members() {
    let transport = FixtureTransport::new(provider);
    let tools = tools_with(Arc::clone(&transport)).await;
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        READS
    );
    assert!(tools.iter().all(|tool| tool.is_read_only()));
    assert!(tools[0].description().starts_with("Toolkit: qa\n"));

    let products = tool(&tools, "list_products")
        .execute(
            context(),
            json!({"filter_product_ids":[12],"client_fields":["name"]}),
        )
        .await
        .expect("list products");
    assert_eq!(products, json!([{"name":"Blog"}]));

    let product = tool(&tools, "get_product")
        .execute(context(), json!({"product_id":11}))
        .await
        .expect("get product");
    assert_eq!(product["name"], "Shop");

    let feature = tool(&tools, "get_feature")
        .execute(context(), json!({"product_id":"11","feature_id":31}))
        .await
        .expect("get feature");
    assert_eq!(feature["title"], "Checkout");

    let story = tool(&tools, "get_user_story")
        .execute(context(), json!({"product_id":11,"story_id":41}))
        .await
        .expect("get user story");
    assert_eq!(
        story,
        json!([{"id":41,"title":"As a buyer","path":"/cart"}])
    );

    let exploratory = tool(&tools, "get_exploratory_test")
        .execute(
            context(),
            json!({"exploratory_test_id":52,"product_id":11,"client_fields":["status"]}),
        )
        .await
        .expect("get exploratory test");
    assert_eq!(exploratory, json!({"status":"locked"}));
    let missing = tool(&tools, "get_exploratory_test")
        .execute(context(), json!({"exploratory_test_id":99,"product_id":11}))
        .await
        .expect("missing exploratory test is null, as the SDK returns None");
    assert_eq!(missing, Value::Null);

    let cases = tool(&tools, "list_test_cases")
        .execute(
            context(),
            json!({"product_id":11,"cycle_id":61,"section_id":5}),
        )
        .await
        .expect("list test cases");
    assert_eq!(cases[0]["title"], "Login");
    let launch = tool(&tools, "get_test_cases_for_test")
        .execute(context(), json!({"product_id":11,"test_case_test_id":61}))
        .await
        .expect("test cases for test");
    assert_eq!(launch, json!({"id":61,"title":"Launch"}));
    let statuses = tool(&tools, "get_test_cases_statuses_for_test")
        .execute(context(), json!({"product_id":11,"test_case_test_id":61}))
        .await
        .expect("statuses");
    assert_eq!(statuses["results"][0]["result"], "passed");
    let test_case = tool(&tools, "get_test_case")
        .execute(context(), json!({"product_id":11,"test_case_id":71}))
        .await
        .expect("test case");
    assert_eq!(test_case["id"], 71);
    let bugs = tool(&tools, "list_bugs_for_test_with_filter")
        .execute(
            context(),
            json!({"filter_product_ids":"11,12","filter_test_cycle_ids":"","client_fields":["id"]}),
        )
        .await
        .expect("bugs");
    assert_eq!(bugs, json!([{"id":81}]));

    assert_eq!(
        transport.urls(),
        [
            "https://api.test.io/customer/v2/products",
            "https://api.test.io/customer/v2/products/11",
            "https://api.test.io/customer/v2/products/11/features?filter_feature_ids=31",
            "https://api.test.io/customer/v2/products/11/user_stories?filter_user_story_ids=41",
            "https://api.test.io/customer/v2/products/11/exploratory_tests",
            "https://api.test.io/customer/v2/products/11/exploratory_tests",
            "https://api.test.io/customer/v2/products/11/test_case_tests/61?filter_section_ids=5",
            "https://api.test.io/customer/v2/products/11/test_case_tests/61",
            "https://api.test.io/customer/v2/products/11/test_case_tests/61/results",
            "https://api.test.io/customer/v2/products/11/test_cases/71",
            "https://api.test.io/customer/v2/bugs?filter_product_ids=11%2C12",
        ]
    );
    for (_, authorization, sensitive) in transport
        .requests
        .lock()
        .expect("TestIO request fixture lock")
        .iter()
    {
        assert_eq!(authorization, "Token testio-secret-token");
        assert!(sensitive);
    }
}

#[tokio::test]
async fn provider_failures_map_to_categories_and_arguments_fail_before_network() {
    let transport = FixtureTransport::new(provider);
    let tools = tools_with(Arc::clone(&transport)).await;
    let not_found = tool(&tools, "get_product")
        .execute(context(), json!({"product_id":404}))
        .await
        .expect_err("404 fails");
    // The admission wrapper replaces every family message with a stable,
    // category-level one; the category is what survives.
    assert_eq!(not_found.category, ErrorCategory::NotFound);
    let unauthorized = tool(&tools, "get_product")
        .execute(context(), json!({"product_id":401}))
        .await
        .expect_err("401 fails");
    assert_eq!(unauthorized.category, ErrorCategory::Unauthorized);

    let before = transport.urls().len();
    for (name, arguments) in [
        ("get_product", json!({})),
        ("get_product", json!({"product_id":-1})),
        ("get_product", json!({"product_id":11,"extra":true})),
        ("list_exploratory_tests", json!({})),
        ("get_exploratory_test", json!({"exploratory_test_id":51})),
        ("list_products", json!({"client_fields":"name"})),
        (
            "get_test_cases_for_test",
            json!({"product_id":11,"test_case_test_id":61,"client_fields":["id"]}),
        ),
    ] {
        assert!(
            tool(&tools, name)
                .execute(context(), arguments.clone())
                .await
                .is_err(),
            "{name} accepted {arguments}"
        );
    }
    assert_eq!(transport.urls().len(), before);
}

#[tokio::test]
async fn selection_keeps_named_reads_and_skips_unserved_writes() {
    let transport = FixtureTransport::new(provider);
    let client: Arc<dyn TestIoApi> = Arc::new(TestIoClient::with_transport(config(), transport));
    let toolset = test_build_with_api(
        "qa",
        &["get_product", "confirm_bug_fix"],
        &policy(&[]),
        &client,
    )
    .expect("selected TestIO tools");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let names = toolset
        .tools(readonly)
        .await
        .expect("selected tools")
        .iter()
        .map(|tool| tool.name().to_owned())
        .collect::<Vec<_>>();
    assert_eq!(names, ["get_product"]);

    let Err(error) = test_build_with_api("qa", &["create_exploratory_test"], &policy(&[]), &client)
    else {
        panic!("a selection of unserved writes must not build");
    };
    assert_eq!(error.code(), TestIoToolsetErrorCode::UnsupportedSelection);

    let writes_only = TestIoToolkitConfig::parse(&settings(&["confirm_bug_fix"]))
        .expect("bounded selection parses");
    let Err(error) = build_testio_toolset("qa", writes_only, &policy(&[])) else {
        panic!("writes-only selection must not build");
    };
    assert_eq!(error.code(), TestIoToolsetErrorCode::UnsupportedSelection);

    let blocked = test_build_with_api(
        "qa",
        &[],
        &policy(&[("testio", &["list_products"])]),
        &client,
    )
    .expect("policy-filtered TestIO toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert!(
        blocked
            .tools(readonly)
            .await
            .expect("filtered tools")
            .iter()
            .all(|tool| tool.name() != "list_products")
    );
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let tools = tools_with(FixtureTransport::new(provider)).await;
    super::sdk_conformance::assert_sdk_conformance("testio", &tools);
}
