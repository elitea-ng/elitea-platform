//! The image-file vision step (ADR-0030 I4, part B): the plan for a standalone
//! image document, and the chunks of the description a model gave it.
//!
//! The SDK's `EliteAImageLoader` sends the image and a prompt to the chat
//! model, takes the reply as the document's text and splits it with the
//! markdown chunker (512 tokens, overlap 10), stamping
//! `processing_method: llm`. This crate never calls a model: [`plan_image`]
//! decides whether the step applies and with which prompt, and the caller
//! hands the description back to [`VisionPlan::chunks`].

use serde_json::Value;

use crate::config::{ChunkParams, ChunkingConfig};
use crate::route::{extension_of, markdown_chunks};
use crate::{Chunk, ChunkError};

/// The extensions the SDK's image loader serves (`DEFAULT_ALLOWED_IMAGE_LLM`
/// in `document_loaders/constants.py`): `use_llm` and `use_default_prompt`
/// default ON for these and nowhere else.
pub const IMAGE_EXTENSIONS: [&str; 7] = [".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".svg"];

/// The SDK's default prompt, `image_processing_prompt` in
/// `tools/utils/content_parser.py`, byte for byte.
const IMAGE_PROCESSING_PROMPT: &str = include_str!("prompts/image_processing.txt");

/// The SDK loader's own default, `DEFAULT_MULTIMODAL_PROMPT` in
/// `runtime/langchain/constants.py`: used when `use_default_prompt` is false
/// and no `prompt` is given.
const MULTIMODAL_PROMPT: &str = include_str!("prompts/multimodal.txt");

/// What the SDK stores when the model's reply is blank.
pub const EMPTY_DESCRIPTION: &str = "[No readable text detected by LLM]";

/// The loader's `max_tokens` / `token_overlap` defaults.
const VISION_MAX_TOKENS: i64 = 512;
const VISION_TOKEN_OVERLAP: i64 = 10;

/// The SDK's default prompt for describing an image.
#[must_use]
pub const fn image_processing_prompt() -> &'static str {
    IMAGE_PROCESSING_PROMPT
}

/// Whether `extension` (`.png` or `PNG`) is one the image loader serves.
#[must_use]
pub fn is_image_extension(extension: &str) -> bool {
    let lower = extension.to_ascii_lowercase();
    let dotted = if lower.starts_with('.') {
        lower
    } else {
        format!(".{lower}")
    };
    IMAGE_EXTENSIONS.contains(&dotted.as_str())
}

/// Whether `config` leaves the vision step on for `extension`: the
/// extension's own `use_llm`, else the top-level one, else ON.
pub(crate) fn vision_enabled(config: &ChunkingConfig, extension: &str) -> bool {
    config.params_for(extension).use_llm.unwrap_or(true)
}

/// Whether the vision step applies to any image extension under `config`.
pub(crate) fn vision_enabled_anywhere(config: &ChunkingConfig) -> bool {
    IMAGE_EXTENSIONS
        .iter()
        .any(|extension| vision_enabled(config, extension))
}

/// What a standalone image document comes to.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ImagePlan {
    /// Describe it with the chat model.
    Vision(VisionPlan),
    /// `use_llm` is off for this extension (the SDK would index nothing of
    /// the picture), or the name is not an image extension: no text.
    Disabled,
}

/// The vision step for one image document.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VisionPlan {
    prompt: String,
    max_tokens: i64,
    token_overlap: i64,
}

impl VisionPlan {
    /// The prompt sent with the image.
    #[must_use]
    pub fn prompt(&self) -> &str {
        &self.prompt
    }

    /// The chunks of the model's `description`: the markdown chunker at the
    /// loader's token window, each stamped `processing_method: llm`. A blank
    /// description is the SDK's placeholder sentence.
    ///
    /// # Errors
    /// [`ChunkError::Tokenizer`] if the encoding is unavailable.
    pub fn chunks(&self, description: &str) -> Result<Vec<Chunk>, ChunkError> {
        let text = if description.trim().is_empty() {
            EMPTY_DESCRIPTION
        } else {
            description
        };
        let params = ChunkParams {
            max_tokens: Some(self.max_tokens),
            token_overlap: Some(self.token_overlap),
            ..ChunkParams::default()
        };
        let mut chunks = markdown_chunks(text, &params)?;
        for chunk in &mut chunks {
            chunk
                .metadata
                .insert("processing_method".to_owned(), Value::from("llm"));
        }
        Ok(chunks)
    }
}

/// Plans the vision step for the image document named `source`.
///
/// `use_llm` is ON unless `config` turns it off for the extension (or at the
/// top level). The prompt, `max_tokens` and `token_overlap` come from the
/// extension's own block only, defaulting to the SDK's values (512 and 10).
/// The prompt is the SDK's default unless the extension's block
/// sets `use_default_prompt: false` (then its `prompt`, else the loader's
/// own multimodal prompt) or gives a `prompt` without saying
/// `use_default_prompt` (the SDK ignores such a prompt; honouring it is the
/// deliberate difference).
#[must_use]
pub fn plan_image(source: &str, config: &ChunkingConfig) -> ImagePlan {
    let extension = extension_of(source);
    if !is_image_extension(&extension) || !vision_enabled(config, &extension) {
        return ImagePlan::Disabled;
    }
    // The loader reads its prompt and window from the extension's OWN block,
    // as the SDK does: top-level text settings (a `max_tokens` for markdown,
    // say) never reach it.
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
    ImagePlan::Vision(VisionPlan {
        prompt: prompt.to_owned(),
        max_tokens: own
            .max_tokens
            .filter(|n| *n > 0)
            .unwrap_or(VISION_MAX_TOKENS),
        token_overlap: own
            .token_overlap
            .filter(|n| *n >= 0)
            .unwrap_or(VISION_TOKEN_OVERLAP),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use sha2::{Digest, Sha256};

    fn config(value: &Value) -> ChunkingConfig {
        ChunkingConfig::from_value(value).unwrap_or_default()
    }

    fn hex(text: &str) -> String {
        Sha256::digest(text.as_bytes())
            .iter()
            .fold(String::new(), |mut out, byte| {
                use std::fmt::Write as _;
                let _ = write!(out, "{byte:02x}");
                out
            })
    }

    /// The SDK's two prompts, pinned by digest of the Python source (SDK
    /// `1b335d5`): `content_parser.image_processing_prompt` and
    /// `runtime/langchain/constants.DEFAULT_MULTIMODAL_PROMPT`.
    #[test]
    fn the_prompts_are_the_sdks_byte_for_byte() {
        assert_eq!(
            hex(image_processing_prompt()),
            "2b1c09b6e99a519bc5e3c3035e74ce73e61cebdf02e07c03fab278410b273b14"
        );
        assert_eq!(image_processing_prompt().len(), 2017);
        assert_eq!(
            hex(MULTIMODAL_PROMPT),
            "3c815ee040a13615e10adc7d89090e4a4275b9c55e71381f1043c5d11ef3bb58"
        );
        assert!(image_processing_prompt().starts_with("\nYou are an AI model designed"));
    }

    #[test]
    fn vision_is_on_for_every_image_extension_and_nowhere_else() {
        for name in [
            "a.png", "a.JPG", "a.jpeg", "a.gif", "a.webp", "a.bmp", "a.svg",
        ] {
            let plan = plan_image(name, &ChunkingConfig::default());
            let ImagePlan::Vision(plan) = plan else {
                panic!("{name}: {plan:?}")
            };
            assert_eq!(plan.prompt(), image_processing_prompt());
        }
        assert_eq!(
            plan_image("a.md", &ChunkingConfig::default()),
            ImagePlan::Disabled
        );
    }

    #[test]
    fn use_llm_false_switches_it_off_per_extension_or_everywhere() {
        let off_png = config(&json!({".png": {"use_llm": false}}));
        assert_eq!(plan_image("a.png", &off_png), ImagePlan::Disabled);
        assert!(matches!(
            plan_image("a.jpg", &off_png),
            ImagePlan::Vision(_)
        ));
        let off_all = config(&json!({"use_llm": false}));
        assert_eq!(plan_image("a.jpg", &off_all), ImagePlan::Disabled);
        let on_png = config(&json!({"use_llm": false, ".png": {"use_llm": true}}));
        assert!(matches!(plan_image("a.png", &on_png), ImagePlan::Vision(_)));
    }

    #[test]
    fn prompts_follow_use_default_prompt_and_prompt() {
        let prompt_of = |value: Value| match plan_image("a.png", &config(&value)) {
            ImagePlan::Vision(plan) => plan.prompt().to_owned(),
            ImagePlan::Disabled => panic!("disabled"),
        };
        let custom = json!({".png": {"prompt": "Read the chart."}});
        assert_eq!(prompt_of(custom), "Read the chart.");
        let forced = json!({".png": {"prompt": "x", "use_default_prompt": true}});
        assert_eq!(prompt_of(forced), image_processing_prompt());
        let own = json!({".png": {"prompt": "x", "use_default_prompt": false}});
        assert_eq!(prompt_of(own), "x");
        let fallback = json!({".png": {"use_default_prompt": false}});
        assert_eq!(prompt_of(fallback), MULTIMODAL_PROMPT);
        assert_eq!(
            prompt_of(json!({".png": {"prompt": "   "}})),
            image_processing_prompt()
        );
    }

    #[test]
    fn the_window_and_prompt_come_from_the_extensions_own_block_only() {
        // Top-level text settings never leak into the vision step.
        let top = config(&json!({"max_tokens": 5, "token_overlap": 3, "prompt": "top"}));
        let ImagePlan::Vision(plan) = plan_image("a.png", &top) else {
            panic!("disabled")
        };
        assert_eq!(plan.prompt(), image_processing_prompt());
        assert_eq!(plan.max_tokens, 512);
        assert_eq!(plan.token_overlap, 10);
        let own = config(&json!({"max_tokens": 5, ".png": {"max_tokens": 64, "token_overlap": 0}}));
        let ImagePlan::Vision(plan) = plan_image("a.png", &own) else {
            panic!("disabled")
        };
        assert_eq!((plan.max_tokens, plan.token_overlap), (64, 0));
        let ImagePlan::Vision(other) = plan_image("a.jpg", &own) else {
            panic!("disabled")
        };
        assert_eq!((other.max_tokens, other.token_overlap), (512, 10));
    }

    #[test]
    fn the_description_is_markdown_chunked_and_stamped() {
        let ImagePlan::Vision(plan) = plan_image("a.png", &ChunkingConfig::default()) else {
            panic!("disabled")
        };
        let chunks = plan.chunks("# Chart\n\nBars rise.\n").expect("chunks");
        assert_eq!(chunks.len(), 1);
        assert_eq!(chunks[0].metadata["processing_method"], "llm");
        assert_eq!(chunks[0].chunk_id, 1);
        let long = (0..1500)
            .map(|n| format!("word{n}"))
            .collect::<Vec<_>>()
            .join(" ");
        let chunks = plan.chunks(&long).expect("chunks");
        assert!(chunks.len() > 2, "{}", chunks.len());
        assert!(
            chunks
                .iter()
                .all(|c| c.metadata["processing_method"] == "llm")
        );
        let blank = plan.chunks("  \n").expect("chunks");
        assert_eq!(blank[0].text, EMPTY_DESCRIPTION);
    }
}
