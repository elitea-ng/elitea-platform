use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Toolset};
use async_trait::async_trait;
use reqwest::header::{ACCEPT, CONTENT_TYPE};
use reqwest::{Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::google_places::client::{
    GooglePlacesApi, GooglePlacesClient, GooglePlacesClientError, GooglePlacesClientErrorCode,
    GooglePlacesRequestKind, GooglePlacesTransport, test_http_status, test_provider_payload,
};
use super::families::google_places::config::{
    GooglePlacesConfigErrorCode, GooglePlacesToolkitConfig,
};
use super::families::google_places::tools::{
    GooglePlacesToolsetErrorCode, build_google_places_read_only_toolset, test_build_with_api,
};
use super::policy::ToolAdmissionPolicy;

fn settings(selected_tools: &[&str], results_count: &Value) -> Map<String, Value> {
    settings_with_key(selected_tools, results_count, "fixture-google-api-key")
}

fn settings_with_key(
    selected_tools: &[&str],
    results_count: &Value,
    api_key: &str,
) -> Map<String, Value> {
    json!({
        "google_places_configuration": {
            "configuration_type": "google_places",
            "api_key": api_key
        },
        "results_count": results_count,
        "selected_tools": selected_tools
    })
    .as_object()
    .expect("Google Places settings fixture")
    .clone()
}

fn config(selected_tools: &[&str], results_count: &Value) -> GooglePlacesToolkitConfig {
    GooglePlacesToolkitConfig::parse(&settings(selected_tools, results_count))
        .expect("valid Google Places configuration")
}

fn policy(blocked: &[(&str, &[&str])]) -> Arc<ToolAdmissionPolicy> {
    let blocked = blocked
        .iter()
        .map(|(toolkit, tools)| {
            (
                (*toolkit).to_owned(),
                tools.iter().map(ToString::to_string).collect::<Vec<_>>(),
            )
        })
        .collect::<BTreeMap<_, _>>();
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Google Places policy fixture"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("google-places-tool-test")
            .with_session_id("session-1")
            .with_function_call_id("call-1"),
    )
}

#[test]
fn materialized_configuration_is_bounded_deduplicated_and_secret_safe() {
    let parsed = config(&["places", "places", "find_near"], &Value::Null);
    assert_eq!(parsed.results_count(), 20);
    assert_eq!(
        parsed.selected_tools(),
        [Box::<str>::from("places"), Box::<str>::from("find_near")]
    );
    assert_eq!(config(&[], &json!(0)).results_count(), 20);
    assert_eq!(config(&[], &json!(200)).results_count(), 20);
    assert_eq!(config(&[], &json!(u64::MAX)).results_count(), 20);

    for invalid in [
        json!({"selected_tools": []}),
        json!({
            "google_places_configuration": {"api_key": ""},
            "selected_tools": []
        }),
        json!({
            "google_places_configuration": {"api_key": "fixture\nsecret"},
            "selected_tools": []
        }),
        json!({
            "google_places_configuration": {"api_key": "fixture-secret"},
            "results_count": -1,
            "selected_tools": []
        }),
    ] {
        let object = invalid
            .as_object()
            .expect("invalid Google Places fixture object");
        let Err(error) = GooglePlacesToolkitConfig::parse(object) else {
            panic!("malformed configuration must fail before client construction");
        };
        assert_eq!(
            error.code(),
            GooglePlacesConfigErrorCode::InvalidConfiguration
        );
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains("fixture-secret"));
    }

    let oversized = json!({
        "google_places_configuration": {"api_key": "x".repeat(8 * 1_024 + 1)},
        "selected_tools": []
    });
    let Err(oversized) = GooglePlacesToolkitConfig::parse(
        oversized
            .as_object()
            .expect("oversized Google Places fixture object"),
    ) else {
        panic!("oversized API key must be rejected");
    };
    assert_eq!(
        oversized.code(),
        GooglePlacesConfigErrorCode::ResourceExhausted
    );
}

#[test]
fn requests_use_supported_places_api_and_fixed_google_origins() {
    let client =
        GooglePlacesClient::new(config(&["places"], &json!(2))).expect("Google Places client");
    let text = client
        .test_request(GooglePlacesRequestKind::SearchText, "coffee near Kyiv")
        .expect("text-search request");
    assert_eq!(
        text.url().as_str(),
        "https://places.googleapis.com/v1/places:searchText"
    );
    assert_eq!(text.method(), reqwest::Method::POST);
    assert_eq!(
        text.headers().get(ACCEPT).expect("JSON accept header"),
        "application/json"
    );
    assert_eq!(
        text.headers().get(CONTENT_TYPE).expect("JSON content type"),
        "application/json"
    );
    assert_eq!(
        text.headers()
            .get("x-goog-fieldmask")
            .expect("bounded field mask"),
        "places.id,places.displayName,places.formattedAddress,places.nationalPhoneNumber,places.websiteUri,places.location"
    );
    assert!(
        text.headers()
            .get("x-goog-api-key")
            .expect("Google API key header")
            .is_sensitive()
    );
    let body: Value = serde_json::from_slice(
        text.body()
            .and_then(reqwest::Body::as_bytes)
            .expect("bounded text-search body"),
    )
    .expect("text-search JSON");
    assert_eq!(
        body,
        json!({"textQuery": "coffee near Kyiv", "pageSize": 2})
    );
    assert_eq!(text.timeout(), Some(&Duration::from_secs(20)));

    let geocode = client
        .test_request(GooglePlacesRequestKind::Geocode, "Kyiv")
        .expect("geocode request");
    assert_eq!(geocode.method(), reqwest::Method::GET);
    assert_eq!(geocode.url().host_str(), Some("maps.googleapis.com"));
    assert_eq!(geocode.url().path(), "/maps/api/geocode/json");
    assert!(
        geocode
            .url()
            .query()
            .is_some_and(|query| query.contains("key="))
    );
}

#[derive(Default)]
struct FixtureTransport {
    requests: Mutex<Vec<String>>,
    bodies: Mutex<Vec<Value>>,
    key_headers: Mutex<Vec<String>>,
}

impl FixtureTransport {
    fn request_urls(&self) -> Vec<String> {
        self.requests
            .lock()
            .expect("Google Places request fixture lock")
            .clone()
    }

    fn request_bodies(&self) -> Vec<Value> {
        self.bodies
            .lock()
            .expect("Google Places body fixture lock")
            .clone()
    }

    fn key_headers(&self) -> Vec<String> {
        self.key_headers
            .lock()
            .expect("Google Places key fixture lock")
            .clone()
    }
}

#[async_trait]
impl GooglePlacesTransport for FixtureTransport {
    async fn execute_json(&self, request: Request) -> Result<Value, GooglePlacesClientError> {
        let url = request.url().clone();
        let body = request
            .body()
            .and_then(reqwest::Body::as_bytes)
            .and_then(|body| serde_json::from_slice::<Value>(body).ok());
        if let Some(body) = &body {
            self.bodies
                .lock()
                .expect("Google Places body fixture lock")
                .push(body.clone());
        }
        if let Some(key) = request
            .headers()
            .get("x-goog-api-key")
            .and_then(|value| value.to_str().ok())
        {
            self.key_headers
                .lock()
                .expect("Google Places key fixture lock")
                .push(key.to_owned());
        }
        self.requests
            .lock()
            .expect("Google Places request fixture lock")
            .push(url.as_str().to_owned());
        match url.path() {
            "/v1/places:searchText"
                if body
                    .as_ref()
                    .and_then(|body| body.get("locationBias"))
                    .is_some() =>
            {
                Ok(json!({
                    "places": [{
                        "id": "near-1",
                        "displayName": {"text": "Nearby Coffee"},
                        "formattedAddress": "Kyiv",
                        "location": {"latitude": 50.45, "longitude": 30.52}
                    }]
                }))
            }
            "/v1/places:searchText" => Ok(json!({
                "places": [
                    {
                        "id": "place-1",
                        "displayName": {"text": "Coffee One"},
                        "formattedAddress": "1 Main Street",
                        "nationalPhoneNumber": "+380 00 000 0000",
                        "websiteUri": "https://coffee.example.test"
                    },
                    {
                        "id": "place-2",
                        "displayName": {"text": "Coffee Two"},
                        "formattedAddress": "2 Main Street"
                    },
                    {
                        "id": "not-requested",
                        "displayName": {"text": "Not Requested"}
                    }
                ]
            })),
            "/maps/api/geocode/json" => Ok(json!({
                "status": "OK",
                "results": [{"geometry": {"location": {"lat": 50.4501, "lng": 30.5234}}}]
            })),
            _ => Ok(json!({"status": "INVALID_REQUEST"})),
        }
    }
}

#[tokio::test]
async fn places_uses_one_supported_provider_request_and_keeps_declared_order() {
    let transport = Arc::new(FixtureTransport::default());
    let client = GooglePlacesClient::test_with_transport(
        config(&["places"], &json!(2)),
        transport.clone() as Arc<dyn GooglePlacesTransport>,
    );
    let result = client
        .places("coffee near Kyiv")
        .await
        .expect("bounded Google Places result");
    let result = result.as_str().expect("formatted Places result");
    assert!(result.starts_with("1. Coffee One\nAddress: 1 Main Street"));
    assert!(result.contains("2. Coffee Two"));
    assert!(result.contains("Phone: Unknown\nWebsite: Unknown"));
    assert!(!result.contains("Not Requested"));

    let requests = transport.request_urls();
    assert_eq!(requests.len(), 1);
    assert_eq!(
        requests[0],
        "https://places.googleapis.com/v1/places:searchText"
    );
    assert_eq!(
        transport.request_bodies(),
        [json!({"textQuery": "coffee near Kyiv", "pageSize": 2})]
    );
}

#[tokio::test]
async fn invocation_scoped_clients_do_not_replace_each_others_credentials() {
    let transport = Arc::new(FixtureTransport::default());
    let first = GooglePlacesToolkitConfig::parse(&settings_with_key(
        &["places"],
        &json!(1),
        "first-fixture-key",
    ))
    .expect("first Google Places configuration");
    let second = GooglePlacesToolkitConfig::parse(&settings_with_key(
        &["places"],
        &json!(1),
        "second-fixture-key",
    ))
    .expect("second Google Places configuration");
    let first = GooglePlacesClient::test_with_transport(
        first,
        transport.clone() as Arc<dyn GooglePlacesTransport>,
    );
    let second = GooglePlacesClient::test_with_transport(
        second,
        transport.clone() as Arc<dyn GooglePlacesTransport>,
    );

    first.places("first query").await.expect("first request");
    second.places("second query").await.expect("second request");

    assert_eq!(
        transport.key_headers(),
        ["first-fixture-key", "second-fixture-key"]
    );
}

#[tokio::test]
async fn nearby_search_geocodes_once_and_preserves_the_bounded_provider_results() {
    let transport = Arc::new(FixtureTransport::default());
    let client = GooglePlacesClient::test_with_transport(
        config(&["find_near"], &json!(20)),
        transport.clone() as Arc<dyn GooglePlacesTransport>,
    );
    let result = client
        .find_near("Kyiv, Ukraine", "coffee", 3_000)
        .await
        .expect("bounded nearby result");
    assert_eq!(result["location_found"], true);
    assert_eq!(result["results"][0]["name"], "Nearby Coffee");
    let requests = transport.request_urls();
    assert_eq!(requests.len(), 2);
    let nearby = requests
        .iter()
        .find(|request| request.contains("places:searchText"))
        .expect("text search request");
    assert_eq!(nearby, "https://places.googleapis.com/v1/places:searchText");
    assert!(
        requests
            .iter()
            .all(|request| !request.contains("nearbysearch"))
    );
    assert_eq!(
        transport.request_bodies(),
        [json!({
            "textQuery": "coffee",
            "pageSize": 20,
            "locationBias": {
                "circle": {
                    "center": {"latitude": 50.4501, "longitude": 30.5234},
                    "radius": 3000
                }
            }
        })]
    );
}

struct EmptyGeocodeTransport;

#[async_trait]
impl GooglePlacesTransport for EmptyGeocodeTransport {
    async fn execute_json(&self, request: Request) -> Result<Value, GooglePlacesClientError> {
        assert_eq!(request.url().path(), "/maps/api/geocode/json");
        Ok(json!({"status": "ZERO_RESULTS", "results": []}))
    }
}

#[tokio::test]
async fn missing_geocode_preserves_the_current_business_message_without_searching_places() {
    let client = GooglePlacesClient::test_with_transport(
        config(&["find_near"], &json!(20)),
        Arc::new(EmptyGeocodeTransport),
    );
    let result = client
        .find_near("Unmapped fixture location", "coffee", 3_000)
        .await
        .expect("bounded no-location result");
    assert_eq!(
        result,
        Value::String(
            "Provided current location Unmapped fixture location is not found.".to_owned()
        )
    );
}

#[test]
fn provider_failures_are_typed_retryable_and_data_free() {
    let secret = "provider-message-that-must-not-render";
    for (status, code, retryable) in [
        (
            "OVER_QUERY_LIMIT",
            GooglePlacesClientErrorCode::RateLimited,
            true,
        ),
        (
            "UNKNOWN_ERROR",
            GooglePlacesClientErrorCode::DependencyUnavailable,
            true,
        ),
        (
            "REQUEST_DENIED",
            GooglePlacesClientErrorCode::Authorization,
            false,
        ),
        (
            "OVER_DAILY_LIMIT",
            GooglePlacesClientErrorCode::Authorization,
            false,
        ),
        (
            "INVALID_REQUEST",
            GooglePlacesClientErrorCode::InvalidInput,
            false,
        ),
    ] {
        let failure = test_provider_payload(json!({
            "status": status,
            "error_message": secret
        }))
        .expect_err("provider status must not become data");
        assert_eq!(failure.code(), code);
        assert_eq!(failure.retryable(), retryable);
        assert!(!format!("{failure:?} {failure}").contains(secret));
    }

    for (status, code, retryable) in [
        (
            StatusCode::UNAUTHORIZED,
            GooglePlacesClientErrorCode::Authentication,
            false,
        ),
        (
            StatusCode::FORBIDDEN,
            GooglePlacesClientErrorCode::Authorization,
            false,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            GooglePlacesClientErrorCode::RateLimited,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            GooglePlacesClientErrorCode::DependencyUnavailable,
            true,
        ),
    ] {
        let failure = test_http_status(status).expect_err("HTTP failure status");
        assert_eq!(failure.code(), code);
        assert_eq!(failure.retryable(), retryable);
    }
}

#[derive(Default)]
struct FixtureGooglePlacesApi {
    places_calls: Mutex<Vec<String>>,
    nearby_calls: Mutex<Vec<String>>,
}

#[async_trait]
impl GooglePlacesApi for FixtureGooglePlacesApi {
    async fn places(&self, query: &str) -> Result<Value, GooglePlacesClientError> {
        self.places_calls
            .lock()
            .expect("places call fixture lock")
            .push(query.to_owned());
        Ok(json!({"places": []}))
    }

    async fn find_near(
        &self,
        current_location_query: &str,
        target: &str,
        radius: u32,
    ) -> Result<Value, GooglePlacesClientError> {
        self.nearby_calls
            .lock()
            .expect("nearby call fixture lock")
            .push(format!("{current_location_query}:{target}:{radius}"));
        Ok(json!({"results": []}))
    }
}

#[tokio::test]
async fn native_tools_preserve_current_schema_defaults_selection_and_policy() {
    let api = Arc::new(FixtureGooglePlacesApi::default());
    let toolset = test_build_with_api(
        "city-search",
        &[],
        &policy(&[("google_places", &["find_near"])]),
        &(api.clone() as Arc<dyn GooglePlacesApi>),
    )
    .expect("native Google Places toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Google Places tools");
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].name(), "places");
    assert!(tools[0].description().contains("free-text place query"));
    assert!(!tools[0].description().contains("location details"));
    assert_eq!(
        tools[0]
            .parameters_schema()
            .and_then(|schema| schema.pointer("/properties/query/maxLength").cloned()),
        Some(json!(4_096))
    );
    assert!(
        tools[0]
            .parameters_schema()
            .and_then(|schema| schema.pointer("/properties/query/description").cloned())
            .and_then(|value| value.as_str().map(ToOwned::to_owned))
            .is_some_and(|description| description.contains("coffee near Kyiv"))
    );
    tools[0]
        .execute(context(), json!({"query": "coffee"}))
        .await
        .expect("places call");
    assert_eq!(
        api.places_calls
            .lock()
            .expect("places call fixture lock")
            .as_slice(),
        &["coffee".to_owned()]
    );

    let all = test_build_with_api(
        "city-search",
        &[],
        &policy(&[]),
        &(api.clone() as Arc<dyn GooglePlacesApi>),
    )
    .expect("complete Google Places toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let all = all.tools(readonly).await.expect("complete tools");
    let near = all
        .iter()
        .find(|tool| tool.name() == "find_near")
        .expect("find_near tool");
    assert!(near.description().contains("Results may fall outside"));
    let near_schema = near
        .parameters_schema()
        .expect("find_near has an argument schema");
    let target = near_schema["properties"]["target"]["description"]
        .as_str()
        .expect("target description is text");
    assert!(target.contains("free-text place or category query"));
    assert!(target.contains("EV charging"));
    near.execute(
        context(),
        json!({
            "current_location_query": "Kyiv",
            "target": "coffee",
            "radius": null
        }),
    )
    .await
    .expect("default nearby call");
    assert_eq!(
        api.nearby_calls
            .lock()
            .expect("nearby call fixture lock")
            .as_slice(),
        &["Kyiv:coffee:3000".to_owned()]
    );
    let calls = api
        .nearby_calls
        .lock()
        .expect("nearby call fixture lock")
        .len();
    for invalid in [
        json!({"current_location_query": ""}),
        json!({"current_location_query": "Kyiv"}),
        json!({"current_location_query": "Kyiv", "target": null}),
        json!({"current_location_query": "Kyiv", "target": "coffee", "radius": 50_001}),
        json!({"current_location_query": "Kyiv", "target": "coffee", "unknown": true}),
    ] {
        assert!(near.execute(context(), invalid).await.is_err());
    }
    assert_eq!(
        api.nearby_calls
            .lock()
            .expect("nearby call fixture lock")
            .len(),
        calls
    );
}

#[test]
fn unsupported_selection_fails_before_provider_use() {
    let Err(error) = build_google_places_read_only_toolset(
        "city-search",
        config(&["unknown"], &json!(20)),
        &policy(&[]),
    ) else {
        panic!("unknown public tool must fail closed");
    };
    assert_eq!(
        error.code(),
        GooglePlacesToolsetErrorCode::UnsupportedSelection
    );
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn GooglePlacesApi> = Arc::new(FixtureGooglePlacesApi::default());
    let toolset = test_build_with_api("gate", &[], &policy(&[]), &api)
        .expect("complete google_places toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("google_places tools");
    super::sdk_conformance::assert_sdk_conformance("google_places", &tools);
}
