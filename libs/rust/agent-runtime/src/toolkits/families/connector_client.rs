//! What the families add to the provider clients they share with the
//! indexing connectors (`elitea-connectors`, ADR-0030 decision 3): the
//! worker's reqwest 0.12 transport, and the mapping of a client error to the
//! `AdkError` the model and the run see.

use std::sync::Arc;

use adk_core::AdkError;
use elitea_connectors::reqwest012::ReqwestTransport;
use elitea_connectors::transport::Transport;
use elitea_connectors::{BuildError, ClientPolicy};

/// A provider client error as the agent sees it. The error types live in
/// `elitea-connectors`, which knows nothing of adk; each family states its
/// mapping here, unchanged from when the type was its own.
pub(crate) trait IntoAdk {
    fn into_adk(self) -> AdkError;
}

/// This host's transport for one provider client: reqwest 0.12 (rustls on
/// ring), built to the provider's policy.
pub(crate) fn transport(policy: &ClientPolicy) -> Result<Arc<dyn Transport>, BuildError> {
    Ok(Arc::new(ReqwestTransport::hardened(policy)?))
}
