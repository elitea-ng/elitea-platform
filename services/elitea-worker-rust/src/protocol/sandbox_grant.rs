//! Verify Main's job-scoped delegation before sandbox admission.
use prost::Message;
use ring::{digest, signature};

use super::{
    command::Ed25519PublicKeyResolver,
    elitea::runtime::v1::{SandboxJobGrantClaimsV1, SignedSandboxJobGrantV1},
    wire::{Schema, scan_message},
};
use crate::sandbox::{ledger::JobScope, preparation::PreparationJob, request::PreparedJob};

pub(super) const DOMAIN: &[u8] = b"elitea.sandbox.job-grant.ed25519.v1\0";

pub struct GrantVerifier<R> {
    pub(super) resolver: R,
    pub(super) audience: String,
    pub(super) code_owner_requester: Option<String>,
}

/// An authenticated scope, without the delegation signature or worker credentials.
pub struct AuthorizedJob {
    authority: VerifiedAuthority,
}

struct VerifiedAuthority {
    scope: JobScope,
    fingerprint: [u8; 32],
    cancel_only: bool,
    expires_at_unix_millis: i64,
    content_root: Option<[u8; 32]>,
    execution_id: String,
    generation: u64,
    dispatch_activation: String,
}
impl AuthorizedJob {
    pub(crate) fn original_execution(&self) -> (&str, u64, &str) {
        (
            &self.authority.execution_id,
            self.authority.generation,
            &self.authority.dispatch_activation,
        )
    }
    pub(crate) fn permits(&self, request: &PreparedJob, now_unix_millis: i64) -> bool {
        !self.authority.cancel_only
            && now_unix_millis < self.authority.expires_at_unix_millis
            && request
                .fingerprint()
                .is_ok_and(|digest| digest == self.authority.fingerprint)
    }
    #[must_use]
    pub fn scope(&self) -> &JobScope {
        &self.authority.scope
    }
}

#[path = "sandbox_code_recovery_grant.rs"]
mod code_recovery;
#[cfg(test)]
pub(crate) use code_recovery::tests::{Fixture as CodeOwnerFixture, code_owner_fixture};
pub(crate) use code_recovery::{
    AuthorizedCodeIntent, AuthorizedCodePlatformOwner, AuthorizedCodeRecovery, CodeOwnerOperation,
    CodePlatformOwnerOperation, SignedCodeEnvelope,
};

/// Preparation authority cannot be passed to execution admission.
pub struct AuthorizedPreparation {
    authority: VerifiedAuthority,
}
impl AuthorizedPreparation {
    /// Recheck the exact preparation request and expiry before preparation admission.
    #[must_use]
    pub fn permits(&self, request: &PreparationJob, now_unix_millis: i64) -> bool {
        !self.authority.cancel_only
            && now_unix_millis < self.authority.expires_at_unix_millis
            && request
                .fingerprint()
                .is_ok_and(|digest| digest == self.authority.fingerprint)
    }
    #[must_use]
    pub fn scope(&self) -> &JobScope {
        &self.authority.scope
    }
}

/// Stop authority cannot be passed to submission.
pub struct AuthorizedCancellation(AuthorizedJob);
impl AuthorizedCancellation {
    pub(crate) fn scope(&self) -> &JobScope {
        self.0.scope()
    }
    pub(crate) fn valid_at(&self, now: i64) -> bool {
        self.0.authority.cancel_only && now < self.0.authority.expires_at_unix_millis
    }
}

/// Content authority cannot admit preparation, code execution, or cancellation.
pub struct AuthorizedContent {
    scope: JobScope,
    root: [u8; 32],
    expires_at_unix_millis: i64,
}
impl AuthorizedContent {
    #[must_use]
    pub fn scope(&self) -> &JobScope {
        &self.scope
    }
    #[must_use]
    pub fn root(&self) -> &[u8; 32] {
        &self.root
    }
    #[must_use]
    pub fn valid_at(&self, now_unix_millis: i64) -> bool {
        now_unix_millis < self.expires_at_unix_millis
    }
}

#[derive(Debug, thiserror::Error)]
#[error("sandbox authorization is invalid, expired, or does not match this request")]
pub struct GrantRejected;

pub(super) fn identity(value: &str) -> bool {
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
        Ok(Self {
            resolver,
            audience,
            code_owner_requester: None,
        })
    }
    /// Exact mandatory mTLS Main identity for owner-only routes. Ordinary
    /// Execute verification is independent; unset keeps owner routes denied.
    /// # Errors
    /// Returns `GrantRejected` if the requester identity fails validation.
    pub fn with_code_owner_requester(mut self, requester: String) -> Result<Self, GrantRejected> {
        if !identity(&requester) {
            return Err(GrantRejected);
        }
        self.code_owner_requester = Some(requester);
        Ok(self)
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
        let fingerprint = job.fingerprint().map_err(|_| GrantRejected)?;
        self.verify_operation(
            grant,
            peer,
            Some(fingerprint),
            now_unix_millis,
            false,
            false,
        )
        .map(|authority| AuthorizedJob { authority })
    }

    /// Verify Main's revision 1 grant over a preparation-specific fingerprint.
    /// `peer` must come from verified mTLS, never caller-controlled metadata.
    ///
    /// # Errors
    /// Returns `GrantRejected` for any signature, wire, request, peer, or lifetime mismatch.
    pub fn verify_preparation(
        &self,
        grant: &SignedSandboxJobGrantV1,
        peer: &str,
        job: &PreparationJob,
        now_unix_millis: i64,
    ) -> Result<AuthorizedPreparation, GrantRejected> {
        let fingerprint = job.fingerprint().map_err(|_| GrantRejected)?;
        self.verify_operation(
            grant,
            peer,
            Some(fingerprint),
            now_unix_millis,
            false,
            false,
        )
        .map(|authority| AuthorizedPreparation { authority })
    }

    /// Verify a stop-only grant without receiving code or state.
    /// # Errors
    /// Returns `GrantRejected` for an invalid scope, signature, purpose, or lifetime.
    pub fn verify_cancellation(
        &self,
        grant: &SignedSandboxJobGrantV1,
        peer: &str,
        now_unix_millis: i64,
    ) -> Result<AuthorizedCancellation, GrantRejected> {
        self.verify_operation(grant, peer, None, now_unix_millis, true, false)
            .map(|authority| AuthorizedCancellation(AuthorizedJob { authority }))
    }

    /// Verify revision 3 authority for the signed preparation scope and content root.
    /// `peer` must come from verified mTLS, never caller-controlled metadata.
    /// Match this scope and root against the recorded preparation before transfer.
    ///
    /// # Errors
    /// Returns `GrantRejected` for invalid signature, wire, purpose, scope, peer, or lifetime.
    pub fn verify_content(
        &self,
        grant: &SignedSandboxJobGrantV1,
        peer: &str,
        now_unix_millis: i64,
    ) -> Result<AuthorizedContent, GrantRejected> {
        let authority = self.verify_operation(grant, peer, None, now_unix_millis, false, true)?;
        Ok(AuthorizedContent {
            scope: authority.scope,
            root: authority.content_root.ok_or(GrantRejected)?,
            expires_at_unix_millis: authority.expires_at_unix_millis,
        })
    }

    fn verify_operation(
        &self,
        grant: &SignedSandboxJobGrantV1,
        peer: &str,
        expected_fingerprint: Option<[u8; 32]>,
        now_unix_millis: i64,
        cancel_only: bool,
        content_only: bool,
    ) -> Result<VerifiedAuthority, GrantRejected> {
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
        let fingerprint: [u8; 32] = claims
            .request_digest
            .as_slice()
            .try_into()
            .map_err(|_| GrantRejected)?;
        let content_root = if content_only {
            Some(
                claims
                    .dependency_bundle_sha256
                    .as_slice()
                    .try_into()
                    .map_err(|_| GrantRejected)?,
            )
        } else {
            None
        };
        let revision = if content_only {
            3
        } else if cancel_only {
            2
        } else {
            1
        };
        if claims.cancel_only != cancel_only
            || (content_only && cancel_only)
            || (!content_only && !claims.dependency_bundle_sha256.is_empty())
            || claims.revision != revision
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
            || expected_fingerprint.is_some_and(|actual| actual != fingerprint)
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
        Ok(VerifiedAuthority {
            scope,
            fingerprint,
            cancel_only,
            expires_at_unix_millis: claims.expires_at_unix_millis,
            content_root,
            execution_id: claims.execution_id,
            generation: claims.generation,
            dispatch_activation: claims.activation_id,
        })
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
    fn preparation(source: &str) -> PreparationJob {
        PreparationJob::new(
            source.into(),
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
            cancel_only: false,
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
            dependency_bundle_sha256: Vec::new(),
        };
        (key, verifier, job, claims)
    }
    #[test]
    fn content_grant_cannot_execute_or_cancel() {
        let (key, verifier, job, mut claims) = fixture();
        claims.revision = 3;
        claims.dependency_bundle_sha256 = vec![9; 32];
        let grant = sign(&key, claims.encode_to_vec());
        assert!(verifier.verify(&grant, "worker-1", &job, 1000).is_err());
        assert!(
            verifier
                .verify_cancellation(&grant, "worker-1", 1000)
                .is_err()
        );
        // Neither changing purpose nor retaining a root on an old revision is valid.
        claims.revision = 1;
        assert!(
            verifier
                .verify(&sign(&key, claims.encode_to_vec()), "worker-1", &job, 1000)
                .is_err()
        );
        claims.revision = 2;
        claims.cancel_only = true;
        assert!(
            verifier
                .verify_cancellation(&sign(&key, claims.encode_to_vec()), "worker-1", 1000)
                .is_err()
        );
    }

    #[test]
    fn content_authority_binds_the_preparation_scope_and_recorded_root() {
        let (key, verifier, _, mut claims) = fixture();
        let request = preparation("print(42)");
        claims.request_digest = request.fingerprint().unwrap().to_vec();
        let prepared = verifier
            .verify_preparation(
                &sign(&key, claims.encode_to_vec()),
                "worker-1",
                &request,
                1000,
            )
            .unwrap();
        claims.revision = 3;
        claims.dependency_bundle_sha256 = vec![9; 32];
        let grant = sign(&key, claims.encode_to_vec());
        let content = verifier.verify_content(&grant, "worker-1", 1000).unwrap();
        assert_eq!(content.root(), &[9; 32]);
        assert!(content.valid_at(1000));
        assert!(!content.valid_at(31000));
        assert_eq!(
            content.scope().runtime_identity().unwrap(),
            prepared.scope().runtime_identity().unwrap()
        );
        assert!(
            verifier
                .verify_preparation(&grant, "worker-1", &request, 1000)
                .is_err()
        );
        assert!(
            verifier
                .verify(&grant, "worker-1", &job("print(42)"), 1000)
                .is_err()
        );
        assert!(
            verifier
                .verify_cancellation(&grant, "worker-1", 1000)
                .is_err()
        );
    }

    #[test]
    fn content_authority_rejects_wrong_purpose_and_digest_lengths() {
        let (key, verifier, _, mut claims) = fixture();
        let execution_grant = sign(&key, claims.encode_to_vec());
        assert!(
            verifier
                .verify_content(&execution_grant, "worker-1", 1000)
                .is_err()
        );
        claims.revision = 2;
        claims.cancel_only = true;
        assert!(
            verifier
                .verify_content(&sign(&key, claims.encode_to_vec()), "worker-1", 1000)
                .is_err()
        );
        for (revision, cancel_only, root_length) in [
            (1, false, 32),
            (2, true, 32),
            (3, true, 32),
            (3, false, 0),
            (3, false, 31),
            (3, false, 33),
        ] {
            claims.revision = revision;
            claims.cancel_only = cancel_only;
            claims.dependency_bundle_sha256 = vec![9; root_length];
            assert!(
                verifier
                    .verify_content(&sign(&key, claims.encode_to_vec()), "worker-1", 1000)
                    .is_err()
            );
        }
        claims.revision = 3;
        claims.cancel_only = false;
        claims.dependency_bundle_sha256 = vec![9; 32];
        for length in [0, 31, 33] {
            claims.request_digest = vec![7; length];
            assert!(
                verifier
                    .verify_content(&sign(&key, claims.encode_to_vec()), "worker-1", 1000)
                    .is_err()
            );
        }
    }

    #[test]
    fn content_authority_requires_verified_peer_audience_lifetime_and_strict_signed_bytes() {
        let (key, verifier, _, mut claims) = fixture();
        claims.revision = 3;
        claims.dependency_bundle_sha256 = vec![9; 32];
        let grant = sign(&key, claims.encode_to_vec());
        assert!(verifier.verify_content(&grant, "worker-2", 1000).is_err());
        for now in [999, 31000] {
            assert!(verifier.verify_content(&grant, "worker-1", now).is_err());
        }
        for expires in [1000, 31001, i64::MAX] {
            let mut changed = claims.clone();
            changed.expires_at_unix_millis = expires;
            assert!(
                verifier
                    .verify_content(&sign(&key, changed.encode_to_vec()), "worker-1", 1000)
                    .is_err()
            );
        }
        let mut changed = claims.clone();
        changed.audience = "different-supervisor".into();
        assert!(
            verifier
                .verify_content(&sign(&key, changed.encode_to_vec()), "worker-1", 1000)
                .is_err()
        );
        let mut changed = grant.clone();
        changed.signature[0] ^= 1;
        assert!(verifier.verify_content(&changed, "worker-1", 1000).is_err());
        let mut changed = grant.clone();
        changed.key_id = "other-key".into();
        assert!(verifier.verify_content(&changed, "worker-1", 1000).is_err());
        let mut changed = grant.clone();
        let mut substituted = claims.clone();
        substituted.dependency_bundle_sha256 = vec![8; 32];
        changed.claims_bytes = substituted.encode_to_vec();
        assert!(verifier.verify_content(&changed, "worker-1", 1000).is_err());
        for suffix in [vec![0x60, 1], vec![8, 3]] {
            let mut bytes = claims.encode_to_vec();
            bytes.extend(suffix);
            assert!(
                verifier
                    .verify_content(&sign(&key, bytes), "worker-1", 1000)
                    .is_err()
            );
        }
    }

    #[test]
    fn content_scope_cannot_match_another_preparation_record() {
        let (key, verifier, _, mut claims) = fixture();
        claims.revision = 3;
        claims.dependency_bundle_sha256 = vec![9; 32];
        let content = verifier
            .verify_content(&sign(&key, claims.encode_to_vec()), "worker-1", 1000)
            .unwrap();
        let original_identity = content.scope().runtime_identity().unwrap();
        for field in 0..5 {
            let mut changed = claims.clone();
            match field {
                0 => changed.tenant_id = "different-tenant".into(),
                1 => changed.project_id += 1,
                2 => changed.execution_id = "different-execution".into(),
                3 => changed.activation_id = "different-activation".into(),
                _ => changed.request_digest = vec![8; 32],
            }
            let different = verifier
                .verify_content(&sign(&key, changed.encode_to_vec()), "worker-1", 1000)
                .unwrap();
            assert_ne!(
                different.scope().runtime_identity().unwrap(),
                original_identity
            );
        }
        claims.generation = 2;
        claims.submitter_workload_identity = "worker-2".into();
        let resumed = verifier
            .verify_content(&sign(&key, claims.encode_to_vec()), "worker-2", 1000)
            .unwrap();
        assert_eq!(
            resumed.scope().runtime_identity().unwrap(),
            original_identity
        );
        assert_eq!(resumed.root(), content.root());
        for field in 0..5 {
            let mut invalid = claims.clone();
            match field {
                0 => invalid.tenant_id = "tenant\0".into(),
                1 => invalid.project_id = 0,
                2 => invalid.execution_id.clear(),
                3 => invalid.activation_id.clear(),
                _ => invalid.generation = 0,
            }
            assert!(
                verifier
                    .verify_content(&sign(&key, invalid.encode_to_vec()), "worker-2", 1000)
                    .is_err()
            );
        }
    }

    #[test]
    fn preparation_and_execution_authority_cannot_be_substituted() {
        let (key, verifier, execution, mut claims) = fixture();
        let request = preparation("print(42)");
        assert_ne!(
            execution.fingerprint().unwrap(),
            request.fingerprint().unwrap()
        );
        let execution_grant = sign(&key, claims.encode_to_vec());
        assert!(
            verifier
                .verify(&execution_grant, "worker-1", &execution, 1000)
                .is_ok()
        );
        assert!(
            verifier
                .verify_preparation(&execution_grant, "worker-1", &request, 1000)
                .is_err()
        );

        // Keep the same activation and material to isolate fingerprint purpose binding.
        claims.request_digest = request.fingerprint().unwrap().to_vec();
        let preparation_grant = sign(&key, claims.encode_to_vec());
        let accepted = verifier
            .verify_preparation(&preparation_grant, "worker-1", &request, 1000)
            .unwrap();
        assert!(accepted.permits(&request, 1000));
        assert!(!accepted.permits(&preparation("print(43)"), 1000));
        assert!(!accepted.permits(&request, 31000));
        assert!(
            verifier
                .verify(&preparation_grant, "worker-1", &execution, 1000)
                .is_err()
        );
        assert!(
            verifier
                .verify_cancellation(&preparation_grant, "worker-1", 1000)
                .is_err()
        );
    }

    #[test]
    fn preparation_grant_binds_every_immutable_field_peer_audience_and_lifetime() {
        let (key, verifier, _, mut claims) = fixture();
        let request = preparation("print(42)");
        claims.request_digest = request.fingerprint().unwrap().to_vec();
        let grant = sign(&key, claims.encode_to_vec());
        assert!(
            verifier
                .verify_preparation(&grant, "worker-2", &request, 1000)
                .is_err()
        );
        for now in [999, 31000] {
            assert!(
                verifier
                    .verify_preparation(&grant, "worker-1", &request, now)
                    .is_err()
            );
        }
        for (source, image, policy, timeout) in [
            ("print(43)", "a", "v1", 30),
            ("print(42)", "b", "v1", 30),
            ("print(42)", "a", "v2", 30),
            ("print(42)", "a", "v1", 31),
        ] {
            let changed = PreparationJob::new(
                source.into(),
                format!("sha256:{}", image.repeat(64)),
                policy.into(),
                timeout,
            )
            .unwrap();
            assert!(
                verifier
                    .verify_preparation(&grant, "worker-1", &changed, 1000)
                    .is_err()
            );
        }
        let mut changed = claims.clone();
        changed.audience = "other".into();
        assert!(
            verifier
                .verify_preparation(
                    &sign(&key, changed.encode_to_vec()),
                    "worker-1",
                    &request,
                    1000
                )
                .is_err()
        );
        let mut changed = claims.clone();
        changed.expires_at_unix_millis += 1;
        assert!(
            verifier
                .verify_preparation(
                    &sign(&key, changed.encode_to_vec()),
                    "worker-1",
                    &request,
                    1000
                )
                .is_err()
        );
        claims.expires_at_unix_millis = claims.issued_at_unix_millis;
        assert!(
            verifier
                .verify_preparation(
                    &sign(&key, claims.encode_to_vec()),
                    "worker-1",
                    &request,
                    1000
                )
                .is_err()
        );
    }

    #[test]
    fn preparation_cancellation_keeps_the_same_scope_after_worker_replacement() {
        use crate::sandbox::preparation::preparation_activation;
        use std::fmt::Write as _;

        let (key, verifier, execution, mut claims) = fixture();
        let execution_scope = verifier
            .verify(
                &sign(&key, claims.encode_to_vec()),
                "worker-1",
                &execution,
                1000,
            )
            .unwrap();
        let request = preparation("print(42)");
        claims.activation_id =
            preparation_activation(&[7; 32])
                .iter()
                .fold(String::new(), |mut hex, byte| {
                    write!(hex, "{byte:02x}").unwrap();
                    hex
                });
        claims.request_digest = request.fingerprint().unwrap().to_vec();
        let preparation_grant = sign(&key, claims.encode_to_vec());
        let accepted = verifier
            .verify_preparation(&preparation_grant, "worker-1", &request, 1000)
            .unwrap();
        assert_ne!(
            accepted.scope().runtime_identity().unwrap(),
            execution_scope.scope().runtime_identity().unwrap()
        );
        claims.generation = 2;
        claims.submitter_workload_identity = "worker-2".into();
        let resumed = verifier
            .verify_preparation(
                &sign(&key, claims.encode_to_vec()),
                "worker-2",
                &request,
                1000,
            )
            .unwrap();
        assert_eq!(
            accepted.scope().runtime_identity().unwrap(),
            resumed.scope().runtime_identity().unwrap()
        );
        claims.revision = 2;
        claims.cancel_only = true;
        let cancellation_grant = sign(&key, claims.encode_to_vec());
        assert!(
            verifier
                .verify_preparation(&cancellation_grant, "worker-2", &request, 1000)
                .is_err()
        );
        let stop = verifier
            .verify_cancellation(&cancellation_grant, "worker-2", 1000)
            .unwrap();
        assert!(stop.valid_at(1000));
        assert_eq!(
            accepted.scope().runtime_identity().unwrap(),
            stop.scope().runtime_identity().unwrap()
        );
        assert!(
            verifier
                .verify_cancellation(&cancellation_grant, "worker-1", 1000)
                .is_err()
        );
    }

    #[test]
    fn preparation_rejects_content_grants_and_changed_signed_scope() {
        let (key, verifier, _, mut claims) = fixture();
        let request = preparation("print(42)");
        claims.request_digest = request.fingerprint().unwrap().to_vec();
        for revision in [1, 2, 3] {
            let mut content = claims.clone();
            content.revision = revision;
            content.dependency_bundle_sha256 = vec![9; 32];
            assert!(
                verifier
                    .verify_preparation(
                        &sign(&key, content.encode_to_vec()),
                        "worker-1",
                        &request,
                        1000
                    )
                    .is_err()
            );
        }
        let mut changed = sign(&key, claims.encode_to_vec());
        claims.tenant_id = "other-tenant".into();
        changed.claims_bytes = claims.encode_to_vec();
        assert!(
            verifier
                .verify_preparation(&changed, "worker-1", &request, 1000)
                .is_err()
        );
        for suffix in [vec![0x60, 1], vec![8, 1]] {
            let mut bytes = claims.encode_to_vec();
            bytes.extend(suffix);
            assert!(
                verifier
                    .verify_preparation(&sign(&key, bytes), "worker-1", &request, 1000)
                    .is_err()
            );
        }
        claims.revision = 3;
        assert!(
            verifier
                .verify_preparation(
                    &sign(&key, claims.encode_to_vec()),
                    "worker-1",
                    &request,
                    1000
                )
                .is_err()
        );
    }

    #[test]
    fn cancellation_grant_cannot_submit_and_submission_grant_cannot_cancel() {
        let (key, verifier, job, mut claims) = fixture();
        let submit = sign(&key, claims.encode_to_vec());
        assert!(
            verifier
                .verify_cancellation(&submit, "worker-1", 1000)
                .is_err()
        );
        claims.revision = 2;
        claims.cancel_only = true;
        let stop = sign(&key, claims.encode_to_vec());
        assert!(verifier.verify(&stop, "worker-1", &job, 1000).is_err());
        assert!(
            verifier
                .verify_cancellation(&stop, "worker-1", 1000)
                .unwrap()
                .valid_at(1000)
        );
        assert!(
            verifier
                .verify_cancellation(&stop, "worker-2", 1000)
                .is_err()
        );
        assert!(
            verifier
                .verify_cancellation(&stop, "worker-1", 31000)
                .is_err()
        );
        claims.revision = 1;
        assert!(
            verifier
                .verify_cancellation(&sign(&key, claims.encode_to_vec()), "worker-1", 1000)
                .is_err()
        );
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
    fn signed_grant_binds_resolved_python_bundle_and_refuses_downgrade() {
        let (key, verifier, baseline, mut claims) = fixture();
        let prepared = job("print(42)")
            .with_python_dependency_bundle("b".repeat(64))
            .unwrap();
        let baseline_grant = sign(&key, claims.encode_to_vec());
        assert!(
            verifier
                .verify(&baseline_grant, "worker-1", &prepared, 1000)
                .is_err()
        );
        claims.request_digest = prepared.fingerprint().unwrap().to_vec();
        let grant = sign(&key, claims.encode_to_vec());
        assert!(verifier.verify(&grant, "worker-1", &prepared, 1000).is_ok());
        let changed = job("print(42)")
            .with_python_dependency_bundle("c".repeat(64))
            .unwrap();
        assert!(verifier.verify(&grant, "worker-1", &changed, 1000).is_err());
        assert!(
            verifier
                .verify(&grant, "worker-1", &baseline, 1000)
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

#[path = "sandbox_compiled_grant.rs"]
mod compiled;
pub(crate) use compiled::{
    AuthorizedSnapshotCompile, AuthorizedSnapshotExecute, AuthorizedSnapshotPublish,
    AuthorizedSnapshotRead,
};
