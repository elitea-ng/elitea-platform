//! Descriptions of the pictures EMBEDDED in a PDF, DOCX or PPTX, put into the
//! document's text (ADR-0030 I4, part C).
//!
//! The SDK's loaders describe an embedded picture with the chat model only
//! when asked: `use_llm` is OFF for documents (`DEFAULT_ALLOWED_WITH_LLM`
//! in `document_loaders/constants.py`; it is ON only for the image files).
//! The description is written INTO the document's text, in a block the
//! loader formats:
//!
//! * PDF (`EliteAPDFLoader.read_pdf_page`) and PPTX
//!   (`EliteAPowerPointLoader.read_pptx_slide`):
//!   `"\n**Image Transcript:**\n" + description + "\n--------------------\n"`;
//! * DOCX (`EliteADocxMammothLoader.__postprocess_original_md`):
//!   `"\n**Image Transcript:**\n" + "Image: <file name>, <description>" + "\n"`,
//!   and a picture whose bytes were already described in the same document
//!   is `Image: <file name> [already transcribed, see above]`.
//!
//! This crate calls no model: [`plan_embedded`] decides whether the step is
//! on for a document and with which prompt, and [`EmbeddedVisionPlan::inline`]
//! splices the descriptions the caller got into the text at the offsets the
//! extractor gave.
//!
//! Where the block goes is a difference from the SDK, stated: the SDK's PDF
//! loader appends a page's pictures after the page's text and so does this
//! (the end of the page); its PPTX loader writes the block at the picture
//! shape's place among the slide's shapes, this one at the end of the slide
//! (the extractor's plain text has no shape order to put it by); its DOCX
//! loader writes it where the picture is, and so does this (right after the
//! picture's own `[Image: ...]` line).

use crate::config::ChunkingConfig;
use crate::route::extension_of;
use crate::vision::{EMPTY_DESCRIPTION, IMAGE_PROCESSING_PROMPT, MULTIMODAL_PROMPT};

/// The extensions whose embedded pictures the SDK's loaders describe
/// (HTML, XML and spreadsheets never do).
pub const EMBEDDED_IMAGE_EXTENSIONS: [&str; 3] = [".pdf", ".docx", ".pptx"];

/// How a document type writes a description into its text.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Layout {
    /// PDF and PPTX: a block closed by a rule.
    Paged,
    /// DOCX: a block that names the picture's file.
    Docx,
}

/// Whether `config` turns the step on for `extension`: the extension's own
/// `use_llm`, else the top-level one, else OFF (the SDK's default for
/// documents).
pub(crate) fn embedded_enabled(config: &ChunkingConfig, extension: &str) -> bool {
    EMBEDDED_IMAGE_EXTENSIONS.contains(&extension.to_ascii_lowercase().as_str())
        && config.params_for(extension).use_llm.unwrap_or(false)
}

/// The embedded-picture step for one document.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EmbeddedVisionPlan {
    prompt: String,
    layout: Layout,
}

/// What became of one picture, for [`EmbeddedVisionPlan::inline`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Described<'a> {
    /// The model's description.
    Description(&'a str),
    /// The same bytes were described earlier in this document (DOCX writes a
    /// back-reference; the other types repeat the description).
    Repeat,
}

/// One picture's place in the text and what to write there.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Inline<'a> {
    /// Byte offset in the text; clamped to the text and moved forward to a
    /// character boundary if it is not on one.
    pub offset: usize,
    /// The picture's file name in the package, when it has one.
    pub name: Option<&'a str>,
    /// The picture's 1-based place among the document's pictures: the name
    /// the SDK gives one without a file name (`image_<n>`).
    pub number: usize,
    pub described: Described<'a>,
}

impl EmbeddedVisionPlan {
    /// The prompt sent with each picture.
    #[must_use]
    pub fn prompt(&self) -> &str {
        &self.prompt
    }

    /// `text` with a block per entry of `pictures` written at its offset.
    /// Pictures at the same offset keep the order given. A blank
    /// description is written as the SDK's placeholder sentence
    /// ([`EMPTY_DESCRIPTION`]), as for an image file: the block exists, so a
    /// later back-reference to it points at something.
    #[must_use]
    pub fn inline(&self, text: &str, pictures: &[Inline<'_>]) -> String {
        let offsets: Vec<usize> = pictures.iter().map(|picture| picture.offset).collect();
        let mut out = String::with_capacity(text.len());
        let mut cursor = 0;
        for index in written_order(text, &offsets) {
            let offset = boundary(text, pictures[index].offset);
            let Some(block) = self.block(&pictures[index]) else {
                continue;
            };
            out.push_str(&text[cursor..offset]);
            cursor = offset;
            out.push_str(&block);
        }
        out.push_str(&text[cursor..]);
        out
    }

    fn block(&self, picture: &Inline<'_>) -> Option<String> {
        match (self.layout, &picture.described) {
            (Layout::Paged, Described::Description(description)) => Some(format!(
                "\n**Image Transcript:**\n{}\n--------------------\n",
                or_placeholder(description)
            )),
            // A repeat is written only by the DOCX layout; the others never
            // ask for one, and describe the picture again (from the cache).
            (Layout::Paged, Described::Repeat) => None,
            (Layout::Docx, described) => {
                let name = picture
                    .name
                    .map_or_else(|| format!("image_{}", picture.number), str::to_owned);
                Some(match described {
                    Described::Description(description) => {
                        format!(
                            "\n**Image Transcript:**\nImage: {name}, {}\n",
                            or_placeholder(description)
                        )
                    }
                    Described::Repeat => format!(
                        "\n**Image Transcript:**\nImage: {name} [already transcribed, see above]\n"
                    ),
                })
            }
        }
    }

    /// Whether a second occurrence of the same picture is written as a
    /// back-reference without asking the model again (DOCX) rather than
    /// described again.
    #[must_use]
    pub fn repeats_are_back_references(&self) -> bool {
        self.layout == Layout::Docx
    }
}

/// The order [`EmbeddedVisionPlan::inline`] writes the blocks of pictures
/// whose offsets are `offsets` in: the indices, by the offset each is
/// written at (clamped to `text` and moved to a character boundary), equal
/// offsets keeping the order given. The FIRST picture of some bytes in this
/// order is the one a DOCX describes; a later one with the same bytes is its
/// back-reference ("see above" must point up).
#[must_use]
pub fn written_order(text: &str, offsets: &[usize]) -> Vec<usize> {
    let mut placed: Vec<(usize, usize)> = offsets
        .iter()
        .enumerate()
        .map(|(index, offset)| (boundary(text, *offset), index))
        .collect();
    // Stable: equal offsets keep the order the caller gave.
    placed.sort_by_key(|(offset, _)| *offset);
    placed.into_iter().map(|(_, index)| index).collect()
}

/// The description, or the SDK's placeholder when the model's reply is blank.
fn or_placeholder(description: &str) -> &str {
    if description.trim().is_empty() {
        EMPTY_DESCRIPTION
    } else {
        description
    }
}

/// `offset` clamped to `text` and moved forward to a character boundary.
fn boundary(text: &str, offset: usize) -> usize {
    let mut offset = offset.min(text.len());
    while !text.is_char_boundary(offset) {
        offset += 1;
    }
    offset
}

/// Plans the embedded-picture step for the document named `source`: `None`
/// unless it is a PDF, DOCX or PPTX whose `use_llm` is on (the SDK's default
/// for documents is off, so a configuration that says nothing gets nothing).
///
/// The prompt comes from the extension's own block exactly as the image-file
/// step reads it (`use_default_prompt`, `prompt`); a `prompt` without
/// `use_default_prompt` is honoured here as there.
#[must_use]
pub fn plan_embedded(source: &str, config: &ChunkingConfig) -> Option<EmbeddedVisionPlan> {
    let extension = extension_of(source);
    if !embedded_enabled(config, &extension) {
        return None;
    }
    let own = config
        .own_params_for(&extension)
        .cloned()
        .unwrap_or_default();
    let own_prompt = own.prompt.as_deref().filter(|p| !p.trim().is_empty());
    let prompt = match (own.use_default_prompt, own_prompt) {
        (Some(true), _) => IMAGE_PROCESSING_PROMPT,
        (Some(false), own) => own.unwrap_or(MULTIMODAL_PROMPT),
        (None, own) => own.unwrap_or(IMAGE_PROCESSING_PROMPT),
    };
    Some(EmbeddedVisionPlan {
        prompt: prompt.to_owned(),
        layout: if extension.eq_ignore_ascii_case(".docx") {
            Layout::Docx
        } else {
            Layout::Paged
        },
    })
}

/// [`plan_embedded`] for a document that has a key AND a media type: by the
/// key's extension when it is one of the three types, else by the media type
/// (a key without an extension, or with a foreign one, that the source
/// delivered as `application/pdf`). The per-extension settings are those of
/// the media type's canonical extension (`.pdf`, `.docx`, `.pptx`).
#[must_use]
pub fn plan_embedded_for(
    key: &str,
    mime: &str,
    config: &ChunkingConfig,
) -> Option<EmbeddedVisionPlan> {
    let by_key = extension_of(key);
    if EMBEDDED_IMAGE_EXTENSIONS.contains(&by_key.as_str()) {
        plan_embedded(key, config)
    } else {
        plan_embedded(mime, config)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{Value, json};

    fn config(value: &Value) -> ChunkingConfig {
        ChunkingConfig::from_value(value).unwrap_or_default()
    }

    fn picture(offset: usize, number: usize, text: &str) -> Inline<'_> {
        Inline {
            offset,
            name: Some("image1.png"),
            number,
            described: Described::Description(text),
        }
    }

    #[test]
    fn pictures_are_written_in_offset_order_and_equal_offsets_keep_the_given_order() {
        // "aé" is 3 bytes: offset 2 is inside the é and moves to 3.
        assert_eq!(written_order("aé!", &[3, 0, 2, 99, 1]), vec![1, 4, 0, 2, 3]);
        assert_eq!(written_order("", &[]), [] as [usize; 0]);
    }

    #[test]
    fn the_step_is_off_unless_the_documents_use_llm_is_on() {
        for name in ["a.pdf", "a.docx", "a.pptx", "A.PDF"] {
            assert!(
                plan_embedded(name, &ChunkingConfig::default()).is_none(),
                "{name}: the SDK's default for documents is off"
            );
            let mut block = serde_json::Map::new();
            block.insert("use_llm".to_owned(), Value::Bool(true));
            let mut top = serde_json::Map::new();
            top.insert(extension_of(name), Value::Object(block));
            let on = config(&Value::Object(top));
            assert!(plan_embedded(name, &on).is_some(), "{name}");
        }
        // Only the three document types; image files have their own step.
        let all = config(&json!({"use_llm": true}));
        for name in ["a.html", "a.xml", "a.xlsx", "a.txt", "a.png", "noext"] {
            assert!(plan_embedded(name, &all).is_none(), "{name}");
        }
        // The top level switches the three on; an extension's own false wins.
        assert!(plan_embedded("a.pdf", &all).is_some());
        let off_pdf = config(&json!({"use_llm": true, ".pdf": {"use_llm": false}}));
        assert!(plan_embedded("a.pdf", &off_pdf).is_none());
        assert!(plan_embedded("a.docx", &off_pdf).is_some());
    }

    #[test]
    fn a_key_without_a_type_is_planned_by_its_media_type() {
        let on = config(&json!({".pdf": {"use_llm": true, "prompt": "Read it."}}));
        for key in ["docs/README", "download", "a.bin"] {
            let plan = plan_embedded_for(key, "application/pdf", &on).expect(key);
            assert_eq!(plan.prompt(), "Read it.", "{key}");
        }
        // The extension that settles it is the media type's, not another's.
        assert!(
            plan_embedded_for(
                "x",
                "application/pdf",
                &config(&json!({".docx": {"use_llm": true}}))
            )
            .is_none()
        );
        let docx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document";
        let pptx = "application/vnd.openxmlformats-officedocument.presentationml.presentation";
        let both = config(&json!({".docx": {"use_llm": true}, ".pptx": {"use_llm": true}}));
        assert!(plan_embedded_for("x", docx, &both).is_some());
        assert!(plan_embedded_for("x", pptx, &both).is_some());
        assert!(plan_embedded_for("x", "text/plain", &both).is_none());
        // The key's own extension still decides when it is one of the three.
        assert!(plan_embedded_for("a.pdf", pptx, &on).is_some());
        assert!(plan_embedded_for("a.docx", "application/pdf", &on).is_none());
    }

    #[test]
    fn the_prompt_follows_the_extensions_own_block_like_the_image_step() {
        let prompt_of = |value: Value| {
            plan_embedded("a.pdf", &config(&value))
                .expect("on")
                .prompt()
                .to_owned()
        };
        assert_eq!(
            prompt_of(json!({".pdf": {"use_llm": true}})),
            IMAGE_PROCESSING_PROMPT
        );
        assert_eq!(
            prompt_of(json!({".pdf": {"use_llm": true, "prompt": "Read the chart."}})),
            "Read the chart."
        );
        assert_eq!(
            prompt_of(
                json!({".pdf": {"use_llm": true, "prompt": "x", "use_default_prompt": true}})
            ),
            IMAGE_PROCESSING_PROMPT
        );
        assert_eq!(
            prompt_of(json!({".pdf": {"use_llm": true, "use_default_prompt": false}})),
            MULTIMODAL_PROMPT
        );
        // Top-level prompt settings never reach it.
        assert_eq!(
            prompt_of(json!({"use_llm": true, "prompt": "top"})),
            IMAGE_PROCESSING_PROMPT
        );
    }

    /// The SDK's blocks, written out from the Python source (SDK
    /// `1b335d5`): the PDF/PPTX loaders and the DOCX post-processor.
    #[test]
    fn the_blocks_are_the_sdks_byte_for_byte() {
        let pdf = plan_embedded("a.pdf", &config(&json!({".pdf": {"use_llm": true}}))).expect("on");
        let text = "Page: 1\nBefore.\nAfter.\n";
        let offset = text.len();
        // read_pdf_page: text_content += "\n**Image Transcript:**\n" + transcript
        //                                + "\n--------------------\n"
        assert_eq!(
            pdf.inline(text, &[picture(offset, 1, "A bar chart.")]),
            "Page: 1\nBefore.\nAfter.\n\n**Image Transcript:**\nA bar chart.\n--------------------\n"
        );
        let docx =
            plan_embedded("a.docx", &config(&json!({".docx": {"use_llm": true}}))).expect("on");
        // __postprocess_original_md: f"\n**Image Transcript:**\n{payload}\n",
        // payload = f"Image: {image_name}, {transcript}".
        let text = "Intro\n\n[Image: Picture 1]\n\nOutro";
        let offset = "Intro\n\n[Image: Picture 1]".len();
        assert_eq!(
            docx.inline(text, &[picture(offset, 1, "A bar chart.")]),
            "Intro\n\n[Image: Picture 1]\n**Image Transcript:**\nImage: image1.png, A bar chart.\n\n\nOutro"
        );
        // A repeat within one document: the SDK's back-reference.
        assert_eq!(
            docx.inline(
                "x",
                &[Inline {
                    offset: 1,
                    name: Some("image2.png"),
                    number: 2,
                    described: Described::Repeat,
                }]
            ),
            "x\n**Image Transcript:**\nImage: image2.png [already transcribed, see above]\n"
        );
        assert!(docx.repeats_are_back_references() && !pdf.repeats_are_back_references());
        // No file name: the SDK's `image_<n>`.
        assert_eq!(
            docx.inline(
                "x",
                &[Inline {
                    offset: 0,
                    name: None,
                    number: 3,
                    described: Described::Description("d"),
                }]
            ),
            "\n**Image Transcript:**\nImage: image_3, d\nx"
        );
    }

    #[test]
    fn descriptions_land_at_their_offsets_in_order_and_never_inside_a_character() {
        let pdf = plan_embedded("a.pdf", &config(&json!({".pdf": {"use_llm": true}}))).expect("on");
        let text = "aé b";
        // Offset 2 is inside 'é' (bytes 1..3): moved forward to 3.
        let out = pdf.inline(
            text,
            &[
                picture(100, 2, "late"),
                picture(2, 1, "mid"),
                picture(0, 3, "first"),
            ],
        );
        let block = |d: &str| format!("\n**Image Transcript:**\n{d}\n--------------------\n");
        assert_eq!(
            out,
            format!(
                "{}a\u{e9}{} b{}",
                block("first"),
                block("mid"),
                block("late")
            )
        );
        // Equal offsets keep the order given.
        let out = pdf.inline("", &[picture(0, 1, "one"), picture(0, 2, "two")]);
        assert_eq!(out, format!("{}{}", block("one"), block("two")));
        // Nothing to write: the text is unchanged, byte for byte.
        assert_eq!(pdf.inline(text, &[]), text);
        // A blank reply is the SDK's placeholder sentence, as for an image
        // file, in both layouts.
        assert_eq!(
            pdf.inline("ab", &[picture(1, 1, "  \n")]),
            format!("a{}b", block(EMPTY_DESCRIPTION))
        );
        let docx =
            plan_embedded("a.docx", &config(&json!({".docx": {"use_llm": true}}))).expect("on");
        assert_eq!(
            docx.inline("", &[picture(0, 1, "")]),
            format!("\n**Image Transcript:**\nImage: image1.png, {EMPTY_DESCRIPTION}\n")
        );
    }
}
