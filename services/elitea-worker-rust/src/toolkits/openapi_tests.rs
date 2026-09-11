use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Toolset};
use async_trait::async_trait;
use reqwest::header::{AUTHORIZATION, CONTENT_TYPE};
use reqwest::{Request, StatusCode};
use serde_json::{Map, Value, json};

use super::delegated_auth::delegated_authorization_requirement;
use super::families::openapi::client::{
    OpenApiAccessToken, OpenApiApi, OpenApiClient, OpenApiClientError, OpenApiRequest,
    OpenApiResponse, OpenApiTransport, oauth_token_lifetime,
};
use super::families::openapi::config::{OpenApiConfigErrorCode, OpenApiToolkitConfig};
use super::families::openapi::spec::{OpenApiSpecErrorCode, parse_operations};
use super::families::openapi::tools::build_openapi_toolset;
use super::materialize::materialize_configured_toolsets_with_tokens_and_authorization;
use super::policy::ToolAdmissionPolicy;
use super::snapshot::FrozenToolSnapshot;

fn inline_spec() -> Value {
    json!({
        "openapi":"3.0.3",
        "servers":[{"url":"https://api.example.test/v1"}],
        "paths":{
            "/users/{id}":{
                "parameters":[{
                    "name":"id",
                    "in":"path",
                    "required":true,
                    "description":"Exact user ID.",
                    "schema":{"type":"string","minLength":1,"maxLength":64}
                }],
                "get":{
                    "summary":"Read one user.",
                    "parameters":[{
                        "name":"search",
                        "in":"query",
                        "allowReserved":true,
                        "schema":{"type":"string"}
                    }],
                    "responses":{"200":{"description":"ok"}}
                }
            },
            "/users":{
                "post":{
                    "operationId":"create_user",
                    "requestBody":{
                        "required":true,
                        "content":{"application/json":{"schema":{"type":"object"}}}
                    },
                    "responses":{"201":{"description":"created"}}
                }
            }
        }
    })
}

fn settings(auth: &Value, selected: &[&str]) -> Map<String, Value> {
    json!({
        "openapi_configuration":auth.clone(),
        "spec":inline_spec(),
        "selected_tools":selected
    })
    .as_object()
    .expect("OpenAPI settings")
    .clone()
}

fn policy(blocked: &[&str]) -> Arc<ToolAdmissionPolicy> {
    let blocked = if blocked.is_empty() {
        BTreeMap::new()
    } else {
        BTreeMap::from([(
            "openapi".to_owned(),
            blocked.iter().map(ToString::to_string).collect(),
        )])
    };
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("OpenAPI policy"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("openapi-test")
            .with_session_id("session-1")
            .with_function_call_id("call-1"),
    )
}

#[test]
fn inline_spec_generates_stable_operations_and_preserves_parameter_schema() {
    let parsed = parse_operations(&inline_spec(), None, &[]).expect("inline OpenAPI spec");
    assert_eq!(parsed.base_url.as_str(), "https://api.example.test/v1");
    assert_eq!(parsed.operations.len(), 2);
    let read = parsed
        .operations
        .iter()
        .find(|operation| operation.name() == "get_users_by_id")
        .expect("generated read operation");
    assert!(read.is_read_only());
    assert_eq!(
        read.parameters_schema()["properties"]["id"]["type"],
        "string"
    );
    assert_eq!(
        read.parameters_schema()["properties"]["id"]["maxLength"],
        64
    );
    assert_eq!(
        read.parameters_schema()["properties"]["id"]["description"],
        "Exact user ID."
    );
    let create = parsed
        .operations
        .iter()
        .find(|operation| operation.name() == "create_user")
        .expect("explicit create operation");
    assert!(!create.is_read_only());
    assert!(
        create.parameters_schema()["required"]
            .as_array()
            .expect("required arguments")
            .contains(&json!("body_json"))
    );
}

#[test]
fn yaml_server_variables_and_exact_selection_are_supported() {
    let yaml = Value::String(
        r"
openapi: 3.0.3
servers:
  - url: https://{tenant}.example.test/api
    variables:
      tenant:
        default: north
paths:
  /records:
    get:
      operationId: list_records
      responses:
        '200':
          description: ok
    post:
      operationId: create_record
      responses:
        '201':
          description: created
"
        .to_owned(),
    );
    let parsed = parse_operations(&yaml, None, &["list_records".to_owned()])
        .expect("selected YAML operation");
    assert_eq!(parsed.base_url.as_str(), "https://north.example.test/api");
    assert_eq!(parsed.operations.len(), 1);
    assert_eq!(parsed.operations[0].name(), "list_records");
}

#[test]
fn remote_specs_unknown_selection_and_malformed_auth_fail_closed_without_secrets() {
    let Err(remote) = parse_operations(
        &Value::String("https://spec.example.test/openapi.yaml".to_owned()),
        None,
        &[],
    ) else {
        panic!("remote spec needs egress authority");
    };
    assert_eq!(remote.code(), OpenApiSpecErrorCode::UnsupportedSource);
    let Err(unknown) = parse_operations(&inline_spec(), None, &["missing".to_owned()]) else {
        panic!("unknown selected operation must fail");
    };
    assert_eq!(unknown.code(), OpenApiSpecErrorCode::InvalidSpecification);

    let invalid = settings(
        &json!({
            "client_id":"client",
            "client_secret":"do-not-log-this-secret",
            "token_url":null
        }),
        &[],
    );
    let Err(error) = OpenApiToolkitConfig::parse("Customer API", &invalid, &Map::new()) else {
        panic!("partial OAuth must fail");
    };
    assert_eq!(error.code(), OpenApiConfigErrorCode::InvalidConfiguration);
    assert!(!format!("{error:?} {error}").contains("do-not-log-this-secret"));
}

#[test]
fn oauth_expires_in_accepts_numeric_provider_strings_and_defaults_safely() {
    assert_eq!(
        oauth_token_lifetime(Some(&json!(3_600))),
        std::time::Duration::from_hours(1)
    );
    assert_eq!(
        oauth_token_lifetime(Some(&json!("3600"))),
        std::time::Duration::from_hours(1)
    );
    for invalid in [json!(null), json!(0), json!(-1), json!("not-a-number")] {
        assert_eq!(
            oauth_token_lifetime(Some(&invalid)),
            std::time::Duration::from_hours(1)
        );
    }
}

#[tokio::test]
async fn delegated_openapi_rebuild_uses_only_its_configuration_scoped_token() {
    let settings = settings(
        &json!({
            "client_id":"client-id", "client_secret":"stored-secret", "configuration_uuid":"config-1",
            "oauth_discovery_endpoint":"https://login.example.test/tenant", "scope":"records.read"
        }),
        &["get_users_by_id"],
    );
    for (key, authorized) in [
        ("config-1:https://login.example.test/tenant", true),
        ("config-2:https://login.example.test/tenant", false),
    ] {
        let tokens = Map::from_iter([(key.into(), json!({"access_token":"claim-fetched-token"}))]);
        let config = OpenApiToolkitConfig::parse("Customer API", &settings, &tokens).unwrap();
        assert_eq!(config.auth().delegated_requirement().is_none(), authorized);
    }
}

#[tokio::test]
async fn delegated_openapi_materializes_original_guarded_tools_and_exact_token_rebuild() {
    let settings = settings(
        &json!({
            "client_id":"client-id",
            "client_secret":"stored-secret",
            "oauth_discovery_endpoint":"https://login.example.test/tenant",
            "scope":"records.read"
        }),
        &["get_users_by_id"],
    );
    let guarded = OpenApiToolkitConfig::parse("Customer API", &settings, &Map::new())
        .expect("guarded delegated config");
    let materialized = build_openapi_toolset("Customer API", guarded, &policy(&[]))
        .expect("guarded OpenAPI toolset");
    assert_eq!(
        materialized
            .delegated_authorization
            .tool_names()
            .collect::<Vec<_>>(),
        ["get_users_by_id"]
    );
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tool = materialized
        .toolset
        .tools(readonly)
        .await
        .expect("guarded tools")
        .pop()
        .expect("guarded operation");
    assert!(tool.description().contains("GET /users/{id}"));
    assert!(tool.description().contains("Toolkit: Customer API"));
    assert!(
        tool.description()
            .contains("Base URL: https://api.example.test/v1")
    );
    let error = tool
        .execute(context(), json!({"id":"private-argument"}))
        .await
        .expect_err("delegated authorization required");
    let requirement = delegated_authorization_requirement(&error).expect("typed requirement");
    assert_eq!(requirement.toolkit_name(), "Customer API");
    assert_eq!(requirement.toolkit_type(), "openapi");
    assert_eq!(requirement.server_url(), "https://api.example.test/v1");
    assert!(!format!("{error:?} {error}").contains("private-argument"));

    let tokens = json!({
        "https://api.example.test/v1":{"access_token":"claim-fetched-token"}
    })
    .as_object()
    .expect("token map")
    .clone();
    let authorized = OpenApiToolkitConfig::parse("Customer API", &settings, &tokens)
        .expect("authorized delegated config");
    let materialized = build_openapi_toolset("Customer API", authorized, &policy(&[]))
        .expect("authorized OpenAPI toolset");
    assert!(materialized.delegated_authorization.is_empty());
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = materialized
        .toolset
        .tools(readonly)
        .await
        .expect("authorized tools");
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].name(), "get_users_by_id");
}

#[tokio::test]
async fn configured_materializer_merges_openapi_authorization_and_honors_tool_policy() {
    let version = json!({
        "tools":[{
            "id":72,
            "type":"openapi",
            "toolkit_name":"Customer API",
            "settings":settings(
                &json!({
                    "client_id":"client-id",
                    "client_secret":"stored-secret",
                    "oauth_discovery_endpoint":"https://login.example.test/tenant"
                }),
                &["get_users_by_id","create_user"]
            )
        }]
    });
    let version = version.as_object().expect("version details");
    let snapshot = FrozenToolSnapshot::from_version_details(version)
        .expect("OpenAPI snapshot")
        .apply_policy(policy(&[]).as_ref());
    let (toolsets, authorization) = materialize_configured_toolsets_with_tokens_and_authorization(
        &snapshot,
        &policy(&["create_user"]),
        &Map::new(),
    )
    .await
    .expect("configured OpenAPI materialization");
    assert_eq!(toolsets.len(), 1);
    assert_eq!(
        authorization.tool_names().collect::<Vec<_>>(),
        ["get_users_by_id"]
    );
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolsets[0]
        .tools(readonly)
        .await
        .expect("policy-bound tools");
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].name(), "get_users_by_id");
}

/// A family name that names nothing, and the synthetic name is the point.
///
/// This case asserts that ONE unsupported family does not take a runnable
/// toolkit down with it, so it needs a type the dispatcher genuinely does not
/// handle. Every REAL name held that role only until someone implemented it:
/// `github` was first, and the day `materialize.rs` began dispatching it the
/// fixture's bare `{"selected_tools":[...]}` stopped meaning "a family we skip"
/// and started meaning "a supported family configured wrongly" — an
/// `InvalidConfiguration` that fails the whole materialization, which is
/// correct behaviour and the exact opposite of what this test checks. `aha`
/// inherited the role and is one dispatch arm from the same silent inversion:
/// `families/aha/` is implemented in-tree and only its artifact resolver and
/// the `materialize_p_to_z` arm are missing.
///
/// A name no family module claims takes the IDENTICAL path: `materialize`
/// routes everything outside its `a_to_k` list, `openapi` and `sharepoint` into
/// `materialize_p_to_z`, whose `_` arm is the same `unsupported_toolkit()` that
/// a real-but-unimplemented family reaches, and nothing screens the name first
/// — `FrozenToolSnapshot` checks identifier shape only, and
/// `ToolAdmissionPolicy` allows any type it does not recognise. So this fixture
/// keeps the discrimination it was written for and cannot be implemented out
/// from under the assertion. Do not repoint it back to a real family.
#[tokio::test]
async fn unsupported_configured_family_does_not_hide_runnable_openapi_toolkit() {
    let version = json!({
        "tools":[
            {
                "id":1,
                "type":"not_a_toolkit_family",
                "toolkit_name":"roadmap",
                "settings":{"selected_tools":["get_ideas"]}
            },
            {
                "id":72,
                "type":"openapi",
                "toolkit_name":"Customer API",
                "settings":settings(&json!({}), &["get_users_by_id"])
            }
        ]
    });
    let version = version.as_object().expect("version details");
    let snapshot = FrozenToolSnapshot::from_version_details(version)
        .expect("mixed snapshot")
        .apply_policy(policy(&[]).as_ref());

    let (toolsets, authorization) = materialize_configured_toolsets_with_tokens_and_authorization(
        &snapshot,
        &policy(&[]),
        &Map::new(),
    )
    .await
    .expect("supported toolkit remains runnable");

    assert_eq!(toolsets.len(), 1);
    assert!(authorization.is_empty());
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolsets[0].tools(readonly).await.expect("OpenAPI tools");
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].name(), "get_users_by_id");
}

struct FixtureTransport {
    requests: Mutex<Vec<OpenApiRequest>>,
    token_requests: Mutex<Vec<Request>>,
    token: String,
    responses: Mutex<Vec<OpenApiResponse>>,
}

#[async_trait]
impl OpenApiTransport for FixtureTransport {
    async fn execute(
        &self,
        request: OpenApiRequest,
    ) -> Result<OpenApiResponse, OpenApiClientError> {
        self.requests
            .lock()
            .expect("fixture requests")
            .push(request);
        Ok(self
            .responses
            .lock()
            .expect("fixture responses")
            .pop()
            .expect("fixture response"))
    }

    async fn token(&self, request: Request) -> Result<OpenApiAccessToken, OpenApiClientError> {
        self.token_requests
            .lock()
            .expect("fixture token requests")
            .push(request);
        Ok(OpenApiAccessToken {
            value: zeroize::Zeroizing::new(self.token.clone()),
            expires_in: std::time::Duration::from_hours(1),
        })
    }
}

#[tokio::test]
async fn operation_execution_binds_path_query_headers_and_client_credentials() {
    let settings = settings(
        &json!({
            "client_id":"client-id",
            "client_secret":"client-secret",
            "token_url":"https://login.example.test/oauth/token",
            "scope":"records.read",
            "method":"Basic"
        }),
        &["get_users_by_id"],
    );
    let config = OpenApiToolkitConfig::parse("Customer API", &settings, &Map::new())
        .expect("client-credential config");
    let operation = config.operations()[0].clone();
    let transport = Arc::new(FixtureTransport {
        requests: Mutex::new(Vec::new()),
        token_requests: Mutex::new(Vec::new()),
        token: "runtime-access-token".to_owned(),
        responses: Mutex::new(vec![
            OpenApiResponse {
                status: StatusCode::OK,
                body: br#"{"name":"Ada"}"#.to_vec(),
            },
            OpenApiResponse {
                status: StatusCode::OK,
                body: br#"{"name":"Ada"}"#.to_vec(),
            },
        ]),
    });
    let client = OpenApiClient::with_transport(config.into_client_parts(), transport.clone());
    let arguments = json!({
        "id":"Ada/Lovelace",
        "search":"name eq 'Ada Lovelace'",
        "headers":{"X-Trace-Id":"trace-1"},
        "regexp":"Ada"
    });
    let result = client
        .execute(
            &operation,
            arguments.as_object().expect("operation arguments"),
        )
        .await
        .expect("OpenAPI execution");
    assert_eq!(result, Value::String("{\"name\":\"\"}".to_owned()));
    let second = client
        .execute(
            &operation,
            arguments.as_object().expect("operation arguments"),
        )
        .await
        .expect("cached-token OpenAPI execution");
    assert_eq!(second, Value::String("{\"name\":\"\"}".to_owned()));

    let requests = transport.requests.lock().expect("fixture requests");
    assert_eq!(requests.len(), 2);
    assert_eq!(
        requests[0].uri().to_string(),
        "https://api.example.test/v1/users/Ada%2FLovelace?search=name%20eq%20'Ada%20Lovelace'"
    );
    assert_eq!(
        requests[0]
            .headers()
            .get("x-trace-id")
            .expect("trace header"),
        "trace-1"
    );
    let authorization = requests[0]
        .headers()
        .get(AUTHORIZATION)
        .expect("Bearer authorization");
    assert_eq!(authorization, "Bearer runtime-access-token");
    assert!(authorization.is_sensitive());
    drop(requests);

    let token_requests = transport
        .token_requests
        .lock()
        .expect("fixture token requests");
    assert_eq!(token_requests.len(), 1);
    assert_eq!(
        token_requests[0]
            .headers()
            .get(CONTENT_TYPE)
            .expect("form content type"),
        "application/x-www-form-urlencoded"
    );
    let basic = token_requests[0]
        .headers()
        .get(AUTHORIZATION)
        .expect("Basic authorization");
    assert!(basic.is_sensitive());
    assert!(!format!("{basic:?}").contains("client-secret"));
}

#[tokio::test]
async fn delegated_openapi_rotated_tokens_stay_bound_during_parallel_resource_calls() {
    let transport = Arc::new(FixtureTransport {
        requests: Mutex::new(Vec::new()),
        token_requests: Mutex::new(Vec::new()),
        token: "must-not-use-client-credentials".to_owned(),
        responses: Mutex::new(
            (0..4)
                .map(|_| OpenApiResponse {
                    status: StatusCode::OK,
                    body: br#"{"name":"Ada"}"#.to_vec(),
                })
                .collect(),
        ),
    });
    // Two configurations share a protected API but use distinct issuers.
    // Rebuild after a grant/refresh; the worker does not exchange refresh tokens.
    for first_token in ["first-access", "rotated-access"] {
        let tokens = json!({
            "config-1:https://issuer-one.example.test/tenant": {
                "access_token":first_token, "refresh_token":"must-not-dispatch-refresh"
            },
            "config-2:https://issuer-two.example.test/tenant": {
                "access_token":"second-access"
            }
        });
        let configs = [
            ("config-1", "https://issuer-one.example.test/tenant"),
            ("config-2", "https://issuer-two.example.test/tenant"),
        ]
        .map(|(configuration, issuer)| {
            let settings = settings(
                &json!({
                    "configuration_uuid":configuration,
                    "oauth_discovery_endpoint":issuer,
                    "client_id":"stored-client", "client_secret":"must-not-dispatch-secret"
                }),
                &["get_users_by_id"],
            );
            OpenApiToolkitConfig::parse(
                "Customer API",
                &settings,
                tokens.as_object().expect("claim tokens"),
            )
            .expect("authorized config")
        });
        let [first, second] = configs;
        assert!(first.auth().delegated_requirement().is_none());
        assert!(second.auth().delegated_requirement().is_none());
        let operation = first.operations()[0].clone();
        let first = OpenApiClient::with_transport(first.into_client_parts(), transport.clone());
        let second = OpenApiClient::with_transport(second.into_client_parts(), transport.clone());
        let arguments = json!({"id":"Ada"});
        let arguments = arguments.as_object().expect("arguments");
        let (first, second) = tokio::join!(
            first.execute(&operation, arguments),
            second.execute(&operation, arguments),
        );
        assert!(first.is_ok());
        assert!(second.is_ok());
    }
    assert!(
        transport
            .token_requests
            .lock()
            .expect("token requests")
            .is_empty()
    );
    let requests = transport.requests.lock().expect("resource requests");
    let mut bearer_values = Vec::new();
    for request in requests.iter() {
        assert_eq!(
            request.uri().to_string(),
            "https://api.example.test/v1/users/Ada"
        );
        let bearer = request.headers().get(AUTHORIZATION).expect("Bearer token");
        assert!(bearer.is_sensitive());
        assert!(!format!("{bearer:?}").contains("access"));
        bearer_values.push(bearer.to_str().expect("Bearer header"));
    }
    bearer_values.sort_unstable();
    assert_eq!(
        bearer_values,
        [
            "Bearer first-access",
            "Bearer rotated-access",
            "Bearer second-access",
            "Bearer second-access"
        ]
    );
}

#[tokio::test]
async fn rfc3986_query_serialization_matches_the_sdk_contract() {
    let spec = json!({
        "openapi":"3.0.3",
        "servers":[{"url":"https://api.example.test/v1"}],
        "paths":{
            "/records":{
                "get":{
                    "operationId":"list_records",
                    "parameters":[
                        {"name":"$filter","in":"query","style":"form","explode":false,"allowReserved":true,"schema":{"type":"string"}},
                        {"name":"$top","in":"query","schema":{"type":"integer"}},
                        {"name":"strict","in":"query","allowReserved":false,"schema":{"type":"string"}},
                        {"name":"tag","in":"query","style":"form","explode":true,"schema":{"type":"array","items":{"type":"string"}}},
                        {"name":"spaced","in":"query","style":"spaceDelimited","explode":false,"schema":{"type":"array","items":{"type":"string"}}},
                        {"name":"piped","in":"query","style":"pipeDelimited","explode":false,"schema":{"type":"array","items":{"type":"string"}}},
                        {"name":"reserved","in":"query","style":"form","explode":false,"allowReserved":true,"schema":{"type":"string"}},
                        {"name":"plus_hash","in":"query","style":"form","explode":false,"allowReserved":true,"schema":{"type":"string"}},
                        {"name":"object_explode","in":"query","style":"form","explode":true,"schema":{"type":"object"}},
                        {"name":"object_flat","in":"query","style":"form","explode":false,"schema":{"type":"object"}}
                    ],
                    "responses":{"200":{"description":"ok"}}
                }
            }
        }
    });
    let config = OpenApiToolkitConfig::parse(
        "RFC API",
        json!({"spec":spec,"selected_tools":["list_records"]})
            .as_object()
            .expect("RFC settings"),
        &Map::new(),
    )
    .expect("RFC OpenAPI config");
    let operation = config.operations()[0].clone();
    let transport = Arc::new(FixtureTransport {
        requests: Mutex::new(Vec::new()),
        token_requests: Mutex::new(Vec::new()),
        token: String::new(),
        responses: Mutex::new(vec![OpenApiResponse {
            status: StatusCode::OK,
            body: b"[]".to_vec(),
        }]),
    });
    let client = OpenApiClient::with_transport(config.into_client_parts(), transport.clone());
    client
        .execute(
            &operation,
            json!({
                "$filter":"Address/City eq 'London' or contains(x,',B,')",
                "$top":500,
                "strict":"a(b)",
                "tag":["a","b"],
                "spaced":["a","b"],
                "piped":["a","b"],
                "reserved":":/?[]@!$&'()*,;=",
                "plus_hash":"a+b#c",
                "object_explode":{"left":"a b","right":"c+d"},
                "object_flat":{"left":"a b","right":"c+d"}
            })
            .as_object()
            .expect("RFC arguments"),
        )
        .await
        .expect("RFC request");

    let requests = transport.requests.lock().expect("RFC requests");
    assert_eq!(requests.len(), 1);
    assert_eq!(
        requests[0].uri().to_string(),
        "https://api.example.test/v1/records?$filter=Address/City%20eq%20'London'%20or%20contains(x,',B,')&%24top=500&strict=a%28b%29&tag=a&tag=b&spaced=a%20b&piped=a|b&reserved=:/?[]@!$&'()*,;=&plus_hash=a%2Bb%23c&left=a%20b&right=c%2Bd&object_flat=left,a%20b,right,c%2Bd"
    );
}

#[tokio::test]
async fn request_body_and_credential_header_overrides_are_strict() {
    let config = OpenApiToolkitConfig::parse(
        "Customer API",
        &settings(
            &json!({"api_key":"key","auth_type":"Bearer"}),
            &["create_user"],
        ),
        &Map::new(),
    )
    .expect("API-key config");
    let operation = config.operations()[0].clone();
    let transport = Arc::new(FixtureTransport {
        requests: Mutex::new(Vec::new()),
        token_requests: Mutex::new(Vec::new()),
        token: String::new(),
        responses: Mutex::new(vec![OpenApiResponse {
            status: StatusCode::CREATED,
            body: Vec::new(),
        }]),
    });
    let client = OpenApiClient::with_transport(config.into_client_parts(), transport.clone());
    let result = client
        .execute(
            &operation,
            json!({"body_json":"{\"name\":\"Ada\"}"})
                .as_object()
                .expect("body arguments"),
        )
        .await
        .expect("body request");
    assert_eq!(result, Value::String(String::new()));
    {
        let requests = transport.requests.lock().expect("fixture requests");
        let body: Value = serde_json::from_slice(requests[0].body()).expect("JSON request body");
        assert_eq!(body, json!({"name":"Ada"}));
    }

    let error = client
        .execute(
            &operation,
            json!({
                "body_json":"{}",
                "headers":{"Authorization":"attacker-value"}
            })
            .as_object()
            .expect("override arguments"),
        )
        .await
        .expect_err("credential override rejected");
    assert_eq!(
        error.code(),
        super::families::openapi::client::OpenApiClientErrorCode::InvalidInput
    );
}

#[tokio::test]
async fn configured_secret_headers_combine_with_and_cannot_override_primary_auth() {
    let config = OpenApiToolkitConfig::parse(
        "Customer API",
        &settings(
            &json!({
                "api_key":"primary-key",
                "auth_type":"Bearer",
                "headers":{
                    "x-api-key":"gateway-key",
                    "x-tenant":"tenant-a",
                    "authorization":"untrusted-value"
                }
            }),
            &["get_users_by_id"],
        ),
        &Map::new(),
    )
    .expect("additional secret headers");
    let operation = config.operations()[0].clone();
    let transport = Arc::new(FixtureTransport {
        requests: Mutex::new(Vec::new()),
        token_requests: Mutex::new(Vec::new()),
        token: String::new(),
        responses: Mutex::new(vec![OpenApiResponse {
            status: StatusCode::OK,
            body: b"{}".to_vec(),
        }]),
    });
    let client = OpenApiClient::with_transport(config.into_client_parts(), transport.clone());
    client
        .execute(
            &operation,
            json!({"id":"1"}).as_object().expect("operation arguments"),
        )
        .await
        .expect("configured headers request");

    let requests = transport
        .requests
        .lock()
        .expect("configured header requests");
    let headers = requests[0].headers();
    assert_eq!(headers["x-api-key"], "gateway-key");
    assert_eq!(headers["x-tenant"], "tenant-a");
    assert_eq!(headers[AUTHORIZATION], "Bearer primary-key");
    assert!(headers["x-api-key"].is_sensitive());
    assert!(headers[AUTHORIZATION].is_sensitive());
}

#[tokio::test]
async fn response_search_uses_declared_collection_and_preserves_response_shape() {
    let spec = json!({
        "openapi":"3.0.3",
        "servers":[{"url":"https://api.example.test"}],
        "paths":{
            "/records":{
                "get":{
                    "operationId":"list_records",
                    "responses":{
                        "200":{
                            "description":"ok",
                            "content":{
                                "application/json":{
                                    "schema":{
                                        "type":"object",
                                        "properties":{
                                            "payload":{
                                                "type":"object",
                                                "properties":{
                                                    "entries":{"type":"array","items":{"type":"object"}}
                                                }
                                            }
                                        }
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
    });
    let config = OpenApiToolkitConfig::parse(
        "Search API",
        json!({"spec":spec,"selected_tools":["list_records"]})
            .as_object()
            .expect("search settings"),
        &Map::new(),
    )
    .expect("search config");
    let operation = config.operations()[0].clone();
    assert_eq!(
        operation.response_collection_paths(),
        &[vec!["payload".to_owned(), "entries".to_owned()]]
    );
    assert_eq!(
        operation.parameters_schema()["properties"]["response_limit"]["maximum"],
        200
    );
    let transport = Arc::new(FixtureTransport {
        requests: Mutex::new(Vec::new()),
        token_requests: Mutex::new(Vec::new()),
        token: String::new(),
        responses: Mutex::new(vec![OpenApiResponse {
            status: StatusCode::OK,
            body: serde_json::to_vec(&json!({
                "audit":[{"title":"database shadow"},{"title":"database shadow two"}],
                "payload":{
                    "entries":[
                        {"id":"routine","title":"routine update"},
                        {"id":"target","title":"critical database outage"}
                    ]
                },
                "count":2
            }))
            .expect("response body"),
        }]),
    });
    let client = OpenApiClient::with_transport(config.into_client_parts(), transport.clone());
    let result = client
        .execute(
            &operation,
            json!({"response_search":"critical database","response_limit":1})
                .as_object()
                .expect("selection arguments"),
        )
        .await
        .expect("selected response");
    let selected: Value = serde_json::from_str(result.as_str().expect("string tool result"))
        .expect("selection envelope");
    assert_eq!(
        selected["_elitea_response_selection"]["collection_path"],
        "$.payload.entries"
    );
    assert_eq!(selected["data"]["payload"]["entries"][0]["id"], "target");
    assert_eq!(selected["data"]["audit"].as_array().map(Vec::len), Some(2));
    assert_eq!(
        transport.requests.lock().expect("selection requests")[0]
            .uri()
            .to_string(),
        "https://api.example.test/records"
    );
}

#[tokio::test]
async fn response_search_preserves_matching_keyed_objects_and_rejects_regexp_mix() {
    let spec = json!({
        "openapi":"3.0.3",
        "servers":[{"url":"https://api.example.test"}],
        "paths":{
            "/users":{
                "get":{
                    "operationId":"list_users",
                    "responses":{
                        "200":{
                            "description":"ok",
                            "content":{
                                "application/json":{
                                    "schema":{
                                        "type":"object",
                                        "properties":{
                                            "usersById":{
                                                "type":"object",
                                                "additionalProperties":{"type":"object"}
                                            }
                                        }
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
    });
    let config = OpenApiToolkitConfig::parse(
        "Map API",
        json!({"spec":spec,"selected_tools":["list_users"]})
            .as_object()
            .expect("map settings"),
        &Map::new(),
    )
    .expect("map config");
    let operation = config.operations()[0].clone();
    let transport = Arc::new(FixtureTransport {
        requests: Mutex::new(Vec::new()),
        token_requests: Mutex::new(Vec::new()),
        token: String::new(),
        responses: Mutex::new(vec![OpenApiResponse {
            status: StatusCode::OK,
            body: serde_json::to_vec(&json!({
                "usersById":{
                    "user-alpha":{"name":"Alice","status":"active"},
                    "user-beta":{"name":"Bob","status":"active"}
                },
                "count":2
            }))
            .expect("map body"),
        }]),
    });
    let client = OpenApiClient::with_transport(config.into_client_parts(), transport.clone());
    let selected = client
        .execute(
            &operation,
            json!({"response_search":"\"user beta\""})
                .as_object()
                .expect("map arguments"),
        )
        .await
        .expect("selected map");
    let selected: Value = serde_json::from_str(selected.as_str().expect("map result string"))
        .expect("map selection envelope");
    assert_eq!(
        selected["data"]["usersById"],
        json!({"user-beta":{"name":"Bob","status":"active"}})
    );

    let error = client
        .execute(
            &operation,
            json!({"regexp":"Bob","response_search":"Bob"})
                .as_object()
                .expect("mixed arguments"),
        )
        .await
        .expect_err("regexp and structured selection conflict");
    assert_eq!(
        error.code(),
        super::families::openapi::client::OpenApiClientErrorCode::InvalidInput
    );
    assert_eq!(transport.requests.lock().expect("map requests").len(), 1);
}

#[test]
fn recursive_server_variable_defaults_stop_at_the_materialization_bound() {
    let mut spec = inline_spec();
    spec["servers"] = json!([{"url":"https://{host}","variables":{"host":{"default":"{host}"}}}]);
    let error = parse_operations(&spec, None, &[])
        .err()
        .expect("bounded substitution");
    assert_eq!(error.code(), OpenApiSpecErrorCode::ResourceExhausted);
}
