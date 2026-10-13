//! The image-file vision step (ADR-0030 I4 part B) through a whole run: a
//! scripted vision `Llm` answers a description, and the pipeline's output is
//! read back from the `elitea-vector` fake.
//!
//! What they pin: the request (the SDK's default prompt beside the image as
//! an `InlineData` part), the chunks (markdown, `processing_method: llm`),
//! the in-run cache (a repeated image costs one call and no cap), the
//! per-image bound, the closed skip reasons, the failure policy shared with
//! the proposal chunker, and that neither bytes nor descriptions are logged.

use std::io::Cursor;
use std::sync::{Arc, Mutex};

use adk_rust::ErrorCategory;
use elitea_content_source::Document;
use image::{DynamicImage, ImageFormat, Rgb, RgbImage};
use serde_json::json;

use super::model_step_tests::{
    Reply, ScriptedChat, failed_with, run_model, run_model_raw, run_with_entries, search,
    search_host, with_chat,
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
        "bmp" => return real_image(name, ImageFormat::Bmp, 8, 8, tag),
        "svg" => b"<svg xmlns=\"http://www.w3.org/2000/svg\"/>".to_vec(),
        other => panic!("{other}"),
    };
    bytes.extend_from_slice(tag.as_bytes());
    let mut document = document(name, "v1", "");
    document.reference.size = bytes.len() as u64;
    document.bytes = bytes;
    document
}

/// A decodable picture of `width` x `height` whose pixels follow `tag`
/// (two tags are two pictures), as `format`.
fn real_image(name: &str, format: ImageFormat, width: u32, height: u32, tag: &str) -> Document {
    let mut state = tag.bytes().fold(0x9e37_79b9_u32, |acc, b| {
        acc.wrapping_mul(31).wrapping_add(u32::from(b))
    });
    let picture = RgbImage::from_fn(width, height, |_, _| {
        state = state.wrapping_mul(1_664_525).wrapping_add(1_013_904_223);
        let [a, b, c, _] = state.to_be_bytes();
        Rgb([a, b, c])
    });
    let mut bytes = Vec::new();
    DynamicImage::ImageRgb8(picture)
        .write_to(&mut Cursor::new(&mut bytes), format)
        .expect("encode");
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
async fn a_big_png_and_a_big_jpeg_are_downscaled_and_described() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(describing);
    with_chat(&mut harness, &chat);
    let big_png = real_image("big.png", ImageFormat::Png, 900, 700, "p");
    let big_jpeg = real_image("big.jpg", ImageFormat::Jpeg, 1400, 1000, "j");
    assert!(big_png.bytes.len() > MAX_INLINE_IMAGE_BYTES);
    assert!(big_jpeg.bytes.len() > MAX_INLINE_IMAGE_BYTES);
    let summary = run_model(
        &harness,
        vec![big_png, big_jpeg, image("small.png", "png", "ok")],
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
    assert!(summary.skipped.is_empty(), "{summary:?}");
    assert_eq!(chat.calls(), 3);
    // What the model received fits the gateway's bound and is a picture.
    for parts in chat.state.images.lock().expect("images").iter() {
        let (media, bytes) = &parts[0];
        assert!(bytes.len() <= MAX_INLINE_IMAGE_BYTES, "{}", bytes.len());
        assert!(matches!(media.as_str(), "image/png" | "image/jpeg"));
    }
}

/// A PNG header claiming `width` x `height`, then padding.
fn bomb(width: u32, height: u32) -> Document {
    fn crc32(bytes: &[u8]) -> u32 {
        let mut crc = !0_u32;
        for byte in bytes {
            crc ^= u32::from(*byte);
            for _ in 0..8 {
                crc = if crc & 1 == 1 {
                    (crc >> 1) ^ 0xedb8_8320
                } else {
                    crc >> 1
                };
            }
        }
        !crc
    }
    let mut ihdr = b"IHDR".to_vec();
    ihdr.extend_from_slice(&width.to_be_bytes());
    ihdr.extend_from_slice(&height.to_be_bytes());
    ihdr.extend_from_slice(&[8, 2, 0, 0, 0]);
    let mut bytes = b"\x89PNG\r\n\x1a\n".to_vec();
    bytes.extend_from_slice(&13_u32.to_be_bytes());
    bytes.extend_from_slice(&ihdr);
    bytes.extend_from_slice(&crc32(&ihdr).to_be_bytes());
    let padding = MAX_INLINE_IMAGE_BYTES + 1;
    bytes.extend_from_slice(&u32::try_from(padding).expect("len").to_be_bytes());
    bytes.extend_from_slice(b"IDAT");
    bytes.extend(std::iter::repeat_n(0_u8, padding));
    let mut document = document("bomb.png", "v1", "");
    document.reference.size = bytes.len() as u64;
    document.bytes = bytes;
    document
}

#[tokio::test(flavor = "multi_thread")]
async fn a_decompression_bomb_is_skipped_too_large_without_a_call_and_the_run_goes_on() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(describing);
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![bomb(100_000, 100_000), image("small.png", "png", "ok")],
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
async fn a_bmp_is_rasterised_to_png_and_described() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(describing);
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![image("scan.bmp", "bmp", "b")],
        None,
        &defaults(),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    let images = chat.state.images.lock().expect("images");
    assert_eq!(images[0][0].0, "image/png");
    assert!(images[0][0].1.starts_with(b"\x89PNG"));
}

#[tokio::test(flavor = "multi_thread")]
async fn a_nameless_download_is_planned_by_the_sniffed_type_whatever_alias_declared_it() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(describing);
    with_chat(&mut harness, &chat);
    let mut download = image("attachments/12345", "jpeg", "d");
    download.reference.mime = "image/jpg".to_owned();
    // The extension's block for the SNIFFED type (.jpg) governs it.
    let summary = run_model(
        &harness,
        vec![download],
        None,
        &parameters(
            false,
            json!({"chunking_config": {".jpg": {"prompt": "Name the people."}}}),
        ),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(
        chat.state.images.lock().expect("images")[0][0].0,
        "image/jpeg"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn only_the_extensions_own_block_sets_the_vision_window() {
    let mut harness = harness(4, None).await;
    let long = (0..120)
        .map(|n| format!("word{n}"))
        .collect::<Vec<_>>()
        .join(" ");
    let chat = ScriptedChat::new(move |_, _| Reply::Text(long.clone()));
    with_chat(&mut harness, &chat);
    // A top-level 8-token window is for text documents; the description of a
    // picture keeps the loader's 512.
    let summary = run_model(
        &harness,
        vec![image("a.png", "png", "x")],
        None,
        &parameters(
            false,
            json!({"chunking_config": {"max_tokens": 8, "token_overlap": 2}}),
        ),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_chunks, 1, "{summary:?}");
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
async fn use_llm_false_and_svg_are_unsupported_and_bmp_is_described() {
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
        Some(&2),
        "{summary:?}"
    );
    assert_eq!(summary.indexed_documents, 2, "{summary:?}");
    assert_eq!(chat.calls(), 2);

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
    assert_eq!(chat.calls(), 2, "no new call");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_vision_off_image_is_unsupported_whatever_its_bytes_are() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(describing);
    with_chat(&mut harness, &chat);
    let mut garbage = image("a.png", "png", "");
    garbage.bytes = b"this is not a png".to_vec();
    garbage.reference.size = garbage.bytes.len() as u64;
    let off = parameters(false, json!({"chunking_config": {"use_llm": false}}));
    let summary = run_model(
        &harness,
        vec![garbage.clone()],
        None,
        &off,
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.skipped.get("unsupported_type"),
        Some(&1),
        "{summary:?}"
    );
    assert_eq!(summary.skipped.get("unreadable"), None);
    // On, the same bytes are unreadable.
    let summary = run_model(
        &harness,
        vec![garbage],
        None,
        &defaults(),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.skipped.get("unreadable"), Some(&1), "{summary:?}");
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
async fn every_attempt_is_charged_and_an_exhausted_cap_skips_the_image_not_the_run() {
    // Two rate limits, then a description: three real calls against an image
    // budget of three (half of a cap of six). The next distinct image finds
    // nothing left: it is skipped `model_cap_exceeded` (a picture never stops
    // the run) and the next run retries it.
    let mut harness = harness(4, None).await;
    harness.runtime.limits.max_model_calls = 6;
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
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::PartlyIndexed as i32,
        "{summary:?}"
    );
    assert_eq!(
        summary.skipped.get("model_cap_exceeded"),
        Some(&1),
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

fn many_images(count: usize) -> Vec<Document> {
    (0..count)
        .map(|n| image(&format!("img-{n}.png"), "png", &format!("tag{n}")))
        .collect()
}

#[tokio::test(flavor = "multi_thread")]
async fn a_model_that_rejects_images_skips_each_vision_refused_and_the_run_goes_on() {
    // A text-only model / a provider 400: the gateway's generic rejection.
    // Six in a row, past the proposal chunker's streak (the breaker ends the calls after three), and the text
    // document after them is still indexed.
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(|_, _| {
        Reply::Fail(ErrorCategory::InvalidInput, "model_gateway.rejected")
    });
    with_chat(&mut harness, &chat);
    let mut documents = many_images(6);
    documents.push(document("notes.md", "v1", "# Notes\n\nPlain text.\n"));
    let summary = run_model(&harness, documents, None, &defaults(), Some("gpt-vision"))
        .await
        .expect("summary");
    assert_eq!(
        summary.skipped.get("vision_refused"),
        Some(&6),
        "{summary:?}"
    );
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    // Three refusals in a row mark the model as text-only: the other three
    // images are skipped without a call.
    assert_eq!(
        chat.calls(),
        3,
        "a refusal is not retried, then the breaker"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn credential_project_and_budget_refusals_still_stop_the_run() {
    for (category, code) in [
        (ErrorCategory::Unauthorized, "model_gateway.unauthorized"),
        (ErrorCategory::Forbidden, "model_gateway.forbidden"),
        (
            ErrorCategory::InvalidInput,
            "model_gateway.project_budget_exhausted",
        ),
    ] {
        let mut harness = harness(4, None).await;
        let chat = ScriptedChat::new(move |_, _| Reply::Fail(category, code));
        with_chat(&mut harness, &chat);
        let summary = run_model(
            &harness,
            many_images(3),
            None,
            &defaults(),
            Some("gpt-vision"),
        )
        .await
        .expect("summary");
        assert!(failed_with(&summary).contains(code), "{code}: {summary:?}");
        assert_eq!(chat.calls(), 1, "{code}");
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn a_request_over_the_gateways_size_bound_is_too_large_and_no_streak() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(|index, _| {
        if index < 5 {
            Reply::Fail(
                ErrorCategory::InvalidInput,
                "model_gateway.request_too_large",
            )
        } else {
            describing(index, "")
        }
    });
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        many_images(6),
        None,
        &defaults(),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.skipped.get("too_large"), Some(&5), "{summary:?}");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    // The model refused those pictures: lost work, not a limit of the worker.
    assert_eq!(
        summary.status,
        crate::protocol::elitea::runtime::v1::IndexIngestStatusV1::PartlyIndexed as i32,
        "{summary:?}"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn image_outages_skip_model_failed_and_never_stop_the_run() {
    // Six images whose every attempt is unavailable: past the proposal
    // chunker's streak of five, which images do not feed.
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(|_, _| {
        Reply::Fail(ErrorCategory::Unavailable, "model_gateway.unavailable")
    });
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        many_images(6),
        None,
        &defaults(),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.skipped.get("model_failed"), Some(&6), "{summary:?}");
    assert_ne!(summary.status, IndexIngestStatusV1::Error as i32);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_model_that_cannot_be_bound_skips_images_no_model_and_asks_once() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::unbindable();
    with_chat(&mut harness, &chat);
    let mut documents = many_images(3);
    documents.push(document("notes.md", "v1", "# Notes\n\nText.\n"));
    let summary = run_model(&harness, documents, None, &defaults(), Some("gpt-vision"))
        .await
        .expect("summary");
    assert_eq!(summary.skipped.get("no_model"), Some(&3), "{summary:?}");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(chat.calls(), 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_description_cut_off_by_max_tokens_is_kept() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(|_, _| Reply::Cut("# Chart\n\nThe bars rise and".to_owned()));
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        many_images(2),
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
    assert_eq!(summary.indexed_documents, 2, "{summary:?}");
    assert_eq!(chat.calls(), 2, "kept, not retried");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_success_resets_the_refused_streak() {
    // fail, fail, ok, fail, fail, ok, fail, fail: never three in a row.
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(|index, _| {
        if index % 3 == 2 {
            Reply::Text(DESCRIPTION.to_owned())
        } else {
            Reply::Fail(ErrorCategory::InvalidInput, "model_gateway.rejected")
        }
    });
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        many_images(8),
        None,
        &defaults(),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(chat.calls(), 8, "every image was asked");
    assert_eq!(
        summary.skipped.get("vision_refused"),
        Some(&6),
        "{summary:?}"
    );
    assert_eq!(summary.indexed_documents, 2, "{summary:?}");
}

#[tokio::test(flavor = "multi_thread")]
async fn no_model_means_no_decode() {
    // A big picture that would be downscaled, in a run with no chat model.
    let harness = harness(4, None).await;
    let big = real_image("big.png", ImageFormat::Png, 900, 700, "no-model");
    assert!(big.bytes.len() > MAX_INLINE_IMAGE_BYTES);
    let bytes = big.bytes.clone();
    let summary = run_model(&harness, vec![big], None, &defaults(), Some("gpt-vision"))
        .await
        .expect("summary");
    assert_eq!(summary.skipped.get("no_model"), Some(&1), "{summary:?}");
    assert_eq!(super::image_fit::decodes::of(&bytes), 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_duplicate_image_is_decoded_once() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(describing);
    with_chat(&mut harness, &chat);
    let first = real_image("a.png", ImageFormat::Png, 900, 700, "dup");
    let second = real_image("copy/b.png", ImageFormat::Png, 900, 700, "dup");
    assert_eq!(first.bytes, second.bytes);
    let bytes = first.bytes.clone();
    let summary = run_model(
        &harness,
        vec![first, second],
        None,
        &defaults(),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 2, "{summary:?}");
    assert_eq!(chat.calls(), 1);
    assert_eq!(super::image_fit::decodes::of(&bytes), 1, "one decode");
}

#[test]
fn images_may_use_half_of_the_cap() {
    use super::model_steps::image_cap;
    assert_eq!(image_cap(0), 0);
    assert_eq!(image_cap(1), 1);
    assert_eq!(image_cap(2), 1);
    assert_eq!(image_cap(5), 2);
    assert_eq!(image_cap(500), 250);
}

#[tokio::test(flavor = "multi_thread")]
async fn images_stop_at_half_the_cap_and_proposal_documents_proceed() {
    let mut harness = harness(4, None).await;
    harness.runtime.limits.max_model_calls = 500;
    let chat = ScriptedChat::new(|_, user| {
        if user == elitea_doc_chunk::image_processing_prompt() {
            Reply::Text(DESCRIPTION.to_owned())
        } else {
            Reply::Text(
                json!({"chunks": [{"chunk_title": "Food", "chunk_summary": "About food.",
                    "propositions": ["Greg likes pizza."]}]})
                .to_string(),
            )
        }
    });
    with_chat(&mut harness, &chat);
    let mut documents = many_images(600);
    for n in 0..5 {
        documents.push(document(
            &format!("guide-{n}.md"),
            "v1",
            &format!("Greg likes pizza {n}. Pizza is food."),
        ));
    }
    let summary = run_model(
        &harness,
        documents,
        None,
        &parameters(false, json!({"chunking_tool": "proposal"})),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.skipped.get("model_cap_exceeded"),
        Some(&350),
        "{summary:?}"
    );
    // 250 images + 5 proposal documents (one window each).
    assert_eq!(chat.calls(), 255);
    assert_eq!(summary.indexed_documents, 255, "{summary:?}");
}

mod lazy_entries {
    use std::sync::atomic::{AtomicUsize, Ordering};

    use async_trait::async_trait;

    use super::*;
    use crate::indexing::model_steps::ChatUnavailable;
    use crate::indexing::pipeline::{ChatEntries, ChatEntrySource};
    use crate::transport::input_content::MaterializedInput;

    /// Entries the claim would fetch, counted.
    struct Entries {
        loads: AtomicUsize,
        model: Option<&'static str>,
        configuration: Option<&'static str>,
        /// The fetch of the configuration entry fails.
        fail: bool,
    }

    impl Entries {
        fn ok() -> Self {
            Self {
                loads: AtomicUsize::new(0),
                model: Some("\"gpt-vision\""),
                configuration: Some("{\"temperature\": 0.2}"),
                fail: false,
            }
        }

        fn loads(&self) -> usize {
            self.loads.load(Ordering::SeqCst)
        }
    }

    #[async_trait]
    impl ChatEntrySource for Entries {
        async fn load(&self) -> Result<ChatEntries, ChatUnavailable> {
            self.loads.fetch_add(1, Ordering::SeqCst);
            if self.fail {
                return Err(ChatUnavailable);
            }
            let entry = |text: Option<&str>| {
                text.map(|text| MaterializedInput::for_test(text.as_bytes().to_vec()))
            };
            Ok(ChatEntries {
                model: entry(self.model),
                configuration: entry(self.configuration),
            })
        }
    }

    fn notes() -> Document {
        document("notes.md", "v1", "# Notes\n\nPlain text.\n")
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_run_that_never_needs_the_model_fetches_no_entry() {
        let mut harness = harness(4, None).await;
        let chat = ScriptedChat::new(describing);
        with_chat(&mut harness, &chat);
        let entries = Entries::ok();
        let summary = run_with_entries(&harness, vec![notes()], &defaults(), &entries)
            .await
            .expect("summary");
        assert_eq!(summary.indexed_documents, 1, "{summary:?}");
        assert_eq!(entries.loads(), 0, "text only: nothing fetched");
        assert_eq!(chat.calls(), 0);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn images_fetch_both_entries_once_on_first_need() {
        let mut harness = harness(4, None).await;
        let chat = ScriptedChat::new(describing);
        with_chat(&mut harness, &chat);
        let entries = Entries::ok();
        let mut documents = many_images(3);
        documents.push(notes());
        let summary = run_with_entries(&harness, documents, &defaults(), &entries)
            .await
            .expect("summary");
        assert_eq!(summary.indexed_documents, 4, "{summary:?}");
        assert_eq!(entries.loads(), 1);
        assert_eq!(chat.calls(), 3);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_failed_load_leaves_images_no_model_and_asks_once() {
        let mut harness = harness(4, None).await;
        let chat = ScriptedChat::new(describing);
        with_chat(&mut harness, &chat);
        let entries = Entries {
            fail: true,
            ..Entries::ok()
        };
        let mut documents = many_images(3);
        documents.push(notes());
        let summary = run_with_entries(&harness, documents, &defaults(), &entries)
            .await
            .expect("summary");
        assert_eq!(summary.skipped.get("no_model"), Some(&3), "{summary:?}");
        assert_eq!(summary.indexed_documents, 1, "{summary:?}");
        assert_eq!(entries.loads(), 1);
        assert_eq!(chat.calls(), 0);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn an_unreadable_configuration_is_no_model_never_default_settings() {
        let mut harness = harness(4, None).await;
        let chat = ScriptedChat::new(describing);
        with_chat(&mut harness, &chat);
        let entries = Entries {
            configuration: Some("[]"),
            ..Entries::ok()
        };
        let summary = run_with_entries(&harness, many_images(2), &defaults(), &entries)
            .await
            .expect("summary");
        assert_eq!(summary.skipped.get("no_model"), Some(&2), "{summary:?}");
        assert_eq!(chat.calls(), 0, "no call with default settings");
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_proposal_run_with_entries_it_cannot_read_fails_clearly() {
        let mut harness = harness(4, None).await;
        let chat = ScriptedChat::new(describing);
        with_chat(&mut harness, &chat);
        let entries = Entries {
            fail: true,
            ..Entries::ok()
        };
        let summary = run_with_entries(
            &harness,
            vec![document("guide.md", "v1", "Greg likes pizza.")],
            &parameters(false, json!({"chunking_tool": "proposal"})),
            &entries,
        )
        .await
        .expect("summary");
        let message = failed_with(&summary);
        assert!(message.contains("chat model entries"), "{message}");
        assert_eq!(entries.loads(), 1);
        assert_eq!(chat.calls(), 0);
    }
}
