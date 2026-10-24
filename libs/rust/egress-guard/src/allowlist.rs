//! The egresslib allowlist grammar (`libs/go/egresslib/allowlist.go`):
//!
//! ```text
//! example.com            an exact host, any port
//! example.com:8000       an exact host, that port only
//! *.example.com          any host UNDER example.com (never example.com itself)
//! *.example.com:8000     the same, that port only
//! 192.168.29.60:8000     an exact IP literal and port
//! 192.168.29.0/24        a CIDR block, matched against IP-literal hosts only
//! ```
//!
//! A malformed entry fails the whole list (fail-closed): a typo that dropped
//! one rule would either open egress wider than intended or wedge a provider.

use std::fmt;
use std::net::IpAddr;

use crate::classify::canonical;

/// A malformed allowlist entry. The entry text is not retained.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct AllowlistError;

impl fmt::Display for AllowlistError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(
            "an egress allowlist entry is malformed: want a host, host:port, *.domain or CIDR block",
        )
    }
}

impl std::error::Error for AllowlistError {}

#[derive(Clone, Debug, Eq, PartialEq)]
enum Entry {
    Exact { host: String, port: Option<u16> },
    Wildcard { suffix: String, port: Option<u16> },
    Cidr { network: IpAddr, prefix: u8 },
}

/// One parsed, immutable allowlist.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Allowlist {
    entries: Vec<Entry>,
}

impl Allowlist {
    /// Parse a comma-separated list (an environment variable). Blank entries
    /// are skipped: a CSV variable routinely carries a trailing comma.
    ///
    /// # Errors
    ///
    /// [`AllowlistError`] when any entry is malformed.
    pub fn parse_csv(text: &str) -> Result<Self, AllowlistError> {
        Self::parse(text.split(','))
    }

    /// Parse raw entries, skipping blank ones.
    ///
    /// # Errors
    ///
    /// [`AllowlistError`] when any entry is malformed.
    pub fn parse<'a>(raw: impl IntoIterator<Item = &'a str>) -> Result<Self, AllowlistError> {
        let mut entries = Vec::new();
        for text in raw {
            if text.trim().is_empty() {
                continue;
            }
            let entry = parse_entry(text)?;
            if !entries.contains(&entry) {
                entries.push(entry);
            }
        }
        Ok(Self { entries })
    }

    /// Whether the list carries any entry.
    #[must_use]
    pub fn is_configured(&self) -> bool {
        !self.entries.is_empty()
    }

    /// Whether `host` on `port` is named by an entry. Unlike the Go
    /// `Allows`, an EMPTY list admits nothing: the web family's provider
    /// origins are fail-closed (#1238).
    #[must_use]
    pub fn allows(&self, host: &str, port: u16) -> bool {
        let host = normalise_host(&host.to_ascii_lowercase());
        if host.is_empty() {
            return false;
        }
        let literal = host.parse::<IpAddr>().ok();
        self.entries.iter().any(|entry| match entry {
            Entry::Exact {
                host: name,
                port: pinned,
            } => *name == host && pinned.is_none_or(|pinned| pinned == port),
            Entry::Wildcard {
                suffix,
                port: pinned,
            } => {
                host.len() > suffix.len()
                    && host.ends_with(suffix.as_str())
                    && pinned.is_none_or(|pinned| pinned == port)
            }
            Entry::Cidr { network, prefix } => {
                literal.is_some_and(|ip| block_contains(*network, *prefix, canonical(ip)))
            }
        })
    }

    /// The entries that permit private egress, each for its own range only
    /// (`addrcheck.PrivatePermits`): a CIDR its block, an IP literal that one
    /// address, `localhost` the loopback blocks, each limited to the entry's
    /// port when it pins one. A name or wildcard permits no private address,
    /// because nothing about it says where it resolves.
    #[must_use]
    pub fn private_permits(&self) -> PermitSet {
        let mut permits = Vec::new();
        for entry in &self.entries {
            match entry {
                Entry::Cidr { network, prefix } => permits.push(Permit {
                    network: *network,
                    prefix: *prefix,
                    port: None,
                }),
                Entry::Exact { host, port } if host == "localhost" => {
                    permits.push(Permit {
                        network: IpAddr::from([127, 0, 0, 0]),
                        prefix: 8,
                        port: *port,
                    });
                    permits.push(Permit {
                        network: IpAddr::from([0u16, 0, 0, 0, 0, 0, 0, 1]),
                        prefix: 128,
                        port: *port,
                    });
                }
                Entry::Exact { host, port } => {
                    if let Ok(ip) = host.parse::<IpAddr>() {
                        let ip = canonical(ip);
                        permits.push(Permit {
                            network: ip,
                            prefix: if ip.is_ipv4() { 32 } else { 128 },
                            port: *port,
                        });
                    }
                }
                Entry::Wildcard { .. } => {}
            }
        }
        PermitSet { permits }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct Permit {
    network: IpAddr,
    prefix: u8,
    port: Option<u16>,
}

/// The private permits of one allowlist.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct PermitSet {
    permits: Vec<Permit>,
}

impl PermitSet {
    /// Whether a permit names this private address on this port.
    #[must_use]
    pub fn admits(&self, address: IpAddr, port: u16) -> bool {
        let address = canonical(address);
        self.permits.iter().any(|permit| {
            permit.port.is_none_or(|pinned| pinned == port)
                && block_contains(permit.network, permit.prefix, address)
        })
    }

    /// Whether the set holds no permit.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.permits.is_empty()
    }
}

fn block_contains(network: IpAddr, prefix: u8, address: IpAddr) -> bool {
    match (network, address) {
        (IpAddr::V4(network), IpAddr::V4(address)) => {
            let mask = if prefix == 0 {
                0
            } else {
                u32::MAX << (32 - u32::from(prefix))
            };
            u32::from(address) & mask == u32::from(network) & mask
        }
        (IpAddr::V6(network), IpAddr::V6(address)) => {
            let mask = if prefix == 0 {
                0
            } else {
                u128::MAX << (128 - u32::from(prefix))
            };
            u128::from(address) & mask == u128::from(network) & mask
        }
        _ => false,
    }
}

fn parse_entry(raw: &str) -> Result<Entry, AllowlistError> {
    let text = raw.trim().to_ascii_lowercase();
    if text.is_empty() || text.contains([' ', '\t']) || text.contains("://") {
        return Err(AllowlistError);
    }
    if let Some((address, prefix)) = text.split_once('/') {
        return parse_cidr(address, prefix);
    }
    let (host, port) = split_host_port(&text)?;
    if let Some(suffix) = host.strip_prefix("*.") {
        if suffix.is_empty() || suffix.contains('*') {
            return Err(AllowlistError);
        }
        return Ok(Entry::Wildcard {
            suffix: format!(".{suffix}"),
            port,
        });
    }
    if host.is_empty() || host.contains('*') {
        return Err(AllowlistError);
    }
    let host = normalise_host(host);
    if host.is_empty() {
        return Err(AllowlistError);
    }
    Ok(Entry::Exact { host, port })
}

fn parse_cidr(address: &str, prefix: &str) -> Result<Entry, AllowlistError> {
    let network = address.parse::<IpAddr>().map_err(|_| AllowlistError)?;
    if prefix.is_empty() || !prefix.bytes().all(|byte| byte.is_ascii_digit()) {
        return Err(AllowlistError);
    }
    let prefix = prefix.parse::<u8>().map_err(|_| AllowlistError)?;
    let max = if network.is_ipv4() { 32 } else { 128 };
    if prefix > max {
        return Err(AllowlistError);
    }
    // An IPv4-mapped block (::ffff:10.0.0.0/104) names the IPv4 block, so it
    // matches the canonical IPv4 address (`addrcheck.CanonicalBlock`).
    if let IpAddr::V6(v6) = network
        && prefix >= 96
        && let Some(v4) = v6.to_ipv4_mapped()
    {
        return Ok(Entry::Cidr {
            network: IpAddr::V4(v4),
            prefix: prefix - 96,
        });
    }
    Ok(Entry::Cidr { network, prefix })
}

/// `net.SplitHostPort` semantics: `host:port`, `[v6]:port`; anything else
/// (a bare host, a bare IPv6 literal) is the host with no port.
fn split_host_port(text: &str) -> Result<(&str, Option<u16>), AllowlistError> {
    let (host, port) = if let Some(rest) = text.strip_prefix('[') {
        match rest.split_once("]:") {
            Some((host, port)) => (host, Some(port)),
            None => return Ok((text, None)),
        }
    } else {
        match text.split_once(':') {
            Some((host, port)) if !port.contains(':') => (host, Some(port)),
            _ => return Ok((text, None)),
        }
    };
    let Some(port) = port else {
        return Ok((host, None));
    };
    if port.is_empty() || !port.bytes().all(|byte| byte.is_ascii_digit()) {
        return Err(AllowlistError);
    }
    match port.parse::<u16>() {
        Ok(0) | Err(_) => Err(AllowlistError),
        Ok(port) => Ok((host, Some(port))),
    }
}

/// Strip a trailing root dot and canonicalise an IP literal.
fn normalise_host(host: &str) -> String {
    let host = host.strip_suffix('.').unwrap_or(host);
    host.parse::<IpAddr>()
        .map_or_else(|_| host.to_owned(), |ip| ip.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn grammar_matches_egresslib() {
        let list = Allowlist::parse_csv(
            "api.tavily.com, *.firecrawl.dev:443,api.search.brave.com,10.0.0.0/8,localhost:9000,",
        )
        .expect("list");
        assert!(list.allows("api.tavily.com", 443));
        assert!(list.allows("API.Tavily.com.", 443));
        assert!(list.allows("api.firecrawl.dev", 443));
        assert!(
            !list.allows("firecrawl.dev", 443),
            "a wildcard never names its apex"
        );
        assert!(!list.allows("api.firecrawl.dev", 8443), "the pinned port");
        assert!(list.allows("10.1.2.3", 443), "CIDR matches IP literals");
        assert!(!list.allows("evil.example", 443));
        assert!(
            !Allowlist::default().allows("api.tavily.com", 443),
            "empty is fail-closed"
        );
    }

    #[test]
    fn malformed_entries_fail_the_list() {
        for bad in [
            "https://api.tavily.com",
            "a b",
            "*.",
            "a.*.b",
            "host:0",
            "host:70000",
            "10.0.0.0/33",
            "example.com/path",
        ] {
            assert_eq!(Allowlist::parse_csv(bad), Err(AllowlistError), "{bad}");
        }
    }

    #[test]
    fn private_permits_open_only_their_range() {
        let list = Allowlist::parse([
            "10.0.0.0/8",
            "192.168.1.5:8080",
            "localhost:9000",
            "::ffff:172.16.0.0/108",
            "*.internal.example",
            "named.example",
        ])
        .expect("list");
        let set = list.private_permits();
        let ip = |text: &str| text.parse::<IpAddr>().expect("address");
        assert!(set.admits(ip("10.9.9.9"), 443));
        assert!(set.admits(ip("::ffff:10.9.9.9"), 443));
        assert!(set.admits(ip("192.168.1.5"), 8080));
        assert!(!set.admits(ip("192.168.1.5"), 443));
        assert!(set.admits(ip("127.0.0.1"), 9000));
        assert!(set.admits(ip("::1"), 9000));
        assert!(!set.admits(ip("127.0.0.1"), 80));
        assert!(set.admits(ip("172.16.3.4"), 443));
        assert!(!set.admits(ip("100.64.0.1"), 443));
    }
}
