//! Revision 4 purpose authority. Auxiliary JSON never substitutes for a signed role.
use super::{DOMAIN, GrantRejected, GrantVerifier, identity};
use crate::{
    protocol::{
        command::Ed25519PublicKeyResolver,
        elitea::runtime::v1::{
            RustCompiledSnapshotGrantClaimsV1, RustCompiledSnapshotPurposeV1,
            SignedSandboxJobGrantV1,
        },
        wire::{Schema, scan_message},
    },
    sandbox::{
        compiled_snapshot::{ContentSha256, Control, Purpose},
        ledger::JobScope,
        request::PreparedJob,
    },
};
use prost::Message as _;
use ring::{digest, signature};
struct Authority {
    scope: JobScope,
    tenant: String,
    project: i32,
    expires: i64,
    request: [u8; 32],
    base: [u8; 32],
    key: [u8; 32],
    root: Option<[u8; 32]>,
    execution: String,
    generation: u64,
    dispatch: String,
    workspace_original_visit: Option<crate::sandbox::code_recovery::OriginalCodeVisitRef>,
}
pub(crate) struct AuthorizedSnapshotCompile(Authority);
pub(crate) struct AuthorizedSnapshotExecute(Authority);
pub(crate) struct AuthorizedSnapshotRead(Authority);
pub(crate) struct AuthorizedSnapshotPublish {
    authority: Authority,
    pub(crate) compilation_job_key: [u8; 32],
    pub(crate) compilation_runtime_id: String,
    pub(crate) compilation_lease_epoch: u64,
}
impl Authority {
    fn permits(&self, job: &PreparedJob, control: &Control, purpose: Purpose, now: i64) -> bool {
        control.binding.tenant_id == self.tenant
            && platform_snapshot_profile_matches(job)
            && control.binding.project_id == self.project
            && now < self.expires
            && control.binding.validate_job(job).is_ok()
            && job.fingerprint().is_ok_and(|base| base == self.base)
            && control
                .snapshot_key_sha256
                .raw()
                .is_ok_and(|key| key == self.key)
            && control
                .intent_digest(purpose)
                .is_ok_and(|intent| intent == self.request)
            && control
                .descriptor_sha256
                .as_ref()
                .map(ContentSha256::raw)
                .transpose()
                .is_ok_and(|root| root == self.root)
    }
}
impl AuthorizedSnapshotCompile {
    /// Original signed compilation identity, never an Execute authority.
    pub(crate) fn original_execution(&self) -> (&str, u64, &str) {
        (&self.0.execution, self.0.generation, &self.0.dispatch)
    }
    /// Present only after strict signed Compile access and prepared-workspace checks.
    pub(crate) fn workspace_original_visit(
        &self,
    ) -> Option<&crate::sandbox::code_recovery::OriginalCodeVisitRef> {
        self.0.workspace_original_visit.as_ref()
    }
    pub(crate) fn scope(&self) -> &JobScope {
        &self.0.scope
    }
    pub(crate) fn permits(&self, job: &PreparedJob, control: &Control, now: i64) -> bool {
        self.0.permits(job, control, Purpose::Compile, now)
            && job.workspace().is_some() == self.0.workspace_original_visit.is_some()
            && compile_platform_permits(job)
    }
}
fn compile_platform_permits(job: &PreparedJob) -> bool {
    job.platform_client().is_none()
        || (job.workspace().is_some() && platform_snapshot_profile_matches(job))
}
fn platform_snapshot_profile_matches(job: &PreparedJob) -> bool {
    job.platform_client().is_none()
        || job.to_transport().is_ok_and(|bytes| {
            serde_json::from_slice::<serde_json::Value>(&bytes)
                .is_ok_and(|value| value["policy_revision"] == "cargo-broker-execute-v1")
        })
}
impl AuthorizedSnapshotExecute {
    pub(crate) fn original_execution(&self) -> (&str, u64, &str) {
        (&self.0.execution, self.0.generation, &self.0.dispatch)
    }
    pub(crate) fn scope(&self) -> &JobScope {
        &self.0.scope
    }
    pub(crate) fn permits(&self, job: &PreparedJob, control: &Control, now: i64) -> bool {
        self.0.permits(job, control, Purpose::Execute, now)
    }
}
impl AuthorizedSnapshotRead {
    pub(crate) fn scope(&self) -> &JobScope {
        &self.0.scope
    }
    pub(crate) fn root(&self) -> Option<&[u8; 32]> {
        self.0.root.as_ref()
    }
    pub(crate) fn permits(&self, job: &PreparedJob, control: &Control, now: i64) -> bool {
        self.0.permits(job, control, Purpose::Execute, now)
    }
}
impl AuthorizedSnapshotPublish {
    pub(crate) fn scope(&self) -> &JobScope {
        &self.authority.scope
    }
    pub(crate) fn root(&self) -> Option<&[u8; 32]> {
        self.authority.root.as_ref()
    }
    pub(crate) fn binds_descriptor(
        &self,
        descriptor: &crate::sandbox::compiled_snapshot::Descriptor,
    ) -> bool {
        descriptor.binding.tenant_id == self.authority.tenant
            && descriptor.binding.project_id == self.authority.project
            && descriptor
                .binding
                .base_prepared_request_sha256
                .raw()
                .is_ok_and(|value| value == self.authority.base)
            && descriptor
                .snapshot_key_sha256
                .raw()
                .is_ok_and(|value| value == self.authority.key)
            && descriptor.bytes().is_ok_and(|bytes| {
                self.root() == Some(&ContentSha256::of(&bytes).raw().unwrap_or([0; 32]))
            })
    }
    pub(crate) fn valid_at(&self, now: i64) -> bool {
        now < self.authority.expires
    }
}
impl<R: Ed25519PublicKeyResolver> GrantVerifier<R> {
    pub(crate) fn verify_snapshot_compile(
        &self,
        grant: &SignedSandboxJobGrantV1,
        peer: &str,
        job: &PreparedJob,
        control: &Control,
        now: i64,
    ) -> Result<AuthorizedSnapshotCompile, GrantRejected> {
        let (authority, _) =
            self.snapshot_authority(grant, peer, now, RustCompiledSnapshotPurposeV1::Compile)?;
        if !authority.permits(job, control, Purpose::Compile, now)
            || job.workspace().is_some() != authority.workspace_original_visit.is_some()
        {
            return Err(GrantRejected);
        }
        Ok(AuthorizedSnapshotCompile(authority))
    }
    pub(crate) fn verify_snapshot_execute(
        &self,
        grant: &SignedSandboxJobGrantV1,
        peer: &str,
        job: &PreparedJob,
        control: &Control,
        now: i64,
    ) -> Result<AuthorizedSnapshotExecute, GrantRejected> {
        let (authority, _) =
            self.snapshot_authority(grant, peer, now, RustCompiledSnapshotPurposeV1::Execute)?;
        if !authority.permits(job, control, Purpose::Execute, now) {
            return Err(GrantRejected);
        }
        Ok(AuthorizedSnapshotExecute(authority))
    }
    pub(crate) fn verify_snapshot_read(
        &self,
        grant: &SignedSandboxJobGrantV1,
        peer: &str,
        job: &PreparedJob,
        control: &Control,
        now: i64,
    ) -> Result<AuthorizedSnapshotRead, GrantRejected> {
        let (authority, _) =
            self.snapshot_authority(grant, peer, now, RustCompiledSnapshotPurposeV1::Read)?;
        if !authority.permits(job, control, Purpose::Execute, now) {
            return Err(GrantRejected);
        }
        Ok(AuthorizedSnapshotRead(authority))
    }
    pub(crate) fn verify_snapshot_publish(
        &self,
        grant: &SignedSandboxJobGrantV1,
        peer: &str,
        now: i64,
    ) -> Result<AuthorizedSnapshotPublish, GrantRejected> {
        let (authority, claims) =
            self.snapshot_authority(grant, peer, now, RustCompiledSnapshotPurposeV1::Publish)?;
        let compilation_job_key = claims
            .compilation_job_key
            .as_slice()
            .try_into()
            .map_err(|_| GrantRejected)?;
        if !identity(&claims.compilation_runtime_id)
            || claims.compilation_lease_epoch == 0
            || claims.compilation_request_digest.as_slice() != authority.request
        {
            return Err(GrantRejected);
        }
        Ok(AuthorizedSnapshotPublish {
            authority,
            compilation_job_key,
            compilation_runtime_id: claims.compilation_runtime_id,
            compilation_lease_epoch: claims.compilation_lease_epoch,
        })
    }
    #[allow(clippy::too_many_lines)] // Keep ordered authority checks and durable phase fences visible together.
    fn snapshot_authority(
        &self,
        grant: &SignedSandboxJobGrantV1,
        peer: &str,
        now: i64,
        purpose: RustCompiledSnapshotPurposeV1,
    ) -> Result<(Authority, RustCompiledSnapshotGrantClaimsV1), GrantRejected> {
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
        let scanned = scan_message(&grant.claims_bytes, Schema::CompiledSnapshotGrant)
            .map_err(|_| GrantRejected)?;
        if scanned.contains(42) {
            let access = scan_message(
                scanned
                    .length_field(42, "the original Code visit access is missing")
                    .map_err(|_| GrantRejected)?,
                Schema::OriginalCodeVisitAccess,
            )
            .map_err(|_| GrantRejected)?;
            scan_message(
                access
                    .length_field(1, "the original Code visit reference is missing")
                    .map_err(|_| GrantRejected)?,
                Schema::OriginalCodeVisitReference,
            )
            .map_err(|_| GrantRejected)?;
        }
        let claims = RustCompiledSnapshotGrantClaimsV1::decode(grant.claims_bytes.as_slice())
            .map_err(|_| GrantRejected)?;
        let workspace_original_visit = match claims.original_code_visit_access.as_ref() {
            Some(access) => {
                if purpose != RustCompiledSnapshotPurposeV1::Compile
                    || !crate::sandbox::code_recovery::hex_id(&claims.execution_id, 32, false)
                    || !(1..=i64::MAX as u64).contains(&claims.generation)
                    || !crate::sandbox::code_recovery::hex_id(&claims.activation_id, 64, true)
                    || !crate::sandbox::code_recovery::hex_id(&access.claim_id, 32, false)
                    || !(1..=i64::MAX as u64).contains(&access.claim_attempt)
                    || !(1..=i64::MAX as u64).contains(&access.lease_epoch)
                    || access.fence_sha256.len() != 32
                    || claims.issued_at_unix_millis <= 0
                {
                    return Err(GrantRejected);
                }
                let reference = access.original_visit.as_ref().ok_or(GrantRejected)?;
                let reference = crate::sandbox::code_recovery::OriginalCodeVisitRef {
                    visit_id: reference.visit_id.clone(),
                    revision: reference.revision,
                    digest_sha256: reference.digest_sha256.clone(),
                };
                if !reference.valid() {
                    return Err(GrantRejected);
                }
                Some(reference)
            }
            None => None,
        };
        let lifetime = claims
            .expires_at_unix_millis
            .checked_sub(claims.issued_at_unix_millis)
            .ok_or(GrantRejected)?;
        let request = claims
            .request_digest
            .as_slice()
            .try_into()
            .map_err(|_| GrantRejected)?;
        let base = claims
            .base_prepared_request_sha256
            .as_slice()
            .try_into()
            .map_err(|_| GrantRejected)?;
        let key = claims
            .snapshot_key_sha256
            .as_slice()
            .try_into()
            .map_err(|_| GrantRejected)?;
        let root = if purpose == RustCompiledSnapshotPurposeV1::Compile {
            if !claims.descriptor_sha256.is_empty() {
                return Err(GrantRejected);
            }
            None
        } else {
            Some(
                claims
                    .descriptor_sha256
                    .as_slice()
                    .try_into()
                    .map_err(|_| GrantRejected)?,
            )
        };
        if claims.revision != 4
            || claims.purpose != purpose as i32
            || claims.generation == 0
            || !identity(&claims.tenant_id)
            || claims.project_id <= 0
            || !identity(&claims.execution_id)
            || !identity(&claims.activation_id)
            || claims.submitter_workload_identity != peer
            || claims.audience != self.audience
            || !(1..=30000).contains(&lifetime)
            || claims.issued_at_unix_millis > now
            || now >= claims.expires_at_unix_millis
            || (purpose != RustCompiledSnapshotPurposeV1::Publish
                && (!claims.compilation_job_key.is_empty()
                    || !claims.compilation_runtime_id.is_empty()
                    || !claims.compilation_request_digest.is_empty()
                    || claims.compilation_lease_epoch != 0))
        {
            return Err(GrantRejected);
        }
        let mut hash = digest::Context::new(&digest::SHA256);
        hash.update(b"elitea.sandbox.activation.v1\0");
        for part in [&claims.execution_id, &claims.activation_id] {
            hash.update(&(part.len() as u64).to_be_bytes());
            hash.update(part.as_bytes());
        }
        let mut activation = [0; 32];
        activation.copy_from_slice(hash.finish().as_ref());
        let scope = JobScope::new(
            claims.tenant_id.clone(),
            claims.project_id,
            activation,
            request,
        )
        .map_err(|_| GrantRejected)?;
        Ok((
            Authority {
                scope,
                tenant: claims.tenant_id.clone(),
                project: claims.project_id,
                expires: claims.expires_at_unix_millis,
                request,
                base,
                key,
                root,
                execution: claims.execution_id.clone(),
                generation: claims.generation,
                dispatch: claims.activation_id.clone(),
                workspace_original_visit,
            },
            claims,
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use ring::signature::KeyPair as _;
    struct Keys([u8; 32]);
    impl Ed25519PublicKeyResolver for Keys {
        fn resolve_ed25519_public_key(&self, _key_id: &str) -> Option<[u8; 32]> {
            Some(self.0)
        }
    }
    fn sign(key: &signature::Ed25519KeyPair, bytes: Vec<u8>) -> SignedSandboxJobGrantV1 {
        let mut message = DOMAIN.to_vec();
        message.extend_from_slice(&(bytes.len() as u64).to_be_bytes());
        message.extend_from_slice(&bytes);
        SignedSandboxJobGrantV1 {
            key_id: "key-1".into(),
            signature: key.sign(&message).as_ref().to_vec(),
            claims_bytes: bytes,
        }
    }
    fn fixture(
        purpose: RustCompiledSnapshotPurposeV1,
    ) -> (
        signature::Ed25519KeyPair,
        GrantVerifier<Keys>,
        PreparedJob,
        Control,
        RustCompiledSnapshotGrantClaimsV1,
    ) {
        use crate::sandbox::{compiled_snapshot::SnapshotProfile, request::Language};
        let template: Control = serde_json::from_slice(include_bytes!(
            "../../tests/fixtures/compiled-snapshot-v1/compile-control.json"
        ))
        .unwrap();
        let job = PreparedJob::new(
            Language::Rust,
            "pub fn main(){}".into(),
            std::collections::BTreeMap::new(),
            template.binding.execution_image_digest.clone(),
            template.binding.policy_revision.clone(),
            30,
        )
        .unwrap();
        let binding = SnapshotProfile::new(template.binding)
            .unwrap()
            .binding(&job, "tenant", 2)
            .unwrap();
        let control = Control {
            revision: 1,
            snapshot_key_sha256: binding.key().unwrap(),
            binding,
            descriptor_sha256: if purpose == RustCompiledSnapshotPurposeV1::Compile
                || purpose == RustCompiledSnapshotPurposeV1::Publish
            {
                None
            } else {
                Some(ContentSha256::of(b"descriptor"))
            },
        };
        let key = signature::Ed25519KeyPair::from_seed_unchecked(&[19; 32]).unwrap();
        let verifier = GrantVerifier::new(
            Keys(key.public_key().as_ref().try_into().unwrap()),
            "sandbox-prod".into(),
        )
        .unwrap();
        let claims = RustCompiledSnapshotGrantClaimsV1 {
            revision: 4,
            tenant_id: "tenant".into(),
            project_id: 2,
            execution_id: "execution-1".into(),
            activation_id: "activation-1".into(),
            request_digest: control
                .intent_digest(
                    if purpose == RustCompiledSnapshotPurposeV1::Compile
                        || purpose == RustCompiledSnapshotPurposeV1::Publish
                    {
                        Purpose::Compile
                    } else {
                        Purpose::Execute
                    },
                )
                .unwrap()
                .to_vec(),
            submitter_workload_identity: "worker-1".into(),
            audience: "sandbox-prod".into(),
            issued_at_unix_millis: 1000,
            expires_at_unix_millis: 31000,
            generation: 1,
            purpose: purpose as i32,
            base_prepared_request_sha256: job.fingerprint().unwrap().to_vec(),
            snapshot_key_sha256: control.snapshot_key_sha256.raw().unwrap().to_vec(),
            descriptor_sha256: control
                .descriptor_sha256
                .as_ref()
                .map(|hash| hash.raw().unwrap().to_vec())
                .unwrap_or_default(),
            ..Default::default()
        };
        (key, verifier, job, control, claims)
    }
    #[test]
    fn compile_and_execute_roles_are_not_interchangeable_or_legacy_authority() {
        let (key, verifier, job, control, claims) = fixture(RustCompiledSnapshotPurposeV1::Compile);
        let grant = sign(&key, claims.encode_to_vec());
        assert!(
            verifier
                .verify_snapshot_compile(&grant, "worker-1", &job, &control, 1000)
                .is_ok()
        );
        assert!(
            verifier
                .verify_snapshot_execute(&grant, "worker-1", &job, &control, 1000)
                .is_err()
        );
        assert!(
            verifier
                .verify_snapshot_read(&grant, "worker-1", &job, &control, 1000)
                .is_err()
        );
        assert!(
            verifier
                .verify_snapshot_publish(&grant, "worker-1", 1000)
                .is_err()
        );
        assert!(verifier.verify(&grant, "worker-1", &job, 1000).is_err());
        assert!(
            verifier
                .verify_cancellation(&grant, "worker-1", 1000)
                .is_err()
        );
        let (key, verifier, job, control, claims) = fixture(RustCompiledSnapshotPurposeV1::Execute);
        let grant = sign(&key, claims.encode_to_vec());
        assert!(
            verifier
                .verify_snapshot_execute(&grant, "worker-1", &job, &control, 1000)
                .is_ok()
        );
        assert!(
            verifier
                .verify_snapshot_compile(&grant, "worker-1", &job, &control, 1000)
                .is_err()
        );
        assert!(
            verifier
                .verify_snapshot_read(&grant, "worker-1", &job, &control, 1000)
                .is_err()
        );
    }
    #[test]
    #[allow(
        clippy::too_many_lines,
        reason = "Keep the complete identity and failure assertions in one fixture."
    )]
    fn compiled_workspace_requires_exact_compile_only_original_visit_access() {
        use crate::protocol::elitea::runtime::v1::{
            OriginalCodeVisitAccessV1, OriginalCodeVisitRefV1,
        };
        let (key, verifier, job, mut control, mut claims) =
            fixture(RustCompiledSnapshotPurposeV1::Compile);
        let workspace = crate::sandbox::workspace::WorkspaceManifest::from_transport(
            include_bytes!(
                "../../../../libs/proto/elitea/runtime/v1/code_workspace_manifest_v1.json"
            ),
            "ccbf5e3ba22d5a09f07b063e4b8e65b7a7b1c6e6b4d0c6c4a5bb285e519c21c1",
            &crate::sandbox::workspace::WorkspacePolicy::default(),
        )
        .unwrap();
        let job = job.with_workspace(workspace.binding().unwrap()).unwrap();
        control.binding = crate::sandbox::compiled_snapshot::SnapshotProfile::new(control.binding)
            .unwrap()
            .binding(&job, "tenant", 2)
            .unwrap();
        control.snapshot_key_sha256 = control.binding.key().unwrap();
        claims.execution_id = "1".repeat(32);
        claims.activation_id = "2".repeat(64);
        claims.base_prepared_request_sha256 = job.fingerprint().unwrap().to_vec();
        claims.request_digest = control.intent_digest(Purpose::Compile).unwrap().to_vec();
        claims.snapshot_key_sha256 = control.snapshot_key_sha256.raw().unwrap().to_vec();
        let reference = OriginalCodeVisitRefV1 {
            visit_id: "3".repeat(64),
            revision: 1,
            digest_sha256: "4".repeat(64),
        };
        claims.original_code_visit_access = Some(OriginalCodeVisitAccessV1 {
            original_visit: Some(reference.clone()),
            claim_id: "5".repeat(32),
            claim_attempt: 2,
            lease_epoch: 3,
            fence_sha256: vec![6; 32],
        });
        let authority = verifier
            .verify_snapshot_compile(
                &sign(&key, claims.encode_to_vec()),
                "worker-1",
                &job,
                &control,
                1001,
            )
            .unwrap();
        assert_eq!(
            authority.original_execution(),
            (
                claims.execution_id.as_str(),
                1,
                claims.activation_id.as_str()
            )
        );
        assert_eq!(
            authority.workspace_original_visit().unwrap().visit_id,
            reference.visit_id
        );
        assert!(authority.permits(&job, &control, 1001));
        for case in 0..8 {
            let mut altered = claims.clone();
            match case {
                0 => altered.original_code_visit_access = None,
                1 => {
                    altered
                        .original_code_visit_access
                        .as_mut()
                        .unwrap()
                        .original_visit
                        .as_mut()
                        .unwrap()
                        .revision = 2;
                }
                2 => {
                    altered
                        .original_code_visit_access
                        .as_mut()
                        .unwrap()
                        .claim_id = "bad-claim".into();
                }
                3 => {
                    altered
                        .original_code_visit_access
                        .as_mut()
                        .unwrap()
                        .claim_attempt = 0;
                }
                4 => altered
                    .original_code_visit_access
                    .as_mut()
                    .unwrap()
                    .fence_sha256
                    .clear(),
                5 => altered.purpose = RustCompiledSnapshotPurposeV1::Execute as i32,
                6 => altered.purpose = RustCompiledSnapshotPurposeV1::Read as i32,
                _ => altered.purpose = RustCompiledSnapshotPurposeV1::Publish as i32,
            }
            let grant = sign(&key, altered.encode_to_vec());
            assert!(
                verifier
                    .verify_snapshot_compile(&grant, "worker-1", &job, &control, 1001)
                    .is_err(),
                "case {case}"
            );
            if case >= 5 {
                assert!(
                    verifier
                        .snapshot_authority(
                            &grant,
                            "worker-1",
                            1001,
                            RustCompiledSnapshotPurposeV1::try_from(altered.purpose).unwrap()
                        )
                        .is_err()
                );
            }
        }
    }
    #[test]
    fn compiled_workspace_broker5_has_compile_access_without_platform_or_execute_authority() {
        use crate::{
            protocol::elitea::runtime::v1::{OriginalCodeVisitAccessV1, OriginalCodeVisitRefV1},
            sandbox::{
                compiled_snapshot::SnapshotProfile, platform_client_binding::PlatformClientBinding,
                request::Language,
            },
        };
        let (key, verifier, _, mut control, mut claims) =
            fixture(RustCompiledSnapshotPurposeV1::Compile);
        let workspace = crate::sandbox::workspace::WorkspaceManifest::from_transport(
            include_bytes!(
                "../../../../libs/proto/elitea/runtime/v1/code_workspace_manifest_v1.json"
            ),
            "ccbf5e3ba22d5a09f07b063e4b8e65b7a7b1c6e6b4d0c6c4a5bb285e519c21c1",
            &crate::sandbox::workspace::WorkspacePolicy::default(),
        )
        .unwrap();
        let job = PreparedJob::new(
            Language::Rust,
            "pub fn main(){}".into(),
            std::collections::BTreeMap::new(),
            control.binding.execution_image_digest.clone(),
            "cargo-broker-execute-v1".into(),
            30,
        )
        .unwrap()
        .with_workspace(workspace.binding().unwrap())
        .unwrap()
        .with_platform_client(PlatformClientBinding::new("a".repeat(64), 8, 4096).unwrap())
        .unwrap();
        control.binding.policy_revision = "cargo-broker-execute-v1".into();
        control.binding = SnapshotProfile::new(control.binding)
            .unwrap()
            .binding(&job, "tenant", 2)
            .unwrap();
        control.snapshot_key_sha256 = control.binding.key().unwrap();
        claims.execution_id = "1".repeat(32);
        claims.activation_id = "2".repeat(64);
        claims.base_prepared_request_sha256 = job.fingerprint().unwrap().to_vec();
        claims.request_digest = control.intent_digest(Purpose::Compile).unwrap().to_vec();
        claims.snapshot_key_sha256 = control.snapshot_key_sha256.raw().unwrap().to_vec();
        claims.original_code_visit_access = Some(OriginalCodeVisitAccessV1 {
            original_visit: Some(OriginalCodeVisitRefV1 {
                visit_id: "3".repeat(64),
                revision: 1,
                digest_sha256: "4".repeat(64),
            }),
            claim_id: "5".repeat(32),
            claim_attempt: 2,
            lease_epoch: 3,
            fence_sha256: vec![6; 32],
        });
        let grant = sign(&key, claims.encode_to_vec());
        let compile = verifier
            .verify_snapshot_compile(&grant, "worker-1", &job, &control, 1001)
            .unwrap();
        assert!(compile.permits(&job, &control, 1001));
        assert!(compile.workspace_original_visit().is_some());
        assert!(
            verifier
                .verify_snapshot_execute(&grant, "worker-1", &job, &control, 1001)
                .is_err()
        );
        assert!(
            verifier
                .verify_snapshot_read(&grant, "worker-1", &job, &control, 1001)
                .is_err()
        );
        assert!(
            verifier
                .verify_snapshot_publish(&grant, "worker-1", 1001)
                .is_err()
        );
        // Correct access cannot turn a different signed broker image policy into eligibility.
        let mut raw: serde_json::Value =
            serde_json::from_slice(&job.to_transport().unwrap()).unwrap();
        raw["policy_revision"] = serde_json::json!("cargo-execute-v1");
        let denied = PreparedJob::from_transport(&serde_json::to_vec(&raw).unwrap()).unwrap();
        assert!(!compile_platform_permits(&denied));
        let mut missing_access = claims;
        missing_access.original_code_visit_access = None;
        assert!(
            verifier
                .verify_snapshot_compile(
                    &sign(&key, missing_access.encode_to_vec()),
                    "worker-1",
                    &job,
                    &control,
                    1001
                )
                .is_err()
        );
    }

    #[test]
    fn signed_compile_access_rejects_unknown_or_duplicate_nested_fields_before_decode() {
        let (key, verifier, _, _, mut claims) = fixture(RustCompiledSnapshotPurposeV1::Compile);
        claims.execution_id = "1".repeat(32);
        claims.activation_id = "2".repeat(64);
        // The owning scanner is recursive for this new capability. Prost unknown-field
        // discard must never turn a different signed selector into admitted authority.
        for nested in [
            vec![0x08, 0x01],
            vec![0x12, 0x01, b'a', 0x12, 0x01, b'b'],
            vec![0x32, 0x00],
        ] {
            let mut raw = claims.encode_to_vec();
            raw.extend_from_slice(&[0xd2, 0x02]);
            raw.push(u8::try_from(nested.len()).unwrap());
            raw.extend_from_slice(&nested);
            assert!(
                verifier
                    .snapshot_authority(
                        &sign(&key, raw),
                        "worker-1",
                        1001,
                        RustCompiledSnapshotPurposeV1::Compile
                    )
                    .is_err()
            );
        }
    }
    #[test]
    fn read_and_execute_share_exact_identity_but_separate_authority() {
        let (key, verifier, job, control, mut claims) =
            fixture(RustCompiledSnapshotPurposeV1::Execute);
        let execute = verifier
            .verify_snapshot_execute(
                &sign(&key, claims.encode_to_vec()),
                "worker-1",
                &job,
                &control,
                1000,
            )
            .unwrap();
        claims.purpose = RustCompiledSnapshotPurposeV1::Read as i32;
        let grant = sign(&key, claims.encode_to_vec());
        let read = verifier
            .verify_snapshot_read(&grant, "worker-1", &job, &control, 1000)
            .unwrap();
        assert!(read.scope() == execute.scope());
        assert!(
            verifier
                .verify_snapshot_execute(&grant, "worker-1", &job, &control, 1000)
                .is_err()
        );
    }
    #[test]
    fn wrong_scope_root_peer_base_image_and_expiry_are_rejected() {
        let (key, verifier, job, control, claims) = fixture(RustCompiledSnapshotPurposeV1::Execute);
        for altered in 0..7 {
            let mut changed = claims.clone();
            match altered {
                0 => changed.tenant_id = "other".into(),
                1 => changed.project_id = 3,
                2 => changed.descriptor_sha256 = vec![8; 32],
                3 => changed.base_prepared_request_sha256 = vec![8; 32],
                4 => changed.snapshot_key_sha256 = vec![8; 32],
                5 => changed.generation = 0,
                _ => changed.expires_at_unix_millis = 1000,
            }
            assert!(
                verifier
                    .verify_snapshot_execute(
                        &sign(&key, changed.encode_to_vec()),
                        "worker-1",
                        &job,
                        &control,
                        1000
                    )
                    .is_err()
            );
        }
        assert!(
            verifier
                .verify_snapshot_execute(
                    &sign(&key, claims.encode_to_vec()),
                    "other-worker",
                    &job,
                    &control,
                    1000
                )
                .is_err()
        );
        let mut changed = control.clone();
        changed.binding.execution_image_digest = format!("sha256:{}", "f".repeat(64));
        assert!(
            verifier
                .verify_snapshot_execute(
                    &sign(&key, claims.encode_to_vec()),
                    "worker-1",
                    &job,
                    &changed,
                    1000
                )
                .is_err()
        );
    }
    #[test]
    fn unknown_duplicate_and_mutated_signed_claims_fail_before_authorization() {
        let (key, verifier, job, control, claims) = fixture(RustCompiledSnapshotPurposeV1::Execute);
        let mut bytes = claims.encode_to_vec();
        bytes.extend_from_slice(&[8, 4]);
        assert!(
            verifier
                .verify_snapshot_execute(&sign(&key, bytes), "worker-1", &job, &control, 1000)
                .is_err()
        );
        let mut bytes = claims.encode_to_vec();
        bytes.extend_from_slice(&[0xf8, 0x03, 1]);
        assert!(
            verifier
                .verify_snapshot_execute(&sign(&key, bytes), "worker-1", &job, &control, 1000)
                .is_err()
        );
        let mut grant = sign(&key, claims.encode_to_vec());
        grant.claims_bytes[1] ^= 1;
        assert!(
            verifier
                .verify_snapshot_execute(&grant, "worker-1", &job, &control, 1000)
                .is_err()
        );
    }
    #[test]
    fn nonpublication_roles_reject_compile_provenance_smuggling() {
        let (key, verifier, job, control, claims) = fixture(RustCompiledSnapshotPurposeV1::Compile);
        for field in 0..4 {
            let mut changed = claims.clone();
            match field {
                0 => changed.compilation_job_key = vec![9; 32],
                1 => changed.compilation_runtime_id = "runtime".into(),
                2 => changed.compilation_request_digest = changed.request_digest.clone(),
                _ => changed.compilation_lease_epoch = 1,
            }
            assert!(
                verifier
                    .verify_snapshot_compile(
                        &sign(&key, changed.encode_to_vec()),
                        "worker-1",
                        &job,
                        &control,
                        1000
                    )
                    .is_err()
            );
        }
    }
    #[test]
    fn inert_native_compile_cannot_use_valid_publish_or_execute_authority() {
        let (key, verifier, job, control, mut claims) =
            fixture(RustCompiledSnapshotPurposeV1::Publish);
        claims.descriptor_sha256 = vec![7; 32];
        claims.compilation_job_key = vec![8; 32];
        claims.compilation_runtime_id = "compiler-runtime".into();
        claims.compilation_request_digest = claims.request_digest.clone();
        claims.compilation_lease_epoch = 2;
        let grant = sign(&key, claims.encode_to_vec());
        assert!(
            verifier
                .verify_snapshot_publish(&grant, "worker-1", 1000)
                .is_ok()
        );
        assert!(
            verifier
                .verify_snapshot_compile(&grant, "worker-1", &job, &control, 1000)
                .is_err()
        );
        let (key, verifier, job, control, claims) = fixture(RustCompiledSnapshotPurposeV1::Execute);
        let grant = sign(&key, claims.encode_to_vec());
        assert!(
            verifier
                .verify_snapshot_execute(&grant, "worker-1", &job, &control, 1000)
                .is_ok()
        );
        assert!(
            verifier
                .verify_snapshot_compile(&grant, "worker-1", &job, &control, 1000)
                .is_err()
        );
        let (key, verifier, job, control, claims) = fixture(RustCompiledSnapshotPurposeV1::Compile);
        let grant = sign(&key, claims.encode_to_vec());
        assert!(
            verifier
                .verify_snapshot_compile(&grant, "worker-1", &job, &control, 30999)
                .is_ok()
        );
        assert!(
            verifier
                .verify_snapshot_compile(&grant, "worker-1", &job, &control, 31000)
                .is_err()
        );
    }
}
