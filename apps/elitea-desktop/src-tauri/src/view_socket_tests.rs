//! The view socket against a loopback WebSocket server: the upgrade with
//! the bearer, the relay both ways, a refused upgrade's status, the one
//! refresh on a 401, and the page's bounds.

use std::sync::Mutex as StdMutex;
use std::sync::atomic::{AtomicUsize, Ordering};

use tokio::net::TcpListener;
use tokio_tungstenite::tungstenite::handshake::server::{ErrorResponse, Request, Response};
use tokio_tungstenite::tungstenite::http::StatusCode;

use super::*;

const SESSION: &str = "elitea-br-0123456789abcdef01234567";

fn scope_for(origin: &str) -> HttpScope {
    let scope = HttpScope::default();
    scope.grant(origin).expect("grant");
    scope
}

#[test]
fn only_the_view_route_of_the_connected_deployment_is_opened() {
    let scope = scope_for("https://elitea.example.com");
    let ok = view_url(
        &format!("wss://elitea.example.com/api/v2/browser_sessions/{SESSION}/view"),
        &scope,
        Some("https://elitea.example.com"),
    )
    .expect("the view route");
    assert_eq!(ok.scheme(), "https");
    assert_eq!(
        view_url(
            "ws://elitea.example.com/x/browser_sessions/s/view",
            &scope_for("http://elitea.example.com"),
            Some("http://elitea.example.com")
        )
        .expect("plain http deployment")
        .scheme(),
        "http"
    );
    for refused in [
        format!("wss://other.example.com/api/v2/browser_sessions/{SESSION}/view"),
        format!("https://elitea.example.com/api/v2/browser_sessions/{SESSION}/view"),
        format!("wss://user:pw@elitea.example.com/api/v2/browser_sessions/{SESSION}/view"),
        format!("wss://elitea.example.com/api/v2/browser_sessions/{SESSION}/view?x=1"),
        format!("wss://elitea.example.com/api/v2/browser_sessions/{SESSION}/view#f"),
        "wss://elitea.example.com/api/v2/conversations/1".to_owned(),
        "wss://elitea.example.com/api/v2/browser_sessions//view".to_owned(),
        "wss://elitea.example.com:8443/api/v2/browser_sessions/s/view".to_owned(),
        "not a url".to_owned(),
    ] {
        assert_eq!(
            view_url(&refused, &scope, Some("https://elitea.example.com"))
                .map(|_| ())
                .unwrap_err()
                .code,
            "url_not_allowed",
            "{refused}"
        );
    }
}

#[test]
fn a_deployment_granted_earlier_is_not_the_connected_one() {
    // The scope keeps every deployment granted since launch; the bearer is
    // the current one's, so only the current deployment's view opens.
    let scope = scope_for("https://old.example.com");
    scope.grant("https://new.example.com").unwrap();
    let old = format!("wss://old.example.com/api/v2/browser_sessions/{SESSION}/view");
    let new = format!("wss://new.example.com/api/v2/browser_sessions/{SESSION}/view");
    assert_eq!(
        view_url(&old, &scope, Some("https://new.example.com"))
            .unwrap_err()
            .code,
        "url_not_allowed"
    );
    view_url(&new, &scope, Some("https://new.example.com")).expect("the connected deployment");
    // The port is part of the origin; not connected at all opens nothing.
    assert!(view_url(&new, &scope, Some("https://new.example.com:8443")).is_err());
    assert!(view_url(&new, &scope, None).is_err());
}

#[test]
fn a_refused_open_closes_on_the_channel_with_a_status() {
    let close = |status| Down::Close {
        code: 1006,
        reason: String::new(),
        status: Some(status),
    };
    let scope = scope_for("https://elitea.example.com");
    let current = Some("https://elitea.example.com");
    let sockets = ViewSockets::default();
    let url = format!("wss://elitea.example.com/api/v2/browser_sessions/{SESSION}/view");

    // Another deployment's view: forbidden, never retried.
    let error = admit(
        &sockets,
        1,
        &format!("wss://other.example.com/api/v2/browser_sessions/{SESSION}/view"),
        &scope,
        current,
    )
    .unwrap_err();
    assert_eq!(refusal(&error), Some(close(403)));

    // One view too many: busy.
    let mut keep = Vec::new();
    for id in 0..MAX_SOCKETS as u64 {
        keep.push(admit(&sockets, id, &url, &scope, current).unwrap().1);
    }
    let error = admit(&sockets, 99, &url, &scope, current).unwrap_err();
    assert_eq!(refusal(&error), Some(close(429)));

    // An id in use is the page's bug: the call rejects.
    let error = admit(&sockets, 0, &url, &scope, current).unwrap_err();
    assert_eq!(error.code, "invalid_request");
    assert_eq!(refusal(&error), None);
}

#[test]
fn a_close_reason_is_cut_to_123_bytes_at_a_character_boundary() {
    assert_eq!(clip_reason("bye".to_owned()), "bye");
    // 62 two-byte characters are 124 bytes: the last one does not fit.
    let reason = clip_reason("é".repeat(62));
    assert_eq!(reason.len(), 122);
    assert_eq!(reason, "é".repeat(61));
    assert_eq!(clip_reason("a".repeat(200)).len(), 123);
}

#[test]
fn a_binary_message_reaches_the_page_as_raw_bytes() {
    let jpeg = Bytes::from_static(&[0xff, 0xd8, 0xff, 0xd9]);
    assert!(
        matches!(Down::Binary(jpeg).body(), InvokeResponseBody::Raw(bytes) if bytes == [0xff, 0xd8, 0xff, 0xd9])
    );
    let InvokeResponseBody::Json(close) = (Down::Close {
        code: 1006,
        reason: String::new(),
        status: Some(404),
    })
    .body() else {
        panic!("json");
    };
    assert_eq!(
        serde_json::from_str::<serde_json::Value>(&close).unwrap(),
        serde_json::json!({"t": "close", "code": 1006, "reason": "", "status": 404})
    );
    let InvokeResponseBody::Json(text) = Down::Text("{\"type\":\"hello\"}".to_owned()).body()
    else {
        panic!("json");
    };
    assert_eq!(
        serde_json::from_str::<serde_json::Value>(&text).unwrap(),
        serde_json::json!({"t": "text", "d": "{\"type\":\"hello\"}"})
    );
}

/// The token a [`FakeBearer`] reads as `Err(HostError::UpgradeRequired)`.
const UPGRADE_REQUIRED: &str = "!upgrade-required";

/// Hands out `tokens` in turn; counts the refreshes.
struct FakeBearer {
    tokens: StdMutex<Vec<&'static str>>,
    refreshes: AtomicUsize,
    refresh: RefreshResult,
}

impl FakeBearer {
    fn new(tokens: &[&'static str], refresh: RefreshResult) -> Self {
        Self {
            tokens: StdMutex::new(tokens.iter().rev().copied().collect()),
            refreshes: AtomicUsize::new(0),
            refresh,
        }
    }
}

#[async_trait::async_trait]
impl Bearer for FakeBearer {
    async fn token(&self) -> Result<Option<String>, HostError> {
        let mut tokens = self.tokens.lock().unwrap();
        let token = if tokens.len() > 1 {
            tokens.pop()
        } else {
            tokens.last().copied()
        };
        // A refresh this build is too old for, as `access_token` reports it.
        if token == Some(UPGRADE_REQUIRED) {
            return Err(HostError::UpgradeRequired);
        }
        Ok(token.map(str::to_owned))
    }

    async fn refresh_after(&self, _failed: &str) -> Result<RefreshResult, HostError> {
        self.refreshes.fetch_add(1, Ordering::SeqCst);
        Ok(self.refresh)
    }
}

/// What the server saw of each upgrade.
#[derive(Debug, Clone)]
struct Seen {
    authorization: Option<String>,
    client_version: Option<String>,
    path: String,
}

/// A loopback server: `accept` decides each upgrade from its bearer (an
/// HTTP status refuses it); an accepted socket runs `serve`.
async fn server<S, F>(
    accept: fn(Option<&str>) -> Option<u16>,
    serve: S,
) -> (String, Arc<StdMutex<Vec<Seen>>>)
where
    S: Fn(WebSocketStream<tokio::net::TcpStream>) -> F + Send + Sync + 'static,
    F: std::future::Future<Output = ()> + Send,
{
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("http://{}", listener.local_addr().unwrap());
    let seen = Arc::new(StdMutex::new(Vec::new()));
    let log = Arc::clone(&seen);
    let serve = Arc::new(serve);
    tokio::spawn(async move {
        while let Ok((stream, _)) = listener.accept().await {
            let log = Arc::clone(&log);
            let serve = Arc::clone(&serve);
            tokio::spawn(async move {
                // The library's callback signature: its error is a whole HTTP response.
                #[allow(clippy::result_large_err)]
                let callback =
                    |request: &Request, response: Response| -> Result<Response, ErrorResponse> {
                        let header = |name: &str| {
                            request
                                .headers()
                                .get(name)
                                .and_then(|v| v.to_str().ok())
                                .map(str::to_owned)
                        };
                        let authorization = header("authorization");
                        log.lock().unwrap().push(Seen {
                            authorization: authorization.clone(),
                            client_version: header("x-client-version"),
                            path: request.uri().path().to_owned(),
                        });
                        let bearer = authorization
                            .as_deref()
                            .and_then(|value| value.strip_prefix("Bearer "));
                        match accept(bearer) {
                            None => Ok(response),
                            Some(status) => {
                                let mut refusal = ErrorResponse::new(Some("refused".to_owned()));
                                *refusal.status_mut() = StatusCode::from_u16(status).unwrap();
                                Err(refusal)
                            }
                        }
                    };
                if let Ok(socket) = tokio_tungstenite::accept_hdr_async(stream, callback).await {
                    serve(socket).await;
                }
            });
        }
    });
    (origin, seen)
}

/// Runs a socket to its end; answers what the page heard.
async fn open(
    origin: &str,
    bearer: &FakeBearer,
    page: impl FnOnce(mpsc::Sender<Up>) + Send,
) -> Vec<Down> {
    let url = view_url(
        &format!(
            "{}/api/v2/browser_sessions/{SESSION}/view",
            origin.replacen("http", "ws", 1)
        ),
        &scope_for(origin),
        Some(origin),
    )
    .expect("view url");
    let (sender, receiver) = mpsc::channel(UP_QUEUE);
    // The page acts once the socket is open, as a page can.
    let mut page = Some((page, sender));
    let heard = StdMutex::new(Vec::new());
    let client = reqwest::Client::builder().http1_only().build().unwrap();
    tokio::time::timeout(
        std::time::Duration::from_secs(10),
        run(&client, bearer, url, "0.1.0", receiver, |event| {
            let opened = event == Down::Open;
            heard.lock().unwrap().push(event);
            if opened && let Some((page, sender)) = page.take() {
                page(sender);
            }
            true
        }),
    )
    .await
    .expect("the socket ends");
    heard.into_inner().unwrap()
}

#[tokio::test]
async fn the_view_opens_with_the_bearer_and_relays_both_ways() {
    let (origin, seen) = server(
        |bearer| (bearer != Some("good")).then_some(401),
        |mut socket| async move {
            socket
                .send(Message::text(r#"{"type":"hello"}"#))
                .await
                .unwrap();
            socket
                .send(Message::binary(vec![0xff, 0xd8, 0xff, 0xd9]))
                .await
                .unwrap();
            // Echo the page's first message, then close with the route's code.
            if let Some(Ok(Message::Text(text))) = socket.next().await {
                socket
                    .send(Message::text(format!("echo {}", text.as_str())))
                    .await
                    .unwrap();
            }
            let frame = CloseFrame {
                code: CloseCode::from(4403),
                reason: "access_revoked".into(),
            };
            let _ = socket.close(Some(frame)).await;
        },
    )
    .await;
    let bearer = FakeBearer::new(&["good"], RefreshResult::Refreshed);
    let heard = open(&origin, &bearer, |up| {
        up.try_send(Up::Text(r#"{"type":"ack","seq":1}"#.to_owned()))
            .unwrap();
        std::mem::forget(up);
    })
    .await;
    assert_eq!(
        heard,
        [
            Down::Open,
            Down::Text(r#"{"type":"hello"}"#.to_owned()),
            Down::Binary(Bytes::from_static(&[0xff, 0xd8, 0xff, 0xd9])),
            Down::Text(r#"echo {"type":"ack","seq":1}"#.to_owned()),
            Down::Close {
                code: 4403,
                reason: "access_revoked".to_owned(),
                status: None
            },
        ]
    );
    let seen = seen.lock().unwrap();
    assert_eq!(seen.len(), 1);
    assert_eq!(seen[0].authorization.as_deref(), Some("Bearer good"));
    assert_eq!(seen[0].client_version.as_deref(), Some("0.1.0"));
    assert_eq!(
        seen[0].path,
        format!("/api/v2/browser_sessions/{SESSION}/view")
    );
    assert_eq!(bearer.refreshes.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn a_refused_upgrade_reports_its_status() {
    for status in [404_u16, 410, 429, 502, 503, 426] {
        let accept: fn(Option<&str>) -> Option<u16> = match status {
            404 => |_| Some(404),
            410 => |_| Some(410),
            429 => |_| Some(429),
            502 => |_| Some(502),
            503 => |_| Some(503),
            _ => |_| Some(426),
        };
        let (origin, _) = server(accept, |_| async {}).await;
        let bearer = FakeBearer::new(&["good"], RefreshResult::Refreshed);
        let heard = open(&origin, &bearer, std::mem::forget).await;
        assert_eq!(
            heard,
            [Down::Close {
                code: 1006,
                reason: String::new(),
                status: Some(status)
            }],
            "{status}"
        );
    }
}

#[tokio::test]
async fn a_401_is_answered_by_one_refresh_and_one_retry() {
    let (origin, seen) = server(
        |bearer| (bearer != Some("fresh")).then_some(401),
        |mut socket| async move {
            let _ = socket.close(None).await;
        },
    )
    .await;
    let bearer = FakeBearer::new(&["stale", "fresh"], RefreshResult::Refreshed);
    let heard = open(&origin, &bearer, std::mem::forget).await;
    assert_eq!(heard.first(), Some(&Down::Open));
    assert_eq!(bearer.refreshes.load(Ordering::SeqCst), 1);
    let seen: Vec<_> = seen
        .lock()
        .unwrap()
        .iter()
        .map(|s| s.authorization.clone())
        .collect();
    assert_eq!(
        seen,
        [
            Some("Bearer stale".to_owned()),
            Some("Bearer fresh".to_owned())
        ]
    );

    // Refused again: signed out, and not refreshed twice.
    let (origin, seen) = server(|_| Some(401), |_| async {}).await;
    let bearer = FakeBearer::new(&["stale", "fresh"], RefreshResult::Refreshed);
    let heard = open(&origin, &bearer, std::mem::forget).await;
    assert_eq!(
        heard,
        [Down::Close {
            code: 1006,
            reason: String::new(),
            status: Some(401)
        }]
    );
    assert_eq!(bearer.refreshes.load(Ordering::SeqCst), 1);
    assert_eq!(seen.lock().unwrap().len(), 2);

    // A refresh the server refused: signed out, no retry.
    let (origin, seen) = server(|_| Some(401), |_| async {}).await;
    let bearer = FakeBearer::new(&["stale"], RefreshResult::Ended);
    let heard = open(&origin, &bearer, std::mem::forget).await;
    assert_eq!(
        heard,
        [Down::Close {
            code: 1006,
            reason: String::new(),
            status: Some(401)
        }]
    );
    assert_eq!(seen.lock().unwrap().len(), 1);

    // Too old a build to refresh: 426.
    let (origin, _) = server(|_| Some(401), |_| async {}).await;
    let bearer = FakeBearer::new(&["stale"], RefreshResult::UpgradeRequired);
    let heard = open(&origin, &bearer, std::mem::forget).await;
    assert_eq!(
        heard,
        [Down::Close {
            code: 1006,
            reason: String::new(),
            status: Some(426)
        }]
    );

    // Refreshed, but reading the new token says this build is too old: 426 too.
    let (origin, seen) = server(|_| Some(401), |_| async {}).await;
    let bearer = FakeBearer::new(&["stale", UPGRADE_REQUIRED], RefreshResult::Refreshed);
    let heard = open(&origin, &bearer, std::mem::forget).await;
    assert_eq!(
        heard,
        [Down::Close {
            code: 1006,
            reason: String::new(),
            status: Some(426)
        }]
    );
    assert_eq!(seen.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn an_unreachable_deployment_has_no_status() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("http://{}", listener.local_addr().unwrap());
    drop(listener);
    let bearer = FakeBearer::new(&["good"], RefreshResult::Refreshed);
    let heard = open(&origin, &bearer, std::mem::forget).await;
    assert_eq!(
        heard,
        [Down::Close {
            code: 1006,
            reason: String::new(),
            status: None
        }]
    );
}

#[tokio::test]
async fn the_page_closes_with_its_code_and_a_dropped_handle_closes_too() {
    let (origin, _) = server(
        |_| None,
        |mut socket| async move {
            // Answer the page's close as a server does: echo its frame.
            while let Some(Ok(message)) = socket.next().await {
                if let Message::Close(_) = message {
                    // Reading on flushes the close reply tungstenite queued.
                    while socket.next().await.is_some() {}
                    return;
                }
            }
        },
    )
    .await;
    let bearer = FakeBearer::new(&["good"], RefreshResult::Refreshed);
    let heard = open(&origin, &bearer, |up| {
        up.try_send(Up::Close(1000, "bye".to_owned())).unwrap();
        std::mem::forget(up);
    })
    .await;
    // Opened, then closed with the page's code.
    assert_eq!(heard.first(), Some(&Down::Open));
    assert!(
        matches!(
            heard.last(),
            Some(Down::Close {
                code: 1000,
                status: None,
                ..
            })
        ),
        "{heard:?}"
    );

    // Dropped once open (a page load): the host sends a close, the server answers it.
    let (origin, _) = server(
        |_| None,
        |mut socket| async move {
            socket.send(Message::text("{}")).await.unwrap();
            while let Some(Ok(message)) = socket.next().await {
                if let Message::Close(_) = message {
                    // Reading on flushes the close reply tungstenite queued.
                    while socket.next().await.is_some() {}
                    return;
                }
            }
        },
    )
    .await;
    let (sender, receiver) = mpsc::channel(UP_QUEUE);
    let url = view_url(
        &format!(
            "{}/api/v2/browser_sessions/{SESSION}/view",
            origin.replacen("http", "ws", 1)
        ),
        &scope_for(&origin),
        Some(&origin),
    )
    .unwrap();
    let client = reqwest::Client::builder().http1_only().build().unwrap();
    let heard = StdMutex::new(Vec::new());
    let mut sender = Some(sender);
    tokio::time::timeout(
        std::time::Duration::from_secs(10),
        run(&client, &bearer, url, "0.1.0", receiver, |event| {
            if matches!(event, Down::Text(_)) {
                sender.take();
            }
            heard.lock().unwrap().push(event);
            true
        }),
    )
    .await
    .expect("ends");
    let heard = heard.into_inner().unwrap();
    assert!(
        matches!(heard.last(), Some(Down::Close { status: None, .. })),
        "{heard:?}"
    );
}

#[tokio::test]
async fn a_page_that_stops_listening_ends_the_socket() {
    const REPLY_DELAY: std::time::Duration = std::time::Duration::from_millis(300);
    let closed = Arc::new(AtomicUsize::new(0));
    let saw = Arc::clone(&closed);
    let (origin, _) = server(
        |_| None,
        move |mut socket| {
            let saw = Arc::clone(&saw);
            async move {
                socket.send(Message::text("{}")).await.unwrap();
                while let Some(Ok(message)) = socket.next().await {
                    if let Message::Close(_) = message {
                        saw.fetch_add(1, Ordering::SeqCst);
                        // A slow deployment: its reply (queued by the library) goes out late.
                        tokio::time::sleep(REPLY_DELAY).await;
                        let _ = socket.flush().await;
                        while socket.next().await.is_some() {}
                        return;
                    }
                }
            }
        },
    )
    .await;
    let bearer = FakeBearer::new(&["good"], RefreshResult::Refreshed);
    let (_sender, receiver) = mpsc::channel(UP_QUEUE);
    let url = view_url(
        &format!(
            "{}/api/v2/browser_sessions/{SESSION}/view",
            origin.replacen("http", "ws", 1)
        ),
        &scope_for(&origin),
        Some(&origin),
    )
    .unwrap();
    let client = reqwest::Client::builder().http1_only().build().unwrap();
    let calls = AtomicUsize::new(0);
    let started = std::time::Instant::now();
    tokio::time::timeout(
        std::time::Duration::from_secs(10),
        run(&client, &bearer, url, "0.1.0", receiver, |event| {
            calls.fetch_add(1, Ordering::SeqCst);
            matches!(event, Down::Open)
        }),
    )
    .await
    .expect("ends");
    // Open, then the text nobody took: no close event after that.
    assert_eq!(calls.load(Ordering::SeqCst), 2);
    // The host closed and waited for the deployment's reply (the close
    // handshake), not dropped the socket on sending its frame; and within
    // its bound.
    let took = started.elapsed();
    assert_eq!(closed.load(Ordering::SeqCst), 1);
    assert!(took >= REPLY_DELAY, "{took:?}");
    assert!(took < DROP_CLOSE_TIMEOUT, "{took:?}");
}

#[tokio::test]
async fn a_deployment_that_never_answers_the_close_is_dropped_after_the_bound() {
    let (origin, _) = server(
        |_| None,
        |mut socket| async move {
            socket.send(Message::text("{}")).await.unwrap();
            // Never reads again: the host's close goes unanswered.
            tokio::time::sleep(std::time::Duration::from_secs(30)).await;
            drop(socket);
        },
    )
    .await;
    let bearer = FakeBearer::new(&["good"], RefreshResult::Refreshed);
    let (_sender, receiver) = mpsc::channel(UP_QUEUE);
    let url = view_url(
        &format!(
            "{}/api/v2/browser_sessions/{SESSION}/view",
            origin.replacen("http", "ws", 1)
        ),
        &scope_for(&origin),
        Some(&origin),
    )
    .unwrap();
    let client = reqwest::Client::builder().http1_only().build().unwrap();
    let started = std::time::Instant::now();
    tokio::time::timeout(
        std::time::Duration::from_secs(10),
        run(&client, &bearer, url, "0.1.0", receiver, |event| {
            matches!(event, Down::Open)
        }),
    )
    .await
    .expect("ends");
    let took = started.elapsed();
    assert!(took >= DROP_CLOSE_TIMEOUT, "{took:?}");
    assert!(took < DROP_CLOSE_TIMEOUT * 2, "{took:?}");
}

#[test]
fn the_table_bounds_open_views_and_forgets_closed_ones() {
    let sockets = ViewSockets::default();
    let mut keep = Vec::new();
    for id in 0..MAX_SOCKETS as u64 {
        let (sender, receiver) = mpsc::channel(1);
        sockets.insert(id, sender).unwrap();
        keep.push(receiver);
    }
    let (sender, _receiver) = mpsc::channel(1);
    assert_eq!(
        sockets.insert(99, sender.clone()).unwrap_err().code,
        "too_many_views"
    );
    // A closed socket (its task ended) makes room.
    drop(keep.pop());
    sockets.insert(99, sender.clone()).unwrap();
    assert_eq!(
        sockets.insert(99, sender).unwrap_err().code,
        "invalid_request"
    );
    assert!(sockets.sender(0).is_some());
    sockets.remove(0);
    assert!(sockets.sender(0).is_none());
    sockets.clear();
    assert_eq!(sockets.len(), 0);
}
