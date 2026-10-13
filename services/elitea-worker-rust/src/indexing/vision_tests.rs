//! The image-file vision step (ADR-0030 I4 part B) through a whole run: a
//! scripted vision `Llm` answers a description, and the pipeline's output is
//! read back from the `elitea-vector` fake.
//!
//! What they pin: the request (the SDK's default prompt beside the image as
//! an `InlineData` part), the chunks (markdown, `processing_method: llm`),
//! the in-run cache (a repeated image costs one call and no cap), the
//! per-image bound, the closed skip reasons, the failure policy shared with
//! the proposal chunker, and that neither bytes nor descriptions are logged.

use std::sync::{Arc, Mutex};

use adk_rust::ErrorCategory;
use elitea_content_source::Document;
use serde_json::json;

use super::model_step_tests::{
    Reply, ScriptedChat, failed_with, run_model, run_model_raw, search, search_host, with_chat,
};
use super::tests::{Always, Harness, document, harness, parameters};
use crate::protocol::elitea::runtime::v1::IndexIngestStatusV1;
use crate::transport::openai_compatible_facade::MAX_INLINE_IMAGE_BYTES;

const DESCRIPTION: &str = "# Bar chart\n\nRevenue rises every quarter.\n";

/// A file of `kind` whose bytes start with that format's signature, then
/// `tag` (so two tags are two images).
fn image(name: &str, kind: &str, tag: &str) -> Document {
    let mut bytes: Vec<u8> = match kind {
        "png" => b"\x89PNG\r\n\x1a\n".to_vec(),
        "jpeg" => vec![0xff, 0xd8, 0xff, 0xe0],
        "gif" => b"GIF89a".to_vec(),
        "webp" => b"RIFF\0\0\0\0WEBPVP8 ".to_vec(),
        "bmp" => b"BM".to_vec(),
        "svg" => b"<svg xmlns=\"http://www.w3.org/2000/svg\"/>".to_vec(),
        other => panic!("{other}"),
    };
    bytes.extend_from_slice(tag.as_bytes());
    let mut document = document(name, "v1", "");
    document.reference.size = bytes.len() as u64;
    document.bytes = bytes;
    document
}

fn describing(_: usize, _: &str) -> Reply {
    Reply::Text(DESCRIPTION.to_owned())
}

fn defaults() -> Vec<u8> {
    parameters(false, json!({}))
}

#[tokio::test(flavor = "multi_thread")]
async fn png_jpeg_gif_and_webp_are_described_and_stored_with_the_sdk_prompt() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(describing);
    with_chat(&mut harness, &chat);
    let documents = vec![
        image("shots/a.png", "png", "one"),
        image("shots/b.jpg", "jpeg", "two"),
        image("shots/c.gif", "gif", "three"),
        image("shots/d.webp", "webp", "four"),
    ];
    let sent: Vec<(String, Vec<u8>)> = documents
        .iter()
        .map(|d| {
            let media = match d.bytes[0] {
                0x89 => "image/png",
                0xff => "image/jpeg",
                b'G' => "image/gif",
                _ => "image/webp",
            };
            (media.to_owned(), d.bytes.clone())
        })
        .collect();
    let summary = run_model(&harness, documents, None, &defaults(), Some("gpt-vision"))
        .await
        .expect("summary");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Ok as i32,
        "{summary:?}"
    );
    assert_eq!(summary.indexed_documents, 4, "{summary:?}");
    assert_eq!(summary.indexed_chunks, 4);
    assert_eq!(chat.calls(), 4);
    // Every request carried exactly one image: the file's bytes under its
    // own media type.
    let mut seen: Vec<(String, Vec<u8>)> = chat
        .state
        .images
        .lock()
        .expect("images")
        .iter()
        .map(|parts| {
            assert_eq!(parts.len(), 1);
            parts[0].clone()
        })
        .collect();
    let mut expected = sent;
    seen.sort();
    expected.sort();
    assert_eq!(seen, expected);
    // The chat model is bound with no system message: the prompt rides in
    // the user message, as the SDK's single HumanMessage.
    let binds = chat.state.binds.lock().expect("binds").clone();
    assert!(
        binds
            .iter()
            .all(|(model, system)| model == "gpt-vision" && system.is_empty())
    );

    let Harness {
        runtime,
        token,
        fake,
        ..
    } = harness;
    assert_eq!(fake.point_count(), 4);
    let runtime = Arc::new(runtime);
    let host = search_host(&runtime, &token);
    let found = search(
        &host,
        json!({"query": "Revenue rises", "index_name": "docs", "cut_off": 0,
               "output_fields": ["page_content", "metadata"]}),
    )
    .await;
    let hits = found.as_array().unwrap_or_else(|| panic!("{found}"));
    assert_eq!(hits.len(), 4, "{found}");
    for hit in hits {
        assert_eq!(hit["metadata"]["processing_method"], "llm", "{hit}");
        assert_eq!(hit["metadata"]["chunk_type"], "document");
        assert!(
            hit["page_content"]
                .as_str()
                .is_some_and(|text| text.contains("Revenue rises every quarter.")),
            "{hit}"
        );
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn the_request_is_the_sdk_prompt_then_the_image() {
    let mut harness = harness(4, None).await;
    let prompts = Arc::new(Mutex::new(Vec::new()));
    let seen = Arc::clone(&prompts);
    let chat = ScriptedChat::new(move |_, user| {
        seen.lock().expect("prompts").push(user.to_owned());
        describing(0, user)
    });
    with_chat(&mut harness, &chat);
    run_model(
        &harness,
        vec![image("a.png", "png", "x")],
        None,
        &defaults(),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    let prompts = prompts.lock().expect("prompts");
    assert_eq!(prompts.len(), 1);
    assert_eq!(prompts[0], elitea_doc_chunk::image_processing_prompt());
}

#[tokio::test(flavor = "multi_thread")]
async fn a_repeated_image_is_one_model_call_and_the_cache_hit_is_not_charged() {
    // Cap of ONE call: three identical images (two keys) would fail on the
    // second document if a hit were charged.
    let mut harness = harness(4, None).await;
    harness.runtime.limits.max_model_calls = 1;
    let chat = ScriptedChat::new(describing);
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![
            image("logo-1.png", "png", "same"),
            image("deep/logo-2.png", "png", "same"),
            image("logo-3.PNG", "png", "same"),
        ],
        None,
        &defaults(),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Ok as i32,
        "{summary:?}"
    );
    assert_eq!(summary.indexed_documents, 3, "{summary:?}");
    assert_eq!(chat.calls(), 1, "one call for three copies");
    assert_eq!(harness.fake.point_count(), 3);
}

#[tokio::test(flavor = "multi_thread")]
async fn the_same_image_with_another_prompt_is_a_new_call() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(describing);
    with_chat(&mut harness, &chat);
    // Declared webp, but the bytes are PNG: the bytes' own type is sent, and
    // the extension's block names a different prompt.
    let summary = run_model(
        &harness,
        vec![
            image("a.png", "png", "same"),
            image("b.webp", "png", "same"),
            image("c.png", "png", "same"),
        ],
        None,
        &parameters(
            false,
            json!({"chunking_config": {".webp": {"prompt": "Read only the axis labels."}}}),
        ),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 3, "{summary:?}");
    assert_eq!(chat.calls(), 2, "two prompts, two calls");
    let images = chat.state.images.lock().expect("images");
    assert!(images.iter().all(|parts| parts[0].0 == "image/png"));
}

#[tokio::test(flavor = "multi_thread")]
async fn an_oversized_image_is_skipped_too_large_without_a_call() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(describing);
    with_chat(&mut harness, &chat);
    let tag = "x".repeat(MAX_INLINE_IMAGE_BYTES);
    let summary = run_model(
        &harness,
        vec![
            image("big.png", "png", &tag),
            image("small.png", "png", "ok"),
        ],
        None,
        &defaults(),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.skipped.get("too_large"), Some(&1), "{summary:?}");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(chat.calls(), 1);
}

#[tokio::test(flavor = "multi_thread")]
async fn no_model_skips_images_and_an_unreadable_chat_entry_does_not_fail_the_run() {
    let mut harness = harness(4, None).await;
    let documents = || {
        vec![
            image("a.png", "png", "x"),
            document("notes.md", "v1", "# Notes\n\nPlain text.\n"),
        ]
    };
    // No binder in the deployment.
    let summary = run_model(&harness, documents(), None, &defaults(), Some("gpt-vision"))
        .await
        .expect("summary");
    assert_eq!(summary.skipped.get("no_model"), Some(&1), "{summary:?}");
    assert_eq!(summary.indexed_documents, 1, "the text document is indexed");

    // A binder, but the claim names no model.
    let chat = ScriptedChat::new(describing);
    with_chat(&mut harness, &chat);
    let summary = run_model(&harness, documents(), None, &defaults(), None)
        .await
        .expect("summary");
    assert_eq!(summary.skipped.get("no_model"), Some(&1), "{summary:?}");
    assert_eq!(chat.calls(), 0);

    // A chat entry that cannot be read, in a run that never asked for the
    // proposal chunker: the images are `no_model`, the run is not refused.
    let summary = run_model_raw(
        &harness,
        documents(),
        None,
        &defaults(),
        Some(b"{not a model name"),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.skipped.get("no_model"), Some(&1), "{summary:?}");
    assert_eq!(chat.calls(), 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn use_llm_false_and_formats_this_build_does_not_convert_are_unsupported() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(describing);
    with_chat(&mut harness, &chat);
    // Off for .png only: .jpg is still described.
    let summary = run_model(
        &harness,
        vec![
            image("a.png", "png", "x"),
            image("b.jpg", "jpeg", "y"),
            image("c.bmp", "bmp", "z"),
            image("d.svg", "svg", "w"),
        ],
        None,
        &parameters(
            false,
            json!({"chunking_config": {".png": {"use_llm": false}}}),
        ),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.skipped.get("unsupported_type"),
        Some(&3),
        "{summary:?}"
    );
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(chat.calls(), 1);

    // Off at the top level: nothing is sent.
    let summary = run_model(
        &harness,
        vec![image("e.jpg", "jpeg", "q")],
        None,
        &parameters(false, json!({"chunking_config": {"use_llm": false}})),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.skipped.get("unsupported_type"),
        Some(&1),
        "{summary:?}"
    );
    assert_eq!(chat.calls(), 1, "no new call");
}

#[tokio::test(flavor = "multi_thread")]
async fn bytes_that_are_not_the_declared_image_are_unreadable() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(describing);
    with_chat(&mut harness, &chat);
    let mut fake = image("a.png", "png", "");
    fake.bytes = b"this is not a png".to_vec();
    fake.reference.size = fake.bytes.len() as u64;
    let summary = run_model(&harness, vec![fake], None, &defaults(), Some("gpt-vision"))
        .await
        .expect("summary");
    assert_eq!(summary.skipped.get("unreadable"), Some(&1), "{summary:?}");
    assert_eq!(chat.calls(), 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn every_attempt_is_charged_and_a_cap_short_of_the_retries_stops_the_run() {
    // Two rate limits, then a description: three real calls against a cap of
    // three. The next distinct image finds nothing left and stops the run.
    let mut harness = harness(4, None).await;
    harness.runtime.limits.max_model_calls = 3;
    let chat = ScriptedChat::new(|index, _| {
        if index < 2 {
            Reply::Fail(ErrorCategory::RateLimited, "model_gateway.rate_limited")
        } else {
            describing(index, "")
        }
    });
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![image("a.png", "png", "one"), image("b.png", "png", "two")],
        None,
        &defaults(),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert!(
        failed_with(&summary).contains("limit of 3 model calls"),
        "{summary:?}"
    );
    assert_eq!(chat.calls(), 3);
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_failing_model_skips_the_image_and_a_refusal_fails_the_run() {
    // Retryable on every attempt: skipped `model_failed` after three
    // attempts, nothing stored for it.
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(|_, _| {
        Reply::Fail(ErrorCategory::Unavailable, "model_gateway.unavailable")
    });
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![image("a.png", "png", "x")],
        None,
        &defaults(),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.skipped.get("model_failed"), Some(&1), "{summary:?}");
    assert_eq!(chat.calls(), 3);
    assert_eq!(harness.fake.point_count(), 0);

    // A refusal another attempt cannot cure.
    let mut harness = super::tests::harness(4, None).await;
    let chat =
        ScriptedChat::new(|_, _| Reply::Fail(ErrorCategory::Forbidden, "model_gateway.forbidden"));
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![image("a.png", "png", "x")],
        None,
        &defaults(),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert!(
        failed_with(&summary).contains("model_gateway.forbidden"),
        "{summary:?}"
    );
    assert_eq!(chat.calls(), 1);
}

#[tokio::test(flavor = "multi_thread")]
async fn an_empty_description_is_the_sdk_placeholder() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(|_, _| Reply::Text("  ".to_owned()));
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![image("a.png", "png", "x"), image("b.png", "png", "x")],
        None,
        &defaults(),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 2, "{summary:?}");
    // A blank reply is not cached (as the SDK): both asked.
    assert_eq!(chat.calls(), 2);
}

const LOG_CHILD_ENV: &str = "ELITEA_VISION_LOG_CHILD";
const SECRET: &str = "SECRET-DESCRIPTION-TEXT";

/// The run's output is read in a child process of this test binary with a
/// process-wide subscriber (a thread-local one races with the other tests'
/// call sites), as the diagnostics tests do.
#[test]
fn neither_image_bytes_nor_descriptions_are_logged() {
    let output = std::process::Command::new(std::env::current_exe().expect("test binary"))
        .args([
            "--exact",
            "indexing::vision_tests::vision_log_child",
            "--nocapture",
            "--test-threads=1",
        ])
        .env(LOG_CHILD_ENV, "1")
        .output()
        .expect("vision log child process");
    let logged = String::from_utf8_lossy(&output.stdout);
    assert!(
        output.status.success(),
        "{logged}\n{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(logged.contains("CHILD-RAN"), "{logged}");
    // The failing first attempt is logged by its code ...
    assert!(logged.contains("index_model_call_failed"), "{logged}");
    assert!(logged.contains("model_gateway.unavailable"), "{logged}");
    // ... and nothing of the picture or its description is.
    assert!(!logged.contains(SECRET), "{logged}");
    assert!(!logged.contains("PIXELS-MARKER"), "{logged}");
    assert!(!logged.contains("PNG"), "{logged}");
}

#[test]
fn vision_log_child() {
    if std::env::var(LOG_CHILD_ENV).is_err() {
        return;
    }
    tracing_subscriber::fmt()
        .with_max_level(tracing::Level::TRACE)
        .without_time()
        .with_ansi(false)
        .with_writer(std::io::stdout)
        .init();
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .expect("child runtime");
    runtime.block_on(async {
        let mut harness = harness(4, None).await;
        let chat = ScriptedChat::new(|index, _| {
            if index == 0 {
                Reply::Fail(ErrorCategory::Unavailable, "model_gateway.unavailable")
            } else {
                Reply::Text(format!("# {SECRET}\n\nthe picture shows {SECRET}"))
            }
        });
        with_chat(&mut harness, &chat);
        let summary = run_model(
            &harness,
            vec![
                image("MARKER-BYTES.png", "png", "PIXELS-MARKER"),
                image("copy.png", "png", "PIXELS-MARKER"),
            ],
            None,
            &defaults(),
            Some("gpt-vision"),
        )
        .await
        .expect("summary");
        assert_eq!(summary.indexed_documents, 2, "{summary:?}");
        assert_eq!(chat.calls(), 2, "one failed attempt, one answer, one hit");
    });
    println!("CHILD-RAN");
}
