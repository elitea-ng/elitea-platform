use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

use async_trait::async_trait;
use elitea_connectors::transport::{
    HeaderMap, Method, Request, Response, StatusCode, Transport, TransportError, Url,
};
use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
use tokio::net::TcpListener;

use super::*;

fn allowlist(entries: &[&str]) -> Arc<RunEgress> {
    RunEgress::new(PublicPolicy::allowlist(entries.iter().copied()).expect("policy"))
}

fn url(text: &str) -> reqwest::Url {
    reqwest::Url::parse(text).expect("url")
}

#[test]
fn open_admits_every_host_and_allowlist_only_its_entries() {
    let open = RunEgress::open();
    assert!(open.check_url(&url("https://anything.example/x")).is_ok());
    assert!(open.check_url(&url("http://203.0.113.9:8080/x")).is_ok());

    let run = allowlist(&["api.example.com", "*.atlassian.net:443", "203.0.113.0/24"]);
    assert!(
        run.check_url(&url("https://api.example.com/v1?key=secret"))
            .is_ok()
    );
    assert!(
        run.check_url(&url("https://acme.atlassian.net/rest"))
            .is_ok()
    );
    assert!(run.check_url(&url("http://203.0.113.9:8080/")).is_ok());
    let refused = run
        .check_url(&url("https://acme.atlassian.net:8443/rest?token=x"))
        .expect_err("pinned port");
    assert_eq!(
        (refused.host(), refused.port()),
        ("acme.atlassian.net", 8443)
    );
    let refused = run
        .check_url(&url("https://evil.example/path?key=secret"))
        .expect_err("unnamed host");
    assert_eq!(refused.authority(), "evil.example:443");
    assert!(refused.message().contains("evil.example:443"));
    assert!(!refused.message().contains("secret"), "never the URL");
    let refused = run
        .check_url(&url("http://[2001:db8::7]:9000/"))
        .expect_err("IPv6 literal");
    assert_eq!(refused.authority(), "[2001:db8::7]:9000");
    let error = refused.tool_error();
    assert_eq!(error.code, EGRESS_HOST_NOT_ALLOWED);
    assert!(!error.is_retryable());
}

#[test]
fn a_url_without_host_or_port_is_refused_and_counted() {
    let open = RunEgress::open();
    assert!(open.check_url(&url("file:///etc/passwd")).is_ok());
    assert!(open.summary().is_empty());

    let run = allowlist(&["api.example.com"]);
    let refused = run
        .check_url(&url("file:///etc/passwd"))
        .expect_err("no host");
    assert_eq!((refused.host(), refused.port()), (INVALID_HOST, 0));
    let refused = run
        .check_url(&url("custom://API.example.com/x"))
        .expect_err("no known port");
    assert_eq!((refused.host(), refused.port()), ("api.example.com", 0));
    let _ = run.check_url(&url("file:///other"));
    let summary = run.summary();
    assert_eq!(summary.reached, Vec::<HostCount>::new());
    assert_eq!(
        summary.refused,
        vec![
            HostCount {
                host: INVALID_HOST.to_owned(),
                port: 0,
                count: 2,
            },
            HostCount {
                host: "api.example.com".to_owned(),
                port: 0,
                count: 1,
            },
        ]
    );
}

#[test]
fn the_summary_counts_and_bounds_hosts() {
    let run = allowlist(&["*.reached.example"]);
    for index in 0..300 {
        let host = format!("https://h{index}.reached.example/");
        run.record_reached(&url(&host));
        run.record_reached(&url(&host));
    }
    for _ in 0..3 {
        let _ = run.check_url(&url("https://Refused.Example./a"));
    }
    let summary = run.summary();
    assert_eq!(summary.reached.len(), MAX_SUMMARY_HOSTS);
    assert!(summary.reached.iter().all(|host| host.count == 2));
    assert_eq!(
        summary.other_hosts,
        300 - MAX_SUMMARY_HOSTS as u64,
        "repeats count once"
    );
    assert_eq!(
        summary.refused,
        vec![HostCount {
            host: "refused.example".to_owned(),
            port: 443,
            count: 3,
        }]
    );
    assert!(RunEgress::open().summary().is_empty());
}

#[tokio::test]
async fn attributed_names_the_first_refusal_of_one_call() {
    let run = allowlist(&["api.example.com"]);
    let (output, refused) = attributed(Arc::clone(&run), async {
        let current = current();
        let _ = current.check("first.example", 443);
        let _ = current.check("second.example", 443);
        7
    })
    .await;
    assert_eq!(output, 7);
    assert_eq!(refused.expect("refused").host(), "first.example");
    // Outside a call nothing is attributed, and the open fallback applies.
    assert!(current().policy().is_open());
    let ((), none) = attributed(run, async {}).await;
    assert!(none.is_none());
}

/// A one-shot HTTP server on loopback; returns its port and how many
/// connections it accepted.
async fn one_shot_server() -> (u16, Arc<AtomicUsize>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.expect("listener");
    let port = listener.local_addr().expect("address").port();
    let accepted = Arc::new(AtomicUsize::new(0));
    let counter = Arc::clone(&accepted);
    tokio::spawn(async move {
        while let Ok((mut socket, _)) = listener.accept().await {
            counter.fetch_add(1, Ordering::SeqCst);
            let mut buffer = [0; 4096];
            let _ = socket.read(&mut buffer).await;
            let _ = socket
                .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
                .await;
        }
    });
    (port, accepted)
}

#[tokio::test]
async fn the_client_refuses_an_ip_literal_before_connecting() {
    let (port, accepted) = one_shot_server().await;
    let target = url(&format!("http://127.0.0.1:{port}/data?key=secret"));

    let refused = scope(allowlist(&["api.example.com"]), async {
        client_builder().no_proxy().build_for_run().expect("client")
    })
    .await;
    let (sent, refusal) = attributed(Arc::clone(refused.egress()), async {
        refused
            .execute(reqwest::Request::new(reqwest::Method::GET, target.clone()))
            .await
    })
    .await;
    let error = sent.expect_err("refused");
    assert!(is_refusal(&error));
    assert!(error.is_connect());
    assert!(!error.to_string().contains("secret"));
    assert_eq!(
        refusal.expect("named").authority(),
        format!("127.0.0.1:{port}")
    );
    assert_eq!(accepted.load(Ordering::SeqCst), 0, "nothing was dialled");
    assert_eq!(refused.egress().summary().refused.len(), 1);

    // The same request on an open run goes out and is counted as reached.
    let open = scope(RunEgress::open(), async {
        client_builder().no_proxy().build_for_run().expect("client")
    })
    .await;
    let response = open
        .execute(reqwest::Request::new(reqwest::Method::GET, target))
        .await
        .expect("sent");
    assert_eq!(response.status(), reqwest::StatusCode::OK);
    assert_eq!(accepted.load(Ordering::SeqCst), 1);
    let summary = open.egress().summary();
    assert_eq!(
        summary.reached,
        vec![HostCount {
            host: "127.0.0.1".to_owned(),
            port,
            count: 1,
        }]
    );
}

#[tokio::test]
async fn an_operator_origin_is_sent_under_any_allowlist_and_recorded() {
    let (port, accepted) = one_shot_server().await;
    let target = url(&format!("http://127.0.0.1:{port}/v1"));
    let run = allowlist(&["api.example.com"]);
    let client = scope(Arc::clone(&run), async {
        client_builder().no_proxy().build_for_run().expect("client")
    })
    .await;
    assert!(
        client
            .execute(reqwest::Request::new(reqwest::Method::GET, target.clone()))
            .await
            .is_err(),
        "a run-chosen request to the same host is refused"
    );
    let response = client
        .execute_operator_origin(reqwest::Request::new(reqwest::Method::GET, target))
        .await
        .expect("sent");
    assert_eq!(response.status(), reqwest::StatusCode::OK);
    assert_eq!(accepted.load(Ordering::SeqCst), 1);
    let summary = run.summary();
    assert_eq!(summary.reached[0].host, "127.0.0.1");
    assert_eq!(summary.reached[0].port, port);
}

struct Counting(AtomicUsize);

#[async_trait]
impl Transport for Counting {
    async fn execute(&self, _request: Request) -> Result<Response, TransportError> {
        self.0.fetch_add(1, Ordering::SeqCst);
        Ok(Response::from_bytes(
            StatusCode::OK,
            HeaderMap::new(),
            bytes::Bytes::new(),
        ))
    }
}

#[tokio::test]
async fn the_connector_transport_refuses_and_records() {
    let inner = Arc::new(Counting(AtomicUsize::new(0)));
    let run = allowlist(&["api.github.com"]);
    let transport = EgressTransport::new(Arc::clone(&run), inner.clone());
    let allowed = Url::parse("https://api.github.com/repos").expect("url");
    let refused = Url::parse("https://evil.example/repos").expect("url");
    assert!(
        transport
            .execute(Request::new(Method::GET, allowed))
            .await
            .is_ok()
    );
    let error = transport
        .execute(Request::new(Method::GET, refused))
        .await
        .expect_err("refused");
    assert!(error.is_refused());
    assert_eq!(inner.0.load(Ordering::SeqCst), 1);
    let summary = run.summary();
    assert_eq!(summary.reached[0].host, "api.github.com");
    assert_eq!(summary.refused[0].host, "evil.example");
}

/// No toolkit family builds a reqwest client of its own: every client goes
/// through [`client_builder`] + [`BuildForRun::build_for_run`], so a new
/// family cannot forget the run's egress policy. The exceptions say why.
#[test]
fn no_toolkit_builds_an_unbound_reqwest_client() {
    const EXEMPT: &[(&str, &str)] = &[
        // The direct fetch's client resolves through the egress guard under
        // the run's policy, and `fetch` checks and records every hop.
        ("toolkits/families/web/direct.rs", "guarded resolver"),
        // reqwest 0.13 for rmcp; the endpoint is checked against the run's
        // policy before connecting, and redirects stay on its origin.
        ("toolkits/mcp.rs", "reqwest_mcp"),
        ("toolkits/mcp_tests.rs", "reqwest_mcp"),
    ];
    let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("src");
    let mut stack = vec![root.join("toolkits")];
    let mut offenders = Vec::new();
    while let Some(dir) = stack.pop() {
        for entry in std::fs::read_dir(&dir).expect("source dir") {
            let path = entry.expect("entry").path();
            if path.is_dir() {
                stack.push(path);
                continue;
            }
            if path.extension().is_none_or(|ext| ext != "rs") {
                continue;
            }
            let relative = path
                .strip_prefix(&root)
                .expect("under src")
                .to_string_lossy()
                .replace('\\', "/");
            if EXEMPT.iter().any(|(file, _)| *file == relative) {
                continue;
            }
            let text = std::fs::read_to_string(&path).expect("source");
            if text.contains("Client::builder()") || text.contains("ClientBuilder::new()") {
                offenders.push(relative);
            }
        }
    }
    assert!(
        offenders.is_empty(),
        "build toolkit HTTP clients with crate::egress::client_builder().build_for_run(): {offenders:?}"
    );
}
