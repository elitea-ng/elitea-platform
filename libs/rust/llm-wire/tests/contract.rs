//! The `/llm` caller contract (`conformance/llm-caller/contract.json`)
//! against the shared pieces themselves; both clients also run it through
//! their real request paths (the worker's facade tests, the model client's
//! `tests/caller_contract.rs`).

use elitea_llm_wire::headers::{
    ABSENT_HEADERS, BEARER_PREFIX, EXECUTION_HEADER, MAX_EXECUTION_ID_BYTES, PROJECT_HEADER,
    valid_execution_id,
};
use elitea_llm_wire::openai::{
    DoneMatch, StrictChunk, TypeCheck, UsageProfile, is_done, parse_strict_chunk, parse_usage,
    reasoning_text,
};
use elitea_llm_wire::refusal::{Refusal, refusal_code, retryable_status};
use elitea_llm_wire::route::CHAT_COMPLETIONS_ROUTE;
use elitea_llm_wire::sse::{SseDialect, SseOptions, SseSplitter};
use elitea_llm_wire::tool_calls::ToolCallLimits;
use serde_json::Value;

const CONTRACT: &str = include_str!("../../../../conformance/llm-caller/contract.json");

fn contract() -> Value {
    serde_json::from_str(CONTRACT).unwrap_or_else(|e| panic!("contract.json: {e}"))
}

#[test]
fn the_request_route_and_headers() {
    let contract = contract();
    let request = &contract["request"];
    assert_eq!(request["route"], CHAT_COMPLETIONS_ROUTE);
    let headers = request["headers"]
        .as_object()
        .unwrap_or_else(|| panic!("headers"));
    assert_eq!(headers[PROJECT_HEADER], "{project_id}");
    assert_eq!(headers[EXECUTION_HEADER], "{execution_id}");
    assert_eq!(
        headers["authorization"],
        format!("{BEARER_PREFIX}{{token}}")
    );
    assert_eq!(headers.len(), 3);
    let absent: Vec<&str> = request["headers_absent"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(Value::as_str)
        .collect();
    assert_eq!(absent, ABSENT_HEADERS);
}

#[test]
fn execution_ids_follow_the_edges_shape_rule() {
    let contract = contract();
    let rule = &contract["request"]["execution_id"];
    assert_eq!(
        rule["max_bytes"].as_u64(),
        u64::try_from(MAX_EXECUTION_ID_BYTES).ok()
    );
    for id in rule["valid"].as_array().into_iter().flatten() {
        assert!(valid_execution_id(id.as_str().unwrap_or_default()), "{id}");
    }
    for id in rule["invalid"].as_array().into_iter().flatten() {
        assert!(!valid_execution_id(id.as_str().unwrap_or_default()), "{id}");
    }
}

#[test]
fn every_refusal_has_its_code_and_retry_hint() {
    let contract = contract();
    let cases = contract["refusals"].as_array().cloned().unwrap_or_default();
    assert!(!cases.is_empty());
    for case in cases {
        let name = case["name"].as_str().unwrap_or_default();
        let status = u16::try_from(case["status"].as_u64().unwrap_or(0)).unwrap_or(0);
        let body = case["body"].to_string();
        assert_eq!(
            refusal_code(status, body.as_bytes()),
            case["code"],
            "{name}"
        );
        assert_eq!(
            Refusal::classify(status, body.as_bytes()).map(Refusal::code),
            case["code"].as_str(),
            "{name}"
        );
        assert_eq!(
            Some(retryable_status(status)),
            case["retryable"].as_bool(),
            "{name}"
        );
    }
}

fn sse(events: &[Value]) -> String {
    let mut text = String::new();
    for event in events {
        text.push_str("data: ");
        text.push_str(&event.to_string());
        text.push_str("\n\n");
    }
    text.push_str("data: [DONE]\n\n");
    text
}

fn events(dialect: SseDialect, stream: &str) -> Vec<Vec<u8>> {
    let mut splitter = SseSplitter::new(SseOptions {
        dialect,
        reject_event_types: dialect == SseDialect::Strict,
        ..SseOptions::default()
    });
    let mut out = Vec::new();
    // Fed in small pieces, as a network would.
    for piece in stream.as_bytes().chunks(7) {
        splitter
            .push_into(piece, &mut out)
            .unwrap_or_else(|e| panic!("{e}"));
    }
    splitter
        .finish_into(&mut out)
        .unwrap_or_else(|e| panic!("{e}"));
    out.into_iter().map(|event| event.data).collect()
}

#[test]
fn the_reasoning_stream_reads_alike_in_both_profiles() {
    let contract = contract();
    let reasoning = &contract["reasoning"];
    let expected = &reasoning["expected"];
    let stream = sse(&reasoning["stream"].as_array().cloned().unwrap_or_default());

    // Lenient: the primitives the model client composes.
    let (mut content, mut thought, mut usage) = (String::new(), String::new(), None);
    for data in events(SseDialect::Lenient, &stream) {
        if is_done(&data, DoneMatch::Trimmed) {
            break;
        }
        let value: Value = serde_json::from_slice(&data).unwrap_or_else(|e| panic!("{e}"));
        if let Ok(Some(found)) = parse_usage(value.get("usage"), UsageProfile::Lenient) {
            usage = Some(found);
        }
        let Some(delta) = value["choices"].get(0).map(|choice| &choice["delta"]) else {
            continue;
        };
        if let Ok(Some(text)) = reasoning_text(delta, TypeCheck::Lenient) {
            thought.push_str(text);
        }
        if let Some(text) = delta["content"].as_str() {
            content.push_str(text);
        }
    }
    assert_eq!(content, expected["content"].as_str().unwrap_or_default());
    assert_eq!(thought, expected["reasoning"].as_str().unwrap_or_default());
    let usage = usage.unwrap_or_default();
    assert_eq!(
        usage.reasoning_tokens,
        expected["reasoning_tokens"].as_u64()
    );
    assert_eq!(usage.cached_tokens, expected["cached_tokens"].as_u64());

    // Strict: the worker's chunk reader.
    let limits = ToolCallLimits {
        max_calls: 16,
        max_argument_bytes: 256 * 1024,
        max_name_bytes: 256,
        max_id_bytes: 512,
    };
    let (mut content, mut thought, mut usage, mut done) =
        (String::new(), String::new(), None, false);
    for data in events(SseDialect::Strict, &stream) {
        match parse_strict_chunk(&data, &limits, i32::MAX.unsigned_abs().into())
            .unwrap_or_else(|e| panic!("{e}"))
        {
            StrictChunk::Done => done = true,
            StrictChunk::Usage(found) => usage = found.or(usage),
            StrictChunk::Delta(delta) => {
                usage = delta.usage.or(usage);
                thought.push_str(delta.reasoning.as_deref().unwrap_or_default());
                content.push_str(delta.content.as_deref().unwrap_or_default());
            }
        }
    }
    assert!(done);
    assert_eq!(content, expected["content"].as_str().unwrap_or_default());
    assert_eq!(thought, expected["reasoning"].as_str().unwrap_or_default());
    let usage = usage.unwrap_or_default();
    assert_eq!(
        usage.reasoning_tokens,
        expected["reasoning_tokens"].as_u64()
    );
    assert_eq!(usage.cached_tokens, expected["cached_tokens"].as_u64());
}
