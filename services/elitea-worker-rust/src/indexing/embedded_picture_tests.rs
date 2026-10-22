//! The pictures EMBEDDED in a PDF, DOCX or PPTX through a whole run
//! (ADR-0030 I4 part C): a scripted extraction sidecar returns a document's
//! text with pictures and the offsets where their descriptions belong, a
//! scripted vision `Llm` describes them, and the stored text is read back
//! from the `elitea-vector` fake.
//!
//! What they pin: the opt-in (`use_llm` on the extension, OFF by default as
//! in the SDK) and that an opt-out document asks the sidecar for no picture
//! at all and costs no model call; the inline formats; the shared cache (a
//! repeated picture costs one call, DOCX writes the SDK's back-reference);
//! the image budget; and the owner's rule that a picture never stops a run
//! or fails a document: no model, a refusing model, a spent budget and the
//! sidecar's own drops all leave the document indexed without that
//! description, named in the summary's message.

use std::sync::Arc;

use adk_rust::ErrorCategory;
use bytes::Bytes;
use elitea_content_source::Document;
use serde_json::{Value, json};

use super::model_step_tests::{
    Reply, ScriptedChat, failed_with, run_model, run_with_entries, search, search_host, with_chat,
};
use super::tests::{
    Harness, binary_document, document, fake_extractor, harness, parameters, socket_path,
    with_extractor,
};
use crate::protocol::elitea::runtime::v1::IndexIngestStatusV1;
use crate::protocol::extract::{EmbeddedImage, ExtractImages, ImagesSkipped};

const PDF_TEXT: &str = "Page: 1\nBefore.\n\nAfter.\n\nPage: 2\nSecond page.";

fn png(tag: &str) -> Vec<u8> {
    let mut bytes = b"\x89PNG\r\n\x1a\n".to_vec();
    bytes.extend_from_slice(tag.as_bytes());
    bytes
}

/// A picture the scripted sidecar returns.
fn picture(tag: &str, ordinal: u32, text_offset: usize, page: u32, name: &str) -> EmbeddedImage {
    EmbeddedImage {
        data: Bytes::from(png(tag)),
        media_type: "image/png".to_owned(),
        ordinal,
        text_offset: text_offset as u64,
        page,
        name: name.to_owned(),
    }
}

fn end_of(text: &str, needle: &str) -> usize {
    text.find(needle).expect("needle") + needle.len()
}

/// `use_llm` on for `extensions`.
fn on(extensions: &[&str]) -> Vec<u8> {
    let mut config = serde_json::Map::new();
    for extension in extensions {
        config.insert((*extension).to_owned(), json!({"use_llm": true}));
    }
    parameters(false, json!({"chunking_config": Value::Object(config)}))
}

/// A run with a scripted sidecar, a scripted chat (its reply is `reply(n)`
/// for the n-th call) and the sidecar's answer for pictures.
struct World {
    harness: Harness,
    fake: Arc<fake_extractor::Fake>,
    chat: Arc<ScriptedChat>,
    socket: std::path::PathBuf,
}

impl World {
    async fn new(
        name: &str,
        scripted: fake_extractor::ScriptedPictures,
        script: impl Fn(usize, &str) -> Reply + Send + Sync + 'static,
    ) -> Self {
        Self::with_chat(name, scripted, ScriptedChat::new(script)).await
    }

    async fn with_chat(
        name: &str,
        scripted: fake_extractor::ScriptedPictures,
        chat: Arc<ScriptedChat>,
    ) -> Self {
        let socket = socket_path(name);
        let fake = fake_extractor::serve(&socket);
        *fake.pictures.lock().expect("lock") = Some(scripted);
        let mut harness = with_extractor(harness(4, None).await, &socket);
        with_chat(&mut harness, &chat);
        Self {
            harness,
            fake,
            chat,
            socket,
        }
    }

    /// The text of every chunk stored for the one document, joined.
    async fn stored_text(self) -> String {
        let Harness {
            runtime,
            token,
            fake: vector,
            ..
        } = self.harness;
        assert!(vector.point_count() >= 1);
        let runtime = Arc::new(runtime);
        let host = search_host(&runtime, &token);
        let found = search(
            &host,
            json!({"query": "page", "index_name": "docs", "cut_off": 0,
                   "output_fields": ["page_content", "metadata"]}),
        )
        .await;
        let _ = std::fs::remove_file(&self.socket);
        found
            .as_array()
            .unwrap_or_else(|| panic!("{found}"))
            .iter()
            .filter_map(|hit| hit["page_content"].as_str())
            .collect::<Vec<_>>()
            .join("")
    }
}

fn scripted(
    text: &str,
    images: Vec<EmbeddedImage>,
    skipped: Option<ImagesSkipped>,
) -> fake_extractor::ScriptedPictures {
    fake_extractor::ScriptedPictures {
        text: text.to_owned(),
        images,
        skipped,
    }
}

fn numbered(index: usize, _: &str) -> Reply {
    Reply::Text(format!("DESCRIPTION-{index}"))
}

#[tokio::test(flavor = "multi_thread")]
async fn an_opted_in_pdf_has_its_pictures_described_at_the_end_of_their_pages() {
    let page_one = end_of(PDF_TEXT, "After.");
    let world = World::new(
        "emb-pdf",
        scripted(
            PDF_TEXT,
            vec![
                picture("one", 0, page_one, 1, ""),
                picture("two", 1, PDF_TEXT.len(), 2, ""),
            ],
            None,
        ),
        numbered,
    )
    .await;
    let summary = run_model(
        &world.harness,
        vec![binary_document("docs/report.pdf", b"%PDF-1.4 x")],
        None,
        &on(&[".pdf"]),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Ok as i32,
        "{summary:?}"
    );
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert!(summary.skipped.is_empty());
    assert!(
        !summary.message.contains("Embedded pictures"),
        "{summary:?}"
    );
    assert_eq!(world.chat.calls(), 2);
    // The sidecar was asked for pictures, with the run's bounds.
    assert_eq!(
        *world.fake.asked.lock().expect("lock"),
        vec![Some(ExtractImages {
            max_images: 20,
            max_image_bytes: 5 << 20,
            max_total_bytes: 12 << 20,
        })]
    );
    // The model got each picture's own bytes under the SDK's default prompt.
    let images = world.chat.state.images.lock().expect("images").clone();
    assert_eq!(
        images,
        vec![
            vec![("image/png".to_owned(), png("one"))],
            vec![("image/png".to_owned(), png("two"))],
        ]
    );
    let text = world.stored_text().await;
    let block =
        |n: usize| format!("\n**Image Transcript:**\nDESCRIPTION-{n}\n--------------------\n");
    let first = text.find(&format!("After.{}", block(0))).expect(&text);
    let second = text
        .find(&format!("Second page.{}", block(1)))
        .expect(&text);
    let page_two = text.find("Page: 2").expect(&text);
    assert!(first < page_two && page_two < second, "{text}");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_document_with_use_llm_off_asks_for_no_picture_and_costs_no_call() {
    let page_one = end_of(PDF_TEXT, "After.");
    let world = World::new(
        "emb-off",
        scripted(PDF_TEXT, vec![picture("one", 0, page_one, 1, "")], None),
        numbered,
    )
    .await;
    // Nothing configured: the SDK's default for documents is OFF.
    let summary = run_model(
        &world.harness,
        vec![binary_document("docs/report.pdf", b"%PDF-1.4 x")],
        None,
        &parameters(false, json!({})),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(world.chat.calls(), 0);
    assert_eq!(*world.fake.asked.lock().expect("lock"), vec![None]);
    let text = world.stored_text().await;
    assert!(!text.contains("Image Transcript"), "{text}");
    assert!(text.contains("Before.") && text.contains("Second page."));

    // Explicitly off for the extension, on at the top level: the extension's
    // own setting wins.
    let world = World::new(
        "emb-off2",
        scripted(PDF_TEXT, vec![picture("one", 0, page_one, 1, "")], None),
        numbered,
    )
    .await;
    let summary = run_model(
        &world.harness,
        vec![binary_document("docs/report.pdf", b"%PDF-1.4 x")],
        None,
        &parameters(
            false,
            json!({"chunking_config": {"use_llm": true, ".pdf": {"use_llm": false}}}),
        ),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(world.chat.calls(), 0);
    assert_eq!(*world.fake.asked.lock().expect("lock"), vec![None]);
    let _ = std::fs::remove_file(&world.socket);
}

#[tokio::test(flavor = "multi_thread")]
async fn opted_in_documents_without_pictures_are_chunked_exactly_as_before() {
    // The sidecar finds no picture: the text is the plain extraction's, no
    // model is asked, and the stored text is what an opt-out run stores.
    let one_run = |extra: Value| {
        let extra = extra.clone();
        async move {
            let world = World::new("emb-none", scripted(PDF_TEXT, vec![], None), numbered).await;
            let summary = run_model(
                &world.harness,
                vec![binary_document("docs/report.pdf", b"%PDF-1.4 x")],
                None,
                &parameters(false, extra),
                Some("gpt-vision"),
            )
            .await
            .expect("summary");
            assert_eq!(summary.indexed_documents, 1, "{summary:?}");
            assert_eq!(world.chat.calls(), 0);
            world.stored_text().await
        }
    };
    let plain = one_run(json!({})).await;
    let opted_in = one_run(json!({"chunking_config": {".pdf": {"use_llm": true}}})).await;
    assert_eq!(opted_in, plain);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_docx_description_follows_its_own_line_names_the_file_and_a_repeat_is_a_back_reference() {
    let text = "Intro.\n\n[Image: Picture 1]\n\nMiddle.\n\n[Image: Picture 2]\n\nOutro.";
    let (one, two) = (
        end_of(text, "[Image: Picture 1]"),
        end_of(text, "[Image: Picture 2]"),
    );
    let world = World::new(
        "emb-docx",
        // The same bytes twice: the second is the SDK's back-reference.
        scripted(
            text,
            vec![
                picture("same", 0, one, 0, "image1.png"),
                picture("same", 1, two, 0, "image2.png"),
            ],
            None,
        ),
        numbered,
    )
    .await;
    let summary = run_model(
        &world.harness,
        vec![binary_document("docs/spec.docx", b"PK\x03\x04 x")],
        None,
        &on(&[".docx"]),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(world.chat.calls(), 1, "one call for two copies");
    let stored = world.stored_text().await;
    let normalised = stored.replace("\n\n\n", "\n\n");
    let first = "[Image: Picture 1]\n**Image Transcript:**\nImage: image1.png, DESCRIPTION-0\n";
    let second = "[Image: Picture 2]\n**Image Transcript:**\nImage: image2.png [already transcribed, see above]\n";
    let a = normalised.find(first).unwrap_or_else(|| panic!("{stored}"));
    let b = normalised
        .find(second)
        .unwrap_or_else(|| panic!("{stored}"));
    assert!(a < b, "{stored}");
    assert!(normalised.contains("Outro."), "{stored}");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_picture_repeated_in_a_pdf_is_one_model_call_and_the_cache_hit_is_not_charged() {
    let page_one = end_of(PDF_TEXT, "After.");
    let mut world = World::new(
        "emb-cache",
        scripted(
            PDF_TEXT,
            vec![
                picture("same", 0, page_one, 1, ""),
                picture("same", 1, PDF_TEXT.len(), 2, ""),
            ],
            None,
        ),
        numbered,
    )
    .await;
    // A cap of ONE call: a charged cache hit would stop the second picture.
    world.harness.runtime.limits.max_model_calls = 1;
    let summary = run_model(
        &world.harness,
        vec![binary_document("docs/report.pdf", b"%PDF-1.4 x")],
        None,
        &on(&[".pdf"]),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Ok as i32,
        "{summary:?}"
    );
    assert_eq!(world.chat.calls(), 1);
    assert!(
        !summary.message.contains("Embedded pictures"),
        "{summary:?}"
    );
    let text = world.stored_text().await;
    assert_eq!(text.matches("DESCRIPTION-0").count(), 2, "{text}");
}

#[tokio::test(flavor = "multi_thread")]
async fn without_a_chat_model_the_pictures_are_skipped_and_the_document_is_still_indexed() {
    let page_one = end_of(PDF_TEXT, "After.");
    let world = World::new(
        "emb-nomodel",
        scripted(
            PDF_TEXT,
            vec![
                picture("one", 0, page_one, 1, ""),
                picture("two", 1, PDF_TEXT.len(), 2, ""),
            ],
            None,
        ),
        numbered,
    )
    .await;
    // No `llm_model` entry at all.
    let summary = run_model(
        &world.harness,
        vec![binary_document("docs/report.pdf", b"%PDF-1.4 x")],
        None,
        &on(&[".pdf"]),
        None,
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Ok as i32,
        "{summary:?}"
    );
    assert_eq!(summary.indexed_documents, 1);
    assert!(summary.skipped.is_empty(), "{:?}", summary.skipped);
    // Nothing could be described, so no picture was even asked of the
    // sidecar: the document is named in the message, not its pictures.
    assert!(
        summary
            .message
            .contains("Embedded pictures not requested for documents: 1 no_model."),
        "{summary:?}"
    );
    assert_eq!(*world.fake.asked.lock().expect("lock"), vec![None]);
    assert_eq!(world.chat.calls(), 0);
    // A final reason: the plain source version is stored, never retried.
    assert_eq!(summary.documents[0].version, "v1");
    let text = world.stored_text().await;
    assert!(!text.contains("Image Transcript"), "{text}");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_model_that_refuses_pictures_never_fails_the_document_or_the_run() {
    let page_one = end_of(PDF_TEXT, "After.");
    let images: Vec<EmbeddedImage> = (0..5)
        .map(|n| picture(&format!("p{n}"), n, page_one, 1, ""))
        .collect();
    let world = World::new("emb-refuse", scripted(PDF_TEXT, images, None), |_, _| {
        Reply::Fail(ErrorCategory::InvalidInput, "model_gateway.rejected")
    })
    .await;
    let summary = run_model(
        &world.harness,
        vec![
            binary_document("docs/report.pdf", b"%PDF-1.4 x"),
            document("README.md", "v1", "# Title\n\nWords.\n"),
        ],
        None,
        &on(&[".pdf"]),
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
    assert!(summary.skipped.is_empty());
    // Three refusals trip the breaker; the other two are skipped without a call.
    assert_eq!(world.chat.calls(), 3);
    assert!(
        summary
            .message
            .contains("Embedded pictures not described: 5 vision_refused."),
        "{summary:?}"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn the_image_budget_is_half_the_cap_and_a_spent_budget_skips_the_rest() {
    let page_one = end_of(PDF_TEXT, "After.");
    let images: Vec<EmbeddedImage> = (0..4)
        .map(|n| picture(&format!("p{n}"), n, page_one, 1, ""))
        .collect();
    let mut world = World::new("emb-budget", scripted(PDF_TEXT, images, None), numbered).await;
    // A cap of 4 calls: images may use at most 2.
    world.harness.runtime.limits.max_model_calls = 4;
    let summary = run_model(
        &world.harness,
        vec![binary_document("docs/report.pdf", b"%PDF-1.4 x")],
        None,
        &on(&[".pdf"]),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Ok as i32,
        "{summary:?}"
    );
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(world.chat.calls(), 2);
    assert!(
        summary
            .message
            .contains("Embedded pictures not described: 2 model_cap_exceeded."),
        "{summary:?}"
    );
    let text = world.stored_text().await;
    assert_eq!(text.matches("Image Transcript").count(), 2, "{text}");
}

#[tokio::test(flavor = "multi_thread")]
async fn what_the_sidecar_dropped_is_counted_and_the_document_is_indexed() {
    let page_one = end_of(PDF_TEXT, "After.");
    let world = World::new(
        "emb-drops",
        scripted(
            PDF_TEXT,
            vec![picture("one", 0, page_one, 1, "")],
            Some(ImagesSkipped {
                too_many: 2,
                too_large: 1,
                unsupported: 3,
            }),
        ),
        numbered,
    )
    .await;
    let summary = run_model(
        &world.harness,
        vec![binary_document("docs/report.pdf", b"%PDF-1.4 x")],
        None,
        &on(&[".pdf"]),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Ok as i32,
        "{summary:?}"
    );
    assert_eq!(summary.indexed_documents, 1);
    assert!(
        summary.message.contains(
            "Embedded pictures not described: 1 too_large, 2 too_many_pictures, 3 unsupported_type."
        ),
        "{summary:?}"
    );
    assert_eq!(world.chat.calls(), 1);
}

#[tokio::test(flavor = "multi_thread")]
async fn only_the_credential_the_project_and_the_budget_stop_the_run() {
    let page_one = end_of(PDF_TEXT, "After.");
    let world = World::new(
        "emb-stop",
        scripted(PDF_TEXT, vec![picture("one", 0, page_one, 1, "")], None),
        |_, _| Reply::Fail(ErrorCategory::Forbidden, "model_gateway.forbidden"),
    )
    .await;
    let summary = run_model(
        &world.harness,
        vec![binary_document("docs/report.pdf", b"%PDF-1.4 x")],
        None,
        &on(&[".pdf"]),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert!(
        failed_with(&summary).contains("model_gateway.forbidden"),
        "{summary:?}"
    );
    let _ = std::fs::remove_file(&world.socket);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_pptx_is_opted_in_per_extension_and_other_types_never_ask() {
    let text = "Slide one\n\nSlide one tail";
    let world = World::new(
        "emb-pptx",
        scripted(
            text,
            vec![picture("deck", 0, text.len(), 1, "image1.png")],
            None,
        ),
        numbered,
    )
    .await;
    // `.pdf` is on, `.pptx` is not: the deck asks for nothing.
    let summary = run_model(
        &world.harness,
        vec![binary_document("decks/a.pptx", b"PK\x03\x04 x")],
        None,
        &on(&[".pdf"]),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(*world.fake.asked.lock().expect("lock"), vec![None]);
    assert_eq!(world.chat.calls(), 0);

    // On for `.pptx`, and the spreadsheet next to it still asks for nothing.
    let summary = run_model(
        &world.harness,
        vec![
            binary_document("decks/b.pptx", b"PK\x03\x04 y"),
            binary_document("sheets/c.xlsx", b"PK\x03\x04 z"),
        ],
        None,
        &on(&[".pptx", ".xlsx"]),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 2, "{summary:?}");
    let asked = world.fake.asked.lock().expect("lock").clone();
    assert_eq!(asked.iter().filter(|asked| asked.is_some()).count(), 1);
    assert_eq!(world.chat.calls(), 1);
    let _ = std::fs::remove_file(&world.socket);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_big_embedded_picture_is_downscaled_to_the_gateways_bound_like_an_image_file() {
    use crate::transport::openai_compatible_facade::MAX_INLINE_IMAGE_BYTES;
    let big = super::vision_tests::real_image("big.png", image::ImageFormat::Png, 900, 700, "b");
    assert!(big.bytes.len() > MAX_INLINE_IMAGE_BYTES);
    let page_one = end_of(PDF_TEXT, "After.");
    let world = World::new(
        "emb-big",
        scripted(
            PDF_TEXT,
            vec![EmbeddedImage {
                data: Bytes::from(big.bytes.clone()),
                media_type: "image/png".to_owned(),
                ordinal: 0,
                text_offset: page_one as u64,
                page: 1,
                name: String::new(),
            }],
            None,
        ),
        numbered,
    )
    .await;
    let summary = run_model(
        &world.harness,
        vec![binary_document("docs/report.pdf", b"%PDF-1.4 x")],
        None,
        &on(&[".pdf"]),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert!(
        !summary.message.contains("Embedded pictures"),
        "{summary:?}"
    );
    let images = world.chat.state.images.lock().expect("images").clone();
    assert_eq!(images.len(), 1);
    let (media, bytes) = &images[0][0];
    assert!(bytes.len() <= MAX_INLINE_IMAGE_BYTES, "{}", bytes.len());
    assert!(matches!(media.as_str(), "image/png" | "image/jpeg"));
    let text = world.stored_text().await;
    assert!(text.contains("DESCRIPTION-0"), "{text}");
}

// ── review round 1 ──────────────────────────────────────────────────────────

fn four_pictures(page_one: usize) -> Vec<EmbeddedImage> {
    (0..4)
        .map(|n| picture(&format!("p{n}"), n, page_one, 1, ""))
        .collect()
}

#[tokio::test(flavor = "multi_thread")]
async fn a_picture_skipped_for_a_transient_reason_is_retried_by_the_next_run() {
    let page_one = end_of(PDF_TEXT, "After.");
    let mut world = World::new(
        "emb-retry",
        scripted(PDF_TEXT, four_pictures(page_one), None),
        numbered,
    )
    .await;
    let pdf = || vec![binary_document("docs/report.pdf", b"%PDF-1.4 x")];
    // A cap of 4 calls: images may use 2; the other two are skipped
    // `model_cap_exceeded`, a reason the next run cures.
    world.harness.runtime.limits.max_model_calls = 4;
    let first = run_model(&world.harness, pdf(), None, &on(&[".pdf"]), Some("m"))
        .await
        .expect("summary");
    assert_eq!(first.indexed_documents, 1, "{first:?}");
    assert!(
        first
            .message
            .contains("Embedded pictures not described: 2 model_cap_exceeded."),
        "{first:?}"
    );
    // Written (its text is valuable) under a version no source lists.
    assert_eq!(
        first.documents[0].version, "v1#pictures-pending-1",
        "{first:?}"
    );
    assert_eq!(world.chat.calls(), 2);

    // The next run finds it changed, describes all four, and stores the
    // plain source version.
    world.harness.runtime.limits.max_model_calls = 100;
    let second = run_model(&world.harness, pdf(), None, &on(&[".pdf"]), Some("m"))
        .await
        .expect("summary");
    assert_eq!(
        (
            second.indexed_documents,
            second.updated,
            second.skipped.len()
        ),
        (1, 1, 0),
        "{second:?}"
    );
    assert_eq!(second.documents[0].version, "v1", "{second:?}");
    assert!(!second.message.contains("Embedded pictures"), "{second:?}");
    assert_eq!(world.chat.calls(), 2 + 4);
    assert_eq!(second.total_documents, Some(1));

    // Now it is settled: unchanged, no model call.
    let third = run_model(&world.harness, pdf(), None, &on(&[".pdf"]), Some("m"))
        .await
        .expect("summary");
    assert_eq!(third.indexed_documents, 0, "{third:?}");
    assert!(third.message.contains("1 unchanged"), "{third:?}");
    assert_eq!(world.chat.calls(), 2 + 4);
    let text = world.stored_text().await;
    assert_eq!(text.matches("Image Transcript").count(), 4, "{text}");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_picture_skipped_for_a_final_reason_is_not_retried() {
    let page_one = end_of(PDF_TEXT, "After.");
    // The sidecar drops pictures (too large, too many, not a format a model
    // takes): no later run would get them either.
    let world = World::new(
        "emb-final",
        scripted(
            PDF_TEXT,
            vec![picture("one", 0, page_one, 1, "")],
            Some(ImagesSkipped {
                too_many: 1,
                too_large: 1,
                unsupported: 1,
            }),
        ),
        numbered,
    )
    .await;
    let pdf = || vec![binary_document("docs/report.pdf", b"%PDF-1.4 x")];
    let first = run_model(&world.harness, pdf(), None, &on(&[".pdf"]), Some("m"))
        .await
        .expect("summary");
    assert_eq!(first.documents[0].version, "v1", "{first:?}");
    let second = run_model(&world.harness, pdf(), None, &on(&[".pdf"]), Some("m"))
        .await
        .expect("summary");
    assert_eq!(second.indexed_documents, 0, "{second:?}");
    assert!(second.message.contains("1 unchanged"), "{second:?}");
    assert_eq!(world.chat.calls(), 1);
    let _ = std::fs::remove_file(&world.socket);

    // A model that refuses pictures: `vision_refused` is final too.
    let images: Vec<EmbeddedImage> = (0..4)
        .map(|n| picture(&format!("r{n}"), n, page_one, 1, ""))
        .collect();
    let world = World::new("emb-final2", scripted(PDF_TEXT, images, None), |_, _| {
        Reply::Fail(ErrorCategory::InvalidInput, "model_gateway.rejected")
    })
    .await;
    let first = run_model(&world.harness, pdf(), None, &on(&[".pdf"]), Some("m"))
        .await
        .expect("summary");
    assert_eq!(first.documents[0].version, "v1", "{first:?}");
    let second = run_model(&world.harness, pdf(), None, &on(&[".pdf"]), Some("m"))
        .await
        .expect("summary");
    assert_eq!(second.indexed_documents, 0, "{second:?}");
    let _ = std::fs::remove_file(&world.socket);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_model_failure_marks_the_document_and_the_retry_mark_never_reaches_the_source() {
    let page_one = end_of(PDF_TEXT, "After.");
    let world = World::new(
        "emb-failed",
        scripted(PDF_TEXT, vec![picture("one", 0, page_one, 1, "")], None),
        |_, _| Reply::Fail(ErrorCategory::Unavailable, "model_gateway.unavailable"),
    )
    .await;
    let mut world = world;
    world.harness.runtime.limits.embedding_retry_backoff = std::time::Duration::ZERO;
    let first = run_model(
        &world.harness,
        vec![binary_document("docs/report.pdf", b"%PDF-1.4 x")],
        None,
        &on(&[".pdf"]),
        Some("m"),
    )
    .await
    .expect("summary");
    assert_eq!(first.indexed_documents, 1, "{first:?}");
    assert_eq!(
        first.documents[0].version, "v1#pictures-pending-1",
        "{first:?}"
    );
    assert!(first.message.contains("model_failed"), "{first:?}");
    // The stored version is what the diff compares; the source's own is
    // what a connector is told it holds.
    assert_eq!(
        super::pipeline::source_version_of("v1#pictures-pending-1"),
        "v1"
    );
    // A mark without a count is a first run (stored by the earlier build).
    assert_eq!(
        super::pipeline::source_version_of("v1#pictures-pending"),
        "v1"
    );
    assert_eq!(
        super::pipeline::source_version_of("v1#pictures-pending-x"),
        "v1#pictures-pending-x"
    );
    let _ = std::fs::remove_file(&world.socket);
}

#[tokio::test(flavor = "multi_thread")]
async fn pictures_are_described_a_few_at_a_time_and_applied_in_document_order() {
    // Eight distinct pictures at one place; the earlier the picture, the
    // slower the model, so completion order is the reverse of document order.
    let page_one = end_of(PDF_TEXT, "After.");
    let images: Vec<EmbeddedImage> = (0..8)
        .map(|n| picture(&format!("q{n}"), n, page_one, 1, ""))
        .collect();
    let chat = ScriptedChat::by_image(|images| {
        let bytes = &images[0].1;
        let tag = String::from_utf8_lossy(&bytes[8..]).into_owned();
        let n: u64 = tag[1..].parse().expect("tag");
        ((8 - n) * 25, Reply::Text(format!("DESC-{tag}")))
    });
    let world = World::with_chat("emb-order", scripted(PDF_TEXT, images, None), chat).await;
    let summary = run_model(
        &world.harness,
        vec![binary_document("docs/report.pdf", b"%PDF-1.4 x")],
        None,
        &on(&[".pdf"]),
        Some("m"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.documents[0].version, "v1", "{summary:?}");
    assert_eq!(world.chat.calls(), 8);
    let in_flight = world
        .chat
        .state
        .max_in_flight
        .load(std::sync::atomic::Ordering::SeqCst);
    assert!(
        (2..=super::pipeline::PICTURES_IN_FLIGHT_FOR_TESTS).contains(&in_flight),
        "{in_flight}"
    );
    let text = world.stored_text().await;
    let at: Vec<usize> = (0..8)
        .map(|n| text.find(&format!("DESC-q{n}")).expect(&text))
        .collect();
    assert!(at.windows(2).all(|w| w[0] < w[1]), "{at:?}\n{text}");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_blank_reply_is_the_placeholder_and_a_back_reference_follows_a_written_transcript() {
    let text = "Intro.\n\n[Image: Picture 1]\n\nMiddle.\n\n[Image: Picture 2]\n\nOutro.";
    let (one, two) = (
        end_of(text, "[Image: Picture 1]"),
        end_of(text, "[Image: Picture 2]"),
    );
    let world = World::new(
        "emb-blank",
        scripted(
            text,
            vec![
                picture("same", 0, one, 0, "image1.png"),
                picture("same", 1, two, 0, "image2.png"),
            ],
            None,
        ),
        |_, _| Reply::Text("   ".to_owned()),
    )
    .await;
    let summary = run_model(
        &world.harness,
        vec![binary_document("docs/spec.docx", b"PK\x03\x04 x")],
        None,
        &on(&[".docx"]),
        Some("m"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(world.chat.calls(), 1);
    let stored = world.stored_text().await;
    // The same sentence an image file with a blank reply is stored with.
    assert!(
        stored.contains("Image: image1.png, [No readable text detected by LLM]"),
        "{stored}"
    );
    // The back-reference points at a transcript that really was written.
    assert!(
        stored.contains("Image: image2.png [already transcribed, see above]"),
        "{stored}"
    );

    // A PDF with a blank reply writes the placeholder block too.
    let page_one = end_of(PDF_TEXT, "After.");
    let world = World::new(
        "emb-blank2",
        scripted(PDF_TEXT, vec![picture("x", 0, page_one, 1, "")], None),
        |_, _| Reply::Text(String::new()),
    )
    .await;
    run_model(
        &world.harness,
        vec![binary_document("docs/report.pdf", b"%PDF-1.4 x")],
        None,
        &on(&[".pdf"]),
        Some("m"),
    )
    .await
    .expect("summary");
    let stored = world.stored_text().await;
    assert!(
        stored.contains("**Image Transcript:**\n[No readable text detected by LLM]\n"),
        "{stored}"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn a_key_without_an_extension_is_planned_by_the_media_type_it_arrived_as() {
    let page_one = end_of(PDF_TEXT, "After.");
    let world = World::new(
        "emb-mime",
        scripted(PDF_TEXT, vec![picture("one", 0, page_one, 1, "")], None),
        numbered,
    )
    .await;
    let mut download = binary_document("downloads/report", b"%PDF-1.4 x");
    download.reference.mime = "application/pdf".to_owned();
    let summary = run_model(
        &world.harness,
        vec![download],
        None,
        &on(&[".pdf"]),
        Some("m"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    // The settings are the media type's extension's: `.pdf` is on.
    assert_eq!(
        world
            .fake
            .asked
            .lock()
            .expect("lock")
            .iter()
            .filter(|a| a.is_some())
            .count(),
        1
    );
    assert_eq!(world.chat.calls(), 1);
    let text = world.stored_text().await;
    assert!(text.contains("DESCRIPTION-0"), "{text}");
}

// ── review round 2 ──────────────────────────────────────────────────────────

fn pdf() -> Vec<Document> {
    vec![binary_document("docs/report.pdf", b"%PDF-1.4 x")]
}

#[tokio::test(flavor = "multi_thread")]
async fn a_document_of_pictures_alone_is_indexed_from_their_descriptions_when_they_are_asked_for() {
    let scan = || {
        scripted(
            "",
            vec![
                picture("scan-one", 0, 0, 1, ""),
                picture("scan-two", 1, 0, 2, ""),
            ],
            None,
        )
    };
    let world = World::new("emb-scan", scan(), numbered).await;
    let summary = run_model(&world.harness, pdf(), None, &on(&[".pdf"]), Some("m"))
        .await
        .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert!(summary.skipped.is_empty(), "{:?}", summary.skipped);
    assert_eq!(world.chat.calls(), 2);
    let text = world.stored_text().await;
    assert!(
        text.contains("DESCRIPTION-0") && text.contains("DESCRIPTION-1"),
        "{text}"
    );

    // `use_llm` off: nothing is asked for, there is no text, and the
    // document is skipped `empty` as before.
    let world = World::new("emb-scan-off", scan(), numbered).await;
    let summary = run_model(
        &world.harness,
        pdf(),
        None,
        &parameters(false, json!({})),
        Some("m"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 0, "{summary:?}");
    assert_eq!(summary.skipped.get("empty"), Some(&1), "{summary:?}");
    assert_eq!(world.chat.calls(), 0);
    let _ = std::fs::remove_file(&world.socket);

    // Asked for, but every picture fails for good: nothing to index, and
    // the pictures of a document that was not indexed are not reported.
    let world = World::new("emb-scan-fail", scan(), |_, _| Reply::Declined).await;
    let summary = run_model(&world.harness, pdf(), None, &on(&[".pdf"]), Some("m"))
        .await
        .expect("summary");
    assert_eq!(summary.indexed_documents, 0, "{summary:?}");
    assert_eq!(summary.skipped.get("empty"), Some(&1), "{summary:?}");
    assert!(
        !summary.message.contains("Embedded pictures"),
        "a document that was not indexed reports no picture: {summary:?}"
    );
    let _ = std::fs::remove_file(&world.socket);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_reply_the_model_will_not_change_is_final_and_never_marks_the_document() {
    let page_one = end_of(PDF_TEXT, "After.");
    let world = World::new(
        "emb-declined",
        scripted(PDF_TEXT, vec![picture("one", 0, page_one, 1, "")], None),
        |_, _| Reply::Declined,
    )
    .await;
    let first = run_model(&world.harness, pdf(), None, &on(&[".pdf"]), Some("m"))
        .await
        .expect("summary");
    assert_eq!(first.indexed_documents, 1, "{first:?}");
    assert_eq!(first.documents[0].version, "v1", "{first:?}");
    assert!(first.message.contains("1 reply_unusable"), "{first:?}");
    let second = run_model(&world.harness, pdf(), None, &on(&[".pdf"]), Some("m"))
        .await
        .expect("summary");
    assert_eq!(second.indexed_documents, 0, "{second:?}");
    assert!(second.message.contains("1 unchanged"), "{second:?}");
    assert_eq!(world.chat.calls(), 1, "not asked again");
    let _ = std::fs::remove_file(&world.socket);
}

/// A picture whose reply was only reasoning cut by `max_tokens` is a final
/// skip (`reasoning_exhausted_tokens`): the same `max_tokens` would be cut
/// the same way, so the document is NOT marked `#pictures-pending`, the
/// next run does not ask again, and the summary says to raise `max_tokens`.
#[tokio::test(flavor = "multi_thread")]
async fn reasoning_that_exhausts_max_tokens_is_final_and_never_marks_the_document() {
    let page_one = end_of(PDF_TEXT, "After.");
    let world = World::new(
        "emb-reasoning",
        scripted(PDF_TEXT, vec![picture("one", 0, page_one, 1, "")], None),
        |_, _| Reply::Cut("<think>still thinking".to_owned()),
    )
    .await;
    let first = run_model(&world.harness, pdf(), None, &on(&[".pdf"]), Some("m"))
        .await
        .expect("summary");
    assert_eq!(first.indexed_documents, 1, "{first:?}");
    assert_eq!(first.documents[0].version, "v1", "never #pictures-pending");
    assert_eq!(
        first.status,
        IndexIngestStatusV1::Ok as i32,
        "benign for the run: {first:?}"
    );
    assert!(
        first.message.contains("1 reasoning_exhausted_tokens"),
        "{first:?}"
    );
    assert!(first.message.contains("raise max_tokens"), "{first:?}");
    let second = run_model(&world.harness, pdf(), None, &on(&[".pdf"]), Some("m"))
        .await
        .expect("summary");
    assert_eq!(second.indexed_documents, 0, "{second:?}");
    assert_eq!(world.chat.calls(), 1, "not asked again");
    let _ = std::fs::remove_file(&world.socket);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_transient_failure_marks_the_document_for_a_bounded_number_of_runs() {
    let page_one = end_of(PDF_TEXT, "After.");
    let mut world = World::new(
        "emb-bounded",
        scripted(PDF_TEXT, vec![picture("one", 0, page_one, 1, "")], None),
        |_, _| Reply::Fail(ErrorCategory::Unavailable, "model_gateway.unavailable"),
    )
    .await;
    world.harness.runtime.limits.embedding_retry_backoff = std::time::Duration::ZERO;
    // Runs 1..=3 carry the mark with their count; the 4th stores the plain
    // source version and the document is settled.
    for (run, version) in [
        "v1#pictures-pending-1",
        "v1#pictures-pending-2",
        "v1#pictures-pending-3",
        "v1",
    ]
    .into_iter()
    .enumerate()
    {
        let summary = run_model(&world.harness, pdf(), None, &on(&[".pdf"]), Some("m"))
            .await
            .expect("summary");
        assert_eq!(summary.indexed_documents, 1, "run {run}: {summary:?}");
        assert_eq!(summary.documents[0].version, version, "run {run}");
        // Each run reports its own outcome, once.
        assert_eq!(
            summary.message.matches("model_failed").count(),
            1,
            "run {run}: {summary:?}"
        );
    }
    let settled = run_model(&world.harness, pdf(), None, &on(&[".pdf"]), Some("m"))
        .await
        .expect("summary");
    assert_eq!(settled.indexed_documents, 0, "{settled:?}");
    assert!(settled.message.contains("1 unchanged"), "{settled:?}");
    let _ = std::fs::remove_file(&world.socket);
}

#[test]
fn the_retry_mark_carries_its_count_and_the_old_mark_is_a_first_run() {
    use super::pipeline::split_version_for_tests as split;
    assert_eq!(split("v1"), ("v1", 0));
    assert_eq!(split("v1#pictures-pending"), ("v1", 1));
    assert_eq!(split("v1#pictures-pending-2"), ("v1", 2));
    assert_eq!(split("v1#pictures-pending-0"), ("v1#pictures-pending-0", 0));
    assert_eq!(split("v1#pictures-pending-x"), ("v1#pictures-pending-x", 0));
}

#[tokio::test(flavor = "multi_thread")]
async fn a_docx_back_reference_follows_the_order_the_blocks_are_written_not_the_pictures_number() {
    let text = "Intro.\n\n[Image: Picture 1]\n\nMiddle.\n\n[Image: Picture 2]\n\nOutro.";
    let (one, two) = (
        end_of(text, "[Image: Picture 1]"),
        end_of(text, "[Image: Picture 2]"),
    );
    // The parser numbered the LATER place first: ordinal 0 is written
    // second. The description goes where it is written first.
    let world = World::new(
        "emb-reorder",
        scripted(
            text,
            vec![
                picture("same", 0, two, 0, "image1.png"),
                picture("same", 1, one, 0, "image2.png"),
            ],
            None,
        ),
        numbered,
    )
    .await;
    let summary = run_model(
        &world.harness,
        vec![binary_document("docs/spec.docx", b"PK\x03\x04 x")],
        None,
        &on(&[".docx"]),
        Some("m"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(world.chat.calls(), 1);
    let stored = world.stored_text().await;
    let normalised = stored.replace("\n\n\n", "\n\n");
    let described = "[Image: Picture 1]\n**Image Transcript:**\nImage: image2.png, DESCRIPTION-0\n";
    let referenced = "[Image: Picture 2]\n**Image Transcript:**\nImage: image1.png [already transcribed, see above]\n";
    let a = normalised
        .find(described)
        .unwrap_or_else(|| panic!("{stored}"));
    let b = normalised
        .find(referenced)
        .unwrap_or_else(|| panic!("{stored}"));
    assert!(a < b, "the reference points up: {stored}");
}

/// Entries that take a while to fetch: the chat model's load.
struct SlowEntries {
    delay: std::time::Duration,
    finished: std::sync::Mutex<Option<std::time::Instant>>,
}

#[async_trait::async_trait]
impl crate::indexing::pipeline::ChatEntrySource for SlowEntries {
    async fn load(
        &self,
    ) -> Result<crate::indexing::pipeline::ChatEntries, crate::indexing::model_steps::ChatUnavailable>
    {
        tokio::time::sleep(self.delay).await;
        *self.finished.lock().expect("lock") = Some(std::time::Instant::now());
        Ok(crate::indexing::pipeline::ChatEntries {
            model: Some(
                crate::transport::input_content::MaterializedInput::for_test(
                    b"\"gpt-vision\"".to_vec(),
                ),
            ),
            configuration: None,
        })
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn a_slow_chat_model_load_does_not_hold_up_the_documents_that_need_no_picture() {
    let page_one = end_of(PDF_TEXT, "After.");
    let world = World::new(
        "emb-slow",
        scripted(PDF_TEXT, vec![picture("one", 0, page_one, 1, "")], None),
        numbered,
    )
    .await;
    let entries = SlowEntries {
        delay: std::time::Duration::from_millis(700),
        finished: std::sync::Mutex::new(None),
    };
    // The PDF is first and needs the model; the deck and the sheet do not
    // (`use_llm` is on for `.pdf` only).
    let summary = run_with_entries(
        &world.harness,
        vec![
            binary_document("docs/report.pdf", b"%PDF-1.4 x"),
            binary_document("decks/b.pptx", b"PK\x03\x04 y"),
            binary_document("sheets/c.xlsx", b"PK\x03\x04 z"),
        ],
        &on(&[".pdf"]),
        &entries,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 3, "{summary:?}");
    let loaded = entries.finished.lock().expect("lock").expect("loaded");
    let arrived = world.fake.arrived.lock().expect("lock").clone();
    let at = |name: &str| {
        arrived
            .iter()
            .find(|(file, _)| file == name)
            .unwrap_or_else(|| panic!("{name} never reached the extractor: {arrived:?}"))
            .1
    };
    // The others were fetched and extracted while the model loaded; the
    // PDF's pictures were asked for only once it had.
    assert!(at("decks/b.pptx") < loaded, "the deck waited for the model");
    assert!(
        at("sheets/c.xlsx") < loaded,
        "the sheet waited for the model"
    );
    assert!(at("docs/report.pdf") >= loaded, "the PDF asked too early");
    let asked = world.fake.asked.lock().expect("lock").clone();
    assert_eq!(asked.iter().filter(|a| a.is_some()).count(), 1);
    assert_eq!(world.chat.calls(), 1);
    let _ = std::fs::remove_file(&world.socket);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_documents_pictures_are_hashed_together_and_in_order() {
    use super::image_fit::{digest, digests};
    let (a, b) = (Bytes::from_static(b"alpha"), Bytes::from_static(b"beta"));
    let all = digests(vec![a.clone(), b.clone(), a.clone()]).await;
    assert_eq!(all.len(), 3);
    assert_eq!(all[0], digest(a).await);
    assert_eq!(all[1], digest(b).await);
    assert_eq!(all[0], all[2]);
    assert_ne!(all[0], all[1]);
    assert_eq!(
        digests(Vec::new()).await,
        [] as [std::option::Option<[u8; 32]>; 0]
    );
}
