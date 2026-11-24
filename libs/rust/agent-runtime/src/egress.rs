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
//! The SSRF rule applies under every policy (#2950). Every client built here
//! resolves through the egress guard ([`elitea_egress_guard`]): each answer
//! is classified, a private, loopback, link-local, CGNAT, multicast or
//! metadata address is refused, and the connection goes to an address that
//! was checked (no second lookup, so DNS rebinding changes nothing). An IP
//! literal, which reqwest never hands a resolver, is classified before
//! sending. The operator's private permit ([`PrivatePermit`], from Main's
//! `worker_egress` section on the signed command) lifts the rule for the
//! on-premises hosts it names; a metadata or link-local address stays
//! refused even then. Such a refusal fails the tool call with
//! [`EGRESS_HOST_NOT_ALLOWED`] and is counted with the reason
//! [`REFUSAL_REASON_NOT_PUBLIC`]. The resolver is reusable by any client that
//! sends for a run: [`client_builder`] (reqwest 0.12), `mcp_client_builder`
//! (reqwest 0.13), `guarded_http_connector` (hyper) and
//! [`hardened_transport`] (the connectors' transport), each read through
//! [`RunEgress::guarded`]. The web fetch keeps its own guarded resolver. The
//! worker's own gRPC to Main and the model gateway never goes through here.

use std::collections::{BTreeMap, HashSet};
use std::fmt;
use std::future::Future;
use std::net::{IpAddr, SocketAddr};
use std::pin::Pin;
use std::sync::{Arc, Mutex, OnceLock, RwLock};

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use elitea_connectors::transport::{Request, Response, Transport, TransportError};
pub use elitea_egress_guard::{
    Allowlist, AllowlistError, MAX_PRIVATE_PERMIT_ENTRIES, MAX_PUBLIC_ALLOWLIST_ENTRIES, PermitSet,
    PrivatePermit, PublicPolicy, PublicPolicyError,
};
use elitea_egress_guard::{Lookup, ResolveError, SystemLookup, resolve_permitted};

/// The tool error code of a refused host.
pub const EGRESS_HOST_NOT_ALLOWED: &str = "egress_host_not_allowed";
/// The refusal reason a summary reports (`EgressHostRefusedV1.reason`) for a
/// host the run's public policy does not name.
pub const REFUSAL_REASON_NOT_ALLOWED: &str = "host_not_allowed";
/// The refusal reason for a host that is (or resolves only to) a private,
/// loopback, link-local or metadata address the operator's private permit
/// does not name (#2950).
pub const REFUSAL_REASON_NOT_PUBLIC: &str = "not_public";
/// The refusal reason for a host whose answer holds more addresses than the
/// guard checks (`elitea_egress_guard`'s bound): a public host behind a large
/// DNS answer, which no private permit entry lifts.
pub const REFUSAL_REASON_TOO_MANY_ADDRESSES: &str = "too_many_addresses";

/// The ledger reason a failed address check reports.
const fn resolve_refusal_reason(error: ResolveError) -> Option<&'static str> {
    match error {
        ResolveError::NotPublic => Some(REFUSAL_REASON_NOT_PUBLIC),
        ResolveError::TooManyAddresses => Some(REFUSAL_REASON_TOO_MANY_ADDRESSES),
        ResolveError::Lookup | ResolveError::NotAllowed => None,
    }
}
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
    reason: &'static str,
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

    /// [`REFUSAL_REASON_NOT_ALLOWED`] or [`REFUSAL_REASON_NOT_PUBLIC`].
    #[must_use]
    pub const fn reason(&self) -> &'static str {
        self.reason
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
        if self.reason == REFUSAL_REASON_NOT_PUBLIC {
            return format!(
                "egress to {} is not allowed: the host is a private, local or metadata address; an administrator can permit an on-premises host in the worker egress private permit",
                self.authority()
            );
        }
        if self.reason == REFUSAL_REASON_TOO_MANY_ADDRESSES {
            return format!(
                "egress to {} is not allowed: the host resolves to more addresses than the worker checks",
                self.authority()
            );
        }
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
    /// Why a refused host was refused ([`REFUSAL_REASON_NOT_ALLOWED`] or
    /// [`REFUSAL_REASON_NOT_PUBLIC`]); empty for a reached host.
    pub reason: &'static str,
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
    /// Keyed by host, port and reason (empty for reached).
    reached: BTreeMap<(String, u16, &'static str), u64>,
    refused: BTreeMap<(String, u16, &'static str), u64>,
    overflow: HashSet<(bool, String, u16)>,
    other_hosts: u64,
}

impl Ledger {
    /// `reason` is `Some` for a refusal.
    fn count(&mut self, reason: Option<&'static str>, host: String, port: u16) {
        let refused = reason.is_some();
        let list = if refused {
            &mut self.refused
        } else {
            &mut self.reached
        };
        let key = (host, port, reason.unwrap_or_default());
        if let Some(count) = list.get_mut(&key) {
            *count = count.saturating_add(1);
            return;
        }
        if list.len() < MAX_SUMMARY_HOSTS {
            list.insert(key, 1);
            return;
        }
        let (host, port, _) = key;
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
///
/// It also carries the run's delegation assertions (ADR-0044 decision 4):
/// the outbound context every remote MCP call of the run reads.
pub struct RunEgress {
    policy: PublicPolicy,
    /// The operator's private permit (#2950), from the signed command.
    private: PrivatePermit,
    /// How names are resolved: the system resolver, a fake in tests.
    lookup: Arc<dyn Lookup>,
    ledger: Mutex<Ledger>,
    /// The `not_public` refusals already logged, so each is logged once.
    logged: Mutex<HashSet<(String, u16)>>,
    assertions: RwLock<Option<Arc<crate::remote_assertion::DelegationAssertions>>>,
}

impl fmt::Debug for RunEgress {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("RunEgress")
            .field("open", &self.policy.is_open())
            .field("private_permit", &!self.private.is_empty())
            .finish_non_exhaustive()
    }
}

/// The private permit a run gets when its command names none: nothing, so
/// every private address is refused. Only this crate's own unit tests, which
/// send to local fixtures on loopback, get loopback permitted; a test that
/// proves the refusal builds its run with [`RunEgress::with_private`].
fn default_private() -> PrivatePermit {
    if cfg!(test) {
        PrivatePermit::parse(["127.0.0.0/8", "::1"]).unwrap_or_default()
    } else {
        PrivatePermit::default()
    }
}

impl RunEgress {
    /// A run under `policy` with no private permit.
    #[must_use]
    pub fn new(policy: PublicPolicy) -> Arc<Self> {
        Self::configured(policy, default_private(), Arc::new(SystemLookup))
    }

    /// A run under `policy` whose private destinations are `private`.
    #[must_use]
    pub fn with_private(policy: PublicPolicy, private: PrivatePermit) -> Arc<Self> {
        Self::configured(policy, private, Arc::new(SystemLookup))
    }

    /// [`Self::with_private`] resolving names through `lookup` (a test's
    /// fake resolver; production uses the system resolver).
    #[must_use]
    pub fn configured(
        policy: PublicPolicy,
        private: PrivatePermit,
        lookup: Arc<dyn Lookup>,
    ) -> Arc<Self> {
        Arc::new(Self {
            policy,
            private,
            lookup,
            ledger: Mutex::new(Ledger::default()),
            logged: Mutex::new(HashSet::new()),
            assertions: RwLock::new(None),
        })
    }

    /// The operator's private permit.
    #[must_use]
    pub const fn private_permit(&self) -> &PrivatePermit {
        &self.private
    }

    /// Bind the delegation assertions of the current claim. A later bind
    /// replaces an earlier one: a `RunEgress` reused for a new claim of the
    /// same command mints under the new claim's lease, never the released
    /// one. The host binds them before it materializes the run's toolkits;
    /// a session materialized earlier keeps the assertions it was given.
    pub fn bind_assertions(&self, assertions: Arc<crate::remote_assertion::DelegationAssertions>) {
        *self
            .assertions
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = Some(assertions);
    }

    /// The delegation assertions of the current claim; `None` when the host
    /// bound none (remote calls then carry no assertion).
    #[must_use]
    pub fn assertions(&self) -> Option<Arc<crate::remote_assertion::DelegationAssertions>> {
        self.assertions
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .clone()
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
        Err(self.refuse(host, port, REFUSAL_REASON_NOT_ALLOWED))
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
            return Err(self.refuse(host, port, REFUSAL_REASON_NOT_ALLOWED));
        }
        self.check(host, port)
    }

    /// The SSRF rule for an IP-literal host, which no resolver is asked
    /// about: a private address the permit does not name, or any forbidden
    /// address, is refused (and counted as [`REFUSAL_REASON_NOT_PUBLIC`]). A
    /// name passes here; the guarded resolver judges what it resolves to.
    ///
    /// # Errors
    ///
    /// [`EgressRefused`] for a refused literal.
    pub fn check_literal(&self, url: &reqwest::Url) -> Result<(), EgressRefused> {
        self.check_literal_with(url, None)
    }

    fn check_literal_with(
        &self,
        url: &reqwest::Url,
        operator: Option<&PrivatePermit>,
    ) -> Result<(), EgressRefused> {
        let host = url.host_str().unwrap_or_default();
        let Ok(address) = host
            .strip_prefix('[')
            .and_then(|rest| rest.strip_suffix(']'))
            .unwrap_or(host)
            .parse::<IpAddr>()
        else {
            return Ok(());
        };
        let port = url.port_or_known_default();
        if self.permit_admits(operator, host, address, port) {
            return Ok(());
        }
        Err(self.refuse(host, port.unwrap_or(0), REFUSAL_REASON_NOT_PUBLIC))
    }

    fn permit_admits(
        &self,
        operator: Option<&PrivatePermit>,
        host: &str,
        address: IpAddr,
        port: Option<u16>,
    ) -> bool {
        self.private.admits(host, address, port)
            || operator.is_some_and(|operator| operator.admits(host, address, port))
    }

    /// The whole rule for one destination, before a client that does not
    /// resolve through [`RunEgress::guarded`] connects (the remote MCP
    /// session): the public policy, then every address the host resolves to.
    /// A lookup that fails is not a refusal; the connection then fails on
    /// its own.
    ///
    /// # Errors
    ///
    /// [`EgressRefused`] for a host the policy does not name, or one that
    /// resolves only to refused addresses.
    pub async fn check_destination(&self, url: &reqwest::Url) -> Result<(), EgressRefused> {
        self.check_url(url)?;
        let (Some(host), Some(port)) = (url.host_str(), url.port_or_known_default()) else {
            return Ok(());
        };
        match resolve_permitted(self.lookup.as_ref(), host, Some(port), &self.private).await {
            Err(error) => match resolve_refusal_reason(error) {
                Some(reason) => Err(self.refuse(host, port, reason)),
                None => Ok(()),
            },
            Ok(_) => Ok(()),
        }
    }

    /// Send under the SSRF rule: `send` is the request of a client built
    /// here ([`client_builder`], [`hardened_transport`],
    /// `guarded_http_connector`), whose resolver reads this run's permit
    /// and `url`'s port while `send` runs. An IP literal is judged before
    /// `send` starts.
    ///
    /// # Errors
    ///
    /// [`EgressRefused`] when the literal or every resolved address was
    /// refused; `send`'s own output (its error included) otherwise.
    pub async fn guarded<F: Future>(
        self: &Arc<Self>,
        url: &reqwest::Url,
        send: F,
    ) -> Result<F::Output, EgressRefused> {
        self.guarded_with(url, None, send).await
    }

    async fn guarded_with<F: Future>(
        self: &Arc<Self>,
        url: &reqwest::Url,
        operator: Option<Arc<PrivatePermit>>,
        send: F,
    ) -> Result<F::Output, EgressRefused> {
        self.check_literal_with(url, operator.as_deref())?;
        let dial = Dial {
            egress: Arc::clone(self),
            port: url.port_or_known_default(),
            operator,
            refused: Arc::new(Mutex::new(None)),
        };
        let refused = Arc::clone(&dial.refused);
        let output = DIAL.scope(dial, send).await;
        let first = refused.lock().ok().and_then(|mut first| first.take());
        match first {
            Some(refused) => Err(refused),
            None => Ok(output),
        }
    }

    /// Count and name a refusal; a `not_public` one is also logged, once
    /// per host and port in the run, with the host and port only.
    fn refuse(&self, host: &str, port: u16, reason: &'static str) -> EgressRefused {
        let host = ledger_host(host);
        let refused = EgressRefused {
            host: if host.is_empty() {
                INVALID_HOST.to_owned()
            } else {
                host
            },
            port,
            reason,
        };
        self.count(Some(reason), refused.host.clone(), port);
        let _ = CALL.try_with(|call| call.note(&refused));
        if reason == REFUSAL_REASON_NOT_PUBLIC && self.first_log(&refused) {
            tracing::warn!(
                event = "egress_not_public_refused",
                host = %refused.host,
                port = refused.port,
                "egress refused a private, local or metadata destination; add an on-premises host to the worker egress private permit"
            );
        }
        refused
    }

    fn first_log(&self, refused: &EgressRefused) -> bool {
        let Ok(mut logged) = self.logged.lock() else {
            return false;
        };
        if logged.len() >= MAX_SUMMARY_HOSTS {
            return false;
        }
        logged.insert((refused.host.clone(), refused.port))
    }

    /// Count one refusal that another enforcer made, with its reason: the
    /// egress proxy refused a host the run's cloud browser asked for
    /// (ADR-0049 decision 5, [`REFUSAL_REASON_NOT_ALLOWED`]). It is in the
    /// run's egress summary as any refusal is, and the tool call in progress
    /// keeps its own result (the browser contract's typed `egress_refused`).
    pub fn record_refused(&self, host: &str, port: u16, reason: &'static str) {
        let host = ledger_host(host);
        let host = if host.is_empty() {
            INVALID_HOST.to_owned()
        } else {
            host
        };
        self.count(Some(reason), host, port);
    }

    /// Count `refused` once more and hand it back: a call that repeats a
    /// refusal already decided (a remote MCP server refused at
    /// materialization).
    pub fn repeat_refusal(&self, refused: &EgressRefused) -> EgressRefused {
        self.count(Some(refused.reason), refused.host.clone(), refused.port);
        let _ = CALL.try_with(|call| call.note(refused));
        refused.clone()
    }

    /// Count one request to `url` that got a response.
    pub fn record_reached(&self, url: &reqwest::Url) {
        if let (Some(host), Some(port)) = (url.host_str(), url.port_or_known_default()) {
            self.count(None, ledger_host(host), port);
        }
    }

    /// Count one outbound connection to `host` on `port` that succeeded (for
    /// non-HTTP protocols such as SQL).
    pub fn record_reached_host(&self, host: &str, port: u16) {
        let host = ledger_host(host);
        if !host.is_empty() && port != 0 {
            self.count(None, host, port);
        }
    }

    fn count(&self, reason: Option<&'static str>, host: String, port: u16) {
        if let Ok(mut ledger) = self.ledger.lock() {
            ledger.count(reason, host, port);
        }
    }

    /// The cumulative snapshot the host reports to Main.
    #[must_use]
    pub fn summary(&self) -> EgressSummary {
        let Ok(ledger) = self.ledger.lock() else {
            return EgressSummary::default();
        };
        let list = |map: &BTreeMap<(String, u16, &'static str), u64>| {
            map.iter()
                .map(|((host, port, reason), count)| HostCount {
                    host: host.clone(),
                    port: *port,
                    count: *count,
                    reason,
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

/// What the guarded resolver reads while a request of one run is sent.
#[derive(Clone)]
struct Dial {
    egress: Arc<RunEgress>,
    /// The connection's port; `None` when it cannot be told.
    port: Option<u16>,
    /// An operator allowlist's private permits for an operator-configured
    /// origin (see [`HttpClient::execute_operator_origin`]).
    operator: Option<Arc<PrivatePermit>>,
    /// The first refusal the resolver made for this request (or, for a
    /// fixed resolver, its session). Only that one is counted on the run, so
    /// a client that resolves again for the same request (a retry, a
    /// reconnect) does not inflate the ledger.
    refused: Arc<Mutex<Option<EgressRefused>>>,
}

tokio::task_local! {
    /// The request being sent ([`RunEgress::guarded`]).
    static DIAL: Dial;
}

type BoxError = Box<dyn std::error::Error + Send + Sync>;
type ResolveFuture = Pin<Box<dyn Future<Output = Result<Vec<SocketAddr>, BoxError>> + Send>>;

/// The guarded resolver every client built here installs (#2950). It reads
/// the request's run from [`RunEgress::guarded`]'s scope, or from the run it
/// was built for ([`mcp_client_builder`], whose session sends outside any
/// scope). With neither it resolves for no run: public addresses only.
///
/// It returns only addresses the SSRF rule admits, with port 0 so the
/// connector dials the URL's port, and the connector never resolves on its
/// own. A refusal is counted on the run and recorded for
/// [`RunEgress::guarded`].
#[derive(Clone)]
struct PermitResolver {
    fixed: Option<Dial>,
}

impl PermitResolver {
    const fn scoped() -> Self {
        Self { fixed: None }
    }

    fn fixed(egress: Arc<RunEgress>, port: Option<u16>) -> Self {
        Self {
            fixed: Some(Dial {
                egress,
                port,
                operator: None,
                refused: Arc::new(Mutex::new(None)),
            }),
        }
    }

    /// Resolve `host`. The dial context is read here, synchronously: the
    /// connector calls this from inside the request's own poll.
    fn resolve_host(&self, host: String) -> ResolveFuture {
        let dial = self
            .fixed
            .clone()
            .or_else(|| DIAL.try_with(Clone::clone).ok());
        Box::pin(async move {
            let Some(dial) = dial else {
                let admitted =
                    resolve_permitted(&SystemLookup, &host, None, &PrivatePermit::default())
                        .await
                        .map_err(|error| Box::new(error) as BoxError)?;
                return Ok(sockets(admitted));
            };
            let permit = match &dial.operator {
                Some(operator) => dial.egress.private.union(operator),
                None => dial.egress.private.clone(),
            };
            match resolve_permitted(dial.egress.lookup.as_ref(), &host, dial.port, &permit).await {
                Ok(admitted) => Ok(sockets(admitted)),
                Err(error) => {
                    if let (Some(reason), Ok(mut first)) =
                        (resolve_refusal_reason(error), dial.refused.lock())
                    {
                        match first.as_ref() {
                            // Counted already: the tool call still learns why.
                            Some(refused) => {
                                let _ = CALL.try_with(|call| call.note(refused));
                            }
                            None => {
                                *first =
                                    Some(dial.egress.refuse(&host, dial.port.unwrap_or(0), reason));
                            }
                        }
                    }
                    Err(Box::new(error) as BoxError)
                }
            }
        })
    }
}

fn sockets(addresses: Vec<IpAddr>) -> Vec<SocketAddr> {
    addresses
        .into_iter()
        .map(|address| SocketAddr::new(address, 0))
        .collect()
}

impl reqwest::dns::Resolve for PermitResolver {
    fn resolve(&self, name: reqwest::dns::Name) -> reqwest::dns::Resolving {
        let resolving = self.resolve_host(name.as_str().to_owned());
        Box::pin(async move {
            let addresses: reqwest::dns::Addrs = Box::new(resolving.await?.into_iter());
            Ok(addresses)
        })
    }
}

impl reqwest_mcp::dns::Resolve for PermitResolver {
    fn resolve(&self, name: reqwest_mcp::dns::Name) -> reqwest_mcp::dns::Resolving {
        let resolving = self.resolve_host(name.as_str().to_owned());
        Box::pin(async move {
            let addresses: reqwest_mcp::dns::Addrs = Box::new(resolving.await?.into_iter());
            Ok(addresses)
        })
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
///
/// # Panics
///
/// Only in this crate's unit tests, inside `strict`, when no run is in
/// scope.
#[must_use]
pub fn current() -> Arc<RunEgress> {
    CURRENT.try_with(Arc::clone).unwrap_or_else(|_| {
        #[cfg(test)]
        assert!(
            STRICT.try_with(|()| ()).is_err(),
            "an HTTP client was built outside any run's egress scope (#1490)"
        );
        unscoped(UNSCOPED_IS_OPEN)
    })
}

#[cfg(test)]
tokio::task_local! {
    /// Set by [`strict`]: a client built outside every run fails the test.
    static STRICT: ();
}

/// Run a test body in which a client built outside any run's egress scope
/// panics, instead of getting the open policy unit tests otherwise get. A
/// path that builds its client after materialization (a governed call) then
/// fails its test even when the call never reaches the network.
#[cfg(test)]
pub(crate) async fn strict<F: Future>(future: F) -> F::Output {
    STRICT.scope((), future).await
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

/// The builder every toolkit family starts its client from: no redirect,
/// no proxy (a proxy would resolve the name instead of the guard), and the
/// guarded resolver, which reads the run and port of each request
/// [`HttpClient::execute`] sends.
pub fn client_builder() -> reqwest::ClientBuilder {
    guard(reqwest::Client::builder())
}

fn guard(builder: reqwest::ClientBuilder) -> reqwest::ClientBuilder {
    builder
        .redirect(reqwest::redirect::Policy::none())
        .no_proxy()
        .dns_resolver(Arc::new(PermitResolver::scoped()))
}

/// The builder of a remote MCP session (reqwest 0.13, rmcp's line), bound to
/// `egress` (or, when `None`, the run in scope, [`current`]) for connections
/// to `endpoint`'s port: no
/// proxy, and the guarded resolver. The session sends outside any request
/// scope, so the run is fixed at build; redirects are the caller's (same
/// origin only, so the port never changes).
pub(crate) fn mcp_client_builder(
    endpoint: &reqwest_mcp::Url,
    egress: Option<Arc<RunEgress>>,
) -> reqwest_mcp::ClientBuilder {
    reqwest_mcp::Client::builder()
        .no_proxy()
        .dns_resolver(PermitResolver::fixed(
            egress.unwrap_or_else(current),
            endpoint.port_or_known_default(),
        ))
}

/// The client a direct fetch builds for one port: no redirect, no proxy,
/// and `resolver` (the egress guard under the fetch's own rules).
pub(crate) fn fetch_client_builder(
    resolver: elitea_egress_guard::GuardedResolver,
) -> reqwest::ClientBuilder {
    reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .no_proxy()
        .dns_resolver(Arc::new(resolver))
}

/// A hyper connector resolving through the guarded resolver: send each
/// request inside [`RunEgress::guarded`].
pub(crate) type GuardedHttpConnector =
    hyper_util::client::legacy::connect::HttpConnector<PermitResolverService>;

/// The resolver half of [`GuardedHttpConnector`].
#[derive(Clone)]
pub(crate) struct PermitResolverService(PermitResolver);

impl tower_service::Service<hyper_util::client::legacy::connect::dns::Name>
    for PermitResolverService
{
    type Response = std::vec::IntoIter<SocketAddr>;
    type Error = BoxError;
    type Future = Pin<Box<dyn Future<Output = Result<Self::Response, BoxError>> + Send>>;

    fn poll_ready(
        &mut self,
        _context: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Result<(), BoxError>> {
        std::task::Poll::Ready(Ok(()))
    }

    fn call(&mut self, name: hyper_util::client::legacy::connect::dns::Name) -> Self::Future {
        let resolving = self.0.resolve_host(name.as_str().to_owned());
        Box::pin(async move { Ok(resolving.await?.into_iter()) })
    }
}

/// [`GuardedHttpConnector`], built.
pub(crate) fn guarded_http_connector() -> GuardedHttpConnector {
    hyper_util::client::legacy::connect::HttpConnector::new_with_resolver(PermitResolverService(
        PermitResolver::scoped(),
    ))
}

/// The connectors' hardened provider transport (`elitea-connectors`) on a
/// guarded client. Wrap it in an [`EgressTransport`], which sends each
/// request inside [`RunEgress::guarded`].
///
/// # Errors
///
/// reqwest could not build the client.
pub fn hardened_transport(
    policy: &elitea_connectors::ClientPolicy,
) -> Result<elitea_connectors::reqwest012::ReqwestTransport, elitea_connectors::BuildError> {
    elitea_connectors::reqwest012::ReqwestTransport::hardened_from(client_builder(), policy)
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
            client: guard(self).build()?,
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
        let Ok(sent) = self
            .egress
            .guarded(&url, self.client.execute(request))
            .await
        else {
            return Err(self.refusal_error().await);
        };
        let response = sent?;
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
    ///
    /// The SSRF rule still applies, with the private addresses `operator`
    /// names (the operator allowlist's CIDRs and literals) admitted beside
    /// the run's private permit.
    pub(crate) async fn execute_operator_origin(
        &self,
        request: reqwest::Request,
        operator: Option<Arc<PrivatePermit>>,
    ) -> reqwest::Result<reqwest::Response> {
        let url = request.url().clone();
        let Ok(sent) = self
            .egress
            .guarded_with(&url, operator, self.client.execute(request))
            .await
        else {
            return Err(self.refusal_error().await);
        };
        let response = sent?;
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
/// [`TransportError::egress_refused`], and records the hosts reached. Each
/// request is sent inside [`RunEgress::guarded`], so an inner transport
/// built by [`hardened_transport`] applies the SSRF rule too.
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
        let Ok(sent) = self.egress.guarded(&url, self.inner.execute(request)).await else {
            return Err(TransportError::egress_refused());
        };
        let response = sent?;
        self.egress.record_reached(&url);
        Ok(response)
    }
}

#[cfg(test)]
#[path = "egress_tests.rs"]
pub(crate) mod tests;
