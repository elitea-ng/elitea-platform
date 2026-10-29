//! A run's public egress policy and its ledger (ADR-0044 decision 2, #1490).
//!
//! Main puts the run's policy on the signed command
//! (`WorkerCommandV1.egress_policy`): open (any public host, the default) or
//! an allowlist in the egresslib grammar. The host builds one [`RunEgress`]
//! per execution and lends it to the toolkit policy
//! ([`crate::toolkits::ToolAdmissionPolicy::with_egress`]); everything a run's
//! tools send goes through it:
//!
//! - [`HttpClient`] is the ONE reqwest client a toolkit family builds
//!   ([`client_builder`] + [`BuildForRun::build_for_run`]). It checks every
//!   request's host and port against the policy before sending, IP literals
//!   included (reqwest never asks a resolver about those), never follows a
//!   redirect on its own, and records the hosts reached. A source test keeps
//!   `reqwest::Client::builder()` out of the toolkit families.
//! - [`EgressTransport`] does the same for the provider clients the families
//!   share with the indexing connectors (`elitea-connectors`).
//! - A refused request never leaves the worker. The tool call that made it
//!   fails with [`EGRESS_HOST_NOT_ALLOWED`] naming the host and port (not the
//!   URL), whatever error the family mapped the refusal to.
//! - The ledger counts reached and refused hosts, at most
//!   [`MAX_SUMMARY_HOSTS`] of each; the host reports it to Main before
//!   settlement ([`RunEgress::summary`]).
//!
//! The SSRF rule is not this module's: the web fetch keeps its guarded
//! resolver, and the families keep reaching the hosts they reached before
//! (an on-premises Jira on a private address included) unless an allowlist
//! leaves them out. The worker's own gRPC to Main and the model gateway never
//! goes through here.

use std::collections::{BTreeMap, HashSet};
use std::fmt;
use std::future::Future;
use std::sync::{Arc, Mutex, OnceLock};

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use elitea_connectors::transport::{Request, Response, Transport, TransportError};
pub use elitea_egress_guard::{
    Allowlist, MAX_PUBLIC_ALLOWLIST_ENTRIES, PublicPolicy, PublicPolicyError,
};

/// The tool error code of a refused host.
pub const EGRESS_HOST_NOT_ALLOWED: &str = "egress_host_not_allowed";
/// The refusal reason a summary reports (`EgressHostRefusedV1.reason`).
pub const REFUSAL_REASON_NOT_ALLOWED: &str = "host_not_allowed";
/// The most hosts each summary list carries; the rest are counted in
/// [`EgressSummary::other_hosts`].
pub const MAX_SUMMARY_HOSTS: usize = 256;
/// How many distinct overflowing hosts are remembered so a repeat is not
/// counted twice; past it every overflowing request counts.
const MAX_OVERFLOW_KEYS: usize = 4_096;
/// The ledger's name for a refused URL that has no host.
pub const INVALID_HOST: &str = "<invalid>";
/// A DNS name is at most 253 bytes; a longer host is cut, never stored whole.
const MAX_HOST_BYTES: usize = 253;

/// A host the run's policy refused. Carries the host and port only, never
/// the URL (its path and query may hold a key).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EgressRefused {
    host: String,
    port: u16,
}

impl EgressRefused {
    #[must_use]
    pub fn host(&self) -> &str {
        &self.host
    }

    #[must_use]
    pub const fn port(&self) -> u16 {
        self.port
    }

    /// `host:port`, an IPv6 literal bracketed.
    #[must_use]
    pub fn authority(&self) -> String {
        if self.host.contains(':') {
            format!("[{}]:{}", self.host, self.port)
        } else {
            format!("{}:{}", self.host, self.port)
        }
    }

    /// The sentence the model and the run read.
    #[must_use]
    pub fn message(&self) -> String {
        format!(
            "egress to {} is not allowed: the host is not on this run's egress allowlist; ask an administrator to add it",
            self.authority()
        )
    }

    /// The tool error a refused call fails with.
    #[must_use]
    pub fn tool_error(&self) -> AdkError {
        AdkError::new(
            ErrorComponent::Guardrail,
            ErrorCategory::Forbidden,
            EGRESS_HOST_NOT_ALLOWED,
            self.message(),
        )
        .with_retry(RetryHint {
            should_retry: false,
            retry_after_ms: None,
            max_attempts: Some(1),
        })
    }
}

impl fmt::Display for EgressRefused {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.message())
    }
}

impl std::error::Error for EgressRefused {}

/// One host in a summary.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct HostCount {
    pub host: String,
    pub port: u16,
    /// Requests that got a response (reached) or were refused.
    pub count: u64,
}

/// A cumulative snapshot of one run's ledger.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct EgressSummary {
    pub reached: Vec<HostCount>,
    pub refused: Vec<HostCount>,
    /// Distinct hosts that did not fit either list.
    pub other_hosts: u64,
}

impl EgressSummary {
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.reached.is_empty() && self.refused.is_empty() && self.other_hosts == 0
    }
}

#[derive(Default)]
struct Ledger {
    reached: BTreeMap<(String, u16), u64>,
    refused: BTreeMap<(String, u16), u64>,
    overflow: HashSet<(bool, String, u16)>,
    other_hosts: u64,
}

impl Ledger {
    fn count(&mut self, refused: bool, host: String, port: u16) {
        let list = if refused {
            &mut self.refused
        } else {
            &mut self.reached
        };
        if let Some(count) = list.get_mut(&(host.clone(), port)) {
            *count = count.saturating_add(1);
            return;
        }
        if list.len() < MAX_SUMMARY_HOSTS {
            list.insert((host, port), 1);
            return;
        }
        if self.overflow.len() < MAX_OVERFLOW_KEYS {
            if self.overflow.insert((refused, host, port)) {
                self.other_hosts = self.other_hosts.saturating_add(1);
            }
        } else if !self.overflow.contains(&(refused, host, port)) {
            self.other_hosts = self.other_hosts.saturating_add(1);
        }
    }
}

/// One execution's public egress policy and what it reached and refused.
pub struct RunEgress {
    policy: PublicPolicy,
    ledger: Mutex<Ledger>,
}

impl fmt::Debug for RunEgress {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("RunEgress")
            .field("open", &self.policy.is_open())
            .finish_non_exhaustive()
    }
}

impl RunEgress {
    #[must_use]
    pub fn new(policy: PublicPolicy) -> Arc<Self> {
        Arc::new(Self {
            policy,
            ledger: Mutex::new(Ledger::default()),
        })
    }

    /// An open run: every public host, today's behaviour.
    #[must_use]
    pub fn open() -> Arc<Self> {
        Self::new(PublicPolicy::Open)
    }

    /// No public host at all: an empty allowlist. What a client gets when it
    /// is built outside any run ([`current`]), and the default of a holder
    /// that was never handed its run's policy.
    #[must_use]
    pub fn closed() -> Arc<Self> {
        Self::new(PublicPolicy::Allowlist(Allowlist::default()))
    }

    #[must_use]
    pub const fn policy(&self) -> &PublicPolicy {
        &self.policy
    }

    /// Whether `host` on `port` may be reached. A refusal is counted, and
    /// named to the tool call in progress (see `attributed`).
    ///
    /// # Errors
    ///
    /// [`EgressRefused`] for a host the policy does not name.
    pub fn check(&self, host: &str, port: u16) -> Result<(), EgressRefused> {
        if self.policy.admits(host, port) {
            return Ok(());
        }
        let refused = EgressRefused {
            host: ledger_host(host),
            port,
        };
        self.count(true, refused.host.clone(), port);
        let _ = CALL.try_with(|call| call.note(&refused));
        Err(refused)
    }

    /// [`Self::check`] for a URL's host and port. A URL without a host, or
    /// with no port its scheme implies, is refused under an allowlist, and
    /// counted like any refusal: a host-less URL as [`INVALID_HOST`], port 0
    /// when the scheme implies none.
    ///
    /// # Errors
    ///
    /// [`EgressRefused`] for a host the policy does not name.
    pub fn check_url(&self, url: &reqwest::Url) -> Result<(), EgressRefused> {
        let host = url.host_str().unwrap_or_default();
        let port = url.port_or_known_default().unwrap_or(0);
        if host.is_empty() || port == 0 {
            if self.policy.is_open() {
                return Ok(());
            }
            let host = ledger_host(host);
            let refused = EgressRefused {
                host: if host.is_empty() {
                    INVALID_HOST.to_owned()
                } else {
                    host
                },
                port,
            };
            self.count(true, refused.host.clone(), port);
            let _ = CALL.try_with(|call| call.note(&refused));
            return Err(refused);
        }
        self.check(host, port)
    }

    /// Count one request to `url` that got a response.
    pub fn record_reached(&self, url: &reqwest::Url) {
        if let (Some(host), Some(port)) = (url.host_str(), url.port_or_known_default()) {
            self.count(false, ledger_host(host), port);
        }
    }

    fn count(&self, refused: bool, host: String, port: u16) {
        if let Ok(mut ledger) = self.ledger.lock() {
            ledger.count(refused, host, port);
        }
    }

    /// The cumulative snapshot the host reports to Main.
    #[must_use]
    pub fn summary(&self) -> EgressSummary {
        let Ok(ledger) = self.ledger.lock() else {
            return EgressSummary::default();
        };
        let list = |map: &BTreeMap<(String, u16), u64>| {
            map.iter()
                .map(|((host, port), count)| HostCount {
                    host: host.clone(),
                    port: *port,
                    count: *count,
                })
                .collect()
        };
        EgressSummary {
            reached: list(&ledger.reached),
            refused: list(&ledger.refused),
            other_hosts: ledger.other_hosts,
        }
    }
}

/// Lower-cased, without brackets or a root dot, at most 253 bytes.
fn ledger_host(host: &str) -> String {
    let host = host
        .strip_prefix('[')
        .and_then(|rest| rest.strip_suffix(']'))
        .unwrap_or(host);
    let host = host.strip_suffix('.').unwrap_or(host);
    let mut host = host.to_ascii_lowercase();
    if host.len() > MAX_HOST_BYTES {
        let mut cut = MAX_HOST_BYTES;
        while !host.is_char_boundary(cut) {
            cut -= 1;
        }
        host.truncate(cut);
    }
    host
}

/// The first refusal one tool call met.
#[derive(Default)]
struct CallRefusal(Mutex<Option<EgressRefused>>);

impl CallRefusal {
    fn note(&self, refused: &EgressRefused) {
        if let Ok(mut first) = self.0.lock() {
            first.get_or_insert_with(|| refused.clone());
        }
    }

    fn take(&self) -> Option<EgressRefused> {
        self.0.lock().ok().and_then(|mut first| first.take())
    }
}

tokio::task_local! {
    /// The run whose clients are being built (materialization) or whose tool
    /// is running.
    static CURRENT: Arc<RunEgress>;
    /// The tool call in progress, to name its refusal.
    static CALL: Arc<CallRefusal>;
}

/// Whether a client built outside any run reaches every public host. Only
/// this crate's own unit tests, which build family clients directly against
/// local fixtures; every other build fails CLOSED (#1490 review).
const UNSCOPED_IS_OPEN: bool = cfg!(test);

fn shared_open() -> Arc<RunEgress> {
    static OPEN: OnceLock<Arc<RunEgress>> = OnceLock::new();
    Arc::clone(OPEN.get_or_init(RunEgress::open))
}

/// The policy of a client built outside any run: closed (no public host),
/// and logged, since every path that sends for a run must name it — through
/// materialization's [`scope`], a tool call's `attributed`, or an explicit
/// [`RunEgress`]. A task-local does not cross `tokio::spawn`, so a missing
/// scope is a bug this refuses instead of silently opening.
fn unscoped(open: bool) -> Arc<RunEgress> {
    if open {
        return shared_open();
    }
    tracing::warn!(
        event = "egress_unscoped_client",
        "an HTTP client was built outside any run's egress scope; it reaches no public host"
    );
    RunEgress::closed()
}

/// The run in scope. Outside any run a client reaches NO public host
/// (fail-closed): a path that sends on behalf of a run must put it in scope
/// or hand it over explicitly.
#[must_use]
pub fn current() -> Arc<RunEgress> {
    CURRENT
        .try_with(Arc::clone)
        .unwrap_or_else(|_| unscoped(UNSCOPED_IS_OPEN))
}

/// Run `future` with `egress` as the run in scope: every client built inside
/// it is bound to `egress`.
pub async fn scope<F: Future>(egress: Arc<RunEgress>, future: F) -> F::Output {
    CURRENT.scope(egress, future).await
}

/// [`scope`] for synchronous code: every client built inside `build` is
/// bound to `egress` (a constructor that is not async, run at assembly).
pub fn scope_sync<R>(egress: Arc<RunEgress>, build: impl FnOnce() -> R) -> R {
    CURRENT.sync_scope(egress, build)
}

/// The first refusal the tool call in progress met, if any (the call keeps
/// it; [`attributed`] still reports it).
#[must_use]
pub(crate) fn call_refusal() -> Option<EgressRefused> {
    CALL.try_with(|call| call.0.lock().ok().and_then(|first| first.clone()))
        .ok()
        .flatten()
}

/// Run one tool call under `egress`, and return the first refusal it met.
pub(crate) async fn attributed<F: Future>(
    egress: Arc<RunEgress>,
    future: F,
) -> (F::Output, Option<EgressRefused>) {
    let call = Arc::new(CallRefusal::default());
    let output = CALL
        .scope(Arc::clone(&call), CURRENT.scope(egress, future))
        .await;
    (output, call.take())
}

/// The builder every toolkit family starts its client from.
pub fn client_builder() -> reqwest::ClientBuilder {
    reqwest::Client::builder().redirect(reqwest::redirect::Policy::none())
}

/// Finish a client bound to the run in scope.
pub trait BuildForRun {
    /// Build the client, bound to [`current`]. Redirects are never followed
    /// by reqwest: a hop would bypass the per-request check.
    ///
    /// # Errors
    ///
    /// reqwest could not build the client.
    fn build_for_run(self) -> reqwest::Result<HttpClient>;
}

impl BuildForRun for reqwest::ClientBuilder {
    fn build_for_run(self) -> reqwest::Result<HttpClient> {
        Ok(HttpClient {
            client: self.redirect(reqwest::redirect::Policy::none()).build()?,
            refusal: refusal_client()?,
            egress: current(),
        })
    }
}

/// A reqwest client bound to one run's policy. Only [`Self::execute`]
/// sends; the inner client is never handed out.
#[derive(Clone)]
pub struct HttpClient {
    client: reqwest::Client,
    refusal: reqwest::Client,
    egress: Arc<RunEgress>,
}

impl fmt::Debug for HttpClient {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("HttpClient")
            .field("egress", &self.egress)
            .finish_non_exhaustive()
    }
}

impl HttpClient {
    /// A request builder on the inner client. Send what it builds with
    /// [`Self::execute`]; `RequestBuilder::send` would skip the check.
    pub fn request(&self, method: reqwest::Method, url: reqwest::Url) -> reqwest::RequestBuilder {
        self.client.request(method, url)
    }

    /// The run this client is bound to.
    #[must_use]
    pub fn egress(&self) -> &Arc<RunEgress> {
        &self.egress
    }

    /// Send `request` if the run's policy admits its host and port.
    ///
    /// # Errors
    ///
    /// reqwest's error; a refused request is a connect error that never left
    /// the worker ([`is_refusal`] tells it apart), and the tool call fails
    /// with [`EGRESS_HOST_NOT_ALLOWED`].
    pub async fn execute(&self, request: reqwest::Request) -> reqwest::Result<reqwest::Response> {
        if self.egress.check_url(request.url()).is_err() {
            return Err(self.refusal_error().await);
        }
        let url = request.url().clone();
        let response = self.client.execute(request).await?;
        self.egress.record_reached(&url);
        Ok(response)
    }

    /// Send `request` to an endpoint the OPERATOR configured for this
    /// worker, which Main cannot list in a run's allowlist: the run's policy
    /// is not consulted, and the host is still recorded as reached. Only for
    /// a URL whose exact origin (scheme, host, port) the caller matched
    /// against the worker's own configuration — never a URL a model, a user
    /// or a toolkit setting chose.
    ///
    /// # Errors
    ///
    /// reqwest's error.
    pub(crate) async fn execute_operator_origin(
        &self,
        request: reqwest::Request,
    ) -> reqwest::Result<reqwest::Response> {
        let url = request.url().clone();
        let response = self.client.execute(request).await?;
        self.egress.record_reached(&url);
        Ok(response)
    }

    /// `reqwest::Error` has no public constructor, and every family maps
    /// reqwest's error type, so a refusal is minted as the error of a request
    /// that cannot leave the worker: a client whose resolver refuses every
    /// name, asked for a reserved `.invalid` name. No socket is opened.
    async fn refusal_error(&self) -> reqwest::Error {
        match self.refusal.get(REFUSAL_URL).send().await {
            Err(error) => error.without_url(),
            Ok(_) => unreachable!("the refusal client's resolver refuses every name"),
        }
    }
}

const REFUSAL_URL: &str = "http://egress-refused.invalid/";

/// The marker a refusal's error chain carries.
#[derive(Debug)]
struct RefusedMarker;

impl fmt::Display for RefusedMarker {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(EGRESS_HOST_NOT_ALLOWED)
    }
}

impl std::error::Error for RefusedMarker {}

/// A resolver that refuses every name, so the refusal client never dials.
struct RefuseAll;

impl reqwest::dns::Resolve for RefuseAll {
    fn resolve(&self, _name: reqwest::dns::Name) -> reqwest::dns::Resolving {
        Box::pin(async { Err(Box::new(RefusedMarker) as Box<dyn std::error::Error + Send + Sync>) })
    }
}

/// The one refusal client, built on first use. No proxy (a proxy would
/// resolve the name instead) and no redirect.
fn refusal_client() -> reqwest::Result<reqwest::Client> {
    static CLIENT: OnceLock<reqwest::Client> = OnceLock::new();
    if let Some(client) = CLIENT.get() {
        return Ok(client.clone());
    }
    let client = reqwest::Client::builder()
        .no_proxy()
        .redirect(reqwest::redirect::Policy::none())
        .dns_resolver(Arc::new(RefuseAll))
        .build()?;
    Ok(CLIENT.get_or_init(|| client).clone())
}

/// Whether a reqwest error is a refusal by the run's egress policy.
#[must_use]
pub fn is_refusal(error: &reqwest::Error) -> bool {
    let mut source: Option<&(dyn std::error::Error + 'static)> = Some(error);
    while let Some(current) = source {
        if current.is::<RefusedMarker>() {
            return true;
        }
        source = current.source();
    }
    false
}

/// A connectors [`Transport`] bound to one run's policy: refuses a request
/// whose host the policy does not name with
/// [`TransportError::egress_refused`], and records the hosts reached.
pub struct EgressTransport {
    egress: Arc<RunEgress>,
    inner: Arc<dyn Transport>,
}

impl EgressTransport {
    /// Bind `inner` to the run in scope ([`current`]).
    #[must_use]
    pub fn for_run(inner: Arc<dyn Transport>) -> Self {
        Self::new(current(), inner)
    }

    #[must_use]
    pub fn new(egress: Arc<RunEgress>, inner: Arc<dyn Transport>) -> Self {
        Self { egress, inner }
    }
}

#[async_trait]
impl Transport for EgressTransport {
    async fn execute(&self, request: Request) -> Result<Response, TransportError> {
        if self.egress.check_url(request.url()).is_err() {
            return Err(TransportError::egress_refused());
        }
        let url = request.url().clone();
        let response = self.inner.execute(request).await?;
        self.egress.record_reached(&url);
        Ok(response)
    }
}

#[cfg(test)]
#[path = "egress_tests.rs"]
mod tests;
