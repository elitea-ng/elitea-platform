//! The cloud browser view's WebSocket, opened by the host for the webview
//! (ADR-0048 decision 7, ADR-0049 decision 6, #1592).
//!
//! **Why the host.** The view route (`GET /api/v2/browser_sessions/{id}/view`
//! on elitea-main) authenticates as every route does: a cookie or a bearer.
//! The desktop has a bearer only, and the page's `WebSocket` constructor
//! cannot send an `Authorization` header. So the host opens the socket, on
//! the connected deployment only, with the access token it holds (the token
//! never crosses the IPC), and relays it to the page.
//!
//! **Wire to the page.** One [`tauri::ipc::Channel`] per socket, in order
//! (the channel numbers its messages; the page puts them back in order):
//!
//! - `{"t":"open"}` once the upgrade succeeded;
//! - `{"t":"text","d":"…"}` for each text message (the view's JSON);
//! - each binary message (a frame's JPEG) as the RAW bytes of a channel
//!   message: never base64. Tauri hands a raw body of 1 KiB or more to the
//!   page through its IPC protocol as an `ArrayBuffer`, one copy;
//! - `{"t":"close","code":…,"reason":"…","status":…}` last. `status` is the
//!   HTTP status of an upgrade the route refused (404, 410, 429, 502, 503,
//!   401 after one token refresh, 426 when this build is too old), `null`
//!   when the socket opened or the deployment could not be reached. A page
//!   `WebSocket` cannot read that status; this one can, so the page needs
//!   no second request to learn why.
//!
//! The page sends with [`browser_view_send`] (text messages in its order,
//! then optionally the close) and nothing else: the host never sends
//! binary, and pings are answered here.
//!
//! **Bounds.** At most [`MAX_SOCKETS`] views at once (the route admits 4 per
//! person); an incoming message is at most [`MAX_MESSAGE`]; one send carries
//! at most [`MAX_SEND_BATCH`] messages of at most [`MAX_UP_MESSAGE`] each
//! (the route's own bound is 4 KiB). A page load closes every socket, as it
//! drops the page's fetches. The URL must be the view route of the connected
//! deployment ([`view_url`]): no other path, no query, no userinfo.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use bytes::Bytes;
use futures::{SinkExt as _, StreamExt as _};
use serde::Serialize;
use tauri::State;
use tauri::ipc::{Channel, InvokeResponseBody};
use tokio::sync::mpsc;
use tokio_tungstenite::WebSocketStream;
use tokio_tungstenite::tungstenite::Message;
use tokio_tungstenite::tungstenite::handshake::client::generate_key;
use tokio_tungstenite::tungstenite::handshake::derive_accept_key;
use tokio_tungstenite::tungstenite::protocol::frame::CloseFrame;
use tokio_tungstenite::tungstenite::protocol::frame::coding::CloseCode;
use tokio_tungstenite::tungstenite::protocol::{Role, WebSocketConfig};
use url::Url;

use crate::auth::{AuthService, RefreshResult};
use crate::commands::AppState;
use crate::error::HostError;
use crate::net::{HttpScope, WebFetch};
use crate::tokens::CLIENT_VERSION_HEADER;

/// Views open at once, across the app.
pub const MAX_SOCKETS: usize = 8;
/// The largest message the deployment may send (a frame's JPEG).
pub const MAX_MESSAGE: usize = 16 * 1024 * 1024;
/// The largest message the page may send; the route refuses past 4 KiB.
pub const MAX_UP_MESSAGE: usize = 8 * 1024;
/// Messages in one [`browser_view_send`].
pub const MAX_SEND_BATCH: usize = 256;
/// Sends queued for one socket before the page waits.
const UP_QUEUE: usize = 64;
/// The close code of a socket that ended without a close frame.
const ABNORMAL: u16 = 1006;
/// How long the deployment has to answer the upgrade. The route waits up
/// to 10 s for the browser service's first message before it answers.
const UPGRADE_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(30);
/// How long a close waits for the deployment's answer.
const CLOSE_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(5);
/// The route's path: `/…/browser_sessions/{session_id}/view`.
const ROUTE_SEGMENT: &str = "browser_sessions";

/// What the page rejects with.
#[derive(Debug, Serialize, PartialEq, Eq)]
pub struct ViewError {
    pub code: &'static str,
    pub message: String,
}

impl ViewError {
    fn new(code: &'static str, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
        }
    }
}

/// The `http(s)` URL of a page's `ws(s)` view URL, if it is the view route
/// of an origin `scope` allows.
///
/// # Errors
///
/// `url_not_allowed`: not `ws`/`wss`, userinfo, a query or fragment, another
/// path, or an origin the app is not connected to.
pub fn view_url(raw: &str, scope: &HttpScope) -> Result<Url, ViewError> {
    let refused = || {
        ViewError::new(
            "url_not_allowed",
            "not a browser view of the connected deployment",
        )
    };
    let mut url = Url::parse(raw).map_err(|_| refused())?;
    let scheme = match url.scheme() {
        "wss" => "https",
        "ws" => "http",
        _ => return Err(refused()),
    };
    url.set_scheme(scheme).map_err(|()| refused())?;
    if url.query().is_some() || url.fragment().is_some() || !scope.allows(&url) {
        return Err(refused());
    }
    let segments: Vec<&str> = url
        .path_segments()
        .map(Iterator::collect)
        .unwrap_or_default();
    let shaped = matches!(segments.as_slice(), [.., route, session, "view"] if *route == ROUTE_SEGMENT && !session.is_empty());
    if !shaped {
        return Err(refused());
    }
    Ok(url)
}

/// What the host tells the page about one socket.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Down {
    Open,
    Text(String),
    Binary(Bytes),
    Close {
        code: u16,
        reason: String,
        /// The HTTP status of a refused upgrade.
        status: Option<u16>,
    },
}

impl Down {
    /// The channel message: JSON, or the raw bytes of a binary message.
    fn body(self) -> InvokeResponseBody {
        match self {
            Self::Open => InvokeResponseBody::Json(r#"{"t":"open"}"#.to_owned()),
            Self::Text(text) => InvokeResponseBody::Json(
                serde_json::json!({ "t": "text", "d": text }).to_string(),
            ),
            Self::Binary(bytes) => InvokeResponseBody::Raw(Vec::from(bytes)),
            Self::Close {
                code,
                reason,
                status,
            } => InvokeResponseBody::Json(
                serde_json::json!({ "t": "close", "code": code, "reason": reason, "status": status })
                    .to_string(),
            ),
        }
    }
}

/// What the page asks of one socket.
#[derive(Debug)]
pub enum Up {
    Text(String),
    Close(u16, String),
}

/// The bearer the socket authenticates with.
#[async_trait::async_trait]
pub trait Bearer: Send + Sync {
    /// The access token now; `Ok(None)` when signed out.
    async fn token(&self) -> Result<Option<String>, HostError>;
    /// `failed` was refused with a 401: a new one, unless a refresh already made it.
    async fn refresh_after(&self, failed: &str) -> Result<RefreshResult, HostError>;
}

#[async_trait::async_trait]
impl Bearer for AuthService {
    async fn token(&self) -> Result<Option<String>, HostError> {
        Ok(self.access_token().await?.map(|token| token.token))
    }

    async fn refresh_after(&self, failed: &str) -> Result<RefreshResult, HostError> {
        self.refresh_from(failed).await
    }
}

/// How one upgrade attempt ended.
enum Attempt {
    Open(Box<WebSocketStream<reqwest::Upgraded>>),
    /// The route answered `status` instead of switching protocols.
    Refused(u16),
    /// The deployment could not be reached, or the handshake was wrong.
    Failed,
}

async fn attempt(
    client: &reqwest::Client,
    url: &Url,
    token: Option<&str>,
    client_version: &str,
) -> Attempt {
    let key = generate_key();
    let mut request = client
        .get(url.clone())
        .header(reqwest::header::CONNECTION, "Upgrade")
        .header(reqwest::header::UPGRADE, "websocket")
        .header(reqwest::header::SEC_WEBSOCKET_VERSION, "13")
        .header(reqwest::header::SEC_WEBSOCKET_KEY, &key)
        .header(CLIENT_VERSION_HEADER, client_version);
    if let Some(token) = token {
        request = request.bearer_auth(token);
    }
    let Ok(Ok(response)) = tokio::time::timeout(UPGRADE_TIMEOUT, request.send()).await else {
        return Attempt::Failed;
    };
    let status = response.status();
    if status != reqwest::StatusCode::SWITCHING_PROTOCOLS {
        return Attempt::Refused(status.as_u16());
    }
    let accepted = response
        .headers()
        .get(reqwest::header::SEC_WEBSOCKET_ACCEPT)
        .and_then(|value| value.to_str().ok())
        == Some(derive_accept_key(key.as_bytes()).as_str());
    if !accepted {
        return Attempt::Failed;
    }
    let Ok(upgraded) = response.upgrade().await else {
        return Attempt::Failed;
    };
    let config = WebSocketConfig::default()
        .max_message_size(Some(MAX_MESSAGE))
        .max_frame_size(Some(MAX_MESSAGE));
    Attempt::Open(Box::new(
        WebSocketStream::from_raw_socket(upgraded, Role::Client, Some(config)).await,
    ))
}

/// Open the view: the token, the upgrade, one refresh and retry on a 401.
async fn connect(
    client: &reqwest::Client,
    bearer: &dyn Bearer,
    url: &Url,
    client_version: &str,
) -> Result<Box<WebSocketStream<reqwest::Upgraded>>, Option<u16>> {
    let token = match bearer.token().await {
        Ok(Some(token)) => token,
        Ok(None) => return Err(Some(401)),
        Err(HostError::UpgradeRequired) => return Err(Some(426)),
        Err(_) => return Err(None),
    };
    match attempt(client, url, Some(&token), client_version).await {
        Attempt::Open(socket) => return Ok(socket),
        Attempt::Refused(401) => {}
        Attempt::Refused(status) => return Err(Some(status)),
        Attempt::Failed => return Err(None),
    }
    match bearer.refresh_after(&token).await {
        Ok(RefreshResult::Refreshed) => {}
        Ok(RefreshResult::UpgradeRequired) => return Err(Some(426)),
        Ok(RefreshResult::Ended) => return Err(Some(401)),
        Ok(RefreshResult::Unavailable) | Err(_) => return Err(None),
    }
    let token = match bearer.token().await {
        Ok(Some(token)) => token,
        Ok(None) => return Err(Some(401)),
        Err(_) => return Err(None),
    };
    match attempt(client, url, Some(&token), client_version).await {
        Attempt::Open(socket) => Ok(socket),
        Attempt::Refused(status) => Err(Some(status)),
        Attempt::Failed => Err(None),
    }
}

/// One socket, from the upgrade to its close. `down` answers `false` once
/// nobody listens (the page went away), which ends the socket.
pub async fn run(
    client: &reqwest::Client,
    bearer: &dyn Bearer,
    url: Url,
    client_version: &str,
    mut up: mpsc::Receiver<Up>,
    mut down: impl FnMut(Down) -> bool,
) {
    let socket = tokio::select! {
        connected = connect(client, bearer, &url, client_version) => connected,
        // The page closed it before it opened.
        () = closed_before_open(&mut up) => {
            down(Down::Close { code: ABNORMAL, reason: String::new(), status: None });
            return;
        }
    };
    let mut socket = match socket {
        Ok(socket) => socket,
        Err(status) => {
            down(Down::Close {
                code: ABNORMAL,
                reason: String::new(),
                status,
            });
            return;
        }
    };
    if !down(Down::Open) {
        let _ = socket.send(Message::Close(None)).await;
        return;
    }
    let mut closing = false;
    // Armed by a close: a deployment that never answers it does not hold the socket.
    let close_deadline = tokio::time::sleep(std::time::Duration::MAX);
    tokio::pin!(close_deadline);
    let (code, reason) = loop {
        tokio::select! {
            () = &mut close_deadline, if closing => break (ABNORMAL, String::new()),
            message = socket.next() => match message {
                Some(Ok(Message::Text(text))) => {
                    if !down(Down::Text(text.as_str().to_owned())) {
                        let _ = socket.send(Message::Close(None)).await;
                        return;
                    }
                }
                Some(Ok(Message::Binary(bytes))) => {
                    if !down(Down::Binary(bytes)) {
                        let _ = socket.send(Message::Close(None)).await;
                        return;
                    }
                }
                Some(Ok(Message::Close(frame))) => {
                    break frame.map_or((1005, String::new()), |f| (u16::from(f.code), f.reason.as_str().to_owned()));
                }
                // Pings are answered by the library; pongs and raw frames carry nothing.
                Some(Ok(_)) => {}
                Some(Err(_)) | None => break (ABNORMAL, String::new()),
            },
            sent = up.recv(), if !closing => match sent {
                Some(Up::Text(text)) => {
                    if socket.send(Message::text(text)).await.is_err() {
                        break (ABNORMAL, String::new());
                    }
                }
                Some(Up::Close(code, reason)) => {
                    closing = true;
                    close_deadline.as_mut().reset(tokio::time::Instant::now() + CLOSE_TIMEOUT);
                    let frame = CloseFrame { code: CloseCode::from(code), reason: reason.into() };
                    if socket.send(Message::Close(Some(frame))).await.is_err() {
                        break (ABNORMAL, String::new());
                    }
                }
                // The page's handle is gone (a page load): close it.
                None => {
                    closing = true;
                    close_deadline.as_mut().reset(tokio::time::Instant::now() + CLOSE_TIMEOUT);
                    let _ = socket.send(Message::Close(None)).await;
                }
            },
        }
    };
    down(Down::Close {
        code,
        reason,
        status: None,
    });
}

/// Resolves when the page closes the socket (or drops it) before it opened.
async fn closed_before_open(up: &mut mpsc::Receiver<Up>) {
    loop {
        match up.recv().await {
            Some(Up::Text(_)) => {}
            Some(Up::Close(..)) | None => return,
        }
    }
}

// ---- the table and the commands ---------------------------------------------

/// The open sockets, by the page's id.
#[derive(Default)]
pub struct ViewSockets {
    open: Mutex<HashMap<u64, mpsc::Sender<Up>>>,
}

impl ViewSockets {
    fn insert(&self, id: u64, sender: mpsc::Sender<Up>) -> Result<(), ViewError> {
        let mut open = crate::control_state::lock(&self.open);
        open.retain(|_, sender| !sender.is_closed());
        if open.contains_key(&id) {
            return Err(ViewError::new(
                "invalid_request",
                "that view is already open",
            ));
        }
        if open.len() >= MAX_SOCKETS {
            return Err(ViewError::new(
                "too_many_views",
                "too many browser views are open",
            ));
        }
        open.insert(id, sender);
        Ok(())
    }

    fn sender(&self, id: u64) -> Option<mpsc::Sender<Up>> {
        crate::control_state::lock(&self.open).get(&id).cloned()
    }

    fn remove(&self, id: u64) {
        crate::control_state::lock(&self.open).remove(&id);
    }

    /// A page load: nobody will read the old page's sockets. Dropping each
    /// sender closes its socket.
    pub fn clear(&self) {
        crate::control_state::lock(&self.open).clear();
    }

    #[cfg(test)]
    fn len(&self) -> usize {
        crate::control_state::lock(&self.open).len()
    }
}

/// `browser_view_open`: open the view at `url` (`ws(s)://`); its events
/// arrive on `events`. Resolves once the socket is under way, not open.
///
/// # Errors
///
/// `url_not_allowed`, `invalid_request` (the id is in use), `too_many_views`.
#[tauri::command]
pub fn browser_view_open(
    id: u64,
    url: String,
    events: Channel<InvokeResponseBody>,
    sockets: State<'_, Arc<ViewSockets>>,
    web: State<'_, WebFetch>,
    app: State<'_, AppState>,
) -> Result<(), ViewError> {
    let url = view_url(&url, &web.scope)?;
    let (sender, receiver) = mpsc::channel(UP_QUEUE);
    sockets.insert(id, sender)?;
    let sockets = Arc::clone(&sockets);
    let http = web.http.clone();
    let auth = Arc::clone(&app.auth);
    tauri::async_runtime::spawn(async move {
        let client = http.upgrade_client().await;
        run(
            client,
            auth.as_ref(),
            url,
            http.client_version(),
            receiver,
            |event| events.send(event.body()).is_ok(),
        )
        .await;
        sockets.remove(id);
    });
    Ok(())
}

/// `browser_view_send`: the page's text messages, in order, then its close.
///
/// # Errors
///
/// `unknown_view` (closed, or never opened), `invalid_request` past the bounds.
#[tauri::command]
pub async fn browser_view_send(
    id: u64,
    messages: Vec<String>,
    close: Option<(u16, String)>,
    sockets: State<'_, Arc<ViewSockets>>,
) -> Result<(), ViewError> {
    if messages.len() > MAX_SEND_BATCH || messages.iter().any(|m| m.len() > MAX_UP_MESSAGE) {
        return Err(ViewError::new(
            "invalid_request",
            "the message is too large",
        ));
    }
    let sender = sockets
        .sender(id)
        .ok_or_else(|| ViewError::new("unknown_view", "no such browser view"))?;
    let gone = || ViewError::new("unknown_view", "the browser view closed");
    for message in messages {
        sender.send(Up::Text(message)).await.map_err(|_| gone())?;
    }
    if let Some((code, reason)) = close {
        // A page may close with 1000 or 3000–4999 (the WebSocket API's rule).
        let code = if code == 1000 || (3000..=4999).contains(&code) {
            code
        } else {
            1000
        };
        let reason: String = reason.chars().take(123).collect();
        sender
            .send(Up::Close(code, reason))
            .await
            .map_err(|_| gone())?;
    }
    Ok(())
}

#[cfg(test)]
#[path = "view_socket_tests.rs"]
mod tests;
