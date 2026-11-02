//! Governed toolkits for nested agents and saved pipelines (ADR-0039, #1474).
//!
//! A nested agent or a saved pipeline builds its configured toolkits and MCP
//! connections from its OWN version's frozen tool list. On a governed run it
//! uses the run's one governed seam, the same path an ordinary agent uses:
//!
//! - a credentialed configured toolkit redeems per invocation through its
//!   adapter;
//! - an MCP connection redeems `mcp.tools.list` once at build time and
//!   `mcp.tool.call` per call (resource = the tool name).
//!
//! Main checks each redemption against the references it froze for the run
//! (decision 7, check 4). Delegation narrowing (ADR-0044 decision 3) keeps a
//! child's toolkits inside its caller's, so a child toolkit is a toolkit the
//! root holds and Main froze. A toolkit Main did not freeze is denied.
//!
//! The paths fail closed:
//!
//! - a governed run that reaches this path without its governed seam is
//!   refused;
//! - a configured toolkit no adapter governs refuses the build;
//! - an MCP connection the run cannot govern (no saved toolkit id, its own
//!   credential headers, a refused listing) refuses the build. These paths
//!   have no run notice to name a left-out connection in.
//!
//! A legacy run is unchanged: it builds with its claim-time settings.

use std::sync::Arc;

use adk_rust::Toolset;
use serde_json::{Map, Value};

use super::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};
use crate::protocol::control::ClaimBoundRuntimeContextAuthority;
use crate::toolkits::{
    AdmittedToolSnapshot, DelegatedAuthorizationCatalog, IndexToolSupport, IndexToolsWithheld,
    McpConnector, McpMaterializationError, ToolAdmissionPolicy, ToolsetMaterializationError,
    materialize_configured_toolsets_with_authorities_governed, materialize_governed_mcp_toolsets,
    materialize_mcp_toolsets_with_tokens_and_authorization,
};
use elitea_agent_runtime::toolkits::governed::GovernedOperationClient;

/// How a call site names a materialization failure.
pub(in crate::agents) struct NestedErrorMapping {
    pub(in crate::agents) configured: fn(ToolsetMaterializationError) -> NativeAgentAssemblyError,
    pub(in crate::agents) mcp: fn(&McpMaterializationError) -> NativeAgentAssemblyError,
}

/// The toolsets of one nested agent or saved pipeline.
pub(in crate::agents) struct NestedToolsets {
    pub(in crate::agents) configured: Vec<Arc<dyn Toolset>>,
    pub(in crate::agents) mcp: Vec<Arc<dyn Toolset>>,
    pub(in crate::agents) delegated_authorization: DelegatedAuthorizationCatalog,
}

/// What one nested build needs.
pub(in crate::agents) struct NestedToolsetInputs<'a> {
    pub(in crate::agents) authority: &'a ClaimBoundRuntimeContextAuthority,
    pub(in crate::agents) governed: Option<&'a Arc<GovernedOperationClient>>,
    pub(in crate::agents) policy: &'a Arc<ToolAdmissionPolicy>,
    pub(in crate::agents) connector: &'a Arc<dyn McpConnector>,
    pub(in crate::agents) mcp_tokens: &'a Map<String, Value>,
}

/// Build the configured toolkits and MCP connections of `frozen`, governed
/// on a governed run.
///
/// # Errors
///
/// A refusal for a governed run without its seam or with a toolkit or
/// connection it cannot govern; otherwise the materializers' own errors,
/// named by `errors`.
pub(in crate::agents) async fn materialize_nested_toolsets(
    frozen: &AdmittedToolSnapshot<'_>,
    inputs: NestedToolsetInputs<'_>,
    errors: &NestedErrorMapping,
) -> Result<NestedToolsets, NativeAgentAssemblyError> {
    let governed = governed_seam(inputs.authority, inputs.governed)?;
    let (configured, mut delegated_authorization) =
        materialize_configured_toolsets_with_authorities_governed(
            frozen,
            inputs.policy,
            inputs.mcp_tokens,
            None,
            IndexToolSupport::Withheld(IndexToolsWithheld::NotServedOnThisPath),
            governed,
        )
        .await
        .map_err(errors.configured)?;
    let (mcp, mcp_authorization) = if let Some(governed) = governed {
        let built =
            materialize_governed_mcp_toolsets(frozen, inputs.connector, inputs.policy, governed)
                .await
                .map_err(|error| (errors.mcp)(&error))?;
        if !built.left_out.is_empty() {
            return Err(mcp_not_governable());
        }
        (built.toolsets, built.delegated_authorization)
    } else {
        materialize_mcp_toolsets_with_tokens_and_authorization(
            frozen,
            inputs.connector.as_ref(),
            inputs.policy,
            inputs.mcp_tokens,
        )
        .await
        .map_err(|error| (errors.mcp)(&error))?
    };
    delegated_authorization
        .merge(mcp_authorization)
        .map_err(|()| invalid_authorization_catalog())?;
    Ok(NestedToolsets {
        configured,
        mcp,
        delegated_authorization,
    })
}

/// The seam a governed run must have; `None` for a legacy run.
fn governed_seam<'a>(
    authority: &ClaimBoundRuntimeContextAuthority,
    governed: Option<&'a Arc<GovernedOperationClient>>,
) -> Result<Option<&'a Arc<GovernedOperationClient>>, NativeAgentAssemblyError> {
    match (authority.governed_context_digest().is_some(), governed) {
        (true, None) => {
            tracing::warn!(
                event = "agent_toolkit_refused",
                reason_code = "toolkit_not_governable",
                "a governed run reached a nested build without its governed seam; refused"
            );
            Err(NativeAgentAssemblyError::new(
                NativeAgentAssemblyErrorCode::UnsupportedCapability,
                "this run is governed by the enterprise credential profile, and a nested agent or saved pipeline has no governed credential path here",
            ))
        }
        // A legacy run never redeems, even when a seam was passed.
        (false, _) => Ok(None),
        (true, Some(governed)) => Ok(Some(governed)),
    }
}

const fn mcp_not_governable() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::UnsupportedCapability,
        "this run is governed by the enterprise credential profile, and an MCP connection of a nested agent or saved pipeline cannot be governed",
    )
}

const fn invalid_authorization_catalog() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidConfiguration,
        "the nested delegated authorization catalog is invalid",
    )
}
