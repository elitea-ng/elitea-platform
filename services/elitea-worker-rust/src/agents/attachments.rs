//! The `input_attachments` content contract on the native runtime (#606).
//!
//! # What arrives here
//!
//! `AgentExecutionPayload::input_attachments` is a JSON ARRAY OF LANGCHAIN
//! CONTENT CHUNKS — not a list of files — exactly as the Python worker receives
//! it (`services/elitea-worker-python/src/elitea_worker/agents/attachments.py`).
//! An attachment is stored as an item whose `content` is a list of chunks, and
//! the admission path CONCATENATES every item's chunks into one flat array
//! (`services/elitea-main/internal/application/agentexecution/attachments.go`,
//! `currentTurnInputAttachments`). The current turn's attachments therefore
//! reach the model as extra chunks appended AFTER the user's own text, which is
//! what [`append_attachment_parts`] produces — the same order and shape the SDK
//! adapter builds in `sdk_adapter.py:926-937`
//! (`input_attachments` → `attachment_message_chunks` → `human_message_content`).
//!
//! # The "needs extraction" marker, and this runtime's boundary
//!
//! A `document` chunk carries a namespaced `elitea_attachment` marker naming the
//! bucket, the object key (`name`), the filepath and the row `item_id`, with
//! `needs_content_extraction: true` (attachments.go, `attachmentContentScaffold`
//! and `attachmentExtractionMarkerKey`). Pylon reads each such file through the
//! SDK artifact toolkit's `read_multiple_files` and APPENDS the text as a second
//! `{"type":"text"}` chunk; the Python worker does the same
//! (`sdk_adapter.py:529-624`, `_read_attachment_documents`).
//!
//! THIS RUNTIME NOW PERFORMS THAT READ, through the one channel it holds. It
//! still has no vault and no `artifact` toolkit family (`toolkits/materialize.rs`
//! rejects `artifact` by kind), and its egress allowlist still reaches the model
//! gateway alone — so the bytes come from elitea-main's private mTLS content
//! listener, over the same live claim and fence that already authorized the turn
//! (`PostAttachmentObject`,
//! `services/elitea-main/internal/infra/storage/runtime_attachment_object.go`).
//! [`pending_attachment_reads`] names what has to be read,
//! [`read_attachment_documents`] reads it, and [`resolved_attachment_chunks`]
//! splices each document in as a SECOND text chunk after its own header —
//! exactly the shape pylon's extraction step leaves behind, and exactly what the
//! Python worker builds (`sdk_adapter.py:529-624`, `_read_attachment_documents`).
//!
//! WHAT MAIN WILL NOT SERVE, AND WHY THE SKIP PATH REMAINS. The route answers
//! text and nothing else: a file over its cap, an empty one, or one that is not
//! valid UTF-8 (a pdf, a docx, a png) is refused there, because this runtime has
//! no extractor and half a document is worse than none. A conversation that does
//! not own the object is refused too. Every one of those — and every transport
//! failure — is treated here as "unreadable": the file's NAME still reaches the
//! model through its header chunk, the read is SKIPPED with a data-free log
//! event, and the TURN IS NEVER REFUSED. That is pylon's own rule for a file the
//! platform cannot read (`rpc/chat_all.py:384-386`): the question may not even be
//! about the file, and the header still tells the model it exists.
//!
//! A header chunk IMMEDIATELY FOLLOWED by a plain text chunk (one with no marker
//! of its own) is already extracted and is NOT read again. That is the storage
//! shape pylon leaves behind, and it is also the shape this module produces, so
//! a replayed turn does not pay for the read twice.
//!
//! Reporting extracted text back onto the stored row (#607) is a separate,
//! unowned platform boundary; `agents/result.rs` returns no
//! `attachment_contents`, and this module writes none. Until it does, a file
//! attached two turns ago reaches the model as its name alone, and every turn
//! that carries a file re-pays its read — the same gap the Python worker has.
//!
//! # Why a marker is STRIPPED and an unknown chunk `type` is REFUSED
//!
//! The marker is stripped before a chunk reaches the model — its only job is to
//! drive extraction, and the header text already states bucket, filename and
//! filepath in prose, so no provider is trusted to ignore an unknown key.
//!
//! An unknown chunk `type` is refused at admission rather than passed through,
//! because a `type` decides what the MODEL is shown: forwarding one this runtime
//! does not understand would silently change the prompt, and a chunk that fails
//! INSIDE the provider call fails after the turn was admitted and billed. The
//! admitted set is the two chunk shapes pylon's own processors produce, `text`
//! and `image_url` — matching the Python worker's `_ADMITTED_CHUNK_TYPES` so the
//! two ends stay deploy-compatible. A marker FIELD, by contrast, decides nothing
//! the model sees, so an unknown field is ignored rather than refused: that
//! asymmetry is deliberate and lets elitea-main add a marker field (as it did
//! `item_id` for #607) without making the two services undeployable apart.

use std::collections::BTreeMap;

use adk_rust::{Content, Part};
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64_STANDARD;
use serde_json::{Map, Value};

use super::assembly::invalid_profile;
use super::runtime::NativeAgentAssemblyError;
use crate::protocol::control::ClaimBoundRuntimeContextAuthority;
use crate::transport::platform_client::PlatformClient;

/// The namespaced marker object. Its name and contents are fixed by the
/// admission path (attachments.go, `attachmentExtractionMarkerKey`); this module
/// only reads them.
const ATTACHMENT_MARKER_KEY: &str = "elitea_attachment";
const ATTACHMENT_MARKER_EXTRACT_FIELD: &str = "needs_content_extraction";

/// The Go admission transaction caps ONE turn at 64 attachments, and each can
/// contribute at most two chunks here — its header and its already-extracted
/// text — so 128 is that same bound expressed in the unit this list is counted
/// in. Raising the Go cap without raising this one turns a large but legitimate
/// turn into an admission refusal.
const MAX_INPUT_ATTACHMENT_CHUNKS: usize = 128;

/// varchar(256) each side in migrations/tenant/0127, and the Go admission path
/// refuses anything longer (`maxAttachmentFieldBytes`). A worker that admitted
/// more would be admitting a reference the platform cannot have stored.
const MAX_ATTACHMENT_FIELD_BYTES: usize = 256;

/// The decoded size of ONE attachment image this runtime will put in front of
/// the model, and the encoded budget for a whole turn's images (#981).
///
/// Both numbers are elitea-main's, not this runtime's: `attachments.go` embeds
/// at most `maxInlineAttachmentImageBytes` (256 KiB) of raw bytes per image and
/// `maxInlineAttachmentImageTurnBytes` (512 KiB) of base64 per turn, so a chunk
/// larger than these did not come from the admission path this runtime is
/// deployed against. Re-checking them here is not distrust of that path: the
/// command is signed but its CONTENT is a document, and the cost of a wrong
/// number is a provider request too large to bill, after the turn was admitted.
const MAX_RENDERED_ATTACHMENT_IMAGE_BYTES: usize = 256 * 1024;
const MAX_RENDERED_ATTACHMENT_IMAGE_TURN_BYTES: usize = 512 * 1024;

/// The media types a provider is handed as an image, and the only ones this
/// runtime renders. It is the same four elitea-main embeds (`inlineAttachment
/// ImageMediaTypes`), which is the same four the Anthropic block type accepts
/// (`ImageMediaType`) and every OpenAI-compatible provider documents. A chunk
/// naming anything else is counted unavailable rather than forwarded: a media
/// type the provider rejects fails the turn INSIDE the provider call, which is
/// exactly the failure mode this module refuses to create.
const RENDERABLE_ATTACHMENT_IMAGE_TYPES: [&str; 4] =
    ["image/png", "image/jpeg", "image/gif", "image/webp"];

/// What one rendered image costs the turn's budget: its base64 length, which is
/// the quantity elitea-main bounded when it chose to embed it.
const fn encoded_image_cost(decoded_len: usize) -> usize {
    decoded_len.div_ceil(3) * 4
}

/// Decode one `image_url` chunk's data URL into the part a model client can
/// send, or `None` when it is not something this runtime will render.
///
/// `None` is never an error: the chunk stays out of the prompt, the file's own
/// header chunk still names it to the model, and the caller counts it. The
/// cases are a non-`data:` URL (an http one would make the provider fetch a
/// tenant URL — this runtime does not forward those), an unsupported or absent
/// media type, a payload that is not base64, an empty image, and one past the
/// per-image cap.
fn decode_attachment_image(url: &str) -> Option<(String, Vec<u8>)> {
    let payload = url.strip_prefix("data:")?;
    let (media_type, encoded) = payload.split_once(";base64,")?;
    let media_type = media_type.trim().to_ascii_lowercase();
    if !RENDERABLE_ATTACHMENT_IMAGE_TYPES.contains(&media_type.as_str()) {
        return None;
    }
    // Bounded BEFORE decoding: base64 is 4 characters per 3 bytes, so a payload
    // past this cannot decode to anything this runtime would send, and
    // measuring first keeps a hostile document from allocating for the answer.
    if encoded.len() > MAX_RENDERED_ATTACHMENT_IMAGE_BYTES * 4 / 3 + 4 {
        return None;
    }
    let data = BASE64_STANDARD.decode(encoded).ok()?;
    if data.is_empty() || data.len() > MAX_RENDERED_ATTACHMENT_IMAGE_BYTES {
        return None;
    }
    Some((media_type, data))
}

/// Admit only the chunks this runtime can put in front of a model.
///
/// Called from `validate_common_profile`, so a disagreement between the platform
/// and this runtime is an admission refusal on a turn that has not started
/// rather than a provider error in the middle of one. Every failure is
/// `InvalidInput`, matching the Python worker's `validate_input_attachments`,
/// which raises `InvalidInput` for every one of these cases.
pub(super) fn validate_input_attachments(
    input_attachments: &[Value],
) -> Result<(), NativeAgentAssemblyError> {
    if input_attachments.len() > MAX_INPUT_ATTACHMENT_CHUNKS {
        return Err(invalid_profile());
    }
    for chunk in input_attachments {
        let chunk = chunk.as_object().ok_or_else(invalid_profile)?;
        match chunk.get("type").and_then(Value::as_str) {
            Some("text") => {
                if !chunk.get("text").is_some_and(Value::is_string) {
                    return Err(invalid_profile());
                }
            }
            Some("image_url") => {
                let well_formed = chunk
                    .get("image_url")
                    .and_then(Value::as_object)
                    .and_then(|image| image.get("url"))
                    .is_some_and(Value::is_string);
                if !well_formed {
                    return Err(invalid_profile());
                }
            }
            // Missing, empty, or unrecognised: all `InvalidInput`, exactly as
            // the Python worker treats "type is required" and "type is not
            // supported".
            Some(_) | None => return Err(invalid_profile()),
        }
        validate_marker(chunk.get(ATTACHMENT_MARKER_KEY))?;
    }
    Ok(())
}

/// Splice this turn's attachment chunks into the human message, AFTER the user's
/// own text.
///
/// With no attachments the content is returned unchanged, so an ordinary turn is
/// untouched. Otherwise each chunk is projected into a model-facing part in
/// order — its marker stripped — and appended after the parts already on
/// `content`. A `document` chunk still flagged `needs_content_extraction`, with
/// no already-extracted text beside it, contributes only its header part and is
/// counted as an unavailable read; after [`resolved_attachment_chunks`] has
/// run, that means a file elitea-main would not serve as text, not a file
/// nobody tried to read.
///
/// An `image_url` chunk becomes a real image part (#981): its data URL is
/// decoded into `Part::InlineData`, which both model clients then render —
/// `image_url` on the OpenAI-compatible path, a base64 `ImageBlock` on the
/// Anthropic one. A chunk this runtime will not render (a media type no
/// provider takes, a payload that is not base64, one past the per-image cap, or
/// one that would take the turn past its image budget) is counted as an
/// unavailable part instead, and the file's own header chunk still names it to
/// the model — the behaviour every image had before #981.
#[must_use]
pub(super) fn append_attachment_parts(
    mut content: Content,
    input_attachments: &[Value],
) -> Content {
    if input_attachments.is_empty() {
        return content;
    }
    let mut unreadable_documents = 0_usize;
    let mut unrendered_images = 0_usize;
    // The turn's image budget, spent in ENCODED bytes so it is the same
    // quantity elitea-main spent when it decided what to embed.
    let mut image_budget = MAX_RENDERED_ATTACHMENT_IMAGE_TURN_BYTES;
    for (index, chunk) in input_attachments.iter().enumerate() {
        let Some(object) = chunk.as_object() else {
            // Unreachable after `validate_input_attachments`; skipping keeps a
            // malformed chunk from breaking a turn whose text is fine.
            continue;
        };
        match object.get("type").and_then(Value::as_str) {
            Some("text") => {
                if let Some(text) = object.get("text").and_then(Value::as_str) {
                    // The marker is a sibling key and is never copied — reading
                    // `text` alone is what strips it before the model.
                    content = content.with_text(text);
                }
            }
            Some("image_url") => {
                match object
                    .get("image_url")
                    .and_then(Value::as_object)
                    .and_then(|image| image.get("url"))
                    .and_then(Value::as_str)
                    .and_then(decode_attachment_image)
                {
                    // The budget is charged in the units the URL carries, and
                    // ONLY for an image that is actually rendered — a
                    // malformed one must not push the next good one off the
                    // end of the turn.
                    Some((media_type, data)) if encoded_image_cost(data.len()) <= image_budget => {
                        image_budget -= encoded_image_cost(data.len());
                        // Constructed directly rather than through
                        // `Content::with_inline_data`, which PANICS past its
                        // own 10 MB ceiling. Nothing can reach that after
                        // `decode_attachment_image`, and a panic inside an
                        // assembly step would take the worker down rather than
                        // the turn.
                        content.parts.push(Part::InlineData {
                            mime_type: media_type,
                            data,
                            uri: None,
                            annotations: None,
                        });
                    }
                    _ => unrendered_images += 1,
                }
            }
            _ => {}
        }
        if needs_unavailable_read(input_attachments, index, object) {
            unreadable_documents += 1;
        }
    }
    // Counts only — filenames and buckets are tenant data and stay out of worker
    // logs, in line with the data-free diagnostics the rest of this worker keeps.
    // Neither is an error path: the turn runs and the file is named to the model.
    if unreadable_documents > 0 {
        tracing::info!(
            event = "agent_input_attachment_extraction_unavailable",
            count = unreadable_documents,
            "attachment document content extraction is unavailable on the native runtime; \
             the file's header still names it to the model"
        );
    }
    if unrendered_images > 0 {
        tracing::info!(
            event = "agent_input_attachment_image_unavailable",
            count = unrendered_images,
            "an attachment image could not be rendered as a model image part \
             (unsupported media type, malformed data URL, or the turn's image \
             budget); the file's header still names it to the model"
        );
    }
    content
}

/// Admit only the marker object the admission path documents.
///
/// Mirrors the Python worker's `_validate_marker`: a `None`/JSON-null marker is
/// inert; an unknown field is ignored (see the module note on that asymmetry);
/// a present reference field must be bounded; and a marker that ASKS for
/// extraction must name both a bucket and a key, because a marker demanding an
/// impossible read is a contract disagreement, not a missing file.
fn validate_marker(marker: Option<&Value>) -> Result<(), NativeAgentAssemblyError> {
    let marker = match marker {
        None | Some(Value::Null) => return Ok(()),
        Some(Value::Object(marker)) => marker,
        Some(_) => return Err(invalid_profile()),
    };
    let needs_extraction = match marker.get(ATTACHMENT_MARKER_EXTRACT_FIELD) {
        None => false,
        Some(Value::Bool(value)) => *value,
        Some(_) => return Err(invalid_profile()),
    };
    for field in ["bucket", "name", "filepath", "item_id"] {
        let malformed = marker
            .get(field)
            .is_some_and(|value| !value.is_null() && !bounded_reference_text(value));
        if malformed {
            return Err(invalid_profile());
        }
    }
    if needs_extraction
        && !(marker.get("bucket").is_some_and(bounded_reference_text)
            && marker.get("name").is_some_and(bounded_reference_text))
    {
        return Err(invalid_profile());
    }
    Ok(())
}

/// One attached document's object reference: the `(bucket, name)` pair its
/// marker carries, which is exactly what the read route selects on.
///
/// `name` keeps its conversation-uuid prefix. That is not cosmetic — it is the
/// object KEY, and elitea-main authorizes the read by requiring the CLAIM's own
/// conversation to prefix it. A worker that trimmed it to a basename would have
/// every read refused, correctly.
#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub(crate) struct AttachmentReference {
    bucket: String,
    name: String,
}

/// The documents this turn managed to read, by reference.
///
/// A `BTreeMap` rather than a hash map so a turn's reads are deterministic in
/// order — the same input produces the same prompt, which is what makes a
/// replay comparable.
pub(crate) type AttachmentContents = BTreeMap<AttachmentReference, String>;

/// The references whose text still has to be read, deduplicated and in
/// first-seen order.
///
/// The same file attached twice costs ONE read, and the order is stable so a
/// turn is reproducible. Mirrors the Python worker's `pending_attachment_reads`.
pub(crate) fn pending_attachment_reads(input_attachments: &[Value]) -> Vec<AttachmentReference> {
    let mut pending: Vec<AttachmentReference> = Vec::new();
    for (index, chunk) in input_attachments.iter().enumerate() {
        let Some(object) = chunk.as_object() else {
            continue;
        };
        let Some(reference) = extraction_reference(input_attachments, index, object) else {
            continue;
        };
        if !pending.contains(&reference) {
            pending.push(reference);
        }
    }
    pending
}

/// Read this turn's pending attachments through the live claim.
///
/// NOTHING HERE MAY FAIL THE TURN, and that is the whole contract. A refused
/// conversation, a file main will not serve as text, a stale reference, a
/// transport failure — each contributes no text and is counted, never raised.
/// The caller renders the header either way, so the model is told the file
/// exists and is not shown a half-read version of it.
///
/// Reads are issued ONE AT A TIME on purpose. The Python worker batches per
/// bucket because its SDK call takes a file list; this route takes one object,
/// and a turn's attachment count is capped at 64 by admission, so serialising
/// keeps one turn from opening 64 concurrent streams on the bounded content
/// listener (`ContentMaxStreams`) and starving every other execution's reads.
pub(crate) async fn read_attachment_documents(
    platform: &PlatformClient,
    authority: &ClaimBoundRuntimeContextAuthority,
    pending: &[AttachmentReference],
) -> AttachmentContents {
    let mut contents = AttachmentContents::new();
    let mut failures = 0_usize;
    for reference in pending {
        match platform
            .read_attachment_object(authority, &reference.bucket, &reference.name)
            .await
        {
            Ok(object) => {
                let text = object.into_content();
                if text.is_empty() {
                    failures += 1;
                } else {
                    contents.insert(reference.clone(), text);
                }
            }
            Err(error) => {
                failures += 1;
                // The CODE only. Bucket, key and filename are tenant data and
                // stay out of worker logs, in line with the data-free
                // diagnostics the rest of this worker keeps.
                tracing::info!(
                    event = "agent_input_attachment_read_failed",
                    error_code = error.code(),
                    "an attached document could not be read; its header still names it to the model"
                );
            }
        }
    }
    if failures > 0 {
        tracing::info!(
            event = "agent_input_attachment_read_unavailable",
            count = failures,
            "attachment document content could not be read for every reference; \
             the turn continues and each file is still named to the model"
        );
    }
    contents
}

/// Project the admitted chunks with this turn's reads spliced in.
///
/// Each successfully read document is inserted as a SECOND `{"type":"text"}`
/// chunk directly after its own header, which is what pylon's extraction step
/// does (`rpc/chat_all.py:366-374`) and what the Python worker's
/// `attachment_message_chunks` produces. Two properties follow from that
/// placement and both are load-bearing:
///
///   * the text sits next to the header that names the file, so a turn carrying
///     several documents cannot associate one file's body with another's name;
///   * the result is the shape [`carries_extracted_text`] already recognises,
///     so nothing downstream counts a filled attachment as an unavailable read.
///
/// The marker is deliberately NOT stripped here — [`append_attachment_parts`]
/// strips it as it renders, and it is what tells this projection which chunks
/// are headers in the first place.
#[must_use]
pub(crate) fn resolved_attachment_chunks(
    input_attachments: &[Value],
    extracted: &AttachmentContents,
) -> Vec<Value> {
    if extracted.is_empty() {
        return input_attachments.to_vec();
    }
    let mut chunks: Vec<Value> = Vec::with_capacity(input_attachments.len() + extracted.len());
    for (index, chunk) in input_attachments.iter().enumerate() {
        chunks.push(chunk.clone());
        let Some(object) = chunk.as_object() else {
            continue;
        };
        let Some(reference) = extraction_reference(input_attachments, index, object) else {
            continue;
        };
        let Some(text) = extracted.get(&reference) else {
            continue;
        };
        if text.is_empty() {
            continue;
        }
        chunks.push(serde_json::json!({"type": "text", "text": text}));
    }
    chunks
}

/// The reference a chunk asks to have read, if it asks at all.
///
/// `None` for a chunk with no marker, a marker that does not ask for
/// extraction, a marker naming nothing to read, or a header already followed by
/// its own text. Mirrors the Python worker's `_extraction_reference`.
fn extraction_reference(
    input_attachments: &[Value],
    index: usize,
    object: &Map<String, Value>,
) -> Option<AttachmentReference> {
    let marker = object.get(ATTACHMENT_MARKER_KEY)?.as_object()?;
    if marker.get(ATTACHMENT_MARKER_EXTRACT_FIELD) != Some(&Value::Bool(true)) {
        return None;
    }
    let bucket = marker
        .get("bucket")
        .filter(|value| bounded_reference_text(value))?;
    let name = marker
        .get("name")
        .filter(|value| bounded_reference_text(value))?;
    if carries_extracted_text(input_attachments, index) {
        return None;
    }
    Some(AttachmentReference {
        bucket: bucket.as_str()?.to_owned(),
        name: name.as_str()?.to_owned(),
    })
}

/// Whether the chunk at `index` STILL asks for a read by the time it is
/// rendered.
///
/// Which now means the read was attempted and did not produce text: the
/// projection above inserts each successful read as the following chunk, and
/// that is precisely what [`extraction_reference`] stops naming. So this is the
/// count of files the model was told about but not shown — not the count of
/// files with a marker.
fn needs_unavailable_read(
    input_attachments: &[Value],
    index: usize,
    object: &Map<String, Value>,
) -> bool {
    extraction_reference(input_attachments, index, object).is_some()
}

/// Whether the header at `index` is already followed by its own extracted text —
/// a plain `text` chunk with no marker of its own, the shape pylon leaves behind
/// after its extraction step ran.
fn carries_extracted_text(input_attachments: &[Value], index: usize) -> bool {
    input_attachments
        .get(index + 1)
        .and_then(Value::as_object)
        .is_some_and(|successor| {
            successor.get("type").and_then(Value::as_str) == Some("text")
                && !successor.contains_key(ATTACHMENT_MARKER_KEY)
        })
}

fn bounded_reference_text(value: &Value) -> bool {
    value.as_str().is_some_and(|text| {
        !text.is_empty()
            && text.len() <= MAX_ATTACHMENT_FIELD_BYTES
            && !text
                .bytes()
                .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
    })
}

#[cfg(test)]
mod tests {
    use std::sync::Mutex;
    use std::time::Duration;

    use super::*;
    use crate::agents::runtime::NativeAgentAssemblyErrorCode;
    use crate::protocol::control::test_runtime_context_authority;
    use crate::transport::runtime_context::{
        RuntimeContextClient, RuntimeContextConfig, RuntimeContextRpc, RuntimeContextTransportError,
    };
    use adk_rust::Part;
    use async_trait::async_trait;
    use bytes::Bytes;
    use http::{Request, Response, StatusCode, Version};
    use http_body_util::Full;
    use serde_json::json;
    use std::sync::Arc;
    use tonic::body::Body;

    fn text_parts(content: &Content) -> Vec<String> {
        content
            .parts
            .iter()
            .filter_map(|part| match part {
                Part::Text { text } => Some(text.clone()),
                _ => None,
            })
            .collect()
    }

    fn user_message() -> Content {
        Content::new("user").with_text("the question")
    }

    #[test]
    fn empty_attachments_leave_the_user_message_unchanged() {
        let content = append_attachment_parts(user_message(), &[]);
        assert_eq!(text_parts(&content), vec!["the question".to_owned()]);
    }

    #[test]
    fn text_chunks_are_appended_after_the_user_text_in_order() {
        let attachments = vec![
            json!({"type": "text", "text": "first file header"}),
            json!({"type": "text", "text": "second file header"}),
        ];
        let content = append_attachment_parts(user_message(), &attachments);
        assert_eq!(
            text_parts(&content),
            vec![
                "the question".to_owned(),
                "first file header".to_owned(),
                "second file header".to_owned(),
            ]
        );
    }

    #[test]
    fn a_header_with_no_text_beside_it_is_announced_alone() {
        // The rendering half in isolation: a header whose read produced nothing
        // (a pdf, a refused conversation, a transport failure) still names the
        // file to the model and contributes no body. `append_attachment_parts`
        // renders what it is given, so this is also what an assembly that
        // performed no read at all produces.
        let attachments = vec![json!({
            "type": "text",
            "text": "Bucket: chat-attachments\nFilename: conv/report.txt\nfilepath: /chat-attachments/conv/report.txt",
            "elitea_attachment": {
                "needs_content_extraction": true,
                "bucket": "chat-attachments",
                "name": "conv/report.txt",
                "filepath": "/chat-attachments/conv/report.txt",
                "item_id": "11111111-1111-1111-1111-111111111111"
            }
        })];
        let content = append_attachment_parts(user_message(), &attachments);
        let parts = text_parts(&content);
        assert_eq!(parts.len(), 2, "the header is announced");
        assert!(
            parts[1].contains("report.txt"),
            "the file is named to the model"
        );
        // The marker was stripped: nothing part-side carries the namespaced key.
        assert!(
            !parts
                .iter()
                .any(|part| part.contains(ATTACHMENT_MARKER_KEY)),
            "the marker must not reach the model"
        );
    }

    #[test]
    fn an_already_extracted_header_renders_both_chunks() {
        // A header immediately followed by a plain text chunk is the shape pylon
        // leaves after extraction ran. Both are plain text here, so both reach
        // the model verbatim with no read required.
        let attachments = vec![
            json!({
                "type": "text",
                "text": "Bucket: chat-attachments\nFilename: conv/report.txt",
                "elitea_attachment": {
                    "needs_content_extraction": true,
                    "bucket": "chat-attachments",
                    "name": "conv/report.txt"
                }
            }),
            json!({"type": "text", "text": "the extracted body of report.txt"}),
        ];
        let content = append_attachment_parts(user_message(), &attachments);
        assert_eq!(
            text_parts(&content),
            vec![
                "the question".to_owned(),
                "Bucket: chat-attachments\nFilename: conv/report.txt".to_owned(),
                "the extracted body of report.txt".to_owned(),
            ]
        );
    }

    fn image_parts(content: &Content) -> Vec<(String, Vec<u8>)> {
        content
            .parts
            .iter()
            .filter_map(|part| match part {
                Part::InlineData {
                    mime_type, data, ..
                } => Some((mime_type.clone(), data.clone())),
                _ => None,
            })
            .collect()
    }

    fn data_url(media_type: &str, bytes: &[u8]) -> String {
        format!("data:{media_type};base64,{}", BASE64_STANDARD.encode(bytes))
    }

    #[test]
    fn an_attached_image_becomes_a_model_image_part() {
        // #981. The bytes the admission path embedded reach the model as an
        // image part, after the file's own header text and in the order the
        // chunks arrived.
        let bytes = b"\x89PNG\r\n\x1a\nautotest".to_vec();
        let attachments = vec![
            json!({"type": "text", "text": "Filename: conv/shot.png"}),
            json!({"type": "image_url", "image_url": {"url": data_url("image/png", &bytes)}}),
        ];
        let content = append_attachment_parts(user_message(), &attachments);
        assert_eq!(
            text_parts(&content),
            vec![
                "the question".to_owned(),
                "Filename: conv/shot.png".to_owned()
            ]
        );
        assert_eq!(image_parts(&content), vec![("image/png".to_owned(), bytes)]);
        // Header first, image after it: a picture with no idea which file it is
        // is worse than one the model can name.
        assert!(matches!(content.parts[2], Part::InlineData { .. }));
    }

    #[test]
    fn every_renderable_media_type_reaches_the_model_as_itself() {
        for media_type in ["image/png", "image/jpeg", "image/gif", "image/webp"] {
            let attachments = vec![
                json!({"type": "image_url", "image_url": {"url": data_url(media_type, b"bytes")}}),
            ];
            let content = append_attachment_parts(user_message(), &attachments);
            assert_eq!(
                image_parts(&content),
                vec![(media_type.to_owned(), b"bytes".to_vec())],
                "{media_type} must render as itself"
            );
        }
    }

    #[test]
    fn an_image_this_runtime_will_not_render_is_counted_not_forwarded() {
        // Every shape that must NOT reach a provider: a remote URL (which would
        // make the provider fetch a tenant address), a media type no provider
        // takes, a payload that is not base64, an empty image, and one past the
        // per-image cap. Each leaves the user's own text alone.
        let oversized = data_url(
            "image/png",
            &vec![7_u8; MAX_RENDERED_ATTACHMENT_IMAGE_BYTES + 1],
        );
        for url in [
            "https://example.invalid/pic.png".to_owned(),
            data_url("image/bmp", b"bytes"),
            "data:image/png;base64,not base64!!".to_owned(),
            data_url("image/png", b""),
            oversized,
        ] {
            let attachments = vec![json!({"type": "image_url", "image_url": {"url": url}})];
            let content = append_attachment_parts(user_message(), &attachments);
            assert_eq!(text_parts(&content), vec!["the question".to_owned()]);
            assert!(
                image_parts(&content).is_empty(),
                "nothing may be forwarded for a chunk this runtime will not render"
            );
            assert_eq!(content.parts.len(), 1);
        }
    }

    #[test]
    fn the_turns_image_budget_bounds_what_is_rendered() {
        // Three images at the per-image cap: the turn's budget admits the ones
        // that fit and counts the rest, so a provider request cannot grow past
        // what elitea-main bounded when it embedded them.
        let big = vec![3_u8; MAX_RENDERED_ATTACHMENT_IMAGE_BYTES];
        let attachments: Vec<Value> = (0..3)
            .map(
                |_| json!({"type": "image_url", "image_url": {"url": data_url("image/png", &big)}}),
            )
            .collect();
        let content = append_attachment_parts(user_message(), &attachments);
        let rendered = image_parts(&content).len();
        let per_image = encoded_image_cost(big.len());
        assert_eq!(
            rendered,
            MAX_RENDERED_ATTACHMENT_IMAGE_TURN_BYTES / per_image,
            "the budget is spent in encoded bytes, once per rendered image"
        );
        assert!(
            rendered < 3,
            "the budget must actually bound something here"
        );
    }

    #[test]
    fn a_refused_image_does_not_spend_the_budget_a_good_one_needs() {
        // The malformed one is counted, not charged: otherwise one bad chunk
        // could push a perfectly good picture off the end of the turn.
        let big = vec![3_u8; MAX_RENDERED_ATTACHMENT_IMAGE_BYTES];
        let attachments = vec![
            json!({"type": "image_url", "image_url": {"url": "data:image/png;base64,!!!!"}}),
            json!({"type": "image_url", "image_url": {"url": data_url("image/png", &big)}}),
        ];
        let content = append_attachment_parts(user_message(), &attachments);
        assert_eq!(image_parts(&content).len(), 1);
    }

    fn err_code(input_attachments: &[Value]) -> NativeAgentAssemblyErrorCode {
        validate_input_attachments(input_attachments)
            .expect_err("expected a refusal")
            .code()
    }

    #[test]
    fn well_formed_chunks_are_admitted() {
        validate_input_attachments(&[
            json!({"type": "text", "text": "header"}),
            json!({"type": "text", "text": "body"}),
            json!({"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}),
            json!({
                "type": "text",
                "text": "header",
                "elitea_attachment": {
                    "needs_content_extraction": true,
                    "bucket": "chat-attachments",
                    "name": "conv/report.txt",
                    "filepath": "/chat-attachments/conv/report.txt",
                    "item_id": "abc"
                }
            }),
        ])
        .expect("well-formed attachments must be admitted");
    }

    #[test]
    fn malformed_shapes_are_refused_as_invalid_input() {
        for attachment in [
            json!("not an object"),
            json!({"artifact_id": "one"}),      // no type
            json!({"type": ""}),                // empty type
            json!({"type": "video"}),           // unsupported type
            json!({"type": "text"}),            // text missing
            json!({"type": "text", "text": 7}), // text not a string
            json!({"type": "image_url"}),       // image missing
            json!({"type": "image_url", "image_url": {"url": 7}}),
            json!({"type": "text", "text": "x", "elitea_attachment": "not an object"}),
            json!({"type": "text", "text": "x", "elitea_attachment": {"needs_content_extraction": "yes"}}),
            // needs extraction but names nothing to read.
            json!({"type": "text", "text": "x", "elitea_attachment": {"needs_content_extraction": true}}),
        ] {
            assert_eq!(
                err_code(std::slice::from_ref(&attachment)),
                NativeAgentAssemblyErrorCode::InvalidInput,
                "expected InvalidInput for {attachment}"
            );
        }
    }

    #[test]
    fn too_many_chunks_are_refused() {
        let attachments =
            vec![json!({"type": "text", "text": "x"}); MAX_INPUT_ATTACHMENT_CHUNKS + 1];
        assert_eq!(
            err_code(&attachments),
            NativeAgentAssemblyErrorCode::InvalidInput
        );
    }

    #[test]
    fn a_null_or_absent_marker_is_inert() {
        validate_input_attachments(&[
            json!({"type": "text", "text": "x"}),
            json!({"type": "text", "text": "x", "elitea_attachment": null}),
        ])
        .expect("an absent or null marker is admitted");
    }

    // ---------------------------------------------------------------------
    // The read path (#606): what has to be read, what a read produces, and
    // what happens when it cannot happen at all.
    // ---------------------------------------------------------------------

    /// One header chunk exactly as admission writes it.
    fn header(name: &str) -> Value {
        json!({
            "type": "text",
            "text": format!("Bucket: chat-attachments\nFilename: {name}"),
            "elitea_attachment": {
                "needs_content_extraction": true,
                "bucket": "chat-attachments",
                "name": name,
                "filepath": format!("/chat-attachments/{name}"),
                "item_id": "11111111-1111-1111-1111-111111111111"
            }
        })
    }

    fn reference(name: &str) -> AttachmentReference {
        AttachmentReference {
            bucket: "chat-attachments".to_owned(),
            name: name.to_owned(),
        }
    }

    #[test]
    fn pending_reads_are_deduplicated_in_first_seen_order() {
        // The same file attached twice is an ordinary thing for a user to do
        // and must cost ONE read. The third file already carries its text and
        // must cost none.
        let attachments = vec![
            header("conv/a.txt"),
            header("conv/b.txt"),
            header("conv/a.txt"),
            header("conv/c.txt"),
            json!({"type": "text", "text": "c.txt was already extracted"}),
            json!({"type": "text", "text": "a plain chunk with no marker"}),
            json!({"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}),
        ];
        assert_eq!(
            pending_attachment_reads(&attachments),
            vec![reference("conv/a.txt"), reference("conv/b.txt")],
        );
    }

    #[test]
    fn a_read_document_is_spliced_beside_its_own_header() {
        // Two files, one read: the body must land next to ITS header, not at
        // the end of the list, or a turn with several documents would hand the
        // model one file's text under another file's name.
        let attachments = vec![header("conv/a.txt"), header("conv/b.txt")];
        let mut extracted = AttachmentContents::new();
        extracted.insert(reference("conv/a.txt"), "the body of a".to_owned());

        let resolved = resolved_attachment_chunks(&attachments, &extracted);
        assert_eq!(resolved.len(), 3);
        assert_eq!(
            resolved[1],
            json!({"type": "text", "text": "the body of a"})
        );

        let parts = text_parts(&append_attachment_parts(user_message(), &resolved));
        assert_eq!(parts.len(), 4);
        assert!(parts[1].contains("a.txt"), "a's header names it");
        assert_eq!(parts[2], "the body of a", "a's body follows a's header");
        assert!(parts[3].contains("b.txt"), "b is still announced");
        assert!(
            !parts
                .iter()
                .any(|part| part.contains(ATTACHMENT_MARKER_KEY)),
            "the marker must not reach the model"
        );

        // …and the filled attachment is no longer a pending read, so a replay
        // does not pay for it twice.
        assert_eq!(
            pending_attachment_reads(&resolved),
            vec![reference("conv/b.txt")]
        );
    }

    #[test]
    fn nothing_read_leaves_the_chunks_exactly_as_they_arrived() {
        let attachments = vec![header("conv/a.txt")];
        let resolved = resolved_attachment_chunks(&attachments, &AttachmentContents::new());
        assert_eq!(resolved, attachments);
    }

    struct FixedRpc {
        responses: Mutex<Vec<Response<Body>>>,
        targets: Arc<Mutex<Vec<String>>>,
    }

    #[async_trait]
    impl RuntimeContextRpc for FixedRpc {
        async fn post(
            &self,
            request: Request<Body>,
        ) -> Result<Response<Body>, RuntimeContextTransportError> {
            // The raw request target, NOT `uri().path()`: what main routes on
            // is the escaped form, and the escaping is the thing under test.
            self.targets
                .lock()
                .map_err(|_| RuntimeContextTransportError::Unavailable)?
                .push(request.uri().to_string());
            self.responses
                .lock()
                .map_err(|_| RuntimeContextTransportError::Unavailable)?
                .pop()
                .ok_or(RuntimeContextTransportError::Unavailable)
        }
    }

    fn attachment_response(status: StatusCode, body: &str) -> Response<Body> {
        Response::builder()
            .status(status)
            .version(Version::HTTP_2)
            .header("content-type", "application/json")
            .header("cache-control", "private, no-cache, no-store")
            .header("pragma", "no-cache")
            .header("content-length", body.len())
            .body(Body::new(Full::new(Bytes::from(body.to_owned()))))
            .expect("attachment fixture response")
    }

    fn platform_with(responses: Vec<Response<Body>>) -> (PlatformClient, Arc<Mutex<Vec<String>>>) {
        let targets = Arc::new(Mutex::new(Vec::new()));
        let rpc = FixedRpc {
            // popped from the back, so the fixture reads naturally in order.
            responses: Mutex::new(responses.into_iter().rev().collect()),
            targets: Arc::clone(&targets),
        };
        let client = RuntimeContextClient::with_rpc(
            rpc,
            RuntimeContextConfig {
                origin: "https://content.internal".to_owned(),
                deadline: Duration::from_secs(1),
                max_response_bytes: 32 * 1_024,
                max_application_response_bytes: 1_024 * 1_024,
                max_attachment_response_bytes: 1_024 * 1_024,
                max_artifact_response_bytes: 2 * 1_024 * 1_024,
            },
        )
        .expect("attachment fixture client");
        (PlatformClient::new(Arc::new(client)), targets)
    }

    #[tokio::test(flavor = "current_thread")]
    async fn a_served_document_reaches_the_model_beside_its_header() {
        // The whole path, end to end: the marker names the object, the claim
        // authorizes it, main answers the envelope, and the text lands in the
        // prompt. `project_id` is 17 because that is what the claim binds —
        // a document naming any other project is refused by the transport.
        let attachments = vec![header("conv/report.txt")];
        let (platform, _) = platform_with(vec![attachment_response(
            StatusCode::OK,
            &json!({
                "schema_version": "elitea.runtime.attachment-object.v1",
                "project_id": 17,
                "bucket": "chat-attachments",
                "name": "conv/report.txt",
                "media_type": "text/plain",
                "byte_length": 30,
                "content": "The secret word is SPLICEDOK.\n",
            })
            .to_string(),
        )]);

        let pending = pending_attachment_reads(&attachments);
        let extracted =
            read_attachment_documents(&platform, &test_runtime_context_authority(), &pending).await;
        assert_eq!(extracted.len(), 1);

        let resolved = resolved_attachment_chunks(&attachments, &extracted);
        let parts = text_parts(&append_attachment_parts(user_message(), &resolved));
        assert_eq!(parts.len(), 3);
        assert!(
            parts[1].contains("report.txt"),
            "the file is still announced"
        );
        assert!(
            parts[2].contains("SPLICEDOK"),
            "the token that lives only inside the file must reach the model"
        );
    }

    #[tokio::test(flavor = "current_thread")]
    async fn an_unreadable_document_is_announced_and_never_refuses_the_turn() {
        // Every refusal main can answer, plus a transport failure. Each must
        // produce NO text, NO error, and a turn that still renders the header —
        // the property the e2e's rust leg pinned before this route existed and
        // that a pdf still relies on today.
        for response in [
            Some(attachment_response(StatusCode::FORBIDDEN, "{}")),
            Some(attachment_response(StatusCode::NOT_FOUND, "{}")),
            Some(attachment_response(StatusCode::UNPROCESSABLE_ENTITY, "{}")),
            Some(attachment_response(StatusCode::SERVICE_UNAVAILABLE, "{}")),
            // A 200 whose document names a project the claim did not bind.
            Some(attachment_response(
                StatusCode::OK,
                &json!({
                    "schema_version": "elitea.runtime.attachment-object.v1",
                    "project_id": 999,
                    "bucket": "chat-attachments",
                    "name": "conv/report.txt",
                    "media_type": "text/plain",
                    "byte_length": 5,
                    "content": "leak!",
                })
                .to_string(),
            )),
            // A 200 answering about a DIFFERENT object than was asked for.
            Some(attachment_response(
                StatusCode::OK,
                &json!({
                    "schema_version": "elitea.runtime.attachment-object.v1",
                    "project_id": 17,
                    "bucket": "chat-attachments",
                    "name": "other-conversation/secret.txt",
                    "media_type": "text/plain",
                    "byte_length": 6,
                    "content": "secret",
                })
                .to_string(),
            )),
            // No response at all: the transport is down.
            None,
        ] {
            let attachments = vec![header("conv/report.txt")];
            let (platform, _) = platform_with(response.into_iter().collect());
            let pending = pending_attachment_reads(&attachments);
            let extracted =
                read_attachment_documents(&platform, &test_runtime_context_authority(), &pending)
                    .await;
            assert!(extracted.is_empty(), "no text may be produced");

            let resolved = resolved_attachment_chunks(&attachments, &extracted);
            let parts = text_parts(&append_attachment_parts(user_message(), &resolved));
            assert_eq!(parts.len(), 2, "the header still reaches the model");
            assert!(parts[1].contains("report.txt"));
        }
    }

    #[tokio::test(flavor = "current_thread")]
    async fn the_object_key_travels_as_one_percent_encoded_path_segment() {
        // The key contains slashes and main routes on `r.URL.RawPath`, so the
        // request target must keep them ESCAPED. An unescaped `/` here would
        // address a route that does not exist and every read would 404 — a
        // failure that looks exactly like a stale attachment.
        let (platform, targets) = platform_with(Vec::new());
        let _ = platform
            .read_attachment_object(
                &test_runtime_context_authority(),
                "chat-attachments",
                "5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1/report file.txt",
            )
            .await;
        let targets = targets.lock().expect("captured targets");
        assert_eq!(targets.len(), 1);
        assert_eq!(
            targets[0],
            "/executions/execution%2Fone/generations/2/runtime-context/attachments/\
             chat-attachments/5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1%2Freport%20file.txt"
                .replace(char::is_whitespace, ""),
        );
    }
}
