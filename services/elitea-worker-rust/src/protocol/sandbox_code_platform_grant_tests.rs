use super::super::tests::{Keys, claims, sign};
use super::*;

fn fixture() -> (
    signature::Ed25519KeyPair,
    GrantVerifier<Keys>,
    WholeCodeBinding,
    CodePlatformBinding,
    CodePlatformOwnerGrantClaims,
) {
    let (key, verifier, job, scope, intent) = claims("1".repeat(32));
    let signed = sign(
        &key,
        &intent,
        INTENT_DOMAIN,
        "elitea.sandbox.original-code-intent-signed.v1",
    );
    let sealed = verifier
        .verify_code_intent(
            &serde_json::to_vec(&signed).unwrap(),
            "dns:worker.fixture",
            &scope,
            &intent.execution_id,
            1,
            &intent.dispatch_activation,
            &job,
            1001,
        )
        .unwrap();
    let whole = sealed.binding(&scope, 1001).unwrap().clone();
    let broker = CodePlatformBinding {
        schema: "elitea.sandbox.code-platform-binding.v1".into(),
        prepared_job_sha256: whole.prepared_job_sha256.clone(),
        prepared_fingerprint: whole.request_digest.clone(),
        // The owning policy syntax admits zero; equality to the original owner record still binds it.
        policy_sha256: "0".repeat(64),
        max_calls: 8,
        max_total_bytes: 4096,
        compiled_execute: None,
    };
    let claims = CodePlatformOwnerGrantClaims {
        schema: "elitea.sandbox.code-platform-owner-grant.v1".into(),
        purpose: "platform_broker_runtime".into(),
        tenant_id: "code-owner-fixture".into(),
        project_id: 1,
        execution_id: whole.execution_id.clone(),
        original_generation: 1,
        claim_id: "7".repeat(32),
        claim_attempt: 2,
        lease_epoch: 2,
        fence_sha256: "8".repeat(64),
        activation_id: whole.activation_id.clone(),
        attempt: whole.attempt,
        dispatch_activation: whole.dispatch_activation.clone(),
        job_key: whole.job_key.clone(),
        request_digest: whole.request_digest.clone(),
        binding_sha256: sha256(&whole.canonical_bytes().unwrap()),
        prepared_job_sha256: broker.prepared_job_sha256.clone(),
        prepared_fingerprint: broker.prepared_fingerprint.clone(),
        compiled_execute: None,
        policy_sha256: broker.policy_sha256.clone(),
        max_calls: 8,
        max_total_bytes: 4096,
        supervisor_audience: "dns:supervisor.fixture".into(),
        requester_workload_identity: "dns:main.fixture".into(),
        operation: CodePlatformOwnerOperation::ReadRetainedRuntime,
        sequence: None,
        platform_request_sha256: None,
        committed_reply_sha256: None,
        issued_at_unix_millis: 1000,
        expires_at_unix_millis: 31_000,
    };
    (key, verifier, whole, broker, claims)
}

fn envelope(
    key: &signature::Ed25519KeyPair,
    claims: &CodePlatformOwnerGrantClaims,
) -> SignedCodeEnvelope {
    sign(
        key,
        claims,
        DOMAIN,
        "elitea.sandbox.code-platform-owner-signed-grant.v1",
    )
}

#[test]
#[allow(
    clippy::too_many_lines,
    reason = "Keep the complete identity and failure assertions in one fixture."
)]
fn compiled_platform_owner_uses_exact_original_execute_selector_and_final_prepared_bytes() {
    use crate::sandbox::{
        compiled_snapshot::{
            Binding, ContentSha256, Descriptor, Purpose, SelectedSnapshot, SnapshotProfile,
        },
        platform_client_binding::PlatformClientBinding,
        request::Language,
    };
    let (key, verifier, _, _, mut intent) = claims("1".repeat(32));
    let source = "pub fn main() {}";
    let job = PreparedJob::new(
        Language::Rust,
        source.into(),
        std::collections::BTreeMap::new(),
        format!("sha256:{}", "a".repeat(64)),
        "cargo-broker-execute-v1".into(),
        30,
    )
    .unwrap()
    .with_platform_client(PlatformClientBinding::new("0".repeat(64), 8, 4096).unwrap())
    .unwrap();
    let h = "b".repeat(64);
    let template: Binding = serde_json::from_value(serde_json::json!({
        "revision":1,"reuse_policy":"snapshot_v1","tenant_id":"code-owner-fixture","project_id":1,
        "base_prepared_request_sha256":h,"source_sha256":h,
        "compilation_image_digest":format!("sha256:{}", "a".repeat(64)),"execution_image_digest":format!("sha256:{}", "a".repeat(64)),
        "platform":"linux/arm64/gnu","target":"aarch64-unknown-linux-gnu","policy_revision":"cargo-broker-execute-v1",
        "cargo_manifest_sha256":h,"cargo_lock_sha256":h,"cargo_config_sha256":h,"vendor_sha256":h,"toolchain_sha256":h,"adapter_sha256":h,"wrapper_sha256":h,"compiler_flags_sha256":h,
    })).unwrap();
    let binding = SnapshotProfile::new(template)
        .unwrap()
        .binding(&job, "code-owner-fixture", 1)
        .unwrap();
    let descriptor = Descriptor {
        revision: 1,
        snapshot_key_sha256: binding.key().unwrap(),
        binding: binding.clone(),
        executable_sha256: ContentSha256::of(b"original executable"),
        executable_bytes: 19,
    };
    let selected = SelectedSnapshot::select(binding, descriptor.bytes().unwrap()).unwrap();
    let request = selected.control.intent_digest(Purpose::Execute).unwrap();
    assert_ne!(request, job.fingerprint().unwrap());
    intent.request_digest = hex(&request);
    intent.language = "rust".into();
    intent.prepared_job_sha256 = sha256(&job.to_transport().unwrap());
    intent.source_sha256 = sha256(source.as_bytes());
    intent.input_sha256 = sha256(b"{}");
    let scope = JobScope::new(
        intent.tenant_id.clone(),
        1,
        decode32(&intent.job_key).unwrap(),
        request,
    )
    .unwrap();
    let signed = sign(
        &key,
        &intent,
        INTENT_DOMAIN,
        "elitea.sandbox.original-code-intent-signed.v1",
    );
    let authority = verifier
        .verify_code_intent(
            &serde_json::to_vec(&signed).unwrap(),
            "dns:worker.fixture",
            &scope,
            &intent.execution_id,
            1,
            &intent.dispatch_activation,
            &job,
            1001,
        )
        .unwrap();
    let whole = authority.binding(&scope, 1001).unwrap();
    assert!(CodePlatformBinding::from_job(&job, whole).is_err());
    let broker = CodePlatformBinding::from_compiled_job(
        &job,
        whole,
        &selected.control,
        &selected.descriptor_bytes,
    )
    .unwrap()
    .unwrap();
    assert!(broker.matches(whole));
    let launch: serde_json::Value =
        serde_json::from_slice(&broker.launch("original-cid").unwrap()).unwrap();
    assert_eq!(launch["revision"], 2);
    assert_eq!(launch["request_digest"], whole.request_digest);
    assert_eq!(launch["prepared_sha256"], hex(&job.fingerprint().unwrap()));
    assert_ne!(launch["request_digest"], launch["prepared_sha256"]);
    assert_eq!(
        broker.prepared_fingerprint,
        hex(&job.fingerprint().unwrap())
    );
    assert_eq!(
        broker.prepared_job_sha256,
        sha256(&job.to_transport().unwrap())
    );
    let (_, _, _, _, mut read) = fixture();
    read.request_digest = whole.request_digest.clone();
    read.binding_sha256 = sha256(&whole.canonical_bytes().unwrap());
    read.prepared_job_sha256 = broker.prepared_job_sha256.clone();
    read.prepared_fingerprint = broker.prepared_fingerprint.clone();
    read.compiled_execute = broker.compiled_execute.clone();
    let owner = verifier
        .verify_code_platform_owner(
            &envelope(&key, &read),
            "dns:main.fixture",
            &whole.job_key,
            CodePlatformOwnerOperation::ReadRetainedRuntime,
            1001,
        )
        .unwrap();
    assert!(owner.permits(whole, &broker, 1001));
    assert!(owner.permits_not_ready(Some(whole), Some(&broker), 1001));
    assert!(owner.permits_completed(whole, &broker, 1001));
    let mut wrong = read.clone();
    wrong
        .compiled_execute
        .as_mut()
        .unwrap()
        .selected_descriptor_sha256 = ContentSha256::of(b"another descriptor");
    assert!(
        verifier
            .verify_code_platform_owner(
                &envelope(&key, &wrong),
                "dns:main.fixture",
                &whole.job_key,
                CodePlatformOwnerOperation::ReadRetainedRuntime,
                1001
            )
            .is_err()
    );
    let mut wrong_base = read.clone();
    wrong_base.prepared_fingerprint = "a".repeat(64);
    assert!(
        verifier
            .verify_code_platform_owner(
                &envelope(&key, &wrong_base),
                "dns:main.fixture",
                &whole.job_key,
                CodePlatformOwnerOperation::ReadRetainedRuntime,
                1001
            )
            .is_err()
    );
    let mut changed_descriptor = selected.descriptor.clone();
    changed_descriptor.executable_bytes += 1;
    assert!(
        CodePlatformBinding::from_compiled_job(
            &job,
            whole,
            &selected.control,
            &changed_descriptor.bytes().unwrap()
        )
        .is_err()
    );
    let mut wire = serde_json::to_value(&broker).unwrap();
    wire.as_object_mut().unwrap().remove("compiled_execute");
    assert!(serde_json::from_value::<CodePlatformBinding>(wire).is_err());
    let mut wire = serde_json::to_value(&read).unwrap();
    wire.as_object_mut().unwrap().remove("compiled_execute");
    assert!(serde_json::from_value::<CodePlatformOwnerGrantClaims>(wire).is_err());
}

#[test]
#[allow(
    clippy::too_many_lines,
    reason = "Keep the complete identity and failure assertions in one fixture."
)]
fn platform_owner_read_requires_original_visit_and_distinct_main_purpose() {
    let (key, verifier, whole, broker, claims) = fixture();
    let signed = envelope(&key, &claims);
    let authority = verifier
        .verify_code_platform_owner(
            &signed,
            "dns:main.fixture",
            &whole.job_key,
            CodePlatformOwnerOperation::ReadRetainedRuntime,
            1001,
        )
        .unwrap();
    assert!(authority.permits(&whole, &broker, 1001));
    let original_plain = crate::sandbox::code_recovery::canonical(
        &serde_json::json!({"revision":1,"retained_runtime_id":"original-cid",
        "prepared_sha256":broker.prepared_fingerprint,"policy_sha256":broker.policy_sha256,
        "max_calls":broker.max_calls,"max_total_bytes":broker.max_total_bytes}),
    )
    .unwrap();
    assert_eq!(broker.launch("original-cid").unwrap(), original_plain);
    let launch: serde_json::Value = serde_json::from_slice(&original_plain).unwrap();
    assert!(launch.get("request_digest").is_none());
    assert!(authority.accepts_reply(None));
    assert!(!authority.accepts_reply(Some(b"arbitrary reply")));
    for changed in [
        CodePlatformOwnerGrantClaims {
            activation_id: "a".repeat(64),
            ..claims.clone()
        },
        CodePlatformOwnerGrantClaims {
            attempt: 2,
            ..claims.clone()
        },
        CodePlatformOwnerGrantClaims {
            binding_sha256: "a".repeat(64),
            ..claims.clone()
        },
        CodePlatformOwnerGrantClaims {
            prepared_job_sha256: "a".repeat(64),
            ..claims.clone()
        },
        CodePlatformOwnerGrantClaims {
            policy_sha256: "a".repeat(64),
            ..claims.clone()
        },
    ] {
        let changed_authority = verifier
            .verify_code_platform_owner(
                &envelope(&key, &changed),
                "dns:main.fixture",
                &whole.job_key,
                CodePlatformOwnerOperation::ReadRetainedRuntime,
                1001,
            )
            .unwrap();
        assert!(!changed_authority.permits(&whole, &broker, 1001));
    }
    assert!(
        verifier
            .verify_code_platform_owner(
                &signed,
                "dns:worker.fixture",
                &whole.job_key,
                CodePlatformOwnerOperation::ReadRetainedRuntime,
                1001
            )
            .is_err()
    );
    assert!(
        verifier
            .verify_code_platform_owner(
                &signed,
                "dns:main.fixture",
                &whole.job_key,
                CodePlatformOwnerOperation::ReadPendingPlatformCall,
                1001
            )
            .is_err()
    );
    assert!(
        verifier
            .verify_code_platform_owner(
                &signed,
                "dns:main.fixture",
                &whole.job_key,
                CodePlatformOwnerOperation::ReadRetainedRuntime,
                31_000
            )
            .is_err()
    );
    let wrong_domain = sign(&key, &claims, READ_DOMAIN, &signed.schema);
    assert!(
        verifier
            .verify_code_platform_owner(
                &wrong_domain,
                "dns:main.fixture",
                &whole.job_key,
                CodePlatformOwnerOperation::ReadRetainedRuntime,
                1001
            )
            .is_err()
    );
    assert!(
        verifier
            .verify_code_recovery(
                &signed,
                "dns:main.fixture",
                &whole.job_key,
                CodeOwnerOperation::Read,
                1001
            )
            .is_err()
    );
}

#[test]
fn platform_publish_requires_exact_committed_reply_and_closed_selectors() {
    let (key, verifier, whole, broker, mut claims) = fixture();
    let reply = b"exact Main committed reply fixture";
    claims.operation = CodePlatformOwnerOperation::PublishCommittedPlatformReply;
    claims.sequence = Some(3);
    claims.platform_request_sha256 = Some("b".repeat(64));
    claims.committed_reply_sha256 = Some(sha256(reply));
    let authority = verifier
        .verify_code_platform_owner(
            &envelope(&key, &claims),
            "dns:main.fixture",
            &whole.job_key,
            claims.operation,
            1001,
        )
        .unwrap();
    assert!(authority.permits(&whole, &broker, 1001));
    assert!(authority.accepts_reply(Some(reply)));
    assert!(!authority.accepts_reply(Some(b"different reply")));
    assert!(!authority.accepts_reply(None));
    for changed in [
        CodePlatformOwnerGrantClaims {
            sequence: None,
            ..claims.clone()
        },
        CodePlatformOwnerGrantClaims {
            sequence: Some(9),
            ..claims.clone()
        },
        CodePlatformOwnerGrantClaims {
            committed_reply_sha256: None,
            ..claims.clone()
        },
        CodePlatformOwnerGrantClaims {
            platform_request_sha256: None,
            ..claims.clone()
        },
        CodePlatformOwnerGrantClaims {
            issued_at_unix_millis: 0,
            ..claims.clone()
        },
    ] {
        assert!(
            verifier
                .verify_code_platform_owner(
                    &envelope(&key, &changed),
                    "dns:main.fixture",
                    &whole.job_key,
                    claims.operation,
                    1001
                )
                .is_err()
        );
    }
    let mut value = serde_json::to_value(&claims).unwrap();
    value
        .as_object_mut()
        .unwrap()
        .insert("runtime_id".into(), serde_json::json!("caller-selected"));
    assert!(serde_json::from_value::<CodePlatformOwnerGrantClaims>(value).is_err());
    let mut value = serde_json::to_value(&claims).unwrap();
    value.as_object_mut().unwrap().remove("sequence");
    assert!(serde_json::from_value::<CodePlatformOwnerGrantClaims>(value).is_err());
}

#[test]
fn not_ready_requires_current_read_authority_and_matching_partial_bindings() {
    let (key, verifier, whole, broker, claims) = fixture();
    let owner = verifier
        .verify_code_platform_owner(
            &envelope(&key, &claims),
            "dns:main.fixture",
            &whole.job_key,
            CodePlatformOwnerOperation::ReadRetainedRuntime,
            1001,
        )
        .unwrap();
    for (binding, retained) in [
        (None, None),
        (Some(&whole), None),
        (None, Some(&broker)),
        (Some(&whole), Some(&broker)),
    ] {
        assert!(owner.permits_not_ready(binding, retained, 1001));
    }
    let mut changed = whole.clone();
    changed.source_sha256 = "a".repeat(64);
    assert!(!owner.permits_not_ready(Some(&changed), None, 1001));
    let mut changed_broker = broker.clone();
    changed_broker.policy_sha256 = "a".repeat(64);
    assert!(!owner.permits_not_ready(None, Some(&changed_broker), 1001));
    assert!(!owner.permits_not_ready(None, None, claims.expires_at_unix_millis));
}
#[test]
fn not_ready_never_authorizes_mailbox_read_or_reply_publication() {
    let (key, verifier, whole, broker, mut claims) = fixture();
    for operation in [
        CodePlatformOwnerOperation::ReadPendingPlatformCall,
        CodePlatformOwnerOperation::PublishCommittedPlatformReply,
    ] {
        claims.operation = operation;
        if operation == CodePlatformOwnerOperation::PublishCommittedPlatformReply {
            claims.sequence = Some(1);
            claims.platform_request_sha256 = Some("a".repeat(64));
            claims.committed_reply_sha256 = Some("b".repeat(64));
        }
        let owner = verifier
            .verify_code_platform_owner(
                &envelope(&key, &claims),
                "dns:main.fixture",
                &whole.job_key,
                operation,
                1001,
            )
            .unwrap();
        assert!(!owner.permits_not_ready(None, None, 1001));
        assert!(!owner.permits_not_ready(Some(&whole), Some(&broker), 1001));
    }
}
#[test]
fn wrong_peer_operation_route_and_expired_grants_cannot_observe_absence() {
    let (key, verifier, whole, _, claims) = fixture();
    let signed = envelope(&key, &claims);
    for (peer, job, operation, now) in [
        (
            "dns:worker.fixture",
            whole.job_key.clone(),
            CodePlatformOwnerOperation::ReadRetainedRuntime,
            1001,
        ),
        (
            "dns:main.fixture",
            "f".repeat(64),
            CodePlatformOwnerOperation::ReadRetainedRuntime,
            1001,
        ),
        (
            "dns:main.fixture",
            whole.job_key.clone(),
            CodePlatformOwnerOperation::ReadPendingPlatformCall,
            1001,
        ),
        (
            "dns:main.fixture",
            whole.job_key.clone(),
            CodePlatformOwnerOperation::ReadRetainedRuntime,
            claims.expires_at_unix_millis,
        ),
    ] {
        assert!(
            verifier
                .verify_code_platform_owner(&signed, peer, &job, operation, now)
                .is_err()
        );
    }
}

#[test]
fn completed_observation_requires_exact_current_read_grant_and_full_bindings() {
    let (key, verifier, whole, broker, mut claims) = fixture();
    for operation in [
        CodePlatformOwnerOperation::ReadRetainedRuntime,
        CodePlatformOwnerOperation::ReadPendingPlatformCall,
    ] {
        claims.operation = operation;
        let owner = verifier
            .verify_code_platform_owner(
                &envelope(&key, &claims),
                "dns:main.fixture",
                &whole.job_key,
                operation,
                1001,
            )
            .unwrap();
        assert!(owner.permits_completed(&whole, &broker, 1001));
        assert!(!owner.permits_completed(&whole, &broker, claims.expires_at_unix_millis));
        assert!(owner.accepts_reply(None));
        assert!(!owner.accepts_reply(Some(b"reply")));
        let mut changed = whole.clone();
        changed.graph_thread = "another thread".into();
        assert!(!owner.permits_completed(&changed, &broker, 1001));
        let mut changed = broker.clone();
        changed.max_calls += 1;
        assert!(!owner.permits_completed(&whole, &changed, 1001));
        let mut changed = broker.clone();
        changed.prepared_fingerprint = "a".repeat(64);
        assert!(!owner.permits_completed(&whole, &changed, 1001));
        // NotReady stays restricted to admission reads.
        assert_eq!(
            owner.permits_not_ready(None, None, 1001),
            operation == CodePlatformOwnerOperation::ReadRetainedRuntime
        );
    }
}
#[test]
fn completion_never_weakens_publication_or_peer_route_and_expiry_fences() {
    let (key, verifier, whole, broker, mut claims) = fixture();
    claims.operation = CodePlatformOwnerOperation::PublishCommittedPlatformReply;
    claims.sequence = Some(1);
    claims.platform_request_sha256 = Some("a".repeat(64));
    claims.committed_reply_sha256 = Some(sha256(b"exact committed reply"));
    let owner = verifier
        .verify_code_platform_owner(
            &envelope(&key, &claims),
            "dns:main.fixture",
            &whole.job_key,
            claims.operation,
            1001,
        )
        .unwrap();
    assert!(owner.permits(&whole, &broker, 1001));
    assert!(!owner.permits_completed(&whole, &broker, 1001));
    assert!(!owner.accepts_reply(None));
    for (peer, job, operation, now) in [
        (
            "dns:worker.fixture",
            whole.job_key.clone(),
            claims.operation,
            1001,
        ),
        ("dns:main.fixture", "f".repeat(64), claims.operation, 1001),
        (
            "dns:main.fixture",
            whole.job_key.clone(),
            CodePlatformOwnerOperation::ReadRetainedRuntime,
            1001,
        ),
        (
            "dns:main.fixture",
            whole.job_key.clone(),
            claims.operation,
            claims.expires_at_unix_millis,
        ),
    ] {
        assert!(
            verifier
                .verify_code_platform_owner(&envelope(&key, &claims), peer, &job, operation, now)
                .is_err()
        );
    }
}
