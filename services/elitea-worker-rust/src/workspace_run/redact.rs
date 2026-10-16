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
}

impl SecretEnv {
    #[must_use]
    pub(crate) fn new(vars: Vec<(String, String)>) -> Self {
        Self {
            vars: vars
                .into_iter()
                .map(|(name, value)| (name, Zeroizing::new(value)))
                .collect(),
        }
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
        for (_, value) in &self.vars {
            if value.chars().count() < MIN_REDACTED_CHARS {
                continue;
            }
            let bytes = value.as_bytes();
            for form in [
                value.to_string(),
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
        assert_eq!(format!("{env:?}"), "SecretEnv { vars: 3 }");
        assert!(!format!("{redactor:?}").contains("tok"));
    }
}
