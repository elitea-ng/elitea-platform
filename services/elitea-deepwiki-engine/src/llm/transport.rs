//! The HTTP half of the model client: one `reqwest` client, the retry loop,
//! bounded body reads, and the mapping of every failure onto the engine's
//! error contract.
//!
//! # Retries
//!
//! A port of the `openai` SDK's policy, which is what `LangChain`'s
//! `max_retries=2` meant in the Python engine: up to `max_retries` more
//! attempts after a connection failure, a timeout, or HTTP 408, 409, 429 or
//! 5xx; the delay is the server's `retry-after-ms` / `retry-after` when it
//! is between 0 and 60 s, else `min(0.5 s · 2^n, 8 s)` less up to 25 %
//! jitter. A stop request ends a wait at once.
//!
//! # Errors
//!
//! `errors::classify` decides the category from the message, so each
//! message here is worded for the category it must land in:
//!
//! | Failure | Type | Category |
//! | --- | --- | --- |
//! | stop requested | `RuntimeError` | `runtime_error` (the stop line) |
//! | timeout, HTTP 504 | `RuntimeError` | `timeout_error` |
//! | HTTP 429 / 503 after the retries | `RuntimeError` | `service_busy` |
//! | HTTP 402 (budget) | `ValueError` | `invalid_input` |
//! | HTTP 404 (unknown model) | `RuntimeError` | `resource_not_found` |
//! | any other refusal, a malformed reply | `RuntimeError` | `inference_failed` |
//!
//! No message carries the API key. Text the gateway sent back is cut to a
//! few hundred characters and has the key replaced before it is used,
//! because a misconfigured upstream can echo request headers.

use crate::errors::{EngineError, ErrorType};
use crate::ingest::secret::Secret;
use crate::runner::StopSignal;
use reqwest::header::{ACCEPT, AUTHORIZATION, CONTENT_TYPE, HeaderMap, HeaderValue};
use reqwest::{Client, Response, StatusCode};
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

/// How much of a refusal body is read to find its message.
const MAX_ERROR_BODY_BYTES: usize = 16 * 1024;

/// How much of an upstream message an error repeats.
const MAX_UPSTREAM_MESSAGE_CHARS: usize = 300;

/// The timeouts of one transport.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Timeouts {
    /// TCP and TLS set-up.
    pub connect: Duration,
    /// One non-streaming call, body included. The `openai` SDK's default
    /// is 600 s; a 64k-token completion from a slow model needs most of it.
    pub request: Duration,
    /// The longest silence a stream may keep: before the response head,
    /// and between two chunks. A reasoning model may think for minutes
    /// before its first token, so this is generous.
    pub stream_idle: Duration,
    /// One whole stream. A reasoning model writing a 48k-token page at
    /// ~10 tokens/s streams for 80 minutes, so this is a backstop, not a
    /// pace; the idle timeout catches a stalled stream. Python had none.
    pub stream_total: Duration,
}

impl Default for Timeouts {
    fn default() -> Self {
        Self {
            connect: Duration::from_secs(10),
            request: Duration::from_mins(10),
            stream_idle: Duration::from_mins(5),
            stream_total: Duration::from_hours(2),
        }
    }
}

/// The backoff of the retry loop.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Backoff {
    pub initial: Duration,
    pub max: Duration,
    /// A `retry-after` above this is ignored for the computed delay, as
    /// the SDK does: a server asking for minutes is not worth waiting on.
    pub max_retry_after: Duration,
}

impl Default for Backoff {
    fn default() -> Self {
        Self {
            initial: Duration::from_millis(500),
            max: Duration::from_secs(8),
            max_retry_after: Duration::from_mins(1),
        }
    }
}

/// Process-wide transport settings: what the environment decides, not the
/// invocation.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct TransportSettings {
    /// A PEM bundle trusted IN ADDITION to the platform roots
    /// (`ELITEA_DEEPWIKI_TLS_CA_FILE`): the gateway of an install with a
    /// private CA, while public endpoints keep working.
    pub ca_file: Option<PathBuf>,
    pub timeouts: Timeouts,
    pub backoff: Backoff,
}

/// A configured HTTP client. Cheap to clone; share one per process.
#[derive(Debug, Clone)]
pub struct Transport {
    client: Client,
    timeouts: Timeouts,
    backoff: Backoff,
}

fn runtime(message: impl Into<String>) -> EngineError {
    EngineError::new(ErrorType::Runtime, message)
}

fn read_ca_file(path: &Path) -> Result<Vec<reqwest::Certificate>, EngineError> {
    // Worded without "not found": a missing CA file is a deployment error,
    // not a missing wiki.
    let pem = std::fs::read(path).map_err(|error| {
        // io's own text for a missing file is "entity not found", which
        // the classifier would file as a missing resource.
        let reason = match error.kind() {
            std::io::ErrorKind::NotFound => "no such file".to_owned(),
            other => other.to_string(),
        };
        runtime(format!(
            "ELITEA_DEEPWIKI_TLS_CA_FILE {} cannot be read: {reason}",
            path.display(),
        ))
    })?;
    let certificates = reqwest::Certificate::from_pem_bundle(&pem).map_err(|_| {
        runtime(format!(
            "ELITEA_DEEPWIKI_TLS_CA_FILE {} is not a PEM certificate bundle",
            path.display()
        ))
    })?;
    if certificates.is_empty() {
        return Err(runtime(format!(
            "ELITEA_DEEPWIKI_TLS_CA_FILE {} holds no certificate",
            path.display()
        )));
    }
    Ok(certificates)
}

impl Transport {
    /// Build the client.
    ///
    /// Redirects are refused: a 3xx from a model gateway is a
    /// misconfiguration, and following one would carry the bearer key to a
    /// host nobody chose.
    ///
    /// # Errors
    ///
    /// A `RuntimeError` when the CA file cannot be read or the TLS stack
    /// cannot be set up.
    pub fn new(settings: &TransportSettings) -> Result<Self, EngineError> {
        let mut builder = Client::builder()
            .connect_timeout(settings.timeouts.connect)
            .redirect(reqwest::redirect::Policy::none())
            .user_agent(concat!(
                "elitea-deepwiki-engine/",
                env!("CARGO_PKG_VERSION")
            ));
        if let Some(path) = &settings.ca_file {
            builder = builder.tls_certs_merge(read_ca_file(path)?);
        }
        let client = builder.build().map_err(|error| {
            runtime(format!(
                "The model client cannot start its TLS stack: {}",
                error_chain(&error)
            ))
        })?;
        Ok(Self {
            client,
            timeouts: settings.timeouts,
            backoff: settings.backoff,
        })
    }

    #[must_use]
    pub fn timeouts(&self) -> Timeouts {
        self.timeouts
    }

    /// The configured client, for the platform's object API (an
    /// artifact-folder source): the same TLS stack, CA bundle and refusal
    /// of redirects as the model calls.
    #[must_use]
    pub fn http_client(&self) -> &Client {
        &self.client
    }
}

/// What one request is, for its error messages and its retries.
#[derive(Debug, Clone, Copy)]
pub(crate) struct Call<'a> {
    /// "Embedding" or "Chat completion".
    pub what: &'static str,
    pub url: &'a str,
    pub model: &'a str,
    pub key: &'a Secret,
    pub organization: Option<&'a str>,
    pub max_retries: u32,
    pub streaming: bool,
}

/// `source()` chain of a transport error, for a message. reqwest's own
/// text names the URL, which holds no credential (see `settings`).
pub(crate) fn error_chain(error: &dyn std::error::Error) -> String {
    let mut text = error.to_string();
    let mut source = error.source();
    while let Some(cause) = source {
        text.push_str(": ");
        text.push_str(&cause.to_string());
        source = cause.source();
    }
    text
}

/// Replace the key wherever it appears, then cut to length.
pub(crate) fn sanitize(text: &str, key: &Secret) -> String {
    let secret = key.expose();
    let cleaned = if secret.is_empty() {
        text.to_owned()
    } else {
        text.replace(secret, "<redacted>")
    };
    let mut cut: String = cleaned.chars().take(MAX_UPSTREAM_MESSAGE_CHARS).collect();
    if cut.len() < cleaned.len() {
        cut.push('…');
    }
    cut
}

/// The SDK's `_calculate_retry_timeout`.
fn retry_delay(backoff: Backoff, retry: u32, headers: &HeaderMap) -> Duration {
    if let Some(delay) = retry_after(headers).filter(|d| *d <= backoff.max_retry_after) {
        return delay;
    }
    let factor = 2_u32.saturating_pow(retry.min(16));
    let base = backoff.initial.saturating_mul(factor).min(backoff.max);
    // Jitter only spreads concurrent retries apart; the clock's nanoseconds
    // are random enough for that and need no RNG dependency.
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |elapsed| elapsed.subsec_nanos());
    let jitter = f64::from(nanos % 1000) / 1000.0 * 0.25;
    base.mul_f64(1.0 - jitter)
}

fn retry_after(headers: &HeaderMap) -> Option<Duration> {
    let header = |name: &str| headers.get(name).and_then(|v| v.to_str().ok());
    if let Some(ms) = header("retry-after-ms").and_then(|v| v.trim().parse::<f64>().ok())
        && ms.is_finite()
        && ms >= 0.0
    {
        return Duration::try_from_secs_f64(ms / 1000.0).ok();
    }
    // Only the delta-seconds form; an HTTP date needs a date parser the
    // engine does not otherwise carry, and the backoff covers it.
    header("retry-after")
        .and_then(|v| v.trim().parse::<f64>().ok())
        .filter(|s| s.is_finite() && *s >= 0.0)
        .and_then(|s| Duration::try_from_secs_f64(s).ok())
}

fn retryable_status(status: StatusCode) -> bool {
    matches!(status.as_u16(), 408 | 409 | 429) || status.is_server_error()
}

/// Wait for `delay`, or fail with the stop line once a stop arrives.
async fn wait(delay: Duration, stop: &StopSignal) -> Result<(), EngineError> {
    tokio::select! {
        () = tokio::time::sleep(delay) => Ok(()),
        () = stop.stopped() => Err(EngineError::cancelled()),
    }
}

/// Read a body up to `limit` bytes. `idle` bounds the wait for each chunk.
pub(crate) async fn read_limited(
    response: &mut Response,
    limit: usize,
    idle: Duration,
) -> Result<Vec<u8>, BodyError> {
    let mut body = Vec::new();
    loop {
        let chunk = match tokio::time::timeout(idle, response.chunk()).await {
            Err(_) => return Err(BodyError::Timeout),
            Ok(Err(error)) if error.is_timeout() => return Err(BodyError::Timeout),
            Ok(Err(error)) => return Err(BodyError::Transport(error_chain(&error))),
            Ok(Ok(None)) => return Ok(body),
            Ok(Ok(Some(chunk))) => chunk,
        };
        if body.len() + chunk.len() > limit {
            return Err(BodyError::TooLarge);
        }
        body.extend_from_slice(&chunk);
    }
}

/// Why a body read stopped.
#[derive(Debug)]
pub(crate) enum BodyError {
    Timeout,
    TooLarge,
    Transport(String),
}

/// The message inside an OpenAI-style refusal: `{"error": {"message"}}`,
/// `{"error": "…"}`, `{"message": "…"}` or `{"detail": "…"}`.
fn upstream_message(body: &[u8]) -> Option<String> {
    let value: serde_json::Value = serde_json::from_slice(body).ok()?;
    let error = value.get("error");
    error
        .and_then(|e| e.get("message"))
        .or(error.filter(|e| e.is_string()))
        .or_else(|| value.get("message"))
        .or_else(|| value.get("detail"))
        .and_then(serde_json::Value::as_str)
        .map(str::to_owned)
}

impl Call<'_> {
    fn headers(&self) -> Result<HeaderMap, EngineError> {
        let mut headers = HeaderMap::new();
        let mut bearer =
            HeaderValue::from_str(&format!("Bearer {}", self.key.expose())).map_err(|_| {
                EngineError::new(
                    ErrorType::Value,
                    "llm_settings.api_key holds characters an HTTP header cannot carry",
                )
            })?;
        // Marks the value for hyper's HPACK (never indexed) and keeps it out
        // of reqwest's Debug output.
        bearer.set_sensitive(true);
        headers.insert(AUTHORIZATION, bearer);
        headers.insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
        headers.insert(
            ACCEPT,
            HeaderValue::from_static(if self.streaming {
                "text/event-stream"
            } else {
                "application/json"
            }),
        );
        if let Some(organization) = self.organization {
            let value = HeaderValue::from_str(organization).map_err(|_| {
                EngineError::new(
                    ErrorType::Value,
                    "llm_settings.organization holds characters an HTTP header cannot carry",
                )
            })?;
            headers.insert("openai-organization", value);
        }
        Ok(headers)
    }

    pub(crate) fn timeout_error(&self, after: Duration) -> EngineError {
        runtime(format!(
            "{} request for model '{}' hit a timeout after {} s",
            self.what,
            self.model,
            after.as_secs()
        ))
    }

    /// A malformed or over-limit reply.
    pub(crate) fn protocol_error(&self, detail: &str) -> EngineError {
        runtime(format!(
            "{} inference failed for model '{}': {}",
            self.what,
            self.model,
            sanitize(detail, self.key)
        ))
    }

    fn status_error(&self, status: StatusCode, body: &[u8], attempts: u32) -> EngineError {
        let upstream = upstream_message(body)
            .map(|message| format!(": {}", sanitize(&message, self.key)))
            .unwrap_or_default();
        let code = status.as_u16();
        let tried = if attempts > 1 {
            format!(" after {attempts} attempts")
        } else {
            String::new()
        };
        match code {
            429 | 503 => runtime(format!(
                "The model service is busy: HTTP {code} for model '{}'{tried}{upstream}",
                self.model
            )),
            504 | 408 => runtime(format!(
                "{} request for model '{}' hit a gateway timeout (HTTP {code}){tried}",
                self.what, self.model
            )),
            402 => EngineError::new(
                ErrorType::Value,
                format!(
                    "The model budget is exhausted: HTTP 402 for model '{}'{upstream}",
                    self.model
                ),
            ),
            404 => runtime(format!(
                "{} model '{}' not found at the gateway (HTTP 404){upstream}",
                self.what, self.model
            )),
            401 | 403 => runtime(format!(
                "{} inference refused: the gateway rejected the credential for model '{}' (HTTP {code}){upstream}",
                self.what, self.model
            )),
            _ => runtime(format!(
                "{} inference failed: HTTP {code} for model '{}'{tried}{upstream}",
                self.what, self.model
            )),
        }
    }

    fn transport_error(&self, error: &reqwest::Error, attempts: u32) -> EngineError {
        if error.is_timeout() {
            return runtime(format!(
                "{} request for model '{}' hit a timeout after {attempts} attempt(s)",
                self.what, self.model
            ));
        }
        let host = reqwest::Url::parse(self.url)
            .ok()
            .and_then(|url| url.host_str().map(str::to_owned))
            .unwrap_or_default();
        runtime(format!(
            "{} inference failed: cannot reach the model gateway at {host} after {attempts} attempt(s): {}",
            self.what,
            sanitize(&error_chain(error), self.key)
        ))
    }
}

/// A gateway's final refusal: its status and the start of its body.
///
/// The body is raw (not sanitised) and capped at `MAX_ERROR_BODY_BYTES`.
/// It is for a caller that must classify the refusal, and must never be
/// formatted into an error or a log line; the [`PostError::error`] beside
/// it is the sanitised form.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct Refusal {
    pub status: u16,
    pub body: String,
}

/// A failed [`Transport::post_classified`]: the engine error, and the
/// refusal when the gateway answered with a status that ended the call.
#[derive(Debug)]
pub(crate) struct PostError {
    pub error: EngineError,
    pub refusal: Option<Refusal>,
}

impl From<EngineError> for PostError {
    fn from(error: EngineError) -> Self {
        Self {
            error,
            refusal: None,
        }
    }
}

impl Transport {
    /// POST `body`, retrying as the SDK does, and return the first
    /// successful response (its body unread).
    ///
    /// For a streaming call the per-attempt timeout covers the response
    /// head only; the caller bounds the stream itself.
    pub(crate) async fn post(
        &self,
        call: &Call<'_>,
        body: Vec<u8>,
        stop: &StopSignal,
    ) -> Result<Response, EngineError> {
        self.post_classified(call, body, stop)
            .await
            .map_err(|failure| failure.error)
    }

    /// [`Transport::post`], but a final non-success status also gives the
    /// caller the [`Refusal`], so that it can recognise one refusal (the
    /// embedding client's context-length fallback) and react to it.
    pub(crate) async fn post_classified(
        &self,
        call: &Call<'_>,
        body: Vec<u8>,
        stop: &StopSignal,
    ) -> Result<Response, PostError> {
        if stop.is_requested() {
            return Err(EngineError::cancelled().into());
        }
        let headers = call.headers()?;
        let attempt_timeout = if call.streaming {
            self.timeouts.stream_idle
        } else {
            self.timeouts.request
        };
        let mut retry = 0_u32;
        loop {
            let mut request = self
                .client
                .post(call.url)
                .headers(headers.clone())
                .body(body.clone());
            if !call.streaming {
                // Covers the body read as well: reqwest keeps the deadline
                // on the response until it is consumed.
                request = request.timeout(self.timeouts.request);
            }
            let sent = tokio::select! {
                sent = tokio::time::timeout(attempt_timeout, request.send()) => sent,
                () = stop.stopped() => return Err(EngineError::cancelled().into()),
            };
            let attempts = retry + 1;
            let can_retry = retry < call.max_retries;
            match sent {
                Err(_) => {
                    if !can_retry {
                        return Err(call.timeout_error(attempt_timeout).into());
                    }
                    tracing::warn!(
                        what = call.what,
                        model = call.model,
                        attempt = attempts,
                        "model request timed out; retrying"
                    );
                    wait(retry_delay(self.backoff, retry, &HeaderMap::new()), stop).await?;
                }
                Ok(Err(error)) => {
                    let transient = error.is_timeout() || error.is_connect() || error.is_request();
                    if !transient || !can_retry {
                        return Err(call.transport_error(&error, attempts).into());
                    }
                    tracing::warn!(what = call.what, model = call.model, attempt = attempts, error = %sanitize(&error_chain(&error), call.key), "model request failed; retrying");
                    wait(retry_delay(self.backoff, retry, &HeaderMap::new()), stop).await?;
                }
                Ok(Ok(response)) if response.status().is_success() => return Ok(response),
                Ok(Ok(mut response)) => {
                    let status = response.status();
                    if retryable_status(status) && can_retry {
                        let delay = retry_delay(self.backoff, retry, response.headers());
                        tracing::warn!(
                            what = call.what,
                            model = call.model,
                            attempt = attempts,
                            status = status.as_u16(),
                            "model gateway refused; retrying"
                        );
                        drop(response);
                        wait(delay, stop).await?;
                    } else {
                        let body = read_limited(
                            &mut response,
                            MAX_ERROR_BODY_BYTES,
                            self.timeouts.connect,
                        )
                        .await
                        .unwrap_or_default();
                        return Err(PostError {
                            error: call.status_error(status, &body, attempts),
                            refusal: Some(Refusal {
                                status: status.as_u16(),
                                body: String::from_utf8_lossy(&body).into_owned(),
                            }),
                        });
                    }
                }
            }
            retry += 1;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn key() -> Secret {
        Secret::new("sk-test-123".to_owned()).unwrap_or_else(|| panic!("empty"))
    }

    fn call(key: &Secret) -> Call<'_> {
        Call {
            what: "Chat completion",
            url: "https://gw.example/llm/v1/chat/completions",
            model: "gpt-4o",
            key,
            organization: None,
            max_retries: 2,
            streaming: false,
        }
    }

    #[test]
    fn each_status_lands_in_its_category() {
        let key = key();
        let call = call(&key);
        let category = |code: u16| {
            call.status_error(StatusCode::from_u16(code).unwrap_or_default(), b"{}", 3)
                .category()
        };
        assert_eq!(category(429), "service_busy");
        assert_eq!(category(503), "service_busy");
        assert_eq!(category(504), "timeout_error");
        assert_eq!(category(402), "invalid_input");
        assert_eq!(category(404), "resource_not_found");
        assert_eq!(category(401), "inference_failed");
        assert_eq!(category(400), "inference_failed");
        assert_eq!(category(500), "inference_failed");
        assert_eq!(
            call.timeout_error(Duration::from_secs(5)).category(),
            "timeout_error"
        );
        assert_eq!(call.protocol_error("bad").category(), "inference_failed");
    }

    #[test]
    fn an_echoed_key_is_redacted() {
        let key = key();
        let call = call(&key);
        let error = call.status_error(
            StatusCode::BAD_REQUEST,
            br#"{"error": {"message": "bad header Authorization: Bearer sk-test-123"}}"#,
            1,
        );
        assert!(!error.message.contains("sk-test-123"), "{}", error.message);
        assert!(error.message.contains("<redacted>"), "{}", error.message);
    }

    #[test]
    fn a_long_upstream_message_is_cut() {
        let key = key();
        let long = "x".repeat(5000);
        assert!(sanitize(&long, &key).chars().count() <= MAX_UPSTREAM_MESSAGE_CHARS + 1);
    }

    #[test]
    fn the_server_delay_wins_within_bounds() {
        let backoff = Backoff::default();
        let mut headers = HeaderMap::new();
        headers.insert("retry-after", HeaderValue::from_static("2"));
        assert_eq!(retry_delay(backoff, 0, &headers), Duration::from_secs(2));
        headers.insert("retry-after-ms", HeaderValue::from_static("150"));
        assert_eq!(
            retry_delay(backoff, 0, &headers),
            Duration::from_millis(150)
        );
        let mut far = HeaderMap::new();
        far.insert("retry-after", HeaderValue::from_static("3600"));
        assert!(retry_delay(backoff, 5, &far) <= backoff.max);
    }

    #[test]
    fn the_computed_delay_doubles_and_is_capped() {
        let backoff = Backoff::default();
        let none = HeaderMap::new();
        let first = retry_delay(backoff, 0, &none);
        assert!(first <= Duration::from_millis(500) && first >= Duration::from_millis(375));
        let late = retry_delay(backoff, 10, &none);
        assert!(late <= Duration::from_secs(8) && late >= Duration::from_secs(6));
    }

    #[test]
    fn upstream_messages_take_the_common_shapes() {
        assert_eq!(
            upstream_message(br#"{"error":{"message":"a"}}"#).as_deref(),
            Some("a")
        );
        assert_eq!(upstream_message(br#"{"error":"b"}"#).as_deref(), Some("b"));
        assert_eq!(upstream_message(br#"{"detail":"c"}"#).as_deref(), Some("c"));
        assert_eq!(upstream_message(b"<html>"), None);
    }
}
