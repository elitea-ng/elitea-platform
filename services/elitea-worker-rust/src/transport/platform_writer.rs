//! The cloud [`PlatformWriter`]: the runtime's platform writes over the
//! claim-bound runtime-context routes (ADR-0029 decision 2).
//!
//! It holds the turn's `PlatformClient` and the single minted
//! `ClaimBoundRuntimeContextAuthority`, shared behind an `Arc` and never
//! duplicated (the authority is neither cloneable nor formattable), so the
//! runtime's `artifact` family acts under the live claim without ever seeing
//! it. Each `RuntimeContextError` maps onto the [`HostErrorCode`] the runtime
//! reads, keeping its own `runtime_context.*` code as the log reason.

use std::sync::Arc;

use async_trait::async_trait;
use elitea_agent_runtime::host::{HostError, HostErrorCode, PlatformWriter};

use super::platform_client::PlatformClient;
use super::runtime_context::{
    ArtifactDeleteOutcome, ArtifactDeleteRequest, ArtifactListOutcome, ArtifactListRequest,
    ArtifactReadOutcome, ArtifactReadRequest, ArtifactWriteOutcome, ArtifactWriteRequest,
    ProjectContextWriteOutcome, ProjectContextWriteRequest, RuntimeContextError, SkillWriteOutcome,
    SkillWriteRequest,
};
use crate::protocol::control::ClaimBoundRuntimeContextAuthority;

/// Platform writes for one claimed turn.
pub(crate) struct ClaimPlatformWriter {
    platform: Arc<PlatformClient>,
    authority: Arc<ClaimBoundRuntimeContextAuthority>,
}

impl ClaimPlatformWriter {
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

/// The runtime-context failure as the runtime classifies it. A refused
/// document (main's 422, `Rejected`) is invalid input; transport failures and
/// timeouts are an unavailable dependency, as `RuntimeContextError::retryable`
/// already treats them.
pub(crate) fn host_error(error: &RuntimeContextError) -> HostError {
    let (code, message) = match error {
        RuntimeContextError::InvalidConfiguration(message) => {
            (HostErrorCode::InvalidConfiguration, *message)
        }
        RuntimeContextError::InvalidResponse(message) => (HostErrorCode::InvalidResponse, *message),
        RuntimeContextError::ResourceExhausted(message) => {
            (HostErrorCode::ResourceExhausted, *message)
        }
        RuntimeContextError::AuthorizationFailed(message) => {
            (HostErrorCode::AuthorizationFailed, *message)
        }
        RuntimeContextError::NotFound(message) => (HostErrorCode::NotFound, *message),
        RuntimeContextError::Rejected(message) => (HostErrorCode::InvalidInput, *message),
        RuntimeContextError::DependencyUnavailable(message)
        | RuntimeContextError::Timeout(message) => (HostErrorCode::DependencyUnavailable, *message),
        RuntimeContextError::Transport(_) => (
            HostErrorCode::DependencyUnavailable,
            "the runtime-context transport failed",
        ),
    };
    HostError::new(code, message).with_reason(error.code())
}

#[async_trait]
impl PlatformWriter for ClaimPlatformWriter {
    async fn list_artifacts(
        &self,
        request: &ArtifactListRequest,
    ) -> Result<ArtifactListOutcome, HostError> {
        self.platform
            .list_artifacts(&self.authority, request)
            .await
            .map_err(|error| host_error(&error))
    }

    async fn read_artifact(
        &self,
        request: &ArtifactReadRequest,
    ) -> Result<ArtifactReadOutcome, HostError> {
        self.platform
            .read_artifact(&self.authority, request)
            .await
            .map_err(|error| host_error(&error))
    }

    async fn write_artifact(
        &self,
        request: &ArtifactWriteRequest,
    ) -> Result<ArtifactWriteOutcome, HostError> {
        self.platform
            .write_artifact(&self.authority, request)
            .await
            .map_err(|error| host_error(&error))
    }

    async fn delete_artifact(
        &self,
        request: &ArtifactDeleteRequest,
    ) -> Result<ArtifactDeleteOutcome, HostError> {
        self.platform
            .delete_artifact(&self.authority, request)
            .await
            .map_err(|error| host_error(&error))
    }

    async fn write_skill(
        &self,
        request: &SkillWriteRequest,
    ) -> Result<SkillWriteOutcome, HostError> {
        self.platform
            .write_skill(&self.authority, request)
            .await
            .map_err(|error| host_error(&error))
    }

    async fn write_project_context(
        &self,
        request: &ProjectContextWriteRequest,
    ) -> Result<ProjectContextWriteOutcome, HostError> {
        self.platform
            .write_project_context(&self.authority, request)
            .await
            .map_err(|error| host_error(&error))
    }
}

/// The platform writer of a path that lends the Office host and nothing else
/// (`toolkit.call_tool.v1` of `pptx` / `imagegen`): those families act only
/// through the host, and the runtime-context writes are not part of that
/// path, so every one is refused rather than silently routed anywhere.
pub(crate) struct HostOnlyPlatformWriter;

fn not_on_this_path() -> HostError {
    HostError::new(
        HostErrorCode::InvalidConfiguration,
        "this path lends no runtime-context writes",
    )
}

#[async_trait]
impl PlatformWriter for HostOnlyPlatformWriter {
    async fn list_artifacts(
        &self,
        _: &ArtifactListRequest,
    ) -> Result<ArtifactListOutcome, HostError> {
        Err(not_on_this_path())
    }

    async fn read_artifact(
        &self,
        _: &ArtifactReadRequest,
    ) -> Result<ArtifactReadOutcome, HostError> {
        Err(not_on_this_path())
    }

    async fn write_artifact(
        &self,
        _: &ArtifactWriteRequest,
    ) -> Result<ArtifactWriteOutcome, HostError> {
        Err(not_on_this_path())
    }

    async fn delete_artifact(
        &self,
        _: &ArtifactDeleteRequest,
    ) -> Result<ArtifactDeleteOutcome, HostError> {
        Err(not_on_this_path())
    }

    async fn write_skill(&self, _: &SkillWriteRequest) -> Result<SkillWriteOutcome, HostError> {
        Err(not_on_this_path())
    }

    async fn write_project_context(
        &self,
        _: &ProjectContextWriteRequest,
    ) -> Result<ProjectContextWriteOutcome, HostError> {
        Err(not_on_this_path())
    }
}

/// The platform writer of a direct `artifact` tool call
/// (`toolkit.call_tool.v1`, D4): the claim's artifact LIST and READ, exactly
/// as the agent path makes them (same claim-bound routes, so the same project,
/// actor and per-bucket access list on Main's side), and nothing else. The
/// direct path runs read-only tools only; a write, a delete or a builder write
/// is refused here as well, so no tool could reach one even if it were let
/// through.
pub(crate) struct ReadOnlyClaimPlatformWriter {
    claim: ClaimPlatformWriter,
}

impl ReadOnlyClaimPlatformWriter {
    #[must_use]
    pub(crate) const fn new(
        platform: Arc<PlatformClient>,
        authority: Arc<ClaimBoundRuntimeContextAuthority>,
    ) -> Self {
        Self {
            claim: ClaimPlatformWriter::new(platform, authority),
        }
    }
}

fn read_only_path() -> HostError {
    HostError::new(
        HostErrorCode::InvalidConfiguration,
        "a direct toolkit call lends artifact reads only",
    )
}

#[async_trait]
impl PlatformWriter for ReadOnlyClaimPlatformWriter {
    async fn list_artifacts(
        &self,
        request: &ArtifactListRequest,
    ) -> Result<ArtifactListOutcome, HostError> {
        self.claim.list_artifacts(request).await
    }

    async fn read_artifact(
        &self,
        request: &ArtifactReadRequest,
    ) -> Result<ArtifactReadOutcome, HostError> {
        self.claim.read_artifact(request).await
    }

    async fn write_artifact(
        &self,
        _: &ArtifactWriteRequest,
    ) -> Result<ArtifactWriteOutcome, HostError> {
        Err(read_only_path())
    }

    async fn delete_artifact(
        &self,
        _: &ArtifactDeleteRequest,
    ) -> Result<ArtifactDeleteOutcome, HostError> {
        Err(read_only_path())
    }

    async fn write_skill(&self, _: &SkillWriteRequest) -> Result<SkillWriteOutcome, HostError> {
        Err(read_only_path())
    }

    async fn write_project_context(
        &self,
        _: &ProjectContextWriteRequest,
    ) -> Result<ProjectContextWriteOutcome, HostError> {
        Err(read_only_path())
    }
}

#[cfg(test)]
mod tests {
    use elitea_agent_runtime::host::HostErrorCode;

    use super::{RuntimeContextError, host_error};
    use crate::transport::runtime_context::RuntimeContextTransportError;

    #[test]
    fn runtime_context_failures_keep_their_class_and_reason_code() {
        let cases = [
            (
                RuntimeContextError::Rejected("too large"),
                HostErrorCode::InvalidInput,
                "runtime_context.rejected",
            ),
            (
                RuntimeContextError::NotFound("gone"),
                HostErrorCode::NotFound,
                "runtime_context.not_found",
            ),
            (
                RuntimeContextError::AuthorizationFailed("denied"),
                HostErrorCode::AuthorizationFailed,
                "runtime_context.authorization_failed",
            ),
            (
                RuntimeContextError::ResourceExhausted("big"),
                HostErrorCode::ResourceExhausted,
                "runtime_context.resource_exhausted",
            ),
            (
                RuntimeContextError::InvalidResponse("odd"),
                HostErrorCode::InvalidResponse,
                "runtime_context.invalid_response",
            ),
            (
                RuntimeContextError::Timeout("slow"),
                HostErrorCode::DependencyUnavailable,
                "runtime_context.timeout",
            ),
            (
                RuntimeContextError::Transport(RuntimeContextTransportError::Unavailable),
                HostErrorCode::DependencyUnavailable,
                "runtime_context.dependency_unavailable",
            ),
            (
                RuntimeContextError::DependencyUnavailable("down"),
                HostErrorCode::DependencyUnavailable,
                "runtime_context.dependency_unavailable",
            ),
            (
                RuntimeContextError::InvalidConfiguration("bad origin"),
                HostErrorCode::InvalidConfiguration,
                "runtime_context.invalid_configuration",
            ),
        ];
        for (error, code, reason) in cases {
            let mapped = host_error(&error);
            assert_eq!(mapped.code(), code);
            assert_eq!(mapped.reason_code(), reason);
        }
    }
}
