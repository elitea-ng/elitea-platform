//! The operator's private-destination permit (#2950).
//!
//! The SSRF rule refuses private addresses (loopback, RFC 1918, unique-local,
//! CGNAT, ...) by default. Some legitimate destinations live there: an
//! on-premises Jira, GitLab or Confluence. A [`PrivatePermit`] is the list an
//! OPERATOR keeps for them, in the egresslib grammar, and it lifts the rule
//! for exactly what it names:
//!
//! - a CIDR block or an IP literal admits the private addresses inside it;
//! - a host name, `host:port` or `*.domain` admits the private addresses that
//!   name resolves to. Unlike a public allowlist's name (which never permits
//!   a private address, because nothing about it says where it resolves),
//!   a name here is the operator saying "this host is on the private
//!   network", which is the point of the list.
//!
//! A port the entry pins is honoured. A forbidden address (unspecified,
//! link-local and the cloud metadata endpoints, multicast, reserved) is never
//! admitted, whatever the permit says: `169.254.0.0/16` admits nothing.
//!
//! The permit is platform configuration (Main's `worker_egress` section),
//! never a value a toolkit or MCP configuration carries, so a project member
//! cannot widen it. Main's Go guard reads the same list for its own outbound
//! calls; this type is the worker's reading of it.

use std::net::IpAddr;

use crate::allowlist::{Allowlist, PermitSet};
use crate::classify::{Class, classify};
use crate::policy::PublicPolicyError;

/// The most entries one private permit may carry (the proto bound on
/// `WorkerEgressPolicyV1.private_allow`).
pub const MAX_PRIVATE_PERMIT_ENTRIES: usize = 256;

/// Private destinations an operator permitted. The default permits nothing.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct PrivatePermit {
    /// The names (and literals) as written, matched against the request's
    /// host.
    names: Allowlist,
    /// The CIDR and IP-literal entries, matched against every resolved
    /// address.
    addresses: PermitSet,
}

impl PrivatePermit {
    /// A permit from raw entries. Fail-closed: a malformed or blank entry, or
    /// more than [`MAX_PRIVATE_PERMIT_ENTRIES`], refuses the whole permit.
    ///
    /// # Errors
    ///
    /// [`PublicPolicyError`] (the same vocabulary as the public allowlist).
    pub fn parse<'a>(
        entries: impl IntoIterator<Item = &'a str>,
    ) -> Result<Self, PublicPolicyError> {
        let entries: Vec<&str> = entries.into_iter().collect();
        if entries.len() > MAX_PRIVATE_PERMIT_ENTRIES {
            return Err(PublicPolicyError::TooManyEntries);
        }
        if entries.iter().any(|entry| entry.trim().is_empty()) {
            return Err(PublicPolicyError::Malformed);
        }
        let names = Allowlist::parse(entries).map_err(|_| PublicPolicyError::Malformed)?;
        let addresses = names.private_permits();
        Ok(Self { names, addresses })
    }

    /// A permit holding only the address entries of `permits` (an operator
    /// allowlist's CIDRs and literals): a name in that list admits nothing
    /// private.
    #[must_use]
    pub fn addresses_only(permits: PermitSet) -> Self {
        Self {
            names: Allowlist::default(),
            addresses: permits,
        }
    }

    /// What either permit admits.
    #[must_use]
    pub fn union(&self, other: &Self) -> Self {
        Self {
            names: self.names.merged(&other.names),
            addresses: self.addresses.merged(&other.addresses),
        }
    }

    /// Whether the permit names nothing.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        !self.names.is_configured() && self.addresses.is_empty()
    }

    /// Whether `address`, one answer for `host`, may be dialled on `port`.
    /// Public addresses always may; private ones when an entry names the
    /// address or the host; forbidden ones never.
    ///
    /// `port` is `None` when the caller cannot tell which port the
    /// connection is for: only entries that pin no port apply then.
    #[must_use]
    pub fn admits(&self, host: &str, address: IpAddr, port: Option<u16>) -> bool {
        match classify(address) {
            Class::Public => true,
            Class::Forbidden => false,
            Class::Private => match port {
                Some(port) => self.addresses.admits(address, port) || self.names.allows(host, port),
                None => self.addresses.admits_unpinned(address) || self.names.allows_unpinned(host),
            },
        }
    }

    /// The answers for `host` this permit admits, in order.
    #[must_use]
    pub fn filter(&self, host: &str, addresses: &[IpAddr], port: Option<u16>) -> Vec<IpAddr> {
        addresses
            .iter()
            .copied()
            .filter(|address| self.admits(host, *address, port))
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ip(text: &str) -> IpAddr {
        text.parse().expect("address")
    }

    #[test]
    fn names_cidrs_and_literals_admit_their_private_addresses() {
        let permit = PrivatePermit::parse([
            "jira.corp.example",
            "*.git.corp.example:8443",
            "10.20.0.0/16",
            "192.168.5.5:8080",
        ])
        .expect("permit");
        assert!(permit.admits("jira.corp.example", ip("10.1.1.1"), Some(443)));
        assert!(permit.admits("a.git.corp.example", ip("172.16.0.9"), Some(8443)));
        assert!(!permit.admits("a.git.corp.example", ip("172.16.0.9"), Some(443)));
        assert!(permit.admits("other.example", ip("10.20.3.4"), Some(443)));
        assert!(!permit.admits("other.example", ip("10.21.3.4"), Some(443)));
        assert!(permit.admits("192.168.5.5", ip("192.168.5.5"), Some(8080)));
        assert!(!permit.admits("192.168.5.5", ip("192.168.5.5"), Some(80)));
        assert!(permit.admits("anything.example", ip("203.0.113.7"), Some(1)));
    }

    #[test]
    fn an_unknown_port_applies_only_unpinned_entries() {
        let permit = PrivatePermit::parse(["pinned.example:8443", "open.example"]).expect("permit");
        assert!(!permit.admits("pinned.example", ip("10.0.0.1"), None));
        assert!(permit.admits("open.example", ip("10.0.0.1"), None));
    }

    #[test]
    fn forbidden_addresses_are_never_admitted() {
        let permit =
            PrivatePermit::parse(["169.254.0.0/16", "0.0.0.0/0", "::/0", "metadata.example"])
                .expect("permit");
        for address in [
            "169.254.169.254",
            "fd00:ec2::254",
            "100.100.100.200",
            "fe80::1",
        ] {
            assert!(
                !permit.admits("metadata.example", ip(address), Some(80)),
                "{address}"
            );
        }
    }

    #[test]
    fn a_malformed_or_blank_entry_fails_the_permit() {
        for bad in ["https://jira.example", " ", "10.0.0.0/33"] {
            assert_eq!(
                PrivatePermit::parse(["ok.example", bad]),
                Err(PublicPolicyError::Malformed),
                "{bad:?}"
            );
        }
        let many: Vec<String> = (0..=MAX_PRIVATE_PERMIT_ENTRIES)
            .map(|index| format!("h{index}.example"))
            .collect();
        assert_eq!(
            PrivatePermit::parse(many.iter().map(String::as_str)),
            Err(PublicPolicyError::TooManyEntries)
        );
        assert!(PrivatePermit::default().is_empty());
        assert!(!PrivatePermit::default().admits("x.example", ip("10.0.0.1"), Some(443)));
    }

    #[test]
    fn addresses_only_ignores_names() {
        let list = Allowlist::parse(["provider.example", "10.0.0.0/8"]).expect("list");
        let permit = PrivatePermit::addresses_only(list.private_permits());
        assert!(!permit.admits("provider.example", ip("192.168.0.1"), Some(443)));
        assert!(permit.admits("provider.example", ip("10.0.0.1"), Some(443)));
    }
}
