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
//! [`SharedHttp`] builds once, off the main thread
//! ([`SharedHttp::prewarm`]), and every caller — the token endpoint,
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

use std::collections::HashMap;
use std::sync::{Arc, Mutex, OnceLock, RwLock};
use std::time::Duration;

use serde::{Deserialize, Serialize};
use tauri::State;
use tokio::sync::watch;
use url::Url;

/// A connection that does not open in this long is not going to.
const CONNECT_TIMEOUT: Duration = Duration::from_secs(15);

/// The `Origin` the plugin sent for the bundled page (`tauri://localhost`);
/// the deployment has always seen it, so the host keeps sending it.
const PAGE_ORIGIN: &str = "tauri://localhost";

/// The one lazily built client. Cloning shares it.
#[derive(Clone)]
pub struct SharedHttp {
    client: Arc<OnceLock<reqwest::Client>>,
    user_agent: Arc<str>,
}

impl SharedHttp {
    #[must_use]
    pub fn new(client_version: &str) -> Self {
        Self {
            client: Arc::new(OnceLock::new()),
            user_agent: format!("elitea-desktop/{client_version}").into(),
        }
    }

    /// Build the client on a background thread now, so the first request
    /// (or the setup that needs it) does not pay the root-store load.
    pub fn prewarm(&self) {
        let this = self.clone();
        let spawned = std::thread::Builder::new()
            .name("http-prewarm".into())
            .spawn(move || {
                let started = std::time::Instant::now();
                let _ = this.client();
                log::debug!("http client ready in {:?}", started.elapsed());
            });
        if let Err(error) = spawned {
            log::warn!("could not prewarm the http client: {error}");
        }
    }

    /// The client; the first caller builds it, concurrent callers wait for that build.
    pub fn client(&self) -> &reqwest::Client {
        self.client.get_or_init(|| build_client(&self.user_agent))
    }
}

fn builder(user_agent: &str) -> reqwest::ClientBuilder {
    reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .connect_timeout(CONNECT_TIMEOUT)
        .user_agent(user_agent)
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
            builder(user_agent)
                .tls_built_in_native_certs(false)
                .build()
                .unwrap_or_else(|_| reqwest::Client::new())
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
    /// False for a null-body status (101, 103, 204, 205, 304): nothing to read.
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
}

struct InFlight {
    cancel: watch::Sender<bool>,
    response: Option<Arc<tokio::sync::Mutex<reqwest::Response>>>,
}

/// The webview's requests in flight, by the page's id.
#[derive(Default)]
pub struct FetchTable {
    entries: Mutex<HashMap<u64, InFlight>>,
}

impl FetchTable {
    fn register(&self, id: u64) -> Result<watch::Receiver<bool>, FetchError> {
        let mut entries = self
            .entries
            .lock()
            .map_err(|_| FetchError::new("internal", "fetch table lock"))?;
        if entries.contains_key(&id) {
            return Err(FetchError::new("invalid_request", "request id in use"));
        }
        let (cancel, receiver) = watch::channel(false);
        entries.insert(
            id,
            InFlight {
                cancel,
                response: None,
            },
        );
        Ok(receiver)
    }

    /// Store the response for its body reads; false when the id was cancelled meanwhile.
    fn attach(&self, id: u64, response: reqwest::Response) -> bool {
        let Ok(mut entries) = self.entries.lock() else {
            return false;
        };
        match entries.get_mut(&id) {
            Some(entry) => {
                entry.response = Some(Arc::new(tokio::sync::Mutex::new(response)));
                true
            }
            None => false,
        }
    }

    fn body(
        &self,
        id: u64,
    ) -> Option<(
        Arc<tokio::sync::Mutex<reqwest::Response>>,
        watch::Receiver<bool>,
    )> {
        let entries = self.entries.lock().ok()?;
        let entry = entries.get(&id)?;
        Some((entry.response.clone()?, entry.cancel.subscribe()))
    }

    fn remove(&self, id: u64) {
        if let Ok(mut entries) = self.entries.lock() {
            entries.remove(&id);
        }
    }

    /// End one request or stream: a pending send or read returns `aborted`.
    pub fn cancel(&self, id: u64) {
        let entry = self.entries.lock().ok().and_then(|mut e| e.remove(&id));
        if let Some(entry) = entry {
            let _ = entry.cancel.send(true);
        }
    }

    /// End everything (the page is reloading: nobody will read these bodies).
    pub fn clear(&self) {
        let drained: Vec<InFlight> = match self.entries.lock() {
            Ok(mut entries) => entries.drain().map(|(_, entry)| entry).collect(),
            Err(_) => return,
        };
        for entry in drained {
            let _ = entry.cancel.send(true);
        }
    }

    #[cfg(test)]
    fn len(&self) -> usize {
        self.entries.lock().map(|e| e.len()).unwrap_or(0)
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
/// JSON [`FetchMeta`], then the request body.
///
/// # Errors
///
/// A frame that is cut short or whose metadata is not valid JSON.
pub fn parse_frame(frame: &[u8]) -> Result<(FetchMeta, &[u8]), FetchError> {
    let bad = || FetchError::new("invalid_request", "malformed fetch frame");
    let (len, rest) = frame.split_first_chunk::<4>().ok_or_else(bad)?;
    let len = usize::try_from(u32::from_be_bytes(*len)).map_err(|_| bad())?;
    if rest.len() < len {
        return Err(bad());
    }
    let (meta, body) = rest.split_at(len);
    let meta: FetchMeta = serde_json::from_slice(meta).map_err(|_| bad())?;
    Ok((meta, body))
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
    body: &[u8],
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
        request = request.body(body.to_vec());
    } else if matches!(method, reqwest::Method::POST | reqwest::Method::PUT) {
        // The fetch spec's zero Content-Length for a bodiless POST / PUT.
        request = request.header(reqwest::header::CONTENT_LENGTH, "0");
    }
    Ok(request)
}

fn head_of(response: &reqwest::Response) -> FetchHead {
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
        has_body: !matches!(status.as_u16(), 101 | 103 | 204 | 205 | 304),
    }
}

/// Send one request and resolve with its head; the body stays with the host
/// under the page's id until read to its end or cancelled.
///
/// # Errors
///
/// [`FetchError`]: `aborted`, `url_not_allowed`, `invalid_request`, `network`.
pub async fn fetch(state: &WebFetch, frame: &[u8]) -> Result<FetchHead, FetchError> {
    let (meta, body) = parse_frame(frame)?;
    let request = outbound(state.http.client(), &state.scope, &meta, body)?;
    let mut cancelled = state.table.register(meta.id)?;
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
    let head = head_of(&response);
    if !head.has_body {
        state.table.remove(meta.id);
        return Ok(head);
    }
    if !state.table.attach(meta.id, response) {
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

/// The next chunk of a body; an empty vector is the end (the id is then forgotten).
///
/// # Errors
///
/// `aborted` once cancelled, `unknown_request` for an id with no body, `network`.
pub async fn read_body(state: &WebFetch, id: u64) -> Result<Vec<u8>, FetchError> {
    let (response, mut cancelled) = state
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
            Ok(Some(bytes)) => return Ok(bytes.to_vec()),
            Ok(None) => {
                state.table.remove(id);
                return Ok(Vec::new());
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
    match request.body() {
        tauri::ipc::InvokeBody::Raw(frame) => fetch(&state, frame).await,
        // The postMessage fallback of the IPC delivers a typed array as JSON numbers.
        tauri::ipc::InvokeBody::Json(serde_json::Value::Array(values)) => {
            let frame = values
                .iter()
                .map(|v| v.as_u64().and_then(|b| u8::try_from(b).ok()))
                .collect::<Option<Vec<u8>>>()
                .ok_or_else(|| FetchError::new("invalid_request", "expected a binary frame"))?;
            fetch(&state, &frame).await
        }
        tauri::ipc::InvokeBody::Json(_) => Err(FetchError::new(
            "invalid_request",
            "expected a binary frame",
        )),
    }
}

/// `http_read_body`: the next chunk as raw bytes; zero bytes is the end.
#[tauri::command]
pub async fn http_read_body(
    id: u64,
    state: State<'_, WebFetch>,
) -> Result<tauri::ipc::Response, FetchError> {
    read_body(&state, id).await.map(tauri::ipc::Response::new)
}

/// `http_cancel`: abort a pending request or end a body (a no-op for an unknown id).
#[tauri::command]
pub fn http_cancel(id: u64, state: State<'_, WebFetch>) {
    state.table.cancel(id);
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{BufRead, BufReader, Read, Write};
    use std::net::TcpListener;

    fn frame(meta: &serde_json::Value, body: &[u8]) -> Vec<u8> {
        let meta = serde_json::to_vec(meta).unwrap();
        let mut out = u32::try_from(meta.len()).unwrap().to_be_bytes().to_vec();
        out.extend_from_slice(&meta);
        out.extend_from_slice(body);
        out
    }

    /// A one-request-per-connection HTTP/1.1 server. `/redirect` answers 302
    /// elsewhere, `/stream` sends two chunks with a pause, `/hang` never
    /// answers, `/echo` returns the request head it saw, anything else 200 "ok".
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
                        "/stream" => {
                            let _ = stream.write_all(b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n6\r\ndata 1\r\n");
                            let _ = stream.flush();
                            std::thread::sleep(Duration::from_millis(150));
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

    fn state(port: u16) -> WebFetch {
        let scope = HttpScope::default();
        scope.grant(&format!("http://127.0.0.1:{port}")).unwrap();
        WebFetch {
            http: SharedHttp::new("0.0.0-test"),
            scope: Arc::new(scope),
            table: FetchTable::default(),
        }
    }

    fn get(id: u64, port: u16, path: &str) -> Vec<u8> {
        frame(
            &serde_json::json!({"id": id, "method": "GET", "url": format!("http://127.0.0.1:{port}{path}")}),
            b"",
        )
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
        let (meta, body) = parse_frame(&f).unwrap();
        assert_eq!(meta.id, 7);
        assert_eq!(meta.headers, vec![("a".to_owned(), "b".to_owned())]);
        assert_eq!(body, b"{\"x\":1}");
        assert!(parse_frame(&[0, 0]).is_err());
        assert!(parse_frame(&[0, 0, 0, 9, b'{']).is_err());
        assert!(parse_frame(&[0, 0, 0, 1, b'x']).is_err());
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

    #[tokio::test]
    async fn a_request_outside_the_scope_never_leaves() {
        let (port, seen) = server();
        let state = state(port);
        let f = frame(
            &serde_json::json!({"id": 1, "method": "GET", "url": "http://localhost:1/x"}),
            b"",
        );
        let error = fetch(&state, &f).await.unwrap_err();
        assert_eq!(error.code, "url_not_allowed");
        assert!(seen.try_recv().is_err());
        assert_eq!(state.table.len(), 0);
    }

    #[tokio::test]
    async fn redirects_are_returned_not_followed() {
        let (port, _seen) = server();
        let state = state(port);
        let head = fetch(&state, &get(1, port, "/redirect")).await.unwrap();
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
        let head = fetch(&state, &f).await.unwrap();
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
        let head = fetch(&state, &get(4, port, "/stream")).await.unwrap();
        assert!(head.has_body);
        let first = read_body(&state, 4).await.unwrap();
        assert_eq!(first, b"data 1");
        assert_eq!(read_all(&state, 4).await, b"data 2");
        assert_eq!(state.table.len(), 0);
    }

    #[tokio::test]
    async fn a_null_body_status_keeps_nothing() {
        let (port, _seen) = server();
        let state = state(port);
        let head = fetch(&state, &get(5, port, "/no-content")).await.unwrap();
        assert_eq!(head.status, 204);
        assert!(!head.has_body);
        assert_eq!(state.table.len(), 0);
    }

    #[tokio::test]
    async fn cancel_ends_a_pending_request() {
        let (port, _seen) = server();
        let state = Arc::new(state(port));
        let pending = {
            let state = state.clone();
            tokio::spawn(async move { fetch(&state, &get(6, port, "/hang")).await })
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
        fetch(&state, &get(8, port, "/stream")).await.unwrap();
        assert_eq!(
            fetch(&state, &get(8, port, "/")).await.unwrap_err().code,
            "invalid_request"
        );
        assert_eq!(read_body(&state, 8).await.unwrap(), b"data 1");
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
        fetch(&state, &get(9, port, "/stream")).await.unwrap();
        fetch(&state, &get(10, port, "/stream")).await.unwrap();
        assert_eq!(state.table.len(), 2);
        state.table.clear();
        assert_eq!(state.table.len(), 0);
    }

    #[tokio::test]
    async fn one_client_serves_every_request() {
        let (port, _seen) = server();
        let state = state(port);
        let first: *const reqwest::Client = state.http.client();
        for id in 20..25 {
            fetch(&state, &get(id, port, "/")).await.unwrap();
            assert_eq!(read_all(&state, id).await, b"ok");
        }
        assert!(std::ptr::eq(first, state.http.client()));
        let clone = state.http.clone();
        assert!(std::ptr::eq(first, clone.client()));
    }
}
