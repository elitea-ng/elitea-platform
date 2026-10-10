use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::figma::client::{
    FigmaClient, FigmaClientError, FigmaHttpResponse, FigmaTransport,
};
use super::families::figma::config::{FigmaConfigErrorCode, FigmaToolkitConfig};
use super::families::figma::tools::{
    FigmaToolsetErrorCode, test_build_with_client, test_served_selection,
};
use super::policy::ToolAdmissionPolicy;

fn settings(extra: &Value) -> Map<String, Value> {
    let mut settings = json!({
        "figma_configuration":{"token":"figma-super-secret"},
        "selected_tools":[]
    })
    .as_object()
    .cloned()
    .expect("Figma fixture settings are an object");
    if let Some(extra) = extra.as_object() {
        settings.extend(extra.clone());
    }
    settings
}

fn config() -> FigmaToolkitConfig {
    FigmaToolkitConfig::parse(&settings(&json!({}))).expect("valid Figma configuration")
}

fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("Figma policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("figma-test").with_function_call_id("figma-call"))
}

#[derive(Clone, Debug)]
struct Captured {
    method: Method,
    url: String,
    token: Option<String>,
    token_sensitive: bool,
    body: Option<Value>,
    effect: bool,
}

type Handler = dyn Fn(&Captured) -> FigmaHttpResponse + Send + Sync;

struct FixtureTransport {
    requests: Mutex<Vec<Captured>>,
    handler: Box<Handler>,
}

impl FixtureTransport {
    fn new(handler: impl Fn(&Captured) -> FigmaHttpResponse + Send + Sync + 'static) -> Arc<Self> {
        Arc::new(Self {
            requests: Mutex::new(Vec::new()),
            handler: Box::new(handler),
        })
    }

    fn requests(&self) -> Vec<Captured> {
        self.requests.lock().expect("Figma fixture lock").clone()
    }
}

#[async_trait]
impl FigmaTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<FigmaHttpResponse, FigmaClientError> {
        let token = request.headers().get("x-figma-token");
        let captured = Captured {
            method: request.method().clone(),
            url: request.url().to_string(),
            token: token
                .and_then(|value| value.to_str().ok())
                .map(ToOwned::to_owned),
            token_sensitive: token.is_some_and(reqwest::header::HeaderValue::is_sensitive),
            body: request
                .body()
                .and_then(reqwest::Body::as_bytes)
                .and_then(|bytes| serde_json::from_slice(bytes).ok()),
            effect,
        };
        let response = (self.handler)(&captured);
        self.requests
            .lock()
            .expect("Figma fixture lock")
            .push(captured);
        Ok(response)
    }
}

fn ok(body: &Value) -> FigmaHttpResponse {
    FigmaHttpResponse::fixture(StatusCode::OK, body)
}

async fn tool_with(
    transport: &Arc<FixtureTransport>,
    config: FigmaToolkitConfig,
    name: &str,
) -> Arc<dyn Tool> {
    let client =
        FigmaClient::with_transport(config, Arc::clone(transport) as Arc<dyn FigmaTransport>);
    let toolset = test_build_with_client("design", &[name.to_owned()], &policy(), client)
        .expect("Figma toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Figma tools");
    assert_eq!(tools.len(), 1, "{name}");
    Arc::clone(&tools[0])
}

async fn call(transport: &Arc<FixtureTransport>, name: &str, arguments: Value) -> Value {
    tool_with(transport, config(), name)
        .await
        .execute(context(), arguments)
        .await
        .expect("Figma tool result")
}

fn file_fixture() -> Value {
    json!({
        "name":"Checkout","lastModified":"2026-09-01T10:00:00Z","thumbnailUrl":"https://thumb",
        "schemaVersion":0,"styles":{},"components":{},"version":"77",
        "document":{"id":"0:0","name":"Document","type":"DOCUMENT","children":[
            {"id":"1:2","name":"Page é","type":"CANVAS","children":[],"fills":[{"type":"SOLID"}],"strokes":[]}
        ]}
    })
}

#[test]
fn configuration_requires_a_token_and_a_compiling_global_regexp() {
    let parsed = config();
    assert!(parsed.selected_tools().is_empty());
    let rendered = format!(
        "{:?}",
        FigmaToolkitConfig::parse(&settings(&json!({}))).err()
    );
    assert!(!rendered.contains("figma-super-secret"));
    for (invalid, expected) in [
        (
            json!({"figma_configuration":{}}),
            FigmaConfigErrorCode::InvalidConfiguration,
        ),
        (
            json!({"figma_configuration":{"token":""}}),
            FigmaConfigErrorCode::InvalidConfiguration,
        ),
        (
            json!({"global_regexp":"(unclosed"}),
            FigmaConfigErrorCode::InvalidConfiguration,
        ),
        (
            json!({"global_limit":0}),
            FigmaConfigErrorCode::InvalidConfiguration,
        ),
        (
            json!({"figma_configuration":{"token":"x".repeat(16 * 1_024 + 1)}}),
            FigmaConfigErrorCode::ResourceExhausted,
        ),
    ] {
        let Err(error) = FigmaToolkitConfig::parse(&settings(&invalid)) else {
            panic!("{invalid} must be refused");
        };
        assert_eq!(error.code(), expected, "{invalid}");
    }
    FigmaToolkitConfig::parse(&settings(
        &json!({"global_regexp":"\"x\"(?=,)","global_limit":10}),
    ))
    .expect("a lookahead global regexp compiles, as in Python");
}

#[test]
fn selection_omits_the_analyzer_and_indexing_tools() {
    let names = |values: &[&str]| {
        values
            .iter()
            .map(|value| Box::<str>::from(*value))
            .collect::<Vec<_>>()
    };
    assert_eq!(
        test_served_selection(&names(&[
            "get_file",
            "analyze_file",
            "index_data",
            "search_index"
        ]))
        .expect("mixed selection"),
        vec!["get_file".to_owned()]
    );
    for refused in [names(&["analyze_file"]), names(&["get_file", "not_figma"])] {
        let Err(error) = test_served_selection(&refused) else {
            panic!("{refused:?} must be refused");
        };
        assert_eq!(error.code(), FigmaToolsetErrorCode::UnsupportedSelection);
    }
}

#[tokio::test]
async fn get_file_sends_named_query_parameters_and_answers_python_json() {
    let transport = FixtureTransport::new(|_| ok(&file_fixture()));
    let result = call(
        &transport,
        "get_file",
        json!({"file_key":"Fp24/../x","geometry":"paths","version":"depth=1"}),
    )
    .await;
    let request = &transport.requests()[0];
    assert_eq!(
        request.url,
        "https://api.figma.com/v1/files/Fp24%2F..%2Fx?geometry=paths&depth=1"
    );
    assert_eq!(request.token.as_deref(), Some("figma-super-secret"));
    assert!(request.token_sensitive);
    let text = result.as_str().expect("process_output answers text");
    assert!(
        text.contains("\"last_modified\": \"2026-09-01T10:00:00Z\""),
        "{text}"
    );
    assert!(text.contains("\"schema_version\": 0"));
    assert!(
        text.contains("Page \\u00e9"),
        "ensure_ascii escapes non-ASCII: {text}"
    );
    assert!(
        !text.contains("\"version\""),
        "FigmaPy's File keeps seven attributes"
    );
}

#[tokio::test]
async fn extra_params_limit_reduces_and_regexp_strips_like_the_sdk() {
    let transport = FixtureTransport::new(|_| ok(&file_fixture()));
    let reduced = call(
        &transport,
        "get_file",
        json!({"file_key":"k","extra_params":{"limit":"250","depth_start":2,"depth_end":4}}),
    )
    .await;
    let text = reduced.as_str().expect("text");
    assert!(
        text.starts_with("## NOTE:\nSize of the output exceeds limit 250. Data reducing has been applied. Starting from the depth_start = 2 the following object fields were removed: []. The following fields were retained: ['children', 'id', 'name', 'type']."),
        "{text}"
    );
    assert_eq!(text.chars().count(), 250);

    let stripped = call(
        &transport,
        "get_file",
        json!({"file_key":"k","extra_params":{"regexp":"\"strokes\": \\[\\](?=[,}])"}}),
    )
    .await;
    let text = stripped.as_str().expect("text");
    assert!(
        !text.contains("strokes"),
        "the lookahead pattern applied: {text}"
    );
    serde_json::from_str::<Value>(text)
        .expect("the SDK's comma repair leaves valid JSON wherever the field sat");

    let invalid = call(
        &transport,
        "get_file",
        json!({"file_key":"k","extra_params":{"regexp":"(unclosed"}}),
    )
    .await;
    assert!(
        invalid
            .as_str()
            .expect("text")
            .starts_with("Error in 'get_file': ")
    );
}

#[tokio::test]
async fn comments_images_projects_and_files_keep_figmapy_shapes() {
    let transport = FixtureTransport::new(|request| {
        if request.url.contains("/comments") {
            ok(
                &json!({"comments":[{"id":"c1","file_key":"k","parent_id":"","user":{"handle":"ann"},
                "created_at":"t","resolved_at":null,"message":"hi","client_meta":null,"order_id":"1","reactions":[]}]}),
            )
        } else if request.url.contains("/images/") {
            ok(&json!({"err":null,"images":{"1:2":"https://img"},"status":200}))
        } else if request.url.contains("/teams/") {
            ok(&json!({}))
        } else {
            ok(&json!({"name":"Team","files":[{"key":"k","name":"File"}]}))
        }
    });
    let comments = call(&transport, "get_file_comments", json!({"file_key":"k"})).await;
    let comments: Value =
        serde_json::from_str(comments.as_str().expect("text")).expect("JSON text");
    assert_eq!(comments["comments"][0]["user"], json!({"handle":"ann"}));
    assert!(comments["comments"][0].get("reactions").is_none());

    let images = call(
        &transport,
        "get_file_images",
        json!({"file_key":"k","ids":"1:2,3:4","scale":"2","format":"svg"}),
    )
    .await;
    assert_eq!(
        serde_json::from_str::<Value>(images.as_str().expect("text")).expect("JSON"),
        json!({"err":null,"images":{"1:2":"https://img"}})
    );
    assert_eq!(
        transport.requests()[1].url,
        "https://api.figma.com/v1/images/k?ids=1%3A2%2C3%3A4&scale=2&format=svg"
    );
    let empty = call(&transport, "get_team_projects", json!({"team_id":"t"})).await;
    assert_eq!(
        empty,
        json!("{\"projects\": null}"),
        "a projected object with a null attribute is not empty"
    );
    let files = call(&transport, "get_project_files", json!({"project_id":"p"})).await;
    assert_eq!(
        files,
        json!("{\"files\": [{\"key\": \"k\", \"name\": \"File\"}]}")
    );
}

#[tokio::test]
async fn nodes_answer_raw_json_and_an_empty_answer_is_the_sdk_message() {
    let transport = FixtureTransport::new(|_| ok(&json!({})));
    let result = call(
        &transport,
        "get_file_nodes",
        json!({"file_key":"k","ids":"1:2"}),
    )
    .await;
    assert_eq!(
        result,
        json!("Response result is empty. Check your input parameters or credentials")
    );
    assert_eq!(
        transport.requests()[0].url,
        "https://api.figma.com/v1/files/k/nodes?ids=1%3A2"
    );
}

#[tokio::test]
async fn post_comment_is_one_effect_with_the_sdk_payload() {
    let transport = FixtureTransport::new(|_| ok(&json!({"id":"c9","message":"Looks good"})));
    let result = call(
        &transport,
        "post_file_comment",
        json!({"file_key":"k","message":"Looks good","client_meta":{"x":1,"y":2}}),
    )
    .await;
    assert_eq!(
        result,
        json!("{\"id\": \"c9\", \"message\": \"Looks good\"}")
    );
    let request = &transport.requests()[0];
    assert_eq!(request.method, Method::POST);
    assert!(request.effect);
    assert_eq!(
        request.body,
        Some(json!({"message":"Looks good","client_meta":{"x":1,"y":2}}))
    );

    let ambiguous = FixtureTransport::new(|_| {
        FigmaHttpResponse::fixture_text(StatusCode::BAD_GATEWAY, "figma-super-secret echoed")
    });
    let error = tool_with(&ambiguous, config(), "post_file_comment")
        .await
        .execute(context(), json!({"file_key":"k","message":"m"}))
        .await
        .expect_err("an ambiguous effect is an error");
    assert!(!error.is_retryable());
    assert!(!format!("{error:?}").contains("figma-super-secret"));
}

fn node_fixture() -> Value {
    json!({"nodes":{"169:14446":{"document":{
        "id":"169:14446","name":"Formula","type":"SECTION",
        "fills":[
            {"type":"SOLID","color":{"r":0.1098,"g":0.4588,"b":0.4157,"a":1.0}},
            {"type":"IMAGE"},
            {"type":"SOLID","color":{"r":0,"g":0,"b":0,"a":0}}
        ],
        "children":[
            {"id":"1","name":"Card","type":"FRAME",
             "fills":[
                {"type":"SOLID","color":{"r":0.1098,"g":0.4588,"b":0.4157,"a":1.0},"opacity":0.5},
                {"type":"GRADIENT_LINEAR","gradientStops":[{"color":{"r":1,"g":1,"b":1,"a":0.3}},{"color":{"r":0,"g":0,"b":0,"a":0}}]}
             ],
             "strokes":[{"type":"SOLID","color":{"r":0,"g":0,"b":0,"a":1}}],
             "effects":[
                {"type":"DROP_SHADOW","visible":true,"radius":6,"offset":{"x":0,"y":2},"color":{"r":0,"g":0,"b":0,"a":0.25}},
                {"type":"DROP_SHADOW","visible":false,"radius":9}
             ],
             "children":[
                {"id":"2","name":"Title","type":"TEXT","style":{"fontFamily":"Noto Sans","fontSize":14.0,"fontWeight":400,"lineHeightPx":20,"italic":false}},
                {"id":"3","name":"Body","type":"TEXT","style":{"fontFamily":"Noto Sans","fontSize":14,"fontWeight":400}},
                {"id":"4","name":"Plain","type":"GROUP"}
             ]}
        ]
    }}}})
}

#[tokio::test]
async fn design_tokens_follow_the_sdk_extraction_and_dedup() {
    let transport = FixtureTransport::new(|_| ok(&node_fixture()));
    let tokens = call(
        &transport,
        "extract_design_tokens",
        json!({"file_key":"Fp24","node_id":"169-14446","depth":6}),
    )
    .await;
    assert_eq!(
        transport.requests()[0].url,
        "https://api.figma.com/v1/files/Fp24/nodes?ids=169%3A14446&depth=6"
    );
    assert_eq!(tokens["node_id"], json!("169:14446"));
    assert_eq!(tokens["node_type"], json!("SECTION"));
    assert_eq!(
        tokens["colors"],
        json!([
            {"hex":"#1C756A","alpha":1.0,"opacity":1.0,"source_path":"Formula"},
            {"hex":"#FFFFFF","alpha":0.3,"opacity":1.0,"source_path":"Formula/Card [GRADIENT_LINEAR]"}
        ])
    );
    assert_eq!(
        tokens["strokes"],
        json!([{"hex":"#000000","alpha":1,"source_path":"Formula/Card"}])
    );
    assert_eq!(
        tokens["typography"],
        json!([{"fontFamily":"Noto Sans","fontSize":14.0,"fontWeight":400,"lineHeightPx":20,"source_path":"Formula/Card/Title"}])
    );
    assert_eq!(tokens["effects"].as_array().map(Vec::len), Some(1));
    assert_eq!(
        tokens["summary"],
        json!({"total_style_entries":4,"unique_colors":2,"unique_fonts":1,"unique_effects":1,"unique_strokes":1})
    );

    let page = FixtureTransport::new(|_| ok(&json!({"nodes":{"1:2":null}})));
    let result = call(
        &page,
        "extract_design_tokens",
        json!({"file_key":"k","node_id":"1:2"}),
    )
    .await;
    assert!(
        result
            .as_str()
            .expect("text")
            .starts_with("Node '1:2' returned null.")
    );
    assert_eq!(
        page.requests()[0].url,
        "https://api.figma.com/v1/files/k/nodes?ids=1%3A2&depth=4"
    );
}

#[tokio::test]
async fn batch_formats_match_the_sdk_shapes_and_keep_entry_order() {
    let transport = FixtureTransport::new(|request| {
        if request.url.contains("broken") {
            FigmaHttpResponse::fixture_text(StatusCode::NOT_FOUND, "missing")
        } else {
            ok(&node_fixture())
        }
    });
    let entries = json!([
        {"file_key":"Fp24","node_id":"169:14446"},
        {"file_key":"broken","node_id":"169:14446","depth":99},
        {"file_key":"Fp24","node_id":null},
        {"file_key":"Fp24","node_id":"169-14446","depth":6}
    ]);
    let full = call(
        &transport,
        "extract_design_tokens_batch",
        json!({"entries":entries}),
    )
    .await;
    assert_eq!(
        full["batch_summary"],
        json!({"total_entries":4,"processed_entries":3,"successful":2,"failed":1,
               "total_colors":4,"total_strokes":2,"total_fonts":2,"total_effects":2})
    );
    let indexes = full["results"]
        .as_array()
        .expect("results")
        .iter()
        .map(|result| result["entry_index"].clone())
        .collect::<Vec<_>>();
    assert_eq!(indexes, vec![json!(0), json!(1), json!(3)]);
    assert_eq!(full["results"][1]["status"], json!("error"));
    assert!(
        full["results"][1]["error"]
            .as_str()
            .expect("error")
            .starts_with("Failed to fetch node '169:14446' from file 'broken': ")
    );

    let compact = call(
        &transport,
        "extract_design_tokens_batch",
        json!({"entries":entries,"output_format":"compact","parallel":false}),
    )
    .await;
    assert_eq!(compact["results"][0]["color_count"], json!(2));
    assert!(compact["results"][0].get("colors").is_none());
    let summary = call(
        &transport,
        "extract_design_tokens_batch",
        json!({"entries":entries,"output_format":"summary"}),
    )
    .await;
    assert_eq!(summary.as_object().map(Map::len), Some(1));
    let guide = call(
        &transport,
        "extract_design_tokens_batch",
        json!({"entries":entries,"output_format":"style_guide"}),
    )
    .await;
    let colors = &guide["style_guide"]["tokens"]["colors"];
    assert_eq!(colors[0]["id"], json!("color_001"));
    assert_eq!(colors[0]["components_count"], json!(2));
    assert_eq!(
        guide["style_guide"]["components"][0]["tokens"]["colors"],
        json!(["color_001", "color_002"])
    );
    assert_eq!(
        guide["style_guide"]["components"][1]["status"],
        json!("error")
    );

    let empty = call(
        &transport,
        "extract_design_tokens_batch",
        json!({"entries":[]}),
    )
    .await;
    assert_eq!(empty, json!("Batch entries list cannot be empty"));
    let missing = call(
        &transport,
        "extract_design_tokens_batch",
        json!({"entries":[{"node_id":"1:2"}]}),
    )
    .await;
    assert_eq!(
        missing,
        json!(
            "Entry 0 missing required fields. Each entry must have 'file_key' and 'node_id'. Got: ['node_id']"
        )
    );
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let transport = FixtureTransport::new(|_| ok(&json!({})));
    let client = FigmaClient::with_transport(config(), transport as Arc<dyn FigmaTransport>);
    let toolset = test_build_with_client("gate", &[], &policy(), client).expect("Figma toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Figma tools");
    assert_eq!(tools.len(), 10);
    for tool in &tools {
        assert!(
            tool.description().starts_with("Toolkit: gate\n"),
            "{}",
            tool.name()
        );
        assert!(tool.description().chars().count() <= 1_000);
    }
    super::sdk_conformance::assert_sdk_conformance("figma", &tools);
}
