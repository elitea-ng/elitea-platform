//! The desktop's REST transport (ADR-0034 decision 14, contract §2.2,
//! §2.3, §2.5) over a host-built reqwest client (the desktop passes its
//! one pooled client; no redirect should be followed by it).
//!
//! The bearer goes to the deployment's origin only. A presigned URL (an
//! object store on another host) never receives it; a URL on the
//! deployment's own origin (the ADR-0016 streaming facade, for backends
//! that cannot presign) does. Main answers the facade and the multipart
//! completion as origin-relative paths (`/api/v2/elitea_core/…`), which
//! are resolved against the origin. A URL whose scheme is neither `https`
//! nor the origin's own `http` (a local stack) is refused.
//!
//! A `409 thread_busy` (another change to the thread is being saved; the
//! server compared nothing, so it is never a conflict) is answered by
//! sending the SAME request again, unchanged, after the server's
//! `Retry-After` with jittered backoff, for up to 30 s in all; an identical
//! retry returns the original result if the first one committed. When the
//! thread stays busy that long, the error is `thread_busy`.
//!
//! Errors keep the server's machine code and message: the desktop error
//! envelope is `{"error": "<code>", "message": "…"}`; the nested form
//! `{"error": {"code", "message", "details"}}` is read too, and `details`
//! (for `ref_conflict`'s `current`) from either place.

use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use async_trait::async_trait;
use reqwest::{Method, StatusCode};
use serde::Serialize;
use serde::de::DeserializeOwned;
use serde_json::{Value, json};

use crate::error::StoreError;
use crate::transport::{
    CompletedPart, FetchPlan, Multipart, PublishRequest, Published, PutTarget, StoreTransport,
    UploadGrant, UploadRequest,
};

/// The slowest upload rate an object PUT's deadline allows for.
const SLOW_LINK_BYTES_PER_SECOND: u64 = 256 * 1024;

/// An object PUT's deadline: the API timeout, or longer for a body that
/// needs it at a slow link's rate. A segment that streams through Main (a
/// store without presign) goes up in one request of up to the segment
/// ceiling.
fn transfer_timeout(base: Duration, bytes: usize) -> Duration {
    base.max(Duration::from_secs(
        (bytes as u64).div_ceil(SLOW_LINK_BYTES_PER_SECOND),
    ))
}

/// Where the bearer comes from.
#[async_trait]
pub trait TokenSource: Send + Sync {
    /// The current access token.
    ///
    /// # Errors
    ///
    /// Not signed in.
    async fn bearer(&self) -> Result<String, StoreError>;
    /// A fresh one after a 401.
    ///
    /// # Errors
    ///
    /// Not signed in any more.
    async fn refreshed(&self) -> Result<String, StoreError>;
}

/// How long and how patiently a `thread_busy` answer is retried.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct BusyRetry {
    /// Give up (with `thread_busy`) when the next wait would pass this,
    /// counted from the first attempt.
    pub deadline: Duration,
    /// No single wait is longer than this.
    pub max_wait: Duration,
}

impl Default for BusyRetry {
    fn default() -> Self {
        Self {
            deadline: Duration::from_secs(30),
            max_wait: Duration::from_secs(8),
        }
    }
}

impl BusyRetry {
    /// The wait before retry number `attempt` (from 0): the server's
    /// `Retry-After` (1 s without one), doubling every second retry, plus
    /// up to a quarter of it as jitter, within `max_wait`.
    fn wait(&self, attempt: u32, retry_after: Option<Duration>) -> Duration {
        let base = retry_after.unwrap_or(Duration::from_secs(1));
        let grown = base.saturating_mul(1u32 << (attempt / 2).min(4));
        let permille = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_or(0, |d| d.subsec_nanos() % 250);
        let jitter = grown / 1000 * permille;
        (grown + jitter).min(self.max_wait)
    }
}

/// The store routes of one thread on one deployment.
pub struct RestStore {
    client: reqwest::Client,
    origin: String,
    project_id: i64,
    conversation_uuid: String,
    tokens: Arc<dyn TokenSource>,
    headers: Vec<(String, String)>,
    timeout: Duration,
    max_get_bytes: u64,
    busy: BusyRetry,
}

impl RestStore {
    /// `origin` is `scheme://host[:port]` without a trailing slash.
    #[must_use]
    pub fn new(
        client: reqwest::Client,
        origin: &str,
        project_id: i64,
        conversation_uuid: &str,
        tokens: Arc<dyn TokenSource>,
    ) -> Self {
        Self {
            client,
            origin: origin.trim_end_matches('/').to_owned(),
            project_id,
            conversation_uuid: conversation_uuid.to_owned(),
            tokens,
            headers: Vec::new(),
            timeout: Duration::from_mins(5),
            max_get_bytes: 512 * 1024 * 1024,
            busy: BusyRetry::default(),
        }
    }

    /// How a `thread_busy` answer is retried (30 s by default).
    #[must_use]
    pub fn with_busy_retry(mut self, busy: BusyRetry) -> Self {
        self.busy = busy;
        self
    }

    /// Headers on every API call (the client version).
    #[must_use]
    pub fn with_header(mut self, name: &str, value: &str) -> Self {
        self.headers.push((name.to_owned(), value.to_owned()));
        self
    }

    /// A GET larger than this is cut off and refused.
    #[must_use]
    pub fn with_max_get_bytes(mut self, max: u64) -> Self {
        self.max_get_bytes = max;
        self
    }

    fn route(&self, name: &str) -> String {
        format!(
            "{}/api/v2/elitea_core/{name}/prompt_lib/{}/{}",
            self.origin, self.project_id, self.conversation_uuid
        )
    }

    /// The absolute URL of a grant's or a plan's `url`, and whether it is
    /// on the deployment's origin. An origin-relative path (`/api/…`, how
    /// Main answers its streaming facade and the multipart completion) is
    /// resolved against the origin; `Err` for a scheme this client does not
    /// send to.
    fn resolve(&self, url: &str) -> Result<(String, bool), StoreError> {
        if url.starts_with('/') && !url.starts_with("//") {
            return Ok((format!("{}{url}", self.origin), true));
        }
        let origin_http = self.origin.starts_with("http://");
        let scheme_ok = url.starts_with("https://") || (origin_http && url.starts_with("http://"));
        if !scheme_ok {
            return Err(StoreError::Invalid(
                "the store handed out a URL this client does not use".into(),
            ));
        }
        let same = url == self.origin || url.starts_with(&format!("{}/", self.origin));
        Ok((url.to_owned(), same))
    }

    async fn api<B: Serialize + Sync, T: DeserializeOwned>(
        &self,
        method: Method,
        url: String,
        body: Option<&B>,
    ) -> Result<T, StoreError> {
        let started = Instant::now();
        let mut busy_retries = 0u32;
        loop {
            let token = self.tokens.bearer().await?;
            let mut answer = self.api_once(&method, &url, body, &token).await?;
            if answer.status == StatusCode::UNAUTHORIZED {
                let token = self.tokens.refreshed().await?;
                answer = self.api_once(&method, &url, body, &token).await?;
            }
            if answer.is_thread_busy() {
                let wait = self.busy.wait(busy_retries, answer.retry_after);
                if started.elapsed() + wait > self.busy.deadline {
                    return Err(StoreError::http(
                        409,
                        "thread_busy",
                        format!(
                            "Another change to this thread was still being saved after {} s. Try again shortly.",
                            self.busy.deadline.as_secs()
                        ),
                    ));
                }
                tracing::debug!(busy_retries, "thread busy; the same request is sent again");
                tokio::time::sleep(wait).await;
                busy_retries += 1;
                continue;
            }
            if !answer.status.is_success() {
                return Err(error_of(answer.status.as_u16(), &answer.value));
            }
            return serde_json::from_value(answer.value).map_err(|_| {
                StoreError::Invalid("the platform's answer did not have the expected shape".into())
            });
        }
    }

    async fn api_once<B: Serialize + Sync>(
        &self,
        method: &Method,
        url: &str,
        body: Option<&B>,
        token: &str,
    ) -> Result<Answer, StoreError> {
        let mut request = self
            .client
            .request(method.clone(), url)
            .timeout(self.timeout)
            .bearer_auth(token)
            .header("Accept", "application/json");
        for (name, value) in &self.headers {
            request = request.header(name, value);
        }
        if let Some(body) = body {
            let bytes = serde_json::to_vec(body)
                .map_err(|_| StoreError::Invalid("cannot encode the request".into()))?;
            request = request
                .header("Content-Type", "application/json")
                .body(bytes);
        }
        let response = request.send().await.map_err(|e| network(&e))?;
        let status = response.status();
        let retry_after = response
            .headers()
            .get("Retry-After")
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.trim().parse::<u64>().ok())
            .map(Duration::from_secs);
        let bytes = response.bytes().await.map_err(|e| network(&e))?;
        let value = if bytes.is_empty() {
            Value::Null
        } else {
            serde_json::from_slice(&bytes).unwrap_or(Value::Null)
        };
        Ok(Answer {
            status,
            value,
            retry_after,
        })
    }

    /// A request to a grant's URL: the bearer only on our own origin.
    async fn object_request(
        &self,
        method: Method,
        url: &str,
    ) -> Result<reqwest::RequestBuilder, StoreError> {
        let (url, same_origin) = self.resolve(url)?;
        let mut request = self.client.request(method, url).timeout(self.timeout);
        if same_origin {
            request = request.bearer_auth(self.tokens.bearer().await?);
        }
        Ok(request)
    }
}

/// One API answer: the status, the JSON body (`Null` without one) and the
/// `Retry-After` seconds when the server named them.
struct Answer {
    status: StatusCode,
    value: Value,
    retry_after: Option<Duration>,
}

impl Answer {
    /// `409 thread_busy`: nothing was compared; not a `ref_conflict`.
    fn is_thread_busy(&self) -> bool {
        self.status == StatusCode::CONFLICT
            && matches!(error_of(409, &self.value), StoreError::Http { code, .. } if code == "thread_busy")
    }
}

fn network(error: &reqwest::Error) -> StoreError {
    if error.is_timeout() {
        StoreError::Network("the platform did not answer in time".into())
    } else {
        StoreError::Network("could not reach the platform".into())
    }
}

/// The server's error from a non-2xx answer.
fn error_of(status: u16, body: &Value) -> StoreError {
    let nested = body.get("error").filter(|error| error.is_object());
    let text = |value: Option<&Value>| value.and_then(Value::as_str).map(str::to_owned);
    let code = text(nested.and_then(|e| e.get("code")))
        .or_else(|| text(body.get("error")))
        .unwrap_or_else(|| format!("http_{status}"));
    let message = text(nested.and_then(|e| e.get("message")))
        .or_else(|| text(body.get("message")))
        .unwrap_or_else(|| format!("the platform answered HTTP {status}"));
    let details = nested
        .and_then(|e| e.get("details"))
        .or_else(|| body.get("details"))
        .cloned()
        .unwrap_or(Value::Null);
    StoreError::Http {
        status,
        code,
        message,
        details,
    }
}

fn object_error(status: StatusCode) -> StoreError {
    StoreError::http(
        status.as_u16(),
        &format!("http_{}", status.as_u16()),
        format!("the object store answered HTTP {}", status.as_u16()),
    )
}

#[async_trait]
impl StoreTransport for RestStore {
    async fn upload_grant(&self, request: &UploadRequest) -> Result<UploadGrant, StoreError> {
        self.api(
            Method::POST,
            self.route("workspace_store_upload"),
            Some(request),
        )
        .await
    }

    async fn put(&self, target: &PutTarget, bytes: &[u8]) -> Result<(), StoreError> {
        let mut request = self
            .object_request(Method::PUT, &target.url)
            .await?
            .timeout(transfer_timeout(self.timeout, bytes.len()));
        for (name, value) in &target.headers {
            request = request.header(name, value);
        }
        let response = request
            .body(bytes.to_vec())
            .send()
            .await
            .map_err(|e| network(&e))?;
        if response.status().is_success() {
            Ok(())
        } else {
            Err(object_error(response.status()))
        }
    }

    async fn put_part(&self, url: &str, bytes: &[u8]) -> Result<String, StoreError> {
        let response = self
            .object_request(Method::PUT, url)
            .await?
            .timeout(transfer_timeout(self.timeout, bytes.len()))
            .body(bytes.to_vec())
            .send()
            .await
            .map_err(|e| network(&e))?;
        if !response.status().is_success() {
            return Err(object_error(response.status()));
        }
        response
            .headers()
            .get("ETag")
            .and_then(|value| value.to_str().ok())
            .map(str::to_owned)
            .ok_or_else(|| {
                StoreError::Invalid("the object store did not name the part (no ETag)".into())
            })
    }

    async fn complete_multipart(
        &self,
        multipart: &Multipart,
        parts: &[CompletedPart],
    ) -> Result<(), StoreError> {
        let body = json!({ "upload_id": multipart.upload_id, "parts": parts });
        let (url, same_origin) = self.resolve(&multipart.complete_url)?;
        if same_origin {
            // Main answers 204 with no body.
            let _: Value = self.api(Method::POST, url, Some(&body)).await?;
            return Ok(());
        }
        let response = self
            .client
            .post(&url)
            .timeout(self.timeout)
            .header("Content-Type", "application/json")
            .body(body.to_string())
            .send()
            .await
            .map_err(|e| network(&e))?;
        if response.status().is_success() {
            Ok(())
        } else {
            Err(object_error(response.status()))
        }
    }

    async fn publish(&self, request: &PublishRequest) -> Result<Published, StoreError> {
        self.api(
            Method::POST,
            self.route("workspace_store_publish"),
            Some(request),
        )
        .await
    }

    async fn fetch_plan(&self, since_seq: u64) -> Result<FetchPlan, StoreError> {
        let url = format!(
            "{}?since_seq={since_seq}",
            self.route("workspace_store_fetch")
        );
        self.api::<Value, _>(Method::GET, url, None).await
    }

    async fn get(&self, url: &str) -> Result<Vec<u8>, StoreError> {
        let mut response = self
            .object_request(Method::GET, url)
            .await?
            .send()
            .await
            .map_err(|e| network(&e))?;
        if !response.status().is_success() {
            return Err(object_error(response.status()));
        }
        let mut out = Vec::new();
        while let Some(chunk) = response.chunk().await.map_err(|e| network(&e))? {
            if out.len() as u64 + chunk.len() as u64 > self.max_get_bytes {
                return Err(StoreError::Integrity(
                    "the segment is larger than any segment may be".into(),
                ));
            }
            out.extend_from_slice(&chunk);
        }
        Ok(out)
    }
}

#[cfg(test)]
mod tests {
    use super::{error_of, transfer_timeout};
    use std::time::Duration;

    #[test]
    fn a_large_object_put_gets_time_for_a_slow_link() {
        let base = Duration::from_mins(5);
        assert_eq!(transfer_timeout(base, 64 << 20), base);
        // 2 GiB at 256 KiB/s.
        assert_eq!(transfer_timeout(base, 2 << 30), Duration::from_secs(8192));
    }

    use serde_json::json;

    #[test]
    fn both_error_envelopes_are_read() {
        let flat = error_of(
            409,
            &json!({"error": "ref_conflict", "message": "moved", "details": {"current": {"a": "b"}}}),
        );
        assert_eq!(flat.code(), "ref_conflict");
        let nested = error_of(
            409,
            &json!({"error": {"code": "stale_epoch", "message": "m", "details": {"x": 1}}}),
        );
        assert_eq!(nested.code(), "stale_epoch");
        let crate::StoreError::Http { details, .. } = nested else {
            panic!()
        };
        assert_eq!(details, json!({"x": 1}));
        assert_eq!(error_of(502, &serde_json::Value::Null).code(), "http_502");
    }
}
