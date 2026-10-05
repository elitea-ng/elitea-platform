#[allow(
    clippy::wildcard_imports,
    reason = "Share the owner module imports with runtime code and its existing tests."
)]
use super::*;
use crate::sandbox::code_platform_owner::CodePlatformBinding;
const DOMAIN: &[u8] = b"elitea.sandbox.platform-broker-owner-grant.ed25519.v1\0";
#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
pub(crate) enum CodePlatformOwnerOperation {
    ReadRetainedRuntime,
    ReadPendingPlatformCall,
    PublishCommittedPlatformReply,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct CodePlatformOwnerGrantClaims {
    pub(crate) schema: String,
    pub(crate) purpose: String,
    pub(crate) tenant_id: String,
    pub(crate) project_id: i32,
    pub(crate) execution_id: String,
    pub(crate) original_generation: u64,
    pub(crate) claim_id: String,
    pub(crate) claim_attempt: u64,
    pub(crate) lease_epoch: u64,
    pub(crate) fence_sha256: String,
    pub(crate) activation_id: String,
    pub(crate) attempt: u16,
    pub(crate) dispatch_activation: String,
    pub(crate) job_key: String,
    pub(crate) request_digest: String,
    pub(crate) binding_sha256: String,
    pub(crate) prepared_job_sha256: String,
    pub(crate) prepared_fingerprint: String,
    #[serde(deserialize_with = "required_nullable")]
    pub(crate) compiled_execute:
        Option<crate::sandbox::code_platform_owner::CodePlatformCompiledExecute>,
    pub(crate) policy_sha256: String,
    pub(crate) max_calls: u16,
    pub(crate) max_total_bytes: u32,
    pub(crate) supervisor_audience: String,
    pub(crate) requester_workload_identity: String,
    pub(crate) operation: CodePlatformOwnerOperation,
    #[serde(deserialize_with = "required_nullable")]
    pub(crate) sequence: Option<u16>,
    #[serde(deserialize_with = "required_nullable")]
    pub(crate) platform_request_sha256: Option<String>,
    #[serde(deserialize_with = "required_nullable")]
    pub(crate) committed_reply_sha256: Option<String>,
    pub(crate) issued_at_unix_millis: i64,
    pub(crate) expires_at_unix_millis: i64,
}
pub(crate) struct AuthorizedCodePlatformOwner {
    scope: JobScope,
    execution: String,
    generation: u64,
    activation: String,
    attempt: u16,
    dispatch: String,
    audience: String,
    binding_sha256: String,
    broker: CodePlatformBinding,
    operation: CodePlatformOwnerOperation,
    reply_sha256: Option<String>,
    expires: i64,
}
impl AuthorizedCodePlatformOwner {
    pub(crate) fn scope(&self) -> &JobScope {
        &self.scope
    }
    pub(crate) fn operation(&self) -> CodePlatformOwnerOperation {
        self.operation
    }
    pub(crate) fn permits(
        &self,
        binding: &WholeCodeBinding,
        broker: &CodePlatformBinding,
        now: i64,
    ) -> bool {
        now < self.expires
            && binding.valid()
            && broker.matches(binding)
            && broker == &self.broker
            && binding.execution_id == self.execution
            && binding.original_generation == self.generation
            && binding.activation_id == self.activation
            && binding.attempt == self.attempt
            && binding.dispatch_activation == self.dispatch
            && binding.supervisor_audience == self.audience
            && binding
                .canonical_bytes()
                .is_ok_and(|wire| sha256(&wire) == self.binding_sha256)
    }
    /// Observe admission only. This does not authorize a runtime operation.
    pub(crate) fn permits_not_ready(
        &self,
        binding: Option<&WholeCodeBinding>,
        broker: Option<&CodePlatformBinding>,
        now: i64,
    ) -> bool {
        self.operation == CodePlatformOwnerOperation::ReadRetainedRuntime
            && now < self.expires
            && binding.is_none_or(|binding| self.permits(binding, &self.broker, now))
            && broker.is_none_or(|broker| broker == &self.broker)
    }
    /// Observe durable completion only. This grants no runtime or reply access.
    pub(crate) fn permits_completed(
        &self,
        binding: &WholeCodeBinding,
        broker: &CodePlatformBinding,
        now: i64,
    ) -> bool {
        matches!(
            self.operation,
            CodePlatformOwnerOperation::ReadRetainedRuntime
                | CodePlatformOwnerOperation::ReadPendingPlatformCall
        ) && self.permits(binding, broker, now)
    }
    pub(crate) fn accepts_reply(&self, reply: Option<&[u8]>) -> bool {
        match (self.operation, reply, self.reply_sha256.as_ref()) {
            (
                CodePlatformOwnerOperation::PublishCommittedPlatformReply,
                Some(bytes),
                Some(hash),
            ) => !bytes.is_empty() && bytes.len() <= 2_166_800 && sha256(bytes) == *hash,
            (
                CodePlatformOwnerOperation::ReadRetainedRuntime
                | CodePlatformOwnerOperation::ReadPendingPlatformCall,
                None,
                None,
            ) => true,
            _ => false,
        }
    }
}
impl<R: Ed25519PublicKeyResolver> GrantVerifier<R> {
    #[allow(
        clippy::too_many_lines,
        reason = "Keep ordered authority checks and durable phases in one owner operation."
    )]
    pub(crate) fn verify_code_platform_owner(
        &self,
        envelope: &SignedCodeEnvelope,
        peer: &str,
        key: &str,
        operation: CodePlatformOwnerOperation,
        now: i64,
    ) -> Result<AuthorizedCodePlatformOwner, GrantRejected> {
        if self.code_owner_requester.as_deref() != Some(peer) {
            return Err(GrantRejected);
        }
        let claims: CodePlatformOwnerGrantClaims = self.signed_code_claims(
            envelope,
            "elitea.sandbox.code-platform-owner-signed-grant.v1",
            DOMAIN,
        )?;
        if claims.schema != "elitea.sandbox.code-platform-owner-grant.v1"
            || claims.purpose != "platform_broker_runtime"
            || claims.job_key != key
            || !hex_id(key, 64, true)
            || !hex_id(&claims.execution_id, 32, false)
            || !(1..=i64::MAX as u64).contains(&claims.original_generation)
            || !current_claim(
                &claims.claim_id,
                claims.claim_attempt,
                claims.lease_epoch,
                &claims.fence_sha256,
            )
            || !hex_id(&claims.activation_id, 64, true)
            || !(1..=16).contains(&claims.attempt)
            || claims.supervisor_audience != self.audience
            || claims.requester_workload_identity != peer
            || !identity(peer)
            || claims.operation != operation
            || !lifetime(
                claims.issued_at_unix_millis,
                claims.expires_at_unix_millis,
                now,
            )
            || crate::sandbox::code_recovery::original_job_key(
                &claims.execution_id,
                &claims.dispatch_activation,
            ) != key
            || !hex_id(&claims.dispatch_activation, 64, true)
            || !hex_id(&claims.binding_sha256, 64, true)
            || !hex_id(&claims.prepared_job_sha256, 64, true)
            || !hex_id(&claims.prepared_fingerprint, 64, true)
            || match &claims.compiled_execute {
                None => claims.request_digest != claims.prepared_fingerprint,
                Some(selected) => {
                    !selected.matches_request(&claims.request_digest, &claims.prepared_fingerprint)
                }
            }
            || !hex_id(&claims.policy_sha256, 64, false)
            || !(1..=4096).contains(&claims.max_calls)
            || !(1..=64 * 1024 * 1024).contains(&claims.max_total_bytes)
        {
            return Err(GrantRejected);
        }
        match operation {
            CodePlatformOwnerOperation::PublishCommittedPlatformReply => {
                if claims
                    .sequence
                    .is_none_or(|v| v == 0 || v > claims.max_calls)
                    || claims
                        .platform_request_sha256
                        .as_ref()
                        .is_none_or(|v| !hex_id(v, 64, true))
                    || claims
                        .committed_reply_sha256
                        .as_ref()
                        .is_none_or(|v| !hex_id(v, 64, true))
                {
                    return Err(GrantRejected);
                }
            }
            _ => {
                if claims.sequence.is_some()
                    || claims.platform_request_sha256.is_some()
                    || claims.committed_reply_sha256.is_some()
                {
                    return Err(GrantRejected);
                }
            }
        }
        Ok(AuthorizedCodePlatformOwner {
            scope: JobScope::new(
                claims.tenant_id,
                claims.project_id,
                decode32(key)?,
                decode32(&claims.request_digest)?,
            )
            .map_err(|_| GrantRejected)?,
            execution: claims.execution_id,
            generation: claims.original_generation,
            activation: claims.activation_id,
            attempt: claims.attempt,
            dispatch: claims.dispatch_activation,
            audience: claims.supervisor_audience,
            binding_sha256: claims.binding_sha256,
            broker: CodePlatformBinding {
                schema: "elitea.sandbox.code-platform-binding.v1".into(),
                prepared_job_sha256: claims.prepared_job_sha256,
                prepared_fingerprint: claims.prepared_fingerprint,
                policy_sha256: claims.policy_sha256,
                max_calls: claims.max_calls,
                max_total_bytes: claims.max_total_bytes,
                compiled_execute: claims.compiled_execute,
            },
            operation,
            reply_sha256: claims.committed_reply_sha256,
            expires: claims.expires_at_unix_millis,
        })
    }
}
fn required_nullable<'de, D, T>(d: D) -> Result<Option<T>, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Deserialize<'de>,
{
    Option::<T>::deserialize(d)
}

#[cfg(test)]
#[path = "sandbox_code_platform_grant_tests.rs"]
mod tests;
