//! The guard's dial rule (`addrcheck.Resolve` + `Filter`): resolve, check
//! every address, and hand the connector only the admitted ones.

use std::fmt;
use std::future::Future;
use std::io;
use std::net::{IpAddr, Ipv4Addr, SocketAddr};
use std::pin::Pin;
use std::sync::Arc;
use std::time::Duration;

use crate::allowlist::PermitSet;
use crate::classify::{Class, classify};
use crate::policy::PublicPolicy;

/// The most addresses one answer may carry (`addrcheck.MaxResolvedAddresses`).
pub const MAX_RESOLVED_ADDRESSES: usize = 32;
const DNS_LOOKUP_TIMEOUT: Duration = Duration::from_secs(5);

/// Why a name was not resolved to a dialable address. Carries no host name.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ResolveError {
    /// The lookup failed, timed out or answered nothing.
    Lookup,
    /// The answer carried more than [`MAX_RESOLVED_ADDRESSES`].
    TooManyAddresses,
    /// Every address was private (unpermitted) or forbidden.
    NotPublic,
    /// The run's [`PublicPolicy`] does not name the host on this port. Nothing
    /// was looked up.
    NotAllowed,
}

impl fmt::Display for ResolveError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::Lookup => "the host name could not be resolved",
            Self::TooManyAddresses => "the host name resolved to too many addresses",
            Self::NotPublic => "the host name resolves only to addresses egress refuses",
            Self::NotAllowed => "the host is not on this run's egress allowlist",
        })
    }
}

impl std::error::Error for ResolveError {}

/// The future a [`Lookup`] returns.
pub type LookupFuture = Pin<Box<dyn Future<Output = io::Result<Vec<IpAddr>>> + Send>>;

/// A name lookup: the system resolver in production, a fake in tests.
pub trait Lookup: Send + Sync {
    /// Every address `host` resolves to.
    fn lookup(&self, host: String) -> LookupFuture;
}

/// The system resolver (`getaddrinfo` through tokio). Scoped (zoned) IPv6
/// answers are dropped, as the Go guard drops them.
#[derive(Clone, Copy, Debug, Default)]
pub struct SystemLookup;

impl Lookup for SystemLookup {
    fn lookup(&self, host: String) -> LookupFuture {
        Box::pin(async move {
            let answers = tokio::net::lookup_host((host.as_str(), 0)).await?;
            Ok(answers
                .filter(|address| match address {
                    SocketAddr::V6(v6) => v6.scope_id() == 0,
                    SocketAddr::V4(_) => true,
                })
                .map(|address| address.ip())
                .collect())
        })
    }
}

/// Keep public addresses, and a private one only when a permit names it on
/// this port. Forbidden addresses are never kept.
#[must_use]
pub fn filter(addresses: &[IpAddr], port: u16, private: &PermitSet) -> Vec<IpAddr> {
    addresses
        .iter()
        .copied()
        .filter(|address| match classify(*address) {
            Class::Public => true,
            Class::Private => private.admits(*address, port),
            Class::Forbidden => false,
        })
        .collect()
}

/// Resolve `host` and return only the addresses egress admits on `port`.
/// An IP literal resolves to itself, `localhost` to 127.0.0.1 (never asked
/// of the resolver). The public policy is [`PublicPolicy::Open`]; see
/// [`resolve_checked_with`].
///
/// # Errors
///
/// [`ResolveError`] when the lookup fails, answers too much, or admits
/// nothing.
pub async fn resolve_checked(
    lookup: &dyn Lookup,
    host: &str,
    port: u16,
    private: &PermitSet,
) -> Result<Vec<IpAddr>, ResolveError> {
    resolve_checked_with(lookup, host, port, private, &PublicPolicy::Open).await
}

/// [`resolve_checked`] under a run's public policy: a host the policy does
/// not name on `port` is refused before any lookup, and the addresses of an
/// admitted one still pass the SSRF rule.
///
/// # Errors
///
/// [`ResolveError::NotAllowed`] for a host outside the policy, otherwise as
/// [`resolve_checked`].
pub async fn resolve_checked_with(
    lookup: &dyn Lookup,
    host: &str,
    port: u16,
    private: &PermitSet,
    public: &PublicPolicy,
) -> Result<Vec<IpAddr>, ResolveError> {
    let host = host
        .strip_prefix('[')
        .and_then(|rest| rest.strip_suffix(']'))
        .unwrap_or(host);
    if !public.admits(host, port) {
        return Err(ResolveError::NotAllowed);
    }
    let addresses = if let Ok(literal) = host.parse::<IpAddr>() {
        vec![literal]
    } else if host.eq_ignore_ascii_case("localhost") {
        vec![IpAddr::V4(Ipv4Addr::LOCALHOST)]
    } else {
        let answer = tokio::time::timeout(DNS_LOOKUP_TIMEOUT, lookup.lookup(host.to_owned()))
            .await
            .map_err(|_| ResolveError::Lookup)?
            .map_err(|_| ResolveError::Lookup)?;
        if answer.is_empty() {
            return Err(ResolveError::Lookup);
        }
        if answer.len() > MAX_RESOLVED_ADDRESSES {
            return Err(ResolveError::TooManyAddresses);
        }
        answer
    };
    let admitted = filter(&addresses, port, private);
    if admitted.is_empty() {
        return Err(ResolveError::NotPublic);
    }
    Ok(admitted)
}

/// A `reqwest` resolver that returns only checked addresses, for one port.
///
/// The connector never resolves on its own, so the address it connects to is
/// the address that was checked: a name re-pointed between the check and the
/// dial (DNS rebinding) cannot reach a private address. The port is fixed per
/// resolver because a permit may pin one; a caller fetching on 443 and 80
/// builds one client per port. Build the client with `no_proxy()`, or a proxy
/// would resolve the name instead.
///
/// reqwest never asks a resolver about an IP-literal URL, so a caller that
/// may fetch one checks it with [`resolve_checked_with`] before sending.
#[derive(Clone)]
pub struct GuardedResolver {
    lookup: Arc<dyn Lookup>,
    port: u16,
    private: PermitSet,
    public: PublicPolicy,
}

impl GuardedResolver {
    /// A resolver over `lookup` for connections on `port`.
    #[must_use]
    pub fn new(lookup: Arc<dyn Lookup>, port: u16, private: PermitSet) -> Self {
        Self {
            lookup,
            port,
            private,
            public: PublicPolicy::Open,
        }
    }

    /// The same resolver under a run's public policy.
    #[must_use]
    pub fn with_public_policy(mut self, public: PublicPolicy) -> Self {
        self.public = public;
        self
    }
}

impl fmt::Debug for GuardedResolver {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GuardedResolver")
            .field("port", &self.port)
            .field("public_open", &self.public.is_open())
            .finish_non_exhaustive()
    }
}

impl reqwest::dns::Resolve for GuardedResolver {
    fn resolve(&self, name: reqwest::dns::Name) -> reqwest::dns::Resolving {
        let resolver = self.clone();
        let host = name.as_str().to_owned();
        Box::pin(async move {
            let admitted = resolve_checked_with(
                resolver.lookup.as_ref(),
                &host,
                resolver.port,
                &resolver.private,
                &resolver.public,
            )
            .await
            .map_err(|error| Box::new(error) as Box<dyn std::error::Error + Send + Sync>)?;
            let port = resolver.port;
            let addresses: reqwest::dns::Addrs = Box::new(
                admitted
                    .into_iter()
                    .map(move |address| SocketAddr::new(address, port)),
            );
            Ok(addresses)
        })
    }
}
