use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use async_trait::async_trait;
use reqwest::{Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::openapi::client::{
    OpenApiAccessToken, OpenApiApi, OpenApiClient, OpenApiClientError, OpenApiClientErrorCode,
    OpenApiRequest, OpenApiResponse, OpenApiTransport,
};
use super::families::openapi::config::OpenApiToolkitConfig;
use super::families::openapi::spec::{
    MAX_SCHEMA_EXPANSION_NODES, MAX_SPEC_EXPANSION_NODES, OpenApiSpecErrorCode, parse_operations,
};

fn spec_error(spec: &Value) -> OpenApiSpecErrorCode {
    parse_operations(spec, None, &[])
        .err()
        .expect("specification must be refused")
        .code()
}

/// YAML specification text with one anchored list referenced `references` times.
fn yaml_spec_with_shared_list(items: usize, references: usize) -> Value {
    let mut yaml = String::from(
        "openapi: 3.0.3\nservers: [{url: 'https://api.example.test'}]\npaths:\n  /items:\n    get:\n      responses: {'200': {description: ok}}\n",
    );
    yaml.push_str("x-shared: &shared [");
    yaml.push_str(&"v,".repeat(items));
    yaml.push_str("]\nx-repeated: [");
    yaml.push_str(&"*shared,".repeat(references));
    yaml.push_str("]\n");
    Value::String(yaml)
}

/// One GET operation per response schema, with all schemas under `components`.
fn spec_with_responses(responses: &[Value], schemas: &Map<String, Value>) -> Value {
    let mut paths = Map::new();
    for (index, schema) in responses.iter().enumerate() {
        paths.insert(
            format!("/items{index}"),
            json!({"get":{"responses":{"200":{
                "description":"ok",
                "content":{"application/json":{"schema":schema}}
            }}}}),
        );
    }
    json!({
        "openapi":"3.0.3",
        "servers":[{"url":"https://api.example.test"}],
        "paths":paths,
        "components":{"schemas":schemas}
    })
}

/// A schema whose response walk visits exactly `visits` schemas: itself and its empty branches.
fn schema_of_visits(visits: usize) -> Value {
    json!({"allOf": vec![json!({}); visits - 1]})
}

fn reference(name: &str) -> Value {
    json!({"$ref": format!("#/components/schemas/{name}")})
}

/// `levels` schemas, each composing `width` references to the next level.
fn layered_schemas(levels: usize, width: usize) -> Map<String, Value> {
    let mut schemas = Map::new();
    for level in 0..levels {
        let next = reference(&format!("L{}", level + 1));
        schemas.insert(format!("L{level}"), json!({"allOf": vec![next; width]}));
    }
    schemas.insert(format!("L{levels}"), json!({"type":"string"}));
    schemas
}

#[test]
fn yaml_specification_expansion_beyond_budget_is_resource_exhausted() {
    assert_eq!(
        spec_error(&yaml_spec_with_shared_list(1_000, 300)),
        OpenApiSpecErrorCode::ResourceExhausted
    );
}

#[test]
fn yaml_specification_expansion_refusal_is_fast_and_identical_on_replay() {
    let spec = yaml_spec_with_shared_list(50_000, 60_000);
    let started = Instant::now();
    let first = spec_error(&spec);
    let second = spec_error(&spec);
    let elapsed = started.elapsed();
    assert_eq!(first, OpenApiSpecErrorCode::ResourceExhausted);
    assert_eq!(first, second);
    assert!(
        elapsed < Duration::from_secs(3),
        "two refusals took {elapsed:?}"
    );
}

#[test]
fn layered_schema_references_beyond_budget_are_resource_exhausted() {
    let spec = spec_with_responses(&[reference("L0")], &layered_schemas(3, 70));
    let started = Instant::now();
    assert_eq!(spec_error(&spec), OpenApiSpecErrorCode::ResourceExhausted);
    assert!(started.elapsed() < Duration::from_secs(3));
}

#[test]
fn layered_parameter_schema_references_beyond_budget_are_resource_exhausted() {
    let spec = json!({
        "openapi":"3.0.3",
        "servers":[{"url":"https://api.example.test"}],
        "paths":{"/items":{"get":{
            "parameters":[{"name":"filter","in":"query","schema":reference("L0")}],
            "responses":{"200":{"description":"ok"}}
        }}},
        "components":{"schemas":layered_schemas(10, 1_000)}
    });
    let started = Instant::now();
    assert_eq!(spec_error(&spec), OpenApiSpecErrorCode::ResourceExhausted);
    assert!(started.elapsed() < Duration::from_secs(3));
}

#[test]
fn single_schema_walk_budget_holds_at_limit_and_refuses_limit_plus_one() {
    let spec = |visits| {
        let mut schemas = Map::new();
        schemas.insert("Single".to_owned(), schema_of_visits(visits));
        spec_with_responses(&[reference("Single")], &schemas)
    };
    assert!(parse_operations(&spec(MAX_SCHEMA_EXPANSION_NODES), None, &[]).is_ok());
    assert_eq!(
        spec_error(&spec(MAX_SCHEMA_EXPANSION_NODES + 1)),
        OpenApiSpecErrorCode::ResourceExhausted
    );
}

#[test]
fn specification_budget_holds_at_limit_and_refuses_limit_plus_one() {
    let per_operation = MAX_SCHEMA_EXPANSION_NODES;
    let operations = MAX_SPEC_EXPANSION_NODES / per_operation;
    assert_eq!(operations * per_operation, MAX_SPEC_EXPANSION_NODES);
    let mut schemas = Map::new();
    schemas.insert("Shared".to_owned(), schema_of_visits(per_operation));
    let mut responses = vec![reference("Shared"); operations];
    assert!(parse_operations(&spec_with_responses(&responses, &schemas), None, &[]).is_ok());
    responses.push(json!({"type":"string"}));
    assert_eq!(
        spec_error(&spec_with_responses(&responses, &schemas)),
        OpenApiSpecErrorCode::ResourceExhausted
    );
}

#[test]
fn recursive_response_schemas_are_accepted() {
    let mut schemas = Map::new();
    schemas.insert(
        "Node".to_owned(),
        json!({"type":"object","properties":{
            "name":{"type":"string"},
            "children":{"type":"object","properties":{"items":{"type":"array","items":reference("Node")}}},
            "parent":reference("Node")
        }}),
    );
    let parsed = parse_operations(
        &spec_with_responses(&[reference("Node")], &schemas),
        None,
        &[],
    )
    .expect("recursive response schema");
    assert_eq!(
        parsed.operations[0].response_collection_paths(),
        &[
            vec!["children".to_owned(), "items".to_owned()],
            vec![
                "parent".to_owned(),
                "children".to_owned(),
                "items".to_owned()
            ],
            vec![
                "parent".to_owned(),
                "parent".to_owned(),
                "children".to_owned(),
                "items".to_owned()
            ],
        ]
    );
}

#[test]
fn template_dot_segments_are_refused_when_the_specification_is_parsed() {
    for path in ["/v1/./items", "/v1/%2e%2e/items", "/v1/%2E/items"] {
        let spec = json!({
            "openapi":"3.0.3",
            "servers":[{"url":"https://api.example.test"}],
            "paths":{path:{"get":{"responses":{"200":{"description":"ok"}}}}}
        });
        assert_eq!(
            spec_error(&spec),
            OpenApiSpecErrorCode::InvalidSpecification,
            "{path}"
        );
    }
}

#[test]
fn shared_schema_references_within_budget_still_resolve() {
    let mut schemas = Map::new();
    schemas.insert(
        "Item".to_owned(),
        json!({"type":"object","properties":{"id":{"type":"string"}}}),
    );
    schemas.insert(
        "Page".to_owned(),
        json!({"type":"object","properties":{
            "items":{"type":"array","items":reference("Item")},
            "first":reference("Item"),
            "last":reference("Item")
        }}),
    );
    let parsed = parse_operations(
        &spec_with_responses(&[reference("Page")], &schemas),
        None,
        &[],
    )
    .expect("shared references resolve");
    assert_eq!(
        parsed.operations[0].response_collection_paths(),
        &[vec!["items".to_owned()]]
    );
}

struct CapturingTransport {
    requests: Mutex<Vec<OpenApiRequest>>,
}

#[async_trait]
impl OpenApiTransport for CapturingTransport {
    async fn execute(
        &self,
        request: OpenApiRequest,
    ) -> Result<OpenApiResponse, OpenApiClientError> {
        self.requests
            .lock()
            .expect("captured requests")
            .push(request);
        Ok(OpenApiResponse {
            status: StatusCode::OK,
            body: b"{}".to_vec(),
        })
    }

    async fn token(&self, _request: Request) -> Result<OpenApiAccessToken, OpenApiClientError> {
        unreachable!("no token flow is configured")
    }
}

async fn call_with(
    path: &str,
    parameters: &Value,
    arguments: &Value,
) -> Result<String, OpenApiClientErrorCode> {
    let settings = json!({
        "openapi_configuration":{},
        "spec":{
            "openapi":"3.0.3",
            "servers":[{"url":"https://api.example.test/v1"}],
            "paths":{path:{"get":{
                "operationId":"read_item",
                "parameters":parameters,
                "responses":{"200":{"description":"ok"}}
            }}}
        },
        "selected_tools":["read_item"]
    });
    let config = OpenApiToolkitConfig::parse(
        "Items API",
        settings.as_object().expect("settings object"),
        &Map::new(),
    )
    .expect("parameter configuration");
    let operation = config.operations()[0].clone();
    let transport = Arc::new(CapturingTransport {
        requests: Mutex::new(Vec::new()),
    });
    let client = OpenApiClient::with_transport(config.into_client_parts(), transport.clone());
    let result = client
        .execute(&operation, arguments.as_object().expect("arguments object"))
        .await;
    let requests = transport.requests.lock().expect("captured requests");
    match result {
        Ok(_) => Ok(requests[0].uri().to_string()),
        Err(error) => {
            assert!(
                requests.is_empty(),
                "a refused call must not send a request"
            );
            Err(error.code())
        }
    }
}

/// Call `path` with every argument bound as a required string path parameter.
async fn call(path: &str, arguments: &Value) -> Result<String, OpenApiClientErrorCode> {
    let parameters = arguments
        .as_object()
        .expect("arguments object")
        .keys()
        .map(|name| json!({"name":name,"in":"path","required":true,"schema":{"type":"string"}}))
        .collect::<Vec<_>>();
    call_with(path, &Value::Array(parameters), arguments).await
}

#[tokio::test]
async fn path_values_stay_inside_one_encoded_segment() {
    for (value, expected) in [
        (
            "Ada/Lovelace",
            "https://api.example.test/v1/items/Ada%2FLovelace",
        ),
        ("v1.2", "https://api.example.test/v1/items/v1.2"),
        ("...", "https://api.example.test/v1/items/..."),
        ("%2F", "https://api.example.test/v1/items/%252F"),
        ("a b?c#d", "https://api.example.test/v1/items/a%20b%3Fc%23d"),
        ("é", "https://api.example.test/v1/items/%C3%A9"),
    ] {
        assert_eq!(
            call("/items/{id}", &json!({"id":value})).await.as_deref(),
            Ok(expected),
            "{value:?}"
        );
    }
}

#[tokio::test]
async fn path_values_that_would_change_the_selected_path_are_refused() {
    for value in [
        "",
        ".",
        "..",
        "%2e",
        "%2e%2e",
        "%2E.",
        ".%2E",
        "../admin",
        "a/../b",
        "a/./b",
        "..\\admin",
        "a%2f..%2fb",
        "a\r\nb",
        "a\nb",
        "a\u{0}b",
        "a\u{85}b",
        "a\u{2028}b",
        "a\u{2029}b",
        "\u{ff0e}\u{ff0e}",
        "\u{2025}",
        "a\u{ff0f}b",
        "a\u{ff3c}b",
    ] {
        assert_eq!(
            call("/items/{id}", &json!({"id":value})).await,
            Err(OpenApiClientErrorCode::InvalidInput),
            "{value:?}"
        );
    }
}

#[tokio::test]
async fn adjacent_path_values_stay_within_their_segment() {
    for (first, second) in [(".", "."), ("", ".."), (".", "")] {
        assert_eq!(
            call(
                "/items/{first}{second}",
                &json!({"first":first,"second":second})
            )
            .await,
            Err(OpenApiClientErrorCode::InvalidInput),
            "{first:?} {second:?}"
        );
    }
    assert_eq!(
        call(
            "/items/{first}{second}",
            &json!({"first":"v1","second":".2"})
        )
        .await,
        Ok("https://api.example.test/v1/items/v1.2".to_owned())
    );
}

#[tokio::test]
async fn two_path_values_stay_within_the_base_path() {
    assert_eq!(
        call(
            "/items/{first}/{second}",
            &json!({"first":"..","second":".."})
        )
        .await,
        Err(OpenApiClientErrorCode::InvalidInput)
    );
}

#[tokio::test]
async fn query_values_are_encoded_and_header_values_refuse_line_breaks() {
    assert_eq!(
        call_with(
            "/items",
            &json!([{"name":"q","in":"query","schema":{"type":"string"}}]),
            &json!({"q":"a\r\nb/../c"}),
        )
        .await,
        Ok("https://api.example.test/v1/items?q=a%0D%0Ab%2F..%2Fc".to_owned())
    );
    assert_eq!(
        call_with(
            "/items",
            &json!([{"name":"X-Item","in":"header","schema":{"type":"string"}}]),
            &json!({"X-Item":"a\r\nX-Other: b"}),
        )
        .await,
        Err(OpenApiClientErrorCode::InvalidInput)
    );
}
