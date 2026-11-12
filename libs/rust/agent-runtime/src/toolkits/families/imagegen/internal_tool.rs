//! The `image_generation` internal tool (#1969): one `image_generation` tool a
//! chat or agent turns on from the Modules panel, with no toolkit to
//! configure.
//!
//! It is the `imagegen` family's `generate_image` under the platform's own
//! settings: the model is the one the host lends (the run's image model), the
//! bucket is the conversation's attachment bucket, and the images are saved
//! there as artifacts through the same host the family uses. A run that has
//! no image model, or whose host cannot reach the images API, is not offered
//! the tool at all ([`ImageGenerationAuthority::withheld`] says why).
//!
//! The answer carries artifact references and a short sentence, never image
//! bytes: nothing here puts base64 into the model's context. A gateway
//! refusal (budget, rate limit, provider error, content policy) is a sentence
//! the model reads, through the family's own failure map.

use std::sync::Arc;

use adk_core::{Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Value, json};

use super::{
    DEFAULT_NAME_PREFIX, ImageGenTool, ImageGenToolImpl, ImageGenToolsetError, Settings,
    host_sentence, string_arg,
};
use crate::toolkits::families::office_host::{ImageGenerateRequest, SharedOfficeToolHost};
use crate::toolkits::invocation::admit_materialized_toolset;
use crate::toolkits::policy::{ToolAdmissionDecision, ToolAdmissionPolicy};

/// The internal tool's name, as the Modules toggle and agent versions spell it.
pub const IMAGE_GENERATION_TOOL: &str = "image_generation";
/// The most images one call asks for.
pub const MAX_IMAGES_PER_CALL: i64 = 4;
const MAX_PROMPT_BYTES: usize = 32 * 1_024;
/// The sizes the tool offers: the sizes the images API's models accept.
pub const IMAGE_SIZES: &[&str] = &[
    "auto",
    "256x256",
    "512x512",
    "1024x1024",
    "1024x1536",
    "1536x1024",
    "1792x1024",
    "1024x1792",
];
/// The quality tiers the tool offers; a model that has none ignores it.
pub const IMAGE_QUALITIES: &[&str] = &["auto", "low", "medium", "high", "standard", "hd"];

const DESCRIPTION: &str = "Generate one or more images from a text prompt. The images are saved as files in the conversation's artifacts and shown to the user in the chat; the result lists their file paths. Generating an image costs money, so ask for no more images than the user needs. Do not describe the files as if you could see them.";
const NO_BUCKET: &str = "Could not save the image: this run has no conversation artifact bucket. Tell the user image generation is not available here.";
const BUCKET_UNAVAILABLE: &str = "Could not save the image: the conversation's artifact bucket is unavailable right now. Try again shortly.";

/// Why the conversation's artifact bucket is not known.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ImageBucketError {
    /// This run has no conversation to attach files to. Not retried.
    NotAvailable,
    /// The platform could not be asked right now. Worth a retry.
    Unavailable,
}

/// The bucket generated images are saved in: the conversation's attachment
/// bucket, as the platform names it.
#[async_trait]
pub trait ImageArtifactBucket: Send + Sync {
    /// The bucket name, or why it is not known.
    async fn artifact_bucket(&self) -> Result<String, ImageBucketError>;
}

/// What the host lends the tool for one run.
#[derive(Clone)]
pub struct ImageGenerationAuthority {
    host: SharedOfficeToolHost,
    model: Option<Box<str>>,
    bucket: Arc<dyn ImageArtifactBucket>,
}

impl ImageGenerationAuthority {
    /// `model` is the run's image model (a registry name); `None` or empty is
    /// "none configured".
    #[must_use]
    pub fn new(
        host: SharedOfficeToolHost,
        model: Option<&str>,
        bucket: Arc<dyn ImageArtifactBucket>,
    ) -> Self {
        Self {
            host,
            model: model
                .map(str::trim)
                .filter(|model| !model.is_empty())
                .map(Into::into),
            bucket,
        }
    }

    /// Why the tool is not offered on this run, or `None` when it is.
    #[must_use]
    pub fn withheld(&self) -> Option<&'static str> {
        if self.model.is_none() {
            Some("no image generation model is configured")
        } else if !self.host.serves_images() {
            Some("this run cannot reach the image generation API")
        } else {
            None
        }
    }
}

/// The JSON schema of the tool's arguments.
#[must_use]
pub fn parameters_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "prompt": {"type": "string", "minLength": 1, "maxLength": MAX_PROMPT_BYTES, "description": "Text prompt describing the image to generate."},
            "size": {"type": "string", "enum": IMAGE_SIZES, "description": "Image size (width x height). Leave out for the model's default."},
            "n": {"type": "integer", "minimum": 1, "maximum": MAX_IMAGES_PER_CALL, "default": 1, "description": "Number of images to generate."},
            "quality": {"type": "string", "enum": IMAGE_QUALITIES, "description": "Quality tier, for models that have one. Leave out for the model's default."}
        },
        "required": ["prompt"],
        "additionalProperties": false
    })
}

/// Build the toolset. `None` when the policy blocks `image_generation` or the
/// run cannot serve it ([`ImageGenerationAuthority::withheld`]).
///
/// # Errors
///
/// The tool definition was refused by admission.
pub fn build_image_generation_toolset(
    authority: &ImageGenerationAuthority,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<Option<BasicToolset>, ImageGenToolsetError> {
    if authority.withheld().is_some()
        || policy.toolkit_decision(IMAGE_GENERATION_TOOL) != ToolAdmissionDecision::Allowed
        // An operator who blocked the `imagegen` toolkit blocked image
        // generation, whichever way it is reached.
        || policy.toolkit_decision("imagegen") != ToolAdmissionDecision::Allowed
    {
        return Ok(None);
    }
    let tool: Arc<dyn Tool> = Arc::new(ImageGenerationTool {
        host: Arc::clone(&authority.host),
        model: authority.model.clone().unwrap_or_default(),
        buckets: Arc::clone(&authority.bucket),
        resolved: tokio::sync::OnceCell::new(),
    });
    admit_materialized_toolset(
        IMAGE_GENERATION_TOOL,
        IMAGE_GENERATION_TOOL,
        policy,
        vec![tool],
    )
    .map(Some)
    .map_err(Into::into)
}

struct ImageGenerationTool {
    host: SharedOfficeToolHost,
    model: Box<str>,
    buckets: Arc<dyn ImageArtifactBucket>,
    resolved: tokio::sync::OnceCell<String>,
}

/// A validated call.
struct Call {
    prompt: String,
    size: Option<String>,
    count: u32,
    quality: Option<String>,
}

fn choice(arguments: &Value, key: &str, allowed: &[&str]) -> Result<Option<String>, String> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(text)) if text.trim().is_empty() => Ok(None),
        Some(Value::String(text)) => {
            let text = text.trim().to_ascii_lowercase();
            if allowed.contains(&text.as_str()) {
                Ok(Some(text))
            } else {
                Err(format!("`{key}` must be one of: {}.", allowed.join(", ")))
            }
        }
        Some(_) => Err(format!("`{key}` must be a string.")),
    }
}

fn parse_call(arguments: &Value) -> Result<Call, String> {
    let prompt = string_arg(arguments, "prompt")
        .ok_or_else(|| "A non-empty `prompt` is required.".to_owned())?;
    if prompt.len() > MAX_PROMPT_BYTES {
        return Err("The `prompt` is too long.".to_owned());
    }
    let count = match arguments.get("n") {
        None | Some(Value::Null) => 1,
        Some(value) => value
            .as_i64()
            .ok_or_else(|| "`n` must be a whole number.".to_owned())?,
    };
    if !(1..=MAX_IMAGES_PER_CALL).contains(&count) {
        return Err(format!("`n` must be between 1 and {MAX_IMAGES_PER_CALL}."));
    }
    Ok(Call {
        prompt,
        size: choice(arguments, "size", IMAGE_SIZES)?.filter(|size| size != "auto"),
        count: u32::try_from(count).unwrap_or(1),
        quality: choice(arguments, "quality", IMAGE_QUALITIES)?
            .filter(|quality| quality != "auto"),
    })
}

impl ImageGenerationTool {
    async fn bucket(&self) -> Result<&str, String> {
        self.resolved
            .get_or_try_init(|| async { self.buckets.artifact_bucket().await })
            .await
            .map(String::as_str)
            .map_err(|error| match error {
                ImageBucketError::NotAvailable => NO_BUCKET.to_owned(),
                ImageBucketError::Unavailable => BUCKET_UNAVAILABLE.to_owned(),
            })
    }

    async fn run(&self, arguments: &Value) -> Result<Value, String> {
        let call = parse_call(arguments)?;
        // Fail on a missing bucket before the (billed) model call.
        let bucket = self.bucket().await?.to_owned();
        let images = self
            .host
            .generate_images(ImageGenerateRequest {
                model: self.model.to_string(),
                prompt: call.prompt,
                size: call.size,
                count: call.count,
                quality: call.quality,
            })
            .await
            .map_err(|error| host_sentence("generate the image", &error))?;
        let saver = ImageGenToolImpl {
            tool: ImageGenTool::GenerateImage,
            settings: Arc::new(Settings {
                model: Some(self.model.clone()),
                bucket: Some(bucket.as_str().into()),
                name_prefix: DEFAULT_NAME_PREFIX.into(),
            }),
            host: Arc::clone(&self.host),
            description: DESCRIPTION.into(),
        };
        let saved = saver.save(images, "generate").await?;
        Ok(self.answer(&saved, &bucket))
    }

    /// The saved files as the model reads them: references and a sentence,
    /// plus a Markdown image link per file when the host knows one.
    fn answer(&self, saved: &Value, bucket: &str) -> Value {
        let artifacts = saved["artifacts"].as_array().cloned().unwrap_or_default();
        let mut shown = Vec::new();
        for item in &artifacts {
            let name = item["filename"].as_str().unwrap_or_default();
            if let Some(link) = self.host.object_link(bucket, name) {
                shown.push(format!("![{name}]({link})"));
            }
        }
        let mut message = format!(
            "Generated {} image(s) and saved them as artifacts: {}.",
            artifacts.len(),
            artifacts
                .iter()
                .filter_map(|item| item["filepath"].as_str())
                .collect::<Vec<_>>()
                .join(", ")
        );
        if shown.is_empty() {
            message.push_str(" The chat shows them to the user.");
        } else {
            message.push_str(" Show them to the user with this Markdown, unchanged:\n");
            message.push_str(&shown.join("\n"));
        }
        let mut answer = json!({"status": "success", "artifacts": artifacts, "message": message});
        if let Some(error) = saved.get("error") {
            answer["error"] = error.clone();
            answer["status"] = json!("partial");
        }
        answer
    }
}

#[async_trait]
impl Tool for ImageGenerationTool {
    fn name(&self) -> &str {
        IMAGE_GENERATION_TOOL
    }

    fn description(&self) -> &str {
        DESCRIPTION
    }

    fn is_read_only(&self) -> bool {
        false
    }

    fn is_concurrency_safe(&self) -> bool {
        false
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(parameters_schema())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        // Every failure is a tool RESULT the model reads, never an error that
        // aborts the turn.
        Ok(self.run(&arguments).await.unwrap_or_else(Value::String))
    }
}
