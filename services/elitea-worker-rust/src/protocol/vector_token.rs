//! The per-claim elitea-vector token (ADR-0031 decision 1).
//!
//! Main mints one bearer per accepted claim of a capability that may use
//! vectors and returns it once, in the claim receipt
//! (`ClaimReceiptV1.vector_token`). It is bound to the claim's execution, the
//! execution's resource project and actor, and the sources it may read and
//! write; Main revokes it as soon as the claim settles, is cancelled or is
//! lost.
//!
//! The worker keeps it in memory only, inside the claim that carried it, and
//! hands it to toolkit and agent code as a [`VectorClaimToken`]. The value is
//! zeroized on drop and has no `Clone`, `Display` or serialization surface;
//! its `Debug` is redacted. It is never spooled: the execution spool holds
//! output frames, and nothing here can become one.

use std::collections::HashSet;
use std::fmt;
use std::sync::{LazyLock, Mutex};

use zeroize::Zeroizing;

use super::elitea::runtime::v1::VectorClaimTokenV1;

/// Every claim token starts with this.
const BEARER_PREFIX: &str = "elvc_";
/// The prefix and 43 base64url characters (32 random bytes).
const BEARER_LENGTH: usize = BEARER_PREFIX.len() + 43;
/// The most distinct sources a token can carry (the keywords this build
/// knows).
const MAX_SOURCES: usize = 3;
/// The most unknown keywords remembered for the log-once rule.
const MAX_REMEMBERED_UNKNOWN: usize = 16;

static LOGGED_UNKNOWN_SOURCES: LazyLock<Mutex<HashSet<String>>> =
    LazyLock::new(|| Mutex::new(HashSet::new()));

/// Logs an unknown source keyword the first time this process sees it.
fn log_unknown_source_once(keyword: &str) {
    let shown: String = keyword
        .chars()
        .filter(|character| !character.is_control())
        .take(64)
        .collect();
    let Ok(mut logged) = LOGGED_UNKNOWN_SOURCES.lock() else {
        return;
    };
    if logged.len() < MAX_REMEMBERED_UNKNOWN && logged.insert(shown.clone()) {
        tracing::warn!(
            source = shown,
            "the claim's vector token names a source this worker does not know; ignoring it"
        );
    }
}

/// A vector source a token may be admitted for, by its payload keyword.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub(crate) enum VectorSource {
    /// `toolkit_index` (ADR-0030).
    ToolkitIndex,
    /// `deepwiki`.
    Deepwiki,
    /// `inventory`, reserved (ADR-0031 decision 7).
    Inventory,
}

impl VectorSource {
    fn from_keyword(keyword: &str) -> Option<Self> {
        match keyword {
            "toolkit_index" => Some(Self::ToolkitIndex),
            "deepwiki" => Some(Self::Deepwiki),
            "inventory" => Some(Self::Inventory),
            _ => None,
        }
    }

    /// The payload keyword elitea-vector uses.
    #[must_use]
    #[allow(
        dead_code,
        reason = "The vector-index client (ADR-0030) is its consumer."
    )]
    pub(crate) const fn keyword(self) -> &'static str {
        match self {
            Self::ToolkitIndex => "toolkit_index",
            Self::Deepwiki => "deepwiki",
            Self::Inventory => "inventory",
        }
    }
}

/// The typed handle toolkit and agent code uses to reach elitea-vector for
/// the claim it runs under. The `vector-index` client takes it by reference
/// and sends [`Self::bearer`] as `authorization: Bearer …`; nothing else may
/// read the bearer.
pub(crate) struct VectorClaimToken {
    bearer: Zeroizing<String>,
    expires_at_unix_millis: i64,
    sources: Vec<VectorSource>,
}

impl VectorClaimToken {
    /// Reads the receipt's token. The token only gates calls to
    /// elitea-vector, so this never fails the claim:
    ///
    /// * a receipt without a token is `None`;
    /// * a token that is malformed (a bearer of the wrong shape, no expiry,
    ///   no source it can use) is dropped and logged, without its value, and
    ///   the claim runs without one (its vector calls are refused
    ///   `UNAUTHENTICATED`);
    /// * a source keyword this build does not know (a newer Main) is ignored
    ///   and logged once per process; the known sources are kept.
    pub(crate) fn from_receipt(token: Option<VectorClaimTokenV1>) -> Option<Self> {
        let token = token?;
        // Owned from here so the wire copy is zeroized with it.
        let VectorClaimTokenV1 {
            bearer,
            expires_at_unix_millis,
            allowed_sources,
        } = token;
        let bearer = Zeroizing::new(bearer);
        let well_formed = bearer.len() == BEARER_LENGTH
            && bearer.starts_with(BEARER_PREFIX)
            && bearer[BEARER_PREFIX.len()..]
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-' || byte == b'_');
        if !well_formed {
            tracing::warn!("the claim's vector token is malformed; running without it");
            return None;
        }
        if expires_at_unix_millis <= 0 {
            tracing::warn!("the claim's vector token has no expiry; running without it");
            return None;
        }
        let mut sources = Vec::with_capacity(MAX_SOURCES);
        for keyword in &allowed_sources {
            match VectorSource::from_keyword(keyword) {
                Some(source) => {
                    if !sources.contains(&source) {
                        sources.push(source);
                    }
                }
                None => log_unknown_source_once(keyword),
            }
        }
        if sources.is_empty() {
            tracing::warn!(
                "the claim's vector token names no source this worker knows; running without it"
            );
            return None;
        }
        Some(Self {
            bearer,
            expires_at_unix_millis,
            sources,
        })
    }

    /// The bearer, for the `authorization` metadata of an elitea-vector
    /// call and nothing else.
    #[must_use]
    #[allow(dead_code)] // The vector-index client (ADR-0030) is its consumer.
    pub(crate) fn bearer(&self) -> &str {
        self.bearer.as_str()
    }

    /// The latest instant the token can be valid. Main revokes it earlier
    /// when the claim ends.
    #[must_use]
    #[allow(dead_code)] // The vector-index client (ADR-0030) is its consumer.
    pub(crate) const fn expires_at_unix_millis(&self) -> i64 {
        self.expires_at_unix_millis
    }

    /// Whether the token is admitted for `source`. elitea-vector enforces
    /// this too; checking first spares a refused call.
    #[must_use]
    #[allow(dead_code)] // The vector-index client (ADR-0030) is its consumer.
    pub(crate) fn allows(&self, source: VectorSource) -> bool {
        self.sources.contains(&source)
    }

    /// A second owned copy for another authority derived from the same claim
    /// (the runtime-context authority an agent run receives). Crate-private,
    /// and deliberately not `Clone`, so copies are made only at those seams.
    #[must_use]
    pub(super) fn duplicate(&self) -> Self {
        Self {
            bearer: Zeroizing::new(self.bearer.as_str().to_owned()),
            expires_at_unix_millis: self.expires_at_unix_millis,
            sources: self.sources.clone(),
        }
    }
}

impl fmt::Debug for VectorClaimToken {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("VectorClaimToken")
            .field("bearer", &"<redacted>")
            .field("expires_at_unix_millis", &self.expires_at_unix_millis)
            .field("sources", &self.sources)
            .finish()
    }
}

#[cfg(test)]
pub(crate) fn test_vector_claim_token() -> VectorClaimTokenV1 {
    VectorClaimTokenV1 {
        bearer: format!("{BEARER_PREFIX}{}", "A".repeat(43)),
        expires_at_unix_millis: 4_102_444_800_000,
        allowed_sources: vec!["toolkit_index".to_owned()],
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_receipt_without_a_token_carries_none() {
        assert!(VectorClaimToken::from_receipt(None).is_none());
    }

    #[test]
    fn a_well_formed_token_is_carried_with_its_sources() {
        let wire = test_vector_claim_token();
        let bearer = wire.bearer.clone();
        let token = VectorClaimToken::from_receipt(Some(wire)).expect("present");
        assert_eq!(token.bearer(), bearer);
        assert_eq!(token.expires_at_unix_millis(), 4_102_444_800_000);
        assert!(token.allows(VectorSource::ToolkitIndex));
        assert!(!token.allows(VectorSource::Deepwiki));
        assert!(!token.allows(VectorSource::Inventory));
        assert_eq!(VectorSource::ToolkitIndex.keyword(), "toolkit_index");
        let copy = token.duplicate();
        assert_eq!(copy.bearer(), bearer);
        assert!(copy.allows(VectorSource::ToolkitIndex));
    }

    #[test]
    fn the_bearer_never_reaches_debug_output() {
        let wire = test_vector_claim_token();
        let bearer = wire.bearer.clone();
        let token = VectorClaimToken::from_receipt(Some(wire)).expect("present");
        let printed = format!("{token:?} {:?}", Some(&token));
        assert!(!printed.contains(&bearer), "{printed}");
        assert!(printed.contains("<redacted>"), "{printed}");
    }

    #[test]
    fn a_malformed_token_is_dropped_and_never_fails() {
        let good = test_vector_claim_token();
        let cases = [
            VectorClaimTokenV1 {
                bearer: String::new(),
                ..good.clone()
            },
            VectorClaimTokenV1 {
                bearer: good.bearer[..BEARER_LENGTH - 1].to_owned(),
                ..good.clone()
            },
            VectorClaimTokenV1 {
                bearer: format!("{}A", good.bearer),
                ..good.clone()
            },
            VectorClaimTokenV1 {
                bearer: format!("elvx_{}", "A".repeat(43)),
                ..good.clone()
            },
            VectorClaimTokenV1 {
                bearer: format!("{BEARER_PREFIX}{}+", "A".repeat(42)),
                ..good.clone()
            },
            VectorClaimTokenV1 {
                bearer: format!("{BEARER_PREFIX}{}\n", "A".repeat(42)),
                ..good.clone()
            },
            VectorClaimTokenV1 {
                bearer: "not a token at all \u{1f510}".to_owned(),
                ..good.clone()
            },
            VectorClaimTokenV1 {
                expires_at_unix_millis: 0,
                ..good.clone()
            },
            VectorClaimTokenV1 {
                expires_at_unix_millis: -5,
                ..good.clone()
            },
            VectorClaimTokenV1 {
                allowed_sources: Vec::new(),
                ..good.clone()
            },
            VectorClaimTokenV1 {
                allowed_sources: vec!["everything".to_owned()],
                ..good.clone()
            },
        ];
        for case in cases {
            assert!(VectorClaimToken::from_receipt(Some(case)).is_none());
        }
    }

    #[test]
    fn an_unknown_source_is_ignored_and_the_known_ones_are_kept() {
        let wire = VectorClaimTokenV1 {
            allowed_sources: vec![
                "toolkit_index".to_owned(),
                "a_future_source".to_owned(),
                "deepwiki".to_owned(),
                "a_future_source".to_owned(),
                "toolkit_index".to_owned(),
            ],
            ..test_vector_claim_token()
        };
        let token = VectorClaimToken::from_receipt(Some(wire)).expect("kept");
        assert!(token.allows(VectorSource::ToolkitIndex));
        assert!(token.allows(VectorSource::Deepwiki));
        assert!(!token.allows(VectorSource::Inventory));
        assert_eq!(token.sources.len(), 2);
    }

    #[test]
    fn an_unknown_source_is_logged_once_per_process() {
        let keyword = "only_logged_once_in_this_test";
        log_unknown_source_once(keyword);
        log_unknown_source_once(keyword);
        let logged = LOGGED_UNKNOWN_SOURCES.lock().expect("lock");
        assert_eq!(logged.iter().filter(|name| *name == keyword).count(), 1);
    }
}
