//! The git-host allowlist, re-checked in the engine before any credential
//! is used (ADR-0022 decision 6, ADR-0026 decision 7).
//!
//! The Go host already refuses a non-allowlisted destination
//! (`run.CheckEgress`). The engine checks again because the host checks
//! what it PREDICTS the clone host to be (`run.DestinationHost`), while
//! this check sees the host the engine actually derived and will connect
//! to. The two can differ: for `provider_config.base_url =
//! "https://api.github.com"` the host checks `api.github.com` and the
//! engine clones from `github.com`, and a full repository URL is a
//! destination for the host's guess but not for the engine (the clone host
//! comes from the provider configuration). Deployments list
//! `github.com,*.github.com`, which admits both.
//!
//! The rules are the Python `security/egress.py` and Go
//! `spi.ParseEgressPolicy` ones, byte for byte (the matching itself is
//! `elitea_connectors::egress::HostAllowlist`, shared with the connectors):
//!
//! * fail-closed: no entries refuses everything;
//! * `*` disables the control, explicitly;
//! * `*.example.com` matches DIRECT subdomains only (not the apex, not
//!   `a.b.example.com`);
//! * comparison is case-insensitive and ignores a port (`host:443`) and the
//!   brackets of an IPv6 literal.
//!
//! A refusal is a `ValueError` (`invalid_input`), as in both other copies.
//!
//! The type-state is the enforcement: the clone takes an
//! [`AdmittedTarget`], and the only way to get one is [`EgressPolicy::admit`].
//! So no code path can reach the transport — where the credential is
//! read — without the check. The admitted host is also the only host the
//! transport connects to: the clone follows no HTTP redirect, with or
//! without a credential (`clone::transport_options`), since a redirect
//! would take an anonymous clone to a host this check never saw.

use super::providers::CloneTarget;
use crate::names::SettingNames;
use elitea_connectors::egress::HostAllowlist;
use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::pyvalue::py_repr;

/// An allowlist of git hosts: the shared rule
/// ([`elitea_connectors::egress::HostAllowlist`], the one every connector
/// applies too) with this engine's setting names and refusal messages.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EgressPolicy {
    allowlist: HostAllowlist,
    names: &'static SettingNames,
}

impl Default for EgressPolicy {
    fn default() -> Self {
        Self {
            allowlist: HostAllowlist::default(),
            names: &SettingNames::NEUTRAL,
        }
    }
}

impl EgressPolicy {
    /// Parse a comma- or whitespace-separated list (Python's
    /// `EgressPolicy.parse`). `None` or blank gives the empty policy.
    #[must_use]
    pub fn parse(raw: Option<&str>) -> Self {
        Self {
            allowlist: HostAllowlist::parse(raw),
            names: &SettingNames::NEUTRAL,
        }
    }

    /// Name the consuming engine's settings: the refusal of an empty list
    /// names its allowlist variable, and an admitted target carries the
    /// names on to the clone (its `User-Agent`).
    #[must_use]
    pub fn named(mut self, names: &'static SettingNames) -> Self {
        self.names = names;
        self
    }

    /// The shared rule this policy applies.
    #[must_use]
    pub fn allowlist(&self) -> &HostAllowlist {
        &self.allowlist
    }

    /// The entries, lower-cased, in order.
    #[must_use]
    pub fn entries(&self) -> &[String] {
        self.allowlist.entries()
    }

    /// The explicit opt-out.
    #[must_use]
    pub fn allows_everything(&self) -> bool {
        self.allowlist.allows_everything()
    }

    /// No entries: every destination is refused.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.allowlist.is_empty()
    }

    /// Whether `host` (optionally with a port) is on the list.
    #[must_use]
    pub fn permits(&self, host: &str) -> bool {
        self.allowlist.permits(host)
    }

    /// Refuse `host` unless it is allowed, with the Python messages.
    ///
    /// # Errors
    ///
    /// A `ValueError` naming the host (and the list, when there is one).
    pub fn check(&self, host: &str, what: &str) -> Result<(), EngineError> {
        if self.is_empty() {
            return Err(EngineError::new(
                ErrorType::Value,
                format!(
                    "No git-host allowlist is configured, so the {what} {} is refused. Set {} to the hosts this deployment may clone from (or '*' to disable the control explicitly).",
                    py_repr(host),
                    self.names.git_allowlist
                ),
            ));
        }
        if !self.permits(host) {
            return Err(EngineError::new(
                ErrorType::Value,
                format!(
                    "The {what} {} is not on the git-host allowlist ({}), so this invocation is refused before any credential is used.",
                    py_repr(host),
                    self.entries().join(", ")
                ),
            ));
        }
        Ok(())
    }

    /// Check a derived clone target and hand back the only value the clone
    /// accepts.
    ///
    /// # Errors
    ///
    /// See [`EgressPolicy::check`].
    pub fn admit(&self, target: CloneTarget) -> Result<AdmittedTarget, EngineError> {
        // The host is the URL's own authority (`CloneTarget` parses it from
        // the URL it will connect to), so the checked host and the
        // connected host cannot disagree.
        let host = if target.host().is_empty() {
            "<unresolvable>"
        } else {
            target.host()
        };
        self.check(host, "clone destination")?;
        Ok(AdmittedTarget(target, self.names))
    }
}

/// A clone target whose host passed the allowlist.
#[derive(Debug)]
pub struct AdmittedTarget(CloneTarget, &'static SettingNames);

impl AdmittedTarget {
    /// The target.
    #[must_use]
    pub fn target(&self) -> &CloneTarget {
        &self.0
    }

    /// The names of the policy that admitted it.
    #[must_use]
    pub fn names(&self) -> &'static SettingNames {
        self.1
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use elitea_engine_core::errors::classify;

    #[test]
    fn an_unset_allowlist_refuses_everything() {
        let policy = EgressPolicy::parse(None);
        assert!(policy.is_empty());
        let error = policy.check("github.com", "destination").err();
        assert!(
            error
                .as_ref()
                .is_some_and(|e| e.message.contains("No git-host allowlist is configured")),
            "{error:?}"
        );
        assert!(EgressPolicy::parse(Some("  , ")).is_empty());
    }

    #[test]
    fn a_host_on_the_list_is_permitted_case_insensitively() {
        let policy = EgressPolicy::parse(Some("github.com, gitlab.example.internal"));
        assert!(policy.check("github.com", "destination").is_ok());
        assert!(
            policy
                .check("GitLab.Example.Internal", "destination")
                .is_ok()
        );
    }

    #[test]
    fn a_host_off_the_list_is_refused() {
        let policy = EgressPolicy::parse(Some("github.com"));
        let error = policy.check("evil.example", "destination").err();
        assert!(
            error
                .as_ref()
                .is_some_and(|e| e.message.contains("not on the git-host allowlist")),
            "{error:?}"
        );
    }

    #[test]
    fn a_wildcard_matches_one_label_only() {
        let policy = EgressPolicy::parse(Some("*.github.com"));
        for (candidate, allowed) in [
            ("api.github.com", true),
            ("github.com", false),
            ("a.b.github.com", false),
            ("notgithub.com", false),
            ("evil.com/api.github.com", false),
        ] {
            assert_eq!(policy.permits(candidate), allowed, "{candidate}");
        }
    }

    #[test]
    fn a_port_does_not_change_the_host() {
        let policy = EgressPolicy::parse(Some("git.internal"));
        assert!(policy.permits("git.internal:8443"));
        assert!(!policy.permits("evil.example:8443"));
    }

    #[test]
    fn a_bare_star_disables_the_control_explicitly() {
        let policy = EgressPolicy::parse(Some("*"));
        assert!(policy.allows_everything());
        assert!(policy.check("anything.at.all", "destination").is_ok());
    }

    #[test]
    fn the_go_shared_rules_hold() {
        // spi_test.go TestEgressPolicyRulesAreTheSharedOnes.
        let policy = EgressPolicy::parse(Some("github.com, *.github.com GITLAB.example"));
        for (host, want) in [
            ("github.com", true),
            ("api.github.com", true),
            ("a.b.github.com", false),
            ("github.com:443", true),
            ("gitlab.example", true),
            ("evil.com", false),
            ("", false),
            ("[::1]", false),
        ] {
            assert_eq!(policy.permits(host), want, "{host:?}");
        }
        assert!(EgressPolicy::parse(Some("::1")).permits("[::1]"));
    }

    #[test]
    fn a_refusal_classifies_as_invalid_input() {
        let error = EgressPolicy::parse(Some("github.com"))
            .check("evil.example", "clone destination")
            .err();
        assert_eq!(
            error.map(|e| classify(e.error_type, &e.message)),
            Some("invalid_input")
        );
        let unset = EgressPolicy::parse(Some("")).check("github.com", "clone destination");
        assert_eq!(
            unset.err().map(|e| classify(e.error_type, &e.message)),
            Some("invalid_input")
        );
    }
}
