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
                reason: REFUSAL_REASON_NOT_ALLOWED,
            },
            HostCount {
                host: "api.example.com".to_owned(),
                port: 0,
                count: 1,
                reason: REFUSAL_REASON_NOT_ALLOWED,
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
            reason: REFUSAL_REASON_NOT_ALLOWED,
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
            reason: "",
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
        .execute_operator_origin(reqwest::Request::new(reqwest::Method::GET, target), None)
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

/// Nothing in this crate builds an HTTP client of its own: every client
/// starts from a builder in `egress.rs` ([`client_builder`] +
/// [`BuildForRun::build_for_run`], [`mcp_client_builder`] for the remote MCP
/// session, [`fetch_client_builder`] for the direct fetch,
/// [`guarded_http_connector`] for hyper, [`hardened_transport`] for the
/// connectors), so a new family cannot forget the run's egress policy or the
/// SSRF rule (#1490, #2950). No exemption: tests build their clients through
/// the same builders. Only `egress.rs` itself and this test module (which
/// names the patterns) are not scanned.
#[test]
fn no_toolkit_builds_an_unbound_reqwest_client() {
    const EXEMPT: &[(&str, &str)] = &[
        ("egress.rs", "the guarded builders"),
        ("egress_tests.rs", "names the patterns"),
    ];
    const PATTERNS: &[&str] = &[
        "Client::builder()",
        "ClientBuilder::new()",
        "reqwest::Client::new()",
        "reqwest_mcp::Client::new()",
        "HttpConnector::new(",
        ".dns_resolver(",
        "Proxy::",
    ];
    let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("src");
    let mut stack = vec![root.clone()];
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
            if PATTERNS.iter().any(|pattern| text.contains(pattern)) {
                offenders.push(relative);
            }
        }
    }
    assert!(
        offenders.is_empty(),
        "build HTTP clients with the builders in crate::egress (client_builder().build_for_run(), mcp_client_builder, ...): {offenders:?}"
    );
}

/// #1490 review: outside any run a client reaches NO public host. This
/// crate's unit tests pass `open` (they build family clients directly
/// against local fixtures); every other build passes `false`, which
/// `tests/egress_unscoped.rs` proves through the real `current()`.
#[test]
fn a_client_built_outside_any_run_fails_closed() {
    let unscoped = unscoped(false);
    assert!(!unscoped.policy().is_open());
    assert!(unscoped.check("api.example.com", 443).is_err());
    assert!(
        unscoped
            .check_url(&url("https://api.example.com/v1"))
            .is_err()
    );
    let closed = RunEgress::closed();
    assert!(closed.check("203.0.113.9", 443).is_err());
}

/// ADR-0044 decision 3: a delegated agent's egress is its declared hosts
/// intersected with the run's allowlist. A host the allowlist does not name
/// refuses the delegation, naming the host.
#[test]
fn delegation_narrows_a_callee_to_the_run_allowlist() {
    use crate::delegation::{DelegationRefusal, DelegationScope, GrantKind, GrantSet};
    use serde_json::json;
    use std::collections::BTreeSet;

    let jira = |url: &str| {
        json!({"id": 7, "type": "jira", "toolkit_name": "Jira",
            "settings": {"jira_configuration": {"base_url": url}, "limit": 5}})
    };
    let caller = DelegationScope::root(
        Arc::default(),
        GrantSet::from_tools(&[jira("https://acme.atlassian.net")]),
    );
    let policy = PublicPolicy::allowlist(["acme.atlassian.net"]).expect("policy");

    let admitted = caller
        .delegate(
            &mut vec![jira("https://acme.atlassian.net")],
            &BTreeSet::new(),
            &policy,
        )
        .expect("an allowlisted host is delegated");
    assert_eq!(
        admitted.scope.grants().canonical()["egress_hosts"],
        json!(["acme.atlassian.net"])
    );

    let refused = caller
        .delegate(
            &mut vec![jira("https://evil.example/rest")],
            &BTreeSet::new(),
            &policy,
        )
        .expect_err("a host off the allowlist is refused");
    assert_eq!(
        refused.refusal,
        DelegationRefusal::GrantMissing {
            kind: GrantKind::EgressHost,
            name: "evil.example".to_owned(),
            toolkit: None,
        }
    );
    // An open run checks no host; a bare label (Main may derive a host from
    // it) is never guessed at.
    caller
        .delegate(
            &mut vec![jira("https://evil.example")],
            &BTreeSet::new(),
            &PublicPolicy::Open,
        )
        .expect("open");
    caller
        .delegate(&mut vec![jira("acme")], &BTreeSet::new(), &policy)
        .expect("a bare label is not read");
}

// ADR-0044 decision 4: a RunEgress reused for a new claim of the same
// command binds that claim's assertions; the released claim's binding is
// not kept.
#[test]
fn a_new_claim_replaces_the_assertion_binding() {
    use crate::remote_assertion::{
        DelegationAssertionError, DelegationAssertionSource, DelegationAssertions, MintedAssertion,
    };

    struct Never;

    #[async_trait]
    impl DelegationAssertionSource for Never {
        async fn mint(&self, _: &str) -> Result<MintedAssertion, DelegationAssertionError> {
            Err(DelegationAssertionError::Unavailable)
        }
    }

    let egress = RunEgress::open();
    assert!(egress.assertions().is_none());
    let first = DelegationAssertions::new(Arc::new(Never), false);
    egress.bind_assertions(Arc::clone(&first));
    let second = DelegationAssertions::new(Arc::new(Never), true);
    egress.bind_assertions(Arc::clone(&second));
    let bound = egress.assertions().expect("bound");
    assert!(Arc::ptr_eq(&bound, &second));
    assert!(!Arc::ptr_eq(&bound, &first));
    assert!(bound.required());
}

// ── The SSRF rule under every family client (#2950) ─────────────────────────

/// A resolver that answers fixed addresses, so no test touches the network.
pub(crate) struct FakeLookup(std::collections::HashMap<&'static str, Vec<IpAddr>>);

impl Lookup for FakeLookup {
    fn lookup(&self, host: String) -> elitea_egress_guard::LookupFuture {
        let answer = self.0.get(host.as_str()).cloned();
        Box::pin(async move { answer.ok_or_else(|| std::io::Error::other("no such host")) })
    }
}

fn ip(text: &str) -> IpAddr {
    text.parse().expect("address")
}

/// The names the tests resolve: one per refused range, and one on loopback
/// that a permit names.
pub(crate) fn private_lookup() -> Arc<dyn Lookup> {
    Arc::new(FakeLookup(std::collections::HashMap::from([
        ("ten.example", vec![ip("10.1.2.3")]),
        ("loop.example", vec![ip("127.0.0.1")]),
        ("metadata.example", vec![ip("169.254.169.254")]),
        ("ula.example", vec![ip("fd12:3456::1")]),
        ("permitted.example", vec![ip("127.0.0.1")]),
        (
            "crowded.example",
            (1..=40).map(|n| ip(&format!("203.0.113.{n}"))).collect(),
        ),
    ])))
}

/// A run with `permit` as its private permit and the fake resolver.
pub(crate) fn guarded_run(permit: &[&str]) -> Arc<RunEgress> {
    RunEgress::configured(
        PublicPolicy::Open,
        PrivatePermit::parse(permit.iter().copied()).expect("permit"),
        private_lookup(),
    )
}

async fn family_client(run: &Arc<RunEgress>) -> HttpClient {
    scope(Arc::clone(run), async {
        client_builder().build_for_run().expect("client")
    })
    .await
}

/// Send one GET through `client` as a tool call, and return the error and
/// the refusal the call was told about.
async fn refused_call(client: &HttpClient, target: &str) -> (reqwest::Error, EgressRefused) {
    let (sent, refusal) = attributed(Arc::clone(client.egress()), async {
        client
            .execute(reqwest::Request::new(reqwest::Method::GET, url(target)))
            .await
    })
    .await;
    let error = sent.expect_err(target);
    (error, refusal.expect("the call is told about the refusal"))
}

#[tokio::test]
async fn a_family_client_refuses_names_that_resolve_to_private_link_local_or_metadata_addresses() {
    let (port, accepted) = one_shot_server().await;
    let run = guarded_run(&[]);
    let client = family_client(&run).await;
    for host in [
        "ten.example",
        "loop.example",
        "metadata.example",
        "ula.example",
    ] {
        let target = format!("http://{host}:{port}/rest/api?token=secret");
        let (error, refused) = refused_call(&client, &target).await;
        assert!(is_refusal(&error), "{host}");
        assert_eq!(refused.reason(), REFUSAL_REASON_NOT_PUBLIC, "{host}");
        assert_eq!(refused.authority(), format!("{host}:{port}"));
        assert_eq!(refused.tool_error().code, EGRESS_HOST_NOT_ALLOWED);
    }
    assert_eq!(accepted.load(Ordering::SeqCst), 0, "nothing was dialled");
}

#[tokio::test]
async fn a_family_client_refuses_private_link_local_and_metadata_ip_literals() {
    let (port, accepted) = one_shot_server().await;
    let run = guarded_run(&[]);
    let client = family_client(&run).await;
    for (target, authority) in [
        ("http://10.1.2.3/x".to_owned(), "10.1.2.3:80".to_owned()),
        (
            format!("http://127.0.0.1:{port}/x"),
            format!("127.0.0.1:{port}"),
        ),
        (
            "http://169.254.169.254/latest/meta-data".to_owned(),
            "169.254.169.254:80".to_owned(),
        ),
        (
            "https://[fd12:3456::1]/x".to_owned(),
            "[fd12:3456::1]:443".to_owned(),
        ),
    ] {
        let (error, refused) = refused_call(&client, &target).await;
        assert!(is_refusal(&error), "{target}");
        assert_eq!(refused.reason(), REFUSAL_REASON_NOT_PUBLIC);
        assert_eq!(refused.authority(), authority);
    }
    assert_eq!(accepted.load(Ordering::SeqCst), 0, "nothing was dialled");
}

#[tokio::test]
async fn a_host_in_the_private_permit_is_reached_on_its_checked_address() {
    let (port, accepted) = one_shot_server().await;
    let run = guarded_run(&["permitted.example"]);
    let client = family_client(&run).await;
    let response = client
        .execute(reqwest::Request::new(
            reqwest::Method::GET,
            url(&format!("http://permitted.example:{port}/rest")),
        ))
        .await
        .expect("a permitted private host is reached");
    assert_eq!(response.status(), reqwest::StatusCode::OK);
    assert_eq!(accepted.load(Ordering::SeqCst), 1);
    assert_eq!(run.summary().reached[0].host, "permitted.example");
    // A name the permit does not list, on the same address, stays refused.
    let (_, refused) = refused_call(&client, &format!("http://loop.example:{port}/")).await;
    assert_eq!(refused.reason(), REFUSAL_REASON_NOT_PUBLIC);
    // A permit that pins another port does not admit this one.
    let pinned = guarded_run(&["permitted.example:1"]);
    let client = family_client(&pinned).await;
    refused_call(&client, &format!("http://permitted.example:{port}/")).await;
    assert_eq!(accepted.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn metadata_stays_refused_when_the_permit_names_link_local() {
    let run = guarded_run(&["169.254.0.0/16", "metadata.example"]);
    let client = family_client(&run).await;
    for target in [
        "http://metadata.example/latest",
        "http://169.254.169.254/latest",
    ] {
        let (error, refused) = refused_call(&client, target).await;
        assert!(is_refusal(&error), "{target}");
        assert_eq!(refused.reason(), REFUSAL_REASON_NOT_PUBLIC);
    }
}

#[tokio::test]
async fn a_refusal_names_the_host_and_port_only_in_the_error_and_the_ledger() {
    let run = guarded_run(&[]);
    let client = family_client(&run).await;
    let (error, refused) =
        refused_call(&client, "https://ten.example:8443/secret/path?key=hunter2").await;
    let tool_error = refused.tool_error();
    assert_eq!(tool_error.code, EGRESS_HOST_NOT_ALLOWED);
    for text in [
        error.to_string(),
        format!("{error:?}"),
        tool_error.message.clone(),
        refused.message(),
    ] {
        assert!(!text.contains("secret"), "{text}");
        assert!(!text.contains("hunter2"), "{text}");
        assert!(!text.contains("/path"), "{text}");
    }
    assert!(refused.message().contains("ten.example:8443"));
    let summary = run.summary();
    assert_eq!(
        summary.refused,
        vec![HostCount {
            host: "ten.example".to_owned(),
            port: 8443,
            count: 1,
            reason: REFUSAL_REASON_NOT_PUBLIC,
        }]
    );
    assert_eq!(summary.reached, Vec::<HostCount>::new());
}

#[tokio::test]
async fn the_connectors_transport_refuses_a_private_destination() {
    let (port, accepted) = one_shot_server().await;
    let run = guarded_run(&[]);
    let transport = EgressTransport::new(
        Arc::clone(&run),
        Arc::new(
            hardened_transport(&elitea_connectors::github::client::CLIENT_POLICY)
                .expect("transport"),
        ),
    );
    let (sent, refusal) = attributed(Arc::clone(&run), async {
        transport
            .execute(Request::new(
                Method::GET,
                Url::parse(&format!("https://loop.example:{port}/x?key=secret")).expect("url"),
            ))
            .await
    })
    .await;
    assert!(sent.expect_err("refused").is_refused());
    assert_eq!(refusal.expect("named").reason(), REFUSAL_REASON_NOT_PUBLIC);
    assert_eq!(run.summary().refused[0].reason, REFUSAL_REASON_NOT_PUBLIC);
    assert_eq!(accepted.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn check_destination_applies_the_permit_before_a_session_dials() {
    let run = guarded_run(&["permitted.example"]);
    assert!(
        run.check_destination(&url("https://permitted.example/mcp"))
            .await
            .is_ok()
    );
    let refused = run
        .check_destination(&url("https://ten.example/mcp?token=x"))
        .await
        .expect_err("private");
    assert_eq!(refused.reason(), REFUSAL_REASON_NOT_PUBLIC);
    // A name that does not resolve is not a refusal: the dial fails itself.
    assert!(
        run.check_destination(&url("https://unknown.example/mcp"))
            .await
            .is_ok()
    );
}

/// A public host behind a DNS answer larger than the guard checks is refused
/// under its own reason, not as a private address the permit could admit.
#[tokio::test]
async fn an_oversized_answer_is_refused_as_too_many_addresses() {
    let run = guarded_run(&[]);
    let refused = run
        .check_destination(&url("https://crowded.example/mcp"))
        .await
        .expect_err("too many");
    assert_eq!(refused.reason(), REFUSAL_REASON_TOO_MANY_ADDRESSES);
    assert!(
        refused.message().contains("more addresses"),
        "{}",
        refused.message()
    );
    assert!(!refused.message().contains("private permit"));
}

/// One request counts one refusal, however often its client resolves.
#[tokio::test]
async fn a_request_counts_one_refusal_however_often_it_resolves() {
    let run = guarded_run(&[]);
    let resolver = PermitResolver::scoped();
    let target = url("https://ten.example/");
    let outcome = run
        .guarded(&target, async {
            for _ in 0..3 {
                assert!(
                    resolver
                        .resolve_host("ten.example".to_owned())
                        .await
                        .is_err()
                );
            }
        })
        .await;
    assert_eq!(
        outcome.expect_err("refused").reason(),
        REFUSAL_REASON_NOT_PUBLIC
    );
    let refused = run.summary().refused;
    assert_eq!(refused.len(), 1);
    assert_eq!(refused[0].count, 1);
}
