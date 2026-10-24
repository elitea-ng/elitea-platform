//! One file per provider. Each maps the normalised request to the vendor's
//! API, the vendor's answer back, and the UNITS the call consumed; none
//! talks to the meter, builds a client or knows about other providers.

pub(crate) mod brave;
pub(crate) mod ddg_instant;
pub(crate) mod firecrawl;
pub(crate) mod shodan;
pub(crate) mod tavily;

use std::time::Duration;

use reqwest::header::{ACCEPT, AUTHORIZATION, CONTENT_TYPE, HeaderName, HeaderValue};
use reqwest::{Method, Request, Url};
use serde_json::{Map, Value};

use super::transport::ProviderError;

/// Search and host queries: 15 s.
pub(crate) const QUERY_TIMEOUT: Duration = Duration::from_secs(15);
/// Provider-backed fetch: 20 s.
pub(crate) const FETCH_TIMEOUT: Duration = Duration::from_secs(20);
/// A search or host answer is at most 2 MiB.
pub(crate) const MAX_QUERY_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
/// A provider fetch answer is at most 5 MiB.
pub(crate) const MAX_FETCH_RESPONSE_BYTES: usize = 5 * 1_024 * 1_024;
/// The longest string field copied out of a provider answer.
const MAX_FIELD_BYTES: usize = 8 * 1_024;

/// A request with a JSON body and a bearer key.
pub(crate) fn post_json(
    url: &str,
    key: &str,
    body: &Value,
    timeout: Duration,
) -> Result<Request, ProviderError> {
    let url = Url::parse(url).map_err(|_| ProviderError::BadRequest)?;
    let mut request = Request::new(Method::POST, url);
    let mut bearer =
        HeaderValue::from_str(&format!("Bearer {key}")).map_err(|_| ProviderError::AuthFailed)?;
    bearer.set_sensitive(true);
    let headers = request.headers_mut();
    headers.insert(AUTHORIZATION, bearer);
    headers.insert(ACCEPT, HeaderValue::from_static("application/json"));
    headers.insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
    *request.body_mut() = Some(
        serde_json::to_vec(body)
            .map_err(|_| ProviderError::BadRequest)?
            .into(),
    );
    *request.timeout_mut() = Some(timeout);
    Ok(request)
}

/// A GET with the given query pairs, and optionally a sensitive key header.
pub(crate) fn get(
    url: &str,
    query: &[(&str, &str)],
    key_header: Option<(HeaderName, &str)>,
    timeout: Duration,
) -> Result<Request, ProviderError> {
    let mut url = Url::parse(url).map_err(|_| ProviderError::BadRequest)?;
    if !query.is_empty() {
        url.query_pairs_mut().extend_pairs(query);
    }
    let mut request = Request::new(Method::GET, url);
    request
        .headers_mut()
        .insert(ACCEPT, HeaderValue::from_static("application/json"));
    if let Some((name, key)) = key_header {
        let mut value = HeaderValue::from_str(key).map_err(|_| ProviderError::AuthFailed)?;
        value.set_sensitive(true);
        request.headers_mut().insert(name, value);
    }
    *request.timeout_mut() = Some(timeout);
    Ok(request)
}

/// A bounded string field.
pub(crate) fn text(object: &Map<String, Value>, key: &str) -> Option<String> {
    object
        .get(key)
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(|value| super::capability::cut(value, MAX_FIELD_BYTES).0.to_owned())
}

/// The host of a result URL, its `source`.
pub(crate) fn host_of(url: &str) -> Option<String> {
    Url::parse(url)
        .ok()
        .and_then(|url| url.host_str().map(str::to_owned))
}

/// Whether a result URL is an http(s) URL worth returning.
pub(crate) fn web_url(url: &str) -> bool {
    Url::parse(url).is_ok_and(|url| matches!(url.scheme(), "http" | "https"))
}

/// `value` as an object, or the provider answered something unreadable.
pub(crate) fn object(value: &Value) -> Result<&Map<String, Value>, ProviderError> {
    value.as_object().ok_or(ProviderError::InvalidResponse)
}
