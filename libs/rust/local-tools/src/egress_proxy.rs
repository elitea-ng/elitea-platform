//! The per-command filtering proxy behind a rule's `hosts` (#3005).
//!
//! The OS sandboxes filter by address, never by host name (Seatbelt's
//! `network-outbound` takes an IP and a port), so a command whose rule
//! limits it to named hosts gets no network but one loopback port: this
//! proxy's. The proxy runs in the host, outside the sandbox, for that one
//! command, and accepts only:
//!
//! * `CONNECT <host>:443`, where `<host>` is one of the rule's hosts (an
//!   exact name, or `*.example.com` for any subdomain of it), never an IP
//!   literal;
//! * with the command's own random credential in `Proxy-Authorization`
//!   (`Basic`, user `elitea`), so another command that may reach the
//!   loopback cannot borrow the tunnel.
//!
//! Everything else is refused (`403`, `407` without the credential, `405`
//! for a plain HTTP request) and the refused host is recorded for the
//! command's result. The tunnel is TLS end to end: the proxy sees the host
//! name and the bytes' count, never their content. The listener and every
//! tunnel stop when the [`EgressProxy`] is dropped (the command ended).

use std::collections::BTreeSet;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use base64::Engine as _;
use serde::Serialize;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::task::JoinSet;

/// The most hosts one rule may name.
pub const MAX_HOSTS: usize = 32;

/// The largest request head read before it is refused.
const MAX_HEAD: usize = 8 * 1024;

/// The most tunnels open at once for one command.
const MAX_TUNNELS: usize = 64;

/// How long the client has to send its request head.
const HEAD_TIMEOUT: Duration = Duration::from_secs(10);

/// How long resolving and connecting upstream may take.
const CONNECT_TIMEOUT: Duration = Duration::from_secs(15);

/// The one port a tunnel may reach.
const HTTPS_PORT: u16 = 443;

/// The proxy user name in the command's proxy URL.
const PROXY_USER: &str = "elitea";

/// Why a host entry of a rule is refused, or `None` for a valid one:
/// lowercase DNS labels (`a-z`, `0-9`, `-`, not at a label's ends), at
/// least two labels, optionally `*.` in front, no IP literal, no port.
#[must_use]
pub fn host_problem(entry: &str) -> Option<String> {
    let name = entry.strip_prefix("*.").unwrap_or(entry);
    let labels: Vec<&str> = name.split('.').collect();
    let label_ok = |label: &&str| {
        !label.is_empty()
            && label.len() <= 63
            && label
                .bytes()
                .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-')
            && !label.starts_with('-')
            && !label.ends_with('-')
    };
    if entry.is_empty() || name.len() > 253 || labels.len() < 2 || !labels.iter().all(label_ok) {
        return Some(format!(
            "`{entry}` is not a host name (lowercase, like api.github.com or *.github.com; no port, no scheme)"
        ));
    }
    if labels
        .last()
        .is_some_and(|top| top.bytes().all(|byte| byte.is_ascii_digit()))
    {
        return Some(format!("`{entry}` looks like an IP address; name a host"));
    }
    None
}

/// Whether `host` (as a client asked for it) is one of `allowed`.
#[must_use]
pub fn host_allowed(allowed: &[String], host: &str) -> bool {
    let host = host.strip_suffix('.').unwrap_or(host).to_ascii_lowercase();
    if host.parse::<std::net::IpAddr>().is_ok() || host_problem(&host).is_some() {
        return false;
    }
    allowed.iter().any(|entry| match entry.strip_prefix("*.") {
        Some(parent) => host
            .strip_suffix(parent)
            .is_some_and(|rest| rest.len() > 1 && rest.ends_with('.')),
        None => *entry == host,
    })
}

/// What a scoped command's network did, for its result.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize)]
pub struct NetworkReport {
    /// The hosts the command was limited to.
    pub allowed_hosts: Vec<String>,
    /// The hosts it asked for and was refused (sorted, each once).
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub refused_hosts: Vec<String>,
}

/// A running proxy for one command.
pub struct EgressProxy {
    addr: SocketAddr,
    credential: String,
    hosts: Vec<String>,
    refused: Arc<Mutex<BTreeSet<String>>>,
    task: tokio::task::JoinHandle<()>,
}

impl std::fmt::Debug for EgressProxy {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // Never the credential.
        formatter
            .debug_struct("EgressProxy")
            .field("addr", &self.addr)
            .field("hosts", &self.hosts)
            .finish_non_exhaustive()
    }
}

impl Drop for EgressProxy {
    fn drop(&mut self) {
        // The accept loop owns the tunnels' JoinSet: aborting it drops the
        // set, which aborts every tunnel.
        self.task.abort();
    }
}

/// A random hex credential (128 bits).
fn random_credential() -> std::io::Result<String> {
    let mut bytes = [0u8; 16];
    getrandom::fill(&mut bytes).map_err(|error| std::io::Error::other(error.to_string()))?;
    Ok(bytes.iter().fold(String::new(), |mut out, byte| {
        use std::fmt::Write as _;
        let _ = write!(out, "{byte:02x}");
        out
    }))
}

impl EgressProxy {
    /// Listen on `127.0.0.1` (an ephemeral port) for `hosts`, upstream
    /// through the system resolver.
    ///
    /// # Errors
    ///
    /// The listener cannot be bound or no random credential is available.
    pub async fn start(hosts: Vec<String>) -> std::io::Result<Self> {
        Self::start_with(hosts, Upstream::System).await
    }

    /// [`Self::start`] with where tunnels connect to (tests: a local
    /// listener instead of the real host).
    ///
    /// # Errors
    ///
    /// As [`Self::start`].
    #[doc(hidden)]
    pub async fn start_with(hosts: Vec<String>, upstream: Upstream) -> std::io::Result<Self> {
        let listener = TcpListener::bind(("127.0.0.1", 0)).await?;
        let addr = listener.local_addr()?;
        let credential = random_credential()?;
        let expected = format!(
            "Basic {}",
            base64::engine::general_purpose::STANDARD.encode(format!("{PROXY_USER}:{credential}"))
        );
        let refused = Arc::new(Mutex::new(BTreeSet::new()));
        let shared = Arc::new(Shared {
            hosts: hosts.clone(),
            expected,
            refused: Arc::clone(&refused),
            upstream,
        });
        let task = tokio::spawn(accept_loop(listener, shared));
        Ok(Self {
            addr,
            credential,
            hosts,
            refused,
            task,
        })
    }

    /// The loopback port the sandbox lets the command reach.
    #[must_use]
    pub const fn port(&self) -> u16 {
        self.addr.port()
    }

    /// The proxy URL the command's environment carries (it holds the
    /// credential: never log it).
    #[must_use]
    pub fn url(&self) -> String {
        format!(
            "http://{PROXY_USER}:{}@127.0.0.1:{}",
            self.credential,
            self.addr.port()
        )
    }

    /// What the command's network did so far.
    #[must_use]
    pub fn report(&self) -> NetworkReport {
        NetworkReport {
            allowed_hosts: self.hosts.clone(),
            refused_hosts: self
                .refused
                .lock()
                .map(|refused| refused.iter().cloned().collect())
                .unwrap_or_default(),
        }
    }
}

/// Where a tunnel connects to.
#[derive(Clone, Debug)]
#[doc(hidden)]
pub enum Upstream {
    /// The host's name through the system resolver, port 443.
    System,
    /// Every allowed host to this address (tests).
    Fixed(SocketAddr),
}

struct Shared {
    hosts: Vec<String>,
    expected: String,
    refused: Arc<Mutex<BTreeSet<String>>>,
    upstream: Upstream,
}

async fn accept_loop(listener: TcpListener, shared: Arc<Shared>) {
    let mut tunnels = JoinSet::new();
    loop {
        tokio::select! {
            accepted = listener.accept() => {
                let Ok((stream, _)) = accepted else { continue };
                if tunnels.len() >= MAX_TUNNELS {
                    drop(stream);
                    continue;
                }
                tunnels.spawn(serve(stream, Arc::clone(&shared)));
            }
            Some(_) = tunnels.join_next(), if !tunnels.is_empty() => {}
        }
    }
}

/// Read the request head (through the blank line), bounded.
async fn read_head(stream: &mut TcpStream) -> Option<Vec<u8>> {
    let mut head = Vec::new();
    let mut byte = [0u8; 1];
    // One byte at a time: nothing past the head is consumed, so bytes the
    // client sends early (a TLS hello) stay in the socket for the tunnel.
    while !head.ends_with(b"\r\n\r\n") {
        if head.len() >= MAX_HEAD {
            return None;
        }
        match stream.read(&mut byte).await {
            Ok(1) => head.push(byte[0]),
            _ => return None,
        }
    }
    Some(head)
}

async fn reply(stream: &mut TcpStream, status: &str) {
    let mut text = format!("HTTP/1.1 {status}\r\nConnection: close\r\nContent-Length: 0\r\n");
    if status.starts_with("407") {
        text.push_str("Proxy-Authenticate: Basic realm=\"elitea\"\r\n");
    }
    text.push_str("\r\n");
    let _ = stream.write_all(text.as_bytes()).await;
    let _ = stream.shutdown().await;
}

/// The request's target `host:port` and its `Proxy-Authorization`, from a
/// `CONNECT` head; `Err(status)` otherwise.
fn parse_connect(head: &[u8]) -> Result<(String, String, Option<String>), &'static str> {
    let text = std::str::from_utf8(head).map_err(|_| "400 Bad Request")?;
    let mut lines = text.split("\r\n");
    let request = lines.next().unwrap_or_default();
    let mut words = request.split(' ');
    let (Some(method), Some(target), Some(version), None) =
        (words.next(), words.next(), words.next(), words.next())
    else {
        return Err("400 Bad Request");
    };
    if !version.starts_with("HTTP/1.") {
        return Err("400 Bad Request");
    }
    if method != "CONNECT" {
        return Err("405 Method Not Allowed");
    }
    let (host, port) = target.rsplit_once(':').ok_or("400 Bad Request")?;
    let authorization = lines.find_map(|line| {
        let (name, value) = line.split_once(':')?;
        name.trim()
            .eq_ignore_ascii_case("proxy-authorization")
            .then(|| value.trim().to_owned())
    });
    Ok((host.to_owned(), port.to_owned(), authorization))
}

/// Constant-time comparison of two strings' bytes.
fn same(left: &str, right: &str) -> bool {
    left.len() == right.len()
        && left
            .bytes()
            .zip(right.bytes())
            .fold(0u8, |acc, (a, b)| acc | (a ^ b))
            == 0
}

async fn serve(mut stream: TcpStream, shared: Arc<Shared>) {
    let head = tokio::time::timeout(HEAD_TIMEOUT, read_head(&mut stream)).await;
    let Ok(Some(head)) = head else {
        reply(&mut stream, "400 Bad Request").await;
        return;
    };
    let (host, port, authorization) = match parse_connect(&head) {
        Ok(parsed) => parsed,
        Err(status) => {
            reply(&mut stream, status).await;
            return;
        }
    };
    if !authorization.is_some_and(|value| same(&value, &shared.expected)) {
        reply(&mut stream, "407 Proxy Authentication Required").await;
        return;
    }
    let shown = host
        .trim_start_matches('[')
        .trim_end_matches(']')
        .to_ascii_lowercase();
    if port.parse::<u16>().ok() != Some(HTTPS_PORT) || !host_allowed(&shared.hosts, &shown) {
        if let Ok(mut refused) = shared.refused.lock()
            && refused.len() < MAX_HOSTS * 4
        {
            refused.insert(format!("{shown}:{port}"));
        }
        reply(&mut stream, "403 Forbidden").await;
        return;
    }
    let name = shown.strip_suffix('.').unwrap_or(&shown).to_owned();
    let upstream = tokio::time::timeout(CONNECT_TIMEOUT, async {
        match &shared.upstream {
            Upstream::System => TcpStream::connect((name.as_str(), HTTPS_PORT)).await,
            Upstream::Fixed(addr) => TcpStream::connect(addr).await,
        }
    })
    .await;
    let Ok(Ok(mut upstream)) = upstream else {
        reply(&mut stream, "502 Bad Gateway").await;
        return;
    };
    if stream
        .write_all(b"HTTP/1.1 200 Connection Established\r\n\r\n")
        .await
        .is_err()
    {
        return;
    }
    let _ = tokio::io::copy_bidirectional(&mut stream, &mut upstream).await;
}

#[cfg(test)]
mod tests {
    use std::net::SocketAddr;

    use base64::Engine as _;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::{TcpListener, TcpStream};

    use super::{EgressProxy, Upstream, host_allowed, host_problem};

    #[test]
    fn host_entries() {
        for good in [
            "api.github.com",
            "github.com",
            "*.githubusercontent.com",
            "a-b.c9.io",
        ] {
            assert_eq!(host_problem(good), None, "{good}");
        }
        for bad in [
            "",
            "github",
            "GitHub.com",
            "api.github.com:443",
            "https://github.com",
            "10.0.0.1",
            "*.*.github.com",
            "*github.com",
            "-a.github.com",
            "a..com",
            "[::1]",
            "a.github.com/x",
        ] {
            assert!(host_problem(bad).is_some(), "{bad}");
        }
    }

    #[test]
    fn host_matching() {
        let allowed = vec![
            "api.github.com".to_owned(),
            "*.githubusercontent.com".to_owned(),
        ];
        for yes in [
            "api.github.com",
            "API.GitHub.com",
            "api.github.com.",
            "x.githubusercontent.com",
            "a.b.githubusercontent.com",
        ] {
            assert!(host_allowed(&allowed, yes), "{yes}");
        }
        for no in [
            "github.com",
            "githubusercontent.com",
            "evilgithubusercontent.com",
            "api.github.com.evil.com",
            "140.82.112.6",
            "::1",
            "x.api.github.com",
        ] {
            assert!(!host_allowed(&allowed, no), "{no}");
        }
    }

    async fn echo_upstream() -> SocketAddr {
        let listener = TcpListener::bind(("127.0.0.1", 0)).await.expect("bind");
        let addr = listener.local_addr().expect("addr");
        tokio::spawn(async move {
            while let Ok((mut stream, _)) = listener.accept().await {
                tokio::spawn(async move {
                    let mut buffer = [0u8; 64];
                    while let Ok(read) = stream.read(&mut buffer).await {
                        if read == 0 || stream.write_all(&buffer[..read]).await.is_err() {
                            break;
                        }
                    }
                });
            }
        });
        addr
    }

    fn auth(proxy: &EgressProxy) -> String {
        let url = proxy.url();
        let userinfo = url
            .trim_start_matches("http://")
            .split('@')
            .next()
            .expect("userinfo")
            .to_owned();
        format!(
            "Basic {}",
            base64::engine::general_purpose::STANDARD.encode(userinfo)
        )
    }

    async fn ask(proxy: &EgressProxy, head: &str) -> (String, TcpStream) {
        let mut stream = TcpStream::connect(("127.0.0.1", proxy.port()))
            .await
            .expect("connect");
        stream.write_all(head.as_bytes()).await.expect("write");
        let mut answer = Vec::new();
        let mut byte = [0u8; 1];
        while !answer.ends_with(b"\r\n\r\n") {
            match stream.read(&mut byte).await {
                Ok(1) => answer.push(byte[0]),
                _ => break,
            }
        }
        let status = String::from_utf8_lossy(&answer)
            .lines()
            .next()
            .unwrap_or_default()
            .to_owned();
        (status, stream)
    }

    #[tokio::test]
    async fn tunnels_only_listed_hosts_with_the_credential() {
        let upstream = echo_upstream().await;
        let proxy =
            EgressProxy::start_with(vec!["api.github.com".to_owned()], Upstream::Fixed(upstream))
                .await
                .expect("proxy");
        let auth = auth(&proxy);
        let url = proxy.url();
        let credential = url
            .trim_start_matches("http://elitea:")
            .split('@')
            .next()
            .expect("credential");
        assert_eq!(credential.len(), 32);
        assert!(!format!("{proxy:?}").contains(credential));

        let (status, mut tunnel) = ask(
            &proxy,
            &format!("CONNECT api.github.com:443 HTTP/1.1\r\nHost: api.github.com:443\r\nProxy-Authorization: {auth}\r\n\r\n"),
        )
        .await;
        assert!(status.contains("200"), "{status}");
        tunnel.write_all(b"ping").await.expect("write");
        let mut echoed = [0u8; 4];
        tunnel.read_exact(&mut echoed).await.expect("echo");
        assert_eq!(&echoed, b"ping");

        for (head, want) in [
            (format!("CONNECT evil.example.com:443 HTTP/1.1\r\nProxy-Authorization: {auth}\r\n\r\n"), "403"),
            (format!("CONNECT 140.82.112.6:443 HTTP/1.1\r\nProxy-Authorization: {auth}\r\n\r\n"), "403"),
            (format!("CONNECT api.github.com:22 HTTP/1.1\r\nProxy-Authorization: {auth}\r\n\r\n"), "403"),
            (format!("GET http://api.github.com/ HTTP/1.1\r\nProxy-Authorization: {auth}\r\n\r\n"), "405"),
            ("CONNECT api.github.com:443 HTTP/1.1\r\n\r\n".to_owned(), "407"),
            ("CONNECT api.github.com:443 HTTP/1.1\r\nProxy-Authorization: Basic ZWxpdGVhOndyb25n\r\n\r\n".to_owned(), "407"),
            ("garbage\r\n\r\n".to_owned(), "400"),
        ] {
            let (status, _) = ask(&proxy, &head).await;
            assert!(status.contains(want), "{head:?}: {status}");
        }
        let report = proxy.report();
        assert_eq!(report.allowed_hosts, vec!["api.github.com".to_owned()]);
        assert_eq!(
            report.refused_hosts,
            vec![
                "140.82.112.6:443".to_owned(),
                "api.github.com:22".to_owned(),
                "evil.example.com:443".to_owned()
            ]
        );
    }

    #[tokio::test]
    async fn dropping_the_proxy_stops_it() {
        let proxy = EgressProxy::start(vec!["api.github.com".to_owned()])
            .await
            .expect("proxy");
        let port = proxy.port();
        drop(proxy);
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;
        assert!(TcpStream::connect(("127.0.0.1", port)).await.is_err());
    }
}
