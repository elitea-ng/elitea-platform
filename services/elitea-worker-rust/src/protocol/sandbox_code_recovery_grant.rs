//! Separate signatures for Execute intent and owner observation. Neither permits Submit.
use super::{GrantRejected, GrantVerifier};
#[cfg(test)]
use crate::sandbox::code_recovery::hex;
use crate::{
    protocol::command::Ed25519PublicKeyResolver,
    sandbox::{
        code_recovery::{CodeRecoveryVisit, WholeCodeBinding, hex_id, identity, sha256},
        ledger::JobScope,
        request::PreparedJob,
    },
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use ring::signature;
use serde::{Deserialize, Serialize};

const INTENT_DOMAIN: &[u8] = b"elitea.sandbox.original-code-intent.ed25519.v1\0";
const READ_DOMAIN: &[u8] = b"elitea.sandbox.node-code-recovery-grant.ed25519.v1\0";
pub(crate) use crate::sandbox::code_recovery::SignedCodeEnvelope;
#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
pub(crate) enum CodeOwnerOperation {
    Read,
    SealNoEffect,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct OriginalCodeIntentClaims {
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
    pub(crate) node_id: String,
    pub(crate) graph_thread: String,
    pub(crate) step: u64,
    pub(crate) attempt: u16,
    pub(crate) node_digest: String,
    pub(crate) dispatch_activation: String,
    pub(crate) job_key: String,
    pub(crate) request_digest: String,
    pub(crate) supervisor_audience: String,
    pub(crate) submitter_workload_identity: String,
    pub(crate) language: String,
    pub(crate) prepared_job_sha256: String,
    pub(crate) source_sha256: String,
    pub(crate) input_sha256: String,
    pub(crate) issued_at_unix_millis: i64,
    pub(crate) expires_at_unix_millis: i64,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct CodeRecoveryGrantClaims {
    pub(crate) schema: String,
    pub(crate) tenant_id: String,
    pub(crate) project_id: i32,
    pub(crate) execution_id: String,
    pub(crate) original_generation: u64,
    pub(crate) claim_id: String,
    pub(crate) claim_attempt: u64,
    pub(crate) lease_epoch: u64,
    pub(crate) fence_sha256: String,
    pub(crate) activation_id: String,
    pub(crate) node_id: String,
    pub(crate) graph_thread: String,
    pub(crate) step: u64,
    pub(crate) attempt: u16,
    pub(crate) expected_revision: u64,
    pub(crate) receipt_sha256: String,
    pub(crate) dispatch_activation: String,
    pub(crate) job_key: String,
    pub(crate) request_digest: String,
    pub(crate) binding_sha256: String,
    pub(crate) supervisor_audience: String,
    pub(crate) requester_workload_identity: String,
    pub(crate) operation: CodeOwnerOperation,
    pub(crate) issued_at_unix_millis: i64,
    pub(crate) expires_at_unix_millis: i64,
}
/// Constructed only by the signature verifier after the existing Execute grant.
pub(crate) struct AuthorizedCodeIntent {
    scope: JobScope,
    binding: WholeCodeBinding,
    expires: i64,
}
impl AuthorizedCodeIntent {
    pub(crate) fn binding(
        &self,
        scope: &JobScope,
        now: i64,
    ) -> Result<&WholeCodeBinding, GrantRejected> {
        if &self.scope != scope || now >= self.expires {
            return Err(GrantRejected);
        }
        Ok(&self.binding)
    }
}
/// Current claim observation authority. This type cannot enter ordinary execution methods.
pub(crate) struct AuthorizedCodeRecovery {
    scope: JobScope,
    execution: String,
    generation: u64,
    dispatch: String,
    audience: String,
    binding_sha256: String,
    visit: CodeRecoveryVisit,
    operation: CodeOwnerOperation,
    expires: i64,
}
impl AuthorizedCodeRecovery {
    pub(crate) fn scope(&self) -> &JobScope {
        &self.scope
    }
    pub(crate) fn visit(&self) -> &CodeRecoveryVisit {
        &self.visit
    }
    pub(crate) fn operation(&self) -> CodeOwnerOperation {
        self.operation
    }
    pub(crate) fn permits(&self, binding: &WholeCodeBinding, now: i64) -> bool {
        now < self.expires
            && binding.valid()
            && binding.matches_original_visit(&self.visit)
            && binding.execution_id == self.execution
            && binding.original_generation == self.generation
            && binding.dispatch_activation == self.dispatch
            && binding.supervisor_audience == self.audience
            && binding
                .canonical_bytes()
                .is_ok_and(|bytes| sha256(&bytes) == self.binding_sha256)
    }
}
impl<R: Ed25519PublicKeyResolver> GrantVerifier<R> {
    fn signed_code_claims<T: serde::de::DeserializeOwned>(
        &self,
        envelope: &SignedCodeEnvelope,
        schema: &str,
        domain: &[u8],
    ) -> Result<T, GrantRejected> {
        if envelope.schema != schema
            || !identity(&envelope.key_id)
            || envelope.claims_base64url.len() > 11_000
        {
            return Err(GrantRejected);
        }
        let claims = URL_SAFE_NO_PAD
            .decode(&envelope.claims_base64url)
            .map_err(|_| GrantRejected)?;
        let sig = URL_SAFE_NO_PAD
            .decode(&envelope.signature_base64url)
            .map_err(|_| GrantRejected)?;
        if claims.is_empty()
            || claims.len() > 8192
            || sig.len() != 64
            || URL_SAFE_NO_PAD.encode(&claims) != envelope.claims_base64url
            || URL_SAFE_NO_PAD.encode(&sig) != envelope.signature_base64url
        {
            return Err(GrantRejected);
        }
        let key = self
            .resolver
            .resolve_ed25519_public_key(&envelope.key_id)
            .ok_or(GrantRejected)?;
        let mut signed = domain.to_vec();
        signed.extend_from_slice(&(claims.len() as u64).to_be_bytes());
        signed.extend_from_slice(&claims);
        signature::UnparsedPublicKey::new(&signature::ED25519, key)
            .verify(&signed, &sig)
            .map_err(|_| GrantRejected)?;
        serde_json::from_slice(&claims).map_err(|_| GrantRejected)
    }
    #[allow(
        clippy::too_many_arguments,
        reason = "Keep the existing authenticated identity fields explicit."
    )]
    pub(crate) fn verify_code_intent(
        &self,
        wire: &[u8],
        peer: &str,
        scope: &JobScope,
        execution: &str,
        generation: u64,
        dispatch: &str,
        job: &PreparedJob,
        now: i64,
    ) -> Result<AuthorizedCodeIntent, GrantRejected> {
        if wire.is_empty() || wire.len() > 16 * 1024 {
            return Err(GrantRejected);
        }
        let envelope: SignedCodeEnvelope =
            serde_json::from_slice(wire).map_err(|_| GrantRejected)?;
        let claims: OriginalCodeIntentClaims = self.signed_code_claims(
            &envelope,
            "elitea.sandbox.original-code-intent-signed.v1",
            INTENT_DOMAIN,
        )?;
        let binding = WholeCodeBinding {
            schema: "elitea.sandbox.whole-code-binding.v1".into(),
            purpose: claims.purpose,
            execution_id: claims.execution_id,
            original_generation: claims.original_generation,
            dispatch_activation: claims.dispatch_activation,
            job_key: claims.job_key,
            request_digest: claims.request_digest,
            supervisor_audience: claims.supervisor_audience,
            node_digest: claims.node_digest,
            activation_id: claims.activation_id.clone(),
            node_id: claims.node_id.clone(),
            graph_thread: claims.graph_thread.clone(),
            step: claims.step,
            attempt: claims.attempt,
            language: claims.language,
            prepared_job_sha256: claims.prepared_job_sha256,
            source_sha256: claims.source_sha256,
            input_sha256: claims.input_sha256,
        };
        if claims.schema != "elitea.sandbox.original-code-intent.v1"
            || !binding.valid()
            || binding.execution_id != execution
            || binding.original_generation != generation
            || binding.dispatch_activation != dispatch
            || binding.supervisor_audience != self.audience
            || claims.submitter_workload_identity != peer
            || JobScope::new(
                claims.tenant_id.clone(),
                claims.project_id,
                decode32(&binding.job_key)?,
                decode32(&binding.request_digest)?,
            )
            .map_err(|_| GrantRejected)?
                != *scope
            || !job.matches_code_binding(&binding)
            || !current_claim(
                &claims.claim_id,
                claims.claim_attempt,
                claims.lease_epoch,
                &claims.fence_sha256,
            )
            || !lifetime(
                claims.issued_at_unix_millis,
                claims.expires_at_unix_millis,
                now,
            )
        {
            return Err(GrantRejected);
        }
        Ok(AuthorizedCodeIntent {
            scope: scope.clone(),
            binding,
            expires: claims.expires_at_unix_millis,
        })
    }
    pub(crate) fn verify_code_recovery(
        &self,
        envelope: &SignedCodeEnvelope,
        peer: &str,
        job_key: &str,
        operation: CodeOwnerOperation,
        now: i64,
    ) -> Result<AuthorizedCodeRecovery, GrantRejected> {
        if self.code_owner_requester.as_deref() != Some(peer) {
            return Err(GrantRejected);
        }
        let claims: CodeRecoveryGrantClaims = self.signed_code_claims(
            envelope,
            "elitea.sandbox.node-code-recovery-signed-grant.v1",
            READ_DOMAIN,
        )?;
        let visit = CodeRecoveryVisit {
            activation_id: claims.activation_id,
            node_id: claims.node_id,
            graph_thread: claims.graph_thread,
            step: claims.step,
            attempt: claims.attempt,
            expected_revision: claims.expected_revision,
            receipt_sha256: claims.receipt_sha256,
        };
        if claims.schema != "elitea.sandbox.node-code-recovery-grant.v1"
            || !visit.valid()
            || !hex_id(&claims.execution_id, 32, false)
            || !(1..=i64::MAX as u64).contains(&claims.original_generation)
            || claims.supervisor_audience != self.audience
            || claims.requester_workload_identity != peer
            || claims.operation != operation
            || claims.job_key != job_key
            || claims.job_key
                != crate::sandbox::code_recovery::original_job_key(
                    &claims.execution_id,
                    &claims.dispatch_activation,
                )
            || !hex_id(&claims.dispatch_activation, 64, true)
            || !hex_id(&claims.binding_sha256, 64, true)
            || !current_claim(
                &claims.claim_id,
                claims.claim_attempt,
                claims.lease_epoch,
                &claims.fence_sha256,
            )
            || !lifetime(
                claims.issued_at_unix_millis,
                claims.expires_at_unix_millis,
                now,
            )
        {
            return Err(GrantRejected);
        }
        let scope = JobScope::new(
            claims.tenant_id,
            claims.project_id,
            decode32(&claims.job_key)?,
            decode32(&claims.request_digest)?,
        )
        .map_err(|_| GrantRejected)?;
        Ok(AuthorizedCodeRecovery {
            scope,
            execution: claims.execution_id,
            generation: claims.original_generation,
            dispatch: claims.dispatch_activation,
            audience: claims.supervisor_audience,
            binding_sha256: claims.binding_sha256,
            visit,
            operation,
            expires: claims.expires_at_unix_millis,
        })
    }
}
fn current_claim(id: &str, attempt: u64, epoch: u64, fence: &str) -> bool {
    hex_id(id, 32, false)
        && (1..=i64::MAX as u64).contains(&attempt)
        && (1..=i64::MAX as u64).contains(&epoch)
        && hex_id(fence, 64, true)
}
fn lifetime(issued: i64, expires: i64, now: i64) -> bool {
    issued > 0
        && issued <= now
        && now < expires
        && expires
            .checked_sub(issued)
            .is_some_and(|ttl| (1..=30_000).contains(&ttl))
}
fn decode32(value: &str) -> Result<[u8; 32], GrantRejected> {
    if !hex_id(value, 64, true) {
        return Err(GrantRejected);
    }
    let mut result = [0; 32];
    for (index, pair) in value.as_bytes().chunks_exact(2).enumerate() {
        let digit = |v: u8| {
            if v.is_ascii_digit() {
                v - b'0'
            } else {
                v - b'a' + 10
            }
        };
        result[index] = digit(pair[0]) * 16 + digit(pair[1]);
    }
    Ok(result)
}

#[cfg(test)]
#[path = "sandbox_code_recovery_grant_tests.rs"]
pub(crate) mod tests;

#[path = "sandbox_code_platform_grant.rs"]
mod platform;
pub(crate) use platform::{AuthorizedCodePlatformOwner, CodePlatformOwnerOperation};
