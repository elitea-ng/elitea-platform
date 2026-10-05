use super::*;
use ring::signature::KeyPair as _;
use std::collections::BTreeMap;
pub(crate) struct Keys([u8; 32]);
impl Ed25519PublicKeyResolver for Keys {
    fn resolve_ed25519_public_key(&self, id: &str) -> Option<[u8; 32]> {
        (id == "fixture-key").then_some(self.0)
    }
}
pub(crate) struct Fixture {
    #[allow(
        dead_code,
        reason = "Retain the prepared-job identity in the signed grant fixture."
    )]
    pub(crate) job: PreparedJob,
    pub(crate) scope: JobScope,
    pub(crate) intent: AuthorizedCodeIntent,
    pub(crate) read: AuthorizedCodeRecovery,
    pub(crate) seal: AuthorizedCodeRecovery,
}
pub(super) fn sign(
    key: &signature::Ed25519KeyPair,
    claims: &impl Serialize,
    domain: &[u8],
    schema: &str,
) -> SignedCodeEnvelope {
    let bytes = serde_json::to_vec(claims).unwrap();
    let mut framed = domain.to_vec();
    framed.extend_from_slice(&(bytes.len() as u64).to_be_bytes());
    framed.extend_from_slice(&bytes);
    SignedCodeEnvelope {
        schema: schema.into(),
        key_id: "fixture-key".into(),
        claims_base64url: URL_SAFE_NO_PAD.encode(&bytes),
        signature_base64url: URL_SAFE_NO_PAD.encode(key.sign(&framed).as_ref()),
    }
}
pub(super) fn claims(
    execution: String,
) -> (
    signature::Ed25519KeyPair,
    GrantVerifier<Keys>,
    PreparedJob,
    JobScope,
    OriginalCodeIntentClaims,
) {
    let key = signature::Ed25519KeyPair::from_seed_unchecked(&[9; 32]).unwrap();
    let verifier = GrantVerifier::new(
        Keys(key.public_key().as_ref().try_into().unwrap()),
        "dns:supervisor.fixture".into(),
    )
    .unwrap()
    .with_code_owner_requester("dns:main.fixture".into())
    .unwrap();
    let job = PreparedJob::new(
        crate::sandbox::request::Language::Python,
        "return 41".into(),
        BTreeMap::from([("input".into(), serde_json::json!("original"))]),
        format!("sha256:{}", "a".repeat(64)),
        "fixture-v1".into(),
        30,
    )
    .unwrap();
    let dispatch = "2".repeat(64);
    let request = job.fingerprint().unwrap();
    let keyhex = crate::sandbox::code_recovery::original_job_key(&execution, &dispatch);
    let scope = JobScope::new(
        "code-owner-fixture".into(),
        1,
        decode32(&keyhex).unwrap(),
        request,
    )
    .unwrap();
    let claims = OriginalCodeIntentClaims {
        schema: "elitea.sandbox.original-code-intent.v1".into(),
        purpose: "whole_code_execute".into(),
        tenant_id: "code-owner-fixture".into(),
        project_id: 1,
        execution_id: execution,
        original_generation: 1,
        claim_id: "3".repeat(32),
        claim_attempt: 1,
        lease_epoch: 1,
        fence_sha256: "4".repeat(64),
        activation_id: "5".repeat(64),
        node_id: "code".into(),
        graph_thread: "owner-fixture-root".into(),
        step: 2,
        attempt: 1,
        node_digest: "6".repeat(64),
        dispatch_activation: dispatch,
        job_key: keyhex,
        request_digest: hex(&request),
        supervisor_audience: "dns:supervisor.fixture".into(),
        submitter_workload_identity: "dns:worker.fixture".into(),
        language: "python".into(),
        prepared_job_sha256: sha256(&job.to_transport().unwrap()),
        source_sha256: sha256(b"return 41"),
        input_sha256: sha256(br#"{"input":"original"}"#),
        issued_at_unix_millis: 1000,
        expires_at_unix_millis: 31_000,
    };
    (key, verifier, job, scope, claims)
}
pub(crate) fn code_owner_fixture(execution: String) -> Fixture {
    let (key, verifier, job, scope, claims) = claims(execution);
    let signed = sign(
        &key,
        &claims,
        INTENT_DOMAIN,
        "elitea.sandbox.original-code-intent-signed.v1",
    );
    let now = chrono::Utc::now().timestamp_millis();
    let mut claims = claims;
    claims.issued_at_unix_millis = now;
    claims.expires_at_unix_millis = now + 30_000;
    let signed = sign(&key, &claims, INTENT_DOMAIN, &signed.schema);
    let wire = serde_json::to_vec(&signed).unwrap();
    let intent = verifier
        .verify_code_intent(
            &wire,
            "dns:worker.fixture",
            &scope,
            &claims.execution_id,
            1,
            &claims.dispatch_activation,
            &job,
            now,
        )
        .unwrap();
    let binding = intent.binding(&scope, now).unwrap();
    let mut read = CodeRecoveryGrantClaims {
        schema: "elitea.sandbox.node-code-recovery-grant.v1".into(),
        tenant_id: "code-owner-fixture".into(),
        project_id: 1,
        execution_id: claims.execution_id.clone(),
        original_generation: 1,
        claim_id: "7".repeat(32),
        claim_attempt: 2,
        lease_epoch: 2,
        fence_sha256: "8".repeat(64),
        activation_id: claims.activation_id.clone(),
        node_id: claims.node_id.clone(),
        graph_thread: claims.graph_thread.clone(),
        step: 2,
        attempt: 1,
        expected_revision: 2,
        receipt_sha256: "9".repeat(64),
        dispatch_activation: claims.dispatch_activation.clone(),
        job_key: claims.job_key.clone(),
        request_digest: claims.request_digest.clone(),
        binding_sha256: sha256(&binding.canonical_bytes().unwrap()),
        supervisor_audience: claims.supervisor_audience.clone(),
        requester_workload_identity: "dns:main.fixture".into(),
        operation: CodeOwnerOperation::Read,
        issued_at_unix_millis: now,
        expires_at_unix_millis: now + 30_000,
    };
    let signed = sign(
        &key,
        &read,
        READ_DOMAIN,
        "elitea.sandbox.node-code-recovery-signed-grant.v1",
    );
    let read_authority = verifier
        .verify_code_recovery(
            &signed,
            "dns:main.fixture",
            &claims.job_key,
            CodeOwnerOperation::Read,
            now,
        )
        .unwrap();
    read.operation = CodeOwnerOperation::SealNoEffect;
    let signed = sign(
        &key,
        &read,
        READ_DOMAIN,
        "elitea.sandbox.node-code-recovery-signed-grant.v1",
    );
    let seal = verifier
        .verify_code_recovery(
            &signed,
            "dns:main.fixture",
            &claims.job_key,
            CodeOwnerOperation::SealNoEffect,
            now,
        )
        .unwrap();
    Fixture {
        job,
        scope,
        intent,
        read: read_authority,
        seal,
    }
}
#[test]
fn original_code_intent_requires_original_execute_binding_and_signature_domain() {
    let (key, verifier, job, scope, claims) = claims("1".repeat(32));
    let envelope = sign(
        &key,
        &claims,
        INTENT_DOMAIN,
        "elitea.sandbox.original-code-intent-signed.v1",
    );
    let wire = serde_json::to_vec(&envelope).unwrap();
    assert!(
        verifier
            .verify_code_intent(
                &wire,
                "dns:worker.fixture",
                &scope,
                &claims.execution_id,
                1,
                &claims.dispatch_activation,
                &job,
                1001
            )
            .is_ok()
    );
    for case in 0..7 {
        let mut changed = claims.clone();
        match case {
            0 => changed.purpose = "preparation".into(),
            1 => changed.source_sha256 = "b".repeat(64),
            2 => changed.input_sha256 = "b".repeat(64),
            3 => changed.original_generation = 2,
            4 => changed.prepared_job_sha256 = "b".repeat(64),
            5 => changed.node_digest = "0".repeat(64),
            _ => changed.expires_at_unix_millis = 1000,
        }
        let wire =
            serde_json::to_vec(&sign(&key, &changed, INTENT_DOMAIN, &envelope.schema)).unwrap();
        assert!(
            verifier
                .verify_code_intent(
                    &wire,
                    "dns:worker.fixture",
                    &scope,
                    &claims.execution_id,
                    1,
                    &claims.dispatch_activation,
                    &job,
                    1001
                )
                .is_err(),
            "case{case}"
        );
    }
    let wrong = serde_json::to_vec(&sign(&key, &claims, READ_DOMAIN, &envelope.schema)).unwrap();
    assert!(
        verifier
            .verify_code_intent(
                &wrong,
                "dns:worker.fixture",
                &scope,
                &claims.execution_id,
                1,
                &claims.dispatch_activation,
                &job,
                1001
            )
            .is_err()
    );
    assert!(
        verifier
            .verify_code_intent(
                &wire,
                "dns:other.fixture",
                &scope,
                &claims.execution_id,
                1,
                &claims.dispatch_activation,
                &job,
                1001
            )
            .is_err()
    );
}

#[test]
#[allow(
    clippy::too_many_lines,
    reason = "Keep the complete identity and failure assertions in one fixture."
)]
fn owner_read_grant_binds_configured_main_peer_and_original_visit_without_execute_authority() {
    let (key, verifier, job, scope, original) = claims("1".repeat(32));
    let signed = sign(
        &key,
        &original,
        INTENT_DOMAIN,
        "elitea.sandbox.original-code-intent-signed.v1",
    );
    let intent = verifier
        .verify_code_intent(
            &serde_json::to_vec(&signed).unwrap(),
            "dns:worker.fixture",
            &scope,
            &original.execution_id,
            1,
            &original.dispatch_activation,
            &job,
            1001,
        )
        .unwrap();
    let binding = intent.binding(&scope, 1001).unwrap();
    let claims = CodeRecoveryGrantClaims {
        schema: "elitea.sandbox.node-code-recovery-grant.v1".into(),
        tenant_id: "code-owner-fixture".into(),
        project_id: 1,
        execution_id: original.execution_id.clone(),
        original_generation: 1,
        claim_id: "7".repeat(32),
        claim_attempt: 2,
        lease_epoch: 2,
        fence_sha256: "8".repeat(64),
        activation_id: original.activation_id.clone(),
        node_id: original.node_id.clone(),
        graph_thread: original.graph_thread.clone(),
        step: original.step,
        attempt: original.attempt,
        expected_revision: 2,
        receipt_sha256: "9".repeat(64),
        dispatch_activation: original.dispatch_activation.clone(),
        job_key: original.job_key.clone(),
        request_digest: original.request_digest.clone(),
        binding_sha256: sha256(&binding.canonical_bytes().unwrap()),
        supervisor_audience: original.supervisor_audience.clone(),
        requester_workload_identity: "dns:main.fixture".into(),
        operation: CodeOwnerOperation::Read,
        issued_at_unix_millis: 1000,
        expires_at_unix_millis: 31_000,
    };
    let signed = sign(
        &key,
        &claims,
        READ_DOMAIN,
        "elitea.sandbox.node-code-recovery-signed-grant.v1",
    );
    let authority = verifier
        .verify_code_recovery(
            &signed,
            "dns:main.fixture",
            &claims.job_key,
            CodeOwnerOperation::Read,
            1001,
        )
        .unwrap();
    assert!(authority.permits(binding, 1001));
    let mut altered = binding.clone();
    altered.activation_id = "a".repeat(64);
    assert!(!authority.permits(&altered, 1001));
    assert!(
        verifier
            .verify_code_recovery(
                &signed,
                "dns:worker.fixture",
                &claims.job_key,
                CodeOwnerOperation::Read,
                1001
            )
            .is_err()
    );
    assert!(
        verifier
            .verify_code_recovery(
                &signed,
                "dns:main.fixture",
                &claims.job_key,
                CodeOwnerOperation::SealNoEffect,
                1001
            )
            .is_err()
    );
    let unconfigured = GrantVerifier::new(
        Keys(key.public_key().as_ref().try_into().unwrap()),
        "dns:supervisor.fixture".into(),
    )
    .unwrap();
    assert!(
        unconfigured
            .verify_code_recovery(
                &signed,
                "dns:main.fixture",
                &claims.job_key,
                CodeOwnerOperation::Read,
                1001
            )
            .is_err()
    );
    let wrong_domain = sign(&key, &claims, INTENT_DOMAIN, &signed.schema);
    assert!(
        verifier
            .verify_code_recovery(
                &wrong_domain,
                "dns:main.fixture",
                &claims.job_key,
                CodeOwnerOperation::Read,
                1001
            )
            .is_err()
    );
    assert!(
        verifier
            .verify_code_intent(
                &serde_json::to_vec(&signed).unwrap(),
                "dns:worker.fixture",
                &scope,
                &original.execution_id,
                1,
                &original.dispatch_activation,
                &job,
                1001
            )
            .is_err()
    );
}
