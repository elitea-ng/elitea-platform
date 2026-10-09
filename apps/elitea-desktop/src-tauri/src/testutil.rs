//! A tiny in-process HTTP server for tests: one closure answers every request.
//! No external services, no network beyond loopback.

use std::collections::HashMap;
use std::net::Ipv4Addr;
use std::sync::{Arc, Mutex};

use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
use tokio::net::TcpListener;

#[derive(Debug, Clone)]
pub struct Req {
    pub method: String,
    pub path: String,
    /// Header names lowercased.
    pub headers: HashMap<String, String>,
    pub body: String,
}

impl Req {
    /// A form-encoded body as key/value pairs.
    pub fn form(&self) -> HashMap<String, String> {
        url::form_urlencoded::parse(self.body.as_bytes())
            .into_owned()
            .collect()
    }
}

pub struct Res {
    pub status: u16,
    pub headers: Vec<(&'static str, String)>,
    pub body: String,
}

impl Res {
    pub fn json(status: u16, body: &serde_json::Value) -> Self {
        Self {
            status,
            headers: vec![("Content-Type", "application/json".into())],
            body: body.to_string(),
        }
    }
}

pub struct MockServer {
    pub origin: String,
    pub seen: Arc<Mutex<Vec<Req>>>,
}

impl MockServer {
    pub fn seen(&self) -> Vec<Req> {
        self.seen.lock().expect("lock").clone()
    }
}

pub async fn serve<F>(handler: F) -> MockServer
where
    F: Fn(&Req) -> Res + Send + Sync + 'static,
{
    serve_dropping(None, handler).await
}

/// Like [`serve`], but the first `n` requests to `path` are read, logged and
/// then dropped without an answer: the "request arrived, response lost" case.
pub async fn serve_dropping<F>(drop_first: Option<(&'static str, usize)>, handler: F) -> MockServer
where
    F: Fn(&Req) -> Res + Send + Sync + 'static,
{
    let dropped = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let listener = TcpListener::bind((Ipv4Addr::LOCALHOST, 0))
        .await
        .expect("bind");
    let port = listener.local_addr().expect("addr").port();
    let seen = Arc::new(Mutex::new(Vec::new()));
    let (handler, log) = (Arc::new(handler), seen.clone());
    tokio::spawn(async move {
        loop {
            let Ok((mut stream, _)) = listener.accept().await else {
                return;
            };
            let (handler, log, dropped) = (handler.clone(), log.clone(), dropped.clone());
            tokio::spawn(async move {
                let Some(req) = read_request(&mut stream).await else {
                    return;
                };
                let res = handler(&req);
                let lose = drop_first.is_some_and(|(path, n)| {
                    req.path == path
                        && dropped.fetch_add(1, std::sync::atomic::Ordering::SeqCst) < n
                });
                log.lock().expect("lock").push(req);
                if lose {
                    return;
                }
                let mut out = format!(
                    "HTTP/1.1 {} X\r\nContent-Length: {}\r\nConnection: close\r\n",
                    res.status,
                    res.body.len()
                );
                for (name, value) in res.headers {
                    out.push_str(&format!("{name}: {value}\r\n"));
                }
                out.push_str("\r\n");
                out.push_str(&res.body);
                let _ = stream.write_all(out.as_bytes()).await;
                let _ = stream.shutdown().await;
            });
        }
    });
    MockServer {
        origin: format!("http://127.0.0.1:{port}"),
        seen,
    }
}

async fn read_request(stream: &mut tokio::net::TcpStream) -> Option<Req> {
    let mut buf = Vec::new();
    let mut chunk = [0_u8; 1024];
    let head_end = loop {
        let n = stream.read(&mut chunk).await.ok()?;
        if n == 0 {
            return None;
        }
        buf.extend_from_slice(&chunk[..n]);
        if let Some(pos) = buf.windows(4).position(|w| w == b"\r\n\r\n") {
            break pos + 4;
        }
    };
    let head = String::from_utf8_lossy(&buf[..head_end]).into_owned();
    let mut lines = head.split("\r\n");
    let mut first = lines.next()?.split(' ');
    let (method, path) = (first.next()?.to_owned(), first.next()?.to_owned());
    let headers: HashMap<String, String> = lines
        .filter_map(|l| l.split_once(':'))
        .map(|(k, v)| (k.trim().to_ascii_lowercase(), v.trim().to_owned()))
        .collect();
    let length: usize = headers
        .get("content-length")
        .and_then(|v| v.parse().ok())
        .unwrap_or(0);
    while buf.len() < head_end + length {
        let n = stream.read(&mut chunk).await.ok()?;
        if n == 0 {
            break;
        }
        buf.extend_from_slice(&chunk[..n]);
    }
    let body = String::from_utf8_lossy(&buf[head_end..]).into_owned();
    Some(Req {
        method,
        path,
        headers,
        body,
    })
}
