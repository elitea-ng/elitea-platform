//! Image turns on the Rust worker (agent-zefir#21, the Rust side of #1065).
//!
//! The Python worker's #1065 defect was a progress event that echoed the
//! multimodal human message — inline `data:image/...;base64` URL included — so
//! an image at the advertised inline budget made one event larger than a node
//! frame and failed the turn before the model was called. These tests run a
//! whole direct-agent image turn through the native runtime, on both model
//! clients, and pin the three properties that defect broke: the turn answers,
//! the model receives the image, and no browser event or worker log line
//! carries the image's bytes.

use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64_STANDARD;

use super::super::assembly_tests::CapturedOutput;
use super::*;
use crate::protocol::node_event::MAX_CURRENT_NODE_EVENT_JSON_BYTES;

/// 48 KiB raw — the size the Python reproduction used, whose base64 alone
/// (64 KiB) is larger than one node event frame (60 KiB). A worker that echoed
/// it into any event could not encode that event at all.
const IMAGE_BYTES: usize = 48 * 1024;

/// Deterministic, incompressible-looking bytes, so the encoded form has no
/// long runs a substring check could match by accident.
fn image_bytes() -> Vec<u8> {
    let mut state: u32 = 0x9E37_79B9;
    (0..IMAGE_BYTES)
        .map(|_| {
            state ^= state << 13;
            state ^= state >> 17;
            state ^= state << 5;
            state.to_le_bytes()[0]
        })
        .collect()
}

fn image_attachment(encoded: &str) -> serde_json::Value {
    serde_json::json!({
        "type": "image_url",
        "image_url": {"url": format!("data:image/png;base64,{encoded}")}
    })
}

/// Every place this test looks for the image: the whole encoded payload, a
/// fragment from its middle (a truncated echo), and the data-URL scheme.
fn assert_no_image_bytes(surface: &str, text: &str, encoded: &str) {
    let middle = &encoded[encoded.len() / 2..encoded.len() / 2 + 64];
    assert!(
        !text.contains(encoded) && !text.contains(middle),
        "{surface} carries the image's base64 bytes"
    );
    assert!(
        !text.contains(";base64,"),
        "{surface} carries an inline data URL"
    );
}

/// The base64 payload of one model-request content part, when it is an image:
/// a PNG data URL on the OpenAI-compatible path, a base64 source block on the
/// Anthropic one.
fn sent_image(part: &serde_json::Value, anthropic: bool) -> Option<String> {
    if anthropic {
        if part["type"] != "image" {
            return None;
        }
        assert_eq!(part["source"]["type"], "base64");
        assert_eq!(part["source"]["media_type"], "image/png");
        return part["source"]["data"].as_str().map(str::to_owned);
    }
    let url = part["image_url"]["url"].as_str()?;
    Some(
        url.strip_prefix("data:image/png;base64,")
            .expect("a PNG data URL")
            .to_owned(),
    )
}

#[tokio::test(flavor = "current_thread")]
#[allow(clippy::too_many_lines)] // One turn per client and kind, checked end to end.
async fn image_turn_reaches_the_model_and_no_event_or_log_carries_the_image() {
    let raw = image_bytes();
    let encoded = BASE64_STANDARD.encode(&raw);
    assert!(encoded.len() > MAX_CURRENT_NODE_EVENT_JSON_BYTES);

    for anthropic in [false, true] {
        for kind in [AgentExecutionKind::Application, AgentExecutionKind::Adhoc] {
            let capture = CapturedOutput::default();
            let subscriber = tracing_subscriber::fmt()
                .without_time()
                .with_ansi(false)
                .with_max_level(tracing::Level::DEBUG)
                .with_writer(capture.clone())
                .finish();
            let logs = tracing::subscriber::set_default(subscriber);

            let mut request = ordinary_request(kind);
            if anthropic {
                use_native_anthropic(&mut request);
            }
            request.payload.input_attachments = vec![image_attachment(&encoded)];
            let (runtime_context, _context_calls) = runtime_context_client();
            let (model_gateway, captured) = test_model_gateway_client(
                vec![TestModelGatewayOutcome::Response(if anthropic {
                    anthropic_response()
                } else {
                    model_response()
                })],
                test_model_gateway_config(),
            )
            .expect("model gateway fixture client");
            let assembler = OrdinaryNativeAgentAssembler::new(
                platform_client(runtime_context),
                Arc::new(ModelFacade::from_gateway(model_gateway)),
                empty_tool_policy(),
            );
            let assembly = AuthorizedNativeAssembly::new(
                &request,
                test_runtime_context_authority(),
                AuthorizedNativeCommandBinding::fixture(),
            );
            let mut invocation = assembler
                .assemble(assembly)
                .await
                .expect("an image turn is admitted");

            let mut frames = Vec::new();
            frames.extend(
                invocation
                    .project_start(chrono::Utc::now())
                    .expect("image turn start"),
            );
            let (mut native, mut projector, completion) = invocation.start().expect("native start");
            while let Some(event) = native.next_event().await.expect("native event") {
                frames.extend(
                    projector
                        .project(&event)
                        .expect("an image turn's events project"),
                );
            }
            let completed = completion.select().await.expect("selected completion");
            frames.extend(
                projector
                    .finish_after_eos(completed, chrono::Utc::now())
                    .expect("finished image turn"),
            );

            let mut answer = None;
            for frame in &frames {
                let encoded_frame =
                    encode_current_node_event_json(frame).expect("every frame encodes");
                assert!(encoded_frame.len() <= MAX_CURRENT_NODE_EVENT_JSON_BYTES);
                let text = String::from_utf8(encoded_frame).expect("frame JSON is UTF-8");
                assert_no_image_bytes(&format!("the {} frame", frame.r#type), &text, &encoded);
                let value: serde_json::Value = serde_json::from_str(&text).expect("frame JSON");
                if value["type"] == "full_message" {
                    answer = value["content"].as_str().map(str::to_owned);
                }
            }
            assert_eq!(
                answer.as_deref(),
                Some(if anthropic {
                    "native Anthropic response"
                } else {
                    "native response"
                }),
                "the image turn answers ({kind:?}, anthropic={anthropic})"
            );

            // The image reached the model as an image, byte for byte.
            let captured = captured.lock().expect("captured model request");
            assert_eq!(captured.len(), 1);
            let body: serde_json::Value =
                serde_json::from_slice(&captured[0].body).expect("model request JSON");
            let user = body["messages"]
                .as_array()
                .and_then(|messages| messages.iter().rev().find(|m| m["role"] == "user"))
                .expect("the current user message");
            let parts = user["content"].as_array().expect("multimodal user content");
            let sent = parts
                .iter()
                .find_map(|part| sent_image(part, anthropic))
                .expect("the model request carries the image");
            assert_eq!(sent, encoded);

            drop(logs);
            assert_no_image_bytes("the worker log", &capture.text(), &encoded);
        }
    }
}
