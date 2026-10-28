use std::path::PathBuf;

use prost::Message as _;
use serde_json::Value;

use super::{
    SCHEMA_REVISION_V1, SecurityContextError, SecurityContextReference, content_id, verify,
};
use crate::protocol::ProtocolError;
use crate::protocol::elitea::runtime::v1::{
    DigestAlgorithmV1, DigestV1, ExecutionSecurityContextReferenceV1, WorkerCommandV1,
};

fn vectors() -> Vec<Value> {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../testdata/proto/runtime/v1/security-context/vectors.json");
    let corpus: Value =
        serde_json::from_slice(&std::fs::read(path).expect("security context corpus"))
            .expect("corpus JSON");
    assert_eq!(corpus["schema_revision"], SCHEMA_REVISION_V1);
    corpus["vectors"].as_array().expect("vectors").clone()
}

fn hex(value: &Value) -> Vec<u8> {
    let text = value.as_str().expect("hex string");
    (0..text.len())
        .step_by(2)
        .map(|index| u8::from_str_radix(&text[index..index + 2], 16).expect("hex"))
        .collect()
}

fn command(execution_id: &str, reference: ExecutionSecurityContextReferenceV1) -> WorkerCommandV1 {
    WorkerCommandV1 {
        execution_id: execution_id.to_owned(),
        security_context_ref: Some(reference),
        ..WorkerCommandV1::default()
    }
}

fn reference_proto(content: &str, digest: Vec<u8>) -> ExecutionSecurityContextReferenceV1 {
    ExecutionSecurityContextReferenceV1 {
        content_entry_id: content.to_owned(),
        digest: Some(DigestV1 {
            algorithm: DigestAlgorithmV1::Sha256 as i32,
            value: digest,
        }),
        schema_revision: SCHEMA_REVISION_V1.to_owned(),
    }
}

#[test]
fn the_shared_corpus_verifies_like_main() {
    let vectors = vectors();
    assert_eq!(vectors.len(), 4);
    for vector in vectors {
        let entry_id = vector["content_id"].as_str().expect("content id");
        let execution_id = entry_id
            .strip_prefix(super::CONTENT_ID_PREFIX)
            .expect("context content id");
        let reference = SecurityContextReference::from_command(&command(
            execution_id,
            reference_proto(entry_id, hex(&vector["reference_digest_hex"])),
        ))
        .expect("reference")
        .expect("governed command");
        let verified = verify(&hex(&vector["context_hex"]), &reference, execution_id);
        match vector["expect"].as_str().expect("expect") {
            "accept" => {
                let context = verified.expect("accepted vector");
                assert_eq!(
                    context.message().agent_binding_epoch,
                    vector["agent_binding_epoch"].as_i64().expect("epoch")
                );
                assert_eq!(
                    context.message().policy_revision_id,
                    vector["policy_revision_id"].as_str().expect("policy")
                );
                assert_eq!(context.content_id(), entry_id);
                // Debug never shows a context field.
                let debug = format!("{context:?}");
                assert!(!debug.contains("2026-10-r1"), "{debug}");
            }
            "digest_mismatch" => {
                assert_eq!(verified.err(), Some(SecurityContextError::DigestMismatch));
            }
            "invalid" => assert_eq!(verified.err(), Some(SecurityContextError::Invalid)),
            other => panic!("unknown expectation {other}"),
        }
    }
}

#[test]
fn a_context_served_for_another_execution_is_refused() {
    let accepted = vectors().remove(0);
    let content = accepted["content_id"].as_str().expect("content id");
    let reference = SecurityContextReference::from_command(&command(
        "execution-1",
        reference_proto(content, hex(&accepted["reference_digest_hex"])),
    ))
    .expect("reference")
    .expect("governed");
    assert_eq!(
        verify(&hex(&accepted["context_hex"]), &reference, "execution-2").err(),
        Some(SecurityContextError::ContentMismatch)
    );
    assert_eq!(
        verify(&[], &reference, "execution-1").err(),
        Some(SecurityContextError::Size)
    );
    assert_eq!(
        verify(&vec![0; super::MAX_BYTES + 1], &reference, "execution-1").err(),
        Some(SecurityContextError::Size)
    );
}

#[test]
fn the_reference_must_name_this_executions_context() {
    let digest = vec![7; 32];
    assert_eq!(
        SecurityContextReference::from_command(&WorkerCommandV1::default()).expect("legacy"),
        None
    );
    let reference = SecurityContextReference::from_command(&command(
        "execution-1",
        reference_proto(&content_id("execution-1"), digest.clone()),
    ))
    .expect("valid")
    .expect("governed");
    assert_eq!(
        reference.immutable_version(),
        format!("sha256:{}", "07".repeat(32))
    );
    let mut wrong_revision = reference_proto(&content_id("execution-1"), digest.clone());
    wrong_revision.schema_revision = "elitea.runtime.security-context.v2".to_owned();
    let mut md5 = reference_proto(&content_id("execution-1"), digest.clone());
    md5.digest = Some(DigestV1 {
        algorithm: DigestAlgorithmV1::Unspecified as i32,
        value: digest.clone(),
    });
    for (execution, malformed) in [
        (
            "execution-1",
            reference_proto(&content_id("execution-2"), digest.clone()),
        ),
        (
            "execution-1",
            reference_proto("execution-1", digest.clone()),
        ),
        (
            "execution-1",
            reference_proto(&content_id("execution-1"), vec![7; 31]),
        ),
        ("execution-1", wrong_revision),
        ("execution-1", md5),
        ("", reference_proto(&content_id(""), digest.clone())),
    ] {
        assert!(matches!(
            SecurityContextReference::from_command(&command(execution, malformed)),
            Err(ProtocolError::InvalidInput(_))
        ));
    }
}

#[test]
fn an_unknown_field_in_the_reference_is_refused_by_the_scan() {
    let mut raw = reference_proto(&content_id("execution-1"), vec![7; 32]).encode_to_vec();
    assert!(super::scan_reference(&raw).is_ok());
    raw.extend_from_slice(&[0x22, 0x00]);
    assert!(matches!(
        super::scan_reference(&raw),
        Err(ProtocolError::IncompatibleVersion(_))
    ));
    // An unknown field inside the nested digest is refused too.
    let mut digest = DigestV1 {
        algorithm: DigestAlgorithmV1::Sha256 as i32,
        value: vec![7; 32],
    }
    .encode_to_vec();
    digest.extend_from_slice(&[0x1a, 0x00]);
    let mut nested = vec![0x12, u8::try_from(digest.len()).expect("short")];
    nested.extend_from_slice(&digest);
    assert!(matches!(
        super::scan_reference(&nested),
        Err(ProtocolError::IncompatibleVersion(_))
    ));
}
