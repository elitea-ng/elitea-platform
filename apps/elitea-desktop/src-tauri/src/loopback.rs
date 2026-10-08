//! The one-shot loopback redirect listener of RFC 8252 section 7.3.
//!
//! The system browser is sent to the deployment's authorize endpoint with
//! `redirect_uri=http://127.0.0.1:<port>/callback`; after sign-in the
//! deployment redirects the browser here. The listener
//!
//! - binds to the IPv4 loopback address only (never `0.0.0.0`), on a port the
//!   OS picks, so nothing off this machine can reach it;
//! - answers only `GET /callback` carrying THIS attempt's `state`; anything
//!   else (a favicon request, a port scanner, a web page probing 127.0.0.1, a
//!   callback with the wrong state) is answered and ignored without ending the
//!   wait, which stops after the first matching callback or the deadline;
//! - serves each connection in its own task, so a connection that stalls
//!   cannot hold up the real callback;
//! - rejects a callback that repeats a parameter, so a code cannot be smuggled
//!   past the `state` check by shadowing;
//! - reads at most [`MAX_REQUEST_BYTES`] before giving up on a connection, and
//!   gives each connection [`READ_TIMEOUT`] to send its request line.

use std::collections::HashMap;
use std::net::{Ipv4Addr, SocketAddr};
use std::time::Duration;

use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
use tokio::net::{TcpListener, TcpStream};
use tokio::time::{Instant, timeout, timeout_at};

use crate::error::HostError;

pub const CALLBACK_PATH: &str = "/callback";
const MAX_REQUEST_BYTES: usize = 8 * 1024;
const READ_TIMEOUT: Duration = Duration::from_secs(2);

/// The query parameters of a valid callback.
pub type CallbackParams = HashMap<String, String>;

pub struct LoopbackListener {
    listener: TcpListener,
    port: u16,
}

impl LoopbackListener {
    /// Bind `127.0.0.1:0` and learn the port the OS chose.
    pub async fn bind() -> Result<Self, HostError> {
        let listener = TcpListener::bind(SocketAddr::from((Ipv4Addr::LOCALHOST, 0)))
            .await
            .map_err(|e| {
                HostError::Internal(format!("could not open the sign-in listener: {e}"))
            })?;
        let port = listener
            .local_addr()
            .map_err(|e| HostError::Internal(format!("could not open the sign-in listener: {e}")))?
            .port();
        Ok(Self { listener, port })
    }

    /// The redirect URI to register on the authorize request.
    pub fn redirect_uri(&self) -> String {
        format!("http://127.0.0.1:{}{CALLBACK_PATH}", self.port)
    }

    /// Wait for the first callback whose `state` equals `expected_state`.
    /// Consumes the listener: one attempt, one port, closed on return.
    pub async fn wait_for_callback(
        self,
        deadline: Duration,
        expected_state: &str,
    ) -> Result<CallbackParams, HostError> {
        let end = Instant::now() + deadline;
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let expected: std::sync::Arc<str> = expected_state.into();
        loop {
            tokio::select! {
                accepted = timeout_at(end, self.listener.accept()) => {
                    let (stream, _) = accepted
                        .map_err(|_| HostError::SignInAborted)?
                        .map_err(|e| HostError::Internal(format!("sign-in listener failed: {e}")))?;
                    let (tx, expected) = (tx.clone(), expected.clone());
                    tokio::spawn(async move {
                        if let Some(params) = serve_connection(stream, &expected).await {
                            let _ = tx.send(params);
                        }
                    });
                }
                Some(params) = rx.recv() => return Ok(params),
            }
        }
    }
}

const PAGE: &str = "<!doctype html><meta charset=utf-8><title>Elitea</title>\
<body style=\"font-family:system-ui,sans-serif;margin:3rem\"><h1>You can close this tab</h1>\
<p>Return to the Elitea desktop app to continue.</p></body>";

/// Read one request, answer it, and return the callback parameters when it
/// was a valid `GET /callback` for this attempt's `state`.
async fn serve_connection(mut stream: TcpStream, expected_state: &str) -> Option<CallbackParams> {
    let line = timeout(READ_TIMEOUT, read_request_line(&mut stream))
        .await
        .ok()??;
    match parse_request_line(&line) {
        Some(params)
            if params
                .get("state")
                .is_some_and(|got| crate::pkce::constant_time_eq(got, expected_state)) =>
        {
            respond(&mut stream, "200 OK", PAGE).await;
            Some(params)
        }
        Some(_) => {
            respond(&mut stream, "400 Bad Request", "").await;
            None
        }
        None => {
            respond(&mut stream, "404 Not Found", "").await;
            None
        }
    }
}

async fn read_request_line(stream: &mut TcpStream) -> Option<String> {
    let mut buf = Vec::with_capacity(512);
    let mut chunk = [0_u8; 512];
    while buf.len() < MAX_REQUEST_BYTES {
        let n = stream.read(&mut chunk).await.ok()?;
        if n == 0 {
            break;
        }
        buf.extend_from_slice(&chunk[..n]);
        if let Some(end) = buf.iter().position(|&b| b == b'\n') {
            buf.truncate(end);
            return String::from_utf8(buf).ok();
        }
    }
    None
}

async fn respond(stream: &mut TcpStream, status: &str, body: &str) {
    let response = format!(
        "HTTP/1.1 {status}\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: {}\r\n\
Cache-Control: no-store\r\nReferrer-Policy: no-referrer\r\nConnection: close\r\n\r\n{body}",
        body.len()
    );
    // The browser may have gone away; there is nothing useful to do about it.
    let _ = stream.write_all(response.as_bytes()).await;
    let _ = stream.shutdown().await;
}

/// `GET /callback?a=b HTTP/1.1` -> the parameters, or `None` for any other
/// method or path, a fragment, a repeated parameter or malformed escapes.
pub fn parse_request_line(line: &str) -> Option<CallbackParams> {
    let mut parts = line.trim_end_matches('\r').split(' ');
    let (method, target) = (parts.next()?, parts.next()?);
    if method != "GET" {
        return None;
    }
    let (path, query) = target.split_once('?')?;
    if path != CALLBACK_PATH || query.contains('#') {
        return None;
    }
    let mut out = CallbackParams::new();
    for pair in query.split('&').filter(|p| !p.is_empty()) {
        let (key, value) = pair.split_once('=').unwrap_or((pair, ""));
        let (key, value) = (decode(key)?, decode(value)?);
        if out.insert(key, value).is_some() {
            return None;
        }
    }
    Some(out)
}

/// Percent-decoding with `+` as space (form encoding); `None` on a malformed escape.
fn decode(input: &str) -> Option<String> {
    let bytes = input.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        match bytes[i] {
            b'+' => out.push(b' '),
            b'%' => {
                let hex = input.get(i + 1..i + 3)?;
                out.push(u8::from_str_radix(hex, 16).ok()?);
                i += 2;
            }
            b => out.push(b),
        }
        i += 1;
    }
    String::from_utf8(out).ok()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_a_callback_request_line() {
        let params = parse_request_line(
            "GET /callback?code=abc&state=xyz&iss=https%3A%2F%2Fh.example HTTP/1.1\r",
        )
        .unwrap();
        assert_eq!(params["code"], "abc");
        assert_eq!(params["state"], "xyz");
        assert_eq!(params["iss"], "https://h.example");
    }

    #[test]
    fn decodes_plus_and_percent_escapes() {
        let params = parse_request_line("GET /callback?error_description=a+b%21 HTTP/1.1").unwrap();
        assert_eq!(params["error_description"], "a b!");
    }

    #[test]
    fn rejects_other_methods_paths_fragments_and_repeats() {
        for line in [
            "POST /callback?code=a HTTP/1.1",
            "GET /favicon.ico HTTP/1.1",
            "GET /callback HTTP/1.1",
            "GET /callbackx?code=a HTTP/1.1",
            "GET /callback?code=a#frag HTTP/1.1",
            "GET /callback?code=a&code=b HTTP/1.1",
            "GET /callback?code=%zz HTTP/1.1",
            "GET /callback?code=%ff HTTP/1.1",
            "",
        ] {
            assert!(
                parse_request_line(line).is_none(),
                "{line:?} should be rejected"
            );
        }
    }

    async fn get(port: u16, target: &str) -> String {
        let mut stream = TcpStream::connect((Ipv4Addr::LOCALHOST, port))
            .await
            .unwrap();
        stream
            .write_all(format!("GET {target} HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n").as_bytes())
            .await
            .unwrap();
        let mut response = String::new();
        stream.read_to_string(&mut response).await.unwrap();
        response
    }

    #[tokio::test]
    async fn binds_loopback_on_an_ephemeral_port_and_serves_one_callback() {
        let listener = LoopbackListener::bind().await.unwrap();
        let uri = listener.redirect_uri();
        assert!(uri.starts_with("http://127.0.0.1:"));
        assert!(uri.ends_with("/callback"));
        let port: u16 = uri
            .rsplit_once(':')
            .unwrap()
            .1
            .trim_end_matches("/callback")
            .parse()
            .unwrap();
        assert_ne!(port, 0);

        let waiter = tokio::spawn(listener.wait_for_callback(Duration::from_secs(10), "the-state"));
        // Noise first: a favicon fetch must neither end the wait nor be answered with the page.
        assert!(get(port, "/favicon.ico").await.starts_with("HTTP/1.1 404"));
        let ok = get(port, "/callback?code=the-code&state=the-state").await;
        assert!(ok.starts_with("HTTP/1.1 200"));
        assert!(ok.contains("You can close this tab"));

        let params = waiter.await.unwrap().unwrap();
        assert_eq!(params["code"], "the-code");
        assert_eq!(params["state"], "the-state");
    }

    fn port_of(listener: &LoopbackListener) -> u16 {
        listener.port
    }

    #[tokio::test]
    async fn a_callback_with_the_wrong_state_does_not_end_the_wait() {
        let listener = LoopbackListener::bind().await.unwrap();
        let port = port_of(&listener);
        let waiter = tokio::spawn(listener.wait_for_callback(Duration::from_secs(10), "right"));
        // A web page probing the port with a well-formed but foreign callback.
        assert!(
            get(port, "/callback?code=x&state=wrong")
                .await
                .starts_with("HTTP/1.1 400")
        );
        assert!(
            get(port, "/callback?code=x")
                .await
                .starts_with("HTTP/1.1 400")
        );
        assert!(!waiter.is_finished(), "a probe must not abort sign-in");
        assert!(
            get(port, "/callback?code=real&state=right")
                .await
                .starts_with("HTTP/1.1 200")
        );
        assert_eq!(waiter.await.unwrap().unwrap()["code"], "real");
    }

    #[tokio::test]
    async fn a_stalled_connection_does_not_delay_the_real_callback() {
        let listener = LoopbackListener::bind().await.unwrap();
        let port = port_of(&listener);
        let waiter = tokio::spawn(listener.wait_for_callback(Duration::from_secs(10), "right"));
        // Connects and says nothing, holding its slot until the read timeout.
        let _stalled = TcpStream::connect((Ipv4Addr::LOCALHOST, port))
            .await
            .unwrap();
        let started = Instant::now();
        let ok = get(port, "/callback?code=real&state=right").await;
        assert!(ok.starts_with("HTTP/1.1 200"));
        assert!(started.elapsed() < READ_TIMEOUT, "served behind the stall");
        assert_eq!(waiter.await.unwrap().unwrap()["code"], "real");
    }

    #[tokio::test]
    async fn gives_up_at_the_deadline() {
        let listener = LoopbackListener::bind().await.unwrap();
        let result = listener
            .wait_for_callback(Duration::from_millis(100), "s")
            .await;
        assert!(matches!(result, Err(HostError::SignInAborted)));
    }

    #[tokio::test]
    async fn the_port_is_closed_after_the_attempt() {
        let listener = LoopbackListener::bind().await.unwrap();
        let uri = listener.redirect_uri();
        let port: u16 = uri
            .rsplit_once(':')
            .unwrap()
            .1
            .trim_end_matches("/callback")
            .parse()
            .unwrap();
        let _ = listener
            .wait_for_callback(Duration::from_millis(50), "s")
            .await;
        assert!(
            TcpStream::connect((Ipv4Addr::LOCALHOST, port))
                .await
                .is_err()
        );
    }
}
