//! The `image_generation` internal tool (#1969): one `image_generation` tool a
//! chat or agent turns on from the Modules panel, with no toolkit to
//! configure.
//!
//! It is the `imagegen` family's generate path under the platform's own
//! settings: the model is the one the host lends (the deployment's image
//! model, which the gateway must be able to address from the calling project:
//! a global shared model), the bucket is the conversation's attachment bucket,
//! and the images are saved there as artifacts through the same host and the
//! same [`save_images`](super::save_images) the family uses. A run that has no
//! image model, or whose host cannot reach the images API, is not offered the
//! tool at all ([`ImageGenerationAuthority::withheld`] says why).
//!
//! The tool's schema and its argument checks come from ONE table per model
//! family ([`ImageFamily`]), so a value the model cannot take is an argument
//! error before any billed call.
//!
//! The answer carries artifact references and a short sentence, never image
//! bytes. A failure is a structured object
//! (`{"status": "error", "reason", "message", "retryable"}`) so the model can
//! tell a refusal from a success and does not retry a billed call that will
//! not work.

use std::sync::Arc;

use adk_core::{Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Value, json};

use super::{DEFAULT_NAME_PREFIX, ImageGenToolsetError, host_sentence, save_images, string_arg};
use crate::toolkits::families::office_host::{
    ImageGenerateRequest, OfficeFailure, OfficeHostError, SharedOfficeToolHost,
};
use crate::toolkits::invocation::admit_materialized_toolset;
use crate::toolkits::policy::{ToolAdmissionDecision, ToolAdmissionPolicy};

/// The internal tool's name, as the Modules toggle and agent versions spell it.
pub const IMAGE_GENERATION_TOOL: &str = "image_generation";
/// The most images one call asks for, whatever the model could do.
pub const MAX_IMAGES_PER_CALL: i64 = 4;
/// The longest prompt, in characters: at four bytes a character it is inside
/// the 32 KiB the family accepts.
const MAX_PROMPT_CHARS: usize = 8_192;
const MAX_FREE_TEXT_CHARS: usize = 32;
/// The reason code the images host gives a gateway 404 `model_not_found`.
pub const MODEL_NOT_FOUND_REASON: &str = "office_host.images_model_not_found";

const DESCRIPTION: &str = "Generate one or more images from a text prompt. The images are saved as files in the conversation's artifacts and shown to the user in the chat; the result lists their file paths. Generating an image costs money, so ask for no more images than the user needs. Do not describe the files as if you could see them. If the result has status \"error\", tell the user why; do not retry when `retryable` is false.";
const NO_BUCKET: &str = "Could not save the image: this run has no conversation artifact bucket. Tell the user image generation is not available here.";
const BUCKET_UNAVAILABLE: &str = "Could not save the image: the conversation's artifact bucket is unavailable right now. Try again shortly.";

/// What one image model accepts. The table is the tool's schema and its
/// argument check.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ImageFamily {
    /// The family's name, for logs and tests.
    pub name: &'static str,
    /// The most images one call may ask for (the tool's own cap is applied too).
    pub max_n: i64,
    /// The sizes the model takes. `auto` leaves the choice to the model.
    pub sizes: &'static [&'static str],
    /// The quality tiers the model takes; empty: the model has none, and an
    /// unknown model takes a short free-text value.
    pub qualities: &'static [&'static str],
    /// An unknown model: a quality is passed through as free text.
    pub free_quality: bool,
}

const DALL_E_3: ImageFamily = ImageFamily {
    name: "dall-e-3",
    max_n: 1,
    sizes: &["1024x1024", "1792x1024", "1024x1792"],
    qualities: &["standard", "hd"],
    free_quality: false,
};
const DALL_E_2: ImageFamily = ImageFamily {
    name: "dall-e-2",
    max_n: 4,
    sizes: &["256x256", "512x512", "1024x1024"],
    qualities: &[],
    free_quality: false,
};
const GPT_IMAGE: ImageFamily = ImageFamily {
    name: "gpt-image",
    max_n: 4,
    sizes: &["1024x1024", "1536x1024", "1024x1536", "auto"],
    qualities: &["low", "medium", "high", "auto"],
    free_quality: false,
};
const UNKNOWN: ImageFamily = ImageFamily {
    name: "unknown",
    max_n: 4,
    sizes: &["auto"],
    qualities: &[],
    free_quality: true,
};

impl ImageFamily {
    /// The family a registry model name belongs to, by what it contains
    /// (`openai/dall-e-3`, `gpt-image-1`, `my-dall-e-3-eu`); anything else is
    /// the conservative unknown family.
    #[must_use]
    pub fn of(model: &str) -> Self {
        let model = model.to_ascii_lowercase();
        if model.contains("dall-e-3") || model.contains("dalle-3") || model.contains("dalle3") {
            DALL_E_3
        } else if model.contains("dall-e-2")
            || model.contains("dalle-2")
            || model.contains("dalle2")
        {
            DALL_E_2
        } else if model.contains("gpt-image") {
            GPT_IMAGE
        } else {
            UNKNOWN
        }
    }

    fn max_images(self) -> i64 {
        self.max_n.min(MAX_IMAGES_PER_CALL)
    }

    /// The JSON schema of the tool's arguments for this family.
    #[must_use]
    pub fn parameters_schema(self) -> Value {
        let mut properties = serde_json::Map::new();
        properties.insert(
            "prompt".into(),
            json!({"type": "string", "minLength": 1, "maxLength": MAX_PROMPT_CHARS, "description": "Text prompt describing the image to generate."}),
        );
        properties.insert(
            "size".into(),
            json!({"type": "string", "enum": self.sizes, "description": "Image size (width x height). Leave out for the model's default."}),
        );
        properties.insert(
            "n".into(),
            json!({"type": "integer", "minimum": 1, "maximum": self.max_images(), "default": 1, "description": "Number of images to generate."}),
        );
        if !self.qualities.is_empty() {
            properties.insert(
                "quality".into(),
                json!({"type": "string", "enum": self.qualities, "description": "Quality tier. Leave out for the model's default."}),
            );
        } else if self.free_quality {
            properties.insert(
                "quality".into(),
                json!({"type": "string", "maxLength": MAX_FREE_TEXT_CHARS, "description": "Quality tier, only if the user names one the model is known to take. Leave out otherwise."}),
            );
        }
        json!({
            "type": "object",
            "properties": properties,
            "required": ["prompt"],
            "additionalProperties": false
        })
    }
}

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
    let model: Box<str> = authority.model.clone().unwrap_or_default();
    let tool: Arc<dyn Tool> = Arc::new(ImageGenerationTool {
        host: Arc::clone(&authority.host),
        family: ImageFamily::of(&model),
        model,
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
    family: ImageFamily,
    buckets: Arc<dyn ImageArtifactBucket>,
    resolved: tokio::sync::OnceCell<String>,
}

/// A failure the model reads: a stable code, a sentence, and whether the same
/// call may be tried again.
struct Failure {
    reason: &'static str,
    message: String,
    retryable: bool,
}

impl Failure {
    fn new(reason: &'static str, message: impl Into<String>, retryable: bool) -> Self {
        Self {
            reason,
            message: message.into(),
            retryable,
        }
    }

    fn argument(message: impl Into<String>) -> Self {
        Self::new("invalid_argument", message, false)
    }

    fn into_value(self) -> Value {
        json!({
            "status": "error",
            "reason": self.reason,
            "message": self.message,
            "retryable": self.retryable,
        })
    }
}

/// A host failure as the tool reports it: the code names the cause, and only
/// a failure that did not spend money and may pass is `retryable`.
fn host_failure(model: &str, error: &OfficeHostError) -> Failure {
    let sentence = host_sentence("generate the image", error);
    match error.kind() {
        OfficeFailure::BudgetRefused => Failure::new("budget_exceeded", sentence, false),
        OfficeFailure::RateLimited => Failure::new("rate_limited", sentence, true),
        OfficeFailure::Timeout => Failure::new("timeout", sentence, true),
        OfficeFailure::Conflict => Failure::new("conflict", sentence, true),
        OfficeFailure::AccessRefused => Failure::new("access_refused", sentence, false),
        OfficeFailure::InvalidRequest => Failure::new(
            "content_policy",
            "Could not generate the image: the image model rejected the request (its content policy, or a setting it does not take). Do not retry the same prompt; change it or tell the user.",
            false,
        ),
        OfficeFailure::NoModel if error.reason_code() == MODEL_NOT_FOUND_REASON => Failure::new(
            "model_not_available",
            format!(
                "Could not generate the image: the deployment's image model {model} is not available in this project; ask an administrator to share it as a global model."
            ),
            false,
        ),
        OfficeFailure::NoModel
        | OfficeFailure::ModelUnusable
        | OfficeFailure::ContextUnusable
        | OfficeFailure::Unsupported => Failure::new("model_not_available", sentence, false),
        _ => Failure::new("provider_error", sentence, false),
    }
}

/// A validated call.
struct Call {
    prompt: String,
    size: Option<String>,
    count: u32,
    quality: Option<String>,
}

fn choice(
    arguments: &Value,
    key: &str,
    allowed: &[&str],
    model: &str,
    family: ImageFamily,
) -> Result<Option<String>, Failure> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(text)) if text.trim().is_empty() => Ok(None),
        Some(Value::String(text)) => {
            let text = text.trim().to_ascii_lowercase();
            if allowed.contains(&text.as_str()) {
                Ok(Some(text))
            } else if allowed.is_empty() {
                Err(Failure::argument(format!(
                    "`{key}` is not offered for the image model {model} ({}). Leave it out.",
                    family.name
                )))
            } else {
                Err(Failure::argument(format!(
                    "`{key}` must be one of: {} for the image model {model} ({}).",
                    allowed.join(", "),
                    family.name
                )))
            }
        }
        Some(_) => Err(Failure::argument(format!("`{key}` must be a string."))),
    }
}

fn parse_call(arguments: &Value, model: &str, family: ImageFamily) -> Result<Call, Failure> {
    let prompt = string_arg(arguments, "prompt")
        .ok_or_else(|| Failure::argument("A non-empty `prompt` is required."))?;
    if prompt.chars().count() > MAX_PROMPT_CHARS {
        return Err(Failure::argument(format!(
            "The `prompt` is longer than {MAX_PROMPT_CHARS} characters."
        )));
    }
    let most = family.max_images();
    let count = match arguments.get("n") {
        None | Some(Value::Null) => 1,
        Some(value) => value
            .as_i64()
            .ok_or_else(|| Failure::argument("`n` must be a whole number."))?,
    };
    if !(1..=most).contains(&count) {
        return Err(Failure::argument(if most == 1 {
            format!(
                "`n` must be 1: the image model {model} ({}) makes one image per call.",
                family.name
            )
        } else {
            format!(
                "`n` must be between 1 and {most} for the image model {model} ({}).",
                family.name
            )
        }));
    }
    let quality = if family.free_quality {
        match arguments.get("quality") {
            None | Some(Value::Null) => None,
            Some(Value::String(text)) => {
                let text = text.trim();
                if text.chars().count() > MAX_FREE_TEXT_CHARS || text.chars().any(char::is_control)
                {
                    return Err(Failure::argument("`quality` is not a valid value."));
                }
                (!text.is_empty()).then(|| text.to_owned())
            }
            Some(_) => return Err(Failure::argument("`quality` must be a string.")),
        }
    } else {
        choice(arguments, "quality", family.qualities, model, family)?
    };
    Ok(Call {
        prompt,
        size: choice(arguments, "size", family.sizes, model, family)?.filter(|size| size != "auto"),
        count: u32::try_from(count).unwrap_or(1),
        quality: quality.filter(|quality| quality != "auto"),
    })
}

impl ImageGenerationTool {
    async fn bucket(&self) -> Result<&str, Failure> {
        self.resolved
            .get_or_try_init(|| async { self.buckets.artifact_bucket().await })
            .await
            .map(String::as_str)
            .map_err(|error| match error {
                ImageBucketError::NotAvailable => {
                    Failure::new("bucket_unavailable", NO_BUCKET, false)
                }
                ImageBucketError::Unavailable => {
                    Failure::new("bucket_unavailable", BUCKET_UNAVAILABLE, true)
                }
            })
    }

    async fn run(&self, arguments: &Value) -> Result<Value, Failure> {
        let call = parse_call(arguments, &self.model, self.family)?;
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
            .map_err(|error| host_failure(&self.model, &error))?;
        let stored = save_images(&self.host, &bucket, DEFAULT_NAME_PREFIX, images, "generate")
            .await
            // The images were generated and billed: say so, and do not retry.
            .map_err(|sentence| Failure::new("save_failed", sentence, false))?;
        Ok(self.answer(&stored, &bucket))
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
        if shown.len() == artifacts.len() && !shown.is_empty() {
            message.push_str(" Show them to the user with this Markdown, unchanged:\n");
            message.push_str(&shown.join("\n"));
        } else {
            message.push_str(
                " This chat cannot show them inline: give the user the file paths and say they are in the conversation's artifacts.",
            );
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
        Some(self.family.parameters_schema())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        // Every failure is a tool RESULT the model reads, never an error that
        // aborts the turn.
        Ok(match self.run(&arguments).await {
            Ok(value) => value,
            Err(failure) => failure.into_value(),
        })
    }
}
