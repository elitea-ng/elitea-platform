//! The pictures a CONNECTOR's documents link, through a whole run (ADR-0030
//! I4, PR-C2): the tests' toolkit hands the worker a `PictureBatch` the way
//! the ADO wiki, ADO boards and Jira connectors do (what each connector reads
//! is pinned in its own crate), a scripted vision `Llm` describes the
//! pictures, and the stored text is read back from the `elitea-vector` fake.
//!
//! What they pin: descriptions land where the pictures were; the connector's
//! prompt (and a picture's own) reaches the model; the shared cache; no chat
//! model leaves the document as it was; and the owner's rule that a picture
//! never fails a document or the run, with the retry mark for reasons another
//! run cures.

use std::fmt::Write as _;
use std::sync::Arc;

use adk_rust::ErrorCategory;
use elitea_connectors::pictures::token;
use elitea_content_source::Document;
use serde_json::{Value, json};

use super::model_step_tests::{Reply, ScriptedChat, run_model, search, search_host, with_chat};
use super::tests::{Harness, document, harness, parameters};
use crate::protocol::elitea::runtime::v1::IndexIngestStatusV1;

/// A document whose connector read `tags.len()` pictures; its template is
/// `before <token 0> between <token 1> after`.
fn page(key: &str, tags: &[&str], prompt: Option<&str>, skipped: &Value) -> Document {
    let mut template = String::from("Before");
    for (index, _) in tags.iter().enumerate() {
        let _ = write!(template, " {} and", token(index));
    }
    template.push_str(" after.");
    let mut document = document(key, "v1", "Before and after, plain.");
    document.metadata.insert(
        "fixture_pictures".to_owned(),
        json!({
            "template": template,
            "prompt": prompt,
            "pictures": tags.iter().map(|tag| json!({
                "tag": tag, "before": "[Image: ", "after": "]", "fallback": format!("!{tag}!"),
            })).collect::<Vec<_>>(),
            "skipped": skipped,
        }),
    );
    document
}

/// What the scripted model says of a picture: `seen <tag>`.
fn by_tag() -> Arc<ScriptedChat> {
    ScriptedChat::by_image(|images| {
        let bytes = &images[0].1;
        let tag = String::from_utf8_lossy(&bytes[8..]).into_owned();
        (0, Reply::Text(format!("seen {tag}")))
    })
}

async fn stored_text(harness: Harness) -> String {
    let Harness {
        runtime,
        token,
        fake,
        ..
    } = harness;
    assert!(fake.point_count() >= 1);
    let runtime = Arc::new(runtime);
    let host = search_host(&runtime, &token);
    let found = search(
        &host,
        json!({"query": "before", "index_name": "docs", "cut_off": 0,
               "output_fields": ["page_content", "metadata"]}),
    )
    .await;
    found
        .as_array()
        .unwrap_or_else(|| panic!("{found}"))
        .iter()
        .filter_map(|hit| hit["page_content"].as_str())
        .collect::<Vec<_>>()
        .join("")
}

#[tokio::test(flavor = "multi_thread")]
async fn descriptions_are_written_where_the_connector_found_the_pictures() {
    let mut harness = harness(4, None).await;
    let chat = by_tag();
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![page("page.md", &["one", "two"], None, &json!({}))],
        None,
        &parameters(false, json!({})),
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
    assert_eq!(summary.documents[0].version, "v1", "everything described");
    assert_eq!(chat.calls(), 2);
    let text = stored_text(harness).await;
    assert!(
        text.contains("Before [Image: seen one] and [Image: seen two] and after."),
        "{text}"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn the_connectors_prompt_reaches_the_model_and_a_repeated_picture_costs_one_call() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(|_, user| {
        assert!(user.contains("Describe the chart."), "{user}");
        Reply::Text("a chart".to_owned())
    });
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![
            page("a.md", &["same"], Some("Describe the chart."), &json!({})),
            page("b.md", &["same"], Some("Describe the chart."), &json!({})),
        ],
        None,
        &parameters(false, json!({})),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 2, "{summary:?}");
    assert_eq!(chat.calls(), 1, "the run's cache: same bytes, same prompt");
}

#[tokio::test(flavor = "multi_thread")]
async fn without_a_chat_model_the_document_is_indexed_as_the_connector_fetched_it() {
    let harness = harness(4, None).await;
    let summary = run_model(
        &harness,
        vec![page("page.md", &["one"], None, &json!({}))],
        None,
        &parameters(false, json!({})),
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
    assert!(
        summary
            .message
            .contains("Linked pictures not requested for documents: 1 no_model."),
        "{summary:?}"
    );
    assert_eq!(summary.documents[0].version, "v1");
    let text = stored_text(harness).await;
    assert!(text.contains("Before and after, plain."), "{text}");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_picture_that_fails_never_fails_the_document_and_marks_it_for_another_run() {
    let mut harness = harness(4, None).await;
    let chat = ScriptedChat::new(|_, _| {
        Reply::Fail(ErrorCategory::Unavailable, "model_gateway.unavailable")
    });
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![page("page.md", &["one"], None, &json!({}))],
        None,
        &parameters(false, json!({})),
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
        summary
            .message
            .contains("Linked pictures not described: 1 model_failed."),
        "{summary:?}"
    );
    assert_eq!(summary.documents[0].version, "v1#pictures-pending-1");
    let text = stored_text(harness).await;
    assert!(
        text.contains("Before !one! and after."),
        "the link stays: {text}"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn pictures_the_connector_could_not_read_are_counted_and_a_failed_fetch_is_retried() {
    let mut harness = harness(4, None).await;
    let chat = by_tag();
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![
            page(
                "a.md",
                &["one"],
                None,
                &json!({"fetch_failed": 1, "too_large": 2}),
            ),
            page("b.md", &["two"], None, &json!({"unsupported_type": 1})),
        ],
        None,
        &parameters(false, json!({})),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Ok as i32,
        "{summary:?}"
    );
    assert_eq!(summary.indexed_documents, 2);
    assert!(
        summary.message.contains(
            "Linked pictures not described: 1 fetch_failed, 2 too_large, 1 unsupported_type."
        ),
        "{summary:?}"
    );
    let versions: std::collections::BTreeMap<_, _> = summary
        .documents
        .iter()
        .map(|d| (d.document_key.as_str(), d.version.as_str()))
        .collect();
    assert_eq!(
        versions["a.md"], "v1#pictures-pending-1",
        "a transient read failure"
    );
    assert_eq!(versions["b.md"], "v1", "a final reason is not retried");
}

#[test]
fn bins_with_llm_forces_the_vision_step_on_for_the_named_extensions_with_the_loaders_prompt() {
    use elitea_connectors::pictures::PictureStep;
    use elitea_doc_chunk::{ChunkingConfig, ImagePlan, plan_embedded_for, plan_image};

    use super::pipeline::binary_overlay;

    let step = PictureStep::Binary {
        extensions: &[".png", ".jpg", ".jpeg", ".pdf"],
        prompt: "Loader prompt.".to_owned(),
    };
    // The deployment switched the step off: the toolkit's own switch wins.
    let off = ChunkingConfig::from_value(&json!({"use_llm": false})).expect("config");
    assert_eq!(plan_image(".png", &off), ImagePlan::Disabled);
    assert!(plan_embedded_for("a.pdf", "application/pdf", &off).is_none());
    let png = binary_overlay(&off, &step, "page/1/attachment/9/shot.PNG").expect("png");
    let ImagePlan::Vision(vision) = plan_image(".png", &png) else {
        panic!("vision is on");
    };
    assert_eq!(vision.prompt(), "Loader prompt.");
    let pdf = binary_overlay(&off, &step, "page/1/attachment/9/spec.pdf").expect("pdf");
    let plan = plan_embedded_for("page/1/attachment/9/spec.pdf", "application/pdf", &pdf)
        .expect("a PDF's pictures are described");
    assert_eq!(plan.prompt(), "Loader prompt.");
    // Other extensions are untouched, and so is the original config.
    assert!(binary_overlay(&off, &step, "page/1/attachment/9/notes.docx").is_none());
    assert_eq!(plan_image(".png", &off), ImagePlan::Disabled);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_document_without_a_picture_link_neither_waits_for_the_model_nor_is_counted() {
    // No chat model: a document that links pictures is counted `no_model`,
    // one that links none is not, and says nothing of pictures.
    let harness = harness(4, None).await;
    let plain = document("plain.md", "v1", "No pictures here.");
    let summary = run_model(
        &harness,
        vec![plain, page("page.md", &["one"], None, &json!({}))],
        None,
        &parameters(false, json!({})),
        None,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 2, "{summary:?}");
    assert!(
        summary
            .message
            .contains("Linked pictures not requested for documents: 1 no_model."),
        "one document, not two: {summary:?}"
    );
    assert!(!summary.message.contains("Embedded"), "{summary:?}");
}

#[tokio::test(flavor = "multi_thread")]
async fn linked_and_embedded_pictures_are_reported_apart() {
    let mut harness = harness(4, None).await;
    let chat = by_tag();
    with_chat(&mut harness, &chat);
    let summary = run_model(
        &harness,
        vec![page("a.md", &["one"], None, &json!({"too_large": 1}))],
        None,
        &parameters(false, json!({})),
        Some("gpt-vision"),
    )
    .await
    .expect("summary");
    assert!(
        summary
            .message
            .contains("Linked pictures not described: 1 too_large."),
        "{summary:?}"
    );
    assert!(
        !summary.message.contains("Embedded pictures"),
        "{summary:?}"
    );
}
