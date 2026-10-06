//! Renew distinct roles per native index without dispatching the original snapshot runtime.
use super::super::dependency_delivery::ContentProof;
use super::{
    AuthorizedSnapshotCompile, AuthorizedSnapshotExecute, AuthorizedSnapshotRead, Control,
    DependencyContentClient, DependencyDelivery, Descriptor, DiagnosticContext, DockerSupervisor,
    JobScope, LEASE_SECONDS, LedgerError, Phase, PreparedJob, Purpose, Reconciliation,
    SnapshotOperation, SnapshotReconciliation, Stage, SupervisorError, native_snapshot_scope,
    observe, validate_compile_receipt,
};

impl DockerSupervisor {
    /// Read the original compiler before requesting any native content authority.
    pub(crate) async fn reconcile_snapshot_compile(
        &self,
        authority: &AuthorizedSnapshotCompile,
        job: &PreparedJob,
        control: &Control,
        content: &DependencyContentClient,
    ) -> Result<Option<SnapshotReconciliation>, SupervisorError> {
        let scope = authority.scope();
        self.validate_snapshot(scope, job, control)?;
        if !authority.permits(job, control, chrono::Utc::now().timestamp_millis()) {
            return Err(SupervisorError::Invalid);
        }
        let record = match self.ledger.read(scope).await {
            Ok(record) => record,
            Err(LedgerError::Missing) => return Ok(None),
            Err(error) => return Err(error.into()),
        };
        if record.phase == Phase::Reserved {
            return Ok(None);
        }
        let proof = self
            .ledger
            .read_compiled(scope, control, Purpose::Compile)
            .await?;
        let _slot = self
            .capacity
            .try_acquire()
            .map_err(|_| SupervisorError::Busy)?;
        if record.phase != Phase::Dispatched {
            let completed = record.phase == Phase::Completed;
            let outcome = self.compiled_terminal(scope, record).await?;
            return Ok(Some(if completed {
                let descriptor = proof.descriptor.ok_or(SupervisorError::Receipt)?;
                if let Reconciliation::Terminal { record, .. } = &outcome {
                    validate_compile_receipt(
                        record
                            .result_json
                            .as_deref()
                            .ok_or(SupervisorError::Receipt)?
                            .as_bytes(),
                        &descriptor,
                    )?;
                }
                SnapshotReconciliation::Captured {
                    descriptor,
                    job_key: scope.key,
                }
            } else {
                SnapshotReconciliation::Outcome(outcome)
            }));
        }
        let Some(lease) = self
            .ledger
            .claim(scope, self.owner.clone(), LEASE_SECONDS)
            .await?
        else {
            return Ok(Some(SnapshotReconciliation::Outcome(
                Reconciliation::OwnedElsewhere,
            )));
        };
        let diagnostic = DiagnosticContext::capture(scope, &lease)
            .with_export_epoch(
                proof
                    .export_epoch
                    .and_then(|epoch| u64::try_from(epoch).ok()),
            )
            .claimed(&lease);
        let result = self
            .snapshot_heartbeat(
                scope,
                &lease,
                self.capture_snapshot_owned(scope, &lease, control, content),
                Some(diagnostic),
            )
            .await?;
        if matches!(&result, SnapshotReconciliation::Captured { .. }) {
            observe(diagnostic, Stage::LeaseRelease, async {
                Ok(self.ledger.release(&lease).await?)
            })
            .await?;
        }
        Ok(Some(result))
    }

    /// Independent Execute, selected-root Read, and native Content roles authorize only an inert index.
    #[allow(clippy::too_many_arguments)] // Keep all three independently verified roles beside the exact selected descriptor and index.
    pub(crate) async fn hydrate_snapshot_execute(
        &self,
        authority: &AuthorizedSnapshotExecute,
        read: &AuthorizedSnapshotRead,
        job: &PreparedJob,
        control: &Control,
        descriptor: &Descriptor,
        canonical: &[u8],
        delivery: DependencyDelivery<'_>,
        index: u32,
    ) -> Result<SnapshotReconciliation, SupervisorError> {
        self.validate_snapshot_execute(authority, read, job, control, descriptor, canonical)?;
        self.hydrate_snapshot_index(
            authority.scope(),
            job,
            control,
            Purpose::Execute,
            Some(canonical),
            delivery,
            index,
        )
        .await
    }
    /// One successful reply acknowledges only this immutable inert transfer.
    #[allow(clippy::too_many_lines)] // Keep index authority, lease, cancellation, and acknowledgement fences visible.
    pub(crate) async fn hydrate_snapshot_compile(
        &self,
        authority: &AuthorizedSnapshotCompile,
        job: &PreparedJob,
        control: &Control,
        delivery: DependencyDelivery<'_>,
        index: u32,
    ) -> Result<SnapshotReconciliation, SupervisorError> {
        let scope = authority.scope();
        self.validate_snapshot(scope, job, control)?;
        if !authority.permits(job, control, chrono::Utc::now().timestamp_millis())
            || delivery.bundle.native().is_none()
        {
            return Err(SupervisorError::Invalid);
        }
        self.hydrate_snapshot_index(scope, job, control, Purpose::Compile, None, delivery, index)
            .await
    }

    #[allow(clippy::too_many_lines)] // Keep index authority, lease, cancellation, and acknowledgement fences visible.
    #[allow(clippy::too_many_arguments)] // A private typed purpose preserves the exact role-specific intent and descriptor journal.
    async fn hydrate_snapshot_index(
        &self,
        scope: &JobScope,
        job: &PreparedJob,
        control: &Control,
        purpose: Purpose,
        canonical: Option<&[u8]>,
        delivery: DependencyDelivery<'_>,
        index: u32,
    ) -> Result<SnapshotReconciliation, SupervisorError> {
        if delivery.bundle.native().is_none() {
            return Err(SupervisorError::Invalid);
        }
        native_snapshot_scope(scope, job, Some((delivery.authorization, delivery.bundle)))?;
        let index = native_compile_index(delivery.bundle, index)?;
        let _slot = self
            .capacity
            .try_acquire()
            .map_err(|_| SupervisorError::Busy)?;
        let record = self.ledger.reserve(scope).await?;
        if record.phase != Phase::Reserved {
            let proof = self.ledger.read_compiled(scope, control, purpose).await?;
            if purpose == Purpose::Execute && proof.descriptor.as_deref() != canonical {
                return Err(SupervisorError::Receipt);
            }
            return if record.phase == Phase::Dispatched {
                Ok(SnapshotReconciliation::Hydrated { ready: true })
            } else {
                self.compiled_terminal(scope, record)
                    .await
                    .map(SnapshotReconciliation::Outcome)
            };
        }
        let lease = self.claim_indexed_transfer(scope).await?;
        let operation = async {
            if let Some(stopped) = self.stop_if_requested(scope, &lease).await? {
                return Ok(SnapshotReconciliation::Outcome(stopped));
            }
            if let Some(expired) = self.expire_hydration_if_needed(scope, &lease).await? {
                return Ok(SnapshotReconciliation::Outcome(expired));
            }
            if lease.observed_phase != Phase::Reserved {
                let proof = self.ledger.read_compiled(scope, control, purpose).await?;
                if purpose == Purpose::Execute && proof.descriptor.as_deref() != canonical {
                    return Err(SupervisorError::Receipt);
                }
                return Ok(SnapshotReconciliation::Hydrated { ready: true });
            }
            self.ledger
                .record_compiled_intent(&lease, control, purpose, canonical)
                .await?;
            let identity = self
                .provision_snapshot(scope, &lease, job, control, purpose)
                .await?;
            let bytes = control
                .bytes(purpose)
                .map_err(|_| SupervisorError::Invalid)?;
            self.import_snapshot(&identity, SnapshotOperation::Control, &bytes, &bytes)
                .await?;
            self.ledger.renew(&lease, LEASE_SECONDS).await?;
            // A later index cannot skip its immutable predecessors. Lost acknowledgements
            // may replay an index, but every prior object must still match the original.
            for file in delivery.bundle.files().iter().take(index) {
                let mut proof = ContentProof::new(file.bytes());
                self.runtime
                    .export_bundle_dependency(
                        &identity,
                        delivery.bundle,
                        file.name(),
                        file.bytes(),
                        &mut proof,
                    )
                    .await
                    .map_err(SupervisorError::Runtime)?;
                proof.verify(file.sha256())?;
            }
            if let Some(stopped) = self.stop_if_requested(scope, &lease).await? {
                return Ok(SnapshotReconciliation::Outcome(stopped));
            }
            self.hydrate_index_owned(&identity, delivery.bundle, index, delivery)
                .await?;
            if let Some(stopped) = self.stop_if_requested(scope, &lease).await? {
                return Ok(SnapshotReconciliation::Outcome(stopped));
            }
            if let Some(expired) = self.expire_hydration_if_needed(scope, &lease).await? {
                return Ok(SnapshotReconciliation::Outcome(expired));
            }
            // Renew after import so a stale owner cannot acknowledge its transfer.
            self.ledger.renew(&lease, LEASE_SECONDS).await?;
            Ok(SnapshotReconciliation::Hydrated {
                ready: index == delivery.bundle.file_count(),
            })
        };
        tokio::pin!(operation);
        let mut heartbeat = tokio::time::interval(std::time::Duration::from_secs(20));
        heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        heartbeat.tick().await;
        let result = loop {
            tokio::select! {
                result = &mut operation => break result,
                _ = heartbeat.tick() => {
                    if let Some(stopped) = self.stop_if_requested(scope, &lease).await? {
                        break Ok(SnapshotReconciliation::Outcome(stopped));
                    }
                    if let Some(expired) = self.expire_hydration_if_needed(scope, &lease).await? {
                        break Ok(SnapshotReconciliation::Outcome(expired));
                    }
                    self.ledger.renew(&lease, LEASE_SECONDS).await?;
                }
            }
        };
        // Unknown allocation retains its lease. A confirmed binding allows safe renewal
        // by a later owner. A busy lease never becomes a false index acknowledgement.
        if result.is_ok() || self.ledger.read(scope).await?.runtime_id.is_some() {
            self.release_failed_preparation(&lease).await?;
        }
        result
    }
}

fn native_compile_index(
    bundle: &crate::sandbox::dependency_bundle::DependencyBundle,
    index: u32,
) -> Result<usize, SupervisorError> {
    let index = usize::try_from(index).map_err(|_| SupervisorError::Invalid)?;
    if bundle.native().is_none() || index > bundle.file_count() {
        return Err(SupervisorError::Invalid);
    }
    Ok(index)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::protocol::{
        command::Ed25519PublicKeyResolver,
        elitea::runtime::v1::{
            RustCompiledSnapshotGrantClaimsV1, RustCompiledSnapshotPurposeV1,
            SandboxJobGrantClaimsV1, SignedSandboxJobGrantV1,
        },
        sandbox_grant::GrantVerifier,
    };
    use prost::Message as _;
    use ring::signature::{self, KeyPair as _};

    struct Keys([u8; 32]);
    impl Ed25519PublicKeyResolver for Keys {
        fn resolve_ed25519_public_key(&self, key: &str) -> Option<[u8; 32]> {
            (key == "test-key").then_some(self.0)
        }
    }
    struct NativeFixture {
        key: signature::Ed25519KeyPair,
        verifier: GrantVerifier<Keys>,
        job: PreparedJob,
        control: Control,
        execute: RustCompiledSnapshotGrantClaimsV1,
        content: SandboxJobGrantClaimsV1,
        bundle: crate::sandbox::dependency_bundle::DependencyBundle,
        now: i64,
    }
    fn signed(key: &signature::Ed25519KeyPair, bytes: Vec<u8>) -> SignedSandboxJobGrantV1 {
        let mut message = b"elitea.sandbox.job-grant.ed25519.v1\0".to_vec();
        message.extend_from_slice(&u64::try_from(bytes.len()).unwrap().to_be_bytes());
        message.extend_from_slice(&bytes);
        SignedSandboxJobGrantV1 {
            key_id: "test-key".into(),
            signature: key.sign(&message).as_ref().to_vec(),
            claims_bytes: bytes,
        }
    }
    fn native_request() -> (
        PreparedJob,
        Control,
        crate::sandbox::dependency_bundle::DependencyBundle,
    ) {
        use crate::sandbox::{
            compiled_snapshot::{ContentSha256, SnapshotProfile},
            native_bundle::{NativeKind, NativePlatform, canonical},
            request::{Language, NativeDependencies},
        };
        let declaration = "[dependencies]\n";
        let source = ContentSha256::of(declaration.as_bytes());
        let mut value: serde_json::Value =
            serde_json::from_slice(include_bytes!("native-cargo-v2.json")).unwrap();
        value["source_sha256"] = source.as_str().into();
        value["payload"]["declaration_sha256"] = source.as_str().into();
        value.as_object_mut().unwrap().remove("digest");
        let root = ContentSha256::of(&canonical(&value).unwrap());
        value["digest"] = root.as_str().into();
        let bundle = crate::sandbox::dependency_bundle::DependencyBundle::parse_record(
            &canonical(&value).unwrap(),
        )
        .unwrap();
        let native = bundle.native().unwrap();
        let job = PreparedJob::new(
            Language::Rust,
            "pub fn run(){}".into(),
            std::collections::BTreeMap::new(),
            native.record.execution_image_digest.clone(),
            native.record.execution_policy_revision.clone(),
            30,
        )
        .unwrap()
        .with_native_dependency_bundle(
            root.as_str().into(),
            NativeDependencies {
                kind: NativeKind::Cargo,
                platform: NativePlatform {
                    os: "linux".into(),
                    arch: "amd64".into(),
                    abi: "gnu".into(),
                },
                preparation_sha256: native.record.preparation_sha256.clone(),
                source_sha256: source.as_str().into(),
                dependencies_toml: Some(declaration.into()),
            },
        )
        .unwrap();
        let original: Control = serde_json::from_slice(include_bytes!(
            "../../tests/fixtures/compiled-snapshot-v1/compile-control.json"
        ))
        .unwrap();
        let mut template = original.binding;
        template
            .compilation_image_digest
            .clone_from(&native.record.execution_image_digest);
        template
            .execution_image_digest
            .clone_from(&native.record.execution_image_digest);
        template
            .policy_revision
            .clone_from(&native.record.execution_policy_revision);
        let binding = SnapshotProfile::new(template)
            .unwrap()
            .binding(&job, "tenant", 2)
            .unwrap();
        let control = Control {
            revision: 1,
            snapshot_key_sha256: binding.key().unwrap(),
            binding,
            descriptor_sha256: Some(ContentSha256::of(b"descriptor")),
        };
        (job, control, bundle)
    }
    fn native_fixture() -> NativeFixture {
        use crate::sandbox::compiled_snapshot::ContentSha256;
        let (job, control, bundle) = native_request();
        let root = ContentSha256::parse(bundle.root().into()).unwrap();
        let key = signature::Ed25519KeyPair::from_seed_unchecked(&[23; 32]).unwrap();
        let verifier = GrantVerifier::new(
            Keys(key.public_key().as_ref().try_into().unwrap()),
            "sandbox-test".into(),
        )
        .unwrap();
        let now = chrono::Utc::now().timestamp_millis();
        let execute = RustCompiledSnapshotGrantClaimsV1 {
            revision: 4,
            tenant_id: "tenant".into(),
            project_id: 2,
            execution_id: "execution".into(),
            activation_id: "original-native-execute".into(),
            request_digest: control.intent_digest(Purpose::Execute).unwrap().to_vec(),
            submitter_workload_identity: "worker".into(),
            audience: "sandbox-test".into(),
            issued_at_unix_millis: now,
            expires_at_unix_millis: now + 30_000,
            generation: 1,
            purpose: RustCompiledSnapshotPurposeV1::Execute as i32,
            base_prepared_request_sha256: job.fingerprint().unwrap().to_vec(),
            snapshot_key_sha256: control.snapshot_key_sha256.raw().unwrap().to_vec(),
            descriptor_sha256: control
                .descriptor_sha256
                .as_ref()
                .unwrap()
                .raw()
                .unwrap()
                .to_vec(),
            ..Default::default()
        };
        let content = SandboxJobGrantClaimsV1 {
            revision: 3,
            tenant_id: execute.tenant_id.clone(),
            project_id: execute.project_id,
            execution_id: execute.execution_id.clone(),
            activation_id: execute.activation_id.clone(),
            request_digest: job.fingerprint().unwrap().to_vec(),
            submitter_workload_identity: execute.submitter_workload_identity.clone(),
            audience: execute.audience.clone(),
            issued_at_unix_millis: now,
            expires_at_unix_millis: now + 30_000,
            generation: 1,
            dependency_bundle_sha256: root.raw().unwrap().to_vec(),
            ..Default::default()
        };
        NativeFixture {
            key,
            verifier,
            job,
            control,
            execute,
            content,
            bundle,
            now,
        }
    }
    #[test]
    fn native_execute_read_and_content_have_valid_signed_but_distinct_authority() {
        let f = native_fixture();
        let execute_grant = signed(&f.key, f.execute.encode_to_vec());
        let execute = f
            .verifier
            .verify_snapshot_execute(&execute_grant, "worker", &f.job, &f.control, f.now)
            .unwrap();
        let mut claims = f.execute.clone();
        claims.purpose = RustCompiledSnapshotPurposeV1::Read as i32;
        let read_grant = signed(&f.key, claims.encode_to_vec());
        let read = f
            .verifier
            .verify_snapshot_read(&read_grant, "worker", &f.job, &f.control, f.now)
            .unwrap();
        assert!(read.scope() == execute.scope());
        let content_grant = signed(&f.key, f.content.encode_to_vec());
        let content = f
            .verifier
            .verify_content(&content_grant, "worker", f.now)
            .unwrap();
        assert!(
            native_snapshot_scope(execute.scope(), &f.job, Some((&content, &f.bundle))).is_ok()
        );
        assert!(
            f.verifier
                .verify_snapshot_execute(&read_grant, "worker", &f.job, &f.control, f.now)
                .is_err()
        );
        assert!(
            f.verifier
                .verify_snapshot_read(&execute_grant, "worker", &f.job, &f.control, f.now)
                .is_err()
        );
        for grant in [&execute_grant, &read_grant] {
            assert!(f.verifier.verify_content(grant, "worker", f.now).is_err());
        }
        assert!(
            f.verifier
                .verify_snapshot_execute(&content_grant, "worker", &f.job, &f.control, f.now)
                .is_err()
        );
        assert!(
            f.verifier
                .verify_snapshot_read(&content_grant, "worker", &f.job, &f.control, f.now)
                .is_err()
        );
        for purpose in [
            RustCompiledSnapshotPurposeV1::Compile,
            RustCompiledSnapshotPurposeV1::Publish,
        ] {
            let mut original = f.control.clone();
            original.descriptor_sha256 = None;
            let mut claims = f.execute.clone();
            claims.purpose = purpose as i32;
            claims.request_digest = original.intent_digest(Purpose::Compile).unwrap().to_vec();
            if purpose == RustCompiledSnapshotPurposeV1::Compile {
                claims.descriptor_sha256.clear();
            } else {
                claims.compilation_job_key = execute.scope().key.to_vec();
                claims.compilation_runtime_id = "original-compiler".into();
                claims.compilation_request_digest = claims.request_digest.clone();
                claims.compilation_lease_epoch = 3;
            }
            let grant = signed(&f.key, claims.encode_to_vec());
            if purpose == RustCompiledSnapshotPurposeV1::Compile {
                assert!(
                    f.verifier
                        .verify_snapshot_compile(&grant, "worker", &f.job, &original, f.now)
                        .is_ok()
                );
            } else {
                assert!(
                    f.verifier
                        .verify_snapshot_publish(&grant, "worker", f.now)
                        .is_ok()
                );
            }
            assert!(
                f.verifier
                    .verify_snapshot_execute(&grant, "worker", &f.job, &f.control, f.now)
                    .is_err()
            );
        }
    }
    #[test]
    fn signed_native_content_root_scope_and_expiry_cannot_cross_execute_admission() {
        let f = native_fixture();
        let execute = f
            .verifier
            .verify_snapshot_execute(
                &signed(&f.key, f.execute.encode_to_vec()),
                "worker",
                &f.job,
                &f.control,
                f.now,
            )
            .unwrap();
        for mode in 0..4 {
            let mut claims = f.content.clone();
            match mode {
                0 => claims.dependency_bundle_sha256 = vec![9; 32],
                1 => claims.request_digest = vec![9; 32],
                2 => claims.activation_id = "other-activation".into(),
                3 => {
                    claims.issued_at_unix_millis = f.now - 30_000;
                    claims.expires_at_unix_millis = f.now;
                }
                _ => unreachable!(),
            }
            let verified_at = if mode == 3 { f.now - 1 } else { f.now };
            let authority = f
                .verifier
                .verify_content(
                    &signed(&f.key, claims.encode_to_vec()),
                    "worker",
                    verified_at,
                )
                .unwrap();
            assert!(
                native_snapshot_scope(execute.scope(), &f.job, Some((&authority, &f.bundle)))
                    .is_err()
            );
        }
        for purpose in [
            RustCompiledSnapshotPurposeV1::Execute,
            RustCompiledSnapshotPurposeV1::Read,
        ] {
            let mut claims = f.execute.clone();
            claims.purpose = purpose as i32;
            let valid = signed(&f.key, claims.encode_to_vec());
            if purpose == RustCompiledSnapshotPurposeV1::Execute {
                assert!(
                    f.verifier
                        .verify_snapshot_execute(
                            &valid,
                            "worker",
                            &f.job,
                            &f.control,
                            f.now + 29_999
                        )
                        .is_ok()
                );
                assert!(
                    f.verifier
                        .verify_snapshot_execute(
                            &valid,
                            "worker",
                            &f.job,
                            &f.control,
                            f.now + 30_000
                        )
                        .is_err()
                );
            } else {
                assert!(
                    f.verifier
                        .verify_snapshot_read(&valid, "worker", &f.job, &f.control, f.now + 29_999)
                        .is_ok()
                );
                assert!(
                    f.verifier
                        .verify_snapshot_read(&valid, "worker", &f.job, &f.control, f.now + 30_000)
                        .is_err()
                );
            }
            claims.descriptor_sha256 = vec![9; 32];
            let wrong = signed(&f.key, claims.encode_to_vec());
            if purpose == RustCompiledSnapshotPurposeV1::Execute {
                assert!(
                    f.verifier
                        .verify_snapshot_execute(&wrong, "worker", &f.job, &f.control, f.now)
                        .is_err()
                );
            } else {
                assert!(
                    f.verifier
                        .verify_snapshot_read(&wrong, "worker", &f.job, &f.control, f.now)
                        .is_err()
                );
            }
        }
    }

    #[test]
    fn native_indices_are_bounded_by_the_exact_immutable_inventory() {
        let bundle = crate::sandbox::dependency_bundle::DependencyBundle::parse_record(
            include_bytes!("native-cargo-v2.json"),
        )
        .unwrap();
        assert_eq!(bundle.file_count(), 2);
        for index in 0..=2 {
            assert_eq!(
                native_compile_index(&bundle, index).unwrap(),
                usize::try_from(index).unwrap()
            );
        }
        assert!(native_compile_index(&bundle, 3).is_err());
        assert!(native_compile_index(&bundle, u32::MAX).is_err());
    }
}
