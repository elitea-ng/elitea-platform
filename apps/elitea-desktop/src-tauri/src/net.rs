//! The host's one HTTP client, and the webview's fetch through it.
//!
//! **Why one client.** `reqwest::ClientBuilder::build` loads the TLS roots,
//! and the native ones (reqwest's `rustls-tls-native-roots`, turned on for
//! the whole process by the agent runtime) come from the macOS trust
//! settings through Security.framework: 85–470 ms per build, and serialised
//! across the process, so eight builds at once take 0.6–1.2 s. A client per
//! request (what `tauri-plugin-http` did for every webview fetch) also opens
//! a new TCP + TLS connection per request, with no keep-alive and no HTTP/2
//! multiplexing. When getaddrinfo puts this deployment's black-holed LAN A record
//! first, every request also pays the 300 ms happy-eyeballs fallback.
//! [`SharedHttp`] builds once, on a blocking thread
//! ([`SharedHttp::prewarm`] starts it at launch; async callers await it and
//! never hold a runtime worker), and every caller — the token endpoint,
//! discovery, the D0 platform and model calls, and the webview — shares its
//! connection pool. Per-call deadlines are request timeouts, never a
//! client-wide one, because the webview's SSE streams must stay open.
//!
//! **The webview's fetch** ([`http_fetch`], [`http_read_body`],
//! [`http_cancel`]) keeps the rules the HTTP plugin had here: only the
//! connected deployment's origin ([`HttpScope`], granted at startup for the
//! stored deployment and on connect, never widened by the page), redirects
//! never followed (a followed redirect would carry the bearer elsewhere), the
//! fetch spec's forbidden request headers dropped, the body streamed one chunk
//! per read (SSE), and an abort that ends a pending request or a stream.
//! There is no cookie jar: the desktop authenticates with a bearer only.
//!
//! **Bounds.** A request body is at most [`MAX_REQUEST_BODY`]. A response
//! the page never reads to its end is not kept forever: one nobody has read
//! for [`IDLE_TIMEOUT`] is dropped (a stream with a read waiting on it is
//! never idle, so SSE is unaffected), and past [`MAX_OPEN`] open entries the
//! longest-idle one is dropped. An abort that reaches the host before the
//! request it names is remembered for [`EARLY_CANCEL_TTL`], so that request
//! is refused instead of sent.

use std::collections::{HashMap, VecDeque};
use std::sync::{Arc, Mutex, RwLock};
use std::time::{Duration, Instant};

use bytes::Bytes;
use serde::{Deserialize, Serialize};
use tauri::State;
use tokio::sync::{OnceCell, watch};
use url::Url;

/// A connection that does not open in this long is not going to.
const CONNECT_TIMEOUT: Duration = Duration::from_secs(15);

/// The `Origin` the plugin sent for the bundled page (`tauri://localhost`);
/// the deployment has always seen it, so the host keeps sending it.
const PAGE_ORIGIN: &str = "tauri://localhost";

/// The largest request body the webview may send. The web app's largest
/// single-request upload is an artifact (`POST …/artifacts/…`, one multipart
/// body), whose deployment default is 150 MiB (`defaultMaxObjectBytes`,
/// `useArtifactUpload`); this covers it with room for the multipart
/// envelope. Mirrored by `MAX_REQUEST_BODY_BYTES` in `hostFetch.ts`.
pub const MAX_REQUEST_BODY: usize = 160 * 1024 * 1024;

/// The JSON metadata half of a frame (method, URL, headers).
const MAX_FRAME_META: usize = 1024 * 1024;

/// The IPC's postMessage fallback delivers a binary frame as a JSON array,
/// one number per byte: tens of bytes of memory per byte sent, before this
/// code sees it. Only small frames are taken that way.
pub const MAX_JSON_FRAME: usize = 1024 * 1024;

/// A response nobody has read for this long is dropped.
pub const IDLE_TIMEOUT: Duration = Duration::from_secs(60);

/// At most this many requests and unread responses are kept at once.
pub const MAX_OPEN: usize = 256;

/// How long an abort that arrived before its request is remembered.
pub const EARLY_CANCEL_TTL: Duration = Duration::from_secs(30);

/// At most this many early aborts are remembered.
const MAX_EARLY_CANCELS: usize = 1024;

/// The one lazily built client. Cloning shares it.
#[derive(Clone)]
pub struct SharedHttp {
    client: Arc<OnceCell<reqwest::Client>>,
    user_agent: Arc<str>,
}

impl SharedHttp {
    #[must_use]
    pub fn new(client_version: &str) -> Self {
        Self {
            client: Arc::new(OnceCell::new()),
            user_agent: format!("elitea-desktop/{client_version}").into(),
        }
    }

    /// Start building the client now, so the first request (or the setup
    /// that needs it) does not pay the root-store load.
    pub fn prewarm(&self) {
        let this = self.clone();
        tauri::async_runtime::spawn(async move {
            let started = Instant::now();
            let _ = this.client().await;
            log::debug!("http client ready in {:?}", started.elapsed());
        });
    }

    /// The client. The first caller builds it on a blocking thread (the
    /// root-store load blocks); every caller meanwhile awaits that build,
    /// so no runtime worker is held.
    pub async fn client(&self) -> &reqwest::Client {
        self.client
            .get_or_init(|| async {
                let user_agent = self.user_agent.clone();
                match tokio::task::spawn_blocking(move || build_client(&user_agent)).await {
                    Ok(client) => client,
                    Err(error) => {
                        log::warn!(
                            "the http client build failed ({error}); using the built-in roots"
                        );
                        built_in_roots(&self.user_agent)
                    }
                }
            })
            .await
    }
}

fn builder(user_agent: &str) -> reqwest::ClientBuilder {
    reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .connect_timeout(CONNECT_TIMEOUT)
        .user_agent(user_agent)
}

/// The bundled web PKI roots only: no Security.framework call.
fn built_in_roots(user_agent: &str) -> reqwest::Client {
    builder(user_agent)
        .tls_built_in_native_certs(false)
        .build()
        .unwrap_or_default()
}

fn build_client(user_agent: &str) -> reqwest::Client {
    match builder(user_agent).build() {
        Ok(client) => client,
        Err(error) => {
            // The native root store can be unreadable (no valid certificate
            // in it): the bundled web PKI roots still reach a public deployment.
            log::warn!(
                "http client with the system roots failed ({error}); using the built-in roots"
            );
            built_in_roots(user_agent)
        }
    }
}

// ---- scope -------------------------------------------------------------

/// The origins the webview may fetch: the connected deployment's, added at
/// startup and on every connect. Like the runtime capability it replaces,
/// nothing is ever removed before a restart.
#[derive(Default)]
pub struct HttpScope {
    origins: RwLock<Vec<String>>,
}

/// `scheme://host[:port]` of an http(s) URL with a host; `None` otherwise.
#[must_use]
pub fn scope_origin(origin: &str) -> Option<String> {
    let url = Url::parse(origin).ok()?;
    if !matches!(url.scheme(), "http" | "https") || url.host_str().is_none() {
        return None;
    }
    Some(url.origin().ascii_serialization())
}

impl HttpScope {
    /// Allow `origin` (any path, any query).
    ///
    /// # Errors
    ///
    /// Not an http(s) origin.
    pub fn grant(&self, origin: &str) -> Result<(), String> {
        let origin = scope_origin(origin).ok_or_else(|| "not an http(s) origin".to_owned())?;
        let mut origins = self.origins.write().map_err(|_| "scope lock".to_owned())?;
        if !origins.contains(&origin) {
            origins.push(origin);
        }
        Ok(())
    }

    /// Grant the stored deployment, if any, at startup.
    pub fn grant_stored(&self, origin: Option<&str>) {
        if let Some(origin) = origin
            && let Err(error) = self.grant(origin)
        {
            log::warn!("could not scope the webview's fetch: {error}");
        }
    }

    /// Whether the webview may fetch `url`: http(s), no userinfo, a granted origin.
    #[must_use]
    pub fn allows(&self, url: &Url) -> bool {
        if !matches!(url.scheme(), "http" | "https")
            || !url.username().is_empty()
            || url.password().is_some()
            || url.host_str().is_none()
        {
            return false;
        }
        let origin = url.origin().ascii_serialization();
        self.origins
            .read()
            .is_ok_and(|origins| origins.contains(&origin))
    }
}

// ---- the webview's fetch ------------------------------------------------

/// The request half of `http_fetch`, as the page frames it (IPC.md).
#[derive(Debug, Deserialize)]
pub struct FetchMeta {
    pub id: u64,
    pub method: String,
    pub url: String,
    #[serde(default)]
    pub headers: Vec<(String, String)>,
}

/// What `http_fetch` resolves with: the response head.
#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct FetchHead {
    pub status: u16,
    pub status_text: String,
    pub headers: Vec<(String, String)>,
    pub url: String,
    /// False for a HEAD request and a null-body status (1xx, 204, 205,
    /// 304): nothing to read, and nothing is kept.
    pub has_body: bool,
}

/// Errors the page maps: `aborted` becomes an `AbortError`, the others a `TypeError` as fetch does.
#[derive(Debug, Serialize, PartialEq, Eq)]
pub struct FetchError {
    pub code: &'static str,
    pub message: String,
}

impl FetchError {
    fn new(code: &'static str, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
        }
    }

    fn aborted() -> Self {
        Self::new("aborted", "the request was aborted")
    }

    fn too_large(message: &str) -> Self {
        Self::new("body_too_large", message)
    }
}

type SharedResponse = Arc<tokio::sync::Mutex<reqwest::Response>>;

struct InFlight {
    cancel: watch::Sender<bool>,
    response: Option<SharedResponse>,
    /// This entry, as opposed to a later one under the same id.
    serial: u64,
    /// Reads waiting on a chunk now: a stream being read is never idle.
    reading: usize,
    /// Since when nobody has read: set when the head is delivered and
    /// after every read; `None` before the head.
    idle_since: Option<Instant>,
}

impl InFlight {
    fn idle(&self) -> Option<Instant> {
        if self.reading == 0 && self.response.is_some() {
            self.idle_since
        } else {
            None
        }
    }
}

#[derive(Default)]
struct Entries {
    live: HashMap<u64, InFlight>,
    /// Ids aborted before their request reached the host, oldest first.
    cancelled_early: VecDeque<(u64, Instant)>,
    next_serial: u64,
}

impl Entries {
    fn prune_early(&mut self, now: Instant) {
        while self
            .cancelled_early
            .front()
            .is_some_and(|(_, at)| now.duration_since(*at) >= EARLY_CANCEL_TTL)
        {
            self.cancelled_early.pop_front();
        }
    }

    /// Drop the longest-idle response, if any.
    fn evict_oldest_idle(&mut self) -> Option<InFlight> {
        let oldest = self
            .live
            .iter()
            .filter_map(|(id, entry)| Some((*id, entry.idle()?)))
            .min_by_key(|(_, since)| *since)
            .map(|(id, _)| id)?;
        self.live.remove(&oldest)
    }
}

enum IdleCheck {
    Gone,
    Expired,
    Wait(Duration),
}

/// The webview's requests in flight, by the page's id. Cloning shares it.
#[derive(Clone)]
pub struct FetchTable {
    entries: Arc<Mutex<Entries>>,
    idle_timeout: Duration,
    max_open: usize,
}

impl Default for FetchTable {
    fn default() -> Self {
        Self::with_limits(IDLE_TIMEOUT, MAX_OPEN)
    }
}

impl FetchTable {
    #[must_use]
    pub fn with_limits(idle_timeout: Duration, max_open: usize) -> Self {
        Self {
            entries: Arc::new(Mutex::new(Entries::default())),
            idle_timeout,
            max_open: max_open.max(1),
        }
    }

    fn lock(&self) -> Result<std::sync::MutexGuard<'_, Entries>, FetchError> {
        self.entries
            .lock()
            .map_err(|_| FetchError::new("internal", "fetch table lock"))
    }

    /// Take `id` for a new request: refused when the page aborted it
    /// already, or while the id is live. At [`MAX_OPEN`], the longest-idle
    /// response makes room.
    fn register(&self, id: u64) -> Result<(watch::Receiver<bool>, u64), FetchError> {
        let mut entries = self.lock()?;
        entries.prune_early(Instant::now());
        if let Some(at) = entries.cancelled_early.iter().position(|(c, _)| *c == id) {
            entries.cancelled_early.remove(at);
            return Err(FetchError::aborted());
        }
        if entries.live.contains_key(&id) {
            return Err(FetchError::new("invalid_request", "request id in use"));
        }
        if entries.live.len() >= self.max_open
            && let Some(evicted) = entries.evict_oldest_idle()
        {
            log::debug!("fetch table full: dropped the longest-unread response");
            let _ = evicted.cancel.send(true);
        }
        let (cancel, receiver) = watch::channel(false);
        entries.next_serial += 1;
        let serial = entries.next_serial;
        entries.live.insert(
            id,
            InFlight {
                cancel,
                response: None,
                serial,
                reading: 0,
                idle_since: None,
            },
        );
        Ok((receiver, serial))
    }

    /// Store the response for its body reads and start its idle clock;
    /// false when the id was cancelled meanwhile.
    fn attach(&self, id: u64, serial: u64, response: reqwest::Response) -> bool {
        let Ok(mut entries) = self.lock() else {
            return false;
        };
        let Some(entry) = entries.live.get_mut(&id).filter(|e| e.serial == serial) else {
            return false;
        };
        entry.response = Some(Arc::new(tokio::sync::Mutex::new(response)));
        entry.idle_since = Some(Instant::now());
        drop(entries);
        let table = self.clone();
        tokio::spawn(async move {
            let mut wait = table.idle_timeout;
            loop {
                tokio::time::sleep(wait).await;
                match table.idle_check(id, serial) {
                    IdleCheck::Gone => return,
                    IdleCheck::Expired => {
                        log::debug!(
                            "dropped a response nobody read for {:?}",
                            table.idle_timeout
                        );
                        return;
                    }
                    IdleCheck::Wait(next) => wait = next,
                }
            }
        });
        true
    }

    /// Drop the entry when nobody has read it for the idle timeout.
    fn idle_check(&self, id: u64, serial: u64) -> IdleCheck {
        let Ok(mut entries) = self.lock() else {
            return IdleCheck::Gone;
        };
        let Some(entry) = entries.live.get(&id).filter(|e| e.serial == serial) else {
            return IdleCheck::Gone;
        };
        let Some(since) = entry.idle() else {
            return IdleCheck::Wait(self.idle_timeout);
        };
        let idle = since.elapsed();
        if idle < self.idle_timeout {
            return IdleCheck::Wait(self.idle_timeout - idle);
        }
        if let Some(entry) = entries.live.remove(&id) {
            let _ = entry.cancel.send(true);
        }
        IdleCheck::Expired
    }

    /// A body read starting: the response, the cancel signal, and a guard
    /// that keeps the entry from counting as idle until the read ends.
    fn body(&self, id: u64) -> Option<(SharedResponse, watch::Receiver<bool>, Reading)> {
        let mut entries = self.lock().ok()?;
        let entry = entries.live.get_mut(&id)?;
        let response = entry.response.clone()?;
        entry.reading += 1;
        Some((
            response,
            entry.cancel.subscribe(),
            Reading {
                table: self.clone(),
                id,
                serial: entry.serial,
            },
        ))
    }

    fn read_done(&self, id: u64, serial: u64) {
        if let Ok(mut entries) = self.lock()
            && let Some(entry) = entries.live.get_mut(&id).filter(|e| e.serial == serial)
        {
            entry.reading = entry.reading.saturating_sub(1);
            entry.idle_since = Some(Instant::now());
        }
    }

    fn remove(&self, id: u64) {
        if let Ok(mut entries) = self.lock() {
            entries.live.remove(&id);
        }
    }

    /// End one request or stream: a pending send or read returns `aborted`.
    /// An id the host has not seen yet is remembered (bounded, for
    /// [`EARLY_CANCEL_TTL`]), so its request, arriving later, is refused.
    pub fn cancel(&self, id: u64) {
        let Ok(mut entries) = self.lock() else {
            return;
        };
        if let Some(entry) = entries.live.remove(&id) {
            drop(entries);
            let _ = entry.cancel.send(true);
            return;
        }
        let now = Instant::now();
        entries.prune_early(now);
        if !entries.cancelled_early.iter().any(|(c, _)| *c == id) {
            if entries.cancelled_early.len() >= MAX_EARLY_CANCELS {
                entries.cancelled_early.pop_front();
            }
            entries.cancelled_early.push_back((id, now));
        }
    }

    /// End everything (the page is reloading: nobody will read these bodies).
    pub fn clear(&self) {
        let drained: Vec<InFlight> = match self.lock() {
            Ok(mut entries) => {
                entries.cancelled_early.clear();
                entries.live.drain().map(|(_, entry)| entry).collect()
            }
            Err(_) => return,
        };
        for entry in drained {
            let _ = entry.cancel.send(true);
        }
    }

    #[cfg(test)]
    fn len(&self) -> usize {
        self.entries.lock().map(|e| e.live.len()).unwrap_or(0)
    }
}

/// A body read in progress (see [`FetchTable::body`]); ends on drop, which
/// also covers a read whose IPC call is dropped.
struct Reading {
    table: FetchTable,
    id: u64,
    serial: u64,
}

impl Drop for Reading {
    fn drop(&mut self) {
        self.table.read_done(self.id, self.serial);
    }
}

/// The host side of the webview's fetch: the shared client, the scope, the table.
pub struct WebFetch {
    pub http: SharedHttp,
    pub scope: Arc<HttpScope>,
    pub table: FetchTable,
}

/// Request headers a page may not set (fetch spec "forbidden request-header",
/// as the HTTP plugin applied it): the host owns the connection, cookies and the origin.
#[must_use]
pub fn is_forbidden_header(name: &str) -> bool {
    let lower = name.to_ascii_lowercase();
    matches!(
        lower.as_str(),
        "accept-charset"
            | "accept-encoding"
            | "access-control-request-headers"
            | "access-control-request-method"
            | "connection"
            | "content-length"
            | "cookie"
            | "cookie2"
            | "date"
            | "dnt"
            | "expect"
            | "host"
            | "keep-alive"
            | "origin"
            | "referer"
            | "set-cookie"
            | "te"
            | "trailer"
            | "transfer-encoding"
            | "upgrade"
            | "via"
    ) || lower.starts_with("proxy-")
        || lower.starts_with("sec-")
}

/// Split the page's frame: a 4-byte big-endian length, that many bytes of
/// JSON [`FetchMeta`], then the request body (a view of `frame`, not a copy).
///
/// # Errors
///
/// A frame that is cut short or whose metadata is not valid JSON
/// (`invalid_request`); metadata over 1 MiB or a body over `max_body`
/// (`body_too_large`), refused before the metadata is parsed.
pub fn parse_frame(frame: &Bytes, max_body: usize) -> Result<(FetchMeta, Bytes), FetchError> {
    let bad = || FetchError::new("invalid_request", "malformed fetch frame");
    let (len, rest) = frame.split_first_chunk::<4>().ok_or_else(bad)?;
    let len = usize::try_from(u32::from_be_bytes(*len)).map_err(|_| bad())?;
    if rest.len() < len {
        return Err(bad());
    }
    if len > MAX_FRAME_META {
        return Err(FetchError::too_large("the request's headers are too large"));
    }
    if rest.len() - len > max_body {
        return Err(FetchError::too_large(&format!(
            "the request body is over the desktop app's {} MiB limit",
            max_body / (1024 * 1024)
        )));
    }
    let meta: FetchMeta = serde_json::from_slice(&rest[..len]).map_err(|_| bad())?;
    Ok((meta, frame.slice(4 + len..)))
}

/// The frame the IPC's postMessage fallback delivers as a JSON array of
/// numbers. Only small frames: see [`MAX_JSON_FRAME`].
///
/// # Errors
///
/// `body_too_large` past [`MAX_JSON_FRAME`] (checked before any byte is
/// read), `invalid_request` for anything but bytes.
pub fn frame_from_json(values: &[serde_json::Value]) -> Result<Bytes, FetchError> {
    if values.len() > MAX_JSON_FRAME {
        return Err(FetchError::too_large(
            "the request is too large for this app's fallback IPC channel",
        ));
    }
    let mut frame = Vec::with_capacity(values.len());
    for value in values {
        let byte = value
            .as_u64()
            .and_then(|b| u8::try_from(b).ok())
            .ok_or_else(|| FetchError::new("invalid_request", "expected a binary frame"))?;
        frame.push(byte);
    }
    Ok(Bytes::from(frame))
}

/// Build the outbound request from the page's frame, enforcing the scope.
///
/// # Errors
///
/// A URL outside the scope, a bad method or header.
pub fn outbound(
    client: &reqwest::Client,
    scope: &HttpScope,
    meta: &FetchMeta,
    body: Bytes,
) -> Result<reqwest::RequestBuilder, FetchError> {
    let url =
        Url::parse(&meta.url).map_err(|_| FetchError::new("invalid_request", "not a valid URL"))?;
    if !scope.allows(&url) {
        return Err(FetchError::new(
            "url_not_allowed",
            "the app may only reach the connected deployment",
        ));
    }
    let method = reqwest::Method::from_bytes(meta.method.to_ascii_uppercase().as_bytes())
        .map_err(|_| FetchError::new("invalid_request", "not a valid method"))?;
    let mut headers = reqwest::header::HeaderMap::new();
    for (name, value) in &meta.headers {
        if is_forbidden_header(name) {
            continue;
        }
        let name = reqwest::header::HeaderName::from_bytes(name.as_bytes())
            .map_err(|_| FetchError::new("invalid_request", "not a valid header name"))?;
        let value = reqwest::header::HeaderValue::from_str(value)
            .map_err(|_| FetchError::new("invalid_request", "not a valid header value"))?;
        headers.append(name, value);
    }
    headers.insert(
        reqwest::header::ORIGIN,
        reqwest::header::HeaderValue::from_static(PAGE_ORIGIN),
    );
    let mut request = client.request(method.clone(), url).headers(headers);
    if !body.is_empty() {
        request = request.body(body);
    } else if matches!(method, reqwest::Method::POST | reqwest::Method::PUT) {
        // The fetch spec's zero Content-Length for a bodiless POST / PUT.
        request = request.header(reqwest::header::CONTENT_LENGTH, "0");
    }
    Ok(request)
}

/// Whether a response with `status` to a HEAD (`head`) or another request has a body to read.
fn has_body(head: bool, status: u16) -> bool {
    !head && !matches!(status, 100..=199 | 204 | 205 | 304)
}

fn head_of(head: bool, response: &reqwest::Response) -> FetchHead {
    let status = response.status();
    FetchHead {
        status: status.as_u16(),
        status_text: status.canonical_reason().unwrap_or_default().to_owned(),
        headers: response
            .headers()
            .iter()
            .filter_map(|(k, v)| Some((k.as_str().to_owned(), v.to_str().ok()?.to_owned())))
            .collect(),
        url: response.url().to_string(),
        has_body: has_body(head, status.as_u16()),
    }
}

/// Send one request and resolve with its head; the body stays with the host
/// under the page's id until read to its end, cancelled, or left unread
/// past the idle timeout.
///
/// # Errors
///
/// [`FetchError`]: `aborted`, `url_not_allowed`, `invalid_request`,
/// `body_too_large`, `network`.
pub async fn fetch(state: &WebFetch, frame: Bytes) -> Result<FetchHead, FetchError> {
    let (meta, body) = parse_frame(&frame, MAX_REQUEST_BODY)?;
    drop(frame);
    // Registered (an early abort refuses here) before anything is sent.
    let (mut cancelled, serial) = state.table.register(meta.id)?;
    let request = match outbound(state.http.client().await, &state.scope, &meta, body) {
        Ok(request) => request,
        Err(error) => {
            state.table.remove(meta.id);
            return Err(error);
        }
    };
    let is_head = meta.method.eq_ignore_ascii_case("HEAD");
    let sent = tokio::select! {
        sent = request.send() => sent,
        _ = cancelled.wait_for(|c| *c) => {
            state.table.remove(meta.id);
            return Err(FetchError::aborted());
        }
    };
    let response = match sent {
        Ok(response) => response,
        Err(error) => {
            state.table.remove(meta.id);
            return Err(FetchError::new("network", network_message(&error)));
        }
    };
    let head = head_of(is_head, &response);
    if !head.has_body {
        state.table.remove(meta.id);
        return Ok(head);
    }
    if !state.table.attach(meta.id, serial, response) {
        return Err(FetchError::aborted());
    }
    Ok(head)
}

fn network_message(error: &reqwest::Error) -> String {
    if error.is_timeout() {
        "the deployment did not answer in time".to_owned()
    } else if error.is_connect() {
        "could not reach the deployment".to_owned()
    } else {
        "the request failed".to_owned()
    }
}

/// The next chunk of a body; an empty buffer is the end (the id is then forgotten).
///
/// # Errors
///
/// `aborted` once cancelled, `unknown_request` for an id with no body
/// (never had one, read to its end, or dropped unread), `network`.
pub async fn read_body(state: &WebFetch, id: u64) -> Result<Bytes, FetchError> {
    let (response, mut cancelled, _reading) = state
        .table
        .body(id)
        .ok_or_else(|| FetchError::new("unknown_request", "no such response"))?;
    let mut response = response.lock().await;
    loop {
        let chunk = tokio::select! {
            chunk = response.chunk() => chunk,
            _ = cancelled.wait_for(|c| *c) => return Err(FetchError::aborted()),
        };
        match chunk {
            Ok(Some(bytes)) if bytes.is_empty() => {}
            Ok(Some(bytes)) => return Ok(bytes),
            Ok(None) => {
                state.table.remove(id);
                return Ok(Bytes::new());
            }
            Err(error) => {
                state.table.remove(id);
                return Err(FetchError::new("network", network_message(&error)));
            }
        }
    }
}

// ---- commands ------------------------------------------------------------

/// `http_fetch`: the raw IPC body is the frame [`parse_frame`] reads.
#[tauri::command]
pub async fn http_fetch(
    request: tauri::ipc::Request<'_>,
    state: State<'_, WebFetch>,
) -> Result<FetchHead, FetchError> {
    let frame = match request.body() {
        tauri::ipc::InvokeBody::Raw(frame) => {
            // Refused before the one copy out of the IPC's buffer.
            if frame.len() > 4 + MAX_FRAME_META + MAX_REQUEST_BODY {
                return Err(FetchError::too_large(
                    "the request body is over the desktop app's limit",
                ));
            }
            Bytes::copy_from_slice(frame)
        }
        // The postMessage fallback of the IPC delivers a typed array as JSON numbers.
        tauri::ipc::InvokeBody::Json(serde_json::Value::Array(values)) => frame_from_json(values)?,
        tauri::ipc::InvokeBody::Json(_) => {
            return Err(FetchError::new(
                "invalid_request",
                "expected a binary frame",
            ));
        }
    };
    fetch(&state, frame).await
}

/// `http_read_body`: the next chunk as raw bytes; zero bytes is the end.
#[tauri::command]
pub async fn http_read_body(
    id: u64,
    state: State<'_, WebFetch>,
) -> Result<tauri::ipc::Response, FetchError> {
    read_body(&state, id)
        .await
        .map(|chunk| tauri::ipc::Response::new(Vec::from(chunk)))
}

/// `http_cancel`: abort a pending request or end a body. An id the host
/// has not seen yet is refused when its request arrives.
#[tauri::command]
pub fn http_cancel(id: u64, state: State<'_, WebFetch>) {
    state.table.cancel(id);
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{BufRead, BufReader, Read, Write};
    use std::net::TcpListener;

    fn frame(meta: &serde_json::Value, body: &[u8]) -> Bytes {
        let meta = serde_json::to_vec(meta).unwrap();
        let mut out = u32::try_from(meta.len()).unwrap().to_be_bytes().to_vec();
        out.extend_from_slice(&meta);
        out.extend_from_slice(body);
        Bytes::from(out)
    }

    /// A one-request-per-connection HTTP/1.1 server. `/redirect` answers 302
    /// elsewhere, `/stream` sends two chunks with a pause, `/slow-stream`
    /// two chunks 400 ms apart, `/hang` never answers, `/echo` returns the
    /// request head it saw, anything else 200 "ok".
    fn server() -> (u16, std::sync::mpsc::Receiver<String>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let (seen_tx, seen_rx) = std::sync::mpsc::channel();
        std::thread::spawn(move || {
            for stream in listener.incoming() {
                let Ok(mut stream) = stream else { continue };
                let seen_tx = seen_tx.clone();
                std::thread::spawn(move || {
                    let mut reader = BufReader::new(stream.try_clone().unwrap());
                    let mut head = String::new();
                    let mut length = 0usize;
                    loop {
                        let mut line = String::new();
                        if reader.read_line(&mut line).unwrap_or(0) == 0 {
                            return;
                        }
                        if let Some(v) = line.to_ascii_lowercase().strip_prefix("content-length:") {
                            length = v.trim().parse().unwrap_or(0);
                        }
                        let end = line == "\r\n";
                        head.push_str(&line);
                        if end {
                            break;
                        }
                    }
                    let mut body = vec![0; length];
                    reader.read_exact(&mut body).unwrap();
                    head.push_str(&String::from_utf8_lossy(&body));
                    let _ = seen_tx.send(head.clone());
                    let path = head.split(' ').nth(1).unwrap_or("/").to_owned();
                    match path.as_str() {
                        "/redirect" => {
                            let _ = stream.write_all(b"HTTP/1.1 302 Found\r\nLocation: http://127.0.0.1:1/secret\r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
                        }
                        "/stream" | "/slow-stream" => {
                            let _ = stream.write_all(b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n6\r\ndata 1\r\n");
                            let _ = stream.flush();
                            std::thread::sleep(Duration::from_millis(if path == "/stream" {
                                150
                            } else {
                                400
                            }));
                            let _ = stream.write_all(b"6\r\ndata 2\r\n0\r\n\r\n");
                        }
                        "/hang" => std::thread::sleep(Duration::from_secs(30)),
                        "/no-content" => {
                            let _ = stream
                                .write_all(b"HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n");
                        }
                        _ => {
                            let _ = stream.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok");
                        }
                    }
                    let _ = stream.flush();
                });
            }
        });
        (port, seen_rx)
    }

    fn state_with(port: u16, table: FetchTable) -> WebFetch {
        let scope = HttpScope::default();
        scope.grant(&format!("http://127.0.0.1:{port}")).unwrap();
        WebFetch {
            http: SharedHttp::new("0.0.0-test"),
            scope: Arc::new(scope),
            table,
        }
    }

    fn state(port: u16) -> WebFetch {
        state_with(port, FetchTable::default())
    }

    fn request(id: u64, port: u16, method: &str, path: &str) -> Bytes {
        frame(
            &serde_json::json!({"id": id, "method": method, "url": format!("http://127.0.0.1:{port}{path}")}),
            b"",
        )
    }

    fn get(id: u64, port: u16, path: &str) -> Bytes {
        request(id, port, "GET", path)
    }

    async fn read_all(state: &WebFetch, id: u64) -> Vec<u8> {
        let mut out = Vec::new();
        loop {
            let chunk = read_body(state, id).await.unwrap();
            if chunk.is_empty() {
                return out;
            }
            out.extend_from_slice(&chunk);
        }
    }

    #[test]
    fn the_scope_is_the_granted_origin_only() {
        let scope = HttpScope::default();
        scope.grant("https://elitea.example.com/some/path").unwrap();
        let allows = |u: &str| scope.allows(&Url::parse(u).unwrap());
        assert!(allows("https://elitea.example.com/api/v2/x?y=1"));
        assert!(allows("https://elitea.example.com:443/"));
        for denied in [
            "http://elitea.example.com/",
            "https://elitea.example.com:8443/",
            "https://evil.example/",
            "https://elitea.example.com.evil.example/",
            "https://user@elitea.example.com/",
            "https://user:pw@elitea.example.com/",
            "file:///etc/passwd",
        ] {
            assert!(!allows(denied), "{denied}");
        }
        for bad in [
            "file:///etc",
            "tauri://localhost",
            "nonsense",
            "javascript:1",
        ] {
            assert!(scope.grant(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn an_empty_scope_allows_nothing() {
        assert!(!HttpScope::default().allows(&Url::parse("https://a.example/").unwrap()));
    }

    #[test]
    fn frames_are_length_prefixed_metadata_then_body() {
        let f = frame(
            &serde_json::json!({"id": 7, "method": "POST", "url": "https://a/", "headers": [["a", "b"]]}),
            b"{\"x\":1}",
        );
        let (meta, body) = parse_frame(&f, MAX_REQUEST_BODY).unwrap();
        assert_eq!(meta.id, 7);
        assert_eq!(meta.headers, vec![("a".to_owned(), "b".to_owned())]);
        assert_eq!(&body[..], b"{\"x\":1}");
        // The body is a view of the frame, not a copy.
        assert_eq!(body.as_ptr(), f[f.len() - body.len()..].as_ptr());
        for bad in [&[0u8, 0][..], &[0, 0, 0, 9, b'{'], &[0, 0, 0, 1, b'x']] {
            assert!(parse_frame(&Bytes::copy_from_slice(bad), MAX_REQUEST_BODY).is_err());
        }
    }

    #[test]
    fn an_oversized_body_or_header_block_is_refused_before_parsing() {
        let meta = serde_json::json!({"id": 1, "method": "POST", "url": "https://a/"});
        assert!(parse_frame(&frame(&meta, &[1; 8]), 8).is_ok());
        assert_eq!(
            parse_frame(&frame(&meta, &[1; 9]), 8).unwrap_err().code,
            "body_too_large"
        );
        // A metadata length over the bound: refused without reading it as JSON.
        let mut huge = u32::try_from(MAX_FRAME_META + 1)
            .unwrap()
            .to_be_bytes()
            .to_vec();
        huge.resize(4 + MAX_FRAME_META + 1, b'x');
        assert_eq!(
            parse_frame(&Bytes::from(huge), MAX_REQUEST_BODY)
                .unwrap_err()
                .code,
            "body_too_large"
        );
    }

    #[test]
    fn the_json_fallback_takes_small_frames_only() {
        let small: Vec<serde_json::Value> = [0, 0, 0, 2, b'{', b'}']
            .iter()
            .map(|b| serde_json::json!(b))
            .collect();
        assert_eq!(&frame_from_json(&small).unwrap()[..], b"\0\0\0\x02{}");
        assert_eq!(
            frame_from_json(&[serde_json::json!(256)]).unwrap_err().code,
            "invalid_request"
        );
        let big = vec![serde_json::json!(0); MAX_JSON_FRAME + 1];
        assert_eq!(frame_from_json(&big).unwrap_err().code, "body_too_large");
    }

    #[test]
    fn forbidden_headers_match_the_fetch_spec() {
        for h in [
            "Cookie",
            "origin",
            "Host",
            "content-length",
            "Sec-Fetch-Mode",
            "Proxy-Authorization",
            "Transfer-Encoding",
        ] {
            assert!(is_forbidden_header(h), "{h}");
        }
        for h in [
            "Authorization",
            "Content-Type",
            "Accept",
            "X-Client-Version",
            "Last-Event-ID",
        ] {
            assert!(!is_forbidden_header(h), "{h}");
        }
    }

    #[test]
    fn head_and_null_body_statuses_have_no_body() {
        assert!(!has_body(true, 200));
        for status in [100, 101, 103, 204, 205, 304] {
            assert!(!has_body(false, status), "{status}");
        }
        for status in [200, 201, 302, 404, 500] {
            assert!(has_body(false, status), "{status}");
        }
    }

    #[tokio::test]
    async fn a_request_outside_the_scope_never_leaves() {
        let (port, seen) = server();
        let state = state(port);
        let f = frame(
            &serde_json::json!({"id": 1, "method": "GET", "url": "http://localhost:1/x"}),
            b"",
        );
        let error = fetch(&state, f).await.unwrap_err();
        assert_eq!(error.code, "url_not_allowed");
        assert!(seen.try_recv().is_err());
        assert_eq!(state.table.len(), 0);
    }

    #[tokio::test]
    async fn redirects_are_returned_not_followed() {
        let (port, _seen) = server();
        let state = state(port);
        let head = fetch(&state, get(1, port, "/redirect")).await.unwrap();
        assert_eq!(head.status, 302);
        assert!(
            head.headers
                .iter()
                .any(|(k, v)| k == "location" && v.contains("/secret"))
        );
        let _ = read_all(&state, 1).await;
        assert_eq!(state.table.len(), 0);
    }

    #[tokio::test]
    async fn headers_are_filtered_and_the_page_origin_is_sent() {
        let (port, seen) = server();
        let state = state(port);
        let f = frame(
            &serde_json::json!({"id": 3, "method": "post", "url": format!("http://127.0.0.1:{port}/echo"),
                "headers": [["Authorization", "Bearer t"], ["Cookie", "a=b"], ["Origin", "https://evil.example"], ["Content-Type", "application/json"]]}),
            b"{\"q\":1}",
        );
        let head = fetch(&state, f).await.unwrap();
        assert_eq!(head.status, 200);
        assert_eq!(read_all(&state, 3).await, b"ok");
        let request = seen
            .recv_timeout(Duration::from_secs(5))
            .unwrap()
            .to_ascii_lowercase();
        assert!(request.starts_with("post /echo "), "{request}");
        assert!(request.contains("authorization: bearer t"));
        assert!(request.contains("content-type: application/json"));
        assert!(request.contains("origin: tauri://localhost"));
        assert!(!request.contains("cookie:"));
        assert!(!request.contains("evil.example"));
        assert!(request.contains("user-agent: elitea-desktop/0.0.0-test"));
        assert!(request.ends_with("{\"q\":1}"));
    }

    #[tokio::test]
    async fn a_body_streams_chunk_by_chunk() {
        let (port, _seen) = server();
        let state = state(port);
        let head = fetch(&state, get(4, port, "/stream")).await.unwrap();
        assert!(head.has_body);
        let first = read_body(&state, 4).await.unwrap();
        assert_eq!(&first[..], b"data 1");
        assert_eq!(read_all(&state, 4).await, b"data 2");
        assert_eq!(state.table.len(), 0);
    }

    #[tokio::test]
    async fn a_null_body_status_keeps_nothing() {
        let (port, _seen) = server();
        let state = state(port);
        let head = fetch(&state, get(5, port, "/no-content")).await.unwrap();
        assert_eq!(head.status, 204);
        assert!(!head.has_body);
        assert_eq!(state.table.len(), 0);
    }

    #[tokio::test]
    async fn a_head_request_keeps_nothing() {
        let (port, _seen) = server();
        let state = state(port);
        let head = fetch(&state, request(11, port, "HEAD", "/")).await.unwrap();
        assert_eq!(head.status, 200);
        assert!(!head.has_body);
        assert_eq!(state.table.len(), 0);
    }

    #[tokio::test]
    async fn an_unread_response_is_dropped_after_the_idle_timeout() {
        let (port, _seen) = server();
        let state = state_with(
            port,
            FetchTable::with_limits(Duration::from_millis(100), MAX_OPEN),
        );
        fetch(&state, get(12, port, "/")).await.unwrap();
        assert_eq!(state.table.len(), 1);
        tokio::time::sleep(Duration::from_millis(300)).await;
        assert_eq!(state.table.len(), 0);
        assert_eq!(
            read_body(&state, 12).await.unwrap_err().code,
            "unknown_request"
        );
    }

    #[tokio::test]
    async fn a_stream_being_read_is_never_idle() {
        let (port, _seen) = server();
        let state = state_with(
            port,
            FetchTable::with_limits(Duration::from_millis(100), MAX_OPEN),
        );
        fetch(&state, get(13, port, "/slow-stream")).await.unwrap();
        assert_eq!(&read_body(&state, 13).await.unwrap()[..], b"data 1");
        // The next chunk is 400 ms away, four idle timeouts: the read
        // waiting on it keeps the stream.
        assert_eq!(&read_body(&state, 13).await.unwrap()[..], b"data 2");
        assert!(read_body(&state, 13).await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn a_full_table_drops_its_longest_unread_response() {
        let (port, _seen) = server();
        let state = state_with(port, FetchTable::with_limits(IDLE_TIMEOUT, 2));
        fetch(&state, get(14, port, "/")).await.unwrap();
        tokio::time::sleep(Duration::from_millis(5)).await;
        fetch(&state, get(15, port, "/")).await.unwrap();
        fetch(&state, get(16, port, "/")).await.unwrap();
        assert_eq!(state.table.len(), 2);
        assert_eq!(
            read_body(&state, 14).await.unwrap_err().code,
            "unknown_request"
        );
        assert_eq!(read_all(&state, 15).await, b"ok");
        assert_eq!(read_all(&state, 16).await, b"ok");
    }

    #[tokio::test]
    async fn an_abort_that_arrives_first_refuses_its_request() {
        let (port, seen) = server();
        let state = state(port);
        state.table.cancel(17);
        assert_eq!(
            fetch(&state, get(17, port, "/")).await.unwrap_err(),
            FetchError::aborted()
        );
        assert!(seen.try_recv().is_err(), "nothing was sent");
        assert_eq!(state.table.len(), 0);
        // Remembered once: other ids are unaffected.
        fetch(&state, get(18, port, "/")).await.unwrap();
        assert_eq!(read_all(&state, 18).await, b"ok");
    }

    #[test]
    fn early_aborts_are_bounded() {
        let table = FetchTable::default();
        for id in 0..=u64::try_from(MAX_EARLY_CANCELS).unwrap() {
            table.cancel(id);
        }
        // The oldest was forgotten to make room; the newest refuses.
        assert!(table.register(0).is_ok());
        assert_eq!(
            table.register(1).unwrap_err(),
            FetchError::aborted(),
            "id 1 is still remembered"
        );
        assert_eq!(
            table.entries.lock().unwrap().cancelled_early.len(),
            MAX_EARLY_CANCELS - 1
        );
        table.clear();
        assert!(table.entries.lock().unwrap().cancelled_early.is_empty());
    }

    #[tokio::test]
    async fn cancel_ends_a_pending_request() {
        let (port, _seen) = server();
        let state = Arc::new(state(port));
        let pending = {
            let state = state.clone();
            tokio::spawn(async move { fetch(&state, get(6, port, "/hang")).await })
        };
        tokio::time::sleep(Duration::from_millis(200)).await;
        state.table.cancel(6);
        let result = tokio::time::timeout(Duration::from_secs(5), pending)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(result.unwrap_err(), FetchError::aborted());
        assert_eq!(state.table.len(), 0);
    }

    #[tokio::test]
    async fn cancel_ends_a_pending_body_read_and_a_reused_id_is_refused_while_live() {
        let (port, _seen) = server();
        let state = Arc::new(state(port));
        fetch(&state, get(8, port, "/stream")).await.unwrap();
        assert_eq!(
            fetch(&state, get(8, port, "/")).await.unwrap_err().code,
            "invalid_request"
        );
        assert_eq!(&read_body(&state, 8).await.unwrap()[..], b"data 1");
        let reading = {
            let state = state.clone();
            tokio::spawn(async move { read_body(&state, 8).await })
        };
        tokio::time::sleep(Duration::from_millis(20)).await;
        state.table.cancel(8);
        let result = tokio::time::timeout(Duration::from_secs(5), reading)
            .await
            .unwrap()
            .unwrap();
        // Either the cancel won, or the second chunk had already arrived.
        assert!(matches!(result, Err(ref e) if *e == FetchError::aborted()) || result.is_ok());
        assert_eq!(state.table.len(), 0);
        assert_eq!(
            read_body(&state, 8).await.unwrap_err().code,
            "unknown_request"
        );
    }

    #[tokio::test]
    async fn clear_ends_everything_for_a_reloading_page() {
        let (port, _seen) = server();
        let state = state(port);
        fetch(&state, get(9, port, "/stream")).await.unwrap();
        fetch(&state, get(10, port, "/stream")).await.unwrap();
        assert_eq!(state.table.len(), 2);
        state.table.clear();
        assert_eq!(state.table.len(), 0);
    }

    #[tokio::test]
    async fn one_client_serves_every_request() {
        let (port, _seen) = server();
        let state = state(port);
        let first: *const reqwest::Client = state.http.client().await;
        for id in 20..25 {
            fetch(&state, get(id, port, "/")).await.unwrap();
            assert_eq!(read_all(&state, id).await, b"ok");
        }
        assert!(std::ptr::eq(first, state.http.client().await));
        let clone = state.http.clone();
        assert!(std::ptr::eq(first, clone.client().await));
    }

    /// Concurrent first callers on a single-threaded runtime: a build that
    /// blocked the runtime's only thread would serialise them behind it;
    /// awaited, a timer on the same thread keeps ticking meanwhile.
    #[tokio::test(flavor = "current_thread")]
    async fn the_first_build_does_not_block_the_runtime() {
        let http = SharedHttp::new("0.0.0-test");
        let ticks = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let ticker = {
            let ticks = ticks.clone();
            tokio::spawn(async move {
                loop {
                    ticks.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                    tokio::task::yield_now().await;
                }
            })
        };
        tokio::task::yield_now().await;
        let before = ticks.load(std::sync::atomic::Ordering::SeqCst);
        let (a, b) = tokio::join!(http.client(), http.client());
        assert!(std::ptr::eq(a, b));
        assert!(
            ticks.load(std::sync::atomic::Ordering::SeqCst) > before + 1,
            "the runtime ran other tasks while the client was built"
        );
        ticker.abort();
    }
}
