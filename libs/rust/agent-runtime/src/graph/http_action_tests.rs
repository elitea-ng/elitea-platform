#![allow(
    clippy::disallowed_methods,
    reason = "inline test fixtures and shared cross-language vector files"
)]

use std::collections::BTreeMap;

use serde::Deserialize;
use serde_json::{Value, json};

use super::*;

#[derive(Deserialize)]
struct RenderVectorsFile {
    schema_version: String,
    vectors: Vec<RenderVector>,
}

#[derive(Deserialize)]
struct RenderVector {
    id: String,
    rule: HttpActionRuleV1,
    node: Value,
    inputs: Value,
    expect: RenderExpect,
}

#[derive(Deserialize)]
struct RenderExpect {
    ok: Option<RenderExpectOk>,
    error: Option<String>,
}

#[derive(Deserialize)]
struct RenderExpectOk {
    method: String,
    path: String,
    query: String,
    url: String,
    headers: Vec<RenderedHttpHeader>,
    body_b64: String,
    body_sha256: String,
    body_utf8: String,
    request_wire: String,
    request_digest: String,
}

/// Cross-language render determinism gate: all 25 vectors in
/// `libs/jsonschema/runtime/v1/fixtures/http-action-render-vectors-v1.json`
/// (15 ok, 10 error codes) must match Go byte-for-byte.
#[test]
fn render_vectors_match() {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../jsonschema/runtime/v1/fixtures/http-action-render-vectors-v1.json");
    let raw = std::fs::read_to_string(&path).expect("render vectors fixture");
    let file: RenderVectorsFile = serde_json::from_str(&raw).expect("parse render vectors");
    assert_eq!(
        file.schema_version,
        "elitea.runtime.http-action-render-vectors.v1"
    );
    assert_eq!(file.vectors.len(), 25, "expected 25 render vectors");

    let mut ok_count = 0;
    let mut err_count = 0;
    for vector in file.vectors {
        let node_json = serde_json::to_string(&vector.node).expect("node json");
        let parsed_node = HttpActionNodeDefinition::from_json_str(&node_json);
        let inputs_bytes = crate::canonical::to_vec(&vector.inputs).expect("canonical inputs");

        if let Some(expected_ok) = vector.expect.ok {
            ok_count += 1;
            let node = parsed_node.unwrap_or_else(|e| panic!("{}: node parse: {e}", vector.id));
            let rendered = render_request(&vector.rule, &node, &inputs_bytes)
                .unwrap_or_else(|e| panic!("{}: render failed: {e:?}", vector.id));
            assert_eq!(rendered.method, expected_ok.method, "{}: method", vector.id);
            assert_eq!(rendered.path, expected_ok.path, "{}: path", vector.id);
            assert_eq!(rendered.query, expected_ok.query, "{}: query", vector.id);
            assert_eq!(rendered.url, expected_ok.url, "{}: url", vector.id);
            assert_eq!(
                rendered.headers, expected_ok.headers,
                "{}: headers",
                vector.id
            );
            assert_eq!(
                rendered.body_b64, expected_ok.body_b64,
                "{}: body_b64",
                vector.id
            );
            assert_eq!(
                rendered.body_sha256, expected_ok.body_sha256,
                "{}: body_sha256",
                vector.id
            );
            assert_eq!(
                String::from_utf8(rendered.body.clone()).expect("utf8 body"),
                expected_ok.body_utf8,
                "{}: body_utf8",
                vector.id
            );
            assert_eq!(
                rendered.request_wire, expected_ok.request_wire,
                "{}: request_wire",
                vector.id
            );
            assert_eq!(
                rendered.request_digest, expected_ok.request_digest,
                "{}: request_digest",
                vector.id
            );
        } else if let Some(expected_err) = vector.expect.error {
            err_count += 1;
            // Restricted header vector (`err-restricted-header`) is already rejected
            // at parse time as `invalid_input` (matching Go `node.go:264`).
            match parsed_node {
                Err(_) => {
                    assert_eq!(
                        expected_err, "invalid_input",
                        "{}: parse rejected unexpected error code",
                        vector.id
                    );
                }
                Ok(node) => {
                    let err = render_request(&vector.rule, &node, &inputs_bytes)
                        .expect_err(&format!("{}: expected error {expected_err}", vector.id));
                    assert_eq!(err.vector_code(), expected_err, "{}: error code", vector.id);
                }
            }
        } else {
            panic!("{}: vector has neither ok nor error", vector.id);
        }
    }
    assert_eq!(ok_count, 15);
    assert_eq!(err_count, 10);
}

#[test]
fn parses_etl_fixture_http_node_shape_and_binds_canonical_inputs() {
    let yaml = r#"
id: enrich_fx
type: http
revision: 2
operation: fx_reference.rates
request:
  path: {run_id: {from: run_id}, batch_no: {from: batch_no}}
  query: {notify: {value: "false"}, page: {from: cursor, optional: true}}
  headers: [{name: X-Trace-Id, from: trace}]
response: {mode: json, accepted_statuses: [{first: 200, last: 200}], max_bytes: 16384}
input: [run_id, batch_no]
output: [fx]
transition: normalize
"#;
    let node = HttpActionNodeDefinition::from_yaml(yaml).expect("parse ETL http node");
    assert_eq!(node.id(), "enrich_fx");
    assert_eq!(node.operation(), "fx_reference.rates");
    assert_eq!(node.output_key(), "fx");
    assert_eq!(node.transition(), Some("normalize"));
    assert!(node.input_keys().contains(&"run_id".to_owned()));
    assert!(node.input_keys().contains(&"batch_no".to_owned()));
    assert!(node.input_keys().contains(&"cursor".to_owned()));
    assert!(node.input_keys().contains(&"trace".to_owned()));

    let mut state = BTreeMap::new();
    state.insert("run_id".to_owned(), json!("20261019a-deadbeef"));
    state.insert("batch_no".to_owned(), json!(7));
    state.insert("trace".to_owned(), json!("tr-1"));
    // Optional `cursor` omitted -> absent from canonical inputs.
    let resolved = node
        .resolve_canonical_inputs(&state)
        .expect("resolve inputs");
    assert_eq!(
        String::from_utf8(resolved.canonical_bytes).expect("utf8"),
        r#"{"headers":{"x-trace-id":"tr-1"},"path":{"batch_no":7,"run_id":"20261019a-deadbeef"}}"#
    );
}

#[test]
fn rejects_unknown_fields_url_method_credential_and_bad_bounds() {
    let base = r"
id: fetch
type: http
revision: 2
operation: tracker.get_item
request: {}
response: {mode: json}
output: [out]
";
    assert!(HttpActionNodeDefinition::from_yaml(base).is_ok());

    for bad in [
        format!("{base}url: https://example.com\n"),
        format!("{base}method: GET\n"),
        format!("{base}credential: {{configuration_id: 1}}\n"),
        base.replace("revision: 2", "revision: 1"),
        format!("{base}timeout_ms: 0\n"),
        format!("{base}timeout_ms: 30001\n"),
        base.replace("output: [out]", "output: [a, b]"),
        base.replace(
            "response: {mode: json}",
            "response: {mode: json, max_bytes: 2097153}",
        ),
        base.replace(
            "response: {mode: json}",
            "response: {mode: json, accepted_statuses: [{first: 300, last: 302}]}",
        ),
        base.replace(
            "response: {mode: json}",
            "response: {mode: json, accepted_statuses: [{first: 400, last: 401}]}",
        ),
        base.replace(
            "response: {mode: json}",
            "response: {mode: json, accepted_statuses: [{first: 403, last: 404}]}",
        ),
    ] {
        assert!(
            HttpActionNodeDefinition::from_yaml(&bad).is_err(),
            "should reject:\n{bad}"
        );
    }
}

#[test]
fn inputs_size_boundary_and_dot_segments_refused() {
    let yaml = r"
id: post_item
type: http
revision: 2
operation: tracker.create_item
request:
  path: {item: {from: item_key}}
  body: {kind: text, from: payload, content_type: text/plain}
response: {mode: json}
output: [out]
";
    let node = HttpActionNodeDefinition::from_yaml(yaml).expect("parse");

    let mut dot_state = BTreeMap::new();
    dot_state.insert("item_key".to_owned(), json!(".."));
    dot_state.insert("payload".to_owned(), json!("ok"));
    assert_eq!(
        node.resolve_canonical_inputs(&dot_state),
        Err(HttpActionError::InvalidInput)
    );

    let mut oversized_state = BTreeMap::new();
    oversized_state.insert("item_key".to_owned(), json!("a"));
    oversized_state.insert(
        "payload".to_owned(),
        json!("x".repeat(MAX_HTTP_INPUTS_BYTES)),
    );
    assert_eq!(
        node.resolve_canonical_inputs(&oversized_state),
        Err(HttpActionError::ResourceExhausted)
    );
}

#[test]
fn visit_id_and_effect_id_v3_are_deterministic_and_attempt_sensitive() {
    let binding = [0xab_u8; 32];
    let binding_hex = hex_encode(&binding);
    let inputs_hex = hex_encode(&[0xcd_u8; 32]);
    let act = visit_id("thread-1", "fetch", 3, binding);
    assert!(valid_hex_digest(&act));
    assert_eq!(act, visit_id("thread-1", "fetch", 3, binding));
    assert_ne!(act, visit_id("thread-1", "fetch", 4, binding));

    let e0 = effect_id_v3_hex("exec-1", 1, &act, 0, &binding_hex, &inputs_hex);
    let e1 = effect_id_v3_hex("exec-1", 1, &act, 1, &binding_hex, &inputs_hex);
    assert!(valid_hex_digest(&e0));
    assert_ne!(e0, e1, "attempt must change effect_id v3");
}

#[test]
fn receipt_v2_fixtures_validate_and_invalid_rejected() {
    let fixtures_dir = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../jsonschema/runtime/v1/fixtures");
    for ok_file in [
        "http-action-receipt-v2.json",
        "http-action-receipt-v2.uncertain.json",
    ] {
        let bytes = std::fs::read(fixtures_dir.join(ok_file)).expect(ok_file);
        assert!(decode_receipt_v2(&bytes).is_ok(), "{ok_file} must decode");
    }
    for bad_file in [
        "http-action-receipt-v2.invalid.completed-with-failure.json",
        "http-action-receipt-v2.invalid.missing-result.json",
        "http-action-receipt-v2.invalid.state-dispatching.json",
    ] {
        let bytes = std::fs::read(fixtures_dir.join(bad_file)).expect(bad_file);
        assert!(decode_receipt_v2(&bytes).is_err(), "{bad_file} must fail");
    }
}
