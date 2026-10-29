//! The public-host policy (ADR-0044 decision 2, #1490).
//!
//! The SSRF rule ([`crate::classify`], [`crate::PermitSet`]) decides which
//! ADDRESSES may be dialled; it never narrows a public one. A
//! [`PublicPolicy`] narrows the HOSTS a run may reach: `Open` admits every
//! host (today's behaviour), `Allowlist` only the hosts its entries name, in
//! the egresslib grammar. The two compose: a host must pass the policy and
//! its addresses the SSRF rule, in every mode.

use std::fmt;

use crate::allowlist::Allowlist;

/// The most entries one public allowlist may carry (the proto bound on
/// `WorkerEgressPolicyV1.allow`).
pub const MAX_PUBLIC_ALLOWLIST_ENTRIES: usize = 1_024;

/// Why a public allowlist was refused. The entry text is not retained.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PublicPolicyError {
    /// An entry is malformed or blank.
    Malformed,
    /// More than [`MAX_PUBLIC_ALLOWLIST_ENTRIES`] entries.
    TooManyEntries,
}

impl fmt::Display for PublicPolicyError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::Malformed => {
                "an egress allowlist entry is malformed: want a host, host:port, *.domain or CIDR block"
            }
            Self::TooManyEntries => "the egress allowlist has more entries than the limit",
        })
    }
}

impl std::error::Error for PublicPolicyError {}

/// Which hosts a run may reach.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub enum PublicPolicy {
    /// Any host; only the SSRF rule applies.
    #[default]
    Open,
    /// Only the hosts the list names. An empty list admits nothing.
    Allowlist(Allowlist),
}

impl PublicPolicy {
    /// An allowlist policy from raw entries. Fail-closed: unlike
    /// [`Allowlist::parse`], a blank entry is malformed rather than skipped,
    /// because these entries are assembled by the platform, not typed into a
    /// CSV variable.
    ///
    /// # Errors
    ///
    /// [`PublicPolicyError`] for a malformed or blank entry, or too many.
    pub fn allowlist<'a>(
        entries: impl IntoIterator<Item = &'a str>,
    ) -> Result<Self, PublicPolicyError> {
        let entries: Vec<&str> = entries.into_iter().collect();
        if entries.len() > MAX_PUBLIC_ALLOWLIST_ENTRIES {
            return Err(PublicPolicyError::TooManyEntries);
        }
        if entries.iter().any(|entry| entry.trim().is_empty()) {
            return Err(PublicPolicyError::Malformed);
        }
        Allowlist::parse(entries)
            .map(Self::Allowlist)
            .map_err(|_| PublicPolicyError::Malformed)
    }

    /// Whether every host is admitted.
    #[must_use]
    pub const fn is_open(&self) -> bool {
        matches!(self, Self::Open)
    }

    /// Whether `host` (a name or an IP literal, brackets allowed) on `port`
    /// may be reached. A CIDR entry matches IP-literal hosts only, as in
    /// egresslib; a name is never judged by where it resolves.
    #[must_use]
    pub fn admits(&self, host: &str, port: u16) -> bool {
        match self {
            Self::Open => true,
            Self::Allowlist(list) => {
                let host = host
                    .strip_prefix('[')
                    .and_then(|rest| rest.strip_suffix(']'))
                    .unwrap_or(host);
                list.allows(host, port)
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn open_admits_everything() {
        let open = PublicPolicy::default();
        assert!(open.is_open());
        assert!(open.admits("anything.example", 443));
        assert!(open.admits("203.0.113.9", 8443));
    }

    #[test]
    fn allowlist_admits_only_what_it_names() {
        let policy = PublicPolicy::allowlist([
            "api.example.com",
            "*.atlassian.net:443",
            "203.0.113.0/24",
            "[2001:db8::1]:8443",
        ])
        .expect("policy");
        assert!(!policy.is_open());
        assert!(policy.admits("api.example.com", 443));
        assert!(
            policy.admits("API.EXAMPLE.COM.", 8080),
            "any port, case-folded"
        );
        assert!(policy.admits("acme.atlassian.net", 443));
        assert!(
            !policy.admits("acme.atlassian.net", 8443),
            "the pinned port"
        );
        assert!(!policy.admits("atlassian.net", 443), "never the apex");
        assert!(policy.admits("203.0.113.77", 443), "CIDR on an IP literal");
        assert!(policy.admits("[2001:db8::1]", 8443), "bracketed literal");
        assert!(!policy.admits("2001:db8::1", 443));
        assert!(!policy.admits("evil.example", 443));
        assert!(!policy.admits("198.51.100.1", 443));
    }

    #[test]
    fn an_empty_allowlist_admits_nothing() {
        let policy = PublicPolicy::allowlist([]).expect("policy");
        assert!(!policy.admits("api.example.com", 443));
    }

    #[test]
    fn a_malformed_or_blank_entry_fails_the_policy() {
        for bad in ["https://api.example.com", "a b", "*.", "host:0", " "] {
            assert_eq!(
                PublicPolicy::allowlist(["api.example.com", bad]),
                Err(PublicPolicyError::Malformed),
                "{bad:?}"
            );
        }
        let many: Vec<String> = (0..=MAX_PUBLIC_ALLOWLIST_ENTRIES)
            .map(|index| format!("h{index}.example"))
            .collect();
        assert_eq!(
            PublicPolicy::allowlist(many.iter().map(String::as_str)),
            Err(PublicPolicyError::TooManyEntries)
        );
    }
}
