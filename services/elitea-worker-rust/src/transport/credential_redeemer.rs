//! The cloud [`CredentialRedeemer`]: governed toolkit credentials redeemed
//! per invocation over `RuntimeContentService.RedeemCredential` (ADR-0039
//! decision 10, #1120), under the turn's one claim-bound authority.
//!
//! Built only for a governed command (the authority names its security
//! context digest); a legacy command never builds one, so its toolkits keep
//! their claim-time settings and nothing here runs.

use std::sync::Arc;

use async_trait::async_trait;
use elitea_agent_runtime::toolkits::governed::{
    AuthAdapterRegistry, BoundToolInvocation, CredentialRedeemer, GovernedOperationClient,
    RedeemFailure, RedeemOutcome, adapters::reference_registry,
};

use super::platform_client::PlatformClient;
use crate::protocol::control::ClaimBoundRuntimeContextAuthority;

/// Redemptions for one claimed governed turn.
pub(crate) struct ClaimCredentialRedeemer {
    platform: Arc<PlatformClient>,
    authority: Arc<ClaimBoundRuntimeContextAuthority>,
}

impl ClaimCredentialRedeemer {
    #[must_use]
    pub(crate) const fn new(
        platform: Arc<PlatformClient>,
        authority: Arc<ClaimBoundRuntimeContextAuthority>,
    ) -> Self {
        Self {
            platform,
            authority,
        }
    }
}

#[async_trait]
impl CredentialRedeemer for ClaimCredentialRedeemer {
    async fn redeem(
        &self,
        invocation: BoundToolInvocation,
    ) -> Result<RedeemOutcome, RedeemFailure> {
        self.platform
            .redeem_credential(&self.authority, invocation)
            .await
    }
}

/// The governed seam of one turn: the reference adapters and this claim's
/// redeemer. `None` for a legacy command: the wrapper is not installed.
#[must_use]
pub(crate) fn governed_client(
    platform: &Arc<PlatformClient>,
    authority: &Arc<ClaimBoundRuntimeContextAuthority>,
) -> Option<Arc<GovernedOperationClient>> {
    governed_client_with(platform, authority, reference_registry())
}

pub(crate) fn governed_client_with(
    platform: &Arc<PlatformClient>,
    authority: &Arc<ClaimBoundRuntimeContextAuthority>,
    registry: AuthAdapterRegistry,
) -> Option<Arc<GovernedOperationClient>> {
    authority.governed_context_digest()?;
    Some(Arc::new(GovernedOperationClient::new(
        Arc::new(registry),
        Arc::new(ClaimCredentialRedeemer::new(
            Arc::clone(platform),
            Arc::clone(authority),
        )),
    )))
}
