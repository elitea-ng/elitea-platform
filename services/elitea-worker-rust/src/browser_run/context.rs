//! The runtime-context `browser` field (ADR-0049 decisions 5 and 8, #1596).
//!
//! Main serves it only to a worker that names it in the accept list, and
//! only for a project whose `browser` setting allows the cloud browser. It
//! is parsed as strictly as the rest of the runtime context: an unknown key
//! refuses the redemption.

use serde::Deserialize;

use crate::workspace_run::context::{EGRESS_UNAVAILABLE_REASONS, ProxyToken, proxy_host_port};

/// The browser's own reasons for egress `unavailable` (Main's
/// browserruntime): Main degrades the field instead of failing the
/// redemption.
pub(crate) const BROWSER_ALLOWLIST_TOO_LARGE: &str = "browser_allowlist_too_large";
const BROWSER_UNAVAILABLE_REASONS: [&str; 3] = [
    BROWSER_ALLOWLIST_TOO_LARGE,
    "browser_allowlist_invalid",
    "browser_context_unavailable",
];

/// The accept-list name of the field.
pub(crate) const RUNTIME_CONTEXT_FIELD: &str = "browser";

/// The longest run token the proxy accepts (runtoken.MaxTokenBytes).
const MAX_TOKEN_BYTES: usize = 8 << 10;
/// The longest run wall a field may name (24 h).
const MAX_RUN_SECONDS: u64 = 24 * 3600;
/// The most monthly seconds a project may hold.
const MAX_QUOTA_SECONDS: u64 = 100_000_000;
/// The most allowed domains a field carries (Main's own bound).
const MAX_ALLOW_DOMAINS: usize = 128;
const MAX_HOST_BYTES: usize = 261;

/// The egress modes Main serves for a browser session. A browser never runs
/// direct: its only route out is the egress proxy.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "lowercase")]
pub(crate) enum BrowserEgressMode {
    /// The egress proxy, with the run token.
    Proxy,
    /// No proxy this deployment can give the session: the browser tools
    /// answer `browser_unavailable`.
    Unavailable,
}

/// The `egress` object of the `browser` field.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub(crate) struct BrowserEgress {
    pub(crate) mode: BrowserEgressMode,
    /// `http://host:port`, no credentials; empty unless the mode is proxy.
    pub(crate) proxy_url: String,
    pub(crate) token: ProxyToken,
    /// Why the proxy is unavailable; empty otherwise.
    pub(crate) reason: String,
}

/// The `browser` field.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub(crate) struct BrowserRuntimeContext {
    /// Only `full` (ADR-0049 decision 2).
    pub(crate) engine: String,
    pub(crate) egress: BrowserEgress,
    /// The run's cap in engine seconds; 0 is no cap.
    pub(crate) max_run_seconds: u64,
    /// The project's monthly engine seconds; 0 is no quota.
    pub(crate) monthly_quota_seconds: u64,
    /// The engine seconds the project used this month before this run.
    pub(crate) month_used_seconds: u64,
    /// A schedule or a webhook started the run: nobody can answer.
    pub(crate) unattended: bool,
    /// The project's own allowed domains, for the step record only: the
    /// proxy enforces the token's allowlist.
    pub(crate) allow_domains: Vec<String>,
}

impl BrowserRuntimeContext {
    /// Check what serde cannot.
    ///
    /// # Errors
    ///
    /// A fixed sentence naming what is wrong.
    pub(crate) fn validate(&self) -> Result<(), &'static str> {
        if self.engine != elitea_agent_runtime::toolkits::BROWSER_ENGINE_FULL {
            return Err("the browser engine is not admitted");
        }
        if self.max_run_seconds > MAX_RUN_SECONDS
            || self.monthly_quota_seconds > MAX_QUOTA_SECONDS
            || self.month_used_seconds > MAX_QUOTA_SECONDS.saturating_mul(10)
        {
            return Err("the browser limits are malformed");
        }
        if self.allow_domains.len() > MAX_ALLOW_DOMAINS
            || self.allow_domains.iter().any(|domain| {
                domain.is_empty()
                    || domain.len() > MAX_HOST_BYTES
                    || domain.chars().any(|c| c.is_control() || c.is_whitespace())
            })
        {
            return Err("the browser allowed domains are malformed");
        }
        let egress = &self.egress;
        let valid = match egress.mode {
            BrowserEgressMode::Proxy => {
                proxy_host_port(&egress.proxy_url).is_some()
                    && valid_token(egress.token.expose())
                    && egress.reason.is_empty()
            }
            BrowserEgressMode::Unavailable => {
                egress.proxy_url.is_empty()
                    && egress.token.expose().is_empty()
                    && (EGRESS_UNAVAILABLE_REASONS.contains(&egress.reason.as_str())
                        || BROWSER_UNAVAILABLE_REASONS.contains(&egress.reason.as_str()))
            }
        };
        if !valid {
            return Err("the browser egress is malformed");
        }
        Ok(())
    }
}

fn valid_token(token: &str) -> bool {
    token.starts_with("ert1.")
        && token.len() <= MAX_TOKEN_BYTES
        && token
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
}

#[cfg(test)]
pub(crate) mod tests {
    use serde_json::{Value, json};

    use super::*;

    pub(crate) fn field() -> Value {
        json!({
            "engine": "full",
            "egress": {
                "mode": "proxy",
                "proxy_url": "http://egress-proxy:3128",
                "token": "ert1.abc.def",
                "reason": "",
            },
            "max_run_seconds": 1800,
            "monthly_quota_seconds": 36000,
            "month_used_seconds": 120,
            "unattended": true,
            "allow_domains": ["shop.example"],
        })
    }

    fn parse(value: Value) -> Result<BrowserRuntimeContext, &'static str> {
        let parsed: BrowserRuntimeContext =
            serde_json::from_value(value).map_err(|_| "serde refused it")?;
        parsed.validate().map(|()| parsed)
    }

    #[test]
    fn a_proxy_field_parses() {
        let parsed = parse(field()).expect("valid");
        assert_eq!(parsed.egress.mode, BrowserEgressMode::Proxy);
        assert!(parsed.unattended);
        assert_eq!(parsed.max_run_seconds, 1800);
        assert!(
            !format!("{parsed:?}").contains("ert1.abc"),
            "the token never formats"
        );
    }

    #[test]
    fn an_unavailable_field_parses() {
        let mut value = field();
        value["egress"] = json!({
            "mode": "unavailable",
            "proxy_url": "",
            "token": "",
            "reason": "egress_proxy_not_enabled",
        });
        assert_eq!(
            parse(value).expect("valid").egress.mode,
            BrowserEgressMode::Unavailable
        );
        // Main's degraded field: the browser's own reasons parse too.
        for reason in BROWSER_UNAVAILABLE_REASONS {
            let mut value = field();
            value["egress"] = json!({
                "mode": "unavailable", "proxy_url": "", "token": "", "reason": reason,
            });
            assert!(parse(value).is_ok(), "{reason}");
        }
    }

    #[test]
    fn a_malformed_field_is_refused() {
        let mut unknown = field();
        unknown["headless"] = json!(false);
        let mut lite = field();
        lite["engine"] = json!("lite");
        let mut direct = field();
        direct["egress"]["mode"] = json!("direct");
        let mut credentialed = field();
        credentialed["egress"]["proxy_url"] = json!("http://run:secret@egress-proxy:3128");
        let mut no_token = field();
        no_token["egress"]["token"] = json!("");
        let mut unknown_reason = field();
        unknown_reason["egress"] = json!({
            "mode": "unavailable", "proxy_url": "", "token": "", "reason": "because",
        });
        let mut long_run = field();
        long_run["max_run_seconds"] = json!(MAX_RUN_SECONDS + 1);
        let mut bad_domain = field();
        bad_domain["allow_domains"] = json!(["a b"]);
        for value in [
            unknown,
            lite,
            direct,
            credentialed,
            no_token,
            unknown_reason,
            long_run,
            bad_domain,
        ] {
            assert!(parse(value.clone()).is_err(), "{value}");
        }
    }
}
