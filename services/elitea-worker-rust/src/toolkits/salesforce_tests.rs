use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use reqwest::header::{AUTHORIZATION, CONTENT_TYPE};
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::salesforce::client::{
    SalesforceApi, SalesforceClient, SalesforceClientError, SalesforceClientErrorCode,
    SalesforceHttpResponse, SalesforceMethod, SalesforceRequestKind, SalesforceTransport,
    test_map_resource_status,
};
use super::families::salesforce::config::{SalesforceConfigErrorCode, SalesforceToolkitConfig};
use super::families::salesforce::tools::{
    SalesforceToolsetErrorCode, build_salesforce_toolset, test_build_with_api,
};
use super::policy::ToolAdmissionPolicy;

fn settings(selected_tools: &[&str]) -> Map<String, Value> {
    json!({
        "api_version": "v59.0",
        "salesforce_configuration": {
            "client_id": "client+id",
            "client_secret": "secret&value",
            "base_url": "https://tenant.my.salesforce.com"
        },
        "selected_tools": selected_tools
    })
    .as_object()
    .cloned()
    .expect("Salesforce fixture settings are an object")
}

fn config(selected_tools: &[&str]) -> SalesforceToolkitConfig {
    SalesforceToolkitConfig::parse(&settings(selected_tools))
        .expect("valid Salesforce configuration")
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Salesforce policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(
        SimpleToolContext::new("salesforce-tool-test").with_function_call_id("salesforce-call"),
    )
}

fn request_body(request: &Request) -> Value {
    request
        .body()
        .and_then(reqwest::Body::as_bytes)
        .and_then(|bytes| serde_json::from_slice(bytes).ok())
        .expect("request has JSON body")
}

#[test]
fn materialized_configuration_is_bounded_deduplicated_and_secret_safe() {
    let parsed = config(&["create_case", "create_case", "search_salesforce"]);
    assert_eq!(
        parsed.test_origin().as_str(),
        "https://tenant.my.salesforce.com/"
    );
    assert_eq!(parsed.test_api_version(), "v59.0");
    assert_eq!(
        parsed.selected_tools(),
        [
            Box::<str>::from("create_case"),
            Box::<str>::from("search_salesforce")
        ]
    );

    let mut defaulted = settings(&[]);
    defaulted.remove("api_version");
    assert_eq!(
        SalesforceToolkitConfig::parse(&defaulted)
            .expect("missing version uses SDK default")
            .test_api_version(),
        "v59.0"
    );

    let rendered = format!("{:?}", SalesforceToolkitConfig::parse(&settings(&[])).err());
    assert!(!rendered.contains("secret&value"));
    assert!(!rendered.contains("client+id"));
}

#[test]
fn malformed_origins_versions_credentials_and_bounds_fail_closed() {
    for invalid in [
        json!({
            "salesforce_configuration": {
                "client_id":"id", "client_secret":"secret", "base_url":"http://tenant.example"
            }
        }),
        json!({
            "salesforce_configuration": {
                "client_id":"id", "client_secret":"secret", "base_url":"https://tenant.example/path"
            }
        }),
        json!({
            "api_version":"59.0",
            "salesforce_configuration": {
                "client_id":"id", "client_secret":"secret", "base_url":"https://tenant.example"
            }
        }),
        json!({
            "salesforce_configuration": {
                "client_id":"id\n", "client_secret":"secret", "base_url":"https://tenant.example"
            }
        }),
    ] {
        let Err(error) = SalesforceToolkitConfig::parse(
            invalid
                .as_object()
                .expect("invalid Salesforce fixture is an object"),
        ) else {
            panic!("invalid Salesforce configuration must fail");
        };
        assert_eq!(
            error.code(),
            SalesforceConfigErrorCode::InvalidConfiguration
        );
    }

    let mut oversized = settings(&[]);
    oversized
        .get_mut("salesforce_configuration")
        .and_then(Value::as_object_mut)
        .expect("nested Salesforce configuration")
        .insert(
            "client_secret".to_owned(),
            Value::String("x".repeat(16 * 1_024 + 1)),
        );
    let Err(error) = SalesforceToolkitConfig::parse(&oversized) else {
        panic!("oversized Salesforce secret must fail");
    };
    assert_eq!(error.code(), SalesforceConfigErrorCode::ResourceExhausted);
}

#[derive(Clone)]
struct CapturedRequest {
    method: Method,
    url: String,
    body: Vec<u8>,
    authorization: Option<String>,
    effect: bool,
}

struct FixtureTransport {
    requests: Mutex<Vec<CapturedRequest>>,
    responses: Mutex<VecDeque<Result<SalesforceHttpResponse, SalesforceClientError>>>,
}

impl FixtureTransport {
    fn new(responses: Vec<Result<SalesforceHttpResponse, SalesforceClientError>>) -> Self {
        Self {
            requests: Mutex::new(Vec::new()),
            responses: Mutex::new(responses.into()),
        }
    }

    fn requests(&self) -> Vec<CapturedRequest> {
        self.requests
            .lock()
            .expect("Salesforce request fixture lock")
            .clone()
    }
}

#[async_trait]
impl SalesforceTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<SalesforceHttpResponse, SalesforceClientError> {
        let body = request
            .body()
            .and_then(reqwest::Body::as_bytes)
            .map_or_else(Vec::new, ToOwned::to_owned);
        let authorization = request
            .headers()
            .get(AUTHORIZATION)
            .and_then(|value| value.to_str().ok())
            .map(ToOwned::to_owned);
        self.requests
            .lock()
            .expect("Salesforce request fixture lock")
            .push(CapturedRequest {
                method: request.method().clone(),
                url: request.url().to_string(),
                body,
                authorization,
                effect,
            });
        self.responses
            .lock()
            .expect("Salesforce response fixture lock")
            .pop_front()
            .expect("one Salesforce response per request")
    }
}

fn response(status: StatusCode, body: Option<Value>) -> SalesforceHttpResponse {
    SalesforceHttpResponse::fixture(status, body)
}

#[test]
fn oauth_and_dedicated_requests_are_origin_version_and_payload_bound() {
    let client = SalesforceClient::new(config(&[])).expect("Salesforce client");
    let auth = client.test_auth_request().expect("OAuth request");
    assert_eq!(auth.method(), Method::POST);
    assert_eq!(
        auth.url().as_str(),
        "https://tenant.my.salesforce.com/services/oauth2/token"
    );
    assert_eq!(
        auth.headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok()),
        Some("application/x-www-form-urlencoded")
    );
    let body = auth
        .body()
        .and_then(reqwest::Body::as_bytes)
        .and_then(|bytes| std::str::from_utf8(bytes).ok())
        .expect("OAuth form body");
    assert_eq!(
        body,
        "grant_type=client_credentials&client_id=client%2Bid&client_secret=secret%26value"
    );

    let case = client
        .test_request(
            SalesforceRequestKind::CreateCase {
                subject: "Need help",
                description: "Printer is offline",
                origin: "Web",
                status: "New",
            },
            "token-one",
        )
        .expect("create Case request");
    assert_eq!(case.method(), Method::POST);
    assert_eq!(
        case.url().as_str(),
        "https://tenant.my.salesforce.com/services/data/v59.0/sobjects/Case/"
    );
    assert_eq!(
        request_body(&case),
        json!({
            "Subject":"Need help", "Description":"Printer is offline", "Origin":"Web", "Status":"New"
        })
    );

    let search = client
        .test_request(
            SalesforceRequestKind::Search {
                query: "SELECT Id FROM Case WHERE Status = 'New'",
            },
            "token-one",
        )
        .expect("SOQL request");
    assert_eq!(search.method(), Method::GET);
    assert_eq!(search.url().query_pairs().count(), 1);
    assert_eq!(
        search
            .url()
            .query_pairs()
            .find(|(key, _)| key == "q")
            .map(|(_, value)| value.into_owned()),
        Some("SELECT Id FROM Case WHERE Status = 'New'".to_owned())
    );
}

#[test]
fn updates_validate_record_ids_omit_empty_fields_and_reject_noop_leads() {
    let client = SalesforceClient::new(config(&[])).expect("Salesforce client");
    let case = client
        .test_request(
            SalesforceRequestKind::UpdateCase {
                case_id: "500000000000000AAA",
                status: "Closed",
                description: Some(""),
            },
            "token",
        )
        .expect("Case update request");
    assert_eq!(request_body(&case), json!({"Status":"Closed"}));

    let lead = client
        .test_request(
            SalesforceRequestKind::UpdateLead {
                lead_id: "00Q000000000001AAA",
                email: Some("lead@example.com"),
                phone: None,
            },
            "token",
        )
        .expect("Lead update request");
    assert_eq!(request_body(&lead), json!({"Email":"lead@example.com"}));

    assert!(
        client
            .test_request(
                SalesforceRequestKind::UpdateLead {
                    lead_id: "00Q000000000001AAA",
                    email: None,
                    phone: Some("")
                },
                "token"
            )
            .is_err()
    );
    assert!(
        client
            .test_request(
                SalesforceRequestKind::UpdateCase {
                    case_id: "../not-an-id",
                    status: "Closed",
                    description: None
                },
                "token"
            )
            .is_err()
    );
}

#[test]
fn generic_get_uses_query_and_effect_methods_use_confined_json_routes() {
    let client = SalesforceClient::new(config(&[])).expect("Salesforce client");
    let params = json!({"limit":10,"active":true,"ignored":null})
        .as_object()
        .cloned()
        .expect("generic params object");
    let get = client
        .test_request(
            SalesforceRequestKind::Generic {
                method: SalesforceMethod::Get,
                relative_url: "/limits",
                params: &params,
            },
            "token",
        )
        .expect("generic GET request");
    assert_eq!(get.method(), Method::GET);
    assert!(get.body().is_none());
    assert_eq!(get.url().query_pairs().count(), 2);

    for method in [
        SalesforceMethod::Post,
        SalesforceMethod::Patch,
        SalesforceMethod::Delete,
    ] {
        let request = client
            .test_request(
                SalesforceRequestKind::Generic {
                    method,
                    relative_url: "/sobjects/Case/500000000000000AAA",
                    params: &params,
                },
                "token",
            )
            .expect("generic effect request");
        assert_eq!(request_body(&request), Value::Object(params.clone()));
    }

    for path in [
        "https://attacker.example/x",
        "//attacker.example/x",
        "/../oauth2/token",
        "/sobjects/Case?id=1",
        "/sobjects/%2e%2e/token",
        "/sobjects\\Case",
    ] {
        assert!(
            client
                .test_request(
                    SalesforceRequestKind::Generic {
                        method: SalesforceMethod::Get,
                        relative_url: path,
                        params: &Map::new()
                    },
                    "token"
                )
                .is_err()
        );
    }
}

#[tokio::test]
async fn explicit_401_refreshes_once_and_cached_token_is_reused() {
    let transport = Arc::new(FixtureTransport::new(vec![
        Ok(response(
            StatusCode::OK,
            Some(json!({"access_token":"token-one"})),
        )),
        Ok(response(
            StatusCode::UNAUTHORIZED,
            Some(json!([{"errorCode":"INVALID_SESSION_ID"}])),
        )),
        Ok(response(
            StatusCode::OK,
            Some(json!({"access_token":"token-two"})),
        )),
        Ok(response(
            StatusCode::CREATED,
            Some(json!({"id":"500", "success":true})),
        )),
        Ok(response(
            StatusCode::OK,
            Some(json!({"totalSize":0,"records":[]})),
        )),
    ]));
    let client = SalesforceClient::with_transport(
        config(&[]),
        transport.clone() as Arc<dyn SalesforceTransport>,
    );
    assert_eq!(
        client
            .create_case("subject", "description", "Web", "New")
            .await
            .expect("create after explicit auth rejection"),
        json!({"id":"500", "success":true})
    );
    client
        .search_salesforce("Case", "SELECT Id FROM Case LIMIT 1")
        .await
        .expect("cached refreshed token");

    let requests = transport.requests();
    assert_eq!(requests.len(), 5);
    assert_eq!(
        requests
            .iter()
            .map(|request| request.effect)
            .collect::<Vec<_>>(),
        [false, true, false, true, false]
    );
    assert_eq!(
        requests[1].authorization.as_deref(),
        Some("Bearer token-one")
    );
    assert_eq!(
        requests[3].authorization.as_deref(),
        Some("Bearer token-two")
    );
    assert_eq!(
        requests[4].authorization.as_deref(),
        Some("Bearer token-two")
    );
}

#[tokio::test]
async fn dedicated_and_generic_success_shapes_match_the_current_sdk_contract() {
    let transport = Arc::new(FixtureTransport::new(vec![
        Ok(response(
            StatusCode::OK,
            Some(json!({"access_token":"token"})),
        )),
        Ok(response(StatusCode::NO_CONTENT, None)),
        Ok(response(StatusCode::NO_CONTENT, None)),
        Ok(response(StatusCode::NO_CONTENT, None)),
    ]));
    let client =
        SalesforceClient::with_transport(config(&[]), transport as Arc<dyn SalesforceTransport>);
    assert_eq!(
        client
            .update_case("500000000000000AAA", "Closed", None)
            .await
            .expect("Case update"),
        json!({"success":true,"message":"Case 500000000000000AAA updated successfully."})
    );
    assert_eq!(
        client
            .update_lead("00Q000000000001AAA", Some("new@example.com"), None)
            .await
            .expect("Lead update"),
        json!({"success":true,"message":"Lead 00Q000000000001AAA updated successfully."})
    );
    assert_eq!(
        client
            .execute_generic(
                SalesforceMethod::Delete,
                "/sobjects/Case/500000000000000AAA",
                &Map::new()
            )
            .await
            .expect("generic DELETE"),
        json!({
            "success":true,
            "message":"DELETE request to /sobjects/Case/500000000000000AAA executed successfully."
        })
    );
}

#[test]
fn status_taxonomy_is_stable_and_effect_uncertainty_is_never_retryable() {
    for (status, effect, code, retryable) in [
        (
            StatusCode::BAD_REQUEST,
            false,
            SalesforceClientErrorCode::InvalidInput,
            false,
        ),
        (
            StatusCode::UNAUTHORIZED,
            false,
            SalesforceClientErrorCode::Authentication,
            false,
        ),
        (
            StatusCode::FORBIDDEN,
            false,
            SalesforceClientErrorCode::Authorization,
            false,
        ),
        (
            StatusCode::NOT_FOUND,
            false,
            SalesforceClientErrorCode::NotFound,
            false,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            false,
            SalesforceClientErrorCode::RateLimited,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            false,
            SalesforceClientErrorCode::DependencyUnavailable,
            true,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            true,
            SalesforceClientErrorCode::RateLimited,
            false,
        ),
        (
            StatusCode::GATEWAY_TIMEOUT,
            true,
            SalesforceClientErrorCode::UnknownOutcome,
            false,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            true,
            SalesforceClientErrorCode::UnknownOutcome,
            false,
        ),
    ] {
        let error = test_map_resource_status(status, effect);
        assert_eq!(error.code(), code);
        assert_eq!(error.retryable(), retryable);
    }
    let error = SalesforceClientError::fixture(SalesforceClientErrorCode::UnknownOutcome, false);
    let rendered = format!("{error:?} {error}");
    assert!(!rendered.contains("secret&value"));
    assert!(!rendered.contains("tenant.my.salesforce.com"));
}

#[derive(Default)]
struct FixtureSalesforceApi {
    calls: Mutex<Vec<Value>>,
}

impl FixtureSalesforceApi {
    fn calls(&self) -> Vec<Value> {
        self.calls
            .lock()
            .expect("Salesforce API fixture lock")
            .clone()
    }

    fn record(&self, call: Value) -> Value {
        self.calls
            .lock()
            .expect("Salesforce API fixture lock")
            .push(call.clone());
        call
    }
}

#[async_trait]
impl SalesforceApi for FixtureSalesforceApi {
    async fn create_case(
        &self,
        subject: &str,
        description: &str,
        origin: &str,
        status: &str,
    ) -> Result<Value, SalesforceClientError> {
        Ok(self.record(json!({
            "tool":"create_case",
            "subject":subject,
            "description":description,
            "origin":origin,
            "status":status
        })))
    }

    async fn create_lead(
        &self,
        last_name: &str,
        company: &str,
        email: &str,
        phone: &str,
    ) -> Result<Value, SalesforceClientError> {
        Ok(self.record(json!({
            "tool":"create_lead",
            "last_name":last_name,
            "company":company,
            "email":email,
            "phone":phone
        })))
    }

    async fn search_salesforce(
        &self,
        object_type: &str,
        query: &str,
    ) -> Result<Value, SalesforceClientError> {
        Ok(self.record(json!({
            "tool":"search_salesforce",
            "object_type":object_type,
            "query":query
        })))
    }

    async fn update_case(
        &self,
        case_id: &str,
        status: &str,
        description: Option<&str>,
    ) -> Result<Value, SalesforceClientError> {
        Ok(self.record(json!({
            "tool":"update_case",
            "case_id":case_id,
            "status":status,
            "description":description
        })))
    }

    async fn update_lead(
        &self,
        lead_id: &str,
        email: Option<&str>,
        phone: Option<&str>,
    ) -> Result<Value, SalesforceClientError> {
        Ok(self.record(json!({
            "tool":"update_lead",
            "lead_id":lead_id,
            "email":email,
            "phone":phone
        })))
    }

    async fn execute_generic(
        &self,
        method: SalesforceMethod,
        relative_url: &str,
        params: &Map<String, Value>,
    ) -> Result<Value, SalesforceClientError> {
        Ok(self.record(json!({
            "tool":"execute_generic_rq",
            "method":format!("{method:?}"),
            "relative_url":relative_url,
            "params":params
        })))
    }
}

fn assert_model_contract(tools: &[Arc<dyn Tool>]) {
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        [
            "create_case",
            "create_lead",
            "search_salesforce",
            "update_case",
            "update_lead",
            "execute_generic_rq"
        ]
    );
    assert!(tools[2].is_read_only());
    assert!(tools[2].is_concurrency_safe());
    assert!(
        tools
            .iter()
            .enumerate()
            .all(|(index, tool)| index == 2 || !tool.is_read_only())
    );
    assert!(tools[0].description().contains("not safe to retry"));
    assert!(
        tools[2]
            .description()
            .contains("first Salesforce query page")
    );
    assert!(
        tools[4]
            .description()
            .contains("at least one non-empty change")
    );
    assert!(tools[5].description().contains("DELETE"));
    assert!(tools[5].description().contains("delete data"));

    let schemas = tools
        .iter()
        .map(|tool| tool.parameters_schema().expect("Salesforce tool schema"))
        .collect::<Vec<_>>();
    for (tool, schema) in tools.iter().zip(&schemas) {
        assert!(tool.description().contains("Toolkit: crm"));
        let properties = schema["properties"]
            .as_object()
            .expect("Salesforce parameters are an object");
        assert!(!properties.is_empty());
        for property in properties.values() {
            assert!(
                property["description"]
                    .as_str()
                    .is_some_and(|description| !description.trim().is_empty()),
                "every Salesforce parameter needs model-facing guidance"
            );
        }
    }
    assert_eq!(
        schemas[0]["required"],
        json!(["subject", "description", "origin", "status"])
    );
    assert!(
        schemas[2]["properties"]["object_type"]["description"]
            .as_str()
            .expect("object type description")
            .contains("compatibility label")
    );
    assert!(
        schemas[2]["properties"]["query"]["description"]
            .as_str()
            .expect("SOQL description")
            .contains("SELECT Id, Subject")
    );
    assert_eq!(
        schemas[3]["properties"]["case_id"]["anyOf"][1]["minLength"],
        json!(18)
    );
    assert_eq!(
        schemas[5]["properties"]["method"]["enum"],
        json!(["GET", "POST", "PATCH", "DELETE"])
    );
    assert!(
        schemas[5]["properties"]["params"]["description"]
            .as_str()
            .expect("params description")
            .contains("JSON object encoded as a string")
    );
}

#[tokio::test]
async fn all_six_tools_preserve_order_groups_metadata_arguments_and_policy() {
    let api = Arc::new(FixtureSalesforceApi::default());
    let api_trait: Arc<dyn SalesforceApi> = api.clone();
    let toolset = test_build_with_api("crm", &[], &policy(&[]), &api_trait)
        .expect("complete Salesforce toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Salesforce tools");
    assert_model_contract(&tools);

    tools[2]
        .execute(
            context(),
            json!({"object_type":"Case","query":"SELECT Id FROM Case LIMIT 2"}),
        )
        .await
        .expect("SOQL tool invocation");
    tools[5]
        .execute(
            context(),
            json!({
                "method":"DELETE",
                "relative_url":"/sobjects/Case/500000000000000AAA",
                "params":"{}"
            }),
        )
        .await
        .expect("generic DELETE invocation");
    assert_eq!(api.calls().len(), 2);

    let blocked = test_build_with_api(
        "crm",
        &[],
        &policy(&[("salesforce", &["execute_generic_rq"])]),
        &api_trait,
    )
    .expect("policy-filtered Salesforce toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert!(
        blocked
            .tools(readonly)
            .await
            .expect("filtered Salesforce tools")
            .iter()
            .all(|tool| tool.name() != "execute_generic_rq")
    );
}

#[tokio::test]
async fn invalid_selection_and_arguments_fail_before_provider_use() {
    let api = Arc::new(FixtureSalesforceApi::default());
    let api_trait: Arc<dyn SalesforceApi> = api.clone();
    let selected = vec!["search_salesforce".to_owned()];
    let toolset = test_build_with_api("crm", &selected, &policy(&[]), &api_trait)
        .expect("selected Salesforce toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("selected tool");
    for invalid in [
        json!({}),
        json!({"object_type":"Case","query":""}),
        json!({"object_type":"Case","query":"SELECT Id FROM Case","extra":true}),
    ] {
        assert!(tools[0].execute(context(), invalid).await.is_err());
    }
    assert!(api.calls().is_empty());

    let unknown = SalesforceToolkitConfig::parse(&settings(&["drop_org"]))
        .expect("bounded unknown Salesforce selection parses");
    let Err(error) = build_salesforce_toolset("crm", unknown, &policy(&[])) else {
        panic!("unknown Salesforce tool must fail");
    };
    assert_eq!(
        error.code(),
        SalesforceToolsetErrorCode::UnsupportedSelection
    );
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn SalesforceApi> = Arc::new(FixtureSalesforceApi::default());
    let toolset =
        test_build_with_api("gate", &[], &policy(&[]), &api).expect("complete salesforce toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("salesforce tools");
    super::sdk_conformance::assert_sdk_conformance("salesforce", &tools);
}
