//! Fixtures for the `web` and `host_intel` family tests: a recording
//! provider transport, a fake meter, a fake DNS and a local HTTP origin.

use std::collections::{BTreeMap, HashMap, VecDeque};
use std::io;
use std::net::IpAddr;
use std::sync::atomic::{AtomicU32, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, Toolset};
use adk_tool::{BasicToolset, SimpleToolContext};
use async_trait::async_trait;
use elitea_egress_guard::{Allowlist, Lookup, LookupFuture};
use reqwest::{Request, StatusCode};
use serde_json::{Map, Value, json};
use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};

use super::families::web::config::{HostIntelConfig, WebConfig};
use super::families::web::meter::{
    Admission, Meter, MeterError, MeteredOp, SettleRequest, Settlement,
};
use super::families::web::registry::RegistryEntry;
use super::families::web::transport::{HttpAnswer, ProviderError, Transport};
use super::families::web::{Parts, WebToolHost, build_host_intel_with, build_web_with};
use super::policy::ToolAdmissionPolicy;

/// One recorded provider answer.
#[derive(Clone, Debug)]
pub(super) struct Fixture {
    status: u16,
    content_type: String,
    retry_after: Option<String>,
    body: Vec<u8>,
}

pub(super) fn fixture(provider: &str, name: &str) -> Fixture {
    let text = match (provider, name) {
        ("tavily", "web_search_ok") => include_str!("fixtures/web/tavily/web_search_ok.json"),
        ("tavily", "web_search_empty") => include_str!("fixtures/web/tavily/web_search_empty.json"),
        ("tavily", "web_fetch_ok") => include_str!("fixtures/web/tavily/web_fetch_ok.json"),
        ("tavily", "error_401") => include_str!("fixtures/web/tavily/error_401.json"),
        ("tavily", "error_429") => include_str!("fixtures/web/tavily/error_429.json"),
        ("firecrawl", "web_search_ok") => include_str!("fixtures/web/firecrawl/web_search_ok.json"),
        ("firecrawl", "web_search_empty") => {
            include_str!("fixtures/web/firecrawl/web_search_empty.json")
        }
        ("firecrawl", "web_fetch_ok") => include_str!("fixtures/web/firecrawl/web_fetch_ok.json"),
        ("firecrawl", "error_401") => include_str!("fixtures/web/firecrawl/error_401.json"),
        ("firecrawl", "error_429") => include_str!("fixtures/web/firecrawl/error_429.json"),
        ("brave", "web_search_ok") => include_str!("fixtures/web/brave/web_search_ok.json"),
        ("brave", "web_search_empty") => include_str!("fixtures/web/brave/web_search_empty.json"),
        ("brave", "error_401") => include_str!("fixtures/web/brave/error_401.json"),
        ("brave", "error_429") => include_str!("fixtures/web/brave/error_429.json"),
        ("ddg_instant", "web_search_ok") => {
            include_str!("fixtures/web/ddg_instant/web_search_ok.json")
        }
        ("ddg_instant", "web_search_empty") => {
            include_str!("fixtures/web/ddg_instant/web_search_empty.json")
        }
        ("ddg_instant", "error_401") => include_str!("fixtures/web/ddg_instant/error_401.json"),
        ("ddg_instant", "error_429") => include_str!("fixtures/web/ddg_instant/error_429.json"),
        ("shodan", "host_lookup_ok") => include_str!("fixtures/web/shodan/host_lookup_ok.json"),
        ("shodan", "host_search_ok") => include_str!("fixtures/web/shodan/host_search_ok.json"),
        ("shodan", "host_search_empty") => {
            include_str!("fixtures/web/shodan/host_search_empty.json")
        }
        ("shodan", "host_count_ok") => include_str!("fixtures/web/shodan/host_count_ok.json"),
        ("shodan", "error_401") => include_str!("fixtures/web/shodan/error_401.json"),
        ("shodan", "error_429") => include_str!("fixtures/web/shodan/error_429.json"),
        _ => panic!("no fixture {provider}/{name}"),
    };
    let value: Value = serde_json::from_str(text).expect("fixture JSON");
    Fixture {
        status: u16::try_from(value["status"].as_u64().expect("status")).expect("status"),
        content_type: value["content_type"]
            .as_str()
            .expect("content type")
            .to_owned(),
        retry_after: value
            .pointer("/headers/retry-after")
            .and_then(Value::as_str)
            .map(str::to_owned),
        body: serde_json::to_vec(&value["body"]).expect("body"),
    }
}

/// A provider transport that answers fixtures in order (the last one
/// repeats) and records every request it was asked to send.
#[derive(Default)]
pub(super) struct FixtureTransport {
    answers: Mutex<VecDeque<Fixture>>,
    pub(super) requests: Mutex<Vec<(String, String, Option<Value>)>>,
}

impl FixtureTransport {
    pub(super) fn answering(answers: &[Fixture]) -> Arc<Self> {
        Arc::new(Self {
            answers: Mutex::new(answers.iter().cloned().collect()),
            requests: Mutex::default(),
        })
    }

    pub(super) fn sent(&self) -> Vec<(String, String, Option<Value>)> {
        self.requests.lock().expect("requests").clone()
    }

    pub(super) fn count(&self) -> usize {
        self.requests.lock().expect("requests").len()
    }
}

#[async_trait]
impl Transport for FixtureTransport {
    async fn send(&self, request: Request, _max_bytes: usize) -> Result<HttpAnswer, ProviderError> {
        let body = request
            .body()
            .and_then(reqwest::Body::as_bytes)
            .and_then(|bytes| serde_json::from_slice(bytes).ok());
        self.requests.lock().expect("requests").push((
            request.method().to_string(),
            request.url().to_string(),
            body,
        ));
        let mut answers = self.answers.lock().expect("answers");
        let answer = if answers.len() > 1 {
            answers.pop_front()
        } else {
            answers.front().cloned()
        }
        .expect("a fixture answer");
        Ok(HttpAnswer {
            status: StatusCode::from_u16(answer.status).expect("status"),
            content_type: Some(answer.content_type),
            retry_after_secs: answer.retry_after.and_then(|value| value.parse().ok()),
            body: answer.body,
        })
    }
}

/// A meter that records what it was asked, and refuses as told.
#[derive(Default)]
pub(super) struct FakeMeter {
    pub(super) admits: Mutex<Vec<MeteredOp>>,
    pub(super) settles: Mutex<Vec<SettleRequest>>,
    pub(super) admit_error: Mutex<Option<MeterError>>,
    /// Settles answered `Unavailable` before one succeeds (`u32::MAX`: never).
    pub(super) settle_failures: AtomicU32,
    pub(super) price_nano: Mutex<i64>,
}

impl FakeMeter {
    pub(super) fn new() -> Arc<Self> {
        let meter = Self::default();
        *meter.price_nano.lock().expect("price") = 8_000_000;
        Arc::new(meter)
    }

    pub(super) fn refusing(error: MeterError) -> Arc<Self> {
        let meter = Self::new();
        *meter.admit_error.lock().expect("admit error") = Some(error);
        meter
    }

    pub(super) fn settled(&self) -> Vec<SettleRequest> {
        self.settles.lock().expect("settles").clone()
    }

    pub(super) fn admitted(&self) -> Vec<MeteredOp> {
        self.admits.lock().expect("admits").clone()
    }
}

#[async_trait]
impl Meter for FakeMeter {
    fn attribution_execution_id(&self) -> &'static str {
        "execution-1"
    }

    async fn admit(&self, op: &MeteredOp) -> Result<Admission, MeterError> {
        self.admits.lock().expect("admits").push(*op);
        if let Some(error) = self.admit_error.lock().expect("admit error").clone() {
            return Err(error);
        }
        let unit = if op.estimate.credits > 0 || op.provider == "shodan" {
            "credit"
        } else if op.estimate.pages > 0 {
            "page"
        } else {
            "request"
        };
        Ok(Admission {
            admission_id: format!("admission-{}-{}", op.provider, op.operation),
            unit: unit.to_owned(),
            nano_usd_per_unit: *self.price_nano.lock().expect("price"),
        })
    }

    async fn settle(&self, request: &SettleRequest) -> Result<Settlement, MeterError> {
        self.settles.lock().expect("settles").push(request.clone());
        let left = self.settle_failures.load(Ordering::SeqCst);
        if left > 0 {
            if left != u32::MAX {
                self.settle_failures.store(left - 1, Ordering::SeqCst);
            }
            return Err(MeterError::Unavailable);
        }
        Ok(Settlement {
            billed_nano_usd: 8_000_000,
            duplicate: false,
        })
    }
}

/// A DNS answering fixed names; `rebind` answers a public address once,
/// then a private one.
pub(super) struct FakeLookup {
    names: HashMap<&'static str, Vec<IpAddr>>,
    rebind_calls: AtomicUsize,
}

impl FakeLookup {
    pub(super) fn new() -> Arc<Self> {
        let ip = |text: &str| text.parse::<IpAddr>().expect("address");
        Arc::new(Self {
            names: HashMap::from([
                ("docs.example.com", vec![ip("127.0.0.1")]),
                ("metadata.example.com", vec![ip("169.254.169.254")]),
                ("private.example.com", vec![ip("10.0.0.7")]),
            ]),
            rebind_calls: AtomicUsize::new(0),
        })
    }
}

impl Lookup for FakeLookup {
    fn lookup(&self, host: String) -> LookupFuture {
        let answer = if host == "rebind.example.com" {
            let call = self.rebind_calls.fetch_add(1, Ordering::SeqCst);
            Some(vec![if call == 0 {
                "93.184.216.34".parse().expect("address")
            } else {
                "10.0.0.8".parse().expect("address")
            }])
        } else {
            self.names.get(host.as_str()).cloned()
        };
        Box::pin(async move { answer.ok_or_else(|| io::Error::other("no such host")) })
    }
}

pub(super) fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy"))
}

/// The host: the fake meter, an allowlist that lifts loopback (the test
/// origin) but nothing else, the fake DNS.
pub(super) fn host(meter: &Arc<FakeMeter>) -> WebToolHost {
    WebToolHost::new(
        Arc::clone(meter) as Arc<dyn Meter>,
        Allowlist::parse_csv("127.0.0.1").expect("allowlist"),
        "https://elitea.example",
    )
    .with_lookup(FakeLookup::new())
}

pub(super) fn settings(value: &Value) -> Map<String, Value> {
    value.as_object().expect("settings object").clone()
}

pub(super) fn web_toolset(
    value: &Value,
    meter: Option<&Arc<FakeMeter>>,
    transport: &Arc<FixtureTransport>,
    extra_fetch_ports: Vec<u16>,
) -> BasicToolset {
    web_toolset_under(value, meter, transport, extra_fetch_ports, &policy())
}

/// [`web_toolset`] under another toolkit policy (a run's egress policy).
pub(super) fn web_toolset_under(
    value: &Value,
    meter: Option<&Arc<FakeMeter>>,
    transport: &Arc<FixtureTransport>,
    extra_fetch_ports: Vec<u16>,
    policy: &Arc<ToolAdmissionPolicy>,
) -> BasicToolset {
    let host = meter.map(host);
    let factory = |_: &RegistryEntry| Arc::clone(transport) as Arc<dyn Transport>;
    build_web_with(
        "web",
        WebConfig::parse(&settings(value)).expect("web settings"),
        policy,
        &Parts {
            host: host.as_ref(),
            transports: &factory,
            extra_fetch_ports,
            // The loopback test origin; production lifts nothing.
            fetch_permits: Allowlist::parse_csv("127.0.0.1")
                .expect("allowlist")
                .private_permits(),
        },
    )
    .expect("web toolset")
}

pub(super) fn host_intel_toolset(
    value: &Value,
    meter: Option<&Arc<FakeMeter>>,
    transport: &Arc<FixtureTransport>,
) -> BasicToolset {
    let host = meter.map(host);
    let factory = |_: &RegistryEntry| Arc::clone(transport) as Arc<dyn Transport>;
    build_host_intel_with(
        "hosts",
        &HostIntelConfig::parse(&settings(value)).expect("host_intel settings"),
        &policy(),
        &Parts {
            host: host.as_ref(),
            transports: &factory,
            extra_fetch_ports: Vec::new(),
            fetch_permits: elitea_egress_guard::PermitSet::default(),
        },
    )
    .expect("host_intel toolset")
}

pub(super) fn context(call: &str) -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("web-tool-test")
            .with_session_id("session-1")
            .with_function_call_id(call),
    )
}

pub(super) async fn tools(toolset: &BasicToolset) -> Vec<Arc<dyn Tool>> {
    let readonly: Arc<dyn ReadonlyContext> = context("list");
    toolset.tools(readonly).await.expect("tools")
}

pub(super) async fn tool(toolset: &BasicToolset, name: &str) -> Arc<dyn Tool> {
    tools(toolset)
        .await
        .into_iter()
        .find(|tool| tool.name() == name)
        .unwrap_or_else(|| panic!("no tool {name}"))
}

/// Call a tool and return its result value (an error result included).
pub(super) async fn call(toolset: &BasicToolset, name: &str, arguments: Value) -> Value {
    tool(toolset, name)
        .await
        .execute(context("call-1"), arguments)
        .await
        .expect("a tool result")
}

/// The `web` settings with every web provider enabled.
pub(super) fn all_web_providers() -> Value {
    json!({
        "providers": {
            "tavily": {"api_key": "tvly-fixture"},
            "firecrawl": {"api_key": "fc-fixture"},
            "brave": {"api_key": "brave-fixture"},
            "ddg_instant": {}
        },
        "default_search_provider": "tavily",
        "default_fetch_backend": "direct"
    })
}

/// A local plain-HTTP origin answering a few fixed paths; returns its port
/// and a counter of the requests it served.
pub(super) async fn origin() -> (u16, Arc<AtomicUsize>) {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("listener");
    let port = listener.local_addr().expect("address").port();
    let served = Arc::new(AtomicUsize::new(0));
    let counter = Arc::clone(&served);
    tokio::spawn(async move {
        loop {
            let Ok((mut stream, _)) = listener.accept().await else {
                return;
            };
            let counter = Arc::clone(&counter);
            tokio::spawn(async move {
                let mut buffer = vec![0_u8; 8_192];
                let mut read = 0;
                while !buffer[..read]
                    .windows(4)
                    .any(|window| window == b"\r\n\r\n")
                {
                    match stream.read(&mut buffer[read..]).await {
                        Ok(0) | Err(_) => return,
                        Ok(n) => read += n,
                    }
                }
                counter.fetch_add(1, Ordering::SeqCst);
                let request = String::from_utf8_lossy(&buffer[..read]).into_owned();
                let path = request.split_whitespace().nth(1).unwrap_or("/").to_owned();
                if path == "/redirect-stall" {
                    // A 302 that announces a 1 MiB body, sends 16 bytes of
                    // it and stalls: a client that reads redirect bodies
                    // hangs here until its own timeout.
                    let head = "HTTP/1.1 302 Found\r\nLocation: /page\r\nContent-Type: text/plain\r\nContent-Length: 1048576\r\n\r\n";
                    let _ = stream.write_all(head.as_bytes()).await;
                    let _ = stream.write_all(&[b'r'; 16]).await;
                    tokio::time::sleep(std::time::Duration::from_mins(2)).await;
                    return;
                }
                let (status, headers, body): (&str, String, Vec<u8>) = match path.as_str() {
                    "/page" => (
                        "200 OK",
                        "Content-Type: text/html; charset=utf-8\r\n".to_owned(),
                        b"<html><head><title>Tokio &amp; friends</title><script>steal()</script><style>p{}</style></head><body><nav>Menu Home About</nav><main><h1>Changelog</h1><p>Fixed a race.</p><form><input name=q></form></main></body></html>".to_vec(),
                    ),
                    "/redirect-metadata" => (
                        "302 Found",
                        "Location: http://169.254.169.254/latest/meta-data/\r\n".to_owned(),
                        Vec::new(),
                    ),
                    "/redirect-metadata-name" => (
                        "302 Found",
                        format!("Location: http://metadata.example.com:{port}/page\r\n"),
                        Vec::new(),
                    ),
                    "/redirect-ok" => ("302 Found", "Location: /page\r\n".to_owned(), Vec::new()),
                    "/loop" => ("302 Found", "Location: /loop\r\n".to_owned(), Vec::new()),
                    "/big" => (
                        "200 OK",
                        "Content-Type: text/plain\r\n".to_owned(),
                        vec![b'a'; 6 * 1_024 * 1_024],
                    ),
                    "/pdf" => (
                        "200 OK",
                        "Content-Type: application/pdf\r\n".to_owned(),
                        b"%PDF-1.7".to_vec(),
                    ),
                    _ => ("404 Not Found", String::new(), Vec::new()),
                };
                let head = format!(
                    "HTTP/1.1 {status}\r\n{headers}Content-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                );
                let _ = stream.write_all(head.as_bytes()).await;
                let _ = stream.write_all(&body).await;
                let _ = stream.shutdown().await;
            });
        }
    });
    (port, served)
}
