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
    )
    .expect("the view route");
    assert_eq!(ok.scheme(), "https");
    assert_eq!(
        view_url(
            "ws://elitea.example.com/x/browser_sessions/s/view",
            &scope_for("http://elitea.example.com")
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
            view_url(&refused, &scope).map(|_| ()).unwrap_err().code,
            "url_not_allowed",
            "{refused}"
        );
    }
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
    let (origin, _) = server(
        |_| None,
        |mut socket| async move {
            socket.send(Message::text("{}")).await.unwrap();
            while socket.next().await.is_some() {}
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
    )
    .unwrap();
    let client = reqwest::Client::builder().http1_only().build().unwrap();
    let calls = AtomicUsize::new(0);
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
