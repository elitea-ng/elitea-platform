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

/// The pages the browser lands on, styled like the deployment's own sign-in
/// pages (services/elitea-main/.../browserauth/templates/auth.css): the same
/// tokens, glow backdrop and card, light and dark. Self-contained — the
/// listener serves nothing else, and the CSP header allows inline style only.
const PAGE_STYLE: &str = "<style>\
:root{color-scheme:light dark;--accent:#c428dd;--page:#f3f7fc;--card:#fff;--text:#0e131d;\
--muted:#545864;--card-border:rgb(61 68 86/12%);--glow-1:var(--accent);--glow-2:#29a3f5;\
--glow-strength:14%;--shadow:0 1px 2px rgb(14 19 29/6%),0 24px 64px -12px rgb(14 19 29/18%);\
--ok:#1b7f4b;--ok-bg:#e8f6ee;--warn:#9e0f0f;--warn-bg:#fdeced}\
@media (prefers-color-scheme:dark){:root{--accent:#6ae8fa;--page:#0b111b;--card:#181f2a;\
--text:#fff;--muted:#a9b7c1;--card-border:rgb(255 255 255/9%);--glow-2:#dd42bb;--glow-strength:18%;\
--shadow:0 1px 2px rgb(0 0 0/30%),0 32px 80px -16px rgb(0 0 0/65%);--ok:#7ee2a8;\
--ok-bg:rgb(46 160 98/16%);--warn:#ffb3ae;--warn-bg:rgb(215 22 22/14%)}}\
html,body{min-height:100%}\
body{margin:0;min-height:100vh;display:grid;place-items:center;padding:56px 16px;box-sizing:border-box;\
background-color:var(--page);background-image:\
radial-gradient(340px circle at calc(50% - 350px) calc(50% + 80px),color-mix(in srgb,var(--glow-2) var(--glow-strength),transparent),transparent),\
radial-gradient(280px circle at calc(50% + 360px) calc(50% - 170px),color-mix(in srgb,var(--glow-1) var(--glow-strength),transparent),transparent),\
radial-gradient(ellipse 60% 55% at 50% 45%,color-mix(in srgb,var(--glow-1) var(--glow-strength),transparent) 0%,transparent 70%);\
background-attachment:fixed;color:var(--text);font-size:15px;line-height:1.5;\
font-family:\"Montserrat\",\"Inter\",system-ui,-apple-system,BlinkMacSystemFont,\"Segoe UI\",Roboto,sans-serif}\
main{width:min(100%,420px);box-sizing:border-box;padding:36px 32px 32px;text-align:center;\
border:1px solid var(--card-border);border-radius:16px;background:color-mix(in srgb,var(--card) 84%,transparent);\
-webkit-backdrop-filter:blur(20px) saturate(140%);backdrop-filter:blur(20px) saturate(140%);box-shadow:var(--shadow)}\
.mark{display:grid;place-items:center;width:48px;height:48px;margin:0 auto 20px;border-radius:50%;\
color:var(--ok);background:var(--ok-bg)}\
.mark.warn{color:var(--warn);background:var(--warn-bg)}\
.mark svg{width:24px;height:24px}\
h1{margin:0 0 8px;font-size:1.5rem;font-weight:600;line-height:1.3}\
p{margin:0;color:var(--muted);font-size:.9375rem}\
</style>";

const CHECK_ICON: &str = "<svg viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" \
stroke-width=\"2.25\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\">\
<path d=\"M5 12.5l4.5 4.5L19 7.5\"/></svg>";

const ALERT_ICON: &str = "<svg viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" \
stroke-width=\"2\" stroke-linecap=\"round\" aria-hidden=\"true\">\
<circle cx=\"12\" cy=\"12\" r=\"9\"/><path d=\"M12 7.5v5.5M12 16.5v.01\"/></svg>";

/// Which page a browser request gets.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Outcome {
    /// The code (or the server's answer) reached the app.
    Done,
    /// The person declined or the server refused; the app shows why.
    Declined,
    /// Not this attempt's callback (an old tab, a reload after use).
    Stale,
}

fn page(outcome: Outcome) -> String {
    let (icon, warn, title, body) = match outcome {
        Outcome::Done => (
            CHECK_ICON,
            false,
            "You're signed in",
            "Return to the Elitea desktop app to continue. You can close this tab.",
        ),
        Outcome::Declined => (
            ALERT_ICON,
            true,
            "Sign-in did not finish",
            "Return to the Elitea desktop app to see why and try again. You can close this tab.",
        ),
        Outcome::Stale => (
            ALERT_ICON,
            true,
            "This sign-in link has expired",
            "Start again from the Elitea desktop app. You can close this tab.",
        ),
    };
    let class = if warn { "mark warn" } else { "mark" };
    format!(
        "<!doctype html><html lang=en><head><meta charset=utf-8>\
<meta name=viewport content=\"width=device-width,initial-scale=1\">\
<title>Elitea sign-in</title>{PAGE_STYLE}</head><body><main>\
<div class=\"{class}\">{icon}</div><h1>{title}</h1><p>{body}</p></main></body></html>"
    )
}

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
            let outcome = if params.contains_key("error") {
                Outcome::Declined
            } else {
                Outcome::Done
            };
            respond(&mut stream, "200 OK", &page(outcome)).await;
            Some(params)
        }
        Some(_) => {
            respond(&mut stream, "400 Bad Request", &page(Outcome::Stale)).await;
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
Cache-Control: no-store\r\nReferrer-Policy: no-referrer\r\n\
Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'\r\nConnection: close\r\n\r\n{body}",
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
        assert!(ok.contains("You're signed in"));
        assert!(
            ok.contains("Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'")
        );

        let params = waiter.await.unwrap().unwrap();
        assert_eq!(params["code"], "the-code");
        assert_eq!(params["state"], "the-state");
    }

    #[test]
    fn each_outcome_has_its_own_page_and_loads_nothing_remote() {
        let done = page(Outcome::Done);
        let declined = page(Outcome::Declined);
        let stale = page(Outcome::Stale);
        assert!(done.contains("You're signed in") && !done.contains("mark warn"));
        assert!(declined.contains("Sign-in did not finish") && declined.contains("mark warn"));
        assert!(stale.contains("This sign-in link has expired") && stale.contains("mark warn"));
        for html in [&done, &declined, &stale] {
            assert!(html.contains("prefers-color-scheme:dark"));
            assert!(
                !html.contains("http://")
                    && !html.contains("https://")
                    && !html.contains("<script")
            );
        }
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
