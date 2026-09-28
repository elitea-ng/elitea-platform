//! Verify Main's job-scoped delegation before sandbox admission.
use prost::Message;
use ring::{digest, signature};

use super::{
    command::Ed25519PublicKeyResolver,
    elitea::runtime::v1::{SandboxJobGrantClaimsV1, SignedSandboxJobGrantV1},
    wire::{Schema, scan_message},
};
use crate::sandbox::{ledger::JobScope, request::PreparedJob};

const DOMAIN: &[u8] = b"elitea.sandbox.job-grant.ed25519.v1\0";

pub struct GrantVerifier<R> {
    resolver: R,
    audience: String,
}

/// An authenticated scope, without the delegation signature or worker credentials.
pub struct AuthorizedJob {
    scope: JobScope,
}
impl AuthorizedJob {
    #[must_use]
    pub fn scope(&self) -> &JobScope {
        &self.scope
    }
}

#[derive(Debug, thiserror::Error)]
#[error("sandbox authorization is invalid, expired, or does not match this request")]
pub struct GrantRejected;

fn identity(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 256
        && !value.bytes().any(|b| matches!(b, b'\r' | b'\n' | b'\0'))
}

impl<R: Ed25519PublicKeyResolver> GrantVerifier<R> {
    /// # Errors
    /// Returns `GrantRejected` for an invalid configured supervisor audience.
    pub fn new(resolver: R, audience: String) -> Result<Self, GrantRejected> {
        if !identity(&audience) {
            return Err(GrantRejected);
        }
        Ok(Self { resolver, audience })
    }

    /// `peer` must come from verified mTLS, never caller-controlled metadata.
    /// Verify immediately before admission; expiry does not reset a running job.
    ///
    /// # Errors
    /// Returns `GrantRejected` for any signature, wire, scope, peer or lifetime mismatch.
    pub fn verify(
        &self,
        grant: &SignedSandboxJobGrantV1,
        peer: &str,
        job: &PreparedJob,
        now_unix_millis: i64,
    ) -> Result<AuthorizedJob, GrantRejected> {
        if !identity(&grant.key_id)
            || !identity(peer)
            || grant.signature.len() != 64
            || grant.claims_bytes.is_empty()
            || grant.claims_bytes.len() > 4096
        {
            return Err(GrantRejected);
        }
        let key = self
            .resolver
            .resolve_ed25519_public_key(&grant.key_id)
            .ok_or(GrantRejected)?;
        let mut input = Vec::with_capacity(DOMAIN.len() + 8 + grant.claims_bytes.len());
        input.extend_from_slice(DOMAIN);
        input.extend_from_slice(&(grant.claims_bytes.len() as u64).to_be_bytes());
        input.extend_from_slice(&grant.claims_bytes);
        signature::UnparsedPublicKey::new(&signature::ED25519, key)
            .verify(&input, &grant.signature)
            .map_err(|_| GrantRejected)?;
        scan_message(&grant.claims_bytes, Schema::SandboxGrant).map_err(|_| GrantRejected)?;
        let claims = SandboxJobGrantClaimsV1::decode(grant.claims_bytes.as_slice())
            .map_err(|_| GrantRejected)?;
        let lifetime = claims
            .expires_at_unix_millis
            .checked_sub(claims.issued_at_unix_millis)
            .ok_or(GrantRejected)?;
        let fingerprint = job.fingerprint().map_err(|_| GrantRejected)?;
        if claims.revision != 1
            || claims.generation == 0
            || !identity(&claims.tenant_id)
            || claims.project_id <= 0
            || !identity(&claims.execution_id)
            || !identity(&claims.activation_id)
            || claims.submitter_workload_identity != peer
            || claims.audience != self.audience
            || !(1..=30_000).contains(&lifetime)
            || claims.issued_at_unix_millis > now_unix_millis
            || claims.expires_at_unix_millis <= now_unix_millis
            || claims.request_digest.as_slice() != fingerprint
        {
            return Err(GrantRejected);
        }
        // Activation identity survives a worker lease/generation replacement.
        // The runtime name additionally binds tenant and project in JobScope.
        let mut hash = digest::Context::new(&digest::SHA256);
        hash.update(b"elitea.sandbox.activation.v1\0");
        for part in [&claims.execution_id, &claims.activation_id] {
            hash.update(&(part.len() as u64).to_be_bytes());
            hash.update(part.as_bytes());
        }
        let mut activation = [0; 32];
        activation.copy_from_slice(hash.finish().as_ref());
        let scope = JobScope::new(claims.tenant_id, claims.project_id, activation, fingerprint)
            .map_err(|_| GrantRejected)?;
        Ok(AuthorizedJob { scope })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::sandbox::request::Language;
    use ring::signature::KeyPair;
    use std::collections::BTreeMap;

    struct Keys([u8; 32]);
    impl Ed25519PublicKeyResolver for Keys {
        fn resolve_ed25519_public_key(&self, id: &str) -> Option<[u8; 32]> {
            (id == "key-1").then_some(self.0)
        }
    }
    fn job(source: &str) -> PreparedJob {
        PreparedJob::new(
            Language::Python,
            source.into(),
            BTreeMap::new(),
            format!("sha256:{}", "a".repeat(64)),
            "v1".into(),
            30,
        )
        .unwrap()
    }
    fn sign(key: &signature::Ed25519KeyPair, bytes: Vec<u8>) -> SignedSandboxJobGrantV1 {
        let mut input = DOMAIN.to_vec();
        input.extend_from_slice(&(bytes.len() as u64).to_be_bytes());
        input.extend_from_slice(&bytes);
        SignedSandboxJobGrantV1 {
            key_id: "key-1".into(),
            claims_bytes: bytes,
            signature: key.sign(&input).as_ref().to_vec(),
        }
    }
    fn fixture() -> (
        signature::Ed25519KeyPair,
        GrantVerifier<Keys>,
        PreparedJob,
        SandboxJobGrantClaimsV1,
    ) {
        let key = signature::Ed25519KeyPair::from_seed_unchecked(&[7; 32]).unwrap();
        let mut public = [0; 32];
        public.copy_from_slice(key.public_key().as_ref());
        let verifier = GrantVerifier::new(Keys(public), "sandbox-prod".into()).unwrap();
        let job = job("print(42)");
        let claims = SandboxJobGrantClaimsV1 {
            revision: 1,
            tenant_id: "tenant".into(),
            project_id: 2,
            execution_id: "execution-1".into(),
            activation_id: "node/activation-1".into(),
            request_digest: job.fingerprint().unwrap().to_vec(),
            submitter_workload_identity: "worker-1".into(),
            audience: "sandbox-prod".into(),
            issued_at_unix_millis: 1000,
            expires_at_unix_millis: 31000,
            generation: 1,
        };
        (key, verifier, job, claims)
    }
    #[test]
    fn grant_requires_exact_peer_request_audience_and_lifetime() {
        let (key, verifier, request, mut claims) = fixture();
        let grant = sign(&key, claims.encode_to_vec());
        let accepted = verifier.verify(&grant, "worker-1", &request, 1000).unwrap();
        assert!(verifier.verify(&grant, "worker-2", &request, 1000).is_err());
        assert!(
            verifier
                .verify(&grant, "worker-1", &job("print(43)"), 1000)
                .is_err()
        );
        assert!(verifier.verify(&grant, "worker-1", &request, 999).is_err());
        assert!(
            verifier
                .verify(&grant, "worker-1", &request, 31000)
                .is_err()
        );
        claims.generation = 2;
        let resumed = verifier
            .verify(
                &sign(&key, claims.encode_to_vec()),
                "worker-1",
                &request,
                1000,
            )
            .unwrap();
        assert_eq!(
            accepted.scope().runtime_identity().unwrap(),
            resumed.scope().runtime_identity().unwrap()
        );
        claims.audience = "other".into();
        assert!(
            verifier
                .verify(
                    &sign(&key, claims.encode_to_vec()),
                    "worker-1",
                    &request,
                    1000
                )
                .is_err()
        );
    }
    #[test]
    fn signature_and_strict_wire_validation_precede_admission() {
        let (key, verifier, request, claims) = fixture();
        let mut grant = sign(&key, claims.encode_to_vec());
        grant.signature[0] ^= 1;
        assert!(verifier.verify(&grant, "worker-1", &request, 1000).is_err());
        for suffix in [vec![0x60, 1], vec![8, 1]] {
            let mut bytes = claims.encode_to_vec();
            bytes.extend(suffix);
            assert!(
                verifier
                    .verify(&sign(&key, bytes), "worker-1", &request, 1000)
                    .is_err()
            );
        }
    }
}
