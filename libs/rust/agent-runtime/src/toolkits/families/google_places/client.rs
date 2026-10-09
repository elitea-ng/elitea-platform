use std::fmt;
use std::fmt::Write as _;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use reqwest::header::{ACCEPT, CONTENT_LENGTH, CONTENT_TYPE, HeaderName, HeaderValue};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value, json};

use super::config::GooglePlacesToolkitConfig;

const GOOGLE_MAPS_ORIGIN: &str = "https://maps.googleapis.com";
const GOOGLE_PLACES_ORIGIN: &str = "https://places.googleapis.com";
const SEARCH_TEXT_PATH: &str = "/v1/places:searchText";
const GEOCODE_PATH: &str = "/maps/api/geocode/json";
const GOOGLE_API_KEY: HeaderName = HeaderName::from_static("x-goog-api-key");
const GOOGLE_FIELD_MASK: HeaderName = HeaderName::from_static("x-goog-fieldmask");
const PLACE_FIELD_MASK: &str = "places.id,places.displayName,places.formattedAddress,places.nationalPhoneNumber,places.websiteUri,places.location";
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_CHARS: usize = 200_000;
const MAX_QUERY_BYTES: usize = 4 * 1_024;
const MAX_PLACE_ID_BYTES: usize = 1_024;
const MAX_RESULT_STRING_BYTES: usize = 16 * 1_024;
const MAX_URL_BYTES: usize = 4 * 1_024;
const MAX_SEARCH_RESULTS: usize = 20;
const MAX_RADIUS_METERS: u32 = 50_000;
const MAX_REQUEST_BYTES: usize = 32 * 1_024;
const USER_AGENT: &str = "elitea-worker-rust/0.1";

/// Stable, secret-free Google Maps provider failure categories.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum GooglePlacesClientErrorCode {
    InvalidConfiguration,
    InvalidInput,
    Authentication,
    Authorization,
    NotFound,
    RateLimited,
    Timeout,
    DependencyUnavailable,
    InvalidResponse,
    ResourceExhausted,
}

/// A bounded provider or request failure that never retains a URL, API key,
/// query, response body, or upstream message.
pub struct GooglePlacesClientError {
    code: GooglePlacesClientErrorCode,
}

impl GooglePlacesClientError {
    #[must_use]
    pub const fn code(&self) -> GooglePlacesClientErrorCode {
        self.code
    }

    #[must_use]
    pub const fn retryable(&self) -> bool {
        matches!(
            self.code,
            GooglePlacesClientErrorCode::RateLimited
                | GooglePlacesClientErrorCode::Timeout
                | GooglePlacesClientErrorCode::DependencyUnavailable
        )
    }

    pub fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            GooglePlacesClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "google_places.configuration.invalid",
                "the Google Places toolkit configuration is invalid",
            ),
            GooglePlacesClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "google_places.request.invalid",
                "the Google Places request is invalid",
            ),
            GooglePlacesClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "google_places.authentication.failed",
                "Google Places authentication failed",
            ),
            GooglePlacesClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "google_places.authorization.failed",
                "Google Places did not authorize the request",
            ),
            GooglePlacesClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "google_places.resource.not_found",
                "the requested Google Places resource was not found",
            ),
            GooglePlacesClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "google_places.rate_limited",
                "Google Places rate limited the request",
            ),
            GooglePlacesClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "google_places.timeout",
                "the Google Places request timed out",
            ),
            GooglePlacesClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "google_places.unavailable",
                "Google Places is unavailable",
            ),
            GooglePlacesClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "google_places.response.invalid",
                "Google Places returned an invalid response",
            ),
            GooglePlacesClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "google_places.response.resource_exhausted",
                "the Google Places response exceeds the approved limit",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable(),
            retry_after_ms: None,
            max_attempts: None,
        })
    }
}

impl fmt::Debug for GooglePlacesClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GooglePlacesClientError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for GooglePlacesClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            GooglePlacesClientErrorCode::InvalidConfiguration => {
                "the Google Places client configuration is invalid"
            }
            GooglePlacesClientErrorCode::InvalidInput => "the Google Places request is invalid",
            GooglePlacesClientErrorCode::Authentication => "Google Places authentication failed",
            GooglePlacesClientErrorCode::Authorization => "Google Places authorization failed",
            GooglePlacesClientErrorCode::NotFound => "the Google Places resource was not found",
            GooglePlacesClientErrorCode::RateLimited => "Google Places rate limited the request",
            GooglePlacesClientErrorCode::Timeout => "the Google Places request timed out",
            GooglePlacesClientErrorCode::DependencyUnavailable => "Google Places is unavailable",
            GooglePlacesClientErrorCode::InvalidResponse => {
                "Google Places returned an invalid response"
            }
            GooglePlacesClientErrorCode::ResourceExhausted => {
                "the Google Places response exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for GooglePlacesClientError {}

/// The two complete read operations exposed by the current SDK family.
#[async_trait]
pub trait GooglePlacesApi: Send + Sync {
    async fn places(&self, query: &str) -> Result<Value, GooglePlacesClientError>;

    async fn find_near(
        &self,
        current_location_query: &str,
        target: &str,
        radius: u32,
    ) -> Result<Value, GooglePlacesClientError>;
}

#[derive(Clone, Copy)]
pub enum GooglePlacesRequestKind {
    SearchText,
    Geocode,
}

#[async_trait]
pub trait GooglePlacesTransport: Send + Sync {
    async fn execute_json(&self, request: Request) -> Result<Value, GooglePlacesClientError>;
}

struct ReqwestGooglePlacesTransport {
    http: reqwest::Client,
}

#[async_trait]
impl GooglePlacesTransport for ReqwestGooglePlacesTransport {
    async fn execute_json(&self, request: Request) -> Result<Value, GooglePlacesClientError> {
        let mut response = self
            .http
            .execute(request)
            .await
            .map_err(|source| map_reqwest_error(&source))?;
        map_http_status(response.status())?;
        if !response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.split(';').next())
            .is_some_and(|value| value.trim().eq_ignore_ascii_case("application/json"))
        {
            return Err(invalid_response());
        }
        if response
            .headers()
            .get(CONTENT_LENGTH)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.parse::<usize>().ok())
            .is_some_and(|length| length > MAX_RESPONSE_BYTES)
        {
            return Err(resource_exhausted());
        }
        let mut body = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|source| map_reqwest_error(&source))?
        {
            let next = body
                .len()
                .checked_add(chunk.len())
                .ok_or_else(resource_exhausted)?;
            if next > MAX_RESPONSE_BYTES {
                return Err(resource_exhausted());
            }
            body.extend_from_slice(&chunk);
        }
        serde_json::from_slice(&body).map_err(|_| invalid_response())
    }
}

/// One invocation-scoped, pooled Google Maps client.
pub struct GooglePlacesClient {
    config: GooglePlacesToolkitConfig,
    transport: Arc<dyn GooglePlacesTransport>,
}

impl GooglePlacesClient {
    pub fn new(config: GooglePlacesToolkitConfig) -> Result<Self, GooglePlacesClientError> {
        let http = reqwest::Client::builder()
            .https_only(true)
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(REQUEST_TIMEOUT)
            .pool_idle_timeout(POOL_IDLE_TIMEOUT)
            .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
            .user_agent(USER_AGENT)
            .build()
            .map_err(|_| invalid_configuration())?;
        Ok(Self {
            config,
            transport: Arc::new(ReqwestGooglePlacesTransport { http }),
        })
    }

    fn build_search_text_request(&self, body: &Value) -> Result<Request, GooglePlacesClientError> {
        let endpoint = endpoint(GOOGLE_PLACES_ORIGIN, SEARCH_TEXT_PATH)?;
        let encoded = serde_json::to_vec(&body).map_err(|_| invalid_input())?;
        if encoded.len() > MAX_REQUEST_BYTES {
            return Err(resource_exhausted());
        }
        let mut key =
            HeaderValue::from_str(self.config.api_key()).map_err(|_| invalid_configuration())?;
        key.set_sensitive(true);
        let mut request = Request::new(Method::POST, endpoint);
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static("application/json"));
        request
            .headers_mut()
            .insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
        request.headers_mut().insert(GOOGLE_API_KEY, key);
        request.headers_mut().insert(
            GOOGLE_FIELD_MASK,
            HeaderValue::from_static(PLACE_FIELD_MASK),
        );
        *request.body_mut() = Some(encoded.into());
        *request.timeout_mut() = Some(REQUEST_TIMEOUT);
        Ok(request)
    }

    fn build_geocode_request(&self, address: &str) -> Result<Request, GooglePlacesClientError> {
        let mut endpoint = endpoint(GOOGLE_MAPS_ORIGIN, GEOCODE_PATH)?;
        endpoint
            .query_pairs_mut()
            .append_pair("address", address)
            .append_pair("key", self.config.api_key());
        let mut request = Request::new(Method::GET, endpoint);
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static("application/json"));
        *request.timeout_mut() = Some(REQUEST_TIMEOUT);
        Ok(request)
    }

    async fn search_text(
        &self,
        body: Value,
    ) -> Result<Map<String, Value>, GooglePlacesClientError> {
        let request = self.build_search_text_request(&body)?;
        let response = self.transport.execute_json(request).await?;
        places_payload(response)
    }

    async fn geocode(&self, address: &str) -> Result<Map<String, Value>, GooglePlacesClientError> {
        let request = self.build_geocode_request(address)?;
        let response = self.transport.execute_json(request).await?;
        legacy_provider_payload(response)
    }
}

#[async_trait]
impl GooglePlacesApi for GooglePlacesClient {
    async fn places(&self, query: &str) -> Result<Value, GooglePlacesClientError> {
        validate_text(query, MAX_QUERY_BYTES)?;
        let payload = self
            .search_text(json!({
                "textQuery": query,
                "pageSize": self.config.results_count()
            }))
            .await?;
        let results = optional_bounded_array(payload.get("places"), MAX_SEARCH_RESULTS)?;
        let results = &results[..results.len().min(self.config.results_count())];
        if results.is_empty() {
            return Ok(Value::String(
                "Google Places did not find any places that match the description.".to_owned(),
            ));
        }
        let mut output = String::new();
        for (index, result) in results.iter().enumerate() {
            let place = project_place(result)?;
            write!(
                output,
                "{}. {}\nAddress: {}\nPlace ID: {}\nPhone: {}\nWebsite: {}",
                index + 1,
                place.name,
                place.address,
                place.id,
                place.phone,
                place.website
            )
            .map_err(|_| invalid_response())?;
            if index + 1 < results.len() {
                output.push_str("\n\n");
            }
            if output.chars().count() > MAX_OUTPUT_CHARS {
                return Err(resource_exhausted());
            }
        }
        Ok(Value::String(output))
    }

    async fn find_near(
        &self,
        current_location_query: &str,
        target: &str,
        radius: u32,
    ) -> Result<Value, GooglePlacesClientError> {
        validate_text(current_location_query, MAX_QUERY_BYTES)?;
        validate_text(target, MAX_QUERY_BYTES)?;
        if !(1..=MAX_RADIUS_METERS).contains(&radius) {
            return Err(invalid_input());
        }
        let geocode = self.geocode(current_location_query).await?;
        let geocode_results = bounded_array(geocode.get("results"), MAX_SEARCH_RESULTS)?;
        let Some(first) = geocode_results.first() else {
            return Ok(Value::String(format!(
                "Provided current location {current_location_query} is not found."
            )));
        };
        let (latitude, longitude) = geocode_location(first)?;
        let nearby = self
            .search_text(json!({
                "textQuery": target,
                "pageSize": MAX_SEARCH_RESULTS,
                "locationBias": {
                    "circle": {
                        "center": {
                            "latitude": latitude,
                            "longitude": longitude
                        },
                        "radius": radius
                    }
                }
            }))
            .await?;
        let results = optional_bounded_array(nearby.get("places"), MAX_SEARCH_RESULTS)?;
        let projected = results
            .iter()
            .map(project_place_json)
            .collect::<Result<Vec<_>, _>>()?;
        bounded_output(json!({
            "location_found": true,
            "results": projected
        }))
    }
}

fn places_payload(response: Value) -> Result<Map<String, Value>, GooglePlacesClientError> {
    let Value::Object(payload) = response else {
        return Err(invalid_response());
    };
    if payload.contains_key("error") {
        return Err(invalid_response());
    }
    let _ = optional_bounded_array(payload.get("places"), MAX_SEARCH_RESULTS)?;
    Ok(payload)
}

fn legacy_provider_payload(response: Value) -> Result<Map<String, Value>, GooglePlacesClientError> {
    let Value::Object(payload) = response else {
        return Err(invalid_response());
    };
    let status = payload
        .get("status")
        .and_then(Value::as_str)
        .ok_or_else(invalid_response)?
        .to_owned();
    match status.as_str() {
        "OK" | "ZERO_RESULTS" => Ok(payload),
        "INVALID_REQUEST" => Err(invalid_input()),
        "REQUEST_DENIED" | "OVER_DAILY_LIMIT" => {
            Err(error(GooglePlacesClientErrorCode::Authorization))
        }
        "OVER_QUERY_LIMIT" => Err(error(GooglePlacesClientErrorCode::RateLimited)),
        "NOT_FOUND" => Err(error(GooglePlacesClientErrorCode::NotFound)),
        "UNKNOWN_ERROR" => Err(dependency_unavailable()),
        _ => Err(invalid_response()),
    }
}

struct ProjectedPlace {
    id: String,
    name: String,
    address: String,
    phone: String,
    website: String,
    location: Option<Value>,
}

fn project_place(value: &Value) -> Result<ProjectedPlace, GooglePlacesClientError> {
    let place = value.as_object().ok_or_else(invalid_response)?;
    let id =
        optional_text(place.get("id"), MAX_PLACE_ID_BYTES)?.unwrap_or_else(|| "Unknown".to_owned());
    let name = match place.get("displayName") {
        None | Some(Value::Null) => "Unknown".to_owned(),
        Some(Value::Object(display_name)) => {
            optional_text(display_name.get("text"), MAX_RESULT_STRING_BYTES)?
                .unwrap_or_else(|| "Unknown".to_owned())
        }
        Some(_) => return Err(invalid_response()),
    };
    let address = optional_text(place.get("formattedAddress"), MAX_RESULT_STRING_BYTES)?
        .unwrap_or_else(|| "Unknown".to_owned());
    let phone = optional_text(place.get("nationalPhoneNumber"), MAX_RESULT_STRING_BYTES)?
        .unwrap_or_else(|| "Unknown".to_owned());
    let website = optional_text(place.get("websiteUri"), MAX_URL_BYTES)?
        .unwrap_or_else(|| "Unknown".to_owned());
    let location = optional_location(place.get("location"))?;
    Ok(ProjectedPlace {
        id,
        name,
        address,
        phone,
        website,
        location,
    })
}

fn project_place_json(value: &Value) -> Result<Value, GooglePlacesClientError> {
    let place = project_place(value)?;
    Ok(json!({
        "place_id": place.id,
        "name": place.name,
        "formatted_address": place.address,
        "formatted_phone_number": place.phone,
        "website": place.website,
        "location": place.location
    }))
}

fn optional_location(value: Option<&Value>) -> Result<Option<Value>, GooglePlacesClientError> {
    match value {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Object(location)) => {
            let latitude = location
                .get("latitude")
                .and_then(Value::as_f64)
                .filter(|value| value.is_finite() && (-90.0..=90.0).contains(value))
                .ok_or_else(invalid_response)?;
            let longitude = location
                .get("longitude")
                .and_then(Value::as_f64)
                .filter(|value| value.is_finite() && (-180.0..=180.0).contains(value))
                .ok_or_else(invalid_response)?;
            Ok(Some(json!({
                "latitude": latitude,
                "longitude": longitude
            })))
        }
        Some(_) => Err(invalid_response()),
    }
}

fn geocode_location(value: &Value) -> Result<(f64, f64), GooglePlacesClientError> {
    let location = value
        .pointer("/geometry/location")
        .and_then(Value::as_object)
        .ok_or_else(invalid_response)?;
    let latitude = location
        .get("lat")
        .and_then(Value::as_f64)
        .filter(|value| value.is_finite() && (-90.0..=90.0).contains(value))
        .ok_or_else(invalid_response)?;
    let longitude = location
        .get("lng")
        .and_then(Value::as_f64)
        .filter(|value| value.is_finite() && (-180.0..=180.0).contains(value))
        .ok_or_else(invalid_response)?;
    Ok((latitude, longitude))
}

fn bounded_array(
    value: Option<&Value>,
    maximum: usize,
) -> Result<&[Value], GooglePlacesClientError> {
    let values = value
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    if values.len() > maximum {
        return Err(resource_exhausted());
    }
    Ok(values)
}

fn optional_bounded_array(
    value: Option<&Value>,
    maximum: usize,
) -> Result<&[Value], GooglePlacesClientError> {
    match value {
        None | Some(Value::Null) => Ok(&[]),
        Some(Value::Array(values)) if values.len() <= maximum => Ok(values),
        Some(Value::Array(_)) => Err(resource_exhausted()),
        Some(_) => Err(invalid_response()),
    }
}

fn optional_text(
    value: Option<&Value>,
    maximum: usize,
) -> Result<Option<String>, GooglePlacesClientError> {
    match value {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => {
            if value.len() > maximum {
                return Err(resource_exhausted());
            }
            if value.bytes().any(|byte| byte == 0) {
                return Err(invalid_response());
            }
            Ok(Some(value.clone()))
        }
        Some(_) => Err(invalid_response()),
    }
}

fn validate_text(value: &str, maximum: usize) -> Result<(), GooglePlacesClientError> {
    if value.len() > maximum {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n')) {
        return Err(invalid_input());
    }
    Ok(())
}

fn endpoint(origin: &str, path: &str) -> Result<Url, GooglePlacesClientError> {
    let mut endpoint = Url::parse(origin).map_err(|_| invalid_configuration())?;
    endpoint.set_path(path);
    Ok(endpoint)
}

fn bounded_output(value: Value) -> Result<Value, GooglePlacesClientError> {
    let length = serde_json::to_string(&value)
        .map_err(|_| invalid_response())?
        .chars()
        .count();
    if length > MAX_OUTPUT_CHARS {
        return Err(resource_exhausted());
    }
    Ok(value)
}

fn map_http_status(status: StatusCode) -> Result<(), GooglePlacesClientError> {
    match status {
        StatusCode::OK => Ok(()),
        StatusCode::BAD_REQUEST => Err(invalid_input()),
        StatusCode::UNAUTHORIZED => Err(error(GooglePlacesClientErrorCode::Authentication)),
        StatusCode::FORBIDDEN => Err(error(GooglePlacesClientErrorCode::Authorization)),
        StatusCode::NOT_FOUND => Err(error(GooglePlacesClientErrorCode::NotFound)),
        StatusCode::REQUEST_TIMEOUT | StatusCode::GATEWAY_TIMEOUT => {
            Err(error(GooglePlacesClientErrorCode::Timeout))
        }
        StatusCode::TOO_MANY_REQUESTS => Err(error(GooglePlacesClientErrorCode::RateLimited)),
        status if status.is_server_error() => Err(dependency_unavailable()),
        _ => Err(invalid_response()),
    }
}

fn map_reqwest_error(source: &reqwest::Error) -> GooglePlacesClientError {
    if source.is_timeout() {
        return error(GooglePlacesClientErrorCode::Timeout);
    }
    if source.is_connect() || source.is_request() || source.is_body() {
        return dependency_unavailable();
    }
    invalid_response()
}

const fn error(code: GooglePlacesClientErrorCode) -> GooglePlacesClientError {
    GooglePlacesClientError { code }
}

const fn invalid_configuration() -> GooglePlacesClientError {
    error(GooglePlacesClientErrorCode::InvalidConfiguration)
}

const fn invalid_input() -> GooglePlacesClientError {
    error(GooglePlacesClientErrorCode::InvalidInput)
}

const fn invalid_response() -> GooglePlacesClientError {
    error(GooglePlacesClientErrorCode::InvalidResponse)
}

const fn resource_exhausted() -> GooglePlacesClientError {
    error(GooglePlacesClientErrorCode::ResourceExhausted)
}

const fn dependency_unavailable() -> GooglePlacesClientError {
    error(GooglePlacesClientErrorCode::DependencyUnavailable)
}

#[cfg(any(test, feature = "test-support"))]
impl GooglePlacesClient {
    pub fn test_with_transport(
        config: GooglePlacesToolkitConfig,
        transport: Arc<dyn GooglePlacesTransport>,
    ) -> Self {
        Self { config, transport }
    }

    pub fn test_request(
        &self,
        kind: GooglePlacesRequestKind,
        value: &str,
    ) -> Result<Request, GooglePlacesClientError> {
        match kind {
            GooglePlacesRequestKind::SearchText => self.build_search_text_request(&json!({
                "textQuery": value,
                "pageSize": self.config.results_count()
            })),
            GooglePlacesRequestKind::Geocode => self.build_geocode_request(value),
        }
    }
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_provider_payload(
    response: Value,
) -> Result<Map<String, Value>, GooglePlacesClientError> {
    legacy_provider_payload(response)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_http_status(status: StatusCode) -> Result<(), GooglePlacesClientError> {
    map_http_status(status)
}
