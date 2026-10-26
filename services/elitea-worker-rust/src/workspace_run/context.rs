//! The runtime-context `workspace` field (ADR-0034 decision 9).
//!
//! Main serves it only to a worker that names it in
//! `X-Elitea-Runtime-Context-Accept`, and only for a cloud turn of a
//! handed-off thread. It is parsed as strictly as the rest of the runtime
//! context: an unknown key anywhere refuses the redemption, so a later Main
//! cannot slip a field this worker would silently ignore.

use std::fmt;

use serde::Deserialize;
use zeroize::Zeroizing;

/// The accept-list name of the field.
pub(crate) const RUNTIME_CONTEXT_FIELD: &str = "workspace";

const MAX_ALLOWLIST_HOSTS: usize = 256;
const MAX_HOST_BYTES: usize = 253;

/// The policy-derived limits of one workspace run.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub(crate) struct WorkspaceLimits {
    pub(crate) max_segment_bytes: u64,
    pub(crate) max_file_bytes: u64,
    pub(crate) max_files: u64,
    pub(crate) max_unpacked_bytes: u64,
    /// The unpacked tree size the desktop declared at send: the disk-backed
    /// workspace volume is sized from it.
    pub(crate) unpacked_bytes: u64,
    /// Zero: the deployment default.
    pub(crate) turn_wall_seconds: u64,
    pub(crate) cpu_millis: u64,
    pub(crate) memory_bytes: u64,
    pub(crate) disk_bytes: u64,
}

/// The `workspace` field: which thread, which epoch, what the turn may do.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
#[allow(
    clippy::struct_excessive_bools,
    reason = "Main's wire format: one switch per policy"
)]
pub(crate) struct WorkspaceRuntimeContext {
    pub(crate) thread: String,
    pub(crate) epoch: u64,
    /// `refs/elitea/cloud` at claim time: the publish's expected old value.
    /// `null` before the first cloud turn of the handoff.
    pub(crate) expected_old_cloud: Option<String>,
    pub(crate) limits: WorkspaceLimits,
    pub(crate) network: bool,
    pub(crate) shell: bool,
    pub(crate) network_allowlist: Vec<String>,
    pub(crate) has_upstream: bool,
    /// The thread has harvested env (contract 6.4): the worker redeems it
    /// over `WorkspaceEnvSecret`. The values are never in this field.
    pub(crate) env_secret: bool,
    /// This turn regenerates the newest cloud turn (ADR-0034 decision 7):
    /// before it runs, `refs/elitea/cloud` moves back from
    /// `expected_old_cloud` (the regenerated turn's commit) to its parent,
    /// through the normal publish compare-and-set. Absent from an older
    /// Main: false.
    #[serde(default)]
    pub(crate) rewind: bool,
    /// How the run reaches the network (decision 9, "Host allowlist: the
    /// egress proxy"). Absent from a Main that predates the proxy: derived
    /// from `network` and `network_allowlist`, with an allowlist then
    /// unenforceable (see [`Self::egress_mode`]).
    #[serde(default)]
    pub(crate) egress: Option<WorkspaceEgress>,
    /// The commit a rewind moves the cloud ref back to: the commit the
    /// regenerated turn started from, a first-parent ancestor of
    /// `expected_old_cloud`. `null` with no rewind. Absent from an older Main
    /// (which named none): the rewind goes to the first parent.
    #[serde(default)]
    pub(crate) rewind_to: Option<String>,
    /// Whether the turn may open a pull request (ADR-0034 decision 10,
    /// amendment 2026-10-26): `off`, `ask` (a person approves each one) or
    /// `allow`. Absent from an older Main: `ask`.
    #[serde(default)]
    pub(crate) pull_requests: PullRequestMode,
    /// The thread's owner chose a GitHub credential for its pull requests:
    /// the worker redeems it over `WorkspaceForgeCredential` when the tool
    /// runs. The credential is never in this field.
    #[serde(default)]
    pub(crate) pull_request_credential: bool,
}

/// The pull-request switch of the `workspace` field.
#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "lowercase")]
pub(crate) enum PullRequestMode {
    /// No `open_pull_request`.
    Off,
    /// Each pull request is a HITL interrupt before it is opened.
    #[default]
    Ask,
    /// Pull requests open without an approval.
    Allow,
}

/// The egress modes Main serves.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "lowercase")]
pub(crate) enum EgressMode {
    /// Network off: no interface but loopback, no DNS.
    None,
    /// Network on, no host allowlist: the public internet minus the deny
    /// ranges.
    Direct,
    /// A host allowlist, enforced by the egress proxy: the run reaches only
    /// the proxy, with the run token.
    Proxy,
    /// A host allowlist this deployment cannot enforce: the turn is refused.
    Unavailable,
}

/// The run token: a credential. Never formatted, zeroized on drop.
#[derive(Clone, Eq, PartialEq)]
pub(crate) struct ProxyToken(Zeroizing<String>);

impl<'de> Deserialize<'de> for ProxyToken {
    fn deserialize<D: serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        String::deserialize(deserializer).map(|token| Self(Zeroizing::new(token)))
    }
}

impl ProxyToken {
    /// No token (every mode but proxy).
    #[cfg(test)]
    #[must_use]
    pub(crate) fn empty() -> Self {
        Self(Zeroizing::new(String::new()))
    }

    #[must_use]
    pub(crate) fn expose(&self) -> &str {
        self.0.as_str()
    }
}

impl fmt::Debug for ProxyToken {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(if self.0.is_empty() {
            "\"\""
        } else {
            "[REDACTED]"
        })
    }
}

/// The `egress` object of the `workspace` field.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub(crate) struct WorkspaceEgress {
    pub(crate) mode: EgressMode,
    /// `http://host:port`, no credentials; empty unless the mode is proxy.
    pub(crate) proxy_url: String,
    pub(crate) token: ProxyToken,
    /// Why an allowlist is unavailable; empty otherwise.
    pub(crate) reason: String,
}

/// The reasons Main gives for mode `unavailable`.
pub(crate) const EGRESS_UNAVAILABLE_REASONS: [&str; 3] = [
    "egress_proxy_not_enabled",
    "egress_token_signing_key_not_configured",
    "egress_token_mint_failed",
];

/// The longest run token the proxy accepts (runtoken.MaxTokenBytes).
const MAX_TOKEN_BYTES: usize = 8 << 10;

/// A run token's characters: base64url and `.` (runtoken's wire format), so
/// it is a proxy-URL password without escaping.
fn valid_token(token: &str) -> bool {
    token.starts_with("ert1.")
        && token.len() <= MAX_TOKEN_BYTES
        && token
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
}

/// `http://host:port` with nothing else: the host and the port.
pub(crate) fn proxy_host_port(url: &str) -> Option<(&str, u16)> {
    let rest = url.strip_prefix("http://")?;
    let rest = rest.strip_suffix('/').unwrap_or(rest);
    if rest.is_empty() || rest.contains(['/', '@', '?', '#', ' ']) {
        return None;
    }
    let (host, port) = rest.rsplit_once(':')?;
    let port: u16 = port.parse().ok().filter(|port| *port != 0)?;
    let host = host
        .strip_prefix('[')
        .and_then(|inner| inner.strip_suffix(']'))
        .unwrap_or(host);
    let valid = !host.is_empty()
        && host.len() <= MAX_HOST_BYTES
        && host
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'-' | b':'));
    valid.then_some((host, port))
}

impl WorkspaceRuntimeContext {
    /// Check what serde cannot: identifiers, bounds, and that the limits are
    /// a usable whole.
    pub(crate) fn validate(&self) -> Result<(), &'static str> {
        if !canonical_uuid(&self.thread) {
            return Err("the workspace thread is malformed");
        }
        if self.epoch == 0 || i64::try_from(self.epoch).is_err() {
            return Err("the workspace epoch is malformed");
        }
        if self
            .expected_old_cloud
            .as_deref()
            .is_some_and(|sha| !sha1_hex(sha))
        {
            return Err("the expected cloud commit is malformed");
        }
        let limits = &self.limits;
        if limits.max_segment_bytes == 0
            || limits.max_file_bytes == 0
            || limits.max_files == 0
            || limits.max_unpacked_bytes == 0
            || limits.unpacked_bytes > limits.max_unpacked_bytes
        {
            return Err("the workspace limits are malformed");
        }
        if self.rewind && self.expected_old_cloud.is_none() {
            return Err("a rewind without a cloud turn to rewind is malformed");
        }
        match (&self.rewind_to, self.rewind) {
            (Some(_), false) => return Err("a rewind target without a rewind is malformed"),
            (Some(to), true) if !sha1_hex(to) || self.expected_old_cloud.as_ref() == Some(to) => {
                return Err("the rewind target is malformed");
            }
            _ => {}
        }
        if !self.network && !self.network_allowlist.is_empty() {
            return Err("a network allowlist without network is malformed");
        }
        if self.network_allowlist.len() > MAX_ALLOWLIST_HOSTS
            || self.network_allowlist.iter().any(|host| !valid_host(host))
        {
            return Err("the network allowlist is malformed");
        }
        if let Some(egress) = &self.egress {
            let consistent = match egress.mode {
                EgressMode::None => !self.network,
                EgressMode::Direct => self.network && self.network_allowlist.is_empty(),
                EgressMode::Proxy | EgressMode::Unavailable => {
                    self.network && !self.network_allowlist.is_empty()
                }
            };
            if !consistent {
                return Err("the egress mode contradicts the network policy");
            }
            let shaped = match egress.mode {
                EgressMode::Proxy => {
                    proxy_host_port(&egress.proxy_url).is_some()
                        && valid_token(egress.token.expose())
                        && egress.reason.is_empty()
                }
                EgressMode::Unavailable => {
                    egress.proxy_url.is_empty()
                        && egress.token.expose().is_empty()
                        && EGRESS_UNAVAILABLE_REASONS.contains(&egress.reason.as_str())
                }
                EgressMode::None | EgressMode::Direct => {
                    egress.proxy_url.is_empty()
                        && egress.token.expose().is_empty()
                        && egress.reason.is_empty()
                }
            };
            if !shaped {
                return Err("the egress object is malformed");
            }
        }
        Ok(())
    }

    /// The egress mode of this turn. A Main that predates the proxy serves no
    /// `egress`: an allowlist is then unenforceable, never direct.
    #[must_use]
    pub(crate) fn egress_mode(&self) -> EgressMode {
        match (
            &self.egress,
            self.network,
            self.network_allowlist.is_empty(),
        ) {
            (Some(egress), _, _) => egress.mode,
            (None, false, _) => EgressMode::None,
            (None, true, true) => EgressMode::Direct,
            (None, true, false) => EgressMode::Unavailable,
        }
    }

    /// Main's reason for mode `unavailable` (a fixed string), or the one an
    /// older Main implies.
    #[must_use]
    pub(crate) fn egress_unavailable_reason(&self) -> &str {
        self.egress
            .as_ref()
            .map_or("egress_proxy_not_enabled", |egress| egress.reason.as_str())
    }
}

pub(crate) fn sha1_hex(value: &str) -> bool {
    value.len() == 40
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

pub(crate) fn canonical_uuid(value: &str) -> bool {
    value.len() == 36
        && value.bytes().enumerate().all(|(index, byte)| match index {
            8 | 13 | 18 | 23 => byte == b'-',
            _ => byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte),
        })
}

fn valid_host(host: &str) -> bool {
    !host.is_empty()
        && host.len() <= MAX_HOST_BYTES
        && host.bytes().all(|byte| {
            byte.is_ascii_lowercase() || byte.is_ascii_digit() || matches!(byte, b'.' | b'-' | b'*')
        })
}

#[cfg(test)]
pub(crate) fn fixture() -> WorkspaceRuntimeContext {
    WorkspaceRuntimeContext {
        thread: "0b9c6f4e-9a7f-4d4e-8f0e-1d2c3b4a5f60".to_owned(),
        epoch: 2,
        expected_old_cloud: None,
        limits: WorkspaceLimits {
            max_segment_bytes: 200 << 20,
            max_file_bytes: 25 << 20,
            max_files: 20_000,
            max_unpacked_bytes: 2 << 30,
            unpacked_bytes: 5 << 20,
            turn_wall_seconds: 0,
            cpu_millis: 0,
            memory_bytes: 0,
            disk_bytes: 0,
        },
        network: true,
        shell: true,
        network_allowlist: Vec::new(),
        has_upstream: true,
        env_secret: false,
        rewind: false,
        egress: Some(WorkspaceEgress {
            mode: EgressMode::Direct,
            proxy_url: String::new(),
            token: ProxyToken(Zeroizing::new(String::new())),
            reason: String::new(),
        }),
        rewind_to: None,
        pull_requests: PullRequestMode::Ask,
        pull_request_credential: false,
    }
}

/// A proxy-mode token for tests (not a signed token: the worker never
/// verifies it, the proxy does).
#[cfg(test)]
pub(crate) const TEST_TOKEN: &str = "ert1.0123456789abcdef.eyJ4IjoxfQ.c2lnbmF0dXJlLWJ5dGVz";

/// The fixture in proxy mode.
#[cfg(test)]
pub(crate) fn proxy_fixture() -> WorkspaceRuntimeContext {
    let mut context = fixture();
    context.network_allowlist = vec!["pypi.org".to_owned()];
    context.egress = Some(WorkspaceEgress {
        mode: EgressMode::Proxy,
        proxy_url: "http://10.96.0.40:3128".to_owned(),
        token: ProxyToken(Zeroizing::new(TEST_TOKEN.to_owned())),
        reason: String::new(),
    });
    context
}

#[cfg(test)]
mod tests {
    use super::{
        EgressMode, ProxyToken, PullRequestMode, TEST_TOKEN, WorkspaceRuntimeContext, fixture,
        proxy_fixture, proxy_host_port,
    };

    fn raw() -> serde_json::Value {
        serde_json::json!({
            "thread": "0b9c6f4e-9a7f-4d4e-8f0e-1d2c3b4a5f60",
            "epoch": 2,
            "expected_old_cloud": null,
            "limits": {
                "max_segment_bytes": 209_715_200_u64, "max_file_bytes": 26_214_400_u64,
                "max_files": 20_000, "max_unpacked_bytes": 2_147_483_648_u64,
                "unpacked_bytes": 5_242_880, "turn_wall_seconds": 0, "cpu_millis": 0,
                "memory_bytes": 0, "disk_bytes": 0
            },
            "network": true, "shell": true, "network_allowlist": [], "has_upstream": true,
            "env_secret": false,
            "egress": {"mode": "direct", "proxy_url": "", "token": "", "reason": ""}
        })
    }

    #[test]
    fn the_field_parses_strictly() {
        let parsed: WorkspaceRuntimeContext =
            serde_json::from_value(raw()).expect("the field Main serves");
        assert_eq!(parsed, fixture());
        assert_eq!(parsed.validate(), Ok(()));

        let mut extra = raw();
        extra["future_key"] = serde_json::json!(1);
        assert!(serde_json::from_value::<WorkspaceRuntimeContext>(extra).is_err());
        let mut egress = raw();
        egress["egress"]["future"] = serde_json::json!(1);
        assert!(serde_json::from_value::<WorkspaceRuntimeContext>(egress).is_err());
        let mut mode = raw();
        mode["egress"]["mode"] = serde_json::json!("open");
        assert!(serde_json::from_value::<WorkspaceRuntimeContext>(mode).is_err());
        let mut nested = raw();
        nested["limits"]["future_limit"] = serde_json::json!(1);
        assert!(serde_json::from_value::<WorkspaceRuntimeContext>(nested).is_err());
        let mut missing = raw();
        missing.as_object_mut().expect("object").remove("shell");
        assert!(serde_json::from_value::<WorkspaceRuntimeContext>(missing).is_err());
    }

    /// Main serves the pull-request switch and the credential flag; an older
    /// Main serves neither (ask, no credential).
    #[test]
    fn the_pull_request_switch_parses() {
        for (wire, mode) in [
            ("off", PullRequestMode::Off),
            ("ask", PullRequestMode::Ask),
            ("allow", PullRequestMode::Allow),
        ] {
            let mut raw = raw();
            raw["pull_requests"] = serde_json::json!(wire);
            raw["pull_request_credential"] = serde_json::json!(true);
            let parsed: WorkspaceRuntimeContext = serde_json::from_value(raw).expect("parsed");
            assert_eq!(parsed.pull_requests, mode);
            assert!(parsed.pull_request_credential);
        }
        let parsed: WorkspaceRuntimeContext = serde_json::from_value(raw()).expect("parsed");
        assert_eq!(parsed.pull_requests, PullRequestMode::Ask);
        assert!(!parsed.pull_request_credential);
        let mut unknown = raw();
        unknown["pull_requests"] = serde_json::json!("always");
        assert!(serde_json::from_value::<WorkspaceRuntimeContext>(unknown).is_err());
    }

    #[test]
    fn validation_refuses_malformed_identity_and_policy() {
        let mut context = fixture();
        context.epoch = 0;
        assert!(context.validate().is_err());
        let mut context = fixture();
        context.thread = "not-a-uuid".to_owned();
        assert!(context.validate().is_err());
        let mut context = fixture();
        context.expected_old_cloud = Some("ABC".to_owned());
        assert!(context.validate().is_err());
        let mut context = fixture();
        context.network = false;
        context.network_allowlist = vec!["pypi.org".to_owned()];
        assert!(context.validate().is_err());
        let mut context = fixture();
        context.limits.unpacked_bytes = context.limits.max_unpacked_bytes + 1;
        assert!(context.validate().is_err());
        let cloud = "1111111111111111111111111111111111111111".to_owned();
        let mut context = fixture();
        context.rewind_to = Some("2222222222222222222222222222222222222222".to_owned());
        assert!(context.validate().is_err(), "a target without a rewind");
        context.rewind = true;
        context.expected_old_cloud = Some(cloud.clone());
        assert_eq!(context.validate(), Ok(()));
        context.rewind_to = Some(cloud);
        assert!(
            context.validate().is_err(),
            "a target equal to the cloud commit"
        );
        context.rewind_to = Some("XYZ".to_owned());
        assert!(context.validate().is_err());
    }

    /// Main serves `rewind` and `rewind_to` on every field now.
    #[test]
    fn the_rewind_target_parses() {
        let mut raw = raw();
        raw["expected_old_cloud"] = serde_json::json!("1111111111111111111111111111111111111111");
        raw["rewind"] = serde_json::json!(true);
        raw["rewind_to"] = serde_json::json!("2222222222222222222222222222222222222222");
        let parsed: WorkspaceRuntimeContext = serde_json::from_value(raw).expect("parsed");
        assert!(parsed.rewind);
        assert_eq!(
            parsed.rewind_to.as_deref(),
            Some("2222222222222222222222222222222222222222")
        );
        assert_eq!(parsed.validate(), Ok(()));
    }

    #[test]
    fn the_egress_mode_must_match_the_policy_and_carry_only_its_values() {
        let context = proxy_fixture();
        assert_eq!(context.validate(), Ok(()));
        assert_eq!(context.egress_mode(), EgressMode::Proxy);
        // The token never prints.
        let printed = format!("{context:?}");
        assert!(!printed.contains(TEST_TOKEN), "{printed}");
        assert!(printed.contains("[REDACTED]"));

        let mut direct_with_allowlist = fixture();
        direct_with_allowlist.network_allowlist = vec!["pypi.org".to_owned()];
        assert!(
            direct_with_allowlist.validate().is_err(),
            "an allowlist is never direct"
        );
        let mut proxy_without_allowlist = proxy_fixture();
        proxy_without_allowlist.network_allowlist.clear();
        assert!(proxy_without_allowlist.validate().is_err());
        let mut bad_url = proxy_fixture();
        bad_url.egress.as_mut().expect("egress").proxy_url = "http://run:x@proxy:3128".to_owned();
        assert!(bad_url.validate().is_err());
        let mut bad_token = proxy_fixture();
        bad_token.egress.as_mut().expect("egress").token =
            ProxyToken(zeroize::Zeroizing::new("ert1.a b".to_owned()));
        assert!(bad_token.validate().is_err());
        let mut token_in_direct = fixture();
        token_in_direct.egress.as_mut().expect("egress").token =
            ProxyToken(zeroize::Zeroizing::new(TEST_TOKEN.to_owned()));
        assert!(token_in_direct.validate().is_err());
        let mut unavailable = proxy_fixture();
        let egress = unavailable.egress.as_mut().expect("egress");
        egress.mode = EgressMode::Unavailable;
        egress.proxy_url.clear();
        egress.token = ProxyToken(zeroize::Zeroizing::new(String::new()));
        egress.reason = "egress_proxy_not_enabled".to_owned();
        assert_eq!(unavailable.validate(), Ok(()));
        unavailable.egress.as_mut().expect("egress").reason = "whatever".to_owned();
        assert!(unavailable.validate().is_err());

        // A Main without the field: an allowlist is unavailable, not direct.
        let mut older = fixture();
        older.egress = None;
        older.network_allowlist = vec!["pypi.org".to_owned()];
        assert_eq!(older.egress_mode(), EgressMode::Unavailable);
        older.network_allowlist.clear();
        assert_eq!(older.egress_mode(), EgressMode::Direct);
        older.network = false;
        assert_eq!(older.egress_mode(), EgressMode::None);
    }

    #[test]
    fn the_proxy_url_is_http_host_port_only() {
        assert_eq!(
            proxy_host_port("http://elitea-egress-proxy.platform.svc.cluster.local:3128"),
            Some(("elitea-egress-proxy.platform.svc.cluster.local", 3128))
        );
        assert_eq!(
            proxy_host_port("http://[fd00::5]:3128/"),
            Some(("fd00::5", 3128))
        );
        for bad in [
            "https://p:3128",
            "http://p",
            "http://p:0",
            "http://u:pw@p:3128",
            "http://p:3128/x",
            "http://:3128",
        ] {
            assert_eq!(proxy_host_port(bad), None, "{bad}");
        }
    }
}
