use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::header::{AUTHORIZATION, CONTENT_TYPE};
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::carrier::client::{
    CarrierClient, CarrierClientError, CarrierHttpResponse, CarrierTransport,
};
use super::families::carrier::config::{CarrierConfigErrorCode, CarrierToolkitConfig};
use super::families::carrier::tools::{
    CarrierToolsetErrorCode, test_build_with_client, test_served_selection,
};
use super::policy::ToolAdmissionPolicy;

const PROJECT: &str = "12";

fn settings() -> Map<String, Value> {
    json!({
        "carrier_configuration":{
            "url":"https://carrier.example.test/",
            "organization":"perf-org",
            "private_token":"carrier-super-secret"
        },
        "project_id":PROJECT,
        "selected_tools":[]
    })
    .as_object()
    .cloned()
    .expect("Carrier fixture settings are an object")
}

fn config() -> CarrierToolkitConfig {
    CarrierToolkitConfig::parse(&settings()).expect("valid Carrier configuration")
}

fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("Carrier policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("carrier-test").with_function_call_id("carrier-call"))
}

#[derive(Clone, Debug)]
struct Captured {
    method: Method,
    url: String,
    authorization: Option<String>,
    authorization_sensitive: bool,
    organization: Option<String>,
    content_type: Option<String>,
    body: Option<String>,
    effect: bool,
}

type Handler = dyn Fn(&Captured) -> CarrierHttpResponse + Send + Sync;

struct FixtureTransport {
    requests: Mutex<Vec<Captured>>,
    handler: Box<Handler>,
}

impl FixtureTransport {
    fn new(
        handler: impl Fn(&Captured) -> CarrierHttpResponse + Send + Sync + 'static,
    ) -> Arc<Self> {
        Arc::new(Self {
            requests: Mutex::new(Vec::new()),
            handler: Box::new(handler),
        })
    }

    fn requests(&self) -> Vec<Captured> {
        self.requests.lock().expect("Carrier fixture lock").clone()
    }
}

#[async_trait]
impl CarrierTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<CarrierHttpResponse, CarrierClientError> {
        let header = |name: &str| {
            request
                .headers()
                .get(name)
                .and_then(|value| value.to_str().ok())
                .map(ToOwned::to_owned)
        };
        let captured = Captured {
            method: request.method().clone(),
            url: request.url().to_string(),
            authorization: header(AUTHORIZATION.as_str()),
            authorization_sensitive: request
                .headers()
                .get(AUTHORIZATION)
                .is_some_and(reqwest::header::HeaderValue::is_sensitive),
            organization: header("x-organization"),
            content_type: header(CONTENT_TYPE.as_str()),
            body: request
                .body()
                .and_then(reqwest::Body::as_bytes)
                .map(|bytes| String::from_utf8_lossy(bytes).into_owned()),
            effect,
        };
        let response = (self.handler)(&captured);
        self.requests
            .lock()
            .expect("Carrier fixture lock")
            .push(captured);
        Ok(response)
    }
}

fn ok(body: &Value) -> CarrierHttpResponse {
    CarrierHttpResponse::fixture(StatusCode::OK, body)
}

fn url(path: &str) -> String {
    format!("https://carrier.example.test/api/v1/{path}")
}

async fn tool(transport: &Arc<FixtureTransport>, name: &str) -> Arc<dyn Tool> {
    let client =
        CarrierClient::with_transport(config(), Arc::clone(transport) as Arc<dyn CarrierTransport>);
    let toolset = test_build_with_client("perf", &[name.to_owned()], &policy(), client)
        .expect("Carrier toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Carrier tools");
    assert_eq!(tools.len(), 1, "{name}");
    Arc::clone(&tools[0])
}

async fn call(transport: &Arc<FixtureTransport>, name: &str, arguments: Value) -> Value {
    tool(transport, name)
        .await
        .execute(context(), arguments)
        .await
        .expect("Carrier tool result")
}

fn ui_tests_fixture() -> Value {
    json!({"rows":[
        {"id":7,"name":"Checkout_Flow","runner":"Lighthouse-NPM_V12","browser":"chrome",
         "env_vars":{"ENV":"stage","cpu_quota":2,"memory_quota":4,"custom_cmd":""},
         "source":{"name":"git_https","repo":"https://git.example.test/ui.git","branch":"main","password":"never-shown"},
         "schedules":[{"id":1,"name":"nightly","cron":"0 2 * * *","active":true},{"id":2,"name":"old","cron":"0 3 * * *","active":false}],
         "integrations":{"reporters":{"reporter_email":{"recipients":["qa@example.test"]}}},
         "test_parameters":[{"name":"test_name","type":"string","default":"Checkout_Flow","description":"n"}]}
    ]})
}

fn ui_details_fixture() -> Value {
    json!({
        "id":7,"name":"Checkout_Flow","location":"default","parallel_runners":1,"loops":2,"aggregation":"max",
        "entrypoint":"checkout.js","runner":"Lighthouse-NPM_V12","source":{"name":"git_https"},
        "env_vars":{"cpu_quota":2,"memory_quota":4,"cloud_settings":{},"ENV":"stage","custom_cmd":""},
        "integrations":{"system":{"s3_integration":{"integration_id":3,"is_local":false}},"reporters":{}},
        "test_parameters":[
            {"name":"test_name","default":"Checkout_Flow"},
            {"name":"test_type","default":"perf"},
            {"name":"env_type","default":"stage"}
        ],
        "schedules":[{"id":1,"name":"nightly","cron":"0 2 * * *","active":true,"project_id":12,"test_id":7}]
    })
}

fn locations_fixture() -> Value {
    json!({
        "public_regions":["default"],
        "project_regions":["eu-runner"],
        "cloud_regions":[{"name":"aws-west","cloud_settings":{"region_name":"us-west-2","integration_name":"aws","id":9,"project_id":12,"image_id":"ami-1"}}]
    })
}

#[test]
fn configuration_normalizes_origin_project_and_selection_without_network() {
    let parsed = config();
    let rendered = format!("{:?}", CarrierToolkitConfig::parse(&settings()).err());
    assert!(!rendered.contains("carrier-super-secret"));
    assert!(parsed.selected_tools().is_empty());

    let mut numeric = settings();
    numeric.insert("project_id".to_owned(), json!(12));
    CarrierToolkitConfig::parse(&numeric).expect("a numeric project id is its decimal text");

    for (mutate, expected) in [
        (
            json!({"carrier_configuration":{"url":"http://carrier.test","organization":"o","private_token":"t"},"project_id":"1"}),
            CarrierConfigErrorCode::InvalidConfiguration,
        ),
        (
            json!({"carrier_configuration":{"url":"https://u@carrier.test","organization":"o","private_token":"t"},"project_id":"1"}),
            CarrierConfigErrorCode::InvalidConfiguration,
        ),
        (
            json!({"carrier_configuration":{"url":"https://carrier.test","organization":"o","private_token":"t"},"project_id":""}),
            CarrierConfigErrorCode::InvalidConfiguration,
        ),
        (
            json!({"carrier_configuration":{"url":"https://carrier.test","organization":"o","private_token":"t"},"project_id":"../2"}),
            CarrierConfigErrorCode::InvalidConfiguration,
        ),
        (
            json!({"carrier_configuration":{"url":"https://carrier.test","organization":"o"},"project_id":"1"}),
            CarrierConfigErrorCode::InvalidConfiguration,
        ),
        (
            json!({"carrier_configuration":{"url":"https://carrier.test","organization":"o","private_token":"x".repeat(16 * 1_024 + 1)},"project_id":"1"}),
            CarrierConfigErrorCode::ResourceExhausted,
        ),
    ] {
        let Err(error) = CarrierToolkitConfig::parse(mutate.as_object().expect("object")) else {
            panic!("invalid Carrier configuration must fail: {mutate}");
        };
        assert_eq!(error.code(), expected, "{mutate}");
    }
}

#[test]
fn selection_serves_fifteen_tools_and_omits_only_the_archive_tools() {
    let names = |values: &[&str]| {
        values
            .iter()
            .map(|value| Box::<str>::from(*value))
            .collect::<Vec<_>>()
    };
    assert!(
        test_served_selection(&[])
            .expect("empty selection")
            .is_empty()
    );
    assert_eq!(
        test_served_selection(&names(&[
            "get_tests",
            "get_report_by_id",
            "create_excel_report"
        ]))
        .expect("mixed selection"),
        vec!["get_tests".to_owned()]
    );
    for refused in [
        names(&["get_report_by_id"]),
        names(&["create_ui_excel_report", "create_excel_report"]),
        names(&["get_tests", "not_a_carrier_tool"]),
    ] {
        let Err(error) = test_served_selection(&refused) else {
            panic!("{refused:?} must be refused");
        };
        assert_eq!(error.code(), CarrierToolsetErrorCode::UnsupportedSelection);
    }
}

#[tokio::test]
async fn ticket_list_reads_one_board_with_session_headers_and_filters_titles() {
    let transport = FixtureTransport::new(|_| {
        ok(&json!({"rows":[
            {"title":"Slow login","status":"Open","tags":[{"tag":"perf"}]},
            {"title":"Timeout","status":"Closed","tags":[{"tag":"perf"}]},
            {"title":"Typo","status":"Open","tags":[{"tag":"ui"}]}
        ]}))
    });
    let result = call(
        &transport,
        "get_ticket_list",
        json!({"board_id":"4","tag_name":"PERF","status":"Open"}),
    )
    .await;
    assert_eq!(result, json!("Slow login"));
    let requests = transport.requests();
    assert_eq!(requests.len(), 1);
    let request = &requests[0];
    assert_eq!(request.method, Method::GET);
    assert_eq!(request.url, url("issues/issues/12?board_id=4&limit=100"));
    assert_eq!(
        request.authorization.as_deref(),
        Some("Bearer carrier-super-secret")
    );
    assert!(request.authorization_sensitive);
    assert_eq!(request.organization.as_deref(), Some("perf-org"));
    assert!(!request.effect);
}

#[tokio::test]
async fn create_ticket_resolves_the_engagement_and_posts_the_validated_payload() {
    let transport = FixtureTransport::new(|request| {
        if request.method == Method::GET {
            ok(&json!({"items":[{"name":"Carrier","hash_id":"eng-hash"}]}))
        } else {
            ok(&json!({"item":{"id":91,"title":"Perf Ticket"}}))
        }
    });
    let result = call(
        &transport,
        "create_ticket",
        json!({
            "title":"Perf Ticket","description":"Investigate","severity":"High","type":"Task",
            "board_id":"4","start_date":"2026-03-13","end_date":"2026-03-30","engagement":"Carrier","tags":["perf"]
        }),
    )
    .await;
    let text = result.as_str().expect("text result");
    assert!(
        text.starts_with("✅ Ticket created successfully!\n{"),
        "{text}"
    );
    assert!(text.contains("\"id\": 91"));
    let requests = transport.requests();
    assert_eq!(requests[0].url, url("engagements/engagements/12"));
    assert_eq!(requests[1].method, Method::POST);
    assert_eq!(requests[1].url, url("issues/issues/12"));
    assert!(requests[1].effect);
    let body: Value =
        serde_json::from_str(requests[1].body.as_deref().expect("body")).expect("JSON");
    assert_eq!(body["engagement"], json!("eng-hash"));
    assert_eq!(body["tags"], json!(["perf"]));
    assert!(body.get("assignee").is_none(), "None fields are excluded");

    let invalid = FixtureTransport::new(|_| panic!("an invalid date never reaches Carrier"));
    let result = call(
        &invalid,
        "create_ticket",
        json!({"title":"t","description":"d","severity":"s","type":"Bug","board_id":"1","start_date":"13/03/2026","end_date":"2026-03-30"}),
    )
    .await;
    assert!(
        result
            .as_str()
            .expect("text")
            .contains("**Missing or invalid fields**: start_date")
    );
}

#[tokio::test]
async fn add_tag_uses_the_bare_header_set_and_reads_the_answer_text() {
    let transport = FixtureTransport::new(|_| {
        CarrierHttpResponse::fixture_text(StatusCode::OK, "Tags was updated")
    });
    let result = call(
        &transport,
        "add_tag_to_report",
        json!({"report_id":"55/../x","tag_name":"baseline"}),
    )
    .await;
    assert_eq!(result, json!("Added tag baseline to report id 55/../x"));
    let request = &transport.requests()[0];
    assert_eq!(
        request.url,
        url("backend_performance/tags/12/55%2F..%2Fx"),
        "a model-supplied id stays one path segment"
    );
    assert_eq!(
        request.authorization.as_deref(),
        Some("bearer carrier-super-secret")
    );
    assert_eq!(request.organization, None);
    assert_eq!(
        serde_json::from_str::<Value>(request.body.as_deref().expect("body")).expect("JSON"),
        json!({"tags":[{"title":"baseline","hex":"#5933c6"}]})
    );

    let refused = FixtureTransport::new(|_| {
        CarrierHttpResponse::fixture_text(StatusCode::BAD_REQUEST, "nope")
    });
    let result = call(
        &refused,
        "add_tag_to_report",
        json!({"report_id":"55","tag_name":"baseline"}),
    )
    .await;
    assert_eq!(result, json!("Failed to add new tag to report id 55"));
}

#[tokio::test]
async fn reports_and_tests_are_trimmed_to_the_sdk_fields() {
    let transport = FixtureTransport::new(|request| {
        if request.url.contains("/reports/") {
            ok(&json!({"rows":[
                {"id":1,"build_id":"b1","name":"api","vusers":5,"secret_field":"x",
                 "tags":[{"title":"baseline","hex":"#fff"}],
                 "test_config":{"test_parameters":[{"name":"VUSERS","default":"5","type":"string"}],"source":{"repo":"r"}}},
                {"id":2,"name":"other","tags":[]}
            ]}))
        } else {
            ok(
                &json!({"rows":[{"id":3,"name":"api-test","runner":"v5.5","env_vars":{"x":1},
                "test_parameters":[{"name":"DURATION","default":"60","description":"d"}]}]}),
            )
        }
    });
    let reports = call(&transport, "get_reports", json!({"tag_name":"baseline"})).await;
    assert_eq!(
        reports,
        json!([{"id":1,"build_id":"b1","name":"api","vusers":5,"tags":["baseline"],
            "test_parameters":[{"name":"VUSERS","default":"5"}],"source":{"repo":"r"}}])
    );
    let tests = call(&transport, "get_tests", json!({})).await;
    assert_eq!(
        tests,
        json!([{"id":3,"name":"api-test","runner":"v5.5","test_parameters":[{"name":"DURATION","default":"60"}]}])
    );
    assert_eq!(
        call(&transport, "get_test_by_id", json!({"test_id":"3"})).await["name"],
        json!("api-test")
    );
    assert_eq!(
        call(&transport, "get_test_by_id", json!({"test_id":"404"})).await,
        json!({})
    );
}

#[tokio::test]
async fn run_test_by_id_walks_the_sdk_confirmation_steps_before_one_run() {
    let transport = FixtureTransport::new(|request| {
        if request.url.contains("/tests/") {
            ok(
                &json!({"rows":[{"id":3,"name":"api-test","parallel_runners":2,
                "env_vars":{"cpu_quota":1},"integrations":{"reporters":{}},
                "test_parameters":[
                    {"name":"test_name","type":"string","description":"n","default":"api-test"},
                    {"name":"VUSERS","type":"string","description":"v","default":"5"}
                ]}]}),
            )
        } else if request.url.contains("/locations/") {
            ok(&locations_fixture())
        } else {
            ok(&json!({"result_id":777}))
        }
    });
    assert_eq!(
        call(&transport, "run_test_by_id", json!({})).await,
        json!({"message":"Please provide test id or test name to start"})
    );
    let confirm = call(&transport, "run_test_by_id", json!({"test_id":3})).await;
    assert_eq!(
        confirm["default_test_parameters"][1]["name"],
        json!("VUSERS")
    );
    let location = call(
        &transport,
        "run_test_by_id",
        json!({"test_id":3,"test_parameters":["VUSERS=10"]}),
    )
    .await;
    assert_eq!(
        location["available_locations"]["cloud_regions"],
        json!(["aws-west"])
    );
    let cloud = call(
        &transport,
        "run_test_by_id",
        json!({"name":"api-test","test_parameters":[],"location":"aws-west"}),
    )
    .await;
    assert_eq!(
        cloud["available_cloud_settings"]["instance_type"],
        json!("spot")
    );
    let invalid = call(
        &transport,
        "run_test_by_id",
        json!({"test_id":3,"test_parameters":[],"location":"aws-west","cloud_settings":{"bogus":1}}),
    )
    .await;
    assert!(
        invalid
            .as_str()
            .expect("text")
            .starts_with("Invalid keys in cloud settings: ['bogus']")
    );

    let started = call(
        &transport,
        "run_test_by_id",
        json!({"test_id":3,"test_parameters":[{"VUSERS":"10"}],"location":"default"}),
    )
    .await;
    assert_eq!(
        started,
        json!(
            "Test started. Report id: 777. Link to report:https://carrier.example.test/-/performance/backend/results?result_id=777"
        )
    );
    let run = transport
        .requests()
        .into_iter()
        .rfind(|request| request.method == Method::POST)
        .expect("one run");
    assert_eq!(run.url, url("backend_performance/test/12/3"));
    assert!(run.effect);
    let body: Value = serde_json::from_str(run.body.as_deref().expect("body")).expect("JSON");
    assert_eq!(
        body["test_parameters"][1],
        json!({"name":"VUSERS","type":"string","description":"v","default":"10"})
    );
    assert_eq!(
        body["common_params"]["test_name"]["default"],
        json!("api-test")
    );
    assert_eq!(body["common_params"]["location"], json!("default"));
    assert_eq!(body["common_params"]["parallel_runners"], json!(2));
    assert_eq!(
        body["common_params"]["env_vars"],
        json!({"cpu_quota":1,"cloud_settings":{}})
    );
    assert_eq!(body["integrations"], json!({"reporters":{}}));

    let missing = call(&transport, "run_test_by_id", json!({"name":"nope"})).await;
    assert_eq!(missing, json!("Test with id None or name nope not found."));
}

#[tokio::test]
async fn create_backend_test_posts_one_form_encoded_definition() {
    let transport = FixtureTransport::new(|request| {
        if request.method == Method::GET {
            ok(
                &json!([{"id":4,"project_id":12,"config":{"name":"mail"},"section":{"integration_description":"Email"}}]),
            )
        } else {
            ok(&json!({"id":31}))
        }
    });
    let base = json!({"test_name":"api","test_type":"baseline","env_type":"stage","entrypoint":"t.jmx","custom_cmd":"-l x","runner":"JMeter_v5.5"});
    let mut no_source = base.clone();
    no_source["runner"] = json!("JMeter_v9");
    assert!(
        call(&transport, "create_backend_test", no_source).await["message"]
            .as_str()
            .expect("message")
            .starts_with("Invalid test runner provided.")
    );
    let mut asks_parameters = base.clone();
    asks_parameters["source"] = json!({"name":"git_https","repo":"https://git.example.test/r.git"});
    assert!(
        call(&transport, "create_backend_test", asks_parameters.clone())
            .await
            .get("example_parameters")
            .is_some()
    );
    let mut asks_email = asks_parameters.clone();
    asks_email["test_parameters"] = json!([{"name":"VUSERS","default":"5"}]);
    let prompt = call(&transport, "create_backend_test", asks_email.clone()).await;
    assert_eq!(
        prompt["available_integrations"],
        json!([{"id":4,"name":"mail","description":"Email"}])
    );
    let mut complete = asks_email;
    complete["email_integration"] = json!({"integration_id":4,"recipients":["qa@example.test"]});
    let result = call(&transport, "create_backend_test", complete).await;
    assert_eq!(result, json!("Test created successfully. {\"id\":31}"));
    let post = transport
        .requests()
        .into_iter()
        .rfind(|request| request.method == Method::POST)
        .expect("one create");
    assert_eq!(post.url, url("backend_performance/tests/12"));
    assert_eq!(
        post.authorization.as_deref(),
        Some("bearer carrier-super-secret")
    );
    assert_eq!(
        post.content_type.as_deref(),
        Some("application/x-www-form-urlencoded")
    );
    let form = post.body.expect("form body");
    let data = url_decode_data(&form);
    assert_eq!(data["common_params"]["runner"], json!("v5.5"));
    assert_eq!(
        data["integrations"],
        json!({"reporters":{"reporter_email":{"id":4,"is_local":true,"project_id":12,"recipients":["qa@example.test"]}}})
    );
}

fn url_decode_data(form: &str) -> Value {
    let parsed = reqwest::Url::parse(&format!("https://f.invalid/?{form}")).expect("form");
    let (_, data) = parsed
        .query_pairs()
        .find(|(key, _)| key == "data")
        .expect("a data field");
    serde_json::from_str(&data).expect("JSON data")
}

#[tokio::test]
async fn ui_reports_filter_by_name_and_time_and_refuse_mixed_offsets() {
    let transport = FixtureTransport::new(|_| {
        ok(&json!({"rows":[
            {"id":1,"name":"Checkout_Flow","start_time":"2026-06-02T10:00:00","test_config":{"test_parameters":[]}},
            {"id":2,"name":"checkout_mobile","start_time":"2026-05-01T10:00:00"},
            {"id":3,"name":"Search","start_time":"2026-06-03T10:00:00"}
        ]}))
    });
    let prompt = call(&transport, "get_ui_reports", json!({"report_id":"1"})).await;
    assert!(
        prompt["message"]
            .as_str()
            .expect("message")
            .starts_with("⚠️")
    );
    let by_name = call(&transport, "get_ui_reports", json!({"name":"CHECKOUT"})).await;
    assert_eq!(by_name.as_array().map(Vec::len), Some(2));
    let windowed = call(
        &transport,
        "get_ui_reports",
        json!({"name":"checkout","start_time":"2026-06-01","end_time":"2026-06-30"}),
    )
    .await;
    assert_eq!(
        windowed,
        json!([{"id":1,"name":"Checkout_Flow","start_time":"2026-06-02T10:00:00","test_parameters":[]}])
    );
    let mixed = call(
        &transport,
        "get_ui_reports",
        json!({"start_time":"2026-06-01T00:00:00+00:00"}),
    )
    .await;
    assert_eq!(
        mixed,
        json!("can't compare offset-naive and offset-aware datetimes")
    );
}

#[tokio::test]
async fn ui_report_by_id_adds_unique_sorted_html_links() {
    let transport = FixtureTransport::new(|request| {
        if request.url.contains("/results/") {
            ok(
                &json!({"loop1":[{"file_name":"b.html#index=1"},{"file_name":"a.html"},{"file_name":"data.json"}],
                       "loop2":[{"file_name":"a.html#index=2"}]}),
            )
        } else {
            ok(&json!({"rows":[{"id":5,"uid":"u-5","name":"Checkout"}]}))
        }
    });
    let report = call(&transport, "get_ui_report_by_id", json!({"report_id":"5"})).await;
    let prefix = "https://platform.getcarrier.io/api/v1/artifacts/artifact/default/12/reports";
    assert_eq!(
        report["report_links"],
        json!([
            format!("{prefix}/a.html"),
            format!("{prefix}/b.html"),
            format!("{prefix}/data.json")
        ])
    );
    assert_eq!(
        transport.requests()[1].url,
        url("ui_performance/results/12/u-5?sort=loop&order=asc")
    );
    let absent = call(&transport, "get_ui_report_by_id", json!({"report_id":"9"})).await;
    assert_eq!(absent, json!({"report_links":[]}));
}

#[tokio::test]
async fn ui_tests_project_config_and_schedules_without_source_secrets() {
    let transport = FixtureTransport::new(|_| ok(&ui_tests_fixture()));
    let tests = call(
        &transport,
        "get_ui_tests",
        json!({"name":"checkout","include_schedules":true,"include_config":true}),
    )
    .await;
    let test = &tests[0];
    assert_eq!(
        test["source"],
        json!({"type":"git_https","repo":"https://git.example.test/ui.git","branch":"main"})
    );
    assert_eq!(test["resources"], json!({"cpu":2,"memory":4}));
    assert_eq!(
        test["reporters"],
        json!({"email_recipients":["qa@example.test"]})
    );
    assert_eq!(
        test["schedules"]["active"],
        json!([{"id":1,"name":"nightly","cron":"0 2 * * *"}])
    );
    assert!(!tests.to_string().contains("never-shown"));
}

#[tokio::test]
async fn run_ui_test_shows_defaults_then_runs_with_resolved_overrides() {
    let transport = FixtureTransport::new(|request| {
        if request.url.ends_with("/ui_performance/tests/12") {
            ok(&ui_tests_fixture())
        } else if request.url.contains("/shared/locations/") {
            ok(&locations_fixture())
        } else if request.method == Method::GET {
            ok(&ui_details_fixture())
        } else {
            ok(&json!({"result_id":"r-1"}))
        }
    });
    let defaults = call(&transport, "run_ui_test", json!({"test_id":"7"})).await;
    let text = defaults.as_str().expect("text");
    assert!(
        text.starts_with("Current default parameters:\n  • CPU Quota: 2"),
        "{text}"
    );
    assert!(text.contains("    - aws-west (us-west-2)"));
    assert!(text.ends_with("specify any parameters you want to override."));

    let invalid = call(
        &transport,
        "run_ui_test",
        json!({"test_name":"checkout","cloud_settings":"mars"}),
    )
    .await;
    assert!(
        invalid
            .as_str()
            .expect("text")
            .starts_with("❌ Invalid location/cloud_settings: 'mars'")
    );

    let started = call(
        &transport,
        "run_ui_test",
        json!({"test_name":"checkout","cloud_settings":"AWS","loops":"3"}),
    )
    .await;
    assert_eq!(
        started,
        json!(
            "✅ UI test started successfully!\nResult ID: r-1\nLocation used: AWS\nLink to report: https://carrier.example.test/-/performance/ui/results?result_id=r-1"
        )
    );
    let run = transport
        .requests()
        .into_iter()
        .rfind(|request| request.method == Method::POST)
        .expect("one run");
    assert_eq!(run.url, url("ui_performance/test/12/7"));
    let body: Value = serde_json::from_str(run.body.as_deref().expect("body")).expect("JSON");
    assert_eq!(body["loops"], json!("3"));
    assert_eq!(body["common_params"]["location"], json!("aws-west"));
    assert_eq!(
        body["common_params"]["env_vars"]["cloud_settings"]["ec2_instance_type"],
        json!("t2.xlarge")
    );
    assert_eq!(body["common_params"]["env_vars"]["cpu_quota"], json!(2));
    assert_eq!(
        body["common_params"]["name"],
        json!({"name":"test_name","default":"Checkout_Flow"})
    );
    assert_eq!(
        body["integrations"]["system"]["s3_integration"],
        json!({"integration_id":3,"is_local":false})
    );

    let missing = call(&transport, "run_ui_test", json!({})).await;
    assert!(
        missing["message"]["text"]
            .as_str()
            .expect("text")
            .contains("- ID: 7, Name: Checkout_Flow, Runner: Lighthouse-NPM_V12")
    );
}

#[tokio::test]
async fn schedule_update_keeps_existing_schedules_and_appends_one() {
    let transport = FixtureTransport::new(|request| {
        if request.url.ends_with("/ui_performance/tests/12") {
            ok(&ui_tests_fixture())
        } else if request.method == Method::GET {
            ok(&ui_details_fixture())
        } else {
            ok(&json!({"ok":true}))
        }
    });
    let invalid = call(
        &transport,
        "update_ui_test_schedule",
        json!({"test_id":"7","schedule_name":"daily","cron_timer":"every day"}),
    )
    .await;
    assert!(
        invalid
            .as_str()
            .expect("text")
            .starts_with("# ❌ Invalid Cron Timer Format")
    );
    let missing = call(
        &transport,
        "update_ui_test_schedule",
        json!({"test_id":"7"}),
    )
    .await;
    assert!(
        missing
            .as_str()
            .expect("text")
            .contains("- **`schedule_name`**\n- **`cron_timer`**")
    );
    let updated = call(
        &transport,
        "update_ui_test_schedule",
        json!({"test_id":"7","schedule_name":"daily","cron_timer":"0 9 * * 1-5"}),
    )
    .await;
    assert!(
        updated
            .as_str()
            .expect("text")
            .starts_with("# ✅ UI Test Schedule Updated Successfully!")
    );
    let put = transport
        .requests()
        .into_iter()
        .find(|request| request.method == Method::PUT)
        .expect("one update");
    assert_eq!(put.url, url("ui_performance/test/12/7"));
    let body: Value = serde_json::from_str(put.body.as_deref().expect("body")).expect("JSON");
    assert_eq!(body["common_params"]["test_type"], json!("perf"));
    assert_eq!(body["schedules"].as_array().map(Vec::len), Some(2));
    assert_eq!(
        body["schedules"][1],
        json!({"active":true,"cron":"0 9 * * 1-5","cron_radio":"custom","errors":{},"id":null,"name":"daily","test_params":[]})
    );
    assert_eq!(body["test_parameters"], json!([]));
}

fn create_ui_arguments() -> Value {
    json!({"message":"create","name":"Checkout","test_type":"perf","env_type":"stage","entrypoint":"c.js",
        "runner":"Sitespeed V36","repo":"https://git.example.test/ui.git","branch":"main","username":"u","password":"p",
        "cpu_quota":2,"memory_quota":4,"parallel_runners":1,"loops":1})
}

#[tokio::test]
async fn create_ui_test_reports_definite_refusals_and_keeps_ambiguity_an_error() {
    let created = FixtureTransport::new(|_| ok(&json!({"id":44})));
    let result = call(&created, "create_ui_test", create_ui_arguments()).await;
    assert!(
        result
            .as_str()
            .expect("text")
            .contains("- **Test ID:** `44`")
    );
    let post = &created.requests()[0];
    assert_eq!(post.url, url("ui_performance/tests/12"));
    assert_eq!(post.organization.as_deref(), Some("perf-org"));
    assert_eq!(
        post.content_type.as_deref(),
        Some("application/x-www-form-urlencoded")
    );
    let data = url_decode_data(post.body.as_deref().expect("form"));
    assert_eq!(data["common_params"]["source"]["password"], json!("p"));
    assert!(
        data["common_params"]["env_vars"]
            .get("custom_cmd")
            .is_none()
    );

    let refused = FixtureTransport::new(|_| {
        CarrierHttpResponse::fixture_text(StatusCode::BAD_REQUEST, "[{\"loc\":[\"name\"]}]")
    });
    let result = call(&refused, "create_ui_test", create_ui_arguments()).await;
    assert!(
        result
            .as_str()
            .expect("text")
            .contains("Request to https://carrier.example.test/api/v1/ui_performance/tests/12 failed with status 400")
    );

    let ambiguous = FixtureTransport::new(|_| {
        CarrierHttpResponse::fixture_text(StatusCode::BAD_GATEWAY, "upstream")
    });
    let error = tool(&ambiguous, "create_ui_test")
        .await
        .execute(context(), create_ui_arguments())
        .await
        .expect_err("an ambiguous effect is an error, not a failure report");
    assert!(!error.is_retryable());
}

#[tokio::test]
async fn cancel_ui_test_lists_running_tests_and_cancels_one_by_id() {
    let transport = FixtureTransport::new(|request| {
        if request.method == Method::PUT {
            ok(&json!({"ok":true}))
        } else {
            ok(&json!({"rows":[
                {"id":11,"name":"Checkout","test_status":{"status":"In progress","percentage":40,"description":"running"}},
                {"id":12,"name":"Done","test_status":{"status":"Finished"}}
            ]}))
        }
    });
    let listing = call(
        &transport,
        "cancel_ui_test",
        json!({"message":"Cancel UI test"}),
    )
    .await;
    let text = listing.as_str().expect("text");
    assert!(text.contains("### 🔸 Test ID: `11`\n- **Name:** `Checkout`\n- **Status:** `In progress`\n- **Progress:** 40%"));
    assert!(!text.contains("`12`"));
    let final_state = call(
        &transport,
        "cancel_ui_test",
        json!({"message":"cancel ui test 12"}),
    )
    .await;
    assert!(
        final_state
            .as_str()
            .expect("text")
            .starts_with("# ❌ Cannot Cancel Test")
    );
    let canceled = call(
        &transport,
        "cancel_ui_test",
        json!({"message":"please Cancel  UI  test 11"}),
    )
    .await;
    assert!(
        canceled
            .as_str()
            .expect("text")
            .starts_with("# ✅ UI Test Canceled Successfully!")
    );
    let put = transport
        .requests()
        .into_iter()
        .find(|request| request.method == Method::PUT)
        .expect("one cancel");
    assert_eq!(put.url, url("ui_performance/report_status/12/11"));
    assert_eq!(
        serde_json::from_str::<Value>(put.body.as_deref().expect("body")).expect("JSON"),
        json!({"test_status":{"status":"Canceled","percentage":100,"description":"Test was canceled"}})
    );
}

#[tokio::test]
async fn unknown_arguments_and_provider_failures_are_redacted_errors() {
    let transport = FixtureTransport::new(|_| {
        CarrierHttpResponse::fixture_text(
            StatusCode::UNAUTHORIZED,
            "token carrier-super-secret rejected",
        )
    });
    let tool = tool(&transport, "get_tests").await;
    let error = tool
        .execute(context(), json!({"unexpected":true}))
        .await
        .expect_err("an undeclared argument is refused");
    assert_eq!(error.code, "tool.execution.invalid_input");
    let error = tool
        .execute(context(), json!({}))
        .await
        .expect_err("a 401 is an error");
    assert!(!format!("{error:?}").contains("carrier-super-secret"));
    assert_eq!(transport.requests().len(), 1);
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let transport = FixtureTransport::new(|_| ok(&json!({})));
    let client = CarrierClient::with_transport(config(), transport as Arc<dyn CarrierTransport>);
    let toolset = test_build_with_client("gate", &[], &policy(), client).expect("Carrier toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Carrier tools");
    assert_eq!(tools.len(), 15);
    for tool in &tools {
        assert!(
            tool.description().ends_with("\nToolkit: gate"),
            "{}",
            tool.name()
        );
    }
    super::sdk_conformance::assert_sdk_conformance("carrier", &tools);
}
