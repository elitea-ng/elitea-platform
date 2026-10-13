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
use serde_json::{Value, json};

use super::model_step_tests::{
    Reply, ScriptedChat, failed_with, run_model, search, search_host, with_chat,
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
        let socket = socket_path(name);
        let fake = fake_extractor::serve(&socket);
        *fake.pictures.lock().expect("lock") = Some(scripted);
        let mut harness = with_extractor(harness(4, None).await, &socket);
        let chat = ScriptedChat::new(script);
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
    assert!(
        summary
            .message
            .contains("Embedded pictures not described: 2 no_model."),
        "{summary:?}"
    );
    assert_eq!(world.chat.calls(), 0);
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
