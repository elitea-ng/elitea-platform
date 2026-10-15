//! The desktop's REST transport (ADR-0034 decision 14, contract §2.2,
//! §2.3, §2.5) over a host-built reqwest client (the desktop passes its
//! one pooled client; no redirect should be followed by it).
//!
//! The bearer goes to the deployment's origin only. A presigned URL (an
//! object store on another host) never receives it; a URL on the
//! deployment's own origin (the ADR-0016 streaming facade, for backends
//! that cannot presign) does. A URL whose scheme is neither `https` nor
//! the origin's own `http` (a local stack) is refused.
//!
//! Errors keep the server's machine code and message: the desktop error
//! envelope is `{"error": "<code>", "message": "…"}`; the nested form
//! `{"error": {"code", "message", "details"}}` is read too, and `details`
//! (for `ref_conflict`'s `current`) from either place.

use std::sync::Arc;
use std::time::Duration;

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
        }
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

    /// Whether `url` is on the deployment's origin; `Err` for a scheme this
    /// client does not send to.
    fn same_origin(&self, url: &str) -> Result<bool, StoreError> {
        let origin_http = self.origin.starts_with("http://");
        let scheme_ok = url.starts_with("https://") || (origin_http && url.starts_with("http://"));
        if !scheme_ok {
            return Err(StoreError::Invalid(
                "the store handed out a URL this client does not use".into(),
            ));
        }
        Ok(url == self.origin || url.starts_with(&format!("{}/", self.origin)))
    }

    async fn api<B: Serialize + Sync, T: DeserializeOwned>(
        &self,
        method: Method,
        url: String,
        body: Option<&B>,
    ) -> Result<T, StoreError> {
        let token = self.tokens.bearer().await?;
        let (status, value) = self.api_once(&method, &url, body, &token).await?;
        let (status, value) = if status == StatusCode::UNAUTHORIZED {
            let token = self.tokens.refreshed().await?;
            self.api_once(&method, &url, body, &token).await?
        } else {
            (status, value)
        };
        if !status.is_success() {
            return Err(error_of(status.as_u16(), &value));
        }
        serde_json::from_value(value).map_err(|_| {
            StoreError::Invalid("the platform's answer did not have the expected shape".into())
        })
    }

    async fn api_once<B: Serialize + Sync>(
        &self,
        method: &Method,
        url: &str,
        body: Option<&B>,
        token: &str,
    ) -> Result<(StatusCode, Value), StoreError> {
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
        let bytes = response.bytes().await.map_err(|e| network(&e))?;
        let value = if bytes.is_empty() {
            Value::Null
        } else {
            serde_json::from_slice(&bytes).unwrap_or(Value::Null)
        };
        Ok((status, value))
    }

    /// A request to a grant's URL: the bearer only on our own origin.
    async fn object_request(
        &self,
        method: Method,
        url: &str,
    ) -> Result<reqwest::RequestBuilder, StoreError> {
        let mut request = self.client.request(method, url).timeout(self.timeout);
        if self.same_origin(url)? {
            request = request.bearer_auth(self.tokens.bearer().await?);
        }
        Ok(request)
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
        let mut request = self.object_request(Method::PUT, &target.url).await?;
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
        if self.same_origin(&multipart.complete_url)? {
            let _: Value = self
                .api(Method::POST, multipart.complete_url.clone(), Some(&body))
                .await?;
            return Ok(());
        }
        let response = self
            .client
            .post(&multipart.complete_url)
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
    use super::error_of;
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
