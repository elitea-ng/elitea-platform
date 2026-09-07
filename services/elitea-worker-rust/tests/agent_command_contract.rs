use std::collections::BTreeMap;
use std::sync::atomic::{AtomicUsize, Ordering};

use elitea_worker_rust::agents::AgentExecutionKind;
use elitea_worker_rust::protocol::ProtocolError;
use elitea_worker_rust::protocol::command::{
    Ed25519CommandAuthenticator, Ed25519PublicKeyResolver, SignedCommandAuthenticator,
    TestOnlyConformanceHmacAuthenticator, parse_and_verify_agent_command,
};
use elitea_worker_rust::protocol::elitea::runtime::v1::{
    DigestAlgorithmV1, SignatureProfileV1, SignedWorkerCommandEnvelopeV1,
    ToolkitCallToolCommandV1, WorkerCommandTypeV1, WorkerCommandV1, worker_command_v1,
};
use prost::Message;
use ring::signature::KeyPair;
use ring::{digest, hmac, signature};

const CONFORMANCE_KEY: &[u8] = b"ELITEA_RUNTIME_V1_TEST_ONLY_NOT_A_SECRET";

fn decode_hex(value: &str) -> Vec<u8> {
    let value = value.trim();
    assert_eq!(value.len() % 2, 0);
    value
        .as_bytes()
        .chunks_exact(2)
        .map(|pair| {
            u8::from_str_radix(std::str::from_utf8(pair).unwrap(), 16).expect("fixture hex")
        })
        .collect()
}

fn vectors() -> BTreeMap<&'static str, Vec<u8>> {
    include_str!("fixtures/agent_command_vectors.txt")
        .lines()
        .map(|line| {
            let (name, value) = line.split_once('=').expect("named fixture");
            (name, decode_hex(value))
        })
        .collect()
}

fn fixture(name: &str) -> Vec<u8> {
    vectors().remove(name).expect("fixture exists")
}

#[test]
fn python_hmac_fixtures_select_both_exact_agent_semantics() {
    let authenticator = TestOnlyConformanceHmacAuthenticator;
    for (name, expected_kind, expected_type, expected_route) in [
        (
            "application_hmac",
            AgentExecutionKind::Application,
            WorkerCommandTypeV1::AgentExecuteApplication,
            "chat_predict",
        ),
        (
            "adhoc_hmac",
            AgentExecutionKind::Adhoc,
            WorkerCommandTypeV1::AgentExecuteAdhoc,
            "chat_continue_predict",
        ),
    ] {
        let verified = parse_and_verify_agent_command(
            &fixture(name),
            Some(&authenticator as &dyn SignedCommandAuthenticator),
        )
        .expect("Python signed agent command");

        assert_eq!(verified.kind(), expected_kind);
        assert_eq!(verified.command().command_type, expected_type as i32);
        assert_eq!(verified.command().execution_id, "execution-1");
        assert_eq!(verified.command().generation, 2);
        assert_eq!(verified.command().dispatch_ordinal, 3);
        let Some(worker_command_v1::CapabilityCommand::AgentExecution(agent)) =
            &verified.command().capability_command
        else {
            panic!("agent command oneof");
        };
        assert_eq!(agent.request_entry_id, "agent-request");
        assert_eq!(agent.sio_event, expected_route);
    }
}

struct StaticResolver {
    key_id: &'static str,
    public_key: [u8; 32],
}

impl Ed25519PublicKeyResolver for StaticResolver {
    fn resolve_ed25519_public_key(&self, key_id: &str) -> Option<[u8; 32]> {
        (key_id == self.key_id).then_some(self.public_key)
    }
}

#[test]
fn python_ed25519_vector_uses_exact_key_domain_and_length_binding() {
    let public_key: [u8; 32] = fixture("ed25519_public").try_into().unwrap();
    let authenticator = Ed25519CommandAuthenticator::new(StaticResolver {
        key_id: "runtime-signing-vector",
        public_key,
    });

    let verified =
        parse_and_verify_agent_command(&fixture("application_ed25519"), Some(&authenticator))
            .expect("Python Ed25519 fixture");

    assert_eq!(verified.kind(), AgentExecutionKind::Application);
    assert_eq!(
        verified.signed().signature_profile,
        SignatureProfileV1::Ed25519 as i32
    );

    let mut wrong_key_id = verified.signed().clone();
    wrong_key_id.key_id = "retired-key".to_owned();
    assert!(matches!(
        authenticator.authenticate(&wrong_key_id),
        Err(ProtocolError::AuthorizationFailed(_))
    ));

    let mut tampered = signed_fixture("application_ed25519");
    tampered.worker_command_bytes[1] ^= 1;
    let tampered_digest = digest::digest(&digest::SHA256, &tampered.worker_command_bytes);
    tampered.worker_command_digest.as_mut().unwrap().value = tampered_digest.as_ref().to_vec();
    assert!(matches!(
        parse_and_verify_agent_command(&tampered.encode_to_vec(), Some(&authenticator)),
        Err(ProtocolError::AuthorizationFailed(message)) if message.contains("signature")
    ));
}

#[test]
fn ed25519_rejects_test_profile_wrong_domain_and_wrong_length_binding() {
    let seed: [u8; 32] = std::array::from_fn(|index| u8::try_from(index).unwrap());
    let key_pair = signature::Ed25519KeyPair::from_seed_unchecked(&seed).unwrap();
    let public_key: [u8; 32] = key_pair.public_key().as_ref().try_into().unwrap();
    let authenticator = Ed25519CommandAuthenticator::new(StaticResolver {
        key_id: "runtime-signing-vector",
        public_key,
    });

    assert!(matches!(
        parse_and_verify_agent_command(&fixture("application_hmac"), Some(&authenticator)),
        Err(ProtocolError::AuthorizationFailed(_))
    ));

    let mut wrong_domain = signed_fixture("application_ed25519");
    wrong_domain.signature = key_pair
        .sign(&wrong_domain.worker_command_bytes)
        .as_ref()
        .to_vec();
    assert!(authenticator.authenticate(&wrong_domain).is_err());

    let mut wrong_length = signed_fixture("application_ed25519");
    let mut signing_input = ED25519_DOMAIN.to_vec();
    let length = u64::try_from(wrong_length.worker_command_bytes.len()).unwrap() + 1;
    signing_input.extend_from_slice(&length.to_be_bytes());
    signing_input.extend_from_slice(&wrong_length.worker_command_bytes);
    wrong_length.signature = key_pair.sign(&signing_input).as_ref().to_vec();
    assert!(authenticator.authenticate(&wrong_length).is_err());
}

struct MapResolver(BTreeMap<String, [u8; 32]>);

impl Ed25519PublicKeyResolver for MapResolver {
    fn resolve_ed25519_public_key(&self, key_id: &str) -> Option<[u8; 32]> {
        self.0.get(key_id).copied()
    }
}

#[test]
fn ed25519_key_rotation_supports_overlap_and_explicit_retirement() {
    let old_seed: [u8; 32] = std::array::from_fn(|index| u8::try_from(index).unwrap());
    let new_seed = [7_u8; 32];
    let old = sign_ed25519(signed_fixture("application_ed25519"), "old", &old_seed);
    let new = sign_ed25519(signed_fixture("application_ed25519"), "new", &new_seed);
    let old_public = signature::Ed25519KeyPair::from_seed_unchecked(&old_seed)
        .unwrap()
        .public_key()
        .as_ref()
        .try_into()
        .unwrap();
    let new_public = signature::Ed25519KeyPair::from_seed_unchecked(&new_seed)
        .unwrap()
        .public_key()
        .as_ref()
        .try_into()
        .unwrap();

    let overlap = Ed25519CommandAuthenticator::new(MapResolver(BTreeMap::from([
        ("old".to_owned(), old_public),
        ("new".to_owned(), new_public),
    ])));
    overlap.authenticate(&old).expect("old overlap key");
    overlap.authenticate(&new).expect("new overlap key");

    let retired = Ed25519CommandAuthenticator::new(MapResolver(BTreeMap::from([(
        "new".to_owned(),
        new_public,
    )])));
    assert!(retired.authenticate(&old).is_err());
    retired.authenticate(&new).expect("new key after cutover");
}

#[test]
fn public_conformance_profile_is_never_an_implicit_production_default() {
    assert!(matches!(
        parse_and_verify_agent_command(&fixture("application_hmac"), None),
        Err(ProtocolError::AuthorizationFailed(message)) if message.contains("no production")
    ));
}

struct CountingAuthenticator(AtomicUsize);

impl SignedCommandAuthenticator for CountingAuthenticator {
    fn authenticate(&self, _signed: &SignedWorkerCommandEnvelopeV1) -> Result<(), ProtocolError> {
        self.0.fetch_add(1, Ordering::SeqCst);
        Ok(())
    }
}

#[test]
fn digest_tamper_fails_before_authenticator_or_command_decode() {
    let mut signed = signed_fixture("application_hmac");
    signed.worker_command_bytes[0] ^= 1;
    let authenticator = CountingAuthenticator(AtomicUsize::new(0));

    assert!(matches!(
        parse_and_verify_agent_command(&signed.encode_to_vec(), Some(&authenticator)),
        Err(ProtocolError::AuthorizationFailed(message)) if message.contains("digest")
    ));
    assert_eq!(authenticator.0.load(Ordering::SeqCst), 0);
}

#[test]
fn signed_envelope_wire_rejects_duplicate_unknown_and_wrong_type_before_authentication() {
    for suffix in [
        &[0x0a, 0x01, b'x'][..],
        &[0x08, 0x01][..],
        &[0xf8, 0x03, 0x00][..],
    ] {
        let mut raw = fixture("application_hmac");
        raw.extend_from_slice(suffix);
        let authenticator = CountingAuthenticator(AtomicUsize::new(0));
        assert!(parse_and_verify_agent_command(&raw, Some(&authenticator)).is_err());
        assert_eq!(authenticator.0.load(Ordering::SeqCst), 0);
    }
}

#[test]
fn signed_envelope_and_inner_command_limits_are_enforced_before_decode() {
    assert!(matches!(
        parse_and_verify_agent_command(
            &vec![0; 48 * 1024 + 1],
            Some(&TestOnlyConformanceHmacAuthenticator)
        ),
        Err(ProtocolError::ResourceExhausted(message)) if message.contains("envelope")
    ));

    let base = signed_fixture("application_hmac");
    let signed = resign_hmac(base, vec![0; 32 * 1024 + 1]);
    assert!(matches!(
        parse_and_verify_agent_command(
            &signed.encode_to_vec(),
            Some(&TestOnlyConformanceHmacAuthenticator)
        ),
        Err(ProtocolError::ResourceExhausted(message)) if message.contains("worker command")
    ));
}

#[test]
fn every_single_bit_signed_fixture_mutation_fails_closed() {
    let original = fixture("application_hmac");
    for index in 0..original.len() {
        let mut mutated = original.clone();
        mutated[index] ^= 1;
        assert!(
            parse_and_verify_agent_command(&mutated, Some(&TestOnlyConformanceHmacAuthenticator))
                .is_err(),
            "single-bit mutation at byte {index} was admitted"
        );
    }
}

#[test]
fn every_resigned_truncated_command_fails_closed() {
    let base = signed_fixture("application_hmac");
    let original = base.worker_command_bytes.clone();
    for end in 0..original.len() {
        let signed = resign_hmac(base.clone(), original[..end].to_vec());
        assert!(
            parse_and_verify_agent_command(
                &signed.encode_to_vec(),
                Some(&TestOnlyConformanceHmacAuthenticator)
            )
            .is_err(),
            "resigned command prefix {end} was admitted"
        );
    }
}

#[test]
fn authenticated_command_wire_rejects_duplicate_unknown_wrong_type_and_oneof() {
    let base = signed_fixture("application_hmac");
    let command = WorkerCommandV1::decode(base.worker_command_bytes.as_slice()).unwrap();
    let Some(worker_command_v1::CapabilityCommand::AgentExecution(agent)) =
        command.capability_command
    else {
        panic!("agent command");
    };
    let agent = agent.encode_to_vec();
    let duplicate_oneof = [
        &[0x9a, 0x02][..],
        encode_varint(agent.len() as u64).as_slice(),
        agent.as_slice(),
    ]
    .concat();

    for suffix in [
        &[0x0a, 0x01, b'x'][..],
        &[0x08, 0x01][..],
        &[0xf8, 0x03, 0x00][..],
        duplicate_oneof.as_slice(),
    ] {
        let mut command = base.worker_command_bytes.clone();
        command.extend_from_slice(suffix);
        let signed = resign_hmac(base.clone(), command);
        assert!(matches!(
            parse_and_verify_agent_command(
                &signed.encode_to_vec(),
                Some(&TestOnlyConformanceHmacAuthenticator)
            ),
            Err(ProtocolError::InvalidInput(_) | ProtocolError::IncompatibleVersion(_))
        ));
    }
}

#[test]
fn capability_type_version_and_oneof_must_select_one_agent_entrypoint() {
    let mut command = command_fixture();
    command.command_type = WorkerCommandTypeV1::AgentExecuteAdhoc as i32;
    assert_unsupported(&command);

    let mut command = command_fixture();
    command.capability_version = "2".to_owned();
    assert_unsupported(&command);

    let mut command = command_fixture();
    command.capability_id = "configuration.validate.v1".to_owned();
    assert_unsupported(&command);
}

// A toolkit tool run belongs to the Python worker. This worker must REFUSE the
// command with a typed error, and it must never treat an unrecognized
// capability as work it silently declined: an unacknowledged command is
// redelivered forever, and a skipped one settles nothing at all. The refusal
// is the settlement.
#[test]
fn toolkit_call_tool_command_is_refused_with_a_typed_unsupported_capability() {
    let mut command = command_fixture();
    command.capability_id = "toolkit.call_tool.v1".to_owned();
    command.capability_version = "1".to_owned();
    command.command_type = WorkerCommandTypeV1::ToolkitCallTool as i32;
    command.capability_command = Some(worker_command_v1::CapabilityCommand::ToolkitCallTool(
        ToolkitCallToolCommandV1 {
            toolkit_type: "github".to_owned(),
            settings_entry_id: "settings".to_owned(),
            tool_name: "get_issue".to_owned(),
            arguments_entry_id: "arguments".to_owned(),
            toolkit_id: "17".to_owned(),
            toolkit_version: "1".to_owned(),
        },
    ));
    assert_unsupported(&command);
}

#[test]
fn command_identity_schedule_reference_and_route_are_bounded() {
    let mut command = command_fixture();
    command.root_execution_id = "different".to_owned();
    assert_invalid(&command, "root identity");

    let mut command = command_fixture();
    command.generation = 0;
    assert_invalid(&command, "scheduling identity");

    let mut command = command_fixture();
    command.input_bundle_ref.as_mut().unwrap().media_type = "application/json".to_owned();
    assert_invalid(&command, "media type");

    let mut command = command_fixture();
    command.input_bundle_ref.as_mut().unwrap().byte_length = 64 * 1024 + 1;
    assert_resource_exhausted(&command, "manifest");

    let mut command = command_fixture();
    command.command_id = "x".repeat(257);
    assert_resource_exhausted(&command, "string limit");

    let mut command = command_fixture();
    command.tracestate = "x".repeat(513);
    assert_resource_exhausted(&command, "trace state");

    let mut command = command_fixture();
    let Some(worker_command_v1::CapabilityCommand::AgentExecution(agent)) =
        command.capability_command.as_mut()
    else {
        panic!("agent command");
    };
    agent.client_stream_id = "unsafe\nstream".to_owned();
    assert_invalid(&command, "correlation");

    let mut command = command_fixture();
    let Some(worker_command_v1::CapabilityCommand::AgentExecution(agent)) =
        command.capability_command.as_mut()
    else {
        panic!("agent command");
    };
    agent.sio_event = "unexpected".to_owned();
    assert_invalid(&command, "event route");
}

fn signed_fixture(name: &str) -> SignedWorkerCommandEnvelopeV1 {
    SignedWorkerCommandEnvelopeV1::decode(fixture(name).as_slice()).expect("signed fixture")
}

fn command_fixture() -> WorkerCommandV1 {
    let signed = signed_fixture("application_hmac");
    WorkerCommandV1::decode(signed.worker_command_bytes.as_slice()).expect("command fixture")
}

fn resign_hmac(
    mut signed: SignedWorkerCommandEnvelopeV1,
    command: Vec<u8>,
) -> SignedWorkerCommandEnvelopeV1 {
    let command_digest = digest::digest(&digest::SHA256, &command);
    signed.worker_command_bytes = command;
    let digest = signed.worker_command_digest.as_mut().unwrap();
    digest.algorithm = DigestAlgorithmV1::Sha256 as i32;
    digest.value = command_digest.as_ref().to_vec();
    let key = hmac::Key::new(hmac::HMAC_SHA256, CONFORMANCE_KEY);
    signed.signature = hmac::sign(&key, &signed.worker_command_bytes)
        .as_ref()
        .to_vec();
    signed
}

const ED25519_DOMAIN: &[u8] = b"elitea.runtime.worker-command.ed25519.v1\0";

fn sign_ed25519(
    mut signed: SignedWorkerCommandEnvelopeV1,
    key_id: &str,
    seed: &[u8; 32],
) -> SignedWorkerCommandEnvelopeV1 {
    let key_pair = signature::Ed25519KeyPair::from_seed_unchecked(seed).unwrap();
    let mut input = ED25519_DOMAIN.to_vec();
    input.extend_from_slice(
        &u64::try_from(signed.worker_command_bytes.len())
            .unwrap()
            .to_be_bytes(),
    );
    input.extend_from_slice(&signed.worker_command_bytes);
    key_id.clone_into(&mut signed.key_id);
    signed.signature = key_pair.sign(&input).as_ref().to_vec();
    signed
}

fn signed_command(command: &WorkerCommandV1) -> Vec<u8> {
    let base = signed_fixture("application_hmac");
    resign_hmac(base, command.encode_to_vec()).encode_to_vec()
}

fn assert_invalid(command: &WorkerCommandV1, expected: &str) {
    let error = parse_and_verify_agent_command(
        &signed_command(command),
        Some(&TestOnlyConformanceHmacAuthenticator),
    );
    assert!(matches!(
        error,
        Err(ProtocolError::InvalidInput(message)) if message.contains(expected)
    ));
}

fn assert_resource_exhausted(command: &WorkerCommandV1, expected: &str) {
    let error = parse_and_verify_agent_command(
        &signed_command(command),
        Some(&TestOnlyConformanceHmacAuthenticator),
    );
    assert!(matches!(
        error,
        Err(ProtocolError::ResourceExhausted(message)) if message.contains(expected)
    ));
}

fn assert_unsupported(command: &WorkerCommandV1) {
    assert!(matches!(
        parse_and_verify_agent_command(
            &signed_command(command),
            Some(&TestOnlyConformanceHmacAuthenticator)
        ),
        Err(ProtocolError::UnsupportedCapability(_))
    ));
}

fn encode_varint(mut value: u64) -> Vec<u8> {
    let mut result = Vec::new();
    while value >= 0x80 {
        result.push(u8::try_from(value & 0x7f).unwrap() | 0x80);
        value >>= 7;
    }
    result.push(u8::try_from(value).expect("final varint octet"));
    result
}
