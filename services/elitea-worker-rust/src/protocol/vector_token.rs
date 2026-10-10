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

use std::fmt;

use zeroize::Zeroizing;

use super::control::ControlSemanticError;
use super::elitea::runtime::v1::VectorClaimTokenV1;

/// Every claim token starts with this.
const BEARER_PREFIX: &str = "elvc_";
/// The prefix and 43 base64url characters (32 random bytes).
const BEARER_LENGTH: usize = BEARER_PREFIX.len() + 43;
/// The most sources a token can name.
const MAX_SOURCES: usize = 4;

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
    /// Validates the receipt's token. A receipt without one is `Ok(None)`;
    /// a malformed one refuses the claim, as any malformed receipt field
    /// does.
    ///
    /// # Errors
    ///
    /// `AuthorizationFailed` for a bearer of the wrong shape, no expiry, or a
    /// missing, unknown or excessive source list.
    pub(crate) fn from_receipt(
        token: Option<VectorClaimTokenV1>,
    ) -> Result<Option<Self>, ControlSemanticError> {
        let Some(token) = token else {
            return Ok(None);
        };
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
            return Err(ControlSemanticError::AuthorizationFailed(
                "the accepted claim carries a malformed vector token",
            ));
        }
        if expires_at_unix_millis <= 0 {
            return Err(ControlSemanticError::AuthorizationFailed(
                "the accepted claim's vector token has no expiry",
            ));
        }
        if allowed_sources.is_empty() || allowed_sources.len() > MAX_SOURCES {
            return Err(ControlSemanticError::AuthorizationFailed(
                "the accepted claim's vector token names no or too many sources",
            ));
        }
        let mut sources = Vec::with_capacity(allowed_sources.len());
        for keyword in &allowed_sources {
            let source = VectorSource::from_keyword(keyword).ok_or(
                ControlSemanticError::AuthorizationFailed(
                    "the accepted claim's vector token names an unknown source",
                ),
            )?;
            if !sources.contains(&source) {
                sources.push(source);
            }
        }
        Ok(Some(Self {
            bearer,
            expires_at_unix_millis,
            sources,
        }))
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
        assert!(
            VectorClaimToken::from_receipt(None)
                .expect("absent")
                .is_none()
        );
    }

    #[test]
    fn a_well_formed_token_is_carried_with_its_sources() {
        let wire = test_vector_claim_token();
        let bearer = wire.bearer.clone();
        let token = VectorClaimToken::from_receipt(Some(wire))
            .expect("valid")
            .expect("present");
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
        let token = VectorClaimToken::from_receipt(Some(wire))
            .expect("valid")
            .expect("present");
        let printed = format!("{token:?} {:?}", Some(&token));
        assert!(!printed.contains(&bearer), "{printed}");
        assert!(printed.contains("<redacted>"), "{printed}");
    }

    #[test]
    fn a_malformed_token_refuses_the_claim() {
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
                expires_at_unix_millis: 0,
                ..good.clone()
            },
            VectorClaimTokenV1 {
                allowed_sources: Vec::new(),
                ..good.clone()
            },
            VectorClaimTokenV1 {
                allowed_sources: vec!["toolkit_index".to_owned(), "everything".to_owned()],
                ..good.clone()
            },
            VectorClaimTokenV1 {
                allowed_sources: vec!["toolkit_index".to_owned(); MAX_SOURCES + 1],
                ..good.clone()
            },
        ];
        for case in cases {
            let error = VectorClaimToken::from_receipt(Some(case)).expect_err("malformed");
            assert!(
                matches!(error, ControlSemanticError::AuthorizationFailed(_)),
                "{error:?}"
            );
        }
    }
}
