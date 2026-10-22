use std::collections::BTreeMap;

use elitea_worker_rust::protocol::ProtocolError;
use elitea_worker_rust::protocol::command::{
    SignedCommandAuthenticator, TestOnlyConformanceHmacAuthenticator,
    parse_and_verify_agent_command,
};
use elitea_worker_rust::protocol::elitea::runtime::v1::{
    DigestAlgorithmV1, ExecutionFenceV1, ExecutionOutputEventTypeV1, NodeEventV1,
    execution_output_frame_v1,
};
use elitea_worker_rust::protocol::node_event::{
    MAX_CURRENT_NODE_EVENT_JSON_BYTES, decode_current_node_event_json,
    encode_current_node_event_json, parse_node_event_proto,
};
use elitea_worker_rust::protocol::output::{
    MAX_OUTPUT_FRAME_BYTES, OUTPUT_SCHEMA_REVISION, build_node_event_output_frame,
};
use prost::Message;
use ring::digest;
use serde::Deserialize;
use serde_json::value::RawValue;

#[derive(Deserialize)]
struct ParityCorpus {
    contract_revision: String,
    current_event_types: Vec<String>,
    cases: Vec<ParityCase>,
}

#[derive(Deserialize)]
struct ParityCase {
    name: String,
    wire_sha256: String,
    event: Box<RawValue>,
}

fn decode_hex(value: &str) -> Vec<u8> {
    let value = value.trim();
    assert_eq!(value.len() % 2, 0);
    value
        .as_bytes()
        .as_chunks::<2>()
        .0
        .iter()
        .map(|pair| {
            u8::from_str_radix(std::str::from_utf8(pair).unwrap(), 16).expect("fixture hex")
        })
        .collect()
}

fn named_vectors(path: &str) -> BTreeMap<String, Vec<u8>> {
    path.lines()
        .map(|line| {
            let (name, value) = line.split_once('=').expect("named fixture");
            (name.to_owned(), decode_hex(value))
        })
        .collect()
}

fn corpus() -> ParityCorpus {
    serde_json::from_str(include_str!(
        "../../../testdata/proto/runtime/v1/node-event/current-parity-corpus.json"
    ))
    .expect("NodeEvent parity corpus")
}

#[test]
fn current_corpus_preserves_all_event_types_wire_digests_and_json_semantics() {
    let corpus = corpus();
    assert_eq!(corpus.contract_revision, "elitea.runtime.node-event.v1");
    assert_eq!(corpus.current_event_types.len(), 36);
    assert_eq!(corpus.cases.len(), 2);

    for event_type in corpus.current_event_types {
        decode_current_node_event_json(format!(r#"{{"type":"{event_type}"}}"#).as_bytes())
            .expect("current event type is representable");
    }

    for test_case in corpus.cases {
        let event = decode_current_node_event_json(test_case.event.get().as_bytes())
            .unwrap_or_else(|error| panic!("{}: {error}", test_case.name));
        let wire = event.encode_to_vec();
        assert!(wire.len() < MAX_OUTPUT_FRAME_BYTES);
        assert_eq!(
            hex_digest(&wire),
            test_case.wire_sha256,
            "{} wire digest",
            test_case.name
        );
        let encoded = encode_current_node_event_json(&event).expect("browser JSON");
        let expected: serde_json::Value =
            serde_json::from_str(test_case.event.get()).expect("corpus event");
        let actual: serde_json::Value = serde_json::from_slice(&encoded).expect("encoded event");
        assert_eq!(actual, expected, "{} JSON semantics", test_case.name);
        parse_node_event_proto(&wire).expect("canonical closed NodeEvent wire");
    }
}

#[test]
fn exact_python_vectors_match_browser_proto_and_claim_bound_output_frame() {
    let vectors = named_vectors(include_str!("fixtures/node_event_vectors.txt"));
    let event = parse_node_event_proto(&vectors["node_event_proto"]).expect("Python NodeEvent");
    assert_eq!(
        encode_current_node_event_json(&event).expect("browser JSON"),
        vectors["browser_json"]
    );

    let commands = named_vectors(include_str!("fixtures/agent_command_vectors.txt"));
    let authenticator = TestOnlyConformanceHmacAuthenticator;
    let verified = parse_and_verify_agent_command(
        &commands["application_hmac"],
        Some(&authenticator as &dyn SignedCommandAuthenticator),
    )
    .expect("Python signed command");
    let fence = fixture_fence();
    let frame = build_node_event_output_frame(&verified, &fence, event, 9, 1_786_940_222_654, 4)
        .expect("progress frame");

    assert_eq!(frame.encode_to_vec(), vectors["output_frame"]);
    assert_eq!(frame.output_schema_revision, OUTPUT_SCHEMA_REVISION);
    assert_eq!(frame.stream_id, "execution-1:2");
    assert_eq!(frame.logical_output_id, "node-event:execution-1:9");
    assert_eq!(frame.event_id, "command-1:9");
    assert_eq!(
        frame.event_type,
        ExecutionOutputEventTypeV1::NodeEvent as i32
    );
    assert!(!frame.terminal);
    assert!(frame.settlement_proposal.is_none());
    assert!(frame.encoded_len() <= MAX_OUTPUT_FRAME_BYTES);
    let Some(execution_output_frame_v1::Payload::NodeEvent(event)) = &frame.payload else {
        panic!("NodeEvent payload");
    };
    let payload_digest = frame.payload_digest.as_ref().expect("payload digest");
    assert_eq!(payload_digest.algorithm, DigestAlgorithmV1::Sha256 as i32);
    assert_eq!(
        payload_digest.value,
        digest::digest(&digest::SHA256, &event.encode_to_vec()).as_ref()
    );
}

#[test]
fn defaults_exact_integers_raw_escapes_and_go_html_escaping_are_stable() {
    let minimal =
        decode_current_node_event_json(br#"{"type":"agent_exception","content":"safe failure"}"#)
            .expect("minimal event");
    assert_eq!(minimal.content, br#""safe failure""#);
    assert_eq!(minimal.response_metadata, b"{}");
    assert_eq!(minimal.references, b"[]");
    assert_eq!(
        encode_current_node_event_json(&minimal).expect("minimal JSON"),
        br#"{"type":"agent_exception","stream_id":null,"message_id":null,"question_id":null,"content":"safe failure","thinking":null,"response_metadata":{},"references":[],"sio_event":null,"created_at":null,"parent_message_id":null,"agent_name":null,"execution_generation":null}"#
    );

    let explicit_nulls = decode_current_node_event_json(
        br#"{"type":"agent_response","response_metadata":null,"references":null}"#,
    )
    .expect("explicit null fragments");
    assert_eq!(explicit_nulls.response_metadata, b"null");
    assert_eq!(explicit_nulls.references, b"null");

    let event = decode_current_node_event_json(
        r#"{"type":"agent_response","content":{"escaped":"\u0061","exact":9007199254740993,"html":"<&>  "},"thinking":"<&>  "}"#
            .as_bytes(),
    )
    .expect("lossless event");
    assert_eq!(
        event.content,
        r#"{"escaped":"\u0061","exact":9007199254740993,"html":"<&>  "}"#.as_bytes()
    );
    let encoded = encode_current_node_event_json(&event).expect("Go-compatible JSON");
    let text = std::str::from_utf8(&encoded).unwrap();
    assert!(text.contains(r#""escaped":"\u0061""#));
    assert!(text.contains("9007199254740993"));
    assert!(text.contains(r"\u003c\u0026\u003e\u2028\u2029"));
    assert!(!text.contains('<'));
    assert!(!text.contains('&'));
    assert!(!text.contains('>'));
}

#[test]
fn go_authoritative_raw_fragment_policy_is_explicit_against_python_normalization() {
    let vectors = named_vectors(include_str!("fixtures/node_event_vectors.txt"));
    let rust_event = decode_current_node_event_json(&vectors["normalization_input"])
        .expect("Go-compatible lossless fragment");
    let python_event = parse_node_event_proto(&vectors["python_normalized_proto"])
        .expect("Python-normalized fixture");

    assert_ne!(rust_event.encode_to_vec(), python_event.encode_to_vec());
    assert_eq!(
        rust_event.content,
        br#"{"escaped":"\u0061","float":1e0,"negative_zero":-0}"#
    );
    assert_eq!(
        python_event.content,
        br#"{"escaped":"a","float":1.0,"negative_zero":0}"#
    );
    assert_eq!(
        encode_current_node_event_json(&python_event).unwrap(),
        vectors["python_normalized_browser_json"]
    );

    let rust_json: serde_json::Value =
        serde_json::from_slice(&encode_current_node_event_json(&rust_event).unwrap()).unwrap();
    let python_json: serde_json::Value =
        serde_json::from_slice(&vectors["python_normalized_browser_json"]).unwrap();
    assert_eq!(
        rust_json["content"]["escaped"],
        python_json["content"]["escaped"]
    );
    assert_eq!(rust_json["content"]["float"].as_f64(), Some(1.0));
    assert_eq!(python_json["content"]["float"].as_f64(), Some(1.0));
    assert_eq!(rust_json["content"]["negative_zero"].as_i64(), Some(0));
    assert_eq!(python_json["content"]["negative_zero"].as_i64(), Some(0));
}

#[test]
fn malformed_shapes_duplicates_strings_timestamps_and_limits_fail_closed() {
    let deep = format!(
        r#"{{"type":"agent_response","content":{}null{}}}"#,
        "[".repeat(64),
        "]".repeat(64)
    );
    let oversized = format!(
        r#"{{"type":"agent_response","content":"{}"}}"#,
        "x".repeat(MAX_CURRENT_NODE_EVENT_JSON_BYTES)
    );
    let invalid = [
        "[]".to_owned(),
        r#"{"content":null}"#.to_owned(),
        r#"{"type":"agent-response"}"#.to_owned(),
        r#"{"type":"agent_response","unknown":true}"#.to_owned(),
        r#"{"type":"agent_response","type":"agent_exception"}"#.to_owned(),
        r#"{"type":"agent_response","t\u0079pe":"agent_exception"}"#.to_owned(),
        r#"{"type":"agent_response","response_metadata":[]}"#.to_owned(),
        r#"{"type":"agent_response","references":{}}"#.to_owned(),
        r#"{"type":"agent_response","created_at":"2023-02-29T00:00:00Z"}"#.to_owned(),
        r#"{"type":"agent_response","created_at":"2026-01-01T00:00:00"}"#.to_owned(),
        r#"{"type":"agent_response","stream_id":"unsafe\nroom"}"#.to_owned(),
        r#"{"type":"agent_response","content":NaN}"#.to_owned(),
        r#"{"type":"agent_response","response_metadata":{"state":1,"st\u0061te":2}}"#.to_owned(),
        deep,
        oversized,
    ];
    for (index, raw) in invalid.iter().enumerate() {
        assert!(
            decode_current_node_event_json(raw.as_bytes()).is_err(),
            "invalid case {index} was accepted"
        );
    }

    decode_current_node_event_json(
        br#"{"type":"agent_response","created_at":"2024-02-29T23:59:59.123456789+23:59"}"#,
    )
    .expect("valid leap-day RFC3339 timestamp");

    let mut malformed_fragment = NodeEventV1 {
        r#type: "agent_response".to_owned(),
        content: b"null".to_vec(),
        response_metadata: b"[]".to_vec(),
        references: b"[]".to_vec(),
        ..NodeEventV1::default()
    };
    assert!(encode_current_node_event_json(&malformed_fragment).is_err());
    malformed_fragment.response_metadata = b"{}".to_vec();
    malformed_fragment.thinking = Some("x".repeat(MAX_CURRENT_NODE_EVENT_JSON_BYTES));
    assert!(encode_current_node_event_json(&malformed_fragment).is_err());
}

#[test]
fn production_sized_tool_output_leaves_complete_frame_headroom() {
    const OUTPUT_BYTES: usize = 51_979;
    const ESCAPED_QUOTE_COUNT: usize = 5_385;
    let tool_output =
        "\"".repeat(ESCAPED_QUOTE_COUNT) + &"x".repeat(OUTPUT_BYTES - ESCAPED_QUOTE_COUNT);
    let metadata = serde_json::to_vec(&serde_json::json!({
        "tool_name": "list_initiatives",
        "tool_run_id": "run-production-sized",
        "tool_inputs": {"max_records": 100},
        "tool_output": tool_output,
        "metadata": {"toolkit_name": "aha"},
        "finish_reason": "stop",
        "timestamp_start": "2026-08-03T18:47:20Z",
        "timestamp_finish": "2026-08-03T18:47:21Z"
    }))
    .unwrap();
    let event = NodeEventV1 {
        r#type: "agent_tool_end".to_owned(),
        content: b"null".to_vec(),
        response_metadata: metadata,
        references: b"[]".to_vec(),
        ..NodeEventV1::default()
    };
    let browser = encode_current_node_event_json(&event).expect("production-sized browser JSON");
    assert!(browser.len() <= MAX_CURRENT_NODE_EVENT_JSON_BYTES);

    let commands = named_vectors(include_str!("fixtures/agent_command_vectors.txt"));
    let authenticator = TestOnlyConformanceHmacAuthenticator;
    let verified = parse_and_verify_agent_command(
        &commands["application_hmac"],
        Some(&authenticator as &dyn SignedCommandAuthenticator),
    )
    .unwrap();
    let frame = build_node_event_output_frame(&verified, &fixture_fence(), event, 1, 1, 0)
        .expect("production-sized output frame");
    assert!(frame.encoded_len() <= MAX_OUTPUT_FRAME_BYTES);
}

#[test]
fn protobuf_wire_and_progress_identity_mutations_are_rejected() {
    let event = decode_current_node_event_json(br#"{"type":"agent_response"}"#).unwrap();
    let wire = event.encode_to_vec();

    let mut unknown = wire.clone();
    unknown.extend_from_slice(&[0x72, 0x00]);
    assert!(matches!(
        parse_node_event_proto(&unknown),
        Err(ProtocolError::IncompatibleVersion(_))
    ));
    let mut duplicate = wire.clone();
    duplicate.extend_from_slice(&[0x0a, 0x01, b'x']);
    assert!(matches!(
        parse_node_event_proto(&duplicate),
        Err(ProtocolError::InvalidInput(_))
    ));
    assert!(matches!(
        parse_node_event_proto(&[0x08, 0x01]),
        Err(ProtocolError::InvalidInput(_))
    ));

    let commands = named_vectors(include_str!("fixtures/agent_command_vectors.txt"));
    let authenticator = TestOnlyConformanceHmacAuthenticator;
    let verified = parse_and_verify_agent_command(
        &commands["application_hmac"],
        Some(&authenticator as &dyn SignedCommandAuthenticator),
    )
    .unwrap();
    for (sequence, occurred, watermark) in [(0, 1, 0), (1, 0, 0), (4, 1, 4)] {
        assert!(
            build_node_event_output_frame(
                &verified,
                &fixture_fence(),
                event.clone(),
                sequence,
                occurred,
                watermark,
            )
            .is_err()
        );
    }
    let mut invalid_fence = fixture_fence();
    invalid_fence.fence_token.pop();
    assert!(build_node_event_output_frame(&verified, &invalid_fence, event, 1, 1, 0).is_err());
    let mut zero_fence = fixture_fence();
    zero_fence.fence_token.fill(0);
    assert!(
        build_node_event_output_frame(
            &verified,
            &zero_fence,
            decode_current_node_event_json(br#"{"type":"agent_response"}"#).unwrap(),
            1,
            1,
            0,
        )
        .is_err()
    );
}

fn fixture_fence() -> ExecutionFenceV1 {
    ExecutionFenceV1 {
        workload_session_id: "workload-session-1".to_owned(),
        claim_attempt: 3,
        lease_epoch: 7,
        producer_id: "rust-worker-fixture".to_owned(),
        fence_token: vec![b'f'; 32],
    }
}

fn hex_digest(raw: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let digest = digest::digest(&digest::SHA256, raw);
    let mut encoded = String::with_capacity(digest.as_ref().len() * 2);
    for &byte in digest.as_ref() {
        encoded.push(char::from(HEX[usize::from(byte >> 4)]));
        encoded.push(char::from(HEX[usize::from(byte & 0x0f)]));
    }
    encoded
}
