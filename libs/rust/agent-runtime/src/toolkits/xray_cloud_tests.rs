use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_core::{ErrorCategory, ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::header::AUTHORIZATION;
use reqwest::{Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::xray_cloud::client::{
    XrayApi, XrayClient, XrayClientError, XrayHttpResponse, XrayTransport,
};
use super::families::xray_cloud::config::{XrayConfigErrorCode, XrayToolkitConfig};
use super::families::xray_cloud::tools::{
    XrayToolsetErrorCode, build_xray_cloud_toolset, test_build_with_api,
};
use super::policy::ToolAdmissionPolicy;

fn settings(limit: Option<u64>, selected_tools: &[&str]) -> Map<String, Value> {
    let mut settings = json!({
        "xray_configuration":{
            "base_url":"",
            "client_id":"client-id",
            "client_secret":"client-secret-value"
        },
        "selected_tools":selected_tools
    });
    if let Some(limit) = limit {
        settings["limit"] = json!(limit);
    }
    settings
        .as_object()
        .cloned()
        .expect("Xray fixture settings are an object")
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Xray policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("xray-test").with_function_call_id("xray-call"))
}

#[derive(Clone, Debug)]
struct Captured {
    url: String,
    authorization: Option<String>,
    body: Value,
}

type Handler = dyn Fn(&str, &Value) -> XrayHttpResponse + Send + Sync;

struct FixtureTransport {
    requests: Mutex<Vec<Captured>>,
    handler: Box<Handler>,
}

impl FixtureTransport {
    fn new(
        handler: impl Fn(&str, &Value) -> XrayHttpResponse + Send + Sync + 'static,
    ) -> Arc<Self> {
        Arc::new(Self {
            requests: Mutex::new(Vec::new()),
            handler: Box::new(handler),
        })
    }

    fn captured(&self) -> Vec<Captured> {
        self.requests.lock().expect("Xray fixture lock").clone()
    }
}

#[async_trait]
impl XrayTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        _effect: bool,
    ) -> Result<XrayHttpResponse, XrayClientError> {
        let url = request.url().as_str().to_owned();
        let body: Value = request
            .body()
            .and_then(reqwest::Body::as_bytes)
            .map(|bytes| serde_json::from_slice(bytes).expect("JSON body"))
            .expect("Xray requests carry a JSON body");
        let authorization = request.headers().get(AUTHORIZATION).map(|value| {
            assert!(value.is_sensitive());
            value.to_str().expect("ASCII").to_owned()
        });
        self.requests
            .lock()
            .expect("Xray fixture lock")
            .push(Captured {
                url: url.clone(),
                authorization,
                body: body.clone(),
            });
        Ok((self.handler)(&url, &body))
    }
}

fn ok(body: Value) -> XrayHttpResponse {
    XrayHttpResponse::fixture(StatusCode::OK, Some(body))
}

/// Xray Cloud v1 authentication and v2 GraphQL responses.
fn provider(url: &str, body: &Value) -> XrayHttpResponse {
    if url == "https://xray.cloud.getxray.app/api/v1/authenticate" {
        return ok(json!("xray-token"));
    }
    assert_eq!(url, "https://xray.cloud.getxray.app/api/v2/graphql");
    let query = body["query"].as_str().expect("GraphQL query");
    let variables = &body["variables"];
    if query.contains("GetTests(") {
        let jql = variables["jql"].as_str().expect("jql");
        if jql == "bad jql" {
            return ok(json!({"data":{"getTests":null},"errors":[{"message":"bad"}]}));
        }
        if jql == "key = \"CALC-7\"" {
            return ok(
                json!({"data":{"getTests":{"limit":1,"results":[{"issueId":"1007"}],"start":0,"total":1}}}),
            );
        }
        return match variables["start"].as_u64() {
            Some(0) => ok(json!({"data":{"getTests":{"limit":2,"results":[
                {"issueId":"1","jira":{"key":"CALC-1"},"preconditions":{"total":0}},
                {"issueId":"2","jira":{"key":"CALC-2"},"preconditions":{"results":[{"issueId":"9"}],"total":1}}
            ],"start":0,"total":3}}})),
            Some(2) => ok(json!({"data":{"getTests":{"limit":2,"results":[
                {"issueId":"3","jira":{"key":"CALC-3"},"preconditions":{"total":0}}
            ],"start":2,"total":3}}})),
            other => panic!("unexpected getTests start {other:?}"),
        };
    }
    if query.contains("GetTest(") {
        return ok(json!({"data":{"getTest":{
            "issueId":"1007",
            "jira":{"key":"CALC-7"},
            "steps":[
                {"action":"Öffnen","attachments":[{"downloadLink":"https://x/a","filename":"a.png","id":"att-1"}],"id":"step-1"},
                {"action":"Close","attachments":[],"id":"step-2"}
            ]
        }}}));
    }
    if query.contains("UpdateTestStep") {
        return ok(json!({"data":{"updateTestStep":{"warnings":["slow"]}}}));
    }
    if query.contains("FAIL") {
        return XrayHttpResponse::fixture(StatusCode::BAD_GATEWAY, None);
    }
    ok(json!({"data":{"createTest":{"test":{"issueId":"55"},"warnings":[]}}}))
}

async fn tools_with(transport: Arc<FixtureTransport>, limit: u64) -> Vec<Arc<dyn Tool>> {
    let config =
        XrayToolkitConfig::parse(&settings(Some(limit), &[])).expect("valid Xray configuration");
    let client: Arc<dyn XrayApi> = Arc::new(XrayClient::with_transport(config, transport));
    let toolset =
        test_build_with_api("qa", limit, &[], &policy(&[]), &client).expect("Xray toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    toolset.tools(readonly).await.expect("Xray tools")
}

fn tool<'a>(tools: &'a [Arc<dyn Tool>], name: &str) -> &'a Arc<dyn Tool> {
    tools
        .iter()
        .find(|tool| tool.name() == name)
        .unwrap_or_else(|| panic!("Xray tool {name}"))
}

async fn call(tools: &[Arc<dyn Tool>], name: &str, arguments: Value) -> Value {
    tool(tools, name)
        .execute(context(), arguments)
        .await
        .unwrap_or_else(|error| panic!("{name}: {error}"))
}

#[test]
fn configuration_defaults_the_cloud_origin_and_page_size() {
    let parsed = XrayToolkitConfig::parse(&settings(None, &["get_tests"])).expect("valid");
    assert_eq!(parsed.selected_tools(), [Box::<str>::from("get_tests")]);
    for invalid in [
        json!({}),
        json!({"xray_configuration":{"client_id":"a"}}),
        json!({"xray_configuration":{"base_url":"http://xray.example","client_id":"a","client_secret":"s"}}),
        json!({"xray_configuration":{"client_id":"a","client_secret":"s"},"limit":0}),
        json!({"xray_configuration":{"client_id":"a","client_secret":"s"},"limit":"10"}),
    ] {
        let Err(error) = XrayToolkitConfig::parse(invalid.as_object().expect("object")) else {
            panic!("invalid Xray configuration accepted: {invalid}");
        };
        assert_eq!(error.code(), XrayConfigErrorCode::InvalidConfiguration);
        assert!(!format!("{error:?}").contains("client-secret-value"));
    }
}

#[tokio::test]
async fn reads_authenticate_once_page_tests_and_render_sdk_text() {
    let transport = FixtureTransport::new(provider);
    let tools = tools_with(Arc::clone(&transport), 2).await;
    assert!(
        tools[0]
            .description()
            .ends_with("\nXray instance: https://xray.cloud.getxray.app")
    );
    let tests = call(&tools, "get_tests", json!({"jql":"project = CALC"})).await;
    assert_eq!(
        tests,
        json!(
            "Extracted tests (3):\n[{'issueId': '1', 'jira': {'key': 'CALC-1'}}, {'issueId': '2', 'jira': {'key': 'CALC-2'}, 'preconditions': {'results': [{'issueId': '9'}], 'total': 1}}, {'issueId': '3', 'jira': {'key': 'CALC-3'}}]"
        )
    );
    let attachments = call(
        &tools,
        "get_test_step_attachments",
        json!({"issue_id":"CALC-7"}),
    )
    .await;
    assert_eq!(
        attachments,
        json!(
            "{\n  \"test\": \"CALC-7\",\n  \"issue_id\": \"1007\",\n  \"total_attachments\": 1,\n  \"attachments\": [\n    {\n      \"id\": \"att-1\",\n      \"filename\": \"a.png\",\n      \"downloadLink\": \"https://x/a\",\n      \"step_id\": \"step-1\",\n      \"step_action\": \"\\u00d6ffnen\"\n    }\n  ]\n}"
        )
    );
    assert_eq!(
        call(
            &tools,
            "get_test_step_attachments",
            json!({"issue_id":"1007","step_id":"step-2"})
        )
        .await,
        json!("No attachments found for test CALC-7, step step-2")
    );

    let captured = transport.captured();
    assert_eq!(
        captured[0].body,
        json!({"client_id":"client-id","client_secret":"client-secret-value"})
    );
    assert_eq!(captured[0].authorization, None);
    assert_eq!(
        captured
            .iter()
            .filter(|request| request.url.ends_with("/api/v1/authenticate"))
            .count(),
        1,
        "the token is fetched once per invocation"
    );
    assert!(
        captured[1..]
            .iter()
            .all(|request| request.authorization.as_deref() == Some("Bearer xray-token"))
    );
    assert_eq!(
        captured[1].body["variables"],
        json!({"jql":"project = CALC","start":0,"limit":2})
    );
    assert_eq!(captured[2].body["variables"]["start"], 2);
    assert_eq!(
        captured[3].body["variables"],
        json!({"jql":"key = \"CALC-7\"","start":0,"limit":1})
    );
    assert_eq!(captured[4].body["variables"], json!({"issueId":"1007"}));

    let invalid = tool(&tools, "get_tests")
        .execute(context(), json!({"jql":"bad jql"}))
        .await
        .expect_err("a null getTests is refused");
    assert_eq!(invalid.category, ErrorCategory::InvalidInput);
    assert!(
        tool(&tools, "get_test_step_attachments")
            .execute(context(), json!({"issue_id":"CALC-1\" OR key = \"X"}))
            .await
            .is_err(),
        "an issue key is never spliced into JQL unchecked"
    );
}

#[tokio::test]
async fn effects_send_the_mutation_text_and_report_unknown_outcomes() {
    let transport = FixtureTransport::new(provider);
    let tools = tools_with(Arc::clone(&transport), 20).await;
    let created_text =
        "Created test case:\n{'data': {'createTest': {'test': {'issueId': '55'}, 'warnings': []}}}";
    assert_eq!(
        call(
            &tools,
            "create_test",
            json!({"graphql_mutation":"mutation { createTest(jira: {}) { warnings } }"})
        )
        .await,
        json!(created_text)
    );
    assert_eq!(
        call(
            &tools,
            "create_tests",
            json!({"graphql_mutations":["mutation { a }","mutation { b }"]})
        )
        .await,
        json!([created_text, created_text])
    );
    assert_eq!(
        call(&tools, "execute_graphql", json!({"graphql":"query { x }"})).await,
        json!(
            "Result of graphql execution:\n{'data': {'createTest': {'test': {'issueId': '55'}, 'warnings': []}}}"
        )
    );
    let attached = call(
        &tools,
        "add_attachment_to_test_step",
        json!({"step_id":"0f9c7d1e-aaaa","filedata":"log line","filename":"run.txt"}),
    )
    .await;
    assert_eq!(
        attached,
        json!(
            "Successfully added attachment 'run.txt' to step 0f9c7d1e-aaaa\nFile size: 8 bytes\nMIME type: text/plain\nWarnings: ['slow']"
        )
    );
    let captured = transport.captured();
    assert_eq!(
        captured[1].body,
        json!({"query":"mutation { createTest(jira: {}) { warnings } }"})
    );
    let upload = captured.last().expect("upload request");
    assert_eq!(
        upload.body["variables"],
        json!({"stepId":"0f9c7d1e-aaaa","step":{"attachments":{"add":[
            {"filename":"run.txt","mimeType":"text/plain","data":"bG9nIGxpbmU="}
        ]}}})
    );

    let before = transport.captured().len();
    assert_eq!(
        call(
            &tools,
            "add_attachment_to_test_step",
            json!({"step_id":"0f9c7d1e-aaaa","filepath":"/bucket/a.png"})
        )
        .await,
        json!(
            "filepath attachments are not available in this runtime: artifact storage is not readable as raw bytes here. Pass the attachment content as filedata with a filename instead."
        )
    );
    assert_eq!(
        call(
            &tools,
            "add_attachment_to_test_step",
            json!({"step_id":"CALC-1","filedata":"x","filename":"a.txt"})
        )
        .await,
        json!(
            "Invalid step_id 'CALC-1'. Step ID must be a UUID (e.g., 'a1b2c3d4-...'), not a test issue key. Use get_tests tool to retrieve step IDs from test details."
        )
    );
    assert_eq!(transport.captured().len(), before);

    let unknown = tool(&tools, "create_test")
        .execute(context(), json!({"graphql_mutation":"mutation { FAIL }"}))
        .await
        .expect_err("a 502 on an effect is an unknown outcome");
    assert_eq!(unknown.category, ErrorCategory::Internal);
    assert!(!unknown.retry.should_retry);
}

#[tokio::test]
async fn failed_authentication_is_unauthorized() {
    let transport = FixtureTransport::new(|url, _| {
        assert!(url.ends_with("/api/v1/authenticate"));
        XrayHttpResponse::fixture(StatusCode::UNAUTHORIZED, Some(json!({"error":"no"})))
    });
    let tools = tools_with(transport, 20).await;
    let error = tool(&tools, "get_tests")
        .execute(context(), json!({"jql":"project = CALC"}))
        .await
        .expect_err("bad credentials fail");
    assert_eq!(error.category, ErrorCategory::Unauthorized);
}

#[tokio::test]
async fn selection_skips_index_tools_and_the_contract_holds() {
    let config = XrayToolkitConfig::parse(&settings(None, &["index_data", "search_index"]))
        .expect("bounded selection parses");
    let Err(error) = build_xray_cloud_toolset("qa", config, &policy(&[])) else {
        panic!("an index-only selection must not build");
    };
    assert_eq!(error.code(), XrayToolsetErrorCode::UnsupportedSelection);

    let tools = tools_with(FixtureTransport::new(provider), 20).await;
    assert_eq!(
        tools
            .iter()
            .filter(|tool| tool.is_read_only())
            .map(|tool| tool.name())
            .collect::<Vec<_>>(),
        ["get_tests", "get_test_step_attachments"]
    );
    super::sdk_conformance::assert_sdk_conformance("xray_cloud", &tools);
}
