//! The egress host allowlist, shared by the engines' git ingest
//! (`elitea-repo-ingest`'s `EgressPolicy`, which wraps this with its own
//! messages) and every connector here.
//!
//! The rules are the Python `security/egress.py` and Go
//! `spi.ParseEgressPolicy` ones, byte for byte:
//!
//! * fail-closed: no entries refuses everything;
//! * `*` disables the control, explicitly;
//! * `*.example.com` matches DIRECT subdomains only (not the apex, not
//!   `a.b.example.com`);
//! * comparison is case-insensitive and ignores a port (`host:443`) and the
//!   brackets of an IPv6 literal.
//!
//! [`EgressGuard`] applies the list to every request a connector sends, not
//! only to its configured base URL: a provider's pagination can hand back an
//! absolute `next` URL, and that host is checked like any other.

use std::sync::Arc;

use async_trait::async_trait;

use crate::transport::{Request, Response, Transport, TransportError};

/// An allowlist of hosts.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct HostAllowlist {
    entries: Vec<String>,
}

impl HostAllowlist {
    /// Parse a comma- or whitespace-separated list (Python's
    /// `EgressPolicy.parse`). `None` or blank gives the empty list, which
    /// refuses everything.
    #[must_use]
    pub fn parse(raw: Option<&str>) -> Self {
        let entries = raw
            .unwrap_or_default()
            .replace(',', " ")
            .split_whitespace()
            .map(|part| part.trim().to_lowercase())
            .filter(|part| !part.is_empty())
            .collect();
        Self { entries }
    }

    /// The entries, lower-cased, in order.
    #[must_use]
    pub fn entries(&self) -> &[String] {
        &self.entries
    }

    /// The explicit opt-out.
    #[must_use]
    pub fn allows_everything(&self) -> bool {
        self.entries.iter().any(|entry| entry == "*")
    }

    /// No entries: every destination is refused.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    /// Whether `host` (optionally with a port) is on the list.
    #[must_use]
    pub fn permits(&self, host: &str) -> bool {
        if self.allows_everything() {
            return true;
        }
        let lowered = host.trim().to_lowercase();
        if lowered.is_empty() {
            return false;
        }
        let candidate = if let Some(rest) = lowered.strip_prefix('[') {
            rest.split(']').next().unwrap_or_default()
        } else if lowered.matches(':').count() == 1 {
            lowered.split(':').next().unwrap_or_default()
        } else {
            lowered.as_str()
        };
        self.entries.iter().any(|entry| {
            if let Some(suffix) = entry.strip_prefix('*') {
                // `suffix` keeps its leading dot: ".github.com".
                suffix.starts_with('.')
                    && candidate
                        .strip_suffix(suffix)
                        .is_some_and(|label| !label.contains('.'))
            } else {
                candidate == entry
            }
        })
    }

    /// Whether a URL's own host is on the list (a URL without a host never
    /// is).
    #[must_use]
    pub fn permits_url(&self, url: &url::Url) -> bool {
        url.host_str().is_some_and(|host| self.permits(host))
    }
}

/// A transport that refuses, before sending, any request whose host is not
/// on the allowlist. Fail-closed: an empty list refuses every request.
pub struct EgressGuard {
    allowlist: HostAllowlist,
    inner: Arc<dyn Transport>,
}

impl EgressGuard {
    #[must_use]
    pub fn new(allowlist: HostAllowlist, inner: Arc<dyn Transport>) -> Self {
        Self { allowlist, inner }
    }

    /// The list this guard applies.
    #[must_use]
    pub fn allowlist(&self) -> &HostAllowlist {
        &self.allowlist
    }
}

#[async_trait]
impl Transport for EgressGuard {
    async fn execute(&self, request: Request) -> Result<Response, TransportError> {
        if !self.allowlist.permits_url(request.url()) {
            return Err(TransportError::other());
        }
        self.inner.execute(request).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::transport::{HeaderMap, Method, StatusCode, Url};
    use std::sync::atomic::{AtomicUsize, Ordering};

    #[test]
    fn the_go_shared_rules_hold() {
        // spi_test.go TestEgressPolicyRulesAreTheSharedOnes.
        let policy = HostAllowlist::parse(Some("github.com, *.github.com GITLAB.example"));
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
        assert!(HostAllowlist::parse(Some("::1")).permits("[::1]"));
        assert!(HostAllowlist::parse(None).is_empty());
        assert!(!HostAllowlist::parse(Some(" , ")).permits("github.com"));
        assert!(HostAllowlist::parse(Some("*")).permits("anything.at.all"));
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
    async fn the_guard_refuses_before_sending() {
        let inner = Arc::new(Counting(AtomicUsize::new(0)));
        let guard = EgressGuard::new(HostAllowlist::parse(Some("api.github.com")), inner.clone());
        let allowed = Url::parse("https://api.github.com/repos").expect("url");
        let refused = Url::parse("https://evil.example/repos").expect("url");
        assert!(
            guard
                .execute(Request::new(Method::GET, allowed))
                .await
                .is_ok()
        );
        assert!(
            guard
                .execute(Request::new(Method::GET, refused))
                .await
                .is_err()
        );
        let closed = EgressGuard::new(HostAllowlist::default(), inner.clone());
        let any = Url::parse("https://api.github.com/").expect("url");
        assert!(
            closed
                .execute(Request::new(Method::GET, any))
                .await
                .is_err()
        );
        assert_eq!(inner.0.load(Ordering::SeqCst), 1);
    }
}
