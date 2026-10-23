//! The transport of the model client: the model calls over
//! `elitea.llm.v1.ModelService` (gRPC over mutual TLS to elitea-main's
//! platform listener, #1359), the retry loop, bounded body reads, and the
//! mapping of every failure onto the engine's error contract.
//!
//! # The model service
//!
//! A call goes to the ORIGIN of `llm_settings.api_base` (the platform
//! listener, `https://elitea-main:9446`); the path only names the route
//! (`…/chat/completions`, `…/embeddings`). The answer comes back as the HTTP
//! edge answered it — status, `content-type`, `retry-after` and the body
//! bytes, SSE framing included (`elitea_model_rpc`) — so everything below is
//! unchanged from the HTTP client it replaced. An `https` origin needs the
//! platform CA ([`TransportSettings::ca_file`]) and presents the callback
//! client certificate ([`TransportSettings::client_identity`]); plaintext
//! `http` is accepted for a loopback host only (tests).
//!
//! The same channel carries the engines' other platform calls
//! ([`Transport::platform_channel`]: the artifact objects and the toolkit
//! invocations, `elitea-platform-rpc`). The `reqwest` client is only the
//! configured HTTP client a caller may still need; no platform call uses it.
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
//! | HTTP 402 (budget: member, project or unscoped) | `ValueError` | `invalid_input` |
//! | HTTP 404 (unknown model) | `RuntimeError` | `resource_not_found` |
//! | any other refusal, a malformed reply | `RuntimeError` | `inference_failed` |
//!
//! Every refusal also has the worker's code ([`refusal_code`]:
//! `model_gateway.member_budget_exhausted`, `model_gateway.rate_limited`, …),
//! recorded on the request's span, so the two callers of `/llm` report a
//! refusal the same way (the `/llm` caller contract, `docs/llm-caller-contract.md`).
//!
//! # Tracing
//!
//! Each request is one `engine.model.request` span (the worker's is
//! `agent.model.request`) with the worker's attribute names where they
//! apply: `model_adapter`, `model_name`, `billing_project_id`,
//! `streaming`, `outcome`, `error_code`, `retryable`, plus `engine`,
//! `operation`, `execution_id`, `attempts` and `http_status`.
//!
//! No message carries the API key. Text the gateway sent back is cut to a
//! few hundred characters and has the key replaced before it is used,
//! because a misconfigured upstream can echo request headers.

use bytes::Bytes;
use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::secret::Secret;
use elitea_engine_core::stream::StopSignal;
use elitea_llm_wire::refusal::{Detail, MessageShapes, redact_and_cut, rejection_detail};
use elitea_model_rpc::{BodyStream, CallError, ModelRequest, ModelServiceClient, Route};
use futures_util::StreamExt as _;
use reqwest::header::HeaderMap;
use reqwest::{Client, StatusCode};
use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tonic::transport::{Channel, ClientTlsConfig, Endpoint};
use tracing::Instrument as _;

/// How much of a refusal body is read to find its message.
const MAX_ERROR_BODY_BYTES: usize = 16 * 1024;

/// The project selector the worker sends (ADR-0018's primary choice), and
/// the run the gateway attributes the call's spend to (the `/llm` edge keeps
/// it only for a live execution of the same project and user).
pub use elitea_llm_wire::headers::{EXECUTION_HEADER, PROJECT_HEADER};

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
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TransportSettings {
    /// A PEM bundle trusted IN ADDITION to the platform roots: the gateway
    /// of an install with a private CA, while public endpoints keep working.
    pub ca_file: Option<PathBuf>,
    /// What the operator calls [`TransportSettings::ca_file`] — the
    /// consuming engine's environment variable — so a CA-file error names
    /// the setting to fix.
    pub ca_file_setting: &'static str,
    /// A client certificate the transport presents when the server asks for
    /// one: the callback hop to elitea-main's internal platform listener,
    /// which requires a runtime-CA client certificate (mutual TLS). `None`
    /// presents nothing.
    pub client_identity: Option<ClientIdentityFiles>,
    /// The origin of elitea-main's platform listener
    /// (`https://elitea-main:9446`): `elitea.llm.v1.ModelService` and the
    /// services of [`Transport::platform_channel`]. Set, every model call
    /// goes there and `llm_settings.api_base` only names the route; unset, a
    /// model call goes to the origin of `api_base` and there is no platform
    /// channel. The engines require it.
    pub model_service: Option<String>,
    /// The `User-Agent` of every request: the consuming engine's name.
    pub user_agent: &'static str,
    pub timeouts: Timeouts,
    pub backoff: Backoff,
}

/// A client certificate chain and its private key, as PEM files, with the
/// settings that name them so a read error says which one to fix.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ClientIdentityFiles {
    pub cert_file: PathBuf,
    pub cert_file_setting: &'static str,
    pub key_file: PathBuf,
    pub key_file_setting: &'static str,
}

impl Default for TransportSettings {
    fn default() -> Self {
        Self {
            ca_file: None,
            ca_file_setting: "The TLS CA file",
            client_identity: None,
            model_service: None,
            user_agent: concat!("elitea-model-client/", env!("CARGO_PKG_VERSION")),
            timeouts: Timeouts::default(),
            backoff: Backoff::default(),
        }
    }
}

/// A configured model transport. Cheap to clone; share one per process.
#[derive(Debug, Clone)]
pub struct Transport {
    client: Client,
    /// The model service hop, shared by the clones (and kept off the stack:
    /// the TLS config is large, and callers hold a `Transport` by value).
    hop: Arc<ModelHop>,
    timeouts: Timeouts,
    backoff: Backoff,
    /// The consuming engine, for the request span (its `User-Agent`).
    engine: &'static str,
}

/// What the model service hop needs beside the timeouts.
#[derive(Debug)]
struct ModelHop {
    /// The trust and the client certificate of the hop.
    tls: Option<ClientTlsConfig>,
    /// The configured model service origin ([`TransportSettings::model_service`]).
    origin: Option<String>,
    /// One channel per model service origin, opened on first use.
    channels: Mutex<HashMap<String, Channel>>,
}

fn runtime(message: impl Into<String>) -> EngineError {
    EngineError::new(ErrorType::Runtime, message)
}

/// Read one PEM file the operator named in `setting`.
fn read_pem(path: &Path, setting: &str) -> Result<Vec<u8>, EngineError> {
    // Worded without "not found": a missing CA or certificate file is a
    // deployment error, not a missing wiki.
    std::fs::read(path).map_err(|error| {
        // io's own text for a missing file is "entity not found", which
        // the classifier would file as a missing resource.
        let reason = match error.kind() {
            std::io::ErrorKind::NotFound => "no such file".to_owned(),
            other => other.to_string(),
        };
        runtime(format!(
            "{setting} {} cannot be read: {reason}",
            path.display(),
        ))
    })
}

/// The client certificate chain and key as one `reqwest::Identity` (rustls
/// takes both from a single PEM buffer).
fn read_identity(files: &ClientIdentityFiles) -> Result<reqwest::Identity, EngineError> {
    let mut pem = read_pem(&files.cert_file, files.cert_file_setting)?;
    let mut key = read_pem(&files.key_file, files.key_file_setting)?;
    pem.push(b'\n');
    pem.extend_from_slice(&key);
    key.fill(0);
    let identity = reqwest::Identity::from_pem(&pem).map_err(|_| {
        runtime(format!(
            "{} {} and {} {} are not a PEM certificate chain and its private key",
            files.cert_file_setting,
            files.cert_file.display(),
            files.key_file_setting,
            files.key_file.display(),
        ))
    });
    // The key copy does not outlive the client build.
    pem.fill(0);
    identity
}

fn read_ca_file(path: &Path, setting: &str) -> Result<Vec<reqwest::Certificate>, EngineError> {
    let pem = read_pem(path, setting)?;
    let certificates = reqwest::Certificate::from_pem_bundle(&pem).map_err(|_| {
        runtime(format!(
            "{setting} {} is not a PEM certificate bundle",
            path.display()
        ))
    })?;
    if certificates.is_empty() {
        return Err(runtime(format!(
            "{setting} {} holds no certificate",
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
            .user_agent(settings.user_agent);
        let mut tls = None;
        if let Some(path) = &settings.ca_file {
            builder = builder.tls_certs_merge(read_ca_file(path, settings.ca_file_setting)?);
            tls = Some(ClientTlsConfig::new().ca_certificate(
                tonic::transport::Certificate::from_pem(read_pem(path, settings.ca_file_setting)?),
            ));
        }
        if let Some(files) = &settings.client_identity {
            builder = builder.identity(read_identity(files)?);
            let cert = read_pem(&files.cert_file, files.cert_file_setting)?;
            let key = read_pem(&files.key_file, files.key_file_setting)?;
            tls = Some(
                tls.unwrap_or_default()
                    .identity(tonic::transport::Identity::from_pem(cert, key)),
            );
        }
        let client = builder.build().map_err(|error| {
            runtime(format!(
                "The model client cannot start its TLS stack: {}",
                error_chain(&error)
            ))
        })?;
        Ok(Self {
            client,
            hop: Arc::new(ModelHop {
                tls,
                origin: settings
                    .model_service
                    .as_deref()
                    .map(str::trim)
                    .filter(|origin| !origin.is_empty())
                    .map(str::to_owned),
                channels: Mutex::default(),
            }),
            timeouts: settings.timeouts,
            backoff: settings.backoff,
            engine: settings.user_agent,
        })
    }

    #[must_use]
    pub fn timeouts(&self) -> Timeouts {
        self.timeouts
    }

    /// The configured HTTP client: the same CA bundle and client
    /// certificate as the model calls, and no redirects. No platform call
    /// uses it.
    #[must_use]
    pub fn http_client(&self) -> &Client {
        &self.client
    }

    /// The channel to elitea-main's platform listener at the configured
    /// origin ([`TransportSettings::model_service`]), the one the model
    /// calls use: the engines' artifact-object and toolkit-invocation calls
    /// ride it (`elitea-platform-rpc`).
    ///
    /// # Errors
    ///
    /// A `RuntimeError` when no origin is configured or it cannot be
    /// reached as the model calls reach it.
    pub fn platform_channel(&self) -> Result<Channel, EngineError> {
        if self.hop.origin.is_none() {
            return Err(runtime(
                "The platform listener is not configured: set the engine's MODEL_SERVICE_URL",
            ));
        }
        self.channel("")
    }

    /// The model service client: the configured origin, else the origin of
    /// `url`.
    fn model_service(&self, url: &str) -> Result<ModelServiceClient<Channel>, EngineError> {
        Ok(ModelServiceClient::new(self.channel(url)?)
            .max_decoding_message_size(MAX_MODEL_ANSWER_BYTES)
            .max_encoding_message_size(MAX_MODEL_MESSAGE_BYTES))
    }

    /// The platform listener's channel: the configured origin, else the
    /// origin of `url`. One per origin, opened on first use.
    fn channel(&self, url: &str) -> Result<Channel, EngineError> {
        let parsed = match &self.hop.origin {
            Some(origin) => reqwest::Url::parse(origin)
                .map_err(|_| runtime(format!("The model service origin {origin} is not a URL")))?,
            None => reqwest::Url::parse(url).map_err(|_| {
                runtime("llm_settings.api_base is not a URL the model service can reach")
            })?,
        };
        let host = parsed.host_str().unwrap_or_default().to_owned();
        let port = parsed.port_or_known_default().unwrap_or(443);
        let origin = format!("{}://{}:{port}", parsed.scheme(), bracket(&host));
        let plaintext = match parsed.scheme() {
            "https" => false,
            "http" if is_loopback(&host) => true,
            _ => {
                return Err(runtime(format!(
                    "The model service at {host} must be reached over https: it is gRPC over mutual TLS to the platform listener (llm_settings.api_base)"
                )));
            }
        };
        let mut channels = self
            .hop
            .channels
            .lock()
            .map_err(|_| runtime("The model client's channel table is poisoned"))?;
        if let Some(channel) = channels.get(&origin) {
            return Ok(channel.clone());
        }
        let mut endpoint = Endpoint::from_shared(origin.clone())
            .map_err(|_| runtime(format!("The model service origin {origin} is not valid")))?
            .connect_timeout(self.timeouts.connect)
            .http2_keep_alive_interval(Duration::from_mins(1))
            .keep_alive_timeout(Duration::from_secs(10))
            .keep_alive_while_idle(true)
            .user_agent(self.engine)
            .map_err(|_| runtime("The model client's user agent is not a header value"))?;
        if !plaintext {
            let tls = self.hop.tls.clone().ok_or_else(|| {
                runtime(format!(
                    "The model service at {host} needs the platform CA: set the engine's callback CA file"
                ))
            })?;
            endpoint = endpoint.tls_config(tls).map_err(|error| {
                runtime(format!(
                    "The model client cannot start its TLS stack: {}",
                    error_chain(&error)
                ))
            })?;
        }
        let channel = endpoint.connect_lazy();
        channels.insert(origin, channel.clone());
        Ok(channel)
    }
}

/// One gRPC message the platform listener receives (its own cap).
const MAX_MODEL_MESSAGE_BYTES: usize = 16 * 1024 * 1024;

/// One gRPC message the platform listener sends: a unary answer
/// (embeddings, images) up to the gateway's 64 MiB unary cap plus its head
/// (elitea-main runtimegrpc.PlatformMaxSendMessageBytes).
const MAX_MODEL_ANSWER_BYTES: usize = 65 * 1024 * 1024;

fn bracket(host: &str) -> String {
    if host.contains(':') && !host.starts_with('[') {
        format!("[{host}]")
    } else {
        host.to_owned()
    }
}

fn is_loopback(host: &str) -> bool {
    let bare = host.trim_start_matches('[').trim_end_matches(']');
    bare.eq_ignore_ascii_case("localhost")
        || bare
            .parse::<std::net::IpAddr>()
            .is_ok_and(|address| address.is_loopback())
}

/// A model answer: the status and headers the HTTP edge answered, and the
/// body as it arrives.
pub(crate) struct Reply {
    status: StatusCode,
    headers: HeaderMap,
    body: BodyStream,
}

impl Reply {
    /// The next body chunk; `None` at the end.
    pub(crate) async fn chunk(&mut self) -> Result<Option<Bytes>, StreamError> {
        match self.body.next().await {
            None => Ok(None),
            Some(Ok(bytes)) => Ok(Some(bytes)),
            Some(Err(status)) => Err(StreamError(status)),
        }
    }
}

/// A body stream that broke.
#[derive(Debug)]
pub(crate) struct StreamError(tonic::Status);

impl StreamError {
    pub(crate) fn is_timeout(&self) -> bool {
        self.0.code() == tonic::Code::DeadlineExceeded
    }
}

impl std::fmt::Display for StreamError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{:?}: {}", self.0.code(), self.0.message())
    }
}

impl std::error::Error for StreamError {}

/// What one request is, for its error messages and its retries.
#[derive(Debug, Clone, Copy)]
pub(crate) struct Call<'a> {
    /// "Embedding" or "Chat completion".
    pub what: &'static str,
    pub url: &'a str,
    pub model: &'a str,
    pub key: &'a Secret,
    pub organization: Option<&'a str>,
    pub execution_id: Option<&'a str>,
    pub max_retries: u32,
    pub streaming: bool,
}

/// `source()` chain of a transport error, for a message. reqwest's own
/// text names the URL, which holds no credential (see `settings`).
pub use elitea_engine_core::errors::error_chain;

/// Replace the key wherever it appears, then cut to length.
pub(crate) fn sanitize(text: &str, key: &Secret) -> String {
    redact_and_cut(text, key.expose(), MAX_UPSTREAM_MESSAGE_CHARS)
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

/// Whether a refusal with this status is worth another attempt: 408, 409,
/// 429 and 5xx, as the worker's categories decide.
#[must_use]
pub fn retryable(status: u16) -> bool {
    elitea_llm_wire::refusal::retryable_status(status)
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
    response: &mut Reply,
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

/// Which ceiling a 402 names, read as the worker reads it
/// (`budget_refusal_scope`): only a `budget_exceeded` error is a budget
/// refusal of this platform; its `scope` decides, and an older gateway that
/// sends no scope is known by `code: member_budget_exceeded`. A provider's
/// own quota refusal (no scope) is [`BudgetScope::Unscoped`]. The worker's
/// code for a gateway refusal ([`refusal_code`]: `validate_response_head`,
/// `budget_refusal_error`), so both callers name a refusal alike.
pub use elitea_llm_wire::refusal::{BudgetScope, budget_scope, refusal_code};

/// The message inside an OpenAI-style refusal (`{"error": {"message"}}`,
/// `{"error": "…"}`, `{"message": "…"}` or `{"detail": "…"}`), with the key
/// replaced and cut to length.
fn upstream_message(body: &[u8], key: &Secret) -> Option<String> {
    rejection_detail(
        body,
        MessageShapes::Common,
        Detail::Redacted {
            secret: key.expose(),
            max_chars: MAX_UPSTREAM_MESSAGE_CHARS,
        },
    )
}

impl Call<'_> {
    /// The model service request: the route the URL names, the bearer, the
    /// project selector and the execution, as the `/llm` caller contract
    /// sent them in headers.
    fn model_request(&self, body: Vec<u8>) -> Result<(Route, ModelRequest), EngineError> {
        let path = reqwest::Url::parse(self.url)
            .map(|url| url.path().to_owned())
            .unwrap_or_default();
        let route = Route::from_path(&path).ok_or_else(|| {
            runtime(format!(
                "{} has no model service route for {path}",
                self.what
            ))
        })?;
        let text = |value: Option<&str>, setting: &str| -> Result<Option<String>, EngineError> {
            match value {
                Some(value) if value.bytes().any(|byte| byte.is_ascii_control()) => {
                    Err(EngineError::new(
                        ErrorType::Value,
                        format!("{setting} holds characters a header cannot carry"),
                    ))
                }
                other => Ok(other.map(str::to_owned)),
            }
        };
        if self
            .key
            .expose()
            .bytes()
            .any(|byte| byte.is_ascii_control() || byte == b' ')
        {
            return Err(EngineError::new(
                ErrorType::Value,
                "llm_settings.api_key holds characters an HTTP header cannot carry",
            ));
        }
        Ok((
            route,
            ModelRequest {
                bearer: self.key.expose().to_owned(),
                project_id: text(self.organization, "llm_settings.organization")?,
                execution_id: text(self.execution_id, "llm_settings.execution_id")?,
                content_type: Some("application/json".to_owned()),
                body: Bytes::from(body),
                ..ModelRequest::default()
            },
        ))
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
        let upstream = upstream_message(body, self.key)
            .map(|message| format!(": {message}"))
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
            402 => {
                let which = match budget_scope(body) {
                    BudgetScope::Member => "member model budget",
                    BudgetScope::Project => "project model budget",
                    BudgetScope::Unscoped => "model budget",
                };
                EngineError::new(
                    ErrorType::Value,
                    format!(
                        "The {which} is exhausted: HTTP 402 for model '{}'{upstream}",
                        self.model
                    ),
                )
            }
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

    fn transport_error(&self, status: &tonic::Status, attempts: u32) -> EngineError {
        if status.code() == tonic::Code::DeadlineExceeded {
            return runtime(format!(
                "{} request for model '{}' hit a timeout after {attempts} attempt(s)",
                self.what, self.model
            ));
        }
        let host = reqwest::Url::parse(self.url)
            .ok()
            .and_then(|url| url.host_str().map(str::to_owned))
            .unwrap_or_default();
        let detail = sanitize(
            &format!("{:?}: {}", status.code(), status.message()),
            self.key,
        );
        if matches!(
            status.code(),
            tonic::Code::PermissionDenied | tonic::Code::Unauthenticated
        ) {
            // No refusal trailer: the platform listener did not admit this
            // workload's client certificate (ELITEA_MODEL_SERVICE_CLIENTS).
            return runtime(format!(
                "{} inference refused: the model service at {host} did not admit this engine's client certificate: {detail}",
                self.what
            ));
        }
        runtime(format!(
            "{} inference failed: cannot reach the model gateway at {host} after {attempts} attempt(s): {detail}",
            self.what
        ))
    }
}

/// The worker's code for a failed request, and whether the worker would
/// call it retryable: a refusal by its status ([`refusal_code`]); a stop,
/// a timeout or a transport failure by the error.
fn failure_code(failure: &PostError) -> (&'static str, bool) {
    if let Some(refusal) = &failure.refusal {
        return (
            refusal_code(refusal.status, refusal.body.as_bytes()),
            retryable(refusal.status),
        );
    }
    if failure.error.message == EngineError::cancelled().message {
        ("cancelled", false)
    } else if failure.error.message.contains("timeout") {
        ("model_gateway.response_header_timeout", true)
    } else {
        ("model_gateway.transport", true)
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
    ) -> Result<Reply, EngineError> {
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
    ) -> Result<Reply, PostError> {
        let span = tracing::info_span!(
            "engine.model.request",
            engine = self.engine,
            operation = call.what,
            model_adapter = "openai_compatible",
            model_name = call.model,
            billing_project_id = call.organization.unwrap_or_default(),
            execution_id = call.execution_id.unwrap_or_default(),
            streaming = call.streaming,
            attempts = tracing::field::Empty,
            http_status = tracing::field::Empty,
            outcome = tracing::field::Empty,
            error_code = tracing::field::Empty,
            retryable = tracing::field::Empty,
        );
        let result = self
            .post_attempts(call, body, stop)
            .instrument(span.clone())
            .await;
        match &result {
            Ok(response) => {
                span.record("http_status", response.status.as_u16());
                span.record(
                    "outcome",
                    if call.streaming {
                        "stream_opened"
                    } else {
                        "responded"
                    },
                );
            }
            Err(failure) => {
                let (code, retryable) = failure_code(failure);
                if let Some(refusal) = &failure.refusal {
                    span.record("http_status", refusal.status);
                }
                span.record("outcome", "failed");
                span.record("error_code", code);
                span.record("retryable", retryable);
            }
        }
        result
    }

    /// The attempts of one request: the retry loop under the span.
    async fn post_attempts(
        &self,
        call: &Call<'_>,
        body: Vec<u8>,
        stop: &StopSignal,
    ) -> Result<Reply, PostError> {
        if stop.is_requested() {
            return Err(EngineError::cancelled().into());
        }
        let (route, request) = call.model_request(body)?;
        let mut service = self.model_service(call.url)?;
        let attempt_timeout = if call.streaming {
            self.timeouts.stream_idle
        } else {
            self.timeouts.request
        };
        let mut retry = 0_u32;
        loop {
            let sent = tokio::select! {
                sent = tokio::time::timeout(attempt_timeout, elitea_model_rpc::call(&mut service, route, request.clone())) => sent,
                () = stop.stopped() => return Err(EngineError::cancelled().into()),
            };
            let attempts = retry + 1;
            tracing::Span::current().record("attempts", attempts);
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
                Ok(Err(CallError::Malformed(reason))) => {
                    return Err(EngineError::new(
                        ErrorType::Value,
                        format!("{} request is malformed: {reason}", call.what),
                    )
                    .into());
                }
                Ok(Err(CallError::Hop(status))) => {
                    let transient = matches!(
                        status.code(),
                        tonic::Code::Unavailable
                            | tonic::Code::DeadlineExceeded
                            | tonic::Code::Unknown
                    );
                    if !transient || !can_retry {
                        return Err(call.transport_error(&status, attempts).into());
                    }
                    tracing::warn!(what = call.what, model = call.model, attempt = attempts, error = %sanitize(status.message(), call.key), "model request failed; retrying");
                    wait(retry_delay(self.backoff, retry, &HeaderMap::new()), stop).await?;
                }
                Ok(Ok(response)) if response.status.is_success() => {
                    return Ok(Reply {
                        status: response.status,
                        headers: response.headers,
                        body: response.body,
                    });
                }
                Ok(Ok(response)) => {
                    let status = response.status;
                    let mut reply = Reply {
                        status,
                        headers: response.headers,
                        body: response.body,
                    };
                    if retryable(status.as_u16()) && can_retry {
                        let delay = retry_delay(self.backoff, retry, &reply.headers);
                        tracing::warn!(
                            what = call.what,
                            model = call.model,
                            attempt = attempts,
                            status = status.as_u16(),
                            "model gateway refused; retrying"
                        );
                        drop(reply);
                        wait(delay, stop).await?;
                    } else {
                        let body =
                            read_limited(&mut reply, MAX_ERROR_BODY_BYTES, self.timeouts.connect)
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
    use reqwest::header::HeaderValue;

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
            execution_id: None,
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
        let key = key();
        assert_eq!(
            upstream_message(br#"{"error":{"message":"a"}}"#, &key).as_deref(),
            Some("a")
        );
        assert_eq!(
            upstream_message(br#"{"error":"b"}"#, &key).as_deref(),
            Some("b")
        );
        assert_eq!(
            upstream_message(br#"{"detail":"c"}"#, &key).as_deref(),
            Some("c")
        );
        assert_eq!(upstream_message(b"<html>", &key), None);
    }
}
