//! The ref allowlist (ADR-0034 decision 2), as Main enforces it on every
//! publish. The client checks it too, so a wrong ref fails before upload.

use crate::error::StoreError;

pub const BASE: &str = "refs/elitea/base";
pub const UPSTREAM: &str = "refs/elitea/upstream";
pub const HANDOFF_PREFIX: &str = "refs/elitea/handoff/";
pub const CLOUD: &str = "refs/elitea/cloud";

/// The `refs/elitea/handoff/N` ref of handoff `n` (Phase 1: always 1).
#[must_use]
pub fn handoff(n: u32) -> String {
    format!("{HANDOFF_PREFIX}{n}")
}

/// Who writes a publish.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Writer {
    /// `base`, `upstream` and `handoff/N`, only while `HANDING_OFF`.
    Desktop,
    /// `refs/elitea/cloud` only, only while `CLOUD_RUNNING`.
    Executor,
}

/// `N` of `handoff/N`: a positive decimal without leading zeros.
fn handoff_number(name: &str) -> Option<u32> {
    let rest = name.strip_prefix(HANDOFF_PREFIX)?;
    if rest.is_empty() || rest.len() > 9 || rest.starts_with('0') {
        return None;
    }
    rest.bytes()
        .all(|b| b.is_ascii_digit())
        .then(|| rest.parse().ok())
        .flatten()
}

/// Whether `writer` may write `name`.
#[must_use]
pub fn allowed(writer: Writer, name: &str) -> bool {
    match writer {
        Writer::Desktop => name == BASE || name == UPSTREAM || handoff_number(name).is_some(),
        Writer::Executor => name == CLOUD,
    }
}

/// An allowlist as a begin response names it: exact names, or a pattern
/// ending in `/*` that matches one numeric component.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Allowlist {
    patterns: Vec<String>,
}

impl Allowlist {
    #[must_use]
    pub fn new(patterns: impl IntoIterator<Item = String>) -> Self {
        Self {
            patterns: patterns.into_iter().collect(),
        }
    }

    #[must_use]
    pub fn matches(&self, name: &str) -> bool {
        self.patterns
            .iter()
            .any(|pattern| match pattern.strip_suffix('*') {
                Some(prefix) if prefix.ends_with('/') => {
                    name.strip_prefix(prefix).is_some_and(|rest| {
                        !rest.is_empty() && rest.bytes().all(|b| b.is_ascii_digit())
                    })
                }
                _ => pattern == name,
            })
    }
}

/// Refuse a publish that names a ref `writer` may not write, or one the
/// server's `allowed` list does not hold, or no ref at all.
///
/// # Errors
///
/// [`StoreError::RefNotAllowed`] naming the ref.
pub fn check(
    writer: Writer,
    allowlist: Option<&Allowlist>,
    names: &[&str],
) -> Result<(), StoreError> {
    if names.is_empty() {
        return Err(StoreError::RefNotAllowed("a publish names no ref".into()));
    }
    for name in names {
        if !allowed(writer, name) || allowlist.is_some_and(|list| !list.matches(name)) {
            return Err(StoreError::RefNotAllowed((*name).to_owned()));
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_desktop_writes_base_upstream_and_handoffs_only() {
        for ok in [
            BASE,
            UPSTREAM,
            "refs/elitea/handoff/1",
            "refs/elitea/handoff/12",
        ] {
            assert!(allowed(Writer::Desktop, ok), "{ok}");
        }
        for refused in [
            CLOUD,
            "refs/heads/main",
            "refs/tags/v1",
            "refs/elitea/handoff/0",
            "refs/elitea/handoff/01",
            "refs/elitea/handoff/x",
            "refs/elitea/handoff/",
            "refs/elitea/handoff/1/2",
            "refs/elitea/base2",
        ] {
            assert!(!allowed(Writer::Desktop, refused), "{refused}");
        }
    }

    #[test]
    fn the_executor_writes_the_cloud_ref_only() {
        assert!(allowed(Writer::Executor, CLOUD));
        for refused in [BASE, UPSTREAM, "refs/elitea/handoff/1", "refs/heads/x"] {
            assert!(!allowed(Writer::Executor, refused));
        }
    }

    #[test]
    fn the_servers_list_narrows_further() {
        let list = Allowlist::new([BASE.to_owned(), "refs/elitea/handoff/*".to_owned()]);
        assert!(
            check(
                Writer::Desktop,
                Some(&list),
                &[BASE, "refs/elitea/handoff/1"]
            )
            .is_ok()
        );
        assert_eq!(
            check(Writer::Desktop, Some(&list), &[UPSTREAM])
                .unwrap_err()
                .code(),
            "ref_not_allowed"
        );
        assert!(check(Writer::Desktop, None, &[]).is_err());
        assert!(!list.matches("refs/elitea/handoff/a"));
    }
}
