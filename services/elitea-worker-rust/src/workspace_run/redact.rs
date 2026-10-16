//! The harvested env of a handed-off thread (contract 6.4) and its
//! redaction.
//!
//! The values are redeemed over `WorkspaceEnvSecret`, held here in memory
//! only (zeroized on drop, never formatted), handed to the sandbox for the
//! `run_command` environment, and removed from every tool result before the
//! result reaches the model, the transcript or the trace: each value of 4 or
//! more characters, and its base64 forms (standard and URL-safe, padded and
//! not), becomes [`REDACTED`].

use std::fmt;
use std::path::Path;
use std::sync::Arc;

use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::{STANDARD, STANDARD_NO_PAD, URL_SAFE, URL_SAFE_NO_PAD};
use serde_json::Value;
use zeroize::Zeroizing;

use super::sandbox::{SandboxRunError, WorkspaceSandbox};

/// What a redacted value reads as.
pub(crate) const REDACTED: &str = "[REDACTED]";
/// Shorter values are not redacted (they would erase ordinary text).
pub(crate) const MIN_REDACTED_CHARS: usize = 4;

/// The thread's harvested env. Not `Clone`, not `Debug` with values.
#[derive(Default)]
pub(crate) struct SecretEnv {
    vars: Vec<(String, Zeroizing<String>)>,
    /// Values redacted like the vars but never handed to the sandbox as
    /// such: the egress run token (it reaches the sandbox only inside the
    /// proxy URLs), under the name the redaction reports use.
    redact_only: Vec<(String, Zeroizing<String>)>,
}

/// The variables that point a command at the egress proxy (host-allowlist
/// mode). Harvested values of these names are replaced, never merged.
pub(crate) const PROXY_VARS: [&str; 4] = ["HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"];
/// Cleared in proxy mode: every host goes through the proxy.
pub(crate) const NO_PROXY_VARS: [&str; 2] = ["NO_PROXY", "no_proxy"];
/// Dropped from a harvested env in proxy mode: a laptop's proxy for other
/// schemes means nothing in the cloud, and the proxy speaks HTTP only.
const DROPPED_PROXY_VARS: [&str; 2] = ["ALL_PROXY", "all_proxy"];
/// The name the run token is reported under when it is redacted from a file.
pub(crate) const EGRESS_TOKEN_NAME: &str = "ELITEA_EGRESS_PROXY_TOKEN";

impl SecretEnv {
    #[must_use]
    pub(crate) fn new(vars: Vec<(String, String)>) -> Self {
        Self {
            vars: vars
                .into_iter()
                .map(|(name, value)| (name, Zeroizing::new(value)))
                .collect(),
            redact_only: Vec::new(),
        }
    }

    /// Host-allowlist mode: the command environment reaches the egress proxy
    /// with the run token (`HTTPS_PROXY` and the rest, `proxy_url` already
    /// carrying `run:<token>@`), every host goes through it (empty
    /// `NO_PROXY`), and the token is redacted wherever a value is.
    pub(crate) fn set_egress_proxy(&mut self, proxy_url: &str, token: &str) {
        self.vars.retain(|(name, _)| {
            !PROXY_VARS.contains(&name.as_str())
                && !NO_PROXY_VARS.contains(&name.as_str())
                && !DROPPED_PROXY_VARS.contains(&name.as_str())
        });
        for name in PROXY_VARS {
            self.vars
                .push((name.to_owned(), Zeroizing::new(proxy_url.to_owned())));
        }
        for name in NO_PROXY_VARS {
            self.vars
                .push((name.to_owned(), Zeroizing::new(String::new())));
        }
        self.redact_only.push((
            EGRESS_TOKEN_NAME.to_owned(),
            Zeroizing::new(token.to_owned()),
        ));
    }

    /// Every value to redact, with its name: the vars and the redact-only
    /// values.
    pub(crate) fn redaction_pairs(&self) -> impl Iterator<Item = (&str, &str)> {
        self.vars
            .iter()
            .chain(self.redact_only.iter())
            .map(|(name, value)| (name.as_str(), value.as_str()))
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) fn is_empty(&self) -> bool {
        self.vars.is_empty()
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) fn len(&self) -> usize {
        self.vars.len()
    }

    /// The pairs, for the sandbox's `run_command` environment only.
    pub(crate) fn pairs(&self) -> impl Iterator<Item = (&str, &str)> {
        self.vars
            .iter()
            .map(|(name, value)| (name.as_str(), value.as_str()))
    }

    /// The redactor of these values.
    #[must_use]
    pub(crate) fn redactor(&self) -> Redactor {
        let mut needles: Vec<Zeroizing<String>> = Vec::new();
        for (_, value) in self.redaction_pairs() {
            if value.chars().count() < MIN_REDACTED_CHARS {
                continue;
            }
            let bytes = value.as_bytes();
            for form in [
                value.to_owned(),
                STANDARD.encode(bytes),
                STANDARD_NO_PAD.encode(bytes),
                URL_SAFE.encode(bytes),
                URL_SAFE_NO_PAD.encode(bytes),
            ] {
                if !needles.iter().any(|seen| seen.as_str() == form) {
                    needles.push(Zeroizing::new(form));
                }
            }
        }
        // Longest first, so a value is replaced before a shorter one inside it.
        needles.sort_by_key(|needle| std::cmp::Reverse(needle.len()));
        Redactor {
            needles: Arc::new(needles),
        }
    }
}

impl fmt::Debug for SecretEnv {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SecretEnv")
            .field("vars", &self.vars.len())
            .field("redact_only", &self.redact_only.len())
            .finish()
    }
}

/// Replaces the env's values (and their base64 forms) in text and JSON.
#[derive(Clone, Default)]
pub(crate) struct Redactor {
    needles: Arc<Vec<Zeroizing<String>>>,
}

impl fmt::Debug for Redactor {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Redactor")
            .field("needles", &self.needles.len())
            .finish()
    }
}

impl Redactor {
    #[must_use]
    pub(crate) fn is_empty(&self) -> bool {
        self.needles.is_empty()
    }

    #[must_use]
    pub(crate) fn text(&self, text: &str) -> String {
        let mut out = text.to_owned();
        for needle in self.needles.iter() {
            if out.contains(needle.as_str()) {
                out = out.replace(needle.as_str(), REDACTED);
            }
        }
        out
    }

    /// Every string in `value`, object keys included.
    #[must_use]
    pub(crate) fn json(&self, value: Value) -> Value {
        if self.is_empty() {
            return value;
        }
        match value {
            Value::String(text) => Value::String(self.text(&text)),
            Value::Array(items) => {
                Value::Array(items.into_iter().map(|item| self.json(item)).collect())
            }
            Value::Object(map) => Value::Object(
                map.into_iter()
                    .map(|(key, item)| (self.text(&key), self.json(item)))
                    .collect(),
            ),
            other => other,
        }
    }
}

/// The turn's sandbox as the tools see it: every result redacted.
pub(crate) struct RedactingSandbox {
    inner: Arc<dyn WorkspaceSandbox>,
    redactor: Redactor,
}

impl RedactingSandbox {
    #[must_use]
    pub(crate) fn new(inner: Arc<dyn WorkspaceSandbox>, redactor: Redactor) -> Self {
        Self { inner, redactor }
    }
}

#[async_trait]
impl WorkspaceSandbox for RedactingSandbox {
    async fn push_tree(&self, worktree: &Path) -> Result<(), SandboxRunError> {
        self.inner.push_tree(worktree).await
    }

    async fn call_tool(
        &self,
        tool: &str,
        call_id: &str,
        args: Value,
    ) -> Result<Value, SandboxRunError> {
        self.inner
            .call_tool(tool, call_id, args)
            .await
            .map(|result| self.redactor.json(result))
    }

    async fn call_tool_approved(
        &self,
        tool: &str,
        call_id: &str,
        args: Value,
        approved: bool,
    ) -> Result<Value, SandboxRunError> {
        self.inner
            .call_tool_approved(tool, call_id, args, approved)
            .await
            .map(|result| self.redactor.json(result))
    }

    async fn classify_command(
        &self,
        command: &str,
        cwd: Option<&str>,
    ) -> Result<bool, SandboxRunError> {
        self.inner.classify_command(command, cwd).await
    }

    async fn pull_tree(&self, worktree: &Path) -> Result<(), SandboxRunError> {
        self.inner.pull_tree(worktree).await
    }

    async fn destroy(&self) -> Result<(), SandboxRunError> {
        self.inner.destroy().await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn values_and_their_base64_forms_are_redacted_and_short_ones_are_not() {
        let env = SecretEnv::new(vec![
            ("DATABASE_URL".into(), "postgres://app:s3cret@db/app".into()),
            ("API_TOKEN".into(), "tok-123456".into()),
            ("SHORT".into(), "abc".into()),
        ]);
        let redactor = env.redactor();
        let encoded = STANDARD.encode("tok-123456");
        let url_safe = URL_SAFE_NO_PAD.encode("postgres://app:s3cret@db/app");
        let output = json!({
            "stdout": format!("url=postgres://app:s3cret@db/app token=tok-123456 b64={encoded} u={url_safe} abc"),
            "tok-123456": ["nested tok-123456"],
        });
        let redacted = redactor.json(output).to_string();
        for secret in ["s3cret", "tok-123456", encoded.as_str(), url_safe.as_str()] {
            assert!(!redacted.contains(secret), "{secret} in {redacted}");
        }
        assert!(
            redacted.contains("abc"),
            "a value under 4 characters is kept"
        );
        assert!(redacted.contains(REDACTED));
        assert_eq!(format!("{env:?}"), "SecretEnv { vars: 3, redact_only: 0 }");
        assert!(!format!("{redactor:?}").contains("tok"));
    }

    #[test]
    fn proxy_mode_sets_the_proxy_vars_replaces_harvested_ones_and_redacts_the_token() {
        let token = crate::workspace_run::context::TEST_TOKEN;
        let mut env = SecretEnv::new(vec![
            ("HTTPS_PROXY".into(), "http://laptop-proxy:8080".into()),
            ("no_proxy".into(), "internal.example".into()),
            ("ALL_PROXY".into(), "socks5://laptop:1080".into()),
            ("API_TOKEN".into(), "tok-123456".into()),
        ]);
        let url = format!("http://run:{token}@10.96.0.40:3128");
        env.set_egress_proxy(&url, token);
        let pairs: Vec<(&str, &str)> = env.pairs().collect();
        for name in PROXY_VARS {
            assert_eq!(
                pairs.iter().filter(|(n, _)| *n == name).collect::<Vec<_>>(),
                [&(name, url.as_str())],
                "{name}"
            );
        }
        for name in NO_PROXY_VARS {
            assert!(pairs.contains(&(name, "")), "{name}");
        }
        assert!(!pairs.iter().any(|(name, _)| *name == "ALL_PROXY"));
        assert!(pairs.contains(&("API_TOKEN", "tok-123456")));
        // The token is never a variable of its own.
        assert!(!pairs.iter().any(|(name, _)| *name == EGRESS_TOKEN_NAME));
        let redacted = env
            .redactor()
            .json(json!({"stdout": format!("{url}\npassword={token}\nb64={}", URL_SAFE_NO_PAD.encode(token))}))
            .to_string();
        assert!(!redacted.contains(token), "{redacted}");
        assert!(
            !redacted.contains(&URL_SAFE_NO_PAD.encode(token)),
            "{redacted}"
        );
        assert!(!format!("{env:?}").contains("ert1"));
    }
}
